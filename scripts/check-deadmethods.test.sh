#!/usr/bin/env bash
# check-deadmethods.sh 的自测。
#
# 为什么需要它：这道门由两半组成，而**两半坏掉时都不会响**——扫描器一个符号都
# 扫不出来时，屏幕上就是「没有调用点的导出方法 0 个，全部命中白名单」，与真的干净
# 一模一样。这与 LESSONS.md 第 16 条记的是同一个形状，第 2 条更具体（红线扫描器
# 曾经自己报「通过」，实际根本没在检查）。第 23 条说清了自测本身要做到什么：
# **每一个守卫都要有一条分辨得出来的用例**，退出码相同不等于走的是同一条分支。
#
# **本自测不碰真仓库的代码**：造一个假的仓库根（只有 scripts/ 与 cmd/）、一棵夹具
# Go 源码树、一个假的 go（只认 go list 的几种调用形状），用 GO= 注入。
# 扫描器用**真的那一份**（从脚本里嵌的源码现构一次，用 HELPER= 注入）——它的判据
# 正是这里要钉的东西，桩掉它等于什么都没测。这与 check-deadcode.test.sh 桩掉
# deadcode 是**相反**的取舍，理由是那边的判据是「deadcode 的输出长什么样时脚本
# 怎么判」，而这边要钉的判据本身就在扫描器里。
#
# 夹具里每一个符号都对应下面一条断言，且都写明了它钉的是哪条边界。
#
# 改动 check-deadmethods.sh、白名单格式或那个上限常量之后跑：
#   bash scripts/check-deadmethods.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-deadmethods.sh"
REAL_WHITELIST="$PWD/scripts/deadmethods-allowlist.txt"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

last_out="$tmp/last.txt"
ok() { pass=$((pass + 1)); }
bad() {
  echo "✗ $1"
  sed 's/^/    | /' "$last_out" | tail -8
  fail=$((fail + 1))
}

# 脚本里钉住的默认上限：自测不写死它，否则改一处要动两处。
#
# 读的是 MAX_ENTRIES_DEFAULT 而不是 MAX_ENTRIES：后者可以被环境变量顶开（见下），
# 而这里要的是脚本自己声明的那个数。
declared=$(grep -m1 '^MAX_ENTRIES_DEFAULT=' "$SCRIPT" | cut -d= -f2)
if [ -z "$declared" ]; then
  echo "✗ 读不出 $SCRIPT 里的 MAX_ENTRIES_DEFAULT"
  exit 1
fi

# 比较类用例用的上限。它们比的是「扫描结果与白名单对不对得上」，与真实存量有几条
# 无关，而夹具只有十条。真实上限降到比夹具还小时，这些用例会先挂在「超过上限」上
# ——那是夹具与真实存量撞车，不是被测逻辑坏了。所以它们把上限顶开，上限本身由
# 后面按默认值跑的那几条钉住。
CAP_WIDE=100

# ── 扫描器 ────────────────────────────────────────────────────────────────
#
# 从脚本里嵌的源码现构一份，全程用这一份。它同时也是「嵌入的 Go 源码是一份能
# 编译、且 gofmt 干净的代码」这条断言的证据。

embedded="$tmp/embedded.go"
awk '
  /<<.DEADMETHODS_SCAN_GO.$/ { inside = 1; next }
  inside && $0 == "DEADMETHODS_SCAN_GO" { inside = 0; next }
  inside { print }
' "$SCRIPT" >"$embedded"

if [ -s "$embedded" ]; then ok; else bad "嵌入的 Go 源码 —— 从 $SCRIPT 里一行都没抽出来"; fi

# 嵌在 heredoc 里的代码不会被 gofmt / lint 看到，所以在这里补一道：
# 一份没格式化的 Go 源码在这个仓库里是不该出现的。
if [ -z "$(gofmt -l "$embedded" 2>&1)" ]; then
  ok
else
  bad "嵌入的 Go 源码 —— gofmt 报它没格式化：$(gofmt -l "$embedded" 2>&1)"
fi

SCANNER="$tmp/deadmethods-scan"
if build_out=$(go build -o "$SCANNER" "$embedded" 2>&1); then
  ok
else
  echo "✗ 嵌入的 Go 源码 —— 编译不过："
  printf '%s\n' "$build_out" | sed 's/^/    | /'
  exit 1
fi

# ── 桩件 ──────────────────────────────────────────────────────────────────

# make_go <路径> <模块路径文件> <deps 文件> [list -m 的退出码] [list -deps 的退出码]
#
# 桩件按调用形状分发，并把收到的参数记在 <路径>.calls 里，供自测断言
# 「自动发现的入口真的传给了 go list」，而不只是被打印出来。
#
# **两次调用的退出码分开给**，不是图省事：合成一个的话，两个失败守卫里只有
# 先跑的那个会被测到——另一个「被测到了」只是因为前一个先报了错，把它改坏
# 自测照样通过（LESSONS.md 第 23 条）。第 20 与 20b 条分别钉住它们。
make_go() {
  local path="$1" modf="$2" depsf="$3" rc="${4:-0}" deps_rc="${5:-${4:-0}}"
  {
    printf '%s\n' '#!/usr/bin/env bash'
    printf '%s\n' '# 桩件：check-deadmethods.sh 的自测用，不是真的 go'
    printf '%s\n' 'printf "%s\n" "$*" >> "$0.calls"'
    printf '%s\n' 'case "$*" in'
    printf "  'list -m') cat '%s'; exit %s ;;\n" "$modf" "$rc"
    printf "  'list -deps'*) cat '%s'; exit %s ;;\n" "$depsf" "$deps_rc"
    printf '%s\n' '  *) echo "桩件不认识这个调用：$*" >&2; exit 3 ;;'
    printf '%s\n' 'esac'
  } >"$path"
  chmod +x "$path"
}

