package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
)

// ── 旧名字仍然可用：统一命名那条 issue 的硬要求 ──────────────────────────
//
// 统一动词与选项名之后，旧写法**不许删**：已经在跑的脚本与部署流程靠它们。
// 而这件事没有任何编译期保障——把 `case "create", "add"` 收回成
// `case "create"`，除了「恰好有人用旧名字跑一次」之外，没有任何地方会响。
// 这一组用例就是那道屏障：每条改名都有一条，用**旧**写法跑同一条命令。
//
// # 怎么比
//
// 每条用例都是「同一件事，两种写法」：
//
//  1. 先用**旧写法**把它做成（add / open / --canonical …）：这一步证明旧名字
//     走的是那条真实的代码路径，而不是被静默忽略；
//  2. 再用旧写法与新写法各跑一次同样的事，两次落在**同一个状态**上；
//  3. 断言两次的 stdout、stderr 与错误逐字相同。
//
// 第 2 步的「同一个状态」是这么来的：需要唯一键的命令（模型名、别名），第 1 步
// 已经把它建出来了，于是第 2、3 次跑到的都是「已经有了」那一档——两支落在同一
// 档上，输出才比得了。不需要唯一键的命令（计价规则、渠道名都不唯一）两次都会
// 成功，于是各自建出一条新记录，两次之间**只允许**差那个 ID（见 assertIdentical）。
//
// 比三个流而不只是退出码：退出码只有几档，「旧名字被当成未知子命令」与
// 「旧名字走了新路径但少打了一行提示」会落在同一个码上。
//
// 这一组都连库：这些命令的行为有一半在写入路径里（唯一键撞不撞、状态改没改），
// 拿假库测出来的只是「我以为写入路径会怎么做」。

// commandRun 是一条子命令跑一次的结果。
type commandRun struct {
	stdout string
	stderr string
	err    error
}

// runOnce 跑一条子命令，把它的两个流与错误都接住。
func runOnce(t *testing.T, run func([]string) error, args []string) commandRun {
	t.Helper()

	var err error
	stdout, stderr := captureStreams(t, func() { err = run(args) })
	return commandRun{stdout: stdout, stderr: stderr, err: err}
}

// assertIdentical 断言两次运行的结果逐字相同。
//
// # 允许差的只有「它自己刚建出来的那条记录的 ID」
//
// 创建类的命令 stdout 上只有一个数，那正是它这次建出来的记录的 ID。两次运行
// 落不到同一条记录上——唯一键挡着同名的第二次创建，而没有唯一键的命令（计价
// 规则、渠道）两次会各建一条——所以这个数必然不同，比较前按**独立的数**把它
// 抹掉（见 maskID）。除了它，三个流里不许有任何差别。
//
// 读类与改类的命令 stdout 不是这个形状，于是什么都不抹。
func assertIdentical(t *testing.T, old, new commandRun) {
	t.Helper()

	oldID, newID := ownID(old), ownID(new)
	oldOut, newOut := maskID(old.stdout, oldID), maskID(new.stdout, newID)
	oldErrOut, newErrOut := maskID(old.stderr, oldID), maskID(new.stderr, newID)

	if oldOut != newOut {
		t.Errorf("stdout 不同：\n旧写法 %q\n新写法 %q", oldOut, newOut)
	}
	if oldErrOut != newErrOut {
		t.Errorf("stderr 不同：\n旧写法 %q\n新写法 %q", oldErrOut, newErrOut)
	}
	if errorText(old.err) != errorText(new.err) {
		t.Errorf("错误不同：\n旧写法 %q\n新写法 %q", errorText(old.err), errorText(new.err))
	}
}

// ownID 取出一次运行「自己建出来的那条记录」的 ID。
func ownID(r commandRun) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(r.stdout), 10, 64)
	if err != nil {
		// 不是「stdout 上只有一个数」那种命令，没有要抹的东西。
		return 0
	}
	return id
}

