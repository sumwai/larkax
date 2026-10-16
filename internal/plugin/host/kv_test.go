package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// fakeKV 是一个按 (plugin, userID, key) 三元组存的 KV。
//
// 它**刻意在假实现里做命名空间隔离**：宿主函数把 plugin 与 userID 传错了，
// 这里就查不到别的用例写进去的值，于是「隔离」这件事是可断言的，
// 而不是靠人读代码相信它。
type fakeKV struct {
	data map[string]string
	ttls map[string]time.Duration

	calls   int
	lastCtx context.Context
	lastOp  string
	lastKey string
	err     error
	// delErr 只在 Del 上报错，用来覆盖「读成功、删失败」这条路径。
	delErr error
}

func newFakeKV() *fakeKV {
	return &fakeKV{data: map[string]string{}, ttls: map[string]time.Duration{}}
}

// testKVKey 是假实现内部的键格式。真实现怎么拼是它的事，
// 这里只要保证不同 (plugin, userID) 落在不同的位置上。
func testKVKey(plugin string, userID int64, key string) string {
	return fmt.Sprintf("%s\x00%d\x00%s", plugin, userID, key)
}

func (f *fakeKV) Get(ctx context.Context, plugin string, userID int64, key string) (string, bool, error) {
	f.calls++
	f.lastOp, f.lastKey, f.lastCtx = "get", key, ctx
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.data[testKVKey(plugin, userID, key)]
	return v, ok, nil
}

func (f *fakeKV) Set(ctx context.Context, plugin string, userID int64, key, value string, ttl time.Duration) error {
	f.calls++
	f.lastOp, f.lastKey, f.lastCtx = "set", key, ctx
	if f.err != nil {
		return f.err
	}
	k := testKVKey(plugin, userID, key)
	f.data[k], f.ttls[k] = value, ttl
	return nil
}

func (f *fakeKV) Del(ctx context.Context, plugin string, userID int64, key string) error {
	f.calls++
	f.lastOp, f.lastKey, f.lastCtx = "del", key, ctx
	if f.err != nil {
		return f.err
	}
	if f.delErr != nil {
		return f.delErr
	}
	k := testKVKey(plugin, userID, key)
	delete(f.data, k)
	delete(f.ttls, k)
	return nil
}

// kvEnv 造一个只有 KV 能力的 Env。
func kvEnv(store KVStore, plugin string, userID int64) Env {
	return Env{
		Plugin: plugin,
		UserID: userID,
		Logger: testLogger(&bytes.Buffer{}),
		KV:     store,
	}
}

// kvFunc 取出某个 KV 函数。
func kvFunc(t *testing.T, env Env, name string) func(context.Context, []moejs.Value) (moejs.Value, error) {
	t.Helper()
	return capability(t, Standard(env), capKV).Funcs[name]
}

func TestKVGetSetRoundTrip(t *testing.T) {
	store := newFakeKV()
	env := kvEnv(store, "p", 7)

	if _, err := kvFunc(t, env, "set")(context.Background(), js("k", "值")); err != nil {
		t.Fatalf("set 出错: %v", err)
	}
	got, err := kvFunc(t, env, "get")(context.Background(), js("k"))
	if err != nil {
		t.Fatalf("get 出错: %v", err)
	}
	if got.String() != "值" {
		t.Errorf("get = %q，期望 %q", got.String(), "值")
	}
}

// TestKVGetMissingReturnsNull 钉住「键不存在返回 null，而不是空串」。
// 空串是一个具体的值，与「没有这个键」是两件事。
func TestKVGetMissingReturnsNull(t *testing.T) {
	store := newFakeKV()
	env := kvEnv(store, "p", 7)

	got, err := kvFunc(t, env, "get")(context.Background(), js("没有这个键"))
	if err != nil {
		t.Fatalf("get 出错: %v", err)
	}
	if !got.IsNull() {
		t.Fatalf("get 未知键 = %q，期望 null", got.String())
	}

	// 空串是显式存进去的值，取回来不该变成 null。
	if _, setErr := kvFunc(t, env, "set")(context.Background(), js("空值", "")); setErr != nil {
		t.Fatalf("set 出错: %v", err)
	}
	got, err = kvFunc(t, env, "get")(context.Background(), js("空值"))
	if err != nil {
		t.Fatalf("get 出错: %v", err)
	}
	if got.IsNull() || got.String() != "" {
		t.Errorf("存进去的空串取回来 = %s，期望空串而非 null", got.String())
	}
}

