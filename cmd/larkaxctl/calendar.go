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

	calendarstore "github.com/sumwai/larkax/internal/calendar/store"
	"github.com/sumwai/larkax/internal/config"
)

// priceCalendarHelp 是 price calendar 的两版帮助文本（见 helpText）。
var priceCalendarHelp = helpText{short: priceCalendarShortUsage, long: priceCalendarUsage}

// priceCalendarShortUsage 是 price calendar 的短版：`-h` / `--help` 与参数写错
// 时打的那一份。
const priceCalendarShortUsage = `larkaxctl price calendar 维护分时计价用的日历数据。

用法：
  larkaxctl price calendar import <文件>   把一份日历数据文件灌进库里
  larkaxctl price calendar list            列出导入过的年份与状态

文件是一个 JSON 对象，字段写错会直接报错；只有 synced 状态才带 days，而 days
必须覆盖本年的每一天。同一年再灌一次是覆盖，不是追加。

更细的边界：larkaxctl price calendar help。
`

// priceCalendarUsage 是 price calendar 的长版：`price calendar help`；这也是
// 那份数据文件格式的说明。
//
// 格式写在这里而不是只写在文档里：跑 `larkaxctl price calendar import` 时手边
// 应当就能查到它，否则「文件该怎么写」要先去找文档。
//
// 它挂在 price 下面（#475）：这些数据是分时计价规则的输入，离开计价规则没有
// 别的用处。旧路径 larkaxctl calendar import 仍然可用，见末尾那一行。
const priceCalendarUsage = `larkaxctl price calendar 维护分时计价用的日历数据。

用法：
  larkaxctl price calendar import <文件>   把一份日历数据文件灌进库里
  larkaxctl price calendar list            列出导入过的年份与状态

文件是一个 JSON 对象，只认下面这些字段（键名写错会直接报错）：

{
  "region": "CN",
  "year": 2031,
  "status": "synced",
  "source": "manual",
  "source_paper_urls": ["https://example.com/notice"],
  "sources_agree": true,
  "last_error": "",
  "days": [
    {"date": "2031-01-01", "name": "元旦", "off_day": true},
    {"date": "2031-01-02", "off_day": false},
    {"date": "2031-01-04", "off_day": false, "makeup_workday": true}
  ]
}

  region              必填。地区代码，目前只有 CN。
  year                必填。这份数据所属的年份。
  status              必填。synced / pending / conflict / degraded 之一。
  source              数据的出处（holiday-cn、manual 之类），落进
                      calendar_days.source。status 为 synced 时必填；
                      pending / conflict / degraded 三种状态**必须不写**
                      ——它只用于数据行，而那三种状态不写数据行。
  days                该年每一天的判定。只有 synced 才带 days，且必须覆盖
                      本年的每一天（含调休补班的周末）。date 用 YYYY-MM-DD，
                      off_day 必填——漏写与写 false 在 JSON 里长得一样，
                      而漏写会让那一天按工作日算。
  source_paper_urls   公告原文地址，可选。
  sources_agree       双源交叉校验结果，可选（不写即「没得比」）。
  last_error          同步失败的原因，可选，status 为 degraded 时有意义。

pending（公告尚未发布）、conflict、degraded 三种状态只记年份状态，不带
days 也不带 source——那一年还没有定论，写进去的数据行会被当成确定的判定
使用，而 source 只用于数据行。

同一年再灌一次是覆盖而不是追加：导入会先清掉这一年上一次导入写下的行。
跨年边界上的日子（例如元旦安排里把去年 12 月 31 日挪成休息）可以写进相邻
年份的文件里，同一天以最后一次导入为准。

数据从哪来不在本命令的范围内：它只负责「读文件 → 写库」。

list 列出库里导入过的年份：地区、年份、状态、天数、双源校验的结果，以及这一行
写下的时刻；同地区内按年份从新到旧（最近灌的那一年在最上面）。**没有导入过的
年份不会出现在这张清单里**——它没有写 calendar_years，也就没有「未导入」这一行
可列。判定时一个没有记录的年份按未知处理：既不算工作日，也不算休息日。

状态打的是库里那一列的原文。synced 表示这一年有数据行；pending 是公告还没发布
（未来年份的正常状态，不是缺陷）；conflict（双源不一致）与 degraded（公告已发布
却同步失败）需要人去看那一年发生了什么，失败的原因单独打在一行里。

旧路径：larkaxctl calendar import <文件> 仍然可用，与
larkaxctl price calendar import <文件> 逐字相同。list 只挂在 price 下面。

更细的约定见 docs/cli-reference.md 的「price calendar」一节。
`

