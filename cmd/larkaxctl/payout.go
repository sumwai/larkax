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
const walletPayoutShortUsage = `larkaxctl wallet payout 商户提现：申请、清算、审批与打款。

用法：
  larkaxctl wallet payout request --user <ID 或用户名> --amount <金额> --ref <申请单号> | --auto-ref
                                  [--asset <资产>] [--destination <收款信息 JSON>]
                                  [--dry-run]
  larkaxctl wallet payout settle  --user <ID 或用户名> --amount <金额> --ref <结算单号> | --auto-ref
                                  --destination <收款信息 JSON> --by <操作人> [--note <说明>]
                                  [--dry-run]
  larkaxctl wallet payout approve --id <提现单 ID> --by <操作人> [--note <说明>]
  larkaxctl wallet payout reject  --id <提现单 ID> --by <操作人> [--note <原因>]
  larkaxctl wallet payout paid    --id <提现单 ID> --gateway <通道> [--gateway-ref <凭据号>]

request 把申请额挪进这条单子的待付（钱在申请这一步就冻住了），settle 是平台主动清算，
一步建单（直接 approved）并记账；approve 与 reject 记下审批结果（reject 顺带把那笔冻结
解除），paid 把钱从待付出账。**paid 只放行已批准的单子**：不存在一条不经过批准就能出钱
的路。settle、approve 与 reject 都要 --by，记的是谁做的（撤回冻结发生在 reject 里，不是
paid）。--amount、--destination（settle）与 --gateway 必填。request 与 settle 的单号
--ref 与 --auto-ref 二选一：--auto-ref 现生成 MANUAL_<UTC 日期>_<随机段> 的单号并打在
输出里，重跑不会幂等。
request 与 settle 都会先核商户的分成余额：分成还没记进账本前会被一律拒掉（退出 1），
那是对的。--destination 必须是一个 JSON 对象，且事后不能改（同一个单号换一份就是冲突，
退出 4）。

更细的边界：larkaxctl wallet payout help。
`

