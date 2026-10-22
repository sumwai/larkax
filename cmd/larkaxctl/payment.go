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

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/payment"
	"github.com/sumwai/larkax/internal/payment/fakechannel"
	"github.com/sumwai/larkax/internal/settings"
)

// walletPaymentHelp 是 wallet payment 的两版帮助文本（见 helpText）。
var walletPaymentHelp = helpText{short: walletPaymentShortUsage, long: walletPaymentUsage}

// walletPaymentShortUsage 是 wallet payment 的短版：`-h` / `--help` 与参数写错时打的那一份。
const walletPaymentShortUsage = `larkaxctl wallet payment 走支付通道建单，并查一张单子的现状。

用法：
  larkaxctl wallet payment create --user <ID 或用户名> --amount <金额> --ref <订单号> | --auto-ref
                                  [--asset <资产>] [--notify-url <回调地址>] [--dry-run]
  larkaxctl wallet payment status --order <订单号>

这条通道默认关闭：settings.payment.fake_enabled 为 true 时才挂回调入口、才允许下单。
create 落一张 payment_orders（状态 created），通道通知到达后才入账。
status 同时打本地的状态与通道侧报的状态。create 的 --dry-run 不向通道下单，
只把本地那一行写一遍再回滚。--auto-ref 现生成 MANUAL_<UTC 日期>_<随机段> 的单号并打在
输出里，重跑不会幂等。

更细的边界：larkaxctl wallet payment help。
`

// walletPaymentUsage 是 wallet payment 的长版：`wallet payment help`。
const walletPaymentUsage = `larkaxctl wallet payment 走支付通道建单，并查一张单子的现状。

用法：
  larkaxctl wallet payment create --user <ID 或用户名> --amount <金额> --ref <订单号> | --auto-ref
                                  [--asset <资产>] [--notify-url <回调地址>] [--dry-run]
  larkaxctl wallet payment status --order <订单号>

create 在通道侧建单，并往 payment_orders 写一行（状态 created）。它**不入账**：
钱要等通道把「已支付」的通知发到回调入口才进来。两者的分工是刻意的——下单只是
表达「这次准备付多少」，入账的依据只能是通道确认过的收款。

--ref 是商户单号（订单号），必填，同时是幂等键：同一个单号重跑只会得到同一张
单子，内容不同的重跑报冲突（退出 4）。它也是通道通知里认回这张单子的依据。
它不能带控制字符（理由与 wallet credit 的 --ref 相同）。

--auto-ref 是 --ref 的替代：命令现生成一个 MANUAL_<UTC 日期>_<随机段> 的单号并打进
输出，重跑不会幂等。两个都给、或两个都不给都是参数错（退出 2）。

--amount 必填且为正，--user 收 ID 或用户名。--asset 默认取
settings.billing.default_asset。--notify-url 是通道把结果发回来的地址，本命令
原样交给通道，不解释它。

create 要求用户已经有这个资产的钱包：缺那一行时与 wallet credit 同一档拒绝（退出 4）。
回调入账也要求它存在——账本要同时更新钱包快照。

status 按单号读本地那一行，再问通道这张单子现在是什么状态。假通道没有自己的账本，
它答的就是本地那一行，所以两栏今天恒等；接上真实通道之后才可能出现差异——通知丢了
时本地还停在 created，而通道那边已经付过。通道侧查不到时按「没有这张支付单」处理
（退出 4）。

**这条通道默认关闭**：settings.payment.fake_enabled 为 false 时 create 与 status
都拒绝，回调入口也不会挂上（那条路径回 501）。打开它之前先看那条配置的说明：
假通道的密钥是公开的占位值，只用于本地联调。

**干跑**：create 接受 --dry-run。它与真跑共用同一组前置校验（通道开着、钱包已存在、
参数合法）和同一条落库 SQL，把 payment_orders 那一行在事务里写一遍再回滚，输出那一行
的字段与真跑逐字相等（订单号、通道、资产、金额、状态 created；这一行没有序列号，
单号由 --ref 给定）。它**不向通道下单**：那会在通道侧建出一张真单子，所以预览里没有
支付地址，末尾一行写明这一点。参数错、通道没开、用户没有钱包、同一个单号配不同内容
（冲突）在干跑下同样报错、同样退出码。

更细的约定见 docs/cli-reference.md 的「wallet payment」一节。
`

// paymentSettingKey 是假支付通道的开关，与 internal/settings 的 Registry 一致。
const paymentSettingKey = "payment.fake_enabled"

