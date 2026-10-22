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
      "prices": {"prompt": "0.00350000", "completion": "0.01050000", "cache_read": "0.00070000"},
      "request": "0.00200000", "minimum": "0.01000000",
      "units": {"cache_write_5m": "0.00050000"}},
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
  - `request` 是**每次请求**的固定费用（元），`minimum` 是每次请求的最低
    消费（元）。量纲与每千 token 的金额不同，不能相加；为 0 时省略这个键
    （规则没写与写了 0 元都不收费，省略比给一个 `"0.00000000"` 更少被读成
    「确认免费」）。两者可以写成用量的函数，这里报的是 `prices.prompt` 那
    一次求值（1000 个输入 token）下的值
  - `units` 是非 token 计量项的单价，键是规则里写过的计量项名、值是**每个
    计量单位**多少元。`units` 没有计量项时整个键不出现；有计量项时，单价
    为 0 的那一项仍然输出 `"0.00000000"`——省略的是字段，不是某一项。这三项与
    `prices.prompt` 取自同一次求值（1000 个输入 token），不与 `prices` 里
    其他维度同一次
  - 按 `prompt` 那一项的价格升序；规则没写输入价时用第一个存在的维度代替。
    同一个商家供多条渠道时，**整套价完全相同**（各维度、`request`、`minimum`
    与 `units` 都相同）才折成一条，任一不同就是两条不同的报价
  - 没有任何适用规则的模型**仍然列出**，只是不带 `offers`：清单回答「能不能
    调通」，报价回答「谁在供、多少钱」，两者不是同一个条件
  - 规则写了输入价时 `price` 等于 `prices.prompt`；规则没有声明输入价时它
    是 `"0.00000000"`，此时要判断「输入免费」还是「没写输入价」，得看
    `prices` 里有没有 `prompt` 这个键
- 报价与结算用同一段规则匹配与同一个求值函数（见 `internal/pricing`），
  所以它是「走这条渠道时按规则算出来的价」，不是另算的一份展示价。
  固定费、最低消费与计量项单价各有自己的字段，量纲与每千 token 的金额不同。
- `offers` 为空时省略这个字段，不输出空数组；`prices` / `units` 为空时同样
  省略
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

### 控制台模型列表

`GET /api/v1/models` 是控制台模型列表页（`/app/models`）的数据来源。它**公开
只读、不要求会话**，理由与模型清单相同。条目里的模型字段来自同一份目录数据
（名称、vendor、上限、能力、模态）；这里只展开另外两块：平台参考价 `pricing`
与按渠道折出的 `offers`。

```json
{"currency": "CNY", "reference_at": "2026-10-21T09:00:00Z",
 "items": [
   {"name": "claude-x", "display_name": "Claude X", "vendor": "Anthropic",
    "context_limit": 200000, "max_output": 64000,
    "capabilities": {"tools": true},
    "modalities": {"input": ["text"], "output": ["text"]},
    "pricing": {"input_per_mtok": "12.00000000", "output_per_mtok": "60.00000000",
                "cache_read_per_mtok": null, "cache_write_per_mtok": null,
                "reasoning_per_mtok": "18.00000000",
                "request": "0.01000000", "minimum": "0.50000000",
                "units": {"cache_write_1h": "0.20000000"}}}
 ]}
```

- `pricing` 只取**平台级**规则（`scope = platform`）；没有平台规则时是
  `null`，不伪造一个起价。求值用 1,000,000 个该维度的 token：输入与缓存读写
  取自「1,000,000 个输入 token」那一次，输出与推理取自「1,000,000 个输出
  token」那一次。
- `input_per_mtok` / `output_per_mtok` 恒有值；`cache_read_per_mtok` /
  `cache_write_per_mtok` / `reasoning_per_mtok` 为 `null` 表示规则**没写**
  这一项，与 0（免费）不是一回事。
