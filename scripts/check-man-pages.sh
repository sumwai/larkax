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
# # 判据（四条）
#
#   1. 生成器产出的每一页，在 $MAN 下都有一份**逐字节相同**的。
#   2. $MAN 下每一页 `larkaxctl*.1` 都是生成器产出的（生成器不再产出它，说明那条
#      命令被删了或改了名，而它那一页还留在库里）。
#   3. 每一页都非空、且以 `.TH ` 开头。
#   4. 手页里汉字与拉丁字母之间没有被吃掉的空格。
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
# 4 判「中英之间有没有少空格」：手页里汉字与拉丁字母直接挨着，几乎总是接硬换行
# 时把原来的空格丢掉了（判据在 internal/manpage 的 needsSpace）。这类缺陷在产物
# 与来源之间是**一致**的，判据 1、2 永远绿，只有读页面的人才会撞上。扫的是生成器
# 刚写出来的那份，不是库里的：判据 1 已经保证两者逐字节相同。
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
# 1. **判据 1、2 只判「同不同」，不判「对不对」**。roff 宏有没有用错、中文有没有
#    被断在难看的字后面，仍然不是这道门的判据。判据 4 是唯一一条「对不对」的
#    判据，它只管得住一件可机器判的事：字母与汉字之间有没有空格。
# 2. **只认 `larkaxctl*.1`**（见上）。别的文件名不在判据 1、2、4 里。
# 3. **生成器的名字与位置写死在这里**：`go test -run '^TestGenerateManPages$'
#    ./cmd/larkaxctl/`。产物由一条测试生成（注册表在 package main 里，别的包引
#    不到，理由见 cmd/larkaxctl/man_test.go），这道门与它是同一件事的两半，不另开
#    一个配置项把位置说第二遍。
# 4. **不判 git 状态**。库里有没有提交、有没有暂存，都不是门的事：比对的是磁盘上
#    的 $MAN，`git status` 由提交的人自己看。
# 5. **判据 4 不判数字**：数字与汉字紧贴是合法的写法（「4096 个」「3 行」里也
#    有不带空格的形式），逐个报会淹没真问题。它也不判全角标点与字母相邻——
#    `（status`、`status，` 是中文排版的正写法。
# 6. **判据 4 只认汉字主体**（U+4E00–U+9FFF 所在的 UTF-8 首字节区间）。假名、
#    扩展区、全角标点不在范围内：手页是中文的，那几档一个都没有。
#
# # 正向证据
#
# 通过时是**沉默**的，而「什么都没输出」与「什么都没检查」长得一样（LESSONS.md
# 第 16 条）。所以它每次都打印计数：生成器产出几页、$MAN 下有几页（其中比对的有
# 几页）、逐页比对了几次；判据 4 报出扫了几页、几处紧贴、白名单放行了几处以及
# 白名单的条数。--update 只打印紧贴那一行（再加上一句写进去几页）：比对那几项
# 在写库这条路上不成立。生成器一页都没写出来、或者写出空白页时，它判「没跑成」
# （退出码 2），不报通过——那时候它不是「同步」，是没生成。
#
# # 用法
#
#   scripts/check-man-pages.sh          # 生成到临时目录，与 $MAN 比对
#   scripts/check-man-pages.sh --update # 生成到 $MAN（改了源文件之后跑这个）
#
# --update 也跑判据 4：一份读起来少空格的手页不该被写进库里，哪怕它与来源一致。
#
# 退出码：
#   0  四条判据都成立
#   1  有判据不成立（逐条列出是哪一页、哪一行）
#   2  没跑成：生成器非零退出、一页都没写出来、写出了空页或没有 .TH 的页、
#      $MAN 不在、判据 4 的扫描器自己出错、剥完转义后还有不认识的 roff 转义
#
# 1 与 2 必须分开：两者在 CI 里都是红的，但处置完全不同——前者去跑 --update 并
# 提交（判据 4 红则回去改内容或生成器），后者说明这道门自己（或生成器）坏了
# （LESSONS.md 第 16 条）。
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
#   MAN        已提交的手页目录，默认 `man/man1`。给自测指向夹具用。
#   GO         go 命令，默认 `go`。给自测换成一个写死页面的桩用。
#   WHITELIST  判据 4 的白名单，默认 scripts/man-pages-allowlist.txt。给自测指向
#              夹具用。
#
# # 自测
#
# scripts/check-man-pages.test.sh。改动本脚本后必须跑它：一个比对写反的同步门，
# 输出与「同步」完全一样（LESSONS.md 第 2、7 条）。

