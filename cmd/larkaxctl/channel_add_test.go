package main

// 本文件是 `channel add` 的用例：位置参数、空白名单、创建即探测。
//
// 探测那几条打的是**假上游**（见 addProbeUpstream）与一张只含被测算法的
// 注册表（见 withAddProbeRegistry）：真注册表里 openai 与 openai-responses
// 探测时打的是同一个路径、同一个头，假上游分不开它们，而用例要钉的正是
// 「哪几套协议探测成功了」。

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/anthropicadapter"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/channel/openaiadapter"
	"github.com/sumwai/larkax/internal/channel/responsesadapter"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/upstream"
)

// 探测用例里的规范模型名。写字面量而不是从别处拼：期望值跟着来源变就钉不住。
const addTestModel = "gpt-4o-mini"

// TestChannelAddRejectsBadShapes 钉住参数形状（不连库、不打上游）。
//
// 这些用例走 runChannelAdd 的入口：参数错要在连库之前、打上游之前报出来，
// 而「它没有先连库」由没有 LARKAX_DB_DSN 时拿到的仍是参数错这一点证明。
func TestChannelAddRejectsBadShapes(t *testing.T) {
	withoutDatabase(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有位置参数也没有 --name", nil, "缺少 --name"},
		{"三个位置参数", []string{"c1", testBaseURL, "extra"}, "不接受多余的参数"},
		{"位置参数与 --name 同时给", []string{
			"c1", testBaseURL, "--name", "c2", "--type", testChanType,
		}, "渠道名给了两遍"},
		{"位置参数与 --base-url 同时给", []string{
			"c1", testBaseURL, "--base-url", testBaseURL, "--type", testChanType,
		}, "上游地址给了两遍"},
		{"没给凭证也没给协议", []string{"c1", testBaseURL}, "缺少 --type 或 --protocols"},
		{"--models 给空串", []string{
			"c1", testBaseURL, "--type", testChanType, "--models", "",
		}, "--models 是空的"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runChannelAdd(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestChannelAddWithoutModelsParsesThrough 钉住「--models 可以不给」：
// 解析这一层不再要求模型名，命令会接着去连库。
//
// 走解析函数而不是整条命令：整条命令在没 DSN 的机器上停在「配置无效」，
// 那句话对「--models 还要不要」什么也说明不了。
func TestChannelAddWithoutModelsParsesThrough(t *testing.T) {
	withoutDatabase(t)

	req, _, err := parseChannelCreate([]string{
		"c1", testBaseURL, "--type", testChanType,
	})
	if err != nil {
		t.Fatalf("不给 --models 应当解析得过去：%v", err)
	}
	if len(req.Models) != 0 {
		t.Errorf("Models = %v，期望空", req.Models)
	}

	// 整条命令的入口也走一遍：它该停在连库那一步，而不是参数错。
	runErr := runChannelAdd([]string{"c1", testBaseURL, "--type", testChanType})
	if runErr == nil {
		t.Fatal("没有数据库时不该真的建出渠道")
	}
	if got := exitCode(runErr); got == exitUsage {
		t.Errorf("不给 --models 被当成了参数错（退出码 %d）：%v", got, runErr)
	}
}

// TestChannelCreateAliasStillWorks 钉住 `channel create` 是 add 的别名：
// 两个写法走同一条实现，报错也一样。
func TestChannelCreateAliasStillWorks(t *testing.T) {
	withoutDatabase(t)

	err := runCaptured(t, func() error { return runChannel([]string{"create"}) })
	if err == nil || !strings.Contains(err.Error(), "缺少 --name") {
		t.Fatalf("channel create 没有走到 add 的实现：%v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望参数错的 %d", got, exitUsage)
	}
}

// TestChannelAddPositionalsRoundTrip 钉住位置参数真的落成了渠道名与地址。
func TestChannelAddPositionalsRoundTrip(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)

	name := uniqueName("channel")
	// 显式给 --type：跳过探测，这条用例只验位置参数。
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			name, testBaseURL, "--type", testChanType,
			"--models", addTestModel, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	info := findChannel(t, db, id)
	if info.Name != name {
		t.Errorf("渠道名 = %q，期望位置参数给的 %q", info.Name, name)
	}
	if info.BaseURL != testBaseURL {
		t.Errorf("上游地址 = %q，期望位置参数给的 %q", info.BaseURL, testBaseURL)
	}
	if len(info.Models) != 1 || info.Models[0] != addTestModel {
		t.Errorf("模型白名单 = %v，期望 [%q]", info.Models, addTestModel)
	}
}

// TestChannelAddWithoutModelsStaysUnselectable 钉住空白名单的两端：
// list 看得见它，选路选不中它。
func TestChannelAddWithoutModelsStaysUnselectable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)

	name := uniqueName("channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			name, testBaseURL, "--type", testChanType, "--credential-stdin",
		}); err != nil {
			t.Fatalf("不给 --models 应当建得出来: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	info := findChannel(t, db, id)
	if len(info.Models) != 0 {
		t.Errorf("模型白名单 = %v，期望空", info.Models)
	}

	listOut, _ := captureStreams(t, func() {
		if listErr := runChannelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	if !strings.Contains(listOut, name) {
		t.Errorf("list 里没有这条渠道：\n%s", listOut)
	}

	got, err := channels.New(db, testSealer(t)).Candidates(context.Background(), addTestModel)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	for _, candidate := range got {
		if candidate.Channel.ID == id {
			t.Fatalf("空白名单的渠道被选成了候选：%d", id)
		}
	}
}

// ── 创建即探测 ──────────────────────────────────────────────────────────

// addProbeUpstream 是探测打的假上游。
//
// 它按请求头分协议：带 x-api-key 的是 anthropic，只有 Authorization 的是
// openai——两个适配器探测时打的是同一个 /v1/models，头是唯一分得开的地方
// （见两个适配器的 setHeaders）。
type addProbeUpstream struct {
	*httptest.Server

	mu     sync.Mutex
	calls  map[string]int
	reject map[string]bool
	hang   map[string]bool
}

// newAddProbeUpstream 起一个假上游，reject 里的协议回 401，其余回一份清单。
func newAddProbeUpstream(t *testing.T, reject ...string) *addProbeUpstream {
	t.Helper()
	up := &addProbeUpstream{
		calls:  map[string]int{},
		reject: map[string]bool{},
		hang:   map[string]bool{},
	}
	for _, name := range reject {
		up.reject[name] = true
	}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocolName := "openai"
		if r.Header.Get("x-api-key") != "" {
			protocolName = "anthropic"
		}
		up.mu.Lock()
		up.calls[protocolName]++
		rejected, hanging := up.reject[protocolName], up.hang[protocolName]
		up.mu.Unlock()

		if hanging {
			// 挂住不答，直到客户端撤走：模拟「上游一直没有回应」。挂在请求的
			// ctx 上而不是 sleep，预算耗尽时这一格会立刻结束，不留后台协程。
			<-r.Context().Done()
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if rejected {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"` + addTestModel + `"}]}`))
	}))
	t.Cleanup(up.Close)
	return up
}

// requests 返回假上游一共收到多少次请求。
func (u *addProbeUpstream) requests() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	for _, n := range u.calls {
		total += n
	}
	return total
}

// callsFor 返回某个协议分类下收到的请求数。
//
// openai 与 openai-responses 探测时打同一个路径、同一组头，假上游把它们都
// 记在「openai」名下——这正是「有没有再发一次带 Bearer 的请求」要看的数。
func (u *addProbeUpstream) callsFor(protocol string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[protocol]
}

// hangProtocol 让某一类请求挂住不答，直到客户端撤走。
func (u *addProbeUpstream) hangProtocol(protocol string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hang[protocol] = true
}

// withAddProbeRegistry 把探测用的注册表换成只含给定类型的替身。
//
// 用真适配器（不是纯替身）：探测走的是真 HTTP 与真的头，假上游能按头分协议；
// 换掉的只是**类型清单**，而清单正是这条链要读的东西（见 probeProtocolsForAdd）。
func withAddProbeRegistry(t *testing.T, types ...string) {
	t.Helper()
	client := httpx.ProbeClient(upstream.ProbeTimeout)
	registry := channel.NewRegistry()
	for _, typ := range types {
		var adapter channel.Adapter
		switch typ {
		case "openai":
			adapter = openaiadapter.NewWithClient(client)
		case "anthropic":
			adapter = anthropicadapter.NewWithClient(client)
		case "openai-responses":
			adapter = responsesadapter.NewWithClient(client)
		default:
			t.Fatalf("这个用例没有为类型 %q 准备适配器", typ)
		}
		if err := registry.Register(adapter); err != nil {
			t.Fatalf("注册适配器 %q 失败: %v", typ, err)
		}
	}
	old := probeChannelRegistry
	probeChannelRegistry = func() (*channel.Registry, error) { return registry, nil }
	t.Cleanup(func() { probeChannelRegistry = old })
}

// withProbeTimeout 把创建时那轮探测摊到每套协议上的预算压小。
//
// 生产路径永远是 upstream.ProbeTimeout；只有这里会让它变成毫秒级，好让
// 「预算耗尽」这一档真的发生（见 probeBudget）。
func withProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := channelProbeTimeoutPerType
	channelProbeTimeoutPerType = d
	t.Cleanup(func() { channelProbeTimeoutPerType = old })
}

// channelRowCount 数 channels 表里的行数（含软删）。
//
// 「没建出来」只能这样判：一条建出来的渠道在别处都有痕迹，而「零行」与
// 「建了一条但没查」，只有行数分得开。
func channelRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM channels`).Scan(&n); err != nil {
		t.Fatalf("数渠道行数失败: %v", err)
	}
	return n
}

// TestChannelAddBindsOnlyProbedProtocol 钉住「只绑探测成功的那一套协议」。
func TestChannelAddBindsOnlyProbedProtocol(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)
	up := newAddProbeUpstream(t, "anthropic")
	withAddProbeRegistry(t, "openai", "anthropic")

	name := uniqueName("channel")
	stdout, stderr := captureStreams(t, func() {
		if err := runChannelAdd([]string{name, up.URL, "--credential-stdin"}); err != nil {
			t.Fatalf("channel add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	info := findChannel(t, db, id)
	if len(info.Protocols) != 1 || string(info.Protocols[0]) != "openai" {
		t.Errorf("协议集合 = %v，期望只有 openai", info.Protocols)
	}
	if info.Type != "openai" {
		t.Errorf("主协议 = %q，期望 openai", info.Type)
	}
	if !strings.Contains(stderr, "未绑定协议 anthropic") {
		t.Errorf("stderr 没有点名没探测成功的 anthropic：%q", stderr)
	}
	// 探测成功的协议不该被当成「没探测成功」打出来。
	if strings.Contains(stderr, "未绑定协议 openai") {
		t.Errorf("stderr 把探测成功的 openai 也列成了未绑定：%q", stderr)
	}
}

// TestChannelAddBindsAllProbedProtocols 钉住「两个都通时两个都绑上，
// 主协议是候选顺序里的第一个」。
//
// 候选顺序就是注册表 Types() 的顺序（按名字升序），所以 anthropic 在前：
// 期望值写字面量，不从 Types() 自己拼。
func TestChannelAddBindsAllProbedProtocols(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)
	up := newAddProbeUpstream(t) // 两个协议都通
	withAddProbeRegistry(t, "openai", "anthropic")

	name := uniqueName("channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{name, up.URL, "--credential-stdin"}); err != nil {
			t.Fatalf("channel add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	info := findChannel(t, db, id)
	got := make([]string, 0, len(info.Protocols))
	for _, p := range info.Protocols {
		got = append(got, string(p))
	}
	if strings.Join(got, ",") != "anthropic,openai" {
		t.Errorf("协议集合 = %v，期望 [anthropic openai]", got)
	}
	if info.Type != "anthropic" {
		t.Errorf("主协议 = %q，期望候选顺序的第一个 anthropic", info.Type)
	}
}

// TestChannelAddAllProbesFailCreatesNothing 钉住「一个都没探测成功就不建」：
// 行数不变，错误里逐个点名协议与原因。
func TestChannelAddAllProbesFailCreatesNothing(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)
	up := newAddProbeUpstream(t, "openai", "anthropic")
	withAddProbeRegistry(t, "openai", "anthropic")

	before := channelRowCount(t, db)
	name := uniqueName("channel")
	err := runChannelAdd([]string{name, up.URL, "--credential-stdin"})
	if err == nil {
		t.Fatal("一套协议都没探测成功，却建出了渠道")
	}
	for _, want := range []string{"openai", "anthropic", "401", "没有一套协议探测成功", "--type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里没有 %q：%v", want, err)
		}
	}
	if got := channelRowCount(t, db); got != before {
		t.Errorf("渠道行数从 %d 变成了 %d：没探测成功时不该留下任何行", before, got)
	}
	if got := exitCode(err); got == exitUsage {
		t.Errorf("「探不通」是运行期失败，不该按参数错报（退出码 %d）", got)
	}
}

// TestChannelAddExplicitTypeSkipsProbing 钉住逃生口：假上游全不通时，
// 显式给 --type 照样建得出来，而且一个探测请求都不发。
func TestChannelAddExplicitTypeSkipsProbing(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)
	up := newAddProbeUpstream(t, "openai", "anthropic")
	withAddProbeRegistry(t, "openai", "anthropic")

	name := uniqueName("channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			name, up.URL, "--type", "openai", "--credential-stdin",
		}); err != nil {
			t.Fatalf("显式给 --type 应当跳过探测并建成功: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}
	if got := up.requests(); got != 0 {
		t.Errorf("显式给了 --type 还发了 %d 次探测请求，期望 0", got)
	}
	info := findChannel(t, db, id)
	if info.Type != "openai" {
		t.Errorf("主协议 = %q，期望 --type 给的 openai", info.Type)
	}
}

// TestChannelAddWithoutCredentialSkipsProbing 钉住「没给凭证就不探」：
// 零账号渠道按 --type 建，上游一次都不打。
func TestChannelAddWithoutCredentialSkipsProbing(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	up := newAddProbeUpstream(t) // 给了凭证的话它两个协议都能探测成功
	withAddProbeRegistry(t, "openai", "anthropic")

	name := uniqueName("channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{name, up.URL, "--type", "openai"}); err != nil {
			t.Fatalf("零账号渠道应当建得出来: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}
	if got := up.requests(); got != 0 {
		t.Errorf("没给凭证还发了 %d 次探测请求，期望 0", got)
	}
	info := findChannel(t, db, id)
	if len(info.Accounts) != 0 {
		t.Errorf("账号数 = %d，期望零账号", len(info.Accounts))
	}
}

// TestChannelAddBudgetExhaustionIsNotAFailure 钉住「探了、没通」与「因预算
// 根本没探」在输出里分得开，而且部分探测成功不阻断创建。
//
// 场景按候选顺序（注册表 Types() 升序：anthropic、openai、openai-responses）：
//
//   - anthropic 立刻通——它是唯一探通的那套，也是落库的协议；
//   - openai 挂住不答，把探测预算耗光；
//   - openai-responses 这时还没轮到，一个请求都不该发出去。
//
// 三档输出各归各的：anthropic 进「已绑定」，openai 进「未绑定」（它被探过，
// 只是超时），openai-responses 进「未探测」——把它说成「未绑定」会把人引去
// 查一个我们从没问过的上游。
func TestChannelAddBudgetExhaustionIsNotAFailure(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)
	up := newAddProbeUpstream(t)
	up.hangProtocol("openai")
	withAddProbeRegistry(t, "anthropic", "openai", "openai-responses")
	withProbeTimeout(t, 200*time.Millisecond)

	name := uniqueName("channel")
	stdout, stderr := captureStreams(t, func() {
		if err := runChannelAdd([]string{name, up.URL, "--credential-stdin"}); err != nil {
			t.Fatalf("部分协议探测成功时应当建得出来: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	info := findChannel(t, db, id)
	if len(info.Protocols) != 1 || string(info.Protocols[0]) != "anthropic" {
		t.Errorf("协议集合 = %v，期望只有探测成功的 anthropic", info.Protocols)
	}
	if info.Type != "anthropic" {
		t.Errorf("主协议 = %q，期望 anthropic", info.Type)
	}

	// 被预算切断的那一套仍归「未绑定」，但原因要说是我们的预算到了，不是
	// 上游的错——原样带出 context deadline exceeded 会被读成上游的问题。
	if !strings.Contains(stderr, "未绑定协议 openai：探测超时：预算内没有回应") {
		t.Errorf("挂住超时的 openai 该记成「未绑定」并说明是预算内的超时：%q", stderr)
	}
	if !strings.Contains(stderr, "未探测协议 openai-responses：探测预算耗尽") {
		t.Errorf("预算耗尽后没轮到的 openai-responses 该记成「未探测」：%q", stderr)
	}
	// 反向：没探的那套不能被说成「未绑定」。
	if strings.Contains(stderr, "未绑定协议 openai-responses") {
		t.Errorf("没发请求的 openai-responses 被说成了「未绑定」：%q", stderr)
	}

	// 预算耗尽之后不该再发请求：带 Bearer 的请求只该有 openai 那一次
	// （openai-responses 打的是同一组头，假上游记在同一个分类下）。
	if got := up.callsFor("openai"); got != 1 {
		t.Errorf("带 Bearer 的探测请求发了 %d 次，期望只有 openai 那一次", got)
	}
}

// TestChannelAddAllZeroWithBudgetExhaustedSaysSo 钉住「全部为 0」那条拒绝错误
// 也分两档：预算先耗尽时，首句不能是「没有一套协议探测成功」——那会让人以为
// 每套都问过了。
func TestChannelAddAllZeroWithBudgetExhaustedSaysSo(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)
	up := newAddProbeUpstream(t)
	up.hangProtocol("anthropic") // 候选顺序的第一套就挂住，后面的根本没轮到
	withAddProbeRegistry(t, "anthropic", "openai")
	withProbeTimeout(t, 150*time.Millisecond)

	before := channelRowCount(t, db)
	err := runChannelAdd([]string{uniqueName("channel"), up.URL, "--credential-stdin"})
	if err == nil {
		t.Fatal("一套协议都没探测成功，却建出了渠道")
	}
	for _, want := range []string{
		"预算内没有探通任何协议，另有 1 套未探测", "anthropic", "openai", "--protocols",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里没有 %q：%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "没有一套协议探测成功") {
		t.Errorf("首句仍说成「没有一套协议探测成功」，没把预算耗尽分开：%v", err)
	}
	if got := channelRowCount(t, db); got != before {
		t.Errorf("渠道行数从 %d 变成了 %d：没探测成功时不该留下行", before, got)
	}
}
