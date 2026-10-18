package snapshotwire

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/snapshot"
)

func testConfig() Config {
	return Config{Enabled: true, MaxBytes: 65536, RetentionDays: 30}
}

// newCollector 建一条「永远没有授权」的链路。
//
// 默认这一档是有意的：绝大多数用例判的是骨架那条路，而「没有授权」正是
// 它今天在装配层拿到的那个答案（见 NoConsentSource）。
func newCollector(t *testing.T, cfg Config, db DB) *Collector {
	t.Helper()
	return newCollectorWithConsent(t, cfg, db, NoConsentSource{})
}

// newCollectorWithConsent 建一条按给定授权来源判定的链路。
func newCollectorWithConsent(t *testing.T, cfg Config, db DB, consent ConsentSource) *Collector {
	t.Helper()
	c, err := New(cfg, db, consent, quietLogger())
	if err != nil {
		t.Fatalf("建立快照链路失败: %v", err)
	}
	return c
}

// 关掉采集时返回 nil，而不是一个「什么都不做」的空实现：
// 网关对 nil 的处理是根本不采集，对空实现的处理是采集一条空快照再落库。
//
// 这条钉的是「不写」，不是「什么都不做」——清理照样跑，见
// TestCleanupRunsWhenCollectingDisabled。
func TestDisabledCollectorDoesNotCapture(t *testing.T) {
	db := &fakeDB{}
	c := newCollector(t, Config{Enabled: false, RetentionDays: 30}, db)
	if c.Collecting() {
		t.Error("关掉采集时 Collecting 应为 false")
	}
	if c.Redactor() != nil {
		t.Error("关掉采集时 Redactor 应为 nil")
	}
	if c.Sink() != nil {
		t.Error("关掉采集时 Sink 应为 nil")
	}

	// 即使有人硬塞一条进来，也不该写库。
	if err := c.Save(context.Background(), newSnap()); err != nil {
		t.Errorf("关掉采集时 Save 不该报错: %v", err)
	}
	if db.execCount() != 0 {
		t.Errorf("关掉采集时不该产生任何 SQL，实际 %d 条", db.execCount())
	}
}

func TestEnabledCollectorSatisfiesGatewayInterfaces(t *testing.T) {
	c := newCollector(t, testConfig(), &fakeDB{})
	// 「是否满足网关那两个接口」由包里的编译期断言保证，这里验证的是
	// 启用时取得到东西、且取到的东西真的能用。
	r := c.Redactor()
	s := c.Sink()
	if r == nil {
		t.Fatal("启用时 Redactor 该有实现")
	}
	if s == nil {
		t.Fatal("启用时 Sink 该有实现")
	}
	// 判据是「取出来的这个能用」，所以身份给零值：授权那一层用不上它
	// （这条链路接的是 NoConsentSource），而零值正是「拿不到身份」的形状。
	if _, err := r.Redact(context.Background(), &channel.Request{Model: "placeholder-model"},
		gateway.CaptureIdentity{}); err != nil {
		t.Errorf("取出来的 Redactor 应当能用: %v", err)
	}
	if err := s.Save(context.Background(), newSnap()); err != nil {
		t.Errorf("取出来的 Sink 应当能用: %v", err)
	}
}

// 上限非法时拒绝建立链路，而不是回落到默认值：
// 上限配错会让每条骨架都降级成摘要层，静默回落会让它永不显形。
func TestNewRejectsNonPositiveMaxBytes(t *testing.T) {
	for _, n := range []int{0, -1, -65536} {
		cfg := testConfig()
		cfg.MaxBytes = n
		if _, err := New(cfg, &fakeDB{}, NoConsentSource{}, quietLogger()); err == nil {
			t.Errorf("max_bytes = %d 时应当报错", n)
		}
	}
}

