package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// walletRefundShortUsage 是 wallet refund 的短版：`-h` / `--help` 与参数写错时打的那一份。
const walletRefundShortUsage = `larkaxctl wallet refund 把用户已经付过的钱退回去。

用法：
  larkaxctl wallet refund --user <ID 或用户名> --amount <金额> --ref <外部单据号>
                          [--asset <资产>] [--memo <原因>]
                          [--merchant <ID 或用户名>] [--merchant-share <金额>]
                          [--upstream-cost <金额>] [--dry-run]

退款走账本写一组 refund 分录：借 user:<用户>，贷 merchant_payable / upstream_cost /
platform_revenue。--amount 必填且为正，是退给用户的金额；--merchant-share 与
--upstream-cost 是这次要冲回的分成与上游成本，不写就都算平台退还的收入，写的时候两项
之和不能超过 --amount。--ref 必填，它是这笔退款的外部单据号，同时是幂等键：同一条命令
重跑两次只退一次。钱包必须已经存在（先 wallet create）。--dry-run 走同一条写路径后回滚，
打印将写入的分录而不动库。

更细的边界：larkaxctl wallet refund help。
`

// walletRefundUsage 是 wallet refund 的长版：`wallet refund help`。
const walletRefundUsage = `larkaxctl wallet refund 把用户已经付过的钱退回去。

用法：
  larkaxctl wallet refund --user <ID 或用户名> --amount <金额> --ref <外部单据号>
                          [--asset <资产>] [--memo <原因>]
                          [--merchant <ID 或用户名>] [--merchant-share <金额>]
                          [--upstream-cost <金额>] [--dry-run]

退款是**消费的镜像**：走账本写一组 refund 分录，借 user:<用户>（他的余额涨回来），
贷方是这次冲回的几项。写成新分录而不是删改原分录：账本只追加，一笔「消费 100、退款
100」与「什么都没发生」在余额上等价，但前者答得出「这笔钱为什么回来了」。

贷方的三项由旗标给，不给就全落在 platform_revenue（平台退还的收入）：

  --merchant <ID 或用户名>   退款要冲回分成的那一方。收 users.id 或用户名，先解析成
                             一个真实存在的用户再记账。
  --merchant-share <金额>    冲回多少分成。与 --merchant 要一起给：只给一方，这一笔
                             退款的商户归属与金额就对不上，命令按参数错拒掉。
  --upstream-cost <金额>     上游退回来的成本。上游按实际用量计费时失败的请求通常不
                             收费；上游已经收过费、事后退回时才写。

两项之和超过 --amount 时报参数错、一笔都不写：这样的一笔会把别的科目冲成负数，而那
不是退款。

--amount 是退给用户的金额，必填且为正。它**可以小于原始消费**（部分退款），但不能大于
——后者不是退款，是一笔要单独说明的调账（用 wallet adjust）。这条上界本命令不查：
账本那一层看不到原始消费，命令行这一路也不去回查那条计费请求，所以**由操作者保证**。

--ref 必填，它是这笔退款对应的**外部单据号**（退款单号、通道的退款流水号），它同时是
幂等键：被确定性地映射成这次交易的 txn_id，于是同一条命令重跑两次只会退一次。同一个
--ref 配不同的金额、用户或对家不是重放，而是冲突（退出 4），一笔都不写。键里不能有
控制字符——它既算交易号又进备注，理由与 credit 那一条相同。**几条命令的键互不相认**：
同一个单据号在 refund 与 credit 上是两笔互不相干的交易。

--memo 是退款原因，可以不写；写的时候不能带控制字符——回显打的是原样的原因，而进分录
的那一份会把它们换成空格，两边就不是同一个串了。这是**可选**的原因：对账时「为什么退」
比「退了多少」更需要解释，所以能写就写上。

「刚退」与「重放」必须分得开（两者都退出 0）：第一次逐条列出写下的分录，重放时打
「幂等键 <ref> 已经退过款，本次未改动」并列出此前写入的分录。

资产默认取 settings 的 billing.default_asset，可以用 --asset 显式指定。用户用 --user
指定（纯数字按 ID 解析，否则按用户名解析）。钱包必须已经存在：先 wallet create。
用户不存在、或用户没有那一行钱包，都退出 4。

--dry-run 走与真跑同一条写路径（同一份校验、同一份幂等判据），把这一笔要写的分录
逐条打出来之后回滚：余额、账本行与钱包快照都不变。参数错与业务拒绝（金额格式、用户
不存在、--ref 缺失、分账之和超过退款额）在干跑下同样报错、同样退出码。

更细的约定见 docs/cli-reference.md 的「wallet」一节。
`

// txnIDForRefund 把退款的幂等键映射成一个交易号。
//
// 与 txnIDForOrder、txnIDForAdjustment 是同一个映射，只换了域分隔前缀——**必须换**：
// 三条命令的幂等键来自同一个物理来源（银行流水号、回单号），共用一个前缀会让「先
// credit 再 refund 同一个单号」算成同一笔交易，于是那笔退款要么被判成冲突（退出 4），
// 要么被判成重放（静默不写），而它本该是一笔新的退款。
func txnIDForRefund(ref string) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-refund:v1", ref)
}

