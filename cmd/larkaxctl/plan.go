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

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/plans"
)

// planHelp 是 plan 的两版帮助文本（见 helpText）。
var planHelp = helpText{short: planShortUsage, long: planUsage}

// planShortUsage 是 plan 的短版：`-h` / `--help` 与参数写错时打的那一份。
//
// 这里只剩**商品定义**（一个包是什么、能不能再发）。某个用户持有的订阅、扣费顺序
// 与 Key 的额度包名单已经搬到 user 那一组，旧路径仍然可用，但不在这份用法里占行。
const planShortUsage = `larkaxctl plan 管理额度包（商品定义）。
用法：
  larkaxctl plan add <key> --name <名称> --unit <amount|token|request>
                           [--quota <可用量>] [--face-value <面值>]
                           [--models <模型,...>] [--validity-days <天数>]
  larkaxctl plan list
  larkaxctl plan get --key <key>
  larkaxctl plan enable|disable --key <key>

key 也可写成 --key，旧动词 create 照旧可用；--name 是展示名。
--validity-days 决定这个包能不能卖：给了才能被 wallet payment create --plan 买。
用户持有的订阅、扣费顺序与 Key 的额度包名单：larkaxctl user plan、larkaxctl user key。
更细的边界：larkaxctl help plan。
`

// planUsage 是 plan 的长版：`larkaxctl help plan` 与 `plan help`。
const planUsage = `larkaxctl plan 管理额度包（商品定义）。

用法：
  larkaxctl plan add <key> --name <名称> --unit <amount|token|request>
                           [--quota <可用量>] [--face-value <面值>]
                           [--models <模型,...>] [--description <说明>]
                           [--validity-days <天数>]
  larkaxctl plan list
  larkaxctl plan get --key <key>
  larkaxctl plan enable|disable --key <key>

**key 是 add 的第一个位置参数**，位置参数必须写在旗标前面：
  larkaxctl plan add pro-monthly --name 「专业版月付」 --unit amount --quota 100
--key 仍然认，与位置参数同值时取位置参数，两处写的不是同一个 key 时报错。
--name 是**展示名**，不是键；键建了就不能改。

# 三种计量单位

  amount    金额包：--quota 与 --face-value 是同一个数，只填一个即可；两个都给且
            不相等时报参数错。
  token     token 包：--quota 是输入加输出的 token 数，--face-value 是这个包值
            多少钱。两个都要填。
  request   次数包：--quota 是可用次数，--face-value 同上。两个都要填。

--models 按规范模型名精确匹配，逗号分隔。token 与次数包必须至少给一个：
它们的价值与模型强相关，不限定范围就是一个可能亏本的包。金额包可以不给，
空表示不限模型。

get 打一个包的详情，list 列出全部（含停用的），两者都不改任何东西。
enable / disable 控制一个包还能不能用于**新的发放**：已经发出去的订阅不受影响，
它们的面值在发放时就定下了。

# 售卖

--validity-days 是这个包卖出去时给订阅的有效期天数。**没给它的包不出售**：
wallet payment create --plan 会拒绝，get 里那一栏打「不售卖」。运营发放（user plan
grant）不读它，有效期仍然由发放那条命令给。商户包（owner_user_id 非空）今天
也不出售——售卖的分成口径未定。

**改天数不影响已经卖出去的单子**：每张支付单在下单时把当时的天数记在自己身上，
付款到账时按它算到期时刻，不回头读包。已经发出去的订阅更不受影响（它按库里那一行
算）。还在等付款的单子**不再自动发放**：包上的天数被改成别的值之后，这类单子在发放前
会被拒（报错点名「售卖有效期在售出之后改过」），单子留在 created 等人工核对。包没动过
的单子照常按下单时记下的天数发放。

# 用户持有的订阅

发放、作废、列订阅与扣费顺序都在 larkaxctl user plan 下（grant / void / list /
order）。旧路径 plan grant / plan void / plan subscriptions / plan order 仍可用，
各条在命中时于 stderr 提示新路径。

grant 与 void 接受 --dry-run：走与真跑同一条写路径（同一份校验、同一份幂等判据、
同一笔面值分录），把订阅与分录打出来之后回滚，user_plans 与账本一条都不落。**除序列号
（订阅 id）外，预览与真跑逐字相等**：分录的科目、金额、借贷方向与余额前后完全相同。
订阅 id 是这次事务从序列取到的值，事务回滚不退号，所以真跑会拿到下一个。作废时剩余
面值仍然从账本现算。

--ref 也可以换成 --auto-ref：命令现生成一个 MANUAL_<UTC 日期>_<随机段> 的单号并打进
输出，重跑不会幂等。两个都给、或两个都不给都是参数错（退出 2）。

一把 API Key 能用哪些订阅在 larkaxctl user key plans 下；旧路径 plan key 仍可用。

# 旧名仍可用

create 是 add 的旧名，行为逐字相同；--key 是第一个位置参数的旧写法。
两者都留着是为了不让已经在跑的脚本与部署流程坏掉，新写的命令请用新名字。

更细的约定见 docs/cli-reference.md 的「plan」一节。
`

