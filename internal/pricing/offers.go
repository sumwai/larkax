package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是**报价**：对一组「模型 × 渠道（连同渠道所属商家）」各取一条适用
// 的售价规则，求出一个可以展示给调用方的价格。
//
// # 它与结算的关系
//
// 取规则用的是与结算同一段 SQL 判据与同一套裁决次序（见 ruleApplicable 与
// ruleOrderBy），求值用的是与结算同一个函数（billing.ComputeWithCharges）。
// 本文件**不做第二种算术**：报价与一次「1000 个输入 token」的结算走的是
// 同一条路径，差别只在用量是固定的。
//
// 这不是为了复用代码，而是为了不让两个数分岔：只要报价自己乘一遍用量，
// 「接口上说 0.0035、账单上按 0.0038 收」就会成为一类查不出来的分歧——
// 两处都算得出一个看起来正常的数。

// offerTokenCount 是报价的参考用量：1000 个输入 token。
//
// 报价按「每千 token」展示，所以乘出来的数就是单价本身，调用方不必再做
// 一次除法——除法是单价的舍入点。缓存与输出不参与：它们各有自己的单价，
// 混进一个「每千 token」的数里就没有意义了。
const offerTokenCount = 1000

// Candidate 是一次报价查询里的一个候选：某个模型在某个渠道上的一条供给。
//
// ChannelID 与 OwnerUserID 直接来自渠道行，调用方不需要先判「这条渠道是
// 平台自营还是商家的」：OwnerUserID 为 0 就是平台自营，与 route.Candidate
// 的约定相同。
type Candidate struct {
	Model       string
	ChannelID   int64
	OwnerUserID int64
	// Merchant 是归属商家的展示名（用户名）；平台自营留空。
	//
	// 它只用于展示，不参与取价：归属判据始终是 OwnerUserID。同名不同 id
	// 在库里不可能（users.username 唯一），所以它只出现在响应里。
	Merchant string
}

// CandidatePrice 是一个候选的报价。
//
// 没有配价（或规则全部不适用）的候选**不出现**在结果里：`Price` 的零值是
// 「0 元」，而那与「没报价」是两件不同的事——把后者写成前者，界面上会显示
// 一个免费价。
type CandidatePrice struct {
	Model       string
	ChannelID   int64
	OwnerUserID int64
	Merchant    string
	// Price 是 1000 个输入 token 的金额，币种沿用结算用的资产维度。
	Price money.Amount
}

// Offer 是一个模型的一条展示报价：商家为 nil 表示平台自营。
type Offer struct {
	Merchant *string
	Price    money.Amount
}

// GroupByModel 把候选价格折成「每个模型一组报价」。
//
// 去重按 (商家, 价格)：同一个商家供多条渠道时，同价只报一次——调用方问的
// 是「这个模型有谁卖、多少钱」，不是「有几条渠道」。
//
// 排序按价格升序；同价时商家名升序、平台自营排在最前。不做「最低价」标记：
// 那等于替调用方判断哪个价该选，而不同商家的渠道在健康度、速度上并不相同，
// 价格不是唯一的比较维度。
//
// 两个 HTTP 表面（调用方清单与控制台）共用这一处折叠：各写一遍的那天，
// 就是「接口上按 A 排序、页面上按 B 排序」开始的那天。
func GroupByModel(prices []CandidatePrice) map[string][]Offer {
	type key struct {
		ownerID int64
		price   string
	}
	seen := make(map[string]map[key]bool, len(prices))
	grouped := make(map[string][]CandidatePrice, len(prices))
	for _, price := range prices {
		if seen[price.Model] == nil {
			seen[price.Model] = make(map[key]bool)
		}
		k := key{ownerID: price.OwnerUserID, price: price.Price.String()}
		if seen[price.Model][k] {
			continue
		}
		seen[price.Model][k] = true
		grouped[price.Model] = append(grouped[price.Model], price)
	}

	out := make(map[string][]Offer, len(grouped))
	for model, items := range grouped {
		sort.SliceStable(items, func(i, j int) bool {
			if cmp := items[i].Price.Cmp(items[j].Price); cmp != 0 {
				return cmp < 0
			}
			if items[i].OwnerUserID == 0 || items[j].OwnerUserID == 0 {
				return items[i].OwnerUserID == 0 && items[j].OwnerUserID != 0
			}
			return items[i].Merchant < items[j].Merchant
		})
		offers := make([]Offer, 0, len(items))
		for _, price := range items {
			offer := Offer{Price: price.Price}
			if price.OwnerUserID != 0 {
				merchant := price.Merchant
				offer.Merchant = &merchant
			}
			offers = append(offers, offer)
		}
		out[model] = offers
	}
	return out
}

// Offers 求一组候选的报价。
//
// 它与 Source 分开是因为依赖不同：Source 只查库；这里还要日历（规则可能用
// isPeak / isWorkday）与求值时刻，以及一份编译缓存。与 References 分开则是
// 因为回答的问题不同：那一份只算平台兜底价，这一份按候选各自的作用域算。
type Offers struct {
	db       *sql.DB
	region   string
	calendar expr.Calendar
	programs *expr.Cache
	now      func() time.Time
}

// NewOffers 装配报价的数据源。
//
// calendar 可以为 nil：不用日历的规则照样求值；用了日历而这里为 nil 时，
// 求值会报 ErrCalendarRequired，那条规则的报价算不出来——比按 UTC 的钟点
// 算一个错价要好。
func NewOffers(db *sql.DB, region string, calendar expr.Calendar) *Offers {
	return &Offers{
		db:       db,
		region:   region,
		calendar: calendar,
		programs: expr.NewCache(offerCacheSize),
		now:      time.Now,
	}
}

