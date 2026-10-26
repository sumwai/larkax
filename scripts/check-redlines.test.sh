#!/usr/bin/env bash
# check-redlines.sh 的自测。
#
# 为什么需要它：LESSONS.md 第 2 条记着，这个脚本曾经「静默漏报」——
# 自己报「通过」，实际根本没在检查。检查工具坏掉时输出和「干净」一模一样，
# 所以它必须有一份已知会命中的样本，否则「通过」不能证明任何事。
#
# 每次改动 check-redlines.sh 后都跑一遍这个。

set -uo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$PWD/scripts/check-redlines.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

last_out=""
. "$PWD/scripts/gateselftest.sh"

# 样本文件名默认用 .env：赋值类模式**只在配置与脚本文件上生效**
#（见 check-redlines.sh 里的 ASSIGN_FILES），用 sample.txt 会让它整组失效。
DEFAULT_SAMPLE="sample.env"

# expect_hit <用例名> <文件内容> [文件名]
expect_hit() {
  local name="$1" content="$2" file="${3:-$DEFAULT_SAMPLE}"
  local dir="$tmp/hit-$RANDOM"
  mkdir -p "$dir"
  printf '%s\n' "$content" > "$dir/$file"
  ( cd "$dir" && git init -q && git add -A >/dev/null 2>&1 )
  if ( cd "$dir" && bash "$SCRIPT" >/dev/null 2>&1 ); then
    echo "✗ $name —— 应命中却报通过"
    GST_FAIL=$((GST_FAIL + 1))
  else
    GST_PASS=$((GST_PASS + 1))
  fi
}

# expect_clean <用例名> <文件内容> [文件名]
expect_clean() {
  local name="$1" content="$2" file="${3:-$DEFAULT_SAMPLE}"
  local dir="$tmp/clean-$RANDOM"
  mkdir -p "$dir"
  printf '%s\n' "$content" > "$dir/$file"
  ( cd "$dir" && git init -q && git add -A >/dev/null 2>&1 )
  if ( cd "$dir" && bash "$SCRIPT" >/dev/null 2>&1 ); then
    GST_PASS=$((GST_PASS + 1))
  else
    echo "✗ $name —— 应放行却报命中"
    GST_FAIL=$((GST_FAIL + 1))
  fi
}

# ── 关于样本值 ────────────────────────────────────────────────────────────
#
# 这里刻意用「前缀对了但取值是字母序列」的样本，而不是形似真实密钥的串。
# 原因是 GitHub 的 push protection 会扫提交历史，而它不看我们的白名单：
# 一份像真的 sk_live_… / xoxb-… 样本会让 push 被直接拒绝，
# 而修好它只能重写历史。样本的用途是证明**模式**能命中，取值本身无所谓。
# 编写规则：
#   - 前缀保留（那是被测的东西），后面跟明显是占位的字母序列
#   - 不出现厂商文档里的样例值（AKIA…EXAMPLE 那类），它们同样会触发拦截
#   - 确需真实形态的用例，先本地跑本文件确认能命中，再确认 push 不被拒

# ── 应当命中 ──────────────────────────────────────────────────────────────

expect_hit "各家密钥前缀" 'STRIPE_SECRET_KEY="sk_live_Ab3Cd5Ef7Gh9Jk2MnPq"'
expect_hit "密钥无引号" 'api_key=abcdef1234567890'
expect_hit "密钥单引号" "token='abcdef1234567890'"
expect_hit "小写变量名" 'password=correct-horse-battery-staple'
expect_hit "AWS 长密钥" 'AWS_SECRET_ACCESS_KEY="Ab3Cd5Ef7Gh9Jk2MnPq4Rs6Tu"'
expect_hit "AWS Access Key ID" 'aws_access_key_id = "AKIA3XQ7ZPLMN2VKRTYW"'
expect_hit "GitHub token" 'GITHUB_TOKEN=ghp_AbCdEfGhIjKlMnOpQrSt'
expect_hit "Google API key" 'key = "AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz0123456"'
expect_hit "Slack token" 'SLACK=xoxb-482913750246-AbCdEfGh'
expect_hit "私钥文件头" '-----BEGIN RSA PRIVATE KEY-----'
expect_hit "本机路径" 'root = /home/somebody/work/larkax'
expect_hit "shell 配置文件" 'source ~/.bashrc'
expect_hit "行首的 shell 配置文件" '.profile'
expect_hit "赋值里的 shell 配置文件" 'PATH="$PATH:~/.zshrc"'
expect_hit "真实邮箱" 'contact = "zhang.wei@somecorp.cn"'
expect_hit "本机非标端口" 'listen = "127.0.0.1:18080"'
expect_hit "内网 10 段非标端口" 'host = "10.1.2.3:7788"'
expect_hit "内网 192.168 段非标端口" 'host = "192.168.10.5:9000"'
expect_hit "内网 172.16-31 段非标端口" 'host = "172.20.0.3:8443"'

