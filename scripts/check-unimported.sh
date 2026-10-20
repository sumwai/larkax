#!/usr/bin/env bash
# 「整包没人 import」的门禁：把「仓库里的包」与「从入口可达的包」两个集合相减。
#
# # 为什么需要它
#
# scripts/check-deadcode.sh 从 cmd/ 下的 main 包出发跑 deadcode，挡住的是
# 「可达的包内部悄悄多了一个没人调的函数」。它挡不住另一种，而那一种更严重：
# **一个包全仓库没有任何 import 方**。deadcode 只分析从入口可达的那些包，
# 这样一个包**整包不在它的分析范围里**——它的不可达清单是空的，
# 而「空」与「全部可达」在输出里长得一模一样（LESSONS.md 第 22 条）。
#
# 实测（本门禁落地时）：internal/plugin 及其两个子包，全仓没有一处 import 它们，
# 而 deadcode 的清单里一条都没有。所以「清单里没有」有两种含义，
# 从那边分不出来：**被入口可达且内部没有不可达符号**，还是**压根没进分析范围**。
#
# 这道门问的是另一个问题，也是前一道门的补集：**这个包有人调吗？**
#
#   仓库自己的包（go list ./...）
#     − 从入口可达的包（go list -deps <cmd/ 下自动发现的 main 包>）
#     = 整包没人 import 的包
#
# 两道门合起来才盖住「有实现、有测试、没有门」：一道管包内的符号，
# 一道管包本身有没有接上。
#
# # 几处要想清楚的地方
#
# **入口是自动发现的**（见 discover_main_packages），与 check-deadcode.sh 是
# 同一份实现、同一个理由：一道需要人记得同步维护的门，会以它要防的方式坏掉——
# 新加一个 main 包而没人记得补进清单，那个包整片没有门，而这一轮的输出
# 看起来完全正常。每一轮都打印实际用了哪几个入口。
#
# **边界（与 check-deadcode.sh 共有）**：只发现 $SCAN_ROOT 下**一层**的目录，
# `cmd/foo/bar/main.go` 这种嵌套的 main 包不会被发现。两道门用的是同一份实现，
# 所以这个边界也是同一处；不单独修是为了让两边不会各自漂移。
#
# **`go list -deps` 的输出含标准库与第三方**，必须按模块前缀过滤掉，
# 否则「无人 import」里会混进几百个标准库包。模块路径取自 `go list -m`，
# 不写死在脚本里——写死的话换模块名（或换 fork）时，过滤会静默地一个都不匹配，
# 而那时两个集合都还在、输出看着正常，只是结论全错。
#
# **测试包不算独立包。** `go list ./...` 一个目录只给一个包；
# `./internal/foo [./internal/foo.test]` 那种形状是 `-test` 才有的，这里不传 `-test`。
# 输出里出现带空白或 `[` 的行按「认不出」处理（退出码 2），而不是当成包名收下。
#
# **只被测试 import 的包算「没人 import」。** 这是刻意的：测试自己就能构造调用方，
# 所以「只有测试在调」与「生产在调」在覆盖率上长得一模一样（LESSONS.md 第 22 条）。
# `go list -deps` 不跟随测试 import，于是这件事不需要额外判据——它天然落在差集里。
# 代价是一批**测试夹具包**会被报出来，所以白名单里要给它们留位置。
#
# **入口包自己不需要特判。** `go list -deps` 把命令行上点名的包也算作依赖，
# 所以 ./cmd/larkax 天然同时出现在两个集合里，不会把自己报成「没人 import」。
#
# **`go list` 往 stderr 写东西不等于失败。** 需要下载模块时它会写
# `go: downloading …` 这样的进度。判失败的依据是**退出码**与**输出能不能解析**，
# 不是 stderr 空不空——按 stderr 判会把「第一次在干净环境里跑」变成一次假警报，
# 而假警报教人学会忽略 CI（LESSONS.md 第 13 条）。stderr 非空时照打出来，
# 让它在日志里可见，但不改退出码。
#
# **集合为空必须报「没跑成」。** 可达集合为空时，差集就是「仓库里的全部包」——
# 那看起来像一次大发现，不像一次故障；而仓库集合为空时差集为空，
# 看起来像「全部接上了」。两种都不许当作通过。
#
# # 白名单
#
# scripts/unimported-allowlist.txt，格式与两档类型见该文件开头。口径与
# check-deadcode.sh 一致：
#
#   **新出现的无人 import 包**（在差集里、不在白名单里）→ 失败。这是这道门
#   要挡的东西：库写完了、测绿了，而「谁来调它」这件事没有任何一步会失败。
#   **陈旧的条目**（在白名单里、已经可达）→ 只提示，不算失败。它出现的原因
#   恰恰是「有人把没门的包接上线了」，那是个好动作。
#
# **不要为了把清单刷空而去硬接调用方**——那是这道门最坏的一种「修法」：
# 接一条假的调用方比留着它更坏（LESSONS.md 第 22 条）。
#
# # 用法
#
#   scripts/check-unimported.sh
#
# 退出码：
#   0  两个集合相减的结果与白名单一致（也可能有已经可以删掉的陈旧条目，见上——那不算失败）
#   1  门禁失败：有新出现的无人 import 包、条目数超过上限、或白名单自身不合格式
#   2  没跑成：go 不在、go list 失败、某个集合是空的、输出认不出、
#      cmd/ 下扫不到 main 包
#
# 1 与 2 必须分开：检查工具坏掉时的输出和「干净」长得一样，退出码也一样
#（LESSONS.md 第 16 条）。CI 里两者都是红的，但处置完全不同。
#
# # 环境变量
#
# 这三个是给 scripts/check-unimported.test.sh 注入桩件用的，CI 与本地都不该设：
#   GO          可执行文件，默认 go
#   WHITELIST   白名单路径，默认 scripts/unimported-allowlist.txt
#   MAX_ENTRIES 条目数上限，默认 MAX_ENTRIES_DEFAULT（见下）

