package channels

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/route"
)

// ── 凭证池的夹具 ────────────────────────────────────────────────────────

// attachPoolAccount 给一条渠道挂一个指定权重与明文的账号，返回 id 与名字。
//
// 与 attachAccount 分开：那一条不管权重（默认 1），而权重是这里被测的东西
// ——用例要能一眼看出「这条挂了 2、那条挂了 1」。
func attachPoolAccount(ctx context.Context, t *testing.T, db *sql.DB,
	sealer *credential.Sealer, channelID int64, plaintext string, weight int,
) (id int64, name string) {
	t.Helper()
	sealed, err := sealer.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("封装凭证失败: %v", err)
	}
	name = "acc-" + itoa(nextID())
	if err := db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, credential_enc) VALUES ($1, $2) RETURNING id`,
		name, sealed).Scan(&id); err != nil {
		t.Fatalf("插入账号失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_accounts (channel_id, account_id, weight) VALUES ($1, $2, $3)`,
		channelID, id, weight); err != nil {
		t.Fatalf("把账号挂到渠道上失败: %v", err)
	}
	return id, name
}

// insertChannelWithoutAccounts 建一条没有任何账号的渠道。
//
// 零账号是合法的中间态（迁移 00067 之后渠道行与账号行是两张表），选路跳过它。
func insertChannelWithoutAccounts(ctx context.Context, t *testing.T, db *sql.DB, model string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO channels (name, type, base_url, status, models, weight, priority, setting)
		VALUES ($1, 'openai-compatible', 'https://api.example.com/v1', 'enabled',
		        $2, 1, 1, '{}'::jsonb)
		RETURNING id`,
		"ch-"+itoa(nextID()), []string{model}).Scan(&id)
	if err != nil {
		t.Fatalf("插入无账号渠道失败: %v", err)
	}
	return id
}

// accountStateRow 是一条 account_state 行在用例里的投影。
type accountStateRow struct {
	health      string
	failures    int
	lastFailure sql.NullTime
	conclusion  string
}

// readAccountState 读一个账号的状态行；没有行时 found 为 false。
func readAccountState(t *testing.T, db *sql.DB, accountID int64) (row accountStateRow, found bool) {
	t.Helper()
	err := db.QueryRowContext(context.Background(), `
		SELECT health, consecutive_failures, last_failure_at, last_conclusion
		  FROM account_state WHERE account_id = $1`, accountID).
		Scan(&row.health, &row.failures, &row.lastFailure, &row.conclusion)
	if errors.Is(err, sql.ErrNoRows) {
		return accountStateRow{}, false
	}
	if err != nil {
		t.Fatalf("读账号状态失败: %v", err)
	}
	return row, true
}

// countCandidatesFor 数出某个渠道在候选里出现了几次（0 或 1）。
func countCandidatesFor(cands []route.Candidate, channelID int64) int {
	n := 0
	for _, c := range cands {
		if c.Channel.ID == channelID {
			n++
		}
	}
	return n
}

// ── 选路：按权重挑账号 ──────────────────────────────────────────────────

// TestCandidatesRotateAccountsByWeight 钉住「一条渠道挂多个账号时按权重轮询」。
//
// 判据是 40 次调用里两个账号各被选中多少次，以及**每次选中的账号的明文凭证
// 真的跟着换了**——只数计数的话，「选中了 B 却还拿着 A 的 key」这种错看不出来，
// 而它会让两个 key 的额度全部记到其中一个上。
func TestCandidatesRotateAccountsByWeight(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	mainID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "main-key", 1)
	backupID, backupName := attachPoolAccount(t.Context(), t, db, sealer, channelID, "backup-key", 3)

	src := New(db, sealer)
	counts := map[int64]int{}
	for i := 0; i < 40; i++ {
		cands, err := src.Candidates(context.Background(), model)
		if err != nil {
			t.Fatalf("第 %d 次 Candidates 出错: %v", i+1, err)
		}
		if len(cands) != 1 {
			t.Fatalf("第 %d 次候选数 = %d，期望 1", i+1, len(cands))
		}
		c := cands[0]
		counts[c.AccountID]++
		// 第一次就该是权重大的那个。比例对得上不代表顺序对：40 次正好是
		// 1:3 的 10 个整周期，相位整个错开一格时计数仍然是 10:30——只看
		// 计数分不出它，这里单独钉第一次。
		if i == 0 && c.AccountID != backupID {
			t.Fatalf("第一次选中账号 %d，期望权重大的 %d", c.AccountID, backupID)
		}
		// 凭证必须是**被选中的那个账号**的：这是「按账号解密」这条链路的
		// 全部意义，而它对调错了在计数上完全看不出来。
		want := map[int64]string{mainID: "main-key", backupID: "backup-key"}[c.AccountID]
		if c.Channel.Credential != want {
			t.Fatalf("第 %d 次选中账号 %d，凭证却是 %q，期望 %q",
				i+1, c.AccountID, c.Channel.Credential, want)
		}
		if c.AccountID == backupID && c.AccountName != backupName {
			t.Errorf("账号名 = %q，期望 %q", c.AccountName, backupName)
		}
	}

	// 1:3 的平滑轮询在 40 次里正好是 10:30，且从权重大的那个开始。
	if counts[mainID] != 10 || counts[backupID] != 30 {
		t.Errorf("选中次数 = %v，期望 1:3（10:30）", counts)
	}
}

// TestCandidatesSkipsDrainedAccount 钉住「一个账号被摘掉，流量全落到剩下的」。
func TestCandidatesSkipsDrainedAccount(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	deadID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "dead-key", 1)
	liveID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "live-key", 1)

	// 刚被上游拒绝：冷却期没过，不参选。
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO account_state (account_id, health, consecutive_failures, last_failure_at, last_conclusion)
		VALUES ($1, 'drained', 1, now(), '凭证被上游拒绝')`, deadID); err != nil {
		t.Fatalf("写入账号状态失败: %v", err)
	}

	src := New(db, sealer)
	for i := 0; i < 10; i++ {
		cands, err := src.Candidates(context.Background(), model)
		if err != nil {
			t.Fatalf("Candidates 出错: %v", err)
		}
		if len(cands) != 1 {
			t.Fatalf("候选数 = %d，期望 1", len(cands))
		}
		if cands[0].AccountID != liveID {
			t.Fatalf("选中账号 %d，期望 %d（另一个已被摘掉）", cands[0].AccountID, liveID)
		}
	}
}

