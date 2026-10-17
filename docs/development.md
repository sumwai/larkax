# 本地开发环境

## 需要什么

- Go 1.26 或更新（`go.mod` 里写明了最低版本）
- Docker 与 `docker compose` v2.1 或更新（跑 PostgreSQL）
- `make`
- 只有开发前端才需要 Node.js 20 或更新
- **要真的打通一次推理**，还需要一个上游服务与它的凭证——本仓库不自带上游，
  示例里的上游地址都是假的

**app 不进容器。** compose 只负责依赖的生命周期，服务本身留在宿主上 `go run`——
改一行即时生效，不必每改一次就 rebuild 镜像。全栈容器化适合「只想跑起来看看」，
不适合开发。

## 一次跑通

```sh
make run
```

一条命令走完：生成 `.env`（只在不存在时）→ 起 PostgreSQL 并等它就绪 → 应用迁移 →
前台跑服务。Ctrl-C 退出服务，数据库仍在跑，用 `make down` 停它。

**`make run` 只证明服务起来了，不证明能成功推理。** 要真的打一次成功的推理，还需要
一个上游账号、一条配好的模型 / 渠道 / 别名 / 定价链路、一个用户与钱包（新开的户
余额是 0，要给它入一笔账）。后面几节分别讲这些；先知道这一点可以少走一段弯路。

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
curl -i 127.0.0.1:9090/healthz
curl -i 127.0.0.1:8080/v1/chat/completions
```

第一个返回 `200`，第二个返回 `405`——这条路径只接受 `POST`。推理接口本身
已经接上了公共端口，见下文「当前能做什么」。
另可验证数据库探针：`curl -s 127.0.0.1:9090/readyz` 应含 `"postgres":"ok"`。
**健康检查在 `:9090`，不在 `:8080`**——公共端口的 `/` 捕获一切路径并返回 `501`。

指标在同一个内部端口上，`curl -s 127.0.0.1:9090/metrics` 应含
`larkax_billing_unbilled_settlements_total 0`。没有请求时只有不带标签的
计数器会输出；带标签的几条（`settlements_total`、`capped_settlements_total`、
`capped_amount_total`、`auth_failures_total`）在第一次自增之前没有序列，
不出现是正常的。指标清单与
命名约定见 [metrics.md](metrics.md)。

**两个监听地址默认都只绑回环**（`127.0.0.1`），服务默认只在本机可达；要对外时
见下文「环境变量」。

**`:9090` 是个常被占的端口**：Clash / mihomo 的 external-controller 默认就是它，
Prometheus 也是。撞上之后服务完全起不来（不是降级），所以 `make run` 在起服务前
会先检查这两个监听地址，占用时会告诉你**是哪个**、以及**怎么换**。

换的方式是把 `LARKAX_INTERNAL_ADDR`（或 `LARKAX_HTTP_ADDR`）写进 `.env`——那样
只需改一次，不必每次都在命令行上带。主机部分照默认的 `127.0.0.1` 写，只把端口
换成一个空闲的：

```sh
LARKAX_INTERNAL_ADDR="127.0.0.1:8082"
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

## 请求快照

快照按天分区（`request_snapshots_YYYY_MM_DD`），留存期从
`settings.request_snapshot.retention_days` 读。到期是整分区 `DROP`，
所以清理必须真的被执行——它不会自己发生。

```sh
go run ./cmd/larkaxctl snapshot cleanup --dry-run   # 只列出会删哪些分区
go run ./cmd/larkaxctl snapshot cleanup             # 执行删除
```

没有可删的分区时命令会打印
`已删除：0 个（留存期 30 天，到期线 <日期>；没有到期的分区）`，而不是什么都不输出。
「没有到期的分区」与「命令没跑成」在安静的终端里长得一样，所以这条区别是刻意的；
括号里的留存期与到期线是这次清理用到的判据——只写「没有」的话，它跟「根本没查」
也分不开。

关掉 `request_snapshot.enabled` 只是**不再写入新快照**：`snapshot cleanup` 照旧按
留存期清理存量分区，命令会在标准错误上说明这次是在采集关闭的情况下跑的。留存期
因此在采集关掉时也要配成正数（它仍然决定清理删到哪里），写成 0 或负数时命令会
报错退出、一条都不删。

写入时若当天分区不存在，写入方会**当场补建**它（幂等，每实例每天只做一次）。
不补建的话行会落进兜底分区 `request_snapshots_default`，而那里的行没有到期日——
留存承诺对它不成立。**兜底分区非空即说明有写入绕过了补建，应当告警。**

### 快照的两个方向与上游证据两列

`request_snapshots` 按 `direction` 分两种行，它们回答的不是同一件事：

| 方向 | 采集点 | 回答的问题 |
|---|---|---|
| `request` | 预留之后、选路与调上游**之前** | 发出去的是什么 |
| `response` | 上游那次调用有了结局之后（流式在流读完或中断之后） | 回来的是什么 |

两份是排查时**配对读**的。请求方向的行上 `upstream_status` 与 `error_kind`
恒为 NULL——那时连会打到哪个渠道都还不知道。这个 NULL 读作「这一行不适用」，
不是「上游没回状态码」。

响应方向的行上两列有确定的含义：

- `upstream_status` 是这次调用**真的拿到过**的上游 HTTP 状态码（2xx 也填）。
  NULL 表示一个 HTTP 响应都没拿到：连不上、TLS 失败、超时、DNS 失败。
  它不是「忘了填」，也不是「上游回了 0 号」——HTTP 里没有 0 这个状态码。
- `error_kind` 是这次响应以上游失败告终时的分类，取值与
  `billing_requests.error_code` 同一套（`upstream_http_error`、
  `upstream_unusable_response`、`upstream_no_answer`、
  `upstream_stream_aborted`、`upstream_stream_truncated`）。NULL 表示没有
  上游失败可归：正常结束、客户端断开、插件阻断。

