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
#   1  没跑成：gremlins 缺失、版本不符、读不出变异器集合，或者有包没跑成
#      （超时过多、一个变异都没跑到、认不出输出）
#   3  拿不到锁：另一个实例正在跑（或锁文件不可用），本次未执行
# 1 与 3 分开是因为处置不同：前者要去看代码或环境，后者等一会儿重跑就行。
#
# **它不判「efficacy 够不够」。** 存活变异要逐条人看——等价变异与
# 不可达分支都该放过，而脚本分不出来。这里只保证跑出来的数字可信。
#
# 环境变量：GREMLINS（可执行文件，默认 gremlins）、COEF、WORKERS、
# LOCK_FILE（锁文件路径，默认 <仓库根>/tmp/check-mutation.lock）、
# GREMLINS_VERSION（自测注入用，见下；CI 与本地都不该设）。
#
# # 变异器集合与工具版本：每次运行都要说清「这把尺子是什么」
#
# gremlins 默认只开一部分变异器。实测 v0.6.0 的默认集合是 ARITHMETIC_BASE、
# CONDITIONALS_BOUNDARY、CONDITIONALS_NEGATION、INCREMENT_DECREMENT、
# INVERT_NEGATIVES；而 INVERT_LOGICAL（`&&` ↔ `||`）**默认关闭**——于是
# `a || b` 这样的判据一个变异体都不生成，efficacy 再高也证明不了它被钉住。
# 本仓库里就有这样的判据（internal/route 的 worthNextCandidate 是 `a || b`），
# 所以这里显式打开它（见 EXTRA_MUTATOR_FLAGS）。
#
# **变异体数就是 efficacy 的分母，而分母由变异器集合决定。** 换一个 gremlins
# 版本 → 换一批变异器 → 同一个包的 efficacy 从 84% 变成 91%，而代码一行没动。
# 而「分母变了」在输出里与「代码改好了」长得一模一样：两行都干净、都像结论，
# 没有一个字说这两次不是同一把尺子。（AGENTS.md 那张表的数字尤其如此——它旁边
# 写着的条件里就有「版本」与「启用的变异器集合」这两项，靠人记得填。）
#
# 所以这里做两件事。
#
# ## 一、钉住版本并校验
#
# 探测方式与 scripts/check-deadcode.sh 一致：读二进制里嵌的模块信息，
# **不用工具自报的版本号**。实测：
#
#   $ gremlins --version    → gremlins version dev linux/amd64
#   $ go version -m <bin>   → mod github.com/go-gremlins/gremlins v0.6.0 h1:…
#
# 自报的那串是 dev：版本号由 release 构建注入，`go install` 装出来的没有。
# 所以「装的是哪一版」只能从模块信息里读。读不出时报错退出，不猜——猜出来的
# 版本比没有版本更坏，它会让下面那一行看起来像个结论。
#
# 边界：`go version -m` 读不了非 go 构建的二进制（自测那种桩件就是），
# GOPATH 模式构建的也可能没有模块信息。那时一律按「读不出」处理。
#
# ## 二、每次运行打一行「这把尺子是什么」
#
# 版本 + 变异器集合 + 已显式打开的开关 + workers + 系数。这样**粘进 AGENTS.md
# 那张表的数字自带条件**，不依赖人记得在旁边补一句。
#
# 集合是**探测来的，不是写死一份常量**：写死的那份会在升版本时与工具脱钩，
# 而脱钩之后打出来的那一行仍然长得像结论——那正是这一行要防的事
#（LESSONS 第 2、17 条：门禁坏掉时的输出与「干净」一样）。读不出集合时同样
# 报错退出，理由与版本读不出时一样。
#
# 边界：探测靠的是 `unleash --help` 的排版。排版整个变了（`enable "X" mutants`
# 一行都认不出）会被「读不出」挡住；但如果只是**部分**行丢了 `(default true)`，
# 打出来的集合就会少记一个默认变异器而不报错。自测的夹具用的是真 gremlins 的
# `unleash --help` 原文，就是为了让这条探测有据可依，而不是按想象造一份。
#
# **版本常量与安装方式必须一致**：安装提示里的 `@版本` 由 PINNED_GREMLINS_VERSION
# 拼出来，不写第二遍。注意本仓**没有** CI job 装 gremlins——变异测试只在发布时
# 本地跑（见 AGENTS.md），CI 跑的是本脚本的自测（用桩件、不调真 gremlins）。
# 将来若加了这样的 job，它的 `go install` 版本必须与这里一致，
# 与 deadcode 那条同理（见 .github/workflows/gates.yml 里装 deadcode 的那一步）。
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

