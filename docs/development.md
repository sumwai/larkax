# 本地开发环境

## 需要什么

- Go 1.26 或更新（`go.mod` 里写明了最低版本）
- Docker（跑 PostgreSQL 与 Redis）
- Node.js 20 或更新（只在开发控制台前端时需要，前端尚未开始做）

## 起依赖

```sh
docker run -d --name larkax-pg \
  -e POSTGRES_PASSWORD=placeholder \
  -e POSTGRES_DB=larkax \
  -p 127.0.0.1:5432:5432 \
  postgres:16-alpine

docker run -d --name larkax-redis \
  -p 127.0.0.1:6379:6379 \
  redis:7-alpine
```

## 配置

```sh
cp .env.example .env
```

`.env` 里的值按需改。生成主密钥：

```sh
openssl rand -base64 32
```

`.env` 在 `.gitignore` 里，不会被提交。**不要把真实凭证写进 `.env.example`**，那是给人看的模板。

服务只从环境变量读配置，不读 `.env` 文件本身。想用 `.env` 的话在启动命令前加
`set -a && . ./.env && set +a`，或者用你习惯的工具（direnv、dotenv 等）。

## 建表

```sh
export LARKAX_DB_DSN='postgres://postgres:placeholder@127.0.0.1:5432/larkax?sslmode=disable'
export LARKAX_ROLE=worker

go run ./cmd/larkaxctl migrate up
go run ./cmd/larkaxctl migrate status
```

`migrate down` 只回滚最近一次迁移。开发时想推倒重来就 `down` 到 0 再 `up`，
不要手写 DROP。

## 起服务

```sh
go run ./cmd/larkax
```

默认监听两个端口：

| 端口 | 用途 | 端点 |
|---|---|---|
| `:8080` | 业务入口 | M0 阶段只有占位响应 |
| `:9090` | 内部端点 | `/healthz`、`/readyz`，后续加 `/metrics` |

内部端点单独一个端口是有意的：它面向编排系统与指标抓取器，
不应该和业务入口共用监听地址，也不应该在同一个 ingress 规则下暴露出去。

## 常用检查

```sh
gofmt -l .                          # 列出未格式化的文件
go vet ./...
go test ./...
go test -race ./...
go test -cover ./...

bash scripts/check-redlines.sh      # 检查暂存区是否混入密钥与私有信息
bash scripts/check-redlines.sh --all
```

提交前至少跑一遍 `gofmt -l .`、`go vet ./...`、`go test ./...` 和红线自查。
CI 会跑同样的东西，本地跑是为了少一轮来回。

## 角色

同一个二进制以 `LARKAX_ROLE` 区分。本地开发一般用 `gateway`，
跑后台任务（日历同步、额度探测、对账）时用 `worker`。

`gateway` 与 `control` 要求配置渠道凭证主密钥；`worker` 不碰凭证，不要求。
这是有意的——不碰凭证的进程就不该拿到解密密钥。
