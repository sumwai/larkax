package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
)

// walletPayoutHelp 是 wallet payout 的两版帮助文本（见 helpText）。
var walletPayoutHelp = helpText{short: walletPayoutShortUsage, long: walletPayoutUsage}

// walletPayoutShortUsage 是 wallet payout 的短版：`-h` / `--help` 与参数写错时打的那一份。
const walletPayoutShortUsage = `larkaxctl wallet payout 商户提现：申请、审批与打款。

用法：
  larkaxctl wallet payout request --user <ID 或用户名> --amount <金额> --ref <申请单号>
                                  [--asset <资产>] [--destination <收款信息 JSON>]
  larkaxctl wallet payout approve --id <提现单 ID> --by <操作人> [--note <说明>]
  larkaxctl wallet payout reject  --id <提现单 ID> --by <操作人> [--note <原因>]
  larkaxctl wallet payout paid    --id <提现单 ID> --gateway <通道> [--gateway-ref <凭据号>]

request 把申请额挪进这条单子的待付（钱在申请这一步就冻住了），approve 与 reject 记下
审批结果（reject 顺带把那笔冻结解除），paid 把钱从待付出账。**paid 只放行已批准的单子**：
不存在一条不经过批准就能出钱的路。approve 与 reject 都要 --by，记的是谁批的（撤回冻结
发生在 reject 里，不是 paid）。--amount、--ref 与 --gateway 必填。

更细的边界：larkaxctl wallet payout help。
`

// walletPayoutUsage 是 wallet payout 的长版：`wallet payout help`。
const walletPayoutUsage = `larkaxctl wallet payout 商户提现：申请、审批与打款。

用法：
  larkaxctl wallet payout request --user <ID 或用户名> --amount <金额> --ref <申请单号>
                                  [--asset <资产>] [--destination <收款信息 JSON>]
  larkaxctl wallet payout approve --id <提现单 ID> --by <操作人> [--note <说明>]
  larkaxctl wallet payout reject  --id <提现单 ID> --by <操作人> [--note <原因>]
  larkaxctl wallet payout paid    --id <提现单 ID> --gateway <通道> [--gateway-ref <凭据号>]

一条提现单按状态往前走，四个动作各一条命令：

    pending --批准--> approved --打款--> paid
        |
        +--驳回--> rejected

**分成三步走而不是一步到位**：申请是商户提出的要求，批准是运营认下它，打款是钱真的
出去了。三者是三个时刻、可能三个人做，所以分成三条命令，各写各的记录（谁在什么时候
批的，谁在什么时候打的款）。

request 建一行 payout_requests 并把申请额挪进待付，分录是：

    贷 merchant_payable:<商户>   A    他还能申请的钱少了一笔
    借 payout:<单id>             A    这条单子的待付多了一笔

钱在**申请**这一步就冻住了，不是打款时才冻：这样「有多少钱已经承诺出去了」随时看得见，
而打款只需要核销那条待付。--amount 必填且为正。--ref 是申请单号，同时是幂等键：同一条
命令重跑两次只会建一条单子。--destination 是商户的收款信息（一段 JSON 对象，落进
payout_requests.destination，不给就是空对象）；本命令不解释它的内容，打款通道要哪些字段
是通道那一侧的事。

approve 与 reject 都**不在这一层动钱**，钱在申请时就动了：

    larkaxctl wallet payout approve --id <单id> --by <操作人>
        只记审批人与时刻（reviewed_by / reviewed_at）。审批是记名的动作，所以 --by 必填：
        它与 wallet adjust 的 --by 走同一条解析（ID 或用户名，先解析成一个真实存在的
        用户），而这一条命令与那一条一样不做角色校验——谁批的只能靠它记下来。

    larkaxctl wallet payout reject --id <单id> --by <操作人> [--note <原因>]
        除了记审批结果，还写一组与申请相反的分录把冻结解除：
        借 merchant_payable:<商户>、贷 payout:<单id>。**少写这一组，钱就永远留在那个
        待付科目里**，账本上看起来仍然是平的。驳回只处理还没批准的单子：已经批准的单子
        要撤销是另一件事（这一族没有那一步），它在这里报状态不允许并退出 4。

paid 是唯一一个把钱付出去的入口，**只放行已批准的单子**：

    larkaxctl wallet payout paid --id <单id> --gateway <通道> [--gateway-ref <凭据号>]

    分录是：贷 payout:<单id> A（这条待付核销）、借 payment_gateway:<通道> A（通道替我们
    垫了这笔钱）。没经过批准的单子在这里被拒（退出 4），所以「先批准再打款」不靠操作者
    记得，而是没有第二条路能走到出账。

    --gateway 是钱实际从哪个通道出去，**必填且没有默认值**：通道未知时这笔分录对不上任何
    一份通道账单，而差的那一笔查不出来。（credit 上那个默认值 manual 在这里不适用：那边
    的常见情形就是线下到账，这边的常见情形是走通道。）通道名归一成小写，它是科目后缀。

    --gateway-ref 是通道给的凭据号（转账流水号、通道回单号），**可选**：它进分录备注供人工
    对账，但**不是幂等键**——一张提现单只可能打款成功一次，所以键取单子自己的 id。拿凭据号
    当键的话，同一张单换一个凭据号会被算成一笔新交易，payout:<单id> 被扣成负数，那正是重复
    打款。不给这一项就留一条没有凭据号的备注：线下打款（银行转账、现金）拿不到通道流水号，
    那里要填就只能是编一个，编出来的号进了备注反而更难对账。

**幂等**：四个动作各自幂等。申请、驳回与打款重放时什么都不写，回执打「已经申请过 /
已经驳回过 / 已经打过款，本次未改动」；批准重跑时也不覆盖上一次的审批人与时刻——一次
重跑不该把「谁批的」改成后一个人。两种结果都是退出 0，只能靠那一行字分辨。

--id 给了一个库里没有的提现单时报「没有这条提现单」；单子停在别的状态上时，那一句里
会把当前状态与这一步要求的状态都打出来。两者都退出 4——下一步都是先看清那一行现在
是什么，而不是改一下命令重发。

**这一族不动余额**：提现动的是分成与待付，商户的用户余额（user 科目）不受影响，所以
四条命令都不要求那个用户有钱包行。列出提现单的清单命令还没有。

更细的约定见 docs/cli-reference.md 的「wallet」一节。
`

