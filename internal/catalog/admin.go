package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/overrides"
	"github.com/sumwai/larkax/internal/textshape"
)

// 目录写入面上的分类错误。
//
// 它们与「库连不上」必须分得开：前者是使用者改一个字段就能继续的错误，
// 后者是环境问题。两者混在一条消息里，第一次配目录的人会去改名字，
// 而真正的问题是 DSN 写错了。
var (
	// ErrModelExists 表示目录里已经有这个规范名了。
	ErrModelExists = errors.New("规范模型已存在")

	// ErrCanonicalTaken 表示这个范围下已经有另一个上游名映射到该规范名。
	//
	// 它对应两条**偏**唯一索引（渠道级与平台级各一条，见 00007 迁移）。
	// 这是写入侧最常撞的一条：渠道级与平台级不是同一个范围，所以
	// 「这个渠道下撞了」与「平台级撞了」要分开说——两条消息给人的下一步动作
	// 不一样（一个去改这个渠道，一个去改兜底那条）。
	ErrCanonicalTaken = errors.New("这个范围下已经有另一个上游名映射到该规范名")

	// ErrUpstreamTaken 表示这个上游名在这个渠道里已经映射到另一个规范名。
	//
	// 它是 (channel_id, upstream_model) 那条原唯一键。平台级别名没有这个约束：
	// NULL 不等于 NULL，那条唯一键对 channel_id IS NULL 的行一行都管不到。
	ErrUpstreamTaken = errors.New("这个上游名在这个渠道里已经映射到另一个规范名")

	// ErrChannelNotFound 表示别名要挂的渠道不存在。
	//
	// 与 channels.ErrChannelNotFound 是同一个事实，但在两个包里各有一份：
	// 别名写入撞的是 model_aliases 那条外键，调用方在这里拿到错误之后要做的
	// 事（建渠道）与本包无关，所以不值得让本包去 import channels。
	ErrChannelNotFound = errors.New("渠道不存在")

	// ErrModelInUse 表示这个规范名还被别名或计价规则引用着。
	//
	// 单独一个 sentinel 而不是复用 ErrModelExists：处置不同——那个是换个名字，
	// 这个是先去解除引用。
	ErrModelInUse = errors.New("规范名还被引用，不能删")

	// ErrAliasIDNotFound 表示按 id 定位的那条别名不在库里。
	//
	// 与 ErrAliasNotFound 分开：那个是**读取**路径说的「这个渠道没配这个模型」，
	// 处置是去配一条；这个是「手里这个 id 已经不对了」，处置是重新列一遍拿到
	// 现存的 id。两者的消息也不同，混用会让「id 写错了」看起来像配置缺失。
	ErrAliasIDNotFound = errors.New("别名不存在")

	// ErrNoChanges 表示这次修改没有任何字段要改。
	//
	// 与 channels.ErrNoChanges 同一个理由：单独报错而不是发一条只动 updated_at
	// 的 UPDATE——后者会让「调用方拼错了结构体」看起来像一次成功的修改。
	ErrNoChanges = errors.New("没有要改的字段")
)

// PostgreSQL 的错误码与目录上几条约束的名字。
//
// 约束名硬编码自迁移 00001_init.sql（model_catalog 的 UNIQUE、model_aliases
// 的两条外键与那条原唯一键）与 00007_model_alias_unique.sql（两条偏唯一索引）。
// **改那两个文件就要同步改这里**，否则下面的识别失效。
//
// 失效的方向是安全的：认不出来就按原错误上抛，不会静默当成功，只是少一层
// 分类，报错从「这个渠道下已经有另一个上游名映射到 X 了」退化成一句驱动原文。
//
// 只认错误码不够——23505 的含义是「有某个唯一约束被违反」，分不出撞的是
// 规范名还是上游名，而这两者给人的下一步动作不同。
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"

	modelCanonicalUnique = "model_catalog_canonical_name_key"

	aliasChannelCanonicalUnique  = "model_aliases_channel_canonical_idx"
	aliasPlatformCanonicalUnique = "model_aliases_platform_canonical_idx"
	aliasChannelUpstreamUnique   = "model_aliases_channel_id_upstream_model_key"

	aliasCanonicalFK = "model_aliases_canonical_name_fkey"
	aliasChannelFK   = "model_aliases_channel_fk"
)

// Admin 是模型目录的管理面：创建、修改、删除规范模型，增删改别名。
//
// 它不在请求热路径上——那是 Catalog 的事。分成两个类型之后，热路径那个类型
// **拿不到**写入能力，而管理面这个拿不到选路要用的查询：两者读的是同一套列，
// 由同一批辅助函数解码，所以「写进去的」与「读出来的」不会各自漂移。
//
// # 规范名是计费规则挂靠的键
//
// pricing_rules.model_patterns 按规范名匹配（见 internal/pricing），所以
// 本类型对规范名的**改名与删除**特别保守：
//
//   - 改名不做。别名那一侧有外键拦着，但计价规则那一侧是 jsonb 数组、
//     没有任何外键——改名之后那些规则会静默地匹配不到任何请求，计价回落到
//     兜底规则，而账单看上去完全正常。要改名就显式走三步：建新名、把别名与
//     规则逐条挪过去、再删旧名，每一步都看得见。
//   - 删除只在无人引用时允许，见 Delete。
type Admin struct {
	db *sql.DB
}

// NewAdmin 包装数据库句柄。
func NewAdmin(db *sql.DB) *Admin { return &Admin{db: db} }

