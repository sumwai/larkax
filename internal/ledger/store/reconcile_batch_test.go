package store

import (
	"slices"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// unionKeys 的次序必须是确定的：加锁顺序按它排出来，而一次交易涉及的多个
// 用户只要不按同一个顺序拿锁，两个并发事务就会互相等待（见 lockWallets 的
// 「为什么必须排序」）。
//
// 要固定住的是「每次都是同一个确定的次序」，而不是「升序」这个方向本身
// ——倒序同样满足死锁那一条。所以期望值写成具体的次序，而不是只比「两次
// 输出相同」：只比一致的话，把 `!=` 改成 `==`（次序退化成 map 的迭代顺序、
// 每次都不同）能被看见，把 `<` 改成 `>=`（整体倒序、每次仍然一样）就看不见了。
//
// 三份 map 的键故意交错，且每个用户有两种资产：只按资产比的那一侧、
// 以及与资产无关的那一侧，都要能被看出来。`{3, CNY}` 同时出现在两份 map 里，
// 期望值里它只算一次——取并集时漏掉去重也会让这条用例红。
//
// 这一条不需要数据库：unionKeys 只碰传进来的三份 map。
func TestUnionKeysOrdersDeterministically(t *testing.T) {
	wallets := map[userAsset]walletSnapshot{
		{userID: 7, asset: "CNY"}: {balance: money.MustParse("10")},
		{userID: 3, asset: "USD"}: {frozen: money.MustParse("2")},
		{userID: 5, asset: "CNY"}: {},
	}
	ledgerSums := map[userAsset]money.Amount{
		{userID: 3, asset: "CNY"}: money.MustParse("10"),
		{userID: 7, asset: "USD"}: money.MustParse("4"),
	}
	pending := map[userAsset]money.Amount{
		{userID: 5, asset: "USD"}: money.MustParse("2"),
		{userID: 3, asset: "CNY"}: money.MustParse("1"),
	}

	// 先按 user_id、再按 asset：这个次序就是加锁与比对走的那一个。
	want := []userAsset{
		{userID: 3, asset: "CNY"},
		{userID: 3, asset: "USD"},
		{userID: 5, asset: "CNY"},
		{userID: 5, asset: "USD"},
		{userID: 7, asset: "CNY"},
		{userID: 7, asset: "USD"},
	}

	// 跑多次：输入的次序来自 map，而 map 的迭代顺序每次都不一样，
	// 所以「排过一次」与「碰巧没打乱」在单次运行里分不出来。
	for i := 0; i < 100; i++ {
		got := unionKeys(wallets, ledgerSums, pending)
		if !slices.Equal(got, want) {
			t.Fatalf("第 %d 次的次序 = %v，期望 %v", i+1, got, want)
		}
	}
}
