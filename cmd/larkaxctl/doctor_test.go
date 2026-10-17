package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/api"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
)

// testDoctorKey 是打网关用的假 API Key。
//
// 一眼能看出是编的，且不构成任何真实凭证的形态——真实的 key 不进仓库，
// 也不进测试：这里要证明的是「它被放进了请求头、且没有被打印出来」，
// 不是「这把 key 有多强」。
const testDoctorKey = "test-key-placeholder"

// ── 第 1 项：数据库 ─────────────────────────────────────────────────────

// TestDoctorDatabaseCheckDoesNotCreateTheVersionTable 钉住第 1 项的只读承诺。
//
// goose 的 GetDBVersionContext 在版本表不存在时会先 EnsureDBVersion 把表
// **建出来**——也就是说「读一下版本」在一个全新库上是一次写入。doctor 对外
// 承诺只读，所以它自己先探一句表在不在。
//
// 这条用例在一个空 schema 上跑一遍，再确认那张表仍然不在。把探测换回 goose
// 的现成函数，这里就会红。
func TestDoctorDatabaseCheckDoesNotCreateTheVersionTable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// schema 名不能带短横线：未加引号的标识符里它是减号。
	schema := strings.ReplaceAll(uniqueName("doctor_readonly"), "-", "_")
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	c := checkDoctorDatabase(ctx, openInSchema(t, schema))
	notes := strings.Join(c.notes, "\n")
	if !strings.Contains(notes, "一条迁移都没跑过") {
		t.Errorf("空 schema 该按「一条迁移都没跑过」报，实际 notes 是 %q", c.notes)
	}
	if !strings.Contains(notes, "最新的迁移版本是 21") {
		t.Errorf("它仍然该报出二进制里的最新版本，实际 notes 是 %q", c.notes)
	}

	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`,
		schema+"."+goose.TableName()).Scan(&exists); err != nil {
		t.Fatalf("查版本表失败: %v", err)
	}
	if exists {
		t.Errorf("doctor 把 %s.%s 建出来了：它承诺只读", schema, goose.TableName())
	}
}

// openInSchema 打开一条 search_path 指向指定 schema 的连接。
//
// 走 DSN 参数而不是 `SET search_path`：SET 只作用于当前那一条连接，而
// database/sql 是连接池，下一条语句完全可能落到另一条连接上。
func openInSchema(t *testing.T, schema string) *sql.DB {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	handle, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	if err := handle.PingContext(context.Background()); err != nil {
		t.Fatalf("连不上数据库: %v", err)
	}
	return handle
}

// ── 第 2 项：渠道 ───────────────────────────────────────────────────────

func TestDoctorChannelsFlagsEachCondition(t *testing.T) {
	cases := []struct {
		name     string
		channels []channels.ChannelInfo
		want     []string
	}{
		{
			name:     "一条渠道都没有",
			channels: nil,
			want:     []string{"没有任何 enabled 的渠道"},
		},
		{
			name: "enabled 但 models 为空",
			channels: []channels.ChannelInfo{
				{ID: 3, Name: "c3", Status: channels.StatusEnabled},
			},
			want: []string{"渠道 3（c3）是 enabled，但 models 是空的"},
		},
		{
			name: "全部是 disabled",
			channels: []channels.ChannelInfo{
				{ID: 4, Name: "c4", Status: channels.StatusDisabled, Models: []string{"m"}},
			},
			want: []string{"没有任何 enabled 的渠道"},
		},
		{
			name: "一条可用的渠道",
			channels: []channels.ChannelInfo{
				{ID: 5, Name: "c5", Status: channels.StatusEnabled, Models: []string{"m"}},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkDoctorChannels(doctorChainInput{channels: tc.channels})
			assertContainsFragments(t, got.findings, tc.want...)
			if got.skip != "" {
				t.Errorf("第 2 项不该报「没跑成」：%s", got.skip)
			}
		})
	}
}

// ── 第 3 项：模型目录 ───────────────────────────────────────────────────

// TestDoctorCatalogFlagsChannelModelMismatch 钉住 issue 里的第一个例子：
// 渠道的 --models 与目录里的规范名对不上。
//
// 这一条要能**单独**指出来：说清是哪一个渠道、哪一个名字，而不是一句
// 「配置不对」——调用那边只会回 503「no available upstream」。
func TestDoctorCatalogFlagsChannelModelMismatch(t *testing.T) {
	in := doctorChainInput{
		channels: []channels.ChannelInfo{
			{ID: 7, Name: "my-upstream", Status: channels.StatusEnabled, Models: []string{"claude-xx"}},
			{ID: 9, Name: "other", Status: channels.StatusEnabled, Models: []string{"claude-xx"}},
			{ID: 8, Name: "disabled", Status: channels.StatusDisabled, Models: []string{"claude-xx"}},
		},
		catalogSet: map[string]bool{"claude-x": true},
	}

	got := checkDoctorCatalog(in)
	assertContainsFragments(t, got.findings,
		"渠道 7（my-upstream）", "渠道 9（other）", `"claude-xx"`, "目录里没有这个名字")
	if strings.Contains(strings.Join(got.findings, "\n"), "渠道 8") {
		t.Error("disabled 的渠道不该出现在问题里：它本来就不接请求")
	}
	if got.status() != doctorFailed {
		t.Errorf("对不上名字时结论应当是不通过，得到 %s", got.status().label())
	}
}

// TestDoctorCatalogSkipsWithoutChannels 钉住「没跑成」与「通过」分得开。
//
// 一条 enabled 渠道都没有时，这一项一个名字都查不到。报「通过」是错的：
// 它会让一次什么都没查的运行看起来像一次查过且没问题的运行
// （见 LESSONS.md 第 16 条）。
func TestDoctorCatalogSkipsWithoutChannels(t *testing.T) {
	got := checkDoctorCatalog(doctorChainInput{catalogSet: map[string]bool{}})
	if got.skip == "" {
		t.Fatal("没有可查的对象时第 3 项应当报「没跑成」，而不是什么都不说")
	}
	if got.status() != doctorSkipped {
		t.Errorf("结论应当是没跑成，得到 %s", got.status().label())
	}
}

// ── 第 4 项：别名 ───────────────────────────────────────────────────────

// TestDoctorAliasFlagsConfiguredButUnreachable 钉住 issue 里的第二个例子：
// 别名配了，但渠道白名单里没有这个模型。
//
// 两张表各看一半都对，合起来不对——所以这一条必须点名是哪个规范名、
// 别名配在哪个范围上。
func TestDoctorAliasFlagsConfiguredButUnreachable(t *testing.T) {
	aliases := []catalog.Alias{
		{ID: 1, ChannelID: 0, CanonicalName: "claude-z", UpstreamModel: "u-platform"},
		{ID: 2, ChannelID: 3, CanonicalName: "claude-z", UpstreamModel: "u-channel"},
		{ID: 3, ChannelID: 0, CanonicalName: "claude-x", UpstreamModel: "u-x"},
	}

	got := doctorAliasReachability(aliases, map[string]bool{"claude-x": true})

	if len(got) != 1 {
		t.Fatalf("只有 claude-z 该被报出来，得到 %d 条：%q", len(got), got)
	}
	assertContainsFragments(t, got, "「claude-z」", "平台级 1 条", "渠道级 1 条（渠道 3）",
		"没有任何 enabled 渠道的 models 里包含它")
}

// TestDoctorAliasReachabilityPassesWhenEveryAliasIsServable 是上一条的对照：
// 有渠道接得住的别名一条都不该被报。
func TestDoctorAliasReachabilityPassesWhenEveryAliasIsServable(t *testing.T) {
	aliases := []catalog.Alias{{ID: 1, CanonicalName: "claude-x", UpstreamModel: "u-x"}}
	if got := doctorAliasReachability(aliases, map[string]bool{"claude-x": true}); len(got) != 0 {
		t.Errorf("有渠道接得住时不该报问题，得到 %q", got)
	}
}

// TestDoctorAliasAndPricingSkipWithoutServableModels 钉住第 4、5 项的空集合分支。
func TestDoctorAliasAndPricingSkipWithoutServableModels(t *testing.T) {
	alias, price := checkDoctorAliasAndPricing(context.Background(), nil, doctorChainInput{
		channels: []channels.ChannelInfo{
			{ID: 1, Name: "c", Status: channels.StatusEnabled, Models: []string{"not-in-catalog"}},
		},
		catalogSet: map[string]bool{"claude-x": true},
	})

	for _, c := range []doctorCheck{alias, price} {
		if c.skip == "" {
			t.Errorf("第 %s 项没有可查的对象时应当报「没跑成」", c.title)
		}
		if c.status() != doctorSkipped {
			t.Errorf("第 %s 项的结论应当是没跑成，得到 %s", c.title, c.status().label())
		}
	}
}

// ── 第 6 项：钱包 ───────────────────────────────────────────────────────

func TestDoctorWalletsFlagsEachCondition(t *testing.T) {
	cases := []struct {
		name  string
		users []doctorUser
		want  []string
	}{
		{
			name:  "一个用户都没有",
			users: nil,
			want:  []string{"还没有用户"},
		},
		{
			name:  "用户没有钱包行",
			users: []doctorUser{{ID: 1, Name: "alice"}},
			want:  []string{"用户 1（alice）没有钱包行"},
		},
		{
			name:  "有钱包行但余额都是 0",
			users: []doctorUser{{ID: 2, Name: "bob", HasWallet: true}},
			want:  []string{"1 个钱包行的余额都是 0"},
		},
		{
			name: "一个用户没有钱包、另一个有余额",
			users: []doctorUser{
				{ID: 3, Name: "carol", HasWallet: true, Funded: true},
				{ID: 4, Name: "dave"},
			},
			want: []string{"用户 4（dave）没有钱包行"},
		},
		{
			name:  "都正常",
			users: []doctorUser{{ID: 5, Name: "erin", HasWallet: true, Funded: true}},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkDoctorWallets(doctorChainInput{users: tc.users})
			assertContainsFragments(t, got.findings, tc.want...)
			if tc.name == "有钱包行但余额都是 0" {
				if strings.Contains(strings.Join(got.findings, "\n"), "没有钱包行") {
					t.Error("有钱包行时不该报「没有钱包行」")
				}
			}
		})
	}
}

// ── 结论与退出码 ────────────────────────────────────────────────────────

// TestDoctorSkippedIsNotPassed 钉住这一条命令最要紧的判据：
// 「没跑成」与「通过」在输出与退出码上都必须分得开。
func TestDoctorSkippedIsNotPassed(t *testing.T) {
	rep := doctorReport{
		database: doctorCheck{title: "数据库"},
		channel:  doctorCheck{title: "渠道"},
		catalog:  doctorCheck{title: "模型目录", skip: "没有可查的对象"},
		alias:    doctorCheck{title: "别名"},
		pricing:  doctorCheck{title: "计价规则"},
		wallet:   doctorCheck{title: "钱包"},
	}

	passed, failed, skipped := rep.counts()
	if passed != 5 || failed != 0 || skipped != 1 {
		t.Fatalf("计数 = 通过 %d / 不通过 %d / 没跑成 %d，期望 5 / 0 / 1", passed, failed, skipped)
	}
	if rep.exitErr(nil) == nil {
		t.Fatal("有一项「没跑成」时退出码必须非 0")
	}

	var buf bytes.Buffer
	printDoctorReport(&buf, rep)
	out := buf.String()
	if !strings.Contains(out, "没跑成：没有可查的对象") {
		t.Errorf("「没跑成」的原因没有打出来：\n%s", out)
	}
	if !strings.Contains(out, "结论：没跑成") {
		t.Errorf("「没跑成」这一项的结论行没有打出来：\n%s", out)
	}
	if !strings.Contains(out, "汇总：通过 5 项、不通过 0 项、没跑成 1 项") {
		t.Errorf("汇总行不对：\n%s", out)
	}
}

// TestDoctorPassesWhenEveryCheckPasses 是上一条的对照：全通过时退出码是 0。
func TestDoctorPassesWhenEveryCheckPasses(t *testing.T) {
	rep := doctorReport{
		database: doctorCheck{title: "数据库"},
		channel:  doctorCheck{title: "渠道"},
		catalog:  doctorCheck{title: "模型目录"},
		alias:    doctorCheck{title: "别名"},
		pricing:  doctorCheck{title: "计价规则"},
		wallet:   doctorCheck{title: "钱包"},
	}
	if err := rep.exitErr(nil); err != nil {
		t.Fatalf("六项全通过时不该报错，得到 %v", err)
	}
}

// TestDoctorKeepsDBUnreachExitCode 钉住连不上库时的退出码。
//
// 它是既有的对外契约（见 exit.go）：脚本按 3 分派「去看环境」，doctor 不该
// 把它变成「其他失败」。
func TestDoctorKeepsDBUnreachExitCode(t *testing.T) {
	rep := doctorReportWithoutDB(context.Background(), dbErr(errors.New("连不上")), doctorOptions{})
	if got := exitCode(rep.exitErr(dbErr(errors.New("连不上")))); got != exitDBUnreach {
		t.Fatalf("连不上库时退出码应当是 %d，得到 %d", exitDBUnreach, got)
	}

	var buf bytes.Buffer
	printDoctorReport(&buf, rep)
	out := buf.String()
	if !strings.Contains(out, "汇总：通过 0 项、不通过 0 项、没跑成 6 项") {
		t.Errorf("连不上库时六项都该报「没跑成」：\n%s", out)
	}
	// 那一句错误原文只出现一次：六项各抄一遍会把报告撑成四十行，
	// 其中六行一模一样，而「哪一项跟别的不一样」正是读者要看的。
	if n := strings.Count(out, "连不上数据库：连不上"); n != 1 {
		t.Errorf("错误原文该只出现在第 1 项里，实际出现 %d 次：\n%s", n, out)
	}
	if !strings.Contains(out, "连不上数据库（同第 1 项）") {
		t.Errorf("后面五项该指向第 1 项：\n%s", out)
	}
}

// ── 第 7 项：真打一次上游 ───────────────────────────────────────────────

// TestDoctorEndpointPathsMatchAPI 钉住路径与 internal/api 是同一份值。
//
// 两处各写一个字符串时，改了服务端路由而没改这里，表现是 doctor 报「404」
// 而人以为链路坏了——这一条防的就是那个缝。
func TestDoctorEndpointPathsMatchAPI(t *testing.T) {
	if doctorPathChat != api.PathChatCompletions {
		t.Errorf("chat 入口路径 = %q，internal/api 里是 %q", doctorPathChat, api.PathChatCompletions)
	}
	if doctorPathMessages != api.PathMessages {
		t.Errorf("messages 入口路径 = %q，internal/api 里是 %q", doctorPathMessages, api.PathMessages)
	}
}

// TestDoctorLiveSendsRequestAndPrintsResult 钉住「它真的发了请求、并把结果
// 打出来了」。
//
// 假上游收下请求并断言形状，doctor 那一侧断言打出来的那几行——两边都看，
// 才能说明这次调用真的走完了，而不是 doctor 自己编了一段输出。
func TestDoctorLiveSendsRequestAndPrintsResult(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","model":"claude-x",`+
			`"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	}))
	defer srv.Close()

	opts := doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	}
	got := runDoctorLive(context.Background(), opts)

	if gotMethod != http.MethodPost {
		t.Errorf("方法 = %q，期望 POST", gotMethod)
	}
	if gotPath != doctorPathChat {
		t.Errorf("路径 = %q，期望 %q", gotPath, doctorPathChat)
	}
	if gotAuth != "Bearer "+testDoctorKey {
		t.Errorf("chat 入口的凭证该走 Authorization: Bearer，得到 %q", gotAuth)
	}
	if gotBody["model"] != "claude-x" {
		t.Errorf("请求体里的模型 = %v，期望 claude-x", gotBody["model"])
	}
	if !got.ok() || got.status != http.StatusOK {
		t.Fatalf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
	}
	if got.usage != "prompt=9 completion=2" {
		t.Errorf("用量 = %q，期望 prompt=9 completion=2", got.usage)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got)
	out := buf.String()
	for _, want := range []string{
		"POST " + srv.URL + doctorPathChat, "模型：claude-x",
		"200", "prompt=9 completion=2", "结论：通过",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, testDoctorKey) {
		t.Errorf("凭证出现在了输出里：\n%s", out)
	}
}

// TestDoctorLiveMessagesUsesAnthropicHeaders 钉住 messages 入口走的是
// Anthropic 的习惯：x-api-key，且请求体里带 max_tokens。
func TestDoctorLiveMessagesUsesAnthropicHeaders(t *testing.T) {
	var (
		gotPath string
		gotKey  string
		gotAuth string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","model":"claude-x",`+
			`"content":[],"usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointMessages,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if gotPath != doctorPathMessages {
		t.Errorf("路径 = %q，期望 %q", gotPath, doctorPathMessages)
	}
	if gotKey != testDoctorKey {
		t.Errorf("messages 入口的凭证该走 x-api-key，得到 %q", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("messages 入口不该同时带 Authorization，得到 %q", gotAuth)
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Error("messages 入口的请求体必须带 max_tokens（Anthropic 协议必填）")
	}
	if got.usage != "input=7 output=3" {
		t.Errorf("用量 = %q，期望 input=7 output=3", got.usage)
	}
	if !got.ok() {
		t.Errorf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
	}
}

// TestDoctorLiveReportsUpstreamRejection 钉住非 2xx 时把错误说明打出来。
func TestDoctorLiveReportsUpstreamRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"no available upstream for the requested model","type":"server_error"}}`)
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if got.ok() {
		t.Fatal("503 不该算通过")
	}
	if got.status != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", got.status)
	}
	if got.errMsg != "no available upstream for the requested model" {
		t.Errorf("错误说明 = %q", got.errMsg)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got)
	out := buf.String()
	if !strings.Contains(out, "503") || !strings.Contains(out, got.errMsg) {
		t.Errorf("输出里该有状态码与错误说明：\n%s", out)
	}
	if !strings.Contains(out, "结论：不通过") {
		t.Errorf("结论该是不通过：\n%s", out)
	}
}