// planTimeout 是这条命令连库与写库的时间上限（理由同 apiKeyTimeout）。
const planTimeout = 30 * time.Second

// planActions 是 plan 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
//
// 建记录只写正式名 add：create 是它的旧名（见 planUsage 的「旧名仍可用」），
// 旧名不占 Tab 的位置（与 price 同一条口径）。
var planActions = []string{"add", "list", "get", "enable", "disable", "grant", "void", "subscriptions", "key", "order"}

func runPlan(args []string) error {
	fs := cli.NewFlagSet("plan", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return cli.UsageErr(errors.New("plan 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "add", "create":
		// create 是 add 的旧名（见 planUsage 的「旧名仍可用」）。两个名字走同
		// 一条路径，命令自己的名字按 add 报，免得在不支持 json 时说出旧名字。
		return runPlanCreate(rest)
	case "list":
		return runPlanList(rest)
	case "get":
		return runPlanGet(rest)
	case "enable":
		return runPlanEnable(rest, true)
	case "disable":
		return runPlanEnable(rest, false)
	case "grant":
		return runLegacyUserPath("plan grant 已搬到 larkaxctl user plan grant <用户> <套餐>",
			func() error { return runPlanGrant(rest) })
	case "void":
		return runLegacyUserPath("plan void 已搬到 larkaxctl user plan void <订阅 ID>",
			func() error { return runPlanVoid(rest) })
	case "subscriptions":
		return runLegacyUserPath("plan subscriptions 已改名为 larkaxctl user plan list <用户>",
			func() error { return runPlanSubscriptions(rest) })
	case "key":
		return runLegacyUserPath("plan key 已搬到 larkaxctl user key plans <show|set> <API Key ID>",
			func() error { return runPlanKey(rest) })
	case "order":
		return runLegacyUserPath("plan order 已搬到 larkaxctl user plan order <show|set> <用户>",
			func() error { return runPlanOrder(rest) })
	default:
		return cli.UsageErr(fmt.Errorf("未知的 plan 子命令 %q", action))
	}
}

// planKeyOperand 是 plan add 的位置参数：额度包的键。
var planKeyOperand = cli.OperandSpec{
	What:  "额度包的键",
	Flag:  "key",
	Usage: "写成第一个位置参数（larkaxctl plan add <key> …）",
}

func runPlanCreate(args []string) error {
	// 写类命令在 json 模式下必须在做任何事之前拦下来（见 cli 包的 format.go）。
	// 命令名写正式名 add：那句会把命令名打给人看，而 create 是旧写法
	// （见 planUsage 的「旧名仍可用」）。
	if err := cli.JSONUnsupported("plan add"); err != nil {
		return err
	}

	operand, rest, takeErr := cli.TakeLeadingOperand(args, planKeyOperand)
	if takeErr != nil {
		return takeErr
	}

	fs := cli.NewFlagSet("plan add", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	key := fs.String("key", "", "")
	name := fs.String("name", "", "")
	description := fs.String("description", "", "")
	unit := fs.String("unit", "", "")
	quota := fs.String("quota", "", "")
	faceValue := fs.String("face-value", "", "")
	validityDays := fs.Int("validity-days", 0, "")
	models := fs.String("models", "", "")
	asset := fs.String("asset", string(ledger.AssetCNY), "")
	if err := fs.Parse(rest); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf(
			"add 不接受多余的参数，得到 %q：key 要写成第一个位置参数，排在旗标前面", fs.Arg(0)))
	}
	given := visitedFlags(fs)
	planKey, err := cli.ResolveOperand(operand, *key, given["key"], planKeyOperand)
	if err != nil {
		return cli.UsageErr(err)
	}
	if strings.TrimSpace(*name) == "" {
		return cli.UsageErr(errors.New("缺少 --name（展示名）"))
	}
	if strings.TrimSpace(*unit) == "" {
		return cli.UsageErr(errors.New("缺少 --unit"))
	}
	if err = cli.CheckDisplayText("--key", planKey); err != nil {
		return err
	}
	if err = cli.CheckDisplayText("--name", strings.TrimSpace(*name)); err != nil {
		return err
	}
	if err = cli.CheckDisplayText("--description", strings.TrimSpace(*description)); err != nil {
		return err
	}
	if err = cli.CheckDisplayList("--models", splitOptionalList(*models)); err != nil {
		return err
	}

	assetValue := ledger.Asset(strings.ToUpper(strings.TrimSpace(*asset)))
	if !assetValue.Valid() {
		return cli.UsageErr(fmt.Errorf("资产 %q 不受支持（目前只有 %s）", *asset, ledger.AssetCNY))
	}

	in := plans.CreatePlanInput{
		Key:         planKey,
		Name:        strings.TrimSpace(*name),
		Description: strings.TrimSpace(*description),
		Asset:       assetValue,
		Unit:        plans.PlanUnit(strings.TrimSpace(*unit)),
		Models:      splitOptionalList(*models),
	}
	switch in.Unit {
	case plans.UnitAmount, plans.UnitToken, plans.UnitRequest:
	default:
		return cli.UsageErr(fmt.Errorf("--unit 只能是 %s、%s 或 %s，得到 %q",
			plans.UnitAmount, plans.UnitToken, plans.UnitRequest, *unit))
	}
	if strings.TrimSpace(*quota) != "" {
		if in.QuotaTotal, err = parseNamedAmountFlag("--quota", *quota); err != nil {
			return err
		}
	}
	if strings.TrimSpace(*faceValue) != "" {
		if in.FaceValue, err = parseNamedAmountFlag("--face-value", *faceValue); err != nil {
			return err
		}
	}
	if given["validity-days"] {
		if *validityDays <= 0 {
			return cli.UsageErr(fmt.Errorf(
				"--validity-days 必须是正数天，得到 %d：0 天等于发出去就已经到期", *validityDays))
		}
		days := *validityDays
		in.ValidityDays = &days
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		created, err := plans.New(db).CreatePlan(ctx, in)
		if err != nil {
			return err
		}
		printCreatedPlan(cli.HumanStderr(), created)
		return nil
	})
}

