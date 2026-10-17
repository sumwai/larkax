package store

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

func reservation(t *testing.T, userID int64, requestID, amount string, asset string) *pipeline.Reservation {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	if asset == "" {
		asset = "CNY"
	}
	return &pipeline.Reservation{
		RequestID: requestID,
		UserID:    userID,
		Asset:     asset,
		Amount:    amt,
		// RuleMatchedAt 落库成 billing_requests.started_at。留零值的话那一行
		// 的时间戳是公元 1 年，而生产里它是**预留当时的墙钟**（pipeline 从
		// p.now() 取，见 pipeline.Reserve）——零值造出来的是库不可能出现的
		// 数据，对账里那条「pending 且 started_at 早于一天」的判据会把它
		// 一律算成陈旧。测试数据必须走同一条路，否则测的是一个假世界。
		//
		// 需要陈旧数据的用例自己把它拨回去（见 reconcile_test.go 的 reserveAt）。
		RuleMatchedAt: time.Now(),
	}
}

// reservationWithRule 造一条**带真实规则 id** 的预留。
//
// 默认那条不记规则（RuleID 为 0，落库成 NULL）：绝大多数用例关心的是
// 钱怎么动，不该为了满足外键先造一条规则。但规则快照是账单可解释性的
// 依据，所以单独有一条用例覆盖它能正确落库。
func reservationWithRule(t *testing.T, userID int64, requestID, amount string, ruleID int64) *pipeline.Reservation {
	t.Helper()
	r := reservation(t, userID, requestID, amount, "CNY")
	r.RuleID = ruleID
	r.PricingExprSnap = `{"prompt": 0.000001}`
	return r
}

// testRequest 造一条落库层会接受的计费请求。
//
// 它顺手给这个用户建一把 API Key：billing_requests.api_key_id 有外键，
// 而落库层拒绝漏设（见 ErrMissingAPIKey）——所以「这一列写没写对」只能在
// 一个真有 key 行的世界里断言。要指定**哪一把** key 用 testRequestWithKey。
//
// ctx 由调用方传进来，一路传到 seedAPIKey 的那条 INSERT（理由见那里的注释）：
// 本函数不造 ctx，也就不会把调用方的超时挡在外面。
func testRequest(ctx context.Context, t *testing.T, db *sql.DB, userID int64, requestID string) pipeline.Request {
	t.Helper()
	return testRequestWithKey(userID, requestID, seedAPIKey(ctx, t, db, userID))
}

// testRequestWithKey 与 testRequest 相同，但 key 由调用方给。
//
// 分成两个函数是为了让**并发用例**也能用：那些用例在 goroutine 里造请求，
// 而 seedAPIKey 会调 t.Fatalf——从测试 goroutine 之外的 goroutine 里调它
// 是不允许的，所以 key 要在起 goroutine 之前就建好。
func testRequestWithKey(userID int64, requestID string, keyID int64) pipeline.Request {
	return pipeline.Request{
		RequestID:      requestID,
		UserID:         userID,
		APIKeyID:       keyID,
		CanonicalModel: "gpt-4o",
		InputTokens:    1000,
		MaxTokens:      500,
		// 落库层拒绝空协议（见 ErrMissingProtocol），所以这里必须给一个。
		// 给 openai 而不是留空：留空会让每条用例都在测同一条报错路径，
		// 而真正要测的是「协议按给的值落库」——那由用例自己改这个字段。
		Protocol: channel.ProtocolOpenAI,
		// 上游模型名同理必须非空（见 ErrMissingUpstreamModel），且**故意与
		// 规范名不同**：两者相同时「落对了哪一列」分辨不出来，而这一列存在的
		// 全部意义就是记下与规范名不同的那个名字。
		UpstreamModel: "vendor/gpt-4o",
	}
}

// ── 预留 ────────────────────────────────────────────────────────────────

func TestSaveReservationFreezesFunds(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	res, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, reqID(t)), reservation(t, userID, reqID(t), "30", "CNY"))
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if res == 0 {
		t.Error("应返回预留记录 id")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("余额被改动了: %s", balance)
	}
	if frozen != "30.00000000" {
		t.Errorf("冻结 = %s，期望 30", frozen)
	}
}

// 预留**不动余额**，只动冻结。余额与冻结分开是「可用 = 余额 − 冻结」
// 这个判断的前提。
func TestReservationDoesNotTouchBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "50")

	req := testRequest(t.Context(), t, db, userID, reqID(t))
	if _, err := s.SaveReservation(context.Background(), req, reservation(t, userID, req.RequestID, "20", "CNY")); err != nil {
		t.Fatal(err)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "20.00000000" {
		t.Errorf("冻结 = %s", frozen)
	}
}

// 可用余额不足时预留失败，且**什么都没改**。
//
// 这里用的是「更新影响 0 行」这条判据，所以它同时验证了那条 SQL 的条件。
func TestReservationInsufficientBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	// 余额 10，已经冻了 8，再想冻 5 —— 可用只有 2
	userID := seed(t, db, "10")
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 8 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	_, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, reqID(t)), reservation(t, userID, reqID(t), "5", "CNY"))
	if err == nil {
		t.Fatal("可用余额不足应当失败")
	}
	if !errors.Is(err, ErrInsufficientAvailable) {
		t.Errorf("期望 ErrInsufficientAvailable，得到 %v", err)
	}

	// 冻结额没变
	if _, frozen := walletOf(t, db, userID); frozen != "8.00000000" {
		t.Errorf("失败时不该改动冻结额，得到 %s", frozen)
	}
	// 也没留下预留记录
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_requests WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("失败时不该留下预留记录，实际 %d 条", n)
	}
}

