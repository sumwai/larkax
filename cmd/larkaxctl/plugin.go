package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
)

// pluginHelp 是 plugin 的两版帮助文本（见 helpText）。
var pluginHelp = helpText{short: pluginShortUsage, long: pluginUsage}

// pluginShortUsage 是 plugin 的短版：`-h` / `--help` 与参数写错时打的那一份。
const pluginShortUsage = `larkaxctl plugin 管理插件。

用法：
  larkaxctl plugin list
  larkaxctl plugin get <key>
  larkaxctl plugin publish <文件> [--key <键>] [--priority <整数>]
  larkaxctl plugin enable <key>
  larkaxctl plugin disable <key>
  larkaxctl plugin delete <key>
  larkaxctl plugin kv get --plugin <key> --user <用户> --key <键>
  larkaxctl plugin kv set --plugin <key> --user <用户> --key <键> --value <值> [--ttl <时长>]
  larkaxctl plugin kv delete --plugin <key> --user <用户> --key <键>

publish 先在沙箱里编译一遍再落库，编译不过的不进库。同一个 (key, version)
重复发布是覆盖；换版本要改源码里的 meta.version，并 enable 一次。delete 把
插件删干净：每一代、启用记录，连同它私有的存储一起。

更细的边界：larkaxctl help plugin。
`

// pluginUsage 是 plugin 的长版：`larkaxctl help plugin` 与 `plugin help`。
const pluginUsage = `larkaxctl plugin 管理插件。

用法：
  larkaxctl plugin list
  larkaxctl plugin get <key>
  larkaxctl plugin publish <文件> [--key <键>] [--priority <整数>]
  larkaxctl plugin enable <key>
  larkaxctl plugin disable <key>
  larkaxctl plugin delete <key>
  larkaxctl plugin kv get --plugin <key> --user <用户> --key <键>
  larkaxctl plugin kv set --plugin <key> --user <用户> --key <键> --value <值> [--ttl <时长>]
  larkaxctl plugin kv delete --plugin <key> --user <用户> --key <键>

插件的 key 与版本写在源码里（export const meta），所以 publish 默认不要求
--key；只有想让注册名与源码里声明的不一致时才需要它——那种情况下两条路会
对不上，命令会拒绝。

publish **先在沙箱里编译一遍再落库**：编译不过的命令以非零退出，库里不会多出
一行。

同一个 (key, version) 重复发布是**覆盖**：源码、元数据、名称换新，代次
（generation）加一，启用状态保持原值。换版本请改源码里的 meta.version，那会
插入一行新代次，状态是 pending，要 enable 才生效。

get 看的是**最新发布的那一代**（代次最高的那一行），并说出这个 key 下还有几个
更老的版本。它与**跑着的那一代**不是同一行：加载器在同一个 key 里取的是状态为
active 且代次最高的那一行，所以发布了新版本还没 enable 时，get 打的是那一版新的
（pending），实际跑的仍是上一代——它的代次与版本在「在跑」那一行上。

「在跑」那一栏答「这一刻装着的到底是不是这一代」，取值是 / 否 / 还没试三档，而
「否」与「还没试」各有两种来路。取中的正是这一代、上一次加载也装上了才写「是」；
取中的正是这一代、上一次加载跳过了它写「否」，原文在表后那一句里；取中的正是这
一代、却还没有加载记录写「还没试」——取中了不等于装上了。不是取中的那一代写「否」：
同一个 key 里至多一行是取中的那一代。它与 status 不是一回事——enable 新版本时旧
的那一行不会被按下去，于是一个 key 下可能有好几代都写着 active，而取中的只有一行。

加载器每启动一次都重新读一遍参与注册的行，并把这一次的结果写回库里
（last_load_result 那一栏）：装上了是 ok、被跳过的是 failed，连同当时的错误原文
与时刻。两个命令都把它打出来——list 的「上次加载」写在取中的那一代那一行上，
「装上了 / 没装成」是它的两个值，其余各行是「-」，失败时表后逐条给出原文；get 有
「上次加载」一行，失败时再在表后打一句原文。它是**加载期**的结果，与 publish 那
一步无关：publish 根本不收编译不过的插件，所以「发布了却没上线」只有这一栏说得
出来。

「上次加载」还没有记录（list 上是「-」）时，下一步分两条路：加载器整条链路与
plugin.enabled 一起装配，这个设置关着时它根本不读 plugins 表，重启多少次都不会
留下记录，先把设置打开；开着时才是「发布或启用之后还没重启过」，重启一次就有了。

**取中的那一代被跳过时，这个 key 什么都不在跑**：加载器在一个 key 里只取一代，
跳过它不会退回更老的那一代。list 那一行的「在跑」因此写着「否」，紧跟的
「上次加载」写着「没装成」，表后那一句把原因原文打出来。

跳过**不改任何状态**，也不改启用记录：那一代的 status 仍是 active，下次启动照样
会再试一次。所以插件没装成时，先把源码或这次的装配调好，再重启一次，不必重新
发布。

delete 是**硬删**：这个 key 在 plugins 里的每一代、plugin_activations 里属于它
的启用记录，以及 plugin_kv 里它私有的键，在一个事务里一起删掉。插件不像渠道
那样有历史数据按 id 引着它（没有别的东西引用 plugins），所以留一行删除标记下来
只会让每一条读路径都得再学一遍「这一行不算数」。

**删掉的私有存储找不回来**：本版没有导出 plugin_kv 的入口，而那张表在插件没了
之后没有第二条路径能碰到（读与删都要键名，而键名是插件自己写的）。删之前先
想清楚。

kv 那一组照宿主给插件的能力铺：get / set / delete，分别对应宿主接口的读、写、
删。每个键按**插件 + 用户**两段隔离，所以三条命令都要给 --user——同名的键在不
同用户下是两个键。--ttl 只认 Go 的时长写法（1h30m、30s），不给或给 0 表示不过期，
负数会被拒（0 已经表达了「不过期」，负数再表达一次只会让两条路各说各话）。

**只读的那几条不要主密钥**：list / get / kv get 在只有库口令的机器上也跑得起来。
delete 与 kv 的 set / delete 会改库，要求 LARKAX_MASTER_KEYS 在场——命令不做
解密，这一条要的是「这台机器带着运维凭证」。

**本版没有热更新**：publish / enable / disable / delete 都只改库，跑着的进程要
重启才会重新加载。

退出码与其余子命令一致；key 在库里没有对应行时按「约束冲突」返回 4。

更细的约定见 docs/cli-reference.md 的「plugin」一节。
`

