#!/usr/bin/env bash
# 分层自查：把几条「只能靠约定维持」的规则变成可执行的检查。
#
# 为什么需要它：这类规则编译器管不了，而文档里的承诺会随实现漂移——
# 写着「只有 A 包 import X」，半年后 B 包也 import 了，没人会发现，
# 直到有人按文档去改，发现文档是错的。
#
# 检查三条：
#   一、直接 import relaykit 的包，仅限 relay 与各协议适配器
#   二、money 之外谁在算术里用 float64 处理金额
#   三、import gateway 的包仅限下面 ALLOWED_GATEWAY_IMPORTERS 里列的那几个
#
# 一与三有唯一答案，命中即失败；二是**提示型**的——它按关键字做排除，
# 认不出「这一处到底是不是金额」，所以只把可疑行打出来让人逐条看，
# 不参与退出码（把误报也判成失败的话，这份报告第二次就没人看了）。
#
# 用法：scripts/check-layering.sh
# 退出码：0 合规、1 违规（规则一与规则三）

set -uo pipefail
cd "$(dirname "$0")/.."

fail=0

# ── 规则一：直接 import relaykit 的包，仅限这一份清单 ─────────────────────
#
# relay 是协议转换的入口；各协议适配器为了与它的接口对接，也直接用它的
# DTO 类型。清单之外出现新的 import，说明有人绕过了这道缝。
ALLOWED_RELAYKIT_IMPORTERS='internal/relay
internal/channel/openaiadapter
internal/channel/anthropicadapter
internal/channel/responsesadapter'

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
# gateway 是入口层，别的包不该 import 它——否则一条依赖链会把路由与计费
# 拖进它们不需要知道的东西里。例外需要一个明确的名字写在这里，
# 而不是一个「大概可以」的判断。
#
# 例外里为什么有 snapshotwire：gateway 用接口声明了自己要什么
# （Redactor / SnapshotSink），而满足那两个接口需要同时看见 snapshot
# 与 gateway 两侧的类型。适配放在一个专门的装配包里，gateway 本身
# 仍然不 import snapshot。
#
# 例外里为什么有 api：gateway 是入口层的**实现**，api 是入口层的 HTTP
# 那一半，它要调 gateway.Chat 并读 gateway.Result。让 gateway 反过来声明
# 一个「给 api 用的窄接口」会把入口的形状搬进 gateway，而那个形状正是
# api 该定的东西。方向仍然是 api → gateway，没有第三个包会顺着它被拖进来。
#
# 例外里为什么有 assembly：装配层是**唯一**同时看见入口与数据面的地方——
# 它构造 gateway，把候选来源、账本存储、快照链路、上游执行器接上去，再交给
# api。这一层不 import gateway 的话，「谁把 gateway 建起来」就没有答案，
# 只能落回 cmd/larkax，而那样端到端测试就够不着生产用的那条装配路径了。
# 它不做任何编排决策，也不被任何包 import。
ALLOWED_GATEWAY_IMPORTERS='internal/snapshotwire
internal/api
internal/assembly'

echo "── 谁 import 了 gateway"
# 先取「扫到了多少个文件」再取命中：只打印命中与否的话，
# 一个因为路径写错而扫了 0 个文件的检查输出与「干净」完全一样
#（LESSONS.md 第 16 条）。条数是这一条的正向证据。
scanned=$(grep -rl --include=*.go '"github.com/sumwai/larkax/internal/gateway"' internal/ cmd/ 2>/dev/null; echo "rc=$?")
rc=${scanned##*rc=}
scanned=${scanned%rc=*}
go_files=$(find internal cmd -name '*.go' 2>/dev/null | wc -l)
if [ "$rc" -gt 1 ]; then
  echo "  ✗ 检查没能执行（grep 退出码 $rc），结果不可信"
  fail=1
elif [ "$go_files" -eq 0 ]; then
  echo "  ✗ 一个 .go 文件都没扫到，检查范围写错了"
  fail=1
else
  echo "  （在 $go_files 个 .go 文件里查找）"
  if [ -z "$scanned" ]; then
    echo "  ✓ 无"
  else
    while IFS= read -r dir; do
      [ -n "$dir" ] || continue
      rel="${dir#./}"
      if printf '%s\n' "$ALLOWED_GATEWAY_IMPORTERS" | grep -qx "$rel"; then
        echo "  ✓ $rel"
      else
        echo "  ✗ $rel —— 不在允许清单里（gateway 是入口层，不该被依赖）"
        fail=1
      fi
    done <<< "$(printf '%s\n' "$scanned" | xargs -r -n1 dirname | sort -u)"
  fi
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "分层自查不通过。"
  exit 1
fi
echo "分层自查通过。"
