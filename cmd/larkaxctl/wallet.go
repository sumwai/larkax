package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/settings"
)

// walletHelp 是 wallet 的两版帮助文本（见 helpText）。
var walletHelp = helpText{short: walletShortUsage, long: walletUsage}

// walletShortUsage 是 wallet 的短版：`-h` / `--help` 与参数写错时打的那一份。
const walletShortUsage = `larkaxctl wallet 管理用户钱包。

用法：
  larkaxctl wallet create --user <ID 或用户名> [--asset <资产>]
  larkaxctl wallet credit --user <ID 或用户名> --amount <金额> --ref <外部单据号>
                          [--asset <资产>] [--gateway <来源>]
  larkaxctl wallet adjust --user <ID 或用户名> --amount <带符号金额> --ref <外部单据号>
                          --memo <原因> --by <操作人>
  larkaxctl wallet list [--user <ID 或用户名>]

list 只读：列出用户 ID、用户名、资产、余额与冻结。
credit 与 adjust 的 --ref 必填，它同时是幂等键：同一条命令重跑两次只记一笔，
换单号才是另一笔。adjust 的 --memo 与 --by 也必填；--amount 带符号，负数是往回扣；
--memo 里不能有控制字符（换行、制表符之类就算，见 help wallet）。
钱包必须已经存在（先 create）；这四条命令都不需要 LARKAX_MASTER_KEYS。

更细的边界：larkaxctl help wallet。
`

// walletUsage 是 wallet 的长版：`larkaxctl help wallet` 与 `wallet help`。
const walletUsage = `larkaxctl wallet 管理用户钱包。

用法：
  larkaxctl wallet create --user <ID 或用户名> [--asset <资产>]
  larkaxctl wallet credit --user <ID 或用户名> --amount <金额> --ref <外部单据号>
                          [--asset <资产>] [--gateway <来源>]
  larkaxctl wallet adjust --user <ID 或用户名> --amount <带符号金额> --ref <外部单据号>
                          --memo <原因> --by <操作人>
  larkaxctl wallet list [--user <ID 或用户名>]

create 给一个已存在的用户开通钱包，余额为 0。它不写任何分录：新开的户在
账本上没有分录，这个用户在 user 科目上的分录之和为 0，与快照里的 0 自洽。

**幂等**：钱包已经存在时什么都不做、照样退出 0。两种结果都退出 0，只能靠
输出的那一行分辨，它们逐字是：

  刚建出来：已为用户 <用户名> 开通 <资产> 钱包，余额 0。
  本来就有：用户 <用户名> 已有 <资产> 钱包，未改动。

脚本要判断时匹配「开通」与「未改动」：前者只出现在刚建出来的那一行，
后者只出现在本来就有那一行。

credit 是运营手工入账：走账本写一组 topup 分录（借 user:<ID>、贷
payment_gateway:<来源>），并在同一个事务里更新 user_wallets.balance。

--ref 是这笔入账对应的**外部单据号**（银行流水号、支付通道的单号），它同时是
幂等键：**必填且没有默认值**。它被确定性地映射成这次交易的 txn_id，于是同一条
命令重跑两次只会入账一次。两种结果都在输出里说清：

  第一次：已入账（并逐条列出写了哪些分录）
  重放：  「已经入过账，本次未改动」，退出 0

同一个 --ref 配不同的金额、用户或通道不是重放，而是冲突：数据库里那笔交易与
这次要写的不是同一批分录，命令退出 4，一笔都不写。换一个单号。

键里不能有控制字符（换行、制表符等）：它既算交易号又进备注，而备注会把控制
字符换成空格，于是两个不同的键会写出两条备注一模一样的入账。

--gateway 是这笔钱从哪个外部来源进来（记进 payment_gateway:<来源> 科目），
默认 manual，表示线下到账（银行转账、现金）。来源名归一成小写：MANUAL 与
manual 是同一个科目。当前不接支付通道的回调，所以来源是操作者说的，
不是系统验证过的。

adjust 是运营手工调账：走账本写一组 adjustment 分录（借/贷 user:<ID>，对家是
platform_revenue），并在同一个事务里更新 user_wallets.balance。修出来的是一笔
**有科目、有原因、有单据号的账本分录**，而不是直接改快照——直接改快照不产生
分录，账本与快照从此对不上，下一轮对账会报差额，报了也查不出出处。

--amount 是**带符号**的：负数是「把记多的钱扣回来」，不是写错了。被调的科目固定
是 user:<ID>，对家固定是 platform_revenue（调账的实质是平台认下这笔差额），
两者都不由旗标指定。它也不碰冻结额：冻结额不在账本里，调它产生不了分录。

--memo 必填，它是这笔调账事后的解释——调账是唯一没有业务依据的分录，不写原因
就成了一笔来路不明的钱。它也不接受控制字符：回执打的是原样的原因，而库里那一份
会把换行、制表符换成空格，两边因此不是同一个串。--by 必填，记的是**谁调的账**
（用户 ID 或用户名）：它先
被解析成一个真实存在的用户，再把用户名与单据号跟在原因后面写进分录的备注，逐字是
「<原因>（操作人 <用户名>，单据 <ref>）」。它只是记名，不是权限门——这条命令与
wallet credit 是同一条面，没有额外的角色校验。单据号进备注有它自己的理由：交易号
是它的哈希，从单据号算得到交易号，反过来算不回来，不落备注就再没有第二条路能把这
张单子对上这笔账。操作人与单据号那一段是**固定后缀、锚在串尾**：原因里可以出现
同样形状的括号，按这个格式反解单号时要从末尾往前认这一段。

--ref 与 credit 的那一个同义（外部单据号 + 幂等键），但**两条命令的键互不相认**：
同一个单据号在调账与入账上是两笔互不相干的交易。键里同样不能有控制字符。同一个
--ref 配不同的金额、用户、原因或操作人不是重放，而是冲突：命令退出 4，一笔都不写。

结果余额会变成负数时拒绝，一笔都不写：库里 user_wallets 的余额有
CHECK (balance >= 0)，而负的用户余额说明某处少扣了钱，它应当被拦住，而不是记成
一笔负数等下一轮对账去发现。

从多少到多少那一对取自分录现算的**权威值**（与 ledger balance --account
user:<ID> 是同一个数），不是 user_wallets 里那个快照：两者不一致时以账本为准，
差额由对账任务比对。

钱包必须已经存在：先 wallet create，再 credit 或 adjust。用户或钱包不存在都退出 4。

资产默认取 settings 的 billing.default_asset（迁移里的默认值是 CNY）。
--asset 可以显式指定，但当前只有 CNY 一种受支持的资产，写别的会被拒绝。
资产品种是钱包主键的一部分：同一个用户可以同时有 CNY 与其他资产的钱包。

用户用 --user 指定，纯数字按 ID 解析，否则按用户名解析。用户不存在时报
「找不到用户」并退出 4——那条错误来自解析用户这一步，与「用户存在但没有
钱包行」是两件事。

create、credit 与 adjust 都只写 user_wallets 与 ledger_entries，不碰 users，也不需要
LARKAX_MASTER_KEYS。

list 是只读的那一条：列出钱包与它的**余额快照**（user_wallets.balance 与
user_wallets.frozen），一栏一个字段：用户 ID、用户名、资产、余额、冻结。
不带 --user 时列出全部钱包，带的时候只列那一个用户；用户不存在时报「找不到用户」
并退出 4，与「用户存在但还没有钱包」是两件事（后者列出来是空的）。

余额那一栏是**快照值**，不是从分录现算的权威值。两者在正常情况下相等，差异由
对账任务比对；要「账本上是多少」用 larkaxctl ledger balance --account user:<ID>。
钱包主键是（用户，资产），所以同一个用户有两种资产时会出现两行。

冻结额是给在飞的请求预扣住的那部分，它**不包含**在余额里——「这个用户还能花
多少」看余额，「有多少正被占着」看冻结额。

**旧名仍可用**：wallet open 等同于 wallet create，credit 上 --idempotency-key 与
--order 都等同于 --ref，-u 是 --user 的简写（四条子命令都认）。它们保留是为了不让
已经在跑的脚本与部署流程坏掉，行为与新名字逐字相同；新写的命令请用新名字。

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -u <ID 或用户名>      同 --user，create、credit、adjust 与 list 都认
  --idempotency-key     同 --ref，只在 credit 上（adjust 没有旧名）。
                        它是这一项上一版的正式写法，已经发布过，删掉就是破坏脚本
  --order               同 --ref，同样只在 credit 上（更早的那一版口语化写法）

更细的约定见 docs/cli-reference.md 的「wallet」一节。
`

