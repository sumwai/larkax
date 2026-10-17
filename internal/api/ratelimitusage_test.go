package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ratelimit"
)

// usageMux 按生产那条装配路径挂上只读查询（见 cmd/larkax 的 newInternalHandler）。
func usageMux(t *testing.T, source RateLimitUsageSource) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	RegisterRateLimitUsage(mux, source)
	return mux
}

// doUsage 打一次那条路径。
func doUsage(t *testing.T, mux *http.ServeMux, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, nil))
	return rec
}

// TestRateLimitUsageReportsWindowAndCount 钉住这条路径对外给出的形状。
//
// 时间用 RFC3339 而不是 Unix 秒：看这个数的人要拿它与日志里的时间对照，
// 而窗口起点是整分钟，读出来就该是那个分钟。
func TestRateLimitUsageReportsWindowAndCount(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)
	limiter := ratelimit.New(func() time.Time { return now })
	limit := 2
	limiter.Allow(7, &limit)
	limiter.Allow(7, &limit)
	limiter.Allow(7, &limit) // 第 3 次被拒，但计数照记（见 internal/ratelimit 的包注释）

	rec := doUsage(t, usageMux(t, limiter), http.MethodGet, PathRateLimitUsage+"?key_id=7")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；正文: %s", rec.Code, rec.Body)
	}

	var got struct {
		KeyID       int64  `json:"key_id"`
		WindowStart string `json:"window_start"`
		WindowEnd   string `json:"window_end"`
		Count       int64  `json:"count"`
		Exact       bool   `json:"exact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}

	// 逐字段比而不是比整个结构体：字段多起来时，整结构体比较的失败信息
	// 只说「不相等」，看不出是哪一个字段变了。
	if got.KeyID != 7 {
		t.Errorf("key_id = %d，期望 7", got.KeyID)
	}
	if got.WindowStart != "2026-10-17T12:00:00Z" || got.WindowEnd != "2026-10-17T12:01:00Z" {
		t.Errorf("窗口 = [%s, %s)，期望 [2026-10-17T12:00:00Z, 2026-10-17T12:01:00Z)",
			got.WindowStart, got.WindowEnd)
	}
	if got.Count != 3 {
		t.Errorf("count = %d，期望 3（含被拒的那一次）", got.Count)
	}
	// 没有共享存储时这个数是近似的，而响应里要说出来——运维据此知道
	// 「这个数只是这个实例看到的」。真的精确那一档在 internal/ratelimit 里测。
	if got.Exact {
		t.Error("exact = true，但没有共享存储，这个数只是本进程看到的那一份")
	}
}

// TestRateLimitUsageIsReadOnlyOnRepeatedCalls 钉住「查多少次都不改计数」。
//
// 一个会顺手记一次的实现也能返回一个数，而它会让「看看用量」变成「把自己
// 算进额度里」——这条断言是这一层唯一的防线。
func TestRateLimitUsageIsReadOnlyOnRepeatedCalls(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)
	limiter := ratelimit.New(func() time.Time { return now })
	limit := 10
	limiter.Allow(7, &limit)

	mux := usageMux(t, limiter)
	for i := 0; i < 5; i++ {
		rec := doUsage(t, mux, http.MethodGet, PathRateLimitUsage+"?key_id=7")
		var got struct {
			Count int64 `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应体不是 JSON: %v", err)
		}
		if got.Count != 1 {
			t.Fatalf("第 %d 次查询读到 %d，期望 1", i+1, got.Count)
		}
	}

	// 额度 10：查询没有把自己算进去，这个 key 还能继续打。
	for i := 0; i < 9; i++ {
		if ok, _ := limiter.Allow(7, &limit); !ok {
			t.Fatalf("第 %d 次被拒：只读查询把自己算进额度里了", i+2)
		}
	}
}

// TestRateLimitUsageRejectsBadInput 钉住坏参数回 400。
//
// 回一个空的用量会让「这个 key 这一分钟没被打过」与「key_id 写错了」在
// 输出里长得一样，而这两件事的处置完全不同。
func TestRateLimitUsageRejectsBadInput(t *testing.T) {
	limiter := ratelimit.New(nil)
	mux := usageMux(t, limiter)

	for _, tc := range []struct{ name, target string }{
		{"缺参数", PathRateLimitUsage},
		{"空值", PathRateLimitUsage + "?key_id="},
		{"不是数字", PathRateLimitUsage + "?key_id=abc"},
		{"零", PathRateLimitUsage + "?key_id=0"},
		{"负数", PathRateLimitUsage + "?key_id=-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doUsage(t, mux, http.MethodGet, tc.target)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("状态码 = %d，期望 400；正文: %s", rec.Code, rec.Body)
			}
			// 回的是 JSON 错误体而不是空体：空体在 curl 下看起来像「成功但没数据」。
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

// TestRateLimitUsageIsGetOnly 钉住这条路径上没有写操作。
//
// 这一层现在只有查用量一件事，所以把别的方法挡在外面是形式主义——但它挡的是
// 以后加进来的东西：这条路径的契约就是只读，方法表是它对外最直接的表达。
func TestRateLimitUsageIsGetOnly(t *testing.T) {
	mux := usageMux(t, ratelimit.New(nil))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			rec := doUsage(t, mux, method, PathRateLimitUsage+"?key_id=1")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s 的状态码 = %d，期望 405", method, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("Allow = %q，期望 %q", allow, "GET, HEAD")
			}
		})
	}

	// HEAD 与 GET 一样放行（抓取器与 curl -I 都用它）。
	rec := doUsage(t, mux, http.MethodHead, PathRateLimitUsage+"?key_id=1")
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD 的状态码 = %d，期望 200", rec.Code)
	}
}

// TestRateLimitUsageIsNotOnThePublicEntry 钉住这条路径**不在**公共入口上。
//
// 这是这条入口最要紧的一条性质：公共端口承接的是用户的推理流量，把内部状态
// 挂在那里，等于给每一个能发推理请求的人一个查询别人 Key 用量的接口。
//
// 判据是 501（公共入口对未知路径的答复），而不是 404：公共入口不分派到
// ServeMux，它自己按路径开关，未知路径一律 501。
func TestRateLimitUsageIsNotOnThePublicEntry(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, PathRateLimitUsage+"?key_id=1", nil))

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("公共入口上 %s 的状态码 = %d，期望 501（它只该在内部监听上）",
			PathRateLimitUsage, rec.Code)
	}
}
