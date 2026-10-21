#!/usr/bin/env bash
# check-man-pages.sh 的自测。
#
# 为什么需要它：这道门的成功信号是**沉默**——比对通过就只打一行证据，于是
# 「判据写反了 / 一页都没比 / 生成器根本没跑」与「同步」在屏幕上完全一样，
# 退出码也可能一样（LESSONS.md 第 16 条；第 2 条是它的第一次现身：检查脚本自己
# 报「通过」，实际根本没在检查）。
#
# 所以下面把**四条判据与六个「没跑成」的出口**各钉一条：
#
#   判据 1 的两半：生成器产出的页不在库里、在库里但内容不同；
#   判据 2：库里多出一页生成器不再产出的；
#   退出码 2：生成器非零退出、一页都没写、写出空页、写出没有 .TH 的页、$MAN 不在、
#            参数写错；
#   边界：$MAN 下不属于这一族的 .1 文件既不比对也不报错（但要在证据行里报数）；
#   判据 4：汉字与字母紧贴要红并点名页行、白名单能放行、数字与全角标点相邻不报、
#          白名单的上限 / 格式 / 陈旧条目各有出口。
#
# **本自测跑的是真脚本**（不是副本），生成器用 GO 指的桩替掉：桩只做一件事——
# 把 $tmp/fresh 里的几页拷进 LARKAX_MAN_OUT。比对那一段在真脚本里，桩掉它等于
# 什么都没测。
#
# 改动 check-man-pages.sh 之后跑：
#   bash scripts/check-man-pages.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-man-pages.sh"
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

# ── 生成器的桩 ────────────────────────────────────────────────────────────
#
# 它只认 LARKAX_MAN_OUT：约定的那一半（生成器把页写到哪儿去）留着，被替掉的是
# 「页是怎么来的」。

mkdir -p "$tmp/bin"

