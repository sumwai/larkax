package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/calendar"
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
  larkaxctl price calendar sync [--year <年>]
                                           从公开数据源拉取一年并写进库里
  larkaxctl price calendar list            列出导入过的年份与状态

文件是一个 JSON 对象，字段写错会直接报错；只有 synced 状态才带 days，而 days
必须覆盖本年的每一天。同一年再灌一次是覆盖，不是追加。sync 拉到的数据源如果
还没发布这一年，写的是 pending，退出码是 0。

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
  larkaxctl price calendar sync [--year <年>]
                                           从公开数据源拉取一年并写进库里
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

import 只管「读文件 → 写库」：文件可以由 sync 拉取，也可以由运维手工准备，
手工那份用于兜底与修正。

sync 从公开数据源 holiday-cn 拉取一年的放假安排，转换成上面这份格式，再走
同一条导入路径写库。--year 缺省取当前年份（按 CN 时区判定），只接受单个年份；
写成 --year 2025,2026 会报参数错。

  数据源明确表示「这一年还没发布」（404，或者返回的 days 为空）时写
  pending，并以 0 退出——那是日历的一个状态，不是命令失败。
  真正的失败（网络、解析、校验）以非零退出：那一年已经是 synced 时库里
  一行都不动，否则记下 degraded 与失败原因。

数据源只给每一天「放不放假」。调休补班日（makeup_workday）按「不是假期、
且该日是周六或周日」推断；isOffDay 为 false 却不在周末的日子照样收下，
off_day 记 false、makeup_workday 也记 false，名称保留。

list 列出库里导入过的年份：地区、年份、状态、天数、双源校验的结果，以及这一行
写下的时刻；同地区内按年份从新到旧（最近灌的那一年在最上面）。**没有导入过的
年份不会出现在这张清单里**——它没有写 calendar_years，也就没有「未导入」这一行
可列。判定时一个没有记录的年份按未知处理：既不算工作日，也不算休息日。

状态打的是库里那一列的原文。synced 表示这一年有数据行；pending 是公告还没发布
（未来年份的正常状态，不是缺陷）；conflict（双源不一致）与 degraded（公告已发布
却同步失败）需要人去看那一年发生了什么，失败的原因单独打在一行里。

旧路径：larkaxctl calendar import <文件> 仍然可用，与
larkaxctl price calendar import <文件> 逐字相同。list 与 sync 只挂在 price 下面。

