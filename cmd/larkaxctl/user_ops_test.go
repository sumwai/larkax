package main

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// ── 用户级履约操作：新路径与旧路径 ──────────────────────────────────────
//
// `user plan|wallet|key|usage` 是这一组操作的正式入口，核心宾语（哪个用户、
// 哪个套餐）写成位置参数。顶层的 `plan` / `wallet` / `apikey` / `usage` 保留
// 旧写法至少一个发布周期，命中时在 stderr 打一行指向新路径。
//
// 这一组钉两件事，缺一半都不成立：
//
//  1. **新路径真的走到了同一段代码**：读类命令的输出与旧写法逐字相同，
//     写类命令落的是同一个状态（幂等键认得出「已经发过」）；
//  2. **旧路径仍然可用，而且说了新路径在哪**：只钉「还能跑」不够——搬家的
//     那一半价值就是让人知道该改去哪儿；只钉提示也不够——提示可以指向一条
//     跑不通的路。

// newUserOpsFixture 建一个用户与一个额度包，供下面几条用例共用。
func newUserOpsFixture(t *testing.T) (db *sql.DB, userID int64, planKey string) {
	t.Helper()
	db = testDB(t)
	setupEnv(t)
	userID = createTestUser(t)
	planKey = uniqueName("user-ops-plan")
	mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "用户级额度包",
			"--unit", "amount", "--quota", "5",
		})
	})
	return db, userID, planKey
}

