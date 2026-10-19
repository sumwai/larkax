package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
)

// 端到端：不重启进程，重新装载之后**下一个请求**走新一代。
//
// 三发请求分别落在「初始那一代」「重新装载之后」「一次失败的重新装载之后」，
// 断言的是**上游收到的那个补丁值**（10 / 20 / 20）：它同时证明两件事——插件真的
// 换了，以及换表没有把在途/后续请求弄坏。
//
// 版本号也单独断言一次（1.0.0 → 2.0.0，失败之后仍是 2.0.0），因为只看请求行为
// 的话，「两个版本恰好补出一个数」也能过；而这一次的判据本来就是「线上换到了
// 哪一代」。
func TestReloadPluginsSwitchesGenerationWithoutRestart(t *testing.T) {
	db := testDB(t)
	// 整批热更新的判据会被这个包共用的库里别处留下的坏插件绊住。本包用例串行，
	// 清掉加载器会读的那两张表不会与谁打架；每个用例自己造夹具。
	clearPluginRows(t, db)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-reload-model")
	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 优先级取一个远高于其他用例夹具的数：这个包共用同一个测试库，别的用例
	// 留下的插件也匹配所有模型，而「一个请求只跑一个插件」取的是优先级最高的。
	const priority = 99
	pluginKey := newPluginKey(t, "reload")
	publishPluginVersion(t, db, pluginKey, "1.0.0", reloadPluginSource(pluginKey, "1.0.0", model, 10), priority)

	app, err := buildAppWithSettings(t, db, map[string]string{"plugin.enabled": `true`})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if got := loadedVersion(t, app, pluginKey); got != "1.0.0" {
		t.Fatalf("装配时装载的版本 = %q，期望 1.0.0", got)
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 第一发：1.0.0 把 max_tokens 补成 10。
	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusOK {
		t.Fatalf("第一发状态码 = %d，响应体：%s", status, body)
	}

	// 发布 2.0.0 并启用，然后走一次热更新（SIGHUP 那条路最终调的就是它）。
	publishPluginVersion(t, db, pluginKey, "2.0.0", reloadPluginSource(pluginKey, "2.0.0", model, 20), priority)
	if err := app.ReloadPlugins(ctx); err != nil {
		t.Fatalf("重新装载失败: %v", err)
	}
	if got := loadedVersion(t, app, pluginKey); got != "2.0.0" {
		t.Fatalf("重新装载之后版本 = %q，期望 2.0.0", got)
	}

	// 第二发：2.0.0 把它补成 20。
	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusOK {
		t.Fatalf("第二发状态码 = %d，响应体：%s", status, body)
	}

	// 发布一个编译不过的 3.0.0 并启用：重新装载必须整批拒绝，线上留在 2.0.0。
	publishPluginVersion(t, db, pluginKey, "3.0.0",
		fmt.Sprintf(`import { definePlugin } from "larkax";`+"\n"+
			`export const meta = { key: %q, version: "3.0.0", apiVersion: 1 };`+"\n"+
			`export default { onRequest() { `, pluginKey), priority)

	reloadErr := app.ReloadPlugins(ctx)
	if reloadErr == nil {
		t.Fatal("新一代编译不过时重新装载该返回错误")
	}
	if !strings.Contains(reloadErr.Error(), pluginKey) {
		t.Errorf("错误信息 %q 里没有插件的 key：运维要知道是哪一条没装成", reloadErr.Error())
	}
	if got := loadedVersion(t, app, pluginKey); got != "2.0.0" {
		t.Fatalf("失败的重新装载之后版本 = %q，期望仍是 2.0.0（旧代次继续服务）", got)
	}

	// 第三发：仍是 2.0.0 的补丁 20，请求不受上一次失败影响。
	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusOK {
		t.Fatalf("第三发状态码 = %d，响应体：%s", status, body)
	}

	sent := fake.sentBodies()
	if len(sent) != 3 {
		t.Fatalf("假上游收到 %d 次请求，期望 3 次", len(sent))
	}
	for i, want := range []float64{10, 20, 20} {
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
			t.Errorf("第 %d 次上游收到的 max_tokens = %v，期望 %v", i+1, *got.MaxTokens, want)
		}
	}
}

// 判据：插件系统没启用时，热更新返回一个可辨认的哨兵，而不是「成功」或一个故障。
//
// 它让 cmd/larkax 那一条日志能说「SIGHUP 收到了，但没有东西要换」——把 no-op
// 记成错误会让人去查一个不存在的故障，记成成功又会让「信号没接上」与「接上了
// 但没开插件」分不开。
func TestReloadPluginsReportsDisabled(t *testing.T) {
	db := testDB(t)

	app, err := buildAppWithSettings(t, db, map[string]string{"plugin.enabled": `false`})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if err := app.ReloadPlugins(context.Background()); err == nil {
		t.Fatal("插件系统关着时该返回错误")
	} else if !strings.Contains(err.Error(), "插件系统未启用") {
		t.Errorf("错误信息 %q 没有说清是「没启用」，那会让人以为热更新坏了", err.Error())
	}
}

// ── 助手 ──────────────────────────────────────────────────────────────

// loadedVersion 取当前注册表里这个插件的版本；不在表里时直接失败。
func loadedVersion(t *testing.T, app *App, key string) string {
	t.Helper()
	reg := app.Plugins.Load()
	if reg == nil {
		t.Fatal("当前没有注册表")
	}
	p, ok := reg.Get(key)
	if !ok {
		t.Fatalf("插件 %s 不在注册表里", key)
	}
	return p.Meta.Version
}

// publishPluginVersion 发布并启用一个版本。
//
// 直接走 store.Publish（而不是 INSERT）：启用记录里的优先级也一起写上，
// 而这是本包共用测试库时「选中这个插件」的唯一办法。
func publishPluginVersion(t *testing.T, db *sql.DB, key, version, source string, priority int) {
	t.Helper()
	ctx := context.Background()
	meta := fmt.Sprintf(`{"key":%q,"name":"热更新用例","version":%q,"apiVersion":1}`, key, version)
	if _, err := pluginstore.New(db).Publish(ctx, pluginstore.Publication{
		Key: key, Version: version, Name: "热更新用例", Source: source,
		Meta: []byte(meta), Priority: priority,
	}); err != nil {
		t.Fatalf("发布插件 %s %s 失败: %v", key, version, err)
	}
	if _, err := pluginstore.New(db).Enable(ctx, key); err != nil {
		t.Fatalf("启用插件 %s 失败: %v", key, err)
	}
}

// reloadPluginSource 造一段「把 max_tokens 补成 n」的插件源码。
//
// 补丁值就是线上跑的是哪一代的观测量：上游收到的请求体里只有这一个数会变。
func reloadPluginSource(key, version, model string, maxTokens int) string {
	return fmt.Sprintf(`import { definePlugin } from "larkax";
export const meta = { key: %q, version: %q, apiVersion: 1, models: [%q] };
export default definePlugin({ onRequest() { return { body: { max_tokens: %d } }; } });
`, key, version, model, maxTokens)
}

// clearPluginRows 清空加载器会读的两张插件表。
//
// 这个包的测试库是共享的，而 SIGHUP 热更新是**整批**判定：别的用例留下的坏插件
// 会让重新装载先因为它失败，而那不是这条用例要看的东西。
func clearPluginRows(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{`DELETE FROM plugin_activations`, `DELETE FROM plugins`} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("清空插件表失败: %v", err)
		}
	}
}
