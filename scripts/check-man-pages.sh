#!/usr/bin/env bash
# 「man/ 下的手页与两份源文件同步」的门禁。
#
# # 为什么需要它
#
# 手页是**产物**：它由命令注册表（cmd/larkaxctl 的 commandGroups）与
# docs/cli-reference.md 生成，生成一次提交进库。产物与来源分家的样子是安静的——
# 改了注册表里的一句话、改了文档的某一节，却没重新生成，那么库里的手页仍然说着
# 旧话。它在 CI 里不会让任何一步失败，只有照着 `man larkaxctl-channel` 敲的人
# 才会撞上；而那时他多半以为是自己的问题。
#
# 形状照抄 cargo 的 validate-man.sh：**重新生成一遍，与提交的那一份比对**，
# 对不上就红。差别只有一处：这里比对的是两个目录里的文件集合，不是 `diff -ru`
# （理由见下面「为什么不是 diff -ru」）。
#
# # 判据（三条）
#
#   1. 生成器产出的每一页，在 $MAN 下都有一份**逐字节相同**的。
#   2. $MAN 下每一页 `larkaxctl*.1` 都是生成器产出的（生成器不再产出它，说明那条
#      命令被删了或改了名，而它那一页还留在库里）。
#   3. 每一页都非空、且以 `.TH ` 开头。
#
# 1 与 2 是一对：只判 1 时，删掉一条命令后它那一页留在库里不算错；只判 2 时，
# 手页整体落后于源文件不算错。两条合起来才是集合相等。
#
# 3 是第二道守卫，第一道在 cmd/larkaxctl/man_test.go 的不变量里（它判的是内存里
# 那份 Content），而这里判的是生成器**落到临时目录**的字节。同一次运行里后者先红：
# 生成器写出空页时，那条测试就没过，这道门以「没跑成」退出（退出码 2），走不到
# 判据 3。留着它，是因为磁盘上那一份正是后面要拿去比对的东西——它在不在、是不是
# 空文件，只有这里看得见。
#
# # 为什么不是 diff -ru
#
# 两个理由。其一，差一个文件时 `diff -ru` 的输出是「Only in …」，报出来的是
# 目录级的差异，读的人还得自己找是哪一页；逐页循环能把页名写在失败行里。其二，
# man/ 以后可能放进**不是这里生成的**页（比如服务端二进制的 larkaxd.1）：`diff -ru`
# 会把「多出来的那一页」一律当成差异，而判据 2 只认 `larkaxctl*.1` 这一族，
# 别的文件既不比也不报——它们会出现在正向证据行里。
#
# # 已知边界（都是有意的）
#
# 1. **只判「同不同」，不判「对不对」**。手页读起来通不通顺、roff 宏有没有用错、
#    中文有没有被断在难看的字后面，都不是这道门的判据。它管的是「产物与来源
#    是不是同一次生成的」。
# 2. **只认 `larkaxctl*.1`**（见上）。别的文件名不在判据 1、2 里。
# 3. **生成器的名字与位置写死在这里**：`go test -run '^TestGenerateManPages$'
#    ./cmd/larkaxctl/`。产物由一条测试生成（注册表在 package main 里，别的包引
#    不到，理由见 cmd/larkaxctl/man_test.go），这道门与它是同一件事的两半，不另开
#    一个配置项把位置说第二遍。
# 4. **不判 git 状态**。库里有没有提交、有没有暂存，都不是门的事：比对的是磁盘上
#    的 $MAN，`git status` 由提交的人自己看。
#
# # 正向证据
#
# 通过时是**沉默**的，而「什么都没输出」与「什么都没检查」长得一样（LESSONS.md
# 第 16 条）。所以它每次都打印一行：生成器产出几页、$MAN 下有几页（其中比对的有
# 几页）、逐页比对了几次。生成器一页都没写出来、或者写出空白页时，它判「没跑成」
# （退出码 2），不报通过——那时候它不是「同步」，是没生成。
#
# # 用法
#
#   scripts/check-man-pages.sh          # 生成到临时目录，与 $MAN 比对
#   scripts/check-man-pages.sh --update # 生成到 $MAN（改了源文件之后跑这个）
#
# 退出码：
#   0  三条判据都成立
#   1  有判据不成立（逐条列出是哪一页）
#   2  没跑成：生成器非零退出、一页都没写出来、写出了空页或没有 .TH 的页、
#      $MAN 不在
#
# 1 与 2 必须分开：两者在 CI 里都是红的，但处置完全不同——前者去跑 --update 并
# 提交，后者说明这道门自己（或生成器）坏了（LESSONS.md 第 16 条）。
#
# # 为什么是 man/man1 而不是 man/
#
# man-db 只认 MANPATH 下的 `<section>/` 子目录（`man1/`、`man5/`…），平铺在
# MANPATH 根下的 *.1 它一份都找不到。放进 man/man1 之后，指向仓库就能翻：
#
#   MANPATH="$PWD/man" man larkaxctl-channel
#
# 平铺的话只有 `man -l man/larkaxctl-channel.1` 这一种翻法，而那意味着每个人都要
# 知道自己手上这份源码树在哪、以及那个 -l 是什么。
#
# # 环境变量
#
#   MAN  已提交的手页目录，默认 `man/man1`。给自测指向夹具用。
#   GO   go 命令，默认 `go`。给自测换成一个写死页面的桩用。
#
# # 自测
#
# scripts/check-man-pages.test.sh。改动本脚本后必须跑它：一个比对写反的同步门，
# 输出与「同步」完全一样（LESSONS.md 第 2、7 条）。

