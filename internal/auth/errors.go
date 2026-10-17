package auth

import "errors"

// ErrUnauthenticated 是「凭证没能解析出用户」这一类失败的共同标识。
//
// 调用方用 errors.Is(err, ErrUnauthenticated) 判断该回 401 还是 5xx，
// 而不是去比对文案。这两件事必须分开：一次数据库抖动若被当成 401，
// 现场表现是「全站密钥突然失效」，而排查方向与「某把 key 真的失效了」
// 完全相反。
var ErrUnauthenticated = errors.New("认证失败")

// Reason 说明这次认证为什么失败。
//
// 它**不对外**：Error() 的文案对所有 Reason 都是同一句话，
// 于是即使调用方把 err.Error() 原样写进响应体，也不会泄露
// 「这个 key 存在但过期了」这类信息——而那正是试探者下一步要的东西。
//
// 但排查时它必须能区分：用户说「我的 key 不能用了」，
// 「库里没有这个哈希」与「这把 key 上周过期了」是两条完全不同的路径。
// 所以它有一个消费者：公共入口在回 401 的那一处按它分桶计数
// （larkax_auth_failures_total，见 internal/api 的 authFailureReason）。
// 不写日志——401 是攻击者可控的，拿垃圾 key 打就能刷日志、刷盘。
type Reason string

// 失败原因。取值会进指标的标签，改名等于改告警契约。
const (
	// ReasonMissingCredential 请求里没有带任何凭证。
	ReasonMissingCredential Reason = "missing_credential"
	// ReasonMalformedCredential 带了凭证但格式不对（如 Authorization 不是 Bearer）。
	ReasonMalformedCredential Reason = "malformed_credential"
	// ReasonKeyNotFound 哈希在 api_keys 里查不到这一行。
	ReasonKeyNotFound Reason = "key_not_found"
	// ReasonKeyDisabled key 已被停用。
	ReasonKeyDisabled Reason = "key_disabled"
	// ReasonKeyRevoked key 已被吊销。
	ReasonKeyRevoked Reason = "key_revoked"
	// ReasonKeyInactive key 的状态不是任何已知取值（约束之外的脏数据）。
	ReasonKeyInactive Reason = "key_inactive"
	// ReasonKeyExpired key 已过期。
	ReasonKeyExpired Reason = "key_expired"
	// ReasonUserInactive key 有效，但它的主人已被封禁或停用。
	ReasonUserInactive Reason = "user_inactive"
)

// Error 是一次认证失败。
//
// 它实现 error 与 Unwrap，于是 errors.Is(err, ErrUnauthenticated) 成立，
// 同时 errors.As(err, &authErr) 能把 Reason 取出来。
type Error struct {
	Reason Reason
}

// Error 的文案刻意不带 Reason——对外不区分，见 Reason 的说明。
func (e *Error) Error() string { return ErrUnauthenticated.Error() }

// Unwrap 把这一类失败归到 ErrUnauthenticated 上。
func (e *Error) Unwrap() error { return ErrUnauthenticated }