// TestUserPlanSubcommandRunsTheSameCode 走一遍 user plan 的四个动作。
//
// 每一步都用新路径，并把「新路径真的到了那一层」钉在可观察的结果上：发放之后
// 订阅清单里能看见那个包、同一个单号重放认得出「已经发过」、扣费顺序改完能读回来、
// 作废之后状态变成作废。
func TestUserPlanSubcommandRunsTheSameCode(t *testing.T) {
	db, userID, planKey := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)
	ctx := context.Background()

	ref := uniqueName("user-ops-grant")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	grantArgs := []string{"plan", "grant", user, planKey, "--ref", ref, "--expires-at", expires}

	_, stderr := mustStream(t, func() error { return runUser(grantArgs) })
	if !strings.Contains(stderr, "已发放订阅") {
		t.Fatalf("新路径的发放回显不对：%q", stderr)
	}
	if strings.Contains(stderr, "提示：") {
		t.Errorf("新路径不该打「旧路径搬到哪儿」的提示：%q", stderr)
	}

	// 同一个单号重放：幂等键认得出，说明两次走的是同一条写入路径。
	_, stderr = mustStream(t, func() error { return runUser(grantArgs) })
	if !strings.Contains(stderr, "已经发过") {
		t.Errorf("重放没有认出「已经发过」：%q", stderr)
	}

	var upID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM user_plans WHERE ref = $1`, ref).Scan(&upID); err != nil {
		t.Fatalf("读订阅 id 失败: %v", err)
	}

	stdout, _ := mustStream(t, func() error { return runUser([]string{"plan", "list", user}) })
	if !strings.Contains(stdout, planKey) {
		t.Errorf("user plan list 里没有 %q：\n%s", planKey, stdout)
	}
	// 与旧写法逐字相同：同一份清单、同一个渲染路径。
	legacy, _ := mustStream(t, func() error {
		return runPlan([]string{"subscriptions", "--user", user})
	})
	if stdout != legacy {
		t.Errorf("user plan list 与 plan subscriptions 的输出不同：\n新：%q\n旧：%q", stdout, legacy)
	}

	mustStream(t, func() error {
		return runUser([]string{"plan", "order", "set", user, "--order", "balance_first"})
	})
	stdout, _ = mustStream(t, func() error {
		return runUser([]string{"plan", "order", "show", user})
	})
	if !strings.Contains(stdout, "balance_first") {
		t.Errorf("扣费顺序没改到：\n%s", stdout)
	}

	_, stderr = mustStream(t, func() error {
		return runUser([]string{
			"plan", "void", strconv.FormatInt(upID, 10), "--ref", uniqueName("user-ops-void"),
		})
	})
	if !strings.Contains(stderr, "已作废") {
		t.Errorf("作废的回显不对：%q", stderr)
	}
}

// TestUserWalletKeyUsageShareTheOldOutput 钉住读类命令的输出逐字相同。
//
// 三条读命令各自对应旧写法的一个分支：`user wallet show` ↔ `wallet list --user`、
// `user key list` ↔ `apikey list --user`、`user usage events|requests` ↔
// `usage … --user`。它们不各写一份渲染，所以这里比的是整段 stdout，不是某一栏。
func TestUserWalletKeyUsageShareTheOldOutput(t *testing.T) {
	_, userID, _ := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)

	mustStream(t, func() error { return runWallet([]string{"create", "--user", user}) })
	mustStream(t, func() error {
		return runUser([]string{
			"wallet", "credit", user, "--amount", "3", "--ref", uniqueName("user-ops-credit"),
		})
	})

	// 写入真的落到了同一个状态上：余额是 3。
	stdout, _ := mustStream(t, func() error { return runUser([]string{"wallet", "show", user}) })
	if !strings.Contains(stdout, "3") {
		t.Errorf("user wallet show 里看不到刚入账的 3：\n%s", stdout)
	}
	legacy, _ := mustStream(t, func() error {
		return runWallet([]string{"list", "--user", user})
	})
	if stdout != legacy {
		t.Errorf("user wallet show 与 wallet list --user 的输出不同：\n新：%q\n旧：%q", stdout, legacy)
	}

	created, _ := mustStream(t, func() error { return runUser([]string{"key", "create", user}) })
	if strings.TrimSpace(created) == "" || strings.Contains(strings.TrimSpace(created), "\n") {
		t.Fatalf("user key create 的 stdout 该只有一行的明文 key，实际是 %q", created)
	}
	stdout, _ = mustStream(t, func() error { return runUser([]string{"key", "list", user}) })
	legacy, _ = mustStream(t, func() error {
		return runAPIKey([]string{"list", "--user", user})
	})
	if stdout != legacy {
		t.Errorf("user key list 与 apikey list --user 的输出不同：\n新：%q\n旧：%q", stdout, legacy)
	}

	for _, action := range []string{"events", "requests"} {
		stdout, _ = mustStream(t, func() error { return runUser([]string{"usage", action, user}) })
		legacy, _ = mustStream(t, func() error {
			return runUsage([]string{action, "--user", user})
		})
		if stdout != legacy {
			t.Errorf("user usage %s 与 usage %s --user 的输出不同：\n新：%q\n旧：%q",
				action, action, stdout, legacy)
		}
	}
}

// TestUserKeyPlansSubcommand 钉住 Key 的额度包名单落在 user key plans 下。
func TestUserKeyPlansSubcommand(t *testing.T) {
	_, userID, planKey := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)

	ref := uniqueName("user-ops-plans-grant")
	mustStream(t, func() error {
		return runUser([]string{
			"plan", "grant", user, planKey, "--ref", ref,
			"--expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	var upID int64
	if err := testDB(t).QueryRowContext(context.Background(),
		`SELECT id FROM user_plans WHERE ref = $1`, ref).Scan(&upID); err != nil {
		t.Fatalf("读订阅 id 失败: %v", err)
	}

	created, _ := mustStream(t, func() error { return runUser([]string{"key", "create", user}) })
	if strings.TrimSpace(created) == "" || strings.Contains(strings.TrimSpace(created), "\n") {
		t.Fatalf("user key create 的 stdout 是明文 key（不是 ID）：%q", created)
	}
	// 名单要的是 Key 的 ID，而 create 交出来的是明文。从库里读回刚建的那一行：
	// 它是这个用户最新的一把。
	var keyRowID int64
	if err := testDB(t).QueryRowContext(context.Background(),
		`SELECT id FROM api_keys WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, userID).
		Scan(&keyRowID); err != nil {
		t.Fatalf("读 API Key id 失败: %v", err)
	}
	keyArg := strconv.FormatInt(keyRowID, 10)

	_, stderr := mustStream(t, func() error {
		return runUser([]string{"key", "plans", "set", keyArg, "--plans", strconv.FormatInt(upID, 10)})
	})
	if strings.TrimSpace(stderr) == "" {
		t.Errorf("user key plans set 没有回显")
	}
	newOut, _ := mustStream(t, func() error {
		return runUser([]string{"key", "plans", "show", keyArg})
	})
	oldOut, _ := mustStream(t, func() error {
		return runPlan([]string{"key", "show", "--id", keyArg})
	})
	if newOut != oldOut {
		t.Errorf("user key plans show 与 plan key show 的输出不同：\n新：%q\n旧：%q", newOut, oldOut)
	}
	if !strings.Contains(newOut, planKey) {
		t.Errorf("名单里没有刚发放的包 %q：\n%s", planKey, newOut)
	}
}