# make_helper <路径> <输出文件> [退出码] [stderr 内容文件]
# 输出文件照抄真扫描器的形状：一行 `# files=…` 统计行，随后每行
# `<相对路径>|<类型>.<方法名>`。
make_helper() {
  local path="$1" out="$2" rc="${3:-0}" err="${4:-}"
  {
    printf '%s\n' '#!/usr/bin/env bash'
    printf '%s\n' '# 桩件：check-deadmethods.sh 的自测用，不是真的扫描器'
    printf "cat '%s'\n" "$out"
    if [ -n "$err" ]; then printf "cat '%s' >&2\n" "$err"; fi
    printf 'exit %s\n' "$rc"
  } >"$path"
  chmod +x "$path"
}

# ── 夹具 ──────────────────────────────────────────────────────────────────
#
# 夹具的包名与路径都用 fixture/ 下的假名：自测不该依赖真仓库当前有哪些包，
# 那些包随时会增删，而增删之后这条用例会以「测试失败」的样子报出来，
# 让人以为门禁坏了。

# make_fixture <仓库根>
make_fixture() {
  local repo="$1"
  mkdir -p "$repo/fixture"/a "$repo/fixture"/b "$repo/fixture"/c "$repo/fixture"/d \
    "$repo/fixture"/e "$repo/fixture"/f "$repo/fixture"/g "$repo/fixture"/h

  # a：有人调的导出方法不报，没人调的报，未导出的根本不在范围内。
  cat >"$repo/fixture/a/a.go" <<'EOF'
package a

// Thing 是一个普通类型。
type Thing struct{}

// Called 有人调。
func (t *Thing) Called() {}

// Uncalled 没人调 —— 必须被报出来。
func (t *Thing) Uncalled() {}

// hidden 未导出，不在这道门的范围内。
func (t *Thing) hidden() {}

// Use 是 Called 唯一的调用点。
func Use(t *Thing) { t.Called() }
EOF

  # b：受控实验的等价物 —— 类型进了接口值，RTA 会掩蔽它的导出方法。
  cat >"$repo/fixture/b/b.go" <<'EOF'
package b

// Sink 是一个接口。
type Sink interface{ Use() }

// Impl 会被塞进接口值。
type Impl struct{}

// Use 没人调。
func (i *Impl) Use() {}

// Also 也没人调。
func (i *Impl) Also() {}

// 这一行让 *Impl 成为 runtime type：deadcode 底下的 RTA 因此把 Impl 的
// 全部导出方法标为可达（理由是反射），而这道门照样要报出来。
var _ Sink = (*Impl)(nil)
EOF

  # c：注释与字符串字面量里的调用不算调用点。
  cat >"$repo/fixture/c/c.go" <<'EOF'
package c

// Cache 的用法示例：
//
//	c := &Cache{}
//	c.Invalidate()
type Cache struct{}

// Invalidate 只在上面那段注释里被调用过 —— 注释不是调用点。
func (c *Cache) Invalidate() {}

// Msg 的字符串字面量里有一句调用 —— 字符串不是调用点。
func (c *Cache) Msg() string { return "见 Cache.Invalidate() 的说明" }
EOF

  # d：只被当作方法值传递（不带括号）的方法会被误报。钉住这个取舍。
  cat >"$repo/fixture/d/d.go" <<'EOF'
package d

// Task 有一个只被当作方法值传递的方法。
type Task struct{}

// Run 从没写过 `.Run(`，只被当作值传出去。
func (t *Task) Run() {}

// Holder 收一个函数值。
type Holder struct{ fn func() }

// New 把 t.Run 传出去（不带括号）。
func New(t *Task) Holder { return Holder{fn: t.Run} }
EOF

  # e：同名方法互相掩护 —— 这道门主要的假阴性来源。钉住它。
  cat >"$repo/fixture/e/e.go" <<'EOF'
package e

// A 与 B 各有一个 Do。
type A struct{}

// B 是另一个类型。
type B struct{}

// Do 在 A 上没人调。
func (a *A) Do() {}

// Do 在 B 上有人调 —— 判据按方法名做，于是 A.Do 被掩护掉了。
func (b *B) Do() {}

// Use 是 B.Do 唯一的调用点。
func Use(b *B) { b.Do() }
EOF

  # f：标准库经接口回调的方法名按名单排除，名单之外的照报。
  cat >"$repo/fixture/f/f.go" <<'EOF'
package f

// E 实现了标准库的两个接口。
type E struct{}

// Unwrap 由 errors.Is / errors.As 经接口回调，文本扫描看不见那些调用点。
func (e *E) Unwrap() error { return nil }

// UnmarshalJSON 由 encoding/json 经接口回调，同上。
func (e *E) UnmarshalJSON(b []byte) error { return nil }

// Blah 不在那份名单里，必须被报出来。
func (e *E) Blah() {}
EOF

  # g：泛型接收者，类型名要从 `*Set[T]` 里剥出来。
  cat >"$repo/fixture/g/g.go" <<'EOF'
package g

// Set 是泛型类型，接收者带类型实参。
type Set[T any] struct{}

// Add 没人调。
func (s *Set[T]) Add(v T) {}

// Len 没人调。
func (s *Set[T]) Len() int { return 0 }
EOF

  # h：只在测试文件里被调的方法，照样算「没有调用点」。
  cat >"$repo/fixture/h/h.go" <<'EOF'
package h

// T 有一个只在测试里被调的方法。
type T struct{}

// OnlyInTest 只在 h_test.go 里被调 —— 测试文件不在扫描范围里。
func (t *T) OnlyInTest() {}
EOF
  cat >"$repo/fixture/h/h_test.go" <<'EOF'
package h

import "testing"

func TestT(t *testing.T) { (&T{}).OnlyInTest() }
EOF
}