set -uo pipefail
cd "$(dirname "$0")/.."

# 白名单条目数上限。**只许减不许增**：新增一条就要同时把这个数往上调，
# 而那是一次会被 review 看到的 diff。自测把这个字面量钉住（见
# check-unimported.test.sh）——所以往上调要动两个文件，不会悄悄发生。
#
# 关掉一批缺口之后把它往下调。不调不会报错，但那样它就不再是当前存量的快照，
# 中间那一段差额可以被人无声地用掉。
#
# 取值走 MAX_ENTRIES_DEFAULT 而不是直接写 MAX_ENTRIES：自测的比较用例用的是
# 自己造的小白名单，条数与真实存量无关，所以它们要能把这个上限顶开——否则
# 真实上限降到比夹具还小时，那些用例会全部挂在「超过上限」上，而它们要测的
# 是比对逻辑。上限本身由自测的第 4、5、18 条钉住：那三条按下面这个默认值跑。
MAX_ENTRIES_DEFAULT=3
MAX_ENTRIES="${MAX_ENTRIES:-$MAX_ENTRIES_DEFAULT}"

# 扫描起点的根目录。里面的 main 包自动发现，见下面的 discover_main_packages。
SCAN_ROOT="cmd"

WHITELIST="${WHITELIST:-scripts/unimported-allowlist.txt}"
GO="${GO:-go}"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# 没跑成。与「门禁失败」（退出码 1）分开：这两者的处置不一样。
not_run() {
  echo "错误：$*" >&2
  exit 2
}

# ── 工具 ──────────────────────────────────────────────────────────────────

go_bin=$(command -v "$GO" 2>/dev/null) || not_run "找不到 $GO（go 不在 PATH 里）。
这道门不依赖 deadcode 之类的外部工具，只需要 go——但 go 不在时它什么都没检查，
所以这里不当作通过。"

