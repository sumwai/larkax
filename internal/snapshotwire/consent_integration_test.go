package snapshotwire

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/gateway"
)

// 四格落库：有授权/无授权 × 请求/响应方向，每一格判库里那一行的
// capture_mode 与 skeleton 列的正文。
//
// # 为什么这一条必须走真库
//
// 上面几条单元用例判的是「档与正文配对正确」，用的是假 DB。它们证明不了
// 两件只有真库才有的事：
//
//   - capture_mode 的两个取值**真的能落进那一列**（表上有 CHECK，
//     而 CHECK 认不认一个新取值只有库说了算）；
//   - skeleton 是 jsonb：原文里的引号、换行、Unicode 转义由驱动与列类型
//     决定，写进去再读回来是不是同一份内容，只有库能回答。
//
// 第二条正是这条链路的用途所在——读的人要拿它复现一次故障，读回来的内容
// 与当时发出去的差一点都算白记。
//
// # 为什么四格都要在
//
// 与单元用例同一条理由：无授权却落原文是泄露，有授权却仍是骨架是判定没接上
// （而它**是安全的**，所以不会以任何故障的形式显形）。两个方向一起判，是因为
// 两条路各有一份脱敏器与一次判定：只接对一边的话，另一个方向会安静地落骨架。
func TestIntegrationCaptureTierLandsInTable(t *testing.T) {
	handle := testDB(t)
	ctx := context.Background()

	day := PartitionName(ownDay)
	dropRequestSnapshotPartition(ctx, t, handle, day)
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, day)
	})

	cells := []struct {
		name      string
		allow     bool
		direction string
		wantMode  string
	}{
		{"无授权/请求方向", false, "request", captureModeSkeleton},
		{"有授权/请求方向", true, "request", captureModeFullConsented},
		{"无授权/响应方向", false, "response", captureModeSkeleton},
		{"有授权/响应方向", true, "response", captureModeFullConsented},
	}

	stamp := time.Now().UTC().Format("20060102150405.000000000")
	for i, cell := range cells {
		t.Run(cell.name, func(t *testing.T) {
			src := &consentStub{allow: cell.allow}
			c := newCollectorWithConsent(t, testConfig(), Wrap(handle), src)
			// 写入指向 ownDay，让这一格的行只落进本用例自己那一天的分区。
			c.store.now = func() time.Time { return ownDay }

			// 内容里带上库里那两种最容易走样的东西：双引号与换行。
			marker := fmt.Sprintf("原样留下的内容 %d\n第二行 \"带引号\"", i)

			var snap *gateway.Snapshot
			var err error
			if cell.direction == "request" {
				snap, err = c.Redact(ctx, consentRequest(marker), gateway.CaptureIdentity{})
			} else {
				snap, err = c.RedactResponse(ctx, consentResponse(marker), gateway.CaptureIdentity{})
			}
			if err != nil {
				t.Fatalf("脱敏失败: %v", err)
			}

			// 归属信息由调用方（网关）填，脱敏器不写它们——这里照网关的做法补上，
			// 否则整张表里几行都用同一个空 request_id，断言挑不出自己那一行。
			snap.RequestID = fmt.Sprintf("wire-consent-%d-%s", i, stamp)
			snap.UserID = 42
			snap.Direction = cell.direction

			if err = c.Save(ctx, snap); err != nil {
				t.Fatalf("落库失败: %v", err)
			}

			var mode, body string
			var replaced int
			err = handle.QueryRowContext(ctx, `
				SELECT capture_mode, skeleton::text, cardinality(redacted_paths)
				  FROM request_snapshots
				 WHERE request_id = $1`, snap.RequestID).Scan(&mode, &body, &replaced)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}

			if mode != cell.wantMode {
				t.Errorf("capture_mode = %q，期望 %q", mode, cell.wantMode)
			}
			if got := strings.Contains(body, "原样留下的内容"); got != cell.allow {
				t.Errorf("授权 = %t，库里正文出现原文 = %t：%s", cell.allow, got, body)
			}
			// 原文那一档一个字段都没替换；被替换掉的那一档至少要有一条路径，
			// 否则「内容不见了」这件事在库里无从解释。
			if cell.allow && replaced != 0 {
				t.Errorf("原样采集时不该有被替换的字段，实际 %d 条", replaced)
			}
			if !cell.allow && cell.direction == "request" && replaced == 0 {
				t.Errorf("默认那一档应当记下被替换的字段，实际 %d 条", replaced)
			}
			// 全文那一档读回来必须还是同一份内容：换行与引号都要在。
			if cell.allow {
				if !strings.Contains(body, `\n`) || !strings.Contains(body, "带引号") {
					t.Errorf("原文读回来走样了: %s", body)
				}
			}
		})
	}
}
