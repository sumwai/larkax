// Package expiry 收「不过期」这一个口径的值与判断。
//
// 需要它的是两端：命令行（cmd/larkaxctl）往库里写这个远期值，控制台
// （internal/consoleapi）把两种落库表示读成同一个答案。不放进 internal/auth，
// 是因为那会让命令行为了一个常量拖进凭据与会话那一整包。
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
// 用 OnceValues 存 error、在 Forever 里 panic，而不是 OnceValue 直接 panic：
// sync.Once 在 f panic 之后仍把 done 置 1，OnceValue 于是只留下零值——服务端
// 每个请求的 panic 都被 net/http recover，结果是只有第一个请求崩，之后所有
// 调用静默返回 year 1，IsForever 对所有真实时刻都回 false。方向正好相反：
// 「全部不过期」会变成「全部过期」。存下 error 之后每次调用都会再 panic。
var forever = sync.OnceValues(func() (time.Time, error) {
	return time.Parse(time.RFC3339, ForeverRFC3339)
})

// Forever 返回 ForeverRFC3339 对应的时刻。
func Forever() time.Time {
	at, err := forever()
	if err != nil {
		// 值来自上面的常量；解析失败只可能是源码改错了。让它在每次用到时
		// 当场暴露，而不是把一件不可能发生的事铺成一条返回 error 的路径。
		panic("expiry: ForeverRFC3339 不是合法的 RFC3339 时间: " + err.Error())
	}
	return at
}

// IsForever 判断一个过期时刻是不是「不过期」。
//
// nil 也为真：auth.Key.ExpiresAt 的注释写着 nil 表示不过期，历史数据里的
// NULL 与远期值是同一件事，页面上也只该有一种说法。比较按 UTC 做，不受
// 数据库会话时区影响。
func IsForever(at *time.Time) bool {
	if at == nil {
		return true
	}
	return at.UTC().Equal(Forever())
}