// TestLegacyUserPathsStayAndPointToNewPath 是旧路径那一半。
//
// 每条旧写法都要：**真的做成那件事**（不是被当成未知子命令），并且在 stderr 上
// 打出一行指向新路径的提示。只钉前者，搬家就变成了「悄悄改道」；只钉后者，提示
// 可能指向一条跑不通的路。
//
// 成功与失败都要有：提示说的是「这条写法搬去哪了」，与这一次跑没跑成无关，
// 而失败恰恰是最需要知道的时候（见 runLegacyUserPath 的注释）。只在成功路径上
// 断言，实现改成「失败即静默」也照样绿——那正是这一条用例先前漏掉的那一格。
func TestLegacyUserPathsStayAndPointToNewPath(t *testing.T) {
	_, userID, planKey := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)

	mustStream(t, func() error { return runWallet([]string{"create", "--user", user}) })

	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	// 一定不存在的行：失败路径要的是「这条命令真的失败了」，不能碰巧命中
	// 同包别的用例建出来的数据（共享库里的 id 会随运行顺序变化）。
	const missingID = "9223372036854775807"

	cases := []struct {
		name string
		run  func() error
		want string // 提示里必须出现的新路径
		// wantErr 表示这条旧写法本来就该失败（数据不存在、取值不合法之类）。
		wantErr bool
	}{
		{
			name: "plan grant",
			run: func() error {
				return runPlan([]string{
					"grant", "--user", user, "--plan", planKey,
					"--ref", uniqueName("legacy-grant"), "--expires-at", expires,
				})
			},
			want: "larkaxctl user plan grant",
		},
		{
			name: "plan grant 套餐不存在",
			run: func() error {
				return runPlan([]string{
					"grant", "--user", user, "--plan", "no-such-plan",
					"--ref", uniqueName("legacy-grant-bad"), "--expires-at", expires,
				})
			},
			want:    "larkaxctl user plan grant",
			wantErr: true,
		},
		{
			name: "plan subscriptions",
			run:  func() error { return runPlan([]string{"subscriptions", "--user", user}) },
			want: "larkaxctl user plan list",
		},
		{
			name: "plan order show",
			run:  func() error { return runPlan([]string{"order", "show", "--user", user}) },
			want: "larkaxctl user plan order",
		},
		{
			name: "plan order set",
			run: func() error {
				return runPlan([]string{"order", "set", "--user", user, "--clear"})
			},
			want: "larkaxctl user plan order",
		},
		{
			// 提示要先于失败出现：它说的是「这条写法搬去哪了」，与结果无关。
			// 这一格就是判据——实现若改成「失败即静默」，它立刻变红。
			name:    "plan key show",
			run:     func() error { return runPlan([]string{"key", "show", "--id", missingID}) },
			want:    "larkaxctl user key plans",
			wantErr: true,
		},
		{
			name: "wallet credit",
			run: func() error {
				return runWallet([]string{
					"credit", "--user", user, "--amount", "1",
					"--ref", uniqueName("legacy-credit"),
				})
			},
			want: "larkaxctl user wallet credit",
		},
		{
			name: "wallet credit 用户不存在",
			run: func() error {
				return runWallet([]string{
					"credit", "--user", "no-such-user", "--amount", "1",
					"--ref", uniqueName("legacy-credit-bad"),
				})
			},
			want:    "larkaxctl user wallet credit",
			wantErr: true,
		},
		{
			name: "wallet adjust",
			run: func() error {
				return runWallet([]string{
					"adjust", "--user", user, "--amount", "1",
					"--memo", "旧路径调账", "--ref", uniqueName("legacy-adjust"),
					"--by", user,
				})
			},
			want: "larkaxctl user wallet adjust",
		},
		{
			name: "wallet list --user",
			run:  func() error { return runWallet([]string{"list", "--user", user}) },
			want: "larkaxctl user wallet show",
		},
		{
			name: "apikey create",
			run:  func() error { return runAPIKey([]string{"create", "--user", user}) },
			want: "larkaxctl user key create",
		},
		{
			name: "apikey create 用户不存在",
			run: func() error {
				return runAPIKey([]string{"create", "--user", "no-such-user"})
			},
			want:    "larkaxctl user key create",
			wantErr: true,
		},
		{
			name: "apikey list --user",
			run:  func() error { return runAPIKey([]string{"list", "--user", user}) },
			want: "larkaxctl user key list",
		},
		{
			name: "usage events --user",
			run:  func() error { return runUsage([]string{"events", "--user", user}) },
			want: "larkaxctl user usage events",
		},
		{
			name: "usage requests --user",
			run:  func() error { return runUsage([]string{"requests", "--user", user}) },
			want: "larkaxctl user usage requests",
		},
		{
			name:    "usage events 用户不存在",
			run:     func() error { return runUsage([]string{"events", "--user", "no-such-user"}) },
			want:    "larkaxctl user usage events",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runStream(t, tc.run)
			if tc.wantErr && err == nil {
				t.Fatalf("这条旧写法本来就该失败，却跑成了：\nstdout:\n%s", stdout)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("旧路径跑不通了：%v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
			}
			if !strings.Contains(stderr, "提示：") {
				t.Errorf("旧路径没有提示：\nstderr:\n%s", stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("提示里没有指向新路径 %q：\nstderr:\n%s", tc.want, stderr)
			}
			// 提示只在 stderr：混进 stdout 就把「能被脚本接走的值」那一档弄脏了。
			if strings.Contains(stdout, "提示：") {
				t.Errorf("提示混进了 stdout：\n%s", stdout)
			}
		})
	}
}

