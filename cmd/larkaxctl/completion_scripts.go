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
	b.WriteString("\tcase \"$path|$prev\" in\n")
	for _, key := range sortedEnumKeys(data.enums) {
		fmt.Fprintf(&b, "\t\t%s)\n\t\t\tCOMPREPLY=( $(compgen -W %q -- \"$cur\") )\n\t\t\treturn 0\n\t\t\t;;\n",
			shellCasePattern(key), strings.Join(data.enums[key], " "))
	}
	b.WriteString("\tesac\n\n")

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
	b.WriteString("\tesac\n\n")

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
		fmt.Fprintf(&b, "complete -c larkaxctl -l %s\n", strings.TrimPrefix(name, "--"))
	}
	for _, path := range flaggedPaths(data) {
		cond := fishSeenCondition(strings.Split(path, " "))
		for _, name := range ownOptionNames(data, path) {
			fmt.Fprintf(&b, "complete -c larkaxctl -n %s -l %s\n",
				fishQuote(cond), strings.TrimPrefix(name, "--"))
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