// maskID 把 id 抹成 `<ID>`：stdout 整个就是一个数时抹整行，其余只抹标了
// 「这是 id」的位置（`id=7`）。
//
// # 为什么不是「所有独立的数」
//
// 同一个数也可能出现在别的字段里：「这条规则没有覆盖 3 个计价项」。一条用例
// 自己建出来的 id 恰好是 3 时，把那句里的 3 也抹掉，两次运行的输出就会一处
// 抹、一处不抹，于是**假红**——整包跑时 id 大、撞不上，只用 `-run` 过滤跑
// 就现形（见 LESSONS.md 第 16 条：整包绿不等于这条用例在干活）。
//
// 带标签的位置不会被别的字段借用，所以只认它们。
func maskID(s string, id int64) string {
	if id == 0 {
		return s
	}
	num := strconv.FormatInt(id, 10)
	if strings.TrimSpace(s) == num {
		return strings.Replace(s, num, "<ID>", 1)
	}
	return regexp.MustCompile(`(?i)\bid\s*[=:]\s*`+num+`\b`).ReplaceAllString(s, "id=<ID>")
}

// errorText 把「没有错误」也变成一个可比较的值。
func errorText(err error) string {
	if err == nil {
		return "<无错误>"
	}
	return err.Error()
}

// TestLegacyModelVerbAndFlagAliases 钉住 model 的旧动词与旧选项名。
func TestLegacyModelVerbAndFlagAliases(t *testing.T) {
	testDB(t)
	setupEnv(t)

	name := uniqueName("model")
	// 用正式写法建出来：这一步证明 `model add <规范名>` 真的能建东西。
	if r := runOnce(t, runModel, []string{"add", name}); r.err != nil {
		t.Fatalf("model add 失败: %v", r.err)
	}

	t.Run("create 等同于 add", func(t *testing.T) {
		// 两支都落在「这个名字已经有了」那一档。
		assertIdentical(t,
			runOnce(t, runModel, []string{"create", "--model", name}),
			runOnce(t, runModel, []string{"add", name}))
	})

	t.Run("show 等同于 get", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runModel, []string{"show", "--model", name}),
			runOnce(t, runModel, []string{"get", "--model", name}))
	})

	t.Run("get 的 --canonical 等同于 --model", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runModel, []string{"get", "--canonical", name}),
			runOnce(t, runModel, []string{"get", "--model", name}))
	})

	t.Run("update 的 --canonical 等同于 --model", func(t *testing.T) {
		// 同一行上连改两次同样的值：两次输出因此逐字相同，而两次走的是两个
		// 不同的选项名。
		assertIdentical(t,
			runOnce(t, runModel, []string{"update", "--canonical", name, "--vendor", "厂商"}),
			runOnce(t, runModel, []string{"update", "--model", name, "--vendor", "厂商"}))
	})

	t.Run("delete 的 --canonical 等同于 --model", func(t *testing.T) {
		// delete 是软删还是硬删不在这条用例里；要比的是「同一个不存在的名字」
		// 给出一模一样的错误，所以先删一次，再比两支。
		if r := runOnce(t, runModel, []string{"delete", "--model", name}); r.err != nil {
			t.Fatalf("model delete 失败: %v", r.err)
		}
		assertIdentical(t,
			runOnce(t, runModel, []string{"delete", "--canonical", name}),
			runOnce(t, runModel, []string{"delete", "--model", name}))
	})
}

// TestLegacyUserVerbAndFlagAliases 钉住 user 的旧动词与旧选项名。
func TestLegacyUserVerbAndFlagAliases(t *testing.T) {
	testDB(t)
	setupEnv(t)

	name := uniqueName("user")
	// 用正式写法建出来：这一步证明 `user add <用户名>` 真的能建东西。
	if r := runOnce(t, runUser, []string{"add", name}); r.err != nil {
		t.Fatalf("user add 失败: %v", r.err)
	}

	t.Run("create --name 等同于 add", func(t *testing.T) {
		// 两支都落在「这个名字已经有了」那一档。
		assertIdentical(t,
			runOnce(t, runUser, []string{"create", "--name", name}),
			runOnce(t, runUser, []string{"add", name}))
	})
}

