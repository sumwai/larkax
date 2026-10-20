#!/usr/bin/env bash
# check-deadcode.sh 的自测。
#
# 为什么需要它：这道门的判据全是「集合比对」，而它坏掉时的输出和「通过」长得
# 一模一样——一个都没解析出来时，屏幕上就是「不可达符号 0 个，全部命中白名单」。
# 这与 LESSONS.md 第 16 条记的是同一个形状（检查工具「没跑成」时输出和
# 「跑成了」一样），而第 2 条更具体：红线扫描器曾经自己报「通过」，
# 实际根本没在检查。所以正反两面都要钉住。
#
# **本自测不调用真的 deadcode**：它要编译整个程序、吃内存，而这里要测的正是
# 「deadcode 输出长什么样时脚本怎么判」——所以造一个假的 deadcode
#（一个普通 bash 脚本，chmod +x），用 DEADCODE= 注入；版本探测同样用
# DEADCODE_VERSION= 注入（桩件是脚本，go version -m 探不出来）。
# 与 check-mutation.test.sh 用桩件而不是真跑 gremlins 是同一个做法：快、可重复。
#
# 改动 check-deadcode.sh、白名单格式或那个上限常量之后跑：
#   bash scripts/check-deadcode.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-deadcode.sh"
REAL_WHITELIST="$PWD/scripts/deadcode-allowlist.txt"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

last_out="$tmp/last.txt"
ok() { pass=$((pass + 1)); }
bad() {
  echo "✗ $1"
  sed 's/^/    | /' "$last_out" | tail -8
  fail=$((fail + 1))
}

# 脚本里钉住的版本：自测不写死它，否则改一处要动两处。
PINNED=$(grep -m1 '^PINNED_TOOLS_VERSION=' "$SCRIPT" | cut -d'"' -f2)
if [ -z "$PINNED" ]; then
  echo "✗ 读不出 $SCRIPT 里的 PINNED_TOOLS_VERSION"
  exit 1
fi

# ── 桩件 ──────────────────────────────────────────────────────────────────

# make_deadcode <路径> <输出文件> [退出码] [stderr 内容文件]
# 输出文件照抄真 deadcode 的形状：`<路径>:<行>:<列>: unreachable func: <符号>`
make_deadcode() {
  local path="$1" out="$2" rc="${3:-0}" err="${4:-}"
  {
    echo '#!/usr/bin/env bash'
    echo '# 桩件：check-deadcode.sh 的自测用，不是真的 deadcode'
    echo "cat '$out'"
    if [ -n "$err" ]; then echo "cat '$err' >&2"; fi
    echo "exit $rc"
  } >"$path"
  chmod +x "$path"
}

# run <桩件> <白名单> [版本]
# 退出码由调用者立刻取走：$? 必须在下一句就读。
run() {
  local stub="$1" wl="$2" ver="${3:-$PINNED}"
  DEADCODE="$stub" WHITELIST="$wl" DEADCODE_VERSION="$ver" \
    bash "$SCRIPT" >"$last_out" 2>&1
}

# 把一份白名单翻成 deadcode 会打印的形状，用于「这份白名单本身能不能过」。
whitelist_to_deadcode() {
  awk -F'|' '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      a = $1; b = $2
      gsub(/^[[:space:]]+/, "", a); gsub(/[[:space:]]+$/, "", a)
      gsub(/^[[:space:]]+/, "", b); gsub(/[[:space:]]+$/, "", b)
      print a ":" NR ":1: unreachable func: " b
    }
  ' "$1"
}

# ── 夹具 ──────────────────────────────────────────────────────────────────

wl_ok="$tmp/wl-ok.txt"
cat >"$wl_ok" <<'EOF'
# 用例用的小白名单：一条 lib、一条 gap
internal/foo/a.go | Alpha | lib | 库构件，先有先测
internal/foo/b.go | Beta  | gap | #1 还没有接线
EOF

dc_ok="$tmp/dc-ok.txt"
cat >"$dc_ok" <<'EOF'
internal/foo/a.go:10:6: unreachable func: Alpha
internal/foo/b.go:20:6: unreachable func: Beta
EOF

stub_ok="$tmp/deadcode-ok"
make_deadcode "$stub_ok" "$dc_ok"

