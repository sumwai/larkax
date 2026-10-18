# API 参考

这份写**调用方看到的那一层**：有哪几条路径、每个响应带什么头、什么情况回什么
状态码、错误体长什么样、流式的帧序列是什么。协议的语义（怎么路由、怎么计费、
限流怎么算）不在这里，各自的文档见每一节的指向。

## 路径与协议

**协议由路径决定，不由请求体决定。** 四条路径，其余一律 `501`：

| 路径 | 协议 | 认证习惯 | 流式 |
|---|---|---|---|
| `POST /v1/chat/completions` | OpenAI | `Authorization: Bearer <key>` | 是 |
| `POST /v1/messages` | Anthropic | `x-api-key: <key>` | 是 |
| `POST /v1/responses` | OpenAI Responses | `Authorization: Bearer <key>` | 是 |
| `POST /v1/systemone` | System One（本网关自己的形状） | `Authorization: Bearer <key>` | **否** |

`/v1/systemone` 不做对话，而是拿一段状态与一组带类型的判定问题、回一组答案，
形状见 [development.md](development.md) 的「决策接口」一节。它在请求体里带
`stream` 时回 `400`，文案由 `internal/systemone` 给出。

**其余路径一律 `501`**（`this endpoint is not implemented`）：只实现了上面这四条
POST 路径，**没有** `GET /v1/models`、`GET /v1/models/{id}`、`/v1/embeddings`、
`/v1/files` 这些 OpenAI 兼容接口的惯例端点。按 OpenAI 那套 SDK 写客户端时，
只要有代码在启动时拉一次模型清单，就会拿到这个 `501`。

### 认证

两个头都认，**`Authorization` 优先**：`Authorization: Bearer <key>`（OpenAI 的
习惯）与 `x-api-key: <key>`（Anthropic 的习惯）。`Authorization` 一旦出现就不再
回退到 `x-api-key`——带了但它不是 `Bearer <token>` 两段时直接回 `401`，不会拿
另一个头再试一次（见 `internal/auth`）。

### 请求本身的两道门

- **`Content-Type` 必须是 `application/json`**，否则 `415`。
- **请求体最大 8 MiB**，超出回 `413`。这条限制在**解析请求体之前**生效，不是可配项，
  见 [limits.md](limits.md) 的「请求体上限」一节。
- **方法必须是 `POST`**，否则 `405`，并带 `Allow: POST`。

## 响应头

| 头 | 什么时候带 | 说明 |
|---|---|---|
| `X-Larkax-Version` | 每个响应 | 发布版本。用户报问题时最该先确认的就是它，见 [versioning.md](versioning.md) |
| `X-Request-Id` | 每个响应 | 这一次请求的关联标识。**流式那份在第一个事件之前就发出去了**。报问题时附上它，我们拿它去日志里找这一次请求 |
| `Content-Type` | 每个响应 | `application/json; charset=utf-8`；流式是 `text/event-stream; charset=utf-8` |
| `Retry-After` | 只有被 RPM 限流拒时 | 整数秒。**不给的那些 429 是有意的**，见 [limits.md](limits.md) 的 `Retry-After` 那一节 |
| `Allow` | 只有 `405` | 恒为 `POST` |

**本网关不返回 `x-ratelimit-*` 系列响应头**（`X-RateLimit-Limit-Requests` /
`-Remaining-Requests` / `-Reset-Requests` 那一套）。限流信息只在**被拒**时给：
`429` 加上面那个 `Retry-After`。按 OpenAI 的惯例写完自限速、然后发现始终读不到
这几个头的人，这一句就是答案——它们不会被返回，不是配错了。

`X-Request-Id` 是**标识**，不是凭证：它不用于鉴权，也不是幂等键。要幂等用
`Idempotency-Key`，见 [idempotency.md](idempotency.md)。

## 状态码