- `request` / `minimum` / `units` 的口径与 `/v1/models` 的 `offers` 相同，
  取自「1,000,000 个输入 token」那一次求值：`request` / `minimum` 是每请求
  多少元，`units` 是每个计量单位多少元。`request` 与 `minimum` 为 0 时
  **整个键不出现**；`units` 没有计量项时整个键不出现；有计量项时，单价
  为 0 的那一项仍然输出 `"0.00000000"`——省略的是字段，不是某一项。
  规则把它们写成用量的函数时，报的就是这一次用量下的值。
- **两种「没配」的写法并存**：几个 `*_per_mtok` 用 `null` 表示没配；
  `request` / `minimum` 为 0、`units` 没有计量项时用键不出现表示（前两者
  收费为 0 与没配等价，省略少一种误读）。按一种约定去读另一种，会把
  `null` 当成 0。
- `offers` 与 `/v1/models` 的同名字段**逐字相同**，包括 `price` / `prices`
  是每 1000 个 token 的金额——同一个响应里两种单位并存，展示时必须分别标注。
  没有报价时省略这个字段。
- `currency` 与 `reference_at` 的口径与「控制台单模型页」相同。

### 控制台单模型页

`GET /api/v1/models/{name}` 是控制台模型页（`/app/models/[model-id]`）的数据
来源。它**公开只读、不要求会话**，与模型清单同一个理由：模型与价格本来就
是公开信息。

可见性判据与模型清单完全相同：规范名在目录里，并且至少有一条启用、未删除的
渠道声明支持它。**不在供与目录里没有回同一个 `404`**，正文逐字相同——否则
调用方能靠状态码探出某个名字在不在目录里。

