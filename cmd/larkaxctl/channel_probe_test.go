package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/upstream"
)

// 本文件是 `channel adapters` 与 `channel probe` 的用例。
//
// 两条命令的共同前提是**不连库**：任何一条用例跑起来都需要 LARKAX_DB_DSN 的
// 话，它们就不再是「落库之前问一次」的工具了。所以每条用例开头都清掉那个
// 变量，成功本身就证明这条链没碰数据库。

// withoutDatabase 清掉 DSN，让「不连库」成为这条用例的前提。
func withoutDatabase(t *testing.T) {
	t.Helper()
	t.Setenv("LARKAX_DB_DSN", "")
	if os.Getenv("LARKAX_DB_DSN") != "" {
		t.Fatalf("LARKAX_DB_DSN 没有清掉：%q", os.Getenv("LARKAX_DB_DSN"))
	}
}

// probeUpstream 是 channel probe 打的假上游。
//
// 记下每次请求的路径与头：判据里最重要的一条是「说了要鉴权的那次真的带上了」。
type probeUpstream struct {
	*httptest.Server

	mu      sync.Mutex
	paths   []string
	headers []http.Header
}

func newProbeUpstream(t *testing.T, handler http.HandlerFunc) *probeUpstream {
	t.Helper()
	up := &probeUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.paths = append(up.paths, r.URL.Path)
		up.headers = append(up.headers, r.Header.Clone())
		up.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	return up
}

// request 返回第 i 次请求的路径与头。
func (u *probeUpstream) request(t *testing.T, i int) (string, http.Header) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if i >= len(u.paths) {
		t.Fatalf("上游只收到 %d 次请求，要第 %d 次的", len(u.paths), i)
	}
	return u.paths[i], u.headers[i]
}

