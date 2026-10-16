# 指标

服务在**内部监听**上暴露一个 `/metrics` 端点，格式是 Prometheus 的文本格式
（`text/plain; version=0.0.4`）。任何现成的采集器都能直接抓走，不需要额外的
导出器或 sidecar。

```sh
curl -s localhost:9090/metrics
```

端口取 `LARKAX_INTERNAL_ADDR`（默认 `:9090`），与 `/healthz`、`/readyz` 是
同一个监听。**公共端口上没有这个端点**：那里跑推理流量，路径全捕获并返回
`501`。

指标回答的是「有没有在发生」，日志回答的是「是哪一次、为什么」。两者不互相
替代：撞上预留上界这件事由计数器的取值报警，具体是哪一次请求则要去日志里按
`request_id` 查。

## 指标清单

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `larkax_billing_settlements_total` | counter | `usage_source` | 按实际用量结算完成的次数。`usage_source` 取 `measured` / `tokenized` / `estimated`，即这笔账用的是实测值、本地精确计数还是启发式估算 |
| `larkax_billing_capped_settlements_total` | counter | 无 | 结算金额被预留上界截住的次数 |
| `larkax_billing_unbilled_settlements_total` | counter | 无 | 未计费的结算次数：上游失败，或请求在结算之前中止 |

## 怎么读

**`capped` 大于 0。** 预留是上界，所以这个数按设计应当一直是 0。它涨起来说明
上界算错了（例如上游报的输出 token 超过了我们给的 `max_tokens`），差额由平台
承担。要注意的是此时**账目仍然是平的**——对账看不出来，只有这个数能看见。
是哪一次请求，靠日志里的 `request_id` 找。

**`capped` 一直是 0。** 先看 `settlements_total` 有没有在涨。它也不涨，说明
计数器根本没接上，而不是「一次都没撞上」——一个一直是 0 的计数器与一个没有
接的计数器，光看这一行是分不出来的，所以这一条必须连着分母一起读。

**`unbilled` 上涨。** 上游失败或请求中途中止。它与 `settlements_total` 的比值
是上游健康度的第一手数据。

**`settlements_total{usage_source="estimated"}` 上涨。** 这笔账是按估算的用量
收的，误差范围见 `internal/tokens` 的 `ErrorBounds`。它不该成为常态。

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