// TestKVTTL 覆盖 ttlSeconds 的三种取值。
func TestKVTTL(t *testing.T) {
	cases := []struct {
		name string
		args []moejs.Value
		want time.Duration
	}{
		{"缺省表示不过期", js("k", "v"), 0},
		{"显式 0 表示不过期", js("k", "v", int64(0)), 0},
		{"正数按秒转", js("k", "v", int64(60)), time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeKV()
			env := kvEnv(store, "p", 7)

			if _, err := kvFunc(t, env, "set")(context.Background(), tc.args); err != nil {
				t.Fatalf("set 出错: %v", err)
			}
			got := store.ttls[testKVKey("p", 7, "k")]
			if got != tc.want {
				t.Errorf("ttl = %v，期望 %v", got, tc.want)
			}
		})
	}

	t.Run("负数报错", func(t *testing.T) {
		store := newFakeKV()
		env := kvEnv(store, "p", 7)
		_, err := kvFunc(t, env, "set")(context.Background(), js("k", "v", int64(-1)))
		if err == nil {
			t.Fatal("负的 ttl 没有明确含义，应当报错而不是猜一个")
		}
		if store.calls != 0 {
			t.Errorf("参数不合法却写了 %d 次", store.calls)
		}
	})

	t.Run("ttl 不是整数报错", func(t *testing.T) {
		store := newFakeKV()
		env := kvEnv(store, "p", 7)
		if _, err := kvFunc(t, env, "set")(context.Background(), js("k", "v", "一会儿")); err == nil {
			t.Fatal("非整数的 ttl 应当报错")
		}
	})
}

func TestKVDel(t *testing.T) {
	store := newFakeKV()
	env := kvEnv(store, "p", 7)

	if _, err := kvFunc(t, env, "set")(context.Background(), js("k", "v")); err != nil {
		t.Fatalf("set 出错: %v", err)
	}
	got, err := kvFunc(t, env, "del")(context.Background(), js("k"))
	if err != nil {
		t.Fatalf("del 出错: %v", err)
	}
	if !got.AsBool() {
		t.Error("删掉了一个存在的键，应当返回 true")
	}
	if _, ok := store.data[testKVKey("p", 7, "k")]; ok {
		t.Error("删完还在")
	}

	// 再删一次：这次它不存在，返回 false。
	got, err = kvFunc(t, env, "del")(context.Background(), js("k"))
	if err != nil {
		t.Fatalf("del 出错: %v", err)
	}
	if got.AsBool() {
		t.Error("删一个不存在的键，应当返回 false")
	}
}

// TestKVNamespaceIsolation 钉住命名空间隔离。
//
// 宿主函数必须把 plugin 与 userID 传给 KVStore；一旦漏传或传错，
// 一个用户的插件状态就会串到另一个用户身上，而那是跨用户的数据泄露。
// 这里用假实现断言「传对了参数」，并顺带验证不同 userID 互相看不见。
func TestKVNamespaceIsolation(t *testing.T) {
	store := newFakeKV()

	alice := kvEnv(store, "p", 1)
	bob := kvEnv(store, "p", 2)

	if _, err := kvFunc(t, alice, "set")(context.Background(), js("k", "alice 的值")); err != nil {
		t.Fatalf("set 出错: %v", err)
	}

	got, err := kvFunc(t, bob, "get")(context.Background(), js("k"))
	if err != nil {
		t.Fatalf("get 出错: %v", err)
	}
	if !got.IsNull() {
		t.Fatalf("另一个用户读到了别人的值: %q", got.String())
	}

	got, err = kvFunc(t, alice, "get")(context.Background(), js("k"))
	if err != nil {
		t.Fatalf("get 出错: %v", err)
	}
	if got.String() != "alice 的值" {
		t.Errorf("自己读自己的值 = %q", got.String())
	}

	// 同一个用户、不同插件之间也要隔离。
	other := kvEnv(store, "另一个插件", 1)
	got, err = kvFunc(t, other, "get")(context.Background(), js("k"))
	if err != nil {
		t.Fatalf("get 出错: %v", err)
	}
	if !got.IsNull() {
		t.Fatalf("另一个插件读到了别人的值: %q", got.String())
	}
}

// TestKVPassesPluginAndUserID 直接断言传给依赖的参数。
func TestKVPassesPluginAndUserID(t *testing.T) {
	for _, op := range []string{"get", "set", "del"} {
		t.Run(op, func(t *testing.T) {
			store := &recordingKV{}
			env := kvEnv(store, "计费插件", 42)

			args := js("k", "v")
			if op == "del" {
				args = js("k")
			}
			if _, err := kvFunc(t, env, op)(context.Background(), args); err != nil {
				t.Fatalf("%s 出错: %v", op, err)
			}
			if store.plugin != "计费插件" {
				t.Errorf("plugin 参数 = %q，期望 %q", store.plugin, "计费插件")
			}
			if store.userID != 42 {
				t.Errorf("userID 参数 = %d，期望 42", store.userID)
			}
			if store.op != op {
				t.Errorf("调用的操作 = %q，期望 %q", store.op, op)
			}
		})
	}
}

// recordingKV 只记录参数，不存数据。
type recordingKV struct {
	op      string
	plugin  string
	userID  int64
	key     string
	value   string
	ttl     time.Duration
	lastCtx context.Context
}