// TestCandidatesDrainedAccountRecoversAfterCooldown 钉住「冷却期之后重新参选」。
//
// 没有这条出口时，被摘掉的账号没有流量，状态永远不会自己变回去——一把换好的
// key 会一直躺在池子外面。
func TestCandidatesDrainedAccountRecoversAfterCooldown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)

	// 冷却期已经过去：最后一次失败在一个冷却期之前。
	old := time.Now().Add(-2 * streakCooldown)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO account_state (account_id, health, consecutive_failures, last_failure_at, last_conclusion)
		VALUES ($1, 'drained', 3, $2, '凭证被上游拒绝')`, accountID, old); err != nil {
		t.Fatalf("写入账号状态失败: %v", err)
	}

	src := New(db, sealer)
	cands, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(cands) != 1 || cands[0].AccountID != accountID {
		t.Fatalf("冷却期过了之后应当重新参选，候选 = %+v", cands)
	}
}

// TestCandidatesSkipsChannelWhenPoolEmptyOrAllDrained 钉住「空池或全挂」。
//
// 两条渠道、同一个模型：一条挂着两个都被摘掉的账号，一条一个账号都没挂。
// 两者对流量是同一个后果——都不该出现在候选里。
func TestCandidatesSkipsChannelWhenPoolEmptyOrAllDrained(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	drainedChannel := insertChannelWithoutAccounts(t.Context(), t, db, model)
	for i := 0; i < 2; i++ {
		accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, drainedChannel, "key", 1)
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO account_state (account_id, health, consecutive_failures, last_failure_at)
			VALUES ($1, 'drained', 1, now())`, accountID); err != nil {
			t.Fatalf("写入账号状态失败: %v", err)
		}
	}
	emptyChannel := insertChannelWithoutAccounts(t.Context(), t, db, model)

	src := New(db, sealer)
	cands, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if n := countCandidatesFor(cands, drainedChannel); n != 0 {
		t.Errorf("全被摘掉的渠道出现了 %d 次，期望 0", n)
	}
	if n := countCandidatesFor(cands, emptyChannel); n != 0 {
		t.Errorf("零账号的渠道出现了 %d 次，期望 0", n)
	}
}