# ── 工具、版本、变异器集合（「这把尺子是什么」）────────────────────────────
#
# 「没装」「版本不符」「读不出变异器集合」都必须是响的：静默通过在这里
# 等于没有这道门（LESSONS 第 16 条）。

# 钉住的 gremlins 版本。换它 = 换变异器集合 = 换 efficacy 的分母，
# 所以 AGENTS.md 那张表的数字必须连同这一版一起读（理由见文件开头）。
# **安装方式要与它一致**：下面所有提示文案都由它拼出来，不写第二遍。
PINNED_GREMLINS_VERSION="v0.6.0"

COEF="${COEF:-10}"
WORKERS="${WORKERS:-1}"

GREMLINS="${GREMLINS:-gremlins}"
bin=$(command -v "$GREMLINS" 2>/dev/null) || {
  echo "找不到 gremlins。安装：go install github.com/go-gremlins/gremlins/cmd/gremlins@${PINNED_GREMLINS_VERSION}"
  echo "或用 GREMLINS=/path/to/gremlins 指定。"
  echo "要装钉住的那一版：版本不同变异器集合就不同，变异体数（efficacy 的分母）"
  echo "跟着变——同一份代码会跑出两个分数，而代码一行没动。"
  exit 1
}

if [ -n "${GREMLINS_VERSION:-}" ]; then
  # 自测注入点：桩件是 bash 脚本，go version -m 探不出来。
  got_version="$GREMLINS_VERSION"
else
  got_version=$(go version -m "$bin" 2>/dev/null |
    awk '$1 == "mod" && $2 == "github.com/go-gremlins/gremlins" { print $3; exit }')
fi
if [ -z "$got_version" ]; then
  echo "读不出 $bin 的版本（go 不在 PATH 里，或者它不是 go install 装出来的？）。"
  echo "版本不同变异器集合就不同，这一轮的分数不可比，所以这里不猜。装钉住的那一份："
  echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@${PINNED_GREMLINS_VERSION}"
  exit 1
fi
if [ "$got_version" != "$PINNED_GREMLINS_VERSION" ]; then
  echo "gremlins 的版本与钉住的不一致，这一轮的结果不可比。"
  echo "  钉住：${PINNED_GREMLINS_VERSION}"
  echo "  实际：${got_version}"
  echo "版本不同 → 变异器集合不同 → 变异体数（efficacy 的分母）不同。"
  echo "代码一行没动，分数照样会变，而两个分数看起来都是结论。装钉住的那一份："
  echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@${PINNED_GREMLINS_VERSION}"
  exit 1
fi

