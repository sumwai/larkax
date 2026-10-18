package store

import (
	"testing"

	"github.com/sumwai/larkax/internal/ledger"
)

// sameTxnContent 是「这一笔是不是重放」的判据，按**多重集**比较，不按顺序。
//
// 为什么单独测它：判宽了不会报错。PostTransaction 把它认成重放时一条分录都不写，
// 调用方拿到的是「已经入过账，本次未改动」；而若这次来的其实是另一笔真交易，
// 那笔钱就在账面上消失了，没有任何地方会报出来。
//
// 库里那笔与这次要写的，完全可能**用到的科目一样而每条的重复次数不一样**。
// 按集合比（wanted 的每个科目在 stored 里出现过就算重放）会把这笔真交易认成重放；
// 按多重集比，`stored=[A,B]` 对 `wanted=[A,A]` 是「不是重放」。
//
// 直接调这个函数：它的判据全在 Go 这一侧，不需要库。
func TestSameTxnContentComparesRepeatCounts(t *testing.T) {
	txn := topupTxn(t, ledger.NewTxnID(), 1, "100", "manual", "ORD-1")
	a, b := txn.Entries[0], txn.Entries[1]
	if entryFingerprint(a) == entryFingerprint(b) {
		t.Fatal("用例前提不成立：同一个交易的两条分录指纹应当不同")
	}

	cases := []struct {
		name   string
		stored []ledger.Entry
		wanted []ledger.Entry
		want   bool
	}{
		{"同内容同顺序", []ledger.Entry{a, b}, []ledger.Entry{a, b}, true},
		{"同内容乱序", []ledger.Entry{a, b}, []ledger.Entry{b, a}, true},
		// 这一格是这条用例存在的理由：want 用到的科目与 stored 完全一样，
		// 只有重复次数不同，按集合比会判成重放。
		{"这次多一条重复（[A,B] 对 [A,A]）", []ledger.Entry{a, b}, []ledger.Entry{a, a}, false},
		{"库里多一条重复（[A,A] 对 [A,B]）", []ledger.Entry{a, a}, []ledger.Entry{a, b}, false},
		{"这次多一条", []ledger.Entry{a}, []ledger.Entry{a, b}, false},
		{"这次少一条", []ledger.Entry{a, b}, []ledger.Entry{a}, false},
		// 两条一样的分录，写库路径不会产出这种交易（一次交易的科目互不相同）。
		// 它在这里管的是相反的方向：判据是「每条的重复次数都一样」，
		// 不是「见到重复就不认」。
		{"两份内容相同（两条一样的分录）", []ledger.Entry{a, a}, []ledger.Entry{a, a}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTxnContent(tc.stored, tc.wanted); got != tc.want {
				t.Errorf("sameTxnContent 返回 %v，期望 %v", got, tc.want)
			}
		})
	}
}
