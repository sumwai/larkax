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
	"net"
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
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/pricing"
)

// doctorHelp 是 doctor 的两版帮助文本（见 helpText）。
var doctorHelp = helpText{short: doctorShortUsage, long: doctorUsage}

// doctorShortUsage 是 doctor 的短版：`-h` / `--help` 与参数写错时打的那一份。
const doctorShortUsage = `larkaxctl doctor 逐项检查「配好的链路能不能接一次请求」。

用法：
  larkaxctl doctor
        只读检查九项，不碰网络
  larkaxctl doctor --live --model <规范名> [--endpoint chat|messages|responses]
                   (--credential-stdin | --credential-env <变量名>)
        额外真打一次上游
  larkaxctl doctor -v
        每项多打一段细节

九项只读；凭证不能写在命令行参数里，只能从标准输入或环境变量读。
「没跑成」与「通过」是两件事，前者也非零退出。

更细的边界：larkaxctl help doctor。
`

// doctorUsage 是 doctor 的长版：`larkaxctl help doctor` 与 `doctor help`。
const doctorUsage = `larkaxctl doctor 逐项检查「配好的链路能不能接一次请求」。

用法：
  larkaxctl doctor
        只读检查九项，不碰网络

  larkaxctl doctor --live --model <规范名> [--endpoint chat|messages|responses]
                   [--base-url <URL>] (--credential-stdin | --credential-env <变量名>)
        额外真打一次上游

  larkaxctl doctor -v
        每项多打一段细节：检查范围、读到的数、表，以及每条问题的完整原文

  larkaxctl doctor --format json
        打一份机器可读的报告，给探针与管道下游的 jq 用（见 docs/cli-output.md）。
        它不走 -v：所有细节本来就在里面，问题清单也不截断。

输出默认每项一行：状态符、标题、一句结论。状态符 ✓ 通过、✗ 不通过、
! 没跑成、- 没跑（这一项不在本次范围内，例如没开 --live）。最后一行是
汇总。**细节一条都没删，只是默认不打**：加 -v 就跟着各自那一行缩进打出来。

它按顺序回答九个问题，每一项都给出「检查了什么、结论是什么」：

  1. 数据库       连得上吗、迁移跑到哪一版
  2. 渠道         有没有至少一条 enabled 且 models 非空的渠道
  3. 模型目录     渠道 models 里出现的每个名字都在 model_catalog 里吗、且目录声明
                  的端点含这条渠道打的那条端点吗
  4. 别名         配过别名的规范名有没有渠道接得住；可服务的规范名在每个服务它的
                  渠道上能不能解析出上游名
  5. 计价规则     可服务的规范名在每个服务它的渠道上都有生效的规则吗
  6. 规则模型名   计价规则与成本规则的 --models 里写到的名字都在目录里吗
  7. 钱包         有用户吗、每个用户都有钱包行吗、有没有一个钱包余额大于 0
  8. 入站别名遮蔽 别名与目录里的规范名同名吗（同名时解析走规范名、这条别名不生效）
  9. 入站别名悬空 入站别名指向的规范名都还在目录里吗

第 3、4、5 项回答的是同一个字符串在两个数组里对不上的问题：渠道的 --models
与目录里的规范名差一个字符，或者模型建了却没加进任何渠道的白名单。第 3 项还
多判一层名字与端点的对应。第 6 项问的是同一个问题的另一半：名字写进的是规则
而不是渠道，写错时那条规则永远命中不了，而它在任何清单里都像是配好的。第 7 项
把「用户没有钱包行」这类缺口在第一次调用之前指出来。第 8、9 项看的是入站别名
（model alias）与目录的对应：同名时别名永远不生效，指向的名字不在目录里时用它
的请求会在选路处失败。

**前九项只读**：不写任何一行、不建任何东西、不发网络请求。第十项要花钱、
要凭证，默认不跑。

--live 真打一次上游：往公共入口发一次非流式推理请求，把状态码、耗时与用量
打出来。**凭证不能写在命令行参数里**（参数会出现在 ps 的输出与 shell 历史里），
只能从标准输入或环境变量读，与 channel create 同一个做法：

  --credential-stdin        从标准输入读到文件结束（末尾的换行会被去掉）
  --credential-env <变量名> 从名为 <变量名> 的环境变量读

这里读的是打网关用的 **API Key**（apikey create 发的那把），不是渠道凭证。

--model 是这次请求要用的**规范模型名**（第 3 项建的那个）。--endpoint 选打哪个
入口：chat 是 POST /v1/chat/completions（Authorization: Bearer），messages 是
POST /v1/messages（x-api-key），responses 是 POST /v1/responses
（Authorization: Bearer）。--base-url 不给时按本次配置里的 LARKAX_HTTP_ADDR 拼
网关地址：那个变量写着 127.0.0.1:8081 就打成 http://127.0.0.1:8081；写成
「:8080」这类不写主机的「所有网卡」时按 127.0.0.1 打。网关不在本机、或者要打的
是另一台，就显式给。

退出码：0 表示九项都通过（--live 给了的话那一次也成功）、1 表示有问题或某一项
没跑成、3 表示连不上数据库。**「没跑成」与「通过」是两件事**：没有可检查的
对象时那一项报「没跑成」而不是「通过」，退出码也非 0——两者在安静的终端里
长得一样，脚本按退出码分派时要留意。

例：

  larkaxctl doctor
  larkaxctl doctor --live --model claude-x --credential-env LARKAX_API_KEY

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -m <规范名>        同 --model
  --env <变量名>     同 --credential-env。它与全局的 --env-file 只差一个词，含义
                     却不同：**这里是环境变量的名字**（从那个变量里读凭证），
                     --env-file 要的是 .env 文件的**路径**。
  --stdin            同 --credential-stdin

-v / --verbose 与 -q / --quiet 是全局选项（见 larkaxctl --help 的「全局选项」），
写在子命令前后都认。两个不能一起给。

更细的约定见 docs/cli-reference.md 的「doctor」一节。
`

// doctorTimeout 是九项只读检查的时间上限。
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

// doctorLiveInstructions 是 responses 入口顶层的 instructions。
//
// Responses 协议把 system 放在顶层的 instructions 里，而不是 messages 里的
// 一条。这里放一句最短的说明：这条命令要的是「上游收下了、回了一个正常响应」，
// 不是一段好回答。
const doctorLiveInstructions = "Reply briefly."

// doctorLiveMaxTokens 是真打上游时这次请求的 max_tokens，三个入口共用。
//
// 网关按它算预留上界（见 pipeline.Request.validate 与 billing.ErrNoUpperBound），
// 所以三个入口都必须带；给得小是为了让这次调用的花费有个上界。
const doctorLiveMaxTokens = 16

// 真打上游的三个入口。
//
// 路径与 internal/api 的 PathChatCompletions / PathMessages / PathResponses
// 是同一个值，doctor_test.go 里有用例钉住这一点。本包不 import internal/api：
// 那会把整个入口层（含 gateway）拖进运维命令的二进制里，而这里要的只是几个
// 字符串。
const (
	doctorEndpointChat      = "chat"
	doctorEndpointMessages  = "messages"
	doctorEndpointResponses = "responses"

	doctorPathChat      = "/v1/chat/completions"
	doctorPathMessages  = "/v1/messages"
	doctorPathResponses = "/v1/responses"
)

// doctorLoopbackHost 是监听地址写成「所有网卡」时，替它去连的那一个主机。
//
// 监听地址有三种写法：`127.0.0.1:8080`（指定主机）、`:8080`（不写主机，示例里
// 用的是这一种）、以及把主机写成通配地址的——`0.0.0.0`、`::`，IPv6 还有长写法。
// 后两种都不是能连的地址，而 doctor 与网关通常跑在同一台机器上，环回是这里面
// 最短的那条路。取 127.0.0.1 而不是 localhost：后者要先解析一次名字，还可能
// 落到 ::1 上，而服务未必听在那一侧。
const doctorLoopbackHost = "127.0.0.1"

