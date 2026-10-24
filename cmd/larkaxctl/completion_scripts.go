package main

import (
	"fmt"
	"slices"
	"strings"
)

// 本文件是三种 shell 的补全脚本生成器。脚本由数据拼出来，不是库里的静态文本：
// 静态文本会与注册表分叉，而分叉的表现是「Tab 补出来的命令敲下去说不存在」。
// 三个生成器读的是同一份 completionData（见 completion.go），只是语法不同。

// bashCompletionScript 生成 bash 补全。
//
// 路径取法：从命令行里第一个不是选项的词开始，连续的非选项词拼成路径
// （`channel map` 这种）。遇到选项就停——选项的值（`--id 3` 的 `3`）不是路径的
// 一段，继续拼下去会把 `channel get` 变成 `channel get 3`，那一条不认识。
func bashCompletionScript(data completionData) string {
	var b strings.Builder
	b.WriteString("# larkaxctl 的 bash 补全。由 `larkaxctl completion bash` 生成，不要手工修改。\n")
	b.WriteString("_larkaxctl() {\n")
	b.WriteString("\tlocal cur prev path w i\n")
	b.WriteString("\tCOMPREPLY=()\n")
	b.WriteString("\tcur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("\tprev=\"${COMP_WORDS[COMP_CWORD-1]}\"\n")
	b.WriteString("\tpath=\"\"\n")
	b.WriteString("\tfor ((i = 1; i < COMP_CWORD; i++)); do\n")
	b.WriteString("\t\tw=\"${COMP_WORDS[i]}\"\n")
	b.WriteString("\t\tcase \"$w\" in -*) break ;; esac\n")
	b.WriteString("\t\tpath=\"${path:+$path }$w\"\n")
	b.WriteString("\tdone\n\n")

	// 枚举取值：前一个词是某个枚举选项时，补它的合法值。
	//
	// 具体键之后是全局选项的通配分支：`$path` 任意、`$prev` 恰是那个选项。
	// 模式由两段拼成——`*` **不引号**才是通配符，`|--format` 则**必须引号**：
	// case 里 `|` 是多模式分隔符，`*|--format` 会被拆成 `*` 与 `--format` 两条
	// 模式，`*` 匹配任意键，这一分支就会对所有键命中并 `return`，把后面的补全
	// 全部截断。引起来的 `"|--format"` 才是字面量。
	// case 里「第一个匹配的赢」，而具体键的 `$prev` 与通配键不同，两者不冲突；
	// `larkaxctl --format <TAB>` 时 path 是空串、键是 `|--format`，
	// `*"|--format"` 的 `*` 匹配空串，一样命中。
	b.WriteString("\tcase \"$path|$prev\" in\n")
	for _, key := range sortedEnumKeys(data.enums) {
		fmt.Fprintf(&b, "\t\t%s)\n\t\t\tCOMPREPLY=( $(compgen -W %q -- \"$cur\") )\n\t\t\treturn 0\n\t\t\t;;\n",
			shellCasePattern(key), strings.Join(data.enums[key], " "))
	}
	for _, flag := range sortedEnumKeys(data.globalEnums) {
		fmt.Fprintf(&b, "\t\t*%s)\n\t\t\tCOMPREPLY=( $(compgen -W %q -- \"$cur\") )\n\t\t\treturn 0\n\t\t\t;;\n",
			shellCasePattern("|"+flag), strings.Join(data.globalEnums[flag], " "))
	}
	b.WriteString("\tesac\n\n")

	// 取值位只 return，不碰 COMPREPLY：末尾 `complete -o default` 在函数产出为空
	// 时才接手补文件名，这里的「空」就是那次回落的前提。
	writeValuePositionFallback(&b, data, "return 0")

	b.WriteString("\tcase \"$cur\" in\n")
	b.WriteString("\t-*)\n")
	b.WriteString("\t\tcase \"$path\" in\n")
	for _, path := range flaggedPaths(data) {
		fmt.Fprintf(&b, "\t\t\t%s) COMPREPLY=( $(compgen -W %q -- \"$cur\") ) ;;\n",
			shellCasePattern(path), strings.Join(completionOptionNames(data, path), " "))
	}
	fmt.Fprintf(&b, "\t\t\t*) COMPREPLY=( $(compgen -W %q -- \"$cur\") ) ;;\n",
		strings.Join(globalOptionNames(), " "))
	b.WriteString("\t\tesac\n")
	b.WriteString("\t\treturn 0\n")
	b.WriteString("\t\t;;\n")
	b.WriteString("\tesac\n\n")

	b.WriteString("\tcase \"$path\" in\n")
	for _, path := range wordPaths(data) {
		fmt.Fprintf(&b, "\t\t%s) COMPREPLY=( $(compgen -W %q -- \"$cur\") ) ;;\n",
			shellCasePattern(path), strings.Join(data.words[path], " "))
	}
	b.WriteString("\tesac\n")
	b.WriteString("}\n")
	b.WriteString("complete -o default -F _larkaxctl larkaxctl\n")
	return b.String()
}