set -uo pipefail
cd "$(dirname "$0")/.."

MAN="${MAN:-man/man1}"
GO="${GO:-go}"
WHITELIST="${WHITELIST:-scripts/man-pages-allowlist.txt}"
PKG="./cmd/larkaxctl/"

# 判据 4 白名单的条数上限。**只许减不许增**：新增一条就要同时把这个数往上调，
# 而那是一次会被 review 看到的 diff。自测把这个字面量钉住（见
# check-man-pages.test.sh）——所以往上调要动两个文件，不会悄悄发生。
MAX_ALLOW_ENTRIES=1

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

# ── 判据 4：中英之间的空格有没有被吃掉 ────────────────────────────────────
#
# 判的是「字母与汉字直接挨着」：手页里这类紧贴几乎总是接硬换行时丢的——源文件
# 那个换行替掉了原来的空格，而生成器没有补回来（判据在 internal/manpage 的
# needsSpace）。数字与汉字紧贴是合法写法（「4096 个」），全角标点与字母相邻也
# 是中文排版的正写法（`（status`），两者都不在本判据里。

bad=0
: >"$tmp/bad"
# 这两个变量分别记「同步」与「紧贴」各有没有红，输出时的处置建议不一样。
bad_sync=0
bad_spacing=0

# 汉字在 UTF-8 里的字节形状：主体（U+4E00–U+9FFF）首字节 0xE4–0xE9，后两个
# 字节 0x80–0xBF。按字节判、LC_ALL=C——`\p{Han}` 只有 grep -P 认得，而 BSD 的
# grep 不认；字节区间两边是同一套。这个首字节区间顺带覆盖 U+4000–U+4DBF（扩展 A，
# 本来就是汉字）与 U+4DC0–4DFF 的易经符号；漏掉的是扩展 A 的 U+3400–3FFF 段、
# 假名（0xE3 打头）与全角标点（0xEF 打头）——手页是中文的，这几档一个都没有。
HAN_START=$'\xe4' HAN_END=$'\xe9'
BYTE_START=$'\x80' BYTE_END=$'\xbf'
TIGHT_RE="[A-Za-z][$HAN_START-$HAN_END][$BYTE_START-$BYTE_END][$BYTE_START-$BYTE_END]"
TIGHT_RE="$TIGHT_RE|[$HAN_START-$HAN_END][$BYTE_START-$BYTE_END][$BYTE_START-$BYTE_END][A-Za-z]"