// doctorBaseURLFromListenAddr 把主服务的监听地址拼成一个能连的网关地址。
//
// 监听地址本身合不合法由服务启动那一步负责——它监不起来就起不来。这里只管
// 「能监听、却不能直接拿去请求」的那几种写法，见上面那个常量。
func doctorBaseURLFromListenAddr(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err == nil && port == "" {
		err = errors.New("没有端口")
	}
	if err != nil {
		return "", fmt.Errorf("从 LARKAX_HTTP_ADDR（%q）读不出主机与端口，"+
			"给不出 --base-url 的默认值（%v）；显式给一个 --base-url 就能接着跑",
			listenAddr, err)
	}
	// 空主机与通配地址（`0.0.0.0`、`::`，以及 IPv6 的长写法）都不是能连的地址。
	// 判通配用 ParseIP().IsUnspecified 而不是列几个字面量：通配地址有好几种
	// 写法，列字面量时漏掉的那一种会拼出一个看着像地址、判据却没认出来的结果。
	// 解析不出 IP 的是主机名（`gateway.example.com`），原样留着。
	if host == "" {
		host = doctorLoopbackHost
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = doctorLoopbackHost
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// resolveDoctorBaseURL 定下这次要打的网关地址。
//
// 显式给了 --base-url 就以它为准（parseDoctorOptions 已经校验过、去过尾斜杠）。
// 没给时按**这一次跑的时候解析出来的配置**拼：主服务听在哪个地址上，doctor 就
// 往哪里打。默认值写死一个端口时，在 .env 里改过 LARKAX_HTTP_ADDR 的人拿到的
// 是一次「连不上」，而那句话读起来像服务没起来——这条命令本来是用来回答
// 「服务能不能用」的，不该反过来让人先猜地址。
//
// 调用点在连库之前：地址不对与库连不上是两件事，而前者先说更省事。
func resolveDoctorBaseURL(opts *doctorOptions, ops *config.OpsConfig) error {
	if !opts.live || opts.baseURLGiven {
		return nil
	}
	base, err := doctorBaseURLFromListenAddr(ops.HTTPAddr)
	if err != nil {
		return err
	}
	opts.baseURL = base
	return nil
}

// 判「这次失败是谁拒的」用的那几个值。
//
// 值与 internal/api 的 HeaderVersion / HeaderRetryAfter / CodeInsufficientQuota
// 以及下面两句 message（那个包里的 MsgInsufficientQuota / MsgInsufficientBalance）
// 是同一份。这五个值全部由 doctor_test.go 的 TestDoctorLiveSignalsMatchAPI
// 逐字钉住——照抄一份而不是直接引用，是因为本包不 import internal/api：那会把
// 整个入口层（含 gateway）拖进运维命令的二进制里，而这里要的只是几个字符串
// ——与上面三条路径同一个理由。
//
// 只看状态码不够的地方有两处：
//
//   - 429 有**两个来源**：我们自己的按 Key 限流（writeRateLimited，带
//     Retry-After）、额度用尽（带 insufficient_quota 码），以及上游的限流
//     （网关原样透出 429，两个都没有）。状态码一样，处置相反：前两个是
//     我们自己这一侧的问题，后一个该等上游缓过来。
//   - 响应根本可能不是我们的网关给的（--base-url 指到了别的服务、或者链路
//     上有个反代自己回了 5xx）。那时按状态码归因会把一次「地址配错了」
//     说成上游的问题。X-Larkax-Version 是每个公共响应都带的头
//     （见 internal/api.Handler.ServeHTTP），没有它就不是我们回的。
//
// insufficient_quota 还要再分一层：它**有两个来源**，共用同一个状态码与码
// （刻意的，见 internal/api.CodeInsufficientQuota 的注释）——按 Key 的累计
// 额度用尽，或者用户钱包的可用余额不够这次预留。码只回答「被拒了」，回答
// 「谁拒的」的是 message（见 internal/api 的 writeInsufficientQuota /
// writeInsufficientBalance）。两个来源要做的下一步相反（改 key 的额度 /
// 充值），所以归因必须先认 message、再拿码兜底（见 attribution）。
const (
	doctorHeaderVersion         = "X-Larkax-Version"
	doctorHeaderRetryAfter      = "Retry-After"
	doctorCodeInsufficientQuota = "insufficient_quota"

	// 与 internal/api 的 MsgInsufficientQuota / MsgInsufficientBalance 逐字
	// 相同（那边已导出），由 TestDoctorLiveSignalsMatchAPI 钉住。两边任一
	// 改了词，doctor 会认不出来（落到 attribution 那条保守的兜底），而那条
	// 用例会红——不钉的话这里会**悄悄**退化成「账户状态类拒绝」，正是 #472
	// 要修的那种指错人以静默的方式复活。
	doctorMsgInsufficientQuota = "this API key has reached its spending limit; " +
		"raise the limit on this key or use a different key"
	doctorMsgInsufficientBalance = "the account balance is insufficient for this request; " +
		"top up the wallet before retrying"
)

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
	doctorSubjectDatabase        = "连得上、迁移跑到哪一版"
	doctorSubjectChannel         = "有没有至少一条 enabled 且 models 非空的渠道"
	doctorSubjectCatalog         = "enabled 渠道的 models 里出现的每个名字都在 model_catalog 里、且目录声明的端点含这条渠道打的那条端点吗"
	doctorSubjectAlias           = "配过别名的规范名有没有渠道接得住；可服务的规范名在每个服务它的渠道上能不能解析出上游名"
	doctorSubjectPricing         = "可服务的规范名在每个服务它的渠道上都有生效的计价规则吗"
	doctorSubjectRuleModels      = "计价规则与成本规则的 --models 里写到的名字都在目录里吗"
	doctorSubjectWallet          = "有用户吗、每个用户都有钱包行吗、有没有一个钱包余额大于 0"
	doctorSubjectInboundShadowed = "入站别名有没有与目录里的规范名同名（同名时解析走规范名、别名不生效）"
	doctorSubjectInboundDangling = "入站别名指向的规范名都还在模型目录里吗"
)

// ── 命令入口 ────────────────────────────────────────────────────────────

// doctorOptions 是 doctor 的参数。
//
// --verbose 不在里面：它是**全局**控制选项（见 globals.go），由最外层的 run()
// 摘出来，写在子命令前面也一样认。这里再注册一次会得到第二个格子，而那只在
// 「从 run() 进来」这条路上被摘掉、从别处调过来时才有用——两个格子迟早对不上。
//
// key 只在 --live 时有值。它不进日志、不进错误信息、不进任何一行输出：
// 全仓库唯一读它的地方是发请求时设置请求头。
type doctorOptions struct {
	live     bool
	model    string
	endpoint string
	baseURL  string
	// baseURLGiven 是「--base-url 显式给过」。没给时 baseURL 先留空，等
	// runDoctor 读到配置再补上（见 resolveDoctorBaseURL）：默认值来自本次
	// 解析出来的监听地址，而那份配置要到参数校验之后才读得到。
	baseURLGiven bool
	key          string
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
	if err := resolveDoctorBaseURL(&opts, cfg); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	// 连不上库时**不直接返回**：报告里「第 1 项没跑成、其余八项也没跑成」
	// 比一句「连接数据库失败」有用得多，而那是这个命令最要紧的一次输出。
	db, dbErr := openDB(ctx, cfg)

	var rep doctorReport
	if dbErr != nil {
		rep = doctorReportWithoutDB(ctx, dbErr, opts)
	} else {
		defer func() { _ = db.Close() }()
		rep = collectDoctorReport(ctx, db, opts)
	}

	// 退出码先算出来，两条渲染路径共用它：`-o json` 下 doctor 有失败项时
	// 仍然退出 1（与交互模式一致），而 JSON 里的 exit_code 也是同一个数——
	// 各算一遍的话，两处迟早分叉，分叉的表现是「报告说 failed、
	// 脚本读到的退出码是 0」。
	return emitDoctorReport(os.Stdout, rep, dbErr, verboseOutput)
}

// emitDoctorReport 把一份报告按选定的格式打出去，并把该用的退出码返回。
//
// 单独抽出来的理由与 printStatusLines 相同，不是复用，是**让用例打到同一条
// 路径上**：真跑一次 doctor 要连库，而「有失败项时 json 模式仍退出 1」
// 恰恰是最该被钉住的一条，不能因为造不出一个有失败项的库就不测。
//
// --quiet 只落在表格那一档上（humanOut）：机器可读那一份照打，理由见
// globals.go 的 humanStdout。
func emitDoctorReport(w io.Writer, rep doctorReport, dbErr error, verbose bool) error {
	cause := rep.exitErr(dbErr)
	if jsonOutput() {
		// verbose 在这里没有意义：JSON 那份本来就把 -v 才打的细节全带上了。
		return emitJSON(w, doctorDoc(rep), cause)
	}

	printDoctorReport(humanOut(w), rep, verbose)
	return cause
}

// parseDoctorOptions 解析并校验参数。
//
// 全部检查发生在连库之前（与其余子命令同一条约定）：一个写错的命令不该先
// 连上生产库再报错。
func parseDoctorOptions(args []string) (doctorOptions, error) {
	fs := newFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, doctorShortUsage) }
	live := fs.Bool("live", false, "")
	model := fs.String("model", "", "")
	stringAlias(fs, model, "m")
	endpoint := fs.String("endpoint", doctorEndpointChat, "")
	// 默认值留空：它跟着本次解析出来的监听地址走，而那份配置要等参数校验完
	// 才读得到（见 resolveDoctorBaseURL）。--live 没给时这一项本来也用不上。
	baseURL := fs.String("base-url", "", "")
	credentialStdin := fs.Bool("credential-stdin", false, "")
	boolAlias(fs, credentialStdin, "stdin")
	credentialEnv := fs.String("credential-env", "", "")
	stringAlias(fs, credentialEnv, "env")
	if err := fs.Parse(args); err != nil {
		return doctorOptions{}, err
	}
	if fs.NArg() > 0 {
		return doctorOptions{}, fmt.Errorf("doctor 不接受位置参数，得到 %q", fs.Arg(0))
	}

	opts := doctorOptions{live: *live, endpoint: *endpoint}

	// 哪几个标志是显式给的。下面两处都要问这件事：--live 才认的那几个，
	// 以及 --base-url 要不要按配置补默认值。
	//
	// 判据要连别名一起看（见 flagGiven）：写 `-m x` 或 `--env FOO` 时
	// Visit 记的是 "m" / "env"，只查规范名的表现是这几条**静默放行**，
	// 而那恰好是下面那一段要拦的东西。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if !*live {
		// 只有 --live 才用得上这些标志。静默忽略它们会让「我给了 --model
		// 但它没生效」看起来像一次成功的检查——而那正是这一项要防的东西。
		for _, group := range [][]string{
			modelFlagNames,
			{"endpoint"},
			{"base-url"},
			credentialStdinFlagNames,
			credentialEnvFlagNames,
		} {
			if flagGiven(given, group...) {
				return doctorOptions{}, fmt.Errorf(
					"--%s 只在 --live 时才有意义：默认那九项不碰网络", group[0])
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

	if flagGiven(given, "base-url") {
		if baseErr := validateDoctorBaseURL(*baseURL); baseErr != nil {
			return doctorOptions{}, baseErr
		}
		opts.baseURL, opts.baseURLGiven = strings.TrimRight(*baseURL, "/"), true
	}

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

// jsonLabel 是这一档在机器可读输出里的取值。
//
// 三档各自一个词，与顶层 status 的 ok / failed / skipped 是**两套**词：
// 顶层说的是整份报告，这里是单项。单项的「通过」是 passed，用 ok 会让
// 「这一项过了」与「整份报告没问题」在文本上长得一样。
func (s doctorStatus) jsonLabel() string {
	switch s {
	case doctorPassed:
		return "passed"
	case doctorFailed:
		return "failed"
	default:
		return "skipped"
	}
}

// doctorCheck 是一项检查的结果。
//
// findings 与 skip 是两种不同的坏消息，分开存是为了让输出能把它们说清楚：
// findings 是「查了，有问题」，skip 是「没查成」。两者都让退出码非 0。
//
// ok 是「通过时那一句」，由各项自己填：结论里那个数（迁移版本、几个可用渠道、
// 几组能解析出上游名）只有查的人知道，从 notes 里反推是猜。
type doctorCheck struct {
	title    string
	subject  string
	ok       string
	notes    []string
	findings []doctorProblem
	skip     string
	table    *doctorTable
}

// doctorProblem 是一项检查里的一条问题。
//
// 拆成两截是为了让默认输出只有一行：
//
//   - headline 说清「哪个对象、坏在哪里」，进默认那一行的结论；
//   - detail 是后果与排查方向（「多半是拼写差了一个字符」这一类），只在 -v
//     时打出来。
//
// 两截都不许删。默认那一眼要的是对象与根因；一次没查对时，人仍然需要下面
// 那句「为什么会这样、接下来该怎么办」。
type doctorProblem struct {
	headline string
	detail   string
}

// String 把两截拼回一整句（headline：detail）。
//
// -v 的输出与「只关心说了什么」的地方（用例）都走这里：拆开存是为了输出，
// 不是为了让人去记「哪半句在哪一栏」。
//
// headline 自己已经是一句完整的话时（以句号结尾，例如清单空状态那一句）用空格
// 接，不再补一个冒号：`暂无用户。可使用 …添加。：没有任何一把 API Key …`
// 里的那个冒号落在句号后面，是两套标点的叠用，读起来像打错了。
func (p doctorProblem) String() string {
	if p.detail == "" {
		return p.headline
	}
	if strings.HasSuffix(p.headline, "。") {
		return p.headline + " " + p.detail
	}
	return p.headline + "：" + p.detail
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

// glyph 把结论折成默认输出里的那个状态符。
func (c doctorCheck) glyph() glyphKind {
	switch c.status() {
	case doctorFailed:
		return glyphFail
	case doctorSkipped:
		return glyphSkip
	default:
		return glyphPass
	}
}

// conclusion 是默认输出里跟在标题后面的那一句。
//
// 四档各说各的：
//
//   - 通过：这一项查到了什么（ok 里那句）；
//   - 不通过：**第一条**问题（headline），另有几条折成一句指向 -v——一条问题
//     一行会让一次坏掉的运行刷屏，而一屏里最先要看的是「坏了哪几处」；
//   - 没跑成：skip 里那句原文。句首不再加「跳过（…）」：状态符已经是 `!`，
//     再包一层只会把括号套起来（「跳过（连不上数据库（同第 1 项））」）。
func (c doctorCheck) conclusion() string {
	switch c.status() {
	case doctorFailed:
		line := c.findings[0].headline
		if extra := len(c.findings) - 1; extra > 0 {
			line += fmt.Sprintf("（另有 %d 条，用 -v 看）", extra)
		}
		return line
	case doctorSkipped:
		return c.skip
	default:
		if c.ok != "" {
			return c.ok
		}
		return "通过"
	}
}

// detailLines 是 -v 时缩进打出来的那几行。
//
// 这是原先默认输出里的那一整段：检查范围、读到的数、表、每一条问题的完整
// 原文、没跑成的原因。默认只留一行之后它一条都没丢，只是挪到了要的时候才看。
func (c doctorCheck) detailLines() []string {
	var lines []string
	lines = append(lines, "检查："+c.subject)
	lines = append(lines, c.notes...)
	lines = append(lines, c.table.lines()...)
	for i, problem := range c.findings {
		if i == doctorMaxListed {
			lines = append(lines, fmt.Sprintf("问题：……另有 %d 条同类问题没有列出",
				len(c.findings)-doctorMaxListed))
			break
		}
		lines = append(lines, "问题："+problem.String())
	}
	if c.skip != "" {
		lines = append(lines, "没跑成："+c.skip)
	}
	// 结论在这一行末尾再打一遍：细节展开时它可能与那一行的标题隔了十几行，
	// 而在 -v 里读的人要的正是「这一堆说明之后到底算过没过」。
	return append(lines, "结论："+c.status().label())
}

// lines 把这张表渲染成若干行，已经按显示宽度对齐（见 table.go）。
//
// 返回切片而不是直接写出去：调用方要把它们缩进着挂到某一项下面，而缩进加在
// 每一行上，对齐仍然由 tableWriter 自己算。
func (t *doctorTable) lines() []string {
	if t == nil || len(t.rows) == 0 {
		return nil
	}
	var sb strings.Builder
	tw := newTableWriter(&sb)
	_, _ = fmt.Fprintln(tw, strings.Join(t.header, "\t"))
	for _, row := range t.rows {
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
	return strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
}

// doctorReport 是九项只读检查的结果，加上可选的那一次真打上游。
type doctorReport struct {
	database   doctorCheck
	channel    doctorCheck
	catalog    doctorCheck
	alias      doctorCheck
	pricing    doctorCheck
	ruleModels doctorCheck
	wallet     doctorCheck
	// 第 8、9 项：入站别名两条检查。追加在末尾而不是插在别名（第 4 项）
	// 旁边，是为了让既有报告里的 1–7 项序号与文案都不动。
	inboundShadowed doctorCheck
	inboundDangling doctorCheck
	live            doctorLive
}

// doctorNamedCheck 是一项检查与它**机器可读的名字**。
//
// 名字是英文、稳定、与 doctorReport 的字段同名：人在终端上看到的是「模型目录」，
// 而脚本要按一个不会被翻译、也不会随排版改动的标识去认它。两套名字放在同一条
// 记录里，是因为它们的**顺序**必须一致——各写一份列表，加一项时只改一处，
// 另一处就会张冠李戴，而那种错在输出上完全看不出来。
type doctorNamedCheck struct {
	name  string
	check doctorCheck
}

// namedChecks 按输出顺序给出九项检查，连同它们的机器可读名字。
func (r doctorReport) namedChecks() []doctorNamedCheck {
	return []doctorNamedCheck{
		{"database", r.database},
		{"channel", r.channel},
		{"catalog", r.catalog},
		{"alias", r.alias},
		{"pricing", r.pricing},
		{"rulemodels", r.ruleModels},
		{"wallet", r.wallet},
		{"inboundshadowed", r.inboundShadowed},
		{"inbounddangling", r.inboundDangling},
	}
}

// checks 按输出顺序给出九项检查。
//
// 从 namedChecks 推出来而不是另列一份：顺序只有一处来源（见 doctorNamedCheck）。
func (r doctorReport) checks() []doctorCheck {
	named := r.namedChecks()
	out := make([]doctorCheck, 0, len(named))
	for _, n := range named {
		out = append(out, n.check)
	}
	return out
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
		// 结论已经在上面那份报告里（每一项一行 `✗` / `!`），这里只给退出码：
		// reportedErr 让 main 不再补打一行 `larkaxctl: …`——那是同一句话说
		// 第二遍，形状还像一次崩溃。
		return reportedErr(fmt.Errorf("doctor 不通过：%d 项有问题、%d 项没跑成", failed, skipped))
	}
	if r.live.enabled && !r.live.ok() {
		return reportedErr(errors.New("doctor 不通过：真打上游那一次没有成功"))
	}
	return nil
}

// ── 收集 ────────────────────────────────────────────────────────────────

// doctorReportWithoutDB 是连不上库时的报告：九项全部「没跑成」。
//
// 真打上游仍然照跑：它走 HTTP，不读库。在一台连不上库的跳板机上，「网关是
// 活的、只是我这里看不到库」与「整条链路都挂了」是两件事，而这个区别只有
// 把那一项也跑一遍才看得出来。
func doctorReportWithoutDB(ctx context.Context, dbErr error, opts doctorOptions) doctorReport {
	// 只有第 1 项带完整的错误原文，后面八项指向它。原样重复九遍的后果是
	// 一份五十行的报告里九行完全相同，而那句错误本身有两行长——读者要在
	// 噪声里找出「到底哪一项跟别的不一样」，而这次一项都不一样。
	// 完整原文还出现在最后那行 `larkaxctl: ...` 里，不会丢。
	full := fmt.Sprintf("连不上数据库：%v", dbErr)
	same := "连不上数据库（同第 1 项）：这一项也要读库"
	blocked := func(title, subject, reason string) doctorCheck {
		return doctorCheck{title: title, subject: subject, skip: reason}
	}
	return doctorReport{
		database:        blocked("数据库", doctorSubjectDatabase, full),
		channel:         blocked("渠道", doctorSubjectChannel, same),
		catalog:         blocked("模型目录", doctorSubjectCatalog, same),
		alias:           blocked("别名", doctorSubjectAlias, same),
		pricing:         blocked("计价规则", doctorSubjectPricing, same),
		ruleModels:      blocked("规则模型名", doctorSubjectRuleModels, same),
		wallet:          blocked("钱包", doctorSubjectWallet, same),
		inboundShadowed: blocked("入站别名遮蔽", doctorSubjectInboundShadowed, same),
		inboundDangling: blocked("入站别名悬空", doctorSubjectInboundDangling, same),
		live:            runDoctorLive(ctx, opts),
	}
}

// collectDoctorReport 跑九项只读检查与可选的那一次真打上游。
func collectDoctorReport(ctx context.Context, db *sql.DB, opts doctorOptions) doctorReport {
	rep := doctorReport{database: checkDoctorDatabase(ctx, db)}

	in, err := gatherDoctorChain(ctx, db)
	if err != nil {
		// 一次读失败就不必再分项报错了：后面八项读的是同一批表，它们会
		// 一起失败，而四条一模一样的错误只会把原因埋掉。
		reason := fmt.Sprintf("读库失败：%v", err)
		blocked := func(title, subject string) doctorCheck {
			return doctorCheck{title: title, subject: subject, skip: reason}
		}
		rep.channel = blocked("渠道", doctorSubjectChannel)
		rep.catalog = blocked("模型目录", doctorSubjectCatalog)
		rep.alias = blocked("别名", doctorSubjectAlias)
		rep.pricing = blocked("计价规则", doctorSubjectPricing)
		rep.ruleModels = blocked("规则模型名", doctorSubjectRuleModels)
		rep.wallet = blocked("钱包", doctorSubjectWallet)
		rep.inboundShadowed = blocked("入站别名遮蔽", doctorSubjectInboundShadowed)
		rep.inboundDangling = blocked("入站别名悬空", doctorSubjectInboundDangling)
		rep.live = runDoctorLive(ctx, opts)
		return rep
	}

	rep.channel = checkDoctorChannels(in)
	rep.catalog = checkDoctorCatalog(in)
	rep.alias, rep.pricing = checkDoctorAliasAndPricing(ctx, db, in)
	rep.ruleModels = checkDoctorRuleModels(ctx, db, in)
	rep.wallet = checkDoctorWallets(in)
	rep.inboundShadowed = checkDoctorInboundShadowed(in)
	rep.inboundDangling = checkDoctorInboundDangling(in)
	rep.live = runDoctorLive(ctx, opts)
	return rep
}

// doctorChainInput 是第 2–9 项共用的一份读出来的数据。
//
// 一次读齐再逐项判定，而不是每项各查一遍：那样同一批表会被读四次，而
// 「两次读之间库变了」会让同一份报告里的两行互相矛盾。
type doctorChainInput struct {
	channels     []channels.ChannelInfo
	catalogNames []string
	// catalogEndpoints 是「规范名 → 目录声明的端点」，键的**存在**表示这个名字
	// 在目录里（值可能是 nil，那是「在目录里、但端点那一项没配过」）。这一栏让
	// 第 3 项既能判「名字在不在目录里」，又能按 channels 那份判据判端点（见
	// checkDoctorCatalog）。
	catalogEndpoints map[string][]channel.Endpoint
	aliases          []catalog.Alias
	// inboundAliases 是入站别名（调用方写的名字 → 规范名）。第 8、9 项读它，
	// 而不是各自再查一遍：与上面几栏同一个理由——同一份报告里的两项要看到
	// 同一批行。
	inboundAliases []catalog.InboundAlias
	users          []doctorUser
}

// inCatalog 报告这个规范名在不在模型目录里。
//
// 按键的存在判，不按值：值可能是 nil（在目录里、但端点那一项没配过），
// 那是「这一个名字已知」而不是「不在目录里」。
func (in doctorChainInput) inCatalog(name string) bool {
	_, ok := in.catalogEndpoints[name]
	return ok
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

// gatherDoctorChain 读齐第 2–9 项要的全部数据。
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
	in.catalogEndpoints = make(map[string][]channel.Endpoint, len(models))
	for _, m := range models {
		in.catalogEndpoints[m.CanonicalName] = m.Capabilities.Endpoints
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

	inbound, err := cat.InboundAliases(ctx)
	if err != nil {
		return in, err
	}
	in.inboundAliases = inbound

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
		c.findings = append(c.findings, doctorProblem{
			headline: "迁移一条都没跑",
			detail:   "先跑 larkaxctl migrate up，否则其余八项查不到任何东西",
		})
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
		// 版本读不出来，但连接是好的：这一项算通过（连得上），只是没法回答
		// 「跑到哪一版了」。结论里如实说这一半，而不是一句笼统的「通过」。
		c.ok = "连接正常，但读不出二进制里的迁移清单"
		return c
	}
	switch {
	case version < latest:
		c.findings = append(c.findings, doctorProblem{
			headline: fmt.Sprintf("迁移没跑完（库里是 %d，二进制里已经有 %d）", version, latest),
			detail:   "跑 larkaxctl migrate up",
		})
	case version > latest:
		c.findings = append(c.findings, doctorProblem{
			headline: fmt.Sprintf("库里的迁移版本 %d 比这个二进制里的 %d 还新", version, latest),
			detail:   "部署的二进制比库旧，先确认版本",
		})
	default:
		c.ok = fmt.Sprintf("连接正常（迁移 v%d）", version)
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
			c.findings = append(c.findings, doctorProblem{
				headline: fmt.Sprintf("渠道 %d（%s）是 enabled，但 models 是空的", ch.ID, ch.Name),
				detail: "读取路径把 models 当白名单用，" +
					"这条渠道永远选不中",
			})
			continue
		}
		usable++
	}
	c.notes = append(c.notes, fmt.Sprintf("enabled 的 %d 条，其中 models 非空的 %d 条", enabled, usable))

	if enabled == 0 {
		c.findings = append(c.findings, doctorProblem{
			headline: "没有任何 enabled 的渠道",
			detail:   "请求会回 503「no available upstream for the requested model」",
		})
	}
	c.ok = fmt.Sprintf("enabled 且 models 非空的渠道 %d 条", usable)
	return c
}

// ── 第 3 项：模型目录 ───────────────────────────────────────────────────

// checkDoctorCatalog 判「enabled 渠道的 models 与模型目录对得上吗」。
//
// 两问：名字在不在目录里（第一段），以及——名字在目录里的那些——它声明的端点
// 含不含这条渠道打的那条端点（第二段，见 doctorEndpointFindings）。
//
// 第二问的判据是 channels.EndpointFitForProtocols，与写入侧建渠道时用的是**同一份**
// （见 internal/channels/endpoints.go）：doctor 是排查入口，它说的与建渠道时
// 说的必须是同一件事，两处各写一份会漂移。
func checkDoctorCatalog(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "模型目录", subject: doctorSubjectCatalog}

	referenced := referencedDoctorModels(in)
	if len(referenced) == 0 {
		// 结论只留「没有可查的名字」，原因进 notes（-v 才打）：默认那一行要
		// 回答的是「这一项为什么没结论」，展开讲是哪一项导致的不该占那一行。
		c.skip = "没有可查的名字"
		c.notes = append(c.notes,
			"一个名字都取不到：没有 enabled 且 models 非空的渠道（见上面「渠道」那一项）")
		return c
	}
	c.notes = append(c.notes,
		fmt.Sprintf("范围：%d 个名字（enabled 渠道的 models 去重后）", len(referenced)))

	for _, name := range referenced {
		if in.inCatalog(name) {
			continue
		}
		c.findings = append(c.findings, doctorProblem{
			headline: fmt.Sprintf("%s 的 models 里有 %q，但目录里没有这个名字",
				doctorChannelsListing(in.channels, name), name),
			detail: "这次调用会回 503「no available upstream for the requested model」。" +
				"多半是拼写差了一个字符，或者那条模型已经被删掉了",
		})
	}

	// 名字在目录里的那些再判端点。它按「渠道 × 模型」判而不是按名字判：同一个
	// 模型挂在不同渠道上结论可能不同（渠道声明的协议决定它打哪条端点）。
	c.findings = append(c.findings, doctorEndpointFindings(in)...)

	c.ok = fmt.Sprintf("%d 个名字都在目录里，端点没有对不上的", len(referenced))
	return c
}

// doctorEndpointFindings 报出「这条渠道打的那条端点服务不了这个模型」的组合。
//
// 判据是 channels 那一份（EndpointFitForProtocols / Mismatch），与写入侧同源。
//
// 单位是「渠道 × 模型」而不是名字：同一个模型挂在不同 type 的渠道上，结论可能
// 不同。只报**明确**对不上的——目录里没有这个模型、那一项没配过、或渠道类型
// 算不出端点，都是「不知道」，与写入侧一样放行；名字不在目录里的那些由上面
// 一段报出，这里跳过（catalogEndpoints 里没有它们的键）。
func doctorEndpointFindings(in doctorChainInput) []doctorProblem {
	var findings []doctorProblem
	for _, ch := range in.channels {
		if ch.Status != channels.StatusEnabled {
			continue
		}
		// 同一个渠道里同一个名字出现两次时只报一次（models 是库里的数组，
		// 写入路径不去重）：重复的一行只会把「有几处配错了」数大。
		seen := map[string]bool{}
		for _, name := range ch.Models {
			if seen[name] {
				continue
			}
			seen[name] = true
			declared, known := in.catalogEndpoints[name]
			if !known {
				continue
			}
			// 按**声明的一组协议**判：多协议渠道只要有一条端点被目录声明，
			// 这个模型就能经由那条协议被服务（见 EndpointFitForProtocols）。
			// 单协议渠道的声明就是它的 type 一个，措辞与之前逐字相同。
			fit := channels.EndpointFitForProtocols(ch.DeclaredProtocols(), declared)
			if !fit.Mismatch() {
				continue
			}
			findings = append(findings, doctorProblem{
				headline: fmt.Sprintf("渠道 %s 的 models 里有 %q：%s",
					doctorChannelLabel(ch.ID, ch.Name), name, fit.Summary()),
				detail: fit.Reason(),
			})
		}
	}
	return findings
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
		reason := "没有可服务的规范名"
		why := "可服务的规范名一个都没有：enabled 渠道的 models 与目录的交集是空的" +
			"（见上面「渠道」与「模型目录」两项）"
		aliasCheck.skip, priceCheck.skip = reason, reason
		aliasCheck.notes = append(aliasCheck.notes, why)
		priceCheck.notes = append(priceCheck.notes, why)
		return aliasCheck, priceCheck
	}

	cat := catalog.New(db)
	priceAdmin := pricing.NewAdmin(db)
	now := time.Now()

	for i := range pairs {
		p := &pairs[i]

		upstream, _, err := cat.Alias(ctx, p.channelID, p.model)
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
				aliasCheck.findings = append(aliasCheck.findings, doctorProblem{
					headline: fmt.Sprintf("「%s」在渠道 %d（%s）上解析不出上游名",
						p.model, p.channelID, p.channelName),
					detail: "这个规范名配过别名，但没有一条覆盖这个渠道，这次调用会回 500。" +
						"补一条渠道级别名、或加一条平台级别名即可",
				})
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
				priceCheck.findings = append(priceCheck.findings, doctorProblem{
					headline: fmt.Sprintf("「%s」在渠道 %d（%s）上没有生效的计价规则",
						p.model, p.channelID, p.channelName),
					detail: "这次调用会失败，而不是按零价放行。" +
						"补一条规则，或者让某条规则的 --models 留空当兜底",
				})
			}
			priceCheck.table.rows = append(priceCheck.table.rows,
				[]string{p.model, doctorChannelLabel(p.channelID, p.channelName), rule, verdict})
		}
	}

	aliasCheck.skip = doctorPairFatalSkip(pairs, func(p doctorPair) error { return p.upstreamFatal }, "别名解析")
	priceCheck.skip = doctorPairFatalSkip(pairs, func(p doctorPair) error { return p.ruleFatal }, "计价规则")
	if aliasCheck.skip == "" {
		aliasCheck.ok = fmt.Sprintf("%d 组都能解析出上游名", len(aliasCheck.table.rows))
	}
	if priceCheck.skip == "" {
		priceCheck.ok = fmt.Sprintf("%d 组都有生效的计价规则", len(priceCheck.table.rows))
	}
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
			if seen[name] || !in.inCatalog(name) {
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
func doctorAliasReachability(aliases []catalog.Alias, servable map[string]bool) []doctorProblem {
	var findings []doctorProblem
	for _, name := range doctorAliasNames(aliases) {
		if servable[name] {
			continue
		}
		findings = append(findings, doctorProblem{
			headline: fmt.Sprintf("「%s」配过别名（%s），但没有任何 enabled 渠道的 models 里包含它",
				name, doctorAliasScopes(aliases, name)),
			detail: "这次调用会回 503「no available upstream for the requested model」。" +
				"别名表与渠道白名单各看一半都对，合起来不对",
		})
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

// ── 第 6 项：规则里的模型名 ─────────────────────────────────────────────

// checkDoctorRuleModels 查计价规则与成本规则里写到的模型名在不在模型目录里。
//
// 写入侧不拦这件事（见 #1052）：先写规则、后建模型是正常顺序，规则也可能由
// 迁移或导入写进来，只在写入口拦等于只拦了一部分。体检是唯一覆盖全部写入
// 路径的地方，所以判据放在这里。
//
// 两类规则同形，都走 model_patterns，判据只写一遍；报出时按来源分开说，因为
// 拿到一个规则 ID 之后，去看它的是两条不同的命令。
//
// 空名单不是问题：它表示「适用于全部模型」（见 pricing.RuleInfo.ModelPatterns），
// 不指向任何名字，也就不可能对不上。
func checkDoctorRuleModels(ctx context.Context, db *sql.DB, in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "规则模型名", subject: doctorSubjectRuleModels}

	rules, err := pricing.NewAdmin(db).List(ctx)
	if err != nil {
		c.skip = fmt.Sprintf("读计价规则失败：%v", err)
		return c
	}
	costs, err := pricing.NewCostAdmin(db).List(ctx, pricing.CostListQuery{})
	if err != nil {
		c.skip = fmt.Sprintf("读成本规则失败：%v", err)
		return c
	}

	channelNames := make(map[int64]string, len(in.channels))
	for _, ch := range in.channels {
		channelNames[ch.ID] = ch.Name
	}

	// 两类规则折成同一个形状再走一遍判据：它们只在「怎么称呼自己」与
	// 「去看它的命令」上不同，差集那一段逐字相同。
	type ruleSource struct {
		label    string
		lookup   string
		patterns []string
	}
	sources := make([]ruleSource, 0, len(rules)+len(costs))
	for _, r := range rules {
		sources = append(sources, ruleSource{
			label:    fmt.Sprintf("计价规则 %d（%s）", r.ID, r.Name),
			lookup:   fmt.Sprintf("larkaxctl price show %d", r.ID),
			patterns: r.ModelPatterns,
		})
	}
	for _, r := range costs {
		channel := fmt.Sprintf("渠道 %d", r.ChannelID)
		if name, ok := channelNames[r.ChannelID]; ok {
			channel = fmt.Sprintf("渠道 %d（%s）", r.ChannelID, name)
		}
		sources = append(sources, ruleSource{
			label:    fmt.Sprintf("成本规则 %d（%s）", r.ID, channel),
			lookup:   fmt.Sprintf("larkaxctl upstream-cost list --channel %d", r.ChannelID),
			patterns: r.ModelPatterns,
		})
	}

	withNames := 0
	names := map[string]bool{}
	c.table = &doctorTable{header: []string{"规则", "模型名", "结论"}}
	for _, src := range sources {
		if len(src.patterns) == 0 {
			continue
		}
		withNames++
		seen := make(map[string]bool, len(src.patterns))
		for _, name := range src.patterns {
			if seen[name] {
				continue
			}
			seen[name] = true
			names[name] = true

			verdict := "在目录里"
			if !in.inCatalog(name) {
				verdict = "目录里没有"
				c.findings = append(c.findings, doctorProblem{
					headline: fmt.Sprintf("%s 的 --models 里有 %q，但目录里没有这个名字",
						src.label, name),
					detail: "这条规则永远命中不了这个模型：--models 按元素逐字匹配规范名，" +
						"差一个字符就不会中。如果这个模型还没建，等它建好这一项就绿了；" +
						"否则是拼写错了。用 " + src.lookup + " 看它的完整配置",
				})
			}
			c.table.rows = append(c.table.rows, []string{src.label, name, verdict})
		}
	}

	disabled := 0
	for _, r := range rules {
		if !r.Enabled {
			disabled++
		}
	}
	c.notes = append(c.notes, fmt.Sprintf(
		"计价规则 %d 条（其中停用的 %d 条）、成本规则 %d 条；其中写了 --models 的 %d 条",
		len(rules), disabled, len(costs), withNames))

	if len(sources) == 0 {
		c.skip = "没有可查的规则"
		c.notes = append(c.notes, "库里还没有计价规则与成本规则")
		return c
	}
	if withNames == 0 {
		c.skip = "规则里没有写模型名"
		c.notes = append(c.notes,
			"空名单表示适用于全部模型，不指向任何名字，没有可对账的对象")
		return c
	}
	c.ok = fmt.Sprintf("%d 条规则、%d 个模型名都在目录里", withNames, len(names))
	return c
}

// ── 第 7 项：钱包 ───────────────────────────────────────────────────────

func checkDoctorWallets(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "钱包", subject: doctorSubjectWallet}

	if len(in.users) == 0 {
		c.findings = append(c.findings, doctorProblem{
			// 这一条就是「用户清单是空的」那件事，所以它说清单空状态那一句
			// （style.go 的 emptyListNote）：同一个状态在两处各说各的措辞时，
			// 读的人分不清它们是同一件事还是两个不同的状态。
			headline: emptyListNote("用户", "user create"),
			detail:   "没有任何一把 API Key 能挂上去，请求会回 401",
		})
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
		c.findings = append(c.findings, doctorProblem{
			headline: who + "没有钱包行",
			detail:   "它第一次调用会在预留那一步失败（500）",
		})
	}
	if wallets > 0 && funded == 0 {
		c.findings = append(c.findings, doctorProblem{
			headline: fmt.Sprintf("%d 个钱包行的余额都是 0", wallets),
			detail:   "调用会在预留那一步因余额不足失败（500）",
		})
	}
	c.ok = fmt.Sprintf("用户 %d 个都有钱包行，其中 %d 个余额大于 0", len(in.users), funded)
	return c
}

// ── 第 8 项：入站别名被遮蔽 ─────────────────────────────────────────────

// checkDoctorInboundShadowed 查「入站别名与目录里的规范名同名」。
//
// 判据与 ResolveInbound 一致：同名时规范名优先，这条别名永远不会生效。写入侧
// 会拒绝这种别名（见 catalog.CreateInboundAlias），但目录行可以事后新建，
// 迁移与导入也绕得过去——体检是唯一看得到全部写入路径的地方。
func checkDoctorInboundShadowed(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "入站别名遮蔽", subject: doctorSubjectInboundShadowed}
	c.notes = append(c.notes, fmt.Sprintf("读到的入站别名 %d 条", len(in.inboundAliases)))

	for _, a := range in.inboundAliases {
		if !in.inCatalog(a.Alias) {
			continue
		}
		c.findings = append(c.findings, doctorProblem{
			headline: fmt.Sprintf("%q 既是入站别名、又是目录里的规范名，解析时规范名优先、这条别名不会被用到", a.Alias),
			detail: "换一个不与任何规范名冲突的别名：先 larkaxctl model alias remove " + a.Alias +
				"，再用 larkaxctl model alias add <新别名> " + a.CanonicalName + " 加回；" +
				"这条别名本来就不需要的话，只 remove 即可",
		})
	}

	switch {
	case len(in.inboundAliases) == 0:
		// 一条都没配是正常状态（大多数部署都没有别名），不是「没跑成」。
		c.ok = "没有入站别名，没有被遮蔽的"
	case len(c.findings) == 0:
		c.ok = fmt.Sprintf("%d 条入站别名里没有被同名规范模型遮蔽的", len(in.inboundAliases))
	}
	return c
}

