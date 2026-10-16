package assembly

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/idempotency"
)

// 幂等键的端到端：一次 HTTP 请求带不带 Idempotency-Key，钱与上游调用的次数
// 完全不同。
//
// # 这一组要证明什么
//
// 与非流式那一组（e2e_test.go）同一个约定：走生产装配路径，假上游是本地
// httptest，断言必须回到库里看钱。除此之外它还要证明三件只在**并发与失败**
// 下才看得见的事：
//
//  1. 同键的第二次请求不产生第二条分录，也不产生第二次上游调用。
//  2. 幂等记录与预留**在同一个事务里**——所以预留失败时记录也不该留下。
//     这一条不能靠读代码证明：分开写两个事务的代码看起来一模一样，
//     要拿「预留失败」这个场景去区分。
//  3. 第一次没走完（上游失败、客户端中途断开）时同键重试**不会**再打一次上游。
//
// # 没有这个头时行为完全不变
//
// 这是这一组里最容易漏掉的一条，而它的证据不在本文件里：e2e_test.go 与
// stream_e2e_test.go 的既有用例全部原样通过。本文件末尾另有一条反向对照
// （TestWithoutIdempotencyKeyChargesTwice），它钉住的是「挡住重复的是这个头，
// 而不是别的东西顺手挡住的」。

// ── 假上游 ────────────────────────────────────────────────────────────

// gatedUpstream 是一个可以被测试卡住的假上游。
//
// 并发同键那条用例需要「第一次请求还在跑」这个状态**持续存在**：不卡住的话，
// 第二次请求到达时第一次已经结束了，测到的就只是串行重放。闸门把第一次
// 卡在调上游那一刻，第二次请求必然落在 in_progress 上。
//
// 它回的响应体是固定的字节：重放要逐字节比对，而「两次渲染出来一样」
// 不是同一件事（渲染里带着每次请求新生成的标识）。
type gatedUpstream struct {
	model string

	release chan struct{}

	mu    sync.Mutex
	calls int
}

func newGatedUpstream(model string) *gatedUpstream {
	return &gatedUpstream{model: model, release: make(chan struct{})}
}

func (u *gatedUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		u.mu.Unlock()

		select {
		case <-u.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixedChatResponse(u.model))
	})
}

