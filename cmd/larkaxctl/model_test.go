package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/catalog"
)

// TestModelAddRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestModelAddRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --canonical", []string{"--display", "展示名"}, "缺少 --canonical"},
		{"没有 --display", []string{"--canonical", "m1"}, "缺少 --display"},
		{"日期不是 YYYY-MM-DD", []string{
			"--canonical", "m1", "--display", "展示名",
			"--knowledge-cutoff", "2026/01/31",
		}, "YYYY-MM-DD"},
		{"多出位置参数", []string{"--canonical", "m1", "--display", "展示名", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runModelAdd(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
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

// TestModelAddThenList 钉住规范模型的写入与列举。
//
// 写进去的值直接从读取路径读回来核对——命令行这一层只是把字符串变成包要的
// 结构，所以「写对了」这件事只能由「读出来对得上」证明。
func TestModelAddThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	display := canonical + "-展示名"

	stdout, _ := captureStreams(t, func() {
		if err := runModelAdd([]string{
			"--canonical", canonical, "--display", display,
			"--vendor", "example-vendor", "--family", "example-family",
			"--context-limit", "200000", "--output-limit", "64000",
			"--tools", "--vision",
			"--modalities-input", "text,image", "--modalities-output", "text",
			"--knowledge-cutoff", "2026-01-31",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是模型 ID：%q（%v）", stdout, err)
	}

	got, err := catalog.New(db).Model(context.Background(), canonical)
	if err != nil {
		t.Fatalf("读回刚写进去的模型失败: %v", err)
	}
	if got.ID != id {
		t.Errorf("读回来的 ID = %d，期望 %d", got.ID, id)
	}
	if got.DisplayName != display {
		t.Errorf("展示名 = %q，期望 %q", got.DisplayName, display)
	}
	if got.Vendor != "example-vendor" || got.Family != "example-family" {
		t.Errorf("厂商/家族 = %q/%q", got.Vendor, got.Family)
	}
	if got.ContextLimit != 200000 || got.OutputLimit != 64000 {
		t.Errorf("上下文/输出上限 = %d/%d", got.ContextLimit, got.OutputLimit)
	}
	// 没给 --input-limit：0 表示「未知」，落库为 NULL，读回来还是 0。
	if got.InputLimit != 0 {
		t.Errorf("没填的输入上限应当是 0（未知），得到 %d", got.InputLimit)
	}
	if !got.Capabilities.Tools || !got.Capabilities.Vision {
		t.Errorf("tools/vision 应当为真：%+v", got.Capabilities)
	}
	if got.Capabilities.Reasoning || got.Capabilities.Audio || got.Capabilities.Cache || got.Capabilities.Batch {
		t.Errorf("没给的能力不该为真：%+v", got.Capabilities)
	}
	if len(got.ModalitiesInput) != 2 || got.ModalitiesInput[0] != "text" || got.ModalitiesInput[1] != "image" {
		t.Errorf("输入模态 = %v", got.ModalitiesInput)
	}
	if got.KnowledgeCutoff == nil || got.KnowledgeCutoff.Format("2006-01-02") != "2026-01-31" {
		t.Errorf("知识截止日期 = %v", got.KnowledgeCutoff)
	}

	listOut, _ := captureStreams(t, func() {
		if listErr := runModelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	for _, want := range []string{canonical, display, "example-vendor", "tools,vision", "text,image→text"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("清单里没有 %q：\n%s", want, listOut)
		}
	}
}

// TestModelAddRejectsDuplicateCanonical 钉住重复的规范名。
//
// 它是选路与计费共用的键，重复的键会让「这次请求按哪个模型算」没有答案，
// 所以必须撞在唯一键上，而不是静默地多出一行。
func TestModelAddRejectsDuplicateCanonical(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	add := func() error {
		return runCaptured(t, func() error {
			return runModelAdd([]string{"--canonical", canonical, "--display", "展示名"})
		})
	}

	if err := add(); err != nil {
		t.Fatalf("第一次 add 应当成功: %v", err)
	}
	err := add()
	if err == nil {
		t.Fatal("重复的规范名应当被拒")
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("重复的规范名是约束冲突，退出码应当是 %d，得到 %d（%v）", exitConflict, got, err)
	}
}
