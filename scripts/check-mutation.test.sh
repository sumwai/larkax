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
make_gremlins() {
  local path="$1" killed="$2" lived="$3" timed="$4" eff="$5" rc="${6:-0}"
  {
    stub_head
    cat <<EOF
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

# 1d. 清单里父子两条同时选中时，要说清范围是重叠的（#769）。
#
#     包路径是递归口径：`./internal/ledger/` 会把子包 internal/ledger/store 的
#     变异体一起跑，于是 store 在表上各算一次。不说的话，那两行会被读成互不
#     相干的两个包，而左边那一行其实读作「ledger + store」。
#     **这句话从 PKGS 算出来**，所以两个方向都要钉：都选中时要说，只选子包时不说。
run "$stub_ok" "$tmp/lock1d" internal/ledger internal/ledger/store
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "父子两条同时选中 —— 应通过，实际退出码 $rc"; dump; fi
grep -q 'internal/ledger 含 internal/ledger/store' "$last_out" \
  || bad "父子两条同时选中 —— 要说明 internal/ledger 那一行含子包 store"
if grep -q 'internal/billing 含' "$last_out"; then
  bad "父子两条同时选中 —— 没选中的 billing / expr 不该被列出来"
else
  ok
fi
# 只选子包时没有重叠可言：父包这一轮根本没跑。
run "$stub_ok" "$tmp/lock1d-child" internal/ledger/store
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "只选子包 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q '范围是重叠的' "$last_out"; then
  bad "只选子包 —— 不该说重叠（父包这一轮没跑）"
else
  ok
fi
# 只选父包时也不说：它这一次跑出来的**就是** ledger + store，表上没有第二行
# 与它重叠，这时说一句「范围重叠」反而会让人以为表里还有另一个 store。
run "$stub_ok" "$tmp/lock1d-parent" internal/ledger
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "只选父包 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q '范围是重叠的' "$last_out"; then
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
#      不会被别的机制补上（CI 里没有 gremlins）。
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

# 13. **不许**用 --coverpkg 把覆盖范围收窄。
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

# 14. 退化路径（没有 flock）崩溃后留下的锁：报错要说清怎么清。
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

# 15. 存活清单要打出来，并且**带包名与位置**——只打计数的话，
#     docs/development.md 那句「对存活的变异逐个判断」没有可判断的对象。
stub_surv="$tmp/gremlins-survivors"
make_gremlins_with_survivors "$stub_surv"
run "$stub_surv" "$tmp/lock15" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "带存活明细 —— 存活不是失败，应通过，实际退出码 $rc"; dump; fi
grep -q '存活的变异' "$last_out" || bad "带存活明细 —— 应打出存活清单"
# 桩件的输出前面带缩进，抄下来时要归一化成「包名 + 类型 + 位置」。
grep -q 'internal/money  CONDITIONALS_BOUNDARY at money.go:103:8' "$last_out" \
  || bad "带存活明细 —— 清单里应有「包名 + 类型 + 位置」（缩进要去掉）"
grep -q 'internal/money  ARITHMETIC_BASE at money.go:54:25' "$last_out" \
  || bad "带存活明细 —— 第二条存活也应列出"

# 16. 没有存活变异时，不该凭空打出一节空清单。
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
grep -q 'internal/money  CONDITIONALS_BOUNDARY at money.go:103:8' "$last_out" \
  || bad "有包不可信 —— 好包那一条存活要出现在清单里"

# ── 「这把尺子」：版本与变异器集合 ────────────────────────────────────────
#
# 这一组是本轮补上的。理由：efficacy 的分母是变异体数，而分母由 gremlins 的
# 变异器集合决定——换一个版本就换一批变异器，同一个包的分数会变而代码一行没动。
# 而「分母变了」在输出里与「代码改好了」长得一模一样，所以「用的是哪把尺子」
# 必须是响的：不符就拒绝运行，并且每次运行都自己说出来。

# 17. 版本不符：必须拒绝运行，并说清钉住的是哪一版、实际是哪一版。
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

# 18. 版本读不出来（桩件是 bash 脚本，`go version -m` 探不出模块信息）：
#     同样拒绝运行。**不猜**——猜出来的版本比没有版本更坏。
env -u GREMLINS_VERSION LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" \
  LOCK_FILE="$tmp/lock18" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "版本读不出 —— 应拒绝运行，实际 $rc"; dump; fi
grep -q '读不出' "$last_out" || bad "版本读不出 —— 要报出是这个原因"

# 19. 读不出变异器集合（工具答不上 `unleash --help`）：拒绝运行。
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

# 20. 正常跑时，第一行就是「这把尺子是什么」：版本 + 变异器集合 + 已开的开关
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

# 21. `--help` 里有变异器开关，但一个默认开的都没有：拒绝运行。
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

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail，没测成 $untested"
[ "$fail" -eq 0 ] || exit 1
[ "$untested" -eq 0 ] || exit 2