// modelsJSON 是一份 OpenAI 形状的模型清单响应。
func modelsJSON(models []string) string {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i, model := range models {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%q}`, model)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// TestChannelAdaptersMatchProbeRegistry 钉住 adapters 打出的类型集合与
// ProbeRegistry().Types() 一致，且协议与端点逐行对得上。
//
// 期望值现场从注册表取，不手抄：手抄的清单只能证明两份字面量彼此一致，
// 加一个适配器时它不会响。
func TestChannelAdaptersMatchProbeRegistry(t *testing.T) {
	withoutDatabase(t)

	var err error
	stdout, stderr := captureStreams(t, func() { err = runChannelAdapters(nil) })
	if err != nil {
		t.Fatalf("channel adapters 出错: %v（stderr=%q）", err, stderr)
	}

	registry, regErr := upstream.ProbeRegistry()
	if regErr != nil {
		t.Fatalf("ProbeRegistry 出错: %v", regErr)
	}
	want := registry.Types()

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != len(want)+1 {
		t.Fatalf("表有 %d 行（含表头），期望表头 + %d 个类型：\n%s", len(lines), len(want), stdout)
	}
	if header := strings.Fields(lines[0]); !slices.Equal(header, []string{"类型", "协议", "端点"}) {
		t.Errorf("表头 = %v，期望「类型 协议 端点」", header)
	}

	var gotTypes []string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("这一行不是三栏：%q（整张表：\n%s）", line, stdout)
		}
		gotTypes = append(gotTypes, fields[0])
		adapter, lookupErr := registry.Lookup(fields[0])
		if lookupErr != nil {
			t.Fatalf("表里列了注册表不认识的类型 %q：%v", fields[0], lookupErr)
		}
		if fields[1] != string(adapter.Protocol()) {
			t.Errorf("%s 的协议 = %q，注册表里是 %q", fields[0], fields[1], adapter.Protocol())
		}
		if fields[2] != string(adapter.Endpoint()) {
			t.Errorf("%s 的端点 = %q，注册表里是 %q", fields[0], fields[2], adapter.Endpoint())
		}
	}
	if !slices.Equal(gotTypes, want) {
		t.Errorf("表里的类型 = %v，注册表的 Types() = %v（应当逐字、逐序相等）", gotTypes, want)
	}
}

// TestChannelAdaptersRejectsPositionalArgs 钉住多余的位置参数是参数错。
func TestChannelAdaptersRejectsPositionalArgs(t *testing.T) {
	withoutDatabase(t)

	err := runCaptured(t, func() error { return runChannelAdapters([]string{"openai"}) })
	if err == nil {
		t.Fatal("adapters 带了位置参数应当被拒")
	}
	if !strings.Contains(err.Error(), "不接受位置参数") {
		t.Errorf("错误信息应当点出位置参数，得到 %q", err.Error())
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望参数错的 %d", got, exitUsage)
	}
}

// TestChannelProbeSendsCredential 钉住有鉴权的上游：请求打到 /v1/models，
// 头里带上这次给的凭证，清单按名字升序一行一个。
func TestChannelProbeSendsCredential(t *testing.T) {
	withoutDatabase(t)

	const credential = "test-key-placeholder"
	const envName = "LARKAX_TEST_PROBE_CREDENTIAL"
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, modelsJSON([]string{"model-b", "model-a"}))
	})
	t.Setenv(envName, credential)

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runChannelProbe([]string{
			"--type", "openai", "--base-url", up.URL, "--credential-env", envName,
		})
	})
	if err != nil {
		t.Fatalf("探测失败: %v（stdout=%q stderr=%q）", err, stdout, stderr)
	}

	path, header := up.request(t, 0)
	if path != "/v1/models" {
		t.Errorf("上游收到的路径 = %q，期望 /v1/models", path)
	}
	if got := header.Get("Authorization"); got != "Bearer "+credential {
		t.Errorf("上游收到的 Authorization = %q，期望带上这次给的凭证", got)
	}

	want := "上游提供 2 个模型。\nmodel-a\nmodel-b\n"
	if stdout != want {
		t.Errorf("stdout = %q，期望 %q", stdout, want)
	}
	if strings.Contains(stdout+stderr, credential) {
		t.Errorf("输出里出现了凭证：stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestChannelProbeUnauthenticatedUpstream 钉住无鉴权的自建上游：不给凭证
// 也能探测成功，而且不会凭空编一把钥匙送出去。
func TestChannelProbeUnauthenticatedUpstream(t *testing.T) {
	withoutDatabase(t)

	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, modelsJSON([]string{"model-a"}))
	})

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runChannelProbe([]string{"--type", "openai", "--base-url", up.URL})
	})
	if err != nil {
		t.Fatalf("无鉴权探测失败: %v（stdout=%q stderr=%q）", err, stdout, stderr)
	}
	if want := "上游提供 1 个模型。\nmodel-a\n"; stdout != want {
		t.Errorf("stdout = %q，期望 %q", stdout, want)
	}

	// 适配器会照常写一个 Authorization 头；没给凭证时它必须是空的。
	_, header := up.request(t, 0)
	if token := strings.TrimSpace(strings.TrimPrefix(header.Get("Authorization"), "Bearer")); token != "" {
		t.Errorf("没给凭证，上游却收到了 %q", header.Get("Authorization"))
	}
}

// TestChannelProbeUnsupportedUpstreamIsNotAnError 钉住三档结果里的第三档：
// 上游不提供模型列表时退出 0，只打一句说明，**stdout 上没有清单**。
//
// 逐字相等是刻意的：只断言「含这一句」的话，实现同时在别处打了一个空清单
// 表头也照样通过，而那正是「空清单与失败分不开」的来源。
func TestChannelProbeUnsupportedUpstreamIsNotAnError(t *testing.T) {
	withoutDatabase(t)

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runChannelProbe([]string{
			"--type", "systemone", "--base-url", "https://api.example.com/v1",
		})
	})
	if err != nil {
		t.Fatalf("上游不支持模型列表不是错误，实际返回 %v（stderr=%q）", err, stderr)
	}
	if got := exitCode(err); got != exitOK {
		t.Errorf("退出码 = %d，期望 0", got)
	}
	want := "这家上游（systemone）不提供模型列表，这不算错误。\n"
	if stdout != want {
		t.Errorf("stdout = %q，期望只有那一句 %q（不许打空清单）", stdout, want)
	}
	if stderr != "探测 systemone 协议，地址 https://api.example.com/v1。\n" {
		t.Errorf("stderr = %q，期望只有这次探测的协议与地址", stderr)
	}
}

// TestChannelProbeFailureRedactsCredential 钉住失败那一档：退出 1，错误里
// 带上游状态码与它那句原文，而原文里回显的凭证已经被换成 <redacted>。
//
// 这三条缺一条都会让「不回显凭证」变成空话：只判「不含凭证」时，实现把
// 正文整段丢掉也照样通过；只判状态码时，正文里的凭证会原样跑到终端上。
func TestChannelProbeFailureRedactsCredential(t *testing.T) {
	withoutDatabase(t)

	const credential = "sk-echo-placeholder"
	const envName = "LARKAX_TEST_PROBE_BAD_CREDENTIAL"
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":"invalid key %s"}`, credential)
	})
	t.Setenv(envName, credential)

	err := runCaptured(t, func() error {
		return runChannelProbe([]string{
			"--type", "openai", "--base-url", up.URL, "--credential-env", envName,
		})
	})
	if err == nil {
		t.Fatal("上游返回 401 时探测应当失败")
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望其他失败的 %d", got, exitFailure)
	}
	msg := err.Error()
	if strings.Contains(msg, credential) {
		t.Errorf("错误信息里出现了凭证：%q", msg)
	}
	if !strings.Contains(msg, "<redacted>") {
		t.Errorf("上游原文里回显的凭证没有被换成 <redacted>：%q", msg)
	}
	if !strings.Contains(msg, "401") {
		t.Errorf("错误信息没说上游状态码：%q", msg)
	}
}

