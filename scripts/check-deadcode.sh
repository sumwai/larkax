#!/usr/bin/env bash
# 「新代码没有门」的门禁：从两个 main 包出发跑 deadcode，与签入仓库的白名单比对。
#
# # 为什么需要它
#
# 「先写库、后接线」在这个仓库里是刻意的顺序，而且是对的。问题是这个顺序上有一个
# 没人盯的接缝：库写完了、测绿了、合并了，而「谁来调它」这件事**没有任何一步会失败**。
# `go build` 不报错、`go vet` 不报错、测试全绿、覆盖率还涨了——于是
# **「有门」与「没门」在 CI 里长得一样**，只在有人真的去用那个功能时才暴露，
# 而那时看代码的人会以为它已经在生效。（LESSONS.md 第 22 条。）
#
# 这道门把「新出现的不可达符号」变成一次失败：要么接线，要么把它**显式**写进
# 白名单——那个「显式」是要被 review 看见的动作。
#
# 它**不是**「所有不可达符号都必须删掉」。`internal/money` 那类库构件先有先测是正常的，
# 它们进白名单天经地义。这道门管的是「悄悄多出来」。
#
# 扫描的起点是 `cmd/` 下的 main 包，**自动发现**（见 discover_main_packages）——
# 清单手写的话，漏掉一个就等于那个包整片没有门，而输出看起来完全正常。
# 每一轮都会打印实际扫了哪几个包。
#
# **边界（与 check-unimported.sh 共有）**：只发现 $SCAN_ROOT 下**一层**的目录，
# `cmd/foo/bar/main.go` 这种嵌套的 main 包不会被发现。两道门用的是同一份实现，
# 所以这个边界也是同一处；不单独修是为了让两边不会各自漂移。
#
# # 一个必须知道的盲区：整包没人 import 时，它根本不在分析范围里
#
# deadcode 只分析**从入口可达的那些包**。一个全仓库没有任何 import 方的包，
# 它内部的符号一个都不会出现在清单里——不是「都可达」，是**没被加载**。
# 实测：`internal/billing/calendaradapter` 在接线前是 0 条（接上之后仍是 0 条），
# `internal/plugin` 整包也是 0 条，而它们当时都没有生产调用方。
#
# 所以「清单里没有」有两种含义，从本门禁的输出上分不出来：
#   1. 这个包被入口可达，且它内部没有不可达符号；
#   2. 这个包压根没进分析范围。
#
# **这一条由 scripts/check-unimported.sh 补上**：它把「仓库自己的包」与
# 「从入口可达的包」两个集合相减，差集就是整包没人 import 的那些，
# 同样走白名单（scripts/unimported-allowlist.txt）。两道门合起来才盖住
# 「有实现、有测试、没有门」——一道管包内的符号，一道管包本身有没有接上。
#
# 要分清的是：**本文件白名单里那几条 `gap` 之所以有意义，是因为它们所在的
# 包本身是可达的**，只是包内某个符号没人调。整包不可达是另一回事，
# 那个包连这里都不会出现。
#
# # 用法
#
#   scripts/check-deadcode.sh
#
# 退出码：
#   0  白名单与现状一致（也可能有已经可以删掉的陈旧条目，见下——那不算失败）
#   1  门禁失败：有新出现的不可达符号、条目数超过上限、或白名单自身不合格式
#   2  没跑成：工具缺失、版本对不上、白名单文件不在、cmd/ 下扫不到 main 包、
#      deadcode 执行失败、输出认不出
#
# 1 与 2 必须分开：检查工具坏掉时的输出和「干净」长得一样，退出码也一样
#（LESSONS.md 第 16 条）。CI 里两者都是红的，但处置完全不同。
#
# # 两种「不一致」为什么处置不同
#
# **新出现的不可达符号**（在扫描结果里、不在白名单里）→ 失败。这是这道门要挡的东西：
# 库写完了、测绿了，而「谁来调它」这件事没有任何一步会失败。
#
# **陈旧的条目**（在白名单里、不在扫描结果里）→ 只提示，不算失败。它出现的**原因**
# 恰恰是「有人把没门的东西接上线了」——那是个好动作。让 CI 为它变红是一条会教人
# 学会忽略 CI 的假警报，而「白名单该清理了」不是一个必须拦住合并的问题。
#
# 「只许减不许增」由下面的 MAX_ENTRIES 守：真实条目数超过上限就失败。
#
# # 版本为什么钉住并校验
#
# deadcode 的输出随 golang.org/x/tools 的版本变化：换一个版本，符号集合就变，
# 白名单与基线都不可比。这与 gremlins 那次是同一个坑。所以脚本不但钉住版本，
# 还在每次运行时校验装的那一份对不对——版本不符时报错退出，而不是跑出一份
# 看起来像「新增了一批符号」的结果，让人去改白名单把它「修好」。
#
# CI 里 `go install` 的版本必须与下面的 PINNED_TOOLS_VERSION 一致
#（见 .github/workflows/ci.yml）。
#
# # 环境变量
#
# 这三个是给 scripts/check-deadcode.test.sh 注入桩件用的，CI 与本地都不该设：
#   DEADCODE          可执行文件，默认 deadcode
#   DEADCODE_VERSION  给定则跳过版本探测，直接当作探测到的版本
#   WHITELIST         白名单路径，默认 scripts/deadcode-allowlist.txt

