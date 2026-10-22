package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
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

// offerTokenCount 是报价的参考用量：每个维度 1000 个 token。
//
// 报价按「每千 token」展示，所以乘出来的数就是单价本身，调用方不必再做
// 一次除法——除法是单价的舍入点。
//
// 五个维度（prompt / completion / cache_read / cache_write / reasoning）各
// 自报一个数，而不是只报输入价：它们不是一个价的倍数，缓存读价通常低一个
// 量级，推理价通常更高。混成一个数会让调用方拿输入价估算一笔缓存为主的
// 请求，差出好几倍。
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

// RuleRef 是一条规则对外可见的那部分元信息：编号、作用域、优先级与生效窗口。
//
// 不含表达式源码，也不含 owner id 与 channel id：源码是定价逻辑，后两者是
// 「谁定的价」，而两条公开响应都不能把它们给出去。作用域只说这一档价是按
// 渠道、按商家还是平台兜底定的，不指到具体是哪条渠道。
type RuleRef struct {
	ID            int64
	Scope         string
	Priority      int
	EffectiveFrom time.Time
	// EffectiveTo 为 nil 表示不过期。
	EffectiveTo *time.Time
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
	// Prices 是各 token 维度的千 token 金额，键是 billing.TokenSlots 里的
	// 槽位名；只含规则**显式写过**的维度。
	//
	// 调用方不得修改：同一个表达式命中多个候选时，这里共用一份 map。
	Prices map[string]money.Amount
	// Price 是 prompt 维度的金额，保留给只关心输入价的调用方。
	// 规则没写 prompt 时它是零值——要区分「免费」与「没写」，看 Prices 里
	// 有没有这个键，不要看 Price 的大小。
	Price money.Amount
	// Request 是每请求的固定费用（charges.Fixed），量纲是「每次请求多少元」，
	// 不能与 Prices 里那些「每 1000 个 token 多少元」相加。零值表示规则
	// 没写固定费。
	Request money.Amount
	// Minimum 是每次请求的最低消费（charges.Minimum）。零值表示没写。
	Minimum money.Amount
	// Units 是非 token 计量项的单价，键是规则里写过的计量项名（例如
	// cache_write_5m）；每个值都是「每个计量单位多少元」。
	//
	// 与 Prices 一样由同一次求值得到，调用方不得修改。
	Units map[string]money.Amount
	// Rule 是这套价出自哪条规则。
	Rule RuleRef
}

// Offer 是一个模型的一条展示报价：商家为 nil 表示平台自营。
//
// 同一个商家可能有多套价（不同渠道配了不同的价），所以一条 Offer 代表的
// 是「商家 × 一整套维度价」，ChannelCount 说明这套价有几条渠道在用。
type Offer struct {
	Merchant *string
	Price    money.Amount
	// Prices 的键与含义同 CandidatePrice.Prices。
	Prices map[string]money.Amount
	// Request / Minimum / Units 的含义同 CandidatePrice 的同名字段。
	Request money.Amount
	Minimum money.Amount
	Units   map[string]money.Amount
	// Rule 是这套价出自哪条规则。同价的多条渠道折成一行时，它是其中一条
	// 渠道适用的规则——价格相同的几条规则对调用方没有区别，但溯源只报一条
	// 比编一个合成的来源诚实。
	Rule RuleRef
	// ChannelCount 是这套价的渠道条数。渠道 id 不对外，去重只能落在
	// 「商家 + 价」上；条数让调用方知道这是一条渠道还是一个商家的多条供给。
	ChannelCount int
}

// ModelOffers 是一个模型的全部展示报价与统计。
type ModelOffers struct {
	// Offers 按报价排序（判据见 lessOffer）：有 token 价的在前，只有一次性
	// 费用的在后。没有任何能算出价的渠道时为空。
	Offers []Offer
	// UnpricedChannels 是在供、但算不出价的渠道条数。
	//
	// 它与 Offers 回答的是两件事：Offers 说「谁卖多少钱」，这个数说
	// 「还有几条渠道在供、只是没有适用规则」。没有它，一个部分定价的模型
	// 看起来就像只有报价里那几个商家在供。
	UnpricedChannels int
}

