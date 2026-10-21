#!/usr/bin/env bash
# 「文档里的变异基线表 ↔ 脚本里的包清单」一致性的门禁。
#
# # 为什么需要它
#
# `scripts/check-mutation.sh` 的 `PKGS` 是变异测试的唯一包清单；
# docs/development.md「各包的现状」那张表是它的记录。两份都是手工维护的，
# 于是有两类漂移，都不会让任何测试失败——表的数字不进 CI：
#
#   - 加了包进 `PKGS`、忘了补表：这个包从此会被量，而数不在任何地方；
#   - 表里有一行、`PKGS` 里没有：那个数是手工跑的，脚本永远不会再跑它，
#     下一次改动之后它就停在旧值，而读表的人以为它是脚本随时能复现的。
#
# 所以单独一道门，判据只有一条：两边的包名集合相等。顺序不参与比较——
# 表按 efficacy 排、清单按优先级排，两者本来就不是同一个序。
#
# # 「没跑成」与「干净」必须分开
#
# 两边任一抽到 0 条时退出 2，而不是报通过：解析范围写错、表头被改掉、
# `PKGS` 那一段换了写法，输出都会和「对得上」一样（LESSONS 第 16 条）。
# 每次运行都打印抽到的条数，作为「这次真的看了东西」的正向证据。
#
# # 怎么认
#
#   - `PKGS`：`scripts/check-mutation.sh` 里 `PKGS=(` 到 `)` 之间的非空行；
#   - 表格：docs/development.md 里以 `| 包 | killed |` 开头的那一段，
#     取每一行第一个带反引号的单元格。
#
# 表格之后的空行即表尾。只认这一张表：文档别处也有包名（目录一览、
# 「哪道门挡什么」），它们不参与。
#
# 本脚本有一份自测：scripts/check-mutation-table.test.sh。改动后必须跑它。

set -uo pipefail
cd "$(dirname "$0")/.."

ROOT="${ROOT:-$PWD}"

SCRIPT="$ROOT/scripts/check-mutation.sh"
DOCS="$ROOT/docs/development.md"

for f in "$SCRIPT" "$DOCS"; do
  if [ ! -f "$f" ]; then
    echo "读不到 $f——没跑成，不是通过。" >&2
    exit 2
  fi
done

# 上一段自测里用的是同一段 awk（check-mutation.test.sh 第 25b 条）。
pkgs=$(awk '
  /^PKGS=\(/ { f = 1; next }
  f && /^\)/  { exit }
  f { sub(/^[[:space:]]+/, ""); sub(/[[:space:]]+$/, ""); if (length($0)) print }
' "$SCRIPT" | LC_ALL=C sort)

rows=$(awk '
  /^\| 包 \| killed \|/ { in_table = 1; next }
  in_table && /^[[:space:]]*$/ { exit }
  in_table && /^\| `/ {
    line = $0
    sub(/^\| `/, "", line)
    sub(/`.*/, "", line)
    print line
  }
' "$DOCS" | LC_ALL=C sort)

n_pkgs=$(printf '%s\n' "$pkgs" | grep -c '[^[:space:]]' || true)
n_rows=$(printf '%s\n' "$rows" | grep -c '[^[:space:]]' || true)

if [ "$n_pkgs" -eq 0 ] || [ "$n_rows" -eq 0 ]; then
  echo "没跑成：解析出 PKGS ${n_pkgs} 条、表格 ${n_rows} 行。" >&2
  echo "  PKGS 为空：scripts/check-mutation.sh 里那段写法变了？" >&2
  echo "  表格为空：docs/development.md 里「| 包 | killed |」表头或行格式变了？" >&2
  exit 2
fi

echo "变异清单与文档表格：PKGS ${n_pkgs} 条，表 ${n_rows} 行。"

only_pkgs=$(LC_ALL=C comm -23 <(printf '%s\n' "$pkgs") <(printf '%s\n' "$rows"))
only_rows=$(LC_ALL=C comm -13 <(printf '%s\n' "$pkgs") <(printf '%s\n' "$rows"))

rc=0
if [ -n "$only_pkgs" ]; then
  echo
  echo "PKGS 里有、表里没有——补行到 docs/development.md 的「各包的现状」："
  printf '%s\n' "$only_pkgs" | sed 's/^/  /'
  rc=1
fi
if [ -n "$only_rows" ]; then
  echo
  echo "表里有、PKGS 里没有——这一行的数是手工跑的，脚本不会再量它："
  printf '%s\n' "$only_rows" | sed 's/^/  /'
  rc=1
fi
exit "$rc"
