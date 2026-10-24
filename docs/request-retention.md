# 请求留存

larkax 保留每个请求的**结构**，不保留**内容**。

展开说：为了能复现报错，我们需要知道这次请求长什么样——用了哪个模型、
推理深度多少、max_tokens 多大、消息有几条、每条内容的块类型与顺序、
工具的参数键名有哪些。这些都在。而用户写了什么提示词、工具叫什么名字、
模型思考了什么内容、图片的字节——这些不在。

字段清单与各字段的取值规则，见 `internal/migrate/sql/00001_init.sql` 中
`request_snapshots` 表的列注释，以及 [docs/development.md](development.md)。
这份文档面向使用者，说明你能得到什么保证。

## 你会得到什么保证

**请求正文不会被留存。** 默认模式下，提示词、工具定义、思考内容、图片与音频数据
在离开网关之前就被替换为占位描述符。到达数据库的不是原文，也不是加密后的原文，
而是 `{"_t":"text","chars":842}` 这样的结构描述。唯一例外是你**自己开的授权**
（见下面「排查问题时想看到原文」），而那一段有明确的范围与到期时间。

**未知字段默认丢弃形状。** 上游 API 新增字段时，它默认被当作内容处理，
而不是默认被当作安全字段保留。所以「新字段悄悄泄露了内容」这类问题不会发生。

**脱敏失败就丢快照。** 如果处理过程中出任何异常，整条快照被丢弃，
不会回落到「保留原文」。宁可失去一次排障能力，也不会赌一次泄露。

**留存期默认 30 天，由运维命令兑现。** 快照按天分区，到期整分区删除——删除发生在
`larkaxctl snapshot cleanup` 里，**不在请求路径上发生**，服务自己也不会去做这件事
（见 [development.md](development.md) 的「请求快照」）。所以在它被删掉之前，到期分区
里的行**照样读得出来**。
按天分区是有意的：清理靠 `DROP TABLE` 而不是 `DELETE`，不会因为一次写错的语句波及历史。

**可以看，但会被记录。** 读一条请求的结构要走
`larkaxctl snapshot get <request_id> --actor <用户 ID 或用户名> --reason <文本>`：
`--actor` 与 `--reason` 都必填，每一次读都会往 `snapshot_access_log` 写一行
（谁、什么时候、什么理由、读了哪一条）。我们内部也一样——有审计日志，才有事后
追溯的可能。（控制台 `web/` 尚未落地，今天只有这一条路。）

**审计行没有留存期，只追加。** 上面那条留存期是对**快照本身**说的，`snapshot_access_log`
不跟它走：快照分区到期删掉之后，这些行是「谁在什么时候读过这条请求」唯一的痕迹，
所以没有任何一条路会删它们——没有清理命令，也没有对应的留存期配置。

## 账号销户

账号可以销户，但它**不是把行删掉**。这条路径分两步，由运维执行
（`larkaxctl user delete`，用法见 [cli-reference.md](cli-reference.md) 的「user」
一节）：

1. **打标，可撤。** 账号进入 `deletion_requested`，从这一刻起它不能再鉴权；
   打标之前要求手里的 API Key 已经全部吊销，所以吊销是这一步之前做的动作。
   原来的状态被记下来，撤销之后回到那一个状态（吊销过的 key 保持吊销）。
2. **匿名化，不可逆。** 只接已经打过标的账号，并且要逐字写出用户名才会执行。
   它在同一个事务里作废 API Key、清掉用户态插件 KV、撤销采集授权，并把用户名、
   邮箱与展示名换成不可逆的占位（`deleted-user-<ID>`、
   `deleted-user-<ID>@example.com`、`已注销用户 #<ID>`）。

打标之前还要求这个账号名下已经清理干净：API Key 全部吊销、商户应付科目
（`merchant_payable:<用户 ID>`）的余额为 0、名下渠道都已删。缺一条就拒绝，并说清
先做哪一步（各自的清理命令见 [cli-reference.md](cli-reference.md) 的「user」一节）。
余额与冻结额在打标与匿名化时各判一次；否则命令拒绝，并说清先要做什么。

终态下各项数据的去处：

| 数据 | 销户之后 |
|---|---|
| `users` 行 | 保留。`status = deleted`、写下 `deleted_at`，三个身份字段是占位值 |
| API Key | 行保留，状态置为 `revoked`；此前停用过的保留它原来的状态。`name` 备注清空 |
| 用户态插件 KV | 删除属于这个用户的行 |
| 采集授权（`capture_consents`） | 行保留，未撤销的写 `revoked_at`；到期仍按各自的 `expires_at` |
| 钱包与账本分录 | **保留**。财务记录不随账号删除清除（见下面「不在这份保证范围内的东西」） |
| 请求快照 | **不动**。按各自的留存期由 `snapshot cleanup` 删除，与账号无关 |

