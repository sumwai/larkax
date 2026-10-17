#!/usr/bin/env bash
# check-test-names.sh 的自测。
#
# 为什么需要它：这道门的成功信号是**沉默**——没有违规就不输出，于是
# 「判据写反了 / 一个文件都没扫到 / 名字根本没提取出来」与「干净」在屏幕上
# 完全一样，退出码也一样（LESSONS.md 第 16 条，第 2 条是它的第一次现身：
# 红线扫描器自己报「通过」，实际根本没在检查）。第 23 条把结论写死了：
# **每一个守卫都要有一条分辨得出来的用例**，退出码相同不等于走的是同一条分支。
#
# 所以下面正反两面都钉：干净时通过、三种形态的中文名各自失败、
# 几个「看着像违规、其实不是」的形态不失败，以及三条「没跑成」的出口。
#
# 其中一条最要紧：**取的是函数名，不是整行**。一行写完的函数里带中文、
# 形参名带中文，都是合法且常见的写法，判据按整行走会误报一片
#（本仓实测至少 5 处），所以那条单独有一条用例。
#
# **本自测跑的是真脚本**（不是副本），用脚本自己声明的 ROOT 指向夹具树——
# 判据、名字提取与枚举文件那三段都在被测的那一份代码里，桩掉它等于什么都没测。
# 夹具全在 $tmp 下，不碰真仓库的工作树。
#
# 改动 check-test-names.sh 之后跑：
#   bash scripts/check-test-names.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-test-names.sh"
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

# run <夹具根>
# 退出码由调用者立刻取走：$? 必须在下一句就读。
run() { ROOT="$1" bash "$SCRIPT" >"$last_out" 2>&1; }

# ── 夹具 ──────────────────────────────────────────────────────────────────
#
# 干净的那一份里，三种合法形态（普通 / 方法 / 泛型）各一条，都是英文名；
# 再加上每一种「看着像违规、其实不是」的形态：
# 中文出现在注释、字符串、t.Run 的子测试名、中文常量名、中文形参名、
# 函数体（同一行）、以及**非 Go 文件**里。这些都不该让门失败。

mk_clean() {
  local root="$1"
  mkdir -p "$root/pkg"
  cat >"$root/pkg/a_test.go" <<'EOF'
package pkg

import "testing"

// 注释里的中文不算：Test注释里的名字不算。
const 中文常量 = "字符串里的中文也不算"

type T struct{}

func TestCleanOne(t *testing.T) {
	t.Run("中文子测试名", func(t *testing.T) {
		t.Log("中文日志")
	})
}

func (T) MethodName() {}

func GenericName[V any](v V) V { return v }

func plainErr() string { return "随便一个错" }

func withChineseParam(原文 string) {}
EOF
  cat >"$root/pkg/impl.go" <<'EOF'
package pkg

func helper() int {
	局部变量 := 1
	return 局部变量
}
EOF
  # 非 Go 文件里的中文「函数名」不在判据里：这条约定管的是 Go 源码里的标识符。
  cat >"$root/pkg/note.txt" <<'EOF'
func 这个文件不是 Go 源码()
EOF
}

clean="$tmp/clean"
mk_clean "$clean"

# ── 1. 干净夹具：退出码 0 ─────────────────────────────────────────────────

run "$clean"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "干净夹具 —— 应通过（退出码 0），实际 $rc"; fi
grep -q '函数名门禁通过。' "$last_out" || bad "干净夹具 —— 要通过时明说通过"

# ── 2. 正向证据：扫了几个文件、几条声明 ───────────────────────────────────
#
# 「什么都没输出」与「什么都没检查」长得一样，所以这行证据必须真的报出数来，
# 而且数要与夹具对得上（2 个 .go 文件、6 条 func 声明；note.txt 不算）。

grep -qE '^扫描器跑了 [1-9][0-9]* 个 \.go 文件 / [1-9][0-9]* 个 func 声明$' "$last_out" ||
  bad "正向证据 —— 要通过时也要报「扫了多少」（CI 靠这行判它真的跑了）"
grep -q '^扫描器跑了 2 个 .go 文件 / 6 个 func 声明$' "$last_out" ||
  bad "正向证据 —— 条数要与夹具对得上（2 个 .go 文件 / 6 条声明）"