// paymentDisabledNote 是通道关着时的说明。
//
// 它把「去哪打开」写在错误里：只说「没有启用」的话，读到的人不知道这是配置项
// 还是编译期决定。
const paymentDisabledNote = "假支付通道没有启用：settings.payment.fake_enabled 为 false。" +
	"打开它（仅限本地联调）：larkaxctl settings set --key payment.fake_enabled --value true --by <操作人>"

// paymentLookup 把本地的支付单存储接成假通道的查询来源。
//
// 包成闭包而不是直接传 orders.ChannelOrder：deadmethods 那道门按「带括号的
// 调用点」数导出方法，方法值那种写法它看不见。闭包不改变行为。
func paymentLookup(orders *payment.Store) fakechannel.Lookup {
	return func(ctx context.Context, orderNo string) (payment.ChannelOrder, error) {
		return orders.ChannelOrder(ctx, orderNo)
	}
}

// runWalletPayment 处理 wallet 下的 payment 子命令。
func runWalletPayment(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, walletPaymentShortUsage)
		return usageErr(errors.New("wallet payment 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create":
		return runPaymentCreate(rest)
	case "status":
		return runPaymentStatus(rest)
	case "help":
		printUsage(os.Stdout, walletPaymentHelp.long)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, walletPaymentHelp.short)
		return nil
	default:
		printUsage(os.Stderr, walletPaymentShortUsage)
		return usageErr(fmt.Errorf("未知的 wallet payment 子命令 %q", action))
	}
}

// runPaymentCreate 在通道侧建单并落一张本地支付单。
func runPaymentCreate(args []string) error {
	if err := jsonUnsupported("wallet payment create"); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPaymentUsage)
		return nil
	}

	fs := flag.NewFlagSet("wallet payment create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPaymentShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	asset := fs.String("asset", "", "")
	ref := fs.String("ref", "", "")
	autoRef := autoRefFlag(fs)
	notifyURL := fs.String("notify-url", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"create 不接受位置参数，得到 %q：用户、金额与订单号都用旗标指定；干跑请写 --dry-run", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	orderNo, refErr := resolveOrderRef(*ref, *autoRef, missingRefError(
		"它是商户单号，同时是幂等键——通道按它认回这张单子，重跑靠它认出「已经下过单」"))
	if refErr != nil {
		return refErr
	}
	if keyErr := checkOrderKey("--ref", orderNo); keyErr != nil {
		return keyErr
	}
	amountValue, amountErr := parseCreditAmount(*amount)
	if amountErr != nil {
		return amountErr
	}
	explicitAsset, assetErr := parseAssetFlag(fs, *asset)
	if assetErr != nil {
		return assetErr
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		enabled, enableErr := paymentEnabled(ctx, db)
		if enableErr != nil {
			return enableErr
		}
		if !enabled {
			return errors.New(paymentDisabledNote)
		}

		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return fmt.Errorf("解析 --user 失败: %w", err)
		}
		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, explicitAsset)
		if assetResolveErr != nil {
			return assetResolveErr
		}

		orders := payment.NewStore(db)
		channel := fakechannel.New(fakechannel.WithLookup(paymentLookup(orders)))
		service := payment.NewService(orders, store.New(db), map[string]payment.Channel{
			fakechannel.Name: channel,
		})

		req := payment.CreateOrderRequest{
			OrderNo:   orderNo,
			UserID:    owner.ID,
			Asset:     assetValue,
			Amount:    amountValue,
			Subject:   fmt.Sprintf("充值 %s %s", amountValue, assetValue),
			NotifyURL: strings.TrimSpace(*notifyURL),
		}
		// 干跑不问通道（那会在通道侧建出一张真单子），只在正确的事务里把
		// 本地那一行写一遍再回滚；两条路共用同一组前置校验与同一条落库 SQL。
		var result payment.CreateResult
		if *dryRun {
			preview, previewErr := service.CreatePreview(ctx, fakechannel.Name, req)
			if previewErr != nil {
				return previewErr
			}
			result = preview
		} else {
			created, createErr := service.Create(ctx, fakechannel.Name, req)
			if createErr != nil {
				return createErr
			}
			result = created
		}

		verdict := "已下单。"
		switch {
		case !result.Applied:
			verdict = fmt.Sprintf("订单号 %s 已经下过单，本次未改动。", orderNo)
		case *dryRun:
			verdict = "将下单。"
		}
		printPayment(humanStdout(), paymentReceipt{
			Verdict: verdict,
			Order:   result.Order,
			User:    owner,
			PayURL:  result.Created.PayURL,
		}, *dryRun)
		return nil
	})
}