// 四个动作各有一个域分隔前缀，理由见 txnIDForAdjustment：共用一个前缀会让两个不同
// 的业务动作算成同一笔交易。申请取**外部申请单号**（它是这条命令的幂等键），驳回与
// 打款取**提现单自己的 id**——这两个动作没有对应的外部单据，而「同一张单被驳两次 /
// 被打两次款」正是要幂等挡住的重复。
func txnIDForPayoutRequest(ref string) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-request:v1", ref)
}

func txnIDForPayoutReject(payoutID int64) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-reject:v1", strconv.FormatInt(payoutID, 10))
}

func txnIDForPayoutPaid(payoutID int64) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-paid:v1", strconv.FormatInt(payoutID, 10))
}

// runWalletPayout 处理 wallet 下的 payout 子命令。
//
// 分成四条命令而不是一个「把单子推到下一步」的入口：每一步都有自己的输入（批准要
// 审批人、打款要通道与凭据），而且谁在哪一步做了什么要能在输出与审计里分开看。
func runWalletPayout(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, walletPayoutShortUsage)
		return usageErr(errors.New("wallet payout 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "request":
		return runPayoutRequest(rest)
	case "approve":
		return runPayoutApprove(rest)
	case "reject":
		return runPayoutReject(rest)
	case "paid":
		return runPayoutPaid(rest)
	case "help":
		printUsage(os.Stdout, walletPayoutHelp.long)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, walletPayoutHelp.short)
		return nil
	default:
		printUsage(os.Stderr, walletPayoutShortUsage)
		return usageErr(fmt.Errorf("未知的 wallet payout 子命令 %q", action))
	}
}