func runPlanList(args []string) error {
	fs := cli.NewFlagSet("plan list", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		list, err := plans.New(db).Plans(ctx)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, planListDoc(list), nil)
		}
		printPlanList(cli.HumanStdout(), list)
		return nil
	})
}

func runPlanGet(args []string) error {
	fs := cli.NewFlagSet("plan get", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	key := fs.String("key", "", "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("get 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if strings.TrimSpace(*key) == "" {
		return cli.UsageErr(errors.New("缺少 --key"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		p, err := plans.New(db).PlanByKey(ctx, strings.TrimSpace(*key))
		if err != nil {
			return err
		}
		// 与 list 同一条约定：JSON 是另一条渲染路径，不是把详情表转成 JSON。
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, planDoc(p), nil)
		}
		printPlanDetail(cli.HumanStdout(), p)
		return nil
	})
}

func runPlanEnable(args []string, enabled bool) error {
	command := "plan enable"
	if !enabled {
		command = "plan disable"
	}
	if err := cli.JSONUnsupported(command); err != nil {
		return err
	}

	fs := cli.NewFlagSet(command, flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	key := fs.String("key", "", "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("%s 不接受位置参数，得到 %q", strings.TrimPrefix(command, "plan "), fs.Arg(0)))
	}
	if strings.TrimSpace(*key) == "" {
		return cli.UsageErr(errors.New("缺少 --key"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		p, err := plans.New(db).SetPlanEnabled(ctx, plans.SetPlanEnabledInput{
			Key:     strings.TrimSpace(*key),
			Enabled: enabled,
		})
		if err != nil {
			return err
		}
		printPlanEnabled(cli.HumanStderr(), p)
		return nil
	})
}

func runPlanGrant(args []string) error {
	if err := cli.JSONUnsupported("plan grant"); err != nil {
		return err
	}

	fs := cli.NewFlagSet("plan grant", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	cli.StringAlias(fs, user, "u")
	planKey := fs.String("plan", "", "")
	ref := fs.String("ref", "", "")
	autoRef := cli.AutoRefFlag(fs)
	expiresAt := fs.String("expires-at", "", "")
	ttl := fs.String("ttl", "", "")
	forever := fs.Bool("forever", false, "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("grant 不接受位置参数，得到 %q；干跑请写 --dry-run", fs.Arg(0)))
	}
	if *user == "" {
		return cli.UsageErr(errors.New("缺少 --user"))
	}
	if strings.TrimSpace(*planKey) == "" {
		return cli.UsageErr(errors.New("缺少 --plan"))
	}
	grantRef, refErr := cli.ResolveOrderRef(*ref, *autoRef, cli.MissingRefError("它是这次发放的幂等键"))
	if refErr != nil {
		return refErr
	}
	if err := cli.CheckDisplayText("--ref", grantRef); err != nil {
		return err
	}
	expires, err := cli.ResolveExpiry(providedFlags(fs), cli.ExpiryFlags{
		ExpiresAt: *expiresAt, TTL: *ttl, Forever: *forever,
	}, cli.ExpiryNow(), func(v string) (time.Time, error) {
		return parseFutureExpiresAt(v, "这份额度发出去就已经到期了")
	})
	if errors.Is(err, cli.ErrExpiryNotGiven) {
		return cli.UsageErr(errors.New("缺少过期时间：--expires-at、--ttl 或 --forever 给一个"))
	}
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		owner, err := authStore.ResolveUser(ctx, *user)
		if err != nil {
			return err
		}

		input := plans.GrantInput{
			TxnID:     txnIDFromKey("larkax:plan-grant:v1", grantRef),
			Ref:       grantRef,
			UserID:    owner.ID,
			PlanKey:   strings.TrimSpace(*planKey),
			ExpiresAt: expires,
		}
		// 干跑与真跑共用同一个输入与同一条写路径（见 plans.GrantTx），差别只有
		// 事务边界：订阅行与那一笔面值分录都在事务里写一遍再回滚。
		var (
			up      plans.UserPlan
			applied bool
		)
		if *dryRun {
			dryErr := cli.InRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
				var postErr error
				up, applied, postErr = plans.New(db).GrantTx(ctx, tx, input)
				return postErr
			})
			if dryErr != nil {
				return dryErr
			}
		} else {
			var grantErr error
			up, applied, grantErr = plans.New(db).Grant(ctx, input)
			if grantErr != nil {
				return grantErr
			}
		}
		printPlanGrant(cli.HumanStderr(), owner, up, applied, *dryRun)
		return nil
	})
}

