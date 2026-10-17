package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/settings"
)

func TestSettingsUnknownSubcommand(t *testing.T) {
	err := runSettings([]string{})
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runSettings([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 settings 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestSettingsSetRejectsBadArgs 走的是**不碰数据库**的那几条路径。
//
// 键名与值的类型都在连库之前校验（见 settings.EncodeValue）：
// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
// 还有一个更坏的可能——它没报错。
func TestSettingsSetRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --key", []string{"--value", "true", "--by", "ops"}, "缺少 --key"},
		{"没有 --by", []string{"--key", "plugin.enabled", "--value", "true"}, "缺少 --by"},
		{"--by 为空",
			[]string{"--key", "plugin.enabled", "--value", "true", "--by", ""}, "缺少 --by"},
		{"位置参数",
			[]string{"plugin.enabled", "--key", "plugin.enabled", "--by", "ops"}, "不接受位置参数"},
		{"未声明的键",
			[]string{"--key", "typo.retentoin_days", "--value", "7", "--by", "ops"}, "Registry"},
		{"值的类型不对",
			[]string{"--key", "plugin.enabled", "--value", "1", "--by", "ops"}, "true 或 false"},
		{"整数键给了文字",
			[]string{"--key", "request_snapshot.retention_days", "--value", "七天", "--by", "ops"}, "整数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runSettingsSet(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSettingsListRejectsPositional(t *testing.T) {
	err := runSettingsList([]string{"extra"})
	if err == nil || !strings.Contains(err.Error(), "不接受位置参数") {
		t.Fatalf("期望报错，得到 %v", err)
	}
}

// TestPrintSettingsList 钉住每一栏的取值。
//
// 值打的是 jsonb 原文：字符串带引号、布尔与整数不带。这不是排版偏好——
// 这一列里到底存了什么正是核对时要看的东西，而一个漏了引号的字符串配置
// 解码之后与正常的看起来一模一样，可它会让服务起不来。
func TestPrintSettingsList(t *testing.T) {
	by := int64(7)
	at := time.Date(2026, 10, 17, 6, 10, 0, 0, time.UTC)

	var buf bytes.Buffer
	printSettingsList(&buf, []settings.Entry{
		{
			// 接线正常的键：plugin.enabled 现在真的被读了（见 internal/plugin
			// 的 LoadConfig 与 internal/assembly 那几条装配用例），所以它
			// 不该再带「尚未接线」那一行。
			Key: "plugin.enabled", Value: []byte(`false`),
			Description: "是否启用插件系统。", Editable: true,
			UpdatedBy: &by, UpdatedAt: at,
		},
		{
			Key: "capture.consent_max_window_hours", Value: []byte(`24`),
			Description: "授权采集的最长窗口。", Editable: true,
			UpdatedAt: at,
		},
		{
			Key: "billing.default_asset", Value: []byte(`"CNY"`),
			Description: "新用户钱包的默认资产品种。", Editable: false,
			UpdatedAt: at,
		},
		{
			// 未声明的键：它是「服务加载不起来」的直接原因，要在类型那一栏
			// 标出来，而不是靠人记得去比对 Registry。
			Key: "typo.key", Value: []byte(`1`),
			Editable: true, UpdatedAt: at,
		},
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// 一行表头；plugin.enabled 两行（元数据、说明）；
	// capture.consent_max_window_hours 三行（元数据、说明、尚未接线）；
	// billing.default_asset 两行；typo.key 一行（没说明，也不是未接线的键）。
	if len(lines) != 1+2+3+2+1 {
		t.Fatalf("打印了 %d 行，期望 9 行：\n%s", len(lines), buf.String())
	}

	if !strings.Contains(lines[0], "jsonb 原文") {
		t.Errorf("表头没有说明值那一列是什么：%q", lines[0])
	}

	first := lines[1]
	for _, want := range []string{
		"plugin.enabled = false", "[bool]", "可改 是", "改自 7", "改于 2026-10-17T06:10:00Z",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("第一行里没有 %q：\n%s", want, first)
		}
	}
	if !strings.Contains(lines[2], "说明：是否启用插件系统。") {
		t.Errorf("第二行没有说明：%q", lines[2])
	}
	// 接线正常的键不该带上那一行：否则「标出来了」就不再是一个信号。
	if strings.Contains(strings.Join(lines[1:3], "\n"), "尚未接线") {
		t.Errorf("已接线的键被标成了未接线：\n%s", strings.Join(lines[1:3], "\n"))
	}

	// 还没有读者的键要在清单里当场说出来。这一行取自 Registry（见
	// settings.Spec.Unwired），所以它是一道真的门：谁把 Unwired 清空了，
	// 这条就挂——而清空正意味着「接上了」或「标错了」，两种都该看一眼。
	//
	// 断言写死字面量，不拿 Registry 里的值当期望：那等于让被测对象给自己
	// 出题，把文案改掉也照样通过。
	// 未接线的键占三行：元数据、说明、尚未接线。
	if !strings.Contains(lines[5], "尚未接线") {
		t.Errorf("未接线的键没有被标出来：%q", lines[5])
	}
	if !strings.Contains(lines[5], "授权采集那条路还没实现，改了不会有任何效果") {
		t.Errorf("未接线那一行没有说清后果：%q", lines[5])
	}

	// 字符串配置的值带引号：这就是它在 jsonb 里的样子。
	second := lines[6]
	for _, want := range []string{
		`billing.default_asset = "CNY"`, "[string]", "可改 否", "改自 -",
	} {
		if !strings.Contains(second, want) {
			t.Errorf("第三行里没有 %q：\n%s", want, second)
		}
	}
	if strings.Contains(strings.Join(lines[6:8], "\n"), "尚未接线") {
		t.Errorf("已接线的键被标成了未接线：\n%s", strings.Join(lines[6:8], "\n"))
	}

	third := lines[8]
	if !strings.Contains(third, "typo.key = 1") || !strings.Contains(third, "[未声明]") {
		t.Errorf("未声明的键没有被标出来：%q", third)
	}
}

// TestPrintSettingsSetResult 钉住 `settings set` 写完之后的输出。
//
// #215 点名的就是这条命令：改一个没人读的键，它回一句「已写入」，与改一个
// 真的生效的键**一模一样**。写入本身没有错（值确实进库了），缺的是说出
// 「这个键改了不会有任何效果」。
//
// 这里比对两条**完整输出**而不是只查关键字：要的正是「两者不一样」。只断言
// 未接线那条里有某个词的话，一个把同一句话打到所有键上的实现也能过，而那样
// 「标出来了」就不再是一个信号。
//
// 期望值写死字面量，不取 Registry 里的文案：那等于让被测对象给自己出题。
func TestPrintSettingsSetResult(t *testing.T) {
	var unwired bytes.Buffer
	printSettingsSetResult(&unwired, "capture.consent_max_window_hours", []byte("24"), "ops", 7)
	wantUnwired := "已写入 settings.capture.consent_max_window_hours = 24 改自 ops(id=7)\n" +
		"  尚未接线：授权采集那条路还没实现，改了不会有任何效果\n"
	if got := unwired.String(); got != wantUnwired {
		t.Errorf("未接线的键，输出 =\n%q\n期望\n%q", got, wantUnwired)
	}

	// 对照组用一个确实接上了线的键。plugin.enabled 与另外三个 plugin.* 键
	// 是刚接上的那一批：internal/assembly 的装配用例钉着「配错会让装配失败」，
	// 所以它们是真的被读了。
	var wired bytes.Buffer
	printSettingsSetResult(&wired, "plugin.enabled", []byte("true"), "ops", 7)
	wantWired := "已写入 settings.plugin.enabled = true 改自 ops(id=7)\n"
	if got := wired.String(); got != wantWired {
		t.Errorf("已接线的键，输出 =\n%q\n期望\n%q", got, wantWired)
	}

	if unwired.String() == wired.String() {
		t.Error("未接线与已接线的输出一样——那正是 #215 说的那件事")
	}
}

func TestPrintSettingsListEmpty(t *testing.T) {
	var buf bytes.Buffer
	printSettingsList(&buf, nil)
	if got, want := buf.String(), "settings 表是空的。\n"; got != want {
		t.Errorf("空清单 = %q，期望 %q", got, want)
	}
}

// ── 机器可读输出（--format json） ───────────────────────────────────────
//
// 这一档最要紧的是 value：它打的是 settings.value 的 **jsonb 原文**，字符串带
// 引号、布尔与整数不带。表格那一档打的正是原文，JSON 里也要保住这一点——
// 解码成 Go 值再编码回去会把「类型写错」这件事盖住，而它会让服务起不来。

func TestSettingsListDocIsMachineReadable(t *testing.T) {
	by := int64(7)
	at := time.Date(2026, 10, 17, 6, 10, 0, 0, time.UTC)

	doc := settingsListDoc([]settings.Entry{
		{
			Key: "plugin.enabled", Value: []byte(`false`),
			Description: "是否启用插件系统。", Editable: true,
			UpdatedBy: &by, UpdatedAt: at,
		},
		{
			// 还没接线的键：Registry 里那句说明要跟着这一条走，不能只出现在
			// 终端里——脚本问的正是「哪几个键改了不会有任何效果」。
			Key: "capture.consent_max_window_hours", Value: []byte(`24`),
			Description: "授权采集的最长窗口。", Editable: true, UpdatedAt: at,
		},
		{
			Key: "billing.default_asset", Value: []byte(`"CNY"`),
			Description: "新用户钱包的默认资产品种。", Editable: false, UpdatedAt: at,
		},
		{
			// 一个 23 位的整数：jsonb 存得下它，而解码成 float64 再编码会把
			// 后面的位数改掉（1.2345678901234568e+22）。位数原样经过，是这一栏
			// 走原文的理由之一。
			Key: "request_snapshot.max_bytes", Value: []byte(`12345678901234567890123`),
			Description: "快照体积上限。", Editable: true, UpdatedAt: at,
		},
		{
			// Registry 里没有这个键：类型与「尚未接线」都给 null，而它正是
			// 「服务加载不起来」的直接原因。
			Key: "typo.key", Value: []byte(`1`), Editable: true, UpdatedAt: at,
		},
	})

	var got struct {
		Status  string `json:"status"`
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Settings []struct {
			Key   string  `json:"key"`
			Value any     `json:"value"`
			Kind  *string `json:"kind"`
			// 布尔就是 JSON 布尔，不是 "true" 字符串：解进 bool 能成功这件事
			// 本身就是判据。
			Editable  bool    `json:"editable"`
			UpdatedBy *int64  `json:"updated_by"`
			UpdatedAt string  `json:"updated_at"`
			Desc      *string `json:"description"`
			Unwired   *string `json:"unwired"`
		} `json:"settings"`
	}
	raw := decodeDoc(t, doc, &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}
	if got.Summary.Total != 5 || len(got.Settings) != 5 {
		t.Fatalf("summary.total = %d、settings 有 %d 条，都想要 5",
			got.Summary.Total, len(got.Settings))
	}

	// 布尔配置：值就是布尔 false，不是字符串 "false"、也不是空值。
	first := got.Settings[0]
	if first.Key != "plugin.enabled" {
		t.Errorf("第一条是 %q，想要 plugin.enabled", first.Key)
	}
	if v, ok := first.Value.(bool); !ok || v {
		t.Errorf("plugin.enabled 的值 = %#v，想要布尔 false", first.Value)
	}
	if first.Kind == nil || *first.Kind != "bool" {
		t.Errorf("kind = %s，想要 bool", showPtr(first.Kind))
	}
	if !first.Editable {
		t.Error("plugin.enabled 应当是可改的")
	}
	if first.UpdatedBy == nil || *first.UpdatedBy != 7 {
		t.Errorf("updated_by = %s，想要 7", showPtr(first.UpdatedBy))
	}
	if first.UpdatedAt != "2026-10-17T06:10:00Z" {
		t.Errorf("updated_at = %q，想要 2026-10-17T06:10:00Z", first.UpdatedAt)
	}
	if first.Desc == nil || *first.Desc != "是否启用插件系统。" {
		t.Errorf("description = %s，想要那一句原文", showPtr(first.Desc))
	}
	// 它已经接上线了，unwired 必须是 null——不是空串。
	if first.Unwired != nil {
		t.Errorf("已接线的键 unwired = %v，想要 null", *first.Unwired)
	}

	// 整数配置走的是 JSON 数字（原文里就不带引号）。
	if v, ok := got.Settings[1].Value.(float64); !ok || v != 24 {
		t.Errorf("capture.consent_max_window_hours 的值 = %#v，想要 24", got.Settings[1].Value)
	}
	if v := got.Settings[1].Unwired; v == nil {
		t.Error("未接线的键要给那句说明，得到 null")
	} else if want := "授权采集那条路还没实现，改了不会有任何效果"; *v != want {
		t.Errorf("未接线那句话 = %q，想要 %q", *v, want)
	}

	// 字符串配置：解出来是 Go 字符串 "CNY"，而不是带引号的 `"CNY"` 字面量。
	if v, ok := got.Settings[2].Value.(string); !ok || v != "CNY" {
		t.Errorf("billing.default_asset 的值 = %#v，想要字符串 CNY", got.Settings[2].Value)
	}
	if got.Settings[2].Editable {
		t.Error("billing.default_asset 不可改（editable=false）")
	}
	if got.Settings[2].Kind == nil || *got.Settings[2].Kind != "string" {
		t.Errorf("kind = %s，想要 string", showPtr(got.Settings[2].Kind))
	}

	// 大整数按原文经过：位数一个都不能变。
	if !strings.Contains(string(raw), `"value":12345678901234567890123`) {
		t.Errorf("大整数被改写了（是不是借道了 float64）：\n%s", raw)
	}

	// 未声明的键：kind 与 unwired 都是 null，说明也是 null（空说明不是「没有值」
	// 之外的另一种意思，但空串在 JSON 里读起来像一个值）。
	last := got.Settings[4]
	if last.Key != "typo.key" {
		t.Fatalf("最后一条是 %q，想要 typo.key", last.Key)
	}
	if last.Kind != nil || last.Unwired != nil || last.Desc != nil {
		t.Errorf("未声明的键那三栏都该是 null，得到 kind=%s unwired=%s description=%s",
			showPtr(last.Kind), showPtr(last.Unwired), showPtr(last.Desc))
	}
	if last.UpdatedBy != nil {
		t.Errorf("没有记录谁改的 → updated_by 应当是 null，得到 %d", *last.UpdatedBy)
	}

	// 键集逐个钉住。
	var generic map[string]any
	decodeDoc(t, doc, &generic)
	entries, ok := generic["settings"].([]any)
	if !ok || len(entries) != 5 {
		t.Fatalf("settings 不是五条的数组：%v", generic["settings"])
	}
	for i, item := range entries {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("第 %d 条不是对象：%v", i, item)
		}
		assertDocKeys(t, entry, []string{
			"key", "value", "kind", "editable", "updated_by",
			"updated_at", "description", "unwired",
		})
	}
	// 表头那句说明是给终端里的人看的，不进 JSON。
	if strings.Contains(string(raw), "jsonb 原文") {
		t.Errorf("表格那一档的说明混进了 JSON：\n%s", raw)
	}
}

// 空清单是 `[]`：settings 表是空的与「还没有配过」不是一回事（那些键来自
// Registry），所以表格那一档留着它的说法，而这一档给一个空数组。
func TestSettingsListDocEmptyListIsEmptyArray(t *testing.T) {
	var got struct {
		Settings *[]map[string]any `json:"settings"`
	}
	decodeDoc(t, settingsListDoc(nil), &got)

	if got.Settings == nil {
		t.Fatal("空清单编成了 null，要的是 []")
	}
	if len(*got.Settings) != 0 {
		t.Errorf("空清单里有 %d 条", len(*got.Settings))
	}
}

// settings set 会写进 settings 表，在 json 模式下必须在连库之前拦下。
func TestSettingsSetRejectsJSONFormatBeforeTouchingTheDB(t *testing.T) {
	withFormat(t, formatJSON)
	// 连不上的库：真的去连就会以 exitDBUnreach 收场。
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	var err error
	stdout, _ := captureStreams(t, func() {
		err = runSettingsSet([]string{"--key", "plugin.enabled", "--value", "true", "--by", "ops"})
	})
	if err == nil {
		t.Fatal("json 模式下应当把 settings set 拦下来")
	}
	if !strings.Contains(err.Error(), "不支持 --format json") {
		t.Fatalf("错误信息 %q 里没有「不支持 --format json」——是不是先连库了", err.Error())
	}
	if !strings.Contains(err.Error(), "settings set") {
		t.Errorf("错误信息里要点名是哪条命令：%v", err)
	}
	if code := exitCode(err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("被拦下时 stdout 应当为空，得到 %q", stdout)
	}
}

// 命令级：过真命令、连真库，stdout 整体是一份合法 JSON。
//
// 这一条还顺带钉住「Load 那份校验跑过」：迁移写下的默认值都合法，所以
// 退出码是 0、status 是 ok；校验不过时那句话走 stderr，而 stdout 上仍然只有
// 一份 JSON（Unmarshal 成功即是判据）。
func TestSettingsListJSONStdoutIsMachineReadable(t *testing.T) {
	testDB(t)
	setupEnv(t)

	withFormat(t, formatJSON)
	var err error
	stdout, stderr := captureStreams(t, func() { err = runSettingsList(nil) })
	if err != nil {
		t.Fatalf("settings list 失败: %v（stderr: %s）", err, stderr)
	}
	assertOnlyJSON(t, stdout)

	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Summary  struct {
			Total int `json:"total"`
		} `json:"summary"`
		Settings []struct {
			Key       string  `json:"key"`
			Value     any     `json:"value"`
			Kind      *string `json:"kind"`
			UpdatedAt string  `json:"updated_at"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, stdout)
	}
	if got.Status != jsonOK || got.ExitCode != exitOK {
		t.Errorf("status=%q exit_code=%d，想要 %q / %d",
			got.Status, got.ExitCode, jsonOK, exitOK)
	}
	// 迁移把 Registry 里的键都写进去了，所以清单必然非空：一条都没有时
	// 这一条会红，而「settings 表是空的」正是要能与「命令没跑成」分开的那件事。
	if got.Summary.Total == 0 || len(got.Settings) != got.Summary.Total {
		t.Fatalf("summary.total = %d、清单有 %d 条：迁移写下的默认值应当都在",
			got.Summary.Total, len(got.Settings))
	}
	for _, e := range got.Settings {
		if e.Key == "" {
			t.Errorf("有一条没有 key：%+v", e)
		}
		if e.Kind == nil {
			t.Errorf("%s 的 kind 是 null：它声明在 Registry 里，不该是未声明的键", e.Key)
		}
		if _, err := time.Parse(time.RFC3339, e.UpdatedAt); err != nil {
			t.Errorf("%s 的 updated_at %q 不是 RFC3339: %v", e.Key, e.UpdatedAt, err)
		}
	}
}

// json 模式下，Load 那份校验照旧跑：警告走 stderr，结论由 exit_code 说出来。
//
// 这一条钉住的是「配置有问题」这件事在机器可读输出里的落点。它有三个不能错的
// 地方：stdout 上仍然只有一份 JSON（警告混进去会让 jq 报错）、那句话确实出现在
// stderr、以及那份**清单照旧列出来**——配置有问题的时候恰恰是最需要看到内容的
// 时候，一个「校验不过就什么都不列」的清单等于在最需要它的时候闭嘴。
func TestSettingsListJSONKeepsValidationWarningOffStdout(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	// 往库里塞一个 Registry 不认识的键：服务启动时那份校验会因此失败
	// （见 settings.Load）。键名带一个只有这条用例会用的后缀，用完删掉。
	const typoKey = "typo.json_stdout_test"
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO settings (key, value, description) VALUES ($1, '1'::jsonb, '用例塞的')`,
		typoKey); err != nil {
		t.Fatalf("塞一行未声明的键失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM settings WHERE key = $1`, typoKey); err != nil {
			t.Fatalf("清理用例塞的那一行失败: %v", err)
		}
	})

	withFormat(t, formatJSON)
	var err error
	stdout, stderr := captureStreams(t, func() { err = runSettingsList(nil) })
	if err == nil {
		t.Fatal("配置加载不起来时命令应当以非零退出码收尾")
	}
	if code := exitCode(err); code != exitFailure {
		t.Errorf("退出码 = %d，想要 %d（其他失败）", code, exitFailure)
	}
	assertOnlyJSON(t, stdout)

	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Settings []struct {
			Key  string  `json:"key"`
			Kind *string `json:"kind"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, stdout)
	}
	// 结论由 exit_code 说出来；status 说的是「这份报告读出什么」，两者各说各的
	// （见 format.go 与 docs/cli-output.md）。
	if got.ExitCode != exitFailure {
		t.Errorf("报告里的 exit_code = %d，想要 %d", got.ExitCode, exitFailure)
	}
	if !strings.Contains(stderr, "警告：") {
		t.Errorf("校验不过那句话应当在 stderr 上：%q", stderr)
	}
	if strings.Contains(stdout, "警告") {
		t.Errorf("警告混进了 stdout：\n%s", stdout)
	}

	// 那一行自己要在清单里，且 kind 是 null——它是「服务加载不起来」的直接
	// 原因，脚本按这一栏就能把它筛出来。
	var found bool
	for _, e := range got.Settings {
		if e.Key != typoKey {
			continue
		}
		found = true
		if e.Kind != nil {
			t.Errorf("未声明的键 kind = %q，想要 null", *e.Kind)
		}
	}
	if !found {
		t.Fatalf("清单里没有那一行未声明的键：\n%s", stdout)
	}
}

// TestSettingsUpdatedAtIsUTCWhateverTheProcessZone 钉住「时刻一律按 UTC 打」。
//
// 理由与 apikey 的 TestAPIKeyTimesAreUTCWhateverTheProcessZone 相同：这一列
// 从库（timestamptz）读回来时带的是会话时区，直接 Format 出来同一行在两台机器
// 上是两个字符串。fixture 特地在东八区、断言那个瞬间的 UTC 写法，所以把
// .UTC() 去掉（或换回按 location 打的写法）时这条用例当场变红。
func TestSettingsUpdatedAtIsUTCWhateverTheProcessZone(t *testing.T) {
	east8 := time.FixedZone("UTC+8", 8*60*60)
	at := time.Date(2026, 10, 17, 14, 10, 0, 0, east8) // = 06:10:00Z

	var got struct {
		Settings []struct {
			UpdatedAt string `json:"updated_at"`
		} `json:"settings"`
	}
	decodeDoc(t, settingsListDoc([]settings.Entry{{
		Key: "plugin.enabled", Value: []byte(`false`), UpdatedAt: at,
	}}), &got)

	if len(got.Settings) != 1 {
		t.Fatalf("settings 有 %d 条，想要 1", len(got.Settings))
	}
	if want := "2026-10-17T06:10:00Z"; got.Settings[0].UpdatedAt != want {
		t.Errorf("updated_at = %q，想要 %q（同一瞬间的 UTC 写法，与进程时区无关）",
			got.Settings[0].UpdatedAt, want)
	}
}
