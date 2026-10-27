package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// 这一组是「提示也一样」那条规格的判据：错误只在用户输入出错时出现，提示也只在
// 有条件可提示时才出现。规格在 docs/cli-output.md 的「提示与警告」一节，落成
// 可执行的判断是四层：
//
//  1. **门**：每条命令的成功路径上，两个流里都没有「提示：」「注意：」「警告：」；
//  2. **门**：写成功的回显只出现在一个流上（这一仓选的是 stderr），stdout 只留
//     能被脚本接走的值；
//  3. **用例**：同一件事只报一次（settings 的配置服务加载失败那条）；
//  4. **用例**：无条件的絮语没了（rotate --apply 的备份提醒，现在跟 -v 走）。
//
// 前两层要连库：命令的成功路径有一半在库里（要有一条渠道可改、有一条快照可读），
// 拿假库测出来的只是「我以为它会怎么打」。所以这一组都在集成档里。

// promptPrefixes 是提示性文字的前缀。
//
// 这三样是「这一行是附注」的标记，而附注只在条件成立时出现（见
// docs/cli-output.md 的「提示与警告」）。成功路径是「没有任何特殊条件」的那一次
// 运行，所以这三样在它上面一个都不该有。
var promptPrefixes = []string{"提示：", "注意：", "警告："}

// ruleExpr 是一条最小可用的计价规则表达式。
//
// 表达式是一份**单价表**（见 docs/development.md 的「配一条计价规则」）：上游报了
// 哪一项用量，规则里就得有哪一项的单价。这一组要的只是「有一条生效的规则」，
// 所以给两项够了。
const ruleExpr = `{"prompt": 0.0000015, "completion": 0.0000075}`

// withVerbose 拨动详细模式开关，用完还原。
//
// 这一组里有两条要「同一件事在默认档与 -v 档下两副样子」。不还原会让后面的用例
// 跟着跑在 -v 上，而那种连带失效看起来像别处坏了，最难查。
func withVerbose(t *testing.T, on bool) {
	t.Helper()
	saved := cli.VerboseOutput
	cli.VerboseOutput = on
	t.Cleanup(func() { cli.VerboseOutput = saved })
}

// ── 门一：成功路径上不出现提示 ──────────────────────────────────────────

// successPathCase 是一条命令的**普通成功路径**。
//
// 「普通」是这一层的关键词：不带 --setting、采集关着、没有另一处轮换在跑——
// 那些条件成立时才出现的附注是允许的（它们走 stderr，且只在条件成立时打），
// 这一层抓的是「无条件就打」的那一类。
type successPathCase struct {
	// command 是顶层命令名，与 commandGroups 里的 name 一致：覆盖判据从注册表
	// 推出来（见下面），不在这里手抄一份。
	command string
	// what 是这一条跑的是哪个子命令，报错时用。
	what string
	// run 跑一次这条命令的成功路径。
	run func() error
	// reportOnly 为真表示这条命令的「成功路径」本身就是给出一份不通过的体检
	// 结论：那时它返回 reportedErr（报告已经打出来了，见 cli 包的 exit.go）。
	reportOnly bool
	// allowed 是**白名单**：这条路径上允许出现的「像提示的话」，其余路径为空。
	//
	// 白名单只有两条（见 noteExceptions），它们写成陈述句，不带上面那三个前缀，
	// 所以前缀那一遍扫不到它们——在这里单独钉一条「它还在」，否则「清理提示」的
	// 人会顺手把它们一起删掉，而它们恰恰是规格点名要留的。
	allowed string
}