// TestCandidatesZeroWeightAccountNeverSelected 钉住权重 0 = 「先别用它」。
func TestCandidatesZeroWeightAccountNeverSelected(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	attachPoolAccount(t.Context(), t, db, sealer, channelID, "paused-key", 0)
	liveID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "live-key", 1)

	src := New(db, sealer)
	for i := 0; i < 10; i++ {
		cands, err := src.Candidates(context.Background(), model)
		if err != nil {
			t.Fatalf("Candidates 出错: %v", err)
		}
		if len(cands) != 1 || cands[0].AccountID != liveID {
			t.Fatalf("第 %d 次选中 %+v，期望只有账号 %d", i+1, cands, liveID)
		}
	}
}

// ── 记账：账号粒度的健康结论 ────────────────────────────────────────────

// TestRecordCallWritesAccountState 钉住一次调用怎么落到账号状态上。
//
// 六种结局各一条判据：成功回 healthy 并清零、普通失败记 degraded、上游拒绝
// 凭证当场 drained（不等阈值）、连败到阈值转 drained、失败之后成功回 healthy、
// 以及「客户端断开但上游已经拒了凭证」仍然 drained（见 accountState 的早退判据）。
func TestRecordCallWritesAccountState(t *testing.T) {
	cases := []struct {
		name       string
		outcomes   []route.CallOutcome
		wantHealth string
		wantFails  int
		wantConcl  string
	}{
		{
			name:       "一次成功",
			outcomes:   []route.CallOutcome{{Verdict: route.VerdictSuccess}},
			wantHealth: AccountHealthHealthy,
			wantFails:  0,
			wantConcl:  accountConclusionSuccess,
		},
		{
			name:       "一次普通失败",
			outcomes:   []route.CallOutcome{{Verdict: route.VerdictFailure}},
			wantHealth: AccountHealthDegraded,
			wantFails:  1,
			wantConcl:  accountConclusionFailure,
		},
		{
			name: "凭证被上游拒绝",
			outcomes: []route.CallOutcome{
				{Verdict: route.VerdictFailure, AccountDrained: true},
			},
			wantHealth: AccountHealthDrained,
			wantFails:  1,
			wantConcl:  accountConclusionDrained,
		},
		{
			// 客户端在上游回 401 的同时断开：渠道侧按「不归因」记（一次
			// 断连不该给渠道加连续失败），但「这把 key 被拒」是凭证自己的
			// 结论，账号仍然当场摘掉。
			name: "断连时上游已拒凭证",
			outcomes: []route.CallOutcome{
				{Verdict: route.VerdictUnattributed, AccountDrained: true},
			},
			wantHealth: AccountHealthDrained,
			wantFails:  1,
			wantConcl:  accountConclusionDrained,
		},
		{
			name: "连败到阈值",
			outcomes: []route.CallOutcome{
				{Verdict: route.VerdictFailure},
				{Verdict: route.VerdictFailure},
				{Verdict: route.VerdictFailure},
			},
			wantHealth: AccountHealthDrained,
			wantFails:  3,
			wantConcl:  accountConclusionFailure,
		},
		{
			name: "失败之后成功",
			outcomes: []route.CallOutcome{
				{Verdict: route.VerdictFailure},
				{Verdict: route.VerdictFailure},
				{Verdict: route.VerdictSuccess},
			},
			wantHealth: AccountHealthHealthy,
			wantFails:  0,
			wantConcl:  accountConclusionSuccess,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			sealer := testSealer(t)
			model := uniqueModel()
			channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
			accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)

			rec := NewRecorder(db, time.Minute)
			for _, outcome := range tc.outcomes {
				outcome.ChannelID = channelID
				outcome.AccountID = accountID
				outcome.Model = model
				outcome.At = time.Now()
				if err := rec.RecordCall(context.Background(), outcome); err != nil {
					t.Fatalf("RecordCall 出错: %v", err)
				}
			}

			row, found := readAccountState(t, db, accountID)
			if !found {
				t.Fatal("没有写出账号状态行")
			}
			if row.health != tc.wantHealth {
				t.Errorf("health = %q，期望 %q", row.health, tc.wantHealth)
			}
			if row.failures != tc.wantFails {
				t.Errorf("连续失败 = %d，期望 %d", row.failures, tc.wantFails)
			}
			if row.conclusion != tc.wantConcl {
				t.Errorf("结论 = %q，期望 %q", row.conclusion, tc.wantConcl)
			}
			// 成功之后不该留着失败时刻：它是 drained 冷却期的时钟，留着会让
			// 下一次查「这个账号多久没失败过」读到一个假的过去。
			if tc.wantHealth == AccountHealthHealthy && row.lastFailure.Valid {
				t.Errorf("成功之后 last_failure_at 仍有值：%v", row.lastFailure.Time)
			}
		})
	}
}