// TestDoctorLivePreviewsNonJSONBody 钉住「响应体不是 JSON」时的输出。
//
// 反代返回的错误页（502 的 HTML）解析不出任何字段，而那一页正是要给人看的
// 东西——只打一句「502」会让人去猜是哪一层挡住的。
func TestDoctorLivePreviewsNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html><body>\n  <h1>502 Bad Gateway</h1>\n</body></html>")
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if got.preview == "" {
		t.Fatal("响应体不是 JSON 时该原样截一段打出来")
	}
	if strings.Contains(got.preview, "\n") {
		t.Errorf("预览该折成一行，得到 %q", got.preview)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got)
	if !strings.Contains(buf.String(), "502 Bad Gateway") {
		t.Errorf("输出里该有响应体的内容：\n%s", buf.String())
	}
}

// roundTripFunc 让用例把传输层换成假的。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDoctorLiveReportsSendFailure 钉住「请求根本没发出去」这一条分支。
//
// 它与「发出去了、上游回了 503」是两件事，而两者的现场完全不同（一个是
// 地址/网络，一个是链路配置），所以必须分开说。传输层是假的，这条分支才
// 不依赖机器上真的有一个连不上的端口。
func TestDoctorLiveReportsSendFailure(t *testing.T) {
	var gotURL string
	saved := doctorHTTPClient
	doctorHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return nil, errors.New("connection refused")
	})}
	t.Cleanup(func() { doctorHTTPClient = saved })

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: "http://127.0.0.1:1", key: testDoctorKey,
	})

	if got.sendErr == nil {
		t.Fatal("传输层失败时该记下那个错误")
	}
	if got.ok() {
		t.Error("请求没发出去时不该算通过")
	}
	if gotURL != "http://127.0.0.1:1"+doctorPathChat {
		t.Errorf("请求打到了 %q", gotURL)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got)
	out := buf.String()
	if !strings.Contains(out, "请求没发出去") || !strings.Contains(out, "结论：不通过") {
		t.Errorf("输出该说清是没发出去、且结论为不通过：\n%s", out)
	}
	if strings.Contains(out, testDoctorKey) {
		t.Errorf("凭证出现在了输出里：\n%s", out)
	}
}

