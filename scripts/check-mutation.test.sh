#!/usr/bin/env bash
# check-mutation.sh 的自测。
#
# 为什么需要它：LESSONS 第 16 条记着同一个形状在项目里出现过四次——检查工具
# 「没跑成」时输出和「跑成了」一模一样，退出码也一样。变异测试这一处最典型的
# 样子就是 `Killed: 0, Lived: 0`（集成测试缺 DSN 被 t.Skip），而屏幕上它和
# 「这个包质量不行」没有区别。所以判据必须有正反两面钉住：
# 「跑成了」通过，「没跑成」必须失败。
#
# **本自测不调用真的 gremlins**：一次要几分钟，还会和别的扫描抢 CPU，
# 而这里要测的正是「gremlins 输出长什么样时脚本怎么判」——所以造一个假的
# gremlins（一个普通 bash 脚本，chmod +x），用 GREMLINS= 注入。
# 与 check-redlines.test.sh 用桩件而不是真跑工具是同一个做法：快、可重复。
#
# **go 也桩掉**（见下面「假 go」一节）：被测脚本每跑一个包之前都会
# `go clean -testcache`（#773），自测真跑就会把**开发者自己的**测试缓存清掉。
#
# 桩件还要**答得上来 `unleash --help`**：脚本每次运行都靠它读「这把尺子是什么」
#（版本 + 变异器集合），读不出会直接拒绝运行。夹具用的是真 gremlins 的
# `unleash --help` 原文（只去掉它自己那两行行尾的空格），不是按想象造一份——
# 探测的判据就架在那份排版上，造出来的夹具只能证明「脚本能解析我造的东西」。
# 版本探测同理用 GREMLINS_VERSION= 注入（桩件是脚本，go version -m 探不出来）。
#
# 每个用例用独立的锁文件，互不干扰，也不会撞上正在跑的真实实例。
#
# 改动 check-mutation.sh 后跑：
#   bash scripts/check-mutation.test.sh
#
# 退出码：
#   0  全部用例跑成
#   1  有用例测出来是坏的
#   2  有用例**没测成**——例如没能观察到锁被持有。它与 1 分开，是因为
#      「这次没测成」与「锁真的坏了」的处置完全不同：前者重跑一次即可，
#      后者要改代码。两者混在一起时，输出会看起来像后者（LESSONS 第 16 条）。

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-mutation.sh"

# 上一次自测被 SIGKILL 时，第 30 条写下的探针文件会留在工作树里，下一次的文件
# 计数就会多一个。开始前先清掉，并说出删了几个——悄悄改别人的工作树是另一回事。
leftover=0
for f in internal/money/zz_narrow_probe_*.go; do
  [ -e "$f" ] || continue
  rm -f "$f"
  leftover=$((leftover + 1))
done
if [ "$leftover" -gt 0 ]; then
  echo "清理了 $leftover 个上次残留的探针文件（internal/money/zz_narrow_probe_*.go）"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0
untested=0

# 脚本里钉住的版本：自测不写死它，否则改一处要动两处。
PINNED=$(grep -m1 '^PINNED_GREMLINS_VERSION=' "$SCRIPT" | cut -d'"' -f2)
if [ -z "$PINNED" ]; then
  echo "✗ 读不出 $SCRIPT 里的 PINNED_GREMLINS_VERSION"
  exit 1
fi

# 用例输出留在这里，失败时打印出来便于定位。
last_out="$tmp/last.txt"

ok()   { pass=$((pass + 1)); }
bad()  { echo "✗ $1"; fail=$((fail + 1)); }
# 「没测成」不是「测出来是坏的」：它只说明这一次的判据不成立。
not_tested() { echo "⊘ $1"; untested=$((untested + 1)); }

# ── 夹具：gremlins 的 `unleash --help` ────────────────────────────────────
#
# 逐字抄自真 gremlins（v0.6.0）的 `unleash --help`，只去掉了它自己那两行
# 行尾的空格（不参与解析）。**不按想象造**：脚本的变异器集合就是从这份排版里
# 读出来的，造一份「我以为它长这样」的夹具，只能证明脚本能解析我造的东西
#（LESSONS 第 33 条）。
HELP_FILE="$tmp/gremlins-help.txt"
cat > "$HELP_FILE" <<'EOF'
Unleashes the gremlins and performs mutation testing on a Go module. It works by
first gathering the coverage of the test suite and then analysing the source
code to look for supported mutants.

Unleash only tests covered mutants, since it doesn't make sense to test mutants
that no test case is able to catch.

In 'dry-run' mode, unleash only performs the analysis of the source code, but it
doesn't actually perform the test.

Thresholds are configurable quality gates that make gremlins exit with an error
if those values are not met. Efficacy is the percent of KILLED mutants over
the total KILLED and LIVED mutants. Mutant coverage is the percent of total
KILLED + LIVED mutants, over the total mutants.

Usage:
  gremlins unleash [path] [flags]

Aliases:
  unleash, run, r

Flags:
  -d, --dry-run                     find mutations but do not executes tests
  -S, --output-statuses string      print only statuses from this flag, allowed values - 'lctkvsr'
  -t, --tags string                 a comma-separated list of build tags
      --coverpkg string             a comma-separated list of package patterns
  -D, --diff string                 diff branch or commit
  -o, --output string               set the output file for machine readable results
  -i, --integration                 makes Gremlins run the complete test suite for each mutation
  -E, --exclude-files stringArray   exclude files from Gremlins run by filepath regexp
      --threshold-efficacy float    threshold for code-efficacy percent
      --threshold-mcover float      threshold for mutant-coverage percent
      --workers int                 the number of workers to use in mutation testing
      --test-cpu int                the number of CPUs to allow each test run to use
      --timeout-coefficient int     the coefficient by which the timeout is increased
      --arithmetic-base             enable "ARITHMETIC_BASE" mutants (default true)
      --conditionals-boundary       enable "CONDITIONALS_BOUNDARY" mutants (default true)
      --conditionals-negation       enable "CONDITIONALS_NEGATION" mutants (default true)
      --invert-assignments          enable "INVERT_ASSIGNMENTS" mutants
      --invert-bitwise              enable "INVERT_BITWISE" mutants
      --invert-bwassign             enable "INVERT_BWASSIGN" mutants
      --increment-decrement         enable "INCREMENT_DECREMENT" mutants (default true)
      --invert-logical              enable "INVERT_LOGICAL" mutants
      --invert-loopctrl             enable "INVERT_LOOPCTRL" mutants
      --invert-negatives            enable "INVERT_NEGATIVES" mutants (default true)
      --remove-self-assignments     enable "REMOVE_SELF_ASSIGNMENTS" mutants
  -h, --help                        help for unleash

Global Flags:
      --config string   override config file
  -s, --silent          suppress output and run in silent mode
EOF

# 上面那份 help 里默认开着的变异器，与显式打开的 INVERT_LOGICAL。
# 脚本打出的「这把尺子」那一行必须正好是这些——**这个字面量是判据的一部分**：
# 探测悄悄少认一个默认变异器时，只有把它写死在这里才看得见。
HELP_DEFAULT_MUTATORS="ARITHMETIC_BASE CONDITIONALS_BOUNDARY CONDITIONALS_NEGATION INCREMENT_DECREMENT INVERT_NEGATIVES"
HELP_EXTRA_MUTATOR="INVERT_LOGICAL"

# ── 假 gremlins ───────────────────────────────────────────────────────────
#
# 输出照抄 gremlins 的汇总行：脚本读的就是这几行。

# stub_head：每个桩件的共同开头。
#
# 脚本每次运行都会先问一句 `unleash --help`（读「这把尺子是什么」），
# 答不上来它会拒绝运行——所以桩件必须答得上来，否则下面每个用例都变成
# 「拒绝运行」，测不到本来要测的判据。
stub_head() {
  cat <<EOF
#!/usr/bin/env bash
# 桩件：check-mutation.sh 的自测用，不是真的变异测试
for __a in "\$@"; do
  case "\$__a" in
    --help|-h) cat "$HELP_FILE"; exit 0 ;;
  esac
done
EOF
}

# make_gremlins <路径> <killed> <lived> <timed> <efficacy> [退出码]
#
# 除了打汇总行，还把收到的参数原样追加到 `$GREMLINS_ARGS_LOG`（默认 /dev/null）。
# 收窄那几条用例要看的正是「实际传了哪些 `--exclude-files`」——只看汇总行的话，
# 参数拼错了也照样打出一张好看的表。
make_gremlins() {
  local path="$1" killed="$2" lived="$3" timed="$4" eff="$5" rc="${6:-0}"
  {
    stub_head
    cat <<EOF
printf '%s\n' "\$*" >> "\${GREMLINS_ARGS_LOG:-/dev/null}"
echo "Killed: $killed, Lived: $lived, Not covered: 3"
echo "Timed out: $timed, Not viable: 0, Skipped: 0"
echo "Test efficacy: $eff"
echo "Mutator coverage: 100.00%"
exit $rc
EOF
  } > "$path"
  chmod +x "$path"
}

# 没有那几个汇总行的桩件：模拟 gremlins 升级后格式变了。
make_gremlins_unknown_format() {
  local path="$1"
  {
    stub_head
    cat <<'EOF'
echo "gremlins v9: ran some mutants, all good probably"
echo "done"
EOF
  } > "$path"
  chmod +x "$path"
}

# 持锁直到用例放行的桩件：用来在第一个实例持锁期间发第二个实例。
#
# 持锁窗口由 $RELEASE_FILE 控制，而**不是一个猜出来的秒数**：负载高时
# fork/exec 会被推迟，靠 sleep 赌窗口在机器忙的时候会让第二个实例撞上已经
# 释放的锁，于是用例假红。带一个 60 秒兜底，避免用例意外退出后留下孤儿进程。
make_gremlins_blocking() {
  local path="$1"
  {
    stub_head
    cat <<'EOF'
# 桩件：一直持着锁，直到用例创建 $RELEASE_FILE 放行
i=0
while [ ! -e "$RELEASE_FILE" ] && [ "$i" -lt 600 ]; do
  sleep 0.1
  i=$((i + 1))
done
echo "Killed: 5, Lived: 2, Not covered: 1"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 71.43%"
echo "Mutator coverage: 87.50%"
EOF
  } > "$path"
  chmod +x "$path"
}

# 带存活明细的桩件：gremlins 每测一个变异会打一行
# `LIVED <类型> at <文件>:<行>:<列>`（前面可能带缩进）。脚本要把它们抄下来，
# 否则「对存活的变异逐个判断」这句话没有可判断的对象。
make_gremlins_with_survivors() {
  local path="$1"
  {
    stub_head
    cat <<'EOF'
echo " LIVED CONDITIONALS_BOUNDARY at money.go:103:8"
echo "  LIVED ARITHMETIC_BASE at money.go:54:25"
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
echo "Mutator coverage: 100.00%"
EOF
  } > "$path"
  chmod +x "$path"
}

# run <桩件> <锁文件> [筛选项...]
# 退出码由调用者立刻取走：$? 必须在下一句就读。
#
# LARKAX_DB_DSN 给个占位值：脚本缺它会直接拒绝运行（集成测试会被跳过，
# internal/ledger/store 那一包等于没测）。桩件不看这个值，只要它在。
# 「缺 DSN 要拒绝」本身是第 12 条用例，那一条显式把它清掉。
#
# GREMLINS_VERSION 注入脚本里钉住的那一版：桩件是 bash 脚本，脚本用
# `go version -m` 探版本探不出来。**这条注入是刻意的**——每个用例只测它自己
# 那一件事，版本探测本身由第 17、18 条单独钉。
testdsn="postgres://user:placeholder@localhost:5432/larkax?sslmode=disable"
run() {
  local stub="$1" lock="$2"; shift 2
  LARKAX_DB_DSN="$testdsn" GREMLINS="$stub" GREMLINS_VERSION="$PINNED" LOCK_FILE="$lock" \
    GREMLINS_ARGS_LOG="${GREMLINS_ARGS_LOG:-/dev/null}" \
    bash "$SCRIPT" "$@" >"$last_out" 2>&1
}

dump() { sed 's/^/    | /' "$last_out" | tail -6; }