func runPlanVoid(args []string) error {
	if err := cli.JSONUnsupported("plan void"); err != nil {
		return err
	}

	fs := cli.NewFlagSet("plan void", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	id := fs.Int64("id", 0, "")
	ref := fs.String("ref", "", "")
	autoRef := cli.AutoRefFlag(fs)
	status := fs.String("status", string(plans.UserPlanCancelled), "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("void 不接受位置参数，得到 %q；干跑请写 --dry-run", fs.Arg(0)))
	}
	if *id <= 0 {
		return cli.UsageErr(errors.New("缺少 --id：要作废哪一条订阅"))
	}
	voidRef, refErr := cli.ResolveOrderRef(*ref, *autoRef, cli.MissingRefError("它是这次作废的幂等键"))
	if refErr != nil {
		return refErr
	}
	if err := cli.CheckDisplayText("--ref", voidRef); err != nil {
		return err
	}
	final := plans.UserPlanStatus(strings.TrimSpace(*status))
	switch final {
	case plans.UserPlanExpired, plans.UserPlanCancelled:
	default:
		return cli.UsageErr(fmt.Errorf("--status 只能是 %s 或 %s，得到 %q",
			plans.UserPlanExpired, plans.UserPlanCancelled, *status))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		input := plans.VoidInput{
			TxnID:      txnIDFromKey("larkax:plan-void:v1", voidRef),
			UserPlanID: *id,
			Ref:        voidRef,
			Status:     final,
		}
		// 干跑与真跑共用同一个输入与同一条写路径（见 plans.VoidTx）：剩余面值
		// 仍然从账本现算，状态与分录都在事务里写一遍再回滚。
		var (
			up      plans.UserPlan
			applied bool
		)
		if *dryRun {
			dryErr := cli.InRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
				var postErr error
				up, applied, postErr = plans.New(db).VoidTx(ctx, tx, input)
				return postErr
			})
			if dryErr != nil {
				return dryErr
			}
		} else {
			var voidErr error
			up, applied, voidErr = plans.New(db).Void(ctx, input)
			if voidErr != nil {
				return voidErr
			}
		}
		printPlanVoid(cli.HumanStderr(), up, applied, *dryRun)
		return nil
	})
}

func runPlanSubscriptions(args []string) error {
	fs := cli.NewFlagSet("plan subscriptions", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	cli.StringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("subscriptions 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *user == "" {
		return cli.UsageErr(errors.New("缺少 --user"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}
		list, err := plans.New(db).UserPlans(ctx, owner.ID)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, userPlansDoc(owner, list), nil)
		}
		printUserPlans(cli.HumanStdout(), list)
		return nil
	})
}

// planKeyActions 是 plan key 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var planKeyActions = []string{"show", "set"}

