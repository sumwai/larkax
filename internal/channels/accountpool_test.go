package channels

import (
	"encoding/base64"
	"sync"
	"testing"
	"time"
)

// TestAccountUsable 钉住「这个账号此刻参不参选」的四条判据。
//
// 它是选路与 `channel health` 共用的那一份（见 accountUsable），所以每一条都
// 要能单独红：判据写岔时症状是「命令行说它可用，选路却一直跳过它」，或者反过来
// ——那条坏 key 被反复调用。两种都不会报错。
func TestAccountUsable(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	justFailed := now.Add(-time.Minute)
	cooledDown := now.Add(-streakCooldown)
	notYet := now.Add(-streakCooldown + time.Second)

	cases := []struct {
		name     string
		weight   int
		health   string
		lastFail *time.Time
		want     bool
	}{
		{"权重 1 正常", 1, AccountHealthHealthy, nil, true},
		{"权重 2 正常", 2, AccountHealthHealthy, nil, true},
		// 权重 0 是运维写的「先别用」：它不是故障，也不该靠冷却期恢复。
		{"权重 0", 0, AccountHealthHealthy, nil, false},
		{"权重为负", -1, AccountHealthHealthy, nil, false},
		{"没被调用过", 1, AccountHealthUnknown, nil, true},
		// degraded 只是「最近不太顺」：挡掉它等于让偶发失败的账号整段冷却期消失。
		{"degraded 仍参选", 1, AccountHealthDegraded, &justFailed, true},
		// drained 带冷却期：刚失败的不参选，满一个冷却期之后重新试一次。
		{"刚被摘掉", 1, AccountHealthDrained, &justFailed, false},
		{"冷却期差一秒", 1, AccountHealthDrained, &notYet, false},
		{"冷却期已满", 1, AccountHealthDrained, &cooledDown, true},
		// 没有 last_failure_at 的 drained 只可能来自手工写入：按「永远摘着」处理，
		// 否则一把坏 key 会被每一轮选路都碰到。
		{"drained 没有失败时刻", 1, AccountHealthDrained, nil, false},
		{"权重 0 盖过冷却期", 0, AccountHealthDrained, &cooledDown, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := accountUsable(tc.weight, tc.health, tc.lastFail, now)
			if got != tc.want {
				t.Errorf("accountUsable(weight=%d, health=%s) = %v，期望 %v",
					tc.weight, tc.health, got, tc.want)
			}
		})
	}
}

// TestUsablePoolKeepsInputOrder 钉住过滤后的顺序。
//
// 顺序不是无关紧要的：轮询在累积值相等时取输入里靠前的那个，而输入按账号 id
// 升序（候选查询的 ORDER BY）。这里乱序会让「同权重时先挑谁」不可复现。
func TestUsablePoolKeepsInputOrder(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * streakCooldown)
	accounts := []poolAccount{
		{ID: 3, Weight: 1, Health: AccountHealthHealthy},
		{ID: 1, Weight: 0, Health: AccountHealthHealthy},
		{ID: 2, Weight: 1, Health: AccountHealthDrained, LastFailureAt: &old},
		{ID: 4, Weight: 1, Health: AccountHealthDrained, LastFailureAt: &old},
	}

	got := usablePool(accounts, now)
	want := []int64{3, 2, 4}
	if len(got) != len(want) {
		t.Fatalf("留下 %d 个账号，期望 %d 个", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("第 %d 个账号是 %d，期望 %d", i, got[i].ID, id)
		}
	}
}

