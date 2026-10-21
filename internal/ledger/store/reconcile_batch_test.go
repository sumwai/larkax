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
// 两条等价变异（gremlins CONDITIONALS_BOUNDARY，把 unionKeys 那个比较器里两处
// `<` 改成 `<=`）：第一支只在两个 user_id 不等时进得来、第二支只在它们相等时
// 进得来，而键来自 map（同一用户的 asset 互不相同），两侧永远不等——`<` 与
// `<=` 逐点相同，没有输入分得出这两版。
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

// comparePlanFaces 只在「在途预留 > 面值余额」时报，相等与更小都不报。
//
// 这一条不碰数据库：两笔数由参数给，于是三格边界可以逐个造出来。
// 真库上的那两条只能自然造出「相等」（面值 10、预留 10）与「超出」
// （直接改大一截），「在途更小」那一格在真库里只是顺带满足——摆在这里
// 是为了让比较写反（`<` 写成 `>`）有一个输入能分出来。
//
// 相等那一格是作废一条还有在途预留的订阅之后的状态（余额 − 在途被核销，
// 剩下恰好等于在途），它必须算合法，否则每次作废都会报一条。
func TestComparePlanFacesReportsOnlyShortfalls(t *testing.T) {
	plans := []planFaceRow{
		{planID: 11, userID: 7, asset: "CNY", balance: money.MustParse("10"), pending: money.MustParse("10")},
		{planID: 12, userID: 7, asset: "CNY", balance: money.MustParse("10"), pending: money.MustParse("15")},
		{planID: 13, userID: 8, asset: "USD", balance: money.MustParse("10"), pending: money.MustParse("3")},
	}

	got := comparePlanFaces(42, plans)
	if len(got) != 1 {
		t.Fatalf("只应有订阅 12 那一条，实际 %d 条: %v", len(got), got)
	}
	f := got[0]
	if f.Kind != FindingPlanFaceShortfall {
		t.Errorf("类别 = %s，期望 %s", f.Kind, FindingPlanFaceShortfall)
	}
	if f.RunID != 42 || f.UserID != 7 || f.Asset != "CNY" {
		t.Errorf("run/user/asset = %d/%d/%s，期望 42/7/CNY", f.RunID, f.UserID, f.Asset)
	}
	if want := prepaidScopePrefix + "12"; f.Scope() != want {
		t.Errorf("scope = %s，期望 %s", f.Scope(), want)
	}
	if f.Snapshot.Cmp(money.MustParse("10")) != 0 || f.Ledger.Cmp(money.MustParse("15")) != 0 {
		t.Errorf("余额/在途 = %s/%s，期望 10/15", f.Snapshot, f.Ledger)
	}
}
