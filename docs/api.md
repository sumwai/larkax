# API 参考

这份写**调用方看到的那一层**：有哪几条路径、每个响应带什么头、什么情况回什么
状态码、错误体长什么样、流式的帧序列是什么。协议的语义（怎么路由、怎么计费、
限流怎么算）不在这里，各自的文档见每一节的指向。

## 路径与协议

**协议由路径决定，不由请求体决定。** 推理有四条路径，模型清单有两条只读路径，
其余一律 `501`——唯一可能的例外是支付回调，它只在假支付通道启用时才挂上
（见下面的「支付回调」）。

| 路径 | 协议 | 认证习惯 | 流式 |
|---|---|---|---|
| `POST /v1/chat/completions` | OpenAI | `Authorization: Bearer <key>` | 是 |
| `POST /v1/messages` | Anthropic | `x-api-key: <key>` | 是 |
| `POST /v1/responses` | OpenAI Responses | `Authorization: Bearer <key>` | 是 |
| `POST /v1/systemone` | System One（本网关自己的形状） | `Authorization: Bearer <key>` | **否** |
| `GET /v1/models` | OpenAI | `Authorization: Bearer <key>`，也认 `x-api-key` | 否 |
| `GET /v1/models/{id}` | OpenAI | 同上 | 否 |

`/v1/systemone` 不做对话，而是拿一段状态与一组带类型的判定问题、回一组答案，
形状见 [development.md](development.md) 的「决策接口」一节。它在请求体里带
`stream` 时回 `400`，文案由 `internal/systemone` 给出。

**其余路径一律 `501`**（`this endpoint is not implemented`）：上面这六条之外的
路径都没有实现，包括 `/v1/embeddings`、`/v1/files` 这些 OpenAI 兼容接口的惯例
端点。

### 模型清单

两条只读路径，用与推理请求同一把 API Key 认证：

| 路径 | 返回 |
|---|---|
| `GET /v1/models` | 这把 key 现在能用的模型清单 |
| `GET /v1/models/{id}` | 其中一条；不可用时 `404` |

**「能用」的判据是供给，不是健康度**：规范名在模型目录里，并且至少有一条
**启用、未删除**的渠道声明支持它（`channels.models` 数组含这个规范名）。
目录里有、但没有任何渠道能接到它的模型**不返回**——列出来调用方一用就是
失败，比不列更糟。渠道的健康度与额度不在这条判据里：一个正在报错的渠道仍然
在供这个模型，它这一刻能不能打通要等真的发一次请求才知道。

响应形状与 OpenAI 的 Model 对象一致，SDK 可以直接解；`offers` 是这里多加的一项：

```json
{"object": "list", "data": [
  {"id": "claude-x", "object": "model", "created": 0, "owned_by": "example",
   "offers": [
     {"merchant": "acme", "price": "0.00350000",
      "prices": {"prompt": "0.00350000", "completion": "0.01050000", "cache_read": "0.00070000"}},
     {"merchant": null, "price": "0.00400000",
      "prices": {"prompt": "0.00400000"}}
   ]}
]}
```

- `offers` 是供应这个模型的**报价**，每项一条：
  - `merchant` 是渠道所属商家的用户名；`null` 表示平台自营渠道
  - `price` 是**每 1000 个输入 token** 的金额，字符串（定点数，不经过浮点）
  - `prices` 是各 token 维度的千 token 金额，键为 `prompt`、`completion`、
    `cache_read`、`cache_write`、`reasoning`；**只含规则显式写过的维度**。
    依次是：非缓存的输入 token、输出 token（不含推理）、命中缓存的输入、
    写入缓存的输入、推理 token。每个维度各按「1000 个该维度 token」单独求值
    ——规则可以引用用量，缓存读与推理的单价通常与输入价不成倍数，不能拿
    `price` 去推
  - 按 `price` 升序；同一个商家供多条渠道时，**整套维度价完全相同**才折成
    一条，任一维度不同就是两条不同的报价
  - 没有任何适用规则的模型**仍然列出**，只是不带 `offers`：清单回答「能不能
    调通」，报价回答「谁在供、多少钱」，两者不是同一个条件
  - `price` 等于 `prices.prompt`；规则没有声明输入价时它是 `"0.00000000"`，
    此时要判断「输入免费」还是「没写输入价」，得看 `prices` 里有没有
    `prompt` 这个键
