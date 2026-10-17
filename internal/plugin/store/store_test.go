package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	"github.com/sumwai/larkax/internal/testdb"
)

// allCaps 是宿主认识的全部能力名。
//
// 下面这几个加载用例测的是「读表、编译、发布」，不是能力收窄——按这次装配
// 提供了什么收窄那一条另有用例（见 TestLoadRegistryNarrowsByProvided）。
func allCaps() []string { return sandbox.CapabilityNames() }

// testDB 连上集成测试用的库，连不上就跳过。
//
// 这一层要测的是「读写这两张表的 SQL 对不对」——唯一键撞不撞、代次算得对
// 不对、jsonb 写进去读出来还是不是同一个。拿内存假装这些，测的是「我以为
// 数据库会怎么做」。
//
// **没有 LARKAX_DB_DSN 时跳过而不是失败**：本机未必起着库。注意「跳过」与
// 「通过」在输出里长得一样，所以这个包也进了 CI 的那道集成测试门禁
// （见 .github/actions/check-integration-tests）。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// 每个用例用独立的 key，避免彼此干扰，也避免撞上上一次运行留下的行。
//
// 用「时间戳 + 进程内计数」而不是纯计数：纯计数在同一个库上跑第二次
// `go test` 时会从头开始，撞上上一次留下的行。
var seq struct {
	sync.Mutex
	n int64
}

var base = time.Now().UnixNano() % 1_000_000_000

func uniqueKey(prefix string) string {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return fmt.Sprintf("%s-%d-%d", prefix, base, seq.n)
}

// goodSource 是一段能编译过的插件源码。
//
// 与 internal/plugin 测试里那份是两处（那边的 src 助手不导出）：这里要的是
// 「一段真实可编译的源码」，而它的内容不参与断言。
func goodSource(key, version string) string {
	return fmt.Sprintf(`
import { definePlugin } from "larkax";
export const meta = { key: %q, version: %q, apiVersion: 1 };
export default definePlugin({ onUsage() { return { promptTokens: 1 }; } });
`, key, version)
}

// brokenSource 是编译不过的源码：语法错。
const brokenSource = "export default { onUsage() { "

// goodMeta 是一份**完整**的 meta：三个字段都在。
//
// 少写 apiVersion 的话 ParseMeta 会判它不合规（契约版本缺失），于是「meta
// 正常」的那条用例也会被标出来——那不是被测对象的问题，是夹具没写全。
func goodMeta(key, version string) string {
	return fmt.Sprintf(`{"key":%q,"version":%q,"apiVersion":1}`, key, version)
}

// insertPlugin 直接写一行，绕过 Publish。
//
// 用例要构造的是**库里已有的状态**（一条编译不过的 active 行、一行坏 meta），
// 而 Publish 只产生它自己那一种——它拒绝编译不过的插件，也不接受坏 meta。
// 这一条路正是「手工改库」在测试里的等价物。
func insertPlugin(t *testing.T, db *sql.DB, key, version, status string, generation int64) {
	t.Helper()
	insertPluginRaw(t, db, key, version, status, generation, goodMeta(key, version), goodSource(key, version))
}

// insertPluginRaw 给全每一列——用来造编译不过、meta 不合规这类**库里已有的坏状态**。
//
// 单独一个入口而不是让 insertPlugin 多两个参数：绝大多数用例要的是「一行
// 正常数据」，多出来的参数会让每个调用点都多两个看不懂的实参。
func insertPluginRaw(t *testing.T, db *sql.DB, key, version, status string,
	generation int64, meta, source string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(),
		`INSERT INTO plugins (key, version, name, source, meta, status, generation)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)`,
		key, version, "测试插件", source, meta, status, generation)
	if err != nil {
		t.Fatalf("插入插件行失败: %v", err)
	}
}

// ── 加载 ──────────────────────────────────────────────────────────────

// 判据：加载器真的把插件装上去了。
//
// 库里放两条 active 的，Publish 出来的注册表里就有两个。
func TestLoadRegistryLoadsActivePlugins(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	a, b := uniqueKey("load-a"), uniqueKey("load-b")
	insertPlugin(t, db, a, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, b, "1.0.0", StatusActive, 0)

	reg, err := New(db).LoadRegistry(ctx, discardLogger(), allCaps())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	for _, key := range []string{a, b} {
		if _, ok := reg.Get(key); !ok {
			t.Errorf("插件 %s 该被装上，实际不在注册表里", key)
		}
	}
}

