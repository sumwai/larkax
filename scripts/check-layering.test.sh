#!/usr/bin/env bash
# check-layering.sh 的自测。
#
# 为什么需要它：这道门原先没有自测，而它的坏法与
# 其它几道同形——**判据写反、守卫没接上、扫描范围变成空**时，输出与「干净」
# 一模一样，退出码也一样（LESSONS.md 第 16 条；第 2 条是它的第一次现身：
# 检查脚本自己报「通过」，实际根本没在检查）。第 23 条把结论写死了：
# **每一个守卫都要有一条分辨得出来的用例**，退出码相同不等于走的是同一条分支。
#
# 下面正反两面都钉：
#
#   - 三条规则各自的命中与放行；
#   - 规则二是**提示型**的：命中时退出码仍然是 0（这一条最容易在重构里被改坏）；
#   - 规则二的三种过滤（money 包、测试文件、关键字）与一处真命中；
#   - 六个「没跑成」的出口（internal/ 没有 .go、go list 空、go list 失败、
#     逐包取 import 失败、grep 自身失败、go 不在），各自走退出码 2；
#   - 正向证据：包数、.go 文件数、可疑行数真的报了数，且与夹具对得上。
#
# **自测不能改真仓库**：夹具是一棵在 $tmp 下搭出来的最小仓库树（scripts/ 里放
# 被测脚本的副本、internal 与 cmd 下放几个 .go），import 关系由一个假的 go
# 按调用形状回答——这与 check-unimported.test.sh / check-deadmethods.test.sh
# 用桩件而不是真跑 go 是同一个做法：快、可重复、不碰工作树。
#
# 最后一节是**自测自己的变异**：把被测脚本的判据在临时副本上逐条改坏，
# 确认每条都让上面的断言变红。挂不了的那条判据，此刻只是「写在脚本里」，
# 不是「被证明在工作」（LESSONS.md 第 23 条）。
#
# 改动 check-layering.sh 之后跑：
#   bash scripts/check-layering.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-layering.sh"
MODULE_FIXTURE='example.com/fake'
tmp=$(mktemp -d)
trap 'chmod -R u+rwX "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT

pass=0
fail=0

last_out="$tmp/last.txt"
ok() { pass=$((pass + 1)); }
bad() {
  echo "✗ $1"
  sed 's/^/    | /' "$last_out" | tail -8
  fail=$((fail + 1))
}

# ── 桩件 ──────────────────────────────────────────────────────────────────

# make_go <路径> <import 关系表> [list 退出码] [list -f 退出码]
#
# 桩件按调用形状分发，并把收到的参数记在 <路径>.calls 里。
# **两种调用的退出码分开给**：合成一个的话，两个失败守卫里只有先跑的那个会被
# 测到——另一个「被测到了」只是因为前一个先报了错（LESSONS.md 第 23 条）。
#
# import 关系表每行形如 `<完整包路径>|<import>,<import>`。
make_go() {
  local path="$1" mapf="$2" rc="${3:-0}" imports_rc="${4:-0}"
  {
    printf '%s\n' '#!/usr/bin/env bash'
    printf '%s\n' '# 桩件：check-layering.sh 的自测用，不是真的 go'
    printf '%s\n' 'printf "%s\n" "$*" >> "$0.calls"'
    printf '%s\n' 'case "$*" in'
    printf "  'list ./internal/... ./cmd/...') cut -d'|' -f1 '%s'; exit %s ;;\n" "$mapf" "$rc"
    # 包名取最后一个参数，import 关系表用 `file` 变量传给 awk。
    # 用变量而不是把路径拼进 awk 程序文本：程序文本里的 `$1` 是给 awk 的，
    # 拼进双引号的 printf 会被这一层 shell 提前展开成空串——生成出来的桩件
    # 里不能有表达式，只能有值。
    printf '%s\n' "  'list -f'*) pkg=\"\${@: -1}\""
    printf "    file='%s'\n" "$mapf"
    printf "    awk -F'|' -v p=\"\$pkg\" '\$1 == p { n = split(\$2, a, \",\"); for (i = 1; i <= n; i++) if (a[i] != \"\") print a[i] }' \"\$file\"; exit %s ;;\n" \
      "$imports_rc"
    printf '%s\n' '  *) echo "桩件不认识这个调用：$*" >&2; exit 3 ;;'
    printf '%s\n' 'esac'
  } >"$path"
  chmod +x "$path"
}