// GroupByModel 把候选价格折成「每个模型一组报价」。
//
// candidates 是这一轮的全部候选（含算不出价的），用来数每条模型在供的
// 渠道；prices 只含能算出价的候选。两者分开传是因为报价层刻意不给算不出
// 价的候选编一个零价（见 CandidatePrice 的注释），而「有几条渠道没价」
// 仍要有人回答。
//
// 去重按 (商家, 一整套维度价)：同一个商家供多条渠道时，整套价相同只报
// 一次，并用 ChannelCount 记条数。只比 prompt 是不够的——两条渠道的输入价
// 相同、缓存价不同，是两套不同的报价，折成一条会让调用方按错的价格估算。
//
// 排序先看量纲：有 token 价的报价按 prompt 那一项升序（规则只有 completion
// 时用第一个存在的维度代替），只有一次性费用的排在它们之后，组内按每请求
// 固定费、再按最低消费升序。两个量纲不可比，混排会让「按次收费」看起来像
// 最便宜的一档。同一组内平台自营在前，再按商家名、整套价、渠道 id。
//
// 两个 HTTP 表面（调用方清单与控制台）共用这一处折叠：各写一遍的那天，
// 就是「接口上按 A 排序、页面上按 B 排序」开始的那天。
func GroupByModel(prices []CandidatePrice, candidates []Candidate) map[string]ModelOffers {
	type key struct {
		ownerID int64
		prices  string
	}
	type acc struct {
		sample CandidatePrice
		count  int
	}
	grouped := make(map[string]map[key]*acc, len(prices))
	pricedByChannel := make(map[string]map[int64]bool, len(prices))
	for _, price := range prices {
		byKey := grouped[price.Model]
		if byKey == nil {
			byKey = make(map[key]*acc)
			grouped[price.Model] = byKey
		}
		if pricedByChannel[price.Model] == nil {
			pricedByChannel[price.Model] = make(map[int64]bool)
		}
		pricedByChannel[price.Model][price.ChannelID] = true

		k := key{ownerID: price.OwnerUserID,
			prices: priceSignature(price.Prices, price.Request, price.Minimum, price.Units)}
		if existing := byKey[k]; existing != nil {
			existing.count++
			continue
		}
		byKey[k] = &acc{sample: price, count: 1}
	}

	// 每个模型在供的渠道集合。用渠道 id 而不是候选条数：同一个
	// (模型, 渠道) 重复出现只算一条。
	supplied := make(map[string]map[int64]bool, len(candidates))
	for _, candidate := range candidates {
		if supplied[candidate.Model] == nil {
			supplied[candidate.Model] = make(map[int64]bool)
		}
		supplied[candidate.Model][candidate.ChannelID] = true
	}

	out := make(map[string]ModelOffers, len(supplied))
	for model, channels := range supplied {
		unpriced := 0
		for channelID := range channels {
			if !pricedByChannel[model][channelID] {
				unpriced++
			}
		}
		out[model] = ModelOffers{UnpricedChannels: unpriced}
	}
	for model, byKey := range grouped {
		sortable := make([]sortableOffer, 0, len(byKey))
		for _, item := range byKey {
			offer := Offer{
				Price:        item.sample.Prices["prompt"],
				Prices:       copyPrices(item.sample.Prices),
				Request:      item.sample.Request,
				Minimum:      item.sample.Minimum,
				Units:        copyPrices(item.sample.Units),
				Rule:         item.sample.Rule,
				ChannelCount: item.count,
			}
			if item.sample.OwnerUserID != 0 {
				merchant := item.sample.Merchant
				offer.Merchant = &merchant
			}
			sortable = append(sortable, sortableOffer{offer: offer, channelID: item.sample.ChannelID})
		}
		sort.SliceStable(sortable, func(i, j int) bool { return lessOffer(sortable[i], sortable[j]) })

		offers := make([]Offer, 0, len(sortable))
		for _, item := range sortable {
			offers = append(offers, item.offer)
		}
		entry := out[model]
		entry.Offers = offers
		out[model] = entry
	}
	return out
}

