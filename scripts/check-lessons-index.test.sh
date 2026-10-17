#!/usr/bin/env bash
# check-lessons-index.sh 的自测。
#
# 为什么需要它：这道门的成功信号是**沉默**——索引与正文对得上就不输出，于是
# 「判据写反了 / 一行索引都没解析出来 / 文件根本没枚举到」与「干净」在屏幕上
# 完全一样，退出码也一样（LESSONS.md 第 16 条；第 2 条是它的第一次现身：
# 检查脚本自己报「通过」，实际根本没在检查）。第 23 条把结论写死了：
# **每一个守卫都要有一条分辨得出来的用例**，退出码相同不等于走的是同一条分支。
#
# 所以下面正反两面都钉：四条判据各自的不一致都要失败、三个「没跑成」的出口
# 都要走第 2 档、以及几种「看着像不一致、其实不是」的排版不失败。
#
# **本自测跑的是真脚本**（不是副本），用脚本自己声明的 ROOT 指向夹具树——
# 判据、解析与枚举那三段都在被测的那一份代码里，桩掉它等于什么都没测。
# 夹具全在 $tmp 下，不碰真仓库的工作树。
#
# 改动 check-lessons-index.sh 之后跑：
#   bash scripts/check-lessons-index.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-lessons-index.sh"
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

# lesson <根> <文件名> <首行编号>
# 正文文件的形状是固定的（首行 `# NN · 主题`），只把编号参数化。
lesson() { printf '# %s · 主题\n\n正文\n' "$3" >"$1/lessons/$2"; }

# ── 夹具：四条判据都成立的那一份 ──────────────────────────────────────────
#
# 它同时钉住三件「看着像不一致、其实不是」的事：
#
#   - 索引表**前后都有段落**（表格不是文件里唯一的行）；
#   - 「状态」列里有一处**交叉引用** `已被[第 3 条](lessons/03-gamma.md)取代`——
#     按「一行里出现过几个链接」来数，那一行会被数成两行，于是第 3 条
#     对着一个正确的索引表天天报红；
#   - 文件名是 `01-…` 而首行是 `# 1 · …`：**编号带不带前导零必须判成同一个**。

mk_clean() {
  local root="$1"
  mkdir -p "$root/lessons"
  lesson "$root" 01-alpha.md 1
  lesson "$root" 02-beta.md 2
  lesson "$root" 03-gamma.md 3
  cat >"$root/LESSONS.md" <<'EOF'
# LESSONS

表格前面的一段说明文字，里面提到了 [lessons/](lessons/) 这个目录。

## 索引

| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/01-alpha.md) | 甲 | 甲的时候 | 活 |
| [2](lessons/02-beta.md) | 乙 | 乙的时候 | 已被[第 3 条](lessons/03-gamma.md)取代 |
| [3](lessons/03-gamma.md) | 丙 | 丙的时候 | 活 |

表格后面还有一段话。
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

# ── 2. 正向证据：解析了几行、扫了几个文件 ─────────────────────────────────
#
# 「什么都没输出」与「什么都没检查」长得一样，所以这行证据必须真的报出数来，
# 而且数要与夹具对得上。**3 行 / 3 个文件**这一个具体数字是关键：
# 状态列里那处交叉引用若被数成第二行，这里会变成 4；表格前后的段落若被当成
# 索引行，这里也会变——两种情况都是这道门自己坏掉的样子。

grep -qE '^索引表解析出 [1-9][0-9]* 行 / lessons/ 下 [1-9][0-9]* 个 \.md 文件$' "$last_out" ||
  bad "正向证据 —— 要通过时也要报「解析出多少行 / 扫到多少个文件」（CI 靠这行判它真的跑了）"
grep -q '^索引表解析出 3 行 / lessons/ 下 3 个 \.md 文件$' "$last_out" ||
  bad "正向证据 —— 数要与夹具对得上（3 行 / 3 个文件）：状态列的交叉引用不算第二行，表格前后的段落不算索引行"

# ── 3. 判据 1：索引里有、文件不存在 ───────────────────────────────────────

miss="$tmp/missing"
mk_clean "$miss"
cat >>"$miss/LESSONS.md" <<'EOF'
| [9](lessons/09-nonexistent.md) | 缺 | 缺的时候 | 活 |
EOF

run "$miss"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 1 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 1 ·' "$last_out" || bad "判据 1 —— 要指出是哪一条判据"
grep -q '09-nonexistent\.md' "$last_out" || bad "判据 1 —— 要报出那个不存在的文件"
grep -q 'LESSONS\.md:[0-9]* 指向' "$last_out" ||
  bad "判据 1 —— 要指回索引里的行号（表格后面追加的那一行）"

# ── 4. 判据 2：文件在、索引里没有 ─────────────────────────────────────────

extra="$tmp/unlisted"
mk_clean "$extra"
lesson "$extra" 04-delta.md 4

run "$extra"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 2 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 2 · lessons/04-delta\.md 在索引里没有一行' "$last_out" ||
  bad "判据 2 —— 要报出那个没进索引的文件"
