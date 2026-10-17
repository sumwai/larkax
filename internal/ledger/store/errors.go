package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件把**数据库的拒绝**翻译成调用方认得出的 Go 错误。
//
// 写路径上能预判的拒绝都写在 SQL 的 WHERE 里（余额够不够、可用额够不够），
// 更新影响 0 行就是拒绝，调用处把它翻译成 ErrInsufficientAvailable 之类。
// 但有一类情况预判不了：**钱包快照本身已经和权威值不一致**——例如 frozen
// 已经小于待释放的预留之和。那时 UPDATE 的结果是负数，由表上的
// `CHECK (balance >= 0 AND frozen >= 0)` 拒绝，错误从驱动里冒上来。
//
// 这一层负责认出那个错误并给它一个名字。**它只翻译，不吞**：
// 认不出来的错误原样包一层返回，绝不因为读不懂而当成成功。
//
// 这里有两层，判据宽窄不同：
//
//   - 具名：认得出来具体含义的（钱包下溢、用户不存在）给一个精确的哨兵，
//     调用方可以直接问出「是哪一种」
//   - 兜底：剩下的唯一键 / 外键冲突统一挂 ErrConstraintViolation。它不回答
//     「是哪一种」，只回答「数据库因为约束拒绝了」，具体约束名仍留在驱动
//     错误上

// PostgreSQL 的错误码与约束名。
//
// 两者都硬编码自迁移 00001_init.sql 里的
// `CONSTRAINT wallet_non_negative CHECK (balance >= 0 AND frozen >= 0)`。
// **改那个约束名就要同步改这里**，否则下面的识别失效。失效的方向是安全的：
// 认不出来就按原错误上抛，不会静默当成功，只是少一层分类。
//
// 只认错误码不够——23514 的含义是「有某个 CHECK 被违反」，
// 不是「冻结额会变负」。
const (
	pgCheckViolation            = "23514"
	walletNonNegativeConstraint = "wallet_non_negative"
)

// pgForeignKeyViolation 是外键约束冲突的错误码，userWalletsUserIDConstraint
// 是 `user_wallets.user_id → users(id)` 这条外键的名字（PostgreSQL 自动生成
// 的名字，硬编码自迁移 00001_init.sql）。
//
// **两个都要判。** 23503 的含义只是「有某个外键被违反」，不是「这个用户不存在」
// ——同一个库里还有十几条外键，别的表插错一行拿到的也是 23503。只认错误码会把
// 「别的表的外键冲突」也说成「用户不存在」，而调用方（命令行）照这个分类给退出码，
// 于是退出码在说谎。判据要窄到这一条约束上。
//
// 与 walletNonNegativeConstraint 同一个形状：**改迁移里的约束名就要同步改这里**。
// 失效的方向是安全的：认不出来就按原错误上抛，不会静默当成成功，只是少一层分类
// （命令行那边表现为退出码从 4 退回 1）。
const (
	pgForeignKeyViolation       = "23503"
	userWalletsUserIDConstraint = "user_wallets_user_id_fkey"
)

// ErrNoSuchUser 表示这次写入引用的用户在 users 里没有对应的一行。
//
// 它与 ErrNoWallet 是**两个方向**的事，不要合并：
//
//   - ErrNoWallet：用户存在，但他没有那一行钱包——缺的是**从属的那一行**
//   - ErrNoSuchUser：连用户本身都没有——缺的是**主那一行**
//
// 报出来的是哪一步由错误文本说清，分类由哨兵说清。
//
// # 为什么不复用 auth.ErrUserNotFound
//
// 那一个属于解析路径（`auth.ResolveUser` 认不出一个标识），而本包**不 import
// auth**：账本层不该依赖鉴权层，只为共用一句错误文本就拉一条依赖，方向是反的。
// 两个哨兵都进命令行的 conflictErrors、都落到退出码 4，所以「同一件事在两条
// 路径上给出同一个退出码」照样成立。
//
// # 为什么要有它
//
// 在这之前 OpenWallet 把驱动的原文（含 SQLSTATE 23503 与约束名）原样上抛，
// 错误链里没有任何哨兵，于是同一个事实——「这个用户不在库里」——在解析路径上
// 退出 4、在这条写入路径上退出 1。运维脚本按退出码分派处置，两个码是两种处置。
var ErrNoSuchUser = errors.New("用户不存在")

