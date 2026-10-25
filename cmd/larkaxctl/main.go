// Command larkaxctl 是运维命令行：迁移、状态检查与后续的运维任务。
//
// 与主服务共用同一份配置加载与迁移代码，避免「命令行跑通了但服务起不来」这类错配。
//
// # 帮助文案只有一处来源：手写的 *Usage 常量
//
// 每个子命令的 fs.Usage 都被改写成打印手写的常量（`channelShortUsage` 这一类），
// 而 flag 包自己的 `PrintDefaults` 全包没有调用点。所以**给 flag 注册传的说明字符串
// 一个字都不会被打印**——注册助手（`stringAlias` / `legacyStringFlag` 那几个）因此
// 不收说明参数，注册时一律传空串。
//
// 要改用户看到的帮助，就改那些 *Usage 常量。**别往 flag 注册里补说明**：补了不会
// 出现在任何输出里，只会多出一份看起来生效的副本，而它与常量分叉时没有任何用例会报出来。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/settings"
)

// version 由构建时注入，与主服务用**同一个变量名**：
// 两个二进制的版本必须能对上，否则「命令行跑通了但服务起不来」这类
// 错配又多出一个来源。
var version = "dev"

func main() {
	err := run()
	if err == nil {
		return
	}
	if code := exitWithFailure(err); code != exitOK {
		os.Exit(code)
	}
}

// exitWithFailure 是一次失败在进程出口上的全部处置，返回该用的退出码。
//
// 它抽出来不是为了「少写两行」：**main 在用例里调不到**，而
// 「同一件事只报一次」这条判据横跨命令与这一层——命令那一半看得见，
// 这一半（`larkaxctl: …` 那一行）看不见。只测命令那一半，两处都打的那种
// 缺陷就仍然只靠人眼（见 streams_test.go 的同一事实那条用例）。
//
// 三个分支与它们各自的理由：
//
//   - **要帮助不是失败**：flag 在 -h / --help 时返回 ErrHelp，而那时用法已经
//     打印过了。把它当错误会让 `larkaxctl channel -h` 在脚本里看起来像
//     一次失败，还多打一行「flag: help requested」。
//   - **报告已经说清了的失败**（体检类命令的结论）不再补打一行：那一行与报告
//     说的是同一件事，而它的形状（`larkaxctl: …`）读起来像一次崩溃。退出码
//     照旧非零，调度器按它分派。
//   - 其余的在 stderr 上打一行 `larkaxctl: <错误>`，退出码由 exitCode 分类。
func exitWithFailure(err error) int {
	if helpRequested(err) {
		return exitOK
	}
	if !reportedOnly(err) {
		fmt.Fprintf(os.Stderr, "larkaxctl: %v\n", err)
	}
	return exitCode(err)
}

// command 是顶层命令空间里的一个条目。
type command struct {
	// name 是命令名，在整个顶层空间里必须唯一：分派按它查表，
	// 重名的后一个条目永远轮不到。
	name string
	// summary 是根帮助页上跟在名字后面的那一句话。
	summary string
	// help 是这条命令的两版帮助文本（见 helpText）。
	help helpText
	// run 处理这条命令，args 是命令名之后的参数。
	run func([]string) error
}

// helpText 是一条命令的两版帮助文本。
//
// 两版的分工是这一条规格的骨架：**短版**回答「这条命令有哪些动作、参数怎么给」，
// 外加一两条最关键的边界，收在一屏之内；**长版**在短版之外再把其余边界说完。
// 哪一版由**要帮助的方式**决定——`-h` / `--help` 与参数写错时打的那一份是短版，
// `larkaxctl help <命令>` 与 `<命令> help` 是长版（见 dispatch）。
//
// 两版都**不写设计理由**：某个字段为什么必填、别家工具怎么做这类话，家是
// docs/cli-reference.md 里那一节。写在帮助里的话，跑命令的人每次敲错参数都要
// 被念一遍，而他要的只是那几行用法。这条有门钉着（见 usage_test.go）。
type helpText struct {
	// short 是短版：首行点出自己是谁，然后是用法骨架，最后指向长版。
	short string
	// long 是长版：短版 + 其余关键边界，末尾指向 docs/cli-reference.md。
	long string
}

// printUsage 打一份帮助文本，是**帮助文本的唯一出口**。
//
// 收成一处之前，帮助散在几十处（各命令 help 分支里的 fmt.Print、各 fs.Usage
// 闭包里的 fmt.Fprint），于是「打哪一版、打在哪条流上」得挨处看一遍才知道，
// 而两处写法不一致在页面上看不出来。现在只剩两条规则：
//
//   - **打哪一版**看这一次要的是哪一版：`larkaxctl help <命令>` 与
//     `<命令> help` 给长版，`-h` / `--help` 与参数写错时给短版。
//   - **打在哪条流**看这份帮助是不是使用者要来的。要来的（`-h`、`help <命令>`）
//     走 stdout——它是这一次运行的结果，要能进管道（`larkaxctl help channel |
//     less`）。跟着错误打的那一份走 stderr：那时 stdout 上可能正要落一份 JSON
//     （`--format json`），把用法页混进去下游的 jq 就解析不了；而且它本来就是
//     那条错误的附注。这与 docs/cli-output.md 定下的分界是同一条。
//
// w 收 os.Stdout / os.Stderr 本身，不是 humanStdout()：`-q` 压的是「给人看」那一
// 档的输出，而帮助是使用者主动要的，不该被它压掉。
//
// 上色在这一处做，不在那几十个调用点各做一遍：只给**结构**上色（段标题与组标题
// 加粗、命令名与选项名加粗、说明与指针行置灰），规则见 style.go 的 paintHelp。
// `--format json` 是个例外——它压掉上色：那一档是给管道下游的纯文本，混进控制码
// 只会让下游多剥一层壳。`-q` 不在例外里：帮助照打、照上色。
func printUsage(w io.Writer, text string) {
	p := newPalette(w)
	if jsonOutput() {
		p = palette{}
	}
	_, _ = fmt.Fprint(w, p.paintHelp(text))
}

