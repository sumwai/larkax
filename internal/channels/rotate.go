package channels

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/credential"
)

// DefaultRotateBatch 是轮换每批处理的行数。
//
// 分批而不是把全表装进一个事务：事务越大，回滚段与锁的持有时间越长，
// 而轮换不需要「一次要么全成要么全不成」这个保证——它是幂等的，可以中断重跑。
const DefaultRotateBatch = 500

// rotateLockKey 是轮换互斥用的 advisory lock 键。
//
// advisory lock 的键空间是全局的，随手取一个整数有与别的功能撞上的可能。
// 这个值取自 "larkax" 六个字节的 ASCII（6c 61 72 6b 61 78）：有出处，
// 不会与手写的常量撞车。
const rotateLockKey int64 = 0x6c61726b6178

// ErrRotateRunning 表示另一处轮换正持着互斥锁。
//
// 不排队等锁：轮换是运维手动触发的动作，等一把不知道还要多久的锁，
// 不如立刻报出来让人去看那一处跑到哪一步了。
var ErrRotateRunning = errors.New("另一处轮换正在运行")

// RotateRow 是逐行报告里的一行。
type RotateRow struct {
	// ID 是 channels.id。
	ID int64

	// Keys 是这一行当前挂着的主密钥标识，见 credential.WrappedWith。
	// 密文解析失败时为空。
	Keys []string

	// Err 说明这一行为什么没被处理：密文不合法、本进程没有能解开它的钥匙，
	// 或者它在读取与写回之间被别的写入改掉了。
	// 为 nil 表示这一行只是「还不含当前主密钥」，没有出错。
	Err error
}

// RotateReport 是一次轮换或干跑的结果。
type RotateReport struct {
	// DryRun 说明这一轮有没有真的写库。
	DryRun bool

	// PrimaryKeyID 是本进程当前用于包装新数据密钥的主密钥标识。
	PrimaryKeyID string

	// RotationRunning 说明扫描开始时另有一处轮换持着互斥锁。
	// 干跑是只读的，不因此失败，但读到的可能不是终态。
	RotationRunning bool

	// Scanned 是扫过的行数。
	Scanned int64

	// Rewrapped 是补上了当前主密钥的行数。干跑时是「会被补上」的行数。
	Rewrapped int64

	// Current 是本来就含当前主密钥、不需要改动的行数。
	Current int64

	// Problems 逐行列出轮换不了的行：密文不合法，或本进程没有能解开它的钥匙。
	// 一行坏掉不会让整次轮换作废，但也不会被静默跳过。
	Problems []RotateRow

	// Concurrent 逐行列出在读取与写回之间被别的写入改掉、因而本轮跳过的行。
	//
	// 跳过不等于那一行还是旧的：改写它的那一方可能刚写进一个含当前主密钥的
	// 信封。所以「还有没有漏网的」要看 Verify 的结论，不是这里的条数。
	Concurrent []RotateRow
}

// VerifyReport 是全库验证的结果。
//
// 它是「旧主密钥能不能退役」的判据：Missing 为空才说明每一行都被当前
// 主密钥包着。只看命令的退出码是不够的——退出码只说明这一次扫描跑完了。
type VerifyReport struct {
	// PrimaryKeyID 是本进程当前用于包装新数据密钥的主密钥标识。
	PrimaryKeyID string

	// RotationRunning 说明扫描开始时另有一处轮换持着互斥锁。
	// 那时清单里的行可能只是「还没轮到」，不代表轮换收尾失败。
	RotationRunning bool

	// Scanned 是扫过的行数。
	Scanned int64

	// Wrapped 是已经含当前主密钥的行数。
	Wrapped int64

	// Missing 逐行列出**不含**当前主密钥的行，包括密文解析失败的那些。
	Missing []RotateRow
}

// OK 表示全库每一行都含当前主密钥。
func (r *VerifyReport) OK() bool { return len(r.Missing) == 0 }

