package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
)

const pluginUsage = `larkaxctl plugin 管理插件。

用法：
  larkaxctl plugin list
  larkaxctl plugin publish <文件> [--key <键>] [--priority <整数>]
  larkaxctl plugin enable <key>
  larkaxctl plugin disable <key>

插件的 key 与版本写在源码里（export const meta），所以 publish 默认不要求
--key；只有想让注册名与源码里声明的不一致时才需要它——那种情况下两条路会
对不上，命令会拒绝。

publish **先在沙箱里编译一遍再落库**：一个编译不过的插件不许进库，否则加载器
每次启动都要处理它。编译失败时命令以非零退出，库里不会多出一行。

同一个 (key, version) 重复发布是**覆盖**：源码、元数据、名称换新，代次
（generation）加一，启用状态保持原值——改一个笔误不该让已启用的插件下线。
换版本请改源码里的 meta.version，那会插入一行新代次，状态是 pending，
要 enable 才生效。

**本版没有热更新**：publish / enable / disable 都只改库，跑着的进程要重启
才会重新加载。

退出码与其余子命令一致；key 在库里没有对应行时按「约束冲突」返回 4。
`

// pluginTimeout 是插件命令连库与写库的时间上限。
//
// 与 channelTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。给得比渠道那条宽：发布要先在沙箱里编译一遍，而编译一个
// 「干实事」的插件要跑一次它的顶层。
const pluginTimeout = 60 * time.Second

func runPlugin(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, pluginUsage)
		return usageErr(errors.New("plugin 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "list":
		return runPluginList(rest)
	case "publish":
		return runPluginPublish(rest)
	case "enable":
		return runPluginSetEnabled(rest, true)
	case "disable":
		return runPluginSetEnabled(rest, false)
	case "help", "-h", "--help":
		fmt.Print(pluginUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, pluginUsage)
		return usageErr(fmt.Errorf("未知的 plugin 子命令 %q", action))
	}
}

func runPluginList(args []string) error {
	fs := flag.NewFlagSet("plugin list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, pluginUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		rows, err := pluginstore.New(db).List(ctx)
		if err != nil {
			return err
		}
		// JSON 是**另一条渲染路径**（见 format.go 开头）：表格那一档把坏掉的
		// meta 折成表后的一行说明，这一档给 meta_problem 那一栏。
		if jsonOutput() {
			return emitJSON(os.Stdout, pluginListDoc(rows), nil)
		}
		printPluginList(humanStdout(), rows)
		return nil
	})
}

// printPluginList 打出插件清单。
//
// 表头用**库里的列名**（key / version / status / generation），不翻译：
// 这些名字在 psql 里、在迁移文件里、在这里是同一个东西，翻译一遍就多一层
// 对应关系要记。给人看的说明在 --help 里。
func printPluginList(w io.Writer, rows []pluginstore.Row) {
	if len(rows) == 0 {
		// 明确打一句，而不是什么都不输出：「暂无插件」与「命令因为某个
		// 原因没跑起来」在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
		_, _ = fmt.Fprintln(w, emptyListNote("插件", "plugin publish"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("KEY"), p.bold("VERSION"), p.bold("STATUS"), p.bold("GENERATION"),
		p.bold("PRIORITY"), p.bold("NAME"))
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
			p.bold(row.Key), row.Version, row.Status, row.Generation, row.Priority, row.Name)
	}
	_ = tw.Flush()

	// meta 坏掉的行单独说明，不跳过也不整次报错：库里有**一行**坏数据时整个
	// 清单一条都看不到，而那正是最需要看清单的时候（同 channels 的凭证那一栏）。
	// 它会坏是因为 meta 是 jsonb——语法由列类型保证，而**形状**（是不是一个
	// 对象、有没有 key、key 与行上的对不对得上）没有任何约束拦得住。
	for _, row := range rows {
		if row.MetaProblem != "" {
			_, _ = fmt.Fprintf(w, "插件 %s 的 meta 有问题：%s\n", row.Key, row.MetaProblem)
		}
	}
}

// pluginListDoc 组装 plugin list 的机器可读输出。
//
// 与 printPluginList 是同一份数据的另一条渲染路径（见 format.go 开头）。
// 键名用**库里的列名**，与那张表的表头同一条理由：它们在 psql 里、在迁移
// 文件里、在这里是同一个东西。
func pluginListDoc(rows []pluginstore.Row) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(rows)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, pluginEntry(row))
	}
	doc["plugins"] = entries
	return doc
}