// zshCompletionScript 生成 zsh 补全。
//
// 结构与 bash 那一份相同：路径取到第一个选项为止，先看枚举、再看选项、最后看
// 下一级动词。`-*` 分支里没有具体路径时退回全局选项。
func zshCompletionScript(data completionData) string {
	var b strings.Builder
	b.WriteString("#compdef larkaxctl\n")
	b.WriteString("# larkaxctl 的 zsh 补全。由 `larkaxctl completion zsh` 生成，不要手工修改。\n")
	b.WriteString("_larkaxctl() {\n")
	b.WriteString("\tlocal cur prev path w\n")
	b.WriteString("\tlocal i\n")
	b.WriteString("\tcur=\"${words[CURRENT]}\"\n")
	b.WriteString("\tprev=\"${words[CURRENT-1]}\"\n")
	b.WriteString("\tpath=\"\"\n")
	b.WriteString("\tfor ((i = 2; i < CURRENT; i++)); do\n")
	b.WriteString("\t\tw=\"${words[i]}\"\n")
	b.WriteString("\t\tcase \"$w\" in -*) break ;; esac\n")
	b.WriteString("\t\tpath=\"${path:+$path }$w\"\n")
	b.WriteString("\tdone\n\n")

	b.WriteString("\tcase \"$path|$prev\" in\n")
	for _, key := range sortedEnumKeys(data.enums) {
		fmt.Fprintf(&b, "\t\t%s) compadd -- %s; return ;;\n",
			shellCasePattern(key), strings.Join(data.enums[key], " "))
	}
	// 全局选项的通配分支，形状与 bash 那一份相同：`*` 不引号才是通配符，
	// `"|--format"` 必须引号，否则 `|` 被当成多模式分隔符（理由见 bash 处注释）。
	for _, flag := range sortedEnumKeys(data.globalEnums) {
		fmt.Fprintf(&b, "\t\t*%s) compadd -- %s; return ;;\n",
			shellCasePattern("|"+flag), strings.Join(data.globalEnums[flag], " "))
	}
	b.WriteString("\tesac\n\n")

	// zsh 没有 bash 的 `-o default`：函数不产出候选时它不会自己去补文件，
	// 取值位要显式调 `_files`。
	writeValuePositionFallback(&b, data, "_files; return")

	b.WriteString("\tcase \"$cur\" in\n")
	b.WriteString("\t-*)\n")
	b.WriteString("\t\tcase \"$path\" in\n")
	for _, path := range flaggedPaths(data) {
		fmt.Fprintf(&b, "\t\t\t%s) compadd -- %s ;;\n",
			shellCasePattern(path), strings.Join(completionOptionNames(data, path), " "))
	}
	fmt.Fprintf(&b, "\t\t\t*) compadd -- %s ;;\n", strings.Join(globalOptionNames(), " "))
	b.WriteString("\t\tesac\n")
	b.WriteString("\t\treturn\n")
	b.WriteString("\t\t;;\n")
	b.WriteString("\tesac\n\n")

	b.WriteString("\tcase \"$path\" in\n")
	for _, path := range wordPaths(data) {
		fmt.Fprintf(&b, "\t\t%s) compadd -- %s ;;\n",
			shellCasePattern(path), strings.Join(data.words[path], " "))
	}
	b.WriteString("\tesac\n")
	b.WriteString("}\n")
	b.WriteString("if [ \"$funcstack[1]\" = \"_larkaxctl\" ]; then\n")
	b.WriteString("\t_larkaxctl \"$@\"\n")
	b.WriteString("else\n")
	b.WriteString("\tcompdef _larkaxctl larkaxctl\n")
	b.WriteString("fi\n")
	return b.String()
}

