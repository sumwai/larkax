# 运行期配置（`settings` 表）

拿不到数据库时**不需要**的值放在这张表里：留存期、功能开关、限额、默认品种。
连库本身需要的值（DSN、主密钥）与每个实例可以不同的值（监听地址、日志级别）
仍是环境变量，见 [development.md](development.md) 的「环境变量」一节与
[.env.example](../.env.example) 顶部的说明。密钥类配置也不放这里——这张表的值
明文落盘并进备份，渠道凭证走 `channels` 表的信封加密。

一共 **23 个键**。清单的唯一来源是 `internal/settings` 的 `Registry`，默认值的
唯一来源是**迁移文件**（代码里不再写第二份，两处默认值迟早会不一致）。

## 加载：不一致就拒绝启动

服务启动时把库里的行与 `Registry` 逐条对照，任何不一致都**报错退出，不回落到
默认值**（静默回落会让「配置写错了」表现为「行为跟预期不一样」，排查成本高得多）：

| 情况 | 结果 |
|---|---|
| 库里有一个 `Registry` 没声明的键 | 拒绝启动，指出键名 |
| `Registry` 声明了、库里没有的键 | 拒绝启动，列出缺哪几个（写一条迁移把这几个键插进去） |
| 值不是合法 JSON（字符串漏了引号，写成 `CNY` 而不是 `"CNY"`） | 拒绝启动，字符串那一类会单独点一句 |
| 值的类型与声明的不符 | 拒绝启动 |

所以**往库里手工写一行新配置是行不通的**：先加迁移里的 `INSERT`，再在
`Registry` 里加一行声明，两处都要改。

## 怎么改

```sh
larkaxctl settings list
larkaxctl settings set --key plugin.enabled --value true --by alice
```

- **`--by` 必填**（用户 ID 或用户名）：`settings.updated_by` 记的是谁改的，一条
  不记名的写入等于没有审计。缺它直接报参数错（退出码 2）。
- 整数直接写数字、布尔只认 `true` / `false`、字符串直接写、**不加引号**。
- **有五个键不接受这条路径的修改**（`settings.editable` 为 `false`，改错后果严重，
  只允许由迁移写入）：`billing.default_asset`、`calendar.default_region`、
  `reconcile.repair_mode`、`reconcile.difference_account`、`admin.token_sha256`。
  命令会报错并指出是哪一个。
- 标着「未接线」的键写得进去，但改了没有任何效果。今天一个都没有：最后一个
  （`capture.consent_max_window_hours`）的读者是 `larkaxctl consent grant`。

**什么时候生效**：下表里除 `reconcile.*`、`idempotency.retention_hours`、
`capture.consent_max_window_hours` 与 `admin.token_sha256` 之外，都是**装配期
读一次**——改完要重启服务。那三个是每轮 / 每次读的，管理令牌是每个管理面请求
读一次，改完下一次就生效。`billing.merchant_share_bp` 在每次结算时从装配期
那份快照里取值，快照本身不刷新，所以同样要重启。

## 请求快照