// kvAskingSource 是一段申请了 kv 的源码，用来测「允许集由调用方给的那一份决定」。
func kvAskingSource(key string) string {
	return fmt.Sprintf(`
import { definePlugin } from "larkax";
export const meta = { key: %q, version: "1.0.0", apiVersion: 1, capabilities: ["kv"] };
export default definePlugin({ onUsage() { return { promptTokens: 1 }; } });
`, key)
}

// 判据：加载器按**调用方给的**那份能力清单收窄，它自己不认识任何能力名。
//
// 这是「两份清单同源」在存储这一层的落点：清单只有一处算得出来（宿主那一侧，
// 按这次装配接上了什么算），本包把它传下去就是了。这里给一份只有 log 的清单，
// 申请 kv 的插件就进不来；给上 kv，同一行数据就上线。
//
// 少了「给上 kv 就上线」那一半，一个什么都不发布的实现也能过。
func TestLoadRegistryNarrowsByProvided(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	key := uniqueKey("load-kv")
	insertPluginRaw(t, db, key, "1.0.0", StatusActive, 0,
		goodMeta(key, "1.0.0"), kvAskingSource(key))

	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))

	reg, err := New(db).LoadRegistry(ctx, logger, []string{sandbox.CapabilityLog})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, ok := reg.Get(key); ok {
		t.Error("这份装配没提供 kv，申请 kv 的插件不该上线——它该在注册期就失败")
	}
	// 跳过是安静的：日志里不点名的话，线上看到的就是「我那个插件没生效」。
	if !strings.Contains(log.String(), key) {
		t.Errorf("跳过的插件该在日志里被点名:\n%s", log.String())
	}

	reg, err = New(db).LoadRegistry(ctx, discardLogger(),
		[]string{sandbox.CapabilityLog, sandbox.CapabilityKV})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	p, ok := reg.Get(key)
	if !ok {
		t.Fatal("清单里提供了 kv，这个插件该上线")
	}
	if len(p.Capabilities) != 1 || p.Capabilities[0] != sandbox.CapabilityKV {
		t.Errorf("生效的能力 = %v，期望 [%s]", p.Capabilities, sandbox.CapabilityKV)
	}
}

// 只有 status='active' 参与注册：另外三种状态都不上。
//
// 这一条钉的是「status 是唯一的开关」——把 WHERE 去掉、或者把 disabled
// 也放进来，它立刻挂。
func TestActiveReadsOnlyActivePlugins(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	active := uniqueKey("st-active")
	pending := uniqueKey("st-pending")
	disabled := uniqueKey("st-disabled")
	failed := uniqueKey("st-failed")
	insertPlugin(t, db, active, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, pending, "1.0.0", StatusPending, 0)
	insertPlugin(t, db, disabled, "1.0.0", StatusDisabled, 0)
	insertPlugin(t, db, failed, "1.0.0", StatusFailed, 0)

	regs, err := New(db).Active(ctx)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	got := map[string]bool{}
	for _, r := range regs {
		got[r.Key] = true
	}
	if !got[active] {
		t.Error("active 的插件该被读出来")
	}
	for _, key := range []string{pending, disabled, failed} {
		if got[key] {
			t.Errorf("状态不是 active 的插件 %s 不该参与注册", key)
		}
	}
}

// 同一个 key 有多代时只取**代次最高**的那一行。
//
// 这是 generation 那一列的用途：发布一次加一，当前生效的只有最高的一代。
// 取错（例如取最低的一代）在库里看不出任何异常——两个版本都是 active。
func TestActivePicksHighestGeneration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	key := uniqueKey("gen")
	insertPlugin(t, db, key, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, key, "2.0.0", StatusActive, 1)

	regs, err := New(db).Active(ctx)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	var found int
	for _, r := range regs {
		if r.Key != key {
			continue
		}
		found++
		if !strings.Contains(r.Source, `"2.0.0"`) {
			t.Errorf("该取代次最高的那一行（2.0.0），实际取到 %s", r.Source)
		}
	}
	if found != 1 {
		t.Fatalf("同一个 key 该只出一行，实际 %d 行", found)
	}
}