// 两个方向各一个脱敏器：内部实现只服务一个方向，接错方向会被它自己挡住。
func TestRedactBridgesBothDirections(t *testing.T) {
	c := newCollector(t, testConfig(), &fakeDB{})

	req := &channel.Request{
		Model:     "gpt-4o-2024",
		MaxTokens: 512,
		Messages: []channel.Message{{
			Role:    "user",
			Content: []channel.ContentBlock{{Type: "text", Text: "帮我看看这段"}},
		}},
	}
	snap, err := c.Redact(context.Background(), req, gateway.CaptureIdentity{})
	if err != nil {
		t.Fatalf("脱敏请求失败: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("skeleton_version = %d，期望 1", snap.Version)
	}
	if _, ok := snap.Skeleton["model"]; !ok {
		t.Errorf("骨架里应当有 model，实际键为 %v", keysOf(snap.Skeleton))
	}
	// 原样文本绝不能出现在骨架里。
	assertNoContent(t, snap, "帮我看看这段")
	// 没有授权时落的就是默认那一档。
	if snap.CaptureMode != captureModeSkeleton {
		t.Errorf("capture_mode = %q，期望 %q", snap.CaptureMode, captureModeSkeleton)
	}

	// 调用方负责填归属信息，脱敏本身不碰它们。
	if snap.RequestID != "" || snap.Direction != "" || snap.UserID != 0 {
		t.Errorf("脱敏不该填归属信息，实际 %q / %q / %d", snap.RequestID, snap.Direction, snap.UserID)
	}
}

func TestRedactResponseBridgesDirection(t *testing.T) {
	c := newCollector(t, testConfig(), &fakeDB{})
	resp := &channel.Response{
		UpstreamModel: "gpt-4o-2024",
		Message: channel.Message{
			Role:    "assistant",
			Content: []channel.ContentBlock{{Type: "text", Text: "上游回复正文"}},
		},
		Usage: channel.Usage{PromptTokens: 100, CompletionTokens: 20},
	}
	snap, err := c.RedactResponse(context.Background(), resp, gateway.CaptureIdentity{})
	if err != nil {
		t.Fatalf("脱敏响应失败: %v", err)
	}
	assertNoContent(t, snap, "上游回复正文")
}

// 没有可脱敏的响应体时返回空骨架，而不是错误。
//
// nil 是一个正常结局（连不上、超时、非 2xx 的正文没有归一化），那一行
// 仍然要落——它的内容是 upstream_status 与 error_kind 两列。返回错误的话
// 网关会把整条快照丢掉，「连不上上游」这件事就不会出现在表里。
func TestRedactResponseNilGivesEmptySkeleton(t *testing.T) {
	c := newCollector(t, testConfig(), &fakeDB{})
	snap, err := c.RedactResponse(context.Background(), nil, gateway.CaptureIdentity{})
	if err != nil {
		t.Fatalf("nil 响应不该报错: %v", err)
	}
	if snap == nil {
		t.Fatal("nil 响应也该给出一条可落库的快照")
	}
	if snap.Version != 1 {
		t.Errorf("skeleton_version = %d，期望 1", snap.Version)
	}
	if len(snap.Skeleton) != 0 {
		t.Errorf("没有响应体时骨架应当为空，实际 %v", snap.Skeleton)
	}
	if snap.Direction != "" || snap.UserID != 0 {
		t.Errorf("脱敏不该填归属信息，实际 %q / %d", snap.Direction, snap.UserID)
	}
}

// 骨架里有不可序列化的值时，Save 只报错、不 panic，也不让调用方以为成功了。
func TestSaveReportsInsertFailure(t *testing.T) {
	c := newCollector(t, testConfig(), &fakeDB{})
	snap := newSnap()
	snap.Skeleton = map[string]any{"bad": make(chan int)}

	before := c.Stats()
	if err := c.Save(context.Background(), snap); err == nil {
		t.Fatal("落库失败时应当返回错误")
	}
	after := c.Stats()
	if after.Failed != before.Failed+1 {
		t.Errorf("Failed = %d，期望 %d", after.Failed, before.Failed+1)
	}
	if after.Written != before.Written {
		t.Errorf("失败的那条不该计入 Written")
	}
}

func TestSaveNilSnapshotIsNoop(t *testing.T) {
	db := &fakeDB{}
	c := newCollector(t, testConfig(), db)
	if err := c.Save(context.Background(), nil); err != nil {
		t.Errorf("空快照不该报错: %v", err)
	}
	if db.execCount() != 0 {
		t.Errorf("空快照不该产生 SQL，实际 %d 条", db.execCount())
	}
}

// 并发上限：满了立刻返回 ErrCapacityExceeded，**不排队等**。
//
// 排队等待看起来能多救几条快照，代价是过载时堆起一批 goroutine 与内存里的
// 骨架，并与计费路径争同一批数据库连接。丢快照是既定行为，堆积不是。
func TestSaveDropsWhenSaturated(t *testing.T) {
	release := make(chan struct{})
	db := &blockingDB{release: release}
	c := newCollector(t, testConfig(), db)

	// 占满全部令牌。
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentWrites; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Save(context.Background(), newSnap())
		}()
	}
	waitUntil(t, func() bool { return db.started() == maxConcurrentWrites })

	// 再来的就地丢弃。
	done := make(chan error, 1)
	go func() { done <- c.Save(context.Background(), newSnap()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCapacityExceeded) {
			t.Errorf("满了时应当返回 ErrCapacityExceeded，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("满了之后 Save 还在等——它不该排队")
	}
	if got := c.Stats().Dropped; got != 1 {
		t.Errorf("Dropped = %d，期望 1", got)
	}

	close(release)
	wg.Wait()
}

// 令牌必须释放，否则一次瞬时高峰会让之后所有快照都被丢掉。
func TestSlotsAreReleasedAfterWrite(t *testing.T) {
	db := &fakeDB{}
	c := newCollector(t, testConfig(), db)

	for i := 0; i < maxConcurrentWrites*3; i++ {
		if err := c.Save(context.Background(), newSnap()); err != nil {
			t.Fatalf("第 %d 次 Save 出错: %v", i, err)
		}
	}
	stats := c.Stats()
	if stats.Dropped != 0 {
		t.Errorf("串行写入不该出现丢弃，实际丢了 %d 条", stats.Dropped)
	}
	if stats.Written != int64(maxConcurrentWrites*3) {
		t.Errorf("Written = %d，期望 %d", stats.Written, maxConcurrentWrites*3)
	}
}

// 四个计数不只要自己记着，还要真的出现在 /metrics 的正文里。
//
// # 为什么钉渲染出来的那一行，而不是 counter 变量
//
// Stats() 与指标是同一批事件的两份口径（见它的注释），但只有后者会被人看见：
// 变量涨了而登记漏了、或者名字写错了，读变量是对的而抓下来的正文里没有那一行。
// 「没接上的计数器」与「一次都没发生的计数器」在采集端长得一样，所以这条
// 用例判的是输出，不是内存里的数。
//
// # 为什么逐个事件读增量
//
// observe.Metrics 是**进程级**的：同一个测试二进制里别的用例也会往这四个
// 计数器上加数，所以判据只能是「这件事之后它涨了 1」。
func TestSnapshotCountsAppearOnMetrics(t *testing.T) {
	// 1. 落库成功。
	expectMetricDelta(t, "larkax_snapshot_written_total", func() {
		if err := newCollector(t, testConfig(), &fakeDB{}).Save(context.Background(), newSnap()); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
	})

	// 2. 并发写已满而就地丢弃。占满令牌之后那一条立刻返回，不等令牌释放。
	expectMetricDelta(t, "larkax_snapshot_dropped_total", func() {
		release := make(chan struct{})
		db := &blockingDB{release: release}
		c := newCollector(t, testConfig(), db)

		var wg sync.WaitGroup
		for i := 0; i < maxConcurrentWrites; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.Save(context.Background(), newSnap())
			}()
		}
		waitUntil(t, func() bool { return db.started() == maxConcurrentWrites })

		if err := c.Save(context.Background(), newSnap()); !errors.Is(err, ErrCapacityExceeded) {
			t.Fatalf("满了时应当返回 ErrCapacityExceeded，实际 %v", err)
		}
		// 放行占着令牌的那些写入再继续：它们会各自记一次 written，
		// 而这一条判的是 dropped 的增量，两者互不干扰。
		close(release)
		wg.Wait()
	})

	// 3. 落库失败：骨架里塞一个 JSON 序列化不了的值，写库之前就报错。
	expectMetricDelta(t, "larkax_snapshot_failed_total", func() {
		snap := newSnap()
		snap.Skeleton = map[string]any{"bad": make(chan int)}
		if err := newCollector(t, testConfig(), &fakeDB{}).Save(context.Background(), snap); err == nil {
			t.Fatal("落库失败时应当返回错误")
		}
	})

	// 4. 脱敏阶段整条丢弃：上限小到连降级后的摘要层都装不下。
	//
	// 它记的是「上限配错了」而不是「存储坏了」，所以必须与上面两条分开——
	// 三种丢弃混成一个数，排查方向就没了。
	expectMetricDelta(t, "larkax_snapshot_oversized_total", func() {
		cfg := testConfig()
		cfg.MaxBytes = 1
		c := newCollector(t, cfg, &fakeDB{})
		if _, err := c.Redact(context.Background(), &channel.Request{Model: "placeholder-model"}, gateway.CaptureIdentity{}); !errors.Is(err, snapshot.ErrTooLarge) {
			t.Fatalf("上限 1 字节时应当整条丢弃并带 ErrTooLarge 标记，实际 %v", err)
		}
	})
}