// TestRotationFollowsWeights 钉住加权轮询的比例与顺序。
//
// 判据是**选中次数**（每次的第一个）而不是返回长度：两者在实现写岔时恰好会
// 分岔——把总和对每个账号都减一遍的实现会让最重的那个永远排第一，而返回长度
// 同样是池子大小。
func TestRotationFollowsWeights(t *testing.T) {
	pool := []poolAccount{
		{ID: 1, Name: "main", Weight: 2, Health: AccountHealthHealthy},
		{ID: 2, Name: "backup", Weight: 1, Health: AccountHealthHealthy},
	}
	var r accountRotation

	// 前六次的选中顺序：2:1 的平滑轮询给出 A,B,A,A,B,A。逐次钉死比只数比例更严
	// ——只数比例时，「先连挑三次再补两次」也能凑出 4:2。
	want := []int64{1, 2, 1, 1, 2, 1}
	for i, id := range want {
		ordered := r.rotate(7, pool)
		if len(ordered) != len(pool) {
			t.Fatalf("第 %d 次返回 %d 个账号，期望 %d 个", i+1, len(ordered), len(pool))
		}
		if ordered[0].ID != id {
			t.Fatalf("第 %d 次选中账号 %d，期望 %d", i+1, ordered[0].ID, id)
		}
		// 备选按输入顺序跟着：第一个解不开时依次往后试。被选中的那个从队列里
		// 拿掉之后，剩下的相对顺序不变。
		wantFallback := make([]int64, 0, len(pool)-1)
		for _, acc := range pool {
			if acc.ID != id {
				wantFallback = append(wantFallback, acc.ID)
			}
		}
		for j, fallback := range wantFallback {
			if got := ordered[j+1].ID; got != fallback {
				t.Errorf("第 %d 次的第 %d 个备选是 %d，期望 %d",
					i+1, j+1, got, fallback)
			}
		}
	}

	// 长跑一次对账：比例必须稳在权重上，而不是前六次碰巧对。
	counts := map[int64]int{}
	for range 60 {
		counts[r.rotate(7, pool)[0].ID]++
	}
	if counts[1] != 40 || counts[2] != 20 {
		t.Errorf("60 次里选中次数 = %v，期望 2:1（40:20）", counts)
	}
}

// TestRotationIsPerChannel 钉住「同一份账号挂两条渠道各转各的」。
//
// 两条渠道的权重可能不同（权重挂在关联上），合成一个计数器会让「这条渠道上
// 2:1」变成两条渠道共同的某个比例。
func TestRotationIsPerChannel(t *testing.T) {
	pool := []poolAccount{
		{ID: 1, Weight: 2, Health: AccountHealthHealthy},
		{ID: 2, Weight: 1, Health: AccountHealthHealthy},
	}
	var r accountRotation

	// 渠道 7 先走一步：累积表里留下「1 号 -1、2 号 1」。
	//
	// 一步就够，而且必须是一步：走两步之后 7 号的状态是「1 号 1、2 号 -1」，
	// 共用一张表也照样先挑到 1 号，这个变异就照不出来。
	r.rotate(7, pool)
	// 渠道 8 从头开始。共用一张表时它会接着用 7 号那份累积值：加完权重
	// 变成 1 号 1、2 号 2，第一次就会挑 2 号——而它自己从零开始时，权重大的
	// 1 号先上。
	if got := r.rotate(8, pool)[0].ID; got != 1 {
		t.Errorf("渠道 8 的第一次选中账号 %d，期望 1（从零开始，权重大的先上）", got)
	}
	// 反过来，8 号那一步也不该推进 7 号：7 号的第二次仍然按它自己的 2:1
	// 序列挑 2 号（共用时那一步会把状态推到「1 号 1、2 号 -1」，第二次挑 1 号）。
	if got := r.rotate(7, pool)[0].ID; got != 2 {
		t.Errorf("渠道 7 的第二次选中账号 %d，期望 2（8 号那一步不该推进它的累积值）", got)
	}
}

// TestRotationRestartsAfterAbsence 钉住「被摘掉又回来的账号从 0 开始」。
//
// 带着摘掉之前的欠账回来会让它一回到池子就连续被选中很多次——那是把冷却期里
// 本该分给别人的流量一次补给它。
//
// 场景必须让它**先跑过一轮**再离开：从没进过累积表的账号本来就等于从 0 开始，
// 拿「它一直不在池子里」当输入时，「不清理池外账号的累积值」这个变异照样绿
// ——那种输入里它根本没有累积值可清。
func TestRotationRestartsAfterAbsence(t *testing.T) {
	pool := []poolAccount{
		{ID: 1, Weight: 1, Health: AccountHealthHealthy},
		{ID: 2, Weight: 3, Health: AccountHealthHealthy},
	}
	var r accountRotation

	// 第一轮选中 2 号，累积表里留下 1 号 = 1、2 号 = -1。
	if got := r.rotate(7, pool)[0].ID; got != 2 {
		t.Fatalf("第一轮选中账号 %d，期望 2（权重 3）", got)
	}
	// 1 号离开池子，只剩 2 号跑三轮：它的累积值在 -1 上原地打转。
	for range 3 {
		if got := r.rotate(7, pool[1:])[0].ID; got != 2 {
			t.Fatalf("池里只剩 2 号时选中了 %d", got)
		}
	}
	// 1 号回来。它的欠账被清掉时累积表是 1 号 = 0、2 号 = -1，加上权重后
	// 2 号（2）大过 1 号（1），选 2 号；不清的话 1 号停在 1，加权重后与 2 号
	// 打平，按「平局取输入靠前」会选 1 号——两种结果的第一次就不一样。
	if got := r.rotate(7, pool)[0].ID; got != 2 {
		t.Errorf("回来的池子第一次选中账号 %d，期望 2（1 号的旧累积值没有被丢掉）", got)
	}
}