// TestLegacyPlanVerbAndFlagAliases 钉住 plan 的旧动词与旧选项名。
func TestLegacyPlanVerbAndFlagAliases(t *testing.T) {
	testDB(t)
	setupEnv(t)

	key := uniqueName("plan")
	fields := []string{"--name", "旧名对照包", "--unit", "amount", "--quota", "5"}
	// 用正式写法建出来：这一步证明 `plan add <key>` 真的能建东西。
	if r := runOnce(t, runPlan, append([]string{"add", key}, fields...)); r.err != nil {
		t.Fatalf("plan add 失败: %v", r.err)
	}

	t.Run("create --key 等同于 add", func(t *testing.T) {
		// 两支都落在「这个 key 已经有了」那一档。
		assertIdentical(t,
			runOnce(t, runPlan, append([]string{"create", "--key", key}, fields...)),
			runOnce(t, runPlan, append([]string{"add", key}, fields...)))
	})
}

// TestLegacyAliasVerbAndFlagAliases 钉住 alias 的旧动词与旧选项名。
func TestLegacyAliasVerbAndFlagAliases(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	if r := runOnce(t, runModel, []string{"add", "--model", canonical, "--display", "展示名"}); r.err != nil {
		t.Fatalf("model add 失败: %v", r.err)
	}
	upstream := uniqueName("upstream")
	// 用旧写法建出来：这一步证明 `alias add` 与 `--canonical` 真的能建东西。
	if r := runOnce(t, runAlias, []string{"add", "--canonical", canonical, "--upstream", upstream}); r.err != nil {
		t.Fatalf("alias add 失败: %v", r.err)
	}

	t.Run("add 等同于 create", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runAlias, []string{"add", "--model", canonical, "--upstream", upstream}),
			runOnce(t, runAlias, []string{"create", "--model", canonical, "--upstream", upstream}))
	})

	t.Run("list 的 --canonical 等同于 --model", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runAlias, []string{"list", "--canonical", canonical}),
			runOnce(t, runAlias, []string{"list", "--model", canonical}))
	})

	t.Run("resolve 的 --canonical 等同于 --model", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runAlias, []string{"resolve", "--canonical", canonical}),
			runOnce(t, runAlias, []string{"resolve", "--model", canonical}))
	})
}

// TestLegacyPriceVerbAndFlagAliases 钉住 price 的旧动词与旧选项名。
func TestLegacyPriceVerbAndFlagAliases(t *testing.T) {
	testDB(t)
	setupEnv(t)

	ownerID := createTestUser(t)
	ownerRef := strconv.FormatInt(ownerID, 10)
	// --by 必填（谁定的这条价），两次对照给**同一个人**：这一组比的是两个
	// 写法逐字相同，换一个人就比不出来了。
	by := createTestUserID(t)

	// 这一组建的规则全部**只适用于一个本用例专属的模型名**。
	//
	// 兜底规则（--models 留空）会适用于所有模型，包括别的用例问的那些名字——
	// 而「一条规则都不适用」本身是有用例钉着的（TestPriceExplainNoRule），
	// 库里多一条平台级兜底规则就会让它再也测不到。规则名不唯一但**适用范围**
	// 会被读到，所以隔离要落在 --models 上，不是落在名字上。
	onlyModel := uniqueName("model")

	// 用旧写法建一条 merchant 规则：这一步证明 `price add` 与 `--owner` 真的
	// 把东西建出来了。
	name := uniqueName("rule")
	created := runOnce(t, runPrice, []string{
		"add", "--scope", "merchant", "--name", name, "--expr", testExpr, "--owner", ownerRef,
		"--models", onlyModel, "--by", by,
	})
	if created.err != nil {
		t.Fatalf("price add 失败: %v", created.err)
	}
	idArg := strconv.FormatInt(ownID(created), 10)

	t.Run("add 等同于 create", func(t *testing.T) {
		// 规则名不唯一，两支都会建出一条新记录——两次之间只允许差那个新 ID。
		// 名字两支给**同一个**：这次要比的是选项名与动词，不是名字。
		sameName := uniqueName("rule")
		assertIdentical(t,
			runOnce(t, runPrice, []string{
				"add", "--scope", "platform", "--name", sameName, "--expr", testExpr,
				"--models", onlyModel, "--by", by,
			}),
			runOnce(t, runPrice, []string{
				"create", "--scope", "platform", "--name", sameName, "--expr", testExpr,
				"--models", onlyModel, "--by", by,
			}))
	})

	t.Run("update 的 --owner 等同于 --user", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runPrice, []string{"update", "--id", idArg, "--owner", ownerRef, "--by", by}),
			runOnce(t, runPrice, []string{"update", "--id", idArg, "--user", ownerRef, "--by", by}))
	})

	t.Run("explain 的 --owner 等同于 --user", func(t *testing.T) {
		// 两次问的是**同一个**模型名，而且就是这一组建的那些规则唯一适用的
		// 那个名字：要比的是选项名，不是「库里有没有规则」。
		//
		// --at 固定成一个远处的将来：不给它时，explain 打出来的「时刻」是跑那
		// 一瞬间的墙钟（截到秒），两次调用跨过秒的边界就不同了——那与要比的
		// 选项名毫无关系，却会让这条用例偶发失败。固定的时刻不改变选中哪条
		// 规则：这些规则都是「不过期」、生效自建立那一刻。
		const at = "2099-01-01T00:00:00Z"
		assertIdentical(t,
			runOnce(t, runPrice, []string{"explain", "--model", onlyModel, "--owner", ownerRef, "--at", at}),
			runOnce(t, runPrice, []string{"explain", "--model", onlyModel, "--user", ownerRef, "--at", at}))
	})
}