// runPriceCalendar 处理 price 下的 calendar 子命令。
func runPriceCalendar(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, priceCalendarShortUsage)
		return usageErr(errors.New("price calendar 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "import":
		return runCalendarImport(rest)
	case "list":
		return runCalendarList(rest)
	case "help":
		printUsage(os.Stdout, priceCalendarUsage)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, priceCalendarShortUsage)
		return nil
	default:
		printUsage(os.Stderr, priceCalendarShortUsage)
		return usageErr(fmt.Errorf("未知的 price calendar 子命令 %q", action))
	}
}

// runCalendar 处理顶层的 calendar 子命令。
//
// 这是一个旧路径：这一组已经搬进 price 域（#475），这里只为已经在跑的脚本与
// 部署流程留着，不列进根帮助页。它把 import 与 help 原样接给搬过去的那一套，
// 所以新旧两条路径的输出逐字相同。
func runCalendar(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, priceCalendarShortUsage)
		return usageErr(errors.New("calendar 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "import":
		return runCalendarImport(rest)
	case "help":
		printUsage(os.Stdout, priceCalendarUsage)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, priceCalendarShortUsage)
		return nil
	default:
		printUsage(os.Stderr, priceCalendarShortUsage)
		return usageErr(fmt.Errorf("未知的 calendar 子命令 %q", action))
	}
}

// runCalendarImport 把一份日历数据文件灌进库里。
//
// 参数只有一个文件路径，没有 --region / --year 之类的覆盖项：地区与年份写在
// 文件里，而「同一件事有两个来源时命令行那个会悄悄赢」是本仓库别处已经踩过的
// 坑。文件读不出来或校验不过时**不连数据库**——那种情况下连上去也没有用，
// 而失败发生在碰库之前，日志里就不会出现一条看不懂的连接记录。
func runCalendarImport(args []string) error {
	if err := jsonUnsupported("price calendar import"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("price calendar import", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceCalendarShortUsage) }
	if err := fs.Parse(args); err != nil {
		// -h / --help 不是失败：flag 在返回 ErrHelp 之前已经把用法打印过了，
		// 再把它当错误返回会让 `larkaxctl price calendar import --help` 以退出码 1
		// 结束（main 把任何非 nil 的错误都当成失败），而 priceCalendarUsage
		// 正好叫人这么跑。`larkaxctl price calendar --help` 走的是另一条分支，
		// 那条本来就返回 nil。
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		// 不打整段用法：人已经会用这条命令了，只是位置参数没给对。用法留给
		// 「还没开始用」的那几处（缺子命令 / 未知子命令 / flag 解析失败）。
		return usageErr(fmt.Errorf("price calendar import 需要一个文件参数，得到 %d 个", fs.NArg()))
	}
	path := fs.Arg(0)

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	// 关闭错误无处可报：此时函数已经在返回的路上，该打印的错误已经打印了，
	// 而读一个只读文件之后的关闭失败不会改变命令的结果。
	defer func() { _ = file.Close() }()

	doc, err := calendarstore.ParseDocument(file)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), calendarImportTimeout)
	defer cancel()
	// 走 openDB 而不是自己 sql.Open + Ping：打开与探活失败时的处置相同
	// （都归 exitDBUnreach），各写一遍的表现是同一类失败在两条命令上给出
	// 不同的退出码，而脚本对它们的处置是不一样的。
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	result, err := calendarstore.NewAdmin(db).Import(ctx, doc)
	if err != nil {
		return err
	}
	printCalendarImport(humanStdout(), result)
	return nil
}

// calendarImportTimeout 是单次导入的时间上限。
//
// 一年的数据是三百多行，正常在几秒内写完。给到两分钟是为了让「库很慢」与
// 「卡住了」区分得开：上限太短会让一次正常的慢导入被当成失败，而命令失败
// 时事务整体回滚，库里看不出发生过什么。
const calendarImportTimeout = 2 * time.Minute

// printCalendarImport 打一行导入结果。
//
// 每次都打：导入成功时不会有任何错误输出，而「什么都没写」与「写成功了」
// 在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
func printCalendarImport(w io.Writer, r calendarstore.Result) {
	_, _ = fmt.Fprintf(w, "已导入 %s %d 年：状态=%s，本年 %d 天，年外 %d 天，替换原有 %d 行\n",
		r.Region, r.Year, r.Status, r.DaysInYear, r.DaysOutsideYear, r.Replaced)
	if r.DaysInYear == 0 {
		// 非 synced 的状态本来就不带数据行，这一句是为了让「0 天」看起来是
		// 预期结果，而不是数据被吞了。
		_, _ = fmt.Fprintf(w, "（status=%s 的年份不带数据行，只记录年份状态）\n", r.Status)
	}
}

