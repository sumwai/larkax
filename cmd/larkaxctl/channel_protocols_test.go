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
		{"两个都没给", base, "缺少 --type 或 --protocols"},
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
		CredentialKeys: []string{"test"},
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
		{"凭证", "test"},
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
		"base_url", "created_at", "credential_error", "credential_hint",
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
		return runChannelCreate([]string{
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
