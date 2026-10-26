#!/usr/bin/env bash
# check-unimported.sh 的自测。
#
# 为什么需要它：这道门的判据全是「集合比对」，而它坏掉时的输出和「通过」长得
# 一模一样——可达集合为空时差集变成「仓库里的全部包」（像一次大发现），
# 仓库集合为空时差集为空（像「全部接上了」）。这与 LESSONS.md 第 16 条记的是
# 同一个形状（检查工具「没跑成」时输出和「跑成了」一样），而第 2 条更具体：
# 红线扫描器曾经自己报「通过」，实际根本没在检查。所以正反两面都要钉住。
#
# **本自测不调用真的 go list**：那要加载整个模块、吃内存，而这里要测的正是
# 「go list 输出长什么样时脚本怎么判」——所以造一个假的 go（一个普通 bash 脚本，
# chmod +x），用 GO= 注入。与 check-deadcode.test.sh 用桩件而不是真跑 deadcode
# 是同一个做法：快、可重复。
#
# 改动 check-unimported.sh、白名单格式或那个上限常量之后跑：
#   bash scripts/check-unimported.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-unimported.sh"
REAL_WHITELIST="$PWD/scripts/unimported-allowlist.txt"
tmp=$(mktemp -d)
last_out="$tmp/last.txt"
. "$PWD/scripts/gateselftest.sh"
trap 'rm -rf "$tmp"' EXIT


# 脚本里钉住的默认上限：自测不写死它，否则改一处要动两处。
#
# 读的是 MAX_ENTRIES_DEFAULT 而不是 MAX_ENTRIES：后者可以被环境变量顶开
# （见下），而这里要的是脚本自己声明的那个数。
declared=$(grep -m1 '^MAX_ENTRIES_DEFAULT=' "$SCRIPT" | cut -d= -f2)
if [ -z "$declared" ]; then
  echo "✗ 读不出 $SCRIPT 里的 MAX_ENTRIES_DEFAULT"
  exit 1
fi

# 比较类用例用的上限。
#
# 它们比的是「差集与白名单对不对得上」，与真实存量有几条无关，而夹具是自己
# 造的两条。真实上限降到 1 之后，这两条会先挂在「超过上限」上——那是夹具与
# 真实存量撞车，不是被测逻辑坏了。所以这些用例把上限顶开，上限本身留给
# 第 4、5、18 条去钉（那三条按脚本声明的默认值跑）。
CAP_WIDE=100

# ── 桩件 ──────────────────────────────────────────────────────────────────

# make_go <路径> <模块路径文件> <deps 文件> <包列表文件> [退出码] [stderr 文件]
# 桩件按调用形状分发，并把收到的参数记在 <路径>.calls 里，供自测断言
# 「这个包真的被传给了 go list」，而不只是被打印出来。
make_go() {
  local path="$1" modf="$2" depsf="$3" pkgsf="$4" rc="${5:-0}" errf="${6:-}"
  {
    printf '%s\n' '#!/usr/bin/env bash'
    printf '%s\n' '# 桩件：check-unimported.sh 的自测用，不是真的 go'
    printf '%s\n' 'printf "%s\n" "$*" >> "$0.calls"'
    printf '%s\n' 'case "$*" in'
    printf "  'list -m') cat '%s' ;;\n" "$modf"
    printf "  'list -deps'*) cat '%s' ;;\n" "$depsf"
    printf "  'list ./...') cat '%s' ;;\n" "$pkgsf"
    printf '%s\n' '  *) echo "桩件不认识这个调用：$*" >&2; exit 3 ;;'
    printf '%s\n' 'esac'
    if [ -n "$errf" ]; then printf "cat '%s' >&2\n" "$errf"; fi
    printf 'exit %s\n' "$rc"
  } >"$path"
  chmod +x "$path"
}

# run <桩件> <白名单>
# 退出码由调用者立刻取走：$? 必须在下一句就读。
run() {
  local stub="$1" wl="$2"
  GO="$stub" WHITELIST="$wl" MAX_ENTRIES="$CAP_WIDE" bash "$SCRIPT" >"$last_out" 2>&1
}

