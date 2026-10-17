#!/usr/bin/env bash
# 「LESSONS.md 索引与 lessons/ 正文一致」的门禁。
#
# # 为什么需要它
#
# LESSONS.md 是索引，正文一条一个文件放在 lessons/ 下（见 AGENTS.md「其他文档在哪」）。
# 两者的一致性原先只靠人手工验——而它恰好是本项目反复出现的那种形状：
# **有实现、没有门**（LESSONS.md 第 22 条）。四条判据里任何一条不成立，
# 表现都是「索引里点进去是 404」，或者更坏：**点进去是另一条**（第 4 条）。
# 而这两种在 CI 里都不会有任何一步失败。
#
# # 判据（四条）
#
#   1. 索引表每一行的链接目标，在 lessons/ 下都存在；
#   2. lessons/ 下每一个 *.md，在索引里都有一行；
#   3. 索引表的行数 == lessons/ 下的文件数；
#   4. lessons/NN-slug.md 的**文件名前缀编号** == 文件首行 `# NN · 主题` 里的编号。
#
# **第 3 条不是第 1、2 条的重复。** 1 与 2 比的是**集合**，而集合看不出重复：
# 索引里把同一行写两遍、指向同一个文件时，两侧的集合仍然相等，只是索引凭空
# 多了一行——那正是第 3 条要抓的。行数同时是这道门的**正向证据**（见下），
# 所以它每一轮都会被打印出来。
#
# # 索引行是怎么认出来的
#
# 一行算不算索引行，看的是**首个单元格**里有没有指向 `lessons/*.md` 的链接：
#
#   | [12](lessons/12-shape-allowlist-false-positives.md) | 形态匹配的白名单会被代码反复误报 | … | … |
#
# 所以改列宽、改列序、给某个单元格换措辞、在表格前后加段落，都不会让它认不出来。
#
# 「只看首格」与「一行只取第一个链接」是两条**各自挡一类误判**的约定：
#
#   - **一行只取第一个链接**：本仓索引表的「状态」列里就有一处交叉引用
#     `已被[第 13 条](lessons/13-scope-by-file-type.md)取代`。按「一行里出现过
#     几个链接」来数，那一行会被数成两行，于是第 3 条对着一个正确的索引表
#     天天报红——而**天天红的门，第二次就没人看了**。
#   - **只看首格**：另一张表格里，首格是说明文字、后面的格子里引用了某条 lessons 时，
#     那一行不该被当成索引行。放宽成「整行里找链接」的话它会被算成一行，
#     而它引用的那一条于是**蒙混过关**（本来该由第 2 条报出来）。
#
# 两条各有一条自测钉着（自测第 2 条钉前者，第 15 条钉后者）。
#
# 编号的比较走**数值**：索引里显示 `[1]`，文件名是 `01-…`，首行是 `# 1 · …`——
# 三处写法不同、编号相同，所以 `01` 与 `1` 必须判成同一个。
#
# # 已知边界（都是有意的）
#
# 1. **索引链接的显示文字不比。** 判的是「链接目标」「文件名前缀」「文件首行」三者，
#    `[13](lessons/13-…md)` 里那个 `13` 与目标文件是否对得上不在判据里。
#    今天每一条都对得上；留着这段说明是给下一个人的——它是**有意的**，不是漏了。
# 2. **表格以外的 `lessons/` 引用不算索引行。** 正文里的「见第 13 条」这类交叉引用、
#    以及 `[lessons/](lessons/)` 这种目录链接都不在判据里（理由见上一节）。
# 3. **`lessons/` 的子目录不看。** 只枚举本层的 `*.md`；子目录里的文件既不会被
#    要求进索引，也不会被算进文件数。
# 4. **文件内容对不对不是这道门的问题。** 它管的是「索引与正文对得上」，
#    不管正文写了什么。
# 5. **索引表以外的说明文字不看。** 索引前面那几段（编号是稳定标识符、格式约定、
#    失效条目怎么标注）都不在判据里。
#
# # 正向证据
#
# 这道门通过时是**沉默**的，而「什么都没输出」与「什么都没检查」长得一样
#（LESSONS.md 第 16 条）。所以它每次都打印**解析出多少行索引、扫到多少个正文文件**
#——两个数都是 0 时它直接判「没跑成」（退出码 2），而不是报通过。
# CI 里那一步会检查这行证据确实出现过（见 .github/workflows/ci.yml）。
#
# # 文件清单
#
# 用 `find` 枚举 `lessons/*.md`，不是 `git ls-files`：这样**尚未提交的文件也在
# 扫描范围内**。只列已跟踪文件时，新写的条目在提交前不在门的范围里，而本机自查
# 「通过」会让人以为没问题（LESSONS.md 第 10 条）。
#
# # 用法
#
#   scripts/check-lessons-index.sh
#
# 退出码：
#   0  四条判据都成立
#   1  有判据不成立（逐条列出是哪一条、哪一行 / 哪个文件）
#   2  没跑成：LESSONS.md 不在、lessons/ 不在、lessons/ 下一个 *.md 都没有、
#      索引表一行都没解析出来、某个索引行或某个文件的编号解析不出来
#
# 1 与 2 必须分开：两者在 CI 里都是红的，但处置完全不同——前者要去改索引或正文，
# 后者说明这道门自己坏了（LESSONS.md 第 16 条）。
#
# # 环境变量
#
#   ROOT  仓库根，默认 `.`。给 scripts/check-lessons-index.test.sh 指向夹具树用，
#         CI 与本地都不该设。
#
# # 自测
#
# scripts/check-lessons-index.test.sh。改动本脚本后必须跑它：
# 一个把判据写反的扫描器，输出与「干净」完全一样。