func (u *gatedUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// open 放行被卡住的上游调用。
func (u *gatedUpstream) open() { close(u.release) }

// fixedChatResponse 是一份内容固定的 OpenAI 协议响应。
func fixedChatResponse(model string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"chatcmpl-placeholder","object":"chat.completion","created":1700000000,`+
			`"model":%q,"choices":[{"index":0,"message":{"role":"assistant",`+
			`"content":"你好，我是假上游"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`, model))
}

// ── 调用方 ────────────────────────────────────────────────────────────

// postIdempotent 打一次带幂等键的公共入口。
//
// 它不复用 postChatFull：那个助手不带这个头，而「带」与「不带」正是这一组
// 全部用例的自变量。给 postChatFull 加一个参数会让另外几十条用例跟着改，
// 而那些用例的期望一个字都不该动（见本文件顶部）。
func postIdempotent(t *testing.T, url, key, idemKey string, body []byte) (int, []byte, http.Header) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(idempotency.HeaderName, idemKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return resp.StatusCode, out, resp.Header
}

// openStreamWithKey 发起一次带幂等键的流式请求。
//
// 与 stream_e2e_test.go 的 openStream 同一个形状，只是多一个头。取消函数是
// 「客户端中途断开」那条用例的全部手段——真实的断开就是客户端关连接，
// 在这里等于取消请求的 context。
func openStreamWithKey(t *testing.T, url, key, idemKey string, body []byte) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(idempotency.HeaderName, idemKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("发流式请求失败: %v", err)
	}
	// 响应体不在这里关，由调用点关（理由见 openStream 的注释）。
	return resp, cancel
}

// ── 库里的幂等记录 ────────────────────────────────────────────────────

// idemRow 是按键读出来的一行幂等记录。
type idemRow struct {
	state            string
	billingRequestID sql.NullInt64
	status           sql.NullInt64
	contentType      sql.NullString
	body             []byte
}

// idemOf 按键读一行幂等记录。没有这一行时第二个返回值为 false。
//
// 按键而不是按 api_key_id 查：测试里键是唯一的（unique 生成），
// 而 api_key_id 要从别处再查一次——那一步查错了会让「记录不存在」与
// 「查错了 key」长得一样。
func idemOf(t *testing.T, db *sql.DB, key string) (idemRow, bool) {
	t.Helper()

	var r idemRow
	err := db.QueryRowContext(context.Background(), `
		SELECT state, billing_request_id, response_status, response_content_type, response_body
		  FROM idempotency_keys
		 WHERE key = $1`, key,
	).Scan(&r.state, &r.billingRequestID, &r.status, &r.contentType, &r.body)
	if err == sql.ErrNoRows {
		return idemRow{}, false
	}
	if err != nil {
		t.Fatalf("查幂等记录失败: %v", err)
	}
	return r, true
}

// idemCount 数一个键有几行记录。
//
// 唯一约束保证它只能是 0 或 1，所以这个数真正回答的是「有没有」——
// 但写成计数是因为「预留失败之后记录也不该留下」那一条要的正是 0。
func idemCount(t *testing.T, db *sql.DB, key string) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("数幂等记录失败: %v", err)
	}
	return n
}

// waitForIdemState 等到某个键的记录进入指定状态。
//
// 非流式那条路上记录是在响应写完之后才更新的，而客户端拿到响应时它可能
// 还没写完；流式那条路更晚（收尾在后台 goroutine 里）。等不到就是真的没写，
// 而那正是要断言的事。
func waitForIdemState(t *testing.T, db *sql.DB, key, want string) idemRow {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	last := "（没有记录）"
	for time.Now().Before(deadline) {
		if r, ok := idemOf(t, db, key); ok {
			if r.state == want {
				return r
			}
			last = r.state
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等幂等记录变成 %q 超时，最后看到的状态是 %q", want, last)
	return idemRow{}
}

// billingCount 数一个用户名下有几行账单。
func billingCount(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM billing_requests WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("数账单失败: %v", err)
	}
	return n
}

// settlementCount 数一个用户名下发生过几次结算。
//
// 数的是**不同的 txn_id**，不是分录条数：账本是复式的，一次结算写两条腿
// （用户账上一条、平台账上一条），数条数会把每次结算都算成两次——
// 那样写出来的断言看起来更严，实际上只是把一个常数乘错了。
func settlementCount(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(context.Background(), `
		SELECT count(DISTINCT txn_id) FROM ledger_entries
		 WHERE billing_request_id IN (SELECT id FROM billing_requests WHERE user_id = $1)`,
		userID).Scan(&n); err != nil {
		t.Fatalf("数结算失败: %v", err)
	}
	return n
}

// assertErrorCode 断言错误体里带着指定的机器可读错误码。
//
// 两套协议放这个码的位置不同（见 internal/api/error.go），所以这里按协议
// 去不同的路径取——**这正是这个助手存在的理由**：如果两边都能从同一个位置
// 取到，那说明其中一套的形状被改坏了，而这个助手会因此报错。
func assertErrorCode(t *testing.T, protocol channel.Protocol, body []byte, wantCode string) {
	t.Helper()

	var got any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v\n%s", err, body)
	}
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("错误体不是一个对象：%s", body)
	}
	errObj, ok := obj["error"].(map[string]any)
	if !ok {
		t.Fatalf("错误体里没有 error 对象：%s", body)
	}

	switch protocol {
	case channel.ProtocolAnthropic:
		if got := errObj["type"]; got != wantCode {
			t.Errorf("error.type = %v，期望 %q（Anthropic 的错误体只有这一个机器可读的位置）", got, wantCode)
		}
		// 顶层的 type 是 Anthropic 形状的一部分，缺了它有些 SDK 解不出来。
		if got := obj["type"]; got != "error" {
			t.Errorf("顶层 type = %v，期望 error", got)
		}
	default:
		if got := errObj["code"]; got != wantCode {
			t.Errorf("error.code = %v，期望 %q", got, wantCode)
		}
		if got := errObj["type"]; got != "invalid_request_error" {
			t.Errorf("error.type = %v，期望 invalid_request_error", got)
		}
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Error("错误体里没有 message——调用方要靠它知道下一步该做什么")
	}
}

// idemFixture 是一次幂等用例要用的全部东西。
type idemFixture struct {
	db     *sql.DB
	sealer *credential.Sealer
	app    *App
	public *httptest.Server

	model  string
	user   int64
	key    string
	body   []byte
	rule   int64
	chanID int64
}

// newIdemFixture 起一套「一个用户、一把 key、一个渠道、一条规则」的台子。
//
// upstream 由调用方给：几条用例对假上游的行为要求完全不同（固定响应、
// 卡住、报错、吐 SSE），而其余部分一模一样。不把它参数化的话，
// 每条用例都要重抄一遍造数据的代码，而抄错的那一份不会报错。
func newIdemFixture(t *testing.T, upstreamURL, model string) *idemFixture {
	t.Helper()

	db := testDB(t)
	sealer := testSealer(t)

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	chanID := seedChannel(t, db, sealer, merchant, upstreamURL, model)
	rule := seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	return &idemFixture{
		db: db, sealer: sealer, app: app, public: public,
		model: model, user: user, key: key, body: chatBody(model),
		rule: rule, chanID: chanID,
	}
}

// ── 用例 ──────────────────────────────────────────────────────────────

// TestIdempotentReplayChargesOnce 是这一组的主用例：两次同键请求只扣一次费。
func TestIdempotentReplayChargesOnce(t *testing.T) {
	model := unique("e2e-idem-replay")
	up := newGatedUpstream(model)
	up.open() // 这条用例不需要卡住上游
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	f := newIdemFixture(t, srv.URL, model)
	idemKey := unique("idem")

	status1, body1, _ := postIdempotent(t, f.public.URL+"/v1/chat/completions", f.key, idemKey, f.body)
	if status1 != http.StatusOK {
		t.Fatalf("第一次请求状态码 = %d，期望 200。响应体：%s", status1, body1)
	}

	// 记录在响应写完之后才更新，所以这里等它——等不到就是真的没写。
	rec := waitForIdemState(t, f.db, idemKey, "completed")

	status2, body2, _ := postIdempotent(t, f.public.URL+"/v1/chat/completions", f.key, idemKey, f.body)
	if status2 != http.StatusOK {
		t.Fatalf("第二次请求状态码 = %d，期望 200（同键同体应当重放）。响应体：%s", status2, body2)
	}

	// 一、逐字节相同。这是「重放」的定义：不是「又渲染了一遍看起来一样」。
	if !bytes.Equal(body1, body2) {
		t.Errorf("第二次的响应体与第一次不同——重放必须是同一串字节\n第一次：%s\n第二次：%s", body1, body2)
	}

	// 二、上游只被调用一次。
	if got := up.callCount(); got != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次（第二次是重放，不该再打上游）", got)
	}

	// 三、账上只有一笔。两条分录各自都平、合计也对，所以「多收」只能靠
	// 数条数发现——这正是这个功能要防的那类错误。
	if got := billingCount(t, f.db, f.user); got != 1 {
		t.Errorf("账单有 %d 行，期望 1 行", got)
	}
	if got := settlementCount(t, f.db, f.user); got != 1 {
		t.Errorf("结算有 %d 次，期望 1 次", got)
	}
	assertConsumeEntry(t, f.db, rec.billingRequestID.Int64, f.user, "0.00033")
	// 余额只被扣一次：100 - 0.00033。
	assertWallet(t, f.db, f.user, "99.99967", "0")

	// 四、记录本身要能把两边关联起来——排查「客户端说重试被拒，但这次请求
	// 到底扣没扣钱」时，这是唯一的入口。
	if !rec.billingRequestID.Valid {
		t.Fatal("幂等记录没有关联到账单行——它与预留不在同一个事务里")
	}
	var billUser int64
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT user_id FROM billing_requests WHERE id = $1`, rec.billingRequestID.Int64,
	).Scan(&billUser); err != nil {
		t.Fatalf("幂等记录指向的账单行读不出来: %v", err)
	}
	if billUser != f.user {
		t.Errorf("幂等记录指向的账单属于用户 %d，期望 %d", billUser, f.user)
	}
}

// TestIdempotentConcurrentRequestsCallUpstreamOnce 是并发同键。
//
// 两个请求同时到达：一个真的跑，一个等它跑完再按重放规则返回。
// 立刻报错是不行的——客户端分辨不出「撞上了自己的并发」与「一次瞬时故障」，
// 于是它会再重试一次，而那正是要避免的。
func TestIdempotentConcurrentRequestsCallUpstreamOnce(t *testing.T) {
	model := unique("e2e-idem-concurrent")
	up := newGatedUpstream(model)
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	f := newIdemFixture(t, srv.URL, model)
	idemKey := unique("idem")
	url := f.public.URL + "/v1/chat/completions"

	type result struct {
		status int
		body   []byte
	}
	first := make(chan result, 1)
	go func() {
		status, body, _ := postIdempotent(t, url, f.key, idemKey, f.body)
		first <- result{status, body}
	}()

	// 等到上游真的被调用：那一刻预留与幂等记录都已经提交（预留必须在调上游
	// 之前），所以第二个请求必然落在 in_progress 上。不等这一步的话，
	// 第二个请求可能先到并抢走键，测的就不是「并发」而是「谁先到」。
	deadline := time.Now().Add(10 * time.Second)
	for up.callCount() == 0 {
		select {
		case r := <-first:
			// 第一次请求在上游被调用之前就结束了：它失败了，而失败的原因
			// 就在它的响应里。只说「等超时」会把唯一那条线索丢掉——
			// 一次 500 与一次真的卡住，在这个循环里长得一模一样。
			t.Fatalf("第一次请求没走到上游就结束了：状态码 %d，响应 %s", r.status, r.body)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("等第一次请求调上游超时：%s 内上游一次没被调用，库里这个键有 %d 行记录",
				10*time.Second, idemCount(t, f.db, idemKey))
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := make(chan result, 1)
	go func() {
		status, body, _ := postIdempotent(t, url, f.key, idemKey, f.body)
		second <- result{status, body}
	}()

	// 让第二个请求确实进入等待：它必须查到那条 in_progress 的记录。
	// 不给它这一点时间的话，闸门放开后它可能直接查到 completed——
	// 那样测到的只是重放，而不是「等待 + 重放」。
	time.Sleep(300 * time.Millisecond)
	if got := up.callCount(); got != 1 {
		t.Fatalf("等待期间上游被调用了 %d 次，期望 1 次（第二个请求不该自己去调上游）", got)
	}
	up.open()

	r1 := <-first
	r2 := <-second
	if r1.status != http.StatusOK || r2.status != http.StatusOK {
		t.Fatalf("两次请求的状态码 = %d / %d，期望都是 200", r1.status, r2.status)
	}
	if !bytes.Equal(r1.body, r2.body) {
		t.Errorf("等待的那个请求拿到的响应与第一次不同\n第一次：%s\n第二次：%s", r1.body, r2.body)
	}
	if got := up.callCount(); got != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次", got)
	}
	if got := billingCount(t, f.db, f.user); got != 1 {
		t.Errorf("账单有 %d 行，期望 1 行", got)
	}
	if got := settlementCount(t, f.db, f.user); got != 1 {
		t.Errorf("结算有 %d 次，期望 1 次", got)
	}
}

// TestIdempotentSameKeyDifferentBodyIsRejected 是同键不同体。
//
// 回第一次的响应等于对一个新请求撒谎——客户端以为自己的新请求被处理了。
// 报错是安全的那一侧，而且当场可诊断。
func TestIdempotentSameKeyDifferentBodyIsRejected(t *testing.T) {
	model := unique("e2e-idem-conflict")
	up := newGatedUpstream(model)
	up.open()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	f := newIdemFixture(t, srv.URL, model)
	idemKey := unique("idem")
	url := f.public.URL + "/v1/chat/completions"

	if status, body, _ := postIdempotent(t, url, f.key, idemKey, f.body); status != http.StatusOK {
		t.Fatalf("第一次请求状态码 = %d，期望 200。响应体：%s", status, body)
	}
	waitForIdemState(t, f.db, idemKey, "completed")

	// 同一个键，换一段消息内容。
	other := chatBodyWithMessage(model, "换一段话")
	status, body, _ := postIdempotent(t, url, f.key, idemKey, other)
	if status != http.StatusConflict {
		t.Fatalf("同键不同体的状态码 = %d，期望 409。响应体：%s", status, body)
	}
	assertErrorCode(t, channel.ProtocolOpenAI, body, "idempotency_key_reused")

	// 被拒的那次**一个副作用都不该留下**：没有第二条分录，也没有第二次上游调用。
	if got := up.callCount(); got != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次（被拒的请求不该走到上游）", got)
	}
	if got := billingCount(t, f.db, f.user); got != 1 {
		t.Errorf("账单有 %d 行，期望 1 行", got)
	}
	if got := settlementCount(t, f.db, f.user); got != 1 {
		t.Errorf("结算有 %d 次，期望 1 次", got)
	}
	assertWallet(t, f.db, f.user, "99.99967", "0")
}

// TestIdempotentFailedAttemptIsRecordedAndNotRetried 是「失败也要记」。
//
// 不记失败的话，失败的重试会一路重试到底——每一次都真的打一次上游。
func TestIdempotentFailedAttemptIsRecordedAndNotRetried(t *testing.T) {
	model := unique("e2e-idem-failed")
	fake := &fakeUpstream{db: nil, userID: 0, model: model}
	fake.fail.Store(true)
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	db := testDB(t)
	sealer := testSealer(t)
	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)
	fake.db = db
	fake.userID = user

	seedChannel(t, db, sealer, merchant, srv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	idemKey := unique("idem")
	url := public.URL + "/v1/chat/completions"
	body := chatBody(model)

	status1, _, _ := postIdempotent(t, url, key, idemKey, body)
	if status1 == http.StatusOK {
		t.Fatalf("第一次请求状态码 = %d，期望一个失败码（假上游在报 500）", status1)
	}
	waitForIdemState(t, db, idemKey, "failed")

	status2, body2, _ := postIdempotent(t, url, key, idemKey, body)
	if status2 != http.StatusConflict {
		t.Fatalf("同键重试的状态码 = %d，期望 409。响应体：%s", status2, body2)
	}
	assertErrorCode(t, channel.ProtocolOpenAI, body2, "idempotency_incomplete")

	// 关键的一条：重试**没有**再打一次上游。
	if got := fake.callCount(); got != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次（失败的那次已经记下了，重试不该再打）", got)
	}
	// 上游失败不计费：一次都没成功，账上不该有分录。
	if got := settlementCount(t, db, user); got != 0 {
		t.Errorf("结算有 %d 次，期望 0 次（上游失败不计费）", got)
	}
	assertWallet(t, db, user, "100", "0")
}

// TestIdempotentDisconnectedFirstAttemptIsNotReplayed 是「第一次没走完」。
//
// 客户端中途断开 → 那一次没有形成完整响应 → 同键重试不重放，报 409。
// 重放一段截断的响应比不重放更坏：客户端会把它当成一次成功的完整回答。
func TestIdempotentDisconnectedFirstAttemptIsNotReplayed(t *testing.T) {
	model := unique("e2e-idem-disconnect")
	db := testDB(t)
	sealer := testSealer(t)
	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	// hang：吐完分片之后挂住，于是客户端断开的那一刻流一定还没结束。
	up := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"你好", "，", "世界"},
		hang:     true,
		db:       db,
		userID:   user,
	}
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	seedChannel(t, db, sealer, merchant, srv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	idemKey := unique("idem")
	url := public.URL + "/v1/chat/completions"
	body := streamBody(model)

	resp, cancel := openStreamWithKey(t, url, key, idemKey, body)
	_ = readFrames(t, resp.Body, 2)
	// 客户端断开：真实的断开就是关连接，在这里等于取消请求的 context。
	cancel()
	_ = resp.Body.Close()

	waitForIdemState(t, db, idemKey, "failed")

	// 重试：不重放，报 409，且不再打上游。
	status, body2, _ := postIdempotent(t, url, key, idemKey, body)
	if status != http.StatusConflict {
		t.Fatalf("断线后同键重试的状态码 = %d，期望 409。响应体：%s", status, body2)
	}
	assertErrorCode(t, channel.ProtocolOpenAI, body2, "idempotency_incomplete")
	if got := up.callCount(); got != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次（重试不该再打）", got)
	}
}

