// Package catalog 承载模型目录的两张表：model_catalog（规范模型名）与
// model_aliases（上游模型名 ↔ 规范模型名）。
//
// # 为什么这两张表需要一个主人
//
// 规范名是**计费规则挂靠的键**：pricing_rules.model_patterns 按它匹配
// （见 internal/pricing），选路又按它换上游名。写入侧没有一个明确的归属时，
// 「改一个规范名」会同时改变计费与选路，而两次改动之间的窗口里，同一次请求
// 可以用一个名字计价、用另一个名字调用上游——账单看上去完全正常。
//
// 所以本包是这两张表**唯一**的读写入口：
//
//   - 读（Catalog）：选路要的「规范名 → 某个渠道上的上游名」，以及管理面的列举
//   - 写（Admin）：创建、修改、删除规范模型；增删改别名
//
// # 那条读取路径原先在 internal/channels
//
// 「按规范名找上游名」原先定义在 internal/channels 的 Source 上，现在搬到这里。
// 理由是**归属**：它查的是 model_aliases，而写入路径也落在这张表上。
// 两边分处两个包时，「渠道级优先于平台级」这条判据、以及两条偏唯一索引
// 保证的「每个范围最多命中一条」，就成了两个包各自维护的假设。
// 那份实现只有这一处——两份判据会漂移，而漂移的表现是「同一次请求，
// 选路与计费用了两个上游名」，账单看上去完全正常。
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrAliasNotFound 表示没有从这个渠道到这个规范模型名的映射。
//
// 它与「查询失败」分开：没有别名是**配置缺失**（这个渠道还没配这个模型，
// 或者只配了平台级的默认名而这里查的是渠道级），而查询失败是数据库的问题。
// 两者的处置完全不同，所以不能都退化成一个笼统的错误。
var ErrAliasNotFound = errors.New("没有对应的上游模型名")

// ErrModelNotFound 表示目录里没有这个规范模型。
//
// 与 ErrAliasNotFound 分开是因为两者指向不同的下一步：前者要人先建规范模型，
// 后者要人给这个渠道配别名。
var ErrModelNotFound = errors.New("规范模型不存在")

// Capabilities 是 model_catalog 上的能力矩阵。
//
// 六个布尔各对应一列，一律 NOT NULL DEFAULT false，所以这里没有「未知」这个
// 状态：没写进目录就等于不支持。这与 context_limit 那种「0 表示未知」的
// 数值列不同，见 Model 的注释。
type Capabilities struct {
	Reasoning bool
	Tools     bool
	Vision    bool
	Audio     bool
	Cache     bool
	Batch     bool
}

// Model 是 model_catalog 里的一条规范模型。
//
// # 可空列在这里怎么表达
//
// vendor / family / canonical_model_id 没填时是 NULL，读出来是空串；
// 三个 limit 没填时也是 NULL，读出来是 0。**「未知」与「0」在库里是同一个
// 状态**，所以这里不造一个额外的区分——真正需要区分的是「0 个 token」，
// 而那样的模型不存在。
//
// 两个日期是可空的，用指针：nil 表示目录里没填，而不是某个「零日期」。
type Model struct {
	ID            int64
	CanonicalName string
	DisplayName   string

	Vendor string
	Family string

	ContextLimit int
	InputLimit   int
	OutputLimit  int

	Capabilities Capabilities

	ModalitiesInput  []string
	ModalitiesOutput []string

	// ReasoningOptions 是 models.dev 的 reasoning_options 原样落库，
	// 形如 [{"type":"toggle"}]，见 00001_init.sql 里那一列的注释。
	// 本包不解释它的内容，只保证原样存取。
	ReasoningOptions []any

	CanonicalModelID string

	KnowledgeCutoff *time.Time
	ReleaseDate     *time.Time

	// Raw 是采集来的原始元信息，不参与任何判断。
	Raw map[string]any

	UpdatedAt time.Time
}

// Alias 是 model_aliases 里的一条：某个范围内，一个上游模型名对应一个规范名。
type Alias struct {
	ID int64

	// ChannelID 为 0 表示平台级别名（库里 channel_id 为 NULL），对全部渠道生效。
	// 有值时它是渠道级覆盖。0 不会等于任何真实的 channel_id（id 从 1 开始）。
	ChannelID int64

	UpstreamModel string
	CanonicalName string

	// Overrides 是商家给这条别名配的一组上游参数覆盖，空对象表示没配。
	//
	// 能覆盖哪些键、与调用方冲突时谁赢，判据在 internal/overrides ——
	// 写入校验与热路径用的是同一份。
	Overrides map[string]any

	CreatedAt time.Time
}

