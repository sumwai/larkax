package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/sumwai/larkax/internal/ratelimit"
)

// PathRateLimitUsage 是限流用量那条只读查询的路径。
//
// 它挂在**内部监听**上（LARKAX_INTERNAL_ADDR，与 /healthz、/readyz、/metrics
// 同一个端口），不在公共入口上。理由与 /metrics 相同：它回答的是内部状态，
// 而公共端口承接的是用户的推理流量——把内部状态挂到那里，等于给每一个能发
// 推理请求的人一个查询别人 Key 用量的接口。
//
// 路径取 /ratelimit/usage 而不是 /ratelimit：这条路径只读，名字里带着 usage
// 就没有歧义。改限额、清空计数、临时封禁这三件事**不做**——它们各自是能绕开
// 或加重限流的权力，要单开一条并带审计（见 docs/limits.md）。
const PathRateLimitUsage = "/ratelimit/usage"

// RateLimitUsageSource 是只读的用量查询口。
//
// 与 Authenticator / RateLimiter 同一个做法：本包只声明自己要什么，具体实现
// （internal/ratelimit 的 Limiter）不必 import 本包。接口里只有一个读方法，
// 是因为这条路径上应该发生的事只有这一件。
type RateLimitUsageSource interface {
	Usage(ctx context.Context, keyID int64) ratelimit.Usage
}

// RegisterRateLimitUsage 把这条只读查询挂到 mux 上。
//
// 由装配层在内部监听那一侧调用（见 internal/assembly 的 RegisterInternalOps）。
// 本包不 import 装配，所以这里只提供挂载点，由调用方决定挂在哪。
func RegisterRateLimitUsage(mux *http.ServeMux, source RateLimitUsageSource) {
	mux.Handle(PathRateLimitUsage, rateLimitUsageHandler(source))
}

// rateLimitUsageHandler 处理 GET /ratelimit/usage?key_id=<id>。
//
// 入参只有 key_id。**不**接受 rpm：额度在 api_keys.rate_limit_rpm 里，要
// 查库才能拿到，而这条路径不连数据库（见 docs/limits.md 里两条各自怎么查）。
// 把额度当参数传进来也走得通，但那会让「谁说了算」出现第二个地方——同一把
// key 在库里是一个值、在这里可以是另一个值，而这正是这一路上都在避免的那类
// 问题（拿到与解释是两件事，解释只有一处）。
func rateLimitUsageHandler(source RateLimitUsageSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 只接受 GET 与 HEAD。放行别的等于凭空多出一条写路径——与
		// /metrics 同一个取舍，虽然这里本来也没有会写东西的分支。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed,
				jsonBody(map[string]any{"error": "这条路径只读，只接受 GET 与 HEAD"}))
			return
		}

		raw := r.URL.Query().Get("key_id")
		keyID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || keyID <= 0 {
			// 坏参数回 400，而不是回一个空的用量：后者会让「这个 key 这一分钟
			// 没被打过」与「key_id 写错了」在输出里长得一样。
			writeJSON(w, http.StatusBadRequest,
				jsonBody(map[string]any{"error": "key_id 必须是正整数（api_keys.id）"}))
			return
		}

		u := source.Usage(r.Context(), keyID)
		writeJSON(w, http.StatusOK, jsonBody(rateLimitUsageBody{
			KeyID:       u.KeyID,
			WindowStart: u.WindowStart.UTC().Format(time.RFC3339),
			WindowEnd:   u.WindowEnd.UTC().Format(time.RFC3339),
			Count:       u.Count,
			Exact:       u.Exact,
		}))
	}
}

// rateLimitUsageBody 是这条路径的响应体。
//
// 时间用 RFC3339 字符串而不是 Unix 秒：看这个数的人要拿它与日志里的时间
// 对照，而窗口起点是整分钟，RFC3339 读出来就是那个分钟。
type rateLimitUsageBody struct {
	KeyID       int64  `json:"key_id"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	Count       int64  `json:"count"`
	// Exact 为 true 表示这个数取自共享存储（所有实例共用那一份），
	// false 表示只是这个实例记的那一份。两者的含义见 internal/ratelimit。
	Exact bool `json:"exact"`
}

// jsonBody 序列化一个响应体。
//
// 与 error.go 里那次 Marshal 同一个理由：这里的值都是本包自己拼的（字符串、
// 整数、布尔），序列化不会失败，所以没有一条「序列化失败」的分支——那种分支
// 永远测不到，而它一旦真的存在，说明结构体里混进了不能编码的东西，那该在改
// 结构体的那一次被发现。
func jsonBody(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