// ── 第 9 项：入站别名指向不存在的规范名 ─────────────────────────────────

// checkDoctorInboundDangling 查「入站别名指向的规范名不在目录里」。
//
// 写入侧会拦：CreateInboundAlias 校验规范名存在，model delete 又按引用数
// 拒绝删除（见 countReferences）。但迁移、导入或绕过命令行的写入不经过这
// 两道校验。用这种别名的请求会在选路处失败，而那条消息不会提到别名——所以
// 由体检点名到别名与它指向的名字。
func checkDoctorInboundDangling(in doctorChainInput) doctorCheck {
	c := doctorCheck{title: "入站别名悬空", subject: doctorSubjectInboundDangling}
	c.notes = append(c.notes, fmt.Sprintf("读到的入站别名 %d 条", len(in.inboundAliases)))

	for _, a := range in.inboundAliases {
		if in.inCatalog(a.CanonicalName) {
			continue
		}
		c.findings = append(c.findings, doctorProblem{
			headline: fmt.Sprintf("入站别名 %q 指向的规范名 %q 不在目录里", a.Alias, a.CanonicalName),
			detail: "写入侧正常会拦这件事（model alias add 校验规范名存在），这类行来自迁移、导入或绕过命令行的写入。" +
				"用这个别名的请求会在选路处失败：调用方拿到 503「no available upstream for the requested model」" +
				"（错误码 no_candidate_channel），那句话不会提到别名。" +
				"处置：把模型建出来（larkaxctl model create --model " + a.CanonicalName +
				" --display <展示名>），或者删掉这条别名（larkaxctl model alias remove " + a.Alias + "）",
		})
	}

	switch {
	case len(in.inboundAliases) == 0:
		c.ok = "没有入站别名，没有悬空的"
	case len(c.findings) == 0:
		c.ok = fmt.Sprintf("%d 条入站别名的规范名都在目录里", len(in.inboundAliases))
	}
	return c
}