// TestChannelProbeTruncatesLongList 钉住截断那一档：Total 是截断前的条数，
// 首行写明一共多少，打出来的行数等于上限。
func TestChannelProbeTruncatesLongList(t *testing.T) {
	withoutDatabase(t)

	models := make([]string, 0, 2001)
	for i := 0; i < 2001; i++ {
		models = append(models, fmt.Sprintf("model-%04d", i))
	}
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, modelsJSON(models))
	})

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runChannelProbe([]string{"--type", "openai", "--base-url", up.URL})
	})
	if err != nil {
		t.Fatalf("探测失败: %v（stderr=%q）", err, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2001 {
		t.Fatalf("打了 %d 行，期望 1 行摘要 + 2000 个模型", len(lines))
	}
	if want := "上游提供 2001 个模型，以下只列出前 2000 个。"; lines[0] != want {
		t.Errorf("首行 = %q，期望 %q", lines[0], want)
	}
}

// TestChannelProbeRejectsBadArguments 逐个钉住参数错（退出码 2）。
func TestChannelProbeRejectsBadArguments(t *testing.T) {
	withoutDatabase(t)

	const badCredentialEnv = "LARKAX_TEST_PROBE_UNSET"
	t.Setenv(badCredentialEnv, "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺 --type", []string{"--base-url", "https://api.example.com/v1"}, "缺少 --type"},
		{"缺 --base-url", []string{"--type", "openai"}, "缺少 --base-url"},
		{"未知类型", []string{"--type", "no-such-type", "--base-url", "https://api.example.com/v1"},
			"未知的渠道类型"},
		{"地址带 query", []string{"--type", "openai", "--base-url", "https://api.example.com/v1?tenant=a"},
			"base_url 不能带 query"},
		{"两个凭证来源", []string{
			"--type", "openai", "--base-url", "https://api.example.com/v1",
			"--credential-stdin", "--credential-env", "LARKAX_TEST_PROBE_CREDENTIAL",
		}, "--credential-stdin 与 --credential-env 只能给一个"},
		{"环境变量没设", []string{
			"--type", "openai", "--base-url", "https://api.example.com/v1",
			"--credential-env", badCredentialEnv,
		}, "未设置或为空"},
		{"位置参数", []string{
			"--type", "openai", "--base-url", "https://api.example.com/v1", "extra",
		}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runChannelProbe(tc.args) })
			if err == nil {
				t.Fatalf("%v 应当被拒", tc.args)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望参数错的 %d：%v", got, exitUsage, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 = %q，期望含 %q", err.Error(), tc.want)
			}
		})
	}

	// 未知类型的报错要列出注册表里的**每一个**类型：清单从 Types() 取，
	// 这里逐个核一遍，而不是只核「含已知类型」这几个字。
	registry, regErr := upstream.ProbeRegistry()
	if regErr != nil {
		t.Fatalf("ProbeRegistry 出错: %v", regErr)
	}
	err := runCaptured(t, func() error {
		return runChannelProbe([]string{"--type", "no-such-type", "--base-url", "https://api.example.com/v1"})
	})
	if err == nil {
		t.Fatal("未知类型应当被拒")
	}
	for _, known := range registry.Types() {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("未知类型的报错里没有列出 %q：%q", known, err.Error())
		}
	}
}