// ErrConstraintViolation 表示这次写入被数据库的约束拒绝：唯一键或外键。
//
// 它是**兜底**，不是具名哨兵的替代品：
//
//   - 认得出具体含义的路径返回那个更精确的错误，调用方可以直接问出
//     「是用户不存在」（ErrNoSuchUser）这类事实
//   - 认不出的路径返回它，调用方只知道「某条唯一键或外键拒绝了这次写入」
//
// 两者可以同时出现在一条错误链里：具名的仍是更精确的那一个，兜底的那个
// 让「是不是约束冲突」有一个统一的判据。判分类时先判具名的，再判它。
//
// # 为什么要有它
//
// 命令行的退出码表把「唯一键、外键」明确算作约束冲突（退出 4），而在它
// 之前只有被逐个认出来的约束才落得到那里：没认出来的外键冲突把驱动的原文
// 直接抛上去，错误链里没有任何哨兵，退出码落到 1。枚举约束名迟早漏——
// 表上多一条外键就多一个口子——所以写路径的出口按 SQLSTATE 兜一层。
//
// # 它不抹平「哪个约束」
//
// 驱动的 *pgconn.PgError 仍然在错误链里（`%w`），表名与约束名都在它上面。
// 这一层只给调用方一个能 errors.Is 的分类，运维要看现场时照旧 errors.As
// 取回驱动错误。
var ErrConstraintViolation = errors.New("违反数据库约束")

// isConstraintViolation 判断 err 是不是唯一键或外键冲突。
//
// 只判 SQLSTATE：这两个码在这里的含义是明确的——「某条唯一键 / 外键被
// 违反」。具体是哪一条由驱动错误上的表名与约束名回答，不由这一层猜：
// 猜错会让分类说谎（#391 的 ErrNoSuchUser 就是为此把判据窄到一条约束上）。
//
// CHECK 违反（23514）不在里面：它与唯一键、外键的下一步处置不同，由各自的
// 调用点翻译成具名哨兵（见 wallet_non_negative 与 ErrFrozenUnderflow）。
func isConstraintViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgForeignKeyViolation || pgErr.Code == pgUniqueViolation
}

// constraintFailure 是写路径的统一出口：认出唯一键 / 外键冲突时挂上
// ErrConstraintViolation，其余错误原样返回。
//
// **认不出来时一个字都不加**，所以可以无差别地套在每一条写语句的错误上，
// 包括那些今天还撞不到约束的语句。新增一条外键时不必回头找这里是哪一处；
// 约束名也不再需要逐个硬编码。
func constraintFailure(err error) error {
	if !isConstraintViolation(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrConstraintViolation, err)
}

// ErrFrozenUnderflow 表示这次解冻会让 frozen 变成负数。
//
// 它与 ErrInsufficientBalance 是**两件事**，处置也不同：
//
//   - ErrInsufficientBalance：结算扣款让 balance 变负。用户真的欠费，
//     是用户能处理的，接口层应当据此返回「余额不足」
//   - ErrFrozenUnderflow：解冻让 frozen 变负。钱包快照与预留记录不一致，
//     是**内部不变量被破坏**，用户没有做错任何事，要人去查
//
// 所以：**拿 ErrInsufficientBalance 判断「用户欠费」是错的**。它在下溢时
// 也成立（见 frozenUnderflowError 的错误链），照它返回「余额不足」会把
// 内部故障判成用户欠费，还会把含用户 id、资产、金额的错误文本漏给用户。
// 将来按错误分类时，先排掉 ErrFrozenUnderflow，再按 ErrInsufficientBalance
// 判断欠费。
var ErrFrozenUnderflow = errors.New("冻结额下溢")

// isMissingUserViolation 判断 err 是不是 user_wallets 上那条指向 users 的
// 外键被违反。
//
// **判据是两个条件的与**（见上面两个常量的注释）：只判错误码会把任何表上的
// 外键冲突都认成「用户不存在」。反向用例 TestMissingUserViolationIsNarrow
// 钉的就是这一点——把约束名那一半去掉，它会红。
//
// 不去查库确认「这个 id 真的不在 users 里」：那要么多一次往返、要么多一个
// 竞态窗口（查完到报错之间用户可能刚被建出来），而这两种做法都不能让分类更准
// ——违反这条约束本身就意味着插入那一刻 users 里没有这一行。
func isMissingUserViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgForeignKeyViolation &&
		pgErr.ConstraintName == userWalletsUserIDConstraint
}