五个取值的分界线是**这次调用有没有真的产生 token**：

- `upstream_stream_aborted`：上游接受了请求、一个字都没吐就结束了。没有用量，
  这次不计费，预留被释放。
- 其余四个都表示上游那边出了事，但计不计费要看路径：非流式那三个
  （`upstream_http_error`、`upstream_unusable_response`、`upstream_no_answer`）
  是这次调用失败的原因，响应没交给调用方，不计费；流式里吐过内容之后才出的
  问题（上游在流里报错 → `upstream_unusable_response`，或者干脆把连接掐断 →
  `upstream_stream_truncated`）**已经产生了 token**，这次照常计费。

两列的取值判据与账单同源（`internal/gateway` 的 `upstreamEvidence` 与
`streamResponseEvidence`），但两张表回答的问题不同：账单的 `error_code` 答的是
「这次为什么没计费」，所以已计费的行上它是 NULL（唯一的例外见下）；快照的
`error_kind` 答的是「这次响应怎么了」，中断的流在这里都有分类。出现分歧时，
钱的问题以账单为准，「上游当时回了什么」以快照为准。

`upstream_stream_truncated` 是这条分界上的例外：那一次回答**计了费**，而
「回答只写了一半」同样值得记在账单上。它今天还只落在快照的 `error_kind` 上——
已计费的行上 `error_code` 的唯一写入点（`internal/ledger/store` 的
`markSettled`）还没接。接上之后两表会取同一个值。

`larkaxctl snapshot show` 按方向与列渲染：请求方向的行上打成
`<不适用（请求方向快照）>`，响应方向的状态码 NULL 打成 `<无上游响应>`、
错误分类 NULL 打成 `<无>`。三个文本不能共用——它们分别说的是「这一列在这个
方向上没有意义」「上游没回过状态码」「这次没有上游失败」。

### 允许表与读回

```sh
# 允许表：哪些字段放行、哪些替换成占位、每一行为什么这么定。不连库。
go run ./cmd/larkaxctl snapshot allowlist                          # 两条表面都打
go run ./cmd/larkaxctl snapshot allowlist --direction request
go run ./cmd/larkaxctl snapshot allowlist --surface decision

# 读回一条快照。--actor 与 --reason 都必填，见下。
go run ./cmd/larkaxctl snapshot show <request_id> --actor <用户 ID 或用户名> --reason <文本>

# 试脱敏：从 stdin 读一段报文，看它会变成什么骨架。不连库。
echo '{"model":"...","messages":[...]}' | \
  go run ./cmd/larkaxctl snapshot redact --direction request --surface chat
```

`--surface` 选的是**哪条表面**：`chat`（`/v1/chat/completions` 那条路）或
`decision`（`/v1/systemone`）。两条表面各有一张允许表，字段集合不同
（决策那条只留判据，清单见 [request-retention.md](request-retention.md)）。
`allowlist` 不给 `--surface` 时两条都打，`redact` 不给时按 `chat` 试——
试脱敏吃的是 chat 形状的报文，默认成 chat 才不会让人拿一份 chat 报文去撞
决策表然后以为规则写漏了。名字不认识时命令**报错退出**，不静默回退：
打出一份看着正常的允许表，比报错更坏。

`show` 是读**敏感数据**的动作，所以每一次读都会往 `snapshot_access_log`
写一行（谁、什么时候、什么理由、读了哪一条）。写日志与读快照在同一个事务里：
日志写不进去时命令整体失败，一行数据都不返回。`--actor` 与 `--reason` 都没有
默认值——「我写了 --reason」与「我忘了写」在库里必须分得开。

同一个 `request_id` 可能有多行（请求与响应各一行，重试也可能各一行），
`show` 全部按 `created_at` 升序打出来。`skeleton_truncated` 为真的那条会额外
说明它被降级成了摘要层——一份被截断的骨架与一份完整的骨架在肉眼下长得一样。

`redact` 的骨架走 stdout（可以直接接给 `jq`），方向、字节上限、被替换的路径
与那句提醒走 stderr。它回答的是「这段报文会长成什么样」，**不是**库里那条快照：
库里那条来自解析之后的请求/响应，字段集合与它不完全一样。

标准输入有 64 MiB 上限，不配开关：这条命令是拿一段**单次请求**的报文来试，
不是拿整份日志。把整份日志喂进来会超出上限并报错，要先拆开、一次喂一段。
## 用量事件的分区

`usage_events` 按月分区，粒度与维护方式都与快照不同——**分区不会自己长出来**，
所以要有一个预建任务。开发时手动跑：

```sh
go run ./cmd/larkaxctl partition ensure --months 3   # 铺满未来 3 个月（含当月）
go run ./cmd/larkaxctl partition check  --months 3   # 只读体检，有问题时非零退出
```

没有缺失的分区时 `ensure` 会打印「没有缺失的分区，本次没有建任何东西。」——
理由与 `snapshot cleanup` 那条一样，刻意区分「没东西可建」与「没跑成」。

