package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/plans"
)

// ── 参数与文档形状（不连库）─────────────────────────────────────────────

// TestUserInspectRequiresItsOperand 管的是缺宾语时报的那句话。
//
// 位置参数是这一层的语法核心：漏掉它时报「缺少 <用户>」，而不是让下面那条
// 命令去说「缺少 --user」——后者会让人以为旗标名写错了。多给一个宾语同样
// 按参数错处置，不静默忽略（那会让「查的是谁」变成一件要猜的事）。
func TestUserInspectRequiresItsOperand(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺用户", []string{"inspect"}, "<用户>"},
		{"多给一个宾语", []string{"inspect", "alice", "bob"}, "只接受一个用户"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runUser(tc.args) })
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）：%v", got, exitUsage, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里没有点名 %q：%v", tc.want, err)
			}
		})
	}
}

// TestUserInspectHelpListsInspect 要求两个帮助版本都列出这一条。
//
// `user -h` 是这些动作唯一的门面，少一行就等于它只能靠猜。
func TestUserInspectHelpListsInspect(t *testing.T) {
	for what, text := range map[string]string{"短版": userShortUsage, "长版": userUsage} {
		if !strings.Contains(text, "user inspect") {
			t.Errorf("user 的%s里没有 user inspect", what)
		}
	}
}

// TestUserInspectDocKeepsCollectionsArrays 要求空集合是 []、缺值是 null。
//
// 判据是「空订阅、无活跃 key、零余额三种情况都稳定（不出现 null 与 [] 混用）」，
// 而这条不连库：空切片与 nil 的差别在组装 doc 那一步就已经定了，真库只是把
// 同一段判断跑到另一份数据上。
func TestUserInspectDocKeepsCollectionsArrays(t *testing.T) {
	doc := userInspectDoc(auth.UserInfo{}, nil, nil, nil, plans.ChargeOrderPlanFirst, plans.ChargeOrderSourcePlatform, nil)

	for _, key := range []string{"wallets", "subscriptions", "keys"} {
		value, ok := doc[key]
		if !ok {
			t.Fatalf("doc 里没有 %q", key)
		}
		list, ok := value.([]map[string]any)
		if !ok {
			t.Fatalf("%q 不是条目的切片：%T", key, value)
		}
		if list == nil {
			t.Errorf("%q 是 nil 切片：空集合要打 []，不是 null", key)
		}
		if len(list) != 0 {
			t.Errorf("%q 空集时有 %d 条", key, len(list))
		}
	}

	// 缺值那一侧仍走 nullableString / jsonTimePtr：空 UserInfo 里 email 与
	// deleted_at 就是 null。两档在同一个 doc 里并存，正是这条判据要的。
	user, ok := doc["user"].(map[string]any)
	if !ok {
		t.Fatalf("doc 里没有 user 对象：%T", doc["user"])
	}
	if user["email"] != nil {
		t.Errorf("空用户的 email 应当是 null，得到 %#v", user["email"])
	}
	if user["deleted_at"] != nil {
		t.Errorf("没销户时 deleted_at 应当是 null，得到 %#v", user["deleted_at"])
	}

	// 扣费顺序两栏总是有值：它不是集合，没有「一条都没有」这一档。
	if _, ok := doc["order"].(string); !ok {
		t.Errorf("order 应当是字符串，得到 %#v", doc["order"])
	}
	if _, ok := doc["source"].(string); !ok {
		t.Errorf("source 应当是字符串，得到 %#v", doc["source"])
	}
}