// TestLegacyPriceEnableFlagForm 钉住旧的 `update --enable / --disable` 仍然能用。
//
// # 为什么这一对比的是状态而不是输出
//
// 新写法说的是「计价规则 7 已启用。」，旧写法走的是 update、说的是「已更新计价
// 规则 7。」——两种说法都是真话，句式却不同，逐字比没有意义。这一对的硬要求是
// 「旧写法仍然能用，行为一致」，所以这里比的是它改出来的**状态**：两种写法跑完
// 之后，库里那条规则处于同一个状态。
func TestLegacyPriceEnableFlagForm(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("rule")
	by := createTestUserID(t)
	// --models 收窄到一个专属名字：兜底规则会适用于所有模型，而别的用例
	// 依赖「某个名字下一条规则都不适用」（见 TestPriceExplainNoRule）。
	created := runOnce(t, runPrice, []string{
		"create", "--scope", "platform", "--name", name, "--expr", testExpr,
		"--models", uniqueName("model"), "--by", by,
	})
	if created.err != nil {
		t.Fatalf("price create 失败: %v", created.err)
	}
	ruleID := ownID(created)
	idArg := strconv.FormatInt(ruleID, 10)

	// 旧形态：停用，再启用。
	if r := runOnce(t, runPrice, []string{
		"update", "--id", idArg, "--disable", "--by", by,
	}); r.err != nil {
		t.Fatalf("price update --disable 失败: %v", r.err)
	}
	if ruleEnabled(t, db, ruleID) {
		t.Error("旧形态 --disable 之后规则仍然启用")
	}
	if r := runOnce(t, runPrice, []string{
		"update", "--id", idArg, "--enable", "--by", by,
	}); r.err != nil {
		t.Fatalf("price update --enable 失败: %v", r.err)
	}
	if !ruleEnabled(t, db, ruleID) {
		t.Error("旧形态 --enable 之后规则仍然是停用的")
	}

	// 新形态：同一个动作，落在同一个状态上。
	if r := runOnce(t, runPrice, []string{"disable", idArg}); r.err != nil {
		t.Fatalf("price disable 失败: %v", r.err)
	}
	if ruleEnabled(t, db, ruleID) {
		t.Error("price disable 之后规则仍然启用")
	}
	if r := runOnce(t, runPrice, []string{"enable", idArg}); r.err != nil {
		t.Fatalf("price enable 失败: %v", r.err)
	}
	if !ruleEnabled(t, db, ruleID) {
		t.Error("price enable 之后规则仍然是停用的")
	}

	// 规则 ID 是位置参数：给错必须报参数错，而不是拿 0 去改一行。
	for _, args := range [][]string{
		{"enable"},
		{"enable", "abc"},
		{"enable", "0"},
		{"enable", "1", "2"},
	} {
		r := runOnce(t, runPrice, append([]string{"enable"}, args...))
		if r.err == nil {
			t.Errorf("price %v 应当报参数错", args)
			continue
		}
		if got := exitCode(r.err); got != exitUsage {
			t.Errorf("price %v 的退出码 = %d，期望参数错的 %d", args, got, exitUsage)
		}
	}
}

