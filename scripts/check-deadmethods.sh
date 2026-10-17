#!/usr/bin/env bash
# 「导出方法没有调用点」的门禁：扫入口可达的包，报出在生产代码里没人调的导出方法。
#
# # 为什么需要它
#
# scripts/check-deadcode.sh 跑的是 golang.org/x/tools 的 deadcode，而它底下的 RTA
# 把**任何 runtime type 的全部导出方法**无条件标为可达，理由是反射
#（go/callgraph/rta/rta.go 开头那段：所有被取地址的函数、以及**任何 runtime type
# 的导出方法**都必须算作可达，因为它们可能被反射调用）。本项目不用反射，于是
# 这一整类都被掩蔽了，而且**掩蔽会沿调用链传递**：一个被掩蔽的方法若是某个自由
# 函数唯一的调用点，那个自由函数也一并不在清单里。
#
# 后果与 check-deadcode.sh 开头写的是同一条（LESSONS.md 第 22 条）：写完一个方法、
# 写完测试、门禁全绿、合并，然后那个方法永远没人调，而 CI 一次都不会响。
#
# 实测：在 internal/calendar 的 New 里加一行把 *MemorySource 塞进接口值，
# deadcode 的清单从 104 条掉到 97 条——MemorySource 的 5 个导出方法连同只被它们
# 调用的 Date.cmp 与 sign 一起消失。它们**本来在清单里**，是那一行代码让 RTA 认为
# 它们「可能被反射调用」。所以「清单里没有」在那边分不出「有人调」与「被掩蔽」。
#
# # 判据
#
# 这一道门不看调用图，看**语法**：一个导出方法（接收者方法，方法名首字母大写）
# 在**入口可达的、非测试的** Go 文件里没有出现过调用语法 `.Name(`，就报出来。
#
# 判据走 Go 的 AST（go/parser，源码嵌在下面的 heredoc 里现构），不是 grep：
# 注释与字符串字面量里出现 `.Name(` 不算调用点。这不是洁癖——本仓的文档注释里
# 就有成段的调用示例（internal/tokens/stream.go 的用法示例一口气写了 5 个方法
# 调用），按文本匹配会把那 5 个方法全部算成「有人调」。
#
# # 与 check-deadcode.sh 的分工与重叠
#
# 两道门的扫描范围一样（cmd/ 下自动发现的 main 包可达的包），判据不同：
#
#   check-deadcode.sh    问「从入口出发的调用图走得到它吗」——RTA，精确，
#                        但对导出方法有一整类结构性盲区
#   check-deadmethods.sh 问「生产代码里有没有 `.Name(` 这处调用」——语法，
#                        对导出方法没有盲区，但看不见跨包的接口调用
#
# **重叠是故意的**：`MemorySource.AddDay` 这类符号两道门都会报（那边因为它不是
# runtime type，这边因为没人调），于是两份白名单各记一条。不共享是有意的：
# 共享之后「改一处要动两处」会变成三处，而两道门的判据将来还可能各自调整。
#
# # 边界（假阳性 / 假阴性）
#
# **只认调用语法 `.Name(`。** 字段访问与「方法值」在语法上不可区分（`x.Units`
# 既可能是字段也可能是方法），取严的一侧才不会把同名的字段访问当成调用点。
# 代价：只被当作方法值传递（`f := x.Name`，不带括号）的方法会被误报。本仓目前
# 没有这种写法，自测里钉住了这个取舍。
#
# **看不见跨包的接口调用。** 调用方在别的模块里时（标准库经接口回调的方法都是
# 这样：errors.Is 调 Unwrap、encoding/json 调 UnmarshalJSON、fmt 调 GoString、
# log/slog 调 LogValue），文本扫描看不到调用点。这类是**结构性**假阳性，不是
# 「没人调」，所以在扫描器里按一份具名的名单排除（STDLIB_INTERFACE_METHODS），
# 每一轮把排除了几条打印出来，名单变长是看得见的。
#
# **同名方法互相掩护。** 判据按**方法名**做，不解析接收者的静态类型，所以只要
# 有别的类型有一个被调用的同名方法，同名的都算「有人调」。这是这道门主要的
# 假阴性来源：`catalog.Admin.Update` 与 `pricing.Admin.Update` 都是死的，而
# `.Update(` 在别处出现过，于是两个都不在清单里。
#
# **自递归**：只被自己调用的方法（`func (c *Cache) X() { c.X() }`）算成有人调。
# 环的处置 deadcode 那边更细，这一条留给它。
#
# # 用法
#
#   scripts/check-deadmethods.sh
#
# 退出码：
#   0  白名单与现状一致（也可能有已经可以删掉的陈旧条目，见下——那不算失败）
#   1  门禁失败：有新出现的无调用点导出方法、条目数超过上限、或白名单自身不合格式
#   2  没跑成：go 不在、go list 失败、扫描器构建或执行失败、输出认不出、
#      cmd/ 下扫不到 main 包
#
# 1 与 2 必须分开：检查工具坏掉时的输出和「干净」长得一样，退出码也一样
#（LESSONS.md 第 16 条）。
#
# # 白名单
#
# scripts/deadmethods-allowlist.txt，格式与两档类型见该文件开头。口径与
# check-deadcode.sh / check-unimported.sh 一致：
#
#   **新出现的无调用点导出方法**（在扫描结果里、不在白名单里）→ 失败。
#   **陈旧的条目**（在白名单里、不在扫描结果里）→ 只提示，不算失败。它出现的
#   原因恰恰是「有人把没门的东西接上线了」，那是个好动作。
#
# **不要为了把清单刷空而去硬接调用方**——接一条假的比留着它更坏（LESSONS.md 第 22 条）。
#
# # 环境变量
#
# 这三个是给 scripts/check-deadmethods.test.sh 注入桩件用的，CI 与本地都不该设：
#   GO          可执行文件，默认 go。只用于 go list；自测把它换成桩件。
#   HELPER      已构建好的扫描器，给定则跳过构建。自测用它省掉每个用例一次构建。
#   WHITELIST   白名单路径，默认 scripts/deadmethods-allowlist.txt