// runWalletRefund 把一笔退款记进账本。
//
// 与 runWalletCredit 同一个形状：把命令行参数变成账本要的输入、调 ledger.Refund 造
// 分录、交给 store.PostTransaction 落库。记账逻辑一行都不在这里——「一笔退款拆到哪
// 几个科目上」是 internal/ledger 已经定好的语义。
func runWalletRefund(args []string) error {
	if err := jsonUnsupported("wallet refund"); err != nil {
		return err
	}

	// `wallet refund help` 给长版，理由与 secret rotate 那一处相同：那一份把
	// 退款拆到哪几个科目上讲全了，短版只是一屏摘要。
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, walletRefundUsage)
		return nil
	}

	fs := flag.NewFlagSet("wallet refund", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletRefundShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	asset := fs.String("asset", "", "")
	ref := fs.String("ref", "", "")
	memo := fs.String("memo", "", "")
	merchant := fs.String("merchant", "", "")
	merchantShare := fs.String("merchant-share", "", "")
	upstreamCost := fs.String("upstream-cost", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"refund 不接受位置参数，得到 %q：用户、金额与单据号都用旗标指定；干跑请写 --dry-run", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	// 外部单据号必填且没有默认值，理由与 credit 那一条相同：默认值会把「操作者
	// 忘了给单号」变成两笔互不相干的退款，而这两笔在账面上都合法。
	refundRef := strings.TrimSpace(*ref)
	if refundRef == "" {
		return usageErr(errors.New(
			"缺少 --ref：它是这笔退款对应的外部单据号，同时是幂等键——重跑同一条命令时靠它认出「已经退过款」"))
	}
	if ctrlErr := checkOrderKey("--ref", refundRef); ctrlErr != nil {
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
	// 原因是可选的：不给就是不解释。给的时候挡掉控制字符，理由见 checkMemo。
	reason := strings.TrimSpace(*memo)
	if reason != "" {
		if memoErr := checkMemo("--memo", reason); memoErr != nil {
			return memoErr
		}
	}
	share, shareErr := parseNonNegativeAmountFlag("--merchant-share", *merchantShare)
	if shareErr != nil {
		return shareErr
	}
	cost, costErr := parseNonNegativeAmountFlag("--upstream-cost", *upstreamCost)
	if costErr != nil {
		return costErr
	}
	// 商户与分成要一起给，缺一边都在连库之前拒掉。缺金额那一侧尤其要在这里挡：
	// ledger.Refund 只在「给了分成金额却没给商户 id」时报错，反过来（给了商户、
	// 没给金额）它什么都不会做——那会是一次**静默丢弃**：操作者以为冲回了那个
	// 商户的分成，账面上一分钱都没落到他身上。
	merchantRef := strings.TrimSpace(*merchant)
	switch {
	case merchantRef != "" && !share.IsPositive():
		return usageErr(errors.New(
			"给了 --merchant 却没给 --merchant-share：这次退款要冲回这个商户多少分成？只给商户的话这一个旗标不产生任何作用"))
	case merchantRef == "" && share.IsPositive():
		return usageErr(errors.New(
			"给了 --merchant-share 却没给 --merchant：这笔分成记在哪个商户名下？先解析成一个真实存在的用户才写得进去"))
	}
	// 和超过退款额也在连库之前拒：Ledger 那一层同样会拒，但那是「其他失败」（退出 1），
	// 而这是命令里的数写错了——改一下命令就能过，退出码该是 2。
	if refunded := share.Add(cost); refunded.Cmp(amountValue) > 0 {
		return usageErr(fmt.Errorf(
			"--merchant-share %s 与 --upstream-cost %s 之和 %s 超过退款额 %s："+
				"退款只能把收过的钱冲回去，多出来的部分是一笔要单独说明的调账（wallet adjust）",
			share, cost, refunded, amountValue))
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		// 变量叫 authStore 而不是 store：本包顶部那个 `store` 是账本包的名字。
		authStore := auth.New(db)

		// 先解析 --user：退款记的是「退给谁」，认不出这个人就没有可退的对象。
		owner, err := authStore.ResolveUser(ctx, *user)
		if err != nil {
			return fmt.Errorf("解析 --user 失败: %w", err)
		}
		// --merchant 也解析成真实用户：账本那一侧没有外键指着 users，一个查不到的
		// 商户照样记得进去，留下的是一笔挂在不存在的用户名下的分成。
		var merchantID int64
		if merchantRef != "" {
			merchantUser, merchantErr := authStore.ResolveUser(ctx, merchantRef)
			if merchantErr != nil {
				return fmt.Errorf("解析 --merchant 失败: %w", merchantErr)
			}
			merchantID = merchantUser.ID
		}

		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, explicitAsset)
		if assetResolveErr != nil {
			return assetResolveErr
		}

		txn, buildErr := ledger.Refund(ledger.RefundParams{
			TxnID:         txnIDForRefund(refundRef),
			UserID:        owner.ID,
			Amount:        amountValue,
			Asset:         assetValue,
			MerchantID:    merchantID,
			MerchantShare: share,
			UpstreamCost:  cost,
			Memo:          reason,
			Ref:           refundRef,
			// BillingRequestID 不给：没有一个把退款挂到那条计费请求上的入口，
			// 而一个没校验过的关联比没有关联更坏——它会把两笔不相干的账挂到一起，
			// 事后按它去查反而查错。见 docs/cli-reference.md 的「wallet」一节。
		})
		if buildErr != nil {
			return buildErr
		}

		if *dryRun {
			return inRolledBackTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
				applied, postErr := store.New(db).PostTransactionTx(ctx, tx, txn)
				if postErr != nil {
					return postErr
				}
				printRefundedWallet(humanStdout(), walletRefund{
					Owner:  owner,
					Asset:  assetValue,
					Amount: amountValue,
					Reason: reason,
					Ref:    refundRef,
					Txn:    txn,
				}, applied, true)
				return nil
			})
		}

		applied, postErr := store.New(db).PostTransaction(ctx, txn)
		if postErr != nil {
			return postErr
		}

		printRefundedWallet(humanStdout(), walletRefund{
			Owner:  owner,
			Asset:  assetValue,
			Amount: amountValue,
			Reason: reason,
			Ref:    refundRef,
			Txn:    txn,
		}, applied, false)
		return nil
	})
}

