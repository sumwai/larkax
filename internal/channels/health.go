package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HealthUnknown 是 channel_quota_state.health 里「没有结论」那一档。
//
// 它同时是**一行都没有**时读侧的兜底取值：新渠道还没有额度状态行，而「没有行」
// 与「行上写着 unknown」在这里是同一件事——两者都说不出这条渠道好不好。
//
// 写入侧今天只有迁移与恢复动作写过这一列（自记账路径刻意不碰它，见 record.go 的
// quotaUpsertSQL），所以它在库里多半就是默认值。
const HealthUnknown = "unknown"

// ChannelHealth 是一条渠道在某个时间窗内的健康读数。
//
// 它是**只读**的一份投影：health 与最近一次探测时刻来自 channel_quota_state，
// 计数与分位数来自 channel_health_rollup 在这个窗口里的桶。本类型不做任何判定
// ——什么时候算 degraded、什么时候降级成 drained 是别处的规则，这里只把库里
// 已有的值读出来。
type ChannelHealth struct {
	ID   int64
	Name string

	// DeletedAt 非零表示这条渠道已经软删。默认清单里不出现它，--all 才带上。
	DeletedAt time.Time

	// Health 是 channel_quota_state.health 的取值（healthy / degraded /
	// drained / unknown 之一）。没有额度状态行时是 HealthUnknown。
	Health string

	// LastProbeAt 是最近一次额度探测的时刻，nil 表示从来没探测过。
	//
	// 「从来没探测过」与「探测过但没拿到结论」是两件事，而它们只看 Health 分不
	// 出来（后者也可能是 unknown）。两个字段合起来才答得出「最近一次探测是什么
	// 时候」——那正是这个字段存在的理由。
	LastProbeAt *time.Time

	// Requests / Successes / Failures 是窗口内小时桶的合计。
	//
	// 三者都从桶里加总，而 requests 计的是全部调用：「没成功也没失败」的那一次
	// （客户端断开）只进 requests（见 record.go 的 rollup）。所以成功数与失败数
	// 之和**可以小于**请求数，那不是缺数据。
	//
	// 桶里那一列 failures 到今天为止没有别的读方（选路按 successes / requests
	// 算成功率），这里是它的第一个读者——这个命令要答的正是「失败了几次」。
	Requests  int64
	Successes int64
	Failures  int64

	// CacheHitTokens / CacheMissTokens 是窗口内小时桶的合计，
	// 口径与选路读的那两列相同（见 channels.go 的 candidateQuery）：命中是
	// SUM(cache_hit_tokens)、未命中是 SUM(cache_miss_tokens)（后者算的是
	// prompt + cache_write，见 record.go 的 rollup）。
	//
	// 0 读作**窗口里没有样本**，与选路侧的读法一致（cacheHitRate 把 0 当「没有
	// 样本」）。0 在这里是「这一小时没有任何缓存命中」这个正常读数，不是「没人
	// 记过」，所以不做 NULLIF——它与 0 在本列上本来就同义，硬分成 null 与 0
	// 只会让调用方各判一次。
	CacheHitTokens  int64
	CacheMissTokens int64

	// LatencyP50ms / LatencyP95ms 各取窗口内**自己**最近一个有值的小时桶
	// （两列因此可能来自不同的小时），nil 表示那一列在窗口里没有任何样本。
	// 为什么不是整个窗口的分位数，见 healthFrom。
	LatencyP50ms *int
	LatencyP95ms *int

	// Cost 是窗口内小时桶的成本合计，十进制定点字符串；nil 表示没有数据。
	//
	// 判据见 healthFrom 里 NULLIF 那一处：NULL 表示窗口里没有一条带成本的事件，
	// 0 仍然按「没记过」处理（接上写入侧之前留下的旧桶）。把它当免费会让每一行
	// 都显示一个凭空的零成本，而那正是这一列最容易骗人的地方。
	Cost *string

	// ConsecutiveFailures 是选路维护的连续失败计数（channels 上的同名列）。
	//
	// 它与桶里的 failures 回答的不是同一个问题：桶计数「窗口里失败了几次」，
	// 这一列说的是「现在连着失败了几次」，成功一次就清零。供概览判断
	// 「这条渠道此刻是不是在连败」。
	ConsecutiveFailures int

	// LastProbeStatus 是最近一次额度探测的结论（ok / unsupported / error），
	// nil 表示没探测过。与 LastProbeAt 配对：一个说「什么时候」，一个说
	// 「那次怎么样」。
	LastProbeStatus *string

	// Accounts 是这条渠道凭证池里的账号，按账号 id 升序。
	//
	// 空表示这条渠道一个账号都没挂——选路会跳过它（见 accountpool.go 的
	// usablePool）。读数来自账号自己的状态行（account_state），不是渠道的：
	// 同一把 key 挂在两条渠道上时两处读数相同，那是对的——被上游拒绝的是
	// 这把 key，不是某一条渠道。
	Accounts []AccountHealth
}