// RotateProgress 是 Apply 每处理完一批交出的进度。
type RotateProgress struct {
	// LastID 是本批最后一行的 id，也是下一批的起点。
	LastID int64

	// Batch 是本批读到的行数；Rewrapped、Current、Concurrent、Failed 是其中
	// 四种处置各自的条数。
	Batch      int
	Rewrapped  int
	Current    int
	Concurrent int
	Failed     int

	// Done 是累计处理过的行数，Total 是开始时数到的总行数。
	// 两者只用于显示：轮换期间有人新建渠道时，它们对不上是正常的。
	Done  int64
	Total int64
}

// Rotator 对 channels.credential_enc 做主密钥轮换。
//
// # 它只重包数据密钥，不解密凭证正文
//
// 每个信封里都有一份用某把主密钥包住的数据密钥（DEK），正文是用 DEK 加密的。
// 轮换要做的是「给每个信封再包一份当前主密钥的副本」，所以本类型调用的是
// credential.Sealer.Rewrap，**从不调用 Open**：明文凭证在这里根本不存在。
//
// # 幂等
//
// Rewrap 对已经含当前主密钥的信封返回原样，所以中断之后重跑是安全的——
// 已经轮换过的行不会被补上第二份副本。进度不需要另行记录，跑完用 Verify 确认。
type Rotator struct {
	db     *sql.DB
	sealer *credential.Sealer

	// BatchSize 是每批处理的行数，0 表示 DefaultRotateBatch。
	BatchSize int
}

// NewRotator 包装数据库句柄与主密钥封装器。
//
// 它不 import 也不持有 Source 或 Admin：三者读的是同一张表，但轮换是
// 独立的一次性运维动作，混进请求路径或管理面只会让那两处的职责变模糊。
func NewRotator(db *sql.DB, sealer *credential.Sealer) *Rotator {
	return &Rotator{db: db, sealer: sealer}
}

// Plan 干跑：扫一遍全表，报告有多少行会被改动、哪些行改不了。**不写库。**
//
// 判据全部来自信封头部（credential.WrappedWith），不需要钥匙材料，
// 也不解密正文。
//
// 「能轮换」是按钥匙标识**预判**的：密文里只要有一份副本挂在本进程持有的
// 主密钥下，就认为能重包。标识对得上而那份副本其实解不开（密文被改过）的行，
// 要等 Apply 才会暴露——那一行会被逐行报出来，不是静默跳过。
func (r *Rotator) Plan(ctx context.Context) (*RotateReport, error) {
	if validateErr := r.validate(); validateErr != nil {
		return nil, validateErr
	}

	report := &RotateReport{
		DryRun:          true,
		PrimaryKeyID:    r.sealer.PrimaryKeyID(),
		RotationRunning: rotateLockHeld(ctx, r.db),
	}
	held := r.heldKeyIDs()
	primary := report.PrimaryKeyID

	scanErr := r.eachRow(ctx, func(id int64, envelope []byte) error {
		report.Scanned++

		keys, keyErr := credential.WrappedWith(envelope)
		if keyErr != nil {
			report.Problems = append(report.Problems, RotateRow{ID: id, Err: keyErr})
			return nil
		}
		if containsKey(keys, primary) {
			report.Current++
			return nil
		}
		if !intersects(keys, held) {
			// 这一行只能靠一把本进程没有的钥匙打开，轮换不了。
			// 报出来而不是跳过：它意味着环境变量里的主密钥少了一把，
			// 而缺的那一把该补回来，不是这一行该被丢掉。
			// 报错里带上本进程**有**哪几把：排查的人要比对的正是这个差集。
			report.Problems = append(report.Problems, RotateRow{
				ID:   id,
				Keys: keys,
				Err:  fmt.Errorf("%w：本进程只有 %v", ErrNoMasterKey, r.sealer.KeyIDs()),
			})
			return nil
		}
		report.Rewrapped++
		return nil
	})
	if scanErr != nil {
		// 与 Apply 不同，这里不返回半截报告：干跑的数一旦不完整，
		// 「有多少行要改」就失去意义，而重跑一次干跑的成本很低。
		return nil, scanErr
	}
	return report, nil
}