# make_deps <输出文件> <仓库根>
#
# 照 go list -deps -f '{{.ImportPath}}|{{.Dir}}|{{join .GoFiles " "}}' 的形状写。
# 两处刻意的地方：
#   - 混进标准库与第三方的行，模块前缀过滤必须把它们滤掉；
#   - fixture/h 的文件清单里**没有** h_test.go，这正是 go list 的行为
#     （.GoFiles 不含测试文件），也是「只有测试在调」算没有调用点的落点。
make_deps() {
  local out="$1" repo="$2" p
  : >"$out"
  for p in alpha beta; do
    printf 'example.com/fake/cmd/%s|%s/cmd/%s|main.go\n' "$p" "$repo" "$p" >>"$out"
  done
  for p in a b c d e f g h; do
    printf 'example.com/fake/fixture/%s|%s/fixture/%s|%s.go\n' "$p" "$repo" "$p" "$p" >>"$out"
  done
  {
    printf 'fmt|/usr/local/go/src/fmt|doc.go\n'
    printf 'os|/usr/local/go/src/os|doc.go\n'
    printf 'github.com/other/dep|/go/pkg/mod/github.com/other/dep@v1.0.0|dep.go\n'
  } >>"$out"
}

# 期望的报告。逐条对应上面夹具里的注释。
report_ok="$tmp/report-ok.txt"
cat >"$report_ok" <<'EOF'
# files=10 decls=15 called=2 excluded=2
fixture/a/a.go|Thing.Uncalled
fixture/b/b.go|Impl.Also
fixture/b/b.go|Impl.Use
fixture/c/c.go|Cache.Invalidate
fixture/c/c.go|Cache.Msg
fixture/d/d.go|Task.Run
fixture/f/f.go|E.Blah
fixture/g/g.go|Set.Add
fixture/g/g.go|Set.Len
fixture/h/h.go|T.OnlyInTest
EOF

wl_ok="$tmp/wl-ok.txt"
cat >"$wl_ok" <<'EOF'
# 用例用的小白名单：与夹具的报告一一对应
fixture/a/a.go | Thing.Uncalled        | gap | #1 夹具
fixture/b/b.go | Impl.Also             | gap | #1 夹具
fixture/b/b.go | Impl.Use              | gap | #1 夹具
fixture/c/c.go | Cache.Invalidate      | gap | #1 夹具
fixture/c/c.go | Cache.Msg             | gap | #1 夹具
fixture/d/d.go | Task.Run              | gap | #1 夹具
fixture/f/f.go | E.Blah                | gap | #1 夹具
fixture/g/g.go | Set.Add               | gap | #1 夹具
fixture/g/g.go | Set.Len               | gap | #1 夹具
fixture/h/h.go | T.OnlyInTest          | gap | #1 夹具
EOF

mod_ok="$tmp/mod-ok.txt"
printf '%s\n' 'example.com/fake' >"$mod_ok"

# make_fake_repo <目标目录> [main 包名…]
# 造一个假的仓库根：只有 scripts/check-deadmethods.sh、cmd/<包名>/main.go
# 与夹具源码树。扫描起点是自动发现的，所以只能这样验——在真仓库里塞一个 cmd/
# 会污染工作树。
make_fake_repo() {
  local repo="$1"
  shift
  mkdir -p "$repo/scripts" "$repo/cmd"
  cp "$SCRIPT" "$repo/scripts/check-deadmethods.sh"
  local name
  for name in "$@"; do
    mkdir -p "$repo/cmd/$name"
    printf 'package main\n\nfunc main() {}\n' >"$repo/cmd/$name/main.go"
  done
  make_fixture "$repo"
}

repo="$tmp/repo"
make_fake_repo "$repo" alpha beta
deps_ok="$tmp/deps-ok.txt"
make_deps "$deps_ok" "$repo"
stub_ok="$tmp/go-ok"
make_go "$stub_ok" "$mod_ok" "$deps_ok"

# run <白名单> [helper]
run() {
  GO="$stub_ok" WHITELIST="$1" MAX_ENTRIES="$CAP_WIDE" HELPER="${2:-$SCANNER}" \
    bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
}

# run_at_cap <白名单> [helper]
# 与 run 相同，但上限取脚本声明的默认值——给「上限本身」那几条用例用。
run_at_cap() {
  GO="$stub_ok" WHITELIST="$1" HELPER="${2:-$SCANNER}" \
    bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
}

# ── 用例 ──────────────────────────────────────────────────────────────────

# 0. 扫描器判据的观察窗口。
#
# 全部命中时脚本只报数、不逐条列（逐条列的是「新出现的」那一块），所以下面
# 第 2~14 条要看的清单得先弄出来：用一份**空白名单**跑一次，于是每一项都是
# 「新出现的」，脚本会把它们连同路径与符号逐条打印。
empty_wl="$tmp/wl-empty.txt"
: >"$empty_wl"
run "$empty_wl"
scan_report="$tmp/scan-report.txt"
sed -n '/^✗ 新出现的/,/^  （加进白名单/p' "$last_out" >"$scan_report"
if [ -s "$scan_report" ]; then ok; else bad "空白名单 —— 脚本应把全部发现逐条打印出来"; fi

