// Package channels 按规范模型名取出可用渠道、解密凭证，产出 route.Candidate。
//
// 它是 gateway 声明的 CandidateSource 的实现，但**不 import gateway**：
// gateway 是入口层，依赖方向只能是入口指向数据（见 scripts/check-layering.sh）。
// 接口由 gateway 声明、由本包用结构满足，两边不需要互相看见。
//
// # 为什么单独一层
//
// route 只决定「选谁」，不关心候选从哪来；本包只决定「谁可选」，
// 不做选择。分开是因为两者的变化原因不同：改选路策略动 route，
// 改渠道来源（表结构、过滤条件）动本包。
//
// # 凭证
//
// 本包是全仓库唯一真正调用 internal/credential 解密的地方。明文只进
// channel.Channel.Credential 交给适配器，不进日志、不进错误信息。
//
// # 管理面
//
// Source 只有读取路径。创建、修改、列举、启停渠道在 Admin 上（见 admin.go），
// 它不在请求热路径上，也不改变 Source 的行为。两个类型读的是同一套列，
// 由同一批辅助函数解码。
//
// # 取一次，不逐个查
//
// 候选通常是几个到几十个，一次查询把所有渠道连同健康度、额度一起取回来。
// 逐个渠道再查一遍会把「一个模型有几个候选」变成几倍的往返，
// 而这段代码在每条请求的热路径上。
//
// 目录里那份模型级声明（能力、上限）另算一次查询，与候选数无关：它是
// 按规范名查的单行，摆给这一批全部候选（见 profile）。
package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// Source 从数据库取候选渠道。
type Source struct {
	db     *sql.DB
	sealer *credential.Sealer

	// catalog 读回规范模型在目录里声明的能力与上限（见 profile）。
	// 由 New 接上，它只包着同一个 *sql.DB，没有自己的状态。
	catalog modelCatalog

	// Logger 记录被跳过的渠道（凭证解不开的那些）。为 nil 时用 slog.Default()。
	Logger *slog.Logger

	// Now 可注入，便于测试「健康数据是否过期」这类与当前时间有关的判断。
	Now func() time.Time
}

// modelCatalog 按规范名读回目录里与选路有关的那几列。
//
// 声明在本包、实现在 internal/catalog：model_catalog 的读写入口只有 catalog
// 一处（见那个包的注释），所以本包不自己写那条 SQL。而 route 不认识那张表
// （它不碰数据库），catalog 也不认识 route——两边的形状对不上，投影就落在
// 本包（见 profile），因为只有本包同时看得见两者。
type modelCatalog interface {
	Constraints(ctx context.Context, canonicalName string) (catalog.Constraints, error)
}

// New 包装一个已打开的数据库句柄与一个主密钥封装器。
//
// sealer 允许为 nil：只有不需要接触上游身份的角色（worker）才可能没有主密钥。
// 那种情况下每个渠道都会以 ErrNoMasterKey 解不开——报错方向明确，
// 好过在这里 panic 或者推迟到别处才发现。
func New(db *sql.DB, sealer *credential.Sealer) *Source {
	return &Source{db: db, sealer: sealer, catalog: catalog.New(db)}
}

// 编译期断言：它要满足 gateway 对候选来源的要求。
//
// 这里写内联接口而不是 import gateway——那个包是入口层，
// 数据面依赖它会把整条入口链路拖进本包。
var _ interface {
	Candidates(context.Context, string) ([]route.Candidate, error)
} = (*Source)(nil)

const (
	// healthWindow 是聚合健康数据的回看窗口。
	//
	// 取 24 小时：够长到有样本（低流量的渠道一小时可能只有几次请求），
	// 又短到不会让昨天的故障一直影响今天的选路。
	healthWindow = 24 * time.Hour

	// streakCooldown 是「连续失败」的冷却期。
	//
	// 连续失败是一个逐次维护的真计数（成功清零、失败加一，见 RecordCall），
	// 而它自己就能形成死循环：计数到阈值 → 渠道被摘掉 → 没有流量 →
	// 计数不动 → 永远摘不掉。冷却期是那个口子：最后一次失败早于冷却期的
	// 渠道重新进入候选集，试一次；失败就再摘一轮，成功就清零。
	//
	// 取 15 分钟：一个坏渠道在此期间最多被打扰一次（一次注定失败的调用），
	// 而一个已经恢复的渠道不必等太久。取更长会让恢复慢一拍，
	// 取更短会让坏渠道被反复试。
	streakCooldown = 15 * time.Minute
)

