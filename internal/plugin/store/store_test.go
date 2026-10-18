package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/migrate"
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

// insertFailedPlugin 造一行「编译失败」，并带上错误原文。
//
// 分开一个入口而不是让 insertPluginRaw 多一个参数：**没有哪条命令会写下这一行**
// （publish 拒绝编译不过的插件，而加载器只把「没装成」写进 last_load_result
// 那三列，status 与 compile_error 都不动），所以它是「手工改库」在用例里唯一的
// 样子，而它要的列与那一个入口其余调用点完全不同。
func insertFailedPlugin(t *testing.T, db *sql.DB, key, version string, generation int64, compileErr string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(),
		`INSERT INTO plugins (key, version, name, source, meta, status, generation, compile_error)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)`,
		key, version, "测试插件", brokenSource, goodMeta(key, version), StatusFailed, generation, compileErr)
	if err != nil {
		t.Fatalf("插入编译失败的行失败: %v", err)
	}
}

// markLoad 手工写下加载期那一次的结果，指名道姓落在 (key, version) 那一行上。
//
// 用的是加载器写回时那一条 SQL（见 recordLoadResultSQL）：用例要造的是「库里已经
// 是这个状态」，而走一遍加载只造得出这次装配给出的那一种结果，读侧那一半要靠
// 别的状态才测得到。共用同一条语句，夹具也就不会与写回那一侧各写各的。
//
// 传版本名而不是代次，理由与写回那一侧同一条：代次会撞车，按它写会改到两行。
func markLoad(t *testing.T, db *sql.DB, key, version, result, errText string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), recordLoadResultSQL,
		key, version, result, nullableText(errText)); err != nil {
		t.Fatalf("写下加载结果失败: %v", err)
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

// 判据：加载完把这一次的结果写回库——跳过的那一代写「没装成」，连同错误原文。
//
// 四件事一起钉住：
//
//   - 结果、原文、时刻都落了库（没时刻就分不出是哪一次启动写下的）；
//   - **status 一个字都没动**：跳过不改状态，所以下次启动照样会再试一次。把它
//     写成 failed 会让「这次装配没提供这个能力」看起来像一个永久的事实；
//   - compile_error 也不动，那是发布期那一列；
//   - 再加载一次、这次装配给上了 kv，同一行刷成 ok 且错误原文清回 NULL——
//     留着上一次的原文，读的人会以为这一次也失败了。
func TestLoadRegistryWritesBackAndClearsLoadResult(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	key := uniqueKey("load-back")
	insertPluginRaw(t, db, key, "1.0.0", StatusActive, 0, goodMeta(key, "1.0.0"), kvAskingSource(key))

	// 这一次装配没有 kv：这个插件在注册期就被跳过。
	if _, err := New(db).LoadRegistry(ctx, discardLogger(), []string{sandbox.CapabilityLog}); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	row := rowsFor(t, db, key)[0]
	if row.lastLoadResult.String != LoadResultFailed {
		t.Errorf("没装成该写下 %s，实际是 %q", LoadResultFailed, row.lastLoadResult.String)
	}
	if !strings.Contains(row.lastLoadError.String, "能力") {
		t.Errorf("错误原文该落在库里（它说的是这一次为什么没装成）：%q", row.lastLoadError.String)
	}
	if !row.lastLoadAt.Valid {
		t.Error("加载结果该带着时刻")
	}
	if row.status != StatusActive {
		t.Errorf("跳过不改状态：该仍是 %s，实际 %s", StatusActive, row.status)
	}
	if row.compileError.Valid {
		t.Errorf("发布期那一列不该被加载器动：%q", row.compileError.String)
	}

	// 再启动一次，这一次装配提供了 kv：结果刷成 ok，原文清掉。
	if _, err := New(db).LoadRegistry(ctx, discardLogger(), allCaps()); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	row = rowsFor(t, db, key)[0]
	if row.lastLoadResult.String != LoadResultOK {
		t.Errorf("装上了该写下 %s，实际是 %q", LoadResultOK, row.lastLoadResult.String)
	}
	if row.lastLoadError.Valid {
		t.Errorf("装上了之后错误原文该回到 NULL，实际是 %q", row.lastLoadError.String)
	}
	if row.status != StatusActive {
		t.Errorf("写回结果不该改状态：该仍是 %s，实际 %s", StatusActive, row.status)
	}
}

// 判据：写回那一步把「谁装上了、谁被跳过」翻对，且按 key 查而不是按下标对。
//
// 不连库：这一段是纯判断，跳过的那几个不进注册表，两边下标对不上——写错的表现是
// 把错误原文安到另一个插件头上，而库里看不出异常。
func TestLoadRecordsMarksSkippedRowsByKey(t *testing.T) {
	rows := []activeRow{
		{reg: plugin.Registration{Key: "ok-one"}, version: "1.0.0"},
		{reg: plugin.Registration{Key: "broken"}, version: "2.0.0"},
		{reg: plugin.Registration{Key: "ok-two"}, version: "3.0.0"},
	}
	recs := loadRecords(rows, map[string]error{"broken": errors.New("申请了未开放的能力: kv")})

	if len(recs) != len(rows) {
		t.Fatalf("写回的条数 = %d，想要 %d（每一行都要有结果）", len(recs), len(rows))
	}
	for i, rec := range recs {
		if rec.key != rows[i].reg.Key || rec.version != rows[i].version {
			t.Errorf("第 %d 条写的不是它读到的那一行：%+v", i, rec)
		}
		wantResult := LoadResultOK
		if rec.key == "broken" {
			wantResult = LoadResultFailed
		}
		if rec.result != wantResult {
			t.Errorf("%s 的结果 = %q，想要 %q", rec.key, rec.result, wantResult)
		}
	}
	if recs[1].errorText == "" {
		t.Error("被跳过的那一条要带上错误原文，否则管理面上只有「没装成」三个字")
	}
	for _, i := range []int{0, 2} {
		if recs[i].errorText != "" {
			t.Errorf("装上了的那一条不该带原文：%q", recs[i].errorText)
		}
	}
}

// fakeExec 记下每一次写回，并按 failOn 决定那一次报不报错（从 1 数，0 表示都成功）。
type fakeExec struct {
	writes [][]any
	failOn int
}

func (f *fakeExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	f.writes = append(f.writes, args)
	if f.failOn == len(f.writes) {
		return nil, errors.New("库连不上了")
	}
	return fakeResult{}, nil
}

// fakeResult 是写回那一步用不到的返回值。
//
// *sql.DB.ExecContext 必须交回一个 sql.Result，而 recordLoadResults 只看错误。
// 给一个零值实现而不是 `return nil, nil`：nil 错误配一个无效的值是 golangci-lint
// 的 nilnil 咬的那一种写法，而它在这里没有要表达的语义——「写成功了，但我拿不到
// 结果」这个让步是接口形状逼出来的，不是这一处的意思。
type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 0, nil }