// TestSuccessPathsPrintNoPrompts 是门一。
//
// 判据两条，缺一条这个门就漏：
//
//  1. 每条命令的成功路径跑下来，两个流里都没有提示前缀；
//  2. 白名单里那两条**真的出现了**——它们必须留着（一次性明文 key 与空清单的
//     下一步）。把白名单当成「可以删掉的例外」会让下一次有人顺手扫掉它们，而那
//     正是这条门禁不该管的事。
func TestSuccessPathsPrintNoPrompts(t *testing.T) {
	fx := newStreamFixture(t)
	cases := successPathCases(t, fx)

	covered := map[string]bool{}
	allowedSeen := map[string]bool{}

	for _, tc := range cases {
		t.Run(tc.command+"/"+tc.what, func(t *testing.T) {
			stdout, stderr, err := runStream(t, tc.run)
			// 失败分两类：真失败，与「体检不通过」——后者的结论已经打在那份
			// 报告里了，非零退出是设计的一部分（见 cli 包的 exit.go 的 reportedErr）。
			// 写成具名的一步：摊成一个德摩根表达式就没法读了。
			concluded := tc.reportOnly && cli.ReportedOnly(err)
			if err != nil && !concluded {
				t.Fatalf("这条命令没走成功路径：%v\nstderr:\n%s", err, stderr)
			}

			combined := stdout + stderr
			for _, prefix := range promptPrefixes {
				if i := strings.Index(combined, prefix); i >= 0 {
					t.Errorf("成功路径上出现了 %q：\n%s", prefix, offendingLine(combined, i))
				}
			}
			if tc.allowed == "" {
				return
			}
			if !strings.Contains(combined, tc.allowed) {
				t.Errorf("白名单里那一句不见了，它必须留着：%q\nstdout:\n%s\nstderr:\n%s",
					tc.allowed, stdout, stderr)
			}
		})
		covered[tc.command] = true
		if tc.allowed != "" {
			allowedSeen[tc.command+"|"+tc.allowed] = true
		}
	}

	// 覆盖判据不手抄：从注册表（commandGroups）取。加一条顶层命令而这张表没跟上
	// 时当场红——那正是「提示又回到某条命令的成功路径上」会溜进来的位置。
	//
	// 旧路径（legacyCommands：reconcile / rotate / idempotency / calendar）不在
	// 这里逐条要：它们接的是同一批处理函数（见 main.go 的 legacyCommands），而
	// 「两条路径输出逐字相同」另有用例钉着（legacy_aliases_test.go）。
	for _, group := range commandGroups {
		for _, c := range group.commands {
			if !covered[c.name] {
				t.Errorf("顶层命令 %s 没有成功路径用例：新加一条命令时它也要进这张表", c.name)
			}
		}
	}

	for _, ex := range noteExceptions {
		if !allowedSeen[ex.command+"|"+ex.contains] {
			t.Errorf("白名单里那条（%s 的 %q）没有出现：它必须留着——%s",
				ex.command, ex.contains, ex.why)
		}
	}
}

// noteExceptions 是这条门禁**仅有的**两条白名单，各附理由。
//
// 两条都不是可省的解释，而是使用者当场必须知道的东西：
//
//   - 一次性明文 key：不提醒就等于让人把它丢掉，而库里只有哈希、再也取不回来；
//   - 空清单的「暂无 …」：不说这一句，空清单与「命令没跑起来」在安静的终端里
//     长得一样（LESSONS.md 第 16 条）。
//
// 除了这两条，默认输出里不该再有任何「像提示的话」：新加一条时先问「它在任何
// 一次成功运行里都会出现吗」，会，就不该在默认输出里（那种话属于 -v，或者属于
// --help 的用法文本）。
var noteExceptions = []struct {
	command  string
	contains string
	why      string
}{
	{
		command:  "apikey",
		contains: "明文只打印这一次",
		why:      "一次性明文 key：库里只有哈希，看不到它就再也取不回来",
	},
	{
		command:  "apikey",
		contains: "暂无 API Key",
		why:      "空清单的下一步：不说「暂无」，空清单与命令没跑起来在安静的终端里长得一样",
	},
}

