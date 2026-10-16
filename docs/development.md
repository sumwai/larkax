# 本地开发环境

## 需要什么

- Go 1.26 或更新（`go.mod` 里写明了最低版本）
- Docker 与 `docker compose` v2.1 或更新（跑 PostgreSQL）
- `make`
- 只有开发前端才需要 Node.js 20 或更新

**app 不进容器。** compose 只负责依赖的生命周期，服务本身留在宿主上 `go run`——
改一行即时生效，不必每改一次就 rebuild 镜像。全栈容器化适合「只想跑起来看看」，
不适合开发。

## 一次跑通

```sh
make run
```

一条命令走完：生成 `.env`（只在不存在时）→ 起 PostgreSQL 并等它就绪 → 应用迁移 →
前台跑服务。Ctrl-C 退出服务，数据库仍在跑，用 `make down` 停它。

`.env` 里的主密钥由 `make` 直接生成写进去（`openssl rand -base64 32`），
不需要手工粘。

`.env` 里的 DSN 默认指向 `127.0.0.1:5432`。**端口被占用就换一个**：

```sh
make run PG_PORT=55432
```

**`.env` 只在不存在时生成，之后不重建。** 覆盖它等于毁掉已加密的存量数据——
主密钥一变，密文就解不开了。

但 `PG_PORT` / `PG_DB` / `DB_PASSWORD` 与 DSN 是同一份配置，仍然每次生效：
`make` 会把 `.env` 里 `LARKAX_DB_DSN` 那一行同步到这三个变量的当前值，
**其余内容一个字不动**。所以换端口只要 `make run PG_PORT=55432`，
不需要 `make clean`（那会连数据卷一起清掉），也不需要手改 `.env`。

DSN 指向的不是本机时不同步——那种 `.env` 是故意配到远端库的——只提示一句。

这条同步不是锦上添花。DSN 与 compose 起的库必须指同一个地方，而不同步的坏法
不止「连不上」：如果原端口上恰好有另一个 larkax 库、口令也相同，连接会
**安静地成功**，然后迁移打到了那个库上，而人以为打的是自己刚起的那个。

验证：

```sh
curl -i localhost:9090/healthz
curl -i localhost:8080/v1/chat/completions
```

第一个返回 `200`，第二个返回 `405`——这条路径只接受 `POST`。推理接口本身
已经接上了公共端口，见下文「当前能做什么」。
另可验证数据库探针：`curl -s localhost:9090/readyz` 应含 `"postgres":"ok"`。
**健康检查在 `:9090`，不在 `:8080`**——公共端口的 `/` 捕获一切路径并返回 `501`。

指标在同一个内部端口上，`curl -s localhost:9090/metrics` 应含
`larkax_billing_capped_settlements_total 0`。指标清单与命名约定见
[metrics.md](metrics.md)。

**`:9090` 是个常被占的端口**：Clash / mihomo 的 external-controller 默认就是它，
Prometheus 也是。撞上之后服务完全起不来（不是降级），所以 `make run` 在起服务前
会先检查这两个监听地址，占用时会告诉你**是哪个**、以及**怎么换**。

换的方式是把 `LARKAX_INTERNAL_ADDR`（或 `LARKAX_HTTP_ADDR`）写进 `.env`——那样
只需改一次，不必每次都在命令行上带：

```sh
LARKAX_INTERNAL_ADDR=":9091"
```

其余目标：

| 命令 | 做什么 |
|---|---|
| `make up` | 只起依赖，等 healthcheck 通过 |
| `make down` | 只停依赖，保留数据卷 |
| `make migrate` | 只跑迁移 |
| `make test` | 跑测试（先确保依赖起着） |
| `make clean` | 清容器、数据卷与生成的 `.env`，回到刚克隆的状态 |

`make test` 会先起依赖并加载 `.env`——集成测试拿不到 `LARKAX_DB_DSN` 时会**跳过**，
而「跳过」在输出里和「通过」长得一样。

`.env.example` 可以直接被 shell 读取，所以 `make` 用 `set -a && . ./.env && set +a`
加载它。**服务本身不读 `.env` 文件**，只读环境变量；用 direnv 或别的工具加载也行。

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

## 分时计价的日历数据

分时计价要判断某一天是不是工作日，而这件事不能靠星期推——调休把周末挪成了
上班日。数据从文件导入：