// Candidates 返回某个规范模型名下的全部可用渠道。
//
// 过滤条件是 `status = 'enabled'` 且 `models` 数组包含该规范名。
// 不支持该模型的渠道根本不出现在结果里——它与「支持但当前不可用」
// 是两回事，后者要留给 route 去排除并给出理由。
func (s *Source) Candidates(ctx context.Context, canonicalModel string) ([]route.Candidate, error) {
	if canonicalModel == "" {
		return nil, errors.New("规范模型名为空")
	}

	now := s.now()
	rows, err := s.db.QueryContext(ctx, candidateQuery, canonicalModel, now.Add(-healthWindow))
	if err != nil {
		return nil, fmt.Errorf("查询候选渠道失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：没有候选是正常结果（这个模型没人配），
	// 不是「没有值」。调用方拿到的总是可以直接遍历与追加的切片。
	out := []route.Candidate{}
	var failures []DecryptFailure

	for rows.Next() {
		row, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}

		plaintext, err := s.decrypt(row.credentialEnc)
		if err != nil {
			// 一个渠道的密文坏了不该让同一模型下别人的渠道一起不可用，
			// 所以跳过它继续。跳过不是静默：下面要么记日志，要么整次调用报错。
			failures = append(failures, DecryptFailure{ChannelID: row.id, Name: row.name, Err: err})
			continue
		}

		cand, err := row.candidate(plaintext, now)
		if err != nil {
			// 这里与上面相反，整次调用报错。两类失败的性质不同：
			// 凭证解不开是**运维中会发生的事**（轮换、少配一把钥匙），
			// 只影响那一行；而 setting 不是 JSON、numeric 不是数字意味着
			// 这一行的类型与表定义对不上——那是系统性的问题，
			// 跳过它只会让坏数据继续悄悄留在库里。
			return nil, err
		}
		out = append(out, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历候选渠道失败: %w", err)
	}

	if len(failures) > 0 {
		if len(out) == 0 {
			// 一个都没解出来。这多半不是「某个渠道的数据坏了」，
			// 而是**这个进程没有主密钥**——那种情况下错误信息要能指向部署，
			// 所以把失败原样报出去，而不是返回一个空候选集。
			return nil, &DecryptErrors{Failures: failures}
		}
		// 还有别的渠道可用：请求照常进行，但把跳过的记下来，
		// 否则「为什么这个渠道一直不参与选路」就无从回答。
		for _, f := range failures {
			s.logger().WarnContext(ctx, "渠道凭证解不开，本次不参与选路",
				"channel_id", f.ChannelID, "channel", f.Name, "err", f.Err)
		}
	}

	if len(out) > 0 {
		// 目录声明是**模型级**的：一次查询，摆给这一批全部候选（route 那边
		// 按它逐条判）。
		//
		// 一个候选都没有时不查：那次请求要报的是「没有可用的渠道」，
		// 多查一次只会多一个失败点。
		profile, err := s.profile(ctx, canonicalModel)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Profile = profile
		}
	}
	return out, nil
}

// profile 读回这个规范模型在目录里声明的能力与上限。
//
// # 目录里没有这条记录时返回 nil
//
// nil 是**未知**，是放行而不是「不支持」：一个还没进目录的模型与一个进去
// 了、但某一项能力没配过的模型，处置相同（都放行），而「目录里没有它」
// 恰恰是最常见的开始状态。按「不支持」处理会让整批还没进目录的模型立刻
// 用不了（见 route.ModelProfile）。
//
// # 读目录失败与「没有这条记录」分开
//
// 前者整次取候选报错，不降级成「未知」。降级看起来更宽容，实际是把一次
// 查询故障变成「目录判据静默失效」——而失效与「这个部署没配目录判据」在
// 日志与响应里长得一样（同 route.ErrNoAliasSource 的那条理由：别让「没接上」
// 与「不用这一项」同形）。这次查询与候选查询打的是同一个库，它失败说明
// 问题不在这一条请求上。
//
// 上面那一节就是 `nolint:nilnil` 的理由：这个 nil 是结论，不是「出错了但没报」。
func (s *Source) profile(ctx context.Context, canonicalName string) (*route.ModelProfile, error) {
	cons, err := s.catalog.Constraints(ctx, canonicalName)
	if errors.Is(err, catalog.ErrModelNotFound) {
		return nil, nil //nolint:nilnil // 见上
	}
	if err != nil {
		return nil, fmt.Errorf("读取模型 %q 的目录声明失败: %w", canonicalName, err)
	}
	// 只带判据用得上的那几项：目录读回来的是整张能力矩阵，而 route 那边的
	// 判据只看这三项（见 route.ModelProfile）。
	//
	// 三项能力是**原样搬过去的指针**，两侧的三态因此是同一份：nil 还是 nil，
	// false 还是 false。中间不做转换是刻意的——「哪一项该按未知放行」这条
	// 判断只该有一处（见 route.declaredUnsupported），投影里再判一次就是
	// 第二份判据，而两份判据漂移的症状是「某条请求莫名其妙选到了不做这件事
	// 的模型」。指针只读：两边都不写它。
	return &route.ModelProfile{
		Tools:        cons.Capabilities.Tools,
		Vision:       cons.Capabilities.Vision,
		Reasoning:    cons.Capabilities.Reasoning,
		ContextLimit: int64(cons.ContextLimit),
		OutputLimit:  int64(cons.OutputLimit),
	}, nil
}

// ErrNoMasterKey 表示本进程没有能解开这份凭证的主密钥。
//
// 它是**部署问题**：环境变量里少了主密钥，或者轮换时把旧密钥删早了。
// 与 ErrCredentialCorrupt 分开是有意的——两者都表现为「解不开」，
// 但一个要去看配置、一个要去看那一行数据，混在一起排查会往错的方向找。
var ErrNoMasterKey = errors.New("没有能解开这份凭证的主密钥")

// ErrCredentialCorrupt 表示凭证密文本身不合法。
//
// 它是**数据问题**：这一行的密文被改动过，或者写进去的时候就不完整。
// 换主密钥、加主密钥都救不回来，只能重写这一行。
var ErrCredentialCorrupt = errors.New("凭证密文已损坏")

// DecryptFailure 是一个渠道解密失败及其原因。
type DecryptFailure struct {
	ChannelID int64
	Name      string
	Err       error
}

// DecryptErrors 是「取到了候选行，但一份凭证都解不开」时的错误。
//
// 单独一个类型是为了让调用方与运维能区分「没有渠道配置」与
// 「有配置但一份都解不开」：前者要去看渠道配了没，后者要去看主密钥。
type DecryptErrors struct {
	Failures []DecryptFailure
}

// Error 汇总失败原因。它只带渠道标识与内层错误——内层错误里没有
// 明文凭证，也没有密钥材料（见 internal/credential 对错误信息的要求）。
func (e *DecryptErrors) Error() string {
	if len(e.Failures) == 0 {
		return "候选渠道的凭证都解不开"
	}
	first := e.Failures[0]
	return fmt.Sprintf("%d 个候选渠道的凭证都解不开，例如渠道 %d（%s）：%v",
		len(e.Failures), first.ChannelID, first.Name, first.Err)
}

// Unwrap 让 errors.Is/As 能穿过这层聚合，直接判断里面是缺主密钥还是密文坏了。
func (e *DecryptErrors) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		out = append(out, f.Err)
	}
	return out
}

