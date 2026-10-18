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
	// **包里每一条写路径都不写它**：publish 拒绝编译不过的插件（一个坏插件
	// 不许进库），加载器遇到坏插件只跳过——它把「这次没装上」写进 last_load_result
	// 那三列，status 一个字都不动（见 LoadRegistry）。所以库里出现这个状态只有
	// 一种来路：手工改库，或者将来另加的写入方。管理面那两句说明也不再挂在它
	// 上面：加载期那一栏才是（见 Row 的 RunningLoad）。
	StatusFailed = "failed"
)

// plugins.last_load_result 的取值，与迁移里的 CHECK 约束逐字一致
// （同上面那几条 status：写一个约束不认的值会在写入时报错，而那看起来像
// 「命令行坏了」，不像「结果名写错了」）。
const (
	// LoadResultOK 是这一代上一次被加载时装上了。
	LoadResultOK = "ok"
	// LoadResultFailed 是上一次尝试跳过了它，原因在 last_load_error 里。
	LoadResultFailed = "failed"
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
// # 写回加载的结果
//
// 读到的每一行都刷一遍 last_load_result / last_load_error / last_load_at
// （见 recordLoadResults）：装上了写 ok，被跳过的写 failed 加错误原文。
// 那三列只是**这一次尝试**的记录，status 与启用记录一个字都不动，所以跳过的
// 插件下次启动照样会再试一次——「改好源码重启」这条路因此是通的。
//
// # 原子
//
// 先读完整批、编译完整批，最后一次性换表（见 plugin.PublishPartial）。
// 不会出现「读到一半的注册表」被请求用上。
func (s *Store) LoadRegistry(ctx context.Context, logger *slog.Logger, provided []string) (*plugin.Registry, error) {
	if logger == nil {
		logger = slog.Default()
	}
	rows, err := s.activeRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 plugins 表失败: %w", err)
	}
	regs := make([]plugin.Registration, 0, len(rows))
	for _, r := range rows {
		regs = append(regs, r.reg)
	}

	// 按 key 记下跳过的原因，而不是只记个数：加载完要写回库，而那一列记的是
	// **哪一个**插件没装上、为什么（见 loadRecords）。
	skipped := map[string]error{}
	// 编译一段源码不需要 ctx，也不该收一个：编译期的时限由沙箱自己给
	// （sandbox 的 inspectTimeout，见那边的注释——它存在正是因为调用方
	// 通常只给 context.Background()）。把一个 ctx 传下去不会中断编译，
	// 只会让人以为可以。整批的时间由调用方在本函数外面兜住。
	//
	//nolint:contextcheck // 理由见上
	reg, err := plugin.PublishPartial(regs, provided, func(r plugin.Registration, cause error) {
		skipped[r.Key] = cause
		logger.Error("插件未能上线，已跳过（其余插件照常上线）",
			"plugin", r.Key, "原因", cause)
	})
	if err != nil {
		return nil, fmt.Errorf("发布插件注册表失败: %w", err)
	}
	// 写回放在发布之后：注册表已经换过表了，这一步只影响管理面上那一栏，
	// 所以它出问题不该把已经可用的注册表一起丢掉。
	recordLoadResults(ctx, s.db, logger, loadRecords(rows, skipped))

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
	logger.Info("插件已加载", "已启用", reg.Len(), "已跳过", len(skipped), "插件", strings.Join(keys, "、"))
	return reg, nil
}

// loadRecord 是一次加载对**一行**的结果，准备写回 plugins 的那三列。
//
// 身份用 (key, version) 而不是 (key, generation)：覆盖发布让代次原地加一，同一个
// key 里两行可以撞到同一个代次上（见 pickedOrder），而 version 在同一个 key 里
// 唯一（UNIQUE (key, version)）——按它写才落得准那一行。
type loadRecord struct {
	key     string
	version string
	result  string
	// errorText 是失败时的错误原文，装上了就是空串（写回时折成 NULL）。
	errorText string
}