set -uo pipefail
cd "$(dirname "$0")/.."

# 白名单条目数上限。**只许减不许增**：新增一条就要同时把这个数往上调，
# 而那是一次会被 review 看到的 diff。自测把这个字面量钉住（见
# check-deadmethods.test.sh）——所以往上调要动两个文件，不会悄悄发生。
#
# 关掉一批缺口之后把它往下调。不调不会报错，但那样它就不再是当前存量的快照，
# 中间那一段差额可以被人无声地用掉。
#
# 取值走 MAX_ENTRIES_DEFAULT 而不是直接写 MAX_ENTRIES：自测的比较用例用的是
# 自己造的小白名单，条数与真实存量无关，所以它们要能把这个上限顶开——否则
# 真实上限降到比夹具还小时，那些用例会全部挂在「超过上限」上，而它们要测的
# 是比对逻辑。上限本身由自测的固定用例按下面这个默认值钉住。
MAX_ENTRIES_DEFAULT=48
MAX_ENTRIES="${MAX_ENTRIES:-$MAX_ENTRIES_DEFAULT}"

# 扫描起点的根目录。里面的 main 包自动发现，见下面的 discover_main_packages。
SCAN_ROOT="cmd"

WHITELIST="${WHITELIST:-scripts/deadmethods-allowlist.txt}"
GO="${GO:-go}"
HELPER="${HELPER:-}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# ── 工具 ──────────────────────────────────────────────────────────────────

go_bin=$(command -v "$GO" 2>/dev/null) || not_run "找不到 $GO（go 不在 PATH 里）。
这道门用 go list 算入口可达的包集合、用 go 构建扫描器——go 不在时它什么都没检查，
所以这里不当作通过。"

# 构建扫描器用的 go。**刻意不取 $GO**：$GO 是自测的注入点，桩件只认识 go list
# 那几种调用形状；构建要的是真的编译器。
go_build=$(command -v go 2>/dev/null) || not_run "找不到 go（不在 PATH 里）。
扫描器要从脚本里嵌的源码现构，没有 go 就构不出来。"