// decrypt 把一行的密文解成明文凭证。
//
// 错误分类集中在这里：调用方只需要判断 ErrNoMasterKey 与 ErrCredentialCorrupt，
// 不必知道 internal/credential 的内部错误类型。
func (s *Source) decrypt(envelope []byte) (string, error) {
	if s.sealer == nil {
		return "", fmt.Errorf("%w：本进程没有配置主密钥", ErrNoMasterKey)
	}
	plaintext, err := s.sealer.Open(envelope)
	if err != nil {
		switch {
		case errors.Is(err, credential.ErrNoKey):
			// 密文里记的钥匙标识，本进程一把都没有。
			//
			// 两个 %w 而不是「%w：%v」：后者会把底层那句「哪把钥匙找不到」
			// 从错误链上摘掉，而那正是排查时唯一有用的信息。Go 1.20 起
			// 一个 Errorf 可以带多个 %w，errors.Is 对两个都成立。
			return "", fmt.Errorf("%w：%w", ErrNoMasterKey, err)
		case errors.Is(err, credential.ErrMalformed):
			return "", fmt.Errorf("%w：%w", ErrCredentialCorrupt, err)
		default:
			return "", fmt.Errorf("解密凭证失败: %w", err)
		}
	}
	return string(plaintext), nil
}

