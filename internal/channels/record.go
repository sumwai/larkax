package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// Recorder 把上游调用的结局写成渠道健康度与额度状态。
//
// # 为什么写入方在这里
//
// 读侧（Source）从 channels / channel_health_rollup / channel_quota_state
// 三张表取候选，写入方与它共用同一批列的解码约定——分开写的话，两边对
// 同一列的理解会各自漂移，而症状是「写进去的值读出来不对」。
//
// 它不 import gateway：那个包是入口层，接口由它声明、由本包用结构满足
// （见文件末尾的编译期断言）。
//
// # 一次调用写三处
//
//   - channels.consecutive_failures / last_failure_at：连续失败的真计数
//   - channel_health_rollup：小时桶的 requests / successes / failures
//   - channel_quota_state：自记账扣减（有基准、且单位对得上时）
//
// 三处都是按主键或唯一键定位的单行写，没有一个大事务把它们绑在一起：
// 健康度与额度是两个独立的信号，其中一处写不进去不该让另外两处也不写。
// 错误交给调用方（见 gateway 的 recordCall），由它决定怎么留痕。
//
// # 桶里的推导列
//
// 延迟、缓存与成本那五列是**推导值**，不是逐次累加的：它们由该桶的
// usage_events 聚合出来（见 rollupSignalsSQL）。读侧把「没有数据」映射成
// 「没有数据」，打分时那一维整个不参与，所以没流量就是未知，而不是很差。
//
// 成本与计数列的量级不同：counts 是逐次 +1，这里是**每次调用重算整桶**。
// 重算发生在结算落库之后（见 gateway.recordAttempts），所以这一次调用的
// 成本已经被算进桶里，不会落后一个请求。
type Recorder struct {
	db *sql.DB

	// selfAccountingStaleAfter 是实测值开始显得陈旧的时长，由装配层从
	// settings 的 quota.probe_interval_seconds 导出（见 quota.SelfAccountingStaleAfter）。
	//
	// 它决定自记账写回的 confidence：基准越旧，写回去的那个数越低。
	// 跟着配置走而不是在 quota 里写死，是因为「多久算旧」与「多久探测一次」
	// 本来就是同一个约定——写死的话，改探测间隔对这个数毫无影响，
	// 而那条链路看上去是通的。
	selfAccountingStaleAfter time.Duration
}

// NewRecorder 包装一个已打开的数据库句柄。
//
// staleAfter 是自记账的陈旧阈值，必须为正。做成必填参数而不是给一个默认值：
// 默认值会让「忘了给」在运行期表现为一个安静的行为改变（零值下每一次
// 自记账都被判成陈旧），而它在库上只体现为一个偏低的 confidence。
// 取值由装配层从 settings 导出并校验（见 internal/assembly）。
func NewRecorder(db *sql.DB, staleAfter time.Duration) *Recorder {
	return &Recorder{db: db, selfAccountingStaleAfter: staleAfter}
}

// 编译期断言：它要满足 gateway 对「调用结局写入方」的要求。
//
// 与 Source 那处同一个做法：内联接口而不是 import gateway——数据面
// 依赖入口层会把整条入口链路拖进本包（见 scripts/check-layering.sh）。
var _ interface {
	RecordCall(context.Context, route.CallOutcome) error
} = (*Recorder)(nil)

// RecordCall 记一次上游调用的结局。
//
// 渠道已经不存在时（调用进行到一半时管理面删了它）什么都不记，也不报错：
// 没有东西可记，而「渠道没了」不是故障。
func (r *Recorder) RecordCall(ctx context.Context, rec route.CallOutcome) error {
	reset, increment, err := streakChange(rec.Verdict)
	if err != nil {
		return err
	}

	// 连续失败先写：它是「别再往这个渠道发」这条保护的唯一来源，
	// 而后面两处失败时它已经落库了。
	//
	// 返回值是「这一行还在不在」。不在就到此为止——后面的 rollup 有外键，
	// 硬写会撞约束。
	alive, err := r.streak(ctx, rec, reset, increment)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}

	if err := r.rollup(ctx, rec); err != nil {
		return err
	}
	return r.selfAccount(ctx, rec)
}

