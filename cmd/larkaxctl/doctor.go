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
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/pricing"
)

const doctorUsage = `larkaxctl doctor 逐项检查「配好的链路能不能接一次请求」。

用法：
  larkaxctl doctor
        只读检查六项，不碰网络

  larkaxctl doctor --live --model <规范名> [--endpoint chat|messages]
                   [--base-url <URL>] (--credential-stdin | --credential-env <变量名>)
        额外真打一次上游

它按顺序回答六个问题，每一项都给出「检查了什么、结论是什么」：

  1. 数据库    连得上吗、迁移跑到哪一版
  2. 渠道      有没有至少一条 enabled 且 models 非空的渠道
  3. 模型目录  渠道 models 里出现的每个名字都在 model_catalog 里吗
  4. 别名      配过别名的规范名有没有渠道接得住；可服务的规范名在每个服务它的
               渠道上能不能解析出上游名
  5. 计价规则  可服务的规范名在每个服务它的渠道上都有生效的规则吗
  6. 钱包      有用户吗、每个用户都有钱包行吗、有没有一个钱包余额大于 0

第 3、4、5 项回答的是同一个字符串在两个数组里对不上的问题：渠道的 --models
与目录里的规范名差一个字符，或者模型建了却没加进任何渠道的白名单——那时调用
回 503「no available upstream for the requested model」，而那句话不会说是
哪一个名字不对。第 6 项把「用户没有钱包行」这类缺口在第一次调用之前指出来，
而不是让人从 500 往回推。

**前六项只读**：不写任何一行、不建任何东西、不发网络请求。第七项要花钱、
要凭证，默认不跑。

--live 真打一次上游：往公共入口发一次非流式推理请求，把状态码、耗时与用量
打出来。**凭证不能写在命令行参数里**（参数会出现在 ps 的输出与 shell 历史里），
只能从标准输入或环境变量读，与 channel create 同一个做法：

  --credential-stdin        从标准输入读到文件结束（末尾的换行会被去掉）
  --credential-env <变量名> 从名为 <变量名> 的环境变量读

这里读的是打网关用的 **API Key**（apikey create 发的那把），不是渠道凭证。

--model 是这次请求要用的**规范模型名**（第 3 项建的那个）。--endpoint 选打哪个
入口：chat 是 POST /v1/chat/completions（Authorization: Bearer），messages 是
POST /v1/messages（x-api-key）。--base-url 默认 http://127.0.0.1:8080，与主服务
LARKAX_HTTP_ADDR 的默认值一致；网关不在本机时要显式给。

退出码：0 表示六项都通过（--live 给了的话那一次也成功）、1 表示有问题或某一项
没跑成、3 表示连不上数据库。**「没跑成」与「通过」是两件事**：没有可检查的
对象时那一项报「没跑成」而不是「通过」，退出码也非 0——两者在安静的终端里
长得一样，是这个项目反复踩的坑（见 LESSONS.md 第 16 条）。

例：

  larkaxctl doctor
  larkaxctl doctor --live --model claude-x --credential-env LARKAX_API_KEY
`

// doctorTimeout 是六项只读检查的时间上限。
//
// 它们全是小查询，但「可服务的规范名 × 服务它的渠道」那一项是逐组查的，
// 模型多、渠道多时组数会涨。给两分钟：够跑完一个中等规模的库，又不至于
// 让一次卡住的查询挂到人发现为止。
const doctorTimeout = 2 * time.Minute

// doctorLiveTimeout 是真打一次上游的时间上限。
//
// 上游本身可能慢（长上下文、推理模型），但这里打的是一个只有几个 token 的
// 请求；一分钟还没回来，通常说明这条链路上有别的东西在挡着。
const doctorLiveTimeout = 60 * time.Second

// doctorMaxListed 是每一项最多列出多少条问题。
//
// 一个配错的库可能有几十条同类问题，全打出来会把别的项挤走。超出的部分打
// 一行「另有 N 条」——不列出来，但也不假装没有。
const doctorMaxListed = 20

// doctorMaxBodyBytes 是真打上游时最多读回多少响应体。
//
// 读它只是为了把状态码与错误说明打出来，而一个代理返回的错误页可能有几 MB。
const doctorMaxBodyBytes = 64 << 10

// doctorPreviewRunes 是响应体不是 JSON 时原样打出的字符数。
const doctorPreviewRunes = 160

// doctorLivePrompt 是真打上游时发的那句话。
//
// 要的是「上游收到了、回了一个正常的响应」，不是一个好答案，所以取最短的
// 一句。它也会进账单，虽然只有几个 token。
const doctorLivePrompt = "hi"

// doctorLiveMaxTokens 是真打上游时这次请求的 max_tokens，两个入口共用。
//
// 网关按它算预留上界（见 pipeline.Request.validate 与 billing.ErrNoUpperBound），
// 所以两个入口都必须带；给得小是为了让这次调用的花费有个上界。
const doctorLiveMaxTokens = 16

// 真打上游的两个入口。
//
// 路径与 internal/api 的 PathChatCompletions / PathMessages 是同一个值，
// doctor_test.go 里有用例钉住这一点。本包不 import internal/api：那会把整个
// 入口层（含 gateway）拖进运维命令的二进制里，而这里要的只是两个字符串。
const (
	doctorEndpointChat     = "chat"
	doctorEndpointMessages = "messages"

	doctorPathChat     = "/v1/chat/completions"
	doctorPathMessages = "/v1/messages"
)

// doctorDefaultBaseURL 与主服务 LARKAX_HTTP_ADDR 的默认值一致。
//
// 不读那个环境变量：它是**监听**地址，可能写成「所有网卡」那一种，而那不是
// 一个能连的地址。让使用者显式给出比在这里替他猜一个更可靠。
const doctorDefaultBaseURL = "http://127.0.0.1:8080"

// doctorHTTPClient 是真打上游用的客户端。
//
// 它是一个变量而不是直接调 http.DefaultClient，好让用例把传输层换成假的
// ——「请求没发出去」那条分支只有换掉它才测得到。
var doctorHTTPClient = &http.Client{Timeout: doctorLiveTimeout}