线上由外部调度器调用，接法与两张表的分工见 [docs/partitions.md](partitions.md)。

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
| `LARKAX_HTTP_ADDR` | `127.0.0.1:8080` | 业务入口监听地址 |
| `LARKAX_INTERNAL_ADDR` | `127.0.0.1:9090` | 内部端点监听地址 |
| `LARKAX_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `LARKAX_ENV` | `development` | 设为 `production` 切换日志格式 |
| `LARKAX_REDIS_DSN` | 空 | 留空表示不启用 Redis |

两个监听地址默认只绑回环（`127.0.0.1`），服务因此默认只在本机可达。容器里端口
映射不进来、或要让别的机器直接连时，显式写宽即可，例如 `LARKAX_HTTP_ADDR=":8080"`。

必填项缺失时服务拒绝启动并一次列出全部问题，不会带着半截配置跑起来。

**直接在 shell 里跑运维命令时，先加载 `.env`。** 服务与 `larkaxctl` 都只读
环境变量，不读 `.env` 文件；`make` 的目标（`make run` / `migrate` / `test`）
已经替你加载。手敲 `go run ./cmd/larkaxctl ...` 之前先：

```sh
set -a && . ./.env && set +a
```

否则第一条命令就会报 `LARKAX_DB_DSN 未设置`。

上表是**服务**（`cmd/larkax`）的清单。运维命令（`cmd/larkaxctl`）的配置面小得多：
它只读 `LARKAX_DB_DSN`、`LARKAX_LOG_LEVEL` 与 `LARKAX_ENV`，缺 DSN 才失败。
`LARKAX_ROLE` 决定不了任何一条运维命令的成败——命令不属于任何角色——所以
`larkaxctl migrate up` 在没有配角色的机器上照样能跑。真正需要主密钥的命令只有
两个（`channel` 的写入与 `rotate`），它们自己读 `LARKAX_MASTER_KEYS`。
两个入口见 `internal/config` 的 `Load` 与 `LoadOps`。

**为什么这么少：** 环境变量必须跟着数据一起走。备份恢复或换机器时漏了一个变量，
数据就可能不可用，而这类错误往往几周后才被发现。所以只有「连上数据库之前就必须拿到」
的值才做成环境变量（DSN、主密钥、监听地址），其余全部放在数据库的 `settings` 表里。

加新环境变量前请先问：它是不是必须在连上数据库之前拿到。不是的话就写进 `settings` 表。

`settings` 表随初始迁移建好，读取与校验在 `internal/settings`：启动时读一次，
与代码里声明的键清单对照，两边对不上就拒绝启动（见「加一个配置项」）。

## 加一个配置项

两步，缺一不可：

1. 在 `internal/migrate/sql/` 里新增一条迁移，插入默认值，并写一句说明
   （`settings.description` 那一列，`settings list` 打的就是它）。
2. 在 `internal/settings/settings.go` 的 `Registry` 里加一行，声明键与类型。

说明只写在迁移里一份，`Registry` 里没有第二份——写两份的话，改了一处另一处不动，
而改错了不会有任何反馈。

只做一步会被拒绝启动，并指出是哪个键对不上。这是有意的——
否则写错键名会安静地生效为一个不存在的配置，改动看起来做了，其实没做。

**加完再确认一句：这个键现在有人读吗？** 库构件先写、接线后做是仓库里刻意的
顺序，但那个接缝没有任何一步会失败：一个没人读的键，在命令行上和一个真的生效
的键长得一模一样（都回「已写入」，库里那一行也确实变了）。所以还没有读者的键
要在 `Registry` 那一行的 `Unwired` 里写明为什么，`settings list` 会把那句话
打出来。**接上线之后把它清空**，那一行说明就跟着消失。

与之相对，装配期读得到的键（例如 `billing.default_asset`、
`quota.probe_interval_seconds`）在配错时会让服务**拒绝启动**并指出是哪个键——
那正是「这个键有人读」唯一的观测量。

默认值只在迁移里写一份。代码里再写一份的话，两处迟早不一致，
而那种不一致极难发现：改了数据库，代码里的那份还在悄悄生效。

字符串类型的值要写成带引号的 JSON（`'"CNY"'`，不是 `'CNY'`）。

改一个已有配置项的值走命令行，不用写 SQL：

```sh
go run ./cmd/larkaxctl settings list                              # 列出全部键、值、类型与说明
go run ./cmd/larkaxctl settings set --key plugin.enabled --value true --by alice
```

`set` 的值按明文给（整数写 `7`、布尔写 `true`、字符串直接写），
键名与类型在连库之前就校验掉：**Registry 里没声明的键写不进去**，
写进去一个代码不认识的键，表现是配置被静默忽略。
`--by` 必填，它落进 `settings.updated_by`。

迁移里的 `editable` 为 `false` 的键（改错会造成严重后果的那些）
不接受 `settings set` 的修改，只能由迁移写入。

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

## 配一条能接请求的链路

推理请求要落到某个上游，所以库里得先有四样东西：目录里的一条规范模型、一条渠道
（指向上游、带凭证）、把上游模型名映射到规范名的别名、一条计价规则。
`larkaxctl` 的 `model` / `channel` / `alias` / `price` 四组子命令就是干这个的。

下面四条命令按顺序跑完，就有了一条能接请求的链路。示例里的上游是假的
（`https://api.example.com/v1`），换成你自己的即可。

这些命令直接用 `go run`，所以先按上文「环境变量」加载 `.env`：
`set -a && . ./.env && set +a`。`larkaxctl` 只读环境变量，不读 `.env` 文件。

**一、把规范模型加进目录。** 规范名是选路与计费共用的键，不能带空白字符：

```sh
go run ./cmd/larkaxctl model add --canonical claude-x --display '示例模型' \
  --vendor example --context-limit 200000 --tools --vision
```

**目录里的能力与上限在选路时生效。** `--tools` / `--vision` / `--reasoning`
声明的是「这个模型能做这件事」：请求用到某项能力、而目录声明不支持时，这个
模型下的候选全部落选。两个上限同理——请求的输入加输出超过 `--context-limit`、
或输出超过 `--output-limit` 时落选。一个候选都不剩时报「N 个候选渠道都不支持
这次请求」（HTTP 502），一个上游调用都不会发出去。判据里的输入 token 数是
**只按正文估算**的那个量（与预留同一个口径，不含工具定义与图片）。

「没填」与「声明不支持」在库里不是一回事，读侧的处置正好相反：

- **两个上限留空（0）表示未知**，不参与判断。采集失败留下的空值不该让一整批
  模型用不了。