// commandGroup 是根帮助页上的一个业务领域分组。
//
// 分组只影响排版，不影响分派：一条命令属于哪一组，与它能被怎么调用无关。
type commandGroup struct {
	title    string
	commands []command
}

// commandGroups 是顶层命令的注册表，顺序就是根帮助页上的顺序。
//
// 根帮助页与分派共用这一份数据：分成两处写的话，加一条命令要记得改两个
// 地方，而漏掉分派那一处的表现是「帮助里有它、跑起来却是未知子命令」。
var commandGroups = []commandGroup{
	{
		title: "核心与诊断 (Core & Diagnostics)",
		commands: []command{
			{name: "doctor", summary: "系统健康与链路连通性体检", help: doctorHelp, run: runDoctor},
			{name: "settings", summary: "运行期动态配置项管理（settings 表）", help: settingsHelp, run: runSettings},
			{name: "admin", summary: "内部监听上 admin API 的令牌（create、revoke）", help: adminHelp, run: runAdmin},
			{name: "version", summary: "打印版本号与编译元数据", help: versionHelp, run: runVersion},
			{name: "completion", summary: "打印 bash / zsh / fish 的补全脚本", help: completionHelp, run: runCompletionCommand},
		},
	},
	{
		title: "路由与模型配置 (Routing & Models)",
		commands: []command{
			{name: "channel", summary: "上游渠道管理（网关、凭证、关联模型与出站映射）", help: channelHelp, run: runChannel},
			{name: "account", summary: "上游凭证账号（建、列、换 key、删）", help: accountHelp, run: runAccount},
			{name: "model", summary: "规范模型目录（catalog 基础定义）", help: modelHelp, run: runModel},
			{name: "alias", summary: "出站映射的旧入口（配置命令在 channel map）", help: aliasHelp, run: runAlias},
			{name: "price", summary: "模型计价规则与日历管理（含 calendar 子命令）", help: priceHelp, run: runPrice},
			{name: "upstream-cost", summary: "上游成本规则（按渠道与模型记成本价）", help: upstreamCostHelp, run: runUpstreamCost},
		},
	},
	{
		title: "权限与财务 (Access & Ledger)",
		commands: []command{
			{name: "user", summary: "系统用户管理（创建、查询、修改与销户）", help: userHelp, run: runUser},
			{name: "apikey", summary: "API Key 访问密钥、状态与额度管理", help: apiKeyHelp, run: runAPIKey},
			{name: "wallet", summary: "用户钱包账户与入账管理", help: walletHelp, run: runWallet},
			{name: "plan", summary: "额度包、订阅与扣费顺序管理", help: planHelp, run: runPlan},
			{name: "usage", summary: "用量与计费的只读查询（事件、请求与汇总）", help: usageHelp, run: runUsage},
			{name: "ledger", summary: "财务账本管理（余额查询、分录校验、对账、对账历史）", help: ledgerHelp, run: runLedger},
		},
	},
	{
		title: "系统与数据维护 (Maintenance & Data)",
		commands: []command{
			{name: "migrate", summary: "数据库版本迁移（up、down、status、version）", help: migrateHelp, run: runMigrate},
			{name: "partition", summary: "数据库月度分区维护（check、ensure）", help: partitionHelp, run: runPartition},
			{name: "secret", summary: "安全凭证与主密钥维护（rotate 密钥轮换）", help: secretHelp, run: runSecret},
			{name: "maintenance", summary: "数据留存与过期清理（idempotency 幂等记录、session 控制台会话）", help: maintenanceHelp, run: runMaintenance},
			{name: "snapshot", summary: "请求结构快照管理（cleanup、allowlist、get、redact）", help: snapshotHelp, run: runSnapshot},
			{name: "consent", summary: "原样采集请求正文的授权（grant、list、revoke）", help: consentHelp, run: runConsent},
			{name: "plugin", summary: "动态插件系统管理（list、get、publish、enable、disable、delete、kv）", help: pluginHelp, run: runPlugin},
		},
	},
}

// legacyCommands 是搬过家、但旧路径必须继续能用的那几条。
//
// 它们不进根帮助页（见 usage），也**不是**「注册了却指向另一个格子」：
// 每一条都直接接在同一个处理函数上，于是新旧两条路径走的是同一段代码、
// 打出的是同一份输出。这与 flags.go 里 legacyStringFlag 是同一个立场——
// 旧名字留下来，而且真的生效。
//
// name 与 commandGroups 里的命令名不重叠（有用例钉着），所以查表时谁先谁后
// 不影响结果，旧路径也不会遮住任何一条正式命令。
//
// help 给的是它们搬去之后那一格的帮助：`larkaxctl help rotate` 与
// `larkaxctl help secret` 打出来的正文指向同一条路，旧路径才不会变成一个
// 「认得出名字、却什么也不说」的入口。
var legacyCommands = []command{
	{name: "reconcile", help: ledgerHelp, run: runReconcile},
	{name: "rotate", help: rotateHelp, run: runRotate},
	{name: "idempotency", help: maintenanceIdempotencyHelp, run: runIdempotency},
	{name: "calendar", help: priceCalendarHelp, run: runCalendar},
}

