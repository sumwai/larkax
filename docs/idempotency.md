# 请求级幂等

调用方在请求头上带一个 `Idempotency-Key`，同一个键重复发送**只执行一次**：
第一次的结果被留档，后续同键请求原样拿回那份结果，不再打一次上游，也不再扣一次钱。

## 怎么用

```http
POST /v1/chat/completions HTTP/1.1
Authorization: Bearer <API_KEY>
Content-Type: application/json
Idempotency-Key: 6f1c2b0a-9d3e-4f57-8a21-0c5b7e9d4a12
```

（凭证两个头都认，按协议的习惯来：OpenAI 那套用 `Authorization: Bearer`，
Anthropic 那套用 `x-api-key`。）

键由调用方自己生成，要求是**可见 ASCII 字符**（`0x21`–`0x7e`）、不超过 256 字节。
UUID、`<订单号>-<第几次尝试>` 之类的都可以；带空格、换行或非 ASCII 的会被拒（`400`）。

**作用域是 API Key，不是用户。** 同一个用户的两把 key 用同一个键互不影响，
不同用户之间也互不影响——一次跨租户的撞键不会让 A 拿到 B 的响应。

**不带这个头时行为完全不变**：不查表、不写记录，每个请求照常执行一次。

## 什么时候该用它

- 请求发出去之后连接断了、超时了，你不确定它有没有被执行。
- 客户端自己会重试（网络抖动、SDK 内置重试）。
- 计费敏感的场景，重复执行一次上游调用就是多扣一次钱。

第一次请求**失败**时重试同一个键不会重新执行——见下文「失败的请求也会被记下来」。
所以真正要防的是「不确定有没有执行」，而不是「执行失败了想再试一次」；
后者应该换一个新的键。

## 三种结果

| 情况 | 返回 |
|---|---|
| 第一次还没跑完 | 等它跑完（上限是这次请求自己的超时），然后按下面的规则返回 |
| 跑完了 | 原样重放第一次的响应：状态码、响应头、响应体逐字节相同 |
| 没跑完（上游失败、中途断开） | `409`，`idempotency_incomplete` |
| 同一个键配了不同的请求体 | `409`，`idempotency_key_reused` |
| 等到自己的超时，第一次仍在跑 | `409`，`idempotency_in_progress` |

**流式响应也重放**：存的是那串 SSE 字节，重放就是原样再发一遍，
连 `Cache-Control: no-cache` 与 `X-Accel-Buffering: no` 一起——
少了这两个，一次重放出去的 SSE 会被中间缓存缓存住、被反向代理缓冲住，
而第一次请求特意防住了那两件事。
一次没有正常结束的流（没收到结束事件）**不算跑完**，不会被重放——
重放一段半截的回答比不重放更坏。

## 三条 409 的码不一样，因为它们要做的事不一样

状态码都是 `409`，**契约是 `code` / `type` 字段**：

| 码 | 该做什么 |
|---|---|
| `idempotency_key_reused` | 这是另一个请求了，换一个新键重发 |
| `idempotency_in_progress` | 稍后用**同一个**键重试 |
| `idempotency_incomplete` | 第一次没成功，换一个新键重发 |

两套协议的错误体形状不同，码放的位置也不同：

```json
// POST /v1/chat/completions（OpenAI 形状）
{"error": {"message": "...", "type": "invalid_request_error",
           "code": "idempotency_key_reused", "param": null}}
```

```json
// POST /v1/messages（Anthropic 形状）
{"type": "error", "error": {"type": "idempotency_key_reused", "message": "..."}}
```

Anthropic 那套的错误体里只有一个机器可读的槽位，所以码放在 `error.type` 上。

## 请求体的什么内容进指纹

同键是否「同一个请求」由请求体的**指纹**判断，而不是整串字节的哈希。
进指纹的字段是这些：