// 每一项的「检查了什么」。
//
// 单独列出来是为了让「这一项查的是什么」与「这一项查到了什么」在输出里
// 各占一行：只打结果的话，一个没问题的空结果看不出它到底查了没有。
const (
	doctorSubjectDatabase = "连得上、迁移跑到哪一版"
	doctorSubjectChannel  = "有没有至少一条 enabled 且 models 非空的渠道"
	doctorSubjectCatalog  = "enabled 渠道的 models 里出现的每个名字都在 model_catalog 里吗"
	doctorSubjectAlias    = "配过别名的规范名有没有渠道接得住；可服务的规范名在每个服务它的渠道上能不能解析出上游名"
	doctorSubjectPricing  = "可服务的规范名在每个服务它的渠道上都有生效的计价规则吗"
	doctorSubjectWallet   = "有用户吗、每个用户都有钱包行吗、有没有一个钱包余额大于 0"
)

// ── 命令入口 ────────────────────────────────────────────────────────────

// doctorOptions 是 doctor 的参数。
//
// key 只在 --live 时有值。它不进日志、不进错误信息、不进任何一行输出：
// 全仓库唯一读它的地方是发请求时设置请求头。
type doctorOptions struct {
	live     bool
	model    string
	endpoint string
	baseURL  string
	key      string
}

func runDoctor(args []string) error {
	opts, err := parseDoctorOptions(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	// 连不上库时**不直接返回**：报告里「第 1 项没跑成、其余五项也没跑成」
	// 比一句「连接数据库失败」有用得多，而那是这个命令最要紧的一次输出。
	db, dbErr := openDB(ctx, cfg)

	var rep doctorReport
	if dbErr != nil {
		rep = doctorReportWithoutDB(ctx, dbErr, opts)
	} else {
		defer func() { _ = db.Close() }()
		rep = collectDoctorReport(ctx, db, opts)
	}

	printDoctorReport(os.Stdout, rep)
	return rep.exitErr(dbErr)
}

// parseDoctorOptions 解析并校验参数。
//
// 全部检查发生在连库之前（与其余子命令同一条约定）：一个写错的命令不该先
// 连上生产库再报错。
func parseDoctorOptions(args []string) (doctorOptions, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, doctorUsage) }
	live := fs.Bool("live", false, "真打一次上游（会花钱、要凭证）")
	model := fs.String("model", "", "真打上游时用的规范模型名")
	endpoint := fs.String("endpoint", doctorEndpointChat, "真打上游打哪个入口：chat 或 messages")
	baseURL := fs.String("base-url", doctorDefaultBaseURL, "网关公共入口的地址")
	credentialStdin := fs.Bool("credential-stdin", false, "从标准输入读 API Key")
	credentialEnv := fs.String("credential-env", "", "从指定环境变量读 API Key")
	if err := fs.Parse(args); err != nil {
		return doctorOptions{}, err
	}
	if fs.NArg() > 0 {
		return doctorOptions{}, fmt.Errorf("doctor 不接受位置参数，得到 %q", fs.Arg(0))
	}

	opts := doctorOptions{live: *live, endpoint: *endpoint, baseURL: *baseURL}

	if !*live {
		// 只有 --live 才用得上这些标志。静默忽略它们会让「我给了 --model
		// 但它没生效」看起来像一次成功的检查——而那正是这一项要防的东西。
		given := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
		for _, name := range []string{"model", "endpoint", "base-url", "credential-stdin", "credential-env"} {
			if given[name] {
				return doctorOptions{}, fmt.Errorf(
					"--%s 只在 --live 时才有意义：默认那六项不碰网络", name)
			}
		}
		return opts, nil
	}

	if strings.TrimSpace(*model) == "" {
		return doctorOptions{}, errors.New("--live 需要 --model（这次请求要用的规范模型名）")
	}
	opts.model = *model

	spec, err := lookupDoctorEndpoint(*endpoint)
	if err != nil {
		return doctorOptions{}, err
	}
	opts.endpoint = spec.name

	if baseErr := validateDoctorBaseURL(*baseURL); baseErr != nil {
		return doctorOptions{}, baseErr
	}
	opts.baseURL = strings.TrimRight(*baseURL, "/")

	key, err := readCredential(*credentialStdin, *credentialEnv, true)
	if err != nil {
		return doctorOptions{}, err
	}
	opts.key = *key
	return opts, nil
}

// validateDoctorBaseURL 校验网关地址。
//
// 在参数解析那一步就拒掉，而不是等请求发不出去：一个少了 http:// 的地址会
// 被 net/http 当成相对路径，报出来的错是「unsupported protocol scheme」，
// 而它读起来像是服务端的问题。
func validateDoctorBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		// 把期望的形状写出来：少写 http:// 的地址（127.0.0.1:8080）会让
		// url.Parse 报一句「first path segment in URL cannot contain colon」，
		// 那句话读起来像内部错误，而不像「你少写了协议」。
		return fmt.Errorf("--base-url %q 不是合法的 URL（要形如 http://127.0.0.1:8080）: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("--base-url %q 必须以 http:// 或 https:// 开头", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("--base-url %q 里没有主机名", raw)
	}
	return nil
}

// ── 报告的形状 ──────────────────────────────────────────────────────────

// doctorStatus 是一项检查的结论。
//
// 「没跑成」单独一档：它与「通过」在输出里必须分得开，否则一次什么都没查
// 的运行看起来像一次全部通过的运行（见 LESSONS.md 第 16 条）。
type doctorStatus int

const (
	doctorPassed doctorStatus = iota
	doctorFailed
	doctorSkipped
)

func (s doctorStatus) label() string {
	switch s {
	case doctorPassed:
		return "通过"
	case doctorFailed:
		return "不通过"
	default:
		return "没跑成"
	}
}

// doctorCheck 是一项检查的结果。
//
// findings 与 skip 是两种不同的坏消息，分开存是为了让输出能把它们说清楚：
// findings 是「查了，有问题」，skip 是「没查成」。两者都让退出码非 0。
type doctorCheck struct {
	title    string
	subject  string
	notes    []string
	findings []string
	skip     string
	table    *doctorTable
}