// lookupCommand 按名字找出顶层命令的条目。
//
// 返回整个条目而不是只返回处理函数：条目上还有 `summary` 与 `help`，
// 而 `larkaxctl help <命令>` 要的正是后者——只返回处理函数的话，取帮助正文
// 就得再走一遍同一张表，两处遍历迟早会漂。
func lookupCommand(name string) (command, bool) {
	for _, g := range commandGroups {
		for _, c := range g.commands {
			if c.name == name {
				return c, true
			}
		}
	}
	for _, c := range legacyCommands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// usage 是根帮助页。
//
// 它由 commandGroups 渲染出来，不是手写的：手写的那一份与分派表是两处数据，
// 加一条命令要记得改两个地方，而漏掉这一处的表现是「命令跑得通，根页面里
// 却没有它」。
//
// 根页面只列到一级命令为止，每行一句话；二级动作与它们的参数各在自己的
// --help 里。三四级的参数（--amount、--idempotency-key 这些）堆在根页面上
// 时，看的人要在三十来行里找自己要的那一条。
var usage = buildUsage()

// buildUsage 渲染根帮助页：命令清单由注册表生成，说明文字来自 usageNotes。
func buildUsage() string {
	width := 0
	for _, g := range commandGroups {
		for _, c := range g.commands {
			if n := len(c.name); n > width {
				width = n
			}
		}
	}

	var b strings.Builder
	// 命令名都是 ASCII，所以按字节数对齐与按显示宽度对齐是一回事。
	// 介绍句与「用法：」之间不空行：根帮助页的行数有上界（见 usage_test.go 的
	// TestRootUsageFitsOneScreen），而「用法：」独成一行才满足段标题的判据
	// （顶格 + 以「：」结尾 + 下一非空行缩进）、才会被加粗；并进骨架那一行就
	// 会整行置灰。所以省下的是那个空行，不是标题那一行。
	b.WriteString("larkaxctl 是 larkax 的运维命令行。\n用法：\n  larkaxctl <命令> [子命令] [选项]\n")
	for _, g := range commandGroups {
		b.WriteString("\n" + g.title + "：\n")
		for _, c := range g.commands {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, c.name, c.summary)
		}
	}
	b.WriteString(usageNotes)
	return b.String()
}

// usageNotes 是根帮助页上不随命令清单变化的那几段。
//
// 只留「跑命令的人当下要的」：全局选项、短选项、退出码与去向。查找规则、
// 取舍与边界那类解释性的长句在 docs/cli-reference.md，这里不再留第二份。
//
// 行数、每行宽度与「不含子命令参数」几条都有用例钉着，见 usage_test.go。
const usageNotes = `
全局选项（写在子命令前后都认）：
  --env-file <路径>       指定要加载的 .env 文件（默认从当前目录往上找到仓库根）
  --format <table|json>   输出格式：table 给终端，json 给管道下游（-o 是它的简写）
  --verbose               打细节，目前 doctor 用得上（-v 是它的简写）
  --quiet                 压掉给人看的输出，只在出错时说话（-q 是它的简写）

短选项：-u --user、-m --model、-o --format、-v --verbose、-q --quiet、-V --version（其余写全名）
退出码：0 成功，1 其他失败，2 命令行参数错，3 连不上数据库，4 约束冲突。
每条命令的详细用法：larkaxctl help <命令>；全局约定与 .env 查找见 docs/cli-reference.md。
`

// migrateHelp 是 migrate 的两版帮助文本（见 helpText）。
var migrateHelp = helpText{short: migrateShortUsage, long: migrateUsage}

// migrateShortUsage 是 migrate 的短版：`-h` / `--help` 与参数写错时打的那一份。
//
// 迁移的七个动作原先列在根帮助页上，那是一行一条的三级用法——根页面只留
// 一级命令，具体动作各归各的 --help。
const migrateShortUsage = `larkaxctl migrate 应用、回滚与生成数据库迁移。

用法：
  larkaxctl migrate up                     应用全部未执行的迁移
  larkaxctl migrate up --allow-missing     连编号更小、此前漏掉的迁移一起补应用
  larkaxctl migrate down                   回滚最近一次迁移
  larkaxctl migrate down-to <版本>          回滚到指定版本（0 表示全部回滚）
  larkaxctl migrate up-to <版本>            前进到指定版本
  larkaxctl migrate status                 显示每个迁移的应用状态
  larkaxctl migrate version                显示当前数据库版本
  larkaxctl migrate create <名称>           生成迁移文件（固定为 .sql）

create 只写文件、不碰数据库，其余各条都要连库。

更细的边界：larkaxctl help migrate。
`

// migrateUsage 是 migrate 的长版：`larkaxctl help migrate` 与 `migrate help`。
const migrateUsage = `larkaxctl migrate 应用、回滚与生成数据库迁移。

用法：
  larkaxctl migrate up                     应用全部未执行的迁移
  larkaxctl migrate up --allow-missing     连编号更小、此前漏掉的迁移一起补应用
  larkaxctl migrate down                   回滚最近一次迁移
  larkaxctl migrate down-to <版本>          回滚到指定版本（0 表示全部回滚）
  larkaxctl migrate up-to <版本>            前进到指定版本
  larkaxctl migrate status                 显示每个迁移的应用状态
  larkaxctl migrate version                显示当前数据库版本
  larkaxctl migrate create <名称>           生成迁移文件（固定为 .sql）

create 只写文件、不碰数据库，其余各条都要连库。

up 默认不补应用「编号比当前版本小、却从未应用过」的迁移：遇到时它列出那几条并停下。
加 --allow-missing 才补，补了哪几条打在标准错误上；其余动作不接受这个开关。

up-to / down-to 一步一个版本地走到目标，不用 goose 自带的同名命令：那两个会停在
目标版本的相邻位置（从 v2 执行 down-to 0 只退到 v1），差一位的表现比不支持更危险。

更细的约定见 docs/cli-reference.md 的「migrate」一节。
`

