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
