package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/relay"
)

// 幂等键在入口这一层的单元测试。
//
// # 它补的是端到端测不到的那一半
//
// 端到端（internal/assembly）证明的是「钱只扣一次」这类最终事实，但它测不了
// **等待**：要构造「第一次还在跑、第二次等它」需要一个能被卡住的假上游，
// 而要构造「等到超时」需要一个真的会超时的请求——后者在真实链路上表现为
// 客户端断开，那时 409 根本发不出去（没人在听）。所以那条分支只能在这里，
// 用一个按调用次序返回不同结果的桩来钉。
//
// # 桩为什么是「按次序」的
//
// 「永远返回同一个值」的桩测不出等待：它让「轮询到状态变了」与「压根没轮询」
// 长得一样。脚本化的返回值才能表达「第一次读到 in_progress、第二次读到
// completed」这个时序，而那正是这条路径的全部内容。

// ── 桩 ────────────────────────────────────────────────────────────────

// idemReply 是桩对一次 Lookup 的答复。
type idemReply struct {
	rec *idempotency.Record
	err error
}

// fakeIdem 是 IdempotencyStore 的桩。
//
// script 按调用次序取；只剩一项时**重复**它（「永远 in_progress」就是
// 一个只有一项的脚本）。取完不报错也不返回 ErrNotFound，是因为那两种都
// 会静默地改变被测行为——一个「脚本用完了」的错误看起来与「查库失败」
// 一模一样。
type fakeIdem struct {
	mu      sync.Mutex
	script  []idemReply
	lookups int

	completes []idemComplete
	fails     []string
	writeErr  error
}

type idemComplete struct {
	apiKeyID    int64
	key         string
	status      int
	contentType string
	body        []byte
}

func (f *fakeIdem) Lookup(_ context.Context, _ int64, _ string) (*idempotency.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if len(f.script) == 0 {
		return nil, idempotency.ErrNotFound
	}
	r := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	return r.rec, r.err
}

func (f *fakeIdem) Complete(_ context.Context, apiKeyID int64, key string, status int, contentType string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completes = append(f.completes, idemComplete{apiKeyID, key, status, contentType, body})
	return f.writeErr
}

func (f *fakeIdem) Fail(_ context.Context, apiKeyID int64, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = append(f.fails, key)
	return f.writeErr
}

func (f *fakeIdem) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *fakeIdem) completed() []idemComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]idemComplete(nil), f.completes...)
}

func (f *fakeIdem) failed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fails...)
}

// ── 夹具 ──────────────────────────────────────────────────────────────

// idemHandler 起一个带幂等存储的 handler。
//
// 用户带上 KeyID：作用域是它（不是 user id），而「入口把哪个值当作用域
// 传下去」这条只有在两者不同时才测得出来——fakeAuth 的默认用户 ID 是 7、
// KeyID 是 0，那种桩上这两条路径不可区分。
func idemHandler() (*Handler, *fakeIdem, *fakeChatter) {
	auth := &fakeAuth{user: User{ID: 7, KeyID: 42}}
	chatter := &fakeChatter{result: fakeResult()}
	h := New(auth, chatter)
	h.Version = "v0.1.0-test"
	h.Now = func() time.Time { return time.Unix(1700000000, 0) }
	store := &fakeIdem{}
	h.Idempotency = store
	return h, store, chatter
}

// hashOf 算出 handler 会算出的那个指纹。
//
// 用生产里的那两个函数而不是写死一串十六进制：写死的那一串会在任何一次
// 与幂等无关的改动（新增一个进哈希的字段、换序列化方式）之后失效，
// 而失效的表现是**每一条**用例都挂——那时没人分得清是改动错了还是夹具旧了。
func hashOf(t *testing.T, protocol channel.Protocol, body string) [32]byte {
	t.Helper()

	upstream, err := relay.ParseRequest(context.Background(), protocol, []byte(body))
	if err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	hash, err := idempotency.HashRequest(protocol, upstream)
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}
	return hash
}

