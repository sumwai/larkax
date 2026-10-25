package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
)

// ── --protocols 的参数形状 ──────────────────────────────────────────────
//
// 这一组全部跑在连库之前：参数写错不该先连一次库，而且没有 DSN 的机器上也
// 必须真的跑（碰库的用例在那边是跳过的，而「跳过」与「通过」长得一样）。

func TestChannelCreateProtocolsParse(t *testing.T) {
	base := []string{
		"--name", "c1", "--base-url", testBaseURL, "--models", "m1", "--credential-stdin",
	}

	cases := []struct {
		name      string
		args      []string
		wantType  string
		wantProto []string
	}{
		{
			name:      "只给 --type（旧写法）",
			args:      append(append([]string{}, base...), "--type", testChanType),
			wantType:  testChanType,
			wantProto: nil,
		},
		{
			name:      "只给 --protocols",
			args:      append(append([]string{}, base...), "--protocols", "anthropic,openai"),
			wantType:  "",
			wantProto: []string{"anthropic", "openai"},
		},
		{
			name: "两个都给",
			args: append(append([]string{}, base...),
				"--type", "openai", "--protocols", "openai,anthropic"),
			wantType:  "openai",
			wantProto: []string{"openai", "anthropic"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每格一条新的标准输入：凭证读一次就到文件结束了，共用一条的话
			// 第一格之后的每一格都会报「标准输入里没有凭证」。
			withStdin(t, testSecret)
			req, _, err := parseChannelCreate(tc.args)
			if err != nil {
				t.Fatalf("解析出错: %v", err)
			}
			if req.Type != tc.wantType {
				t.Errorf("Type = %q，期望 %q", req.Type, tc.wantType)
			}
			if strings.Join(req.Protocols, ",") != strings.Join(tc.wantProto, ",") {
				t.Errorf("Protocols = %v，期望 %v", req.Protocols, tc.wantProto)
			}
		})
	}
}

// 两个都没给、或者给了空的 --protocols，都该在连库之前报出来。
func TestChannelCreateProtocolsRejectsMissing(t *testing.T) {
	withStdin(t, testSecret)

	base := []string{
		"--name", "c1", "--base-url", testBaseURL, "--models", "m1", "--credential-stdin",
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		// 没给凭证才要求点名协议：给了凭证时协议由探测得出，那时缺 --type
		// 不是参数错（见 probeProtocolsForAdd）。
		{"两个都没给", without(base, "--credential-stdin"), "缺少 --type 或 --protocols"},
		{"--protocols 是空的", append(append([]string{}, base...), "--protocols", "  "),
			"--protocols 里没有协议名"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseChannelCreate(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望报错且含 %q，得到 %v", tc.want, err)
			}
		})
	}
}

// update 的 --protocols 是「整体替换」，所以它必须真的出现在请求里（指针非
// nil），而「没给」与「给了空」要分开：前者是没改这一项，后者是写错了。
func TestChannelUpdateProtocolsParse(t *testing.T) {
	req, err := parseChannelUpdate([]string{"--id", "3", "--protocols", "openai,anthropic"})
	if err != nil {
		t.Fatalf("解析出错: %v", err)
	}
	if req.Protocols == nil || strings.Join(*req.Protocols, ",") != "openai,anthropic" {
		t.Fatalf("Protocols = %v，期望整体替换成两项", req.Protocols)
	}

	if _, err := parseChannelUpdate([]string{"--id", "3", "--protocols", ""}); err == nil ||
		!strings.Contains(err.Error(), "--protocols 里没有协议名") {
		t.Fatalf("空的 --protocols 应当报错，得到 %v", err)
	}

	if _, err := parseChannelUpdate([]string{"--id", "3"}); err == nil ||
		!strings.Contains(err.Error(), "至少要改一项") {
		t.Fatalf("什么都没改应当报错，得到 %v", err)
	}
}

// ── 单协议渠道的输出必须一字节不差 ──────────────────────────────────────

// sampleSingleProtocolChannel 是一条从 --type 建出来的渠道在多协议之后的样子：
// 迁移会为它写一行 channel_protocols，读取侧也把它读进 ChannelInfo.Protocols。
//
// 这正是一条**行为不许变**的渠道：它今天怎么打，加完多协议之后还得怎么打。
func sampleSingleProtocolChannel(withProtocols bool) channels.ChannelInfo {
	info := channels.ChannelInfo{
		ID: 7, Name: "c1", Type: testChanType, BaseURL: testBaseURL,
		Status: channels.StatusEnabled, Models: []string{"m1"}, Weight: 1,
		// 凭证在账号上（见 00067）：平铺的三个字段描述主账号，与 List 读出来
		// 的那份保持一致，否则夹具本身就是一个库里到不了的状态。
		Accounts: []channels.ChannelAccount{{
			ID: 1, Name: "acc-1", Weight: 1, Hint: "cdef",
			CredentialKeys: []string{"test"},
		}},
		CredentialKeys: []string{"test"},
		Hint:           "cdef",
		CreatedAt:      time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}
	if withProtocols {
		info.Protocols = []channel.Protocol{channel.ProtocolOpenAI}
	}
	return info
}

