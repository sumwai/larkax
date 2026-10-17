// Package dberr 把 PostgreSQL 的**约束冲突**翻译成一个跨包共用的哨兵。
//
// # 为什么是一个独立的叶子包
//
// 每个写语句都要回答同一个问题：「这次失败是不是数据库因为唯一键或外键
// 拒绝了这次写入？」答案本来在各包各写一份，而判定口径只有一个——
// 判据抄成两份的那一天，两处就开始分岔，表现为同一条约束在 A 包退出 4、
// 在 B 包退出 1。
//
// 它不 import 仓库里的任何包：只依赖标准库与驱动，任何写路径都可以 import
// 它，不会因为「账本层不该依赖鉴权层」这类方向问题被挡住。
//
// # 两层判据，这一层只管兜底
//
//   - 具名：调用点认得出来具体含义的约束（用户名被占、模型已存在、用户
//     不存在）继续由各包自己的哨兵表达，调用方可以直接问出「是哪一种」
//   - 兜底：剩下的唯一键 / 外键统一挂 ErrConstraintViolation，它只回答
//     「数据库因为约束拒绝了」，不回答「是哪一条」
//
// 两层可以同时出现在一条错误链里：具名的仍然是更精确的那一个，兜底的让
// 「是不是约束冲突」有一个统一判据。分类时先判具名的，再判它。
package dberr

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL 的错误码。它们来自 PostgreSQL 的 errcodes，不是本仓库的
// 约束名，所以不会因为改迁移而失效。
const (
	// pgForeignKeyViolation 是外键约束冲突（foreign_key_violation）。
	pgForeignKeyViolation = "23503"
	// pgUniqueViolation 是唯一约束冲突（unique_violation）。
	pgUniqueViolation = "23505"
)

// ErrConstraintViolation 表示这次写入被数据库的约束拒绝：唯一键或外键。
//
// 它是**兜底**，不是具名哨兵的替代品。认得出具体含义的路径返回那个更
// 精确的错误；认不出的路径返回它，调用方只知道「某条唯一键或外键拒绝了
// 这次写入」。
//
// # 为什么要有它
//
// 命令行的退出码表把「唯一键、外键」明确算作约束冲突（退出 4）。在它
// 之前只有被逐个认出来的约束才落得到那里：没认出来的外键冲突把驱动的
// 原文直接抛上去，错误链里没有任何哨兵，退出码落到 1。枚举约束名迟早
// 漏——表上多一条外键就多一个口子——所以写路径的出口按 SQLSTATE 兜一层。
//
// # 它不抹平「哪个约束」
//
// 驱动的 *pgconn.PgError 仍然在错误链里（`%w`），表名与约束名都在它上面。
// 这一层只给调用方一个能 errors.Is 的分类，运维要看现场时照旧 errors.As
// 取回驱动错误。
var ErrConstraintViolation = errors.New("违反数据库约束")

// IsConstraintViolation 判断 err 是不是唯一键或外键冲突。
//
// 只判 SQLSTATE：这两个码在这里的含义是明确的——「某条唯一键 / 外键被
// 违反」。具体是哪一条由驱动错误上的表名与约束名回答，不由这一层猜：
// 猜错会让分类说谎（同一个库里十几条外键，只判错误码会把「别的表的外键
// 冲突」也说成某一件具体的事）。
//
// # CHECK 违反（23514）为什么不并进来
//
// 它与唯一键、外键的**下一步处置不同**：唯一键 / 外键说「数据之间对不上」
// （引用了一行不存在的、或者已经有一行同键的），CHECK 说的是「这一行自己
// 的值不合法」——例如钱包下溢，那是内部不变量被破坏，要人去查，不是换个
// 名字重试就能过去的事。各调用点把它翻译成自己的具名哨兵
// （ledger/store 的 ErrFrozenUnderflow 等），并在这里保留它在这个兜底
// 集合之外的位置。
func IsConstraintViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgForeignKeyViolation || pgErr.Code == pgUniqueViolation
}

// ConstraintFailure 是写路径的统一出口：认出唯一键 / 外键冲突时挂上
// ErrConstraintViolation，其余错误原样返回。
//
// **认不出来时一个字都不加**，所以可以无差别地套在写语句的错误上，
// 包括那些今天还撞不到约束的语句。新增一条外键时不必回头找这里是哪一处；
// 约束名也不再需要逐个硬编码。
//
// 具名哨兵走它不会丢：`fmt.Errorf("%w: %w", ErrConstraintViolation, err)`
// 把整条原错误链留在后面，errors.Is / errors.As 照旧能问出更精确的那个。
func ConstraintFailure(err error) error {
	if !IsConstraintViolation(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrConstraintViolation, err)
}