// parseNonNegativeAmountFlag 解析一个**可以不给**、且不许为负的金额旗标。
//
// 不给（空串）就是零：退款的分成与上游成本都属于「不写就没有这一项」，而零在这
// 两处都是合法的取值（没有分成 / 上游没退钱），所以空串与 0 落成同一个数，没有
// 第三档要说。负数则拒——它不是这两项的取值：冲回的分成为负等于给商户记应收，
// 那不是退款。
//
// 报错时点名是哪一个旗标：wallet refund 上有三个金额旗标，一句「--amount 不是
// 金额」会把人指去改另一个。
func parseNonNegativeAmountFlag(flagName, plain string) (money.Amount, error) {
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" {
		return money.Zero, nil
	}
	amount, err := parseNamedAmountFlag(flagName, trimmed)
	if err != nil {
		return money.Zero, err
	}
	if amount.IsNegative() {
		return money.Zero, usageErr(fmt.Errorf(
			"%s 不能为负数，得到 %s：它是这次要冲回的金额，方向由分录决定", flagName, amount))
	}
	return amount, nil
}

// walletRefund 是一次退款的结果，供渲染路径打给人看。
type walletRefund struct {
	// Owner 是收到退款的用户，也就是余额涨回来的那一方。
	Owner auth.User
	// Asset 与 Amount 是这次退给他的品种与金额（Amount 必为正）。
	Asset  ledger.Asset
	Amount money.Amount
	// Reason 是**原样的** --memo（没给就是空串）。分录里那一份是清洗过的备注，
	// 两者在这里相同（--memo 的控制字符在连库之前就被挡掉了），但口径上它们是
	// 两个来源，所以分开存。
	Reason string
	Ref    string
	Txn    ledger.Transaction
}

// printRefundedWallet 打印退款结果。
//
// 分录逐条列出来，理由与 printCreditedWallet 相同：--amount 被拆到哪几个科目上，
// 只有每条都打出来才核得对——平台那一侧拿了多少是**推出来的**（退款额减去分成与
// 上游成本），只看总额看不出它拆错。
//
// 「刚退」与「重放」必须能分开（LESSONS 第 16 条）：两者都退出 0，而一次静默的
// 空操作与一次真的退了钱不是同一个结果。
func printRefundedWallet(w io.Writer, r walletRefund, applied, dryRun bool) {
	if !applied {
		_, _ = fmt.Fprintf(w, "幂等键 %s 已经退过款，本次未改动。交易 %s。\n",
			r.Ref, r.Txn.TxnID)
		_, _ = fmt.Fprintln(w, "此前写入的分录：")
		printEntryLines(w, r.Txn.Entries)
		if dryRun {
			printDryRunNote(w)
		}
		return
	}

	verb := "已退款给用户"
	if dryRun {
		verb = "将退款给用户"
	}
	_, _ = fmt.Fprintf(w, "%s %s：%s %s，幂等键 %s，交易 %s。\n",
		verb, r.Owner.Username, r.Amount, r.Asset, r.Ref, r.Txn.TxnID)
	// 原因单独打一行，而不是只让它留在分录的备注里：备注要在一屏分录中去找，而
	// 它是这次退款最需要解释的一件事。
	if r.Reason != "" {
		_, _ = fmt.Fprintf(w, "原因：%s\n", r.Reason)
	}
	header := "分录："
	if dryRun {
		header = "将写入的分录："
	}
	_, _ = fmt.Fprintln(w, header)
	printEntryLines(w, r.Txn.Entries)
	if dryRun {
		printDryRunNote(w)
	}
}
