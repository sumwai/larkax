#!/usr/bin/env bash
# 分支新增的迁移编号必须大于 base 上已有的最大号。
#
# # 为什么需要它
#
# 迁移文件名里的号是写文件那一刻的「next」，而合并顺序与编号顺序无关。两条分支
# 同时加迁移时，号大的先合入并部署，已经升过级的库就缺了号小的那条：goose 记下的
# 是大号，从此拒绝应用小号，后续迁移全部卡住（见 lessons/45-migration-number-merge-order.md）。
# 原先靠合并前人工看一眼 `origin/main` 上最大的号，这一步可以机器判。
#
# # 判据
#
#   - base（默认 `origin/main`）树里 internal/migrate/sql 的最大号；
#   - HEAD 相对 base **新增**的迁移（`git diff --name-status <base>...HEAD`，只认 A）；
#   - 任何新增号的编号 ≤ base 的最大号即失败，并点名该文件。
#
# 只比号，不比文件名里的描述；改名与删除不参与。
#
# # 「没跑成」与「干净」必须分开
#
# base 解析不出提交、base 树里一条迁移都抽不到、`git diff` 本身失败（浅克隆没有
# 合并基）时，退出码 2 而不是 0：这几件事的屏幕输出都与「没有乱序」一模一样
# （LESSONS 第 16 条）。每次运行都打印抽到的条数与 base 最大号，作为「这次真的
# 看了东西」的正向证据。
#
# # 已知边界
#
# 它看不见「CI 跑完之后 base 又前进了」的那种情况——那需要在合并前再看一眼
# （工作流 skill 第 ⑦ 步）。它也不判重号，那由库里的迁移测试负责。
#
# 本脚本有一份自测：scripts/check-migration-order.test.sh。改动后必须跑它。

set -uo pipefail

ROOT="${ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$ROOT"

BASE="${1:-origin/main}"
DIR="internal/migrate/sql"

if ! git rev-parse --verify --quiet "${BASE}^{commit}" >/dev/null 2>&1; then
  echo "解析不出 base 提交 ${BASE}——没跑成，不是通过。" >&2
  echo "  取过 origin/main 吗（git fetch origin main）？ref 的名字写对了吗？" >&2
  exit 2
fi

# 迁移文件名形如 00051_api_key_plans.sql：号在第一个下划线之前。
base_nums=$(git ls-tree -r --name-only "$BASE" -- "$DIR" |
  sed -n 's|^.*/\([0-9][0-9]*\)_.*\.sql$|\1|p' |
  LC_ALL=C sort -n)

n_base=$(printf '%s\n' "$base_nums" | grep -c . || true)
if [ "$n_base" -eq 0 ]; then
  echo "没跑成：base ${BASE} 的 ${DIR} 里解析出 0 条迁移。" >&2
  echo "  目录搬走了，或者迁移文件名不再以数字加下划线开头？" >&2
  exit 2
fi

# 前缀是补零的 5 位（00049），而 [ 与 $(( )) 会把前导零当八进制，
# 所以比较用剥掉前导零的数，报给读者看的是文件名里的原样（00049）。
base_max_str=$(printf '%s\n' "$base_nums" | tail -1)
base_max=$((10#$base_max_str))

if ! added_raw=$(git diff --name-status "${BASE}...HEAD" -- "$DIR" 2>&1); then
  echo "没跑成：git diff ${BASE}...HEAD 失败：" >&2
  printf '%s\n' "$added_raw" | sed 's/^/  /' >&2
  echo "  浅克隆可能没有合并基（取全历史再试，fetch-depth: 0）。" >&2
  exit 2
fi

added=$(printf '%s\n' "$added_raw" | awk -F'\t' '$1 == "A" { print $2 }')
n_added=$(printf '%s\n' "$added" | grep -c . || true)

echo "迁移编号顺序：base ${BASE} 最大号 ${base_max_str}（共 ${n_base} 条），本分支新增 ${n_added} 条。"

bad=0
while IFS= read -r file; do
  [ -n "$file" ] || continue
  num=$(basename "$file" | sed -n 's/^\([0-9][0-9]*\)_.*\.sql$/\1/p')
  if [ -z "$num" ]; then
    echo "  ${file}：文件名不以数字加下划线开头，判不了号" >&2
    bad=1
    continue
  fi
  if [ "$((10#$num))" -le "$base_max" ]; then
    echo "  ${file}（${num} ≤ ${base_max_str}）" >&2
    bad=1
  fi
done <<<"$added"

if [ "$bad" -ne 0 ]; then
  echo >&2
  echo "迁移编号乱序：新增的号必须大于 base 上的最大号 ${base_max_str}。" >&2
  echo "改成 ${DIR} 里下一个可用的号再提交，否则号小的那条永远上不了已经升过级的库。" >&2
  exit 1
fi

exit 0
