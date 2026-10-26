#!/usr/bin/env bash
# check-mutation-table.sh 的自测。
#
# 为什么需要它：这道门的成功信号是「条数对得上」——判据写反、解析范围错了、
# 表头没认出来时，屏幕上同样可能什么都不报。所以正反两面都钉：集合一致时
# 通过，少一行 / 多一行时点名那一行，「没跑成」时走退出码 2 而不是 0。
#
# 前几条用 $tmp 下的夹具；最后一条跑真仓库，钉住「落地时确实是对得上的」，
# 同时用条数下限证明解析范围没有缩成几条。
#
# 改动 check-mutation-table.sh 之后跑：
#   bash scripts/check-mutation-table.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-mutation-table.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

last_out="$tmp/last.txt"
. "$PWD/scripts/gateselftest.sh"

# run <夹具根>：退出码由调用者立刻取走。
run() { ROOT="$1" bash "$SCRIPT" >"$last_out" 2>&1; }

# mk_fixture <根> <PKGS 条目…>（表格行由调用者再改）
mk_fixture() {
  local root="$1"
  shift
  mkdir -p "$root/scripts" "$root/docs"
  {
    printf '#!/usr/bin/env bash\nPKGS=(\n'
    for p in "$@"; do printf '  %s\n' "$p"; done
    printf ')\n'
  } >"$root/scripts/check-mutation.sh"
  {
    printf '# 开发\n\n## 变异测试\n\n### 各包的现状\n\n'
    printf '| 包 | killed | 存活 |\n|---|---|---|\n'
    for p in "$@"; do printf '| `%s` | 1 | 0 |\n' "$p"; done
    printf '\n**表后说明。**\n'
  } >"$root/docs/development.md"
}

# ── 1. 集合一致：通过，并报出条数 ──────────────────────────────────────────
mk_fixture "$tmp/clean" internal/db internal/dberr internal/systemone
run "$tmp/clean"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "一致 —— 应通过，实际退出码 $rc"; fi
grep -q 'PKGS 3 条，表 3 行' "$last_out" || bad "一致 —— 没报出解析条数（正向证据）"

# ── 2. PKGS 有、表里少一行：失败并点名 ─────────────────────────────────────
mk_fixture "$tmp/missing-row" internal/db internal/dberr internal/systemone
sed -i '/internal\/dberr/d' "$tmp/missing-row/docs/development.md"
run "$tmp/missing-row"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "表里少一行 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'internal/dberr' "$last_out" || bad "表里少一行 —— 没点名 internal/dberr"
grep -q 'PKGS 里有、表里没有' "$last_out" || bad "表里少一行 —— 没说清缺在哪一侧"

# ── 3. 表里有、PKGS 没有：失败并点名 ───────────────────────────────────────
mk_fixture "$tmp/extra-row" internal/db internal/dberr internal/systemone
# 插在表内最后一行之后（表后那个空行是表尾，脚本读到它就停）。
sed -i '/^| `internal\/systemone`/ a | `internal/ghost` | 9 | 0 |' "$tmp/extra-row/docs/development.md"
run "$tmp/extra-row"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "表里多一行 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'internal/ghost' "$last_out" || bad "表里多一行 —— 没点名 internal/ghost"
grep -q '表里有、PKGS 里没有' "$last_out" || bad "表里多一行 —— 没说清多在哪一侧"

# ── 4. PKGS 一段都解析不出来：没跑成（退出码 2），不是通过 ─────────────────
mk_fixture "$tmp/empty-pkgs" internal/db
printf '#!/usr/bin/env bash\n# PKGS 没了\n' >"$tmp/empty-pkgs/scripts/check-mutation.sh"
run "$tmp/empty-pkgs"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "PKGS 为空 —— 应判没跑成（退出码 2），实际 $rc"; fi
grep -q '没跑成' "$last_out" || bad "PKGS 为空 —— 没说明是没跑成"

# ── 5. 表头都认不出来：没跑成，而不是「表里 0 行所以一致」 ─────────────────
mk_fixture "$tmp/empty-table" internal/db
sed -i '/^| 包 | killed |/d' "$tmp/empty-table/docs/development.md"
run "$tmp/empty-table"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "表格为空 —— 应判没跑成（退出码 2），实际 $rc"; fi
grep -q '没跑成' "$last_out" || bad "表格为空 —— 没说明是没跑成"

# ── 6. 真仓库：这道门在它自己身上是干净的 ──────────────────────────────────
#
# 前五条用的是夹具。这一条跑真仓库，钉住「落地时确实是对得上的」，
# 并用条数下限证明解析范围没有缩成几条。
ROOT="$PWD" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "真仓库 —— 应通过（退出码 0），实际 $rc"; fi
repo_pkgs=$(grep -oE 'PKGS [0-9]+ 条' "$last_out" | grep -oE '[0-9]+')
repo_rows=$(grep -oE '表 [0-9]+ 行' "$last_out" | grep -oE '[0-9]+')
if [ "${repo_pkgs:-0}" -ge 30 ]; then ok; else
  bad "真仓库 —— 只解析出 ${repo_pkgs:-0} 条 PKGS，解析范围像是写错了"
fi
if [ "${repo_rows:-0}" -ge 30 ]; then ok; else
  bad "真仓库 —— 只解析出 ${repo_rows:-0} 行表格，解析范围像是写错了"
fi

# ── 结果 ──────────────────────────────────────────────────────────────────

# 造一次违规要真的红，这是第 2、3 条的职责；这里再印一遍结论供人核对。
report