// walletTimeout 是这条命令连库与写库的时间上限。
//
// 与 userTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。各写各的常量，是因为它们将来可能各调各的。
const walletTimeout = 30 * time.Second

// manualGateway 是线下到账的通道标识。
//
// 它不是某个真实的支付通道，而是「操作者手工确认了这笔钱到账」这件事在
// 账面上的落点：payment_gateway:manual 的余额与线下流水对，而不是与某个
// 通道的账单对。用 payment_gateway 这一大类而不是 platform_revenue：
// 这笔钱是外部进来的，不是平台挣的。
const manualGateway = "manual"

// runWallet 处理 wallet 子命令。
func runWallet(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, walletShortUsage)
		return usageErr(errors.New("wallet 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create", "open":
		return runWalletCreate(rest)
	case "credit":
		return runWalletCredit(rest)
	case "adjust":
		return runWalletAdjust(rest)
	case "list":
		return runWalletList(rest)
	case "-h", "--help":
		printUsage(os.Stdout, walletShortUsage)
		return nil
	default:
		printUsage(os.Stderr, walletShortUsage)
		return usageErr(fmt.Errorf("未知的 wallet 子命令 %q", action))
	}
}

// runWalletCreate 给一个用户开通钱包。
func runWalletCreate(args []string) error {
	if err := jsonUnsupported("wallet create"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("wallet create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	asset := fs.String("asset", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q：用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}

	explicitAsset, assetErr := parseAssetFlag(fs, *asset)
	if assetErr != nil {
		return assetErr
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		// 先解析用户再开户：外键也会挡下不存在的 id，但那时拿到的是驱动的
		// 原文；这里能说清是哪个标识没对上，也让退出码落到「那一行不存在」。
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}

		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, explicitAsset)
		if assetResolveErr != nil {
			return assetResolveErr
		}

		created, openErr := store.New(db).OpenWallet(ctx, owner.ID, assetValue)
		if openErr != nil {
			return openErr
		}

		printOpenedWallet(humanStdout(), owner, assetValue, created)
		return nil
	})
}

