package snapshotwire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/gateway"
)

// Rows 是查询结果的迭代器。*sql.Rows 直接满足它。
//
// 单独声明而不是直接用 *sql.Rows：那个类型只能由数据库驱动构造，
// 于是「分区名筛选」这段逻辑就只能靠一个真库来测。而它恰恰是最该有
// 单元测试的一段——判错了会删掉不该删的分区，那不可逆。
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// DB 是本包需要的数据库能力。
//
// 抽成接口是为了让落库逻辑能用假实现测：分区名、SQL 参数与错误路径
// 都是这条链路上最容易写错的地方，而它们不需要一个真库就能验证。
// 真库上验证的是另一件事——DDL 与分区裁剪是否真的如预期（见集成测试）。
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (Rows, error)
}

// Store 把快照落进 request_snapshots。
type Store struct {
	db     DB
	logger *slog.Logger

	// now 是写入时用的时钟，nil 表示 time.Now。
	//
	// 它同时决定「这一行属于哪一天」与「补建哪一天的分区」——两者必须同源
	// （见 insertSQL 上的注释），所以只留这一个口子，而不是分开传两个时刻。
	//
	// 留这个口子是为了让集成用例能把写入指向一个合成的日期。用例要证明的是
	// 「缺的那天分区会被补出来」，而按当天写用例就等于删当天分区——那是全库
	// 共享的状态，会连累同一个库上并行跑的别的包（见 LESSONS.md 第 26 条）。
	now func() time.Time

	// partitionDay 记录当前实例已经确认过「哪一天的分区存在」。
	//
	// 记到天而不是一个 bool：跨天之后必须重新确认一次，否则新的一天里
	// 第一条快照会落进兜底分区——而那条路是静默的。
	mu             sync.Mutex
	partitionDay   string
	partitionReady bool
}

// NewStore 包装一个数据库句柄。
func NewStore(db DB, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{db: db, logger: logger}
}

// nowUTC 返回写入用的当前时刻，统一 UTC。没设过时钟就用 time.Now。
func (s *Store) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// insertSQL 是唯一写入这张表的地方。
//
// 列名逐个写出来而不是用 SELECT *：骨架的列会随 schema 演进，
// 而 `*` 在演进时会静默改变列序与含义。
// created_at 显式传值，而不是用列上的 DEFAULT now()。
//
// 理由是分区归属：补建的是**应用算出来的那一天的**分区，而 DEFAULT now()
// 用的是数据库的时刻。两者在 UTC 午夜附近会差一天，那时行会落进兜底分区——
// 而那条路是静默的，直到有人发现留存删不掉它。让补建与写入同源，
// 「这一行属于哪一天」就只有一个答案。
//
// # upstream_status 与 error_kind 照实写
//
// 两列由网关填好交进来：请求方向的行上它们是 nil——那一行回答的是「发出去的
// 是什么」，不记结局（见 internal/gateway 的 Snapshot 注释）；响应方向的行上
// 分别表示「上游回了几号」与「这次响应怎么结束的」。nil 落成 NULL，
// 而 NULL 在响应方向的行上是一个有内容的值——「一个 HTTP 响应都没拿到」。
const insertSQL = `
	INSERT INTO request_snapshots (
		request_id, user_id, direction, skeleton_version, capture_mode,
		skeleton, skeleton_truncated, redacted_paths,
		upstream_status, error_kind, created_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// captureModeSkeleton 是默认采集模式。
//
// 取值与表上的 CHECK 约束一致。用户授权原样采集（full_consented）是另一条
// 链路的事，要带同意记录与自动到期，不在这里。
const captureModeSkeleton = "skeleton"

// Insert 写一条快照。
//
// # 为什么写之前要确认分区
//
// 表按天分区，落到兜底分区 request_snapshots_default 的行**没有到期日**：
// 留存期的承诺对它不成立，而它是靠「非空即告警」被人发现的。写入方在这里
// 做一次幂等的补建，是为了不让自己的行成为那条告警的来源——
// 告警应该指向「预建任务漏跑了」，而不是指向一次正常的写入。
//
// 补建按天缓存：一天之内只查一次，热路径上不再有 DDL 往返。
//
// # 失败只返回错误
//
// 落库失败不重试也不 panic：调用方（网关）不等结果，这条链路的任何失败
// 都不该影响请求与计费。重试留给「值不值得为一条排障数据再试一次」的判断，
// 而那需要先有丢弃与失败的计数——它今天有了：一边是本实例的计数
// （见 Collector.Stats），一边是指标族 larkax_snapshot_failed_total。
func (s *Store) Insert(ctx context.Context, snap *gateway.Snapshot) error {
	if snap == nil {
		return errors.New("快照为空")
	}
	createdAt := s.nowUTC()

	if err := s.ensurePartition(ctx, createdAt); err != nil {
		// 补建失败仍然尝试写入：分区可能刚被别人建好，或者表根本没有分区
		// （例如在测试库里被重建过）。写失败会在下面报出来，而这里的错误
		// 单独记一条——「补建失败但写进去了」与「补建失败且写不进去」
		// 是两种现场。
		s.logger.Warn("快照分区补建失败，仍尝试写入",
			"day", PartitionName(createdAt),
			"err", err,
		)
	}

	skeleton, err := json.Marshal(snap.Skeleton)
	if err != nil {
		// 骨架是脱敏器产出的普通 map，序列化不了说明它里面有非 JSON 值，
		// 那是脱敏器的 bug 而不是上游的问题。这里报出来而不是存一个空对象：
		// 空骨架看起来像「这次请求什么都没发」。
		return fmt.Errorf("骨架无法序列化: %w", err)
	}

	paths := snap.RedactedPaths
	if paths == nil {
		// text[] 列不喜欢 nil：写成 NULL 的话后续 `redacted_paths @> ...`
		// 这类查询会静默漏掉这些行。空数组与「没有字段被脱敏」才是同一个意思。
		paths = []string{}
	}

	// 指针列转成 database/sql 认的形态：nil 落 NULL，非 nil 落值本身。
	// sql 包也会解引用指针，但显式写出来，「NULL 是有意的」这件事就不靠
	// 读者去记驱动的行为。
	var upstreamStatus any
	if snap.UpstreamStatus != nil {
		upstreamStatus = *snap.UpstreamStatus
	}
	var errorKind any
	if snap.ErrorKind != nil {
		errorKind = *snap.ErrorKind
	}

	_, err = s.db.ExecContext(ctx, insertSQL,
		snap.RequestID,
		snap.UserID,
		snap.Direction,
		snap.Version,
		captureModeSkeleton,
		skeleton,
		snap.Truncated,
		paths,
		upstreamStatus,
		errorKind,
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("插入快照失败: %w", err)
	}
	return nil
}

// PartitionName 是某一天对应的分区表名。
//
// 用 UTC 日期：分区的边界由服务器时区决定，而部署地可能不在同一个时区。
// 统一按 UTC 命名，让「哪一天」这件事只有一个答案。
func PartitionName(t time.Time) string {
	return "request_snapshots_" + t.UTC().Format("2006_01_02")
}

// DayBounds 返回某一天分区的上下界，左闭右开。
//
// 与分区 DDL 的 FOR VALUES 一致（形如 `FROM ('2026-10-16T00:00:00Z')
// TO ('2026-10-17T00:00:00Z')`）。边界按 UTC 算出，与 PartitionName 同源——
// 两处若各算一次，跨时区部署时分区名与边界会错开一天，而那种错误不会报错。
func DayBounds(t time.Time) (from, to time.Time) {
	u := t.UTC()
	from = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 0, 1)
}