// CreateModelRequest 是新建一条规范模型所需的字段。
//
// # 零值的含义
//
// 这里的零值都是**有意义的值**，不是「没填」：limits 为 0 表示未知（落 NULL）、
// vendor / family / canonical_model_id 为空串表示没有这一项、raw 与
// reasoning_options 为空表示空对象与空数组。所以本类型不用指针去区分
// 「没提到」与「填了零值」——那个区分在这里没有第二种状态可表达。
type CreateModelRequest struct {
	// CanonicalName 是选路与计费共用的键，必须非空、且不含空白字符。
	CanonicalName string

	// DisplayName 是给人看的名字，必须非空：表上那一列没有默认值，
	// 本包也不替调用方回落成规范名——回落会让「忘了填」看起来像填过了。
	DisplayName string

	Vendor string
	Family string

	// 三个 limit 为 0 表示未知，落库为 NULL。负数会被拒绝。
	ContextLimit int
	InputLimit   int
	OutputLimit  int

	// Capabilities 是三态能力矩阵（见 Capabilities）：nil 落库为 NULL =
	// 未知。零值就是「一项都没配」，所以新建时不给任何一个开关，
	// 库里就是六个 NULL——不是六个 false。
	//
	// Endpoints 那一项同一套口径：nil 落 NULL、空切片落 `{}`、非空落那几条。
	Capabilities Capabilities

	ModalitiesInput  []string
	ModalitiesOutput []string

	ReasoningOptions []any

	CanonicalModelID string

	KnowledgeCutoff *time.Time
	ReleaseDate     *time.Time

	Raw map[string]any
}

// Create 新建一条规范模型，返回它的 id。
func (a *Admin) Create(ctx context.Context, req CreateModelRequest) (int64, error) {
	name, err := normalizeCanonicalName(req.CanonicalName)
	if err != nil {
		return 0, err
	}
	display, err := normalizeDisplayName(req.DisplayName)
	if err != nil {
		return 0, err
	}
	contextLimit, err := normalizeLimit("context_limit", req.ContextLimit)
	if err != nil {
		return 0, err
	}
	inputLimit, err := normalizeLimit("input_limit", req.InputLimit)
	if err != nil {
		return 0, err
	}
	outputLimit, err := normalizeLimit("output_limit", req.OutputLimit)
	if err != nil {
		return 0, err
	}
	reasoning, err := encodeJSONArray("reasoning_options", req.ReasoningOptions)
	if err != nil {
		return 0, err
	}
	raw, err := encodeJSONObject("raw", req.Raw)
	if err != nil {
		return 0, err
	}
	endpoints, err := endpointValues("capabilities.endpoints", req.Capabilities.Endpoints)
	if err != nil {
		return 0, err
	}
	vendor, err := optionalText("--vendor（厂商）", req.Vendor)
	if err != nil {
		return 0, err
	}
	family, err := optionalText("--family（家族）", req.Family)
	if err != nil {
		return 0, err
	}
	if err = checkTexts("--input-modalities（输入模态）", req.ModalitiesInput); err != nil {
		return 0, err
	}
	if err = checkTexts("--output-modalities（输出模态）", req.ModalitiesOutput); err != nil {
		return 0, err
	}

	var id int64
	err = a.db.QueryRowContext(ctx, `
		INSERT INTO model_catalog
			(canonical_name, display_name, vendor, family,
			 context_limit, input_limit, output_limit,
			 supports_reasoning, supports_tools, supports_vision,
			 supports_audio, supports_cache, supports_batch,
			 modalities_input, modalities_output, reasoning_options,
			 canonical_model_id, knowledge_cutoff, release_date, raw,
			 supported_endpoints)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
		        $8, $9, $10, $11, $12, $13,
		        $14, $15, $16::jsonb,
		        $17, $18::date, $19::date, $20::jsonb,
		        $21)
		RETURNING id`,
		name, display, textOrNull(vendor), textOrNull(family),
		contextLimit, inputLimit, outputLimit,
		req.Capabilities.Reasoning, req.Capabilities.Tools, req.Capabilities.Vision,
		req.Capabilities.Audio, req.Capabilities.Cache, req.Capabilities.Batch,
		nonNilStrings(req.ModalitiesInput), nonNilStrings(req.ModalitiesOutput),
		string(reasoning), textOrNull(req.CanonicalModelID),
		dateOrNull(req.KnowledgeCutoff), dateOrNull(req.ReleaseDate),
		string(raw), endpoints).Scan(&id)
	if err != nil {
		return 0, createModelError(name, err)
	}
	return id, nil
}

// UpdateModelRequest 描述对一条规范模型的一次修改。
//
// 每个字段都是指针：nil 表示「不改这一项」，非 nil 表示「改成这个值」。
// 指针指向**零值**表示把这一项清空（落 NULL 或空数组）——表上这些列的 NULL
// 与零值本来就是同一个意思（见 Model 的注释），所以不需要第三种状态来表达
// 「清空」。
//
// Capabilities 这一项的形状与别的不同：它自己就是「给不给」的开关，而它
// 指向的那六项各自是三态（nil 落 NULL = 未知）。一次修改会**整体覆盖**那
// 六列（见 modelUpdateSets），所以调用方要「只改其中一项」时得先读出当前那
// 一行再合并——`larkaxctl model update` 就是这么做的（见 cmd/larkaxctl）。
type UpdateModelRequest struct {
	// CanonicalName 定位要改的那一行，**它本身不可改**，见 Admin 的注释。
	CanonicalName string

	DisplayName *string
	Vendor      *string
	Family      *string

	ContextLimit *int
	InputLimit   *int
	OutputLimit  *int

	Capabilities *Capabilities

	ModalitiesInput  *[]string
	ModalitiesOutput *[]string

	ReasoningOptions *[]any

	CanonicalModelID *string

	KnowledgeCutoff *time.Time
	ReleaseDate     *time.Time

	Raw *map[string]any
}

