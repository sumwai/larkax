package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 本文件是 usage 命令的用例。**根帮助页那一组不在里**：它们在 usage_test.go
// （那个文件名说的是「帮助文本里的 usage」）。

// usageFixtureTime 是这一组用例用的两个时间窗。
//
// 两个窗落在**相邻的两个月**上：usage_events 按月分区，跨月那道边界正是最容易
// 写错、又在库里看不出来的那一格（见 TestUsageEventsWindowCrossesMonthBoundary）。
var (
	usageWindowOctFrom = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	usageWindowOctTo   = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	usageWindowNovFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	usageWindowNovTo   = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
)

// insertUsageEvent 直接写一行用量事件。
//
// 走 SQL 而不是走结算路径：这一组用例要的是「库里已经有这几行」这个前提，而结算
// 那条路上的每一步（预留、求值、落库）都有自己的用例。created_at 显式给——它既是
// 分区键也是这条命令筛窗口用的那一列，用例要把它钉在指定的月份里。
func insertUsageEvent(t *testing.T, db *sql.DB, requestID string,
	userID int64, model string, createdAt time.Time, cost string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO usage_events
			(request_id, user_id, canonical_model, prompt_tokens, completion_tokens,
			 billing_path, cost, asset, priced_at, created_at)
		VALUES ($1, $2, $3, 100, 50, 'measured', $4::numeric, 'CNY', $5, $5)`,
		requestID, userID, model, cost, createdAt)
	if err != nil {
		t.Fatalf("写用量事件失败: %v", err)
	}
}

// usageEventsModels 跑一次 usage events，返回打出来的那些模型名。
//
// 判据取**模型名**这一栏而不是行数：给每一行起一个各不相同的模型名之后，
// 「按谁筛的、筛掉了谁」在输出里逐行对得上；只数行数的话，把两条记录筛反了
// 也是同样的行数。
func usageEventsModels(t *testing.T, args ...string) []string {
	t.Helper()

	r := runOnce(t, runUsage, append([]string{"events"}, args...))
	if r.err != nil {
		t.Fatalf("usage events %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return tableModels(t, r.stdout)
}

// usageModelField 是模型那一栏在数据行里的**空白字段**下标，不是栏号。
//
// 表格是按显示宽度补空格的，所以「第几栏」与「第几个空白字段」不是一回事：
// 「时刻」那一栏自带一个空格（`2026-10-20 10:00`），按空白切开占两项。栏的布局
// 见 usage.go 的 printUsageEvents（10 栏，模型是第 4 栏），于是模型落在下标 4 上：
// 0 与 1 是时刻的两项，2 是用户 ID，3 是用户名。
//
// 这个下标有个前提：**它左边每一栏都非空**。空白栏会被 strings.Fields 整个吃掉，
// 右边的下标于是整体左移（用户名那一栏空着时，下标 4 会取到输入 token 数）。
// 用例里的用户都由 createTestUser 造出来、一定带用户名，所以今天触发不到；
// tableModels 用 usageRowFields 兜着这一条——认行不按项数，但项数对不上就报错，
// 免得将来某个夹具把某一栏造空之后，这里安静地取错值。
const usageModelField = 4

// usageRowFields 是 usage events 数据行按空白切开的项数（10 栏，其中「时刻」占 2 项）。
//
// 它不参与认行（那是 usageRowClock 的事），只是一条一致性判据：项数对不上说明
// usageModelField 那个下标的前提不成立了，与其取错值，不如当场报出来。
const usageRowFields = 11

// usageRowClock 认出表格里的**数据行**：它们都以「时刻」那一栏开头。
//
// 表头行与末尾那句截断说明都不是这个形状，于是这一条把两者一并挡在外面。按项数
// 认数据行是不行的：表头里的「用户 ID」也含空格，切下来与数据行同样多（各 11 项），
// 而截断那句话切出来是 5 项——按下标取它的第 5 项，会取出一段说明文字当模型名。
var usageRowClock = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2} `)

// tableModels 从一张 usage events 表格里抠出模型名那一栏。
//
// 这一组用例造出来的模型名都是 uniqueName 的唯一值，所以拿到的都是能一眼对上的
// 名字，不必解析列宽。
func tableModels(t *testing.T, out string) []string {
	t.Helper()

	var models []string
	for _, line := range strings.Split(out, "\n") {
		if !usageRowClock.MatchString(line) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != usageRowFields {
			t.Fatalf("usage events 的数据行该切出 %d 项（见 usageRowFields），实际 %d 项：%q",
				usageRowFields, len(fields), line)
		}
		models = append(models, fields[usageModelField])
	}
	return models
}

// countUsageRows 数一张用量清单里打了几行数据。
//
// 与 tableModels 同一条：数据行按行首那个时刻认（表头与截断说明都不是这个形状）。
// events 与 requests 两张表的第一栏都是时刻，所以这一个助手服务两条命令。
func countUsageRows(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if usageRowClock.MatchString(line) {
			n++
		}
	}
	return n
}