- **六项能力是 `NOT NULL DEFAULT false`**：「没勾」与「勾掉」在库里是同一个值，
  都按不支持判。所以一条已经在目录里的模型，如果这几项一直是默认值，带工具或
  带图片的请求就选不到它——加进目录时把这些开关一起填了。
- **目录里没有这条记录**时不判任何条件：采集还没跑到是最常见的开始状态。

**二、建一条渠道。** 凭证只能从标准输入或环境变量读，**不能写在命令行参数里**
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

`--type` 必须是已注册的适配器类型（当前是 `openai`、`anthropic` 与
`openai-responses`）。上面的示例用 `openai` 只是占位；上游说的是 Anthropic
协议（例如 Claude）时用 `anthropic`，不要照抄示例的 `openai`。
`--models` 是这条渠道支持的**规范模型名**（第一步建的那个），至少一个：
读取路径把 `models` 当白名单用，空数组的渠道永远选不中。
stdout 只有新渠道的 ID，其余信息走 stderr。

**`--setting`：键归适配器解释，选路层不认。** 它是渠道私有差异的归属地
（`channels.setting`，一个自由形态的 JSON 对象）：某个渠道「大体像 OpenAI
但有几处不一样」时，那几处属于这个渠道与它的协议族之间的差别，由**该渠道
`type` 指向的那个适配器**解释；选路层不读这里的任何键——选路要认的判据
（能力、上下文上限）在 `model_catalog` 那边，不从这份 jsonb 里读第二份。
因此管理面**不做键名校验**：拼错一个键不会报错，只会静默地不生效。

**当前没有任何适配器读它。** 四个适配器与选路层一次都没按 `setting` 做判断，
所以写进去的键今天对请求路径没有任何影响。这件事会在写入成功后（`channel
create` / `update` 给了 `--setting` 时）打到 stderr 上，`channel list` 也会
把带着 `setting` 的渠道单独点出来——一个配了却不生效的键，从库里和清单上
看与一个生效的配置完全一样，不说出来就只能等人去查代码。第一个键被某个
适配器实现之后，这两处提示会一起消失（判据是 `internal/channel` 里的
`SettingReadByAdapters`，由同包的用例盯着它与代码是否一致）。

`--owner` 把渠道记在某个用户名下（留空表示平台自营）。它**只写
`channels.owner_user_id`，不做权限检查**：任何存在的用户都可以被写成归属，
而这个用户是不是 `merchant` 没有一处代码会看（见下文「用户的角色」）。
归属是分账的依据——商家作用域的计价规则按它匹配，结算时
`req.OwnerUserID` 跟着渠道走进账本，而不是「他有没有资格」的判据。

**三、配别名：这家上游对同一个模型叫什么名字。**

```sh
go run ./cmd/larkaxctl alias add --canonical claude-x \
  --upstream claude-3-5-sonnet-latest
```

不带 `--channel` 的别名是平台级的，对所有渠道生效；带 `--channel <ID>` 的只对
那个渠道生效，并盖过平台级那条。同一个范围里一个规范名只能有一个上游名。

**请求时按「选中的那个渠道」翻译**，所以查不到别名时分两种，处置相反：

- **这个规范名一条别名都没配过** → 按同名发出。别名表表达的是「哪些模型的上游
  叫法与规范名不同」，一条都没有的模型不在这个集合里。一条别名都没配的部署
  （最常见的那一类）不受影响。
- **配过、但没有一条覆盖选中的渠道** → 这次调用失败，返回 `500`。别名是逐渠道
  配的，某个渠道漏了的时候静默按同名发出去，会表现为「上游说这个模型不存在」，
  把排查方向指向上游。补一条渠道级别名、或者加一条平台级别名即可。

后一种在升级时是**可见的行为变化**：升级前别名在请求路径上完全不生效，配了也
白配；升级后它开始生效，于是「只给部分渠道配过别名」的规范名会在没配的那些
渠道上失败。

`larkaxctl alias resolve --canonical <名>` 回答的正是「这个规范名在某个渠道上
实际会发出去什么名字」，它走的就是选路那同一条查询（见 `cmd/larkaxctl/alias.go`
里 `runAliasResolve` 的注释）。不带 `--channel` 时只看平台级那条，所以「配了
渠道级别名、没有平台级别名」时它会报错、退出码走「配置缺失」那一档。

**四、配一条计价规则。** 没有生效的规则时请求会失败，而不是按零价放行：

```sh
go run ./cmd/larkaxctl price add --scope platform --name example-price \
  --expr '{"prompt": 0.0000015, "completion": 0.0000075,
           "cache_read": 0.0000002, "cache_write": 0.0000019,
           "reasoning": 0.0000090}'
```

计价项一共五个：`prompt`、`completion`、`cache_read`、`cache_write`、`reasoning`。
**上游报了哪一项，规则里就必须有哪一项的单价**——少一项不是少收，是这次请求
**整个失败**（500），因为上游报了我们没定价的用量，宁可报错也不要静默少收：

```
结算失败: 用量里有规则未定价的计量项: "cache_read"（规则里没写这个单价；要免费就显式写 0）
```

会报缓存的上游很常见（Anthropic 的 `cache_read_input_tokens` /
`cache_creation_input_tokens`，OpenAI 的 `prompt_tokens_details.cached_tokens`），
所以**新规则把这五项都写上**、用不到的填 `0`，比漏掉再被 500 拦住省事。

`--models` 留空表示适用于全部模型（兜底规则）。表达式在写入前会编译一次，
编译不过的命令直接失败。作用域决定哪两栏 id 有效：`platform` 两栏都留空、
`merchant` 必须给 `--owner`、`channel` 必须给 `--channel`。

几条规则同时适用时按裁决次序取第一条：**作用域**（`channel` > `merchant` >
`platform`）→ `priority` 大的优先 → **模型名单非空的优先于兜底** → `id`
大的优先。于是同作用域、同优先级下，后加一条兜底规则**不会**盖掉已有的
模型专属规则；要让一条兜底赢过专属的，把它的 `--priority` 调高——改价因此
总是显式的。