// AccountHealth 是凭证池里一个账号在本渠道上的读数。
//
// 名字、权重与渠道有关（权重挂在关联上），健康与失败计数与渠道无关（挂在
// 账号上）。两半放在同一行是因为这条命令回答的是「这条渠道现在能用谁」，
// 运维看的是池子，不是两个分离的清单。
type AccountHealth struct {
	ID     int64
	Name   string
	Weight int

	// Health 是 account_state.health（healthy / degraded / drained / unknown）。
	// 没有状态行（这个账号还没被调用过）时是 AccountHealthUnknown。
	Health string
	// ConsecutiveFailures 是连续失败次数，成功一次清零。
	ConsecutiveFailures int
	// LastFailureAt 是最近一次失败的时刻，nil 表示没失败过。
	LastFailureAt *time.Time
	// Conclusion 是最近一次调用的结论（成功 / 调用失败 / 凭证被上游拒绝）。
	// 没有状态行时是空串。
	Conclusion string
}

// Usable 判断这个账号此刻参不参选。
//
// 判据与选路共用一份（见 accountUsable）：「命令行说可用、选路却跳过」这种
// 不一致不该由两处各写一遍来判断。
func (a AccountHealth) Usable(now time.Time) bool {
	return accountUsable(a.Weight, a.Health, a.LastFailureAt, now)
}

// 凭证池的整体状态，供 channel health 打一列「池子现在怎么样」。
const (
	// PoolStateEmpty：一个账号都没挂。选路跳过这条渠道。
	PoolStateEmpty = "empty"
	// PoolStateDrained：挂了账号，但此刻一个都不可用（全被摘了或权重全 0）。
	// 选路同样跳过——它与空池对流量是同一个后果，只是原因不同。
	PoolStateDrained = "drained"
	// PoolStateDegraded：还有可用的账号，但池子里有被摘掉的（或权重 0 的）。
	// 流量全落在剩下的那几个上，所以这是一条「还能用但已经在咬牙」的渠道。
	PoolStateDegraded = "degraded"
	// PoolStateHealthy：池子里每个账号都可用。
	PoolStateHealthy = "healthy"
)

// UsableAccounts 数出此刻可用的账号个数。
func (h ChannelHealth) UsableAccounts(now time.Time) int {
	n := 0
	for _, acc := range h.Accounts {
		if acc.Usable(now) {
			n++
		}
	}
	return n
}

// PoolState 给出凭证池的整体状态。
//
// **这是推算出来的，不是库里的一列**：它由「挂了几个」与「几个可用」决定，
// 而可用与否带冷却期（drained 的账号过一个 window 会重新参选），落库就等于
// 要么写一个会过期的值，要么每 15 分钟扫一遍全表。
//
// 「全挂」与「空池」分开报：两者对流量是同一个后果（选路跳过），补救动作
// 却完全不同——前者去换 key 或等冷却期，后者去挂账号。
func (h ChannelHealth) PoolState(now time.Time) string {
	if len(h.Accounts) == 0 {
		return PoolStateEmpty
	}
	usable := h.UsableAccounts(now)
	switch {
	case usable == 0:
		return PoolStateDrained
	case usable < len(h.Accounts):
		return PoolStateDegraded
	default:
		return PoolStateHealthy
	}
}