// successPathCases 是门一那张表：每条顶层命令至少一条。
//
// 每条命令挑的是**最容易出现附注的那条路径**：写（channel / model / alias /
// price 的 update）、列清单、快照、轮换、体检。破坏性的那三条（delete）不在这里
// ——它们的回显由门二看着。
func successPathCases(t *testing.T, fx *streamFixture) []successPathCase {
	t.Helper()
	id := func(v int64) string { return strconv.FormatInt(v, 10) }

	// 一条没有 key 的用户，用来跑出空清单那一句（白名单第二条）。
	emptyUserID := createTestUser(t)

	return []successPathCase{
		{command: "doctor", what: "doctor", reportOnly: true,
			run: func() error { return runDoctor(nil) }},
		{command: "version", what: "version",
			run: func() error { return runVersion(nil) }},
		{command: "completion", what: "bash",
			// 补全脚本只打到 stdout，不连库；走注册表里那个转发函数，
			// 顺带证明它接得上。
			run: func() error { return runCompletionCommand([]string{"bash"}) }},

		{command: "settings", what: "list",
			run: func() error { return runSettingsList(nil) }},
		{command: "settings", what: "set",
			run: func() error {
				return runSettingsSet([]string{
					"--key", "plugin.enabled", "--value", "true", "--by", id(fx.userID),
				})
			}},

		{command: "admin", what: "token create",
			// 明文那行是结果不是附注，与 apikey create 同一类：stdout 只有它，
			// 「只有这一次」那句说明在 stderr，且不带提示前缀。
			run: func() error { return runAdmin([]string{"token", "create"}) }},

		{command: "channel", what: "update",
			run: func() error {
				return runChannel([]string{"update", "--id", id(fx.channelID), "--priority", "7"})
			}},
		{command: "channel", what: "list",
			run: func() error { return runChannel([]string{"list"}) }},
		{command: "account", what: "list",
			run: func() error { return runAccount([]string{"list"}) }},

		{command: "model", what: "update",
			run: func() error {
				return runModel([]string{"update", "--model", fx.modelName, "--vendor", "夹具厂商"})
			}},
		{command: "model", what: "list",
			run: func() error { return runModel([]string{"list"}) }},

		{command: "alias", what: "update",
			run: func() error {
				return runAlias([]string{
					"update", "--id", id(fx.aliasID), "--upstream", uniqueName("stream-up2"),
				})
			}},
		{command: "alias", what: "resolve",
			run: func() error {
				return runAlias([]string{"resolve", "--model", fx.modelName})
			}},

		{command: "price", what: "update",
			run: func() error {
				return runPrice([]string{
					"update", "--id", id(fx.ruleID), "--priority", "4", "--by", id(fx.userID),
				})
			}},
		{command: "price", what: "list",
			run: func() error { return runPrice([]string{"list"}) }},

		{command: "upstream-cost", what: "create",
			run: func() error {
				return runUpstreamCost([]string{
					"create", "--channel", id(fx.channelID), "--expr", ruleExpr,
					"--models", fx.modelName,
				})
			}},
		{command: "upstream-cost", what: "list",
			run: func() error { return runUpstreamCost([]string{"list"}) }},

		// 用户级履约操作这一族走**新路径**：旧写法在成功路径上会打一句「搬到哪儿
		// 了」，而那正是这条门禁扫的提示前缀。这与旧路径不进这张表是同一条立场
		// （见上面覆盖判据那一段）：门禁管的是**正式写法**的成功路径。旧写法那一句
		// 由 user_ops_test.go 逐条钉着，不会因为这里改道而没人看着。
		{command: "user", what: "create",
			run: func() error {
				return runUser([]string{"create", "--name", uniqueName("stream-user")})
			}},

		{command: "apikey", what: "create",
			allowed: "明文只打印这一次",
			run: func() error {
				return runUser([]string{"key", "create", id(fx.userID)})
			}},
		{command: "apikey", what: "list 空清单",
			allowed: "暂无 API Key",
			run: func() error {
				return runUser([]string{"key", "list", id(emptyUserID)})
			}},

		{command: "wallet", what: "credit",
			run: func() error {
				return runUser([]string{
					"wallet", "credit", id(fx.userID), "--amount", "1",
					"--gateway", "manual", "--ref", uniqueName("stream-order"),
				})
			}},
		{command: "wallet", what: "adjust",
			run: func() error {
				return runUser([]string{
					"wallet", "adjust", id(fx.userID), "--amount", "1",
					"--memo", "夹具调账", "--ref", uniqueName("stream-adjust"),
					"--by", id(fx.userID),
				})
			}},

		// 每条一级命令自己那一层也各留一条：上面的 user 那几条走的是新路径，
		// 各命令的分发函数不该因此一条用例都没有。
		{command: "apikey", what: "get",
			run: func() error { return runAPIKey([]string{"get", "--id", id(fx.keyID)}) }},
		{command: "wallet", what: "create 已存在",
			run: func() error { return runWallet([]string{"create", "--user", id(fx.userID)}) }},
		{command: "plan", what: "list",
			run: func() error { return runPlan([]string{"list"}) }},
		{command: "usage", what: "events 全量",
			run: func() error { return runUsage([]string{"events", "--limit", "1"}) }},

		{command: "plan", what: "create + grant",
			run: func() error {
				key := uniqueName("stream-plan")
				if err := runPlan([]string{
					"create", "--key", key, "--name", "门禁额度包", "--unit", "amount",
					"--quota", "10",
				}); err != nil {
					return err
				}
				return runUser([]string{
					"plan", "grant", id(fx.userID), key,
					"--ref", uniqueName("stream-grant"),
					"--expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				})
			}},

		{command: "usage", what: "events 空清单",
			run: func() error {
				return runUser([]string{"usage", "events", id(emptyUserID)})
			}},
		{command: "usage", what: "requests 空清单",
			run: func() error {
				return runUser([]string{"usage", "requests", id(emptyUserID)})
			}},

		{command: "ledger", what: "balance --all",
			run: func() error { return runLedger([]string{"balance", "--all"}) }},
		{command: "ledger", what: "reconcile", reportOnly: true,
			run: func() error { return runLedger([]string{"reconcile"}) }},
		{command: "ledger", what: "runs",
			run: func() error { return runLedger([]string{"runs"}) }},

		{command: "migrate", what: "status",
			run: func() error { return runMigrate([]string{"status"}) }},

		{command: "partition", what: "ensure",
			run: func() error { return runPartition([]string{"ensure", "--months", "2"}) }},

		{command: "secret", what: "rotate 干跑",
			run: func() error { return runSecret([]string{"rotate"}) }},
		{command: "secret", what: "rotate --apply",
			run: func() error { return runSecret([]string{"rotate", "--apply"}) }},
		{command: "secret", what: "rotate --verify", reportOnly: true,
			run: func() error { return runSecret([]string{"rotate", "--verify"}) }},

		{command: "maintenance", what: "idempotency --dry-run",
			run: func() error { return runMaintenance([]string{"idempotency", "--dry-run"}) }},
		{command: "maintenance", what: "session --dry-run",
			run: func() error { return runMaintenance([]string{"session", "--dry-run"}) }},

		{command: "snapshot", what: "allowlist",
			run: func() error { return runSnapshot([]string{"allowlist"}) }},
		{command: "snapshot", what: "get",
			run: func() error {
				return runSnapshot([]string{
					"get", fx.requestID, "--actor", id(fx.userID), "--reason", "门禁用例",
				})
			}},
		{command: "snapshot", what: "access-log",
			run: func() error {
				return runSnapshot([]string{"access-log", "--request-id", fx.requestID})
			}},
		{command: "snapshot", what: "redact",
			run: func() error {
				withOSStdin(t, `{"model":"`+fx.modelName+`","messages":[{"role":"user","content":"m"}]}`)
				return runSnapshot([]string{"redact", "--direction", "request"})
			}},

		{command: "consent", what: "grant",
			// 这条成功路径上 stderr 里有整段同意文案，而它不是附注：它是操作者
			// 要同意的那件事本身，写在 os.Stderr 上是为了让 -q 压不掉它（压掉就
			// 成了「没看见也同意过」）。门扫的是「提示：」那三个前缀，文案里没有。
			run: func() error {
				return runConsent([]string{
					"grant", "--user", id(fx.userID), "--scope", "single_request",
					"--request-id", fx.requestID, "--expires-at", rfc3339(2 * time.Hour),
					"--agree",
				})
			}},

		{command: "plugin", what: "list",
			run: func() error { return runPlugin([]string{"list"}) }},
		{command: "plugin", what: "publish",
			run: func() error {
				path := writePluginFile(t, "stream-publish.js",
					pluginSource(uniqueName("stream-plugin2"), "1.0.0", 0))
				return runPlugin([]string{"publish", path, "--by", id(fx.userID)})
			}},
	}
}

// createStreamCostRule 给门二那两条改库用例先建一条成本规则，返回它的 ID。
//
// 建的时候那一次回显（「已加入成本规则」）不在这条用例的判据里：它自己的回显
// 由 create 那一行看着。这里只借它拿到一个能改的 ID。
func createStreamCostRule(t *testing.T, fx *streamFixture) int64 {
	t.Helper()

	out, _ := mustStream(t, func() error {
		return runUpstreamCost([]string{
			"create", "--channel", strconv.FormatInt(fx.channelID, 10),
			"--expr", ruleExpr, "--models", fx.modelName,
		})
	})
	return idFromStdout(t, "成本规则", out)
}

// offendingLine 把命中位置那一行切出来，报错时只打这一行。
func offendingLine(s string, at int) string {
	start := strings.LastIndexByte(s[:at], '\n') + 1
	end := strings.IndexByte(s[at:], '\n')
	if end < 0 {
		return s[start:]
	}
	return s[start : at+end]
}

// ── 门二：写成功的回显只在一个流上 ──────────────────────────────────────

// stdoutKind 说这条写命令的 stdout 上该留下什么。
//
// 「能被脚本接走的值」是 stdout 唯一的住户（见 docs/cli-output.md 的那张表）：
// 新建对象的 ID、一次性明文 key。回显（「已创建…」「已更新…」）不是这种值——
// 它是给人看的确认，所以走 stderr。
type stdoutKind int

const (
	// stdoutEmpty：这条命令没有值要交出去，stdout 一个字节都不该有。
	stdoutEmpty stdoutKind = iota
	// stdoutID：stdout 恰好一行，是新建对象的 ID。
	stdoutID
	// stdoutSecret：stdout 恰好一行，是一次性明文 key。
	stdoutSecret
)

// writeEchoCase 是一条写命令。
type writeEchoCase struct {
	command string
	what    string
	// marker 是回显里**不含变量**的那一截（「已更新渠道」而不是「已更新渠道 3。」）。
	//
	// 用一截而不是整行：整行要把被测代码里的格式串抄第二遍，改文案时两处一起改
	// 才不红，而那时它已经不再看着什么了。这一截仍然能回答门二要问的那件事——
	// 「这句确认落在哪个流上」。
	marker string
	// stdout 说这条命令的 stdout 上该留下什么。
	stdout stdoutKind
	run    func(t *testing.T, fx *streamFixture) (string, string)
}

// TestWriteEchoesAppearOnlyOnStderr 是门二。
//
// 判据三条，缺一条这个门就漏：
//
//  1. 那句确认不在 stdout 上；
//  2. 它在 stderr 上恰好出现一次——「一次都不打」看起来比打错地方更像正常；
//  3. stdout 上只有能被接走的值（一行的 ID / 一次性明文 key），或者什么都没有。
//
// 为什么统一在 stderr：stdout 只放能被脚本接走的值，于是
// `larkaxctl channel update --id 3 | head -1` 的行为才是确定的——在那之前它会
// 拿到一句「已更新渠道 3。」，而那句话既不是值，也不是谁要接的东西。
//
// 这张表只收「写成功之后只回一句确认」的命令。wallet 的 create / credit /
// adjust 不在里面：它们打在 stdout 上的是**一份报告**（余额、分录、交易号），
// 属于表里「结果」那一档，不是要挪走的那类回显。`user update` 同理（stdout 上是
// 这次改了哪几列、从什么变成什么）。
func TestWriteEchoesAppearOnlyOnStderr(t *testing.T) {
	fx := newStreamFixture(t)
	id := func(v int64) string { return strconv.FormatInt(v, 10) }

	cases := []writeEchoCase{
		{command: "user", what: "create", marker: "已创建用户", stdout: stdoutID,
			run: func(t *testing.T, _ *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runUser([]string{"create", "--name", uniqueName("echo-user")})
				})
			}},

		// 走新路径（user key create）：旧写法在成功路径上会打一句「搬到哪儿了」，
		// 那是附注，不该混进这条用例要数的回显里。
		{command: "apikey", what: "create", marker: "已创建 API Key", stdout: stdoutSecret,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runUser([]string{"key", "create", id(fx.userID)})
				})
			}},
		{command: "apikey", what: "update", marker: "已改 API Key", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runAPIKey([]string{"update", "--id", id(fx.keyID), "--rpm", "60"})
				})
			}},

		{command: "settings", what: "set", marker: "已写入 settings.", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runSettingsSet([]string{
						"--key", "plugin.enabled", "--value", "true", "--by", id(fx.userID),
					})
				})
			}},

		{command: "channel", what: "create", marker: "已创建渠道", stdout: stdoutID,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				withStdin(t, testSecret)
				return mustStream(t, func() error {
					return runChannel([]string{
						"create", "--name", uniqueName("echo-channel"), "--type", testChanType,
						"--base-url", testBaseURL, "--models", fx.modelName, "--credential-stdin",
					})
				})
			}},
		{command: "channel", what: "update", marker: "已更新渠道", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runChannel([]string{"update", "--id", id(fx.channelID), "--priority", "5"})
				})
			}},
		{command: "channel", what: "disable", marker: "已停用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runChannel([]string{"disable", "--id", id(fx.channelID)})
				})
			}},
		{command: "channel", what: "enable", marker: "已启用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runChannel([]string{"enable", "--id", id(fx.channelID)})
				})
			}},
		{command: "channel", what: "delete", marker: "已删除渠道", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				withStdin(t, testSecret)
				created, _ := mustStream(t, func() error {
					return runChannel([]string{
						"create", "--name", uniqueName("echo-doomed-channel"), "--type", testChanType,
						"--base-url", testBaseURL, "--models", fx.modelName, "--credential-stdin",
					})
				})
				victim := idFromStdout(t, "渠道", created)
				return mustStream(t, func() error {
					return runChannel([]string{"delete", "--id", id(victim)})
				})
			}},
		{command: "channel", what: "restore", marker: "已恢复渠道", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				withStdin(t, testSecret)
				created, _ := mustStream(t, func() error {
					return runChannel([]string{
						"create", "--name", uniqueName("echo-revived-channel"), "--type", testChanType,
						"--base-url", testBaseURL, "--models", fx.modelName, "--credential-stdin",
					})
				})
				victim := idFromStdout(t, "渠道", created)
				mustStream(t, func() error {
					return runChannel([]string{"delete", "--id", id(victim)})
				})
				return mustStream(t, func() error {
					return runChannel([]string{"restore", "--id", id(victim)})
				})
			}},

		{command: "model", what: "create", marker: "已加入规范模型", stdout: stdoutID,
			run: func(t *testing.T, _ *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runModel([]string{
						"create", "--model", uniqueName("echo-model"), "--display", "回显夹具",
					})
				})
			}},
		{command: "model", what: "update", marker: "已更新规范模型", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runModel([]string{"update", "--model", fx.modelName, "--vendor", "回显厂商"})
				})
			}},
		{command: "model", what: "delete", marker: "已删除规范模型", stdout: stdoutEmpty,
			run: func(t *testing.T, _ *streamFixture) (string, string) {
				name := uniqueName("echo-doomed-model")
				mustStream(t, func() error {
					return runModel([]string{"create", "--model", name, "--display", "待删"})
				})
				return mustStream(t, func() error {
					return runModel([]string{"delete", "--model", name})
				})
			}},

		{command: "alias", what: "create", marker: "已加入别名", stdout: stdoutID,
			run: func(t *testing.T, _ *streamFixture) (string, string) {
				// 平台级的一条规范名只容得下一个上游名（换名字要配渠道级别名），
				// 所以这两条 create / delete 各自另起一个模型来挂。
				model := aliasHostModel(t)
				return mustStream(t, func() error {
					return runAlias([]string{
						"create", "--model", model, "--upstream", uniqueName("echo-upstream"),
					})
				})
			}},
		{command: "alias", what: "update", marker: "已更新别名", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runAlias([]string{
						"update", "--id", id(fx.aliasID), "--upstream", uniqueName("echo-up2"),
					})
				})
			}},
		{command: "alias", what: "delete", marker: "已删除别名", stdout: stdoutEmpty,
			run: func(t *testing.T, _ *streamFixture) (string, string) {
				model := aliasHostModel(t)
				created, _ := mustStream(t, func() error {
					return runAlias([]string{
						"create", "--model", model, "--upstream", uniqueName("echo-doomed-up"),
					})
				})
				victim := idFromStdout(t, "别名", created)
				return mustStream(t, func() error {
					return runAlias([]string{"delete", "--id", id(victim)})
				})
			}},

		{command: "price", what: "create", marker: "已加入计价规则", stdout: stdoutID,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPrice([]string{
						"create", "--scope", "platform", "--name", uniqueName("echo-rule"),
						"--expr", ruleExpr, "--models", fx.modelName, "--by", id(fx.userID),
					})
				})
			}},
		{command: "price", what: "update", marker: "已更新计价规则", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPrice([]string{
						"update", "--id", id(fx.ruleID), "--priority", "4", "--by", id(fx.userID),
					})
				})
			}},
		// 成本规则那三条与上面 price 那条同一个形状：写成功的回显只走 stderr。
		// update 与 disable 各自先建一条：这张表里的每条用例各跑各的，前一条
		// 建出来的规则接不到后一条手上。
		{command: "upstream-cost", what: "create", marker: "已加入成本规则", stdout: stdoutID,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runUpstreamCost([]string{
						"create", "--channel", id(fx.channelID), "--expr", ruleExpr,
						"--models", fx.modelName,
					})
				})
			}},
		{command: "upstream-cost", what: "update", marker: "已更新成本规则", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				ruleID := createStreamCostRule(t, fx)
				return mustStream(t, func() error {
					return runUpstreamCost([]string{"update", "--id", id(ruleID), "--priority", "3"})
				})
			}},
		{command: "upstream-cost", what: "disable", marker: "已停用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				ruleID := createStreamCostRule(t, fx)
				return mustStream(t, func() error {
					return runUpstreamCost([]string{"disable", id(ruleID)})
				})
			}},

		{command: "price", what: "disable", marker: "已停用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPrice([]string{"disable", id(fx.ruleID)})
				})
			}},
		{command: "price", what: "enable", marker: "已启用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPrice([]string{"enable", id(fx.ruleID)})
				})
			}},

		{command: "plugin", what: "enable", marker: "已启用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPlugin([]string{"enable", fx.pluginKey})
				})
			}},
		{command: "plugin", what: "disable", marker: "已停用", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPlugin([]string{"disable", fx.pluginKey})
				})
			}},
		// 下面三条改库，回显同样只走 stderr。卸载用的是另起的一个插件：
		// 删掉 fx.pluginKey 之后，后面两条 kv 就没有插件可用了。
		{command: "plugin", what: "delete", marker: "已删除插件", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				key := uniqueName("echo-doomed-plugin")
				path := writePluginFile(t, "doomed.js", pluginSource(key, "1.0.0", 0))
				mustStream(t, func() error {
					return runPlugin([]string{"publish", path, "--by", id(fx.userID)})
				})
				return mustStream(t, func() error { return runPlugin([]string{"delete", key}) })
			}},
		{command: "plugin", what: "kv set", marker: "已写入插件", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				return mustStream(t, func() error {
					return runPlugin([]string{
						"kv", "set", "--plugin", fx.pluginKey, "--user", id(fx.userID),
						"--key", "echo", "--value", "v",
					})
				})
			}},
		// 这一条的 marker 与卸载那条不同：两条回显的开头本来就该分开
		// （一个删插件、一个只删键），marker 跟着分开才测得出那件事。
		{command: "plugin", what: "kv delete", marker: "的键 echo-doomed", stdout: stdoutEmpty,
			run: func(t *testing.T, fx *streamFixture) (string, string) {
				// 先写一个再删：删一个不存在的键本身是失败（退出码 4）。
				mustStream(t, func() error {
					return runPlugin([]string{
						"kv", "set", "--plugin", fx.pluginKey, "--user", id(fx.userID),
						"--key", "echo-doomed", "--value", "v",
					})
				})
				return mustStream(t, func() error {
					return runPlugin([]string{
						"kv", "delete", "--plugin", fx.pluginKey, "--user", id(fx.userID),
						"--key", "echo-doomed",
					})
				})
			}},
	}

	// 条数写成判据：少了说明某条写命令没人看着了，而「回显又跑回 stdout」正是
	// 那样溜进来的。
	if len(cases) != 28 {
		t.Fatalf("这张表里 %d 条写命令，期望 28 条", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.command+"/"+tc.what, func(t *testing.T) {
			stdout, stderr := tc.run(t, fx)

			if strings.Contains(stdout, tc.marker) {
				t.Errorf("写成功的回显混进了 stdout：\n%s", stdout)
			}
			if got := strings.Count(stderr, tc.marker); got != 1 {
				t.Errorf("回显在 stderr 上出现 %d 次，期望恰好一次：\n%s", got, stderr)
			}
			checkStdoutValue(t, tc.stdout, stdout)
		})
	}
}

