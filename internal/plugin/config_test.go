package plugin

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
	"github.com/sumwai/larkax/internal/settings"
)

// fakeSource 让配置读取脱离数据库测试。
//
// 出错的分支（缺键、类型不符、开关关闭、值越界）恰好是最需要覆盖的部分，
// 而它们在真库上很难构造——真库里的值来自迁移，改它要连库。
type fakeSource struct{ rows []settings.Row }

func (f fakeSource) Load(context.Context) ([]settings.Row, error) { return f.rows, nil }

// factorySettings 是迁移写下的那套出厂值。
//
// 必须补齐**全部**键：settings.Load 会按 Registry 对照，少一个就报错——
// 那是它的设计（键名写错不该安静生效），所以测试也得给全。代价是这份清单
// 与 Registry 耦合：别的包新增一个配置项，这里就会失败，而失败信息会直接
// 点出缺哪个键。仓库里同形状的行集还有 internal/snapshotwire 的测试。
var factorySettings = map[string]string{
	"request_snapshot.retention_days":    `30`,
	"request_snapshot.enabled":           `true`,
	"request_snapshot.max_bytes":         `65536`,
	"capture.consent_max_window_hours":   `24`,
	"quota.probe_interval_seconds":       `300`,
	"quota.exhausted_cooldown_seconds":   `900`,
	"billing.default_asset":              `"CNY"`,
	"billing.max_output_tokens":          `65536`,
	"billing.stream_freeze_step":         `1024`,
	"billing.merchant_share_bp":          `0`,
	"calendar.default_region":            `"CN"`,
	"idempotency.retention_hours":        `24`,
	"console.session_retention_days":     `30`,
	"plugin.enabled":                     `false`,
	"plugin.call_timeout_ms":             `5000`,
	"plugin.memory_limit_bytes":          `67108864`,
	"plugin.max_payload_bytes":           `4194304`,
	"reconcile.auto_repair":              `false`,
	"reconcile.repair_mode":              `"report"`,
	"reconcile.difference_account":       `"difference"`,
	"reconcile.shard_size":               `1000`,
	"reconcile.full_scan_interval_hours": `24`,
	"payment.fake_enabled":               `false`,
	"plans.charge_order":                 `"plan_first"`,
	"admin.token_sha256":                 `""`,
}

func settingsValues(t *testing.T, overrides map[string]string) *settings.Values {
	t.Helper()

	values := make(map[string]string, len(factorySettings))
	for k, v := range factorySettings {
		values[k] = v
	}
	for k, v := range overrides {
		values[k] = v
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	rows := make([]settings.Row, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, settings.Row{Key: k, Value: []byte(values[k])})
	}
	v, err := settings.Load(context.Background(), fakeSource{rows: rows})
	if err != nil {
		t.Fatalf("装载配置失败: %v", err)
	}
	return v
}

// 判据：plugin.enabled 为 false 时**不读**另外三个键。
//
// 这是「没开的功能不该因为一个坏配置把启动搞挂」的落点。三个值全是非法的
// （负数、零、零），而这里必须一声不吭地过——没开的功能连校验都不该做，
// 否则「先关掉再说」这条退路也走不通。
//
// 反过来说，这一条也分辨得出「读了」与「没读」：把 LoadConfig 改成先校验
// 再判开关，它立刻挂。
func TestLoadConfigDoesNotReadLimitsWhenDisabled(t *testing.T) {
	v := settingsValues(t, map[string]string{
		"plugin.enabled":            `false`,
		"plugin.call_timeout_ms":    `-1`,
		"plugin.memory_limit_bytes": `0`,
		"plugin.max_payload_bytes":  `0`,
	})

	cfg, err := LoadConfig(v)
	if err != nil {
		t.Fatalf("关掉开关时不该读另外三个键，实际报错: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled 该是 false")
	}
	if cfg.Limits != (sandbox.Limits{}) {
		t.Fatalf("关掉时限额该是零值，实际 %+v", cfg.Limits)
	}
}

