# 本地开发环境

## 需要什么

- Go 1.26 或更新（`go.mod` 里写明了最低版本）
- Docker（跑 PostgreSQL，开发控制台前端时也用得到容器化的 Node）
- 只有开发前端才需要 Node.js 20 或更新

## 一次跑通

下面四条命令连着跑，是达成「服务起来并能响应」的最短路径。

```sh
# 1. 起数据库。端口被占用就改左边的数字，并同步改下一步的 DSN。
docker run -d --name larkax-pg \
  -e POSTGRES_USER=larkax \
  -e POSTGRES_PASSWORD=placeholder \
  -e POSTGRES_DB=larkax \
  -p 127.0.0.1:5432:5432 \
  postgres:16-alpine

# 2. 准备环境变量
cp .env.example .env

# 3. 生成主密钥并写进 .env 的 LARKAX_MASTER_KEYS
openssl rand -base64 32

# 4. 加载变量、建表、起服务
set -a && . ./.env && set +a
go run ./cmd/larkaxctl migrate up
go run ./cmd/larkax
```

验证：

```sh
curl -i localhost:9090/healthz
curl -i localhost:8080/v1/chat/completions
```

第一个返回 `200`，第二个返回 `501`（推理接口尚未实现，见下文「当前能做什么」）。
另可验证数据库探针：`curl -s localhost:9090/readyz` 应含 `"postgres":"ok"`。

`.env.example` 可以直接被 shell 读取，所以用 `set -a && . ./.env && set +a` 加载即可。
**服务本身不读 `.env` 文件**，只读环境变量；用 direnv 或别的工具加载也行。

## 请求快照的留存

快照按天分区（`request_snapshots_YYYY_MM_DD`），留存期从
`settings.request_snapshot.retention_days` 读。到期是整分区 `DROP`，
所以清理必须真的被执行——它不会自己发生。

```sh
go run ./cmd/larkaxctl snapshot cleanup --dry-run   # 只列出会删哪些分区
go run ./cmd/larkaxctl snapshot cleanup             # 执行删除
```

没有可删的分区时命令会打印 `已删除：0 个（没有到期分区）`，而不是什么都不输出。
「没有到期分区」与「命令没跑成」在安静的终端里长得一样，所以这条区别是刻意的。

写入时若当天分区不存在，写入方会**当场补建**它（幂等，每实例每天只做一次）。
不补建的话行会落进兜底分区 `request_snapshots_default`，而那里的行没有到期日——
留存承诺对它不成立。**兜底分区非空即说明有写入绕过了补建，应当告警。**

## 环境变量

一共八个，全部有明确理由：