// channelAlive 是「这一行渠道没有软删」这条判据的**唯一**一处字面量。
//
// channels.deleted_at 非 NULL 表示它被 larkaxctl channel delete 删掉了。
// 软删之后从属的行都还在（七条外键是 ON DELETE CASCADE，真删会带走它们），
// 所以「这条渠道还算不算数」不再能从「行在不在」读出来——它必须由每一条
// 读 channels 的查询自己带上。
//
// # 为什么是片段而不是 View / RLS
//
// View 或 RLS 会把「管理面 --all 看得见已删的行」这条例外变成另一套读法，
// 而例外一旦藏在 schema 层，「谁看得见已删的行」就不再能从调用点读出来；
// 何况本仓的测试都是对着表写 SQL 的，View 会让夹具与生产走两条路。
//
// # 加一个新读点时要做什么
//
// 把这条片段拼进 WHERE。漏掉它不会报错：一条已删的渠道会继续参与选路、
// 出现在清单上，而它的 status 同时被删除动作置成了 disabled——选路那一侧
// 有 status 兜底，别处没有。所以清单、轮换、健康汇总、别名列举这些按 id 取
// 行的路径要各自钉住这条（见 integration_test.go 的软删用例）。
const channelAlive = `deleted_at IS NULL`

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// candidateQuery 一次取回某个规范模型名下的全部渠道及其健康度、额度。
//
// 三张表的关系：channels 是主表，健康度按小时桶存（一个渠道多行，
// 所以要聚合），额度每个渠道最多一行。用 LEFT JOIN 是因为后两者都**允许缺失**：
// 新渠道还没有健康数据，很多上游也查不到额度。缺失不是错误，
// 后面的映射会把它们表达成「未知」。
//
// 「连续失败」取自 channels 的列而不是从桶里算：桶里没有逐次记录，
// 桶内有过成功时那段连续长度落在 0 与 failures 之间，算不出来。
//
// # 成本、延迟、缓存三列不套 COALESCE
//
// 那三列是打分的三个维度，而**「没有数据」与「值是 0」在选路里是相反的
// 结论**：一个没记录过成本的渠道既不该被当成免费，也不该占掉成本那一维的
// 权重。COALESCE 把「这个窗口里一行都没有」的 NULL 抹成 0，而 cost 与两个
// cache 列本身又是 `NOT NULL DEFAULT 0`（见 00001_init.sql）——于是「没记过」
// 与「记了个 0」在库里就是同一个值，读侧再抹一层就更分不开。这正是「三列
// 恒为 0、权重写着在起作用而实际是常数」那条缺陷的根因。
//
// 读侧仍然只取最近一桶的 p95（见下面的 ARRAY_AGG）：写入侧每桶算一次，
// 这里取最近的那个非空值，不跨桶合并。
//
// 所以聚合列原样返回 NULL，由 candidateRow 用 sql.Null* 接住，
// 再映射成 route.Candidate 上的 nil。
//
// 要注意这三列**不是同一种可空性**：
//
//   - latency_p95_ms 本来就可空，而且查询里已经用 FILTER 滤掉了空值，
//     所以「没有样本」与「样本是 0 毫秒」在 SQL 这一层就分得开。
//     它此前的问题只在 Go 侧：那个 NULL 被扫进一个零值 Duration。
//     写入侧今天确实会写它了（见 record.go 的 rollupSignalsSQL），
//     但「这个窗口里没有一条带耗时的用量事件」仍然是 NULL。
//   - cost 与两个 cache 列是 `NOT NULL DEFAULT 0`，所以去掉 COALESCE 只能
//     还原「窗口里一行都没有」这一种缺失。**「有桶、但那一列从没被写过」
//     仍然是 0**，与「真的是 0」在库上无法区分——那一层由 costPerUnit /
//     cacheHitRate 的判据兜着（0 按「没有数据」处理，理由见那两处注释）。
//     cache 两列今天由写入侧按桶重算（它们是 **命中与未命中的 token 数**，
//     见 rollupSignalsSQL），一个请求都没服务过的桶里它们就是 0；
//     而 0 在 cacheHitRate 那里读作「没有样本」——两件事在这里恰好同义，
//     因为 token 数为 0 的请求不存在。cost 仍然没有写入方。
//
// 其中 requests / successes 仍然套 COALESCE：它们的 0 是**有意义的 0**
// （这个窗口里一次都没请求过），不是缺失。
//
// # failures 这一列没有读方
//
// 它由写入侧维护，但连续失败另有 channels.consecutive_failures 这个逐次
// 维护的真计数（桶重建不出「连续」这个量）。成功率的分子分母是
// successes / requests，而「没成功也没失败」（客户端断开）刻意留在分母里
// （见 record.go 的 rollup），所以这里也不需要 failures 去还原它。
// 留着它是为了让「有多少次是失败」在库里可查，读侧不需要它——
// 不为消掉一列而硬接一条假的消费。
const candidateQuery = `
SELECT
    c.id,
    c.name,
    c.type,
    c.base_url,
    array_to_json(c.models)::text,
    c.setting,
    c.weight,
    c.priority,
    COALESCE(c.owner_user_id, 0),
    c.credential_enc,
    c.consecutive_failures,
    c.last_failure_at,
    h.requests,
    h.successes,
    h.latency_p95_ms,
    h.cost,
    h.cache_hit_tokens,
    h.cache_miss_tokens,
    q.remaining_estimate,
    q.unit,
    q.confidence,
    q.probe_kind,
    q.health,
    q.resets_at,
    q.last_probe_at,
    q.last_error_code
  FROM channels c
  LEFT JOIN LATERAL (
        SELECT
            COALESCE(SUM(r.requests), 0)  AS requests,
            COALESCE(SUM(r.successes), 0) AS successes,
            (ARRAY_AGG(r.latency_p95_ms ORDER BY r.bucket DESC)
                FILTER (WHERE r.latency_p95_ms IS NOT NULL))[1] AS latency_p95_ms,
            SUM(r.cost)              AS cost,
            SUM(r.cache_hit_tokens)  AS cache_hit_tokens,
            SUM(r.cache_miss_tokens) AS cache_miss_tokens
          FROM channel_health_rollup r
         WHERE r.channel_id = c.id
           AND r.bucket >= $2
       ) h ON true
  LEFT JOIN channel_quota_state q ON q.channel_id = c.id
 WHERE c.status = 'enabled'
   AND c.models @> ARRAY[$1]::text[]
   AND c.` + channelAlive + `
 ORDER BY c.id`

