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
# # 正向证据
#
# 这条门通过时的成功信号接近沉默，而「什么都没输出」与「什么都没检查」
# 长得一样（LESSONS.md 第 16 条）。所以它每次都打印扫了多少个包、
# 多少个 .go 文件、可疑几行；范围那两个数为 0（`internal/` 下没有 .go、
# go list 一个包都没列出来）时判「没跑成」（退出码 2），而不是报通过。
# 「可疑 0 行」是合法的结论，不是证据缺失——它说的是规则二逐行看过了、
# 一行都没命中。
#
# # 用法
#
#   scripts/check-layering.sh
#
# 退出码：
#   0  三条都查过了，规则一与规则三没有违规（规则二有命中也不算失败）
#   1  规则一或规则三命中
#   2  没跑成：go 不在、go list 失败、一个包都没列出来、一个 .go 文件都
#      没扫到、grep 自身出错
#
# 1 与 2 在 CI 里都是红的，但处置完全不同：前者要去改依赖方向，后者说明
# 这道门自己坏了。
#
# # 环境变量
#
# 两个都是给 scripts/check-layering.test.sh 注入夹具用的，CI 与本地都不该设：
#   GO      可执行文件，默认 go
#   MODULE  模块路径，默认 github.com/sumwai/larkax。go list 的输出带这个
#           前缀，gateway 的 import 路径也拼它；自测用假模块名造夹具。
#
# # 自测
#
# scripts/check-layering.test.sh。改动本脚本后必须跑它：一个判据写反、
# 或者一个守卫整个没接上时，它的输出与「干净」完全一样。

set -uo pipefail
cd "$(dirname "$0")/.."

GO="${GO:-go}"
MODULE="${MODULE:-github.com/sumwai/larkax}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

fail=0

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# 数非空行。空字符串算 0 行——`grep -c` 对着一个空行会给出 0，两者一致。
count_nonempty() { printf '%s\n' "$1" | grep -c . || true; }

# ── 扫描范围 ──────────────────────────────────────────────────────────────
#
# 先确认「扫得到东西」。三条规则里前两条只看 internal/、第三条看 internal/
# 与 cmd/，所以两个计数各自代表一条规则的证据；为 0 时那条规则等于没跑。

internal_files=$(find internal -name '*.go' 2>/dev/null | wc -l)
cmd_files=$(find cmd -name '*.go' 2>/dev/null | wc -l)
go_files=$((internal_files + cmd_files))
if [ "$internal_files" -eq 0 ]; then
  not_run "internal/ 下一个 .go 文件都没扫到。
路径写错、或者扫描根不对时就是这样——那不是「干净」，是没扫。"
fi

# ── 规则一：直接 import relaykit 的包，仅限这一份清单 ─────────────────────
#
# relay 是协议转换的入口；各协议适配器为了与它的接口对接，也直接用它的
# DTO 类型。清单之外出现新的 import，说明有人绕过了这道缝。
ALLOWED_RELAYKIT_IMPORTERS='internal/relay
internal/channel/openaiadapter
internal/channel/anthropicadapter
internal/channel/responsesadapter'

echo "── 谁 import 了 relaykit"

if ! command -v "$GO" >/dev/null 2>&1; then
  not_run "找不到 $GO（go 不在 PATH 里）。
规则一要靠 go list 拿包的 import 关系；go 不在时它什么都没检查，
所以这里不当作通过。"
fi

"$GO" list ./internal/... ./cmd/... >"$tmpdir/pkgs" 2>"$tmpdir/pkgs.err"
rc=$?
if [ "$rc" -ne 0 ]; then
  not_run "go list ./internal/... ./cmd/... 失败（退出码 $rc）。
这不是「没有包 import relaykit」，是没跑成：
$(sed 's/^/   /' "$tmpdir/pkgs.err")"
fi

pkg_count=$(grep -c . "$tmpdir/pkgs" || true)
if [ "$pkg_count" -eq 0 ]; then
  not_run "go list 一个包都没列出来。
