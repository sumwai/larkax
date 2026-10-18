// Package store 读写插件的三张表：plugins、plugin_activations 与 plugin_kv。
//
// # 为什么是子包，而不是放进 internal/plugin
//
// internal/plugin 是**契约**：注册表、五个 hook、补丁与契约类型，它不连库、
// 不读配置、不碰时钟。加载器要读表，于是它需要 database/sql 与一条具体的
// SQL——那是另一件事。分开之后，「读表失败」（连接断了、SQL 错了、表不在）
// 与「插件源码编译不过」发生在两个包、两条错误路径上，排查时不必先分辨是
// 哪一种（见 LoadRegistry 的注释）。
//
// 仓库里同样的分法还有 internal/ledger/store 与 internal/calendar/store：
// 逻辑一个包、它落库的那一半一个子包。
//
// # 各表管什么
//
//	plugins             源码、元数据快照、代次，以及**参与注册与否**（status）
//	plugin_activations  按作用域的启用记录，priority 只在这里
//	plugin_kv           插件的私有存储，按 (plugin_key, user_id, key) 隔离（见 kv.go）
//
// 所以「一个插件跑不跑」只有一个答案：plugins.status。让 plugin_activations
// 的 enabled 也能关掉它的话，就会出现「界面上是启用的、实际没跑」这种状态，
// 而两个开关不一致时没有任何一处会报错。本包只从那张表读 priority
// （platform 作用域），enabled 与 user / channel 两个作用域留给按用户、
// 按渠道启用的那一步——那是请求路径上的事。
//
// plugin_kv 的表结构**不是本包建的**：它随插件那批表一起在 00003 里建好，
// 这里是它的第一个使用者（早先它一直空着，因为 kv 能力没有宿主实现）。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/plugin"
)

// plugins.status 的取值，与迁移里的 CHECK 约束逐字一致。
//
// 照约束写而不是自己发明状态值：写一个约束不认的值会在写入时报错，
// 而那看起来像「命令行坏了」，不像「状态名写错了」。
const (
	// StatusPending 是已发布、尚未启用。列的 DEFAULT 就是它，所以
	// `plugin publish` 不必显式写这一列。
	StatusPending = "pending"
	// StatusActive 是参与注册。加载器只读这个状态的插件。
	StatusActive = "active"
	// StatusDisabled 是停用。
	StatusDisabled = "disabled"
	// StatusFailed 是编译失败。
	//
	// **本包没有任何一处写它**：publish 拒绝编译不过的插件（一个坏插件不许
	// 进库），而加载器遇到坏插件只跳过并记日志，不回写库——启动路径上一个
	// 只读的加载器更安全，多实例部署时也不会几个实例同时改同一行。
	// 它留给「把加载期的失败写回库、让管理界面看得见」那一步。
	StatusFailed = "failed"
)

// ErrPluginNotFound 表示要改的那个 key 在库里没有对应的行。
//
// 命令行按它给出「约束冲突」那一档退出码：与「参数写错了」的下一步动作不同，
// 这一条要去看数据里现在是什么。
var ErrPluginNotFound = errors.New("插件不存在")

// Store 是插件的存储层。
type Store struct {
	db *sql.DB
}

// New 造一个存储层。
func New(db *sql.DB) *Store { return &Store{db: db} }

