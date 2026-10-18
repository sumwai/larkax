package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/settings"
)

// 本文件是对账任务：校验钱包快照与账本一致。
//
// # 它比对的是什么
//
// 两件事，各自有各自的权威方：
//
//   - `balance` 的权威是**账本**：它应当等于 `user:<id>` 科目在该资产上的
//     全部分录之和。
//   - `frozen` 的权威是**预留记录**：它应当等于 `billing_requests` 里
//     `status='pending'` 那些行的 `reserved_amount` 之和。冻结不进账本
//     （冻结与释放都不写分录，理由见 Release 的注释），所以它的权威
//     只能在预留表上。
//
// 不一致本身是**异常信号**，不是一种正常状态：它说明有代码路径绕过了
// store（直接改了钱包或直接写了分录），或者预留算错了。
//
// # 还有一类要报的东西，它两边是对得上的
//
// 上面那两件事都问「两边相不相等」。一条**没人管的** pending 预留两边恰好
// 相等——钱冻在钱包上，预留记在 billing_requests 上，两个都在——于是上面
// 那两件事一条都不会报，而对账也就对得平。可是那笔钱永远冻着。
//
// 所以另立一条判据（FindingStaleReservation）：pending 且 started_at 早于
// staleReservationAfter。它报的不是差额，是滞留；处置也不是修正，是人工
// 决定这笔钱该不该放开。
//
// # 「写一条 adjustment 分录把快照修正到账本值」为什么不成立
//
// issue 要的是这个，而实现时会发现这句话自相矛盾。值得写下来，因为它
// 决定了这里的行为：
//
// 设账本值为 L、快照值为 W，两者不等，要写一条金额为 D 的分录去修。
// 修正之后账本值变成 L+D，而快照要被修正到的目标**正是**账本值——
// 于是快照变成 L+D，两者又差 D。下一轮对账再发现一次，再写一条……
// 要么永远修不平，要么把差额算两次（把用户余额改成 W+2D）。
//
// 根子在于：**账本从来没有记错。** 不一致意味着快照偏离了账本，而账本里
// 那些分录从写下那一刻起就是权威的。所以修正快照不需要任何分录——没有
// 资金流动，只是重算一个派生值（store 包注释里的「余额可以随时由账本
// 重新算出来」就是这个意思）。
//
// 于是这里提供两种**各自自洽**的口径，用设置项选（见 RepairMode）：
//
//   - 账本为准（默认）：快照重算为账本值，不写分录。差额没有资金流动，
//     修正留痕在 reconcile_runs 这张只追加的表里。
//   - 快照为准：写一条调账分录把**账本**改到快照的值，快照不动。这时
//     「差额落哪个科目」才有意义——它是平台认下的、来路不明的一笔收支，
//     见 ReconcileConfig.DifferenceAccount。
//
// 两种口径都不改已有的分录。账本只追加，这一点没有例外。
//
// # 跑多快
//
// 一次全表扫描在大库上很贵，所以默认按增量跑：只看上次跑成之后
// **变动过**的行，起点记在 reconcile_runs.watermark 里（理由见该列的注释）。
//
// 增量有一个盲区：**两边都在上次扫描之前就已经错着**的行，它永远不看。
// 所以定期要做一次全量（ReconcileConfig.FullScanInterval），
// 那是「每一行至少被完整看过一次」的唯一保证。
//
// # 报出来之后还会再报
//
// 增量扫描看不见「没被修掉、自己也不再变动」的那些。所以每一轮报出来的
// 不一致会写进 reconcile_findings，并且**下一轮把它算回候选集合**——
// 否则告警响一次就永久安静，而不一致还挂在那里（见 reconcileAll）。
// 只有某一轮确认它消失了，那一行才删掉。

// RepairMode 决定发现不一致之后往哪个方向修。
//
// 两个取值各自自洽，差别只在「哪个值算数」——那是个业务判断，不是技术
// 问题，所以它做成配置项而不是写死在代码里。
type RepairMode string

const (
	// RepairResyncSnapshot 以账本为准：把快照重算成账本值。
	//
	// 这不写任何分录：账本从来没有记错，偏离的是快照，修正它只是重算一个
	// 派生值。差额本身没有资金流动，写成分录反而是在账本里凭空多出一笔
	// 来路不明的账。
	RepairResyncSnapshot RepairMode = "resync_snapshot"

	// RepairAdjustLedger 以快照为准：写一条调账分录把账本对齐到快照值，
	// 差额落到 DifferenceAccount，快照不动。
	//
	// 与上一种的取舍是明确的：它**从不减少用户余额**（用户的账面数字就是
	// 他看到的数字），代价是把一笔来路不明的钱记进了账本。
	RepairAdjustLedger RepairMode = "adjust_ledger"
)

// RepairModes 返回全部修正口径，供配置校验与运维界面枚举。
func RepairModes() []RepairMode { return []RepairMode{RepairResyncSnapshot, RepairAdjustLedger} }

// 设置项键名，与迁移 00004_reconcile.sql 里的 INSERT 一一对应。
const (
	// SettingAutoRepair 控制发现不一致之后要不要动手。
	SettingAutoRepair = "reconcile.auto_repair"
	// SettingRepairMode 选择修正方向，仅在 SettingAutoRepair 为真时生效。
	SettingRepairMode = "reconcile.repair_mode"
	// SettingDifferenceAccount 是快照为准时修正分录的对家科目。
	SettingDifferenceAccount = "reconcile.difference_account"
	// SettingShardSize 是一批处理的钱包行数上限。
	SettingShardSize = "reconcile.shard_size"
	// SettingFullScanIntervalHours 是两次全量扫描之间的最长间隔。
	SettingFullScanIntervalHours = "reconcile.full_scan_interval_hours"
)

// defaultShardSize 是分片大小读不出来时用的值。
//
// 默认值本身写在迁移里、代码里不写第二份（理由见 internal/settings 的包
// 注释），但**结构化**的默认值必须在这里兜：零值会让分片循环空转，
// 而那看起来跟「跑完了，没有不一致」一模一样。
const defaultShardSize = 1000

// staleRunAfter 是判定「上一次运行已经死掉」的时限。
//
// 它针对的是**硬崩**——进程被 kill、机器掉电，没有任何代码有机会把那一行
// 结掉。跑出错与超时**不在**此列：结账那一步不带取消，自己就能写回去
// （见 finishReconcileRun），所以超时不会留下要等一小时才清的 running 行。
//
// 阈值必须**长于一轮合法对账可能活着的时长**，否则并发的另一轮会把一个还
// 活着的运行标成 failed。命令行给单轮的上限是 30 分钟（cmd/larkaxctl 的
// reconcileTimeout，也是唯一的调用方），一小时留了余量。两个数呈这个大小
// 关系是有意的：**上限必须短于阈值**，改任何一个都要看另一个。
//
// 留着没结掉的行比清掉更危险：运行记录里会堆起一串看不出结局的行，
// 事后翻的时候说不清哪一轮是真的没跑完。
const staleRunAfter = time.Hour