# table_field <表头标签> <包名> <输出文件>
#
# 取汇总表里某一栏的值：**先按表头量出那一栏的起止位置，再按同一位置从数据行上取**。
#
# 不按空白切：表是定宽 printf 排出来的（包名一栏 %-20s、数值栏 %7s 之类），
# 空白项数与列数不是一回事——按项数写死的解析会在某一栏变宽时静默错位，
# 而「错位」与「这一栏没有值」在断言里长得一样。
#
# `LC_ALL=C` 是必须的，不是顺手加的：表头第一栏是汉字「包」，bash 的 printf
# 按**字节**把它补到 20 位（3 字节 + 17 个空格），数据行那一栏是 ASCII，
# 于是整张表的列位置只在**字节**口径下与表头对齐。awk 在 UTF-8 locale 下按
# 字符数 index/substr，每一栏会整体左移 2 位，读到的是**邻栏**的值——
# 断言于是「每栏都有值」，只是值全是隔壁那一栏的。
table_field() {
  LC_ALL=C awk -v label="$1" -v pkg="$2" '
    !hdr && index($0, label) && /killed/ { start = index($0, label); w = length(label); hdr = 1; next }
    hdr && $0 ~ ("^" pkg " ") { print substr($0, start, w); exit }
  ' "$3"
}

# wait_lock_held <第一个实例的日志> <它的 PID> <最多等多少拍，每拍 0.1s>
#
# 判据是「**真的观察到**锁被持有」，不是「等了多久」。返回：
#   0  观察到了，可以往下走
#   2  没观察到（超时，或第一个实例在此之前就退出了）——这次没测成，
#      不能据此判断锁是好是坏
#
# 观察的是持锁方自己打的那行「已拿到锁」，**不是从外面用 flock 去探**：
# 探锁本身要抢同一把锁，负载高时探测方反而先拿到，被测实例自己被判
# 「另一个实例正在跑」——那会把「没测成」伪装成「锁坏了」，正是本条要修的东西。
#
# 只等固定时长而不记结果，是本条用例原先会反的原因：负载高时第一个实例的
# fork/exec 被推迟到等待窗口之后，窗口耗尽后第二个实例反而先拿到锁，于是
# 「第二个拒绝了」与「第一个跑完了」两个断言同时颠倒，看起来像锁坏了。
wait_lock_held() {
  local log="$1" pid="$2" max="$3" waited=0
  while [ "$waited" -lt "$max" ]; do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "没测成：第一个实例（PID $pid）在拿到锁之前就退出了。"
      return 2
    fi
    if grep -q '已拿到锁' "$log" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  echo "没测成：等了 $((max * 100)) 毫秒，没有观察到锁被持有。"
  return 2
}

# ── 假 go ─────────────────────────────────────────────────────────────────
#
# 被测脚本现在**每跑一个包之前**都会 `go clean -testcache`（#773）。那是它的正经行为，
# 但自测替它执行一遍，清掉的是**开发者自己的**测试缓存——自测不该有这种副作用。
# 而且这里要问的是「这条命令有没有发在对的时点」，不是「清缓存本身管不管用」，
# 所以整个自测都把 go 桩掉：摆在 PATH 最前面，只记下收到的子命令，别的一概不做。
# **给整个自测都摆上，不是只给新用例**：老用例也走这条路径，只桩住新用例的话，
# 其余二十来个用例照样会把缓存清掉。
#
# 序列默认记到 /dev/null（大多数用例不关心它），要看序列的用例设 FAKE_GO_LOG；
# 失败注入走 FAKE_GO_RC（第 22b 条）。桩件对任何子命令都退出 0 且不打输出——
# 版本探测那一路（第 18 条）拿到的仍是「读不出」。
fakebin="$tmp/fakebin"
mkdir -p "$fakebin"
cat > "$fakebin/go" <<'EOF'
#!/usr/bin/env bash
# 桩件：check-mutation.sh 的自测用，不是真的 go
printf 'go %s\n' "$*" >> "${FAKE_GO_LOG:-/dev/null}"
exit "${FAKE_GO_RC:-0}"
EOF
chmod +x "$fakebin/go"
PATH="$fakebin:$PATH"
export PATH

# ── 桩件 ──────────────────────────────────────────────────────────────────

stub_ok="$tmp/gremlins-ok"
stub_none="$tmp/gremlins-none"
stub_timeout="$tmp/gremlins-timeout"
stub_at_limit="$tmp/gremlins-at-limit"
stub_all_killed="$tmp/gremlins-all-killed"
stub_rc1="$tmp/gremlins-rc1"
stub_unknown="$tmp/gremlins-unknown"
stub_junk="$tmp/gremlins-junk"

make_gremlins "$stub_ok"          8 2 0  "80.00%"
make_gremlins "$stub_none"        0 0 0  "0.00%"      # 缺 DSN 时集成测试被跳过的形状
make_gremlins "$stub_timeout"     3 1 11 "25.00%"     # 超时超出上限
make_gremlins "$stub_at_limit"    3 1 10 "25.00%"     # 恰好等于上限
make_gremlins "$stub_all_killed"  12 0 0 "100.00%"    # 全部变异被杀，是良性的
make_gremlins "$stub_rc1"         0 0 0 "0.00%" 1     # gremlins 自己失败
make_gremlins_unknown_format "$stub_unknown"

# 既没有数字也没有已知关键词的失败输出：解析不到任何东西。
#
# **要退出 0**：退出码非 0 是第 6 条（「gremlins 自己失败」）那一类，脚本在解析
# 之前就拦下了。这一条要的是「跑完了、退出码 0，但输出里没有任何可判的东西」——
# 那才是最难与「通过」区分开的一类。
#
# 也要带 stub_head：否则脚本在更早的一步（读不出变异器集合）就退出了，
# 第 8 条会「通过」得毫无理由。
{
  stub_head
  cat <<'EOF'
echo "error: could not build the test binary"
echo "make: *** [Makefile:12: test] Error 2"
EOF
} > "$stub_junk"
chmod +x "$stub_junk"

# ── 用例 ──────────────────────────────────────────────────────────────────

# 1. 正常输出：有杀有活、没超时。这是「跑成了」的样子。
run "$stub_ok" "$tmp/lock1" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "正常输出 —— 应通过，实际退出码 $rc"; dump; fi
grep -q '全部跑成' "$last_out" || bad "正常输出 —— 通过时也要说清「全部跑成」"

# 1b. 汇总表要连着「有多少变异体没被碰到」一起打（#769）。
#
#     efficacy 的分母只是**被测到的**那些变异体。internal/keyquota 刚进清单
#     那一轮的实际输出是 Killed 2 / Lived 0 / Not covered 6，efficacy 100.00%、
#     Mutator coverage 25.00%——那个 100% 说的是「`over()` 的两个变异体被杀掉了」，
#     不是「这一包被钉住了」。桩件照抄的就是这一轮。
#
#     这一条同时钉住「数字照实打、不拦」：Mutator coverage 只有 25% 仍然**通过**。
#     覆盖率不进判据——这一条要解决的只是「范围窄这件事在表上看不出来」。
stub_lowcov="$tmp/gremlins-lowcov"
{
  stub_head
  cat <<'EOF'
echo "Killed: 2, Lived: 0, Not covered: 6"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 100.00%"
echo "Mutator coverage: 25.00%"
EOF
} > "$stub_lowcov"
chmod +x "$stub_lowcov"
run "$stub_lowcov" "$tmp/lock1b" internal/keyquota
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "低覆盖的包 —— 应通过（覆盖率不进判据），实际退出码 $rc"; dump; fi
grep -q 'Not covered' "$last_out" || bad "低覆盖的包 —— 表头要有 Not covered 这一栏"
grep -q 'Mutator coverage' "$last_out" || bad "低覆盖的包 —— 表头要有 Mutator coverage 这一栏"
# 逐栏按表头量出来的列取值：新两栏要打出来，而且**打在对的栏里**。
# 只断言「输出里有 6 与 25.00%」不够——两栏写反、或塞进别的栏里，一样能过。
for pair in "killed:2" "存活:0" "超时:0" "Not covered:6" "Mutator coverage:25.00%" "efficacy:100.00%"; do
  label="${pair%%:*}"; want="${pair#*:}"
  got=$(table_field "$label" internal/keyquota "$last_out" | tr -d ' ')
  if [ "$got" = "$want" ]; then
    ok
  else
    bad "低覆盖的包 —— 「$label」那一栏应是 $want，实际读到「$got」"
  fi
done

# 1c. 只有老那四行、没有 Not covered / Mutator coverage：按「认不出输出」处理。
#
#     这两栏现在上了表，读不出来时表上会留两个空格子，而空格子看起来像
#     「这两个数不存在」，不像「没读到」。版本是钉住的，所以真出现这种情况
#     更可能是格式变了——那时照旧不许当成通过。
stub_oldfmt="$tmp/gremlins-oldfmt"
{
  stub_head
  cat <<'EOF'
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
EOF
} > "$stub_oldfmt"
chmod +x "$stub_oldfmt"
run "$stub_oldfmt" "$tmp/lock1c" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "少了新两行 —— 应失败，实际退出码 $rc"; dump; fi
grep -q '认不出' "$last_out" || bad "少了新两行 —— 应报出是「认不出输出」"

# 1d. 父子两条**一起写成参数**时拒绝执行（#866）。
#
#     包路径是递归口径：`./internal/ledger/` 会把子包 internal/ledger/store 的
#     变异体一起跑。两个一起传，store 的几百个变异体会被跑两遍——实测第一包已经
#     出了汇总行，第二包才开始重跑同一批。所以显式给出时直接拒绝，并说清谁包含谁、
#     该只留哪个；而且**一个变异都不许跑**（拒绝发生在调用 gremlins 之前）。
run "$stub_ok" "$tmp/lock1d" internal/ledger internal/ledger/store
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "父子两条一起传 —— 应拒绝，实际退出码 $rc"; dump; fi
grep -q 'internal/ledger 递归包含 internal/ledger/store' "$last_out" \
  || bad "父子两条一起传 —— 要说明谁包含谁"
grep -q '这一轮请只留：internal/ledger' "$last_out" \
  || bad "父子两条一起传 —— 要提示只留外层那个"
if grep -q '全部跑成' "$last_out" || grep -q '^internal/ledger ' "$last_out"; then
  bad "父子两条一起传 —— 拒绝之后不该跑出任何汇总行"
else
  ok
fi
# 只选子包时没有重叠可言：父包这一轮根本没跑。
run "$stub_ok" "$tmp/lock1d-child" internal/ledger/store
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "只选子包 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q '递归包含' "$last_out"; then
  bad "只选子包 —— 不该说重叠（父包这一轮没跑）"
else
  ok
fi
# **不带参数跑全部包是例外**：那是本脚本的基线口径，表本来就按「父包含子包」读。
# 它必须照常跑成，只打一行注记。判据也要钉住，否则哪天有人把拒绝逻辑挪到选包之后，
# 「跑全部包」会变成一句「请只留外层」。
LARKAX_DB_DSN="$testdsn" LARKAX_REDIS_DSN="redis://placeholder:6379/0" \
  GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" LOCK_FILE="$tmp/lock1d-all" \
  bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "不带参数跑全部包 —— 应照常跑成，实际退出码 $rc"; dump; fi
grep -q 'internal/ledger 含 internal/ledger/store' "$last_out" \
  || bad "不带参数跑全部包 —— 重叠要照旧打注记"
grep -q '全部跑成' "$last_out" || bad "不带参数跑全部包 —— 收尾应报「全部跑成」"
# 全部跑时，另一对真实重叠（billing / expr）也要列出来：注记从 PKGS 算，
# 不是只认某一个包名。
if grep -q 'internal/billing 含 internal/billing/expr' "$last_out"; then
  ok
else
  bad "不带参数跑全部包 —— billing / expr 这一对重叠也要列出来"
fi
# 只选父包时也不说：它这一次跑出来的**就是** ledger + store，表上没有第二行
# 与它重叠，这时说一句「范围重叠」反而会让人以为表里还有另一个 store。
run "$stub_ok" "$tmp/lock1d-parent" internal/ledger
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "只选父包 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q '递归包含' "$last_out"; then
  bad "只选父包 —— 不该说重叠（表里只有它这一行）"
else
  ok
fi