// walletPayoutUsage 是 wallet payout 的长版：`wallet payout help`。
const walletPayoutUsage = `larkaxctl wallet payout 商户提现：申请、清算、审批与打款。

用法：
  larkaxctl wallet payout request --user <ID 或用户名> --amount <金额> --ref <申请单号> | --auto-ref
                                  [--asset <资产>] [--destination <收款信息 JSON>]
                                  [--dry-run]
  larkaxctl wallet payout settle  --user <ID 或用户名> --amount <金额> --ref <结算单号> | --auto-ref
                                  --destination <收款信息 JSON> --by <操作人> [--note <说明>]
                                  [--dry-run]
  larkaxctl wallet payout approve --id <提现单 ID> --by <操作人> [--note <说明>]
  larkaxctl wallet payout reject  --id <提现单 ID> --by <操作人> [--note <原因>]
  larkaxctl wallet payout paid    --id <提现单 ID> --gateway <通道> [--gateway-ref <凭据号>]

一条提现单按状态往前走，动它的动作各一条命令：

    pending --批准--> approved --打款--> paid
        |
        +--驳回--> rejected

settle 不经过这一段的第一半：它建出来的单子**直接是 approved**。

**分成三步走而不是一步到位**：申请是商户提出的要求，批准是运营认下它，打款是钱真的
出去了。三者是三个时刻、可能三个人做，所以分成几条命令，各写各的记录（谁在什么时候
批的，谁在什么时候打的款）。settle 是另一条入口：平台自己决定清算，批准与建单在
同一条命令里落下来，不需要商户先申请。

request 建一行 payout_requests 并把申请额挪进待付，分录是：

    贷 merchant_payable:<商户>   A    他还能申请的钱少了一笔
    借 payout:<单id>             A    这条单子的待付多了一笔

钱在**申请**这一步就冻住了，不是打款时才冻：这样「有多少钱已经承诺出去了」随时看得见，
而打款只需要核销那条待付。

--amount 必填且为正。申请时**先核商户的分成余额**，余额不够就拒掉、一笔都不写（退出 1，
与 wallet adjust 把余额调成负数同一档）：提现只能提平台已经欠他的钱。报错里带着两个数
——当前能提多少、这次要提多少。商户分成的写入侧（consume 时的 merchant_share）今天还没有
调用方落进这条流水线，所以在那里接上之前，申请会被一律拒掉；那是刻意的，不是这条命令坏了。
这条校验只保证「申请时不能提平台还不欠的钱」，**不是**「分成科目永远非负」：退款把分成
冲成负数是允许的（平台多付给了商户、商户欠平台），退款不因此被拒。

--ref 是申请单号，同时是幂等键：同一条命令重跑两次只会建一条单子。它也进 payout 那条
分录的备注，逐字是「提现申请（单据 <ref>）」——交易号是它的哈希，从单据号算得到交易号，
反过来算不回来，不落备注就再没有第二条路把这张申请单对上这笔账。

--auto-ref 是 --ref 的替代：命令现生成一个 MANUAL_<UTC 日期>_<随机段> 的单号并打进
输出，重跑不会幂等。两个都给、或两个都不给都是参数错（退出 2）。

request 的 --destination 是商户的收款信息，必须是**一个 JSON 对象**（形如 {"bank":"…"}），
落进 payout_requests.destination；不给就是空对象（settle 那一档必填，见下）。字符串、
数组与 null 都在连库之前按参数错拒掉（退出 2）：那一列是 jsonb，而打款通道要按字段取值，
一个不是对象的 JSON 取不出任何字段。本命令不解释它的内容，要哪些字段是通道那一侧的事。

收款信息**事后不能改**：这一族没有修改命令。所以同一个申请单号配同一份收款信息是重放，
配了另一份不是重放而是冲突（退出 4），一笔都不写——否则重跑一条旧命令就会把那张单子的
收款账号悄悄换掉。要换收款方式只能驳回后重新申请。

settle 是**平台主动清算**：平台已经决定付这笔钱，于是建单与记账在同一条命令里做完。
单子直接落 approved（reviewed_by 记 --by、reviewed_at 取库里的 now()、review_note 记
--note），之后照旧用 paid 收尾。分录类型是 settle，与商户申请的 payout_request 分开，
对账时「平台清算的」与「商户申请的」在账面上分得开。

    larkaxctl wallet payout settle --user <ID 或用户名> --amount <金额> --ref <结算单号>
                                   --destination <收款信息 JSON> --by <操作人> [--note <说明>]

    --ref 是结算单号，同时是幂等键：同一个单号重跑只会建一张单、记一次账，回显「已经
    清算过，本次未改动」。它也可以换成 --auto-ref（规则与申请那一条相同）。它也进分录
    备注，逐字是「平台清算（单据 <ref>）」，理由与申请那一条相同。同一个单号配不同的
    金额、商户或收款信息不是重放，而是冲突（退出 4），一笔都不写。

    --destination 在这一档**必填**：平台已经决定付了，付到哪里必须当场说清；申请那一档
    可以留空，表示商户可能还没有填。--amount 为正，且不超过商户当前的分成余额，超了退出 1、
    一笔都不写（与 request 同一档）。

approve 与 reject 都**不在这一层动钱**，钱在申请或清算时就动了：

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

**幂等**：五个动作各自幂等。申请、清算、驳回与打款重放时什么都不写，回显打「已经申请过 /
已经清算过 / 已经驳回过 / 已经打过款，本次未改动」；批准重跑时也不覆盖上一次的审批人与
时刻——一次重跑不该把「谁批的」改成后一个人。两种结果都是退出 0，只能靠那一行字分辨。

--id 给了一个库里没有的提现单时报「没有这条提现单」；单子停在别的状态上时，那一句里
会把当前状态与这一步要求的状态都打出来。两者都退出 4——下一步都是先看清那一行现在
是什么，而不是改一下命令重发。

**这一族不动余额**：提现动的是分成与待付，商户的用户余额（user 科目）不受影响，所以
这些命令都不要求那个用户有钱包行。列出提现单的清单命令还没有。

**干跑**：request 与 settle 接受 --dry-run，走与真跑同一条写路径（同一份余额校验、
同一份幂等判据、同一组 SQL），把单子与分录打出来之后回滚，payout_requests 与账本
一条都不落。**除序列号（提现单 id）外，预览与真跑逐字相等**：分录的科目、金额、借贷
方向与余额前后完全相同。提现单 id 是这次事务从序列取到的值，事务回滚不退号，所以真跑
会拿到下一个。分成余额不够、收款信息不合法这类拒绝在干跑下同样报错、同样退出码。
approve / reject / paid 没有这个旗标。

更细的约定见 docs/cli-reference.md 的「wallet」一节。
`

