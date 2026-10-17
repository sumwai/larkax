package main

import (
	"context"
	"database/sql"
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

// maskID 把 id 作为一个**独立的数**从 s 里抹掉：`id=7` 与单独一行的 `7` 都要
// 抹，而 `70` 里的那个 7 不能动——不然抹出来的两份文本会「碰巧一样」。
func maskID(s string, id int64) string {
	if id == 0 {
		return s
	}
	return regexp.MustCompile(`\b`+strconv.FormatInt(id, 10)+`\b`).ReplaceAllString(s, "<ID>")
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
	// 用旧写法建出来：这一步证明 `model add` 真的能建东西。
	if r := runOnce(t, runModel, []string{"add", "--model", name, "--display", "展示名"}); r.err != nil {
		t.Fatalf("model add 失败: %v", r.err)
	}

	t.Run("add 等同于 create", func(t *testing.T) {
		// 两支都落在「这个名字已经有了」那一档。
		assertIdentical(t,
			runOnce(t, runModel, []string{"add", "--model", name, "--display", "展示名"}),
			runOnce(t, runModel, []string{"create", "--model", name, "--display", "展示名"}))
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
		"--models", onlyModel,
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
				"--models", onlyModel,
			}),
			runOnce(t, runPrice, []string{
				"create", "--scope", "platform", "--name", sameName, "--expr", testExpr,
				"--models", onlyModel,
			}))
	})

	t.Run("update 的 --owner 等同于 --user", func(t *testing.T) {
		assertIdentical(t,
			runOnce(t, runPrice, []string{"update", "--id", idArg, "--owner", ownerRef}),
			runOnce(t, runPrice, []string{"update", "--id", idArg, "--user", ownerRef}))
	})

	t.Run("explain 的 --owner 等同于 --user", func(t *testing.T) {
		// 两次问的是**同一个**模型名，而且就是这一组建的那些规则唯一适用的
		// 那个名字：要比的是选项名，不是「库里有没有规则」。
		assertIdentical(t,
			runOnce(t, runPrice, []string{"explain", "--model", onlyModel, "--owner", ownerRef}),
			runOnce(t, runPrice, []string{"explain", "--model", onlyModel, "--user", ownerRef}))
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
	// --models 收窄到一个专属名字：兜底规则会适用于所有模型，而别的用例
	// 依赖「某个名字下一条规则都不适用」（见 TestPriceExplainNoRule）。
	created := runOnce(t, runPrice, []string{
		"create", "--scope", "platform", "--name", name, "--expr", testExpr,
		"--models", uniqueName("model"),
	})
	if created.err != nil {
		t.Fatalf("price create 失败: %v", created.err)
	}
	ruleID := ownID(created)
	idArg := strconv.FormatInt(ruleID, 10)

	// 旧形态：停用，再启用。
	if r := runOnce(t, runPrice, []string{"update", "--id", idArg, "--disable"}); r.err != nil {
		t.Fatalf("price update --disable 失败: %v", r.err)
	}
	if ruleEnabled(t, db, ruleID) {
		t.Error("旧形态 --disable 之后规则仍然启用")
	}
	if r := runOnce(t, runPrice, []string{"update", "--id", idArg, "--enable"}); r.err != nil {
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