// completedRecord 造一条可以重放的记录。
func completedRecord(hash [32]byte, body []byte) *idempotency.Record {
	return &idempotency.Record{
		State:               idempotency.StateCompleted,
		Hash:                hash,
		ResponseStatus:      http.StatusOK,
		ResponseContentType: "application/json; charset=utf-8",
		ResponseBody:        body,
	}
}

// postIdem 发一个带 Idempotency-Key 的请求。
func postIdem(t *testing.T, h *Handler, path, body, idemKey string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotency.HeaderName, idemKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postIdemCtx 与 postIdem 相同，但用调用方给的 context。
//
// 「等到超时」那条用例靠它：等待的上限就是这次请求自己的超时，
// 而在这里表达「这次请求的超时」的唯一手段就是这个 context。
func postIdemCtx(ctx context.Context, h *Handler, path, body, idemKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotency.HeaderName, idemKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// errorCodeOf 从错误体里取出机器可读的错误码。
func errorCodeOf(t *testing.T, protocol channel.Protocol, body []byte) string {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v\n%s", err, body)
	}
	errObj, ok := obj["error"].(map[string]any)
	if !ok {
		t.Fatalf("错误体里没有 error 对象：%s", body)
	}
	field := "code"
	if protocol == channel.ProtocolAnthropic {
		field = "type"
	}
	code, _ := errObj[field].(string)
	return code
}

// ── 用例 ──────────────────────────────────────────────────────────────

// logCapture 收下这个用例产生的日志。
//
// 断言「**没打**某条日志」必须有个能看见日志的地方：没有它，那条用例在
// 「日志根本没被接出来」与「确实没打」之间分不出来，而这两种情形在测试
// 输出里长得一模一样（LESSONS.md 第 16 条）。所以配套的那条「仍然要打」
// 的用例不只是对照，它同时证明这个捕获器在工作。
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// logger 造一个把日志写进本捕获器的日志器。
//
// 级别给 debug：要断言的正是「正常路径上不该出现的那些」，
// 而它们会被降到 debug，级别卡在 info 就看不见了。
func (c *logCapture) logger() *slog.Logger {
	return observe.NewLoggerWithWriter(c, "debug", "text", false)
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// 没有这个头时，幂等那一层一步都不该被碰。
//
// 这是「行为完全不变」在入口这一层的证据：不查库、不写记录、请求上也
// 不带声明——带了的话落库层会去写幂等记录，而那会让一条普通的请求多一次写。
func TestNoIdempotencyKeyTouchesNothing(t *testing.T) {
	h, store, chatter := idemHandler()

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := store.lookupCount(); got != 0 {
		t.Errorf("查了 %d 次幂等记录，期望 0 次（没有这个头）", got)
	}
	if got := len(store.completed()) + len(store.failed()); got != 0 {
		t.Errorf("写了 %d 次幂等记录，期望 0 次", got)
	}
	if chatter.req.Idempotency != nil {
		t.Error("请求上带了幂等声明——落库层会因此多写一条记录")
	}
}

// 命中的记录直接重放，一个字都不改：状态码、Content-Type、响应体。
func TestIdempotentReplaysCompletedResponse(t *testing.T) {
	h, store, chatter := idemHandler()
	const key = "key-replay"
	stored := []byte(`{"id":"stored-response","object":"chat.completion"}`)
	store.script = []idemReply{{rec: completedRecord(hashOf(t, channel.ProtocolOpenAI, validBody), stored)}}

	rec := postIdem(t, h, PathChatCompletions, validBody, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), stored) {
		t.Errorf("重放的响应体 = %s，期望 %s（逐字节相同）", rec.Body, stored)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q，期望留档的那一个", got)
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次（重放不该走到上游）", chatter.calls)
	}
}

// 同键不同体：报错，且不碰上游。
func TestIdempotentDifferentBodyConflicts(t *testing.T) {
	h, store, chatter := idemHandler()
	const key = "key-conflict"
	// 留档的指纹来自**另一份**请求体。
	other := `{"model":"m-1","messages":[{"role":"user","content":"另一段话"}],"max_tokens":16}`
	store.script = []idemReply{{rec: completedRecord(hashOf(t, channel.ProtocolOpenAI, other), []byte(`{}`))}}

	rec := postIdem(t, h, PathChatCompletions, validBody, key)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409。响应体：%s", rec.Code, rec.Body)
	}
	if got := errorCodeOf(t, channel.ProtocolOpenAI, rec.Body.Bytes()); got != CodeIdempotencyKeyReused {
		t.Errorf("错误码 = %q，期望 %q", got, CodeIdempotencyKeyReused)
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", chatter.calls)
	}
}