// pluginEntry 打清单里的一个插件。
//
// meta_problem 是那一行的 meta 快照有什么问题，没问题时是 null：它在表格那一档
// 折成表后的一行说明，在这里必须落进**这一条自己**——脚本问的是「哪几个插件的
// meta 有问题」，而表后那几行是给人读的。
func pluginEntry(row pluginstore.Row) map[string]any {
	return map[string]any{
		"key":        row.Key,
		"version":    row.Version,
		"status":     row.Status,
		"generation": row.Generation,
		"priority":   row.Priority,
		"name":       row.Name,
		// 坏掉的那一行照样在清单里（见 printPluginList 的注释）：整次报错或
		// 跳过它，都会让最需要看清单的那一刻看不到东西。
		"meta_problem": nullableString(row.MetaProblem),
	}
}

func runPluginPublish(args []string) error {
	// 在**函数体第一行**拦下来，早于读文件、更早于连库与落库：publish 会写进
	// plugins 表，做完再报不支持就成了一次静默的写入（见 format.go）。
	if err := jsonUnsupported("plugin publish"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plugin publish", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, pluginUsage) }
	key := fs.String("key", "", "插件 key；省略时取源码 meta 里声明的那个")
	priority := fs.Int("priority", 0, "同一 key 的优先级，越大越先被选中")
	if err := parseFlagsAnywhere(fs, args); err != nil {
		return usageErr(err)
	}
	switch {
	case fs.NArg() == 0:
		fs.Usage()
		return usageErr(errors.New("缺少插件源文件路径"))
	case fs.NArg() > 1:
		return usageErr(fmt.Errorf("publish 只接受一个源文件，多出 %q", fs.Arg(1)))
	}
	path := fs.Arg(0)

	source, err := os.ReadFile(path)
	if err != nil {
		// 路径写错了是参数问题，与「文件在那儿但读不动」（权限、磁盘）分开：
		// 前者的下一步是改命令，后者是看环境。脚本按退出码分派，混成一个数
		// 就只能去匹配错误文本了。
		if errors.Is(err, os.ErrNotExist) {
			return usageErr(fmt.Errorf("读取插件源文件失败: %w", err))
		}
		return fmt.Errorf("读取插件源文件失败: %w", err)
	}

	// 先编译再落库。这一步与加载器走的是同一段代码（plugin.Compile*），
	// 所以「这里说能用」与「线上真能用」不会各说各话。
	//
	// 两条路只差注册名从哪来：给了 --key 就用它（并要求与 meta 里的 key 一致），
	// 没给就取 meta 里声明的那个。
	var compiled *plugin.Plugin
	if *key == "" {
		compiled, err = plugin.CompileSource(path, string(source), *priority)
	} else {
		compiled, err = plugin.Compile(plugin.Registration{
			Key: *key, Source: string(source), Priority: *priority,
		})
	}
	if err != nil {
		// 分类后再打。沙箱给出的错误里已经带上了种类（元数据不合法、
		// 顶层超时、内存超限、没有默认导出），而它**不带**引擎的调用栈
		// ——栈对上传的人没有用，他要的是「改哪一处」。
		return fmt.Errorf("插件编译失败（%s）：%w", compileErrorKind(err), err)
	}
	// version 是行的一部分（UNIQUE (key, version)），也是代次之间的区分。
	// 空着的话所有版本会挤成同一个 (key, '')，第二次发布就变成覆盖第一代，
	// 而那种「怎么发都是同一行」在清单上要盯着 generation 才看得出来。
	if strings.TrimSpace(compiled.Meta.Version) == "" {
		return usageErr(fmt.Errorf(
			"插件 %s 的 meta 里没有 version：它在源码里声明（export const meta = { version: \"1.0.0\", … }）",
			compiled.Key))
	}

	pub, err := pluginstore.NewPublication(compiled, string(source))
	if err != nil {
		return err
	}

	var res pluginstore.Result
	err = withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		var pubErr error
		res, pubErr = pluginstore.New(db).Publish(ctx, pub)
		return pubErr
	})
	if err != nil {
		return err
	}

	action := "已发布"
	if res.Overwritten {
		action = "已覆盖（同一版本重新发布）"
	}
	_, _ = fmt.Fprintf(humanStdout(), "插件 %s %s，版本 %s，第 %d 代，状态 %s。重启后生效。\n",
		res.Key, action, res.Version, res.Generation, res.Status)
	if res.Status != pluginstore.StatusActive {
		_, _ = fmt.Fprintf(humanStdout(), "它还没有生效：%s 不参与注册，用 `larkaxctl plugin enable %s` 启用。\n",
			pluginstore.StatusPending, res.Key)
	}
	return nil
}

