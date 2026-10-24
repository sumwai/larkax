package pricing

import (
	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
)

// UncoveredTokenItems 返回一条表达式规则没有定价的 token 计价项。
//
// 判据是「规则的结果表里有没有这个键」，不是「源码里有没有出现这个名字」：
// 键的语义见 expr.Program.Keys（所有三元分支的键并集）。结算侧的
// billing.Prices.Declared 只记命中的那一支，而根位置三元两支的 token 计价项
// 由编译期要求一致（见 expr 的 checkBranchTokenKeys），所以 token 这一侧
// 两者不会因为走了哪一支而分岔。写成
// {"prompt": 0.0000015} 这种常量表时源码里一个用量变量都没有，而 prompt
// 确实定了价；反过来 {"prompt": c > 100 ? 1.5 : 0} 里的 c 只是条件，
// 不代表 completion 被定价。
//
// 返回的顺序照 billing.TokenSlots：那是「有哪些 token 槽位、它们在规则里
// 叫什么」的唯一一份清单，这里不另抄一遍名字。全部覆盖时返回空切片。
func UncoveredTokenItems(source string) ([]string, error) {
	prog, err := expr.Compile(source)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]struct{}, len(prog.Keys()))
	for _, key := range prog.Keys() {
		declared[key] = struct{}{}
	}
	return missingTokenItems(declared), nil
}

// UncoveredScheduleTokenItems 返回一张结构化定价表没有定价的 token 计价项。
//
// 判据与 UncoveredTokenItems 相同：表先编译成表达式（见 Schedule.Validate），
// 再由同一份 Keys() 判定。不拿 Base 的键另算一遍——表的每个键在编译结果里
// 都至少出现一次，两条路径因此不会分岔。
func UncoveredScheduleTokenItems(s *Schedule) ([]string, error) {
	src, err := s.Validate()
	if err != nil {
		return nil, err
	}
	return UncoveredTokenItems(src)
}

// missingTokenItems 从已声明的键里找出漏掉的 token 计价项。
//
// 顺序由 billing.TokenSlots 决定，调用方不必再排一次。全部覆盖时返回
// 长度为零的切片（而不是 nil）：调用方只管看长度，两种形态的意义相同，
// 但「空切片」与注释里说的那件事一致。
func missingTokenItems(declared map[string]struct{}) []string {
	missing := make([]string, 0, len(billing.TokenSlots))
	for _, slot := range billing.TokenSlots {
		if _, ok := declared[slot.Name]; !ok {
			missing = append(missing, slot.Name)
		}
	}
	return missing
}