// streakChange 把调用结论翻成对连续失败计数的处置。
//
// 三态在这里收成两个布尔值，SQL 里就不必再认识 success / failure 这些
// 字面量。把字面量写进 SQL 是个静默的坑：改一个 Go 常量名不会编译失败，
// 只会让那条语句悄悄走进「什么都不改」的分支，而「保护失效」在库里与
// 「没有失败」长得一模一样。
func streakChange(v route.Verdict) (reset, increment bool, err error) {
	switch v {
	case route.VerdictSuccess:
		return true, false, nil
	case route.VerdictFailure:
		return false, true, nil
	case route.VerdictUnattributed:
		// 客户端断开、请求本身有错：不是渠道的失败，也不打断已有的连续失败。
		return false, false, nil
	}
	return false, false, fmt.Errorf("未知的调用结论 %q", v)
}

// streakSQL 维护连续失败计数与它的时间戳。
//
// 加一在 SQL 里做而不是在 Go 里算好再写：同一渠道的两次并发调用若各自
// 读到一个旧值再加一，后写的那次会把前一次抹掉，于是「连续失败 3 次」
// 永远不会到——而它与「没有失败」在库里长得一样。一条语句里做 CASE，
// 行锁在语句执行期间持有，计数是对的。
const streakSQL = `
UPDATE channels
   SET consecutive_failures = CASE WHEN $2 THEN 0
                                   WHEN $3 THEN consecutive_failures + 1
                                   ELSE consecutive_failures END,
       last_failure_at = CASE WHEN $2 THEN NULL
                              WHEN $3 THEN $4
                              ELSE last_failure_at END
 WHERE id = $1 AND ` + channelAlive