// candidateRow 是查询结果的一行。
//
// 可空的列用 sql.Null* 接住：它们要么来自 LEFT JOIN 的缺失，要么来自
// 表里本就允许为 NULL 的列。**不能用一个零值把「没有」与「是 0」合并**——
// 「额度未知」与「额度是 0」在选路里是相反的结论。
type candidateRow struct {
	id          int64
	name        string
	channelType string
	baseURL     string
	// modelsJSON 是 channels.models 的 JSON 形式（见查询里的 array_to_json）。
	// database/sql 只认有限的几种 driver.Value，text[] 不在其中，
	// 所以让数据库把它转成 JSON 文本，再在 Go 侧解成 []string。
	modelsJSON    string
	models        []string
	setting       []byte
	weight        int
	priority      int
	ownerUserID   int64
	credentialEnc []byte

	streak        int
	lastFailureAt sql.NullTime

	requests  int64
	successes int64
	// latencyP95 / cost / cacheHit / cacheMiss 四列都是可空的，
	// 而且**必须**可空：它们各自的「没有数据」与「值是 0」是相反的结论
	// （见 candidateQuery 的注释）。用零值接住就等于把两者合并。
	latencyP95 sql.NullInt64
	cost       sql.NullString
	cacheHit   sql.NullInt64
	cacheMiss  sql.NullInt64

	remaining sql.NullString
	unit      sql.NullString
	// confidence 与 remaining 一起走：它说的是「这个余量有多可信」，
	// 没有余量时它没有意义。列为 NULL 只可能来自 LEFT JOIN 没连上额度行
	// （表上这一列是 NOT NULL DEFAULT 0）。
	confidence  sql.NullString
	probeKind   sql.NullString
	quotaHealth sql.NullString
	resetsAt    sql.NullTime
	// lastProbeAt / lastErrorCode 与 remaining 一起决定 quota.State.Routable：
	// 「余量未知」有两种，一种是从来没观测过（上游不支持查询额度），
	// 另一种是观测过却没拿到数值。少带这两列就分不出它们，判据退化成
	// 「有没有数值」。
	lastProbeAt   sql.NullTime
	lastErrorCode sql.NullString
}

func scanCandidate(rows *sql.Rows) (candidateRow, error) {
	var row candidateRow
	err := rows.Scan(
		&row.id, &row.name, &row.channelType, &row.baseURL,
		&row.modelsJSON, &row.setting, &row.weight, &row.priority, &row.ownerUserID,
		&row.credentialEnc,
		&row.streak, &row.lastFailureAt,
		&row.requests, &row.successes, &row.latencyP95,
		&row.cost, &row.cacheHit, &row.cacheMiss,
		&row.remaining, &row.unit, &row.confidence,
		&row.probeKind, &row.quotaHealth, &row.resetsAt,
		&row.lastProbeAt, &row.lastErrorCode,
	)
	if err != nil {
		return candidateRow{}, fmt.Errorf("读取渠道行失败: %w", err)
	}
	models, err := decodeModels(row.id, row.modelsJSON)
	if err != nil {
		return candidateRow{}, err
	}
	row.models = models
	return row, nil
}