// TestDoctorLiveDisabledByDefault 钉住默认不跑：没开 --live 时不发请求。
func TestDoctorLiveDisabledByDefault(t *testing.T) {
	saved := doctorHTTPClient
	doctorHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("没开 --live 时不该发任何请求")
		return nil, errors.New("不该走到这里")
	})}
	t.Cleanup(func() { doctorHTTPClient = saved })

	got := runDoctorLive(context.Background(), doctorOptions{model: "claude-x"})
	if got.enabled {
		t.Error("没开 --live 时这一项该是「没跑」")
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got)
	if !strings.Contains(buf.String(), "没跑：要加 --live") {
		t.Errorf("默认那一次该打一行「没跑」：\n%s", buf.String())
	}
}

// TestDoctorLiveDoesNotAffectExitCodeWhenDisabled 钉住没开 --live 时它不进退出码。
func TestDoctorLiveDoesNotAffectExitCodeWhenDisabled(t *testing.T) {
	rep := doctorReport{
		database: doctorCheck{title: "数据库"},
		channel:  doctorCheck{title: "渠道"},
		catalog:  doctorCheck{title: "模型目录"},
		alias:    doctorCheck{title: "别名"},
		pricing:  doctorCheck{title: "计价规则"},
		wallet:   doctorCheck{title: "钱包"},
	}
	if err := rep.exitErr(nil); err != nil {
		t.Fatalf("没开 --live 时它不该影响退出码，得到 %v", err)
	}

	rep.live = doctorLive{enabled: true, status: http.StatusServiceUnavailable}
	if rep.exitErr(nil) == nil {
		t.Fatal("开了 --live 且那一次没成功时，退出码必须非 0")
	}
}