**查看现状**：

```sh
go run ./cmd/larkaxctl model list
go run ./cmd/larkaxctl channel list
go run ./cmd/larkaxctl alias list
go run ./cmd/larkaxctl price list
```

`channel list` **不打印凭证密文**，只打印它挂在哪些主密钥下（那不需要任何
密钥材料）。默认只列**还在用**的渠道，`channel list --all` 把已删除的也列出来，
多一栏「已删」记着删除时刻。`price list` 按裁决次序排列，排第一的那条就是会被
选中的那条。

**删一条渠道是软删**：`channel delete --id <渠道 ID>` 把 `channels.deleted_at`
置上时刻，并把状态改成 `disabled`。

```sh
go run ./cmd/larkaxctl channel delete --id 7
```

行本身留着——`billing_requests` 与 `usage_events` 仍按 `channel_id` 引用它，
真删会让对账与快照追溯不到历史。删掉之后：

- 那个渠道不再进候选集，也不再出现在 `channel list` 的默认输出里（`--all` 才看得见）；
- `channel update` / `enable` / `disable` 对它会**明确报错**，而不是改一行你看不见的数据；
- 凭证密文与从属的行都不动，所以 `rotate` 与健康度统计都会跳过它。

删除是幂等的：重复删同一条不是错误，第二次打印「已经是删除状态」。

**退出码**：`0` 成功、`1` 其他失败、`2` 命令行参数错、`3` 连不上数据库、
`4` 约束冲突（唯一键、外键、要改或要查的那一行不存在；`wallet credit` 的
同一个幂等键已经对应另一笔入账也归这一档）。脚本按这个分派处置。
「要查的那一行」指解析一个标识时它不在库里（例如 `--user` 给了一个不存在的
用户名），它的下一步与撞唯一键一样：先去看数据里现在是什么。

需要主密钥（`LARKAX_MASTER_KEYS`）的命令只有两个：渠道的**写入**
（`channel create` / `update`）与 `rotate`。凭证是信封加密的，只有
`credential.Sealer` 能产出可解开的密文，直接往库里写一行 `bytea` 补不上。
它们读的是这个环境变量本身，与 `LARKAX_ROLE` 无关——运维命令不属于任何角色，
把命令的成败绑到角色上，会让 `LARKAX_ROLE=worker` 时明明设好了主密钥却报
「没有读到主密钥」。其余命令不碰凭证，没有主密钥也能跑，其中
`channel list` / `enable` / `disable` / `delete` 连密钥材料都不需要。

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

### 用户的角色（今天只是记下来）

三个角色都是用户，`merchant` 与 `admin` 落成 `users.is_merchant` /
`users.is_admin` 两个布尔标记（`admin` 同时打上两个）。**全仓没有生产代码
读这两个标记**，所以：

- `--role merchant` **不会**让这个用户多出什么权限，`--role admin` 也**不会**
  让他能进管理接口。管理面还不存在。
- `channel create --owner <用户>` 不检查那个人是不是 `merchant`：归属只决定
  分账记到谁的账上，不决定「有没有资格」。
- 角色改不了。`user create` 只建新用户，重名直接报错，没有「改角色」的命令。

这两个标记是给管理面的鉴权**预留**的（#158）。在那条路接上之前，把 `--role`
当成一条备注，而不是一条访问控制。

### 开户：新用户需要一行钱包

`user create` 只写 `users` 表，不建钱包行；而每一次推理请求在预留那一步都要
锁住用户的钱包，锁不到就报「没有对应的钱包」。所以新用户在打请求之前要开一次户：

```sh
go run ./cmd/larkaxctl wallet open --user alice
```

它只建 `user_wallets` 那一行，余额为 0，**不写任何分录**：新开的户在账本上
没有分录，这个用户在 user 科目上的分录之和为 0，与快照里的 0 自洽。反过来
不成立（充过又花光的老用户余额也是 0，却有分录），所以这是一句单向的说法。
带初始金额意味着这次开户同时是一次充值，而那是另一条路（下面的
「入账」），不属于开户。

命令是幂等的：钱包已经存在时什么都不做、照样退出 0，输出里写明「已有…未改动」，
所以可以放进重复执行的初始化脚本。资产默认取 settings 的
`billing.default_asset`（迁移里的默认值是 `CNY`），`--asset` 可以显式指定；
当前只有 `CNY` 一种受支持的资产。用户不存在时报「找不到用户」并退出 4。

### 入账：给钱包写一笔

新开的户余额是 0，所以第一次调用会在预留那一步因余额不足被拒绝（`429` +
`insufficient_quota`，见 [limits.md](limits.md) 的「钱包余额不足」一节）。给它入一笔：

```sh
go run ./cmd/larkaxctl wallet credit --user alice --amount 100 --order ORD-20261017-1
```

它走账本：写一组 `topup` 分录（借 `user:<ID>`、贷 `payment_gateway:<来源>`），
并在同一个事务里更新 `user_wallets.balance`。余额因此始终等于分录之和，
`ledger balance` 与 `reconcile` 都不会报出差额。

| 旗标 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `--user` | 是 | — | 用户 ID 或用户名 |
| `--amount` | 是 | — | 正数，最多 8 位小数（末尾的 0 不算位数） |
| `--order` | 是 | — | 幂等键，没有默认值 |
| `--asset` | 否 | settings 的 `billing.default_asset`（`CNY`） | 当前只有 `CNY` 一种 |
| `--gateway` | 否 | `manual` | 钱的来源，进 `payment_gateway:<来源>` 科目；来源名归一成小写 |

`--order` 被确定性地映射成这次交易的 `txn_id`，所以同一条命令重跑两次只入账
一次：第二次输出「已经入过账，本次未改动」并退出 0。同一个键配不同的金额、
用户或通道不是重放而是冲突——退出 4，一笔都不写。

