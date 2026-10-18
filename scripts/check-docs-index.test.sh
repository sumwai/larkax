#!/usr/bin/env bash
# check-docs-index.sh 的自测。
#
# 为什么需要它：这道门的成功信号是**沉默**——两处索引与正文对得上就不输出，
# 于是「判据写反了 / 一个链接都没解析出来 / 一条标题都没扫到」与「干净」在
# 屏幕上完全一样，退出码也一样（LESSONS.md 第 16 条；第 2 条是它的第一次
# 现身：检查脚本自己报「通过」，实际根本没在检查）。第 23 条把结论写死了：
# **每一个守卫都要有一条分辨得出来的用例**，退出码相同不等于走的是同一条分支。
#
# 所以下面正反两面都钉：三条判据各自的不一致都要失败、六个「没跑成」的出口
# 都要走第 2 档、以及几种「看着像不一致、其实不是」的排版不失败。
#
# **本自测跑的是真脚本**（不是副本），用脚本自己声明的 ROOT 指向夹具树——
# 判据、锚点函数与枚举那三段都在被测的那一份代码里，桩掉它等于什么都没测。
# 夹具全在 $tmp 下，不碰真仓库的工作树。
#
# 改动 check-docs-index.sh 之后跑：
#   bash scripts/check-docs-index.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-docs-index.sh"
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

# ── 夹具：三条判据都成立的那一份 ──────────────────────────────────────────
#
# 它同时钉住五件「看着像不一致、其实不是」的事：
#
#   - 索引表格**前后都有段落**（表格不是文件里唯一的行）；
#   - 索引里有**指向别处**的链接（`../README.md#quickstart`、`../contract/README.md`）：
#     目标里含 `/`，它们既不算「本目录的链接」，也不被要求存在；
#   - 链接**带锚点**（`development.md#子节`）时与不带锚点的是同一份；
#   - 标题里的**全角括号、行内代码、斜杠、顿号、大写 ASCII** 全部要算对
#     （`决策接口（`/v1/systemone`）` → `决策接口v1systemone`）；
#   - TOC 里有一行**普通列表项**（没有 `](#`）：它不算 TOC 行，也不是错误。
#
# 另外它故意**链了两次 development.md**：正向证据里「条数」与「覆盖的份数」
# 是两个数，只报一个的话这类重复看不见。