// staleReservationAfter 是判定「这条 pending 预留已经没人管了」的时限。
//
// # 它要区分的是「彻底没人管了」与「只是慢」
//
// 后者是**正常**的：预留与释放之间隔着一次上游调用，而流式回复可以跑很久
// （适配器的 HTTP 客户端因此只设 ResponseHeaderTimeout，见
// internal/channel/httpx——整体超时会在一段长回复的中途把连接掐断）。
// 阈值贴着正常请求的耗时取，就会把在途的请求报成滞留，而这一类 finding
// 一旦开始误报，人就再也不看它了。
//
// 本仓库另外两个同类的量级都短得多，因为它们量的都是**有上限**的东西：
// cmd/larkaxctl 的 reconcileTimeout（30 分钟）是一轮对账的上限，
// staleRunAfter（一小时）是「这一轮已经死了」的判据——两者都建立在
// 「一轮对账最多活 30 分钟」这条已知的上限之上。请求没有这样一个上限，
// 所以阈值只能取到「慢」这个解释完全站不住的地方。
//
// 一天的依据：一次请求的耗时应以分钟计，这里留了三个数量级的余量。
// 它顺带盖住了另一件事——started_at 是**应用进程的墙钟**（SaveReservation
// 显式绑进去的是 RuleMatchedAt），而判据比的是数据库的 now()，两个时钟
// 之间的偏差在一天这个量级上完全淹没在余量里。
//
// 报晚一天没有代价：这是一笔一直冻着的钱，不是一次正在发生的故障。
// 反过来说，阈值取短了是有代价的：一条误报会让人开始怀疑整张清单。
const staleReservationAfter = 24 * time.Hour

// finishTimeout 是「把这次运行的结局写回去」这一步的时间上限。
//
// 单独给一个上限，是因为这一步用的是**不带取消**的 ctx（见
// finishReconcileRun）：它不再受调用方那个 ctx 的期限约束，得自己带一个，
// 否则一次卡住的写会挂到进程退出为止。这个上限**对成功与失败两条路径一视
// 同仁**——改前成功路径吃的是调用方剩下的预算（命令行最多给到 30 分钟），
// 现在也是这么多，是一次收窄，只是不认为会真撞上。
//
// 五秒的依据：写的是单行、按主键定位的 UPDATE，量级是毫秒。连它都写不进去，
// 说明库已经不接受写入了，把上限放宽到几十秒也救不回来；而这一步的全部意义
// 就是不许挂住。真撞上它，结果是留一行 running——与硬崩同一种收尾，由下一轮
// 的 stale 清理捞走（见 staleRunAfter）。
const finishTimeout = 5 * time.Second

// 认「这个科目属于哪个用户」的写法。**对账里的每一处都必须用这里的定义**，
// 不能各写各的：曾经有三处各写各的，于是 `user:0<id>` 这类写法被写路径
// 算进了用户的钱包，却既不在汇总里、也不被判为畸形——对账拿一个偏小的
// 权威值去比，把一个**正确的**余额改小。
//
// Go 侧的同一套规则是 ledger.UserIDFromAccount（写路径与 ParseAccount 用它）。
// SQL 没法调用 Go，所以只能在这里再写一遍：**改一处就要四处一起改**——
// 这里的三个常量，加上 ledger.UserIDFromAccount。不一致本身就是那条缺陷。
//
//   - userAccountCanonicalPattern 是唯一能进账本的写法，ledger.ParseAccount
//     只放行它。畸形判据用它描述「规范」。
//   - userAccountAliasPattern 是旧写路径也认过的写法：它把后缀读成整数，
//     所以前导零与正号当年**是**算进钱包的。对账必须照样认出来，否则库里
//     已有的那些会让汇总值偏小，缺陷照旧。它比规范写法宽，正好覆盖规范写法。
//   - userAccountCanonicalSQL 把科目原文归一到规范写法。**只做字符串处理、
//     不转整数**：`::bigint` 在超范围的取值上会报错，而 WHERE 里两个条件的
//     求值顺序没有保证，一个报错会让整轮对账失败。这里的写法在任何取值上
//     都不报错。它**必须与 userAccountAliasPattern 一起用**：表达式自己会把
//     `user :42` 归成 `user:42`（分隔符前面那个空格被 split_part 吃掉了）、
//     把 `user:1:2` 归成 `user:1`，是那个写法判据把它们挡在外面的。
//     只用表达式就会把钱算到错的人头上。
const (
	userAccountCanonicalPattern = `^user:[1-9][0-9]{0,17}$`
	userAccountAliasPattern     = `^user:[+]?0*[0-9]{1,18}$`
	userAccountCanonicalSQL     = `'user:' || ` +
		`COALESCE(NULLIF(ltrim(ltrim(split_part(account, ':', 2), '+'), '0'), ''), '0')`
)

// ReconcileConfig 是一轮对账的开关与口径。
type ReconcileConfig struct {
	// AutoRepair 为 false 时只报告不修正（默认）。
	AutoRepair bool
	// RepairMode 在 AutoRepair 为真时决定往哪个方向修。
	RepairMode RepairMode
	// DifferenceAccount 是 RepairAdjustLedger 下修正分录的对家科目。
	DifferenceAccount ledger.Account
	// ShardSize 是一批处理的钱包行数上限（按 user_id 分片）。
	ShardSize int
	// FullScanInterval 是两次全量扫描之间的最长间隔。
	// 小于等于零表示每次都全量。
	FullScanInterval time.Duration
}

// normalize 把零值补成明确的行为，并挡住非法取值。
//
// 不静默回落：设置项写错时应当在一轮对账**开始之前**就报出来，
// 而不是按一个没人预期的口径去改钱。
func (c ReconcileConfig) normalize() (ReconcileConfig, error) {
	if c.RepairMode == "" {
		c.RepairMode = RepairResyncSnapshot
	}
	valid := false
	for _, m := range RepairModes() {
		if c.RepairMode == m {
			valid = true
		}
	}
	if !valid {
		return c, fmt.Errorf("未知的对账修正口径 %q", c.RepairMode)
	}
	if c.DifferenceAccount.IsZero() {
		c.DifferenceAccount = ledger.ReconcileDifferenceAccount()
	}
	parsed, err := ledger.ParseAccount(c.DifferenceAccount.String())
	if err != nil {
		return c, fmt.Errorf("对账差额科目不合法: %w", err)
	}
	c.DifferenceAccount = parsed
	if parsed.Type() == ledger.AccountUser {
		// 对家落在用户钱包上时，那条分录会让**另一个**用户的钱包偏离账本，
		// 而对账修正直接写分录、不走 applyToWallets，没人会去修它。
		return c, fmt.Errorf("对账差额科目不能是用户钱包科目（%s）", parsed)
	}
	if c.ShardSize <= 0 {
		c.ShardSize = defaultShardSize
	}
	return c, nil
}

