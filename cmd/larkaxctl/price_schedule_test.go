package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cliSchedule 是一份只用到星期维度的定价表：它不需要日历，所以能在一台
// 任何配置的实例上走完整条写入路径。
const cliSchedule = `{
  "tz": "Asia/Shanghai",
  "base": { "prompt": 0.0000015, "completion": 0.0000075 },
  "overrides": [ { "weekdays": ["fri"], "prompt": 0.000001 } ]
}`

// writeScheduleFile 把一份定价表落到临时目录，返回路径。
func writeScheduleFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "price.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写定价表文件失败: %v", err)
	}
	return path
}

// TestPriceScheduleRejectsBadFlags 走的是不碰数据库的那几条路径。
//
// 「二选一」的两侧都要拒：同时给两份说法时，谁覆盖谁都没有出处；
// 一份都不给时，落库那一栏非空，拒绝必须发生在连库之前。
func TestPriceScheduleRejectsBadFlags(t *testing.T) {
	path := writeScheduleFile(t, cliSchedule)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"create：表达式与定价表同时给",
			[]string{
				"--scope", "platform", "--name", "n1",
				"--expr", testExpr, "--schedule-file", path, "--by", "ops",
			},
			"只能给一个",
		},
		{
			"create：两份都没给",
			[]string{"--scope", "platform", "--name", "n1", "--by", "ops"},
			"缺少 --expr 或 --schedule-file",
		},
		{
			"create：文件不存在",
			[]string{
				"--scope", "platform", "--name", "n1",
				"--schedule-file", filepath.Join(t.TempDir(), "nope.json"), "--by", "ops",
			},
			"读定价表文件",
		},
		{
			"create：文件里不是合法的表",
			[]string{
				"--scope", "platform", "--name", "n1",
				"--schedule-file", writeScheduleFile(t, `{"tz":"Asia/Shanghai","base":{}}`),
				"--by", "ops",
			},
			"base 不能为空",
		},
		{
			"update：清表与换表同时给",
			[]string{
				"--id", "1", "--clear-schedule", "--schedule-file", path, "--by", "ops",
			},
			"只能给一个",
		},
		{
			"update：换表时路径是空串",
			[]string{"--id", "1", "--schedule-file", "  ", "--by", "ops"},
			"--schedule-file 不能为空",
		},
		{
			"update：表达式与定价表同时给",
			[]string{
				"--id", "1", "--expr", testExpr, "--schedule-file", path, "--by", "ops",
			},
			"只能给一个",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if strings.HasPrefix(tc.name, "create") {
				err = runCaptured(t, func() error { return runPriceCreate(tc.args) })
			} else {
				err = runCaptured(t, func() error { return runPriceUpdate(tc.args) })
			}
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPriceShowRejectsBadID 钉住 show 的位置参数只有「一个正整数」这一种形状。
func TestPriceShowRejectsBadID(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有参数", nil, "需要一个规则 ID"},
		{"多给了一个", []string{"1", "2"}, "需要一个规则 ID"},
		{"不是数字", []string{"abc"}, "必须是正整数"},
		{"零", []string{"0"}, "必须是正整数"},
		// 负号打头的参数要先过 `--`，否则记法层就把它当成一个旗标。
		{"负数", []string{"--", "-3"}, "必须是正整数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPriceShow(tc.args) })
			if err == nil {
				t.Fatal("期望报错，却通过了")
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

// TestPriceCreateScheduleThenShow 钉住「命令行写出的是同一张表」。
//
// 写入之后库里有两栏：schedule 是作者给的那张表，expr 是它编译出来的结果。
// show 打前者，list/求值用后者；这里两侧都核对一遍，免得某一侧静默变成
// 另一侧的近似物。
func TestPriceCreateScheduleThenShow(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("sched")
	// 模型名单收窄到一条只属于本用例的模型名：兜底规则会匹配到每一个
	// 模型，别的用例（例如 explain 找不到规则那一格）就会读到它。
	model := uniqueName("model")
	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", name, "--models", model,
		"--schedule-file", writeScheduleFile(t, cliSchedule),
	})
	idArg := strconv.FormatInt(id, 10)

	// 落库的表达式就是这张表编译出来的那一份。
	got := findRule(t, listRules(t, db), id)
	if got.Schedule == nil {
		t.Fatal("清单里读回来的规则没有定价表")
	}
	if !strings.Contains(got.Expr, `weekday("Asia/Shanghai") in [5]`) {
		t.Errorf("表达式里没有星期维度：%s", got.Expr)
	}

	// 人读的那一档打表，不打表达式。
	out, _ := captureStreams(t, func() {
		if err := runPriceShow([]string{idArg}); err != nil {
			t.Fatalf("price show 失败: %v", err)
		}
	})
	for _, want := range []string{name, "结构化定价表", `"tz": "Asia/Shanghai"`, `"fri"`} {
		if !strings.Contains(out, want) {
			t.Errorf("show 的输出里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "表达式：") {
		t.Errorf("带表的规则应当打表而不是表达式：\n%s", out)
	}

	// 机器可读的那一档两栏都给：脚本要能核对「expr 是这张表的编译结果」。
	withFormat(t, formatJSON)
	jsonOut, _ := captureStreams(t, func() {
		if err := runPriceShow([]string{idArg}); err != nil {
			t.Fatalf("price show --format json 失败: %v", err)
		}
	})
	for _, want := range []string{`"schedule"`, `"weekdays"`, `"expr"`, `weekday(`} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("json 输出里没有 %q：\n%s", want, jsonOut)
		}
	}
}

// TestPriceUpdateScheduleNeedsClearSchedule 钉住命令行上的「先清表再改写」。
//
// 带表的规则直接改表达式会让两份说法分家：表说的是这张表，求值用的是另一条
// 手写表达式。写入路径拒掉它，命令这一层要把那句提示原样报出来——使用者要
// 看到的正是「该给哪个旗标」。
func TestPriceUpdateScheduleNeedsClearSchedule(t *testing.T) {
	testDB(t)
	setupEnv(t)
	by := createTestUserID(t)

	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("sched"),
		"--models", uniqueName("model"),
		"--schedule-file", writeScheduleFile(t, cliSchedule),
	})
	idArg := strconv.FormatInt(id, 10)

	handwritten := `{"prompt": 0.5}`
	err := runCaptured(t, func() error {
		return runPriceUpdate([]string{
			"--id", idArg, "--expr", handwritten, "--by", by,
		})
	})
	if err == nil {
		t.Fatal("带表的规则直接改表达式应当被拒")
	}
	if !strings.Contains(err.Error(), "clear-schedule") {
		t.Errorf("错误信息里没有 --clear-schedule：%v", err)
	}

	// 显式清表之后同一条命令放行。
	_, _ = captureStreams(t, func() {
		if updErr := runPriceUpdate([]string{
			"--id", idArg, "--expr", handwritten, "--clear-schedule", "--by", by,
		}); updErr != nil {
			t.Fatalf("--clear-schedule 之后改表达式失败: %v", updErr)
		}
	})

	out, _ := captureStreams(t, func() {
		if showErr := runPriceShow([]string{idArg}); showErr != nil {
			t.Fatalf("price show 失败: %v", showErr)
		}
	})
	if !strings.Contains(out, "表达式：") || !strings.Contains(out, handwritten) {
		t.Errorf("清表之后 show 应当打表达式：\n%s", out)
	}
	if strings.Contains(out, "结构化定价表") {
		t.Errorf("清表之后 show 仍在打表：\n%s", out)
	}
}