set -uo pipefail
cd "$(dirname "$0")/.."

# 白名单条目数上限。**只许减不许增**：新增一条就要同时把这个数往上调，
# 而那是一次会被 review 看到的 diff。自测把这个字面量钉住（见
# check-deadcode.test.sh）——所以往上调要动两个文件，不会悄悄发生。
#
# 关掉一批缺口之后把它往下调。不调不会报错，但那样它就不再是当前存量的快照，
# 中间那一段差额可以被人无声地用掉。
MAX_ENTRIES=34

# deadcode 输出的条件之一。换版本 = 换符号集合 = 白名单不可比。
PINNED_TOOLS_VERSION="v0.52.0"

# 扫描起点的根目录。里面的 main 包自动发现，见下面的 discover_main_packages。
SCAN_ROOT="cmd"

WHITELIST="${WHITELIST:-scripts/deadcode-allowlist.txt}"
DEADCODE="${DEADCODE:-deadcode}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# ── 工具与版本 ────────────────────────────────────────────────────────────
#
# 「没装」与「装了但版本不对」都必须是响的：静默通过在这里等于没有这道门。

bin=$(command -v "$DEADCODE" 2>/dev/null) || not_run "找不到 $DEADCODE。
装一份，版本要与脚本里的 PINNED_TOOLS_VERSION 一致：
  go install golang.org/x/tools/cmd/deadcode@${PINNED_TOOLS_VERSION}
装完确认 \$(go env GOPATH)/bin 在 PATH 里。"

if [ -n "${DEADCODE_VERSION:-}" ]; then
  # 自测注入点：桩件是 bash 脚本，go version -m 探不出来。
  got_version="$DEADCODE_VERSION"
else
  got_version=$(go version -m "$bin" 2>/dev/null |
    awk '$1 == "mod" && $2 == "golang.org/x/tools" { print $3; exit }')
fi
if [ -z "$got_version" ]; then
  not_run "读不出 $bin 的版本（go 不在 PATH 里，或者它不是 go install 装出来的？）。
版本不同符号集合就不同，白名单不可比，所以这里不猜。装钉住的那一份：
  go install golang.org/x/tools/cmd/deadcode@${PINNED_TOOLS_VERSION}"
fi
if [ "$got_version" != "$PINNED_TOOLS_VERSION" ]; then
  not_run "deadcode 的版本与钉住的不一致，这一轮的结果不可比。
  钉住：${PINNED_TOOLS_VERSION}
  实际：${got_version}
  go install golang.org/x/tools/cmd/deadcode@${PINNED_TOOLS_VERSION}"
fi

echo "deadcode ${got_version}（钉住 ${PINNED_TOOLS_VERSION}）"