// TestIdempotentReplaysStreamingResponse 是流式的重放。
//
// 存的就是那串 SSE 字节，重放就是原样再发一遍（同一个 Content-Type）。
func TestIdempotentReplaysStreamingResponse(t *testing.T) {
	model := unique("e2e-idem-stream")
	db := testDB(t)
	sealer := testSealer(t)
	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	up := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"你好", "，", "世界"},
		db:       db,
		userID:   user,
	}
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	seedChannel(t, db, sealer, merchant, srv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	idemKey := unique("idem")
	url := public.URL + "/v1/chat/completions"
	body := streamBody(model)

	// 第一次：完整读完。
	resp1, cancel1 := openStreamWithKey(t, url, key, idemKey, body)
	first := readFrames(t, resp1.Body, 0)
	contentType1 := resp1.Header.Get("Content-Type")
	_ = resp1.Body.Close()
	cancel1()

	waitForIdemState(t, db, idemKey, "completed")

	// 第二次：同一个键，拿到的是那串字节。
	resp2, cancel2 := openStreamWithKey(t, url, key, idemKey, body)
	defer cancel2()
	defer func() { _ = resp2.Body.Close() }()
	raw2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("读第二次的响应失败: %v", err)
	}

	if got := resp2.Header.Get("Content-Type"); got != contentType1 {
		t.Errorf("重放的 Content-Type = %q，期望 %q", got, contentType1)
	}
	if !bytes.Contains(raw2, []byte("data: [DONE]")) {
		t.Errorf("重放的流里没有结束标记——客户端据此知道回答是完整的\n%s", raw2)
	}
	if len(first) == 0 {
		t.Fatal("第一次一帧都没读到")
	}
	if got := up.callCount(); got != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次", got)
	}
	if got := settlementCount(t, db, user); got != 1 {
		t.Errorf("结算有 %d 次，期望 1 次", got)
	}

	// 重放的那份与库里存的那份是同一串字节。
	rec := waitForIdemState(t, db, idemKey, "completed")
	if !bytes.Equal(raw2, rec.body) {
		t.Errorf("重放发出去的字节与库里存的不同\n发出去：%q\n库里：%q", raw2, rec.body)
	}
}