// Update 按 UpdateModelRequest 修改一条规范模型。
func (a *Admin) Update(ctx context.Context, req UpdateModelRequest) error {
	name, err := normalizeCanonicalName(req.CanonicalName)
	if err != nil {
		return err
	}

	sets, args, err := modelUpdateSets(req)
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return ErrNoChanges
	}
	// updated_at 每次都动，于是「改了几个字段」不影响这条语句的行为，
	// 下面也就能用影响行数判断这条模型在不在。
	sets = append(sets, "updated_at = now()")
	args = append(args, name)

	query := "UPDATE model_catalog SET " + strings.Join(sets, ", ") +
		" WHERE canonical_name = $" + strconv.Itoa(len(args))
	res, err := a.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("修改规范模型失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认修改结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：%q", ErrModelNotFound, name)
	}
	return nil
}

// modelUpdateSets 把一次修改编成 SET 片段与参数。
//
// # 为什么单独一个函数
//
// 它只做「校验 + 编码」，不碰数据库，于是可以单独读、单独改；更要紧的是
// 它**没有函数作用域上的 err**：每个分支各自 `x, err := ...`，作用域互不重叠，
// 也就不会遮蔽谁。全写在一个函数里时，最上面那次校验会声明一个函数级 err，
// 下面每个分支的 `:=` 都变成遮蔽它——govet 的 shadow 检查器会逐个报出来
// （.golangci.yml 里 govet 开了 enable-all）。
//
// 只把真正要改的列拼进 SET：没提到的列连现在是什么值都不必知道。
func modelUpdateSets(req UpdateModelRequest) ([]string, []any, error) {
	var sets []string
	var args []any
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}
	// addCast 与 add 相同，但显式写出目标类型。
	//
	// jsonb 那一列写不写 `::jsonb` 都能跑：服务器会拿目标列的类型去解这个
	// 文本参数。显式写出来是为了**不依赖那一步推断**——推断的结论随列类型与
	// 驱动而变，而它一旦不成立，存进去的就是一个 JSON 字符串（带引号的），
	// 读出来解不回对象，且只在某些列上出现。
	addCast := func(column, cast string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args))+"::"+cast)
	}

	if req.DisplayName != nil {
		display, err := normalizeDisplayName(*req.DisplayName)
		if err != nil {
			return nil, nil, err
		}
		add("display_name", display)
	}
	if req.Vendor != nil {
		vendor, err := optionalText("--vendor（厂商）", *req.Vendor)
		if err != nil {
			return nil, nil, err
		}
		add("vendor", textOrNull(vendor))
	}
	if req.Family != nil {
		family, err := optionalText("--family（家族）", *req.Family)
		if err != nil {
			return nil, nil, err
		}
		add("family", textOrNull(family))
	}
	if req.ContextLimit != nil {
		limit, err := normalizeLimit("context_limit", *req.ContextLimit)
		if err != nil {
			return nil, nil, err
		}
		add("context_limit", limit)
	}
	if req.InputLimit != nil {
		limit, err := normalizeLimit("input_limit", *req.InputLimit)
		if err != nil {
			return nil, nil, err
		}
		add("input_limit", limit)
	}
	if req.OutputLimit != nil {
		limit, err := normalizeLimit("output_limit", *req.OutputLimit)
		if err != nil {
			return nil, nil, err
		}
		add("output_limit", limit)
	}
	if req.Capabilities != nil {
		// 六项原样交出去：每项是 *bool，nil 由驱动落成 NULL（= 未知），
		// false/true 落成那一列的真值（见 Capabilities）。这里不替调用方
		// 把 nil 折成 false——那正是这条三态要区分的东西。
		add("supports_reasoning", req.Capabilities.Reasoning)
		add("supports_tools", req.Capabilities.Tools)
		add("supports_vision", req.Capabilities.Vision)
		add("supports_audio", req.Capabilities.Audio)
		add("supports_cache", req.Capabilities.Cache)
		add("supports_batch", req.Capabilities.Batch)
		// 端点清单跟着能力那一组一起写：它与那六列是同一行上的同一组声明
		// （见 Capabilities），分成两个开关的话「只改能力」会静默把端点清空
		// ——端点被清空等于放行，那是**放宽**，不会有人来报错。
		//
		// 因此调用方要「只改一项」时得先读出当前那一行再合并，与那六列同一条
		// 规矩（`larkaxctl model update` 就是这么做的）。
		endpoints, err := endpointValues("capabilities.endpoints", req.Capabilities.Endpoints)
		if err != nil {
			return nil, nil, err
		}
		add("supported_endpoints", endpoints)
	}
	if req.ModalitiesInput != nil {
		if err := checkTexts("--input-modalities（输入模态）", *req.ModalitiesInput); err != nil {
			return nil, nil, err
		}
		add("modalities_input", nonNilStrings(*req.ModalitiesInput))
	}
	if req.ModalitiesOutput != nil {
		if err := checkTexts("--output-modalities（输出模态）", *req.ModalitiesOutput); err != nil {
			return nil, nil, err
		}
		add("modalities_output", nonNilStrings(*req.ModalitiesOutput))
	}
	if req.ReasoningOptions != nil {
		reasoning, err := encodeJSONArray("reasoning_options", *req.ReasoningOptions)
		if err != nil {
			return nil, nil, err
		}
		addCast("reasoning_options", "jsonb", string(reasoning))
	}
	if req.CanonicalModelID != nil {
		add("canonical_model_id", textOrNull(*req.CanonicalModelID))
	}
	if req.KnowledgeCutoff != nil {
		addCast("knowledge_cutoff", "date", dateOrNull(req.KnowledgeCutoff))
	}
	if req.ReleaseDate != nil {
		addCast("release_date", "date", dateOrNull(req.ReleaseDate))
	}
	if req.Raw != nil {
		raw, err := encodeJSONObject("raw", *req.Raw)
		if err != nil {
			return nil, nil, err
		}
		addCast("raw", "jsonb", string(raw))
	}

	return sets, args, nil
}

