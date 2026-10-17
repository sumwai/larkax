# 额度与限流

larkax 对一把 API Key 有**两种**限制，都按 Key 计，**互不相干**：

| | 按 Key 的 RPM | 按 Key 的累计额度 |
|---|---|---|
| 列 | `api_keys.rate_limit_rpm` | `api_keys.quota_limit` |
| 算的是什么 | 每分钟的**请求次数** | 这把 key 累计**花掉的钱** |
| 什么时候恢复 | 下一个分钟窗口 | **不会自己恢复** |
| 计数放在哪 | 进程内存 | 账本（`billing_requests`） |
| 超限返回 | `429` | `429` |

两者都不设时（两列都是 `NULL`）这把 key 不受任何限制。

另有一条**按用户钱包**的限制（余额够不够这次预留），它不按 key 设、
与上面两条互不相干，但拒绝时用的是**同一套码**——见下面的
「[钱包余额不足](#钱包余额不足)」。

## 超限时的响应

两条都是 `429`，但**处置相反**，所以区分它们的是**机器可读的码**，
不是状态码。码放在哪个字段按调用协议给：

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

| | `Retry-After` | 该做什么 |
|---|---|---|
| RPM | 有，单位秒，指向当前窗口结束 | **等这么久再重发**，同一个请求重发就会成功 |
| 累计额度 | **没有** | **不要重发**。等多久都不会变——要改的是这把 key 的额度，或者换一把 key |

额度那条不回这个头是刻意的：回一个等待时间等于告诉客户端「照做就行」，
而它照做之后必然再撞一次。不给这个头，客户端就只能去看码，而码会告诉它
该改配置而不是该退避。

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
分支的程序不受影响，读文案的人才能得到正确的下一步：

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
  预留金额是这次请求的**上界**（它按 `max_tokens` 算出来，见
  `internal/pipeline` 的 `Reserve`），所以被计入的通常略高于实际会扣的。
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
larkaxctl apikey create --user alice --rate-limit-rpm 60 --quota-limit 100

# 改一把已有 key 的额度（按 id 定位，见 larkaxctl apikey list）
larkaxctl apikey update --id 42 --quota-limit 200
larkaxctl apikey update --id 42 --rate-limit-rpm 120

# 取消其中一项（写回 NULL）
larkaxctl apikey update --id 42 --clear-quota-limit
larkaxctl apikey update --id 42 --clear-rate-limit-rpm
```

`--quota-limit` 最多 8 位小数。负数一律拒绝：写下 `-60` 的人几乎总是想写
`60`，让他当场改掉比让它变成一个静默的封禁好。

改完之后**下一个请求就生效**，不需要重启任何东西。

## 两条限制各自还带一个限制

- **RPM 的计数在进程内。** 多实例部署时每个实例各计一份，一把 `60` RPM 的
  key 在 N 个实例上最多能打到 `60×N` RPM。启动时会有一条 `warn` 把这件事
  说出来。共享计数要有一个所有实例都连得上的存储，目前没有。
- **累计额度要汇总这把 key 的全部历史账单。** 索引在
  `billing_requests (api_key_id)` 上；量级很大时它是这条判定最贵的部分。

## 改它的时候

| 要改什么 | 改哪 |
|---|---|
| 两种限制各自的语义（`NULL` / `0` / 正数） | `internal/ratelimit`（RPM）与 `internal/keyquota`（额度），各只有一处 |
| 「累计花费」的口径（算哪段时间、哪些状态、预留算不算） | `internal/keyquota` 的 `spentSQL` |
| 超限响应的状态码与码 | `internal/api/error.go`（`writeRateLimited` / `writeInsufficientQuota` / `writeInsufficientBalance`） |
| 判定发生在请求的哪一步 | `internal/api` 的 `serve`（入口早拒绝）与 `internal/ledger/store` 的 `SaveReservation`（权威判定） |

两种限制都是**两处判定、一份口径**：入口那一层在读请求体之前做一次早拒绝
（省掉一次预留事务与最多 8 MiB 的请求体读入），真正的判定在预留那个事务里
（与插账单行同一个事务、靠行锁串行化，所以并发打满也穿不过去）。两处引用
的是同一个包里的同一份判据——只改一处会让两边给出不同的答案。