// Catalog 是模型目录的读取面。
//
// # 为什么读与写是两个类型
//
// 与 channels.Source / channels.Admin 同一套分工：Alias 在请求热路径上
// （每个请求选路都要调），而写入只在运维动作里发生。分成两个类型之后，
// 热路径拿到的那个对象**没有**写入能力——这是编译期的事实，不是一句约定。
type Catalog struct {
	db *sql.DB
}

// New 包装数据库句柄。
func New(db *sql.DB) *Catalog { return &Catalog{db: db} }

// Alias 解析这个渠道上这个规范名对应的上游名，连同这条别名上配的参数覆盖。
//
// # 为什么查渠道级优先于平台级
//
// model_aliases 里 channel_id 为 NULL 的行是平台级别名，对全部渠道生效；
// 有 channel_id 的是渠道级覆盖。**渠道级优先**——商家配一条渠道级别的
// 别名，意思就是「这家上游的叫法与别家不同」，平台级那条只是兜底。
//
// channelID 传 0 表示「不针对具体渠道」，此时只会命中平台级的行
// （id 从 1 开始，0 不会等于任何真实的 channel_id）。
//
// # 每个范围最多命中一条，所以这里不需要兜底的取舍规则
//
// 两条偏唯一索引（见 00007 迁移）把「两个上游名映射到同一个规范名」挡在了
// 写入侧：渠道级 (channel_id, canonical_name) 唯一，平台级 canonical_name
// 唯一。于是这个查询最多命中两条——一条渠道级、一条平台级——按渠道级优先取
// 一条即可，不需要再拿别的列给「同一范围下的多条候选」排个序。
//
// 约束建立之前，这里按 upstream_model 排序取第一条来给多条候选定一个确定的
// 顺序。那只保证了**确定**，没保证**正确**：上游改名或换版本时，两个名字在
// 上游侧的计价可能不同，选错就是差价，而账单看上去完全正常。约束到位后那种
// 数据进不来了，所以这里只按「渠道级优先于平台级」排序——它表达的是配置的
// 优先级，不是在一堆等价候选里挑一个。
//
// 写入被拦下这件事由 integration_test.go 里的用例钉住（断言拿到的是具体的
// 唯一约束冲突），删掉索引会立刻让那几条用例失败。
//
// # 一条别名都没配过的规范名按同名处理
//
// 判据按**规范名**分，不按「这一次查不查得到」分：
//
//   - 这个规范名在**任何范围**下都没有别名 → 上游就叫这个名字，原样返回，
//     覆盖为空。大多数部署一条别名都没配（规范名与上游名逐字相同），而
//     「查不到就报错」会把它们每一次请求都打死——那是把一个能用的部署变成
//     完全不能用。别名表表达的是「哪些模型的上游叫法与规范名不同」，
//     一条都没有的模型不在这个集合里。
//   - 有别名，但没有一条覆盖这个渠道 → ErrAliasNotFound。
//
// 后一种正是**配了一半**：别名是逐渠道配的，某个渠道漏了的时候静默回落成
// 规范名，会让它表现为「上游说这个模型不存在」，排查方向指向对方。前一种
// 不是配置缺失——它连一条要翻的映射都没有。
//
// 两种情形各有用例钉住（见 integration_test.go）：只判「这次查不到」会让
// 前者与后者同形，而它们在「一条都没配」与「配了一半」两个部署下的正确
// 处置正好相反。
//
// # 覆盖跟着命中的那一条走
//
// overrides 是**行**上的一个字段，所以它与上游名来自同一条别名：命中了渠道级
// 那条就用渠道级那条的覆盖，不会回平台级那条上再凑一次。让两者各取一处的话，
// 「名字按这家上游的叫法发出去、参数却按平台级那条配」会成为一种配得出来、
// 但解释不通的状态。
//
// 它的语义（能覆盖哪些键、与调用方冲突时谁赢）不在这里，在 internal/overrides
// ——那份判据的另外两个用户（写入校验、预留上界）也用同一份。
func (c *Catalog) Alias(ctx context.Context, channelID int64, canonicalModel string) (string, map[string]any, error) {
	if canonicalModel == "" {
		return "", nil, errors.New("规范模型名为空")
	}

	rows, err := c.aliasRows(ctx, canonicalModel)
	if err != nil {
		return "", nil, err
	}

	// 渠道级优先于平台级：与旧查询的 `ORDER BY (channel_id IS NULL)` 是同一条
	// 判据（Postgres 里 FALSE 排在 TRUE 前面）。
	if picked, matched := pickAlias(rows, channelID); matched {
		return picked.UpstreamModel, picked.Overrides, nil
	}
	if len(rows) == 0 {
		// 这个规范名一条别名都没配过：上游就叫这个名字。
		return canonicalModel, nil, nil
	}
	return "", nil, fmt.Errorf("%w：渠道 %d 的 %q", ErrAliasNotFound, channelID, canonicalModel)
}