// runPayoutRequest 建一条提现单。
//
// 与其余写类命令同一个形状：把命令行参数变成 store 那一层的输入、由它建单并与分录
// 写在同一个事务里。**单子与分录都在 store 里写**（不在命令行里拼两次写操作）：两条
// 事实缺一条，账面就有一个说得通而实际不对的状态（见 store/payout.go 的开头）。
func runPayoutRequest(args []string) error {
	if err := jsonUnsupported("wallet payout request"); err != nil {
		return err
	}
	// 四个子命令（request / approve / reject / paid）都认 `help` 这一个位置参数：
	// 它们都不接受别的位置参数，而这个字不可能是某个旗标的取值，所以认它不会有
	// 歧义；四个里三个认、一个不认的话，`wallet payout request help` 会以「不接受
	// 位置参数」收场，而同一族里相邻的那条命令却打出一屏说明书。
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPayoutHelp.long)
		return nil
	}

	fs := flag.NewFlagSet("wallet payout request", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPayoutShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	asset := fs.String("asset", "", "")
	ref := fs.String("ref", "", "")
	destination := fs.String("destination", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"request 不接受位置参数，得到 %q：商户、金额与申请单号都用旗标指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	requestRef := strings.TrimSpace(*ref)
	if requestRef == "" {
		return usageErr(errors.New(
			"缺少 --ref：它是这张提现单的申请单号，同时是幂等键——重跑同一条命令时靠它认出「已经申请过」"))
	}
	if ctrlErr := checkOrderKey("--ref", requestRef); ctrlErr != nil {
		return ctrlErr
	}
	amountValue, amountErr := parseCreditAmount(*amount)
	if amountErr != nil {
		return amountErr
	}
	explicitAsset, assetErr := parseAssetFlag(fs, *asset)
	if assetErr != nil {
		return assetErr
	}
	// 收款信息在连库之前判一次是否合法 JSON：这一栏要落进 jsonb 列，写错一个括号
	// 由库报出来时那句话是「invalid input syntax for type json」，看不出是哪条命令
	// 的哪个参数。空串表示还没填这一栏，落库时空对象。
	destinationJSON := strings.TrimSpace(*destination)
	if destinationJSON != "" && !json.Valid([]byte(destinationJSON)) {
		return usageErr(fmt.Errorf(
			"--destination 不是合法的 JSON，得到 %q：它是商户的收款信息，要落进 payout_requests.destination（一个 jsonb 列），例如 '{\"bank\":\"…\"}'",
			destinationJSON))
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		owner, err := authStore.ResolveUser(ctx, *user)
		if err != nil {
			return fmt.Errorf("解析 --user 失败: %w", err)
		}
		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, explicitAsset)
		if assetResolveErr != nil {
			return assetResolveErr
		}

		row, applied, requestErr := store.New(db).RequestPayout(ctx, store.RequestPayoutInput{
			TxnID:       txnIDForPayoutRequest(requestRef),
			UserID:      owner.ID,
			Amount:      amountValue,
			Asset:       assetValue,
			Destination: []byte(destinationJSON),
		})
		if requestErr != nil {
			return requestErr
		}
		// 回执要逐条列出这次写的分录。重建一遍而不是让 store 把交易交回来：构造函数
		// 是确定性的（同一批输入给同一批分录），而单子的 id 只有库知道——重放时用的
		// 也是这一行，所以两条路径重建出来的是同一批分录。
		txn, buildErr := ledger.PayoutRequest(ledger.PayoutRequestParams{
			TxnID:    txnIDForPayoutRequest(requestRef),
			PayoutID: row.ID,
			UserID:   row.UserID,
			Amount:   row.Amount,
			Asset:    row.Asset,
		})
		if buildErr != nil {
			return buildErr
		}

		verdict := "已申请提现。"
		if !applied {
			verdict = fmt.Sprintf("申请单号 %s 已经建过单，本次未改动。", requestRef)
		}
		printPayout(humanStdout(), payoutReceipt{
			Verdict:    verdict,
			Payout:     row,
			User:       owner,
			RequestRef: requestRef,
			Txn:        &txn,
		})
		return nil
	})
}

// runPayoutApprove 批准一条待审批的提现单。
func runPayoutApprove(args []string) error {
	return runPayoutReview("approve", args)
}

// runPayoutReject 驳回一条待审批的提现单，并解除那笔冻结。
func runPayoutReject(args []string) error {
	return runPayoutReview("reject", args)
}