# run_at_cap <桩件> <白名单>
# 与 run 相同，但上限取脚本声明的默认值——给「上限本身」那几条用例用。
run_at_cap() {
  local stub="$1" wl="$2"
  GO="$stub" WHITELIST="$wl" bash "$SCRIPT" >"$last_out" 2>&1
}

# make_fake_repo <目标目录> [main 包名...]
# 造一个假的仓库根：只有 scripts/check-unimported.sh 与 cmd/<包名>/main.go。
# 扫描起点是自动发现的，所以只能这样验——在真仓库里塞一个 cmd/ 会污染工作树。
make_fake_repo() {
  local repo="$1"
  shift
  mkdir -p "$repo/scripts" "$repo/cmd"
  cp "$SCRIPT" "$repo/scripts/check-unimported.sh"
  local name
  for name in "$@"; do
    mkdir -p "$repo/cmd/$name"
    printf 'package main\n\nfunc main() {}\n' >"$repo/cmd/$name/main.go"
  done
}

# run_in_repo <仓库根> <桩件> <白名单>
run_in_repo() {
  local repo="$1" stub="$2" wl="$3"
  GO="$stub" WHITELIST="$wl" MAX_ENTRIES="$CAP_WIDE" \
    bash "$repo/scripts/check-unimported.sh" >"$last_out" 2>&1
}

# ── 夹具 ──────────────────────────────────────────────────────────────────

# 模块路径与包集合都用 example.com 下的假名：自测不该依赖真仓库当前有哪些包，
# 那些包随时会增删，而增删之后这条用例会以「测试失败」的样子报出来，
# 让人以为门禁坏了。
mod_ok="$tmp/mod-ok.txt"
printf '%s\n' 'example.com/fake' >"$mod_ok"

# 六个仓库自己的包；deps 里只有四个 → 无人 import 的是 orphan 与 spare。
pkgs_ok="$tmp/pkgs-ok.txt"
cat >"$pkgs_ok" <<'EOF'
example.com/fake/cmd/alpha
example.com/fake/cmd/beta
example.com/fake/internal/one
example.com/fake/internal/two
example.com/fake/internal/orphan
example.com/fake/internal/spare
EOF

# deps 里混着标准库与第三方：它们必须被模块前缀滤掉，
# 否则「无人 import」里会多出几百条。
deps_ok="$tmp/deps-ok.txt"
cat >"$deps_ok" <<'EOF'
example.com/fake/cmd/alpha
example.com/fake/cmd/beta
example.com/fake/internal/one
example.com/fake/internal/two
fmt
os
strings
github.com/other/dep
golang.org/x/tools/go/packages
EOF

wl_ok="$tmp/wl-ok.txt"
cat >"$wl_ok" <<'EOF'
# 用例用的小白名单：一条 lib、一条 gap
internal/orphan | lib | 库构件，先有先测
internal/spare  | gap | #1 还没有接线
EOF

stub_ok="$tmp/go-ok"
make_go "$stub_ok" "$mod_ok" "$deps_ok" "$pkgs_ok"

# ── 用例 ──────────────────────────────────────────────────────────────────

# 1. 差集与白名单一致：通过。**通过时必须有正向证据**——比了多少个包、
#    哪些包被判为可达。「什么都没输出」与「什么都没检查」长得一样，
#    而这道门的成功信号恰恰是沉默。
run "$stub_ok" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "全部命中 —— 应通过，实际退出码 $rc"; fi
grep -q '仓库自己的包 6 个：入口可达 4 个，无人 import 2 个' "$last_out" ||
  bad "全部命中 —— 通过时要报出比了多少个包（仓库 6 / 可达 4 / 无人 import 2）"
grep -q '全部命中白名单' "$last_out" || bad "全部命中 —— 通过时要说清「全部命中」"
grep -q '^  cmd/alpha$' "$last_out" || bad "全部命中 —— 可达的包要逐个列出来"
grep -q '^  internal/two$' "$last_out" || bad "全部命中 —— 可达的包要逐个列出来"
if grep -q '^  fmt$' "$last_out"; then
  bad "全部命中 —— 标准库不该出现在可达列表里（模块前缀没滤掉）"