// 第一次没走完：不重放，报 409，且错误码与「同键不同体」那条**不同**。
//
// 两条 409 的处置不一样（一条该换新键重发，一条该等），所以码必须分得开。
func TestIdempotentFailedRecordIsNotReplayed(t *testing.T) {
	h, store, chatter := idemHandler()
	const key = "key-incomplete"
	store.script = []idemReply{{rec: &idempotency.Record{
		State: idempotency.StateFailed,
		Hash:  hashOf(t, channel.ProtocolOpenAI, validBody),
	}}}

	rec := postIdem(t, h, PathChatCompletions, validBody, key)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409。响应体：%s", rec.Code, rec.Body)
	}
	got := errorCodeOf(t, channel.ProtocolOpenAI, rec.Body.Bytes())
	if got != CodeIdempotencyIncomplete {
		t.Errorf("错误码 = %q，期望 %q", got, CodeIdempotencyIncomplete)
	}
	if got == CodeIdempotencyKeyReused {
		t.Error("这条 409 的码与「同键不同体」那条相同——客户端分不出该换键还是该等")
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", chatter.calls)
	}
}

// 并发同键：等第一次跑完，再按重放规则返回。
//
// 断言里带时间：只断言「最后重放了」的话，一个**不等待、直接重试**的实现
// 也会通过（它第二次查就查到了 completed）。要区分两者，必须看到它真的
// 等了，而且中间又查了一次。
func TestIdempotentWaitsForInProgressThenReplays(t *testing.T) {
	h, store, chatter := idemHandler()
	const key = "key-wait"
	hash := hashOf(t, channel.ProtocolOpenAI, validBody)
	stored := []byte(`{"id":"stored-response"}`)
	store.script = []idemReply{
		{rec: &idempotency.Record{State: idempotency.StateInProgress, Hash: hash}},
		{rec: completedRecord(hash, stored)},
	}

	start := time.Now()
	rec := postIdem(t, h, PathChatCompletions, validBody, key)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), stored) {
		t.Errorf("响应体 = %s，期望重放的那一份 %s", rec.Body, stored)
	}
	if got := store.lookupCount(); got < 2 {
		t.Errorf("只查了 %d 次幂等记录，期望至少 2 次——查到 in_progress 之后必须再查一次才知道它变没变", got)
	}
	// 断言落在**字面量**上，不落在 idempotencyPollInterval 上。
	//
	// 拿被测的常量当期望，测的就是「相对关系」而不是「真的等了多久」：
	// 把 idempotencyPollInterval 从 100ms 改成 1µs（等于每次撞键都忙轮询
	// 数据库，查询量涨五个数量级），这条断言照样通过。同一个文件里的
	// TestValidateKey 与 TestRetention 都刻意用了字面量。
	//
	// 50ms 是半个轮询间隔：真的等了一个间隔就一定跨过它，忙轮询就一定不到它，
	// 两边各留一倍余量。常量本身由 TestPollIntervalIs100ms 单独钉住。
	if elapsed < 50*time.Millisecond {
		t.Errorf("耗时 %s，不足 50ms——它没有等一个轮询间隔，而是立刻又查了一次", elapsed)
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", chatter.calls)
	}
}

