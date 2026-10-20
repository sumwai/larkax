package channels

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
)

// 轮换的用例需要一个真实的 PostgreSQL，与 channels 的其他集成测试同一个库。
//
// # 断言只认自己插入的那些行
//
// 这个库是**共享**的：同一个包的其他用例、以及并行跑着的其他包（catalog、
// assembly 等）都会往 channels 里插行，而轮换扫的是全表。所以——
//
//   - 主密钥标识带本次运行的唯一后缀（见 rotateTestKeys）。别的行挂的是别的
//     标识，于是本用例的轮换**只可能**改写自己插入的行：Rewrap 只对含当前
//     主密钥的信封动手，而那个标识只存在于本用例的行上。反过来，别的行也
//     不会被本用例碰到。
//   - 逐行清单（Problems / Missing）里可能有别人的行，所以断言是「这一行在
//     不在清单里」，不是「清单有几条」。
//   - 全局计数器里只有那些**结构上不受别人影响**的才断言具体值：比如重跑时
//     Rewrapped 必须恰好是 0（别人没有本用例的标识，不可能被补上副本）。

// rotateTestKeys 造一组「旧钥匙包着存量、新钥匙是当前」的主密钥。
func rotateTestKeys() (oldKey, newKey config.MasterKey) {
	suffix := itoa(nextID())
	return config.MasterKey{ID: "rot-old-" + suffix, Key: bytes.Repeat([]byte{0x11}, config.KeySize)},
		config.MasterKey{ID: "rot-new-" + suffix, Key: bytes.Repeat([]byte{0x22}, config.KeySize)}
}

// newRotateSealer 用给定的主密钥（第一把是当前）构造封装器。
func newRotateSealer(t *testing.T, keys ...config.MasterKey) *credential.Sealer {
	t.Helper()
	sealer, err := credential.NewSealer(keys)
	if err != nil {
		t.Fatalf("构造 Sealer 失败: %v", err)
	}
	return sealer
}

// readEnvelope 读一行的凭证密文。
func readEnvelope(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	var envelope []byte
	if err := db.QueryRowContext(context.Background(),
		`SELECT credential_enc FROM channels WHERE id = $1`, id).Scan(&envelope); err != nil {
		t.Fatalf("读渠道 %d 的密文失败: %v", id, err)
	}
	return envelope
}

// mustHaveKeys 断言这一行的密文恰好挂在给定的主密钥下（顺序不计）。
func mustHaveKeys(t *testing.T, envelope []byte, want ...string) {
	t.Helper()
	got, err := credential.WrappedWith(envelope)
	if err != nil {
		t.Fatalf("解析密文失败: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("密文挂在 %v 下，期望恰好 %v", got, want)
	}
	for _, id := range want {
		if !containsKey(got, id) {
			t.Fatalf("密文挂在 %v 下，期望含 %q", got, id)
		}
	}
}

// findRow 在逐行清单里找某一行。
func findRow(rows []RotateRow, id int64) (RotateRow, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return RotateRow{}, false
}

// insertRotateRows 插入 n 行渠道，凭证用 sealer 封。
func insertRotateRows(t *testing.T, db *sql.DB, sealer *credential.Sealer, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, insertChannel(t.Context(), t, db, sealer, channelSpec{model: uniqueModel()}))
	}
	return ids
}

