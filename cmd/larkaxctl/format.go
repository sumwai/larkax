package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 这一层是**机器可读输出**（`--format json`）的共用部分：取值怎么解析、
// 顶层形状长什么样、怎么保证 stdout 上只有一份 JSON。
//
// 它与 style.go / table.go 是**并列的两条渲染路径**，不是「另一套样式」：
// 表格那一档服务的是终端里扫一眼的人，这一档服务的是 `| jq` 与探针。
// 两条路各自把同一份数据渲染一遍（见各命令的 `xxxDoc` 与 `printXxx`），
// 而不是先把表格转成 JSON——那样金额会变成字符串、nil 会变成占位符，
// 而这两件事正是机器可读输出最不能有的（见 docs/cli-output.md）。

// formatKind 是 `--format` 的取值。
//
// 只有两档：面向终端的那一档与机器可读的那一档。不给 `--format` 就是前者，
// 所以脚本要多写一个参数、人什么都不用改。
type formatKind int

const (
	formatTable formatKind = iota // 面向终端：表格、状态符、着色
	formatJSON                    // 机器可读：stdout 只有一份 JSON
)

// 这一档**不给 formatKind 写 String()**：`--format` 在命令行上的两个取值
// 由 takeFormatFlag 直接与字面量比，没有任何地方要把 formatKind 打成字符串。
// 先前有过一个 String()，check-deadcode 报它不可达（`format.go|formatKind.String`）
// ——它与 fmt 的 Stringer 接口一样只在 AST 上看不到调用点，所以这里选择删掉，
// 而不是去把白名单的 MAX_ENTRIES 往上调：留一个没人调的方法只换来测试失败
// 信息里多一个词。将来真要用到再就地加回来。

// outputFormat 是本次运行选定的输出格式。
//
// 它是一个包级变量而不是逐层传下去的形参：二十来条子命令的入口都是
// `runXxx([]string) error`，加一个形参会改到四十多处的调用与用例，而取值
// 只在 run() 里定一次、之后全程只读。用例要跑另一档时改它，改完复原
// （见 format_test.go 的 withFormat）。
var outputFormat = formatTable

// jsonOutput 报告这一次是不是机器可读的那一档。
func jsonOutput() bool { return outputFormat == formatJSON }

// isFormatFlag 判断一个参数名是不是全局选项 --format 或它的简写 -o。
//
// 三种拼法一并认（`--format`、`-format`、`-o` 与它们的 `--o` 写法）：本仓
// 其余选项由 flag 包解析，而 flag 同时接受 -name 与 --name；只有全局选项
// 不认的话，`larkaxctl -format json doctor` 会以「未知子命令」收场——
// 那不是使用者能自己看明白的错（与 --env-file 同一条理由）。
func isFormatFlag(name string) bool {
	switch name {
	case "--format", "-format", "--o", "-o":
		return true
	}
	return false
}

// takeFormatFlag 从参数里摘出全局选项 --format，返回它的取值与剩下的参数。
//
// # 为什么在最外层摘，而不是给每条子命令各接一个同名选项
//
// 与 takeEnvFileFlag 同一条理由：全局选项写在子命令**前后**都该能用。
//
//	larkaxctl --format json doctor
//	larkaxctl doctor --format json
//
// 后者尤其重要——人补一个选项时习惯写在最后，而这本该是最自然的写法，却会被
// 子命令自己的 flag 集当成「flag provided but not defined」。摘掉再分派，
// 二十来条子命令不必各加一遍同一个选项，也不会因为漏掉一条而只有那条命令
// 认不出这个选项，更不会出现「有的命令认、有的命令安静地当没看见」。
//
// 代价是这个字面量在**子命令的参数区里**也会被摘走。它没有第二种含义，
// 而把它当普通参数传下去的场景（`snapshot get` 的 request_id 恰好叫
// `--format`）不值得为它多开一条规则。
//
// 认 `--format json` 与 `--format=json` 两种写法，与 flag 包一致。
// 出现多次时取最后一个：同样与 flag 包「后写的赢」一致。
func takeFormatFlag(args []string) (formatKind, []string, error) {
	format := formatTable
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !isFormatFlag(name) {
			rest = append(rest, args[i])
			continue
		}

		if !hasValue {
			if i+1 >= len(args) {
				return formatTable, nil, usageErr(fmt.Errorf("%s 后面要跟 %s", name, formatChoices()))
			}
			i++
			value = args[i]
		}
		parsed, err := parseFormat(value)
		if err != nil {
			return formatTable, nil, usageErr(err)
		}
		format = parsed
	}

	return format, rest, nil
}

