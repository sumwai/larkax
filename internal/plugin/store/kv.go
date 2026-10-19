package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/plugin/host"
	"github.com/sumwai/larkax/internal/textshape"
)

// KV 是 host.KVStore 的生产实现，落在 plugin_kv 表上。
//
// # 表从哪来
//
// plugin_kv 在 00003_plugin.sql 里就建好了：三段主键、可空的 expires_at、
// 按到期时间的部分索引，形状正好是这一片要的。**这一片是第一次用它**——
// 那次迁移把「插件运行期需要的东西」一次建齐，而宿主侧的 KV 能力到现在才接上。
// 所以这一片没有任何迁移：表在，再写一条 CREATE TABLE 只会撞上「已存在」。
//
// # 三条语义各自落在哪一层
//
//   - **隔离**在表的主键上（plugin_key, user_id, key）。方法都带 plugin 与
//     userID，而漏写一处 WHERE 只会查不到，不会跨用户读到别人的行——这正是
//     那次迁移把隔离放进主键而不是留给查询的原因。
//   - **过期**由 SQL 的 now() 判：Get / Del 的可见性条件与 Set 算出的到期时刻
//     用同一个时钟。进程与库的钟可以差几秒，而「这个键还在不在」不该随它变；
//     两边各用各的钟时，同一个 TTL 在两条路径上会给出不同的答案。
//   - **配额**在这一层判（上限见 host.KVMaxKeys / host.KVMaxBytes），而且是
//     硬的：写入前先按 (plugin, user) 取一把事务级顾问锁，见 Set。
type KV struct {
	db *sql.DB
}

// NewKV 造一个插件 KV 的存储层。
func NewKV(db *sql.DB) *KV { return &KV{db: db} }

// 编译期钉住契约：宿主函数按 host.KVStore 调它，签名对不上时这里报错，
// 而不是等到装配那一处赋值时才报（那时离原因已经隔了两层）。
var _ host.KVStore = (*KV)(nil)

// kvVisible 是「这一行还在」的条件，Get 与 Del 共用。
//
// 抽成常量而不是各写一遍：两处漏一处就是「读不到但删得掉」这种自相矛盾的
// 行为，而它只在这两个方法上出现，看代码的人很容易以为它们是同一个条件。
const kvVisible = `expires_at IS NULL OR expires_at > now()`

// Get 读一个键。键不存在、或者已经过期时回 found = false。
//
// 过期与不存在**归成同一个答案**：插件看到的都是 null，它没有第二种处置。
// 分成两个返回值只会让每个调用方都要判一次「过期算不算没有」，而它们会
// 各自判出不同的答案。
func (k *KV) Get(ctx context.Context, plugin string, userID int64, key string) (string, bool, error) {
	const query = `
		SELECT value FROM plugin_kv
		 WHERE plugin_key = $1 AND user_id = $2 AND key = $3 AND (` + kvVisible + `)`

	var value string
	switch err := k.db.QueryRowContext(ctx, query, plugin, userID, key).Scan(&value); {
	case errors.Is(err, sql.ErrNoRows):
		// 正常的「没有这个键」，不是错误。
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("读取插件 KV 失败: %w", err)
	}
	return value, true, nil
}

