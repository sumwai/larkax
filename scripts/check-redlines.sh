#!/usr/bin/env bash
# 提交前红线自查：扫出密钥、私有主机、真实凭证等不该进公开仓库的内容。
#
# 用法：
#   scripts/check-redlines.sh              # 扫暂存区（pre-commit 用）
#   scripts/check-redlines.sh --all        # 扫整个工作树
#   scripts/check-redlines.sh <ref>        # 扫 <ref> 到工作树之间的改动
#
# 退出码：0 干净、1 命中、2 扫描器自身出错（grep 失败、文件读不了）。
# 2 与 1 必须分开：检查工具坏掉时的输出和「干净」长得一样。
#
# 这是启发式检查，不是安全边界：它只认形态，不理解语义。
# 漏报比误报危险，所以宁可多报——命中后人工确认，确属误报就把该行写成
# 本脚本允许的形态（见 AGENTS.md 的占位符约定）。
# 真正重要凭证的兜底是环境变量 + .gitignore，不是这个脚本。
#
# 本脚本有一份自测：scripts/check-redlines.test.sh。改动后必须跑它。

set -uo pipefail

# 本脚本自身满屏都是检测模式，跳过
SKIP_FILES='^scripts/check-redlines\.(sh|test\.sh)$'

# 命中的**片段**里出现这些词就放行，不报。
#
# 过滤的是片段而不是整行：按整行过滤时，只要同一行别处出现「example」或「占位」
# 字样，该行上的真密钥会被一起放过。
#
# 白名单里只放**结构性**的占位形态。厂商文档里的样例值（AKIA…EXAMPLE）不能进
# 白名单——那正是要抓的东西，放行它等于给最像真密钥的字符串开后门。
ALLOW='<[A-Z_]+>|placeholder|example\.(com|org|net)|REDACTED|占位|示例'
# 环境变量引用不是字面量。右侧不要求闭合花括号：-o 取到的片段可能被截在 } 之前。
ALLOW+='|\$\{?[A-Za-z0-9_]*\}?'
# 值是**选择器链**而不是字面量：`Tokens: r.Usage.OutputTokens`、`secret: cfg.S.Key`。
# 凭证不会长成 `pkg.Field.SubField` 这种形状，而 Go/Rust/TS 的字段访问随处可见，
# 且它们的字段名里常有 token / secret 这类词。不排除的话，
# 每一个带此类字段名的赋值语句都会被报出来，噪音会淹掉真命中。
#
# 这里匹配的是**去掉键名与分隔符之后的值**，所以模式从行首锚定，
# 不再要求前面有 = 或 :。
ALLOW+='|^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$'
ALLOW+='|^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+[[:space:]]*[({]'
# localhost / 127.0.0.1 / 内网网段配「众所周知的服务端口」放行：那类写法在示例
# 配置里到处都是，不携带任何本机信息。非默认端口仍然会报。
ALLOW+='|(localhost|127\.0\.0\.1|10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}'
ALLOW+='|192\.168\.[0-9]{1,3}\.[0-9]{1,3}'
ALLOW+='|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}):(80|443|3000|3306|5432|6379|8000|8080|8081|8082|9090|27017)\b'

# 形态类：命中片段整体就是敏感内容，按整个片段判断是否为占位。
PATTERNS_SHAPE=(
  # 各家前缀不同，认的是形态而不是某一家。大小写不敏感：
  # 变量名写成 STRIPE_SECRET_KEY 也要命中。
  '(sk|pk|rk)[-_](live|test|proj)?[-_]?[A-Za-z0-9_-]{12,}'
  'AKIA[0-9A-Z]{16}'
  'AIza[0-9A-Za-z_-]{35}'
  'xox[baprs]-[A-Za-z0-9-]{10,}'
  'gh[pousr]_[A-Za-z0-9]{20,}'
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'

  # 私有主机与非标端口
  '(127\.0\.0\.1|localhost|0\.0\.0\.0|10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}):[0-9]{2,5}'

  # 本机私有路径与 shell 配置文件
  '(/home|/Users)/[a-z][a-z0-9_-]+/'
  '\.bashrc|\.zshrc|\.profile'

  # 邮箱（example.* 由 ALLOW 兜掉）
  '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}'
)