这条命令是给运营手工记账用的：钱包必须已经存在（先 `wallet open`），
`--gateway manual` 表示线下到账（转账、现金），那个科目的余额与线下流水核对。
它不做退款、不做提现，也不接支付通道的回调。

拿到的 key 打一次推理请求：

```sh
curl -i 127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<模型名>","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}'
```

`max_tokens` 是**必填**的，虽然 OpenAI 协议里它可选：预留必须有上界，而预留上界
由它算出来（见 `billing.ErrNoUpperBound`）。漏了它会在请求出网关之前就回 400
`max_tokens is required`，错误里不会提到这是本地网关的要求。写 `stream: true`
则走 SSE，其余字段照旧。

推理接口接到公共端口上的进度见下文「当前能做什么」。

### 看余额与花费

余额用只读的账本命令查，值从分录现算：

```sh
go run ./cmd/larkaxctl ledger balance --account user:1
go run ./cmd/larkaxctl ledger balance --all
```

每次请求的金额落在 `billing_requests` 表：`reserved_amount` 是预留，
`final_amount` 是实际结算。按请求查要直接连库，SQL 示例见
[metrics.md](metrics.md) 的「怎么读」。**目前没有一条按请求列出花费的命令。**

## 检查这条链路通不通

上面两节走完之后，「通不通」由一条命令回答：

```sh
go run ./cmd/larkaxctl doctor
```

它按顺序查六项，每一项都打出「检查了什么、结论是什么」：

| # | 查什么 |
|---|---|
| 1 | 数据库连得上吗、迁移跑到哪一版 |
| 2 | 有没有至少一条 enabled 且 `models` 非空的渠道 |
| 3 | 渠道 `models` 里出现的每个名字都在 `model_catalog` 里吗 |
| 4 | 配过别名的规范名有没有渠道接得住；可服务的规范名在每个服务它的渠道上能不能解析出上游名 |
| 5 | 可服务的规范名在每个服务它的渠道上都有生效的计价规则吗 |
| 6 | 有用户吗、每个用户都有钱包行吗、有没有一个钱包余额大于 0 |

第 3、4、5 项回答的是同一个字符串在两个数组里对不上的问题。渠道的 `--models`
与目录里的规范名差一个字符、或者模型建了却没加进任何渠道的白名单时，调用回
`503 no available upstream for the requested model`——那句话不会说是哪一个
名字不对，而 doctor 会点名是哪个渠道、哪个名字。第 6 项把「用户没有钱包行」
这类缺口在第一次调用之前指出来，而不是让人从 `500` 往回推。

**第 6 项在「入账」那一步走完之前不通过。** 它要求有一个钱包余额大于 0，而新开的
户余额是 0，所以只开户不入账时 doctor 会以「通过 5 项、不通过 1 项」收尾——那是
预期结果，不是配置漏了。按上文「入账」写一笔，再跑一次这一项就绿了。

**前六项只读**：不写任何一行、不建任何东西、不发网络请求。

### 真打一次上游

要验证到上游那一端，加 `--live`：它往公共入口发一次非流式推理请求，把状态码、
耗时与用量打出来。它会花钱，所以默认不跑。

```sh
go run ./cmd/larkaxctl doctor --live --model claude-x --credential-env LARKAX_API_KEY
cat key.txt | go run ./cmd/larkaxctl doctor --live --model claude-x --credential-stdin
```

| 旗标 | 含义 |
|---|---|
| `--model <规范名>` | 这次请求用的规范模型名，`--live` 时必填 |
| `--endpoint chat\|messages\|responses` | 打哪个入口，默认 `chat`。`chat` 是 `POST /v1/chat/completions`（`Authorization: Bearer`），`messages` 是 `POST /v1/messages`（`x-api-key`），`responses` 是 `POST /v1/responses`（`Authorization: Bearer`） |
| `--base-url <URL>` | 网关公共入口的地址，默认 `http://127.0.0.1:8080` |
| `--credential-stdin` / `--credential-env <变量名>` | 读 API Key 的来源，二者只能给一个 |

这里读的是打网关用的 **API Key**（`apikey create` 发的那把），不是渠道凭证。
与 `channel create` 同一条约定：**凭证不能写在命令行参数里**——参数会出现在
`ps` 的输出与 shell 历史里。

### 退出码

`0` 六项都通过（给了 `--live` 时那一次也成功）、`1` 有问题或某一项没跑成、
`3` 连不上数据库。

**「没跑成」与「通过」是两件事**，输出里分得开：没有可检查的对象时（例如一条
enabled 渠道都没有，第 3 项就没有名字可查）那一项报「没跑成」而不是「通过」，
退出码也非 0。两者在安静的终端里长得一样，是这个项目反复踩的坑
（见 [LESSONS.md](../LESSONS.md) 第 16 条）。

## 给一把 key 设额度

两列额度可以在创建时给，也可以事后调：

```sh
go run ./cmd/larkaxctl apikey create --user alice --rate-limit-rpm 60 --quota-limit 12.34
go run ./cmd/larkaxctl apikey list                       # id、前缀、用户、备注、状态、每分钟、额度、创建、过期、最近使用
go run ./cmd/larkaxctl apikey update --id 3 --rate-limit-rpm 120
go run ./cmd/larkaxctl apikey update --id 3 --clear-rate-limit-rpm
```

| 旗标 | 含义 |
|---|---|
| `--rate-limit-rpm <整数>` | 每分钟请求额度 |
| `--quota-limit <金额>` | 这把 key 累计可花费的金额上限，单位是该用户钱包的资产品种 |
| `--clear-rate-limit-rpm` / `--clear-quota-limit` | 写回 NULL（继承用户默认） |

**`0` 与不填不是一回事**：`0` 是一个都不放行，NULL 是继承用户默认
（`users` 表上目前没有这样一个默认值，所以等价于不限）。
`update` 不给某个旗标表示「这一项不改」，与清空也不同。
负数一律拒绝。

