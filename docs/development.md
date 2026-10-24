# 本地开发环境

本文件很长，按下面的目录找。想看「这个仓库有哪些文档」而不是「这一份讲了什么」，
去 [docs/README.md](README.md) 的索引。

- [需要什么](#需要什么)
- [一次跑通](#一次跑通)
- [仓库结构与分层](#仓库结构与分层)
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
- `openssl`——`make` 用它生成主密钥（见「主密钥」一节）。**没有它时 `make init`
  会在生成之前停下并说明缺的是什么**，不会产出一份主密钥为空的 `.env`——
  那份文件一旦存在就不再重建（见下），于是空的那份会一直留着。
- 开发前端（`web/`，尚未落地）才需要 Node.js，版本见仓库根的 `.nvmrc`。
  CI 也从那个文件读；`web/package.json` 的 `engines.node` 写的是支持的最低
  版本，`npm install` 在低于它时会给警告
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
一个上游账号、一条配好的模型 / 渠道 / 出站映射 / 定价链路、一个用户与钱包（新开的户
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
另可验证两项就绪探针：`curl -s 127.0.0.1:9090/readyz` 应含 `"postgres":"ok"`
（连得上库）与 `"tokens":"ok"`（本机支持的每一种分词词表都加载得出来，没预热
或没解出来时报不就绪）。
**健康检查在 `:9090`，不在 `:8080`**——公共端口的 `/` 捕获其余路径并返回 `501`。
同一个内部端口上还有只读的 `/ratelimit/usage?key_id=<id>`，以及管理面
`/admin/v1/*`（见 [admin-api.md](admin-api.md)）。

`LARKAX_ROLE=worker` 的部署不接推理流量：它在公共端口上一律回 `501`，响应里点出
这个角色，所以配错了的部署一眼能看出来，而不是看起来像别的问题。

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
| `make web` | 构建控制台并打成 `dist/larkax-web_<版本>.tar.gz`（要 Node） |
| `make release` | `dist` + `web` + `SHA256SUMS`，发布用的全部资产 |
| `make install` | 装两个二进制、短名链接、手册与 bash/zsh/fish 补全到 `~/.local`（`PREFIX` 可改，详见 [versioning.md](versioning.md)） |
| `make uninstall` | 移除 `make install` 装进去的那些名字 |
| `make help` | 把常用的那几条打出来（不含本表里的 `init` 与 `check-ports`） |

`make migrate` 与 `make test` 都会先跑 `make init`——它们都要那份 DSN。

`make test` 会先起依赖并加载 `.env`——集成测试拿不到 `LARKAX_DB_DSN` 时会**跳过**，
而「跳过」在输出里和「通过」长得一样。

`.env.example` 可以直接被 shell 读取（引号、`export` 前缀、行尾注释都认），
所以 `make` 用 `set -a && . ./.env && set +a` 加载它。**服务与 `larkaxctl` 自己
也读 `.env`**（见下文「环境变量」），所以 `go run` 之前不必再手工导出一遍；
用 direnv 或别的工具加载也不会打架——环境变量优先。

## 仓库结构与分层

```
cmd/larkax/                       服务二进制
cmd/larkaxctl/                    迁移与运维命令行
internal/api/                     公共 HTTP 入口：路径、解析、响应形状
internal/assembly/                把入口层装配成能跑的服务
internal/auth/                    把请求带的凭证换成它属于哪个用户
internal/billing/                 把用量换成钱：预留、结算、明细
internal/billing/calendaradapter/ 把 internal/calendar 接进计价引擎
internal/billing/expr/            求值计价规则的表达式引擎
internal/calendar/                按地区判断工作日 / 节假日 / 峰谷时段
internal/calendar/store/          日历表，以及导入一年的数据
internal/catalog/                 模型目录：规范名与上游别名
internal/channel/                 适配器接口、注册表、统一的请求与响应类型
internal/channel/anthropicadapter/  Anthropic Messages 上游
internal/channel/httpx/           适配器共用的 HTTP 细节
internal/channel/openaiadapter/   OpenAI Chat Completions 上游
internal/channel/responsesadapter/  OpenAI Responses 上游
internal/channels/                从库里取候选渠道，把调用结果写回去
internal/config/                  启动配置（环境变量）
internal/credential/              渠道凭证的信封加密
internal/db/                      数据库连接池
internal/dberr/                   把 Postgres 约束冲突归成同一个错误值
internal/gateway/                 一次请求的顺序：预留、选路、调上游、结算
internal/idempotency/             Idempotency-Key 请求头：重发的请求只执行一次
internal/keyquota/                一把 key 的累计花费有没有超过限额
internal/ledger/                  复式记账：构造与校验分录
internal/ledger/store/            分录、钱包与预留的落库
internal/manpage/                 从命令行注册表与 docs/cli-reference.md 渲染手册
internal/migrate/                 内嵌的迁移（internal/migrate/sql/）
internal/money/                   定点金额；全仓只有这里做舍入
internal/observe/                 日志、请求 ID、健康与就绪端点
internal/overrides/               别名上配的上游参数覆盖
internal/pipeline/                把计费各阶段接成一条请求流程
internal/plugin/                  五个 hook：注册表、分发、失败策略
internal/plugin/host/             注入插件的宿主能力（日志、kv、日历）
internal/plugin/sandbox/          moejs 运行时、限额、中断
internal/pricing/                 挑出这次请求适用的计价规则
internal/quota/                   上游额度状态：观测、自记账、置信度
internal/ratelimit/               按 key 的每分钟次数，配了 Redis 时在 Redis 里计数
internal/relay/                   经 relaykit 做协议转换
internal/route/                   选路与故障转移
internal/settings/                运行期配置，存在数据库里
internal/snapshot/                只留结构的请求快照，含脱敏
internal/snapshotwire/            读快照配置、写骨架、删到期分区
internal/systemone/               决策表面自己的请求与响应形状
internal/testdb/                  仅测试用：每个测试包一个库
internal/tokens/                  分词计数，以及流式中断时的估算
internal/upstream/                装配协议适配器，按渠道类型分发
scripts/                          仓库自带的检查与生成脚本
```

分层，让数据面不依赖控制面：

```
gateway  ──►  route  ──►  channel.Adapter  ──►  upstream
                │                │
                │                └── 只做协议转换
                └── 选路、重试、故障转移

gateway  ──►  pipeline  ──►  billing  ──►  money
                   │             │
                   │             └── 规则来自 billing/expr，它构造 billing.Prices
                   ├── 把分录交回调用方
                   └── 用量从 channel.Usage 归一，token 只是其中一项标签
```

`gateway` 是入口层，`internal/api` 是它的 HTTP 那一半；`cmd/` 在它们之上，
`money`、`calendar`、`observe` 与 `migrate` 是叶子。`relay` 不是叶子——它依赖
`internal/channel` 的类型。除下面点名的三处例外，没有别的包反过来依赖 `gateway`。

有几条规则贯穿整棵树，由 `scripts/check-layering.sh` 在 CI 里查，而不是靠眼睛看
——它列出允许 import relaykit 的包，并标出 `internal/money` 之外出现的
`float64(`（那是给人看的提示，不是失败）：

- **只有三个包 import [relaykit](https://github.com/QuantumNous/new-api)：**
  `internal/relay`（接缝本身）与两个协议适配器（它们需要 DTO 类型去对接）。
  其余包一律用 larkax 自己的类型。
- **`internal/money` 是唯一做舍入的地方。** 其余地方都是对定点值做整数运算。
- **入口层以下的包不 import `internal/gateway`。** 三个例外写在
  `scripts/check-layering.sh` 里：`internal/api` 是入口层的 HTTP 那一半，调用
  `gateway.Chat`；`internal/snapshotwire` 把快照包适配到 gateway 声明的接口；
  `internal/assembly` 构造 gateway，也是唯一同时看得到入口层与数据面的地方。

计费那条路刻意拆成几段，每段都能不起库、不连网就测：`money`（算术）→
`billing`（用量 × 价格）→ `billing/expr`（规则）→ `pipeline`（各步的顺序）。

`internal/gateway` 持有一次请求的整个顺序——调上游之前先预留、流已经开始吐字之后
不再换渠道、快照那条路永远不许拖慢请求。`internal/assembly` 把它装进服务二进制，
包括从库里加载的插件注册表。仓库里还没有 `plugins/`（随包发布的插件源码）；
浏览器控制台在 `web/`。

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

这一列比响应体上的同名码**窄**：响应体那一侧，`upstream_http_error` 盖住两档——
「上游回了非 2xx」与「一个响应都没拿到」；这一列把两档分开记，因为排查与对账要
回答的问题正是「这次请求到底有没有到上游」（见 [api.md](api.md) 的码表）。

五个取值的分界线是**这次调用有没有真的产生 token**：

- `upstream_stream_aborted`：上游接受了请求、一个字都没吐就结束了。没有用量，
  这次不计费，预留被释放。
- 其余四个都表示上游那边出了事，但计不计费要看路径：非流式那三个
  （`upstream_http_error`、`upstream_unusable_response`、`upstream_no_answer`）
  是这次调用失败的原因，响应没交给调用方，不计费；流式里吐过内容之后才出的
  问题（上游在流里报错 → `upstream_unusable_response`，或者干脆把连接掐断 →
  `upstream_stream_truncated`）**已经产生了 token**，这次照常计费。

两列的取值判据与账单同源（`internal/gateway` 的 `upstreamEvidence` 与
`streamResponseEvidence`），但两张表回答的问题不同：快照的 `error_kind` 答的是
「这次响应怎么了」，不看计不计费；账单的 `error_code` 在两种行上答两个问题——
计费的行上它答的是「钱收了，回答是不是在调用方不知情的情况下被截短」，不计费
的行上是「这次没向上游收钱，是不是因为上游这一侧出了事」。出现分歧时，钱的
问题以账单为准，「上游当时回了什么」以快照为准。

`upstream_stream_truncated` 是唯一在两表上都写的分类：那一次回答**计了费**，
而「回答只写了一半」同样值得记在账单上：它没有错误帧，调用方只能自己看出这条
流没写完（见 [api.md](api.md) 的「没有结束标记就是被截断」一节）；而对按账单
核对金额的人来说，账单是唯一能说明这件事的地方（快照另有留存期）。它在账单上
落进 `billing_requests` 的那两列，路径是：网关在流的结局那一刻取定
（`internal/gateway` 的 `billedStreamEvidence`）→ 随 `pipeline.Settle` 搬进
`Charge` → `internal/ledger/store` 的 `markSettled` 写入，`NULLIF` 的口径与
不计费那条路（`Release`）逐字一致。上游在流里报错的那一次不写：错误帧当时就
送到调用方手里了，账单不必再讲一遍。

上游回 4xx 时，响应方向的骨架里还可能多一个 `upstream_error` 子对象。它只在
4xx 出现，里面固定只有三格：`type` 与 `param` 是两个短枚举词，`message` 只留
长度（`{"_t":"text","chars":N}`）。错误体里其余字段整个不保留，不是先存下来再
替换；这条正文的内容一个字也不落库，**任何采集模式都一样**。

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

原样采集（`capture_mode = full_consented`）的行在打正文之前会按这一行的
`user_id` 与 `request_id` 再查一次授权：到期或撤销之后，那一行的正文不显示，
机器可读输出里 `raw_withheld` 为真、`skeleton` 为 `null`。查库失败按「没有授权」
处理，所以失败的方向是扣下正文。

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
上班日。数据从公开数据源拉取，或者从文件导入：

```sh
go run ./cmd/larkaxctl price calendar sync --year 2026
go run ./cmd/larkaxctl price calendar import ./cn-2031.json
```

`sync` 从 holiday-cn 拉取一年并补成全年每一天的判定；`import` 用于手工维护与
修正单日。文件格式、状态取值与落库语义见 [calendar-data.md](calendar-data.md)。
**仓库里不带任何真实数据集。**

## 环境变量

一共九个，全部有明确理由：

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
| `LARKAX_WEB_DIR` | 空 | 控制台静态产物目录（Vite 的 `dist`）。留空时公开监听只提供 API；镜像里指向 `/srv/web` |

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
`larkaxctl migrate up` 在没有配角色的机器上照样能跑。读这个变量的是这几组命令：
`channel create` / `channel update`（写凭证）、`secret rotate`、以及
`plugin delete` / `plugin kv set` / `plugin kv delete`（删插件、或动它的私有存储）
——它们自己读 `LARKAX_MASTER_KEYS`。两个入口见 `internal/config` 的 `Load` 与
`LoadOps`。

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

**每个键的默认值、取值范围，以及各自改完什么时候生效，
见 [settings.md](settings.md)。** 这一节讲的是**加一个新键**的步骤。

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

这段占位符的拼写只在 `Makefile` 顶部声明一次，替换与生成之后那次复查都从它来：
改名时要与 `.env.example` 一起改，只改一处时 `make init` 会直接报错、不产出
`.env`（不报错的话，那份 `.env` 里会是空的或留着占位符，而它之后不再重建）。

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

### 编号更小的迁移漏掉时

`migrate up` 默认会拒绝前进：库里的当前版本是 50，二进制里却有一条从未应用过的 49，
goose 报 `found 1 missing migrations before current version 50` 并停下。

常见的情形是两条迁移同时开发：各自按「当时的 next」取了编号，编号大的先合入并部署，
编号小的后合入，库里的版本就跳过了它。

那条迁移确实该补（没有被别的迁移取代，补在现在的位置也能跑通）时，加开关：

```sh
go run ./cmd/larkaxctl migrate up --allow-missing
```

goose 先按版本顺序补应用这些编号更小的迁移，再应用所有更大的未应用迁移。
补了哪几条打在 stderr 上。默认不开，是因为同一个现象还有另一种来由：
库里那条迁移被人手工回滚过。那时补应用会与已经改过的数据或表结构冲突，
而 goose 分不出这两种情况，只能由人确认。

不改二进制时的等价做法：

1. 从迁移文件里取出 `-- +goose Up` 与 `-- +goose Down` 之间的 SQL；
2. 在同一个事务里执行那段 SQL，再往 `goose_db_version` 插一行：

   ```sql
   BEGIN;
   -- 这里放上一步取出的 SQL
   INSERT INTO goose_db_version (version_id, is_applied) VALUES (49, true);
   COMMIT;
   ```

3. 再正常跑 `migrate up`，它会从当前版本继续前进。

两步要在同一个事务里：分开做的话，SQL 成功而插行失败时，库已经改了、版本表却没记，
再补就得先分辨哪些改动已经做过。goose 自己跑迁移时也是这个做法。

## 配一条能接请求的链路

推理请求要落到某个上游，所以库里得先有四样东西：目录里的一条规范模型、一条渠道
（指向上游、带凭证）、把规范名映射到上游模型名的出站映射、一条计价规则。
`larkaxctl` 的 `model`、`channel`（出站映射是它的 `channel map` 子命令）与
`price` 就是干这个的。

这四样之外还有一个**前置**：计价规则要记下是谁定的（`--by`），而 `--by` 收的
是一个**已经存在的用户**，所以第四步会先建一个用户、再建规则。用户本身不在
这条选路链路上——选路看的是模型、渠道、出站映射与价——但没有它，那条规则建不出来。

下面四步按顺序跑完，就有了一条能接请求的链路。示例里的上游是假的
（`https://api.example.com/v1`），换成你自己的即可。

文中 `<...>` 是**占位符**，尖括号本身不要原样粘进 shell：出现在参数位上时
（`--upstream <上游模型 id>`）它是安全的，出现在赋值前时
（`LARKAX_CHANNEL_CREDENTIAL=<凭证>`）shell 会把尖括号当成重定向，那一行不会
设置任何环境变量。手工填值时把整段用单引号括起来。

**先问你的上游它有哪些模型 id。** 渠道的 `--models` 与
`channel map add` 的上游模型名填的是
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
或输出超过 `--output-limit` 时落选。一个候选都不剩时回 HTTP 502，**一次上游
调用都不会发出去**。这件事有两句话：响应体里给调用方的那句带
`no_candidate_channel` 码，日志里那句是「N 个候选渠道都不支持这次请求」
——两边说的是同一件事，只是话说给不同的人（见 [api.md](api.md) 的
「错误体的形状」一节）。判据里的输入 token 数是
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
`…/v1/chat/completions/v1/chat/completions`，上游回 `404`。**调用方看不到这个
`404`**：上游的状态码一律不透传（见 [api.md](api.md) 的两条刻意口径），他会拿到
`502` 与 `upstream call failed`——反倒丢掉了「模型不存在」这条线索。

`--models` 是这条渠道支持的**规范模型名**（第一步建的那个），至少一个：
读取路径把 `models` 当白名单用，空数组的渠道永远选不中。
stdout 只有新渠道的 ID，其余信息走 stderr。

**`--setting`：键归适配器解释，选路层不认。** 它是渠道私有差异的归属地
（`channels.setting`，一个自由形态的 JSON 对象）：某个渠道「大体像 OpenAI
但有几处不一样」时，那几处属于这个渠道与它的协议族之间的差别，由**该渠道
`type` 指向的那个适配器**解释；选路层不读这里的任何键——选路要认的判据
（能力、上下文上限）在 `model_catalog` 那边，不从这份 jsonb 里读第二份。
因此管理面**不做键名校验**：拼错一个键不会被拒绝，也不会挡住写入——它在写入
之后与列举时被指出来（见下面两段）。

**今天有两个键有读者。**

`reasoning_effort_map`：读者是 `openai` 与 `openai-responses` 两个适配器，
翻译共用一个函数（`channel.ReasoningEffortDialect`，见
`internal/channel/reasoning_effort.go`），把 `reasoning_effort` 的规范取值翻成
该渠道认的写法；映射到空字符串表示把这个字段从出站报文里去掉，表里没有的取值
照原样发。可照抄的写法（嵌一层对象、值是字符串）见 `larkaxctl channel --help`
与 `docs/cli-reference.md` 的 `--setting` 一节。

`usage`：形状是 `{"usage": {"completion_includes_reasoning": false}}`，它的
`completion_includes_reasoning` 这一位决定上游报的输出 token 里含不含推理。
三个适配器（`openai`、`anthropic`、`openai-responses`）都读它，读法是
`channel.UsageCompletionIncludesReasoning`（见
`internal/channel/usage_setting.go`）。**不写时按「含」算**，与 OpenAI 的
`completion_tokens`、Anthropic 的 `output_tokens` 的定义一致；只有把可见输出
与推理分列报出的上游（例如 DeepSeek）才需要显式写 `false`——不写的话，计费前
减掉推理会让输出那一侧少收，推理比可见输出多时整段输出都收不到钱。这个关系
不能靠数字推断：同一组数字在两种口径下都成立。

其余键没有读者，`systemone` 适配器与选路层也都不读 `setting`，写进去不影响
请求路径。

一个没有读者的键，从库里和清单上看与一个生效的配置完全一样，不说出来就只能
等人去查代码。所以管理面在**写入成功之后**（`channel create`，以及给了
`--setting` 或 `--type` 的 `channel update`）与 `channel list` 上，按这条渠道的
`type` 逐键点名：键名拼错的、别的 `type` 的、这个 `type` 还没接的，都在这句话
里；内层键跟着外层一起点，写成 `usage.completion_includes_reasoning` 这样的
点号路径。判据是 `internal/channel` 的 `settingKeysByType`（类型 → 它认的键，
分顶层与内层两层），它由
同包的用例与实际读键的代码双向核对（见 `internal/channel/setting_test.go`）：
表里声明了却没人在读、或者有人读了而表里没写，两种都会让用例失败。读者按
**目录集合**算：`openai`、`anthropic` 与 `openai-responses` 除了各自的适配器
目录，还要算上 `internal/channel` 本身——适配器共用那几段读键的翻译函数，读键
的那一行在那里。

`--user` 把渠道记在某个用户名下（留空表示平台自营）。它**只写
`channels.owner_user_id`，不做权限检查**：任何存在的用户都可以被写成归属，
而这个用户是不是 `merchant` 没有一处代码会看（见下文「用户的角色」）。
归属是分账的依据——商家作用域的计价规则按它匹配，结算时
`req.OwnerUserID` 跟着渠道走进账本，而不是「他有没有资格」的判据。

**三、配出站映射：这家上游对同一个模型叫什么名字。**

```sh
go run ./cmd/larkaxctl channel map add --all-channels claude-x \
  claude-3-5-sonnet-latest
```

`--all-channels` 表示平台级映射，对所有渠道生效；换成渠道 ID
（`channel map add <渠道> claude-x claude-3-5-sonnet-latest`）则只对那个渠道生效，
并盖过平台级那条。同一个范围里一个规范名只能有一个上游名。旧写法
`alias create --model … --upstream …` 仍可用，会在 stderr 上指向这里，
保留一个发布周期。

**请求时按「选中的那个渠道」翻译**，所以查不到映射时分两种，处置相反：

- **这个规范名一条映射都没配过** → 按同名发出。映射表表达的是「哪些模型的上游
  叫法与规范名不同」，一条都没有的模型不在这个集合里。一条映射都没配的部署
  （最常见的那一类）不受影响。按同名发出去之后，上游若不认这个名字，
  调用方拿到的是 `502 upstream call failed`。
- **配过、但没有一条覆盖选中的渠道** → 这次调用失败，返回 `500`。映射是逐渠道
  配的，某个渠道漏了的时候静默按同名发出去，会表现为「上游说这个模型不存在」，
  把排查方向指向上游。补一条渠道级映射、或者加一条平台级映射即可。

后一种在升级时是**可见的行为变化**：升级前映射在请求路径上完全不生效，配了也
白配；升级后它开始生效，于是「只给部分渠道配过映射」的规范名会在没配的那些
渠道上失败。

`larkaxctl alias resolve --model <名>` 回答的正是「这个规范名在某个渠道上
实际会发出去什么名字」，它走的就是选路那同一条查询（见 `cmd/larkaxctl/alias.go`
里 `runAliasResolve` 的注释）。不带 `--channel` 时只看平台级那条，所以「配了
渠道级映射、没有平台级映射」时它会报错、退出码走「配置缺失」那一档。

**映射上还能配一组参数覆盖**（`--overrides '<JSON 对象>'`）。它改的是**发往
上游的那份请求**，用来给不传某些参数的调用方一个默认值，或者把某个上限钉住：

```sh
go run ./cmd/larkaxctl channel map add --all-channels claude-x \
  claude-3-5-sonnet-latest \
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

覆盖与上游名来自**同一条映射行**：渠道级映射命中时用它那条的覆盖，不会回平台级
那条上再凑一次。

**四、配一条计价规则。** 没有生效的规则时请求会失败，而不是按零价放行。

写规则要记下**这条价是谁定的**，所以 `--by` 必填（收用户 ID 或用户名），
且它填的那个用户**得已经存在于库里**——先建一个：

```sh
go run ./cmd/larkaxctl user create --name alice

go run ./cmd/larkaxctl price add example-price --scope platform --by alice \
  --in 1.5 --out 7.5 --cr 0.2 --cw 1.9 --reasoning 9
```

规则名是 `price add` 的第一个位置参数，写在旗标前面；`--name` 仍然认，`create` 是
`add` 的旧名。五个单价旗标的单位是**每百万 token**，与供应商报价单一致
（`--in 1.5` 就是每百万输入 token 1.5）；存库前换成每 token（`0.0000015`），库里
存的仍是表达式。要直接按 token 给价时加 `--unit token`。这组旗标把五个计价项全写进
表达式，没给的项写 0，`--in` 与 `--out` 至少给一项。带阶梯、分时或门槛的规则仍用
`--expr` 或 `--schedule-file`：三种给法三选一。

缺少 `--by` 时它报参数错（退出码 2），不会按「不记名」落库。

计价项一共五个：`prompt`、`completion`、`cache_read`、`cache_write`、`reasoning`。
**上游报了哪一项，规则里就必须有哪一项的单价**——少一项不是少收，是这一笔的
**结算失败**，因为上游报了我们没定价的用量，宁可报错也不要静默少收。
非流式请求里，调用方拿到的是 `500` 与一句 `internal error`（`500` 这一档不点名
原因，见 [api.md](api.md) 的状态码表）；流式请求的流已经推出去，那次失败只落在
**服务端日志**里，调用方不会再收到错误码。日志里点得出原因的那一句是：

```text
结算失败: 用量里有规则未定价的计量项: "cache_read"（规则里没写这个单价；要免费就显式写 0）
```

会报缓存的上游很常见（Anthropic 的 `cache_read_input_tokens` /
`cache_creation_input_tokens`，OpenAI 的 `prompt_tokens_details.cached_tokens`），
所以**新规则把这五项都写上**、用不到的填 `0`，比漏掉再被结算拦住省事。

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
go run ./cmd/larkaxctl channel map list
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

需要主密钥（`LARKAX_MASTER_KEYS`）的是这几组命令：渠道的**写入**
（`channel create` / `update`）、`plugin delete` / `plugin kv set` /
`plugin kv delete`、以及 `secret rotate`。它们要它的缘由不是同一个：渠道那几条与
`secret rotate` 都要**写凭证**——凭证是信封加密的，只有 `credential.Sealer` 能产出
可解开的密文，直接往库里写一行 `bytea` 补不上；`plugin` 那三条不对任何东西解密，
要的是「跑它的机器带着运维凭证」——把一条删数据的入口开在只有库口令的机器上，
等于把「能不能删」交给连接串。**别把它读成「凡改库都要」**：`settings set`、
`apikey create`、`user create`、`wallet credit` 都改库，都不读它。
它们读的是这个环境变量本身，与 `LARKAX_ROLE` 无关——运维命令不属于任何角色，
把命令的成败绑到角色上，会让 `LARKAX_ROLE=worker` 时明明设好了主密钥却报
「没有读到主密钥」。其余命令没有主密钥也能跑，其中
`channel list` / `enable` / `disable` / `delete` 与 `plugin list` / `get` /
`kv get` 连密钥材料都不需要。

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
所以这条命令不能拿来「给已有用户补点什么」；那件事走 `user update`（只改显式给出
的字段，改了什么按「旧值 → 新值」打到 stdout）。

### 用户的角色（今天只是记下来）

三个角色都是用户，`merchant` 与 `admin` 落成 `users.is_merchant` /
`users.is_admin` 两个布尔标记（`admin` 同时打上两个）。**全仓没有生产代码
读这两个标记**，所以：

- `--role merchant` **不会**让这个用户多出什么权限，`--role admin` 也**不会**
  让他能进管理接口。管理面的鉴权不读这两个标记：它用单独一套管理令牌
  （见 [admin-api.md](admin-api.md)）。
- `channel create --user <用户>` 不检查那个人是不是 `merchant`：归属只决定
  分账记到谁的账上，不决定「有没有资格」。
- 角色能改，但改的也只是这两个标记：`user update` 的 `--merchant` / `--no-merchant` /
  `--admin` / `--no-admin`（`--admin` 同时打上两个，`--no-admin` 不动 `merchant`）。
  `--no-merchant` 不能单独打在 `admin` 上——那样会留下一行「读出来是 admin、`merchant`
  却是假」的数据，当场报参数错；先 `--no-admin`，或者两个一起给，一步落到普通用户。
  `user create` 仍然只建新用户，重名直接报错。

这两个标记今天仍然只是备注，不是访问控制：管理面的令牌**不对应任何用户**，
也不读这两个标记（#158）。把 `--role` 当成一条记录，而不是一条授权。

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

`"model"` 填的是第一步那条**规范名**（这里就是 `claude-x`），不是映射里的上游名
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
`final_amount` 是实际结算。额度包覆盖的请求 `reserved_amount` 是 0（钱包一分没冻），
它占的是订阅的面值与可用量，记在同一行的 `plan_reserved_amount` 与 `reserved_quota` 上。
额度包结算按**预留额封顶**：实际用量超过这次预留时，`quota_used` 只加到预留额为止，
超出部分不计入，同时留一条带 `plan_id`、`actual`、`reserved` 的 WARN——宁可少记，
也不让包的可用量透支；账本那一侧「结算不超过预留」是同一条规则。
按请求查要直接连库，SQL 示例见
[metrics.md](metrics.md) 的「怎么读」。**目前没有一条按请求列出花费的命令。**

控制台的三个聚合视图（`usage/summary`、`usage/series`、`usage/breakdown`）与行级的
`usage/requests` 一样，把额度包面值与 `cost` **并列**输出（字段名
`plan_reserved_amount`）。`cost` 是这笔用量实际收了多少；`plan_reserved_amount`
是预留时占住的面值上界，已随结算释放。两者不合并，也不相加——它们是同一笔消费的
实收与上界，相加会把同一笔钱算两遍。聚合侧读 `usage_events.plan_reserved_amount`，
那一列在结算时由 `billing_requests` 的同名列原样搬过来，不在聚合层重新推算；
迁移之前的已结算行由 00058 按 `request_id` 回填补齐，`billing_requests` 里已经
没有的请求保持 0。

## 检查这条链路通不通

上面两节走完之后，「通不通」由一条命令回答：

```sh
go run ./cmd/larkaxctl doctor
```

它按顺序查九项，每一项都打出「检查了什么、结论是什么」；末尾另有一行「上游连通」，
不加 `--live` 时那一行一直是 `-`（见「真打一次上游」）：

| # | 查什么 |
|---|---|
| 1 | 数据库连得上吗、迁移跑到哪一版 |
| 2 | 有没有至少一条 enabled 且 `models` 非空的渠道 |
| 3 | 渠道 `models` 里出现的每个名字都在 `model_catalog` 里吗 |
| 4 | 配过映射的规范名有没有渠道接得住；可服务的规范名在每个服务它的渠道上能不能解析出上游名 |
| 5 | 可服务的规范名在每个服务它的渠道上都有生效的计价规则吗 |
| 6 | 计价规则与成本规则的 `model_patterns` 里写到的名字都在模型目录里吗 |
| 7 | 有用户吗、每个用户都有钱包行吗、有没有一个钱包余额大于 0 |
| 8 | 入站别名与目录里的规范名同名吗（同名时解析走规范名、这条别名不生效） |
| 9 | 入站别名指向的规范名都还在模型目录里吗 |

第 3、4、5 项回答的是同一个字符串在两个数组里对不上的问题。渠道的 `--models`
与目录里的规范名差一个字符、或者模型建了却没加进任何渠道的白名单时，调用回
`503 no available upstream for the requested model`——那句话不会说是哪一个
名字不对，而 doctor 会点名是哪个渠道、哪个名字。第 6 项问的是同一件事的另一半，
对象换成了规则：写错的模型名让那条规则永远命中不了，而它在任何清单里都像是
配好的。第 7 项把「用户没有钱包行」这类缺口在第一次调用之前指出来，而不是让人
从 `500` 往回推。第 8、9 项看入站别名（`model alias`）：同名时别名永远不生效，
指向的名字不在目录里时用它的请求会在选路处失败。

**「钱包」那一项在「入账」那一步走完之前不通过。** 它要求有一个钱包余额大于 0，
而新开的户余额是 0，所以只开户不入账时 doctor 会以「这一项不通过」收尾——那是
预期结果，不是配置漏了。按上文「入账」写一笔，再跑一次这一项就绿了。

**前九项只读**：不写任何一行、不建任何东西、不发网络请求。

### 输出的形状

默认每项一行，末尾一行汇总：

```text
larkaxctl doctor：逐项检查「配好的链路能不能接一次请求」。

  ✓ 数据库        连接正常（迁移 v23）
  ✓ 渠道          enabled 且 models 非空的渠道 1 条
  ✗ 模型目录      渠道 1（channel-a）的 models 里有 "model-x"，但目录里没有这个名字
  ! 别名          没有可服务的规范名
  ! 计价规则      没有可服务的规范名
  ✓ 规则模型名    2 条规则、3 个模型名都在目录里
  ✗ 钱包          用户 1（alice）没有钱包行
  ✓ 入站别名遮蔽  1 条入站别名里没有被同名规范模型遮蔽的
  ✓ 入站别名悬空  1 条入站别名的规范名都在目录里
  - 上游连通      没跑：加 --live 才会真打一次上游（会花钱、要凭证）

  doctor: 2 项异常, 2 项跳过, 5 项通过 (运行 -v 查看细节)
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
| `--base-url <URL>` | 网关公共入口的地址。不给时按配置里的 `LARKAX_HTTP_ADDR` 拼（那个变量写着 `127.0.0.1:8081` 就打成 `http://127.0.0.1:8081`；写成 `:8080` 这类不写主机的「所有网卡」时按 `127.0.0.1` 打），网关不在本机时要显式给 |
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

`0` 九项都通过（给了 `--live` 时那一次也成功）、`1` 有问题或某一项没跑成、
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
`insufficient_quota`，见 [limits.md](limits.md)）。**落点有两处**——入口在读请求体
之前的早拒绝（`internal/ledger/store` 的 `Store.CheckQuota`，它转给
`internal/keyquota` 的 `Check`），以及预留那个事务里的权威判定
（同一包的 `CheckInTx`）。

两处共用的是两样东西：读「这把 key 累计花了多少」的口径（`spentSQL`），
以及「算不算超」那条判据（`over`）；它们不是两条各算各的规则。
差别只在早拒绝那一支：它传 `additional = 0`，也就是「已经超了才拒」，
所以它只会比权威判定更松，不会拒掉一个权威判定会放行的请求。

## 当前能做什么

**推理已经接到公共端口上，流式与非流式都走通了**：

| 端口 | 端点 | 现在返回 |
|---|---|---|
| `:8080` | `POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses` | 推理；`stream: true` 时是 SSE 流；没有有效 API Key 时 `401` |
| `:8080` | `POST /v1/systemone` | 决策调用，见下节；**从不流式**，带了 `stream: true` 回 `400` |
| `:8080` | `GET /v1/models`、`GET /v1/models/{id}` | 这把 key 当前可用的模型清单；形状见 [api.md](api.md) 的「模型清单」；没有有效 API Key 时 `401` |
| `:8080` | 其余任意路径 | `501`，明确的「尚未实现」JSON |
| `:9090` | `/healthz` | `200`，进程存活 |
| `:9090` | `/readyz` | `200`，含 `postgres` 与 `tokens` 两项探针（worker 角色只有 `postgres`：它没装计数器） |
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

两处给的是同一个形状，即 Responses 协议本身定义的那一份 url_citation：`url`、
`title`、`start_index` 这些字段与 `type` 并排。非流式那条路上游过来的那一份，
先被转换层折成了两层。那是 chat 那套非官方扩展的形状，
见 relayconvert 的 `responseAnnotationToChat`；`internal/api` 在渲染响应体与
流式事件时摊平（见 `responsesAnnotation`），所以客户端按一个形状写解析就够。

要真的拿到一次 `200`，库里得先有：一个用户与钱包、一把 API Key
（`larkaxctl apikey create --user <ID 或用户名>`）、一条指向上游的渠道、
一条适用的计价规则。这四样现在都有管理入口——模型、渠道、别名与计价规则
见上文「配一条能接请求的链路」，用户与 API Key 见下文。没有计价规则时请求会
失败而不是按零价放行。

`501` 是有意的占位：推理路径与模型清单之外的任何请求都会立刻得到明确答复，
而不是超时或误导性的 404。这样部署验证能确认端口通、TLS 通、负载均衡转发通。

启动时会连数据库并加载 `settings`。连不上或配置对不上就拒绝启动，
`/readyz` 也会在数据库不可用时返回 `503`。

内部端点单独一个端口：它面向编排系统与指标抓取器（`/healthz`、`/readyz`、
`/metrics`），不和业务入口共用监听地址，也就不需要跟着业务入口一起暴露在
ingress 规则下。同一个端口上还有管理面 `/admin/v1/*`，用单独一套管理令牌鉴权，
接口面与令牌怎么发见 [admin-api.md](admin-api.md)。

运维命令行走另一个入口（`cmd/larkaxctl`），目前有这些：

| 子命令 | 做什么 |
|---|---|
| `channel` | 上游渠道：`create` / `list`（`--all` 含已删）/ `get`（单条详情）/ `health`（健康与窗口内的用量）/ `update` / `enable` / `disable` / `delete`（软删）/ `restore`（`delete` 的逆操作）/ `sync-models`（从上游拉一次模型清单并落库）/ `upstream-models`（读库里那一份清单与同步状态）/ `adapters`（列出适配器类型）/ `probe`（落库之前探一次上游模型清单）；出站映射：`map`（`add` / `list` / `remove`） |
| `model` | 模型目录：`create` / `list` / `get` / `update` / `delete` / `onboard`（一个事务里写完目录、渠道白名单、出站映射与两条价格规则）；入站别名（调用方写的名字 → 规范名）：`alias`（`add` / `remove` / `list`）；名字链路诊断：`trace` |
| `alias` | 规范模型名到上游模型名的映射（旧入口，配置改用 `channel map`）：`create` / `list` / `update` / `delete` / `resolve` |
| `price` | 计价规则：`add` / `list` / `show` / `update` / `enable` / `disable` / `explain`；兼管分时计价的日历数据：`calendar`（`import` / `sync` / `list`） |
| `upstream-cost` | 上游成本规则（每条渠道每个模型按生效窗口与优先级挑一条）：`create` / `list` / `update` / `disable` |
| `user` | 用户：`create` / `list` / `get` / `update`；全景：`inspect`（一个只读事务取齐账号、钱包、订阅、扣费顺序与 Key）；口令：`password` / `unlock`；销户：`delete`（`request` 打标可撤 / `revert` 撤回 / `purge` 匿名化，不可逆）；用户级履约操作：`plan`（`grant` 发放 / `list` 订阅 / `void` 作废 / `order` 扣费顺序的 `show` / `set`）、`wallet`（`show` / `credit` / `adjust` / `refund`）、`key`（`list` / `create` / `plans` 的 `show` / `set`）、`usage`（`events` / `requests`） |
| `wallet` | 用户钱包：`create`（余额 0）/ `credit`（手工入账，走账本写分录）/ `adjust`（手工调账，走账本写分录）/ `refund`（退款，走账本写分录）/ `list`；商户提现：`payout`（`request` 申请并冻住待付 / `settle` 平台主动清算，一步建单并记账，直接落已批准 / `approve` 批准 / `reject` 驳回并解除冻结 / `paid` 打款，只放行已批准的单子）；支付通道：`payment`（`create` 下单 / `status` 查单，通道确认收款后才入账） |
| `usage` | 用量与计费（只读）：`events`（用量事件）/ `requests`（每次请求的预留与结算）/ `summary`（按用户汇总） |
| `apikey` | API Key：`create` / `update` / `get` / `list`；`update` 兼改状态、备注与过期时间 |
| `plan` | 额度包（商品定义）：`create` / `list` / `get`（单个包的详情）/ `enable` / `disable`（只挡新的发放）。用户持有的订阅、Key 的额度包名单与扣费顺序搬到了 user 子命令那一组；旧写法 `grant` / `void` / `subscriptions` / `key`（`show` / `set`）/ `order`（`show` / `set`）仍可用，命中时在 stderr 提示新路径 |
| `ledger` | 账本：`balance` / `verify` / `runs`（三条都只读：余额、逐笔校验、对账的历史运行）与 `reconcile`（对账，会写运行记录） |
| `settings` | 运行期配置：`list` / `get` / `set` |
| `admin` | 内部监听上 admin API 的令牌：`token`（`create` / `revoke`） |
| `plugin` | 插件：`list` / `get` / `publish` / `enable` / `disable` / `delete`；插件私有的键值存储：`kv`（`get` / `set` / `delete`） |
| `partition` | `usage_events` 月分区：`ensure` / `check` |
| `doctor` | 逐项检查这条链路通不通（`--live` 才真打上游） |
| `snapshot` | 请求快照：`cleanup` / `allowlist` / `get` / `access-log`（只读的访问记录）/ `redact` |
| `consent` | 原样采集请求正文的授权：`grant` / `list` / `revoke` |
| `secret` | 主密钥轮换：`rotate` |
| `maintenance` | 运行期数据的留存清理：`idempotency` / `session`（两条都支持 `--dry-run`，只数不删） |
| `migrate` | 迁移：`up` / `down` / `down-to` / `up-to` / `status` / `version` / `create` |
| `version` | 打印版本号 |
| `completion` | 打印 bash / zsh / fish 的补全脚本 |

每个子命令的用法见 `go run ./cmd/larkaxctl <子命令> --help`，
或直接 `go run ./cmd/larkaxctl` 看总览。对账的开关、口径与运行记录见
[reconciliation.md](reconciliation.md)，日历数据的文件格式见
[calendar-data.md](calendar-data.md)。

两个端点都认请求头 `Idempotency-Key`：带上它时同一个键重复发送只执行一次，
第一次的结果被原样重放。用法、三条 `409` 的码与留存规则见
[idempotency.md](idempotency.md)。

## 决策接口（`/v1/systemone`）

`POST /v1/systemone` 是第四条**推理**路径（公共入口上另有两条只读的
`GET /v1/models`，见 [api.md](api.md) 的「模型清单」）。它不做对话，而是拿一段
**状态**（一段自由文本，比如一张工单）与一组**带类型的判定问题**，回一组答案。
与另外三条推理路径一样：协议由路径决定，计费走同一条两阶段流水线，上游由
选路层按渠道挑。它的错误体用 OpenAI 形状、不支持流式，两条都在
[api.md](api.md) 里。入站别名在这条路径上同样解析，但位置与 chat 那条不同：
这条表面没有幂等，所以不存在「解析要排在指纹之前」的约束，解析排在请求校验
之后、组装计费请求之前（见 `serveDecision` 的注释）。

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
bash scripts/check-docs-index.sh    # docs/README.md 的分组表、development.md 的 TOC 与正文对得上
bash scripts/check-man-pages.sh     # man/man1 下的手册与命令注册表、cli-reference.md 对得上
bash scripts/check-mutation-table.sh # 「各包的现状」那张表与 PKGS 的包名集合一致
bash scripts/check-migration-order.sh # 分支新增的迁移编号必须大于 base 上的最大号
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
`check-test-names.test.sh`、`check-layering.test.sh`、`check-mutation.test.sh`、
`check-mutation-table.test.sh`、`check-migration-order.test.sh`、`check-lessons-index.test.sh`、
`check-docs-index.test.sh`、`check-man-pages.test.sh`。
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

`scripts/check-docs-index.sh` 管的是 `docs/` 下那两处索引与正文的一致性：
[README.md](README.md) 的「想做 X → 看 Y」分组表 ↔ `docs/` 下的文件，以及
`development.md` 顶部的 TOC ↔ 文中那些标题。判据三条：每一份文档都在索引里
（正文 → 索引）；索引里每条指向本目录 `.md` 的链接都有对应的文件（索引 → 正文）；
TOC 的每个锚点在文中都有对应标题。前两条是一对，只留一条时另一类错照样漏：
新写的文档没进索引、或者索引里链了一份已经不存在的文档，两种都是「点进去 404」。
它同样只依赖 bash 与 coreutils，所以也放在那个 job 里。

哪些标题该进 TOC 是取舍；索引分组的措辞好不好、指向仓库别处的链接（`../…`）
有没有写错，都不在它里面——这些在脚本开头的「已知边界」里逐条写着，不是漏判。

`scripts/check-mutation-table.sh` 管的是「各包的现状」那张表与 `PKGS` 的包名集合
是否相等。表不是 `PKGS` 的生成物，而是手工记录：加了包进清单却没补行，那个包从此
被量而数不在任何地方；表里有行而清单里没有，那一行的数是手工跑的，脚本永远不会
再量它，下一次改动之后它停在旧值，读表的人却以为随时能复现。两种漂移都不会让别的
测试失败，所以单独一道门。顺序不参与比较——表按 efficacy 排、清单按优先级排。
两边任一解析出 0 条时退出 2（「没跑成」），不报通过；每次运行都打印解析出的条数。

`scripts/check-man-pages.sh` 管的是 `man/man1/` 下的手册与它们的来源是否同一次生成
的。手册是**产物**：名字、分组与一句话来自命令注册表（`cmd/larkaxctl` 的
`commandGroups`），长文本来自 `docs/cli-reference.md`，由 `internal/manpage` 渲染成
roff，生成一次提交进库。产物与来源分家的样子是安静的——改了注册表里的一句话或
文档的某一节却没重新生成，库里的手册仍然说着旧话，而 CI 里不会有任何一步失败，
只有照着 `man larkaxctl-channel` 敲的人才会撞上。形状照抄 cargo 的 `validate-man.sh`：
重新生成一遍，与提交的那一份逐页比对，对不上就红。

改了那两处源文件之后，重新生成并提交：

```sh
scripts/check-man-pages.sh --update
```

翻一份源码树里的手册（`man` 只认 `MANPATH` 下的 `man1/` 这类子目录，所以指到 `man/` 那一层）：

```sh
MANPATH="$PWD/man" man larkaxctl-channel
```

它判四件事：生成器产出的每一页在 `man/man1/` 下都有一份逐字节相同的；库里这一族
的每一页都由生成器产出；每页非空且以 `.TH` 开头；手册里汉字与拉丁字母之间没有被
吃掉的空格（那一格几乎总是接硬换行时丢的，判据在 `internal/manpage` 的
`needsSpace`，数字与全角标点相邻不算）。最后一条是唯一一条判「对不对」的：
前三条判「同不同」，产物与来源一致时它们永远绿，而少一个空格的手册正好是一致的。
确实该紧贴的写法进 `scripts/man-pages-allowlist.txt`，条目只许减不许增。

它的成功信号也是沉默，所以脚本每次都打印证据行：比对路径两行（逐页比对几次、
紧贴扫出几处），`--update` 一行（紧贴几处，另有一句说写进去几页）。
生成器非零退出、一页都没写出来、写出空页或没有 `.TH` 的页、剥完 roff 转义后
还有不认识的转义、紧贴扫描器自己出错时判「没跑成」（退出码 2），不报通过。

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

CI 跑的是同一批门；真正的变异测试另有一个只对搬钱的代码跑的 job，见下文「CI 跑什么」。

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
`check-layering.test.sh`）、红线扫描及其自测、变异测试脚本的自测、
变异清单与文档表格一致性门禁及其自测（`check-mutation-table.sh`）、
迁移编号顺序门禁及其自测（`check-migration-order.sh`：本分支新增的迁移号必须
大于 base 树里的最大号，否则号小的一条永远上不了已经升过级的库）、
不可达符号门禁
及其自测（`check-deadcode.sh`）、整包没人 import 的门禁及其自测
（`check-unimported.sh`）、导出方法调用点门禁及其自测（`check-deadmethods.sh`）、
测试函数名门禁及其自测（`check-test-names.sh`）、索引与正文一致性门禁及其自测
（`check-lessons-index.sh`）、文档索引与正文一致性门禁及其自测
（`check-docs-index.sh`）、手册同步门禁及其自测（`check-man-pages.sh`）、
非 UTC 时区下的时刻门禁（`TZ=Asia/Shanghai` 再跑一遍
受影响的包，见 [cli-output.md](cli-output.md) 的「时刻与时区」）、契约测试（见下）。

`Go` job 在单测之前装 `fish`、`zsh` 与 `bash-completion`：`sudo apt-get install -y`
之后再各打一次 `--version` 自证装上，`set -euo pipefail` 让 apt 失败当场红；bash 自身不用装，
runner 镜像自带。bash-completion 是 `make install` 用例要的：它靠那份脚本的按需加载证明
「文件放在用户目录下就会被认」，没有它就只能退化到 source 装好的那一份。单测那一步设
`LARKAX_REQUIRE_COMPLETION_SHELLS=1`——补全的行为用例
与 `make install` 的端到端用例（`TestMakeInstallWritesShellCompletions`，它会跑一次
真的 `make install` 再从装好的目录取候选）
要在真 shell 里取候选，本地没装壳时可以只记一行日志放行，CI 装了壳还找不到就必须
硬失败：装壳那一步没生效时，放行会让整条链全绿（见 [LESSONS.md](../LESSONS.md) 第 16 条）。

前端另有一个 `Web` job，与 `Go`、`契约` 并列，在 `web/` 下依次跑：
`npm ci` → `npx tsc -b` → `npx vitest run` → `npx eslint src --max-warnings 0`。

- `npm ci` 按 `web/package-lock.json` 精确安装，lock 与 `package.json` 对不上时
  这一步就失败（`npm install` 会改写 lock，于是「锁文件可用」这条判据就没了）。
  依赖缓存的键也是这份 lock。
- 顺序是有意的：类型检查最先跑，它挂了之后的用例结果与 lint 结果都不必再看。
- `vitest` 那一步除退出码外，还要从汇总行里取用例数：必须读到「Tests N passed」，
  且跳过数为零。默认 reporter 的这一行是「跑了多少」的唯一正面证据；它不在输出里
  （reporter 换了、输出被截断）时，退出码不能说明用例跑过（见
  [LESSONS.md](../LESSONS.md) 第 16 条）。计数会打印在日志里，
  「0 条跑过」与「全部通过」因此分得开。
- `eslint` 带 `--max-warnings 0`：警告留着不管，下一次「只加一条警告」就没人拦。
  `web/src/pages/ops` 下的组件与共享函数已按这条规则分成两个文件
  （`common.tsx` 与 `common-helpers.tsx`），没有文件级豁免。

耗时量级（本地一次完整跑；CI 上的实际耗时以那次运行的日志为准）：
`npm ci` 约 10 秒（依赖缓存热的量级）、`tsc -b` 约 20 秒、
`vitest run` 约 40–75 秒、`eslint src` 约 10 秒。

**变异测试另有一个独立的工作流**（`.github/workflows/mutation.yml`），它跑的是
真的 gremlins，不是脚本自测。**它只对搬钱的代码自动跑**：哪些目录算搬钱，由
`scripts/check-mutation.sh` 里的 `MONEY_PKGS` 回答——账本、计费、金额、算出结算额、
给出单价，以及 `internal/plans` 的结转算式。工作流用
`scripts/check-mutation.sh --list-packages --money` 读这份清单，自己不另列前缀：
两处各写一份，就会有一个包「改了钱却不量」，而界面上与「不需要跑」长得一样。
这个范围之外的改动不自动跑：全清单一轮以小时计，而 `PKGS` 有三十多个包；
让每个 PR 都自动跑一遍会把公开仓库的 runner 队列占住，而大多数改动与钱无关。
要量别的包时，用 `workflow_dispatch` 手工给 `packages`。PR 自动轮还**按改动
收窄**：只变异 PR 改到的那些生产文件，口径与分数怎么读见「变异测试」一节。

工作流**不重写开关与解析**：它调用的还是 `scripts/check-mutation.sh`，所以
gremlins 版本、`workers=1`、系数 10、`--invert-logical`、每个包之前清一次测试
缓存、Redis 依赖的判定，与本地是同一份实现，数字可以直接与本地对账。**选包的
匹配规则也只写在脚本里**：改动目录交给 `scripts/check-mutation.sh --list-packages
--match <目录>`，由脚本取「最近的、在 `PKGS` 里的祖先」——包路径是递归的，所以
`internal/billing/calendaradapter`（真实计费包，但不在 `PKGS` 里）由
`internal/billing` 那条命令覆盖。工作流自己不写「目录 == 清单条目」这类判断：
两处各写一份，刚出生就会漂成一个包都没量到。

选包那一步在**每个 PR** 上都跑（几秒钟，不装工具、不起数据库），为的是让「这一轮
不跑」看得见，并且**说清是哪一种不跑**：改动不涉及 `MONEY_PKGS` 里的目录；或者
改动在那些目录里、但没有任何 `PKGS` 条目覆盖。后者是兜底而不是常态——`MONEY_PKGS`
的每一条都有 `PKGS` 条目覆盖，这一点由 `scripts/check-mutation.test.sh` 的结构断言
核着（前缀没有覆盖时必须变红）；只有将来某条前缀在清单里失去覆盖时才会出现，那时
summary 会列出那些目录，并提示把覆盖它们的包加进 `PKGS`。两种情形下变异 job 都显示
为 **skipped**，不是「跑了且通过」。草稿 PR 不跑——还在改的代码不该占着 runner 跑
几小时；工作流订阅了 `ready_for_review`，标成 ready 的那一刻就会触发一轮。

**变异在 CI 上跑是常态，本地变异只作保底。** CI 在独立的 runner 工作区里跑，
它的锁与本地不是同一个文件。本地那把锁按**克隆**一级：同一克隆的各棵工作树共享
`<git 公共目录>/tmp/check-mutation.lock`，两个工作树同时起变异时，后起的那个会
报「另一个实例正在跑」并以**退出码 3** 停下——同一克隆内的互斥由脚本保证，不靠
记性。仍然要人工错开的是**同一台机器上的不同克隆**：它们各有各的锁，互相看不见。
真的并行起来时后果是超时判据失真，而失真的那一格在表上看起来与真实结论一样。

这个 job **不替代本地的那批门**：它只跑变异，`gofmt`、`go vet`、
`golangci-lint`、各扫描器与契约测试仍然在「门禁」那条流水线里。反过来，
本地开 PR 前先跑一遍门禁也不跑变异——两边各管一头，不是二选一。

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

**本仓库不做部署**：`main` 合入即是流水线的终点。发布由 tag 触发 `release.yml`——
门禁 → `make release`（二进制包 + 控制台包）与镜像推送 → 把资产传到 GitHub Release。
制品（二进制、控制台包、镜像）放到哪台机器上属于使用方的事，不在本仓库范围内。

镜像由仓库根的 `Dockerfile` 定义：两个构建阶段（Go 与 Node）都跑在构建机架构上，
二进制按 `$TARGETARCH` 交叉编译，所以一个 amd64 的 runner 就能同时出 amd64 与 arm64
两个镜像，不需要 qemu。镜像里的二进制与页面分别来自 `make print-ldflags` 和
`make web-build`，不另写一份。本地试构建：

```sh
docker build -t larkax:dev .
docker run --rm -p 8080:8080 -e LARKAX_DB_DSN=... -e LARKAX_MASTER_KEYS=... larkax:dev
```

另有一个**手动触发**的构建工作流 [.github/workflows/dist.yml](../.github/workflows/dist.yml)：
它只把某一棵树打成产物——`make dist` 出的二进制包与 `make web` 出的控制台包——
通过 workflow artifact 取，**不进 Release，也不部署**。它**不跑门禁**：门禁在
push 与 PR 上已经跑过，触发前先确认那个提交是绿的。要「门禁 + 构建 + 发布」一条龙，走
tag 与 `release.yml`。控制台那一格调的也是 `make web`（与发布同一个目标），
`VITE_API_MODE` 只是它的一个参数。

控制台那份产物的 `VITE_API_MODE` 由工作流的 `api_mode` 输入决定，**默认 `http`**（连真实
API）；`mock` 只用来出演示包。这一格默认值反了会很隐蔽：演示界面上有横幅，但登录页、菜单
与数字都长得像真的，打错要等部署之后才发现。工作流里另有一道检查，`api_mode=http` 时会
确认产物里不含只有 `MockApiClient` 才有的字符串。

## 变异测试

改了**计费、账本、金额**这三处代码时，除了单测还要跑变异测试。额度包的发放与
作废搬的也是账本上的钱（`internal/plans`），同样归在这一条线上。覆盖率只说明
「这一行被执行过」，不说明「这一行的每个分支被钉住了」——把 `>=` 改成 `>`、
把分支方向反过来，覆盖率一点不变，而测试照样通过。

```sh
export LARKAX_DB_DSN="postgres://larkax:placeholder@127.0.0.1:5432/larkax?sslmode=disable"
export LARKAX_REDIS_DSN="redis://127.0.0.1:6379/0"   # 测试依赖 Redis 的包才需要
scripts/check-mutation.sh              # 全部包（整包口径）
scripts/check-mutation.sh money route  # 只跑指定的
```

**按改动收窄**（只变异这次改动涉及的生产文件）：

```sh
scripts/check-mutation.sh --changed-from origin/main
scripts/check-mutation.sh --changed-files changes.txt internal/ledger
```

`--changed-files` 读一份仓库相对的路径清单（一行一个，`-` 表示 stdin）；
`--changed-from` 从 git 现算（参照到工作树，所以未提交的改动与还没被跟踪的
新 `.go` 文件都算进来）。不给这两个开关时跑整包——本地默认与 CI 手工触发
都是这个口径。

收窄的做法是**排除没改到的**：每个包里没有出现在清单里的生产 `.go` 文件，
逐个作为 `--exclude-files` 传给 gremlins。一个包里只有 `_test.go` 变了时
不排除任何文件、跑整包：这种改动的风险恰恰是测试被改弱，排除文件会把要看的
地方排除掉。新文件在清单里就算「改到」，不会被排除。改动同时落在父子包时
只留外层（外层那条命令递归含子包）。

**同时改了生产文件与测试文件时按生产文件收窄，这是一个有代价的取舍**：
被改的那个测试所针对的文件如果本身没变，这一轮就不会覆盖它——测试被改弱时，
它原本保护的那段代码不会跟着重跑。退回整包的条件是**该包里没有任何生产文件
改动**（例如清单里只有 `_test.go`），不是「只改了测试文件」——清单里一条落在
这个包的 `.go` 都没有时，同样跑整包。

**收窄与整包不是同一个口径，分数不能互相比较。** 只变异了一部分文件时，
Killed、存活、超时、Not covered 都只覆盖参与变异的那些文件。脚本会在汇总表
前面逐包打出参与 / 排除了几个生产文件，并写明这一轮不可与整包基线对账；
它与下面「各包的现状」那张整包基线表对不上是正常的，不是回归。

包参数**按后缀匹配脚本里那份包清单**（`scripts/check-mutation.sh` 的 `PKGS`），
照清单里的名字写，别照目录树猜：`PKGS` 是手工维护的，不等于目录树——
`internal/calendar/store` 的父包 `internal/calendar` 在清单里、它自己不在，
所以 `scripts/check-mutation.sh internal/calendar/store` 会**一条都不选中**，
几秒钟就结束。认出这种情况的判据是那句「没有选中任何包」——看到它就说明
**这次什么都没检查**，哪怕屏幕上还有一张汇总表的表头。

**只漏掉一部分时也会说。** 匹配到的那几包照常跑、照常出表，而没匹配到的参数
会被列出来，退出码为 1。表里的行数少于给出的参数个数时，不必自己数：脚本会点出
少的是哪一个（提示里带着「没有匹配到任何包」，收尾还会再点一次）。**子包名不会
被自动归到父包**——那会把「名字写错」变成「悄悄跑了另一个包」。

**`LARKAX_DB_DSN` 是硬前提，不是可选参数**：没有它 `internal/ledger/store` 的
集成测试整包被 `t.Skip`，那一包等于没测。脚本会直接拒绝运行，而不是跑出一批
不可比的数字（见 [LESSONS.md](../LESSONS.md) 第 17 条）。

**`LARKAX_REDIS_DSN` 同理，但只对测试依赖它的包生效**：被选中的包里只要有测试
文件引用了这个变量，它就是那一轮的前提。没有它时那些集成测试整组被 `t.Skip`，
只在它们里碰得到的变异体落进 Not covered——不进 efficacy 的分母，分数只会变好看。
脚本同样拒绝运行，并把要它的包列出来。判据是「测试文件里有没有引用它」，
不是一份包名清单：加了新测试不用回来改这里。

**跑起来之后是逐行输出，不是等到结束才出结果。** 每行 gremlins 输出前面带序号与
已用时间；每完成 20 个**变异体**打一行进度（`PROGRESS_EVERY` 可调，0 表示不打）。
进度只数变异结果行，不数输出行——行数里混着 `Starting...`、gathering 与最后的汇总，
拿它和变异体总数相减算出来的剩余时间没有依据。ETA 只在 gremlins 自己报了总数时才算；
v0.6.0 正常运行不报总数，所以实际看到的是「已完成 + 已用 + 平均每个」，没有 ETA。
**不为了打一个剩余时间编一个总数**：编出来的剩余时间比没有更误导。

实测：覆盖率很高的包照样有**存活**变异（逐包见「各包的现状」那张表）——它们集中在
「恰好相等」「恰好为零」这类边界上，而写测试时的自然倾向正是跳过这些。

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
`gremlins` 命令**：串行慢，但变异只对搬钱的改动跑（CI 里也只在 `MONEY_PKGS`
里的包变动时自动跑），**慢而可信比快而误导好**。

### CI 里怎么跑

`.github/workflows/mutation.yml` 在 PR 上自动跑，触发范围由脚本里的 `MONEY_PKGS`
决定（账本、计费、金额、结算额、单价，加上 `internal/plans` 的结转算式；理由见
「CI 跑什么」）。
工作流从脚本的 `PINNED_GREMLINS_VERSION` 读版本再安装，脚本装完自己校验一遍；
它调用同一个 `scripts/check-mutation.sh`——所以「CI 与本地是同一把尺子」不是
靠两边记得同步，而是只有一份实现。

数字仍然要连着条件读：CI 的机器与本地不同（CPU、磁盘），超时列按下面的判据读；
`Not covered` 与 `Mutator coverage` 不随机器变，可以直接与本地对账。

**PR 自动轮按改动收窄**：选包那一步把改动的 `.go` 文件清单传给变异 job，脚本
排除包里没改到的生产文件（规则见上面「按改动收窄」）。所以同一个包在 PR 上
跑出来的数字是「这次改动涉及的那部分代码」的分数，要看日志开头那段「按改动
收窄」列出的参与 / 排除数，别与整包基线对账。`workflow_dispatch` 手工点名
不带收窄：手工触发要量的是整个包。

PR 同时改到父包与子包时（例如 `internal/ledger` 与 `internal/ledger/store`），
选包只留外层：外层那条命令递归就包含了子包，两个都跑会把子包跑两遍。

两个入口对父子对的处置不同：**本地显式传父子对会报错退出**（提示只留外层），
**CI 手工触发传了父子对则在选包那一步收敛成外层并打一条 notice**。跑起来的包是
同一组，差的是入口——本地那个更严格，因为它同时是「人手敲参数时防写错」的那道。

手工指定包：GitHub 页面上 Actions → 变异测试 → Run workflow，`packages` 里填
逗号分隔的包名（例如 `internal/money,internal/pricing`）。它按 `PKGS` 的后缀
匹配，与本地参数同一规则；一条都没匹配到就在选包那一步报错，不会起数据库。
**不会默认跑全清单**：不给 `packages` 就不跑。

单包从几分钟到几小时量级（变异体数见「各包的现状」），job 上限 330 分钟
（平台上限是 6 小时，留 30 分钟给 checkout、装工具与 service 启动）。
一次超过两个包时，选包那一步会给出警告——拆成几次跑，别让它撞上上限。
超时与被同一 PR 的新 push 取消，结论都是「没跑成」：job 的状态分别是 failure
与 cancelled，不会是 success。

### 一个分数要连着它的条件一起读

固定了 workers 还不够。efficacy 的分母只是**被测到的**那些变异体，而「被测到多少」
取决于跑的是哪些测试；同一个包换了条件就不是同一个数。所以脚本的汇总表除了
killed / 存活 / 超时 / efficacy，还打 **Not covered** 与 **Mutator coverage**
（这两个数 gremlins 本来就在打，以前只是没被抄进表）。

**只看 efficacy 会把「没被碰到」读成「被钉住」。** `internal/keyquota` 那一包是
活例——8 个变异体，本包用例只碰到 `over()` 这个纯函数：

```
Killed: 2, Lived: 0, Not covered: 6
Test efficacy: 100.00%
Mutator coverage: 25.00%
```

那个 `100.00%` 说的是「`over()` 的两个变异体被杀掉了」，不是「这一包被钉住了」。
两栏都照实打、都不进退出码：判据仍然只看存活与超时，覆盖低不是失败，
是「这一包还有多少没测」。

**`Not covered` 会把「没被执行到」与「执行了却不在任何块里」混在一起。** 覆盖块
从 `case` 体开头的 `{` 起算，条件那几列不属于任何块，于是 `switch` 的 `case`
条件即使天天被执行也记成未覆盖。`internal/catalog` 那 40 条里 19 条是这一类
（`aliasWriteError` 的五个 `case` 条件、`pickAlias` 的两个）：把这些条件上的
`&&` 改成 `||`，整包照样红。读这一列时先看它集中在哪——整函数 0 覆盖是缺口，
`case` 条件多半不是。

**覆盖范围说的是「哪些测试参与」，与判据是两件事。** 默认口径是**被测包自己的
测试**，脚本用的就是默认——`--coverpkg` 与 `--integration` 都不传。这组实验调用
（`internal/keyquota`，workers=1、系数 10、`--invert-logical`，库建好并迁移过、
DSN 有效）说明单独传哪个都不改变本仓的结果，只有两个一起才把别的包的测试纳进来：

| 开关 | Gathering coverage | Killed / Not covered | Mutator coverage |
|---|---|---|---|
| 默认 | 219ms | 2 / 6 | 25.00% |
| `--coverpkg=./...` | 269ms | 2 / 6 | 25.00% |
| `--integration` | 34.16s | 2 / 6 | 25.00% |
| `--integration --coverpkg=./...` | 37.85s | 8 / 0 | 100.00% |

**两个开关管的是两件不同的事。** 看 gremlins 在 gathering 阶段实际调的命令行就清楚
了——这是用记日志的 `go` 包一层抓到的，gremlins 按 `PATH` 找 `go`：

```sh
# 不传 --integration
go test -cover -coverprofile … ./internal/keyquota/...

# 传了 --integration
go test -cover -coverprofile … ./...

# 两个都传
go test -coverpkg ./... -cover -coverprofile … ./...
```

**`--integration` 管「跑哪些测试」**：从这一个包变成整套（实测 33.16s，热缓存下
`go test ./...` 整套 48 个包是 32.81s）。**`-coverpkg` 管「把哪些包的行计进覆盖」**：
少了它，别的包执行到 keyquota 的那几行不算在 keyquota 头上，所以跑全了还是 6 条
`Not covered`。

那 6 条守卫（`Spent` 与 `CheckInTx` 里的 `err != nil`）整个测试集**确实执行得到**：
`go test -coverpkg=./internal/keyquota/ ./...` 给出 `over 100.0% / Spent 71.4% /
Check 83.3% / CheckInTx 68.8%`，而这个包自己的测试只有 3.3%。但那条路走不得——
最后一个开关组合每个变异体跑一遍整个测试集，本仓一套上百个变异体，代价不现实。
**这一节要解决的不是「范围窄」，而是「范围窄这件事在表上看不出来」。**

**`./<pkg>/` 是递归口径，这一条最容易被读错。** `./internal/ledger/` 一次同时变异
`internal/ledger` 与子包 `internal/ledger/store`：同一轮的输出里同时有
`account.go:行:列` 与 `store/reconcile.go:行:列` 两种位置。所以汇总表上
`internal/ledger` 那一行读作「ledger + store」。

**父子两条一起写成参数会被拒绝。** 递归已经包含子包，两个都传就把 store 的几百个
变异体跑两遍（实测第一包已经出了汇总行，第二包才开始重跑同一批）。脚本在这种参数
出现时报错退出，列出谁包含谁并提示只留外层。**不带参数跑全部包是例外**：那是基线
口径，汇总表本来就按「父包那一行含子包」来读，所以它照常跑，只打一行注记。

**这与「选中了哪些包」是两件事。** 包参数按后缀匹配清单（照清单里的名字写，
别照目录树猜）：传 `internal/ledger` 只选中清单里那一条，**不会**顺带选中
`internal/ledger/store`；想单独量 store 就得把 `internal/ledger/store` 自己写上。
递归管的是「变异哪些文件」，选包管的是「跑清单里哪一行」。

DSN 改的正是**递归进来那一半**。实测（两轮都是手工裸 gremlins：
`gremlins unleash --timeout-coefficient=10 --workers=1 --invert-logical ./internal/ledger/`，
库先建好并 `migrate up`，两轮只差 DSN）：

| 条件 | Killed | Lived | Not covered | Test efficacy | Mutator coverage |
|---|---|---|---|---|---|
| `LARKAX_DB_DSN` 有效 | 476 | 9 | 92 | 98.14% | 84.06% |
| 不设 `LARKAX_DB_DSN` | 195 | 6 | 376 | 97.01% | 34.84% |

两轮的变异体总数都是 577；两轮的 `Not covered` 是**包含**关系——A 轮那 72 处位置
每一处都在 B 轮的清单里（B 轮 341 处）。`internal/ledger` 自己的文件
（`account.go`、`entry.go`、`balance.go`）两轮都是 32 条，一处没变；涨起来的
284 条全在 `store/` 下（`store/read.go` 9 → 60、`store/reconcile.go` 5 → 60）。

**变的是「有多少变异体被测到」，不是「杀掉的比例」。** 两个 Test efficacy
几乎一样（98.14% 与 97.01%），差得多的是 Mutator coverage——所以
**只看 efficacy 会把这件事完全盖住**，要连着 Mutator coverage 一起看。

修法是**把 DSN 变成硬前提**，而不是去收窄覆盖范围：脚本缺 `LARKAX_DB_DSN`
时直接拒绝运行。脚本也不传 `--coverpkg`、不传 `--integration`——上表里单独传
哪个都不改变本仓的结果，而两个一起的代价不现实。

另一条来源是机器上的别的负载——它挡不住，只能按下面的判据识别：

| 包 | 机器空闲 | 有别的负载 |
|---|---|---|
| `internal/billing` | `Killed 145, Timed out 0` | `Killed 136, Timed out 10` |
| `internal/tokens` | `Killed 100, Timed out 0` | `Killed 95, Timed out 5` |

这两行的数出自更早的一轮（那时 gremlins 还没钉版本、`--invert-logical` 也还没开），
只用来对照同一行的两列，不与下面「各包的现状」那张表对账。

**判据是超时条数，不是百分比。** 超时不为 0 就说明这一轮受了干扰：那一行照实写进
表里、连超时条数一起写（那一列就是为这个存在的），只是不与别的轮次比较，也不据此
判这个包测得好不好。
（这句话管的是怎么**读数**；门禁另有一条判据——脚本里的 `MAX_TIMED_OUT` 是 10，
超时超过 10 才判「这一轮结果不可信」并让这道门失败，两者各管一头。）

### 对存活的变异逐个判断

`scripts/check-mutation.sh` 跑完会把**存活的变异逐条列出来**（包名 + `LIVED`
+ 变异类型 + 位置）。
只打一个计数的话这一节没法执行——数得出来，看不了，于是「逐个判断」实际退化成
「看一个百分比」。存活不进退出码：存活本身不是失败，判过之后可以放过。

清单与汇总表是**同一批产出**：某个包的结果不可信或没跑成时，警告照打，
清单也照打，不因为另一个包的坏结果而消失。脚本里它排在收尾的成败判定
**之前**，这是有意的——挪到判定之后，一个包的坏结果就会让其余所有包的
清单一条都不打印。退出码仍然非 0：不可信或没跑成仍要失败、要被看见。

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

### 报一个变异体数量时带上三样

汇总表里每一列的意义都取决于它是怎么跑出来的。报任何一个变异体数量——写进 issue、
PR 描述或提交信息里时——同时给出下面三样，否则读的人会把同一个量当成另一个量：

1. **列名**：`killed`、存活、超时、`Not covered` 里的哪一列，照表头抄。
   改写成「杀掉的比例」「覆盖数」这类近义说法，就失去了对账的锚点。
2. **口径**：整包，还是只变异改动涉及的生产文件（`LARKAX_MUTATION_SCOPE=narrow`，
   由 `--changed-from` / `--changed-files` 触发；CI 的 PR 自动轮是后者）。
   收窄时四个计数列都只覆盖参与变异的那些文件。
3. **变异器集合**：是否打开了 `--invert-logical`（gremlins 默认关闭，
   `scripts/check-mutation.sh` 在 `EXTRA_MUTATOR_FLAGS` 里显式打开），
   以及 gremlins 的版本——两者共同决定变异体总数，也就是 efficacy 的分母。
   脚本每次运行打的那行「这把尺子」写的就是后两样，报数时一并贴上。

四种读法都出自「同一个量被当成另一个量」，不是算术错：

- **`killed` 不是候选总数。** 候选是 `killed`、存活、超时与 `Not covered`
  四个数之和，`killed` 只是其中被杀掉的那一列。
- **整包口径的候选总数不能与窄口径的 `killed` 相减。** 两个数覆盖的文件集合不同，
  相减得到的「缺口」不是任何一轮的产出。
- **一个位置可以挂多条变异体。** 同一个 `行:列` 上可能同时有
  `CONDITIONALS_BOUNDARY` 与 `CONDITIONALS_NEGATION` 两条（`<` 改 `<=` 与改 `>=`），
  所以数位置不等于数变异体；汇总表的数字是变异体数。
- **两个变异器集合下的数不能并排比较。** 不开 `--invert-logical` 时 `a || b`
  一个变异体都不生成，两边的分母不同，不是同一个包的两个样本。

三样写全的样子（数见上文「一个分数要连着它的条件一起读」）：

> 整包口径、gremlins v0.6.0、`--invert-logical` 已开：`internal/keyquota` 的
> `killed` 是 2，存活 0，`Not covered` 6。

### 各包的现状

由 `scripts/check-mutation.sh` 跑出。**下面每个数都连着这一轮的条件**——它们不是
「一般怎么跑」，是这些数实际跑出来的样子。

先贴上那一行「这把尺子」（脚本每次运行都会打，这里是原样）：

```
这把尺子：gremlins v0.6.0 / 变异器集合 ARITHMETIC_BASE CONDITIONALS_BOUNDARY CONDITIONALS_NEGATION INCREMENT_DECREMENT INVERT_NEGATIVES INVERT_LOGICAL / --invert-logical 已开 / workers=1 / 系数 10
```

它逐项的意思：

- **gremlins 版本**由脚本里的 `PINNED_GREMLINS_VERSION` 钉住，运行时探测并校验，
  不符就拒绝运行；
- **变异器集合**由脚本从 `unleash --help` 探测出来，不在这里抄一份——抄第二份的
  那天就是两份开始分岔的那天；
- **`--invert-logical` 已开**（`&&` ↔ `||`，默认关闭）。不开它时 `a || b` 这类
  判据一个变异体都不生成，那时的「efficacy 高」不能当成「布尔判据也被钉住了」；
- **workers=1、系数 10**，理由见上文「并发与超时系数是同一个旋钮」那一段；
- **`LARKAX_DB_DSN` 有效**，缺了它 `internal/ledger/store` 整包被跳过；
- **`LARKAX_REDIS_DSN` 有效，但只对测试依赖它的包**：判据是包里的测试文件有没有
  引用它，不是一份包名清单。缺它时那些集成测试整组被跳过，只在它们里碰得到的
  变异体落进 `Not covered`——不进 efficacy 的分母，分数只会变好看。脚本同样
  拒绝运行。

**贴数字时把那一行「这把尺子」一起贴上**：它是数字的条件，不是装饰。两次运行的
条件不同，两个数就不是同一个包的两个样本，别放在一起比。

**还有一条不在这一行里，而它同样在改数字**：gathering 那一次 `go test` 的耗时就是
每个变异体的超时预算（× 系数），而那一次调用没有 `-count=1`、会命中 go 的测试结果
缓存。命中时预算按缓存里的耗时算（实测 `134ms`，真跑一次是 `5.7s`），整包的变异体
会假超时：同一份代码、同一把尺子，`internal/ledger/store` 跑出过 `44 / 0 / 303`
与 `340 / 7 / 0` 两组，差别只在有没有先 `go clean -testcache`。这一轮的数是在清掉
测试结果缓存之后跑的（见 #773）。

**表里的包名集合必须与脚本的 `PKGS` 一致**（顺序不必：表按 efficacy 排）。这一条
由 `scripts/check-mutation-table.sh` 机器核对：加包进清单时要同时补行；反过来，
手工跑出来的行也不能留在表里。

| 包 | killed | 存活 | 超时 | Not covered | Mutator coverage | Test efficacy（递归） | 覆盖率（包内） |
|---|---|---|---|---|---|---|---|
| `internal/payment` | 110 | 0 | 0 | 5 | 95.65% | 100.00% | 77.8% |
| `internal/keyquota` | 2 | 0 | 0 | 6 | 25.00% | 100.00% | 3.3% |
| `internal/pricing` | 176 | 0 | 0 | 46 | 79.28% | 100.00% | 89.2% |
| `internal/snapshotwire` | 105 | 0 | 0 | 3 | 97.22% | 100.00% | 94.6% |
| `internal/overrides` | 18 | 0 | 0 | 0 | 100.00% | 100.00% | 96.2% |
| `internal/catalog` | 178 | 0 | 0 | 40 | 81.65% | 100.00% | 77.6% |
| `internal/adminapi` | 114 | 0 | 0 | 5 | 95.80% | 100.00% | 70.7% |
| `internal/assembly` | 71 | 0 | 0 | 10 | 87.65% | 100.00% | 80.1% |
| `internal/settings` | 27 | 0 | 0 | 4 | 87.10% | 100.00% | 73.6% |
| `internal/upstream` | 9 | 0 | 0 | 1 | 90.00% | 100.00% | 97.2% |
| `internal/config` | 43 | 0 | 0 | 0 | 100.00% | 100.00% | 97.2% |
| `internal/db` | 2 | 0 | 0 | 2 | 50.00% | 100.00% | 84.6% |
| `internal/dberr` | 3 | 0 | 0 | 0 | 100.00% | 100.00% | 100.0% |
| `internal/systemone` | 37 | 0 | 0 | 1 | 97.37% | 100.00% | 93.7% |
| `internal/textshape` | 4 | 0 | 0 | 1 | 80.00% | 100.00% | 100.0% |
| `internal/channels` | 288 | 1 | 1 | 41 | 87.58% | 99.65% | 76.6% |
| `internal/plugin/store` | 78 | 0 | 0 | 15 | 83.87% | 100.00% | 72.4% |
| `internal/calendar` | 150 | 2 | 0 | 14 | 91.57% | 98.68% | 97.8% |
| `internal/ledger` | 576 | 8 | 0 | 92 | 86.39% | 98.63% | 96.3% |
| `internal/plans` | 140 | 2 | 0 | 15 | 90.45% | 98.59% | 75.2% |
| `internal/api` | 197 | 3 | 0 | 19 | 91.32% | 98.50% | 90.2% |
| `internal/pipeline` | 62 | 1 | 0 | 0 | 100.00% | 98.41% | 90.1% |
| `internal/ledger/store` | 728 | 12 | 0 | 48 | 93.91% | 98.38% | 82.6% |
| `internal/auth` | 221 | 4 | 1 | 8 | 96.57% | 98.22% | 86.6% |
| `internal/relay` | 112 | 3 | 0 | 7 | 94.26% | 97.39% | 87.7% |
| `internal/channel` | 313 | 9 | 0 | 22 | 93.60% | 97.20% | 88.5% |
| `internal/idempotency` | 34 | 1 | 0 | 0 | 100.00% | 97.14% | 83.3% |
| `internal/plugin/sandbox` | 65 | 0 | 0 | 5 | 92.86% | 100.00% | 93.8% |
| `internal/route` | 123 | 4 | 0 | 24 | 84.11% | 96.85% | 97.8% |
| `internal/gateway` | 238 | 9 | 0 | 9 | 96.48% | 96.36% | 95.7% |
| `internal/observe` | 28 | 1 | 0 | 1 | 96.67% | 96.55% | 94.1% |
| `internal/plugin/host` | 107 | 1 | 0 | 4 | 96.43% | 99.07% | 94.6% |
| `internal/snapshot` | 153 | 6 | 0 | 50 | 76.08% | 96.23% | 90.1% |
| `internal/credential` | 96 | 4 | 0 | 1 | 99.01% | 96.00% | 92.1% |
| `internal/quota` | 45 | 2 | 0 | 0 | 100.00% | 95.74% | 95.2% |
| `internal/billing/expr` | 168 | 8 | 0 | 12 | 93.62% | 95.45% | 85.9% |
| `internal/ratelimit` | 33 | 2 | 0 | 3 | 92.11% | 94.29% | 92.0% |
| `internal/billing` | 282 | 18 | 0 | 13 | 95.85% | 94.00% | 98.2% |
| `internal/tokens` | 89 | 14 | 0 | 51 | 66.88% | 86.41% | 97.2% |
| `internal/money` | 92 | 15 | 0 | 4 | 96.40% | 85.98% | 96.9% |

**`internal/billing` 与 `internal/ledger/store` 这两行是 2026-10-24 单独重跑的**，
其余各行仍然是最初那一轮整跑的记录。重跑的条件与上面「这把尺子」那段一致：
gremlins v0.6.0、开着 `--invert-logical`、workers=1、超时系数 10，两行的超时
都是 0。
所以**父子行之间不再能互相核对**：`internal/ledger` 那一行的变异范围含 `store/`，
但它记的是旧数，killed 反而比 `internal/ledger/store` 自己那一行小；
`internal/billing` 与 `internal/billing/expr` 同理。各行读各行的，别拿父行的数
去核子行。

**最后两栏的范围不一样，别混着读。** `Test efficacy（递归）` 说的是 `./<pkg>/`
这一条命令：包路径递归，子包的变异体一起算进来；`覆盖率（包内）` 另取一条命令
（`go test -cover ./<pkg>/`），只算这一个包自己的行。两个集合不同，同一行这两栏
没有可比性。

**有两对行的范围是重叠的。** `internal/ledger` 那一行的变异范围含
`internal/ledger/store`，`internal/billing` 那一行含 `internal/billing/expr`——
表里的数来自**整包跑**（不是 `--changed-files` 那种收窄跑），所以子包的变异体
在父包那一条命令里跑一次、又在子包自己那一行跑一次，脚本会打一行注记点明。
**显式把这样一对一起传会被拒绝**（见上文）；**这不是「父包会把子包选中」**：
递归管的是「变异哪些文件」，选包管
的是「跑清单里哪一行」，两件事，规则见上文。

**超时列不为 0 的行不与别的轮次比较**——见上文。改动过某个包的测试之后重跑并在
表里更新。`internal/auth` 那一行的超时是**自然超时**而不是机器干扰：`deletion.go:712`
把占位名循环里的 `n++` 改成 `n--` 之后，第 1 个候选被占住就永不终止。
**测试里等信号的那一处要带上限**：被测路径走不到时，一个无上限的接收会把变异体
记成 Timed out 而不是 Killed——两个字母之差，一条真缺口在账面上就成了「机器忙」。
`internal/api` 原先的两个超时就是这么来的（`<-entered` 没有上限），加上限之后
它们变成 Killed，那一行也从 195/3/2 变成 197/3/0。

**`internal/plugin/store` 原先按「前提让它不可达」放过的那一条，现在由直接用例钉住了。**
`generationOrNil` 的两个 `sql.Null*` 出自 `runningJoin()` 同一个
`LEFT JOIN LATERAL … LIMIT 1` 的同一行，而 `plugins.version` 与
`plugins.generation` 在迁移里都是 `NOT NULL`，所以两列的 `Valid` 恒等，
`||` 与 `&&` 同值。前提成立不等于守卫可以被删：那毕竟是函数自己的判断，不是
schema 替它做的。`TestGenerationOrNil` 补了「只有版本」与「只有代次」两种单边
为空的输入，返回都是 nil；schema 那条前提仍由 `TestPluginJoinColumnsAreNotNull`
对着 `information_schema` 机器核对，列哪天改成可空，它会红。这一行从
`77 / 1` 变成 `78 / 0`。

**`internal/plugin/host` 只剩数组容量提示那一条等价。** `log.go:259` 把
「字段总预算 ÷ 单个数值的估长」改成乘法，只改 `make` 的预分配上界，仍被
`len(keys)` 压着，不进内容、也不参与任何失败判断；换出来的数组一样，
`TestLogBoundsArrayFieldValues` 钉着结果。

另外三条原先也只靠前提放过，现在都由直接用例钉住：`truncateMessage` 与
`boundedString` 的 `cut > 0` 拿非法 UTF-8 输入钉住
（`TestTruncationStopsAtZeroForInvalidUTF8`：首字节是续字节时切点一路退到 0，
`>= 0` 那一版就走到负下标）；`fieldAttrs` 在空对象上返回 nil 还是空切片由
`TestFieldAttrsReturnsNilForEmptyObject` 钉住——日志行在调用点看来一样，但返回值
是函数自己的承诺。这一行从 `104 / 4` 变成 `107 / 1`。

**`internal/plugin/sandbox` 那两条守卫现在也由直接用例钉住。** `Call` 里的
`limit > 0`（0 表示不设上限）与合并两个父 ctx 时的 `parent != nil && parent != ctx`
（没有父 ctx 是允许的状态）：`runtimeState` 只有 `newRuntime` 一个构造点，它先把
零值与负值的字节上限归一成默认的 4 MiB，又把 `injectCtx` 置成 `Background`，
所以正常构造走不到这两格。`TestZeroInputLimitMeansNoLimit` 与
`TestCallWithoutInjectedContext` 直接改 `runtimeState` 的字段，钉的是守卫本身；
「构造路径总会归一」那两条前提的用例（`TestNewRuntimeAlwaysNormalizesInputLimit`、
`TestInjectCtxIsNeverNil`）继续留着。这一行从 `63 / 2` 变成 `65 / 0`。

**`internal/api` 那三条存活也都是等价。** `error.go:352` 的 `secs < 1` 改成 `<= 1`：
`secs` 只有 0、1、大于 1 三种，等于 1 时两版都返回 1。`response.go:261` 的
`input < 0` 改成 `<= 0`：等于 0 时两版都赋 0。`responses.go:311` 是 map 的容量
提示，`+1` 改成 `-1` 只改分配，不进内容——负的容量提示不 panic，这一点与切片不同，
由内层为空的那条用例实证。

**`internal/adminapi` 是唯一一个一条等价都不需要的包。** 首轮基线 `104 / 5 / 0 / 10`，
五条存活全是可达的真缺口：`id = 0` 被当成合法 id、端点的 NULL 与 `[]` 在响应里互换、
`parseDate` 把空串与 nil 中的一个判错，以及更新模型时「字段缺席」与「传空串清空」
被对调。补上用例之后是 `114 / 0 / 0 / 5`——killed 涨的十条里，五条是原先那五条存活，
另五条是原先落在 `Not covered` 里的变异体：新用例顺带把那些行也走到了。所以这个包
**别拿两次的 `Not covered` 当同一件东西比**。

**`internal/channel` 那九条存活分成两类：容量提示，与冗余的提前退出。**
三条是 `bufio.Scanner` 的初始容量（`anthropicadapter/stream.go:45`、
`openaiadapter/stream.go:33`、`responsesadapter/stream.go:74`）：`64*1024` 里的
`*` 换成 `/` 只改预分配，Scanner 会自己长到 `maxTokenSize`，扫描上限那一格
（`4*1024*1024`）不受影响。三条是事件行判据里的 `||` 改成 `&&`
（`anthropicadapter/stream.go:108`、`openaiadapter/stream.go:50`、
`responsesadapter/stream.go:166`）：空行与 `:` 注释行既不匹配 `event:` 也不匹配
`data:`，落到后面那两道前缀判断同样被跳过，不打日志也不发事件。另外三条各是
一格边界：`anthropicadapter/stream.go:259` 的 `!ok || m == nil`——`!ok` 时 m 本来
就是 nil，而两条都为假时四次读取都给 nil、四处赋值又都要求 `v > 0`，累计值不变；
`httpx/httpx.go:110` 的 `idx < 0` 改成 `<=`——只在 `idx == 0` 时不同，那时切出来
的段是空串，紧跟着的长度判据照样返回空；`openaiadapter/dto.go:122` 的
`len(raw) == 0` 改成 `>=`——空字节上两次解码都报错，字段停在零值 nil。最后这条
还依赖「默认编解码器就是 `encoding/json`」，仓里没有 `SetCodec` 的调用点，这个
前提写在用例注释里。

**`internal/observe` 那一条存活是结构性的等价。** `metrics.go:159` 的排序比较符
`<` 改成 `<=`：参与比较的 `labels` 由不同的 map 键渲染而来，而键是
`strings.Join(values, "\x00")`——只要标签值不含 `\x00`，渲染就是单射，两个待比较
的串恒不相等，两版给出的顺序完全一致。本包的标签值都取自闭枚举
（`authFailureReason`、`UsageSource`、`Protocol`），前提成立；论证写在
`internal/observe/metrics_test.go` 末尾。标签值含 `\x00` 时这个前提不成立，
但那是另一件事——它会让两条序列渲染成同一个串，与排序无关（见 #858）。
`health.go:20` 的 `readyzTimeout` 是包级常量，gremlins 永远记「未覆盖」，补用例
改变不了；用 `go test -overlay` 把 `*` 换成 `/` 单独验过，
`TestReadyzPassesUnexpiredDeadlineToChecks` 会红。

**`internal/systemone`、`internal/db`、`internal/textshape` 那几条 `Not covered`
也都在包级常量上，与 `readyzTimeout` 是同一形状。** gremlins 的覆盖块从语句或
分支开头起算，包级 `const` 的声明不属于任何块，所以那几行上的变异体一律记
「未覆盖」、根本不会被运行——对这几条常量本身，改用例改变不了这一点。逐条核过：

- `systemone.go:93` 的 `MaxInputTokens = 64 * 1024`：`TestInputUpperBoundIsPinned`
  已经钉住这个数。用 `go test -overlay` 把 `*` 换成 `/` 单独验过，那条用例会红
  （`MaxInputTokens = 0`）——它不是缺口，只是这一格进不了分母。
- `db.go:31` 的 `pingTimeout = 5 * time.Second`：同样用 overlay 把 `*` 换成 `/`
  （值变 0）验过，`TestOpenKeepsUnderlyingCause` 会红（错误从解析失败变成
  `context deadline exceeded`）。
- `db.go:26` 的 `connMaxIdleTime = 10 * time.Minute`：换成 `/` 之后用例照常通过。
  这一条没有能在用例里观察的出口：`database/sql` 没有读回空闲连接上限的 getter，
  `DB.Stats().MaxIdleTimeClosed` 虽然就是「因这个上限被关掉的连接数」，但同样要
  真等满十分钟才会计数，属于用例不该做的事。与上面两条不同的是，这个值没有任何
  断言钉着——将来把它从十分钟改成三十分钟不会有东西响。补一条值断言不会改变这里
  的分数（那一格照样记未覆盖），所以这是文档诚实度的问题，不是缺口。
- `textshape.go:37` 的 `whyRendered`：变异器把两个字符串字面量之间的 `+` 换成
  `-`，overlay 实测连编译都过不去（`operator - not defined on "…" (untyped string
  constant)`）。就算它进了覆盖范围也只会记 `NOT VIABLE`，不存在能杀掉它的用例。

**上面这几个包不在自动变异的触发范围里。** `.github/workflows/mutation.yml` 的
触发范围由 `scripts/check-mutation.sh` 的 `MONEY_PKGS` 决定（账本、计费、金额、
结算额、单价、额度包结转），这几个包不在其中，PR 的自动轮不会选到它们；要量只能
手工触发（`workflow_dispatch` 的 `packages` 填包名）或本地跑。所以表里这几行会
停在入库的这一轮，改了它们不会自动更新——动这几个包时记得手工重跑并在表里更新。

**`internal/config` 的三条存活都是真实缺口，这一包没有等价变异。** 两条是
`RedactDSN` 里判断「这串是不是 URL 形式」的 `&&` 链（`config.go:309` 的两个运算符
各一条）：现有用例只喂过「带 scheme 的 URL」与「没有主机的关键字形式」，从没走过
「有主机、没有 scheme」那一格，两条变异体于是都能把这种串改道进 URL 分支、让
`u.String()` 重新转义一遍。第三条是 `dotenv.go:61` 里 `os.Getwd` 的失败判据：
反过来之后，成功取到的目录会被丢掉，而自动查找 `.env` 正是从这个目录出发的。
三条都用 `go test -overlay` 造出变异体跑红、还原后跑绿，复跑是 `43 / 0 / 0`。

**`internal/money` 与 `internal/pipeline` 这一轮各多出一条等价，前提都落在零值上。**
`money.go:376` 的 `bp == 0 || a.units == 0` 改成 `&&`：任一项为 0 时乘积恒为 0，
`Quo` 出来仍是 0，而 Amount 的零值就是 `Zero`；`||` 省的只是一次 big.Int 分配。
`pipeline.go:1258` 的 `bp > 0` 改成 `>= 0`：`BasisPointsFor` 只可能返回 0…10000，
两版只在 0 处分岔，而那一格 `MulBasisPoints(0)` 早退返回 `Zero`。两条论证分别写在
`TestMulBasisPoints` 与 `TestSettleMerchantShareBoundaries` 的注释里。

**覆盖率高而 efficacy 低，说明测试断言的是「相对关系」而不是「具体边界」。**
这类包的用例写的是「A 明显比 B 好」这种比较，于是改 `<=` 为 `<`、去掉分母的加数、
改常量，只要顺序不变就测不出来。按这一轮这把尺子，两栏差得最大的是 `tokens`
（覆盖率 97.2%、efficacy 86.41%）与 `money`（96.8% 对 86.54%）——差得大就值得去
脚本打印的那份存活清单里逐条看。

**不要为了把 efficacy 刷上去而写对着分支的琐碎断言。** 它会让数字好看，
但不会让行为更可信。`snapshot` 的遍历器那一层还没补，因为**它需要按功能
场景补而不是按函数补**（例如「一次含工具往返的请求走完整流程」）。