// Delete 删掉一条规范模型，但**只在无人引用它时**允许。
//
// # 为什么是拒绝而不是级联
//
// 规范名是计价规则挂靠的键。级联删别名看着干净，实际是把「这个模型不再提供」
// 翻译成了「这些渠道的配置不见了」——渠道的 models 白名单里还留着这个名字，
// 于是它变成一条谁都看不懂的废配置。计价规则那一边更糟：model_patterns 是
// jsonb 数组，删掉一条规则不会报错，只会让下一次请求落到别的规则上。
//
// 所以这里把引用数列出来让人自己处置：要么先解除引用，要么确认了再删。
//
// # 这个检查是尽力而为的，真正的保证在外键上
//
// 出站别名那一边的保证来自 model_aliases 那条外键：检查通过之后、DELETE
// 之前有人插了一条别名，DELETE 会被外键拒掉（并发下会拿到 ErrModelInUse 而
// 不是「删成功」）。入站别名（inbound_model_aliases）与计价规则那一边**没有
// 任何外键**——前者的表按设计不挂外键（见 00060 迁移），后者是 jsonb 数组
// 建不了外键——所以那里的检查就是全部：刚查完、还没删的窗口里新配一条，
// 是拦不住的。说清这一点，好过假装挡住了。
//
// 入站别名这一路是本单补的：漏了它，先建入站别名再删规范模型就能删掉，
// 别名于是悬空——而它悬空之后每个用它的请求都会在选路处报「查无此模型」，
// 那条消息不会提到别名。补上之后 inbound_model_aliases 里不该再出现悬空行，
// 所以 model trace 的「断在规范模型」那一段是防御性诊断，不是常态。
func (a *Admin) Delete(ctx context.Context, canonicalName string) error {
	// 宽松规整，不判路径段：这里要认出的是库里已有的那一行，而不是判断
	// 这个名字现在还能不能写进去。理由见 normalizeExistingCanonicalName。
	name, err := normalizeExistingCanonicalName(canonicalName)
	if err != nil {
		return err
	}

	refs, err := a.countReferences(ctx, name)
	if err != nil {
		return err
	}
	if refs.any() {
		return fmt.Errorf("%w：%q 还被 %d 条出站别名、%d 条入站别名与 %d 条计价规则引用。"+
			"出站别名与入站别名都要先删（渠道的 models 白名单里还留着这个名字，删别名不等于删那条配置），"+
			"计价规则要先改掉 model_patterns——直接删规则不会报错，只会让请求落到别的规则上",
			ErrModelInUse, name, refs.outboundAliases, refs.inboundAliases, refs.rules)
	}

	res, err := a.db.ExecContext(ctx, `DELETE FROM model_catalog WHERE canonical_name = $1`, name)
	if err != nil {
		return deleteModelError(name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认删除结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：%q", ErrModelNotFound, name)
	}
	return nil
}

// modelReferences 是一个规范名被引用的三处计数。
type modelReferences struct {
	// outboundAliases 是 model_aliases 的行数：这些行有外键，删模型时会被
	// 数据库再挡一次。
	outboundAliases int
	// inboundAliases 是 inbound_model_aliases 的行数：这张表**没有**外键
	// （理由见 00060 迁移），所以这里的检查就是全部。
	inboundAliases int
	// rules 是 model_patterns 里写着这个名字的计价规则数。jsonb 数组建不了
	// 外键，同样只有这一道检查。
	rules int
}

func (r modelReferences) any() bool {
	return r.outboundAliases > 0 || r.inboundAliases > 0 || r.rules > 0
}

// countReferences 数出这个规范名被哪几处引用。
//
// # 三处都要数
//
// 少了任何一处，那条路径上的引用就会在删模型时消失：先建一条入站别名、
// 再删掉规范模型，别名就悬空指向一个不存在的名字——而它悬空之后每个用它的
// 请求都会在选路处报「查无此模型」，那条消息不会提到别名。
//
// # 计价规则那一侧是精确匹配
//
// pricing 的查询是 `model_patterns ? $2`，也就是「数组里有这个元素」。所以
// 这里用同一个判据，而不是去猜通配符——匹配口径要是两处不一致，删掉的就会是
// 一条其实还在被用的名字。
func (a *Admin) countReferences(ctx context.Context, canonicalName string) (modelReferences, error) {
	var refs modelReferences

	err := a.db.QueryRowContext(ctx,
		`SELECT count(*) FROM model_aliases WHERE canonical_name = $1`, canonicalName).Scan(&refs.outboundAliases)
	if err != nil {
		return modelReferences{}, fmt.Errorf("统计出站别名的引用失败: %w", err)
	}

	err = a.db.QueryRowContext(ctx,
		`SELECT count(*) FROM inbound_model_aliases WHERE canonical_name = $1`, canonicalName).Scan(&refs.inboundAliases)
	if err != nil {
		return modelReferences{}, fmt.Errorf("统计入站别名的引用失败: %w", err)
	}

	err = a.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pricing_rules WHERE model_patterns ? $1`, canonicalName).Scan(&refs.rules)
	if err != nil {
		return modelReferences{}, fmt.Errorf("统计计价规则的引用失败: %w", err)
	}
	return refs, nil
}

// CreateAliasRequest 是新建一条别名所需的字段。
type CreateAliasRequest struct {
	// ChannelID 为 0 表示平台级别名（库里存 NULL），对全部渠道生效。
	// 负数无效——id 从 1 开始，负数不可能等于任何真实的渠道。
	ChannelID int64

	// UpstreamModel 是这家上游对同一个模型的叫法。
	UpstreamModel string

	// CanonicalName 必须已经在 model_catalog 里，否则外键会拒。
	CanonicalName string

	// Overrides 是这条别名上配的一组**上游请求参数**覆盖。空/nil 表示没配。
	//
	// 允许的键与冲突时谁赢，见 internal/overrides——写入校验、热路径应用与
	// 预留上界三处共用那一份判据。非法键在这里就被拒绝（见 checkOverrides）。
	Overrides map[string]any
}

// CreateAlias 新建一条别名，返回它的 id。
func (a *Admin) CreateAlias(ctx context.Context, req CreateAliasRequest) (int64, error) {
	if req.ChannelID < 0 {
		return 0, fmt.Errorf("渠道 id %d 无效（0 表示平台级别名）", req.ChannelID)
	}
	canonical, err := normalizeCanonicalName(req.CanonicalName)
	if err != nil {
		return 0, err
	}
	upstream, err := normalizeUpstreamModel(req.UpstreamModel)
	if err != nil {
		return 0, err
	}
	// 覆盖的键在**写入时**校验，不拖到热路径（见 checkOverrides）。
	//
	// 变量名不复用外层那个 err：同一个块里再 `err :=` 是遮蔽，govet 的 shadow
	// 检查器会报（同 Model 里那一处）。
	if overrideErr := checkOverrides(req.Overrides); overrideErr != nil {
		return 0, overrideErr
	}
	overridesJSON, err := encodeJSONObject("overrides", req.Overrides)
	if err != nil {
		return 0, err
	}

	// 平台级要传 NULL，不能传 0：0 会去匹配 id 为 0 的渠道，而那个渠道不存在
	// ——外键会直接拒绝，于是「平台级别名」这条路径根本走不通。
	var channel any
	if req.ChannelID != 0 {
		channel = req.ChannelID
	}

	var id int64
	err = a.db.QueryRowContext(ctx, `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name, overrides)
		VALUES ($1, $2, $3, $4::jsonb)
		RETURNING id`,
		channel, upstream, canonical, string(overridesJSON)).Scan(&id)
	if err != nil {
		return 0, aliasWriteError(req.ChannelID, canonical, upstream, err)
	}
	return id, nil
}

// UpdateAliasRequest 描述对一条别名的一次修改。
//
// **渠道范围与规范名都不可改**：改渠道范围是换了一条别名（同一对名字在
// 另一个范围上是另一条配置），改规范名则等于把这条别名改挂到另一个计费键上
// ——那会让同一次请求的选路与计费指向两个不同的模型。要改就删掉重建，
// 两次改动各自看得见。
type UpdateAliasRequest struct {
	// ID 是 model_aliases 的主键。它而不是「渠道 + 上游名」来定位，是因为
	// 同一对名字在两个范围上可以各有一条，用名字定位会撞上两条。
	ID int64

	UpstreamModel *string
	// Overrides 为 nil 表示不动这一栏；指针指向空对象表示清空。
	//
	// 取值同样在写入时校验（见 checkOverrides）。
	Overrides *map[string]any
}

// UpdateAlias 按 UpdateAliasRequest 修改一条别名。
func (a *Admin) UpdateAlias(ctx context.Context, req UpdateAliasRequest) error {
	if req.ID <= 0 {
		return fmt.Errorf("别名 id %d 无效", req.ID)
	}

	var sets []string
	var args []any
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}
	// overrides 是 jsonb：参数是文本，不显式转的话写进去的会是一个 JSON
	// 字符串（带引号的），读出来解不回对象。
	addCast := func(column, cast string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args))+"::"+cast)
	}

	if req.UpstreamModel != nil {
		upstream, err := normalizeUpstreamModel(*req.UpstreamModel)
		if err != nil {
			return err
		}
		add("upstream_model", upstream)
	}
	if req.Overrides != nil {
		// 与新建那条路同一个位置、同一条判据：改这一栏也要在写入时拦住
		// （见 checkOverrides）。
		if err := checkOverrides(*req.Overrides); err != nil {
			return err
		}
		overridesJSON, err := encodeJSONObject("overrides", *req.Overrides)
		if err != nil {
			return err
		}
		addCast("overrides", "jsonb", string(overridesJSON))
	}

	if len(sets) == 0 {
		return ErrNoChanges
	}
	args = append(args, req.ID)

	query := "UPDATE model_aliases SET " + strings.Join(sets, ", ") +
		" WHERE id = $" + strconv.Itoa(len(args))
	res, err := a.db.ExecContext(ctx, query, args...)
	if err != nil {
		return aliasUpdateError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认修改结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：别名 id %d", ErrAliasIDNotFound, req.ID)
	}
	return nil
}

// DeleteAlias 删掉一条别名。
func (a *Admin) DeleteAlias(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("别名 id %d 无效", id)
	}

	res, err := a.db.ExecContext(ctx, `DELETE FROM model_aliases WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除别名失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认删除结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：别名 id %d", ErrAliasIDNotFound, id)
	}
	return nil
}

