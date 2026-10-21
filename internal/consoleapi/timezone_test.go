package consoleapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAdminGroupsShareTheMetaLocation 钉住「站点时区只有一个来源」。
//
// /meta 报给前端的是 a.loc（前端全站的「今天」按它切），概览的「今天」
// 也必须是同一个 *time.Location——不是同名字符串、更不是再解析一遍。
// 空配置的回落只发生在 newAPI 那一处，adminapi 不再收字符串。
func TestAdminGroupsShareTheMetaLocation(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	a := &API{
		log:      testLogger(),
		loc:      loc,
		siteName: "larkax",
		baseURL:  "/v1",
		currency: "CNY",
	}

	cfg := a.adminConfig(Config{}, PathPrefix+"merchant/", nil)
	if cfg.Loc != a.loc {
		t.Fatalf("adminapi 的 Loc 与站点时区不是同一个对象：%p vs %p", cfg.Loc, a.loc)
	}

	rec := httptest.NewRecorder()
	a.handleMeta(rec, httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, PathPrefix+"meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("meta 状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("meta 响应不是 JSON: %v", err)
	}
	if body.Timezone != cfg.Loc.String() {
		t.Errorf("meta 的时区 = %q，概览用的是 %q——两个口径对不上",
			body.Timezone, cfg.Loc.String())
	}
}

// testLogger 给只用到 a.log 的用例一个不写盘的 logger。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