// loadRecords 整理出「刚读的这些行各自装上了没有」，供写回用。
//
// skipped 是 PublishPartial 跳掉的那些（key → 原因）。按 key 查而不是按下标对：
// 跳过的那些不进注册表，两边的下标对不上。
func loadRecords(rows []activeRow, skipped map[string]error) []loadRecord {
	out := make([]loadRecord, 0, len(rows))
	for _, r := range rows {
		rec := loadRecord{key: r.reg.Key, version: r.version, result: LoadResultOK}
		if cause, ok := skipped[r.reg.Key]; ok {
			rec.result = LoadResultFailed
			rec.errorText = cause.Error()
		}
		out = append(out, rec)
	}
	return out
}

// recordLoadResultSQL 写回一行。WHERE 落在 (key, version) 上：那就是刚读的那一行
// 的身份，写回因此不必再拼一遍「取中哪一行」那条判据（见 activeRow）。
//
// 用 version 而不是 generation：代次会撞车（见 pickedOrder），按代次写会连另一个
// 版本的行一起改掉。
//
// 时刻取库里的 now() 而不是 Go 那边的时钟：这一列要与库里的其它时刻比先后，
// 而进程的时钟与库的时钟未必是同一个。
const recordLoadResultSQL = `
	UPDATE plugins
	   SET last_load_result = $3, last_load_error = $4, last_load_at = now()
	 WHERE key = $1 AND version = $2`

// execer 是写回加载结果用到的那一点 SQL 能力。
//
// 收一个窄接口而不是 *sql.DB：那条「写不进去也不拖垮启动」的纪律要能在没有库的
// 情况下钉住（见 store_test.go 里的假实现），而 *sql.DB、*sql.Tx 都满足它。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// recordLoadResults 把这一次加载的结果写回 plugins 的三列。
//
// # 两条纪律
//
//   - **写不进去只记日志，没有返回值**：插件是可选增强（plugin.enabled 默认是
//     false）。为了给管理面上那一栏留一份记录，让整个服务起不来，代价与它带来的
//     信息不成比例。所以这里不外传错误，也不给调用方一个可以失败的返回值。
//   - **多实例各自写、最后一次赢**：这里存的是「最近一次尝试」，不是所有权状态。
//     两个实例同时启动时，库里留下的是后写的那个——它不比另一个更正确，只是
//     更近，而这正是这一栏要说的事。
//
// 一条一条写而不是拼一条多行的 SQL：要写的行数与启用的插件数同阶（手工发布，
// 量很小），而一条一条写让失败的行能被单独点名。
func recordLoadResults(ctx context.Context, ex execer, logger *slog.Logger, recs []loadRecord) {
	for _, rec := range recs {
		_, err := ex.ExecContext(ctx, recordLoadResultSQL,
			rec.key, rec.version, rec.result, nullableText(rec.errorText))
		if err != nil {
			// 文案点名「不影响插件运行」：看得见的那半是管理面上少了一栏记录，
			// 而读到这条日志的人最先担心的是「插件是不是没装上」。
			logger.Error("写回插件加载结果失败（只影响管理面上的记录，不影响插件运行）",
				"plugin", rec.key, "version", rec.version, "原因", err)
		}
	}
}

// nullableText 把空串折成 NULL。
//
// 与那几条读路径上的 nullableString 是同一个判据的反面：那一列可空，而空串在
// 这里不表示「错误原文是空字符串」，表示「这一次没失败」。
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pickedOrder 给出「同一个 key 里取中哪一行」的排序键，alias 是 plugins 在那个
// 查询里的别名。
//
// 排序键是**两段**，第二段不能省：代次高的排前面，代次撞车时按版本名定胜负。
// 代次会撞车是真的——覆盖发布时它是原地加一（见 Publish），所以同一个 key 里
// A(gen 0)、B(gen 1)，之后再发一次 A，A 也成了 gen 1，两行并排。version 在同一个
// key 里唯一（UNIQUE (key, version)），拿它收尾就定得下一行。
//
// 代次撞车时「谁更新」已经无从判断了——覆盖发布不另记时刻——所以这一档只求**定得
// 下来**：按版本名收尾。撞车是覆盖发布的副产物，一个版本只发布一次的库里不会有。
//
// 四处共用它：读表那一处（activeRows）、两处管理面查询（runningJoin、Get），以及
// Enable 抬哪一代。它们必须选出**同一行**——写回落在加载器读的那一行上，管理面报的
// 也得是那一行，enable 抬的还得是那一行；各自写一遍排序的话，撞车时可能各选一行，
// 而库里看不出任何异常。
func pickedOrder(alias string) string {
	return alias + ".generation DESC, " + alias + ".version DESC"
}