// FailureRate 给出窗口内的失败率，窗口里没有请求时返回 nil。
//
// 分母是 Requests 而不是成功数加失败数：「没成功也没失败」的那一次留在分母里
// （与选路的成功率口径一致）。把它摘出去会让失败率在客户端频繁断开的渠道上偏高。
//
// 返回 nil 是**「这个窗口里一次都没请求过」这个结论**，不是「出错了但没报」，
// 所以这里显式 nolint 而不是造一个哨兵错误让每个调用方多判一次。
func (h ChannelHealth) FailureRate() *float64 {
	if h.Requests <= 0 {
		return nil //nolint:nilnil // 见上
	}
	rate := float64(h.Failures) / float64(h.Requests)
	return &rate
}

// HealthBucket 是一条渠道在某一小时里的读数，供 `channel health --id` 打明细。
//
// 字段与 ChannelHealth 上那几个是同名的同一种口径，区别只在于它是**一个桶**而
// 不是窗口的合计。分位数在这里是桶自己的值，不做任何跨桶合并。
type HealthBucket struct {
	Bucket          time.Time
	Requests        int64
	Successes       int64
	Failures        int64
	CacheHitTokens  int64
	CacheMissTokens int64
	LatencyP50ms    *int
	LatencyP95ms    *int
	Cost            *string
}

// healthColumns 是清单与单条详情共用的取数表达式。
//
// 两处共用一份：分开写时，「窗口」在一条上写成 `bucket >= $1`、另一条写成
// `bucket > $1` 这类偏差不会有任何一步失败，只会让两条路对同一条渠道给出不同的数。
const healthColumns = `
    c.id,
    c.name,
    c.deleted_at,
    COALESCE(q.health, $2),
    q.last_probe_at,
    COALESCE(h.requests, 0),
    COALESCE(h.successes, 0),
    COALESCE(h.failures, 0),
    COALESCE(h.cache_hit_tokens, 0),
    COALESCE(h.cache_miss_tokens, 0),
    h.latency_p50_ms,
    h.latency_p95_ms,
    h.cost,
    c.consecutive_failures,
    q.last_probe_status,
    COALESCE(ac.accounts, '[]')`