// Apply 逐行轮换：给每个信封补上当前主密钥的数据密钥副本。
//
// progress 可以为 nil；不为 nil 时每批调用一次，用来打进度。
//
// # 中断与重跑
//
// 每批一个事务，批与批之间互相独立：中断时已提交的批次留在库里，未提交的
// 那一批整体不生效。因为 Rewrap 对已经含当前主密钥的信封返回原样，重跑是
// 幂等的——已经轮换过的行不会被补上第二份副本，也不会被重新包一遍。
// 所以中断之后直接再跑一次即可，不需要记录进度。
//
// # 互斥
//
// 全程持有一把 PostgreSQL 会话级 advisory lock，拿不到就返回 ErrRotateRunning。
// 轮换期间所有语句都走持锁的那条连接——会话级的锁挂在连接上，用连接池里
// 随手挑的连接发语句会让锁形同虚设。
//
// # 出错时的返回
//
// 返回的报告即使带着错误也是有效的：跑到哪一批、哪些行失败都在里面。
// 调用方应当先打报告再看错误。
func (r *Rotator) Apply(ctx context.Context, progress func(RotateProgress)) (*RotateReport, error) {
	if validateErr := r.validate(); validateErr != nil {
		return nil, validateErr
	}

	lock, lockErr := acquireRotateLock(ctx, r.db)
	if lockErr != nil {
		return nil, lockErr
	}
	defer lock.release(ctx)

	total, countErr := countChannels(ctx, lock.conn)
	if countErr != nil {
		return nil, countErr
	}

	report := &RotateReport{PrimaryKeyID: r.sealer.PrimaryKeyID()}
	var lastID int64
	for {
		result, batchErr := r.applyBatch(ctx, lock.conn, lastID)
		if batchErr != nil {
			return report, batchErr
		}
		if result.rows == 0 {
			return report, nil
		}

		report.Scanned += int64(result.rows)
		report.Rewrapped += int64(result.rewrapped)
		report.Current += int64(result.current)
		report.Problems = append(report.Problems, result.problems...)
		report.Concurrent = append(report.Concurrent, result.concurrent...)

		lastID = result.lastID
		// Done 是**累计**处理过的行数，与 report.Scanned 是同一个量。
		// 早先这里另有一个 done 累加器，在同一个循环体里与 report.Scanned
		// 各加一次 result.rows，中间没有任何语句能改动它——两者必须永远
		// 相等，却没有任何东西保证这一点。进度条要的就是这个量本身，
		// 不是它的第二份副本。
		if progress != nil {
			progress(RotateProgress{
				LastID:     lastID,
				Batch:      result.rows,
				Rewrapped:  result.rewrapped,
				Current:    result.current,
				Concurrent: len(result.concurrent),
				Failed:     len(result.problems),
				Done:       report.Scanned,
				Total:      total,
			})
		}
	}
}

// Verify 扫一遍全表，报告每一行是否都含当前主密钥。
//
// 它是轮换收尾的判据，也是退役旧主密钥的前置条件。逐行列出不含的行，
// 而不是只给一个条数：漏网的行要能被定位到，否则「还差几行」没法处置。
func (r *Rotator) Verify(ctx context.Context) (*VerifyReport, error) {
	if validateErr := r.validate(); validateErr != nil {
		return nil, validateErr
	}

	report := &VerifyReport{
		PrimaryKeyID:    r.sealer.PrimaryKeyID(),
		RotationRunning: rotateLockHeld(ctx, r.db),
	}
	scanErr := r.eachRow(ctx, func(id int64, envelope []byte) error {
		report.Scanned++

		keys, keyErr := credential.WrappedWith(envelope)
		if keyErr != nil {
			// 解析不了的信封当然也证明不了它含当前主密钥，所以它进 Missing。
			report.Missing = append(report.Missing, RotateRow{ID: id, Err: keyErr})
			return nil
		}
		if containsKey(keys, report.PrimaryKeyID) {
			report.Wrapped++
			return nil
		}
		report.Missing = append(report.Missing, RotateRow{ID: id, Keys: keys})
		return nil
	})
	if scanErr != nil {
		return nil, scanErr
	}
	return report, nil
}