func runPlanKey(args []string) error {
	rest := args
	if len(rest) == 0 {
		cli.PrintUsage(os.Stderr, planShortUsage)
		return cli.UsageErr(errors.New("plan key 需要 show 或 set"))
	}
	action, rest := rest[0], rest[1:]
	switch action {
	case "show":
		return runPlanKeyShow(rest)
	case "set":
		return runPlanKeySet(rest)
	case "-h", "--help":
		// 这一组没有自己的两版文本：它由 plan 拆出来，读者要的仍是 plan 那一份
		// 里关于 Key 额度包的那几行。所以 -h 给 plan 的短版、help 给它长版，
		// 与「帮助文案只有一处来源」这条一致。
		cli.PrintUsage(os.Stdout, planShortUsage)
		return nil
	case "help":
		cli.PrintUsage(os.Stdout, planUsage)
		return nil
	default:
		return cli.UsageErr(fmt.Errorf("未知的 plan key 子命令 %q", action))
	}
}

func runPlanKeyShow(args []string) error {
	fs := cli.NewFlagSet("plan key show", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	id := fs.Int64("id", 0, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("show 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *id <= 0 {
		return cli.UsageErr(errors.New("缺少 --id：要看哪一把 API Key"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		svc := plans.New(db)
		declared, err := svc.APIKeyPlanIDs(ctx, *id)
		if err != nil {
			return err
		}
		list, err := svc.APIKeyPlans(ctx, *id)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, apiKeyPlansDoc(*id, declared, list), nil)
		}
		printAPIKeyPlans(cli.HumanStdout(), *id, declared, list)
		return nil
	})
}

func runPlanKeySet(args []string) error {
	if err := cli.JSONUnsupported("plan key set"); err != nil {
		return err
	}

	fs := cli.NewFlagSet("plan key set", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	id := fs.Int64("id", 0, "")
	raw := fs.String("plans", "", "")
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("set 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *id <= 0 {
		return cli.UsageErr(errors.New("缺少 --id：要改哪一把 API Key"))
	}
	given := providedFlags(fs)
	switch {
	case *all && given["plans"]:
		return cli.UsageErr(errors.New("--all 与 --plans 不能同时给：一个表示全部，一个表示指定几条"))
	case !*all && !given["plans"]:
		return cli.UsageErr(errors.New("要么用 --plans 给出名单，要么用 --all 恢复成全部"))
	}

	var ids []int64
	if !*all {
		var err error
		if ids, err = parseIDList(*raw); err != nil {
			return err
		}
		if len(ids) == 0 {
			return cli.UsageErr(errors.New("--plans 给了一个空名单：要恢复成全部用 --all"))
		}
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		got, err := plans.New(db).SetAPIKeyPlans(ctx, *id, ids)
		if err != nil {
			return err
		}
		printAPIKeyPlansSet(cli.HumanStderr(), *id, *all, got)
		return nil
	})
}

// planOrderActions 是 plan order 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var planOrderActions = []string{"show", "set"}

func runPlanOrder(args []string) error {
	rest := args
	if len(rest) == 0 {
		cli.PrintUsage(os.Stderr, planShortUsage)
		return cli.UsageErr(errors.New("plan order 需要 show 或 set"))
	}
	action, rest := rest[0], rest[1:]
	switch action {
	case "show":
		return runPlanOrderShow(rest)
	case "set":
		return runPlanOrderSet(rest)
	case "-h", "--help":
		// 与 plan key 同理：这一组没有自己的两版文本，给 plan 的那一份。
		cli.PrintUsage(os.Stdout, planShortUsage)
		return nil
	case "help":
		cli.PrintUsage(os.Stdout, planUsage)
		return nil
	default:
		return cli.UsageErr(fmt.Errorf("未知的 plan order 子命令 %q", action))
	}
}

func runPlanOrderShow(args []string) error {
	fs := cli.NewFlagSet("plan order show", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	cli.StringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("show 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *user == "" {
		return cli.UsageErr(errors.New("缺少 --user"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}
		order, source, err := plans.New(db).EffectiveChargeOrder(ctx, owner.ID)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, planOrderDoc(owner, order, source), nil)
		}
		printPlanOrder(cli.HumanStdout(), owner, order, source)
		return nil
	})
}

