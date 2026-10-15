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

pass=0
fail=0

# expect_hit <用例名> <文件内容>
expect_hit() {
  local name="$1" content="$2"
  local dir="$tmp/hit-$RANDOM"
  mkdir -p "$dir"
  printf '%s\n' "$content" > "$dir/sample.txt"
  ( cd "$dir" && git init -q && git add -A >/dev/null 2>&1 )
  if ( cd "$dir" && bash "$SCRIPT" >/dev/null 2>&1 ); then
    echo "✗ $name —— 应命中却报通过"
    fail=$((fail + 1))
  else
    pass=$((pass + 1))
  fi
}

# expect_clean <用例名> <文件内容>
expect_clean() {
  local name="$1" content="$2"
  local dir="$tmp/clean-$RANDOM"
  mkdir -p "$dir"
  printf '%s\n' "$content" > "$dir/sample.txt"
  ( cd "$dir" && git init -q && git add -A >/dev/null 2>&1 )
  if ( cd "$dir" && bash "$SCRIPT" >/dev/null 2>&1 ); then
    pass=$((pass + 1))
  else
    echo "✗ $name —— 应放行却报命中"
    fail=$((fail + 1))
  fi
}

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
expect_hit "真实邮箱" 'contact = "zhang.wei@somecorp.cn"'
expect_hit "本机非标端口" 'listen = "127.0.0.1:18080"'
expect_hit "内网 10 段非标端口" 'host = "10.1.2.3:7788"'
expect_hit "内网 192.168 段非标端口" 'host = "192.168.10.5:9000"'
expect_hit "内网 172.16-31 段非标端口" 'host = "172.20.0.3:8443"'

# 回归：过滤粒度曾是整行，同行出现占位词就把真值一起放过
expect_hit "与占位词同行的真密钥" 'password=correct-horse-battery-staple  # 示例环境（example）'
expect_hit "与占位词同行的形态密钥" 'key = "sk_live_Ab3Cd5Ef7Gh9Jk2MnPq"  # placeholder 待替换'
expect_hit "样例形态的 AWS 密钥" 'AWS_ACCESS_KEY_ID=AKIA3XQ7ZPLMN2VKRTYW'

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

# ── 结果 ──────────────────────────────────────────────────────────────────

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ] || exit 1