// status 给出这一项的结论。有问题时以「不通过」为准——一个具体的问题比
// 「还有一半没查」更该被先看见，而没查成的那一半仍然会单独打一行。
func (c doctorCheck) status() doctorStatus {
	switch {
	case len(c.findings) > 0:
		return doctorFailed
	case c.skip != "":
		return doctorSkipped
	}
	return doctorPassed
}

// doctorTable 是某一项要打出来的一张表。
type doctorTable struct {
	header []string
	rows   [][]string
}

// doctorReport 是六项只读检查的结果，加上可选的那一次真打上游。
type doctorReport struct {
	database doctorCheck
	channel  doctorCheck
	catalog  doctorCheck
	alias    doctorCheck
	pricing  doctorCheck
	wallet   doctorCheck
	live     doctorLive
}

// checks 按输出顺序给出六项检查。
func (r doctorReport) checks() []doctorCheck {
	return []doctorCheck{r.database, r.channel, r.catalog, r.alias, r.pricing, r.wallet}
}

func (r doctorReport) counts() (passed, failed, skipped int) {
	for _, c := range r.checks() {
		switch c.status() {
		case doctorPassed:
			passed++
		case doctorFailed:
			failed++
		case doctorSkipped:
			skipped++
		}
	}
	return passed, failed, skipped
}

// exitErr 把报告折成一个退出码。
//
// 连不上库时原样返回那个错误：它已经被标成 exitDBUnreach（见 openDB），
// 而那是既有的对外契约，doctor 不该把它变成「其他失败」。
func (r doctorReport) exitErr(dbErr error) error {
	if dbErr != nil {
		return dbErr
	}
	_, failed, skipped := r.counts()
	if failed > 0 || skipped > 0 {
		return fmt.Errorf("doctor 不通过：%d 项有问题、%d 项没跑成", failed, skipped)
	}
	if r.live.enabled && !r.live.ok() {
		return errors.New("doctor 不通过：真打上游那一次没有成功")
	}
	return nil
}

// ── 收集 ────────────────────────────────────────────────────────────────

// doctorReportWithoutDB 是连不上库时的报告：六项全部「没跑成」。
//
// 真打上游仍然照跑：它走 HTTP，不读库。在一台连不上库的跳板机上，「网关是
// 活的、只是我这里看不到库」与「整条链路都挂了」是两件事，而这个区别只有
// 把那一项也跑一遍才看得出来。
func doctorReportWithoutDB(ctx context.Context, dbErr error, opts doctorOptions) doctorReport {
	// 只有第 1 项带完整的错误原文，后面五项指向它。原样重复六遍的后果是
	// 一份四十行的报告里六行完全相同，而那句错误本身有两行长——读者要在
	// 噪声里找出「到底哪一项跟别的不一样」，而这次一项都不一样。
	// 完整原文还出现在最后那行 `larkaxctl: ...` 里，不会丢。
	full := fmt.Sprintf("连不上数据库：%v", dbErr)
	same := "连不上数据库（同第 1 项）：这一项也要读库"
	blocked := func(title, subject, reason string) doctorCheck {
		return doctorCheck{title: title, subject: subject, skip: reason}
	}
	return doctorReport{
		database: blocked("数据库", doctorSubjectDatabase, full),
		channel:  blocked("渠道", doctorSubjectChannel, same),
		catalog:  blocked("模型目录", doctorSubjectCatalog, same),
		alias:    blocked("别名", doctorSubjectAlias, same),
		pricing:  blocked("计价规则", doctorSubjectPricing, same),
		wallet:   blocked("钱包", doctorSubjectWallet, same),
		live:     runDoctorLive(ctx, opts),
	}
}

// collectDoctorReport 跑六项只读检查与可选的那一次真打上游。
func collectDoctorReport(ctx context.Context, db *sql.DB, opts doctorOptions) doctorReport {
	rep := doctorReport{database: checkDoctorDatabase(ctx, db)}

	in, err := gatherDoctorChain(ctx, db)
	if err != nil {
		// 一次读失败就不必再分项报错了：后面四项读的是同一批表，它们会
		// 一起失败，而四条一模一样的错误只会把原因埋掉。
		reason := fmt.Sprintf("读库失败：%v", err)
		blocked := func(title, subject string) doctorCheck {
			return doctorCheck{title: title, subject: subject, skip: reason}
		}
		rep.channel = blocked("渠道", doctorSubjectChannel)
		rep.catalog = blocked("模型目录", doctorSubjectCatalog)
		rep.alias = blocked("别名", doctorSubjectAlias)
		rep.pricing = blocked("计价规则", doctorSubjectPricing)
		rep.wallet = blocked("钱包", doctorSubjectWallet)
		rep.live = runDoctorLive(ctx, opts)
		return rep
	}

	rep.channel = checkDoctorChannels(in)
	rep.catalog = checkDoctorCatalog(in)
	rep.alias, rep.pricing = checkDoctorAliasAndPricing(ctx, db, in)
	rep.wallet = checkDoctorWallets(in)
	rep.live = runDoctorLive(ctx, opts)
	return rep
}

// doctorChainInput 是四项链路检查共用的一份读出来的数据。
//
// 一次读齐再逐项判定，而不是每项各查一遍：那样同一批表会被读四次，而
// 「两次读之间库变了」会让同一份报告里的两行互相矛盾。
type doctorChainInput struct {
	channels     []channels.ChannelInfo
	catalogNames []string
	catalogSet   map[string]bool
	aliases      []catalog.Alias
	users        []doctorUser
}

// doctorUser 是一个用户与它的钱包状态。
type doctorUser struct {
	ID   int64
	Name string
	// HasWallet 是「这个用户在 user_wallets 里有行」。
	HasWallet bool
	// Funded 是「至少有一行的余额大于 0」。
	Funded bool
}

