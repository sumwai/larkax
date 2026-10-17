package snapshotwire

import (
	"fmt"

	"github.com/sumwai/larkax/internal/settings"
)

// 三个配置项的键名。
//
// 与迁移里插入的键逐字一致（见 internal/migrate/sql/00002_settings.sql），
// 也与 settings.Registry 里声明的一致。写成常量而不是散在各处的字面量：
// 键名写错在运行期的表现是「启动就失败」，而那样一眼看不出是拼写问题。
const (
	KeyEnabled       = "request_snapshot.enabled"
	KeyMaxBytes      = "request_snapshot.max_bytes"
	KeyRetentionDays = "request_snapshot.retention_days"
)

// Config 是快照链路的运行期配置，全部来自 settings 表。
//
// 三项都不是环境变量：它们不参与连接数据库，而且必须与数据同生共死——
// 换机器、恢复备份时，「留存期是 30 天」这条承诺要跟着数据一起回来。
type Config struct {
	// Enabled 为 false 时这条链路不采集：不脱敏、不写新快照。
	// 存量分区的清理照旧执行——它不看这个开关（见 Collector.DropExpired）。
	Enabled bool
	// MaxBytes 是单条骨架的字节上限，超限降级为摘要层。
	// 只在 Enabled 为 true 时有意义，那时它必须为正。
	MaxBytes int
	// RetentionDays 是分区留存天数，到期整分区 DROP。
	// 采集关掉时它照样有用：清理存量分区按它算到期线。
	RetentionDays int
}

// LoadConfig 从已加载的 settings 里读出快照相关配置。
//
// # 关掉采集时仍然读留存期
//
// retention_days 在关闭采集时也有后果：存量分区照旧按它清理（见
// Collector.DropExpired）。所以它必须读出来、也必须校验——「关了就不采集，
// 留存期填错也不会有后果」在这条链路上不再成立，而它一旦不成立，静默留一个
// 0 的后果就是「清理什么也不删，却报成功」，那正是 #292 的形状。
//
// max_bytes 仍然只在开启时校验：关了就没有骨架会经过它，那时拒绝启动
// 只会让「先关掉再说」这条退路也走不通。
//
// 非法值一律返回错误而不回落：
//
//   - max_bytes 非正 → 每条骨架都会降级成摘要层（甚至被丢），
//     静默回落会让这个配置错误表现为「快照怎么都是空的」。
//   - retention_days 非正 → 清理会把刚写进去的分区当天就删掉；而它现在
//     在采集关掉时也在跑，静默按默认值删会删掉本该留下的数据。
func LoadConfig(v *settings.Values) (Config, error) {
	if v == nil {
		return Config{}, fmt.Errorf("settings 为空，无法读取快照配置")
	}

	enabled, err := v.Bool(KeyEnabled)
	if err != nil {
		return Config{}, err
	}

	days, err := v.Int(KeyRetentionDays)
	if err != nil {
		return Config{}, err
	}
	if days <= 0 {
		return Config{}, fmt.Errorf("settings.%s 必须为正数，当前为 %d", KeyRetentionDays, days)
	}

	cfg := Config{Enabled: enabled, RetentionDays: int(days)}
	if !enabled {
		return cfg, nil
	}

	maxBytes, err := v.Int(KeyMaxBytes)
	if err != nil {
		return Config{}, err
	}
	if maxBytes <= 0 {
		return Config{}, fmt.Errorf("settings.%s 必须为正数，当前为 %d", KeyMaxBytes, maxBytes)
	}
	cfg.MaxBytes = int(maxBytes)

	return cfg, nil
}