## 排查问题时想看到原文

原样采集要由你**主动开一条授权**：

```sh
larkaxctl consent grant --user alice --scope single_request \
    --request-id req-abc --expires-at 2026-10-18T18:00:00+08:00 --agree
larkaxctl consent grant --user alice --scope time_window \
    --from 2026-10-18T14:00:00+08:00 --to 2026-10-18T15:00:00+08:00 --agree
larkaxctl consent list
larkaxctl consent revoke 7
```

两个范围可选：**单个请求**只对 `--request-id` 那一次调用生效，**一段时间窗口**对
`--from` 到 `--to` 之间的全部请求生效。一次授权的时长有上限（`settings` 的
`capture.consent_max_window_hours`，见 [settings.md](settings.md)）：超过上限的申请被
拒绝，不会被悄悄缩短。

`--agree` 是必须的。不带它时命令只把同意文案打出来、什么都不写：同意文案与它的版本号
要一起落进 `capture_consents`（`consent_text`、`consent_version`），这是合规举证所需要的
——事后要能回答「当时同意的是哪一句话」。

授权**自动到期**，不需要你回来手动关闭。你也可以随时撤销，而**撤销立即生效**：判定发生
在每一次请求脱敏的那一刻，并且每次都重新查一次授权记录，没有缓存。到期、撤销、时间窗
没开始或已经过去，四种情况都在那一刻现判。

授权生效期间，这次请求的正文会原样存下来，并标记为「用户授权采集」
（`capture_mode = full_consented`），可以单独筛出来审计；没有授权的那些仍然是骨架
（`skeleton`）。读原文还是走 `snapshot get`，同样写访问日志。

读的时候会按这一条的用户与请求再查一次授权，而且**没有缓存**：授权到期或撤销之后，
已经存下来的正文不再打出来；这次查询失败时同样按「没有授权」处理，宁可不显示，
也不放行。所以原文的可读期就是授权的有效期，不必等哪一步把它从盘上删掉——留存期
仍与骨架相同，由 `larkaxctl snapshot cleanup` 按分区兑现。

## 决策接口的留存范围

`/v1/systemone`（见 [development.md](development.md)）这条路径**也留存，但只留
判据**：它记的是「这次决策有多少候选、各自什么状态、最后选中了谁、试过谁、
每一步为什么不成」，而不是它问了什么、模型答了什么。

这条表面上的内容——请求里的 `state` 与 `questions`、响应里的 `answers`——
**不经过脱敏器**：它们根本不会被摊进快照的输入，所以不存在「脱敏规则写漏了
一个字段」这类风险。这是刻意的取舍：`state` 里装的是用户原文（与提示词同一类），
`answers` 里装的是模型判断的结果。

一条决策请求落**两行**（与对话请求一样），方向分别是 `request` 与 `response`：

- `request` 方向记**选路之前**手上的候选集合与它们的判据；
- `response` 方向记**选路之后**的结局：选中的渠道与上游名、每一次尝试的结果
  （是否值得换下一个渠道、失败分类、上游状态码、耗时），同一份候选集合
  上哪几个被选中、哪几个被淘汰，以及**被选路筛掉的候选与原因**。

**怎么区分这两条表面。** `request_snapshots.direction` 只有 `request` 与
`response` 两个取值，决策请求与对话请求落在相同的两个方向里。区分它们的是
骨架里的 `surface` 字段：对话是 `chat`，决策是 `decision`。

**这条表面记下来的全部字段**（这张清单**逐个对得上代码里的允许表**，
由 `internal/snapshot` 的 `TestDecisionAllowlistDocumented` 钉住：
清单里少一个、多一个、写错一个都会让那条用例变红。所以它就是准确的答案，
而不是一份会过期的说明）：

```text
surface                        这一行是哪条表面产出的：chat 还是 decision
model                          入站解析之后的规范模型名（不是调用方写的原名，也不是上游名）
candidates_count               候选数
candidates                     候选集合
candidates[].channel_id        候选渠道的 id，也是与账单对上的那个数
candidates[].priority          排序的第一判据：数值大者先试
candidates[].weight            渠道上配的权重；不参与排序，只被记下来
candidates[].consecutive_failures  连续失败次数
candidates[].success_rate      近期成功率
candidates[].latency_p95_ms    近期响应时间分位（毫秒）
candidates[].quota_unit        余量的单位
candidates[].quota_remaining   剩余量
candidates[].quota_confidence  这个余量有多可信
candidates[].quota_kind        余量的来源：实测 / 自记账 / 错误观测
candidates[].cost_per_unit     最近观察到的单位成本
candidates[].cache_hit_rate    近期缓存命中率
candidates[].sample_count      上面两条统计的样本数
selected                       实际选中并发出去的那一个（只有成功时才有）
selected.channel_id            选中的渠道，与账单上的 channel_id 是同一个数
selected.upstream_model        这次实际发出去的上游模型名
candidates[].selected          这一段候选是不是被选中的那个
attempts                       按顺序记下每一次尝试，含最后成功的那次
attempts[].channel_id          这一次试的是哪个渠道
attempts[].succeeded           这一次成没成
attempts[].retryable           这次的失败是否值得换下一个渠道
attempts[].upstream_status     这次拿到的上游 HTTP 状态码
attempts[].error_kind          这次的失败分类
attempts[].duration_ms         这一次耗了多久
excluded_count                 被选路筛掉的候选数（没有排除时整个字段不出现）
excluded                       被选路筛掉的候选（没有排除时整段不出现）
excluded[].channel_id          被筛掉的是哪个渠道
excluded[].code                为什么被筛掉：ineligible（配置不全）/ model_not_declared（渠道没声明支持这个模型）/ capability（目录能力）/ limit（目录上限）/ health（健康度）/ quota（额度）
```