// Del 删一个键，返回删之前它是不是**可见**的（存在且没过期）。
//
// 一次条件删除就把结果回出来：先问在不在、再删的话，两次调用之间不是原子的，
// 并发的两次 del 会都报 true（见 host.KVStore.Del 的注释）。
//
// 已经过期的行**不动它**：它对调用方等于不存在（回 false），而清理是 Set 那
// 一步的事（见那边的顺手清）。两处都想清的话，同一个命名空间就有两个清理者，
// 而它们的触发时机不同，出问题时得先分辨是谁清的。
func (k *KV) Del(ctx context.Context, plugin string, userID int64, key string) (bool, error) {
	const query = `
		DELETE FROM plugin_kv
		 WHERE plugin_key = $1 AND user_id = $2 AND key = $3 AND (` + kvVisible + `)`

	res, err := k.db.ExecContext(ctx, query, plugin, userID, key)
	if err != nil {
		return false, fmt.Errorf("删除插件 KV 失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("确认插件 KV 的删除结果失败: %w", err)
	}
	return affected > 0, nil
}

// Set 写一个键。ttl 为零或负数表示不过期。
//
// # 一个事务里做四件事
//
// 取命名空间的写锁、清掉过期的行、按**写完之后的状态**判配额、写入。
// 分开做的话每一步都说得通，合起来就不再是「写一个键」了——配额会在
// 清理之前判（于是刚过期的行还占着额度）、或者写入与判定之间插进另一个写。
//
// # 配额为什么是一把顾问锁
//
// 判据是「这个 (plugin, user) 下所有行的键数与字节数」，而那是一个跨行的
// 事实：两个事务各读一份旧用量时会双双通过，于是硬约束变成「大致不超过」。
// 锁按 (plugin, user) 取名，所以只有同一个插件同一个用户的并发写会互相等。
// 它是一把互斥锁而不是一个键，哈希撞车只让两个无关的命名空间多等一次，
// 不影响正确性。
//
// 直接对这张表写 SQL 的路径（如果有）不受它管辖——配额是这一层的约束，
// 与「隔离靠主键」不是一回事，后者连绕过实现也成立。
func (k *KV) Set(ctx context.Context, plugin string, userID int64, key, value string, ttl time.Duration) error {
	// 键会被 `plugin kv get` 打成「键」那一行，所以写的时候就要挡住控制字符。
	// 值不在这条判据里：它按设计排在表外原样打（见 cmd/larkaxctl 的
	// printPluginKVEntry），换行与制表符都是它可能带的。
	if err := textshape.Validate("--key", key); err != nil {
		return err
	}
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	// 提交成功后这次 Rollback 是空操作；失败路径上它负责收尾。
	defer func() { _ = tx.Rollback() }()

	// 顾问锁的键要在 SQL 里拼成文本（插件名 + 用户 id），两个参数各自带一个
	// 显式类型：pgx 按语句描述推参数类型，`$2::text` 那一支会把 int64 的实参
	// 当成文本编码，报的是「编码不了」而不是「类型不对」。
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::bigint::text, 0))`,
		plugin, userID); err != nil {
		return fmt.Errorf("取插件 KV 的写锁失败: %w", err)
	}

	// 顺手清掉这个命名空间里已经过期的行。
	//
	// 清理在写入这一侧：读不写库（Get 把过期的行过滤掉就够了），而读远多于写，
	// 所以「顺手清」放在写上是唯一不需要另起一个调度器的地方。表上那条按
	// expires_at 的部分索引（00003）留给将来真需要全局清理的那一步。
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM plugin_kv
		 WHERE plugin_key = $1 AND user_id = $2
		   AND expires_at IS NOT NULL AND expires_at <= now()`,
		plugin, userID); err != nil {
		return fmt.Errorf("清理过期的插件 KV 失败: %w", err)
	}

	// 数用量，但**不含正在写的那个键**：它按这次的新值算（下面加进去）。
	// 于是「新写一个键」与「覆盖一个键」是同一个算法——按写完之后的状态判，
	// 而不是按增量判，把一个占了很多字节的键改小时也不会因为旧值大而被拒。
	//
	// octet_length 与 Go 的 len 都是字节数（列是 text，编码 UTF-8），
	// 所以两处相加不会一格两制。
	var others, otherBytes int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE key <> $3),
		       COALESCE(sum(octet_length(key) + octet_length(value))
		                FILTER (WHERE key <> $3), 0)
		  FROM plugin_kv
		 WHERE plugin_key = $1 AND user_id = $2`,
		plugin, userID, key).Scan(&others, &otherBytes); err != nil {
		return fmt.Errorf("统计插件 KV 的用量失败: %w", err)
	}

	// 错误里包住 host.ErrKVQuotaExceeded：宿主那一侧按它给出 quota_exceeded
	// 那个码（见 host/kv.go 的 kvWriteError），插件据此知道「先删掉一些旧键」
	// 是有用的一步。文案里带上是哪一项到顶了——只有「配额用尽」的话，
	// 插件作者不知道是该清键还是该改小值。
	if keys := others + 1; keys > host.KVMaxKeys {
		return fmt.Errorf("%w: 键数会达到 %d（每个插件每个用户最多 %d 个）",
			host.ErrKVQuotaExceeded, keys, host.KVMaxKeys)
	}
	if used := otherBytes + int64(len(key)+len(value)); used > host.KVMaxBytes {
		return fmt.Errorf("%w: 字节数会达到 %d（每个插件每个用户最多 %d 字节）",
			host.ErrKVQuotaExceeded, used, host.KVMaxBytes)
	}

	// 到期时刻由 **SQL 的 now()** 算，不用进程时钟：理由见类型注释里「过期」
	// 那一段。单位是秒（带小数）而不是整数秒：一个不到一秒的 TTL 取整之后
	// 是 0，而 0 在这里表示「不过期」——一个 500ms 的 TTL 变成永不过期正是
	// 这类换算最容易出的错。
	//
	// 覆盖时 expires_at 跟着这次的值走，不是留着上一个：插件把 ttl 从
	// 「1 小时」改成 0（不过期）时，那个键必须真的不再过期。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plugin_kv (plugin_key, user_id, key, value, expires_at, updated_at)
		VALUES ($1, $2, $3, $4,
		        CASE WHEN $5::double precision <= 0 THEN NULL
		             ELSE now() + make_interval(secs => $5::double precision) END,
		        now())
		ON CONFLICT (plugin_key, user_id, key)
		DO UPDATE SET value = EXCLUDED.value,
		              expires_at = EXCLUDED.expires_at,
		              updated_at = now()`,
		plugin, userID, key, value, ttl.Seconds()); err != nil {
		return fmt.Errorf("写入插件 KV 失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交插件 KV 的写入失败: %w", err)
	}
	return nil
}