// runWalletCredit 给一个用户手工入账。
//
// 它只做三件事：把命令行参数变成账本要的输入、调 ledger.Topup 造分录、
// 交给 store.PostTransaction 落库。记账逻辑一行都不在这里——见
// internal/ledger 的包注释与 store.PostTransaction 的幂等说明。
func runWalletCredit(args []string) error {
	if err := jsonUnsupported("wallet credit"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("wallet credit", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	asset := fs.String("asset", "", "")
	ref := legacyStringFlag(fs, "ref", "idempotency-key", "")
	stringAlias(fs, ref, "order")
	gateway := fs.String("gateway", manualGateway, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"credit 不接受位置参数，得到 %q：用户、金额与幂等键都用旗标指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	// 外部单据号必填且**没有默认值**：默认值会把「操作者忘了给单号」变成两笔
	// 互不相干的入账，而这两笔在账面上都合法——正是最难事后发现的那种错。
	orderKey := strings.TrimSpace(*ref)
	if orderKey == "" {
		return usageErr(errors.New(
			"缺少 --ref：它是这笔入账对应的外部单据号，同时是幂等键——重跑同一条命令时靠它认出「已经入过账」"))
	}
	if ctrlErr := checkOrderKey("--ref", orderKey); ctrlErr != nil {
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
	gatewayName, gatewayErr := parseGatewayFlag(fs, *gateway)
	if gatewayErr != nil {
		return gatewayErr
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}

		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, explicitAsset)
		if assetResolveErr != nil {
			return assetResolveErr
		}

		txn, buildErr := ledger.Topup(ledger.TopupParams{
			TxnID:  txnIDForOrder(orderKey),
			UserID: owner.ID,
			Amount: amountValue,
			Asset:  assetValue,
			// 手续费这一条命令不接：手工入账在线下已经结清，没有通道费。
			Gateway: gatewayName,
			OrderNo: orderKey,
		})
		if buildErr != nil {
			return buildErr
		}

		applied, postErr := store.New(db).PostTransaction(ctx, txn)
		if postErr != nil {
			return postErr
		}

		printCreditedWallet(humanStdout(), owner, txn, orderKey, applied)
		return nil
	})
}

// runWalletAdjust 给一个用户手工调账。
//
// 与 runWalletCredit 同一个形状：把命令行参数变成账本要的输入、调
// ledger.Adjustment 造分录、交给 store.PostTransaction 落库。记账逻辑一行都不在
// 这里——「一笔调账算哪个科目、对家是谁」是 internal/ledger 已经定好的语义。
func runWalletAdjust(args []string) error {
	if err := jsonUnsupported("wallet adjust"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("wallet adjust", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	amount := fs.String("amount", "", "")
	memo := fs.String("memo", "", "")
	ref := fs.String("ref", "", "")
	by := fs.String("by", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"adjust 不接受位置参数，得到 %q：用户、金额、原因、单据号与操作人都用旗标指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user"))
	}
	adjustRef := strings.TrimSpace(*ref)
	if adjustRef == "" {
		return usageErr(errors.New(
			"缺少 --ref：它是这笔调账对应的外部单据号，同时是幂等键——重跑同一条命令时靠它认出「已经调过账」"))
	}
	if ctrlErr := checkOrderKey("--ref", adjustRef); ctrlErr != nil {
		return ctrlErr
	}
	// 原因在这里判空，而不是留给 ledger.Adjustment 那句「人工调账必须写 memo」：
	// 送进去的备注是「原因 + 操作人」拼出来的，原因空掉时它照样非空，于是那一层
	// 看不出少了什么，落库的会是一条只有操作人、没有原因的备注。
	reason := strings.TrimSpace(*memo)
	if reason == "" {
		return usageErr(errors.New(
			"缺少 --memo：调账是唯一没有业务依据的分录，不写清原因，这笔钱事后解释不了"))
	}
	// 控制字符在这里挡掉，理由见 checkMemo：回执打的是原样的原因，而库里那一份
	// 过了 sanitizeMemo。
	if memoErr := checkMemo("--memo", reason); memoErr != nil {
		return memoErr
	}
	if strings.TrimSpace(*by) == "" {
		return usageErr(errors.New("缺少 --by：这条命令要记下是谁调的账"))
	}
	// 与 credit 走同一个解析，只是不判正负：调账的金额**带符号**，负数是
	// 「把记多的钱扣回来」，零由 ledger.Adjustment 自己拒。
	amountValue, amountErr := parseAmountFlag(*amount)
	if amountErr != nil {
		return amountErr
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		// 变量叫 authStore 而不是 store：本文件顶部那个 `store` 是账本包的名字，
		// 遮掉它会让下面那些 store.New 看起来是同一侧的（与 runWalletList 同）。
		authStore := auth.New(db)

		// 先解析 --by：账本这一侧没有外键指着 users，一个查不到人的身份照样写得
		// 进去，留下的是一条查无此人的审计记录（与 settings set 的 --by 同一条
		// 理由）。两条解析各带旗标名，报错时才说得出该改哪一个。
		operator, err := authStore.ResolveUser(ctx, *by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}
		owner, err := authStore.ResolveUser(ctx, *user)
		if err != nil {
			return fmt.Errorf("解析 --user 失败: %w", err)
		}

		// 这条命令不接 --asset：口径是「只调 user:<id> 这一个科目」，而首先
		// 得知道是哪种资产上那一行。资产随默认值走（同 credit 不给 --asset 时）。
		assetValue, assetResolveErr := resolveWalletAsset(ctx, db, "")
		if assetResolveErr != nil {
			return assetResolveErr
		}

		account := ledger.UserAccount(owner.ID)
		// 余额那一对取自账本现算的权威值，不用 user_wallets 里那个快照：快照是
		// 派生物，两者不一致时账本才是「应该是多少」（见 ledger 的 BalanceOf）。
		// 它也是 ledger balance --account 那条命令读的同一个数。
		before, balanceErr := store.New(db).AccountBalance(ctx, account, assetValue)
		if balanceErr != nil {
			return balanceErr
		}
		// 调后余额先算出来再落库，算不出来就不写：两个数都要进报告，而
		// money.Add 在溢出时夹到 int64 边界——夹出来的数不等于真实的和，打进
		// 报告就是一个错的余额。它要凑出来得让余额与金额之和超出 int64。
		//
		// 这条早退发生在**重放判定之前**（那个判定在 PostTransaction 里面），
		// 于是余额在表示上界或以上时，重放同一条命令拿到的不是「已经调过账」，
		// 而是下面两道门之一：
		//
		//   - 余额正好落在 92233720368.54775807 这个上界：这里报参数错（退出码
		//     2）。条件是重放的那笔金额为**正**——负的装得下，走得到重放判定；
		//   - 余额已被顶过那个上界（账本的和放不进 money.Amount 了）：上面读
		//     余额那一步（AccountBalance）先失败，退出码 1，任何金额、连重放都
		//     到不了重放判定。库里的 numeric(20, 8) 存得下这种和，放不下的是
		//     money，所以这一格是入账自己就能造出来的，连金额都不用挑。
		//
		// 不去修它：命令行这一层要在这两道门之前拿到重放判定，得先按交易号查
		// 一次分录，或干脆先落库再算，两种都比「把余额调到上界以上」这件事本身
		// 更贵。
		after, fits := money.SumChecked(before, amountValue)
		if !fits {
			return usageErr(fmt.Errorf(
				"这次调账会让用户 %s 的 %s 余额超出金额可表示的范围（现值 %s 加上 %s）：拆成两笔，或先确认那个余额是不是对的",
				owner.Username, assetValue, before, amountValue))
		}

		txn, buildErr := ledger.Adjustment(ledger.AdjustmentParams{
			TxnID:   txnIDForAdjustment(adjustRef),
			Account: account,
			Amount:  amountValue,
			Asset:   assetValue,
			// 原因、操作人与单据号写进同一条备注，格式固定成
			// 「（操作人 X，单据 Y）」：账本里没有 actor 列，谁调的只能落在这里；
			// 单据号则是因为交易号是它的哈希——从单据号能算出交易号，反过来算不
			// 回来，不落备注就再没有第二条路能把这个单号对上这笔账。清洗（控制
			// 字符换成空格）由 Adjustment 自己做。
			Memo: fmt.Sprintf("%s（操作人 %s，单据 %s）", reason, operator.Username, adjustRef),
			// 对家不给：留零表示 platform_revenue，那正是人工调账的实质——平台
			// 认下这笔差额。对账修正要的是另一个科目，走的是 reconcile 那条路。
		})
		if buildErr != nil {
			return buildErr
		}

		applied, postErr := store.New(db).PostTransaction(ctx, txn)
		if postErr != nil {
			// 调减到负余额由库里那道 `UPDATE … AND balance + delta >= 0` 拦下
			// （见 store.applyToWallets）。**不在命令行自己判**：判据要拿当前
			// 值算，而重放时算出来的「调后余额」是错的——那一笔早就调过了，再
			// 扣一次当然是负的（PostTransaction 在动钱包之前就先认重放，命令行
			// 这一层看不到那一步）。这里只把措辞换成调账的那一侧：原文说的是
			// 「余额不足以扣」，那是结算路径的说法。
			//
			// ErrFrozenUnderflow 先排掉再按 ErrInsufficientBalance 判，是 store
			// 那条分类规则要求的顺序（它的错误链里同时带着这两个哨兵）。本命令
			// 不写 frozen，所以它今天不会出现在这里——判据按规则写，不按今天是
			// 不是会发生写。
			if errors.Is(postErr, store.ErrInsufficientBalance) &&
				!errors.Is(postErr, store.ErrFrozenUnderflow) {
				return fmt.Errorf(
					"不允许把余额调成负数：这次调账会让用户 %s 的 %s 余额变成负的，先调够再调减: %w",
					owner.Username, assetValue, postErr)
			}
			return postErr
		}

		printAdjustedWallet(humanStdout(), walletAdjustment{
			Owner:    owner,
			Operator: operator,
			Asset:    assetValue,
			Before:   before,
			After:    after,
			Delta:    amountValue,
			Reason:   reason,
			Ref:      adjustRef,
			Txn:      txn,
		}, applied)
		return nil
	})
}

