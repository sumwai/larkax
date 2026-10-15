package observe

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReadinessCheck 是一项就绪依赖的探针。
//
// 只关心「能不能服务请求」，不关心细节：慢依赖由各自的超时与指标暴露，
// 就绪接口必须快速返回，否则编排系统会在这里排队。
type ReadinessCheck struct {
	Name  string
	Check func(context.Context) error
}

// readyzTimeout 是单次就绪检查的总超时。
const readyzTimeout = 2 * time.Second

// RegisterHealthHandlers 注册存活与就绪两个端点。
//
// 两个端点含义不同，不能合并：
//   - /healthz 回答「进程还活着吗」。只要 HTTP 服务能响应就是 200，
//     绝不检查下游依赖——数据库抖一下就重启进程，只会让故障扩散。
//   - /readyz 回答「能接流量了吗」。任一依赖不可用即 503，
//     让负载均衡把实例摘出去，而不是让请求打进来再失败。
func RegisterHealthHandlers(mux *http.ServeMux, checks []ReadinessCheck) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
		defer cancel()

		results := make(map[string]string, len(checks))
		ready := len(checks) > 0

		for _, c := range checks {
			if err := c.Check(ctx); err != nil {
				results[c.Name] = err.Error()
				ready = false
				continue
			}
			results[c.Name] = "ok"
		}

		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"ready": ready, "checks": results})
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