# make_deadcode_recording <路径> <输出文件>
# 与 make_deadcode 相同，但把**收到的参数**记在 <路径>.args 里，供自测断言
# 「这个包真的被传给了 deadcode」，而不只是被打印出来。
make_deadcode_recording() {
  local path="$1" out="$2"
  {
    printf '%s\n' '#!/usr/bin/env bash'
    printf '%s\n' '# 桩件：把收到的参数记在 <自己>.args 里，供自测断言'
    printf '%s\n' 'printf "%s\n" "$*" > "$0.args"'
    printf "cat '%s'\n" "$out"
  } >"$path"
  chmod +x "$path"
}

# make_fake_repo <目标目录> [main 包名...]
# 造一个假的仓库根：只有 scripts/check-deadcode.sh 与 cmd/<包名>/main.go。
# 扫描起点是自动发现的，所以只能这样验——在真仓库里塞一个 cmd/ 会污染工作树。
make_fake_repo() {
  local repo="$1"
  shift
  mkdir -p "$repo/scripts" "$repo/cmd"
  cp "$SCRIPT" "$repo/scripts/check-deadcode.sh"
  local name
  for name in "$@"; do
    mkdir -p "$repo/cmd/$name"
    printf 'package main\n\nfunc main() {}\n' >"$repo/cmd/$name/main.go"
  done
}

# run_in_repo <仓库根> <桩件> <白名单> [版本]
run_in_repo() {
  local repo="$1" stub="$2" wl="$3" ver="${4:-$PINNED}"
  DEADCODE="$stub" WHITELIST="$wl" DEADCODE_VERSION="$ver" \
    bash "$repo/scripts/check-deadcode.sh" >"$last_out" 2>&1
}

# ── 用例 ──────────────────────────────────────────────────────────────────

# 1. 全部命中白名单：通过。**通过时必须有正向证据**——比了多少条、比的是什么。
#    「什么都没输出」与「什么都没检查」长得一样，而这道门的成功信号恰恰是沉默。
run "$stub_ok" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "全部命中 —— 应通过，实际退出码 $rc"; fi
grep -q '全部命中白名单' "$last_out" || bad "全部命中 —— 通过时要说清「全部命中」"
grep -q '不可达符号 2 个' "$last_out" || bad "全部命中 —— 通过时要报出比对了多少条"

# 2. 出现一个不在白名单里的不可达符号：退出码非 0，并把它打印出来。
#    这是这道门存在的理由：新代码没有门时不能悄悄过去。
dc_new="$tmp/dc-new.txt"
cp "$dc_ok" "$dc_new"
echo 'internal/foo/c.go:5:1: unreachable func: Gamma' >>"$dc_new"
stub_new="$tmp/deadcode-new"
make_deadcode "$stub_new" "$dc_new"
run "$stub_new" "$wl_ok"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "新符号 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'internal/foo/c.go|Gamma' "$last_out" || bad "新符号 —— 要逐条打印出来，让人知道该加白名单还是该接线"
grep -q '要么接线' "$last_out" || bad "新符号 —— 提示里要给出两条路"

# 3. 白名单里有已不存在的条目：**打印出来，但退出码仍然是 0**。
#    它出现的原因恰恰是「有人把没门的东西接上线了」——那是个好动作，
#    让 CI 为它变红是一条会教人学会忽略 CI 的假警报，而「白名单该清理了」
#    不是一个必须拦住合并的问题。所以这一条与第 2 条是**相反**的判据：
#    一边钉「新符号必须拦住」，一边钉「陈旧条目不许拦」。
#    留着不吭声也不行：那会让这个符号将来重新变成不可达时被静默放行。
wl_stale="$tmp/wl-stale.txt"
cp "$wl_ok" "$wl_stale"
echo 'internal/foo/c.go | Gamma | gap | #1 已经接上了' >>"$wl_stale"
run "$stub_ok" "$wl_stale"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "陈旧条目 —— 不该拦住合并（应退出码 0），实际 $rc"; fi
grep -q 'internal/foo/c.go|Gamma' "$last_out" || bad "陈旧条目 —— 要报出是哪一条"
grep -q '可以删掉' "$last_out" || bad "陈旧条目 —— 要说清这一条已经可以删了"

# 3b. 新符号与陈旧条目同时出现：仍然是失败，而且两件事都要报出来。
#     陈旧的那一半只提示，不能把真正该拦的那一半盖过去。
wl_both="$tmp/wl-both.txt"
cat >"$wl_both" <<'EOF'
internal/foo/a.go | Alpha | lib | 库构件，先有先测
internal/foo/c.go | Gamma | gap | #1 已经接上了
EOF
dc_both="$tmp/dc-both.txt"
cat >"$dc_both" <<'EOF'
internal/foo/a.go:10:6: unreachable func: Alpha
internal/foo/b.go:20:6: unreachable func: Beta
EOF
stub_both="$tmp/deadcode-both"
make_deadcode "$stub_both" "$dc_both"
run "$stub_both" "$wl_both"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "新符号 + 陈旧条目 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'internal/foo/b.go|Beta' "$last_out" || bad "新符号 + 陈旧条目 —— 新符号要报出来"
grep -q 'internal/foo/c.go|Gamma' "$last_out" || bad "新符号 + 陈旧条目 —— 陈旧条目也要报出来"