// pluginKVShortUsage 是 plugin kv 的短版：参数写错与 `plugin kv -h` 时打的那一份。
//
// 单独一份而不是复用 pluginShortUsage：这一组的参数全是旗标，打回 plugin 那一屏
// 会把真正要看的三条用法挤走。
const pluginKVShortUsage = `larkaxctl plugin kv 读写插件私有的键值存储（plugin_kv 表）。

用法：
  larkaxctl plugin kv get --plugin <key> --user <用户> --key <键>
  larkaxctl plugin kv set --plugin <key> --user <用户> --key <键> --value <值> [--ttl <时长>]
  larkaxctl plugin kv delete --plugin <key> --user <用户> --key <键>

键按插件 + 用户两段隔离。get 与 delete 只认没过期的键：一个已经过期的键按
「不存在」处置。--user 收 ID 或用户名。

更细的边界：larkaxctl plugin kv help。
`

// pluginKVUsage 是 plugin kv 的长版：`plugin kv help` 与 `plugin kv --help`。
const pluginKVUsage = `larkaxctl plugin kv 读写插件私有的键值存储（plugin_kv 表）。

用法：
  larkaxctl plugin kv get --plugin <key> --user <用户> --key <键>
  larkaxctl plugin kv set --plugin <key> --user <用户> --key <键> --value <值> [--ttl <时长>]
  larkaxctl plugin kv delete --plugin <key> --user <用户> --key <键>

表的主键是 (plugin_key, user_id, key)，所以一个键的身份是**三段**：换一个插件、
换一个用户，同名的键都是另一个键。三条命令因此都要给全 --plugin 与 --user。

--user 收 users.id 或用户名，与其余命令同一个解析路径（认不出来时报「用户不
存在」）。

**过期的键按不存在处置**：get 说「没有这个键」，delete 也说「没有可见的键」。
过期由数据库的 now() 判（不是进程时钟），ttl 到期之后那些行还躺在表里，只是谁也
看不见——它们由下一次 set 顺手清掉。

--ttl 只认 Go 的时长写法：1h30m、45s、1h。不给或给 0 表示**不过期**，负数会被拒
——0 已经表达了不过期，让负数再表达一次只会多出一条要记的规则。--value 按明文
给，不做解析：它是插件的私有数据，宿主没有立场猜它的类型。

**在命令行上替插件写私有存储是一件要克制的事**：这里写进去的键与插件自己写的
没有区别，插件读到的是一个它没写过的值。它的用处是看插件存了什么、把一个坏键
删掉、以及在排障时摆一个值进去。

删除是不可逆的，而且**没有导出入口**：删之前先 get 看一眼。

更细的约定见 docs/cli-reference.md 的「plugin」一节。
`

// pluginKVHelp 是 plugin kv 的两版帮助文本（见 helpText）。
var pluginKVHelp = helpText{short: pluginKVShortUsage, long: pluginKVUsage}

// pluginTimeout 是插件命令连库与写库的时间上限。
//
// 与 channelTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。给得比渠道那条宽：发布要先在沙箱里编译一遍，而编译一个
// 「干实事」的插件要跑一次它的顶层。
const pluginTimeout = 60 * time.Second

func runPlugin(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, pluginShortUsage)
		return usageErr(errors.New("plugin 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "list":
		return runPluginList(rest)
	case "get":
		return runPluginGet(rest)
	case "publish":
		return runPluginPublish(rest)
	case "enable":
		return runPluginSetEnabled(rest, true)
	case "disable":
		return runPluginSetEnabled(rest, false)
	case "delete":
		return runPluginDelete(rest)
	case "kv":
		return runPluginKV(rest)
	case "-h", "--help":
		printUsage(os.Stdout, pluginShortUsage)
		return nil
	default:
		printUsage(os.Stderr, pluginShortUsage)
		return usageErr(fmt.Errorf("未知的 plugin 子命令 %q", action))
	}
}

