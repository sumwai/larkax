package assembly

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/upstream"
)

// 运行期配置里那几个「必须在装配期读出来」的键。
//
// # 这一组用例要证明什么
//
// 键在 Registry 里声明过、在库里有一行，这不等于**有代码读它**。读与不读
// 在装配成功时长得一模一样：装配照样过，服务照样起，只是那个键改了不会有
// 任何效果（见 LESSONS.md 第 22 条）。
//
// 所以这里断言的是「配错时装配会失败」——那是唯一一个把「读了」与
// 「没读」分开的观测量。配错之前，改这个键对装配**没有任何影响**，
// 于是这几条会挂。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，说明见 e2e_test.go 顶部。

// buildWithSetting 把某个键临时改成 value，然后按生产路径装配一次。
//
// 改完立刻还原（t.Cleanup）：这些键是迁移写下的出厂配置，同一个库上还有
// 别的用例指望它们是原样，而「另一个用例挂了」看起来像它自己的问题。
//
// 返回装配的错误。装配成功时返回 nil——那是这些用例里的**失败**情形。
func buildWithSetting(t *testing.T, db *sql.DB, key, value string) error {
	t.Helper()
	return buildWithSettings(t, db, map[string]string{key: value})
}

// buildWithSettings 是上面那个的多键版本。
//
// 一次改一个键不够用：插件的开关用例要同时摆出「开关关着」与「另外三个键
// 是坏的」两种状态，而那正是它要证明的事——关着的时候后面那三个键根本
// 不该被读。分成两次装配就证明不了，因为中间那次装配里开关是开着的。
func buildWithSettings(t *testing.T, db *sql.DB, overrides map[string]string) error {
	t.Helper()
	_, err := buildAppWithSettings(t, db, overrides)
	return err
}

// buildAppWithSettings 同上，但把装配好的 App 也交出来。
//
// 要看的除了「起不起得来」还有「起来之后装上了什么」——插件的开关用例
// 两头都要：关着时注册表该是 nil，开着时该真有插件在里面。
func buildAppWithSettings(t *testing.T, db *sql.DB, overrides map[string]string) (*App, error) {
	t.Helper()
	ctx := context.Background()

	before := make(map[string]string, len(overrides))
	for key := range overrides {
		var v string
		if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v); err != nil {
			t.Fatalf("读 settings.%s 失败: %v", key, err)
		}
		before[key] = v
	}
	t.Cleanup(func() {
		for key, v := range before {
			if _, err := db.ExecContext(context.WithoutCancel(ctx),
				`UPDATE settings SET value = $2 WHERE key = $1`, key, v); err != nil {
				t.Errorf("还原 settings.%s 失败: %v", key, err)
			}
		}
	})
	for key, value := range overrides {
		if _, err := db.ExecContext(ctx,
			`UPDATE settings SET value = $2 WHERE key = $1`, key, value); err != nil {
			t.Fatalf("改 settings.%s 失败: %v", key, err)
		}
	}

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		// 值本身不合法（类型不对、JSON 写坏了）时 Load 就会拒绝。
		// 那也是一种「装配起不来」，但它证明的是 settings 那一侧的校验，
		// 不是这里要看的接线，所以单独报出来，别混进断言里。
		return nil, err
	}

	executor, err := upstream.New(10 * time.Second)
	if err != nil {
		t.Fatalf("构造上游执行器失败: %v", err)
	}

	return Build(Config{
		DB:       db,
		Sealer:   testSealer(t),
		Settings: values,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:  "wiring-test",
		Executor: executor,
		Rules:    pricing.New(db),
		Costs:    pricing.NewCostSource(db),
	})
}

// 账本记不了的资产品种必须在装配期被拒。
//
// 这一条钉的是 billing.default_asset 真的被读了。原先那个键没有任何读者，
// 而结算品种是 internal/pipeline 里写死的 "CNY"：把库里改成 USD，装配照样
// 成功、服务照样起、账单照样记 CNY——改动看上去做了，其实没做。
//
// 现在配一个账本认不出的品种会让装配失败。**这条用例的价值全在这一点上**：
// 配错之后的两种行为（失败 / 静默按 CNY 跑）长得完全不一样，才分辨得出
// 那个键有没有被读。
func TestBuildRefusesAssetTheLedgerCannotBook(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"账本不支持的品种", `"USD"`, "USD"},
		{"空品种", `""`, "billing.default_asset"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSetting(t, db, "billing.default_asset", tc.value)
			if err == nil {
				t.Fatalf("settings.billing.default_asset = %s 时装配成功了——"+
					"这个键没有被读，结算品种仍然是代码里写死的那个", tc.value)
			}
			for _, want := range []string{"billing.default_asset", tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息 %q 里没有 %q：配错的人要能一眼看出改哪个键", err.Error(), want)
				}
			}
		})
	}
}