// 五个动作各有一个域分隔前缀，理由见 txnIDForAdjustment：共用一个前缀会让两个不同
// 的业务动作算成同一笔交易。申请与清算取**外部单号**（它是这两条命令的幂等键），驳回
// 与打款取**提现单自己的 id**——这两个动作没有对应的外部单据，而「同一张单被驳两次 /
// 被打两次款」正是要幂等挡住的重复。
func txnIDForPayoutRequest(ref string) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-request:v1", ref)
}

// txnIDForPayoutSettle 与申请那一把互不相认：同一个单号在「商户申请」与「平台清算」
// 上是两笔不相干的交易，共用前缀会把它们算成同一笔。
func txnIDForPayoutSettle(ref string) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-settle:v1", ref)
}

func txnIDForPayoutReject(payoutID int64) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-reject:v1", strconv.FormatInt(payoutID, 10))
}

func txnIDForPayoutPaid(payoutID int64) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-payout-paid:v1", strconv.FormatInt(payoutID, 10))
}

// runWalletPayout 处理 wallet 下的 payout 子命令。
//
// 每个动作一条命令而不是一个「把单子推到下一步」的入口：每一步都有自己的输入（清算
// 要收款信息、批准要审批人、打款要通道与凭据），而且谁在哪一步做了什么要能在输出与
// 审计里分开看。

// walletPayoutActions 是 wallet payout 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var walletPayoutActions = []string{"request", "settle", "approve", "reject", "paid"}