# 4. 白名单条目数超过上限：退出码非 0。
#    上限从脚本里读，所以这条用例不依赖那个具体数值。
declared=$(grep -m1 '^MAX_ENTRIES=' "$SCRIPT" | cut -d= -f2)
if [ -z "$declared" ]; then
  bad "读不出 $SCRIPT 里的 MAX_ENTRIES"
else
  wl_over="$tmp/wl-over.txt"
  dc_over="$tmp/dc-over.txt"
  : >"$wl_over"
  : >"$dc_over"
  i=0
  while [ "$i" -le "$declared" ]; do
    printf 'internal/foo/f%d.go | F%d | gap | #1 还没有接线\n' "$i" "$i" >>"$wl_over"
    printf 'internal/foo/f%d.go:1:1: unreachable func: F%d\n' "$i" "$i" >>"$dc_over"
    i=$((i + 1))
  done
  stub_over="$tmp/deadcode-over"
  make_deadcode "$stub_over" "$dc_over"
  run "$stub_over" "$wl_over"
  rc=$?
  if [ "$rc" -eq 1 ]; then ok; else bad "条目数超过上限（$declared + 1）—— 应失败（退出码 1），实际 $rc"; fi
  grep -q '超过上限' "$last_out" || bad "条目数超过上限 —— 要说清是这个原因"
  grep -q '只许减不许增' "$last_out" || bad "条目数超过上限 —— 要说明「只许减不许增」这条规矩"
fi

# 5. 上限是个具名常量，自测把它钉住。
#    往上调必须同时改这一处，于是那是一次会被 review 看到的 diff。
#    关掉一批缺口之后，把脚本里的常量与这里的字面量一起往下调。
if [ "$declared" = "23" ]; then ok; else bad "MAX_ENTRIES 被改成了 $declared（自测钉住的是 23）"; fi

# 6. deadcode 不在 PATH：退出码 2，并给出安装命令。
#    用一个确定不存在的名字，避免依赖运行环境里有没有装 deadcode。
DEADCODE="deadcode-not-installed-$$" WHITELIST="$wl_ok" \
  bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "deadcode 不在 PATH —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'go install' "$last_out" || bad "deadcode 不在 PATH —— 要给出安装命令"

# 7. deadcode 自身退出码非 0：退出码 2，不能当成「干净」。
stub_rc1="$tmp/deadcode-rc1"
make_deadcode "$stub_rc1" "$dc_ok" 1
run "$stub_rc1" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "deadcode 退出码非 0 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '执行失败' "$last_out" || bad "deadcode 退出码非 0 —— 要报出是执行失败，不是「干净」"

# 8. deadcode 往 stderr 写了东西：退出码 2。
#    成功的 deadcode 不往 stderr 写。有噪音但结论看着正常，是最容易被放过的一类。
err_file="$tmp/deadcode.err.txt"
printf '%s\n' 'warning: 有 3 个包加载失败，已跳过' >"$err_file"
stub_noisy="$tmp/deadcode-noisy"
make_deadcode "$stub_noisy" "$dc_ok" 0 "$err_file"
run "$stub_noisy" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "stderr 非空 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'stderr' "$last_out" || bad "stderr 非空 —— 要说清是 stderr 有内容"

# 9. 输出格式变了、认不出：退出码 2。
#    **解析不到任何东西时必须往失败一侧倒**——此时脚本什么都没在判断，
#    而它的输出（「不可达符号 0 个，全部命中白名单」）与真的干净没有区别。
dc_unknown="$tmp/dc-unknown.txt"
printf '%s\n' 'unreachable funcs in this module:' '  Alpha (internal/foo/a.go)' >"$dc_unknown"
stub_unknown="$tmp/deadcode-unknown"
make_deadcode "$stub_unknown" "$dc_unknown"
run "$stub_unknown" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "认不出的输出 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '认不出' "$last_out" || bad "认不出的输出 —— 要报出是「认不出」，而不是「0 个不可达符号」"