见 [request-retention.md](request-retention.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `request_snapshot.enabled` | bool | `true` | `true` / `false` | 是否采集。关掉后不脱敏、不写新快照；**存量分区的清理照旧执行**，它不看这个开关 |
| `request_snapshot.retention_days` | int | `30` | 正数（天） | 快照分区到期整块 `DROP` 的天数。**关掉采集也照旧生效**——存量分区按它算到期线，所以它必须为正，否则清理会把刚写进去的分区当天删掉 |
| `request_snapshot.max_bytes` | int | `65536` | 开启时为正数 | 单条快照正文的字节上限。骨架与授权后原样留下的正文吃的是同一个上限；超限降级成只保留顶层标量的摘要层，摘要层仍超限时整条丢弃。只在采集开启时校验 |

## 授权采集

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `capture.consent_max_window_hours` | int | `24` | 正数（小时） | 一次原样采集授权能开多长。`larkaxctl consent grant` 超过它就报错拒绝，不夹到上限：`single_request` 比的是此刻到 `--expires-at`，`time_window` 比的是 `--from` 到 `--to` 的长度 |

## 额度探测

见 [channel-health.md](channel-health.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `quota.probe_interval_seconds` | int | `300` | `1`–`31536000` | 额度探测的默认间隔，**所有渠道共用这一个值**——没有按渠道覆盖的入口（`channels.setting` 这一列今天不被这一项读）。它同时是两件事的间隔：渠道被用到而上次尝试早于它时，异步探测一次（见 [channel-health.md](channel-health.md) 的「探测什么时候发生」）；额度可信度的**衰减起点由它导出**——连着三个间隔都没拿到新的实测值，就认为探测本身出了问题。越界时服务拒绝启动 |
| `quota.exhausted_cooldown_seconds` | int | `900` | `1`–`31536000` | 余量见底的渠道重新进入候选集的冷却期：距上一次探测尝试满这么多秒，路由放行它回到候选集，被尝试到时重新探测；探测结果落库前它一直在候选里，仍有 0 就再等一个冷却期（那次尝试会把 `last_probe_attempt_at` 推到当下）。它只决定渠道多快回到候选集，**真正再探测一次仍由 `quota.probe_interval_seconds` 把关**——配得比探测间隔还短时，它会在候选集里待到探测间隔到了为止，这期间每次请求都可能真的打到一条见底的渠道。详见 [channel-health.md](channel-health.md) 的「余量见底的渠道怎么恢复」。越界时服务拒绝启动 |

## 计费

见 [max-tokens.md](max-tokens.md) 与 [metrics.md](metrics.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `billing.max_output_tokens` | int | `65536` | `1`–`1048576` | 单次请求允许声明的 `max_tokens` 上界。模型目录里登记了 `output_limit` 的按目录那个数判，没登记的按这个数判；非流式请求超过它会被拒（`400`）。越界时服务拒绝启动 |
| `billing.stream_freeze_step` | int | `4096` | `1`–`65536` | 流式请求递进冻结的段（token）：本地累计产出每越过一段就补冻一次，**同时这也是冻结额最多领先已产出量的量**。调大它数据库往返少、冻住的余量多；调小反之 |
| `billing.merchant_share_bp` | int | `0` | `0`–`10000` | 售价里分给渠道商户的比例，单位基点（`10000` = 100%，平台抽 5% 时填 `9500`）。分给商户的金额按售价乘以它、**向下取整**，零头归平台；`0` 表示不分账，消费里没有 `merchant_payable` 那条分录。越界或读不出来时按 `0` 处理并写一条告警，请求照常完成 |
| `billing.default_asset` | string | `"CNY"` | 目前只有 `CNY` | 新用户钱包与结算用的资产品种。**不接受 `settings set`** |

## 日历

见 [calendar-data.md](calendar-data.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `calendar.default_region` | string | `"CN"` | 目前只有 `CN` | 分时计价默认使用的节假日地区。**不接受 `settings set`** |

## 幂等记录

见 [idempotency.md](idempotency.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `idempotency.retention_hours` | int | `24` | 正数（小时） | 幂等记录的留存小时数，到期由 `larkaxctl maintenance idempotency` 删除。改完不必重启，下一次清理就按新值算 |

## 支付

见 [cli-reference.md](cli-reference.md) 的「wallet payment」一节。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `payment.fake_enabled` | bool | `false` | `true` / `false` | 假支付通道的开关。打开后才挂上回调入口、才允许 `wallet payment` 下单。它只用于本地联调：签名密钥是源码里的公开占位值，**不要在接真实流量的部署上打开**。改完要重启才生效——装配期读一次 |

## 插件

见 [plugin-contract.md](plugin-contract.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `plugin.enabled` | bool | `false` | `true` / `false` | 关闭时整条链路不装配：不读表、不构造注册表，请求按没有插件处理 |
| `plugin.call_timeout_ms` | int | `5000` | `1`–`3600000` | 单次 hook 调用的时限（毫秒）。超时按插件失败处理，不拖垮请求。上界取一小时：既远超任何合理的 hook 时限，也把「毫秒换算成纳秒」那条溢出整个堵死 |
| `plugin.memory_limit_bytes` | int | `67108864` | 正数 | 单个插件运行时的内存上限。**超限中断无法被脚本捕获** |
| `plugin.max_payload_bytes` | int | `4194304` | 正数 | 插件单次调用的入参与出参字节上限。内存上限管不住宿主塞进去的值，这一项才管得住 |

`plugin.enabled` 为 `false` 时**另外三项不读也不校验**：关了就不跑插件，限额填错
也不会有后果，此时拒绝启动只会让「先关掉再说」这条退路也走不通。快照那三项的
`max_bytes` 同理。

## 对账

见 [reconciliation.md](reconciliation.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `reconcile.auto_repair` | bool | `false` | `true` / `false` | 发现不一致时是否自动修正。关闭时只报告 |
| `reconcile.repair_mode` | string | `"resync_snapshot"` | `resync_snapshot` / `adjust_ledger` | 修正方向：以账本为准把快照重算成账本值（不写分录），或以快照为准写一条调账分录。**不接受 `settings set`** |
| `reconcile.difference_account` | string | `"reconcile_difference"` | 科目名 | `repair_mode=adjust_ledger` 时修正分录的对家科目。默认单独归集，不污染平台收入。**不接受 `settings set`** |
| `reconcile.shard_size` | int | `1000` | 正数 | 一批比对的钱包行数上限。按 `user_id` 分片逐批处理，避免一次把大表读进内存 |
| `reconcile.full_scan_interval_hours` | int | `168` | `0` 表示每次都全量 | 距上一次全量扫描超过这个小时数就做一次全量。增量只看上次起点之后变动过的行，而「两边都早就错着」的那种不一致只有全量能发现 |

## 管理面

见 [admin-api.md](admin-api.md)。

| 键 | 类型 | 默认值 | 取值 | 影响 |
|---|---|---|---|---|
| `admin.token_sha256` | string | `""` | 空串（没配置），或 64 位小写十六进制 | 内部监听上 `/admin/v1/*` 管理令牌的 SHA-256。空串时管理面一律回 `503`；撤销之后是 64 个 0（没有令牌能匹配上，请求得到 `401`）。只由 `larkaxctl admin token create` / `revoke` 写入，**不接受 `settings set`**；改完下一个请求就生效 |

## 加一个键

1. 写一条迁移：`INSERT INTO settings (key, value, description, editable) VALUES (...)`。
   **默认值只写在这里**，`description` 是给 `settings list` 用的一句话说明。
2. 在 `internal/settings` 的 `Registry` 里加一行声明类型（还没有读者的键，
   同时把 `Unwired` 写上它的理由）。
3. 在读它的那个包里加一个 `KeyXxx` 常量，别在各处散写字面量——键名拼错在运行期
   的表现是「启动就失败」，一眼看不出是拼写问题。
4. 有取值范围的在读的地方校验并**返回错误**（`internal/assembly` 装配期读的那几个
   就是这样），不要回落到默认值。