# 赋值类：命中片段形如「键=值」，只按**值**判断是否为占位。
# 键名里出现 example 之类的词不应当放过同一个片段里的真值，
# 所以这两类要分开按不同粒度过滤。
#
# 引号可有可无——.env、shell、YAML、JSON 里没有引号的写法很常见，漏掉等于不设防。
# 右侧允许连字符与下划线：口令常写成 few-words-strung-together 或
# correct_horse_battery，只认字母数字会漏掉这类真口令。
#
# 代价是它会命中 Go 代码：`Tokens: r.Usage.OutputTokens` 里有「token」这个键名，
# 右侧又是一长串标识符，形态上与凭证无从区分。这类由 ALLOW 里
# 「值是选择器链」那一条兜掉，见下。
PATTERNS_ASSIGN=(
  '(api[_-]?key|apikey|secret|password|passwd|token|credential|access[_-]?key)[A-Za-z0-9_]*[[:space:]]*[:=][[:space:]]*[^[:space:]]*[A-Za-z0-9_][A-Za-z0-9_-]{11,}'
)

# 标志位必须分开传。合并成 -n-P 时某些 grep 实现会解析失败并静默返回「无命中」。
# -i 是有意的：这是按形态抓密钥，大小写不该影响结果。
# -o 只取命中片段：过滤与输出都以片段为单位，不被同行的无关文字干扰。
GREP_OPTS=(-n -o -i)
# 探测 PCRE 支持时同样不能合并标志——上面那句注释说的就是这件事，
# 探测本身用 -qP 就是重犯。分开写，让「探测失败」和「引擎不支持」表现一致。
if echo "" | grep -q -P "" 2>/dev/null; then
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

# scan <文件> <模式> <过滤粒度: frag|value>
# 输出该模式下未被放行的命中行；grep 自身出错则以退出码 2 终止整个检查。
scan() {
  local f="$1" p="$2" mode="$3" out rc line frag subject
  out=$(grep "${GREP_OPTS[@]}" -- "$p" "$f" 2>&1)
  rc=$?
  # rc>1 是 grep 自身出错（正则语法、参数、权限）。绝不能当成「没命中」放过。
  if [ "$rc" -gt 1 ]; then
    echo "错误：grep 在 $f 上执行失败（退出码 $rc）：" >&2
    printf '%s\n' "$out" | sed 's/^/   /' >&2
    exit 2
  fi
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    frag=${line#*:}
    if [ "$mode" = value ]; then
      subject=${frag#*[=:]}
    else
      subject=$frag
    fi
    grep -qiE "$ALLOW" <<<"$subject" && continue
    printf '%s\n' "$line"
  done <<< "$out"
}

hits=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  [[ "$f" =~ $SKIP_FILES ]] && continue
  # 跳过二进制与图片
  case "$f" in *.png|*.jpg|*.jpeg|*.gif|*.webp|*.ico|*.woff*|*.pdf) continue ;; esac

  found=""
  for p in "${PATTERNS_SHAPE[@]}"; do
    found+=$(scan "$f" "$p" frag)$'\n'
  done
  for p in "${PATTERNS_ASSIGN[@]}"; do
    found+=$(scan "$f" "$p" value)$'\n'
  done
  found=$(printf '%s' "$found" | sed '/^$/d')

  if [ -n "$found" ]; then
    echo "── $f"
    printf '%s\n' "$found" | sort -u -t: -k1,1n | sed 's/^/   /'
    hits=$((hits + 1))
  fi
done <<< "$files"

echo
if [ "$hits" -gt 0 ]; then
  echo "发现 $hits 个文件命中红线。逐个确认：是占位符就改成本脚本允许的形态（见 AGENTS.md），是真值就换成占位符。"
  exit 1
fi
echo "红线自查通过。"
