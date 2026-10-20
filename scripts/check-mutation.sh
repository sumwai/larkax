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
# 代价是慢。但变异测试不是每次提交都跑的东西（见 docs/development.md），
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
# # 为什么每跑一个包之前都要清 go 的测试结果缓存（#773）
#
# gremlins 给每个变异的超时预算 = **它那一次 gathering 的耗时 × 系数**。
# 它内部拼的 gathering 命令是 `go test -cover -coverprofile …`——**没有 `-count=1`**，
# 于是会命中 go 的**测试结果缓存**，预算就按从缓存取回的那点耗时算：
#
#   缓存热          gremlins 自报 done in 134.469213ms   → 预算 ≈ 1.34s
#   go clean 之后   gremlins 自报 done in 5.734197153s   → 预算 ≈ 57s
#
# 而 internal/ledger/store 的测试真跑一遍是 5.1s，预算 1.34s 时几乎每个变异体都会超时。
# 同一批变异体，命中缓存那一轮是 Killed 44 / 存活 0 / 超时 303，清掉缓存重跑就正常 KILLED。
#
# 缓存是被**同一份清单里排在前面的包**预热出来的：父包跑完，子包那条命令就成了
# `(cached) 0.218s`。所以清理要在**每次调用 gremlins 之前**，一整轮只清一次挡不住。
#
# 它比上面的 workers 与系数更隐蔽：那两个改的是「每个变异体有多少 CPU」，
# 这个改的是**预算本身**。预算只是略微偏小时，只有个别变异体被算成超时，
# 条数低于 MAX_TIMED_OUT、表照常输出——而超时那一格既不进 efficacy 也不进
# Mutator coverage 的分母，于是这一包的分数会整体偏移，而在表上它与
# 「这一轮真受了别的负载」长得一模一样：超时列本来就是给负载用的，
# 同一个现象很容易被归给负载。
#
# 改不了那条命令本身：它由 gremlins 内部拼出来，脚本能给的只有它自己命令行上的输入
# （--coverpkg 之类），GOFLAGS 也不行——`-count` 对 `go build` 是未知标志。
# 变异体自己那次测试不受影响：变异体改了代码，缓存键不同，本来就命不中。
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
# # 包参数没匹配到包时也要说出来
#
# 包参数按后缀匹配 `PKGS`，而 `PKGS` 是手工维护的一份清单、不等于目录树——
# 照目录树写出来的名字（例如 internal/calendar/store，父包 internal/calendar
# 在清单里、它自己不在）一条都匹配不上。**一条都没选中时**脚本已经会报
#「没有选中任何包」；会漏的是**只选中一部分**：匹配到的那几包照常跑、汇总表照出，
# 少的只是一行，收尾仍打「全部跑成。」。写参数的人以为覆盖了三个包，实际只有两个，
# 而少验的那一包不会被别的机制补上——CI 里没有 gremlins。
#
# 所以选包之后核一遍：**每个包参数至少要匹配到 `PKGS` 里的一条**，没匹配到的
# 当场列出来，并让整轮判为失败。**匹配规则与 `PKGS` 清单都不动**：把子包自动
# 归到父包会把「名字写错」变成「悄悄跑了另一个包」，那是另一种看不见。
#
# 用法：
#   scripts/check-mutation.sh              # 跑全部包
#   scripts/check-mutation.sh money route  # 只跑指定的（按包名后缀匹配）
#
# 退出码：
#   0  全部跑成
#   1  没跑成：gremlins 缺失、版本不符、读不出变异器集合、有包参数没匹配到任何包、
#      前置环境变量缺失（LARKAX_DB_DSN，或选中包的测试要的 LARKAX_REDIS_DSN），
#      或者有包没跑成（超时过多、一个变异都没跑到、认不出输出、测试缓存清不掉）
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
# 没有一个字说这两次不是同一把尺子。（docs/development.md「各包的现状」那张表
# 的数字尤其如此——它旁边
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
# 版本 + 变异器集合 + 已显式打开的开关 + workers + 系数。这样**粘进
# docs/development.md「各包的现状」那张表的数字自带条件**，不依赖人记得在旁边补一句。
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
# 本地跑（见 docs/development.md），CI 跑的是本脚本的自测（用桩件、不调真 gremlins）。
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