// checkStdoutValue 断言 stdout 上留下的正是「能被脚本接走的值」。
func checkStdoutValue(t *testing.T, kind stdoutKind, stdout string) {
	t.Helper()
	switch kind {
	case stdoutEmpty:
		if stdout != "" {
			t.Errorf("这条命令没有值要交出去，stdout 该是空的，实际是：\n%s", stdout)
		}
	case stdoutID:
		if _, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64); err != nil {
			t.Errorf("stdout 该只有一行的 ID，实际是 %q（%v）", stdout, err)
		}
	case stdoutSecret:
		if strings.TrimSpace(stdout) == "" || strings.Contains(strings.TrimRight(stdout, "\n"), "\n") {
			t.Errorf("stdout 该只有一行的明文 key，实际是 %q", stdout)
		}
	}
}

// ── 用例三：同一件事只报一次 ────────────────────────────────────────────

// TestSettingsLoadFailureIsReportedOnce 是「同一事实只报一次」那条。
//
// 配置服务加载不起来时，这件事原先有两处会说：命令自己先打一句「警告：…」，
// 返回的那个错误又被 main 原样打一遍。**两处各说各的可以，同一句话不能打两遍**。
//
// 判据取「错误详情」这一截：主句由错误出口说，重复的正是后面的细节。数的是
// 合并输出，因为使用者看到的就是两股流合起来的样子。
func TestSettingsLoadFailureIsReportedOnce(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	withVerbose(t, false)

	// 往库里塞一个 Registry 不认识的键：校验会因此失败（见 settings.Load）。
	// 键名只有这条用例会用，用完删掉。
	const typoKey = "typo.report_once_test"
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO settings (key, value, description) VALUES ($1, '1'::jsonb, '用例塞的')`,
		typoKey); err != nil {
		t.Fatalf("塞一行未声明的键失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM settings WHERE key = $1`, typoKey); err != nil {
			t.Errorf("清理用例塞的那一行失败: %v", err)
		}
	})

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runSettingsList(nil)
		if err != nil {
			// main 在真实进程里做的下一步（见 main.go 的 exitWithFailure）。
			// 不算上它，测的只是这条命令那一半，而两处都打的那一半在 main 里。
			exitWithFailure(err)
		}
	})
	if err == nil {
		t.Fatal("配置加载不起来时命令应当以非零退出码收尾")
	}

	const lead = "这套配置服务加载不起来: "
	if !strings.Contains(err.Error(), lead) {
		t.Fatalf("错误里要说出这意味着什么（服务起不来），实际是 %v", err)
	}
	detail := strings.TrimPrefix(err.Error(), lead)
	if got := strings.Count(stdout+stderr, detail); got != 1 {
		t.Errorf("同一句话在输出里出现了 %d 次，只该一次：\nstdout:\n%s\nstderr:\n%s",
			got, stdout, stderr)
	}
	// 那句话要么在报告里，要么走错误出口。这一条钉住它走的是后者，而且带上了
	// 「谁在说」的形状（`larkaxctl: `）。
	if !strings.Contains(stderr, "larkaxctl: "+lead) {
		t.Errorf("错误出口那一行没打出来：\n%s", stderr)
	}
	// 清单照旧列出来：配置有问题的时候恰恰是最需要看到内容的时候。
	if !strings.Contains(stdout, typoKey) {
		t.Errorf("校验不过时清单也照列：\n%s", stdout)
	}
}