// ── 第 10 项：真打一次上游 ──────────────────────────────────────────────

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

// doctorEndpoints 是本命令认的三个入口，按 chat → messages → responses 排。
//
// 合法取值、路径、请求体与凭证头放在同一张表里：查表、报错里列出的那一串、
// 以及需要逐个拿到取值的地方，读的都是这一份。分成两处时，加第四个入口要改
// 好几处，而漏改报错那一处没有任何东西会报出来——它看起来完全正常，只是说了
// 一句错在自己边界上的话。
//
// 这张表是校验与报错的唯一来源。帮助文本里另有两处手写的枚举，都由用例钉住：
// 短版与长版用法里的 `[--endpoint …]` 串（TestDoctorUsageMatchesEndpointTable），
// 以及 doctorUsage 说明段里「<名字> 是 POST <path>」的逐条描述
// （TestDoctorUsageDescribesEveryEndpoint）。手页由 usage 生成，
// check-man-pages.sh 逐字节比对产物与来源，改完帮助要重生成；docs/ 里的那几处
// 清单（散文段落与表格）随文档一起人工维护，不在用例覆盖内。
//
// 新增一个入口 = 加一条表项，并同步帮助文本里那两处；漏改时上面两条用例会红，
// 测试里的字面量（清单、整句报错、渲染串、路径表）也会跟着红。
var doctorEndpoints = []doctorEndpointSpec{
	{
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
	},
	{
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
	},
	{
		name: doctorEndpointResponses,
		path: doctorPathResponses,
		payload: func(model string) any {
			return doctorResponsesRequest{
				Model:           model,
				Instructions:    doctorLiveInstructions,
				Input:           []doctorResponsesInput{{Role: "user", Content: doctorLivePrompt}},
				MaxOutputTokens: doctorLiveMaxTokens,
			}
		},
		// Responses 是 OpenAI 那一套，凭证按它的习惯走 Bearer。
		setAuth: func(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) },
	},
}

