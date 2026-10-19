package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// withFormat 把这一次运行的输出格式换成 f，用例结束时复原。
//
// 复原到**进入时的那个值**而不是写死 table：一个用例里嵌套调用两次时，
// 写死会让内层提前把外层的设置抹掉，而那种错在用例里表现为「另一条用例
// 莫名其妙跑在 json 档」。
func withFormat(t *testing.T, f formatKind) {
	t.Helper()
	saved := outputFormat
	outputFormat = f
	t.Cleanup(func() { outputFormat = saved })
}

// ── --format 的解析 ─────────────────────────────────────────────────────
//
// 这一层要钉住三件事，每一件都在线上见过同类错：
//
//  1. 写在子命令**前后**都认——人补一个选项时习惯写在最后；
//  2. 取值不认识时报参数错，**不静默回落成表格**；
//  3. 它旁边的参数一个都不许被吃掉。

func TestTakeFormatFlagAcceptsBothPositions(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want formatKind
		rest []string
	}{
		{
			// 写在子命令之前：与 --env-file 一样，全局选项的老位置。
			name: "写在前面",
			args: []string{"--format", "json", "doctor"},
			want: formatJSON,
			rest: []string{"doctor"},
		},
		{
			// 写在子命令之后：这一条最要紧，它是人补选项时最自然的写法。
			name: "写在后面",
			args: []string{"doctor", "--format", "json"},
			want: formatJSON,
			rest: []string{"doctor"},
		},
		{
			name: "等号写法",
			args: []string{"channel", "list", "--format=json"},
			want: formatJSON,
			rest: []string{"channel", "list"},
		},
		{
			name: "简写 -o",
			args: []string{"-o", "json", "model", "list"},
			want: formatJSON,
			rest: []string{"model", "list"},
		},
		{
			// 单横线的长写法：本仓其余选项由 flag 包解析，而它同时认
			// -name 与 --name。只有全局选项不认的话，`-format json doctor`
			// 会以「未知子命令」收场，那不是使用者能自己看明白的错。
			name: "单横线长写法",
			args: []string{"-format", "json", "doctor"},
			want: formatJSON,
			rest: []string{"doctor"},
		},
		{
			name: "显式 table",
			args: []string{"doctor", "--format", "table"},
			want: formatTable,
			rest: []string{"doctor"},
		},
		{
			// 不给 --format 就是面向终端那一档：脚本要多写一个参数，
			// 人什么都不用改。
			name: "不给",
			args: []string{"doctor"},
			want: formatTable,
			rest: []string{"doctor"},
		},
		{
			// 后写的赢：与 flag 包一致。
			name: "出现两次",
			args: []string{"--format", "table", "doctor", "-o", "json"},
			want: formatJSON,
			rest: []string{"doctor"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := takeFormatFlag(tc.args)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("格式 = %v，想要 %v", got, tc.want)
			}
			if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
				t.Errorf("剩下的参数 = %q，想要 %q", rest, tc.rest)
			}
		})
	}
}

// 取值不认识时报参数错（退出码 2），不回落成表格。
//
// 回落的表现最坏：`--format=jsn` 的脚本会拿到一段表格文本，而报错的是下游的
// jq——看日志的人会去查 jq 的用法，而问题在命令行这一侧。
func TestTakeFormatFlagRejectsUnknownValue(t *testing.T) {
	for _, raw := range []string{"jsn", "yaml", "", "JSON", "table,json"} {
		_, rest, err := takeFormatFlag([]string{"doctor", "--format", raw})
		if err == nil {
			t.Fatalf("--format %q 应当报错", raw)
		}
		if rest != nil {
			t.Errorf("报错时不该返回剩下的参数，得到 %q", rest)
		}
		if code := exitCode(err); code != exitUsage {
			t.Errorf("--format %q 的退出码 = %d，想要 %d（参数错）", raw, code, exitUsage)
		}
		// 错误信息里要给出合法取值：只说「不认识」会让人去翻源码。
		for _, want := range []string{"table", "json"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误信息里应当列出取值 %q：%v", want, err)
			}
		}
	}
}

// --format 后面没跟取值时报参数错，不会去把下一个参数当取值之外的别的东西。
func TestTakeFormatFlagRequiresValue(t *testing.T) {
	_, _, err := takeFormatFlag([]string{"doctor", "--format"})
	if err == nil {
		t.Fatal("--format 后面没跟取值时应当报错")
	}
	if code := exitCode(err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d", code, exitUsage)
	}
}

// ── stdout 的纯净性 ─────────────────────────────────────────────────────
//
// 这是 `--format json` 对外的全部承诺：stdout 上只有一份严格合法的 JSON。

