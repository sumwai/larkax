package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// RequestID 是贯穿一次请求全链路的关联标识。
//
// 用字符串而不是整数：它会出现在日志、响应头、以及数据库的 text 列里，
// 跨进程与跨服务传递时字符串不需要约定字节序，也不会因为溢出而回绕。
type RequestID string

// requestIDKey 是本包在 context 中存放请求标识的私有键类型。
type requestIDKey struct{}

// requestIDBytes 是随机部分的字节数。
//
// 8 字节（64 位）足够：它的用途是在一段有限时间的日志里做关联，
// 而不是做全局唯一标识。按每天一亿请求算，碰撞概率低到可以忽略，
// 而 16 个十六进制字符在日志里读起来不费劲——32 个字符会挤占每行的宽度。
const requestIDBytes = 8

// HeaderRequestID 是客户端可以主动提供的关联标识。
//
// 接受它是有意为之：调用方想把 larkax 的日志与自己的日志对上时，
// 最自然的做法就是带一个 id 过来。但它只用于**关联**，不用于鉴权或
// 幂等——同一个 id 重复出现不会被视为同一次请求。
const HeaderRequestID = "x-request-id"

// fallbackSeq 是熵源不可用时的退化计数器。
var fallbackSeq atomic.Uint64

// NewRequestID 生成一个新的请求标识。
//
// 熵源不可用时退回递增计数而不是 panic：日志关联是辅助功能，
// 它坏掉不应该让服务无法处理请求。
func NewRequestID() RequestID {
	buf := make([]byte, requestIDBytes)
	if _, err := rand.Read(buf); err != nil {
		fallbackSeq.Add(1)
		return RequestID(fmt.Sprintf("nosec%012x", fallbackSeq.Load()))
	}
	return RequestID(hex.EncodeToString(buf))
}

// RequestIDFromHeader 读取客户端提供的标识，没有或不可用时生成一个。
//
// 客户端提供的值只有在「非空、不超过 128 字节、且全是可见 ASCII」时才被沿用；
// 只要有一项不满足就**整条丢弃**并重新生成，而不是清洗掉非法字符再保留其余。
// 不校验的话，一个带换行的 id 会被原样写进日志，从而伪造出额外的日志行——
// 日志注入是最容易被忽略的一类问题，因为日志本身被认为「只是输出」。
func RequestIDFromHeader(v string) RequestID {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 128 || !isVisibleASCII(v) {
		return NewRequestID()
	}
	return RequestID(v)
}

func isVisibleASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// WithRequestID 返回带请求标识的 context。
func WithRequestID(ctx context.Context, id RequestID) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext 取出 context 里的请求标识，没有则返回空。
//
// 返回空而不是生成一个新的：调用方拿到空值时应当意识到「这个位置不该没有 id」，
// 而静默生成一个会让缺失的埋点看起来一切正常。
func RequestIDFromContext(ctx context.Context) RequestID {
	if id, ok := ctx.Value(requestIDKey{}).(RequestID); ok {
		return id
	}
	return ""
}

// Middleware 为每个请求建立关联标识，并写进 context 与响应头。
//
// 顺序上它应当是最外层的一层：放在鉴权、限流之前，
// 这样那些中间件拒绝的请求也带着 id，能被排查到。
//
// 它只装请求标识，不装日志器：日志器是进程级的（见 FromContext 与
// AdoptAsDefault），逐请求变化的那一项只有标识。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromHeader(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, string(id))
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}
