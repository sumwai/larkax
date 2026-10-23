package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/plans"
	"github.com/sumwai/larkax/internal/snapshot"
	"github.com/sumwai/larkax/internal/snapshotwire"
	"github.com/sumwai/larkax/internal/upstream"
)

// completionHelp 是 completion 的两版帮助文本（见 helpText）。
var completionHelp = helpText{short: completionShortUsage, long: completionUsage}

// completionShortUsage 是 completion 的短版：`-h` / `--help` 与参数写错时打的那一份。
const completionShortUsage = `larkaxctl completion 打印 shell 补全脚本。

用法：
  larkaxctl completion bash      补全命令、子命令与长选项
  larkaxctl completion zsh
  larkaxctl completion fish

当前会话生效：source <(larkaxctl completion bash)
写进配置文件：larkaxctl completion bash > ~/.local/share/bash-completion/completions/larkaxctl

更细的边界：larkaxctl help completion。
`

// completionUsage 是 completion 的长版：`larkaxctl help completion` 与
// `completion help`。
const completionUsage = `larkaxctl completion 打印 shell 补全脚本。

用法：
  larkaxctl completion bash
  larkaxctl completion zsh
  larkaxctl completion fish

脚本由命令行自己的注册表生成：一级命令来自根帮助页那一份，二级动作来自各命令
的分发表，长选项来自各命令自己注册的 flag 集。产物只打到 stdout，装到哪里由
使用者决定：

  bash   source <(larkaxctl completion bash)
         larkaxctl completion bash > ~/.local/share/bash-completion/completions/larkaxctl
  zsh    larkaxctl completion zsh > ~/.zfunc/_larkaxctl
  fish   larkaxctl completion fish > ~/.config/fish/completions/larkaxctl.fish

补全只列**长选项**的正式写法；短选项与旧名仍能敲，只是不占 Tab 的位置。

更细的约定见 docs/cli-reference.md 的「completion」一节。
`

// completionShells 是补全支持的 shell。校验（参数错时列出的可选项）与生成
// 分派共用这一份清单。
var completionShells = []string{"bash", "zsh", "fish"}

// completionCommandRun 是 completion 这条命令的处理函数。
//
// 它不直接在注册表里写 runCompletion：commandGroups 是包级初值，而补全要从
// 这张表读一级命令，直接引用会形成初始化环
// （commandGroups → runCompletion → dispatch → lookupCommand → commandGroups）。
// 注册表里放下面那个转发函数，真正的处理函数在这个变量上接一次，环就断在这里。
var completionCommandRun func([]string) error

// runCompletionCommand 是注册表里的那一条：调用时再取 completionCommandRun，
// 取到的才是 init 装上的那个。
func runCompletionCommand(args []string) error { return completionCommandRun(args) }

func init() { completionCommandRun = runCompletion }

// runCompletion 打印一个 shell 的补全脚本。
func runCompletion(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, completionShortUsage)
		return usageErr(errors.New("completion 需要 shell 名"))
	}
	switch args[0] {
	case "-h", "--help":
		printUsage(os.Stdout, completionShortUsage)
		return nil
	}

	shell := args[0]
	if !slices.Contains(completionShells, shell) {
		printUsage(os.Stderr, completionShortUsage)
		return usageErr(fmt.Errorf("未知的 shell %q（可选：%s）",
			shell, strings.Join(completionShells, "、")))
	}
	if len(args) > 1 {
		return usageErr(fmt.Errorf("completion %s 不接受多余参数，得到 %q", shell, args[1]))
	}

	data := buildCompletionData()
	var script string
	switch shell {
	case "bash":
		script = bashCompletionScript(data)
	case "zsh":
		script = zshCompletionScript(data)
	case "fish":
		script = fishCompletionScript(data)
	}
	_, _ = fmt.Fprint(os.Stdout, script)
	return nil
}

// completionData 是生成补全脚本要的全部数据。
type completionData struct {
	// commands 是一级命令，顺序与根帮助页一致。
	commands []completionCommand
	// words 按路径列出下一层能接哪些词：键 "" 是全部一级命令，
	// `channel` 是它的动作，`channel map` 是三级动词。
	words map[string][]string
	// flags 以命令路径为键，值是该路径注册过的选项名（含旧名与别名）。
	flags map[string][]string
	// enums 以「命令路径|--选项名」为键，值是那个选项的合法取值。
	enums map[string][]string
}

// completionCommand 是一条一级命令在补全里的样子。
type completionCommand struct {
	name    string
	summary string
}