// calendarListTimeout 是列日历的时间上限。
//
// 与 calendarImportTimeout 分开写：一条 SELECT 与一次几百行的导入不是一个量级，
// 共用一个常量会让「查询卡住了」多等近两分钟。
const calendarListTimeout = 30 * time.Second

// runCalendarList 列出导入过的年份。
//
// 只读，不要主密钥：与 list 那几组同一条。
func runCalendarList(args []string) error {
	fs := flag.NewFlagSet("price calendar list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceCalendarShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(calendarListTimeout, func(ctx context.Context, db *sql.DB) error {
		years, err := calendarstore.NewSource(db).List(ctx)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, calendarListDoc(years), nil)
		}
		printCalendarList(humanStdout(), years, verboseOutput)
		return nil
	})
}

// printCalendarList 打出导入过的年份。
//
// 表头用中文（与其余清单一致）。天数那一栏是当年写下的数据行数：非 synced 的
// 年份是 0，而 0 在这里是**正确值**，不是「没读到」——状态那一栏同时说明了这
// 一年本来就不带数据行。
//
// 同步失败的原因不打进表里：它是整句话，挤进一栏会把整张表撑到折行，而折出来
// 的行与下一年分不开。它单独排在表后，一行一条。
func printCalendarList(w io.Writer, years []calendarstore.Year, verbose bool) {
	if len(years) == 0 {
		// 明确打一句，而不是什么都不输出：一个没导入过的库与「命令没跑起来」
		// 在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
		_, _ = fmt.Fprintln(w, emptyListNote("日历", "price calendar import"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("地区"), p.bold("年份"), p.bold("状态"), p.bold("天数"), p.bold("双源"), p.bold("同步于"))
	for _, y := range years {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			p.bold(y.Region), p.bold(strconv.Itoa(y.Year)), y.Status, y.DayCount,
			agreeText(y.SourcesAgree), formatStampShort(y.SyncedAt))
	}
	_ = tw.Flush()

	for _, y := range years {
		if y.LastError != "" {
			_, _ = fmt.Fprintf(w, "%s %d 年的同步失败原因：%s\n", y.Region, y.Year, y.LastError)
		}
	}
	// 公告原文地址只在 -v 下打：一行的链接可能有三四个，默认那一屏要的是
	// 「哪一年灌过、状态是什么」（见 docs/cli-output.md 的「提示与警告」）。
	if !verbose {
		return
	}
	for _, y := range years {
		if len(y.SourcePaperURLs) > 0 {
			_, _ = fmt.Fprintf(w, "%s %d 年的公告：%s\n",
				y.Region, y.Year, strings.Join(y.SourcePaperURLs, " "))
		}
	}
}

// agreeText 把双源校验的结果折成表里那一个词。
//
// 三档都要有话说：nil 不是「一致」也不是「不一致」，它表示那一年没有第二个源
// 可比。留空会把「没得比」与「这一栏没读到」混成同一个样子。
func agreeText(agree *bool) string {
	switch {
	case agree == nil:
		return "没得比"
	case *agree:
		return "一致"
	default:
		return "不一致"
	}
}

// calendarListDoc 组装 price calendar list 的机器可读输出。
//
// 与 printCalendarList 是同一份数据的另一条渲染路径（见 format.go 开头）。
func calendarListDoc(years []calendarstore.Year) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(years)}

	// 从空切片起：一年都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(years))
	for _, y := range years {
		entries = append(entries, calendarYearEntry(y))
	}
	doc["calendars"] = entries
	return doc
}

// calendarYearEntry 打清单里的一个年份。
//
// 键名用**库里的列名**（region / year / status / day_count / …），与表格那一档
// 的表头同一条理由：它们在 psql 里、在迁移文件里、在这里是同一个东西。
func calendarYearEntry(y calendarstore.Year) map[string]any {
	// nil 是「没得比」——那一列可空，与「比过、结果是不一致」不是一回事。
	var agree any
	if y.SourcesAgree != nil {
		agree = *y.SourcesAgree
	}
	// 公告地址从空切片起：一条都没有时打 `[]`，不是 `null`。
	urls := y.SourcePaperURLs
	if urls == nil {
		urls = []string{}
	}

	return map[string]any{
		"region":    y.Region,
		"year":      y.Year,
		"status":    y.Status,
		"day_count": y.DayCount,
		// 原文照搬：这几条链接是公告修订时要拿来比对的东西。
		"source_paper_urls": urls,
		"sources_agree":     agree,
		"last_error":        nullableString(y.LastError),
		// 走 jsonTime（恒 UTC、完整精度），不走表格那一档的截断。
		"synced_at": jsonTime(y.SyncedAt),
	}
}