// 干跑必须一行都不改，并且把改不了的行逐行报出来。
func TestRotatePlanDoesNotWrite(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	rotator := NewRotator(db, newRotateSealer(t, newKey, oldKey))

	ids := insertRotateRows(t, db, oldSealer, 3)
	badID := insertChannel(ctx, t, db, oldSealer, channelSpec{
		model:      uniqueModel(),
		credential: []byte("not-an-envelope"),
	})

	before := make(map[int64][]byte, len(ids))
	for _, id := range ids {
		before[id] = readEnvelope(t, db, id)
	}
	badBefore := readEnvelope(t, db, badID)

	report, err := rotator.Plan(ctx)
	if err != nil {
		t.Fatalf("干跑失败: %v", err)
	}
	if !report.DryRun {
		t.Error("干跑的报告应当标着 DryRun")
	}
	if report.PrimaryKeyID != newKey.ID {
		t.Errorf("当前主密钥 = %q，期望 %q", report.PrimaryKeyID, newKey.ID)
	}
	if report.Rewrapped != 3 {
		t.Errorf("会被轮换的行数 = %d，期望 3", report.Rewrapped)
	}
	if report.Current != 0 {
		t.Errorf("本来就含当前主密钥的行数 = %d，期望 0", report.Current)
	}

	badRow, found := findRow(report.Problems, badID)
	if !found {
		t.Fatalf("解析失败的行 %d 没有被报出来：%+v", badID, report.Problems)
	}
	if badRow.Err == nil {
		t.Errorf("行 %d 被报成「轮换不了」但没给原因", badID)
	}

	for _, id := range ids {
		if !bytes.Equal(before[id], readEnvelope(t, db, id)) {
			t.Errorf("干跑改动了行 %d 的密文", id)
		}
		mustHaveKeys(t, readEnvelope(t, db, id), oldKey.ID)
	}
	if !bytes.Equal(badBefore, readEnvelope(t, db, badID)) {
		t.Errorf("干跑改动了坏行 %d 的密文", badID)
	}
}

// 轮换之后每一行都含新主密钥，旧主密钥还在，正文一个字节都没动。
func TestRotateApplyAddsCurrentKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	newSealer := newRotateSealer(t, newKey, oldKey)
	rotator := NewRotator(db, newSealer)

	ids := insertRotateRows(t, db, oldSealer, 3)

	report, err := rotator.Apply(ctx, nil)
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if report.Rewrapped != 3 {
		t.Errorf("补上副本的行数 = %d，期望 3", report.Rewrapped)
	}
	if report.Current != 0 {
		t.Errorf("本来就含当前主密钥的行数 = %d，期望 0", report.Current)
	}
	for _, id := range ids {
		if row, found := findRow(report.Problems, id); found {
			t.Errorf("行 %d 被报成轮换不了：%v", id, row.Err)
		}
	}

	for _, id := range ids {
		envelope := readEnvelope(t, db, id)
		// 新旧两把都在：旧钥匙还没退役，这是轮换期间该有的样子。
		mustHaveKeys(t, envelope, newKey.ID, oldKey.ID)

		// 正文没被动过——两个方向都解一遍，拿到的还是原来那份凭证。
		// 这条断言同时说明轮换只重包了数据密钥。
		gotNew, err := newSealer.Open(envelope)
		if err != nil {
			t.Fatalf("行 %d 用新主密钥解不开: %v", id, err)
		}
		gotOld, err := oldSealer.Open(envelope)
		if err != nil {
			t.Fatalf("行 %d 用旧主密钥解不开: %v", id, err)
		}
		if string(gotNew) != "test-key-placeholder" || string(gotOld) != "test-key-placeholder" {
			t.Errorf("行 %d 的凭证正文变了：新钥匙解出 %q，旧钥匙解出 %q", id, gotNew, gotOld)
		}
	}
}

