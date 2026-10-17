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
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/settings"
)

const walletUsage = `larkaxctl wallet 管理用户钱包。

用法：
  larkaxctl wallet create --user <ID 或用户名> [--asset <资产>]
  larkaxctl wallet credit --user <ID 或用户名> --amount <金额> --ref <外部单据号>
                          [--asset <资产>] [--gateway <来源>]

create 给一个已存在的用户开通钱包，余额为 0。它不写任何分录：新开的户在
账本上没有分录，这个用户在 user 科目上的分录之和为 0，与快照里的 0 自洽。
带初始金额意味着这次开户同时是一次充值，而那属于充值入口——用 credit。

**幂等**：钱包已经存在时什么都不做、照样退出 0。两种结果都退出 0，只能靠
输出的那一行分辨，它们逐字是：

  刚建出来：已为用户 <用户名> 开通 <资产> 钱包，余额 0。
  本来就有：用户 <用户名> 已有 <资产> 钱包，未改动。

脚本要判断时匹配「开通」与「未改动」：前者只出现在刚建出来的那一行，
后者只出现在本来就有那一行。

credit 是运营手工入账：走账本写一组 topup 分录（借 user:<ID>、贷
payment_gateway:<来源>），并在同一个事务里更新 user_wallets.balance。
它不绕过账本直接改余额——那样会让钱包快照与分录对不上，下一轮对账会报。

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

钱包必须已经存在：先 wallet create，再 credit。用户或钱包不存在都退出 4。

资产默认取 settings 的 billing.default_asset（迁移里的默认值是 CNY）。
--asset 可以显式指定，但当前只有 CNY 一种受支持的资产，写别的会被拒绝。
资产品种是钱包主键的一部分：同一个用户可以同时有 CNY 与其他资产的钱包。

用户用 --user 指定，纯数字按 ID 解析，否则按用户名解析。用户不存在时报
「找不到用户」并退出 4——那条错误来自解析用户这一步，与「用户存在但没有
钱包行」是两件事。

这两条命令只写 user_wallets 与 ledger_entries，不碰 users，也不需要
LARKAX_MASTER_KEYS。

**旧名仍可用**：wallet open 等同于 wallet create，--idempotency-key 与 --order
都等同于 --ref，-u 是 --user 的简写。它们保留是为了不让已经在跑的脚本与部署
流程坏掉，行为与新名字逐字相同；新写的命令请用新名字。

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -u <ID 或用户名>      同 --user，create 与 credit 都认
  --idempotency-key     同 --ref。它是这一项上一版的正式写法，已经发布过，
                        删掉就是破坏脚本
  --order               同 --ref（更早的那一版口语化写法）
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
		fmt.Fprint(os.Stderr, walletUsage)
		return usageErr(errors.New("wallet 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create", "open":
		return runWalletCreate(rest)
	case "credit":
		return runWalletCredit(rest)
	case "help", "-h", "--help":
		fmt.Print(walletUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, walletUsage)
		return usageErr(fmt.Errorf("未知的 wallet 子命令 %q", action))
	}
}

// runWalletCreate 给一个用户开通钱包。
func runWalletCreate(args []string) error {
	if err := jsonUnsupported("wallet create"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("wallet create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, walletUsage) }
	user := fs.String("user", "", "钱包归属的用户：用户 ID 或用户名")
	stringAlias(fs, user, "u", sameAs("user"))
	asset := fs.String("asset", "", "资产品种；留空表示取 settings 的 billing.default_asset")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q：用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		fs.Usage()
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
	fs.Usage = func() { fmt.Fprint(os.Stderr, walletUsage) }
	user := fs.String("user", "", "入账的用户：用户 ID 或用户名")
	stringAlias(fs, user, "u", sameAs("user"))
	amount := fs.String("amount", "", "入账金额，必须为正")
	asset := fs.String("asset", "", "资产品种；留空表示取 settings 的 billing.default_asset")
	ref := legacyStringFlag(fs, "ref", "idempotency-key", "", "外部单据号，同时是幂等键；必填，同一条命令重跑不会重复入账")
	stringAlias(fs, ref, "order", legacyNote("ref"))
	gateway := fs.String("gateway", manualGateway, "这笔钱从哪个外部来源进来；manual 表示线下到账")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"credit 不接受位置参数，得到 %q：用户、金额与幂等键都用旗标指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --user"))
	}
	// 外部单据号必填且**没有默认值**：默认值会把「操作者忘了给单号」变成两笔
	// 互不相干的入账，而这两笔在账面上都合法——正是最难事后发现的那种错。
	orderKey := strings.TrimSpace(*ref)
	if orderKey == "" {
		fs.Usage()
		return usageErr(errors.New(
			"缺少 --ref：它是这笔入账对应的外部单据号，同时是幂等键——重跑同一条命令时靠它认出「已经入过账」"))
	}
	if ctrlErr := checkOrderKey(orderKey); ctrlErr != nil {
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
func checkOrderKey(order string) error {
	for _, r := range order {
		if r < 0x20 || r == 0x7f {
			return usageErr(fmt.Errorf(
				"--idempotency-key 不能含控制字符（这里是 %q）：它同时是幂等键与备注里的"+
					"单据号，带控制字符会让两笔不同的入账在账面上看起来一样",
				r))
		}
	}
	return nil
}

// parseCreditAmount 解析 --amount。
//
// 与 parseQuotaLimit 同一条判据：先自己数小数位。money.Parse 会把多出来的
// 位四舍五入，而入账金额是「敲进去多少就该记多少」的数——静默改掉最后一位
// 会让账本上的数字与操作者手里的单据对不上。
func parseCreditAmount(plain string) (money.Amount, error) {
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
	if !amount.IsPositive() {
		return money.Zero, usageErr(fmt.Errorf(
			"--amount 必须为正数，得到 %s：入账不带负号，方向由分录决定", amount))
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

// txnIDForOrder 把运维给的幂等键确定性映射成一个交易号。
//
// 为什么是确定性映射而不是每次生成一个新的随机 uuid：幂等键的用途就是
// 「同一条命令重跑不会重复入账」。随机 id 会让每次重跑都成为一笔新交易，
// 幂等就只能靠「先按备注查一遍」——而备注会清洗、会被人改，不是判据。
// 把键映射成 txn_id 之后，重跑的第二次在写库那一步撞上同一笔交易，
// 由 store.PostTransaction 判定「内容一致 = 重放」。
//
// 域分隔前缀让同一个键在别的用途下算出来的 uuid 不会撞进账本。
// 版本位用 8（RFC 9562 的自定义版本）而不是 5：底层是 SHA-256，
// 写成 v5 会谎称是 SHA-1 名字空间 uuid。ParseTxnID 不检查版本位，
// 这里设它只是为了让生成的值是一枚形态规范的 uuid。
func txnIDForOrder(order string) ledger.TxnID {
	sum := sha256.Sum256([]byte("larkax:wallet-credit:v1\x00" + order))
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

	for _, e := range txn.Entries {
		direction, magnitude := "借", e.Amount
		if e.Amount.IsNegative() {
			direction, magnitude = "贷", e.Amount.Neg()
		}
		// 借方是正、贷方是负（见 ledger.Entry.Amount）；打给人看时换成
		// 「借 / 贷 + 正数」，否则两条分录一条正一条负，读起来像两种金额。
		line := fmt.Sprintf("  %s %-24s %s %s", direction, e.Account, magnitude, e.Asset)
		if e.Memo != "" {
			line += "  " + e.Memo
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