```sh
go run ./cmd/larkaxctl calendar import ./cn-2031.json
```

文件格式、状态取值与落库语义见 [calendar-data.md](calendar-data.md)。
**仓库里不带任何真实数据集**：数据从哪来由运维决定，这条命令只负责读文件写库。

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

### 轮换

换主密钥分两步：先把新的放进 `LARKAX_MASTER_KEYS` 的**首位**（旧的留在后面）并重启，
再给存量密文补上新的副本。

```sh
go run ./cmd/larkaxctl rotate            # 干跑：列出有多少行要改，不写库
go run ./cmd/larkaxctl rotate --apply    # 轮换：可中断、可重跑
go run ./cmd/larkaxctl rotate --verify   # 每一行是否都含新主密钥
```

顺序是「新钥匙放进首位 → 轮换 → `--verify` 退出码为 0 → 才把旧钥匙去掉」。
旧钥匙去掉得太早，那些还没轮换到的行就再也解不开了。完整说明见
`larkaxctl rotate -h`。

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

## 配一条能用的渠道

推理请求要落到某个上游，所以库里得先有四样东西：一条渠道（指向上游、带凭证）、
目录里的一条规范模型、把上游模型名映射到规范名的别名、一条计价规则。
`larkaxctl` 的 `channel` / `model` / `alias` / `price` 四组子命令就是干这个的。

下面四条命令按顺序跑完，就有了一条能接请求的链路。示例里的上游是假的
（`https://api.example.com/v1`），换成你自己的即可。

**一、建一条渠道。** 凭证只能从标准输入或环境变量读，**不能写在命令行参数里**
——参数会出现在 `ps` 的输出与 shell 历史里：

```sh
# 从标准输入
cat secret.txt | go run ./cmd/larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-stdin

# 或者从环境变量
LARKAX_CHANNEL_CREDENTIAL=<CHANNEL_CREDENTIAL> go run ./cmd/larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-env LARKAX_CHANNEL_CREDENTIAL
```

`--type` 必须是已注册的适配器类型（当前是 `openai` 与 `anthropic`）。
`--models` 是这条渠道支持的**规范模型名**（下一步要建的那个），至少一个：
读取路径把 `models` 当白名单用，空数组的渠道永远选不中。
stdout 只有新渠道的 ID，其余信息走 stderr。

**二、把规范模型加进目录。** 规范名是选路与计费共用的键，不能带空白字符：

```sh
go run ./cmd/larkaxctl model add --canonical claude-x --display '示例模型' \
  --vendor example --context-limit 200000 --tools --vision
```

**三、配别名：这家上游对同一个模型叫什么名字。**

```sh
go run ./cmd/larkaxctl alias add --canonical claude-x \
  --upstream claude-3-5-sonnet-latest
```

不带 `--channel` 的别名是平台级的，对所有渠道生效；带 `--channel <ID>` 的只对
那个渠道生效，并盖过平台级那条。同一个范围里一个规范名只能有一个上游名。

**四、配一条计价规则。** 没有生效的规则时请求会失败，而不是按零价放行：

```sh
go run ./cmd/larkaxctl price add --scope platform --name example-price \
  --expr '{"prompt": 0.0000015, "completion": 0.0000075}'
```

`--models` 留空表示适用于全部模型（兜底规则）。表达式在写入前会编译一次，
编译不过的命令直接失败。作用域决定哪两栏 id 有效：`platform` 两栏都留空、
`merchant` 必须给 `--owner`、`channel` 必须给 `--channel`。

**查看现状**：

```sh
go run ./cmd/larkaxctl channel list
go run ./cmd/larkaxctl model list
go run ./cmd/larkaxctl alias list
go run ./cmd/larkaxctl price list
```

`channel list` **不打印凭证密文**，只打印它挂在哪些主密钥下（那不需要任何
密钥材料）。`price list` 按裁决次序排列，排第一的那条就是会被选中的那条。

**退出码**：`0` 成功、`1` 其他失败、`2` 命令行参数错、`3` 连不上数据库、
`4` 约束冲突（唯一键、外键、要改的那一行不存在）。脚本按这个分派处置。

渠道的写入命令需要主密钥（`LARKAX_MASTER_KEYS`）：凭证是信封加密的，只有
`credential.Sealer` 能产出可解开的密文，直接往库里写一行 `bytea` 补不上。
它读的是这个环境变量本身，与 `LARKAX_ROLE` 无关。`channel list` / `enable` /
`disable` 不碰凭证，没有主密钥也能跑。