// LoadRegistry 读表、编译、发布，返回一份可用的注册表。
//
// # provided 从调用方来
//
// 它是**这次装配真的提供**的能力名（见 host.Available）。这个包连的是库，
// 不知道宿主接上了哪些能力，所以它不自己算——也不该有一份自己的猜测：
// 「能申请的」与「能提供的」两份清单各自维护过一次，代价是插件申请 kv
// 注册成功、运行期拿到 undefined。判据只有一处，就是宿主那一边算出来的那份。
//
// # 两条错误路径必须分得开
//
//   - **读表失败**（连接断了、SQL 错了、表不存在）→ 本函数返回错误，文案以
//     「读取 plugins 表失败」开头。这是**加载器坏了**，处置是看库、看网络。
//   - **单个插件编译不过** → 跳过，交给 logger 记一条 ERROR，错误里带着 key。
//     这是**插件坏了**（源码不对、或者申请了这份装配没有的能力），处置是修那个
//     插件。函数照样返回一份可用的注册表。
//
// 两者混成一句「加载插件失败」的话，运维手上只有一个起不来的进程和一句
// 无从下手的错——而它们的下一步动作完全不同。
//
// # 原子
//
// 先读完整批、编译完整批，最后一次性换表（见 plugin.PublishPartial）。
// 不会出现「读到一半的注册表」被请求用上。
func (s *Store) LoadRegistry(ctx context.Context, logger *slog.Logger, provided []string) (*plugin.Registry, error) {
	if logger == nil {
		logger = slog.Default()
	}
	regs, err := s.Active(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 plugins 表失败: %w", err)
	}

	skipped := 0
	// 编译一段源码不需要 ctx，也不该收一个：编译期的时限由沙箱自己给
	// （sandbox 的 inspectTimeout，见那边的注释——它存在正是因为调用方
	// 通常只给 context.Background()）。把一个 ctx 传下去不会中断编译，
	// 只会让人以为可以。整批的时间由调用方在本函数外面兜住。
	//
	//nolint:contextcheck // 理由见上
	reg, err := plugin.PublishPartial(regs, provided, func(r plugin.Registration, cause error) {
		skipped++
		logger.Error("插件未能上线，已跳过（其余插件照常上线）",
			"plugin", r.Key, "原因", cause)
	})
	if err != nil {
		return nil, fmt.Errorf("发布插件注册表失败: %w", err)
	}

	// 计数与 key 清单是这一条的正向证据：装上 0 个与「加载器根本没读表」
	// 在日志里长得一样（见 LESSONS.md 第 16 条）。
	//
	// 逐个列出 key 而不是只报个数：启动之后最常被问的一句是「我那个插件到底
	// 上了没有」，只给一个数字答不了它。插件是手工发布的，条数不会大到把日志
	// 淹掉；真到了那一天，该换的是日志级别而不是删掉这一行。
	keys := make([]string, 0, reg.Len())
	for _, p := range reg.Plugins() {
		keys = append(keys, p.Key)
	}
	logger.Info("插件已加载", "已启用", reg.Len(), "已跳过", skipped, "插件", strings.Join(keys, "、"))
	return reg, nil
}

