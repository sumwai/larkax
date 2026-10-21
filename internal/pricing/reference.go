package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
)

// 本文件是控制台上的**参考价**：平台规则在当前时刻、按每百万 token 求值。
//
// # 为什么叫参考价
//
// 规则可以是分时的、分级的、按渠道覆盖的。同一个模型此刻在平台规则下是
// 这个价，而一次真实请求最终按它命中的那条规则结算——可能更贵，也可能
// 更便宜。所以这里给的是「平台兜底价」这一个切面，响应里必须带 reference_at
// 标明它是哪一刻的，界面要写「参考」。
//
// 没有平台规则时返回 nil（界面显示「—」），**不伪造一个起价**：一个编出来的
// 数会在真实结算按渠道价算时变成一句错话。
//
// # 为什么求两次
//
// 规则的结果是一组单价，而不是「输入价 / 输出价」两个字段。要回答「一百万个
// 输入 token 多少钱」，就让用量只有一个输入 token 的百万倍；输出同理。两次
// 求值都用同一份规则与同一个时刻，于是两个数出自同一条分支，不会出现
// 「输入按高峰、输出按低谷」这种拼出来的价。

// ReferencePriceTTL 是参考价的缓存时长。
//
// 模型页是公开的，刷新一下就会重算；而规则的生效粒度是分钟级，60 秒的缓存
// 让「多个人同时打开页面」不会变成 N 次规则查询。它是常量而不是配置：
// 改成可配只会多一个没人调的旋钮。
const ReferencePriceTTL = 60 * time.Second

// referenceTokenCount 是求值用的参考用量：一百万个 token。
//
// 选 10^6 是因为响应按「每百万 token」展示，乘出来的数就是单价本身，
// 不必再做一次除法——除法是单价的舍入点。
const referenceTokenCount = 1_000_000

// ReferencePrice 是一个模型的参考价，单位是每百万 token。
//
// 四个字段里除了输入与输出，其余可为 nil：规则里没声明的那一项是「没配」，
// 不是 0。0 是「免费」，两者在界面上与账目上都是两件事。
type ReferencePrice struct {
	InputPerMTok      money.Amount
	OutputPerMTok     money.Amount
	CacheReadPerMTok  *money.Amount
	CacheWritePerMTok *money.Amount
}

// References 求平台规则下的参考价。
//
// 它持有编译缓存与一份 60 秒的短缓存。与 Source 分开是因为依赖不同：Source
// 只查库；这里还要日历（规则可能用 isPeak / isWorkday）与求值时刻。
type References struct {
	db       *sql.DB
	region   string
	calendar expr.Calendar
	programs *expr.Cache
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]referenceCacheEntry
}

// referenceCacheEntry 是一条缓存下来的参考价。
type referenceCacheEntry struct {
	price    *ReferencePrice
	cachedAt time.Time
}

// NewReferences 装配参考价的数据源。
//
// calendar 可以为 nil：不用日历的规则照样求值；用了日历而这里为 nil 时，
// 求值会报 ErrCalendarRequired，那个模型的参考价于是取不到——比按 UTC 的
// 钟点算一个错价要好。
func NewReferences(db *sql.DB, region string, calendar expr.Calendar) *References {
	return &References{
		db:       db,
		region:   region,
		calendar: calendar,
		programs: expr.NewCache(referenceCacheSize),
		now:      time.Now,
		cache:    make(map[string]referenceCacheEntry),
	}
}

// referenceCacheSize 是编译缓存的容量。平台规则通常只有个位数，
// 给 64 足够覆盖「每个模型一条」的常见情况。
const referenceCacheSize = 64

// ReferencePrices 取一组模型的参考价。
//
// at 为零值时用当前时刻。模型没有平台规则、或规则里没写过这两个单价、
// 或命中了一条取不出价的平台规则时，该模型的值为 nil——调用方原样映射成
// JSON 的 null。一条规则写坏只影响命中它的模型：某个模型配错一条平台规则，
// 不该让整页模型清单都打不开。查询本身失败（连库、扫描）仍然是错误。
//
// 返回的 map 只含有值的模型；调用方按请求的模型清单查，缺的就是 nil。
func (r *References) ReferencePrices(ctx context.Context, models []string, at time.Time) (map[string]*ReferencePrice, error) {
	if len(models) == 0 {
		return map[string]*ReferencePrice{}, nil
	}
	if at.IsZero() {
		at = r.now().UTC()
	}
	return collectPrices(ctx, models, at, r.priceOf)
}

// rulePriceError 是「这条规则本身取不出价」：编译不过、求值失败，或换算到
// 每百万 token 时溢出。这类失败只让那个模型的参考价为 nil；查询失败不属于
// 这一类，它仍让整次取价失败。
type rulePriceError struct {
	ruleID int64
	err    error
}

func (e *rulePriceError) Error() string { return e.err.Error() }
func (e *rulePriceError) Unwrap() error { return e.err }

// collectPrices 按模型逐个取价，并把两类失败分开处置。
//
// priceOf 作为参数传进来，是为了让这条分界（规则写坏只跳过那个模型、查询
// 失败整次报错）能在不连库的情况下被钉住。
func collectPrices(ctx context.Context, models []string, at time.Time,
	priceOf func(context.Context, string, time.Time) (*ReferencePrice, error)) (map[string]*ReferencePrice, error) {
	out := make(map[string]*ReferencePrice, len(models))
	for _, model := range models {
		price, err := priceOf(ctx, model, at)
		if err != nil {
			var bad *rulePriceError
			if !errors.As(err, &bad) {
				return nil, err
			}
			// 表达式编译失败时，错误文本会附上出错那一行的源码与一个 ^
			// 标记；规则表达式可能很长，只把第一行打进日志——它已经写清
			// 了是哪一类错误、在第几列。
			reason, _, _ := strings.Cut(bad.err.Error(), "\n")
			observe.FromContext(ctx).Warn("规则取价失败，跳过该模型的参考价",
				"model", model,
				"rule_id", bad.ruleID,
				"reason", reason)
			continue
		}
		if price != nil {
			out[model] = price
		}
	}
	return out, nil
}