func runWalletPayout(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, walletPayoutShortUsage)
		return usageErr(errors.New("wallet payout 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "request":
		return runPayoutRequest(rest)
	case "settle":
		return runPayoutSettle(rest)
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
	// 五个子命令（request / settle / approve / reject / paid）都认 `help` 这一个
	// 位置参数：它们都不接受别的位置参数，而这个字不可能是某个旗标的取值，所以
	// 认它不会有歧义；五个里四个认、一个不认的话，`wallet payout request help`
	// 会以「不接受位置参数」收场，而同一族里相邻的那条命令却打出一屏说明书。
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPayoutHelp.long)
		return nil
	}

	fs := newFlagSet("wallet payout request", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPayoutShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	asset := fs.String("asset", "", "")
	ref := fs.String("ref", "", "")
	autoRef := autoRefFlag(fs)
	destination := fs.String("destination", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"request 不接受位置参数，得到 %q：商户、金额与申请单号都用旗标指定；干跑请写 --dry-run", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	requestRef, refErr := resolveOrderRef(*ref, *autoRef, missingRefError(
		"它是这张提现单的申请单号，同时是幂等键——重跑同一条命令时靠它认出「已经申请过」"))
	if refErr != nil {
		return refErr
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
	// 收款信息在连库之前判一次：这一栏要落进 jsonb 列，而且必须是**对象**——
	// `"abc"`、`[1]`、`null` 都是合法 JSON，落库看着也像填过了，但打款通道按
	// 字段取值，在它们上面一个字段都取不到。写错一个括号由库报出来时，那句话
	// 是「invalid input syntax for type json」，看不出是哪条命令的哪个参数。
	// 空串表示还没填这一栏，落库时空对象。
	destinationJSON := strings.TrimSpace(*destination)
	if destinationJSON != "" &&
		(!strings.HasPrefix(destinationJSON, "{") || !json.Valid([]byte(destinationJSON))) {
		return usageErr(fmt.Errorf(
			`--destination 必须是合法的 JSON 对象（形如 {"bank":"…"}），得到 %q：`+
				"它要落进 payout_requests.destination（一个 jsonb 列），打款通道按字段取值", destinationJSON))
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

		// 干跑与真跑共用同一个输入与同一条写路径（见 RequestPayoutTx），
		// 差别只有事务边界。
		input := store.RequestPayoutInput{
			TxnID:       txnIDForPayoutRequest(requestRef),
			Ref:         requestRef,
			UserID:      owner.ID,
			Amount:      amountValue,
			Asset:       assetValue,
			Destination: []byte(destinationJSON),
		}
		var (
			row     store.Payout
			applied bool
		)
		if *dryRun {
			dryErr := inRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
				var postErr error
				row, applied, postErr = store.New(db).RequestPayoutTx(ctx, tx, input)
				return postErr
			})
			if dryErr != nil {
				return dryErr
			}
		} else {
			var requestErr error
			row, applied, requestErr = store.New(db).RequestPayout(ctx, input)
			if requestErr != nil {
				return requestErr
			}
		}
		// 回显要逐条列出这次写的分录。重建一遍而不是让 store 把交易交回来：构造函数
		// 是确定性的（同一批输入给同一批分录），而单子的 id 只有库知道——重放时用的
		// 也是这一行，所以两条路径重建出来的是同一批分录。
		txn, buildErr := ledger.PayoutRequest(ledger.PayoutRequestParams{
			TxnID:    txnIDForPayoutRequest(requestRef),
			PayoutID: row.ID,
			UserID:   row.UserID,
			Amount:   row.Amount,
			Asset:    row.Asset,
			Ref:      requestRef,
		})
		if buildErr != nil {
			return buildErr
		}

		verdict := "已申请提现。"
		switch {
		case !applied:
			verdict = fmt.Sprintf("申请单号 %s 已经建过单，本次未改动。", requestRef)
		case *dryRun:
			verdict = "将申请提现。"
		}
		printPayout(humanStdout(), payoutReceipt{
			Verdict:    verdict,
			Payout:     row,
			User:       owner,
			RequestRef: requestRef,
			Txn:        &txn,
		}, *dryRun)
		return nil
	})
}