// activeRow 是加载器读出来的一行：注册要用的那几样，外加它在 plugins 上是哪一行。
//
// version 也带着，因为加载完要把结果**写回**（见 recordLoadResults）：写回要指名
// 道姓地落在「刚读的是哪一行」上，而 (key, version) 正是那一行的身份。让写回自己
// 再拼一遍「取中哪一行」那条判据的话，两份判据会各自漂移，而漂移的那一份会把结果
// 写到另一行上——库里看不出任何异常，管理面上那一栏从此说的不是加载器刚做的事。
type activeRow struct {
	reg     plugin.Registration
	version string
}

// activeRows 读出参与注册的行：加载器装配时读的就是这一份。
//
// 每个 key 只取**状态为 active 且取中**的那一行：同一个 key 可以有多行
// （版本不同），发布一次代次加一，而参与注册的只有被 enable 过的那一代——
// 新发布的版本状态是 pending，要 Enable 一次才生效。
//
// 「取中哪一行」这条判据在两处出现：这里与 runningJoin（管理面报的）。两处必须
// 选出同一行，所以排序键共用 pickedOrder——各自写一遍的话，代次撞车时两边可能
// 各选一行，而写回落在加载器读的那一行上，管理面说的却会是另一行。（判据本身
// 仍有第二份，改动时两处要一起改；测试里有一条同代次两行的用例守着这一点。）
func (s *Store) activeRows(ctx context.Context) ([]activeRow, error) {
	query := `
		SELECT DISTINCT ON (p.key) p.key, p.source, p.version,
		       COALESCE((SELECT MAX(a.priority) FROM plugin_activations a
		                  WHERE a.plugin_key = p.key AND a.scope = 'platform'), 0)
		FROM plugins p
		WHERE p.status = $1
		ORDER BY p.key, ` + pickedOrder("p")

	rows, err := s.db.QueryContext(ctx, query, StatusActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：一个插件都没有是正常结果（还没发布），
	// 不是「没有值」。
	out := []activeRow{}
	for rows.Next() {
		var r activeRow
		if err := rows.Scan(&r.reg.Key, &r.reg.Source, &r.version, &r.reg.Priority); err != nil {
			return nil, fmt.Errorf("读取插件行失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历插件失败: %w", err)
	}
	return out, nil
}

// Generation 是库里的一个代次：版本名与它在 generation 那一列上的值。
//
// 两样都要：代次是加载器比大小的依据（同一个 key 里取最大的那一行），而人看到
// 一个数字认不出那是哪一版。
type Generation struct {
	Version string
	// Number 是 generation 那一列的值：同一个 key 里每发布一次加一。
	Number int64
}

// LoadAttempt 是**加载期**那一次尝试的结果：加载器有没有把这一代装上。
//
// 三样一起给，因为它们来自同一次写（见迁移 00034）：结果、失败时的原文、以及
// 那一次的时刻。它与 compile_error 说的是两件事——那一列记的是**发布期**编译
// 不过，而发布根本不收编译不过的插件，所以只有手工改库的行有值。
type LoadAttempt struct {
	// Result 是 LoadResultOK 或 LoadResultFailed。
	Result string
	// Error 是没装成时的错误原文（编译错，或者它申请了这次装配没提供的能力）。
	// 装上了就是空串。
	Error string
	// At 是那一次尝试的时刻，取自库里的时钟。
	//
	// 可能是零值——手工改库的行只填了结果那一列时就是。渲染要按「没有时刻」处理
	// 它（见 cmd/larkaxctl 里那两处），不要直接打出来。
	At time.Time
}

// Failed 问这一次尝试有没有装成。
func (a LoadAttempt) Failed() bool { return a.Result == LoadResultFailed }

// Row 是管理面看到的一行。
type Row struct {
	Key        string
	Version    string
	Name       string
	Status     string
	Generation int64
	// Priority 是 platform 作用域那条启用记录的优先级，没有记录时为 0。
	Priority int
	// CompileError 是**发布期**编译不过留下的错误原文（plugins.compile_error）。
	// 没失败过时是空串——那一列可空，而 publish 拒绝编译不过的插件，所以今天
	// 只有手工改库的行有值。加载期没装成的原因不在这里，见 RunningLoad。
	CompileError string
	// Latest 表示这一行是这个 key 里代次最高的那一行，也就是**最新一次尝试**。
	// 一个 key 只发布过一代时，这一行既是取中的那一代，也是它的最新一次尝试。
	Latest bool
	// Running 是同一个 key 里**加载器取中的那一代**：状态为 active 且代次最高的
	// 那一行。这个 key 现在一代都没跑时是 nil（每一代都是 pending / disabled /
	// failed 是正常状态）。
	//
	// 取中不等于装上了：上一次真的装上没有看 RunningLoad，而被跳过时加载器不会
	// 退回更老的那一代——那时这个 key 哪一代都没在跑。
	//
	// 它按 **key** 算，所以同一个 key 的几代带着同一个值——与 Priority 一样，
	// 它是这一行所属那个 key 的属性。这一行自己是不是取中的那一代，问 IsRunning。
	Running *Generation
	// RunningLoad 是**取中的那一代**上一次被加载时装上了没有：库里
	// last_load_result / last_load_error / last_load_at 三列的合体。
	//
	// 它跟着 Running 走，所以也是按 key 给的：那三列是加载器写在**它读的那一行**
	// 上的，而它读的正是这一代。这个 key 没有取中的一代时是 nil；「取中的这一代
	// 还没被加载过」（三列都是 NULL）同样给 nil——这里分不出来，而读的那一侧分得
	// 出来：Running 为空是「没有取中的一代」，不为空而这是 nil 是「取中了但还没
	// 试过」（见 cmd/larkaxctl 里「在跑」那一栏的分档）。
	//
	// 之所以不从这一行自己的列上读：加载器写的是它读的那一行，而清单里最新那一行
	// 未必是它（发布了没 enable 时就不是）。失败的那一条记录因此只挂在取中的那一代
	// 上，从这里读才读得到。
	RunningLoad *LoadAttempt
	// MetaProblem 是这一行的 meta 快照有什么问题，没问题时为空串。
	//
	// 为什么是**一行一栏**而不是整次报错：库里有**一行**坏数据时整个清单一条都
	// 看不到，而那正是最需要看清单的时候（同 channels.Admin.List 对坏密文的处置）。
	// 与那一处的区别在于判据的来源：密文是 bytea，没有任何约束拦得住一行垃圾；
	// meta 是 jsonb，**语法**由列类型保证，可能坏的是**形状**——不是一个对象、
	// 缺 key、key 与行上的对不上。形状检查用 plugin.ParseMeta，与编译期同一份。
	MetaProblem string
}

// IsRunning 问这一行是不是加载器取中的那一行。
//
// 比的是 (代次, 版本名) 两样：代次**不唯一**——覆盖发布时它是原地加一（见
// Publish），所以 A(gen 0)、B(gen 1) 之后再发一次 A，A 也成了 gen 1，同一个 key
// 里两行撞在同一个代次上。只比代次会把两行都说成取中的那一行，而加载器只读一行。
// 加上版本名就定得下来：同一个 key 里 version 唯一（UNIQUE (key, version)），
// 而这正是 pickedOrder 那条排序键。
//
// 取中不等于装着——真的装上没有看 RunningLoad。
func (r Row) IsRunning() bool {
	return r.Running != nil && r.Running.Number == r.Generation && r.Running.Version == r.Version
}

// runningJoin 摘出「这个 key 取中的是哪一代」以及那一代上次加载的结果，
// 供 List 与 Get 共用。
//
// 写成一段共用的 SQL 而不是两处各写一遍：这一处与读表那一处（见 activeRows）报的
// 必须是同一行，而两边的判据只要有一边漂了，管理面就会报出一个根本没在跑的代次。
// 「取中哪一行」那条判据因此两处共用同一条排序键（pickedOrder）——判据本身仍有第二
// 份（WHERE 那两句），改动时两处要一起改，测试里有一条同代次两行的用例守着。
//
// LEFT JOIN 而不是 INNER：没有取中的一代的 key 也要出现在清单里，那时右边整片
// 是 NULL——「哪一代都没启用」是一个要看得见的状态，不是一行要丢掉的记录。
//
// 加载期那三列跟着这一代一起摘出来，而不是从 p 上读：它们是加载器写在它读的那
// 一行上的，而最新那一行未必是它。
//
// 是函数而不是常量，只因为排序键要经 pickedOrder 拼（别名是子查询里的 a）。
func runningJoin() string {
	return `
	LEFT JOIN LATERAL (
		SELECT a.version, a.generation,
		       a.last_load_result, a.last_load_error, a.last_load_at
		  FROM plugins a
		 WHERE a.key = p.key AND a.status = 'active'
		 ORDER BY ` + pickedOrder("a") + `
		 LIMIT 1
	) r ON true`
}

// List 列出全部插件行，按 key、再按代次降序。
//
// 每一行都带上「这个 key 取中的是哪一代、它上一次装上没有」（见 runningJoin）与
// 「这一行是不是最新一次尝试」：清单要回答的是「库里有什么」与「其中哪一代真的
// 装着」，而这几件事的判据都不在这一行自己的列上——status 只说了这一代参不参与
// 注册，一个 key 里可以有好几代都是 active，而加载结果写在加载器读的那一行上。
func (s *Store) List(ctx context.Context) ([]Row, error) {
	query := `
		SELECT p.key, p.version, p.name, p.status, p.generation,
		       COALESCE((SELECT MAX(a.priority) FROM plugin_activations a
		                  WHERE a.plugin_key = p.key AND a.scope = 'platform'), 0),
		       p.meta, p.compile_error,
		       p.generation = (SELECT MAX(m.generation) FROM plugins m WHERE m.key = p.key),
		       r.version, r.generation,
		       r.last_load_result, r.last_load_error, r.last_load_at
		FROM plugins p` + runningJoin() + `
		ORDER BY p.key, ` + pickedOrder("p")

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询插件失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Row{}
	for rows.Next() {
		var (
			row         Row
			meta        []byte
			compileErr  sql.NullString
			runVersion  sql.NullString
			runGeneratn sql.NullInt64
			runResult   sql.NullString
			runErrText  sql.NullString
			runAt       sql.NullTime
		)
		if err := rows.Scan(&row.Key, &row.Version, &row.Name, &row.Status,
			&row.Generation, &row.Priority, &meta, &compileErr, &row.Latest,
			&runVersion, &runGeneratn, &runResult, &runErrText, &runAt); err != nil {
			return nil, fmt.Errorf("读取插件行失败: %w", err)
		}
		row.CompileError = compileErr.String
		row.Running = generationOrNil(runVersion, runGeneratn)
		row.RunningLoad = loadAttemptFrom(runResult, runErrText, runAt)
		row.MetaProblem = metaProblem(row.Key, meta)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历插件失败: %w", err)
	}
	return out, nil
}

// generationOrNil 把 LEFT JOIN 出来的两列拼成一个「取中的那一代」，没有时是 nil。
//
// 两列一起判而不是只看代次：没有取中的一代时右边整片是 NULL，而代次 0 是一个
// 合法的值（第一次发布），拿零值当「没有」会把第一代说成没被取中。
func generationOrNil(version sql.NullString, number sql.NullInt64) *Generation {
	if !version.Valid || !number.Valid {
		return nil
	}
	return &Generation{Version: version.String, Number: number.Int64}
}

// loadAttemptFrom 把 LEFT JOIN 出来的三列拼成一次加载，没有记录时是 nil。
//
// 只看结果那一列：三列是加载器同一次写下来的（见 recordLoadResults），结果为空
// 就是这一代还没被加载过——没有取中的一代时右边整片是 NULL。
//
// At 可能是零值：三列由加载器一次写入，但手工改库的行可以只填其中一列，那时结果
// 有值而时刻没有；写回失败也会留下一片 NULL（见迁移里那一段）。这里照实给出零值，
// 由渲染那一步说「没有时刻」——在这里补一个假时刻，「0001-01-01」这种看着像真
// 数据的值就会出现在管理面上。
func loadAttemptFrom(result, errText sql.NullString, at sql.NullTime) *LoadAttempt {
	if !result.Valid {
		return nil
	}
	return &LoadAttempt{Result: result.String, Error: errText.String, At: at.Time}
}

// Detail 是管理面看到的单插件详情：最新一代那一行，加上「这一行是哪来的」。
//
// 嵌 Row 而不是另起一份字段：详情与清单说的是同一个东西，各写一遍字段名之后
// 两条读路径会漂（清单多一栏、详情少一栏），而它们本该一起变。Row 上那几样
// 也跟着过来：这一行**就是**最新一次尝试（Latest 恒为真），而 Running 与
// RunningLoad 正是「取中的是哪一代、它上次装上了没有」的答案——这一页比清单更
// 需要它们。
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
// 不是同一件事：加载器走 activeRows，只在同一个 key 里取状态为 active 且代次最高的
// 那一行。发布了新版本还没 Enable 时，这里给的是那一版新行（状态 pending），
// 而跑着的仍是上一代——所以这一行带着 Row.Running，两个问题一次答完。
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
	query := `
		SELECT p.key, p.version, p.name, p.status, p.generation,
		       COALESCE((SELECT MAX(a.priority) FROM plugin_activations a
		                  WHERE a.plugin_key = p.key AND a.scope = 'platform'), 0),
		       p.meta, p.created_by, p.created_at,
		       (SELECT count(*) FROM plugins c WHERE c.key = p.key),
		       p.compile_error, r.version, r.generation,
		       r.last_load_result, r.last_load_error, r.last_load_at
		FROM plugins p` + runningJoin() + `
		 WHERE p.key = $1
		 ORDER BY ` + pickedOrder("p") + `
		 LIMIT 1`

	var (
		d           Detail
		meta        []byte
		createdBy   sql.NullInt64
		compileErr  sql.NullString
		runVersion  sql.NullString
		runGeneratn sql.NullInt64
		runResult   sql.NullString
		runErrText  sql.NullString
		runAt       sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, query, key).Scan(
		&d.Key, &d.Version, &d.Name, &d.Status, &d.Generation, &d.Priority,
		&meta, &createdBy, &d.CreatedAt, &d.Versions,
		&compileErr, &runVersion, &runGeneratn, &runResult, &runErrText, &runAt)
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
	d.CompileError = compileErr.String
	// 这一行就是这个 key 里代次最高的那一行（上面 ORDER BY … LIMIT 1），所以它
	// 一定是「最新一次尝试」。
	d.Latest = true
	d.Running = generationOrNil(runVersion, runGeneratn)
	d.RunningLoad = loadAttemptFrom(runResult, runErrText, runAt)
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
//
// 排序键与「取中哪一行」那几处共用（见 pickedOrder）：代次撞车时（覆盖发布的
// 副产物）只按代次排是任意的，可能抬起一行而加载器读的是另一行——那一次 enable
// 看起来成功了，跑的却还是原来那一代。
func (s *Store) Enable(ctx context.Context, key string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE plugins SET status = $2
		  WHERE id = (SELECT id FROM plugins WHERE key = $1
		               ORDER BY `+pickedOrder("plugins")+` LIMIT 1)`,
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
