package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/calendar"
	calendarstore "github.com/sumwai/larkax/internal/calendar/store"
)

// 导入成功时不会有任何错误输出，而「什么都没写」与「写成功了」在安静的终端里
// 长得一样（LESSONS.md 第 16 条）。所以输出里的每一个数都要被断言。
func TestPrintCalendarImportReportsEveryCount(t *testing.T) {
	out := captureStdout(t, func() {
		printCalendarImport(os.Stdout, calendarstore.Result{
			Region: "CN", Year: 2031, Status: calendar.YearSynced,
			DaysInYear: 365, DaysOutsideYear: 2, Replaced: 363,
		})
	})
	for _, want := range []string{"CN", "2031", "synced", "365", "2", "363"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里应当出现 %q，实际为 %q", want, out)
		}
	}
}

// 非 synced 的状态本来就不带数据行，「0 天」要有一句说明，
// 否则它看起来像数据被吞了。
func TestPrintCalendarImportExplainsZeroDays(t *testing.T) {
	out := captureStdout(t, func() {
		printCalendarImport(os.Stdout, calendarstore.Result{
			Region: "CN", Year: 2031, Status: calendar.YearPending,
		})
	})
	if !strings.Contains(out, "pending") {
		t.Errorf("输出里应当说明状态，实际为 %q", out)
	}
	if !strings.Contains(out, "不带数据行") {
		t.Errorf("0 天时要说明这是预期结果，实际为 %q", out)
	}
}

func TestRunCalendarRequiresSubcommand(t *testing.T) {
	if err := runCalendar(nil); err == nil {
		t.Error("没有子命令时应当报错")
	}
	if err := runCalendar([]string{"清理"}); err == nil {
		t.Error("未知子命令应当报错")
	}
}

func TestRunCalendarImportRequiresOneFile(t *testing.T) {
	if err := runCalendarImport(nil); err == nil {
		t.Error("没有文件参数时应当报错")
	}
	if err := runCalendarImport([]string{"a.json", "b.json"}); err == nil {
		t.Error("两个文件参数时应当报错")
	}
}

func TestRunCalendarImportReportsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "没有这个文件.json")
	err := runCalendarImport([]string{path})
	if err == nil {
		t.Fatal("文件不存在时应当报错")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("报错里要点出是哪个文件：%v", err)
	}
}

// 文件本身有问题时，命令必须在**连数据库之前**就失败。
//
// 这个用例跑在可能设了 LARKAX_DB_DSN 的环境里（CI 就是那样），所以它同时
// 证明了失败发生在碰库之前：真去连的话，导入会往测试库里写进一整年的数据。
func TestRunCalendarImportRejectsBadFileBeforeConnecting(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"键名写错", `{"region":"CN","year":2031,"status":"pending","offDay":true}`, "offDay"},
		{"不是 JSON", `这不是 JSON`, "解析"},
		{"状态认不出", `{"region":"CN","year":2031,"status":"published"}`, "status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "calendar.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("写测试文件失败: %v", err)
			}
			err := runCalendarImport([]string{path})
			if err == nil {
				t.Fatal("坏文件应当被拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里应当提到 %q，实际为 %v", tc.want, err)
			}
		})
	}
}