# ── 3. 普通形态的中文函数名：退出码 1，且逐条列出文件、行号、名字 ─────────
#
# 这是这道门存在的理由，也是自测里最不能少的一条。

plain="$tmp/plain"
mk_clean "$plain"
cat >>"$plain/pkg/impl.go" <<'EOF'

func 中文名字() {}
EOF

run "$plain"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "普通形态中文名 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   .*impl\.go:[0-9]* —— 中文名字$' "$last_out" ||
  bad "普通形态中文名 —— 要逐条列出「文件:行号 —— 名字」"
grep -q '函数名门禁不通过。' "$last_out" || bad "普通形态中文名 —— 失败时要明说不通过"

# ── 4. 方法形态的中文名：同样失败（名字在接收者之后） ─────────────────────

meth="$tmp/method"
mk_clean "$meth"
cat >>"$meth/pkg/impl.go" <<'EOF'

type recv struct{}

func (recv) 中文方法() {}
EOF

run "$meth"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "方法形态中文名 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   .*impl\.go:[0-9]* —— 中文方法$' "$last_out" ||
  bad "方法形态中文名 —— 要跳过接收者、报出名字本身（不是 recv 也不是整行）"

# ── 5. 泛型形态的中文名：同样失败（名字截到 `[` 之前） ─────────────────────

gen="$tmp/generic"
mk_clean "$gen"
cat >>"$gen/pkg/impl.go" <<'EOF'

func 映射[V any](v V) V { return v }
EOF

run "$gen"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "泛型形态中文名 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   .*impl\.go:[0-9]* —— 映射$' "$last_out" ||
  bad "泛型形态中文名 —— 名字要截到 [ 之前，不能把类型参数一起当成名字"

# ── 6. 泛型形态的英文名：不误报 ───────────────────────────────────────────
#
# 与上一条成对：类型参数里的 `[V any]` 不能被当成名字的一部分，
# 也不能因为多了个方括号就判成「解析不出名字」。

genok="$tmp/generic-ok"
mkdir -p "$genok/pkg"
cat >"$genok/pkg/g.go" <<'EOF'
package pkg

func Map[K comparable, V any](m map[K]V) []K { return nil }

func Slice[T any](v T) []T { return []T{v} }
EOF

run "$genok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "泛型形态英文名 —— 不该误报，实际退出码 $rc"; fi
grep -q '^扫描器跑了 1 个 .go 文件 / 2 个 func 声明$' "$last_out" ||
  bad "泛型形态英文名 —— 两条声明都要被数到（提取器不能把它们跳过）"

# ── 7. 非 ASCII 但不含汉字：同样失败 ──────────────────────────────────────
#
# 判据是「字节 > 0x7f」而不是「落在 CJK 区间」。按区间认的话，
# 下一个用西里尔字母或带重音的拉丁字母命名的人就漏过去了。

uni="$tmp/unicode"
mk_clean "$uni"
cat >>"$uni/pkg/impl.go" <<'EOF'

func Über() {}
EOF

run "$uni"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "带重音的函数名 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'Über' "$last_out" || bad "带重音的函数名 —— 要报出这个名字"

# ── 8. 行内中文：不被报出来 ───────────────────────────────────────────────
#
# 拿的是第 3 条那个夹具：它同时含「一个中文函数名」与各种行内中文形态，
# 所以正确答案是**只报前者**。判据从「标识符」放宽成「整行含非 ASCII」时，
# 这一条会响——而那正是本仓里真实存在的 5 处误报的样子：
# 一行写完的函数体里有中文、形参名叫 `原文`。

run "$plain"
grep -q '中文名字' "$last_out" || bad "行内中文 —— 那个中文函数名本身要照报"
if grep -qE '中文子测试名|中文日志|中文常量|随便一个错|—— 原文$|这个文件不是 Go 源码' "$last_out"; then
  bad "行内中文 —— 子测试名/字符串/常量名/形参名/非 Go 文件都不该被报出来"
else
  ok
fi

