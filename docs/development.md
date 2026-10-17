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

## 请求快照

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

### 允许表与读回

```sh
# 允许表：哪些字段放行、哪些替换成占位、每一行为什么这么定。不连库。
go run ./cmd/larkaxctl snapshot allowlist
go run ./cmd/larkaxctl snapshot allowlist --direction request

# 读回一条快照。--actor 与 --reason 都必填，见下。
go run ./cmd/larkaxctl snapshot show <request_id> --actor <用户 ID 或用户名> --reason <文本>

# 试脱敏：从 stdin 读一段报文，看它会变成什么骨架。不连库。
echo '{"model":"...","messages":[...]}' | \
  go run ./cmd/larkaxctl snapshot redact --direction request
```

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

## 给一把 key 设额度

两列额度可以在创建时给，也可以事后调：

```sh
go run ./cmd/larkaxctl apikey create --user alice --rate-limit-rpm 60 --quota-limit 12.34
go run ./cmd/larkaxctl apikey list                       # id、前缀、额度、最近使用
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
| `:8080` | `POST /v1/chat/completions`、`POST /v1/messages` | 推理；`stream: true` 时是 SSE 流；没有有效 API Key 时 `401` |
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
| `snapshot` | 请求快照：`cleanup` / `allowlist` / `show` / `redact` |
| `idempotency` | 幂等记录的留存清理：`cleanup` / `cleanup --dry-run` |
| `calendar` | 节假日日历数据：`import` |
| `rotate` | 主密钥轮换 |
| `migrate` | 迁移：`up` / `down-to` / `status` / `create` |
| `reconcile` | 对账 |

每个子命令的用法见 `go run ./cmd/larkaxctl <子命令> --help`，
或直接 `go run ./cmd/larkaxctl` 看总览。对账的开关、口径与运行记录见
[reconciliation.md](reconciliation.md)，日历数据的文件格式见
[calendar-data.md](calendar-data.md)。

两个端点都认请求头 `Idempotency-Key`：带上它时同一个键重复发送只执行一次，
第一次的结果被原样重放。用法、三条 `409` 的码与留存规则见
[idempotency.md](idempotency.md)。

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
bash scripts/check-deadcode.sh      # 检查有没有新出现的不可达符号
bash scripts/check-unimported.sh    # 检查有没有整包没人 import

bash scripts/check-redlines.test.sh   # 只在改过红线脚本后需要跑
bash scripts/check-deadcode.test.sh   # 同上，改过不可达符号脚本后跑
bash scripts/check-unimported.test.sh # 同上，改过整包没人 import 脚本后跑
```

提交前至少跑一遍 `gofmt -l .`、`go vet ./...`、`go test ./...` 和四个自查脚本。
CI 会跑同样的东西，本地跑是为了少一轮来回。

### 不可达符号

`check-deadcode.sh` 从 `cmd/` 下的 main 包（自动发现，当前是 `./cmd/larkax` 与
`./cmd/larkaxctl`）出发跑 `deadcode`，把不可达的符号与
`scripts/deadcode-allowlist.txt` 比对：

- **新出现的不可达符号** → 失败。要么接线，要么把它写进白名单
- **白名单里已经消失的条目** → 只提示「这一条可以删了」，不算失败。
  它出现的原因是「有人把没门的东西接上线了」，那是个好动作
- 白名单条目数超过上限 → 失败。新增一条要同时把脚本里的 `MAX_ENTRIES`
  往上调，那个改动会被 review 看到

它需要 `deadcode`，版本必须与脚本里 `PINNED_TOOLS_VERSION` 一致——
符号集合随 `golang.org/x/tools` 的版本变化，装错版本脚本会拒绝运行：

```sh
go install golang.org/x/tools/cmd/deadcode@v0.52.0
```

### 整包没人 import

`check-deadcode.sh` 从入口出发跑 `deadcode`，而 `deadcode` 只分析**从入口可达
的那些包**。一个全仓库没有任何 import 方的包**整包不在它的分析范围里**，
于是它的不可达清单是空的——而「空」与「全部可达」在输出里长得一样。
所以「整包没人接」是那道门唯一系统性漏掉的一类，也是「有实现、有测试、
没有门」最严重的一种。

`check-unimported.sh` 补上这一半，只依赖 `go`：

```sh
go list ./...             # 仓库自己的包
go list -deps ./cmd/...   # 从入口可达的包（入口由 cmd/ 自动发现）
                          # 两者相减 = 整包没人 import 的包
```

结果与 `scripts/unimported-allowlist.txt` 比对，口径与 `check-deadcode.sh` 一致：

- **新出现的无人 import 包** → 失败。要么接线，要么把它写进白名单
- **白名单里已经可达的条目** → 只提示「这一条可以删了」，不算失败
- 白名单条目数超过上限 → 失败。新增一条要同时把脚本里的 `MAX_ENTRIES`
  往上调，那个改动会被 review 看到

两处需要知道的约定：

- **只被测试 import 的包算「没人 import」。** 测试自己就能构造调用方，
  所以「只有测试在调」与「生产在调」在覆盖率上长得一样。代价是测试夹具包
  也会被报出来，它们进白名单。
- **入口是自动发现的**（`cmd/` 下带 `package main` 的目录），不手写清单——
  一道需要人记得同步维护的门，会以它要防的方式坏掉。

不要为了把白名单刷空而去硬接一条调用方：接一条假的比留着它更坏。

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