// TestIdempotentConflictErrorShapePerProtocol 钉住两套协议的 409 形状。
//
// 调用方的 SDK 按自己那套解错误体，形状不对时它连错误类型都读不出来。
func TestIdempotentConflictErrorShapePerProtocol(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		protocol channel.Protocol
		chType   string
	}{
		{name: "openai", path: "/v1/chat/completions", protocol: channel.ProtocolOpenAI, chType: "openai"},
		{name: "anthropic", path: "/v1/messages", protocol: channel.ProtocolAnthropic, chType: "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := unique("e2e-idem-shape-" + tc.name)
			db := testDB(t)
			sealer := testSealer(t)
			merchant := seedUser(t, db, true, "0")
			user := seedUser(t, db, false, "100")
			key := issueKey(t, db, user)

			// 上游一律失败：这条用例要的是**错误体的形状**，而第一次请求
			// 只要留下一条记录就够了。用失败的那条路顺带覆盖了「同键不同体
			// 的判据排在状态之前」——记录是 failed，冲突仍然是冲突。
			fake := &fakeUpstream{db: db, userID: user, model: model}
			fake.fail.Store(true)
			srv := httptest.NewServer(fake.handler())
			t.Cleanup(srv.Close)

			seedChannelOfType(t, db, sealer, merchant, srv.URL, model, tc.chType, 0)
			seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

			app := newApp(t, db, sealer)
			public := httptest.NewServer(app.Public)
			t.Cleanup(public.Close)

			idemKey := unique("idem")
			url := public.URL + tc.path

			if status, body, _ := postIdempotent(t, url, key, idemKey, chatBody(model)); status == http.StatusOK {
				t.Fatalf("第一次请求状态码 = %d，期望一个失败码。响应体：%s", status, body)
			}
			waitForIdemState(t, db, idemKey, "failed")

			status, body, _ := postIdempotent(t, url, key, idemKey, chatBodyWithMessage(model, "换一段话"))
			if status != http.StatusConflict {
				t.Fatalf("同键不同体的状态码 = %d，期望 409。响应体：%s", status, body)
			}
			assertErrorCode(t, tc.protocol, body, "idempotency_key_reused")
		})
	}
}