// 优先级取自 platform 作用域的启用记录；没有记录时是 0。
func TestActiveReadsPlatformPriority(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	withPriority := uniqueKey("prio-yes")
	without := uniqueKey("prio-no")
	insertPlugin(t, db, withPriority, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, without, "1.0.0", StatusActive, 0)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO plugin_activations (plugin_key, scope, priority) VALUES ($1, 'platform', 7)`,
		withPriority); err != nil {
		t.Fatalf("插入启用记录失败: %v", err)
	}
	// 别的作用域的记录不该被当成平台级：它说的是「某个用户启用了」。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO plugin_activations (plugin_key, scope, priority) VALUES ($1, 'channel', 99)`,
		without); err != nil {
		t.Fatalf("插入渠道级启用记录失败: %v", err)
	}

	regs, err := New(db).Active(ctx)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	got := map[string]int{}
	for _, r := range regs {
		got[r.Key] = r.Priority
	}
	if got[withPriority] != 7 {
		t.Errorf("platform 记录的优先级该是 7，实际 %d", got[withPriority])
	}
	if got[without] != 0 {
		t.Errorf("只有 channel 级记录时优先级该是 0（那是按渠道启用，不是平台级），实际 %d", got[without])
	}
}

// 判据：坏插件被跳过，好的仍然上线，而且日志里点得出是哪一个。
//
// 「点得出是哪一个」是这条用例的重点：一条只说「有个插件编译不过」的日志，
// 在库里有几十个插件时等于没说。
func TestLoadRegistrySkipsBrokenPlugin(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	good := uniqueKey("skip-good")
	broken := uniqueKey("skip-broken")
	insertPlugin(t, db, good, "1.0.0", StatusActive, 0)
	// 坏的那条要**写进去的时候就是坏的**：先插一条好源码、再 UPDATE 改掉的话，
	// 这一条测的就成了「那句 UPDATE 有没有生效」，而不是「加载器怎么处置坏插件」。
	insertPluginRaw(t, db, broken, "1.0.0", StatusActive, 0,
		goodMeta(broken, "1.0.0"), brokenSource)

	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))

	reg, err := New(db).LoadRegistry(ctx, logger, allCaps())
	if err != nil {
		t.Fatalf("一个坏插件不该让加载整体失败: %v", err)
	}
	if _, ok := reg.Get(good); !ok {
		t.Errorf("好的插件 %s 该照常上线", good)
	}
	if _, ok := reg.Get(broken); ok {
		t.Errorf("编译不过的插件 %s 不该上线", broken)
	}
	// 日志里要有坏插件的 key，也要有正向证据（装上了几个）。
	for _, want := range []string{broken, "已跳过", good} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("日志里没有 %q：\n%s", want, log.String())
		}
	}
}

// 判据：加载器坏了与插件坏了分得开。
//
// DSN 指到一个连不上的地址时，错误必须说的是**连接/读表**，而不是
// 「某个插件编译失败」——两者的下一步动作完全不同（一个去看库，一个去修插件）。
func TestLoadRegistryReportsLoaderFailureNotPluginFailure(t *testing.T) {
	// 127.0.0.1:1 上不会有人监听。sql.Open 是惰性的，失败发生在查询那一步。
	db, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("打开连接失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = New(db).LoadRegistry(ctx, discardLogger(), allCaps())
	if err == nil {
		t.Fatal("连不上库时该报错")
	}
	if !strings.Contains(err.Error(), "读取 plugins 表失败") {
		t.Errorf("错误信息 %q 里没有「读取 plugins 表失败」：读表失败要说清是加载器的问题", err.Error())
	}
	for _, wrong := range []string{"编译", "无法注册", "已跳过"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("错误信息 %q 里出现了 %q：读表失败不该被说成插件坏了", err.Error(), wrong)
		}
	}
}

// ── 发布 ──────────────────────────────────────────────────────────────