// decodeModels 解开 models 列的 JSON 文本。
//
// 查询里用 array_to_json 把它转成文本（database/sql 不认 text[]），
// 所以两处查询都要按同一个假设解回来。写路径也走这个函数：两边对同一列的
// 读法不一致时，症状是「写进去的值读出来不对」，而那只在某些行上出现。
func decodeModels(channelID int64, modelsJSON string) ([]string, error) {
	var models []string
	if err := json.Unmarshal([]byte(modelsJSON), &models); err != nil {
		return nil, fmt.Errorf("渠道 %d 的 models 不是合法的 JSON 数组: %w", channelID, err)
	}
	return models, nil
}

// candidate 把一行映射成一个候选。
//
// plaintext 是已解密的明文，由调用方传入——本函数不接触解密，
// 于是它可以脱离数据库与主密钥单独测。
func (row candidateRow) candidate(plaintext string, now time.Time) (route.Candidate, error) {
	setting, err := decodeSetting(row.setting)
	if err != nil {
		return route.Candidate{}, fmt.Errorf("渠道 %d 的 setting 不是合法 JSON: %w", row.id, err)
	}
	quotaState, err := row.quota()
	if err != nil {
		return route.Candidate{}, err
	}
	cost, err := row.costPerUnit()
	if err != nil {
		return route.Candidate{}, err
	}
	cacheRate := row.cacheHitRate()

	return route.Candidate{
		Channel: channel.Channel{
			ID:         row.id,
			Name:       row.name,
			Type:       row.channelType,
			BaseURL:    row.baseURL,
			Credential: plaintext,
			Models:     row.models,
			Setting:    setting,
		},
		OwnerUserID:  row.ownerUserID,
		Priority:     row.priority,
		Weight:       row.weight,
		Health:       row.health(now),
		Quota:        quotaState,
		CostPerUnit:  cost,
		CacheHitRate: cacheRate,
		// SampleCount 取参与缓存统计的 token 数，而不是请求数。
		//
		// route 用它判断命中率可不可信（样本太少就不参与打分）。用请求数会
		// 判错：不报缓存明细的上游很常见，那些渠道请求数可以很多，
		// 两个缓存计数却都是 0——请求数会让「命中率 0」看起来有依据，
		// 于是它们被打成最低分，而事实是**没有数据**。
		SampleCount: row.cacheSamples(),
	}, nil
}

// health 由健康度桶汇总出 route.Health。
//
// # 没有数据时给的是「未知」而不是「最差」
//
// 新渠道还没有任何桶，此时 SuccessRate 留零值、LatencyP95 为 nil。route 对
// 零值的处理就是「未知」：scoreHealth 对 SuccessRate <= 0 给中性分，
// scoreLatency 对 nil 把延迟这一维整个剔除。**不能反过来填一个「很差」的
// 值**——那会让新渠道永远选不上，而它可能正是最该被试用的那个。
//
// # 延迟为什么用指针而不是零值
//
// 「这个窗口里没有 p95 样本」与「p95 恰好是 0 毫秒」是两件事，而一个
// time.Duration 的零值把两者合成了一个数。合掉的后果是延迟这一维永远
// 拿不到真数据：读侧把 NULL 扫成 0，route 又把 0 当成未知，于是即便写入
// 侧某天开始写这一列，它也仍然是一条走不通的路。
func (row candidateRow) health(now time.Time) route.Health {
	h := route.Health{}

	// 成功率：窗口内的成功数 / 请求数。没有请求时留 0（未知）。
	if row.requests > 0 {
		h.SuccessRate = float64(row.successes) / float64(row.requests)
	}
	if row.latencyP95.Valid {
		p95 := time.Duration(row.latencyP95.Int64) * time.Millisecond
		h.LatencyP95 = &p95
	}
	h.ConsecutiveFailures = row.consecutiveFailures(now)
	return h
}

// consecutiveFailures 给出「连续失败次数」。
//
// # 它取自一个真计数，不是从桶里估的
//
// channels.consecutive_failures 由调用收尾处维护：成功清零、失败加一
// （见 RecordCall）。小时桶里的三个计数重建不出这个量——桶内有过成功时，
// 那段连续长度落在 0 与 failures 之间。
//
// # 冷却期
//
// 计数早于冷却期的渠道按 0 处理，见 streakCooldown：被摘掉的渠道没有流量，
// 计数不会自己降回去，冷却期是让它重新被试一次的那个口子。
//
// 时间戳缺失而计数非零的行按「冷却期已过」处理。那种行只可能来自手工写入
// ——写入方总是把两列一起写。两个方向不对称：按已过处理最多是少摘一次，
// 按新鲜处理会让渠道永远摘不掉，而那正是这一列要消除的状态。
func (row candidateRow) consecutiveFailures(now time.Time) int {
	if row.streak <= 0 {
		return 0
	}
	if !row.lastFailureAt.Valid {
		return 0
	}
	if now.Sub(row.lastFailureAt.Time) >= streakCooldown {
		return 0
	}
	return row.streak
}