// TestChannelSingleProtocolTextOutputUnchanged 钉住 channel get 的文本输出。
//
// 判据有两层，缺一层都漏得过去：
//
//  1. **带着协议集合与不带，渲染结果逐字节相同**——证明新加的那一栏没有从
//     某个角落漏进输出；
//  2. 逐行的标签与取值与加多协议之前那份清单**逐项相同**——只看 1 的话，
//     两版都多打一行「协议」也是绿的。
func TestChannelSingleProtocolTextOutputUnchanged(t *testing.T) {
	without := renderChannelDetail(t, sampleSingleProtocolChannel(false))
	with := renderChannelDetail(t, sampleSingleProtocolChannel(true))

	if with != without {
		t.Errorf("带上协议集合之后 channel get 的输出变了：\n--- 不带 ---\n%s\n--- 带 ---\n%s", without, with)
	}

	lines := strings.Split(strings.TrimSuffix(with, "\n"), "\n")
	// 一栏一行，顺序固定。逐项写出来而不是只数行数：换掉一栏、挪个位置
	// 都会在这里红。
	want := [][2]string{
		{"ID", "7"},
		{"名称", "c1"},
		{"状态", string(channels.StatusEnabled)},
		{"类型", testChanType},
		{"上游地址", testBaseURL},
		{"模型", "m1"},
		{"归属", "平台"},
		{"权重", "1"},
		{"优先级", "0"},
		{"凭证", "test …cdef"},
		{"账号", "acc-1"},
		{"setting", "{}"},
		{"创建时间", "2026-10-01T00:00:00Z"},
		{"更新时间", "2026-10-02T00:00:00Z"},
	}
	if len(lines) != len(want) {
		t.Fatalf("输出 %d 行，期望 %d 行（多一栏就是行为变了）：\n%s", len(lines), len(want), with)
	}
	// 标签与取值之间是两个以上空格（表格对齐），取值本身不含空格。
	lineRE := regexp.MustCompile(`^(\S+)\s{2,}(.*)$`)
	for i, line := range lines {
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("第 %d 行不是「标签 + 对齐空格 + 取值」：%q", i+1, line)
		}
		if m[1] != want[i][0] || m[2] != want[i][1] {
			t.Errorf("第 %d 行 = %q / %q，期望 %q / %q",
				i+1, m[1], m[2], want[i][0], want[i][1])
		}
	}
}

func renderChannelDetail(t *testing.T, info channels.ChannelInfo) string {
	t.Helper()
	var buf bytes.Buffer
	printChannelDetail(&buf, info)
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("非 TTY 下不该带样式码：%q", buf.String())
	}
	return buf.String()
}

// TestChannelSingleProtocolJSONOutputUnchanged 钉住 channel get -o json。
//
// 这里用**键集合**而不是「有没有 protocols 这个键」：只看后者的话，顺手多塞
// 一个别的栏也照样绿。键集合既钉住「没多」，也钉住「没少」。
func TestChannelSingleProtocolJSONOutputUnchanged(t *testing.T) {
	withoutDoc := channelGetDoc(sampleSingleProtocolChannel(false))
	withDoc := channelGetDoc(sampleSingleProtocolChannel(true))

	withoutRaw, err := json.Marshal(withoutDoc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	withRaw, err := json.Marshal(withDoc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(withRaw) != string(withoutRaw) {
		t.Errorf("-o json 的输出因为多了协议集合而变了：\n%s\n%s", withoutRaw, withRaw)
	}

	entry := jsonObjectField(t, jsonRoundTrip(t, withDoc), "channel")
	want := []string{
		"accounts", "base_url", "created_at", "credential_error", "credential_hint",
		"credential_keys", "deleted_at", "id", "models", "name",
		"owner_user_id", "priority", "setting", "status", "type", "updated_at",
		"weight",
	}
	got := make([]string, 0, len(entry))
	for key := range entry {
		got = append(got, key)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("channel 那一栏的键 = %v，期望 %v", got, want)
	}
}

// ── 未知协议名在写入时报错，且报错里带已知清单 ──────────────────────────

// TestChannelCreateRejectsUnknownProtocol 走一次完整的命令行写入路径。
//
// 与 channels 包的 TestCreateRejectsUnknownProtocol 判的是同一件事，但那一
// 条直接调管理面；这一条要看**使用者实际遇到的那句话**——错误从适配器注册
// 表里取已知清单，而不是在命令行另写一份（写第二份的那天就开始分岔）。
func TestChannelCreateRejectsUnknownProtocol(t *testing.T) {
	testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)

	err := runCaptured(t, func() error {
		return runChannelAdd([]string{
			"--name", uniqueName("channel"), "--protocols", "openai,no-such-protocol",
			"--base-url", testBaseURL, "--models", "m1", "--credential-stdin",
		})
	})
	if err == nil {
		t.Fatal("未知的协议应当被拒")
	}
	for _, want := range []string{`"no-such-protocol"`, "已知协议",
		string(channel.ProtocolOpenAI), string(channel.ProtocolAnthropic)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%v", want, err)
		}
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("报错里出现了凭证本身：%v", err)
	}
}

