#!/usr/bin/env bash
# 提交前红线自查：扫出密钥、私有主机、真实凭证等不该进公开仓库的内容。
#
# 用法：
#   scripts/check-redlines.sh              # 扫暂存区（pre-commit 用）
#   scripts/check-redlines.sh --all        # 扫整个工作树
#   scripts/check-redlines.sh HEAD~1       # 扫某个 commit 的改动
#
# 这是启发式检查，不是安全边界：它只认形态，不理解语义。
# 漏报比误报危险，所以宁可多报——命中后人工确认，确属误报就把该行写成本脚本允许的形态。
# 真正重要凭证的兜底是环境变量 + .gitignore，不是这个脚本。

set -uo pipefail

# 本脚本自身满屏都是检测模式，跳过
SKIP_FILES='^scripts/check-redlines\.sh$'

# 行内出现这些词就视为占位内容，不再报（整行粒度，属有意为之的粗筛）。
#
# localhost / 127.0.0.1 配「众所周知的服务端口」也放行：那类写法在示例配置里
# 到处都是，不携带任何本机信息。非默认端口（本机某个私有服务的端口）仍然会报。
ALLOW='<[A-Z_]+>|placeholder|EXAMPLE|example\.(com|org|net)|REDACTED|占位|示例'
ALLOW+='|(localhost|127\.0\.0\.1):(80|443|3000|3306|5432|6379|8000|8080|8081|8082|9090|27017)\b'

PATTERNS=(
  # 密钥形态
  'sk-[A-Za-z0-9_-]{8,}'
  'gh[pousr]_[A-Za-z0-9]{20,}'
  'AKIA[0-9A-Z]{16}'
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
  # 赋值式凭证：右边不是占位符才报
  '(api[_-]?key|apikey|secret|password|passwd|token|credential)[[:space:]]*[:=][[:space:]]*"[^"<{$]{6,}"'
  "(api[_-]?key|apikey|secret|password|passwd|token|credential)[[:space:]]*[:=][[:space:]]*'[^'<{$]{6,}'"
  # 私有主机与非标端口
  '(127\.0\.0\.1|localhost|0\.0\.0\.0):[0-9]{2,5}'
  # 本机私有路径
  '(/home|/Users)/[a-z][a-z0-9_-]+/'
  '\.bashrc|\.zshrc|\.profile'
  # 邮箱（example.* 由 ALLOW 兜掉）
  '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}'
)

# 标志位必须分开传。合并成 -n-P 时某些 grep 实现会解析失败并静默返回「无命中」。
GREP_OPTS=(-n)
if echo "" | grep -qP "" 2>/dev/null; then
  GREP_OPTS+=(-P)
else
  GREP_OPTS+=(-E)
fi

case "${1:-}" in
  --all)  files=$(git ls-files) ;;
  "")     files=$(git diff --cached --name-only --diff-filter=ACM) ;;
  *)      files=$(git diff --name-only --diff-filter=ACM "$1") ;;
esac

if [ -z "$files" ]; then
  echo "没有需要检查的文件。"
  exit 0
fi

hits=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  [[ "$f" =~ $SKIP_FILES ]] && continue
  # 跳过二进制与图片
  case "$f" in *.png|*.jpg|*.jpeg|*.gif|*.webp|*.ico|*.woff*|*.pdf) continue ;; esac

  found=""
  for p in "${PATTERNS[@]}"; do
    out=$(grep "${GREP_OPTS[@]}" -- "$p" "$f" 2>&1)
    rc=$?
    # rc>1 是 grep 自身出错（正则语法、参数、权限）。绝不能当成「没命中」放过。
    if [ "$rc" -gt 1 ]; then
      echo "错误：grep 在 $f 上执行失败（退出码 $rc）："
      printf '%s\n' "$out" | sed 's/^/   /'
      exit 2
    fi
    out=$(printf '%s\n' "$out" | grep -viE "$ALLOW")
    [ -n "$out" ] && found+="$out"$'\n'
  done

  if [ -n "$found" ]; then
    echo "── $f"
    printf '%s' "$found" | sort -u -t: -k1,1n | sed 's/^/   /'
    hits=$((hits + 1))
  fi
done <<< "$files"

echo
if [ "$hits" -gt 0 ]; then
  echo "发现 $hits 个文件命中红线。逐个确认：是占位符就改成本脚本允许的形态（见 AGENTS.md），是真值就换成占位符。"
  exit 1
fi
echo "红线自查通过。"
