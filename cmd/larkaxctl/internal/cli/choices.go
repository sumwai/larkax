package cli

import "strings"

// JoinChoiceNames 把一组合法取值渲染成错误信息里那一串：顿号分隔，最后一项前用「或」。
//
// `--format` 与 `--endpoint` 两处报错共用这一条规则：各写一份时，分隔符或空格
// 的写法一旦分岔（「a 或 b」与「a或b」差一个空格就看不出来），同一条命令行的
// 两句报错会长成两种样子。
//
// 一项时原样返回，不补分隔符。
func JoinChoiceNames(names []string) string {
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			if i == len(names)-1 {
				b.WriteString(" 或 ")
			} else {
				b.WriteString("、")
			}
		}
		b.WriteString(name)
	}
	return b.String()
}
