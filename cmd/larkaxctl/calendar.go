package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	calendarstore "github.com/sumwai/larkax/internal/calendar/store"
	"github.com/sumwai/larkax/internal/config"
)

// calendarUsage 是 calendar 子命令的帮助，同时也是这份文件格式的说明。
//
// 格式写在这里而不是只写在文档里：跑 `larkaxctl calendar import` 时手边
// 应当就能查到它，否则「文件该怎么写」要先去找文档。
const calendarUsage = `larkaxctl calendar 维护分时计价用的日历数据。

用法：
  larkaxctl calendar import <文件>   把一份日历数据文件灌进库里

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
`

// runCalendar 处理 calendar 子命令。
func runCalendar(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, calendarUsage)
		return errors.New("calendar 需要子命令")
	}
	action, rest := args[0], args[1:]

	switch action {
	case "import":
		return runCalendarImport(rest)
	case "help", "-h", "--help":
		fmt.Print(calendarUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, calendarUsage)
		return fmt.Errorf("未知的 calendar 子命令 %q", action)
	}
}

// runCalendarImport 把一份日历数据文件灌进库里。
//
// 参数只有一个文件路径，没有 --region / --year 之类的覆盖项：地区与年份写在
// 文件里，而「同一件事有两个来源时命令行那个会悄悄赢」是本仓库别处已经踩过的
// 坑。文件读不出来或校验不过时**不连数据库**——那种情况下连上去也没有用，
// 而失败发生在碰库之前，日志里就不会出现一条看不懂的连接记录。
func runCalendarImport(args []string) error {
	fs := flag.NewFlagSet("calendar import", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, calendarUsage) }
	if err := fs.Parse(args); err != nil {
		// -h / --help 不是失败：flag 在返回 ErrHelp 之前已经把用法打印过了，
		// 再把它当错误返回会让 `larkaxctl calendar import --help` 以退出码 1
		// 结束（main 把任何非 nil 的错误都当成失败），而 main.go 的用法文本
		// 正好叫人这么跑。`larkaxctl calendar --help` 走的是另一条分支，
		// 那条本来就返回 nil。
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("calendar import 需要一个文件参数，得到 %d 个", fs.NArg())
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), calendarImportTimeout)
	defer cancel()
	// 变量名不复用 err：这里的每一次重新声明都会让后面读代码的人停下来判断
	// 「看到的是哪个 err」，而包一层名字的成本是零。
	if pingErr := db.PingContext(ctx); pingErr != nil {
		return fmt.Errorf("连接数据库失败: %w", pingErr)
	}

	result, err := calendarstore.NewAdmin(db).Import(ctx, doc)
	if err != nil {
		return err
	}
	printCalendarImport(os.Stdout, result)
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