// streak 更新连续失败计数，返回这一行是否还在。
//
// 已软删的行按「不在了」处理：它的计数不再有读者（选路看不见这条渠道），
// 而一行数字继续涨会让「它删除时是什么状态」这个事实在库里被改写。
// 返回 false 让 RecordCall 到此为止，后面的 rollup 也就不会写进去。
func (r *Recorder) streak(ctx context.Context, rec route.CallOutcome, reset, increment bool) (bool, error) {
	res, err := r.db.ExecContext(ctx, streakSQL, rec.ChannelID, reset, increment, rec.At)
	if err != nil {
		return false, fmt.Errorf("更新渠道 %d 的连续失败计数失败: %w", rec.ChannelID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取渠道 %d 的更新行数失败: %w", rec.ChannelID, err)
	}
	return n > 0, nil
}

// rollupSQL 把这次调用累加进它所属的小时桶。
//
// 用 ON CONFLICT 累加而不是先查后写：桶是**多写者**的（同一渠道的并发
// 请求会同时命中同一个桶），先查后写会丢掉并发的那些。
const rollupSQL = `
INSERT INTO channel_health_rollup (channel_id, bucket, requests, successes, failures)
VALUES ($1, $2, 1, $3, $4)
ON CONFLICT (channel_id, bucket) DO UPDATE
   SET requests  = channel_health_rollup.requests  + 1,
       successes = channel_health_rollup.successes + EXCLUDED.successes,
       failures  = channel_health_rollup.failures  + EXCLUDED.failures`

// rollupSignalsSQL 用一个桶的用量事件重算它那五列**推导值**。
//
// # 为什么是重算，不是累加
//
// 延迟要的是分位数，而分位数不是可累加的量：每次只有一个样本，把它写进
// p95 等于给一个错的数起了一个对的名字。所以样本留在 usage_events 里
// （duration_ms 一列），这里按桶把它们聚成分位数。缓存两列与成本用同一个
// 来源，于是四类数说的是同一批请求、同一个时刻窗口。
//
// 重算的代价说清楚：每记一次调用就扫一遍该渠道在当前小时里的用量行
// （走 usage_events_channel_idx）。低流量渠道可以忽略；高流量渠道就是
// 「每小时行数」这个量级的一次索引扫描，换来的是不用再加一张样本表、
// 也不用在读侧维护一个跨进程的聚合状态。
//
// 重算是**幂等**的，这带来一个有用的性质：上一条语句成功、这一条失败时，
// 写进去的是旧值，而该桶的下一次调用会把它们整体重算一遍——不会像累加
// 那样永久偏离。
//
// # 四个口径
//
//   - cache_hit_tokens = SUM(cache_read_tokens)：命中缓存、直接复用的输入。
//   - cache_miss_tokens = SUM(prompt_tokens + cache_write_tokens)：
//     **没被缓存服务到的输入总量**。usage_events.prompt_tokens 已经是不含
//     缓存的输入（见 billing.FromChannelUsage），缓存写入是这次新建的条目
//     ——它既不是命中，也不在 prompt_tokens 里，漏掉它会让命中率偏高。
//     两列相加正好是这次请求的输入总量。
//     注意不是「cache_write 就是 miss」：单价上的写与读是两件事，这里算的
//     是**服务方式**的比例——写入的 token 同样没有被缓存命中。
//   - latency_p50_ms = percentile_disc(0.5)、latency_p95_ms = percentile_disc(0.95)：
//     同一个样本集里的两个百分位，各取一个真实观测到的样本（列是 integer，
//     离散分位不会给出一个从未出现过的毫秒数）。两列由同一条语句、同一个
//     窗口算出，所以 p50 不可能大于 p95；同一桶里两列要么都有值、要么都是
//     NULL——分位数取的是同一批非空样本。
//     ordered-set 聚合会**忽略 NULL**，所以没有耗时的行不参与，也不会被
//     当成 0 毫秒把分位数往下拉。整桶都没有样本时结果是 NULL，写回去是
//     「没有数据」，与「一行都没写过」在读侧同一档。
//   - cost = SUM(upstream_cost)：这个桶里所有请求的上游成本合计。
//     不 COALESCE：整桶都没有成本时 SUM 是 NULL，写回去读作「这个桶没有
//     成本数据」，与「上游不要钱」（合计 0）分得开——那一列因此是可空的
//     （见迁移 00046）。写 0 会让每个没配成本规则的渠道拿到成本这一维的
//     满分，而那正是最容易超支的路径。
//     部分事件有成本、部分没有时 SUM 只加有值的那些：它按请求数（requests）
//     相除读成「平均每次请求成本」，覆盖面不全时会被摊低。这是单列能表达的
//     最小单位，不是这里的选择（见 costPerUnit）。
//
// 两个分位数都有人读：选路只看 p95（见 candidateQuery），而 `channel health`
// 把两列一起打出来——同一个窗口里，中位数与 95 分位分开看才知道这个渠道是
// 整体慢，还是只有少数几条在拖尾。
//
// # 桶的归属
//
// 用量行按 created_at（落库时刻）落桶，与计数列用的 rec.At（调用结束时刻）
// 不是同一个钟：结算发生在调用结束之后，通常同一小时，跨整点时会差一个桶。
// 没有更近的时刻可用——usage_events 上没有「调用结束」这一列（priced_at 是
// **受理**时刻，对长请求差得更远）。
const rollupSignalsSQL = `
UPDATE channel_health_rollup h
   SET cache_hit_tokens  = a.cache_hit,
       cache_miss_tokens = a.cache_miss,
       latency_p50_ms    = a.latency_p50_ms,
       latency_p95_ms    = a.latency_p95_ms,
       cost              = a.cost
  FROM (
        SELECT COALESCE(SUM(u.cache_read_tokens), 0) AS cache_hit,
               COALESCE(SUM(u.prompt_tokens + u.cache_write_tokens), 0) AS cache_miss,
               percentile_disc(0.5) WITHIN GROUP (ORDER BY u.duration_ms) AS latency_p50_ms,
               percentile_disc(0.95) WITHIN GROUP (ORDER BY u.duration_ms) AS latency_p95_ms,
               SUM(u.upstream_cost) AS cost
          FROM usage_events u
         WHERE u.channel_id = $1
           AND u.created_at >= $2
           AND u.created_at <  $3
       ) a
 WHERE h.channel_id = $1
   AND h.bucket = $2`

// rollup 把一次调用累加进小时桶，并按桶重算延迟、缓存与成本。
//
// 「没成功也没失败」（客户端断开）也计入 requests 而不计入另外两个：
// 分母要诚实，而成功率的分子不该为一件与渠道无关的事记一笔。
// 读侧对 successes = 0 的成功率给中性分，所以这不会把一个渠道打成最差。
//
// 两次写分两条语句：PostgreSQL 的数据修改 CTE 与外层语句用的是同一个快照，
// 外层 UPDATE 看不到同一语句里刚插入或更新的那一行（同一行改两次也不被
// 支持）。两条语句之间失败时计数已经落库、推导列留旧值，由该桶的下一次
// 调用重算纠正（见 rollupSignalsSQL）。
func (r *Recorder) rollup(ctx context.Context, rec route.CallOutcome) error {
	var success, failure int
	switch rec.Verdict {
	case route.VerdictSuccess:
		success = 1
	case route.VerdictFailure:
		failure = 1
	case route.VerdictUnattributed:
	}

	bucket := rec.At.UTC().Truncate(time.Hour)
	_, err := r.db.ExecContext(ctx, rollupSQL, rec.ChannelID, bucket, success, failure)
	if err != nil {
		// channel_health_rollup.channel_id 有指向 channels 的外键。渠道在
		// 「选出候选」与「记下这次调用」之间被删掉时，这里拿到的是 23503；
		// 兜底哨兵让调用方至少能把它与「库连不上」分开。
		return dberr.ConstraintFailure(fmt.Errorf("累加渠道 %d 的健康度桶失败: %w", rec.ChannelID, err))
	}

	if _, err := r.db.ExecContext(ctx, rollupSignalsSQL, rec.ChannelID, bucket, bucket.Add(time.Hour)); err != nil {
		return fmt.Errorf("重算渠道 %d 在 %s 这一桶的延迟、缓存与成本失败: %w", rec.ChannelID, bucket.Format(time.RFC3339), err)
	}
	return nil
}

// quotaStateSQL 读一个渠道当前的额度状态。
//
// FOR UPDATE 与下面的写在同一事务里：自记账是「读旧余量 → 减消耗 → 写回」，
// 两次并发调用各读到同一个旧值的话，后写的那次会把前一次的扣减抹掉。
const quotaStateSQL = `
SELECT remaining_estimate, COALESCE(unit, ''), confidence, probe_kind,
       last_probe_at, updated_at
  FROM channel_quota_state
 WHERE channel_id = $1
   FOR UPDATE`

// quotaUpsertSQL 写回自记账推进后的状态。
//
// 只动自记账会变的六列。health / resets_at / window_* / last_error_* 一概
// 不碰：它们来自别的来源（上游实测、滚动窗口、错误观测），本路径没有
// 关于它们的新信息，顺手覆盖等于把别人写的值抹掉。
const quotaUpsertSQL = `
INSERT INTO channel_quota_state
       (channel_id, remaining_estimate, unit, confidence, probe_kind, last_probe_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (channel_id) DO UPDATE
   SET remaining_estimate = EXCLUDED.remaining_estimate,
       unit               = EXCLUDED.unit,
       confidence         = EXCLUDED.confidence,
       probe_kind         = EXCLUDED.probe_kind,
       last_probe_at      = EXCLUDED.last_probe_at,
       updated_at         = EXCLUDED.updated_at`

// selfAccount 用这次调用的用量推进额度自记账。
//
// # 合并语义复用 internal/quota
//
// 「实测之后自记账仍然要扣减」「自记账的可信度按时长衰减」这些规则在
// quota.Merge / quota.Project 里已经写好并测过，这里只负责读旧状态、
// 调用它们、写回新状态。另写一套的话，两处的规则会各自漂移，而额度算错
// 是这套系统里最难发现的一类错。
//
// # 没有基准时不写
//
// 自记账是一次**推进**：以上一次已知的余量为基准减去消耗。基准未知
// （这一行不存在，或 remaining_estimate 为 NULL）时它推不出任何数——
// 写一行只有 NULL 的额度状态既改变不了读侧（Known 仍是 false），
// 又是一次没有信息量的写。基准来自上游实测或人工录入，两者都不在这条
// 路径上（见 docs/channel-health.md）。
//
// # 单位必须对得上
//
// 只有 unit = tokens 的渠道能扣：上游给的用量本身就是 token 数。其余单位
// （按元计、按次计）换不出来——把 token 数当成元数扣下去是在**改错一个数**，
// 而它看起来完全正常。这与 route 的 scoreQuota「跨单位不比」是同一条规则，
// 只是一个管读、一个管写。
//
// # 落库前先校验
//
// 观测进 Merge 之前过一遍 quota.Validate：余量是不是有限数、是不是负的、
// 观测时间有没有落在将来。这些错不会让程序崩，只会让额度慢慢偏掉，
// 而偏掉之后要很久才看得出来。校验失败就**拒绝这次写入并报错**——
// 静默丢弃会让「观测被丢掉」与「观测写进去了」在库上长得一样。
func (r *Recorder) selfAccount(ctx context.Context, rec route.CallOutcome) error {
	consumed := consumedTokens(rec.Usage)
	if consumed <= 0 {
		// 上游没给用量（失败的调用、或上游没报 usage）。扣零等于没扣，
		// 写一次「没有变化」的状态只是多一次写。
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启额度自记账事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, found, err := readQuotaState(ctx, tx, rec.ChannelID)
	if err != nil {
		return err
	}
	if !found || prev.Remaining == nil || prev.Unit != quota.UnitTokens {
		return nil
	}

	obs := quota.Project(prev, float64(consumed), rec.At)
	// 落库**之前**校验这次观测自不自洽。失败就拒绝这次写入并把原因报出去，
	// 不静默丢弃：观测被丢弃与观测写进去了在库上长得一样，而前者的后果是
	// 额度慢慢偏掉——那正是这道校验要防的事（见 internal/quota 的 Validate）。
	//
	// 基准用 time.Now() 而不是 rec.At：rec.At 就是这次观测自己的时刻，
	// 拿它当「现在」会让「观测时间在将来」那条判据恒真（自己比自己），
	// 而它要拦的正是「调用方传进来一个未来的时间戳」。
	if err := quota.Validate(obs, time.Now()); err != nil {
		return fmt.Errorf("渠道 %d 的自记账观测不自洽，拒绝写入: %w", rec.ChannelID, err)
	}

	next := quota.Merge(prev, obs, rec.At, r.selfAccountingStaleAfter)
	if next.Remaining == nil {
		// Project 在基准未知时返回一个没有数值的观测，Merge 于是也不动
		// Remaining。上面已经挡过一遍，这里是第二道：写一个 NULL 回去
		// 会把已有的余量抹掉。
		return nil
	}

	if err := writeQuotaState(ctx, tx, next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交渠道 %d 的额度自记账失败: %w", rec.ChannelID, err)
	}
	return nil
}

// readQuotaState 读一个渠道的额度状态。第二个返回值为 false 表示还没有行。
func readQuotaState(ctx context.Context, tx *sql.Tx, channelID int64) (quota.State, bool, error) {
	var (
		remaining  sql.NullString
		unit       string
		confidence string
		probeKind  string
		lastProbe  sql.NullTime
		updatedAt  time.Time
	)
	err := tx.QueryRowContext(ctx, quotaStateSQL, channelID).Scan(
		&remaining, &unit, &confidence, &probeKind, &lastProbe, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return quota.State{}, false, nil
	}
	if err != nil {
		return quota.State{}, false, fmt.Errorf("读取渠道 %d 的额度状态失败: %w", channelID, err)
	}

	state := quota.State{
		ChannelID: channelID,
		Unit:      unit,
		Kind:      quota.Kind(probeKind),
		Estimated: !isAPIQuery(probeKind),
		UpdatedAt: updatedAt,
	}
	if remaining.Valid {
		v, err := strconv.ParseFloat(remaining.String, 64)
		if err != nil {
			return quota.State{}, false, fmt.Errorf("渠道 %d 的 remaining_estimate 不是数字（%q）: %w",
				channelID, remaining.String, err)
		}
		state.Remaining = &v
	}
	if confidence != "" {
		v, err := strconv.ParseFloat(confidence, 64)
		if err != nil {
			return quota.State{}, false, fmt.Errorf("渠道 %d 的 confidence 不是数字（%q）: %w",
				channelID, confidence, err)
		}
		state.Confidence = v
	}
	if lastProbe.Valid {
		at := lastProbe.Time
		state.LastProbeAt = &at
	}
	return state, true, nil
}

// writeQuotaState 把推进后的状态写回去。
func writeQuotaState(ctx context.Context, tx *sql.Tx, s quota.State) error {
	// 单位为空写 NULL 而不是空字符串：读侧两者都当未知，但库里 NULL 表示
	// 「不知道按什么计」，空字符串表示「单位是空」——后者不是一件事。
	unit := sql.NullString{String: s.Unit, Valid: s.Unit != ""}
	var lastProbe sql.NullTime
	if s.LastProbeAt != nil {
		lastProbe = sql.NullTime{Time: *s.LastProbeAt, Valid: true}
	}

	_, err := tx.ExecContext(ctx, quotaUpsertSQL,
		s.ChannelID, *s.Remaining, unit, s.Confidence, string(s.Kind), lastProbe, s.UpdatedAt)
	if err != nil {
		// channel_quota_state.channel_id 有指向 channels 的外键：渠道在这条
		// 写回之前被删时，插入分支拿到 23503。ON CONFLICT 管的是主键重复，
		// 管不到「这个渠道已经不在了」。
		return dberr.ConstraintFailure(fmt.Errorf("写回渠道 %d 的额度状态失败: %w", s.ChannelID, err))
	}
	return nil
}

// consumedTokens 给出这次调用消耗的 token 总数。
//
// 口径与 internal/billing 的 FromChannelUsage + TotalTokens 一致：channel.Usage
// 的 PromptTokens **含**缓存读写，CompletionTokens 是否含推理由
// CompletionIncludesReasoning 声明——那一位由适配器按渠道 setting 填进来
// （见 channel.UsageCompletionIncludesReasoning），所以推理只有在**没被算进
// 输出**时才另加。两边算出来的是同一个总数（那里把缓存与推理拆开是为了按不同
// 单价计费，加起来就是这里要的数）。
func consumedTokens(u channel.Usage) int64 {
	total := u.PromptTokens + u.CompletionTokens
	if !u.CompletionIncludesReasoning {
		total += u.ReasoningTokens
	}
	return total
}

// isAPIQuery 判断一个 probe_kind 是不是上游实测。
//
// 与读侧的 candidateRow.withValue 共用同一个判据：只有 api_query 是实测，
// 其余（自记账、从错误推断）都是推算。它填的是 State.Estimated，读者是插件
// 契约（见 internal/plugin 的 RouteContextFrom）；路由打分**不看**这个标记，
// 它按 Confidence 加权（见 internal/route 的 scoreQuota）。
func isAPIQuery(probeKind string) bool {
	return probeKind == probeKindAPIQuery
}