// emitJSON 写出来的东西必须**整份**能被 json.Unmarshal 收下，并且不含
// 任何转义序列。
//
// 判据走 json.Unmarshal 而不是「看着像 JSON」：混进一行提示、一个进度点、
// 或者一段 ANSI 上色，都会让 Unmarshal 在这里失败——而人眼看那些输出时，
// 一行提示语与 JSON 混在一起未必看得出来。
func TestEmitJSONWritesNothingButJSON(t *testing.T) {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": 1}
	doc["items"] = []map[string]any{{"id": 1, "name": "a<b>&c"}}

	var buf bytes.Buffer
	if err := emitJSON(&buf, doc, nil); err != nil {
		t.Fatalf("写出失败: %v", err)
	}

	out := buf.String()
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout 不是一份合法的 JSON: %v\n%s", err, out)
	}
	if strings.ContainsRune(out, escByte) {
		t.Errorf("json 模式的输出里不许有转义序列：%q", out)
	}
	// 不转义 HTML：原文里这几个字符不该变成 < 这类。它们仍是合法 JSON，
	// 但人 `| jq .` 看那份输出时就多了一层要解码的东西。
	if !strings.Contains(out, "a<b>&c") {
		t.Errorf("HTML 字符不该被转义成 \\u 序列：\n%s", out)
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Errorf("一份文档应当以换行结尾：%q", out)
	}
}

// exit_code 由 cause 算出来，且与 status 各说各的。
func TestEmitJSONFillsExitCodeFromCause(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  int
	}{
		{"成功", nil, exitOK},
		{"其他失败", errors.New("不通过"), exitFailure},
		{"参数错", usageErr(errors.New("取值不认识")), exitUsage},
		{"连不上库", dbErr(errors.New("连接数据库失败")), exitDBUnreach},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := emitJSON(&buf, jsonDoc(jsonFailed), tc.cause); !errors.Is(err, tc.cause) {
				t.Fatalf("emitJSON 应当把 cause 原样返回，得到 %v", err)
			}
			var got struct {
				ExitCode int `json:"exit_code"`
			}
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("stdout 不是一份合法的 JSON: %v", err)
			}
			if got.ExitCode != tc.want {
				t.Errorf("exit_code = %d，想要 %d", got.ExitCode, tc.want)
			}
		})
	}
}

// 编不出来的文档不许在 stdout 上留下半份 JSON。
//
// chan 是 encoding/json 收不下的类型，用它是为了造出「序列化失败」这一支。
// 那一支下 stdout 必须是**空的**：留下一半比什么都不留更坏——下游的 jq
// 会报一句语法错，而真正的原因在 stderr 上。
func TestEmitJSONLeavesStdoutEmptyOnMarshalFailure(t *testing.T) {
	doc := jsonDoc(jsonOK)
	doc["broken"] = make(chan int)

	var buf bytes.Buffer
	if err := emitJSON(&buf, doc, nil); err == nil {
		t.Fatal("编不出来的文档应当报错")
	}
	if buf.Len() != 0 {
		t.Errorf("序列化失败时 stdout 应当为空，得到 %q", buf.String())
	}
}

// 还没接机器可读输出的子命令在 json 模式下**报参数错**，不静默打表格。
//
// 静默回落是最坏的结果：`larkaxctl apikey create ... -o json | jq` 会拿到
// 一段给人看的文本，报错的是 jq，看起来像 jq 的问题；而写类命令那时事情
// 已经做完了，脚本只看到一句解析错误，于是重跑一次。
func TestJSONUnsupportedOnlyFailsInJSONMode(t *testing.T) {
	withFormat(t, formatTable)
	if err := jsonUnsupported("apikey create"); err != nil {
		t.Fatalf("table 档不该被拦下: %v", err)
	}

	withFormat(t, formatJSON)
	err := jsonUnsupported("apikey create")
	if err == nil {
		t.Fatal("json 档应当把还没接的命令拦下")
	}
	if code := exitCode(err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
	}
	if !strings.Contains(err.Error(), "apikey create") {
		t.Errorf("错误信息里要点名是哪条命令：%v", err)
	}
}

