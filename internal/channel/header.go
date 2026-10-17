package channel

// ValidHeaderValue 判断一个请求头的值是否符合 RFC 7230 的 field-value 文法。
//
// 允许的是可见字符（0x21–0x7E）、obs-text（0x80–0xFF）以及 SP 与 HTAB，
// **其余一律拒绝**。CR 与 LF 当然在其中（值里带换行等于能注入出额外的头），
// 但同样被拒的还有 NUL 这类 C0 控制字符与 DEL。
//
// # 为什么不能只挡 CR/LF
//
// 只挡换行的话，值里的控制字符会一路走到 net/http，由它在构造请求时才发现
// `invalid header field value`。那时失败的位置离原因很远：它表现为**上游失败**，
// 可能触发换渠道重试，而真正的原因是插件写了一个坏头。在这里拒掉，
// 插件作者拿到的就是「这个头被拒了」。
//
// # 为什么放在 channel 包
//
// 用它的是两处：插件补丁的校验（internal/plugin）与适配器最后那道过滤
// （internal/channel/httpx）。两处各写一份的话，改了一处另一处会静默留着旧规则，
// 而这是安全判据——两份不一致时的表现是「同一种值在一条路径上被拒、
// 在另一条上放行」。channel 是这两处共同的依赖，且它本身不带 HTTP 栈，
// 所以放在这里不会把 net/http 拖进只想用类型定义的地方。
func ValidHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == ' ' || c == '\t':
		case c >= 0x21 && c <= 0x7E:
		case c >= 0x80: // obs-text：非 ASCII 的历史遗留写法，RFC 允许
		default:
			return false
		}
	}
	return true
}