`selected` 三项、`attempts` 七项、`excluded` 四项，以及 `candidates[].selected`，
只出现在 `response` 方向的行上；其余每一项在两个方向的行上都有。

`candidates[].cost_per_unit` 是**这个候选在本次请求的规范模型上**的单位成本：
桶按（渠道，规范模型，小时）存，读侧只取那个模型的那一行。该模型还没有带成本的
事件时这个值为空——不拿渠道级合并值冒充，因此空值读作「这个模型上还没有数据」，
而不是「免费」。

`excluded` 记的是「那几个没发出去的候选**为什么**不在候选里」的答案：
`candidates_count` 说的是选路**之前**有几个候选，`attempts` 说的是**真的发出去过**
哪几个，两者之间那几条从前只剩一道差额——差额本身看不出发成两端的原因。
`excluded[].code` 记的是**分类**（一个固定的词）而不是那段原文：原文里带着模型名与
数值，属于自由文本，不在留存范围内。

**它不是一份穷尽的划分。** 差额里还有「没轮到就被提前放弃」的候选——别名翻不出来、
客户端断开、遇到不可重试的失败时仍有候选没试过。它们既不在 `excluded` 里，也不在
`attempts` 里（不可重试那一种能从 `attempts[].retryable` 读出来）。

**`excluded` 的条数上界。** 它最多是这次请求手上候选集合的大小（被筛掉的候选
只能来自那个集合），而候选集合由渠道配置决定，没有单独的条数上限。整条骨架受
`request_snapshot.max_bytes`（默认 64 KiB）约束：超限时降级成只留顶层标量的摘要层，
那时 `excluded` 整段连同 `candidates`、`attempts` 一起消失，留下的是
`excluded_count` 与 `candidates_count` 两个标量——「几个候选里筛掉了几个」这件事
仍然读得出来。摘要层仍然超限时整条快照被丢弃——这一类有计数
（`larkax_snapshot_oversized_total`，见 [metrics.md](metrics.md)）：整条丢掉是
静默的，而现场只是「这张表里这一段时间是空的」，与「这段时间没有请求」长得一样。

没有数据的项**整个字段不出现**，而不是写成 0：「没有成本数据」与「成本是 0」
在选路里是两个不同的结论，快照把两者记成一样，下一次排查就得重新踩一遍。

**各维分数本身不在留存范围内。** 路由只给出加权后的总分，拆不开。
留下的是上面这些输入——它们才是「为什么选了它」能被人复核的部分。

**已知的偏差。** 决策这条表面回给我们的响应里没有 HTTP 状态码，所以一个
**成功**的决策请求在快照表上 `upstream_status` 与 `error_kind` 两列是 NULL。
这两列在响应方向的行上通常读作「一个 HTTP 响应都没拿到」，而这里不是那个意思；
每一次尝试的状态码记在骨架的 `attempts[].upstream_status` 里。


## 不在这份保证范围内的东西

- **账单记录永久保留。** 用量、金额、扣费明细属于财务记录，不随快照的留存期清除，
  也不随账号删除而清除。这是法定义务，不是我们的选择。
- **上游供应商会看到请求内容。** larkax 是转发方，请求最终发往你选择的模型供应商。
  他们的留存政策由他们决定，我们无法代为保证。哪些供应商、各自什么政策，
  会在文档站单独列出（尚未建立）。
- **网关层的访问日志**（源 IP、时间、状态码）与快照是两套独立的数据，分别管理。

## 请求内容与计费的关系

客户端在流式响应中途断开时，费用仍然产生并需要结算。这时 token 数无法从上游拿到，
只能当场按实际收到的内容估算。

这个估算发生在网关内存里，不经过快照链路——快照是异步落库、内容已脱敏、
且可能整条丢弃的。所以「估算用了多少 token」和「留存了什么结构」是两件互不影响的事。