// FindingKind 是一条不一致的类别。
type FindingKind string

const (
	// FindingBalanceMismatch 余额与账本汇总不一致。
	FindingBalanceMismatch FindingKind = "balance_mismatch"
	// FindingFrozenMismatch 冻结额与未结算预留之和不一致。
	FindingFrozenMismatch FindingKind = "frozen_mismatch"
	// FindingWalletMissing 有分录或预留，却没有对应的钱包行。
	FindingWalletMissing FindingKind = "wallet_missing"
	// FindingMalformedAccount 账本里出现了畸形的 user 科目。
	//
	// 它比一条余额差额更严重：钱记在了一个谁也认不出来的科目上，
	// 而「按 user:<id> 汇总」永远汇总不到它。
	FindingMalformedAccount FindingKind = "malformed_account"
	// FindingStaleReservation 是一条挂了太久的未结算预留。
	//
	// 它与上面三类**不是同一回事**：那三类报的是「两边对不上」，这一条报的
	// 是「两边对得上，但这笔钱冻着没人管」。所以上面三类一条都不会报它——
	// frozen 与 pending 预留之和恰好相等（冻结记在快照上、预留记在
	// billing_requests 上，两边都在），对账因此对得平。要发现它只能另立判据。
	//
	// 它有两个来源，都不留别的痕迹：
	//
	//   - 释放本身失败。gateway 的收尾路径丢弃 Release 的错误（那是有意的，
	//     见它的注释：不能拿释放的错误盖掉最初那个上游错误）。
	//   - 进程在「预留落库」与「释放」之间硬崩。两者隔着一次可能数分钟的
	//     上游调用，重启是设计里就考虑过的情形（见 gateway.ReservationStore
	//     的注释）。
	//
	// # 只报不自动释放
	//
	// 自动释放与「请求还在途」有竞态：一条正在被服务的预留，在对账眼里与
	// 一条没人管的预留长得一模一样。误放一笔在途请求的预留会让冻结额低于
	// 实际待结算额——`frozen` 减到负数，被表上的 CHECK 拒绝；而夹到零就是
	// 平台悄悄认下差额。宁可报给人看。
	FindingStaleReservation FindingKind = "stale_reservation"
)

// Finding 是一条不一致。
type Finding struct {
	// RunID 是发现它的那次运行。
	RunID int64
	// UserID 是涉及的用户。畸形科目在能归位到某个用户时（`user:042` 那种）
	// 也填它，归不到任何人时为零——两种「零」只有后一种。
	UserID int64
	// Asset 是资产种类。
	Asset string
	// Kind 是不一致的类别。
	Kind FindingKind
	// Snapshot 是快照里的值（余额或冻结额）。没有钱包行时是零值。
	//
	// 例外：Kind 为 FindingStaleReservation 时**恒为零**。这一类不是一组
	// 两边相比的值，它是「这一行滞留太久」的标记，没有快照侧。
	Snapshot money.Amount
	// Ledger 是推导出来的权威值（账本汇总或未结算预留之和）。
	//
	// 例外：Kind 为 FindingWalletMissing 时它**只是分录之和**，不含冻结侧
	// （那时没有钱包行可挂冻结额，未结算预留之和只写在 Detail 里）。读这个
	// 字段时别把它当成那一行的全部差额，列注释见
	// internal/migrate/sql/00004_reconcile.sql 的 ledger 列。
	//
	// 例外：Kind 为 FindingStaleReservation 时它装的是**那条预留冻住的
	// 金额**。它不是「权威值」，与 Snapshot 也不构成一组差额——两边的值
	// 本来就相等，只是这笔钱一直冻着。放在这一列里是为了让「一共冻着多少」
	// 能直接由 `SELECT sum(ledger) FROM reconcile_findings WHERE
	// kind = 'stale_reservation'` 回答，而不必去解析 Detail 里的文字。
	Ledger money.Amount
	// Unrepairable 非空时说明**为什么不能自动修**。
	//
	// 它必须存在，而不是让修正静默跳过：一条报出来、等人工处置的不一致，
	// 与一条报出来又被修掉的不一致，处置完全不同。
	Unrepairable string
	// Repaired 表示这次运行已经把它修正了。
	Repaired bool
	// Detail 是补充说明（例如畸形科目的原文）。
	Detail string
	// Key 是畸形科目的科目原文。钱包类的不一致留空。
	//
	// 单独一个字段而不是从 Detail 里抠：Detail 是给人看的，它带着
	// 「几条分录」这类每轮都在变的数字，拿它当标识会让同一条不一致
	// 每轮都被当成新的一条。
	Key string
}

// Diff 是权威值与快照值之差（Ledger − Snapshot）。
//
// Kind 为 FindingStaleReservation 时这个返回值**没有意义**：那一类不是
// 一组两边相比的值，它只有一个金额（装在 Ledger 里，见该字段的注释），
// 而 Snapshot 恒为零。别拿它当「差额」读，也别拿它判断这一类严不严重——
// 严不严重的判据是滞留了多久，那在 Detail 里。
func (f Finding) Diff() money.Amount { return f.Ledger.Sub(f.Snapshot) }

// Scope 是这条不一致在 reconcile_findings 里的标识。
//
// # 为什么不能用 (user_id, asset, kind)
//
// 畸形科目归不到任何用户头上——它的 UserID 是零，资产可能根本解析不出来。
// 它只能用**科目原文**标识，而那个值恰恰是 `user :42` 这类畸形写法。
// 所以标识是一个字符串，钱包类的不一致写成 `user:<id>@<asset>`，畸形科目
// 写成 `account:<原文>`。
//
// 陈旧预留同理用**请求标识**写成 `request:<request_id>`：滞留的预留是一行
// 一行的，放开其中一条之后那一条就该从清单里消失；按 (user, asset) 合并的话，
// 三条里处理掉两条看起来与一条都没处理一样。
//
// 三类的形态用一个前缀区分开，而不是靠「user: 开头的是钱包类」这种判断：
// 畸形科目的原文也可能以 `user:` 开头（`user:abc` 就是）。
func (f Finding) Scope() string {
	switch f.Kind {
	case FindingMalformedAccount:
		return malformedScopePrefix + f.Key
	case FindingStaleReservation:
		return reservationScopePrefix + f.Key
	}
	return fmt.Sprintf("user:%d@%s", f.UserID, f.Asset)
}

// Alert 是不一致的告警通道。
//
// 做成回调而不是在这里直接写日志：告警落地的方式（日志、通知、工单）是
// 部署的事，本包只管「发现了什么」。传 nil 表示不告警——但那样不一致就
// 只留在 Report 与 reconcile_runs 里，需要调用方自己去看。
type Alert func(ctx context.Context, f Finding)

