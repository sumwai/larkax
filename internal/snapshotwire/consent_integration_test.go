package snapshotwire

import (
	"context"
	"database/sql"
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

// 授权表上的四种「此刻不算数」各自真的把落库那一档压回骨架。
//
// # 为什么这一条必须走真库
//
// 撤销、到期、窗口两端这三件事判的是**库里的列**，而单元用例里的假 DB 只回答
// 「查到了没有」——它回答不了「WHERE 里那三个条件写对了没有」。把
// `revoked_at IS NULL` 写成 `revoked_at IS NOT NULL`，或者把 `expires_at > $3`
// 写成 `>=`，假 DB 那几条用例全绿，而线上是「撤销过的授权继续放行原文」。
// 这里的每一格都往库里写一行真的记录，再让采集链路去读它。
//
// # 为什么每一格都从空开始
//
// 时间窗那一档的授权按用户生效，覆盖这个用户的**全部**请求：上一条子用例留下
// 的窗口会让下一条的判定无缘无故变成真，而那时用例报出来的是它自己那一格。
// 所以每一格开头先把这个用户的授权记录清干净（见 resetConsents）。
func TestIntegrationConsentJudgmentReadsTheTable(t *testing.T) {
	handle := testDB(t)
	ctx := context.Background()

	day := PartitionName(ownDay)
	dropRequestSnapshotPartition(ctx, t, handle, day)
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, day)
	})

	// capture_consents.user_id 上有外键，得先有一个真的用户。
	var userID int64
	username := fmt.Sprintf("consent-it-%s", time.Now().UTC().Format("20060102150405.000000000"))
	if err := handle.QueryRowContext(ctx,
		`INSERT INTO users (username, status) VALUES ($1, 'active') RETURNING id`,
		username).Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = handle.ExecContext(bg, `DELETE FROM capture_consents WHERE user_id = $1`, userID)
		_, _ = handle.ExecContext(bg, `DELETE FROM users WHERE id = $1`, userID)
	})

	store := NewConsentStore(Wrap(handle))
	src := NewDBConsentSource(Wrap(handle), quietLogger())

	resetConsents := func(t *testing.T) {
		t.Helper()
		if _, err := handle.ExecContext(ctx,
			`DELETE FROM capture_consents WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("清理授权记录失败: %v", err)
		}
	}

	// writeOne 走一次采集，把这一条落库之后的档与正文读回来。
	//
	// 档与正文一起读：只判档的话，「判成原文那一档、正文却还是骨架」这种接线
	// 错位不会被发现，而它正好是这条链路唯一要保证的东西。
	writeOne := func(t *testing.T, reqID string) (mode, body string) {
		t.Helper()
		c := newCollectorWithConsent(t, testConfig(), Wrap(handle), src)
		// 写入指向 ownDay，让这一条只落进本用例自己那一天的分区。
		c.store.now = func() time.Time { return ownDay }

		marker := "原样留下的内容 " + reqID
		snap, err := c.Redact(ctx, consentRequest(marker),
			gateway.CaptureIdentity{UserID: userID, RequestID: reqID})
		if err != nil {
			t.Fatalf("脱敏失败: %v", err)
		}
		// 归属信息由调用方（网关）填，脱敏器不写它们——这里照网关的做法补上。
		snap.UserID = userID
		snap.RequestID = reqID
		snap.Direction = "request"

		if err := c.Save(ctx, snap); err != nil {
			t.Fatalf("落库失败: %v", err)
		}
		if err := handle.QueryRowContext(ctx, `
				SELECT capture_mode, skeleton::text
				  FROM request_snapshots
				 WHERE request_id = $1`, reqID).Scan(&mode, &body); err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		return mode, body
	}

	// grant 写一条授权，返回它的 id。
	grant := func(t *testing.T, req ConsentRequest) int64 {
		t.Helper()
		req.UserID = userID
		id, err := store.Grant(ctx, req, 24*time.Hour)
		if err != nil {
			t.Fatalf("写授权失败: %v", err)
		}
		return id
	}

	t.Run("生效中的时间窗放行原文", func(t *testing.T) {
		resetConsents(t)
		now := time.Now().UTC()
		grant(t, ConsentRequest{
			Scope:       ConsentScopeTimeWindow,
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(time.Hour),
		})

		if !src.AllowsRawCapture(ctx, userID, "consent-it-window-active") {
			t.Fatal("窗口正盖着此刻，判定应当为真")
		}
		mode, body := writeOne(t, "consent-it-window-active")
		if mode != captureModeFullConsented {
			t.Errorf("capture_mode = %q，期望 %q", mode, captureModeFullConsented)
		}
		if !strings.Contains(body, "原样留下的内容") {
			t.Errorf("原文那一档里没有原文: %s", body)
		}
	})

	t.Run("撤销之后立刻回到骨架", func(t *testing.T) {
		resetConsents(t)
		now := time.Now().UTC()
		id := grant(t, ConsentRequest{
			Scope:       ConsentScopeTimeWindow,
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(time.Hour),
		})
		if !src.AllowsRawCapture(ctx, userID, "consent-it-revoked") {
			t.Fatal("撤销之前判定应当为真")
		}

		revoked, err := store.Revoke(ctx, id)
		if err != nil || !revoked {
			t.Fatalf("撤销 = (%v, %v)，期望 (true, nil)", revoked, err)
		}

		// 撤销立即生效：没有任何后台任务参与，判定是在读的那一刻重查的。
		if src.AllowsRawCapture(ctx, userID, "consent-it-revoked") {
			t.Error("撤销之后判定应当为假")
		}
		if mode, _ := writeOne(t, "consent-it-revoked"); mode != captureModeSkeleton {
			t.Errorf("撤销之后 capture_mode = %q，期望 %q", mode, captureModeSkeleton)
		}

		var revokedAt sql.NullTime
		if err := handle.QueryRowContext(ctx,
			`SELECT revoked_at FROM capture_consents WHERE id = $1`, id).Scan(&revokedAt); err != nil {
			t.Fatalf("查询撤销时刻失败: %v", err)
		}
		if !revokedAt.Valid {
			t.Error("撤销过的那一行 revoked_at 应当有值")
		}
	})

	t.Run("到期之后回到骨架", func(t *testing.T) {
		resetConsents(t)
		now := time.Now().UTC()
		id := grant(t, ConsentRequest{
			Scope:     ConsentScopeSingleRequest,
			RequestID: "consent-it-expired",
			ExpiresAt: now.Add(time.Hour),
		})
		if !src.AllowsRawCapture(ctx, userID, "consent-it-expired") {
			t.Fatal("到期之前判定应当为真")
		}

		// 把到期时间挪到过去：这一行还是授权时的形状，只是过期了。清理任务
		// 还没轮到它（那是 S3 的事），而判定不该等清理任务。
		if _, err := handle.ExecContext(ctx,
			`UPDATE capture_consents SET expires_at = now() - interval '1 minute' WHERE id = $1`,
			id); err != nil {
			t.Fatalf("改到期时间失败: %v", err)
		}

		if src.AllowsRawCapture(ctx, userID, "consent-it-expired") {
			t.Error("过期之后判定应当为假")
		}
		if mode, _ := writeOne(t, "consent-it-expired"); mode != captureModeSkeleton {
			t.Errorf("过期之后 capture_mode = %q，期望 %q", mode, captureModeSkeleton)
		}
	})

	t.Run("时间窗还没开始不放行", func(t *testing.T) {
		resetConsents(t)
		now := time.Now().UTC()
		grant(t, ConsentRequest{
			Scope:       ConsentScopeTimeWindow,
			WindowStart: now.Add(time.Hour),
			WindowEnd:   now.Add(2 * time.Hour),
		})

		if src.AllowsRawCapture(ctx, userID, "consent-it-window-future") {
			t.Error("窗口还没开始，判定应当为假")
		}
		if mode, _ := writeOne(t, "consent-it-window-future"); mode != captureModeSkeleton {
			t.Errorf("窗口还没开始时 capture_mode = %q，期望 %q", mode, captureModeSkeleton)
		}
	})

	t.Run("时间窗已经过去不放行", func(t *testing.T) {
		resetConsents(t)
		now := time.Now().UTC()
		id := grant(t, ConsentRequest{
			Scope:       ConsentScopeTimeWindow,
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(time.Hour),
		})
		if _, err := handle.ExecContext(ctx, `
			UPDATE capture_consents
			   SET window_start = now() - interval '2 hours',
			       window_end   = now() - interval '1 hour',
			       expires_at   = now() - interval '1 hour'
			 WHERE id = $1`, id); err != nil {
			t.Fatalf("挪窗口失败: %v", err)
		}

		if src.AllowsRawCapture(ctx, userID, "consent-it-window-past") {
			t.Error("窗口已经过去，判定应当为假")
		}
		if mode, _ := writeOne(t, "consent-it-window-past"); mode != captureModeSkeleton {
			t.Errorf("窗口过去之后 capture_mode = %q，期望 %q", mode, captureModeSkeleton)
		}
	})

	t.Run("单条请求的授权不覆盖别的请求", func(t *testing.T) {
		resetConsents(t)
		now := time.Now().UTC()
		grant(t, ConsentRequest{
			Scope:     ConsentScopeSingleRequest,
			RequestID: "consent-it-single-a",
			ExpiresAt: now.Add(time.Hour),
		})

		if !src.AllowsRawCapture(ctx, userID, "consent-it-single-a") {
			t.Fatal("授权指向的那一次请求应当放行")
		}
		if src.AllowsRawCapture(ctx, userID, "consent-it-single-b") {
			t.Error("授权只覆盖那一次请求，别的请求应当为假")
		}
		if mode, _ := writeOne(t, "consent-it-single-b"); mode != captureModeSkeleton {
			t.Errorf("别的请求 capture_mode = %q，期望 %q", mode, captureModeSkeleton)
		}
	})

	// request_id 是空串的那一行不许放行。
	//
	// 手工插一行：命令行走不到这里（grant 要求 --request-id 非空），而表上的
	// shape 约束只要求 request_id IS NOT NULL——空串不算 NULL，插得进去。
	//
	// 这一行有多危险取决于 WHERE 怎么写：匹配那一支如果只写 `request_id = $2`，
	// 那么任何一次**拿不到 request id** 的请求（$2 也是空串）都会与它撞上，
	// 于是这个用户的每一条请求都变成原文。方向是放行，出事时没人会注意到。
	t.Run("request_id 为空串的同意记录不放行", func(t *testing.T) {
		resetConsents(t)
		if _, err := handle.ExecContext(ctx, `
			INSERT INTO capture_consents (
				user_id, scope, request_id, consent_text, consent_version, expires_at
			) VALUES ($1, 'single_request', '', $2, $3, now() + interval '1 hour')`,
			userID, ConsentText, ConsentVersion); err != nil {
			t.Fatalf("插入空 request_id 的授权失败: %v", err)
		}

		// 前提：库里真的有一条 request_id 为空串的有效授权。没有它的话，下面
		// 那句「不放行」可能是因为压根没有行而成立——那就什么都没验到。
		var n int
		if err := handle.QueryRowContext(ctx, `
			SELECT count(*) FROM capture_consents
			 WHERE user_id = $1 AND request_id = '' AND revoked_at IS NULL
			   AND expires_at > now()`, userID).Scan(&n); err != nil {
			t.Fatalf("数那一行失败: %v", err)
		}
		if n != 1 {
			t.Fatalf("库里 request_id 为空串的有效授权有 %d 条，用例的前提没成立", n)
		}

		if src.AllowsRawCapture(ctx, userID, "") {
			t.Error("request_id 为空串的同意记录不该放行原文")
		}
	})
}