# 1. 全部命中：通过，而且**通过时要有正向证据**——扫了几个文件、比了多少条。
#    「什么都没输出」与「什么都没检查」长得一样，而这道门的成功信号恰恰是沉默。
run "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "全部命中 —— 应通过，实际退出码 $rc"; fi
grep -q '扫描器跑了 10 个文件 / 15 个导出方法声明 / 2 个调用点名字' "$last_out" ||
  bad "全部命中 —— 通过时要报出扫了多少（10 个文件 / 15 个声明 / 2 个调用点名字）"
grep -q '没有调用点的导出方法 10 个' "$last_out" ||
  bad "全部命中 —— 通过时要报出比了多少条（10 个）"
grep -q '全部命中白名单' "$last_out" || bad "全部命中 —— 通过时要说清「全部命中」"

# 2. **核心判据**：一个没人调用的导出方法必须被报出来。
#    这是这道门存在的理由——扫描器坏掉时，它的输出与「干净」长得一模一样。
grep -qxF '   fixture/a/a.go|Thing.Uncalled' "$scan_report" ||
  bad "没人调的导出方法 —— 必须被报出来（fixture/a/a.go|Thing.Uncalled）"

# 3. 有人调的导出方法不能报。没有这一条，一个「什么都报」的扫描器也能过第 2 条。
if grep -q 'Thing.Called' "$scan_report"; then
  bad "有人调的导出方法 —— 不该被报出来（Thing.Called）"
else
  ok
fi

# 4. 未导出的方法不在范围内：RTA 的盲区只覆盖**导出**方法，未导出的 deadcode 看得见。
if grep -q 'Thing.hidden' "$scan_report"; then
  bad "未导出的方法 —— 不在这道门的范围内（Thing.hidden）"
else
  ok
fi

# 5. **受控实验的等价物**：类型进了接口值（`var _ Sink = (*Impl)(nil)`）时，
#    RTA 会把它的全部导出方法标为可达、deadcode 一条都不报，而这道门照样要报。
#    这条用例就是这道门存在的判据本身。
grep -qxF '   fixture/b/b.go|Impl.Use' "$scan_report" ||
  bad "接口值里的类型 —— 它的导出方法必须被报出来（Impl.Use）"
grep -qxF '   fixture/b/b.go|Impl.Also' "$scan_report" ||
  bad "接口值里的类型 —— 它的导出方法必须被报出来（Impl.Also）"

# 6. 注释里的调用不算调用点。本仓的文档注释里有成段的调用示例，
#    按文本匹配会把那些方法全部算成「有人调」。
grep -qxF '   fixture/c/c.go|Cache.Invalidate' "$scan_report" ||
  bad "注释里的调用 —— 不算调用点（Cache.Invalidate）"

# 7. 字符串字面量里的调用不算调用点。
grep -qxF '   fixture/c/c.go|Cache.Msg' "$scan_report" ||
  bad "字符串里的调用 —— 不算调用点（Cache.Msg）"

# 8. 方法值（不带括号）**会**被误报。这是判据取严一侧的代价，钉住它，
#    免得将来有人以为「报出来的都真的没人调」。
grep -qxF '   fixture/d/d.go|Task.Run' "$scan_report" ||
  bad "方法值 —— 只被当作值传递的方法会被误报，这条钉的是那个取舍（Task.Run）"

# 9. 同名方法互相掩护：A.Do 与 B.Do 同名，B.Do 有人调，于是 A.Do 也不报。
#    这是这道门主要的假阴性来源，钉住它——不钉的话，将来判据变了也没人知道。
if grep -q 'A\.Do' "$scan_report"; then
  bad "同名方法 —— B.Do 有人调时 A.Do 会被掩护掉，不该报出来"
else
  ok
fi

# 10. 标准库经接口回调的方法名按名单排除（调用方在标准库里，文本扫描看不见）。
if grep -q 'E\.Unwrap\|E\.UnmarshalJSON' "$scan_report"; then
  bad "标准库接口方法 —— 应在名单里被排除（E.Unwrap / E.UnmarshalJSON）"
else
  ok
fi

# 11. 名单之外的照报：排除不能变成一片盲区。
grep -qxF '   fixture/f/f.go|E.Blah' "$scan_report" ||
  bad "标准库接口方法 —— 名单之外的方法仍要报出来（E.Blah）"

# 12. 泛型接收者：类型名要从 `*Set[T]` 里剥出来，剥不干净会得到 `Set[T].Add`。
grep -qxF '   fixture/g/g.go|Set.Add' "$scan_report" ||
  bad "泛型接收者 —— 类型名要剥成 Set（Set.Add）"
grep -qxF '   fixture/g/g.go|Set.Len' "$scan_report" ||
  bad "泛型接收者 —— 类型名要剥成 Set（Set.Len）"

# 13. 只在测试文件里被调的方法算「没有调用点」。
#     测试自己就能构造调用方，所以「只有测试在调」与「生产在调」在覆盖率上
#     长得一模一样（LESSONS.md 第 22 条）。
grep -qxF '   fixture/h/h.go|T.OnlyInTest' "$scan_report" ||
  bad "只有测试在调 —— 算「没有调用点」（T.OnlyInTest）"

# 14. 统计行里的 excluded 要真的算上，不能恒为 0。
#     它是「名单排除了几个」的正向证据：恒 0 说明排除那一段没生效。
grep -q '按标准库接口方法排除 2 个' "$last_out" ||
  bad "统计行 —— 按名单排除的条数要报出来（夹具里是 2 个）"

# 15. 出现一个新的无调用点导出方法：退出码 1，并把它打印出来。
#     这是这道门存在的理由：新写的方法不能悄悄过去。
report_new="$tmp/report-new.txt"
cp "$report_ok" "$report_new"
printf '%s\n' 'fixture/a/a.go|Thing.BrandNew' >>"$report_new"
stub_new="$tmp/helper-new"
make_helper "$stub_new" "$report_new"
run "$wl_ok" "$stub_new"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "新出现的方法 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   fixture/a/a.go|Thing.BrandNew$' "$last_out" ||
  bad "新出现的方法 —— 要逐条打印出来，让人知道该加白名单还是该接线"