// 轮询间隔就是 100ms。
//
// 上一条只能证明「至少等了 50ms」，证明不了「等的是 100ms」——把常量改成
// 60ms，它照样绿。而这个常量是「撞键的请求要多等多久」与「一次长请求要
// 产生多少次查询」之间唯一的旋钮：它既是调用方感知到的额外延迟，也是
// 库上多出来的查询量。随手改掉它应该让某条用例挂掉。
func TestPollIntervalIs100ms(t *testing.T) {
	if idempotencyPollInterval != 100*time.Millisecond {
		t.Errorf("idempotencyPollInterval = %s，期望 100ms（字面量，"+
			"不是拿它自己测自己）", idempotencyPollInterval)
	}
}

// 等到请求自己的超时还在处理中：409，且码与另外两条都不同。
func TestIdempotentWaitTimesOutIntoConflict(t *testing.T) {
	h, store, _ := idemHandler()
	const key = "key-timeout"
	store.script = []idemReply{{rec: &idempotency.Record{
		State: idempotency.StateInProgress,
		Hash:  hashOf(t, channel.ProtocolOpenAI, validBody),
	}}}

	// 「请求自己的超时」在这里就是调用方的 context。300ms 是刻意的：
	// 它比轮询间隔大得多，于是「等了一轮以上」这件事是必然发生的，
	// 而不是碰运气。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	rec := postIdemCtx(ctx, h, PathChatCompletions, validBody, key)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409。响应体：%s", rec.Code, rec.Body)
	}
	got := errorCodeOf(t, channel.ProtocolOpenAI, rec.Body.Bytes())
	if got != CodeIdempotencyInProgress {
		t.Errorf("错误码 = %q，期望 %q", got, CodeIdempotencyInProgress)
	}
	if got := store.lookupCount(); got < 2 {
		t.Errorf("只查了 %d 次，期望至少 2 次——它没有轮询，而是一次就放弃了", got)
	}
}

// 查库失败时**不能**当成「没命中」继续走。
//
// 那会把一次数据库抖动变成一次真的重复调用，而重复调用正是这个功能要防的事。
func TestIdempotentLookupFailureFailsClosed(t *testing.T) {
	h, store, chatter := idemHandler()
	store.script = []idemReply{{err: errors.New("placeholder: 数据库连不上")}}

	rec := postIdem(t, h, PathChatCompletions, validBody, "key-dberror")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（查不动时宁可失败，也不能当成没命中）", rec.Code)
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次——查库失败时放行等于关掉了幂等", chatter.calls)
	}
}

// 建立记录时被并发请求抢走：不是失败，而是转去等它出结果。
func TestIdempotentClaimRaceFallsBackToWaiting(t *testing.T) {
	h, store, chatter := idemHandler()
	const key = "key-race"
	hash := hashOf(t, channel.ProtocolOpenAI, validBody)
	stored := []byte(`{"id":"winner-response"}`)

	// 时序就是这段竞态的本来面目：先查——还没有记录（那个赢家的事务还没提交）；
	// 于是走原路径——调上游时键已被占住（落库层返回 ErrClaimed）；再查——
	// 这时那条记录已经是 completed。
	chatter.err = idempotency.ErrClaimed
	store.script = []idemReply{
		{err: idempotency.ErrNotFound},
		{rec: completedRecord(hash, stored)},
	}

	rec := postIdem(t, h, PathChatCompletions, validBody, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（撞键不是失败）。响应体：%s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), stored) {
		t.Errorf("响应体 = %s，期望赢的那个请求的响应 %s", rec.Body, stored)
	}
	if got := len(store.failed()); got != 0 {
		t.Errorf("把撞键记成了 %d 次失败——撞键时这次请求什么都没做，记成失败会让同键的下一次重试也拿不到结果", got)
	}
}