// parseAssetFlag 校验 --asset 的写法并返回显式给的资产（空表示没给）。
//
// 「给了 --asset 但值是空白」与「没给 --asset」是两件事，判据只能是
// 旗标出现过没有（见 providedFlags）：两者的值都是空串。`--asset "$VAR"`
// 而 VAR 没设时静默回落成默认资产，会让脚本拿到一个它没有指定的品种，
// 而资产是钱包主键的一部分——悄悄换一个品种就是悄悄开了另一个钱包。
//
// 显式给的资产在连库之前校验（与 user create / apikey create 同一条约定：
// 一个写错的命令不该先连上生产库再报错）。留空时只能连库之后读 settings，
// 那时再校验（见 resolveWalletAsset）。
func parseAssetFlag(fs *flag.FlagSet, asset string) (ledger.Asset, error) {
	if providedFlags(fs)["asset"] && strings.TrimSpace(asset) == "" {
		return "", usageErr(errors.New(
			"--asset 不能为空：不给这个旗标才表示取 settings 的 billing.default_asset"))
	}
	if strings.TrimSpace(asset) == "" {
		return "", nil
	}
	explicit := ledger.Asset(strings.TrimSpace(asset))
	if !explicit.Valid() {
		return "", usageErr(fmt.Errorf("资产 %q 不受支持（目前只有 %s）", asset, ledger.AssetCNY))
	}
	return explicit, nil
}