// versionHelp 是 version 的两版帮助文本（见 helpText）。
var versionHelp = helpText{short: versionShortUsage, long: versionUsage}

// versionShortUsage 是 version 的短版：`-h` / `--help` 与参数写错时打的那一份。
//
// 「每个一级命令的 --help 至少有一句话」：这条命令原先把参数全丢掉、直接打
// 版本号，于是 `larkaxctl version --help` 打出来的是一个版本号。
const versionShortUsage = `larkaxctl version 打印版本号与编译元数据。

用法：
  larkaxctl version              打印版本号
  larkaxctl version -o json      打成 {"ok":true,"version":"..."}

更细的边界：larkaxctl help version。
`

// versionUsage 是 version 的长版：`larkaxctl help version` 与 `version help`。
const versionUsage = `larkaxctl version 打印版本号与编译元数据。

用法：
  larkaxctl version              打印版本号
  larkaxctl version -o json      打成 {"ok":true,"version":"..."}

版本号在构建时注入，从源码直接编译出来的是 dev。

更细的约定见 docs/cli-reference.md 的「version」一节。
`

// migrationsDir 是迁移文件所在目录，相对于仓库根目录。
const migrationsDir = "internal/migrate/sql"

func run() error {
	// 全局选项先摘出来，再分派子命令：它们必须在**任何**子命令之前生效——
	// --env-file 是因为每条子命令一进去就加载配置，而配置可能就来自那个文件；
	// --format 是因为它决定那几条命令往 stdout 上打什么。
	//
	// 两个各扫一遍全表，所以它们之间的先后不影响结果：
	// `--env-file x --format json doctor` 与 `--format json --env-file x doctor`
	// 都认。
	envFile, args, err := takeEnvFileFlag(os.Args[1:])
	if err != nil {
		return err
	}
	if envFile != "" {
		if envErr := config.UseEnvFile(envFile); envErr != nil {
			// 归到参数错：文件路径是命令行给的，找不到它要改的是命令。
			return usageErr(envErr)
		}
	}

	format, args, err := takeFormatFlag(args)
	if err != nil {
		return err
	}
	outputFormat = format

	// 两个开关也扫全表，理由与上面两个相同。它们之间的先后不影响结果。
	verboseOutput, args, err = takeVerboseFlag(args)
	if err != nil {
		return err
	}
	quietOutput, args, err = takeQuietFlag(args)
	if err != nil {
		return err
	}
	// 两个一起给是**参数错**，不是「后者赢」：它们要的是相反的东西，而按顺序
	// 取一个的表现是「我明明加了 -v，怎么什么都没多打」——那种错要人查半天。
	if verboseOutput && quietOutput {
		return usageErr(errors.New(
			"--verbose 与 --quiet 不能同时给：一个要每一项的细节，一个要一声不吭"))
	}

	if len(args) == 0 {
		printUsage(os.Stderr, usage)
		return usageErr(errors.New("缺少子命令"))
	}
	return dispatch(args)
}

// dispatch 按第一个参数分派子命令。全局选项在调用之前已经摘掉（见 run）。
//
// 单独拆出来是为了**能被用例调到**：run 自己从 os.Args 取参数，而
// 「根帮助页指的那条去向真的走得通」这件事（`larkaxctl help <命令>`）
// 要有一个不经进程的参数入口才测得了。
func dispatch(args []string) error {
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help":
		// `larkaxctl help <命令>` 打那条命令自己的**长版**帮助，不给名字时打根帮助页。
		// 根帮助页的末行指的就是这个写法——**指针指向的必须真的能用**，否则
		// 它比不写更坏：人照着敲一次，得到的是刚才那一页。
		if len(rest) == 0 {
			printUsage(os.Stdout, usage)
			return nil
		}
		c, ok := lookupCommand(rest[0])
		if !ok {
			printUsage(os.Stderr, usage)
			return usageErr(fmt.Errorf("未知子命令 %q", rest[0]))
		}
		printUsage(os.Stdout, c.help.long)
		return nil
	case "-V", "--version":
		name, rest = "version", nil
	}

	c, ok := lookupCommand(name)
	if !ok {
		printUsage(os.Stderr, usage)
		return usageErr(fmt.Errorf("未知子命令 %q", name))
	}
	// `<命令> help` 与 `larkaxctl help <命令>` 是同一条路，在这里一次收掉。
	//
	// 收在分派这一处、不在各命令自己的参数解析里各补一个分支：收一次之后每条
	// 命令（含以后新加的）自带长版入口，不必记得补；也省得「有的命令认 help、
	// 有的不认」这种要敲下去才发现的不一致。-h / --help 不走这里——它是参数，
	// 要与同一条命令的其它参数一起看（`channel list --help` 说 `list`）。
	if len(rest) > 0 && rest[0] == "help" {
		printUsage(os.Stdout, c.help.long)
		return nil
	}
	return c.run(rest)
}