// ── 参数 ────────────────────────────────────────────────────────────────

// TestDoctorRejectsFlagsThatNeedLive 钉住「只有 --live 才用得上」的那几个标志。
//
// 静默忽略它们会让「我给了 --model 但它没生效」看起来像一次成功的检查。
func TestDoctorRejectsFlagsThatNeedLive(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "claude-x"},
		{"--endpoint", doctorEndpointMessages},
		{"--base-url", "http://127.0.0.1:8080"},
		{"--credential-stdin"},
		{"--credential-env", "SOME_VAR"},
	} {
		err := runCaptured(t, func() error { return runDoctor(args) })
		if err == nil {
			t.Errorf("%v 该被拒：它只在 --live 时才有意义", args)
			continue
		}
		if !strings.Contains(err.Error(), "只在 --live 时才有意义") {
			t.Errorf("%v 的错误信息是 %v", args, err)
		}
		if got := exitCode(err); got != exitUsage {
			t.Errorf("%v 是参数错，退出码应当是 %d，得到 %d", args, exitUsage, got)
		}
	}
}

func TestDoctorLiveRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --model", []string{"--live", "--credential-env", "SOME_VAR"}, "需要 --model"},
		{"模型名只有空白", []string{"--live", "--model", "  ", "--credential-env", "SOME_VAR"}, "需要 --model"},
		{"入口不认识", []string{"--live", "--model", "m", "--endpoint", "bogus", "--credential-env", "SOME_VAR"}, "--endpoint 只能是"},
		{"地址没有协议", []string{"--live", "--model", "m", "--base-url", "127.0.0.1:8080", "--credential-env", "SOME_VAR"}, "不是合法的 URL（要形如 http://127.0.0.1:8080）"},
		{"协议不认识", []string{"--live", "--model", "m", "--base-url", "ftp://example.com", "--credential-env", "SOME_VAR"}, "必须以 http:// 或 https:// 开头"},
		{"地址没有主机名", []string{"--live", "--model", "m", "--base-url", "http://", "--credential-env", "SOME_VAR"}, "没有主机名"},
		{"没有凭证", []string{"--live", "--model", "m"}, "缺少凭证"},
		{"多出位置参数", []string{"--live", "--model", "m", "--credential-env", "SOME_VAR", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runDoctor(tc.args) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望报错且含 %q，得到 %v", tc.want, err)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestDoctorLiveCredentialSources 钉住凭证的两个来源，以及「不从参数读」。
//
// 命令行参数会出现在 ps 的输出与 shell 历史里，所以这里**没有**一条从参数
// 取凭证的路径：给一个 --credential <值> 会被当成未知标志拒掉。
func TestDoctorLiveCredentialSources(t *testing.T) {
	withStdin(t, testDoctorKey+"\n")
	opts, err := parseDoctorOptions([]string{"--live", "--model", "claude-x", "--credential-stdin"})
	if err != nil {
		t.Fatalf("从标准输入读凭证失败: %v", err)
	}
	if opts.key != testDoctorKey {
		t.Errorf("从标准输入读到的凭证 = %q，末尾的换行该被去掉", opts.key)
	}

	t.Setenv("LARKAX_TEST_DOCTOR_KEY", testDoctorKey)
	opts, err = parseDoctorOptions([]string{"--live", "--model", "claude-x", "--credential-env", "LARKAX_TEST_DOCTOR_KEY"})
	if err != nil {
		t.Fatalf("从环境变量读凭证失败: %v", err)
	}
	if opts.key != testDoctorKey {
		t.Errorf("从环境变量读到的凭证 = %q", opts.key)
	}

	err = runCaptured(t, func() error {
		return runDoctor([]string{"--live", "--model", "claude-x", "--credential", testDoctorKey})
	})
	if err == nil {
		t.Fatal("--credential 不该被接受：凭证只能从标准输入或环境变量读")
	}
	if strings.Contains(err.Error(), testDoctorKey) {
		t.Errorf("错误信息里出现了凭证本身：%v", err)
	}
}

// TestDoctorLiveTrimsTrailingSlashFromBaseURL 钉住地址末尾的斜杠不会拼出双斜杠。
func TestDoctorLiveTrimsTrailingSlashFromBaseURL(t *testing.T) {
	withStdin(t, testDoctorKey)
	opts, err := parseDoctorOptions([]string{
		"--live", "--model", "claude-x", "--base-url", "http://127.0.0.1:8080/",
		"--credential-stdin",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := opts.baseURL + doctorPathChat; got != "http://127.0.0.1:8080"+doctorPathChat {
		t.Errorf("拼出来的地址 = %q", got)
	}
}

// ── 端到端（需要数据库） ────────────────────────────────────────────────

// runDoctorCaptured 跑一遍完整的 doctor，返回它打到 stdout 的内容与错误。
//
// doctor 的结论同时在**输出**与**退出码**上，而两者都要断言——只看退出码
// 会漏掉「报对了但没有说清是哪一项」。
func runDoctorCaptured(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out, _ := captureStreams(t, func() { err = runDoctor(args) })
	return out, err
}

// doctorSection 取出报告里某一节的内容（从标题行到下一节）。
func doctorSection(t *testing.T, out, title string) string {
	t.Helper()
	idx := strings.Index(out, title)
	if idx < 0 {
		t.Fatalf("报告里没有 %q 这一节：\n%s", title, out)
	}
	rest := out[idx:]
	if end := strings.Index(rest[1:], "\n["); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

// createDoctorChannel 建一条渠道，返回它的 ID 与名字。
func createDoctorChannel(t *testing.T, models ...string) (int64, string) {
	t.Helper()
	setupEnv(t)
	withStdin(t, testSecret+"\n")

	name := uniqueName("doctor-channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelCreate([]string{
			"--name", name, "--type", testChanType, "--base-url", testBaseURL,
			"--models", strings.Join(models, ","), "--credential-stdin",
		}); err != nil {
			t.Fatalf("建渠道失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}
	return id, name
}

// createDoctorModel 往目录里加一条规范模型。
func createDoctorModel(t *testing.T, canonical string) {
	t.Helper()
	if err := runCaptured(t, func() error {
		return runModelAdd([]string{"--canonical", canonical, "--display", "示例模型"})
	}); err != nil {
		t.Fatalf("建规范模型 %s 失败: %v", canonical, err)
	}
}

// 下面三条断言的是 collectDoctorReport 收出来的**问题清单**，而不是打出来的
// 那几行：doctor 的检查面是整个库，而这个包里的其他用例也在往同一个库上写
// 行，于是清单里会有与本次无关的问题。打印时每项最多列 doctorMaxListed 条
// （超出的折成一句「另有 N 条」），所以「本次造的那一条排在第几」取决于库里
// 攒了多少东西——按打印出来的行断言会变成一条随别的用例数量漂移的用例。
//
// 「跑完整条命令、按打出来的行断言」由 TestDoctorIntegrationLiveAgainstFakeGateway
// 负责，那里断言的是它自己那一节。

// TestDoctorIntegrationFlagsChannelModelMismatch 是 issue 第一个例子的现场：
// 渠道的 --models 里写一个目录里没有的名字，doctor 必须点名说出来。
func TestDoctorIntegrationFlagsChannelModelMismatch(t *testing.T) {
	db := testDB(t)

	canonical := uniqueName("doctor-model")
	createDoctorModel(t, canonical)
	// 差一个字符：这正是「写错一个字」在库里的样子。
	channelID, channelName := createDoctorChannel(t, canonical+"-typo")

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.catalog.findings,
		canonical+"-typo", strconv.FormatInt(channelID, 10), channelName, "目录里没有这个名字")
	if rep.catalog.status() != doctorFailed {
		t.Errorf("第 3 项的结论应当是不通过，得到 %s", rep.catalog.status().label())
	}
	err := rep.exitErr(nil)
	if err == nil {
		t.Fatal("库里有对不上的名字时 doctor 该非零退出")
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望 %d（有问题）", got, exitFailure)
	}
}

// TestDoctorIntegrationFlagsAliasWithoutChannel 是 issue 第二个例子的现场：
// 别名配了，但没有任何渠道的 models 里包含它。
func TestDoctorIntegrationFlagsAliasWithoutChannel(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-alias")
	createDoctorModel(t, canonical)
	upstream := uniqueName("upstream")
	if err := runCaptured(t, func() error {
		return runAliasAdd([]string{"--canonical", canonical, "--upstream", upstream})
	}); err != nil {
		t.Fatalf("建别名失败: %v", err)
	}

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.alias.findings,
		canonical, "没有任何 enabled 渠道的 models 里包含它")
	if rep.alias.status() != doctorFailed {
		t.Errorf("第 4 项的结论应当是不通过，得到 %s", rep.alias.status().label())
	}
}

// TestDoctorIntegrationFlagsUserWithoutWallet 钉住 #337 那类缺口被指出来，
// 而不是让人从 500 往回推。
func TestDoctorIntegrationFlagsUserWithoutWallet(t *testing.T) {
	db := testDB(t)

	name := uniqueName("doctor-user")
	if err := runCaptured(t, func() error {
		return runUserCreate([]string{"--name", name})
	}); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.wallet.findings, name, "没有钱包行")
	if rep.wallet.status() != doctorFailed {
		t.Errorf("第 6 项的结论应当是不通过，得到 %s", rep.wallet.status().label())
	}
}

// TestDoctorIntegrationPrintsEverySection 钉住跑完整条命令时的输出形状。
//
// 断言的是「六节都在、汇总行在、退出码非 0」，不涉及任何一条具体问题——
// 那些问题取决于库里当时有什么，见上面那段说明。
func TestDoctorIntegrationPrintsEverySection(t *testing.T) {
	testDB(t)

	out, err := runDoctorCaptured(t)
	if err == nil {
		t.Fatalf("这个库不可能六项全通过（用例自己就会造出没有钱包行的用户）：\n%s", out)
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", got, exitFailure)
	}
	for _, title := range []string{
		"[1/6] 数据库", "[2/6] 渠道", "[3/6] 模型目录",
		"[4/6] 别名", "[5/6] 计价规则", "[6/6] 钱包", "[真打上游]",
	} {
		if !strings.Contains(out, title) {
			t.Errorf("报告里没有 %q：\n%s", title, out)
		}
	}
	if !strings.Contains(out, "汇总：通过 ") {
		t.Errorf("报告里没有汇总行：\n%s", out)
	}
	if !strings.Contains(out, "没跑：要加 --live") {
		t.Errorf("没开 --live 时该打一行「没跑」：\n%s", out)
	}
}

// TestDoctorIntegrationLiveAgainstFakeGateway 跑完整条命令，只是把网关换成
// 一个假的：证明 --live 那条路在真实调用链上真的发出去了、并把结果打了出来。
//
// 这里不断言整份报告的结论：本包的其他用例也往同一个库上写行，而那些行会
// 让 doctor 报出与本次无关的问题。要断言的是**那一节**说了什么。
func TestDoctorIntegrationLiveAgainstFakeGateway(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-live")
	createDoctorModel(t, canonical)
	createDoctorChannel(t, canonical)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","model":"`+canonical+`","choices":[],`+
			`"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)
	}))
	defer srv.Close()

	withStdin(t, testDoctorKey+"\n")
	out, _ := runDoctorCaptured(t,
		"--live", "--model", canonical, "--base-url", srv.URL, "--credential-stdin")

	if gotPath != doctorPathChat {
		t.Fatalf("假网关收到的路径 = %q，期望 %q", gotPath, doctorPathChat)
	}
	liveSection := doctorSection(t, out, "[真打上游]")
	for _, want := range []string{"POST " + srv.URL + doctorPathChat, canonical, "prompt=4 completion=1", "结论：通过"} {
		if !strings.Contains(liveSection, want) {
			t.Errorf("真打上游那一节里没有 %q：\n%s", want, liveSection)
		}
	}
	if strings.Contains(out, testDoctorKey) {
		t.Errorf("凭证出现在了输出里：\n%s", out)
	}
}

// ── 用例内的断言辅助 ────────────────────────────────────────────────────

// assertContainsFragments 断言问题清单里每一项片段都出现过。
//
// 按**片段**而不是整句比对：整句断言会把措辞的每一次调整都变成一次失败，
// 而这里要钉住的是「有没有把这件事指出来、有没有点名到具体对象」。
func assertContainsFragments(t *testing.T, findings []string, want ...string) {
	t.Helper()
	for _, fragment := range want {
		found := false
		for _, f := range findings {
			if strings.Contains(f, fragment) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("问题清单里没有含 %q 的一条：%q", fragment, findings)
		}
	}
}