// completionActionTable 把命令路径接到它自己的动作常量上。
//
// 值全部是各命令分发表旁边的那份常量（见 channel.go 的 channelActions 这一类），
// 这里只做接线，不抄任何名字：清单只有这一份；分派在各自的 switch 里。
// 键是路径，层级由空格分开。
var completionActionTable = map[string][]string{
	"admin":           adminActions,
	"admin token":     adminTokenActions,
	"settings":        settingsActions,
	"channel":         channelActions,
	"channel map":     channelMapActions,
	"model":           modelActions,
	"model alias":     modelAliasActions,
	"alias":           aliasActions,
	"price":           priceActions,
	"price calendar":  priceCalendarActions,
	"upstream-cost":   upstreamCostActions,
	"user":            userActions,
	"user delete":     userDeleteActions,
	"user plan":       userPlanActions,
	"user plan order": userPlanOrderActions,
	"user wallet":     userWalletActions,
	"user key":        userKeyActions,
	"user key plans":  userKeyPlansActions,
	"user usage":      userUsageActions,
	"apikey":          apiKeyActions,
	"wallet":          walletActions,
	"wallet payout":   walletPayoutActions,
	"wallet payment":  walletPaymentActions,
	"plan":            planActions,
	"plan key":        planKeyActions,
	"plan order":      planOrderActions,
	"usage":           usageActions,
	"ledger":          ledgerActions,
	"migrate":         migrateActions,
	"partition":       partitionActions,
	"secret":          secretActions,
	"maintenance":     maintenanceActions,
	"snapshot":        snapshotActions,
	"consent":         consentActions,
	"plugin":          pluginActions,
	"plugin kv":       pluginKVActions,
	"idempotency":     idempotencyActions,
	"calendar":        calendarActions,
}

// buildCompletionData 从注册表与各命令的动作常量组装补全数据。
func buildCompletionData() completionData {
	var commands []completionCommand
	top := make([]string, 0, len(commandGroups))
	for _, g := range commandGroups {
		for _, c := range g.commands {
			commands = append(commands, completionCommand{name: c.name, summary: c.summary})
			top = append(top, c.name)
		}
	}
	for _, c := range legacyCommands {
		commands = append(commands, completionCommand{name: c.name})
		top = append(top, c.name)
	}

	words := make(map[string][]string, len(completionActionTable)+1)
	words[""] = top
	for path, verbs := range completionActionTable {
		words[path] = verbs
	}

	return completionData{
		commands: commands,
		words:    words,
		flags:    collectCommandFlags(completionPaths(commands)),
		enums:    completionEnumValues(),
	}
}

// completionPaths 列出要遍历的命令路径。
//
// migrate 只走一级：它的动作不吃自己的 flag 集，而 `-h` 落在动作之后会被当成
// 位置参数（`migrate create -h` 会真的写一个迁移文件），所以不往动作走。
func completionPaths(commands []completionCommand) [][]string {
	paths := make([][]string, 0, len(commands)*4)
	seen := map[string]bool{}
	add := func(path ...string) {
		key := strings.Join(path, " ")
		if seen[key] {
			return
		}
		seen[key] = true
		paths = append(paths, path)
	}
	for _, c := range commands {
		if c.name == "completion" {
			continue
		}
		add(c.name)
	}
	for path, verbs := range completionActionTable {
		parts := strings.Split(path, " ")
		if len(parts) == 0 || parts[0] == "migrate" || parts[0] == "completion" {
			continue
		}
		add(parts...)
		for _, verb := range verbs {
			add(append(append([]string{}, parts...), verb)...)
		}
	}
	return paths
}

// 收集 flag 名期间用的几个包级状态（见 flagset.go 的 record）。
var (
	collectingCommandFlags bool
	currentCommandPath     string
	collectedFlags         map[string][]string
)

// collectCommandFlags 把每条命令路径按 `<路径> -h` 走一遍，记下它注册的选项。
//
// `-h` 让 flag 在解析阶段返回 ErrHelp，命令走不到连库与写库；万一有哪条路径
// 在解析之前就要连库，openDB 在收集期间直接失败（见 db.go），
// 不会真的碰到数据库。输出一律丢掉：这里要的只是 flag 注册这件副作用。
func collectCommandFlags(paths [][]string) map[string][]string {
	collectedFlags = make(map[string][]string, len(paths))
	collectingCommandFlags = true

	savedOut, savedErr := os.Stdout, os.Stderr
	savedFormat, savedVerbose, savedQuiet := outputFormat, verboseOutput, quietOutput
	devNull, openErr := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if openErr == nil {
		os.Stdout, os.Stderr = devNull, devNull
	}
	// 收集期间按默认档走：`-o json` 会让写类命令在解析参数之前就返回，
	// 那样它们的 flag 集根本不会注册。
	outputFormat, verboseOutput, quietOutput = formatTable, false, false

	for _, path := range paths {
		currentCommandPath = strings.Join(path, " ")
		// 错误一律忽略：不认 `-h` 的那几条路径本来就不会注册 flag 集。
		_ = dispatch(append(append([]string{}, path...), "-h"))
	}

	outputFormat, verboseOutput, quietOutput = savedFormat, savedVerbose, savedQuiet
	os.Stdout, os.Stderr = savedOut, savedErr
	if devNull != nil {
		_ = devNull.Close()
	}
	collectingCommandFlags = false
	return collectedFlags
}