// runPayoutReview 是批准与驳回共用的那一段：两条命令的输入与形状完全相同，差别只有
// 落到哪个状态、以及驳回要多写一组解冻分录（后者在 store 那一层按动作分开）。
//
// 合成一个函数而不是抄成两份：这两条命令的旗标、校验与措辞必须逐字一致——一份写成
// --by 必填、另一份忘了判，只有敲到那一条时才发现。动作名进错误信息与回执，于是
// 使用者看到的仍然是「approve」「reject」那两个词。
func runPayoutReview(action string, args []string) error {
	command := "wallet payout " + action
	if err := jsonUnsupported(command); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPayoutHelp.long)
		return nil
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPayoutShortUsage) }
	id := fs.Int64("id", 0, "")
	by := fs.String("by", "", "")
	note := fs.String("note", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"%s 不接受位置参数，得到 %q：提现单与操作人都用旗标指定", action, fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(fmt.Errorf(
			"缺少 --id：要%s的是哪条提现单（payout_requests.id，申请那一步的回执里有）", payoutActionWord(action)))
	}
	if strings.TrimSpace(*by) == "" {
		return usageErr(fmt.Errorf(
			"缺少 --by：%s要记下是谁做的", payoutActionWord(action)))
	}
	// 说明会原样落进 payout_requests.review_note（那一列不过清洗），也会打进回执，
	// 所以挡住控制字符的理由是「回执会原样打出来」，与 --memo 那一条不同。
	reviewNote := strings.TrimSpace(*note)
	if reviewNote != "" {
		if noteErr := checkPayoutNote("--note", reviewNote); noteErr != nil {
			return noteErr
		}
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		// --by 先解析成真实存在的用户：这一栏是「谁批的」唯一的落点，一个查不到的
		// 身份照样写得进去（reviewed_by 虽然有外键，但那是 users 上的行，不是「这个
		// 人真的在场」），留下的是一条查无此人的审计记录。
		operator, err := authStore.ResolveUser(ctx, *by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}

		var (
			row     store.Payout
			applied bool
			rowErr  error
		)
		switch action {
		case "approve":
			row, applied, rowErr = store.New(db).ApprovePayout(ctx, store.ApprovePayoutInput{
				PayoutID: *id,
				Reviewer: operator.ID,
				Note:     reviewNote,
			})
		case "reject":
			row, applied, rowErr = store.New(db).RejectPayout(ctx, store.ReviewPayoutInput{
				PayoutID: *id,
				Reviewer: operator.ID,
				Note:     reviewNote,
				TxnID:    txnIDForPayoutReject(*id),
			})
		}
		if rowErr != nil {
			return rowErr
		}
		// 商户与审批人都从**库里那一行**解析：重放时批的是上一次那个人，回执要回答
		// 的正是「库里记着谁」，而不是这次命令的 --by。
		owner, ownerErr := payoutUser(ctx, authStore, row.UserID, "商户")
		if ownerErr != nil {
			return ownerErr
		}
		reviewer, reviewerErr := payoutReviewer(ctx, authStore, row)
		if reviewerErr != nil && !errors.Is(reviewerErr, errPayoutNotReviewed) {
			return reviewerErr
		}

		// 驳回要连那组解冻分录一起打出来；批准没有分录可打（它不写分录）。
		var txn *ledger.Transaction
		if action == "reject" {
			built, buildErr := ledger.PayoutReject(ledger.PayoutRejectParams{
				TxnID:    txnIDForPayoutReject(*id),
				PayoutID: row.ID,
				UserID:   row.UserID,
				Amount:   row.Amount,
				Asset:    row.Asset,
			})
			if buildErr != nil {
				return buildErr
			}
			txn = &built
		}

		verdict := fmt.Sprintf("已%s提现单 %d。", payoutActionWord(action), row.ID)
		if !applied {
			verdict = fmt.Sprintf("提现单 %d 已经%s过，本次未改动。", row.ID, payoutActionWord(action))
		}
		printPayout(humanStdout(), payoutReceipt{
			Verdict:  verdict,
			Payout:   row,
			User:     owner,
			Reviewer: reviewer,
			Txn:      txn,
		})
		return nil
	})
}

// payoutActionWord 把一个动作名翻成中文动词，用在错误信息与回执里。
//
// 只有「拒绝」与「同意」这两种处置各要一个词，而库里的状态名（rejected /
// approved）说的是结果不是动作——回执那句话说「已驳回提现单 7」，不说「已 rejected」。
func payoutActionWord(action string) string {
	if action == "reject" {
		return "驳回"
	}
	return "批准"
}

