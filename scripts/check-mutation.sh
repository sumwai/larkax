#!/usr/bin/env bash
# 对核心计费与账本包跑变异测试。
#
# # 为什么固定 workers=1
#
# gremlins 给每个变异的超时预算 = 「跑一次测试集的时间 × 系数」，而那个
# 基准是**串行**测出来的。并发跑变异时机器上同时有 N 个 go test 在编译与
# 执行，超时按墙钟算，于是并发越高、每个变异实际能用的 CPU 越少，
# 越多变异被判超时。
#
# 实测（internal/money，系数固定 10）：
#
#   workers=1  → Killed 80, Timed out 0
#   workers=2  → Killed 80, Timed out 0
#   workers=4  → Killed  3, Timed out 92
#
# 所以**并发与系数是同一个旋钮**：调大系数是在补并发造成的短缺，
# 而那会让「这个包需要多大系数」变成一个随机器负载变化的假属性。
# 固定串行之后，系数才回到它的本意——容纳「变异体比原代码慢」的那种包。
#
# 代价是慢。但变异测试不是每次提交都跑的东西（见 AGENTS.md），
# 慢而可信比快而误导好。
#
# # 为什么同一时刻只允许一个实例
#
# 上面那件事还有别的来源：**机器上任何并发的 CPU 密集工作**。两个实例各自
# 都固定 workers=1，但机器上同时有 2N 个 go test 在编译与执行，超时判据一样
# 会失真。
#
# 实测（同一份代码，workers=1、系数 10）：
#
#   机器空闲：internal/tokens → Killed 100, Timed out 0
#   有别的负载：internal/tokens → Killed  95, Timed out 5
#   机器空闲：internal/billing → Killed 145, Timed out 0
#   有别的负载：internal/billing → Killed 136, Timed out 10
#
# 所以脚本运行期间持一把排他锁。**它挡不住别的负载**（别人在跑 go test 一样
# 会影响），但「两个人/两个 agent 同时跑这个脚本」是一类可以避免的来源，
# 而且代价只有一行 flock。
#
# 跑出分数之后先看一眼超时条数：不为 0 就说明这一轮受了干扰，
# 别把那几个数当成包的属性。
#
# # 什么算「没跑成」
#
# gremlins 跑完但一个变异都没测到时，它的输出是
#
#   Killed: 0, Lived: 0, Not covered: 44
#   Timed out: 0, Not viable: 0, Skipped: 0
#   Test efficacy: 0.00%
#
# 而 0.00% efficacy 与「这个包质量不行」在屏幕上长得一模一样，退出码也都是 0。
# 集成测试要 LARKAX_DB_DSN，没设时整包被 t.Skip——这是真实会发生的。
# 所以 Killed 与 Lived 同时为 0 判为「没跑成」，单独报出来并计入失败；
# 同理，认不出输出格式（没有那几行）也往失败一侧倒，不静默当成通过。
#（LESSONS 第 16 条：检查工具「没跑成」时看起来和「跑成了」一样。）
#
# 用法：
#   scripts/check-mutation.sh              # 跑全部包
#   scripts/check-mutation.sh money route  # 只跑指定的（按包名后缀匹配）
#
# 退出码：
#   0  全部跑成
#   1  有包没跑成（超时过多、一个变异都没跑到、认不出输出、或工具缺失）
#   3  拿不到锁：另一个实例正在跑（或锁文件不可用），本次未执行
# 1 与 3 分开是因为处置不同：前者要去看代码或环境，后者等一会儿重跑就行。
#
# **它不判「efficacy 够不够」。** 存活变异要逐条人看——等价变异与
# 不可达分支都该放过，而脚本分不出来。这里只保证跑出来的数字可信。
#
# 环境变量：GREMLINS（可执行文件，默认 gremlins）、COEF、WORKERS、
# LOCK_FILE（锁文件路径，默认 <仓库根>/tmp/check-mutation.lock）。
#
# # 变异器集合与工具版本
#
# gremlins 默认只开一部分变异器。v0.6.0 的默认集合是 ARITHMETIC_BASE、
# CONDITIONALS_BOUNDARY、CONDITIONALS_NEGATION、INCREMENT_DECREMENT、
# INVERT_NEGATIVES；而 INVERT_LOGICAL（`&&` ↔ `||`）**默认关闭**——于是
# `a || b` 这样的判据一个变异体都不生成，efficacy 再高也证明不了它被钉住。
# 本仓库里就有这样的判据（internal/route 的 worthNextCandidate 是 `a || b`），
# 所以这里显式打开它。
#
# **打开它意味着历史数字与现在的口径不可比。** efficacy 的分母是变异体数，
# 分母变了分数就会变。AGENTS.md 那张表因此必须按这一套口径整表重跑一次；
# 在那之前表里的数字只能当历史值看，不要拿来与新一轮比较。
#
# 版本同样要钉住：`@latest` 不是版本，装出来的二进制会随上游发布而变。
# 而且它自报的版本是 dev（版本号由 release 构建注入，`go install` 装的没有），
# 所以脚本**没法在运行时核对版本**——只能把版本写死在安装方式里，
# 并在 AGENTS.md 的表旁记下它出自哪个版本。
#
# 本脚本有一份自测：scripts/check-mutation.test.sh。改动后必须跑它。