// formatChoices 是给错误信息用的取值清单。
//
// 单独一个函数而不是在字符串里写死：下面那条错误信息里两处提到它，各写一遍
// 就会在加第三档时只改一处，而错误信息里少一档的表现是「明明支持却说没有」。
func formatChoices() string { return "取值 table 或 json" }

// parseFormat 解析 --format 的取值。
//
// 取值不合法时**报错**，不静默回落成表格：一个 `--format=json` 打错成
// `--format=jsn` 的脚本，回落的表现是 stdout 上出现一段表格文本，
// 而报错的是下游的 jq——看起来像 jq 的问题（见 docs/cli-output.md）。
func parseFormat(raw string) (formatKind, error) {
	switch raw {
	case "table":
		return formatTable, nil
	case "json":
		return formatJSON, nil
	default:
		return formatTable, fmt.Errorf("--format 的取值 %q 不认识：%s", raw, formatChoices())
	}
}

// ── 文档的顶层形状 ──────────────────────────────────────────────────────

// status 的取值。三档与表格那一档的状态符一一对应（见 style.go 的 glyphKind）：
//
//	ok      全部通过（退出码 0）
//	failed  至少一项查出问题
//	skipped 一项都没查出问题，但有项目**没跑成**（依赖没满足）
//
// skipped 不是 ok 的近义词，这正是这一档存在的理由：「没跑成」与「通过」在
// 安静的终端里长得一样，是这个项目反复踩的坑（见 LESSONS.md 第 16 条），
// 而机器读的输出更不能让这两件事落进同一个字符串。读到 skipped 的一方要看
// summary 里哪几项没跑成，以及退出码。
const (
	jsonOK      = "ok"
	jsonFailed  = "failed"
	jsonSkipped = "skipped"
)

// jsonDoc 起一份机器可读输出。
//
// 顶层形状对所有子命令一致：
//
//	{
//	  "status": "ok",
//	  "summary": { ... },          // 可选：这一档的计数
//	  "<实体>": [ ... ],            // 条目数组，键名是英文复数
//	  "exit_code": 0
//	}
//
// exit_code 由 emitJSON 填，调用点不必自己管。
func jsonDoc(status string) map[string]any {
	return map[string]any{"status": status}
}

// emitJSON 把一份机器可读输出写到 w，并把 cause 原样返回。
//
// 它把 cause 的退出码算出来填进 exit_code，写完 JSON 与一个换行。
// **别的什么都不打**：提示、警告、进度一律走 stderr，因为 stdout 是给管道
// 下游的（`larkaxctl ... --format json | jq`），混进一行提示就会让 jq 报错。
//
// 返回 cause 是刻意的：调用点一行 `return emitJSON(os.Stdout, doc, err)`
// 就同时表达了「打这份报告」与「用这个错误定退出码」，而退出码必须与交互
// 模式一致（doctor 有失败项时仍退出 1）——两处各写一遍迟早会分叉。
func emitJSON(w io.Writer, doc map[string]any, cause error) error {
	doc["exit_code"] = exitCode(cause)

	enc := json.NewEncoder(w)
	// 缩进两格：这份输出会被人 `| jq .` 也要能直接看，而压成一行在终端里
	// 读不了。jq 两种都收。
	enc.SetIndent("", "  ")
	// 不转义 HTML：默认会把 `>` `<` `&` 写成 > 这类，虽然仍是合法 JSON，
	// 但 doctor 的原文、表达式源码里这些字符并不少，转义之后人读那份输出
	// 就多了一层解码。
	enc.SetEscapeHTML(false)

	if err := enc.Encode(doc); err != nil {
		// 编不出来只可能是有人往 doc 里塞了编不了的东西（chan、函数）——
		// 那是这份代码的 bug，不是使用者的输入错，所以标成「内部错误」。
		// stdout 此时仍是干净的：Encoding 失败不会写出去半个字节以外的东西
		// ——它先编完再写，所以这里不会留下半份 JSON 让 jq 更迷惑。
		return internalErr(fmt.Errorf("序列化 JSON 输出失败: %w", err))
	}
	return cause
}

// jsonUnsupported 在 json 模式下拒掉还没接机器可读输出的子命令。
//
// 静默回落成表格是最坏的结果：`larkaxctl apikey create ... --format json | jq`
// 会拿到一段给人看的文本，报错的是 jq，看起来像 jq 的问题；更坏的是写类命令
// 已经把事情做完了，脚本却只看到一句解析错误，于是重跑一次。
//
// 所以这里报**参数错**（退出码 2），并且必须在命令做任何事之前调用——
// 放在写库之后就成了「先做了再报不支持」。
func jsonUnsupported(command string) error {
	if !jsonOutput() {
		return nil
	}
	return usageErr(fmt.Errorf(
		"%s 还不支持 --format json：目前接上的是读类子命令（见 docs/cli-output.md 的「机器可读输出」）",
		command))
}

