# 管理面（`/admin/v1/*`）

内部监听上的管理接口：改渠道、模型目录、别名与计价规则。它只挂在
`LARKAX_INTERNAL_ADDR` 那个端口上，**不**出现在公共监听，也不属于任何调用协议。
装配与挂载点见 [development.md](development.md) 的「当前能做什么」一节。

**用户销户不在这个面上**：它只有命令行入口（`larkaxctl user delete`），三条前置
条件与各自的清理命令见 [cli-reference.md](cli-reference.md) 的 `user` 一节，
留存口径见 [request-retention.md](request-retention.md) 的「账号销户」。

公共监听的鉴权是用户 API Key，那一把 Key 的语义是「以某个用户的名义发推理请求」。
管理面改的是全局数据，所以它用**单独一套管理令牌**，不复用 API Key。

## 令牌

```sh
larkaxctl admin token create    # 生成一枚，明文只在这一次打印
larkaxctl admin token revoke    # 撤销当前令牌
```

`create` 生成 32 字节随机令牌（base64url，43 个字符）。库里只存它的 SHA-256
（64 位小写十六进制），**明文只有 stdout 那一行**，丢了只能重新生成。再
`create` 一次直接替换旧令牌，不留两枚都有效的窗口。`revoke` 之后任何令牌都
匹配不上。

`revoke` 在**从没创建过**令牌的库上是一次空操作：它不改那一行，直接报「没有
配置过管理令牌」。空串表示「这个部署还没启用管理面」（回 503），而撤销表示
「换一枚令牌」（回 401）——把前者写成 401 会让运维去换一枚他从来没创建过的
令牌。

令牌存在 `settings` 表的 `admin.token_sha256` 那一行，由上面两条命令写入，不接受
`settings set`（那一列 `editable` 为 false）。

**每次请求都读一次库**，所以生成与撤销立刻生效，不必重启。管理面的流量很小，
一次主键查询不值得为它引入缓存与失效逻辑；**它不在请求热路径上**，也不该被当成
热路径使用——推理请求不会经过这里。

请求头：

```
Authorization: Bearer <令牌>
```

## 状态码

| 情形 | 状态码 | 正文 |
|---|---|---|
| 没有配置过令牌（`admin.token_sha256` 是空串） | `503` | 只说管理面没启用、提示先 `larkaxctl admin token create`；不透露是哪一项没配 |
| 没带令牌，或令牌不对（含撤销之后） | `401` | 两者逐字相同 |
| 这个实例没有装配管理面（worker 角色） | `404` | 路径根本没有注册 |

「没配置」与「没授权」要分开：前者的下一步是先把服务配起来，后者是换一枚令牌。
「没装配」与「没授权」也要分开：worker 角色上 `/admin/v1/` 回 404，不是 401。

