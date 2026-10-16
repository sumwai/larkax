package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/config"
)

// worker 角色的公共端口是占位响应，而不是装配失败：它本来就不承接推理流量，
// 而起不来的进程连「端口通、TLS 通、负载均衡转发通」都验证不了。
func TestWorkerRoleServesPlaceholder(t *testing.T) {
	h, err := publicHandler(&config.Config{Role: config.RoleWorker}, nil, nil, nil)
	if err != nil {
		t.Fatalf("worker 角色不该装配失败: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("状态码 = %d，期望 501", rec.Code)
	}
	// 这个 501 要自己说清来由。与「功能还没接上」的 501 长得一样的话，
	// 看到它的人会去查功能，而真正的问题是部署把 worker 摆在了公共端口上。
	if !strings.Contains(rec.Body.String(), "worker") {
		t.Errorf("占位响应应当点名角色: %s", rec.Body)
	}
}

// 本该接推理流量的角色缺主密钥时必须**启动失败**，而不是静默退化成占位响应：
// 后者在编排系统看来是「起来了」，要等到第一波真实流量才暴露，而现场只是一个
// 与「未实现」无从区分的 501。
func TestTrafficRoleWithoutMasterKeyFailsToStart(t *testing.T) {
	for _, role := range []config.Role{config.RoleGateway, config.RoleControl} {
		t.Run(string(role), func(t *testing.T) {
			h, err := publicHandler(&config.Config{Role: role}, nil, nil, nil)
			if err == nil {
				t.Fatalf("%s 角色缺主密钥时应当装配失败，实际拿到 %T", role, h)
			}
			if !strings.Contains(err.Error(), "主密钥") {
				t.Errorf("错误信息应当说明缺的是主密钥: %v", err)
			}
		})
	}
}