// priceOf 取一个模型的参考价，带 60 秒缓存。
func (r *References) priceOf(ctx context.Context, model string, at time.Time) (*ReferencePrice, error) {
	now := r.now()
	r.mu.Lock()
	entry, ok := r.cache[model]
	r.mu.Unlock()
	if ok && now.Sub(entry.cachedAt) < ReferencePriceTTL {
		return entry.price, nil
	}

	price, err := r.compute(ctx, model, at)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	// 缓存大小没有上限的风险：模型数由模型目录决定，而目录是运维维护的。
	// 真到了需要淘汰的规模，这里换成按条目数截断即可，与 expr.Cache 同一种做法。
	r.cache[model] = referenceCacheEntry{price: price, cachedAt: now}
	r.mu.Unlock()
	return price, nil
}

// compute 查平台规则并求值。没有平台规则时返回 nil（不是错误）。
func (r *References) compute(ctx context.Context, model string, at time.Time) (*ReferencePrice, error) {
	if model == "" {
		return nil, errors.New("模型名为空")
	}

	var (
		id     int64
		source string
	)
	// 平台规则的判据与 Source.Match 逐字相同，只多了 scope = 'platform'：
	// 平台价的定义就是「不依赖任何渠道的那一条」。owner 为 0 / channel 为 0
	// 在 ruleApplicable 里匹配不到商家级与渠道级规则，所以这里只需要再限定
	// scope 一个词——写出第二份「什么算平台价」的判据，正是它开始分岔的地方。
	err := r.db.QueryRowContext(ctx, `
		SELECT r.id, r.expr
		  FROM pricing_rules r
		 WHERE r.enabled
		   AND `+applicableWindowAndModel("$2")+`
		   AND r.scope = 'platform'
		 ORDER BY `+ruleOrderBy+`
		 LIMIT 1`, at, model).Scan(&id, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // nil 表示「没有平台价」，是合法结果
	}
	if err != nil {
		return nil, fmt.Errorf("查询模型 %q 的平台计价规则失败: %w", model, err)
	}

	program, err := r.programs.Get(fmt.Sprintf("%d:%d", id, versionOf(source)), source)
	if err != nil {
		return nil, &rulePriceError{ruleID: id,
			err: fmt.Errorf("编译模型 %q 的平台计价规则失败: %w", model, err)}
	}
	env := expr.Env{
		At:       at,
		Region:   r.region,
		Calendar: r.calendar,
	}

	// 输入侧与输出侧各求一次。两次用的是同一条规则、同一个时刻，所以
	// 「输入按高峰、输出按低谷」这种拼出来的价不会出现。
	env.Usage = billing.Usage{PromptTokens: referenceTokenCount}
	inputResult, err := program.Eval(env)
	if err != nil {
		return nil, &rulePriceError{ruleID: id,
			err: fmt.Errorf("求模型 %q 的输入价失败: %w", model, err)}
	}
	env.Usage = billing.Usage{CompletionTokens: referenceTokenCount}
	outputResult, err := program.Eval(env)
	if err != nil {
		return nil, &rulePriceError{ruleID: id,
			err: fmt.Errorf("求模型 %q 的输出价失败: %w", model, err)}
	}

	price := &ReferencePrice{}
	if price.InputPerMTok, err = perMillion(inputResult.Prices.Prompt); err != nil {
		return nil, &rulePriceError{ruleID: id, err: fmt.Errorf("模型 %q 的输入价: %w", model, err)}
	}
	if price.OutputPerMTok, err = perMillion(outputResult.Prices.Completion); err != nil {
		return nil, &rulePriceError{ruleID: id, err: fmt.Errorf("模型 %q 的输出价: %w", model, err)}
	}
	// 缓存两项从输入侧那一份结果里取：缓存读写都属于输入侧，而规则里
	// 给它们定价的通常是同一条分支。规则没声明的那一项保持 nil。
	if inputResult.Prices.Declared["cache_read"] {
		amount, err := perMillion(inputResult.Prices.CacheRead)
		if err != nil {
			return nil, &rulePriceError{ruleID: id, err: fmt.Errorf("模型 %q 的缓存读价: %w", model, err)}
		}
		price.CacheReadPerMTok = &amount
	}
	if inputResult.Prices.Declared["cache_write"] {
		amount, err := perMillion(inputResult.Prices.CacheWrite)
		if err != nil {
			return nil, &rulePriceError{ruleID: id, err: fmt.Errorf("模型 %q 的缓存写价: %w", model, err)}
		}
		price.CacheWritePerMTok = &amount
	}
	return price, nil
}

// perMillion 把「每个 token 的单价」换算成「每百万 token 的金额」。
//
// 乘而不是除：除法是单价的舍入点，而这里要的正是「一百万个 token 时
// 算出来多少」这个乘积。溢出时报错，不夹住——夹住的价看起来像一个正常的
// 高价，而不是一个错。
func perMillion(price money.Amount) (money.Amount, error) {
	amount, ok := money.MulIntChecked(price, referenceTokenCount)
	if !ok {
		return money.Zero, fmt.Errorf("%w: 单价 %s × %d 超出可表示范围",
			money.ErrOverflow, price, referenceTokenCount)
	}
	return amount, nil
}
