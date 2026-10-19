package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// QuotaProbe 是额度探测的**写入侧**：发现某个渠道的探测结果过期时，异步向上游
// 问一次，把结果写回 channel_quota_state。
//
// # 触发方式是用到时，不是定时轮询
//
// 它在路由真的把一个渠道当作候选来尝试时被调用（见 route.Router.OnAttempt）。
// 没有独立的采集进程：一条渠道长期没有流量，它的额度状态就停在原处，这不会
// 影响任何请求——下一次有人用到它时才会去问。
//
// # 请求不等探测结果
//
// ProbeIfStale 只做三件很轻的事：看时间戳、登记这次探测、起一个 goroutine，
// 然后立刻返回。这次请求用的是**已有的**额度快照，探测结果留给下一轮请求读。
// 探测失败只记日志与状态，不改变任何一次请求的成败。
//
// # 每个渠道同一时刻只有一次探测
//
// 过期的那一刻往往不止一条请求命中同一个渠道。单飞（single-flight）保证只问
// 上游一次，其余请求直接跳过。判定与登记在同一把锁里完成，所以并发的「都发现
// 过期」不会各自起一个 goroutine。
//
// 另外，本进程内成功发起过一次探测之后，TTL 内即使调用方拿的是**探测之前**
// 取到的候选快照，也不会再探：lastAttempt 里记着这个进程真正发起探测的时刻。
// 少了这一半，一条慢请求携带的旧快照会让刚探过的渠道马上被再探一次。
type QuotaProbe struct {
	db *sql.DB

	// adapters 按渠道类型查询上游额度。生产里是 *upstream.Executor。
	adapters QuotaAdapter

	// interval 是同一个渠道两次探测之间的最小间隔，来自 settings 的
	// quota.probe_interval_seconds。
	interval time.Duration
	// staleAfter 是实测值开始显得陈旧的时长，由探测间隔导出（见
	// quota.SelfAccountingStaleAfter）。它只传给 quota.Merge：实测值那一条
	// 分支今天不读它，但合并规则归那个包管，这里不自己判一遍。
	staleAfter time.Duration

	// Now 可注入，便于测试与请求路径共用同一个时钟。
	Now func() time.Time
	// Timeout 是一次探测的上限。零值时用 probeTimeout。
	//
	// 做成字段是为了让测试把超时压到毫秒级，覆盖「查询恰好超时、写回仍要
	// 成功」那个边界；把 probeTimeout 做成变量会让所有实例共享同一个值。
	Timeout time.Duration
	// Logger 记录探测失败与写库失败。为 nil 时用 slog.Default()。
	Logger *slog.Logger

	mu sync.Mutex
	// inflight 是正在探测的渠道。值为 true 表示已有一次探测在路上。
	inflight map[int64]bool
	// lastAttempt 是本进程最近一次**发起**探测的时刻，按渠道记。
	lastAttempt map[int64]time.Time
	// wg 让测试能等异步探测落库。生产代码不读它。
	wg sync.WaitGroup
}

// QuotaAdapter 按渠道向上游查询额度。
//
// 声明在本包、由 internal/upstream 的 Executor 满足：本包不认识适配器注册表，
// 只要求「给我这个渠道的额度」。契约这么窄，测试才能拿一个假适配器把写入侧
// 单独测掉，而不接真实上游。
type QuotaAdapter interface {
	Quota(ctx context.Context, ch channel.Channel) (quota.State, error)
}

// 一次探测的结论，取值与 channel_quota_state.last_probe_status 的 CHECK 一致。
const (
	// probeStatusOK 表示拿到了实测值。
	probeStatusOK = "ok"
	// probeStatusUnsupported 表示这家上游没有额度查询接口。它是正常结果，
	// 不是失败：多数订阅制上游都是这一档。
	probeStatusUnsupported = "unsupported"
	// probeStatusError 表示探测本身失败了（网络、鉴权、上游报错）。此时
	// 上一次的有效值原样保留。
	probeStatusError = "error"
)

// probeTimeout 是一次探测的上限。
//
// 探测不在请求路径上等，但也不能永远挂着：一个卡住的上游会让那个渠道的
// inflight 标记一直不释放，于是这个渠道在本进程寿命内再也探不了。取两分钟
// ——比一次普通的额度查询宽得多，又远短于一个进程的寿命。
const probeTimeout = 2 * time.Minute

// probeWriteTimeout 是写回一次探测结论的上限。
//
// 它与查询的超时分开：查询恰好在 deadline 上返回时，那个 context 已经结束，
// 复用它写库会立刻失败——于是「三种结果都会更新 last_probe_attempt_at」在
// 最需要成立的时刻不成立，而那个渠道会带着旧时刻被反复探测。写回只是一次
// 带主键的 upsert，五秒足够。
const probeWriteTimeout = 5 * time.Second