else
  ok
fi

# 2. 出现一个没人 import 的包：退出码非 0，并把它打印出来。
#    这是这道门存在的理由：整包没人接时不能悄悄过去。
pkgs_new="$tmp/pkgs-new.txt"
cp "$pkgs_ok" "$pkgs_new"
echo 'example.com/fake/internal/newbie' >>"$pkgs_new"
stub_new="$tmp/go-new"
make_go "$stub_new" "$mod_ok" "$deps_ok" "$pkgs_new"
run "$stub_new" "$wl_ok"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "新包 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   internal/newbie$' "$last_out" || bad "新包 —— 要逐条打印出来，让人知道该加白名单还是该接线"
grep -q '要么接线' "$last_out" || bad "新包 —— 提示里要给出两条路"

# 3. 白名单里的包已经可达：**打印出来，但退出码仍然是 0**。
#    它出现的原因恰恰是「有人把没门的包接上线了」——那是个好动作，
#    让 CI 为它变红是一条会教人学会忽略 CI 的假警报。所以这一条与第 2 条
#    是**相反**的判据：一边钉「新包必须拦住」，一边钉「陈旧条目不许拦」。
#    留着不吭声也不行：那会让这个包将来重新变成无人 import 时被静默放行。
deps_stale="$tmp/deps-stale.txt"
cp "$deps_ok" "$deps_stale"
echo 'example.com/fake/internal/spare' >>"$deps_stale"
stub_stale="$tmp/go-stale"
make_go "$stub_stale" "$mod_ok" "$deps_stale" "$pkgs_ok"
run "$stub_stale" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "陈旧条目 —— 不该拦住合并（应退出码 0），实际 $rc"; fi
grep -q '^   internal/spare$' "$last_out" || bad "陈旧条目 —— 要报出是哪一条"
grep -q '可以删掉' "$last_out" || bad "陈旧条目 —— 要说清这一条已经可以删了"

# 3b. 新包与陈旧条目同时出现：仍然是失败，而且两件事都要报出来。
#     陈旧的那一半只提示，不能把真正该拦的那一半盖过去。
pkgs_both="$tmp/pkgs-both.txt"
cp "$pkgs_ok" "$pkgs_both"
echo 'example.com/fake/internal/newbie' >>"$pkgs_both"
wl_both="$tmp/wl-both.txt"
cat >"$wl_both" <<'EOF'
internal/orphan | lib | 库构件，先有先测
internal/spare  | gap | #1 已经接上了
EOF
stub_both="$tmp/go-both"
make_go "$stub_both" "$mod_ok" "$deps_stale" "$pkgs_both"
run "$stub_both" "$wl_both"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "新包 + 陈旧条目 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '^   internal/newbie$' "$last_out" || bad "新包 + 陈旧条目 —— 新包要报出来"
grep -q '^   internal/spare$' "$last_out" || bad "新包 + 陈旧条目 —— 陈旧条目也要报出来"

# 4. 白名单条目数超过上限：退出码非 0。
#    上限从脚本里读，所以这条用例不依赖那个具体数值。
wl_over="$tmp/wl-over.txt"
pkgs_over="$tmp/pkgs-over.txt"
cp "$pkgs_ok" "$pkgs_over"
: >"$wl_over"
i=0
while [ "$i" -le "$declared" ]; do
  printf 'internal/f%d | gap | #1 还没有接线\n' "$i" >>"$wl_over"
  printf 'example.com/fake/internal/f%d\n' "$i" >>"$pkgs_over"
  i=$((i + 1))
done
stub_over="$tmp/go-over"
make_go "$stub_over" "$mod_ok" "$deps_ok" "$pkgs_over"
# 这一条测的正是上限本身，所以不能顶开它。
run_at_cap "$stub_over" "$wl_over"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "条目数超过上限（$declared + 1）—— 应失败（退出码 1），实际 $rc"; fi
grep -q '超过上限' "$last_out" || bad "条目数超过上限 —— 要说清是这个原因"
grep -q '只许减不许增' "$last_out" || bad "条目数超过上限 —— 要说明「只许减不许增」这条规矩"