func runPluginList(args []string) error {
	fs := flag.NewFlagSet("plugin list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pluginShortUsage) }
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
// 表头前几栏用**库里的列名**（key / version / status / generation），不翻译：
// 这些名字在 psql 里、在迁移文件里、在这里是同一个东西，翻译一遍就多一层
// 对应关系要记。给人看的说明在 --help 里。
//
// 最后两栏是**算出来的**，库里没有这两列。前一个「在跑」答「这一刻真的装着的是
// 哪一代」，取值是 / 否 / 还没试（四档见 runningMark）：同一个 key 里至多一行是
// 「是」，而取中的那一代被跳过时一行都不是——加载器不会退回更老的那一代。它的
// 答案与 status 那一栏不一样：enable 新版本时旧的那一行不会被按下去，于是一个
// key 下可能有好几代都写着 active。两栏都排在库里那几栏之后，不混进它们中间。
//
// 紧随其后的「上次加载」是**加载期**的结果（库里的 last_load_result 那三列，
// 由加载器每次启动时写）：上面那一栏答「取的是哪一代」，它答「上一次启动装上了
// 没有」。它只写在取中的那一代那一行上，其余的行是 `-`——那份记录按 key 给，
// 打在别的行上会被读成「这一代被加载过」。两栏要一起读：被跳过的那一代状态仍是
// active，只有这一栏说得出来。
func printPluginList(w io.Writer, rows []pluginstore.Row) {
	if len(rows) == 0 {
		// 明确打一句，而不是什么都不输出：「暂无插件」与「命令因为某个
		// 原因没跑起来」在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
		_, _ = fmt.Fprintln(w, emptyListNote("插件", "plugin publish"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("KEY"), p.bold("VERSION"), p.bold("STATUS"), p.bold("GENERATION"),
		p.bold("PRIORITY"), p.bold("NAME"), p.bold("在跑"), p.bold("上次加载"))
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			p.bold(row.Key), row.Version, row.Status, row.Generation, row.Priority, row.Name,
			runningMark(row), loadMark(row))
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

	// 上一次加载没装成的插件逐条说明：库里那一栏只说了一个「没装成」，而原因是
	// 加载器交回来的原文，除了这里没有第二条路能读到它（日志里那一条是同一次
	// 失败的另一个副本，不是同一条路）。
	//
	// 「现在哪一代都没在跑」这半句不是修饰：加载器跳过一代之后**不会**退回更老的
	// 那一代（见 internal/plugin 的 PublishPartial），所以这个 key 这一刻什么都不
	// 在跑——不说这一句，读者会以为老的那一代还顶着。
	//
	// 打的是**取中的那一代**：加载器读的就是它，结果也写在它上面。所以同一个
	// key 只在这里说明一次，而不是每一代都重复一遍。
	for _, row := range rows {
		if !row.IsRunning() || !loadFailed(row.RunningLoad) {
			continue
		}
		_, _ = fmt.Fprintf(w, "插件 %s 的第 %d 代（%s）没装成，现在这个 key 哪一代都没在跑：%s（%s）\n",
			row.Key, row.Generation, row.Version,
			loadErrorText(row.RunningLoad.Error), loadTime(row.RunningLoad))
	}
}

// runningMark 说这一代现在是不是真的装着。
//
// 四档、三种取值（与 docs/cli-reference.md 的「plugin」一节、重生成的 man 页
// 同一套）：
//
//   - 取中的那一代、上一次加载也装上了 → 是；
//   - 取中的那一代、上一次加载跳过了它 → 否（原文在表后那一句里）；
//   - 取中的那一代、还没有加载记录 → 还没试。**这一档不能并进「是」**：三列是
//     加载器写下来的，它还没写过（发布或启用之后还没重启过；plugin.enabled 关着
//     时它根本不读表），而「取中了」与「装上了」是两件事；
//   - 不是取中的那一代（含这个 key 哪一代都没取中）→ 否。
//
// 打「是」与「否」而不是打勾打叉：清单里每一行都有一个值，而勾与叉在中文表格里
// 更像两行之间的一条注释，读起来要先分辨「有符号的那一行才算」。
//
// 「还没试」是在说这一行**还没有被加载器试过**，不是在说它失败了：真失败了的那一档
// 在表后带着错误原文，这一档没有。
func runningMark(row pluginstore.Row) string {
	switch {
	case !row.IsRunning():
		return "否"
	case row.RunningLoad == nil:
		return "还没试"
	case row.RunningLoad.Failed():
		return "否"
	default:
		return "是"
	}
}

// loadMark 打「这一代上一次被加载时装上了没有」。
//
// 只打在加载器取中的那一代上：加载结果按 key 给（见 Row.RunningLoad），打在别的
// 行上会被读成「这一代被加载过、没装成」。取中的那一代还没有加载记录时也打 `-`：
// 与「这是别的代」是同一个符号，两种情形都是「这一行没有加载结果」，分得开的是
// 「在跑」那一栏——那里写着「还没试」还是「否」。
func loadMark(row pluginstore.Row) string {
	switch {
	case !row.IsRunning() || row.RunningLoad == nil:
		return "-"
	case row.RunningLoad.Failed():
		return "没装成"
	default:
		return "装上了"
	}
}

// loadFailed 问上一次加载有没有把这一代跳掉。没有记录（还没试过）不算失败：
// 那一条走的是「还没试」那一档，见 runningMark。
func loadFailed(a *pluginstore.LoadAttempt) bool {
	return a != nil && a.Failed()
}

// loadErrorText 给出「没装成」后面的那半句。
//
// 空着时不留白也不省掉这半句：那一列记的是加载器交回来的错误原文，而一个真正的
// 失败总该有话说——真为空只可能是手工改库的行，或者将来换了一个写入方。读者看到
// 一句没有原因的「没装成」会以为是自己没看见：说清「库里没留下」，他才知道下一步
// 不是去看命令行的别的角落，而是去看日志里加载器那一条。
func loadErrorText(errText string) string {
	if errText == "" {
		return "库里没有留下错误原文"
	}
	return errText
}

// loadResultText 打「取中的那一代上一次加载」的结果。
//
// 没有记录时也不留白，但**不说「重启一次再看」这一句**：这一栏为空有两种来路，而
// 它们的下一步不一样。加载器整条链路与 plugin.enabled 一起装配，这个设置关着时它
// 根本不读 plugins 表，重启多少次都不会留下记录；开着时才轮到「发布或启用之后还
// 没重启过」那一句。把两条路一起写出来，比让他照着一条不对的下一步再试一遍有用。
//
// 这个 key 没有取中的一代时是另一回事：没东西可试，所以那一句说的是不适用。
func loadResultText(g *pluginstore.Generation, a *pluginstore.LoadAttempt) string {
	switch {
	case g == nil:
		return "不适用：这个 key 里没有取中的一代（见「在跑」那一栏）"
	case a == nil:
		return "没有记录：加载器还没试过它——发布或启用之后还没重启过，或者 plugin.enabled " +
			"是关着的（那时加载器不读表，重启也不会有记录）"
	case a.Failed():
		return fmt.Sprintf("没装成（%s）", loadTime(a))
	default:
		return fmt.Sprintf("装上了（%s）", loadTime(a))
	}
}

// loadTime 打加载那一次的时刻。
//
// At 可能是零值：三列由加载器一次写入，而手工改库的行可以只填结果那一列（见
// store.LoadAttempt.At）。打 0001-01-01T00:00:00Z 会像一个真时刻——读者会以为加载
// 发生在那个时间点上，而它不是；留空又会像「这一栏没值」。说一句「没有记下时刻」，
// 读者才知道要看的是别处（日志里那一条）。
func loadTime(a *pluginstore.LoadAttempt) string {
	if a == nil || a.At.IsZero() {
		return "没有记下时刻"
	}
	return formatTimeValue(a.At)
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
//
// 后四栏与表格那一档最后那一栏、以及表后那几句说明是同一件事的两半，只是这里
// 要机器读得出来：
//
//   - latest 说这一条是不是最新一次尝试（代次最高的那一行）；
//   - running_generation 说这个 key 里加载器取中的那一代，没有时是 null。
//     它按 key 给，所以同一个 key 的每一条带着同一个值（priority 也是这么给的）。
//     取中不等于装着：要读成「这一代真的在跑」，还得看下一条的 result 是 ok——
//     取中却被跳过时，这个 key 哪一代都没在跑；
//   - running_load 说取中的那一代上一次被加载时装上了没有：{result, error, at}，
//     result 是 ok / failed，error 是失败时的原文（装上了是 null），这一代还
//     没有加载记录时整个是 null。它也跟着 key 走，理由同上一条；
//   - compile_error 是**发布期**编译不过留下的原文，没有时是 null（空串读起来
//     像一个值，见 nullableString）。它只可能在手工改库的行上有值——publish
//     不收编译不过的插件；加载期没装上的原因在 running_load.error 里。
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
		"meta_problem":       nullableString(row.MetaProblem),
		"compile_error":      nullableString(row.CompileError),
		"latest":             row.Latest,
		"running_generation": generationOrNull(row.Running),
		"running_load":       loadAttemptOrNull(row.RunningLoad),
	}
}

// generationOrNull 把一个代次编成 JSON：没在跑的一代给 null。
func generationOrNull(g *pluginstore.Generation) any {
	if g == nil {
		return nil
	}
	return map[string]any{"version": g.Version, "generation": g.Number}
}

// loadAttemptOrNull 把一次加载期尝试编成 JSON：没有记录时给 null。
//
// 三栏一起给、一起为 null，因为它们来自同一次写：结果、失败时的原文、那一次的
// 时刻。error 用 nullableString 折空串——「装上了」不是「错误原文是空字符串」；
// at 在零值时是 null 而不是 0001-01-01（见 jsonTimeOrNull）：那一列可由手工改库的
// 行只填一半，而一个看着像真时刻的值会被脚本拿去做时间线。
func loadAttemptOrNull(a *pluginstore.LoadAttempt) any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"result": a.Result,
		"error":  nullableString(a.Error),
		"at":     jsonTimeOrNull(a.At),
	}
}