- 报价与结算用同一段规则匹配与同一个求值函数（见 `internal/pricing`），
  所以它是「走这条渠道时按规则算出来的价」，不是另算的一份展示价。
  固定费与最低消费不进报价——它们属于一次请求，不属于每千 token 的单价。
- `offers` 为空时省略这个字段，不输出空数组；`prices` 为空时同样省略
- `id` 是**规范名**，也就是请求体里 `model` 字段该写的那个名字。别名只在
  路由时用于把规范名翻成上游模型名，不出现在清单里
- `created` 固定 `0`：目录里没有「模型创建时间」这一列，不编一个看起来像
  时间戳的值
- `owned_by` 取目录里的 vendor；那一列为空时是 `larkax`
- 清单按规范名排序，**不做分页**：规模小，等真有需要再说
- 不返回上游模型名与渠道信息——那是运营面的事

失败时（错误体形状与推理路径相同）：

| 状态码 | 什么时候 | 码 |
|---|---|---|
| `401` | 没有带凭证，或凭证不能用 | `invalid_api_key` |
| `404` | `{id}` 不可用：目录里没有，或没有任何启用渠道在供 | `model_not_found` |
| `405` | 方法不是 `GET`（响应头带 `Allow: GET`） | — |
| `500` | 我们这一侧读不出来（连不上库等） | — |
| `501` | 这个实例没有接上模型清单 | — |

这两条路径**不参与限流与额度**：它们只读，不产生用量。它们也不判渠道凭证
能不能解开——那要逐渠道解密，代价与这条只读接口不匹配，核查由
`larkaxctl doctor` 那条链路负责。

### 控制台单模型页

`GET /api/v1/models/{name}` 是控制台模型页（`/app/models/[model-id]`）的数据
来源。它**公开只读、不要求会话**，与模型清单同一个理由：模型与价格本来就
是公开信息。

可见性判据与模型清单完全相同：规范名在目录里，并且至少有一条启用、未删除的
渠道声明支持它。**不在供与目录里没有回同一个 `404`**，正文逐字相同——否则
调用方能靠状态码探出某个名字在不在目录里。

```json
{
  "currency": "CNY",
  "reference_at": "2026-10-21T09:00:00Z",
  "model": {
    "name": "claude-x",
    "display_name": "Claude X",
    "vendor": "Anthropic",
    "family": "claude",
    "context_limit": 200000,
    "input_limit": 200000,
    "output_limit": 64000,
    "capabilities": {
      "reasoning": true,
      "tools": false,
      "vision": null,
      "audio": null,
      "cache": true,
      "batch": null,
      "endpoints": ["chat"]
    },
    "modalities_input": ["text", "image"],
    "modalities_output": ["text"],
    "reasoning_options": [{"type": "toggle"}],
    "canonical_model_id": "claude-x-2026",
    "knowledge_cutoff": "2025-03-01",
    "release_date": null,
    "updated_at": "2026-10-21T08:30:00Z",
    "suppliers": [
      {"merchant": "acme", "price": "0.00200000", "prices": {"prompt": "0.00200000"},
       "channel_count": 2},
      {"merchant": null, "price": "0.00300000", "prices": {"prompt": "0.00300000"},
       "channel_count": 1}
    ],
    "unpriced_channels": 1
  }
}
```

模型字段：

- `display_name` 为空时取规范名，与模型清单一致。
- `context_limit` / `input_limit` / `output_limit` 为 `0` 表示目录里**没填**，
  不是「上限是 0」。
- `capabilities` 六项与 `endpoints` 都是三态，原样透传目录里的值：
  `null` 表示没采集过，`false` 表示明确不支持，两者不能合并。
  `endpoints` 的 `[]` 表示**明确声明一条都不支持**，与 `null`（未知）不同。
- `knowledge_cutoff` / `release_date` 是 `YYYY-MM-DD` 或 `null`；
  `updated_at` 是 RFC3339 UTC。
- 不返回采集来的 `raw`：那是运维排查用的原始元信息。

`suppliers` 是供应这个模型的商家与报价：

- 一行 = **商家（平台自营单列）× 一整套维度价**：同一商家、所有维度价格
  都相同的多条渠道折成一行，`channel_count` 记这套价的渠道条数；任一维度
  不同则是两行。按 `price` 升序，同价时平台自营在前，再按商家名。