// runVersion 打印版本号与编译元数据。
//
// 多出来的这些参数原先被整段丢掉：`larkaxctl version --help` 打出来的是一个
// 版本号，而未知子命令会被分派拦下、`larkaxctl version --nonsense` 却会安安静
// 静地成功。参数错要报成参数错（退出码 2），脚本才分得清「我写错了」与
// 「命令跑了」。
func runVersion(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help":
			printUsage(os.Stdout, versionShortUsage)
			return nil
		}
		return usageErr(fmt.Errorf("version 不接受参数，得到 %q", args[0]))
	}

	if jsonOutput() {
		doc := jsonDoc(jsonOK)
		doc["version"] = version
		return emitJSON(os.Stdout, doc, nil)
	}
	_, _ = fmt.Fprintln(humanStdout(), version)
	return nil
}

// isEnvFileFlag 判断一个参数名是不是全局选项 --env-file。
//
// 单横线一并认：本仓其余选项由 flag 包解析，而 flag 同时接受 -name 与
// --name；只有全局选项不认的话，`larkaxctl -env-file x doctor` 会以
// 「未知子命令」收场——那不是使用者能自己看明白的错。
func isEnvFileFlag(name string) bool {
	return name == "--env-file" || name == "-env-file"
}

// takeEnvFileFlag 从参数里摘出全局选项 --env-file，返回它的取值与剩下的参数。
//
// # 为什么在最外层摘，而不是给每条子命令各接一个同名选项
//
// 全局选项写在子命令**前后**都该能用：
//
//	larkaxctl --env-file .env.staging doctor
//	larkaxctl doctor --env-file .env.staging
//
// 后者尤其重要——人补一个选项时习惯写在最后，而这本该是最自然的写法，却会被
// 子命令自己的 flag 集当成「flag provided but not defined」。摘掉再分派，
// 二十来条子命令不必各加一遍同一个选项，也不会因为漏掉一条而只有那条命令
// 认不出这个选项。
//
// 代价是这个字面量在**子命令的参数区里**也会被摘走。它没有第二种含义，
// 而把它当普通参数传下去的场景（用 --expr 之类传一段以它开头的文本）不值得
// 为它多开一条规则。
//
// 认 `--env-file <路径>` 与 `--env-file=<路径>` 两种写法，与 flag 包一致。
// 出现多次时取最后一个：同样与 flag 包「后写的赢」一致。
func takeEnvFileFlag(args []string) (string, []string, error) {
	var envFile string
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !isEnvFileFlag(name) {
			rest = append(rest, args[i])
			continue
		}

		if !hasValue {
			if i+1 >= len(args) {
				return "", nil, usageErr(fmt.Errorf("%s 后面要跟文件路径", name))
			}
			i++
			value = args[i]
		}
		if value == "" {
			// 空路径不当作「没给」：`--env-file=` 与 `--env-file ""` 都是
			// 明确给了、但给错了。当成没给的话，它会安静地用自动找到的那一份，
			// 而那与使用者的意图正好相反。
			return "", nil, usageErr(fmt.Errorf("%s 的路径不能为空", name))
		}
		envFile = value
	}

	return envFile, rest, nil
}

// runReconcile 跑一轮对账任务，命令是 larkaxctl ledger reconcile。
//
// # 为什么它对接到命令行而不是做成后台定时任务
//
// 定时任务需要选主与「上一轮还没跑完怎么办」这两件事，而它们与对账本身的
// 逻辑无关。挂到命令行之后，跑它的是外部的调度器（cron、Kubernetes 的
// CronJob、运维的手），那两件事由**那个**调度器负责——它本来就更擅长。
//
// 开关与口径全部来自 settings 表，命令行不带参数：这样「线上跑的是哪一套
// 口径」只有一个答案（库里的值），而不是「命令行里带了什么」。
//
// 它原先是一个顶层命令（larkaxctl reconcile），#475 把它收进账本域。那条旧
// 路径留着（见 legacyCommands），接的就是这个函数。
func runReconcile(args []string) error {
	if err := jsonUnsupported("ledger reconcile"); err != nil {
		return err
	}

	fs := newFlagSet("ledger reconcile", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, ledgerShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"ledger reconcile 不接受位置参数，得到 %q：开关与口径在 settings 表里", fs.Arg(0)))
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	// 走 openDB 而不是自己 sql.Open + Ping：打开与探活失败时的处置相同
	// （都归 exitDBUnreach），各写一遍的表现是同一类失败在两条命令上给出
	// 不同的退出码，而脚本对它们的处置是不一样的。
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return err
	}
	rc, err := store.ConfigFromSettings(values)
	if err != nil {
		return err
	}

	logger := observe.NewLogger(cfg.LogLevel, "", cfg.IsProduction())
	report, err := store.New(db).Reconcile(ctx, rc, store.LogAlerts(logger))
	// 报告先打，再处理错误：跑了一半的结果比一句「失败了」有用得多——
	// 它至少说明比到了哪里、已经发现了什么。
	//
	// err 一并交给打印函数：它决定这一轮的汇总行是「跑完了」还是「没跑完」。
	// 只看报告分不出这两种情况——中途失败时 CheckedWallets 也是 0、Findings
	// 也是空的，与「跑完了、范围内确实没有钱包」逐字相同。
	printReconcileReport(humanStdout(), report, err)
	if err != nil {
		return err
	}
	// 发现不一致时以非零退出码收尾：调度器与告警系统本来就盯着退出码，
	// 而「对账跑完了但有 3 条不一致」与「对账跑完了一切正常」是两件事，
	// 让它们落在同一个退出码上等于把告警交给「有没有人去看日志」。
	return reconcileFindingsErr(report)
}