# 2. 一个变异都没跑到。**这是本自测存在的理由**：
#    它以前被判为「全部跑成」，而 0.00% efficacy 看着像真实的低分。
run "$stub_none" "$tmp/lock2" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "Killed 0 / Lived 0 —— 应失败，实际退出码 $rc"; dump; fi
grep -q '一个变异都没跑到' "$last_out" || bad "Killed 0 / Lived 0 —— 报错要说清是「没跑到」而不是分数低"
grep -q 'LARKAX_DB_DSN' "$last_out" || bad "Killed 0 / Lived 0 —— 提示里应给出缺 DSN 这个常见原因"

# 3. 超时超过上限：这一轮的分数不可信，按失败处理。
run "$stub_timeout" "$tmp/lock3" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "超时 11 个（上限 10）—— 应失败，实际退出码 $rc"; dump; fi
grep -q '不可信' "$last_out" || bad "超时超限 —— 报错要说清「这个分数不可信」"

# 4. 超时恰好等于上限：边界的内侧，应当通过。
#    判据是 > 而不是 >=，这两条一起把边界钉住。
run "$stub_at_limit" "$tmp/lock4" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "超时 10 个（等于上限）—— 应通过，实际退出码 $rc"; dump; fi

# 5. 全部变异被杀（lived 为 0、killed 不为 0）：良性的，不该误判成「没跑到」。
run "$stub_all_killed" "$tmp/lock5" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "Killed 12 / Lived 0 —— 应通过，实际退出码 $rc"; dump; fi

# 6. gremlins 自身退出码非 0。
run "$stub_rc1" "$tmp/lock6" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "gremlins 退出码非 0 —— 应失败，实际退出码 $rc"; dump; fi
grep -q 'gremlins 执行失败' "$last_out" || bad "gremlins 退出码非 0 —— 应报出是执行失败"

# 7. 输出格式变了：认不出那几行。
#    **解析不到任何数字时必须往失败一侧倒**——此时脚本什么都没在判断，
#    静默当成通过就是又回到第 16 条那个形状。
run "$stub_unknown" "$tmp/lock7" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "输出格式变了 —— 应失败，实际退出码 $rc"; dump; fi
grep -q '认不出' "$last_out" || bad "输出格式变了 —— 应报出是「认不出输出」"

# 8. 输出里既没有数字也没有已知关键词（编译失败的形状）：同样往失败一侧倒。
run "$stub_junk" "$tmp/lock8" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "无法解析的输出 —— 应失败，实际退出码 $rc"; dump; fi
# 断言**理由**，不只断言退出码：这一步若因为别的原因（比如读不出变异器集合）
# 退出 1，只看退出码的用例会绿着什么都不测。
grep -q '认不出' "$last_out" || bad "无法解析的输出 —— 应报出是「认不出输出」"

# 9. 两个实例同时跑：第二个必须拒绝执行。
#    并行会让超时判据失真（两个实例各自固定 workers=1，机器上照样是 2 个 go test），
#    同一份代码会给出两个不同的分数——所以这里宁可拒绝，也不一起跑。
#
#    **这条只在机器负载高时会反**：第一个实例的 fork/exec 被推迟到等待窗口之后，
#    第二个实例反而先拿到锁，于是两个断言同时颠倒，看起来像「锁坏了」。所以：
#      - 等的是「真的观察到锁被持有」（wait_lock_held），不是猜一个时长；
#      - 观察不到就报「没测成」（退出码 2），而不是继续往下让断言互相颠倒；
#      - 第一个实例的持锁期由 RELEASE_FILE 控制，第二个实例的发射时点因此
#        不依赖 sleep 的长短。
#    第二个实例用一个快桩件：锁若真的坏了，它跑完就退出，用例不会卡在慢桩件上。
stub_par="$tmp/gremlins-par"
stub_par_fast="$tmp/gremlins-par-fast"
make_gremlins_blocking "$stub_par"
make_gremlins "$stub_par_fast" 5 2 0 "71.43%"
lock_par="$tmp/lock-par"
release_par="$tmp/par-release"
rm -f "$release_par"
out_par1="$tmp/par1.txt"
out_par2="$tmp/par2.txt"
(
  RELEASE_FILE="$release_par" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_par" GREMLINS_VERSION="$PINNED" LOCK_FILE="$lock_par" \
    bash "$SCRIPT" internal/money >"$out_par1" 2>&1
) &
bg=$!
if wait_lock_held "$out_par1" "$bg" 60; then
  LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_par_fast" GREMLINS_VERSION="$PINNED" LOCK_FILE="$lock_par" \
    bash "$SCRIPT" internal/money >"$out_par2" 2>&1
  rc2=$?
  : > "$release_par"        # 放行第一个实例
  wait "$bg"
  rc1=$?
  if [ "$rc2" -eq 3 ]; then ok; else bad "第二个实例 —— 应拒绝执行（退出码 3），实际 $rc2"; sed 's/^/    | /' "$out_par2" | tail -6; fi
  grep -q '另一个实例正在跑' "$out_par2" || bad "第二个实例 —— 应说明是「另一个实例正在跑」"
  grep -q '失真' "$out_par2" || bad "第二个实例 —— 应说明并行会让超时判据失真"
  # 先发的那个照常跑完：锁只挡并发的第二个，不影响正常的那个
  if [ "$rc1" -eq 0 ]; then ok; else bad "第一个实例 —— 应正常跑完，实际 $rc1"; sed 's/^/    | /' "$out_par1" | tail -6; fi
else
  # 没观察到锁被持有：这次没测成。先把第一个实例放行并收掉，再单独记一笔。
  : > "$release_par"
  wait "$bg"
  rc1=$?
  not_tested "两个实例抢锁 —— 没能观察到锁被持有（第一个实例退出码 $rc1）。这次没测成，不能据此判断锁是好是坏。"
fi

# 9b. 观察不到锁时的两条分支本身也要钉住：一条守不住它的用例，
#     会把「没测成」重新混回「锁坏了」（LESSONS 第 23 条）。
#     (a) 后台进程还活着但从不碰锁 → 等满窗口后报没测成；
#     (b) 后台进程提前退出 → 立刻报没测成。
sleep 5 & never=$!
: > "$tmp/log-never"
if wait_lock_held "$tmp/log-never" "$never" 3 >"$last_out" 2>&1; then
  bad "没观察到锁（进程还在）—— 应报没测成，实际返回 0"
else
  rc=$?
  if [ "$rc" -eq 2 ] && grep -q '没测成' "$last_out"; then
    ok
  else
    bad "没观察到锁（进程还在）—— 应返回 2 并说明没测成，实际 $rc"
  fi
fi
kill "$never" 2>/dev/null
wait "$never" 2>/dev/null
( exit 1 ) & dead=$!
wait "$dead" 2>/dev/null       # 先回收，确保 kill -0 看到的是「已退出」而不是僵尸
if wait_lock_held "$tmp/log-dead" "$dead" 3 >"$last_out" 2>&1; then
  bad "第一个实例提前退出 —— 应报没测成，实际返回 0"
else
  rc=$?
  if [ "$rc" -eq 2 ] && grep -q '在拿到锁之前就退出' "$last_out"; then
    ok
  else
    bad "第一个实例提前退出 —— 应返回 2 并说明它已经退出，实际 $rc"
  fi
fi
wait "$dead" 2>/dev/null

# 9c. 默认锁路径是**克隆级**的（#1152）：同一克隆的两棵工作树要解析出同一个路径。
#
#     原先默认值取 `$PWD`，而每棵树各有自己的 `$PWD`，于是同一克隆里两个变异实例
#     各拿各的锁、并行跑同一批集成用例：包级固定库名时不只是慢，而是互相删表；
#     超时也会凭空变多，而超时与「这一轮真的受了干扰」在输出里长得一样。
#
#     断言两边**逐字相等**，且等于 `git rev-parse --git-common-dir` 推出来的那个
#     路径。只断言「两边相等」不够：两个都落回 `$PWD/tmp/…`（即旧的坏值）时也相等，
#     而那正是这条用例要挡住的回归。
#
#     自建临时仓库加 `git worktree`，不依赖调用自测的这棵树有没有 `.git`：门禁镜像
#     用 rsync 同步、排除 `.git`，在真仓库里跑这条会变成「没测成」（理由同第 34 条）。
#
#     **不设 LOCK_FILE**：覆盖路径会绕开这里要测的默认值逻辑。
clone_ready=0
clone="$tmp/clone"
clone_wt="$tmp/clone-wt"
if ! command -v git >/dev/null 2>&1; then
  bad "默认锁路径 —— 找不到 git，这条用例无法验证"