# ── 扫描起点 ──────────────────────────────────────────────────────────────
#
# 从 $SCAN_ROOT 里**自动发现** main 包，不手写清单。理由是这一条：
# 「新加了一个 main 包、没人记得补进清单，于是那个包整片没有门，而这一轮的
# 输出看起来完全正常（条数没变）」——那正是这道门自己要防的那类失败。
#
# 认的是「非测试的 .go 文件里写着 package main」，也就是可执行包的定义，
# 与 check-deadcode.sh 里那份是同一份实现。
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

# 一个都没发现时必须报错：没有入口 = 可达集合为空 = 差集变成「仓库里的全部包」，
# 那看起来像一次大发现，不像一次故障。
if [ "${#TARGETS[@]}" -eq 0 ]; then
  not_run "$SCAN_ROOT/ 下面没找到 main 包（认的是非测试 .go 文件里的 package main）。
没有入口就没有「可达」这一侧，差集会把整个仓库报成没人 import，
所以这里不当作通过。"
fi

echo "扫描起点 ${TARGETS[*]}"

# ── 三次 go list ──────────────────────────────────────────────────────────

# run_go_list <说明> <输出文件> <go list 的参数…>
run_go_list() {
  local desc="$1" out="$2"
  shift 2
  local err="$out.err" rc
  "$go_bin" list "$@" >"$out" 2>"$err"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "错误：go list $desc 失败（退出码 $rc）。这不是「干净」，是没跑成：" >&2
    sed 's/^/   /' "$out" "$err" >&2
    exit 2
  fi
  if [ -s "$err" ]; then
    # 不是失败判据——见文件开头「go list 往 stderr 写东西不等于失败」。
    echo "· go list $desc 往 stderr 写了内容（多为模块下载进度，不是失败）："
    sed 's/^/   /' "$err"
  fi
  if [ ! -s "$out" ]; then
    not_run "go list $desc 一行都没输出。
空集合会让差集变成「仓库里的全部包」或者「一个都没有」，两种看起来都像结论——
所以这里不当作通过。"
  fi
}

# parse_paths <原始输出> <结果文件> <说明>
#
# 认不出的行不能丢：那正是「工具换了措辞或换了参数」的样子（比如有人给它加上
# `-json` 或 `-test`）。丢掉之后这一轮就变成两个集合都少了一批，
# 而结果看起来像「差异变小了」。
parse_paths() {
  local in="$1" out="$2" desc="$3" line bad=0
  : >"$out"
  while IFS= read -r line || [ -n "$line" ]; do
    [ -n "$line" ] || continue
    if [[ "$line" =~ [[:space:]] || "$line" == *"["* || "$line" == *:* ]]; then
      echo "   $line" >&2
      bad=$((bad + 1))
      continue
    fi
    printf '%s\n' "$line" >>"$out"
  done <"$in"
  if [ "$bad" -ne 0 ]; then
    not_run "上面 $bad 行认不出，不是 go list 该有的输出（$desc）。
工具换了措辞或换了参数时就是这样——不能把它当成「没有差异」。"
  fi
  sort -u -o "$out" "$out"
}

# 模块路径：用来把标准库与第三方从 -deps 的输出里滤掉。
mod_out="$tmpdir/mod"
run_go_list "（模块路径）" "$mod_out" -m
mod_lines=$(wc -l <"$mod_out")
MOD=$(head -n1 "$mod_out")
if [ "$mod_lines" -ne 1 ] || [ -z "$MOD" ] || [[ "$MOD" =~ [[:space:]] ]]; then
  echo "错误：go list -m 的输出认不出（$mod_lines 行）：" >&2
  sed 's/^/   /' "$mod_out" >&2
  exit 2
fi
echo "模块 $MOD"

deps_raw="$tmpdir/deps.raw"
run_go_list "（从入口可达的依赖）" "$deps_raw" -deps "${TARGETS[@]}"

pkgs_raw="$tmpdir/pkgs.raw"
run_go_list "（仓库自己的包）" "$pkgs_raw" ./...

