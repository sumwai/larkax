package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是控制台（internal/consoleapi）要的**聚合读**：用量汇总、序列、
// 分组、请求日志，以及用户钱包的分录清单。
//
// 它们与 usage_read.go 的关系是「同一批表、不同的问题」：那一份回答「这个
// 用户发过哪些请求」（逐行），这一份回答「花了多少、花在哪」（聚合）。聚合
// 的 SQL 不能放进 handler：金额求和的口径必须和账本、额度判定落在同一个包里，
// 否则同一笔钱会在两处各算一遍。
//
// # 金额怎么求和
//
// 库里是 numeric(20, 8)，Go 这一侧是 int64 定点数。求和一律先 `::text`
// 再 money.Parse，然后在 Go 里相加——不让驱动把 numeric 折成浮点，
// 也不在 SQL 里 `::float8` 取巧：那会在 8 位小数上留下一个对不上的尾数。

// UsageWindow 是一次用量查询的范围。
//
// 金额与 token 取 usage_events，按 created_at（它的分区键）筛；按 key 过滤
// 走 billing_requests，因为 usage_events 没有 api_key_id，而历史行补不上。
type UsageWindow struct {
	UserID int64
	// From 是起点（含），To 是终点（**不含**）。
	From time.Time
	To   time.Time
	// APIKeyID 为 nil 表示不限 key。
	APIKeyID *int64
	// Model 为空表示不限模型。
	Model string
}

// validate 检查窗口本身是否自洽。
//
// 只判「不是空的、起点在终点之前」：跨度上限与分桶上限由调用方按端点各自的
// 语义判（汇总不限跨度，序列才有点数上限），在库里再判一遍会让两处各有一个
// 边界，而这个边界正好是 366 天。
func (w UsageWindow) validate() error {
	if w.UserID <= 0 {
		return fmt.Errorf("%w: 用户 ID 必须是正整数，得到 %d", ErrInvalidQuery, w.UserID)
	}
	if w.From.IsZero() || w.To.IsZero() {
		return fmt.Errorf("%w: 起点与终点都必须给出", ErrInvalidQuery)
	}
	if !w.From.Before(w.To) {
		return fmt.Errorf("%w: 起点 %s 不早于终点 %s", ErrInvalidQuery,
			w.From.Format(time.RFC3339), w.To.Format(time.RFC3339))
	}
	return nil
}

// usageEventsCTE 构造「这个用户在窗口内的用量事件」这一段子查询。
//
// 所有聚合共用它：汇总、序列、分组回答的是同一批行上的三个问题，而 WHERE
// 写三遍时，改了一处另两处还是老样子——表现为「汇总与图表对不上」，且两边
// 各自的 SQL 看起来都对。
//
// param 由调用方给，占位符编号跟着它走。
func usageEventsCTE(w UsageWindow, param func(any) string) string {
	conds := []string{
		"e.user_id = " + param(w.UserID),
		"e.created_at >= " + param(w.From),
		"e.created_at < " + param(w.To),
	}
	if w.Model != "" {
		conds = append(conds, "e.canonical_model = "+param(w.Model))
	}
	if w.APIKeyID != nil {
		// 按 key 过滤必须先落到 billing_requests 上：usage_events 没有这一列，
		// 而且不能加——历史行补不上，加出来的列对旧数据永远是 NULL。
		//
		// 子查询里重复给了用户与时间窗：它走 billing_requests 自己的索引
		// (user_id, started_at)，而两张表的时间列不是同一个（created_at 是
		// 落库时刻，started_at 是受理时刻）。只按 request_id 关联、把时间
		// 条件留给外层，会让这个 IN 在大窗口下退化成全表扫。
		conds = append(conds, `e.request_id IN (
			SELECT b.request_id FROM billing_requests b
			 WHERE b.user_id = `+param(w.UserID)+`
			   AND b.api_key_id = `+param(*w.APIKeyID)+`
			   AND b.started_at >= `+param(w.From)+`
			   AND b.started_at < `+param(w.To)+`)`)
	}
	return `SELECT e.request_id, e.canonical_model, e.prompt_tokens,
	               e.completion_tokens, e.cache_read_tokens, e.cache_write_tokens,
	               e.reasoning_tokens, e.billing_path, e.cost,
	               e.plan_reserved_amount, e.created_at
	          FROM usage_events e
	         WHERE ` + strings.Join(conds, " AND ")
}