# 这一条同时会撞上判据 3（4 个文件、3 行索引）：两条都要报出来，
# 只报一条的话，读的人会以为改一处就够了。
grep -q '判据 3 ·' "$last_out" || bad "判据 2 的夹具同时行数不等，判据 3 也该报出来"

# ── 5. 判据 3：索引把同一行写了两遍 ───────────────────────────────────────
#
# 这是**判据 3 唯一不可替代**的场景：两侧的集合完全相等（1、2 两条判据都过），
# 只有行数对不上。所以这里同时断言「判据 1 / 2 都没有报」——
# 否则这条用例证明不了它测的是重复，而不是顺带撞上了别的判据。

dup="$tmp/duplicate"
mk_clean "$dup"
cat >>"$dup/LESSONS.md" <<'EOF'
| [3](lessons/03-gamma.md) | 丙（又写了一遍） | 丙的时候 | 活 |
EOF

run "$dup"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 3（重复行）—— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 3 · 索引 4 行，正文 3 个文件，数目不等' "$last_out" ||
  bad "判据 3（重复行）—— 要报出两侧的数目"
grep -q '└ 索引里 lessons/03-gamma\.md 被写了不止一行' "$last_out" ||
  bad "判据 3（重复行）—— 要指出是哪一行重复了"
if grep -qE '判据 [12] ·' "$last_out"; then
  bad "判据 3（重复行）—— 集合是相等的，判据 1 / 2 都不该报"
else
  ok
fi

# ── 6. 判据 4：文件名编号与首行编号不一致 ─────────────────────────────────
#
# 这一条是这道门存在的理由：点进去不是 404，**是另一条**。

# 只改首行的编号（文件名与索引都不动）：这样判据 1、2、3 全过，
# 报出来的必须**只有**判据 4，否则这条用例证明不了它测的是编号脱钩。
drift="$tmp/drift"
mk_clean "$drift"
lesson "$drift" 03-gamma.md 4

run "$drift"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "判据 4 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '判据 4 · lessons/03-gamma\.md 的文件名编号是 03，首行写的是 4' "$last_out" ||
  bad "判据 4 —— 要同时报出两边的编号（文件名 03 / 首行 4）"
if grep -qE '判据 [123] ·' "$last_out"; then
  bad "判据 4 —— 这个夹具只动了首行编号，其它三条判据都不该报"
else
  ok
fi

# ── 7. 编号带不带前导零：判成同一个 ───────────────────────────────────────
#
# 上一条用的是 04 vs 5，天然不等；这一条反过来钉「0 前缀不算差异」，
# 免得哪天有人把比较改成字符串比较（`01` != `1`）而第 1 条照样绿。

pad="$tmp/padding"
mkdir -p "$pad/lessons"
lesson "$pad" 7-seven.md 7
lesson "$pad" 08-eight.md 8
cat >"$pad/LESSONS.md" <<'EOF'
| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [7](lessons/7-seven.md) | 七 | 七的时候 | 活 |
| [8](lessons/08-eight.md) | 八 | 八的时候 | 活 |
EOF

run "$pad"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "前导零 —— 两边写法不同、编号相同，应通过，实际 $rc"; fi

# ── 8. 索引表一行都解析不出来：退出码 2 ───────────────────────────────────
#
# 这是「没跑成」而不是「干净」。判成通过的话，判据写坏（比如行首锚点不对）
# 就变成一次静默的假保证——而它的输出与「索引与正文一致」一模一样。

norows="$tmp/norows"
mkdir -p "$norows/lessons"
lesson "$norows" 01-alpha.md 1
cat >"$norows/LESSONS.md" <<'EOF'
# LESSONS

这一份里只有说明文字，没有索引表。

- 顺手把条目写成了列表： [1](lessons/01-alpha.md)
EOF

run "$norows"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "索引一行都没有 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一行索引都没解析出来' "$last_out" || bad "索引一行都没有 —— 要说清是这个原因"

# ── 9. lessons/ 目录不存在：退出码 2 ──────────────────────────────────────

nodir="$tmp/nodir"
mkdir -p "$nodir"
printf '# LESSONS\n' >"$nodir/LESSONS.md"

run "$nodir"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "lessons/ 不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '不存在' "$last_out" || bad "lessons/ 不存在 —— 要说清是这个原因"

# ── 10. LESSONS.md 不存在：退出码 2 ───────────────────────────────────────

noindex="$tmp/noindex"
mkdir -p "$noindex/lessons"
lesson "$noindex" 01-alpha.md 1

run "$noindex"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "LESSONS.md 不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi

# ── 11. lessons/ 是空的：退出码 2 ─────────────────────────────────────────

empty="$tmp/empty"
mkdir -p "$empty/lessons"
cat >"$empty/LESSONS.md" <<'EOF'
| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/01-alpha.md) | 甲 | 甲的时候 | 活 |
EOF

