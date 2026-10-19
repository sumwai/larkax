package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/textshape"
)

// 渠道状态里运维能设置的两个取值。
//
// 表里的 CHECK 还允许 'drained'，但它**不在**这里：那是观测到的状态
// （上游说自己没额度了，由额度监控写入），不是运维的决定。让管理面能把它设上，
// 等于允许把一条观测记录伪造成配置。
const (
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
)

// ErrChannelNotFound 表示要改的那条渠道不在库里。
//
// 与「改不动」分开：前者是 id 写错了或者那条已经被删了，后者是数据库的问题。
// 没有这个区分的话，一次拼错 id 的调用会静默成功，而人以为改完了。
var ErrChannelNotFound = errors.New("渠道不存在")

// ErrNoChanges 表示这次修改没有任何字段要改。
//
// 单独报错而不是发一条只动 updated_at 的 UPDATE：后者会让「调用方拼错了
// 结构体」看起来像一次成功的修改。
var ErrNoChanges = errors.New("没有要改的字段")

// ErrChannelDeleted 表示这条渠道已经被软删，不能再改。
//
// 与 ErrChannelNotFound 分开，是因为两者的下一步动作不同：那一条是 id 写错了
// （重新列一遍拿现存的 id），这一条是「行还在，但它已经不在使用中」——要把它
// 变回可用得走 Restore，不是改 id 能解决的。
//
// 写路径（Update / SetStatus）撞上它时**一行都不改**：静默改一行用户看不见的
// 数据，会让「我已经删掉它了」与「它又被改回来了」在库里分不出来。
var ErrChannelDeleted = errors.New("渠道已删除")

// Admin 是渠道的管理面：创建、修改、列举、启停。
//
// 它不在请求热路径上，也不改变 Source.Candidates 的行为——两者读的是同一套列，
// 由同一批辅助函数解码，所以「写进去的」与「读出来的」不会各自漂移。
//
// # 为什么是单独一个类型，而不是挂在 Source 上
//
// Source 在请求热路径上——每个请求都会调 Candidates。管理面只在运维动作里
// 被调用，而它需要的东西（适配器注册表）读路径根本不需要。分成两个类型之后，
// 热路径那个类型**拿不到**写入能力：这是编译期的事实，不是一句约定。
//
// # 凭证只封不开
//
// 本类型不调用、也不该调用 credential.Sealer.Open。全仓库唯一解出明文凭证的
// 地方是 Source.decrypt，那是调用上游时的必需步骤；写入侧只需要 Seal。
// 每多一处能拿到明文的地方，日志与错误信息的去向就更难控制。
//
// 明文在这里的生命周期只有三步：交给 Seal → 拿到密文 → 落库。它不进日志、
// 不进错误信息、不进返回值；报错一律说「哪个字段不对」，不回显值。
type Admin struct {
	db       *sql.DB
	sealer   *credential.Sealer
	registry *channel.Registry

	// catalog 读模型目录里那条端点的声明（见 checkModelEndpoints）。
	//
	// 由 NewAdmin 现建一个，与 Source 上那个是**同一个包的同一个读取面**，
	// 只是各拿一份：两者都不持有状态，包着同一个 *sql.DB。不把它做成构造参数
	// 是为了不改 NewAdmin 的签名——那会波及装配处（internal/assembly）与
	// cmd/，而它们不需要知道这件事。
	catalog *catalog.Catalog
}

// NewAdmin 包装数据库句柄、主密钥封装器与适配器注册表。
//
// registry 为 nil 时创建与改类型都会报错，而不是 panic：一个拼错构造参数的
// 管理面调用不该把进程带走。
func NewAdmin(db *sql.DB, sealer *credential.Sealer, registry *channel.Registry) *Admin {
	return &Admin{db: db, sealer: sealer, registry: registry, catalog: catalog.New(db)}
}

// CreateRequest 是新建一条渠道所需的全部字段。
type CreateRequest struct {
	// OwnerUserID 为 0 表示平台自营，库里存 NULL（route 把两者都看成 0）。
	OwnerUserID int64

	Name string
	// Type 必须是一个已注册的适配器类型，见 internal/channel 的注册表。
	Type    string
	BaseURL string

	// Credential 是**明文**凭证。它只用于封装，不落日志、不进错误信息、
	// 不进返回值。
	Credential string

	// Models 是该渠道支持的规范模型名，至少要有一个。
	Models []string

	// Weight 为 0 表示用默认值 1。
	Weight int

	Priority int
	Setting  map[string]any
}

