package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/expiry"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/plans"
)

// 这一组钉 plan 这条命令的两半：参数判据（不连库）与一条走通的完整流程（连库）。
//
// 流程那一条从 create 一路走到 void，中间读回三次：`subscriptions` 看见发放、
// `key show` 看见名单收窄、`order show` 看见用户覆盖与跟随默认的切换。每一步都
// 只断言**屏幕上那句话**，因为命令与库之间的那一层（internal/plans）有自己的
// 用例；这里要证明的是「命令行把旗标接到了那一层、把结果打了出来」。

// plan key set 的两种错法：什么都没给、两个相反的意思一起给。
func TestPlanKeySetRejectsMissingOrConflictingSelection(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"既没 --plans 也没 --all", []string{"key", "set", "--id", "1"}, "--all"},
		{"--plans 与 --all 一起给", []string{"key", "set", "--id", "1", "--plans", "2", "--all"}, "不能同时"},
		{"名单里是空项", []string{"key", "set", "--id", "1", "--plans", "2,,3"}, "第 2 项"},
		{"名单里不是 id", []string{"key", "set", "--id", "1", "--plans", "abc"}, "不是正的整数"},
		{"名单里是 0", []string{"key", "set", "--id", "1", "--plans", "0"}, "不是正的整数"},
		{"缺 --id", []string{"key", "set", "--plans", "2"}, "缺少 --id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 这些错误都在连库之前判出来：不需要 DSN，也不会碰库。
			t.Setenv("LARKAX_DB_DSN", "")
			err := runPlan(tc.args)
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if exitCode(err) != exitUsage {
				t.Errorf("应当报参数错（退出码 2），得到 %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里没有 %q：%v", tc.want, err)
			}
		})
	}
}

// plan order set 的两种错法，外加一个认不出的顺序。
func TestPlanOrderSetRejectsBadOrder(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"既没 --order 也没 --clear", []string{"order", "set", "--user", "1"}, "--clear"},
		{"--order 与 --clear 一起给", []string{"order", "set", "--user", "1", "--order", "plan_first", "--clear"}, "不能同时"},
		{"认不出的顺序", []string{"order", "set", "--user", "1", "--order", "tokens_first"}, "tokens_first"},
		{"缺 --user", []string{"order", "set", "--order", "plan_first"}, "缺少 --user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LARKAX_DB_DSN", "")
			err := runPlan(tc.args)
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if exitCode(err) != exitUsage {
				t.Errorf("应当报参数错（退出码 2），得到 %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里没有 %q：%v", tc.want, err)
			}
		})
	}
}

// plan 的子命令缺省与未知值。
func TestPlanDispatchRejectsUnknownSubcommands(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")
	for _, args := range [][]string{{}, {"bogus"}, {"key"}, {"key", "bogus"}, {"order"}, {"order", "bogus"}} {
		if err := runPlan(args); err == nil {
			t.Errorf("plan %v 应当报错", args)
		}
	}
}

// parseIDList 的正向用法。
func TestParseIDList(t *testing.T) {
	got, err := parseIDList("3, 1,3")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []int64{3, 1, 3}
	if len(got) != len(want) {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %d，期望 %d", i+1, got[i], want[i])
		}
	}
}

