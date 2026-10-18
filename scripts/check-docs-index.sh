#!/usr/bin/env bash
# 「docs/ 的两处索引与正文一致」的门禁。
#
# # 为什么需要它
#
# 仓库里的文档索引有两处，都只会在读者那里现形：
#
#   - docs/README.md 的「想做 X → 看 Y」分组 ↔ docs/ 下的文件；
#   - docs/development.md 顶部的 TOC ↔ 文中那些标题。
#
# 它与 check-lessons-index.sh 管的那一对是同一个形状（LESSONS.md 第 22 条：
# 有实现、没有门），坏掉的样子也相同：**索引里点进去是 404**，或者更坏——
# 点进去是别的东西。而这两种在 CI 里都不会有任何一步失败
#（第 16 条：「没跑成」与「跑成了」的输出一模一样）。
#
# # 判据（三条，刻意收窄）
#
#   1. docs/ 下的每一份 *.md（`README.md` 自身除外），在 docs/README.md 里
#      被链接到。（正文 → 索引）
#   2. docs/README.md 里每一条**指向本目录 .md** 的链接，目标文件在 docs/ 下
#      存在。（索引 → 正文）
#   3. docs/development.md 里**指回本文的每一个锚点**（TOC 行上的 `](#…)`），
#      在文中都有一个标题，其派生锚点与它相同。
#
# 1 与 2 是一对，缺一条另一条就漏一类：只判 1 时，索引里链一份不存在的文档
# 不算错（它只是**没覆盖到**哪份文档）；只判 2 时，新写一份文档忘了进索引也
# 不算错。两条合起来才是集合相等。
#
# 三条都只判「能机器判的」那一半：**判不了的不做成脚本**。文件里的叙述与代码
# 是否一致、索引分组的措辞好不好、哪些标题该进 TOC，都不在这里——那是人读的
# 东西，写成脚本只会得到一个没人看的假保证。
#
# # 索引行 / TOC 行是怎么认出来的
#
# 判据 1 与判据 2 认的是**同一批链接**（判据 1 拿它当「覆盖了哪些文档」，
# 判据 2 拿它逐条去要文件），也就是目标形如 `某文档.md` 的那种（可以带锚点：
# `某文档.md#某节`）。据此：
#
#   - `](../README.md)`、`](../contract/README.md)` **不算**：目标里含 `/`，
#     它指的不是 docs/ 下的文件。索引里指向别处的链接既不进判据 1 的对象，
#     也不被判据 2 要求存在（见「已知边界」）。
#   - `](#某节)` **不算**：那是本文内部的锚点，没有 .md。
#   - 一行里出现几个链接就算几条。今天 docs/README.md 里有几行链了不止一份
#     （`development.md` 被链了四次），所以「条数」与「覆盖的份数」是两个数，
#     两个都打进正向证据里。
#
# 判据 3 认的 TOC 行是「去掉行首空白后以 `- [` 打头、且含 `](#` 的行」。
# 只看这一类行是有意的：索引是**列出来的**，它坏掉的样子是把某一项写成别的
# 形状（那时锚点数为 0，直接判「没跑成」，见下），而正文里的行内锚点不在
# 这条判据里（见「已知边界」）。
#
# # 锚点是怎么算出来的
#
# 标题 → 锚点的规则是 GitHub 那一套，本脚本按下面的顺序自己实现它：
#
#   1. ASCII 大写转小写（CJK 不动）；
#   2. 删掉中文标点：（）「」『』、，。；：？！·—–…《》〈〉【】〔〕；
#   3. 删掉 ASCII 标点，但**留下** `-` 与 `_`（GitHub 的规则就是这样：
#      它删的是 `!-,` `./` `:-@` `[-\^` `` ` `` `{-~` 这七段，`-` 与 `_`
#      恰好落在段与段之间）；
#   4. 每个空格换成一个 `-`。
#
# 第 3 步只删 **ASCII 字节**（< 0x80），这是一个必须守住的性质：UTF-8 的
# 多字节序列里不出现 < 0x80 的字节，所以按字节删除**碰不到任何汉字**。
# 第 2 步因此不能用 `tr -d`——`（` 的第二个字节（0x88）在别的汉字里也出现，
# 按字节删会把那些汉字删坏。所以中文标点走 `sed` 的逐串替换，ASCII 标点
# 才走 `tr -d`。
#
# 这套规则在真仓库的 18 个锚点（41 个标题里被 TOC 点到的那一批）上逐条对得上
# ——自测的干净夹具钉着它：`决策接口（`/v1/systemone`）` 这一条带全角括号与
# 行内代码的标题就是为它准备的，规则一动那一份夹具就不再干净。它是**本脚本
# 自己的实现**，不是 GitHub 的实现：规则之外的形状（今天不存在）会被报成
# 不一致，而不是被放过——报红看得见，放过看不见。
#
# # 排序与比较用同一套 locale
#
# 脚本里所有外部命令都在 `LC_ALL=C`（字节语义）下跑，开工时 export 一次。
# 这不是洁癖：**`sort` 与 `comm` 必须对「有序」用同一个判断**，否则 comm 会
# 一边抱怨 `file 2 is not in sorted order`、一边把两份文件里**都有**的行当成
# 差异报出来。本机 locale 是 en_US.UTF-8 时就会这样——它按主级比较，`-` 与
# `.` 不参与，于是按字节排好的 `partitions-new.md / partitions.md` 被判成乱序，
# 而 `partitions.md` 明明两侧都有，却被报成「没进索引」：一道对着正确的索引
# 点名一份并不缺的文档的门（LESSONS.md 第 2 条那一类）。按字节比时两个文件名
# 的大小关系是稳定的，comm 与 sort 就此一致。自测第 19 条钉着这一条。
#
# # 已知边界（都是有意的）
#
# 1. **只判 docs/**。`contract/` 与 `lessons/` 各有各的现状，后者已经有一道门
#    （check-lessons-index.sh）。
# 2. **两条索引判据只看「本目录的 .md 链接」。** 目标里含 `/` 的
#    （`](../README.md)`、`](../contract/README.md)`）指的不是 docs/ 下的文件：
#    既不进判据 1 的对象，也不被判据 2 要求存在——**指向仓库别处的链接写错了，
#    这道门看不出来**。
# 3. **判据 3 只看 TOC 行。** 正文里的行内锚点链接（例如「见上面某一节」）不看。
# 4. **判据 3 不要求每个标题都进 TOC。** 判的是「TOC 里的锚点都在」，不是
#    「正文里的标题都在 TOC 里」——哪些标题该进 TOC 是取舍，机器判不了。
# 5. **`README.md` 自身不进判据 1 的对象。** 它是索引，不要求链回自己。
# 6. **不解析 Markdown 语法。** 判据 1 / 2 认的是「文本里有 `](…)` 这个形状」，
#    所以写在行内代码或代码块里的链接例子一样会被算成一条链接：它能让一份该进
#    索引的文档蒙混过关，也能让一个只出现在例子里的链接被判据 2 要求存在。
# 7. **文档内容对不对不是这道门的问题。** 它管的是「索引与正文对得上」。
#
# # 正向证据
#
# 这道门通过时是**沉默**的，而「什么都没输出」与「什么都没检查」长得一样
#（LESSONS.md 第 16 条）。所以它每次都打印一行证据：docs/ 下扫到几份 .md、
# 判据 1 要求几份、索引里解析出几条本目录链接、覆盖几份、TOC 锚点几个、
# 文中标题几个。任何一个数是 0 时它直接判「没跑成」（退出码 2），而不是
# 报通过——那时候它不是「干净」，是没扫。
#
# # 文件清单
#
# 用 `find` 枚举 `docs/*.md`，不是 `git ls-files`：这样**尚未提交的文件也在
# 扫描范围内**。只列已跟踪文件时，新写的文档在提交前不在门的范围里，而本机
# 自查「通过」会让人以为没问题（LESSONS.md 第 10 条）。
#
# # 用法
#
#   scripts/check-docs-index.sh
#
# 退出码：
#   0  三条判据都成立
#   1  有判据不成立（逐条列出是哪一条、哪一行 / 哪一份文件）
#   2  没跑成：docs/ 或两处索引之一不在、docs/ 下一个 *.md 都没有、
#      只有 README.md 一份（判据 1 没有可判的对象）、索引里一条本目录链接都
#      解析不出来、TOC 锚点或文中标题一个都解析不出来
#
# 1 与 2 必须分开：两者在 CI 里都是红的，但处置完全不同——前者要去改索引或
# 正文，后者说明这道门自己坏了（LESSONS.md 第 16 条）。
#
# # 环境变量
#
#   ROOT  仓库根，默认 `.`。给 scripts/check-docs-index.test.sh 指向夹具树用，
#         CI 与本地都不该设。
#
# # 自测
#
# scripts/check-docs-index.test.sh。改动本脚本后必须跑它：
# 一个把判据写反的扫描器，输出与「干净」完全一样。