func runPlanOrderSet(args []string) error {
	if err := cli.JSONUnsupported("plan order set"); err != nil {
		return err
	}

	fs := cli.NewFlagSet("plan order set", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	cli.StringAlias(fs, user, "u")
	order := fs.String("order", "", "")
	clear := fs.Bool("clear", false, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("set 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *user == "" {
		return cli.UsageErr(errors.New("缺少 --user"))
	}
	given := providedFlags(fs)
	switch {
	case *clear && given["order"]:
		return cli.UsageErr(errors.New("--clear 与 --order 不能同时给：一个清掉覆盖，一个设值"))
	case !*clear && !given["order"]:
		return cli.UsageErr(errors.New("要么用 --order 给出顺序，要么用 --clear 回到平台默认"))
	}
	value := plans.ChargeOrder(strings.TrimSpace(*order))
	if !*clear && !value.Valid() {
		return cli.UsageErr(fmt.Errorf("--order 只能是 %s、%s 或 %s，得到 %q",
			plans.ChargeOrderPlanFirst, plans.ChargeOrderBalanceFirst,
			plans.ChargeOrderBalanceOnly, *order))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}
		svc := plans.New(db)
		if *clear {
			if err := svc.ClearUserChargeOrder(ctx, owner.ID); err != nil {
				return err
			}
			printPlanOrderSet(cli.HumanStderr(), owner, "", true)
			return nil
		}
		if err := svc.SetUserChargeOrder(ctx, owner.ID, value); err != nil {
			return err
		}
		printPlanOrderSet(cli.HumanStderr(), owner, value, false)
		return nil
	})
}

// parseIDList 解析逗号分隔的 id 名单。
func parseIDList(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for i, part := range parts {
		text := strings.TrimSpace(part)
		if text == "" {
			return nil, cli.UsageErr(fmt.Errorf("名单里第 %d 项是空的", i+1))
		}
		id, err := strconv.ParseInt(text, 10, 64)
		if err != nil || id <= 0 {
			return nil, cli.UsageErr(fmt.Errorf("名单里第 %d 项 %q 不是正的整数 id", i+1, part))
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// unitLabel 把计量单位翻成给人看的说法。
func unitLabel(unit plans.PlanUnit) string {
	switch unit {
	case plans.UnitAmount:
		return "金额"
	case plans.UnitToken:
		return "token"
	case plans.UnitRequest:
		return "次数"
	}
	return string(unit)
}

// planModelsLabel 打一个包的模型范围。金额包没给范围时是「不限」。
func planModelsLabel(p plans.Plan) string {
	if len(p.Models) > 0 {
		return strings.Join(p.Models, ",")
	}
	if p.Unit == plans.UnitAmount {
		return "不限"
	}
	return "-"
}

// planValidityLabel 打一个包的售卖有效期。没设天数的包不出售。
func planValidityLabel(p plans.Plan) string {
	if p.ValidityDays == nil {
		return "不售卖"
	}
	return fmt.Sprintf("%d 天", *p.ValidityDays)
}

// printCreatedPlan 打印新建出来的额度包。
func printCreatedPlan(w io.Writer, p plans.Plan) {
	sale := ""
	if p.ValidityDays != nil {
		sale = fmt.Sprintf("，售卖 %d 天", *p.ValidityDays)
	}
	_, _ = fmt.Fprintf(w, "已建额度包 %s：%s，%s，可用量 %s，面值 %s，模型 %s%s。\n",
		p.Key, p.Name, unitLabel(p.Unit), p.QuotaTotal, p.FaceValue, planModelsLabel(p), sale)
}

// printPlanDetail 打印一个额度包的详情，一栏一行。
func printPlanDetail(w io.Writer, p plans.Plan) {
	pal := cli.NewPalette(w)
	tw := cli.NewTableWriter(w)
	status := "启用"
	if !p.Enabled {
		status = "停用"
	}
	rows := [][2]string{
		{"KEY", p.Key},
		{"名称", p.Name},
		{"说明", orDash(p.Description)},
		{"资产", string(p.Asset)},
		{"单位", unitLabel(p.Unit)},
		{"可用量", p.QuotaTotal.String()},
		{"面值", p.FaceValue.String()},
		{"模型", planModelsLabel(p)},
		{"售卖有效期", planValidityLabel(p)},
	}
	if p.OwnerUserID != nil {
		rows = append(rows, [2]string{"归属商户", strconv.FormatInt(*p.OwnerUserID, 10)})
	}
	rows = append(rows, [2]string{"状态", status})
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", pal.Bold(row[0]), row[1])
	}
	_ = tw.Flush()
}

// printPlanEnabled 打印启用/停用的结果。
func printPlanEnabled(w io.Writer, p plans.Plan) {
	action := "已启用"
	if !p.Enabled {
		action = "已停用"
	}
	_, _ = fmt.Fprintf(w, "%s额度包 %s。\n", action, p.Key)
}

// printPlanList 打印额度包清单。
func printPlanList(w io.Writer, list []plans.Plan) {
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, cli.EmptyListNote("额度包", "plan add"))
		return
	}

	pal := cli.NewPalette(w)
	tw := cli.NewTableWriter(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		pal.Bold("KEY"), pal.Bold("名称"), pal.Bold("单位"), pal.Bold("可用量"),
		pal.Bold("面值"), pal.Bold("售卖"), pal.Bold("模型"), pal.Bold("状态"))
	for _, p := range list {
		status := "启用"
		if !p.Enabled {
			status = "停用"
		}
		// 「售卖」这一栏回答的是「哪些包能卖」：有天数才卖，空表示不出售。
		// 它与「状态」是两件事——停用的包也可能配了天数，只是暂时不上架。
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			pal.Bold(p.Key), p.Name, unitLabel(p.Unit), p.QuotaTotal.String(),
			p.FaceValue.String(), planValidityLabel(p), planModelsLabel(p), status)
	}
	_ = tw.Flush()
}