// completionOptionNames 返回一条命令路径要补全的长选项：它自己注册的那些，
// 加上全局选项。
//
// 注册过的名字里混着短选项与旧名（`u`、`m`、`owner` 这类）。短名字只有一两个
// 字母，写在 Tab 里反而把真正要选的那几个挤走；旧名不占 Tab 的位置（见帮助
// 文本末段）。所以只列正式的长选项。
func completionOptionNames(data completionData, path string) []string {
	out := ownOptionNames(data, path)
	out = append(out, globalOptionNames()...)
	slices.Sort(out)
	return slices.Compact(out)
}

// ownOptionNames 是这条路径自己注册的正式长选项（不含全局选项），按字典序。
func ownOptionNames(data completionData, path string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range data.flags[path] {
		if len(name) < 2 || strings.HasPrefix(name, "-") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, "--"+name)
	}
	slices.Sort(out)
	return out
}

// globalOptionNames 从 usageNotes 的「全局选项」一节取长选项名。
//
// 那一节是全局选项的唯一对外清单；另抄一份的话，加一个全局选项时补全会安静地
// 少一条。
func globalOptionNames() []string {
	section := usageNotes
	if i := strings.Index(section, "全局选项"); i >= 0 {
		section = section[i:]
	}
	if i := strings.Index(section, "\n\n"); i >= 0 {
		section = section[:i]
	}
	matches := longOptionRE.FindAllString(section, -1)
	slices.Sort(matches)
	return slices.Compact(matches)
}

// longOptionRE 认一个长选项。
var longOptionRE = regexp.MustCompile(`--[a-z][a-z0-9-]*`)

// completionEnumValues 是各选项的合法取值，键是「命令路径|--选项名」。
//
// 取值一律取自校验那一侧已有的来源（常量或清单函数），这里不抄字面量：
// 抄一份的表现是库里加了一种取值，而 Tab 补出来的还是旧的那几项。
// 有来源不等于已经接线：没接上的那几格留白——留白比补错安全，接线另算。
func completionEnumValues() map[string][]string {
	values := map[string][]string{}
	put := func(path, flag string, vals []string) {
		values[path+"|--"+flag] = vals
	}

	statuses := auth.KeyStatuses()
	for _, path := range []string{"apikey create", "apikey update"} {
		put(path, "status", statuses)
	}

	roles := auth.Roles()
	for _, path := range []string{"user create", "user list", "user update"} {
		put(path, "role", roles)
	}
	for _, path := range []string{"user list", "user update"} {
		put(path, "status", auth.UserStatuses())
	}

	if registry, err := upstream.Registry(); err == nil {
		// --protocols 收逗号分隔的协议集合，Tab 一次补一个名字，多选的写法照旧手敲。
		for _, path := range []string{"channel create", "channel update"} {
			put(path, "type", registry.Types())
			put(path, "protocols", registry.Types())
		}
	}

	roots := make([]string, 0, len(snapshot.Roots()))
	for _, root := range snapshot.Roots() {
		roots = append(roots, string(root))
	}
	surfaces := make([]string, 0, len(snapshot.Surfaces()))
	for _, surface := range snapshot.Surfaces() {
		surfaces = append(surfaces, string(surface))
	}
	for _, path := range []string{"snapshot allowlist", "snapshot get", "snapshot redact"} {
		put(path, "direction", roots)
		put(path, "surface", surfaces)
	}

	put("consent grant", "scope", []string{
		snapshotwire.ConsentScopeSingleRequest, snapshotwire.ConsentScopeTimeWindow,
	})
	put("plan void", "status", []string{
		string(plans.UserPlanExpired), string(plans.UserPlanCancelled),
	})

	asset := []string{string(ledger.AssetCNY)}
	for _, path := range []string{
		"wallet create", "wallet credit", "wallet adjust", "wallet refund",
		"wallet payment create", "wallet payout request",
		"plan create", "ledger balance",
		"user wallet credit", "user wallet adjust", "user wallet refund",
	} {
		put(path, "asset", asset)
	}

	return values
}
