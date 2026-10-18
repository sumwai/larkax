package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/snapshot"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// cleanupAt 是清理类用例的固定时刻。
//
// 到期线由「留存期 + 当前时刻」算出来，时间不固定的话断言里那个日期就会随
// 运行日漂移——而漂移的断言迟早会被人改成「只要含 0 个就行」。
var cleanupAt = time.Date(2026, 10, 16, 15, 30, 0, 0, time.UTC)

// 一个都没删时也要有明确输出，而且那行输出要带上判据。
//
// 「没有到期的分区」与「命令没跑成」在安静的终端里长得一样（LESSONS.md 第 16 条），
// 所以这里断言的是**有一行输出**，而不只是「没报错」。
//
// 但「没有到期分区」这句话本身还不够：它与「根本没查」也长得一样，而这条命令
// 曾经正是「说自己在清理、实际一步都没走，输出的那行字却完全正常」。所以留存期
// 与到期线这两个判据必须一起打出来——「查过了、确实没有」才是可以核对的。
func TestPrintPartitionsEmptyIsExplicit(t *testing.T) {
	out := captureStdout(t, func() {
		printPartitions("已删除", nil, 30, cleanupAt)
	})
	if !strings.Contains(out, "0 个") {
		t.Errorf("空清单的输出应说明是 0 个，实际为 %q", out)
	}
	if !strings.Contains(out, "留存期 30 天") {
		t.Errorf("空清单的输出应带上这次用的留存期，实际为 %q", out)
	}
	if !strings.Contains(out, "2026-09-16") {
		t.Errorf("空清单的输出应带上到期线（留存 30 天、当天 10-16 → 09-16），实际为 %q", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("空清单不该什么都不输出")
	}
}

func TestPrintPartitionsListsEveryName(t *testing.T) {
	names := []string{"request_snapshots_2026_09_01", "request_snapshots_2026_09_02"}
	out := captureStdout(t, func() {
		printPartitions("将删除", names, 30, cleanupAt)
	})
	for _, n := range names {
		if !strings.Contains(out, n) {
			t.Errorf("输出里应当列出 %s，实际为 %q", n, out)
		}
	}
	if !strings.Contains(out, "2") {
		t.Errorf("输出里应当给出条数，实际为 %q", out)
	}
}

// 采集关闭时的提示语必须与行为一致：它说「仍按留存期清理」，而清理真的会跑
// （见 snapshotwire 的 TestCleanupRunsWhenCollectingDisabled）。
//
// 采集开着时一个字都不打：一条常驻的提示会让人不再读它，而那时它也没说什么。
func TestPrintCleanupHintOnlyWhenCollectingDisabled(t *testing.T) {
	var on, off bytes.Buffer
	printCleanupHint(&on, snapshotwire.Config{Enabled: true, RetentionDays: 30})
	if on.Len() != 0 {
		t.Errorf("采集开着时不该打提示，实际为 %q", on.String())
	}

	printCleanupHint(&off, snapshotwire.Config{Enabled: false, RetentionDays: 7})
	if !strings.Contains(off.String(), "采集已关闭") {
		t.Errorf("采集关着时应说明这一点，实际为 %q", off.String())
	}
	if !strings.Contains(off.String(), "7 天") {
		t.Errorf("提示里应带上留存期，实际为 %q", off.String())
	}
}

func TestSnapshotUnknownSubcommand(t *testing.T) {
	err := runSnapshot(nil)
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runSnapshot([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 snapshot 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestSnapshotRootsFor 钉住方向解析。
//
// 取值从 snapshot.Roots() 里找，不在这里另列一遍：允许表里加了新方向时
// 命令行要跟着能用，而手抄一份的表现是「表里有的方向，命令行说它不认识」。
func TestSnapshotRootsFor(t *testing.T) {
	all, err := snapshotRootsFor("")
	if err != nil {
		t.Fatalf("空方向应当表示全部，却报错: %v", err)
	}
	if len(all) != len(snapshot.Roots()) {
		t.Fatalf("空方向给了 %d 个，期望全部 %d 个", len(all), len(snapshot.Roots()))
	}

	one, err := snapshotRootsFor(string(snapshot.RootRequest))
	if err != nil {
		t.Fatalf("指名方向失败: %v", err)
	}
	if len(one) != 1 || one[0] != snapshot.RootRequest {
		t.Fatalf("指名 request 得到 %v", one)
	}

	if _, err := snapshotRootsFor("both"); err == nil {
		t.Fatal("不认识的方向应当报错")
	}
}

// TestPrintAllowlistsCarriesEveryNote 是 allowlist 那条命令的判据。
//
// Note 是这条命令存在的理由：它写着「这一行为什么放行、为什么脱敏」，
// 而在它之前使用者只能去读源码。所以每一条都必须**完整**打出来——
// 截断过的理由与没有理由在用途上差不多，而两者看起来都像「打出来了」。
func TestPrintAllowlistsCarriesEveryNote(t *testing.T) {
	roots := snapshot.Roots()
	if len(roots) < 2 {
		t.Fatalf("方向应当至少有两个，得到 %d 个", len(roots))
	}
	surfaces := snapshot.Surfaces()
	if len(surfaces) < 2 {
		t.Fatalf("表面应当至少有两个，得到 %d 个", len(surfaces))
	}

	var buf bytes.Buffer
	printAllowlists(&buf, surfaces, roots)
	out := buf.String()

	for _, s := range surfaces {
		for _, root := range roots {
			if !strings.Contains(out, "表面 "+string(s)+" 方向 "+string(root)+"：") {
				t.Errorf("输出里没有表面 %s 方向 %s 的分段标题", s, root)
			}
			rules := snapshot.AllowlistFor(s, root)
			if len(rules) == 0 {
				t.Fatalf("表面 %s 方向 %s 的允许表是空的，这条命令就没有内容可打", s, root)
			}
			for _, rule := range rules {
				if strings.TrimSpace(rule.Note) == "" {
					t.Errorf("表面 %s 方向 %s 的 %s 没有 Note，用例需要更新", s, root, rule.Path)
					continue
				}
				// 按整行比对而不是子串：一条 Note 是另一条的前缀时，
				// 子串匹配分辨不出「后一条被截断了」。
				if !strings.Contains(out, "    "+rule.Path+"："+rule.Note+"\n") {
					t.Errorf("表面 %s 方向 %s 的 %s 的 Note 没有完整打出来", s, root, rule.Path)
				}
			}
		}
	}

	// 列名一起打出来，输出才是自解释的——只有值没有名字的清单，
	// 读的人得先回去看代码才知道哪一列是什么。
	for _, want := range []string{"路径", "类型", "上游值"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有列名 %q", want)
		}
	}
}

// TestPrintAllowlistsSingleDirection 确认 --direction 只打一段。
func TestPrintAllowlistsSingleDirection(t *testing.T) {
	var buf bytes.Buffer
	printAllowlists(&buf, []snapshot.Surface{snapshot.SurfaceChat}, []snapshot.Root{snapshot.RootResponse})
	out := buf.String()

	if !strings.Contains(out, "表面 chat 方向 response：") {
		t.Errorf("指定 response 时应当只打这一段：\n%s", out)
	}
	if strings.Contains(out, "方向 request：") {
		t.Errorf("指定 response 时不该把 request 那一段也打出来")
	}
}

// TestPrintAllowlistsSingleSurface 确认 --surface 只打一张表。
//
// 这一条要钉的是「两条表面不会互相顶掉」：分段标题里带表面名，所以
// 「指定决策却打了两张」与「指定 chat 却打了决策那张」都会红。
func TestPrintAllowlistsSingleSurface(t *testing.T) {
	var buf bytes.Buffer
	printAllowlists(&buf, []snapshot.Surface{snapshot.SurfaceDecision},
		[]snapshot.Root{snapshot.RootRequest})
	out := buf.String()

	if !strings.Contains(out, "表面 decision 方向 request：") {
		t.Errorf("指定 decision 时应当只打这一段：\n%s", out)
	}
	if strings.Contains(out, "表面 chat") {
		t.Errorf("指定 decision 时不该把 chat 那张表也打出来")
	}
	if strings.Contains(out, "方向 response：") {
		t.Errorf("指定 request 时不该把 response 那一段也打出来")
	}
}

// TestSurfaceForRejectsUnknownName 确认拼错的表面名会被拦下。
//
// 回落成 chat 的后果是「一条按决策表写的字段静静按对话表处理」——
// 命令照常退出 0，打出来的东西看着也正常。
func TestSurfaceForRejectsUnknownName(t *testing.T) {
	if _, err := surfaceFor("decisions", snapshot.SurfaceChat); err == nil {
		t.Fatal("拼错的表面名应当报错，而不是回落到默认值")
	}
	if got, err := surfaceFor("decision", snapshot.SurfaceChat); err != nil || got != snapshot.SurfaceDecision {
		t.Fatalf("指名 decision 得到 %q / %v", got, err)
	}
	// 空串表示默认值：redact 不写 --surface 时走 chat。
	if got, err := surfaceFor("", snapshot.SurfaceChat); err != nil || got != snapshot.SurfaceChat {
		t.Fatalf("空串应当回落到传入的默认表面，得到 %q / %v", got, err)
	}
}

// TestPrintSnapshotsPrintsEveryRow 钉住渲染层的两件事：
// 同一个 request_id 的多行都要打出来，且按传入顺序（= created_at 升序）。
//
// 只取一行会让「这次请求还有另一半」看不见。
func TestPrintSnapshotsPrintsEveryRow(t *testing.T) {
	first := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	rows := []snapshotRow{
		{
			ID: 1, Direction: "request", SkeletonVersion: 1, CaptureMode: "skeleton",
			Skeleton:      map[string]any{"model": "probe-model"},
			RedactedPaths: []string{"messages[0].content[0].text"},
			CreatedAt:     first,
		},
		{
			ID: 2, Direction: "response", SkeletonVersion: 1, CaptureMode: "skeleton",
			Skeleton:       map[string]any{"upstream_status": json.Number("500")},
			UpstreamStatus: sql.NullInt64{Int64: 500, Valid: true},
			ErrorKind:      sql.NullString{String: "upstream_error", Valid: true},
			CreatedAt:      first.Add(time.Millisecond),
		},
	}

	var buf, note bytes.Buffer
	printSnapshots(&buf, &note, "req-probe", rows)
	out := buf.String()

	if !strings.Contains(out, "2 条快照") {
		t.Errorf("应当报出条数：\n%s", out)
	}
	for _, want := range []string{
		"direction=request", "direction=response",
		"created_at=2026-10-17T01:02:03Z", "created_at=2026-10-17T01:02:03.001Z",
		"upstream_status=500", "error_kind=upstream_error",
		"messages[0].content[0].text",
		"skeleton_truncated=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	if strings.Index(out, "direction=request") > strings.Index(out, "direction=response") {
		t.Errorf("两条快照应当按 created_at 升序打：\n%s", out)
	}

	// 请求方向的行上那两列**不适用**：这一路收不到状态码与分类（它记的是
	// 发出去的那份请求）。打成 <未记录> 会被读成「该有值却漏记了」。
	if !strings.Contains(out, "upstream_status=<不适用（请求方向快照）>") {
		t.Errorf("请求方向的 NULL 状态码应标成不适用：\n%s", out)
	}
	if !strings.Contains(out, "error_kind=<不适用（请求方向快照）>") {
		t.Errorf("请求方向的 NULL 错误分类应标成不适用：\n%s", out)
	}
	// 一条都没被替换时也要有一行：空清单与「这一步没跑」长得一样。
	if !strings.Contains(out, "redacted_paths：0 条") {
		t.Errorf("空的 redacted_paths 应当显式写出来：\n%s", out)
	}
}

// TestPrintSnapshotResponseDirectionExplainsNulls 钉住另一半：响应方向的行上，
// NULL 是一个有内容的值，两列的含义不同。
//
//   - upstream_status 为 NULL = 一个 HTTP 响应都没拿到（连不上、超时）；
//   - error_kind 为 NULL = 没有上游失败可归（正常结束、客户端断开、插件阻断）。
//
// 打成同一个文本会把两件事混成一件事，而「没拿到响应」与「没有失败」
// 在排查时指向完全不同的方向。
func TestPrintSnapshotResponseDirectionExplainsNulls(t *testing.T) {
	rows := []snapshotRow{
		{
			ID: 1, Direction: "response", SkeletonVersion: 1, CaptureMode: "skeleton",
			Skeleton:  map[string]any{"model": "probe-model"},
			CreatedAt: time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC),
		},
	}

	var buf, note bytes.Buffer
	printSnapshots(&buf, &note, "req-probe", rows)
	out := buf.String()

	for _, want := range []string{
		"upstream_status=<无上游响应>",
		"error_kind=<无>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("响应方向的 NULL 应按列解释，输出里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "未记录") {
		t.Errorf("响应方向的 NULL 不该再打「未记录」——它读作漏记，而这两种 NULL 都是有内容的值：\n%s", out)
	}
	if strings.Contains(out, "不适用") {
		t.Errorf("响应方向的行不该打「不适用」：\n%s", out)
	}
}

// TestPrintSnapshotRequestDirectionShowsUnexpectedValue 钉住那两个渲染函数的
// 「只改写 NULL」：请求方向的行上出现非 NULL 值，说明有写入路径绕过了
// snapshotwire 的门，那个异常必须原样显示出来，不能被「不适用」藏掉。
func TestPrintSnapshotRequestDirectionShowsUnexpectedValue(t *testing.T) {
	rows := []snapshotRow{
		{
			ID: 1, Direction: "request", SkeletonVersion: 1, CaptureMode: "skeleton",
			Skeleton:       map[string]any{"model": "probe-model"},
			UpstreamStatus: sql.NullInt64{Int64: 502, Valid: true},
			ErrorKind:      sql.NullString{String: "upstream_error", Valid: true},
			CreatedAt:      time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC),
		},
	}

	var buf, note bytes.Buffer
	printSnapshots(&buf, &note, "req-probe", rows)
	out := buf.String()

	if !strings.Contains(out, "upstream_status=502") ||
		!strings.Contains(out, "error_kind=upstream_error") {
		t.Errorf("请求方向上的非 NULL 值应当原样显示出来：\n%s", out)
	}
	if strings.Contains(out, "不适用") {
		t.Errorf("有值的时候不该把它说成「不适用」：\n%s", out)
	}
}

// TestPrintSnapshotMarksTruncation 是判据「skeleton_truncated 为真时输出里有说明」。
//
// 一份被降级成摘要层的骨架与一份完整的骨架在肉眼下长得一样，而前者会让人
// 以为「上游就是只发了这些」。所以这一行必须出现，且只在真的截断时出现——
// 每次都打等于每次都读不到。
//
// 它走**附注那一档**（stderr）：它是「这一行该怎么读」的说明，不是快照本身，
// 而 stdout 上那份要能接给 jq。所以这里对两个流分别断言。
func TestPrintSnapshotMarksTruncation(t *testing.T) {
	rows := []snapshotRow{
		{ID: 1, Direction: "request", Skeleton: map[string]any{"model": "probe-model"}},
		{ID: 2, Direction: "response", Truncated: true,
			Skeleton: map[string]any{"_truncated": true}},
	}

	var buf, note bytes.Buffer
	printSnapshots(&buf, &note, "req-probe", rows)
	out := buf.String()

	if got := strings.Count(note.String(), "降级成摘要层"); got != 1 {
		t.Errorf("说明应当只出现在被截断的那一条上，实际出现 %d 次：\n%s", got, note.String())
	}
	if strings.Contains(out, "降级成摘要层") {
		t.Errorf("说明不该混进 stdout——那一份是快照本身：\n%s", out)
	}
	if !strings.Contains(out, "skeleton_truncated=true") {
		t.Errorf("截断标志本身也要打出来：\n%s", out)
	}
}

// TestPrintSnapshotSkeletonIsStable 钉住「同一个库两次跑出来逐行相同」。
//
// 骨架是 map，直接 fmt 打印的键序是随机的；走 json.Marshal 才有稳定输出，
// 而稳定是「比对两次输出」这件事成立的前提。
func TestPrintSnapshotSkeletonIsStable(t *testing.T) {
	row := snapshotRow{
		ID: 1, Direction: "request", SkeletonVersion: 1, CaptureMode: "skeleton",
		Skeleton: map[string]any{
			"model": "probe-model", "stream": true, "max_tokens": json.Number("1024"),
			"temperature": json.Number("0.7"),
		},
		CreatedAt: time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC),
	}

	var first, second, firstNote, secondNote bytes.Buffer
	printSnapshots(&first, &firstNote, "req-probe", []snapshotRow{row})
	printSnapshots(&second, &secondNote, "req-probe", []snapshotRow{row})
	if first.String() != second.String() {
		t.Errorf("同一个骨架两次渲染不一样：\n%s\n---\n%s", first.String(), second.String())
	}

	// 字面量原样保留：库里存的是 0.7 还是 0.70，打出来就该是哪一个。
	if !strings.Contains(first.String(), `"temperature": 0.7`) {
		t.Errorf("数字的字面量没有原样保留：\n%s", first.String())
	}
}