// NewQuotaProbe 构造写入侧。
//
// interval 必须为正：零值不是「不探测」，而是「每一条命中的请求都探测」，
// 两种行为在配置里长得一样。适配器为 nil 时直接失败，而不是等到第一次
// 探测才发现——那时它只表现为一次日志。
func NewQuotaProbe(db *sql.DB, adapters QuotaAdapter, interval time.Duration) (*QuotaProbe, error) {
	if db == nil {
		return nil, errors.New("构造额度探测：缺少数据库句柄")
	}
	if adapters == nil {
		return nil, errors.New("构造额度探测：缺少额度查询适配器")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("额度探测间隔必须为正，得到 %v", interval)
	}
	staleAfter, err := quota.SelfAccountingStaleAfter(interval)
	if err != nil {
		return nil, fmt.Errorf("由探测间隔导出陈旧阈值失败: %w", err)
	}
	return &QuotaProbe{
		db:          db,
		adapters:    adapters,
		interval:    interval,
		staleAfter:  staleAfter,
		inflight:    make(map[int64]bool),
		lastAttempt: make(map[int64]time.Time),
	}, nil
}

// ProbeIfStale 在候选的探测结果过期时异步发起一次探测，并立刻返回。
//
// 它**必须在请求路径上足够轻**：这里只碰内存，不查库也不发 HTTP。判过期的
// 依据是候选上带的两列（last_probe_attempt_at）与本进程记下的发起时刻。
//
// 同一个渠道已有探测在路上时直接返回——并发命中过期渠道的 N 条请求只会
// 看到一次上游调用。
func (p *QuotaProbe) ProbeIfStale(c route.Candidate) {
	id := c.Channel.ID
	if id == 0 {
		return
	}

	now := p.now()

	p.mu.Lock()
	if !p.dueLocked(id, c.Quota.LastProbeAttemptAt, now) {
		p.mu.Unlock()
		return
	}
	if p.inflight[id] {
		p.mu.Unlock()
		return
	}
	p.inflight[id] = true
	p.lastAttempt[id] = now
	p.wg.Add(1)
	p.mu.Unlock()

	// 探测不跟随请求的 context：请求可能先结束，而这一次探测的结果仍然该
	// 落库。它自己的超时见 probeTimeout。
	go p.probe(c, now)
}

// dueLocked 判断这个渠道该不该探。调用方必须持有 p.mu。
//
// 两个时间取较晚的那个：候选快照可能是在上一次探测完成之前读出来的，
// 只看它会让刚探过的渠道马上被再探一次。
func (p *QuotaProbe) dueLocked(id int64, fromCandidate *time.Time, now time.Time) bool {
	var latest time.Time
	if fromCandidate != nil {
		latest = *fromCandidate
	}
	if at, ok := p.lastAttempt[id]; ok && at.After(latest) {
		latest = at
	}
	if latest.IsZero() {
		return true
	}
	return now.Sub(latest) >= p.interval
}

// probe 真正发起一次上游查询并写回结果。它在自己的 goroutine 里跑。
func (p *QuotaProbe) probe(c route.Candidate, attemptedAt time.Time) {
	defer p.finish(c.Channel.ID)

	//nolint:contextcheck // 探测有意脱离请求的 context：请求先结束不代表这次探测该被取消
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()

	state, err := p.adapters.Quota(ctx, c.Channel)

	// 写回另起一个会合期限：查询恰好在 deadline 上返回时，上面那个 ctx
	// 已经结束，复用它写库会立刻失败（见 probeWriteTimeout）。WithoutCancel
	// 保留 ctx 上的值，只换掉那个已过期的期限。
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), probeWriteTimeout)
	defer cancelWrite()

	switch {
	case errors.Is(err, channel.ErrUnsupported):
		// 「这家查不到」是设计如此，不是故障：落一行「查不到」，不告警。
		p.recordMissing(writeCtx, c.Channel.ID, probeStatusUnsupported, attemptedAt)
	case err != nil:
		p.recordMissing(writeCtx, c.Channel.ID, probeStatusError, attemptedAt)
		p.logger().WarnContext(writeCtx, "渠道额度探测失败，保留上一次有效值",
			"channel_id", c.Channel.ID, "err", err)
	default:
		if writeErr := p.recordValue(writeCtx, c.Channel.ID, state, attemptedAt, p.now()); writeErr != nil {
			p.logger().WarnContext(writeCtx, "渠道额度探测结果没有写进去",
				"channel_id", c.Channel.ID, "err", writeErr)
		}
	}
}

// finish 放开这个渠道的单飞标记。
func (p *QuotaProbe) finish(id int64) {
	p.mu.Lock()
	delete(p.inflight, id)
	p.mu.Unlock()
	p.wg.Done()
}