// 判据：写回的实参对得上（成功的那一条把原文写成 NULL，目标行是刚读的那一行）。
func TestRecordLoadResultsWritesBothOutcomes(t *testing.T) {
	ex := &fakeExec{}
	recs := []loadRecord{
		{key: "a", version: "1.0.0", result: LoadResultOK},
		{key: "b", version: "3.0.0", result: LoadResultFailed, errorText: "申请了未开放的能力: kv"},
	}

	recordLoadResults(context.Background(), ex, discardLogger(), recs)

	if len(ex.writes) != len(recs) {
		t.Fatalf("写了 %d 次，想要 %d 次", len(ex.writes), len(recs))
	}
	// 目标行是 (key, version)：写回落在刚读的那一行上，不必再拼一遍「取中哪一行」
	// 那条判据。用版本名而不是代次，是因为代次会撞车（见 recordLoadResultSQL）。
	if got := ex.writes[1]; got[0] != "b" || got[1] != "3.0.0" {
		t.Errorf("第二条写的不是 (b, 3.0.0)：%v", got)
	}
	// 装上了那一条把错误原文写成 NULL，不是空串：那一列的空串读起来像一个值。
	if got := ex.writes[0][3]; got != nil {
		t.Errorf("装上了的错误原文该是 NULL，实际 %#v", got)
	}
	if got := ex.writes[1][3]; got != "申请了未开放的能力: kv" {
		t.Errorf("没装成那一条该带上原文，实际 %#v", got)
	}
}

// 判据：一条写不进去不拖垮其余的，而且只记日志。
//
// 这一条钉的是启动路径上那条纪律——插件是可选增强，管理面上少一栏记录不该让服务
// 起不来。recordLoadResults 没有返回值，所以「不外传错误」这件事在签名上就成立，
// 这里钉的是它的另一半：失败之后还接着写，并且点名是哪一个插件。
func TestRecordLoadResultsLogsFailureAndKeepsGoing(t *testing.T) {
	var log bytes.Buffer
	ex := &fakeExec{failOn: 1}
	recs := []loadRecord{
		{key: "first", version: "1.0.0", result: LoadResultOK},
		{key: "second", version: "1.0.0", result: LoadResultOK},
	}

	recordLoadResults(context.Background(), ex, slog.New(slog.NewTextHandler(&log, nil)), recs)

	if len(ex.writes) != len(recs) {
		t.Errorf("第一条写失败之后该接着写剩下的：写了 %d 次，想要 %d 次", len(ex.writes), len(recs))
	}
	if !strings.Contains(log.String(), "first") {
		t.Errorf("写回失败该在日志里点名是哪一个插件：\n%s", log.String())
	}
	if strings.Contains(log.String(), "second") {
		t.Errorf("没失败的那一条不该被点名：\n%s", log.String())
	}
}