grep -q '要么接线' "$last_out" || bad "新出现的方法 —— 提示里要给出两条路"

# 16. 白名单里的方法已经有人调了：**打印出来，但退出码仍然是 0**。
#     它出现的原因恰恰是「有人把没门的东西接上线了」——那是个好动作，
#     让 CI 为它变红是一条会教人学会忽略 CI 的假警报。所以这一条与第 15 条
#     是**相反**的判据：一边钉「新方法必须拦住」，一边钉「陈旧条目不许拦」。
#     留着不吭声也不行：那会让这个方法将来重新变成没人调时被静默放行。
report_stale="$tmp/report-stale.txt"
grep -vxF 'fixture/g/g.go|Set.Len' "$report_ok" >"$report_stale"
stub_stale="$tmp/helper-stale"
make_helper "$stub_stale" "$report_stale"
run "$wl_ok" "$stub_stale"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "陈旧条目 —— 不该拦住合并（应退出码 0），实际 $rc"; fi
grep -q '^   fixture/g/g.go|Set.Len$' "$last_out" || bad "陈旧条目 —— 要报出是哪一条"
grep -q '可以删掉' "$last_out" || bad "陈旧条目 —— 要说清这一条已经可以删了"

# 16b. 新方法与陈旧条目同时出现：仍然是失败，而且两件事都要报出来。
#      陈旧的那一半只提示，不能把真正该拦的那一半盖过去。
report_both="$tmp/report-both.txt"
cp "$report_new" "$report_both"
grep -vxF 'fixture/g/g.go|Set.Len' "$report_both" >"$report_both.tmp"
mv "$report_both.tmp" "$report_both"
stub_both="$tmp/helper-both"
make_helper "$stub_both" "$report_both"
run "$wl_ok" "$stub_both"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "新方法 + 陈旧条目 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   fixture/a/a.go|Thing.BrandNew$' "$last_out" || bad "新方法 + 陈旧条目 —— 新方法要报出来"
grep -q '^   fixture/g/g.go|Set.Len$' "$last_out" || bad "新方法 + 陈旧条目 —— 陈旧条目也要报出来"

# 17. 白名单条目数超过上限：退出码 1。上限从脚本里读，所以这条用例不依赖那个具体数值。
wl_over="$tmp/wl-over.txt"
report_over="$tmp/report-over.txt"
: >"$wl_over"
printf '# files=1 decls=1 called=0 excluded=0\n' >"$report_over"
i=0
while [ "$i" -le "$declared" ]; do
  printf 'fixture/x/x.go | X.M%d | gap | #1 还没有接线\n' "$i" >>"$wl_over"
  printf 'fixture/x/x.go|X.M%d\n' "$i" >>"$report_over"
  i=$((i + 1))
done
stub_over="$tmp/helper-over"
make_helper "$stub_over" "$report_over"
# 这一条测的正是上限本身，所以不能顶开它。
run_at_cap "$wl_over" "$stub_over"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "条目数超过上限（$declared + 1）—— 应失败（退出码 1），实际 $rc"; fi
grep -q '超过上限' "$last_out" || bad "条目数超过上限 —— 要说清是这个原因"
grep -q '只许减不许增' "$last_out" || bad "条目数超过上限 —— 要说明「只许减不许增」这条规矩"

# 18. 上限是个具名常量，自测把它钉住。
#     往上调必须同时改这一处，于是那是一次会被 review 看到的 diff。
#     关掉一批缺口之后，把脚本里的常量与这里的字面量一起往下调。
if [ "$declared" = "25" ]; then ok; else bad "MAX_ENTRIES_DEFAULT 被改成了 $declared（自测钉住的是 25）"; fi

# 19. go 不在 PATH：退出码 2，并说清是没跑成。
#     用一个确定不存在的名字，避免依赖运行环境里有没有装 go。
GO="go-not-installed-$$" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "go 不在 PATH —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '找不到' "$last_out" || bad "go 不在 PATH —— 要说清找不到的是哪个可执行文件"

# 20. go list -deps 自身退出码非 0：退出码 2，不能当成「干净」。
#
#     桩件这里**只让 -deps 失败**（`0 1`），-m 照常成功：两个守卫合成一个退出码
#     的话，先跑的 -m 会先把脚本拦下，这一条就变成在测 -m 那条守卫了——退出码
#     一样、报的错也一样含「没跑成」，看起来通过，实际没走到这里（LESSONS.md
#     第 23 条）。所以下面断言的是**报出的原因**，不是「有个错」。
stub_rc1="$tmp/go-rc1"
make_go "$stub_rc1" "$mod_ok" "$deps_ok" 0 1
GO="$stub_rc1" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "go list -deps 退出码非 0 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'go list -deps 失败' "$last_out" || bad "go list -deps 退出码非 0 —— 要说清失败的是 -deps 这一次调用"

# 20b. go list -m 自身退出码非 0：退出码 2。
#      与 20 对称：这里只让 -m 失败（`1 0`），否则这一条测的会是 -deps 那条守卫。
stub_modrc="$tmp/go-modrc"
make_go "$stub_modrc" "$mod_ok" "$deps_ok" 1 0
GO="$stub_modrc" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "go list -m 退出码非 0 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'go list -m 失败' "$last_out" || bad "go list -m 退出码非 0 —— 要说清失败的是 -m 这一次调用"