// Active 读出参与注册的插件。
//
// 每个 key 只取**状态为 active 且代次最高**的那一行：同一个 key 可以有多行
// （版本不同），发布一次代次加一，而参与注册的只有被 enable 过的那一代——
// 新发布的版本状态是 pending，要 Enable 一次才生效。
func (s *Store) Active(ctx context.Context) ([]plugin.Registration, error) {
	const query = `
		SELECT DISTINCT ON (p.key) p.key, p.source,
		       COALESCE((SELECT MAX(a.priority) FROM plugin_activations a
		                  WHERE a.plugin_key = p.key AND a.scope = 'platform'), 0)
		FROM plugins p
		WHERE p.status = $1
		ORDER BY p.key, p.generation DESC`

	rows, err := s.db.QueryContext(ctx, query, StatusActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：一个插件都没有是正常结果（还没发布），
	// 不是「没有值」。
	out := []plugin.Registration{}
	for rows.Next() {
		var reg plugin.Registration
		if err := rows.Scan(&reg.Key, &reg.Source, &reg.Priority); err != nil {
			return nil, fmt.Errorf("读取插件行失败: %w", err)
		}
		out = append(out, reg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历插件失败: %w", err)
	}
	return out, nil
}

// Row 是管理面看到的一行。
type Row struct {
	Key        string
	Version    string
	Name       string
	Status     string
	Generation int64
	// Priority 是 platform 作用域那条启用记录的优先级，没有记录时为 0。
	Priority int
	// MetaProblem 是这一行的 meta 快照有什么问题，没问题时为空串。
	//
	// 为什么是**一行一栏**而不是整次报错：库里有**一行**坏数据时整个清单一条都
	// 看不到，而那正是最需要看清单的时候（同 channels.Admin.List 对坏密文的处置）。
	// 与那一处的区别在于判据的来源：密文是 bytea，没有任何约束拦得住一行垃圾；
	// meta 是 jsonb，**语法**由列类型保证，可能坏的是**形状**——不是一个对象、
	// 缺 key、key 与行上的对不上。形状检查用 plugin.ParseMeta，与编译期同一份。
	MetaProblem string
}

// List 列出全部插件行，按 key、再按代次降序。
func (s *Store) List(ctx context.Context) ([]Row, error) {
	const query = `
		SELECT p.key, p.version, p.name, p.status, p.generation,
		       COALESCE((SELECT MAX(a.priority) FROM plugin_activations a
		                  WHERE a.plugin_key = p.key AND a.scope = 'platform'), 0),
		       p.meta
		FROM plugins p
		ORDER BY p.key, p.generation DESC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询插件失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Row{}
	for rows.Next() {
		var (
			row  Row
			meta []byte
		)
		if err := rows.Scan(&row.Key, &row.Version, &row.Name, &row.Status,
			&row.Generation, &row.Priority, &meta); err != nil {
			return nil, fmt.Errorf("读取插件行失败: %w", err)
		}
		row.MetaProblem = metaProblem(row.Key, meta)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历插件失败: %w", err)
	}
	return out, nil
}

// Detail 是管理面看到的单插件详情：最新一代那一行，加上「这一行是哪来的」。
//
// 嵌 Row 而不是另起一份字段：详情与清单说的是同一个东西，各写一遍字段名之后
// 两条读路径会漂（清单多一栏、详情少一栏），而它们本该一起变。
type Detail struct {
	Row
	// CreatedBy 是写下这一行的人（users.id）。nil 表示没有记录——那一列可空，
	// 迁移或手工 SQL 写下的行就是 NULL。
	CreatedBy *int64
	// CreatedAt 是这一行写下的时刻。
	CreatedAt time.Time
	// Versions 是这个 key 下的行数（含这一行）。同一个 key 可以有多代：
	// 换 version 发布是多一行，同 (key, version) 重发是原地加代次。
	Versions int64
}

// Get 读一个插件 key 最新的一代。
//
// # 给的是哪一行
//
// 代次最高的那一行，也就是**最新发布**的那一代。它与**加载器实际跑的那一行**
// 不是同一件事：加载器走 Active，只在同一个 key 里取状态为 active 的一行。
// 发布了新版本还没 Enable 时，这里给的是那一版新行（状态 pending），而跑着的
// 仍是上一代——要问「现在生效的是哪一版」，得看这一行自己的状态。
//
// 更老的代次是历史，要列全就去看 List——那条清单按代次降序列，正适合回答
// 「之前发过哪几版」。
//
// # 为什么不是 List 加一个筛选
//
// List 把每一代都读回来再筛，代价是照行数涨的；而这里要的是一行。更实际的理由
// 是**语义**：Get 查不到要报「插件不存在」（ErrPluginNotFound），而 List 的
// 空结果是一个正常的空清单。两者的下一步动作不同，合成一条路径就得在调用方
// 再判一次「是清单为空还是 key 写错了」。
func (s *Store) Get(ctx context.Context, key string) (Detail, error) {
	const query = `
		SELECT p.key, p.version, p.name, p.status, p.generation,
		       COALESCE((SELECT MAX(a.priority) FROM plugin_activations a
		                  WHERE a.plugin_key = p.key AND a.scope = 'platform'), 0),
		       p.meta, p.created_by, p.created_at,
		       (SELECT count(*) FROM plugins c WHERE c.key = p.key)
		  FROM plugins p
		 WHERE p.key = $1
		 ORDER BY p.generation DESC
		 LIMIT 1`

	var (
		d         Detail
		meta      []byte
		createdBy sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, query, key).Scan(
		&d.Key, &d.Version, &d.Name, &d.Status, &d.Generation, &d.Priority,
		&meta, &createdBy, &d.CreatedAt, &d.Versions)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 与 Enable / Disable 共用同一个哨兵：三条命令问的都是「这个 key 在库里
		// 有没有行」，答案一样时下一步也一样（先跑 plugin list 看现在有什么）。
		return Detail{}, fmt.Errorf("%w: %s", ErrPluginNotFound, key)
	case err != nil:
		return Detail{}, fmt.Errorf("查询插件 %s 失败: %w", key, err)
	}
	if createdBy.Valid {
		id := createdBy.Int64
		d.CreatedBy = &id
	}
	d.MetaProblem = metaProblem(d.Key, meta)
	return d, nil
}

// metaProblem 判断库里那份 meta 快照有没有问题，没问题返回空串。
func metaProblem(key string, raw []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Sprintf("meta 不是一个 JSON 对象: %v", err)
	}
	if obj == nil {
		return "meta 是空的"
	}
	meta, err := plugin.ParseMeta(obj)
	if err != nil {
		return err.Error()
	}
	if meta.Key != key {
		return fmt.Sprintf("meta 里的 key %q 与行上的 key %q 不一致", meta.Key, key)
	}
	return ""
}

// Publication 是一条待发布的记录：源码 + 它的元数据快照 + 优先级。
type Publication struct {
	Key     string
	Version string
	Name    string
	Source  string
	// Meta 是源码里声明的元数据原文（一个 JSON 对象）。
	Meta []byte
	// Priority 写进 platform 作用域的启用记录。
	Priority int
}

// NewPublication 从编译好的插件与它的源码造一条待发布记录。
//
// 元数据快照取自**模块上那一份**（编译期读出来的原文），不是把 Meta 结构体
// 再序列化一遍：后者会丢字段（结构体只留宿主要用的那几个），而这一列存的是
// 「作者当时声明了什么」，将来要能照着它回答「这个插件申请过什么能力」。
func NewPublication(p *plugin.Plugin, source string) (Publication, error) {
	meta, err := json.Marshal(p.Module.Meta())
	if err != nil {
		return Publication{}, fmt.Errorf("序列化插件 %s 的元数据失败: %w", p.Key, err)
	}
	return Publication{
		Key:      p.Key,
		Version:  p.Meta.Version,
		Name:     p.Meta.Name,
		Source:   source,
		Meta:     meta,
		Priority: p.Priority,
	}, nil
}

// Result 是一次发布的结果。
type Result struct {
	Key        string
	Version    string
	Generation int64
	// Overwritten 为 true 表示这次**覆盖**了同一个 (key, version) 的旧行。
	Overwritten bool
	// Status 是发布后这一行的状态。
	Status string
}

// Publish 落一条插件。
//
// # 同一个 (key, version) 重复发布是「覆盖」，不是「拒绝」
//
// 覆盖会换掉 source / meta / name，代次加一，**状态保持原值**。
//
//   - 为什么不拒绝：作者改一个笔误（注释、常量）不该先升 version，否则
//     version 会变成「发布次数」而不是「版本」；而 version 写在源码的 meta 里，
//     升它要改源码——多一步，而且容易忘。
//   - 为什么代次加一而不是就地静默改：`generation` 那一列的存在正说明同一个
//     key 会被反复发布，每一代都是可追溯的一步。库里的行只有一行（UNIQUE
//     (key, version) 拦着），所以旧源码不留存——要回退就重新发布一次旧源码。
//   - 为什么状态保持原值：已启用的插件不该因为「改了个错别字」就下线。
//     新 version 走的是另一条路（插入新行，状态 pending），那条路上启用与否
//     由 enable 决定。
//
// **本片没有热更新**：覆盖之后库里是新源码，而跑着的进程里还是旧的，要重启
// 才生效。命令行的输出会把「重启后生效」写出来。
//
// 两处写入（plugins 与 plugin_activations）在同一个事务里：优先级写不进去时
// 源码也不该落库，否则库里会出现一个「发布了但优先级不是你要的」的插件。
func (s *Store) Publish(ctx context.Context, pub Publication) (Result, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("开启事务失败: %w", err)
	}
	// 提交成功后这次 Rollback 是空操作；失败路径上它负责收尾。
	defer func() { _ = tx.Rollback() }()

	res := Result{Key: pub.Key, Version: pub.Version}

	var existingID int64
	var existingStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT id, status FROM plugins WHERE key = $1 AND version = $2`,
		pub.Key, pub.Version).Scan(&existingID, &existingStatus)
	switch {
	case err == nil:
		// 同一代（同 key 同 version）已经在了：覆盖它。
		res.Overwritten = true
		res.Status = existingStatus
		if _, execErr := tx.ExecContext(ctx,
			`UPDATE plugins
			    SET name = $2, source = $3, meta = $4::jsonb, generation = generation + 1
			  WHERE id = $1`,
			existingID, pub.Name, pub.Source, string(pub.Meta)); execErr != nil {
			return Result{}, fmt.Errorf("覆盖插件 %s 失败: %w", pub.Key, execErr)
		}
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT generation FROM plugins WHERE id = $1`, existingID).Scan(&res.Generation); scanErr != nil {
			return Result{}, fmt.Errorf("读取插件 %s 的新代次失败: %w", pub.Key, scanErr)
		}

	case errors.Is(err, sql.ErrNoRows):
		// 新的版本：插入一行新代次。状态交给列默认值（pending）——
		// 发布了不等于启用了，启用是 enable 那个动词的事。
		res.Status = StatusPending
		if scanErr := tx.QueryRowContext(ctx,
			`INSERT INTO plugins (key, version, name, source, meta, generation)
			 VALUES ($1, $2, $3, $4, $5::jsonb,
			         (SELECT COALESCE(MAX(generation), -1) + 1 FROM plugins WHERE key = $1))
			 RETURNING generation`,
			pub.Key, pub.Version, pub.Name, pub.Source, string(pub.Meta)).Scan(&res.Generation); scanErr != nil {
			// 上面那次 SELECT 到这次 INSERT 之间，另一次发布可能已经把同一个
			// (key, version) 插进去了；唯一键会挡下后到的那一次并给出 23505。
			// 本包没有为它准备具名哨兵（它不是调用方按名字处理的失败），
			// 兜底哨兵至少让「约束冲突」这个分类存在。
			return Result{}, dberr.ConstraintFailure(
				fmt.Errorf("写入插件 %s 失败: %w", pub.Key, scanErr))
		}

	default:
		return Result{}, fmt.Errorf("查询插件 %s 失败: %w", pub.Key, err)
	}

	if err := setPriority(ctx, tx, pub.Key, pub.Priority); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("提交插件 %s 的发布失败: %w", pub.Key, err)
	}
	return res, nil
}

// setPriority 把 platform 作用域那条启用记录的优先级写成 priority。
//
// 表上**没有** (plugin_key, scope) 的唯一约束，所以这里不能靠 ON CONFLICT：
// 先更新，一行都没更新到才插入。更新那一句不带 LIMIT，重复的平台行会一起
// 被改成同一个值——读取侧取的是 MAX，于是重复行不会让「读到的优先级」与
// 「刚写进去的」不一致。
func setPriority(ctx context.Context, tx *sql.Tx, key string, priority int) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE plugin_activations SET priority = $2
		  WHERE plugin_key = $1 AND scope = 'platform'`,
		key, priority)
	if err != nil {
		return fmt.Errorf("更新插件 %s 的优先级失败: %w", key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认插件 %s 的优先级写入失败: %w", key, err)
	}
	if affected > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO plugin_activations (plugin_key, scope, priority)
		 VALUES ($1, 'platform', $2)`,
		key, priority); err != nil {
		return fmt.Errorf("写入插件 %s 的启用记录失败: %w", key, err)
	}
	return nil
}

// Enable 把该 key 代次最高的那一行置为 active。
//
// 只抬最高的一代：更老的代次躺着不动，它们永远选不中（加载器每个 key 只取
// 代次最高的那一行）。反过来，Disable 必须把**每一代**都按下去——只按最高的
// 那一行的话，加载器会落到上一代上，表现为「停用了，但它换了个版本继续跑」。
func (s *Store) Enable(ctx context.Context, key string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE plugins SET status = $2
		  WHERE id = (SELECT id FROM plugins WHERE key = $1
		               ORDER BY generation DESC LIMIT 1)`,
		key, StatusActive)
	if err != nil {
		return 0, fmt.Errorf("启用插件 %s 失败: %w", key, err)
	}
	return affectedOrNotFound(res, key)
}

// Disable 把该 key 的每一代都置为 disabled。
func (s *Store) Disable(ctx context.Context, key string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE plugins SET status = $2 WHERE key = $1`,
		key, StatusDisabled)
	if err != nil {
		return 0, fmt.Errorf("停用插件 %s 失败: %w", key, err)
	}
	return affectedOrNotFound(res, key)
}

// Deleted 汇总一次卸载实际删掉了什么。
//
// 与 Result 同一条理由：它是这条命令的**正向证据**。「什么都没删」与「删干净了」
// 在安静的终端里长得一样（见 LESSONS.md 第 16 条），而这几个数正是「删的到底是
// 这一个插件的那几行」与「连别人的东西一起扫掉了」之间的区别。
type Deleted struct {
	Key string
	// Versions 是 plugins 里删掉的行数——同一个 key 的每一代。
	Versions int64
	// Activations 是 plugin_activations 里删掉的行数。
	Activations int64
	// KVKeys 是 plugin_kv 里删掉的键数。
	KVKeys int64
}

// Delete 把一个插件从库里彻底删掉：每一代、它的启用记录、以及它私有的 KV。
//
// # 为什么是硬删，而不是像渠道那样软删
//
// 渠道必须软删：历史账单按 channel_id 引用它，删了行之后那些记录就认不出这条
// 渠道了（见 docs/cli-reference.md 的 channel 一节）。**插件没有这条路**——没有
// 任何一张表按插件引用它，读它的只有加载器（按 key 与 status 读）与插件自己的
// KV。留一行 deleted 标记下来，唯一的后果是加载器与清单都得再学一遍「这一行
// 不算数」，而那个「不算数」的判断还不止一处。
//
// # 为什么连 plugin_kv 一起删
//
// 那张表的主键是 (plugin_key, user_id, key)，**没有外键指着 plugins**。插件没了
// 之后，它里面那些行没有第二条路径能碰到：Get 与 Del 要键名，而键名是插件自己
// 写的；Set 只在同一个命名空间里清过期的行，而没有人会再往这个命名空间里写。
// 留着它们就是留一堆谁也看不见、谁也清不掉的垃圾——而「库里出现一个程序不认识
// 的东西」正是这个仓库反复防的那一类。
//
// 代价要说清楚：这些行删掉就没了，而本版**没有导出它们的入口**。所以命令的
// 帮助与 docs/cli-reference.md 里都写着这一条。
//
// # 为什么在一个事务里
//
// 三张表删一半会留下「插件没了、启用记录还在」或者「插件没了、私有存储还在」
// 这类状态，而它们都不报错。同一个事务里删完，要么都在要么都不在。
func (s *Store) Delete(ctx context.Context, key string) (Deleted, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Deleted{}, fmt.Errorf("开启事务失败: %w", err)
	}
	// 提交成功后这次 Rollback 是空操作；失败路径上它负责收尾。
	defer func() { _ = tx.Rollback() }()

	// 先删 plugins。删不到行说明这个 key 在库里没有东西，那就一行都不要动：
	// 继续往下删的话，一个拼错的 key 会把另一个插件的启用记录或 KV 扫掉
	// （那两张表按 plugin_key 存，与 plugins 之间没有外键拦着）。
	res, err := tx.ExecContext(ctx, `DELETE FROM plugins WHERE key = $1`, key)
	if err != nil {
		return Deleted{}, fmt.Errorf("删除插件 %s 失败: %w", key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Deleted{}, fmt.Errorf("确认插件 %s 的删除结果失败: %w", key, err)
	}
	if affected == 0 {
		return Deleted{}, fmt.Errorf("%w: %s", ErrPluginNotFound, key)
	}
	out := Deleted{Key: key, Versions: affected}

	if out.Activations, err = deleteByKey(ctx, tx, `plugin_activations`, key,
		"启用记录"); err != nil {
		return Deleted{}, err
	}
	if out.KVKeys, err = deleteByKey(ctx, tx, `plugin_kv`, key, "私有存储"); err != nil {
		return Deleted{}, err
	}

	if err := tx.Commit(); err != nil {
		return Deleted{}, fmt.Errorf("提交插件 %s 的删除失败: %w", key, err)
	}
	return out, nil
}

// deleteByKey 删掉一张按 plugin_key 索引的表里属于这个插件的行。
//
// 表名由调用方给字面量、拼进 SQL：这两张表的名字是这里的常量，不是外部输入，
// 而写成参数（$1）在 PostgreSQL 里根本不行——表名不是值。写成一个小函数是为了
// 两处调用（plugins 那一处除外）的报错措辞与行数确认只有一份。
func deleteByKey(ctx context.Context, tx *sql.Tx, table, key, what string) (int64, error) {
	//nolint // 表名是调用点写的字面量，不是外部输入
	res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE plugin_key = $1`, key)
	if err != nil {
		return 0, fmt.Errorf("删除插件 %s 的%s失败: %w", key, what, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认插件 %s 的%s删除结果失败: %w", key, what, err)
	}
	return affected, nil
}

// affectedOrNotFound 一行都没改到时返回 ErrPluginNotFound。
//
// 「key 写错了」与「改成功了但值本来就是那样」在 RowsAffected 上都是 0，
// 所以这里只报「这个 key 在库里没有行」——那是唯一能确定的一件事。
// 重复启用一个已经启用的插件因此会报错，而不是安静地成功：命令行说「已启用」
// 而其实什么都没改，与「真的改了」在输出上长得一样（见 LESSONS.md 第 16 条）。
func affectedOrNotFound(res sql.Result, key string) (int64, error) {
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认插件 %s 的写入结果失败: %w", key, err)
	}
	if affected == 0 {
		return 0, fmt.Errorf("%w: %s", ErrPluginNotFound, key)
	}
	return affected, nil
}