- `merchant` 是渠道所属商家的用户名，`null` 表示平台自营渠道。
- `price` 是**每 1000 个输入 token** 的金额，字符串（定点数，不经过浮点）。
  `prices` 是各 token 维度的千 token 金额，键为 `prompt`、`completion`、
  `cache_read`、`cache_write`、`reasoning`，只含规则显式写过的维度。含义与
  模型清单一节相同：非缓存输入、输出（不含推理）、缓存命中/写入的输入、推理
  token。每个维度各按「1000 个该维度 token」单独求值，规则可以引用用量，
  所以不要用 `price` 去推其他维度。
- `price` 等于 `prices.prompt`；规则没有声明输入价时它是 `"0.00000000"`，
  此时要判断「输入免费」还是「没写输入价」，得看 `prices` 里有没有 `prompt`
  这个键。`price` 由 `internal/pricing` 按这条渠道实际适用的规则求出，与结算
  同源；固定费与最低消费不进报价——它们属于一次请求，不属于每千 token 的
  单价。
- 只列能算出价的渠道：一条适用规则都没有的渠道给不出这个数，编一个 0 会
  变成「免费」。这些渠道由模型级的 `unpriced_channels` 计数回答（总是输出，
  没有这种渠道时是 `0`）。整个模型一条报价都没有时仍然返回 `200`，
  `suppliers` 字段省略（不是空数组）。
- 不返回渠道 id、`base_url`、凭证、上游地址与上游模型名——上游名会把调用方
  引去绕过网关。
- `reference_at` 是这次求价的时刻；报价随规则分时变化，以结算金额为准。
- 这条响应的 `Cache-Control` 是 `public, max-age=60`：它只依赖目录与计价
  规则，不随调用方变化。

### 支付回调

| 路径 | 谁调 | 认证 | 流式 |
|---|---|---|---|
| `POST /v1/payments/callback/fake` | 支付通道（不是 API 调用方） | 通知体里的签名，**没有 API Key** | 否 |

这条路径**只在 `settings.payment.fake_enabled` 为 `true` 时才挂上**；关着时它
与其余未知路径一样回 `501`。打开它之前先看
[settings.md](settings.md) 的「支付」一节：假通道的签名密钥是源码里的公开
占位值，任何人都能伪造一份「已支付」的通知，它只用于本地联调。

请求体上限 64 KiB，`Content-Type` 必须是 `application/json`。成功时回 `200`
与一份 JSON：

```json
{"ok": true, "order_no": "ORD-20261020-1", "applied": true, "ignored": false, "status": "paid"}
```

`applied` 为 `false` 表示这次没有新写分录（同一份通知重发）。`ignored` 为
`true` 表示这张单子已经在终态（`failed` / `expired` / `refunded`），状态机不
往回走。两者都回 `200`：通道看到非 2xx 会重发，而重发并不会改变结果。

失败时按原因回：

| 状态码 | 什么时候 | 响应体 |
|---|---|---|
| `400` | 通知缺字段，或金额、资产、状态读不出来 | `{"error": "<具体哪一项>"}` |
| `401` | 签名对不上 | `{"error": "signature mismatch"}`，**不回显解析出来的字段** |
| `404` | 没有这张支付单 | `{"error": "<原因>"}` |
| `405` | 方法不是 `POST` | 带 `Allow: POST` |
| `409` | 单号已经是一条内容不同的支付单，或通知金额/资产与下单时不一致 | `{"error": "<原因>"}` |
| `415` | `Content-Type` 不是 `application/json` | `{"error": "<原因>"}` |
| `500` | 我们这一侧失败（账本写不进去等） | `{"error": "failed to process payment notification"}` |

**回调本身是幂等的**：幂等键是商户单号，落在 `payment_orders.order_no` 的唯一
约束上。同一份通知重发不改变分录与余额；同一单号配不同的手续费或金额不是重放，
而是 `409`。