// TestIdempotencyRecordSharesTransactionWithReservation 证明「同一个事务」。
//
// 读代码证明不了这件事：分开写两个事务的代码看起来一模一样。能区分的只有
// 一个场景——**预留失败**。同一个事务时那条记录随事务一起回滚，分开写时
// 它会留在库里，而留下来的后果是「重试被告知已处理，钱却没扣」。
func TestIdempotencyRecordSharesTransactionWithReservation(t *testing.T) {
	model := unique("e2e-idem-tx")
	up := newGatedUpstream(model)
	up.open()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	db := testDB(t)
	sealer := testSealer(t)
	merchant := seedUser(t, db, true, "0")
	key := issueKey(t, db, seedUser(t, db, false, "0")) // 余额 0：预留必然失败
	seedChannel(t, db, sealer, merchant, srv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	idemKey := unique("idem")
	status, body, _ := postIdempotent(t, public.URL+"/v1/chat/completions", key, idemKey, chatBody(model))
	if status == http.StatusOK {
		t.Fatalf("余额为 0 的请求状态码 = %d，期望失败。响应体：%s", status, body)
	}

	// 这就是判据：预留没成，记录也不该留下。
	if got := idemCount(t, db, idemKey); got != 0 {
		t.Errorf("预留失败之后库里留下了 %d 条幂等记录——它没有与预留写在同一个事务里。"+
			"留下的后果是重试被告知「已处理」而钱一分没扣", got)
	}
	if got := up.callCount(); got != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次（钱没冻住就不该调上游）", got)
	}
}

