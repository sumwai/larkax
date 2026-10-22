package upstream

// 本文件是「探测上游模型清单」用的那一份装配。
//
// 它与 New / Registry 是同一组适配器，差别只在客户端：探测打的是**用户
// 填的地址**，所以不跟随重定向、响应体有上限、整体超时短（见
// httpx.ProbeClient）。运行期执行器一个字节都不改——那边跟随重定向是
// net/http 的默认行为，而那条路径打的是我们自己配好的上游。

import (
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
)

// ProbeTimeout 是一次上游模型探测的整体上限。
//
// 探测是给人等的一次短动作：拿不到清单就退回手填。取十秒是因为跨洲链路
// 建连加一次列表请求通常在一秒级，十秒留足余量；再长就是在让一个填错的
// 地址把表单卡住。
const ProbeTimeout = 10 * time.Second

// ProbeRegistry 返回探测用的注册表。
//
// 与 Registry 同一份适配器清单（见 builtinAdapters）：类型枚举与写渠道时
// 的校验读的都是它，所以「探测认得的渠道类型」不会与「服务认得的类型」
// 分叉。不同的是每个适配器拿到的是 ProbeClient——不跟随重定向、响应体上限
// 8 MiB、整体超时 ProbeTimeout。
//
// 四个适配器共用一个客户端：连接池是并发安全的，拆开只会多几份空闲连接。
func ProbeRegistry() (*channel.Registry, error) {
	return probeRegistry(probeClient())
}

// probeClient 是探测用的 HTTP 客户端。
//
// 单独一层是为了让用例能钉住「ProbeTimeout 真的进了这个客户端」，而不必
// 等满十秒去观察一次超时。
func probeClient() *http.Client {
	return httpx.ProbeClient(ProbeTimeout)
}

// probeRegistry 用给定的客户端装配探测用的注册表。
//
// 注入客户端是为用例留的口子：本地慢上游配一个短时限的客户端，就能证明
// 客户端确实交给了适配器，而不是在装配里另造了一个默认的。
func probeRegistry(client *http.Client) (*channel.Registry, error) {
	executor, err := newWith(builtinAdapters(func() *http.Client { return client })...)
	if err != nil {
		return nil, err
	}
	return executor.registry, nil
}