| 状态码 | 什么时候 | 谁给的 | 该做什么 |
|---|---|---|---|
| `400` | 请求体不是合法 JSON；`max_tokens` 缺失、非正或超过单次上限；`/v1/systemone` 带了 `stream` | 网关 | 改请求，**不要重发** |
| `401` | 凭证缺失、格式不对、不存在或已过期 | 网关 | 换一把 key |
| `405` | 方法不是 `POST` | 网关 | 改方法 |
| `409` | 幂等键的三个情形（同键不同体 / 同键还在跑 / 同键上次没走完） | 网关 | 见 [idempotency.md](idempotency.md)，三条的处置**相反** |
| `413` | 请求体超过 8 MiB | 网关 | 改小请求 |
| `415` | `Content-Type` 不是 `application/json` | 网关 | 改请求头 |
| `429` | 这个 Key 的 RPM 用满 / 这个 Key 的累计额度用尽 / 用户钱包可用余额不足 / **上游**限流 | 前三条是网关，第四条是上游 | 四个来源的处置**两两相反**，见 [limits.md](limits.md) |
| `500` | 我们这一侧坏了（账本、候选来源、鉴权查询失败、认不出来的错误） | 网关 | 上报，附 `X-Request-Id` |
| `501` | 路径不在上面那四条里；这个构建没接推理（例如 `worker` 角色）；没配上游执行器 | 网关 | 换路径或改部署 |
| `502` | 上游调用失败，底下有四种起因：上游那一侧失败（回了非 2xx，或者没答上来）/ 候选全被本地判据挡在门外 / 候选的渠道类型不支持这条协议 / 我们这一侧失败（插件阻断、账本写不进去） | 上游那一类由我们转述，另三类是网关判的 | 按 `error.code` 分，见下文那张码表——**不是每一种都能重发**；我们这一侧那一种不给码，码表下面那一段说的就是它 |
| `503` | `no available upstream for the requested model` | 网关 | 这个词底下**没有可用渠道**。它不点名是哪个名字不对，`larkaxctl doctor` 会，见 [development.md](development.md) |

两条刻意的口径：

- **上游说「你的请求不对」（4xx）时不跟着回 4xx**，一律 `502`。跟着回 `400` 会让
  调用方去改一个本来正确的请求，而真正要改的是我们与上游之间的适配。
- **`429` 的状态码不足以分派**：四个来源里两个该重发、两个不该（见上面的表），
  所以分派必须按**机器可读的码**，见 [limits.md](limits.md)。

## 错误体的形状

形状与**路径对应的协议**绑定：用 OpenAI 格式调用就回 OpenAI 形状，Anthropic
同理。这不是文案问题——调用方的 SDK 按自己那套解错误体，形状不对时它连错误类型
都读不出来，于是「参数写错了」会表现为「服务返回了无法解析的响应」。

三套形状，加两处归并：

| 路径 | 形状 | 码放在哪 |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `error.code` |
| `/v1/responses` | OpenAI（**逐字相同**：两家共用同一个官方 SDK 的错误解析器） | `error.code` |
| `/v1/messages` | Anthropic | `error.type` |
| `/v1/systemone` | OpenAI | `error.code` |
| 未知路径（`501`） | OpenAI（路径不认识，就无从知道调用方说哪套协议） | `error.code` |

```json
{"error":{"message":"invalid credentials","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}
```

```json
{"type":"error","error":{"type":"authentication_error","message":"invalid credentials"}}
```

**OpenAI 那套要按 `error.code` 分支，Anthropic 那套要按 `error.type` 分支。**
同一个失败在两套里落在不同字段上，这是两家协议的差别，不是我们的选择。
**我们自己给的码也照这条走**：Anthropic 那套只有 `error.type` 一个机器可读的
位置，码就写在那里、把推出来的类型名顶掉——幂等的三条 409、额度那两条 429、
上游失败那几档都是这样。

`type` / `error.type` 由状态码推出来，调用方据此粗分「改请求 / 换凭证 / 稍后重试」：

| 状态码 | OpenAI 的 `type` | OpenAI 的 `code` | Anthropic 的 `error.type` |
|---|---|---|---|
| `400` / `405` / `415` / `409` | `invalid_request_error` | 推不出来时为 `null`（幂等那三条会给具体的码） | `invalid_request_error`（幂等那三条这一栏放的是码） |
| `401` | `invalid_request_error` | `invalid_api_key` | `authentication_error` |
| `413` | `invalid_request_error` | `request_too_large` | `request_too_large` |
| `429` | `rate_limit_error` | 推不出来时为 `null`（额度那两条给 `insufficient_quota`，上游限流给 `upstream_http_error`） | `rate_limit_error`（额度那两条与上游限流这一栏放的是码） |
| `501` | `not_implemented` | 推不出来时为 `null` | `not_implemented` |
| 其余 | `api_error` | 推不出来时为 `null` | `api_error` |