// ruleEnabled 读库里那条规则当前的启用状态。
func ruleEnabled(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()

	rules, err := pricing.NewAdmin(db).List(context.Background())
	if err != nil {
		t.Fatalf("读规则清单失败: %v", err)
	}
	for _, rule := range rules {
		if rule.ID == id {
			return rule.Enabled
		}
	}
	t.Fatalf("清单里没有规则 %d", id)
	return false
}

// TestLegacyWalletVerbAndFlagAliases 钉住 wallet 的旧动词与旧选项名。
func TestLegacyWalletVerbAndFlagAliases(t *testing.T) {
	testDB(t)
	setupEnv(t)

	userRef := strconv.FormatInt(createTestUser(t), 10)

	// 用旧写法开户：这一步证明 `wallet open` 真的把钱包开出来了。
	if r := runOnce(t, runWallet, []string{"open", "--user", userRef}); r.err != nil {
		t.Fatalf("wallet open 失败: %v", r.err)
	}

	t.Run("open 等同于 create", func(t *testing.T) {
		// 钱包是按 (用户, 资产) 幂等的，两支都落在「已经有了」那一档。
		assertIdentical(t,
			runOnce(t, runWallet, []string{"open", "--user", userRef}),
			runOnce(t, runWallet, []string{"create", "--user", userRef}))
	})

	t.Run("--order 等同于 --idempotency-key", func(t *testing.T) {
		key := uniqueName("order")
		// 用旧写法入账一次：这一步证明旧选项名真的算进了幂等键（重放那次会
		// 认出来）。
		if r := runOnce(t, runWallet, []string{
			"credit", "--user", userRef, "--amount", "1", "--order", key,
		}); r.err != nil {
			t.Fatalf("wallet credit --order 失败: %v", r.err)
		}
		// 两支都落在重放那一档，交易号由键算出来，因此逐字相同。
		assertIdentical(t,
			runOnce(t, runWallet, []string{
				"credit", "--user", userRef, "--amount", "1", "--order", key,
			}),
			runOnce(t, runWallet, []string{
				"credit", "--user", userRef, "--amount", "1", "--idempotency-key", key,
			}))
	})
}

// TestLegacyChannelOwnerFlag 钉住 channel 的旧选项名 --owner。
func TestLegacyChannelOwnerFlag(t *testing.T) {
	testDB(t)
	setupEnv(t)

	ownerRef := strconv.FormatInt(createTestUser(t), 10)
	name := uniqueName("channel")

	// 用旧写法建一条：这一步证明 `--owner` 真的把渠道记在了那个用户名下。
	// 凭证从标准输入读，所以每跑一次都要重新灌一份。
	withStdin(t, testSecret)
	created := runOnce(t, runChannel, []string{
		"create", "--name", name, "--type", testChanType, "--base-url", testBaseURL,
		"--models", uniqueName("model"), "--credential-stdin", "--owner", ownerRef,
	})
	if created.err != nil {
		t.Fatalf("channel create --owner 失败: %v", created.err)
	}
	createdID := ownID(created)
	if createdID == 0 {
		t.Fatal("create 的 stdout 里没有渠道 ID")
	}

	// 渠道名不唯一，两支都会建出一条新渠道——两次之间只允许差那个新 ID。
	withStdin(t, testSecret)
	old := runOnce(t, runChannel, []string{
		"create", "--name", name, "--type", testChanType, "--base-url", testBaseURL,
		"--models", uniqueName("model"), "--credential-stdin", "--owner", ownerRef,
	})
	withStdin(t, testSecret)
	newer := runOnce(t, runChannel, []string{
		"create", "--name", name, "--type", testChanType, "--base-url", testBaseURL,
		"--models", uniqueName("model"), "--credential-stdin", "--user", ownerRef,
	})
	assertIdentical(t, old, newer)
}