# 变异器集合从 `unleash --help` 里读出来。相关的那几行形如：
#
#   --arithmetic-base             enable "ARITHMETIC_BASE" mutants (default true)
#   --invert-logical              enable "INVERT_LOGICAL" mutants
#
# 认的是 `enable "X" mutants` 这一段，所以非变异器的开关（--workers、
# --coverpkg 之类）会被滤掉。一行都认不出时下面的判据报错退出，不静默当成
# 「没有变异器」——那正是「读不出」与「干净」长得一样的那一类。
unleash_help=$("$bin" unleash --help 2>&1)
mutator_table=$(printf '%s\n' "$unleash_help" | awk '
  {
    if (!match($0, /--[a-z][a-z-]*/)) next
    flag = substr($0, RSTART, RLENGTH)
    if (!match($0, /enable "[A-Z_]+" mutants/)) next
    name = substr($0, RSTART + 8, RLENGTH - 17)
    def = (index($0, "(default true)") > 0) ? "yes" : "no"
    print name "|" flag "|" def
  }')
if [ -z "$mutator_table" ]; then
  echo "读不出 gremlins 的变异器集合（$GREMLINS unleash --help 里没有 enable \"X\" mutants 那几行）。"
  echo "变异器集合就是 efficacy 的分母的来源，读不出就说不清这一轮用的是哪把尺子——"
  echo "而说不清的分数不该被当成结论。先确认装的是钉住的那一版："
  echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@${PINNED_GREMLINS_VERSION}"
  exit 1
fi

# gremlins 默认关闭、而本仓库需要显式打开的开关（理由见文件开头）。
# 命令行由这个数组拼出来，所以「实际传了哪些开关」与「下面那行说开了哪些」
# 不会各说各话——不需要人记得同步两处。
EXTRA_MUTATOR_FLAGS=(--invert-logical)

enabled_mutators=""
default_mutators=0
while IFS='|' read -r m_name m_flag m_def; do
  [ -n "$m_name" ] || continue
  if [ "$m_def" = "yes" ]; then
    enabled_mutators="$enabled_mutators $m_name"
    default_mutators=$((default_mutators + 1))
  fi
done <<<"$mutator_table"
enabled_mutators="${enabled_mutators# }"

GREMLINS_ARGS=(unleash --timeout-coefficient="$COEF" --workers="$WORKERS")
for f in "${EXTRA_MUTATOR_FLAGS[@]}"; do
  # 开关名 → 变异器名。读不出来说明这一版不认识这个开关：那这一轮到底开了
  # 什么就说不清，而说不清的分数不该被当成结论。
  f_name=$(printf '%s\n' "$mutator_table" | awk -F'|' -v f="$f" '$2 == f { print $1; exit }')
  if [ -z "$f_name" ]; then
    echo "$f 不是这一版 gremlins 认得的变异器开关（unleash --help 里没有它）。"
    echo "说不清这一轮开了什么，就不跑。"
    exit 1
  fi
  GREMLINS_ARGS+=("$f")
  # 默认集合之后跟上显式打开的那些，让「比默认多开了什么」一眼看得出来。
  enabled_mutators="$enabled_mutators $f_name"
done

# 一个默认变异器都没有，几乎不可能是真的——更可能是 `--help` 的排版变了、
# `(default true)` 没被认出来，而那会让下面那行少记一批变异器而不报错。
if [ "$default_mutators" -eq 0 ]; then
  echo "gremlins 的 --help 里没有一行带 (default true) 的变异器开关。"
  echo "更可能是排版变了而不是真的一个都不开，所以这里不猜，直接退出。"
  exit 1
fi

# 「这把尺子是什么」：粘进 AGENTS.md 那张表的数字自带条件，不依赖人记得补。
extra_desc=""
for f in "${EXTRA_MUTATOR_FLAGS[@]}"; do extra_desc="$extra_desc $f 已开"; done
echo "这把尺子：gremlins ${got_version} / 变异器集合 ${enabled_mutators} /${extra_desc} / workers=${WORKERS} / 系数 ${COEF}"

# 集成测试需要它。缺了的话 internal/ledger/store 整包被 t.Skip，
# 那个包根本没测到——而跳过的输出与「跑成了」在屏幕上一样。
if [ -z "${LARKAX_DB_DSN:-}" ]; then
  echo "没有 LARKAX_DB_DSN，本次未执行。"
  echo "internal/ledger/store 的集成测试会整包被 t.Skip，那一包等于没测。"
  echo "本地起库的办法见 AGENTS.md 的「Lint」一节。"
  exit 1
fi

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

# 存活清单。收尾时打出来——AGENTS.md 要求「对存活的变异逐个判断」，
# 而只打一个计数的话那句话没法执行：数得出来，看不了。
SURVIVORS=$(mktemp -t check-mutation-survivors.XXXXXX)
# 这条 trap 会盖掉上面退化锁路径设的那一条，所以两条要一起写在这里。
# 用 flock 时 $LOCK_FILE.d 不存在，`rm -rf` 是空操作。
trap 'rm -f "$SURVIVORS"; rm -rf "$LOCK_FILE.d"' EXIT

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
  # 开关都在 GREMLINS_ARGS 里拼好了（--invert-logical 是显式打开的，默认关，
  # 理由见文件开头：不开的话 `a || b` 这类判据一个变异体都不生成，而「没生成」
  # 与「生成了且被杀死」在分数上分不出来）。这里不再单独写一遍开关，
  # 免得命令行与上面那行「这把尺子」各说各话。
  out=$("$GREMLINS" "${GREMLINS_ARGS[@]}" "./$pkg/" 2>&1)
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

  # gremlins 每测一个变异就打一行 `LIVED <类型> at <文件>:<行>:<列>`
  # （前面可能带缩进），把它抄下来。**不解析 JSON**：那要再引一个解析器，
  # 而这里要的只是「哪几个活了」，gremlins 自己已经打出来了。
  printf '%s\n' "$out" | awk -v pkg="$pkg" '
    /^[[:space:]]*LIVED[[:space:]]/ {
      sub(/^[[:space:]]*LIVED[[:space:]]+/, "")
      print pkg "  " $0
    }' >>"$SURVIVORS"

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
echo "全部跑成。"

# 存活清单打出来。AGENTS.md 写着「对存活的变异逐个判断，只有等价变异与
# 不可达分支可以放过」——**只打计数时那句话没法执行**：看不了就判不了，
# 于是「逐个判断」实际退化成「看一个百分比」。清单本身是判据的一部分。
#
# 这一节不进退出码：存活不是失败（判过之后可以放过），失败的是「没跑成」。
if [ -s "$SURVIVORS" ]; then
  echo
  echo "── 存活的变异（逐条判断：等价变异 / 不可达分支才可放过，放过时在测试里写一行说明）──"
  cat "$SURVIVORS"
fi