set -uo pipefail
cd "$(dirname "$0")/.."

ROOT="${ROOT:-.}"

# 字节语义，全局一次：sort 与 comm 必须对「有序」用同一个判断（见开头
# 「排序与比较用同一套 locale」）。slug() 里仍逐条写着 LC_ALL=C——那几行的
# 字节安全是判据的一部分，写在用它的地方比继承一个全局设置更看得见。
#
# 这一行与自测第 19 条的分工：那一份夹具钉的是**两侧判断一致**——把 sort 的
# LC_ALL=C 单独写回来（最初写坏的样子），它就红。反过来，整行删掉时它不会红，
# 因为 sort 与 comm 会一起退回本机 locale、彼此仍然一致。所以这一行防的是
# 「下次再出现一侧按字节、一侧不是」，不是防它自己被删。
export LC_ALL=C

DIR="$ROOT/docs"
INDEX="$DIR/README.md"
TOC="$DIR/development.md"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# ── 输入 ──────────────────────────────────────────────────────────────────

[ -d "$DIR" ] || not_run "目录 $DIR 不存在。
文档目录不在时这道门什么都没检查，所以这里不当作通过。"

# 先枚举、再要求索引存在：两者都缺时报的是「一份 .md 都没有」——那是
# 「扫描根指错了」的样子，而它比「索引不存在」更接近真正的原因。反过来
# （先要求索引）时，这条枚举出口永远走不到，等于没有。
docs_all=$(find "$DIR" -maxdepth 1 -type f -name '*.md' -print 2>/dev/null | sort)
docs_count=$(printf '%s\n' "$docs_all" | grep -c . || true)
if [ "$docs_count" -eq 0 ]; then
  not_run "在 $DIR 下一个 *.md 都没扫到。