# 自测要观察「锁真的被拿到了」。**探锁不能去抢锁**：从外面用 `flock -n`
# 试一下会与被测实例抢同一把锁——负载高时探测方反而先拿到，被测实例自己
# 被判「另一个实例正在跑」（退出码 3），于是自测把「没测成」当成「锁坏了」。
# 所以由持锁方自己报一声，自测等这一行。
echo "已拿到锁：$LOCK_FILE"

# ── 工具、版本、变异器集合（「这把尺子是什么」）────────────────────────────
#
# 「没装」「版本不符」「读不出变异器集合」都必须是响的：静默通过在这里
# 等于没有这道门（LESSONS 第 16 条）。

# 钉住的 gremlins 版本。换它 = 换变异器集合 = 换 efficacy 的分母，
# 所以 docs/development.md「各包的现状」那张表的数字必须连同这一版一起读（理由见文件开头）。
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

# 「这把尺子是什么」：粘进 docs/development.md「各包的现状」那张表的数字自带条件，不依赖人记得补。
extra_desc=""
for f in "${EXTRA_MUTATOR_FLAGS[@]}"; do extra_desc="$extra_desc $f 已开"; done
echo "这把尺子：gremlins ${got_version} / 变异器集合 ${enabled_mutators} /${extra_desc} / workers=${WORKERS} / 系数 ${COEF}"

# 集成测试需要它。缺了的话 internal/ledger/store 整包被 t.Skip，
# 那个包根本没测到——而跳过的输出与「跑成了」在屏幕上一样。
if [ -z "${LARKAX_DB_DSN:-}" ]; then
  echo "没有 LARKAX_DB_DSN，本次未执行。"
  echo "internal/ledger/store 的集成测试会整包被 t.Skip，那一包等于没测。"
  echo "本地起库的办法见 docs/development.md 的「每个包一个测试库」一节。"
  exit 1
fi

PKGS=(
  internal/money
  internal/billing
  internal/billing/expr
  internal/pricing
  internal/ledger
  internal/ledger/store
  internal/pipeline
  internal/calendar
  internal/quota
  internal/keyquota
  internal/tokens
  internal/route
  internal/channels
  internal/credential
  internal/idempotency
  internal/overrides
  internal/ratelimit
  internal/auth
  internal/catalog
  internal/relay
  internal/plugin/store
  internal/plugin/host
  internal/plugin/sandbox
  internal/api
  internal/adminapi
  internal/assembly
  internal/settings
  internal/channel
  internal/snapshot
  internal/snapshotwire
  internal/gateway
  internal/payment
  internal/upstream
  internal/observe
)

# 一个包里超时的变异超过这个数，就认为系数不够、结果不可信。
MAX_TIMED_OUT=10

filters=("$@")

# 后缀匹配。**规则只写这一处**：选包与下面的核对共用它，免得两处各写一份、
# 日后只改一处就分岔。
matches_filter() {  # matches_filter <包参数> <包>
  case "$2" in *"$1") return 0 ;; esac
  return 1
}