// TestDecodeSkeletonKeepsBigIntegers 钉住 json.Number。
//
// 走 json.Unmarshal 的话大整数会被静默转成浮点，而**错的那一份会被当成
// 「当时存的就是这个」**——读快照的人没有第二个来源可以对照。
func TestDecodeSkeletonKeepsBigIntegers(t *testing.T) {
	got, err := decodeSkeleton([]byte(`{"seed":9007199254740993,"ratio":1.0}`))
	if err != nil {
		t.Fatalf("解析骨架失败: %v", err)
	}
	if n, ok := got["seed"].(json.Number); !ok || n.String() != "9007199254740993" {
		t.Errorf("大整数没有被原样接住: %#v", got["seed"])
	}
	if n, ok := got["ratio"].(json.Number); !ok || n.String() != "1.0" {
		t.Errorf("小数点的写法没有被保留: %#v", got["ratio"])
	}
}

// JSON null 存得进 jsonb 列（列上是 NOT NULL），但读出来是一张空骨架，
// 与「这次请求什么都没发」分不开——所以不当成正常值。
func TestDecodeSkeletonRejectsNull(t *testing.T) {
	if _, err := decodeSkeleton([]byte("null")); err == nil {
		t.Fatal("JSON null 不该被当成一张空骨架")
	}
}