// sortableOffer 是排序期的报价：对外形状加上一个只用于定序的渠道 id。
//
// 渠道 id 不进 Offer——它对调用方没有意义，也不该出现在公开响应里；但排序
// 需要一个能把「三个键全相同」的两笔报价分开的比较键，而它恰好是现成且
// 稳定的：同一个渠道在同一份数据里永远给出同一个 id。没有它，末位比较会
// 落到 map 的迭代顺序上，同一份数据两次请求可能给出不同的行序。
type sortableOffer struct {
	offer     Offer
	channelID int64
}

// lessOffer 是报价的排序判据，从主键到末位兜底依次比较：
//
//  1. **有 token 价的排在前**，组内按 offerSortPrice 升序。没有 token 价的
//     报价在 offerSortPrice 上的零值会让它排到最前，看起来最便宜；而它与
//     每千 token 的金额量纲不同，不能这样混排。
//  2. **只有一次性费用的排在后面**，组内按每请求固定费、再按最低消费升序。
//     两者都是「每次请求多少元」，彼此可比。
//  3. 平台自营在前，再按商家名、整套价的内容。
//  4. 渠道 id——保证这是一个全序：任意两笔报价都有确定的先后。
func lessOffer(a, b sortableOffer) bool {
	aHas, bHas := len(a.offer.Prices) > 0, len(b.offer.Prices) > 0
	if aHas != bHas {
		return aHas
	}
	if aHas {
		if cmp := offerSortPrice(a.offer.Prices).Cmp(offerSortPrice(b.offer.Prices)); cmp != 0 {
			return cmp < 0
		}
	} else {
		if cmp := a.offer.Request.Cmp(b.offer.Request); cmp != 0 {
			return cmp < 0
		}
		if cmp := a.offer.Minimum.Cmp(b.offer.Minimum); cmp != 0 {
			return cmp < 0
		}
	}
	if (a.offer.Merchant == nil) != (b.offer.Merchant == nil) {
		return a.offer.Merchant == nil
	}
	if a.offer.Merchant != nil && *a.offer.Merchant != *b.offer.Merchant {
		return *a.offer.Merchant < *b.offer.Merchant
	}
	aSignature := priceSignature(a.offer.Prices, a.offer.Request, a.offer.Minimum, a.offer.Units)
	bSignature := priceSignature(b.offer.Prices, b.offer.Request, b.offer.Minimum, b.offer.Units)
	if aSignature != bSignature {
		return aSignature < bSignature
	}
	return a.channelID < b.channelID
}

// priceSignature 把一整套价写成稳定的字符串，用作去重与排序的键。
//
// 一整套价包括 token 维度、一次性费用与计量项单价：两条渠道的 token 价
// 相同而固定费不同，是两套不同的报价，按 token 价折成一条会让调用方按错的
// 总价估算。
//
// token 维度必须按 TokenSlots 的顺序拼，计量项按名字排序：map 的迭代顺序
// 每次不同，直接遍历 map 会让同一个商家同一套价在两次请求里得到不同的键。
func priceSignature(prices map[string]money.Amount, request, minimum money.Amount,
	units map[string]money.Amount) string {
	var b strings.Builder
	for _, slot := range billing.TokenSlots {
		value, ok := prices[slot.Name]
		if !ok {
			continue
		}
		b.WriteString(slot.Name)
		b.WriteByte('=')
		b.WriteString(value.String())
		b.WriteByte(';')
	}
	b.WriteString("request=")
	b.WriteString(request.String())
	b.WriteString(";minimum=")
	b.WriteString(minimum.String())
	b.WriteByte(';')
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString("unit:")
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(units[name].String())
		b.WriteByte(';')
	}
	return b.String()
}