# 21. go list -deps 一行都没输出：退出码 2。
#     可达集合为空时扫描范围是零个包，那看起来像「全部命中白名单」——
#     最危险的一种假通过。
deps_empty="$tmp/deps-empty.txt"
: >"$deps_empty"
stub_empty="$tmp/go-empty"
make_go "$stub_empty" "$mod_ok" "$deps_empty"
GO="$stub_empty" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "deps 输出为空 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一行都没输出' "$last_out" || bad "deps 输出为空 —— 要说清是这个原因"

# 22. go list -deps 的输出里有不是三栏的行：退出码 2。
#     那正是「工具换了措辞或换了参数」的样子（比如有人给它加上 -json）。
#     丢掉之后扫描范围会少一批，而结果看起来像「差异变小了」。
deps_bad="$tmp/deps-bad.txt"
cp "$deps_ok" "$deps_bad"
printf '%s\n' 'example.com/fake/fixture/a|/tmp/x|a.go|extra' >>"$deps_bad"
stub_depsbad="$tmp/go-depsbad"
make_go "$stub_depsbad" "$mod_ok" "$deps_bad"
GO="$stub_depsbad" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "deps 行不是三栏 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '不是三栏' "$last_out" || bad "deps 行不是三栏 —— 要说清是这个原因"

# 23. 模块前缀一个包都对不上：退出码 2。
#     此时扫描范围为空，而那个结果看起来像结论。
mod_wrong="$tmp/mod-wrong.txt"
printf '%s\n' 'example.com/other' >"$mod_wrong"
stub_modwrong="$tmp/go-modwrong"
make_go "$stub_modwrong" "$mod_wrong" "$deps_ok"
GO="$stub_modwrong" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "模块前缀对不上 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个本仓库的包都没有' "$last_out" || bad "模块前缀对不上 —— 要说清是这个原因"

# 24. go list -m 的输出认不出（多行）：退出码 2。
#     模块路径读错时白名单里的相对路径一条都对不上，整个仓库会被报成
#     「新出现的」——一次假的大发现，而人可能就去改白名单把它「修好」。
mod_multi="$tmp/mod-multi.txt"
printf '%s\n' 'example.com/fake' 'example.com/第二个' >"$mod_multi"
stub_modmulti="$tmp/go-modmulti"
make_go "$stub_modmulti" "$mod_multi" "$deps_ok"
GO="$stub_modmulti" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "模块路径认不出 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'go list -m 的输出认不出' "$last_out" || bad "模块路径认不出 —— 要说清是这一处"

# 25. go list -deps 往 stderr 写了东西但退出码是 0：**仍然通过**。
#     这一条钉的是一个刻意的选择：需要下载模块时 go 会写 `go: downloading …`，
#     按 stderr 判失败会把「第一次在干净环境里跑」变成一次假警报（LESSONS.md 第 13 条）。
#     脚本对这一处只打印、不改退出码；而扫描器的 stderr 是另一回事（见第 26 条）。
deps_err="$tmp/deps-err.txt"
printf '%s\n' 'go: downloading github.com/other/dep v1.0.0' >"$deps_err"
stub_download="$tmp/go-download"
{
  printf '%s\n' '#!/usr/bin/env bash'
  printf '%s\n' 'printf "%s\n" "$*" >> "$0.calls"'
  printf '%s\n' 'case "$*" in'
  printf "  'list -m') cat '%s' ;;\n" "$mod_ok"
  printf "  'list -deps'*) cat '%s'; cat '%s' >&2 ;;\n" "$deps_ok" "$deps_err"
  printf '%s\n' '  *) echo "桩件不认识这个调用：$*" >&2; exit 3 ;;'
  printf '%s\n' 'esac'
  printf '%s\n' 'exit 0'
} >"$stub_download"
chmod +x "$stub_download"
GO="$stub_download" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "deps 的 stderr 有下载进度 —— 不该判失败（应退出码 0），实际 $rc"; fi
grep -q '往 stderr 写了内容' "$last_out" || bad "deps 的 stderr 有下载进度 —— 仍要打出来让人看见"

# 26. 扫描器往 stderr 写了东西：退出码 2。
#     与第 25 条是**相反**的判据，而且必须相反：成功的扫描器不往 stderr 写东西，
#     写了说明这一轮有它自己才知道的状况，而「有噪音但结论看着正常」最容易被放过。
helper_err="$tmp/helper-err.txt"
printf '%s\n' 'deadmethods-scan: 有什么不对劲' >"$helper_err"
stub_helper_err="$tmp/helper-stderr"
make_helper "$stub_helper_err" "$report_ok" 0 "$helper_err"
run "$wl_ok" "$stub_helper_err"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫描器写了 stderr —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '往 stderr 写了东西' "$last_out" || bad "扫描器写了 stderr —— 要说清是这个原因"

# 27. 扫描器自身退出码非 0：退出码 2。
stub_helper_rc1="$tmp/helper-rc1"
make_helper "$stub_helper_rc1" "$report_ok" 1
run "$wl_ok" "$stub_helper_rc1"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫描器退出码非 0 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '扫描器执行失败' "$last_out" || bad "扫描器退出码非 0 —— 要说清是这个原因"

# 28. 扫描器输出里有认不出的行：退出码 2。
#     认不出的行不能丢——那正是「工具换了输出格式」的样子，丢掉它之后就变成
#     「一个符号都没扫到」，而那个结果与「全部命中白名单」在屏幕上没有区别。
report_unknown="$tmp/report-unknown.txt"
cp "$report_ok" "$report_unknown"
printf '%s\n' 'fixture/a/a.go: 这一行不是扫描器该有的形状' >>"$report_unknown"
stub_unknown="$tmp/helper-unknown"
make_helper "$stub_unknown" "$report_unknown"
run "$wl_ok" "$stub_unknown"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "认不出的输出 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '认不出' "$last_out" || bad "认不出的输出 —— 要报出是「认不出」，而不是当成符号收下"