// 发布一个新版本：插入一行新代次，状态是 pending。
//
// 「发布了不等于启用了」是这一条的落点：新版本要 enable 才生效，而直接
// 写成 active 的话，一次发布就等于一次上线，回退只能靠停用。
func TestPublishInsertsNewVersionAsPending(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	key := uniqueKey("pub-new")
	res, err := s.Publish(ctx, Publication{
		Key: key, Version: "1.0.0", Name: "测试插件",
		Source: goodSource(key, "1.0.0"), Meta: []byte(`{"key":"` + key + `"}`),
	})
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if res.Overwritten {
		t.Error("第一次发布不该是覆盖")
	}
	if res.Generation != 0 {
		t.Errorf("第一个版本的代次该是 0，实际 %d", res.Generation)
	}
	if res.Status != StatusPending {
		t.Errorf("新发布的插件状态该是 %s，实际 %s", StatusPending, res.Status)
	}

	// 换一个版本：新行，代次加一。
	res, err = s.Publish(ctx, Publication{
		Key: key, Version: "2.0.0", Name: "测试插件",
		Source: goodSource(key, "2.0.0"), Meta: []byte(`{"key":"` + key + `"}`),
	})
	if err != nil {
		t.Fatalf("发布第二个版本失败: %v", err)
	}
	if res.Overwritten {
		t.Error("换版本该是插入新行，不是覆盖")
	}
	if res.Generation != 1 {
		t.Errorf("第二个版本的代次该是 1，实际 %d", res.Generation)
	}

	// 两行都在：同 key 多代是设计支持的。
	rows := rowsFor(t, db, key)
	if len(rows) != 2 {
		t.Fatalf("该有 2 行（两个版本），实际 %d 行", len(rows))
	}
}

// 同一个 (key, version) 重复发布是**覆盖**：代次加一，状态保持原值。
//
// 「状态保持原值」是这一条的重点：已启用的插件不该因为改了一个错别字就下线。
// 写成「覆盖时重置成 pending」的话，改一次注释就等于停用一次插件，而它在
// 命令行上只表现为一行「已覆盖」。
func TestPublishOverwritesSameVersion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	key := uniqueKey("pub-over")
	pub := Publication{
		Key: key, Version: "1.0.0", Name: "测试插件",
		Source: goodSource(key, "1.0.0"), Meta: []byte(`{"key":"` + key + `"}`),
	}
	if _, err := s.Publish(ctx, pub); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}
	if _, err := s.Enable(ctx, key); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	pub.Source = goodSource(key, "1.0.0") + "\n// 改了一个错别字\n"
	res, err := s.Publish(ctx, pub)
	if err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if !res.Overwritten {
		t.Error("同一个 (key, version) 的第二次发布该报成覆盖")
	}
	if res.Generation != 1 {
		t.Errorf("覆盖之后代次该是 1，实际 %d", res.Generation)
	}
	if res.Status != StatusActive {
		t.Errorf("覆盖不该改启用状态：该仍是 %s，实际 %s", StatusActive, res.Status)
	}

	rows := rowsFor(t, db, key)
	if len(rows) != 1 {
		t.Fatalf("覆盖该只留一行，实际 %d 行", len(rows))
	}
	if !strings.Contains(rows[0].source, "错别字") {
		t.Error("覆盖之后源码该是新的")
	}
}

// 优先级写进 platform 作用域的启用记录，重复发布时更新同一个值。
func TestPublishWritesPriority(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	key := uniqueKey("pub-prio")
	pub := Publication{
		Key: key, Version: "1.0.0", Name: "测试插件",
		Source: goodSource(key, "1.0.0"), Meta: []byte(`{"key":"` + key + `"}`),
		Priority: 5,
	}
	if _, err := s.Publish(ctx, pub); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if got := priorityFor(t, db, key); got != 5 {
		t.Fatalf("优先级该是 5，实际 %d", got)
	}

	pub.Priority = 9
	if _, err := s.Publish(ctx, pub); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if got := priorityFor(t, db, key); got != 9 {
		t.Fatalf("覆盖之后优先级该是 9，实际 %d", got)
	}
}

// ── 启用 / 停用 ───────────────────────────────────────────────────────

// 停用必须把**每一代**都按下去。
//
// 只按最高那一代的话，加载器会落到上一代上——表现为「停用了，但它换了个
// 版本继续跑」，而库里两行都还是 active 与 disabled 混着，看不出问题。
func TestDisableStopsEveryGeneration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	key := uniqueKey("enable")
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if _, err := s.Publish(ctx, Publication{
			Key: key, Version: v, Name: "测试插件",
			Source: goodSource(key, v), Meta: []byte(`{"key":"` + key + `"}`),
		}); err != nil {
			t.Fatalf("发布 %s 失败: %v", v, err)
		}
	}

	if _, err := s.Enable(ctx, key); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	regs, err := s.Active(ctx)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	if !hasKey(regs, key) {
		t.Fatal("启用之后该参与注册")
	}

	if _, disableErr := s.Disable(ctx, key); disableErr != nil {
		t.Fatalf("停用失败: %v", disableErr)
	}
	regs, err = s.Active(ctx)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	if hasKey(regs, key) {
		t.Error("停用之后不该再参与注册")
	}
	for _, row := range rowsFor(t, db, key) {
		if row.status != StatusDisabled {
			t.Errorf("停用之后每一代都该是 %s，%s 那一代实际是 %s",
				StatusDisabled, row.version, row.status)
		}
	}
}