// healthFrom 是健康读数的取数主体：channels 是主表，两个信号表按渠道左连。
//
// # 只读渠道级合并行
//
// 健康度桶带模型维度（迁移 00057），但这条命令回答的是「这个渠道」怎么样，
// 所以两个查询都过滤 model 为空串——那是写入侧每次调用维护的合并行。模型行
// 里也有计数与推导值（留给将来按模型看），加进来会把同一个数算两遍，逐小时
// 明细还会同一个桶出现多次。
//
// # 分位数为什么每列各取最近一个非空桶
//
// 分位数**不可累加**：把几个桶的 p95 求平均、或者对它们再取一次分位数，得到的
// 数不对应任何一个真实观测。要算出真正覆盖整个窗口的分位数，得有窗口内每次调用
// 的耗时——那在 usage_events 里，是第三张表，而这条命令的范围是「把库里已有的
// 那两处读出来」。
//
// 所以 p50 与 p95 各取**自己**在窗口内最近一个非空桶的值，两列因此可能来自不同
// 的小时。写入侧两列是同一个样本集算出来的（见 record.go 的 rollupSignalsSQL），
// 于是新写的桶里两列同时有值或同时为空——能差开的是这一列接上写入方之前留下的
// 旧桶，那儿 p95 有值而 p50 是 NULL。这与选路读路径
// （channels.go 的 candidateQuery）完全一致：那条路径取的也是
// ARRAY_AGG(... ORDER BY bucket DESC) FILTER (WHERE ... IS NOT NULL))[1]，对 p95
// 单独算。两处一致是有意的——命令行上看到的延迟，应当就是选路打分时用的那个数；
// 换一种算法会让「为什么它被打了低分」变成第二个需要解释的谜题。
//
// 代价要说清：汇总行里请求数是**整个窗口**的合计，而每一列的分位数只来自它自己
// 最近那一桶有样本的小时。时间跨度不同，`--id` 的逐小时明细正是为此而打的。
//
// # cost 为什么套 NULLIF
//
// 那一列可空（见迁移 00046）：写入侧按桶从 usage_events 的 upstream_cost 重算，
// 整桶没有一条带成本的事件时写 NULL。SUM 之后再见 NULL，就已经是「这个窗口里
// 没有成本数据」了——这一层不需要 NULLIF。
//
// NULLIF 挡的是 0：接上写入侧之前那一列是 NOT NULL DEFAULT 0，窗口里可能还留着
// 那时写下的 0（那些桶没有成本事件，也不会再被重算）。把它当免费会让它们显示
// 一个凭空的零成本。判据放在 SQL 里，Go 侧于是只需要认一种「没有数据」：NULL；
// 留在 Go 侧判的话，两处（清单与逐小时明细）各要判一次，漏掉一处就是一行零成本。
//
// 代价是：真的免费（规则算出 0）今天也无法与「没记过」区分开。这是那一列历史
// 遗留的边界，不是这里的选择。
//
// 缓存 token 那两列**不套** NULLIF：它们与成本同为 NOT NULL DEFAULT 0，但 0 是
// 「这一小时没有命中/没有未命中」这个正常读数（token 数为 0 的请求不存在），
// 不是「没人记过」。
const healthFrom = `
  FROM channels c
  LEFT JOIN LATERAL (
        SELECT
            SUM(r.requests)  AS requests,
            SUM(r.successes) AS successes,
            SUM(r.failures)  AS failures,
            SUM(r.cache_hit_tokens)  AS cache_hit_tokens,
            SUM(r.cache_miss_tokens) AS cache_miss_tokens,
            (ARRAY_AGG(r.latency_p50_ms ORDER BY r.bucket DESC)
                FILTER (WHERE r.latency_p50_ms IS NOT NULL))[1] AS latency_p50_ms,
            (ARRAY_AGG(r.latency_p95_ms ORDER BY r.bucket DESC)
                FILTER (WHERE r.latency_p95_ms IS NOT NULL))[1] AS latency_p95_ms,
            NULLIF(SUM(r.cost), 0) AS cost
          FROM channel_health_rollup r
         WHERE r.channel_id = c.id
           AND r.bucket >= $1
           -- 只读渠道级合并行：模型行里也有一份计数与推导值，加进来会把
           -- 同一个数算两遍（见迁移 00057 与 record.go 的 rollupSignalsSQL）。
           AND r.model = ''
       ) h ON true
  LEFT JOIN channel_quota_state q ON q.channel_id = c.id
  LEFT JOIN LATERAL (
        SELECT json_agg(json_build_object(
                   'id', a.id, 'name', a.name, 'weight', ca.weight,
                   'health', COALESCE(s.health, ''),
                   'failures', COALESCE(s.consecutive_failures, 0),
                   'last_failure_at', extract(epoch FROM s.last_failure_at),
                   'conclusion', COALESCE(s.last_conclusion, '')
               ) ORDER BY a.id)::text AS accounts
          FROM channel_accounts ca
          JOIN accounts a ON a.id = ca.account_id
          LEFT JOIN account_state s ON s.account_id = a.id
         WHERE ca.channel_id = c.id
       ) ac ON true`

// Health 读窗口内的渠道健康，按 id 排序。includeDeleted 为真时带上已软删的渠道。
func (a *Admin) Health(ctx context.Context, since time.Time, includeDeleted bool) ([]ChannelHealth, error) {
	return a.health(ctx, since, 0, includeDeleted, nil)
}

// HealthOwned 读一位用户名下（未软删）渠道的健康，按 id 排序。
//
// 与 Health 共用同一条查询：多出来的只有归属那一句条件。另写一份时，
// 「窗口」「分位数取最近非空桶」「成本 NULLIF」这些口径会各走各的。
func (a *Admin) HealthOwned(ctx context.Context, ownerID int64, since time.Time) ([]ChannelHealth, error) {
	if ownerID <= 0 {
		return nil, errors.New("用户 id 无效")
	}
	return a.health(ctx, since, 0, false, &ownerID)
}

