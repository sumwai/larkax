#!/usr/bin/env bash
# 「Go 函数声明的标识符只用 ASCII」的门禁。
#
# # 为什么需要它
#
# AGENTS.md 写着「代码、标识符、命令、文件路径、必须保留原文的技术术语保持英文」。
# 这条约定在很长一段时间里只靠人自觉：落地这道门之前，全仓有 75 个 `func Test`
# 的标识符带中文，其中 71 个挤在 internal/plugin/sandbox 一个包里——一个包这么写，
# 下一个包就跟着写，而没有任何一步会失败（LESSONS.md 第 22 条：「有约定、没有门」）。
#
# 它不只是「不好看」：
#
#   - `go test -run` 的正则要写中文，而它在不同终端与日志管道里的编码不一定一致；
#   - CI 的失败行是要被人读的，函数名是那一行里最显眼的部分。
#
# 中文描述本身是好东西，所以改法不是删掉它，而是**换个位置**：
# 标识符用英文，原来的中文描述原样留在函数体上方一行注释里。
#
# 它最初只盯测试函数名（那是它的出身，文件名里的 test 也是这么来的）。
# 现在判据是**全部 `func` 声明**：约定说的是「标识符」，不是「测试函数名」，
# 而留几个已知的中文标识符在仓库里、门又照不到它们，正是这个项目不接受的那种
# 「悬着」——internal/plugin/host 那两个辅助函数就是这么被找出来的。
# 文件名保持不变，免得动 CI 与自测里的引用。
#
# # 判据
#
# `^func` 那一行里的**函数名**只许由 `[A-Za-z0-9_]` 组成。
# 三种形态都要认，取的都是名字那一个标识符：
#
#   func Name(...)          普通函数
#   func (r *T) Name(...)   方法——先跳过接收者再取名字
#   func Name[T any](...)   泛型函数——名字截到 `[` 之前
#
# **取的是函数名，不是整行。** 按整行判会误报一片：本仓实测至少 5 处的中文
# 出现在函数体或形参里，而不是名字上——
#
#   func plainErr() error { return errors.New("随便一个错") }
#   func (e *rejectedError) Error() string { return e.sentinel.Error() + "：" + e.detail }
#   func assertClassified(t *testing.T, err error, code string, 原文 error)
#
# 一行写完的函数、以及中文形参名，都会让「整行含非 ASCII」这种判据响。
#
# 「非 ASCII」在这里就是**字节 > 0x7f**：中文、日文、带重音的拉丁字母、西里尔字母
# 都落在 `[A-Za-z0-9_]` 之外，所以一个判据全都能抓住，不需要单独认 CJK 区间——
# 而按区间认的话，下一个用西里尔字母命名的人就漏过去了。
# 比对走 `LC_ALL=C grep`，按字节判，与运行环境的 locale 无关：
# 用 UTF-8 的字符类比对时，同一串字节在不同 locale 下会得到不同结论。
#
# # 已知边界（都是有意的）
#
# 1. **函数名以外的标识符不判**：包级常量名、类型名、变量名、形参名、局部变量名。
#    它们不出现在 `go test -run` 的正则里，也不出现在 CI 的失败行里——而那正是
#    这条约定的动机。**今天没有实例**：这条边界曾经有两个实例（两个测试记号常量），
#    它们后来也改成了英文。留着这段说明是给下一个人的——常量名不在判据里是
#    **有意的**，不是因为漏了。边界本身由自测第 9 条用夹具钉着（`type` / `var` /
#    `const` 各一个中文名，都不该让门失败）。
# 2. **非 Go 文件不判**：这条约定管的是 Go 源码里的标识符。
# 3. **只认行首的 `func`**：注释、字符串、函数值字面量里的同名形状不算。
#    所以 `t.Run("中文子测试名", …)` 不触发——子测试名是字符串，不是标识符，
#    而且它**要求保留中文**。
# 4. **解析不出名字时判「没跑成」**（退出码 2），不是判通过：那说明 Go 又多了一种
#    这个提取器不认识的声明形状，而那时它一个名字都没在判，输出却与「干净」一样
#    （LESSONS.md 第 16 条）。
#
# # 正向证据
#
# 这道门通过时是**沉默**的，而「什么都没输出」与「什么都没检查」长得一样
#（LESSONS.md 第 16 条：检查工具「没跑成」时看起来和「跑成了」一模一样）。
# 所以它每次都打印扫了多少个文件、找到了多少条 `func` 声明——
# 两个数都是 0 时它直接判「没跑成」（退出码 2），而不是报通过。
# CI 里那一步会检查这行证据确实出现过（见 .github/workflows/ci.yml）。
#
# # 文件清单
#
# 用 `find` 枚举全部 `*.go`，不是 `git ls-files`：这样**尚未提交的文件也在扫描
# 范围内**。只列已跟踪文件时，新写的文件在提交前不在门的范围里，而本机自查
# 「通过」会让人以为没问题（LESSONS.md 第 10 条）。
# 代价是 gitignore 掉的文件也会被扫——对一道门来说这个方向是安全的。
# `.git` / `node_modules` / `vendor` 剪掉，不进目录树。
#
# # 用法
#
#   scripts/check-test-names.sh
#
# 退出码：
#   0  通过
#   1  有函数名含非 ASCII 字符
#   2  没跑成：一个 `*.go` 都没扫到、一条 `func` 声明都没找到、某个 `func`
#      行里解析不出函数名、或 grep 自身出错
#
# 1 与 2 必须分开：两者在 CI 里都是红的，但处置完全不同——前者要去改函数名，
# 后者说明这道门自己坏了。
#
# # 环境变量
#
#   ROOT  扫描根，默认 `.`。给 scripts/check-test-names.test.sh 指向夹具树用，
#         CI 与本地都不该设。
#
# # 自测
#
# scripts/check-test-names.test.sh。改动本脚本后必须跑它：
# 一个把判据写反的扫描器，输出与「干净」完全一样。