// 请求标识从 context 里带进日志，这条链路就是给排障用的，日志拼不上等于白记。
func TestLogCarriesRequestTagWithoutPanic(t *testing.T) {
	c := newCollector(t, testConfig(), &fakeDB{})
	ctx := contextWithRequestID(t, "req-from-ctx")
	if err := c.Save(ctx, newSnap()); err != nil {
		t.Fatalf("Save 出错: %v", err)
	}
}

// 留存期与 DropExpired 的配合：留存天数来自配置，不在命令行另设一份。
func TestCollectorDropExpiredUsesConfiguredRetention(t *testing.T) {
	cfg := testConfig()
	cfg.RetentionDays = 3
	db := &fakeDB{rows: &fakeRows{names: []string{
		"request_snapshots_2026_10_10",
		"request_snapshots_2026_10_15",
		"request_snapshots_default",
	}}}
	c := newCollector(t, cfg, db)

	dropped, err := c.DropExpired(context.Background(), testDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != "request_snapshots_2026_10_10" {
		t.Errorf("删除清单 = %v，期望只含 2026_10_10（留存 3 天）", dropped)
	}

	// 演练给出的清单必须与之一致。
	listed, err := c.ExpiredPartitions(context.Background(), testDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0] != dropped[0] {
		t.Errorf("演练清单 = %v，实际删除 %v", listed, dropped)
	}
}

// 关掉采集**不**关掉清理：写新快照与清存量是两件事。
//
// 把两者绑在同一个谓词上的后果，是「关掉采集」这个动作顺带废掉了留存承诺——
// 而关掉采集的动机通常正是「不想让快照再涨」，那时存量分区反而更需要被清掉。
// 而那种失效是静默的：命令照样打印「已删除：0 个」并退出 0。
//
// 这条用例的前身（TestDropExpiredDisabledCollectorIsNoop）断言的是它的反面：
// 关掉采集时 DropExpired 一个分区都不许删、一条 DDL 都不许执行。它把缺陷本身
// 钉住了。改判据之前先确认原来那条钉的是哪一件事——同一个文件里
// TestDisabledCollectorDoesNotCapture 钉的是「不写新快照」，那一条是对的。
func TestCleanupRunsWhenCollectingDisabled(t *testing.T) {
	cfg := Config{Enabled: false, RetentionDays: 3}
	db := &fakeDB{rows: &fakeRows{names: []string{
		"request_snapshots_2026_10_10",
		"request_snapshots_2026_10_15",
		"request_snapshots_default",
	}}}
	c := newCollector(t, cfg, db)

	// 前提：采集确实是关着的。不成立的话下面断言的东西就不是这条用例要说的。
	if c.Collecting() || c.Sink() != nil {
		t.Fatal("这条用例的前提是采集关着")
	}

	dropped, err := c.DropExpired(context.Background(), testDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != "request_snapshots_2026_10_10" {
		t.Errorf("删除清单 = %v，期望只含 2026_10_10（留存 3 天）", dropped)
	}
	// 判据是**真的执行了 DROP**，不是返回值里有个名字：只说删了、没删，
	// 正是这条链路原来的失败方式。
	if db.execCount() != 1 {
		t.Fatalf("Exec 次数 = %d，期望 1（DROP 掉那个到期分区）", db.execCount())
	}
	if got := db.lastExec(); got != "DROP TABLE IF EXISTS request_snapshots_2026_10_10" {
		t.Errorf("执行的语句 = %q，期望 DROP 掉 2026_10_10", got)
	}

	// 演练走同一条路，采集关着也要照实列出来。
	listed, err := c.ExpiredPartitions(context.Background(), testDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0] != dropped[0] {
		t.Errorf("演练清单 = %v，实际删除 %v", listed, dropped)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

// blockingDB 让写入停在 DDL 之前，用来把并发写占满。
type blockingDB struct {
	mu       sync.Mutex
	release  chan struct{}
	inFlight int
}

func (b *blockingDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	b.mu.Lock()
	b.inFlight++
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return fakeResult{}, nil
}

func (b *blockingDB) QueryContext(context.Context, string, ...any) (Rows, error) {
	return &fakeRows{}, nil
}

func (b *blockingDB) started() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// assertNoContent 断言原文没有出现在骨架里。
//
// 逐层遍历而不是只看顶层：内容可能藏在任意一层，而「只查顶层」的断言
// 在字段改到第二层之后就会静默失效。
func assertNoContent(t *testing.T, snap *gateway.Snapshot, secret string) {
	t.Helper()
	if containsString(snap.Skeleton, secret) {
		t.Errorf("骨架里出现了原文 %q", secret)
	}
	if containsString(snap.RedactedPaths, secret) {
		t.Errorf("redacted_paths 里出现了原文 %q", secret)
	}
}

func containsString(v any, needle string) bool {
	switch x := v.(type) {
	case string:
		return x == needle
	case []string:
		for _, s := range x {
			if containsString(s, needle) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if containsString(e, needle) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if containsString(k, needle) || containsString(e, needle) {
				return true
			}
		}
	}
	return false
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

// expectMetricDelta 断言 do 让某个不带标签的计数器恰好涨 1。
//
// 涨的幅度才是判据：observe.Metrics 是进程级的，同一个二进制里别的用例
// （并行跑的也算）会往同一个计数器上加数，所以「取值等于 1」这种断言
// 只要别处也用一次就碎。
func expectMetricDelta(t *testing.T, name string, do func()) {
	t.Helper()
	before := metricValue(t, name)
	do()
	after := metricValue(t, name)
	if after != before+1 {
		t.Errorf("%s 从 %d 变成 %d，期望涨 1", name, before, after)
	}
}

// metricValue 渲染一次 /metrics，取某个不带标签的计数器的取值。
//
// 走的是登记表自己那条渲染路径（Render），与采集端抓到的正文同源——
// 直接读字段的话，测到的就只是那个变量，而没登记、名字写错、格式不对
// 这几种情况它一种都拦不住。
func metricValue(t *testing.T, name string) uint64 {
	t.Helper()
	var buf bytes.Buffer
	if err := observe.Metrics.Render(&buf); err != nil {
		t.Fatalf("渲染指标失败: %v", err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			t.Fatalf("指标 %s 的取值解析不了: %q", name, line)
		}
		return n
	}
	t.Fatalf("指标里没有 %s 这一行:\n%s", name, buf.String())
	return 0
}

func contextWithRequestID(t *testing.T, id string) context.Context {
	t.Helper()
	return observe.WithRequestID(context.Background(), observe.RequestID(id))
}

// 网关给的请求标识为空时，从 context 里补上。
//
// 网关那一侧取标识用的 context 键与 observe 写进去的不是同一个，于是它
// 交过来的永远是空串。空 request_id 意味着「这条快照属于哪次请求」无从回答，
// 而那正是快照唯一的用途——所以缺口在这里补，而不是等落库之后才发现。
func TestSaveFillsRequestIDFromContext(t *testing.T) {
	db := &fakeDB{}
	c := newCollector(t, testConfig(), db)

	snap := newSnap()
	snap.RequestID = ""
	if err := c.Save(observe.WithRequestID(context.Background(), "req-from-ctx"), snap); err != nil {
		t.Fatalf("Save 出错: %v", err)
	}
	if got := db.lastArgs()[0]; got != "req-from-ctx" {
		t.Errorf("落库的 request_id = %v，期望从 context 补上的 req-from-ctx", got)
	}
}

// 网关给的标识非空时不被覆盖：两边都填了的话，以网关那个为准。
func TestSaveKeepsRequestIDWhenAlreadySet(t *testing.T) {
	db := &fakeDB{}
	c := newCollector(t, testConfig(), db)

	snap := newSnap()
	snap.RequestID = "req-from-gateway"
	if err := c.Save(observe.WithRequestID(context.Background(), "req-from-ctx"), snap); err != nil {
		t.Fatalf("Save 出错: %v", err)
	}
	if got := db.lastArgs()[0]; got != "req-from-gateway" {
		t.Errorf("落库的 request_id = %v，期望保留网关给的 req-from-gateway", got)
	}
}

// 两处都取不到时落一个空串，而不是编一个出来。
//
// 空值在库里是可查的（`request_id = ”`），而编出来的标识会让它看起来
// 像一次真实请求——排查时最怕的就是这种「看起来正常」的假数据。
func TestSaveLeavesRequestIDEmptyWhenAbsent(t *testing.T) {
	db := &fakeDB{}
	c := newCollector(t, testConfig(), db)

	snap := newSnap()
	snap.RequestID = ""
	if err := c.Save(context.Background(), snap); err != nil {
		t.Fatalf("Save 出错: %v", err)
	}
	if got := db.lastArgs()[0]; got != "" {
		t.Errorf("落库的 request_id = %v，期望空串", got)
	}
}