// offerSortPrice 取 token 维度的排序键：prompt 价；规则没写 prompt 时用
// 第一个存在的维度。
//
// 调用点只在 Prices 非空时用它：没有 token 价的报价（只有固定费）在
// lessOffer 里先被分到另一组，零值不再是一个排序位置。
func offerSortPrice(prices map[string]money.Amount) money.Amount {
	if value, ok := prices["prompt"]; ok {
		return value
	}
	for _, slot := range billing.TokenSlots {
		if value, ok := prices[slot.Name]; ok {
			return value
		}
	}
	return money.Zero
}

// copyPrices 复制一份维度价，让对外暴露的 Offer 不与缓存共用 map。
func copyPrices(prices map[string]money.Amount) map[string]money.Amount {
	out := make(map[string]money.Amount, len(prices))
	for name, value := range prices {
		out[name] = value
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
	SELECT q.model, q.channel_id, q.owner_user_id,
	       r.id, r.expr, r.scope, r.priority, r.effective_from, r.effective_to
	  FROM unnest($2::text[], $3::bigint[], $4::bigint[])
	       AS q(model, channel_id, owner_user_id)
	  LEFT JOIN LATERAL (
	        SELECT r.id, r.expr, r.scope, r.priority, r.effective_from, r.effective_to
	          FROM pricing_rules r
	         WHERE ` + ruleApplicable("q.model", "q.owner_user_id", "q.channel_id") + `
	         ORDER BY ` + ruleOrderBy + `
	         LIMIT 1
	  ) AS r ON true
	 ORDER BY q.model, q.channel_id`

// Prices 求每个候选的报价。
//
// at 为零值时用当前时刻。返回的切片只包含求得出价的候选，顺序按 (模型, 渠道)：
// 没有适用规则的候选，以及命中了一条编译或求值失败的规则的候选，都被跳过。
// 求不出价只影响那个候选——某条规则写坏不该让整份清单变成错误；全部候选都
// 求不出价时返回空切片与 nil 错误。查询本身失败仍是错误。
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
	prices := make(map[ruleKey]rulePrices, len(unique))
	// 取价失败的规则也按同一个键记下来：一条兜底规则写坏时，命中它的候选
	// 可能有几十个，它们都该被跳过，而日志只该记一次。
	broken := make(map[ruleKey]bool)

	out := []CandidatePrice{}
	for rows.Next() {
		var (
			model         string
			channelID     int64
			ownerID       int64
			ruleID        sql.NullInt64
			source        sql.NullString
			scope         sql.NullString
			priority      sql.NullInt64
			effectiveFrom sql.NullTime
			effectiveTo   sql.NullTime
		)
		if scanErr := rows.Scan(&model, &channelID, &ownerID, &ruleID, &source,
			&scope, &priority, &effectiveFrom, &effectiveTo); scanErr != nil {
			return nil, fmt.Errorf("读候选计价规则失败: %w", scanErr)
		}
		// 没有匹配规则的候选在这一行上是 NULL（LEFT JOIN 的产物）。
		// 跳过它，而不是给它编一个零价——零价在界面上就是「免费」。
		if !ruleID.Valid {
			continue
		}

		key := ruleKey{id: ruleID.Int64, version: versionOf(source.String)}
		quoted, ok := prices[key]
		if !ok {
			if broken[key] {
				continue
			}
			// 一条规则取不出价，只跳过命中它的候选：某个商家配错一条规则，
			// 不该让所有调用方的模型清单都变成 500。规则 id 与原因记日志，
			// 否则这次跳过在外部只剩下「这个模型没有报价」。
			quoted, err = o.pricesOf(ruleID.Int64, source.String, at)
			if err != nil {
				broken[key] = true
				observe.FromContext(ctx).Warn("规则取价失败，跳过命中它的候选",
					"rule_id", ruleID.Int64,
					"reason", firstLine(err))
				continue
			}
			prices[key] = quoted
		}
		merchant := byKey[candidateKey{model: model, channelID: channelID}].Merchant
		out = append(out, CandidatePrice{
			Model:       model,
			ChannelID:   channelID,
			OwnerUserID: ownerID,
			Merchant:    merchant,
			// 缓存的 map 只读共享：调用方不得修改它（见 CandidatePrice）。
			Prices:  quoted.Prices,
			Price:   quoted.Prices["prompt"],
			Request: quoted.Request,
			Minimum: quoted.Minimum,
			Units:   quoted.Units,
			Rule:    ruleRef(ruleID.Int64, scope.String, priority.Int64, effectiveFrom, effectiveTo),
		})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("读候选计价规则失败: %w", err)
	}
	return out, nil
}

// firstLine 取错误文本的第一行。
//
// 表达式编译失败时，错误文本会附上出错那一行的源码与一个 ^ 标记；规则表达式
// 可能很长，整段打进日志既没有多出什么信息，也会把一行日志撑得没法读。第一行
// 已经写清了是哪一类错误、在第几列。
func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// rulePrices 是一条规则求出的整张价表：逐维度的千 token 价、每次请求的
// 一次性费用与计量项单价。
type rulePrices struct {
	Prices  map[string]money.Amount
	Request money.Amount
	Minimum money.Amount
	Units   map[string]money.Amount
}

// pricesOf 编译并求一条规则的整张价表。
//
// 逐维度单独求值，而不是求一次再挑字段：规则可以引用 usage（阶梯价、
// 按用量分支），用 1000 个 prompt 求出来的 completion 价未必等于用 1000 个
// completion 求出来的那一项。每个维度各造一份「只有这一维有 1000 个 token」
// 的用量，就是「这一千个 token 按结算会收多少」——与对拍用例的算法一致。
//
// 只返回规则显式写过的 token 维度：没写的维度在结算时会被 Validate 拒绝，
// 报价不能替它补一个 0。
//
// 一次性费用与计量项单价取自**第一份求值结果**（prompt 那一次）。它们与
// token 用量无关（量纲分别是「每请求」与「每计量单位」），为它们再求一遍
// 只会多出一次求值；而当规则真的把 fixed/minimum 写成用量的函数时，报出
// 来的就是参考用量（1000 个 prompt）下的那一档，与逐维度价用的是同一个用量。
func (o *Offers) pricesOf(ruleID int64, source string, at time.Time) (rulePrices, error) {
	var out rulePrices

	program, err := o.programs.Get(fmt.Sprintf("%d:%d", ruleID, versionOf(source)), source)
	if err != nil {
		return out, fmt.Errorf("编译规则 %d 失败: %w", ruleID, err)
	}

	prices := make(map[string]money.Amount, len(billing.TokenSlots))
	chargesTaken := false
	for _, slot := range billing.TokenSlots {
		usage := billing.Usage{}
		slot.SetCount(&usage, offerTokenCount)
		result, err := program.Eval(expr.Env{
			At:       at,
			Region:   o.region,
			Calendar: o.calendar,
			Usage:    usage,
		})
		if err != nil {
			return out, fmt.Errorf("求规则 %d 的报价失败: %w", ruleID, err)
		}
		if !chargesTaken {
			out.Request = result.Charges.Fixed
			out.Minimum = result.Charges.Minimum
			out.Units = result.Prices.Units
			chargesTaken = true
		}
		if !result.Prices.Declared[slot.Name] {
			continue
		}
		// 与结算同一个函数。用量里只有这一维非零，所以 Subtotal 恰好是
		// 这一维的金额；固定费与最低消费是「一次请求」的费用，不进这一维的
		// 金额——它们单独报（见 rulePrices 的 Request / Minimum）。
		breakdown := billing.ComputeWithCharges(usage, result.Prices, result.Charges)
		prices[slot.Name] = breakdown.Subtotal()
	}
	out.Prices = prices
	return out, nil
}

// ruleRef 把查询到的规则元信息折成对外形状。时刻字段只在有效时取出来，
// NULL（LEFT JOIN 没匹配到规则时的产物）不会走到这里。
func ruleRef(id int64, scope string, priority int64, from, to sql.NullTime) RuleRef {
	ref := RuleRef{ID: id, Scope: scope, Priority: int(priority), EffectiveFrom: from.Time}
	if to.Valid {
		value := to.Time
		ref.EffectiveTo = &value
	}
	return ref
}