// TestRecordCallWithoutAccountSkipsAccountState 钉住「没有账号维度的调用不写账号」。
func TestRecordCallWithoutAccountSkipsAccountState(t *testing.T) {
	db := testDB(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)

	// 同一张库里还有别的用例写下的行，所以比的是**这一次调用前后**的差，
	// 不是「整张表是 0」。
	countRows := func() int {
		t.Helper()
		var rows int
		if err := db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM account_state`).Scan(&rows); err != nil {
			t.Fatalf("数账号状态行失败: %v", err)
		}
		return rows
	}
	before := countRows()

	rec := NewRecorder(db, time.Minute)
	if err := rec.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: channelID,
		Model:     model,
		At:        time.Now(),
		Verdict:   route.VerdictSuccess,
	}); err != nil {
		t.Fatalf("RecordCall 出错: %v", err)
	}

	if after := countRows(); after != before {
		t.Errorf("没有账号维度的调用写出了 %d 行账号状态，期望 0", after-before)
	}
}

// TestRecordCallAccountStateIgnoresUnattributed 钉住「不是渠道的失败也不动账号」。
//
// 客户端断开、请求本身有错的那一次既不该算成功，也不该让一个正常账号离池子
// 更近一步。
func TestRecordCallAccountStateIgnoresUnattributed(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)

	rec := NewRecorder(db, time.Minute)
	if err := rec.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: channelID,
		AccountID: accountID,
		Model:     model,
		At:        time.Now(),
		Verdict:   route.VerdictUnattributed,
	}); err != nil {
		t.Fatalf("RecordCall 出错: %v", err)
	}
	if _, found := readAccountState(t, db, accountID); found {
		t.Error("Unattributed 的调用不该写出账号状态行")
	}
}

// TestAccountStateCascadesWithAccountDeletion 钉住「删账号带走它的健康状态」。
//
// 状态行是按账号主键挂的（ON DELETE CASCADE）：留着它会让一个同名的下一个
// 账号继承上一把坏 key 的结论。
func TestAccountStateCascadesWithAccountDeletion(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO account_state (account_id, health, last_failure_at)
		VALUES ($1, 'drained', now())`, accountID); err != nil {
		t.Fatalf("写入账号状态失败: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`DELETE FROM accounts WHERE id = $1`, accountID); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}
	if _, found := readAccountState(t, db, accountID); found {
		t.Error("账号删掉之后状态行还在")
	}
}

// ── 摘账号 ──────────────────────────────────────────────────────────────

// TestRemoveAccountSingleChannel 钉住「只挂在一条渠道上时直接删」。
func TestRemoveAccountSingleChannel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	accountID, name := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)

	admin := NewAdmin(db, nil, nil)
	res, err := admin.RemoveAccount(context.Background(), name, false)
	if err != nil {
		t.Fatalf("RemoveAccount 出错: %v", err)
	}
	if res.Name != name {
		t.Errorf("结果里的名字 = %q，期望 %q", res.Name, name)
	}
	if len(res.Channels) != 1 || res.Channels[0].ID != channelID {
		t.Errorf("挂在的渠道 = %+v，期望只有 %d", res.Channels, channelID)
	}
	// 这条渠道本来就只有这一个账号：删完它一个都不剩，必须报出来。
	if len(res.EmptiedChannels) != 1 || res.EmptiedChannels[0].ID != channelID {
		t.Errorf("删完变空的渠道 = %+v，期望 %d", res.EmptiedChannels, channelID)
	}

	var rows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM accounts WHERE id = $1`, accountID).Scan(&rows); err != nil {
		t.Fatalf("数账号行失败: %v", err)
	}
	if rows != 0 {
		t.Error("账号行还在库里")
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channel_accounts WHERE channel_id = $1`, channelID).Scan(&rows); err != nil {
		t.Fatalf("数关联行失败: %v", err)
	}
	if rows != 0 {
		t.Error("关联行还在库里")
	}
}