每条子命令的完整用法见 `larkaxctl <子命令> --help`，例如
`go run ./cmd/larkaxctl channel --help`。

## 造用户、造 key、打一次请求

推理请求要带一把 API Key，而 key 必须挂在某个用户上，所以这条链的起点是建用户：

```sh
go run ./cmd/larkaxctl user create --name alice      # stdout 是新用户的 ID
go run ./cmd/larkaxctl apikey create --user alice    # stdout 是 key 的明文
```

两条命令的 stdout 都**只有可以直接接走的那个值**（前者是 ID，后者是明文），
其余信息走 stderr。所以它们能直接串起来：

```sh
KEY=$(go run ./cmd/larkaxctl apikey create --user "$(go run ./cmd/larkaxctl user create --name alice)")
```

`user create` 还有 `--email` 与 `--role`（`user`、`merchant`、`admin`，默认 `user`）。
用户名唯一且大小写不敏感，重名会直接报错——**它不会改到已有的那个用户**，
所以这条命令不能拿来「给已有用户补点什么」。

拿到的 key 打一次推理请求：

```sh
curl -i localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<模型名>","messages":[{"role":"user","content":"hi"}]}'
```

推理接口接到公共端口上的进度见下文「当前能做什么」。

## 当前能做什么

**非流式推理已经接到公共端口上**：

| 端口 | 端点 | 现在返回 |
|---|---|---|
| `:8080` | `POST /v1/chat/completions`、`POST /v1/messages` | 推理；没有有效 API Key 时 `401` |
| `:8080` | 其余任意路径 | `501`，明确的「尚未实现」JSON |
| `:9090` | `/healthz` | `200`，进程存活 |
| `:9090` | `/readyz` | `200`，含 `postgres` 探针 |
| `:9090` | `/metrics` | Prometheus 文本格式的指标，清单见 [metrics.md](metrics.md) |

装配在 `internal/assembly`，由 `cmd/larkax` 调用：鉴权、候选渠道、计费两阶段、
选路、上游协议适配、账本落库、请求快照、节假日日历都接上了。流式
（`stream: true`）返回 `501`——那一条还没实现。

要真的拿到一次 `200`，库里得先有：一个用户与钱包、一把 API Key
（`larkaxctl apikey create --user <ID 或用户名>`）、一条指向上游的渠道、
一条适用的计价规则。这四样现在都有管理入口——渠道、模型、别名与计价规则
见上文「配一条能用的渠道」，用户与 API Key 见下文。没有计价规则时请求会
失败而不是按零价放行。

`501` 是有意的占位：推理路径之外的任何请求都会立刻得到明确答复，而不是超时或
误导性的 404。这样部署验证能确认端口通、TLS 通、负载均衡转发通。

启动时会连数据库并加载 `settings`。连不上或配置对不上就拒绝启动，
`/readyz` 也会在数据库不可用时返回 `503`。

内部端点单独一个端口：它面向编排系统与指标抓取器（`/healthz`、`/readyz`、
`/metrics`），不和业务入口共用监听地址，也就不需要跟着业务入口一起暴露在
ingress 规则下。

运维命令行走另一个入口（`cmd/larkaxctl`），目前有这些：

| 子命令 | 做什么 |
|---|---|
| `channel` | 上游渠道：`create` / `list` / `update` / `enable` / `disable` |
| `model` | 模型目录：`add` / `list` |
| `alias` | 规范模型名到上游模型名的映射：`add` / `list` |
| `price` | 计价规则：`add` / `list` |
| `user` | 用户：`create` |
| `apikey` | API Key：`create` |
| `snapshot` | 请求快照的留存清理 |
| `calendar` | 节假日日历数据：`import` |
| `rotate` | 主密钥轮换 |
| `migrate` | 迁移：`up` / `down-to` / `status` / `create` |
| `reconcile` | 对账 |

每个子命令的用法见 `go run ./cmd/larkaxctl <子命令> --help`，
或直接 `go run ./cmd/larkaxctl` 看总览。对账的开关、口径与运行记录见
[reconciliation.md](reconciliation.md)，日历数据的文件格式见
[calendar-data.md](calendar-data.md)。

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