// 单次输出上限超出允许范围时必须在装配期被拒。
//
// 这一条钉的是 billing.max_output_tokens 真的被读了。它只在那条判据上生效
// （见 gateway.checkOutputCap），而配错的表现有两种，都不是报错：0 会让这条
// 判据整条失效（见 gateway.ErrNoOutputCap），配得太大会让「这个数还算不算
// 一个界」变成一个没有答案的问题。两者在请求上都看不出来，所以在这里判掉。
func TestBuildRefusesOutOfRangeMaxOutputTokens(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		value string
	}{
		{"零", `0`},
		{"负数", `-1`},
		{"超过上界", strconv.Itoa(maxMaxOutputTokens + 1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSetting(t, db, maxOutputTokensKey, tc.value)
			if err == nil {
				t.Fatalf("settings.%s = %s 时装配成功了——这个键没有被读",
					maxOutputTokensKey, tc.value)
			}
			if !strings.Contains(err.Error(), maxOutputTokensKey) {
				t.Errorf("错误信息 %q 里没有键名：配错的人要能一眼看出改哪个键", err.Error())
			}
		})
	}
}

// 探测间隔非正时必须在装配期被拒。
//
// 这一条钉的是 quota.probe_interval_seconds 真的被读了。原先那个键没有任何
// 读者：自记账的陈旧阈值是 internal/quota 里写死的 15 分钟，把探测间隔改成
// 1 分钟，阈值一动不动。
//
// 间隔为 0 会让阈值也变成 0，于是每一次自记账都被判成「实测已经很旧」，
// 可信度从第一次扣减起就在衰减——而它在库里与「配置生效了」长得一样。
// 与其让它安静地生效，不如让装配当场失败。
func TestBuildRefusesNonPositiveProbeInterval(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		value string
	}{
		{"零", `0`},
		{"负数", `-1`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSetting(t, db, "quota.probe_interval_seconds", tc.value)
			if err == nil {
				t.Fatalf("settings.quota.probe_interval_seconds = %s 时装配成功了——"+
					"这个键没有被读，陈旧阈值仍然是代码里写死的那个", tc.value)
			}
			if !strings.Contains(err.Error(), "quota.probe_interval_seconds") {
				t.Errorf("错误信息 %q 里没有键名：配错的人要能一眼看出改哪个键", err.Error())
			}
		})
	}
}

// 换算会溢出的探测间隔必须在装配期被拒，而且要在**乘法之前**判掉。
//
// 只判上界挡不住超大负数：Go 的整数溢出不报错，它绕回一个看起来正常的数，
// 于是装配成功、服务照常启动，而阈值是一个谁也没配过的值。下面两个数都是
// 实测出来的绕法，不是「大得离谱所以显然会挂」：
//
//	-9223372036854775807 秒 → 换算后 +1s，再乘 staleAfterFactor 得 +3s
//	-3074457345618258603 秒 → 换算后 +1708031h…，再乘得 -1s
//
// 两个都「小于上界」，所以只判上界时它们一路通过。断言里写的是**字面量**，
// 不拿 minProbeIntervalSeconds / maxProbeIntervalSeconds 去测它们自己——那
// 等于让被测常量给自己出题，把它改掉也照样通过。
func TestBuildRefusesProbeIntervalOutOfRange(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		value string
	}{
		{"超大负数，换算后绕成正阈值", `-9223372036854775807`},
		{"超大负数，换算后绕成负阈值", `-3074457345618258603`},
		{"比上限多一秒", `31536001`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSetting(t, db, "quota.probe_interval_seconds", tc.value)
			if err == nil {
				t.Fatalf("settings.quota.probe_interval_seconds = %s 时装配成功了——"+
					"绕出来的阈值谁也没配过，而服务会照常启动", tc.value)
			}
			if !strings.Contains(err.Error(), "quota.probe_interval_seconds") {
				t.Errorf("错误信息 %q 里没有键名：配错的人要能一眼看出改哪个键", err.Error())
			}
		})
	}
}

