package cli

import (
	"fmt"
	"strings"
)

// OperandSpec 描述一条命令的「核心宾语写成位置参数、同名旗标保留为别名」。
//
// price add 的规则名、user add 的用户名、plan add 的 key、model add 的规范名
// 是同一件事的四个实例：第一个位置参数是正式写法，旗标是旧写法（或者反过来，
// 见各命令的用法文本）。四条命令各写一份时，同一个错误会长出四种说法。
type OperandSpec struct {
	// What 是这一项在报错里的称呼，例如「规则名」「用户名」。
	What string
	// Flag 是兼容旗标的名字，不带 --。它同时用在「或者改用 --x」这句话里；
	// 空表示这条命令没有同名旗标，位置参数是唯一写法（见 price tier）。
	Flag string
	// Usage 是没给时该怎么说：把位置参数该排在哪写清，形状是
	// 「写成第一个位置参数（larkaxctl price add <规则名> …）」。
	Usage string
}

// TakeLeadingOperand 从参数最前面摘掉一个位置参数。
//
// 只认**最前面**那一个词，因为 flag 包遇到第一个非旗标词就停止解析：写成
// `add --scope platform n1` 时 n1 之后的一整段都落进 NArg，由调用方按「多给了
// 参数」报出来，那句话会点明位置参数要写在旗标前面。
//
// 允许它缺席：宾语既可以写成位置参数，也可以走旧写法的旗标，而旧写法前面
// 直接就是旗标。
//
// 收集补全数据时（见 collect.go 的 CollectFlags）不摘：那时走的是
// `<命令> -h`，参数里没有位置参数，而摘错一个会把 -h 吃掉。
func TakeLeadingOperand(args []string, spec OperandSpec) (string, []string, error) {
	if CollectingFlags() {
		return "", args, nil
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args, nil
	}
	operand := args[0]
	if strings.TrimSpace(operand) == "" {
		if spec.Flag == "" {
			return "", nil, UsageErr(fmt.Errorf("%s是空串：换成非空的值", spec.What))
		}
		return "", nil, UsageErr(fmt.Errorf("%s是空串：换成非空的值，或改用 --%s",
			spec.What, spec.Flag))
	}
	return operand, args[1:], nil
}

// ResolveOperand 把「位置参数」与旗标收拢成一个值。
//
// 两条都给时**同值取位置参数、异值报错**：静默取其中一个的表现是「我明明改了
// 名字，落库的还是旧的」，而那是一次成功的写入。
//
// flagGiven 传的是那个旗标这次有没有**出现在命令行上**（fs.Visit 记的），不是
// 取值的非空：`--name ""` 是一个明确的错误输入，静默当成没给会让它一路走到
// 「缺少…」或更远处。
func ResolveOperand(operand, flagValue string, flagGiven bool, spec OperandSpec) (string, error) {
	operand = strings.TrimSpace(operand)
	flagValue = strings.TrimSpace(flagValue)
	switch {
	case operand != "" && flagGiven && flagValue != operand:
		return "", fmt.Errorf(
			"%s给了两个不同的值：位置参数 %q，--%s %q；只能写一处，或者两处写成同一个值",
			spec.What, operand, spec.Flag, flagValue)
	case operand != "":
		return operand, nil
	case flagGiven:
		if flagValue == "" {
			return "", fmt.Errorf("--%s 是空串：%s不能为空", spec.Flag, spec.What)
		}
		return flagValue, nil
	}
	return "", fmt.Errorf("缺少%s：%s", spec.What, spec.Usage)
}