# ── 扫描起点 ──────────────────────────────────────────────────────────────
#
# 从 $SCAN_ROOT 里**自动发现** main 包，不手写清单。理由是这一条：
# 「新加了一个 main 包、没人记得补进清单，于是那个包整片没有门，而这一轮的
# 输出看起来完全正常（条数没变）」——那正是这道门自己要防的那类失败。
# **一道需要人记得同步维护的门，会以它要防的方式坏掉。**
#
# 认的是「非测试的 .go 文件里写着 package main」，也就是可执行包的定义，
# 与 check-deadcode.sh / check-unimported.sh 里那份是同一份实现。
discover_main_packages() {
  local root="$1" d f
  local files=()
  for d in "$root"/*/; do
    [ -d "$d" ] || continue
    files=()
    for f in "$d"*.go; do
      case "$f" in *_test.go) continue ;; esac
      [ -f "$f" ] && files+=("$f")
    done
    [ "${#files[@]}" -gt 0 ] || continue
    grep -qE '^package[[:space:]]+main[[:space:]]*$' "${files[@]}" || continue
    printf './%s\n' "${d%/}"
  done
}

TARGETS=()
while IFS= read -r pkg; do
  [ -n "$pkg" ] && TARGETS+=("$pkg")
done < <(discover_main_packages "$SCAN_ROOT")

# 一个都没发现时必须报错：没有扫描起点就等于什么都没检查，
# 而那个结果与「全部命中白名单」在屏幕上没有区别。
if [ "${#TARGETS[@]}" -eq 0 ]; then
  not_run "$SCAN_ROOT/ 下面没找到 main 包（认的是非测试 .go 文件里的 package main）。
没有扫描起点 = 什么都没检查，所以这里不当作通过。"
fi

echo "扫描起点 ${TARGETS[*]}"

# ── 模块路径 ──────────────────────────────────────────────────────────────
#
# 用来把标准库与第三方从 -deps 的输出里滤掉。模块路径取自 go list -m，
# 不写死在脚本里——写死的话换模块名（或换 fork）时，过滤会静默地一个都不匹配，
# 而那时输出看着正常，只是结论全错。

mod_out="$tmpdir/mod"
mod_err="$tmpdir/mod.err"
"$go_bin" list -m >"$mod_out" 2>"$mod_err"
rc=$?
if [ "$rc" -ne 0 ]; then
  echo "错误：go list -m 失败（退出码 $rc）。这不是「干净」，是没跑成：" >&2
  sed 's/^/   /' "$mod_out" "$mod_err" >&2
  exit 2
fi
mod_lines=$(wc -l <"$mod_out")
MOD=$(head -n1 "$mod_out")
if [ "$mod_lines" -ne 1 ] || [ -z "$MOD" ] || [[ "$MOD" =~ [[:space:]] ]]; then
  echo "错误：go list -m 的输出认不出（$mod_lines 行）：" >&2
  sed 's/^/   /' "$mod_out" >&2
  exit 2
fi
echo "模块 $MOD"

# ── 入口可达的包，与每个包参与构建的文件 ──────────────────────────────────
#
# 取 {{.GoFiles}} 而不是「目录下所有 .go」：GoFiles 是**这个包实际参与构建的
# 文件**，已经排掉了测试文件与被 build tag 挡掉的文件（例如 internal/assembly
# 下带 contract tag 的那些）。自己按目录扫会把它们算进来，于是那些文件里的导出
# 方法会以「没人调」的样子出现，而它们根本不在生产构建里。

deps_out="$tmpdir/deps.out"
deps_err="$tmpdir/deps.err"
"$go_bin" list -deps -f '{{.ImportPath}}|{{.Dir}}|{{join .GoFiles " "}}' "${TARGETS[@]}" \
  >"$deps_out" 2>"$deps_err"
rc=$?
if [ "$rc" -ne 0 ]; then
  echo "错误：go list -deps 失败（退出码 $rc）。这不是「干净」，是没跑成：" >&2
  sed 's/^/   /' "$deps_out" "$deps_err" >&2
  exit 2
fi
if [ -s "$deps_err" ]; then
  # 不是失败判据：需要下载模块时 go 会写 `go: downloading …`，
  # 按 stderr 判失败会把「第一次在干净环境里跑」变成一次假警报（LESSONS.md 第 13 条）。
  echo "· go list -deps 往 stderr 写了内容（多为模块下载进度，不是失败）："
  sed 's/^/   /' "$deps_err"
fi
[ -s "$deps_out" ] || not_run "go list -deps 一行都没输出。
可达集合为空会让扫描范围变成零个包，而「零个包」与「全部命中白名单」在屏幕上
没有区别，所以这里不当作通过。"

# 认不出的行不能丢：那正是「工具换了措辞或换了参数」的样子（比如有人给它加上
# -json）。丢掉之后扫描范围会少一批，而结果看起来像「差异变小了」。
deps_bad=$(awk -F'|' 'NF != 3 { n++ } END { print n + 0 }' "$deps_out")
if [ "$deps_bad" -ne 0 ]; then
  not_run "go list -deps 的输出里有 $deps_bad 行不是三栏（<import path>|<目录>|<文件…>）。
工具换了措辞或换了参数时就是这样——不能把它当成「没有差异」。"
fi

# 只留本仓库的包，并把它变成扫描器的输入：<包目录>|<文件名…>。
# 前缀匹配的两个分支都要有：只写 `index(...) == 1` 的话，模块根目录下那个包
#（import path 恰好等于模块路径）会被滤掉，而它是最该被看见的一个。
scan_in="$tmpdir/scan.in"
awk -v mod="$MOD" -F'|' '
  ($1 == mod || index($1, mod "/") == 1) && $3 != "" { print $2 "|" $3 }
' "$deps_out" | sort -u >"$scan_in"

pkg_count=$(wc -l <"$scan_in")
if [ "$pkg_count" -eq 0 ]; then
  not_run "按模块前缀 $MOD 过滤之后，可达集合里一个本仓库的包都没有。
模块路径读错、或者入口根本没 import 自己的任何包时就是这样——两种都不该当作通过。"
fi
echo "入口可达的包 $pkg_count 个"

# ── 扫描器 ────────────────────────────────────────────────────────────────
#
# 源码嵌在下面，现构一份。用 Go 的 AST 而不是 grep：注释与字符串字面量里的
# `.Name(` 不算调用点，而本仓的文档注释里就有成段的调用示例。
#
# 它只依赖标准库，所以不需要额外的依赖，也不需要网络。
#
# 输入：stdin，每行 `<包目录>|<文件名…>`。
# 输出：stdout，先一行 `# files=… decls=… called=… excluded=…`，
#       随后每行 `<相对路径>|<接收者类型>.<方法名>`。

if [ -n "$HELPER" ]; then
  scanner="$HELPER"
else
  helper_src="$tmpdir/deadmethods-scan.go"
  cat >"$helper_src" <<'DEADMETHODS_SCAN_GO'
// Command deadmethods-scan 报出「在非测试的生产代码里没有调用点的导出方法」。
//
// 为什么需要它：golang.org/x/tools 的 RTA 把**任何 runtime type 的全部导出方法**
// 无条件标为可达（理由是反射，见 go/callgraph/rta/rta.go 开头那段），于是
// `deadcode` 对导出方法有一整类结构性盲区，而且掩蔽会沿调用链传递——被掩蔽的
// 方法若是某个自由函数唯一的调用点，那个自由函数也一起看不见。本项目不用反射，
// 所以这一整类都该被看见。
//
// 判据：一个导出方法在**入口可达的、非测试的**代码里没有出现过调用语法
// `.Name(`，就报出来。只认调用语法是刻意的——字段访问与「方法值」在语法上
// 不可区分（`x.Units` 既可能是字段也可能是方法），取严的一侧才不会把
// 同名的字段访问当成调用点。
//
// 输入：标准库 stdin，每行 `<包目录>|<该包参与构建的文件名，空格分隔>`。
// 输出：stdout，先一行 `# files=… decls=… called=… excluded=…`（正向证据），
// 随后每行 `<相对路径>|<接收者类型>.<方法名>`，已排序去重。
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// STDLIB_INTERFACE_METHODS 是标准库经**接口**调用的方法名：调用方在标准库里，
// 文本扫描看不到那些调用点，于是它们是这道门的结构性假阳性，不是「没人调」。
//
// 名单只收**打印与序列化**这一族，不收 io / net/http / sort 那一族：后者在业务
// 代码里被直接调用的概率高，收进来会变成一片没人盯的盲区。加名字之前先确认它
// 确实由标准库经接口回调——每加一个都是一处永久的盲区，而这份名单本身是**看不见**
// 的（每一轮只打印排除了几条，不打印排除了哪些）。
//
// 名单里目前真的被用到的：Unwrap、UnmarshalJSON、GoString、MarshalText、
// UnmarshalText、LogValue。其余几个（Error / Is / As / String / Format /
// MarshalBinary / UnmarshalBinary）是同一族里可预见的成员，收在这里是为了
// 一次说清边界，而不是等它们各自冒出来时再补。
var stdlibInterfaceMethods = map[string]bool{
	// error 与 errors.Is / errors.As 的回调。
	"Error": true, "Unwrap": true, "Is": true, "As": true,
	// fmt 的三个打印接口与 log/slog 的取值接口。
	"String": true, "GoString": true, "Format": true, "LogValue": true,
	// encoding 的三对序列化接口。
	"MarshalJSON": true, "UnmarshalJSON": true,
	"MarshalText": true, "UnmarshalText": true,
	"MarshalBinary": true, "UnmarshalBinary": true,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "deadmethods-scan: %v\n", err)
		os.Exit(1)
	}
}

type decl struct {
	file string
	recv string
	name string
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	fset := token.NewFileSet()
	var decls []decl
	called := map[string]bool{}
	files, excluded := 0, 0

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		dir, list, ok := strings.Cut(line, "|")
		if !ok {
			return fmt.Errorf("输入行认不出（应为 <目录>|<文件名…>）：%q", line)
		}
		for _, name := range strings.Fields(list) {
			path := filepath.Join(dir, name)
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return fmt.Errorf("算 %s 的相对路径失败：%w", path, err)
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("解析 %s 失败：%w", path, err)
			}
			files++
			collectDecls(f, rel, &decls)
			collectCalls(f, called)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// 一个文件都没扫到时不能报「没有不可达符号」——那与「全部命中白名单」
	// 长得一模一样（LESSONS.md 第 16 条）。
	if files == 0 {
		return fmt.Errorf("一个文件都没扫到")
	}

	sort.Slice(decls, func(i, j int) bool {
		if decls[i].file != decls[j].file {
			return decls[i].file < decls[j].file
		}
		if decls[i].recv != decls[j].recv {
			return decls[i].recv < decls[j].recv
		}
		return decls[i].name < decls[j].name
	})

	// 先算报告，再打统计行：excluded 是报告的一部分，统计行要把它算进去。
	report := make([]string, 0, len(decls))
	seen := map[string]bool{}
	for _, d := range decls {
		if called[d.name] {
			continue
		}
		if stdlibInterfaceMethods[d.name] {
			excluded++
			continue
		}
		key := d.file + "|" + d.recv + "." + d.name
		if seen[key] {
			continue
		}
		seen[key] = true
		report = append(report, key)
	}

	fmt.Printf("# files=%d decls=%d called=%d excluded=%d\n", files, len(decls), len(called), excluded)
	for _, key := range report {
		fmt.Println(key)
	}
	return nil
}

// collectDecls 收集导出方法声明。接收者类型名从接收者表达式里剥出来，
// 泛型接收者 `*T[K]` 与括号写法 `(*T)` 都归到 `T`。
func collectDecls(f *ast.File, rel string, out *[]decl) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		if !fd.Name.IsExported() {
			continue
		}
		recv := recvTypeName(fd.Recv.List[0].Type)
		if recv == "" {
			continue
		}
		*out = append(*out, decl{file: rel, recv: recv, name: fd.Name.Name})
	}
}

// collectCalls 收集「调用语法」里出现的选择子名字：`x.Name(…)` 与方法表达式
// `T.Name(…)` 都落在 `CallExpr.Fun` 是 `SelectorExpr` 这一条上。
//
// 只看 `CallExpr`，不看所有 `SelectorExpr`：后者会把字段访问（`f.Units`）
// 与注释里的调用示例一起算成调用点，而这两类恰恰是这道门要穿透的东西。
// 代价是「只被当作方法值传递」（`f := x.Name`，不带括号）的方法会被误报。
func collectCalls(f *ast.File, called map[string]bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.IsExported() {
			called[sel.Sel.Name] = true
		}
		return true
	})
}

// recvTypeName 剥掉指针、括号与类型实参，取接收者的类型名。
func recvTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}
DEADMETHODS_SCAN_GO

  scanner="$tmpdir/deadmethods-scan"
  build_out=$("$go_build" build -o "$scanner" "$helper_src" 2>&1)
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "错误：扫描器构建失败（退出码 $rc）。这不是「干净」，是没跑成：" >&2
    printf '%s\n' "$build_out" | sed 's/^/   /' >&2
    exit 2
  fi
fi

[ -x "$scanner" ] || not_run "扫描器 $scanner 不是一个可执行文件。"

# ── 扫描 ──────────────────────────────────────────────────────────────────

scan_out="$tmpdir/scan.out"
scan_err="$tmpdir/scan.err"
"$scanner" <"$scan_in" >"$scan_out" 2>"$scan_err"
rc=$?
if [ "$rc" -ne 0 ]; then
  echo "错误：扫描器执行失败（退出码 $rc）。这不是「干净」，是没跑成：" >&2
  sed 's/^/   /' "$scan_out" "$scan_err" >&2
  exit 2
fi
# 成功的扫描器不往 stderr 写东西。写了说明这一轮有它自己才知道的状况，
# 而「有噪音但结论看着正常」正是最容易被放过的一类。
if [ -s "$scan_err" ]; then
  echo "错误：扫描器往 stderr 写了东西，先看一眼再决定这一轮算不算数：" >&2
  sed 's/^/   /' "$scan_err" >&2
  exit 2
fi

# 输出的形状：一行 `# files=… decls=… called=… excluded=…`，随后
# `<相对路径>|<类型>.<方法名>`。认不出的行不能丢——那正是「工具换了输出格式」
# 的样子，丢掉它之后就变成「一个符号都没扫到」，而那个结果与「全部命中白名单」
# 在屏幕上没有区别。
scan_keys="$tmpdir/scan-keys"
: >"$scan_keys"
scan_stats=""
scan_bad=0
while IFS= read -r line || [ -n "$line" ]; do
  [ -n "$line" ] || continue
  case "$line" in
    '# '*)
      scan_stats="$line"
      continue
      ;;
  esac
  if [[ "$line" =~ ^([^|]+)\|([A-Za-z_][A-Za-z0-9_]*)\.([A-Z][A-Za-z0-9_]*)$ ]]; then
    printf '%s\n' "$line" >>"$scan_keys"
  else
    echo "   $line" >&2
    scan_bad=$((scan_bad + 1))
  fi
done <"$scan_out"

if [ "$scan_bad" -ne 0 ]; then
  echo "错误：上面 $scan_bad 行认不出，不是扫描器该有的输出。" >&2
  echo "  工具换了措辞时就是这样——不能把它当成「没有无调用点的导出方法」。" >&2
  exit 2
fi

# 统计行是这一轮的**正向证据**：扫了几个文件、见了几个声明、几个调用点名字、
# 按标准库接口方法排除了几个。它不在的时候，「什么都没扫到」与「干净」长得一样。
if [ -z "$scan_stats" ]; then
  not_run "扫描器没给出统计行（应有一行以 '# ' 开头）。
缺了它就没有「扫了几个文件」这条正向证据，而「没扫到」与「干净」在屏幕上没有区别。"
fi
# 四个数各自取出来。files 是**正向证据**：扫了几个文件。少了它，
# 「什么都没扫到」与「干净」在屏幕上没有区别。
stat_field() {
  local v
  v=$(sed -nE "s/.* $1=([0-9]+).*/\1/p" <<<"$scan_stats")
  printf '%s' "$v"
}
scan_files=$(stat_field files)
scan_decls=$(stat_field decls)
scan_called=$(stat_field called)
scan_excluded=$(stat_field excluded)
if [ -z "$scan_files" ] || [ "$scan_files" -eq 0 ] ||
  [ -z "$scan_decls" ] || [ -z "$scan_called" ] || [ -z "$scan_excluded" ]; then
  not_run "统计行读不全（要有 files / decls / called / excluded 四个数）：$scan_stats"
