package ledger

import (
	"fmt"
	"sort"

	"github.com/sumwai/larkax/internal/money"
)

// BalanceOf 从一组分录里算出某个科目在某个资产上的余额。
//
// 纯函数：**不查库**。调用方负责自己把某个科目的全部相关分录取出来——
// 通常是 `WHERE account = $1 AND asset = $2`，或者限制在一段时间窗口内
// 做一次对账。这样「余额怎么算」这件事可以脱离数据库单独测试，
// 而它算错的表现是数字对不上，不报错（唯一会报错的情形是求和放不进
// int64，见下）。
//
// # 各科目余额的含义
//
// 借方记正、贷方记负，于是「余额 = 全部金额之和」这一句话对每个科目
// 都成立，而每个科目正数的含义不同（完整约定见 account.go）：
//
//	user                    正 = 平台欠用户的钱，负 = 用户欠平台
//	merchant_payable        正 = 平台欠商户的分成
//	platform_revenue        正 = 平台收到的钱，负 = 平台付出的钱
//	upstream_cost           正 = 上游成本
//	payout                  正 = 已申请未打款的待付
//	payment_gateway         负 = 通道替我们持有的钱
//	reconcile_difference    正 = 账本高于快照、多出来的那笔被冲回，
//	                        负 = 账本低于快照、平台补上了差额
//
// 用户余额（user_wallets.balance）对应的是 user 科目的和。钱包表的
// CHECK 约束要求 balance >= 0，所以负的用户余额在那里会写不进去。
// 这是**有意**的：负余额说明某处少扣了钱，它应当被拦住并暴露出来，
// 而不是安静地存进库里供后续请求继续扣。
//
// # 求和超出表示范围时报错，不夹住
//
// 分录之和可能落在 money 的 int64 之外——库里的列是 numeric(20, 8)，比 int64
// 宽。夹住给出的是一个**不等于真实和**的数（正向溢出时比真实和小），而余额
// 偏小看起来正是「少扣了钱」，那是账目上最难查的一类。所以这里报错。
func BalanceOf(entries []Entry, account Account, asset Asset) (money.Amount, error) {
	amounts := make([]money.Amount, 0, len(entries))
	for _, e := range entries {
		if e.Asset != asset || e.Account != account {
			continue
		}
		amounts = append(amounts, e.Amount)
	}
	total, ok := money.SumChecked(amounts...)
	if !ok {
		return money.Zero, balanceOverflow(account, asset, len(amounts))
	}
	return total, nil
}

// Balances 汇总一组分录，得到每个科目、每种资产各自的余额。
//
// 用途是对账：一次把一段分录的全部科目算出来，与快照表
// （user_wallets 等）逐项比对。比对的是「权威值」与「快照值」，
// 两者不一致就是一分账目异常，需要人工查。
//
// 返回值是 map[资产]map[科目]金额。分资产而不是拍平成一个 map，
// 是因为同一个科目在两种资产上的余额是两件独立的事——
// 拍平会让 USDT 的余额看起来像是 CNY 余额的一部分。
//
// 某一个科目的和超出表示范围时整份汇总都算不出来，而不是把那个科目
// 少算一点交出去（理由见 BalanceOf）。要先按资产把金额归组、再逐组求和，
// 是因为判据是「这一组的**真实和**放不下」，而它不能边加边判（见
// money.SumChecked）。代价是多出一份「每条分录一个金额」的临时切片，
// 与调用方本来就要把全部分录读进内存相比是同一个量级。
func Balances(entries []Entry) (map[Asset]map[Account]money.Amount, error) {
	grouped := make(map[Asset]map[Account][]money.Amount)
	for _, e := range entries {
		byAccount, ok := grouped[e.Asset]
		if !ok {
			byAccount = make(map[Account][]money.Amount)
			grouped[e.Asset] = byAccount
		}
		byAccount[e.Account] = append(byAccount[e.Account], e.Amount)
	}

	// 溢出的科目先全部收集起来再排序报第一个：map 的遍历顺序是随机的，而报错
	// 文本必须稳定——同一个错误的措辞在两次运行之间不同，测试就断言不了，
	// 排查时也无法按报错文本去搜日志（与 sortedAssets 同一个理由）。
	var overflowed []overflowedAccount
	out := make(map[Asset]map[Account]money.Amount, len(grouped))
	for asset, byAccount := range grouped {
		sums := make(map[Account]money.Amount, len(byAccount))
		for acct, amounts := range byAccount {
			total, ok := money.SumChecked(amounts...)
			if !ok {
				overflowed = append(overflowed, overflowedAccount{asset, acct, len(amounts)})
				continue
			}
			sums[acct] = total
		}
		out[asset] = sums
	}
	if len(overflowed) > 0 {
		sort.Slice(overflowed, func(i, j int) bool {
			if overflowed[i].asset != overflowed[j].asset {
				return overflowed[i].asset < overflowed[j].asset
			}
			return overflowed[i].account.String() < overflowed[j].account.String()
		})
		first := overflowed[0]
		err := balanceOverflow(first.account, first.asset, first.entries)
		if len(overflowed) > 1 {
			// 剩下的也要说：只说第一个会让人以为修好这一个就没事了。
			err = fmt.Errorf("%w（另有 %d 个科目也超出范围）", err, len(overflowed)-1)
		}
		return nil, err
	}
	return out, nil
}

// overflowedAccount 是 Balances 里撞上溢出、没能算出来的一个科目。
type overflowedAccount struct {
	asset   Asset
	account Account
	entries int
}

// balanceOverflow 说明「这组分录的和超出了 money 能表示的范围」。
//
// 它把 money.ErrOverflow 包在里面，于是调用方能按 errors.Is 认出这一类。
// 文本里要写清楚这是**表示能力**不够、不是账不平：两者的处置不同——前者要么
// 扩表示、要么明确拒绝写入这个量级，后者要去查数据。
func balanceOverflow(account Account, asset Asset, entries int) error {
	return fmt.Errorf(
		"科目 %s 在 %s 上的余额算不出来（%d 条分录之和放不进 int64；库里的 numeric(20, 8) 存得下这个数，money 表示不了）: %w",
		account, asset, entries, money.ErrOverflow)
}

// sortedAssets 按资产的字典序返回键。
//
// 存在的理由只有一个：map 的遍历顺序是随机的，而校验失败时的错误信息
// 必须稳定——同一个错误的报错文本在两次运行之间不同，测试就断言不了，
// 排查时也无法按报错文本去搜日志。
func sortedAssets(m map[Asset]money.Amount) []Asset {
	keys := make([]Asset, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