// printUserPlans 打印一组订阅。
func printUserPlans(w io.Writer, list []plans.UserPlan) {
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, cli.EmptyListNote("额度包订阅", "plan grant"))
		return
	}

	pal := cli.NewPalette(w)
	tw := cli.NewTableWriter(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		pal.Bold("ID"), pal.Bold("用户"), pal.Bold("额度包"), pal.Bold("单位"),
		pal.Bold("状态"), pal.Bold("到期"), pal.Bold("面值"), pal.Bold("剩余"),
		pal.Bold("发放单号"))
	for _, up := range list {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			pal.Bold(strconv.FormatInt(up.ID, 10)), up.UserID, up.PlanKey,
			unitLabel(up.Unit), string(up.Status),
			formatTimeShort(up.ExpiresAt, "不过期"),
			up.FaceValue.String(), up.FaceValueRemaining.String(), up.Ref)
	}
	_ = tw.Flush()
}

// printAPIKeyPlans 打印一把 key 的额度包名单与展开后的允许集。
func printAPIKeyPlans(w io.Writer, keyID int64, declared []int64, list []plans.UserPlan) {
	if len(declared) == 0 {
		_, _ = fmt.Fprintf(w, "API Key %d 的名单：全部（没有单独限制）。\n", keyID)
	} else {
		ids := make([]string, 0, len(declared))
		for _, id := range declared {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		_, _ = fmt.Fprintf(w, "API Key %d 的名单：%s。\n", keyID, strings.Join(ids, ","))
	}
	printUserPlans(w, list)
}

// printAPIKeyPlansSet 打印名单改完之后的结果。
func printAPIKeyPlansSet(w io.Writer, keyID int64, all bool, list []plans.UserPlan) {
	if all {
		_, _ = fmt.Fprintf(w, "API Key %d 的名单已清空：可以用这个用户名下全部订阅（%d 条）。\n",
			keyID, len(list))
		return
	}
	ids := make([]string, 0, len(list))
	for _, up := range list {
		ids = append(ids, strconv.FormatInt(up.ID, 10))
	}
	_, _ = fmt.Fprintf(w, "API Key %d 的名单已更新：%s。\n", keyID, strings.Join(ids, ","))
}

// printPlanGrant 打印一次发放。
//
// 干跑与真跑共用这一份：动词换成「将发放」，订阅行与面值都来自同一条写路径
// （见 plans.GrantTx）。预览里的订阅 id 是这次事务从序列取到的号——事务回滚后
// 这个号不会再被真跑用到，所以那一个数可能与真跑的不同；其余字段逐字相同。
func printPlanGrant(w io.Writer, owner auth.User, up plans.UserPlan, applied, dryRun bool) {
	if !applied {
		_, _ = fmt.Fprintf(w, "发放单号 %s 已经发过（订阅 %d），本次未改动。\n", up.Ref, up.ID)
		return
	}
	verb := "已发放"
	if dryRun {
		verb = "将发放"
	}
	_, _ = fmt.Fprintf(w,
		"%s订阅 %d（用户 %s，额度包 %s，到期 %s，面值 %s，剩余 %s）。发放单号 %s。\n",
		verb, up.ID, owner.Username, up.PlanKey, formatTimeShort(up.ExpiresAt, "不过期"),
		up.FaceValue, up.FaceValueRemaining, up.Ref)
}

// printPlanVoid 打印一次作废。
func printPlanVoid(w io.Writer, up plans.UserPlan, applied, dryRun bool) {
	if !applied {
		_, _ = fmt.Fprintf(w, "作废单号 %s 已经用过（订阅 %d 现在是 %s），本次未改动。\n",
			up.VoidRef, up.ID, up.Status)
		return
	}
	verb := "已作废"
	if dryRun {
		verb = "将作废"
	}
	_, _ = fmt.Fprintf(w, "%s订阅 %d：状态 %s，剩余面值已转回平台收入。作废单号 %s。\n",
		verb, up.ID, up.Status, up.VoidRef)
}

// printPlanOrder 打印一个用户的有效扣费顺序。
func printPlanOrder(w io.Writer, owner auth.User, order plans.ChargeOrder, source plans.ChargeOrderSource) {
	from := "跟随平台默认"
	if source == plans.ChargeOrderSourceUser {
		from = "用户覆盖"
	}
	_, _ = fmt.Fprintf(w, "用户 %s 的扣费顺序：%s（%s）。\n", owner.Username, order, from)
}

// printPlanOrderSet 打印扣费顺序的修改结果。
func printPlanOrderSet(w io.Writer, owner auth.User, order plans.ChargeOrder, cleared bool) {
	if cleared {
		_, _ = fmt.Fprintf(w, "用户 %s 的扣费顺序已清除：跟随平台默认。\n", owner.Username)
		return
	}
	_, _ = fmt.Fprintf(w, "用户 %s 的扣费顺序已设为 %s。\n", owner.Username, order)
}

// planListDoc 组装 plan list 的机器可读输出。
func planListDoc(list []plans.Plan) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["summary"] = map[string]any{"total": len(list)}
	entries := make([]map[string]any, 0, len(list))
	for _, p := range list {
		entries = append(entries, planEntry(p))
	}
	doc["plans"] = entries
	return doc
}