// TestUsageEventsFiltersByUserAndWindow 钉住筛选真的生效。
//
// 同一张表里放两个用户、两个时间窗共四条记录，然后分别按用户筛、按窗口筛：
// 每次都只该捞回该捞的那几条。「有输出就算过」在这里没有意义——四条记录全捞回来
// 也是一次有输出的成功。
func TestUsageEventsFiltersByUserAndWindow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	alice := createTestUser(t)
	bob := createTestUser(t)
	aliceOct := uniqueName("model-alice-oct")
	aliceNov := uniqueName("model-alice-nov")
	bobOct := uniqueName("model-bob-oct")

	// 窗口内的两条各在不同月份，第三条是 bob 的同月记录，第四条在下一个窗口里。
	insertUsageEvent(t, db, uniqueName("req"), alice, aliceOct,
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "0.01000000")
	insertUsageEvent(t, db, uniqueName("req"), alice, aliceNov,
		time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC), "0.02000000")
	insertUsageEvent(t, db, uniqueName("req"), bob, bobOct,
		time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), "0.03000000")

	aliceRef := itoa64(alice)

	// ① 按用户 + 窗口筛：只有 alice 在十月的那一条。
	// 用户名与 ID 两种写法都要认（与其余命令同一条约定），所以各跑一次。
	for _, ref := range []string{aliceRef, usernameOf(t, db, alice)} {
		got := usageEventsModels(t, "--user", ref,
			"--from", usageWindowOctFrom.Format(time.RFC3339),
			"--to", usageWindowOctTo.Format(time.RFC3339))
		if len(got) != 1 || got[0] != aliceOct {
			t.Errorf("--user %s 十月窗口筛出来 %v，只该有 %s", ref, got, aliceOct)
		}
	}

	// ② 换用户：同一窗口里只剩下 bob 那一条。
	got := usageEventsModels(t, "--user", itoa64(bob),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339))
	if len(got) != 1 || got[0] != bobOct {
		t.Errorf("bob 十月窗口筛出来 %v，只该有 %s", got, bobOct)
	}

	// ③ 换窗口（用户不变）：同一个用户的两条按月份分开。
	got = usageEventsModels(t, "--user", aliceRef,
		"--from", usageWindowNovFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if len(got) != 1 || got[0] != aliceNov {
		t.Errorf("alice 十一月窗口筛出来 %v，只该有 %s", got, aliceNov)
	}
}

// TestUsageEventsWindowCrossesMonthBoundary 钉住跨月窗口一条都不漏。
//
// usage_events 按月分区，而窗口谓词写的正是分区键那一列（created_at）。两个月的
// 交界上：窗口跨月时两条都要在，窗口只覆盖后一个月时前一条不能进来。这一格错了
// 在库里看不出来——少一行与「那个月本来就没用量」在输出上长得一样。
func TestUsageEventsWindowCrossesMonthBoundary(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	lastOfOct := uniqueName("model-oct-last")
	firstOfNov := uniqueName("model-nov-first")

	// 十月最后一小时与十一月第一小时，落在两个相邻分区上。
	insertUsageEvent(t, db, uniqueName("req"), userID, lastOfOct,
		time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), "0.01000000")
	insertUsageEvent(t, db, uniqueName("req"), userID, firstOfNov,
		time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC), "0.01000000")

	got := usageEventsModels(t, "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if !contains(got, lastOfOct) || !contains(got, firstOfNov) {
		t.Errorf("跨月窗口（十月一日到十二月一日）筛出来 %v，交界两边的 %s 与 %s 都要在",
			got, lastOfOct, firstOfNov)
	}

	// 窗口起点压在十一月一日零点：十月那一条（含 10-31 23:00）不再进来，
	// 十一月那一条照旧——左闭右开的边界在**分区边界**上也要成立。
	got = usageEventsModels(t, "--user", itoa64(userID),
		"--from", usageWindowNovFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if contains(got, lastOfOct) {
		t.Errorf("十一月窗口里出现了十月那一条（%s）：窗口起点是左闭的，十月的不该在", lastOfOct)
	}
	if !contains(got, firstOfNov) {
		t.Errorf("十一月窗口里没有 %s：跨月之后的那一条被漏掉了", firstOfNov)
	}
}