// fishCompletionScript 生成 fish 补全。
//
// fish 的补全按条件挂：一条命令路径用 `__fish_seen_subcommand_from` 把它那几段
// 连起来。选项按路径各挂一次，全局选项挂一次（它们写在子命令前后都认）。
func fishCompletionScript(data completionData) string {
	var b strings.Builder
	b.WriteString("# larkaxctl 的 fish 补全。由 `larkaxctl completion fish` 生成，不要手工修改。\n")
	b.WriteString("complete -c larkaxctl -f\n\n")

	for _, command := range data.commands {
		fmt.Fprintf(&b, "complete -c larkaxctl -n '__fish_use_subcommand' -a %s",
			fishQuote(command.name))
		if command.summary != "" {
			fmt.Fprintf(&b, " -d %s", fishQuote(command.summary))
		}
		b.WriteString("\n")
	}

	for _, path := range wordPaths(data) {
		if path == "" {
			continue
		}
		cond := fishSeenCondition(strings.Split(path, " "))
		for _, word := range data.words[path] {
			fmt.Fprintf(&b, "complete -c larkaxctl -n %s -a %s\n",
				fishQuote(cond), fishQuote(word))
		}
	}

	for _, name := range globalOptionNames() {
		suffix := ""
		if slices.Contains(globalValueFlags, name) {
			// 与自有选项同一条判据：`-r` 声明吃取值，文件补全仍开着。
			// 枚举类的全局选项走下面那条 `-x -a`，这里不加。
			suffix = " -r"
		}
		fmt.Fprintf(&b, "complete -c larkaxctl -l %s%s\n",
			strings.TrimPrefix(name, "--"), suffix)
	}
	// 全局选项的取值挂无条件的那一条：fish 的选项在子命令前后都认，不加
	// `__fish_seen_subcommand_from` 条件；加了反而只在某几条路径下补得出来。
	for _, flag := range sortedEnumKeys(data.globalEnums) {
		fmt.Fprintf(&b, "complete -c larkaxctl -l %s -x -a %s\n",
			strings.TrimPrefix(flag, "--"),
			fishQuote(strings.Join(data.globalEnums[flag], " ")))
	}
	for _, path := range flaggedPaths(data) {
		cond := fishSeenCondition(strings.Split(path, " "))
		for _, option := range ownOptions(data, path) {
			// 吃取值的选项要声明 `-r`，fish 才会停下来等取值；不声明的话它把
			// 选项当开关，敲完按 Tab 会按位置接着补动词。
			//
			// 有枚举的选项不加：取值由下面那条 `-x -a` 补。`-r` 与 `-x` 也不是
			// 同一件事——`-r` 只说明后面跟一个取值，文件补全仍然开着；`-x` 才
			// 连文件一起关掉。补文件路径的选项（如 `--schedule-file`）要的正是
			// 前者。
			suffix := ""
			if option.takesValue {
				if _, wired := data.enums[path+"|"+option.name]; !wired {
					suffix = " -r"
				}
			}
			fmt.Fprintf(&b, "complete -c larkaxctl -n %s -l %s%s\n",
				fishQuote(cond), strings.TrimPrefix(option.name, "--"), suffix)
		}
	}
	for _, key := range sortedEnumKeys(data.enums) {
		path, flag, ok := strings.Cut(key, "|")
		if !ok {
			continue
		}
		cond := fishSeenCondition(strings.Split(path, " "))
		fmt.Fprintf(&b, "complete -c larkaxctl -n %s -l %s -x -a %s\n",
			fishQuote(cond), strings.TrimPrefix(flag, "--"),
			fishQuote(strings.Join(data.enums[key], " ")))
	}
	return b.String()
}