## 接口面

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/admin/v1/channels` | 渠道清单 |
| `POST` | `/admin/v1/channels` | 建渠道 |
| `GET` | `/admin/v1/channels/{id}` | 单条渠道 |
| `PATCH` | `/admin/v1/channels/{id}` | 改渠道 |
| `GET` | `/admin/v1/adapters` | 内置适配器枚举（渠道表单用） |
| `POST` | `/admin/v1/channels/probe-models` | 拉一次上游模型清单（不落库） |
| `GET` | `/admin/v1/models` | 模型目录 |
| `POST` | `/admin/v1/models` | 建模型 |
| `PATCH` | `/admin/v1/models/{name}` | 改模型（`{name}` 是规范名） |
| `DELETE` | `/admin/v1/models/{name}` | 删模型 |
| `GET` | `/admin/v1/aliases` | 别名清单；`?channel_id=N` 收紧到某条渠道 |
| `POST` | `/admin/v1/aliases` | 建别名 |
| `PATCH` | `/admin/v1/aliases/{id}` | 改别名 |
| `DELETE` | `/admin/v1/aliases/{id}` | 删别名 |
| `GET` | `/admin/v1/pricing/rules` | 售价规则清单 |
| `POST` | `/admin/v1/pricing/rules` | 建售价规则 |
| `PATCH` | `/admin/v1/pricing/rules/{id}` | 改售价规则 |
| `GET` | `/admin/v1/pricing/costs` | 成本规则清单 |
| `POST` | `/admin/v1/pricing/costs` | 建成本规则 |
| `PATCH` | `/admin/v1/pricing/costs/{id}` | 改成本规则 |

别名清单不带 `channel_id` 时给的是**平台级**那一段（`channel_id = 0`），与
`larkaxctl channel map list` 的默认口径一致。

**用户管理不在这里**：`/api/v1/admin/users` 只挂在控制台的会话面（见
[api.md](api.md) 的「控制台管理接口：用户」）。它的判据里有「不能改自己」
「不能撤掉最后一个 active 管理员」，而运维令牌没有「自己」这个概念。内部
管理面的资源组是一份显式清单（`adminapi.InternalGroups`），`users` 组只随
显式组名注册，不在那份清单里。

`/admin/v1/users` 的 `404` 以**带有效令牌**为前提：管理面一旦装配，鉴权就
包住整张路由表（`internal/adminapi/adminapi.go` 的 `return authorize(mux)`），
不带令牌访问其中任何路径——包括没有注册的组——都先被鉴权拦下
（部署配置过令牌回 `401`，没配置过回 `503`）。只有当令牌有效、走进 `mux`
才发现该组没有注册（`if groups[GroupUsers]`）时，才是 `404`。worker 角色
根本没有装配管理面，那是另一回事，见上面的「状态码」表。

这一层是薄层：写入语义与校验全在 `internal/channels`、`internal/catalog`、
`internal/pricing` 的 admin 类型里，接口只做「解请求 → 调它 → 翻成 JSON」。
字段名用 snake_case，与 CLI 的 `--flag` 一一对应（`base_url` ↔ `--base-url`），
完整字段见各资源的请求结构（`internal/adminapi/resources.go`）与
[cli-reference.md](cli-reference.md) 的对应命令。

## 请求与响应

- 正文是 JSON，上限 1 MiB。**未定义的字段会被拒绝**（`400`），不是静默忽略：
  管理面的写错一个字段名应当当场知道。
- 清单回数组（`200`，空清单是 `[]`），建回 `201` 与 `{"id": N}`，改回 `200`
  与 `{"ok": true}`，删回 `204`。
- `PATCH` 只改给出的字段：没出现的键表示不改。少数可空字段另有「清空」的写法，
  见 `resources.go` 里各更新结构的注释（例如模型的 `knowledge_cutoff` 给空串）。
- 渠道的凭证**只写不读**：`credential` 是写入字段，读回的是 `credential_keys`
  与 `credential_hint`（打了码的提示），任何路径都不回明文。

## 渠道表单的输入

两个端点给「建渠道 / 改渠道」的表单用：适配器从注册表里选，模型名先问一次
上游。它们随 `channels` 组注册，所以控制台的 `/api/v1/merchant/` 与
`/api/v1/admin/` 下是同一对路径。

### 适配器枚举

`GET /admin/v1/adapters`

```json
{"items": [
  {"type": "anthropic", "protocol": "anthropic", "endpoint": "/v1/messages"},
  {"type": "openai", "protocol": "openai", "endpoint": "/v1/chat/completions"}
]}
```

按 `type` 升序。三个字段都来自适配器自己：`type` 是 `channels.type` 的取值，
`endpoint` 是这套适配器把请求发到哪条路径。清单不额外声明「哪个类型能列模型」
——那由下面的探测在运行时回答。

### 模型探测

`POST /admin/v1/channels/probe-models`

```json
{"channel_id": 12, "type": "openai",
 "base_url": "https://api.example.com/v1", "credential": "<API_KEY>"}