else
  # 临时仓库里的身份与 gpg 签名都用 -c 显式给：这台机器上没有全局 git 身份，
  # 而 commit 失败会变成另一种「没测成」。
  git_cfg=(-c user.name=larkax-test -c user.email=user@example.com)
  mkdir -p "$clone/scripts" "$clone/internal/money"
  cp "$SCRIPT" "$clone/scripts/check-mutation.sh"
  printf 'package money\n' >"$clone/internal/money/money.go"
  # init 与首次提交的退出码要各查一次：`set -e` 没有开，init 失败会一路走到
  # `worktree add`，报出来的是「建不出第二个工作树」——把失败挪到了另一处，
  # 而真正的原因（init 自己的输出）被丢掉了。
  clone_setup=""
  if ! clone_setup=$(git -C "$clone" "${git_cfg[@]}" -c init.defaultBranch=main init -q 2>&1); then
    bad "默认锁路径 —— 临时仓库 git init 失败：$clone_setup"
  elif ! clone_setup=$(git -C "$clone" "${git_cfg[@]}" add -A 2>&1); then
    bad "默认锁路径 —— 临时仓库 git add 失败：$clone_setup"
  elif ! clone_setup=$(git -C "$clone" "${git_cfg[@]}" -c commit.gpgsign=false commit -q -m fixture 2>&1); then
    bad "默认锁路径 —— 临时仓库首次提交失败：$clone_setup"
  elif ! git -C "$clone" worktree add -q "$clone_wt" -b second >/dev/null 2>&1; then
    bad "默认锁路径 —— 建不出第二个工作树（git worktree add 失败）"
  else
    clone_ready=1
    out_lock_main="$tmp/default-lock-main.txt"
    out_lock_wt="$tmp/default-lock-wt.txt"
    LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" \
      bash "$clone/scripts/check-mutation.sh" internal/money >"$out_lock_main" 2>&1
    rc_main=$?
    LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" \
      bash "$clone_wt/scripts/check-mutation.sh" internal/money >"$out_lock_wt" 2>&1
    rc_wt=$?
    lock_main=$(sed -n 's/^已拿到锁：//p' "$out_lock_main" | head -n 1)
    lock_wt=$(sed -n 's/^已拿到锁：//p' "$out_lock_wt" | head -n 1)
    if [ "$rc_main" -eq 0 ] && [ "$rc_wt" -eq 0 ]; then
      ok
    else
      bad "默认锁路径 —— 两棵树里都应正常跑完，实际主树 $rc_main、第二棵树 $rc_wt"
      sed 's/^/    | /' "$out_lock_main" | tail -4
      sed 's/^/    | /' "$out_lock_wt" | tail -4
    fi
    if [ "$lock_main" = "$lock_wt" ] && [ -n "$lock_main" ]; then
      ok
    else
      bad "默认锁路径 —— 两棵树应解析出同一个路径：主树「$lock_main」，第二棵树「$lock_wt」"
    fi
    # 与 `git rev-parse --git-common-dir` 逐字对齐：上面那一条只保证「两边一样」，
    # 两边一起落在工作树下的 tmp/（旧的坏值）也同样「一样」。
    common_dir=$(git -C "$clone" rev-parse --git-common-dir 2>/dev/null)
    case "$common_dir" in
      /*) ;;
      *) common_dir="$clone/$common_dir" ;;
    esac
    common_dir=$(cd "$common_dir" && pwd -P)
    lock_expected="$common_dir/tmp/check-mutation.lock"
    if [ "$lock_main" = "$lock_expected" ]; then
      ok
    else
      bad "默认锁路径 —— 应等于 git rev-parse --git-common-dir 推出来的那个：期望「$lock_expected」，实际「$lock_main」"
    fi
  fi
fi

# 9d. 默认路径下同一克隆的两棵树互斥（#1152）：第一个持锁时第二个必须被挡。
#
#     与第 9 条的差别只在锁从哪来：这一条**不设 LOCK_FILE**，测的正是默认路径
#     落到同一把锁上。第二个实例用快桩件——锁若真的坏了，它跑完就退出，用例不会
#     卡在慢桩件上；再断言它一次都没调到 gremlins，「没有并行跑」于是有两层证据
#     （退得出来，且没干活）。
#
#     失败文案在这里逐字比对，且期望值里带的是**克隆一级**的路径：文案原先只是
#     「另一个实例正在跑（锁：…）」，改默认值后路径会变成公共目录下的那一个，
#     读者据此就能找到那把锁。
if [ "$clone_ready" -ne 1 ]; then
  not_tested "同一克隆的两棵树互斥 —— 临时仓库没建起来，这条没测成"
else
  stub_clone_slow="$tmp/gremlins-clone-slow"
  stub_clone_fast="$tmp/gremlins-clone-fast"
  make_gremlins_blocking "$stub_clone_slow"
  make_gremlins "$stub_clone_fast" 5 2 0 "71.43%"
  release_clone="$tmp/clone-release"
  rm -f "$release_clone"
  out_clone1="$tmp/clone1.txt"
  out_clone2="$tmp/clone2.txt"
  args_clone2="$tmp/clone2-args.txt"
  : >"$args_clone2"
  (
    RELEASE_FILE="$release_clone" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_clone_slow" \
      GREMLINS_VERSION="$PINNED" bash "$clone/scripts/check-mutation.sh" internal/money >"$out_clone1" 2>&1
  ) &
  bg=$!
  if wait_lock_held "$out_clone1" "$bg" 60; then
    GREMLINS_ARGS_LOG="$args_clone2" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_clone_fast" \
      GREMLINS_VERSION="$PINNED" bash "$clone_wt/scripts/check-mutation.sh" internal/money >"$out_clone2" 2>&1
    rc2=$?
    : >"$release_clone"        # 放行第一个实例
    wait "$bg"
    rc1=$?
    if [ "$rc2" -eq 3 ]; then ok; else bad "另一棵树里的第二个实例 —— 应拒绝执行（退出码 3），实际 $rc2"; sed 's/^/    | /' "$out_clone2" | tail -6; fi
    if [ "$rc1" -eq 0 ]; then ok; else bad "先发的那棵树 —— 应正常跑完，实际 $rc1"; sed 's/^/    | /' "$out_clone1" | tail -6; fi
    if [ -s "$args_clone2" ]; then
      bad "另一棵树里的第二个实例 —— 被挡住就不该调用 gremlins（收到：$(cat "$args_clone2")）"
    else
      ok
    fi
    grep -qxF "另一个实例正在跑（锁：$lock_expected），本次未执行。" "$out_clone2" \
      || bad "第二个实例 —— 失败文案第一行要逐字不变，且写的是克隆级锁路径（期望「另一个实例正在跑（锁：$lock_expected），本次未执行。」）"
    grep -qxF '并行会让超时判据失真：gremlins 的超时按墙钟算，而基准是串行测出来的，' "$out_clone2" \
      || bad "第二个实例 —— 失败文案第二行要逐字不变"
    grep -qxF '两个实例一起跑时被判超时的变异会凭空变多（见本文件开头与 LESSONS 第 15 条）。' "$out_clone2" \
      || bad "第二个实例 —— 失败文案第三行要逐字不变"
  else
    : >"$release_clone"
    wait "$bg"
    rc1=$?
    not_tested "同一克隆的两棵树互斥 —— 没能观察到锁被持有（第一个实例退出码 $rc1）。这次没测成，不能据此判断锁是好是坏。"
  fi
fi

# 9e. 读不出 git 公共目录时退回工作树级，并且**出声**（#1152 评审）。
#
#     退回的两种成因——「本来就不在仓库里」与「读不出公共目录」（dubious
#     ownership、`.git` 损坏）——都让跨工作树互斥失效，而那正是这把克隆级锁
#     要挡的事。静默退回时，坏掉的恰好是「看起来一切正常」的那一半。
#
#     这一行必须到 stderr：stdout 与退出码一律不变，按 stdout 判结果的调用方
#     不受影响。所以这里分开收两个流，两边都断言。
#
#     不在真仓库里跑，也不设 LOCK_FILE：测的正是默认值的退回路径。
#
#     **前提要自己钉住**：`$tmp` 必须不落在任何仓库里。TMPDIR 指到仓库内时
#     这条会假红——更糟的是它会把外层仓库当成「工作树」，拿默认锁去碰**真实
#     那把锁**，而那正是本单要杜绝的事。所以先探一次公共目录：探得出来就记
#     ⊘（没测成），不判红。
plain="$tmp/plain"
mkdir -p "$plain/scripts" "$plain/internal/money"
if git -C "$plain" rev-parse --git-common-dir >/dev/null 2>&1; then
  not_tested "退回工作树级 —— 临时目录落在某个仓库里（TMPDIR 指到了仓库内？），这条没测成：跑它会去碰外层仓库的那把锁"
else
  cp "$SCRIPT" "$plain/scripts/check-mutation.sh"
  printf 'package money\n' >"$plain/internal/money/money.go"
  out_plain="$tmp/plain-out.txt"
  err_plain="$tmp/plain-err.txt"
  (
    cd "$plain" || exit 1
    LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" \
      bash "$plain/scripts/check-mutation.sh" internal/money >"$out_plain" 2>"$err_plain"
  )
  rc=$?
  if [ "$rc" -eq 0 ]; then
    ok
  else
    bad "退回工作树级 —— 应当照常跑完，实际退出码 $rc"
    sed 's/^/    | /' "$err_plain" | tail -4
  fi
  grep -qF "锁退回工作树级：$plain/tmp/check-mutation.lock（读不出 git 公共目录）" "$err_plain" \
    || bad "退回工作树级 —— 要在 stderr 上说清锁退到了哪一个路径"
  if grep -q '锁退回工作树级' "$out_plain"; then
    bad "退回工作树级 —— 这行不能出现在 stdout 上"
  else
    ok
  fi
  # 退回之后仍然是工作树级，所以这把锁照旧在 <工作树>/tmp 下——这里顺带确认
  # 路径没被别的逻辑改掉（换机器、换 git 版本时最先悄悄变的正是它）。
  grep -qxF "已拿到锁：$plain/tmp/check-mutation.lock" "$out_plain" \
    || bad "退回工作树级 —— stdout 上的拿锁行应指向工作树下的 tmp/（期望「已拿到锁：$plain/tmp/check-mutation.lock」）"
fi

# 10. gremlins 不在 PATH：要给出安装提示，而不是别的错误。
#     用一个确定不存在的名字，避免依赖运行环境里有没有装 gremlins。
GREMLINS="gremlins-not-installed-$$" LOCK_FILE="$tmp/lock10" \
  bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "gremlins 不在 PATH —— 应失败，实际退出码 $rc"; dump; fi
grep -q 'go install' "$last_out" || bad "gremlins 不在 PATH —— 应给出安装提示"

# 11. 筛选项一个包都没选中：这次什么都没检查，不算通过。
run "$stub_ok" "$tmp/lock11" internal/no-such-package
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "筛选项没选中任何包 —— 应失败，实际退出码 $rc"; dump; fi
grep -q '没有选中任何包' "$last_out" || bad "筛选项没选中任何包 —— 应报出「什么都没检查」"
grep -q '不算通过' "$last_out" || bad "筛选项没选中任何包 —— 要说明这不算通过"

# 11b. **只有一部分**包参数没匹配到：也要说，且整轮判失败（#746）。
#      与第 11 条的区别是「一条都没选中」与「只漏了一部分」：后者原先与「全部
#      跑成」长得一模一样——匹配到的照常跑，汇总表少一行，收尾照打「全部跑成。」，
#      退出码 0。而写参数的人以为自己覆盖了两个包，实际只覆盖一个，少验的那一包
#      不会被别的机制补上（CI 的变异 job 与本脚本共用同一份清单，名字写错同样
#      会少跑一包）。
#      两个方向都钉：没匹配到的参数要被列出来，而匹配到的包该跑还是要跑——判据是
#      「不静默」，不是「一个参数写错就整轮什么都不做」。
run "$stub_ok" "$tmp/lock11b" internal/money internal/no-such-package
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "部分包参数没匹配到 —— 应失败，实际退出码 $rc"; dump; fi
grep -q '没匹配到任何包' "$last_out" || bad "部分包参数没匹配到 —— 要报出是「没匹配到」"
grep -q 'internal/no-such-package' "$last_out" || bad "部分包参数没匹配到 —— 要列出没匹配到的是哪一个参数"
grep -q 'internal/money ' "$last_out" || bad "部分包参数没匹配到 —— 匹配到的包仍应照常跑"
if grep -q '全部跑成' "$last_out"; then
  bad "部分包参数没匹配到 —— 收尾不能照打「全部跑成」"
else
  ok
fi

# 12. 缺 LARKAX_DB_DSN：必须拒绝运行，而不是跑出一批不可比的数字。
#     internal/ledger/store 的集成测试缺它时整包被 t.Skip，而那个包在 PKGS
#     清单里有自己的一行——那一行等于没测（Killed 0、Lived 0）。
#     它还会连带改掉别的行的数：`internal/ledger` 那一条是**递归**口径
#     （`./internal/ledger/` 同时选中子包 store），store 的变异体全落进
#     Not covered，于是 ledger 那一行的 Not covered 也涨上去。
#     机制与实测见 docs/development.md「一个分数要连着它的条件一起读」。
env -u LARKAX_DB_DSN GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" LOCK_FILE="$tmp/lock12" \
  bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "缺 DSN —— 应拒绝运行，实际退出码 $rc"; dump; fi
grep -q '没有 LARKAX_DB_DSN' "$last_out" || bad "缺 DSN —— 应报出是这个原因"
grep -q 't.Skip' "$last_out" || bad "缺 DSN —— 应说明 store 会被整包跳过"

# 13. 缺 LARKAX_REDIS_DSN：选中包里只要测试读它，同样必须拒绝运行。
#     与第 12 条同一个道理，只是范围按**选中包**算：数据库是每包都要，Redis
#     只有测试里读那个变量的那几包才要（当前是 internal/ratelimit）。
#     缺它时 redis_integration_test.go 那几条整组 t.Skip，只在它们里碰得到的
#     变异体落进 Not covered——不进 efficacy 的分母，分数只会变好看；而
#     Killed 仍然大于 0，所以「Killed 与 Lived 同时为 0」那条判据不会触发。
#     不给筛选项就是全部包选中，所以这一条覆盖到 ratelimit。
#     判据不依赖真 Redis：脚本看的是测试文件里的引用，在任何机器上都能跑。
env -u LARKAX_REDIS_DSN LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock13r" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "缺 LARKAX_REDIS_DSN —— 应拒绝运行，实际退出码 $rc"; dump; fi
grep -q '没有 LARKAX_REDIS_DSN' "$last_out" || bad "缺 LARKAX_REDIS_DSN —— 应报出是这个原因"
grep -q '这些包的测试会读它：internal/ratelimit' "$last_out" \
  || bad "缺 LARKAX_REDIS_DSN —— 应列出会整组跳过的包（认的是脚本那句话，不是表里的包名）"
grep -q 't.Skip' "$last_out" || bad "缺 LARKAX_REDIS_DSN —— 应说明那些集成测试会被跳过"

# 14. 同一个变量，只跑**不依赖** Redis 的包时不是前提：筛了 internal/money
#     就该照常跑。判据落在选中包上，不是「脚本里提到这个变量就必须给」——
#     否则只跑 money 的人会被一个与这一包无关的变量挡在门外。
env -u LARKAX_REDIS_DSN LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock14r" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "只跑 internal/money 时不该要 LARKAX_REDIS_DSN，实际退出码 $rc"; dump; fi

# 15. **不许**用 --coverpkg 把覆盖范围收窄。
#     它单独并不改变「哪些测试参与」——实测 internal/keyquota：默认与
#     `--coverpkg=./...` 给同一组数（见 docs/development.md
#     「一个分数要连着它的条件一起读」那张表），所以命令行里没有它。
#     而把它收窄成 `./<pkg>/` 只会让表上的覆盖数变小，且看不出为什么小。
#     这一条要解决的是「范围窄在表上看不出来」，不是去调范围。
#     桩件把收到的参数打出来，这里断言 --coverpkg 不在其中。
stub_args="$tmp/gremlins-args"
{
  stub_head
  cat <<'EOF'
printf '%s\n' "$*" > "$ARGS_OUT"
echo "Killed: 5, Lived: 2, Not covered: 1"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 71.43%"
echo "Mutator coverage: 87.50%"
EOF
} > "$stub_args"
chmod +x "$stub_args"
ARGS_OUT="$tmp/args.txt" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_args" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock13" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "记录参数 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q -- '--coverpkg' "$tmp/args.txt" 2>/dev/null; then
  bad "命令行里冒出 --coverpkg —— 它单独不改变哪些测试参与，收窄只会让覆盖数变小（收到：$(cat "$tmp/args.txt" 2>/dev/null)）"
else
  ok
fi
# 顺带钉住：显式打开的那个开关真的传给了 gremlins。它只在「这把尺子」那一行里
# 被说出来，命令行里漏了的话，说出来的条件就是假的。
grep -q -- '--invert-logical' "$tmp/args.txt" 2>/dev/null \
  || bad "记录参数 —— --invert-logical 应真的传给 gremlins（收到：$(cat "$tmp/args.txt" 2>/dev/null)）"

# 16. 退化路径（没有 flock）崩溃后留下的锁：报错要说清怎么清。
#     这里直接验证提示文本本身——它由 check-mutation.sh 按锁目录里的 PID
#     判断持有者是否还在，PID 不存在就是残留锁。
#     没有 flock 的环境才走 mkdir 路径；有 flock 时锁由内核释放，不会有残锁，
#     所以这一步只在缺 flock 时执行。
if ! command -v flock >/dev/null 2>&1; then
  lock_stale="$tmp/lock-stale"
  mkdir -p "$lock_stale.d"
  printf '%s\n' 4000001 > "$lock_stale.d/pid"   # 这个 PID 不会存在
  LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="$PINNED" LOCK_FILE="$lock_stale" \
    bash "$SCRIPT" internal/money >"$last_out" 2>&1
  rc=$?
  if [ "$rc" -eq 3 ]; then ok; else bad "残留锁 —— 应拒绝执行（退出码 3），实际 $rc"; dump; fi
  grep -q 'rm -rf ' "$last_out" || bad "残留锁 —— 提示里应给出清理命令"
else
  echo "（跳过残留锁一条：本机有 flock，锁由内核释放，不存在残留锁）"
fi

# 17. 存活清单要打出来，并且**带包名与位置**——只打计数的话，
#     docs/development.md 那句「对存活的变异逐个判断」没有可判断的对象。
stub_surv="$tmp/gremlins-survivors"
make_gremlins_with_survivors "$stub_surv"
run "$stub_surv" "$tmp/lock15" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "带存活明细 —— 存活不是失败，应通过，实际退出码 $rc"; dump; fi
grep -q '存活的变异' "$last_out" || bad "带存活明细 —— 应打出存活清单"
# 桩件的输出前面带缩进，抄下来时要归一化成「包名 + 类型 + 位置」。
grep -q 'internal/money  LIVED CONDITIONALS_BOUNDARY at money.go:103:8' "$last_out" \
  || bad "带存活明细 —— 清单里应有「包名 + 类型 + 位置」（缩进要去掉）"
grep -q 'internal/money  LIVED ARITHMETIC_BASE at money.go:54:25' "$last_out" \
  || bad "带存活明细 —— 第二条存活也应列出"

# 17b. 逐行透传 + 进度（#866）。
#
#      以前一个包的输出被整个吞到结束才看得见，跑几十分钟时没法判断「还有五分钟
#      还是五十分钟」。现在每行带序号与已用时间；每 PROGRESS_EVERY 条打一行进度。
#
#      **gremlins v0.6.0 正常运行不报总数**：源码 cmd/unleash.go 里 `Starting...`
#      之后直接是逐条结果，汇总在最后；实际输出也核过。所以默认走降级路径，
#      只报「已完成 + 已用 + 平均每条」，不为了打 ETA 编一个总数。
PROGRESS_EVERY=2 LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_surv" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock17b" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "逐行透传 —— 应通过，实际退出码 $rc"; dump; fi
grep -qE '^\[ *1\] \[[0-9]+s\]  LIVED CONDITIONALS_BOUNDARY at money.go:103:8' "$last_out" \
  || bad "逐行透传 —— 第一行要带序号、已用时间与原样内容"
grep -qE '^\[ *2\] \[[0-9]+s\]   LIVED ARITHMETIC_BASE at money.go:54:25' "$last_out" \
  || bad "逐行透传 —— 序号要逐条递增，原行的缩进保留"
grep -q '── 进度 已完成 2 个变异体，已用 .*平均每个 .*s（gremlins 没报总数，不给 ETA）' "$last_out" \
  || bad "逐行透传 —— 每 N 个变异体要给降级进度（计数 + 已用 + 平均每个）"
grep -qE '^internal/money +8 ' "$last_out" \
  || bad "逐行透传 —— 汇总表那一行照旧要打出来"

# 17c. 拿得到总数时算 ETA。总数只认 gremlins 自己报的形态（`<N> mutants` 或
#      dry run 的 `Runnable: N, Not covered: M`），认不出就走 17b 的降级路径。
stub_total="$tmp/gremlins-total"
{
  stub_head
  cat <<'EOF'
echo "Found 12 mutants"
echo " LIVED CONDITIONALS_BOUNDARY at money.go:103:8"
echo "  LIVED ARITHMETIC_BASE at money.go:54:25"
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
echo "Mutator coverage: 100.00%"
EOF
} >"$stub_total"
chmod +x "$stub_total"
PROGRESS_EVERY=1 LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_total" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock17c" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "有总数时的 ETA —— 应通过，实际退出码 $rc"; dump; fi
grep -q '── 进度 \[1/12\] 已用 .*预估还剩' "$last_out" \
  || bad "有总数时的 ETA —— 要打「已完成/总数」与预估剩余"
grep -q '── 进度 \[2/12\] 已用 .*预估还剩' "$last_out" \
  || bad "有总数时的 ETA —— 逐条递增后仍按同一个总数算"
# 进度只数变异结果行：`Found 12 mutants` 是第 1 行输出，但它不是变异体。
# 若按行数算，第一条 LIVED 会报成 [2/12]，最后会多报一条 [3/12]。
if grep -q '\[3/12\]' "$last_out"; then
  bad "有总数时的 ETA —— 进度把输出行当成了变异体（出现了 [3/12]，实际只有 2 个变异体）"
else
  ok
fi

# 18. 没有存活变异时，不该凭空打出一节空清单。
run "$stub_all_killed" "$tmp/lock16" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "无存活 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q '存活的变异' "$last_out"; then
  bad "无存活 —— 不该打出空的存活清单"
else
  ok
fi

# 16b. 一个包的结果不可信，**不该把其余包的存活清单一起吃掉**（#566）。
#
#      清单说的是所有包，而「这一包不可信」说的是一个包。
#      清单若排在那个判定之后，一个包的坏结果就会把其余所有包的清单一起吃掉。
#      实测一轮全量：store 那一包整包超时之后，屏幕上只剩一个 ⚠ 和汇总表，
#      而其余 11 个包两百余条存活一条都没打印出来——那一轮独占机器跑了一小时。
#
#      桩件看包名作答。store 那一包故意超时超限，money 那一包照常吐一条存活——
#      这样一次运行里就同时有「不可信的包」与「有存活的好包」。
stub_mixed="$tmp/gremlins-mixed"
{
  stub_head
  cat <<'EOF'
case "$*" in
  *ledger/store*)
    echo "Killed: 3, Lived: 1, Not covered: 9"
    echo "Timed out: 282, Not viable: 0, Skipped: 0"
    echo "Test efficacy: 75.00%"
    echo "Mutator coverage: 30.77%"
    ;;
  *)
    echo " LIVED CONDITIONALS_BOUNDARY at money.go:103:8"
    echo "Killed: 8, Lived: 1, Not covered: 3"
    echo "Timed out: 0, Not viable: 0, Skipped: 0"
    echo "Test efficacy: 88.89%"
    echo "Mutator coverage: 75.00%"
    ;;
esac
EOF
} > "$stub_mixed"
chmod +x "$stub_mixed"
run "$stub_mixed" "$tmp/lock16b" money ledger/store
rc=$?
# 判定本身没变：结果不可信仍然要失败、要被看见。
if [ "$rc" -eq 1 ]; then ok; else bad "有包不可信 —— 应失败，实际退出码 $rc"; dump; fi
grep -q '不可信' "$last_out" || bad "有包不可信 —— 要报出是这个原因"
# 而清单照打：好包那一条是这一轮唯一的产出，丢了就只能重跑。
grep -q '存活的变异' "$last_out" || bad "有包不可信 —— 存活清单仍然要打印（#566）"
grep -q 'internal/money  LIVED CONDITIONALS_BOUNDARY at money.go:103:8' "$last_out" \
  || bad "有包不可信 —— 好包那一条存活要出现在清单里"

# ── 「这把尺子」：版本与变异器集合 ────────────────────────────────────────
#
# 这一组是本轮补上的。理由：efficacy 的分母是变异体数，而分母由 gremlins 的
# 变异器集合决定——换一个版本就换一批变异器，同一个包的分数会变而代码一行没动。
# 而「分母变了」在输出里与「代码改好了」长得一模一样，所以「用的是哪把尺子」
# 必须是响的：不符就拒绝运行，并且每次运行都自己说出来。

# 19. 版本不符：必须拒绝运行，并说清钉住的是哪一版、实际是哪一版。
#     这条是活的：把 check-mutation.sh 里那几行校验删掉，它会红
#     （脚本会照常跑完，退出码 0）。
LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" GREMLINS_VERSION="v9.9.9" \
  LOCK_FILE="$tmp/lock17" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "版本不符 —— 应拒绝运行（退出码 1），实际 $rc"; dump; fi
grep -q '版本与钉住的不一致' "$last_out" || bad "版本不符 —— 要说明是版本对不上"
grep -q "钉住：${PINNED}" "$last_out" || bad "版本不符 —— 要打出钉住的是哪一版（$PINNED）"
grep -q '实际：v9.9.9' "$last_out" || bad "版本不符 —— 要打出实际装的是哪一版"
grep -q "gremlins@${PINNED}" "$last_out" || bad "版本不符 —— 要给出该装哪一版的命令"
# 拒绝运行的意思就是**一个包都没跑**：不能跑出半份分数再报错。
# 判据是那张表的表头（`killed`）与收尾那行，不是 grep `efficacy`——
# 报错文案自己就写着「efficacy 的分母」，拿它当判据等于什么都没测。
if grep -q 'killed\|全部跑成' "$last_out"; then
  bad "版本不符 —— 拒绝运行前不该跑出任何分数"
else
  ok
fi

# 20. 版本读不出来（桩件是 bash 脚本，`go version -m` 探不出模块信息）：
#     同样拒绝运行。**不猜**——猜出来的版本比没有版本更坏。
env -u GREMLINS_VERSION LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" \
  LOCK_FILE="$tmp/lock18" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "版本读不出 —— 应拒绝运行，实际 $rc"; dump; fi
grep -q '读不出' "$last_out" || bad "版本读不出 —— 要报出是这个原因"

# 21. 读不出变异器集合（工具答不上 `unleash --help`）：拒绝运行。
#     集合就是分母的来源，读不出就说不清这一轮用的是哪把尺子。
stub_nohelp="$tmp/gremlins-nohelp"
cat > "$stub_nohelp" <<'EOF'
#!/usr/bin/env bash
# 桩件：答不上 --help（模拟换了实现或排版整个变了）
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
EOF
chmod +x "$stub_nohelp"
run "$stub_nohelp" "$tmp/lock19" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "读不出变异器集合 —— 应拒绝运行，实际 $rc"; dump; fi
grep -q '读不出 gremlins 的变异器集合' "$last_out" || bad "读不出变异器集合 —— 要报出是这个原因"

# 22. 正常跑时，第一行就是「这把尺子是什么」：版本 + 变异器集合 + 已开的开关
#     + workers + 系数。**粘进 docs/development.md「各包的现状」那张表的数字自带条件**，不依赖人记得补。
#     集合是探测来的，所以这里逐项比对，而不是只看它打了一行字。
run "$stub_ok" "$tmp/lock20" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "「这把尺子」—— 应通过，实际退出码 $rc"; dump; fi
grep -q '这把尺子' "$last_out" || bad "「这把尺子」—— 每次运行都要打出这一行"
grep -q "gremlins ${PINNED}" "$last_out" || bad "「这把尺子」—— 要带版本"
grep -q 'workers=1' "$last_out" || bad "「这把尺子」—— 要带 workers"
grep -q '系数 10' "$last_out" || bad "「这把尺子」—— 要带超时系数"
grep -q -- '--invert-logical 已开' "$last_out" || bad "「这把尺子」—— 要说明 INVERT_LOGICAL 是显式打开的"
mismatch=""
for m in $HELP_DEFAULT_MUTATORS $HELP_EXTRA_MUTATOR; do
  grep -q "$m" "$last_out" || mismatch="$mismatch $m"
done
if [ -n "$mismatch" ]; then
  bad "「这把尺子」—— 变异器集合少了：$mismatch（实际那行：$(grep '这把尺子' "$last_out"))"
else
  ok
fi
# 反面：默认关着的变异器**不该**出现在那一行里。少了这个断言的话，
# 把「默认集合」写成「--help 里所有变异器」也会绿。
for m in INVERT_ASSIGNMENTS INVERT_BITWISE INVERT_BWASSIGN INVERT_LOOPCTRL REMOVE_SELF_ASSIGNMENTS; do
  if grep -q "$m" "$last_out"; then
    bad "「这把尺子」—— 默认关着的 $m 不该算进这一轮的集合里"
  else
    ok
  fi
done

# 23. `--help` 里有变异器开关，但一个默认开的都没有：拒绝运行。
#     这几乎不可能是真的——更可能是排版变了、`(default true)` 没被认出来。
#     那会让上面那一行少记一批默认变异器**而不报错**，所以这里要挡住。
stub_nodefault="$tmp/gremlins-nodefault"
{
  cat <<'EOF'
#!/usr/bin/env bash
for __a in "$@"; do
  case "$__a" in
    --help|-h)
      echo '      --arithmetic-base             enable "ARITHMETIC_BASE" mutants'
      echo '      --invert-logical              enable "INVERT_LOGICAL" mutants'
      exit 0
      ;;
  esac
done
EOF
  cat <<'EOF'
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
EOF
} > "$stub_nodefault"
chmod +x "$stub_nodefault"
run "$stub_nodefault" "$tmp/lock21" internal/money
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "一个默认变异器都没有 —— 应拒绝运行，实际 $rc"; dump; fi
grep -q 'default true' "$last_out" || bad "一个默认变异器都没有 —— 要报出是这个原因"

# 24. 每个包在调用 gremlins **之前**都清了一次 go 的测试结果缓存（#773）。
#
#     gremlins 的超时预算 = 它那一次 gathering 的耗时 × 系数。gathering 是它内部
#     拼的 `go test -cover -coverprofile …`（没有 `-count=1`），于是会命中 go 的
#     测试结果缓存，预算按缓存那点耗时算：实测同一个包 134ms → 预算 ≈1.34s，
#     清掉之后 5.73s → 预算 ≈57s，而 internal/ledger/store 真跑一遍是 5.1s。
#     缓存命中那一轮整包的数字都不可信（Killed 44 / 存活 0 / 超时 303），清掉后
#     重跑正常 KILLED。
#
#     **判据是「序列」，不是「序列里有这一条」**：缓存是清单里排在前面的包预热出来的
#     （父包跑完，子包那条命令就是 `(cached) 0.218s`），所以一整轮只在循环前清一次
#     挡不住——那正是这条要防的。所以选两个包，断言序列严格是「清、跑、清、跑」。
#     两个包都选不重叠的，免得「范围重叠」那段注干扰对序列的判读。
#
#     go 与 gremlins 都由桩件顶着，两个桩件往同一份日志里各记一笔，于是「谁先谁后」
#     是**观察出来的**，不是从脚本的结构推出来的。
stub_seq="$tmp/gremlins-seq"
{
  stub_head
  cat <<'EOF'
printf 'gremlins-unleash\n' >> "$FAKE_GO_LOG"
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
echo "Mutator coverage: 100.00%"
EOF
} > "$stub_seq"
chmod +x "$stub_seq"

seq_log="$tmp/seq22.txt"
: > "$seq_log"
FAKE_GO_LOG="$seq_log" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_seq" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock22" bash "$SCRIPT" internal/money internal/pipeline >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "清缓存的时点 —— 应通过，实际退出码 $rc"; dump; fi
if awk '
  /^go clean -testcache$/ { if (s == 1) { print "连续两次清理之间没跑 gremlins"; bad = 1 } s = 1; cleans++; next }
  /^gremlins-unleash$/    { if (s != 1) { print "这次 gremlins 之前没有清理"; bad = 1 } s = 0; runs++; next }
  { print "没认出的行：" $0; bad = 1 }
  END {
    if (s != 0) { print "最后一次清理之后没跑 gremlins（清理排在调用之后？）"; bad = 1 }
    if (cleans != 2 || runs != 2) { print "清理 " cleans " 次、跑 gremlins " runs " 次；两个包各应一次"; bad = 1 }
    exit bad
  }' "$seq_log" > "$tmp/seq22-check.txt" 2>&1; then
  ok
else
  bad "清缓存的时点 —— 序列不是「清、跑」逐个交替：$(tr '\n' ' ' < "$tmp/seq22-check.txt")实际序列：$(tr '\n' ' ' < "$seq_log")"
fi

# 22b. 清不掉时必须响，而且**不许照常跑 gremlins**。
#      预算错了的话这一包的分数整体偏移，而超时那一格在表上与「真受了别的负载」
#      长得一样——静默继续跑正是这一处最难被发现的地方。所以这一包判为没跑成。
: > "$seq_log"
FAKE_GO_LOG="$seq_log" FAKE_GO_RC=1 LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_seq" GREMLINS_VERSION="$PINNED" \
  LOCK_FILE="$tmp/lock22b" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "清缓存失败 —— 应判没跑成（退出码 1），实际 $rc"; dump; fi
grep -q 'go clean -testcache 失败' "$last_out" || bad "清缓存失败 —— 要报出是这一步失败了"
grep -q 'internal/money' "$last_out" || bad "清缓存失败 —— 要说清是哪一个包没跑"
if grep -q 'gremlins-unleash' "$seq_log"; then
  bad "清缓存失败 —— 不该照常调 gremlins（缓存没清，超时预算就是错的）"
else
  ok
fi

# 25. `--list-packages`：CI 的选包那一步靠它读清单，所以它必须**不依赖任何
#     运行前提**——选包跑在起数据库、装 gremlins 之前。
#
#     判据四条：
#       a) 缺 gremlins、缺 DSN、锁文件根本建不出来时照样退出 0；
#       b) 不打印「已拿到锁」：这个开关只读数据；
#       c) 打印的每个名字在仓库里都是一个真实目录，且条数非零——防它打出表头
#          或空行，把「清单读到了」变成「读到了一些行」（LESSONS 第 16 条：
#          空清单与干净清单长得一样）；
#       d) 钱的那几个包在——「清单没被清空或截断」的正向证据。
: > "$tmp/notadir"
list_out=$(GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
  LOCK_FILE="$tmp/notadir/lock" bash "$SCRIPT" --list-packages 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then ok; else
  bad "--list-packages —— 应不依赖 gremlins/DSN/锁就跑成，实际退出码 $rc"
  printf '%s\n' "$list_out" | sed 's/^/    | /' | tail -5
fi
if printf '%s\n' "$list_out" | grep -q '已拿到锁'; then
  bad "--list-packages —— 不该拿锁"
else
  ok
fi
list_count=$(printf '%s\n' "$list_out" | grep -c .)
if [ "$list_count" -gt 0 ]; then ok; else bad "--list-packages —— 一条包名都没打印"; fi
missing_dirs=$(printf '%s\n' "$list_out" | while IFS= read -r p; do [ -d "$p" ] || printf '%s\n' "$p"; done)
if [ -z "$missing_dirs" ]; then ok; else
  bad "--list-packages —— 这些不是仓库里的目录：$(printf '%s' "$missing_dirs" | tr '\n' ' ')"
fi
for want in internal/money internal/billing internal/ledger internal/pricing internal/pipeline; do
  if printf '%s\n' "$list_out" | grep -qx "$want"; then ok; else bad "--list-packages —— 清单里缺了 $want"; fi
done

# 25b. 不带 `--match` 时输出必须与 `PKGS` 数组逐字一致（内容与顺序）。
#
#      CI 与用例都依赖「这一条命令只打印清单」这个形状（#866 评审要求改参数后
#      原样保留）。这里不用 `--list-packages` 自己的打印结果做对照，而是从源码里
#      把 `PKGS=(...)` 那一段独立抽出来比：这样钉住的是打印路径，而不是
#      「脚本打印了它自己刚读到的同一个数组」这种同义反复。
pkgs_from_source=$(awk '/^PKGS=\(/{f=1;next} f&&/^\)/{exit} f{sub(/^[[:space:]]+/,"");sub(/[[:space:]]+$/,"");if(length($0))print}' "$SCRIPT")
if [ "$list_out" = "$pkgs_from_source" ]; then
  ok
else
  bad "--list-packages —— 输出与 PKGS 数组不一致（内容或顺序变了）"
  diff <(printf '%s\n' "$list_out") <(printf '%s\n' "$pkgs_from_source") | head -5 | sed 's/^/    | /'
fi

# 26. `--list-packages --match <目录>`：CI 选包步骤靠它把「改动目录」映射到
#     「该跑哪一条 PKGS」。判据是**最近的、在清单里的祖先**：
#       - 目录本身在清单里 → 取它自己（internal/ledger/store 不能被并到父包，
#         否则 store 那几百个变异体的数字会从表里消失）；
#       - 子包不在清单里 → 取最近的祖先（internal/billing/calendaradapter →
#         internal/billing）。这一条正是 #866 评审里的失败形态：精确相等会把
#         「只改了计费代码」的 PR 判成「没有可跑的包」；
#       - 整棵树里没有覆盖它的条目 → 什么都不打印、退出 1，选包步骤据此报出
#         「在树里但没有可跑的包」，而不是混进「改动不涉及 MONEY_PKGS」。
match_one() {
  match_out=$(GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
    LOCK_FILE="$tmp/notadir/lock" bash "$SCRIPT" --list-packages --match "$1" 2>&1)
  match_rc=$?
}

match_one internal/billing/calendaradapter
if [ "$match_rc" -eq 0 ] && [ "$match_out" = "internal/billing" ]; then
  ok
else
  bad "--match —— 子包应映射到清单里的最近祖先 internal/billing，实际 rc=$match_rc 输出=<$match_out>"
fi
match_one internal/ledger/store
if [ "$match_rc" -eq 0 ] && [ "$match_out" = "internal/ledger/store" ]; then
  ok
else
  bad "--match —— 目录本身在清单里时应取它自己，实际 rc=$match_rc 输出=<$match_out>"
fi
match_one internal/billing/expr
if [ "$match_rc" -eq 0 ] && [ "$match_out" = "internal/billing/expr" ]; then
  ok
else
  bad "--match —— 最近的祖先是更深的条目时要取更深那条，实际 rc=$match_rc 输出=<$match_out>"
fi
match_one internal/money
if [ "$match_rc" -eq 0 ] && [ "$match_out" = "internal/money" ]; then
  ok
else
  bad "--match —— 顶层包应原样返回，实际 rc=$match_rc 输出=<$match_out>"
fi
match_one internal/no-such-package-$$
if [ "$match_rc" -eq 1 ] && [ -z "$match_out" ]; then
  ok
else
  bad "--match —— 没有条目覆盖时应空输出且退出 1，实际 rc=$match_rc 输出=<$match_out>"
fi

# 27. `--list-packages --suffix <片段>`：手工触发的后缀匹配也由脚本回答。
#     workflow 不再自己写一遍 `case "$p" in *"$f")`——两处各写一份，就会像
#     #866 评审第 1、4 条那样刚出生就漂。
suffix_one() {
  suffix_out=$(GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
    LOCK_FILE="$tmp/notadir/lock" bash "$SCRIPT" --list-packages --suffix "$1" 2>&1)
  suffix_rc=$?
}

suffix_one money
if [ "$suffix_rc" -eq 0 ] && [ "$suffix_out" = "internal/money" ]; then
  ok
else
  bad "--suffix —— 应打出 internal/money，实际 rc=$suffix_rc 输出=<$suffix_out>"
fi
suffix_one ledger/store
if [ "$suffix_rc" -eq 0 ] && [ "$suffix_out" = "internal/ledger/store" ]; then
  ok
else
  bad "--suffix —— 应按后缀打出 internal/ledger/store，实际 rc=$suffix_rc 输出=<$suffix_out>"
fi
suffix_one no-such-suffix-$$
if [ "$suffix_rc" -eq 1 ] && [ -z "$suffix_out" ]; then
  ok
else
  bad "--suffix —— 没有匹配时应空输出且退出 1，实际 rc=$suffix_rc 输出=<$suffix_out>"
fi

# 27b. `--list-packages --money`：PR 自动轮的**触发范围**也只从脚本读一份。
#
#      这一节防的是一次已经发生过的分叉：`internal/plans` 在 `PKGS` 里，却不在
#      工作流自己列的前缀里——于是动了结转算式的 PR 照报「变异通过」，而那个包
#      连分母都没进，也不告警（告警只覆盖「在前缀里、但没有 PKGS 条目」那一种）。
#      判据四条：
#        a) 每一行以 `/` 结尾，工作流直接拿它做 `case "$f" in "$pre"*)`；
#        b) 去掉结尾 `/` 后 `--list-packages --match` 必须成功——这个前缀确实有
#           PKGS 条目覆盖，否则「触发了却没有包可跑」会显示成跳过；
#        c) 负控：把前缀换成 PKGS 里不存在的目录，**同一条检查必须红**。没有
#           (c)，(b) 就分不清「检查通过」与「检查压根没看数据」（LESSONS 第 2、7 条）；
#        d) `internal/plans` 在清单里——今天分叉的那一格，钉住它。
money_out=$(GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
  LOCK_FILE="$tmp/notadir/lock" bash "$SCRIPT" --list-packages --money 2>&1)
money_rc=$?
if [ "$money_rc" -eq 0 ] && [ -n "$money_out" ]; then ok; else
  bad "--money —— 应不依赖 gremlins/DSN/锁就跑成并打出前缀，实际 rc=$money_rc 输出=<$money_out>"
fi
malformed=$(printf '%s\n' "$money_out" | grep -v '/$' | grep .)
if [ -z "$malformed" ]; then ok; else
  bad "--money —— 这些行不以 / 结尾（工作流要用它做前缀匹配）：$(printf '%s' "$malformed" | tr '\n' ' ')"
fi
if printf '%s\n' "$money_out" | grep -qx 'internal/plans/'; then ok; else
  bad "--money —— 清单里缺了 internal/plans：结转算式在那里，缺了就又会「改了钱却不量」"
fi

# check_money_prefixes <脚本>：对 `--money` 的每一行核 (b)。全部有覆盖返回 0；
# 有前缀没有 PKGS 条目时打印它并返回 1。正控与负控调同一个函数——负控要红的
# 就是这条判据本身，不是另写一段。
check_money_prefixes() {
  local script="$1" line pre rc=0
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    case "$line" in
      */) ;;
      *) echo "前缀不以 / 结尾：$line"; rc=1; continue ;;
    esac
    pre="${line%/}"
    if ! GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
      LOCK_FILE="$tmp/notadir/lock" bash "$script" --list-packages --match "$pre" >/dev/null 2>&1; then
      echo "前缀 $line 没有任何 PKGS 条目覆盖"
      rc=1
    fi
  done < <(GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
    LOCK_FILE="$tmp/notadir/lock" bash "$script" --list-packages --money 2>/dev/null)
  return "$rc"
}

