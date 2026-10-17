package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件维护 usage_events 的**月分区**。
//
// # 为什么需要它
//
// usage_events 按 created_at 做 RANGE 分区（见迁移 00001），而迁移里只铺了
// 两个种子分区加一个兜底分区 usage_events_default。**没有任何东西会预建后续
// 月份的分区**，于是过了种子覆盖的月份之后，每一行用量事件都落进兜底分区：
// 不报错、查询照常返回正确的行，只是这张表退化成「所有数据堆在一个分区里」。
//
// 兜底分区里的行不是「暂时放一放」。实测（PostgreSQL 16）：一旦有行落进
// usage_events_default，补建那个月的分区会**直接失败**——
//
//	ERROR: updated partition constraint for default partition "usage_events_default"
//	       would be violated by some row        (SQLSTATE 23514)
//
// 也就是说兜底分区里的行挡着那个月的分区建不出来，要人工先把行搬走。
// 所以写入侧也必须保证分区存在，不能指望「有兜底分区，先落进去也没事」。
//
// # 谁调它
//
// 两个调用方共用这一份实现，不是两套机制：
//
//   - 写入侧：Store.Save 在开事务之前保证**当月**那一个分区存在
//     （见 Store.ensureUsageEventPartition）
//   - 运维侧：`larkaxctl partition ensure` 一次铺满未来若干个月，
//     由外部调度器调用（见 docs/partitions.md）
//
// # 另一张分区表不在这里
//
// request_snapshots 按天分区，由写入路径自己按天补建
// （见 internal/snapshotwire/store.go）。它的粒度、留存期与兜底策略都不同，
// 合并进来只会让两套口径互相迁就。

// Querier 是分区维护需要的最小数据库能力。
//
// 只声明这两个方法：*sql.DB 与 *sql.Tx 都满足它，于是同一份实现既能跑在
// 写入侧（事务之外，见 Store.ensureUsageEventPartition 里「为什么必须在事务
// 之外」），也能跑在命令行里（拿到的就是 *sql.DB）。
//
// 不声明 QueryRowContext：本文件一次「查一行」都没有，多声明一个方法只会
// 把将来某个只想传窄接口的调用方挡在外面。
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// 分区名的形状：前缀 + YYYY_MM。
//
// 用 UTC 月份：分区的边界由服务器时区决定，而部署地可能不在同一个时区。
// 统一按 UTC 命名，让「哪个月」这件事只有一个答案——与 internal/snapshotwire
// 按 UTC 给分区命名的理由相同。
const (
	usageEventPartitionPrefix = "usage_events_"
	usageEventPartitionLayout = "2006_01"
)

// UsageEventDefaultPartition 是 usage_events 的兜底分区名。
//
// 它按设计存在于 schema 里（迁移 00001 建的），不是「不认识的分区名」，
// 所以它非空是**告警**而不是错误：说明预建任务漏跑了，而落进去的行没有
// 到期日、也没有「按分区整块删除」可言。
//
// 导出是因为 `larkaxctl partition check` 要按名字统计它的行数。
const UsageEventDefaultPartition = "usage_events_default"

// UsageEventPartitionName 是某个月对应的分区表名。
func UsageEventPartitionName(t time.Time) string {
	return usageEventPartitionPrefix + t.UTC().Format(usageEventPartitionLayout)
}