# ── 扫描起点 ──────────────────────────────────────────────────────────────
#
# 从 $SCAN_ROOT 里**自动发现** main 包，不手写清单。理由是这一条：
# 「新加了一个 main 包、没人记得补进清单，于是那个包整片没有门，而这一轮的
# 输出看起来完全正常（条数没变）」——那正是这道门自己要防的那类失败。
# **一道需要人记得同步维护的门，会以它要防的方式坏掉。**
#
# 认的是「非测试的 .go 文件里写着 package main」，也就是可执行包的定义。
# 不用 go list：脚本只依赖 deadcode 一个可执行文件，自测也就不必再桩一个 go。
discover_main_packages() {
  local root="$1" d f
  local files=()
  for d in "$root"/*/; do
    [ -d "$d" ] || continue
    files=()
    for f in "$d"*.go; do
      case "$f" in *_test.go) continue ;; esac
      [ -f "$f" ] && files+=("$f")
    done
    [ "${#files[@]}" -gt 0 ] || continue
    grep -qE '^package[[:space:]]+main[[:space:]]*$' "${files[@]}" || continue
    printf './%s\n' "${d%/}"
  done
}

TARGETS=()
while IFS= read -r pkg; do
  [ -n "$pkg" ] && TARGETS+=("$pkg")
done < <(discover_main_packages "$SCAN_ROOT")

# 一个都没发现时必须报错：没有扫描起点就等于什么都没检查，
# 而那个结果与「全部命中白名单」在屏幕上没有区别。
if [ "${#TARGETS[@]}" -eq 0 ]; then
  not_run "$SCAN_ROOT/ 下面没找到 main 包（认的是非测试 .go 文件里的 package main）。
没有扫描起点 = 什么都没检查，所以这里不当作通过。"
fi

# 打出来，「清单变了」才在 CI 日志里看得见。
echo "扫描起点 ${TARGETS[*]}"

# ── 白名单 ────────────────────────────────────────────────────────────────
#
# 格式与两档类型见 scripts/deadcode-allowlist.txt 的开头。
# 白名单自身坏掉时不能继续比：那样「比对」比的是半份清单，
# 而它的输出与「全部命中」长得一样。

[ -f "$WHITELIST" ] || not_run "白名单 $WHITELIST 不存在。"

wl_keys="$tmpdir/wl-keys"
: > "$wl_keys"
wl_count=0
wl_bad=0
lineno=0

# trim <字符串>：去掉首尾空白
trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line=$(trim "${raw%$'\r'}")
  [ -n "$line" ] || continue
  case "$line" in \#*) continue ;; esac

  IFS='|' read -r f_path f_sym f_kind f_reason f_extra <<<"$line"
  f_path=$(trim "$f_path")
  f_sym=$(trim "$f_sym")
  f_kind=$(trim "$f_kind")
  f_reason=$(trim "$f_reason")

  if [ -z "$f_path" ] || [ -z "$f_sym" ] || [ -z "$f_kind" ] || [ -z "$f_reason" ] ||
    [ -n "$(trim "$f_extra")" ]; then
    echo "✗ 白名单第 $lineno 行格式不对：应为「<路径> | <符号> | <类型> | <理由>」四栏，理由里不能有 |" >&2
    wl_bad=1
    continue
  fi
  case "$f_path" in
    *.go) ;;
    *)
      echo "✗ 白名单第 $lineno 行的路径不像 Go 源文件：$f_path" >&2
      wl_bad=1
      continue
      ;;
  esac
  case "$f_kind" in
    lib | gap) ;;
    *)
      echo "✗ 白名单第 $lineno 行的类型只能是 lib（库构件）或 gap（被追踪的缺口）：$f_kind" >&2
      wl_bad=1
      continue
      ;;
  esac
  # gap 必须挂着一个 issue：白名单不能变成垃圾桶，review 时要能问「这一条为什么还在」。
  if [ "$f_kind" = gap ] && ! grep -qE '#[0-9]+' <<<"$f_reason"; then
    echo "✗ 白名单第 $lineno 行是 gap，但理由里没有 issue 号（#123）：$f_reason" >&2
    wl_bad=1
    continue
  fi
  key="$f_path|$f_sym"
  if grep -qxF "$key" "$wl_keys"; then
    echo "✗ 白名单第 $lineno 行与前文重复：$key" >&2
    wl_bad=1
    continue
  fi
  printf '%s\n' "$key" >>"$wl_keys"
  wl_count=$((wl_count + 1))
done <"$WHITELIST"

if [ "$wl_bad" -ne 0 ]; then
  echo >&2
  echo "白名单 $WHITELIST 自身不合格式，先修它。" >&2
  exit 1
fi

echo "白名单 $WHITELIST —— $wl_count 条 / 上限 $MAX_ENTRIES"

if [ "$wl_count" -gt "$MAX_ENTRIES" ]; then
  echo
  echo "✗ 白名单 $wl_count 条，超过上限 $MAX_ENTRIES。"
  echo "  条目数只许减不许增：新增一条就要同时把 MAX_ENTRIES 往上调，"
  echo "  而那个改动会被 review 看到。要么接线，要么说清为什么必须加。"
  exit 1
fi

# ── 扫描 ──────────────────────────────────────────────────────────────────

dc_out="$tmpdir/deadcode.out"
dc_err="$tmpdir/deadcode.err"
"$DEADCODE" "${TARGETS[@]}" >"$dc_out" 2>"$dc_err"
rc=$?
if [ "$rc" -ne 0 ]; then
  echo "错误：deadcode 执行失败（退出码 $rc）。这不是「干净」，是没跑成：" >&2
  sed 's/^/   /' "$dc_out" "$dc_err" >&2
  exit 2
fi
# 成功的 deadcode 不往 stderr 写东西。写了说明这一轮有它自己才知道的状况，
# 而「有噪音但结论看着正常」正是最容易被放过的一类。
if [ -s "$dc_err" ]; then
  echo "错误：deadcode 往 stderr 写了东西，先看一眼再决定这一轮算不算数：" >&2
  sed 's/^/   /' "$dc_err" >&2
  exit 2
fi

# 输出的形状：`<路径>:<行>:<列>: unreachable func: <符号>`
# 认不出的行不能丢——那正是「工具换了输出格式」的样子，丢掉它之后就变成
# 「一个符号都没扫到」，而那个结果与「全部命中白名单」在屏幕上没有区别。
dc_keys="$tmpdir/dc-keys"
: > "$dc_keys"
dc_unparsed=0
while IFS= read -r line || [ -n "$line" ]; do
  [ -n "$line" ] || continue
  if [[ "$line" =~ ^([^:]+):[0-9]+:[0-9]+:\ unreachable\ (func|method):\ (.+)$ ]]; then
    printf '%s|%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[3]}" >>"$dc_keys"
  else
    echo "   $line" >&2
    dc_unparsed=$((dc_unparsed + 1))
  fi
done <"$dc_out"

if [ "$dc_unparsed" -ne 0 ]; then
  echo "错误：上面 $dc_unparsed 行认不出，不是 deadcode 该有的输出。" >&2
  echo "  工具换了版本或换了措辞时就是这样——不能把它当成「没有不可达符号」。" >&2
  exit 2
fi

sort -u -o "$dc_keys" "$dc_keys"
sort -u -o "$wl_keys" "$wl_keys"
dc_count=$(wc -l <"$dc_keys")

echo "不可达符号 $dc_count 个"

# ── 比对 ──────────────────────────────────────────────────────────────────

new=$(comm -23 "$dc_keys" "$wl_keys")
stale=$(comm -13 "$dc_keys" "$wl_keys")
bad=0

if [ -n "$new" ]; then
  bad=1
  echo
  echo "✗ 新出现的不可达符号 —— 要么接线，要么加进白名单："
  while IFS= read -r k; do
    echo "   $k"
  done <<<"$new"
  echo "  （加进白名单就要同时把 MAX_ENTRIES 往上调，那是一个会被 review 看到的动作）"
fi

# 陈旧条目只提示，不改退出码——见文件开头「两种不一致为什么处置不同」。
# 不吭声也不行：留着它的后果是这个符号将来重新变成不可达时被静默放行。
if [ -n "$stale" ]; then
  echo
  echo "· 白名单里这几条已经不在扫描结果里了，可以删掉了："
  while IFS= read -r k; do
    echo "   $k"
  done <<<"$stale"
fi

if [ "$bad" -ne 0 ]; then
  echo
  echo "白名单与现状不一致。"
  exit 1
fi

# 通过时也要给出正向证据：比了多少条、比的是什么。
# 「什么都没输出」与「什么都没检查」长得一样，而这道门的成功信号恰恰是沉默。
echo "全部命中白名单。"
echo "门禁通过。"