# 5. 上限是个具名常量，自测把它钉住。
#    往上调必须同时改这一处，于是那是一次会被 review 看到的 diff。
#    关掉一批缺口之后，把脚本里的常量与这里的字面量一起往下调。
if [ "$declared" = "2" ]; then ok; else bad "MAX_ENTRIES_DEFAULT 被改成了 $declared（自测钉住的是 2）"; fi

# 6. go 不在 PATH：退出码 2，并说清是没跑成。
#    用一个确定不存在的名字，避免依赖运行环境里有没有装 go。
GO="go-not-installed-$$" WHITELIST="$wl_ok" MAX_ENTRIES="$CAP_WIDE" \
  bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "go 不在 PATH —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '找不到' "$last_out" || bad "go 不在 PATH —— 要说清找不到的是哪个可执行文件"

# 7. go list 自身退出码非 0：退出码 2，不能当成「干净」。
stub_rc1="$tmp/go-rc1"
make_go "$stub_rc1" "$mod_ok" "$deps_ok" "$pkgs_ok" 1
run "$stub_rc1" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "go list 退出码非 0 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '没跑成' "$last_out" || bad "go list 退出码非 0 —— 要报出是没跑成，不是「干净」"

# 8. deps 输出里出现认不出的行：退出码 2。
#    认不出的行不能丢——那正是「工具换了措辞或换了参数」的样子（比如有人给它
#    加上 -test，于是多出 `./internal/foo [./internal/foo.test]` 这种形状）。
#    丢掉之后这一轮就变成两个集合都少了一批，而结果看起来像「差异变小了」。
deps_unknown="$tmp/deps-unknown.txt"
cp "$deps_ok" "$deps_unknown"
echo 'example.com/fake/internal/one [example.com/fake/internal/one.test]' >>"$deps_unknown"
stub_unknown="$tmp/go-unknown"
make_go "$stub_unknown" "$mod_ok" "$deps_unknown" "$pkgs_ok"
run "$stub_unknown" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "认不出的输出 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '认不出' "$last_out" || bad "认不出的输出 —— 要报出是「认不出」，而不是当成包名收下"

# 9. 仓库包列表一行都没有：退出码 2。
#    空集合时差集也是空的，那看起来像「全部接上了」——最危险的一种假通过。
pkgs_empty="$tmp/pkgs-empty.txt"
: >"$pkgs_empty"
stub_pkgs_empty="$tmp/go-pkgs-empty"
make_go "$stub_pkgs_empty" "$mod_ok" "$deps_ok" "$pkgs_empty"
run "$stub_pkgs_empty" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "包列表为空 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一行都没输出' "$last_out" || bad "包列表为空 —— 要说清是这个原因"

# 10. deps 里一个本仓库的包都没有（模块前缀读错了，或入口没 import 自己人）：
#     退出码 2。此时差集是「仓库里的全部包」，看起来像一次大发现。
deps_stdlib_only="$tmp/deps-stdlib.txt"
printf '%s\n' 'fmt' 'os' 'github.com/other/dep' >"$deps_stdlib_only"
stub_stdlib="$tmp/go-stdlib"
make_go "$stub_stdlib" "$mod_ok" "$deps_stdlib_only" "$pkgs_ok"
run "$stub_stdlib" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "可达集合为空 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个本仓库的包都没有' "$last_out" || bad "可达集合为空 —— 要说清是这个原因"

# 11. 模块路径与可达集合对不上：退出码 2。两种对不上走的是两条不同的守卫，
#     两条都要有，所以这里分两个用例——只测一条的话，另一条关掉了也没人知道。
#
# 11a. 一条依赖都对不上：可达集合过滤之后是空的。
mod_wrong="$tmp/mod-wrong.txt"
printf '%s\n' 'example.com/other' >"$mod_wrong"
stub_modwrong="$tmp/go-modwrong"
make_go "$stub_modwrong" "$mod_wrong" "$deps_ok" "$pkgs_ok"
run "$stub_modwrong" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "模块路径对不上依赖 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '一个本仓库的包都没有' "$last_out" || bad "模块路径对不上依赖 —— 要说清是这个原因"