// runPluginGet 看单个插件的详情。
//
// 只读，所以**不要主密钥**（与 list 同一条）：它不改任何东西，而多要一份密钥
// 材料只会让查看这一个动作在只有库口令的机器上跑不起来。
func runPluginGet(args []string) error {
	fs := flag.NewFlagSet("plugin get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pluginShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf("get 需要一个插件 key，得到 %d 个参数", fs.NArg()))
	}
	key := strings.TrimSpace(fs.Arg(0))
	if key == "" {
		return usageErr(errors.New("get 的插件 key 不能为空"))
	}

	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		d, err := pluginstore.New(db).Get(ctx, key)
		if err != nil {
			return err
		}
		// 与 list 同一条约定：JSON 是另一条渲染路径，不是把上面那张表转成 JSON
		// （见 format.go 开头）。
		if jsonOutput() {
			return emitJSON(os.Stdout, pluginGetDoc(d), nil)
		}
		printPluginDetail(humanStdout(), d)
		return nil
	})
}

// printPluginDetail 打出单个插件的详情。
//
// 一栏一行（与 apikey get、model detail 同一个形状）：详情不是清单，没有「一行
// 一个实体」要维持，把字段挤成横向的表反而让最长的那个值（名称、时刻）把整张
// 表撑开。
//
// 表头用中文，与这两句说明一致：这里打的是**给人看的**一页，而库里的列名
// （key / version / …）已经在清单那张表上出现过一次了——那一处要的是与 psql、
// 与迁移文件对得上，这一处要的是读得顺。
func printPluginDetail(w io.Writer, d pluginstore.Detail) {
	tw := newTableWriter(w)
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("KEY", d.Key)
	row("版本", d.Version)
	row("状态", d.Status)
	row("代次", strconv.FormatInt(d.Generation, 10))
	row("优先级", strconv.Itoa(d.Priority))
	row("名称", d.Name)
	// 时刻按完整 RFC3339 打（见 printKeyDetail 同一条）：清单才需要截到分钟，
	// 一条实体的回显是要拿去对时间线的。
	row("写入", formatTimeValue(d.CreatedAt))
	row("写自", pluginCreatedBy(d.CreatedBy))
	// 「在跑」是上面那几栏答不了的一件事：上面说的是最新一代，而加载器取的是
	// 状态为 active 且代次最高的那一行，两者在发布了没 enable 时并不是同一行。
	// 答不出来时给一句「没有」，不留空——空栏与「这一栏不存在」长得一样。
	//
	// 四档与 list 那一栏同一套（见 runningMark）；这里写成句子，所以「取中但
	// 没装成」「取中但还没有记录」分得开，而「这个 key 哪一代都没在跑」也不与它们
	// 混在一起。取中却被跳过时把取的是哪一代一起写出来：那一刻这个 key 什么都不在
	// 跑（加载器跳过之后不会退回更老的那一代），而用户从「没有」两个字上看不出它是
	// 「没启用」还是「装了没成」。
	switch {
	case d.Running == nil:
		row("在跑", "没有：这个 key 里哪一代都没在跑")
	case loadFailed(d.RunningLoad):
		row("在跑", fmt.Sprintf("没有：取的是第 %d 代（%s），上一次加载没装成",
			d.Running.Number, d.Running.Version))
	case d.RunningLoad == nil:
		row("在跑", fmt.Sprintf("没有：取的是第 %d 代（%s），但它还没有加载记录",
			d.Running.Number, d.Running.Version))
	default:
		row("在跑", fmt.Sprintf("第 %d 代（%s）", d.Running.Number, d.Running.Version))
	}
	// 「上次加载」说的是**加载期**那一次尝试（库里的 last_load_result 那三列），
	// 与上面那几栏不是同一件事：上面说「库里有什么、谁在跑」，这一栏说「上一次
	// 启动读表时，取中的那一代装上了没有」。失败的原因原文排在表外（见下），
	// 所以这里只给结果与时刻。
	//
	// 与 list 那一栏同一个名字（栏头也是「上次加载」）：同一件事两个名字，读者
	// 会以为清单与详情说的不是同一列。
	row("上次加载", loadResultText(d.Running, d.RunningLoad))
	_ = tw.Flush()

	if d.MetaProblem != "" {
		_, _ = fmt.Fprintf(w, "这个插件的 meta 有问题：%s\n", d.MetaProblem)
	}
	// 加载期没装成的原因排在表外，与 plugin kv 的值同一条理由：它是引擎交回来的
	// 任意文本，换行与制表符都可能带，塞进单元格会顶出一行、多出一栏。上面那一行
	// 写着「没装成」时，这里给的是为什么——原文只在库里那一列上，除了这一句没有
	// 第二条路能读到它。
	//
	// 点名代次：这一页打的那一行是最新发布的一代，而没装成的是加载器取中的那一代
	// （在跑那一栏里），两者在发布了没 enable 时并不是同一行。取中一代才会有加载
	// 记录，所以这里 d.Running 一定有值。
	if loadFailed(d.RunningLoad) {
		_, _ = fmt.Fprintf(w, "第 %d 代（%s）上次加载没装成：%s\n",
			d.Running.Number, d.Running.Version, loadErrorText(d.RunningLoad.Error))
	}
	if d.Versions > 1 {
		// 只打最新的一代，所以这里必须说清「还有别的」：不说的话，「这个插件
		// 只有一行」与「它有好几代、你看到的只是最上面那一个」在这份输出上一样。
		_, _ = fmt.Fprintf(w, "这个 key 下还有 %d 个更老的版本：larkaxctl plugin list 列得全。\n",
			d.Versions-1)
	}
}