// quota 把额度快照映射成 quota.State。
//
// # 三态，不是两态
//
// 「有额度行且 remaining_estimate 有值」「有额度行但它是 NULL」「根本没有额度行」
// 里，后两种都是**未知**：NULL 表示这个渠道的额度查不出来（多数订阅制上游
// 不提供查询接口），不是「额度是 0」。两者都映射成 Remaining 为 nil。
//
// 混掉的后果是具体的：route 的 DrainReason 把「有值且 <= 0」当成「额度已
// 耗尽」直接排除该渠道。把未知当 0 会让所有查不到额度的渠道一律选不上，
// 而这几乎等于把全部订阅制上游排除在外。
//
// # 但 drained 是有信号的那一种，它先判
//
// 上面那条说的是**没有信号**时的兜底。health = 'drained' 不是没有信号：
// 它是上游明确说过「这个渠道没额度了」。有信号就该按事实走，映射成
// Remaining 为 0，让 DrainReason 把它排除掉。
//
// 两者的分界是「有没有观测到」，不是「remaining_estimate 是不是 NULL」：
// 一行 health='drained' 完全可能同时带着 NULL 的 remaining_estimate
// （上游从错误里报的 drained，没给出数字）。**所以 drained 的判断必须排在
// 前面**——顺序反了，这一行会落进「未知」分支，于是 route 照旧会选它，
// 而这正是本分支要消除的那条路径。
//
// 其余三态（healthy / degraded / unknown）都按原映射走：它们说的不是
// 「没额度了」。degraded 尤其不能当成 0——那是「不太顺」，不是「用完了」。
//
// # 观测痕迹一并带出去
//
// last_probe_at 与 last_error_code 也填进 State：它们与余量一起决定
// Routable（「从未观测过」与「观测过却没拿到数值」是两回事），
// 而这两列正是本函数早先丢掉的那两列——丢掉的后果是判据在路由侧退化成
// 「有没有数值」。
//
// confidence 同样要带出去（由 withValue 填），理由见那里。
func (row candidateRow) quota() (quota.State, error) {
	state := quota.State{
		ChannelID:     row.id,
		Unit:          row.unit.String,
		Kind:          quota.Kind(row.probeKind.String),
		LastErrorCode: row.lastErrorCode.String,
	}
	if row.lastProbeAt.Valid {
		at := row.lastProbeAt.Time
		state.LastProbeAt = &at
	}
	if row.resetsAt.Valid {
		reset := row.resetsAt.Time
		state.ResetsAt = &reset
	}

	if strings.EqualFold(row.quotaHealth.String, quotaHealthDrained) {
		return row.withValue(state, 0)
	}
	if !row.remaining.Valid {
		return state, nil
	}
	remaining, err := strconv.ParseFloat(row.remaining.String, 64)
	if err != nil {
		return quota.State{}, fmt.Errorf("渠道 %d 的 remaining_estimate 不是数字（%q）: %w",
			row.id, row.remaining.String, err)
	}
	return row.withValue(state, remaining)
}

// withValue 填上「有余量」的那三列：余量本身、它是实测还是推算，以及这个数
// 有多可信。
//
// drained 与实测值共用它，于是「推算值怎么标」只有一处答案——两条路径
// 不该在这里分叉。它同时保证这三列只在真的有余量时才有值：没有数值的状态
// 既不是实测也不是推算，可信度也无从谈起。
//
// # confidence 为什么必须带出去
//
// route 按它把额度那一项往中性分拉（见 route 的 scoreQuota）。少带它，
// 每个候选的额度项都退成中性分——**这种失效是静默的**：候选照样排得出来，
// 只是排法与额度完全无关，看起来和「大家额度都差不多」一模一样。
// 本函数早先正是漏了这一列（issue #233）。
func (row candidateRow) withValue(state quota.State, remaining float64) (quota.State, error) {
	state.Remaining = &remaining
	// 只有 api_query 是上游实测。自记账与「从错误里推断」都是推算，
	// 插件契约按这个标记给可信度打折（见 internal/plugin 的 RouteContextFrom）。
	state.Estimated = !isAPIQuery(row.probeKind.String)

	// confidence 为 NULL 只可能是没有额度行（LEFT JOIN 没连上）；表上这一列
	// 是 NOT NULL DEFAULT 0。解析不了则说明这一行的类型与表定义对不上，
	// 与 remaining_estimate 同样处置：整次调用报错，不静默当成 0。
	if row.confidence.Valid && row.confidence.String != "" {
		v, err := strconv.ParseFloat(row.confidence.String, 64)
		if err != nil {
			return quota.State{}, fmt.Errorf("渠道 %d 的 confidence 不是数字（%q）: %w",
				row.id, row.confidence.String, err)
		}
		state.Confidence = v
	}
	return state, nil
}