# ── 夹具 ──────────────────────────────────────────────────────────────────

# mk_repo <根>：造一个空的假仓库根，scripts/ 里放被测脚本的副本。
mk_repo() {
  mkdir -p "$1/scripts"
  cp "$SCRIPT" "$1/scripts/check-layering.sh"
  : >"$1/.imports"
}

# mk_pkg <根> <相对包路径> [import 行…]：加一个包，并把它写进 import 关系表。
# .imports 放在仓库根而不是 internal/ 下，免得被规则二/三的 grep 扫到。
mk_pkg() {
  local root="$1" rel="$2"
  shift 2
  mkdir -p "$root/$rel"
  local pkgname=${rel##*/}
  local csv="" imp
  for imp in "$@"; do
    if [ -z "$csv" ]; then csv="$imp"; else csv="$csv,$imp"; fi
  done
  printf '%s/%s|%s\n' "$MODULE_FIXTURE" "$rel" "$csv" >>"$root/.imports"
  {
    printf 'package %s\n' "$pkgname"
    for imp in "$@"; do printf 'import "%s"\n' "$imp"; done
  } >"$root/$rel/main.go"
}

# mk_clean <根>：三条都干净的一棵树。10 个包、10 个 .go 文件、
# 其中 4 个 import relaykit、3 个 import gateway。
mk_clean() {
  local root="$1"
  mk_repo "$root"
  mk_pkg "$root" internal/relay github.com/QuantumNous/new-api/relaykit
  mk_pkg "$root" internal/channel/openaiadapter github.com/QuantumNous/new-api/relaykit
  mk_pkg "$root" internal/channel/anthropicadapter github.com/QuantumNous/new-api/relaykit
  mk_pkg "$root" internal/channel/responsesadapter github.com/QuantumNous/new-api/relaykit
  mk_pkg "$root" internal/gateway fmt
  mk_pkg "$root" internal/api "$MODULE_FIXTURE/internal/gateway"
  mk_pkg "$root" internal/assembly "$MODULE_FIXTURE/internal/gateway"
  mk_pkg "$root" internal/snapshotwire "$MODULE_FIXTURE/internal/gateway"
  mk_pkg "$root" internal/billing fmt
  mk_pkg "$root" cmd/larkax "$MODULE_FIXTURE/internal/api"
}

# run_expect <仓库根> <桩件> <期望退出码> <期望输出正则>
# 返回 0 = 退出码与输出都对得上。输出留在 $last_out。
run_expect() {
  local root="$1" stub="$2" want_rc="$3" want_re="$4" rc
  GO="$stub" MODULE="$MODULE_FIXTURE" bash "$root/scripts/check-layering.sh" >"$last_out" 2>&1
  rc=$?
  [ "$rc" -eq "$want_rc" ] || return 1
  grep -qE "$want_re" "$last_out" || return 1
  return 0
}

# case_expect <名字> <仓库根> <桩件> <期望退出码> <期望正则> [不该出现的正则]
case_expect() {
  local name="$1" root="$2" stub="$3" want_rc="$4" want_re="$5" not_re="${6:-}" rc why=""
  GO="$stub" MODULE="$MODULE_FIXTURE" bash "$root/scripts/check-layering.sh" >"$last_out" 2>&1
  rc=$?
  [ "$rc" -eq "$want_rc" ] || why="退出码 $rc，期望 $want_rc"
  if [ -z "$why" ] && ! grep -qE "$want_re" "$last_out"; then
    why="输出里没有 /$want_re/"
  fi
  if [ -z "$why" ] && [ -n "$not_re" ] && grep -qE "$not_re" "$last_out"; then
    why="输出里不该出现 /$not_re/"
  fi
  if [ -z "$why" ]; then ok; else bad "$name —— $why"; fi
}

# ── 1. 干净夹具：退出码 0，且给出正向证据 ────────────────────────────────
#
# 「什么都没输出」与「什么都没检查」长得一样，所以通过时也必须报出
# 扫了多少个包、多少个 .go 文件、可疑几行，而且数要与夹具对得上。

clean="$tmp/clean"
mk_clean "$clean"
stub_clean="$tmp/go-clean"
make_go "$stub_clean" "$clean/.imports"

case_expect "干净夹具" "$clean" "$stub_clean" 0 '三条都查过了：包 10 个（其中 import relaykit 的 4 个）/ \.go 文件 10 个 / 可疑 0 行'
case_expect "干净夹具 —— 通过时要明说通过" "$clean" "$stub_clean" 0 '^分层自查通过。$'
case_expect "干净夹具 —— 允许清单里的包要逐个列出来" "$clean" "$stub_clean" 0 '^  ✓ internal/relay$'
case_expect "干净夹具 —— gateway 的允许方也要列出来" "$clean" "$stub_clean" 0 '^  ✓ internal/assembly$'
case_expect "干净夹具 —— 规则三要有「在几个文件里查找」的证据" "$clean" "$stub_clean" 0 '（在 10 个 \.go 文件里查找）'

# 规则一确实逐包问了 go list，而不是只打了一行标题。
if grep -q '^list -f' "$stub_clean.calls"; then ok; else
  bad "干净夹具 —— 每个包都要真的传给 go list -f，而不是只看包列表"
fi

# ── 2. 规则一：清单外的包 import relaykit → 退出码 1，并点名 ─────────────

r1="$tmp/r1"
mk_clean "$r1"
mk_pkg "$r1" internal/rogue github.com/QuantumNous/new-api/relaykit
stub_r1="$tmp/go-r1"
make_go "$stub_r1" "$r1/.imports"

case_expect "relaykit 越界" "$r1" "$stub_r1" 1 '^  ✗ internal/rogue —— 不在允许清单里$'
case_expect "relaykit 越界 —— 失败时要明说不通过" "$r1" "$stub_r1" 1 '^分层自查不通过。$'

# ── 3. 规则三：清单外的包 import gateway → 退出码 1，并点名 ──────────────
#
# 与第 2 条是两个独立的判据，所以各有一条用例：只测一条的话，另一条
# 被关掉也没人知道。

r3="$tmp/r3"
mk_clean "$r3"
mk_pkg "$r3" internal/billing-ingest "$MODULE_FIXTURE/internal/gateway"
stub_r3="$tmp/go-r3"
make_go "$stub_r3" "$r3/.imports"

case_expect "gateway 越界" "$r3" "$stub_r3" 1 '^  ✗ internal/billing-ingest —— 不在允许清单里'
case_expect "gateway 越界 —— 失败时要明说不通过" "$r3" "$stub_r3" 1 '^分层自查不通过。$'

# ── 4. 没有任何包 import gateway：通过，并明说「无」 ─────────────────────

none="$tmp/none"
mk_repo "$none"
mk_pkg "$none" internal/billing fmt
mk_pkg "$none" cmd/larkax fmt
stub_none="$tmp/go-none"
make_go "$stub_none" "$none/.imports"

case_expect "没人 import gateway" "$none" "$stub_none" 0 '^  ✓ 无$'

# ── 5. 规则二：提示型，命中时退出码仍然是 0 ──────────────────────────────
#
# 这是最容易在重构里被改坏的一条：把提示型判成失败，报告里就会多出一批
# 误报，而**误报第二次就没人看了**。所以这里同时钉住「命中」与「不拦」。

hint="$tmp/hint"
mk_clean "$hint"
# money 包里用 float64：规则二不管（舍入点本来就该在这里）。
mkdir -p "$hint/internal/money"
cat >"$hint/internal/money/m.go" <<'EOF'
package money

func toFloat(n int64) float64 { return float64(n) }
EOF
# 测试文件不算。
cat >"$hint/internal/billing/amount_test.go" <<'EOF'
package billing

func helper(n int64) float64 { return float64(n) }
EOF
# 关键字排除：这一行的路径里就带 rate。
cat >"$hint/internal/billing/rate.go" <<'EOF'
package billing

func ratio(n int64) float64 { r := float64(n); return r }
EOF
# 这一处才是真命中。
cat >"$hint/internal/billing/amount.go" <<'EOF'
package billing

func cents(n int64) float64 { y := float64(n) + 1; return y }
EOF
stub_hint="$tmp/go-hint"
make_go "$stub_hint" "$hint/.imports"

case_expect "提示型命中不参与退出码" "$hint" "$stub_hint" 0 '提示型，不参与退出码'
case_expect "提示型命中 —— 要逐条打出来" "$hint" "$stub_hint" 0 '^  \? internal/billing/amount\.go:'
case_expect "提示型命中 —— 条数要与命中对上（只该有一行）" "$hint" "$stub_hint" 0 '命中 1 行'
case_expect "提示型 — 过滤 money / 测试文件 / 关键字" "$hint" "$stub_hint" 0 '^分层自查通过。$' \
  '^  \? internal/money/|^  \? internal/billing/amount_test\.go|^  \? internal/billing/rate\.go'

# ── 6. 「没跑成」的五个出口，各自走退出码 2 ──────────────────────────────
#
# 与「门禁失败」（退出码 1）分开：两者在 CI 里都是红的，但处置完全不同——
# 前者要去改依赖方向，后者说明这道门自己坏了。五条各走一条不同的守卫。

# 6a. internal/ 下一个 .go 都没有：扫描范围写错了。
nofiles="$tmp/nofiles"
mk_repo "$nofiles"
mkdir -p "$nofiles/internal" "$nofiles/cmd/larkax"
printf 'package main\n' >"$nofiles/cmd/larkax/main.go"
: >"$nofiles/internal/note.txt"
printf '%s/cmd/larkax|\n' "$MODULE_FIXTURE" >"$nofiles/.imports"
stub_nofiles="$tmp/go-nofiles"
make_go "$stub_nofiles" "$nofiles/.imports"
case_expect "internal/ 没有 .go" "$nofiles" "$stub_nofiles" 2 'internal/ 下一个 \.go 文件都没扫到'

# 6b. go list 一个包都没列出来：空集合时「谁都没 import relaykit」与
#     「一个包都没扫」在输出上分不出来。
nopkgs="$tmp/nopkgs"
mk_clean "$nopkgs"
: >"$nopkgs/.imports"
stub_nopkgs="$tmp/go-nopkgs"
make_go "$stub_nopkgs" "$nopkgs/.imports"
case_expect "go list 一个包都没列出" "$nopkgs" "$stub_nopkgs" 2 'go list 一个包都没列出来'

# 6c. go list 自身失败。
listfail="$tmp/listfail"
mk_clean "$listfail"
stub_listfail="$tmp/go-listfail"
make_go "$stub_listfail" "$listfail/.imports" 1
case_expect "go list 失败" "$listfail" "$stub_listfail" 2 'go list ./internal/\.\.\. \./cmd/\.\.\. 失败'

# 6d. 逐包取 import 失败：这个包的 import 关系没读到，不能当作「它没 import relaykit」。
impfail="$tmp/impfail"
mk_clean "$impfail"
stub_impfail="$tmp/go-impfail"
make_go "$stub_impfail" "$impfail/.imports" 0 1
case_expect "逐包取 import 失败" "$impfail" "$stub_impfail" 2 '这个包的 import 关系没读到'

# 6e. grep 自身失败（目录读不动）。这条要与规则三那一处分开：两处扫的是同一批
#     目录，所以两边都会失败，**唯一分辨得出来的是报的是哪一处的错**。
#     root 下 chmod 挡不住，跳过并说明（CI 与本地都不是 root）。
if [ "$(id -u)" -eq 0 ]; then
  echo "· 以 root 运行，跳过「grep 自身失败」那一条（chmod 挡不住 root）"
else
  grepfail="$tmp/grepfail"
  mk_clean "$grepfail"
  mkdir -p "$grepfail/internal/locked"
  printf 'package locked\n' >"$grepfail/internal/locked/x.go"
  chmod 000 "$grepfail/internal/locked"
  stub_grepfail="$tmp/go-grepfail"
  make_go "$stub_grepfail" "$grepfail/.imports"
  case_expect "grep 自身失败" "$grepfail" "$stub_grepfail" 2 'grep 扫描 internal/ 时执行失败'
  chmod 755 "$grepfail/internal/locked"
fi

# 6f. go 不在 PATH：用的是确定不存在的名字，不依赖运行环境有没有装 go。
case_expect "找不到 go" "$clean" "go-not-installed-$$" 2 '找不到 go-not-installed'

# ── 7. 真仓库：这道门在它自己身上是干净的 ────────────────────────────────
#
# 前六节用的是夹具。这一条跑真仓库，钉住「落地时确实是对的」——顺带证明
# 夹具之外的真实包树也能被 go list 走到。

GO=go MODULE= bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "真仓库 —— 应通过（退出码 0），实际 $rc"; fi
repo_pkgs=$(grep -oE '包 [0-9]+ 个' "$last_out" | grep -oE '[0-9]+' | head -n1)
if [ "${repo_pkgs:-0}" -ge 20 ]; then ok; else
  bad "真仓库 —— 只数到 ${repo_pkgs:-0} 个包，扫描范围像是写错了"
fi
repo_files=$(grep -oE '\.go 文件 [0-9]+ 个' "$last_out" | grep -oE '[0-9]+' | head -n1)
if [ "${repo_files:-0}" -ge 100 ]; then ok; else
  bad "真仓库 —— 只扫到 ${repo_files:-0} 个 .go 文件，扫描范围像是写错了"
fi

# ── 8. 自测自己的变异：判据逐条改坏，每一条都要让上面红 ──────────────────
#
# 只改**临时副本**，原文件不动。改的是一整行（awk 按整行相等替换），
# 要替换的那一行必须真的在脚本里——找不到就说明脚本被重写过了，
# 这条变异已经不再指向任何判据，必须报错而不是静默跳过。

mutate_line() {
  local path="$1" old="$2" new="$3"
  grep -qxF -- "$old" "$path" || {
    echo "✗ 变异：要改的那一行不在脚本里（判据可能被重写了）：$old"
    fail=$((fail + 1))
    return 1
  }
  # 新旧两行走 ENVIRON 而不是 -v：`-v` 会解释反斜杠转义，而这里要改的
  # 判据里恰好含 `\n`（`printf '%s\n'`），经 `-v` 之后变成真换行、匹配不上，
  # 于是变异体其实是原样的脚本——自测照样绿，而它以为自己在测变异。
  OLD="$old" NEW="$new" awk '
    $0 == ENVIRON["OLD"] && !done { print ENVIRON["NEW"]; done = 1; next }
    { print }
  ' "$path" >"$path.tmp"
  mv "$path.tmp" "$path"
}

# expect_mutant_red <名字> <夹具根> <桩件> <原判据下的期望退出码> <期望正则>
# 变异体上这条断言必须**不再成立**；仍然成立说明自测分辨不出这个判据。
expect_mutant_red() {
  local name="$1" root="$2" stub="$3" want_rc="$4" want_re="$5"
  if run_expect "$root" "$stub" "$want_rc" "$want_re"; then
    bad "变异[$name] —— 判据被改坏后自测仍然通过，这条判据没被钉住"
  else
    ok
  fi
}

# 8a. 关掉规则一的允许清单判据：越界的包会被放行。
m1="$tmp/mut-r1"
mk_clean "$m1"
mk_pkg "$m1" internal/rogue github.com/QuantumNous/new-api/relaykit
stub_m1="$tmp/go-mut-r1"
make_go "$stub_m1" "$m1/.imports"
mutate_line "$m1/scripts/check-layering.sh" \
  '    if ! printf '"'"'%s\n'"'"' "$ALLOWED_RELAYKIT_IMPORTERS" | grep -qx "$rel"; then' \
  '    if false; then' &&
  expect_mutant_red "关掉 relaykit 允许清单" "$m1" "$stub_m1" 1 '不在允许清单里'

# 8b. 关掉规则三的允许清单判据。
m3="$tmp/mut-r3"
mk_clean "$m3"
mk_pkg "$m3" internal/billing-ingest "$MODULE_FIXTURE/internal/gateway"
stub_m3="$tmp/go-mut-r3"
make_go "$stub_m3" "$m3/.imports"
mutate_line "$m3/scripts/check-layering.sh" \
  '    if printf '"'"'%s\n'"'"' "$ALLOWED_GATEWAY_IMPORTERS" | grep -qx "$rel"; then' \
  '    if true; then' &&
  expect_mutant_red "关掉 gateway 允许清单" "$m3" "$stub_m3" 1 '不在允许清单里'

# 8c. 把提示型改成拦：规则二一命中就失败。
#     关闭之前「命中仍退出 0」的断言会变成退出码 1。
m2="$tmp/mut-r2"
mk_clean "$m2"
cat >"$m2/internal/billing/amount.go" <<'EOF'
package billing

func cents(n int64) float64 { y := float64(n) + 1; return y }
EOF
stub_m2="$tmp/go-mut-r2"
make_go "$stub_m2" "$m2/.imports"
mutate_line "$m2/scripts/check-layering.sh" \
  '  echo "  （提示型，不参与退出码：逐条确认，是金额运算就该挪进 money 包）"' \
  '  fail=1' &&
  expect_mutant_red "提示型改成拦" "$m2" "$stub_m2" 0 '提示型，不参与退出码'

# 8d. 关掉「go list 一个包都没列出」的守卫。
m4="$tmp/mut-nopkgs"
mk_clean "$m4"
: >"$m4/.imports"
stub_m4="$tmp/go-mut-nopkgs"
make_go "$stub_m4" "$m4/.imports"
mutate_line "$m4/scripts/check-layering.sh" \
  'if [ "$pkg_count" -eq 0 ]; then' \
  'if [ "$pkg_count" -eq -1 ]; then' &&
  expect_mutant_red "关掉空包列表守卫" "$m4" "$stub_m4" 2 'go list 一个包都没列出来'

# 8e. 关掉「internal/ 没有 .go」的守卫。
m5="$tmp/mut-nofiles"
mk_repo "$m5"
mkdir -p "$m5/internal" "$m5/cmd/larkax"
printf 'package main\n' >"$m5/cmd/larkax/main.go"
printf '%s/cmd/larkax|\n' "$MODULE_FIXTURE" >"$m5/.imports"
stub_m5="$tmp/go-mut-nofiles"
make_go "$stub_m5" "$m5/.imports"
mutate_line "$m5/scripts/check-layering.sh" \
  'if [ "$internal_files" -eq 0 ]; then' \
  'if [ "$internal_files" -eq -1 ]; then' &&
  expect_mutant_red "关掉空扫描范围守卫" "$m5" "$stub_m5" 2 'internal/ 下一个 \.go 文件都没扫到'

# 8f. 吞掉规则二的 grep 失败：把退出码 2 当成正常的「没命中」。
#     这一条分辨得出来，是因为规则三随后会失败，但报的是**另一处**的错。
if [ "$(id -u)" -ne 0 ]; then
  m6="$tmp/mut-grepfail"
  mk_clean "$m6"
  mkdir -p "$m6/internal/locked"
  printf 'package locked\n' >"$m6/internal/locked/x.go"
  chmod 000 "$m6/internal/locked"
  stub_m6="$tmp/go-mut-grepfail"
  make_go "$stub_m6" "$m6/.imports"
  mutate_line "$m6/scripts/check-layering.sh" \
    '  0 | 1) ;;' \
    '  0 | 1 | 2) ;;' &&
    expect_mutant_red "吞掉规则二的 grep 失败" "$m6" "$stub_m6" 2 'grep 扫描 internal/ 时执行失败'
  chmod 755 "$m6/internal/locked"
fi

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