// 重跑是幂等的：已经补过的行不会被补上第二份副本。
func TestRotateApplyIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	rotator := NewRotator(db, newRotateSealer(t, newKey, oldKey))

	ids := insertRotateRows(t, db, oldSealer, 3)

	first, err := rotator.Apply(ctx, nil)
	if err != nil {
		t.Fatalf("第一次轮换失败: %v", err)
	}
	if first.Rewrapped != 3 {
		t.Fatalf("第一次补上副本的行数 = %d，期望 3", first.Rewrapped)
	}
	afterFirst := make(map[int64][]byte, len(ids))
	for _, id := range ids {
		afterFirst[id] = readEnvelope(t, db, id)
	}

	second, err := rotator.Apply(ctx, nil)
	if err != nil {
		t.Fatalf("重跑失败: %v", err)
	}
	if second.Rewrapped != 0 {
		t.Errorf("重跑又改动了 %d 行，Rewrap 应当是幂等的", second.Rewrapped)
	}
	if second.Current != 3 {
		t.Errorf("重跑时「本来就含当前主密钥」的行数 = %d，期望 3", second.Current)
	}
	for _, id := range ids {
		envelope := readEnvelope(t, db, id)
		if !bytes.Equal(afterFirst[id], envelope) {
			t.Errorf("重跑给行 %d 又写了一次密文", id)
		}
		// 副本份数没有变成 3：多出来的那一份就是重复轮换留下的垃圾。
		mustHaveKeys(t, envelope, newKey.ID, oldKey.ID)
	}
}

// 一行坏掉只报那一行，不整批中止。
func TestRotateApplyReportsBadEnvelopePerRow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	rotator := NewRotator(db, newRotateSealer(t, newKey, oldKey))

	ids := insertRotateRows(t, db, oldSealer, 2)
	badID := insertChannel(ctx, t, db, oldSealer, channelSpec{
		model:      uniqueModel(),
		credential: []byte("not-an-envelope"),
	})
	badBefore := readEnvelope(t, db, badID)

	report, err := rotator.Apply(ctx, nil)
	if err != nil {
		t.Fatalf("一行坏掉不该让整次轮换失败: %v", err)
	}
	if report.Rewrapped != 2 {
		t.Errorf("补上副本的行数 = %d，期望 2（坏的那一行不该被算进去）", report.Rewrapped)
	}

	badRow, found := findRow(report.Problems, badID)
	if !found {
		t.Fatalf("坏信封 %d 没有被逐行报出：%+v", badID, report.Problems)
	}
	if badRow.Err == nil {
		t.Errorf("行 %d 被报成轮换不了但没给原因", badID)
	}
	if !bytes.Equal(badBefore, readEnvelope(t, db, badID)) {
		t.Errorf("坏行 %d 的密文被改动了", badID)
	}

	for _, id := range ids {
		if row, found := findRow(report.Problems, id); found {
			t.Errorf("好行 %d 也被报成轮换不了：%v", id, row.Err)
		}
		mustHaveKeys(t, readEnvelope(t, db, id), newKey.ID, oldKey.ID)
	}
}

// 本进程没有能解开某一行的钥匙时，干跑就要说出来。
func TestRotatePlanReportsRowsItCannotOpen(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	// 只带当前主密钥，不带旧的那把：旧信封谁也解不开。
	rotator := NewRotator(db, newRotateSealer(t, newKey))

	ids := insertRotateRows(t, db, oldSealer, 2)

	report, err := rotator.Plan(ctx)
	if err != nil {
		t.Fatalf("干跑失败: %v", err)
	}
	if report.Rewrapped != 0 {
		t.Errorf("会被轮换的行数 = %d，期望 0（本进程没有旧钥匙）", report.Rewrapped)
	}
	for _, id := range ids {
		row, found := findRow(report.Problems, id)
		if !found {
			t.Fatalf("行 %d 缺钥匙却没被报出来：%+v", id, report.Problems)
		}
		if !errors.Is(row.Err, ErrNoMasterKey) {
			t.Errorf("行 %d 的错误 = %v，期望 ErrNoMasterKey", id, row.Err)
		}
	}
}