目录是空的、或者扫描根指错了时就是这样——那不是「干净」，是没扫。"
fi

# 判据 1 的对象：docs/ 下的 .md 去掉索引自身。
#
# 这一段排在「两份索引在不在」之前是有意的：`docs/` 下只剩一份索引时，
# 报「判据 1 没有对象」比报「development.md 不存在」更接近真正的原因
#（扫描根指到了一份别的树）。反过来排的话，这个出口永远走不到。
required="$tmpdir/required"
: >"$required"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ "$f" = "$INDEX" ] && continue
  printf '%s\n' "${f##*/}" >>"$required"
done <<<"$docs_all"
sort -o "$required" "$required"
required_count=$(grep -c . "$required" || true)
if [ "$required_count" -eq 0 ]; then
  not_run "$DIR 下除 $INDEX 之外一份 .md 都没有。
判据 1 问的是「每一份都在索引里」，没有对象时它什么都没判——而它的输出是
「0 份文档都在索引里」，那与「都索引到了」长得一样，所以这里不当作通过。"
fi

[ -f "$INDEX" ] || not_run "文档索引 $INDEX 不存在。
索引不在时判据 1 没有对象可判，所以这里不当作通过。"

[ -f "$TOC" ] || not_run "$TOC 不存在。
判据 3 判的是这一份顶部的 TOC，它不在时这道门什么都没检查。"

# ── 判据 1 与判据 2 的输入：docs/README.md 里指向本目录的 .md 链接 ───────
#
# 输出两列：<INDEX 里的行号>\t<文件名>。行号留着是为了报错时能指回具体哪一行。