// TestLegacySnapshotGetVerb 钉住 snapshot 的旧动词 show。
func TestLegacySnapshotGetVerb(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	userID, username := seedSnapshotUser(t, db)
	requestID := uniqueName("req")
	insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "request",
		skeleton: `{"model":"probe-model"}`, createdAt: time.Now().UTC(),
	})

	// 读一条快照会往 snapshot_access_log 写一行，但它不进 stdout/stderr——
	// 两次读的是同一行数据，两个流因此逐字相同。
	assertIdentical(t,
		runOnce(t, runSnapshot, []string{
			"show", requestID, "--actor", username, "--reason", "排查一次 500",
		}),
		runOnce(t, runSnapshot, []string{
			"get", requestID, "--actor", username, "--reason", "排查一次 500",
		}))
}

// ── #475：搬过家的那几条命令，旧路径仍然可用 ────────────────────────────
//
// 上面那一组测的是**同一个命令的两种写法**（动词与选项名改名），这一组测的是
// **同一条命令挂在两个地方**：收进资源域之后，旧的顶层路径必须继续能用，
// 因为已经在跑的脚本与部署流程靠它们。
//
// 比法一样：同一件事，两种写法，三个流逐字相同。

// TestLegacyReconcilePath 钉住 larkaxctl reconcile 与 larkaxctl ledger reconcile。
//
// # 为什么先跑一轮，以及为什么有两个字段要抹掉
//
// 对账每一轮都往 reconcile_runs 写一行，报告里的 run_id 因此必然不同；
// 「起点」是**上一轮**的水位，连跑两次也不一样。这两个字段是这一轮的身份，
// 不是行为——把它们算成差异，等于要求两次运行是同一次运行。
//
// 先跑一轮（暖场）是为了让被比较的两次都站在「前面已经有一轮跑完了」这个
// 起点上：第一轮与后面的轮次水位不同，而被比的范围由水位圈定。
//
// 其余部分逐字比：比对了几行、发现了哪几条不一致、结论是「没有不一致」还是
// 「发现 N 条」，都在这份报告里。对账默认不动手（settings 的
// reconcile.auto_repair 出厂是 false），所以两次看到的是同一份数据。
func TestLegacyReconcilePath(t *testing.T) {
	testDB(t)
	setupEnv(t)

	// 暖场：这一步同时证明 larkaxctl reconcile 真的把一轮跑完了。
	if r := runOnce(t, runReconcile, nil); !strings.Contains(r.stdout, "对账 run_id=") {
		t.Fatalf("reconcile 没有跑出一轮报告（err=%v）：\n%s", r.err, r.stdout)
	}

	assertSameReconcileRun(t,
		runOnce(t, runReconcile, nil),
		runOnce(t, runLedger, []string{"reconcile"}))
}

// reconcileIdentity 把一轮对账的「身份」抹掉：run_id、增量起点与日志时间戳。
//
// 只抹这三样。它们的共同点是「每一轮各自的值」：那一行是这一轮刚写进去的、
// 水位是上一轮留下的、时间戳是这一刻打出来的。把它们当成差异就永远比不出
// 东西来；其余每一个字段——比对了几行、每条不一致的用户与金额、结论那一句
// ——都要逐字对上。
func reconcileIdentity(s string) string {
	s = regexp.MustCompile(`run_id=\d+`).ReplaceAllString(s, "run_id=<N>")
	s = regexp.MustCompile(`起点=\S+`).ReplaceAllString(s, "起点=<T>")
	return regexp.MustCompile(`time=\S+`).ReplaceAllString(s, "time=<T>")
}