// ── 内部实现 ────────────────────────────────────────────────────────────

// batchResult 是一批的处理结果。
type batchResult struct {
	lastID     int64 // 本批最后一行的 id；本批为空时保持调用方给的值
	rows       int
	rewrapped  int
	current    int
	problems   []RotateRow
	concurrent []RotateRow
}

// applyBatch 处理一批：读到 afterID 之后的 BatchSize 行，逐行重包并写回。
//
// 一批一个事务：批与批之间互相独立，中途中断时已提交的批次留在库里，
// 未提交的那一批整体不生效——「一半新一半旧」只可能出现在批与批之间，
// 而那个范围由 Verify 看得见。
func (r *Rotator) applyBatch(ctx context.Context, conn *sql.Conn, afterID int64) (*batchResult, error) {
	tx, txErr := conn.BeginTx(ctx, nil)
	if txErr != nil {
		return nil, fmt.Errorf("开启事务失败: %w", txErr)
	}
	// 回滚只在没提交时做，且它的错误不覆盖真正的失败原因：提交失败时
	// 事务已经不可用，再报一条回滚错误只会把原因埋掉。
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 先把整批读进内存再写：同一条连接上不能一边遍历游标一边发 UPDATE。
	// 一批的行数有上界，装得下。
	type row struct {
		id       int64
		envelope []byte
	}
	var batch []row
	lastID, scanned, scanErr := scanBatch(ctx, tx, afterID, r.batchSize(), func(id int64, envelope []byte) error {
		batch = append(batch, row{id: id, envelope: envelope})
		return nil
	})
	if scanErr != nil {
		return nil, scanErr
	}

	result := &batchResult{lastID: lastID, rows: scanned}
	for _, item := range batch {
		rewrapped, rewrapErr := r.sealer.Rewrap(item.envelope)
		if rewrapErr != nil {
			result.problems = append(result.problems, RotateRow{
				ID:   item.id,
				Keys: keysOf(item.envelope),
				Err:  rewrapErr,
			})
			continue
		}
		if bytes.Equal(rewrapped, item.envelope) {
			// 已经含当前主密钥，Rewrap 原样返回。这一条就是重跑幂等的兑现点。
			result.current++
			continue
		}

		res, execErr := tx.ExecContext(ctx, rotateUpdateQuery, item.id, rewrapped, item.envelope)
		if execErr != nil {
			return nil, fmt.Errorf("改写渠道 %d 的凭证失败: %w", item.id, execErr)
		}
		affected, affectedErr := res.RowsAffected()
		if affectedErr != nil {
			return nil, fmt.Errorf("确认渠道 %d 的改写结果失败: %w", item.id, affectedErr)
		}
		if affected == 0 {
			// 读取与写回之间这一行被别的写入改掉了。带条件的 UPDATE 让它落不
			// 下去，而不是把对方的改动盖掉——那可能是一份刚换上去的新凭证，
			// 盖掉它就是静默地丢了一次运维动作。
			result.concurrent = append(result.concurrent, RotateRow{ID: item.id, Keys: keysOf(item.envelope)})
			continue
		}
		result.rewrapped++
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return nil, fmt.Errorf("提交本批失败: %w", commitErr)
	}
	committed = true
	return result, nil
}

// eachRow 按 id 顺序分批遍历 channels 表，对每一行调用 fn。
//
// 分批而不是一次把全表读进内存：凭证密文一行几百字节，渠道数通常不大，
// 但「现在不大」不是不留余量的理由，而按 id 前进的分页写法并不更复杂。
func (r *Rotator) eachRow(ctx context.Context, fn func(id int64, envelope []byte) error) error {
	var afterID int64
	for {
		lastID, scanned, scanErr := scanBatch(ctx, r.db, afterID, r.batchSize(), fn)
		if scanErr != nil {
			return scanErr
		}
		if scanned == 0 {
			return nil
		}
		afterID = lastID
	}
}