// TestIdempotencyScopeIsAPIKey 钉住作用域是「同一把 key 下」。
//
// 跨租户撞键必须互不影响：全局唯一会让一个用户能用掉另一个用户的键，
// 而这里的判据是「两个用户用同一个键字符串各自成功」——入口若把 user_id
// 当成作用域传下去，这条用例会挂。
func TestIdempotencyScopeIsAPIKey(t *testing.T) {
	model := unique("e2e-idem-scope")
	up := newGatedUpstream(model)
	up.open()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	db := testDB(t)
	sealer := testSealer(t)
	merchant := seedUser(t, db, true, "0")
	userA := seedUser(t, db, false, "100")
	userB := seedUser(t, db, false, "100")
	keyA := issueKey(t, db, userA)
	keyB := issueKey(t, db, userB)
	seedChannel(t, db, sealer, merchant, srv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 同一个键字符串，两把不同的 key，**内容也不同**：如果作用域串了，
	// 第二个用户会拿到 409（同键不同体）。
	idemKey := unique("idem")
	url := public.URL + "/v1/chat/completions"

	if status, body, _ := postIdempotent(t, url, keyA, idemKey, chatBody(model)); status != http.StatusOK {
		t.Fatalf("用户 A 的状态码 = %d，期望 200。响应体：%s", status, body)
	}
	if status, body, _ := postIdempotent(t, url, keyB, idemKey, chatBodyWithMessage(model, "另一个用户")); status != http.StatusOK {
		t.Fatalf("用户 B 的状态码 = %d，期望 200（不同 key 各自用自己的键空间）。响应体：%s", status, body)
	}

	if got := up.callCount(); got != 2 {
		t.Errorf("上游被调用了 %d 次，期望 2 次（两个用户的请求各自要真的跑一次）", got)
	}
	if got := settlementCount(t, db, userA); got != 1 {
		t.Errorf("用户 A 有 %d 次结算，期望 1 次", got)
	}
	if got := settlementCount(t, db, userB); got != 1 {
		t.Errorf("用户 B 有 %d 次结算，期望 1 次", got)
	}
}

// TestWithoutIdempotencyKeyChargesTwice 是反向对照。
//
// 它证明「挡住重复的是这个头」而不是别的东西顺手挡住的：不带这个头时，
// 同样的两次请求就是两次真实调用、两笔钱——这正是这个功能存在的原因。
//
// 它同时是「没有这个头时行为完全不变」的一条正面证据：这个头出现之前，
// 系统就是这个行为，而现在它仍然是这样。
func TestWithoutIdempotencyKeyChargesTwice(t *testing.T) {
	model := unique("e2e-idem-absent")
	up := newGatedUpstream(model)
	up.open()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	f := newIdemFixture(t, srv.URL, model)
	url := f.public.URL + "/v1/chat/completions"

	for i := 1; i <= 2; i++ {
		if status, body := postChat(t, url, f.key, f.body); status != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 = %d，期望 200。响应体：%s", i, status, body)
		}
	}

	if got := up.callCount(); got != 2 {
		t.Errorf("上游被调用了 %d 次，期望 2 次（没有幂等键时两次就是两次）", got)
	}
	if got := billingCount(t, f.db, f.user); got != 2 {
		t.Errorf("账单有 %d 行，期望 2 行", got)
	}
	if got := settlementCount(t, f.db, f.user); got != 2 {
		t.Errorf("结算有 %d 次，期望 2 次", got)
	}
	assertWallet(t, f.db, f.user, "99.99934", "0")
}