// TestUsageEventsListsNewestFirst 钉住清单的顺序：新的在前。
//
// 两把钥匙各钉一次。**created_at 相同的那两行**才是要紧的那一格：只按 created_at
// 排的话，它们的先后由数据库决定，两次读出来可以不同，「同一段窗口读两次、清单对
// 不上」于是变成一件碰运气的事——排序因此补了 id（见 store 的 UsageEvents）。所以
// 这里造两行同一时刻的、一行更晚的，判据是输出里那三行的先后与库里的 id 对得上。
func TestUsageEventsListsNewestFirst(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	tie := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	// 前两行同一时刻，第三行晚一小时。id 从库里读回来，不按插入顺序推。
	models := make([]string, 0, 3)
	idByModel := map[string]int64{}
	times := []time.Time{tie, tie, tie.Add(time.Hour)}
	for _, at := range times {
		requestID, model := uniqueName("req"), uniqueName("model")
		insertUsageEvent(t, db, requestID, userID, model, at, "0.01000000")
		var id int64
		if err := db.QueryRowContext(context.Background(),
			`SELECT id FROM usage_events WHERE request_id = $1`, requestID).Scan(&id); err != nil {
			t.Fatalf("读用量事件的 id 失败: %v", err)
		}
		models = append(models, model)
		idByModel[model] = id
	}
	if idByModel[models[1]] <= idByModel[models[0]] {
		t.Fatalf("同一时刻的两行里，后写的 id 反而更小（%d 与 %d）：这一条的前提不成立",
			idByModel[models[0]], idByModel[models[1]])
	}

	got := usageEventsModels(t, "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339))
	if len(got) != 3 {
		t.Fatalf("窗口里应当正好三行，得到 %v", got)
	}
	// ① 晚的那一行在最前。
	if got[0] != models[2] {
		t.Errorf("最前的一行是 %s，想要最晚落库的 %s：清单新的在前", got[0], models[2])
	}
	// ② 同一时刻的相邻两行按 id 从大到小。
	ids := make([]int64, 0, 2)
	for _, model := range got[1:] {
		id, ok := idByModel[model]
		if !ok {
			t.Fatalf("清单里出现了这一条没造过的行（%s）：%v", model, got)
		}
		ids = append(ids, id)
	}
	if ids[0] <= ids[1] {
		t.Errorf("同一时刻的相邻两行是 %s（id=%d）与 %s（id=%d）：id 大的该在前",
			got[1], ids[0], got[2], ids[1])
	}
}

// insertBillingRequest 直接写一行计费请求。
//
// 与用量事件那条同一条理由：这一组要的是「库里已经有这几行」这个前提。
func insertBillingRequest(t *testing.T, db *sql.DB, requestID string,
	userID int64, model string, startedAt time.Time, status string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO billing_requests
			(request_id, user_id, canonical_model, protocol, streamed,
			 reserved_amount, reserved_asset, status, started_at)
		VALUES ($1, $2, $3, 'openai', false, 0.02, 'CNY', $4, $5)`,
		requestID, userID, model, status, startedAt)
	if err != nil {
		t.Fatalf("写计费请求失败: %v", err)
	}
}

// TestUsageRequestsFiltersByUserAndWindow 与用量事件那条同一个判据：筛真的生效。
func TestUsageRequestsFiltersByUserAndWindow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	alice := createTestUser(t)
	bob := createTestUser(t)
	aliceReq := uniqueName("req-alice")
	bobReq := uniqueName("req-bob")

	insertBillingRequest(t, db, aliceReq, alice, uniqueName("model"),
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "pending")
	insertBillingRequest(t, db, bobReq, bob, uniqueName("model"),
		time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC), "pending")

	out := runUsageRequestsOut(t, "--user", itoa64(alice),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339))
	if !strings.Contains(out, aliceReq) {
		t.Errorf("按 alice 筛出来的清单里没有她自己的请求 %s：\n%s", aliceReq, out)
	}
	if strings.Contains(out, bobReq) {
		t.Errorf("按 alice 筛出来的清单里出现了 bob 的请求 %s：\n%s", bobReq, out)
	}

	// 换窗口：十一月那一段里两条都不在。
	out = runUsageRequestsOut(t, "--user", itoa64(alice),
		"--from", usageWindowNovFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if strings.Contains(out, aliceReq) {
		t.Errorf("十一月窗口里出现了十月的请求 %s：\n%s", aliceReq, out)
	}
}

// runUsageRequestsOut 跑一次 usage requests 并把 stdout 交回来。
func runUsageRequestsOut(t *testing.T, args ...string) string {
	t.Helper()

	r := runOnce(t, runUsage, append([]string{"requests"}, args...))
	if r.err != nil {
		t.Fatalf("usage requests %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return r.stdout
}

// insertBillingRequestWithRule 造一条计价规则，再写一行指向它的计费请求，返回规则 id。
//
// 规则按**只有本用例知道**的模型名建（与 internal/ledger/store 的
// TestReservationRecordsPricingRule 同一条理由）：pricing_rules 是全局表，而测试库由
// 同包与别的包的用例共用，一条不带 model_patterns 的规则会匹配**任何**模型，于是
// 「一条规则都匹配不上」这个状态在整库范围内消失，受影响的正是那条规则留在库里的
// 那个窗口。
func insertBillingRequestWithRule(t *testing.T, db *sql.DB, requestID string,
	userID int64, model string, startedAt time.Time, status string) int64 {
	t.Helper()

	var ruleID int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules (scope, name, model_patterns, expr, priority)
		VALUES ('platform', 'placeholder-rule', to_jsonb($1::text[]),
		        '{"prompt": 0.000001}', 0)
		RETURNING id`, []string{model}).Scan(&ruleID); err != nil {
		t.Fatalf("造计价规则失败: %v", err)
	}

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO billing_requests
			(request_id, user_id, canonical_model, protocol, streamed,
			 reserved_amount, reserved_asset, status, started_at, pricing_rule_id)
		VALUES ($1, $2, $3, 'openai', false, 0.02, 'CNY', $4, $5, $6)`,
		requestID, userID, model, status, startedAt, ruleID); err != nil {
		t.Fatalf("写计费请求失败: %v", err)
	}

	// 清理的顺序不能反：billing_requests.pricing_rule_id 指向这条规则且没有 ON DELETE，
	// 先删规则会被外键挡下（实测报 billing_requests_pricing_rule_id_fkey）。清不掉要
	// 报出来——被忽略的失败与清理成功在输出里长得一样。
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM billing_requests WHERE request_id = $1`, requestID); err != nil {
			t.Errorf("清理账单行失败: %v", err)
		}
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM pricing_rules WHERE id = $1`, ruleID); err != nil {
			t.Errorf("清理计价规则失败: %v", err)
		}
	})
	return ruleID
}

// usageTableLastCell 在一张清单表格里找到含 needle 的那一行，返回它**最后一栏**。
//
// 取末项而不按下标取：栏是按显示宽度补空格的，栏里出现空格（用户名、模型）就会让
// 下标整体错位（见 LESSONS 里「清单表用例按空白项数解析」那一条）。末项在
// usage requests 上有确定的含义——规则是表格的末栏，而它前面每一栏都非空
// （没值时打 `-`，不打空白）。
func usageTableLastCell(t *testing.T, out, needle string) string {
	t.Helper()

	fields := usageRequestRowFields(t, out, needle)
	return fields[len(fields)-1]
}

// ruleCellFromDoc 按 docs/cli-reference.md 里 `usage` 那一节写的，把机器可读档的
// pricing_rule_id 翻成表格里该打的样子：没值时表格打 `-`，有值时打那个 id。
//
// 照抄一遍那条规则，是为了让「表格与 json 是同一套」变成一条能挂的断言：两边分开
// 算、再比一次，改了任一边而另一边没跟上都会红。规则本身写在文档里，这里不另立一份
// 口径——它要是变了，这条用例与文档一起改。
func ruleCellFromDoc(id *int64) string {
	if id == nil {
		return "-"
	}
	return itoa64(*id)
}

// TestUsageRequestsShowsPricingRuleID 让账单答得出「这一单是按哪条规则算的」。
//
// 库里的 pricing_rule_id 写得到、却一处都不读，这一条就是它的读者：落了规则的行，
// 表格那一栏与机器可读档那一个字段都等于库里的规则 id；没落到任何规则的行（库里是
// NULL），表格打 `-`、机器可读档是 null——0 不是一个合法的规则 id，用它冒充「没有」
// 会被读成「一条编号为 0 的规则」。
func TestUsageRequestsShowsPricingRuleID(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	window := []string{"--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)}
	startedAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	withRule := uniqueName("req-rule")
	withoutRule := uniqueName("req-none")
	ruleID := insertBillingRequestWithRule(t, db, withRule, userID,
		uniqueName("model"), startedAt, "pending")
	insertBillingRequest(t, db, withoutRule, userID, uniqueName("model"),
		startedAt.Add(time.Minute), "pending")

	// ── 表格那一档 ──
	out := runUsageRequestsOut(t, window...)
	if got := usageTableLastCell(t, out, withRule); got != itoa64(ruleID) {
		t.Errorf("%s 那一行的规则栏 = %q，想要库里的规则 id %d：\n%s",
			withRule, got, ruleID, out)
	}
	if got := usageTableLastCell(t, out, withoutRule); got != "-" {
		t.Errorf("%s 那一行没落规则，规则栏该打 `-`，得到 %q：\n%s", withoutRule, got, out)
	}

	// ── 机器可读那一档 ──
	//
	// 夹具都在切到 json 之前建好：写类的命令在那一档会被拒（见 format.go 的
	// jsonUnsupported）。这一条只用读类命令，但这条纪律照样守着。
	withFormat(t, formatJSON)

	r := runOnce(t, runUsage, append([]string{"requests"}, window...))
	if r.err != nil {
		t.Fatalf("usage requests -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var doc struct {
		Requests []struct {
			RequestID     string `json:"request_id"`
			PricingRuleID *int64 `json:"pricing_rule_id"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if len(doc.Requests) != 2 {
		t.Fatalf("json 里 %d 行，想要 2 行：\n%s", len(doc.Requests), r.stdout)
	}
	got := map[string]*int64{}
	for _, req := range doc.Requests {
		got[req.RequestID] = req.PricingRuleID
	}
	if id, ok := got[withRule]; !ok || id == nil || *id != ruleID {
		t.Errorf("%s 的 pricing_rule_id = %s，想要 %d：\n%s",
			withRule, showPtr(id), ruleID, r.stdout)
	}
	if id, ok := got[withoutRule]; !ok || id != nil {
		t.Errorf("%s 没落到任何规则上，pricing_rule_id 该是 null，得到 %s：\n%s",
			withoutRule, showPtr(id), r.stdout)
	}
}