// usageTotalsColumns 是金额、额度包面值与五组 token 的求和列，几个聚合共用。
//
// 金额与面值分别占一列（在 SELECT 里排前两位），其余按 prompt / completion /
// cache_read / cache_write / reasoning 的固定次序——次序必须与扫描处逐字
// 对齐，错一格的表现是「token 数串位」，而它不会报错。
//
// 两列都包 COALESCE：空窗口里 SUM 返回 NULL，而这里的「没有额度包占用」与
// 「和为 0」是同一件事（这一列 NOT NULL、默认 0）。
const usageTotalsColumns = `COALESCE(SUM(cost), 0)::text,
	COALESCE(SUM(plan_reserved_amount), 0)::text,
	COALESCE(SUM(prompt_tokens), 0),
	COALESCE(SUM(completion_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(reasoning_tokens), 0)`

// UsageTotals 是一段时间里的总额。
type UsageTotals struct {
	// Cost 是这段用量实际收了多少：额度包覆盖的请求从 prepaid 科目扣，
	// 它在这一列是实收金额，不是 0。
	Cost money.Amount
	// PlanReservedAmount 是这段用量占着的额度包面值上界之和，与 Cost 并列。
	//
	// 两者不合并，也不相加——它们是同一笔消费的实收与上界，相加会把同一笔
	// 钱算两遍。
	PlanReservedAmount money.Amount
	Requests           int64
	// 五个 token 槽位，与 usage_events 同名同义。
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// UsagePathTotal 是计费路径的一档。
type UsagePathTotal struct {
	// Path 取 measured / estimated / fallback 之一。
	Path     string
	Cost     money.Amount
	Requests int64
}

// UsageSummary 是一次汇总查询的结果。
type UsageSummary struct {
	Totals UsageTotals
	// Paths 总是三档齐全，没有数据的那一档金额为 0——图表不该因为缺一档
	// 就少一条线。
	Paths []UsagePathTotal
}

// usagePaths 是 Summary 里 Paths 的固定次序：响应**总是**给三档，没有数据
// 的那一档金额为 0。
//
// 表上的 CHECK 今天只允许 measured 与 estimated（见迁移 00020）：fallback
// 没有产出点，所以这一档在库里永远不会出现。仍然把它列进来，是因为响应形状
// 由控制台的接口约定固定——少返回一档，读的人分不清「这一档没有」与「这个
// 部署还没有这一档」。
var usagePaths = []string{"measured", "estimated", "fallback"}

// UsageSummary 读一段时间里的总额与计费路径分布。
//
// requests 用 COUNT(DISTINCT request_id)。现在的结算路径保证一个请求只落一行
// （唯一调用点是 store.Save：它锁住 pending 账单行后置 settled，而
// billing_requests.request_id 是唯一键），但 usage_events 表上没有唯一约束——
// 历史形状与绕过结算路径的写入都可能多行。直接 count 行会偏大，而偏大的方向
// 恰好是「看起来请求更多、单笔更便宜」，所以这里仍按可能多行处理。
func (s *Store) UsageSummary(ctx context.Context, w UsageWindow) (UsageSummary, error) {
	if err := w.validate(); err != nil {
		return UsageSummary{}, err
	}

	var (
		args  []any
		param = func(v any) string {
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
	)
	cte := usageEventsCTE(w, param)

	var out UsageSummary
	var costText, planReservedText string
	err := s.db.QueryRowContext(ctx, `
		WITH ev AS (`+cte+`)
		SELECT `+usageTotalsColumns+`,
		       COUNT(DISTINCT request_id)
		  FROM ev`, args...,
	).Scan(&costText, &planReservedText, &out.Totals.PromptTokens,
		&out.Totals.CompletionTokens, &out.Totals.CacheReadTokens,
		&out.Totals.CacheWriteTokens, &out.Totals.ReasoningTokens, &out.Totals.Requests)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("汇总用量失败: %w", err)
	}
	out.Totals.Cost, err = money.Parse(costText)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("用量总额 %q 解析失败: %w", costText, err)
	}
	out.Totals.PlanReservedAmount, err = money.Parse(planReservedText)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("额度包面值合计 %q 解析失败: %w", planReservedText, err)
	}

	// 路径分布是第二条查询，而不是从上面那一行里拆出来：上面是整体，
	// 这里是按路径分组，两者是同一批行上的两个投影。合成一条要写
	// GROUPING SETS，而它会把这个文件里最简单的那个查询变成最难读的一个。
	rows, err := s.db.QueryContext(ctx, `
		WITH ev AS (`+cte+`)
		SELECT billing_path, COALESCE(SUM(cost), 0)::text, COUNT(DISTINCT request_id)
		  FROM ev GROUP BY billing_path`, args...)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("汇总计费路径失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	found := make(map[string]UsagePathTotal, len(usagePaths))
	for rows.Next() {
		var (
			path     string
			cost     string
			requests int64
		)
		if err := rows.Scan(&path, &cost, &requests); err != nil {
			return UsageSummary{}, fmt.Errorf("读计费路径失败: %w", err)
		}
		amount, err := money.Parse(cost)
		if err != nil {
			return UsageSummary{}, fmt.Errorf("计费路径 %s 的金额 %q 解析失败: %w", path, cost, err)
		}
		found[path] = UsagePathTotal{Path: path, Cost: amount, Requests: requests}
	}
	if err := rows.Err(); err != nil {
		return UsageSummary{}, fmt.Errorf("遍历计费路径失败: %w", err)
	}
	for _, path := range usagePaths {
		if total, ok := found[path]; ok {
			out.Paths = append(out.Paths, total)
			continue
		}
		out.Paths = append(out.Paths, UsagePathTotal{Path: path})
	}
	return out, nil
}