// activeRegs 读一遍参与注册的行（见 Store.activeRows），只取用例关心的那几样。
//
// 用例要的是「哪些 key 会被装上、带着什么优先级」，而写回加载结果那一步要的版本名
// 在这里没有用——包内直接借 activeRows，就不必为测试留着一条只被测试调用的
// 导出方法。
func activeRegs(t *testing.T, s *Store) []plugin.Registration {
	t.Helper()
	rows, err := s.activeRows(context.Background())
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	out := make([]plugin.Registration, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.reg)
	}
	return out
}

// 只有 status='active' 参与注册：另外三种状态都不上。
//
// 这一条钉的是「status 是唯一的开关」——把 WHERE 去掉、或者把 disabled
// 也放进来，它立刻挂。
func TestActiveReadsOnlyActivePlugins(t *testing.T) {
	db := testDB(t)
	active := uniqueKey("st-active")
	pending := uniqueKey("st-pending")
	disabled := uniqueKey("st-disabled")
	failed := uniqueKey("st-failed")
	insertPlugin(t, db, active, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, pending, "1.0.0", StatusPending, 0)
	insertPlugin(t, db, disabled, "1.0.0", StatusDisabled, 0)
	insertPlugin(t, db, failed, "1.0.0", StatusFailed, 0)

	regs := activeRegs(t, New(db))
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
	key := uniqueKey("gen")
	insertPlugin(t, db, key, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, key, "2.0.0", StatusActive, 1)

	regs := activeRegs(t, New(db))
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

	regs := activeRegs(t, New(db))
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

// 同一个 (key, version) 重复发布是**覆盖**：代次取该 key 的最大值加一，状态保持原值。
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

// 判据：覆盖取的是**整个 key** 的最大代次加一，于是「代次最高」「最新一次发布」与
// 「加载器取中的」重新是同一件事。
//
// 三个问题各问一次，因为它们走三条不同的读路径（加载器的 activeRows、Get 那一份
// SQL、清单里 latest 那一栏）。改动之前覆盖只把那一行**自己**的代次加一：A(1.0.0)
// 是 0、B(2.0.0) 是 1，重发 A 之后 A 也成了 1，与 B 并列；而并列由 pickedOrder 收尾
// （代次一样就比版本名），于是加载器与 Get 给的会是 **B**，清单里两行都说自己是
// 「最新一次尝试」。这一条在那一刻是红的，而库与命令行上什么都看不出来。
func TestPublishOverwriteTakesMaxGenerationPlusOne(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	key := uniqueKey("pub-max")
	pub := Publication{
		Key: key, Version: "1.0.0", Name: "测试插件",
		Source: goodSource(key, "1.0.0"), Meta: []byte(`{"key":"` + key + `"}`),
	}

	// A@0，启用它：只有 active 的那一代参与注册。
	if _, err := s.Publish(ctx, pub); err != nil {
		t.Fatalf("发布 1.0.0 失败: %v", err)
	}
	if _, err := s.Enable(ctx, key); err != nil {
		t.Fatalf("启用 1.0.0 失败: %v", err)
	}
	// B@1，也启用：enable 新版本不会把旧的那一行按下去，两行都成了 active。
	pub.Version = "2.0.0"
	pub.Source = goodSource(key, "2.0.0")
	if _, err := s.Publish(ctx, pub); err != nil {
		t.Fatalf("发布 2.0.0 失败: %v", err)
	}
	if _, err := s.Enable(ctx, key); err != nil {
		t.Fatalf("启用 2.0.0 失败: %v", err)
	}

	// 重发**旧**的那一版（改了个错别字）：覆盖。
	pub.Version = "1.0.0"
	pub.Source = goodSource(key, "1.0.0") + "\n// 改了一个错别字\n"
	res, err := s.Publish(ctx, pub)
	if err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if !res.Overwritten {
		t.Error("同一个 (key, version) 的第二次发布该报成覆盖")
	}
	if res.Generation != 2 {
		t.Errorf("覆盖该取这个 key 的最大代次加一（2），实际 %d——只把这一行自己加一的话"+
			"它会与 2.0.0 并列", res.Generation)
	}

	// 答案一：Get 给的是刚覆盖的这一行。
	d, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("看详情失败: %v", err)
	}
	if d.Version != "1.0.0" || d.Generation != 2 {
		t.Errorf("Get 该给刚覆盖的这一行（1.0.0 / 2）：%+v", d.Row)
	}
	if !d.IsRunning() || !d.Latest {
		t.Errorf("刚覆盖的这一行既在跑，又是最新一次尝试：%+v", d.Row)
	}

	// 答案二：清单里恰好一行是「最新一次尝试」，也恰好一行是「在跑」，都是它。
	rows, err := s.List(ctx)
	if err != nil {
		t.Fatalf("列清单失败: %v", err)
	}
	var latest, running []Row
	generations := map[string]int64{}
	for _, r := range rows {
		if r.Key != key {
			continue
		}
		generations[r.Version] = r.Generation
		if r.Latest {
			latest = append(latest, r)
		}
		if r.IsRunning() {
			running = append(running, r)
		}
	}
	if len(latest) != 1 || latest[0].Version != "1.0.0" {
		t.Errorf("最新一次尝试该恰好是刚覆盖的那一行，实际 %d 行：%+v", len(latest), latest)
	}
	if len(running) != 1 || running[0].Version != "1.0.0" {
		t.Errorf("取中的该恰好是刚覆盖的那一行，实际 %d 行：%+v", len(running), running)
	}
	// 覆盖只动它自己那一行：另一行还是它原来的代次。
	if generations["2.0.0"] != 1 {
		t.Errorf("覆盖不该动另一行：2.0.0 的代次 = %d，想要 1", generations["2.0.0"])
	}

	// 答案三：加载器取中的就是它——真的装配一遍，装上的是刚发的那一代。
	reg, err := s.LoadRegistry(ctx, discardLogger(), allCaps())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	p, ok := reg.Get(key)
	if !ok {
		t.Fatal("这个 key 有两代 active，该有一代被装上")
	}
	if p.Meta.Version != "1.0.0" {
		t.Errorf("加载器该取中刚覆盖的那一代（1.0.0），实际装的是 %s", p.Meta.Version)
	}
}