// compileErrorKind 给编译失败分一个类。
//
// 分类不是为了让文案好看：上传的人拿到一句「编译失败」之后要做的第一件事是
// 判断改哪儿——元数据缺 key、申请了没有的能力、顶层跑超时、源码语法错，
// 四处改的地方完全不同。分类来自**沙箱与注册表的哨兵错误**，不是去匹配
// 引擎的文案（文案会随引擎版本变）。
func compileErrorKind(err error) string {
	switch {
	case errors.Is(err, plugin.ErrMetaMissingKey):
		return "元数据缺少 key"
	case errors.Is(err, plugin.ErrMetaAPIVersion):
		return "契约版本不受支持"
	case errors.Is(err, plugin.ErrMetaCapability):
		return "申请了宿主未开放的能力"
	case errors.Is(err, sandbox.ErrNoDefaultExport):
		return "没有 export default"
	case errors.Is(err, sandbox.ErrMemoryLimit):
		return "求值顶层超出内存上限"
	case errors.Is(err, sandbox.ErrCallTimeout):
		return "求值顶层超时"
	default:
		return "源码编译不过"
	}
}

// parseFlagsAnywhere 解析开关，允许它们写在位置参数**之后**。
//
// 标准库的 flag 遇到第一个位置参数就停止解析，于是
// `plugin publish foo.js --key bar` 里的 --key 会被当成第二个位置参数，
// 报一句「只接受一个源文件」。而那个写法恰恰是用法里印出来的那一种，
// 也是所有人第一次会敲的那一种——命令不该因为参数顺序不同而表现不同。
//
// 仓库里别的子命令没有这个问题，因为它们不收位置参数（渠道用 --id 指定）。
// 这个 helper 只有 publish 用，所以留在本文件里，不往公共处放。
//
// 判断一个开关吃不吃值靠 IsBoolFlag：不吃值的开关后面那个词是位置参数，
// 跟着一起挪会把文件名当成开关的值。
func parseFlagsAnywhere(fs *flag.FlagSet, args []string) error {
	flags := make([]string, 0, len(args))
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// 显式分隔符：从它往后全是位置参数，连同它一起交给 flag 收尾。
			return fs.Parse(append(flags, args[i:]...))
		}
		name, isFlag := flagName(arg)
		if !isFlag {
			rest = append(rest, arg)
			continue
		}
		flags = append(flags, arg)
		// 已经带了 = 值的（--key=bar）自带值，不吃下一个词。
		if strings.ContainsRune(arg, '=') || isBoolFlag(fs, name) {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return fs.Parse(append(flags, rest...))
}

// flagName 判断一个参数是不是开关，是的话给出它的名字（去掉前导 - 与 =值）。
func flagName(arg string) (string, bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", false
	}
	name := strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name, name != ""
}

// isBoolFlag 问一个开关是不是布尔开关（即不取值）。
//
// 未声明的名字按「取值」处理：多挪一个词进去会让 flag 报一句「flag provided
// but not defined」，比把文件名吃掉更容易看懂。
func isBoolFlag(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func runPluginSetEnabled(args []string, enable bool) error {
	verb, done := "enable", "已启用"
	if !enable {
		verb, done = "disable", "已停用"
	}
	// 命令名按 enable 算出来：这两条命令共用这一个入口，写死一个会让其中一条
	// 在不支持 json 时报出兄弟命令的名字。拦在**函数体第一行**、连库之前——
	// 它会改 plugin_activations，做完再报不支持就成了一次静默的启用/停用。
	if err := jsonUnsupported("plugin " + verb); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plugin "+verb, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, pluginUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return usageErr(fmt.Errorf("%s 需要一个插件 key，得到 %d 个参数", verb, fs.NArg()))
	}
	key := strings.TrimSpace(fs.Arg(0))
	if key == "" {
		return usageErr(fmt.Errorf("%s 的插件 key 不能为空", verb))
	}

	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		s := pluginstore.New(db)
		var err error
		if enable {
			_, err = s.Enable(ctx, key)
		} else {
			_, err = s.Disable(ctx, key)
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStdout(), "插件 %s %s。重启后生效。\n", key, done)
		return nil
	})
}
