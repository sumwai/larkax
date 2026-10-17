package assembly

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
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
	ctx := context.Background()

	var before string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&before); err != nil {
		t.Fatalf("读 settings.%s 失败: %v", key, err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE settings SET value = $2 WHERE key = $1`, key, before); err != nil {
			t.Errorf("还原 settings.%s 失败: %v", key, err)
		}
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = $2 WHERE key = $1`, key, value); err != nil {
		t.Fatalf("改 settings.%s 失败: %v", key, err)
	}

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		// 值本身不合法（类型不对、JSON 写坏了）时 Load 就会拒绝。
		// 那也是一种「装配起不来」，但它证明的是 settings 那一侧的校验，
		// 不是这里要看的接线，所以单独报出来，别混进断言里。
		return err
	}

	executor, err := upstream.New(10 * time.Second)
	if err != nil {
		t.Fatalf("构造上游执行器失败: %v", err)
	}

	_, err = Build(Config{
		DB:       db,
		Sealer:   testSealer(t),
		Settings: values,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:  "wiring-test",
		Executor: executor,
		Rules:    pricing.New(db),
	})
	return err
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