// LogAlerts 返回一个把不一致写成错误日志的告警通道。
//
// 用 Error 级别而不是 Warn：对账不一致意味着有代码路径绕过了 store，
// 或者预留算错了，两者都是需要有人去看的缺陷，而 Warn 在多数部署里
// 进了同一个桶，区别只在有没有人肯承认它在看。
func LogAlerts(logger *slog.Logger) Alert {
	if logger == nil {
		logger = slog.Default()
	}
	return func(_ context.Context, f Finding) {
		attrs := []any{
			"run_id", f.RunID,
			"kind", string(f.Kind),
			"user_id", f.UserID,
			"asset", f.Asset,
		}
		// 陈旧预留没有「快照 / 权威值」这一对（见 Finding 的字段注释），
		// 所以不给它打 snapshot 与 diff：一个叫 diff 而其实是金额本身的数
		// 会被读成一条差额，而这一类恰恰一条差额都没有。
		if f.Kind == FindingStaleReservation {
			attrs = append(attrs, "ledger", f.Ledger.String())
		} else {
			attrs = append(attrs,
				"snapshot", f.Snapshot.String(),
				"ledger", f.Ledger.String(),
				"diff", f.Diff().String())
		}
		attrs = append(attrs,
			"repaired", f.Repaired,
			"unrepairable", f.Unrepairable,
			"detail", f.Detail)

		logger.Error("对账发现不一致", attrs...)
	}
}

// Report 是一轮对账的结果。
type Report struct {
	// RunID 是 reconcile_runs 里的那一行。
	RunID int64
	// FullScan 表示这一轮是全量还是增量。
	FullScan bool
	// StartedAt 是这一轮开始的时刻（数据库时钟）。
	StartedAt time.Time
	// Watermark 是这一轮比对的起点，也是下一轮的起点。
	Watermark time.Time
	// CheckedWallets 是这一轮比对过的钱包行数。
	CheckedWallets int
	// Findings 是这一轮发现的全部不一致。
	//
	// 顺序：畸形科目、陈旧预留、然后才是按 (user_id, asset) 稳定排序的钱包类
	// 不一致。前两类各有各的查询（它们不属于任何一个分片），按查询自己的
	// 顺序追加在前面；钱包类那一段的排序是稳定的，那一条不变。
	Findings []Finding

	// malformed 是畸形科目的条数。
	//
	// 用计数而不是在 Findings 里再数一遍：Findings 可能被调用方裁剪
	// （例如只打印前 N 条），而进 reconcile_runs 的数字必须是完整的——
	// 它回答的是「这一轮账上出现了几处写法不合规范的 user 科目」。
	//
	// 其中有一类读得出用户 id（`user:0<id>` 这种），金额已经归到了那个
	// 用户名下，要订正的只是写法；另一类才是真的归不到人头上。两者的
	// 分工见 reconcileMalformed。
	malformed int
}

// BalanceMismatches 数出余额类的不一致条数。
func (r *Report) BalanceMismatches() int { return r.countOf(FindingBalanceMismatch) }

// FrozenMismatches 数出冻结类的不一致条数。
func (r *Report) FrozenMismatches() int { return r.countOf(FindingFrozenMismatch) }

// MalformedAccounts 数出畸形科目的条数。
func (r *Report) MalformedAccounts() int { return r.malformed }

// StaleReservations 数出陈旧 pending 预留的条数。
//
// 从 Findings 里数，与 BalanceMismatches / FrozenMismatches 一致。
func (r *Report) StaleReservations() int { return r.countOf(FindingStaleReservation) }

// Repaired 数出被修正的不一致条数。
func (r *Report) Repaired() int {
	n := 0
	for _, f := range r.Findings {
		if f.Repaired {
			n++
		}
	}
	return n
}

func (r *Report) countOf(k FindingKind) int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == k {
			n++
		}
	}
	return n
}

// ConfigFromSettings 把 settings 里的值翻成 ReconcileConfig。
//
// 放在本包而不是调用方，是为了让「哪个键控制哪件事」只有一处。读取失败
// 一律返回错误：对账会改钱，不该在一个读不懂的配置上跑起来。
func ConfigFromSettings(v *settings.Values) (ReconcileConfig, error) {
	autoRepair, err := v.Bool(SettingAutoRepair)
	if err != nil {
		return ReconcileConfig{}, err
	}
	mode, err := v.String(SettingRepairMode)
	if err != nil {
		return ReconcileConfig{}, err
	}
	account, err := v.String(SettingDifferenceAccount)
	if err != nil {
		return ReconcileConfig{}, err
	}
	parsed, err := ledger.ParseAccount(account)
	if err != nil {
		return ReconcileConfig{}, fmt.Errorf("settings.%s 不是合法科目: %w", SettingDifferenceAccount, err)
	}
	shard, err := v.Int(SettingShardSize)
	if err != nil {
		return ReconcileConfig{}, err
	}
	if shard < 0 {
		// 负数会被 normalize 当成「没填」而换成默认值——那是静默回落，
		// 表现为「我明明调小了分片，它却按 1000 跑」。报出来。
		return ReconcileConfig{}, fmt.Errorf("settings.%s 不能为负: %d（0 表示用默认值）", SettingShardSize, shard)
	}
	hours, err := v.Int(SettingFullScanIntervalHours)
	if err != nil {
		return ReconcileConfig{}, err
	}
	if hours < 0 {
		return ReconcileConfig{}, fmt.Errorf("settings.%s 不能为负: %d", SettingFullScanIntervalHours, hours)
	}

	cfg, err := ReconcileConfig{
		AutoRepair:        autoRepair,
		RepairMode:        RepairMode(mode),
		DifferenceAccount: parsed,
		ShardSize:         int(shard),
		FullScanInterval:  time.Duration(hours) * time.Hour,
	}.normalize()
	if err != nil {
		return ReconcileConfig{}, err
	}
	if int64(int(shard)) != shard {
		// int 在 32 位平台上装不下过大的 shard_size，静默截断会让分片变成
		// 另一个值，而那看起来跟「跑完了」一样。报出来。
		// 判据写成往返转换而不是与某个上界比较：上界随平台变，
		// 而「转过去再转回来还是不是原来那个数」在任何平台都成立。
		return ReconcileConfig{}, fmt.Errorf("settings.%s 超出可表示范围: %d", SettingShardSize, shard)
	}
	return cfg, nil
}