// runPayoutPaid 给一条已批准的提现单打款。
func runPayoutPaid(args []string) error {
	if err := jsonUnsupported("wallet payout paid"); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPayoutHelp.long)
		return nil
	}

	fs := flag.NewFlagSet("wallet payout paid", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPayoutShortUsage) }
	id := fs.Int64("id", 0, "")
	gateway := fs.String("gateway", "", "")
	gatewayRef := fs.String("gateway-ref", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"paid 不接受位置参数，得到 %q：提现单与通道都用旗标指定", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New(
			"缺少 --id：要打款的是哪条提现单（payout_requests.id，申请那一步的回执里有）"))
	}
	// 通道必填且没有默认值：见 walletPayoutUsage 里那一段（credit 上那个 manual
	// 默认值在这一侧不适用）。
	if strings.TrimSpace(*gateway) == "" {
		return usageErr(errors.New(
			"缺少 --gateway：钱实际从哪个通道出去？这笔分录要记在 payment_gateway:<通道> 上，通道未知时它对不上任何一份通道账单"))
	}
	gatewayName, gatewayErr := parseGatewayFlag(fs, *gateway)
	if gatewayErr != nil {
		return gatewayErr
	}
	ref := strings.TrimSpace(*gatewayRef)
	if ctrlErr := checkPayoutNote("--gateway-ref", ref); ctrlErr != nil {
		return ctrlErr
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		row, applied, payErr := store.New(db).PayPayout(ctx, store.PayPayoutInput{
			PayoutID: *id,
			TxnID:    txnIDForPayoutPaid(*id),
			Gateway:  gatewayName,
			Ref:      ref,
		})
		if payErr != nil {
			return payErr
		}
		owner, ownerErr := payoutUser(ctx, authStore, row.UserID, "商户")
		if ownerErr != nil {
			return ownerErr
		}
		reviewer, reviewerErr := payoutReviewer(ctx, authStore, row)
		if reviewerErr != nil && !errors.Is(reviewerErr, errPayoutNotReviewed) {
			return reviewerErr
		}
		txn, buildErr := ledger.PayoutPaid(ledger.PayoutPaidParams{
			TxnID:    txnIDForPayoutPaid(*id),
			PayoutID: row.ID,
			UserID:   row.UserID,
			Amount:   row.Amount,
			Asset:    row.Asset,
			Gateway:  gatewayName,
			Ref:      ref,
		})
		if buildErr != nil {
			return buildErr
		}

		verdict := fmt.Sprintf("已打款提现单 %d。", row.ID)
		if !applied {
			verdict = fmt.Sprintf("提现单 %d 已经打过款，本次未改动。", row.ID)
		}
		printPayout(humanStdout(), payoutReceipt{
			Verdict:  verdict,
			Payout:   row,
			User:     owner,
			Reviewer: reviewer,
			Txn:      &txn,
		})
		return nil
	})
}

// payoutUser 按 id 取回一个用户，报错时点名它是这条命令的哪一侧。
//
// 提现单上存的是 merchant_user_id（外键，删不掉），所以查不到只可能是那一行被删过
// 或者号被人改过——两种都要报出来，而不是打一个空名字。
func payoutUser(ctx context.Context, authStore *auth.Store, id int64, what string) (auth.User, error) {
	user, err := authStore.ResolveUser(ctx, strconv.FormatInt(id, 10))
	if err != nil {
		return auth.User{}, fmt.Errorf("读取提现单的%s（用户 %d）失败: %w", what, id, err)
	}
	return user, nil
}

// errPayoutNotReviewed 表示这一行还没有审批人。
//
// 「还没走过审批」是这条链上的一个正常状态，不是失败：调用方拿到它就把回执上
// 审批人那一栏留空，别的错误照常往上抛。用一个哨兵把它说成返回值里的一项，
// 而不是返回 (nil, nil)——(nil, nil) 让「没有审批人」与「查到了、但读不出来」
// 在调用点长得一样，而后者是要报出来的。
var errPayoutNotReviewed = errors.New("这条提现单还没有审批人")