// Create 新建一条渠道，返回它的 id。
//
// 新建的渠道一律是 enabled：读取路径只取 enabled 的行，一条建出来就不可选的
// 渠道除了占位没有别的用途。要建完立刻停用，就再调一次 SetStatus。
func (a *Admin) Create(ctx context.Context, req CreateRequest) (int64, error) {
	name, err := normalizeName(req.Name)
	if err != nil {
		return 0, err
	}
	if err = a.validateType(req.Type); err != nil {
		return 0, err
	}
	baseURL, err := NormalizeBaseURL(req.BaseURL)
	if err != nil {
		return 0, err
	}
	models, err := normalizeModels(req.Models)
	if err != nil {
		return 0, err
	}
	// 端点的校验排在封凭证之前：校验失败时这次调用根本没碰到明文
	// （见下面 sealCredential 那一处的理由）。
	if err = a.checkModelEndpoints(ctx, req.Type, models); err != nil {
		return 0, err
	}
	weight, err := normalizeWeight(req.Weight)
	if err != nil {
		return 0, err
	}
	setting, err := encodeSetting(req.Setting)
	if err != nil {
		return 0, err
	}
	// 封凭证放在所有校验之后：校验失败时这次调用根本没碰到明文。
	envelope, err := a.sealCredential(req.Credential)
	if err != nil {
		return 0, err
	}

	// owner_user_id 是外键且可空。用 any 传值而不是传 0：
	// 0 会去匹配 id 为 0 的用户，而那个用户不存在——外键会直接拒绝，
	// 于是「平台自营」这条路径根本走不通。
	var owner any
	if req.OwnerUserID != 0 {
		owner = req.OwnerUserID
	}

	var id int64
	err = a.db.QueryRowContext(ctx, `
		INSERT INTO channels
			(owner_user_id, name, type, base_url, credential_enc, credential_hint,
			 status, models, weight, priority, setting)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		RETURNING id`,
		owner, name, req.Type, baseURL, envelope, credential.Hint(req.Credential),
		StatusEnabled, models, weight, req.Priority, string(setting)).Scan(&id)
	if err != nil {
		// owner_user_id 上有一条指向 users 的外键：解析完归属用户、插入之前
		// 那个用户被删，这里拿到的是 23503。本包没有为它准备具名哨兵——
		// 这条路径要回答的只是「数据库因为约束拒绝了这次写入」，兜底哨兵够用，
		// 退出码也因此落在 4 而不是 1。
		return 0, dberr.ConstraintFailure(fmt.Errorf("创建渠道失败: %w", err))
	}
	return id, nil
}

// UpdateRequest 描述一次修改。
//
// 每个字段都是指针：nil 表示「不改这一项」，非 nil 表示「改成这个值」。
// 用指针而不是零值是因为零值本身有意义——priority 改成 0、setting 改成空对象
// 都是合法修改，与「没提到这个字段」必须区分开。
type UpdateRequest struct {
	ID int64

	Name     *string
	Type     *string
	BaseURL  *string
	Models   *[]string
	Weight   *int
	Priority *int
	Setting  *map[string]any

	// Credential 非 nil 时重新封装并替换密文。
	//
	// **为 nil 时生成的 SQL 里根本不出现 credential_enc**，密文一个字节都不动。
	// 这一条是有意的：重封一次会换掉 DEK，密文变了而凭证没变，事后审计时
	// 「这条渠道的凭证什么时候换过」就再也答不出来。
	Credential *string
}

// Update 按 UpdateRequest 修改一条渠道。
func (a *Admin) Update(ctx context.Context, req UpdateRequest) error {
	if req.ID <= 0 {
		return errors.New("渠道 id 无效")
	}

	// 只把真正要改的列拼进 SET：没提到的列连现在是什么值都不必知道。
	var sets []string
	var args []any
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}

	// 归一化一次模型清单：这一支要拿它写 SET，下面的端点校验也要用它算
	// 「改完之后这条渠道挂着哪些模型」。没给 models 时保持 nil。
	var newModels []string

	if req.Name != nil {
		name, err := normalizeName(*req.Name)
		if err != nil {
			return err
		}
		add("name", name)
	}
	if req.Type != nil {
		if err := a.validateType(*req.Type); err != nil {
			return err
		}
		add("type", *req.Type)
	}
	if req.BaseURL != nil {
		baseURL, err := NormalizeBaseURL(*req.BaseURL)
		if err != nil {
			return err
		}
		add("base_url", baseURL)
	}
	if req.Models != nil {
		models, err := normalizeModels(*req.Models)
		if err != nil {
			return err
		}
		newModels = models
		add("models", models)
	}

	// ── 端点校验 ────────────────────────────────────────────────────────
	//
	// Type 与 Models **任何一个**变了都要重判，因为两者各自都会改变「哪些
	// 模型挂在这条渠道上、它往哪条路径发」：
	//
	//	只改 models  可能把一个这条端点服务不了的模型挂进来
	//	只改 type    会把**已有的整批模型**从一条端点挪到另一条上
	//
	// 只在 Models 那一支里判会漏掉后者，而那个缺口更隐蔽——它不需要动
	// models，光看这份改动看不出渠道的模型会落到另一条端点上。
	//
	// 判的是**改完之后**的值：给了就用给的，没给读库里当前那一条（见
	// attachmentAfter）。渠道不存在或已删时跳过，让下面那条 UPDATE 去报
	// ——它分得出「id 打错了」与「已经删了」，在这里先报一次反而少一层区分。
	if req.Type != nil || req.Models != nil {
		channelType, models, ok, err := a.attachmentAfter(ctx, req, newModels)
		if err != nil {
			return err
		}
		if ok {
			if err := a.checkModelEndpoints(ctx, channelType, models); err != nil {
				return err
			}
		}
	}
	if req.Weight != nil {
		weight, err := normalizeWeight(*req.Weight)
		if err != nil {
			return err
		}
		add("weight", weight)
	}
	if req.Priority != nil {
		add("priority", *req.Priority)
	}
	if req.Setting != nil {
		setting, err := encodeSetting(*req.Setting)
		if err != nil {
			return err
		}
		add("setting", string(setting))
	}
	if req.Credential != nil {
		envelope, err := a.sealCredential(*req.Credential)
		if err != nil {
			return err
		}
		add("credential_enc", envelope)
		// 提示与密文一起换：留着旧提示会让清单上的「末四位」指向
		// 一把已经不在这条渠道上的凭证——界面拿它做辨识，而它认错了人。
		add("credential_hint", credential.Hint(*req.Credential))
	}

	if len(sets) == 0 {
		return ErrNoChanges
	}
	// updated_at 每次都动，于是「改了几个字段」不影响这条语句的行为，
	// 下面也就能用影响行数判断这条渠道在不在。
	sets = append(sets, "updated_at = now()")
	args = append(args, req.ID)

	// 已删的渠道不许改：条件里带上 channelAlive，于是「行不在」与「行已删」
	// 都会让这条 UPDATE 影响 0 行，下面的 explainNoRow 把它们分开报。
	query := "UPDATE channels SET " + strings.Join(sets, ", ") +
		" WHERE id = $" + strconv.Itoa(len(args)) + " AND " + channelAlive
	res, err := a.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("修改渠道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认修改结果失败: %w", err)
	}
	if affected == 0 {
		return a.explainNoRow(ctx, req.ID)
	}
	return nil
}