selected() {
  [ ${#filters[@]} -eq 0 ] && return 0
  local pkg="$1"
  for f in "${filters[@]}"; do
    matches_filter "$f" "$pkg" && return 0
  done
  return 1
}

failed=0
checked=0

# ── 核对：每个包参数至少要匹配到 PKGS 里的一条 ─────────────────────────────
#
# 只选中一部分时，不加核对就一个字都不说：匹配到的照常跑、汇总表照出，少的只是
# 一行，收尾照打「全部跑成。」——与「跳过与通过长得一样」同类（LESSONS 第 16 条，
# 理由见文件开头）。
#
# 这里只负责说出来，**不改匹配规则、也不把子包归到父包**：那会把「名字写错」
# 变成「悄悄跑了另一个包」。没匹配到的参数对应的包这一轮没跑，所以整轮判失败。
unmatched_filters=()
for f in "${filters[@]}"; do
  matched=0
  for pkg in "${PKGS[@]}"; do
    if matches_filter "$f" "$pkg"; then matched=1; break; fi
  done
  [ "$matched" -eq 1 ] || unmatched_filters+=("$f")
done
if [ ${#unmatched_filters[@]} -gt 0 ]; then
  echo "⚠ 这些包参数没有匹配到任何包：${unmatched_filters[*]}"
  echo "   包参数按后缀匹配本脚本里的 PKGS 清单，照清单里的名字写，别照目录树猜"
  echo "   （清单是手工维护的，不等于目录树）。没匹配到的参数对应的包"
  echo "   这一轮没跑，所以整轮判为失败。"
fi

# ── 清单里父子两条同时在选中范围内时，要说出来 ─────────────────────────────
#
# 包路径是**递归**的：`./internal/ledger/` 会把子包 internal/ledger/store 一起
# 变异（实测：那一轮的输出里能读到 `store/reconcile.go:行:列` 这样的位置）。
# 而 PKGS 里 internal/ledger 与 internal/ledger/store 两条都在，internal/billing
# 与 internal/billing/expr 也一样——于是子包的变异体在两条命令里各跑一次，
# 汇总表上那两行也不再是互不相干的两个包：左边那一行要读作「含右边」。
#
# 这件事**从 PKGS 算出来，不写死**：写死一份「哪个包含哪个子包」的表，就是
# 这份清单的第二份，两者开始分岔的那天没人知道哪一份算数。只在这一对都选中时
# 才说——只选了其中一个时，没有重叠可言。
overlap_pairs=()
for a in "${PKGS[@]}"; do
  selected "$a" || continue
  for b in "${PKGS[@]}"; do
    [ "$a" = "$b" ] && continue
    selected "$b" || continue
    case "$b" in "$a"/*) overlap_pairs+=("$a $b") ;; esac
  done
done
if [ ${#overlap_pairs[@]} -gt 0 ]; then
  echo "注：下面这些行的范围是重叠的——包路径递归，左边那条命令会把右边那个包的"
  echo "    变异体一起跑，于是它在表上各算一次，左边那一行读作「含右边」："
  for pair in "${overlap_pairs[@]}"; do
    echo "      ${pair%% *} 含 ${pair#* }"
  done
  echo
fi

# ── 前置：被选中的包里，测试要 Redis 的，这一轮得有 LARKAX_REDIS_DSN ─────────
#
# 与上面那条 LARKAX_DB_DSN 同一个形状，只是范围不同：数据库是清单里每个包都要
# 的硬前提，Redis 只有**测试文件里读它**的那几包才要。internal/ratelimit 进
# 清单之后是第一个这样的包。
#
# 缺它时 redis_integration_test.go 那几条整组 `t.Skip`，而只在这些用例里碰得到
# 的变异体会落进 Not covered——不进 efficacy 的分母。于是分数**变好看而不是
# 变差**：Killed 仍然大于 0，下面「Killed 与 Lived 同时为 0」那条判据不触发，
# 表照出、门照过。它与 LESSONS 第 16 条是同一个问题：跳过与通过长得一样。
#
# 判法是「这个包的测试文件里有没有出现那个变量」，与
# .github/actions/check-integration-tests 同一套做法：按名字找引用，不另立一份
# 「哪些包要 Redis」的名单——那份名单会在下一次有人加集成测试时悄悄漏掉一包。
# 判据只看包目录：变量只由**包外**的测试辅助文件引用时，这里看不见。
# 检查放在包选择之后：只跑不依赖 Redis 的包时，这个变量不是前提。
redis_deps=()
for pkg in "${PKGS[@]}"; do
  selected "$pkg" || continue
  if grep -rq 'LARKAX_REDIS_DSN' --include='*_test.go' "$pkg" 2>/dev/null; then
    redis_deps+=("$pkg")
  fi
done
if [ ${#redis_deps[@]} -gt 0 ] && [ -z "${LARKAX_REDIS_DSN:-}" ]; then
  echo "没有 LARKAX_REDIS_DSN，本次未执行。"
  echo "这些包的测试会读它：${redis_deps[*]}"
  echo "缺它时那些集成测试整组被 t.Skip，只有它们碰得到的变异体会落进 Not covered，"
  echo "那几包等于没测——分数只会变好看，所以这里不把这一轮当结果。"
  echo "本地起 Redis 的办法见 docs/development.md 的「本地跑门禁的前提」一节。"
  exit 1
fi

# 表里要带上「有多少变异体根本没被测到」这两栏（#769）。
#
# gremlins 自己打了 `Not covered` 与 `Mutator coverage`，以前只抄了四个数。
# 而只打 efficacy 时，**「大部分变异体没有任何用例碰到」与「真被钉住」在表上
# 长得一模一样**：internal/keyquota 刚进清单那一轮是 Killed 2 / Lived 0 /
# Not covered 6，efficacy 100.00%，Mutator coverage 只有 25.00%——那个 100%
# 说的是「`over()` 的两个变异体被杀掉了」，不是「这一包被钉住了」。
# 它与 LESSONS 第 16 条同类：`Not covered` 与 `Lived` 都不出现在表上，
# 而处置完全相反（前者要补测试，后者要判等价）。
#
# 两栏都照实打、都不进退出码：判据仍然只看存活与超时。
printf '%-20s %7s %6s %7s %12s %17s  %s\n' \
  "包" "killed" "存活" "超时" "Not covered" "Mutator coverage" "efficacy"
printf '%s\n' "─────────────────────────────────────────────────────────────────────────────────"

# 存活清单。docs/development.md 要求「对存活的变异逐个判断」，而只打一个计数时
# 那句话没法执行：数得出来，看不了。所以每个包一跑完就把它的存活行抄下来，
# 收尾统一打印，打印的位置在下面（在成败判定之前，理由写在那里）。
SURVIVORS=$(mktemp -t check-mutation-survivors.XXXXXX)
# 这条 trap 会盖掉上面退化锁路径设的那一条，所以两条要一起写在这里。
# 用 flock 时 $LOCK_FILE.d 不存在，`rm -rf` 是空操作。
trap 'rm -f "$SURVIVORS"; rm -rf "$LOCK_FILE.d"' EXIT

for pkg in "${PKGS[@]}"; do
  selected "$pkg" || continue
  checked=$((checked + 1))

  # 这里**不传** --coverpkg、也不传 --integration：用默认调用。
  #
  # 默认口径是**被测包自己的测试**。实测（internal/keyquota，workers=1、系数 10、
  # --invert-logical，库建好并迁移过、DSN 有效，8 个变异体）：
  #
  #   默认                            gathering   219ms   Killed 2 / Not covered 6   25.00%
  #   --coverpkg=./...                gathering   269ms   Killed 2 / Not covered 6   25.00%
  #   --integration                   gathering 34.16s   Killed 2 / Not covered 6   25.00%
  #   --integration --coverpkg=./...  gathering 37.85s   Killed 8 / Not covered 0  100.00%
  #
  # **两个一起**才把别的包贡献的覆盖算进来（最后一列是 Mutator coverage）。
  # 那 6 条守卫（Spent 与 CheckInTx 里的 `err != nil`）整个测试集**确实执行得到**：
  # `go test -coverpkg=./internal/keyquota/ ./...` 给出 Spent 71.4%、CheckInTx 68.8%，
  # 而这个包自己的测试只有 3.3%——而 `--integration` 单独跑一轮，gathering 从
  # 219ms 涨到 34.16s（范围确实宽了）却一个都没多测到。
  #
  # 那条路仍然走不得：每个变异体跑一遍整个测试集，本仓一套上百个变异体。
  # 这一条要解决的不是「范围窄」，而是**范围窄这件事在表上看不出来**——所以汇总表
  # 连 Not covered 与 Mutator coverage 一起打（见上面表头那段），判据不变。
  #
  # **包路径是递归的**：`./internal/ledger/` 会把子包 internal/ledger/store 一起
  # 变异（实测：那一轮的输出里能读到 `store/reconcile.go:行:列` 这样的位置）。
  # 于是 LARKAX_DB_DSN 这个硬前提仍然成立——它不在时 internal/ledger/store 整包
  # t.Skip，而递归口径下 store 那部分变异体全落进 Not covered，internal/ledger
  # 那一行的数也跟着变（实测：92 → 376，涨的全在 store/ 下）。**那两个包在 PKGS
  # 里各占一行、范围互相重叠**，脚本在这一对都选中时会自己说出来（见上面那段注）。
  # 开关都在 GREMLINS_ARGS 里拼好了（--invert-logical 是显式打开的，默认关，
  # 理由见文件开头：不开的话 `a || b` 这类判据一个变异体都不生成，而「没生成」
  # 与「生成了且被杀死」在分数上分不出来）。这里不再单独写一遍开关，
  # 免得命令行与上面那行「这把尺子」各说各话。
  #
  # 清掉 go 的测试结果缓存，再让 gremlins 去 gathering（#773）。
  #
  # **每个包各清一次**：缓存是被清单里排在前面的包预热出来的，父包跑完，
  # 子包那条命令就成了 `(cached) 0.218s`。所以清理要在这一次调用**之前**，
  # 一整轮只清一次挡不住。
  #
  # 清不掉时必须响：预算错了的话这一包的分数会整体偏移，而超时那一格看起来与
  # 「这一轮真受了别的负载」一模一样——那正是这一处最难被看出来的地方
  #（实测与理由见文件开头那一节）。
  clean_out=$(go clean -testcache 2>&1)
  clean_rc=$?
  if [ "$clean_rc" -ne 0 ]; then
    echo "⚠ $pkg: go clean -testcache 失败（退出码 $clean_rc），这一包没跑。"
    echo "   测试结果缓存没清掉时，gathering 会命中缓存，超时预算被压到缓存耗时，"
    echo "   这一包的变异体会被算成超时——不是它们慢，是预算错了。而那一格与"
    echo "   「真受了别的负载」在表上长得一样，所以这里不把它当结果（#773）。"
    if [ -n "$clean_out" ]; then printf '%s\n' "$clean_out" | tail -3 | sed 's/^/   /'; fi
    failed=1
    continue
  fi

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
  notcov=$(printf '%s\n' "$out" | sed -n 's/^.*Not covered: \([0-9]*\).*/\1/p')
  eff=$(printf '%s\n' "$out" | sed -n 's/^Test efficacy: \(.*\)$/\1/p')
  mutcov=$(printf '%s\n' "$out" | sed -n 's/^Mutator coverage: \(.*\)$/\1/p')

  # 一行都认不出来，说明 gremlins 的输出格式变了（升级、换了实现）。
  # 这时脚本没法判断的东西与表上读不出来的格子，都绝不能被当成「都没问题」。
  #
  # Not covered 与 Mutator coverage 也在这一组里：它们上了表，读不出来时
  # 表上就会留两个空格子，而那看起来像「这两个数不存在」，不像「没读到」。
  if [ -z "$killed" ] || [ -z "$lived" ] || [ -z "$timed" ] || [ -z "$eff" ] ||
    [ -z "$notcov" ] || [ -z "$mutcov" ]; then
    echo "⚠ $pkg: 认不出 gremlins 的输出——没有 Killed / Lived / Timed out / Not covered / Test efficacy / Mutator coverage 这几行。"
    echo "   格式变了，这一轮的数读不全，这不是「通过」。下面是它实际打出来的："
    printf '%s\n' "$out" | tail -5 | sed 's/^/   /'
    failed=1
    continue
  fi

  printf '%-20s %7s %6s %7s %12s %17s  %s\n' \
    "$pkg" "$killed" "$lived" "$timed" "$notcov" "$mutcov" "$eff"

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

# 存活清单打出来。docs/development.md 写着「对存活的变异逐个判断，只有等价变异与
# 不可达分支可以放过」——**只打计数时那句话没法执行**：看不了就判不了，
# 于是「逐个判断」实际退化成「看一个百分比」。清单本身是判据的一部分。
#
# **它排在下面那两个判据之前，这是有意的**（#566）。清单说的是**所有包**，
# 而那两个判据说的都是**某一个包**：「有包没跑成」与「有包的结果不可信」。
# 清单若排在它们之后，一个包的坏结果就会把其余所有包的清单一起吃掉——
# 实测一轮全量：store 那一包整包超时之后，屏幕上只剩一个 ⚠ 和汇总表，
# 而其余 11 个包两百余条存活一条都没打印出来；那一轮独占机器跑了一小时。
#
# 清单与汇总表一样是这一轮的产出，不该依赖别的包的成败。
#
# 这一节不进退出码：存活不是失败（判过之后可以放过），失败的是「没跑成」。
if [ -s "$SURVIVORS" ]; then
  echo "── 存活的变异（逐条判断：等价变异 / 不可达分支才可放过，放过时在测试里写一行说明）──"
  cat "$SURVIVORS"
  echo
fi

if [ "$checked" -eq 0 ]; then
  echo "没有选中任何包（筛选条件：${filters[*]:-无}）——这次什么都没检查，不算通过。"
  exit 1
fi
if [ "$failed" -ne 0 ] || [ ${#unmatched_filters[@]} -gt 0 ]; then
  if [ ${#unmatched_filters[@]} -gt 0 ]; then
    echo "有包参数没匹配到任何包（${unmatched_filters[*]}）——那些包这一轮没跑。"
  fi
  if [ "$failed" -ne 0 ]; then
    echo "有包的结果不可信或没跑成，见上面的 ⚠。"
  fi
  exit 1
fi
echo "全部跑成。"