```

两种形态：

- 给 `channel_id`：`type`、`base_url`、`credential` 都从库里那一行取；给了
  `base_url` 或 `credential` 就用给的那个（编辑中的表单还没保存）。渠道不存在、
  已被软删、或不属于当前商家，都回 `404`——归属判据在读库之后、覆盖之前。
- 不给 `channel_id`：`type` 与 `base_url` 必填，`credential` 可以为空
  （无鉴权的自建上游）。`base_url` 走与建渠道同一条校验（`NormalizeBaseURL`）。

成功回 `200`：

```json
{"supported": true, "models": ["gpt-4o"], "total": 1, "truncated": false}
```

- `models` 去重、按名字升序；空清单是 `[]`。
- 上游没有列模型的能力（今天的 `systemone`）：`200` 与
  `{"supported": false, "models": [], "total": 0, "truncated": false}`。
  「这家没有这个能力」不是一次失败的请求。
- `truncated` 表示清单超过 2000 条被截断；`total` 仍是截断前的条数。
  **响应体**超过 8 MiB 是另一回事，见下面的 `502`。

失败：

| 状态码 | 什么时候 |
|---|---|
| `400` | 两种形态缺字段、`base_url` 校验不过、`type` 不是已知类型 |
| `404` | `channel_id` 不存在、已软删、不属于当前商家 |
| `429` | 同一调用方两次探测间隔不到 2 秒，或一分钟内超过 10 次；带 `Retry-After`（整数秒，向上取整） |
| `500` | 这一行的凭证密文损坏：换主密钥救不回来，要重新写一次凭证。正文是固定文案 |
| `502` | 上游连不上、回非 `2xx`、或响应体超过 8 MiB。几种情形的 `error.code` 与正文见下 |
| `504` | 上游 10 秒内没有返回响应头，或响应体没读完。时限挂在探测自己的 HTTP 客户端上，与「上游回了个错」分开 |

控制台商家面上，`404` 的正文与渠道资源的越权 404 逐字相同（`not found`）：
「不存在」与「不是你的」能分辨时，这个 404 就成了「这一行存在」的确认。
内部管理面没有归属可收窄，正文里带着那个 `id`（调用者是运维）。
`base_url` 校验不过的正文是一句固定英文，`NormalizeBaseURL` 给出的中文原因
只写进日志——它与命令行共用同一个函数，那边报错的对象是敲命令的人。

`502` 的几种情形，`error.code` 与正文各不相同：

- 「响应体超过 8 MiB」→ `upstream_response_too_large`。8 MiB 是**读的时候**
  生效的上限，不是拿 `Content-Length` 提前判的——上游完全可以不告诉我们长度。
  正文是一句固定说明。
- 「根本没连上」（DNS 解析失败、连接被拒、连接中断，没有任何响应）→
  `upstream_unreachable`。超时不走这条，它单独是 `504`。正文是
  `the upstream call failed`：没有响应，也就没有状态码可报。
- 拿到响应但不可用：非 `2xx`，或 `2xx` 但响应体不是模型清单的形状（例如一个
  正常的网页或一段 HTML 错误页）。不给 `error.code`，正文是「上游状态码 +
  截短到 200 字符的响应体片段」，状态码可能是 `200`。

探测不跟随重定向（`3xx` 按失败处理）：它回答的是「这个地址返回什么」，跟到
别处读到的已经是另一个事实。它也不按私网段拦截——自建上游在内网是真实用法；
这块成本由限流压住。

凭证只在服务端的内存里出现一次，不落库、不进日志、不进响应。上游错误体里
回显了这次用的凭证时，返回给调用方的正文里那一串会被换成 `<redacted>`。
探测的请求体不是一次推理请求，不会进请求快照。

## 错误体

与公共监听同一个形状（`internal/api` 的 `WriteError`，OpenAI 那一套）：

```json
{"error": {"message": "…", "type": "…", "param": null, "code": null}}
```

| 状态码 | 什么时候 |
|---|---|
| `400` | 请求体不是 JSON、有未定义的字段、校验失败 |
| `401` | 令牌缺失或不对 |
| `404` | 目标不存在 |
| `409` | 约束冲突（重名、仍被引用、别名撞车） |
| `429` | 模型探测被限流（见「渠道表单的输入」） |
| `502` | 模型探测时上游失败、连不上或响应体过大（见「渠道表单的输入」） |
| `504` | 模型探测时上游超时 |
| `500` | 数据库报错、或探测一条已存渠道时凭证密文损坏（见「渠道表单的输入」） |
| `503` | 没配置令牌、数据库连不上、或探测一条已存渠道时本进程没有主密钥 |

表里说的是**由 handler 产出**的错误体。路径根本没有注册的 `404`（未注册的
资源组、worker 角色上整片 `/admin/v1/`）由 `http.ServeMux` 的默认处理器给出，
正文是纯文本 `404 page not found`，没有这层信封。

消息是给调用方看的一句话，不带上游原文与内部路径；需要细节时去查服务端日志。
**模型探测是这条的具名例外**：它的 `502` 正文回的是上游状态码加一段截短的响应体
片段（凭证换成 `<redacted>`），见「渠道表单的输入 → 模型探测」。
`500` 与 `503` 的正文是**固定文案**：数据库驱动与连接错误可能带着连接串、主机名
与表名，那些只写进日志，不回给调用方。