// Reconcile 跑一轮对账。
//
// 返回的 Report 里带着这一轮的全部不一致；alert 非空时每一条还会单独告警。
// 中途出错会返回已经查出来的部分与错误，并把这次运行标成 failed——
// 部分结果比一句「失败了」有用，它至少说明查到哪里为止。
func (s *Store) Reconcile(ctx context.Context, cfg ReconcileConfig, alert Alert) (*Report, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	startedAt, err := s.dbNow(ctx)
	if err != nil {
		return nil, err
	}
	watermark, full, err := s.reconcileStart(ctx, startedAt, cfg)
	if err != nil {
		return nil, err
	}
	runID, err := s.beginReconcileRun(ctx, startedAt, watermark, full)
	if err != nil {
		return nil, err
	}

	report := &Report{RunID: runID, FullScan: full, StartedAt: startedAt, Watermark: watermark}
	if err := s.reconcileAll(ctx, cfg, alert, report); err != nil {
		// 失败也要把这一行结掉：留着 status='running' 会让下一轮以为
		// 「上一次还在跑」，而它其实早就死在这里了。ctx 已经不能用时
		// 同样要能写进去——见 finishReconcileRun。
		_ = s.finishReconcileRun(ctx, report, err)
		return report, err
	}
	if err := s.finishReconcileRun(ctx, report, nil); err != nil {
		return report, err
	}
	return report, nil
}

// dbNow 取数据库的当前时间。
//
// 不用 Go 的 time.Now()：写入方的 created_at/updated_at 都由数据库的 now()
// 生成，两个时钟之间有偏差时「起点之后写入的行」会被算错，而算错的方向是
// **漏读**——漏掉的不一致会一直留在那里，且没有任何东西会提醒。
func (s *Store) dbNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("读数据库时间失败: %w", err)
	}
	return now, nil
}

// reconcileStart 算出这一轮的起点，并决定要不要全量。
func (s *Store) reconcileStart(ctx context.Context, startedAt time.Time, cfg ReconcileConfig) (watermark time.Time, full bool, err error) {
	// 起点取**上一次跑成的那一轮的开始时刻**，而不是它的结束时刻。
	//
	// 取开始时刻是为了兜住「跑的过程中新写入的行」：那些行的 created_at
	// 都 >= 上一轮的起点，于是它们会被下一轮再读一次。宁可重复读
	// （比对是幂等的），不能漏读。
	var lastDone, lastFull sql.NullTime
	err = s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT started_at FROM reconcile_runs WHERE status = 'done'
			  ORDER BY started_at DESC LIMIT 1),
			(SELECT started_at FROM reconcile_runs WHERE status = 'done' AND full_scan
			  ORDER BY started_at DESC LIMIT 1)`).Scan(&lastDone, &lastFull)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("读上一轮对账的起点失败: %w", err)
	}

	watermark = startedAt
	if lastDone.Valid {
		watermark = lastDone.Time
	}

	switch {
	case !lastDone.Valid:
		// 从来没跑成过：没有起点可用，只能全量。
		full = true
	case cfg.FullScanInterval <= 0:
		full = true
	case !lastFull.Valid:
		full = true
	default:
		full = startedAt.Sub(lastFull.Time) >= cfg.FullScanInterval
	}
	return watermark, full, nil
}

// beginReconcileRun 落一行运行记录并返回它的 id。
func (s *Store) beginReconcileRun(ctx context.Context, startedAt, watermark time.Time, full bool) (int64, error) {
	// 先清掉上一次崩在中间留下的 'running'：不清的话下一轮会读到一个
	// 假的「在跑」状态，而那个状态决定的正是它从哪里开始扫。
	if _, err := s.db.ExecContext(ctx, `
		UPDATE reconcile_runs
		   SET status = 'failed', finished_at = now(), error = '进程中断，未跑完'
		 WHERE status = 'running'
		   AND started_at < now() - make_interval(secs => $1)`,
		int64(staleRunAfter.Seconds())); err != nil {
		return 0, constraintFailure(fmt.Errorf("清理未跑完的对账记录失败: %w", err))
	}

	var id int64
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO reconcile_runs (started_at, watermark, full_scan, status)
		VALUES ($1, $2, $3, 'running')
		RETURNING id`, startedAt, watermark, full).Scan(&id); err != nil {
		return 0, constraintFailure(fmt.Errorf("写对账运行记录失败: %w", err))
	}
	return id, nil
}

// finishReconcileRun 把这次运行的结局写回去。
//
// # 这一步为什么不跟着调用方的 ctx 走
//
// 它最需要写进去的时候，恰恰是那个 ctx 已经不能用的那一刻：一轮对账超时、
// 被取消时，正是这一行在说「这次运行到此为止」。跟着一个已经取消的 ctx 走，
// 这句「结掉」自己就写不进去，库里于是留一行 status='running'，要等
// staleRunAfter（一小时）之后才被下一轮的 stale 清理捞走。超时是最常见的
// 失败，所以那不是罕见情形。
//
// WithoutCancel 只去掉取消与期限，另配一个独立的上限（finishTimeout）。
// 那个上限到了同样会留一行 running——与硬崩同一种收尾。
func (s *Store) finishReconcileRun(ctx context.Context, r *Report, cause error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	status, msg := "done", ""
	if cause != nil {
		status, msg = "failed", cause.Error()
	}
	// 这几个条数是运行记录上那几列的全部来源（balance_mismatches /
	// frozen_mismatches / repaired / malformed_accounts / stale_reservations）：
	// 它们由 Report 上那几个方法算出来（见 Repaired 与 countOf）。
	//
	// 所以判定「落库的条数对不对」时期望值不能取同一个方法：方法被改坏时两边
	// 一起变，恒等，而这一列在报表上就是错的，却没有任何一处会红。用例那边
	// 自己遍历 Findings 数一遍（见 reconcile_test.go 的 countFindings）。
	_, err := s.db.ExecContext(writeCtx, `
		UPDATE reconcile_runs
		   SET finished_at = now(), checked_wallets = $2,
		       balance_mismatches = $3, frozen_mismatches = $4,
		       repaired = $5, malformed_accounts = $6,
		       stale_reservations = $7,
		       status = $8, error = NULLIF($9, '')
		 WHERE id = $1`,
		r.RunID, r.CheckedWallets, r.BalanceMismatches(), r.FrozenMismatches(),
		r.Repaired(), r.MalformedAccounts(), r.StaleReservations(), status, msg)
	if err != nil {
		return constraintFailure(fmt.Errorf("写对账结果失败: %w", err))
	}
	return nil
}