// TestRemoveAccountRefusesWhenAttachedElsewhere 钉住「还挂在别的渠道上时要 --force」。
//
// 拒绝时要把渠道清单带回去：只说「还挂着」而不说是哪几条，运维还得自己再查
// 一遍，而那条查询正是这里刚刚做过的。
func TestRemoveAccountRefusesWhenAttachedElsewhere(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	first := insertChannelWithoutAccounts(t.Context(), t, db, model)
	second := insertChannelWithoutAccounts(t.Context(), t, db, model)

	// 同一个账号挂在两条渠道上：先建在 first 上，再挂到 second。
	accountID, name := attachPoolAccount(t.Context(), t, db, sealer, first, "shared-key", 1)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_accounts (channel_id, account_id, weight) VALUES ($1, $2, 1)`,
		second, accountID); err != nil {
		t.Fatalf("挂到第二条渠道上失败: %v", err)
	}

	admin := NewAdmin(db, nil, nil)
	_, err := admin.RemoveAccount(context.Background(), name, false)
	if !errors.Is(err, ErrAccountInUse) {
		t.Fatalf("期望 ErrAccountInUse，得到 %v", err)
	}
	var inUse *AccountInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("错误链里没有 AccountInUseError：%v", err)
	}
	if len(inUse.Channels) != 2 {
		t.Errorf("错误里带回了 %d 条渠道，期望 2 条：%v", len(inUse.Channels), inUse.Channels)
	}

	// 被拒绝之后账号与两条关联都还在：拒绝不是「删了一半」。
	var rows int
	if countErr := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channel_accounts WHERE account_id = $1`, accountID).Scan(&rows); countErr != nil {
		t.Fatalf("数关联行失败: %v", countErr)
	}
	if rows != 2 {
		t.Errorf("关联行 = %d，期望 2（拒绝之后不该动任何一行）", rows)
	}

	// 加 --force 之后照删，两条关联一起消失。
	res, err := admin.RemoveAccount(context.Background(), name, true)
	if err != nil {
		t.Fatalf("--force 时 RemoveAccount 出错: %v", err)
	}
	if len(res.Channels) != 2 {
		t.Errorf("结果里的渠道 = %+v，期望 2 条", res.Channels)
	}
	// 两条渠道上都没有别的账号：两条都该报成「删完变空」。
	if len(res.EmptiedChannels) != 2 {
		t.Errorf("删完变空的渠道 = %+v，期望 2 条", res.EmptiedChannels)
	}
}