money_check=$(check_money_prefixes "$SCRIPT")
if [ "$?" -eq 0 ]; then ok; else
  bad "--money —— 有前缀没有 PKGS 条目覆盖：$(printf '%s' "$money_check" | tr '\n' ' ')"
fi

# 负控：把 MONEY_PKGS 里的 internal/plans 换成不存在的目录，同一条检查必须红。
# 副本放在 $tmp 下，不动工作树；先确认副本里真的换掉了，否则「没红」可能只是
# 因为 sed 没生效——那时这组负控自己是假的。
neg_script="$tmp/check-mutation-neg.sh"
awk '
  /^MONEY_PKGS=\(/ { inmoney = 1 }
  inmoney && /^  internal\/plans$/ { print "  internal/no-such-money-pkg"; next }
  inmoney && /^\)$/ { inmoney = 0 }
  { print }
' "$SCRIPT" > "$neg_script"
if bash "$neg_script" --list-packages --money 2>/dev/null | grep -qx 'internal/no-such-money-pkg/'; then ok; else
  bad "--money 负控 —— 临时副本里没换上那个不存在的目录，下面的负控不成立"
fi
neg_out=$(check_money_prefixes "$neg_script")
neg_rc=$?
if [ "$neg_rc" -ne 0 ] && printf '%s' "$neg_out" | grep -q 'internal/no-such-money-pkg/'; then ok; else
  bad "--money 负控 —— 前缀不在 PKGS 里时结构断言应报出它并失败，实际 rc=$neg_rc 输出=<$neg_out>"