// 上下界的**边界值本身**必须装得起来。
//
// 上面几条只说明「界外会失败」，它们分辨不出「界画错了一位」：把上限写成
// 3153600（少一个零）时，上面几条照样全过，而一个完全合理的配置会在启动时
// 被拒。这一条把两个界钉死：31536000 秒与 1 秒都装得起来，界往内挪一格
// 它就挂。
func TestBuildAcceptsProbeIntervalAtBounds(t *testing.T) {
	db := testDB(t)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"上界（一年）", `31536000`},
		{"下界（一秒）", `1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildWithSetting(t, db, "quota.probe_interval_seconds", tc.value); err != nil {
				t.Fatalf("settings.quota.probe_interval_seconds = %s 是允许范围内的值，"+
					"装配却失败了：%v", tc.value, err)
			}
		})
	}
}

// 出厂配置下装配必须成功。
//
// 上面两条断言的是「配错会失败」，而它们自己分辨不出「校验太严，什么都装
// 不出来」。这一条是那个分母：库里那份出厂值装配得起来。
func TestBuildAcceptsFactorySettings(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	values, err := settings.Load(context.Background(), settings.NewPgSource(db))
	if err != nil {
		t.Fatalf("加载运行期配置失败: %v", err)
	}
	if got, err := values.String("billing.default_asset"); err != nil || got != "CNY" {
		t.Fatalf("出厂资产品种 = %q（%v），期望 CNY：这条用例的落点没了", got, err)
	}

	app := newApp(t, db, sealer)
	if app.Public == nil {
		t.Fatal("出厂配置下装出来的 handler 是 nil")
	}
}

// 流式递进冻结的阈值段必须被读到，而且真的接到网关上。
//
// 两头各会静默出错，所以一起钉：
//
//   - 装配期的范围校验：配成 0 或负数时网关会拒掉**每一个**流式请求
//     （见 gateway.ErrNoFreezeStep），现场是一批 5xx；配得比输出上界还大时
//     第一段就把整笔冻满，递进冻结整条不发生；
//   - 接线：校验过了、网关那个字段却还是零值（漏接或接错字段）时，表现与
//     第一条完全一样（一批 5xx）——「配置错」与「代码漏接」在那批 5xx 里
//     分不出来，而处置是两件事。
//
// 所以这一条既比对库里的值，也比对**网关拿到的值**：只看前者，漏接照样绿。
func TestBuildReadsStreamFreezeStep(t *testing.T) {
	db := testDB(t)

	app, err := buildAppWithSettings(t, db, nil)
	if err != nil {
		t.Fatalf("出厂配置下装配失败: %v", err)
	}
	// 出厂值由迁移写下（4096）。写死这个字面量而不是引用某个常量：这条用例
	// 要钉的正是「库里那个数」与「网关拿到的数」是同一个。
	if got := app.Gateway.StreamFreezeStep; got != 4096 {
		t.Errorf("出厂阈值段 = %d，期望 4096（迁移写下的那个数）", got)
	}

	app, err = buildAppWithSettings(t, db, map[string]string{streamFreezeStepKey: "8192"})
	if err != nil {
		t.Fatalf("改阈值段之后装配失败: %v", err)
	}
	if got := app.Gateway.StreamFreezeStep; got != 8192 {
		t.Errorf("改过之后网关的阈值段 = %d，期望 8192——这个键没有被读到网关上", got)
	}
}

// 阈值段超出允许范围或非正时必须在装配期被拒。
//
// 非正的值会让每一个流式请求都失败（网关拒掉整条路），而这与代码故障同形；
// 超过出厂输出上界默认值（65536）的段让初始预留就冻满整笔，等于把递进冻结
// 关掉——「关掉」与「配得很宽」在请求上长得一模一样。
//
// 越界值写**字面量** 65537，不写 maxStreamFreezeStep + 1：后者是拿实现里的
// 常量测实现里的常量，把上界改成多少这条用例都绿，钉不住这个数（见 AGENTS.md
// 的「用常量测常量等于没测」）。边界值本身装得起来那一半见下一条。
func TestBuildRefusesOutOfRangeStreamFreezeStep(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		value string
	}{
		{"零", `0`},
		{"负数", `-1`},
		{"超过上界", `65537`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSetting(t, db, streamFreezeStepKey, tc.value)
			if err == nil {
				t.Fatalf("settings.%s = %s 时装配成功了——这个键没有被读",
					streamFreezeStepKey, tc.value)
			}
			if !strings.Contains(err.Error(), streamFreezeStepKey) {
				t.Errorf("错误信息 %q 里没有键名：配错的人要能一眼看出改哪个键", err.Error())
			}
		})
	}
}

// 阈值段的**边界值本身**必须装得起来。
//
// 上一条只说明「界外会失败」，它分辨不出「界画错了一位」：把上界写成 6553
// （少一位）时，上一条照样全过，而一个完全合理的配置会在启动时被拒；反过来
// 把上界抬回 1<<20 时，上一条也全过，而那条要防的配置又放行了。这一条把上界
// 钉在 65536：它装得起来，界往内挪一格它就红。下界 1 同理。
func TestBuildAcceptsStreamFreezeStepAtBounds(t *testing.T) {
	db := testDB(t)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"上界（65536，出厂输出上界默认值）", `65536`},
		{"下界（一段）", `1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildWithSetting(t, db, streamFreezeStepKey, tc.value); err != nil {
				t.Fatalf("settings.%s = %s 是允许范围内的值，装配却失败了：%v",
					streamFreezeStepKey, tc.value, err)
			}
		})
	}
}