deps_all="$tmpdir/deps.all"
parse_paths "$deps_raw" "$deps_all" "从入口可达的依赖"
deps_total=$(wc -l <"$deps_all")

# 按模块前缀过滤。`$0 == mod` 与 `index(...) == 1` 两分支都要有：
# 只写前缀匹配的话，模块根目录下的那个包（import path 恰好等于模块路径）
# 会被滤掉，而它是最该被看见的一个。
awk -v mod="$MOD" '$0 == mod || index($0, mod "/") == 1' "$deps_all" >"$tmpdir/reach.abs"
sort -u -o "$tmpdir/reach.abs" "$tmpdir/reach.abs"
reach_abs_count=$(wc -l <"$tmpdir/reach.abs")
if [ "$reach_abs_count" -eq 0 ]; then
  not_run "按模块前缀 $MOD 过滤之后，可达集合里一个本仓库的包都没有。
模块路径读错、或者入口根本没 import 自己的任何包时就是这样——两种都不该当作通过。"
fi

repo_abs="$tmpdir/repo.abs"
parse_paths "$pkgs_raw" "$repo_abs" "仓库自己的包"

# `go list ./...` 会把 **node_modules 里自带的 Go 包**当成仓库自己的包收进来
# （实测：某个 npm 依赖带了 `golang/` 实现）。它们不是本仓库的源码：既不该接线，
# 也不该进白名单。不滤掉的话，跑过 `npm install` 的工作树每次都会多出一条
# 「新出现的整包没人 import」，而干净的 CI 上是绿的——两边各说各话，
# 最后教人学会无视这道门（LESSONS.md 第 13 条）。
#
# **滤掉几条要打印出来**：安静地滤掉与「本来就没有」长得一样，
# 而后者才是干净的形状。
repo_abs_all="$tmpdir/repo-all.abs"
cp "$repo_abs" "$repo_abs_all"
awk 'index($0, "/node_modules/") == 0' "$repo_abs_all" >"$repo_abs"
skipped_nm=$(( $(wc -l <"$repo_abs_all") - $(wc -l <"$repo_abs") ))
if [ "$skipped_nm" -gt 0 ]; then
  echo "· 跳过 $skipped_nm 个 node_modules 下的包（依赖自带的 Go 代码，不是本仓库的源码）"
fi

# 模块前缀必须真的剥得掉。剥不掉时白名单里的相对路径一条都对不上，
# 于是整个仓库会被报成「新出现的无人 import」——一次假的大发现。
stripped=$(awk -v mod="$MOD" '$0 == mod || index($0, mod "/") == 1' "$repo_abs" | wc -l)
if [ "$stripped" -eq 0 ]; then
  not_run "go list ./... 输出的包没有一个带模块前缀 $MOD。
模块路径读错了，或者 go list 的输出形状变了——两种都不该当作通过。"
fi

# relpaths <模块路径> <输入> <输出>：剥掉模块前缀，与白名单里的相对路径对齐。
# 白名单存相对路径，这样换模块名（或换 fork）时它不会整体失效。
relpaths() {
  awk -v mod="$1" '
    { if ($0 == mod) print "."; else if (index($0, mod "/") == 1) print substr($0, length(mod) + 2); else print $0 }
  ' "$2" >"$3"
}

reach="$tmpdir/reach.rel"
repo="$tmpdir/repo.rel"
relpaths "$MOD" "$tmpdir/reach.abs" "$reach"
relpaths "$MOD" "$repo_abs" "$repo"
repo_count=$(wc -l <"$repo")

# ── 白名单 ────────────────────────────────────────────────────────────────
#
# 格式与两档类型见 scripts/unimported-allowlist.txt 的开头。
# 白名单自身坏掉时不能继续比：那样「比对」比的是半份清单，
# 而它的输出与「全部命中」长得一样。

[ -f "$WHITELIST" ] || not_run "白名单 $WHITELIST 不存在。"