// pluginCreatedBy 打「这一行是谁写下的」。
//
// 只有 users.id 可打：plugins.created_by 存的是那个 ID，而这里不再多查一次
// users 表——一个不在清单上的字段把它变成一次连表，而要看用户名的人有 user get。
// 没有记录时打 `-`，不是空：空栏与「这一栏不存在」长得一样。
func pluginCreatedBy(id *int64) string {
	if id == nil {
		return "-"
	}
	return strconv.FormatInt(*id, 10)
}

// pluginGetDoc 组装 plugin get 的机器可读输出。
//
// 条目形状**逐字段**借用 list 的 pluginEntry，再补上详情才有的三栏：同一份数据
// 的两条读路径给出两套字段名时，对读它的脚本来说那就是两件事（与 apikey get
// 同一条理由）。这三栏是**加**不是换——list 里问不出来的正是「这一行是谁、什么
// 时候写的」与「这个 key 下有几行」。
func pluginGetDoc(d pluginstore.Detail) map[string]any {
	doc := jsonDoc(jsonOK)

	entry := pluginEntry(d.Row)
	entry["versions"] = d.Versions
	// 走 jsonTime（恒 UTC、完整精度），不走表格那一档的 formatTimeValue：库里
	// 交回来的是带会话时区的时刻，直接打出来同一行在两台机器上不一样。
	entry["created_at"] = jsonTime(d.CreatedAt)
	// nil 是「没有记录」——那一列可空，迁移或手工 SQL 写下的行就是 NULL，
	// 与「写自用户 0」不是一回事。
	var createdBy any
	if d.CreatedBy != nil {
		createdBy = *d.CreatedBy
	}
	entry["created_by"] = createdBy

	doc["plugin"] = entry
	return doc
}