// ensurePartition 在当天分区缺失时补建它。
//
// IF NOT EXISTS 让并发的实例之间不互相报错：两个实例同时发现缺分区并同时
// 补建，结果必须一样。
func (s *Store) ensurePartition(ctx context.Context, at time.Time) error {
	day := PartitionName(at)
	if s.partitionKnown(day) {
		return nil
	}

	from, to := DayBounds(at)
	// 标识符不能参数化，所以分区名是拼进 SQL 的。它全部由日期与固定前缀
	// 构成（PartitionName 只产出 [a-z0-9_]），没有任何外部输入能进到这里。
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF request_snapshots FOR VALUES FROM ('%s') TO ('%s')`,
		day, from.Format(time.RFC3339), to.Format(time.RFC3339),
	)
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("补建分区 %s 失败: %w", day, err)
	}

	s.mu.Lock()
	s.partitionDay = day
	s.partitionReady = true
	s.mu.Unlock()
	return nil
}

func (s *Store) partitionKnown(day string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.partitionReady && s.partitionDay == day
}

// listPartitionsSQL 列出快照表下的全部分区。
//
// 判据是分区名的日期，不是分区里的数据：分区按天切，所以「整个分区都过期」
// 等价于「这一天的日期过期」。按数据判断会退化成 DELETE，而 DELETE 的代价
// 与风险都远高于 DROP。
//
// LIKE 只按前缀筛，不写年份范围：写死 `_20%` 会让 1999 年那种分区（本地调试
// 造出来的）被整条漏掉——清理漏删是静默的，而它正是留存承诺失效的方式。
// 形状的判断交给 PartitionDate，那一步对不上的会被跳过并记日志。
const listPartitionsSQL = `
	SELECT c.relname
	  FROM pg_class c
	  JOIN pg_inherits i ON i.inhrelid = c.oid
	  JOIN pg_class p ON p.oid = i.inhparent
	 WHERE p.relname = 'request_snapshots'
	   AND c.relname LIKE 'request_snapshots\_%'`

// partitionDateLayout 是分区名里那一段日期的格式。
const partitionDateLayout = "2006_01_02"

// partitionPrefix 是分区名的固定前缀。
const partitionPrefix = "request_snapshots_"

// DefaultPartition 是兜底分区。
//
// 它按设计存在于 schema 里，不是「不认识的分区名」，所以跳过它时不记日志：
// 每次清理都报一句没人能处理的警告，会让真正的异常（别的表挂在快照表下）
// 混在噪音里被忽略。
const DefaultPartition = "request_snapshots_default"

// ExpiredPartitions 列出已到期的分区名，不删除。
//
// 与 DropExpired 共用同一份判据（见 expiredNames），于是「--dry-run 说会删哪
// 些」与「实际删了哪些」不可能给出不同的答案——两处各写一遍筛选条件，
// 迟早会出现「演练说删 3 个、执行删了 5 个」。
func (s *Store) ExpiredPartitions(ctx context.Context, retentionDays int, at time.Time) ([]string, error) {
	return s.expiredNames(ctx, retentionDays, at)
}

// DropExpired 删除到期分区，返回被删除的分区名。
//
// # 只删名字对得上的分区
//
// 兜底分区（request_snapshots_default）与任何名字不符合 `前缀 + 日期` 形状的
// 分区都不会被删。这不是防御性编程而是必须的：一个 DROP TABLE 打错目标
// 是不可逆的，而这里的输入来自系统目录。形状对不上就跳过并记一条日志——
// 「有张不认识的表挂在快照表下面」本身值得被人看到。
//
// # 删除的边界
//
// 保留最近 retentionDays 天。判据是**分区日期**小于「今天 - 留存天数」。
// 用分区日期而不是 created_at 的最大值：后者要扫全表，而分区日期就是这张
// 分区的语义边界，它才是「这一天」的定义。
func (s *Store) DropExpired(ctx context.Context, retentionDays int, at time.Time) ([]string, error) {
	names, err := s.expiredNames(ctx, retentionDays, at)
	if err != nil {
		return nil, err
	}

	var dropped []string
	for _, name := range names {
		// 只有形状经过校验的名字才会走到这里，也才会拼进 SQL。
		if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
			// 一个分区删不掉就停：继续删下去会让「删了哪些」变得难以说清，
			// 而下一次运行会重试（DROP 是幂等的）。
			return dropped, fmt.Errorf("删除分区 %s 失败: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

// RetentionCutoff 返回到期线：分区日期早于这一天的分区都已到期。
//
// 它是「哪些分区到期」这个判据的定义处——Store 的筛选与命令行打印的那句说明
// 都从这里取。两处各算一遍的话，命令说出来的到期线会与实际删的错开一天，
// 而那种错误不报错，只是把话说错。
//
// retentionDays 必须为正：判据在 LoadConfig（配错就拒绝启动）与 expiredNames
// （不删任何东西并报错）上各有一道，这里不再判第三次。
func RetentionCutoff(retentionDays int, at time.Time) time.Time {
	cutoff, _ := DayBounds(at.AddDate(0, 0, -retentionDays))
	return cutoff
}

// expiredNames 是到期分区的唯一判据。清理与演练都走它。
func (s *Store) expiredNames(ctx context.Context, retentionDays int, at time.Time) ([]string, error) {
	if retentionDays <= 0 {
		// 不删任何东西，也不回落到某个默认值：留存期写错时静默按默认值删，
		// 会删掉本来该留的数据。这里返回错误，让调用方自己决定怎么办。
		return nil, fmt.Errorf("留存天数必须为正数，当前为 %d", retentionDays)
	}

	rows, err := s.db.QueryContext(ctx, listPartitionsSQL)
	if err != nil {
		return nil, fmt.Errorf("列出快照分区失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取分区名失败: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分区名: %w", err)
	}

	// 到期线：日期早于这一天的分区都删。今天减去留存天数，
	// 于是「留存 30 天」意味着最近 30 天的分区（含今天）留着。
	cutoff := RetentionCutoff(retentionDays, at)

	var expired []string
	for _, name := range names {
		if name == DefaultPartition {
			// 兜底分区永远不删：删掉它之后，任何越界写入都会直接失败，
			// 而「写不进去」比「写进了没有到期日的地方」更难被发现。
			continue
		}
		day, ok := PartitionDate(name)
		if !ok {
			s.logger.Warn("快照表下有不认识的分区名，跳过不删",
				"partition", name,
				"expected_shape", partitionPrefix+"YYYY_MM_DD",
			)
			continue
		}
		if day.Before(cutoff) {
			expired = append(expired, name)
		}
	}
	sort.Strings(expired)
	return expired, nil
}

// PartitionDate 解析分区名里的日期。
//
// 名字不符合 `request_snapshots_YYYY_MM_DD` 时返回 false——调用方据此跳过，
// 而不是猜一个日期出来。返回的时间是 UTC 零点。
func PartitionDate(name string) (time.Time, bool) {
	if len(name) != len(partitionPrefix)+len(partitionDateLayout) {
		return time.Time{}, false
	}
	if name[:len(partitionPrefix)] != partitionPrefix {
		return time.Time{}, false
	}
	day, err := time.Parse(partitionDateLayout, name[len(partitionPrefix):])
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}
