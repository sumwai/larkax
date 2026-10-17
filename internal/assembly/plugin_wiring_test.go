package assembly

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 判据：plugin.enabled 为 false 时，另外三个键**根本不被读**。
//
// 三个值全是非法的（负数、零、零），而装配必须成功。这一条分辨的是「读了
// 没有」：把 LoadConfig 改成先校验限额再判开关，它立刻挂。
//
// 为什么值得这么在意：一个坏掉的插件限额不该让整个服务起不来，而「先关掉
// 再说」正是出事时最该走得通的那条退路。没开的功能连校验都不该做。
func TestBuildIgnoresPluginLimitsWhenDisabled(t *testing.T) {
	db := testDB(t)

	err := buildWithSettings(t, db, map[string]string{
		"plugin.enabled":            `false`,
		"plugin.call_timeout_ms":    `-1`,
		"plugin.memory_limit_bytes": `0`,
		"plugin.max_payload_bytes":  `0`,
	})
	if err != nil {
		t.Fatalf("关掉插件系统时，另外三个键不该被读，装配却失败了: %v", err)
	}
}

// 判据的另一半：开着的时候，同样的坏值必须让装配失败，而且点出是哪个键。
//
// 没有这一条，上面那条是可以白拿的——一个从来不读这三个键的实现也能过。
// 两条一起看才说明「读，但只在开着的时候读」。
//
// 失败要**在连库之前**发生（LoadConfig 在 buildPlugins 的第一步），所以
// 这里不必真的往库里放插件。
func TestBuildRefusesBadPluginLimitsWhenEnabled(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"调用时限为零", "plugin.call_timeout_ms", `0`},
		{"调用时限为负", "plugin.call_timeout_ms", `-1`},
		{"调用时限超出上界", "plugin.call_timeout_ms", `3600001`},
		{"内存上限为零", "plugin.memory_limit_bytes", `0`},
		{"载荷上限为零", "plugin.max_payload_bytes", `0`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSettings(t, db, map[string]string{
				"plugin.enabled": `true`,
				tc.key:           tc.value,
			})
			if err == nil {
				t.Fatalf("settings.%s = %s 时装配成功了——这个键没有被读", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("错误信息 %q 里没有键名 %s：配错的人要能一眼看出改哪个键",
					err.Error(), tc.key)
			}
			// 这是**配置**问题，不是「插件坏了」：两者的下一步动作不同
			// （一个去改 settings，一个去修那个插件的源码）。
			if !strings.Contains(err.Error(), "读取插件配置失败") {
				t.Errorf("错误信息 %q 没有说明失败在读配置这一步", err.Error())
			}
			if strings.Contains(err.Error(), "插件编译失败") {
				t.Errorf("配置写错被说成了插件编译失败：%q", err.Error())
			}
		})
	}
}