// UsagePoint 是序列上的一个桶。
type UsagePoint struct {
	// Start 是桶的起点（站点时区下的整点或整日）。
	Start time.Time
	UsageTotals
}

// 序列分桶的边界。
const (
	// UsageSeriesMaxDays 是日序列最多的点数。366 天覆盖一个闰年。
	UsageSeriesMaxDays = 366
	// UsageSeriesMaxHours 是小时序列最多的点数：31 天。
	//
	// 它不是 24×366：小时粒度的用途是「看最近几天」，把窗口拉到一年只会
	// 让前端画一张画不出来的图，而服务端要先扫完全部行。上限与日序列的
	// 上限是两件事，各有各的理由。
	UsageSeriesMaxHours = 744
)

// UsageSeries 按天或小时分桶，空桶补零。
//
// # 为什么在 Go 里补零
//
// SQL 只会返回有数据的桶。图表需要连续的横轴，而「补零」这一步放在前端
// 就意味着每个客户端各写一遍——时区、边界、四舍五入各有一份自己的理解。
// 放在这里，空桶与有数据的桶来自同一套时间计算。
//
// # 时区
//
// 分桶按 tz（站点时区），不是 UTC：用户看到的「今天花了多少」是当地的一天。
// 时刻在响应里仍是带偏移的 RFC3339，调用方不需要知道时区规则。
func (s *Store) UsageSeries(ctx context.Context, w UsageWindow, interval string, tz *time.Location) ([]UsagePoint, error) {
	if err := w.validate(); err != nil {
		return nil, err
	}
	if tz == nil {
		return nil, fmt.Errorf("%w: 分桶时区为空", ErrInvalidQuery)
	}
	unit, maxPoints := "day", UsageSeriesMaxDays
	switch interval {
	case "day":
	case "hour":
		unit, maxPoints = "hour", UsageSeriesMaxHours
	default:
		return nil, fmt.Errorf("%w: 分桶粒度 %q，只认 day、hour", ErrInvalidQuery, interval)
	}

	// 桶的起点与点数先在 Go 里算出来。它与下面 SQL 里的 date_trunc 用的是
	// 同一个时区与同一个单位；两边各算一遍是有意的——SQL 负责把行归进桶，
	// Go 负责把「没有行的桶」也画出来。
	starts := seriesStarts(w.From, w.To, unit, tz)
	if len(starts) == 0 {
		return []UsagePoint{}, nil
	}
	if len(starts) > maxPoints {
		return nil, fmt.Errorf("%w: %s 粒度的点数 %d 超过上限 %d，请缩小窗口",
			ErrInvalidQuery, interval, len(starts), maxPoints)
	}

	var (
		args  []any
		param = func(v any) string {
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
	)
	cte := usageEventsCTE(w, param)
	tzParam := param(tz.String())
	unitParam := param(unit)

	rows, err := s.db.QueryContext(ctx, `
		WITH ev AS (`+cte+`)
		SELECT date_trunc(`+unitParam+`, created_at AT TIME ZONE `+tzParam+`)
		           AT TIME ZONE `+tzParam+` AS bucket,
		       `+usageTotalsColumns+`,
		       COUNT(DISTINCT request_id)
		  FROM ev GROUP BY bucket`, args...)
	if err != nil {
		return nil, fmt.Errorf("按 %s 汇总用量失败: %w", interval, err)
	}
	defer func() { _ = rows.Close() }()

	byStart := make(map[int64]UsagePoint, len(starts))
	for rows.Next() {
		var (
			bucket           time.Time
			costText         string
			planReservedText string
			point            UsagePoint
		)
		if err := rows.Scan(&bucket, &costText, &planReservedText, &point.PromptTokens,
			&point.CompletionTokens, &point.CacheReadTokens,
			&point.CacheWriteTokens, &point.ReasoningTokens, &point.Requests); err != nil {
			return nil, fmt.Errorf("读用量桶失败: %w", err)
		}
		cost, err := money.Parse(costText)
		if err != nil {
			return nil, fmt.Errorf("用量桶 %s 的金额 %q 解析失败: %w",
				bucket.Format(time.RFC3339), costText, err)
		}
		point.Cost = cost
		planReserved, err := money.Parse(planReservedText)
		if err != nil {
			return nil, fmt.Errorf("用量桶 %s 的额度包面值 %q 解析失败: %w",
				bucket.Format(time.RFC3339), planReservedText, err)
		}
		point.PlanReservedAmount = planReserved
		point.Start = bucket.In(tz)
		byStart[bucket.UTC().Unix()] = point
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用量桶失败: %w", err)
	}

	out := make([]UsagePoint, 0, len(starts))
	for _, start := range starts {
		if point, ok := byStart[start.UTC().Unix()]; ok {
			out = append(out, point)
			continue
		}
		out = append(out, UsagePoint{Start: start})
	}
	return out, nil
}

// seriesStarts 列出 [from, to) 覆盖的每个桶的起点。
//
// 桶的边界按 tz 的日历走（短月、闰年、夏令时都由 time 包处理），而步进
// 用的是 AddDate/Add：直接加 24h 会在夏令时那天错一小时，而这一天的图
// 看起来仍然正常，只是少了一格。
func seriesStarts(from, to time.Time, unit string, tz *time.Location) []time.Time {
	start := from.In(tz)
	if unit == "day" {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, tz)
	} else {
		start = time.Date(start.Year(), start.Month(), start.Day(), start.Hour(), 0, 0, 0, tz)
	}
	to = to.In(tz)

	out := make([]time.Time, 0, 1)
	for start.Before(to) {
		out = append(out, start)
		if unit == "day" {
			start = start.AddDate(0, 0, 1)
		} else {
			start = start.Add(time.Hour)
		}
	}
	return out
}