linked="$tmpdir/linked"
: >"$linked"

RE_LINK='[]][(][^)]*[)]' # `](…)`：`[]]` 是「匹配 ]」的 POSIX 写法

lineno=0
while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line=${raw%$'\r'}

  # 一行里可能有多个链接，所以逐个取。取不到时内层循环一次都不进，不是错误：
  # 大多行本来就没有链接。
  while IFS= read -r target; do
    [ -n "$target" ] || continue
    file=${target%%#*} # `某文档.md#某节` 与 `某文档.md` 指同一份

    # 含 `/` 的目标不是本目录的文件（`../README.md`、`../contract/README.md`）。
    case "$file" in */*) continue ;; esac
    case "$file" in *.md) ;; *) continue ;; esac

    printf '%s\t%s\n' "$lineno" "$file" >>"$linked"
  done < <(printf '%s\n' "$line" | grep -oE "$RE_LINK" | sed -e 's/^[]](//' -e 's/)$//')
done <"$INDEX"

link_count=$(grep -c . "$linked" || true)
if [ "$link_count" -eq 0 ]; then
  not_run "$INDEX 里一条指向本目录 .md 的链接都没解析出来。
这条判据认的是「目标不含 / 且以 .md 结尾」的链接。一条都认不出时这道门
什么都没检查，而它的输出与「索引与正文一致」长得一样。"
fi

cut -f2 "$linked" | sort -u >"$tmpdir/covered"
covered_count=$(grep -c . "$tmpdir/covered" || true)
comm -13 "$tmpdir/covered" "$required" >"$tmpdir/missing"

# ── 判据 2：索引里的链接都有对应的文件 ────────────────────────────────────
#
# 逐条判、不比对集合：同一个文档被链两次时，两条链接都要各自落到一个存在的
# 文件上（这里用不上判据 1 那份去重后的 `covered`）。行号一并留着，报错时指回
# 具体是哪一行。

dangling="$tmpdir/dangling"
: >"$dangling"
while IFS=$'\t' read -r ll file; do
  [ -n "$file" ] || continue
  [ -f "$DIR/$file" ] && continue
  printf '%s\t%s\n' "$ll" "$file" >>"$dangling"
done <"$linked"
dangling_count=$(grep -c . "$dangling" || true)

# ── 判据 3：TOC 里的锚点在文中都有对应的标题 ──────────────────────────────

anchors="$tmpdir/anchors"
: >"$anchors"

lineno=0
while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line=${raw%$'\r'}

  # TOC 行 = 去掉行首空白后以 `- [` 打头、且含 `](#` 的行。
  t="${line#"${line%%[![:space:]]*}"}"
  case "$t" in "- ["*) ;; *) continue ;; esac
  case "$t" in *'](#'*) ;; *) continue ;; esac

  while IFS= read -r a; do
    [ -n "$a" ] || continue
    printf '%s\t%s\n' "$lineno" "$a" >>"$anchors"
  done < <(printf '%s\n' "$t" | grep -oE '[]][(]#[^)]*[)]' | sed -e 's/^[]](#//' -e 's/)$//')
done <"$TOC"

anchor_count=$(grep -c . "$anchors" || true)
if [ "$anchor_count" -eq 0 ]; then
  not_run "$TOC 里一个 TOC 行都没解析出来。
TOC 行的判据是「去掉行首空白后以 - [ 打头、且含 ](#」。一行都认不出时
这道门什么都没检查，而它的输出与「锚点都对得上」长得一样。"
fi

# ── 锚点函数 ──────────────────────────────────────────────────────────────
#
# 顺序与理由见文件开头「锚点是怎么算出来的」。两步删标点是分开的：ASCII 那一步
# 走 tr -d（只删 < 0x80 的字节，从字节上就碰不到汉字），中文标点走 sed 的
# 逐串替换（按字节删会删坏别的汉字）。

# ASCII 里要删掉的那些字节：控制字符与七段标点，留下 `-`(055) 与 `_`(137)。
# 写成八进制是 POSIX 的写法，各家的 tr 都认。
PUNCT_ASCII='\000-\037\041-\054\056-\057\072-\100\133-\136\140\173-\176\177'

# slug 从参数取一行标题，把它的锚点写到 stdout。
#
# 末尾那条单独的 printf 不是多余的：删除集里含 \012（换行，GitHub 也删它），
# 所以管道里的输入不能带换行，换行只能在管道之后补上。
slug() {
  printf '%s' "$1" \
    | LC_ALL=C tr 'A-Z' 'a-z' \
    | LC_ALL=C sed \
      -e 's/（//g' -e 's/）//g' -e 's/「//g' -e 's/」//g' \
      -e 's/『//g' -e 's/』//g' -e 's/、//g' -e 's/，//g' \
      -e 's/。//g' -e 's/；//g' -e 's/：//g' -e 's/？//g' \
      -e 's/！//g' -e 's/·//g' -e 's/—//g' -e 's/–//g' \
      -e 's/…//g' -e 's/《//g' -e 's/》//g' -e 's/〈//g' \
      -e 's/〉//g' -e 's/【//g' -e 's/】//g' -e 's/〔//g' \
      -e 's/〕//g' \
    | LC_ALL=C tr -d "$PUNCT_ASCII" \
    | LC_ALL=C tr ' ' '-'
  printf '\n'
}

# 文中标题（2~6 级）派生出的锚点。
slugs="$tmpdir/slugs"
: >"$slugs"
lineno=0
while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line=${raw%$'\r'}
  case "$line" in '#'*) ;; *) continue ;; esac
  if [[ "$line" =~ ^#{2,6}[[:space:]]+(.*)$ ]]; then
    slug "${BASH_REMATCH[1]}" >>"$slugs"
  fi
done <"$TOC"

heading_count=$(grep -c . "$slugs" || true)
if [ "$heading_count" -eq 0 ]; then
  not_run "$TOC 里一个标题都没解析出来。
判据 3 判的是「锚点都有对应的标题」，标题一个都没有时它什么都没判。"
fi

# ── 汇总 ──────────────────────────────────────────────────────────────────

bad=0
: >"$tmpdir/bad"

while IFS= read -r base; do
  [ -n "$base" ] || continue
  printf '判据 1 · %s/%s 在目录下，%s 里却链不到它\n' \
    "$DIR" "$base" "$INDEX" >>"$tmpdir/bad"
  bad=1
done <"$tmpdir/missing"

while IFS=$'\t' read -r ll file; do
  [ -n "$file" ] || continue
  printf '判据 2 · %s:%s 指向 %s/%s，而那个文件不存在\n' \
    "$INDEX" "$ll" "$DIR" "$file" >>"$tmpdir/bad"
  bad=1
done <"$dangling"

while IFS=$'\t' read -r al anchor; do
  [ -n "$anchor" ] || continue
  grep -qxF "$anchor" "$slugs" && continue
  printf '判据 3 · %s:%s 的锚点 #%s 在文中没有对应的标题\n' \
    "$TOC" "$al" "$anchor" >>"$tmpdir/bad"
  bad=1
done <"$anchors"

# ── 输出 ──────────────────────────────────────────────────────────────────
#
# 通过时也把正向证据打出来，理由见开头「正向证据」那一节。

echo "证据：docs/ 下 $docs_count 份 .md / 判据 1 要求 $required_count 份 / 索引里 $link_count 条本目录链接（其中 $dangling_count 条指向不存在的文档）、覆盖 $covered_count 份 / TOC 锚点 $anchor_count 个 / 文中标题 $heading_count 个（2~6 级都算）"
echo

if [ "$bad" -ne 0 ]; then
  echo "✗ docs/ 的两处索引与正文不一致（判据见本脚本开头）："
  sed 's/^/   /' "$tmpdir/bad"
  echo
  echo "   文中标题派生出的锚点（共 $heading_count 个）："
  sed 's/^/     /' "$slugs"
  echo
  echo "docs/ 的两处索引与正文不一致。"
  exit 1
fi

echo "三条判据都成立：$required_count 份文档都在索引里，索引里 $link_count 条本目录链接都指向存在的文档，$anchor_count 个锚点都有对应的标题。"
echo "门禁通过。"
