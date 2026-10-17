package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// captureStdout 捕获函数执行期间写到标准输出的内容。
//
// printPartitions 的输出是这条命令唯一的正向证据（删了哪些分区），
// 所以它值得被断言，而不是靠人看一眼终端。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()

	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// ── 失败路径不能与成功路径同形 ─────────────────────────────────────────
//
// 两条命令（reconcile、ledger verify）都是「先把报告打出来，再处理错误」。
// 那个设计是对的：已经比出来的部分比一句「失败了」有用。要钉住的是另一半
// ——**中途失败时，stdout 不能说出一句只有跑完才成立的话**。
//
// 这两种情况在报告里的样子逐字相同：中途失败时 CheckedWallets / CheckedTxns
// 也是 0、Findings 也是空的，与「跑完了、范围内确实没有东西」一模一样。
// 所以判据只能是 err，下面每一对用例都把它单独摆出来。

// 对账失败时不许打「没有不一致。」，并且要说明没跑完。
func TestPrintReconcileReportFailureIsNotSilentSuccess(t *testing.T) {
	// 失败前一个都没比到：这正是 issue 里复现的那个形状（解析账本汇总失败）。
	var none bytes.Buffer
	printReconcileReport(&none, &store.Report{
		RunID: 2, Watermark: time.Date(2026, 10, 17, 15, 8, 48, 0, time.UTC),
	}, errors.New("解析账本汇总 \"184467440736.00000000\" 失败: 金额超出可表示范围"))
	out := none.String()
	if strings.Contains(out, "没有不一致") {
		t.Errorf("失败路径不该说「没有不一致」：\n%s", out)
	}
	if !strings.Contains(out, "没跑完") {
		t.Errorf("失败路径要明说这一轮没跑完：\n%s", out)
	}
	// 失败前比过多少照打：那是「报告先打」要保住的信息。
	if !strings.Contains(out, "比对=0 行") {
		t.Errorf("失败路径仍要打出已经比过的行数：\n%s", out)
	}

	// 失败前已经比出一部分：那部分仍然要打出来，但同样不能收在成功句式上。
	var partial bytes.Buffer
	printReconcileReport(&partial, &store.Report{
		RunID: 3, CheckedWallets: 7,
		Findings: []store.Finding{{
			Kind: store.FindingBalanceMismatch, UserID: 42, Asset: "CNY",
			Snapshot: money.MustParse("50"), Ledger: money.MustParse("80"),
		}},
	}, errors.New("取对账候选用户失败: connection reset"))
	partialOut := partial.String()
	if !strings.Contains(partialOut, "没跑完") {
		t.Errorf("失败路径要明说这一轮没跑完：\n%s", partialOut)
	}
	if !strings.Contains(partialOut, "不平衡") && !strings.Contains(partialOut, "balance_mismatch") {
		t.Errorf("失败前已经发现的不一致仍要列出来：\n%s", partialOut)
	}
	if strings.Contains(partialOut, "没有不一致") {
		t.Errorf("失败路径不该说没有不一致：\n%s", partialOut)
	}
}

// 对账跑完而范围内确实没有钱包时，「比对=0 行」与「没有不一致。」都是真话，
// 不能被上面那条改动顺手改掉——分辨点在 err，不在计数。
func TestPrintReconcileReportZeroWalletsIsStillSuccess(t *testing.T) {
	var buf bytes.Buffer
	printReconcileReport(&buf, &store.Report{
		RunID: 4, Watermark: time.Date(2026, 10, 17, 15, 8, 48, 0, time.UTC),
	}, nil)
	out := buf.String()
	for _, want := range []string{"比对=0 行", "没有不一致。"} {
		if !strings.Contains(out, want) {
			t.Errorf("正常跑完的空范围仍要说 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "没跑完") {
		t.Errorf("正常跑完不该出现「没跑完」：\n%s", out)
	}
}

// ledger verify 失败时不许说「全部自洽。」，并且要说明没跑完。
func TestPrintLedgerVerifyReportFailureIsNotSilentSuccess(t *testing.T) {
	// 读第一笔就失败：报告是空的（CheckedTxns == 0、Findings 为空）。
	var none bytes.Buffer
	printLedgerVerifyReport(&none, &store.VerifyReport{},
		errors.New("交易 17f10c24 的分录金额读不出来: 金额超出可表示范围"))
	out := none.String()
	if strings.Contains(out, "全部自洽") {
		t.Errorf("失败路径不该说「全部自洽」：\n%s", out)
	}
	if !strings.Contains(out, "没跑完") {
		t.Errorf("失败路径要明说这一轮没跑完：\n%s", out)
	}
	if !strings.Contains(out, "0 笔") {
		t.Errorf("失败路径仍要打出已经校验过的笔数：\n%s", out)
	}

	// 校验了一部分之后失败：那一部分的不合法照列，但整轮的结论不是「自洽」。
	txn := ledger.NewTxnID()
	var partial bytes.Buffer
	printLedgerVerifyReport(&partial, &store.VerifyReport{
		CheckedTxns: 2, CheckedEntries: 4,
		Findings: []store.VerifyFinding{{
			TxnID: txn,
			Entries: []ledger.Entry{{
				TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
				Amount: money.MustParse("30"), EntryType: ledger.EntryAdjustment,
			}},
			Err: errors.New("第 1 条分录: 科目 platform_revenue 的金额为零"),
		}},
	}, errors.New("读分录失败: connection reset"))
	partialOut := partial.String()
	if !strings.Contains(partialOut, "没跑完") {
		t.Errorf("失败路径要明说这一轮没跑完：\n%s", partialOut)
	}
	if !strings.Contains(partialOut, string(txn)) {
		t.Errorf("失败前已经校验出的不合法仍要列出来：\n%s", partialOut)
	}
	if strings.Contains(partialOut, "全部自洽") {
		t.Errorf("失败路径不该说全部自洽：\n%s", partialOut)
	}
}