`{name}` 由多段通配符接住，取值按 `net/http` 的规则**逐路径段反转义、再以
`/` 拼接**：`vendor%2Falpha` 与 `vendor/alpha` 是同一个名字；`%25` 解成字面量
`%` 且只解一层，所以 `vendor%252Falpha` 对应名字 `vendor%2Falpha`。

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
       "request": "0.00100000", "minimum": "0.00500000",
       "units": {"cache_write_1h": "0.00020000"},
       "rule": {"id": 42, "scope": "merchant", "priority": 10,
                "effective_from": "2026-01-01T00:00:00Z", "effective_to": null},
       "channel_count": 2},
      {"merchant": null, "price": "0.00300000", "prices": {"prompt": "0.00300000"},
       "rule": {"id": 7, "scope": "platform", "priority": 0,
                "effective_from": "2026-01-01T00:00:00Z", "effective_to": null},
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
- `reasoning_options` 只有两态：`[]` 表示没有可选项，非空数组表示有哪几项。
  这个字段**没有 `null` 态**——列是 `NOT NULL DEFAULT '[]'`。库里即便出现
  字面量 JSON `null`（异常数据），响应里也是 `[]`：那是把异常值抹平，不是
  第三种状态。
- `knowledge_cutoff` / `release_date` 是 `YYYY-MM-DD` 或 `null`。
- `updated_at` 是目录行的**写入时刻**，RFC3339 UTC：创建时由列的默认值
  `now()` 置初值（`internal/migrate/sql/00001_init.sql`），之后每次修改由
  `internal/catalog` 的 `Admin.Update` 刷新（`updated_at = now()`）。它不是
  上游发布或更新模型的时间。发布日期的字段是 `release_date`。
- 不返回采集来的 `raw`：那是运维排查用的原始元信息。

`suppliers` 是供应这个模型的商家与报价：

- 一行 = **商家（平台自营单列）× 一整套价**：同一商家、所有维度价格、固定费、
  最低消费与计量项单价都相同的多条渠道折成一行，`channel_count` 记这套价的
  渠道条数；任一项不同则是两行。按 `prompt` 那一项的价格升序，规则没写输入价
  时用第一个存在的维度代替；同价时平台自营在前，再按商家名，同商家同价时按
  整套价的内容排。
- `merchant` 是渠道所属商家的用户名，`null` 表示平台自营渠道。
- `price` 是**每 1000 个输入 token** 的金额，字符串（定点数，不经过浮点）。
  `prices` 是各 token 维度的千 token 金额，键为 `prompt`、`completion`、
  `cache_read`、`cache_write`、`reasoning`，只含规则显式写过的维度。含义与
  模型清单一节相同：非缓存输入、输出（不含推理）、缓存命中/写入的输入、推理
  token。每个维度各按「1000 个该维度 token」单独求值，规则可以引用用量，
  所以不要用 `price` 去推其他维度。
- `request` 是**每次请求**的固定费用（元），`minimum` 是每次请求的最低消费
  （元），`units` 是非 token 计量项的单价（**每个计量单位**多少元）。量纲与
  每千 token 的金额不同，不能相加。`request` 与 `minimum` 为 0 时整个键
  不出现；`units` 没有计量项时整个键不出现；有计量项时，单价
  为 0 的那一项仍然输出 `"0.00000000"`——省略的是字段，不是某一项。三者取自
  `prices.prompt` 那一次求值（1000 个输入 token）；规则把它们写成用量的函数
  时，报的就是这一次用量下的值。
- `rule` 说明这套价出自哪条规则：`id`、`scope`（`channel` / `merchant` /
  `platform`）、`priority`、`effective_from` 与 `effective_to`（RFC3339 UTC，
  `null` 表示不过期）。同价的多条渠道折成一行时，它是其中渠道编号最小的那条
  渠道适用的规则（渠道编号本身不出现在响应里）。**每条 supplier 都带
  `rule`**：只有匹配到规则的渠道才算得出价，没有匹配到规则的渠道不进
  `suppliers`（计入 `unpriced_channels`），所以出现的一定来自某条规则。
  不含表达式源码、owner id、channel id 与 `base_url`。
- 规则写了输入价时 `price` 等于 `prices.prompt`；规则没有声明输入价时它是
  `"0.00000000"`，此时要判断「输入免费」还是「没写输入价」，得看 `prices`
  里有没有 `prompt` 这个键。`price` 由 `internal/pricing` 按这条渠道实际
  适用的规则求出，与结算同源；固定费、最低消费与计量项单价各占一个字段，
  不属于每千 token 的单价。
- 只列能算出价的渠道：一条适用规则都没有的渠道给不出这个数，编一个 0 会
  变成「免费」；命中的规则编译或求值失败的渠道同样算不出价。这些渠道
  都由模型级的 `unpriced_channels` 计数回答（总是输出，没有这种渠道时是
  `0`）。整个模型一条报价都没有时仍然返回 `200`，
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

## 控制台管理接口：用户

这三个端点只挂在公共监听的 `/api/v1/admin/users`，要求控制台会话（HttpOnly
Cookie）与控制台折出来的 `admin` 角色；写方法另要 `X-CSRF-Token`。**内部管理面
`/admin/v1/` 上没有这一组**：那里的调用者是运维令牌，没有「自己」这个概念，
而这段接口有两条判据离不开操作者——不能改自己的角色/状态、不能撤掉最后一个
active 管理员。对应的装配见 `internal/adminapi` 的 `GroupUsers` 与
`InternalGroups`。

角色只有三档，以 `users.is_merchant` / `users.is_admin` 两列落库，对外的
`role` 由 `Role.Flags()` / `roleOf` 折出来（`admin` 同时也是 merchant）。
**不要实现第四份映射**。

### 清单

`GET /api/v1/admin/users?query=&role=&status=&limit=&cursor=`

- `query` 在用户名与邮箱上做不区分大小写的包含匹配；`%`、`_`、`\` 按字面量
  转义，搜一个 `%` 不会列出全部用户。
- `role` 取 `user`、`merchant`、`admin`；`merchant` 的口径是「`is_merchant`
  为真**且** `is_admin` 为假」，所以 `admin` 不会混进来。`status` 取
  `active`、`suspended`、`banned`（销户流程里的两种状态不在这一页的筛选里）。
- `limit` 默认 50、上限 200。分页是 id keyset，响应：

```json
{
  "items": [
    {"id": 42, "username": "alice", "email": "a***@example.com",
     "display_name": "Alice", "role": "merchant", "status": "active",
     "created_at": "2026-10-01T08:00:00Z", "last_seen_at": "2026-10-21T09:12:00Z"}
  ],
  "next_cursor": "dTE6NDI"
}
```

- `email` 是打码后的写法（本地部分只留首字符）；`email` 为 `null` 表示这名
  用户没填邮箱。完整邮箱只在详情里给。
- `last_seen_at` 是该用户所有会话 `last_seen_at` 的最大值。除会话探测
  （`GET /auth/session`）与登出外，控制台的受保护请求都会尝试刷新它，但至多
  每分钟写一次库（`internal/auth` 的 `sessionTouchInterval`），所以它的精度是
  分钟；从没登录过、或会话行已被清理的老用户都是 `null`。
- `next_cursor` 为 `null` 表示没有下一页。**游标不绑定筛选条件**：翻页途中
  改 `query` / `role` / `status`，客户端要从第一页重新取，否则会拿上一组条件
  的位置去翻这一组。

### 详情

`GET /api/v1/admin/users/{id}` 返回清单的全部字段，加上：

| 字段 | 含义 |
|---|---|
| `email_full` | 完整邮箱；没填时为 `null` |
| `balance` | 钱包余额，定点数字符串（8 位小数）。没有钱包行是 `0.00000000`，与 `/api/v1/wallet` 同口径 |
| `active_key_count` | `api_keys.status = 'active'` 的把数 |
| `requests_7d` / `amount_7d` | 最近 7×24 小时的请求数（去重到 request）与金额 |
| `channel_count` | 该用户名下未删除的渠道数 |
| `key_last_used_at` | 该用户所有 key 里 `last_used_at` 的最大值；从没用过为 `null` |

`{id}` **只接受十进制正整数**；其余（用户名、`0`、负数、带别的字符）一律
`404`。不能把路径段原样交给按「id 或用户名」解析的那条路——那会让
`/users/alice` 读到 alice。

### 修改

`PATCH /api/v1/admin/users/{id}`，body 只认两个字段：

```json
{"role": "merchant", "status": "active"}
```

- 两个字段都可省略，但至少给一个；空 body、未知字段、未知取值都回 `400`。
- 角色收**一个** `role`，服务端用 `Role.Flags()` 同时写两列。收两个布尔会让
  「admin 且非 merchant」变成一个能发出来的请求，而库里那条
  `users_admin_implies_merchant` 约束会把它变成 `500`。
- 成功时返回与详情相同的响应体。

拒绝规则（各自一句文案，`4xx`）：

| 情形 | 状态码 |
|---|---|
| 改**自己的角色** | `400` |
| 改**自己的状态** | `400`（与上一条文案不同） |
| 把**最后一个 active 管理员**降级、停用或封禁 | `409` |
| 目标是 `deletion_requested` / `deleted` | `409`：前者说明账号正在走销户流程、这一页只管理 active / suspended / banned，要从销户流程撤销；后者说明已经匿名化、不可逆 |
| 目标不存在，或 `{id}` 不是正整数 | `404` |

「最后一个 active 管理员」= `status = 'active' AND is_admin`。判据落在
`auth.UpdateUser` 的事务里（目标行 `FOR UPDATE` + 事务级咨询锁下计数），
所以 `larkaxctl user update --no-admin` 也挡得住；并发两笔降级只有一笔成功。
操作者只来自服务端会话，请求头与 body 里的「actor」不参与判定；每次修改写
一条结构化日志，含操作者 id、目标 id、改动的列与前后值、`request_id`。

### 停用与封禁的语义

这几条是**网关的行为**，页面据此解释，不要各自发明：

- 停用**不需要动 `api_keys`**：每次鉴权都读 `users.status`，所以用户手里所有
  key 一起失效。它们的 `status` 仍是 `active`，列表里也照这样显示——重新启用
  后自动恢复，不需要重新建 key。
- `suspended` 与 `banned` 今天在鉴权上没有区别（只判 `status != 'active'`）；
  两者只差展示与将来的策略。页面不要假装它们有不同后果。
- **新请求拒绝，已建立的连接继续**：非流式请求按原结算走完；流式不会因为
  这次 PATCH 断开。
- 角色改动**立即生效**，不需要吊销会话：控制台每次请求都从 `users` 折角色。

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

## 控制台商家接口：概览、上游、我的模型与渠道表单

这五个端点挂在控制台的 `/api/v1/merchant/` 下，要求控制台会话（HttpOnly
Cookie）与 `merchant` 角色；`admin` 也能进，且**不限定归属**（看全部渠道与模型）。
未登录 `401`，非商家 `403`。成功的响应都是 `application/json`。

其中四个是只读的 `GET`（概览、上游、我的模型、适配器枚举）；
`POST /api/v1/merchant/channels/probe-models` 是写方法：与其余非安全方法一样
必须带会话的 `X-CSRF-Token`，缺了或不匹配回 `403`。

### 适配器与模型探测

建渠道的表单从 `GET /api/v1/merchant/adapters` 取可选类型，用
`POST /api/v1/merchant/channels/probe-models` 先问一次上游有哪些模型。
两个端点与内部管理面的同名端点行为一致，字段、错误码与限流见
[admin-api.md](admin-api.md) 的「渠道表单的输入」；控制台这一侧只多两条：

- 商家会话只能探测**自己的**渠道：`channel_id` 指到别人的渠道时回 `404`，
  与不存在同形。
- 限流按用户计：同一商家两次探测间隔不到 2 秒、或一分钟内超过 10 次时回
  `429` 并带 `Retry-After`。计数是**按实例**的：它在进程内，不跨进程，
  所以多实例部署时整体额度按实例数放大（N 个实例就是每 2 秒 N 次、
  每分钟 10×N 次）。
- 控制台的管理员组与内部管理面没有商家归属，两者共用同一个 `key = 0` 的
  桶：它们互相占额，不按管理员身份分开计。

### 概览

`GET /api/v1/merchant/overview`

```json
{
  "channels": 12,
  "models_in_supply": 34,
  "today": {"requests": 1280, "cost": "12.34000000"},
  "unhealthy_channels": [
    {"id": 7, "name": "…", "reasons": ["failure_rate", "no_requests_24h"]}
  ]
}
```

- `channels`：名下未软删的渠道数（含停用的）。
- `models_in_supply`：名下启用渠道在供的规范模型数。判据与模型清单「在供」那一
  条是同一份（`enabled` + 渠道的 `models` 含该规范名 + 未软删 + 目录里有这一行）。
- `today`：**站点当地的今天**（`meta` 的 `timezone`），不是最近 24 小时——跨零点
  时两者给的不是同一批行。`requests` 是去重到 request 的次数；`cost` 是**售价**
  （调用方被计的那笔钱），不是上游成本，也不是按商家分成后的所得。两个数都
  只含带渠道的用量事件（`channel_id` 非空）；没有渠道的事件不计入这一页的合计。
- `unhealthy_channels`：最近 24 小时窗口内的异常渠道，**没有就是 `[]`，不是
  `null`**。每条带 `reasons`，枚举如下（多条同时成立时全部列出）：
  - `failure_rate`：窗口内失败率 ≥ 20%，且请求数 ≥ 20（样本太少不判）；
  - `quota_state`：额度状态是 `degraded` 或 `drained`；
  - `probe_failed`：最近一次额度探测的结论是 `error`；
  - `consecutive_failures`：当前连续失败数大于 0；
  - `no_requests_24h`：24 小时内一次请求都没有（新渠道也落这一条）。

### 上游

`GET /api/v1/merchant/upstreams?limit=`

```json
{
  "items": [
    {"type": "openai", "base_url": "https://api.example.com/v1",
     "health": "degraded", "requests": 120, "failures": 6, "failure_rate": 0.05,
     "channels": [{"id": 3, "name": "primary", "status": "enabled", "health": "healthy"},
                  {"id": 9, "name": "backup", "status": "enabled", "health": "degraded"}]}
  ],
  "total": 1,
  "truncated": false
}
```

- 按 `(type, base_url)` 分组：同一个地址上的多条渠道是**多个账号**（各自一份
  凭证），归成一条上游、组内列渠道。组内按渠道 id 升序，组间按 `(type,
  base_url)` 升序。
- `health` 取组内最差一档（`healthy` < `unknown` < `degraded` < `drained`）；
  `requests` / `failures` / `failure_rate` 是组内合计，窗口同概览的 24 小时。
- `limit` 默认 50、上限 200，超出回 `400`。超过上限时返回前 `limit` 条，并置
  `truncated: true`、`total` 给总条数——页面据此显示「还有多少条」。
- **响应里没有任何凭证**：只有类型、地址、渠道 id 与名字；凭证提示与信封信息
  都不在这一组里。

### 我的模型

`GET /api/v1/merchant/models?limit=`

```json
{
  "items": [
    {"model": "gpt-4o", "display_name": "GPT-4o", "channels": 2, "priced": true}
  ],
  "total": 1,
  "truncated": false
}
```

- 只列**在供**的模型：判据与概览的 `models_in_supply` 是同一段（渠道已启用、
  未软删、`models` 含该规范名、目录里有这一行）。停用、软删、不含该名的渠道
  都不计入。
- `channels`：名下在供这个模型的渠道数。同一个 `(type, base_url)` 上的多个
  账号各算一条——它们是不同账号，不是重复行。
- `display_name`：目录里的展示名；为空时给规范名本身。
- `priced`：这个商家**生效中**（已启用、已到生效时刻、未过期）的 merchant
  作用域规则里，有没有一条命中这个模型——模型名单为空（兜底，适用于全部
  模型）或含这个规范名。没有价是 `false`，不是 `null`。
- `priced` 回答的是「我给这个模型配过价没有」，**不是**「这个模型实际按哪条
  计价」：多条命中时计价引擎按裁决次序只取一条（作用域 `channel` >
  `merchant` > `platform` → `priority` 大的优先 → 模型名单非空的优先于兜底
  → id 大的），渠道作用域的规则还可能让商家的规则完全不生效。平台规则与
  渠道规则不计入这一列。
- 管理员看全局时是同一套判据、不同的主语：问的是「全平台有没有一条商家
  规则命中它」，**不要求规则的主人正在供这个模型**——一条挂着某个模型的
  商家规则会让管理员在这一行看到 `true`，哪怕那位商家没有任何在供它的渠道。
- 按规范名升序；`limit` 默认 50、上限 200，越界回 `400`；被截断时
  `truncated: true`、`total` 给截断前的条数。

## 改它的时候

| 要改什么 | 改哪 |
|---|---|
| 路径与协议的对应、路径外的 `501` | `internal/api` 的 `ServeHTTP`（`PathChatCompletions`、`PathMessages`、`PathResponses`、`PathSystemOne`、`PathModels`） |
| 模型清单的内容（列出哪些模型、`owned_by`、单条不可用时的 `404`） | `internal/api/models.go`；供给判据在 `internal/channels` 的 `SuppliedModels` |
| 解析之前的检查顺序（方法、`Content-Type`、鉴权、限流、额度） | `internal/api` 的 `admit` |
| 状态码与错误码的映射 | `internal/api/error.go` 的 `errorKind` 与 `internal/api/api.go` 的 `httpStatusOfUpstream` |
| 错误体的形状 | `internal/api/error.go`（每套协议一份独立结构体） |
| 请求体上限 | `internal/api` 的 `maxRequestBodyBytes` |
| 控制台商家概览与上游的读数口径（异常理由、分组、窗口） | `internal/adminapi/merchant_ops.go`、`internal/channels/health.go` |
| 控制台商家「我的模型」的在供判据、渠道数与 `priced` 口径 | `internal/adminapi/merchant_models.go`、`internal/channels/channels.go` 的 `SuppliedModelsOwned` |
| 渠道表单的适配器枚举与上游模型探测 | `internal/adminapi/probe.go`、`internal/channels/modelprobe.go`、`internal/upstream/probe.go` |
| 流式的帧序列 | `internal/api/stream.go`（每套协议一个渲染器） |
| 流中途的错误帧 | `internal/api/stream.go` 的 `streamErrorFrame` 与 `responsesStream.errorFrame` |
| 响应头 | `internal/api` 的 `HeaderVersion` / `HeaderRetryAfter`、`internal/observe` 的 `HeaderRequestID` |