# 29. 扫描器没给统计行：退出码 2。
#     统计行是「扫了几个文件」这条正向证据。缺了它，「没扫到」与「干净」同形。
report_nostats="$tmp/report-nostats.txt"
grep -v '^# ' "$report_ok" >"$report_nostats"
stub_nostats="$tmp/helper-nostats"
make_helper "$stub_nostats" "$report_nostats"
run "$wl_ok" "$stub_nostats"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "缺统计行 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '没给出统计行' "$last_out" || bad "缺统计行 —— 要说清是这个原因"

# 30. 统计行说 files=0：退出码 2。
#     这一条与第 29 条走的是**两条不同的守卫**，所以必须分开测——只测一条的话，
#     另一条关掉了也没人知道。
report_zerofiles="$tmp/report-zerofiles.txt"
cp "$report_ok" "$report_zerofiles"
sed -i '1s/files=[0-9]*/files=0/' "$report_zerofiles"
stub_zerofiles="$tmp/helper-zerofiles"
make_helper "$stub_zerofiles" "$report_zerofiles"
run "$wl_ok" "$stub_zerofiles"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "统计行 files=0 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '读不全\|读不出' "$last_out" || bad "统计行 files=0 —— 要说清是这个原因"

# 31. 扫描器不是一个可执行文件（HELPER 指错）：退出码 2。
#     「扫描器没跑起来」与「没有无调用点的方法」在退出码上必须分得开。
run "$wl_ok" "$tmp/根本没有这个扫描器"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫描器不可执行 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '不是一个可执行文件' "$last_out" || bad "扫描器不可执行 —— 要说清是这个原因"

# 32. 不注入 HELPER 时脚本会自己现构一份。这条走的是**真实的构建路径**，
#     第 1~31 条全都绕开了它（都用 HELPER 注入）。
GO="$stub_ok" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" \
  bash "$repo/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "自己现构扫描器 —— 应通过，实际退出码 $rc"; fi
grep -q '全部命中白名单' "$last_out" || bad "自己现构扫描器 —— 构建路径也要能跑通"

# 33. 白名单格式不对（栏数不足）：退出码 1，并指出是第几行。
#     断言的是**格式守卫自己的那句话**，不是随便一句带行号的报错：这一行后面
#     还会被「gap 要挂 issue 号」接住，而那条报错同样带行号——只断言「第 N 行」
#     的话，把格式守卫整个关掉它也照样通过（LESSONS.md 第 23 条）。
wl_badfmt="$tmp/wl-badfmt.txt"
printf '%s\n' 'fixture/a/a.go | Thing.Uncalled | gap' >"$wl_badfmt"
run "$wl_badfmt"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单格式不对 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '四栏' "$last_out" || bad "白名单格式不对 —— 要说出正确的栏数是四栏"

# 33b. 白名单坏掉时必须失败，**哪怕剩下的条目恰好能对上**。
#      这一条才是那个守卫的价值所在，也是上一条用例分辨不出来的：
#      坏行指的是一个**已经在白名单里**的符号（`Set.Len`，它在扫描结果里），
#      于是把它跳过之后 new 与 stale 都是空的——**不比就通过**，
#      而输出与真的「全部命中」一模一样。
#      上一条用例里坏行指的是一个在扫描结果里的符号，跳过它照样会报「新出现的」，
#      所以把守卫整个关掉它也不会挂。
wl_badfmt2="$tmp/wl-badfmt2.txt"
{
  grep -v '^#' "$wl_ok"
  printf '%s\n' 'fixture/g/g.go | Set.Len'
} >"$wl_badfmt2"
run "$wl_badfmt2" "$stub_stale"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单坏掉但剩下的条目恰好对上 —— 必须仍然失败（退出码 1），实际 $rc"; fi
grep -q '第 11 行' "$last_out" || bad "白名单坏掉但剩下的条目恰好对上 —— 要指出是第几行"

# 33c. 白名单多出一栏（理由里混进了 |）：退出码 1。
#      这一条钉的是**格式守卫本身**，而上面两条钉不住它：栏数不足的行会被下游的
#      类型 / issue 检查接住，把格式守卫关掉也照样失败。多出来的那一栏则不同——
#      路径、类型、issue 号全都合法，下游没人看得见它，只有格式守卫会拦。
wl_extra="$tmp/wl-extra.txt"
{
  grep -v '^#' "$wl_ok" | grep -v '^fixture/a/a.go'
  printf '%s\n' 'fixture/a/a.go | Thing.Uncalled | gap | #1 夹具 | 多出来的一栏'
} >"$wl_extra"
run "$wl_extra"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单多出一栏 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '四栏' "$last_out" || bad "白名单多出一栏 —— 要说出正确的栏数是四栏"

# 34. 类型不认识：退出码 1。
wl_badkind="$tmp/wl-badkind.txt"
printf '%s\n' 'fixture/a/a.go | Thing.Uncalled | maybe | 说不清' >"$wl_badkind"
run "$wl_badkind"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "类型不认识 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'lib' "$last_out" || bad "类型不认识 —— 要说出合法取值是哪两个"

# 35. gap 条目没有 issue 号：退出码 1。
#     白名单不能变成垃圾桶：gap 的每一条都要能回答「这一条为什么还在」。
wl_noissue="$tmp/wl-noissue.txt"
printf '%s\n' 'fixture/a/a.go | Thing.Uncalled | gap | 反正暂时留着' >"$wl_noissue"
run "$wl_noissue"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "gap 没有 issue 号 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'issue 号' "$last_out" || bad "gap 没有 issue 号 —— 要说清缺的是什么"