// 开着的时候非法值一律拒绝启动，而且错误里要点出是哪个键。
//
// 静默回落的表现是「插件跑在另一套限额下」——不报错，只是行为不同，
// 而那种差异只有在出问题时才看得出来。
func TestLoadConfigRejectsBadLimits(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"调用时限为零", KeyCallTimeoutMS, `0`},
		{"调用时限为负", KeyCallTimeoutMS, `-1`},
		{"调用时限超出上界", KeyCallTimeoutMS, `3600001`},
		{"内存上限为零", KeyMemoryLimitBytes, `0`},
		{"内存上限为负", KeyMemoryLimitBytes, `-1`},
		{"载荷上限为零", KeyMaxPayloadBytes, `0`},
		{"载荷上限为负", KeyMaxPayloadBytes, `-1`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := settingsValues(t, map[string]string{
				KeyEnabled: `true`,
				tc.key:     tc.value,
			})
			_, err := LoadConfig(v)
			if err == nil {
				t.Fatalf("%s = %s 时该拒绝，实际通过了", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("错误信息 %q 里没有键名 %s：配错的人要能一眼看出改哪个键",
					err.Error(), tc.key)
			}
		})
	}
}

// 映射到 sandbox.Limits 的落点是**具体数字**，不是「比另一个大」。
//
// 断言写死字面量：拿 sandbox.DefaultLimits 当期望等于让被测对象给自己出题，
// 把默认值改掉也照样通过（见 LESSONS.md 第 21 条）。
func TestLoadConfigMapsLimits(t *testing.T) {
	v := settingsValues(t, map[string]string{
		KeyEnabled:          `true`,
		KeyCallTimeoutMS:    `1500`,
		KeyMemoryLimitBytes: `8388608`,
		KeyMaxPayloadBytes:  `1024`,
	})

	cfg, err := LoadConfig(v)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("Enabled 该是 true")
	}
	if cfg.Limits.CallTimeout != 1500*time.Millisecond {
		t.Errorf("CallTimeout = %v，期望 1.5s", cfg.Limits.CallTimeout)
	}
	if cfg.Limits.MemoryBytes != 8388608 {
		t.Errorf("MemoryBytes = %d，期望 8388608", cfg.Limits.MemoryBytes)
	}
	// 一个键管两头：入参与出参都取 max_payload_bytes。
	if cfg.Limits.MaxInputBytes != 1024 {
		t.Errorf("MaxInputBytes = %d，期望 1024", cfg.Limits.MaxInputBytes)
	}
	if cfg.Limits.MaxOutputBytes != 1024 {
		t.Errorf("MaxOutputBytes = %d，期望 1024", cfg.Limits.MaxOutputBytes)
	}
}

// 两个界本身必须装得起来。
//
// 上面那条只说明「界外会失败」，它分辨不出「界画错了一位」：把上界写成
// 360000（少一个零）时上面照样全过，而一个完全合理的配置会在启动时被拒。
// 这一条把两个界钉死，界往内挪一格它就挂。
func TestLoadConfigAcceptsTimeoutAtBounds(t *testing.T) {
	for _, ms := range []string{`1`, `3600000`} {
		t.Run(ms, func(t *testing.T) {
			v := settingsValues(t, map[string]string{
				KeyEnabled:       `true`,
				KeyCallTimeoutMS: ms,
			})
			if _, err := LoadConfig(v); err != nil {
				t.Fatalf("边界值 %s 毫秒该被接受，实际 %v", ms, err)
			}
		})
	}
}

// settings 为空时不能返回一个「关掉了」的配置。
//
// 返回零值的话，装配层会把它读成「插件没开」——而真正的原因是这一份配置
// 根本没读出来，两件事在行为上分不开。
func TestLoadConfigRejectsNilSettings(t *testing.T) {
	if _, err := LoadConfig(nil); err == nil {
		t.Fatal("settings 为 nil 时该报错，而不是当成「没开」")
	}
}