// missingUserError 把「这次插入引用的用户不在库里」翻译成调用方认得出的错误。
//
// 保留驱动错误在链里（`%w` 而不是 `%v`）：SQLSTATE 与 DETAIL 是运维现场唯一
// 拿得到的依据，而这里只是给它加一个名字，不是替掉它。
func missingUserError(err error, userID int64) error {
	return fmt.Errorf("%w: 用户 %d 在 users 里没有对应的一行: %w",
		ErrNoSuchUser, userID, err)
}

// isFrozenUnderflow 判断 err 是不是「这次更新会让 frozen 变成负数」被拒。
func isFrozenUnderflow(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgCheckViolation && pgErr.ConstraintName == walletNonNegativeConstraint
}

// frozenUnderflowError 是解冻下溢的分类错误。
//
// 它的错误链一次满足三件事。**不用 errors.Join**：那个的错误文本是换行
// 拼起来的，而这条要进日志与告警，单行更好读。
//
//   - errors.Is(err, ErrFrozenUnderflow)：新语义可以单独判定
//   - errors.Is(err, ErrInsufficientBalance)：**保留**，与「ErrInsufficientBalance
//     一类」的既有判断兼容（但反过来拿它判断用户欠费是错的，见该 sentinel）
//   - errors.As(err, &pgErr)：驱动给的原始错误仍在链里，SQLSTATE 与 DETAIL
//     都还拿得到
type frozenUnderflowError struct {
	subject string
	cause   error
}

func (e *frozenUnderflowError) Error() string {
	return fmt.Sprintf("%v: 解冻%s会让冻结额变成负数，快照的 frozen 已经小于待释放的预留之和: %v",
		ErrFrozenUnderflow, e.subject, e.cause)
}

// Unwrap 返回整条错误链。三个元素缺一不可，理由见类型注释。
func (e *frozenUnderflowError) Unwrap() []error {
	return []error{ErrFrozenUnderflow, ErrInsufficientBalance, e.cause}
}

// unfreezeFailure 把解冻语句的错误翻译成调用方认得出的错误。
//
// subject 描述这次解冻的对象（谁的钱、哪次请求），只用于错误信息：
// 出问题时人要看的是「谁的钱不对」，而不是哪条 SQL 失败了。
// 它会被拼进「解冻<subject>会让冻结额变成负数」，所以得写成名词短语。
//
// 为什么解冻下溢要报出来而不是夹零，见 `Release` 里的注释。
func unfreezeFailure(err error, subject string) error {
	if !isFrozenUnderflow(err) {
		return fmt.Errorf("解冻失败: %w", err)
	}
	return &frozenUnderflowError{subject: subject, cause: err}
}

// walletRowMissing 报「解冻时没有钱包行可落」。
//
// 它是 unfreezeFailure 的邻居，管的是另一种失败：那条语句**根本没碰到行**。
// 冻结额长在钱包行上，行不在就没有任何东西被释放，而 SQL 对影响 0 行不报错——
// 不主动看 RowsAffected，一次解冻就会在账上表现为成功、冻结额却一分未动，
// 而预留记录已经标成 released，那笔钱再也追不回来。
//
// # 为什么不新增 sentinel
//
// 这个条件与 SaveReservation 那条路径上的 ErrNoWallet 是**同一件事**：
// 用户没有对应的钱包行。为同一种事实造第二个 sentinel，只会让调用方
// 两处都得判断；复用既有的那个，调用方认 ErrNoWallet 一处就够。
// 报的是哪一步（预留还是解冻）由错误文本说清，不由 sentinel 说。
//
// # 为什么不回查一次区分原因
//
// diagnoseFreezeFailure 要回查，是因为那里影响 0 行有**两种**解释
// （没有钱包行 / 可用余额不足）。这里只有一种：两条解冻语句的 WHERE
// 都不筛业务条件——`Release` 那条只按主键定位，`releaseFrozen` 那条的
// status = 'settled' 在同一事务里刚被 markSettled 置上。所以 0 行就是
// 「没有钱包行」，没有第二种解释可区分，回查只会多一次往返和一个
// 走不到的分支。
//
// subject 描述这次解冻的对象，与 unfreezeFailure 的 subject 同构。
func walletRowMissing(subject string) error {
	return fmt.Errorf("%w: 解冻%s时没有钱包行可落，冻结额一分未动", ErrNoWallet, subject)
}
