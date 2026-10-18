# 本地开发环境

本文件很长，按下面的目录找。想看「这个仓库有哪些文档」而不是「这一份讲了什么」，
去 [docs/README.md](README.md) 的索引。

- [需要什么](#需要什么)
- [一次跑通](#一次跑通)
- [请求快照](#请求快照)
- [用量事件的分区](#用量事件的分区)
- [分时计价的日历数据](#分时计价的日历数据)
- [环境变量](#环境变量)
- [加一个配置项](#加一个配置项)
- [主密钥](#主密钥)
- [建表](#建表)
- [配一条能接请求的链路](#配一条能接请求的链路)
- [造用户、造 key、打一次请求](#造用户造-key打一次请求)
- [检查这条链路通不通](#检查这条链路通不通)
- [给一把 key 设额度](#给一把-key-设额度)
- [当前能做什么](#当前能做什么)
- [决策接口（`/v1/systemone`）](#决策接口v1systemone)
- [常用检查](#常用检查)
- [CI 跑什么](#ci-跑什么)
- [变异测试](#变异测试)

## 需要什么

- Go 1.26 或更新（`go.mod` 里写明了最低版本）。本机装的是更低的版本也不必先
  升级：进到模块目录里构建时，`go` 会自动拉取 `go.mod` 要的那一版。`go version`
  在模块外打的是本机那一版，`go env GOTOOLCHAIN` 说明允不允许自动拉取。
- Docker 与 `docker compose` v2.1 或更新（跑 PostgreSQL）
- `make`
- `openssl`——`make` 用它生成主密钥（见「主密钥」一节）。**没有它时生成的 `.env`
  里主密钥是空的**，服务会以「主密钥未设置」拒绝启动。
- 开发前端（`web/`，尚未落地）才需要 Node.js 20 或更新
- **要真的打通一次推理**，还需要一个上游服务与它的凭证——本仓库不自带上游，
  示例里的上游地址都是假的

三个端口要在本机上有空位：PostgreSQL 的 `5432`、公共入口的 `8080`、内部端点的
`9090`。三个都能换——`make run PG_PORT=55432`，两个监听地址见「一次跑通」里的
`LARKAX_HTTP_ADDR` / `LARKAX_INTERNAL_ADDR`。**下文示例里的端口写的是默认值**，
换过的话那些示例要跟着换，否则连不上——而「连不上」与「服务没起来」在终端里
长得一样。

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

验证（这里用的是默认端口；换过端口的话，下面几行的端口也要换）：

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
| `make init` | 只生成或同步 `.env`，不碰容器 |
| `make check-ports` | 只查那两个监听地址有没有被占（`make run` 起服务之前会跑它） |
| `make help` | 把常用的那几条打出来（不含本表里的 `init` 与 `check-ports`） |

`make migrate` 与 `make test` 都会先跑 `make init`——它们都要那份 DSN。

`make test` 会先起依赖并加载 `.env`——集成测试拿不到 `LARKAX_DB_DSN` 时会**跳过**，
而「跳过」在输出里和「通过」长得一样。

`.env.example` 可以直接被 shell 读取（引号、`export` 前缀、行尾注释都认），
所以 `make` 用 `set -a && . ./.env && set +a` 加载它。**服务与 `larkaxctl` 自己
也读 `.env`**（见下文「环境变量」），所以 `go run` 之前不必再手工导出一遍；
用 direnv 或别的工具加载也不会打架——环境变量优先。

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
| `request` | **选路之后**（非流式在选路返回后、流式在那条流建起来之后） | 发出去的是什么 |
| `response` | 上游那次调用有了结局之后（流式在流读完或中断之后） | 回来的是什么 |

两份是排查时**配对读**的。请求方向的行上 `upstream_status` 与 `error_kind`
恒为 NULL——**这一路收不到那两列**（它记的是发出去的那份请求，不回答这次调用的
结局），不是「上游没回状态码」。这个 NULL 读作「这一行不适用」。

请求方向的采集点原先在「调上游之前」，2026-10 挪到了「选路之后」：它记的是
**真正发出去的那份**请求——别名上的上游名与参数覆盖都要选路之后才落上去。
由此两件事要留意：

- `model` 那一格记的是**上游名**，而决策表面（骨架里 `surface=decision` 那类行）
  的同名字段记的是**规范名**。两者不要混着 join。
- 这一行跟着那次上游调用一起晚落：上游挂住时它也不会先出现。

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

`larkaxctl snapshot get` 按方向与列渲染：请求方向的行上打成
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
go run ./cmd/larkaxctl snapshot get <request_id> --actor <用户 ID 或用户名> --reason <文本>

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

`get` 是读**敏感数据**的动作，所以每一次读都会往 `snapshot_access_log`
写一行（谁、什么时候、什么理由、读了哪一条）。写日志与读快照在同一个事务里：
日志写不进去时命令整体失败，一行数据都不返回。`--actor` 与 `--reason` 都没有
默认值——「我写了 --reason」与「我忘了写」在库里必须分得开。

同一个 `request_id` 可能有多行（请求与响应各一行，重试也可能各一行），
`get` 全部按 `created_at` 升序打出来。`skeleton_truncated` 为真的那条会额外
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
go run ./cmd/larkaxctl price calendar import ./cn-2031.json
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
| `LARKAX_LOG_LEVEL` | `info` | `debug` / `info` / `warn`（`warning` 也认）/ `error` |
| `LARKAX_ENV` | `development` | 只有 `production` 特殊：它把日志格式切成 JSON。**其余任何取值都等同 `development`**，写成 `prod` 既不会报错也不会切过去 |
| `LARKAX_REDIS_DSN` | 空 | 限流的共享计数（`redis://` / `rediss://` / `unix://`）。留空时计数在进程内，单实例正确、多实例下额度按实例数放大 |

两个监听地址默认只绑回环（`127.0.0.1`），服务因此默认只在本机可达。容器里端口
映射不进来、或要让别的机器直接连时，显式写宽即可，例如 `LARKAX_HTTP_ADDR=":8080"`。

必填项缺失时服务拒绝启动并一次列出全部问题，不会带着半截配置跑起来。

**`.env` 由程序自己读，不必先导出成环境变量。** 服务（`cmd/larkax`）与
`larkaxctl` 在读取配置之前都会先找一份 `.env`：从当前工作目录往上找，
**找到第一个就用，不越过 git 仓库根**；整条祖先链上没有仓库时只看当前目录。
所以 `go run ./cmd/larkaxctl migrate up` 在仓库根或仓库里任何一个子目录下
都能直接跑，不必记得先 `set -a && . ./.env && set +a`。

**系统环境变量优先**：`.env` 只填补**尚未设置**的变量，绝不覆盖已经有的。
生产容器把配置注入成环境变量，覆盖它就等于生产连上开发库。

文件不存在时静默跳过——环境变量仍然是一条完整的配置路径，容器里就不需要
`.env`。要指定另一份文件时用全局选项 `--env-file`（写在子命令前后都认）：

```sh
go run ./cmd/larkaxctl --env-file .env.staging wallet create --user alice
```

指明的文件不存在会报错（退出码 2），不会安静地用自动找到的那一份。

`.env` 的语法与 shell 一致（引号、`export` 前缀、行尾注释都认），
`.env.example` 可以直接被 shell 读取，所以 `make` 仍然用
`set -a && . ./.env && set +a` 加载它。两套机制并存不会打架：先导出的
环境变量优先，程序自己读的那一份只填补没导出的键。

上表是**服务**（`cmd/larkax`）的清单。运维命令（`cmd/larkaxctl`）的配置面小得多：
它只读 `LARKAX_DB_DSN`、`LARKAX_LOG_LEVEL` 与 `LARKAX_ENV`，缺 DSN 才失败。
`LARKAX_ROLE` 决定不了任何一条运维命令的成败——命令不属于任何角色——所以
`larkaxctl migrate up` 在没有配角色的机器上照样能跑。真正需要主密钥的命令只有
两个（`channel` 的写入与 `secret rotate`），它们自己读 `LARKAX_MASTER_KEYS`。
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

两处 `<MASTER_KEY>` 是**两把不同的钥匙**，各跑一次 `openssl rand -base64 32`。
两处写成同一个值，下面「轮换」与「丢一把不至于毁掉存量数据」这两条理由就当场
失效了。示例里两处拼写相同，是因为 `make` 会把整段替换成一把生成的钥匙。

`make` 写进去的是**一把不带标识的裸 base64**，标识取默认值 `default`；手工填时
按上面这个形状写。

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
go run ./cmd/larkaxctl secret rotate            # 干跑：列出有多少行要改，不写库
go run ./cmd/larkaxctl secret rotate --apply    # 轮换：可中断、可重跑
go run ./cmd/larkaxctl secret rotate --verify   # 每一行是否都含新主密钥
```

顺序是「新钥匙放进首位 → 轮换 → `--verify` 退出码为 0 → 才把旧钥匙去掉」。
旧钥匙去掉得太早，那些还没轮换到的行就再也解不开了。完整说明见
`larkaxctl secret rotate -h`。

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

文中 `<...>` 是**占位符**，尖括号本身不要原样粘进 shell：出现在参数位上时
（`--upstream <上游模型 id>`）它是安全的，出现在赋值前时
（`LARKAX_CHANNEL_CREDENTIAL=<凭证>`）shell 会把尖括号当成重定向，那一行不会
设置任何环境变量。手工填值时把整段用单引号括起来。

**先问你的上游它有哪些模型 id。** 渠道的 `--models` 与别名的 `--upstream` 填的是
**上游自己的 id**，不是它宣传页上的名字——这两者经常不一样：

```sh
curl -s https://api.example.com/v1/models -H "Authorization: Bearer $UPSTREAM_KEY"
```

猜一个名字的代价不是当场报错：`doctor` 照样全绿（它不校验上游侧的名字），第一次
真实调用才回 `503 no available upstream for the requested model`，而那句话**不点名
是哪个名字不对**（见「检查这条链路通不通」）。

这些命令直接用 `go run` 即可：`larkaxctl` 自己会读 `.env`（见上文
「环境变量」），不必先手工导出。

**动词与选项名是全仓统一的**：建一条记录是 `create`、读一条是 `get`、改属性是
`update`、删是 `delete`、启停是 `enable` / `disable`（`price enable <规则 ID>`，
不是在 `update` 上给 `--enable`）；模型规范名一律 `--model`、归属用户一律
`--user`。改名之前的旧写法**仍然可用**（`model add`、`wallet open`、
`--canonical`、`--owner`、`price update --enable`、`wallet credit --order`），
已经在跑的脚本不必改；它们写在每条子命令 `--help` 的末尾。

同一批还搬了几条命令的位置：对账 `reconcile` → `ledger reconcile`、主密钥轮换
`rotate` → `secret rotate`、幂等记录清理 `idempotency cleanup` →
`maintenance idempotency`、日历导入 `calendar import` → `price calendar import`。
旧路径同样仍然可用，各条命令的 `--help` 里写着（见
[docs/cli-reference.md](cli-reference.md)）。
下面用的都是新写法。

最常用的几个选项还各有一个**单字母简写**：`-u`（`--user`）、`-m`（`--model`）、
`-o`（`--format`）、`-v`（`--verbose`）、`-q`（`--quiet`）、`-V`（`--version`）。
就这六个，其余参数一律要写全（`--channel`、`--rate-limit-rpm` 这类限额与作用域
写短了容易看错）。简写与旧名一样列在 `--help` 末尾，用法行里只有规范名；完整
约定见 [docs/cli-reference.md](cli-reference.md)。

**`-v` 不是版本号**：打版本是 `-V` 或 `--version`。

**一、把规范模型加进目录。** 规范名是选路与计费共用的键，不能带空白字符：

```sh
go run ./cmd/larkaxctl model create --model claude-x --display '示例模型' \
  --vendor example --context-limit 200000 --tools --vision
```

**目录里的能力与上限在选路时生效。** `--tools` / `--vision` / `--reasoning`
声明的是「这个模型能做这件事」：请求用到某项能力、而目录声明**不支持**时，这个
模型下的候选全部落选。两个上限同理——请求的输入加输出超过 `--context-limit`、
或输出超过 `--output-limit` 时落选。一个候选都不剩时报「N 个候选渠道都不支持
这次请求」（HTTP 502），一个上游调用都不会发出去。判据里的输入 token 数是
**只按正文估算**的那个量（`channel.Request.PromptText`），不含工具定义、图片与历史里的
tool_use 参数。它与**预留**已经不是同一个口径——预留把那几项算了进去（见
`internal/api` 的 `inputTokensOf`）。方向是判据偏宽松：这个数偏小只会少挡几个其实接得住的请求，
不会把一个上游接得住的请求拒掉。

「没填」与「声明不支持」在库里不是一回事，读侧的处置正好相反：

- **两个上限留空（0）表示未知**，不参与判断。采集失败留下的空值不该让一整批
  模型用不了。
- **六项能力各是三态**（库里的 `supports_*` 列可空）：不给那个开关 = 未知
  （`NULL`，不参与判断）、给 = 支持、`--tools=false` = 明确声明不支持。
  所以不带那六个开关建出来的记录不会被任何请求筛掉；声明过不支持的才会被筛掉
  （要放行就 `larkaxctl model update --model <规范名> --tools`）。`model get` 逐项打出
  三态，`model list` 只列已声明的项。
- **早于 `00025` 建出来的记录，那六项是 `false`**（旧列的默认值）。那条迁移
  不动存量数据——运维没声明过的能力不会被替它猜成「未知」，所以那些记录仍按
  「不支持」判，要放行同样用上面那条 `update`。
- **目录里没有这条记录**时不判任何条件：采集还没跑到是最常见的开始状态。

**二、建一条渠道。** 凭证只能从标准输入或环境变量读，**不能写在命令行参数里**
——参数会出现在 `ps` 的输出与 shell 历史里：

```sh
# 从标准输入
cat secret.txt | go run ./cmd/larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-stdin

# 或者从环境变量
LARKAX_CHANNEL_CREDENTIAL='<CHANNEL_CREDENTIAL>' go run ./cmd/larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-env LARKAX_CHANNEL_CREDENTIAL
```

`--type` 必须是已注册的适配器类型（当前是 `openai`、`anthropic`、
`openai-responses` 与 `systemone`——最后一条是决策表面，见「决策接口」一节）。
上面的示例用 `openai` 只是占位；上游说的是 Anthropic 协议（例如 Claude）时用
`anthropic`，不要照抄示例的 `openai`。

`--base-url` 填到**端点之前**：`/chat/completions`（`anthropic` 渠道则是
`/messages`）由我们补上去。带不带版本段都行——`https://api.example.com/v1` 与
`https://api.example.com` 拼出来是同一条 URL，已经带了 `/v1` 时不会重复加。
**端点本身不能写进去**：`https://api.example.com/v1/chat/completions` 会拼成
`…/v1/chat/completions/v1/chat/completions`，上游回 `404`——而那个 404 长得
很像「模型不存在」。

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

`--user` 把渠道记在某个用户名下（留空表示平台自营）。它**只写
`channels.owner_user_id`，不做权限检查**：任何存在的用户都可以被写成归属，
而这个用户是不是 `merchant` 没有一处代码会看（见下文「用户的角色」）。
归属是分账的依据——商家作用域的计价规则按它匹配，结算时
`req.OwnerUserID` 跟着渠道走进账本，而不是「他有没有资格」的判据。

**三、配别名：这家上游对同一个模型叫什么名字。**

```sh
go run ./cmd/larkaxctl alias create --model claude-x \
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

`larkaxctl alias resolve --model <名>` 回答的正是「这个规范名在某个渠道上
实际会发出去什么名字」，它走的就是选路那同一条查询（见 `cmd/larkaxctl/alias.go`
里 `runAliasResolve` 的注释）。不带 `--channel` 时只看平台级那条，所以「配了
渠道级别名、没有平台级别名」时它会报错、退出码走「配置缺失」那一档。

**别名上还能配一组参数覆盖**（`--overrides '<JSON 对象>'`）。它改的是**发往
上游的那份请求**，用来给不传某些参数的调用方一个默认值，或者把某个上限钉住：

```sh
go run ./cmd/larkaxctl alias create --model claude-x \
  --upstream claude-3-5-sonnet-latest \
  --overrides '{"temperature": 0.2, "max_tokens": 4096}'
```

三条口径：

| 问题 | 口径 |
|---|---|
| 能覆盖哪些字段 | 只有 `temperature`、`top_p`、`max_tokens`；**其余一律在写入时拒绝** |
| 与调用方冲突时谁赢 | `max_tokens` 由覆盖赢；`temperature` / `top_p` 调用方给了就调用方赢 |
| 计价按哪一份值算 | 按**覆盖之后**的那份：预留的输出上界取自覆盖之后的 `max_tokens`（非流式按它一次冻满；流式先冻第一段，见下） |

第一条是白名单而不是黑名单：请求体里能改的字段比列得出来的多，黑名单漏一个的
后果是它悄悄生效了，白名单漏一个的后果是它不生效且**写入时就报错**说明原因。
`model` / `messages` / `tools` / `stream` 这四个除了不在白名单里，还被单独点名
——它们改的是请求的形状（发给谁、正文是什么、有没有工具、是不是流式），报错时
会说清这一点：

```
overrides 不允许覆盖 "messages"：它是调用方发来的正文（允许覆盖的只有 temperature、top_p、max_tokens）
```

第二条里 `max_tokens` 与另外两个相反，是因为它管的东西不同：它是输出上界，
商家限制它是在保护自己的成本，而调用方要的是「这次想要多长」。所以覆盖总能顶掉
调用方给的那个数，**包括比它更大**。

第三条是第二条的后果，也是这一列最容易算错的地方：预留发生在选路之前，那时的
输出上界只能按候选集里覆盖之后的 `max_tokens` 取最大。按调用方给的那个数算，
而实际发出去的请求允许生成得更多时，结算会撞上预留上界——差额被截掉，表现为
**少收**，而账上那一行完全正常。所以商家把上限调大时，**非流式**请求被冻住的
金额也会跟着变大（结算仍按上游实际报的用量算，收多少与上界无关）。

**对流式不成立。** 流式只先冻第一段，之后随真实产出递进补冻（见
`internal/pipeline` 的 `ReserveStream` 与 [metrics.md](metrics.md) 的
`stream_balance_exhausted_total`），上限只决定冻结额的**封顶**——把上限调大通常
不会让用户被冻住更多，调和它只影响一条真正产出很多、撞到封顶的流。

覆盖与上游名来自**同一条别名行**：渠道级别名命中时用它那条的覆盖，不会回平台级
那条上再凑一次。

**四、配一条计价规则。** 没有生效的规则时请求会失败，而不是按零价放行：

```sh
go run ./cmd/larkaxctl price create --scope platform --name example-price \
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
`merchant` 必须给 `--user`、`channel` 必须给 `--channel`。

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
- 凭证密文与从属的行都不动，所以 `secret rotate` 与健康度统计都会跳过它。

删除是幂等的：重复删同一条不是错误，第二次打印「已经是删除状态」。

**退出码**：`0` 成功、`1` 其他失败、`2` 命令行参数错、`3` 连不上数据库、
`4` 约束冲突（唯一键、外键、要改或要查的那一行不存在；`wallet credit` 的
同一个幂等键已经对应另一笔入账也归这一档）。脚本按这个分派处置。
「要查的那一行」指解析一个标识时它不在库里（例如 `--user` 给了一个不存在的
用户名），它的下一步与撞唯一键一样：先去看数据里现在是什么。

需要主密钥（`LARKAX_MASTER_KEYS`）的命令只有两个：渠道的**写入**
（`channel create` / `update`）与 `secret rotate`。凭证是信封加密的，只有
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
- `channel create --user <用户>` 不检查那个人是不是 `merchant`：归属只决定
  分账记到谁的账上，不决定「有没有资格」。
- 角色改不了。`user create` 只建新用户，重名直接报错，没有「改角色」的命令。

这两个标记是给管理面的鉴权**预留**的（#158）。在那条路接上之前，把 `--role`
当成一条备注，而不是一条访问控制。

### 开户：新用户需要一行钱包

`user create` 只写 `users` 表，不建钱包行；而每一次推理请求在预留那一步都要
锁住用户的钱包，锁不到就报「没有对应的钱包」。所以新用户在打请求之前要开一次户：

```sh
go run ./cmd/larkaxctl wallet create --user alice
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
go run ./cmd/larkaxctl wallet credit --user alice --amount 100 --ref ORD-20261017-1
```

它走账本：写一组 `topup` 分录（借 `user:<ID>`、贷 `payment_gateway:<来源>`），
并在同一个事务里更新 `user_wallets.balance`。余额因此始终等于分录之和，
`ledger balance` 与 `ledger reconcile` 都不会报出差额。

| 旗标 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `--user` | 是 | — | 用户 ID 或用户名 |
| `--amount` | 是 | — | 正数，最多 8 位小数（末尾的 0 不算位数） |
| `--ref` | 是 | — | 这笔入账对应的外部单据号，同时是幂等键，没有默认值 |
| `--asset` | 否 | settings 的 `billing.default_asset`（`CNY`） | 当前只有 `CNY` 一种 |
| `--gateway` | 否 | `manual` | 钱的来源，进 `payment_gateway:<来源>` 科目；来源名归一成小写 |

`--ref` 被确定性地映射成这次交易的 `txn_id`，所以同一条命令重跑两次只入账
一次：第二次输出「已经入过账，本次未改动」并退出 0。同一个键配不同的金额、
用户或通道不是重放而是冲突——退出 4，一笔都不写。

这条命令是给运营手工记账用的：钱包必须已经存在（先 `wallet create`），
`--gateway manual` 表示线下到账（转账、现金），那个科目的余额与线下流水核对。
它不做退款、不做提现，也不接支付通道的回调。

拿到的 key 打一次推理请求：

```sh
curl -i 127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}'
```

`"model"` 填的是第一步那条**规范名**（这里就是 `claude-x`），不是别名里的上游名
——规范名是选路与计费共用的键。填成上游名会选不中任何渠道，回 `503`。

`max_tokens` 是**必填**的，虽然 OpenAI 协议里它可选：预留必须有上界，而预留上界
由它算出来（见 `billing.ErrNoUpperBound`）。漏了它会在请求出网关之前就回 400
`max_tokens is required: the request must set a positive output token limit`，
错误里不会提到这是本地网关的要求。写 `stream: true` 则走 SSE，其余字段照旧。

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

它按顺序查六项，每一项都打出「检查了什么、结论是什么」；末尾另有一行「上游连通」，
不加 `--live` 时那一行一直是 `-`（见「真打一次上游」）：

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

### 输出的形状

默认每项一行，末尾一行汇总：

```text
larkaxctl doctor：逐项检查「配好的链路能不能接一次请求」。

  ✓ 数据库    连接正常（迁移 v23）
  ✓ 渠道      enabled 且 models 非空的渠道 1 条
  ✗ 模型目录  渠道 1（channel-a）的 models 里有 "model-x"，但目录里没有这个名字
  ! 别名      没有可服务的规范名
  ! 计价规则  没有可服务的规范名
  ✗ 钱包      用户 1（alice）没有钱包行
  - 上游连通  没跑：加 --live 才会真打一次上游（会花钱、要凭证）

  doctor: 2 项异常, 2 项跳过, 2 项通过 (运行 -v 查看细节)
```

行首是状态符：`✓` 通过、`✗` 不通过、`!` 没跑成、`-` 没跑。加 `-v`（或
`--verbose`）时，每项还会跟一段缩进的细节：检查范围、读到的数、别名与计价规则
的表、每条问题的完整原文（「多半是拼写差了一个字符」这一类）。

**细节一条都没删，只是默认不打。** 一次全通过的运行原先要刷三十多行，而其中
没有一行需要动作。

其余命令的输出约定（颜色什么时候出现、表格怎么对齐、空清单怎么说、加一个新的
检查项或新的清单要照什么形状写）见 [docs/cli-output.md](cli-output.md)。

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
| `--base-url <URL>` | 网关公共入口的地址，默认 `http://127.0.0.1:8080`（换过 `LARKAX_HTTP_ADDR` 就要显式给） |
| `--credential-stdin` / `--credential-env <变量名>` | 读 API Key 的来源，二者只能给一个（`--stdin` / `--env` 是同一个选项的简写） |
| `-v` / `--verbose` | 每一项多打一段细节，默认不打。**全局选项**，写在子命令前或后都认；目前只有 `doctor` 用它 |
| `-q` / `--quiet` | 压掉「给人看」那一档的全部输出，只留退出码（致命错误与 `-o json` 不受影响，见 [cli-output.md](cli-output.md)） |
| `-V` / `--version` | 打版本号，**不是** `-v` 那个详细模式。同样是全局选项 |

六个简写的完整清单，以及它们和全名的对应关系，见
[docs/cli-reference.md](cli-reference.md)。

这里读的是打网关用的 **API Key**（`apikey create` 发的那把），不是渠道凭证。
与 `channel create` 同一条约定：**凭证不能写在命令行参数里**——参数会出现在
`ps` 的输出与 shell 历史里。

### `doctor` 的退出码

`0` 六项都通过（给了 `--live` 时那一次也成功）、`1` 有问题或某一项没跑成、
`3` 连不上数据库。

这只是退出码契约落在 `doctor` 上的那一部分。完整的是五档——`0` 成功、
`1` 其他失败、`2` 命令行参数错、`3` 连不上数据库、`4` 约束冲突，
外加「写入路径自己校验出来的参数问题归 `1` 不归 `2`」那条脚注——
见 [docs/cli-reference.md](cli-reference.md) 的「退出码」一节。那一份
才是对所有命令生效的对外契约。

**「没跑成」与「通过」是两件事**，输出里分得开：没有可检查的对象时（例如一条
enabled 渠道都没有，第 3 项就没有名字可查）那一项报「没跑成」而不是「通过」，
退出码也非 0。两者在安静的终端里长得一样，是这个项目反复踩的坑
（见 [LESSONS.md](../LESSONS.md) 第 16 条）。

## 给一把 key 设额度

两列额度可以在创建时给，也可以事后调：

```sh
go run ./cmd/larkaxctl apikey create --user alice --rpm 60 --quota 12.34
go run ./cmd/larkaxctl apikey list                       # id、前缀、用户、备注、状态、每分钟、额度、创建、过期、最近使用
go run ./cmd/larkaxctl apikey update --id 3 --rpm 120
go run ./cmd/larkaxctl apikey update --id 3 --clear-rpm
```

| 旗标 | 含义 |
|---|---|
| `--rpm <整数>` | 每分钟请求额度 |
| `--quota <金额>` | 这把 key 累计可花费的金额上限，单位是该用户钱包的资产品种 |
| `--clear-rpm` / `--clear-quota` | 写回 NULL（继承用户默认） |

**`0` 与不填不是一回事**：`0` 是一个都不放行，NULL 是继承用户默认
（`users` 表上目前没有这样一个默认值，所以等价于不限）。
`update` 不给某个旗标表示「这一项不改」，与清空也不同。
负数一律拒绝。这两项**不开简写**：它们是限额，写短了容易看错也容易按错。

`--quota` **是生效的**，不是只记下来：超限的请求会被拒（`429` +
`insufficient_quota`，见 [limits.md](limits.md)）。判据只有一份
（`internal/keyquota` 的 `CheckInTx`），**落点有两处**——入口在读请求体之前的
早拒绝（`internal/ledger/store` 的 `Store.CheckQuota`），以及预留那个事务里的
权威判定。两处引用的是同一份判据，不是两条各算各的规则。

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
| `model` | 模型目录：`create` / `list` / `get` / `update` / `delete` |
| `alias` | 规范模型名到上游模型名的映射：`create` / `list` / `update` / `delete` / `resolve` |
| `price` | 计价规则：`create` / `list` / `update` / `enable` / `disable` / `explain`；兼管分时计价的日历数据：`price calendar import` |
| `user` | 用户：`create` |
| `wallet` | 用户钱包：`create`（余额 0）/ `credit`（手工入账，走账本写分录） |
| `apikey` | API Key：`create` / `update` / `list` |
| `ledger` | 账本：`balance` / `verify`（两条都只读）与 `reconcile`（对账，会写运行记录） |
| `settings` | 运行期配置：`list` / `set` |
| `plugin` | 插件：`list` / `publish` / `enable` / `disable` |
| `partition` | `usage_events` 月分区：`ensure` / `check` |
| `doctor` | 逐项检查这条链路通不通（`--live` 才真打上游） |
| `snapshot` | 请求快照：`cleanup` / `allowlist` / `get` / `redact` |
| `secret` | 主密钥轮换：`rotate` |
| `maintenance` | 运行期数据的留存清理：`idempotency`（`--dry-run` 只数不删） |
| `migrate` | 迁移：`up` / `down` / `down-to` / `up-to` / `status` / `version` / `create` |
| `version` | 打印版本号 |

每个子命令的用法见 `go run ./cmd/larkaxctl <子命令> --help`，
或直接 `go run ./cmd/larkaxctl` 看总览。对账的开关、口径与运行记录见
[reconciliation.md](reconciliation.md)，日历数据的文件格式见
[calendar-data.md](calendar-data.md)。

两个端点都认请求头 `Idempotency-Key`：带上它时同一个键重复发送只执行一次，
第一次的结果被原样重放。用法、三条 `409` 的码与留存规则见
[idempotency.md](idempotency.md)。

## 决策接口（`/v1/systemone`）

`POST /v1/systemone` 是第四条公开路径。它不做对话，而是拿一段**状态**（一段
自由文本，比如一张工单）与一组**带类型的判定问题**，回一组答案。与另外三条
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

### 门禁脚本

`scripts/` 下的门禁脚本，外加契约测试的两条命令：

```sh
bash scripts/check-redlines.sh      # 公开红线：密钥、私有主机、本机路径
bash scripts/check-layering.sh      # 分层约定：谁 import 谁
bash scripts/check-deadcode.sh      # 不可达符号：可达的包内部有没有没人调的函数
bash scripts/check-unimported.sh    # 整包没人 import：包本身有没有接上
bash scripts/check-deadmethods.sh   # 导出方法有没有调用点（上面两道门都照不到）
bash scripts/check-test-names.sh    # Go 顶层函数声明的名字必须是 ASCII
bash scripts/check-lessons-index.sh # LESSONS.md 的索引与 lessons/ 下的正文对得上
scripts/check-mutation.sh           # 变异测试（改动计费/账本时），见下文

# 契约测试：用官方 SDK 当客户端验我们对外响应的形状。要先装 Python 依赖，
# 且 go vet 默认不看 build tag 后面的文件，所以这里要单独带 -tags 跑一遍。
pip install -r contract/requirements.txt
go test -tags=contract -count=1 -v ./internal/assembly/ -run Contract
go vet -tags=contract ./internal/assembly/
```

`scripts/check-redlines.sh --all` 扫全树而不是暂存区。

**改了哪个脚本就跑它自己的自测**：`scripts/check-redlines.test.sh`、
`check-deadcode.test.sh`、`check-unimported.test.sh`、`check-deadmethods.test.sh`、
`check-test-names.test.sh`、`check-layering.test.sh`、`check-mutation.test.sh`。
扫描器坏掉时的输出和「干净」一模一样，没有自测就无法证明它还在工作
（见 [LESSONS.md](../LESSONS.md) 第 2、7 条）。

### 每道门在挡什么

`scripts/check-deadcode.sh` 需要 `deadcode`，版本必须与脚本里的
`PINNED_TOOLS_VERSION` 一致——符号集合随 `golang.org/x/tools` 的版本变化，
版本不对它直接拒绝运行（它会打印该装哪一版，然后拒绝运行）。它比对的白名单是
`scripts/deadcode-allowlist.txt`：新出现的不可达符号直接失败；白名单里已经消失的
条目只提示「可以删了」，不算失败（它出现的原因是有人把没门的东西接上线了）；
条目数只许减不许增。

`scripts/check-unimported.sh` 是它的**补集**，只依赖 `go`：把「仓库自己的包」
（`go list ./...`）与「从入口可达的包」（`go list -deps`，入口同样由 `cmd/`
自动发现）两个集合相减，差集就是**整包没有任何 import 方**的那些——
`deadcode` 照不到这一类，因为它只分析从入口可达的包。白名单是
`scripts/unimported-allowlist.txt`，口径与上面那份一致。

**只被测试 import 的包算「没人 import」。** 测试自己就能构造调用方，
所以「只有测试在调」与「生产在调」在覆盖率上长得一样
（见 [LESSONS.md](../LESSONS.md) 第 22 条）；测试夹具包因此也要进白名单。

**不要为了把白名单刷空而去硬接一条调用方**——接一条假的比留着它更坏。

`scripts/check-deadmethods.sh` 是**第三类**，只依赖 `go`：一个导出方法在
「入口可达的、非测试的」文件里有没有出现过调用语法。`deadcode` 照不到这一类——
它底下的 RTA 把「任何 runtime type 的导出方法」整体当成可达（反射可能调到），
而且掩蔽会沿调用链传下去：一个被掩蔽的方法如果是某个自由函数唯一的调用方，
那个函数也从清单里消失。本仓不用反射，所以这一类要单独有一道门。
判据走 Go 的 AST 而不是文本匹配（注释与字符串里的 `.Name(` 不算调用点），
白名单是 `scripts/deadmethods-allowlist.txt`，口径与上面两份一致。

它有两个**已知的边界**，写在脚本与白名单的开头，别当成 bug：按方法名判，
所以同名方法会互相掩护（假阴性）；`f := x.Name` 这种方法值语法与字段访问
无法区分，取严的一侧（假阳性）。它与 `deadcode` 的白名单**不共享条目**——
判据不同，同一个符号可能在两边各记一条，也可能只在一处出现。

`scripts/check-lessons-index.sh` 管的是 [LESSONS.md](../LESSONS.md) 与 `lessons/`
下正文的一致性。LESSONS.md 是索引、每条正文各一个文件，两者原先只靠手工对齐——
索引里多一条、正文没建，点进去是 404；编号与文件名脱钩时更坏，点进去是**另一条**
而没人会察觉。它只依赖 bash 与 coreutils，与分层自查同一类，所以放在那个 job 里。

它与上面几道门同一个形状：判据是集合比对，坏掉时（索引一行都没解析出来、
`lessons/` 一个文件都没扫到）的输出和「全部对得上」一模一样。所以它每次都打印
「解析出多少行索引 / 扫到多少个文件」，任一为 0 就判「没跑成」（退出码 2），
而不是报通过（见 [LESSONS.md](../LESSONS.md) 第 16 条），自测也照第 2、23 条一并跑。

`scripts/check-test-names.sh` 管的是「标识符保持英文」这条约定：**Go 顶层
`func` 声明的名字**只许由 `[A-Za-z0-9_]` 组成。判的是**名字那个标识符**，
不是「整行含中文」——后者会把中文在函数体、字符串或形参名上的行全部误报
（实测全仓 5 处命中，5 处都是误报）。三种声明形态都认：普通函数、方法
（跳过接收者）、泛型函数（名字截到 `[` 之前）。退出码分三档：0 通过、
1 命中、2 **没跑成**（一个文件都没扫到、一条 `func` 都没有、或者解析不出名字）
——第 2 档与第 1 档分开，是因为「没跑成」在输出里和「干净」长得一样
（见 [LESSONS.md](../LESSONS.md) 第 16 条）。

它的**已知边界**写在脚本头注释里，各有自测钉着：函数体内的局部变量、形参、
包级 `const` / `var` / `type` 名不判；非 Go 文件不判。这条约定的动机是
`go test -run` 的正则与 CI 失败行里的可读性，那些名字不出现在这两处。

### golangci-lint

`golangci-lint` **只开有价值的检查器，不做「全部打开」**——一份报上百条无关警告的
报告，第二次就没人看了。配置里每一项都有它对应的理由。

版本上有两个坑，都实际撞过：

1. **用比 `go.mod` 更旧的 Go 构建的 golangci-lint 会直接拒绝运行**。
   release 二进制通常比当前 Go 旧，所以本地要用 `go install` 一份，
   CI 里用 `install-mode: goinstall`。
2. **它自带的 typechecker 也要够新**。旧版读不了新版 Go 的导出数据格式，
   报 `export data version N is greater than maximum supported version M`——
   这个错看起来像代码问题，其实是工具版本问题。

所以升级 Go 或 golangci-lint 时，两者要一起看。

### 本地跑门禁的前提

- **`check-deadcode.sh` 要先装 `deadcode`**，版本与脚本里的 `PINNED_TOOLS_VERSION`
  一致；不一致时脚本会打印该装哪一版，然后拒绝运行。
- **集成测试要数据库**：`LARKAX_DB_DSN` 没设时那些用例整包 `t.Skip`，
  而「跳过」在输出里和「通过」长得一样。起库见上文「一次跑通」。
- **限流那几条集成测试要 Redis**：`LARKAX_REDIS_DSN` 没设时 `internal/ratelimit`
  里那几条同样跳过，判据同上（跳过与通过长得一样）。本地跑：

  ```sh
  docker run -d --rm -p 127.0.0.1:6379:6379 redis:7-alpine
  export LARKAX_REDIS_DSN="redis://127.0.0.1:6379/0"
  go test ./internal/ratelimit/
  ```

  它们验的是「键真的有过期时间」「两个实例真的共用一份计数」这类**只有真存储
  才有的性质**，拿内存替身装作不出来（替身另有一组用例，不需要 Redis）。
- **`check-mutation.sh` 最慢**：它串行跑（固定 `workers=1`），只在改动计费、
  账本、金额时跑，见下文「变异测试」。

CI 跑的是同一批门（变异测试只跑它的自测），见下文「CI 跑什么」。

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

**DSN 指向一个空库就够了，不必先手工 `migrate up`**——助手会在它上面建库、
跑全量迁移、跑完删掉，DSN 里的库名只当连接信息用。（`larkaxctl` 自己要连的
那个库仍然要迁移，那是另一回事：`go run ./cmd/larkaxctl migrate up`。）
不想走 compose 那一套时，直接起一个库也行：

```sh
docker run -d --name larkax-pg -e POSTGRES_USER=larkax \
  -e POSTGRES_PASSWORD=placeholder -e POSTGRES_DB=larkax \
  -p 127.0.0.1:5432:5432 postgres:16-alpine

export LARKAX_DB_DSN="postgres://larkax:placeholder@127.0.0.1:5432/larkax?sslmode=disable"
go test ./internal/ledger/store/
```

**没设 `LARKAX_DB_DSN` 时集成测试照旧跳过；设了却连不上（或建不了库）时是失败，
不是跳过。** 这两种状态在输出里原本长得一样，而它们的处置完全不同：前者是
「本机没起库」，后者是「你以为在跑集成测试，其实一个都没跑」。

## CI 跑什么

CI 包含：`gofmt` 检查、`go vet`、`golangci-lint`、单测（含 `-race`）、覆盖率摘要、
迁移 up→down→up 往返（真 PostgreSQL）、分层自查及其自测（`check-layering.sh` /
`check-layering.test.sh`）、红线扫描及其自测、变异测试脚本的自测、不可达符号门禁
及其自测（`check-deadcode.sh`）、整包没人 import 的门禁及其自测
（`check-unimported.sh`）、导出方法调用点门禁及其自测（`check-deadmethods.sh`）、
测试函数名门禁及其自测（`check-test-names.sh`）、索引与正文一致性门禁及其自测
（`check-lessons-index.sh`）、非 UTC 时区下的时刻门禁（`TZ=Asia/Shanghai` 再跑一遍
受影响的包，见 [cli-output.md](cli-output.md) 的「时刻与时区」）、契约测试（见下）。

**变异测试不在 CI 里跑**（CI 跑的是它自己的自测），它只在本地按需跑；
本地先把这一批门跑一遍是为了少一轮来回。

契约测试是**独立的一个 job**（`契约`），与上面那道流水线并列：它多两个运行期
依赖（Python 3.12 与两个官方 SDK），塞进 Go 那个 job 会让「Go 测试挂了」与
「Python 依赖没装」混在同一份日志里。它验的是**我们对外响应的形状**，用官方 SDK
当客户端——仓库里的端到端用例用的是自己写的假上游与自己构造的请求，证明的只是
「我们的响应我们自己能解析」。怎么跑、怎么加一条、有哪些已知偏差，见
[contract/README.md](../contract/README.md)。

它与集成测试那道门是同一个形状：`-run Contract` **匹配不到任何用例时 `go test`
照样退出 0**，所以那个 job 的判据是「`--- PASS: TestContract...` 出现过几次」，
不是退出码。

集成测试另有一道**「确认真跑了」的门禁**——它逐包显式跑依赖数据库的那些包，
被 `t.Skip` 时报错，因为「跳过」在输出里和「通过」长得一样
（见 [LESSONS.md](../LESSONS.md) 第 16 条）。**包清单的唯一来源是**
[.github/actions/check-integration-tests](../.github/actions/check-integration-tests)，
新增依赖数据库的测试包时要往那里补；别处不重复列一遍——两处清单迟早会不一致，
而那时没人知道哪一份算数。

**自动部署尚未接入**，落地时补进这条流水线。

## 变异测试

改了**计费、账本、金额**这三处代码时，除了单测还要跑变异测试。覆盖率只说明
「这一行被执行过」，不说明「这一行的每个分支被钉住了」——把 `>=` 改成 `>`、
把分支方向反过来，覆盖率一点不变，而测试照样通过。

```sh
export LARKAX_DB_DSN="postgres://larkax:placeholder@127.0.0.1:5432/larkax?sslmode=disable"
scripts/check-mutation.sh              # 全部包
scripts/check-mutation.sh money route  # 只跑指定的
```

**`LARKAX_DB_DSN` 是硬前提，不是可选参数**：没有它 `internal/ledger/store` 的
集成测试整包被 `t.Skip`，那一包等于没测。脚本会直接拒绝运行，而不是跑出一批
不可比的数字（见 [LESSONS.md](../LESSONS.md) 第 17 条）。

实测：计费与账本各包的覆盖率都在 82%~98%，而变异测试每次都找出两位数个
**存活**变异——它们集中在「恰好相等」「恰好为零」这类边界上，
而写测试时的自然倾向正是跳过这些。

**并发与超时系数是同一个旋钮，而并发是更该先调的那个。** gremlins 给每个
变异的超时预算 = 「跑一次测试集的时间 × 系数」，而那个基准是**串行**测出来的；
并发跑变异时机器上同时有 N 个 `go test` 在编译与执行，超时按墙钟算，
于是并发越高、越多变异被判超时。

实测（`internal/money`，系数固定 10）：

| workers | 结果 |
|---|---|
| 1 | `Killed 80, Timed out 0` |
| 2 | `Killed 80, Timed out 0` |
| **4** | `Killed 3, Timed out 92` |

所以**调大系数是在补并发造成的短缺**，而那会让「这个包需要多大系数」
变成一个随机器负载变化的**假属性**——同一份代码两次运行给出截然不同的分数，
一次「0.00% efficacy」看起来像真实结论。

`scripts/check-mutation.sh` 因此固定 `workers=1`、系数 10，**不要自己拼
`gremlins` 命令**：串行慢，但变异测试不是每次提交都跑的东西，
**慢而可信比快而误导好**。

### 一个分数要连着它的条件一起读

固定了 workers 还不够。同一个包在两种条件下给出的不是同一个数：

| 条件 | `internal/ledger` | Mutator coverage |
|---|---|---|
| `LARKAX_DB_DSN` 有效 | `Killed 132, Lived 11, Not covered 27` | 84.12% |
| `LARKAX_DB_DSN` 不存在 | `Killed 94, Lived 7, Not covered 69` | 59.41% |

**变的是「有多少变异体被测到」，不是「杀掉的比例」。** 两个 Test efficacy
几乎一样（92.31% 与 93.07%），差得多的是 Mutator coverage——所以
**只看 efficacy 会把这件事完全盖住**，要连着 Mutator coverage 一起看。

原因：gremlins 收集覆盖时用的是**整个模块**的测试集，而被测包只是其中一个。
`internal/ledger/store` 的集成测试要 `LARKAX_DB_DSN`，没设时整包 `t.Skip`，
于是它给 `internal/ledger` 贡献的那部分覆盖也没了——可测的变异体从 143 个
掉到 101 个，那 42 个从「被杀/存活」挪进了「不可达」。

修法是**把 DSN 变成硬前提**，而不是去收窄覆盖范围：脚本缺 `LARKAX_DB_DSN`
时直接拒绝运行。收窄成 `--coverpkg=./<pkg>/` 是错的方向——那是把「被别的包
测试覆盖到」的部分白白丢掉（可测变异体 143 → 101），数字看着稳了、信息量少了。

另一条来源是机器上的别的负载——它挡不住，只能按下面的判据识别：

| 包 | 机器空闲 | 有别的负载 |
|---|---|---|
| `internal/billing` | `Killed 145, Timed out 0` | `Killed 136, Timed out 10` |
| `internal/tokens` | `Killed 100, Timed out 0` | `Killed 95, Timed out 5` |

**判据是超时条数，不是百分比。** 超时不为 0 就说明这一轮受了干扰，
那几个数不与历史比较、也不抄进表里。

### 对存活的变异逐个判断

`scripts/check-mutation.sh` 跑完会把**存活的变异逐条列出来**（包名 + 变异类型 + 位置）。
只打一个计数的话这一节没法执行——数得出来，看不了，于是「逐个判断」实际退化成
「看一个百分比」。存活不进退出码：存活本身不是失败，判过之后可以放过。

只有两类可放过：

- **等价变异**：改法与改后行为不可区分（例如 `f >= unit` 与 `f > unit`
  在「小数部分恰好填满一个单位」时结果相同）
- **不可达分支**：前置条件已经保证走不到，例如「商超出 int64 时它不可能为 0」

放过时在测试里写一行说明，否则下一个人会以为是漏了。

### 用常量测常量等于没测

`internal/quota` 里曾经写成

```go
if got := score(selfAccountingStaleAfter); got != baseScore { ... }
```

被测的阈值与喂进去的输入是同一个常量。把它从 15 分钟改成 15 秒，
用例照样通过——测的是「相对关系」而不是这个数。要钉住一个数就用字面量：
`score(14 * time.Minute)`。

### 各包的现状

由 `scripts/check-mutation.sh` 跑出（workers=1、系数 10、带 `LARKAX_DB_DSN`）。
**超时列不为 0 的包，这一轮的分数不与历史比较**——见上文。改动过某个包的测试
之后重跑并在表里更新。

**这张表是历史值，暂不与新一轮比较。** 它出自一个没钉版本的 gremlins
（`@latest` 装出来的，上游一变变异器集合就变），与现在脚本跑出来的不是同一把尺子。
efficacy 的分母是变异体数，分母变了分数就会变——所以**表待按当前口径整表重跑**。

**怎么跑**：`scripts/check-mutation.sh`（需要 `LARKAX_DB_DSN` 与钉住那一版的
gremlins；缺哪个它都会拒绝运行并给出安装命令）。它每次运行打一行「这把尺子：…」，
写的就是这个数的三个条件：

- gremlins 版本：`v0.6.0`，由脚本里的 `PINNED_GREMLINS_VERSION` 钉住，
  运行时探测并校验，不符就拒绝运行；
- 变异器集合：脚本从 `unleash --help` 探测，不在这里抄一份——抄第二份的那天
  就是两份开始分岔的那天；
- `--invert-logical` 已打开（`&&` ↔ `||`，默认关闭）。打开它之前，`a || b`
  这类判据一个变异体都不生成，那时「efficacy 高」不能当成「布尔判据也被钉住了」。

**贴数字时把那一行「这把尺子」一起贴上**：它是数字的条件，不是装饰。
两次运行的条件不同，两个数就不是同一个包的两个样本，别放在一起比。

| 包 | 覆盖率 | Test efficacy | 超时 |
|---|---|---|---|
| `internal/gateway` | 96.4% | 100% | 0 |
| `internal/pipeline` | 86.8% | 96.0% | 0 |
| `internal/quota` | 95.6% | 93.9% | 0 |
| `internal/calendar` | 97.8% | 93.6% | 1 |
| `internal/ledger` | 96.5% | 92.3% | 0 |
| `internal/ledger/store` | 79.8% | 90.2% | 1 |
| `internal/route` | 93.3% | 84.3% | 0 |
| `internal/money` | 95.9% | 84.2% | 0 |
| `internal/billing` | 97.5% | 81.0% | 0 |
| `internal/tokens` | 97.4% | 74.6% | 4 |
| `internal/snapshot` | 88.6% | 70.8% | 7 |
| `internal/snapshotwire` | 95.2% | 100% | 0 |

**覆盖率高而 efficacy 低，说明测试断言的是「相对关系」而不是「具体边界」。**
`route` 与 `snapshot` 两个包都是这个形状：原用例写的是「A 明显比 B 好」
这种比较，于是改 `<=` 为 `<`、去掉分母的加数、改常量，只要顺序不变就测不出来。

**不要为了把 efficacy 刷上去而写对着分支的琐碎断言。** 它会让数字好看，
但不会让行为更可信。`snapshot` 的遍历器那一层还没补，因为**它需要按功能
场景补而不是按函数补**（例如「一次含工具往返的请求走完整流程」）。