fi

# ── 按改动收窄（#883）────────────────────────────────────────────────────
#
# 收窄的做法是「排除没改到的」：每个包里没有出现在改动清单里的生产 `.go` 文件，
# 逐个作为 `--exclude-files` 传给 gremlins。下面钉住五件事：
#
#   28. `-E` 的拼装形态：锚定两端、转义点号、包相对路径、一个文件一条，
#       并且被改到的文件不出现在里面；
#   29. 一个包里只有 `_test.go` 变了：不排除任何文件，跑整包；
#   30. 新文件（清单里但 git 还没跟踪）算「改到」，不被排除；
#   31. 改动同时落在父子包：从清单推出来的重叠自己去重，只留外层，不报错；
#   32. 只改了子包：选中子包，不折到父包（父包那几百个变异体不该跟着跑）。
#
# 夹具用仓库里真实的目录树与真实的改动清单，但 gremlins 是桩件——这里要测的是
# 「参数怎么拼」，不是变异本身。文件总数**现算**（与 check-mutation.sh 同一条
# find），不写死：这几个数就是目录树的形状，写死了每加一个生产文件都要回来改
# 一遍测试，而这里要钉的是「摘要里的数等于目录里的数」，不是某一天的数字。
# 第 34 条是例外：它自带一个临时 git 仓库（见那一条），因为 `--changed-from`
# 要从仓库历史现算改动，不能依赖调用自测的这棵树有没有 `.git`。

