#!/usr/bin/env bash
# 分层自查：把几条「只能靠约定维持」的规则变成可执行的检查。
#
# 为什么需要它：这类规则编译器管不了，而文档里的承诺会随实现漂移——
# 写着「只有 A 包 import X」，半年后 B 包也 import 了，没人会发现，
# 直到有人按文档去改，发现文档是错的。
#
# 用法：scripts/check-layering.sh
# 退出码：0 合规、1 违规

set -uo pipefail
cd "$(dirname "$0")/.."

fail=0

# ── 规则一：直接 import relaykit 的包，仅限这一份清单 ─────────────────────
#
# relay 是协议转换的入口；两个适配器为了与它的接口对接，也直接用它的
# DTO 类型。清单之外出现新的 import，说明有人绕过了这道缝。
ALLOWED_RELAYKIT_IMPORTERS='internal/relay
internal/channel/openaiadapter
internal/channel/anthropicadapter'

echo "── 谁 import 了 relaykit"
found=$(for p in $(go list ./internal/... ./cmd/... 2>/dev/null); do
  if go list -f '{{join .Imports "\n"}}' "$p" 2>/dev/null | grep -q 'relaykit'; then
    echo "${p#github.com/sumwai/larkax/}"
  fi
done)

while IFS= read -r pkg; do
  [ -n "$pkg" ] || continue
  if ! printf '%s\n' "$ALLOWED_RELAYKIT_IMPORTERS" | grep -qx "$pkg"; then
    echo "  ✗ $pkg —— 不在允许清单里"
    fail=1
  else
    echo "  ✓ $pkg"
  fi
done <<< "$found"

# ── 规则二：金额运算只在 money 包里出现 ───────────────────────────────────
#
# 别的包若自己拿 float64 乘金额，那个舍入点就在 money 之外了，
# 而对账时不一致的舍入表现为一笔永远找不平的差额。
echo "── 除 money 外谁在算术里用 float64 处理金额"
suspicious=$(grep -rn --include=*.go -E '\bfloat64\(' internal/ 2>/dev/null \
  | grep -v '^internal/money/' \
  | grep -v '_test.go' \
  | grep -viE '//|rate|ratio|score|confidence|hit|percent|weight|latency|factor' || true)
if [ -n "$suspicious" ]; then
  printf '%s\n' "$suspicious" | sed 's/^/  ? /'
  echo "  （逐条确认：是金额运算就该挪进 money 包）"
fi

# ── 规则三：数据面不依赖控制面 ────────────────────────────────────────────
#
# gateway 还没写，所以这条现在只能检查「有没有反向依赖」：
# 任何包都不该 import 一个尚不存在的 gateway。
echo "── 反向依赖检查"
if grep -rn --include=*.go '"github.com/sumwai/larkax/internal/gateway"' internal/ cmd/ 2>/dev/null; then
  echo "  ✗ 有包 import 了 gateway（那是入口层，不该被依赖）"
  fail=1
else
  echo "  ✓ 无"
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "分层自查不通过。"
  exit 1
fi
echo "分层自查通过。"
