package channels

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// 账号健康结论的取值，与迁移 00068 的 CHECK 约束同一套。
//
// 它们与渠道侧（channel_quota_state.health）复用同一组词：同一套状态机、同一种
// 读法，运维在一处学过「drained 表示不参与选路」，在另一处不必再学一遍。
const (
	// AccountHealthUnknown 是「还没被调用过」。没有 account_state 行时也读作它。
	AccountHealthUnknown = "unknown"
	// AccountHealthHealthy 表示最近一次调用成功。
	AccountHealthHealthy = "healthy"
	// AccountHealthDegraded 表示最近连着失败过，但还没到摘除的阈值。
	AccountHealthDegraded = "degraded"
	// AccountHealthDrained 表示这个账号暂时不参与选路。
	AccountHealthDrained = "drained"
)

// accountFailureCutoff 是连续失败多少次之后把账号标成 drained。
//
// 取 3，与 route 那边的渠道阈值同一个数、同一条理由：偶发的一次抖动很常见，
// 一次失败就摘掉账号会让流量在两个 key 之间来回跳；连续三次已经说明这把 key
// 此刻确实不行。渠道侧那个常量在 internal/route（`consecutiveFailureCutoff`），
// 两处目前同值但**不是同一个判据**：渠道的阈值决定「这条渠道还参不参选」，
// 这里的决定「这个账号还参不参选」，将来其中一个要调时不至于牵连另一个。
const accountFailureCutoff = 3

// poolAccount 是候选查询带回来的一个账号：选路要看的那几列。
//
// 它与 ChannelAccount（管理面展示用）刻意分开：这里带着**密文**，只在本包内
// 流转，解出来的明文进 route.Candidate；管理面那个只带「挂在哪些主密钥下」。
type poolAccount struct {
	ID     int64
	Name   string
	Weight int
	// Envelope 是信封加密的凭证密文（已从 base64 解回来）。
	Envelope []byte

	// Health 是 account_state.health；没有状态行时是 AccountHealthUnknown。
	Health string
	// ConsecutiveFailures 是连续失败次数，成功清零。
	ConsecutiveFailures int
	// LastFailureAt 是最近一次失败的时刻，nil 表示没失败过。
	//
	// 它同时是 drained 的冷却期时钟：没有它，被摘掉的账号没有流量，
	// 状态也就永远不会自己变回去（与渠道侧的 streakCooldown 同一个问题）。
	LastFailureAt *time.Time
}

// poolAccountJSON 是查询里 json_agg 出来的一个账号的形状。
//
// health 与 failures 在 SQL 里已经 COALESCE 过（没有状态行时是 unknown / 0），
// 所以这里不需要指针；last_failure_at 必须是可空的，而 epoch 秒是 JSON 能原样
// 带过来的最小形态（timestamptz 的文本形式要另解一次，多一处可能走样的地方）。
type poolAccountJSON struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Weight      int      `json:"weight"`
	Enc         string   `json:"enc"`
	Health      string   `json:"health"`
	Failures    int      `json:"failures"`
	LastFailure *float64 `json:"last_failure_at"`
}

// decodePoolAccounts 解开候选查询带出来的账号池 JSON。
//
// 池子是**热路径**上的一列：它坏了要么整条渠道被跳过（静默），要么整次选路
// 报错。这里选后者——与候选查询对「setting 不是 JSON」的处置一致：那是系统性的
// 问题，跳过只会让坏数据继续留在库里。
func decodePoolAccounts(channelID int64, raw string) ([]poolAccount, error) {
	var decoded []poolAccountJSON
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("渠道 %d 的账号池不是合法的 JSON 数组: %w", channelID, err)
	}
	out := make([]poolAccount, 0, len(decoded))
	for _, acc := range decoded {
		envelope, err := base64.StdEncoding.DecodeString(acc.Enc)
		if err != nil {
			return nil, fmt.Errorf("渠道 %d 的账号 %d 的凭证密文不是合法的 base64: %w",
				channelID, acc.ID, err)
		}
		account := poolAccount{
			ID:                  acc.ID,
			Name:                acc.Name,
			Weight:              acc.Weight,
			Envelope:            envelope,
			Health:              acc.Health,
			ConsecutiveFailures: acc.Failures,
		}
		if acc.LastFailure != nil {
			at := time.Unix(int64(*acc.LastFailure), 0)
			account.LastFailureAt = &at
		}
		out = append(out, account)
	}
	return out, nil
}

// usable 判断这个账号此刻参不参选。
//
// 三条判据，各自的「不好」不一样：
//
//   - 权重 0：运维明确写了「先别用」（迁移 00067 允许 0）。它不是故障，
//     不要靠冷却期去恢复它：这一条只在权重被改回非 0 之后才放行
//     （`account update --channel … --weight N`，见 Admin.SetAccountWeight），
//     等再久也不会自己变。
//   - drained：凭证被上游拒绝，或连续失败到了阈值。带冷却期：距最后一次失败
//     满一个 streakCooldown 之后重新参选一次。没有这条出口的话，被摘掉的账号
//     没有流量，状态永远不会自己变回去——与渠道侧 streakCooldown 同一条理由。
//   - degraded / healthy / unknown：都参选。degraded 只是「最近不太顺」，
//     把它一起挡掉等于让一个偶发失败的账号在冷却期里完全消失。
//
// drained 没有 last_failure_at 的行按「永远摘着」处理：那种行只可能来自手工
// 写入（写入方总是两列一起写），而按已恢复处理会让一个坏 key 每 15 分钟被
// 打扰一次的操作变成每一轮选路都落到它上面。
func (a poolAccount) usable(now time.Time) bool {
	return accountUsable(a.Weight, a.Health, a.LastFailureAt, now)
}