// TestLegacyUserPathsStayQuietOnUsageErrors 钉住提示不打在参数错上。
//
// 退出码 2 说明命令本身还没写成形，此时 stderr 上已经是一屏用法；再补一句
// 「这条写法搬去哪了」会把「先把参数写对」挤到后面。这一格与上一条一起，
// 划出提示的边界：参数合法就说话，无论成败；参数不合法就只说参数。
func TestLegacyUserPathsStayQuietOnUsageErrors(t *testing.T) {
	// 这些错都在连库之前判出来，不需要 DSN。
	t.Setenv("LARKAX_DB_DSN", "")

	cases := []struct {
		name string
		run  func() error
	}{
		{"plan grant 缺 --ref", func() error {
			return runPlan([]string{"grant", "--user", "alice", "--plan", "p",
				"--expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		}},
		{"plan key show 缺 --id", func() error { return runPlan([]string{"key", "show"}) }},
		{"wallet credit 缺 --amount", func() error {
			return runWallet([]string{"credit", "--user", "alice", "--ref", "r"})
		}},
		{"wallet credit 金额不是数", func() error {
			return runWallet([]string{"credit", "--user", "alice", "--amount", "abc", "--ref", "r"})
		}},
		{"apikey list 多给一个位置参数", func() error {
			return runAPIKey([]string{"list", "--user", "alice", "extra"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			_, stderr := captureStreams(t, func() { err = tc.run() })
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Fatalf("应当是参数错（%d），得到 %d（%v）", cli.ExitUsage, got, err)
			}
			if strings.Contains(stderr, "提示：") {
				t.Errorf("参数错时不该打搬家提示：\n%s", stderr)
			}
		})
	}
}

// TestLegacyUserPathsHintIsSuppressedByQuiet 钉住提示跟 -q 走。
//
// 它是附注那一档（见 docs/cli-output.md）：-q 说的是「没有人看着」，一句
// 「该改去哪儿」在那种场合是噪音；而结果本身照旧。
func TestLegacyUserPathsHintIsSuppressedByQuiet(t *testing.T) {
	_, userID, _ := newUserOpsFixture(t)
	user := strconv.FormatInt(userID, 10)

	saved := cli.QuietOutput
	cli.QuietOutput = true
	t.Cleanup(func() { cli.QuietOutput = saved })

	_, stderr, err := runStream(t, func() error {
		return runUsage([]string{"events", "--user", user})
	})
	if err != nil {
		t.Fatalf("旧路径跑不通了：%v", err)
	}
	if strings.Contains(stderr, "提示：") {
		t.Errorf("-q 之下提示该被压掉：\n%s", stderr)
	}
}

// TestUserSubgroupsAskForTheirOperand 钉住「缺宾语」报的是缺宾语。
//
// 位置参数是这一层的语法核心：漏掉它时报「缺少 <用户>」，而不是让下面那条命令
// 去说「缺少 --user」——后者会让人以为旗标名写错了。
func TestUserSubgroupsAskForTheirOperand(t *testing.T) {
	// 这些错都在连库之前判出来，不需要 DSN。
	t.Setenv("LARKAX_DB_DSN", "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"user plan grant 缺用户", []string{"plan", "grant"}, "<用户>"},
		{"user plan grant 缺套餐", []string{"plan", "grant", "alice"}, "<套餐>"},
		{"user plan list 缺用户", []string{"plan", "list"}, "<用户>"},
		{"user plan void 缺订阅", []string{"plan", "void"}, "<订阅 ID>"},
		{"user plan order show 缺用户", []string{"plan", "order", "show"}, "<用户>"},
		{"user wallet show 缺用户", []string{"wallet", "show"}, "<用户>"},
		{"user wallet credit 缺用户", []string{"wallet", "credit"}, "<用户>"},
		{"user key list 缺用户", []string{"key", "list"}, "<用户>"},
		{"user key plans show 缺 Key", []string{"key", "plans", "show"}, "<API Key ID>"},
		{"user usage events 缺用户", []string{"usage", "events"}, "<用户>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			_, stderr := captureStreams(t, func() { err = runUser(tc.args) })
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("缺宾语是参数错（%d），得到 %d（%v）", cli.ExitUsage, got, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里没有点名 %q：%v", tc.want, err)
			}
			// 已经会用这条命令的人漏了一个宾语，不该再糊一屏用法。
			if strings.Contains(stderr, "用法：") {
				t.Errorf("缺宾语不该打整段用法：\n%s", stderr)
			}
		})
	}
}

// TestUserHelpListsSubgroups 钉住 `user -h` 把四个子组列出来。
//
// 子组不进根帮助页（根页面只到一级命令），所以 `user -h` 是它们唯一的门面：
// 这里少了哪一组，那一组就等于只能靠猜。
func TestUserHelpListsSubgroups(t *testing.T) {
	for _, group := range []string{"plan", "wallet", "key", "usage"} {
		if !strings.Contains(userShortUsage, "user "+group) {
			t.Errorf("user 的短版没有列出子组 %q", group)
		}
		if !strings.Contains(userUsage, "user "+group) {
			t.Errorf("user 的长版没有列出子组 %q", group)
		}
	}
}

// TestPlanHelpKeepsOnlyProductDefinitions 钉住 `plan -h` 只剩商品定义。
//
// 判据是**用法行**（以 `larkaxctl plan ` 开头的那几行），不是整份文本：长版末尾
// 要说明那几个动作搬去了哪儿，那里出现 grant / void 是应该的。搬走的动作如果还
// 留在用法行里，读的人会以为它们还该在这儿敲——而两套推荐做法里有一套是错的。
func TestPlanHelpKeepsOnlyProductDefinitions(t *testing.T) {
	usageLines := func(text string) []string {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "larkaxctl plan ") {
				lines = append(lines, line)
			}
		}
		return lines
	}

	for _, gone := range []string{"grant", "void", "subscriptions", "plan key", "plan order"} {
		for what, text := range map[string]string{"短版": planShortUsage, "长版": planUsage} {
			for _, line := range usageLines(text) {
				if strings.Contains(line, gone) {
					t.Errorf("plan 的%s用法行里还有搬走的动作 %q：%q", what, gone, line)
				}
			}
		}
	}
	for _, kept := range []string{"plan add", "plan list", "plan get", "plan enable"} {
		if !strings.Contains(planShortUsage, kept) {
			t.Errorf("plan 的短版里没有商品定义 %q", kept)
		}
	}
}