// ── 命令级：还没接的命令一个都不许静默回落 ─────────────────────────────
//
// 这条用例把「哪些命令还没接」整个钉住：每一条都必须在**做任何事之前**报参数错。
// 少了一条 guard，那条命令在 json 模式下会打出一段表格或一段给人看的文本，
// 而 stdout 的纯净性承诺就悄悄破了——写类命令更是会在报错之前先把事情做完。
//
// 全部用「参数为空」调用：guard 是函数体第一行，所以它应当在参数校验之前就
// 拦下，运行时不会连库、也不会写任何东西。哪天有人把 guard 挪到参数校验之后，
// 这里会先看到一条关于参数的报错，而不是 jsonUnsupported 那一句。
func TestUnsupportedCommandsRefuseJSONBeforeDoingAnything(t *testing.T) {
	cases := []struct {
		command string
		run     func([]string) error
		args    []string
	}{
		{"user create", runUserCreate, nil},
		{"wallet create", runWalletCreate, nil},
		{"wallet credit", runWalletCredit, nil},
		{"wallet adjust", runWalletAdjust, nil},
		{"wallet refund", runWalletRefund, nil},
		{"wallet payout request", runPayoutRequest, nil},
		{"wallet payout approve", runPayoutApprove, nil},
		{"wallet payout reject", runPayoutReject, nil},
		{"wallet payout paid", runPayoutPaid, nil},
		{"price calendar import", runCalendarImport, nil},
		{"maintenance idempotency", runIdempotencyCleanup, nil},
		{"partition ensure", runPartitionEnsure, nil},
		{"secret rotate", runRotate, nil},
		{"ledger reconcile", runReconcile, nil},
		{"snapshot redact", runSnapshotRedact, nil},
		{"snapshot cleanup", runSnapshotCleanup, nil},
		{"migrate up", runMigrate, []string{"up"}},
		{"migrate status", runMigrate, []string{"status"}},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			withFormat(t, formatJSON)

			// 输出要接住：guard 万一漏了，这条命令会去打表格或动库，
			// 而那时用例看到的应当是「它没被拦下」，不是一片刷屏。
			var err error
			stdout, stderr := captureStreams(t, func() { err = tc.run(tc.args) })

			if err == nil {
				t.Fatalf("%s 在 json 模式下应当被拦下，实际跑通了", tc.command)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("%s 的退出码 = %d，想要 %d（参数错）", tc.command, code, exitUsage)
			}
			if !strings.Contains(err.Error(), tc.command) {
				t.Errorf("%s 的错误信息要点名命令：%v", tc.command, err)
			}
			// 拦下来之后 stdout 上一个字节都不该有：报错发生在产出结果之前。
			if stdout != "" {
				t.Errorf("%s 被拦下后 stdout 应当为空，得到 %q", tc.command, stdout)
			}
			// 错误信息自己走 stderr 是 main 的活，这里只确认这两条流
			// 没有被这类命令提前写脏。
			_ = stderr
		})
	}
}

// ── 命令级：过 run() 的那条路 ───────────────────────────────────────────
//
// 上面几条测的是零件，这一条测的是「真的从命令行进来会怎样」：摘参数的顺序、
// 分派、退出码，三处任何一处接错都在这里现形。