// gatherDoctorChain 读齐四项链路检查要的全部数据。
func gatherDoctorChain(ctx context.Context, db *sql.DB) (doctorChainInput, error) {
	var in doctorChainInput

	chs, err := channels.NewAdmin(db, nil, nil).List(ctx)
	if err != nil {
		return in, err
	}
	in.channels = chs

	cat := catalog.New(db)
	models, err := cat.Models(ctx)
	if err != nil {
		return in, err
	}
	in.catalogSet = make(map[string]bool, len(models))
	for _, m := range models {
		in.catalogSet[m.CanonicalName] = true
		in.catalogNames = append(in.catalogNames, m.CanonicalName)
	}
	slices.Sort(in.catalogNames)

	// 全部别名要两种范围各取一遍：Aliases 一次只列一个范围（见它的注释），
	// 而「配过别名但没有渠道接得住」那一项要把两种范围都算进来。
	//
	// 循环里的 err 另起一个名字：它与上面那个同处一个作用域，遮蔽之后
	// govet 的 shadow 检查器会报（与 db.go 的 pingErr 同一条约定）。
	platform, err := cat.Aliases(ctx, 0)
	if err != nil {
		return in, err
	}
	in.aliases = append(in.aliases, platform...)
	for _, ch := range chs {
		scoped, scopedErr := cat.Aliases(ctx, ch.ID)
		if scopedErr != nil {
			return in, scopedErr
		}
		in.aliases = append(in.aliases, scoped...)
	}

	users, err := readDoctorUsers(ctx, db)
	if err != nil {
		return in, err
	}
	in.users = users
	return in, nil
}