// 走完的响应要原样存下来，连同这次请求的幂等声明一起。
func TestIdempotentStoresRenderedResponse(t *testing.T) {
	h, store, chatter := idemHandler()
	const key = "key-store"

	rec := postIdem(t, h, PathChatCompletions, validBody, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	// 声明必须跟着请求走到落库层：少了它，幂等记录与预留就写不到一个事务里。
	if chatter.req.Idempotency == nil {
		t.Fatal("请求上没有幂等声明——落库层不会写幂等记录")
	}
	if got := chatter.req.Idempotency.APIKeyID; got != 42 {
		t.Errorf("作用域 = %d，期望 42（那把 API Key 的 id，不是用户 id）", got)
	}
	if got := chatter.req.Idempotency.Key; got != key {
		t.Errorf("键 = %q，期望 %q", got, key)
	}
	if got := chatter.req.Idempotency.Hash; got != hashOf(t, channel.ProtocolOpenAI, validBody) {
		t.Error("请求上的指纹与从请求体算出来的不一致——同键同体会被误判成不同体")
	}

	done := store.completed()
	if len(done) != 1 {
		t.Fatalf("写回响应 %d 次，期望 1 次", len(done))
	}
	if done[0].status != http.StatusOK {
		t.Errorf("留档的状态码 = %d，期望 200", done[0].status)
	}
	if done[0].contentType != "application/json; charset=utf-8" {
		t.Errorf("留档的 Content-Type = %q", done[0].contentType)
	}
	// 存的必须就是**发出去的那串字节**，而不是「又渲染了一遍」。
	if !bytes.Equal(done[0].body, rec.Body.Bytes()) {
		t.Errorf("留档的响应体与发出去的不同\n留档：%s\n发出：%s", done[0].body, rec.Body)
	}
	if got := len(store.failed()); got != 0 {
		t.Errorf("把一次成功的请求记成了 %d 次失败", got)
	}
}

// 上游失败：留档记成失败，而响应照常是一个错误响应。
func TestIdempotentRecordsFailure(t *testing.T) {
	h, store, chatter := idemHandler()
	chatter.err = errors.New("placeholder: 上游炸了")

	rec := postIdem(t, h, PathChatCompletions, validBody, "key-fail")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if got := store.failed(); len(got) != 1 || got[0] != "key-fail" {
		t.Errorf("失败记录 = %v，期望一条 key-fail——不记失败的话，失败的重试会一路重试到底", got)
	}
	if got := len(store.completed()); got != 0 {
		t.Errorf("把一次失败记成了 %d 次成功", got)
	}
}

// 预留失败（记录根本不在）时**不该**打「写幂等结局失败」那条 ERROR。
//
// # 为什么这一条是必要的
//
// 那条 ERROR 是这个设计里唯一能指出「记录停在 in_progress」的信号，而
// 预留回滚的每一次（余额不足、没有可用渠道、缺 max_tokens）都会走到这里：
// 记录随事务一起回滚，于是 Fail/Complete 影响 0 行。这些全是**正常路径**，
// 让告警在这里每次都响，等于把那个信号淹掉。
func TestIdempotentMissingRecordDoesNotLogWriteFailure(t *testing.T) {
	h, store, _ := idemHandler()
	store.writeErr = idempotency.ErrNotFound

	logs := &logCapture{}
	ctx := observe.WithLogger(context.Background(), logs.logger())

	rec := postIdemCtx(ctx, h, PathChatCompletions, validBody, "key-no-record")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}
	// 前提：确实走到了写回那一步——否则这条用例证明不了任何事。
	if got := len(store.completed()); got != 1 {
		t.Fatalf("写回被调用了 %d 次，期望 1 次（没走到写回就没有告警可言）", got)
	}
	if got := logs.text(); strings.Contains(got, "写幂等结局失败") {
		t.Errorf("记录不存在时打了「写幂等结局失败」——那是正常路径，"+
			"它会让这条唯一的告警在每次余额不足时都响一遍\n日志：%s", got)
	} else if !strings.Contains(got, "没建成幂等记录") {
		t.Errorf("既没有那条 ERROR，也没有那句 Debug——这条用例就只证明了"+
			"「什么日志都没打」，证明不了「走的是正常路径那条分支」\n日志：%s", got)
	}
}

