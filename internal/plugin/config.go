package plugin

import (
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
	"github.com/sumwai/larkax/internal/settings"
)

// 四个配置项的键名。
//
// 与迁移里插入的键逐字一致（见 internal/migrate/sql/00003_plugin.sql），
// 也与 settings.Registry 里声明的一致。写成常量而不是散在各处的字面量：
// 键名写错在运行期的表现是「启动就失败」，而那样一眼看不出是拼写问题。
const (
	KeyEnabled          = "plugin.enabled"
	KeyCallTimeoutMS    = "plugin.call_timeout_ms"
	KeyMemoryLimitBytes = "plugin.memory_limit_bytes"
	KeyMaxPayloadBytes  = "plugin.max_payload_bytes"
)

// Config 是插件链路的运行期配置，全部来自 settings 表。
//
// 四项都不是环境变量：它们不参与连接数据库，而且必须与数据同生共死——
// 换机器、恢复备份时，「插件跑在多大限额下」这条承诺要跟着数据一起回来。
type Config struct {
	// Enabled 为 false 时整条链路不装配：不读表、不构造注册表。
	Enabled bool
	// Limits 是每次调用的限额，请求路径建会话时用它。
	Limits sandbox.Limits
}

// call_timeout_ms 允许的范围（毫秒）。
//
// 两个界都在**换算之前**判。time.Duration 以纳秒计，毫秒数乘 1e6 会溢出，
// 而 Go 的整数溢出不报错——它绕回一个看起来正常的数，于是插件以一个谁也
// 没配过的时限运行。这与 internal/assembly 的 quotaStaleAfter 是同一处陷阱，
// 判法也一样：判配置里那个数本身，不判换算之后的结果。
//
// 上界一小时远超任何合理的 hook 时限（默认 5 秒，一次请求的预算大部分要给
// 上游），同时把溢出这条整个堵死。下界 1 毫秒：0 与负数在语义上不成立，
// 而 sandbox.Limits 把它们当作「没设」回落到默认值——那会让「配成 0」与
// 「配成默认值」在库里分不开。
const (
	minCallTimeoutMS = 1
	maxCallTimeoutMS = 60 * 60 * 1000
)

// LoadConfig 从已加载的 settings 里读出插件相关配置。
//
// # 关掉开关时不读另外三项
//
// 与 internal/snapshotwire 的 LoadConfig 同一条理由：关了就不跑插件，限额填错
// 也不会有后果，此时拒绝启动只会让「先关掉再说」这条退路也走不通。判据在
// internal/assembly 的装配用例里——plugin.enabled 为 false 时，把
// call_timeout_ms 配成一个非法值，服务照样起得来。
//
// 反过来，开着的时候非法值一律返回错误而不回落：回落会让「配错了」表现为
// 「插件跑在另一套限额下」，而那种差异只有在出问题时才看得出来。
func LoadConfig(v *settings.Values) (Config, error) {
	if v == nil {
		return Config{}, fmt.Errorf("settings 为空，无法读取插件配置")
	}

	enabled, err := v.Bool(KeyEnabled)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Enabled: enabled}
	if !enabled {
		return cfg, nil
	}

	timeoutMS, err := v.Int(KeyCallTimeoutMS)
	if err != nil {
		return Config{}, err
	}
	if timeoutMS < minCallTimeoutMS || timeoutMS > maxCallTimeoutMS {
		return Config{}, fmt.Errorf("settings.%s = %d 毫秒，超出允许范围 [%d, %d] 毫秒",
			KeyCallTimeoutMS, timeoutMS, minCallTimeoutMS, maxCallTimeoutMS)
	}

	memory, err := v.Int(KeyMemoryLimitBytes)
	if err != nil {
		return Config{}, err
	}
	if memory <= 0 {
		return Config{}, fmt.Errorf("settings.%s 必须为正数，当前为 %d", KeyMemoryLimitBytes, memory)
	}

	payload, err := v.Int(KeyMaxPayloadBytes)
	if err != nil {
		return Config{}, err
	}
	if payload <= 0 {
		return Config{}, fmt.Errorf("settings.%s 必须为正数，当前为 %d", KeyMaxPayloadBytes, payload)
	}

	cfg.Limits = sandbox.Limits{
		CallTimeout: time.Duration(timeoutMS) * time.Millisecond,
		MemoryBytes: memory,
		// 一个键管两头：迁移里那一行的说明就是「入参与出参字节上限」。
		// 拆成两个键的话，两个数不一致时「插件能收多大的请求」与
		// 「它能回多大的结果」会各说各话，而这两件事在契约上是一件事
		//（见 sandbox.Limits 上 MaxInputBytes 的注释：它必须单独存在，
		// 因为内存上限管不住宿主塞进去的值）。
		MaxInputBytes:  int(payload),
		MaxOutputBytes: int(payload),
	}
	return cfg, nil
}