run "$empty"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "lessons/ 是空的 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个 \*\.md 都没扫到' "$last_out" || bad "lessons/ 是空的 —— 要说清是这个原因"

# ── 12. 文件名里认不出编号：退出码 2 ──────────────────────────────────────
#
# `lessons/README.md` 这种文件今天不存在，但它是最可能被加进来的那一个。
# 判成通过的话，它就落在四条判据之外——「范围之外的东西不是判成通过，
# 是根本没看」（LESSONS.md 第 22 条）。所以这里要响亮地失败。

badname="$tmp/badname"
mkdir -p "$badname/lessons"
lesson "$badname" README.md 1
cat >"$badname/LESSONS.md" <<'EOF'
| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/README.md) | 甲 | 甲的时候 | 活 |
EOF

run "$badname"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "文件名认不出编号 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '文件名里认不出编号' "$last_out" || bad "文件名认不出编号 —— 要说清是这个原因"

# ── 13. 首行里认不出编号：退出码 2 ────────────────────────────────────────

badhead="$tmp/badhead"
mkdir -p "$badhead/lessons"
printf '# 第 1 条：主题\n\n正文\n' >"$badhead/lessons/01-alpha.md"
cat >"$badhead/LESSONS.md" <<'EOF'
| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/01-alpha.md) | 甲 | 甲的时候 | 活 |
EOF

run "$badhead"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "首行认不出编号 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '首行里认不出编号' "$last_out" || bad "首行认不出编号 —— 要说清是这个原因"

# ── 14. 链接目标不是 lessons/ 本层的 .md：退出码 2 ────────────────────────
#
# 子目录是这道门声明不看的范围（见脚本开头「已知边界」）。但一个**指向**它的
# 链接是另一回事：这道门认不出那个形状，认不出就不能当作通过。

# 本层也放一个正常文件：不放的话这一条会先撞上「一个 *.md 都没扫到」，
# 于是它测的是那个出口，而不是「链接指向子目录」这一个。
subdir="$tmp/subdir"
mkdir -p "$subdir/lessons/sub"
lesson "$subdir" 02-beta.md 2
lesson "$subdir" sub/01-alpha.md 1
cat >"$subdir/LESSONS.md" <<'EOF'
| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/sub/01-alpha.md) | 甲 | 甲的时候 | 活 |
EOF

run "$subdir"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "链接指向子目录 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '链接目标不是 lessons/ 本层的' "$last_out" ||
  bad "链接指向子目录 —— 要说清是这个原因"

# ── 15. 首格没有链接的行不算索引行 ────────────────────────────────────────
#
# 一行算不算索引行，看的是**首个单元格**里有没有链接。这个夹具里第二行把编号
# 写成了裸数字、链接挪到了「状态」列——它不是索引行，所以 02-beta.md 与
# 03-gamma.md 都成了「在索引里没有一行」。
#
# 判据从「首格」放宽成「整行里找链接」时，那一行会被算成一行（指向 03-gamma.md），
# 于是**行数变成 2**、而且 03-gamma.md 蒙混过关——这一条两个数都会响。

firstcell="$tmp/firstcell"
mkdir -p "$firstcell/lessons"
lesson "$firstcell" 01-alpha.md 1
lesson "$firstcell" 02-beta.md 2
lesson "$firstcell" 03-gamma.md 3
cat >"$firstcell/LESSONS.md" <<'EOF'
| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/01-alpha.md) | 甲 | 甲的时候 | 活 |
| 2 | 乙 | 乙的时候 | 已被[第 3 条](lessons/03-gamma.md)取代 |
EOF

run "$firstcell"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "首格没链接的行 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^索引表解析出 1 行 / lessons/ 下 3 个 \.md 文件$' "$last_out" ||
  bad "首格没链接的行 —— 只有首格带链接的那一行算索引行（应为 1 行，不是 2 行）"
grep -q '判据 2 · lessons/02-beta\.md 在索引里没有一行' "$last_out" ||
  bad "首格没链接的行 —— 02-beta.md 该被报成没进索引"
grep -q '判据 2 · lessons/03-gamma\.md 在索引里没有一行' "$last_out" ||
  bad "首格没链接的行 —— 状态列里那处交叉引用不该让 03-gamma.md 蒙混过关"

# ── 16. 真仓库：这道门在它自己身上是干净的 ────────────────────────────────
#
# 前十四条用的是夹具。这一条跑真仓库，钉住「落地时确实是对得上的」——
# 顺带证明夹具之外的真实文件树也能被扫（文件名里有点、有连字符，行数上百）。

ROOT="$PWD" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "真仓库 —— 应通过（退出码 0），实际 $rc"; fi
repo_rows=$(grep -oE '^索引表解析出 [0-9]+ 行' "$last_out" | grep -oE '[0-9]+')
if [ "${repo_rows:-0}" -ge 30 ]; then ok; else
  bad "真仓库 —— 只解析出 ${repo_rows:-0} 行索引，解析范围像是写错了"
fi

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
