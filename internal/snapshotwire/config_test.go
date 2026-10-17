package snapshotwire

import (
	"context"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/settings"
)

// fakeSource 让配置读取脱离数据库测试。
//
// 出错的分支（缺键、类型不符、开关关闭、值非正）恰好是最需要覆盖的部分，
// 而它们在真库上很难构造——真库里的值来自迁移，改它要连库。
type fakeSource struct {
	rows []settings.Row
	err  error
}

func (f *fakeSource) Load(context.Context) ([]settings.Row, error) { return f.rows, f.err }

// settingsRows 是除快照三项之外的其余配置行。
//
// 必须补齐：settings.Load 会按 Registry 对照，少一个键就报错——
// 那是它的设计（键名写错不该安静生效），所以测试也得给全。
//
// 代价是这份清单与 Registry 耦合：**别的包新增一个配置项，这里就会失败**。
// 那是有意保留的——失败信息会直接点出缺哪个键，比让测试默默用上一份
// 过期的配置好。（同样形状的行集在 internal/settings 与
// internal/ledger/store 的测试里各有一份。）
func settingsRows(overrides map[string]string) []settings.Row {
	values := map[string]string{
		"request_snapshot.retention_days":    `30`,
		"request_snapshot.enabled":           `true`,
		"request_snapshot.max_bytes":         `65536`,
		"capture.consent_max_window_hours":   `24`,
		"quota.probe_interval_seconds":       `300`,
		"billing.default_asset":              `"CNY"`,
		"calendar.default_region":            `"CN"`,
		"idempotency.retention_hours":        `24`,
		"plugin.enabled":                     `false`,
		"plugin.call_timeout_ms":             `5000`,
		"plugin.memory_limit_bytes":          `67108864`,
		"plugin.max_payload_bytes":           `4194304`,
		"reconcile.auto_repair":              `false`,
		"reconcile.repair_mode":              `"resync_snapshot"`,
		"reconcile.difference_account":       `"reconcile_difference"`,
		"reconcile.shard_size":               `1000`,
		"reconcile.full_scan_interval_hours": `168`,
	}
	for k, v := range overrides {
		values[k] = v
	}
	rows := make([]settings.Row, 0, len(values))
	for k, v := range values {
		rows = append(rows, settings.Row{Key: k, Value: []byte(v)})
	}
	return rows
}

func loadValues(t *testing.T, overrides map[string]string) *settings.Values {
	t.Helper()
	v, err := settings.Load(context.Background(), &fakeSource{rows: settingsRows(overrides)})
	if err != nil {
		t.Fatalf("加载 settings 失败: %v", err)
	}
	return v
}

func TestLoadConfigReadsAllThree(t *testing.T) {
	cfg, err := LoadConfig(loadValues(t, map[string]string{
		"request_snapshot.enabled":        `true`,
		"request_snapshot.max_bytes":      `1024`,
		"request_snapshot.retention_days": `7`,
	}))
	if err != nil {
		t.Fatalf("LoadConfig 出错: %v", err)
	}
	if !cfg.Enabled || cfg.MaxBytes != 1024 || cfg.RetentionDays != 7 {
		t.Errorf("cfg = %+v；期望 enabled=true max_bytes=1024 retention_days=7", cfg)
	}
}

// 关掉开关时不校验另外两项：关了就不采集，留存期填错没有后果，
// 此时拒绝启动只会让「先关掉再说」这条退路也走不通。
func TestLoadConfigDisabledSkipsValidation(t *testing.T) {
	cfg, err := LoadConfig(loadValues(t, map[string]string{
		"request_snapshot.enabled":        `false`,
		"request_snapshot.max_bytes":      `0`,
		"request_snapshot.retention_days": `-1`,
	}))
	if err != nil {
		t.Fatalf("关掉开关时不该报错: %v", err)
	}
	if cfg.Enabled {
		t.Error("期望 Enabled 为 false")
	}
}

// 非正值不回落：max_bytes 为 0 会让每条骨架都降级甚至被丢，
// retention_days 为 0 会让刚写进去的分区当天就被删。
func TestLoadConfigRejectsNonPositive(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		wantKey   string
	}{
		{"max_bytes 为 0", map[string]string{"request_snapshot.max_bytes": `0`}, KeyMaxBytes},
		{"max_bytes 为负", map[string]string{"request_snapshot.max_bytes": `-1`}, KeyMaxBytes},
		{"retention_days 为 0", map[string]string{"request_snapshot.retention_days": `0`}, KeyRetentionDays},
		{"retention_days 为负", map[string]string{"request_snapshot.retention_days": `-3`}, KeyRetentionDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(loadValues(t, tc.overrides))
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("错误信息里应点名 %s，实际为 %v", tc.wantKey, err)
			}
		})
	}
}

// 类型不符时把错误抛出来，而不是当成零值：settings 那一侧已经按声明的
// 类型校验过，这条覆盖的是「调用方按错的类型读」。
func TestLoadConfigPropagatesTypeError(t *testing.T) {
	values := loadValues(t, nil)
	if _, err := LoadConfig(values); err != nil {
		t.Fatalf("正常配置不该报错: %v", err)
	}
	if _, err := LoadConfig(nil); err == nil {
		t.Error("settings 为 nil 时期望报错")
	}
}

// 配置项键名必须与 settings.Registry 里声明的一致。
//
// 写错的表现是启动时「缺少配置项」或「未声明」，而那种错误看起来
// 像数据库的问题，实际只是这里拼错了一个词。
func TestConfigKeysAreRegistered(t *testing.T) {
	registered := make(map[string]bool, len(settings.Registry))
	for _, s := range settings.Registry {
		registered[s.Key] = true
	}
	for _, key := range []string{KeyEnabled, KeyMaxBytes, KeyRetentionDays} {
		if !registered[key] {
			t.Errorf("键 %q 不在 settings.Registry 里", key)
		}
	}
}