wl_keys="$tmpdir/wl-keys"
: >"$wl_keys"
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

  IFS='|' read -r f_pkg f_kind f_reason f_extra <<<"$line"
  f_pkg=$(trim "$f_pkg")
  f_kind=$(trim "$f_kind")
  f_reason=$(trim "$f_reason")

  if [ -z "$f_pkg" ] || [ -z "$f_kind" ] || [ -z "$f_reason" ] ||
    [ -n "$(trim "$f_extra")" ]; then
    echo "✗ 白名单第 $lineno 行格式不对：应为「<包路径> | <类型> | <理由>」三栏，理由里不能有 |" >&2
    wl_bad=1
    continue
  fi
  case "$f_pkg" in
    /* | *..*)
      echo "✗ 白名单第 $lineno 行的包路径必须是模块内的相对路径（如 internal/plugin）：$f_pkg" >&2
      wl_bad=1
      continue
      ;;
  esac
  if ! grep -qE '^[A-Za-z0-9._~/-]+$' <<<"$f_pkg"; then
    echo "✗ 白名单第 $lineno 行的包路径里有非法字符：$f_pkg" >&2
    wl_bad=1
    continue
  fi
  case "$f_kind" in
    lib | gap) ;;
    *)
      echo "✗ 白名单第 $lineno 行的类型只能是 lib（库构件 / 测试夹具）或 gap（被追踪的缺口）：$f_kind" >&2
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
  if grep -qxF "$f_pkg" "$wl_keys"; then
    echo "✗ 白名单第 $lineno 行与前文重复：$f_pkg" >&2
    wl_bad=1
    continue
  fi
  printf '%s\n' "$f_pkg" >>"$wl_keys"
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

sort -u -o "$wl_keys" "$wl_keys"

# ── 比对 ──────────────────────────────────────────────────────────────────

comm -23 "$repo" "$reach" >"$tmpdir/unimported"
unimported_count=$(wc -l <"$tmpdir/unimported")

# 通过时也要给出正向证据：比了多少个包、哪些包被判为可达、差集是哪些。
# 「什么都没输出」与「什么都没检查」长得一样，而这道门的成功信号恰恰是沉默。
echo
echo "仓库自己的包 $repo_count 个：入口可达 $reach_abs_count 个，无人 import $unimported_count 个"
echo
echo "入口可达（$reach_abs_count 个）："
entry_list=""
for t in "${TARGETS[@]}"; do
  entry_list+="${t#./} "
done
while IFS= read -r p; do
  [ -n "$p" ] || continue
  mark=""
  case " $entry_list " in *" $p "*) mark="   ← 入口" ;; esac
  printf '  %s%s\n' "$p" "$mark"
done <"$reach"

echo
if [ "$unimported_count" -eq 0 ]; then
  echo "无人 import 的包：0 个。"
else
  echo "无人 import 的包（$unimported_count 个，理由见 $WHITELIST）："
  sed 's/^/  /' "$tmpdir/unimported"
fi

new=$(comm -23 "$tmpdir/unimported" "$wl_keys")
stale=$(comm -13 "$tmpdir/unimported" "$wl_keys")
bad=0

if [ -n "$new" ]; then
  bad=1
  echo
  echo "✗ 新出现的「整包没人 import」 —— 要么接线，要么加进白名单："
  while IFS= read -r k; do
    echo "   $k"
  done <<<"$new"
  echo "  （加进白名单就要同时把 MAX_ENTRIES 往上调，那是一个会被 review 看到的动作）"
fi

# 陈旧条目只提示，不改退出码——见文件开头「白名单」那一节。
# 不吭声也不行：留着它的后果是这个包将来重新变成无人 import 时被静默放行。
if [ -n "$stale" ]; then
  echo
  echo "· 白名单里这几个包已经可达了，可以删掉了："
  while IFS= read -r k; do
    echo "   $k"
  done <<<"$stale"
fi

if [ "$bad" -ne 0 ]; then
  echo
  echo "白名单与现状不一致。"
  exit 1
fi

echo
echo "全部命中白名单。"
echo "门禁通过。"