// TestPrintUserInspectEmbedsEachSection 是判据一在渲染这一层的证据（不连库）。
//
// 真库那一份（TestUserInspectMatchesSeparateCommands）证明两条路读到的是同一份
// 数据；这一条证明拼装那一步没有另写一份排版——每一段都直接调用对应命令的
// 打印函数，所以那一段与单独跑那条命令的 stdout 逐字相同。分开写是因为拼接
// 出错的两种方式（多一栏、少一栏）在真库那一份里要等到夹具变复杂才现形。
func TestPrintUserInspectEmbedsEachSection(t *testing.T) {
	info := auth.UserInfo{
		ID:       7,
		Username: "user-placeholder",
		Email:    "user@example.com",
		Status:   "active",
		Role:     auth.RoleUser,
	}
	rows := []walletRow{{
		UserID:   info.ID,
		Username: info.Username,
		Asset:    ledger.AssetCNY,
		Balance:  money.MustParse("12.5"),
	}}
	subs := []plans.UserPlan{{
		ID:        21,
		UserID:    info.ID,
		PlanID:    3,
		PlanKey:   "plan-placeholder",
		Asset:     ledger.AssetCNY,
		Unit:      plans.UnitAmount,
		Status:    plans.UserPlanActive,
		FaceValue: money.MustParse("5"),
		Ref:       "ref-placeholder",
	}}
	keys := []auth.Key{{
		ID:       31,
		UserID:   info.ID,
		Username: info.Username,
		Prefix:   "lx_placeholder",
		Status:   "active",
	}}
	order, source := plans.ChargeOrderBalanceFirst, plans.ChargeOrderSourceUser

	var inspect bytes.Buffer
	printUserInspect(&inspect, info, rows, subs, order, source, keys)
	out := inspect.String()

	sections := []struct {
		name  string
		print func(io.Writer)
	}{
		{"账号", func(w io.Writer) { printUserDetail(w, info) }},
		{"钱包", func(w io.Writer) { printWalletList(w, rows) }},
		{"订阅", func(w io.Writer) { printUserPlans(w, subs) }},
		{"扣费顺序", func(w io.Writer) {
			printPlanOrder(w, auth.User{ID: info.ID, Username: info.Username}, order, source)
		}},
		{"API Key", func(w io.Writer) { printKeyList(w, keys) }},
	}
	for _, section := range sections {
		var standalone bytes.Buffer
		section.print(&standalone)
		if !strings.Contains(out, standalone.String()) {
			t.Errorf("%s 那一段与单独跑对应命令的输出不同：\n--- 全景 ---\n%s\n--- %s ---\n%s",
				section.name, out, section.name, standalone.String())
		}
		if !strings.Contains(out, "\n"+section.name+"：\n") {
			t.Errorf("全景里没有 %s 的段标题：\n%s", section.name, out)
		}
	}
}

// ── 以下要连库 ──────────────────────────────────────────────────────────

// runJSONDoc 跑一条命令的 json 档，返回解出来的顶层对象。
func runJSONDoc(t *testing.T, fn func() error) map[string]any {
	t.Helper()
	saved := outputFormat
	outputFormat = formatJSON
	defer func() { outputFormat = saved }()

	stdout, stderr, err := runStream(t, fn)
	if err != nil {
		t.Fatalf("命令没跑成: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout 不是 JSON：%v\n%s", err, stdout)
	}
	return doc
}

// sameJSON 断言两份字段的 JSON 编码逐字相同。
//
// 比编码后的字节而不是 reflect.DeepEqual：判据是「与那条命令的输出逐字相等」，
// 编码结果正是下游 `| jq` 看到的东西；map 的键在 encoding/json 里按字典序
// 输出，所以同一份数据编两遍不会因为遍历顺序不同而误报。
func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("编码 %s（inspect）失败: %v", what, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("编码 %s（单独命令）失败: %v", what, err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("%s 与单独跑那条命令的输出不同：\nuser inspect: %s\n单独命令:    %s", what, gotJSON, wantJSON)
	}
}