// key 在库里没有对应行时返回 ErrPluginNotFound。
//
// 命令行按它给出「约束冲突」那一档退出码：与「参数写错了」的下一步动作不同。
func TestEnableUnknownKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	_, err := New(db).Enable(ctx, uniqueKey("不存在"))
	if err == nil {
		t.Fatal("启用一个不存在的 key 该报错")
	}
	if !strings.Contains(err.Error(), ErrPluginNotFound.Error()) {
		t.Errorf("错误信息 %q 里没有 %q：脚本要能认出这是「库里没有这一行」",
			err.Error(), ErrPluginNotFound.Error())
	}
}

// ── 清单 ──────────────────────────────────────────────────────────────

// 判据：meta 有问题的行要**被标出来**，而不是让整个清单崩掉或跳过它。
//
// 这一条在真库上构造得出来，因为 meta 是 jsonb：**语法**由列类型保证，
// 而**形状**（是不是一个对象、有没有 key、key 与行上的对不对得上）没有任何
// 约束拦得住。三种坏形状各一条。
func TestListFlagsBadMeta(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	notObject := uniqueKey("meta-arr")
	keyMismatch := uniqueKey("meta-key")
	noKey := uniqueKey("meta-none")
	good := uniqueKey("meta-good")

	insertPluginRaw(t, db, notObject, "1.0.0", StatusActive, 0, `[]`, goodSource(notObject, "1.0.0"))
	insertPluginRaw(t, db, keyMismatch, "1.0.0", StatusActive, 0, `{"key":"另一个名字"}`, goodSource(keyMismatch, "1.0.0"))
	insertPluginRaw(t, db, noKey, "1.0.0", StatusActive, 0, `{"name":"没有 key"}`, goodSource(noKey, "1.0.0"))
	insertPlugin(t, db, good, "1.0.0", StatusActive, 0)

	rows, err := New(db).List(ctx)
	if err != nil {
		t.Fatalf("清单不该因为一行坏 meta 而失败: %v", err)
	}

	problems := map[string]string{}
	for _, row := range rows {
		problems[row.Key] = row.MetaProblem
	}
	for _, key := range []string{notObject, keyMismatch, noKey} {
		if problems[key] == "" {
			t.Errorf("插件 %s 的 meta 有问题，清单里该标出来", key)
		}
	}
	if problems[good] != "" {
		t.Errorf("插件 %s 的 meta 没问题，不该被标出来：%q", good, problems[good])
	}
	// 坏掉的行**仍然要出现在清单里**：整次报错或跳过它，都会让最需要看清单的
	// 那一刻看不到东西。
	if _, ok := problems[notObject]; !ok {
		t.Error("meta 坏掉的行不该从清单里消失")
	}
}

// ── 助手 ──────────────────────────────────────────────────────────────

type pluginRow struct {
	version string
	status  string
	source  string
}

func rowsFor(t *testing.T, db *sql.DB, key string) []pluginRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT version, status, source FROM plugins WHERE key = $1 ORDER BY generation`, key)
	if err != nil {
		t.Fatalf("查插件行失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []pluginRow
	for rows.Next() {
		var r pluginRow
		if err := rows.Scan(&r.version, &r.status, &r.source); err != nil {
			t.Fatalf("读插件行失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历插件行失败: %v", err)
	}
	return out
}

func priorityFor(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var p int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(MAX(priority), -1) FROM plugin_activations
		  WHERE plugin_key = $1 AND scope = 'platform'`, key).Scan(&p); err != nil {
		t.Fatalf("查优先级失败: %v", err)
	}
	return p
}

func hasKey(regs []plugin.Registration, key string) bool {
	for _, r := range regs {
		if r.Key == key {
			return true
		}
	}
	return false
}

// discardLogger 让不需要看日志的用例不往测试输出里灌东西。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}