// ── 约束冲突的翻译 ──────────────────────────────────────────────────────
//
// 这一批函数把**数据库的拒绝**翻译成调用方认得出的错误。认不出来的原样包一层
// 返回，绝不因为读不懂而当成成功。

// rejectedError 是一次被数据库约束拒掉的写入。
//
// 它的错误链一次满足三件事，与 ledger/store 的 frozenUnderflowError 同一个形状：
//
//   - errors.Is(err, <sentinel>)：调用方能按「该改什么」分类
//   - errors.As(err, &pgErr)：驱动给的原始错误仍在链里，SQLSTATE 与约束名
//     都还拿得到——**测试正是靠它断言撞的是哪一条索引**，而不是「报了错」。
//     少了这一条，只有那句人话能证明撞的是什么，而人话是可以写错的。
//   - Error() 只说人能看懂的那句：把驱动原文拼在后面会让日志里多一段
//     `ERROR: duplicate key value violates unique constraint ...`，
//     而那段的信息这句话已经说全了。
//
// 不用 errors.Join：它的错误文本是换行拼起来的，而这条要进日志，单行更好读。
type rejectedError struct {
	sentinel error
	detail   string
	cause    error
}

func (e *rejectedError) Error() string { return e.sentinel.Error() + "：" + e.detail }