# 36. 白名单里有重复条目：退出码 1。
#     重复的条目会让「这一条为什么还在」有两个互相矛盾的答案，
#     而集合比对本身看不出重复——它去重之后照样相等。
wl_dup="$tmp/wl-dup.txt"
{
  grep -v '^#' "$wl_ok" | grep -v '^fixture/a/a.go'
  printf '%s\n' 'fixture/a/a.go | Thing.Uncalled | gap | #1 夹具'
  printf '%s\n' 'fixture/a/a.go | Thing.Uncalled | lib | 库构件'
} >"$wl_dup"
run "$wl_dup"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单有重复条目 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '重复' "$last_out" || bad "白名单有重复条目 —— 要说清是这个原因"

# 37. 路径写成绝对路径或带 ..：退出码 1。
#     白名单里存的是仓库内的相对路径，混进别的写法时它永远匹配不上，
#     而「匹配不上」表现为「这一条怎么没生效」，不是一条错误。
wl_abs="$tmp/wl-abs.txt"
printf '%s\n' '/fixture/a/a.go | Thing.Uncalled | gap | #1 绝对路径' >"$wl_abs"
run "$wl_abs"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "路径是绝对路径 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '相对路径' "$last_out" || bad "路径是绝对路径 —— 要说清该写成什么样"

# 37b. 路径不是 Go 源文件：退出码 1。与上一条走的是两条不同的守卫。
wl_nogo="$tmp/wl-nogo.txt"
printf '%s\n' 'fixture/a/a.txt | Thing.Uncalled | gap | #1 不是 Go 源文件' >"$wl_nogo"
run "$wl_nogo"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "路径不是 .go —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'Go 源文件' "$last_out" || bad "路径不是 .go —— 要说清该写成什么样"

# 38. 白名单文件不在：退出码 2。
#     「文件不在」与「清单是空的」在集合比对上长得一样（都是零条），
#     而零条会让整份扫描结果报成「新出现的」——所以必须是「没跑成」。
run "$tmp/根本没有这份白名单.txt"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "白名单不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '不存在' "$last_out" || bad "白名单不存在 —— 要说清是这个原因"

# 39. 签入仓库的那份白名单本身要能过。
#     用真白名单生成扫描结果，于是这一条同时验了：文件格式、两档类型、
#     gap 都挂着 issue 号、条目数没超上限、路径能原样往返。
#     符号不写死：白名单将来增删时这条用例不该跟着坏。
real_report="$tmp/real-report.txt"
{
  printf '# files=1 decls=1 called=0 excluded=0\n'
  awk -F'|' '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      p = $1; s = $2
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", p)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
      print p "|" s
    }
  ' "$REAL_WHITELIST"
} >"$real_report"
stub_real="$tmp/helper-real"
make_helper "$stub_real" "$real_report"
run_at_cap "$REAL_WHITELIST" "$stub_real"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "签入的白名单 —— 应通过，实际退出码 $rc"; fi
real_count=$(awk '/^[[:space:]]*#/ { next } /^[[:space:]]*$/ { next } { n++ } END { print n + 0 }' "$REAL_WHITELIST")
grep -q "没有调用点的导出方法 $real_count 个" "$last_out" ||
  bad "签入的白名单 —— 应报出比对了 $real_count 条"

# 40. 扫描起点是**自动发现**的，不是手写清单。
#     手写清单会以它要防的方式坏掉：新加一个 main 包、没人记得补进来，于是那个包
#     整片没有门，而这一轮的输出看起来完全正常。这里验三件事：带 package main 的
#     目录被发现、不带的不被当成起点、发现的包**真的传给了 go list -deps**。
repo_new="$tmp/repo-new"
make_fake_repo "$repo_new" alpha beta gamma
mkdir -p "$repo_new/cmd/notmain"
printf 'package notmain\n' >"$repo_new/cmd/notmain/lib.go"
deps_new="$tmp/deps-new.txt"
make_deps "$deps_new" "$repo_new"
stub_rec="$tmp/go-rec"
make_go "$stub_rec" "$mod_ok" "$deps_new"
GO="$stub_rec" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo_new/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "自动发现扫描起点 —— 应通过，实际退出码 $rc"; fi
grep -q '扫描起点 ./cmd/alpha ./cmd/beta ./cmd/gamma' "$last_out" ||
  bad "自动发现扫描起点 —— 新加的 main 包要出现在扫描起点里（否则它整片没有门）"
if grep -q 'notmain' "$last_out"; then
  bad "自动发现扫描起点 —— 不是 main 包的目录不该进扫描起点"
else
  ok
fi
rec_args=$(grep -m1 '^list -deps' "$stub_rec.calls" 2>/dev/null || echo "（没记到参数）")
case "$rec_args" in
  *./cmd/alpha*./cmd/beta*./cmd/gamma*) ok ;;
  *) bad "自动发现扫描起点 —— 发现的包要真的传给 go list -deps（收到：$rec_args）" ;;
esac

# 41. 一个 main 包都没发现：退出码 2。
#     「没有入口」= 可达集合为空 = 扫描范围为空，而那个结果看起来像结论。
repo_empty="$tmp/repo-empty"
make_fake_repo "$repo_empty"
GO="$stub_ok" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" HELPER="$SCANNER" \
  bash "$repo_empty/scripts/check-deadmethods.sh" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫不到 main 包 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '没找到 main 包' "$last_out" || bad "扫不到 main 包 —— 要说清是这个原因"

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