// TestRemoveAccountKeepsOtherAccounts 钉住「变空的判定按删之前的池子算」。
//
// 一条渠道上两个账号、另一条只有一个：删掉共享的那个之后，第一条还剩一个
// 账号（不算变空），第二条才是空的。判据写反时（按删之后数）两条都会被报成
// 变空，而「有一条渠道因此下线」正是这个结果里最该准的那一句。
func TestRemoveAccountKeepsOtherAccounts(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	shared := insertChannelWithoutAccounts(t.Context(), t, db, model)
	solo := insertChannelWithoutAccounts(t.Context(), t, db, model)

	accountID, name := attachPoolAccount(t.Context(), t, db, sealer, shared, "shared-key", 1)
	attachPoolAccount(t.Context(), t, db, sealer, shared, "other-key", 1)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_accounts (channel_id, account_id, weight) VALUES ($1, $2, 1)`,
		solo, accountID); err != nil {
		t.Fatalf("挂到第二条渠道上失败: %v", err)
	}

	res, err := NewAdmin(db, nil, nil).RemoveAccount(context.Background(), name, true)
	if err != nil {
		t.Fatalf("RemoveAccount 出错: %v", err)
	}
	if len(res.EmptiedChannels) != 1 || res.EmptiedChannels[0].ID != solo {
		t.Errorf("删完变空的渠道 = %+v，期望只有 %d", res.EmptiedChannels, solo)
	}
}

// TestRemoveAccountNotFound 钉住按名字找不到时的哨兵错误。
//
// 它与 ErrAccountInUse 分开：一个要核对名字，一个要改命令加 --force。
func TestRemoveAccountNotFound(t *testing.T) {
	db := testDB(t)
	_, err := NewAdmin(db, nil, nil).RemoveAccount(context.Background(), "no-such-account-"+itoa(nextID()), false)
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("期望 ErrAccountNotFound，得到 %v", err)
	}
}

// TestRemoveAccountLeavesInflightRequestIntact 钉住「在飞请求不受影响」。
//
// 这正是「删除不等、也不打断」那句话的可验形态：候选在选出时**已经**解出了
// 明文凭证，删账号只影响之后的新请求。用例先取一份候选（模拟一次正在进行的
// 请求），再删账号，然后确认手里这份候选仍有可用的凭证。
func TestRemoveAccountLeavesInflightRequestIntact(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	_, name := attachPoolAccount(t.Context(), t, db, sealer, channelID, "in-flight-key", 1)

	src := New(db, sealer)
	cands, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(cands))
	}

	if _, delErr := NewAdmin(db, nil, nil).RemoveAccount(context.Background(), name, false); delErr != nil {
		t.Fatalf("RemoveAccount 出错: %v", delErr)
	}

	// 已经拿到手的候选不受影响：凭证在选路那一刻就解密并带在请求里了。
	if got := cands[0].Channel.Credential; got != "in-flight-key" {
		t.Errorf("在飞请求的凭证变成了 %q", got)
	}

	// 新的请求选不到它：渠道一个账号都不剩，整条退出候选。
	after, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("删除后 Candidates 出错: %v", err)
	}
	if n := countCandidatesFor(after, channelID); n != 0 {
		t.Errorf("删掉唯一的账号之后，渠道仍在候选里出现 %d 次", n)
	}
}

// TestUpdateAccountClearsHealth 钉住「换 key 之后账号立刻回到池子」。
//
// 旧 key 攒下的 drained 是旧 key 的问题：不清掉的话，换好的 key 要白等一个
// 冷却期才参选，而那段时间流量全压在其他账号上。
func TestUpdateAccountClearsHealth(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	accountID, name := attachPoolAccount(t.Context(), t, db, sealer, channelID, "old-key", 1)

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO account_state (account_id, health, consecutive_failures, last_failure_at)
		VALUES ($1, 'drained', 3, now())`, accountID); err != nil {
		t.Fatalf("写入账号状态失败: %v", err)
	}

	if _, err := NewAdmin(db, sealer, nil).UpdateAccount(context.Background(), name, "new-key"); err != nil {
		t.Fatalf("UpdateAccount 出错: %v", err)
	}
	if _, found := readAccountState(t, db, accountID); found {
		t.Error("换 key 之后旧的状态行还在")
	}

	src := New(db, sealer)
	cands, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(cands) != 1 || cands[0].Channel.Credential != "new-key" {
		t.Fatalf("换 key 之后应当立刻回到池子并解出新 key，候选 = %+v", cands)
	}
}

