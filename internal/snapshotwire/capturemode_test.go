package snapshotwire

import (
	"context"
	"strings"
	"testing"
)

// 采集档原样落进 capture_mode 那一列。
//
// 只断言「插入语句里有 capture_mode」是不够的：档与正文必须同一档，
// 而参数位错一格时 SQL 照样执行（列都可空），表现成一行「正文是原文、
// 档写着骨架」的快照——库里读出来时它看起来完全正常。
func TestInsertWritesFullConsentedMode(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	snap := newSnap()
	snap.CaptureMode = captureModeFullConsented
	snap.RedactedPaths = []string{}
	if err := s.Insert(context.Background(), snap); err != nil {
		t.Fatalf("Insert 出错: %v", err)
	}

	if got := db.lastArgs()[4]; got != captureModeFullConsented {
		t.Errorf("capture_mode = %v，期望 %v", got, captureModeFullConsented)
	}
}

// 空串按**默认那一档**落库，不按原样那一档。
//
// 方向是这条的性质所在：两个取值里只有 skeleton 是不落原文的那个，所以认不出
// 时只能往它回落。反过来（空串落成全文）会让「没特别说明」变成一次泄露，
// 而它不会以任何故障的形式出现——那一行的正文看着正常，只是内容是用户原文。
//
// Snapshot 有几个构造点（脱敏器之外，测试与别的写入方也直接拼），空值在
// 那些地方表示「没特别说明」，所以这里不能把它当错误。
func TestInsertTreatsEmptyCaptureModeAsSkeleton(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	snap := newSnap()
	if snap.CaptureMode != "" {
		t.Fatalf("这条用例的前提是采集档为空，实际 %q", snap.CaptureMode)
	}
	if err := s.Insert(context.Background(), snap); err != nil {
		t.Fatalf("Insert 出错: %v", err)
	}
	if got := db.lastArgs()[4]; got != captureModeSkeleton {
		t.Errorf("capture_mode = %v，期望回落到 %v", got, captureModeSkeleton)
	}
}

// 认不出的取值在写之前就报出来，且一个字都不写。
//
// 表上有 CHECK 兜着，但它的报错只说得出是哪一列，说不出那个词是什么——
// 而「哪个写入方造了一个新取值」正是这条要回答的。等 CHECK 拒收的话，
// 调用方拿到的是一句「违反检查约束 request_snapshots_capture_mode_check」，
// 而日志里看不到到底传了什么。
//
// 这里判的是**没有 INSERT**：只有一句错误、INSERT 照样发出去的实现，
// 会在真库上撞 CHECK 失败，而那一条失败与重试逻辑、连接问题混在一起。
func TestInsertRejectsUnknownCaptureMode(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	snap := newSnap()
	snap.CaptureMode = "verbatim"
	err := s.Insert(context.Background(), snap)
	if err == nil {
		t.Fatal("认不出的采集档应当报错")
	}
	if !strings.Contains(err.Error(), "verbatim") {
		t.Errorf("报错里要带上那个认不出的取值，实际为 %q", err)
	}
	for _, q := range db.execs {
		if strings.Contains(q, "INSERT") {
			t.Errorf("认不出的采集档不该发出 INSERT，实际执行了 %q", q)
		}
	}
}