// resolveWalletAsset 返回这次要用的资产：显式给的直接用，否则取运行期配置。
func resolveWalletAsset(ctx context.Context, db *sql.DB, explicit ledger.Asset) (ledger.Asset, error) {
	if explicit != "" {
		return explicit, nil
	}
	// 默认资产取自运行期配置，而不是在代码里写死第二个 CNY：
	// 这个键的说明就是「新用户钱包的默认资产品种」，而结算用的资产
	// 也是同一个键（入口读它），两边因此不会各说各的。
	//
	// 这两个变量名不复用 err：调用方的 err 还在作用域里，这里再
	// `err :=` 是遮蔽，govet 的 shadow 检查器会报（同 db.go 那一处）。
	values, loadErr := settings.Load(ctx, settings.NewPgSource(db))
	if loadErr != nil {
		return "", loadErr
	}
	configured, assetErr := values.String("billing.default_asset")
	if assetErr != nil {
		return "", assetErr
	}
	assetValue := ledger.Asset(configured)
	if !assetValue.Valid() {
		// 先给走得通的那条路：--asset 在命令行上就能用。改配置那一句
		// 要带上前提——这个键的 editable 是 false，只允许由迁移写入，
		// `settings set` 会直接拒绝，不写清楚会让人白试一遍。
		return "", fmt.Errorf(
			"settings.billing.default_asset = %q 不是受支持的资产（目前只有 %s）："+
				"用 --asset 指定一个；那个键要改得写一条迁移（它的 editable 为 false，命令行改不了）",
			configured, ledger.AssetCNY)
	}
	return assetValue, nil
}

// hasControlChar 报告 s 里第一个控制字符（C0 与 DEL）。
//
// 两处用它：幂等键与 --memo。两处的**理由不同**（见 checkOrderKey 与 checkMemo），
// 判据是同一条——这两样都会被人读，而控制字符在终端里会被当成动作，不是字。
func hasControlChar(s string) (rune, bool) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return r, true
		}
	}
	return 0, false
}

// checkOrderKey 拒绝幂等键里的控制字符。
//
// 键是**逐字**用来算交易号的，而它同时进备注，备注在 ledger 那边过一道
// sanitizeMemo（把 \r\n\t 换成空格）。两处口径不一致时会出现两笔账面看起来
// 一模一样的入账：`ORD\nX` 与 `ORD X` 是两个键、两个交易号、各自合法入账
// 一次，而库里两条分录的备注都是「充值（单据 ORD X）」——事后按备注核对时，
// 它们看起来是同一笔写了两次。换行键还会把「一行一条」的输出拆成两行。
//
// 归一（把控制字符换成空格）也能消掉这个差，但那样两个不同的键会变成同一个
// 交易号，第二次入账会静默变成一次「已经入过账」的重放——操作者以为记了一笔，
// 钱没动。这里选拒绝：键本来就是外部单据号（转账流水号、回单号），出现控制
// 字符说明它被拼错了，改一下键重发即可。
//
// 判据比 sanitizeMemo 宽（它只处理 \r\n\t）：\x07 这类控制字符会原样进备注，
// 而备注是要被人读的。宽的这一侧只影响这条命令自己的入账，不动别的写路径。
//
// flagName 是这条命令上那个旗标的**规范名**（credit 是 --ref，adjust 也是
// --ref）：报出来的那句话要能照着重敲，点名一个这条命令不认的旧名（例如
// adjust 上没有的 --idempotency-key）等于把人指去一条走不通的路。
func checkOrderKey(flagName, order string) error {
	if r, bad := hasControlChar(order); bad {
		return usageErr(fmt.Errorf(
			"%s 不能含控制字符（这里是 %q）：它同时是幂等键与备注里的单据号，"+
				"带控制字符会让两笔不同的入账在账面上看起来一样",
			flagName, r))
	}
	return nil
}