// TestUserInspectMatchesSeparateCommands 是判据一的对拍。
//
// 同一个库、同一份夹具，两条路各跑一次：先跑分别的五条命令、再跑 inspect，
// 比的是两次生成的字符串，不是手抄的期望值——手抄的那一份会在别人改子命令
// 之后静默过时。
//
// 夹具里刻意放了**已经作废的订阅**与**已经停用的 Key**：列表不按状态过滤，
// 靠各自的状态栏区分（见 printUserInspect 与 userInspectDoc 的注释）。少了
// 这两条，一个「只列 active」的实现也会照样通过。
func TestUserInspectMatchesSeparateCommands(t *testing.T) {
	db, userID, planKey := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)

	// 钱包：一个零余额之外的真余额，让金额那两栏不是占位。
	mustStream(t, func() error { return runWallet([]string{"create", "--user", user}) })
	mustStream(t, func() error {
		return runUser([]string{"wallet", "credit", user, "--amount", "12.5", "--ref", uniqueName("inspect-credit")})
	})

	// 订阅：一条留着、一条作废，证明列表里两种状态都在。
	mustStream(t, func() error {
		return runUser([]string{"plan", "grant", user, planKey, "--ref", uniqueName("inspect-grant-ok"), "--forever"})
	})
	voidedRef := uniqueName("inspect-grant-voided")
	mustStream(t, func() error {
		return runUser([]string{"plan", "grant", user, planKey, "--ref", voidedRef, "--forever"})
	})
	var voidedID int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT id FROM user_plans WHERE ref = $1`, voidedRef).Scan(&voidedID); err != nil {
		t.Fatalf("读第二条订阅的 id 失败: %v", err)
	}
	mustStream(t, func() error {
		return runUser([]string{"plan", "void", strconv.FormatInt(voidedID, 10), "--ref", uniqueName("inspect-void")})
	})

	// 扣费顺序：改成用户覆盖，证明 source 那一栏也是真读出来的。
	mustStream(t, func() error {
		return runUser([]string{"plan", "order", "set", user, "--order", "balance_first"})
	})

	// Key：两把，一把停用、一把带过期时间。
	for range 2 {
		mustStream(t, func() error { return runUser([]string{"key", "create", user, "--ttl", "30d"}) })
	}
	var keyID int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT max(id) FROM api_keys WHERE user_id = $1`, userID).Scan(&keyID); err != nil {
		t.Fatalf("读新 key 的 id 失败: %v", err)
	}
	mustStream(t, func() error {
		return runAPIKey([]string{"update", "--id", strconv.FormatInt(keyID, 10), "--status", "disabled"})
	})

	// 表格那一档：每一段都必须是那条命令 stdout 的一整块，逐字相同。
	inspectOut, _ := mustStream(t, func() error { return runUser([]string{"inspect", user}) })
	blocks := []struct {
		name string
		run  func() error
	}{
		{"user get", func() error { return runUser([]string{"get", "--user", user}) }},
		{"user wallet show", func() error { return runUser([]string{"wallet", "show", user}) }},
		{"user plan list", func() error { return runUser([]string{"plan", "list", user}) }},
		{"user plan order show", func() error { return runUser([]string{"plan", "order", "show", user}) }},
		{"user key list", func() error { return runUser([]string{"key", "list", user}) }},
	}
	for _, block := range blocks {
		want, _ := mustStream(t, block.run)
		if !strings.Contains(inspectOut, want) {
			t.Errorf("user inspect 的输出里没有 %s 的整段输出：\n--- inspect ---\n%s\n--- %s ---\n%s",
				block.name, inspectOut, block.name, want)
		}
	}

	// json 那一档：字段名与值都按那几条命令的那一份比。
	inspectDoc := runJSONDoc(t, func() error { return runUser([]string{"inspect", user}) })
	getDoc := runJSONDoc(t, func() error { return runUser([]string{"get", "--user", user}) })
	walletDoc := runJSONDoc(t, func() error { return runUser([]string{"wallet", "show", user}) })
	planDoc := runJSONDoc(t, func() error { return runUser([]string{"plan", "list", user}) })
	orderDoc := runJSONDoc(t, func() error { return runUser([]string{"plan", "order", "show", user}) })
	keyDoc := runJSONDoc(t, func() error { return runUser([]string{"key", "list", user}) })

	sameJSON(t, "user", inspectDoc["user"], getDoc["user"])
	sameJSON(t, "wallets", inspectDoc["wallets"], walletDoc["wallets"])
	sameJSON(t, "subscriptions", inspectDoc["subscriptions"], planDoc["subscriptions"])
	sameJSON(t, "keys", inspectDoc["keys"], keyDoc["keys"])
	sameJSON(t, "order", inspectDoc["order"], orderDoc["order"])
	sameJSON(t, "source", inspectDoc["source"], orderDoc["source"])

	// 上面那几条对拍只有在夹具真的带了非 active 条目时才说明得了「不过滤」。
	// 把它们数出来，免得将来夹具退化成一个只含 active 的集合而这条用例照样绿。
	subs, _ := inspectDoc["subscriptions"].([]any)
	if len(subs) != 2 {
		t.Fatalf("夹具应当有两条订阅（一条有效、一条作废），inspect 给了 %d 条", len(subs))
	}
	if !jsonContainsStatus(t, subs, "canceled") {
		t.Errorf("inspect 的订阅里没有作废的那一条：%v", subs)
	}
	keys, _ := inspectDoc["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("夹具应当有两把 Key（一把有效、一把停用），inspect 给了 %d 把", len(keys))
	}
	if !jsonContainsStatus(t, keys, "disabled") {
		t.Errorf("inspect 的 Key 里没有停用的那一把：%v", keys)
	}
}

// jsonContainsStatus 报告 entries 里有没有一条的 status 是 want。
func jsonContainsStatus(t *testing.T, entries []any, want string) bool {
	t.Helper()
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("条目不是对象：%T", raw)
		}
		if entry["status"] == want {
			return true
		}
	}
	return false
}

