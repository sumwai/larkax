#!/usr/bin/env bash
# 对核心计费与账本包跑变异测试。
#
# # 为什么固定 workers=1
#
# gremlins 给每个变异的超时预算 = 「跑一次测试集的时间 × 系数」，而那个
# 基准是**串行**测出来的。并发跑变异时机器上同时有 N 个 go test 在编译与
# 执行，超时按墙钟算，于是并发越高、每个变异实际能用的 CPU 越少，
# 越多变异被判超时。
#
# 实测（internal/money，系数固定 10）：
#
#   workers=1  → Killed 80, Timed out 0
#   workers=2  → Killed 80, Timed out 0
#   workers=4  → Killed  3, Timed out 92
#
# 所以**并发与系数是同一个旋钮**：调大系数是在补并发造成的短缺，
# 而那会让「这个包需要多大系数」变成一个随机器负载变化的假属性。
# 固定串行之后，系数才回到它的本意——容纳「变异体比原代码慢」的那种包。
#
# 代价是慢。但变异测试不是每次提交都跑的东西（见 AGENTS.md），
# 慢而可信比快而误导好。
#
# 用法：
#   scripts/check-mutation.sh              # 跑全部包
#   scripts/check-mutation.sh money route  # 只跑指定的（按包名后缀匹配）
#
# 退出码：0 全部跑成、1 有包没跑成（超时过多或工具缺失）。
#
# **它不判「efficacy 够不够」。** 存活变异要逐条人看——等价变异与
# 不可达分支都该放过，而脚本分不出来。这里只保证跑出来的数字可信。

set -uo pipefail
cd "$(dirname "$0")/.."

GREMLINS="${GREMLINS:-gremlins}"
if ! command -v "$GREMLINS" >/dev/null 2>&1; then
  echo "找不到 gremlins。安装：go install github.com/go-gremlins/gremlins/cmd/gremlins@latest"
  echo "或用 GREMLINS=/path/to/gremlins 指定。"
  exit 1
fi

COEF="${COEF:-10}"
WORKERS="${WORKERS:-1}"

PKGS=(
  internal/money
  internal/billing
  internal/ledger
  internal/pipeline
  internal/calendar
  internal/quota
  internal/tokens
  internal/route
  internal/snapshot
  internal/gateway
)

# 一个包里超时的变异超过这个数，就认为系数不够、结果不可信。
MAX_TIMED_OUT=10

filters=("$@")
selected() {
  [ ${#filters[@]} -eq 0 ] && return 0
  local pkg="$1"
  for f in "${filters[@]}"; do
    case "$pkg" in *"$f") return 0 ;; esac
  done
  return 1
}

failed=0
printf '%-20s %8s %7s %8s  %s\n' "包" "killed" "存活" "超时" "efficacy"
printf '%s\n' "──────────────────────────────────────────────────────────────"

for pkg in "${PKGS[@]}"; do
  selected "$pkg" || continue

  out=$("$GREMLINS" unleash --timeout-coefficient="$COEF" --workers="$WORKERS" "./$pkg/" 2>&1)
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "── $pkg: gremlins 执行失败（退出码 $rc）"
    printf '%s\n' "$out" | tail -5 | sed 's/^/   /'
    failed=1
    continue
  fi

  killed=$(printf '%s\n' "$out" | sed -n 's/^Killed: \([0-9]*\).*/\1/p')
  lived=$(printf '%s\n' "$out" | sed -n 's/^.*Lived: \([0-9]*\).*/\1/p')
  timed=$(printf '%s\n' "$out" | sed -n 's/^.*Timed out: \([0-9]*\).*/\1/p')
  eff=$(printf '%s\n' "$out" | sed -n 's/^Test efficacy: \(.*\)$/\1/p')

  printf '%-20s %8s %7s %8s  %s\n' "$pkg" "${killed:-?}" "${lived:-?}" "${timed:-?}" "${eff:-?}"

  if [ -n "${timed:-}" ] && [ "$timed" -gt "$MAX_TIMED_OUT" ]; then
    echo "   ⚠ 超时 $timed 个（上限 $MAX_TIMED_OUT）——这个分数不可信，先查是不是跑并发跑了"
    failed=1
  fi
done

echo
if [ "$failed" -ne 0 ]; then
  echo "有包的结果不可信或没跑成，见上面的 ⚠。"
  exit 1
fi
echo "全部跑成。存活清单要逐条人看——等价变异与不可达分支都该放过。"