narrow_seq=0

# count_prod_files <包目录>：数这个包（递归）里参与变异的生产文件。
#
# 与 check-mutation.sh 的 find 逐字一致。自测与被测脚本各算各的：两个数对不上，
# 说明摘要里的口径与目录里的文件不一致——那正是这条门禁要钉的关系，而不是某一天
# 的具体数字。
count_prod_files() {
  find "$1" -type f -name '*.go' ! -name '*_test.go' | wc -l | tr -d ' '
}

# run_narrow <改动清单文件> [参数...]：跑一轮收窄，参数落在 LAST_ARGS。
run_narrow() {
  local list="$1"; shift
  narrow_seq=$((narrow_seq + 1))
  LAST_ARGS_LOG="$tmp/gremlins-args-$narrow_seq"
  : >"$LAST_ARGS_LOG"
  GREMLINS_ARGS_LOG="$LAST_ARGS_LOG" \
    run "$stub_ok" "$tmp/lock-narrow-$narrow_seq" --changed-files "$list" "$@"
  LAST_RC=$?
  LAST_ARGS=$(cat "$LAST_ARGS_LOG")
}

# 28. `-E` 的拼装形态。
#
#     改动清单：internal/ledger/account.go 与 internal/ledger/store/posting.go。
#     选中 internal/ledger（递归含 store），两个文件参与，其余全部排除。
#     要看的是：锚定与转义（`^balance\.go$`）、子目录用包相对路径
#     （`^store/payout\.go$`）、一个文件一条（不是逗号拼一条），
#     以及被改到的两个文件**不**出现在排除里。基础开关（`--invert-logical`）
#     也不能被收窄挤掉。
ledger_files=$(count_prod_files internal/ledger)
ledger_excluded=$((ledger_files - 2))
printf '%s\n' internal/ledger/account.go internal/ledger/store/posting.go >"$tmp/changed-excl.txt"
run_narrow "$tmp/changed-excl.txt" internal/ledger
if [ "$LAST_RC" -eq 0 ]; then ok; else bad "收窄 -E —— 应通过，实际退出码 $LAST_RC"; dump; fi
grep -q "参与 2 / 共 $ledger_files 个生产文件，排除 $ledger_excluded 个未改动的" "$last_out" \
  || bad "收窄 -E —— 摘要要说清参与 2 / 共 $ledger_files，排除 $ledger_excluded"
grep -q '与整包基线不可比' "$last_out" \
  || bad "收窄 -E —— 摘要要说清分数与整包基线不可比"
if printf '%s\n' "$LAST_ARGS" | grep -qF -- '--exclude-files=^balance\.go$'; then
  ok
else
  bad "收窄 -E —— 顶层文件要锚定两端并转义点号（--exclude-files=^balance\\.go\$）"
fi
if printf '%s\n' "$LAST_ARGS" | grep -qF -- '--exclude-files=^store/payout\.go$'; then
  ok
else
  bad "收窄 -E —— 子目录文件要用包相对路径（--exclude-files=^store/payout\\.go\$）"
fi
if printf '%s\n' "$LAST_ARGS" | grep -qF -- 'account.go'; then
  bad "收窄 -E —— 被改到的文件不该出现在排除里（account.go）"
else
  ok
fi
if printf '%s\n' "$LAST_ARGS" | grep -qF -- 'store/posting.go'; then
  bad "收窄 -E —— 被改到的子包文件不该出现在排除里（store/posting.go）"
else
  ok
fi
excl_count=$(printf '%s\n' "$LAST_ARGS" | grep -o -- '--exclude-files=' | wc -l)
if [ "$excl_count" -eq "$ledger_excluded" ]; then ok; else
  bad "收窄 -E —— 要一个文件一条 -E（预期 $ledger_excluded 条，实际 $excl_count 条）"
fi
if printf '%s\n' "$LAST_ARGS" | grep -q -- '--invert-logical'; then ok; else
  bad "收窄 -E —— 收窄不该挤掉基础开关（--invert-logical）"
fi
if grep -qx "LARKAX_MUTATION_SCOPE=narrow excluded=$ledger_excluded" "$last_out"; then ok; else
  bad "收窄 -E —— 口径标记应为 LARKAX_MUTATION_SCOPE=narrow excluded=$ledger_excluded"
fi

# 29. 只有测试文件变了：跑整包，不传任何 `-E`。
#
#     这种改动的风险恰恰是「测试被改弱」——排除文件会把要看的地方排除掉。
printf '%s\n' internal/ledger/account_test.go >"$tmp/changed-testonly.txt"
run_narrow "$tmp/changed-testonly.txt" internal/ledger
if [ "$LAST_RC" -eq 0 ]; then ok; else bad "只改测试 —— 应通过，实际退出码 $LAST_RC"; dump; fi
grep -q 'internal/ledger *整包（这一包没有生产文件改动）' "$last_out" \
  || bad "只改测试 —— 摘要要说清这一包跑整包"
if printf '%s\n' "$LAST_ARGS" | grep -qF -- '--exclude-files'; then
  bad "只改测试 —— 不该排除任何文件"
else
  ok
fi
if grep -qx 'LARKAX_MUTATION_SCOPE=full' "$last_out"; then ok; else
  bad "只改测试 —— 口径标记应为 full（这一轮实际跑整包）"
fi

