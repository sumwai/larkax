# 额度与限流

larkax 对一把 API Key 有**两种**限制，都按 Key 计，**互不相干**：

| | 按 Key 的 RPM | 按 Key 的累计额度 |
|---|---|---|
| 列 | `api_keys.rate_limit_rpm` | `api_keys.quota_limit` |
| 算的是什么 | 每分钟的**请求次数** | 这把 key 累计**花掉的钱** |
| 什么时候恢复 | 下一个分钟窗口 | **不会自己恢复** |
| 计数放在哪 | Redis（没配或连不上时退回进程内存，见下） | 账本（`billing_requests`） |
| 超限返回 | `429` | `429` |

两者都不设时（两列都是 `NULL`）这把 key 不受任何限制。

另有一条**按用户钱包**的限制（余额够不够这次预留），它不按 key 设、
与上面两条互不相干，但拒绝时用的是**同一套码**——见下面的
「[钱包余额不足](#钱包余额不足)」。

## 超限时的响应

两条都是 `429`，但**处置相反**，所以区分它们的是**机器可读的码**，
不是状态码。错误体的完整形状（每套协议长什么样、`type` 与 `code` 由什么推出来）
见 [api.md](api.md) 的「错误体的形状」一节；这里只说码放在哪个字段：

| 调用协议 | 超限原因 | 码放在哪 | 取值 |
|---|---|---|---|
| OpenAI（`/v1/chat/completions`） | RPM | `error.code` | `null` |
| OpenAI | 累计额度 | `error.code` | `insufficient_quota` |
| OpenAI Responses（`/v1/responses`） | RPM | `error.code` | `null` |
| OpenAI Responses | 累计额度 | `error.code` | `insufficient_quota` |
| Anthropic（`/v1/messages`） | RPM | `error.type` | `rate_limit_error` |
| Anthropic | 累计额度 | `error.type` | `insufficient_quota` |

Responses 那一套的错误体与 OpenAI 那一套**逐字相同**（两家共用同一个官方
SDK 的错误解析器，见 `internal/api/error.go`），所以两处的码放在同一个位置。
差别只在**流式**那条路上：Responses 流中途失败是一个平铺的事件
（`type` / `code` / `message` 在顶层，没有 `error` 外层），见
`internal/api/stream.go` 的 `responsesStream.errorFrame`。

**OpenAI 那套要按 `error.code` 分支，不能按 `error.type`**：限流与额度
两条的 `error.type` 都是 `rate_limit_error`，只有 `error.code` 分得开
（`null` 对 `insufficient_quota`）。

真实的响应体（限流，带 `Retry-After`）：

```json
{"error":{"message":"rate limit exceeded for this API key, retry later",
          "type":"rate_limit_error","param":null,"code":null}}
```

```json
{"type":"error","error":{"type":"rate_limit_error",
                         "message":"rate limit exceeded for this API key, retry later"}}
```

真实的响应体（累计额度，不带 `Retry-After`）：

```json
{"error":{"message":"this API key has reached its spending limit; raise the limit on this key or use a different key",
          "type":"rate_limit_error","param":null,"code":"insufficient_quota"}}
```

```json
{"type":"error","error":{"type":"insufficient_quota",
                         "message":"this API key has reached its spending limit; raise the limit on this key or use a different key"}}
```

### `Retry-After` 只有限流那条回

`insufficient_quota` 的两个来源都要分行写：它们同样**没有**这个头，但「该做什么」
相反——一个改 key 的额度，一个充值。混成一行会把余额不足那句也说成「改 key 的额度」。

| 拒的原因 | `Retry-After` | 该做什么 |
|---|---|---|
| RPM | 有，单位秒，指向当前窗口结束 | **等这么久再重发**，同一个请求重发就会成功 |
| 这把 Key 的累计额度用尽 | **没有** | **不要重发**。等多久都不会变——要改的是这把 key 的额度，或者换一把 key |
| 钱包可用余额不足 | **没有** | **不要重发**。要改的是钱——充值，之后下一个请求就生效（见下） |
| 上游限流（不是我们拒的） | **没有** | **可以重发**，通常换一条渠道就过去了。文案是 `upstream is rate limited, retry later` |

第四行与上面两行**处置相反**，而它在响应里长得几乎一样：同样是 `429`、同样
`error.type` 是 `rate_limit_error`、同样 `error.code` 是 `null`，区别只在文案。
所以「见到 429 就退避」与「见到 429 就放弃」两种写法都会在它上面错——要么白等，
要么放掉一个重发就能成的请求。

额度与余额那两条不回这个头是刻意的：回一个等待时间等于告诉客户端「照做就行」，
而它照做之后必然再撞一次。不给这个头，客户端就只能去看响应体——而这两个来源
同码不同源，分得开它们的是 `message`，不是码，所以这一步必须读文案（见下）。

## 钱包余额不足

按 Key 的额度之外还有一条限制，**按用户钱包**算：每次请求在预留那一步会
检查钱包的「余额 − 冻结」够不够这笔上界，不够就拒绝。两条限制互不相干
——一把没设额度上限的 key 照样会因为用户余额不足被拒。

它回的是**同一套码**：

| 调用协议 | 码放在哪 | 取值 |
|---|---|---|
| OpenAI / OpenAI Responses | `error.code` | `insufficient_quota` |
| Anthropic | `error.type` | `insufficient_quota` |

理由是按 Key 的额度用尽与这里的余额不足都是**账户状态**而不是服务故障，
调用方只该写一个分支。状态码同为 `429`，同样**不回** `Retry-After`
（充值不是等一会儿的事），同样不该重发。

两者的**文案**不同，因为该做的事不同——按 `error.code` / `error.type`
分支的程序不受影响，读文案的人才能得到正确的下一步。**码分不开这两个来源，
`message` 分得开**：`larkaxctl doctor --live` 的归因就按这句 message 认，
认不出来时退回「账户状态类拒绝」这种更保守的说法，不往任何一个方向猜。

真实的响应体（余额不足，不带 `Retry-After`）：

```json
{"error":{"message":"the account balance is insufficient for this request; top up the wallet before retrying",
          "type":"rate_limit_error","param":null,"code":"insufficient_quota"}}
```

```json
{"type":"error","error":{"type":"insufficient_quota",
                         "message":"the account balance is insufficient for this request; top up the wallet before retrying"}}
```

充值走 `larkaxctl wallet credit`（见 [development.md](development.md) 的
「入账」一节），之后**下一个请求就生效**。

**注意这一条与「结算时扣款让余额变成负」不是一回事。** 后者是账本内部的
不变量被破坏（预留本该挡住它），不是「现在没钱了」，它的处置仍是 `500`
且不带可分支的码——看到它要查账，不要去充值。

## 累计额度的口径

「累计花费」= 这把 key 名下**已结算**的那些请求的最终金额之和，
加上**还挂着、正在跑**的那些请求的预留金额之和。

四条都要知道，因为每一条都影响你什么时候会被拦：

- **全部历史累计，不按周期重置。** 没有「本月额度」这个概念，上个月花的
  钱这个月照样算在里面。
- **正在跑的请求算进去，而且按预留金额算。** 一个长请求已经冻住了一笔钱，
  那一笔立即计入。所以并发打满时不会每一笔都「还没结算」而全部放行。
  预留金额的口径**分流式与非流式两种**：非流式按这次请求声明的**上界**
  一次冻满（它按 `max_tokens` 算出来，见 `internal/pipeline` 的 `Reserve`），
  所以被计入的通常略高于实际会扣的；流式只先冻**第一段**，之后随真实产出
  递进补冻（见 `internal/pipeline` 的 `ReserveStream`），于是正在跑的那一笔
  更贴近实际会扣的，但它仍是一条上界，随时可能随流变大。
  `max_tokens` 本身还有一条单次上限：非流式请求超过它会先被那条判据拒掉
  （回 `400`，见 [max-tokens.md](max-tokens.md)），轮不到这一条；**流式与
  `/v1/systemone`（决策表面）不受这条判据约束**——流式有中途观测点，钱的处置
  是随流递进冻结（见 [metrics.md](metrics.md) 的
  `stream_balance_exhausted_total`），决策表面的上界取自我们自己的两个常量。
- **只算已结算与正在跑的。** 上游失败、被释放、被退款的那些请求没有从用户
  身上扣走钱，不计入。
- **卡住的请求会一直占着额度。** 一笔预留如果因为进程被杀而没有结算也没有
  释放，它会停在「正在跑」那个状态，于是那笔钱**永久**占着这把 key 的额度
  ——它一分都没花，但额度不会自己回来。这类行会被对账报成 `stale_reservation`
  找出来（见 [reconciliation.md](reconciliation.md)），**释放它需要人工确认**：
  自动释放与真正在途的请求有竞态，所以对账只报不修。

金额的单位是**该用户钱包的资产品种**（见 `user_wallets.asset`）。

### `NULL` 与 `0` 不是一回事

两列都一样，这一条必须分清楚：

| 列里的值 | 意思 |
|---|---|
| `NULL` | **没设上限**，不限 |
| `0` | **一个都不放行**（一分钱都不让花 / 一分钟一次都不放行） |
| `> 0` | 就是那个上限 |

把 `0` 读成「不限」是这类配置最典型的错法，所以两边都不做这个合并：
写 `0` 的意图是「先把这把 key 停掉」，它会真的停掉。

## 怎么设置

`rate_limit_rpm` 与 `quota_limit` 都按**明文**给，不填表示写 `NULL`：

```sh
# 建一把 key，同时设两种额度
larkaxctl apikey create --user alice --rpm 60 --quota 100

# 改一把已有 key 的额度（按 id 定位，见 larkaxctl apikey list）
larkaxctl apikey update --id 42 --quota 200
larkaxctl apikey update --id 42 --rpm 120

# 取消其中一项（写回 NULL）
larkaxctl apikey update --id 42 --clear-quota
larkaxctl apikey update --id 42 --clear-rpm
```

`--quota` 最多 8 位小数。负数一律拒绝：写下 `-60` 的人几乎总是想写
`60`，让他当场改掉比让它变成一个静默的封禁好。

改名之前的写法（`--rate-limit-rpm`、`--quota-limit`、`--clear-rate-limit-rpm`、
`--clear-quota-limit`）仍然可用，完整清单在 `larkaxctl apikey --help` 末尾的
「别名」一段。

改完之后**下一个请求就生效**，不需要重启任何东西。

## 两条限制各自还带一个限制

- **RPM 的计数在 Redis 里**（`LARKAX_REDIS_DSN`），所有实例共用一份，所以
  「60 RPM」说的是整个部署 60 次，不是每个实例 60 次。键是
  `rl:<api_keys.id>:<窗口起点的 Unix 秒>`，每个键的过期时间是两倍窗口（一分钟
  的窗口留两分钟），而且**每次判定都把它续回两倍窗口**——于是键的存活时间跟着
  最后一次使用走，它们会自己消失、不会攒起来。

  三件事要知道：

  - **没配 `LARKAX_REDIS_DSN`** 时计数退回进程内，那时它只在**单实例**下正确，
    N 个实例的总放行量最高到 `N ×` 配置值。启动日志里有一条 `warn` 说这一种。
  - **Redis 连不上时**同样退回进程内计数，判定变成近似的（放行量偏大），
    **但请求不会失败**——共享存储出问题是平台的问题，不该让用户吃 4xx。
    恢复是自动的，下一次判定成功起就又是精确的。
  - **是精确还是近似**看两个数：`larkax_ratelimit_exact`（1 精确 / 0 近似）与
    `larkax_ratelimit_store_errors_total`（失败了多少次）。见 [metrics.md](metrics.md)。

  要查某个 Key 当前这一分钟用了多少，走内部监听上的只读入口：

  ```sh
  curl -s "127.0.0.1:9090/ratelimit/usage?key_id=42"
  # {"key_id":42,"window_start":"2026-10-17T12:00:00Z",
  #  "window_end":"2026-10-17T12:01:00Z","count":17,"exact":true}
  ```

  它**只读**：不计数、不续过期时间、不改任何状态，查多少次都不影响判定。
  用法是拿这里的 `count` 与库里的 `rate_limit_rpm`（`larkaxctl apikey list`
  查得到）对照——这条路径不连数据库，所以额度不进它的入参，也就不会出现
  「同一把 key 两个额度」这种事。

  改限额、清空计数、临时封禁**都不做**：那三件各自是能绕开或加重限流的权力
  （清空计数等于临时解除限流），要单开一条并带审计。

  **本网关不返回 `x-ratelimit-*` 系列响应头**（`X-RateLimit-Limit-Requests` /
  `-Remaining-Requests` / `-Reset-Requests` 那一套）。限流信息只在**被拒**时给：
  `429` 加上面那个 `Retry-After`。按 OpenAI 的惯例写完自限速、然后发现始终读不到
  这几个头的人，这一句就是答案——它们不会被返回，不是配错了。

- **累计额度要汇总这把 key 的全部历史账单。** 索引在
  `billing_requests (api_key_id)` 上；量级很大时它是这条判定最贵的部分。

## 请求体上限（不按 Key 算）

上面几条按 Key 计，这一条只看请求本身：**请求体最大 8 MiB**，不是配置项
（`internal/api` 的 `maxRequestBodyBytes`）。超出回：

| 调用协议 | 状态码 | 码放在哪 | 取值 |
|---|---|---|---|
| OpenAI / OpenAI Responses | `413` | `error.code` | `request_too_large` |
| Anthropic | `413` | `error.type` | `request_too_large` |

文案是 `request body exceeds 8388608 bytes`。

这条限制在**解析请求体之前**生效。这是有意的：超限的请求不该让我们为它读进
8 MiB 内存，更不该走到预留那一步——那会先把余额冻住，再为一个早就该拒的请求
去解冻。**多模态请求（正文里带 base64 图片）是最容易撞上它的一类。**

## 上游响应头超时（不按 Key 算）

还有一条时限，同样只看请求本身：**上游必须在 10 分钟内回响应头**
（`cmd/larkax` 的 `upstreamTimeout`，是常量、不是配置项）。超了，这一次尝试就
算失败，调用方拿到 `502` 与 `upstream call failed`。它会被当成可重试的失败——
路由会换下一个渠道再试一次，所以候选有几个，最坏情况就要等几个 10 分钟。

它只管「多久拿不到响应头」，**不给整次调用设时限**：流式响应会长期占用连接，
整体超时会在长回复中途把连接掐断。所以**一个已经开始吐字的流没有时长上限**——
它一直开着，直到上游结束、或调用方自己断开。

理由与请求体上限那条一样：上游卡住时不能让请求永远不返回，而调用方要能算出
自己的客户端超时该设多少。

## 改它的时候

| 要改什么 | 改哪 |
|---|---|
| 两种限制各自的语义（`NULL` / `0` / 正数） | `internal/ratelimit`（RPM）与 `internal/keyquota`（额度），各只有一份判据（落点有两处，见下） |
| 窗口长度、键名、过期时间、热路径的存储超时 | `internal/ratelimit`（`windowLen` / `windowKey` / `keyTTL` / `storeTimeout`） |
| 共享存储挂掉时的行为（降级还是拒请求） | `internal/ratelimit` 的 `Allow`（判定）与 `internal/assembly` 的 `buildRateLimiter`（启动那一次探测） |
| 「累计花费」的口径（算哪段时间、哪些状态、预留算不算） | `internal/keyquota` 的 `spentSQL` |
| 超限响应的状态码与码 | `internal/api/error.go`（`writeRateLimited` / `writeInsufficientQuota` / `writeInsufficientBalance`） |
| 只读查询那条路径的形状 | `internal/api/ratelimitusage.go` |
| 判定发生在请求的哪一步 | `internal/api` 的 `serve`（入口早拒绝）与 `internal/ledger/store` 的 `SaveReservation`（权威判定） |
| 上游响应头超时的时长 | `cmd/larkax` 的 `upstreamTimeout` |

两种限制都是**两处判定、一份口径**：入口那一层在读请求体之前做一次早拒绝
（省掉一次预留事务与最多 8 MiB 的请求体读入），真正的判定在预留那个事务里
（与插账单行同一个事务、靠行锁串行化，所以并发打满也穿不过去）。两处引用
的是同一个包里的同一份判据——只改一处会让两边给出不同的答案。