// 参数错在**连库之前**就报出来，所以这些用例不需要库。
//
// 一个写错的命令不该先连上生产库再报错——而「连上库之后才发现参数不对」
// 还有一个更坏的可能：它没报错，而是照着错的参数读了一条别人的请求出来。
func TestSnapshotGetRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --reason", []string{"req-1", "--actor", "ops"}, "缺少 --reason"},
		{"没有 --actor", []string{"req-1", "--reason", "看看"}, "缺少 --actor"},
		{"--reason 只有空白", []string{"req-1", "--actor", "ops", "--reason", "   "}, "缺少 --reason"},
		{"--actor 只有空白", []string{"req-1", "--actor", "  ", "--reason", "看看"}, "缺少 --actor"},
		{"没有 request_id", []string{"--actor", "ops", "--reason", "看看"}, "用法"},
		{"多余的位置参数",
			[]string{"req-1", "extra", "--actor", "ops", "--reason", "看看"}, "只接受一个位置参数"},
		{"方向不认识",
			[]string{"req-1", "--actor", "ops", "--reason", "看看", "--direction", "both"}, "不认识"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runSnapshotGet(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// TestSnapshotGetParsesFlagsAfterRequestID 钉住参数顺序。
//
// Go 的 flag 包遇到第一个非旗标参数就停止解析，而这条命令的形状是
// `show <request_id> --actor X --reason Y`——request_id 在旗标前面。
// 只用一次 Parse 的话，--actor 会被当成第二个位置参数，报出来的错是
// 「只接受一个位置参数」，而那句话把人引向参数个数，问题却在参数顺序。
//
// 判据是**报出来的错是哪一个**：缺 --reason 说明 --actor 被认出来了。
// 这条断言在两种实现下取值不同（一次 Parse 给「位置参数」，
// 分两趟给「缺少 --reason」），所以它分辨得出走的是哪条路。
func TestSnapshotGetParsesFlagsAfterRequestID(t *testing.T) {
	err := runSnapshotGet([]string{"req-1", "--actor", "ops"})
	if err == nil {
		t.Fatal("缺 --reason 应当报错")
	}
	if !strings.Contains(err.Error(), "缺少 --reason") {
		t.Fatalf("request_id 之后的旗标没有被解析: %v", err)
	}
}

// 旗标写在 request_id 前面也要能用——两种写法都自然。
func TestSnapshotGetAcceptsFlagsBeforeRequestID(t *testing.T) {
	err := runSnapshotGet([]string{"--actor", "ops", "--reason", "看看", "--direction", "request"})
	if err == nil {
		t.Fatal("缺 request_id 应当报错")
	}
	if !strings.Contains(err.Error(), "用法") {
		t.Fatalf("旗标在前时应当报「缺 request_id」，得到 %v", err)
	}
}

func TestSnapshotRedactRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --direction", nil, "缺少 --direction"},
		{"方向不认识", []string{"--direction", "both"}, "不认识"},
		{"上限是 0", []string{"--direction", "request", "--max-bytes", "0"}, "必须为正数"},
		{"上限是负数", []string{"--direction", "request", "--max-bytes", "-1"}, "必须为正数"},
		{"位置参数", []string{"--direction", "request", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runSnapshotRedact(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// TestRunSnapshotRedactPrintsSkeleton 是 redact 那条命令的判据。
//
// 三件事一起钉：用户内容不落进骨架、stdout 只有骨架本身（能直接接给 jq）、
// 以及方向、字节上限这些条件与条件的后果都在 stderr 上。
//
// 「这不是库里那条快照」那句只在 -v 下打（见 printRedaction）：它在每一次运行里
// 都会出现，而每次都出现的那句解释读第二遍就没有信息了。所以它在这条用例里
// 按两档判——默认那一档**不许有**，-v 那一档必须有。
func TestRunSnapshotRedactPrintsSkeleton(t *testing.T) {
	const secret = "这段提示词不该出现在骨架里"
	body := fmt.Sprintf(`{"model":"probe-model","stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":%q}]}]}`, secret)
	withOSStdin(t, body)

	var runErr error
	out, info := captureStreams(t, func() {
		runErr = runSnapshotRedact([]string{"--direction", "request"})
	})
	if runErr != nil {
		t.Fatalf("redact 失败: %v", runErr)
	}

	if strings.Contains(out, secret) {
		t.Errorf("用户内容不该出现在骨架里：\n%s", out)
	}
	// stdout 是**只有**骨架的：能直接接给 jq 是这条命令的形状的一部分。
	var skeleton map[string]any
	if err := json.Unmarshal([]byte(out), &skeleton); err != nil {
		t.Fatalf("stdout 不是一段合法的 JSON（%v）：\n%s", err, out)
	}
	if skeleton["model"] != "probe-model" {
		t.Errorf("骨架里的模型名不对: %#v", skeleton["model"])
	}

	for _, want := range []string{
		"方向=request", "字节上限=", "被截断=false",
		"redacted_paths：", "messages[0].content[0].text",
	} {
		if !strings.Contains(info, want) {
			t.Errorf("stderr 里没有 %q：\n%s", want, info)
		}
	}
	// 默认那一档不打那句解释。
	if strings.Contains(info, "不是库里那条快照") {
		t.Errorf("那句解释只在 -v 下打，默认输出里不该有：\n%s", info)
	}

	// -v 那一档：这个产物与库里那条快照字段集合不同，看起来像同一个东西
	// 正是它会误导人的地方，所以要问的时候答得出来。
	withOSStdin(t, body)
	var verboseErr error
	withVerbose(t, true)
	_, verboseInfo := captureStreams(t, func() {
		verboseErr = runSnapshotRedact([]string{"--direction", "request"})
	})
	if verboseErr != nil {
		t.Fatalf("redact -v 失败: %v", verboseErr)
	}
	if !strings.Contains(verboseInfo, "不是库里那条快照") {
		t.Errorf("-v 下要说明这一条不是库里那条快照：\n%s", verboseInfo)
	}
}

// 骨架超限时降级成摘要层，而且必须说出来。
//
// 字节上限是「这一行 truncated 说的是什么」的条件，所以它也要一起打出来：
// 线上那个值来自 settings，读的人得知道这次用的是哪个数。
func TestRunSnapshotRedactReportsTruncation(t *testing.T) {
	// 消息条数决定骨架大小：内容本身会被换成「类型 + 长度」描述符，
	// 加长一条消息不影响骨架有多大，加条数才会。
	msgs := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, `{"role":"user","content":"m"}`)
	}
	withOSStdin(t, `{"model":"probe-model","messages":[`+strings.Join(msgs, ",")+`]}`)

	var runErr error
	out, info := captureStreams(t, func() {
		runErr = runSnapshotRedact([]string{"--direction", "request", "--max-bytes", "200"})
	})
	if runErr != nil {
		t.Fatalf("redact 失败: %v", runErr)
	}

	if !strings.Contains(info, "被截断=true") {
		t.Errorf("stderr 里应当报出被截断：\n%s", info)
	}
	if !strings.Contains(info, "字节上限=200") {
		t.Errorf("stderr 里应当报出这次用的上限：\n%s", info)
	}
	if !strings.Contains(info, "降级成摘要层") {
		t.Errorf("截断必须说明，实际 stderr：\n%s", info)
	}
	if !strings.Contains(out, `"_truncated": true`) {
		t.Errorf("摘要层应当在骨架里留下标记：\n%s", out)
	}
}

// 一段不是 JSON 对象的报文要在连库之前就被拒掉——这条命令不碰数据库，
// 所以「解析失败」与「脱敏失败」是它仅有的两种失败。
func TestRunSnapshotRedactRejectsBadPayload(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空输入", ""},
		{"只有空白", "   \n"},
		{"不是 JSON", "这不是 JSON"},
		{"是数组不是对象", "[1,2,3]"},
		{"对象后面还跟着别的东西", `{"model":"probe-model"}{"model":"another"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withOSStdin(t, tc.body)
			var runErr error
			_, _ = captureStreams(t, func() {
				runErr = runSnapshotRedact([]string{"--direction", "request"})
			})
			if runErr == nil {
				t.Fatal("坏报文应当报错")
			}
			// 不是参数错：报文是从标准输入来的，命令行本身没写错。
			if code := exitCode(runErr); code == exitUsage {
				t.Errorf("报文问题不该报成参数错: %v", runErr)
			}
		})
	}
}

// 上限的边界要精确：恰好等于上限通过，多一个字节就拒。
//
// 用一个小的 limit 把 `>` 与 `>=` 的区别钉死。「很大的输入会被拒」这种断言
// 钉不住它——上限被改成 1 字节之后那句话照样成立。上限做成参数就是为了这个：
// 不必真造 64 MiB 才能测边界。
func TestReadBoundedInputBoundary(t *testing.T) {
	const limit = 8

	got, err := readBoundedInput(strings.NewReader(strings.Repeat("a", limit)), limit)
	if err != nil {
		t.Fatalf("恰好 %d 字节应当通过，实际报错: %v", limit, err)
	}
	if len(got) != limit {
		t.Errorf("读到的长度 = %d，期望 %d", len(got), limit)
	}

	_, err = readBoundedInput(strings.NewReader(strings.Repeat("a", limit+1)), limit)
	if err == nil {
		t.Fatal("超过上限的输入应当报错")
	}
	// 报错要说清上限是多少，以及这条命令是拿什么来试的：只说「太大了」的话，
	// 读的人既不知道能喂多大，也不知道该把日志拆开。
	for _, want := range []string{"超过", strconv.Itoa(limit), "上限", "单次请求"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 里没有 %q", err.Error(), want)
		}
	}
}

// 帮助文本里那个上限与常量必须是同一个数。
//
// snapshotUsage 是原始字符串，插不进值，所以那个「64 MiB」是手写的——改常量时
// 最容易漏掉的就是它，而漏掉的表现是帮助文本安静地说着一个过时的数。
func TestSnapshotUsageStatesTheInputLimit(t *testing.T) {
	want := fmt.Sprintf("%d MiB", snapshotRedactMaxInput>>20)
	if !strings.Contains(snapshotUsage, want) {
		t.Errorf("帮助文本里没有 %q：改上限时帮助文本也要跟着改", want)
	}
}

// 喂一段超过上限的输入：非零退出，错误里带上限，而且不往 stdout 打半个骨架。
//
// 输入用稀疏文件造——Truncate 只改元数据，读出来是 64 MiB 的 0，既不占盘也不
// 在测试进程里多留一份。不能走 withOSStdin：它先把内容整个写进管道再换掉
// os.Stdin，而管道缓冲只有 64 KiB，写 64 MiB 会当场死锁（读端要等它写完才开始读）。
func TestRunSnapshotRedactRejectsOversizedInput(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "oversized-*.json")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	if truncErr := f.Truncate(snapshotRedactMaxInput + 1); truncErr != nil {
		t.Fatalf("扩展临时文件失败: %v", truncErr)
	}
	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		t.Fatalf("回到文件开头失败: %v", seekErr)
	}

	saved := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = saved })

	var runErr error
	out, _ := captureStreams(t, func() {
		runErr = runSnapshotRedact([]string{"--direction", "request"})
	})
	if runErr == nil {
		t.Fatal("超过上限的输入应当报错")
	}
	if !strings.Contains(runErr.Error(), strconv.FormatInt(snapshotRedactMaxInput, 10)) {
		t.Errorf("错误里要带上限 %d：%v", snapshotRedactMaxInput, runErr)
	}
	// 退出码是 1，不是 2：命令行本身没写错，问题出在喂进来的东西上——与上面
	// 「坏报文」那条同一个理由（main.go 把 2 留给命令行参数）。
	if code := exitCode(runErr); code != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", code, exitFailure)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("超限在解析之前就拦下，stdout 不该有内容：\n%s", out)
	}
}

// ── 下面几条要真库 ──────────────────────────────────────────────────────

// seedSnapshotUser 造一个用户，返回 (id, username)。
//
// snapshot_access_log.actor_user_id 有外键指向 users，所以不能只写一张表。
// 用裸 SQL 而不是走 runUserCreate：这条用例要的是「有一个能解析出来的 actor」，
// 不是「user create 能不能跑」（它有自己的用例）。
func seedSnapshotUser(t *testing.T, db *sql.DB) (int64, string) {
	t.Helper()
	username := uniqueName("snapshot-actor")
	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username) VALUES ($1) RETURNING id`,
		username).Scan(&id); err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	return id, username
}

// ensureSnapshotPartition 确保某一天的分区存在。
//
// 迁移只铺了种子分区（部署时那几天），而用例写的是「现在」这一行——
// 换一天跑就会落进兜底分区。这里按 snapshotwire 自己的 PartitionName 与
// DayBounds 建一次：写入方补建分区时用的就是那两个函数，用例跟着它们走，
// 分区名与边界就不会与生产错开。
func ensureSnapshotPartition(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	from, to := snapshotwire.DayBounds(at)
	name := snapshotwire.PartitionName(at)
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF request_snapshots FOR VALUES FROM ('%s') TO ('%s')`,
		name, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if _, err := db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("补建分区 %s 失败: %v", name, err)
	}
}

// probeSnapshot 描述要造的一行快照。
type probeSnapshot struct {
	requestID string
	userID    int64
	direction string
	skeleton  string
	truncated bool
	status    *int
	errorKind *string
	redacted  []string
	createdAt time.Time
}

// insertSnapshot 造一行快照，返回它的 id。
func insertSnapshot(t *testing.T, db *sql.DB, p probeSnapshot) int64 {
	t.Helper()
	at := p.createdAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	ensureSnapshotPartition(t, db, at)

	paths := p.redacted
	if paths == nil {
		// text[] 列不喜欢 nil：写成 NULL 的话后面 `redacted_paths @> ...`
		// 这类查询会静默漏掉这些行。
		paths = []string{}
	}

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO request_snapshots
			(request_id, user_id, direction, skeleton, skeleton_truncated,
			 upstream_status, error_kind, redacted_paths, created_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9)
		RETURNING id`,
		p.requestID, p.userID, p.direction, p.skeleton, p.truncated,
		p.status, p.errorKind, paths, at).Scan(&id); err != nil {
		t.Fatalf("造快照失败: %v", err)
	}
	return id
}

// accessLogRow 是 snapshot_access_log 的一行。
type accessLogRow struct {
	SnapshotID  int64
	ActorUserID sql.NullInt64
	ActorKind   string
	Reason      sql.NullString
}

// readAccessLog 读出某个 request_id 下的全部访问日志，按写入顺序。
func readAccessLog(t *testing.T, db *sql.DB, requestID string) []accessLogRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT snapshot_id, actor_user_id, actor_kind, reason
		  FROM snapshot_access_log
		 WHERE request_id = $1
		 ORDER BY id`, requestID)
	if err != nil {
		t.Fatalf("读访问日志失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []accessLogRow
	for rows.Next() {
		var row accessLogRow
		if err := rows.Scan(&row.SnapshotID, &row.ActorUserID, &row.ActorKind, &row.Reason); err != nil {
			t.Fatalf("读访问日志行失败: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历访问日志: %v", err)
	}
	return out
}

// TestSnapshotGetWritesAccessLog 是这条命令最要紧的判据：
// 读了别人的请求结构，就必须留下痕迹。
//
// 它同时覆盖「同一个 request_id 的多行都打出来」——造的是请求与响应各一行，
// 而只取一行的那种实现会在这里挂掉。
func TestSnapshotGetWritesAccessLog(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID, username := seedSnapshotUser(t, db)

	requestID := uniqueName("req")
	at := time.Now().UTC().Add(-time.Minute)
	reqRowID := insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "request",
		skeleton:  `{"model":"probe-model","stream":true}`,
		redacted:  []string{"messages[0].content[0].text"},
		createdAt: at,
	})
	respRowID := insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "response",
		skeleton:  `{"upstream_model":"probe-model"}`,
		truncated: true,
		status:    ptrTo(500), errorKind: ptrTo("upstream_error"),
		createdAt: at.Add(time.Millisecond),
	})

	const reason = "排查一次 500"
	var runErr error
	out, note := captureStreams(t, func() {
		runErr = runSnapshotGet([]string{
			requestID, "--actor", username, "--reason", reason,
		})
	})
	if runErr != nil {
		t.Fatalf("show 失败: %v", runErr)
	}

	// 两行都打出来了，而且列上的东西都在。
	for _, want := range []string{
		"2 条快照",
		"direction=request", "direction=response",
		"upstream_status=500", "error_kind=upstream_error",
		"messages[0].content[0].text",
		"skeleton_truncated=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	// 「降级成摘要层」是那一行的**附注**，走 stderr：它在下面那条清单里，
	// 与「这一行被截断了」是同一件事的两种说法，混在表格里会被 `| head` 接走。
	if !strings.Contains(note, "降级成摘要层") {
		t.Errorf("截断那句附注该走 stderr：\n%s", note)
	}
	if strings.Contains(out, "降级成摘要层") {
		t.Errorf("截断那句附注混进了 stdout：\n%s", out)
	}

	// 访问日志：读了几行就写几行，字段都要对。
	logged := readAccessLog(t, db, requestID)
	if len(logged) != 2 {
		t.Fatalf("访问日志应当有 2 行（请求、响应各一行），实际 %d 行", len(logged))
	}
	wantIDs := []int64{reqRowID, respRowID}
	for i, row := range logged {
		if row.SnapshotID != wantIDs[i] {
			t.Errorf("第 %d 行日志的 snapshot_id = %d，期望 %d", i+1, row.SnapshotID, wantIDs[i])
		}
		if !row.ActorUserID.Valid || row.ActorUserID.Int64 != userID {
			t.Errorf("第 %d 行日志的 actor_user_id = %v，期望 %d", i+1, row.ActorUserID, userID)
		}
		if row.ActorKind != snapshotActorKindAdmin {
			t.Errorf("第 %d 行日志的 actor_kind = %q，期望 %q", i+1, row.ActorKind, snapshotActorKindAdmin)
		}
		if !row.Reason.Valid || row.Reason.String != reason {
			t.Errorf("第 %d 行日志的 reason = %v，期望 %q", i+1, row.Reason, reason)
		}
	}
}

// TestSnapshotGetUnknownRequestDoesNotLog 是判据「不存在的 request_id
// 非零退出，且不写访问日志」。
//
// 没读到东西不算一次访问：把「没找到」也记进去，日志里就会混进一批
// 「有人查过这个不存在的 ID」，而那不是审计要回答的问题。
func TestSnapshotGetUnknownRequestDoesNotLog(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	_, username := seedSnapshotUser(t, db)
	requestID := uniqueName("req-absent")

	var runErr error
	out, _ := captureStreams(t, func() {
		runErr = runSnapshotGet([]string{requestID, "--actor", username, "--reason", "看看"})
	})
	if runErr == nil {
		t.Fatal("不存在的 request_id 应当报错")
	}
	if code := exitCode(runErr); code == exitOK {
		t.Errorf("退出码不该是成功：%v", runErr)
	}
	if strings.Contains(out, "skeleton") {
		t.Errorf("没读到东西不该打印骨架：\n%s", out)
	}
	if logged := readAccessLog(t, db, requestID); len(logged) != 0 {
		t.Errorf("没读到东西不该写访问日志，实际写了 %d 行", len(logged))
	}
}

// TestSnapshotGetDirectionFilterNarrowsTheRead 确认 --direction 只读那一个方向。
func TestSnapshotGetDirectionFilterNarrowsTheRead(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID, username := seedSnapshotUser(t, db)

	requestID := uniqueName("req")
	at := time.Now().UTC().Add(-time.Minute)
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "request",
		skeleton: `{"model":"probe-model"}`, createdAt: at,
	})
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "response",
		skeleton: `{"upstream_model":"probe-model"}`, createdAt: at.Add(time.Millisecond),
	})

	var runErr error
	out, _ := captureStreams(t, func() {
		runErr = runSnapshotGet([]string{
			requestID, "--actor", username, "--reason", "只看请求", "--direction", "request",
		})
	})
	if runErr != nil {
		t.Fatalf("show 失败: %v", runErr)
	}
	if !strings.Contains(out, "1 条快照") || !strings.Contains(out, "direction=request") {
		t.Errorf("--direction request 应当只读请求那一行：\n%s", out)
	}
	if strings.Contains(out, "direction=response") {
		t.Errorf("--direction request 不该把响应那一行也读出来：\n%s", out)
	}
	// 读了几行就写几行日志：读了 1 行就该只有 1 行。
	if logged := readAccessLog(t, db, requestID); len(logged) != 1 {
		t.Errorf("访问日志应当只有 1 行，实际 %d 行", len(logged))
	}
}

// TestSnapshotGetFilteredMissSaysWhichDirection 钉住「一条都没读到」的措辞。
//
// 库里确实有这次请求、但只有另一个方向那一行时，「没有快照」与
// 「没有这个方向的快照」是两件事，而后者指向一个写错的 --direction，
// 不是一次写错的采集。
func TestSnapshotGetFilteredMissSaysWhichDirection(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID, username := seedSnapshotUser(t, db)

	requestID := uniqueName("req")
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "response",
		skeleton: `{"upstream_model":"probe-model"}`,
	})

	var runErr error
	_, _ = captureStreams(t, func() {
		runErr = runSnapshotGet([]string{
			requestID, "--actor", username, "--reason", "看请求", "--direction", "request",
		})
	})
	if runErr == nil {
		t.Fatal("该方向没有快照时应当报错")
	}
	if !strings.Contains(runErr.Error(), "request 方向") {
		t.Errorf("错误信息应当点出是哪个方向没有：%v", runErr)
	}
	if logged := readAccessLog(t, db, requestID); len(logged) != 0 {
		t.Errorf("没读到东西不该写访问日志，实际写了 %d 行", len(logged))
	}
}

// TestSnapshotGetLogFailureBlocksRead 是判据「读了但没记不可能发生」。
//
// 让写日志那一步在真库上失败：reason 里带一个 NUL 字节，PostgreSQL 的 text
// 收不了它（表上没有任何约束会因为「理由太长」而拒绝，所以这是这条路径上
// 唯一能确定复现的失败方式）。此时命令必须整体失败，一行数据都不返回。
//
// 判据是**输出里有没有骨架**：把写日志挪到读之后（或者去掉事务）的实现，
// 会在这里打出骨架并且返回 nil——那正是「读了但没记」的形状。
func TestSnapshotGetLogFailureBlocksRead(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID, username := seedSnapshotUser(t, db)

	requestID := uniqueName("req")
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "request",
		skeleton: `{"model":"probe-model"}`,
	})

	var runErr error
	out, _ := captureStreams(t, func() {
		runErr = runSnapshotGet([]string{
			requestID, "--actor", username, "--reason", "坏\x00理由",
		})
	})
	if runErr == nil {
		t.Fatal("访问日志写不进去时，读也必须失败——否则就是「读了但没记」")
	}
	if !strings.Contains(runErr.Error(), "写快照访问日志失败") {
		t.Errorf("错误应当指向写日志那一步，得到 %v", runErr)
	}
	if strings.Contains(out, "skeleton") {
		t.Errorf("日志没写进去就不该返回数据，实际输出：\n%s", out)
	}
	if logged := readAccessLog(t, db, requestID); len(logged) != 0 {
		t.Errorf("事务应当整体回滚，日志里却有 %d 行", len(logged))
	}
}

// ptrTo 造一个指向字面量的指针，供可空列用。
func ptrTo[T any](v T) *T { return &v }

// ── 机器可读输出 ────────────────────────────────────────────────────────

// jsonDocOf 把一份文档编成 JSON 再解回 map。
//
// 先编再解而不是直接读 map：用例要钉的正是**编出来的那串字节**能不能被
// 下游解析。直接读 map 会漏掉「有个字段编不出来」这一类——那正是
// docs/cli-output.md 里 stdout 纯净性那一条要防的东西。
func jsonDocOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("文档编不成 JSON: %v", err)
	}
	if strings.ContainsRune(string(raw), escByte) {
		t.Errorf("不许有转义序列：%s", raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("编出来的不是一份合法的 JSON: %v\n%s", err, raw)
	}
	return got
}

// 快照的每一栏都要进 JSON，且两栏 NULL 给 null 而不是表格里那句占位符。
func TestSnapshotsDocCarriesEveryColumn(t *testing.T) {
	first := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	rows := []snapshotRow{
		{
			ID: 1, Direction: "request", SkeletonVersion: 1, CaptureMode: "skeleton",
			Skeleton:      map[string]any{"model": "probe-model"},
			RedactedPaths: []string{"messages[0].content[0].text"},
			CreatedAt:     first,
		},
		{
			ID: 2, Direction: "response", SkeletonVersion: 1, CaptureMode: "summary",
			Skeleton:       map[string]any{"upstream_model": "probe-model"},
			Truncated:      true,
			UpstreamStatus: sql.NullInt64{Int64: 500, Valid: true},
			ErrorKind:      sql.NullString{String: "upstream_error", Valid: true},
			CreatedAt:      first.Add(time.Millisecond),
		},
	}

	doc := jsonDocOf(t, snapshotsDoc("req-probe", rows))
	if doc["request_id"] != "req-probe" {
		t.Errorf("request_id = %v", doc["request_id"])
	}
	if summary, _ := doc["summary"].(map[string]any); summary["total"] != float64(2) {
		t.Errorf("summary.total = %v，想要 2", doc["summary"])
	}

	items, ok := doc["snapshots"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("snapshots 不是两条：%#v", doc["snapshots"])
	}
	req, _ := items[0].(map[string]any)
	resp, _ := items[1].(map[string]any)

	if req["id"] != float64(1) || req["direction"] != "request" {
		t.Errorf("请求方向那一行不对：%#v", req)
	}
	// 时刻给完整精度（表格那一档截到分钟是排版需要）。
	if req["created_at"] != "2026-10-17T01:02:03Z" {
		t.Errorf("created_at = %v", req["created_at"])
	}
	if req["skeleton_version"] != float64(1) || req["capture_mode"] != "skeleton" {
		t.Errorf("版本与采集模式不对：%#v", req)
	}
	// 表格里的三段文本（<不适用…>、<无上游响应>、<无>）一个都不进 JSON。
	for _, item := range []map[string]any{req, resp} {
		if item["upstream_status"] == "不适用" || item["upstream_status"] == "<不适用（请求方向快照）>" {
			t.Errorf("占位符不该进 JSON：%#v", item)
		}
	}
	if req["upstream_status"] != nil || req["error_kind"] != nil {
		t.Errorf("请求方向那两列该是 null：%#v", req)
	}
	if req["skeleton_truncated"] != false {
		t.Errorf("没被截断时该是 false，得到 %v", req["skeleton_truncated"])
	}
	paths, _ := req["redacted_paths"].([]any)
	if len(paths) != 1 || paths[0] != "messages[0].content[0].text" {
		t.Errorf("redacted_paths 不对：%#v", req["redacted_paths"])
	}
	skeleton, _ := req["skeleton"].(map[string]any)
	if skeleton["model"] != "probe-model" {
		t.Errorf("骨架要原样带出来：%#v", req["skeleton"])
	}

	if resp["upstream_status"] != float64(500) || resp["error_kind"] != "upstream_error" {
		t.Errorf("响应方向那两列不对：%#v", resp)
	}
	// 被降级成摘要层这件事必须带出来：它与一份完整的骨架在肉眼下长得一样。
	if resp["skeleton_truncated"] != true {
		t.Errorf("被截断时该是 true，得到 %v", resp["skeleton_truncated"])
	}
}

// 一条都没读到时是空数组，不是 null，也不是表格那句「0 条快照」。
func TestSnapshotsDocEmptyListIsArray(t *testing.T) {
	doc := jsonDocOf(t, snapshotsDoc("req-none", nil))
	items, ok := doc["snapshots"].([]any)
	if !ok {
		t.Fatalf("空清单该是数组而不是 null：%#v", doc["snapshots"])
	}
	if len(items) != 0 {
		t.Errorf("空清单该是空的：%#v", items)
	}
	if summary, _ := doc["summary"].(map[string]any); summary["total"] != float64(0) {
		t.Errorf("summary.total = %v，想要 0", doc["summary"])
	}
}

// 允许表：每一对「表面 × 方向」一段，每条规则的 Note 完整带出来。
//
// Note 是这条命令存在的理由（见 printAllowlists），JSON 这一档同样不许省。
func TestAllowlistDocCarriesEveryNote(t *testing.T) {
	surfaces := snapshot.Surfaces()
	roots := snapshot.Roots()

	doc := jsonDocOf(t, allowlistDoc(surfaces, roots))
	groups, ok := doc["allowlist"].([]any)
	if !ok {
		t.Fatalf("allowlist 不是数组：%#v", doc["allowlist"])
	}
	if len(groups) != len(surfaces)*len(roots) {
		t.Fatalf("分段数 = %d，想要 %d", len(groups), len(surfaces)*len(roots))
	}

	rules := 0
	for i, raw := range groups {
		group, _ := raw.(map[string]any)
		s := surfaces[i/len(roots)]
		root := roots[i%len(roots)]
		if group["surface"] != string(s) || group["direction"] != string(root) {
			t.Errorf("第 %d 段的表面/方向不对：%#v", i, group)
		}

		items, _ := group["rules"].([]any)
		want := snapshot.AllowlistFor(s, root)
		if len(items) != len(want) {
			t.Fatalf("表面 %s 方向 %s 的规则数 = %d，想要 %d", s, root, len(items), len(want))
		}
		rules += len(items)
		for j, rawRule := range items {
			rule, _ := rawRule.(map[string]any)
			if rule["path"] != want[j].Path {
				t.Errorf("规则路径 = %v，想要 %q", rule["path"], want[j].Path)
			}
			if rule["kind"] != want[j].Kind {
				t.Errorf("%s 的 kind = %v，想要 %q", want[j].Path, rule["kind"], want[j].Kind)
			}
			if rule["upstream_value"] != want[j].UpstreamValue {
				t.Errorf("%s 的 upstream_value = %v，想要 %t",
					want[j].Path, rule["upstream_value"], want[j].UpstreamValue)
			}
			// 按整串比对而不是子串：一条 Note 是另一条的前缀时，
			// 子串匹配分辨不出「后一条被截断了」。
			if rule["note"] != want[j].Note {
				t.Errorf("%s 的 Note 没有完整带出来：%v", want[j].Path, rule["note"])
			}
		}
	}
	if summary, _ := doc["summary"].(map[string]any); summary["rules"] != float64(rules) {
		t.Errorf("summary.rules = %v，想要 %d", doc["summary"], rules)
	}
}

// 命令级：`snapshot allowlist -o json` 的 stdout 是一份纯 JSON。
//
// 这条命令不连库，所以它是「整条命令的 stdout 契约」最省事的那条证据。
func TestSnapshotAllowlistJSONStdoutIsPure(t *testing.T) {
	withFormat(t, formatJSON)

	var err error
	stdout, _ := captureStreams(t, func() { err = runSnapshotAllowlist(nil) })
	if err != nil {
		t.Fatalf("allowlist 失败: %v", err)
	}
	if strings.ContainsRune(stdout, escByte) {
		t.Errorf("json 模式下 stdout 里不许有转义序列：%q", stdout)
	}

	var got struct {
		Status    string `json:"status"`
		ExitCode  int    `json:"exit_code"`
		Allowlist []struct {
			Surface   string `json:"surface"`
			Direction string `json:"direction"`
			Rules     []struct {
				Path string `json:"path"`
				Note string `json:"note"`
			} `json:"rules"`
		} `json:"allowlist"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &got); jsonErr != nil {
		t.Fatalf("json 模式下 stdout 不是一份合法的 JSON: %v\n%s", jsonErr, stdout)
	}
	if got.Status != jsonOK || got.ExitCode != exitOK {
		t.Errorf("status=%q exit_code=%d", got.Status, got.ExitCode)
	}
	if len(got.Allowlist) == 0 {
		t.Fatal("允许表不该是空的，这条命令就没有内容可打")
	}
	for _, group := range got.Allowlist {
		if len(group.Rules) == 0 {
			t.Errorf("表面 %s 方向 %s 的规则是空的", group.Surface, group.Direction)
		}
		for _, rule := range group.Rules {
			if strings.TrimSpace(rule.Note) == "" {
				t.Errorf("规则 %s 没有 Note", rule.Path)
			}
		}
	}
	// 表格那一档的那句「表面 chat 方向 request：N 条」不该出现在 json 里。
	if strings.Contains(stdout, "表面 ") || strings.Contains(stdout, " 条\n") {
		t.Errorf("json 的 stdout 里混进了表格那一档的排版：\n%s", stdout)
	}
}

