package cli

import "errors"

// 这一组哨兵由命令文件自己造，判定却是 exit.go 的 conflictErrors（退出码 4）。
// 分类与「造这个错误」必须用**同一个值**：另造一个、或用 fmt.Errorf 包一层不带
// %w 的副本，errors.Is 就认不出来，那条命令的退出码会静默从 4 退回 1，而
// 屏幕上的那句话看起来完全正常。
//
// 声明放在 cli 里，是因为搬走 exit.go 之后分类表在 cli、造错误的地方在命令那一侧；
// 值只能有一份，所以声明跟分类表走，命令那一侧改用导出名引用。

// ErrOnboardExists 表示向导的某一步发现目标已经在库里，而这次没给 --update。
//
// 它是「一个已经配了一半」的信号，不是唯一键撞出来的：渠道白名单里有这个
// 名字、计价规则已经覆盖这个模型，库里都没有唯一约束会拒绝第二次写入。
// 归进 conflictErrors（exit.go）：下一步都是「先去库里看现在是什么，再决定
// 要不要覆盖」，与撞唯一键同一档。
var ErrOnboardExists = errors.New("已经存在")

// ErrPluginKVKeyNotFound 表示要读或要删的那个键不在。
//
// 由命令自己造，而不是从存储层拿：KV 的读与删把「不存在」与「已经过期」归成
// 同一个答案（回 false，见 kv.go 的 Get），所以那一层没有、也不该有一个表示
// 「找不到」的错误值——它的调用方（插件）看到的都是 null。命令行这一档要给出
// 一个退出码，于是这个判断落在这里（与 snapshot get 的 errSnapshotNotFound
// 同一个形状）。
var ErrPluginKVKeyNotFound = errors.New("插件 KV 里没有这个键")

// ErrSettingsNotFound 表示要看的那个键在 settings 表里没有行。
//
// 由命令自己造，不是从包里拿：internal/settings 有一个 ErrUnknownKey，但它是
// **写入路径**的哨兵——`settings set` 撞上它时给的退出码是 1（键名与取值由
// settings 包校验的那一档，见 docs/cli-reference.md 的退出码一节）。读这一侧
// 要的是「要查的那一行不在」，那是 4。两者共用一个错误值的话，改其中一个的
// 分类就把另一个的退出码也带跑了（与 errSnapshotNotFound 同一个位置）。
var ErrSettingsNotFound = errors.New("settings 里没有这一行")

// ErrSnapshotNotFound 是读路径上「要读的那一条快照不在库里」的哨兵。
//
// 它登记在 conflictErrors（exit.go）里，落 exitConflict：与「要改的那一行不
// 在」是同一个形状——下一步都是先去看数据里现在是什么，而不是改一条命令重发。
var ErrSnapshotNotFound = errors.New("要读的快照不存在")