// offerCacheSize 是编译缓存的容量。一次清单请求的候选通常是「模型数 ×
// 几个渠道」，重复的表达式（同一条规则覆盖多个模型）只编译一次，64 足够
// 覆盖常见配置。
const offerCacheSize = 64

// offerRulesQuery 一次取回全部候选各自适用的那条规则。
//
// $1 是匹配时刻，$2/$3/$4 是三个平行数组：模型名、渠道 id、商家 id。
//
// 一条 SQL 查完全部候选，而不是每个模型往返一次：清单规模由目录决定，
// 而这条路是面向调用方的高频只读接口。LATERAL 让每个候选各自取它的
// 第一条规则，判据与次序与 Match / MatchAll 逐字相同（同一段片段）。
var offerRulesQuery = `
	SELECT q.model, q.channel_id, q.owner_user_id, r.id, r.expr
	  FROM unnest($2::text[], $3::bigint[], $4::bigint[])
	       AS q(model, channel_id, owner_user_id)
	  LEFT JOIN LATERAL (
	        SELECT r.id, r.expr
	          FROM pricing_rules r
	         WHERE ` + ruleApplicable("q.model", "q.owner_user_id", "q.channel_id") + `
	         ORDER BY ` + ruleOrderBy + `
	         LIMIT 1
	  ) AS r ON true
	 ORDER BY q.model, q.channel_id`

// Prices 求每个候选的报价。
//
// at 为零值时用当前时刻。返回的切片只含有报价的候选，顺序按 (模型, 渠道)。
func (o *Offers) Prices(ctx context.Context, at time.Time, candidates []Candidate) ([]CandidatePrice, error) {
	if len(candidates) == 0 {
		return []CandidatePrice{}, nil
	}
	if at.IsZero() {
		at = o.now().UTC()
	}

	// 去重：同一个 (模型, 渠道) 出现两次只会得到同一份报价，而重复的行
	// 会让调用方多出一条看上去像「两个商家」的报价。
	type candidateKey struct {
		model     string
		channelID int64
	}
	seen := make(map[candidateKey]bool, len(candidates))
	byKey := make(map[candidateKey]Candidate, len(candidates))
	unique := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Model == "" {
			return nil, errors.New("候选的模型名为空")
		}
		key := candidateKey{model: candidate.Model, channelID: candidate.ChannelID}
		if seen[key] {
			continue
		}
		seen[key] = true
		byKey[key] = candidate
		unique = append(unique, candidate)
	}

	models := make([]string, len(unique))
	channelIDs := make([]int64, len(unique))
	ownerIDs := make([]int64, len(unique))
	for i, candidate := range unique {
		models[i] = candidate.Model
		channelIDs[i] = candidate.ChannelID
		ownerIDs[i] = candidate.OwnerUserID
	}

	rows, err := o.db.QueryContext(ctx, offerRulesQuery, at, models, channelIDs, ownerIDs)
	if err != nil {
		return nil, fmt.Errorf("查询候选计价规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 同一个表达式可能被多个候选命中（一条兜底规则覆盖很多模型），
	// 按 (规则 id, 版本) 缓存求值结果。
	type ruleKey struct {
		id      int64
		version int64
	}
	prices := make(map[ruleKey]money.Amount, len(unique))

	out := []CandidatePrice{}
	for rows.Next() {
		var (
			model     string
			channelID int64
			ownerID   int64
			ruleID    sql.NullInt64
			source    sql.NullString
		)
		if scanErr := rows.Scan(&model, &channelID, &ownerID, &ruleID, &source); scanErr != nil {
			return nil, fmt.Errorf("读候选计价规则失败: %w", scanErr)
		}
		// 没有匹配规则的候选在这一行上是 NULL（LEFT JOIN 的产物）。
		// 跳过它，而不是给它编一个零价——零价在界面上就是「免费」。
		if !ruleID.Valid {
			continue
		}

		key := ruleKey{id: ruleID.Int64, version: versionOf(source.String)}
		price, ok := prices[key]
		if !ok {
			price, err = o.priceOf(ruleID.Int64, source.String, at)
			if err != nil {
				return nil, err
			}
			prices[key] = price
		}
		merchant := byKey[candidateKey{model: model, channelID: channelID}].Merchant
		out = append(out, CandidatePrice{
			Model:       model,
			ChannelID:   channelID,
			OwnerUserID: ownerID,
			Merchant:    merchant,
			Price:       price,
		})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("读候选计价规则失败: %w", err)
	}
	return out, nil
}

// priceOf 编译并求一条规则在 1000 个输入 token 下的价。
func (o *Offers) priceOf(ruleID int64, source string, at time.Time) (money.Amount, error) {
	program, err := o.programs.Get(fmt.Sprintf("%d:%d", ruleID, versionOf(source)), source)
	if err != nil {
		return money.Zero, fmt.Errorf("编译规则 %d 失败: %w", ruleID, err)
	}
	usage := billing.Usage{PromptTokens: offerTokenCount}
	result, err := program.Eval(expr.Env{
		At:       at,
		Region:   o.region,
		Calendar: o.calendar,
		Usage:    usage,
	})
	if err != nil {
		return money.Zero, fmt.Errorf("求规则 %d 的报价失败: %w", ruleID, err)
	}
	// 与结算同一个函数：价格行的金额就是 `ComputeWithCharges` 的 Prompt
	// 那一项。固定费与最低消费不进报价——它们是「一次请求」的费用，而报价
	// 回答的是「一千个输入 token 多少钱」。
	return billing.ComputeWithCharges(usage, result.Prices, result.Charges).Prompt, nil
}