// TestRotationEmptyPool 钉住空池不 panic。
func TestRotationEmptyPool(t *testing.T) {
	var r accountRotation
	if got := r.rotate(7, nil); got != nil {
		t.Errorf("空池应当返回 nil，得到 %v", got)
	}
}

// TestRotationConcurrent 让轮询在并发下跑一遍（配合 -race 才有意义）。
//
// 累积值是一张共享的表：少了那把锁时，症状是计数错乱（比例偏）或 map 并发写
// 直接让进程崩掉，两种都不会在这条用例之外的地方报出来。
func TestRotationConcurrent(t *testing.T) {
	pool := []poolAccount{
		{ID: 1, Weight: 1, Health: AccountHealthHealthy},
		{ID: 2, Weight: 1, Health: AccountHealthHealthy},
		{ID: 3, Weight: 1, Health: AccountHealthHealthy},
	}
	var r accountRotation
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got := r.rotate(7, pool); len(got) != len(pool) {
					t.Errorf("并发轮询返回 %d 个账号，期望 %d 个", len(got), len(pool))
					return
				}
			}
		}()
	}
	wg.Wait()

	// 等权三账号：2400 次里每个该被选中 800 次。
	counts := map[int64]int{}
	for range 2400 {
		counts[r.rotate(7, pool)[0].ID]++
	}
	for _, id := range []int64{1, 2, 3} {
		if counts[id] != 800 {
			t.Errorf("账号 %d 被选中 %d 次，期望 800 次（总数 %v）", id, counts[id], counts)
		}
	}
}

// TestDecodePoolAccounts 钉住候选查询那一列 JSON 的解析。
//
// 池子是热路径上的一列：解坏了要么整条渠道被跳过（静默），要么整次选路报错。
// 这里要的是后者，所以坏数据必须返回错误而不是零值池子。
func TestDecodePoolAccounts(t *testing.T) {
	failedAt := int64(1_761_000_000)
	envelope := base64.StdEncoding.EncodeToString([]byte("ciphertext"))

	t.Run("正常解出", func(t *testing.T) {
		raw := `[{"id":1,"name":"main","weight":2,"enc":"` + envelope + `",` +
			`"health":"healthy","failures":0,"last_failure_at":null},` +
			`{"id":2,"name":"backup","weight":1,"enc":"` + envelope + `",` +
			`"health":"drained","failures":3,"last_failure_at":` +
			itoa(failedAt) + `}]`
		got, err := decodePoolAccounts(7, raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("解出 %d 个账号，期望 2 个", len(got))
		}
		first := got[0]
		if first.ID != 1 || first.Name != "main" || first.Weight != 2 {
			t.Errorf("第一个账号 = %+v", first)
		}
		if string(first.Envelope) != "ciphertext" {
			t.Errorf("密文解回来是 %q，期望 %q", first.Envelope, "ciphertext")
		}
		if first.Health != AccountHealthHealthy || first.LastFailureAt != nil {
			t.Errorf("第一个账号的健康读数 = %+v", first)
		}
		second := got[1]
		if second.ConsecutiveFailures != 3 {
			t.Errorf("第二个账号的连败计数 = %d，期望 3", second.ConsecutiveFailures)
		}
		if second.LastFailureAt == nil ||
			second.LastFailureAt.Unix() != failedAt {
			t.Errorf("第二个账号的失败时刻 = %v，期望 %d", second.LastFailureAt, failedAt)
		}
	})

	t.Run("不是 JSON 数组", func(t *testing.T) {
		if _, err := decodePoolAccounts(7, `{"id":1}`); err == nil {
			t.Fatal("期望报错，却通过了")
		}
	})

	t.Run("密文不是 base64", func(t *testing.T) {
		raw := `[{"id":1,"name":"main","weight":1,"enc":"%%%",` +
			`"health":"healthy","failures":0,"last_failure_at":null}]`
		if _, err := decodePoolAccounts(7, raw); err == nil {
			t.Fatal("期望报错，却通过了")
		}
	})
}