func runPluginPublish(args []string) error {
	// 在**函数体第一行**拦下来，早于读文件、更早于连库与落库：publish 会写进
	// plugins 表，做完再报不支持就成了一次静默的写入（见 format.go）。
	if err := jsonUnsupported("plugin publish"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plugin publish", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pluginShortUsage) }
	key := fs.String("key", "", "")
	priority := fs.Int("priority", 0, "")
	if err := parseFlagsAnywhere(fs, args); err != nil {
		return usageErr(err)
	}
	switch {
	case fs.NArg() == 0:
		// 不打整段用法：人已经会用这条命令了，只是漏了源文件路径。用法留给
		// 「还没开始用」的那几处（缺子命令 / 未知子命令 / flag 解析失败）。
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

// errPluginKVKeyNotFound 表示要读或要删的那个键不在。
//
// 由命令自己造，而不是从存储层拿：KV 的读与删把「不存在」与「已经过期」归成
// 同一个答案（回 false，见 kv.go 的 Get），所以那一层没有、也不该有一个表示
// 「找不到」的错误值——它的调用方（插件）看到的都是 null。命令行这一档要给出
// 一个退出码，于是这个判断落在这里（与 snapshot get 的 errSnapshotNotFound
// 同一个形状）。
var errPluginKVKeyNotFound = errors.New("插件 KV 里没有这个键")

// requireMasterKey 要求这台机器带着主密钥。
//
// 卸载插件与写插件私有的存储都是**改库**的动作，与渠道写入同一档：要求
// LARKAX_MASTER_KEYS 在场。命令本身不解密任何东西——这一层要的是「跑它的人
// 手里有运维凭证」，而不是那几把钥匙的字节；把一条破坏性的入口开在一个只有
// 库口令的机器上，等于把「能不能删」交给连接串。
//
// 边界与只读那几条一样明确：list / get / kv get 不要它。排查时手上往往只有
// 库口令，而那时正是要看清单的时候。
//
// publish 也不走这里：它不 seal 任何东西，而这一层要的不是「会封装」而是
// 「带着运维凭证」——把它挂在 publish 上只会让发布多一道与它无关的门。
func requireMasterKey() error {
	if _, err := config.MasterKeysFromEnv(); err != nil {
		return fmt.Errorf("读取主密钥失败（LARKAX_MASTER_KEYS）: %w", err)
	}
	return nil
}

// runPluginDelete 把一个插件从库里删干净。
//
// 它是要主密钥的那三条之一（见 requireMasterKey）：删的是数据，而这一层要的是
// 「这台机器带着运维凭证」——**不是「凡写类都要」**，publish 与 settings set
// 同样是写类，都不读它。
func runPluginDelete(args []string) error {
	// 在**函数体第一行**拦下来，早于连库：这条命令会删插件与它的私有存储，
	// 做完再报不支持就成了一次静默的删除（见 format.go）。
	if err := jsonUnsupported("plugin delete"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plugin delete", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pluginShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf("delete 需要一个插件 key，得到 %d 个参数", fs.NArg()))
	}
	key := strings.TrimSpace(fs.Arg(0))
	if key == "" {
		return usageErr(errors.New("delete 的插件 key 不能为空"))
	}

	if err := requireMasterKey(); err != nil {
		return err
	}
	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		res, err := pluginstore.New(db).Delete(ctx, key)
		if err != nil {
			return err
		}
		printPluginDeleteResult(humanStderr(), res)
		return nil
	})
}

// printPluginDeleteResult 打印一次卸载的结果。
//
// 打字面量而不是「已删除插件。」：这一次删掉的东西有三样，而「新建的插件看起
// 来还在」与「连它的私有存储一起清了」在处理上是两件事——后者没有撤销键。
// 只复述一句「已删除」的话，把 KV 一起删掉这件事在输出上完全看不出来。
//
// 抽成函数是为了有一条不连库也能跑的判据：删除本身要连库，而「这一行里有没有
// 说出删掉了几样东西」正是要钉住的东西（与 printSettingsSetResult 同一个理由）。
func printPluginDeleteResult(w io.Writer, r pluginstore.Deleted) {
	_, _ = fmt.Fprintf(w, "已删除插件 %s：%d 个版本、%d 条启用记录、%d 个私有存储的键。重启后生效。\n",
		r.Key, r.Versions, r.Activations, r.KVKeys)
}

// ── plugin kv：插件私有的键值存储 ──────────────────────────────────────

// runPluginKV 处理 plugin 下的 kv 子命令。
func runPluginKV(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, pluginKVShortUsage)
		return usageErr(errors.New("plugin kv 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "get":
		return runPluginKVGet(rest)
	case "set":
		return runPluginKVSet(rest)
	case "delete":
		return runPluginKVDelete(rest)
	case "help":
		printUsage(os.Stdout, pluginKVHelp.long)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, pluginKVHelp.short)
		return nil
	default:
		printUsage(os.Stderr, pluginKVShortUsage)
		return usageErr(fmt.Errorf("未知的 plugin kv 子命令 %q", action))
	}
}

