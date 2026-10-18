package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：一个申请了 kv 与 calendar 的插件，从公共入口走到假上游再走回来，
// 两个能力都真的读写了。
//
// # 这一条证明什么
//
//   - **注册期**：这份装配接上了 kv 与 calendar，于是插件申请得到（负向那一半
//     见 plugin_capabilities_test.go 里「只有 log 的装配」那一条）。
//   - **请求期**：`globalThis.__larkax.kv` / `.calendar` 真的是那两个命名空间，
//     而不是 undefined。用回一个 undefined 的能力时插件会抛异常，而**插件抛
//     异常时请求照常走完**（非阻断），所以这里不能断言「插件没报错」，只能断言
//     它在两处留下的痕迹：上游收到的补丁、以及库里的那些行。
//   - **组合**：插件上一次写进 KV 的东西，这一次读得到。这正是插件用 KV 做
//     「计数 / 游标 / 偏好」这类状态的用法，也是「KV 真的落库了」的证明
//     ——补丁里那个数只能来自上一次写下的值。
//
// # 全部是假依赖
//
// 上游是 httptest 起的假上游，日历数据是 fixture，KV 落在本包的测试库上。
// 仓库里的 e2e 一律如此，不碰真实上游。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，说明见 e2e_test.go 顶部。
func TestPluginKVAndCalendarEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-plugin-kv")

	// 2031-02-03 是**周一**，库里把它标成休息日：插件问出来的那个结论不能靠
	// 「今天是星期几」推出来，于是这一条分辨得出「走了日历」与「就地猜的」。
	holiday := time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)
	seedCalendarDay(t, db, holiday, "测试节假日", true)
	// 插件手里的时间戳是秒。取北京时间的上午十点。
	ts := holiday.Add(2 * time.Hour).Unix()

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	pluginKey := newPluginKey(t, "e2e-kv-calendar")
	insertPluginRow(t, db, pluginKey, kvCalendarSource(pluginKey, model, ts))

	app, err := buildAppWithSettings(t, db, map[string]string{"plugin.enabled": `true`})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if _, ok := app.Plugins.Get(pluginKey); !ok {
		t.Fatal("申请了 kv 与 calendar 的插件该上线——这份装配两项都接上了")
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 第一次：KV 里还没有东西，插件据此把 max_tokens 调小到 10。
	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}
	// 第二次：上一次写进去的值这次读得到，补丁因此变成 20。
	status, body = postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("第二次状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、插件真的跑过，而且补丁落到了上游。
	//
	// 两个数必须不同，而且第二个只能是**上一次写进 KV 的那个值**：插件第一次
	// 读不到键（kv.get 返回 null），第二次读得到。把 KV 那两项能力摘掉、或者
	// 让它们注入成 undefined，插件在 onRequest 里就抛异常，补丁不会生效，
	// 两次都会是调用方自己写的那个 max_tokens。
	sent := fake.sentBodies()
	if len(sent) != 2 {
		t.Fatalf("假上游收到 %d 次请求，期望 2 次", len(sent))
	}
	for i, want := range []float64{10, 20} {
		var got struct {
			MaxTokens *float64 `json:"max_tokens"`
		}
		if err := json.Unmarshal(sent[i], &got); err != nil {
			t.Fatalf("第 %d 次上游收到的请求体不是合法 JSON: %v\n%s", i+1, err, sent[i])
		}
		if got.MaxTokens == nil {
			t.Fatalf("第 %d 次上游收到的请求体里没有 max_tokens", i+1)
		}
		if *got.MaxTokens != want {
			t.Errorf("第 %d 次上游收到的 max_tokens = %v，期望 %v（补丁没生效，或者 KV 读不出上一次的值）",
				i+1, *got.MaxTokens, want)
		}
	}

	// 二、库里真的有了这个插件、这个用户的行，而且那个值是**库里的判定**，
	// 不是插件就地猜的：
	//
	//   rest_day  库里那一行标的是休息日（而 2031-02-03 是**周一**，
	//             按星期推不出这个结论）
	//   true      休息日就是不上班的那一天
	//   peak      北京上午十点落在 [08:00, 22:00) 里
	//   off_peak  同一个时刻在 22:00-08:00 之外——**档位跟着窗口走**，
	//             一个把窗口丢掉、自己按某种写死的排班判的实现给不出这一对
	rows := readPluginKV(t, db, pluginKey, user)
	if got := rows["日历"]; got != "rest_day/true/peak/off_peak" {
		t.Errorf("插件写进 KV 的日历结论 = %q，期望 %q", got, "rest_day/true/peak/off_peak")
	}
	if got := rows["看到过了"]; got != "上一次跑过" {
		t.Errorf("插件写进 KV 的值 = %q，期望 %q", got, "上一次跑过")
	}
	if len(rows) != 2 {
		t.Errorf("这个命名空间里有 %d 个键，期望 2 个：%v", len(rows), rows)
	}

	// 三、隔离落在请求路径上：插件拿到的是**被鉴权那个用户**的命名空间。
	//
	// store 那边的用例逐条钉过隔离的实现（同一个插件的两个用户互不可见），
	// 这一条钉的是另一件事：宿主函数收到的 userID 是不是鉴权给出的那一个。
	// 传错的话（比如恒为 0），插件写的东西会落到所有人的公用命名空间里。
	stranger := seedUser(t, db, false, "100")
	if rows := readPluginKV(t, db, pluginKey, stranger); len(rows) != 0 {
		t.Errorf("另一个用户名下也有这个插件的行: %v", rows)
	}
}