// recordValue 把一次成功的实测合并进额度状态并写回。
//
// 读旧值、调 quota.Merge、写回在一个事务里，FOR UPDATE 挡住并发的自记账扣减
// ——否则「读旧值 → 覆盖」会把这段时间里扣掉的量抹掉。合并规则本身不在这里
// 重写：实测怎么覆盖、可信度给多少、要不要清错误信号，都由 internal/quota 决定。
func (p *QuotaProbe) recordValue(ctx context.Context, id int64, state quota.State, attemptedAt, observedAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启渠道 %d 的额度探测事务失败: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, _, err := readQuotaState(ctx, tx, id)
	if err != nil {
		return err
	}
	prev.ChannelID = id

	obs := quota.Observation{
		Kind:       quota.KindAPIQuery,
		Remaining:  state.Remaining,
		Unit:       state.Unit,
		ResetsAt:   state.ResetsAt,
		ObservedAt: observedAt,
	}
	// 落库之前校验这次观测自不自洽，与自记账那条路同一个判据（见
	// internal/quota 的 Validate）。校验失败就拒绝写入：一次「实测」既没给
	// 数值也没说耗尽，放过去会让状态被标成实测而毫无信息。
	if validateErr := quota.Validate(obs, observedAt); validateErr != nil {
		return fmt.Errorf("渠道 %d 的额度探测结果不自洽，拒绝写入: %w", id, validateErr)
	}

	next := quota.Merge(prev, obs, observedAt, p.staleAfter)
	if next.Remaining == nil {
		return fmt.Errorf("渠道 %d 的实测结果没有剩余量，拒绝写入", id)
	}

	unit := sql.NullString{String: next.Unit, Valid: next.Unit != ""}
	lastProbe := sql.NullTime{Time: observedAt, Valid: true}
	resetsAt := sql.NullTime{Time: observedAt, Valid: false}
	if state.ResetsAt != nil {
		resetsAt = sql.NullTime{Time: *state.ResetsAt, Valid: true}
	}
	_, err = tx.ExecContext(ctx, quotaProbeValueSQL,
		id, *next.Remaining, unit, next.Confidence, string(next.Kind),
		lastProbe, attemptedAt, probeStatusOK, next.UpdatedAt, resetsAt)
	if err != nil {
		return fmt.Errorf("写回渠道 %d 的额度探测结果失败: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交渠道 %d 的额度探测结果失败: %w", id, err)
	}
	return nil
}

// recordMissing 记下「没有拿到实测值」的结论与尝试时刻。
//
// 只动 last_probe_attempt_at 与 last_probe_status：余量、可信度、probe_kind、
// last_probe_at 全部原样保留。查不到与探测失败都不该被写成一个额度值——那会
// 让路由在一段完全未知的时间里按一个假数排序。
func (p *QuotaProbe) recordMissing(ctx context.Context, id int64, status string, attemptedAt time.Time) {
	_, err := p.db.ExecContext(ctx, quotaProbeMissingSQL, id, attemptedAt, status)
	if err != nil {
		p.logger().WarnContext(ctx, "渠道额度探测的结论没有写进去",
			"channel_id", id, "status", status, "err", err)
	}
}

func (p *QuotaProbe) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *QuotaProbe) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return probeTimeout
}

func (p *QuotaProbe) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// quotaProbeValueSQL 写一次成功的实测。
//
// 只动实测会变的列：remaining_estimate / unit / confidence / probe_kind /
// last_probe_at / updated_at，加上触发侧的两列。health 一概不碰——它来自
// 别的来源，这里没有关于它的新信息，顺手写 NULL 会把别人写的值抹掉。
//
// resets_at 用 COALESCE：适配器这次带回了重置时刻就更新，没带回就保留原值。
// 「这次没提」不等于「不会重置」，用 NULL 覆盖会让 scoreQuota 的紧迫度那一半
// 悄悄退回中性分。
//
// last_error_code / last_error_at 是例外：成功实测会清掉上一次的错误信号，
// 这是 quota.Merge 的规则（见它的 KindAPIQuery 分支），所以这里也清。
// error_observed 今天没有写入方，清的是本来就为 NULL 的两列。
const quotaProbeValueSQL = `
INSERT INTO channel_quota_state
       (channel_id, remaining_estimate, unit, confidence, probe_kind, last_probe_at,
        last_probe_attempt_at, last_probe_status, updated_at, resets_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (channel_id) DO UPDATE
   SET remaining_estimate    = EXCLUDED.remaining_estimate,
       unit                  = EXCLUDED.unit,
       confidence            = EXCLUDED.confidence,
       probe_kind            = EXCLUDED.probe_kind,
       last_probe_at         = EXCLUDED.last_probe_at,
       last_probe_attempt_at = EXCLUDED.last_probe_attempt_at,
       last_probe_status     = EXCLUDED.last_probe_status,
       updated_at            = EXCLUDED.updated_at,
       resets_at             = COALESCE(EXCLUDED.resets_at, channel_quota_state.resets_at),
       last_error_code       = NULL,
       last_error_at         = NULL`

// quotaProbeMissingSQL 写「查不到」或「探测失败」的结论。
//
// 一行都没有时插一行：没有这一行，触发侧就没有时刻可比，同一个渠道会被
// 每一条命中的请求反复探测。插出来的行余量是 NULL，读侧按「从未观测过」
// 处置，渠道照旧参与路由——这正是「查不到是正常结果」的含义。
const quotaProbeMissingSQL = `
INSERT INTO channel_quota_state (channel_id, last_probe_attempt_at, last_probe_status)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id) DO UPDATE
   SET last_probe_attempt_at = EXCLUDED.last_probe_attempt_at,
       last_probe_status     = EXCLUDED.last_probe_status`