// ── 每个协议自己的地址 ──────────────────────────────────────────────────

// 分址旗标点名的协议等于被绑上：给了地址就够了，不必再写 --protocols；
// 两个都写时它们必须指同一批协议，不一致是参数错（不是静默取一个）。
func TestChannelCreateProtocolBaseURLFlags(t *testing.T) {
	const (
		baseURL      = "https://api.example.com/v1"
		anthropicURL = "https://api.example.com/anthropic"
	)
	cases := []struct {
		name          string
		args          []string
		wantType      string
		wantProtocols []string
		wantURLs      map[string]string
		wantErr       string
	}{
		{
			name:          "只给分址：协议由地址带出来",
			args:          []string{"--anthropic", anthropicURL},
			wantProtocols: []string{"anthropic"},
			wantURLs:      map[string]string{"anthropic": anthropicURL},
		},
		{
			name:          "--type 在前、分址协议跟在后",
			args:          []string{"--type", "openai", "--anthropic", anthropicURL},
			wantType:      "openai",
			wantProtocols: []string{"openai", "anthropic"},
			wantURLs:      map[string]string{"anthropic": anthropicURL},
		},
		{
			name:          "--protocols 与分址指同一批协议",
			args:          []string{"--protocols", "openai,anthropic", "--anthropic", anthropicURL},
			wantProtocols: []string{"openai", "anthropic"},
			wantURLs:      map[string]string{"anthropic": anthropicURL},
		},
		{
			name:    "--protocols 与分址不一致",
			args:    []string{"--protocols", "openai", "--anthropic", anthropicURL},
			wantErr: "不在这次的协议集合里",
		},
		{
			name:    "分址给的是空串",
			args:    []string{"--type", "openai", "--openai", "  "},
			wantErr: "--openai 的地址是空的",
		},
		{
			name:    "分址没过地址校验",
			args:    []string{"--type", "openai", "--openai", "not-a-url"},
			wantErr: "--openai 的地址不合法",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"--name", "c1", "--base-url", baseURL, "--models", "m1",
			}, tc.args...)
			req, _, err := parseChannelCreate(args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("期望报错且含 %q，得到 %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析出错: %v", err)
			}
			if req.Type != tc.wantType {
				t.Errorf("Type = %q，期望 %q", req.Type, tc.wantType)
			}
			if strings.Join(req.Protocols, ",") != strings.Join(tc.wantProtocols, ",") {
				t.Errorf("Protocols = %v，期望 %v", req.Protocols, tc.wantProtocols)
			}
			if len(req.ProtocolBaseURLs) != len(tc.wantURLs) {
				t.Fatalf("ProtocolBaseURLs = %v，期望 %v", req.ProtocolBaseURLs, tc.wantURLs)
			}
			for p, want := range tc.wantURLs {
				if got := req.ProtocolBaseURLs[p]; got != want {
					t.Errorf("ProtocolBaseURLs[%q] = %q，期望 %q", p, got, want)
				}
			}
		})
	}
}