// jsonAmount 把金额打成十进制定点字符串。
//
// **不打成 JSON 数字**：金额在库里是定点数（见 internal/money），过一遍
// float64 会丢精度，而丢掉的恰好是账单要精确到的那几位；也不打成
// `{"units": ...}` ——Amount 的字段不导出，直接交给 encoding/json 出来的是
// 一个空对象，看起来像「这个字段没值」。字符串是它在 JSON 里唯一无损的表达，
// 要拿它算数的一方自己按小数解析。
func jsonAmount(a money.Amount) string { return a.String() }

// 下面几个把「表格里的占位符」换成 JSON 里的 null。
//
// 表格那一档用 `-`、`不过期`、`从未` 这类占位符把「这一栏没值」铺满，那是
// 给人一眼扫的；JSON 里必须给 null，理由是同一件事：留空（键不在）与
// null（键在、值是空）是两件事，而脚本要能按它分派。占位符进 JSON 更坏——
// `"expires_at": "不过期"` 会让按时间解析它的一方拿到一个解析不了的值。

// nullableString 把空串换成 null。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt 把 0 换成 null。
//
// 只用在「0 不是一个合法取值」的栏上（HTTP 状态码、用户 ID）。计数类的 0
// 是真值，那些地方直接给数字。
func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// nullableError 把一个错误折成它的原文，nil 就是 null。
//
// 只打错误那句话，不打它的类型或包装链：`%v` 出来的正是人看到的那一句，
// 而这个字段的读者是要把原因转述出去的人（或日志），不是按类型分派的代码
// ——真要分派应当看 `error_code` 那类具名信号。
func nullableError(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}

// jsonList 把 nil 切片换成空切片。
//
// nil 切片编出来是 `null`，而空切片是 `[]`：前者读起来像「这一栏没有」，
// 后者才是「这一栏有，一条都没有」。空清单必须走后者——「一条都没有」与
// 「这一步没跑」是本仓反复要分清的两件事（见 LESSONS.md 第 16 条）。
func jsonList[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ── 取值助手：把 Go 侧的「零值 = 没有」翻成 JSON 的 null ────────────────
//
// 上面那几个（nullableString / nullableInt / nullableError / jsonList）与
// 下面这几个是同一件事的不同档：哪一档用哪一栏，取决于那一栏在 Go 侧怎么
// 表达「没有值」——空串、0、nil 指针、零值时刻、nil 切片。放在这里而不是
// 某条命令自己的文件里，是因为它们被好几条命令共用（渠道、模型、别名、价格
// 都用到时刻与日期那两档）。

// jsonID 把一个外键打成 null 或数字。
//
// 0 表示「不挂任何实体」——平台自营的渠道、平台级别名、平台价——而 0 不是
// 一个合法的 ID（它们从 1 开始），所以 0 在这里是「没有」而不是「编号 0」。
// 与 nullableInt 是同一条判据，只是外键在库里的类型是 bigint，Go 侧是 int64。
func jsonID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// jsonTime 把一个时刻打成 RFC3339。
//
// 库里那几栏都是 timestamptz，统一按 UTC 打：同一个瞬间在任何机器、任何
// TZ 下都编出同一个字符串。精度给到完整（RFC3339Nano），不做任何截断——
// 表格那一档怎么排版是它的事，机器读的一方不该被排版规则顺手丢掉信息。
func jsonTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// jsonTimeOrNull 打一个可空时刻，零值就是 null。
//
// 零值在这里是「这一栏没有值」而不是「公元 1 年」（见 channels.ChannelInfo
// 的 DeletedAt、pricing.RuleInfo 的 EffectiveFrom 一类约定）。
func jsonTimeOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return jsonTime(t)
}

// jsonTimePtr 打一个用指针表达的可空时刻，nil 就是 null。
func jsonTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return jsonTime(*t)
}

// jsonDate 打一个可空日期。
//
// 只打日子不打时刻，与表格那一档的 dateLabel 同一条理由：表上
// knowledge_cutoff / release_date 两列是 date（无时区），按某个时区渲染成
// 时刻会让同一个值在不同机器上打出来不一样。
func jsonDate(d *time.Time) any {
	if d == nil {
		return nil
	}
	return d.Format("2006-01-02")
}

// jsonObject 打一个原始 JSON 对象。
//
// nil 是「这一栏没有值」，给 null；空对象 `{}` 是一个**真值**（库里的默认
// 就是它，表示「没配任何键」），照原样打出来——把 `{}` 折成 null 会让
// 「配了一个空对象」与「这一栏根本没写过」变成同一件事。
func jsonObject(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}
