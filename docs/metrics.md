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
| `larkax_billing_capped_amount_total` | counter | `protocol`, `asset` | 结算被预留上界截住的那部分**金额**，按入口协议与资产品种分。与上一条是一对：那个数次数，这个数少收了多少钱。`protocol` 同上一行；`asset` 取 `ledger.Asset` 的取值：`CNY`。**单位是最小计价单位（`money.Scale` 位定点，1e-8），不是元**——理由见下面的命名约定 |
| `larkax_billing_unbilled_settlements_total` | counter | 无 | 未计费的结算次数：上游失败，或请求在结算之前中止 |
| `larkax_billing_stream_underfrozen_total` | counter | 无 | 流式请求**补冻没冻上而放行**的次数：写库失败，或算不出价（取规则 / 编译 / 求值失败）。之后产出的 token 没有对应冻结额，结算到它们时会撞上界，差额由平台承担。按设计应当一直是 0 |
| `larkax_billing_stream_balance_exhausted_total` | counter | 无 | 因可用余额不足被**我们切断**的流式请求次数。调用方收到一条流内错误帧，它的文案说的是充值而不是重试（见 [limits.md](limits.md)）。替代了原先「按声明上界冻满而遭 429 拒掉」的那个形状 |
| `larkax_auth_failures_total` | counter | `reason` | 认证失败被回以 401 的次数，按失败原因分。`reason` 取 `internal/auth` 的 `Reason`：`missing_credential` / `malformed_credential` / `key_not_found` / `key_disabled` / `key_revoked` / `key_inactive` / `key_expired` / `user_inactive`，外加 `unclassified` |
| `larkax_ratelimit_exact` | gauge | 无 | RPM 限流的计数**当前**是否取自共享存储：`1` 精确（计数在 Redis 里，各实例共用一份，放行总量不超过配置值），`0` 近似（退回进程内计数，各实例各算各的，放行量偏大）。**没设置过就不输出**，所以「没有这一行」表示这个实例上没接限流器（worker 角色） |
| `larkax_ratelimit_store_errors_total` | counter | 无 | 共享计数存储（Redis）调用失败的次数，**含启动期那一次探活**。有请求之后，每一次因为存储不可用而退回进程内计数的判定再各记一次——它是上面那个仪表变成 `0` 的原因，也是「配了但连不上」与「没配」分得开的那一半 |

## 怎么读

**`capped` 大于 0。** 预留是上界，所以这些序列按设计应当一直是 0。哪一条
`protocol` 的序列涨起来，就说明那一条入口的上界算错了（chat 按 `max_tokens`
与正文估算，决策表面按 `internal/systemone` 的两个常量），差额由平台承担。
要注意的是此时**账目仍然是平的**——对账看不出来，只有这个数能看见。

**`capped_amount_total` 大于 0。** 它和 `capped_settlements_total` 是同一件事
的两个口径：条数回答「撞了几次」，金额回答「**少收了多少钱**」。后者才是
平台真正承担掉的那笔钱，而把同一 `protocol` 上的金额除以条数，就是「一次
撞界平均少收多少」——只擦边与上界错得离谱，在这个比值上分得开。

它的取值是**最小单位**（`money.Scale` 位定点），换算成元要除以 `1e8`：
`0.098 元` 记成 `9800000`。同一个时间窗内，它应当等于库里
`sum(computed_amount - reserved_amount)`（同一个差额，见下面的 SQL），
两边对不上说明有一处漏记。`asset` 这一维现在只有一个取值，留着是因为
少收的可能是另一种资产——那时两条序列要分开看，混在一起则两笔钱都算不清。

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

**`stream_underfrozen_total` 大于 0。** 流式请求的补冻没冻上就放行了——库在拒绝
写入，或定价侧算不出价（取规则 / 编译 / 求值失败）。按设计应当一直是 0：它一涨，
说明**这一次请求的账已经对不上预留**，之后产出的 token 没有冻结额，结算撞上界、
差额由平台承担，而对账看不出来（撞上界时两边记的是同一个被截小的数，账目仍然是
平的）。所以它是那件事唯一的即时信号。要分「库在报错」还是「算不出价」，去日志里
按 `request_id` 查那条告警——它把失败的 `error` 原样记下来。

**`stream_balance_exhausted_total` 上涨。** 我们因为用户余额不够继续冻而切断了
一条流（本轮不引入透支），它与「上游失败」是两回事，处置也不同：那一条流**不会**
被记成渠道的失败（不污染渠道健康度），调用方收到的错误帧文案指向充值。它涨得多
与 `capped` 指的不是同一件事——那里是我们的上界算错，这里多半是用户真的没钱了。

**`settlements_total{usage_source="estimated"}` 上涨。** 这笔账是按估算的用量
收的，误差范围见 `internal/tokens` 的 `ErrorBounds`。它不该成为常态。