// update 的分址：给了就写，空串是「清掉」（与「没给这一项」不是一回事）。
func TestChannelUpdateProtocolBaseURLFlags(t *testing.T) {
	const anthropicURL = "https://api.example.com/anthropic"

	req, err := parseChannelUpdate([]string{"--id", "3", "--anthropic", anthropicURL})
	if err != nil {
		t.Fatalf("解析出错: %v", err)
	}
	if req.ProtocolBaseURLs == nil || (*req.ProtocolBaseURLs)["anthropic"] != anthropicURL {
		t.Fatalf("ProtocolBaseURLs = %v，期望写入 anthropic 的地址", req.ProtocolBaseURLs)
	}

	clear, err := parseChannelUpdate([]string{"--id", "3", "--anthropic", ""})
	if err != nil {
		t.Fatalf("解析出错: %v", err)
	}
	if clear.ProtocolBaseURLs == nil {
		t.Fatal("空串也是给了这一项：ProtocolBaseURLs 不该是 nil")
	}
	if got, ok := (*clear.ProtocolBaseURLs)["anthropic"]; !ok || got != "" {
		t.Errorf("ProtocolBaseURLs = %v，期望 anthropic 对应空串（清掉）", *clear.ProtocolBaseURLs)
	}

	if _, err := parseChannelUpdate([]string{
		"--id", "3", "--protocols", "openai", "--anthropic", anthropicURL,
	}); err == nil || !strings.Contains(err.Error(), "不在这次的协议集合里") {
		t.Fatalf("集合外的分址应当报错，得到 %v", err)
	}
	if _, err := parseChannelUpdate([]string{"--id", "3", "--anthropic", "not-a-url"}); err == nil ||
		!strings.Contains(err.Error(), "--anthropic 的地址不合法") {
		t.Fatalf("地址没过校验应当报错，得到 %v", err)
	}
}

// splitProtocolChannel 是一条声明两套协议、anthropic 配了自己地址的渠道。
func splitProtocolChannel() channels.ChannelInfo {
	info := sampleSingleProtocolChannel(true)
	info.Protocols = []channel.Protocol{channel.ProtocolOpenAI, channel.ProtocolAnthropic}
	info.ProtocolBaseURLs = map[channel.Protocol]string{
		channel.ProtocolAnthropic: "https://api.example.com/anthropic",
	}
	return info
}

// protocolAddressRow 从 channel get 的文本输出里取出「协议地址」那一行的取值。
//
// 不按固定列宽找：那一栏的标签宽度由同一张表里最长的标签决定，写死空格数
// 会让这个用例在别的字段改名之后莫名其妙地红。
func protocolAddressRow(t *testing.T, rendered string) (string, bool) {
	t.Helper()
	lineRE := regexp.MustCompile(`^(\S+)\s{2,}(.*)$`)
	for _, line := range strings.Split(rendered, "\n") {
		m := lineRE.FindStringSubmatch(line)
		if m != nil && m[1] == "协议地址" {
			return m[2], true
		}
	}
	return "", false
}

// channel get 的文本输出：多一栏「协议地址」，逐个协议列出实际生效的地址。
func TestChannelSplitProtocolTextOutput(t *testing.T) {
	want := "anthropic=https://api.example.com/anthropic,openai=" + testBaseURL
	if got, ok := protocolAddressRow(t, renderChannelDetail(t, splitProtocolChannel())); !ok || got != want {
		t.Errorf("协议地址 = %q（在不在：%v），期望 %q", got, ok, want)
	}

	// 多协议但没配专属地址：每一侧都沿用渠道地址，同样要打出来——
	// 这时「上游地址」那一栏已经不等于每一侧实际打的地址。
	none := sampleSingleProtocolChannel(true)
	none.Protocols = []channel.Protocol{channel.ProtocolOpenAI, channel.ProtocolAnthropic}
	wantNone := "anthropic=" + testBaseURL + ",openai=" + testBaseURL
	if got, ok := protocolAddressRow(t, renderChannelDetail(t, none)); !ok || got != wantNone {
		t.Errorf("多协议没分址时协议地址 = %q（在不在：%v），期望 %q", got, ok, wantNone)
	}
}

// channel get -o json：多一个 protocol_addresses 对象，键是协议名。
func TestChannelSplitProtocolJSONOutput(t *testing.T) {
	doc := channelGetDoc(splitProtocolChannel())
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	entry := jsonObjectField(t, jsonRoundTrip(t, doc), "channel")
	addrs, ok := entry["protocol_addresses"].(map[string]any)
	if !ok {
		t.Fatalf("channel 那一栏里没有 protocol_addresses 对象：%s", raw)
	}
	want := map[string]string{
		"anthropic": "https://api.example.com/anthropic",
		"openai":    testBaseURL,
	}
	if len(addrs) != len(want) {
		t.Fatalf("protocol_addresses = %v，期望 %v", addrs, want)
	}
	for p, wantURL := range want {
		if got, _ := addrs[p].(string); got != wantURL {
			t.Errorf("protocol_addresses[%q] = %v，期望 %q", p, addrs[p], wantURL)
		}
	}
}