// kvTarget 是 kv 三条命令共用的那三段身份：插件、用户、键。
//
// 三段都走旗标，收成一个结构再校验：三条命令的参数一模一样，各写一遍解析就会
// 各漏各的必填项（漏掉 --user 的那一条会把别人的键读出来看，而它在输出上与正常
// 那一次完全一样）。
type kvTarget struct {
	plugin string
	user   string
	key    string
	userID int64
}

// parseKVTarget 解析 kv 三条命令共用的旗标，并做**连库之前**的那一半校验。
//
// 理由与 settings set 那一段相同（见那边的注释）：Go 的 flag 包遇到第一个非旗标
// 参数就停止解析，而 `plugin kv get --plugin p --user u --key k` 这种写法里三个
// 值都是旗标，形状就统一了；另外键名与用户名都可能以减号开头（排障时插一个
// `-1` 进去），走旗标才不会被当成另一个开关。
func parseKVTarget(verb string, args []string) (kvTarget, error) {
	fs := flag.NewFlagSet("plugin kv "+verb, flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pluginKVShortUsage) }
	plugin := fs.String("plugin", "", "")
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	key := fs.String("key", "", "")
	if err := fs.Parse(args); err != nil {
		return kvTarget{}, usageErr(err)
	}
	if fs.NArg() > 0 {
		return kvTarget{}, usageErr(fmt.Errorf(
			"%s 不接受位置参数，得到 %q：插件用 --plugin、用户用 --user、键用 --key",
			verb, fs.Arg(0)))
	}

	t := kvTarget{plugin: strings.TrimSpace(*plugin), user: *user, key: *key}
	if t.plugin == "" {
		return kvTarget{}, usageErr(errors.New("缺少 --plugin：这是哪个插件的键"))
	}
	if t.user == "" {
		return kvTarget{}, usageErr(errors.New("缺少 --user：这个键是哪个用户的"))
	}
	if t.key == "" {
		return kvTarget{}, usageErr(errors.New("缺少 --key：要看、要写还是要删哪一个键"))
	}
	return t, nil
}

// resolveKVTarget 解析用户、确认插件在库里，把 userID 补进 target。
//
// 先解析用户（auth.ResolveUser），与 settings set 的 --by 同一条路：users 表上
// 认不出来的标识在这里就报「用户不存在」（退出码 4），而不是落到 KV 上查不到行、
// 报成「没有这个键」——那会把「人写错了」说成「数据不在」。
//
// 再确认插件在库里：plugin_kv **没有外键指着 plugins**，所以给一个不存在的插件
// 写键不会报错，只会留下一行谁也读不到、谁也清不掉的数据（读与删都要键名，而
// 那个键名属于一个不存在的插件）。这一步让 `--plugin` 写错当场失败，而不是
// 往库里塞一行垃圾。读也照同一个理由查一遍：插件不在时「没有这个键」是一句
// 误导的话，它真正该说的是「这个插件在库里没有行」。
func resolveKVTarget(ctx context.Context, db *sql.DB, t kvTarget) (kvTarget, error) {
	actor, err := auth.New(db).ResolveUser(ctx, t.user)
	if err != nil {
		return kvTarget{}, fmt.Errorf("解析 --user 失败: %w", err)
	}
	t.userID = actor.ID

	if _, err := pluginstore.New(db).Get(ctx, t.plugin); err != nil {
		return kvTarget{}, err
	}
	return t, nil
}

func runPluginKVGet(args []string) error {
	target, err := parseKVTarget("get", args)
	if err != nil {
		return err
	}

	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		resolved, err := resolveKVTarget(ctx, db, target)
		if err != nil {
			return err
		}
		value, found, err := pluginstore.NewKV(db).Get(ctx, resolved.plugin, resolved.userID, resolved.key)
		if err != nil {
			return err
		}
		if !found {
			// 过期与不存在归成同一句话：存储层就是这么给的（见 kv.go 的 Get），
			// 而在这条命令的输出上分辨它们也分辨不出来——表里那一行还在，但它
			// 已经不算数了。
			return fmt.Errorf("%w：插件 %s 的用户 %s 名下没有键 %q（也可能它已经过期）",
				errPluginKVKeyNotFound, resolved.plugin, resolved.user, resolved.key)
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, pluginKVGetDoc(resolved, value), nil)
		}
		printPluginKVEntry(humanStdout(), resolved, value)
		return nil
	})
}

// printPluginKVEntry 打出一个键。
//
// 三段身份一栏一行（与 plugin get 同一个形状）；**值排在表外**：它是插件自己写
// 进去的任意文本，换行与制表符都可能带（写 JSON 时最常见），塞进单元格会顶出
// 一行、多出一栏。命令不改写它，原样打出来。
func printPluginKVEntry(w io.Writer, target kvTarget, value string) {
	tw := newTableWriter(w)
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("插件", target.plugin)
	row("用户", target.user)
	row("键", target.key)
	_ = tw.Flush()

	// 值排在表外、原样打：它是插件自己写进去的任意文本，换行与制表符都可能带
	// （写 JSON 时最常见），塞进单元格会顶出一行、多出一栏。这一段不缩进、也不
	// 按列对齐——它不是一个字段，是那段数据本身。
	_, _ = fmt.Fprintf(w, "值：%s\n", value)
}