除了由状态码推出来那两个，还有几个码**只能读 `code` 才知道**——它们落在同一个
状态码上，而各自的下一步完全不同：

| `error.code` | 状态码 | 什么时候给 | 调用方该做什么 |
|---|---|---|---|
| `no_candidate_channel` | `503` / `502` | `503`：这个模型一条渠道都没挂上。`502`：候选有，但全被本地判据挡在门外（能力、上限、健康度、额度），**一次上游调用都没发生** | 改渠道配置或模型目录声明；重试多少次都是同一个结果 |
| `channel_type_unsupported` | `502` | **试过的**候选都不支持这条协议：每条都是被渠道类型挡下的。被本地状态（额度、健康度）挡掉的候选不在这句话的范围里 | 换一条类型对的渠道 |
| `upstream_http_error` | `502` / `429` | 上游那一侧失败：真的回了非 2xx，或者**没答上来**（连不上、TLS 握手失败、响应头超时——上游可能根本没收到这次请求，这一条码分不出这两种）；`429` 那一档是上游限流——OpenAI 那侧 `error.type` 仍是 `rate_limit_error`、变的是 `error.code`（原来是 `null`），Anthropic 那侧码落在 `error.type`，所以 `rate_limit_error` 在它这里不再出现 | `429` 与「没答上来」都可以退避重试；其余看 `message` 与日志：改请求，或者换上游 |

**我们这一侧**的失败也是 `502`，它就是状态码表里那第四种起因：候选都失败了，
而最后一个错误不是上游那一侧的（插件阻断、账本写不进去）。这一种**不给码**
——把它标成上游那一侧的失败，调用方会照码去查上游，而该查的是我们。

Anthropic 那套没有 `error.code`，这几个码写在 `error.type` 上（见上面那张形状
表）：那一侧连 `type` 也跟着变，调用方照样分支得出来。

**`message` 里不回上游的错误原文，也不回内部路径与标识**：上游的错误体里可能
回显请求内容，而调用方不需要它也知道该改什么。要细节就拿 `X-Request-Id` 去查
日志。

## 流式

三条路径支持流式（请求体里 `stream: true`）。响应的第一件事就是写状态码与响应头，
**之后任何失败都只能作为流里的事件表达**——一个已经发出去的 `200` 没法改回
`500`。

帧的形状按协议分两种：OpenAI 只用 `data:` 行，Anthropic 与 Responses 是
**带名字的事件**（`event: <名字>` 加一行 `data:`）。

### 三套事件序列

```text
OpenAI      data: {"choices":[{"delta":{...}}]}     # 增量
            data: {"choices":[{...,"finish_reason":"stop"}],"usage":{...}}
            data: [DONE]

Anthropic   event: message_start
            event: content_block_start → content_block_delta… → content_block_stop
            event: message_delta      # 收尾的 stop_reason 与输出用量
            event: message_stop

Responses   event: response.created
            event: response.output_item.added
                   → response.output_text.delta… → response.output_text.done
                   → response.output_item.done
            event: response.completed     # 或 response.incomplete
```

Responses 的每一帧都带 `sequence_number`（从 `0` 起递增，官方 SDK 的事件模型
把它标成必填）。它按 `output_index` 把增量拼回项里：正文项在第一个正文增量到达时
才分配下标，工具项在它第一次出现时分配；正文项只有一项，工具项按出现顺序排在它
后面。

### 没有结束标记就是被截断

**只有真的收到上游的 done 事件才写结束标记**（`[DONE]` / `message_stop` /
`response.completed`）。上游中途断了、连接被关掉，都不写——伪造一个正常结束会让
「回答被截断」看起来像「模型答完了」，而调用方会据此把半截结果当完整结果用。
所以：**帧序列里没有结束标记，就是截断的信号。**

### 流中途的错误

