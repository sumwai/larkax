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
  larkaxctl plan create --key <key> --name <名称> --unit <amount|token|request>
                        [--quota <可用量>] [--face-value <面值>] [--models <模型,...>]
  larkaxctl plan list
  larkaxctl plan get --key <key>
  larkaxctl plan enable|disable --key <key>

用户持有的订阅、扣费顺序与 Key 的额度包名单：larkaxctl user plan、larkaxctl user key。
更细的边界：larkaxctl help plan。
`

// planUsage 是 plan 的长版：`larkaxctl help plan` 与 `plan help`。
const planUsage = `larkaxctl plan 管理额度包（商品定义）。

用法：
  larkaxctl plan create --key <key> --name <名称> --unit <amount|token|request>
                        [--quota <可用量>] [--face-value <面值>]
                        [--models <模型,...>] [--description <说明>]
  larkaxctl plan list
  larkaxctl plan get --key <key>
  larkaxctl plan enable|disable --key <key>

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

更细的约定见 docs/cli-reference.md 的「plan」一节。
`

// planTimeout 是这条命令连库与写库的时间上限（理由同 apiKeyTimeout）。
const planTimeout = 30 * time.Second

func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New("plan 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "create":
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
		return usageErr(fmt.Errorf("未知的 plan 子命令 %q", action))
	}
}

func runPlanCreate(args []string) error {
	// 写类命令在 json 模式下必须在做任何事之前拦下来（见 format.go）。
	if err := jsonUnsupported("plan create"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plan create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	key := fs.String("key", "", "")
	name := fs.String("name", "", "")
	description := fs.String("description", "", "")
	unit := fs.String("unit", "", "")
	quota := fs.String("quota", "", "")
	faceValue := fs.String("face-value", "", "")
	models := fs.String("models", "", "")
	asset := fs.String("asset", string(ledger.AssetCNY), "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if strings.TrimSpace(*key) == "" {
		return usageErr(errors.New("缺少 --key"))
	}
	if strings.TrimSpace(*name) == "" {
		return usageErr(errors.New("缺少 --name"))
	}
	if strings.TrimSpace(*unit) == "" {
		return usageErr(errors.New("缺少 --unit"))
	}
	if err := checkDisplayText("--key", strings.TrimSpace(*key)); err != nil {
		return err
	}
	if err := checkDisplayText("--name", strings.TrimSpace(*name)); err != nil {
		return err
	}
	if err := checkDisplayText("--description", strings.TrimSpace(*description)); err != nil {
		return err
	}
	if err := checkDisplayList("--models", splitOptionalList(*models)); err != nil {
		return err
	}

	assetValue := ledger.Asset(strings.ToUpper(strings.TrimSpace(*asset)))
	if !assetValue.Valid() {
		return usageErr(fmt.Errorf("资产 %q 不受支持（目前只有 %s）", *asset, ledger.AssetCNY))
	}

	in := plans.CreatePlanInput{
		Key:         strings.TrimSpace(*key),
		Name:        strings.TrimSpace(*name),
		Description: strings.TrimSpace(*description),
		Asset:       assetValue,
		Unit:        plans.PlanUnit(strings.TrimSpace(*unit)),
		Models:      splitOptionalList(*models),
	}
	switch in.Unit {
	case plans.UnitAmount, plans.UnitToken, plans.UnitRequest:
	default:
		return usageErr(fmt.Errorf("--unit 只能是 %s、%s 或 %s，得到 %q",
			plans.UnitAmount, plans.UnitToken, plans.UnitRequest, *unit))
	}
	var err error
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

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		created, err := plans.New(db).CreatePlan(ctx, in)
		if err != nil {
			return err
		}
		printCreatedPlan(humanStderr(), created)
		return nil
	})
}

func runPlanList(args []string) error {
	fs := flag.NewFlagSet("plan list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		list, err := plans.New(db).Plans(ctx)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, planListDoc(list), nil)
		}
		printPlanList(humanStdout(), list)
		return nil
	})
}

func runPlanGet(args []string) error {
	fs := flag.NewFlagSet("plan get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	key := fs.String("key", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("get 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if strings.TrimSpace(*key) == "" {
		return usageErr(errors.New("缺少 --key"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		p, err := plans.New(db).PlanByKey(ctx, strings.TrimSpace(*key))
		if err != nil {
			return err
		}
		// 与 list 同一条约定：JSON 是另一条渲染路径，不是把详情表转成 JSON。
		if jsonOutput() {
			return emitJSON(os.Stdout, planDoc(p), nil)
		}
		printPlanDetail(humanStdout(), p)
		return nil
	})
}

func runPlanEnable(args []string, enabled bool) error {
	command := "plan enable"
	if !enabled {
		command = "plan disable"
	}
	if err := jsonUnsupported(command); err != nil {
		return err
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	key := fs.String("key", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("%s 不接受位置参数，得到 %q", strings.TrimPrefix(command, "plan "), fs.Arg(0)))
	}
	if strings.TrimSpace(*key) == "" {
		return usageErr(errors.New("缺少 --key"))
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		p, err := plans.New(db).SetPlanEnabled(ctx, plans.SetPlanEnabledInput{
			Key:     strings.TrimSpace(*key),
			Enabled: enabled,
		})
		if err != nil {
			return err
		}
		printPlanEnabled(humanStderr(), p)
		return nil
	})
}

func runPlanGrant(args []string) error {
	if err := jsonUnsupported("plan grant"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plan grant", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	planKey := fs.String("plan", "", "")
	ref := fs.String("ref", "", "")
	autoRef := autoRefFlag(fs)
	expiresAt := fs.String("expires-at", "", "")
	ttl := fs.String("ttl", "", "")
	forever := fs.Bool("forever", false, "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("grant 不接受位置参数，得到 %q；干跑请写 --dry-run", fs.Arg(0)))
	}
	if *user == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	if strings.TrimSpace(*planKey) == "" {
		return usageErr(errors.New("缺少 --plan"))
	}
	grantRef, refErr := resolveOrderRef(*ref, *autoRef, missingRefError("它是这次发放的幂等键"))
	if refErr != nil {
		return refErr
	}
	if err := checkDisplayText("--ref", grantRef); err != nil {
		return err
	}
	expires, err := resolveExpiry(providedFlags(fs), expiryFlags{
		expiresAt: *expiresAt, ttl: *ttl, forever: *forever,
	}, expiryNow(), func(v string) (time.Time, error) {
		return parseFutureExpiresAt(v, "这份额度发出去就已经到期了")
	})
	if errors.Is(err, errExpiryNotGiven) {
		return usageErr(errors.New("缺少过期时间：--expires-at、--ttl 或 --forever 给一个"))
	}
	if err != nil {
		return usageErr(err)
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
			dryErr := inRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
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
		printPlanGrant(humanStderr(), owner, up, applied, *dryRun)
		return nil
	})
}

func runPlanVoid(args []string) error {
	if err := jsonUnsupported("plan void"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plan void", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	id := fs.Int64("id", 0, "")
	ref := fs.String("ref", "", "")
	autoRef := autoRefFlag(fs)
	status := fs.String("status", string(plans.UserPlanCancelled), "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("void 不接受位置参数，得到 %q；干跑请写 --dry-run", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id：要作废哪一条订阅"))
	}
	voidRef, refErr := resolveOrderRef(*ref, *autoRef, missingRefError("它是这次作废的幂等键"))
	if refErr != nil {
		return refErr
	}
	if err := checkDisplayText("--ref", voidRef); err != nil {
		return err
	}
	final := plans.UserPlanStatus(strings.TrimSpace(*status))
	switch final {
	case plans.UserPlanExpired, plans.UserPlanCancelled:
	default:
		return usageErr(fmt.Errorf("--status 只能是 %s 或 %s，得到 %q",
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
			dryErr := inRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
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
		printPlanVoid(humanStderr(), up, applied, *dryRun)
		return nil
	})
}

func runPlanSubscriptions(args []string) error {
	fs := flag.NewFlagSet("plan subscriptions", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("subscriptions 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *user == "" {
		return usageErr(errors.New("缺少 --user"))
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
		if jsonOutput() {
			return emitJSON(os.Stdout, userPlansDoc(owner, list), nil)
		}
		printUserPlans(humanStdout(), list)
		return nil
	})
}

func runPlanKey(args []string) error {
	rest := args
	if len(rest) == 0 {
		printUsage(os.Stderr, planShortUsage)
		return usageErr(errors.New("plan key 需要 show 或 set"))
	}
	action, rest := rest[0], rest[1:]
	switch action {
	case "show":
		return runPlanKeyShow(rest)
	case "set":
		return runPlanKeySet(rest)
	default:
		return usageErr(fmt.Errorf("未知的 plan key 子命令 %q", action))
	}
}

func runPlanKeyShow(args []string) error {
	fs := flag.NewFlagSet("plan key show", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	id := fs.Int64("id", 0, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("show 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id：要看哪一把 API Key"))
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
		if jsonOutput() {
			return emitJSON(os.Stdout, apiKeyPlansDoc(*id, declared, list), nil)
		}
		printAPIKeyPlans(humanStdout(), *id, declared, list)
		return nil
	})
}

func runPlanKeySet(args []string) error {
	if err := jsonUnsupported("plan key set"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plan key set", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	id := fs.Int64("id", 0, "")
	raw := fs.String("plans", "", "")
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("set 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id：要改哪一把 API Key"))
	}
	given := providedFlags(fs)
	switch {
	case *all && given["plans"]:
		return usageErr(errors.New("--all 与 --plans 不能同时给：一个表示全部，一个表示指定几条"))
	case !*all && !given["plans"]:
		return usageErr(errors.New("要么用 --plans 给出名单，要么用 --all 恢复成全部"))
	}

	var ids []int64
	if !*all {
		var err error
		if ids, err = parseIDList(*raw); err != nil {
			return err
		}
		if len(ids) == 0 {
			return usageErr(errors.New("--plans 给了一个空名单：要恢复成全部用 --all"))
		}
	}

	return withDB(planTimeout, func(ctx context.Context, db *sql.DB) error {
		got, err := plans.New(db).SetAPIKeyPlans(ctx, *id, ids)
		if err != nil {
			return err
		}
		printAPIKeyPlansSet(humanStderr(), *id, *all, got)
		return nil
	})
}

func runPlanOrder(args []string) error {
	rest := args
	if len(rest) == 0 {
		printUsage(os.Stderr, planShortUsage)
		return usageErr(errors.New("plan order 需要 show 或 set"))
	}
	action, rest := rest[0], rest[1:]
	switch action {
	case "show":
		return runPlanOrderShow(rest)
	case "set":
		return runPlanOrderSet(rest)
	default:
		return usageErr(fmt.Errorf("未知的 plan order 子命令 %q", action))
	}
}

func runPlanOrderShow(args []string) error {
	fs := flag.NewFlagSet("plan order show", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("show 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *user == "" {
		return usageErr(errors.New("缺少 --user"))
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
		if jsonOutput() {
			return emitJSON(os.Stdout, planOrderDoc(owner, order, source), nil)
		}
		printPlanOrder(humanStdout(), owner, order, source)
		return nil
	})
}

func runPlanOrderSet(args []string) error {
	if err := jsonUnsupported("plan order set"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("plan order set", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, planShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	order := fs.String("order", "", "")
	clear := fs.Bool("clear", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("set 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *user == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	given := providedFlags(fs)
	switch {
	case *clear && given["order"]:
		return usageErr(errors.New("--clear 与 --order 不能同时给：一个清掉覆盖，一个设值"))
	case !*clear && !given["order"]:
		return usageErr(errors.New("要么用 --order 给出顺序，要么用 --clear 回到平台默认"))
	}
	value := plans.ChargeOrder(strings.TrimSpace(*order))
	if !*clear && !value.Valid() {
		return usageErr(fmt.Errorf("--order 只能是 %s、%s 或 %s，得到 %q",
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
			printPlanOrderSet(humanStderr(), owner, "", true)
			return nil
		}
		if err := svc.SetUserChargeOrder(ctx, owner.ID, value); err != nil {
			return err
		}
		printPlanOrderSet(humanStderr(), owner, value, false)
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
			return nil, usageErr(fmt.Errorf("名单里第 %d 项是空的", i+1))
		}
		id, err := strconv.ParseInt(text, 10, 64)
		if err != nil || id <= 0 {
			return nil, usageErr(fmt.Errorf("名单里第 %d 项 %q 不是正的整数 id", i+1, part))
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

// printCreatedPlan 打印新建出来的额度包。
func printCreatedPlan(w io.Writer, p plans.Plan) {
	_, _ = fmt.Fprintf(w, "已建额度包 %s：%s，%s，可用量 %s，面值 %s，模型 %s。\n",
		p.Key, p.Name, unitLabel(p.Unit), p.QuotaTotal, p.FaceValue, planModelsLabel(p))
}

// printPlanDetail 打印一个额度包的详情，一栏一行。
func printPlanDetail(w io.Writer, p plans.Plan) {
	pal := newPalette(w)
	tw := newTableWriter(w)
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
		{"状态", status},
	}
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", pal.bold(row[0]), row[1])
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
		_, _ = fmt.Fprintln(w, emptyListNote("额度包", "plan create"))
		return
	}

	pal := newPalette(w)
	tw := newTableWriter(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		pal.bold("KEY"), pal.bold("名称"), pal.bold("单位"), pal.bold("可用量"),
		pal.bold("面值"), pal.bold("模型"), pal.bold("状态"))
	for _, p := range list {
		status := "启用"
		if !p.Enabled {
			status = "停用"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			pal.bold(p.Key), p.Name, unitLabel(p.Unit), p.QuotaTotal.String(),
			p.FaceValue.String(), planModelsLabel(p), status)
	}
	_ = tw.Flush()
}

// printUserPlans 打印一组订阅。
func printUserPlans(w io.Writer, list []plans.UserPlan) {
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("额度包订阅", "plan grant"))
		return
	}

	pal := newPalette(w)
	tw := newTableWriter(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		pal.bold("ID"), pal.bold("用户"), pal.bold("额度包"), pal.bold("单位"),
		pal.bold("状态"), pal.bold("到期"), pal.bold("面值"), pal.bold("剩余"),
		pal.bold("发放单号"))
	for _, up := range list {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			pal.bold(strconv.FormatInt(up.ID, 10)), up.UserID, up.PlanKey,
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
	doc := jsonDoc(jsonOK)
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
		"quota_total": jsonAmount(p.QuotaTotal),
		"face_value":  jsonAmount(p.FaceValue),
		"models":      jsonList(p.Models),
		"enabled":     p.Enabled,
	}
}

// planDoc 组装 plan get 的机器可读输出：单个包而不是数组。
func planDoc(p plans.Plan) map[string]any {
	doc := jsonDoc(jsonOK)
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
		"starts_at":            jsonTime(up.StartsAt),
		"expires_at":           jsonTimePtr(up.ExpiresAt),
		"quota_total":          jsonAmount(up.QuotaTotal),
		"face_value":           jsonAmount(up.FaceValue),
		"face_value_remaining": jsonAmount(up.FaceValueRemaining),
		"ref":                  up.Ref,
		"void_ref":             nullableString(up.VoidRef),
		"voided_at":            jsonTimePtr(up.VoidedAt),
	}
}

// userPlansDoc 组装 plan subscriptions 的机器可读输出。
func userPlansDoc(owner auth.User, list []plans.UserPlan) map[string]any {
	doc := jsonDoc(jsonOK)
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
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(list)}
	doc["api_key_id"] = keyID
	doc["restricted"] = len(declared) > 0
	doc["plan_ids"] = jsonList(declared)
	entries := make([]map[string]any, 0, len(list))
	for _, up := range list {
		entries = append(entries, userPlanEntry(up))
	}
	doc["subscriptions"] = entries
	return doc
}

// planOrderDoc 组装 plan order show 的机器可读输出。
func planOrderDoc(owner auth.User, order plans.ChargeOrder, source plans.ChargeOrderSource) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["user"] = map[string]any{"id": owner.ID, "username": owner.Username}
	doc["order"] = string(order)
	doc["source"] = string(source)
	return doc
}