`--quota-limit` 这一列目前只有写入路径，**没有读侧**——它记下了额度，
但还没有任何地方拿它去拦请求。

## 当前能做什么

**推理已经接到公共端口上，流式与非流式都走通了**：

| 端口 | 端点 | 现在返回 |
|---|---|---|
| `:8080` | `POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses` | 推理；`stream: true` 时是 SSE 流；没有有效 API Key 时 `401` |
| `:8080` | `POST /v1/systemone` | 决策调用，见下节；**从不流式**，带了 `stream: true` 回 `400` |
| `:8080` | 其余任意路径 | `501`，明确的「尚未实现」JSON |
| `:9090` | `/healthz` | `200`，进程存活 |
| `:9090` | `/readyz` | `200`，含 `postgres` 探针 |
| `:9090` | `/metrics` | Prometheus 文本格式的指标，清单见 [metrics.md](metrics.md) |

装配在 `internal/assembly`，由 `cmd/larkax` 调用：鉴权、候选渠道、计费两阶段、
选路、上游协议适配、账本落库、请求快照、节假日日历都接上了。

两条路各有一套按协议区分的渲染器（`internal/api` 的 `serveOnce` 与 `serveStream`）：
调用协议与上游协议可以不同，转换在 `internal/relay` 那一层做，用量口径的换算
也在那里——Anthropic 的 `input_tokens` 不含缓存而 OpenAI 的 `prompt_tokens` 含，
互转时这个差要补上或拆掉，否则**账单金额会错而响应形状看着完全正常**。
决策那条路（`/v1/systemone`）不走 `internal/relay`，见下节。

**`openai-responses` 上游只转发正文、引用标注与工具调用**，不转发内置工具及其
产物：`web_search_call`、`file_search_call`、`code_interpreter_call`、
`image_generation_call` 一类的事件，以及它们的进度事件。流式下上游发来这些时
本适配器记一条 warn 并跳过——它们带内容，只是没有转发通道，记成「已知且不必
转发」会让这条链路未来接上内置工具时没人知道旧事件一直在被丢。非流式那条路
由 relaykit 按输出项类型过滤，内置工具被**静默**跳过，不记日志。同样按这个
方式处理的是 `refusal.*` 与 `audio.*`：**拒答文案与音频都不进正文**（拒答本身
就是一次回答，是否计费是另一件事，与这里的转发无关）。

引用标注（`response.output_text.annotation.added`）是**转发**的：它是正文的脚注
（哪一段对应哪个来源），不参与分词与计费。流式下它是独立的一帧
（`response.output_text.annotation.added`），非流式下挂在
`output[].content[].annotations`。

要真的拿到一次 `200`，库里得先有：一个用户与钱包、一把 API Key
（`larkaxctl apikey create --user <ID 或用户名>`）、一条指向上游的渠道、
一条适用的计价规则。这四样现在都有管理入口——模型、渠道、别名与计价规则
见上文「配一条能接请求的链路」，用户与 API Key 见下文。没有计价规则时请求会
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
| `channel` | 上游渠道：`create` / `list`（`--all` 含已删）/ `update` / `enable` / `disable` / `delete`（软删） |
| `model` | 模型目录：`add` / `list` / `show` / `update` / `delete` |
| `alias` | 规范模型名到上游模型名的映射：`add` / `list` / `update` / `delete` / `resolve` |
| `price` | 计价规则：`add` / `list` / `update` / `explain` |
| `user` | 用户：`create` |
| `wallet` | 用户钱包：`open`（余额 0）/ `credit`（手工入账，走账本写分录） |
| `apikey` | API Key：`create` / `update` / `list` |
| `ledger` | 只读账本：`balance` / `verify` |
| `settings` | 运行期配置：`list` / `set` |
| `plugin` | 插件：`list` / `publish` / `enable` / `disable` |
| `partition` | `usage_events` 月分区：`ensure` / `check` |
| `doctor` | 逐项检查这条链路通不通（`--live` 才真打上游） |
| `snapshot` | 请求快照：`cleanup` / `allowlist` / `show` / `redact` |
| `idempotency` | 幂等记录的留存清理：`cleanup` / `cleanup --dry-run` |
| `calendar` | 节假日日历数据：`import` |
| `rotate` | 主密钥轮换 |
| `migrate` | 迁移：`up` / `down` / `down-to` / `up-to` / `status` / `version` / `create` |
| `reconcile` | 对账 |
| `version` | 打印版本号 |

每个子命令的用法见 `go run ./cmd/larkaxctl <子命令> --help`，
或直接 `go run ./cmd/larkaxctl` 看总览。对账的开关、口径与运行记录见
[reconciliation.md](reconciliation.md)，日历数据的文件格式见
[calendar-data.md](calendar-data.md)。

两个端点都认请求头 `Idempotency-Key`：带上它时同一个键重复发送只执行一次，
第一次的结果被原样重放。用法、三条 `409` 的码与留存规则见
[idempotency.md](idempotency.md)。

## 决策接口（`/v1/systemone`）

`POST /v1/systemone` 是第三条公开路径。它不做对话，而是拿一段**状态**（一段
自由文本，比如一张工单）与一组**带类型的判定问题**，回一组答案。与另外两条
路径一样：协议由路径决定，计费走同一条两阶段流水线，上游由选路层按渠道挑。

请求体是一个 JSON 对象：

```json
{
  "model": "decision-model",
  "state": "工单: 用户说付款后没到账",
  "questions": {
    "q_noul":   {"type": "noul",   "instructions": "这是账单问题吗"},
    "q_choice": {"type": "choice", "instructions": "路由到哪个团队",
                 "criteria": {"billing": "付款相关", "tech": "技术故障"}},
    "q_score":  {"type": "score",  "instructions": "紧急程度",
                 "criteria": ["low", "medium", "high"]}
  }
}
```

