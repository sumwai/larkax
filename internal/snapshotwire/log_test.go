package snapshotwire

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/observe"
)

// captureLogger 把日志写进内存，供断言。
//
// 这类断言针对的是「**没出事时不要有日志**」与「出事时有日志」这一对：
// 前者只有断言了才成立——一个每次都记 warn 的实现，在只看屏幕上
// 什么都看不出来，而它会让人开始忽略这个日志。
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return observe.NewLoggerWithWriter(&buf, "debug", "json", false), &buf
}

// 日志器为 nil 时回落到默认，而不是让 NilLogger 在第一次记日志时 panic。
// nil 是「没传」这一种情况的正常写法，不该在运行到某个分支时才炸。
func TestNilLoggerFallsBackToDefault(t *testing.T) {
	s := NewStore(&fakeDB{}, nil)
	if s.logger == nil {
		t.Fatal("落库层的日志器不该是 nil")
	}
	if err := s.Insert(context.Background(), newSnap()); err != nil {
		t.Fatalf("Insert 出错: %v", err)
	}

	c, err := New(testConfig(), &fakeDB{}, NoConsentSource{}, nil)
	if err != nil {
		t.Fatalf("建链路出错: %v", err)
	}
	if c.logger == nil {
		t.Fatal("快照链路的日志器不该是 nil")
	}
	// 这条路径会经过并发满与落库失败两个记日志的分支。
	if err := c.Save(context.Background(), newSnap()); err != nil {
		t.Fatalf("Save 出错: %v", err)
	}
}

// 正常的写入不该留下任何警告。
//
// 每一次清理、每一次补建都刷一句 warn 的实现，在屏幕上与「一切正常」
// 无法区分，而代价是这个日志很快会被所有人忽略。
func TestSuccessfulInsertLogsNoWarning(t *testing.T) {
	logger, buf := captureLogger()
	s := NewStore(&fakeDB{}, logger)

	if err := s.Insert(context.Background(), newSnap()); err != nil {
		t.Fatalf("Insert 出错: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "WARN") || strings.Contains(out, "ERROR") {
		t.Errorf("补建成功时不该有警告，实际日志为 %q", out)
	}
}

// 补建分区失败时必须留下日志。
//
// 它与上一条是一对：只断言「成功时没日志」的话，一个永远不记日志的
// 实现也能通过——而那恰好是排障时最需要的信息。
func TestPartitionDDLFailureIsLogged(t *testing.T) {
	logger, buf := captureLogger()
	s := NewStore(&failFirstDB{fake: &fakeDB{}, failFirst: true}, logger)

	if err := s.Insert(context.Background(), newSnap()); err != nil {
		t.Fatalf("补建失败但插入成功时不该报错: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "补建") {
		t.Errorf("补建失败应当留下日志，实际为 %q", out)
	}
}

// 请求标识要跟着日志走：这条链路就是给排障用的，日志拼不上等于白记。
func TestLogCarriesRequestTagWhenPresent(t *testing.T) {
	logger, buf := captureLogger()
	c, err := New(testConfig(), &fakeDB{execErr: errors.New("库挂了")},
		NoConsentSource{}, logger)
	if err != nil {
		t.Fatal(err)
	}

	ctx := observe.WithRequestID(context.Background(), "req-from-ctx")
	if err := c.Save(ctx, newSnap()); err == nil {
		t.Fatal("落库失败时应当报错")
	}
	if out := buf.String(); !strings.Contains(out, "req-from-ctx") {
		t.Errorf("日志里应当带上 context 里的请求标识，实际为 %q", out)
	}

	// 没有标识时不硬塞一个空值进去：多一个恒为空的字段会让「这条日志
	// 来自请求链路之外」这件事变得看不出来。
	buf.Reset()
	if err := c.Save(context.Background(), newSnap()); err == nil {
		t.Fatal("落库失败时应当报错")
	}
	if out := buf.String(); strings.Contains(out, `"request_id":""`) {
		t.Errorf("没有请求标识时不该写出空的 request_id，实际为 %q", out)
	}
}