// ── channel health 的账号栏 ─────────────────────────────────────────────

// TestHealthReportsAccountPool 钉住健康读数里的凭证池。
//
// 渠道级 health 与账号级状态是两件事：这里要的是**账号那一栏**（谁、权重多少、
// 现在什么状态、最近一次结论），以及由它推算出来的池子状态。
func TestHealthReportsAccountPool(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
	freshID, freshName := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 2)
	deadID, deadName := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key2", 1)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO account_state (account_id, health, consecutive_failures, last_failure_at, last_conclusion)
		VALUES ($1, 'drained', 2, now(), '凭证被上游拒绝')`, deadID); err != nil {
		t.Fatalf("写入账号状态失败: %v", err)
	}

	rows, err := NewAdmin(db, nil, nil).Health(context.Background(), time.Now().Add(-time.Hour), false)
	if err != nil {
		t.Fatalf("Health 出错: %v", err)
	}
	var got *ChannelHealth
	for i := range rows {
		if rows[i].ID == channelID {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("健康读数里没有渠道 %d", channelID)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("账号 %d 个，期望 2 个", len(got.Accounts))
	}

	// 按账号 id 升序：两个账号的 id 顺序由插入顺序决定。
	first, second := got.Accounts[0], got.Accounts[1]
	if first.ID != freshID || first.Name != freshName || first.Weight != 2 {
		t.Errorf("第一个账号 = %+v，期望 %d/%s/权重 2", first, freshID, freshName)
	}
	if first.Health != AccountHealthUnknown {
		t.Errorf("没被调用过的账号健康 = %q，期望 %q", first.Health, AccountHealthUnknown)
	}
	if first.Conclusion != "" {
		t.Errorf("没被调用过的账号结论 = %q，期望空串", first.Conclusion)
	}
	if second.ID != deadID || second.Name != deadName {
		t.Errorf("第二个账号 = %+v，期望 %d/%s", second, deadID, deadName)
	}
	if second.Health != AccountHealthDrained || second.ConsecutiveFailures != 2 {
		t.Errorf("被摘掉的账号 = %+v", second)
	}
	if second.Conclusion != "凭证被上游拒绝" {
		t.Errorf("结论 = %q", second.Conclusion)
	}
	if second.LastFailureAt == nil {
		t.Error("被摘掉的账号没有失败时刻")
	}

	now := time.Now()
	if got.PoolState(now) != PoolStateDegraded {
		t.Errorf("池子状态 = %q，期望 %q", got.PoolState(now), PoolStateDegraded)
	}
	if n := got.UsableAccounts(now); n != 1 {
		t.Errorf("可用账号 = %d，期望 1", n)
	}
}

// TestHealthReportsEmptyPool 钉住「零账号的渠道照样读得出来」。
//
// 它是合法的中间态：渠道建得出来、在 channel list 里看得见，选路跳过它。
// 健康读数不能把它整行漏掉——那会让「这条渠道怎么了」这个问题无从提起。
func TestHealthReportsEmptyPool(t *testing.T) {
	db := testDB(t)
	model := uniqueModel()
	channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)

	rows, err := NewAdmin(db, nil, nil).Health(context.Background(), time.Now().Add(-time.Hour), false)
	if err != nil {
		t.Fatalf("Health 出错: %v", err)
	}
	var got *ChannelHealth
	for i := range rows {
		if rows[i].ID == channelID {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("健康读数里没有渠道 %d", channelID)
	}
	if len(got.Accounts) != 0 {
		t.Errorf("零账号渠道的账号栏 = %+v，期望空", got.Accounts)
	}
	if got.PoolState(time.Now()) != PoolStateEmpty {
		t.Errorf("池子状态 = %q，期望 %q", got.PoolState(time.Now()), PoolStateEmpty)
	}
}

// TestRecordCallAccountStateSurvivesChannelGone 钉住「渠道那一层写不进去、或者
// 渠道已经不在了，账号层的结论仍然要记」。
//
// 账号健康与渠道健康是两个信号：`account_state` 的外键只指向 accounts（见迁移
// 00068），所以渠道在调用中途被软删时它照样写得进去；而「这把 key 被上游拒了」
// 是凭证自己的结论，不该因为另一条语句超时就被丢掉——丢掉的方向是一条已知
// 无效的 key 继续接流量。
func TestRecordCallAccountStateSurvivesChannelGone(t *testing.T) {
	t.Run("渠道在调用中途被软删", func(t *testing.T) {
		db := testDB(t)
		sealer := testSealer(t)
		model := uniqueModel()
		channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
		accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)
		softDeleteRow(t, db, channelID)

		rec := NewRecorder(db, time.Minute)
		if err := rec.RecordCall(context.Background(), route.CallOutcome{
			ChannelID: channelID, AccountID: accountID, Model: model,
			At: time.Now(), Verdict: route.VerdictFailure, AccountDrained: true,
		}); err != nil {
			t.Fatalf("RecordCall 出错: %v", err)
		}

		row, found := readAccountState(t, db, accountID)
		if !found {
			t.Fatal("渠道已删时账号层被一起挡掉了：没有写出账号状态行")
		}
		if row.health != AccountHealthDrained {
			t.Errorf("health = %q，期望 %q", row.health, AccountHealthDrained)
		}
	})

	t.Run("连续失败那条语句失败", func(t *testing.T) {
		db := testDB(t)
		sealer := testSealer(t)
		model := uniqueModel()
		channelID := insertChannelWithoutAccounts(t.Context(), t, db, model)
		accountID, _ := attachPoolAccount(t.Context(), t, db, sealer, channelID, "key", 1)

		// 让 streak 那条 UPDATE 必定失败。用触发器而不是改库权限：本包的
		// 测试用户就是库的所有者，权限挡不住它。
		if _, err := db.ExecContext(context.Background(), `
			CREATE OR REPLACE FUNCTION record_deny_channels_update() RETURNS trigger
			LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'blocked by test'; END $$;
			CREATE TRIGGER record_deny_channels_update BEFORE UPDATE ON channels
			FOR EACH ROW EXECUTE FUNCTION record_deny_channels_update();`); err != nil {
			t.Fatalf("装触发器失败: %v", err)
		}
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(),
				`DROP TRIGGER IF EXISTS record_deny_channels_update ON channels;
				 DROP FUNCTION IF EXISTS record_deny_channels_update()`); err != nil {
				t.Errorf("拆触发器失败（后面的用例会被它挡住）: %v", err)
			}
		})

		rec := NewRecorder(db, time.Minute)
		err := rec.RecordCall(context.Background(), route.CallOutcome{
			ChannelID: channelID, AccountID: accountID, Model: model,
			At: time.Now(), Verdict: route.VerdictFailure, AccountDrained: true,
		})
		if err == nil {
			t.Fatal("连续失败那条写不进去时应当把错误交回调用方")
		}

		row, found := readAccountState(t, db, accountID)
		if !found {
			t.Fatal("streak 失败把账号层一起挡掉了：没有写出账号状态行")
		}
		if row.health != AccountHealthDrained {
			t.Errorf("health = %q，期望 %q", row.health, AccountHealthDrained)
		}
	})
}
