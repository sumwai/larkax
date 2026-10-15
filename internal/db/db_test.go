package db

import (
	"context"
	"strings"
	"testing"
)

// 连不上就返回错误，并且**不交出连接池句柄**。
// 返回一个非 nil 的 handle 会让调用方以为可以继续用，
// 而它后面是一条永远连不上的连接。
func TestOpenFailsWithoutLeakingHandle(t *testing.T) {
	handle, err := Open(context.Background(), "postgres://user:placeholder@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	if err == nil {
		handle.Close()
		t.Fatal("连不上时应报错")
	}
	if handle != nil {
		t.Errorf("出错时不应返回句柄，实际拿到 %v", handle)
	}
}

// DSN 写错与数据库没起来是两回事，排查方向完全不同：
// 前者要看配置，后者要看服务。错误信息里必须留下原始原因供区分。
func TestOpenKeepsUnderlyingCause(t *testing.T) {
	_, err := Open(context.Background(), "://not a dsn")
	if err == nil {
		t.Fatal("非法 DSN 应报错")
	}
	if !strings.Contains(err.Error(), "解析") && !strings.Contains(err.Error(), "parse") {
		t.Errorf("错误信息没留下原始原因：%v", err)
	}
}