// 恰好够用应当成功——边界。
func TestReservationExactBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "10")

	req := testRequest(t.Context(), t, db, userID, reqID(t))
	if _, err := s.SaveReservation(context.Background(), req, reservation(t, userID, req.RequestID, "10", "CNY")); err != nil {
		t.Errorf("恰好够用应当成功: %v", err)
	}
}

// 没有钱包行时给出可区分的错误。
func TestReservationWithoutWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)

	// 建用户但不建钱包
	id := 900000 + nextID()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO users (id, username) VALUES ($1, $2)`, id, "u"+itoa(id)); err != nil {
		t.Fatal(err)
	}

	req := testRequest(t.Context(), t, db, id, reqID(t))
	_, err := s.SaveReservation(context.Background(), req, reservation(t, id, req.RequestID, "1", "CNY"))
	if err == nil {
		t.Fatal("没有钱包应当失败")
	}
	// 「用户不存在」与「钱不够」要能分开——处置完全不同
	if errors.Is(err, ErrInsufficientAvailable) {
		t.Errorf("应报告「没有钱包」而不是「余额不足」: %v", err)
	}
}

// 同一个 request_id 重复预留要被唯一键挡下。
func TestDuplicateReservationRejected(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	if _, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, requestID), reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, requestID), reservation(t, userID, requestID, "10", "CNY"))
	if err == nil {
		t.Fatal("重复的 request_id 应当被唯一键挡下")
	}
}

// 撞键的那次预留**整个回滚**：一分钱没冻，也没多出一行账单。
//
// # 这是「幂等记录与预留同一个事务」唯一能从外面看见的一面
//
// 把幂等记录挪到预留提交之后单独写，绝大多数行为**完全一样**——同键照样
// 只扣一次钱、照样重放。区别只在这一个窗口：撞键的请求先把钱冻住、再发现
// 自己没抢到键。两者不在一个事务里，那笔冻结就留下来了。
//
// 而它没有任何东西会报出来：账单停在 pending、余额与账本两边都自洽、
// 对账只看那两样。用户看到的是「额度莫名少了一笔」，直到某个超时把它解冻，
// 或者永远不解冻。
//
// # 为什么不用「余额不足」那条路来证明
//
// 那条路（余额 0 的用户）在两种写法下表现相同：预留插不进去就返回了，
// 根本走不到写幂等记录那一步。它能证明「记录没留下来」，但证不了
// 「两者在同一个事务里」——这正是这个用例存在的理由。
func TestClaimConflictRollsBackReservation(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	apiKeyID := seedAPIKey(ctx, t, db, userID)
	claim := &idempotency.Claim{
		APIKeyID: apiKeyID,
		Key:      "claim-rollback-" + itoa(nextID()),
		Hash:     [32]byte{0x11},
	}

	// 第一次：正常占住这个键。
	firstID := reqID(t)
	firstReq := testRequest(ctx, t, db, userID, firstID)
	firstReq.Idempotency = claim
	firstReservationID, err := s.SaveReservation(ctx, firstReq, reservation(t, userID, firstID, "10", "CNY"))
	if err != nil {
		t.Fatalf("第一次预留失败: %v", err)
	}
	_, frozenBefore := walletOf(t, db, userID)

	// 第二次：同一个键、另一次请求。它会先把钱冻住，再在写幂等记录时
	// 发现键已被占住。
	secondID := reqID(t)
	secondReq := testRequest(ctx, t, db, userID, secondID)
	secondReq.Idempotency = claim
	// 错误单独接一个名字，不遮蔽上面那个 err：遮蔽掉的话，这一行之后
	// 任何一处用到「第一次那个 err」都会静默地读到这一个。
	_, claimErr := s.SaveReservation(ctx, secondReq, reservation(t, userID, secondID, "10", "CNY"))
	if !errors.Is(claimErr, idempotency.ErrClaimed) {
		t.Fatalf("第二次预留的错误 = %v，期望 ErrClaimed——它是竞态信号，不是失败", claimErr)
	}

	if _, frozenAfter := walletOf(t, db, userID); frozenAfter != frozenBefore {
		t.Errorf("撞键后冻结额 = %s，期望仍为 %s——那次预留没有回滚，这笔钱白白冻着，"+
			"而账单停在 pending、账本与余额两边自洽，没有任何东西会报出来",
			frozenAfter, frozenBefore)
	}
	if n := countRequests(t, db, secondID); n != 0 {
		t.Errorf("撞键的请求留下了 %d 行账单，期望 0 行", n)
	}

	// 键仍然指着第一次那次预留：撞键的请求没能把它改指向自己。
	rec, err := idempotency.New(db).Lookup(ctx, apiKeyID, claim.Key)
	if err != nil {
		t.Fatalf("读幂等记录失败: %v", err)
	}
	if rec.BillingRequestID != firstReservationID {
		t.Errorf("幂等记录指向的预留 = %d，期望 %d（第一次那次）",
			rec.BillingRequestID, firstReservationID)
	}
	if rec.State != idempotency.StateInProgress {
		t.Errorf("撞键后记录状态 = %q，期望仍是 %q——撞键的那次什么都没做成",
			rec.State, idempotency.StateInProgress)
	}
}

// ── 结算 ────────────────────────────────────────────────────────────────

func TestSaveSettlementMovesMoney(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	// 实际只花了 12，比预留少
	entries := consumeEntries(t, userID, "12", billID)
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  entries,
	}
	charge.Settlement.Actual = money.MustParse("12")

	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "88.00000000" {
		t.Errorf("余额 = %s，期望 88（100 − 12）", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("预留应当被释放，冻结 = %s", frozen)
	}

	// 钱包余额必须等于账本推出来的——这是「快照是账本的具体化物」
	if got, want := balance, ledgerBalance(t, db, userID).String(); got != want {
		t.Errorf("钱包 %s 与账本 %s 不一致", got, want)
	}
}

// 结算要把**上游模型名**落进 billing_requests.upstream_model。
//
// 这一列与 channel_id 同一个位置：两者都只有选路之后才知道，所以预留那一行
// 必然是 NULL，值由结算补上。它也**必须**与 canonical_model 分开落——这一列
// 存在的全部意义就是记下「发出去的与用户要的不是同一个名字」，两者落成一样
// 时它一点信息都没有（见 #338）。
func TestSaveSettlementWritesUpstreamModel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if req.UpstreamModel == req.CanonicalModel {
		t.Fatalf("夹具的上游名与规范名相同（%q），这条用例分辨不出落错了哪一列", req.UpstreamModel)
	}

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	// 预留时还没选路，这一列必须留空——写一个「当时猜的」名字比留空更坏，
	// 因为它看起来像一个已经确定的事实。
	if got := upstreamModelOf(t, db, requestID); got.Valid {
		t.Errorf("预留时 upstream_model = %q，期望 NULL", got.String)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "12", billID),
	}
	charge.Settlement.Actual = money.MustParse("12")

	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	got := upstreamModelOf(t, db, requestID)
	if !got.Valid || got.String != req.UpstreamModel {
		t.Errorf("结算后 upstream_model = %+v，期望 %q", got, req.UpstreamModel)
	}
}

// 漏设上游模型名要**报错**，而不是往那一列写 NULL。
//
// 与 ErrMissingPricedAt 同一条理由：写入的那一刻是唯一还分得清「漏设」与
// 「历史行」的地方。这一列从建表那天起一直全是 NULL，所以事后从库里看，
// 「新代码漏传了」与「那行是接通之前留下的」完全一样——而前者意味着
// 「别名到底生效没有」这个问题又回到了无从查证的状态。
func TestSaveRejectsMissingUpstreamModel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "12", billID),
	}
	charge.Settlement.Actual = money.MustParse("12")

	req.UpstreamModel = ""
	if err := s.Save(ctx, req, charge); !errors.Is(err, ErrMissingUpstreamModel) {
		t.Fatalf("漏设 UpstreamModel 应当返回 ErrMissingUpstreamModel，得到 %v", err)
	}

	// 一个副作用都不该留下：它在开事务之前就被挡下了。
	if n := countUsageEvents(ctx, t, db, requestID); n != 0 {
		t.Errorf("被挡下的结算写了 %d 行用量事件，期望 0 行", n)
	}
	if balance, frozen := walletOf(t, db, userID); balance != "100.00000000" || frozen != "30.00000000" {
		t.Errorf("被挡下的结算动了钱包：余额 %s、冻结 %s，期望 100 与 30", balance, frozen)
	}
	var status string
	if qerr := db.QueryRowContext(ctx,
		`SELECT status FROM billing_requests WHERE request_id = $1`, requestID).Scan(&status); qerr != nil {
		t.Fatalf("读账单状态失败: %v", qerr)
	}
	if status != "pending" {
		t.Errorf("被挡下的结算把账单状态改成了 %q，期望仍是 pending", status)
	}
}

// # 幂等
//
// 上游超时、进程重启、队列重投都会让同一次结算到达两次，
// 而重复扣费是用户立刻能发现的错。
func TestSettlementIsIdempotent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "12", billID),
	}
	charge.Settlement.Actual = money.MustParse("12")

	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	// 重放
	err := s.Save(ctx, req, charge)
	if !errors.Is(err, ErrAlreadySettled) {
		t.Errorf("重放应当返回 ErrAlreadySettled，得到 %v", err)
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "88.00000000" {
		t.Errorf("重放后余额 = %s，期望仍是 88（只扣一次）", balance)
	}
}

// 结算要把**实际选中的渠道**写回账单。
//
// 预留发生在选路之前（见 internal/gateway 的 begin），那时还不知道会选中谁，
// 所以 INSERT 写进去的是 NULL。结算时渠道已经定了，markSettled 是那个值唯一
// 的落点——漏掉它，这一列恒为 NULL，按渠道归因用量、渠道级对账都拿不到数据，
// 而单看预留那一侧（它本来就该是 NULL）看不出任何异常。
func TestSettlementWritesBackChannel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	channelID := seedChannel(t, db)
	requestID := reqID(t)

	// 预留：渠道未知。testRequest 不带 ChannelID，与生产同一条路。
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	if got := channelOf(t, db, requestID); got.Valid {
		t.Fatalf("预留时 channel_id = %d，期望 NULL——预留发生在选路之前", got.Int64)
	}

	// 结算：渠道已知，带着它落库。
	settleReq := req
	settleReq.ChannelID = channelID

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "12", billID),
	}
	charge.Settlement.Actual = money.MustParse("12")

	if err := s.Save(ctx, settleReq, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got := channelOf(t, db, requestID); !got.Valid || got.Int64 != channelID {
		t.Errorf("结算后 channel_id = %v，期望 %d（这次请求实际走的渠道）", got, channelID)
	}

	// 重放不该把它抹掉：那一行已经 settled，整条 UPDATE 影响 0 行。
	if err := s.Save(ctx, settleReq, charge); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("重放应当返回 ErrAlreadySettled，得到 %v", err)
	}
	if got := channelOf(t, db, requestID); !got.Valid || got.Int64 != channelID {
		t.Errorf("重放后 channel_id = %v，期望仍是 %d", got, channelID)
	}
}

// 客户端断开这件事要真的落库，而它只有这一个落点。
//
// 网关当场判定「客户端断的」（internal/gateway 的 pumpStream），一路带到
// pipeline.Outcome 与 pipeline.Charge；库里留下痕迹的只有 markSettled 这一条
// UPDATE。在它接上之前，那个信号走到 Charge 就到头了——「这个渠道是不是经常
// 被客户端掐断」这个问题因此答不出来，而客户端断开是真实发生的事。
//
// 两个方向都钉：取消时要留下时刻，没取消时必须留 NULL。只钉前一半的话，
// 把这一列写成恒有值也能通过，而那会让每一笔账单都记成「客户端断的」。
func TestSettlementRecordsClientCancel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")

	for _, tc := range []struct {
		name     string
		canceled bool
	}{
		{"客户端断开的流式请求", true},
		{"正常结束的请求", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestID := reqID(t)
			req := testRequest(ctx, t, db, userID, requestID)
			billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
			if reserveErr != nil {
				t.Fatal(reserveErr)
			}

			charge := &pipeline.Charge{
				RequestID: requestID, UserID: userID, Asset: "CNY",
				PricedAt:       pricedAt(),
				Entries:        consumeEntries(t, userID, "12", billID),
				ClientCanceled: tc.canceled,
			}
			charge.Settlement.Actual = money.MustParse("12")

			if err := s.Save(ctx, req, charge); err != nil {
				t.Fatalf("结算失败: %v", err)
			}

			at := clientCanceledAt(ctx, t, db, requestID)
			if !tc.canceled {
				if at.Valid {
					t.Errorf("没有取消的账单写下了 client_canceled_at = %v，期望 NULL", at.Time)
				}
				return
			}
			if !at.Valid {
				t.Fatal("客户端取消的账单没有留下 client_canceled_at")
			}
			// 钉的是「它是个真实的时刻」而不只是「非空」：写死一个常量
			// （例如纪元）也能让非空断言通过，而那一列的全部价值就在于
			// 「什么时候断的」。界放得宽是有意的——这里只排除明显不对的值，
			// 不要求它与测试机的钟对齐。
			if age := time.Since(at.Time); age > time.Hour || age < -time.Minute {
				t.Errorf("client_canceled_at = %v（距今 %v），不像刚刚写下的时刻", at.Time, age)
			}
		})
	}
}

// 并发重放也只能扣一次。
//
// 这条与上一条不同：它压的是「幂等判断在加锁之后」这个顺序。
// 判断放在加锁之前的话，两个并发重放会各自看到「还没结算」。
func TestConcurrentSettlementChargesOnce(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "50", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	const n = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		okCount  int
		dupCount int
		otherErr error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			charge := &pipeline.Charge{
				RequestID: requestID, UserID: userID, Asset: "CNY",
				PricedAt: pricedAt(),
				Entries:  consumeEntries(t, userID, "10", billID),
			}
			charge.Settlement.Actual = money.MustParse("10")

			err := s.Save(ctx, req, charge)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrAlreadySettled):
				dupCount++
			default:
				otherErr = err
			}
		}()
	}
	wg.Wait()

	if otherErr != nil {
		t.Fatalf("并发结算出错: %v", otherErr)
	}
	if okCount != 1 {
		t.Errorf("成功结算 %d 次，期望恰好 1 次", okCount)
	}
	if dupCount != n-1 {
		t.Errorf("重复结算 %d 次，期望 %d 次", dupCount, n-1)
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "90.00000000" {
		t.Errorf("余额 = %s，期望 90（只扣一次）", balance)
	}
}

// 扣款会让余额变负时必须报错，而不是夹到零。
func TestSettlementRefusesNegativeBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	// 预留 10，但结算 150 —— 预留机制本该不让这事发生，
	// 真发生说明有别的路径动了钱包或预留算错了。
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "150", billID),
	}
	charge.Settlement.Actual = money.MustParse("150")

	err := s.Save(ctx, req, charge)
	if err == nil {
		t.Fatal("扣款会让余额变负，应当失败")
	}
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Errorf("期望 ErrInsufficientBalance，得到 %v", err)
	}

	// 失败时整个事务回滚：余额与冻结都没变
	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" || frozen != "10.00000000" {
		t.Errorf("失败时应当整体回滚，得到 balance=%s frozen=%s", balance, frozen)
	}

	// 账本里不该留下那组分录。seed 造过一笔 100 的充值，所以期望值
	// 是**充值额**而不是零——按零断言会把「充值也没了」也当成通过。
	if got := ledgerBalance(t, db, userID); got.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("失败时账本不该有变化，期望仍是充值的 100，得到 %s", got)
	}
}

// ── 释放 ────────────────────────────────────────────────────────────────

func TestReleaseUnfreezes(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("释放不该动余额: %s", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("冻结应当释放: %s", frozen)
	}
}

// 释放是幂等的，且**不写分录**——它没有发生资金流动。
func TestReleaseIsIdempotentAndWritesNoEntry(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Release(ctx, req, nil); err != nil {
			t.Fatalf("第 %d 次释放失败: %v", i+1, err)
		}
	}

	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("重复释放不该把冻结扣成负数: %s", frozen)
	}
	// seed 造过一条充值分录，所以基线是 1 而不是 0
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("释放不该写分录（账本只记资金流动），期望仍只有 seed 的那 1 条，实际 %d 条", n)
	}
}

// 释放一个不存在的预留不是错误：目的已经达到了。
//
// 请求直接构造而不是走 testRequest：这一条路径**不写** billing_requests
// （没有预留记录可释放），所以它不需要一把真的 key，而 testRequest 会去
// 建一把（那要求 userID 真的存在）。
func TestReleaseUnknownRequest(t *testing.T) {
	db := testDB(t)
	s := New(db)
	req := testRequestWithKey(1, reqID(t), 0)
	if err := s.Release(context.Background(), req, nil); err != nil {
		t.Errorf("释放不存在的预留不该报错: %v", err)
	}
}

// 不计费的结局要把上游失败的依据落进账单的两列。
//
// 这两列此前恒为 NULL：markSettled 里那两个参数位写的是字面量 0 与空串，
// 而失败那条路根本不走结算。于是「这次结算对应的上游回了什么」在账单表里
// 答不出来，得去 join 快照表——而快照是可以被关掉、按月清掉的。
//
// 写入点只有 Release 这一处（见它的注释），所以三条断言都落在这一行上：
// 依据写进去了、重复释放不会把它改掉、没有依据时是 NULL 而不是 0 或空串。
func TestReleaseRecordsUpstreamFailure(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}

	// 上游回了 400。这一条正是「这两列最该有值」的那个场景。
	failure := &pipeline.UpstreamFailure{
		RequestID: requestID,
		Status:    http.StatusBadRequest,
		Code:      pipeline.ErrorUpstreamHTTPError,
	}
	if err := s.Release(ctx, req, failure); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	status, code := upstreamFailureOf(t, db, requestID)
	if !status.Valid || status.Int64 != http.StatusBadRequest {
		t.Errorf("upstream_status = %v，期望 400", status)
	}
	if !code.Valid || code.String != pipeline.ErrorUpstreamHTTPError {
		t.Errorf("error_code = %v，期望 %q", code, pipeline.ErrorUpstreamHTTPError)
	}

	// 重复释放是空操作（那一行已经不是 pending 了），依据不能被第二次的
	// 覆盖掉：把「谁先写的」交给调用顺序，事后读到的就不是当时那一次失败。
	if err := s.Release(ctx, req, &pipeline.UpstreamFailure{
		RequestID: requestID,
		Status:    http.StatusInternalServerError,
		Code:      pipeline.ErrorUpstreamNoAnswer,
	}); err != nil {
		t.Fatalf("重复释放失败: %v", err)
	}
	if status, code := upstreamFailureOf(t, db, requestID); status.Int64 != http.StatusBadRequest ||
		code.String != pipeline.ErrorUpstreamHTTPError {
		t.Errorf("重复释放改掉了依据：upstream_status=%v error_code=%v，期望仍是 400 / %q",
			status, code, pipeline.ErrorUpstreamHTTPError)
	}
}

// 不计费的结局也要记下「打到了哪个渠道、用哪个上游名」。
//
// 与上面那条同一个位置、同一个理由：这两列在**预留**时写不出来（预留发生
// 在选路之前），而失败这条路不走结算——不写在这里，失败行上它们恒为 NULL。
// 那正是「某个渠道对某批模型全在报错」时最需要它们的地方：只有 status 与
// error_code 时，看得出「为什么失败」，看不出「打的是谁」。
//
// # 判据为什么是两个具体值
//
// 夹具给的上游名（vendor/gpt-4o）**故意与规范名不同**，渠道 id 也是真插了
// 一行拿到的。写了个别的渠道、或者把规范名抄进 upstream_model，都过不了；
// 只断言「非 NULL」的话那两种写法一样能过。
func TestReleaseRecordsChannelAndModel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	// 选路之后才知道的那两样东西，由网关在释放之前填上
	// （见 internal/gateway 的 handle.routedTo）。
	req.ChannelID = seedChannel(t, db)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	channelID, model := channelAndModelOf(t, db, requestID)
	if !channelID.Valid || channelID.Int64 != req.ChannelID {
		t.Errorf("channel_id = %v，期望 %d", channelID, req.ChannelID)
	}
	if !model.Valid || model.String != req.UpstreamModel {
		t.Errorf("upstream_model = %v，期望 %q（夹具的上游名与规范名故意不同，"+
			"相等说明写的是规范名）", model, req.UpstreamModel)
	}
}

// 一个候选都没选中时，那两列是 NULL 而不是 0 与空串。
//
// 两个方向都要钉：只钉「选出来了就写得进去」的话，把两列无条件写成 `$4` 与
// `$5`（0 与空串照写）也能通过，而 0 在库里会读成「渠道 0」——那个 id 根本
// 不存在（外键会拒绝它），空串则让「没有名字」与「名字是空的」分不开。
//
// 这条路径真实存在：候选集被筛空、别名翻不出来、选路之前 context 就取消了，
// 都走到这里，而它们一次上游调用都没发生。
func TestReleaseWithoutSelectedChannelWritesNull(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	// 选路一次都没选出渠道：两列保持零值，与网关那边的行为一致。
	req.ChannelID = 0
	req.UpstreamModel = ""

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	if channelID, model := channelAndModelOf(t, db, requestID); channelID.Valid || model.Valid {
		t.Errorf("没有选中渠道时两列应当是 NULL，得到 channel_id=%v upstream_model=%v",
			channelID, model)
	}
}

// 没有上游失败的依据时，两列必须是 NULL 而不是 0 与空串。
//
// 两个方向都要钉：只钉「有依据时写得进去」的话，把两列无条件写成
// `$2` 与 `$3`（0 与空串照写）也能通过，而那样库里就分不出「上游回了 0 号」
// 与「没有这回事」——HTTP 里没有 0 这个状态码，它是「没有值」的编码。
//
// 这条路径真实存在：插件阻断、结算本身出错，以及「结算之前就返回了」
// （客户端断开、context 取消）都走到这里，而它们都不是上游的失败。
func TestReleaseWithoutEvidenceWritesNull(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	if status, code := upstreamFailureOf(t, db, requestID); status.Valid || code.Valid {
		t.Errorf("没有依据时两列应当是 NULL，得到 upstream_status=%v error_code=%v", status, code)
	}
}

// 已结算的行这两列仍然是 NULL——它们记的是**上游失败**的证据。
//
// 它钉的是另一半：把这两列顺手也写进结算那条 UPDATE（写个 200 上去）会让
// 「这一行是不是上游挂的」这个问题失去判据。而上游响应里根本没有状态码
// （channel.Response 不带它），所以那一步写得出来的只有编出来的数。
func TestSettlementLeavesUpstreamFailureColumnsNull(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "12", billID),
	}
	charge.Settlement.Actual = money.MustParse("12")
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	if status, code := upstreamFailureOf(t, db, requestID); status.Valid || code.Valid {
		t.Errorf("已结算的行不该有上游失败的依据，得到 upstream_status=%v error_code=%v", status, code)
	}
}

// 已经结算过的请求不能再释放——那会把冻结额扣两次。
func TestReleaseAfterSettlementIsNoop(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "12", billID),
	}
	charge.Settlement.Actual = money.MustParse("12")
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("结算后释放不该报错: %v", err)
	}

	// 余额与冻结都保持结算后的状态
	balance, frozen := walletOf(t, db, userID)
	if balance != "88.00000000" || frozen != "0.00000000" {
		t.Errorf("结算后释放应当是空操作，得到 balance=%s frozen=%s", balance, frozen)
	}
}

// ── 对账 ────────────────────────────────────────────────────────────────

// 钱包快照必须等于账本推出来的值。
//
// 这是「快照是具体化物、账本是权威」这条关系的可执行检查。
func TestWalletMatchesLedger(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")

	for i := 0; i < 5; i++ {
		requestID := reqID(t)
		req := testRequest(ctx, t, db, userID, requestID)
		billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "100", "CNY"))
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		amount := money.FromInt(int64(10 + i))
		charge := &pipeline.Charge{
			RequestID: requestID, UserID: userID, Asset: "CNY",
			PricedAt: pricedAt(),
			Entries:  consumeEntries(t, userID, amount.String(), billID),
		}
		charge.Settlement.Actual = amount
		if err := s.Save(ctx, req, charge); err != nil {
			t.Fatal(err)
		}
	}

	balance, _ := walletOf(t, db, userID)
	want := ledgerBalance(t, db, userID)
	if money.MustParse(balance).Cmp(want) != 0 {
		t.Errorf("钱包 %s 与账本 %s 不一致", balance, want)
	}
	// 1000 − (10+11+12+13+14) = 940
	if balance != "940.00000000" {
		t.Errorf("余额 = %s，期望 940", balance)
	}
}

// ── 死锁 ────────────────────────────────────────────────────────────────

// 两个并发的多用户交易，若加锁顺序不固定就会死锁。
//
// 这里让两个 goroutine 以**相反的用户顺序**造分录，而 store 内部会按
// 排序后的顺序拿锁。跑若干轮，任何一次死锁都会让 PostgreSQL 报错。
func TestConcurrentMultiWalletNoDeadlock(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	// 一批用户，每个都有余额
	users := make([]int64, 6)
	for i := range users {
		users[i] = seed(t, db, "1000")
	}
	// 每个用户各一把 key，在起 goroutine **之前**建好：seedAPIKey 会调
	// t.Fatalf，而从测试 goroutine 之外的 goroutine 里调它是不允许的。
	keys := make(map[int64]int64, len(users))
	for _, u := range users {
		keys[u] = seedAPIKey(ctx, t, db, u)
	}

	const rounds = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for r := 0; r < rounds; r++ {
		// 一轮里两个方向相反的交易
		for _, order := range [][]int64{users, reverse(users)} {
			wg.Add(1)
			go func(order []int64) {
				defer wg.Done()
				requestID := reqID(t)
				// 用第一个用户作为「请求主体」，但分录覆盖全部用户
				req := testRequestWithKey(order[0], requestID, keys[order[0]])
				if _, err := s.SaveReservation(ctx, req, reservation(t, order[0], requestID, "1", "CNY")); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				charge := &pipeline.Charge{
					RequestID: requestID, UserID: order[0], Asset: "CNY",
					PricedAt: pricedAt(),
					Entries:  multiUserEntries(t, order),
				}
				charge.Settlement.Actual = money.FromInt(1)
				if err := s.Save(ctx, req, charge); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(order)
		}
	}
	wg.Wait()

	for _, err := range errs {
		if err == nil {
			continue
		}
		// 死锁的报错文本里带 "deadlock"
		if isDeadlock(err) {
			t.Fatalf("出现死锁——加锁顺序没有固定住: %v", err)
		}
		t.Errorf("并发交易出错: %v", err)
	}
}

// multiUserEntries 造一组「从每个用户各扣一点、汇总到平台」的分录。
//
// 用户按传入顺序出现，于是不同的调用方会给出不同的科目顺序——
// 这正是死锁的温床。
func multiUserEntries(t *testing.T, users []int64) []ledger.Entry {
	t.Helper()
	txn := ledger.NewTxnID()
	one := money.MustParse("0.1")

	var out []ledger.Entry
	total := money.Zero
	for _, id := range users {
		out = append(out, ledger.Entry{
			TxnID: txn, Account: ledger.UserAccount(id), Asset: ledger.AssetCNY,
			Amount: one.Neg(), EntryType: ledger.EntryConsume,
		})
		total = total.Add(one)
	}
	out = append(out, ledger.Entry{
		TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
		Amount: total, EntryType: ledger.EntryConsume,
	})
	return out
}

func reverse(v []int64) []int64 {
	out := make([]int64, len(v))
	for i := range v {
		out[i] = v[len(v)-1-i]
	}
	return out
}

func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") || strings.Contains(msg, "40p01")
}

// ── 精度 ────────────────────────────────────────────────────────────────

// 金额经 numeric(20,8) 往返之后必须一分不差。
//
// 这一层是 Go 的定点数与数据库的 numeric 之间唯一的转换点，
// 精度在这里丢了的话，账目会慢慢对不上而没有任何信号。
func TestAmountPrecisionRoundTrip(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")

	// 带满 8 位小数的金额
	const amount = "0.00000001"
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, amount, "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, amount, billID),
	}
	charge.Settlement.Actual = money.MustParse(amount)
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	balance, _ := walletOf(t, db, userID)
	want := money.MustParse("1000").Sub(money.MustParse(amount))
	if money.MustParse(balance).Cmp(want) != 0 {
		t.Errorf("余额 = %s，期望 %s", balance, want)
	}
}

// 大额也要精确。
func TestLargeAmountPrecision(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	const start = "99999999.99999999"
	userID := seed(t, db, start)

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	const amount = "12345678.87654321"
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, amount, "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, amount, billID),
	}
	charge.Settlement.Actual = money.MustParse(amount)
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	balance, _ := walletOf(t, db, userID)
	want := money.MustParse(start).Sub(money.MustParse(amount))
	if money.MustParse(balance).Cmp(want) != 0 {
		t.Errorf("余额 = %s，期望 %s", balance, want)
	}
}

// reqID 生成一个唯一的请求标识。
func reqID(t *testing.T) string {
	t.Helper()
	return "req-" + itoa(nextID())
}

// 规则快照要能正确落库——它是账单可解释性的依据。
//
// 规则事后可能被改，而账单必须能回答「当时按哪条规则算的」。
// 这条也顺带验证了 pricing_rule_id 的外键约束真的生效
// （前面那条用例就是因为这个约束才发现自己漏造了规则）。
func TestReservationRecordsPricingRule(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	requestID := reqID(t)

	// 造一条真规则，只匹配**本用例自己的**模型名。
	//
	// pricing_rules 是全局表，而测试库由同包与别的包的用例共用（goose 只
	// 保证迁移幂等，不隔离数据）。一条不带 model_patterns 的平台规则匹配
	// **任何**模型，于是「一条规则都匹配不上」（pipeline.ErrNoRule）这个状态
	// 在整库范围内消失，而那条分支必须能被测到——`go test ./...` 是并行跑
	// 包的，受影响的正是这条规则留在库里的那个窗口。
	//
	// 所以给它一个只有本用例知道的模型名：即使它还在库里，也影响不到别人。
	model := "model-rule-" + itoa(nextID())
	var ruleID int64
	if ruleErr := db.QueryRowContext(ctx, `
		INSERT INTO pricing_rules (scope, name, model_patterns, expr, priority)
		VALUES ('platform', 'placeholder-rule', to_jsonb($1::text[]), '{"prompt": 0.000001}', 0)
		RETURNING id`, []string{model}).Scan(&ruleID); ruleErr != nil {
		t.Fatalf("造规则失败: %v", ruleErr)
	}

	// 用完删掉，不让共享的库累积规则。删的顺序不能反：
	// billing_requests.pricing_rule_id 指向这条规则且没有 ON DELETE，先删
	// 规则会被外键挡下（实测报 billing_requests_pricing_rule_id_fkey）。
	// 清理失败必须报出来——被忽略的失败与清理成功在输出里长得一样。
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM billing_requests WHERE request_id = $1`, requestID); err != nil {
			t.Errorf("清理账单行失败: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`DELETE FROM pricing_rules WHERE id = $1`, ruleID); err != nil {
			t.Errorf("清理计价规则失败: %v", err)
		}
	})

	userID := seed(t, db, "100")
	req := testRequest(ctx, t, db, userID, requestID)
	req.CanonicalModel = model

	if _, err := s.SaveReservation(ctx, req,
		reservationWithRule(t, userID, requestID, "10", ruleID)); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	var gotRule int64
	var gotExpr sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT pricing_rule_id, pricing_expr_snapshot FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&gotRule, &gotExpr)
	if err != nil {
		t.Fatal(err)
	}
	if gotRule != ruleID {
		t.Errorf("规则 id = %d，期望 %d", gotRule, ruleID)
	}
	if !gotExpr.Valid || gotExpr.String == "" {
		t.Error("表达式快照应当落库——规则事后可变，账单要能解释当时算的是什么")
	}
}