// 互斥：另一处轮换持着锁时，Apply 直接失败，一行都不改。
func TestRotateApplyRefusesConcurrentRun(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	rotator := NewRotator(db, newRotateSealer(t, newKey, oldKey))

	id := insertRotateRows(t, db, oldSealer, 1)[0]

	// 扮演「另一处轮换」：在同一条固定连接上持住那把 advisory lock。
	// 锁是会话级的，所以必须固定在一条连接上——换一条连接就不是同一把锁了。
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	defer func() { _ = holder.Close() }()

	var locked bool
	if lockErr := holder.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock($1)`, rotateLockKey).Scan(&locked); lockErr != nil {
		t.Fatalf("占锁失败: %v", lockErr)
	}
	if !locked {
		t.Fatal("没拿到锁，用例的前提不成立")
	}

	report, applyErr := rotator.Apply(ctx, nil)
	if !errors.Is(applyErr, ErrRotateRunning) {
		t.Fatalf("并发轮换应当返回 ErrRotateRunning，得到 %v", applyErr)
	}
	if report != nil {
		t.Errorf("被挡下时不该返回报告，得到 %+v", report)
	}
	mustHaveKeys(t, readEnvelope(t, db, id), oldKey.ID)

	// 干跑与验证是只读的，不因此失败，但要提示锁被占用。
	plan, planErr := rotator.Plan(ctx)
	if planErr != nil {
		t.Fatalf("干跑失败: %v", planErr)
	}
	if !plan.RotationRunning {
		t.Error("锁被持有时干跑应当提示「另有一处轮换在跑」")
	}

	// 放掉锁之后必须能跑：release 把锁漏在池化连接上会让后续每一次轮换
	// 都报「另一处正在运行」，而那时命令的输出与「真的有人在跑」一模一样。
	if _, unlockErr := holder.ExecContext(ctx,
		`SELECT pg_advisory_unlock($1)`, rotateLockKey); unlockErr != nil {
		t.Fatalf("解锁失败: %v", unlockErr)
	}
	after, afterErr := rotator.Apply(ctx, nil)
	if afterErr != nil {
		t.Fatalf("锁放掉之后轮换仍然失败: %v", afterErr)
	}
	if after.Rewrapped != 1 {
		t.Errorf("锁放掉之后补上副本的行数 = %d，期望 1", after.Rewrapped)
	}
}

// 验证把「还差哪几行」逐行列出来。
func TestRotateVerifyReportsMissingRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	newSealer := newRotateSealer(t, newKey, oldKey)
	rotator := NewRotator(db, newSealer)

	stale := insertRotateRows(t, db, oldSealer, 2)
	fresh := insertRotateRows(t, db, newSealer, 1)[0]

	report, err := rotator.Verify(ctx)
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if report.PrimaryKeyID != newKey.ID {
		t.Errorf("当前主密钥 = %q，期望 %q", report.PrimaryKeyID, newKey.ID)
	}
	if report.Wrapped != 1 {
		t.Errorf("含当前主密钥的行数 = %d，期望 1", report.Wrapped)
	}
	if report.OK() {
		t.Error("还有两行不含当前主密钥，OK 不该为真")
	}
	for _, id := range stale {
		row, found := findRow(report.Missing, id)
		if !found {
			t.Fatalf("行 %d 不含当前主密钥却没进清单：%+v", id, report.Missing)
		}
		if row.Err != nil {
			t.Errorf("行 %d 的信封是好的，不该带错误：%v", id, row.Err)
		}
	}
	if _, found := findRow(report.Missing, fresh); found {
		t.Errorf("行 %d 已经含当前主密钥，不该进清单", fresh)
	}

	if _, applyErr := rotator.Apply(ctx, nil); applyErr != nil {
		t.Fatalf("轮换失败: %v", applyErr)
	}

	after, err := rotator.Verify(ctx)
	if err != nil {
		t.Fatalf("轮换后验证失败: %v", err)
	}
	if after.Wrapped != 3 {
		t.Errorf("轮换后含当前主密钥的行数 = %d，期望 3", after.Wrapped)
	}
	for _, id := range append(append([]int64{}, stale...), fresh) {
		if _, found := findRow(after.Missing, id); found {
			t.Errorf("轮换之后行 %d 仍在缺钥匙清单里", id)
		}
	}
}

// 轮换按批走，一批不超过 BatchSize 行。
func TestRotateApplyBatchesByID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	rotator := NewRotator(db, newRotateSealer(t, newKey, oldKey))
	rotator.BatchSize = 2

	ids := insertRotateRows(t, db, oldSealer, 5)

	var batches []RotateProgress
	report, err := rotator.Apply(ctx, func(p RotateProgress) { batches = append(batches, p) })
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if len(batches) == 0 {
		t.Fatal("一批都没报进度")
	}
	// 断言钉的是 Done/Total 这对量的**语义**（CLI 进度条的百分比），
	// 而不是实现里那次自增：
	//
	//  1. Done 是**累计**行数——第 k 批的 Done 等于到此为止各批 Batch 之和。
	//     写成「本批的行数」（`Done: result.rows`）时进度条永远停在第一批。
	//  2. 收尾时 Done 不小于开始时数到的 Total——小于的话进度条到不了 100%。
	//  3. Total 不小于本用例插进去的行数——它真的是数了一遍表，不是个常量。
	//
	// 不断言 Done == Total：轮换期间别的包会往 channels 里插行，那时 Done
	// 大于 Total 是正常的（见 RotateProgress 的注释），共用库上等式会挂。
	// 也不断言 Done == report.Scanned：两者由同一个累加器喂出来，等式恒成立，
	// 钉不住任何东西（改成 `Done: report.Scanned` 照样绿）。
	cumulative := 0
	for i, p := range batches {
		if p.Batch > 2 {
			t.Errorf("第 %d 批处理了 %d 行，超过 BatchSize 2", i+1, p.Batch)
		}
		if p.Batch == 0 {
			t.Errorf("第 %d 批报了 0 行", i+1)
		}
		if p.Total <= 0 {
			t.Errorf("第 %d 批的总行数 = %d，期望为正", i+1, p.Total)
		}
		cumulative += p.Batch
		if p.Done != int64(cumulative) {
			t.Errorf("第 %d 批的累计行数 = %d，到此为止各批之和 = %d——Done 是累计值，不是本批的行数",
				i+1, p.Done, cumulative)
		}
	}
	last := batches[len(batches)-1]
	if last.Done < last.Total {
		t.Errorf("收尾时累计 %d 小于开始时数到的 %d：进度条到不了 100%%", last.Done, last.Total)
	}
	if last.Total < int64(len(ids)) {
		t.Errorf("开始时数到的总行数 = %d，比本用例插进去的 %d 行还少", last.Total, len(ids))
	}
	if report.Scanned < int64(len(ids)) {
		t.Errorf("报告里扫过 %d 行，比本用例插进去的 %d 行还少", report.Scanned, len(ids))
	}

	for _, id := range ids {
		mustHaveKeys(t, readEnvelope(t, db, id), newKey.ID, oldKey.ID)
	}
}

// 分批扫描要跨过一批的边界：Plan 与 Verify 都按 BatchSize 翻页，只看第一批
// 会漏掉后面的行。把 BatchSize 压到 2，本用例的四行无论落在表的哪个位置，
// 都装不进一批，必然跨批——这条不依赖测试注册顺序或行的先后。
//
// 同时钉住两个计数器的语义：Scanned 必须真的随扫描前进，Current 只数
// **本来就含当前主密钥**的行。
func TestRotatePlanAndVerifyCountEveryBatch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	newSealer := newRotateSealer(t, newKey, oldKey)
	rotator := NewRotator(db, newSealer)
	rotator.BatchSize = 2

	stale := insertRotateRows(t, db, oldSealer, 3)
	fresh := insertRotateRows(t, db, newSealer, 1)[0]
	wantRows := int64(4)

	plan, err := rotator.Plan(ctx)
	if err != nil {
		t.Fatalf("干跑失败: %v", err)
	}
	if plan.Rewrapped != 3 {
		t.Errorf("会被轮换的行数 = %d，期望 3", plan.Rewrapped)
	}
	if plan.Current != 1 {
		t.Errorf("本来就含当前主密钥的行数 = %d，期望 1", plan.Current)
	}
	if plan.Scanned < wantRows {
		t.Errorf("干跑只扫了 %d 行，比本用例插进去的 %d 行还少——分批扫描漏了后面的行",
			plan.Scanned, wantRows)
	}
	for _, id := range stale {
		if row, found := findRow(plan.Problems, id); found {
			t.Errorf("好行 %d 被报成轮换不了：%v", id, row.Err)
		}
	}

	verify, err := rotator.Verify(ctx)
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if verify.Wrapped != 1 {
		t.Errorf("验证时含当前主密钥的行数 = %d，期望 1", verify.Wrapped)
	}
	if verify.Scanned < wantRows {
		t.Errorf("验证只扫了 %d 行，比本用例插进去的 %d 行还少——分批扫描漏了后面的行",
			verify.Scanned, wantRows)
	}
	for _, id := range stale {
		if _, found := findRow(verify.Missing, id); !found {
			t.Errorf("行 %d 不含当前主密钥却没进缺钥匙清单", id)
		}
	}
	if _, found := findRow(verify.Missing, fresh); found {
		t.Errorf("行 %d 已经含当前主密钥，不该进清单", fresh)
	}
}

// 解不开的行，报告里要写出它挂在哪几把钥匙下——那正是排查要比对的差集。
//
// 信封能解析、只是本进程没有能解开它的钥匙：keysOf 必须给出钥匙标识，
// 返回 nil 等于把「缺哪一把」这条线索丢掉。
func TestRotateApplyReportsKeysItCannotOpen(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	oldKey, newKey := rotateTestKeys()
	oldSealer := newRotateSealer(t, oldKey)
	// 只带当前主密钥：旧信封解析得出钥匙标识，但解不开。
	rotator := NewRotator(db, newRotateSealer(t, newKey))

	ids := insertRotateRows(t, db, oldSealer, 2)

	report, err := rotator.Apply(ctx, nil)
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if report.Rewrapped != 0 {
		t.Errorf("补上副本的行数 = %d，期望 0（本进程没有旧钥匙）", report.Rewrapped)
	}
	for _, id := range ids {
		row, found := findRow(report.Problems, id)
		if !found {
			t.Fatalf("行 %d 缺钥匙却没被报出来：%+v", id, report.Problems)
		}
		if !containsKey(row.Keys, oldKey.ID) {
			t.Errorf("行 %d 的问题清单里挂着 %v，期望含 %q——缺哪把钥匙是排查要比对的差集",
				id, row.Keys, oldKey.ID)
		}
	}
}

// 解锁成功时连接只是还回池里，不该被当成坏连接丢掉。
//
// release 只在解锁**失败**时把连接标坏（锁可能还留在会话上，换一条连接继续
// 用会让后续轮换误判「另一处正在运行」）。判据反过来之后，每次成功的轮换都
// 会白丢一条池化连接——行为上只是慢一点，所以除了看池子本身没有别的观察口。
// 这条用例独占一个句柄：只有一条连接时，丢没丢直接体现在 OpenConnections 上。
func TestRotateReleaseKeepsHealthyConnection(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	lock, err := acquireRotateLock(ctx, db)
	if err != nil {
		t.Fatalf("拿锁失败: %v", err)
	}
	if got := db.Stats().OpenConnections; got != 1 {
		t.Fatalf("持锁时的连接数 = %d，期望 1（这条用例需要一个只开过一条连接的句柄）", got)
	}

	lock.release(ctx)

	if got := db.Stats().OpenConnections; got != 1 {
		t.Errorf("解锁成功之后的连接数 = %d，期望 1——健康的连接应当还回池里，不该被标成坏连接丢弃", got)
	}
}