// runPayoutSettle 是平台主动清算：一步建一条已批准的提现单，并把商户的分成
// 挪进它的待付。
//
// 与 request 分成两条命令而不是加一个 --approved 开关：两者的输入不同（清算要
// 操作人与收款信息，申请不要），落库的也是两件不同的事实（谁发起的），在审计时
// 这两个问题完全不同。
func runPayoutSettle(args []string) error {
	if err := jsonUnsupported("wallet payout settle"); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletPayoutHelp.long)
		return nil
	}

	fs := newFlagSet("wallet payout settle", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletPayoutShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	ref := fs.String("ref", "", "")
	autoRef := autoRefFlag(fs)
	destination := fs.String("destination", "", "")
	by := fs.String("by", "", "")
	note := fs.String("note", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"settle 不接受位置参数，得到 %q：商户、金额、单号与操作人都用旗标指定；干跑请写 --dry-run", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	settleRef, refErr := resolveOrderRef(*ref, *autoRef, missingRefError(
		"它是这次清算的单号，同时是幂等键——重跑同一条命令时靠它认出「已经清算过」"))
	if refErr != nil {
		return refErr
	}
	if ctrlErr := checkOrderKey("--ref", settleRef); ctrlErr != nil {
		return ctrlErr
	}
	amountValue, amountErr := parseCreditAmount(*amount)
	if amountErr != nil {
		return amountErr
	}
	// 收款信息在这一档必填：平台已经决定付了，付到哪里必须当场说清。检查放在
	// 连库之前，理由与 request 那一处相同（坏 JSON 由库报出来的话，看不出是
	// 哪条命令的哪个参数）。
	destinationJSON := strings.TrimSpace(*destination)
	if destinationJSON == "" {
		return usageErr(errors.New(
			"缺少 --destination：平台清算要把钱付到哪里？这一档是平台已经决定付了，收款信息必须当场说清"))
	}
	if !strings.HasPrefix(destinationJSON, "{") || !json.Valid([]byte(destinationJSON)) {
		return usageErr(fmt.Errorf(
			`--destination 必须是合法的 JSON 对象（形如 {"bank":"…"}），得到 %q：`+
				"它要落进 payout_requests.destination（一个 jsonb 列），打款通道按字段取值", destinationJSON))
	}
	if strings.TrimSpace(*by) == "" {
		return usageErr(errors.New(
			"缺少 --by：清算要记下是谁做的（这一栏落进 reviewed_by）"))
	}
	settleNote := strings.TrimSpace(*note)
	if settleNote != "" {
		if noteErr := checkPayoutNote("--note", settleNote); noteErr != nil {
			return noteErr
		}
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		owner, err := authStore.ResolveUser(ctx, *user)
		if err != nil {
			return fmt.Errorf("解析 --user 失败: %w", err)
		}
		// --by 先解析成真实存在的用户：reviewed_by 上有外键，但那只保证这一行
		// 在 users 里，不保证「这个人真的在场」（理由同审批那一条）。
		operator, err := authStore.ResolveUser(ctx, *by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}
		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, "")
		if assetResolveErr != nil {
			return assetResolveErr
		}

		// 干跑与真跑共用同一个输入与同一条写路径（见 SettlePayoutTx），
		// 差别只有事务边界。
		input := store.SettlePayoutInput{
			TxnID:       txnIDForPayoutSettle(settleRef),
			Ref:         settleRef,
			UserID:      owner.ID,
			Amount:      amountValue,
			Asset:       assetValue,
			Destination: []byte(destinationJSON),
			Reviewer:    operator.ID,
			Note:        settleNote,
		}
		var (
			row     store.Payout
			applied bool
		)
		if *dryRun {
			dryErr := inRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
				var postErr error
				row, applied, postErr = store.New(db).SettlePayoutTx(ctx, tx, input)
				return postErr
			})
			if dryErr != nil {
				return dryErr
			}
		} else {
			var settleErr error
			row, applied, settleErr = store.New(db).SettlePayout(ctx, input)
			if settleErr != nil {
				return settleErr
			}
		}
		// 回显上的审批人从**库里那一行**取，不从这次的 --by 拿：重放时记着的
		// 是上一次那个人，回显要回答的正是「库里记着谁」（同审批与打款那两条）。
		reviewer, reviewerErr := payoutReviewer(ctx, authStore, row)
		if reviewerErr != nil && !errors.Is(reviewerErr, errPayoutNotReviewed) {
			return reviewerErr
		}
		// 回显要逐条列出这次写的分录。重建一遍而不是让 store 把交易交回来：
		// 构造函数是确定性的，而单子的 id 只有库知道——重放时用的也是这一行。
		txn, buildErr := ledger.Settle(ledger.SettleParams{
			TxnID:    txnIDForPayoutSettle(settleRef),
			UserID:   row.UserID,
			Amount:   row.Amount,
			Asset:    row.Asset,
			PayoutID: row.ID,
			Ref:      settleRef,
		})
		if buildErr != nil {
			return buildErr
		}

		verdict := fmt.Sprintf("已清算提现单 %d。", row.ID)
		switch {
		case !applied:
			verdict = fmt.Sprintf("结算单号 %s 已经清算过，本次未改动。", settleRef)
		case *dryRun:
			verdict = fmt.Sprintf("将清算提现单 %d。", row.ID)
		}
		printPayout(humanStdout(), payoutReceipt{
			Verdict:   verdict,
			Payout:    row,
			User:      owner,
			Reviewer:  reviewer,
			SettleRef: settleRef,
			Txn:       &txn,
		}, *dryRun)
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
// --by 必填、另一份忘了判，只有敲到那一条时才发现。动作名进错误信息与回显，于是
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

	fs := newFlagSet(command, flag.ContinueOnError)
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
			"缺少 --id：要%s的是哪条提现单（payout_requests.id，申请那一步的回显里有）", payoutActionWord(action)))
	}
	if strings.TrimSpace(*by) == "" {
		return usageErr(fmt.Errorf(
			"缺少 --by：%s要记下是谁做的", payoutActionWord(action)))
	}
	// 说明会原样落进 payout_requests.review_note（那一列不过清洗），也会打进回显，
	// 所以挡住控制字符的理由是「回显会原样打出来」，与 --memo 那一条不同。
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
		// 商户与审批人都从**库里那一行**解析：重放时批的是上一次那个人，回显要回答
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
		}, false)
		return nil
	})
}