// checkMemo 拒绝 --memo 里的控制字符。
//
// 原因会**原样**进回执，而落库的那一份由 ledger 的 sanitizeMemo 把 \r\n\t 换成
// 空格——两个串因此不是同一个，报告里写的与账本里记的就对不上。带换行时回执里会
// 多出一行，按 docs/cli-output.md「加一个清单命令」那一节的说法，折出来的行与下
// 一条记录分不开；ESC 之类则会原样进终端。不带控制字符时两边逐字相同，这道判据把
// 两者钉成同一份。
//
// 不选「回执打清洗后的那一份」：那要么在命令行里抄第二份清洗判据（与 ledger 迟早
// 分叉），要么让 ledger 为显示开一个出口。原因本来就不该是多行的，拒掉更省事。
//
// 判据比 sanitizeMemo 宽（它只处理 \r\n\t）：\x07 这类照样会原样进终端。
func checkMemo(flagName, memo string) error {
	if r, bad := hasControlChar(memo); bad {
		return usageErr(fmt.Errorf(
			"%s 不能含控制字符（这里是 %q）：回执打的是原样的原因，落库那一份会把"+
				"控制字符换成空格，两边就不是同一个串了",
			flagName, r))
	}
	return nil
}

// parseCreditAmount 解析 credit 的 --amount。
//
// 与 adjust 共用 parseAmountFlag 那一条判据，差别只有符号：入账只收正数，
// 调账带符号。
func parseCreditAmount(plain string) (money.Amount, error) {
	amount, err := parseAmountFlag(plain)
	if err != nil {
		return money.Zero, err
	}
	if !amount.IsPositive() {
		return money.Zero, usageErr(fmt.Errorf(
			"--amount 必须为正数，得到 %s：入账不带负号，方向由分录决定", amount))
	}
	return amount, nil
}

// parseAmountFlag 解析一个金额旗标：去空白、自己数小数位、交给 money.Parse。
//
// 先自己数小数位，与 parseQuotaLimit 同一条判据：money.Parse 会把多出来的位
// 四舍五入，而钱的金额是「敲进去多少就该记多少」的数——静默改掉最后一位会让账本
// 上的数字与操作者手里的单据对不上。
func parseAmountFlag(plain string) (money.Amount, error) {
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" {
		return money.Zero, usageErr(errors.New("缺少 --amount"))
	}
	// 末尾的 0 不算位数：`1.000000000` 与 `1` 是同一个数，列存得下，没有
	// 任何东西会被舍掉。不先去尾就拒，会让人以为库有问题。
	if _, frac, hasDot := strings.Cut(trimmed, "."); hasDot {
		if kept := strings.TrimRight(frac, "0"); len(kept) > money.Scale {
			return money.Zero, usageErr(fmt.Errorf(
				"--amount 最多 %d 位小数，得到 %q：金额列是 numeric(20, %d)，多出来的位存不进去",
				money.Scale, plain, money.Scale))
		}
	}
	amount, err := money.Parse(trimmed)
	if err != nil {
		return money.Zero, usageErr(fmt.Errorf("--amount 不是金额（例如 12.34）: %w", err))
	}
	return amount, nil
}

// parseGatewayFlag 校验 --gateway 并返回通道标识。
//
// 来源名归一成小写：它是科目后缀，而科目是按字符串比对与汇总的，
// `MANUAL` 与 `manual` 是两个科目——同一个来源写成两种大小写会让
// `payment_gateway` 下面多出一个互不相干的科目，与通道对账时对不上。
//
// **拼错的那一半不在这里挡**（`manul` 照样会开出新科目）：来源名是自由取值，
// 没有一份已知来源的清单可以比。真接上支付通道时它应当换成枚举（那时通道由
// 系统而不是操作者决定），在那之前靠的是 `--help` 里那句「来源是操作者说的」。
func parseGatewayFlag(fs *flag.FlagSet, gateway string) (string, error) {
	if providedFlags(fs)["gateway"] && strings.TrimSpace(gateway) == "" {
		return "", usageErr(errors.New(
			"--gateway 不能为空：不给这个旗标才表示 manual（线下到账）"))
	}
	name := strings.ToLower(strings.TrimSpace(gateway))
	if _, err := ledger.ParseAccount(ledger.PaymentGatewayAccount(name).String()); err != nil {
		return "", usageErr(fmt.Errorf("--gateway %q 不是合法的通道标识: %w", gateway, err))
	}
	return name, nil
}

// txnIDForOrder 把入账的幂等键映射成一个交易号。
func txnIDForOrder(order string) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-credit:v1", order)
}

// txnIDForAdjustment 把调账的幂等键映射成一个交易号。
//
// 与 txnIDForOrder 是同一个映射，只换了域分隔前缀——**必须换**：两条命令的幂等
// 键来自同一个物理来源（银行流水号、回单号），共用一个前缀会让「先 credit 再
// adjust 同一个单号」算成同一笔交易，于是那笔调账要么被判成冲突（退出 4），要么
// 被判成重放（静默不写），而它本该是一笔新的调账。
func txnIDForAdjustment(ref string) ledger.TxnID {
	return txnIDFromKey("larkax:wallet-adjust:v1", ref)
}

// txnIDFromKey 把（用途前缀 + 幂等键）确定性映射成一枚交易号。
//
// 为什么是确定性映射而不是每次生成一个新的随机 uuid：幂等键的用途就是
// 「同一条命令重跑不会重复记账」。随机 id 会让每次重跑都成为一笔新交易，
// 幂等就只能靠「先按备注查一遍」——而备注会清洗、会被人改，不是判据。
// 把键映射成 txn_id 之后，重跑的第二次在写库那一步撞上同一笔交易，
// 由 store.PostTransaction 判定「内容一致 = 重放」。
//
// 域分隔前缀让同一个键在别的用途下算出来的 uuid 不会撞进账本。
// 版本位用 8（RFC 9562 的自定义版本）而不是 5：底层是 SHA-256，
// 写成 v5 会谎称是 SHA-1 名字空间 uuid。ParseTxnID 不检查版本位，
// 这里设它只是为了让生成的值是一枚形态规范的 uuid。
func txnIDFromKey(domain, key string) ledger.TxnID {
	sum := sha256.Sum256([]byte(domain + "\x00" + key))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return ledger.TxnID(out[:])
}

