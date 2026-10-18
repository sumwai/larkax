# larkax 文档索引

本目录的文档面向**参与开发的人**，全部是中文。[README](../README.md) 是英文的，
它自带一条从克隆到一次真实调用的最短路径（Quickstart），只想把服务跑起来的人
不必读这里。

本目录里的每一份都只讲一件事，按「想做什么」找：

## 上手

| 想做的事 | 看哪份 |
|---|---|
| 在干净机器上把服务跑起来、发出第一次请求 | [README 的 Quickstart](../README.md#quickstart)（英文），逐条的命令与判据；[development.md](development.md) 是同一件事的详细版 |
| 本机环境怎么搭、`make` 各目标做什么、`.env` 与主密钥怎么用 | [development.md](development.md) |
| 常用检查怎么跑、CI 跑哪些门、变异测试怎么读 | [development.md](development.md) 的「常用检查」「CI 跑什么」「变异测试」 |
| 加一个环境变量或一个 `settings` 键 | [development.md](development.md) 的「加一个配置项」 |
| 某个配置键的默认值、取值范围、改了什么时候生效 | [settings.md](settings.md) |
| 查 `larkaxctl` 的全局约定与退出码 | [cli-reference.md](cli-reference.md) |
| 查命令输出的形状（颜色、表格、空清单、时刻与时区） | [cli-output.md](cli-output.md) |

## 接调用方

| 想做的事 | 看哪份 |
|---|---|
| 四条路径分别是什么协议、响应头与状态码、错误体的形状、流式的帧序列 | [api.md](api.md) |
| 一把 Key 的每分钟次数与累计额度是什么语义、超限时回什么 | [limits.md](limits.md) |
| 同一个请求重发两次只执行一次（`Idempotency-Key`） | [idempotency.md](idempotency.md) |
| 单次请求的输出上限（`max_tokens`）怎么定、超了回什么 | [max-tokens.md](max-tokens.md) |
| 发布版本与 API 版本的区分、哪些改动算破坏性 | [versioning.md](versioning.md) |

## 计费与钱

| 想做的事 | 看哪份 |
|---|---|
| 钱包快照与账本对不上时怎么查 | [reconciliation.md](reconciliation.md) |
| 分时计价的节假日数据从哪来、文件什么格式 | [calendar-data.md](calendar-data.md) |

## 观测与运维

| 想做的事 | 看哪份 |
|---|---|
| 有哪些指标、每一条该配什么告警 | [metrics.md](metrics.md) |
| 上游渠道的健康度与额度状态从哪读 | [channel-health.md](channel-health.md) |
| 请求快照留了什么、怎么读一条、怎么清到期分区 | [request-retention.md](request-retention.md) |
| `usage_events` 的月分区怎么预建、缺了会怎样 | [partitions.md](partitions.md) |

## 扩展

| 想做的事 | 看哪份 |
|---|---|
| 写一个插件：五个 hook 的契约、沙箱边界、失败怎么算 | [plugin-contract.md](plugin-contract.md) |
| 契约测试怎么跑、怎么加一条、有哪些已知偏差 | [contract/README.md](../contract/README.md) |