set -uo pipefail
cd "$(dirname "$0")/.."

MAN="${MAN:-man/man1}"
GO="${GO:-go}"
PKG="./cmd/larkaxctl/"

update=0
case "${1:-}" in
  "") ;;
  --update) update=1 ;;
  *)
    echo "用法：$0 [--update]" >&2
    exit 2
    ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

out="$tmp/out"

# ── 生成 ──────────────────────────────────────────────────────────────────
#
# `-count=1` 不是可选的：go test 会缓存结果，而缓存命中时**这条测试根本没跑**，
# 那个临时目录就是空的——门会报「生成器一页都没写出来」，而人照着这句话去查
# 生成器，查到的是一切正常。

if ! LARKAX_MAN_OUT="$out" $GO test -count=1 -run '^TestGenerateManPages$' "$PKG" \
  >"$tmp/gen.log" 2>&1; then
  not_run "生成器没跑成（$GO test 非零退出）：
$(tail -20 "$tmp/gen.log" | sed 's/^/    /')
生成器自己坏了时，这道门没有可判的东西。"
fi

generated="$tmp/generated"
find "$out" -maxdepth 1 -name '*.1' -type f -print 2>/dev/null | LC_ALL=C sort >"$generated"
gen_count=$(grep -c . "$generated" || true)
if [ "$gen_count" -lt 2 ]; then
  not_run "生成器产出 $gen_count 页。
一页都没有（或只有一页）时它什么都没生成，而它的输出与「同步」长得一样。
生成日志的最后几行：
$(tail -10 "$tmp/gen.log" | sed 's/^/    /')"
fi

# 每页都得非空、以 .TH 开头：空文件与「内容一致」在比对里分不出来。
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if [ ! -s "$f" ]; then
    not_run "生成器把 $f 写成了空文件。"
  fi
  if [ "$(head -c 4 "$f")" != ".TH " ]; then
    not_run "生成器写出的 $f 不是以 .TH 开头，man 认不出这一页。"
  fi
done <"$generated"

# 环境变量没设时生成器只核对、不落盘（见 man_test.go），所以上面这几条也在证明
# 「LARKAX_MAN_OUT 真的被生成器看见了」——否则它一页都不会写，这里早就退出 2 了。

# ── --update：把生成结果搬进 $MAN ─────────────────────────────────────────

if [ "$update" -eq 1 ]; then
  [ -d "$MAN" ] || mkdir -p "$MAN" || not_run "建不了目录 $MAN。"
  # 只删这一族的页（判据 2 的边界）：别的文件（比如以后手工补的一页）不动。
  rm -f "$MAN"/larkaxctl.1 "$MAN"/larkaxctl-*.1
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    cp "$f" "$MAN/" || not_run "把 $f 拷进 $MAN 失败。"
  done <"$generated"
  echo "已把 $gen_count 页写进 $MAN/。"
  exit 0
fi

# ── 判据 1 与判据 2 ───────────────────────────────────────────────────────

[ -d "$MAN" ] || not_run "$MAN 不在。
它要么还没生成过（跑 scripts/check-man-pages.sh --update），要么 MAN 指错了。"

bad=0
: >"$tmp/bad"
compared=0

# 判据 1：生成器产出的每一页，$MAN 下都要有逐字节相同的一份。
while IFS= read -r f; do
  [ -n "$f" ] || continue
  base=${f##*/}
  if [ ! -f "$MAN/$base" ]; then
    printf '判据 1 · %s/%s 不存在（生成器产出了它）\n' "$MAN" "$base" >>"$tmp/bad"
    bad=1
    continue
  fi
  if ! cmp -s "$f" "$MAN/$base"; then
    printf '判据 1 · %s/%s 与刚生成的不一致\n' "$MAN" "$base" >>"$tmp/bad"
    bad=1
  fi
  compared=$((compared + 1))
done <"$generated"

# 判据 2：$MAN 下这一族的每一页都要是生成器产出的。
committed_count=0
for f in "$MAN"/larkaxctl*.1; do
  [ -f "$f" ] || continue
  committed_count=$((committed_count + 1))
  base=${f##*/}
  if ! grep -qxF "$out/$base" "$generated"; then
    printf '判据 2 · %s/%s 生成器不再产出（那条命令改了名或被删了）\n' "$MAN" "$base" >>"$tmp/bad"
    bad=1
  fi
done

# $MAN 下别的文件（不是这一族的）：既不比也不报，但要说出来——「没看见」与
# 「看见了、按边界不管」在输出里长得一样。
other=0
for f in "$MAN"/*.1; do
  [ -f "$f" ] || continue
  case "${f##*/}" in larkaxctl*) continue ;; esac
  other=$((other + 1))
done

# ── 输出 ──────────────────────────────────────────────────────────────────

echo "证据：生成器产出 $gen_count 页 / $MAN 下这一族的页 $committed_count 页、别的 .1 文件 $other 个 / 逐页比对 $compared 次"
echo

if [ "$bad" -ne 0 ]; then
  echo "✗ man/ 下的手页与源文件不同步（判据见本脚本开头）："
  sed 's/^/   /' "$tmp/bad"
  echo
  echo "重新生成并提交："
  echo "  scripts/check-man-pages.sh --update"
  exit 1
fi

echo "三条判据都成立：$gen_count 页手页都在 $MAN 下且逐字节相同，$MAN 下这一族的 $committed_count 页都有出处。"
echo "门禁通过。"