// plan 走一遍完整流程：建包、发放、看订阅、收窄名单、切扣费顺序、作废。
func TestPlanCLIFlow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := createTestUser(t)
	user := strconv.FormatInt(userID, 10)

	planKey := uniqueName("cli-plan")
	stdout, stderr := mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "CLI 额度包",
			"--unit", "amount", "--quota", "5",
		})
	})
	if stdout != "" {
		t.Errorf("写类命令不该往 stdout 写东西，得到 %q", stdout)
	}
	if !strings.Contains(stderr, "已建额度包 "+planKey) {
		t.Errorf("建包回显不对：%q", stderr)
	}

	// 同一个发放单号重放：第二次打「已经发过」，退出 0。
	ref := uniqueName("cli-grant")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	grantArgs := []string{
		"grant", "--user", user, "--plan", planKey, "--ref", ref, "--expires-at", expires,
	}
	_, stderr = mustStream(t, func() error { return runPlan(grantArgs) })
	if !strings.Contains(stderr, "已发放订阅") {
		t.Errorf("发放回显不对：%q", stderr)
	}
	var upID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM user_plans WHERE ref = $1`, ref).Scan(&upID); err != nil {
		t.Fatalf("读订阅 id 失败: %v", err)
	}
	_, stderr = mustStream(t, func() error { return runPlan(grantArgs) })
	if !strings.Contains(stderr, "已经发过") {
		t.Errorf("重放回显没有说「已经发过」：%q", stderr)
	}

	// 订阅清单里能看见它，剩余面值等于面值。
	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"subscriptions", "--user", user})
	})
	if !strings.Contains(stdout, planKey) {
		t.Errorf("订阅清单里没有 %q：\n%s", planKey, stdout)
	}
	if !strings.Contains(stdout, "5") {
		t.Errorf("订阅清单里没有剩余面值：\n%s", stdout)
	}

	// 一把 key，名单从「全部」收窄到这一条，再清回「全部」。
	var keyID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, key_hash, key_prefix)
		VALUES ($1, $2, $3) RETURNING id`,
		userID, uniqueName("cli-hash"), uniqueName("cli-prefix")).Scan(&keyID); err != nil {
		t.Fatalf("建 API Key 失败: %v", err)
	}
	keyArg := strconv.FormatInt(keyID, 10)

	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"key", "show", "--id", keyArg})
	})
	if !strings.Contains(stdout, "名单：全部") {
		t.Errorf("没配名单时应当说「全部」：\n%s", stdout)
	}

	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"key", "set", "--id", keyArg, "--plans", strconv.FormatInt(upID, 10)})
	})
	if !strings.Contains(stderr, "名单已更新") {
		t.Errorf("设名单回显不对：%q", stderr)
	}
	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"key", "show", "--id", keyArg})
	})
	if strings.Contains(stdout, "名单：全部") {
		t.Errorf("设过名单之后还说「全部」：\n%s", stdout)
	}
	if !strings.Contains(stdout, planKey) {
		t.Errorf("名单里那条订阅没有打出来：\n%s", stdout)
	}

	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"key", "set", "--id", keyArg, "--all"})
	})
	if !strings.Contains(stderr, "已清空") {
		t.Errorf("清空名单回显不对：%q", stderr)
	}

	// 扣费顺序：跟随默认 → 用户覆盖 → 清掉覆盖。
	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"order", "show", "--user", user})
	})
	if !strings.Contains(stdout, "plan_first") || !strings.Contains(stdout, "跟随平台默认") {
		t.Errorf("默认顺序读出来不对：\n%s", stdout)
	}

	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"order", "set", "--user", user, "--order", "balance_first"})
	})
	if !strings.Contains(stderr, "已设为 balance_first") {
		t.Errorf("设顺序回显不对：%q", stderr)
	}
	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"order", "show", "--user", user})
	})
	if !strings.Contains(stdout, "balance_first") || !strings.Contains(stdout, "用户覆盖") {
		t.Errorf("用户覆盖没生效：\n%s", stdout)
	}

	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"order", "set", "--user", user, "--clear"})
	})
	if !strings.Contains(stderr, "已清除") {
		t.Errorf("清覆盖回显不对：%q", stderr)
	}
	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"order", "show", "--user", user})
	})
	if !strings.Contains(stdout, "plan_first") || !strings.Contains(stdout, "跟随平台默认") {
		t.Errorf("清掉覆盖之后没回到默认：\n%s", stdout)
	}

	// 停用只挡新的发放：get 能看见状态，发放被拒；启用之后又能发。
	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"disable", "--key", planKey})
	})
	if !strings.Contains(stderr, "已停用额度包 "+planKey) {
		t.Errorf("停用回显不对：%q", stderr)
	}
	stdout, _ = mustStream(t, func() error {
		return runPlan([]string{"get", "--key", planKey})
	})
	if !strings.Contains(stdout, "停用") {
		t.Errorf("get 没打出停用状态：\n%s", stdout)
	}
	if err := runPlan([]string{
		"grant", "--user", user, "--plan", planKey,
		"--ref", uniqueName("cli-grant-disabled"),
		"--expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}); err == nil {
		t.Error("停用的包还能发放")
	}
	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"enable", "--key", planKey})
	})
	if !strings.Contains(stderr, "已启用额度包 "+planKey) {
		t.Errorf("启用回显不对：%q", stderr)
	}

	// 作废：把剩余面值转回平台，状态变 canceled。
	voidRef := uniqueName("cli-void")
	_, stderr = mustStream(t, func() error {
		return runPlan([]string{"void", "--id", strconv.FormatInt(upID, 10), "--ref", voidRef})
	})
	if !strings.Contains(stderr, "已作废订阅") {
		t.Errorf("作废回显不对：%q", stderr)
	}
}