cat >"$tmp/bin/go-copy" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
src="${STUB_FRESH:?}"
out="${LARKAX_MAN_OUT:?}"
mkdir -p "$out"
for f in "$src"/*.1; do
  [ -f "$f" ] || continue
  cp "$f" "$out/"
done
EOF

cat >"$tmp/bin/go-fail" <<'EOF'
#!/usr/bin/env bash
echo "stub: 生成器炸了" >&2
exit 1
EOF

cat >"$tmp/bin/go-quiet" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

# 空页：写一个零字节的 .1
cat >"$tmp/bin/go-empty" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
out="${LARKAX_MAN_OUT:?}"
mkdir -p "$out"
: >"$out/larkaxctl.1"
: >"$out/larkaxctl-doctor.1"
EOF

# 没有 .TH 的页
cat >"$tmp/bin/go-notth" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
out="${LARKAX_MAN_OUT:?}"
mkdir -p "$out"
echo "随便一页" >"$out/larkaxctl.1"
echo ".TH LARKAXCTL\\-DOCTOR 1" >"$out/larkaxctl-doctor.1"
EOF

chmod +x "$tmp/bin/go-copy" "$tmp/bin/go-fail" "$tmp/bin/go-quiet" "$tmp/bin/go-empty" "$tmp/bin/go-notth"

# ── 夹具：一套「刚生成出来的」页 ──────────────────────────────────────────
#
# 三页：首页与两条命令。内容不重要，重要的是第一行是 .TH（真脚本判这一条）。

mk_fresh() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/larkaxctl.1" <<'EOF'
.TH LARKAXCTL 1 "" "larkax" "larkaxctl"
.SH NAME
larkaxctl \- 夹具
EOF
  cat >"$dir/larkaxctl-doctor.1" <<'EOF'
.TH LARKAXCTL\-DOCTOR 1 "" "larkax" "larkaxctl-doctor"
.SH NAME
larkaxctl\-doctor \- 体检
EOF
  cat >"$dir/larkaxctl-channel.1" <<'EOF'
.TH LARKAXCTL\-CHANNEL 1 "" "larkax" "larkaxctl-channel"
.SH NAME
larkaxctl\-channel \- 渠道
EOF
}

# 一套与 fresh 逐字节相同的「已提交」的页。
mk_committed() {
  local dir="$1"
  mk_fresh "$dir"
}

# run <已提交的目录> [脚本的参数…]
# 退出码由调用者立刻取走：$? 必须在下一句就读。
run() {
  local man="$1"
  shift
  STUB_FRESH="$fresh" MAN="$man" GO="$tmp/bin/go-copy" \
    bash "$SCRIPT" "$@" >"$last_out" 2>&1
}

fresh="$tmp/fresh"
mk_fresh "$fresh"

# ── 1. 干净夹具：退出码 0，证据行报出页数 ─────────────────────────────────

man1="$tmp/man1"
mk_committed "$man1"
run "$man1"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "干净夹具 —— 应通过（退出码 0），实际 $rc"; fi
if grep -q '生成器产出 3 页' "$last_out"; then ok; else
  bad "干净夹具 —— 证据行里没报出 3 页（生成器与比对各漏了一头就看不出来）"
fi

# ── 2. 判据 1：生成器产出的页，库里那份内容不同 ───────────────────────────

man2="$tmp/man2"
mk_committed "$man2"
printf '.SH 别人改过这一页\n' >>"$man2/larkaxctl-doctor.1"
run "$man2"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "内容不一致 —— 应失败（退出码 1），实际 $rc"; fi
if grep -q 'larkaxctl-doctor.1 与刚生成的不一致' "$last_out"; then ok; else
  bad "内容不一致 —— 没有点名是哪一页"
fi

# ── 3. 判据 1：生成器产出的页，库里根本没有 ───────────────────────────────

man3="$tmp/man3"
mk_committed "$man3"
rm -f "$man3/larkaxctl-channel.1"
run "$man3"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "库里的页缺一份 —— 应失败（退出码 1），实际 $rc"; fi
if grep -q 'larkaxctl-channel.1 不存在' "$last_out"; then ok; else
  bad "库里的页缺一份 —— 没有点名是哪一页"
fi

# ── 4. 判据 2：库里多出一页，生成器不再产出它 ─────────────────────────────
#
# 这一条与第 3 条是一对：只判「生成器产出的都在库里」时，删掉一条命令之后
# 它那一页会永远留在库里（一个只增不减的目录）。

man4="$tmp/man4"
mk_committed "$man4"
cat >"$man4/larkaxctl-ghost.1" <<'EOF'
.TH LARKAXCTL\-GHOST 1 "" "larkax" "larkaxctl-ghost"
EOF
run "$man4"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "库里多出一页 —— 应失败（退出码 1），实际 $rc"; fi
if grep -q 'larkaxctl-ghost.1 生成器不再产出' "$last_out"; then ok; else
  bad "库里多出一页 —— 没有点名是哪一页"
fi

# ── 5. 边界：$MAN 下不属于这一族的 .1 不比对、也不报错 ────────────────────
#
# 以后要是补一页手工写的（比如服务端二进制的 larkaxd.1），它不该让这道门红。

man5="$tmp/man5"
mk_committed "$man5"
printf '.TH LARKAXD 1\n' >"$man5/larkaxd.1"
run "$man5"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "手工补的别族手页 —— 应通过（退出码 0），实际 $rc"; fi
if grep -q '别的 .1 文件 1 个' "$last_out"; then ok; else
  bad "手工补的别族手页 —— 证据行里没报出「别的 .1 文件 1 个」（不说出来的话，边界与没扫到长得一样）"
fi

# ── 6~8. 三个「生成器坏了」的出口：都走退出码 2 ───────────────────────────

man6="$tmp/man6"
mk_committed "$man6"

STUB_FRESH="$fresh" MAN="$man6" GO="$tmp/bin/go-fail" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "生成器非零退出 —— 应「没跑成」（退出码 2），实际 $rc"; fi
if grep -q '生成器没跑成' "$last_out"; then ok; else bad "生成器非零退出 —— 提示里没说是生成器的事"; fi

STUB_FRESH="$fresh" MAN="$man6" GO="$tmp/bin/go-quiet" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "生成器一页都没写 —— 应「没跑成」（退出码 2），实际 $rc"; fi

STUB_FRESH="$fresh" MAN="$man6" GO="$tmp/bin/go-empty" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "空页 —— 应「没跑成」（退出码 2），实际 $rc"; fi
if grep -q '空文件' "$last_out"; then ok; else bad "空页 —— 提示里没说是空文件"; fi

STUB_FRESH="$fresh" MAN="$man6" GO="$tmp/bin/go-notth" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "没有 .TH 的页 —— 应「没跑成」（退出码 2），实际 $rc"; fi

# ── 9. $MAN 不在 ──────────────────────────────────────────────────────────

run "$tmp/根本没有这个目录"
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "手页目录不在 —— 应「没跑成」（退出码 2），实际 $rc"; fi

# ── 10. 参数写错 ──────────────────────────────────────────────────────────

run "$man1" --别的参数
rc=$?
if [ "$rc" -eq 2 ]; then ok; else bad "参数写错 —— 应「没跑成」（退出码 2），实际 $rc"; fi

# ── 11. --update：把生成结果搬进 $MAN，之后比对通过 ───────────────────────
#
# 它同时钉住两件容易漏的事：拿掉 --update 时**不写**库（第 2 条已经钉了不通过，
# 这里钉库没被动过），以及 --update **不删别族的文件**（第 12 条）。

man11="$tmp/man11"
mk_committed "$man11"
printf '.SH 别人改过这一页\n' >>"$man11/larkaxctl-doctor.1"
printf '.TH LARKAXD 1\n' >"$man11/larkaxd.1"

run "$man11"
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "--update 之前的比对 —— 应失败（退出码 1），实际 $rc"; fi
if cmp -s "$man11/larkaxctl-doctor.1" "$fresh/larkaxctl-doctor.1"; then
  bad "--update 之前的比对 —— 不该动库里的文件"
else ok; fi

run "$man11" --update
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "--update —— 应成功（退出码 0），实际 $rc"; fi

run "$man11"
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "--update 之后 —— 应通过（退出码 0），实际 $rc"; fi
if cmp -s "$man11/larkaxctl-doctor.1" "$fresh/larkaxctl-doctor.1"; then ok; else
  bad "--update —— 没有把重新生成的页写进库"
fi
if [ -f "$man11/larkaxd.1" ]; then ok; else
  bad "--update —— 把别族的手页（larkaxd.1）删掉了"
fi

# ── 12. 真仓库：这道门在它自己身上是干净的 ────────────────────────────────
#
# 前十一条用的是桩。这一条跑真的生成器与真的 man/，钉住「落地时确实是对得上的」
# ——顺带证明脚本与生成器之间的约定（LARKAX_MAN_OUT、退出码、页数）真的接通了。

MAN="$PWD/man/man1" GO="go" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "真仓库 —— 应通过（退出码 0），实际 $rc"; fi
repo_pages=$(grep -oE '生成器产出 [0-9]+ 页' "$last_out" | grep -oE '[0-9]+')
if [ "${repo_pages:-0}" -ge 10 ]; then ok; else
  bad "真仓库 —— 只生成出 ${repo_pages:-0} 页，命令注册表或扫描范围像是写错了"
fi

# ── 13. 判据 4：汉字与字母紧贴 → 红，并点名页、行与那两个紧贴的字 ──────────
#
# 夹具照抄缺陷的形态：一行里「是active」直接挨着（源文件按列宽硬换行时，
# 那个空格被换行吃掉了）。

cat >"$tmp/bin/go-tightjoin" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
src="${STUB_FRESH:?}"
out="${LARKAX_MAN_OUT:?}"
mkdir -p "$out"
for f in "$src"/*.1; do
  [ -f "$f" ] || continue
  cp "$f" "$out/"
done
printf '权限是active 的管理员才看得到。\n' >>"$out/larkaxctl.1"
EOF

# 数字与汉字紧贴（「4096个」）与全角标点挨着字母（「（status）」）都不该报：
# 两条是判据 4 有意放过的写法，夹具把「不报」也钉住。
cat >"$tmp/bin/go-legitjoin" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
src="${STUB_FRESH:?}"
out="${LARKAX_MAN_OUT:?}"
mkdir -p "$out"
for f in "$src"/*.1; do
  [ -f "$f" ] || continue
  cp "$f" "$out/"
done
printf '共 4096个 token，取值（status）与 `--role`。\n' >>"$out/larkaxctl.1"
EOF

chmod +x "$tmp/bin/go-tightjoin" "$tmp/bin/go-legitjoin"

# mk_from_stub <目录> <桩>：用桩把页生成到 <目录>，当作「库里已提交的那一份」。
# 判据 1、2 与判据 4 要分开钉：库里的那份必须与刚生成的一致，否则先红的是同步，
# 判据 4 的结果就看不见了。
mk_from_stub() {
  local dir="$1" stub="$2"
  rm -rf "$dir"
  mkdir -p "$dir"
  STUB_FRESH="$fresh" LARKAX_MAN_OUT="$dir" "$stub"
}

man13="$tmp/man13"
mk_from_stub "$man13" "$tmp/bin/go-tightjoin"
man15="$tmp/man15"
mk_from_stub "$man15" "$tmp/bin/go-legitjoin"

# 一份空白名单：判据 4 的命中不被任何条目放行。
: >"$tmp/allow-empty.txt"

STUB_FRESH="$fresh" MAN="$man13" GO="$tmp/bin/go-tightjoin" \
  WHITELIST="$tmp/allow-empty.txt" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "中英紧贴 —— 应失败（退出码 1），实际 $rc"; fi
if grep -q '判据 4 · .*larkaxctl\.1:[0-9]* 里「是a」紧贴' "$last_out"; then ok; else
  bad "中英紧贴 —— 没有点出页、行与那两个紧贴的字"
fi

# ── 14. 判据 4：白名单放行命中的那一处 ────────────────────────────────────

printf 'larkaxctl.1 | 是a | 夹具：故意留一处紧贴，验证白名单出口\n' >"$tmp/allow-one.txt"
STUB_FRESH="$fresh" MAN="$man13" GO="$tmp/bin/go-tightjoin" \
  WHITELIST="$tmp/allow-one.txt" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "白名单放行 —— 应通过（退出码 0），实际 $rc"; fi
if grep -q '白名单放行 1 处' "$last_out"; then ok; else
  bad "白名单放行 —— 证据行里没报出放行了几处（放行与没扫到在输出里长得一样）"
fi

# ── 15. 判据 4：数字紧贴与全角标点相邻都放行 ──────────────────────────────

STUB_FRESH="$fresh" MAN="$man15" GO="$tmp/bin/go-legitjoin" \
  WHITELIST="$tmp/allow-empty.txt" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else
  bad "数字 / 全角标点相邻 —— 应通过（退出码 0），实际 $rc"
fi
if grep -q '中英紧贴扫描 3 页、0 处' "$last_out"; then ok; else
  bad "数字 / 全角标点相邻 —— 证据行里扫描处数不是 0（判据把合法的写法也报了）"
fi

# ── 16. 判据 4 的白名单：上限、格式、陈旧条目 ─────────────────────────────
#
# 「只许减不许增」由脚本里的 MAX_ALLOW_ENTRIES 守，自测把它钉住：往上调必须
# 同时改这里，于是那是一次会被 review 看到的 diff。

declared_allow=$(grep -m1 '^MAX_ALLOW_ENTRIES=' "$SCRIPT" | cut -d= -f2)
if [ "$declared_allow" = "1" ]; then ok; else
  bad "MAX_ALLOW_ENTRIES 被改成了 ${declared_allow:-空}（自测钉住的是 1）"
fi

if [ -n "$declared_allow" ]; then
  : >"$tmp/allow-over.txt"
  i=0
  while [ "$i" -le "$declared_allow" ]; do
    printf 'larkaxctl.1 | 是a%d | 夹具：第 %d 条\n' "$i" "$i" >>"$tmp/allow-over.txt"
    i=$((i + 1))
  done
  STUB_FRESH="$fresh" MAN="$man1" GO="$tmp/bin/go-tightjoin" \
    WHITELIST="$tmp/allow-over.txt" bash "$SCRIPT" >"$last_out" 2>&1
  rc=$?
  if [ "$rc" -eq 1 ]; then ok; else bad "白名单超上限 —— 应失败（退出码 1），实际 $rc"; fi
  if grep -q '超过上限' "$last_out"; then ok; else
    bad "白名单超上限 —— 没说「超过上限」"
  fi
fi

printf 'larkaxctl.1 | 只有两栏\n' >"$tmp/allow-malformed.txt"
STUB_FRESH="$fresh" MAN="$man1" GO="$tmp/bin/go-tightjoin" \
  WHITELIST="$tmp/allow-malformed.txt" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "白名单格式坏 —— 应失败（退出码 1），实际 $rc"; fi
if grep -q '不合格式' "$last_out"; then ok; else bad "白名单格式坏 —— 没说哪一行不合格式"; fi

# 陈旧的条目（不再命中）只提示、不失败：那说明紧贴的写法被改掉了。
printf 'larkaxctl-doctor.1 | 夹a | 夹具：这一页没有紧贴\n' >"$tmp/allow-stale.txt"
STUB_FRESH="$fresh" MAN="$man1" GO="$tmp/bin/go-copy" \
  WHITELIST="$tmp/allow-stale.txt" bash "$SCRIPT" >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok; else bad "陈旧的白名单条目 —— 应通过（退出码 0），实际 $rc"; fi
if grep -q '已经不再命中' "$last_out"; then ok; else
  bad "陈旧的白名单条目 —— 没有提示可以删掉"
fi

# ── 17. --update 遇到判据 4 红：不写库 ────────────────────────────────────
#
# 一份少空格的手页不该因为「跑一次 --update」就进库：那会把判据 4 变成一句
# 建议而不是门。

man17="$tmp/man17"
mk_committed "$man17"
STUB_FRESH="$fresh" MAN="$man17" GO="$tmp/bin/go-tightjoin" \
  WHITELIST="$tmp/allow-empty.txt" bash "$SCRIPT" --update >"$last_out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok; else bad "--update 遇判据 4 红 —— 应失败（退出码 1），实际 $rc"; fi
if cmp -s "$man17/larkaxctl.1" "$fresh/larkaxctl.1"; then ok; else
  bad "--update 遇判据 4 红 —— 还是把生成结果写进了库"
fi

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