// reconcileFindingsErr 把一轮对账的结果折成「要不要非零收尾」。
//
// 结论已经在打印出来的报告里（run_id、范围、每一条不一致），所以它标成
// reportedErr：main 不再补打那一行，只留退出码给调度器。
func reconcileFindingsErr(report *store.Report) error {
	if len(report.Findings) > 0 {
		return reportedErr(fmt.Errorf(
			"发现 %d 条不一致（见上面的清单与 run_id=%d）", len(report.Findings), report.RunID))
	}
	return nil
}

// reconcileTimeout 是单轮对账的时间上限。
//
// 命令行要有上限，否则一次卡住的查询会挂到运维发现为止。默认值给得宽：
// 全量扫描在大库上本来就慢，而把上限设小会让「跑不完」看起来像
// 「跑完了什么都没有」。
//
// 它必须**短于** internal/ledger/store 的 staleRunAfter（一小时）：后者是
// 「这一行确实已经死了」的判据，它只有在长于一轮合法运行的上限时才成立，
// 否则并发的另一轮会把一个还活着的运行标成 failed。超时不会留下结不掉的
// running 行——结账那一步不带取消，见 store.finishReconcileRun；只有结账
// 那一步自己也写不进去时才会留，那是与硬崩同一种收尾。
const reconcileTimeout = 30 * time.Minute

// printReconcileReport 把一轮对账的结果打到 w。
//
// runErr 是这一轮对账返回的错误，非空表示**没跑完**：报告里只有失败之前
// 已经比出来的那部分。它必须传进来，因为「没跑完」这件事报告本身说不出来
// ——中途失败时 CheckedWallets 是 0、Findings 是空的，与「跑完了、范围内
// 确实没有钱包」逐字相同。两条路径的 stdout 因此不能同形：失败时汇总行点明
// 「没跑完」，并且不出现「没有不一致。」——那句只有跑完才成立。
//
// # 为什么是「调用点传 err」而不是别的做法
//
//   - 把 err 塞进 store.Report 的一个字段：那等于让 store 在报告里记一个
//     它自己返回的错误的副本，同一个事实有两个来源，迟早对不上；而且
//     Report 还要落库（reconcile_runs），这个字段在那里没有对应列。
//   - 失败时不打印汇总行：与「报告先打」的设计冲突。失败之前比过几个钱包、
//     报出了哪几条不一致，恰恰是这一轮最有用的部分，不能一起丢掉。
//   - 按 CheckedWallets == 0 推断「没跑完」：那是拿计数猜原因。一轮正常
//     跑完而范围内没有候选用户时它同样是 0，而那时「比对=0 个用户」是真话，必须
//     照常打出来。判据只能是 err。
//
// 每处写入的返回值都显式丢弃（`_, _ =`）：目标是 stdout，写不进去只可能是
// 下游管道断了（`larkaxctl reconcile | head` 就是最常见的一种），
// 那种情况下重试与报错都没有意义，而它对这一轮对账的结论也毫无影响。
func printReconcileReport(w io.Writer, r *store.Report, runErr error) {
	if r == nil {
		return
	}
	scope := "增量"
	if r.FullScan {
		scope = "全量"
	}
	_, _ = fmt.Fprintf(w, "对账 run_id=%d %s 起点=%s 比对=%d 个用户\n",
		r.RunID, scope, r.Watermark.Format(time.RFC3339), r.CheckedWallets)

	if runErr != nil {
		// 已经比出来的不一致照常列出来（它仍然是真的），只是不能收在一句
		// 「没有不一致」上：这一轮没跑完，没比到的那些行什么也没说明。
		_, _ = fmt.Fprintln(w, "这一轮没跑完，上面的数字只到失败时为止，没比过的行仍然未知（原因见 stderr）。")
		if len(r.Findings) > 0 {
			printReconcileFindings(w, r)
		}
		return
	}

	if len(r.Findings) == 0 {
		_, _ = fmt.Fprintln(w, "没有不一致。")
		return
	}
	printReconcileFindings(w, r)
}

// printReconcileFindings 把这一轮发现的不一致逐条打出来。
func printReconcileFindings(w io.Writer, r *store.Report) {
	_, _ = fmt.Fprintf(w, "不一致 %d 条（余额 %d、冻结 %d、畸形科目 %d、陈旧预留 %d、面值 %d、低于下界 %d）：\n",
		len(r.Findings), r.BalanceMismatches(), r.FrozenMismatches(),
		r.MalformedAccounts(), r.StaleReservations(), r.PlanFaceShortfalls(),
		r.AccountBelowFloors())
	for _, f := range r.Findings {
		printFinding(w, f)
	}
}