// Unwrap 返回整条错误链。两个元素缺一不可，理由见类型注释。
func (e *rejectedError) Unwrap() []error { return []error{e.sentinel, e.cause} }

// createModelError 把新建规范模型的错误翻译成分类错误。
func createModelError(canonicalName string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == modelCanonicalUnique {
		return &rejectedError{
			sentinel: ErrModelExists,
			detail:   fmt.Sprintf("%q 已经在目录里了", canonicalName),
			cause:    err,
		}
	}
	return fmt.Errorf("创建规范模型失败: %w", err)
}

// deleteModelError 把删除规范模型的错误翻译成分类错误。
//
// 只有一种要翻译的：并发下刚查完引用、还没删的时候有人配了别名，外键会拒。
// 那时报出来的应当是 ErrModelInUse（「还有人引用」），而不是一句驱动原文。
func deleteModelError(canonicalName string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == aliasCanonicalFK {
		return &rejectedError{
			sentinel: ErrModelInUse,
			detail:   fmt.Sprintf("%q 刚有别名挂上来", canonicalName),
			cause:    err,
		}
	}
	return fmt.Errorf("删除规范模型失败: %w", err)
}

// aliasWriteError 把别名的写入错误翻译成人能看懂的话。
//
// 每条约束对应一句「下一步该改什么」，而不是把 SQLSTATE 抛出去——23505 本身
// 不说明撞的是规范名还是上游名，也不说明是渠道级还是平台级，而这几件事的
// 处置完全不同。认不出来的错误原样上抛。
func aliasWriteError(channelID int64, canonicalName, upstreamModel string, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("写入模型别名失败: %w", err)
	}

	switch {
	case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == aliasChannelCanonicalUnique:
		return &rejectedError{
			sentinel: ErrCanonicalTaken,
			detail: fmt.Sprintf("渠道 %d 下已经有另一个上游名映射到 %q 了"+
				"（同一个渠道里一个规范名只能有一个上游名，否则选路无从取舍）",
				channelID, canonicalName),
			cause: err,
		}

	case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == aliasPlatformCanonicalUnique:
		return &rejectedError{
			sentinel: ErrCanonicalTaken,
			detail: fmt.Sprintf("平台级已经有另一个上游名映射到 %q 了"+
				"（要在这个渠道上用另一个名字，就配一条渠道级别名覆盖它）",
				canonicalName),
			cause: err,
		}

	case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == aliasChannelUpstreamUnique:
		return &rejectedError{
			sentinel: ErrUpstreamTaken,
			detail: fmt.Sprintf("渠道 %d 里 %q 已经映射到另一个规范名了"+
				"（一个上游名只能对应一个规范名，否则计费按哪个名字算就没有依据）",
				channelID, upstreamModel),
			cause: err,
		}

	case pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == aliasCanonicalFK:
		return &rejectedError{
			sentinel: ErrModelNotFound,
			detail:   fmt.Sprintf("%q 不在目录里，先把模型建进 model_catalog 再配别名", canonicalName),
			cause:    err,
		}

	case pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == aliasChannelFK:
		return &rejectedError{
			sentinel: ErrChannelNotFound,
			detail:   fmt.Sprintf("id %d", channelID),
			cause:    err,
		}
	}

	return fmt.Errorf("写入模型别名失败: %w", err)
}

// aliasUpdateError 把改别名的错误翻译成人能看懂的话。
//
// 与新建不同的只有一处：渠道范围与规范名都不参与 SET（见 UpdateAliasRequest），
// 所以能撞上的只剩「同一个渠道里一个上游名只对应一个规范名」那条唯一键——
// 两条偏索引管的是 (channel_id, canonical_name)，那两个字段在这里改不了。
// 撞上时说不清是哪个渠道（渠道 id 不在参数里），所以那句话里不带渠道。
func aliasUpdateError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == aliasChannelUpstreamUnique {
		return &rejectedError{
			sentinel: ErrUpstreamTaken,
			detail: "这个上游名在这个渠道里已经映射到另一个规范名了" +
				"（一个上游名只能对应一个规范名，否则计费按哪个名字算就没有依据）",
			cause: err,
		}
	}
	return fmt.Errorf("修改别名失败: %w", err)
}

// ── 校验与编码 ──────────────────────────────────────────────────────────
//
// 这一批函数是写入路径与读取路径「同一套列」的兑现方式：值的形态在写入侧
// 定下来，读取侧按同一个假设解。

