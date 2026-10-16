package snapshot

import (
	"errors"
	"fmt"
)

// ErrRedaction 是「这条快照必须被丢掉」这一结论的标记。
//
// 本包只返回带这个标记的错误，调用方因此不必逐个错误去猜该不该丢：
// 拿到任何错误就丢整条快照。不提供「部分结果 + 错误」的返回形态，
// 是因为那种形态的下一步总是「那先用已有的部分吧」，而「部分骨架」与
// 「完整的旧骨架」混在一列数据里之后，没人分得清哪条可信。
//
// 与之配套的一条：本包绝不返回「错误为 nil、骨架里却是原文」的结果。
// 全包只有制造占位的函数会往骨架里写字符串，它们写入的都是本包自己产生的常量、
// 长度与引用编号（`T1`、`T2`），没有任何一处写入上游或客户端传来的字符串。
var ErrRedaction = errors.New("快照脱敏失败")

// redactionError 用 %w 包装 ErrRedaction，保留「哪一步失败、为什么」。
//
// 错误信息里只带字段路径与类型名，不带取值：脱敏器的错误会进日志，
// 而日志的留存期与传播范围都远大于快照本身——定位问题只需要路径，
// 把取值写进错误信息恰好是这一步要防的事。
func redactionError(path, reason string, args ...any) error {
	detail := fmt.Sprintf(reason, args...)
	if path == "" {
		return fmt.Errorf("%w: %s", ErrRedaction, detail)
	}
	return fmt.Errorf("%w: 路径 %s: %s", ErrRedaction, path, detail)
}

// joinPath 拼出 JSON 路径，形如 messages[0].content.text。
//
// 点号分段、数组下标用方括号，是 JSONPath 的常见写法：这份路径会落进
// redacted_paths 供人直接读，也会被拿去和「原本以为该保留的字段」对照，
// 写法不能让读的人先做一次翻译。
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// indexPath 拼出数组元素的路径。
func indexPath(parent string, i int) string {
	return fmt.Sprintf("%s[%d]", parent, i)
}