// kvCalendarSource 造一个申请了 kv 与 calendar 的插件。
//
// onRequest 里两件事都是真的：读一个 KV 键决定这次补丁、把日历的判定写回 KV。
// 补丁是「插件跑过了」在**上游那一边**留下的痕迹，KV 里那两行是它在**库里**
// 留下的痕迹——两处都有，这条链路才算通。
//
// 「日历」那个键里放四段结论：哪一天、是不是休息日，以及**同一个时刻在两个窗口
// 下的峰谷**。后两段是这条链路上唯一走完整条峰谷路的地方：窗口从插件写出来，
// 经宿主校验、经适配器折成排班、由 `calendar.PeakAt` 拿库里的数据判定。
// 两个窗口对同一时刻给出相反的档位，所以「窗口传下去了」在这里也看得见。
//
// 数不在这里做加减（moejs 那边没有 Number 可用），于是「读到过」与「没读到」
// 直接对应两个常量：10 与 20。两个都要**小于**调用方给的那个 max_tokens：
// 补丁只许把输出上界调小（见 plugin.RequestBodyPatch 那一侧的限制），
// 调大是改价，而定价权不在插件手里。
func kvCalendarSource(key, model string, ts int64) string {
	return fmt.Sprintf(`import { definePlugin, kv, calendar } from "larkax";
export const meta = {
  key: %q, version: "1.0.0", apiVersion: 1,
  models: [%q], capabilities: ["kv", "calendar"],
};
export default definePlugin({
  onRequest() {
    const prev = kv.get("看到过了");
    kv.set("看到过了", "上一次跑过");
    kv.set("日历", calendar.kind(%d, "CN") + "/" + String(calendar.isOffDay(%d, "CN")) +
      "/" + calendar.peak(%d, "CN", "08:00-22:00") +
      "/" + calendar.peak(%d, "CN", "22:00-08:00"));
    return { body: { max_tokens: prev === null ? 10 : 20 } };
  },
});
`, key, model, ts, ts, ts, ts)
}

// readPluginKV 把一个 (插件, 用户) 命名空间里的键值读出来。
//
// 直接查表而不是走 store.NewKV：这一条要证明的是**请求路径上真的落库了**，
// 用被测实现去读自己的写，出问题时分辨不出是「没写进去」还是「读不出来」。
func readPluginKV(t *testing.T, db *sql.DB, plugin string, userID int64) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT key, value FROM plugin_kv WHERE plugin_key = $1 AND user_id = $2`,
		plugin, userID)
	if err != nil {
		t.Fatalf("查 plugin_kv 失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("读 plugin_kv 的行失败: %v", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 plugin_kv 失败: %v", err)
	}
	return out
}