**入账与支付单状态在同一个事务里写**：分录、余额快照与 `payment_orders.status`
要么一起落，要么一起不落。任何一步失败都是 `500`，库里不会留下「钱进了账、单子
还停在待支付」或反过来「单子已支付、账上什么都没有」的中间态。

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
| `401` | 凭证缺失、格式不对、不存在或已过期（推理与模型清单都是这一条）；支付回调那条路径上是**签名不对** | 网关 | 换一把 key；回调路径看「支付回调」一节 |
| `404` | 模型清单上这个 `{id}` 不可用：目录里没有，或没有任何启用渠道在供 | 网关 | 换一个模型，见「模型清单」一节 |
| `405` | 方法不对：推理路径只收 `POST`，模型清单只收 `GET` | 网关 | 改方法 |
| `409` | 幂等键的三个情形（同键不同体 / 同键还在跑 / 同键上次没走完） | 网关 | 见 [idempotency.md](idempotency.md)，三条的处置**相反** |
| `413` | 请求体超过 8 MiB | 网关 | 改小请求 |
| `415` | `Content-Type` 不是 `application/json` | 网关 | 改请求头 |
| `429` | 这个 Key 的 RPM 用满 / 这个 Key 的累计额度用尽 / 用户钱包可用余额不足 / **上游**限流 | 前三条是网关，第四条是上游 | 四个来源的处置**两两相反**，见 [limits.md](limits.md) |
| `500` | 我们这一侧坏了（账本、候选来源、鉴权查询失败、认不出来的错误） | 网关 | 上报，附 `X-Request-Id` |
| `501` | 路径不在上面那几条里；支付回调在通道关着时也落回这一条；这个构建没接推理（例如 `worker` 角色）或没接模型清单；没配上游执行器 | 网关 | 换路径或改部署 |
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
| `/v1/models`、`/v1/models/{id}` | OpenAI | `error.code` |
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
| `404` | `invalid_request_error` | `model_not_found` | —（模型清单只走 OpenAI 那套） |
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
| `upstream_http_error` | `502` / `429` | 上游那一侧失败：真的回了非 2xx，或者**没答上来**（连不上、TLS 握手失败、响应头超时——上游可能根本没收到这次请求，这一条码分不出这两种）；`429` 那一档是上游限流——OpenAI 那侧 `error.type` 仍是 `rate_limit_error`、变的是 `error.code`（原来是 `null`），Anthropic 那侧码落在 `error.type`，所以 `rate_limit_error` 在它这里不再出现 | `429` 与「没答上来」都可以退避重试；其余看 `message` 与日志：改请求，或者换上游。**名字是规范名、上游却回「模型不存在」时，缺的是 alias**（见 [README](../README.md) 里 `doctor` 那一段） |

同一个码在账本那一侧是**窄**的：开了请求快照的部署里，`upstream_status` 那一格记的是
这次调用真的拿到过的状态码，一个响应都没拿到时它是空的；`error_kind` 那一格则记
`upstream_no_answer`。响应体回答不了「上游到底收没收到这次请求」，账本回答得了。
两列的读法见 [development.md](development.md) 的「快照的两个方向与上游证据两列」一节。

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
- **流式与同协议的非流式 5xx 用了不同的错误类型**：OpenAI 与 Responses 那两套里，
  流式帧是 `server_error`；非流式的 `error.type` 是 `api_error`。Anthropic 那侧
  流式帧是 `api_error`，非流式那一栏放的是码 `upstream_http_error`
  （那套协议没有 `error.code`，见上面那张表）。这是现状，统一它属于对外契约变更，按
  [versioning.md](versioning.md) 的版本策略走。
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
| 路径与协议的对应、路径外的 `501` | `internal/api` 的 `ServeHTTP`（`PathChatCompletions`、`PathMessages`、`PathResponses`、`PathSystemOne`、`PathModels`） |
| 模型清单的内容（列出哪些模型、`owned_by`、单条不可用时的 `404`） | `internal/api/models.go`；供给判据在 `internal/channels` 的 `SuppliedModels` |
| 解析之前的检查顺序（方法、`Content-Type`、鉴权、限流、额度） | `internal/api` 的 `admit` |
| 状态码与错误码的映射 | `internal/api/error.go` 的 `errorKind` 与 `internal/api/api.go` 的 `httpStatusOfUpstream` |
| 错误体的形状 | `internal/api/error.go`（每套协议一份独立结构体） |
| 请求体上限 | `internal/api` 的 `maxRequestBodyBytes` |
| 流式的帧序列 | `internal/api/stream.go`（每套协议一个渲染器） |
| 流中途的错误帧 | `internal/api/stream.go` 的 `streamErrorFrame` 与 `responsesStream.errorFrame` |
| 响应头 | `internal/api` 的 `HeaderVersion` / `HeaderRetryAfter`、`internal/observe` 的 `HeaderRequestID` |