// TestUserInspectUnknownUserMatchesUserGet 是判据二：同一条错误路径。
//
// 文案与退出码都要与 user get 一样，所以比的是两条路各自返回的那一个错误：
// 它是同一句 `%w: %q`（auth.UserByRef），而不是在这里另写一句。
func TestUserInspectUnknownUserMatchesUserGet(t *testing.T) {
	testDB(t)
	setupEnv(t)
	ref := uniqueName("nobody")

	getErr := runCaptured(t, func() error { return runUser([]string{"get", "--user", ref}) })
	inspectErr := runCaptured(t, func() error { return runUser([]string{"inspect", ref}) })

	if getErr == nil || inspectErr == nil {
		t.Fatalf("用户不存在时两条命令都该失败：user get = %v，user inspect = %v", getErr, inspectErr)
	}
	if getErr.Error() != inspectErr.Error() {
		t.Errorf("错误文案不同：\nuser get:     %v\nuser inspect: %v", getErr, inspectErr)
	}
	if got, want := exitCode(inspectErr), exitCode(getErr); got != want {
		t.Errorf("退出码 = %d，user get 是 %d", got, want)
	}
	if !errors.Is(inspectErr, auth.ErrUserNotFound) {
		t.Errorf("错误链里没有 auth.ErrUserNotFound：%v", inspectErr)
	}
}

// TestUserInspectJSONStableOnEmptyStates 是判据三：三种空状态各跑一遍。
//
// 一个刚建出来、只开了钱包的用户：没有订阅、没有 Key、余额是 0。这三件事在
// json 里各自是一个数组或一个金额字符串，**没有一处是 null**——「没有条目」
// 与「这一栏没有值」是两件事，混用之后下游分不出「查了，没有」与「没查」。
func TestUserInspectJSONStableOnEmptyStates(t *testing.T) {
	_, userID, _ := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)
	mustStream(t, func() error { return runWallet([]string{"create", "--user", user}) })

	doc := runJSONDoc(t, func() error { return runUser([]string{"inspect", user}) })

	for _, key := range []string{"wallets", "subscriptions", "keys"} {
		value, ok := doc[key]
		if !ok {
			t.Fatalf("doc 里没有 %q", key)
		}
		if value == nil {
			t.Fatalf("%q 是 null：空集合要打 []", key)
		}
		if _, ok := value.([]any); !ok {
			t.Fatalf("%q 不是数组：%T", key, value)
		}
	}
	if subs, _ := doc["subscriptions"].([]any); len(subs) != 0 {
		t.Errorf("新用户不该有订阅，得到 %v", subs)
	}
	if keys, _ := doc["keys"].([]any); len(keys) != 0 {
		t.Errorf("新用户不该有 Key，得到 %v", keys)
	}

	// 零余额：一行钱包，两栏都是定点小数的字符串，不是 null、也不是数字。
	wallets, _ := doc["wallets"].([]any)
	if len(wallets) != 1 {
		t.Fatalf("刚开户的用户应当有一行钱包，得到 %v", wallets)
	}
	row, ok := wallets[0].(map[string]any)
	if !ok {
		t.Fatalf("钱包条目不是对象：%T", wallets[0])
	}
	for _, column := range []string{"balance", "frozen"} {
		if row[column] != "0.00000000" {
			t.Errorf("%s = %#v，期望零余额的定点串 %q", column, row[column], "0.00000000")
		}
	}
}

// TestUserInspectJSONKeepsInactiveKey 说的是「无活跃 Key」不等于「没有 Key」。
//
// 一把停用的 Key 仍然要出现在数组里（带着它的状态）：列表回答的是「这个人
// 名下有哪几把钥匙」，而「哪一把现在能用」由 status 那一栏说。把停用的那些
// 从数组里拿掉，读的人就分不出「从来没建过」与「建过、已经停了」——
// 后者恰恰是排查时要看的。
func TestUserInspectJSONKeepsInactiveKey(t *testing.T) {
	db, userID, _ := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)
	mustStream(t, func() error { return runUser([]string{"key", "create", user}) })

	var keyID int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT max(id) FROM api_keys WHERE user_id = $1`, userID).Scan(&keyID); err != nil {
		t.Fatalf("读新 key 的 id 失败: %v", err)
	}
	mustStream(t, func() error {
		return runAPIKey([]string{"update", "--id", strconv.FormatInt(keyID, 10), "--status", "disabled"})
	})

	doc := runJSONDoc(t, func() error { return runUser([]string{"inspect", user}) })
	keys, ok := doc["keys"].([]any)
	if !ok {
		t.Fatalf("keys 不是数组：%T", doc["keys"])
	}
	if len(keys) != 1 {
		t.Fatalf("停用的 Key 也应当在数组里，得到 %v", keys)
	}
	entry, _ := keys[0].(map[string]any)
	if entry["status"] != "disabled" {
		t.Errorf("Key 的 status = %#v，期望 disabled", entry["status"])
	}
}