// 不记规则时该字段是 NULL，而不是 0。
//
// 0 会被读成「规则 0」——一个不存在的规则，而 NULL 才是「没有规则」。
func TestReservationWithoutRuleStoresNull(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	var ruleID sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT pricing_rule_id FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	if ruleID.Valid {
		t.Errorf("没记规则时应当是 NULL，得到 %d", ruleID.Int64)
	}
}

// ── 协议 ────────────────────────────────────────────────────────────────

// 协议按**调用方给的值**落库，而不是某一套入口的固定值。
//
// 这一列是事后按协议统计的依据（「Anthropic 入口这个月花了多少」），
// 所以断言的是字面量 "anthropic" / "openai"，不是「非空」——写死一个值
// 也能让「非空」通过。
func TestReservationRecordsProtocol(t *testing.T) {
	cases := []struct {
		name     string
		protocol channel.Protocol
		want     string
	}{
		{name: "OpenAI 入口", protocol: channel.ProtocolOpenAI, want: "openai"},
		{name: "Anthropic 入口", protocol: channel.ProtocolAnthropic, want: "anthropic"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			s := New(db)
			ctx := context.Background()

			userID := seed(t, db, "100")
			requestID := reqID(t)
			req := testRequest(ctx, t, db, userID, requestID)
			req.Protocol = tc.protocol

			if _, err := s.SaveReservation(ctx, req,
				reservation(t, userID, requestID, "30", "CNY")); err != nil {
				t.Fatalf("预留失败: %v", err)
			}

			var got string
			if err := db.QueryRowContext(ctx,
				`SELECT protocol FROM billing_requests WHERE request_id = $1`,
				requestID).Scan(&got); err != nil {
				t.Fatalf("读回协议失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("billing_requests.protocol = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 漏设协议时**报错**，而不是往那一列写一个空串。
//
// 报错要指向调用方漏了字段：这一列的 NOT NULL 拦的是 NULL，空串写得进去，
// 所以「漏了赋值」本来会表现成一条协议为空的账单——事后看起来像数据脏了，
// 而不像代码里少了一处。这一条同时钉住「报错时什么都没改」。
func TestReservationRejectsMissingProtocol(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	req.Protocol = ""

	_, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if !errors.Is(err, ErrMissingProtocol) {
		t.Fatalf("错误 = %v，期望 ErrMissingProtocol", err)
	}
	// 措辞要让读日志的人知道去改哪儿，而不是去查数据库。
	if !strings.Contains(err.Error(), "pipeline.Request.Protocol") {
		t.Errorf("报错没指明是调用方漏设了字段: %v", err)
	}

	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("被拒绝的预留不该冻住余额，冻结 = %s", frozen)
	}
	if n := countRequests(t, db, requestID); n != 0 {
		t.Errorf("被拒绝的预留不该留下记录，却有 %d 行", n)
	}
}

// 协议不是两套入口之一时同样报错，且与「漏设」分开。
func TestReservationRejectsUnknownProtocol(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	// 库里那一列是 text，注释写着 openai / anthropic 但它自己不拦别的值
	// （见 internal/migrate/sql/00001_init.sql）。约束在代码里，这一条钉住它。
	req.Protocol = channel.Protocol("custom")

	_, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if !errors.Is(err, ErrUnknownProtocol) {
		t.Fatalf("错误 = %v，期望 ErrUnknownProtocol", err)
	}
	if errors.Is(err, ErrMissingProtocol) {
		t.Error("「取值不对」不该被报成「漏设」——处置不同")
	}

	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("被拒绝的预留不该冻住余额，冻结 = %s", frozen)
	}
	if n := countRequests(t, db, requestID); n != 0 {
		t.Errorf("被拒绝的预留不该留下记录，却有 %d 行", n)
	}
}

// countRequests 数某个请求标识在 billing_requests 里有几行。
func countRequests(t *testing.T, db *sql.DB, requestID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("统计预留记录失败: %v", err)
	}
	return n
}

// ── API Key ─────────────────────────────────────────────────────────────

// 账单要记下这次请求用的是**哪一把** API Key。
//
// 这一列此前有列、有外键、有注释，却没有任何写入点：这张表唯一的 INSERT
// 没有它，于是每一行都是 NULL，「按 API Key 查用量」在账本上做不了，
// 只能退到按用户——而一个用户本来就可能有多把 key。
//
// 断言的是**那把 key 的 id 字面量**，不是「非 NULL」：一个把所有请求都写成
// 同一把 key 的实现也能过「非 NULL」，而它给出的答案同样是错的。所以这里
// 造**同一个用户的两把 key**，各发一次请求，各自认自己那一个 id。
func TestReservationRecordsAPIKey(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	// 同一个用户的两把 key：这一列要能分辨它们，否则「按 key 归因」这件事
	// 与「按用户归因」没有区别。
	firstKey := seedAPIKey(ctx, t, db, userID)
	secondKey := seedAPIKey(ctx, t, db, userID)
	if firstKey == secondKey {
		t.Fatalf("两把 key 拿到了同一个 id %d，用例本身不成立", firstKey)
	}

	cases := []struct {
		name      string
		keyID     int64
		wantKeyID int64
	}{
		{name: "第一把", keyID: firstKey, wantKeyID: firstKey},
		{name: "第二把", keyID: secondKey, wantKeyID: secondKey},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requestID := reqID(t)
			req := testRequestWithKey(userID, requestID, tc.keyID)

			if _, err := s.SaveReservation(ctx, req,
				reservation(t, userID, requestID, "30", "CNY")); err != nil {
				t.Fatalf("预留失败: %v", err)
			}

			var got sql.NullInt64
			if err := db.QueryRowContext(ctx,
				`SELECT api_key_id FROM billing_requests WHERE request_id = $1`,
				requestID).Scan(&got); err != nil {
				t.Fatalf("读回 API Key 失败: %v", err)
			}
			if !got.Valid {
				t.Fatalf("billing_requests.api_key_id 是 NULL，期望 %d——这一列没有写入点",
					tc.wantKeyID)
			}
			if got.Int64 != tc.wantKeyID {
				t.Errorf("billing_requests.api_key_id = %d，期望 %d（这次请求用的那把 key）",
					got.Int64, tc.wantKeyID)
			}
		})
	}
}

// 漏设 API Key 时**报错**，而不是往那一列写 NULL。
//
// 这一列可空，而且历史行本来就是空的，所以「调用方漏了赋值」与「这是补写入点
// 之前的行」在库里长得一模一样——写进去的那一刻是唯一还分得清的地方。
// 这一条同时钉住「报错时什么都没改」。
func TestReservationRejectsMissingAPIKey(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequestWithKey(userID, requestID, 0)

	_, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("错误 = %v，期望 ErrMissingAPIKey", err)
	}
	// 措辞要让读日志的人知道去改哪儿，而不是去查数据库。
	if !strings.Contains(err.Error(), "pipeline.Request.APIKeyID") {
		t.Errorf("报错没指明是调用方漏设了字段: %v", err)
	}

	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("被拒绝的预留不该冻住余额，冻结 = %s", frozen)
	}
	if n := countRequests(t, db, requestID); n != 0 {
		t.Errorf("被拒绝的预留不该留下记录，却有 %d 行", n)
	}
}