// reconcileAll 是主循环：按 user_id 分片逐批比对。
//
// # 未消除的不一致要每轮重新报
//
// 增量扫描只看「上次起点之后变动过的行」。一条只报告、没被修掉的不一致
// 本身不会再变动，于是下一轮增量根本不会再看它——告警响一次就永久安静，
// 而不一致还挂在那里。那比「不告警」更糟：它看起来像已经处理过了。
//
// 所以两件事配合：上表里没消除的不一致会被算进候选用户，而这一轮报出来的
// 会被覆盖回去（见 persistFindings）。
func (s *Store) reconcileAll(ctx context.Context, cfg ReconcileConfig, alert Alert, report *Report) error {
	open, err := s.loadOpenFindings(ctx)
	if err != nil {
		return err
	}

	if err := s.reconcileMalformed(ctx, report, alert, open); err != nil {
		return err
	}

	// 陈旧预留与畸形科目一样，不属于任何一个分片：判据只看那一行自己，
	// 与钱包、账本都无关。放在分片循环之前是为了让「不属于分片的那两类」
	// 在报告里连着，读的人一眼能看出它们是另一回事。
	if err := s.reconcileStaleReservations(ctx, report, alert); err != nil {
		return err
	}

	after := int64(0)
	for {
		ids, err := s.reconcileCandidates(ctx, after, report.Watermark, report.FullScan, cfg.ShardSize, open)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		findings, err := s.reconcileUsers(ctx, cfg, report.RunID, ids)
		if err != nil {
			return err
		}
		report.CheckedWallets += len(ids)
		report.Findings = append(report.Findings, findings...)
		for _, f := range findings {
			if alert != nil {
				alert(ctx, f)
			}
		}
		after = ids[len(ids)-1]
	}

	// 只在跑完之后落库：中途失败的那些轮次里，这一轮的 Findings 是不完整的，
	// 拿它去比对「哪些不一致消失了」会把还没看到的一律当成已解决。
	return s.persistFindings(ctx, report)
}

// openFinding 是 reconcile_findings 里的一行，读回来供重新比对。
type openFinding struct {
	scope string
	kind  FindingKind
	// userID 是这条不一致涉及的用户，与 Finding.UserID 同一套约定
	// （畸形科目能归位时也非零）。它只给钱包类当候选用，见 reconcileCandidates。
	userID int64
}