# 回归：过滤粒度曾是整行，同行出现占位词就把真值一起放过
expect_hit "与占位词同行的真口令" 'password=correct-horse-battery-staple  # 示例环境（example）'
expect_hit "与占位词同行的形态密钥" 'key = "sk_live_Ab3Cd5Ef7Gh9Jk2MnPq"  # placeholder 待替换'
expect_hit "厂商样例形态的 AWS 密钥" 'AWS_ACCESS_KEY_ID=AKIA3XQ7ZPLMN2VKRTYW'
expect_hit "更长但仍含占位词的形态" 'key = "sk_live_Ab3Cd5Ef7Gh9Jk2MnPq"  # 占位待换'

# ── 应当放行 ──────────────────────────────────────────────────────────────

expect_clean "占位符尖括号" 'api_key = "<API_KEY>"'
expect_clean "示例域名" 'base_url = "https://api.example.com/v1"'
expect_clean "示例邮箱" 'contact = "user@example.com"'
expect_clean "本地开发默认端口" 'dsn = "postgres://user:placeholder@localhost:5432/larkax"'
expect_clean "Redis 默认端口" 'LARKAX_REDIS_DSN="redis://localhost:6379/0"'
expect_clean "内网地址加默认端口" 'host = "10.0.0.5:5432"'
expect_clean "占位词" 'password = "placeholder-password-value"'
expect_clean "环境变量引用" 'api_key = "${LARKAX_API_KEY}"'
expect_clean "普通计数与配置" 'max_conns = 20
shutdown_timeout = 30s
retention_days = 30'
expect_clean "短标识不算密钥" 'token_type = bearer
key_id = k1'

# 回归：代码里的字段访问不是凭证
expect_clean "Go 字段访问" 'Tokens: r.Usage.OutputTokens'
expect_clean "Go 结构体字面量" 'TokensDetails: dto.InputTokenDetails'
expect_clean "链式选择器带括号" 'token: cfg.Auth.Token'
expect_clean "多级选择器单独成值" 'secret: app.config.Store.Key'
expect_clean "带空格的赋值" 'token = resp.Data.Token'

# 回归：字段名叫 Profile 不是 shell 配置文件。这条抓到过一次真实误报——
# 一个 `*ModelProfile` 字段在十几处被访问，整棵树的红线自查全红。
expect_clean "Go 里的 Profile 字段" 'profile, err := s.profile(ctx, name)' 'sample.go'
expect_clean "Go 里的切片元素字段" 'out[i].Profile = profile' 'sample.go'
expect_clean "Go 注释里的候选字段" '// 记录时不做判据（见 Candidate.Profile）' 'sample.go'

# 但真凭证混在同类行里仍要报：选择器放行的是「整行只有选择器」，
# 别处有字面量就得报出来
expect_hit "选择器同行仍有真口令" 'password: correct-horse-battery-staple'

# ── 赋值类只在配置与脚本上生效 ────────────────────────────────────────────
#
# 这是被误报逼出来的：赋值类模式认形态，而代码里到处是形状相同的表达式。
# 补白名单能治标，但每遇到一种新语法就要再补一次。

# 同样的内容在源码文件里不该报——那是变量赋值，不是凭证配置
expect_clean "Go 源码里的字段访问" 'Tokens: r.Usage.OutputTokens' 'sample.go'
expect_clean "Go 源码里的解引用" 'Tokens = *u.PromptTokens' 'sample.go'
expect_clean "TS 源码里的赋值" 'token = resp.data.accessToken' 'sample.ts'
expect_clean "Python 里的字段访问" 'self.token = cfg.auth.token' 'sample.py'

# 但在配置文件里仍然要报
expect_hit "配置文件里的真口令" 'password=correct-horse-battery-staple' 'app.yaml'
expect_hit "env 文件里的真口令" 'DB_PASSWORD=correct-horse-battery-staple' '.env'
expect_hit "shell 脚本里的导出" 'export API_KEY=abcdef1234567890' 'setup.sh'
expect_hit "JSON 配置里的真口令" '{"password":"correct-horse-battery-staple"}' 'config.json'
expect_hit "TOML 配置里的真口令" 'password = "correct-horse-battery-staple"' 'app.toml'

# 形态类（密钥前缀、私钥头、私有主机）不受文件类型限制：
# 它们在代码里同样意味着泄露
expect_hit "源码里的私钥头" '-----BEGIN RSA PRIVATE KEY-----' 'sample.go'
expect_hit "源码里的本机路径" 'root = /home/somebody/work/larkax' 'sample.go'

# ── --all 必须覆盖尚未 add 的文件 ─────────────────────────────────────────
#
# 只扫 git ls-files（已跟踪）会留一个盲区：新写的文件在推送前不在扫描范围内，
# 本机自查「通过」会让人以为没问题，等 CI 扫到时就晚了。

uncovered_dir="$tmp/all-untracked"
mkdir -p "$uncovered_dir"
printf '%s\n' 'password=correct-horse-battery-staple' > "$uncovered_dir/new_file.env"
( cd "$uncovered_dir" && git init -q )
if ( cd "$uncovered_dir" && bash "$SCRIPT" --all >/dev/null 2>&1 ); then
  echo "✗ --all 漏掉了尚未 add 的文件"
  GST_FAIL=$((GST_FAIL + 1))
else
  GST_PASS=$((GST_PASS + 1))
fi


# ── 结果 ──────────────────────────────────────────────────────────────────

report
