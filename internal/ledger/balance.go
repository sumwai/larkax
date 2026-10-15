package ledger

import (
	"sort"

	"github.com/sumwai/larkax/internal/money"
)

// BalanceOf 从一组分录里算出某个科目在某个资产上的余额。
//
// 纯函数：**不查库**。调用方负责自己把某个科目的全部相关分录取出来——
// 通常是 `WHERE account = $1 AND asset = $2`，或者限制在一段时间窗口内
// 做一次对账。这样「余额怎么算」这件事可以脱离数据库单独测试，
// 而它算错的表现是数字对不上，不报错。
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
//
// 用户余额（user_wallets.balance）对应的是 user 科目的和。钱包表的
// CHECK 约束要求 balance >= 0，所以负的用户余额在那里会写不进去。
// 这是**有意**的：负余额说明某处少扣了钱，它应当被拦住并暴露出来，
// 而不是安静地存进库里供后续请求继续扣。
func BalanceOf(entries []Entry, account Account, asset Asset) money.Amount {
	total := money.Zero
	for _, e := range entries {
		if e.Asset != asset || e.Account != account {
			continue
		}
		total = total.Add(e.Amount)
	}
	return total
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
func Balances(entries []Entry) map[Asset]map[Account]money.Amount {
	out := make(map[Asset]map[Account]money.Amount)
	for _, e := range entries {
		byAccount, ok := out[e.Asset]
		if !ok {
			byAccount = make(map[Account]money.Amount)
			out[e.Asset] = byAccount
		}
		byAccount[e.Account] = byAccount[e.Account].Add(e.Amount)
	}
	return out
}

// Sum 汇总一组分录在某个资产上的金额，不区分科目。
//
// 它与 Verify 的「和为零」是同一个量，但**不校验**：对账脚本想知道
// 「这批分录加起来差多少」时，正需要这个非零的差额本身；
// 而 Verify 只回答「平不平」。
//
// 调用单位是「一个 txn_id 的那几条分录」——跨交易求和对账本没有意义，
// 因为不同交易的金额本来就不该抵消。
func Sum(entries []Entry, asset Asset) money.Amount {
	total := money.Zero
	for _, e := range entries {
		if e.Asset != asset {
			continue
		}
		total = total.Add(e.Amount)
	}
	return total
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
