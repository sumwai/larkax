// Package expiry 收「不过期」这一个口径的值与判断。
//
// 需要它的是两端：命令行（cmd/larkaxctl）往库里写这个远期值，控制台
// （internal/consoleapi）把两种落库表示读成同一个答案。不放进 internal/auth，
// 是因为那里的职责是鉴权与会话，不是「过期时刻」这条时间口径；而且这个值
// 同时被命令行与控制台用，放进凭据包会让一条纯时间规则依附在鉴权上。
// 本包不依赖任何内部包，谁都能引。
package expiry

import (
	"sync"
	"time"
)

// ForeverRFC3339 是「永久」写进时间戳列的那一刻。
//
// 只有这一份：--forever 与 apikey 上的旧名 --clear-expires-at 都写它。库里
// 的类型不变，仍然是一个时间戳；「永久」是选定的一个远期值，不是另一个状态。
const ForeverRFC3339 = "9999-12-31T23:59:59Z"

// forever 只解析一次。
//
// 不放在 init 里：一个常量笔误不该让整个二进制起不来。放到第一次用到时，
// 影响面就收窄到真正读这个值的那条路径。
//
// OnceValue 在 f panic 之后每次调用都重抛同一个值（标准库保证），所以不必
// 自己存 error。
var forever = sync.OnceValue(func() time.Time {
	at, err := time.Parse(time.RFC3339, ForeverRFC3339)
	if err != nil {
		// 值来自上面的常量；解析失败只可能是源码改错了。让它在每次用到时
		// 当场暴露，而不是把一件不可能发生的事铺成一条返回 error 的路径。
		panic("expiry: ForeverRFC3339 不是合法的 RFC3339 时间: " + err.Error())
	}
	return at
})

// Forever 返回 ForeverRFC3339 对应的时刻。
func Forever() time.Time {
	return forever()
}

// IsForever 判断一个过期时刻是不是「不过期」。
//
// nil 也为真：auth.Key.ExpiresAt 的注释写着 nil 表示不过期，历史数据里的
// NULL 与远期值是同一件事，页面上也只该有一种说法。比较按时刻做（Equal），
// 与两边时区字段的写法无关，所以同一个瞬间换成别的时区也判为真；.UTC()
// 只是把参与比较的值写成同一种形式，去掉它结论不变。
func IsForever(at *time.Time) bool {
	if at == nil {
		return true
	}
	return at.UTC().Equal(Forever())
}