set -uo pipefail
cd "$(dirname "$0")/.."

# ── 排他锁 ────────────────────────────────────────────────────────────────
#
# 锁文件放在仓库根的 tmp/ 下：.gitignore 的 /tmp/ 规则覆盖它，所以它既不进
# 版本库，也不会被红线自查的 --all 扫到（那里用了 --exclude-standard）。
LOCK_FILE="${LOCK_FILE:-$PWD/tmp/check-mutation.lock}"
mkdir -p "$(dirname "$LOCK_FILE")"

# 已经有实例在跑时的统一出口。
lock_busy() {
  local holder=""
  holder=$(cat "$LOCK_FILE.d/pid" 2>/dev/null || true)
  echo "另一个实例正在跑（锁：$LOCK_FILE），本次未执行。"
  echo "并行会让超时判据失真：gremlins 的超时按墙钟算，而基准是串行测出来的，"
  echo "两个实例一起跑时被判超时的变异会凭空变多（见本文件开头与 LESSONS 第 15 条）。"
  # mkdir 退化路径下崩溃（kill -9、断电）会留下目录，这时要说清怎么清。
  if [ -n "$holder" ] && ! kill -0 "$holder" 2>/dev/null; then
    echo "这把锁是残留的：持有者 PID $holder 已经不在了。清掉再跑：rm -rf $LOCK_FILE.d"
  fi
  exit 3
}

if command -v flock >/dev/null 2>&1; then
  # flock 的锁由内核在进程退出时释放，kill -9 也不例外——所以不会留下
  # 需要人工清理的残锁。这是优先用它的理由，也够用。
  exec 9>"$LOCK_FILE" || { echo "打不开锁文件 $LOCK_FILE，本次未执行。"; exit 3; }
  flock -n 9 || lock_busy
else
  # 退化路径：mkdir 是原子的，能当锁用，代价是崩溃会留下目录（见上面的提示）。
  if mkdir "$LOCK_FILE.d" 2>/dev/null; then
    printf '%s\n' "$$" > "$LOCK_FILE.d/pid"
    trap 'rm -rf "$LOCK_FILE.d"' EXIT
  else
    lock_busy
  fi
fi

GREMLINS="${GREMLINS:-gremlins}"
if ! command -v "$GREMLINS" >/dev/null 2>&1; then
  echo "找不到 gremlins。安装：go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0"
  echo "或用 GREMLINS=/path/to/gremlins 指定。"
  exit 1
fi

# 集成测试需要它。缺了的话 internal/ledger/store 整包被 t.Skip，
# 那个包根本没测到——而跳过的输出与「跑成了」在屏幕上一样。
if [ -z "${LARKAX_DB_DSN:-}" ]; then
  echo "没有 LARKAX_DB_DSN，本次未执行。"
  echo "internal/ledger/store 的集成测试会整包被 t.Skip，那一包等于没测。"
  echo "本地起库的办法见 AGENTS.md 的「Lint」一节。"
  exit 1
fi

COEF="${COEF:-10}"
WORKERS="${WORKERS:-1}"

PKGS=(
  internal/money
  internal/billing
  internal/ledger
  internal/ledger/store
  internal/pipeline
  internal/calendar
  internal/quota
  internal/tokens
  internal/route
  internal/snapshot
  internal/snapshotwire
  internal/gateway
)

# 一个包里超时的变异超过这个数，就认为系数不够、结果不可信。
MAX_TIMED_OUT=10