// plan list 的机器可读输出里有这个包，金额是字符串而不是数字。
func TestPlanListJSON(t *testing.T) {
	testDB(t)
	userID := createTestUser(t) // 顺带确保库已迁移
	_ = userID

	planKey := uniqueName("json-plan")
	_, stderr := mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "JSON 额度包",
			"--unit", "amount", "--quota", "7.5",
		})
	})
	if !strings.Contains(stderr, "已建额度包") {
		t.Fatalf("建包失败: %q", stderr)
	}

	saved := outputFormat
	outputFormat = formatJSON
	defer func() { outputFormat = saved }()

	var err error
	stdout, _ := captureStreams(t, func() { err = runPlan([]string{"list"}) })
	if err != nil {
		t.Fatalf("plan list --format json 出错: %v", err)
	}

	var doc struct {
		Status string `json:"status"`
		Plans  []struct {
			Key        string   `json:"key"`
			Unit       string   `json:"unit"`
			QuotaTotal string   `json:"quota_total"`
			FaceValue  string   `json:"face_value"`
			Models     []string `json:"models"`
		} `json:"plans"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, stdout)
	}
	if doc.Status != jsonOK {
		t.Errorf("status = %q，期望 %q", doc.Status, jsonOK)
	}
	var found bool
	for _, p := range doc.Plans {
		if p.Key != planKey {
			continue
		}
		found = true
		if p.QuotaTotal != "7.50000000" || p.FaceValue != "7.50000000" {
			t.Errorf("金额 = %q / %q，期望都是按精度补零的字符串 \"7.50000000\"", p.QuotaTotal, p.FaceValue)
		}
		if p.Models == nil {
			t.Error("models 应当是 [] 而不是 null")
		}
	}
	if !found {
		t.Errorf("JSON 里没有刚建的包 %q：\n%s", planKey, stdout)
	}
}

// grant 的 --ttl 与 --forever 走真命令写库。
//
// --ttl 的期望值来自注入的现在，与命令自己取的那个是同一个来源；--forever
// 与 apikey 共用同一个远期上限。
func TestPlanGrantTTLAndForever(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := createTestUser(t)
	user := strconv.FormatInt(userID, 10)

	planKey := uniqueName("ttl-plan")
	mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "时长包",
			"--unit", "amount", "--quota", "5",
		})
	})

	now := time.Now().UTC().Truncate(time.Second)
	withExpiryNow(t, now)

	expiryOf := func(ref string) time.Time {
		t.Helper()
		var at time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT period_end FROM user_plans WHERE ref = $1`, ref).Scan(&at); err != nil {
			t.Fatalf("读订阅 %q 的 period_end 失败: %v", ref, err)
		}
		return at
	}

	ttlRef := uniqueName("ttl-grant")
	mustStream(t, func() error {
		return runPlan([]string{
			"grant", "--user", user, "--plan", planKey, "--ref", ttlRef, "--ttl", "30d",
		})
	})
	wantTTL := now.AddDate(0, 0, 30)
	if got := expiryOf(ttlRef); !got.Equal(wantTTL) {
		t.Errorf("--ttl 30d 写入 %s，期望 %s",
			got.Format(time.RFC3339), wantTTL.Format(time.RFC3339))
	}

	foreverRef := uniqueName("forever-grant")
	mustStream(t, func() error {
		return runPlan([]string{
			"grant", "--user", user, "--plan", planKey, "--ref", foreverRef, "--forever",
		})
	})
	if got := expiryOf(foreverRef); got.UTC().Format(time.RFC3339) != expiry.ForeverRFC3339 {
		t.Errorf("--forever 写入 %s，期望 %s",
			got.UTC().Format(time.RFC3339), expiry.ForeverRFC3339)
	}
}

// 三个过期写法互斥：plan grant 的参数判据不连库。
func TestPlanGrantRejectsConflictingExpiryFlags(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"expires-at 与 ttl", []string{"grant", "--user", "1", "--plan", "p", "--ref", "r",
			"--expires-at", "2030-01-01T00:00:00Z", "--ttl", "30d"}, "不能同时给"},
		{"ttl 与 forever", []string{"grant", "--user", "1", "--plan", "p", "--ref", "r",
			"--ttl", "30d", "--forever"}, "不能同时给"},
		{"三个都没给", []string{"grant", "--user", "1", "--plan", "p", "--ref", "r"},
			"缺少过期时间"},
		{"ttl 单位不认识", []string{"grant", "--user", "1", "--plan", "p", "--ref", "r",
			"--ttl", "30w"}, "单位"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runPlan(tc.args)
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if exitCode(err) != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", exitCode(err), exitUsage)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，里面没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// plan list 的人读表有「售卖」一栏：可售的包打天数，不可售的打「不售卖」。
//
// 这一栏回答的是「哪些包能卖」——清单页上缺了它，就得逐个 `plan get` 才知道。
// 断言按「这一行里有没有那个值」判，不按空白切出来的字段数：表格列宽随内容变，
// 按字段数解析的用例在加列时会静默错位（本仓踩过这一条）。
func TestPlanListShowsValidity(t *testing.T) {
	days := 30
	list := []plans.Plan{
		{
			Key: "sale-listed", Name: "可售包", Unit: plans.UnitAmount,
			QuotaTotal: money.MustParse("30"), FaceValue: money.MustParse("30"),
			ValidityDays: &days, Enabled: true,
		},
		{
			Key: "sale-not-listed", Name: "不出售包", Unit: plans.UnitAmount,
			QuotaTotal: money.MustParse("30"), FaceValue: money.MustParse("30"),
			Enabled: true,
		},
	}

	var buf bytes.Buffer
	printPlanList(&buf, list)
	out := buf.String()

	for _, tc := range []struct{ key, want string }{
		{"sale-listed", "30 天"},
		{"sale-not-listed", "不售卖"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			line := ""
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, tc.key) {
					line = l
					break
				}
			}
			if line == "" {
				t.Fatalf("表里没有 %q 那一行：\n%s", tc.key, out)
			}
			if !strings.Contains(line, tc.want) {
				t.Errorf("%q 那一行里没有 %q（「售卖」栏没打出来）：%q", tc.key, tc.want, line)
			}
		})
	}
}