更细的约定见 docs/cli-reference.md 的「price calendar」一节。
`

// priceCalendarActions 是 price calendar 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var priceCalendarActions = []string{"import", "sync", "list"}

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
	case "sync":
		return runPriceCalendarSync(rest)
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

// calendarActions 是 calendar 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var calendarActions = []string{"import"}

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

	fs := newFlagSet("price calendar import", flag.ContinueOnError)
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

// ── price calendar sync ──────────────────────────────────────────────

// calendarSyncTimeout 是拉取一次数据源的时间上限。
//
// 一份数据文件只有几十条记录，正常在一秒内回来。给到一分钟是为了让「上游慢」
// 与「连不上」区分得开：连不上时连接本身很快就失败，等不到这个上限的。
const calendarSyncTimeout = 60 * time.Second

// holidayCNBaseURL 是 holiday-cn 数据文件所在的目录，一年一个文件（<年>.json），
// 没有索引文件。
//
// 包级变量而不是常量：用例把它指向 httptest.Server，不打真网。
var holidayCNBaseURL = "https://raw.githubusercontent.com/NateScarlet/holiday-cn/master"

// calendarSyncHTTPClient 是拉数据源用的客户端，与 doctor 那一份分开。
var calendarSyncHTTPClient = &http.Client{Timeout: calendarSyncTimeout}

// calendarSyncNow 是「现在」的注入点：--year 缺省时取它的年份。
var calendarSyncNow = time.Now

// holidayCNFile 是 holiday-cn 的一份数据文件。
//
// 只认用得上的三个字段，**不像 Document 那样拒绝未知键**：这份文件由外部维护，
// 里面还有 $schema、$id 这类字段，多出一个字段不该让整年同步失败。仓库自己的
// 文件格式是另一回事——那边认错键名的代价是那一天的判定被悄悄用错。
type holidayCNFile struct {
	Year   int                  `json:"year"`
	Papers []string             `json:"papers"`
	Days   []holidayCNDaySource `json:"days"`
}

// holidayCNDaySource 是数据源里的一天。它只给「放不放假」，调休补班要靠推断。
type holidayCNDaySource struct {
	Name     string `json:"name"`
	Date     string `json:"date"`
	IsOffDay bool   `json:"isOffDay"`
}

// runPriceCalendarSync 从 holiday-cn 拉取一年的数据并写库。
// price calendar sync 的状态机：行是库里这一年的状态，列是这次拉回来的东西。
//
//	前态 \ 这次拉回 | 数据             | pending（404 / 空 days） | 错误
//	----------------+------------------+--------------------------+----------------------
//	无记录          | 写 synced，0     | 写 pending，0            | 写 degraded，非 0
//	pending         | 写 synced，0     | 写 pending，0（幂等）    | 写 degraded，非 0
//	degraded        | 写 synced，0     | 写 pending，0            | 覆盖 degraded，非 0
//	synced          | 覆盖，0          | 不写，0，打一行说明      | 不写，非 0
//	conflict        | 覆盖，0          | 不写，0                  | 不写，非 0
//
// 每一格都由下面的代码显式回答。「有定论」指 synced 与 conflict——两者都是
// 这一年已经有结果；无记录、pending、degraded 则还没有结果，它们之间可以互相
// 覆盖。synced 与 conflict 那两格「不写」是同一个理由：Import 会先删掉这一年
// 原有的行，而单源的 pending 或一次拉取失败，都不该把一份已经有结果的数据降级。
//
// conflict 与 synced 同等保护：两源对不上也是一条结论，把它降成单源的 pending
// 等于丢掉「两源不一致」这个事实，而那正是下一次人工核对该找的东西。反方向是
// 允许的：一次成功且一致的拉取可以把 conflict 改写成 synced（「覆盖」那一列）。
func runPriceCalendarSync(args []string) error {
	if err := jsonUnsupported("price calendar sync"); err != nil {
		return err
	}

	fs := newFlagSet("price calendar sync", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceCalendarShortUsage) }
	rawYear := fs.String("year", "", "要同步的年份；缺省取 CN 时区的当年")
	if err := fs.Parse(args); err != nil {
		// -h / --help 不是失败：flag 在返回 ErrHelp 之前已经把用法打印过了，
		// 再把它当错误返回会让 `larkaxctl price calendar sync --help` 以退出码 1
		// 结束。理由与 runCalendarImport 那一处相同。
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("price calendar sync 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	year, err := syncTargetYear(*rawYear)
	if err != nil {
		return err
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}

	// 拉取、解析、转换、校验全在内存里完成，一步不碰数据库：校验不过的数据
	// 不能走进 Import——它会先在事务里删掉这一年上一次导入写下的行。
	syncCtx, cancelSync := context.WithTimeout(context.Background(), calendarSyncTimeout)
	defer cancelSync()
	doc, syncErr := fetchCalendarDocument(syncCtx, year)

	dbCtx, cancelDB := context.WithTimeout(context.Background(), calendarImportTimeout)
	defer cancelDB()
	// 走 openDB 而不是自己 sql.Open + Ping：打开与探活失败时的处置与 import
	// 相同（都归 exitDBUnreach），各写一遍的表现是同一类失败在两条命令上给出
	// 不同的退出码。
	db, err := openDB(dbCtx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// 先读一次这一年的当前状态，三条路（写入数据、数据源说未发布、拉取失败）共用
	// 同一个判断。Import 会在事务里先删掉这一年上一次写下的所有行，所以「已经
	// synced 就不许降级」不能只写在失败那条路上：数据源返回 404 或空 days 时，
	// 拿回来的是一份 pending 文档，把它写进去同样会清掉整年的判定，而状态看上
	// 去还是正常的 pending——list 看不出异常，计价规则却从那一天起整年失败。
	state, err := calendarstore.NewSource(db).YearState(dbCtx, calendar.RegionCN, year)
	if err != nil {
		return err
	}

	if syncErr == nil {
		if doc.Status == string(calendar.YearPending) && calendarYearHasVerdict(state) {
			_, _ = fmt.Fprintf(humanStdout(),
				"%d 年库里已有定论（%s），数据源这次表示未发布，未改动。\n", year, state)
			return nil
		}
		result, err := calendarstore.NewAdmin(db).Import(dbCtx, doc)
		if err != nil {
			return err
		}
		printCalendarImport(humanStdout(), result)
		return nil
	}

	// 真正的失败。库里那一年已经有定论就只报错——写一份不带 days 的文档进去
	// 会把那一年的好数据清空；否则记下 degraded。
	if calendarYearHasVerdict(state) {
		return fmt.Errorf("%w（%d 年库里已经有定论：%s，数据没有改动）", syncErr, year, state)
	}
	if _, err := calendarstore.NewAdmin(db).Import(dbCtx, degradedCalendarDocument(year, syncErr)); err != nil {
		// 两个错误都用 %w 包上：命令失败的主因是同步失败，而「连记录失败都失败了」
		// 也是一条要能分类的错误（例如库连不上）。
		return fmt.Errorf("%w；另外，把这次失败记进日历也失败了: %w", syncErr, err)
	}
	return fmt.Errorf("%w（这次失败已记进 %d 年的日历状态：degraded）", syncErr, year)
}

// calendarYearHasVerdict 报告库里这一年是不是已经有定论。
//
// 有定论的年份不允许被单源的 pending 或一次拉取失败降级——那两条路都会先删掉
// 这一年原有的行。「有定论」的取值见 runPriceCalendarSync 上面的状态机表：synced
// 与 conflict 都算，无记录 / pending / degraded 不算。
func calendarYearHasVerdict(state calendar.YearState) bool {
	return state == calendar.YearSynced || state == calendar.YearConflict
}

// syncTargetYear 是命令行实际走的那个入口：它把注入的「现在」接给 syncYear。
func syncTargetYear(raw string) (int, error) {
	return syncYear(raw, calendarSyncNow())
}

// syncYear 决定这一次同步哪一年。
//
// 缺省取「此刻在地区时区里是哪一年」，不是机器本地时区：一台跑在 UTC 的
// 服务器在元旦当天用本地时区算，会去拉上一年。
//
// 只接受一个年份：`--year 2025,2026` 报参数错，而不是悄悄只取第一个——
// 「想要两年，实际只写了一半」在库里看不出来。
func syncYear(raw string, now time.Time) (int, error) {
	if raw == "" {
		loc, err := calendar.Timezone(calendar.RegionCN)
		if err != nil {
			return 0, fmt.Errorf("取 %s 的时区失败: %w", calendar.RegionCN, err)
		}
		return now.In(loc).Year(), nil
	}
	year, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, usageErr(fmt.Errorf("--year 只接受一个年份（例如 2026），得到 %q", raw))
	}
	// 上界与 Document 的年份校验同一个范围：超出它，本年最后一天也构造不出来。
	if year < 1 || year > 9999 {
		return 0, usageErr(fmt.Errorf("--year %d 不在 [1, 9999] 内", year))
	}
	return year, nil
}

// fetchCalendarDocument 拉取并转换一年的日历数据。它不碰数据库。
//
// 返回的文档已经走过 ParseDocument 的校验：写库那一步拿到的必然是一份能通过
// validate 的数据，网络与数据本身的问题都留在这一侧。
func fetchCalendarDocument(ctx context.Context, year int) (calendarstore.Document, error) {
	url := fmt.Sprintf("%s/%d.json", holidayCNBaseURL, year)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return calendarstore.Document{}, fmt.Errorf("构造拉取 %d 年日历数据的请求失败: %w", year, err)
	}
	resp, err := calendarSyncHTTPClient.Do(req)
	if err != nil {
		return calendarstore.Document{}, fmt.Errorf("拉取 %d 年日历数据失败: %w", year, err)
	}
	// 关闭错误无处可报：响应体已经读完，而读完之后的关闭失败不会改变同步结果。
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// 数据源连这一年这一份文件都还没有。它与 days 为空是同一件事：
		// 这一年还没发布，不是失败。
		return validateCalendarDocument(pendingCalendarDocument(year))
	default:
		return calendarstore.Document{}, fmt.Errorf("拉取 %d 年日历数据失败: HTTP %d", year, resp.StatusCode)
	}

	var file holidayCNFile
	if err = json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return calendarstore.Document{}, fmt.Errorf("解析 %d 年的 holiday-cn 数据失败: %w", year, err)
	}
	doc, err := holidayCNDocument(year, file)
	if err != nil {
		return calendarstore.Document{}, err
	}
	return validateCalendarDocument(doc)
}

// validateCalendarDocument 用导入那条路自己的校验器验一遍要写库的文档。
//
// 绕一圈 JSON 是刻意的：Import 吃的就是这份格式，用同一个 ParseDocument 校验，
// 才不会出现「转换认为合法、导入时才发现不合法」——那时 Import 已经在事务里
// 删掉了这一年的原有行。
func validateCalendarDocument(doc calendarstore.Document) (calendarstore.Document, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return calendarstore.Document{}, fmt.Errorf("序列化 %d 年的日历数据失败: %w", doc.Year, err)
	}
	valid, err := calendarstore.ParseDocument(bytes.NewReader(body))
	if err != nil {
		return calendarstore.Document{}, err
	}
	return valid, nil
}

// holidayCNDocument 把 holiday-cn 的一份文件换成要写库的文档（纯函数）。
//
// 数据源只给每一天「放不放假」（isOffDay），调休补班日要自己推：不是假期、又
// 落在周六或周日，就是被挪来上班的那一天。落在周末的法定节日照常写
// off_day: true 并保留名称——「这一天本来就休息」不能成为跳过它的理由，
// 名称是账单解释「为什么这天算高峰」的依据。
//
// isOffDay 为 false 却不在周末的日子照样收下（off_day: false、
// makeup_workday: false、name 保留）：它对「这天是不是上班」的判定没有影响，
// 只影响 isMakeupWorkday()。数据源将来多出这样一天就整年导不进去，代价远大于
// 收益。
func holidayCNDocument(year int, file holidayCNFile) (calendarstore.Document, error) {
	if file.Year != year {
		return calendarstore.Document{}, fmt.Errorf(
			"数据源返回的 year 是 %d，与要同步的 %d 年不一致：拉错了文件就不能拿它当这一年的判定", file.Year, year)
	}
	if len(file.Days) == 0 {
		// 文件在、days 却是空的：数据源用这个形状表示「这一年还没发布」
		// （未发布年份是 200 + 空数组，不是 404）。两个形状业务上是同一件事。
		return pendingCalendarDocument(year), nil
	}

	sources := make(map[calendar.Date]holidayCNDaySource, len(file.Days))
	outside := make([]calendar.Date, 0, len(file.Days))
	for i, src := range file.Days {
		date, err := parseHolidayCNDate(src.Date)
		if err != nil {
			return calendarstore.Document{}, fmt.Errorf("数据源第 %d 天: %w", i+1, err)
		}
		if _, dup := sources[date]; dup {
			return calendarstore.Document{}, fmt.Errorf("数据源里 %s 出现了两次", date)
		}
		sources[date] = src
		if date.Year != year {
			outside = append(outside, date)
		}
	}
	// 年外的日子排在最后，且按日期排序：输出与数据源里的先后无关，同一份输入
	// 永远产出同一份文件。
	sort.Slice(outside, func(i, j int) bool { return outside[i].String() < outside[j].String() })

	days := make([]calendarstore.Day, 0, 366+len(outside))
	// 这条路必须与覆盖检查（validateDays）用同一份日期列表：两处各算一遍的
	// 话，闰年这类边界会在一边对、一边错。
	for _, date := range calendarstore.DaysOfYear(year) {
		src, ok := sources[date]
		days = append(days, holidayCNDay(date, src, ok))
	}
	for _, date := range outside {
		days = append(days, holidayCNDay(date, sources[date], true))
	}

	return calendarstore.Document{
		Region: string(calendar.RegionCN),
		Year:   year,
		Status: string(calendar.YearSynced),
		Source: "holiday-cn",
		// 双源交叉校验不写：只有第二个源出现时它才有意义，现在写等于写一个
		// 恒为 true 的字段。
		SourcePaperURLs: file.Papers,
		Days:            days,
	}, nil
}

// holidayCNDay 把「数据源里有没有这一天」折成导入文件里的一天。
func holidayCNDay(date calendar.Date, src holidayCNDaySource, inSource bool) calendarstore.Day {
	if !inSource {
		// 数据源没有这一天：周末按休息、平日按上班。两档都是「这一天没有
		// 特殊安排」的意思，名称留空。
		offDay := date.IsWeekend()
		return calendarstore.Day{Date: date.String(), OffDay: &offDay}
	}
	offDay := src.IsOffDay
	// 补班日是「不是假期、又落在周末」的那一天；其余日子都不补班。
	makeup := !offDay && date.IsWeekend()
	return calendarstore.Day{
		Date:          date.String(),
		Name:          src.Name,
		OffDay:        &offDay,
		MakeupWorkday: makeup,
	}
}

// pendingCalendarDocument 是一份「这一年还没发布」的文档：只记年份状态，
// 不带 days 与 source（那两种字段只用于已定论的年份）。
func pendingCalendarDocument(year int) calendarstore.Document {
	return calendarstore.Document{
		Region: string(calendar.RegionCN),
		Year:   year,
		Status: string(calendar.YearPending),
	}
}

// degradedCalendarDocument 是一份同步失败的记录。
func degradedCalendarDocument(year int, cause error) calendarstore.Document {
	return calendarstore.Document{
		Region:    string(calendar.RegionCN),
		Year:      year,
		Status:    string(calendar.YearDegraded),
		LastError: cause.Error(),
	}
}

// parseHolidayCNDate 按 YYYY-MM-DD 解数据源里的一个日期。
//
// 与 store 里那份同一个判据：time.Parse 同时挡住格式不对（"2026-1-5"）与日期
// 不成立（"2026-02-30"）两类输入，而这两类都不该被规范化成另一天。
func parseHolidayCNDate(raw string) (calendar.Date, error) {
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return calendar.Date{}, fmt.Errorf("日期 %q 不是 YYYY-MM-DD 格式: %w", raw, err)
	}
	date := calendar.Date{Year: parsed.Year(), Month: parsed.Month(), Day: parsed.Day()}
	if !date.Valid() {
		return calendar.Date{}, fmt.Errorf("%w: %s", calendar.ErrInvalidDate, date)
	}
	return date, nil
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
	fs := newFlagSet("price calendar list", flag.ContinueOnError)
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