空集合时「没有包 import relaykit」与「一个包都没扫」在输出上分不出来，
所以这里不当作通过。"
fi
echo "  （在 $pkg_count 个包里查找）"

relaykit_count=0
while IFS= read -r p; do
  [ -n "$p" ] || continue
  "$GO" list -f '{{join .Imports "\n"}}' "$p" >"$tmpdir/imports" 2>"$tmpdir/imports.err"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    not_run "go list -f '{{join .Imports \"\\n\"}}' $p 失败（退出码 $rc）。
这个包的 import 关系没读到，不能当作「它没 import relaykit」：
$(sed 's/^/   /' "$tmpdir/imports.err")"
  fi
  if grep -q 'relaykit' "$tmpdir/imports"; then
    relaykit_count=$((relaykit_count + 1))
    rel=${p#"$MODULE"/}
    if ! printf '%s\n' "$ALLOWED_RELAYKIT_IMPORTERS" | grep -qx "$rel"; then
      echo "  ✗ $rel —— 不在允许清单里"
      fail=1
    else
      echo "  ✓ $rel"
    fi
  fi
done <"$tmpdir/pkgs"

# ── 规则二：金额运算只在 money 包里出现 ───────────────────────────────────
#
# 别的包若自己拿 float64 乘金额，那个舍入点就在 money 之外了，
# 而对账时不一致的舍入表现为一笔永远找不平的差额。
echo "── 除 money 外谁在算术里用 float64 处理金额"
# 先取原始命中、单独取退出码：`grep | grep | grep` 的管道里，第一个 grep
# 打不开目录时（退出码 2）会被后面的 grep 的 1 盖掉，那正是 LESSONS.md
# 第 2 条的形状（「没跑成」与「没命中」被一起吞掉）。
raw=$(grep -rn --include=*.go -E '\bfloat64\(' internal/ 2>"$tmpdir/float.err")
rc=$?
case "$rc" in
  0 | 1) ;;
  *)
    not_run "grep 扫描 internal/ 时执行失败（退出码 $rc）。
这不是「没有可疑行」，是没扫成：
$(sed 's/^/   /' "$tmpdir/float.err")"
    ;;
esac

suspicious=$(printf '%s\n' "$raw" \
  | grep -v '^internal/money/' \
  | grep -v '_test.go' \
  | grep -viE '//|rate|ratio|score|confidence|hit|percent|weight|latency|factor' || true)
suspicious_count=$(count_nonempty "$suspicious")
echo "  （在 $internal_files 个 .go 文件里查找，命中 $suspicious_count 行）"
if [ -n "$suspicious" ]; then
  printf '%s\n' "$suspicious" | sed 's/^/  ? /'
  echo "  （提示型，不参与退出码：逐条确认，是金额运算就该挪进 money 包）"
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
# 扫描条数是这一条的正向证据：只打印命中与否的话，一个因为路径写错而扫了
# 0 个文件的检查，输出与「干净」完全一样（LESSONS.md 第 16 条）。
scanned=$(grep -rl --include=*.go "\"$MODULE/internal/gateway\"" internal/ cmd/ 2>"$tmpdir/gateway.err")
rc=$?
if [ "$rc" -gt 1 ]; then
  not_run "grep 扫描 internal/ 与 cmd/ 时执行失败（退出码 $rc）——结果不可信：
$(sed 's/^/   /' "$tmpdir/gateway.err")"
fi
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
  done <<<"$(printf '%s\n' "$scanned" | xargs -r -n1 dirname | sort -u)"
fi

# ── 结论 ──────────────────────────────────────────────────────────────────
#
# 通过时也把正向证据打出来，理由见文件开头「正向证据」那一节。

echo
echo "三条都查过了：包 $pkg_count 个（其中 import relaykit 的 $relaykit_count 个）/ .go 文件 $go_files 个 / 可疑 $suspicious_count 行"
if [ "$fail" -ne 0 ]; then
  echo "分层自查不通过。"
  exit 1
fi
echo "分层自查通过。"