// chatBodyWithMessage 组装一份消息内容不同的请求体。
//
// 只改消息：改 model 或 max_tokens 同样会让指纹变，但那两种改动在别的用例里
// 有别的含义（换模型、换上界），用它们当「不同体」会让失败信息误导人。
func chatBodyWithMessage(model, text string) []byte {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": text}},
		"max_tokens": 64,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// TestIdempotentFailoverStillChargesOnce 钉住「上游内部换渠道重试不是客户端重试」。
//
// # 这两件事的区别只在次数上，而次数正是这个功能要防的
//
// 一次请求里，上游 500 之后网关会换一个渠道再试一次——那**仍然是同一次
// 请求**：一次预留、一次结算、一条幂等记录。而客户端的重试是另一次请求。
// 幂等键要挡住的是后者；如果它把前者也当成一次新的执行，一个上游抖动的
// 请求就会在库里留下两条记录，同键的下一次重试会撞上「同键不同体」——
// 用户看到的是一个自己没改过请求却被拒的 409。
//
// # 为什么这条要端到端测
//
// 网关那一层已有 TestFailoverToSecondChannel，但它看不到幂等记录：
// 「两次上游调用、一次结算、一条记录」这三个数只有在装配完整的链路上
// 才能一起断言。少了其中任何一个，这个性质都只是「读代码觉得对」。
func TestIdempotentFailoverStillChargesOnce(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	model := unique("e2e-idem-failover")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	// 第一个渠道（priority 10）回 500——5xx 判为可重试，所以网关会换渠道。
	// 它不吐字，所以这次失败发生在响应头之前，换渠道是允许的。
	broken := &fakeUpstream{db: db, userID: user, model: model}
	broken.fail.Store(true)
	brokenSrv := httptest.NewServer(broken.handler())
	t.Cleanup(brokenSrv.Close)

	// 第二个渠道（priority 0）健康。响应体是固定字节，所以重放可以逐字节比对。
	healthy := newGatedUpstream(model)
	healthy.open()
	healthySrv := httptest.NewServer(healthy.handler())
	t.Cleanup(healthySrv.Close)

	seedChannelOfType(t, db, sealer, merchant, brokenSrv.URL, model, "openai", 10)
	seedChannelOfType(t, db, sealer, merchant, healthySrv.URL, model, "openai", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	idemKey := unique("idem")
	url := public.URL + "/v1/chat/completions"
	body := chatBody(model)

	status, first, _ := postIdempotent(t, url, key, idemKey, body)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（换渠道之后应当成功）。响应体：%s", status, first)
	}

	// 上游被调了两次：那是**一次**请求内部的换渠道，不是两次请求。
	if got := broken.callCount(); got != 1 {
		t.Errorf("第一个渠道被调用 %d 次，期望 1 次", got)
	}
	if got := healthy.callCount(); got != 1 {
		t.Errorf("第二个渠道被调用 %d 次，期望 1 次（换渠道重试）", got)
	}
	if got := billingCount(t, db, user); got != 1 {
		t.Errorf("账单有 %d 行，期望 1 行——换渠道重试是同一次请求，不该多出一行", got)
	}
	if got := settlementCount(t, db, user); got != 1 {
		t.Errorf("结算有 %d 次，期望 1 次——内部重试只扣一次", got)
	}
	if got := idemCount(t, db, idemKey); got != 1 {
		t.Errorf("幂等记录有 %d 条，期望 1 条", got)
	}
	rec := waitForIdemState(t, db, idemKey, "completed")
	if rec.state != "completed" {
		t.Errorf("记录状态 = %q，期望 completed", rec.state)
	}

	// 重放：不再打上游，也不再扣钱。
	status, second, _ := postIdempotent(t, url, key, idemKey, body)
	if status != http.StatusOK {
		t.Fatalf("重放的状态码 = %d，期望 200。响应体：%s", status, second)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("重放的响应与第一次不同\n第一次：%s\n第二次：%s", first, second)
	}
	if got := broken.callCount(); got != 1 {
		t.Errorf("重放之后第一个渠道被调用 %d 次，期望仍是 1 次", got)
	}
	if got := healthy.callCount(); got != 1 {
		t.Errorf("重放之后第二个渠道被调用 %d 次，期望仍是 1 次", got)
	}
	if got := settlementCount(t, db, user); got != 1 {
		t.Errorf("重放之后结算有 %d 次，期望仍是 1 次", got)
	}
}