// 记录在、但真的写不进去时**仍然**要打那条 ERROR。
//
// 与上一条配对：只有两条都在，才能说明这里做的是「把两种 0 行分开」，
// 而不是「把告警关掉」。这条里记录停在 in_progress（写失败的就是它），
// 而那正是那条日志存在的全部理由——同键的下一次请求会一直等到自己的
// 超时才拿到 409，真正的原因只在这条日志里。
func TestIdempotentWriteFailureStillLogs(t *testing.T) {
	h, store, _ := idemHandler()
	store.writeErr = errors.New("placeholder: 数据库写不进去")

	logs := &logCapture{}
	ctx := observe.WithLogger(context.Background(), logs.logger())

	rec := postIdemCtx(ctx, h, PathChatCompletions, validBody, "key-write-fail")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（写库失败改不了已经发出去的响应）。响应体：%s", rec.Code, rec.Body)
	}

	got := logs.text()
	// 断言的是 **ERROR 级**，不只是那段文字：把这条日志降级成 Debug 与删掉它
	// 在现场是同一件事——默认级别下没人看得见——而只比对文字的断言对前者
	// 完全无感。
	if !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "写幂等结局失败") {
		t.Errorf("记录在而写不进去时没打那条 **ERROR**——记录会停在 in_progress，"+
			"而这条日志是唯一能指出它的信号（降级成 Debug 也算没打）\n日志：%s", got)
	}
	if !strings.Contains(got, "key-write-fail") {
		t.Errorf("那条日志里没有键名——排查时它是唯一的入口\n日志：%s", got)
	}
}

// 键本身不合法：当场 400，一个副作用都不留。
func TestIdempotentRejectsInvalidKey(t *testing.T) {
	h, store, chatter := idemHandler()

	rec := postIdem(t, h, PathChatCompletions, validBody, "带中文的键")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400。响应体：%s", rec.Code, rec.Body)
	}
	if got := store.lookupCount(); got != 0 {
		t.Errorf("查了 %d 次幂等记录，期望 0 次（键不合法，不该走到那里）", got)
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", chatter.calls)
	}
}