// pluginKVGetDoc 组装 plugin kv get 的机器可读输出。
//
// 三段身份按**给进来的写法**打（用户是名字或 ID 的原文），而不是解析出来的
// users.id：这两条读路径要回答的是「我查的是哪一个键」，照抄一次输入最不容易
// 读错。要那个 ID 的话它在 users 表上，user get 打的就是它。
func pluginKVGetDoc(target kvTarget, value string) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["entry"] = map[string]any{
		"plugin": target.plugin,
		"user":   target.user,
		"key":    target.key,
		"value":  value,
	}
	return doc
}

func runPluginKVSet(args []string) error {
	// 在**函数体第一行**拦下来，早于 flag 解析、更早于连库：这条命令会往
	// plugin_kv 里写一行，做完再报不支持就成了一次没人知道的写入（见 format.go）。
	if err := jsonUnsupported("plugin kv set"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plugin kv set", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pluginKVShortUsage) }
	plugin := fs.String("plugin", "", "")
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	key := fs.String("key", "", "")
	value := fs.String("value", "", "")
	ttl := fs.Duration("ttl", 0, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("set 不接受位置参数，得到 %q：值用 --value 给", fs.Arg(0)))
	}

	target := kvTarget{plugin: strings.TrimSpace(*plugin), user: *user, key: *key}
	if target.plugin == "" {
		return usageErr(errors.New("缺少 --plugin：这是哪个插件的键"))
	}
	if target.user == "" {
		return usageErr(errors.New("缺少 --user：这个键是哪个用户的"))
	}
	if target.key == "" {
		return usageErr(errors.New("缺少 --key：要写哪一个键"))
	}
	// 负数直接拒，不按「不过期」处置：0 已经表达了不过期，让 -1 再表达一次
	// 只会多出一条要记的规则，而写 `--ttl -1` 的人多半是想写一个很短的时长、
	// 手滑多打了一个减号。
	if *ttl < 0 {
		return usageErr(fmt.Errorf("--ttl 不接受负数（得到 %s）：不给或给 0 就是不过期", *ttl))
	}

	if err := requireMasterKey(); err != nil {
		return err
	}
	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		resolved, err := resolveKVTarget(ctx, db, target)
		if err != nil {
			return err
		}
		if err := pluginstore.NewKV(db).Set(ctx, resolved.plugin, resolved.userID, resolved.key,
			*value, *ttl); err != nil {
			return err
		}
		printPluginKVSetResult(humanStderr(), resolved, *ttl, len(*value))
		return nil
	})
}

// printPluginKVSetResult 打印一次写入。
//
// 字节数打出来（不是值的长度）：插件自己的配额按**字节**算（见 host.KVMaxBytes），
// 而中文键值在「字符数」与「字节数」上差三倍——只报一个字符数的话，配额报错时
// 人对不上账。
//
// 过期时刻打的是**输入的那个时长**而不是算出来的时刻：算出来的那一刻由数据库
// 的 now() 决定（见 kv.go），命令这一侧没有那个时钟，照着本机时钟编一个时刻出来
// 会与库里那一行不一致。
func printPluginKVSetResult(w io.Writer, target kvTarget, ttl time.Duration, bytes int) {
	expiry := "不过期"
	if ttl > 0 {
		expiry = ttl.String() + " 后过期"
	}
	_, _ = fmt.Fprintf(w, "已写入插件 %s 的键 %s（用户 %s，%s，%d 字节）。\n",
		target.plugin, target.key, target.user, expiry, bytes)
}

func runPluginKVDelete(args []string) error {
	// 与其余写类同一条：函数体第一行拦下 json 档。
	if err := jsonUnsupported("plugin kv delete"); err != nil {
		return err
	}

	target, err := parseKVTarget("delete", args)
	if err != nil {
		return err
	}

	if err := requireMasterKey(); err != nil {
		return err
	}
	return withDB(pluginTimeout, func(ctx context.Context, db *sql.DB) error {
		resolved, err := resolveKVTarget(ctx, db, target)
		if err != nil {
			return err
		}
		deleted, err := pluginstore.NewKV(db).Del(ctx, resolved.plugin, resolved.userID, resolved.key)
		if err != nil {
			return err
		}
		if !deleted {
			// 与 kv get 用同一个哨兵：这一条的下一步动作与那一条完全一样——先
			// 看这个键在不在。删一个不存在的键报成成功，会让脚本把「我删掉了」
			// 记下来，而库里那一行可能正被别人重新写进去。
			return fmt.Errorf("%w：插件 %s 的用户 %s 名下没有可见的键 %q（也可能它已经过期）",
				errPluginKVKeyNotFound, resolved.plugin, resolved.user, resolved.key)
		}
		// 开头不写「已删除插件」：那是卸载那条命令的回显开头，两条读起来
		// 像是同一件事。这一条删的是一个键，插件还在。
		_, _ = fmt.Fprintf(humanStderr(), "已删除 %s 的键 %s（用户 %s）。\n",
			resolved.plugin, resolved.key, resolved.user)
		return nil
	})
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
	fs.Usage = func() { printUsage(os.Stderr, pluginShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
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
		_, _ = fmt.Fprintf(humanStderr(), "插件 %s %s。重启后生效。\n", key, done)
		return nil
	})
}