// printFinding 打一条不一致。
//
// 陈旧预留与额度包面值各有一种行式，其余类别共用一种。
//
// 陈旧预留没有「快照 / 权威值」这一对（见 store.Finding 的字段注释），按通用
// 格式打出来会多出一个叫「差」而其实是金额本身的数。
//
// 面值类的两栏同样不是那一对：Snapshot 是 `prepaid:<订阅 id>` 的余额、Ledger
// 是该订阅的在途预留之和，按通用格式打出来会写成「快照=… 账本=…」——那是把
// 一条不等式的两边说成同一笔钱的两种记录。所以两栏各有自己的名字，「差」是
// **缺口**（在途减余额），订阅单独打出来，因为这一类是按订阅区分的。
//
// 科目余额下界只有「余额」一个数，另一边是表里给的下界（Snapshot）。按通用
// 格式打出来会写成「快照=0 账本=-5 差=-5」——那个「差」不是差额，而是比下界
// 低了多少。科目原文也要打出来：这一类报的就是「这个科目越界了」，不点名科目
// 的话读的人无从下手。
func printFinding(w io.Writer, f store.Finding) {
	switch f.Kind {
	case store.FindingStaleReservation:
		_, _ = fmt.Fprintf(w, "  %-18s user=%d asset=%s 冻住=%s 已修=%v\n",
			f.Kind, f.UserID, f.Asset, f.Ledger, f.Repaired)
	case store.FindingPlanFaceShortfall:
		_, _ = fmt.Fprintf(w, "  %-18s user=%d asset=%s 订阅=%s 余额=%s 在途=%s 缺口=%s 已修=%v\n",
			f.Kind, f.UserID, f.Asset, f.Key, f.Snapshot, f.Ledger, f.Diff(), f.Repaired)
	case store.FindingAccountBelowFloor:
		_, _ = fmt.Fprintf(w, "  %-18s asset=%s 科目=%s 余额=%s 下界=%s 已修=%v\n",
			f.Kind, f.Asset, f.Key, f.Ledger, f.Snapshot, f.Repaired)
	default:
		_, _ = fmt.Fprintf(w, "  %-18s user=%d asset=%s 快照=%s 账本=%s 差=%s 已修=%v\n",
			f.Kind, f.UserID, f.Asset, f.Snapshot, f.Ledger, f.Diff(), f.Repaired)
	}
	// 原因与补充说明各占一行：两者都可能很长，挤在同一行里会让人
	// 分不清哪一段是「为什么没修」、哪一段是「具体是什么情况」。
	if f.Unrepairable != "" {
		_, _ = fmt.Fprintf(w, "      未修原因：%s\n", f.Unrepairable)
	}
	if f.Detail != "" {
		_, _ = fmt.Fprintf(w, "      详情：%s\n", f.Detail)
	}
}

// withDB 打开数据库、确认连得上，再把句柄交给 fn。
//
// 收一个回调而不是把「打开」与「关闭」拆成两半：每条子命令都有提前返回的
// 分支，而漏掉一处 defer 关闭的表现是连接慢慢漏光——那要等到线上跑了一阵
// 才现形。放在一个函数里，关闭与打开就是同一段代码。
//
// timeout 由调用方给：各条命令的合理上限不一样（迁移要几分钟，一条单行
// 插入 30 秒就够），而把它们统一成一个数会让短的那条失去意义、
// 或者让长的那条被无故掐断。
//
// 打开与探活复用 openDB，不在这里再写一遍：那两件事失败时的处置相同
// （都归 exitDBUnreach），各写一遍的表现是同一类失败在两条命令上给出
// 不同的退出码，而脚本对它们的处置是不一样的。
func withDB(timeout time.Duration, fn func(context.Context, *sql.DB) error) error {
	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	return fn(ctx, db)
}

// allowMissingFlagNames 是 migrate up 的开关 --allow-missing 认的写法。
//
// 单横线的长写法一并认，理由与全局开关相同（见 globals.go 的 takeBoolFlag）。
var allowMissingFlagNames = []string{"--allow-missing", "-allow-missing"}

// takeAllowMissingFlag 从 migrate 的参数里摘出 --allow-missing。
func takeAllowMissingFlag(args []string) (bool, bool, []string, error) {
	return takeBoolFlag(args, allowMissingFlagNames, "--allow-missing")
}

// migrateActions 是 migrate 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var migrateActions = []string{"up", "down", "down-to", "up-to", "status", "version", "create"}