// normalizeCanonicalName 校验**写入侧**的规范名：Create、Update 与
// CreateAlias 都走这一条。
//
// 它是选路与计费**共用**的键：请求里的模型名要按它匹配，计价规则的
// model_patterns 也按它匹配。所以这里比别的字段严一格：
//
//   - 空白字符一律拒绝：带空白的名字不会等于任何请求里的模型名，写进去
//     就是一条谁都命中不了的配置，而它的表现是「这个模型配了却永远选不中」。
//   - 空路径段与 `.` / `..` 段一律拒绝：规范名会作为路径出现在
//     `GET /v1/models/{name}` 里，而 `net/http` 的 ServeMux 在匹配之前会
//     先做 URL 规范化（折叠空段、解析 `.` 与 `..`）。这样的名字同样写进去
//     不报错——读的时候要么 301、要么落到另一个模型上。
func normalizeCanonicalName(raw string) (string, error) {
	name, err := normalizeExistingCanonicalName(raw)
	if err != nil {
		return "", err
	}
	for _, segment := range strings.Split(name, "/") {
		switch segment {
		case "":
			return "", fmt.Errorf("规范模型名 %q 的路径段为空：名字按 / 分段，空段会被 URL 规范化折叠掉，这个名字读不回来", name)
		case ".", "..":
			return "", fmt.Errorf("规范模型名 %q 含路径段 %q：`.` 与 `..` 会被 URL 解析掉，这个名字读不回来", name, segment)
		}
	}
	return name, nil
}

// normalizeExistingCanonicalName 校验一个**按名字找已有行**的输入：Delete
// 直接用它，严格版也建在它之上。
//
// 它只做宽松的规整：去首尾空白、拒空、拒控制字符，不判路径段。
//
// # 为什么与写入侧不是同一条规则
//
// Delete 要认出的是库里**已经存在**的那一行，而不是判断这个名字现在还能
// 不能写进去。严格规则是给新写入定的；拿它来校验一个存量标识符，库里一旦
// 出现 `a//b` 这样的名字，删除就会先被名字本身拒绝——这条历史遗留从此没有
// 去处。宽松版只放宽路径段这一层，空白与控制字符照拒：后两者不可能属于
// 一条真实存在的名字。
//
// 放宽到这一层也就到此为止：Update 仍按严格规则校验，存量名字能走的只有
// 删除（删掉再按合法名字重建），改名不在其中。
//
// 写入侧在它之上再加一层（见 normalizeCanonicalName）。两者共用这一份
// 基础规整，返回的值来自同一处，不会各自 trim 出一份略有差别的名字。
func normalizeExistingCanonicalName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("规范模型名不能为空")
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return "", fmt.Errorf("规范模型名 %q 里有空白字符：它是选路与计费共用的键，带空白的名字匹配不到任何请求", name)
	}
	// 空白之外还有一类控制字符会打坏终端：ESC 与其余 C0。规范名进
	// `model list` 与 `model get`，两处都按一栏一行排版。
	if err := textshape.Validate("--model（规范模型名）", name); err != nil {
		return "", err
	}
	return name, nil
}

// normalizeUpstreamModel 校验上游模型名。
//
// 只去首尾空白、要求非空：这个名字**原样发给上游**，上游认什么名字由上游决定，
// 本包没有依据替它定更严的规则。首尾空白则一定是笔误——它同样匹配不上任何
// 上游的模型名，而报错会来自上游，离「配置多打了一个空格」已经很远。
//
// # 与 schema 上那条 CHECK 的分工
//
// model_aliases.upstream_model 上还有一条 CHECK（见迁移 00022），判据是
// btrim(upstream_model) 不为空。两者不是同一道门的两次实现，去掉哪一道都会
// 少一样保证：
//
//   - 这道函数多做的、CHECK 不做的是**规整**：它 trim 之后才判，于是
//     '  x  ' 在这里变成 'x' 再写进去；直接走 SQL 写 '  x  ' 能通过 CHECK
//     （它不是空白）。
//   - CHECK 多做的、这道函数做不到的是覆盖**不走本包的写入方**：管理面之外的
//     SQL、以及将来新增的写入路径，这道函数一道都照不到。
//
// 所以别因为「反正库里有 CHECK」就把这里放宽。
func normalizeUpstreamModel(raw string) (string, error) {
	upstream := strings.TrimSpace(raw)
	if upstream == "" {
		return "", errors.New("上游模型名不能为空")
	}
	// 「原样发给上游」不等于「什么都能收」：这个名字同时进 `alias list` 的
	// 「上游模型名」那一栏，换行会把那一栏折出续行，ESC 会原样进终端。
	if err := textshape.Validate("--upstream（上游模型名）", upstream); err != nil {
		return "", err
	}
	return upstream, nil
}

// checkOverrides 校验一条别名的参数覆盖。
//
// # 拦在写入时
//
// 覆盖的键与取值在这里判，热路径只是把判过的值用起来。理由是**谁配错了要能
// 立刻看见**：一旦非法值进了库，它表现为这个别名上的每一次请求都失败，而现场
// 能拿到的是一个笼统的失败——是哪一行、哪个键配错了，得回库里翻那一行才知道。
// 写入时报错则直接指着那个键说。
//
// 白名单与取值判据在 internal/overrides，与热路径（internal/route 的应用、
// internal/gateway 取预留上界）用的是**同一份**：这里多一个键而热路径不认它，
// 就会变回这一列此前那种「配了跟没配一样」。
func checkOverrides(v map[string]any) error {
	return overrides.Validate(v)
}