// UsageEventMonthBounds 返回某个月分区的上下界，左闭右开。
//
// 与分区 DDL 的 FOR VALUES 一致（形如 `FROM ('2026-10-01T00:00:00Z')
// TO ('2026-11-01T00:00:00Z')`）。边界按 UTC 算出，与 UsageEventPartitionName
// 同源——两处若各算一次，跨时区部署时分区名与边界会错开一个月，而那种错误
// 不会报错。
//
// 用 time.Date 与 AddDate 逐月推进，不用 `Add(30*24*time.Hour)` 那类近似：
// 月份长度不等（28~31 天），近似法会累积漂移，几个季度之后边界就落到别的
// 月份里去了。那种错误同样不报错——只是分区覆盖的区间与它的名字不再对应。
func UsageEventMonthBounds(t time.Time) (from, to time.Time) {
	u := t.UTC()
	from = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

// UsageEventPartitionMonth 解析分区名里的月份。
//
// 名字不符合 `usage_events_YYYY_MM` 时返回 false——调用方据此跳过，而不是猜
// 一个月份出来。返回的时间是那个月的 UTC 一号零点。
//
// # 长度检查在这里是不够的
//
// `usage_events_default` 与 `usage_events_YYYY_MM` **长度恰好相同**（13 + 7
// 与 13 + 7）。所以兜底分区能通过前缀与长度两道检查，是靠 time.Parse 认不出
// `default` 才被判掉的。少最后那一步，兜底分区会被当成一个合法的月分区。
func UsageEventPartitionMonth(name string) (time.Time, bool) {
	if len(name) != len(usageEventPartitionPrefix)+len(usageEventPartitionLayout) {
		return time.Time{}, false
	}
	if name[:len(usageEventPartitionPrefix)] != usageEventPartitionPrefix {
		return time.Time{}, false
	}
	month, err := time.Parse(usageEventPartitionLayout, name[len(usageEventPartitionPrefix):])
	if err != nil {
		return time.Time{}, false
	}
	return month, true
}

// listUsageEventPartitionsSQL 列出 usage_events 下的全部分区。
//
// 走系统目录而不是 `information_schema`：后者在分区表上给的是分区自己的
// 表名与它继承来的列，要拼出「谁是 usage_events 的分区」得再绕一圈。
//
// LIKE 只按前缀筛，兜底分区也会被它选中——那一条由 UsageEventPartitionMonth
// 的形状检查排掉，不在这里用 `!= 'usage_events_default'` 特判：特判只挡得住
// 兜底分区这一个名字，而形状检查挡得住所有「名字不像月分区」的东西。
const listUsageEventPartitionsSQL = `
	SELECT c.relname
	  FROM pg_class c
	  JOIN pg_inherits i ON i.inhrelid = c.oid
	  JOIN pg_class p ON p.oid = i.inhparent
	 WHERE p.relname = 'usage_events'
	   AND c.relname LIKE 'usage_events\_%'`

// usageEventMonths 是「应当存在哪些月份」的唯一口径。
//
// 返回 from 所在的那个月起、连续 months 个月，按时间升序。
//
// months <= 0 报错而不是当空操作：`--months 0` 多半是调度器里的变量没填上，
// 静默地什么都不建会让「预建任务在跑」与「预建任务建了 0 个分区」看起来一样，
// 而后者正是这个文件要消灭的那种静默。这与 internal/snapshotwire 的
// expiredNames 对留存天数 <= 0 的处置一致。
func usageEventMonths(from time.Time, months int) ([]time.Time, error) {
	if months <= 0 {
		return nil, fmt.Errorf("月份数必须为正数，当前为 %d", months)
	}
	start, _ := UsageEventMonthBounds(from)
	out := make([]time.Time, 0, months)
	for i := range months {
		// 起点是一号，AddDate 逐月加不会遇到「这个月没有 31 号」的归一日。
		out = append(out, start.AddDate(0, i, 0))
	}
	return out, nil
}

// existingUsageEventPartitions 读出当前已存在的 usage_events 分区名集合。
func existingUsageEventPartitions(ctx context.Context, q Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, listUsageEventPartitionsSQL)
	if err != nil {
		return nil, fmt.Errorf("列出 usage_events 分区失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取分区名失败: %w", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分区名: %w", err)
	}
	return names, nil
}

// MissingUsageEventPartitions 只读，列出应当存在而缺失的分区名。
//
// 返回的名字按时间升序。一个都不缺时返回 nil（`len(missing) == 0` 是调用方
// 该用的判据，不要与 nil 比较）。
//
// 它是 `larkaxctl partition check` 的判据之一，也是 EnsureUsageEventPartitions
// 的第一步——两处共用同一份「应当存在」与「已经存在」的口径，于是「体检说缺
// 三个」与「预建建了三个」不可能给出不同的答案。
func MissingUsageEventPartitions(ctx context.Context, q Querier, from time.Time, months int) ([]string, error) {
	monthsList, err := usageEventMonths(from, months)
	if err != nil {
		return nil, err
	}
	existing, err := existingUsageEventPartitions(ctx, q)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, m := range monthsList {
		name := UsageEventPartitionName(m)
		if !existing[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// EnsureUsageEventPartitions 建出缺失的 usage_events 月分区。
//
// from 所在的那个月到 from 之后 months-1 个月，逐月建，已存在的跳过。
// 返回本次**真正建出来**的分区名（按时间升序）；一个都没建时返回 nil，
// 调用方按 `len(created) == 0` 判断，不要与 nil 比较。
//
// months <= 0 返回错误（口径见 usageEventMonths）。
//
// # 幂等与并发
//
// 建表语句带 IF NOT EXISTS，所以重复调用是安全的。但 `IF NOT EXISTS` 在
// PostgreSQL 上**不是完全并发安全的**：两个进程同时发现缺同一个分区并同时
// 建它，其中一个会收到目录唯一键的冲突（42P07 duplicate_table 或
// 23505 unique_violation），而不是安静地跳过。
//
// 这里选择**认出这两个错误码并当成「别人建好了」**，而不是用
// pg_advisory_xact_lock 串行化。理由是加锁要一个事务，而写入侧的调用点
// **必须在事务之外**（见 Store.ensureUsageEventPartition）：为了加锁去开一个
// 事务，等于把一次 DDL 的锁持有期从「那一条语句」拉长到「整个事务」。
//
// 这两个码只可能来自「这个名字已经被占了」，所以认它们不会放过真正的失败：
// 兜底分区挡着建不出来是 23514（check_violation），不在这个集合里，
// 会照常作为错误返回。
//
// # 出错时的返回
//
// 已经建出来的那几个照常返回，连同错误一起。调用方拿到部分结果可以照实
// 报告「建了哪几个、卡在哪一个」，而下一次调用会从缺的那个接着建。
func EnsureUsageEventPartitions(ctx context.Context, q Querier, from time.Time, months int) ([]string, error) {
	missing, err := MissingUsageEventPartitions(ctx, q, from, months)
	if err != nil {
		return nil, err
	}

	var created []string
	for _, name := range missing {
		month, ok := UsageEventPartitionMonth(name)
		if !ok {
			// 走不到：name 是 UsageEventPartitionName 刚算出来的。
			// 留着是为了让「这里假设名字一定合法」这个前提有个出口，
			// 而不是在下面静默地用一个零值时间。
			return created, fmt.Errorf("内部错误：分区名 %q 解析不出月份", name)
		}
		if err := createUsageEventPartition(ctx, q, name, month); err != nil {
			return created, err
		}
		created = append(created, name)
	}
	return created, nil
}

// createUsageEventPartition 建一个月的分区，已存在时什么都不做。
func createUsageEventPartition(ctx context.Context, q Querier, name string, month time.Time) error {
	from, to := UsageEventMonthBounds(month)

	// 标识符不能参数化，所以分区名是拼进 SQL 的。它由固定前缀与 UTC 月份
	// 拼成——UsageEventPartitionName 只产出 [a-z0-9_]，两个边界是 RFC3339
	// 格式的时间串——没有任何外部输入能进到这里，没有注入面。
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_events FOR VALUES FROM ('%s') TO ('%s')`,
		name, from.Format(time.RFC3339), to.Format(time.RFC3339),
	)
	if _, err := q.ExecContext(ctx, stmt); err != nil {
		if isPartitionAlreadyExists(err) {
			// 另一个进程刚建好。见 EnsureUsageEventPartitions 的注释。
			return nil
		}
		return fmt.Errorf("建分区 %s 失败: %w", name, err)
	}
	return nil
}

// ensureUsageEventPartition 保证**当月**的 usage_events 分区存在。
//
// 它是 Save 在开事务之前调的那一步，形状照 internal/snapshotwire/store.go
// 的 ensurePartition（那张表按天，所以它记到天，这里记到月）。
//
// # 为什么必须在事务之外
//
// `CREATE TABLE ... PARTITION OF` 会拿父表 usage_events 的 ACCESS EXCLUSIVE
// 锁。放在结算事务里，那把锁会一直持有到提交——而锁的是**整张 usage_events**，
// 于是所有并发的结算、以及任何读这张表的查询，都要排队等这次结算提交完。
// 放在 BeginTx 之前，锁的持有期就只有那一条语句本身。
//
// # 为什么要有进程内缓存
//
// 没有缓存就是**每个结算一次 DDL**——把一个「每月一次」的动作放进最热的
// 那条路，代价是每次结算都去查一次系统目录，偶尔还要拿一把排它锁。
// 缓存记的是「哪个月已经确认过」，跨月之后自然会重新确认一次。
//
// # 失败为什么不返回错误
//
// 分区建不出来时这一行仍然会被写进兜底分区——账记下来了，而那正是
// `larkaxctl partition check` 要报出来的事。让结算因此失败是过度反应：
// 一次瞬时的目录冲突或连接抖动会把一笔正常的账挡在门外，而它并没有错。
//
// 与 internal/snapshotwire 的处置一致（那里补建失败也只记一条 Warn 然后
// 继续写）。差别在级别：快照是可以丢的排障数据，用量事件是账的一部分，
// 落进兜底分区意味着这张表的分区承诺已经失效，所以这里记 Error。
//
// **不写缓存**：写缓存等于把一次瞬时失败当成「这个月已经确认过了」，
// 那会让整个月都放弃补建，而没有人会再试一次。
func (s *Store) ensureUsageEventPartition(ctx context.Context) {
	at := s.nowUTC()
	month := UsageEventPartitionName(at)
	if s.usagePartitionKnown(month) {
		return
	}

	if _, err := EnsureUsageEventPartitions(ctx, s.db, at, 1); err != nil {
		s.log().Error("usage_events 当月分区补建失败，本次结算的用量事件可能落进兜底分区",
			"month", month,
			"err", err,
		)
		return
	}

	s.partMu.Lock()
	s.partMonth = month
	s.partEnsured = true
	s.partMu.Unlock()
}

// usagePartitionKnown 报告某个月的分区是不是已经被本进程确认过。
func (s *Store) usagePartitionKnown(month string) bool {
	s.partMu.Lock()
	defer s.partMu.Unlock()
	return s.partEnsured && s.partMonth == month
}

// PostgreSQL 错误码：并发建同一个分区时目录上会撞的那两个。
//
// 硬编码自 PostgreSQL 的 errcodes（duplicate_table / unique_violation）。
// 它们不是本仓库的约束名，所以不会因为改迁移而失效。
const (
	pgDuplicateTable  = "42P07"
	pgUniqueViolation = "23505"
)

// isPartitionAlreadyExists 判断 err 是不是「这个名字已经被占了」。
//
// 只认错误码，不认错误文本：文本会随 PostgreSQL 版本与语言环境变化。
func isPartitionAlreadyExists(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgDuplicateTable || pgErr.Code == pgUniqueViolation
}