// queryer 是分批扫描要用的最小接口。*sql.DB、*sql.Conn 与 *sql.Tx 都满足它。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scanBatch 取 afterID 之后的一批行，逐行交给 fn，返回本批最后一行的 id 与行数。
//
// fn 返回错误时整批中止，错误原样上抛——调用方（eachRow 与 applyBatch）
// 用它表达「这一行本身有问题」以外的失败。逐行的坏数据由调用方在 fn 里
// 收进报告，不走这条路径。
func scanBatch(ctx context.Context, q queryer, afterID int64, limit int, fn func(id int64, envelope []byte) error) (int64, int, error) {
	rows, queryErr := q.QueryContext(ctx, rotateBatchQuery, afterID, limit)
	if queryErr != nil {
		return 0, 0, fmt.Errorf("查询渠道失败: %w", queryErr)
	}
	defer func() { _ = rows.Close() }()

	var (
		lastID  int64
		scanned int
	)
	for rows.Next() {
		var (
			id       int64
			envelope []byte
		)
		if scanErr := rows.Scan(&id, &envelope); scanErr != nil {
			return 0, 0, fmt.Errorf("读取渠道行失败: %w", scanErr)
		}
		if fnErr := fn(id, envelope); fnErr != nil {
			return 0, 0, fnErr
		}
		lastID = id
		scanned++
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return 0, 0, fmt.Errorf("遍历渠道失败: %w", rowsErr)
	}
	return lastID, scanned, nil
}

// rotateLock 是一次持有中的轮换互斥锁。
//
// 用会话级 advisory lock 而不是事务级：轮换跨多个事务（每批一个），
// 事务级的锁在第一批提交时就释放了，互斥随之失效。
//
// 会话级的锁挂在**连接**上，所以持锁期间的所有语句都必须走同一条连接——
// database/sql 会从池里挑连接，锁在 A 上而语句落到 B 上是完全可能的，
// 那时互斥名存实亡，而命令照常打印「完成」。
type rotateLock struct {
	conn *sql.Conn
}

// acquireRotateLock 拿一把轮换互斥锁，拿不到就返回 ErrRotateRunning。
func acquireRotateLock(ctx context.Context, db *sql.DB) (*rotateLock, error) {
	conn, connErr := db.Conn(ctx)
	if connErr != nil {
		return nil, fmt.Errorf("获取数据库连接失败: %w", connErr)
	}

	var got bool
	if lockErr := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, rotateLockKey).Scan(&got); lockErr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("获取轮换互斥锁失败: %w", lockErr)
	}
	if !got {
		_ = conn.Close()
		return nil, ErrRotateRunning
	}
	return &rotateLock{conn: conn}, nil
}

// release 解锁并把连接还给连接池。
//
// 必须显式解锁：Conn.Close 是把连接**还回池子**，不是关掉会话——靠它释放锁
// 会让锁留在这条池化连接上，直到这条连接被回收为止，表现为后续每一次轮换
// 都报「另一处正在运行」。
//
// 解锁走 context.WithoutCancel：取消信号恰恰在「跑得太久」时到来，而那时
// 更需要把锁放掉，所以这一步不该跟着调用方的取消走。解锁失败时把连接标成坏的，
// 让池子丢弃它（Raw 返回 driver.ErrBadConn 就是这个意思），锁随会话结束释放。
func (l *rotateLock) release(ctx context.Context) {
	if _, unlockErr := l.conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, rotateLockKey); unlockErr != nil {
		_ = l.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = l.conn.Close()
}