// fishSeenCondition 把一条路径翻成 fish 的条件。
func fishSeenCondition(path []string) string {
	parts := make([]string, 0, len(path))
	for _, token := range path {
		parts = append(parts, fmt.Sprintf("__fish_seen_subcommand_from %s", token))
	}
	return strings.Join(parts, " and ")
}

// fishQuote 把一段文本放进 fish 的单引号里。
func fishQuote(text string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), "'", `\'`) + "'"
}

// shellCasePattern 把一个路径键放进 case 分支的模式位置。
//
// 键里只有命令名、动作名与选项名（字母、数字、连字符、空格与竖线），
// 不含 shell 的元字符，所以直接用双引号包住即可。
func shellCasePattern(key string) string {
	return `"` + key + `"`
}

// writeValuePositionFallback 写出「取值位」那一段：`$prev` 是吃取值的选项时，
// 把补全交回给 shell 自己（bash 传 `return 0`，zsh 传 `_files; return`）。
//
// 每条分支里的内层 `case "$cur" in -*` 是守卫：当前词以 `-` 开头时什么都不做，
// 落回下面那段的选项名补全。没有它，`larkaxctl --env-file -<TAB>` 一个候选都不给
// ——敲到这里的人下一步想要的是另一个选项，不是文件名。
//
// 位置**不能挪**，这是本段唯一的坑：
//   - 挪到下面 `case "$cur"` 的动词分派之后：空 `$cur` 会先被填上一级命令，再交回
//     shell 已经晚了，顶层 `-o default` 的回落照样被压掉。
//   - 挪进 `case "$cur" in -*` 的嵌套分支：那一支只处理以 `-` 开头的当前词，空
//     `$cur` 时它根本不执行，这一段等于没写；`$cur` 以 `-` 开头时守卫让两版一样，
//     那一格分不出来。
//
// 两种放法在行为用例覆盖的那几格（`--env-file`、`--schedule-file`）上真 shell 里都
// 看得见，但行为用例只覆盖那几格：生成器漏掉某一条键时，别的位置静默退回动词分派，
// 补出来的是一级命令。所以 completion_test.go 另有一条位置断言，按 valueOptionKeys
// 逐条钉住「在顶层 `case "$cur"` 之前、恰好一份」。
func writeValuePositionFallback(b *strings.Builder, data completionData, action string) {
	b.WriteString("\tcase \"$path|$prev\" in\n")
	for _, key := range valueOptionKeys(data) {
		writeValuePositionBranch(b, shellCasePattern(key), action)
	}
	// 全局选项对每条命令都生效，路径用 `*` 通配，写法与全局枚举那一支相同：
	// `*` 不引号才是通配符，`|--env-file` 必须引号（否则 `|` 是多模式分隔符）。
	names := slices.Clone(globalValueFlags)
	slices.Sort(names)
	for _, name := range names {
		writeValuePositionBranch(b, "*"+shellCasePattern("|"+name), action)
	}
	b.WriteString("\tesac\n\n")
}

// writeValuePositionBranch 写出一条取值位分支：守卫 + 两壳各自的动作。
func writeValuePositionBranch(b *strings.Builder, pattern, action string) {
	fmt.Fprintf(b, "\t\t%s)\n", pattern)
	b.WriteString("\t\t\tcase \"$cur\" in\n")
	b.WriteString("\t\t\t-*) ;;\n")
	fmt.Fprintf(b, "\t\t\t*) %s ;;\n", action)
	b.WriteString("\t\t\tesac\n")
	b.WriteString("\t\t\t;;\n")
}

// wordPaths 是 words 的键，按字典序且空路径排在最前面（它是顶层那一支）。
func wordPaths(data completionData) []string {
	paths := make([]string, 0, len(data.words))
	for path := range data.words {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	// 空串排在任何非空键之前，正是 slices.Sort 给出的顺序。
	return paths
}

// flaggedPaths 是注册过自己选项的路径，按字典序。
func flaggedPaths(data completionData) []string {
	var paths []string
	for path := range data.flags {
		if len(ownOptionNames(data, path)) > 0 {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

// sortedEnumKeys 按字典序返回枚举表的键，生成结果才稳定。
func sortedEnumKeys(enums map[string][]string) []string {
	keys := make([]string, 0, len(enums))
	for key := range enums {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
