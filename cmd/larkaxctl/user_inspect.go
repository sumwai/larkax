package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/plans"
)

// runUserInspect 在一个只读事务里取齐一个用户的全景，一次打出来。
//
// # 为什么要有这一条
//
// 排查一个用户反馈通常要跑五条只读命令（user get、user wallet show、
// user plan list、user plan order show、user key list）。少看一条就会误判，
// 而误判的方向通常是「余额还有钱，所以不是额度问题」——没看的是套餐余量
// 已经用尽，或者这个用户的扣费顺序是 balance_first。
//
// # 为什么是一个事务
//
// 五份数据只有取自同一时刻才说明得了同一件事。PostgreSQL 在 READ COMMITTED
// 下每条语句各取一次快照，所以这里要的是 REPEATABLE READ 的只读事务：
// 否则一笔入账可能已经出现在钱包那一栏、还没出现在订阅余量那一栏，
// 而屏幕上那一屏看起来一切正常。
//
// 只读是字面意义上的：这个事务里只有 SELECT，写方法在 NewInTx 造出来的
// 那些句柄上没有可用的连接（见各包的 NewInTx）。
func runUserInspect(args []string) error {
	// -h 要在摘位置参数之前收掉（见 helpflag.go）。
	if answerHelpFlag(args, userShortUsage) {
		return nil
	}
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErr(fmt.Errorf("inspect 只接受一个用户，多出来的 %q 不认识", rest[0]))
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{
			Isolation: sql.LevelRepeatableRead,
			ReadOnly:  true,
		})
		if err != nil {
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// 用 UserByRef 而不是 ResolveUser：判据是「用户不存在」与 user get
		// 逐字一致（同一句话、同一个退出码），而那句话长在 UserByRef 里。
		authStore := auth.NewInTx(tx)
		info, err := authStore.UserByRef(ctx, user)
		if err != nil {
			return err
		}

		snapshots, err := store.NewInTx(tx).Wallets(ctx, &info.ID)
		if err != nil {
			return fmt.Errorf("列出钱包失败: %w", err)
		}
		// 用户名取自上面那一行，而不是像 wallet list 那样把整张 users 再列
		// 一遍：同一个用户的两份数据里它只能有一个值，重列一次只会多一次
		// 往返，外加一个它能与那一行不一致的机会。
		rows := make([]walletRow, 0, len(snapshots))
		for _, w := range snapshots {
			rows = append(rows, walletRow{
				UserID:     w.UserID,
				Username:   info.Username,
				Asset:      w.Asset,
				Balance:    w.Balance,
				Frozen:     w.Frozen,
				BalanceErr: w.BalanceErr,
				FrozenErr:  w.FrozenErr,
				BalanceRaw: w.BalanceRaw,
				FrozenRaw:  w.FrozenRaw,
			})
		}
		// 与 user wallet show（以及它的旧写法 wallet list --user）同一条判据：
		// 有一格读不出来就判这一份报告失败。表格照打、退出码落 1，那句话由
		// main 打到 stderr（库里的原文只有那一条路出得来，见
		// walletUnreadableCause）。
		cause := walletUnreadableCause(rows)

		planSvc := plans.NewInTx(tx)
		subs, err := planSvc.UserPlans(ctx, info.ID)
		if err != nil {
			return err
		}
		order, source, err := planSvc.EffectiveChargeOrder(ctx, info.ID)
		if err != nil {
			return err
		}
		keys, err := authStore.ListKeys(ctx, &info.ID)
		if err != nil {
			return fmt.Errorf("列出 API Key 失败: %w", err)
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, userInspectDoc(info, rows, cause, subs, order, source, keys), cause)
		}
		printUserInspect(humanStdout(), info, rows, subs, order, source, keys)
		return cause
	})
}

// printUserInspect 打一个用户的全景。
//
// 每一段都调用对应子命令自己的打印函数，而不是另写一份：判据要求每一段与
// 分别跑 user get / user wallet show / user plan list / user plan order show /
// user key list 的输出逐字相同，共用打印机是唯一不必靠人守着的做法——加一栏
// 只改一处，两条路就还是同一个形状。
//
// 段标题单独一行并以「：」结尾，与帮助文本里的段标题同一套写法；它不进
// 表格，也不参与列的宽度计算。
func printUserInspect(w io.Writer, info auth.UserInfo, rows []walletRow,
	subs []plans.UserPlan, order plans.ChargeOrder, source plans.ChargeOrderSource,
	keys []auth.Key) {
	p := newPalette(w)
	section := func(title string) {
		_, _ = fmt.Fprintf(w, "\n%s\n", p.bold(title+"："))
	}

	_, _ = fmt.Fprintf(w, "用户 %s 的全景：\n", info.Username)
	section("账号")
	printUserDetail(w, info)
	section("钱包")
	printWalletList(w, rows)
	section("订阅")
	printUserPlans(w, subs)
	section("扣费顺序")
	printPlanOrder(w, auth.User{ID: info.ID, Username: info.Username}, order, source)
	section("API Key")
	printKeyList(w, keys)
}

// userInspectDoc 组装 user inspect 的机器可读输出。
//
// 字段名沿用各子命令已有的那一个（user、wallets、subscriptions、keys，以及
// 扣费顺序的 order 与 source），不另起一套——否则同一份数据会有两套命名，
// 而第二套会先出现在这里。
//
// 空集合一律给 []，缺值一律给 null：三个列表回答的是「这一栏有什么」，
// 一条都没有就是空数组，不会是 null；user 里没填的列（email、display_name、
// deleted_at）与条目里的可空列照旧给 null。于是「没有订阅」与「订阅没读出来」
// 在形状上分得开——后者是整份报告失败（cause 不为 nil），那一栏根本不会
// 被当成「没有」。
func userInspectDoc(info auth.UserInfo, rows []walletRow, cause error,
	subs []plans.UserPlan, order plans.ChargeOrder, source plans.ChargeOrderSource,
	keys []auth.Key) map[string]any {
	status := jsonOK
	if cause != nil {
		status = jsonFailed
	}
	doc := jsonDoc(status)
	doc["user"] = userDetailEntry(info)

	// 三个列表都从空切片起：没有条目时打 `[]`，不是 `null`（见 format.go 的
	// jsonList 与上面那段「空集合一律给 []」）。
	wallets := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		wallets = append(wallets, walletEntry(r))
	}
	doc["wallets"] = wallets

	subscriptions := make([]map[string]any, 0, len(subs))
	for _, up := range subs {
		subscriptions = append(subscriptions, userPlanEntry(up))
	}
	doc["subscriptions"] = subscriptions

	doc["order"] = string(order)
	doc["source"] = string(source)

	keyEntries := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		keyEntries = append(keyEntries, apiKeyEntry(k))
	}
	doc["keys"] = keyEntries
	return doc
}