func runMigrate(args []string) error {
	// 开关在 fs.Parse 之前摘掉：flag 包在第一个非选项参数（子命令）处停下，
	// 所以 `migrate up --allow-missing` 里的它根本到不了 fs。
	allowMissingSet, allowMissing, args, flagErr := takeAllowMissingFlag(args)
	if flagErr != nil {
		return flagErr
	}

	fs := newFlagSet("migrate", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, migrateShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New("migrate 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	// 子命令与它吃的取值先校验掉，**再连库**：`migrate bogus` 与
	// `migrate down-to abc` 都是用户输入错，报出来的应该是「改命令」（退出码 2）。
	// 把它们留到连库之后判，库一连不上就会盖成「连不上库」（3），脚本按
	// 退出码分派时走错分支——一个该改命令的错被当成「稍后重试」。
	//
	// 先只认子命令，再判开关，最后才判它吃的取值：开关只属于 up，回滚、按版本走、
	// 看状态都不该出现「补应用」这个动作，所以 `create --allow-missing` 报的是
	// 用错的那个参数。判的是**参数里有没有它**，不是它的取值——
	// `migrate down --allow-missing=false` 仍然是用错了子命令。
	switch action {
	case "create", "up", "down", "status", "version", "down-to", "up-to":
	default:
		return usageErr(fmt.Errorf("未知的 migrate 子命令 %q", action))
	}
	if allowMissingSet && action != "up" {
		return usageErr(fmt.Errorf("--allow-missing 只用于 migrate up：%s 不接受它", action))
	}

	var target *int64
	switch action {
	case "create":
		if len(rest) < 1 {
			return usageErr(errors.New("用法：larkaxctl migrate create <名称>"))
		}
	case "up", "down", "status", "version":
		// 这四条不吃取值。
	case "down-to", "up-to":
		ver, verErr := parseVersion(action, rest)
		if verErr != nil {
			return usageErr(verErr)
		}
		target = ver
	}

	// migrate 的每一条子命令都还没接机器可读输出：它的输出是 goose 自己打的
	// （goose.RunContext 往 stdout 直接写「goose: no migrations to run」这类
	// 行），要接 JSON 得先把它那份 logger 收住，那是另一件事。在那之前，
	// `migrate up -o json` 报参数错，而不是打一半 goose 的日志再打一半 JSON。
	if jsonErr := jsonUnsupported("migrate " + action); jsonErr != nil {
		return jsonErr
	}

	if action == "create" {
		// 第二个参数固定为 sql。goose 默认生成 .go 迁移，而内嵌规则是
		// sql/*.sql，生成的 Go 文件既不会被执行，又会在迁移目录里多出一个包。
		//
		// create 只写文件，不碰数据库（第二个参数为 nil），所以这里没有可传的
		// context；RunContext 在 db 为 nil 时同样只做文件操作。
		if err := goose.RunContext(context.Background(), "create", nil, migrationsDir, rest[0], "sql"); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStderr(), "已生成到 %s/\n", migrationsDir)
		return nil
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	// 变量名不复用 err：这里的任何一次重新声明都会让后面读代码的人
	// 停下来判断「看到的是哪个 err」，而包一层名字的成本是零。
	dialectErr := migrate.SetDialect("postgres")
	if dialectErr != nil {
		return fmt.Errorf("设置迁移方言失败: %w", dialectErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// 走 openDB 而不是自己 sql.Open + Ping：打开与探活失败时的处置相同
	// （都归 exitDBUnreach），各写一遍的表现是同一类失败在两条命令上给出
	// 不同的退出码，而脚本对它们的处置是不一样的。
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrate.FS())

	switch action {
	case "up":
		if allowMissing {
			return gooseUpAllowMissing(ctx, db)
		}
		return gooseRun(ctx, db, action)
	case "down", "status", "version":
		return gooseRun(ctx, db, action)
	case "down-to":
		return stepTo(ctx, db, *target, false)
	case "up-to":
		return stepTo(ctx, db, *target, true)
	}
	return nil
}

// stepTo 一步一步地前进或回滚到目标版本。
//
// 不用 goose 自带的 up-to / down-to：实测它们会停在目标版本的相邻位置
// （从 v2 执行 down-to 0 只退到 v1），差一位的表现比不支持更危险——
// 使用者会以为已经回滚干净了。自己循环，行为就是「版本号到目标为止」，
// 与直觉一致，也好测。
func stepTo(ctx context.Context, db *sql.DB, target int64, forward bool) error {
	for {
		current, err := goose.GetDBVersionContext(ctx, db)
		if err != nil {
			return fmt.Errorf("读取当前版本失败: %w", err)
		}

		if forward {
			if current >= target {
				return nil
			}
			if err := goose.UpByOneContext(ctx, db, "."); err != nil {
				return fmt.Errorf("前进到 %d 时失败: %w", target, err)
			}
			continue
		}

		if current <= target {
			return nil
		}
		if err := goose.DownContext(ctx, db, "."); err != nil {
			return fmt.Errorf("回滚到 %d 时失败: %w", target, err)
		}
	}
}

func gooseRun(ctx context.Context, db *sql.DB, action string) error {
	if err := goose.RunContext(ctx, action, db, "."); err != nil {
		return fmt.Errorf("执行 %s 失败: %w", action, err)
	}
	return nil
}

// gooseUpAllowMissing 让 goose 补应用编号比当前版本小、此前漏掉的迁移，
// 再应用所有更大的未应用迁移。
//
// 那份清单先由自己打一遍：goose 只逐条打 `OK <文件>`，从里面看不出哪一条是
// 补应用的乱序迁移、哪一条是正常前进。清单与 goose 的 findMissingMigrations
// 是同一组条件，读的是同一张版本表，所以两边的名单不会分家。
func gooseUpAllowMissing(ctx context.Context, db *sql.DB) error {
	missing, err := missingMigrations(ctx, db)
	if err != nil {
		return fmt.Errorf("读取待补应用的乱序迁移失败: %w", err)
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(humanStderr(), "将补应用 %d 条编号更小、此前漏掉的迁移：%s\n",
			len(missing), strings.Join(missing, "、"))
	}
	if err := goose.UpContext(ctx, db, ".", goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("执行 up 失败: %w", err)
	}
	return nil
}

// missingMigrations 返回编号比库里最大版本小、却没有任何应用记录的迁移文件，
// 按版本升序。
//
// 判据照抄 goose 在 WithAllowMissing 生效时用的那一条：版本小于库里的最大版本，
// 且版本表里没有它的行。表不存在时还没有任何迁移跑过，自然没有「漏掉」的。
func missingMigrations(ctx context.Context, db *sql.DB) ([]string, error) {
	exists, err := gooseVersionTableExists(ctx, db)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	migrations, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, "SELECT version_id FROM "+goose.TableName())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int64]bool, len(migrations))
	var maxVersion int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
		if version > maxVersion {
			maxVersion = version
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing []string
	for _, m := range migrations {
		if !applied[m.Version] && m.Version < maxVersion {
			missing = append(missing, filepath.Base(m.Source))
		}
	}
	return missing, nil
}

func parseVersion(action string, args []string) (*int64, error) {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return nil, fmt.Errorf("用法：larkaxctl migrate %s <版本>", action)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil || v < 0 {
		return nil, fmt.Errorf("%s 的版本号必须是非负整数，当前为 %q", action, args[0])
	}
	return &v, nil
}