// payoutReviewer 按 reviewed_by 取回审批人；这条单子还没走过审批时返回
// errPayoutNotReviewed。
//
// 从那一行读，不从这次的 --by 拿：重放（或对一张早先批过的单子打款）时批的是上一次
// 那个人，而回执要回答的正是「库里记着谁」。
func payoutReviewer(ctx context.Context, authStore *auth.Store, row store.Payout) (*auth.User, error) {
	if row.ReviewedBy == nil {
		return nil, errPayoutNotReviewed
	}
	user, err := payoutUser(ctx, authStore, *row.ReviewedBy, "审批人")
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// checkPayoutNote 拒绝审批说明里的控制字符。
//
// 与 checkMemo 的理由不同：这一栏**不过清洗**（它原样落进 payout_requests.review_note），
// 所以两边不会不一致。挡住它是因为回执要打这一栏——换行会把「一栏一行」的回执拆开
// （见 docs/cli-output.md 的「加一个清单命令」那一节），ESC 之类会原样进终端。
// 打款凭据号走同一条判据：它同样原样落库、原样进回执与分录备注。
func checkPayoutNote(flagName, note string) error {
	if r, bad := hasControlChar(note); bad {
		return usageErr(fmt.Errorf(
			"%s 不能含控制字符（这里是 %q）：回执会原样打出来，换行会把一栏拆成两行",
			flagName, r))
	}
	return nil
}

// payoutReceipt 是一次提现动作的结果，供渲染路径打给人看。
//
// 收成一个结构体而不是把七八个值平铺给打印函数：四个动作给得出的那几栏不同（申请有
// 单号、审批有说明、打款有凭据），位置参数排到后面读调用点的人得数着看。
type payoutReceipt struct {
	// Verdict 是这次动作落下来的那一句话（「已批准提现单 7。」「提现单 7 已经批准过，
	// 本次未改动。」），由调用方按动作与 applied 给——「刚做完」与「重放」必须分得开
	// （LESSONS 第 16 条），两者都退出 0。
	Verdict string
	// Payout 是这次动作之后**库里的那一行**（重放时是早就存在的那一行）：状态、审批人、
	// 打款时刻与凭据都从它取——库里记的是什么就说什么。
	Payout store.Payout
	// User 是这条单子的商户。
	User auth.User
	// Reviewer 是那一行上记着的审批人（没有就是 nil）。
	Reviewer *auth.User
	// RequestRef 是申请单号，只有申请那一条有：驳回与打款按单子 id 走，键就是单子
	// 自己，再打一遍是同一件事说两遍。
	RequestRef string
	// Txn 是这次动作写的交易；批准不写分录，所以那一档是 nil。
	Txn *ledger.Transaction
}

// printPayout 打印一次提现动作的结果。
//
// 一栏一行（走 tableWriter，与 wallet adjust 的回执同一个形状）：这些是给人核对的字段，
// 拼成一句在窄终端里会自己折行，折出来的行与下一条信息分不开。
//
// **不回收款信息**（payout_requests.destination）：那是商户填的 jsonb 原文，原样打会把
// 里面的控制字符带进终端，清洗之后打又与库里不是同一份。要看它得走读取路径——那一族
// 命令还没有。
func printPayout(w io.Writer, r payoutReceipt) {
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintln(w, r.Verdict)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("提现单", strconv.FormatInt(r.Payout.ID, 10))
	row("商户", r.User.Username)
	row("资产", string(r.Payout.Asset))
	row("金额", r.Payout.Amount.String())
	row("状态", fmt.Sprintf("%s（%s）", r.Payout.Status, r.Payout.Status.Label()))
	if r.RequestRef != "" {
		row("申请单号", r.RequestRef)
	}
	if r.Reviewer != nil {
		row("审批人", r.Reviewer.Username)
	}
	if at := r.Payout.ReviewedAt; at != nil {
		row("审批时刻", formatTimeValue(*at))
	}
	if r.Payout.ReviewNote != "" {
		row("说明", r.Payout.ReviewNote)
	}
	if at := r.Payout.PaidAt; at != nil {
		row("打款时刻", formatTimeValue(*at))
	}
	if r.Payout.PayoutRef != "" {
		row("打款凭据", r.Payout.PayoutRef)
	}
	if r.Txn != nil {
		row("交易", string(r.Txn.TxnID))
	}
	_ = tw.Flush()

	if r.Txn != nil {
		_, _ = fmt.Fprintln(w, "分录：")
		printEntryLines(w, r.Txn.Entries)
	}
}