set -uo pipefail
cd "$(dirname "$0")/.."

ROOT="${ROOT:-.}"

INDEX="$ROOT/LESSONS.md"
DIR="$ROOT/lessons"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# ── 输入 ──────────────────────────────────────────────────────────────────

[ -f "$INDEX" ] || not_run "索引 $INDEX 不存在。
索引不在时这道门什么都没检查，所以这里不当作通过。"

[ -d "$DIR" ] || not_run "目录 $DIR 不存在。
正文目录不在时，索引里的每一行都会指向一个不存在的文件——那不是「索引写错了」，
是扫描根不对，所以这里不当作通过。"

files=$(find "$DIR" -maxdepth 1 -type f -name '*.md' -print 2>/dev/null | LC_ALL=C sort)
file_count=$(printf '%s\n' "$files" | grep -c . || true)
if [ "$file_count" -eq 0 ]; then
  not_run "在 $DIR 下一个 *.md 都没扫到。
目录存在却是空的、或者扫描根写错时就是这样——那不是「干净」，是没扫。"
fi

# ── 解析索引表 ────────────────────────────────────────────────────────────
#
# 输出两列：<LESSONS.md 里的行号>\t<链接目标的文件名>。
# 行号留着是为了报错时能指回具体哪一行。

# 索引行首格里的链接：`[<任意文字>](lessons/<文件名>.md)`。
# 放变量而不是内联，理由见下面用到它的那一处。
RE_LINK='\[[^]]*\][(](lessons/[^)]+)[)]'

rows="$tmpdir/rows"
: >"$rows"
lineno=0
while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line=${raw%$'\r'}

  # 去掉行首空白后必须是表格行（`|` 打头）。段落、标题、分隔线都从这里出去。
  t="${line#"${line%%[![:space:]]*}"}"
  case "$t" in "|"*) ;; *) continue ;; esac

  # 首个单元格 = 第一个 `|` 之后、第二个 `|` 之前。
  rest=${t#|}
  cell=${rest%%|*}

  # 首格里没有指向 lessons/ 的链接 → 不是索引行（表头行、`|---|` 分隔行、别的表格）。
  case "$cell" in *'](lessons/'*) ;; *) continue ;; esac

  # 取链接目标。首格里只取**第一个**链接——理由见开头「索引行是怎么认出来的」。
  #
  # 括号写成 [(] / [)] 而不是 \( / \)：ERE 里 `\(` 是未定义行为，各家实现不一样。
  # 正则放变量、不内联，也是必须的：`[^]]` 这种「反括号类里含 `]`」的写法会让
  # bash 的 `[[ … ]]` **解析器**（不是正则引擎）在这里报
  # `syntax error in conditional expression`，而它报错的那一行正是判据本身。
  # 放进变量就绕开了那个解析路径（bash 5.2 实测）。
  if [[ "$cell" =~ $RE_LINK ]]; then
    target="${BASH_REMATCH[1]}"
  else
    not_run "$INDEX:$lineno 的首格里有 lessons/ 链接，却解析不出目标：
   $cell"
  fi

  # 只认 lessons/ 本层的 .md，形如 lessons/01-slug.md。
  # 认不出时判「没跑成」而不是「不一致」——这个链接的形状这道门还不认识，
  # 而「不认识」与「都对上了」在输出上分不出来（LESSONS.md 第 16 条）。
  if [[ "$target" =~ ^lessons/([^/]+[.]md)$ ]]; then
    base="${BASH_REMATCH[1]}"
  else
    not_run "$INDEX:$lineno 的链接目标不是 lessons/ 本层的 .md：
   $target
这道门只认「lessons/<文件名>.md」这一种形状。"
  fi

  printf '%s\t%s\n' "$lineno" "$base" >>"$rows"
done <"$INDEX"