- `questions` 是**按 id 索引的对象**，不是数组。数组会被上游拒。
- 三种类型的 `criteria` 形状不同：`noul`（是/否）不带；`choice` 是
  `{选项 id: 说明}`；`score` 是档位名的数组。
- `state` 空不空由上游判断，入口不拦。
- **未定义的字段会被忽略。** 请求里多写的键（例如 `temperature`）不会报错，
  也不会转发给上游——它们只是没生效。上游将来加的参数也一样：这里认的是
  本文列出的那几个键，多写一个不会让你收到 400，但也不会有任何作用。

响应体：

```json
{
  "model": "decision-model",
  "answers": {
    "q_noul":   {"type": "noul",   "noul": 0.75},
    "q_choice": {"type": "choice", "choice": "billing", "confidence": 1,
                 "probabilities": {"billing": 1, "tech": 0}},
    "q_score":  {"type": "score",  "score": 1.8, "confidence": 0.7,
                 "legend": {"0": "low", "1": "medium", "2": "high"},
                 "probabilities": {"0": 0, "1": 0.2, "2": 0.8}}
  },
  "usage": {"input_tokens": 387, "output_tokens": 72}
}
```

`model` 回的是**调用方要的那个名字**，不是上游回报的那个。

`score` 是**数值**而不是档位下标：上面这个 1.8 落在下标 1 与 2 之间，要
连着 `legend` 读——`legend` 是「下标 → 标签」的映射（`"2"` → `"high"`），
它原样透传。`probabilities` 的键随类型变：`choice` 是选项名，`score` 是
档位下标的字符串形式。零值也照常出现（`"score": 0` 是合法答案，最低那一档），
不会因为它是零就被省掉。

### 配一条能接决策请求的渠道

与推理那条链路的四样东西相同，只是渠道的 `--type` 用 `systemone`：

```sh
cat secret.txt | go run ./cmd/larkaxctl channel create \
  --name my-decision-upstream --type systemone --base-url https://api.example.com/v1 \
  --models decision-model --credential-stdin
```

### 计费

`usage.input_tokens` 与 `usage.output_tokens` 按 prompt 与 completion 两个槽位
计价，与另外两条路径同一套规则。规则里**两个槽位都要有价**：少写一个时整条
请求失败，而不是按写了的那个少收。

预留的上界来自 `internal/systemone` 的两个常量（输入 64K、输出 4096），不是
按正文估出来的数——这条表面的请求里没有 `max_tokens` 一类的参数，而按正文
估出来的是**下界**，拿它去预留会让结算撞上界被静默截断。

### 这条表面没有的东西

- **从不流式。** 上游收到 `stream: true` 会当没看见、照常回普通 JSON，所以
  入口**拒掉**带 `stream: true` 的请求（`400`），而不是忽略它——忽略的结果是
  调用方按 SSE 去解析一份 JSON，错误出现在他那一侧而且离原因很远。
- **没有幂等。** `Idempotency-Key` 不生效，带上它的请求照常执行。
- **没有请求快照。** 见 [request-retention.md](request-retention.md)。
- **没有插件挂载点。** 五个 hook 都不在这条路径上。

## 常用检查

```sh
gofmt -l .                          # 列出未格式化的文件
go vet ./...
golangci-lint run ./...             # 跨文件与语义检查，配置见 .golangci.yml
go test ./...
go test -race ./...
go test -cover ./...
```

`scripts/` 下另有一组门禁脚本，多数还带一份自测。**这份清单的唯一来源是
[AGENTS.md](../AGENTS.md) 的「Lint」一节**——有哪些门、各自挡什么、判据与白名单
在哪、怎么改，都写在那里，这里不重复列一遍。

本地跑它们时的几个前提：

- **`check-deadcode.sh` 要先装 `deadcode`**，版本与脚本里的 `PINNED_TOOLS_VERSION`
  一致；不一致时脚本会打印该装哪一版，然后拒绝运行。
- **集成测试要数据库**：`LARKAX_DB_DSN` 没设时那些用例整包 `t.Skip`，
  而「跳过」在输出里和「通过」长得一样。起库见上文「一次跑通」。
- **`check-mutation.sh` 最慢**：它串行跑（固定 `workers=1`），只在改动计费、
  账本、金额时跑，见下文「变异测试」。

CI 跑的是同一批门（变异测试只跑它的自测），本地先跑一遍是为了少一轮来回。

### 每个包一个测试库

集成测试**不共用** `LARKAX_DB_DSN` 指向的那个库。每个连库的测试包在自己的
`TestMain` 里按包路径建一个库（`larkax_test_<包路径>`），跑完删掉；
`LARKAX_DB_DSN` 里的库名只用来推出主机、端口、账号与维护库。

```go
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }
```

`go test ./...` 默认并行跑包，共用一个库时「A 包的夹具被 B 包看见」是一条真实
存在的路，而它只在并行窗口里现形（见 [LESSONS.md](../LESSONS.md) 第 26 条）。
助手在 [`internal/testdb`](../internal/testdb/testdb.go)：库名怎么派生、
什么时候删、怎么保证只删自己那一个库，都写在那里。

**因此跑集成测试的角色需要 `CREATEDB`。** 缺权限时报错里会给出授权语句，
照做一次即可：

```sh
psql "$LARKAX_DB_DSN" -c 'ALTER ROLE <角色> CREATEDB;'
```

compose 起的库与 CI 里的 `postgres` 都是超级用户，默认就有这个权限；
用别的角色时补上面这一条。

**没设 `LARKAX_DB_DSN` 时集成测试照旧跳过；设了却连不上（或建不了库）时是失败，
不是跳过。** 这两种状态在输出里原本长得一样，而它们的处置完全不同：前者是
「本机没起库」，后者是「你以为在跑集成测试，其实一个都没跑」。

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