# 30. 新文件算「改到」。
#
#     判据是改动清单本身，不是 git 的文件状态：一个还没被跟踪的新文件在清单里，
#     就不该被排除。夹具是一个临时探针文件（用例结束就删），名字带 PID，
#     避免与并发跑同一份自测的另一个实例撞名。
probe_file="internal/money/zz_narrow_probe_$$.go"
trap 'rm -f "$probe_file"; rm -rf "$tmp"' EXIT
printf 'package money\n' >"$probe_file"
printf '%s\n' "$probe_file" >"$tmp/changed-new.txt"
run_narrow "$tmp/changed-new.txt" internal/money
if [ "$LAST_RC" -eq 0 ]; then ok; else bad "新文件不被排除 —— 应通过，实际退出码 $LAST_RC"; dump; fi
money_files=$(count_prod_files internal/money)
money_excluded=$((money_files - 1))
grep -q "参与 1 / 共 $money_files 个生产文件，排除 $money_excluded 个未改动的" "$last_out" \
  || bad "新文件不被排除 —— 摘要应把新文件算作参与（参与 1 / 共 $money_files）"
if printf '%s\n' "$LAST_ARGS" | grep -qF -- '--exclude-files=^money\.go$'; then
  ok
else
  bad "新文件不被排除 —— 没改到的 money.go 应被排除"
fi
probe_base=$(basename "$probe_file")
if printf '%s\n' "$LAST_ARGS" | grep -qF -- "$probe_base"; then
  bad "新文件不被排除 —— 清单里的新文件被排除了（$probe_base）"
else
  ok
fi

# 31. 改动同时落在父子包：从清单推出来的重叠自己去重，只留外层，不报错。
#
#     显式传父子两条参数仍要拒绝（第 1d 条），那是写参数的人犯的错；这里是从
#     改动清单推出来的，两边都跑会把子包的变异体跑两遍。外层递归就含子包，
#     所以只留 internal/ledger，`-E` 按整棵子树算（参与 2 个）。
printf '%s\n' internal/ledger/account.go internal/ledger/store/read.go >"$tmp/changed-parentchild.txt"
run_narrow "$tmp/changed-parentchild.txt"
if [ "$LAST_RC" -eq 0 ]; then ok; else bad "父子去重 —— 应通过（去重，不报错），实际退出码 $LAST_RC"; dump; fi
grep -q '改动同时落在父子包' "$last_out" \
  || bad "父子去重 —— 要说清发生了什么"
grep -q "参与 2 / 共 $ledger_files 个生产文件，排除 $ledger_excluded 个未改动的" "$last_out" \
  || bad "父子去重 —— 外层应按整棵子树算（参与 2 / 共 $ledger_files）"
if grep -qE '^internal/ledger +[0-9]' "$last_out"; then ok; else
  bad "父子去重 —— 外层包要跑（表里应有 internal/ledger 那一行）"
fi
if grep -qE '^internal/ledger/store +[0-9]' "$last_out"; then
  bad "父子去重 —— 子包不该再单独跑一遍（表里出现了 internal/ledger/store 一行）"
else
  ok
fi
if grep -q '包参数的范围重叠' "$last_out"; then
  bad "父子去重 —— 从清单推出来的重叠不该按「参数写错」拒绝"
else
  ok
fi

# 32. 只改了子包：选中子包，不折到父包。
#
#     包路径递归，但选包取的是**最近的**祖先：store 自己就在清单里，所以只跑
#     store 那一条，父包的文件与 store 的其余文件都被排除。折到父包会让整棵
#     子树全跑，正是这条要防的。
store_files=$(count_prod_files internal/ledger/store)
store_excluded=$((store_files - 1))
printf '%s\n' internal/ledger/store/read.go >"$tmp/changed-child.txt"
run_narrow "$tmp/changed-child.txt"
if [ "$LAST_RC" -eq 0 ]; then ok; else bad "只改子包 —— 应通过，实际退出码 $LAST_RC"; dump; fi
grep -q "参与 1 / 共 $store_files 个生产文件，排除 $store_excluded 个未改动的" "$last_out" \
  || bad "只改子包 —— 应按子包算（参与 1 / 共 $store_files），不能折到父包"
if grep -qE '^internal/ledger/store +[0-9]' "$last_out"; then ok; else
  bad "只改子包 —— 表里应有 internal/ledger/store 那一行"
fi
if grep -qE '^internal/ledger +[0-9]' "$last_out"; then
  bad "只改子包 —— 不该折到父包（表里出现了 internal/ledger 一行）"
else
  ok
fi

# 33. 不给收窄开关时是整包口径：一个 `-E` 都不传。
#
#     本地与 CI 手工触发走的都是这条路，别让收窄把默认口径悄悄改掉。
args_log="$tmp/gremlins-args-default"
: >"$args_log"
GREMLINS_ARGS_LOG="$args_log" run "$stub_ok" "$tmp/lock-default" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "默认口径 —— 应通过，实际退出码 $rc"; dump; fi
if grep -qF -- '--exclude-files' "$args_log"; then
  bad "默认口径 —— 不给 --changed-* 时不该传任何 --exclude-files"
else
  ok
fi
if grep -q '按改动收窄' "$last_out"; then
  bad "默认口径 —— 不打收窄那一段"
else
  ok
fi

# 33a. 收窄到「没有可变代码」的文件：gremlins 正常退出，但说没有结果。
#
#      internal/billing/expr/doc.go 只有包注释，真 gremlins 对它输出
#      `No results to report.` 并退出 0。这不是「没跑成」——这一组文件里确实
#      没有可变的代码；判失败会让「只改一行注释」的 PR 永远过不去。
#
#      **认的是那一句原样输出**，不是「输出为空」。空输出是格式变了的形状。
stub_nomutants="$tmp/gremlins-nomutants"
{
  stub_head
  cat <<'EOF'
echo "Starting..."
echo "Gathering coverage..."
echo "No results to report."
EOF
} >"$stub_nomutants"
chmod +x "$stub_nomutants"

printf '%s\n' internal/billing/expr/doc.go >"$tmp/changed-doc.txt"
GREMLINS_ARGS_LOG=/dev/null LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_nomutants" \
  GREMLINS_VERSION="$PINNED" LOCK_FILE="$tmp/lock-nomutants" \
  bash "$SCRIPT" --changed-files "$tmp/changed-doc.txt" internal/billing/expr >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "收窄到无可变代码 —— 收窄模式应通过，实际退出码 $rc"; dump; fi
grep -q '这一包收窄后没有可变的代码（0 个变异体）' "$last_out" \
  || bad "收窄到无可变代码 —— 要打一行说明，不能静默算通过"

# 33b. 同样的输出在整包模式下仍然是失败。
#
#     整包一个变异体都没有，说明清单或工具出了问题，而不是「这一包没有可变的
#     代码」。两种情形不能用同一条判据放过去。
run "$stub_nomutants" "$tmp/lock-nomutants-full" internal/billing/expr
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "整包无可变代码 —— 应判失败，实际退出码 $rc"; dump; fi
grep -q '认不出 gremlins 的输出' "$last_out" \
  || bad "整包无可变代码 —— 要说清是认不出输出"

# 33c. 退回整包的那一支不能走绿分支。
#
#      包里只改了 `_test.go` 时，脚本退回整包（口径标记自己打 full）。这时
#      gremlins 打「没有结果」并退出 0，仍然是「整包一个变异体都没有」——说明
#      清单或工具出了问题，必须判失败。判据一旦从「这一包有生产文件参与收窄」
#      放宽成「这一包有生产文件」，这一支就会静默变绿。
printf '%s\n' internal/billing/expr/compile_test.go >"$tmp/changed-testonly-nomutants.txt"
GREMLINS_ARGS_LOG=/dev/null LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_nomutants" \
  GREMLINS_VERSION="$PINNED" LOCK_FILE="$tmp/lock-nomutants-testonly" \
  bash "$SCRIPT" --changed-files "$tmp/changed-testonly-nomutants.txt" internal/billing/expr >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "退回整包 + 没有结果 —— 应判失败，实际退出码 $rc"; dump; fi
grep -q '认不出 gremlins 的输出' "$last_out" \
  || bad "退回整包 + 没有结果 —— 要说清是认不出输出"
if grep -q '这一包收窄后没有可变的代码' "$last_out"; then
  bad "退回整包 + 没有结果 —— 不该走「收窄后没有可变代码」的绿分支"
else
  ok
fi

# 34. `--changed-from <ref>`：改动清单从 git 现算，未跟踪的新文件也算。
#
#     跑在一个**临时 git 仓库**里，不依赖调用自测的这棵树有没有 `.git`：
#     门禁镜像用 rsync 同步、排除 `.git`，在真仓库里跑会得到「bad revision」
#     而变成「没测成」。临时仓库自带 `scripts/check-mutation.sh` 的副本，脚本
#     的 `cd 到仓库根` 落在那里面；gremlins 与 go 仍是桩件，这里测的是清单怎么算。
#
#     夹具：提交两个生产文件与一个测试文件，然后改掉 money.go、再加一个未跟踪的
#     new.go。期望参与 2（money.go + new.go）、排除 1（rate.go），测试文件不计。
#
#     git 不在 PATH 时报错而不是跳过：那是「测不成」，不能让它看起来像通过。
if ! command -v git >/dev/null 2>&1; then
  bad "--changed-from —— 找不到 git，这条用例无法验证"
else
  gitroot="$tmp/gitroot"
  mkdir -p "$gitroot/scripts" "$gitroot/internal/money"
  cp "$SCRIPT" "$gitroot/scripts/check-mutation.sh"

  printf 'package money\n' >"$gitroot/internal/money/money.go"
  printf 'package money\n' >"$gitroot/internal/money/rate.go"
  printf 'package money\n' >"$gitroot/internal/money/money_test.go"

  # 身份与 gpg 签名都用 -c 显式给（与第 9c 条共用同一份 git_cfg）：这台机器上
  # 没有全局 git 身份，而 commit 失败会变成另一种「没测成」。
  git -C "$gitroot" "${git_cfg[@]}" -c init.defaultBranch=main init -q
  git -C "$gitroot" "${git_cfg[@]}" add -A
  git -C "$gitroot" "${git_cfg[@]}" -c commit.gpgsign=false commit -q -m fixture

  # 已跟踪的改动与未跟踪的新文件各一个，两条都要进清单。
  printf 'package money\n\n// changed\n' >"$gitroot/internal/money/money.go"
  printf 'package money\n' >"$gitroot/internal/money/new.go"

  args_log="$tmp/gremlins-args-from"
  : >"$args_log"
  GREMLINS_ARGS_LOG="$args_log" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" \
    GREMLINS_VERSION="$PINNED" LOCK_FILE="$tmp/lock-from" \
    bash "$gitroot/scripts/check-mutation.sh" --changed-from HEAD internal/money >"$last_out" 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then ok; else bad "--changed-from —— 应通过，实际退出码 $rc"; dump; fi
  grep -q '参与 2 / 共 3 个生产文件，排除 1 个未改动的' "$last_out" \
    || bad "--changed-from —— 已跟踪的改动与未跟踪的新文件都要算进清单（参与 2 / 共 3）"
  if grep -qF -- '--exclude-files=^money\.go$' "$args_log"; then
    bad "--changed-from —— 已跟踪的改动不该被排除（money.go）"
  else
    ok
  fi
  if grep -qF -- '--exclude-files=^new\.go$' "$args_log"; then
    bad "--changed-from —— 未跟踪的新文件不该被排除（new.go）"
  else
    ok
  fi
  if grep -qF -- '--exclude-files=^rate\.go$' "$args_log"; then ok; else
    bad "--changed-from —— 没改到的 rate.go 应被排除"
  fi
fi

# 35. 改动清单读不了、或者里面没有落在 PKGS 包里的 Go 文件：都要响。
GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
  bash "$SCRIPT" --changed-files "$tmp/no-such-list-$$" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "读不了改动清单 —— 应退出 1，实际 $rc"; dump; fi
grep -q '读不了这份改动清单' "$last_out" || bad "读不了改动清单 —— 要说清是哪一份"

printf '%s\n' docs/development.md >"$tmp/changed-nopkg.txt"
GREMLINS="gremlins-not-installed-$$" LARKAX_DB_DSN="" \
  bash "$SCRIPT" --changed-files "$tmp/changed-nopkg.txt" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "清单里没有可跑的包 —— 应退出 1，实际 $rc"; dump; fi
grep -q '没有落在任何 PKGS 包里的 Go 文件' "$last_out" \
  || bad "清单里没有可跑的包 —— 要说清这次什么都没检查"

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail，没测成 $untested"
[ "$fail" -eq 0 ] || exit 1
[ "$untested" -eq 0 ] || exit 2