row_count=$(grep -c . "$rows" || true)
if [ "$row_count" -eq 0 ]; then
  not_run "$INDEX 里一行索引都没解析出来。
索引行的判据是「首个单元格里有指向 lessons/*.md 的链接」。一行都认不出时，
这道门什么都没检查，而它的输出与「索引与正文一致」长得一样。"
fi

# ── 判据 1：索引行的目标文件存在 ──────────────────────────────────────────

bad=0
: >"$tmpdir/bad"

while IFS=$'\t' read -r rl base; do
  [ -n "$base" ] || continue
  if [ ! -f "$DIR/$base" ]; then
    printf '判据 1 · %s:%s 指向 lessons/%s，而那个文件不存在\n' \
      "$INDEX" "$rl" "$base" >>"$tmpdir/bad"
    bad=1
  fi
done <"$rows"

# ── 判据 2：lessons/ 下的每个文件在索引里都有一行 ────────────────────────

index_names="$tmpdir/index_names" # 按索引行，含重复
index_uniq="$tmpdir/index_uniq"
file_uniq="$tmpdir/file_uniq"

cut -f2 "$rows" | LC_ALL=C sort >"$index_names"
LC_ALL=C sort -u -o "$index_uniq" "$index_names"

: >"$file_uniq"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  printf '%s\n' "${f##*/}" >>"$file_uniq"
done <<<"$files"
LC_ALL=C sort -u -o "$file_uniq" "$file_uniq"

comm -13 "$index_uniq" "$file_uniq" >"$tmpdir/unlisted"
while IFS= read -r b; do
  [ -n "$b" ] || continue
  printf '判据 2 · lessons/%s 在索引里没有一行\n' "$b" >>"$tmpdir/bad"
  bad=1
done <"$tmpdir/unlisted"

# ── 判据 3：索引行数 == 正文文件数 ────────────────────────────────────────
#
# 1 与 2 比的是集合，集合看不出重复；这一条把那一类补上。

if [ "$row_count" -ne "$file_count" ]; then
  printf '判据 3 · 索引 %s 行，正文 %s 个文件，数目不等\n' \
    "$row_count" "$file_count" >>"$tmpdir/bad"
  dups=$(uniq -d "$index_names")
  if [ -n "$dups" ]; then
    while IFS= read -r d; do
      [ -n "$d" ] || continue
      printf '         └ 索引里 lessons/%s 被写了不止一行\n' "$d" >>"$tmpdir/bad"
    done <<<"$dups"
  fi
  bad=1
fi

# ── 判据 4：文件名前缀编号 == 首行编号 ────────────────────────────────────
#
# 四条里最要紧的一条：编号与文件名脱钩时，读者点进去看到的是**另一条**，
# 而那种错误不会让任何人停下来（404 会）。

while IFS= read -r f; do
  [ -n "$f" ] || continue
  base="${f##*/}"

  if [[ "$base" =~ ^([0-9]+)- ]]; then
    fnum="${BASH_REMATCH[1]}"
  else
    not_run "lessons/$base 的文件名里认不出编号。
约定是「NN-slug.md」，这道门靠这个前缀与首行的编号比对；认不出时它一个编号
都没在判，所以这里不当作通过。"
  fi

  head1=$(head -n1 "$f")
  head1=${head1%$'\r'}
  if [[ "$head1" =~ ^#[[:space:]]*([0-9]+)[[:space:]]*· ]]; then
    hnum="${BASH_REMATCH[1]}"
  else
    not_run "lessons/$base 的首行里认不出编号：
   $head1
约定是「# NN · 主题」。认不出时这一条的编号没在判，所以这里不当作通过。"
  fi

  # 10# 前缀：按十进制读，免得 08 被当成八进制。
  if [ "$((10#$fnum))" -ne "$((10#$hnum))" ]; then
    printf '判据 4 · lessons/%s 的文件名编号是 %s，首行写的是 %s\n' \
      "$base" "$fnum" "$hnum" >>"$tmpdir/bad"
    bad=1
  fi
done <<<"$files"

# ── 输出 ──────────────────────────────────────────────────────────────────
#
# 通过时也把正向证据打出来，理由见开头「正向证据」那一节。

echo "索引表解析出 $row_count 行 / lessons/ 下 $file_count 个 .md 文件"
echo

if [ "$bad" -ne 0 ]; then
  echo "✗ LESSONS.md 索引与 lessons/ 正文不一致（判据见本脚本开头）："
  sed 's/^/   /' "$tmpdir/bad"
  echo
  echo "LESSONS.md 索引与 lessons/ 正文不一致。"
  exit 1
fi

echo "四条判据都成立：索引 $row_count 行 ↔ 正文 $file_count 个文件，编号逐条对上。"
echo "门禁通过。"