// 命令级：`snapshot show -o json` 在真库上也是一份纯 JSON，且访问日志照写。
//
// 读快照要落访问日志是一条硬契约（谁读了、为什么读）。JSON 这一档换的是
// **怎么打**，不是「读没读」，所以日志那一半必须原样还在。
func TestSnapshotGetJSONStdoutIsPure(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID, username := seedSnapshotUser(t, db)

	requestID := uniqueName("req-json")
	at := time.Now().UTC().Add(-time.Minute)
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "request",
		skeleton: `{"model":"probe-model"}`, createdAt: at,
	})
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "response",
		skeleton: `{"upstream_model":"probe-model"}`, truncated: true,
		status: ptrTo(500), errorKind: ptrTo("upstream_error"),
		createdAt: at.Add(time.Millisecond),
	})

	withFormat(t, formatJSON)
	var runErr error
	stdout, _ := captureStreams(t, func() {
		runErr = runSnapshotGet([]string{
			requestID, "--actor", username, "--reason", "排查一次 500",
		})
	})
	if runErr != nil {
		t.Fatalf("show 失败: %v", runErr)
	}

	var got struct {
		RequestID string `json:"request_id"`
		Snapshots []struct {
			ID            int64  `json:"id"`
			Direction     string `json:"direction"`
			Truncated     bool   `json:"skeleton_truncated"`
			UpstreamState *int64 `json:"upstream_status"`
			ErrorKind     string `json:"error_kind"`
		} `json:"snapshots"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &got); jsonErr != nil {
		t.Fatalf("json 模式下 stdout 不是一份合法的 JSON: %v\n%s", jsonErr, stdout)
	}
	if got.RequestID != requestID {
		t.Errorf("request_id = %q，想要 %q", got.RequestID, requestID)
	}
	if len(got.Snapshots) != 2 {
		t.Fatalf("快照数 = %d，想要 2", len(got.Snapshots))
	}
	if got.Snapshots[0].Direction != "request" || got.Snapshots[0].UpstreamState != nil {
		t.Errorf("请求方向那一行不对：%#v", got.Snapshots[0])
	}
	if got.Snapshots[1].ErrorKind != "upstream_error" || !got.Snapshots[1].Truncated {
		t.Errorf("响应方向那一行不对：%#v", got.Snapshots[1])
	}

	if logged := readAccessLog(t, db, requestID); len(logged) != 2 {
		t.Errorf("换输出格式不该少写访问日志：得到 %d 行，想要 2 行", len(logged))
	}
}