| 路径 | 帧 |
|---|---|
| `/v1/chat/completions` | 一个 `data:` 帧，`error.type` 是 `server_error` |
| `/v1/messages` | `event: error`，`error.type` 是 `api_error` |
| `/v1/responses` | `event: error`，平铺的 `code` / `message`（**没有 `error` 外层**），仍带 `sequence_number` |

三处共用的两条：**写完这一帧就结束这条流**，以及 `message` 里不回上游原文。
**官方 SDK 拿到这三种帧之后各是什么反应**（一个抛异常、一个按事件名认、一个
不抛而是交给调用方一个事件对象），见 [contract/README.md](../contract/README.md)
的「流内错误帧」一节——那一份是用真 SDK 钉出来的，这里只写帧本身长什么样。
失败原因分得出三种，只体现在 `message` 上：认不出来的失败、余额不足、以及响应
已经开始产出之后被中断（`the response was interrupted before it finished`）。
余额那一条的措辞与请求前那条 `429` 刻意不同——请求已经发出去了，它要说清这段
回答**是被截断的**，不能当成完整回答用：

```text
the account balance ran out while this response was streaming, so it was cut short; top up the wallet before retrying
```

## 与上游契约的已知偏差

完整的清单在 [contract/README.md](../contract/README.md) 的「已知偏差」一节
（那一份由真 SDK 的契约测试钉着，含跨协议流式的 `input_tokens` 是估算、
`[DONE]` 在 SDK 那一侧看不到、以及我们总是给流式响应带 usage 这几条）。
这里只列**调用方在自己的响应里直接看得到的形状差异**：

- **OpenAI 的用量合并在结束分片里**，而官方是在它之后另发一个 `choices` 为空的
  分片。分开写会让按 `choices[0]` 取值的朴素客户端读到空数组而崩；合并则是多一个
  它们不认识的字段——后者是更安全的偏差。
- **流式与同协议的非流式 5xx 用了不同的错误类型**：流式帧里是 `server_error`
  （Anthropic 那一侧是 `api_error`），非流式的 `error.type` 是 `api_error`——而
  Anthropic 那一栏放的是码 `upstream_http_error`（它没有 `error.code`，见上面那张
  表）。这是现状，统一它属于对外契约变更，按 [versioning.md](versioning.md) 的
  版本策略走。
- **引用标注（citation / annotation）只在 Responses 这条路上有事件**
  （`response.output_text.annotation.added`）。chat 与 Anthropic 两侧都不转它：
  chat completions 的分片里没有它的位置（那会是 relaykit 的 dto 扩字段，一次
  契约变更），Anthropic 的对应物要挂在具体正文块上、字段映射也没定。
- **Anthropic 侧的工具块一旦打开就开到流末尾**，多个工具块可以同时开着。统一事件
  流里没有「这个工具调用说完了」这件事，中途关掉会让后面的参数分片落在自己的
  `content_block_stop` 之后——两个官方 SDK 的累加器都接得住同时开着的块。
- **`/v1/responses` 有几条入口级的拒法**（有状态的字段被拒、非 function 的工具
  声明被拒、`store` 被丢弃），同样是刻意的，见 [contract/README.md](../contract/README.md)
  的「Responses 入口上的四条」。

## 改它的时候

| 要改什么 | 改哪 |
|---|---|
| 路径与协议的对应、路径外的 `501` | `internal/api` 的 `ServeHTTP`（`PathChatCompletions` 等四个常量） |
| 解析之前的检查顺序（方法、`Content-Type`、鉴权、限流、额度） | `internal/api` 的 `admit` |
| 状态码与错误码的映射 | `internal/api/error.go` 的 `errorKind` 与 `internal/api/api.go` 的 `httpStatusOfUpstream` |
| 错误体的形状 | `internal/api/error.go`（每套协议一份独立结构体） |
| 请求体上限 | `internal/api` 的 `maxRequestBodyBytes` |
| 流式的帧序列 | `internal/api/stream.go`（每套协议一个渲染器） |
| 流中途的错误帧 | `internal/api/stream.go` 的 `streamErrorFrame` 与 `responsesStream.errorFrame` |
| 响应头 | `internal/api` 的 `HeaderVersion` / `HeaderRetryAfter`、`internal/observe` 的 `HeaderRequestID` |