// loadOpenFindings 读回尚未消除的不一致。
func (s *Store) loadOpenFindings(ctx context.Context) ([]openFinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope, kind, user_id FROM reconcile_findings`)
	if err != nil {
		return nil, fmt.Errorf("读未消除的不一致失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []openFinding
	for rows.Next() {
		var (
			f    openFinding
			kind string
		)
		if err := rows.Scan(&f.scope, &kind, &f.userID); err != nil {
			return nil, fmt.Errorf("读未消除的不一致失败: %w", err)
		}
		f.kind = FindingKind(kind)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历未消除的不一致失败: %w", err)
	}
	return out, nil
}

// persistFindings 把这一轮的结果覆盖进 reconcile_findings。
//
// 一次事务里两件事：报出来的写回去，没报出来的删掉。**删是有前提的**——
// 这一轮确实比对过它的 scope：候选集合里带上了所有已记未消除不一致的用户，
// 畸形科目也在 reconcileMalformed 里被单独捞回来，所以这一点成立。
func (s *Store) persistFindings(ctx context.Context, report *Report) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	scopes := make([]string, 0, len(report.Findings))
	kinds := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		scopes = append(scopes, f.Scope())
		kinds = append(kinds, string(f.Kind))
		// first_seen_at 只在首次插入时写：它是「这笔钱从什么时候开始
		// 不对的」的答案，被每轮覆盖掉就等于没有。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reconcile_findings
				(scope, kind, user_id, asset, snapshot, ledger,
				 unrepairable, detail, last_run_id)
			VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8, $9)
			ON CONFLICT (scope, kind) DO UPDATE SET
				user_id = EXCLUDED.user_id,
				asset = EXCLUDED.asset,
				snapshot = EXCLUDED.snapshot,
				ledger = EXCLUDED.ledger,
				unrepairable = EXCLUDED.unrepairable,
				detail = EXCLUDED.detail,
				last_seen_at = now(),
				last_run_id = EXCLUDED.last_run_id`,
			f.Scope(), string(f.Kind), f.UserID, f.Asset,
			f.Snapshot.String(), f.Ledger.String(),
			f.Unrepairable, f.Detail, report.RunID); err != nil {
			// last_run_id 有外键指向上一句刚插进去的 reconcile_runs 行。
			return constraintFailure(fmt.Errorf("记下未消除的不一致失败: %w", err))
		}
	}

	// 没在这一轮报出来的，说明它的 (scope, kind) 已经被比对过且现在一致了。
	//
	// 判据是**两个字段一起**，不能只看 scope：同一个用户的余额与冻结是两条
	// 独立的不一致，余额对上了而冻结还没对上时，只按 scope 删会把那条已经
	// 解决的留着，而它会一直在告警里响。
	//
	// 还要带上 `last_seen_at < 本轮的起点`：两轮对账重叠时（cron 上一轮还没
	// 跑完，或者两次手跑），后开始的那一轮看不见前一轮刚记下的那些 scope
	// ——它们是在它读完之后才写进去的。没有这个条件，它会把前一轮的证据
	// 删掉，表现为「不一致刚报出来就没影了」。加上之后，只有比本轮更早记下
	// 的行才可能被删，而它们确实都被本轮看过（见 reconcileAll 的注释）。
	//
	// 一次删完而不是逐个 DELETE：一次运行的条数可能很多。
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM reconcile_findings f
		 WHERE f.last_seen_at < $3
		   AND NOT EXISTS (
			SELECT 1 FROM unnest($1::text[], $2::text[]) AS seen(scope, kind)
			 WHERE seen.scope = f.scope AND seen.kind = f.kind)`,
		scopes, kinds, report.StartedAt); err != nil {
		return constraintFailure(fmt.Errorf("清理已解决的不一致失败: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交对账结果失败: %w", err)
	}
	return nil
}

// reconcileCandidates 取下一批要比对的用户 id。
//
// # 为什么两个来源都要
//
//   - 钱包 updated_at 变过：快照被动过。正常写入路径每次都会更新它，
//     绕过 store 直接改余额的那条路也会。
//   - 账本 created_at 变过：分录写了而快照没跟上。那种情况下钱包行一动
//     不动，只看 updated_at 永远发现不了。
//
// # 为什么 union 里有账本那一侧
//
// 有分录却没有钱包行本身就是一条不一致（见 FindingWalletMissing），
// 而它只出现在账本的这一侧。
//
// # 为什么 union 里还有未结算预留那一侧
//
// 「有 pending 预留、既没有钱包行也没有账本分录」的用户，另外两侧都看不见
// 他——而还没开过户的用户恰好是这个形状，WalletMissing 那条分支存在的意义
// 正是把他报出来。少了这一侧，他整体在候选集之外。（开户当前由
// `larkaxctl wallet create` 做，见 internal/ledger/store/wallet.go：注册流程
// 从来就不建钱包行，所以这不是「注册漏了一步」，而是「还没人开过户」。）
//
// # 认科目用的是哪一种写法
//
// 账本那一侧用的是 userAccountAliasPattern：旧写路径把后缀读成整数，
// 所以 `user:0<id>` 这类写法当年也**是**算进钱包的，只认规范写法会让
// 这些用户进不了候选集。畸形判据用的是更窄的 userAccountCanonicalPattern，
// 两者一宽一窄是有意的：能归位的一律归位，归不了位的必须报出来。
//
// 上限十八位是有意的：`split_part(...)::bigint` 在位数过多时会**报错**，
// 而一个报错的查询会让整轮对账失败。位数超限的科目不是合法用户 id，
// 它由 reconcileMalformed 单独报出来——那里不要求能解析成整数。
//
// # 为什么要并上 open 里的用户
//
// 没被修掉的旧不一致必须先回到候选里，否则这一轮的 report.Findings 里没有
// 它，persistFindings 就会把它当成「已经解决了」而删掉——那等于把一个
// 真实存在的问题标记成已处理。它自己的行不会变动，增量扫描看不见它。
func (s *Store) reconcileCandidates(
	ctx context.Context, after int64, watermark time.Time,
	full bool, limit int, open []openFinding,
) ([]int64, error) {
	// 已经记下未消除不一致的钱包用户，无论这一轮是不是增量都要带上。
	//
	// 按 kind 排掉畸形科目，而不是按 `userID != 0`：畸形科目的 finding 现在
	// 也带着用户 id（能归位的那一类，见 reconcileMalformed），但它该不该进
	// 候选集由畸形查询自己管（那里按科目原文捞回来）。带上用户会把他拖进
	// 钱包比对——比对本身不会出错，只是白扫一遍，而且「carried 是谁」会多出
	// 一个与钱包不一致无关的来源。
	//
	// 陈旧预留同理：它由 reconcileStaleReservations 按请求标识捞回来，与
	// 钱包比对无关。它虽然也带用户 id，但**不能**靠这一侧进候选集——那些
	// 用户的 pending 行恰恰因为 started_at 很旧，落在增量窗口之外，而这一轮
	// 不需要替他们比对钱包（冻结与预留本来就是相等的，否则报的就是另一类了）。
	carried := make([]int64, 0, len(open))
	for _, f := range open {
		switch f.kind {
		case FindingMalformedAccount, FindingStaleReservation:
			continue
		}
		carried = append(carried, f.userID)
	}

	const fullQuery = `
		SELECT user_id FROM (
			SELECT user_id FROM user_wallets
			UNION
			SELECT DISTINCT split_part(account, ':', 2)::bigint
			  FROM ledger_entries WHERE account ~ '` + userAccountAliasPattern + `'
			UNION
			SELECT DISTINCT user_id FROM billing_requests WHERE status = 'pending'
			UNION
			SELECT unnest($3::bigint[]) AS user_id
		) u
		 WHERE user_id > $1
		 ORDER BY user_id
		 LIMIT $2`
	// 增量这一侧的时间窗口与另外两侧**不同源**，这是既成事实，不是笔误：
	//
	//   - user_wallets.updated_at、ledger_entries.created_at 都由 SQL 的 now()
	//     写，与 watermark（reconcile_runs.started_at）同一个数据库时钟。
	//   - billing_requests.started_at 是**应用进程的墙钟**：SaveReservation
	//     显式把 r.RuleMatchedAt 绑进去，绕过了该列的 DEFAULT now()；它记的
	//     还是「这次请求匹配上规则的时刻」，而不是「这行写进库的时刻」。
	//     这张表上没有别的库生成时间列（settled_at 要等到结算才写，而结算
	//     之后这一行已经不是 pending 了），所以换不掉。
	//
	// 偏差的方向决定漏不漏，两种方向后果不同：
	//
	//   - 应用时钟比数据库**慢**：在 watermark 之后实时写入的行，started_at
	//     可能落在 watermark 之前，这一轮读不到；watermark 只增不减，下一轮
	//     同样读不到——**漏读**，直到下一次全量扫描。
	//   - 应用时钟比数据库快：多读一遍，无害（比对是幂等的）。
	//
	// 漏读的那部分落在增量本来就有的盲区里（见本文件顶部），由
	// FullScanInterval 兜底。要根治得让这一侧也按数据库时钟走，那要改
	// SaveReservation 的写入，不在这里做。
	const incrementalQuery = `
		SELECT user_id FROM (
			SELECT user_id FROM user_wallets WHERE updated_at >= $3
			UNION
			SELECT DISTINCT split_part(account, ':', 2)::bigint
			  FROM ledger_entries
			 WHERE account ~ '` + userAccountAliasPattern + `' AND created_at >= $3
			UNION
			SELECT DISTINCT user_id FROM billing_requests
			 WHERE status = 'pending' AND started_at >= $3
			UNION
			SELECT unnest($4::bigint[]) AS user_id
		) u
		 WHERE user_id > $1
		 ORDER BY user_id
		 LIMIT $2`

	query, args := incrementalQuery, []any{after, limit, watermark, carried}
	if full {
		query, args = fullQuery, []any{after, limit, carried}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("取对账候选用户失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读候选用户失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历候选用户失败: %w", err)
	}
	return out, nil
}

// reconcileMalformed 找出账本里畸形的 user 科目。
//
// 它们是对账的**盲区**：按 `user:<id>` 汇总永远汇总不到 `user :42`、
// `user:abc` 或干脆写成 `user` 这种科目，而那笔钱既不属于任何用户的钱包，
// 也没有别的地方会看见它——除了这里。
//
// 判据是 `LIKE 'user%'` 加上**规范写法**的反匹配，而**不是** `LIKE 'user:%'`：
// 后者要求冒号紧跟在 user 后面，恰恰漏掉最典型的那种畸形（中间多一个空格）。
// 以 user 开头又不合规范的行，只可能是想写成用户科目而写坏了。
//
// 「规范」由 userAccountCanonicalPattern 定义，与 ledger.ParseAccount 放行的
// 是同一套——两处不一致时，一条写法可能既被写路径拒绝、又不会被报出来，
// 那正是本条缺陷原来的形状。
//
// 报出来的这一堆里有两类，处置完全不同，所以 UserID 与 Detail 分开写：
//
//   - 读得出用户 id 的（`user:042`、`user:+42`）：钱已经按那个 id 归位了
//     （见 readLedgerSums），要订正的是写法。新写入不会再产生它们。
//     这类带上 UserID：这条不一致说的就是「某个用户名下有一笔写法要订正的
//     钱」，按用户查 reconcile_findings 时应当能查到它。
//   - 读不出的（`user :42`、`user:abc`、`user:0`、位数超限）：那笔钱不属于
//     任何人的钱包，得由人判断它原本该记给谁。UserID 为零——不是「还不知道」，
//     而是**确实没有**归属。
//
// 只报告不修：修它要先判断这笔钱原本该记给谁，那是人的事，而猜错的代价
// 是把钱挪到另一个用户的账上。
//
// # 为什么要把已记未消除的那些也查回来
//
// 增量查询只看上次起点之后新增的行。一条早就存在、又没人处理的畸形科目
// 不会再新增，于是这一轮的 Findings 里没有它，persistFindings 就会把它
// 当成「已经解决了」而删掉。所以已记下的那些科目名要一起查回来，
// 无论它们的 created_at 是什么时候。
func (s *Store) reconcileMalformed(ctx context.Context, report *Report, alert Alert, open []openFinding) error {
	var carried []string
	for _, f := range open {
		if f.kind == FindingMalformedAccount {
			carried = append(carried, strings.TrimPrefix(f.scope, malformedScopePrefix))
		}
	}

	const fullQuery = `
		SELECT account, asset, SUM(amount)::text, count(*)::text
		  FROM ledger_entries
		 WHERE account LIKE 'user%' AND account !~ '` + userAccountCanonicalPattern + `'
		 GROUP BY account, asset
		 ORDER BY account, asset`
	const incrementalQuery = `
		SELECT account, asset, SUM(amount)::text, count(*)::text
		  FROM ledger_entries
		 WHERE account LIKE 'user%' AND account !~ '` + userAccountCanonicalPattern + `'
		   AND (created_at >= $1 OR account = ANY($2))
		 GROUP BY account, asset
		 ORDER BY account, asset`

	query, args := incrementalQuery, []any{report.Watermark, carried}
	if report.FullScan {
		query, args = fullQuery, nil
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("查畸形科目失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var account, asset, total, count string
		if err := rows.Scan(&account, &asset, &total, &count); err != nil {
			return fmt.Errorf("读畸形科目失败: %w", err)
		}
		amt, err := money.Parse(total)
		if err != nil {
			return fmt.Errorf("解析畸形科目的金额 %q 失败: %w", total, err)
		}

		// 先把三个随类别而定的值算出来，再装进 Finding：默认值与覆盖写在
		// 同一处，读的人不必回头看后面有没有把它改掉。
		var (
			userID       int64
			unrepairable = "科目写法畸形，归属无法确定"
			// Detail 里的条数每轮都会变，所以它只给人看；标识用的是
			// Key（科目原文），见 Finding.Scope。
			detail = fmt.Sprintf("科目 %q，%s 条分录", account, count)
		)
		if id, ok := ledger.UserIDFromAccount(account); ok {
			// 读得出用户 id：钱已经归到那个人头上了，这条要订正的是写法。
			// 与读不出 id 的那一类分开说——不然两条的处置看起来一样，
			// 而「钱不属于任何人」才是真的钱不见了。
			userID = id
			unrepairable = "科目写法非规范，需人工订正"
			detail = fmt.Sprintf("科目 %q，%s 条分录；金额已按 user:%d 归位",
				account, count, id)
		}

		f := Finding{
			RunID: report.RunID, UserID: userID, Asset: asset,
			Kind: FindingMalformedAccount, Ledger: amt,
			Unrepairable: unrepairable, Detail: detail, Key: account,
		}
		report.Findings = append(report.Findings, f)
		report.malformed++
		if alert != nil {
			alert(ctx, f)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历畸形科目失败: %w", err)
	}
	return nil
}

// reconcileStaleReservations 找出挂了太久的未结算预留。
//
// 判据只有两条：那一行还是 pending，且它的 started_at 早于
// staleReservationAfter（阈值为什么取一天见那个常量的注释）。
//
// # 它为什么不参与分片
//
// 判据只看那一行自己，与钱包、账本都无关，所以一次查完，不按 user_id 分片。
// 代价由 billing_requests 上 `status = 'pending'` 的部分索引承担。
//
// # 判据里没有时间窗
//
// 这与钱包类的增量扫描不同：那些看的是「上次起点之后变动过的行」，而这里
// 每轮都重新筛一遍**全部** pending 行。于是两件事同时成立：
//
//   - 一条早就滞留、没人处理的预留，只要还 pending，每轮都会被报出来
//     （persistFindings 那一侧因此不必为它开特例）；
//   - 它一旦被放开或结算，下一轮不再命中判据，那一行就会被删掉。
//
// 判据与 watermark 无关，所以全量还是增量对结果没有影响。
//
// # 只报不修
//
// 见 FindingStaleReservation 的注释：自动释放与「请求还在途」有竞态。
// Unrepairable 里也写着这一句——一条报出来、等人工处置的不一致，与一条
// 报出来又被修掉的不一致，处置完全不同。
func (s *Store) reconcileStaleReservations(ctx context.Context, report *Report, alert Alert) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, reserved_asset, reserved_amount::text, request_id, started_at
		  FROM billing_requests
		 WHERE status = 'pending'
		   AND started_at < now() - make_interval(secs => $1)
		 ORDER BY started_at, request_id`, int64(staleReservationAfter.Seconds()))
	if err != nil {
		return fmt.Errorf("查陈旧的未结算预留失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			userID    int64
			asset     string
			reserved  string
			requestID string
			startedAt time.Time
		)
		if err := rows.Scan(&userID, &asset, &reserved, &requestID, &startedAt); err != nil {
			return fmt.Errorf("读陈旧的未结算预留失败: %w", err)
		}
		amt, err := money.Parse(reserved)
		if err != nil {
			return fmt.Errorf("解析预留金额 %q 失败: %w", reserved, err)
		}

		f := Finding{
			RunID: report.RunID, UserID: userID, Asset: asset,
			Kind: FindingStaleReservation,
			// Ledger 装的是这条预留冻住的金额——这一类的两个数字列不成对，
			// 见 Finding 的字段注释。Snapshot 留零。
			Ledger:       amt,
			Unrepairable: "只报告不释放：自动释放与在途请求有竞态",
			// 滞留时长按 report.StartedAt 算：它与判据里的 now() 同源
			// （都是数据库时钟），而 started_at 是应用进程写进去的墙钟，
			// 所以这个时长是个近似值。它以天计，偏差以秒计。
			Detail: fmt.Sprintf(
				"请求 %q 的预留自 %s 起一直是 pending，已滞留 %s；冻住 %s（冻结不进账本，只在预留表上）",
				requestID, startedAt.Format(time.RFC3339),
				report.StartedAt.Sub(startedAt).Round(time.Second), amt),
			// 标识用请求标识，一行一个：三条里放开一条之后，只有那一条
			// 会从清单里消失。
			Key: requestID,
		}
		report.Findings = append(report.Findings, f)
		if alert != nil {
			alert(ctx, f)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历陈旧的未结算预留失败: %w", err)
	}
	return nil
}

// malformedScopePrefix 是畸形科目在 reconcile_findings 里的标识前缀。
//
// 与 Finding.Scope 里那两个字面量共用一处：两边写死同一个字符串时，
// 改了一处而漏了另一处的表现是「那条不一致每轮都被重新发现一次」，
// 而那看起来跟「一直没解决」一模一样。
const malformedScopePrefix = "account:"

// reservationScopePrefix 是陈旧预留的标识前缀，键是请求标识。
//
// 与 malformedScopePrefix 一样只写一处：两边各写一个字面量时，改了一处而
// 漏了另一处的表现是「那条不一致每轮都被重新发现一次」，而那看起来跟
// 「一直没解决」一模一样。
const reservationScopePrefix = "request:"