mk_clean() {
  local root="$1"
  mkdir -p "$root/docs" "$root/contract"

  printf '# 仓库首页\n\n## Quickstart\n\n正文\n' >"$root/README.md"
  printf '# 契约测试\n\n正文\n' >"$root/contract/README.md"

  printf '# alpha\n\n正文\n' >"$root/docs/alpha.md"
  printf '# beta guide\n\n正文\n' >"$root/docs/beta-guide.md"

  cat >"$root/docs/README.md" <<'EOF'
# 文档索引

这一段链到 [仓库首页](../README.md#quickstart) 与 [契约测试](../contract/README.md)，
两者都不在 docs/ 下。

## 上手

| 想做的事 | 看哪份 |
|---|---|
| 甲 | [alpha.md](alpha.md) |
| 乙 | [beta-guide.md](beta-guide.md) |
| 丙 | [development.md 的某节](development.md#子节) 与 [development.md](development.md) |
EOF

  cat >"$root/docs/development.md" <<'EOF'
# 本地开发环境

本文件很长，按下面的目录找。

- [开始](#开始)
- [决策接口（`/v1/systemone`）](#决策接口v1systemone)
- [造用户、造 key、打一次请求](#造用户造-key打一次请求)
- [CI 跑什么](#ci-跑什么)
- [下划线与连字符](#保留下划线_a-与连字符-b)
- [子节](#子节)
- 普通列表项，没有锚点

## 开始

正文。

## 决策接口（`/v1/systemone`）

正文。

## 造用户、造 key、打一次请求

正文。

## CI 跑什么

正文。

## 保留下划线_a 与连字符-b

正文。

### 子节

正文。
EOF
}

# ── 1. 干净夹具：退出码 0 ─────────────────────────────────────────────────

clean="$tmp/clean"
mk_clean "$clean"

run "$clean"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "干净夹具 —— 应通过（退出码 0），实际 $rc"; fi
grep -q '门禁通过。' "$last_out" || bad "干净夹具 —— 要通过时明说通过"
grep -q '✗' "$last_out" && bad "干净夹具 —— 不该有任何命中" || ok

# ── 2. 正向证据：七个数的每一个都要对 ─────────────────────────────────────
#
# 「什么都没输出」与「什么都没检查」长得一样，所以这行证据必须真的报出数来。
# 这一个具体数字是关键——它一次钉住五件事：
#
#   - docs/ 下 4 份（`find` 枚举到了，且只数本层的 .md）；
#   - 判据 1 要求 3 份（`README.md` 自身被排除）；
#   - 索引里 4 条本目录链接（`../` 那两条被排除）、覆盖 3 份（重复的算一份）；
#   - 其中 0 条指向不存在的文档（判据 2 真的逐条去要了文件）；
#   - TOC 锚点 6 个（普通列表项不算）、文中标题 6 个（`###` 那一级也算）。
#
# 任何一处判据写反（比如把 `../README.md` 算成本目录链接、或者只认 `##`
# 不认 `###`），这里都会变。

grep -qE '^证据：docs/ 下 [1-9][0-9]* 份 \.md / 判据 1 要求 [1-9][0-9]* 份 / 索引里 [1-9][0-9]* 条本目录链接（其中 [0-9]+ 条指向不存在的文档）、覆盖 [1-9][0-9]* 份 / TOC 锚点 [1-9][0-9]* 个 / 文中标题 [1-9][0-9]* 个（2~6 级都算）$' "$last_out" ||
  bad "正向证据 —— 要通过时也要报出七个计数（除「指向不存在的文档」外都不该是 0）"
grep -q '^证据：docs/ 下 4 份 \.md / 判据 1 要求 3 份 / 索引里 4 条本目录链接（其中 0 条指向不存在的文档）、覆盖 3 份 / TOC 锚点 6 个 / 文中标题 6 个（2~6 级都算）$' "$last_out" ||
  bad "正向证据 —— 数要与夹具对得上：README.md 不进判据 1、../ 的链接不算本目录、重复的算一份、### 也算标题"

# ── 3. 判据 1：有文件、索引里没有 ─────────────────────────────────────────

unlisted="$tmp/unlisted"
mk_clean "$unlisted"
printf '# gamma\n\n正文\n' >"$unlisted/docs/gamma.md"

run "$unlisted"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 1 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 1 ·' "$last_out" || bad "判据 1 —— 要指出是哪一条判据"
grep -q 'docs/gamma\.md 在目录下' "$last_out" || bad "判据 1 —— 要报出那份没进索引的文件"
# 反方向不能被顺带报出来：gamma.md 只是没进索引，索引里没有一条指向它。
if grep -q '判据 2 ·' "$last_out"; then
  bad "判据 1 —— 索引里没有指向 gamma.md 的链接，判据 2 不该报"
else
  ok
fi

# ── 4. 判据 1 的边界：链接目标带锚点也算链到了 ─────────────────────────────
#
# 索引里只用 `development.md#子节` 链一次时，`development.md` 仍算「在索引里」。
# 把锚点当成文件名的一部分时，这一条会红——而它是把正确的索引判成错的。

anchoronly="$tmp/anchoronly"
mk_clean "$anchoronly"
cat >"$anchoronly/docs/README.md" <<'EOF'
# 文档索引

| 想做的事 | 看哪份 |
|---|---|
| 甲 | [alpha.md](alpha.md) |
| 乙 | [beta-guide.md](beta-guide.md) |
| 丙 | [development.md 的某节](development.md#子节) |
EOF

run "$anchoronly"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "带锚点的链接 —— 目标仍是 development.md，应通过，实际 $rc"; fi
grep -q '^证据：docs/ 下 4 份 \.md / 判据 1 要求 3 份 / 索引里 3 条本目录链接（其中 0 条指向不存在的文档）、覆盖 3 份 / TOC 锚点 6 个 / 文中标题 6 个（2~6 级都算）$' "$last_out" ||
  bad "带锚点的链接 —— 条数应变成 3、覆盖仍是 3"

# ── 5. 判据 2：索引里链了一份不存在的文档 ─────────────────────────────────
#
# 判据 1 对这一条**无感**：那份文档在目录下不存在，所以「每一份都在索引里」
# 完全成立。这就是两条判据必须成对的原因——夹具同时断言判据 1 不报。

nowhere="$tmp/nowhere"
mk_clean "$nowhere"
cat >>"$nowhere/docs/README.md" <<'EOF'
| 丁 | [gamma.md](gamma.md) |
EOF

run "$nowhere"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 2（链向不存在的文档）—— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 2 ·' "$last_out" || bad "判据 2（链向不存在的文档）—— 要指出是哪一条判据"
grep -q 'docs/README\.md:[0-9]* 指向 .*docs/gamma\.md，而那个文件不存在' "$last_out" ||
  bad "判据 2（链向不存在的文档）—— 要报出那份不存在的文件，并指回索引里的行号"
if grep -q '判据 1 ·' "$last_out"; then
  bad "判据 2（链向不存在的文档）—— 目录下没有 gamma.md，判据 1 不该报"
else
  ok
fi

# ── 6. 判据 2 的边界：指向别处的链接不被要求存在 ──────────────────────────
#
# `../` 的目标不是 docs/ 下的文件，写去什么地方都不归这道门管——判定
# 「文件存在吗」时若忘了那条 `/` 过滤，这里会拿 `$DIR/../nowhere.md` 去要文件，
# 于是对着一个正确的索引报红。夹具里那份 `../nowhere.md` 确实不存在。

outside="$tmp/outside"
mk_clean "$outside"
cat >"$outside/docs/README.md" <<'EOF'
# 文档索引

第一段链到 [仓库首页](../nowhere.md)（它不在 docs/ 下，也不存在）。

| 想做的事 | 看哪份 |
|---|---|
| 甲 | [alpha.md](alpha.md) |
| 乙 | [beta-guide.md](beta-guide.md) |
| 丙 | [development.md](development.md) |
EOF

run "$outside"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "别处的链接 —— 含 / 的目标不归这道门管，应通过，实际 $rc"; fi
grep -q '^证据：docs/ 下 4 份 \.md / 判据 1 要求 3 份 / 索引里 3 条本目录链接（其中 0 条指向不存在的文档）、覆盖 3 份 / TOC 锚点 6 个 / 文中标题 6 个（2~6 级都算）$' "$last_out" ||
  bad "别处的链接 —— 它既不算本目录链接，也不该被判成指向不存在的文档"

# ── 7. 判据 3：TOC 里的锚点在文中没有对应的标题 ───────────────────────────

drift="$tmp/drift"
mk_clean "$drift"
# 改标题、不动 TOC：锚点于是变死链（这正是「改了标题忘了跟 TOC」的样子）。
sed -i 's/^## 开始$/## 起步/' "$drift/docs/development.md"

run "$drift"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 3（标题改了）—— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 3 ·' "$last_out" || bad "判据 3（标题改了）—— 要指出是哪一条判据"
grep -q '的锚点 #开始 在文中没有对应的标题' "$last_out" ||
  bad "判据 3（标题改了）—— 要报出那个死锚点"
grep -q 'docs/development\.md:[0-9]*' "$last_out" ||
  bad "判据 3（标题改了）—— 要指回 TOC 里的行号"
if grep -q '判据 [12] ·' "$last_out"; then
  bad "判据 3（标题改了）—— 这个夹具没动索引，索引那两条判据都不该报"
else
  ok
fi

# ── 8. 判据 3 的另一面：TOC 里多一条指向不存在标题的锚点 ──────────────────

extraanchor="$tmp/extraanchor"
mk_clean "$extraanchor"
printf -- '- [不存在的节](#不存在的节)\n' >>"$extraanchor/docs/development.md"

run "$extraanchor"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 3（多一条锚点）—— 应失败（退出码 1），实际 $rc"; fi
grep -q '的锚点 #不存在的节 在文中没有对应的标题' "$last_out" ||
  bad "判据 3（多一条锚点）—— 要报出那个锚点"

# ── 9. TOC 行缩进时照样认得出 ─────────────────────────────────────────────
#
# TOC 的判据是「去掉行首空白后以 - [ 打头」，所以嵌在列表里的 TOC 行也算。
# 不剥行首空白时锚点数会掉到 0，那就是一次静默的假保证。

indent="$tmp/indent"
mk_clean "$indent"
sed -i 's/^- /  - /' "$indent/docs/development.md"

run "$indent"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "TOC 行缩进 —— 应通过（退出码 0），实际 $rc"; fi
grep -q '^证据：docs/ 下 4 份 \.md / 判据 1 要求 3 份 / 索引里 4 条本目录链接（其中 0 条指向不存在的文档）、覆盖 3 份 / TOC 锚点 6 个 / 文中标题 6 个（2~6 级都算）$' "$last_out" ||
  bad "TOC 行缩进 —— 锚点数应仍是 6"

# ── 10. docs/ 目录不存在：退出码 2 ────────────────────────────────────────

nodir="$tmp/nodir"
mkdir -p "$nodir"

run "$nodir"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "docs/ 不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'docs 不存在' "$last_out" || bad "docs/ 不存在 —— 要说清是这个原因"

# ── 11. docs/ 下一个 *.md 都没有：退出码 2 ────────────────────────────────
#
# 枚举在「索引存在」之前，所以这一条真的走到了那个出口。反过来排的时候它
# 永远走不到（索引与枚举看的是同一批文件），那等于这个守卫不存在。

emptydir="$tmp/emptydir"
mkdir -p "$emptydir/docs"

run "$emptydir"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "docs/ 下没有 .md —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个 \*\.md 都没扫到' "$last_out" || bad "docs/ 下没有 .md —— 要说清是这个原因"

# ── 12. docs/README.md 不存在：退出码 2 ───────────────────────────────────

noindex="$tmp/noindex"
mk_clean "$noindex"
rm "$noindex/docs/README.md"

run "$noindex"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "索引不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '文档索引' "$last_out" || bad "索引不存在 —— 要说清是这个原因"

# ── 13. docs/development.md 不存在：退出码 2 ──────────────────────────────

notoc="$tmp/notoc"
mk_clean "$notoc"
rm "$notoc/docs/development.md"

run "$notoc"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "TOC 那份不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi

# ── 14. 索引里只有指向别处的链接：退出码 2 ────────────────────────────────
#
# 这一条把「含 / 的目标不算本目录链接」这条过滤钉在**没跑成**那一侧：
# 过滤写坏（`../README.md` 被算成本目录链接）时，链接条数不再是 0，
# 这道门就会对着一个什么都没索引的索引表报通过。

onlyoutside="$tmp/onlyoutside"
mk_clean "$onlyoutside"
cat >"$onlyoutside/docs/README.md" <<'EOF'
# 文档索引

只链到 [仓库首页](../README.md) 与 [契约测试](../contract/README.md)。
EOF

run "$onlyoutside"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "索引里只有别处的链接 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一条指向本目录 \.md 的链接都没解析出来' "$last_out" ||
  bad "索引里只有别处的链接 —— 要说清是这个原因"

# ── 15. docs/ 下只有索引自己：退出码 2 ────────────────────────────────────
#
# 判据 1 问的是「每一份都在索引里」，一份对象都没有时它什么都没判——
# 那时候它的输出是「0 份文档都在索引里」，而那与「都索引到了」长得一样。
# 所以这里要退出码 2，不是 0。
#
# 夹具里 `docs/` 下**只有**索引这一份文件，且索引链的是**它自己**：这样链接
# 条数不是 0，走的是「对象为空」这个出口，而不是上面那个
# 「一条链接都没解析出来」。

onlyindex="$tmp/onlyindex"
mkdir -p "$onlyindex/docs"
cat >"$onlyindex/docs/README.md" <<'EOF'
# 文档索引

| 想做的事 | 看哪份 |
|---|---|
| 索引自己 | [README.md](README.md) |
EOF

run "$onlyindex"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "只有索引自己 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一份 \.md 都没有' "$last_out" || bad "只有索引自己 —— 要说清是这个原因"

# ── 16. development.md 里没有 TOC 行：退出码 2 ────────────────────────────

notocline="$tmp/notocline"
mk_clean "$notocline"
sed -i '/^- \[/d' "$notocline/docs/development.md"

run "$notocline"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "没有 TOC 行 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个 TOC 行都没解析出来' "$last_out" || bad "没有 TOC 行 —— 要说清是这个原因"

# ── 17. development.md 里没有标题：退出码 2 ───────────────────────────────

noheads="$tmp/noheads"
mk_clean "$noheads"
sed -i '/^#\{2,\} /d' "$noheads/docs/development.md"

run "$noheads"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "没有标题 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个标题都没解析出来' "$last_out" || bad "没有标题 —— 要说清是这个原因"

# ── 18. 判据之间不互相顶替 ────────────────────────────────────────────────
#
# 三条判据都是**逐个**判的：一份文档没进索引，不能因为另一份进了就放过；
# 一个链接指向不存在的文件，不能因为别的链接都好就放过；一个锚点是死链，
# 不能因为别的锚点活着就放过。夹具里同时制造三处不一致，三处都要报出来——
# 只报一处的话，读的人会以为改一个地方就够了。

both="$tmp/both"
mk_clean "$both"
printf '# gamma\n\n正文\n' >"$both/docs/gamma.md"
cat >>"$both/docs/README.md" <<'EOF'
| 丁 | [delta.md](delta.md) |
EOF
sed -i 's/^## 开始$/## 起步/' "$both/docs/development.md"

run "$both"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "三处都不一致 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'docs/gamma\.md 在目录下' "$last_out" || bad "三处都不一致 —— 判据 1 要报出来"
grep -q 'docs/delta\.md，而那个文件不存在' "$last_out" || bad "三处都不一致 —— 判据 2 也要报出来"
grep -q '的锚点 #开始 在文中没有对应的标题' "$last_out" ||
  bad "三处都不一致 —— 判据 3 也要报出来"

# ── 19. sort 与 comm 用的是同一套排序 ─────────────────────────────────────
#
# `sort` 与 `comm` 对「有序」的判断必须一致，否则 comm 会把**两侧都有**的行
# 报成差异——于是一道门对着一个正确的索引，点名一份根本不缺的文档。本机
# locale（en_US.UTF-8）按主级比较、`-` 与 `.` 不参与，所以按字节排好的
# `alpha-new.md / alpha.md` 被判成乱序：缺的是 `alpha-new.md`，被顺带报出来的
# 却是索引里明明有的 `alpha.md`。
#
# 三条断言一起钉：缺的那一份要报、不缺的那一份不许报、comm 本身不许抱怨乱序。

locale_fixture="$tmp/locale"
mk_clean "$locale_fixture"
printf '# alpha new\n\n正文\n' >"$locale_fixture/docs/alpha-new.md"

run "$locale_fixture"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "排序用同一套 locale —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'docs/alpha-new\.md 在目录下' "$last_out" ||
  bad "排序用同一套 locale —— 要报出真的缺的那一份"
if grep -q 'docs/alpha\.md 在目录下' "$last_out"; then
  bad "排序用同一套 locale —— alpha.md 在索引里，不该被报成没进索引（sort 与 comm 的排序判断不一致时就会这样）"
else
  ok
fi
if grep -q 'not in sorted order' "$last_out"; then
  bad "排序用同一套 locale —— comm 不该抱怨输入乱序（两侧的 LC_ALL 必须一致）"
else
  ok
fi

# ── 20. 真仓库：这道门在它自己身上是干净的 ────────────────────────────────
#
# 前十九条用的是夹具。这一条跑真仓库，钉住「落地时确实是对得上的」——
# 顺带证明夹具之外的真实文件树也能被扫（文件名里有连字符、TOC 里有
# 全角括号与行内代码的行）。

ROOT="$PWD" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "真仓库 —— 应通过（退出码 0），实际 $rc"; fi
repo_docs=$(grep -oE 'docs/ 下 [0-9]+ 份' "$last_out" | grep -oE '[0-9]+')
repo_anchors=$(grep -oE 'TOC 锚点 [0-9]+ 个' "$last_out" | grep -oE '[0-9]+')
if [ "${repo_docs:-0}" -ge 10 ]; then ok; else
  bad "真仓库 —— 只扫到 ${repo_docs:-0} 份文档，枚举范围像是写错了"
fi
if [ "${repo_anchors:-0}" -ge 10 ]; then ok; else
  bad "真仓库 —— 只解析出 ${repo_anchors:-0} 个锚点，TOC 行的判据像是写错了"
fi

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