// printOpenedWallet 打印开户结果。
//
// 「刚建出来」与「本来就有」必须能分开：两者都是退出 0，而一次静默的空操作
// 与一次真的建了行在结果上不一样（LESSONS 第 16 条）。这里打给人看，
// 没有需要被脚本接走的单个值，所以只有 stdout 一个流。
func printOpenedWallet(w io.Writer, owner auth.User, asset ledger.Asset, created bool) {
	if created {
		_, _ = fmt.Fprintf(w, "已为用户 %s 开通 %s 钱包，余额 0。\n",
			owner.Username, asset)
		return
	}
	_, _ = fmt.Fprintf(w, "用户 %s 已有 %s 钱包，未改动。\n",
		owner.Username, asset)
}

// printCreditedWallet 打印入账结果。
//
// 分录逐条列出来：运维命令的输出是给人核对的，不是给人 grep 的——「写了
// 多少、写到哪个科目」只有把每条分录的科目与金额都打出来才核得对，只打
// 一个总数的话，方向或科目写错时总数照样是对的。
//
// 「刚入账」与「重放」必须能分开（LESSONS 第 16 条）：两者都退出 0，
// 而一次静默的空操作与一次真的入账不是同一个结果。
func printCreditedWallet(w io.Writer, owner auth.User, txn ledger.Transaction,
	order string, applied bool,
) {
	// 入账金额与资产取自打到 user 科目上的那条分录。从分录上取，而不是把
	// money.Amount 从命令行的 flag 再传一遍：口径只有一个，免得两处算出来
	// 的数不一样。
	var (
		credited money.Amount
		asset    ledger.Asset
	)
	for _, e := range txn.Entries {
		if e.Account == ledger.UserAccount(owner.ID) {
			credited, asset = e.Amount, e.Asset
		}
	}

	if applied {
		_, _ = fmt.Fprintf(w, "已入账：用户 %s %s %s，幂等键 %s，交易 %s。\n",
			owner.Username, credited, asset, order, txn.TxnID)
		_, _ = fmt.Fprintln(w, "分录：")
	} else {
		_, _ = fmt.Fprintf(w, "幂等键 %s 已经入过账，本次未改动。交易 %s。\n",
			order, txn.TxnID)
		_, _ = fmt.Fprintln(w, "此前写入的分录：")
	}

	printEntryLines(w, txn.Entries)
}