// TestChannelHealthPoolState 钉住凭证池那四种整体状态。
//
// 「全挂」与「空池」对流量是同一个后果（选路跳过），但补救动作不同——前者去换
// key 或等冷却期，后者去挂账号。两者在这里必须是不同的值。
func TestChannelHealthPoolState(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	justFailed := now.Add(-time.Minute)
	cooledDown := now.Add(-2 * streakCooldown)

	cases := []struct {
		name     string
		accounts []AccountHealth
		want     string
		usable   int
	}{
		{"一个都没挂", nil, PoolStateEmpty, 0},
		{
			"全被摘了",
			[]AccountHealth{
				{ID: 1, Weight: 1, Health: AccountHealthDrained, LastFailureAt: &justFailed},
				{ID: 2, Weight: 1, Health: AccountHealthDrained, LastFailureAt: &justFailed},
			},
			PoolStateDrained, 0,
		},
		{
			"权重全 0 也算全挂",
			[]AccountHealth{{ID: 1, Weight: 0, Health: AccountHealthHealthy}},
			PoolStateDrained, 0,
		},
		{
			"还有能用的",
			[]AccountHealth{
				{ID: 1, Weight: 1, Health: AccountHealthHealthy},
				{ID: 2, Weight: 1, Health: AccountHealthDrained, LastFailureAt: &justFailed},
			},
			PoolStateDegraded, 1,
		},
		{
			"冷却期过了都回来",
			[]AccountHealth{
				{ID: 1, Weight: 1, Health: AccountHealthHealthy},
				{ID: 2, Weight: 1, Health: AccountHealthDrained, LastFailureAt: &cooledDown},
			},
			PoolStateHealthy, 2,
		},
		{
			"degraded 仍算可用",
			[]AccountHealth{{ID: 1, Weight: 1, Health: AccountHealthDegraded}},
			PoolStateHealthy, 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ChannelHealth{Accounts: tc.accounts}
			if got := h.PoolState(now); got != tc.want {
				t.Errorf("PoolState() = %q，期望 %q", got, tc.want)
			}
			if got := h.UsableAccounts(now); got != tc.usable {
				t.Errorf("UsableAccounts() = %d，期望 %d", got, tc.usable)
			}
		})
	}
}

// TestDecodeAccountHealth 钉住健康那条查询带出来的账号池。
//
// 与候选查询那一条（decodePoolAccounts）分开测：这一边没有密文，多的是结论，
// 而且空健康串要折成 unknown——SQL 里只写 COALESCE(s.health, ”)，健康那套词
// 归 Go 侧一份常量。
func TestDecodeAccountHealth(t *testing.T) {
	raw := `[{"id":1,"name":"main","weight":2,"health":"healthy",` +
		`"failures":0,"last_failure_at":null,"conclusion":"成功"},` +
		`{"id":2,"name":"backup","weight":1,"health":"",` +
		`"failures":0,"last_failure_at":null,"conclusion":""}]`

	got, err := decodeAccountHealth(7, raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("解出 %d 个账号，期望 2 个", len(got))
	}
	if got[0].Name != "main" || got[0].Weight != 2 || got[0].Conclusion != "成功" {
		t.Errorf("第一个账号 = %+v", got[0])
	}
	if got[1].Health != AccountHealthUnknown {
		t.Errorf("没有状态行的账号健康 = %q，期望 %q", got[1].Health, AccountHealthUnknown)
	}
	if got[1].Conclusion != "" {
		t.Errorf("没有状态行的账号结论 = %q，期望空串", got[1].Conclusion)
	}

	if _, err := decodeAccountHealth(7, "not json"); err == nil {
		t.Fatal("坏数据应当报错，却通过了")
	}
}

// TestDecodeAccountHealthEmptyArray 钉住「一个账号都没挂」的形态。
//
// SQL 侧是 COALESCE(json_agg(...), '[]')：空池返回空数组而不是 NULL，否则
// 这一列在 NULL 与 "[]" 之间有两套读法。
func TestDecodeAccountHealthEmptyArray(t *testing.T) {
	got, err := decodeAccountHealth(7, "[]")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空池解出 %d 个账号", len(got))
	}
}