// ── 用例四：无条件的絮语没了 ────────────────────────────────────────────

// TestRotateApplyBackupReminderOnlyUnderVerbose 是絮语那条。
//
// 「开始前请确认已经备份」原先只要带 --apply 就出现——而**每一次** --apply 都会
// 出现的那句话，读的人第二遍就不再读它了。它现在跟着 -v 走：要动手的人按需多问
// 一句，而用法文本里那一节（rotateUsage 的「跑 --apply 之前先备份 channels 表」）
// 照旧在。
//
// 不带任何特殊条件地跑一次 --apply（库是刚迁好的，没有渠道也算正常：轮换是全表
// 扫描，扫到 0 行同样是成功路径）。
func TestRotateApplyBackupReminderOnlyUnderVerbose(t *testing.T) {
	testDB(t)
	setupEnv(t)

	withVerbose(t, false)
	stdout, stderr, err := runStream(t, func() error {
		return runSecret([]string{"rotate", "--apply"})
	})
	if err != nil {
		t.Fatalf("rotate --apply 失败: %v（stderr: %s）", err, stderr)
	}
	if strings.Contains(stdout+stderr, "备份") {
		t.Errorf("默认那一档不该打备份提醒：\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	withVerbose(t, true)
	_, verboseStderr, verboseErr := runStream(t, func() error {
		return runSecret([]string{"rotate", "--apply"})
	})
	if verboseErr != nil {
		t.Fatalf("rotate --apply -v 失败: %v（stderr: %s）", verboseErr, verboseStderr)
	}
	if !strings.Contains(verboseStderr, "备份") {
		t.Errorf("-v 下备份提醒要还在（它是从默认输出里挪走、不是删掉）：\n%s", verboseStderr)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// streamFixture 是上面那些用例共用的一批实体。
//
// 建一次被几十条用例复用：每一条只跑**它自己那条命令**，共用同一批实体能省掉
// 几十次建对象的往返，也让「这条命令到底打了什么」不掺进夹具自己的输出。
type streamFixture struct {
	db         *sql.DB
	userID     int64
	keyID      int64
	channelID  int64
	modelName  string
	aliasID    int64
	ruleID     int64
	pluginKey  string
	requestID  string
	calendarAt string
}

// newStreamFixture 建好夹具，并把三个输出开关拨回默认档。
//
// 拨回默认档这一步不能省：这一组判的是**默认那一档**的输出，而别的用例可能把
// outputFormat / quietOutput 留在别的档上，那样跑出来的绿是假的。
func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	db := testDB(t)
	setupEnv(t)

	cli.OutputFormat, cli.VerboseOutput, cli.QuietOutput = cli.FormatTable, false, false
	t.Cleanup(func() { cli.OutputFormat, cli.VerboseOutput, cli.QuietOutput = cli.FormatTable, false, false })

	fx := &streamFixture{db: db, modelName: uniqueName("stream-model")}
	fx.userID = createTestUser(t)
	fx.keyID = seedAPIKey(t, db)

	ustr := strconv.FormatInt(fx.userID, 10)

	mustStream(t, func() error {
		return runModel([]string{"create", "--model", fx.modelName, "--display", "流夹具模型"})
	})
	fx.channelID = createTestChannel(t, uniqueName("stream-channel"), fx.modelName)

	aliasOut, _ := mustStream(t, func() error {
		return runAlias([]string{
			"create", "--model", fx.modelName, "--upstream", uniqueName("stream-upstream"),
		})
	})
	fx.aliasID = idFromStdout(t, "别名", aliasOut)

	ruleOut, _ := mustStream(t, func() error {
		return runPrice([]string{
			"create", "--scope", "platform", "--name", uniqueName("stream-rule"),
			"--expr", ruleExpr, "--models", fx.modelName, "--by", ustr,
		})
	})
	fx.ruleID = idFromStdout(t, "计价规则", ruleOut)

	mustStream(t, func() error { return runWallet([]string{"create", "--user", ustr}) })

	fx.pluginKey = uniqueName("stream-plugin")
	path := writePluginFile(t, "stream.js", pluginSource(fx.pluginKey, "1.0.0", 0))
	mustStream(t, func() error { return runPlugin([]string{"publish", path, "--by", ustr}) })

	// 快照：snapshot get 要有一条能读出来的行。**不截断**——截断那一档另有一句
	// 附注（它是有条件的，所以允许出现），而夹具让这一条走普通那一档。
	fx.requestID = uniqueName("stream-request")
	insertSnapshot(t, db, probeSnapshot{
		requestID: fx.requestID, userID: fx.userID, direction: "request",
		skeleton: `{"model":"` + fx.modelName + `"}`,
	})

	// 一份日历数据文件（pending 那一档不带天数据，是最小的一份合法文件）。
	fx.calendarAt = filepath.Join(t.TempDir(), "calendar.json")
	if err := os.WriteFile(fx.calendarAt,
		[]byte(`{"region":"CN","year":2031,"status":"pending"}`), 0o600); err != nil {
		t.Fatalf("写日历数据文件失败: %v", err)
	}

	return fx
}

// runStream 跑一条命令，把两个流接回来。
func runStream(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	stdout, stderr = captureStreams(t, func() { err = fn() })
	return stdout, stderr, err
}

// mustStream 跑一条命令，非要它成功不可；失败时把两个流一起打出来。
func mustStream(t *testing.T, fn func() error) (string, string) {
	t.Helper()
	stdout, stderr, err := runStream(t, fn)
	if err != nil {
		t.Fatalf("命令没跑成: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	return stdout, stderr
}

// idFromStdout 读出一条 create 命令打在 stdout 上的那个 ID。
func idFromStdout(t *testing.T, what, stdout string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("%s 的 stdout 应当是 ID，得到 %q（%v）", what, stdout, err)
	}
	return id
}

// aliasHostModel 建一个专门用来挂别名的规范模型，返回它的名字。
//
// 一条规范名在平台级只容得下一个上游名（再配一条会被拒），所以「凭空造一条别名」
// 的用例得自己带一个模型来，不能借用夹具那一条——那条上已经挂着了。
func aliasHostModel(t *testing.T) string {
	t.Helper()
	model := uniqueName("echo-alias-model")
	mustStream(t, func() error {
		return runModel([]string{"create", "--model", model, "--display", "别名宿主"})
	})
	return model
}