// printEntryLines 逐条打印一组分录。
//
// 借方是正、贷方是负（见 ledger.Entry.Amount）；打给人看时换成「借 / 贷 + 正数」，
// 否则两条分录一条正一条负，读起来像两种金额。
//
// 口径只有这一处：wallet credit 与 wallet adjust 都把分录逐条列出来（理由见
// printCreditedWallet），两处各写一遍的话，迟早有一处只改了一半。
func printEntryLines(w io.Writer, entries []ledger.Entry) {
	for _, e := range entries {
		direction, magnitude := "借", e.Amount
		if e.Amount.IsNegative() {
			direction, magnitude = "贷", e.Amount.Neg()
		}
		line := fmt.Sprintf("  %s %-24s %s %s", direction, e.Account, magnitude, e.Asset)
		if e.Memo != "" {
			line += "  " + e.Memo
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// walletAdjustment 是一次人工调账的结果，供渲染路径打给人看。
//
// 收成一个结构体而不是把八个值平铺给打印函数：它们都是同一件事的几个面，位置
// 参数排到第七八个之后，读调用点的人得数着看哪个是哪个；而「谁调的」与「调谁的」
// 是两个不同的人，混在同一串位置参数里读起来容易当成同一个人。
type walletAdjustment struct {
	// Owner 是余额被调的那个用户。
	Owner auth.User
	// Operator 是这次调账的操作人（--by），与 Owner 可能不是同一个人。
	Operator auth.User
	Asset    ledger.Asset
	// Before 与 After 是这次调账前后**账本现算的权威值**。
	Before, After money.Amount
	// Delta 是这次调的**带符号**金额：负数是往回扣。
	Delta money.Amount
	// Reason 是原样的 --memo，不含操作人那一段（备注里那一份由 Adjustment 拼）。
	Reason string
	Ref    string
	Txn    ledger.Transaction
}

// printAdjustedWallet 打印调账结果。
//
// 一栏一行，而不是把几件事拼成一句：操作人、被调用户、余额从多少到多少、原因、
// 单据号与交易号都是要核对的东西，拼成一句在窄终端里会自己折行，折出来的行与
// 下一条信息分不开（见 docs/cli-output.md 的「加一个清单命令」）。
//
// 「刚调完」与「重放」必须能分开（LESSONS 第 16 条）：两者都退出 0，而一次静默的
// 空操作与一次真的调了账不是同一个结果。重放那一档**不打**余额那一对——那两行会
// 让人以为这一次真的动了钱，而它一分钱都没动。
func printAdjustedWallet(w io.Writer, adj walletAdjustment, applied bool) {
	if !applied {
		_, _ = fmt.Fprintf(w, "幂等键 %s 已经调过账，本次未改动。交易 %s。\n",
			adj.Ref, adj.Txn.TxnID)
		_, _ = fmt.Fprintln(w, "此前写入的分录：")
		printEntryLines(w, adj.Txn.Entries)
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(w, "已调账用户 %s\n", adj.Owner.Username)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	// 资产紧挨着余额：余额那个数单独看没有意义，它总是某个资产上的一个数。
	row("资产", string(adj.Asset))
	row("余额", adj.Before.String()+" → "+adj.After.String())
	row("调整", adj.Delta.String())
	row("原因", adj.Reason)
	row("操作人", adj.Operator.Username)
	row("幂等键", adj.Ref)
	row("交易", string(adj.Txn.TxnID))
	_ = tw.Flush()

	_, _ = fmt.Fprintln(w, "分录：")
	printEntryLines(w, adj.Txn.Entries)
}

// walletRow 是一条钱包记录的展示形态：账本那几列加上从 users 取来的用户名。
//
// 收成一个结构体而不是把两个来源分别传给两条渲染路径：用户名与钱包列在两张表
// 上，而「哪一行的用户叫什么」只该解析一次——两条渲染路径各查一次的话，同一份
// 清单在表格与 JSON 里可能对不上号。
type walletRow struct {
	UserID   int64
	Username string
	Asset    ledger.Asset
	Balance  money.Amount
	Frozen   money.Amount
}

// runWalletList 列出钱包与余额快照。
func runWalletList(args []string) error {
	fs := flag.NewFlagSet("wallet list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, walletShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q：按用户过滤用 --user", fs.Arg(0)))
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		// 变量叫 authStore 而不是 store：本文件顶部那个 `store` 是账本包的名字
		// （create 与 credit 都走 store.New）。在这里把包名遮掉，读的人会以为
		// 下面那些 store.xxx 都是同一侧的。
		authStore := auth.New(db)

		// --user 先解析成 ID 再查：让「按用户名查不到」与「按 ID 查不到」都停在
		// 同一步（与 apikey list 同一条约定）。
		var userID *int64
		if *user != "" {
			owner, err := authStore.ResolveUser(ctx, *user)
			if err != nil {
				return err
			}
			userID = &owner.ID
		}

		snapshots, err := store.New(db).Wallets(ctx, userID)
		if err != nil {
			return fmt.Errorf("列出钱包失败: %w", err)
		}

		names, err := userNames(ctx, authStore)
		if err != nil {
			return err
		}

		rows := make([]walletRow, 0, len(snapshots))
		for _, w := range snapshots {
			rows = append(rows, walletRow{
				UserID:   w.UserID,
				Username: names[w.UserID],
				Asset:    w.Asset,
				Balance:  w.Balance,
				Frozen:   w.Frozen,
			})
		}

		// JSON 是另一条渲染路径，不是把上面那张表转成 JSON（见 format.go 开头）。
		if jsonOutput() {
			return emitJSON(os.Stdout, walletListDoc(rows), nil)
		}
		printWalletList(humanStdout(), rows)
		return nil
	})
}

// userNames 取「用户 id → 用户名」，供清单里那一栏用。
//
// 单独查一次而不是在账本那条 SQL 里 JOIN users：用户名属于 auth 那一侧
// （见 ledger/store 的 Wallets）。它**每次都读整张 users**，给了 --user 也一样
// ——筛的是钱包那一侧，这一栏只是按 id 把名字补上，没有「筛了就只读一行」这
// 回事。users 是一张小表，换来的是「谁是用户」只有一处定义。
func userNames(ctx context.Context, store *auth.Store) (map[int64]string, error) {
	users, err := store.ListUsers(ctx, auth.ListUsersFilter{})
	if err != nil {
		return nil, fmt.Errorf("列出用户失败: %w", err)
	}
	names := make(map[int64]string, len(users))
	for _, u := range users {
		names[u.ID] = u.Username
	}
	return names, nil
}

// printWalletList 打一张钱包清单。
//
// 用户 ID 与用户名各占一栏：ID 是别的命令要接过去的那个值
// （`ledger balance --account user:<ID>`），所以它得在清单里看得见，而不是
// 要人再回头去 user list 里查一次。
func printWalletList(w io.Writer, rows []walletRow) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("钱包", "wallet create"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		p.bold("用户 ID"), p.bold("用户名"), p.bold("资产"), p.bold("余额"), p.bold("冻结"))
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.FormatInt(r.UserID, 10)), p.bold(r.Username),
			string(r.Asset), r.Balance.String(), r.Frozen.String())
	}
	_ = tw.Flush()
}

// walletListDoc 组装 wallet list 的机器可读输出。
//
// 与 printWalletList 是同一份数据的另一条渲染路径：那一档把金额按定点小数打给
// 人看，这一档打**字符串**——金额过一遍 float64 会丢精度（见 format.go 的
// jsonAmount）。
func walletListDoc(rows []walletRow) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(rows)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, map[string]any{
			"user_id":  r.UserID,
			"username": nullableString(r.Username),
			"asset":    string(r.Asset),
			"balance":  jsonAmount(r.Balance),
			"frozen":   jsonAmount(r.Frozen),
		})
	}
	doc["wallets"] = entries
	return doc
}