// 见底渠道的恢复冷却期必须被读到，而且真的接到路由上。
//
// 两头各会静默出错：键没人读时，改多少次都不会有渠道恢复——现场只表现为
// 「额度回来之后渠道还是不用」；读到却漏接路由时一模一样。所以既比对库里
// 的值，也比对**路由拿到的值**：只看前者，漏接照样绿。
//
// 出厂值 900 秒与迁移写下的一致，这里写字面量而不是引用常量：这条用例要钉
// 的正是那个数与路由字段是同一个。
func TestBuildReadsQuotaExhaustedCooldown(t *testing.T) {
	db := testDB(t)

	app, err := buildAppWithSettings(t, db, nil)
	if err != nil {
		t.Fatalf("出厂配置下装配失败: %v", err)
	}
	if got := app.Gateway.Router.QuotaCooldown; got != 900*time.Second {
		t.Errorf("出厂冷却期 = %v，期望 15m（迁移写下的 900 秒）", got)
	}

	app, err = buildAppWithSettings(t, db, map[string]string{quotaCooldownKey: "60"})
	if err != nil {
		t.Fatalf("改冷却期之后装配失败: %v", err)
	}
	if got := app.Gateway.Router.QuotaCooldown; got != 60*time.Second {
		t.Errorf("改过之后路由的冷却期 = %v，期望 1m——这个键没有被读到路由上", got)
	}
}

// 冷却期非正或超出范围时必须在装配期被拒。
//
// 零值会让这条恢复路径整个关掉（见 route.Router.QuotaCooldown），而「关掉」
// 与「配成了一个正数」在运行期长得一样；超大负数还会在换算成 time.Duration
// 时绕回一个看起来正常的时长。两个方向都在启动时判掉。
//
// 越界值写字面量，不拿 min/max 常量给自己出题。
func TestBuildRefusesOutOfRangeQuotaCooldown(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name  string
		value string
	}{
		{"零", `0`},
		{"负数", `-1`},
		{"超大负数，换算后绕成正时长", `-9223372036854775807`},
		{"超大负数，换算后绕成负时长", `-3074457345618258603`},
		{"比上限多一秒", `31536001`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildWithSetting(t, db, quotaCooldownKey, tc.value)
			if err == nil {
				t.Fatalf("settings.%s = %s 时装配成功了——这个键没有被读，或者范围判据漏了",
					quotaCooldownKey, tc.value)
			}
			if !strings.Contains(err.Error(), quotaCooldownKey) {
				t.Errorf("错误信息 %q 里没有键名：配错的人要能一眼看出改哪个键", err.Error())
			}
		})
	}
}

// 冷却期的边界值本身必须装得起来：一年与一秒。
//
// 上一条只说明「界外会失败」，分辨不出「界画错了一位」：把上限写成
// 3153600（少一个零）时它照样全过，而一个完全合理的配置会在启动时被拒。
func TestBuildAcceptsQuotaCooldownAtBounds(t *testing.T) {
	db := testDB(t)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"上界（一年）", `31536000`},
		{"下界（一秒）", `1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildWithSetting(t, db, quotaCooldownKey, tc.value); err != nil {
				t.Fatalf("settings.%s = %s 是允许范围内的值，装配却失败了：%v",
					quotaCooldownKey, tc.value, err)
			}
		})
	}
}