// 判据：这个 key 上已经有别人持着发布锁时，Publish **等**，而不是照写。
//
// 上面那一条测不出两条发布真的重叠时的样子（能不能重叠取决于调度）。这里由用例
// 自己在一个事务里持住同一把 advisory lock，再看 Publish 会不会立刻完成：把上锁
// 那一句删掉之后，它会立刻写完并返回，这条用例必红。
//
// 它按 key 去拿锁，所以也钉住了「锁挂在哪个键上」：键换成别的东西（例如进程内的
// mutex），跨进程那条路就不再被覆盖，而这条用例会红。
func TestPublishWaitsOnKeyLock(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	key := uniqueKey("pub-lock")
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启持锁事务失败: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	// 这一句是照 Publish 上锁那一句抄的**字面量**（见 publishLockSQL），不是引用
	// 那个常量：常量自己改掉（换了前缀、换了哈希）而 Publish 与用例一起跟着走的话，
	// 「锁挂在哪个键上」就没人守着了。
	if _, err := holder.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('plugin:' || $1::text, 0))`, key); err != nil {
		t.Fatalf("用例自己拿锁失败: %v", err)
	}

	var (
		res    Result
		pubErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, pubErr = s.Publish(ctx, Publication{
			Key: key, Version: "1.0.0", Name: "测试插件",
			Source: goodSource(key, "1.0.0"), Meta: []byte(`{"key":"` + key + `"}`),
		})
	}()

	select {
	case <-done:
		t.Fatal("这个 key 已经被锁着时，Publish 不该完成：那把事务级 advisory lock 没生效")
	case <-time.After(200 * time.Millisecond):
	}

	// 放锁之后它接着跑完——是等待，不是放弃，也不是报错。
	if err := holder.Rollback(); err != nil {
		t.Fatalf("释放持锁事务失败: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("放锁之后 Publish 该接着跑完")
	}
	if pubErr != nil {
		t.Fatalf("发布失败: %v", pubErr)
	}
	if res.Generation != 0 {
		t.Errorf("首次发布的代次该是 0，实际 %d", res.Generation)
	}
	if rows := rowsFor(t, db, key); len(rows) != 1 {
		t.Errorf("该只留下 1 行，实际 %d 行", len(rows))
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
	regs := activeRegs(t, s)
	if !hasKey(regs, key) {
		t.Fatal("启用之后该参与注册")
	}

	if _, disableErr := s.Disable(ctx, key); disableErr != nil {
		t.Fatalf("停用失败: %v", disableErr)
	}
	regs = activeRegs(t, s)
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

// 判据：「在跑的是哪一代」「最新一次尝试是哪一代」「上一次加载装上了没有」是三个
// 问题，清单各答各的——前两个从 status 与代次算出来，第三个读那三列。
//
// 六种落在 key 上的状态各造一个，因为它们在**同一个 key** 上给出的答案不一样：
//
//   - 一代 active、没有加载记录：取中的是它，也是最新一次尝试，加载那一栏空着；
//   - 一代 active、上一次加载是 ok：同上，另外说得出装上了、什么时候装的；
//   - 两代 active：取中的只有代次最高的那一代。这一格是 status 答不出来的那个
//     ——enable 新版本时旧的那一行不会被按下去，两行都写着 active。旧的那一代
//     身上留着它当年被加载的结果，而读出来的必须是**取中那一代**的（空）；
//   - 一代 active、上一次加载被跳过：结果与错误原文都读得出来，而它的 status
//     仍是 active——跳过不改状态，下次启动还会再试；
//   - active + 手工写下的 failed：取中的是 active 的旧代，最新一次尝试是 failed
//     的那一代，它还带着发布期那一列；
//   - 只有 pending：一代都没在跑（Running 是 nil，不是「代次 0」）。
func TestListReportsRunningLatestAttemptAndLoadResult(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	single := uniqueKey("run-one")
	loaded := uniqueKey("run-loaded")
	twoActive := uniqueKey("run-two")
	skipped := uniqueKey("run-skipped")
	failed := uniqueKey("run-failed")
	pending := uniqueKey("run-pending")

	insertPlugin(t, db, single, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, loaded, "1.0.0", StatusActive, 0)
	markLoad(t, db, loaded, "1.0.0", LoadResultOK, "")
	insertPlugin(t, db, twoActive, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, twoActive, "2.0.0", StatusActive, 1)
	// 旧的那一代身上留一条**它当年**被加载的结果：读清单时取中的已经不是它了。
	markLoad(t, db, twoActive, "1.0.0", LoadResultFailed, "上一次装配没提供 kv")
	insertPlugin(t, db, skipped, "1.0.0", StatusActive, 0)
	markLoad(t, db, skipped, "1.0.0", LoadResultFailed, "源码编译不过：Unexpected token }")
	insertPlugin(t, db, failed, "1.0.0", StatusActive, 0)
	insertFailedPlugin(t, db, failed, "2.0.0", 1, "源码编译不过：Unexpected token }")
	insertPlugin(t, db, pending, "1.0.0", StatusPending, 0)

	rows, err := New(db).List(ctx)
	if err != nil {
		t.Fatalf("列清单失败: %v", err)
	}
	got := map[string]Row{}
	for _, row := range rows {
		got[row.Key+" "+row.Version] = row
	}

	if r := got[single+" 1.0.0"]; !r.IsRunning() || !r.Latest {
		t.Errorf("只有一代 active 时，它既在跑又是最新一次尝试：%+v", r)
	}
	if r := got[single+" 1.0.0"]; r.RunningLoad != nil {
		t.Errorf("从没被加载过的一代不该有加载结果：%+v", r.RunningLoad)
	}

	// 装上了：结果不是「没装成」，时刻也读得出来（没有时刻就分不出是哪一次启动）。
	if r := got[loaded+" 1.0.0"]; r.RunningLoad == nil || r.RunningLoad.Failed() {
		t.Errorf("上一次加载装上了：%+v", r.RunningLoad)
	} else if r.RunningLoad.At.IsZero() {
		t.Error("加载结果该带着那一刻的时刻")
	}

	// 两代都 active：在跑的是代次最高的那一行，另一行只是 active。
	if r := got[twoActive+" 2.0.0"]; !r.IsRunning() || !r.Latest {
		t.Errorf("两代都 active 时，代次最高的那一行在跑：%+v", r)
	}
	if r := got[twoActive+" 1.0.0"]; r.IsRunning() {
		t.Errorf("两代都 active 时只能有一行在跑，1.0.0 不该是它：%+v", r)
	}
	// 加载那几列按**取中的那一代**给，不是按这一行自己的：旧代身上那条记录
	// 说的是「它当年被跳过」，而这一刻取中的是新一代、还没有记录——两行都该是空。
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if r := got[twoActive+" "+v]; r.RunningLoad != nil {
			t.Errorf("取中的是没被加载过的新一代，%s 这一行不该报出旧代的加载结果：%+v",
				v, r.RunningLoad)
		}
	}

	// 取中的那一代上一次被跳过：结果与原文都读得出来，代次与版本指着它。
	r := got[skipped+" 1.0.0"]
	if !r.IsRunning() || r.RunningLoad == nil || !r.RunningLoad.Failed() {
		t.Fatalf("取中的那一代上一次加载没装成：%+v", r)
	}
	if !strings.Contains(r.RunningLoad.Error, "Unexpected token") {
		t.Errorf("错误原文没有读出来：%q", r.RunningLoad.Error)
	}
	if r.RunningLoad.At.IsZero() {
		t.Error("加载结果该带着那一刻的时刻")
	}
	// 跳过不改状态：它还是参与注册的那一代，下次启动照样会再试。
	if r.Status != StatusActive {
		t.Errorf("跳过一次之后 status 仍是 %s，实际 %s", StatusActive, r.Status)
	}

	// active + failed：两个问题各有各的答案，且发布期那一列读得出来。
	old := got[failed+" 1.0.0"]
	if !old.IsRunning() || old.Latest {
		t.Errorf("active 的旧代该在跑、且不是最新一次尝试：%+v", old)
	}
	newest := got[failed+" 2.0.0"]
	if newest.IsRunning() || !newest.Latest {
		t.Errorf("failed 的那一代是最新一次尝试，但不该在跑：%+v", newest)
	}
	if !strings.Contains(newest.CompileError, "Unexpected token") {
		t.Errorf("compile_error 没有读出来：%q", newest.CompileError)
	}
	// 「在跑的那一代」按 key 给，所以同一个 key 的每一行带的是同一个值。
	want := &Generation{Version: "1.0.0", Number: 0}
	if old.Running == nil || *old.Running != *want {
		t.Errorf("在跑的那一代 = %+v，想要 %+v", old.Running, want)
	}
	if newest.Running == nil || *newest.Running != *want {
		t.Errorf("同一个 key 的每一行该带着同一个在跑代次：%+v", newest.Running)
	}

	if r := got[pending+" 1.0.0"]; r.Running != nil || r.IsRunning() || r.RunningLoad != nil {
		t.Errorf("没有一代 active 时不该报出在跑的一代，也不该有加载结果（代次 0 是合法值，不能拿它当「没有」）：%+v", r)
	}
}

// 判据：同一个 key 里两行撞在同一个代次上时，「取中的是哪一行」三处指的是同一行，
// 而写回只落在加载器读的那一行上。
//
// 代次撞车来自**早先写下的行**（见 Publish 与 pickedOrder）：那时覆盖发布把代次
// 原地加一，A(1.0.0) 发布后代次是 0，B(2.0.0) 发布时拿 1，再把 A 重发一次，A 也
// 变成 1。于是同一个 key 里两行同代次，而只按代次比大小的写法在三处各错一次：
//
//   - 读表那一处（activeRows）与管理面那一处（runningJoin）各写一份排序，就会各选
//     一行——写回落在加载器读的那一行上，而清单报的是另一行，库里看不出异常；
//   - IsRunning 只比代次时两行都说自己在跑，而加载器只读一行；
//   - 写回按 (key, generation) 定位时一条 UPDATE 改掉两行，另一行凭空多出一条记录。
//
// 夹具让**取中的那一行**（版本名大的那一行，见 pickedOrder）编译不过：它被跳过，
// 另一行是好的。于是三件事同时可判：只有它被写回、另一行三列仍是 NULL，而它的
// status 一个字没动。
//
// # 这一条为什么得在真库上跑
//
// 本机没有库时整个包 skip，而「跳过」与「通过」在输出里长得一模一样（见
// LESSONS.md 第 16 条）：这一条第一次真的连上库时就是红的，红在断言里那句手抄的
// 错误原文——引擎报的是「Unexpected end of input」，而当时断言等的是
// 「Unexpected token」。所以现在原文从同一个入口取（见下面那一句），措辞不写死。
func TestLoadRegistrySameGenerationWritesBackOneRow(t *testing.T) {
	db := testDB(t)
	key := uniqueKey("tie")
	insertPluginRaw(t, db, key, "2.0.0", StatusActive, 1, goodMeta(key, "2.0.0"), brokenSource)
	insertPlugin(t, db, key, "1.0.0", StatusActive, 1)

	reg, err := New(db).LoadRegistry(context.Background(), discardLogger(), allCaps())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	// 取中的那一代被跳过时不会退回另一代（见 PublishPartial）：这个 key 什么都不在跑。
	if _, ok := reg.Get(key); ok {
		t.Error("取中的那一代被跳过之后，这个 key 不该有插件在跑")
	}

	byVersion := map[string]pluginRow{}
	for _, r := range rowsFor(t, db, key) {
		byVersion[r.version] = r
	}
	if len(byVersion) != 2 {
		t.Fatalf("这个 key 该有两行，实际 %d 行", len(byVersion))
	}
	picked := byVersion["2.0.0"]
	if !picked.lastLoadResult.Valid || picked.lastLoadResult.String != LoadResultFailed {
		t.Errorf("被跳过的那一行该写回 failed：%+v", picked.lastLoadResult)
	}
	// 原文从**同一个入口**取：拿同一份源码走一遍 CompileWith，它给出的那一句就是
	// 加载器交给写回的那一句。这一栏钉的是「写回的就是编译给出的那一句」，不是那个
	// 版本的引擎把语法错说成什么。
	_, compileErr := plugin.CompileWith(plugin.Registration{Key: key, Source: brokenSource}, allCaps())
	if compileErr == nil {
		t.Fatal("夹具坏了：这份源码该编译不过")
	}
	if !picked.lastLoadError.Valid || picked.lastLoadError.String != compileErr.Error() {
		t.Errorf("写回的原文 = %+v，想要编译给出的那一句 %q", picked.lastLoadError, compileErr.Error())
	}
	if !picked.lastLoadAt.Valid {
		t.Error("写回该带上那一刻的时刻（没有时刻就分不出是哪一次启动写下的）")
	}
	if picked.status != StatusActive {
		t.Errorf("跳过不改状态：status 仍是 %s，实际 %s", StatusActive, picked.status)
	}
	other := byVersion["1.0.0"]
	if other.lastLoadResult.Valid || other.lastLoadError.Valid || other.lastLoadAt.Valid {
		t.Errorf("写回只该落在加载器读的那一行上，另一行三列都该是 NULL：%+v", other)
	}

	// 管理面那一处：同一个 key 里恰好一行是「取中的那一行」，而它是加载器读的那一行。
	rows, err := New(db).List(context.Background())
	if err != nil {
		t.Fatalf("列清单失败: %v", err)
	}
	var running []Row
	for _, r := range rows {
		if r.Key == key && r.IsRunning() {
			running = append(running, r)
		}
	}
	if len(running) != 1 || running[0].Version != "2.0.0" || running[0].Generation != 1 {
		t.Fatalf("同一个 key 里该恰好一行是取中的那一行（2.0.0 / 1），实际 %d 行：%+v",
			len(running), running)
	}
	if running[0].RunningLoad == nil || !running[0].RunningLoad.Failed() {
		t.Errorf("取中的那一行该带着它那一次加载的结果：%+v", running[0].RunningLoad)
	}

	// 代次并列时 latest 两行都为真：这一栏答的是「这一行的代次是不是最高的」，
	// 而并列的两行代次都是 1。它不替「取中」定唯一一行——那一行的判据在
	// pickedOrder 那一侧（见 Row.Latest），上面那条断言问的就是它。
	var tie []Row
	for _, r := range rows {
		if r.Key == key {
			tie = append(tie, r)
		}
	}
	if len(tie) != 2 || !tie[0].Latest || !tie[1].Latest {
		t.Errorf("代次并列时两行的 latest 都该为真，实际：%+v", tie)
	}
}

// 判据：两个常量与迁移里那条 CHECK 说的是同一套字面量。
//
// 这两个常量是列上那个约束在 Go 这一侧的对应物（见迁移 00034）：库那一侧写不进第三
// 个值，而读的那一侧靠它们比对。把两边的值互换（ok 与 failed 对调），别的用例全都
// 照过——它们只用常量互相对照，一处也没有与迁移对过话，而库里的含义整个反了：装上
// 的记录会被读成「没装成」。
//
// 期望串按常量的**顺序**拼出来，再从内嵌的那份迁移里读一遍（migrate.FS，跑起来的
// 就是那一份）：改 CHECK 而没改常量、改常量而没改 CHECK、或者把两个常量对调，都在
// 这里挂。
func TestLoadResultConstantsMatchMigrationCheck(t *testing.T) {
	b, err := fs.ReadFile(migrate.FS(), "00034_plugin_last_load_result.sql")
	if err != nil {
		t.Fatalf("读迁移文件失败: %v", err)
	}
	want := fmt.Sprintf("CHECK (last_load_result IN ('%s', '%s'))", LoadResultOK, LoadResultFailed)
	if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), want) {
		t.Errorf("迁移里那条 CHECK 与常量对不上，想要 %q", want)
	}
}

// Get 给的是最新一次尝试那一行，另外说出取中的是哪一代、它上一次装上没有。
//
// 与 List 分开：这一条走的是另一段 SQL（那一份没有逐行的那一栏，只有一行），
// 而三个问题的答案都落在同一个 Detail 上。
//
// 加载那一栏最值得在这里钉住：这一页打的是**最新发布**的那一行，而加载器写的是
// **取中**的那一代——发布了没 enable 时两者不是同一行，那一栏得从取中的那一代读。
func TestGetReportsRunningAndLoadResultBesideTheNewestOne(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	key := uniqueKey("get-run")
	insertPlugin(t, db, key, "1.0.0", StatusActive, 0)
	insertFailedPlugin(t, db, key, "2.0.0", 1, "源码编译不过：Unexpected token }")

	d, err := New(db).Get(ctx, key)
	if err != nil {
		t.Fatalf("看详情失败: %v", err)
	}
	if d.Version != "2.0.0" || d.Status != StatusFailed || !d.Latest {
		t.Errorf("详情该给最新的那一代（2.0.0 / failed / latest）：%+v", d.Row)
	}
	if !strings.Contains(d.CompileError, "Unexpected token") {
		t.Errorf("compile_error 没有读出来：%q", d.CompileError)
	}
	if d.IsRunning() {
		t.Error("最新一次尝试没有上线，它不该是「取中的那一代」")
	}
	if want := (&Generation{Version: "1.0.0", Number: 0}); d.Running == nil || *d.Running != *want {
		t.Errorf("取中的那一代 = %+v，想要 %+v", d.Running, want)
	}
	if d.RunningLoad != nil {
		t.Errorf("这两行都没有加载记录：%+v", d.RunningLoad)
	}
	if d.Versions != 2 {
		t.Errorf("versions = %d，想要 2", d.Versions)
	}

	// 取中的那一代被跳过、而详情打的是另一行：加载结果要从取中的那一代读出来。
	skipped := uniqueKey("get-skipped")
	insertPlugin(t, db, skipped, "1.0.0", StatusActive, 0)
	insertPlugin(t, db, skipped, "2.0.0", StatusPending, 1)
	markLoad(t, db, skipped, "1.0.0", LoadResultFailed, "源码编译不过：Unexpected token }")

	d, err = New(db).Get(ctx, skipped)
	if err != nil {
		t.Fatalf("看详情失败: %v", err)
	}
	if d.Version != "2.0.0" || d.IsRunning() {
		t.Errorf("详情给的是还没 enable 的那一版：%+v", d.Row)
	}
	if want := (&Generation{Version: "1.0.0", Number: 0}); d.Running == nil || *d.Running != *want {
		t.Errorf("取中的那一代 = %+v，想要 %+v", d.Running, want)
	}
	if d.RunningLoad == nil || !d.RunningLoad.Failed() {
		t.Fatalf("取中的那一代上一次没装成，这一栏该读得出来：%+v", d.RunningLoad)
	}
	if !strings.Contains(d.RunningLoad.Error, "Unexpected token") {
		t.Errorf("错误原文没有读出来：%q", d.RunningLoad.Error)
	}
	if d.RunningLoad.At.IsZero() {
		t.Error("加载结果该带着那一刻的时刻")
	}

	// 一代都没跑时是 nil：这一格与「代次 0」分得开（见 List 那条用例）。
	onlyPending := uniqueKey("get-pending")
	insertPlugin(t, db, onlyPending, "1.0.0", StatusPending, 0)
	d, err = New(db).Get(ctx, onlyPending)
	if err != nil {
		t.Fatalf("看详情失败: %v", err)
	}
	if d.Running != nil || d.RunningLoad != nil {
		t.Errorf("pending 的插件既没有取中的一代，也没有加载结果：%+v / %+v", d.Running, d.RunningLoad)
	}
}

// ── 助手 ──────────────────────────────────────────────────────────────

type pluginRow struct {
	version string
	status  string
	source  string
	// 加载期那三列（见迁移 00034），加上发布期那一列：写回那一侧要断言的是
	// 「哪几列动了、哪几列没动」，所以都读回来。没被加载过时三列都是 NULL。
	lastLoadResult sql.NullString
	lastLoadError  sql.NullString
	lastLoadAt     sql.NullTime
	compileError   sql.NullString
}

// 排序键带上 version：代次会撞车（早先写下的行，见 pickedOrder），只按代次排的话
// 两行的先后由库决定，用例里就会随机。
func rowsFor(t *testing.T, db *sql.DB, key string) []pluginRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT version, status, source,
		        last_load_result, last_load_error, last_load_at, compile_error
		   FROM plugins WHERE key = $1 ORDER BY generation, version`, key)
	if err != nil {
		t.Fatalf("查插件行失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []pluginRow
	for rows.Next() {
		var r pluginRow
		if err := rows.Scan(&r.version, &r.status, &r.source,
			&r.lastLoadResult, &r.lastLoadError, &r.lastLoadAt, &r.compileError); err != nil {
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