func (r *recordingKV) Get(ctx context.Context, plugin string, userID int64, key string) (string, bool, error) {
	r.op, r.plugin, r.userID, r.key, r.lastCtx = "get", plugin, userID, key, ctx
	return "", false, nil
}

func (r *recordingKV) Set(ctx context.Context, plugin string, userID int64, key, value string, ttl time.Duration) error {
	r.op, r.plugin, r.userID, r.key, r.value, r.ttl, r.lastCtx = "set", plugin, userID, key, value, ttl, ctx
	return nil
}

func (r *recordingKV) Del(ctx context.Context, plugin string, userID int64, key string) error {
	r.op, r.plugin, r.userID, r.key, r.lastCtx = "del", plugin, userID, key, ctx
	return nil
}

// TestKVRejectsBadArguments 钉住参数校验：报错且一次都不访问存储。
func TestKVRejectsBadArguments(t *testing.T) {
	cases := []struct {
		name  string
		fn    string
		args  []moejs.Value
		wantR string
	}{
		{"get 键缺失", "get", nil, "键不能为空"},
		{"get 键是 null", "get", js(nil), "键不能为空"},
		{"get 键是空串", "get", js(""), "键不能为空"},
		{"set 键缺失", "set", js("", "v"), "键不能为空"},
		{"del 键缺失", "del", nil, "键不能为空"},
		{"键过长", "get", js(strings.Repeat("k", kvKeyMaxBytes+1)), "字节上限"},
		{"值过长", "set", js("k", strings.Repeat("v", kvValueMaxBytes+1)), "字节上限"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeKV()
			env := kvEnv(store, "p", 7)

			_, err := kvFunc(t, env, tc.fn)(context.Background(), tc.args)
			if err == nil {
				t.Fatal("非法参数应当报错，而不是静默按零值处理")
			}
			if !strings.Contains(err.Error(), tc.wantR) {
				t.Errorf("错误 = %v，期望提到 %q", err, tc.wantR)
			}
			if store.calls != 0 {
				t.Errorf("参数不合法却访问了 %d 次存储", store.calls)
			}
		})
	}
}

// TestKVStoreErrorSurfaces 确认存储出错时如实上报，不吞掉。
func TestKVStoreErrorSurfaces(t *testing.T) {
	sentinel := errors.New("存储不可用")
	for _, op := range []string{"get", "set", "del"} {
		t.Run(op, func(t *testing.T) {
			store := newFakeKV()
			store.err = sentinel
			env := kvEnv(store, "p", 7)

			args := js("k", "v")
			if op == "del" {
				args = js("k")
			}
			_, err := kvFunc(t, env, op)(context.Background(), args)
			if !errors.Is(err, sentinel) {
				t.Fatalf("错误 = %v，期望包住 %v", err, sentinel)
			}
		})
	}
}

// TestKVDelFailsWhenStoreErrors 覆盖 del 的取-删两步里第一步就失败的路径。
func TestKVDelFailsWhenStoreErrors(t *testing.T) {
	sentinel := errors.New("存储不可用")
	store := newFakeKV()
	env := kvEnv(store, "p", 7)
	if _, err := kvFunc(t, env, "set")(context.Background(), js("k", "v")); err != nil {
		t.Fatalf("set 出错: %v", err)
	}

	// 让 get 失败、del 正常：确认第一步失败时不会继续删。
	store.err = sentinel
	_, err := kvFunc(t, env, "del")(context.Background(), js("k"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误 = %v，期望包住 %v", err, sentinel)
	}
	if _, ok := store.data[testKVKey("p", 7, "k")]; !ok {
		t.Error("读失败时不该已经把键删掉了")
	}
}

// TestKVDelFailsWhenDeleteErrors 覆盖「读到了、删失败」这条路径：
// 删失败必须报错，而不是带着一个「删掉了」的布尔值返回。
func TestKVDelFailsWhenDeleteErrors(t *testing.T) {
	sentinel := errors.New("删除失败")
	store := newFakeKV()
	env := kvEnv(store, "p", 7)
	if _, err := kvFunc(t, env, "set")(context.Background(), js("k", "v")); err != nil {
		t.Fatalf("set 出错: %v", err)
	}
	store.delErr = sentinel

	_, err := kvFunc(t, env, "del")(context.Background(), js("k"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误 = %v，期望包住 %v", err, sentinel)
	}
	if _, ok := store.data[testKVKey("p", 7, "k")]; !ok {
		t.Error("删失败了键却没了，说明假实现写错了")
	}
}

// TestKVNotAvailableWithoutStore 钉住 Available()。
func TestKVNotAvailableWithoutStore(t *testing.T) {
	c := capability(t, Standard(Env{}), capKV)
	if c.Available() {
		t.Error("KV 为 nil 时 Available() 应当为 false")
	}
}