// quotaHealthDrained 是「上游说这个渠道没额度了」的 quota health 取值。
const quotaHealthDrained = "drained"

// probeKindAPIQuery 是「这个数字来自上游查询」的 probe_kind 取值。
const probeKindAPIQuery = "api_query"

// costPerUnit 取窗口内的平均每次请求成本，没有数据时返回 nil。
//
// 表里只有桶的成本**总额**与请求数，没有 token 总量，所以算不出
// 每 token 的成本；每次请求的平均成本是这里能给出的最小单位。
//
// # 为什么 0 也算「没有数据」
//
// 那一列是 `numeric NOT NULL DEFAULT 0`（见 00001_init.sql），而写入侧
// 从来没有写过它——所以「有桶、成本是 0」这个组合今天只可能表示**没记过
// 成本**，而不是「免费」。把它当免费会让每一个没记过成本的渠道拿到成本
// 这一维的满分，而那正是最容易超支的路径。
//
// 代价是：真的免费（成本确实为 0）今天无法与「没记过」区分开。这是列的
// 可空性带来的边界，不是本函数的选择——要分清得先让那一列可空，或者让
// 写入侧显式写下「这次没有成本数据」。在那之前，把 0 判成未知是两者中
// 更保守的一侧。
//
// 返回的 nil 是**「这一维没有数据」这个合法结论**，不是「出错了但没报」——
// 所以这里显式 nolint 而不是造一个哨兵错误让每个调用方多判一次。
func (row candidateRow) costPerUnit() (*float64, error) {
	if row.requests <= 0 || !row.cost.Valid {
		return nil, nil //nolint:nilnil // 见上
	}
	total, err := strconv.ParseFloat(row.cost.String, 64)
	if err != nil {
		return nil, fmt.Errorf("渠道 %d 的 cost 不是数字（%q）: %w", row.id, row.cost.String, err)
	}
	if total <= 0 {
		return nil, nil //nolint:nilnil // 见上
	}
	per := total / float64(row.requests)
	return &per, nil
}

// cacheHitRate 取窗口内的缓存命中率，没有样本时返回 nil。
//
// 两个计数是 `bigint NOT NULL DEFAULT 0`，由写入侧按桶从 usage_events
// 重算（见 record.go 的 rollupSignalsSQL）——它们分别是**命中**与
// **没被缓存服务到**的输入 token 数，分母因此就是输入总量。
//
// 合计为 0 就表示**没有样本**（一次请求的输入不可能是 0 个 token）。
// 所以这里不需要额外判 NULL：合计为 0 与「没有数据」是同一件事。
//
// 命中率**恰好为 0** 只有一种来路：有样本、且一次都没命中。那时返回的是
// 指向 0 的指针，与 nil 分得开，route 会给它 0 分而不是把这一维剔除。
func (row candidateRow) cacheHitRate() *float64 {
	total := row.cacheSamples()
	if total <= 0 {
		return nil
	}
	rate := float64(row.cacheHit.Int64) / float64(total)
	return &rate
}

// cacheSamples 是参与缓存统计的 token 数（命中 + 未命中）。
//
// 两个计数都可空（没有桶时 SUM 给 NULL），NULL 按 0 计：没有样本。
func (row candidateRow) cacheSamples() int64 {
	return row.cacheHit.Int64 + row.cacheMiss.Int64
}

// decodeSetting 解开 channels.setting 的 jsonb。
//
// 解出来的 map 原样交给 Channel.Setting：**本包不解释任何键**，键由该渠道
// type 指向的适配器认（见 channel.Channel.Setting）。今天没有任何适配器读它，
// 所以这里正确解出来的值也不会影响请求路径。
//
// 列是 NOT NULL DEFAULT '{}'，所以正常总是有值的；长度为零时给一个空 map
// 而不是报错——那说明这一行是别的手段写进去的，不值得让整个候选集取不出来。
func decodeSetting(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