set -uo pipefail
cd "$(dirname "$0")/.."

ROOT="${ROOT:-.}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# ── 枚举文件 ──────────────────────────────────────────────────────────────
#
# `.git` 在 worktree 里是个文件、在主检出里是个目录，两种都要剪掉：
# 不剪的话主检出会去遍历整个对象库。

files=$(find "$ROOT" \
  \( -name .git -o -name node_modules -o -name vendor \) -prune -o \
  -type f -name '*.go' -print 2>/dev/null | LC_ALL=C sort)

file_count=$(printf '%s\n' "$files" | grep -c . || true)
if [ "$file_count" -eq 0 ]; then
  not_run "在 $ROOT 下一个 *.go 都没扫到。
路径写错、或者扫描根不对时就是这样——那不是「干净」，是没扫。"
fi

# ── 找 `func` 声明 ────────────────────────────────────────────────────────
#
# 逐个文件 grep 而不是一次传一串文件名：这样每个文件的退出码都能单独看。
# 合并成一次调用时，grep 的「没命中」（1）与「执行失败」（2）会混在一起，
# 而前者是正常结果、后者必须致命——红线扫描器就是在这里栽过一次
#（LESSONS.md 第 2 条：把退出码 2 和 1 一起吞掉，于是每次都判定为「干净」）。
#
# `-H` 强制带文件名前缀：只有一个文件时 grep 默认不打前缀，
# 而这里每条命中都要能指回文件。

decls="$tmpdir/decls"
: >"$decls"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  out=$(LC_ALL=C grep -HnE '^func[[:space:]]' -- "$f" 2>&1)
  rc=$?
  if [ "$rc" -gt 1 ]; then
    echo "错误：grep 在 $f 上执行失败（退出码 $rc）：" >&2
    printf '%s\n' "$out" | sed 's/^/   /' >&2
    exit 2
  fi
  [ -n "$out" ] && printf '%s\n' "$out" >>"$decls"
done <<<"$files"

decl_count=$(grep -c . "$decls" || true)
if [ "$decl_count" -eq 0 ]; then
  not_run "扫了 $file_count 个 *.go，却一条 func 声明都没找到。
Go 源码里不可能没有函数，找不到就说明这条判据写错了——不是「没有违规」。"
fi

# ── 逐条判 ────────────────────────────────────────────────────────────────

bad=0
bad_count=0
: >"$tmpdir/bad"
while IFS= read -r line; do
  [ -n "$line" ] || continue
  # grep -Hn 的输出形如 <文件>:<行号>:<源码>
  rest=${line#*:}
  lineno=${rest%%:*}
  code=${rest#*:}

  # 取函数名：先去 func，再去空白，再跳过方法的接收者，最后截到 ( 或 [ 之前。
  # 全程用 bash 的参数展开（按字节走），不引第二个工具。
  name=${code#func}
  name=${name#"${name%%[![:space:]]*}"}
  case "$name" in
    "("*) name=${name#*)}; name=${name#"${name%%[![:space:]]*}"} ;;
  esac
  name=${name%%(*}
  name=${name%%\[*}

  # 提取不出名字：Go 又多了一种这里不认识的声明形状。此时一个名字都没在判，
  # 而「没在判」与「都干净」在输出上分不出来，所以这里必须响亮地失败。
  if [ -z "$name" ]; then
    not_run "${line%%:*}:$lineno 的 func 行里解析不出函数名：
   $code
这多半是 Go 又多了一种声明形状，而这道门还不认识它。
认不出就不能当作通过——那等于这一段根本没被检查。"
  fi

  if LC_ALL=C grep -q '[^A-Za-z0-9_]' <<<"$name"; then
    printf '%s:%s —— %s\n' "${line%%:*}" "$lineno" "$name" >>"$tmpdir/bad"
    bad=1
    bad_count=$((bad_count + 1))
  fi
done <"$decls"

# ── 输出 ──────────────────────────────────────────────────────────────────
#
# 通过时也把正向证据打出来，理由见文件开头「正向证据」那一节。

echo "扫描器跑了 $file_count 个 .go 文件 / $decl_count 个 func 声明"
echo

if [ "$bad" -ne 0 ]; then
  echo "✗ 有 $bad_count 个函数名的标识符含非 ASCII 字符（判据见本脚本开头）："
  sed 's/^/   /' "$tmpdir/bad"
  echo
  echo "  改法：标识符用英文，原来的中文描述原样挪到函数体上方一行注释。"
  echo "  例："
  echo "      // 宿主错误原文不回显给插件。"
  echo "      func TestHostErrorTextIsNotEchoedToPlugin(t *testing.T) {"
  echo
  echo "函数名门禁不通过。"
  exit 1
fi

echo "函数名门禁通过。"
