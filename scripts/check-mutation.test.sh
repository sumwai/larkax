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
# 每个用例用独立的锁文件，互不干扰，也不会撞上正在跑的真实实例。
#
# 改动 check-mutation.sh 后跑：
#   bash scripts/check-mutation.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-mutation.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# 用例输出留在这里，失败时打印出来便于定位。
last_out="$tmp/last.txt"

ok()   { pass=$((pass + 1)); }
bad()  { echo "✗ $1"; fail=$((fail + 1)); }

# ── 假 gremlins ───────────────────────────────────────────────────────────
#
# 输出照抄 gremlins 的汇总行：脚本读的就是这几行。

# make_gremlins <路径> <killed> <lived> <timed> <efficacy> [退出码]
make_gremlins() {
  local path="$1" killed="$2" lived="$3" timed="$4" eff="$5" rc="${6:-0}"
  cat > "$path" <<EOF
#!/usr/bin/env bash
# 桩件：check-mutation.sh 的自测用，不是真的变异测试
echo "Killed: $killed, Lived: $lived, Not covered: 3"
echo "Timed out: $timed, Not viable: 0, Skipped: 0"
echo "Test efficacy: $eff"
echo "Mutator coverage: 100.00%"
exit $rc
EOF
  chmod +x "$path"
}

# 没有那几个汇总行的桩件：模拟 gremlins 升级后格式变了。
make_gremlins_unknown_format() {
  local path="$1"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
# 桩件：一段认不出格式的输出
echo "gremlins v9: ran some mutants, all good probably"
echo "done"
EOF
  chmod +x "$path"
}

# 跑得慢的桩件：用来在第一个实例持锁期间发第二个实例。
make_gremlins_slow() {
  local path="$1" secs="$2"
  cat > "$path" <<EOF
#!/usr/bin/env bash
# 桩件：慢速版本，模拟一次真实的变异测试
sleep $secs
echo "Killed: 5, Lived: 2, Not covered: 1"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 71.43%"
EOF
  chmod +x "$path"
}

# 带存活明细的桩件：gremlins 每测一个变异会打一行
# `LIVED <类型> at <文件>:<行>:<列>`（前面可能带缩进）。脚本要把它们抄下来，
# 否则「对存活的变异逐个判断」这句话没有可判断的对象。
make_gremlins_with_survivors() {
  local path="$1"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
# 桩件：汇总行之外还带存活明细
echo " LIVED CONDITIONALS_BOUNDARY at money.go:103:8"
echo "  LIVED ARITHMETIC_BASE at money.go:54:25"
echo "Killed: 8, Lived: 2, Not covered: 3"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 80.00%"
echo "Mutator coverage: 100.00%"
EOF
  chmod +x "$path"
}

# run <桩件> <锁文件> [筛选项...]
# 退出码由调用者立刻取走：$? 必须在下一句就读。
#
# LARKAX_DB_DSN 给个占位值：脚本缺它会直接拒绝运行（集成测试会被跳过，
# internal/ledger/store 那一包等于没测）。桩件不看这个值，只要它在。
# 「缺 DSN 要拒绝」本身是第 13 条用例，那一条显式把它清掉。
testdsn="postgres://user:placeholder@localhost:5432/larkax?sslmode=disable"
run() {
  local stub="$1" lock="$2"; shift 2
  LARKAX_DB_DSN="$testdsn" GREMLINS="$stub" LOCK_FILE="$lock" bash "$SCRIPT" "$@" >"$last_out" 2>&1
}

dump() { sed 's/^/    | /' "$last_out" | tail -6; }

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
printf '%s\n' 'error: could not build the test binary' 'make: *** [Makefile:12: test] Error 2' > "$stub_junk"
chmod +x "$stub_junk"

# ── 用例 ──────────────────────────────────────────────────────────────────

# 1. 正常输出：有杀有活、没超时。这是「跑成了」的样子。
run "$stub_ok" "$tmp/lock1" internal/money
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "正常输出 —— 应通过，实际退出码 $rc"; dump; fi
grep -q '全部跑成' "$last_out" || bad "正常输出 —— 通过时也要说清「全部跑成」"

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

# 9. 两个实例同时跑：第二个必须拒绝执行。
#    并行会让超时判据失真（两个实例各自固定 workers=1，机器上照样是 2 个 go test），
#    同一份代码会给出两个不同的分数——所以这里宁可拒绝，也不一起跑。
stub_par="$tmp/gremlins-par"
make_gremlins_slow "$stub_par" 2
lock_par="$tmp/lock-par"
out_par1="$tmp/par1.txt"
out_par2="$tmp/par2.txt"
(
  LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_par" LOCK_FILE="$lock_par" bash "$SCRIPT" internal/money >"$out_par1" 2>&1
) &
bg=$!
# 等锁真的被持有（文件出现不等于锁已拿到），再发第二个实例
waited=0
while [ "$waited" -lt 60 ]; do
  if command -v flock >/dev/null 2>&1; then
    flock -n "$lock_par" true 2>/dev/null || break   # 拿不到 = 已经有人持着
  else
    [ -e "$lock_par.d" ] && break
  fi
  sleep 0.1
  waited=$((waited + 1))
