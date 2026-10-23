package adminapi

import (
	"net/http"
	"testing"
	"time"
)

// 本文件钉住「目录组与商家组各自占哪几条路径」。不连库：这里只问
// 「装配得起来吗」与「哪条路径落在哪一组上」；返回值语义（全局目录含他人
// 的模型、商家视图只含自己名下）要真库，见 groups_integration_test.go。

const groupsPrefix = "/api/v1/merchant/"

// groupsSurface 按给定的组装一份 handler。
//
// 授权换成放行、Loc 固定给 UTC：这条用例问的是路由表，不是鉴权与切日。
func groupsSurface(t *testing.T, groups ...string) http.Handler {
	t.Helper()
	return New(Config{
		Prefix:    groupsPrefix,
		Authorize: func(next http.Handler) http.Handler { return next },
		Groups:    groups,
		Loc:       time.UTC,
	})
}

// routeStatus 用一个**碰库之前就失败**的请求问路由在不在。
//
// 只有两种结果可判：404 是 mux 说「这条路径没有路由」，400 是 handler 在读库
// 之前把参数拒掉了。全组都不连库，所以把 DSN 关掉时这条用例照样跑。
func routeStatus(t *testing.T, h http.Handler, method, path string, body any) int {
	t.Helper()
	return call(t, h, method, path, "", body).Code
}

// TestModelsAndOverviewGroupsCoexist 钉住 #1135：目录组与商家组同时列时装配
// 不能炸。
//
// 两组原先都注册 `GET <prefix>models`（目录组是全局目录，商家组是「我名下
// 在供的模型」）。单独列哪一组都正常，同时列时 net/http 在注册期 panic：
//
//	panic: pattern "GET /api/v1/merchant/models" (registered at ...) conflicts
//	with pattern "GET /api/v1/merchant/models" (registered at ...)
//
// 商家视图改名到 `<prefix>my-models` 之后这一对才能共存。三份装配分别问：
// 两组各自守着自己那条，同时列也互不遮挡。
func TestModelsAndOverviewGroupsCoexist(t *testing.T) {
	// 目录组那条的探针是 POST + 坏正文：decodeBody 在碰库之前回 400。
	// 商家组那条的探针是非法 limit：parseListLimit 在碰库之前回 400。
	const catalogProbe = http.MethodPost
	const merchantProbe = groupsPrefix + "my-models?limit=0"

	// 两组同列放在最前面：旧代码的那次 panic 就发生在这里的装配上，
	// 红要红在「同时列两组装不起来」这条断言上，而不是别的哪一条。
	t.Run("两组同列", func(t *testing.T) {
		both := groupsSurface(t, GroupModels, GroupOverview)
		if code := routeStatus(t, both, catalogProbe, groupsPrefix+"models", "not an object"); code != http.StatusBadRequest {
			t.Errorf("POST models 的状态码 = %d，期望 400（目录路由不在）", code)
		}
		if code := routeStatus(t, both, http.MethodGet, merchantProbe, nil); code != http.StatusBadRequest {
			t.Errorf("my-models 的状态码 = %d，期望 400（商家路由不在）", code)
		}
	})

	// 只列目录组：模型目录在，商家视图不在。
	t.Run("只有目录组", func(t *testing.T) {
		catalogOnly := groupsSurface(t, GroupModels)
		if code := routeStatus(t, catalogOnly, catalogProbe, groupsPrefix+"models", "not an object"); code != http.StatusBadRequest {
			t.Errorf("POST models 的状态码 = %d，期望 400（目录路由不在）", code)
		}
		if code := routeStatus(t, catalogOnly, http.MethodGet, merchantProbe, nil); code != http.StatusNotFound {
			t.Errorf("my-models 的状态码 = %d，期望 404（这一组不该有它）", code)
		}
	})

	// 只列商家组：商家视图在，旧路径 models 不在。
	//
	// 这一条是改名真正的行为变更：改名之前 `models` 在商家组里是有的。
	t.Run("只有商家组", func(t *testing.T) {
		merchantOnly := groupsSurface(t, GroupOverview)
		if code := routeStatus(t, merchantOnly, http.MethodGet, merchantProbe, nil); code != http.StatusBadRequest {
			t.Errorf("my-models 的状态码 = %d，期望 400（商家路由不在）", code)
		}
		if code := routeStatus(t, merchantOnly, http.MethodGet, groupsPrefix+"models", nil); code != http.StatusNotFound {
			t.Errorf("旧路径 models 的状态码 = %d，期望 404", code)
		}
	})
}