// rotateLockHeld 报告是否已有另一处轮换持着互斥锁。
//
// 探法就是试着拿一次再立刻放掉：advisory lock 没有「现在是谁持着」的可靠读法
// （pg_locks 里那个键被拆成两半，读法依赖实现细节）。探测本身是幂等的，
// 拿不到就说明有人持着。
//
// 它是**提示**，不是判据：拿到的瞬间另一处可能刚好开始。真正的互斥在
// acquireRotateLock 里，那里拿不到锁就直接失败。探测本身失败（连不上库、
// 权限不足）按「没有在跑」处理：这个提示不该让一条只读命令报错。
func rotateLockHeld(ctx context.Context, db *sql.DB) bool {
	lock, lockErr := acquireRotateLock(ctx, db)
	if lockErr != nil {
		return errors.Is(lockErr, ErrRotateRunning)
	}
	lock.release(ctx)
	return false
}

// countChannels 数一下全表有多少行，只用来给进度一个分母。
//
// 数出来的值与实际扫过的行数可能差一点（轮换期间有人新建渠道），
// 所以它只出现在进度里，不参与任何判断。
func countChannels(ctx context.Context, conn *sql.Conn) (int64, error) {
	var total int64
	if countErr := conn.QueryRowContext(ctx, `SELECT count(*) FROM channels`).Scan(&total); countErr != nil {
		return 0, fmt.Errorf("统计渠道数失败: %w", countErr)
	}
	return total, nil
}

// validate 确认这个 Rotator 能用。
func (r *Rotator) validate() error {
	if r.db == nil {
		return errors.New("没有数据库句柄")
	}
	if r.sealer == nil {
		// 与读取路径同一个错误：两种情况要处置的都是「这台机器没配主密钥」。
		return fmt.Errorf("%w：本进程没有配置主密钥", ErrNoMasterKey)
	}
	return nil
}

func (r *Rotator) batchSize() int {
	if r.BatchSize > 0 {
		return r.BatchSize
	}
	return DefaultRotateBatch
}

// heldKeyIDs 返回本进程持有的主密钥标识集合。
func (r *Rotator) heldKeyIDs() map[string]bool {
	ids := r.sealer.KeyIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// containsKey 判断标识清单里有没有 id。
func containsKey(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// intersects 判断标识清单与集合有没有交集。
func intersects(ids []string, set map[string]bool) bool {
	for _, id := range ids {
		if set[id] {
			return true
		}
	}
	return false
}

// keysOf 尽力取出一份密文挂在哪几把主密钥下，用于逐行报告。
// 解析失败时返回 nil——那正是这一行要被报出来的原因，不必再叠一层错误。
func keysOf(envelope []byte) []string {
	keys, err := credential.WrappedWith(envelope)
	if err != nil {
		return nil
	}
	return keys
}

// rotateBatchQuery 按 id 顺序取一批渠道。
//
// 用 keyset 分页（id > 上一批的最后一个）而不是 OFFSET：轮换会改写前面的行，
// OFFSET 的分页边界随写而漂移，会漏掉或重复某些行。按 id 前进则不会——
// 每一行要么在本批被处理，要么在更早的批次里处理过。
const rotateBatchQuery = `SELECT id, credential_enc FROM channels WHERE id > $1 ORDER BY id LIMIT $2`

// rotateUpdateQuery 写回补过副本的密文。
//
// 带 credential_enc = $3 这个条件是有意的（乐观并发）：本命令读到写回之间
// 若有人改了这一行的凭证，这次 UPDATE 会匹配不到行，而不是把对方的改动盖掉。
// 被跳过的那一行进 Concurrent 清单，最终由 Verify 判它到底补上没有。
//
// 不动 updated_at：这一列的含义是「这条渠道的配置什么时候改过」，而轮换
// 既没有改配置也没有改凭证——它只是让同一份密文多挂一把钥匙。轮换一次就把
// 全库渠道的 updated_at 推到同一天，会把这一列上「谁改了什么」的信息冲掉。
const rotateUpdateQuery = `UPDATE channels SET credential_enc = $2 WHERE id = $1 AND credential_enc = $3`