// UsageGroup 是按模型或按 key 分组的一行。
type UsageGroup struct {
	// Model 在 group_by=model 时有值。
	Model string
	// KeyID 在 group_by=key 时有值；nil 表示这批事件没有关联到任何 key
	// （历史行，或 key 已被删除）。
	KeyID   *int64
	KeyName string
	UsageTotals
}

// UsageBreakdown 是一次分组查询的结果。
type UsageBreakdown struct {
	// Items 是金额最大的前 limit 项。
	Items []UsageGroup
	// Other 是其余项的和；没有剩余时是全零，而不是 nil。
	Other UsageGroup
}

// 分组展示的边界。
const (
	// UsageBreakdownDefaultLimit 是没给 limit 时取几组。
	UsageBreakdownDefaultLimit = 10
	// UsageBreakdownMaxLimit 是一次最多取几组。
	UsageBreakdownMaxLimit = 50
)

// UsageBreakdown 按模型或 key 分组，取金额最大的若干组，其余折进 Other。
//
// # 为什么先全读再在 Go 里截断
//
// 模型数与一个用户的 key 数都是几十的量级，而「其余折进 Other」需要知道
// 被截掉那些组的和——用 SQL 写成两段（前 N 一段、其余一段）要么查两遍，
// 要么用窗口函数包一层。这里读全部再切，口径只有一处：先按金额排，前 N 个
// 进 Items，剩下的加起来进 Other。组数真到需要分页的那天，改的是这个函数，
// 而不是每个调用点。
func (s *Store) UsageBreakdown(ctx context.Context, w UsageWindow, groupBy string, limit int) (UsageBreakdown, error) {
	if err := w.validate(); err != nil {
		return UsageBreakdown{}, err
	}
	if limit <= 0 {
		limit = UsageBreakdownDefaultLimit
	}
	if limit > UsageBreakdownMaxLimit {
		limit = UsageBreakdownMaxLimit
	}

	var (
		args  []any
		param = func(v any) string {
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
	)
	cte := usageEventsCTE(w, param)

	var query string
	switch groupBy {
	case "model":
		query = `
			WITH ev AS (` + cte + `)
			SELECT canonical_model, NULL::bigint, ''::text,
			       ` + usageTotalsColumns + `,
			       COUNT(DISTINCT request_id)
			  FROM ev GROUP BY canonical_model`
	case "key":
		// usage_events 没有 api_key_id（见 UsageWindow）。这里按 request_id
		// 关联回 billing_requests 取它，再左连 api_keys 取当前的名字：
		// key 已被删时 api_key_id 也随之级联，历史行本来就不在了。
		//
		// 关联只取「这个用户、这个窗口」内的计费行，与 CTE 同一组条件——
		// 只按 request_id 关联会让别人的请求混进来，而别人的金额出现在
		// 自己的账单里是最不该发生的一类错。
		query = `
			WITH ev AS (` + cte + `),
			br AS (
				SELECT b.request_id, b.api_key_id
				  FROM billing_requests b
				 WHERE b.user_id = ` + param(w.UserID) + `
				   AND b.started_at >= ` + param(w.From) + `
				   AND b.started_at < ` + param(w.To) + `)
			SELECT '', b.api_key_id, COALESCE(k.name, ''),
			       ` + usageTotalsColumns + `,
			       COUNT(DISTINCT e.request_id)
			  FROM ev e
			  JOIN br b ON b.request_id = e.request_id
			  LEFT JOIN api_keys k ON k.id = b.api_key_id
			 GROUP BY b.api_key_id, k.name`
	default:
		return UsageBreakdown{}, fmt.Errorf("%w: 分组方式 %q，只认 model、key", ErrInvalidQuery, groupBy)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return UsageBreakdown{}, fmt.Errorf("按 %s 分组用量失败: %w", groupBy, err)
	}
	defer func() { _ = rows.Close() }()

	var groups []UsageGroup
	for rows.Next() {
		var (
			group            UsageGroup
			keyID            sql.NullInt64
			costText         string
			planReservedText string
		)
		if err := rows.Scan(&group.Model, &keyID, &group.KeyName, &costText,
			&planReservedText, &group.PromptTokens, &group.CompletionTokens,
			&group.CacheReadTokens, &group.CacheWriteTokens, &group.ReasoningTokens,
			&group.Requests); err != nil {
			return UsageBreakdown{}, fmt.Errorf("读用量分组失败: %w", err)
		}
		cost, err := money.Parse(costText)
		if err != nil {
			return UsageBreakdown{}, fmt.Errorf("用量分组的金额 %q 解析失败: %w", costText, err)
		}
		group.Cost = cost
		planReserved, err := money.Parse(planReservedText)
		if err != nil {
			return UsageBreakdown{}, fmt.Errorf("用量分组的额度包面值 %q 解析失败: %w",
				planReservedText, err)
		}
		group.PlanReservedAmount = planReserved
		if keyID.Valid {
			id := keyID.Int64
			group.KeyID = &id
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return UsageBreakdown{}, fmt.Errorf("遍历用量分组失败: %w", err)
	}

	sortGroupsByCost(groups)
	var out UsageBreakdown
	if len(groups) > limit {
		other, err := sumGroups(groups[limit:])
		if err != nil {
			return UsageBreakdown{}, err
		}
		out.Items = groups[:limit]
		out.Other = other
	} else {
		out.Items = groups
	}
	if out.Items == nil {
		out.Items = []UsageGroup{}
	}
	return out, nil
}

// sortGroupsByCost 按金额降序排，金额相同时按展示键排。
//
// 金额相同时必须有一个稳定的次序：没有它，同一份数据两次查询给出的前 10
// 可能不同，而「其他」那一项也会跟着变——界面看起来像在跳。
func sortGroupsByCost(groups []UsageGroup) {
	keyOf := func(g UsageGroup) string {
		if g.Model != "" {
			return g.Model
		}
		if g.KeyID != nil {
			return strconv.FormatInt(*g.KeyID, 10)
		}
		return ""
	}
	// 手写插入排序不值当：组数只有几十，而 sort.Slice 的稳定性由比较器里
	// 的第二键保证（金额相等时比键）。
	sort.Slice(groups, func(i, j int) bool {
		if c := groups[i].Cost.Cmp(groups[j].Cost); c != 0 {
			return c > 0
		}
		return keyOf(groups[i]) < keyOf(groups[j])
	})
}

// sumGroups 把若干组合并成一组（用于「其他」）。
//
// 求和走 money.SumChecked：库里的金额列比 int64 宽，一组分组之和真的可能
// 放不下。放不下时报错而不是交一个夹住的值——夹住的「其他」比真实和小，
// 而偏小看起来像「少算了钱」。额度包面值与 cost 同样处理：两者是两个口径，
// 各自都要放得下。
func sumGroups(groups []UsageGroup) (UsageGroup, error) {
	var out UsageGroup
	costs := make([]money.Amount, 0, len(groups))
	planReserved := make([]money.Amount, 0, len(groups))
	for _, g := range groups {
		costs = append(costs, g.Cost)
		planReserved = append(planReserved, g.PlanReservedAmount)
		out.PromptTokens += g.PromptTokens
		out.CompletionTokens += g.CompletionTokens
		out.CacheReadTokens += g.CacheReadTokens
		out.CacheWriteTokens += g.CacheWriteTokens
		out.ReasoningTokens += g.ReasoningTokens
		out.Requests += g.Requests
	}
	total, ok := money.SumChecked(costs...)
	if !ok {
		return UsageGroup{}, fmt.Errorf("%w: 其余 %d 组的金额之和超出可表示范围",
			money.ErrOverflow, len(groups))
	}
	out.Cost = total
	planTotal, ok := money.SumChecked(planReserved...)
	if !ok {
		return UsageGroup{}, fmt.Errorf("%w: 其余 %d 组的额度包面值之和超出可表示范围",
			money.ErrOverflow, len(groups))
	}
	out.PlanReservedAmount = planTotal
	return out, nil
}