fi
echo "扫描器跑了 $scan_files 个文件 / $scan_decls 个导出方法声明 / $scan_called 个调用点名字"
echo "  按标准库接口方法排除 $scan_excluded 个（名单见脚本里的 STDLIB_INTERFACE_METHODS）"

sort -u -o "$scan_keys" "$scan_keys"
scan_count=$(wc -l <"$scan_keys")

# ── 白名单 ────────────────────────────────────────────────────────────────
#
# 格式与两档类型见 scripts/deadmethods-allowlist.txt 的开头。
# 白名单自身坏掉时不能继续比：那样「比对」比的是半份清单，
# 而它的输出与「全部命中」长得一样。

[ -f "$WHITELIST" ] || not_run "白名单 $WHITELIST 不存在。"

wl_keys="$tmpdir/wl-keys"
: >"$wl_keys"
wl_count=0
wl_bad=0
lineno=0

# trim <字符串>：去掉首尾空白
trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line=$(trim "${raw%$'\r'}")
  [ -n "$line" ] || continue
  case "$line" in \#*) continue ;; esac

  IFS='|' read -r f_path f_sym f_kind f_reason f_extra <<<"$line"
  f_path=$(trim "$f_path")
  f_sym=$(trim "$f_sym")
  f_kind=$(trim "$f_kind")
  f_reason=$(trim "$f_reason")

  if [ -z "$f_path" ] || [ -z "$f_sym" ] || [ -z "$f_kind" ] || [ -z "$f_reason" ] ||
    [ -n "$(trim "$f_extra")" ]; then
    echo "✗ 白名单第 $lineno 行格式不对：应为「<路径> | <符号> | <类型> | <理由>」四栏，理由里不能有 |" >&2
    wl_bad=1
    continue
  fi
  case "$f_path" in
    /* | *..*)
      echo "✗ 白名单第 $lineno 行的路径要是仓库内的相对路径（如 internal/money/money.go）：$f_path" >&2
      wl_bad=1
      continue
      ;;
  esac
  case "$f_path" in
    *.go) ;;
    *)
      echo "✗ 白名单第 $lineno 行的路径不像 Go 源文件：$f_path" >&2
      wl_bad=1
      continue
      ;;
  esac
  case "$f_kind" in
    lib | gap) ;;
    *)
      echo "✗ 白名单第 $lineno 行的类型只能是 lib（库构件 / 测试夹具）或 gap（被追踪的缺口）：$f_kind" >&2
      wl_bad=1
      continue
      ;;
  esac
  # gap 必须挂着一个 issue：白名单不能变成垃圾桶，review 时要能问「这一条为什么还在」。
  if [ "$f_kind" = gap ] && ! grep -qE '#[0-9]+' <<<"$f_reason"; then
    echo "✗ 白名单第 $lineno 行是 gap，但理由里没有 issue 号（#123）：$f_reason" >&2
    wl_bad=1
    continue
  fi
  key="$f_path|$f_sym"
  if grep -qxF "$key" "$wl_keys"; then
    echo "✗ 白名单第 $lineno 行与前文重复：$key" >&2
    wl_bad=1
    continue
  fi
  printf '%s\n' "$key" >>"$wl_keys"
  wl_count=$((wl_count + 1))
done <"$WHITELIST"

if [ "$wl_bad" -ne 0 ]; then
  echo >&2
  echo "白名单 $WHITELIST 自身不合格式，先修它。" >&2
  exit 1
fi

echo "白名单 $WHITELIST —— $wl_count 条 / 上限 $MAX_ENTRIES"

if [ "$wl_count" -gt "$MAX_ENTRIES" ]; then
  echo
  echo "✗ 白名单 $wl_count 条，超过上限 $MAX_ENTRIES。"
  echo "  条目数只许减不许增：新增一条就要同时把 MAX_ENTRIES 往上调，"
  echo "  而那个改动会被 review 看到。要么接线，要么说清为什么必须加。"
  exit 1
fi

sort -u -o "$wl_keys" "$wl_keys"

# ── 比对 ──────────────────────────────────────────────────────────────────

echo "没有调用点的导出方法 $scan_count 个"

new=$(comm -23 "$scan_keys" "$wl_keys")
stale=$(comm -13 "$scan_keys" "$wl_keys")
bad=0

if [ -n "$new" ]; then
  bad=1
  echo
  echo "✗ 新出现的「导出方法没有调用点」 —— 要么接线，要么加进白名单："
  while IFS= read -r k; do
    echo "   $k"
  done <<<"$new"
  echo "  （加进白名单就要同时把 MAX_ENTRIES 往上调，那是一个会被 review 看到的动作）"
fi

# 陈旧条目只提示，不改退出码——见文件开头「白名单」那一节。
# 不吭声也不行：留着它的后果是这个方法将来重新变成没人调时被静默放行。
if [ -n "$stale" ]; then
  echo
  echo "· 白名单里这几条已经不在扫描结果里了，可以删掉了："
  while IFS= read -r k; do
    echo "   $k"
  done <<<"$stale"
fi

if [ "$bad" -ne 0 ]; then
  echo
  echo "白名单与现状不一致。"
  exit 1
fi

# 通过时也要给出正向证据：扫了多少、比了多少条。
# 「什么都没输出」与「什么都没检查」长得一样，而这道门的成功信号恰恰是沉默。
echo "全部命中白名单。"
echo "门禁通过。"