| 字段 | 为什么它进 |
|---|---|
| `protocol` | 同一个键在 `/v1/chat/completions` 与 `/v1/messages` 上是两个请求 |
| `model` | 换模型就是换请求 |
| `messages` | 对话内容 |
| `max_tokens` | 它决定预留金额，也决定回答的长度 |
| `temperature`、`top_p` | 采样参数 |
| `reasoning_effort` | 推理深度，影响计费与输出 |
| `tools` | 工具定义，影响上游行为 |
| `stream` | 流式与非流式拿到的是两种响应，不能互相重放 |
| `extra` | **当前恒为空**，见下面的说明 |

**不进指纹的**：请求头（`Authorization`、`Content-Type` 等是传输层的，
不是请求内容）；模型别名解析、选路结果、计价规则这些由我们这边决定的中间量
（换个渠道不该让同一个请求变成另一个请求）。

`extra` 那一行要单独说一句：**它现在永远是空的**。入口唯一的解析出口
（`internal/relay` 的 `FromRelaykitRequest`）不写这个字段，所以走这两条入口
的请求在这里恒为 `null`。它留在表里是因为适配器那一层会把它透传给上游
（见 `internal/relay` 的 `ToRelaykit`）——哪天那条路接上了，改这个 map 就是
改一次请求，该算成另一次。在那之前，别把它当成「有字段会被透传」。

## 留存与清理

记录留存 **24 小时**，由 `settings` 表的 `idempotency.retention_hours` 控制。
到期靠运维命令删，不在请求路径上删：

```sh
larkaxctl idempotency cleanup             # 按留存期删除到期记录，打印删了多少条
larkaxctl idempotency cleanup --dry-run   # 只数一遍，不删
```

留存期只在 `settings` 里配，命令行上不另设参数——同一件事有两个来源时，
命令行的那个会悄悄赢，而「留存期到底是几小时」会变成一件要看是谁执行的才知道的事。

**留存期写成 0 或负数时，清理命令会报错退出、一条都不删**，而不是「不留存」。
那是配置写错的样子，而一次笔误清空整张表比停下来报错贵得多。
表因此不会缩小，命令会一直失败——这是有意的：它响亮、可发现，
而「静默地什么都不删」会让人以为留存承诺兑现了。

## 改它的时候

| 想改什么 | 改哪 |
|---|---|
| 表的列、索引、约束 | `internal/migrate/sql/00011_idempotency_keys.sql` |
| 进指纹的字段 | `internal/idempotency/idempotency.go` 的 `hashInput`——**加字段要想清楚**，见那里逐条的说明 |
| 键的合法性 | `internal/idempotency` 的 `ValidateKey` |
| 读写记录 | `internal/idempotency/store.go` |
| 等待的轮询间隔与写回超时 | `internal/api/idempotency.go` 顶部的两个常量 |
| 三条 409 的码与措辞 | `internal/api/error.go` |
| 清理命令 | `cmd/larkaxctl/idempotency.go` |

三处约定容易踩：

- **幂等记录与预留写在同一个事务里**（`internal/ledger/store` 的 `SaveReservation`）。
  分成两个事务写，就必然有一个窗口落在「记了幂等但没扣钱」或「扣了钱但没记幂等」上，
  而这两种状态没有任何东西会报出来。
- **留档的是真正写出去的字节**，不是「再渲染一遍」。第二处渲染逻辑迟早与第一处不一致，
  而那种不一致表现为「重试拿到的回答与第一次不一样」，同样没有东西会报。
- **响应体整份留档，响应头不是**：只留了 `Content-Type`，流式那几个头由
  `internal/api/stream.go` 的 `streamHeaders` 补上（第一次请求与重放共用同一份
  定义，所以往那里加头两边都会生效）。**在别处给响应加头的话**，重放那条路不会
  带上它——加头时要想清楚这一点。

改了 `internal/api/error.go` 里那几条码，要同步 `internal/api/idempotency_test.go`
与 `internal/assembly/idempotency_e2e_test.go` 里按协议断言码位置的用例。