**`ratelimit_exact` 是 0。** RPM 限流的计数退回了进程内，那一刻判定是近似的
（各实例各算各的，放行量偏大）。**它单独看分不出两种情形，要连着
`ratelimit_store_errors_total` 一起读**：

- 仪表是 0、那个计数器**是 0** → 这个部署本来就没配共享存储
  （`LARKAX_REDIS_DSN` 为空，配置如此）。单实例下完全正确；多实例下总放行量
  最高到「配置值 × 实例数」。启动日志里有一条 `warn` 说这一种。
- 仪表是 0、那个计数器**大于 0** → 配了 Redis 但连不上（故障）。请求不受
  影响（降级是有意的方向，见 [limits.md](limits.md)），但额度会松。
  恢复之后仪表自己变回 1，不需要重启。

读的是那个计数器的**取值**，不是它的增速。启动期那一次探活失败也记一次，
所以「配了、连不上、还没来过请求」这个实例的计数器是 1，而它只涨过那一次：
按增速读会把它读成「没配」，正好是这条判断要分开的那一种。
探活那一次也得记：还没有请求时，进程里只有它留下过「配了但连不上」的痕迹。
有请求之后每一次降级也各记一次，所以它一直是 0 只可能是没配。

**`ratelimit_exact` 这一行不存在。** 这个实例上没接限流器：worker 角色不接
推理流量，也就不按 key 计数。一个「永远是 0」的仪表在这里会把「没接」说成
「近似」，所以它没被设置过时整族不输出（见下面那条约定）。

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

- 名字以 `larkax_` 开头，之后是子系统（`billing`、`upstream`、`ledger`……）。
  **计数器以 `_total` 结尾，仪表不带这个后缀**：`_total` 在 Prometheus 的
  约定里就是「只增不减」，一个可升可降的数带上它，采集端按名字推断类型、
  再拿 `rate()` 去算就会得出没有意义的结论。
- **计数器只增不减，仪表可升可降**，分成两个类型（`Counter` 与 `Gauge`）
  而不是一个带 `Set` 的计数器：一个只增的数被谁 `Set` 回去，是这里最坏的
  一类 bug（曲线忽然回头，而「一直在涨」是看它的人唯一的判据）。
- **标签值必须来自有限集合。** 用户 id、渠道 id、请求 id、模型名一律不做标签：
  每个取值各占一条时间序列，而序列数没有上限，采集端的内存与查询开销会跟着
  请求数一起涨。要区分渠道就用渠道**类型**，要区分上游就用上游**种类**。
  这也是为什么「某个 key 的用量」不在指标里，而在
  `/ratelimit/usage?key_id=…` 那条只读查询上（见 [limits.md](limits.md)）。
- 每个指标都要有 `# HELP`，写清楚它是什么，以及什么取值算异常。
- 时间用秒并以 `_seconds` 结尾；金额不带单位后缀时以元为单位。**唯一的例外是
  `larkax_billing_capped_amount_total`**：它按最小单位（1e-8）累加整数，因为
  这笔金额细到一次只有几十个最小单位，浮点元在这里会丢精度，而它还要能和库里
  `numeric(20, 8)` 的同一个差额逐分对得上。

## 加一个指标

三步，前两步都在 `internal/observe/metrics.go`：

1. 在 `NewRegistry` 里加一行，字段的创建与登记写在同一次调用里：

   ```go
   r.XxxTotal = r.newCounter("larkax_xxx_total", "说明")
   r.YyyTotal = r.newLabeled("larkax_yyy_total", "说明", "kind")
   r.Zzz = r.newGauge("larkax_zzz", "说明")
   ```

2. 在 `Registry` 上加对应的导出字段。**不要只声明字段、忘了登记**：那样它安静地
   不输出，与「根本没接上」长得一模一样（`TestEveryDeclaredCounterIsRegistered`
   会拦住这种写法）。
3. 在增量点调用 `observe.Metrics.XxxTotal.Inc()`（带标签的用
   `observe.Metrics.YyyTotal.With("值").Inc()`，仪表用 `.Set(n)`）。

增量点**只加一行**：不要为了计数去改被计数的逻辑，也不要在计数器里做判断。
标签值一律用常量或有限枚举，不要从请求里取。

不带标签的计数器总是输出，包括值为 0 的时候——「一直是 0」本身就是要看的那个值。
带标签的计数器在第一次自增之前没有任何序列，整族不输出。**仪表按第三套：
没被 `Set` 过就不输出**——仪表上的 0 往往是一个有意义的取值（限流那个 0 是
「近似」），所以「没设置过」必须与「值就是 0」分得开，否则一个没接仪表的进程
会输出一行 0，看起来与「接上了、当前是 0」一模一样。

最后更新上面那张清单表：它是这份文档里唯一会被采集端依赖的部分。