filters=("$@")
selected() {
  [ ${#filters[@]} -eq 0 ] && return 0
  local pkg="$1"
  for f in "${filters[@]}"; do
    case "$pkg" in *"$f") return 0 ;; esac
  done
  return 1
}

failed=0
checked=0
printf '%-20s %8s %7s %8s  %s\n' "包" "killed" "存活" "超时" "efficacy"
printf '%s\n' "──────────────────────────────────────────────────────────────"

for pkg in "${PKGS[@]}"; do
  selected "$pkg" || continue
  checked=$((checked + 1))

  # 这里**不传** --coverpkg：让它用默认范围（整个模块的测试集）。
  #
  # 曾经想过收紧成 --coverpkg=./<pkg>/ 来「固定范围」，那是错的方向。
  # 默认范围下 internal/ledger 有 143 个变异体可被测到，收窄后只剩 101 个——
  # 收窄是把「被别的包测试覆盖到」的那部分白白丢掉，数字看着稳了，信息量少了。
  #
  # 真正让范围漂移的是 LARKAX_DB_DSN：它不在时 internal/ledger/store 整包
  # t.Skip，于是 ledger 的可测变异体从 143 掉到 101。所以上面把 DSN 变成了
  # 硬前提，而不是在这里收窄范围。
  #
  # 实测（ledger）：有 DSN 时 132/11/27（可测 143），无 DSN 时 94/7/69（可测 101）。
  # 注意两个百分比别混：Test efficacy 是 92.31% 与 93.07%（几乎不变），
  # 差得多的是 Mutator coverage，84.12% 与 59.41%。
  # --invert-logical 是显式打开的（默认关，理由见文件开头）：不开的话
  # `a || b` 这类判据一个变异体都不生成，而「没生成」与「生成了且被杀死」
  # 在分数上分不出来。
  out=$("$GREMLINS" unleash --invert-logical --timeout-coefficient="$COEF" --workers="$WORKERS" "./$pkg/" 2>&1)
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "⚠ $pkg: gremlins 执行失败（退出码 $rc）"
    printf '%s\n' "$out" | tail -5 | sed 's/^/   /'
    failed=1
    continue
  fi

  killed=$(printf '%s\n' "$out" | sed -n 's/^Killed: \([0-9]*\).*/\1/p')
  lived=$(printf '%s\n' "$out" | sed -n 's/^.*Lived: \([0-9]*\).*/\1/p')
  timed=$(printf '%s\n' "$out" | sed -n 's/^.*Timed out: \([0-9]*\).*/\1/p')
  eff=$(printf '%s\n' "$out" | sed -n 's/^Test efficacy: \(.*\)$/\1/p')

  # 一行都认不出来，说明 gremlins 的输出格式变了（升级、换了实现）。
  # 这时脚本什么都没在判断，而「什么都没判断」绝不能被当成「都没问题」。
  if [ -z "$killed" ] || [ -z "$lived" ] || [ -z "$timed" ] || [ -z "$eff" ]; then
    echo "⚠ $pkg: 认不出 gremlins 的输出——没有 Killed / Lived / Timed out / Test efficacy 这几行。"
    echo "   格式变了，脚本没在判断任何东西，这不是「通过」。下面是它实际打出来的："
    printf '%s\n' "$out" | tail -5 | sed 's/^/   /'
    failed=1
    continue
  fi

  printf '%-20s %8s %7s %8s  %s\n' "$pkg" "$killed" "$lived" "$timed" "$eff"

  # 一个变异都没测到：这不是「这个包质量不行」，是「这一次没有结果」。
  # 最省事的判据是 Killed 与 Lived 同时为 0——Not covered 有多少个都不算数。
  if [ "$killed" -eq 0 ] && [ "$lived" -eq 0 ]; then
    echo "⚠ $pkg: 这次一个变异都没跑到（Killed 0、Lived 0）。"
    echo "   常见原因：集成测试要 LARKAX_DB_DSN，没设时整包被 t.Skip；包编译不过；"
    echo "   或测试全部被跳过。先去把这些排掉，再谈这一包的分数。"
    printf '%s\n' "$out" | tail -5 | sed 's/^/   /'
    failed=1
  fi

  if [ "$timed" -gt "$MAX_TIMED_OUT" ]; then
    echo "⚠ 超时 $timed 个（上限 $MAX_TIMED_OUT）——这个分数不可信，先查是不是有别的实例或别的负载在抢 CPU"
    failed=1
  fi
done

echo
if [ "$checked" -eq 0 ]; then
  echo "没有选中任何包（筛选条件：${filters[*]:-无}）——这次什么都没检查，不算通过。"
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "有包的结果不可信或没跑成，见上面的 ⚠。"
  exit 1
fi
echo "全部跑成。存活清单要逐条人看——等价变异与不可达分支都该放过。"