// aliasRows 取回这个规范名在**所有范围**下的别名行。
//
// 一次取全部、在 Go 里挑，而不是「先查命中的那一行，没命中再查一次『这个规范名
// 配过没有』」：那样在**最常见的部署**（一条别名都没配）上要两次往返，而第二次
// 只在没命中时才发生——最贵的顺序恰好就是最常见的顺序。
//
// 行数是个位数：按 canonical_name 有索引，且 `(channel_id, canonical_name)`
// 与 `(canonical_name)` 各有一条唯一约束。整行取回来比多一次往返便宜。
//
// 用 aliasQuery 整行取（而不是只取用得上的几列）：列清单与 scanAlias 的假设
// 只该有一份，而多带几列的代价是几十个字节。
func (c *Catalog) aliasRows(ctx context.Context, canonicalModel string) ([]Alias, error) {
	rows, err := c.db.QueryContext(ctx, aliasQuery+` WHERE a.canonical_name = $1`, canonicalModel)
	if err != nil {
		return nil, fmt.Errorf("查询模型别名失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Alias{}
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历模型别名失败: %w", err)
	}
	return out, nil
}

// pickAlias 在给出的别名行里按「渠道级优先于平台级」挑一条。
//
// matched 为 false 表示这些行里没有一条覆盖这个渠道——调用方要能把它与
// 「一行都没有」分开，两者的处置相反（见 Alias）。
func pickAlias(rows []Alias, channelID int64) (Alias, bool) {
	var platform Alias
	havePlatform := false
	for _, alias := range rows {
		switch {
		case alias.ChannelID != 0 && alias.ChannelID == channelID:
			return alias, true
		case alias.ChannelID == 0:
			platform, havePlatform = alias, true
		}
	}
	if havePlatform {
		return platform, true
	}
	return Alias{}, false
}

// OverridesFor 返回这个规范名在给定渠道上各自生效的参数覆盖。
//
// # 它是给预留用的
//
// 预留发生在选路**之前**，那时只有一个候选集，而覆盖是逐渠道配的：这次请求
// 到底会按哪个上限发出去，要等选路才知道。预留的处置是按**候选集**取上界
// （与规则那一侧同一个口径，见 pipeline.Reserve），所以这里要的是候选集里
// 每一个渠道各自生效的覆盖。
//
// 逐渠道各查一次会变成 N 次往返，所以它一次取回这个规范名的全部别名行再在
// Go 里挑——与 Alias 走**同一条查询、同一条优先级**（pickAlias）。
//
// # 返回的 map 只含真有覆盖的渠道
//
// 没配别名、别名没覆盖这个渠道、以及覆盖是空对象，三者对调用方是同一件事：
// 按原值走。不进 map 就表达了这个。渠道 id 传 0（平台级）的项不会出现——
// 预留问的是「这个候选渠道上生效什么」，而 0 不是候选。
func (c *Catalog) OverridesFor(ctx context.Context, canonicalModel string, channelIDs []int64) (map[int64]map[string]any, error) {
	if canonicalModel == "" {
		return nil, errors.New("规范模型名为空")
	}
	out := make(map[int64]map[string]any, len(channelIDs))
	if len(channelIDs) == 0 {
		return out, nil
	}

	rows, err := c.aliasRows(ctx, canonicalModel)
	if err != nil {
		return nil, err
	}
	for _, channelID := range channelIDs {
		if channelID == 0 {
			continue
		}
		picked, matched := pickAlias(rows, channelID)
		if matched && len(picked.Overrides) > 0 {
			out[channelID] = picked.Overrides
		}
	}
	return out, nil
}

// Constraints 是 model_catalog 上选路要认的那几列：能力矩阵与两个上限。
//
// # 为什么与 Model 分开
//
// 它读的是**请求热路径**：每个请求选路时都要按它筛候选（见
// internal/route 的 Candidate.Profile）。Model 是管理面的读取面，会把
// raw、reasoning_options 两个 JSON 列一起解出来——那些列对选路没有用，
// 而热路径上每一条请求都要付这份解码成本。
//
// 六个能力布尔一起读，不做「只读判据用到的那三项」的裁剪：它们在同一行上，
// 少读三列省不下一次往返，却会让这个结构体的字段看起来像「这个模型只有
// 三项能力」。判据用到哪几项是**读侧**的事（见 internal/route 的 ModelProfile）。
//
// # input_limit 不在这里
//
// 三个上限里只有 context_limit 与 output_limit 参与选路判据；input_limit
// 今天没有消费者（管理面读写它，没有一处按它判断）。所以不读它——不为消掉
// 一列硬接一条假的消费，与 internal/channels 的 candidateQuery 对 failures
// 那一列的处置同一条理由。
type Constraints struct {
	// Capabilities 是六个能力布尔，一律 NOT NULL DEFAULT false，
	// 所以这里没有「未知」这个状态：没写进目录就等于不支持。
	Capabilities Capabilities
	// ContextLimit 是上下文窗口（输入 + 输出）。**0 表示未知**（库里是 NULL）
	// ——与能力列不同，这一列允许为空。
	ContextLimit int
	// OutputLimit 是单次输出的上限。0 表示未知。
	OutputLimit int
}

// Constraints 按规范名读回选路要的那几列。
//
// 目录里没有这个模型时返回 ErrModelNotFound：那是**未知**，不是「不支持」
// ——调用方据此放行（见 internal/route 的 ModelProfile 注释）。两者的区别
// 在库里看不出来：一条采集到一半的记录与一条没采集到的记录，长得很不一样，
// 而「有一条记录但能力列全是 false」与「压根没有这条记录」恰恰是后者更像
// 「未知」的那一侧。
func (c *Catalog) Constraints(ctx context.Context, canonicalName string) (Constraints, error) {
	if canonicalName == "" {
		return Constraints{}, errors.New("规范模型名为空")
	}

	rows, err := c.db.QueryContext(ctx, constraintsQuery, canonicalName)
	if err != nil {
		return Constraints{}, fmt.Errorf("查询规范模型失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		// 与 Model 同一条：变量名不复用外层那个 err，避免 govet 的 shadow。
		if rowErr := rows.Err(); rowErr != nil {
			return Constraints{}, fmt.Errorf("查询规范模型失败: %w", rowErr)
		}
		return Constraints{}, fmt.Errorf("%w：%q", ErrModelNotFound, canonicalName)
	}

	var out Constraints
	err = rows.Scan(
		&out.ContextLimit, &out.OutputLimit,
		&out.Capabilities.Reasoning, &out.Capabilities.Tools,
		&out.Capabilities.Vision, &out.Capabilities.Audio,
		&out.Capabilities.Cache, &out.Capabilities.Batch,
	)
	if err != nil {
		return Constraints{}, fmt.Errorf("读取规范模型行失败: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Constraints{}, fmt.Errorf("遍历规范模型失败: %w", err)
	}
	return out, nil
}

// Model 按规范名取回一条规范模型。
//
// 管理面的写入路径用它读回来核对——写入与读取对同一套列的假设是否一致，
// 只有「写进去再读出来」能证明。
func (c *Catalog) Model(ctx context.Context, canonicalName string) (Model, error) {
	if canonicalName == "" {
		return Model{}, errors.New("规范模型名为空")
	}

	rows, err := c.db.QueryContext(ctx, modelQuery+` WHERE m.canonical_name = $1`, canonicalName)
	if err != nil {
		return Model{}, fmt.Errorf("查询规范模型失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		// 变量名不复用外层那个 err：同一个块里再 `err :=` 是遮蔽，
		// govet 的 shadow 检查器会报（.golangci.yml 里 govet 开了 enable-all）。
		if rowErr := rows.Err(); rowErr != nil {
			return Model{}, fmt.Errorf("查询规范模型失败: %w", rowErr)
		}
		return Model{}, fmt.Errorf("%w：%q", ErrModelNotFound, canonicalName)
	}
	model, err := scanModel(rows)
	if err != nil {
		return Model{}, err
	}
	if err := rows.Err(); err != nil {
		return Model{}, fmt.Errorf("遍历规范模型失败: %w", err)
	}
	return model, nil
}

// Models 列出全部规范模型，按规范名排序。
//
// 从空切片开始而不是 nil：目录是空的（还没采集过）是正常结果，不是「没有值」。
func (c *Catalog) Models(ctx context.Context) ([]Model, error) {
	rows, err := c.db.QueryContext(ctx, modelQuery+` ORDER BY m.canonical_name`)
	if err != nil {
		return nil, fmt.Errorf("查询规范模型失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Model{}
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历规范模型失败: %w", err)
	}
	return out, nil
}

// Aliases 列出某个渠道配了哪些别名，按上游模型名排序。
//
// channelID 传 0 表示列**平台级**别名（channel_id IS NULL）——与
// Alias 的 0 同一个含义。两个范围不会混在一条结果里：一条渠道级的
// 别名说的只是「这家上游这么叫」，把它混进平台级清单会让人以为它对所有渠道
// 生效，而那是错的。
func (c *Catalog) Aliases(ctx context.Context, channelID int64) ([]Alias, error) {
	if channelID < 0 {
		return nil, fmt.Errorf("渠道 id %d 无效", channelID)
	}

	query := aliasQuery + ` WHERE a.channel_id = $1 ORDER BY a.upstream_model`
	args := []any{channelID}
	if channelID == 0 {
		query = aliasQuery + ` WHERE a.channel_id IS NULL ORDER BY a.upstream_model`
		args = nil
	}

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询模型别名失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Alias{}
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历模型别名失败: %w", err)
	}
	return out, nil
}

// UpstreamModels 列出某个规范名在各处对应了哪些上游名。
//
// 渠道级与平台级一起返回（Alias.ChannelID 区分），顺序按「平台级在前、
// 再按渠道 id」——平台级那条是兜底，先看它符合人对配置的读法。
//
// 一条都没有时返回空切片而不是错误：目录里有个模型、但还没有任何渠道
// 配别名，是**采集完成而渠道未接**的正常中间状态。
func (c *Catalog) UpstreamModels(ctx context.Context, canonicalName string) ([]Alias, error) {
	if canonicalName == "" {
		return nil, errors.New("规范模型名为空")
	}

	rows, err := c.db.QueryContext(ctx, aliasQuery+`
		 WHERE a.canonical_name = $1
		 ORDER BY (a.channel_id IS NOT NULL), a.channel_id, a.upstream_model`, canonicalName)
	if err != nil {
		return nil, fmt.Errorf("查询模型别名失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Alias{}
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历模型别名失败: %w", err)
	}
	return out, nil
}

// ── 查询与解码 ──────────────────────────────────────────────────────────
//
// 读写两侧对同一套列的假设写在同一个地方：查询在这里，编码在 admin.go，
// 两处对不上的症状是「写进去的值读出来不对」，而那通常只发生在某些行上。

const modelQuery = `
SELECT
    m.id,
    m.canonical_name,
    m.display_name,
    COALESCE(m.vendor, ''),
    COALESCE(m.family, ''),
    COALESCE(m.context_limit, 0),
    COALESCE(m.input_limit, 0),
    COALESCE(m.output_limit, 0),
    m.supports_reasoning,
    m.supports_tools,
    m.supports_vision,
    m.supports_audio,
    m.supports_cache,
    m.supports_batch,
    array_to_json(m.modalities_input)::text,
    array_to_json(m.modalities_output)::text,
    m.reasoning_options,
    COALESCE(m.canonical_model_id, ''),
    m.knowledge_cutoff,
    m.release_date,
    m.raw,
    m.updated_at
  FROM model_catalog m`

// constraintsQuery 是热路径那条查询：只取选路判据要的列。
//
// 与 modelQuery 一样对两个上限套 COALESCE（NULL 折成 0 = 未知），能力列
// 原样读——它们是 NOT NULL，false 是一个真实的值。两个列族的这个差别写在
// Capabilities 与 Model 的注释里，改这张表的列时要一起看。
const constraintsQuery = `
SELECT
    COALESCE(m.context_limit, 0),
    COALESCE(m.output_limit, 0),
    m.supports_reasoning,
    m.supports_tools,
    m.supports_vision,
    m.supports_audio,
    m.supports_cache,
    m.supports_batch
  FROM model_catalog m
 WHERE m.canonical_name = $1`

// aliasQuery 取回一条别名的全部列。
//
// channel_id 用 COALESCE 折成 0：Go 侧只需要一个「是不是平台级」的判断，
// 而 sql.NullInt64 会让每个使用它的地方都多一次解包。
const aliasQuery = `
SELECT
    a.id,
    COALESCE(a.channel_id, 0),
    a.upstream_model,
    a.canonical_name,
    a.overrides,
    a.created_at
  FROM model_aliases a`

func scanModel(rows *sql.Rows) (Model, error) {
	var (
		model        Model
		inJSON       string
		outJSON      string
		reasoningRaw []byte
		rawJSON      []byte
		knowledgeCut sql.NullTime
		releaseDate  sql.NullTime
	)
	err := rows.Scan(
		&model.ID, &model.CanonicalName, &model.DisplayName, &model.Vendor, &model.Family,
		&model.ContextLimit, &model.InputLimit, &model.OutputLimit,
		&model.Capabilities.Reasoning, &model.Capabilities.Tools,
		&model.Capabilities.Vision, &model.Capabilities.Audio,
		&model.Capabilities.Cache, &model.Capabilities.Batch,
		&inJSON, &outJSON, &reasoningRaw, &model.CanonicalModelID,
		&knowledgeCut, &releaseDate, &rawJSON, &model.UpdatedAt,
	)
	if err != nil {
		return Model{}, fmt.Errorf("读取规范模型行失败: %w", err)
	}

	var decodeErr error
	if model.ModalitiesInput, decodeErr = decodeStrings(inJSON); decodeErr != nil {
		return Model{}, fmt.Errorf("规范模型 %q 的 modalities_input 不是合法 JSON 数组: %w", model.CanonicalName, decodeErr)
	}
	if model.ModalitiesOutput, decodeErr = decodeStrings(outJSON); decodeErr != nil {
		return Model{}, fmt.Errorf("规范模型 %q 的 modalities_output 不是合法 JSON 数组: %w", model.CanonicalName, decodeErr)
	}
	if decodeErr = json.Unmarshal(reasoningRaw, &model.ReasoningOptions); decodeErr != nil {
		return Model{}, fmt.Errorf("规范模型 %q 的 reasoning_options 不是合法 JSON 数组: %w", model.CanonicalName, decodeErr)
	}
	if err := json.Unmarshal(rawJSON, &model.Raw); err != nil {
		return Model{}, fmt.Errorf("规范模型 %q 的 raw 不是合法 JSON 对象: %w", model.CanonicalName, err)
	}
	if knowledgeCut.Valid {
		model.KnowledgeCutoff = &knowledgeCut.Time
	}
	if releaseDate.Valid {
		model.ReleaseDate = &releaseDate.Time
	}
	return model, nil
}

func scanAlias(rows *sql.Rows) (Alias, error) {
	var (
		alias   Alias
		overRaw []byte
	)
	err := rows.Scan(&alias.ID, &alias.ChannelID, &alias.UpstreamModel,
		&alias.CanonicalName, &overRaw, &alias.CreatedAt)
	if err != nil {
		return Alias{}, fmt.Errorf("读取模型别名行失败: %w", err)
	}
	if err := json.Unmarshal(overRaw, &alias.Overrides); err != nil {
		return Alias{}, fmt.Errorf("别名 %d 的 overrides 不是合法 JSON 对象: %w", alias.ID, err)
	}
	return alias, nil
}

// decodeStrings 解 text[] 经 array_to_json 转出来的 JSON 文本。
//
// 与 channels 的 decodeModels 同一个做法：database/sql 不认 text[]，
// 所以查询侧统一转成 JSON 文本，解码在这里。
//
// **永远返回非 nil 的切片**：两个 modalities 列都是 NOT NULL，空数组是
// 正常值（这个模型没有这类模态），而 nil 会让调用方得先判空才能比较。
// 空列在表上出现不了，真拿到空串说明这一行不是按表定义写进去的——
// 那也按空数组给，让读路径不必为一种不可能的状态写分支。
func decodeStrings(raw string) ([]string, error) {
	out := []string{}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