// payoutActionWord 把一个动作名翻成中文动词，用在错误信息与回显里。
//
// 只有「拒绝」与「同意」这两种处置各要一个词，而库里的状态名（rejected /
// approved）说的是结果不是动作——回显那句话说「已驳回提现单 7」，不说「已 rejected」。
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

	fs := newFlagSet("wallet payout paid", flag.ContinueOnError)
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
			"缺少 --id：要打款的是哪条提现单（payout_requests.id，申请那一步的回显里有）"))
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
		}, false)
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
// 「还没走过审批」是这条链上的一个正常状态，不是失败：调用方拿到它就把回显上
// 审批人那一栏留空，别的错误照常往上抛。用一个哨兵把它说成返回值里的一项，
// 而不是返回 (nil, nil)——(nil, nil) 让「没有审批人」与「查到了、但读不出来」
// 在调用点长得一样，而后者是要报出来的。
var errPayoutNotReviewed = errors.New("这条提现单还没有审批人")

// payoutReviewer 按 reviewed_by 取回审批人；这条单子还没走过审批时返回
// errPayoutNotReviewed。
//
// 从那一行读，不从这次的 --by 拿：重放（或对一张早先批过的单子打款）时批的是上一次
// 那个人，而回显要回答的正是「库里记着谁」。
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
// 所以两边不会不一致。挡住它是因为回显要打这一栏——换行会把「一栏一行」的回显拆开
// （见 docs/cli-output.md 的「加一个清单命令」那一节），ESC 之类会原样进终端。
// 打款凭据号走同一条判据：它同样原样落库、原样进回显与分录备注。
func checkPayoutNote(flagName, note string) error {
	if r, bad := hasControlChar(note); bad {
		return usageErr(fmt.Errorf(
			"%s 不能含控制字符（这里是 %q）：回显会原样打出来，换行会把一栏拆成两行",
			flagName, r))
	}
	return nil
}

// payoutReceipt 是一次提现动作的结果，供渲染路径打给人看。
//
// 收成一个结构体而不是把七八个值平铺给打印函数：五个动作给得出的那几栏不同（申请有
// 单号、清算多一栏结算单号、审批有说明、打款有凭据），位置参数排到后面读调用点的人
// 得数着看。
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
	// SettleRef 是结算单号，只有 settle 那一条有（理由同 RequestRef）。
	SettleRef string
	// Txn 是这次动作写的交易；批准不写分录，所以那一档是 nil。
	Txn *ledger.Transaction
}

// printPayout 打印一次提现动作的结果。
//
// 一栏一行（走 tableWriter，与 wallet adjust 的回显同一个形状）：这些是给人核对的字段，
// 拼成一句在窄终端里会自己折行，折出来的行与下一条信息分不开。
//
// **不回收款信息**（payout_requests.destination）：那是商户填的 jsonb 原文，原样打会把
// 里面的控制字符带进终端，清洗之后打又与库里不是同一份。要看它得走读取路径——那一族
// 命令还没有。
func printPayout(w io.Writer, r payoutReceipt, dryRun bool) {
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
	if r.SettleRef != "" {
		row("结算单号", r.SettleRef)
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
		header := "分录："
		if dryRun {
			header = "将写入的分录："
		}
		_, _ = fmt.Fprintln(w, header)
		printEntryLines(w, r.Txn.Entries)
	}
	if dryRun {
		printDryRunNote(w)
	}
}