// assertSameReconcileRun 断言两轮对账除身份之外逐字相同。
func assertSameReconcileRun(t *testing.T, old, new commandRun) {
	t.Helper()

	oldOut, newOut := reconcileIdentity(old.stdout), reconcileIdentity(new.stdout)
	if oldOut != newOut {
		t.Errorf("stdout 不同：\n旧写法 %q\n新写法 %q", oldOut, newOut)
	}

	oldErrOut, newErrOut := reconcileIdentity(old.stderr), reconcileIdentity(new.stderr)
	if oldErrOut != newErrOut {
		t.Errorf("stderr 不同：\n旧写法 %q\n新写法 %q", oldErrOut, newErrOut)
	}

	// 发现不一致时退出码非零，那句话里也带着 run_id——两轮都可能以它收尾，
	// 所以它同样要比。
	oldErr, newErr := reconcileIdentity(errorText(old.err)), reconcileIdentity(errorText(new.err))
	if oldErr != newErr {
		t.Errorf("错误不同：\n旧写法 %q\n新写法 %q", oldErr, newErr)
	}
}

// TestLegacyRotatePath 钉住 larkaxctl rotate 与 larkaxctl secret rotate。
//
// 跑的是默认的干跑：它只读、不写库，所以两次看到的是同一份数据，连「扫过
// N 行」都一样，三个流可以逐字比。--apply 不在这里跑——它改写全库的凭证
// 密文，用例不该动它。
func TestLegacyRotatePath(t *testing.T) {
	testDB(t)
	setupEnv(t)

	assertIdentical(t,
		runOnce(t, runRotate, nil),
		runOnce(t, runSecret, []string{"rotate"}))
}

// TestLegacyIdempotencyPath 钉住 larkaxctl idempotency cleanup 与
// larkaxctl maintenance idempotency。
//
// 用 --dry-run 比：真删会改库，而这里要比的是路径，不是留存期算得对不对
// （那是 idempotency_test.go 那一组的事）。干跑只数一遍，两次落在同一个数上。
func TestLegacyIdempotencyPath(t *testing.T) {
	testDB(t)
	setupEnv(t)

	assertIdentical(t,
		runOnce(t, runIdempotency, []string{"cleanup", "--dry-run"}),
		runOnce(t, runMaintenance, []string{"idempotency", "--dry-run"}))
}

// TestLegacyCalendarPath 钉住 larkaxctl calendar import 与
// larkaxctl price calendar import。
//
// # 为什么要先灌一次
//
// 导入的结果里最后一段是「替换原有 N 行」：第一次灌某一年时 N=0，之后再灌是
// 这一年的天数。直接比新旧两次，差的是「库里本来有没有那一年的数据」，不是
// 路径——所以先用旧路径灌一次，被比的两次就都落在「这一年已经有了」那一档。
func TestLegacyCalendarPath(t *testing.T) {
	testDB(t)
	setupEnv(t)

	path := writeCalendarFile(t, 2031)
	if r := runOnce(t, runCalendar, []string{"import", path}); r.err != nil {
		t.Fatalf("calendar import 失败: %v", r.err)
	}

	assertIdentical(t,
		runOnce(t, runCalendar, []string{"import", path}),
		runOnce(t, runPrice, []string{"calendar", "import", path}))
}

// writeCalendarFile 造一份覆盖整整一年的日历数据文件，返回它的路径。
//
// 每一天都要有判定是导入的硬要求（少一天就报错），所以按年份生成而不是手写：
// 手写的那份会在碰上闰年时少一天，而那一天正好是 12 月 31 日，看起来像导入
// 本身出了问题。
func writeCalendarFile(t *testing.T, year int) string {
	t.Helper()

	days := make([]string, 0, 366)
	for d := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC); d.Year() == year; d = d.AddDate(0, 0, 1) {
		weekend := d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
		days = append(days, fmt.Sprintf(`{"date":%q,"off_day":%t}`, d.Format("2006-01-02"), weekend))
	}
	body := fmt.Sprintf(`{"region":"CN","year":%d,"status":"synced","source":"manual","days":[%s]}`,
		year, strings.Join(days, ","))

	path := filepath.Join(t.TempDir(), fmt.Sprintf("cn-%d.json", year))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写日历数据文件失败: %v", err)
	}
	return path
}