// TestUsageRequestsRuleColumnAgreesAcrossFormats 比一遍表格那一栏与 json 那一个字段
// 是不是同一套。
//
// 两档各跑一次，再按文档里那条推导式从 json 算一遍表格该打什么，逐行比。两边分开算，
// 改了任一边而另一边没跟上都会红——这是「两个档位由同一份数据推出来」在测试里的落点
// （照 plugin_test.go 的「四处同一套」那一条的形状）。
func TestUsageRequestsRuleColumnAgreesAcrossFormats(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	window := []string{"--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)}
	startedAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	withRule := uniqueName("req-rule")
	withoutRule := uniqueName("req-none")
	insertBillingRequestWithRule(t, db, withRule, userID,
		uniqueName("model"), startedAt, "pending")
	insertBillingRequest(t, db, withoutRule, userID, uniqueName("model"),
		startedAt.Add(time.Minute), "pending")

	table := runUsageRequestsOut(t, window...)

	withFormat(t, formatJSON)
	r := runOnce(t, runUsage, append([]string{"requests"}, window...))
	if r.err != nil {
		t.Fatalf("usage requests -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}

	var doc struct {
		Requests []struct {
			RequestID     string `json:"request_id"`
			PricingRuleID *int64 `json:"pricing_rule_id"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	// 先数一遍行数：两行都读不回来时下面的循环一次都不进，一条断言都没跑过也算通过
	// （见 LESSONS.md 第 30 条）。
	if len(doc.Requests) != 2 {
		t.Fatalf("json 里 %d 行，想要 2 行：\n%s", len(doc.Requests), r.stdout)
	}

	for _, req := range doc.Requests {
		want := ruleCellFromDoc(req.PricingRuleID)
		if got := usageTableLastCell(t, table, req.RequestID); got != want {
			t.Errorf("%s：json 的 pricing_rule_id 推出表格该打 %q，表格打的是 %q\n表格:\n%s",
				req.RequestID, want, got, table)
		}
	}
}

// TestUsageListsStateEmptyResults 钉住空结果也说一句话。
//
// 一行都不打的话，「这个窗口里没有记录」与「命令没跑成」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。两条命令各一档，机器可读档里同一件事是 total 为 0。
func TestUsageListsStateEmptyResults(t *testing.T) {
	setupEnv(t)
	// 先连库再建数据：没有库时这一条跳过，而不是在造夹具那一步报一个
	// 与它想验证的东西无关的错。
	testDB(t)
	userID := createTestUser(t)

	empty := []string{"--user", itoa64(userID),
		"--from", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"--to", time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)}

	cases := []struct {
		verb string
		want string
	}{
		{"events", "这个范围内没有用量事件。"},
		{"requests", "这个范围内没有计费请求。"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			r := runOnce(t, runUsage, append([]string{tc.verb}, empty...))
			if r.err != nil {
				t.Fatalf("usage %s 失败: %v\nstderr:\n%s", tc.verb, r.err, r.stderr)
			}
			if !strings.Contains(r.stdout, tc.want) {
				t.Errorf("空清单没有说出那句话 %q：\nstdout=%s", tc.want, r.stdout)
			}
			// 空结果不是失败：退出码仍是 0，一行表格都没有。
			if strings.Contains(r.stdout, "时刻") {
				t.Errorf("空清单打出了表头：\n%s", r.stdout)
			}
		})
	}
}

// TestUsageLimitSaysHowManyItDropped 钉住被截住时清单自己说出来。
//
// 一份少了后文的清单与一份完整的清单在屏幕上长得一样，看的人会照着第一份下结论
// （见 LESSONS.md 第 16 条），所以末尾那句话必须有；而没被截住时它不该出现——
// 每次都说一句「可能还有」与不说一样没用。
//
// 两条子命令各读一张表、各走一条查询，所以各自的截断与边界都在这一条里钉一次。
func TestUsageLimitSaysHowManyItDropped(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	window := []string{"--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)}
	for i := 0; i < 3; i++ {
		insertUsageEvent(t, db, uniqueName("req"), userID, uniqueName("model"),
			time.Date(2026, 10, 20, 10, i, 0, 0, time.UTC), "0.01000000")
	}

	got := usageEventsModels(t, append(window, "--limit", "2")...)
	if len(got) != 2 {
		t.Fatalf("--limit 2 打了 %d 行，想要 2 行：%v", len(got), got)
	}
	r := runOnce(t, runUsage, append([]string{"events", "--limit", "2"}, window...))
	if !strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("被 --limit 截住时没有说明只打了多少条：\n%s", r.stdout)
	}

	// 不限条数时三条都在，那句话也不该出现。
	got = usageEventsModels(t, append(window, "--limit", "0")...)
	if len(got) != 3 {
		t.Fatalf("--limit 0 打了 %d 行，想要 3 行：%v", len(got), got)
	}
	r = runOnce(t, runUsage, append([]string{"events", "--limit", "0"}, window...))
	if strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("没被截住却说了「更早的没打出来」：\n%s", r.stdout)
	}

	// 边界那一格：--limit 正好等于行数。这时后面没有别的行了，而「打出来的条数
	// 等于上限」在两件事上长得一样（刚好打完 / 恰好被截住），所以截断的判据不能
	// 由行数推——多读一行才分得开。这一格写错时，一份已经读完的清单会劝人再去
	// 调大 --limit。
	r = runOnce(t, runUsage, append([]string{"events", "--limit", "3"}, window...))
	if got := len(tableModels(t, r.stdout)); got != 3 {
		t.Fatalf("--limit 3 打了 %d 行，想要 3 行：\n%s", got, r.stdout)
	}
	if strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("--limit 正好等于行数（后面没有别的行了）却说被截住了：\n%s", r.stdout)
	}

	// 计费请求走的是另一条查询（store 的 BillingRequests），「多读一行」与边界
	// 各有一份，所以这一格它也钉一次。
	for i := 0; i < 3; i++ {
		insertBillingRequest(t, db, uniqueName("req"), userID, uniqueName("model"),
			time.Date(2026, 10, 20, 11, i, 0, 0, time.UTC), "pending")
	}
	r = runOnce(t, runUsage, append([]string{"requests", "--limit", "3"}, window...))
	if got := countUsageRows(r.stdout); got != 3 {
		t.Fatalf("usage requests --limit 3 打了 %d 行，想要 3 行：\n%s", got, r.stdout)
	}
	if strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("usage requests 的 --limit 正好等于行数（后面没有别的行了）却说被截住了：\n%s",
			r.stdout)
	}
}

// TestUsageJSONShapes 钉住机器可读档的两条形状。
//
// 判据是「只有一份 JSON、金额是定点字符串、没结算的那一栏是 null、筛的是什么
// 带在 summary 里」——这几样都是给管道下游看的契约（见 docs/cli-output.md）。
func TestUsageJSONShapes(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	model := uniqueName("model")
	pendingReq := uniqueName("req-pending")
	insertUsageEvent(t, db, uniqueName("req"), userID, model,
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "0.01234500")
	insertBillingRequest(t, db, pendingReq, userID, model,
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "pending")

	// 夹具都在切到 json 之前建好：写类的命令在那一档会被拒（见 format.go 的
	// jsonUnsupported）。这一组只用读类命令，但这条纪律照样守着。
	withFormat(t, formatJSON)

	r := runOnce(t, runUsage, []string{"events", "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)})
	if r.err != nil {
		t.Fatalf("usage events -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var events struct {
		Status  string `json:"status"`
		Summary struct {
			Total     int    `json:"total"`
			Truncated bool   `json:"truncated"`
			Limit     int    `json:"limit"`
			UserID    *int64 `json:"user_id"`
			From      string `json:"from"`
		} `json:"summary"`
		Events []struct {
			RequestID  string `json:"request_id"`
			UserID     int64  `json:"user_id"`
			Model      string `json:"canonical_model"`
			Cost       string `json:"cost"`
			Billing    string `json:"billing_path"`
			CreatedAt  string `json:"created_at"`
			Prompt     int64  `json:"prompt_tokens"`
			Completion int64  `json:"completion_tokens"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &events); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if events.Status != jsonOK || events.Summary.Total != 1 || len(events.Events) != 1 {
		t.Fatalf("形状不对：status=%q total=%d 行数=%d\n%s",
			events.Status, events.Summary.Total, len(events.Events), r.stdout)
	}
	if events.Summary.Truncated {
		t.Errorf("一条都没截掉，truncated 却是 true：\n%s", r.stdout)
	}
	// 默认上限写成字面量，不引 usageDefaultLimit：引那个常量的话，它哪天被改小，
	// 这一条会跟着一起改，等于没钉住「不给 --limit 时是文档里那个数」。
	if events.Summary.Limit != 50 {
		t.Errorf("summary.limit = %d，没给 --limit 时应当是默认的 50（见帮助与 docs/cli-reference.md）",
			events.Summary.Limit)
	}
	if events.Summary.UserID == nil || *events.Summary.UserID != userID {
		t.Errorf("summary 里没有说这次筛的是哪个用户：%s", r.stdout)
	}
	if events.Summary.From == "" {
		t.Errorf("summary 里没有说这次窗口从哪开始：%s", r.stdout)
	}
	e := events.Events[0]
	if e.UserID != userID || e.Model != model {
		t.Errorf("这一行是 %+v，想要 user_id=%d model=%s", e, userID, model)
	}
	// 金额是十进制定点字符串，不是 JSON 数字：过一遍 float64 会丢精度。
	if e.Cost != "0.01234500" {
		t.Errorf("cost = %q，想要定点字符串 0.01234500", e.Cost)
	}
	if e.Billing != "measured" || e.CreatedAt == "" {
		t.Errorf("billing_path 或 created_at 不对：%+v", e)
	}

	r = runOnce(t, runUsage, []string{"requests", "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)})
	if r.err != nil {
		t.Fatalf("usage requests -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var requests struct {
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Requests []struct {
			RequestID     string  `json:"request_id"`
			Status        string  `json:"status"`
			Reserved      string  `json:"reserved_amount"`
			FinalAmount   *string `json:"final_amount"`
			SettledAt     *string `json:"settled_at"`
			PricingRuleID *int64  `json:"pricing_rule_id"`
			Streamed      bool    `json:"streamed"`
			CanonicalName string  `json:"canonical_model"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &requests); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if requests.Summary.Total != 1 || len(requests.Requests) != 1 {
		t.Fatalf("形状不对：total=%d 行数=%d\n%s",
			requests.Summary.Total, len(requests.Requests), r.stdout)
	}
	req := requests.Requests[0]
	if req.RequestID != pendingReq || req.Status != "pending" {
		t.Errorf("这一行是 %+v，想要 request_id=%s status=pending", req, pendingReq)
	}
	// 没结算：结算金额与结算时刻都是 null，不是 `-`，也不是 0。
	if req.FinalAmount != nil || req.SettledAt != nil {
		t.Errorf("没结算的请求，final_amount 与 settled_at 应当是 null：%+v", req)
	}
	// 这一行的夹具没挂规则：规则 id 同样是 null，不是 0。
	if req.PricingRuleID != nil {
		t.Errorf("没落到任何规则上，pricing_rule_id 应当是 null：%+v", req)
	}
	if req.Reserved != "0.02000000" {
		t.Errorf("reserved_amount = %q，想要 0.02000000", req.Reserved)
	}
}

// TestUsageRejectsBadFlags 钉住参数错在连库之前就报出来。
//
// 这一组**不连库**：判据是参数错（退出码 2），而那要发生在配置加载与连库之前
// ——先连库再报参数错的话，一台连不上库的机器上这几个错都看不出来。
func TestUsageRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"窗口上下颠倒", []string{"events", "--from", "2026-11-01", "--to", "2026-10-01"},
			"--from 必须早于 --to"},
		{"时刻看不懂", []string{"events", "--from", "昨天"}, "--from 的取值"},
		{"条数为负", []string{"events", "--limit", "-1"}, "--limit 不能为负"},
		{"多余的位置参数", []string{"events", "alice"}, "不接受位置参数"},
		{"没有子命令", nil, "usage 需要子命令"},
		{"未知子命令", []string{"spend"}, "未知的 usage 子命令"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, runUsage, tc.args)
			if r.err == nil {
				t.Fatalf("这一次应当报错，却成功返回了：stdout=%q", r.stdout)
			}
			if got := exitCode(r.err); got != exitUsage {
				t.Errorf("退出码 = %d，参数错应当是 %d（err=%v）", got, exitUsage, r.err)
			}
			if !strings.Contains(r.err.Error(), tc.want) {
				t.Errorf("错误里没有 %q：%v", tc.want, r.err)
			}
		})
	}
}

// usernameOf 取一个用户的用户名，供「ID 与用户名两种写法都认」那一档用。
func usernameOf(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()

	var name string
	if err := db.QueryRowContext(context.Background(),
		`SELECT username FROM users WHERE id = $1`, userID).Scan(&name); err != nil {
		t.Fatalf("查用户名失败: %v", err)
	}
	return name
}

// itoa64 把用户 ID 打成命令行上那种写法。
func itoa64(v int64) string { return fmt.Sprintf("%d", v) }

// contains 判断一串里有没有这一项。
func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// insertBillingRequestWithReservations 写一行计费请求并显式给出两笔预留。
//
// 通用夹具 insertBillingRequest 把钱固定成 0.02 且不碰 plan_reserved_amount；
// 这一组要的正是「钱包 0、包有值」那一格，所以两列都由调用方给。
func insertBillingRequestWithReservations(t *testing.T, db *sql.DB, requestID string,
	userID int64, model string, startedAt time.Time, wallet, plan string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO billing_requests
			(request_id, user_id, canonical_model, protocol, streamed,
			 reserved_amount, plan_reserved_amount, reserved_asset, status, started_at)
		VALUES ($1, $2, $3, 'openai', false, $4::numeric, $5::numeric, 'CNY', 'pending', $6)`,
		requestID, userID, model, wallet, plan, startedAt); err != nil {
		t.Fatalf("写计费请求失败: %v", err)
	}
}

// usageRequestRowFields 找到含 needle 的那一行计费请求，按空白切开交回来。
//
// 调用方一律从**行尾**取栏：末三栏恒在——包预留恒有值，结算与规则可能打 `-`
// （非空，不影响项数），所以从后面数不会因为时刻自带空格、用户名或模型里出现
// 空白而错位（见 LESSONS 里「清单表用例按空白项数解析」那一条）。
func usageRequestRowFields(t *testing.T, out, needle string) []string {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		if usageRowClock.MatchString(line) && strings.Contains(line, needle) {
			return strings.Fields(line)
		}
	}
	t.Fatalf("表格里没有含 %q 的数据行：\n%s", needle, out)
	return nil
}

// TestUsageRequestsShowsPlanReserved 钉住「钱包预留」与「包预留」两栏并列出现。
//
// 两条夹具刻意写成互补的两格：额度包覆盖的在途请求是钱包 0、包有值，而走余额的
// 是钱包有值、包 0。把包的面值并进钱包那一栏、或者只读 reserved_amount 时，
// 「包预留」那一条断言会红；只读 plan_reserved_amount 时，钱包那一格会红。
func TestUsageRequestsShowsPlanReserved(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	window := []string{"--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)}
	startedAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	planReq := uniqueName("req-plan")
	walletReq := uniqueName("req-wallet")
	insertBillingRequestWithReservations(t, db, planReq, userID,
		uniqueName("model"), startedAt, "0", "2.50")
	insertBillingRequestWithReservations(t, db, walletReq, userID,
		uniqueName("model"), startedAt.Add(time.Minute), "1.50", "0")

	// ── 表格 ──
	out := runUsageRequestsOut(t, window...)
	if !strings.Contains(out, "钱包预留") || !strings.Contains(out, "包预留") {
		t.Errorf("表头应当有「钱包预留」与「包预留」两栏：\n%s", out)
	}
	// 末四栏依次是：钱包预留、包预留、结算、规则。pending 行的结算与规则都是 `-`。
	cells := usageRequestRowFields(t, out, planReq)
	if got := cells[len(cells)-4]; got != "0.00000000" {
		t.Errorf("%s 的钱包预留 = %q，额度包覆盖的请求应当是 0：\n%s", planReq, got, out)
	}
	if got := cells[len(cells)-3]; got != "2.50000000" {
		t.Errorf("%s 的包预留 = %q，期望 2.50000000：\n%s", planReq, got, out)
	}
	cells = usageRequestRowFields(t, out, walletReq)
	if got := cells[len(cells)-4]; got != "1.50000000" {
		t.Errorf("%s 的钱包预留 = %q，期望 1.50000000：\n%s", walletReq, got, out)
	}
	if got := cells[len(cells)-3]; got != "0.00000000" {
		t.Errorf("%s 的包预留 = %q，走余额的请求应当是 0：\n%s", walletReq, got, out)
	}

	// ── 机器可读档 ──
	// 夹具都在切到 json 之前建好（见 format.go 的 jsonUnsupported）。
	withFormat(t, formatJSON)

	r := runOnce(t, runUsage, append([]string{"requests"}, window...))
	if r.err != nil {
		t.Fatalf("usage requests -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var doc struct {
		Requests []struct {
			RequestID    string `json:"request_id"`
			Reserved     string `json:"reserved_amount"`
			PlanReserved string `json:"plan_reserved_amount"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if len(doc.Requests) != 2 {
		t.Fatalf("json 里 %d 行，想要 2 行：\n%s", len(doc.Requests), r.stdout)
	}
	got := map[string]struct{ wallet, plan string }{}
	for _, req := range doc.Requests {
		got[req.RequestID] = struct{ wallet, plan string }{req.Reserved, req.PlanReserved}
	}
	if pair := got[planReq]; pair.wallet != "0.00000000" || pair.plan != "2.50000000" {
		t.Errorf("%s 读到 wallet=%q plan=%q，期望 0 与 2.50000000：\n%s",
			planReq, pair.wallet, pair.plan, r.stdout)
	}
	if pair := got[walletReq]; pair.wallet != "1.50000000" || pair.plan != "0.00000000" {
		t.Errorf("%s 读到 wallet=%q plan=%q，期望 1.50000000 与 0：\n%s",
			walletReq, pair.wallet, pair.plan, r.stdout)
	}
}