# 11b. 对上了依赖、却剥不掉仓库包的前缀（这里把模块路径读成了一个标准库包名）。
#      此时白名单里的相对路径一条都对不上，整个仓库会被报成
#      「新出现的无人 import」——一次假的大发现，而人可能就去改白名单把它「修好」。
mod_partial="$tmp/mod-partial.txt"
printf '%s\n' 'os' >"$mod_partial"
stub_modpartial="$tmp/go-modpartial"
make_go "$stub_modpartial" "$mod_partial" "$deps_ok" "$pkgs_ok"
run "$stub_modpartial" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "模块前缀剥不掉 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '没有一个带模块前缀' "$last_out" || bad "模块前缀剥不掉 —— 要说清是这个原因"

# 12. go list -m 的输出认不出（多行）：退出码 2。
mod_multi="$tmp/mod-multi.txt"
printf '%s\n' 'example.com/fake' 'example.com/第二个' >"$mod_multi"
stub_modmulti="$tmp/go-modmulti"
make_go "$stub_modmulti" "$mod_multi" "$deps_ok" "$pkgs_ok"
run "$stub_modmulti" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "模块路径认不出 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q 'go list -m 的输出认不出' "$last_out" || bad "模块路径认不出 —— 要说清是这一处"

# 13. go list 往 stderr 写了东西但退出码是 0：**仍然通过**。
#     这一条钉的是一个刻意的选择：需要下载模块时 go 会写 `go: downloading …`，
#     按 stderr 判失败会把「第一次在干净环境里跑」变成一次假警报，
#     而假警报教人学会忽略 CI。所以判据是退出码与可解析性，不是 stderr 空不空。
err_download="$tmp/go-download.err"
printf '%s\n' 'go: downloading github.com/other/dep v1.0.0' >"$err_download"
stub_download="$tmp/go-download"
make_go "$stub_download" "$mod_ok" "$deps_ok" "$pkgs_ok" 0 "$err_download"
run "$stub_download" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "stderr 有下载进度 —— 不该判失败（应退出码 0），实际 $rc"; fi
grep -q '往 stderr 写了内容' "$last_out" || bad "stderr 有下载进度 —— 仍要打出来让人看见"

# 14. 白名单格式不对（栏数不足）：退出码 1，并指出是第几行。
wl_badfmt="$tmp/wl-badfmt.txt"
printf '%s\n' 'internal/orphan | lib' >"$wl_badfmt"
run "$stub_ok" "$wl_badfmt"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单格式不对 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '第 1 行' "$last_out" || bad "白名单格式不对 —— 要指出是第几行"

# 14b. 白名单坏掉时必须失败，**哪怕剩下的条目恰好能对上**。
#      这一条才是那个守卫的价值所在，也是上面那条用例分辨不出来的：
#      格式错的那一行对应的包是可达的（本来会是一条陈旧条目），于是把它跳过之后
#      new 与 stale 都是空的——不比就通过，而输出与真的「全部命中」一模一样。
#      上一条用例里坏行对应的包没人 import，跳过它照样会报「新出现的」，
#      所以把守卫整个关掉它也不会挂。
wl_badfmt2="$tmp/wl-badfmt2.txt"
cat >"$wl_badfmt2" <<'EOF'
internal/orphan | lib | 库构件，先有先测
internal/spare  | lib
EOF
run "$stub_stale" "$wl_badfmt2"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单坏掉但剩下的条目恰好对上 —— 必须仍然失败（退出码 1），实际 $rc"; fi
grep -q '第 2 行' "$last_out" || bad "白名单坏掉但剩下的条目恰好对上 —— 要指出是第几行"