done
sleep 0.2
LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_par" LOCK_FILE="$lock_par" bash "$SCRIPT" internal/money >"$out_par2" 2>&1
rc2=$?
wait "$bg"
rc1=$?
if [ "$rc2" -eq 3 ]; then ok; else bad "第二个实例 —— 应拒绝执行（退出码 3），实际 $rc2"; sed 's/^/    | /' "$out_par2" | tail -6; fi
grep -q '另一个实例正在跑' "$out_par2" || bad "第二个实例 —— 应说明是「另一个实例正在跑」"
grep -q '失真' "$out_par2" || bad "第二个实例 —— 应说明并行会让超时判据失真"
# 先发的那个照常跑完：锁只挡并发的第二个，不影响正常的那个
if [ "$rc1" -eq 0 ]; then ok; else bad "第一个实例 —— 应正常跑完，实际 $rc1"; sed 's/^/    | /' "$out_par1" | tail -6; fi

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

# 12. 缺 LARKAX_DB_DSN：必须拒绝运行，而不是跑出一批不可比的数字。
#     internal/ledger/store 的集成测试会整包被 t.Skip，而它给别的包贡献的
#     那部分覆盖也跟着没了——同一个包的可测变异体会少掉四成。
env -u LARKAX_DB_DSN GREMLINS="$stub_ok" LOCK_FILE="$tmp/lock12" \
  bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "缺 DSN —— 应拒绝运行，实际退出码 $rc"; dump; fi
grep -q '没有 LARKAX_DB_DSN' "$last_out" || bad "缺 DSN —— 应报出是这个原因"
grep -q 't.Skip' "$last_out" || bad "缺 DSN —— 应说明 store 会被整包跳过"

# 13. **不许**用 --coverpkg 把覆盖范围收窄。
#     这是走过一遍的错路：收窄确实让数字稳定（不再随 DSN 变），但代价是把
#     「被别的包测试覆盖到」的那部分丢掉——internal/ledger 的可测变异体从
#     143 个掉到 101 个，而表面上只是数字变小了一点，没有任何提示。
#     真正的变量是 DSN，那就固定 DSN（第 12 条），不要砍范围。
#     桩件把收到的参数打出来，这里断言 --coverpkg 不在其中。
stub_args="$tmp/gremlins-args"
cat > "$stub_args" <<'EOF'
#!/usr/bin/env bash
# 桩件：把收到的参数记下来，供自测断言
printf '%s\n' "$*" > "$ARGS_OUT"
echo "Killed: 5, Lived: 2, Not covered: 1"
echo "Timed out: 0, Not viable: 0, Skipped: 0"
echo "Test efficacy: 71.43%"
EOF
chmod +x "$stub_args"
ARGS_OUT="$tmp/args.txt" LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_args" \
  LOCK_FILE="$tmp/lock13" bash "$SCRIPT" internal/money >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "记录参数 —— 应通过，实际退出码 $rc"; dump; fi
if grep -q -- '--coverpkg' "$tmp/args.txt" 2>/dev/null; then
  bad "覆盖范围被收窄了 —— 收窄会丢掉别的包贡献的覆盖（收到：$(cat "$tmp/args.txt" 2>/dev/null)）"
else
  ok
fi

# 14. 退化路径（没有 flock）崩溃后留下的锁：报错要说清怎么清。
#     这里直接验证提示文本本身——它由 check-mutation.sh 按锁目录里的 PID
#     判断持有者是否还在，PID 不存在就是残留锁。
#     没有 flock 的环境才走 mkdir 路径；有 flock 时锁由内核释放，不会有残锁，
#     所以这一步只在缺 flock 时执行。
if ! command -v flock >/dev/null 2>&1; then
  lock_stale="$tmp/lock-stale"
  mkdir -p "$lock_stale.d"
  printf '%s\n' 4000001 > "$lock_stale.d/pid"   # 这个 PID 不会存在
  LARKAX_DB_DSN="$testdsn" GREMLINS="$stub_ok" LOCK_FILE="$lock_stale" bash "$SCRIPT" internal/money >"$last_out" 2>&1
  rc=$?
  if [ "$rc" -eq 3 ]; then ok; else bad "残留锁 —— 应拒绝执行（退出码 3），实际 $rc"; dump; fi
  grep -q 'rm -rf ' "$last_out" || bad "残留锁 —— 提示里应给出清理命令"
else
  echo "（跳过残留锁一条：本机有 flock，锁由内核释放，不存在残留锁）"
fi

# 15. 存活清单要打出来，并且**带包名与位置**——只打计数的话，
#     AGENTS.md 那句「对存活的变异逐个判断」没有可判断的对象。
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

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