// planEntry 打一个额度包。
func planEntry(p plans.Plan) map[string]any {
	return map[string]any{
		"key":         p.Key,
		"name":        p.Name,
		"description": p.Description,
		"asset":       string(p.Asset),
		"unit":        string(p.Unit),
		"quota_total": cli.JSONAmount(p.QuotaTotal),
		"face_value":  cli.JSONAmount(p.FaceValue),
		"models":      cli.JSONList(p.Models),
		"enabled":     p.Enabled,
		// nil 会被编码成 null：那一栏的含义是「这个包不出售」。
		"validity_days": p.ValidityDays,
		"owner_user_id": p.OwnerUserID,
	}
}

// planDoc 组装 plan get 的机器可读输出：单个包而不是数组。
func planDoc(p plans.Plan) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["plan"] = planEntry(p)
	return doc
}

// userPlanEntry 打一条订阅。
func userPlanEntry(up plans.UserPlan) map[string]any {
	return map[string]any{
		"id":                   up.ID,
		"user_id":              up.UserID,
		"plan_id":              up.PlanID,
		"plan":                 up.PlanKey,
		"asset":                string(up.Asset),
		"unit":                 string(up.Unit),
		"status":               string(up.Status),
		"starts_at":            cli.JSONTime(up.StartsAt),
		"expires_at":           cli.JSONTimePtr(up.ExpiresAt),
		"quota_total":          cli.JSONAmount(up.QuotaTotal),
		"face_value":           cli.JSONAmount(up.FaceValue),
		"face_value_remaining": cli.JSONAmount(up.FaceValueRemaining),
		"ref":                  up.Ref,
		"void_ref":             cli.NullableString(up.VoidRef),
		"voided_at":            cli.JSONTimePtr(up.VoidedAt),
	}
}

// userPlansDoc 组装 plan subscriptions 的机器可读输出。
func userPlansDoc(owner auth.User, list []plans.UserPlan) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["summary"] = map[string]any{"total": len(list)}
	doc["user"] = map[string]any{"id": owner.ID, "username": owner.Username}
	entries := make([]map[string]any, 0, len(list))
	for _, up := range list {
		entries = append(entries, userPlanEntry(up))
	}
	doc["subscriptions"] = entries
	return doc
}

// apiKeyPlansDoc 组装 plan key show 的机器可读输出。
//
// restricted 说明这把 key 配过名单没有：表格那一档把「没有配过」打成一句
// 「全部」，这一档必须能用布尔分开，而展开后的 subscriptions 两种情况下都
// 是同一份「允许集」。
func apiKeyPlansDoc(keyID int64, declared []int64, list []plans.UserPlan) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["summary"] = map[string]any{"total": len(list)}
	doc["api_key_id"] = keyID
	doc["restricted"] = len(declared) > 0
	doc["plan_ids"] = cli.JSONList(declared)
	entries := make([]map[string]any, 0, len(list))
	for _, up := range list {
		entries = append(entries, userPlanEntry(up))
	}
	doc["subscriptions"] = entries
	return doc
}

// planOrderDoc 组装 plan order show 的机器可读输出。
func planOrderDoc(owner auth.User, order plans.ChargeOrder, source plans.ChargeOrderSource) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["user"] = map[string]any{"id": owner.ID, "username": owner.Username}
	doc["order"] = string(order)
	doc["source"] = string(source)
	return doc
}