// doctorEndpointNames 是三个入口的名字，顺序与 doctorEndpoints 一致。
//
// 由表算出来，不另写一份：报错里那一串由 doctorEndpointChoices 取用，手写的
// 第二份不会跟着表变。
func doctorEndpointNames() []string {
	names := make([]string, 0, len(doctorEndpoints))
	for _, spec := range doctorEndpoints {
		names = append(names, spec.name)
	}
	return names
}

// doctorEndpointChoices 把入口名字渲染成报错里那一串：顿号分隔，最后一项前用「或」。
//
// 由 doctorEndpointNames() 算出来而不是另写一份字面量：手写的那一份在清单变动
// 之后不会跟着变，而「报错里列的还是老的那几条」没有任何东西会报出来。
func doctorEndpointChoices() string {
	names := doctorEndpointNames()
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			if i == len(names)-1 {
				b.WriteString(" 或 ")
			} else {
				b.WriteString("、")
			}
		}
		b.WriteString(name)
	}
	return b.String()
}

func lookupDoctorEndpoint(name string) (doctorEndpointSpec, error) {
	for _, spec := range doctorEndpoints {
		if spec.name == name {
			return spec, nil
		}
	}

	return doctorEndpointSpec{}, fmt.Errorf("--endpoint 只能是 %s，当前为 %q",
		doctorEndpointChoices(), name)
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

// doctorResponsesRequest 是 responses 入口的请求体（OpenAI Responses 形状）。
//
// 它与另外两条差得远，不能照 chat 的 messages[] 凑：
//
//   - system 是顶层的 instructions，不是一条 system 消息；
//   - 输入放在 input[] 里，这里放一条 message item；
//   - 输出上界叫 max_output_tokens。
//
// max_output_tokens 同样是必填——协议里它是可选的，但 larkax 的网关按它算
// 预留上界（见 pipeline.Request.validate 与 billing.ErrNoUpperBound），缺了
// 会在请求出网关之前就 400。relaykit 把它落在 MaxCompletionTokens 上，
// 所以这个字段名不能写成 max_tokens（见 internal/relay 的 FromRelaykitRequest）。
type doctorResponsesRequest struct {
	Model           string                 `json:"model"`
	Instructions    string                 `json:"instructions"`
	Input           []doctorResponsesInput `json:"input"`
	MaxOutputTokens int                    `json:"max_output_tokens"`
}

// doctorResponsesInput 是 input[] 里的一条 message item。
//
// content 用字符串形态：Responses 也接受 [{"type":"input_text","text":…}]，
// 但一条纯文本消息用字符串更短，且 relaykit 的两条形状都认（见 internal/relay
// 的 TestParseResponsesRequestInputItems）。
type doctorResponsesInput struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// doctorLiveResponse 是三个入口的响应体里 doctor 要看的那几栏。
//
// 两套协议的用量字段名不同（prompt/completion 与 input/output），所以两组
// 都收着，由 usageLabel 挑一组打得出来的。
//
// Error.Code 与 Error.Type 是同一个机器可读位置的两种写法：OpenAI 的码在
// error.code，Anthropic 的码在 error.type（见 internal/api 的 writeErrorCode）。
// doctor 只用它认一个值——insufficient_quota，而那个码恰恰是两套协议都在
// 各自那个位置给的（见 internal/api 的 writeInsufficientQuota）。但那个码
// 分不开它的两个来源，所以归因还读 Error.Message（见 attribution）。
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
		Code    string `json:"code"`
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
	errCode string
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
	// 码优先取 error.code（OpenAI 与 Responses 那一套），其次取 error.type
	// （Anthropic 把码写在这里，见 internal/api 的 writeErrorCode）。两条
	// 分支合起来覆盖三套协议，不需要再看别的位置。
	switch {
	case parsed.Error.Code != "":
		out.errCode = parsed.Error.Code
	case parsed.Error.Type != "":
		out.errCode = parsed.Error.Type
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
	errCode   string
	preview   string

	// version 是网关回的 X-Larkax-Version 头，没有时为空的。
	version string
	// retryAfter 是网关回的 Retry-After 头（限流那一条会带），没有时为空。
	retryAfter string

	sendErr error
}

// ok 判断这一次真打上游算不算通过。没开 --live 时它恒为 false，调用方要
// 先看 enabled。
func (l doctorLive) ok() bool {
	return l.enabled && l.sendErr == nil && l.status >= 200 && l.status < 300
}

// fromGateway 判断这个响应是不是我们自己的网关回的。
//
// 判据是响应头 X-Larkax-Version：公共入口的每一个响应都带它（见
// internal/api 的 Handler.ServeHTTP），而链路上的反代、别的服务、以及
// 上游自己都不会带。
//
// 用头而不是「耗时 3ms」：耗时是一次性的观测，机器负载一变它就不成立，
// 而它也说不出「是网关的哪一层拒的」。用头而不是状码：502/503/500 既可能
// 是我们的网关回的，也可能是反代自己回的。
//
// 已知边界：网关以 `dev` 版本裸跑时这个头也在（Handler.Version 非空即写），
// 所以「版本头在不在」与「版本值是什么」是两件事——这里只看在不在。
func (l doctorLive) fromGateway() bool {
	return l.version != ""
}

// rejectedByUs 判断这个失败的响应是我们自己的网关**在调上游之前**拒掉的，
// 而不是上游拒的。
//
// 「在调上游之前」的判据是状态码：internal/api 的 httpStatusOfUpstream 把
// 每一类失败都映射到固定的状态码，而上游给出的失败只有两种落点——
// 上游限流原样透出 429，其余一律 502（见那个函数的注释）。所以 4xx 与
// 401/403/413/415/429/501/503/500 这些是**我们**在这一侧判的，502 才是
// 上游那一次调用失败。
//
// 429 是个例外，必须单独看：网关既会用 429 回自己的按 Key 限流（带
// Retry-After）与额度用尽（带 insufficient_quota 码），也会把上游的限流
// 原样透出 429（两个都不带）。前两个是我们这一侧的结论，后一个不是。
//
// 没有版本头时一律判「不是我们拒的」：那时连这个响应是谁回的都不确定，
// 说成「我们拒的」只是把一次地址配错说成一次配置问题。
func (l doctorLive) rejectedByUs() bool {
	if !l.fromGateway() || l.ok() {
		return false
	}
	switch {
	case l.status == http.StatusTooManyRequests:
		return l.retryAfter != "" || l.errCode == doctorCodeInsufficientQuota
	case l.status == http.StatusBadGateway:
		// 上游那一次调用失败。**不是**我们在入口拒的。
		return false
	case l.status >= 400 && l.status < 600:
		return true
	}
	// 3xx 之类：网关不会拿它当失败回，走到这里说明判据与实现不一致，
	// 不替它编一个归因。
	return false
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
	out.version = resp.Header.Get(doctorHeaderVersion)
	out.retryAfter = resp.Header.Get(doctorHeaderRetryAfter)
	parsed := parseDoctorLiveBody(body)
	out.respModel, out.usage, out.errMsg = parsed.model, parsed.usage, parsed.errMsg
	out.errCode, out.preview = parsed.errCode, parsed.preview
	return out
}

// ── 输出 ────────────────────────────────────────────────────────────────

// printDoctorReport 打一份体检报告。
//
// 每一行由 statusLine / printStatusLines（style.go）渲染，与其余诊断体检类
// 命令共用同一条路径：一行一个状态符，细节只在 -v 时缩进跟在后面。
func printDoctorReport(w io.Writer, rep doctorReport, verbose bool) {
	_, _ = fmt.Fprintln(w, "larkaxctl doctor：逐项检查「配好的链路能不能接一次请求」。")
	_, _ = fmt.Fprintln(w)

	printStatusLines(w, statusLines(rep), verbose)

	passed, failed, skipped := rep.counts()
	_, _ = fmt.Fprintf(w, "\n  doctor: %d 项异常, %d 项跳过, %d 项通过 (运行 -v 查看细节)\n",
		failed, skipped, passed)
}

// statusLines 把报告折成默认输出的那几行。
func statusLines(rep doctorReport) []statusLine {
	checks := rep.checks()
	lines := make([]statusLine, 0, len(checks)+1)
	for _, c := range checks {
		lines = append(lines, statusLine{
			kind:       c.glyph(),
			title:      c.title,
			conclusion: c.conclusion(),
			detail:     c.detailLines(),
		})
	}
	return append(lines, doctorLiveLine(rep.live))
}

// doctorLiveLine 把真打上游那一次折成一行。
func doctorLiveLine(l doctorLive) statusLine {
	line := statusLine{title: "上游连通", detail: liveDetail(l)}
	switch {
	case !l.enabled:
		line.kind = glyphNone
		line.conclusion = "没跑：加 --live 才会真打一次上游（会花钱、要凭证）"

	case l.sendErr != nil:
		// 「没发出去」与「发出去了、上游回了失败」是两件事，现场完全不同
		// （一个是地址/网络，一个是链路配置），所以结论里也是两句话。
		line.kind = glyphFail
		line.conclusion = fmt.Sprintf("请求没发出去：%v", l.sendErr)

	case l.ok():
		line.kind = glyphPass
		line.conclusion = liveOKLabel(l)

	default:
		line.kind = glyphFail
		// 归因那一句（谁拒的）就是这一项的根因，所以它进默认那一行：
		// #381 那次事故里缺的正是这一句，而它在 -v 里等于没打。
		line.conclusion = l.attribution()
	}
	return line
}

// liveOKLabel 是那一次调用成功时的结论：状态码、耗时、响应模型与用量。
func liveOKLabel(l doctorLive) string {
	label := fmt.Sprintf("通过（%d，耗时 %s", l.status, l.elapsed.Round(time.Millisecond))
	if l.respModel != "" {
		label += "，响应里的模型是 " + l.respModel
	}
	if l.usage != "" {
		label += "，用量 " + l.usage
	}
	return label + "）"
}

// liveDetail 是 -v 时真打上游那一行的细节。
func liveDetail(l doctorLive) []string {
	if !l.enabled {
		return []string{
			"没跑：要加 --live 才会跑，它会真的发一次请求到上游，会花钱、要凭证。",
			"真打上游不在这九项里：它的结论在上一行，失败时退出码同样非 0。",
		}
	}

	lines := []string{"入口：POST " + l.url, "模型：" + l.model}
	if l.sendErr != nil {
		return append(lines, fmt.Sprintf("结论：不通过——请求没发出去：%v", l.sendErr))
	}

	lines = append(lines, fmt.Sprintf("结果：%d，耗时 %s", l.status, l.elapsed.Round(time.Millisecond)))
	if l.errMsg != "" {
		lines = append(lines, "错误："+l.errMsg)
	}
	if l.preview != "" {
		lines = append(lines, "响应体："+l.preview)
	}
	if l.ok() {
		return append(lines, "结论：通过")
	}
	return append(lines, "谁拒的："+l.attribution(), "结论：不通过")
}

// attribution 说明这次失败是谁拒的。
//
// 这句话是这个命令最要紧的一行：状态码 400 与「上游回了一个 400」在输出里
// 长得一样，而两者的排查方向完全相反——一个要改自己的请求或配置，
// 一个要去看上游。doctor 原先只打「结果：400」，于是有人朝上游找，
// 而问题是它自己发出的请求缺了一个字段（见 issue #381）。
//
// 「是我们拒的」这一层里还要分清是**哪一种**拒绝：429 + insufficient_quota
// 有两个来源，处置相反（改 key 的额度 / 充值），码分不开、message 分得开。
// 所以这一层先按 message 精确认，认不出来再按码说一句更保守的话——认不出
// 不等于可以往一个方向猜（见那两条分支上的注释）。
func (l doctorLive) attribution() string {
	if !l.fromGateway() {
		return "这个响应不是 larkax 网关回的（没有 " + doctorHeaderVersion +
			" 头）：先确认 --base-url 指对了地方、链路上有没有别的服务在拦"
	}
	if l.rejectedByUs() {
		reason := "在网关入口就被拒了，请求没有出网关，不是上游"
		switch {
		// 先按我们自己那句 message 认「谁拒的」，认不出来再按码兜底。
		// 429 + insufficient_quota 有两个来源（按 Key 的额度用尽 / 钱包
		// 可用余额不足），码分不开、message 分得开（见上面那个 const 块的
		// 注释）——只按码归因会把第二种说成第一种，让人去查 key 的额度，
		// 而该做的是充值。
		case l.errMsg == doctorMsgInsufficientQuota:
			reason = "是这把 API Key 的额度用尽了（" + doctorCodeInsufficientQuota +
				"），要改的是这把 key 的额度或换一把 key；请求没有出网关，不是上游"
		case l.errMsg == doctorMsgInsufficientBalance:
			reason = "是这个账户的钱包可用余额不够这次预留（" + doctorCodeInsufficientQuota +
				"），要充值；请求没有出网关，不是上游"
		case l.status == http.StatusTooManyRequests && l.retryAfter != "":
			reason = "是我们自己的按 Key 限流（带 " + doctorHeaderRetryAfter +
				" 头），请求没有出网关，不是上游"
		case l.status == http.StatusTooManyRequests && l.errCode == doctorCodeInsufficientQuota:
			// 码认出来了、message 却不在我们认识的那两句里（文案换过词、
			// 或者将来又多了一个来源）。这时**不猜**：说成「账户状态类拒绝」
			// 并让对方去看那句 message，比按码断言「是 Key 的额度」更保守
			// ——后者正是这个 issue 要修的那种指错人。
			reason = "是账户状态类拒绝（" + doctorCodeInsufficientQuota +
				"），具体是额度还是余额看上面那句 message；请求没有出网关，不是上游"
		}
		if l.errMsg != "" {
			reason += "；上面那句错误是我们自己写的"
		}
		return reason
	}
	if l.status == http.StatusBadGateway {
		return "上游那一次调用失败（网关回 502 表示「我们与上游之间」）：问题在上游或渠道配置"
	}
	if l.status == http.StatusTooManyRequests {
		// 走到这里说明 429 上既没有 Retry-After、也没有额度码——网关把上游的
		// 限流原样透出了（见 internal/api 的 httpStatusOfUpstream）。
		return "上游在限流（网关把它原样透出 429）：等一会儿再试，不是我们这一侧拒的"
	}
	return "无法确定是谁拒的：这个状态码不是网关自己会写的失败，看上面的错误说明与响应体"
}

// ── 机器可读输出 ────────────────────────────────────────────────────────
//
// 这一节把同一份报告渲染成 `--format json` 那一档（见 format.go 与
// docs/cli-output.md）。它与上面 printDoctorReport 那一条路**并列**，共用的
// 是同一个 doctorReport，所以两条路不可能出现「表格说过了、JSON 说没过」。
//
// 与表格那一档的三处刻意的差别：
//
//   - **问题不截断**。表格默认只打第一条、其余的折成「另有 N 条」——
//     那是屏幕宽度的问题。脚本按模型名去筛某一条问题时，截断会让它
//     「查不到」而不是「有更多」，那是两件完全不同的事。
//   - **-v 不存在**。表格那边细节要 -v 才打；这里所有字段一直都在，
//     否则 `-o json` 的调用方还得再记一个 -v。
//   - **结论那一栏两种都有**。每项既给结论那一句（`conclusion`，与终端上
//     看到的一字不差），也给结构化的问题清单（`problems`）。前者是「人会
//     怎么读」，后者是「脚本要筛什么」，从同一份 doctorCheck 推出来。

// jsonStatus 给出整份报告的那一档结论。
//
// 判据与退出码**无关**：退出码是进程的契约（连不上库是 3），而这里的
// status 说的是「这份报告读出什么」。两者不一致时以 exit_code 为准
// （见 docs/cli-output.md），所以 status 不必去复刻那五个数。
func (r doctorReport) jsonStatus() string {
	_, failed, skipped := r.counts()
	if failed > 0 || (r.live.enabled && !r.live.ok()) {
		return jsonFailed
	}
	if skipped > 0 {
		return jsonSkipped
	}
	return jsonOK
}

// doctorDoc 把一份体检报告折成机器可读输出。
//
// summary 里的 total 是**九项只读检查**，不含真打上游那一次：那一项有自己的
// 开关（--live），把它算进分母会让「九项都过了」与「九项都过了、上游也通了」
// 给出不同的 total，而两者的区别在 live 那个对象里。
func doctorDoc(r doctorReport) map[string]any {
	passed, failed, skipped := r.counts()
	doc := jsonDoc(r.jsonStatus())
	doc["summary"] = map[string]any{
		"total":   len(r.checks()),
		"passed":  passed,
		"failed":  failed,
		"skipped": skipped,
	}

	checks := make([]map[string]any, 0, len(r.checks()))
	for _, named := range r.namedChecks() {
		checks = append(checks, doctorCheckDoc(named.name, named.check))
	}
	doc["checks"] = checks
	doc["live"] = doctorLiveDoc(r.live)
	return doc
}

// doctorCheckDoc 把一项检查折成机器可读的一条。
func doctorCheckDoc(name string, c doctorCheck) map[string]any {
	problems := make([]map[string]any, 0, len(c.findings))
	for _, p := range c.findings {
		problems = append(problems, map[string]any{
			"headline": p.headline,
			"detail":   nullableString(p.detail),
		})
	}

	out := map[string]any{
		"name":       name,
		"title":      c.title,
		"status":     c.status().jsonLabel(),
		"subject":    c.subject,
		"conclusion": c.conclusion(),
		"ok":         nullableString(c.ok),
		"notes":      jsonList(c.notes),
		"problems":   problems,
		// skip 与 problems 是两种不同的坏消息，分开两栏：前者是「没查成」，
		// 后者是「查了，有问题」。合成一栏会让脚本分不出「这项没问题」
		// 与「这项没查」——而这个项目为这一条单独立过一个状态符（见 style.go）。
		"skip_reason": nullableString(c.skip),
		"table":       doctorTableDoc(c.table),
	}
	return out
}

// doctorTableDoc 把某一项的表折成机器可读的形状。
//
// 表里的单元格是**已经渲染好的字符串**（列宽、状态符那些都在
// printXxx 那一层），这里不假装它们是结构化数据：给出表头与行，让读的人
// 自己按表头对。没有表时是 null 而不是空表——空表会让「这一项本来就没有表」
// 与「表在、但一行都没有」长得一样。
func doctorTableDoc(t *doctorTable) any {
	if t == nil || len(t.rows) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(t.rows))
	for _, row := range t.rows {
		rows = append(rows, jsonList(row))
	}
	return map[string]any{"header": jsonList(t.header), "rows": rows}
}

