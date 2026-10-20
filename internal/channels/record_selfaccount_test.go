package channels

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/route"
)

// 零用量（失败调用、上游没报 usage）不推进自记账，也不该把额度状态写一遍。
//
// 「扣零等于没扣」——但真的进了事务之后，Project(prev, 0) 仍然会改写
// probe_kind（→ self_accounting）、把 confidence 从实测的 1 降到自记账的
// 基线 0.8，并推一次 updated_at。库里那一行看起来就像「刚刚自记账过一次」，
// 而实际上什么观测都没有发生。这条用例钉住「零用量在开事务之前就返回」。
func TestRecordCallZeroUsageLeavesQuotaStateUntouched(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: uniqueModel()})
	insertQuota(t, db, id, "1000", "tokens", "api_query", "healthy", 1)

	r := NewRecorder(db, testStaleAfter)
	// 失败的调用没有用量。
	if err := r.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: id,
		At:        time.Now().UTC(),
		Verdict:   route.VerdictFailure,
	}); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	snap := readQuota(t, db, id)
	if !snap.hasRemain || snap.remaining != 1000 {
		t.Errorf("remaining = %v（有值 %v），期望不动（1000）", snap.remaining, snap.hasRemain)
	}
	if snap.probeKind != "api_query" {
		t.Errorf("probe_kind = %q，期望仍是 api_query——零用量的调用不该被记成一次自记账", snap.probeKind)
	}
	if snap.confidence != 1 {
		t.Errorf("confidence = %v，期望仍是 1（实测），零用量的调用不该把它降成自记账基线", snap.confidence)
	}
}

// 下面两段是等价变异的说明，不是用例。判据见 scripts/check-mutation.sh 的
// 「存活变异要逐条人看」那一段：能写出结构性证明的存活条目不必补用例。

// record.go:358 的 `!found || prev.Remaining == nil || prev.Unit != quota.UnitTokens`
// ——把靠左的那个 ||（第 12 列）改成 && 后是等价变异。
//
// 改动后的判据是 `(!found && prev.Remaining == nil) || prev.Unit != quota.UnitTokens`。
// 逐格看：
//
//   - prev.Unit != tokens 时，两版都为真，都在这里返回；
//   - 单位是 tokens 时，两版之差只剩「恰好一个前提不成立」的两格：
//     found 且 Remaining == nil 时原版返回、变异体继续往下走；
//     !found 且 Remaining != nil 时变异体返回——但「没有行」时
//     readQuotaState 返回的是 `quota.State{}, false, nil`（record.go:403-404），
//     Remaining 必然为 nil，这一格不可达。
//   - 唯一可达的那一格没有可观察差别：继续往下走时 Project 在基准为 nil 时
//     返回一个没有数值的观测，Merge 于是也不动 Remaining，紧接着的第二道
//     守卫（record.go:375-380 的 `if next.Remaining == nil { return nil }`）
//     原样返回，一个字节都不写、错误也不变。
//
// 同一条里靠右的那个 ||（第 37 列）也有一个变异体，它被
// TestRecordCallSelfAccountingSkipsForeignUnit 杀掉了（单位对不上时会扣减并
// 改写 probe_kind），不在存活清单里。

// record.go:425 的 `confidence != ""`——把 != 改成 == 是**真缺口**，不是等价变异。
//
// 这一列是 `numeric(3, 2) NOT NULL DEFAULT 0`（internal/migrate/sql/00001_init.sql:203），
// 扫进 string 一定是十进制文本（"0.00"、"0.85"…），不可能是空串。于是原判据
// **恒为真**：每一次读都会解析并写进 state.Confidence。变异体的 `== ""` 恒为假，
// 每次都跳过解析，Confidence 留在零值——列里写着 0.85 时，两版读出的是 0.85
// 与 0。这不是等价，只是此前没有用例在 confidence 非零时走读路径。
// 由 TestReadQuotaStateParsesConfidence 钉住。
//
// 注意当前两个调用方（selfAccount 与探测的写回）都会用 Merge 重算 confidence，
// 所以差别只体现在 readQuotaState 返回的 quota.State 上——它返回的是完整的
// 状态，Confidence 是契约的一部分（交集见 internal/route 的 scoreQuota）。

// readQuotaState 必须把 confidence 解析出来，不能靠「跳过空串」把这一列丢掉。
//
// 这一列 NOT NULL，空串这一支在原版里永远走不到；判据一旦反过来写成
// 「只在空串时解析」，等于每次都跳过——而 0 在打分里表示「这个数不可信」，
// 与一次实测的可信度正好相反。当前调用方会用 Merge 重算它，所以断言放在
// 读函数自己的返回值上：它是 quota.State 的完整读出。
func TestReadQuotaStateParsesConfidence(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: uniqueModel()})
	insertQuota(t, db, id, "1000", "tokens", "api_query", "healthy", 0.85)

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, found, err := readQuotaState(t.Context(), tx, id)
	if err != nil {
		t.Fatalf("读额度状态失败: %v", err)
	}
	if !found {
		t.Fatal("额度状态行存在，却读成「没有行」")
	}
	if state.Confidence != 0.85 {
		t.Errorf("Confidence = %v，期望 0.85——读路径必须解析这一列", state.Confidence)
	}
}