// normalizeDisplayName 校验展示名。
//
// 表上那一列 NOT NULL 且没有默认值。这里不回落成规范名：回落会让「忘了填」
// 与「就是这么填的」在库里长得一模一样，而展示名正是给人看的那一栏。
func normalizeDisplayName(raw string) (string, error) {
	display := strings.TrimSpace(raw)
	if display == "" {
		return "", errors.New("display_name 不能为空：它是给人看的名字，不会回落到规范名")
	}
	// 展示名进 `model list` 的「展示名」那一栏与 `model get` 的同名行。
	// 判在 trim 之后的值上，与 textOrNull 落库的那个串一致。
	if err := textshape.Validate("--display（展示名）", display); err != nil {
		return "", err
	}
	return display, nil
}

// optionalText 校验并归一一个可空的展示文本列（vendor / family）。
//
// 返回 trim 之后的值；空串表示「没填」，由调用方经 textOrNull 落 NULL——
// 这里不直接返回 nil：那会让「取值」与「空」共用一个 nil 接口值，而 nilnil
// 检查咬住的正是这个形状（`(nil, nil)` 读起来像「成功但没值」）。
// 判在 trim 之后的值上，与落库的那个串一致。
func optionalText(field, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if err := textshape.Validate(field, value); err != nil {
		return "", err
	}
	return value, nil
}

// checkTexts 逐个校验一组展示文本（模态清单）。
//
// 模态不清洗也不 trim：`--input-modalities 'a, b'` 里的空格会原样落库
// （见 nonNilStrings），所以这里也判原值。它在 `model list` 的「模态」那一栏
// 与 `model get` 里按逗号连起来打。
func checkTexts(field string, values []string) error {
	for i, value := range values {
		if err := textshape.Validate(fmt.Sprintf("%s 里第 %d 个", field, i+1), value); err != nil {
			return err
		}
	}
	return nil
}

// normalizeLimit 校验 token 上限。
//
// 0 落成 NULL：表上那三列可空，「未知」与「0」在读取侧是同一个状态
// （见 Model 的注释），所以库里存 0 只会让「没填」看起来像一个测出来的值。
// 负数一定是算错了——它在上限比较里会让任何请求都不合格。
//
// 返回的 nil 是**这一列的合法取值**（SQL NULL），不是「出错了但没报」——
// 所以这里显式 nolint 而不是造一个哨兵错误。
func normalizeLimit(field string, limit int) (any, error) {
	if limit < 0 {
		return nil, fmt.Errorf("%s 不能为负数：%d", field, limit)
	}
	if limit == 0 {
		return nil, nil //nolint:nilnil // 见上
	}
	return limit, nil
}

// textOrNull 把空串落成 NULL。
//
// 表上这几列可空，而读取侧把它们折成空串（见 Model 的注释）——所以「空串」
// 与「NULL」在库里不该是两种状态：存哪一种，读出来都一样，而看库的人会以为
// 它们有区别。
func textOrNull(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

// dateOrNull 把日期落成 `YYYY-MM-DD` 文本，配合 SQL 里的 ::date。
//
// 不直接把 time.Time 交出去：驱动会把它当 timestamptz 发，而
// timestamptz → date 的转换按**会话时区**算——同一个瞬间在 UTC+8 与 UTC-5 下
// 可能落在不同的日期上。日期本来就是无时区的，所以这里给日期文本，
// 让转换只发生一次，且不依赖连接是怎么配的。
//
// 指针为 nil 或指向零值都落 NULL：表上这两列可空，读取侧用 nil 表示没填
// （见 Model 的注释），所以库里不该出现一个「零日期」。
func dateOrNull(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.Format("2006-01-02")
}

// nonNilStrings 把 nil 切片换成空切片。
//
// modalities 那两列是 NOT NULL DEFAULT '{}'：驱动把 nil 切片发成 NULL，
// 而 NULL 会被列约束拒掉——报错说的是「不能为空」，而调用方只是没填。
// 空切片才是「这个模型没有这类模态」的正确表达。
func nonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}
	return value
}

// endpointValues 把一组端点编成可以交给驱动的值。
//
// # 三态一个都不能少
//
//	nil   落 NULL = 未知（这一项不参与选路判据）
//	{}    落 `{}` = 明确一条都不支持
//	非空  落那几条
//
// 所以 nil 不折成空切片——supported_endpoints 与 modalities 那两列不同，
// 它可空，而 NULL 与 `{}` 在选路里的结论正好相反（见 Capabilities.Endpoints）。
//
// # 为什么每一项都要过一遍解析
//
// 入参是 []channel.Endpoint，而它只是 string 的别名：调用方可以 `Endpoint(x)`
// 塞进任意串。校验放在这里、与读取侧的 decodeEndpoints 共用一套判据——两侧
// 各判各的时，症状是「写进去的值读不出来」，而那要等到有人真的读那一行才发现。
//
// 返回的 nil 是**这一列的合法取值**（SQL NULL），不是「出错了但没报」——
// 所以这里显式 nolint，与 normalizeLimit 同一条理由。
func endpointValues(field string, values []channel.Endpoint) (any, error) {
	if values == nil {
		return nil, nil //nolint:nilnil // 见上
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		endpoint, err := channel.ParseEndpoint(string(value))
		if err != nil {
			return nil, fmt.Errorf("%s 里有一项不是合法的端点：%w", field, err)
		}
		out = append(out, string(endpoint))
	}
	return out, nil
}

// encodeJSONObject 把 map 编成 jsonb 的文本形式。
func encodeJSONObject(field string, value map[string]any) ([]byte, error) {
	if len(value) == 0 {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s 不是可序列化的 JSON 对象: %w", field, err)
	}
	return raw, nil
}

// encodeJSONArray 把切片编成 jsonb 的文本形式。
func encodeJSONArray(field string, value []any) ([]byte, error) {
	if len(value) == 0 {
		return []byte("[]"), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s 不是可序列化的 JSON 数组: %w", field, err)
	}
	return raw, nil
}