| 变量 | 默认 | 说明 |
|---|---|---|
| `LARKAX_ROLE` | `gateway` | `gateway` / `control` / `worker` |
| `LARKAX_DB_DSN` | 无 | **必填**。PostgreSQL 连接串 |
| `LARKAX_MASTER_KEYS` | 无 | `gateway` 与 `control` **必填**，`worker` 不需要 |
| `LARKAX_HTTP_ADDR` | `:8080` | 业务入口监听地址 |
| `LARKAX_INTERNAL_ADDR` | `:9090` | 内部端点监听地址 |
| `LARKAX_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `LARKAX_ENV` | `development` | 设为 `production` 切换日志格式 |
| `LARKAX_REDIS_DSN` | 空 | 留空表示不启用 Redis |

必填项缺失时服务拒绝启动并一次列出全部问题，不会带着半截配置跑起来。

**为什么这么少：** 环境变量必须跟着数据一起走。备份恢复或换机器时漏了一个变量，
数据就可能不可用，而这类错误往往几周后才被发现。所以只有「连上数据库之前就必须拿到」
的值才做成环境变量（DSN、主密钥、监听地址），其余全部放在数据库的 `settings` 表里。

加新环境变量前请先问：它是不是必须在连上数据库之前拿到。不是的话就写进 `settings` 表。

`settings` 表随初始迁移建好，读取与校验在 `internal/settings`：启动时读一次，
与代码里声明的键清单对照，两边对不上就拒绝启动（见「加一个配置项」）。

## 加一个配置项

两步，缺一不可：

1. 在 `internal/migrate/sql/` 里新增一条迁移，插入默认值。
2. 在 `internal/settings/settings.go` 的 `Registry` 里加一行，声明键、类型与说明。

只做一步会被拒绝启动，并指出是哪个键对不上。这是有意的——
否则写错键名会安静地生效为一个不存在的配置，改动看起来做了，其实没做。

默认值只在迁移里写一份。代码里再写一份的话，两处迟早不一致，
而那种不一致极难发现：改了数据库，代码里的那份还在悄悄生效。

字符串类型的值要写成带引号的 JSON（`'"CNY"'`，不是 `'CNY'`）。

## 主密钥

格式 `"<标识>:<base64 的 32 字节>"`，多把用逗号分隔：

```
LARKAX_MASTER_KEYS="current:<MASTER_KEY>,previous:<MASTER_KEY>"
```

为什么支持多把：渠道凭证是用数据密钥（DEK）加密的，DEK 又用每把主密钥各包一份存在库里。
任何一把都能解开 DEK，所以——

- **轮换**时新旧并存即可，不需要停机重加密全部数据；
- **弄丢一把**（变量被覆盖、密钥服务迁移出错）不会让存量数据变成废纸；
- 可以留一把离线保管作为恢复密钥。

标识必须保持稳定，密文里记着它，改名等于丢失对应数据的可读性。
只有一把时可以不写前缀，直接填 base64。

生成本地开发用的密钥：

```sh
openssl rand -base64 32
```

## 建表

```sh
go run ./cmd/larkaxctl migrate up
go run ./cmd/larkaxctl migrate status
```

迁移文件在 `internal/migrate/sql/`，用 goose 的命名与格式（`-- +goose Up` / `-- +goose Down`）。
它们会被 `go:embed` 编进二进制，部署时不需要另外投放 SQL。

新建迁移：

```sh
go run ./cmd/larkaxctl migrate create <名称>
```

文件会直接生成到 `internal/migrate/sql/`，并强制为 `.sql` 格式
（goose 默认生成 `.go`，那种既不会被内嵌也不会被执行，还会在迁移目录里多出一个包）。

开发时想推倒重来：

```sh
go run ./cmd/larkaxctl migrate down-to 0
go run ./cmd/larkaxctl migrate up
```

不要手写 `DROP`；迁移的 Down 段是唯一被验证过的回滚路径。

### 迁移能不能改

**分界线是「有没有一个不在开发者本人机器上的数据库」。**

在这之前，修改已有迁移文件是可以的，而且更好：迁移历史读起来应当是对当前 schema
的一份连贯描述，而不是一部「四天前那个 bug 怎么修的」流水账。

一旦有任何共享数据库——**包括 staging**——已有迁移一律只追加，不再改动。
此时代码与数据库已经可能分叉，就地改会让两者静默不一致，而现场表现是
「本地能跑，部署后表结构不对」。

首次打 tag、第一个外部贡献者、公开发布都不构成分界线；共享数据库才是。

## 当前能做什么

目前只有骨架，**推理接口还没实现**：

| 端口 | 端点 | 现在返回 |
|---|---|---|
| `:8080` | 任意路径 | `501`，明确的「尚未实现」JSON |
| `:9090` | `/healthz` | `200`，进程存活 |
| `:9090` | `/readyz` | `200`，含 `postgres` 探针 |

启动时会连数据库并加载 `settings`。连不上或配置对不上就拒绝启动，
`/readyz` 也会在数据库不可用时返回 `503`。

`501` 是有意的占位：任何真实流量都会立刻得到明确答复，而不是超时或误导性的 404。
这样部署验证能确认端口通、TLS 通、负载均衡转发通。

内部端点单独一个端口：它面向编排系统与指标抓取器，不和业务入口共用监听地址，
也就不需要跟着业务入口一起暴露在 ingress 规则下。

运维命令行走另一个入口，目前有迁移与对账：

```sh
go run ./cmd/larkaxctl migrate up
go run ./cmd/larkaxctl reconcile
```

对账的开关、口径与运行记录见 [reconciliation.md](reconciliation.md)。

## 常用检查

```sh
gofmt -l .                          # 列出未格式化的文件
go vet ./...
golangci-lint run ./...             # 跨文件与语义检查，配置见 .golangci.yml
go test ./...
go test -race ./...
go test -cover ./...

bash scripts/check-redlines.sh      # 检查暂存区是否混入密钥与私有信息
bash scripts/check-redlines.sh --all
bash scripts/check-layering.sh      # 检查分层约定有没有被破坏

bash scripts/check-redlines.test.sh # 只在改过红线脚本后需要跑
```

提交前至少跑一遍 `gofmt -l .`、`go vet ./...`、`go test ./...` 和两个自查脚本。
CI 会跑同样的东西，本地跑是为了少一轮来回。

## 变异测试

改了**计费、账本、金额**这三处代码时，除了单测还要跑变异测试：

```sh
export LARKAX_DB_DSN="postgres://larkax:placeholder@127.0.0.1:5432/larkax?sslmode=disable"
scripts/check-mutation.sh              # 全部包
scripts/check-mutation.sh money route  # 只跑指定的
```

`LARKAX_DB_DSN` 是必须的：没有它 `internal/ledger/store` 的集成测试整包
被 `t.Skip`，那一包等于没测。脚本会直接拒绝运行，而不是跑出一批
不可比的数字。

覆盖率只说明「这一行被执行过」，不说明「这一行的每个分支被钉住了」。
实测：计费与账本各包的覆盖率都在 82%~98%，而变异测试每次都找出两位数个
**存活**变异——它们集中在「恰好相等」「恰好为零」这类边界上，
而写测试时的自然倾向正是跳过这些。

**不要自己拼 `gremlins` 命令**：并发数用错会让结果完全失真。实测
`internal/money` 在系数固定 10 时，workers=1/2 得到 `Killed 80`，
workers=4 得到 `Killed 3, Timed out 92`——同一份代码，看起来像两个
不同的结论。脚本固定 `workers=1`，详见 AGENTS.md。

**读分数的时候连结果一起读超时列。** 超时不为 0 说明这一轮受了机器负载的
干扰（gremlins 的超时按墙钟算），那几个数不与历史比较。

存活变异要**逐条判断**，不是看一个总数。只有两类可以放过：

- **等价变异**：改法与改后行为在数值上不可区分
- **不可达分支**：前置条件已经保证了走不到

放过时在测试里写一行说明，否则下一个人会以为是漏了。