# 15. 类型不认识：退出码 1。
wl_badkind="$tmp/wl-badkind.txt"
printf '%s\n' 'internal/orphan | maybe | 说不清' >"$wl_badkind"
run "$stub_ok" "$wl_badkind"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "类型不认识 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'lib' "$last_out" || bad "类型不认识 —— 要说出合法取值是哪两个"

# 16. gap 条目没有 issue 号：退出码 1。
#     白名单不能变成垃圾桶：gap 的每一条都要能回答「这一条为什么还在」。
wl_noissue="$tmp/wl-noissue.txt"
printf '%s\n' 'internal/orphan | gap | 反正暂时留着' >"$wl_noissue"
run "$stub_ok" "$wl_noissue"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "gap 没有 issue 号 —— 应失败（退出码 1），实际 $rc"; fi
grep -q 'issue 号' "$last_out" || bad "gap 没有 issue 号 —— 要说清缺的是什么"

# 16b. 白名单里有重复条目：退出码 1。
#      重复的条目会让「这一条为什么还在」有两个互相矛盾的答案，
#      而集合比对本身看不出重复——它去重之后照样相等。
wl_dup="$tmp/wl-dup.txt"
cat >"$wl_dup" <<'EOF'
internal/orphan | lib | 库构件，先有先测
internal/orphan | gap | #1 还没有接线
EOF
run "$stub_ok" "$wl_dup"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单有重复条目 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '重复' "$last_out" || bad "白名单有重复条目 —— 要说清是这个原因"

# 16c. 白名单文件不在：退出码 2。
#      「文件不在」与「清单是空的」在集合比对上长得一样（都是零条），
#      而零条会让整个仓库报成「新出现的无人 import」——所以必须是「没跑成」。
run "$stub_ok" "$tmp/根本没有这份白名单.txt"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "白名单不存在 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '不存在' "$last_out" || bad "白名单不存在 —— 要说清是这个原因"

# 17. 包路径写成绝对路径或带 ..：退出码 1。
#     白名单里存的是模块内的相对路径，混进别的写法时它永远匹配不上，
#     而「匹配不上」表现为「这一条怎么没生效」，不是一条错误。
wl_abs="$tmp/wl-abs.txt"
printf '%s\n' '/internal/orphan | lib | 绝对路径' >"$wl_abs"
run "$stub_ok" "$wl_abs"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "包路径是绝对路径 —— 应失败（退出码 1），实际 $rc"; fi
grep -q '相对路径' "$last_out" || bad "包路径是绝对路径 —— 要说清该写成什么样"

# 18. 签入仓库的那份白名单本身要能过。
#     桩件按真白名单生成包集合，于是这一条同时验了：文件格式、两档类型、
#     gap 都挂着 issue 号、条目数没超上限、相对路径能原样往返。
#     包名不写死：白名单将来增删时这条用例不该跟着坏。
mod_real="example.com/real"
real_pkgs="$tmp/real-pkgs.txt"
real_deps="$tmp/real-deps.txt"
{
  printf '%s/cmd/alpha\n' "$mod_real"
  printf '%s/internal/one\n' "$mod_real"
  awk -F'|' '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      p = $1
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", p)
      print "'"$mod_real"'/" p
    }
  ' "$REAL_WHITELIST"
} >"$real_pkgs"
{
  printf '%s/cmd/alpha\n' "$mod_real"
  printf '%s/internal/one\n' "$mod_real"
} >"$real_deps"
mod_real_file="$tmp/mod-real.txt"
printf '%s\n' "$mod_real" >"$mod_real_file"
stub_real="$tmp/go-real"
make_go "$stub_real" "$mod_real_file" "$real_deps" "$real_pkgs"
# 按默认上限跑：这一条同时验「真实白名单的条目数没超上限」。
run_at_cap "$stub_real" "$REAL_WHITELIST"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "签入的白名单 —— 应通过，实际退出码 $rc"; fi
real_count=$(awk '/^[[:space:]]*#/ { next } /^[[:space:]]*$/ { next } { n++ } END { print n + 0 }' "$REAL_WHITELIST")
grep -q "无人 import $real_count 个" "$last_out" ||
  bad "签入的白名单 —— 应报出比对了 $real_count 个包"