// TestChannelProbeAndAdaptersRejectJSONFormat 钉住「不静默回落成表格」：
// json 档下两个新命令都要显式报参数错，而不是打一张给人看的表。
func TestChannelProbeAndAdaptersRejectJSONFormat(t *testing.T) {
	withoutDatabase(t)
	withFormat(t, formatJSON)

	cases := []struct {
		command string
		run     func() error
	}{
		{"channel adapters", func() error { return runChannelAdapters(nil) }},
		{"channel probe", func() error { return runChannelProbe(nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			err := runCaptured(t, tc.run)
			if err == nil {
				t.Fatalf("%s 在 json 模式下应当被拒", tc.command)
			}
			if !strings.Contains(err.Error(), tc.command+" 还不支持 --format json") {
				t.Errorf("错误信息应当指出 %s 不支持 json，得到 %q", tc.command, err.Error())
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望参数错的 %d", got, exitUsage)
			}
		})
	}
}

// TestChannelAdaptersEmptyRegistryIsInternalError 钉住空注册表那一格。
//
// 正常构建里造不出空表（内置清单恒为四项），所以这里把取注册表的那个包级
// 变量临时换掉——它存在的唯一理由就是这条用例。空表不能被读成「今天没有
// 适配器」：没有任何添加适配器的命令，这句话会让读者去找一条走不通的路。
// 它说的是一件内部故障，所以退出码是 1，文案以「内部错误」起头。
//
// 这个变量是全局的：本条**不并行**，跑完立刻还原成真实现。别的用例若并行，
// 它们会在本条结束后才继续（testing 的并行语义），不会看到被换掉的那一份。
func TestChannelAdaptersEmptyRegistryIsInternalError(t *testing.T) {
	withoutDatabase(t)

	probeChannelRegistry = func() (*channel.Registry, error) { return channel.NewRegistry(), nil }
	t.Cleanup(func() { probeChannelRegistry = upstream.ProbeRegistry })

	err := runCaptured(t, func() error { return runChannelAdapters(nil) })
	if err == nil {
		t.Fatal("空注册表应当报错")
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望其他失败的 %d", got, exitFailure)
	}
	const want = "内部错误: 内置适配器注册表为空：这个二进制的装配不完整"
	if got := err.Error(); got != want {
		t.Errorf("错误文本 = %q，期望逐字 %q", got, want)
	}
	// 「添加适配器」不是一个可执行的下一步：这话一出现，读者就会去试一条
	// 不存在的命令。
	if strings.Contains(err.Error(), "添加") {
		t.Errorf("错误文本给出了一个不存在的动作：%q", err.Error())
	}
}

// TestChannelHelpListsAdaptersAndProbe 钉住短版与长版都写上了这两个动作。
//
// 短版受一屏预算约束，最容易在下次加动作时被悄悄挤掉；而它在参数写错时
// 就是读者唯一看到的一页。
func TestChannelHelpListsAdaptersAndProbe(t *testing.T) {
	for _, help := range []struct {
		name string
		text string
	}{
		{"短版", channelShortUsage},
		{"长版", channelUsage},
	} {
		for _, want := range []string{"channel adapters", "channel probe --type"} {
			if !strings.Contains(help.text, want) {
				t.Errorf("%s里没有 %q", help.name, want)
			}
		}
	}
}

// TestChannelHelpStatesOwnershipBoundary 钉住两版用法文本都写明「归属没有修改入口」。
//
// 短版受一屏预算约束，最容易被下次加内容时挤掉；而它在参数写错时是读者唯一
// 看到的一页。归属在 add 时定下之后没有修改入口这件事，只在文档里说是不够的。
func TestChannelHelpStatesOwnershipBoundary(t *testing.T) {
	for _, help := range []struct {
		name string
		text string
	}{
		{"短版", channelShortUsage},
		{"长版", channelUsage},
	} {
		for _, want := range []string{"归属", "没有修改入口"} {
			if !strings.Contains(help.text, want) {
				t.Errorf("%s里没有 %q", help.name, want)
			}
		}
	}
}