// runPaymentStatus 读一张支付单的现状。
func runPaymentStatus(args []string) error {
	if err := jsonUnsupported("wallet payment status"); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPaymentUsage)
		return nil
	}

	fs := flag.NewFlagSet("wallet payment status", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPaymentShortUsage) }
	orderNo := fs.String("order", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"status 不接受位置参数，得到 %q：订单号用 --order 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*orderNo) == "" {
		return usageErr(errors.New("缺少 --order：要查的是哪一张支付单"))
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		enabled, enableErr := paymentEnabled(ctx, db)
		if enableErr != nil {
			return enableErr
		}
		if !enabled {
			return errors.New(paymentDisabledNote)
		}

		orders := payment.NewStore(db)
		row, getErr := orders.Get(ctx, strings.TrimSpace(*orderNo))
		if getErr != nil {
			return getErr
		}
		owner, ownerErr := auth.New(db).ResolveUser(ctx, strconv.FormatInt(row.UserID, 10))
		if ownerErr != nil {
			return fmt.Errorf("读支付单 %s 的用户失败: %w", row.OrderNo, ownerErr)
		}

		channel := fakechannel.New(fakechannel.WithLookup(paymentLookup(orders)))
		queried, queryErr := channel.QueryOrder(ctx, row.OrderNo)
		if queryErr != nil {
			return queryErr
		}

		printPayment(humanStdout(), paymentReceipt{
			Verdict:       fmt.Sprintf("支付单 %s 的现状。", row.OrderNo),
			Order:         row,
			User:          owner,
			ChannelStatus: &queried.Status,
		}, false)
		return nil
	})
}

// paymentEnabled 读假支付通道的开关。
func paymentEnabled(ctx context.Context, db *sql.DB) (bool, error) {
	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return false, err
	}
	enabled, boolErr := values.Bool(paymentSettingKey)
	if boolErr != nil {
		return false, boolErr
	}
	return enabled, nil
}

// paymentReceipt 是一张支付单的回显。
type paymentReceipt struct {
	// Verdict 是首行结论（「已下单」「未改动」「现状」）。
	Verdict string
	Order   payment.Order
	User    auth.User
	// PayURL 只有 create 那一条会给。
	PayURL string
	// ChannelStatus 只有 status 那一条会给。
	ChannelStatus *payment.Status
}

// printPayment 打印一张支付单的回显。
//
// 与提现回显同一类（一栏一行）：金额、状态与单据号是人工对账时要逐项核的东西，
// 挤成一行反而难读。
func printPayment(w io.Writer, r paymentReceipt, dryRun bool) {
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintln(w, r.Verdict)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("订单号", r.Order.OrderNo)
	row("用户", r.User.Username)
	row("通道", r.Order.Gateway)
	row("资产", string(r.Order.Asset))
	row("金额", r.Order.Amount.String())
	row("状态", fmt.Sprintf("%s（%s）", r.Order.Status, r.Order.Status.Label()))
	if r.ChannelStatus != nil {
		row("通道状态", fmt.Sprintf("%s（%s）", *r.ChannelStatus, r.ChannelStatus.Label()))
	}
	if r.PayURL != "" {
		row("支付地址", r.PayURL)
	}
	if r.Order.Fee.IsPositive() {
		row("通道手续费", r.Order.Fee.String())
	}
	if r.Order.GatewayTxnID != "" {
		row("通道流水", r.Order.GatewayTxnID)
	}
	if r.Order.PaidAt != nil {
		row("支付时刻", formatTimeValue(*r.Order.PaidAt))
	}
	if !r.Order.CreatedAt.IsZero() {
		row("创建时刻", formatTimeValue(r.Order.CreatedAt))
	}
	if r.Order.Status == payment.StatusPaid {
		row("交易", string(payment.TxnIDForOrder(r.Order.OrderNo)))
	}
	_ = tw.Flush()
	// 干跑的尾注要说全两件事：通道侧没有下单（所以没有支付地址），本地也没有
	// 那一行。只说「没有写进数据库」会让人以为通道那边已经建好了单。
	if dryRun {
		_, _ = fmt.Fprintln(w, "干跑：没有向通道下单，也没有写入数据库。")
	}
}