# 10. 版本与钉住的不一致：退出码 2。
#     版本不同符号集合就不同，这一轮的「新增」全是假的，而人可能就去改白名单
#     把它「修好」——那正好把基线污染了。
run "$stub_ok" "$wl_ok" "v0.0.0-test"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "版本不符 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q "$PINNED" "$last_out" || bad "版本不符 —— 要报出钉住的是哪个版本"
grep -q 'v0.0.0-test' "$last_out" || bad "版本不符 —— 要报出实际探测到的是哪个版本"

# 11. 白名单格式不对（栏数不足）：退出码 1，并指出是第几行。
wl_badfmt="$tmp/wl-badfmt.txt"
printf '%s\n' 'internal/foo/a.go | Alpha | lib' >"$wl_badfmt"
run "$stub_ok" "$wl_badfmt"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单格式不对 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '第 1 行' "$last_out" || bad "白名单格式不对 —— 要指出是第几行"

# 12. 类型不认识：退出码 1。
wl_badkind="$tmp/wl-badkind.txt"
printf '%s\n' 'internal/foo/a.go | Alpha | maybe | 说不清' >"$wl_badkind"
run "$stub_ok" "$wl_badkind"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "类型不认识 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'lib' "$last_out" || bad "类型不认识 —— 要说出合法取值是哪两个"

# 13. gap 条目没有 issue 号：退出码 1。
#     白名单不能变成垃圾桶：gap 的每一条都要能回答「这一条为什么还在」。
wl_noissue="$tmp/wl-noissue.txt"
printf '%s\n' 'internal/foo/a.go | Alpha | gap | 反正暂时留着' >"$wl_noissue"
run "$stub_ok" "$wl_noissue"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "gap 没有 issue 号 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'issue 号' "$last_out" || bad "gap 没有 issue 号 —— 要说清缺的是什么"

# 14. 签入仓库的那份白名单本身要能过。
#     桩件按真白名单生成输出，于是这一条同时验了：文件格式、两档类型、
#     gap 都挂着 issue 号、条目数没超上限、键能原样往返。
wl_real_dc="$tmp/real-wl-as-deadcode.txt"
whitelist_to_deadcode "$REAL_WHITELIST" >"$wl_real_dc"
stub_real="$tmp/deadcode-real"
make_deadcode "$stub_real" "$wl_real_dc"
run "$stub_real" "$REAL_WHITELIST"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "签入的白名单 —— 应通过，实际退出码 $rc"; fi
real_count=$(wc -l <"$wl_real_dc")
grep -q "不可达符号 $real_count 个" "$last_out" ||
  bad "签入的白名单 —— 应报出比对了 $real_count 条"

# 15. 扫描起点是**自动发现**的，不是手写清单。
#     手写清单会以它要防的方式坏掉：新加一个 main 包、没人记得补进来，于是那个包
#     整片没有门，而这一轮的输出看起来完全正常。这里造一个假仓库（不动真仓库），
#     验三件事：带 package main 的目录被发现、不带的不被当成起点、
#     发现的包**真的传给了 deadcode**（而不只是被打印出来）。
repo_new="$tmp/repo-new"
make_fake_repo "$repo_new" alpha beta
mkdir -p "$repo_new/cmd/notmain"
printf 'package notmain\n' >"$repo_new/cmd/notmain/lib.go"
stub_rec="$tmp/deadcode-rec"
make_deadcode_recording "$stub_rec" "$dc_ok"
run_in_repo "$repo_new" "$stub_rec" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "自动发现扫描起点 —— 应通过，实际退出码 $rc"; fi
grep -q '扫描起点 ./cmd/alpha ./cmd/beta' "$last_out" ||
  bad "自动发现扫描起点 —— 发现的包要打印出来（一行），让「清单变了」在日志里看得见"
if grep -q 'notmain' "$last_out"; then
  bad "自动发现扫描起点 —— 不是 main 包的目录不该进扫描起点"
else
  ok
fi
rec_args=$(cat "$stub_rec.args" 2>/dev/null || echo "（没记到参数）")
case "$rec_args" in
  *./cmd/alpha*./cmd/beta*) ok ;;
  *) bad "自动发现扫描起点 —— 发现的包要真的传给 deadcode（收到：$rec_args）" ;;
esac

# 16. 一个 main 包都没发现：退出码 2。
#     「没有扫描起点」= 什么都没检查，而那个结果与「全部命中白名单」长得一样。
repo_empty="$tmp/repo-empty"
make_fake_repo "$repo_empty"
run_in_repo "$repo_empty" "$stub_ok" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫不到 main 包 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '没找到 main 包' "$last_out" || bad "扫不到 main 包 —— 要说清是这个原因"

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