# 白名单：三栏 `<页名> | <紧贴片段> | <理由>`，与 scripts/*-allowlist.txt 同一个
# 惯例。比对按「页名 + 片段」做集合匹配，所以同一个片段在同一页里出现几次都算
# 一条；同样的条目写两遍不会多算，只会让条数虚高。
allow_rows="$tmp/allow"
: >"$allow_rows"
allow_count=0
if [ -f "$WHITELIST" ]; then
  bad_allow_lines=$(awk -F'|' '
    /^[[:space:]]*$/ { next }
    /^[[:space:]]*#/ { next }
    {
      if (NF != 3) { print NR; next }
      for (i = 1; i <= 3; i++) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", $i) }
      if ($1 == "" || $2 == "" || $3 == "") { print NR }
    }' "$WHITELIST")
  if [ -n "$bad_allow_lines" ]; then
    echo "✗ 白名单 $WHITELIST 有不合格式的行（行号：$(printf '%s' "$bad_allow_lines" | tr '\n' ' ')）。" >&2
    echo "  每行三栏：<页名> | <紧贴片段> | <理由>，用 | 隔开。" >&2
    exit 1
  fi
  awk -F'|' '
    /^[[:space:]]*$/ || /^[[:space:]]*#/ { next }
    {
      for (i = 1; i <= 3; i++) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", $i) }
      print $1 "|" $2 "|" $3
    }' "$WHITELIST" >"$allow_rows"
  allow_count=$(grep -c . "$allow_rows" || true)
fi

if [ "$allow_count" -gt "$MAX_ALLOW_ENTRIES" ]; then
  echo "✗ 白名单 $WHITELIST 有 $allow_count 条，超过上限 $MAX_ALLOW_ENTRIES。"
  echo "  条目数只许减不许增：新增一条就要同时把 MAX_ALLOW_ENTRIES 往上调，"
  echo "  而那个改动会被 review 看到。要么改掉紧贴的写法，要么说清为什么必须加。"
  exit 1
fi

# 扫描前把 roff 转义换成它渲染出来的样子：`\:`、`\fB`/`\fR`/`\fP`/`\fI`、`\&`
# 都是零宽的，直接删掉；`\-` 是连字符，`\(bu` 是列表的圆点，`\e` 是反斜杠。
# 不剥这一步，`认\fBstatus` 里那两个字中间隔着四个字节，判据什么也看不见。
#
# 这张表是写死的，所以剥完还要查一遍有没有**不认识的**转义留了下来：一个
# `\s-1` 之类的序列横在汉字与字母之间时，判据 4 两边都看不见，把缺陷当成
# 干净——而输出与「一处紧贴都没有」一模一样。`\e` 换成**两个**反斜杠，正是
# 为了在下一步把它与不认识的转义分开：游戏规则变了，剩下的单个反斜杠都不是
# 合法的 roff（字面量反斜杠必须写成 `\e`）。
stripped="$tmp/stripped"
mkdir -p "$stripped"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if ! sed -e 's/\\f[BRPI]//g' -e 's/\\://g' -e 's/\\-/-/g' -e 's/\\&//g' -e 's/\\(bu/·/g' -e 's/\\e/\\\\/g' \
    "$f" >"$stripped/${f##*/}"; then
    not_run "剥 roff 转义时 sed 失败（$f）。"
  fi
done <"$generated"

# 单个反斜杠＝不认识的转义。先把成对的（来自 `\e`）删掉再看——正则里数不了
# 奇偶，删成对的再找剩下的那一个才是对的判定。删完再整批扫一遍：命中与否看
# 输出文件，不看逐文件循环的退出码（最后一个文件的退出码盖住前面的）。
nopairs="$tmp/nopairs"
mkdir -p "$nopairs"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if ! sed 's/\\\\//g' "$f" >"$nopairs/${f##*/}"; then
    not_run "判据 4 的转义检查里 sed 失败（$f）。"
  fi
done < <(find "$stripped" -maxdepth 1 -name '*.1' -type f -print | LC_ALL=C sort)

unknown_rc=0
LC_ALL=C grep -n '\\' "$nopairs"/*.1 >"$tmp/unknown-escape.raw" 2>"$tmp/unknown-escape.err" || unknown_rc=$?
sed "s|^$nopairs/||" "$tmp/unknown-escape.raw" >"$tmp/unknown-escape"
if [ "$unknown_rc" -gt 1 ]; then
  not_run "判据 4 的转义检查没跑成（grep 退出码 $unknown_rc）：
$(sed 's/^/    /' "$tmp/unknown-escape.err")
检查器坏掉时的输出与「没有不认识的转义」完全一样，所以这里不报通过。"
fi
if [ -s "$tmp/unknown-escape" ]; then
  not_run "剥完 roff 转义之后手页里还有不认识的转义：
$(head -5 "$tmp/unknown-escape" | sed 's/^/    /')
判据 4 判的是汉字与字母**直接相邻**；不认识的序列夹在中间时它两边都看不见，
会把缺陷当成干净。先在脚本的 sed 替换表里认下这个转义，再重新跑。"
fi

hits="$tmp/hits"
: >"$hits"
scan_rc=0
LC_ALL=C grep -n -o -E "$TIGHT_RE" "$stripped"/*.1 >"$hits" 2>"$tmp/scan.err" || scan_rc=$?
if [ "$scan_rc" -gt 1 ]; then
  not_run "判据 4 的扫描器没跑成（grep 退出码 $scan_rc）：
$(sed 's/^/    /' "$tmp/scan.err")
扫描器坏掉时的输出与「一处紧贴都没有」完全一样，所以这里不报通过。"
fi

tight=0
allowed=0
used="$tmp/allow.used"
: >"$used"
while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  tight=$((tight + 1))
  page=${hit%%:*}
  page=${page##*/}
  rest=${hit#*:}
  lineno=${rest%%:*}
  pair=${rest#*:}
  if awk -F'|' -v p="$page" -v q="$pair" \
    '$1 == p && $2 == q { found = 1 } END { exit !found }' "$allow_rows"; then
    allowed=$((allowed + 1))
    printf '%s|%s\n' "$page" "$pair" >>"$used"
    continue
  fi
  printf '判据 4 · %s/%s:%s 里「%s」紧贴：汉字与字母之间少了空格（接硬换行时被吃掉了）\n' \
    "$MAN" "$page" "$lineno" "$pair" >>"$tmp/bad"
  bad=1
  bad_spacing=1
done <"$hits"

# 按文件名而不是 NR == FNR 分文件：第一个文件为空时，后一个文件的第一条也会满足
# NR == FNR，陈旧条目就永远算不出来。
stale=$(awk -F'|' 'FILENAME == ARGV[1] { seen[$1 "|" $2] = 1; next }
  !(($1 "|" $2) in seen) { print $1 " | " $2 }' "$used" "$allow_rows")
if [ -n "$stale" ]; then
  echo "提示：白名单 $WHITELIST 里这些条目已经不再命中，可以删掉："
  printf '%s\n' "$stale" | sed 's/^/   /'
fi

# ── --update：把生成结果搬进 $MAN ─────────────────────────────────────────

if [ "$update" -eq 1 ]; then
  # 判据 4 红就不写：一份读起来少空格的手页不该进库，哪怕它与来源逐字节一致。
  if [ "$bad_spacing" -ne 0 ]; then
    echo "✗ 生成出来的手页没通过判据 4，不写进 $MAN："
    sed 's/^/   /' "$tmp/bad"
    echo
    echo "改生成器（internal/manpage 的 needsSpace）或源文件里的写法，再重新生成。"
    exit 1
  fi
  [ -d "$MAN" ] || mkdir -p "$MAN" || not_run "建不了目录 $MAN。"
  # 只删这一族的页（判据 2 的边界）：别的文件（比如以后手工补的一页）不动。
  rm -f "$MAN"/larkaxctl.1 "$MAN"/larkaxctl-*.1
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    cp "$f" "$MAN/" || not_run "把 $f 拷进 $MAN 失败。"
  done <"$generated"
  echo "证据：生成器产出 $gen_count 页 / 判据 4 扫了 $gen_count 页、紧贴 $tight 处（白名单放行 $allowed 处）"
  echo "已把 $gen_count 页写进 $MAN/。"
  exit 0
fi

# ── 判据 1 与判据 2 ───────────────────────────────────────────────────────

[ -d "$MAN" ] || not_run "$MAN 不在。
它要么还没生成过（跑 scripts/check-man-pages.sh --update），要么 MAN 指错了。"

compared=0

# 判据 1：生成器产出的每一页，$MAN 下都要有逐字节相同的一份。
while IFS= read -r f; do
  [ -n "$f" ] || continue
  base=${f##*/}
  if [ ! -f "$MAN/$base" ]; then
    printf '判据 1 · %s/%s 不存在（生成器产出了它）\n' "$MAN" "$base" >>"$tmp/bad"
    bad=1
    bad_sync=1
    continue
  fi
  if ! cmp -s "$f" "$MAN/$base"; then
    printf '判据 1 · %s/%s 与刚生成的不一致\n' "$MAN" "$base" >>"$tmp/bad"
    bad=1
    bad_sync=1
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
    bad_sync=1
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
echo "证据：中英紧贴扫描 $gen_count 页、$tight 处（白名单放行 $allowed 处；白名单 $allow_count 条 / 上限 $MAX_ALLOW_ENTRIES）"
echo

if [ "$bad" -ne 0 ]; then
  echo "✗ 手页没通过（判据见本脚本开头；每条都点出了页与行）："
  sed 's/^/   /' "$tmp/bad"
  echo
  if [ "$bad_sync" -ne 0 ]; then
    echo "判据 1、2 红：重新生成并提交："
    echo "  scripts/check-man-pages.sh --update"
  fi
  if [ "$bad_spacing" -ne 0 ]; then
    echo "判据 4 红：改生成器（internal/manpage 的 needsSpace）或源文件里的写法，"
    echo "再重新生成；确实该紧贴的地方进 $WHITELIST。"
  fi
  exit 1
fi

echo "四条判据都成立：$gen_count 页手页都在 $MAN 下且逐字节相同，$MAN 下这一族的 $committed_count 页都有出处，字母与汉字之间没有丢空格。"
echo "门禁通过。"
