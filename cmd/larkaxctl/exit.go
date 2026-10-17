package main

import (
	"errors"
	"flag"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/pipeline"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
	"github.com/sumwai/larkax/internal/pricing"
)

// 退出码。运维脚本按它分派处置，所以这几个数是对外契约的一部分，
// 改它们等于改所有用它的脚本：
//
//	0  成功
//	1  其他失败
//	2  参数错（用法、取值、缺字段）
//	3  连不上数据库
//	4  约束冲突（唯一键、外键、要改或要查的那一行不存在）
//
// 分这几类而不是统一给 1，是因为它们的下一步动作不同：参数错要改命令、
// 连不上库要去看环境、约束冲突要改数据。混成一个码，脚本只能去匹配错误
// 文本，而错误文本是给人看的，措辞会随改动而变。
const (
	exitOK        = 0
	exitFailure   = 1
	exitUsage     = 2
	exitDBUnreach = 3
	exitConflict  = 4
)

// codedError 给一个错误带上退出码。
//
// Error() 原样返回内层：退出码是给脚本看的，不该改变人看到的那句话。
type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// usageErr 把一个参数错误标成 exitUsage。
//
// 用法是「解析与校验参数的那一段返回什么，就把它包一层」。校验全部发生在
// 连库之前，所以这一层能标的一定是参数问题，不必去猜错误的来源。
func usageErr(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: exitUsage, err: err}
}

// dbErr 把一个连库失败标成 exitDBUnreach。
func dbErr(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: exitDBUnreach, err: err}
}

// conflictErrors 是各包的写入与解析路径撞上数据约束时返回的哨兵错误。
//
// 集中列在这里，是为了让命令那一层只管把包的错误原样返回——「这个错误算不算
// 冲突」是分类问题，不是每个子命令各自判断一遍的问题（各判断一遍时，
// 漏掉一个的表现是它悄悄退回 exitFailure，而 1 与 4 在脚本里是两种处置）。
//
// 两类都算冲突：撞上唯一键 / 外键，以及要改或要查的那一行不在库里。后者的
// 下一步动作与前者的性质相同——都是「先去看数据里现在是什么」。「要查的」
// 那一类来自**解析**路径（例如 auth.ResolveUser 认不出一个标识），它与写入
// 路径共用同一个哨兵，所以这里两类并列而不是只列写入。
var conflictErrors = []error{
	auth.ErrUserNotFound,
	// 「要查的那一行不在」在写入路径上的对应物：解析完用户之后、插入钱包之前
	// 用户被删，store 那边由外键挡下并返回它（见 store.ErrNoSuchUser）。它与
	// auth.ErrUserNotFound 是同一个事实的两条路径，共用一个退出码。
	store.ErrNoSuchUser,
	// 用户存在、但没有那一行钱包。与上一条是同一个形状（缺一行），只是缺的
	// 是从属的那一行，下一步动作也一样：先去看数据里现在是什么——这一次是
	// 先跑 wallet create。
	store.ErrNoWallet,
	// 同一个 txn_id 已经是一笔内容不同的交易。它同样要人去看一眼库里那笔
	// 是什么，而不是改一下命令重发（那会写进第二笔，或者继续撞同一笔）。
	store.ErrTxnConflict,
	// 兜底的约束冲突：各包写路径认不出具体含义的唯一键 / 外键都归到它。
	// 上面那个具名哨兵比它精确（errors.Is 能直接问出「是用户不存在」），
	// 但**只有被逐个认出来的约束**才有具名哨兵——枚举迟早漏，而退出码表
	// 把外键与唯一键整体算作约束冲突。
	//
	// 它是**一个跨包的哨兵**（internal/dberr），不是每包一个：判据只有一份，
	// 新增写路径套上 dberr.ConstraintFailure 之后不必回来登记第二条。store 的
	// 同名变量是它的别名，两处指向同一个错误值。
	//
	// 这一列的每一项都返回同一个 exitConflict，**先后无语义**：冲突之间没有
	// 「更冲突」之说，列表顺序只是给人读的。需要区分具体含义的调用方按具名
	// 哨兵判断。
	dberr.ErrConstraintViolation,

	channels.ErrChannelNotFound,

	catalog.ErrModelExists,
	catalog.ErrCanonicalTaken,
	catalog.ErrUpstreamTaken,
	catalog.ErrModelInUse,
	catalog.ErrModelNotFound,
	catalog.ErrAliasIDNotFound,
	catalog.ErrAliasNotFound,
	catalog.ErrChannelNotFound,

	pricing.ErrRuleNotFound,
	// 没有一条规则适用。它与「规则不存在」是同一个形状：要查的那条不在，
	// 下一步都是先去看数据里现在是什么。请求路径上它是拒绝服务，不是按 0 元计
	// （见 pricing.Admin.Explain），所以这里也不能让它悄悄退回「其他失败」。
	pipeline.ErrNoRule,

	pluginstore.ErrPluginNotFound,
}

// noChangeErrors 是「这次调用没提到任何要改的字段」。
//
// 它是参数问题：命令里少写了一个字段，重发一次就能成。三个包各有一个
// 同名的哨兵，值不同，所以逐个列出。
var noChangeErrors = []error{
	channels.ErrNoChanges,
	catalog.ErrNoChanges,
	pricing.ErrNoChanges,
}

// exitCode 给出一个错误该用的退出码。
//
// 显式标记（codedError）优先于按哨兵分类：命令自己知道得最清楚。
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}

	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	for _, target := range noChangeErrors {
		if errors.Is(err, target) {
			return exitUsage
		}
	}
	for _, target := range conflictErrors {
		if errors.Is(err, target) {
			return exitConflict
		}
	}
	return exitFailure
}

// helpRequested 判断这次失败其实是「用户要帮助」。
//
// flag 在解析到 -h / --help 时返回 ErrHelp，而那时用法已经打印过了。
// 把它当失败（还多打一行「flag: help requested」）会让 `larkaxctl channel -h`
// 在脚本里看起来像一次错误，而它显然不是。
func helpRequested(err error) bool {
	return errors.Is(err, flag.ErrHelp)
}