# ── 9. 非函数声明的中文标识符：不失败（已知边界，写在脚本开头） ───────────
#
# 包级常量名、类型名、变量名不在判据里。夹具 1 的 `const 中文常量` 已经钉住了常量，
# 这里再补类型名与变量名，把这条边界写全。

decl="$tmp/decls"
mkdir -p "$decl/pkg"
cat >"$decl/pkg/d.go" <<'EOF'
package pkg

type 中文类型 struct{}

var 中文变量 = 1

const 中文常量 = 2

func Fine() {}
EOF

run "$decl"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "非函数声明 —— 类型/变量/常量名不在判据里，应通过，实际 $rc"; fi

# ── 10. 一个 .go 都没扫到：退出码 2 ───────────────────────────────────────
#
# 这是「没跑成」而不是「干净」。判成通过的话，ROOT 写错就变成一次静默的假保证。

nogo="$tmp/nogo"
mkdir -p "$nogo/pkg"
printf 'func 这里没有 Go 文件()\n' >"$nogo/pkg/note.txt"

run "$nogo"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫不到 Go 文件 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个 \*.go 都没扫到' "$last_out" || bad "扫不到 Go 文件 —— 要说清是这个原因"

# ── 11. 有 Go 文件但一条 func 都没有：退出码 2 ────────────────────────────
#
# 判据写错（锚点形状不对）时就是这个样子：一条都不判，
# 于是输出与「没有违规」完全一样。

nofunc="$tmp/nofunc"
mkdir -p "$nofunc/pkg"
printf 'package pkg\n\nvar x = 1\n' >"$nofunc/pkg/a.go"

run "$nofunc"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "一条声明都没有 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一条 func 声明都没找到' "$last_out" || bad "一条声明都没有 —— 要说清是这个原因"

# ── 12. 解析不出函数名：退出码 2 ──────────────────────────────────────────
#
# 这一行**不是合法 Go**，它站在「Go 又多了一种这个提取器不认识的声明形状」的
# 位置上。那时一个名字都没在判，而「没在判」与「都干净」在输出上分不出来——
# 所以必须是响亮的失败，不是通过。

noname="$tmp/noname"
mkdir -p "$noname/pkg"
printf 'package pkg\n\nfunc ()\n' >"$noname/pkg/a.go"

run "$noname"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "解析不出名字 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '解析不出函数名' "$last_out" || bad "解析不出名字 —— 要说清是这个原因"

# ── 13. 多文件、多层目录：每一条违规都要报出来 ────────────────────────────
#
# 只报第一条、或者只扫第一层目录时，这里会响。

multi="$tmp/multi"
mkdir -p "$multi/a/b/c"
cat >"$multi/a/x.go" <<'EOF'
package a

func 第一个() {}
EOF
cat >"$multi/a/b/c/y.go" <<'EOF'
package c

func 第二个() {}
EOF

run "$multi"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "多层目录 —— 应失败（退出码 1），实际 $rc"; fi
if grep -q '^扫描器跑了 2 个 .go 文件 / 2 个 func 声明$' "$last_out"; then ok; else
  bad "多层目录 —— 两层目录都要扫到（应为 2 个文件 / 2 条声明）"
fi
grep -q '第一个' "$last_out" || bad "多层目录 —— 第一条违规要报出来"
grep -q '第二个' "$last_out" || bad "多层目录 —— 第二条违规也要报出来（不能只报第一条）"
grep -q '^✗ 有 2 个函数名的标识符含非 ASCII 字符' "$last_out" ||
  bad "多层目录 —— 计数要与列出来的条数一致"

# ── 14. 真仓库：这道门在它自己身上是干净的 ────────────────────────────────
#
# 前十三条用的是夹具。这一条跑真仓库，钉住「落地时确实改干净了」——
# 顺带证明夹具之外的真实文件树也能被扫（路径含 /、文件名形形色色）。

ROOT="$PWD" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "真仓库 —— 应通过（退出码 0），实际 $rc"; fi
repo_files=$(grep -oE '^扫描器跑了 [0-9]+ 个' "$last_out" | grep -oE '[0-9]+')
if [ "${repo_files:-0}" -ge 100 ]; then ok; else
  bad "真仓库 —— 只扫到 ${repo_files:-0} 个 Go 文件，扫描范围像是写错了"
fi

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