// 流式：存的是那串 SSE 字节，重放就是原样再发一遍。
func TestIdempotentStoresStreamBytesAndReplays(t *testing.T) {
	h, store, chatter := idemHandler()
	chatter.streamResult = &gateway.Result{Events: streamOf(
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		channel.StreamEvent{Kind: "done"},
	)}
	const key = "key-stream"

	first := postIdem(t, h, PathChatCompletions, streamBody, key)
	if first.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", first.Code, first.Body)
	}
	done := store.completed()
	if len(done) != 1 {
		t.Fatalf("写回流式响应 %d 次，期望 1 次", len(done))
	}
	if done[0].contentType != sseContentType {
		t.Errorf("留档的 Content-Type = %q，期望 %q", done[0].contentType, sseContentType)
	}
	if !bytes.Equal(done[0].body, first.Body.Bytes()) {
		t.Errorf("留档的流与发出去的不同\n留档：%q\n发出：%q", done[0].body, first.Body.Bytes())
	}
	if !bytes.Contains(done[0].body, []byte("data: [DONE]")) {
		t.Errorf("留档的流里没有结束标记——重放出去客户端会以为回答是半截的：%q", done[0].body)
	}

	// 第二次：同一个键，拿到的是那串字节，且没有再去调上游。
	//
	// 指纹按**流式的**请求体算。这一条也在钉「指纹进了 stream 这个字段」：
	// 漏掉它的话，同一次对话的流式与非流式请求会得到同一个指纹，
	// 于是流式重试会拿到非流式的那份响应体。
	store.script = []idemReply{{rec: &idempotency.Record{
		State:               idempotency.StateCompleted,
		Hash:                hashOf(t, channel.ProtocolOpenAI, streamBody),
		ResponseStatus:      http.StatusOK,
		ResponseContentType: sseContentType,
		ResponseBody:        done[0].body,
	}}}

	second := postIdem(t, h, PathChatCompletions, streamBody, key)
	if second.Code != http.StatusOK {
		t.Fatalf("重放的状态码 = %d，期望 200。响应体：%s", second.Code, second.Body)
	}
	if !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) {
		t.Errorf("重放的字节与第一次不同\n第一次：%q\n第二次：%q", first.Body.Bytes(), second.Body.Bytes())
	}
	if got := second.Header().Get("Content-Type"); got != sseContentType {
		t.Errorf("重放的 Content-Type = %q，期望 %q", got, sseContentType)
	}
	if chatter.streamCalls != 1 {
		t.Errorf("上游被调用了 %d 次，期望 1 次（只有第一次）", chatter.streamCalls)
	}
}

// 截断的流**不能**被重放。
//
// 没有 done 就是半截回答，把它存成「走完了」会让后续同键请求拿到一段
// 看起来正常、其实少了一半的响应——那比不重放更坏。
func TestIdempotentTruncatedStreamIsNotCompleted(t *testing.T) {
	h, store, _ := idemHandler()
	h.Chatter.(*fakeChatter).streamResult = &gateway.Result{Events: streamOf(
		channel.StreamEvent{Kind: "delta", Text: "半截"},
	)}

	rec := postIdem(t, h, PathChatCompletions, streamBody, "key-truncated")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（帧已经发出去了，改不了状态码）", rec.Code)
	}
	if got := len(store.completed()); got != 0 {
		t.Errorf("把一段截断的流记成了 %d 次成功——重放它会骗过客户端", got)
	}
	if got := store.failed(); len(got) != 1 {
		t.Errorf("失败记录 = %v，期望一条", got)
	}
}

// 两套协议的错误体形状各断言一次。
//
// 码放在哪个字段上两套不同（见 error.go），而调用方的 SDK 按自己那套解——
// 放错了它连错误类型都读不出来。
func TestIdempotentConflictShapePerProtocol(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		protocol channel.Protocol
		body     string
	}{
		{name: "openai", path: PathChatCompletions, protocol: channel.ProtocolOpenAI, body: validBody},
		{name: "anthropic", path: PathMessages, protocol: channel.ProtocolAnthropic,
			body: `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, _ := idemHandler()
			other := strings.Replace(tc.body, "hi", "另一段话", 1)
			store.script = []idemReply{{rec: completedRecord(hashOf(t, tc.protocol, other), []byte(`{}`))}}

			rec := postIdem(t, h, tc.path, tc.body, "key-shape")
			if rec.Code != http.StatusConflict {
				t.Fatalf("状态码 = %d，期望 409。响应体：%s", rec.Code, rec.Body)
			}

			var obj map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
				t.Fatalf("错误体不是合法 JSON: %v\n%s", err, rec.Body)
			}
			errObj, _ := obj["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("错误体里没有 error 对象：%s", rec.Body)
			}
			if got := errorCodeOf(t, tc.protocol, rec.Body.Bytes()); got != CodeIdempotencyKeyReused {
				t.Errorf("错误码 = %q，期望 %q", got, CodeIdempotencyKeyReused)
			}
			if msg, _ := errObj["message"].(string); msg == "" {
				t.Error("错误体里没有 message——调用方要靠它知道下一步该做什么")
			}
		})
	}
}