// explainNoRow 把「这次的 UPDATE 一行都没匹配上」翻译成具体的那一种。
//
// Update 与 SetStatus 的 WHERE 都带 channelAlive，0 行因此有两种含义：
// 没有这个 id，或者这个 id 已经被软删。它们的退出码也不同（一个走「要改的
// 那一行不存在」，一个是别的失败），所以不能统一报 ErrChannelNotFound。
//
// 只在 0 行时才多一条查询：正常路径一次往返，与不带这个条件时一样。
func (a *Admin) explainNoRow(ctx context.Context, id int64) error {
	var deleted bool
	err := a.db.QueryRowContext(ctx,
		`SELECT deleted_at IS NOT NULL FROM channels WHERE id = $1`, id).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：id %d", ErrChannelNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("查渠道 %d 的删除状态失败: %w", id, err)
	}
	if deleted {
		return fmt.Errorf("%w：id %d（用 larkaxctl channel list --all 可以看到已删的渠道，"+
			"用 larkaxctl channel restore 可以把它恢复回来）", ErrChannelDeleted, id)
	}
	// 行还在、没删，却没被 WHERE 匹配上：两次语句之间它刚被删掉。
	// 与「已经删了」是同一个事实，只是晚了一步。
	return fmt.Errorf("%w：id %d", ErrChannelDeleted, id)
}

// SetStatus 启用或停用一条渠道。
//
// 只接受 StatusEnabled 与 StatusDisabled，见那两个常量的说明。
func (a *Admin) SetStatus(ctx context.Context, id int64, status string) error {
	if id <= 0 {
		return errors.New("渠道 id 无效")
	}
	if status != StatusEnabled && status != StatusDisabled {
		return fmt.Errorf("渠道状态 %q 不是管理面能设置的取值（只接受 %q 与 %q）",
			status, StatusEnabled, StatusDisabled)
	}

	res, err := a.db.ExecContext(ctx,
		`UPDATE channels SET status = $2, updated_at = now() WHERE id = $1 AND `+channelAlive, id, status)
	if err != nil {
		return fmt.Errorf("设置渠道状态失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认设置结果失败: %w", err)
	}
	if affected == 0 {
		return a.explainNoRow(ctx, id)
	}
	return nil
}