// run() 认子命令前后的 --format，且 json 模式下 stdout 是一份能解析的 JSON。
func TestRunVersionJSONIsMachineReadable(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"写在前面", []string{"larkaxctl", "--format", "json", "version"}},
		{"写在后面", []string{"larkaxctl", "version", "--format", "json"}},
		{"简写", []string{"larkaxctl", "-o", "json", "version"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// run() 会按参数把 outputFormat 定下来，跑完要还原，
			// 否则后面那些用例会跟着跑在 json 档上。
			t.Cleanup(func() { outputFormat = formatTable })

			saved := os.Args
			os.Args = tc.args
			t.Cleanup(func() { os.Args = saved })

			var err error
			stdout, _ := captureStreams(t, func() { err = run() })
			if err != nil {
				t.Fatalf("跑失败: %v", err)
			}

			var got struct {
				Status   string `json:"status"`
				Version  string `json:"version"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("json 模式下 stdout 不是一份合法的 JSON: %v\n%s", err, stdout)
			}
			if got.Status != jsonOK || got.ExitCode != exitOK {
				t.Errorf("status=%q exit_code=%d，想要 %q / %d",
					got.Status, got.ExitCode, jsonOK, exitOK)
			}
			// stdout 恰好一份文档：前后不许有别的行。
			if strings.Count(stdout, "{") != 1 {
				t.Errorf("stdout 上应当只有一份文档：\n%s", stdout)
			}
		})
	}
}

// 取值不合法时退出码是 2，stdout 上一个字节都不该有。
func TestRunRejectsUnknownFormatWithoutWritingStdout(t *testing.T) {
	t.Cleanup(func() { outputFormat = formatTable })

	saved := os.Args
	os.Args = []string{"larkaxctl", "doctor", "--format", "jsn"}
	t.Cleanup(func() { os.Args = saved })

	var err error
	stdout, _ := captureStreams(t, func() { err = run() })
	if err == nil {
		t.Fatal("取值不合法时应当报错")
	}
	if code := exitCode(err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("参数错时 stdout 应当为空，得到 %q", stdout)
	}
}

// 连不上库时 doctor 仍然打出一份完整的 JSON，退出码是 3。
//
// 这一条不需要真库：DSN 指向一个没人听的端口，连接立刻被拒。它钉住的是
// **另一条**契约——外部的探针要能在库挂了的时候仍然解析 stdout，而不是
// 拿到一份空输出、再去区分「库挂了」与「探针自己坏了」。
func TestDoctorJSONWithoutDatabaseStillEmitsReport(t *testing.T) {
	withFormat(t, formatJSON)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	var err error
	stdout, _ := captureStreams(t, func() { err = runDoctor(nil) })

	var got struct {
		Status  string `json:"status"`
		Summary map[string]int
		Checks  []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Skipped any    `json:"skip_reason"`
		} `json:"checks"`
		ExitCode int `json:"exit_code"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &got); jsonErr != nil {
		t.Fatalf("json 模式下 stdout 不是一份合法的 JSON: %v\n%s", jsonErr, stdout)
	}
	if code := exitCode(err); code != exitDBUnreach {
		t.Errorf("退出码 = %d，想要 %d（连不上库）", code, exitDBUnreach)
	}
	if got.ExitCode != exitDBUnreach {
		t.Errorf("报告里的 exit_code = %d，与进程的退出码对不上", got.ExitCode)
	}
	// 六项都「没跑成」——这个 Status 不是 ok，也不是 failed：它说的是
	// 「一项都没查成」，与「查了、都没问题」是两件事（见 LESSONS 第 16 条）。
	if got.Status != jsonSkipped {
		t.Errorf("status = %q，想要 %q", got.Status, jsonSkipped)
	}
	if got.Summary["total"] != 6 || got.Summary["skipped"] != 6 {
		t.Errorf("summary 应当是六项全跳过，得到 %v", got.Summary)
	}
	if len(got.Checks) != 6 {
		t.Fatalf("checks 应当有六项，得到 %d", len(got.Checks))
	}
	if got.Checks[0].Name != "database" {
		t.Errorf("第一项的名字 = %q，想要 database", got.Checks[0].Name)
	}
	for _, c := range got.Checks {
		if c.Status != "skipped" {
			t.Errorf("第 %s 项的 status = %q，想要 skipped", c.Name, c.Status)
		}
		if c.Skipped == nil || c.Skipped == "" {
			t.Errorf("第 %s 项没跑成，skip_reason 必须有原文", c.Name)
		}
	}
}

// ── 机器读的时刻恒 UTC ──────────────────────────────────────────────────

// TestJSONTimeIsUTCWhateverTheProcessZone 钉住「机器读的时刻恒 UTC，
// 与进程时区无关」。
//
// ⚠️ 这条用例的价值全在**非 UTC 时区下会不会变红**。CI runner 默认 UTC，
// 而库里 timestamptz 经 pgx 交回来的 time.Time 带的是 time.Local
// （pgx v5 的 timestamptz 在 ScanLocation 未设时用 time.Local 构造，
// 见 pgtype/timestamptz.go 的 ScanLocation 说明）。于是「本地恰好是 UTC」
// 的机器上，去掉 .UTC() 与留着它编出**同一个**字符串——只在 UTC 下跑的
// 那道门在结构上必然通过，它就是个摆设。gates.yml 里因此单开一步
// `TZ=Asia/Shanghai ... go test`，并先证明那个时区真的生效了。
//
// fixture 特意用 **time.Local** 构造，而不是 FixedZone：FixedZone 的那一版
// （见 apikey_test.go 的 TestAPIKeyTimesAreUTCWhateverTheProcessZone）在任何
// 进程时区下都会红，证明不了「这道门跑在哪个时区」。这一版的成败**取决于
// 进程时区**，正是那道门的判据：
//
//   - TZ=UTC：tm 的 location 就是 UTC，去掉 .UTC() 也编出同样的串 → 绿；
//   - TZ=Asia/Shanghai：tm 是 +08:00，去掉 .UTC() 编出
//     `2026-01-02T11:04:05+08:00`（同一瞬间的另一个写法）→ 红。
func TestJSONTimeIsUTCWhateverTheProcessZone(t *testing.T) {
	// 东八区的 11:04:05 与 UTC 的 03:04:05 是同一瞬间。先构造成 UTC，
	// 再 .In(time.Local) 得到「同一瞬间、location 是进程时区」的那个值。
	instant := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tm := instant.In(time.Local)

	const want = "2026-01-02T03:04:05Z"
	if got := jsonTime(tm); got != want {
		t.Errorf("jsonTime = %q，想要 %q：机器读的时刻必须恒 UTC，与进程时区无关",
			got, want)
	}
	// 可空那两档走的是同一个 jsonTime，一并钉住：将来把其中一档改成自己
	// Format（绕开 jsonTime）时，这一条会把它照出来。
	if got := jsonTimePtr(&tm); got != want {
		t.Errorf("jsonTimePtr = %v，想要 %q", got, want)
	}
	if got := jsonTimeOrNull(tm); got != want {
		t.Errorf("jsonTimeOrNull = %v，想要 %q", got, want)
	}
}