# 19. 扫描起点是**自动发现**的，不是手写清单。
#     手写清单会以它要防的方式坏掉：新加一个 main 包、没人记得补进来，于是那个包
#     整片没有门，而这一轮的输出看起来完全正常。这里造一个假仓库（不动真仓库），
#     验三件事：带 package main 的目录被发现、不带的不被当成起点、
#     发现的包**真的传给了 go list -deps**（而不只是被打印出来）。
repo_new="$tmp/repo-new"
make_fake_repo "$repo_new" alpha beta gamma
mkdir -p "$repo_new/cmd/notmain"
printf 'package notmain\n' >"$repo_new/cmd/notmain/lib.go"
stub_rec="$tmp/go-rec"
make_go "$stub_rec" "$mod_ok" "$deps_ok" "$pkgs_ok"
run_in_repo "$repo_new" "$stub_rec" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "自动发现扫描起点 —— 应通过，实际退出码 $rc"; fi
grep -q '扫描起点 ./cmd/alpha ./cmd/beta ./cmd/gamma' "$last_out" ||
  bad "自动发现扫描起点 —— 新加的 main 包要出现在扫描起点里（否则它整片没有门）"
grep -q '^  cmd/alpha   ← 入口' "$last_out" ||
  bad "自动发现扫描起点 —— 入口要在可达列表里标出来，否则看不出用了哪几个入口"
if grep -q 'notmain' "$last_out"; then
  bad "自动发现扫描起点 —— 不是 main 包的目录不该进扫描起点"
else
  ok
fi
rec_args=$(grep -m1 '^list -deps' "$stub_rec.calls" 2>/dev/null || echo "（没记到参数）")
case "$rec_args" in
  *./cmd/alpha*./cmd/beta*./cmd/gamma*) ok ;;
  *) bad "自动发现扫描起点 —— 发现的包要真的传给 go list -deps（收到：$rec_args）" ;;
esac

# 20. 一个 main 包都没发现：退出码 2。
#     「没有入口」= 可达集合为空 = 差集变成整个仓库，而那个结果看起来像结论。
repo_empty="$tmp/repo-empty"
make_fake_repo "$repo_empty"
run_in_repo "$repo_empty" "$stub_ok" "$wl_ok"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "扫不到 main 包 —— 应报「没跑成」（退出码 2），实际 $rc"; fi
grep -q '没找到 main 包' "$last_out" || bad "扫不到 main 包 —— 要说清是这个原因"

# 21. node_modules 里自带的 Go 包不算「仓库自己的包」。
#     `go list ./...` 不看 .gitignore，会把它们收进来；不滤掉的话，
#     跑过 `npm install` 的工作树会得到一条假的「新出现的整包没人 import」，
#     而 CI 上是绿的。同时钉住「滤掉几条要打印出来」——安静地滤掉与
#     「本来就没有」长得一样。
pkgs_nm="$tmp/pkgs-nm.txt"
cp "$pkgs_ok" "$pkgs_nm"
echo 'example.com/fake/web/node_modules/somedep/golang/pkg/leaf' >>"$pkgs_nm"
stub_nm="$tmp/go-nm"
make_go "$stub_nm" "$mod_ok" "$deps_ok" "$pkgs_nm"
run "$stub_nm" "$wl_ok"
rc=$?
if [ "$rc" -eq 0 ]; then
  ok
else
  bad "node_modules —— 依赖自带的 Go 包不该让门禁失败，实际退出码 $rc"
fi
grep -q '跳过 1 个 node_modules 下的包' "$last_out" ||
  bad "node_modules —— 滤掉几条要打印出来（否则与「本来就没有」分不开）"
if grep -q 'somedep' "$last_out"; then
  bad "node_modules —— 依赖自带的包不该出现在无人 import 清单里"
else
  ok
fi
grep -q '仓库自己的包 6 个' "$last_out" ||
  bad "node_modules —— 滤掉之后的包数才是「仓库自己的包」（期望 6）"

# ── 结果 ──────────────────────────────────────────────────────────────────

report