// accountUsable 是「这个账号此刻参不参选」的唯一判据。
//
// 选路（poolAccount）与健康展示（AccountHealth）共用它：两边各写一份时，
// 「命令行上说它可用、选路却跳过它」这种不一致没有任何一步会失败。
func accountUsable(weight int, health string, lastFailureAt *time.Time, now time.Time) bool {
	if weight <= 0 {
		return false
	}
	if health != AccountHealthDrained {
		return true
	}
	if lastFailureAt == nil {
		return false
	}
	return now.Sub(*lastFailureAt) >= streakCooldown
}

// usablePool 从池子里挑出此刻参选的账号，保持输入顺序（按账号 id 升序）。
func usablePool(accounts []poolAccount, now time.Time) []poolAccount {
	out := make([]poolAccount, 0, len(accounts))
	for _, acc := range accounts {
		if acc.usable(now) {
			out = append(out, acc)
		}
	}
	return out
}

// accountRotation 是进程内的平滑加权轮询状态。
//
// # 为什么状态在进程里、不在库里
//
// 每次选路往库里加一次「这个账号被挑中了一次」的写，是在热路径上多一次写操作，
// 而它换来的是跨进程精确的轮询比例——本仓的部署形态不需要这个精度：每个进程
// 各自按权重轮询，流量整体上仍然按权重分摊。
//
// # 状态按渠道分开
//
// 轮询比例说的是「这条渠道的池子里，谁被挑中多少次」。同一份账号挂在两条渠道上
// 时，两条渠道各转各的——权重本来就挂在关联上（见迁移 00067），两条渠道上可以
// 不同，合并成一个计数器会让「这条渠道上 2:1」变成两条渠道共同的某个比例。
//
// # 账号进出池子时
//
// 不在本次池子里的账号，它的累积值被丢掉（见 rotate）：一个被摘掉又回来的账号
// 从 0 开始，而不是带着摘掉之前的欠账回来。新账号同样从 0 开始。
type accountRotation struct {
	mu      sync.Mutex
	current map[int64]map[int64]int
}

// forget 丢掉一个渠道的轮询状态。
//
// 池子空了（渠道被删、账号全被摘掉）时调用：留着的那份累积值没有读者，
// 而这张表没有别的清理时机。账号回来时从 0 开始——见 rotate 的「账号进出池子」。
func (r *accountRotation) forget(channelID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.current, channelID)
}

// rotate 按平滑加权轮询给出这批账号的尝试顺序，并推进一次计数。
//
// 返回的顺序用于两件事：第一个是这次选中的账号；后面的留着——第一个账号的密文
// 解不开时依次往后试，一次解不开不代表池子里别的也解不开。
//
// # 只有第一名按权重轮出来
//
// 累积结构只服务**每一次选中的那一个**：把权重加到每个账号的累积值上，挑最大的
// 出来，然后只从它身上减去总和。剩下那几个是按输入顺序跟在后头的备选，不再各自
// 减总和平局——那样做时每个账号每轮都被减掉一次总和，差值会越拉越大，最重的那个
// 于是永远排第一（2:1 的池子会变成每次都是 A），权重就白设了。
//
// # 为什么是平滑加权轮询
//
// 朴素加权轮询（按权重铺成 1,1,2,1,1,2…）在权重差很大时会把高权重账号连续
// 排上很多次，池子小的时候看起来像流量一阵一阵地偏。平滑版本每次只看累积值，
// 2:1 的池子给出 A,B,A,A,B,A 这样均匀交错的顺序。
//
// # 等权时先挑谁
//
// 累积值相等时取输入顺序里靠前的那个，而输入按账号 id 升序——同一份配置每次
// 选到同一个首选项，排查时「这条渠道用的是哪把 key」因此可复现。
func (r *accountRotation) rotate(channelID int64, accounts []poolAccount) []poolAccount {
	if len(accounts) == 0 {
		r.forget(channelID)
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		r.current = make(map[int64]map[int64]int)
	}
	current := r.current[channelID]
	if current == nil {
		current = make(map[int64]int)
		r.current[channelID] = current
	}

	// 丢掉不在本次池子里的账号：被摘掉又回来的从 0 开始。
	present := make(map[int64]bool, len(accounts))
	total := 0
	for _, acc := range accounts {
		present[acc.ID] = true
		weight := acc.Weight
		if weight <= 0 {
			weight = 1
		}
		current[acc.ID] += weight
		total += weight
	}
	for id := range current {
		if !present[id] {
			delete(current, id)
		}
	}

	best := 0
	for i := 1; i < len(accounts); i++ {
		if current[accounts[i].ID] > current[accounts[best].ID] {
			best = i
		}
	}
	current[accounts[best].ID] -= total

	ordered := make([]poolAccount, 0, len(accounts))
	ordered = append(ordered, accounts[best])
	ordered = append(ordered, accounts[:best]...)
	ordered = append(ordered, accounts[best+1:]...)
	return ordered
}