// health 是清单与单条详情共用的那一遍查询。
//
// id 非 0 时只读那一条。已软删的渠道**照读**：`--all` 那个开关管的是「清单要不要
// 变长」，不是「单条能不能看」——行还在库里，单条读取的目的是核对配置，藏起来
// 反而让人打不开（与 channel get 同一条口径）。
//
// ownerID 非 nil 时只读那个用户名下的渠道（控制台 merchant 组）。
func (a *Admin) health(ctx context.Context, since time.Time, id int64,
	includeDeleted bool, ownerID *int64,
) ([]ChannelHealth, error) {
	args := []any{since, HealthUnknown}
	var conds []string
	if id != 0 {
		args = append(args, id)
		conds = append(conds, "c.id = $"+strconv.Itoa(len(args)))
	}
	if !includeDeleted {
		conds = append(conds, "c."+channelAlive)
	}
	if ownerID != nil {
		args = append(args, *ownerID)
		conds = append(conds, "c.owner_user_id = $"+strconv.Itoa(len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "\n WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := a.db.QueryContext(ctx,
		"SELECT"+healthColumns+healthFrom+where+"\n ORDER BY c.id", args...)
	if err != nil {
		return nil, fmt.Errorf("查询渠道健康失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：一条都没有是正常结果（还没配渠道），不是「没有值」。
	out := []ChannelHealth{}
	for rows.Next() {
		h, err := scanHealth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历渠道健康失败: %w", err)
	}
	return out, nil
}

// HealthOne 读一条渠道的健康与它在这个窗口里的逐小时明细。
//
// 渠道不存在时返回 ErrChannelNotFound；已软删的照读（调用方在输出里标出）。
// 明细按**时刻倒序**，最近的一小时在最上面：这条命令回答的是「它最近怎么了」。
func (a *Admin) HealthOne(ctx context.Context, id int64, since time.Time) (ChannelHealth, []HealthBucket, error) {
	if id <= 0 {
		return ChannelHealth{}, nil, errors.New("渠道 id 无效")
	}
	rows, err := a.health(ctx, since, id, true, nil)
	if err != nil {
		return ChannelHealth{}, nil, err
	}
	if len(rows) == 0 {
		return ChannelHealth{}, nil, fmt.Errorf("%w：id %d", ErrChannelNotFound, id)
	}
	buckets, err := a.healthBuckets(ctx, id, since)
	if err != nil {
		return ChannelHealth{}, nil, err
	}
	return rows[0], buckets, nil
}

// healthBucketsQuery 取一条渠道在窗口内的逐小时行。
//
// 投影与 healthFrom 里那个子查询同一套口径（同样的 NULLIF、同样不跨桶合并），
// 所以明细加起来与汇总行对得上——只有分位数例外，汇总取的是最近一个非空桶，
// 而明细是逐桶各自的值。
const healthBucketsQuery = `
SELECT bucket, requests, successes, failures,
       cache_hit_tokens, cache_miss_tokens,
       latency_p50_ms, latency_p95_ms, NULLIF(cost, 0)
  FROM channel_health_rollup
 WHERE channel_id = $1
   AND bucket >= $2
   -- 与 healthFrom 同一个口径：只读渠道级合并行，否则逐小时明细会把每个
   -- 模型各打一行（同一个桶出现多次，计数也跟着拆开）。
   AND model = ''
 ORDER BY bucket DESC`

func (a *Admin) healthBuckets(ctx context.Context, id int64, since time.Time) ([]HealthBucket, error) {
	rows, err := a.db.QueryContext(ctx, healthBucketsQuery, id, since)
	if err != nil {
		return nil, fmt.Errorf("查询渠道 %d 的逐小时健康失败: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	out := []HealthBucket{}
	for rows.Next() {
		var (
			b    HealthBucket
			p50  sql.NullInt64
			p95  sql.NullInt64
			cost sql.NullString
		)
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.Successes, &b.Failures,
			&b.CacheHitTokens, &b.CacheMissTokens, &p50, &p95, &cost); err != nil {
			return nil, fmt.Errorf("读取渠道 %d 的健康桶失败: %w", id, err)
		}
		b.LatencyP50ms = nullIntPtr(p50)
		b.LatencyP95ms = nullIntPtr(p95)
		b.Cost = nullStringPtr(cost)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历渠道 %d 的健康桶失败: %w", id, err)
	}
	return out, nil
}

func scanHealth(rows *sql.Rows) (ChannelHealth, error) {
	var (
		h            ChannelHealth
		deleted      sql.NullTime
		probeAt      sql.NullTime
		p50          sql.NullInt64
		p95          sql.NullInt64
		cost         sql.NullString
		probeText    sql.NullString
		accountsJSON string
	)
	err := rows.Scan(&h.ID, &h.Name, &deleted, &h.Health, &probeAt,
		&h.Requests, &h.Successes, &h.Failures,
		&h.CacheHitTokens, &h.CacheMissTokens, &p50, &p95, &cost,
		&h.ConsecutiveFailures, &probeText, &accountsJSON)
	if err != nil {
		return ChannelHealth{}, fmt.Errorf("读取渠道健康行失败: %w", err)
	}
	// NULL 折成零值时刻：Go 侧只问「删了没」，与 ChannelInfo.DeletedAt 同一条。
	h.DeletedAt = deleted.Time
	if probeAt.Valid {
		at := probeAt.Time
		h.LastProbeAt = &at
	}
	h.LatencyP50ms = nullIntPtr(p50)
	h.LatencyP95ms = nullIntPtr(p95)
	h.Cost = nullStringPtr(cost)
	h.LastProbeStatus = nullStringPtr(probeText)
	h.Accounts, err = decodeAccountHealth(h.ID, accountsJSON)
	if err != nil {
		return ChannelHealth{}, err
	}
	return h, nil
}

// accountHealthJSON 是 healthFrom 里 json_agg 出来的一个账号。
//
// 与候选查询那条（poolAccountJSON）分开：这里**没有凭证密文**，多的是最近一次
// 结论。同一条查询不该把它不解密也用不上的密文读出来。
type accountHealthJSON struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Weight      int      `json:"weight"`
	Health      string   `json:"health"`
	Failures    int      `json:"failures"`
	LastFailure *float64 `json:"last_failure_at"`
	Conclusion  string   `json:"conclusion"`
}

// decodeAccountHealth 解开 healthFrom 带出来的账号池 JSON。
//
// health 在 SQL 里是 COALESCE(s.health, ”)：空串表示这个账号没有状态行，
// 在这里折成 AccountHealthUnknown——健康那套词归 Go 侧的一份常量，SQL 里
// 不再出现第二份字面量。
func decodeAccountHealth(channelID int64, raw string) ([]AccountHealth, error) {
	var decoded []accountHealthJSON
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("渠道 %d 的账号状态不是合法的 JSON 数组: %w", channelID, err)
	}
	out := make([]AccountHealth, 0, len(decoded))
	for _, acc := range decoded {
		health := acc.Health
		if health == "" {
			health = AccountHealthUnknown
		}
		row := AccountHealth{
			ID:                  acc.ID,
			Name:                acc.Name,
			Weight:              acc.Weight,
			Health:              health,
			ConsecutiveFailures: acc.Failures,
			Conclusion:          acc.Conclusion,
		}
		if acc.LastFailure != nil {
			at := time.Unix(int64(*acc.LastFailure), 0)
			row.LastFailureAt = &at
		}
		out = append(out, row)
	}
	return out, nil
}

// nullIntPtr 把可空整数折成指针：NULL 是「没有样本」，与「样本是 0」分开。
//
// 合成一个函数是因为它有三处调用点（两个分位数、清单与明细各一遍）。零值接住
// NULL 会把「窗口里一个样本都没有」写成「延迟 0 毫秒」——那是这条命令最不该出的
// 一种错，因为它看起来像一条极快的渠道。
func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// nullStringPtr 把可空文本折成指针：NULL 是「没有数据」。
//
// 金额保持**数据库交给我们的那串定点十进制**，不过一遍 money.Parse：那一列是
// numeric(20, 8)，PG 的文本输出本来就是 8 位小数的定点串，正是机器可读那一档
// 要的形态；多一次解析只是多一处可能失败的地方，而失败时的处置（丢掉一个数？
// 报错？）没有一个说得通的答案。
func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