// doctorLiveDoc 把真打上游那一次折成机器可读的一条。
//
// 没开 --live 时只给 enabled=false 一个键：其余几栏都没有取值，铺一排 null
// 会让人以为「跑了，只是没读到」。enabled 为真时所有栏都在（缺的给 null），
// 因为那时「没有值」本身是有含义的——没有 status_code 就是「一个 HTTP 响应
// 都没拿到」，与「上游回了 4xx」是两件事。
func doctorLiveDoc(l doctorLive) map[string]any {
	if !l.enabled {
		return map[string]any{"enabled": false, "ok": false}
	}

	out := map[string]any{
		"enabled":  true,
		"ok":       l.ok(),
		"endpoint": l.endpoint,
		"url":      l.url,
		"model":    l.model,
		// 状态码 0 表示请求根本没拿到 HTTP 响应（发不出去、或读响应体失败），
		// 这里给 null 而不是 0：0 不是一个 HTTP 状态码，读成「上游回了 0」
		// 比读成「没拿到」坏得多。
		"status_code":     nullableInt(l.status),
		"elapsed_ms":      l.elapsed.Milliseconds(),
		"response_model":  nullableString(l.respModel),
		"usage":           nullableString(l.usage),
		"error":           nullableString(l.errMsg),
		"error_code":      nullableString(l.errCode),
		"send_error":      nullableError(l.sendErr),
		"gateway_version": nullableString(l.version),
		"retry_after":     nullableString(l.retryAfter),
		"from_gateway":    l.fromGateway(),
		"rejected_by_us":  l.rejectedByUs(),
		// 归因那一句只在失败时给：成功时 attribution 会去打 200 的
		// 那一串「无法确定是谁拒的」分支，把那句话放进一份通过的报告里
		// 只会让读的人以为自己漏了什么。
		"attribution": nil,
	}
	if !l.ok() {
		out["attribution"] = l.attribution()
	}
	return out
}
