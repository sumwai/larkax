#!/usr/bin/env bash
# check-migration-order.sh 的自测。
#
# 为什么需要它：这道门的成功信号是沉默——base 解析不到、目录搬走、diff 范围写错、
# 判据写反时，屏幕上同样什么都不报。所以正反两面都钉：乱序时红并点名那个文件，
# 有序时绿，两条新增只点名乱序的那条，判据坏了走退出码 2 而不是 0。
#
# 全部夹具都在 $tmp 下自建 git 仓库，不读调用者这棵树的 `.git`：门禁镜像用 rsync
# 同步、排除 `.git`，依赖真实历史的用例在那里只会变成「没测成」。脚本的 `ROOT`
# 环境变量就是为这件事留的，所以这里每个用例都拿 ROOT 指到夹具上；在真仓库里跑
# 这条自测，它同样不会去看真仓库的历史。
#
# 改动 check-migration-order.sh 之后跑：
#   bash scripts/check-migration-order.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-migration-order.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0
last_out="$tmp/last.txt"

ok()  { pass=$((pass + 1)); }
bad() {
  echo "✗ $1"
  sed 's/^/    | /' "$last_out" | tail -8
  fail=$((fail + 1))
}

# run <夹具根> [base-ref]：退出码由调用者立刻取走。
run() { ROOT="$1" bash "$SCRIPT" "${2:-base}" >"$last_out" 2>&1; }

# git 身份与 gpg 签名都用 -c 显式给：这台机器上没有全局 git 身份，
# 而 commit 失败会变成另一种「没测成」。
git_user=(-c user.name=larkax-test -c user.email=user@example.com -c commit.gpgsign=false)
g() { local dir="$1"; shift; git -C "$dir" "${git_user[@]}" "$@"; }

# mk_repo <根> <base 号…>：在 main 上提交这些迁移，再留一条 `base` 分支指向
# 同一个提交；HEAD 仍停在 main，调用者继续往上加迁移。
mk_repo() {
  local root="$1"
  shift
  local n
  mkdir -p "$root/internal/migrate/sql"
  g "$root" -c init.defaultBranch=main init -q
  for n in "$@"; do
    printf -- '-- 基线迁移 %s\n' "$n" >"$root/internal/migrate/sql/${n}_base.sql"
  done
  g "$root" add -A
  g "$root" commit -q -m base
  g "$root" branch base
}

# add_migration <根> <号> [描述]：在 HEAD 上加一条迁移并提交。
add_migration() {
  printf -- '-- 分支迁移 %s\n' "$2" >"$1/internal/migrate/sql/${2}_${3:-new}.sql"
  g "$1" add -A
  g "$1" commit -q -m "add $2"
}

# ── 1. base 有 00050、HEAD 加 00049：红，点名 49 与 base 最大号 ────────────
mk_repo "$tmp/late" 00050
add_migration "$tmp/late" 00049
run "$tmp/late"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "编号落后 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '00049_new' "$last_out" || bad "编号落后 —— 没点名 00049 那个文件"
grep -q '（00049 ≤ 00050）' "$last_out" || bad "编号落后 —— 没打出「00049 ≤ base 最大号 00050」"
grep -q '最大号 00050' "$last_out" || bad "编号落后 —— 没报出 base 的最大号（正向证据）"

# ── 2. base 有 00050、HEAD 加 00051：绿，并报出抽到的条数 ──────────────────
mk_repo "$tmp/ok" 00050
add_migration "$tmp/ok" 00051
run "$tmp/ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "编号更大 —— 应通过（退出码 0），实际 $rc"; fi
grep -q '最大号 00050（共 1 条）' "$last_out" || bad "编号更大 —— 没报出 base 抽到 1 条、最大号 00050"
grep -q '本分支新增 1 条' "$last_out" || bad "编号更大 —— 没报出新增 1 条"

# ── 3. 新增两条、一条乱序：红，且只点名乱序的那条 ─────────────────────────
mk_repo "$tmp/mixed" 00050
add_migration "$tmp/mixed" 00051
add_migration "$tmp/mixed" 00049
run "$tmp/mixed"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "一乱一顺 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '00049_new' "$last_out" || bad "一乱一顺 —— 没点名乱序的 00049"
grep -q '本分支新增 2 条' "$last_out" || bad "一乱一顺 —— 没报出新增 2 条"
if grep -q '00051_new（' "$last_out"; then
  bad "一乱一顺 —— 把顺序正确的那条也点成了违规"
else
  ok
fi

# ── 4. base ref 解析不出来：没跑成（退出码 2），不是通过 ───────────────────
mk_repo "$tmp/no-ref" 00050
add_migration "$tmp/no-ref" 00051
run "$tmp/no-ref" nosuchref
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "base 不存在 —— 应判没跑成（退出码 2），实际 $rc"; fi
grep -q '没跑成' "$last_out" || bad "base 不存在 —— 没说明是没跑成"

# ── 5. base 树里一条迁移都没有：也是没跑成，而不是「没有新增所以干净」 ─────
empty="$tmp/no-migrations"
mkdir -p "$empty"
g "$empty" -c init.defaultBranch=main init -q
printf 'no migrations here\n' >"$empty/README"
g "$empty" add -A
g "$empty" commit -q -m empty
run "$empty" HEAD
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "base 无迁移 —— 应判没跑成（退出码 2），实际 $rc"; fi
grep -q '0 条迁移' "$last_out" || bad "base 无迁移 —— 没报出解析到 0 条"

# ── 6. 新增的迁移文件名不是数字开头：判不了号，按失败处理 ──────────────────
mk_repo "$tmp/bad-name" 00050
printf -- '-- 没有号\n' >"$tmp/bad-name/internal/migrate/sql/readme.sql"
g "$tmp/bad-name" add -A
g "$tmp/bad-name" commit -q -m "add readme.sql"
run "$tmp/bad-name"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "文件名无号 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'readme.sql' "$last_out" || bad "文件名无号 —— 没点名 readme.sql"

# ── 结果 ──────────────────────────────────────────────────────────────────

# 造违规要真的红，这是第 1、3 条的职责；这里再印一遍结论供人核对。
echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
