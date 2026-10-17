# 指标

服务在**内部监听**上暴露一个 `/metrics` 端点，格式是 Prometheus 的文本格式
（`text/plain; version=0.0.4`）。任何现成的采集器都能直接抓走，不需要额外的
导出器或 sidecar。

```sh
curl -s 127.0.0.1:9090/metrics
```

端口取 `LARKAX_INTERNAL_ADDR`（默认 `127.0.0.1:9090`，只绑回环），与
`/healthz`、`/readyz` 是同一个监听。**公共端口上没有这个端点**：那里跑推理流量，
路径全捕获并返回 `501`。

指标回答的是「有没有在发生」，日志回答的是「是哪一次、为什么」。两者不互相
替代：撞上预留上界这件事由计数器的取值报警，具体是哪一次请求则要去日志里按
`request_id` 查。

## 指标清单

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `larkax_billing_settlements_total` | counter | `usage_source` | 按实际用量结算完成的次数。`usage_source` 取 `measured` / `tokenized` / `estimated`，即这笔账用的是实测值、本地精确计数还是启发式估算 |
| `larkax_billing_capped_settlements_total` | counter | `protocol` | 结算金额被预留上界截住的次数，按入口协议分。`protocol` 取 `channel.Protocol` 的取值：`openai` / `anthropic` / `openai-responses` / `systemone` |
| `larkax_billing_unbilled_settlements_total` | counter | 无 | 未计费的结算次数：上游失败，或请求在结算之前中止 |
| `larkax_auth_failures_total` | counter | `reason` | 认证失败被回以 401 的次数，按失败原因分。`reason` 取 `internal/auth` 的 `Reason`：`missing_credential` / `malformed_credential` / `key_not_found` / `key_disabled` / `key_revoked` / `key_inactive` / `key_expired` / `user_inactive`，外加 `unclassified` |

## 怎么读

**`capped` 大于 0。** 预留是上界，所以这些序列按设计应当一直是 0。哪一条
`protocol` 的序列涨起来，就说明那一条入口的上界算错了（chat 按 `max_tokens`
与正文估算，决策表面按 `internal/systemone` 的两个常量），差额由平台承担。
要注意的是此时**账目仍然是平的**——对账看不出来，只有这个数能看见。

`capped` 是**带标签**的计数器：第一次撞上界之前，它一条序列都不输出。所以
「没有这一行」与「一次都没撞上」在输出里长得一样，不能只看它——要连着
`settlements_total` 一起读（见下一条）。分协议读同时回答了「是哪一条协议
在漏」：只有一个 `protocol` 的序列在涨，就只看那一条的预留上界来源。

**是哪一次请求撞的、撞了多少**，这个计数答不了（它没有 `request_id` 标签）。
要落到具体请求上，查 `billing_requests` 的 `computed_amount`——它是**未截断**的
结算金额，于是

```sql
SELECT request_id, reserved_amount, computed_amount,
       computed_amount - reserved_amount AS 平台承担的差额
  FROM billing_requests
 WHERE computed_amount > reserved_amount
 ORDER BY settled_at DESC;
```

就是那些撞上界的请求，差额一列是平台吃掉的钱。`computed_amount` 与
`reserved_amount` 都记在同一行上，所以这件事不必再靠日志拼。

注意查出来的行数**只覆盖这一列上线之后结算的请求**：更早的行 `computed_amount`
是 NULL，且补不回来（撞上界时 `final_amount` 就等于预留，未截断的那个数没有
留下痕迹）。那些行是「没有记录」，不能并进「没撞上界」——见迁移 00021 的注释。

**`capped` 一直是 0（或者一条序列都没有）。** 先看 `settlements_total` 有没有
在涨。它也不涨，说明计数器根本没接上，而不是「一次都没撞上」——一个一直是 0
的计数器与一个没有接的计数器，光看这一行是分不出来的，所以这一条必须连着
分母一起读。`capped` 现在带标签，第一次撞上界之前不出现，这一点让「没接上」
与「没撞过」在它自己那一行上更难分辨，分母因此更重要。

**`unbilled` 上涨。** 上游失败或请求中途中止。它与 `settlements_total` 的比值
是上游健康度的第一手数据。

**`settlements_total{usage_source="estimated"}` 上涨。** 这笔账是按估算的用量
收的，误差范围见 `internal/tokens` 的 `ErrorBounds`。它不该成为常态。

**`auth_failures_total{reason="key_expired"}` 与 `{reason="key_not_found"}` 要分开看。**
用户报「我的 key 不能用了」时，这两种情况指向的排查动作完全相反：前者是这把 key
到期了（续期或换一把），后者是库里根本没有这个哈希（发错了 key，或者客户端的
凭证被改过）。而**它们对外的表现完全一致**——401 的响应体与文案对所有原因都是
同一句，因为区分开等于送出一个「这个 key 存在吗」的探测器。所以这两条的区分
只存在于这里，别处都看不到。

**`reason="unclassified"` 出现。** 按设计不该出现。它说明有一条 401 不是由
`*auth.Error` 产生的（例如某条路径直接 `return auth.ErrUnauthenticated`），
而那种 401 回答不了「为什么不能用了」。看到它就说明那条路径的接线坏了。

**这条路径上不写日志，只记指标。** 这与上面「指标回答有没有在发生、日志回答是
哪一次」的分工有一个刻意的例外：401 是攻击者可控的，拿垃圾 key 打就能刷日志、
刷盘，所以「是哪一次」在这里没法安全地记下来。`reason` 是一个闭合枚举，
基数有界，正好补上这个位置。

代价是「是哪一把 key」这个问题指标答不了——它没有 key 标签，也不该有（那是
无界集合），而这条路径上又没有日志可查。所以那个问题要靠报障的人给出 key 前缀，
再去 `api_keys.key_prefix` 里定位。

## 命名与标签约定

- 名字以 `larkax_` 开头，之后是子系统（`billing`、`upstream`、`ledger`……），
  计数器以 `_total` 结尾。
- **标签值必须来自有限集合。** 用户 id、渠道 id、请求 id、模型名一律不做标签：
  每个取值各占一条时间序列，而序列数没有上限，采集端的内存与查询开销会跟着
  请求数一起涨。要区分渠道就用渠道**类型**，要区分上游就用上游**种类**。
- 每个指标都要有 `# HELP`，写清楚它是什么，以及什么取值算异常。
- 时间用秒并以 `_seconds` 结尾；金额不带单位后缀时以元为单位。

## 加一个指标

三步，前两步都在 `internal/observe/metrics.go`：

1. 在 `NewRegistry` 里加一行，字段的创建与登记写在同一次调用里：

   ```go
   r.XxxTotal = r.newCounter("larkax_xxx_total", "说明")
   r.YyyTotal = r.newLabeled("larkax_yyy_total", "说明", "kind")
   ```

2. 在 `Registry` 上加对应的导出字段。**不要只声明字段、忘了登记**：那样它安静地
   不输出，与「根本没接上」长得一模一样（`TestEveryDeclaredCounterIsRegistered`
   会拦住这种写法）。
3. 在增量点调用 `observe.Metrics.XxxTotal.Inc()`（带标签的用
   `observe.Metrics.YyyTotal.With("值").Inc()`）。

增量点**只加一行**：不要为了计数去改被计数的逻辑，也不要在计数器里做判断。
标签值一律用常量或有限枚举，不要从请求里取。

不带标签的计数器总是输出，包括值为 0 的时候——「一直是 0」本身就是要看的那个值。
带标签的计数器在第一次自增之前没有任何序列，整族不输出。

最后更新上面那张清单表：它是这份文档里唯一会被采集端依赖的部分。