// SoftDelete 把一条渠道标记为已删除，返回它是不是**这次才**删掉的。
//
// 已删的再删一次不是错误（返回 false）：删除是幂等的，脚本因此可以重复执行。
// 这与「id 不存在」不同——那一条仍然报 ErrChannelNotFound，因为 id 写错时
// 幂等会让一次打错的调用看起来像成功。
//
// # 它不删任何从属的行
//
// 七条外键是 ON DELETE CASCADE，真删会带走额度状态、健康汇总、别名、计价
// 规则、上游成本与插件激活；billing_requests.channel_id 没有 ON DELETE，
// 真删直接撞外键。软删让这些历史行继续认得这条渠道——对账与快照的可追溯性
// 靠的正是它。所以这里只改两列：
//
//   - deleted_at 置 now()，这一列是删除状态的**唯一**判据；
//   - status 顺带置成 disabled。它不是第二份判据，是给漏网读路径的兜底：
//     选路要求 status = 'enabled'，于是即便某处忘了带 channelAlive，被删的
//     渠道也选不中。它也让 channels_status_idx 不再为这行留位置。
//
// 凭证密文不动：作废的凭证留在备份里是已知代价（见本 issue 的取舍），而
// 清空它会让「这条渠道当时用的是什么凭证」在对账时无从查起。
//
// # 直接写 now() 而不是收一个时刻参数
//
// 删除时刻要能与库里别的时间比较（`deleted_at IS NULL` 只看有没有值，
// 但清单要把它打出来）。由数据库给时刻，就与 created_at / updated_at 同一
// 个时钟，不会出现应用与数据库时区不一致导致「删除早于创建」的行。
func (a *Admin) SoftDelete(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, errors.New("渠道 id 无效")
	}

	res, err := a.db.ExecContext(ctx, `
		UPDATE channels
		   SET deleted_at = now(), status = $2, updated_at = now()
		 WHERE id = $1 AND `+channelAlive,
		id, StatusDisabled)
	if err != nil {
		return false, fmt.Errorf("删除渠道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("确认删除结果失败: %w", err)
	}
	if affected == 1 {
		return true, nil
	}

	// 0 行：要么没有这个 id，要么它已经删了。两者要分开报——前者是打错了
	// id，后者是幂等。
	var exists bool
	err = a.db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM channels WHERE id = $1`, id).
		Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w：id %d", ErrChannelNotFound, id)
	}
	if err != nil {
		return false, fmt.Errorf("查渠道 %d 的删除状态失败: %w", id, err)
	}
	// 行在，且上面那条带 channelAlive 的 UPDATE 没匹配上：它已经删了。
	return false, nil
}

// Get 读一条渠道的详情，**已软删的也读**。
//
// # 为什么已删的也读得出来
//
// 清单（List）默认不列已删的渠道，那个开关管的是「清单要不要变长」。按 id 读一条
// 是另一件事：行还在库里，而单条读取的目的是核对配置——藏起来只会让人打不开，
// 逼他去翻 --all 或者直接看库。已删与否由返回值的 DeletedAt 说出来，调用方据此
// 在输出里标出它。
//
// 写路径对已删渠道的处置不照这条走，而且彼此不同：Update / SetStatus 撞上已删的
// 渠道要拒绝，理由是「不静默改一行用户看不见的数据」（见 ErrChannelDeleted）；
// Restore 恰恰相反，它是唯一**接受**已删渠道的写路径——把 deleted_at 清掉正是它
// 的职责。
func (a *Admin) Get(ctx context.Context, id int64) (ChannelInfo, error) {
	if id <= 0 {
		return ChannelInfo{}, errors.New("渠道 id 无效")
	}
	row := a.db.QueryRowContext(ctx, listQuery+"\n WHERE c.id = $1", id)
	info, err := scanInfo(row)
	if errors.Is(err, sql.ErrNoRows) {
		// 与「改不动」分开：这里要答的是「有没有这一行」，没有就是 id 写错了。
		return ChannelInfo{}, fmt.Errorf("%w：id %d", ErrChannelNotFound, id)
	}
	if err != nil {
		return ChannelInfo{}, err
	}
	return info, nil
}

// Restore 把一条软删的渠道恢复回可用，返回它是不是**这次才**恢复的。
//
// 它是 SoftDelete 的逆操作，写三处：
//
//   - deleted_at 置回 NULL，这一列是删除状态的唯一判据；
//   - status 置成 enabled。删除时那一列被改成了 disabled 当兜底（见 SoftDelete），
//     所以「删之前是不是运维主动停用过」在库里已经不存在了，还原不回来。恢复
//     成 disabled 的渠道会出现在清单上却选不中——那正是这类「看起来好了、实际
//     没好」的错，所以这里给的是 enabled，也就是「恢复回可用」的字面意思。
//   - channel_quota_state.health 置回 unknown。它上一次的结论是**软删之前**的，
//     直接沿用等于把一段已经过去的历史当成现在的判断；而这条渠道在删除期间没有
//     任何流量，谁都无法说它现在好不好。没有额度状态行时什么都不写——没有行本来
//     就读作 unknown，补一行空状态只是一次没有信息量的写。
//
// 连续失败计数（channels.consecutive_failures / last_failure_at）不动。那个计数
// 自带冷却期：读取侧对「最后一次失败早于 15 分钟」的行按 0 处理（见
// streakCooldown），而软删到恢复之间几乎一定超过 15 分钟，所以恢复后它自动不算数。
//
// # 已经没被删的再恢复一次不是错误
//
// 返回 false 表示这一行本来就没删，与「id 不存在」不同：后者仍然报
// ErrChannelNotFound，因为 id 写错时幂等会让一次打错的调用看起来像成功。这与
// SoftDelete 的口径一致。
func (a *Admin) Restore(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, errors.New("渠道 id 无效")
	}

	// 两处写在一个事务里：恢复一半（渠道回来了、健康结论还是软删前那个
	// drained）留下的正是这条改动要消除的那种状态——库上看起来恢复了，
	// 而选路会照旧摘掉它。
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("开启恢复渠道事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE channels
		   SET deleted_at = NULL, status = $2, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NOT NULL`,
		id, StatusEnabled)
	if err != nil {
		return false, fmt.Errorf("恢复渠道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("确认恢复结果失败: %w", err)
	}
	if affected == 0 {
		// 0 行：要么没有这个 id，要么它本来就没删。先回滚，再分开报——两次
		// 查询报的是同一件事，而这里分得出「id 打错了」与「本来就是可用的」。
		_ = tx.Rollback()
		return false, a.explainNotDeleted(ctx, id)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE channel_quota_state SET health = $2, updated_at = now() WHERE channel_id = $1`,
		id, HealthUnknown); err != nil {
		return false, fmt.Errorf("把渠道 %d 的健康结论置回 %s 失败: %w", id, HealthUnknown, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("提交恢复渠道失败: %w", err)
	}
	return true, nil
}

// explainNotDeleted 把「这次 UPDATE 一行都没匹配上」翻译成具体的那一种。
//
// 与 explainNoRow 是同一件事的另一侧：那一处的 WHERE 带 channelAlive，0 行意味着
// 「不存在」或「已删」；这里的 WHERE 要求 deleted_at 非空，0 行意味着「不存在」或
// 「没删」。两种都不该统一报一个错——前者要重新拿 id，后者是一条正常的幂等调用。
func (a *Admin) explainNotDeleted(ctx context.Context, id int64) error {
	var deleted bool
	err := a.db.QueryRowContext(ctx,
		`SELECT deleted_at IS NOT NULL FROM channels WHERE id = $1`, id).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：id %d", ErrChannelNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("查渠道 %d 的删除状态失败: %w", id, err)
	}
	// 行在、没删：这次恢复是一次幂等重跑，不是失败。
	return nil
}

// ChannelInfo 是管理面看到的一条渠道。**它不含密文，也不含明文凭证。**
//
// 字段集合由 admin_test.go 的一条用例逐个钉住：加字段会先让那条用例失败，
// 于是「这个新字段该不该出现在管理面的返回值里」会被问一次。
// 这正是本类型存在的意义——列举的目的是核对配置，不是把密钥搬来搬去。
type ChannelInfo struct {
	ID          int64
	OwnerUserID int64
	Name        string
	Type        string
	BaseURL     string
	Status      string
	Models      []string
	Weight      int
	Priority    int
	Setting     map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// CredentialKeys 是这份密文被哪些主密钥包过，见 credential.WrappedWith。
	//
	// 它回答的是运维真正会问的问题：「这条渠道挂在哪把钥匙下、轮换补到哪一步了」。
	// 列出它不需要任何密钥材料，也不需要解密。
	CredentialKeys []string

	// Hint 是凭证的末四位（见 credential.Hint），供界面辨识。
	//
	// 它与 CredentialKeys 回答的是两个不同的问题：那一栏说「挂在哪把钥匙下」，
	// 这一栏说「挂的是我刚发的那个凭证吗」。空表示这一行没有提示——建渠道时
	// 没写、或凭证短到没有末四位可辨。
	//
	// 它**不是**凭证的一部分：库里存的仍然只有密文，这一列是明文尾部几个字符，
	// 由写入路径在封装之前顺手取下来（密文一旦生成就再也取不到了）。
	Hint string

	// CredentialErr 说明 CredentialKeys 为什么是空的。为 nil 表示信封头部解析正常。
	//
	// 它记的是**结构**问题：密文被截断、被手工改过。列举不碰钥匙，
	// 所以「解不开」在这里根本不会发生。
	CredentialErr error

	// DeletedAt 是这条渠道被软删的时刻，零值表示没删。
	//
	// 只有 ListAll（--all）看得见非零值：默认清单按软删过滤，所以那里的
	// 每一行都是零值。它是清单上「已删」那一栏的唯一依据——把已删的行
	// 与「停用」的行混在一起列出来，人分不出哪一行还能恢复使用。
	DeletedAt time.Time
}

// List 列出**没有软删**的渠道，按 id 排序。
//
// 已删的渠道只在运维明确要看的时候出现（见 ListAll）：它们仍然是历史行
// （billing_requests / usage_events 引用着），但不再代表任何可用的配置。
//
// # 一行密文坏了不影响别的行
//
// WrappedWith 只解析信封头部，不碰钥匙，所以它失败只可能是这一行的密文本身
// 不合法。那种行仍然列出来，原因写在那一行的 CredentialErr 上——**不跳过、
// 也不整次报错**。
//
// 整次报错是最先写的那一版，实际用起来是错的：库里有**一行**手工写进去的
// 密文（那种 `'\x00'::bytea` 占位符），整个清单就一条都看不到，而那正是最需要
// 看清单的时候。这与读取路径对凭证解不开的处置一致（见 Candidates：
// 一个渠道的密文坏了不该让同模型下别人的渠道一起不可用），区别只在于
// 这里连日志都不必——调用方手里就有那一行。
//
// 同一个函数里 setting 解析失败却是整次报错（见 scanInfo），不是双标：
// setting 是 jsonb，**合法性由列类型保证**，解不开说明这一行根本不是按
// 表定义写进去的；credential_enc 是 bytea，没有任何约束拦得住一行垃圾，
// 而它坏掉只影响它自己那一栏。
func (a *Admin) List(ctx context.Context) ([]ChannelInfo, error) {
	return a.list(ctx, false)
}

// ListAll 与 List 相同，但**含已删的渠道**（larkaxctl 的 list --all）。
//
// 它给的是同一个形状的结果，已删的行在 DeletedAt 上非零——调用方据此标出
// 它们，而不是靠「status 是不是 disabled」猜：停用是运维的临时决定，删除
// 是另一件事。
func (a *Admin) ListAll(ctx context.Context) ([]ChannelInfo, error) {
	return a.list(ctx, true)
}

func (a *Admin) list(ctx context.Context, includeDeleted bool) ([]ChannelInfo, error) {
	query := listQuery
	if !includeDeleted {
		query += "\n WHERE c." + channelAlive
	}
	query += "\n ORDER BY c.id"
	rows, err := a.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询渠道失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：没有渠道是正常结果（还没配），不是「没有值」。
	out := []ChannelInfo{}
	for rows.Next() {
		info, err := scanInfo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历渠道失败: %w", err)
	}
	return out, nil
}

// listQuery 取回管理面要看的全部列。
//
// credential_enc 也在里面，但只用来算 CredentialKeys，不会离开本包：
// 解码之后它变成一串主密钥标识，密文本体到此为止。
//
// credential_hint 是唯一一列**明文**的凭证信息（末四位），它本来就是给界面
// 看的，所以照原样带出去。这一条与「只存密文」不矛盾：密文仍然是唯一的
// 凭证本体，提示是写入时顺手取下的尾部字符，解不出任何东西。
//
// 列名与 candidateQuery 保持同一套（models 同样用 array_to_json 转成 JSON 文本，
// 因为 database/sql 不认 text[]）——两处对同一列的读法不一致时，
// 症状是「写入路径存进去的值读出来不对」，而它只在某些行上出现。
//
// 提示只有 listQuery 取，candidateQuery 不取：候选集是推理热路径，
// 多带一列明文尾部没有任何用处。
const listQuery = `
SELECT
    c.id,
    COALESCE(c.owner_user_id, 0),
    c.name,
    c.type,
    c.base_url,
    c.status,
    array_to_json(c.models)::text,
    c.weight,
    c.priority,
    c.setting,
    c.credential_enc,
    c.credential_hint,
    c.created_at,
    c.updated_at,
    c.deleted_at
  FROM channels c`

// rowScanner 是 *sql.Rows 与 *sql.Row 共用的那一小段：两者都只有一个 Scan，
// 于是同一段解码能同时给「列一批」与「按 id 取一行」用。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInfo(rows rowScanner) (ChannelInfo, error) {
	var (
		info       ChannelInfo
		modelsJSON string
		settingRaw []byte
		envelope   []byte
		hint       sql.NullString
		deletedAt  sql.NullTime
	)
	err := rows.Scan(
		&info.ID, &info.OwnerUserID, &info.Name, &info.Type, &info.BaseURL,
		&info.Status, &modelsJSON, &info.Weight, &info.Priority, &settingRaw,
		&envelope, &hint, &info.CreatedAt, &info.UpdatedAt, &deletedAt,
	)
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("读取渠道行失败: %w", err)
	}
	// NULL 折成零值时间：Go 侧只问「删了没」，见 ChannelInfo.DeletedAt。
	info.DeletedAt = deletedAt.Time
	// NULL 与空串在这里合并成同一个值：这一列没有「空提示」与「没有提示」
	// 的区分——凭证短到不足四位时 credential.Hint 原样返回，那种串也不会是
	// 空的（写入路径拒绝空凭证）。
	info.Hint = hint.String

	models, err := decodeModels(info.ID, modelsJSON)
	if err != nil {
		return ChannelInfo{}, err
	}
	info.Models = models

	setting, err := decodeSetting(settingRaw)
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("渠道 %d 的 setting 不是合法 JSON: %w", info.ID, err)
	}
	info.Setting = setting

	keys, err := credential.WrappedWith(envelope)
	if err != nil {
		info.CredentialErr = fmt.Errorf("凭证密文不合法: %w", err)
	} else {
		info.CredentialKeys = keys
	}

	return info, nil
}

// ── 校验与归一化 ────────────────────────────────────────────────────────
//
// 这一批函数是写入路径与读取路径「同一套列」的兑现方式：值的形态在写入侧
// 定下来，读取侧按同一个假设解。校验失败时只报**哪个字段不对**，不回显值——
// 凭证字段固然不能回显，其余字段也用同一套写法，免得后来把某个字段换成
// 凭证时忘了改这一处。

// attachmentAfter 给出这次修改之后这条渠道挂在哪个类型、哪些模型上。
//
// newModels 是调用方归一化过的新模型清单，为 nil 表示这次不改模型；类型同理
// 取 req.Type。没给的那一项读库里当前的值——因为**两者都会单独改变**「这批
// 模型落在哪条端点上」：只改 models 可能把服务不了的模型挂进来，只改 type
// 会把已有的整批模型挪到另一条路径上。
//
// ok 为 false 表示这条渠道不存在或已删：调用方据此跳过端点校验，让随后那条
// UPDATE 去报 ErrChannelNotFound / ErrChannelDeleted——两次报的是同一件事，
// 而 UPDATE 那次分得出「id 打错了」与「已经删了」。
func (a *Admin) attachmentAfter(ctx context.Context, req UpdateRequest, newModels []string) (string, []string, bool, error) {
	var (
		channelType string
		modelsJSON  string
	)
	err := a.db.QueryRowContext(ctx,
		`SELECT type, array_to_json(models)::text FROM channels WHERE id = $1 AND `+channelAlive,
		req.ID).Scan(&channelType, &modelsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("查渠道 %d 的类型与模型失败: %w", req.ID, err)
	}
	models, err := decodeModels(req.ID, modelsJSON)
	if err != nil {
		return "", nil, false, err
	}
	if req.Type != nil {
		channelType = *req.Type
	}
	if newModels != nil {
		models = newModels
	}
	return channelType, models, true, nil
}

// checkModelEndpoints 确认这批模型能在本条渠道的端点上被服务。
//
// # 它拦的是什么
//
// 模型目录里可以声明「这个模型只在这些端点上被服务」（见 catalog.Capabilities
// 的 Endpoints）。把这样一个模型挂到一条端点数对不上的渠道上，请求会在
// **上游**那里失败：400 unsupported_model，从我们这边看与「模型不存在」长得
// 一样，而排查方向指向对方。挂错的那条渠道本身看不出任何问题——它 status 是
// enabled、模型在 models 白名单里、别的渠道也在用同一份配置。
//
// 所以挡住写入侧：这是配置错误，而配置错误该在写入时说话。
//
// 判据与措辞都收在 endpoints.go 的 EndpointFit 上，doctor 排查时按**同一份**
// 判（同样的三档、同一张「渠道类型 → 端点」表）——两处各写一份的症状是
// 「建的时候放行、doctor 报错」，比不校验更坏。
//
// # 只在「目录里明确说了端点」且「对不上」时拒绝
//
//   - 目录里没有这个模型 → 放行（未知）
//   - 端点那一项是 NULL（没配过）→ 放行（未知）
//   - 目录里那条记录说 `{}`（明确一条都不支持）→ 拒绝：任何渠道都服务不了它
//
// 两条放行与「不知情就不拦」一致（同 route 的端点评据）：目录是**人工维护、
// 会过时**的一份声明，刚接一家上游时它多半还是空的，那时按「不支持」处理会
// 让渠道一条都建不出来。
//
// # 读目录失败要报错，不降级成「未知」
//
// 与 Source.profile 同一条理由：降级看起来更宽容，实际是把一次查询故障变成
// 「判据静默失效」——而失效与「这个部署没配目录」在日志里分不开。这条校验
// 与这次写入打的是同一个库，读失败说明问题不在这一条配置上。
//
// # 为什么走一次库
//
// 管理面不在请求热路径上，而 Create / Update 本来就已经在写库了；多读几行
// （每个模型一行）不改变这条路径的量级。反过来，「写入时不查」意味着那条
// 配置要等到第一次请求才暴露，而那时它已经挂在库上了。
func (a *Admin) checkModelEndpoints(ctx context.Context, channelType string, models []string) error {
	for _, model := range models {
		declared, err := DeclaredEndpoints(ctx, a.catalog, model)
		if err != nil {
			return err
		}
		fit := EndpointFitFor(channelType, declared)
		if !fit.Mismatch() {
			continue
		}
		// 判据（EndpointFit.Mismatch）与措辞（Summary/Reason）都从同一处来：
		// 它们与 doctor 报的是同一件事（见 internal/channels/endpoints.go）。
		// 这里只补主语「模型 "m"」，与 doctor 那半句拼的是同一截正文，所以
		// 两处不会各说一个版本。
		return fmt.Errorf("模型 %q %s：%s", model, fit.Summary(), fit.Reason())
	}
	return nil
}

// validateType 确认这个渠道类型有对应的适配器。
func (a *Admin) validateType(channelType string) error {
	if a.registry == nil {
		return errors.New("没有适配器注册表，无法校验渠道类型")
	}
	if _, err := a.registry.Lookup(channelType); err != nil {
		// 库里存一个没人实现的类型，选路时会表现为「这个渠道永远选不中」——
		// 一条看起来配好了、实际不可用的渠道。所以挡在写入侧。
		return fmt.Errorf("渠道类型无效: %w", err)
	}
	return nil
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("渠道名不能为空")
	}
	// 渠道名进 `channel list` 与 `channel health` 的「名称」那一栏，两处都按
	// 一栏一行排版。判在 trim 之后的值上，与落库的那个串一致。
	if err := textshape.Validate("--name（渠道名）", name); err != nil {
		return "", err
	}
	return name, nil
}

// NormalizeBaseURL 校验上游地址。
//
// 只认 http/https：没有 scheme 的地址会一路走到 httpx.Endpoint，在那里拼出
// 一个说不清哪里错的 URL（见那个函数的注释），报错时离「配置写错了」已经很远，
// 而排查要从几百个渠道里找出是哪一个。
//
// # 为什么导出
//
// 命令行在**连库之前**要跑一遍同一个判据：一个写着坏地址的命令不该先连上生产库
// 再报错，而参数错该给的是参数错的退出码（见 cmd/larkaxctl/exit.go 的 usageErr）。
// 写入路径自己也调它，所以绕过命令行的调用方（将来的管理面、脚本）同样写不进坏
// 地址——两处共用这一个实现，判据不会漂移。同一条路上的先例见 auth.ParseRole
// 与 settings.EncodeValue。
//
// # query 与 fragment 一律拒
//
// httpx.Endpoint 是**字符串拼接**：它只看结尾斜杠，看不见 `?` 与 `#`（见那个
// 函数的注释）。于是
//
//	https://api.example.com/v1?tenant=abc + /v1/chat/completions
//	  → https://api.example.com/v1?tenant=abc/v1/chat/completions
//
// 端点路径被接到 query 后面，请求打到的地方根本不是模型接口；`#` 之后的部分
// 则压根不发给上游。两种都是**安静的坏配置**——配置写错的人要等到请求失败（或者
// 更糟：请求成功了，但打的不是那个端点）才知道，那时离写下它已经很远。所以在
// 写入侧挡住，并且说清是哪一部分不合法。
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("base_url 不能为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("base_url 不是合法的 URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base_url 必须以 http:// 或 https:// 开头")
	}
	if parsed.Host == "" {
		return "", errors.New("base_url 里没有主机名")
	}
	// ForceQuery 也要判：`…/v1?` 这种问号后面什么都没有的写法在 RawQuery 上是
	// 空串，拼出来的地址照样是坏的（`…/v1?/v1/chat/completions`）。
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New(
			"base_url 不能带 query（`?` 起的那一段）：端点路径会被接在 query 后面，" +
				"拼出来的地址是坏的。上游地址写到路径为止")
	}
	// 判 fragment 要比 query 多看一处原文：`…/v1#` 这种**空的** fragment 在
	// net/url 里 Fragment 与 RawFragment 都是空串，连 String() 都把那个 `#` 丢掉，
	// 除了「原文以 `#` 收尾」没有别的信号。`#` 不在收尾时 Fragment 必定非空，
	// 两个条件是互补的，不是重复判一次。
	if parsed.Fragment != "" || strings.HasSuffix(raw, "#") {
		return "", errors.New(
			"base_url 不能带 fragment（`#` 起的那一段）：端点路径会被接在它后面，" +
				"而 `#` 之后的部分不会发给上游。上游地址写到路径为止")
	}
	return raw, nil
}

