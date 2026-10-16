package main

import (
	"strings"
	"testing"
)

// 一个都没删时也要有明确输出。
//
// 「没有到期分区」与「命令没跑成」在安静的终端里长得一样（LESSONS.md 第 16 条），
// 所以这里断言的是**有一行输出**，而不只是「没报错」。
func TestPrintPartitionsEmptyIsExplicit(t *testing.T) {
	out := captureStdout(t, func() {
		printPartitions("已删除", nil)
	})
	if !strings.Contains(out, "0 个") {
		t.Errorf("空清单的输出应说明是 0 个，实际为 %q", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("空清单不该什么都不输出")
	}
}

func TestPrintPartitionsListsEveryName(t *testing.T) {
	names := []string{"request_snapshots_2026_09_01", "request_snapshots_2026_09_02"}
	out := captureStdout(t, func() {
		printPartitions("将删除", names)
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