// readDoctorUsers 读用户与钱包行的对应关系。
//
// 一个用户可能有多种资产的钱包行，所以按用户聚合而不是直接 join 出多行：
// 那会让「用户 3 有钱包」在两行上各说一次，而计数也会跟着虚高。
func readDoctorUsers(ctx context.Context, db *sql.DB) ([]doctorUser, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT u.id,
		       u.username::text,
		       count(w.user_id) > 0,
		       COALESCE(bool_or(w.balance > 0), false)
		  FROM users u
		  LEFT JOIN user_wallets w ON w.user_id = u.id
		 GROUP BY u.id, u.username
		 ORDER BY u.id`)
	if err != nil {
		return nil, fmt.Errorf("查询用户与钱包失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []doctorUser
	for rows.Next() {
		var u doctorUser
		if err := rows.Scan(&u.ID, &u.Name, &u.HasWallet, &u.Funded); err != nil {
			return nil, fmt.Errorf("读取用户与钱包失败: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户与钱包失败: %w", err)
	}
	return out, nil
}

// ── 第 1 项：数据库 ─────────────────────────────────────────────────────

// checkDoctorDatabase 查连得上与迁移版本。
//
// 连接本身由 openDB 证明（走到这里说明连上了），所以这一项回答的是版本。
func checkDoctorDatabase(ctx context.Context, db *sql.DB) doctorCheck {
	c := doctorCheck{title: "数据库", subject: doctorSubjectDatabase}

	if err := migrate.SetDialect("postgres"); err != nil {
		c.skip = fmt.Sprintf("设置迁移方言失败：%v", err)
		return c
	}
	goose.SetBaseFS(migrate.FS())

	latest, latestErr := latestEmbeddedMigration()
	if latestErr != nil {
		c.notes = append(c.notes, fmt.Sprintf("读不出二进制里的迁移清单：%v", latestErr))
	} else {
		c.notes = append(c.notes, fmt.Sprintf("二进制里最新的迁移版本是 %d", latest))
	}

	// 先只读地问一句「版本表在不在」：goose 的 GetDBVersionContext 在表不存在
	// 时会**建表并插入一条 0**（见 goose 的 EnsureDBVersionContext），而这一项
	// 承诺只读。表不存在等价于「一条迁移都没跑」，那个结论不建表也说得出来。
	exists, err := gooseVersionTableExists(ctx, db)
	if err != nil {
		c.skip = fmt.Sprintf("查 %s 表是否存在失败：%v", goose.TableName(), err)
		return c
	}
	if !exists {
		c.notes = append(c.notes, fmt.Sprintf("库里没有 %s 表：一条迁移都没跑过", goose.TableName()))
		c.findings = append(c.findings,
			"迁移一条都没跑：先跑 larkaxctl migrate up，否则下面五项查不到任何东西")
		return c
	}

	version, err := goose.GetDBVersionContext(ctx, db)
	switch {
	case errors.Is(err, goose.ErrNoNextVersion):
		// 表在、一条已应用的记录都没有。它是「版本 0」而不是一次失败。
		version = 0
	case err != nil:
		c.skip = fmt.Sprintf("读迁移版本失败：%v", err)
		return c
	}
	c.notes = append(c.notes, fmt.Sprintf("库里的迁移版本是 %d", version))

	if latestErr != nil {
		return c
	}
	switch {
	case version < latest:
		c.findings = append(c.findings, fmt.Sprintf(
			"迁移没跑完：库里是 %d，二进制里已经有 %d——跑 larkaxctl migrate up", version, latest))
	case version > latest:
		c.findings = append(c.findings, fmt.Sprintf(
			"库里的版本 %d 比这个二进制里的 %d 还新：部署的二进制比库旧，先确认版本", version, latest))
	}
	return c
}

// gooseVersionTableExists 只读地判断 goose 的版本表在不在。
//
// to_regclass 对不存在的表返回 NULL 而不是报错——「表不存在」在这里是一个
// 结论，不是一次失败。
func gooseVersionTableExists(ctx context.Context, db *sql.DB) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, goose.TableName()).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// latestEmbeddedMigration 给出二进制里内嵌的最新迁移版本。
//
// 用 goose 自己的清单而不是自己解析文件名：版本号怎么从文件名里取出来是
// goose 的规则（前缀、分隔符、允许哪些后缀），抄一份就会在它改规则时漂移。
func latestEmbeddedMigration() (int64, error) {
	migrations, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, m := range migrations {
		if m.Version > latest {
			latest = m.Version
		}
	}
	return latest, nil
}

// ── 第 2 项：渠道 ───────────────────────────────────────────────────────

func checkDoctorChannels(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "渠道", subject: doctorSubjectChannel}
	c.notes = append(c.notes, fmt.Sprintf("库里共 %d 条渠道", len(in.channels)))

	enabled, usable := 0, 0
	for _, ch := range in.channels {
		if ch.Status != channels.StatusEnabled {
			continue
		}
		enabled++
		if len(ch.Models) == 0 {
			c.findings = append(c.findings, fmt.Sprintf(
				"渠道 %d（%s）是 enabled，但 models 是空的：读取路径把 models 当白名单用，"+
					"这条渠道永远选不中", ch.ID, ch.Name))
			continue
		}
		usable++
	}
	c.notes = append(c.notes, fmt.Sprintf("enabled 的 %d 条，其中 models 非空的 %d 条", enabled, usable))

	if enabled == 0 {
		c.findings = append(c.findings,
			"没有任何 enabled 的渠道：请求会回 503「no available upstream for the requested model」")
	}
	return c
}

// ── 第 3 项：模型目录 ───────────────────────────────────────────────────

func checkDoctorCatalog(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "模型目录", subject: doctorSubjectCatalog}

	referenced := referencedDoctorModels(in)
	if len(referenced) == 0 {
		c.skip = "没有 enabled 且 models 非空的渠道可查（见上面「渠道」那一项）"
		return c
	}
	c.notes = append(c.notes,
		fmt.Sprintf("范围：%d 个名字（enabled 渠道的 models 去重后）", len(referenced)))

	for _, name := range referenced {
		if in.catalogSet[name] {
			continue
		}
		c.findings = append(c.findings, fmt.Sprintf(
			"%s 的 models 里有 %q，但目录里没有这个名字：这次调用会回 503「no available "+
				"upstream for the requested model」。多半是拼写差了一个字符，或者那条模型已经被删掉了",
			doctorChannelsListing(in.channels, name), name))
	}
	return c
}

// referencedDoctorModels 是 enabled 渠道的 models 里出现过的名字，去重后排序。
func referencedDoctorModels(in doctorChainInput) []string {
	seen := map[string]bool{}
	var names []string
	for _, ch := range in.channels {
		if ch.Status != channels.StatusEnabled {
			continue
		}
		for _, name := range ch.Models {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// doctorChannelsListing 列出引用了某个模型名的 enabled 渠道。
//
// 每一段都带「渠道」两个字，所以多个渠道时读起来是「渠道 1（a）、渠道 3（b）」，
// 而不是一个光秃秃的「1（a）、3（b）」——后者得回去看上文才知道那是渠道 ID。
func doctorChannelsListing(chs []channels.ChannelInfo, model string) string {
	var parts []string
	for _, ch := range chs {
		if ch.Status != channels.StatusEnabled || !slices.Contains(ch.Models, model) {
			continue
		}
		parts = append(parts, "渠道 "+doctorChannelLabel(ch.ID, ch.Name))
	}
	if len(parts) == 0 {
		return "（没有）"
	}
	return strings.Join(parts, "、")
}

// ── 第 4、5 项：别名与计价规则 ──────────────────────────────────────────

// doctorPair 是一个「规范名 × 服务它的渠道」的组合。
//
// 别名解析与计价规则都按这一组问：同一个规范名在不同渠道上可能解析出不同的
// 上游名，而渠道级计价规则只对那一条渠道生效。按模型问一次会把这两件事都
// 问漏。
type doctorPair struct {
	model       string
	channelID   int64
	channelName string

	upstream      string
	upstreamErr   error
	upstreamFatal error

	ruleID    int64
	ruleName  string
	ruleErr   error
	ruleFatal error
}

// checkDoctorAliasAndPricing 查第 4、5 两项。
//
// 两项共用同一批「组」，所以一起算；判据各归各的结论。
//
// 别名解析走的是选路用的那条查询（catalog.Catalog.UpstreamModel），计价规则
// 走的是裁决用的那条（pricing.Admin.Explain），都不在这里另写一份判据——
// 两处各写一份时，漂移的表现是 doctor 说通、请求却失败。
func checkDoctorAliasAndPricing(ctx context.Context, db *sql.DB, in doctorChainInput) (doctorCheck, doctorCheck) {
	aliasCheck := doctorCheck{title: "别名", subject: doctorSubjectAlias}
	priceCheck := doctorCheck{title: "计价规则", subject: doctorSubjectPricing}

	pairs := doctorPairs(in)
	servable := map[string]bool{}
	for _, p := range pairs {
		servable[p.model] = true
	}

	aliasNames := doctorAliasNames(in.aliases)
	aliasCheck.notes = append(aliasCheck.notes,
		fmt.Sprintf("配过别名的规范名 %d 个", len(aliasNames)))
	aliasCheck.findings = append(aliasCheck.findings, doctorAliasReachability(in.aliases, servable)...)

	if len(pairs) == 0 {
		reason := "没有可服务的规范名（enabled 渠道的 models 与目录的交集是空的，见上面「渠道」与「模型目录」两项）"
		aliasCheck.skip = reason
		priceCheck.skip = reason
		return aliasCheck, priceCheck
	}

	cat := catalog.New(db)
	priceAdmin := pricing.NewAdmin(db)
	now := time.Now()

	for i := range pairs {
		p := &pairs[i]

		upstream, err := cat.UpstreamModel(ctx, p.channelID, p.model)
		switch {
		case err == nil:
			p.upstream = upstream
		case errors.Is(err, catalog.ErrAliasNotFound):
			p.upstreamErr = err
		default:
			p.upstreamFatal = err
		}

		rules, err := priceAdmin.Explain(ctx, pricing.ExplainQuery{
			CanonicalModel: p.model,
			ChannelID:      p.channelID,
			At:             now,
		})
		switch {
		case err == nil && len(rules) > 0:
			p.ruleID, p.ruleName = rules[0].ID, rules[0].Name
		case errors.Is(err, pipeline.ErrNoRule):
			p.ruleErr = err
		case err != nil:
			p.ruleFatal = err
		}
	}

	aliasCheck.notes = append(aliasCheck.notes,
		fmt.Sprintf("别名解析：%d 组（可服务的规范名 × 服务它的渠道）", len(pairs)))
	aliasCheck.table = &doctorTable{header: []string{"规范名", "渠道", "上游名", "结论"}}
	priceCheck.notes = append(priceCheck.notes,
		fmt.Sprintf("计价规则：%d 组（可服务的规范名 × 服务它的渠道）", len(pairs)))
	priceCheck.table = &doctorTable{header: []string{"规范名", "渠道", "规则", "结论"}}

	for _, p := range pairs {
		if p.upstreamFatal == nil {
			upstream, verdict := p.upstream, "没有别名，按同名发"
			switch {
			case p.upstreamErr != nil:
				upstream, verdict = "（解析不出）", "别名没覆盖这个渠道"
				aliasCheck.findings = append(aliasCheck.findings, fmt.Sprintf(
					"「%s」在渠道 %d（%s）上解析不出上游名：这个规范名配过别名，但没有一条覆盖这个"+
						"渠道，这次调用会回 500。补一条渠道级别名、或加一条平台级别名即可",
					p.model, p.channelID, p.channelName))
			case p.upstream != p.model:
				verdict = "有别名，按它翻译"
			}
			aliasCheck.table.rows = append(aliasCheck.table.rows,
				[]string{p.model, doctorChannelLabel(p.channelID, p.channelName), upstream, verdict})
		}

		if p.ruleFatal == nil {
			rule, verdict := fmt.Sprintf("%d（%s）", p.ruleID, p.ruleName), "有生效的规则"
			if p.ruleErr != nil {
				rule, verdict = "（没有）", "没有适用的规则"
				priceCheck.findings = append(priceCheck.findings, fmt.Sprintf(
					"「%s」在渠道 %d（%s）上没有生效的计价规则：这次调用会失败，而不是按零价放行。"+
						"补一条规则，或者让某条规则的 --models 留空当兜底",
					p.model, p.channelID, p.channelName))
			}
			priceCheck.table.rows = append(priceCheck.table.rows,
				[]string{p.model, doctorChannelLabel(p.channelID, p.channelName), rule, verdict})
		}
	}

	aliasCheck.skip = doctorPairFatalSkip(pairs, func(p doctorPair) error { return p.upstreamFatal }, "别名解析")
	priceCheck.skip = doctorPairFatalSkip(pairs, func(p doctorPair) error { return p.ruleFatal }, "计价规则")
	return aliasCheck, priceCheck
}

// doctorPairFatalSkip 把「有几组根本没查成」折成一句说明，没有时返回空串。
func doctorPairFatalSkip(pairs []doctorPair, fatalOf func(doctorPair) error, what string) string {
	var count int
	var first error
	for _, p := range pairs {
		if err := fatalOf(p); err != nil {
			count++
			if first == nil {
				first = err
			}
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("%s有 %d 组没查成（表里没有它们）：%v", what, count, first)
}

// doctorPairs 列出所有「规范名 × 服务它的渠道」的组合。
//
// 只算 enabled、models 非空、且名字在目录里的那些：不在目录里的名字已经由
// 第 3 项报出来了，在这里再问一遍别名与计价只会把同一个问题说三次。
func doctorPairs(in doctorChainInput) []doctorPair {
	var pairs []doctorPair
	for _, ch := range in.channels {
		if ch.Status != channels.StatusEnabled || len(ch.Models) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, name := range ch.Models {
			if seen[name] || !in.catalogSet[name] {
				continue
			}
			seen[name] = true
			pairs = append(pairs, doctorPair{model: name, channelID: ch.ID, channelName: ch.Name})
		}
	}
	// 渠道本身按 id 排好了，但一个渠道里的 models 是库里的数组顺序，
	// 同一个库两次跑出来的表必须逐行相同。
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].model != pairs[j].model {
			return pairs[i].model < pairs[j].model
		}
		return pairs[i].channelID < pairs[j].channelID
	})
	return pairs
}

// doctorAliasReachability 报出「配过别名、却没有任何渠道接得住」的规范名。
//
// 这是「两张表各看一半都对、合起来不对」的那一半：别名表说这个模型配过名字，
// 渠道白名单里却没有它，于是调用回 503，而那句话说的是「没有可用上游」，
// 排查方向指向选路而不是配置。
//
// servable 是「有 enabled 渠道服务、且名字在目录里」的那些规范名。
func doctorAliasReachability(aliases []catalog.Alias, servable map[string]bool) []string {
	var findings []string
	for _, name := range doctorAliasNames(aliases) {
		if servable[name] {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"「%s」配过别名（%s），但没有任何 enabled 渠道的 models 里包含它：这次调用会回 "+
				"503「no available upstream for the requested model」。别名表与渠道白名单各看一半都对，合起来不对",
			name, doctorAliasScopes(aliases, name)))
	}
	return findings
}

// doctorAliasNames 是配过别名的规范名，去重后排序。
func doctorAliasNames(aliases []catalog.Alias) []string {
	seen := map[string]bool{}
	var names []string
	for _, a := range aliases {
		if seen[a.CanonicalName] {
			continue
		}
		seen[a.CanonicalName] = true
		names = append(names, a.CanonicalName)
	}
	slices.Sort(names)
	return names
}

// doctorAliasScopes 说明某个规范名的别名配在哪些范围上。
func doctorAliasScopes(aliases []catalog.Alias, canonical string) string {
	platform := 0
	channelIDs := map[int64]bool{}
	for _, a := range aliases {
		if a.CanonicalName != canonical {
			continue
		}
		if a.ChannelID == 0 {
			platform++
			continue
		}
		channelIDs[a.ChannelID] = true
	}

	var parts []string
	if platform > 0 {
		parts = append(parts, fmt.Sprintf("平台级 %d 条", platform))
	}
	if len(channelIDs) > 0 {
		ids := make([]int64, 0, len(channelIDs))
		for id := range channelIDs {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		labels := make([]string, 0, len(ids))
		for _, id := range ids {
			labels = append(labels, strconv.FormatInt(id, 10))
		}
		parts = append(parts, fmt.Sprintf("渠道级 %d 条（渠道 %s）", len(ids), strings.Join(labels, "、")))
	}
	if len(parts) == 0 {
		// 调用方只在别名表里出现过这个名字时才调这里，所以走到这一句说明
		// 判据与数据不一致；打一句实话，不编一个范围。
		return "别名表里查不到它"
	}
	return strings.Join(parts, "、")
}

// doctorChannelLabel 把渠道打成「3（my-upstream）」。
func doctorChannelLabel(id int64, name string) string {
	return fmt.Sprintf("%d（%s）", id, name)
}

// ── 第 6 项：钱包 ───────────────────────────────────────────────────────

func checkDoctorWallets(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "钱包", subject: doctorSubjectWallet}

	if len(in.users) == 0 {
		c.findings = append(c.findings,
			"还没有用户：没有任何一把 API Key 能挂上去，请求会回 401")
		return c
	}

	wallets, funded := 0, 0
	var missing []string
	for _, u := range in.users {
		if u.HasWallet {
			wallets++
		} else {
			missing = append(missing, fmt.Sprintf("用户 %d（%s）", u.ID, u.Name))
		}
		if u.Funded {
			funded++
		}
	}
	c.notes = append(c.notes, fmt.Sprintf("用户 %d 个，有钱包行的 %d 个，其中余额大于 0 的 %d 个",
		len(in.users), wallets, funded))

	for _, who := range missing {
		c.findings = append(c.findings,
			who+"没有钱包行：它第一次调用会在预留那一步失败（500）")
	}
	if wallets > 0 && funded == 0 {
		c.findings = append(c.findings, fmt.Sprintf(
			"%d 个钱包行的余额都是 0：调用会在预留那一步因余额不足失败（500）", wallets))
	}
	return c
}

// ── 第 7 项：真打一次上游 ───────────────────────────────────────────────

// doctorEndpointSpec 是一个入口的形状：路径、请求体、以及凭证放哪个头。
//
// 凭证放哪个头跟着协议走：OpenAI 的习惯是 Authorization: Bearer，Anthropic
// 的是 x-api-key。两个头服务端都认（见 internal/auth），但按各自协议的写法
// 发才对得上真实客户端的行为——这条链路要验的正是真实客户端走的那条。
type doctorEndpointSpec struct {
	name    string
	path    string
	payload func(model string) any
	setAuth func(h http.Header, key string)
}

func lookupDoctorEndpoint(name string) (doctorEndpointSpec, error) {
	switch name {
	case doctorEndpointChat:
		return doctorEndpointSpec{
			name: doctorEndpointChat,
			path: doctorPathChat,
			payload: func(model string) any {
				return doctorChatRequest{
					Model:     model,
					MaxTokens: doctorLiveMaxTokens,
					Messages:  []doctorChatMessage{{Role: "user", Content: doctorLivePrompt}},
				}
			},
			setAuth: func(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) },
		}, nil

	case doctorEndpointMessages:
		return doctorEndpointSpec{
			name: doctorEndpointMessages,
			path: doctorPathMessages,
			payload: func(model string) any {
				return doctorMessagesRequest{
					Model:     model,
					MaxTokens: doctorLiveMaxTokens,
					Messages:  []doctorChatMessage{{Role: "user", Content: doctorLivePrompt}},
				}
			},
			setAuth: func(h http.Header, key string) { h.Set("x-api-key", key) },
		}, nil
	}

	return doctorEndpointSpec{}, fmt.Errorf("--endpoint 只能是 %s 或 %s，当前为 %q",
		doctorEndpointChat, doctorEndpointMessages, name)
}

// doctorChatRequest 是 chat 入口的请求体（OpenAI 形状）。
//
// max_tokens 必填：OpenAI 协议里它是可选的，但 larkax 的网关按它算预留上界
// （见 pipeline.Request.validate 与 billing.ErrNoUpperBound），缺了会在请求
// 出网关之前就 400。
type doctorChatRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	Messages  []doctorChatMessage `json:"messages"`
}

// doctorMessagesRequest 是 messages 入口的请求体（Anthropic 形状）。
//
// max_tokens 与 chat 入口一样必填——**不是**因为 Anthropic 协议要求它，而是
// 因为 larkax 的网关按它算预留上界（见 pipeline.Request.validate 与
// billing.ErrNoUpperBound）：算不出上界就不放行，否则结算可能超过预留。
// 两个入口在这件事上没有差别。
type doctorMessagesRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	Messages  []doctorChatMessage `json:"messages"`
}

type doctorChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// doctorLiveResponse 是两个入口的响应体里 doctor 要看的那几栏。
//
// 两套协议的用量字段名不同（prompt/completion 与 input/output），所以两组
// 都收着，由 usageLabel 挑一组打得出来的。
type doctorLiveResponse struct {
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// usageLabel 打这次调用的用量，两种协议各认一套字段名。
func (r doctorLiveResponse) usageLabel() string {
	switch {
	case r.Usage.PromptTokens > 0 || r.Usage.CompletionTokens > 0:
		return fmt.Sprintf("prompt=%d completion=%d", r.Usage.PromptTokens, r.Usage.CompletionTokens)
	case r.Usage.InputTokens > 0 || r.Usage.OutputTokens > 0:
		return fmt.Sprintf("input=%d output=%d", r.Usage.InputTokens, r.Usage.OutputTokens)
	}
	return ""
}

// doctorLiveBody 是从响应体里读出来的东西。
//
// preview 只在响应体不是 JSON 时才有值：一个反代返回的错误页（502 的 HTML）
// 解析不出任何字段，而那一页正是要给人看的东西。
type doctorLiveBody struct {
	model   string
	usage   string
	errMsg  string
	preview string
}

func parseDoctorLiveBody(body []byte) doctorLiveBody {
	var parsed doctorLiveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return doctorLiveBody{preview: doctorPreview(body)}
	}
	out := doctorLiveBody{model: parsed.Model, usage: parsed.usageLabel()}
	switch {
	case parsed.Error.Message != "":
		out.errMsg = parsed.Error.Message
	case parsed.Error.Type != "":
		out.errMsg = parsed.Error.Type
	}
	return out
}

// doctorPreview 把一段非 JSON 的响应体折成一行短说明。
//
// 按**字符**而不是字节截断：按字节截会把一个汉字切一半，打出来是一个乱码
// 字符，而看的人会以为是编码问题。
func doctorPreview(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	runes := []rune(text)
	if len(runes) <= doctorPreviewRunes {
		return text
	}
	return string(runes[:doctorPreviewRunes]) + "…"
}

// doctorLive 是真打一次上游的结果。
type doctorLive struct {
	enabled  bool
	endpoint string
	url      string
	model    string

	status  int
	elapsed time.Duration

	respModel string
	usage     string
	errMsg    string
	preview   string
	sendErr   error
}

// ok 判断这一次真打上游算不算通过。没开 --live 时它恒为 false，调用方要
// 先看 enabled。
func (l doctorLive) ok() bool {
	return l.enabled && l.sendErr == nil && l.status >= 200 && l.status < 300
}

// runDoctorLive 往公共入口发一次非流式推理请求。
//
// 凭证只出现在请求头里：它不进 URL（URL 会跟着传输层错误一起被打出来）、
// 不进请求体、不进任何一行输出。
func runDoctorLive(ctx context.Context, opts doctorOptions) doctorLive {
	if !opts.live {
		return doctorLive{}
	}

	spec, err := lookupDoctorEndpoint(opts.endpoint)
	if err != nil {
		// 参数解析那一步已经拒过不合法的取值；走到这里说明两处的判据不一致，
		// 而那种情况下发出去一个不知道打到哪儿的请求更坏。
		return doctorLive{enabled: true, sendErr: err}
	}

	out := doctorLive{
		enabled:  true,
		endpoint: spec.name,
		url:      opts.baseURL + spec.path,
		model:    opts.model,
	}

	payload, err := json.Marshal(spec.payload(opts.model))
	if err != nil {
		out.sendErr = fmt.Errorf("拼请求体失败: %w", err)
		return out
	}

	reqCtx, cancel := context.WithTimeout(ctx, doctorLiveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, out.url, bytes.NewReader(payload))
	if err != nil {
		out.sendErr = fmt.Errorf("构造请求失败: %w", err)
		return out
	}
	req.Header.Set("Content-Type", "application/json")
	spec.setAuth(req.Header, opts.key)

	start := time.Now()
	resp, err := doctorHTTPClient.Do(req)
	out.elapsed = time.Since(start)
	if err != nil {
		out.sendErr = err
		return out
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, doctorMaxBodyBytes))
	if err != nil {
		out.sendErr = fmt.Errorf("读响应体失败: %w", err)
		return out
	}

	out.status = resp.StatusCode
	parsed := parseDoctorLiveBody(body)
	out.respModel, out.usage, out.errMsg, out.preview = parsed.model, parsed.usage, parsed.errMsg, parsed.preview
	return out
}

// ── 输出 ────────────────────────────────────────────────────────────────

func printDoctorReport(w io.Writer, rep doctorReport) {
	_, _ = fmt.Fprintln(w, "larkaxctl doctor：逐项检查「配好的链路能不能接一次请求」。")
	_, _ = fmt.Fprintln(w)

	checks := rep.checks()
	for i, c := range checks {
		printDoctorCheck(w, i+1, len(checks), c)
	}
	printDoctorLive(w, rep.live)

	passed, failed, skipped := rep.counts()
	_, _ = fmt.Fprintf(w, "汇总：通过 %d 项、不通过 %d 项、没跑成 %d 项\n", passed, failed, skipped)
	_, _ = fmt.Fprintln(w, "真打上游不在这六项里：它要 --live 才跑，结论在它自己那一节（失败时退出码同样非 0）。")
}

func printDoctorCheck(w io.Writer, index, total int, c doctorCheck) {
	_, _ = fmt.Fprintf(w, "[%d/%d] %s\n", index, total, c.title)
	_, _ = fmt.Fprintf(w, "  检查：%s\n", c.subject)
	for _, note := range c.notes {
		_, _ = fmt.Fprintf(w, "  %s\n", note)
	}
	printDoctorTable(w, c.table)
	for i, finding := range c.findings {
		if i == doctorMaxListed {
			_, _ = fmt.Fprintf(w, "  问题：……另有 %d 条同类问题没有列出\n", len(c.findings)-doctorMaxListed)
			break
		}
		_, _ = fmt.Fprintf(w, "  问题：%s\n", finding)
	}
	if c.skip != "" {
		_, _ = fmt.Fprintf(w, "  没跑成：%s\n", c.skip)
	}
	_, _ = fmt.Fprintf(w, "  结论：%s\n", c.status().label())
	_, _ = fmt.Fprintln(w)
}

// printDoctorTable 把一项检查的表打出来。
//
// 整体缩进两格：它挂在某一项下面，与那一项的说明文字平级会让人分不清哪几行
// 是表、哪几行是结论。缩进加在第一栏上，所以对齐仍然由 tableWriter 按显示
// 宽度算（见 table.go）。
func printDoctorTable(w io.Writer, t *doctorTable) {
	if t == nil || len(t.rows) == 0 {
		return
	}
	tw := newTableWriter(w)
	_, _ = fmt.Fprintf(tw, "    %s\n", strings.Join(t.header, "\t"))
	for _, row := range t.rows {
		_, _ = fmt.Fprintf(tw, "    %s\n", strings.Join(row, "\t"))
	}
	_ = tw.Flush()
}

func printDoctorLive(w io.Writer, l doctorLive) {
	_, _ = fmt.Fprintln(w, "[真打上游]")
	if !l.enabled {
		_, _ = fmt.Fprintln(w, "  没跑：要加 --live 才会跑，它会真的发一次请求到上游，会花钱、要凭证")
		_, _ = fmt.Fprintln(w)
		return
	}

	_, _ = fmt.Fprintf(w, "  入口：POST %s\n", l.url)
	_, _ = fmt.Fprintf(w, "  模型：%s\n", l.model)

	switch {
	case l.sendErr != nil:
		_, _ = fmt.Fprintf(w, "  结论：不通过——请求没发出去：%v\n", l.sendErr)

	case l.ok():
		_, _ = fmt.Fprintf(w, "  结果：%d，耗时 %s", l.status, l.elapsed.Round(time.Millisecond))
		if l.respModel != "" {
			_, _ = fmt.Fprintf(w, "，响应里的模型是 %s", l.respModel)
		}
		if l.usage != "" {
			_, _ = fmt.Fprintf(w, "，用量 %s", l.usage)
		}
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "  结论：通过")

	default:
		_, _ = fmt.Fprintf(w, "  结果：%d，耗时 %s\n", l.status, l.elapsed.Round(time.Millisecond))
		if l.errMsg != "" {
			_, _ = fmt.Fprintf(w, "  错误：%s\n", l.errMsg)
		}
		if l.preview != "" {
			_, _ = fmt.Fprintf(w, "  响应体：%s\n", l.preview)
		}
		_, _ = fmt.Fprintln(w, "  结论：不通过")
	}
	_, _ = fmt.Fprintln(w)
}