// normalizeModels 校验模型名列表。
//
// # 空数组不是「支持全部模型」
//
// 读取路径把 models 当白名单用：`models @> ARRAY[规范名]`。空数组匹配不到任何
// 模型名，于是这条渠道永远选不中。读取路径里**没有**「空即全部」这个语义，
// 所以这里不能替它发明一个——放进去的行会是一条安静的废配置。
func normalizeModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, errors.New("models 至少要有一个模型名：空数组匹配不到任何模型，这条渠道永远选不中")
	}
	out := make([]string, 0, len(models))
	for i, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, fmt.Errorf("models 里第 %d 个模型名为空", i+1)
		}
		// 这一栏在 `channel list` 里按逗号连起来打成「模型」那一栏：一个带
		// 换行的模型名会在表里折出续行，带 ESC 的则直接进终端。
		if err := textshape.Validate(fmt.Sprintf("--models 里第 %d 个模型名", i+1), m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// normalizeWeight 校验渠道权重。
//
// 表默认值是 1。这里把 0 落成 1，而不是把 0 存进库：今天没有哪个读取方会
// 用到这一列（Weight 不参与排序，见 route.Candidate.Weight），库里留着一个
// 「有含义但没人读」的值，只会让看库的人多翻一处代码。
//
// 负值直接拒绝：它多半是调用方算错了，静默落成 1 会把那个错误藏进库里。
func normalizeWeight(weight int) (int, error) {
	if weight < 0 {
		return 0, errors.New("weight 不能为负数")
	}
	if weight == 0 {
		return 1, nil
	}
	return weight, nil
}

// encodeSetting 把 setting 编成 jsonb 的文本形式。
//
// 它**不校验键名**：键归该渠道 type 指向的适配器解释（见 channel.Channel.Setting），
// 而管理面不知道任何适配器认哪些键。拼错一个键因此不会报错，只会静默地不生效——
// 管理面的诚实靠两处提示承担，不靠这里的一条判据（见 channel.SettingUnreadNote）。
func encodeSetting(setting map[string]any) ([]byte, error) {
	if len(setting) == 0 {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(setting)
	if err != nil {
		return nil, fmt.Errorf("setting 不是可序列化的 JSON 对象: %w", err)
	}
	return raw, nil
}

// sealCredential 把明文凭证封成信封密文。
//
// 明文只在参数与返回值之间流动。凭证本身是否合法（非空、不过长）由
// credential.Sealer.Seal 判定，这里不重复一遍：两个地方各写一份判据，
// 改了一处而没改另一处时，「封出来的密文」与「判定为合法的凭证」就不一致了。
// Seal 的错误只说长度，不含明文；这里再包一层，不回显任何东西。
func (a *Admin) sealCredential(plaintext string) ([]byte, error) {
	if a.sealer == nil {
		// 与读取路径同一个错误：两种情况下要处置的都是「这台机器没配主密钥」。
		return nil, fmt.Errorf("%w：本进程没有配置主密钥", ErrNoMasterKey)
	}
	envelope, err := a.sealer.Seal([]byte(plaintext))
	if err != nil {
		return nil, fmt.Errorf("封装凭证失败: %w", err)
	}
	return envelope, nil
}