// 开着的时候：注册表真的从库里建起来了，四个键映射成限额。
//
// 这一条是这一片的正向证据——前面几条都只证明「配错会失败」，而一个永远
// 返回错误的实现也满足那些条件。
func TestBuildLoadsPluginsWhenEnabled(t *testing.T) {
	db := testDB(t)
	key := insertActivePlugin(t, db)

	app, err := buildAppWithSettings(t, db, map[string]string{
		"plugin.enabled":            `true`,
		"plugin.call_timeout_ms":    `2500`,
		"plugin.memory_limit_bytes": `16777216`,
		"plugin.max_payload_bytes":  `2048`,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if app.Plugins == nil {
		t.Fatal("开着的时候该有一份注册表")
	}
	if _, ok := app.Plugins.Get(key); !ok {
		t.Errorf("刚放进库里的插件 %s 该被装配进注册表", key)
	}

	// 四个键映射到 sandbox.Limits 的落点。断言写死字面量，不拿
	// sandbox.DefaultLimits 当期望——那等于让被测对象给自己出题（见
	// LESSONS.md 第 21 条）。
	if app.PluginLimits.CallTimeout != 2500*time.Millisecond {
		t.Errorf("CallTimeout = %v，期望 2.5s", app.PluginLimits.CallTimeout)
	}
	if app.PluginLimits.MemoryBytes != 16777216 {
		t.Errorf("MemoryBytes = %d，期望 16777216", app.PluginLimits.MemoryBytes)
	}
	// 一个键管两头：入参与出参都取 plugin.max_payload_bytes。
	if app.PluginLimits.MaxInputBytes != 2048 {
		t.Errorf("MaxInputBytes = %d，期望 2048", app.PluginLimits.MaxInputBytes)
	}
	if app.PluginLimits.MaxOutputBytes != 2048 {
		t.Errorf("MaxOutputBytes = %d，期望 2048", app.PluginLimits.MaxOutputBytes)
	}
}

// 关着的时候注册表是 nil，不是一份空表。
//
// 空表与「一个插件都没有」分不开：装配层拿到空表时无从判断是开关关着、
// 还是库里真的没插件——而这两种状态下该做的事完全不同（一个什么都不做，
// 一个要去查为什么插件没发布上去）。nil 让「没开」是一个说得出口的状态。
func TestBuildHasNoRegistryWhenDisabled(t *testing.T) {
	db := testDB(t)
	insertActivePlugin(t, db) // 库里有插件，但它不该被读

	app, err := buildAppWithSettings(t, db, map[string]string{
		"plugin.enabled": `false`,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if app.Plugins != nil {
		t.Errorf("关着的时候不该构造注册表，实际装了 %d 个插件", app.Plugins.Len())
	}
	if app.PluginLimits != (sandbox.Limits{}) {
		t.Errorf("关着的时候限额该是零值，实际 %+v", app.PluginLimits)
	}
}

// 坏插件不该让服务起不来：库里有编译不过的行时，装配照样成功。
//
// 这一条钉的是**加载策略**——「跳过」而不是「整体拒绝」。反过来写的话，
// 库里一条坏插件就等于一次线上起不来，而修它要先能连上库、找到那一行。
func TestBuildSkipsBrokenPluginAtStartup(t *testing.T) {
	db := testDB(t)
	good := insertActivePlugin(t, db)
	broken := insertBrokenPlugin(t, db)

	app, err := buildAppWithSettings(t, db, map[string]string{"plugin.enabled": `true`})
	if err != nil {
		t.Fatalf("一条坏插件不该让装配失败: %v", err)
	}
	if _, ok := app.Plugins.Get(good); !ok {
		t.Errorf("好的插件 %s 该照常上线", good)
	}
	if _, ok := app.Plugins.Get(broken); ok {
		t.Errorf("编译不过的插件 %s 不该上线", broken)
	}
}

// ── 助手 ──────────────────────────────────────────────────────────────

// insertActivePlugin 往库里放一条能编译过的 active 插件，返回它的 key。
//
// 直接写 SQL 而不是走 pluginstore.Publish：要构造的是**库里已有的状态**，
// 而 Publish 只产生它自己那一种（新行是 pending，要 enable 才 active）。
func insertActivePlugin(t *testing.T, db *sql.DB) string {
	t.Helper()
	key := newPluginKey(t, "wiring")
	// 源码 meta 里的 key 必须与行上的 key 一致：加载器会核对这两处
	// （见 plugin.Compile），不一致的话这一条会挂在「插件没上线」上，
	// 而原因看起来像加载器坏了。
	return insertPluginRow(t, db, key, pluginSource(key, "1.0.0"))
}

func insertBrokenPlugin(t *testing.T, db *sql.DB) string {
	t.Helper()
	return insertPluginRow(t, db, newPluginKey(t, "wiring-broken"),
		"import { definePlugin } from \"larkax\";\nexport default { onUsage() { ")
}

// newPluginKey 造一个本次运行独一份的 key。
//
// 带上测试名与纳秒：同一个库上重复跑不能撞上上一次留下的行，否则第二次
// 会插进同一个 (key, version) 而报唯一键冲突——看起来像代码坏了。
func newPluginKey(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%s-%d", prefix,
		strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())),
		time.Now().UnixNano())
}

func insertPluginRow(t *testing.T, db *sql.DB, key, source string) string {
	t.Helper()

	meta := fmt.Sprintf(`{"key":%q,"version":"1.0.0","apiVersion":1}`, key)

	_, err := db.ExecContext(context.Background(),
		`INSERT INTO plugins (key, version, name, source, meta, status, generation)
		 VALUES ($1, '1.0.0', '接线用例', $2, $3::jsonb, 'active', 0)`,
		key, source, meta)
	if err != nil {
		t.Fatalf("插入插件行失败: %v", err)
	}
	return key
}

// pluginSource 造一段能编译过的插件源码。
//
// 里面的 key 与 meta 里的 key 一致：加载器会核对这两处（见 plugin.Compile），
// 不一致的话这一条会挂在「插件没上线」上，而原因看起来像加载器坏了。
func pluginSource(name, version string) string {
	return fmt.Sprintf(`import { definePlugin } from "larkax";
export const meta = { key: %q, version: %q, apiVersion: 1 };
export default definePlugin({ onUsage() { return { promptTokens: 1 }; } });
`, name, version)
}
