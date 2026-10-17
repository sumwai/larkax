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
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/settings"
)

const walletUsage = `larkaxctl wallet 管理用户钱包。

用法：
  larkaxctl wallet open --user <ID 或用户名> [--asset <资产>]

open 给一个已存在的用户开通钱包，余额为 0。它不写任何分录：
0 余额的户在账本上的形状就是「这个用户在 user 科目上没有分录」，
与快照里的 0 自洽。带初始金额意味着这次开户同时是一次充值，
而那属于充值入口（当前还没有），所以这里不带。

**幂等**：钱包已经存在时什么都不做、照样退出 0，输出里写明「已存在」；
刚建出来的写「已开通」。两者只能靠输出分辨，脚本要判断时看那一行。

资产默认取 settings 的 billing.default_asset（迁移里的默认值是 CNY）。
--asset 可以显式指定，但当前只有 CNY 一种受支持的资产，写别的会被拒绝。
资产品种是钱包主键的一部分：同一个用户可以同时有 CNY 与其他资产的钱包。

用户用 --user 指定，纯数字按 ID 解析，否则按用户名解析。用户不存在时报
「找不到用户」并退出 4——那条错误来自解析用户这一步，与「用户存在但没有
钱包行」是两件事。

这条命令只建 user_wallets 那一行，不碰 users，也不需要 LARKAX_MASTER_KEYS。
`

// walletTimeout 是这条命令连库与写库的时间上限。
//
// 与 userTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。各写各的常量，是因为它们将来可能各调各的。
const walletTimeout = 30 * time.Second

// runWallet 处理 wallet 子命令。
func runWallet(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, walletUsage)
		return usageErr(errors.New("wallet 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "open":
		return runWalletOpen(rest)
	case "help", "-h", "--help":
		fmt.Print(walletUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, walletUsage)
		return usageErr(fmt.Errorf("未知的 wallet 子命令 %q", action))
	}
}

// runWalletOpen 给一个用户开通钱包。
func runWalletOpen(args []string) error {
	fs := flag.NewFlagSet("wallet open", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, walletUsage) }
	user := fs.String("user", "", "钱包归属的用户：用户 ID 或用户名")
	asset := fs.String("asset", "", "资产品种；留空表示取 settings 的 billing.default_asset")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("open 不接受位置参数，得到 %q：用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --user"))
	}

	// 显式给的资产在连库之前校验（与 user create / apikey create 同一条约定：
	// 一个写错的命令不该先连上生产库再报错）。留空时只能连库之后读 settings，
	// 那时再校验。
	var explicitAsset ledger.Asset
	if strings.TrimSpace(*asset) != "" {
		explicitAsset = ledger.Asset(strings.TrimSpace(*asset))
		if !explicitAsset.Valid() {
			return usageErr(fmt.Errorf("资产 %q 不受支持（目前只有 %s）", *asset, ledger.AssetCNY))
		}
	}

	return withDB(walletTimeout, func(ctx context.Context, db *sql.DB) error {
		// 先解析用户再开户：外键也会挡下不存在的 id，但那时拿到的是驱动的
		// 原文；这里能说清是哪个标识没对上，也让退出码落到「那一行不存在」。
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}

		assetValue := explicitAsset
		if assetValue == "" {
			// 默认资产取自运行期配置，而不是在代码里写死第二个 CNY：
			// 这个键的说明就是「新用户钱包的默认资产品种」，而结算用的资产
			// 也是同一个键（入口读它），两边因此不会各说各的。
			values, err := settings.Load(ctx, settings.NewPgSource(db))
			if err != nil {
				return err
			}
			configured, err := values.String("billing.default_asset")
			if err != nil {
				return err
			}
			assetValue = ledger.Asset(configured)
			if !assetValue.Valid() {
				return fmt.Errorf(
					"settings.billing.default_asset = %q 不是受支持的资产（目前只有 %s）：改那个配置，或用 --asset 指定",
					configured, ledger.AssetCNY)
			}
		}

		created, err := store.New(db).OpenWallet(ctx, owner.ID, assetValue)
		if err != nil {
			return err
		}

		printOpenedWallet(os.Stdout, owner, assetValue, created)
		return nil
	})
}

// printOpenedWallet 打印开户结果。
//
// 「刚建出来」与「本来就有」必须能分开：两者都是退出 0，而一次静默的空操作
// 与一次真的建了行在结果上不一样（LESSONS 第 16 条）。这里打给人看，
// 没有需要被脚本接走的单个值，所以只有 stdout 一个流。
func printOpenedWallet(w io.Writer, owner auth.User, asset ledger.Asset, created bool) {
	if created {
		_, _ = fmt.Fprintf(w, "已为用户 %s(id=%d) 开通 %s 钱包，余额 0。\n",
			owner.Username, owner.ID, asset)
		return
	}
	_, _ = fmt.Fprintf(w, "用户 %s(id=%d) 已有 %s 钱包，未改动。\n",
		owner.Username, owner.ID, asset)
}
