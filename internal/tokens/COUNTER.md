# 计数与估算

本文件说明 `internal/tokens` 的计数口径、估算式的来源与误差范围。
只写「是什么、怎么用、怎么改」。

## 是什么

按模型把一段文本换算成 token 数，并标明这个数是实测的、本地精确的，
还是估算的。不做计费——那在 `internal/billing`。

计数的三种来源由 `Source` 承载：

| 来源 | 含义 | 能否与上游对账 |
|---|---|---|
| `measured` | 上游上报的 usage（或 `count_tokens` 端点） | 能 |
| `tokenized` | 用词表本地精确计数 | 不能（差在框架与特殊 token） |
| `estimated` | 启发式估算 | 不能 |

**来源与数字绑在同一个结构体里**（`Count`），这是有意的：
只要两者可以分开传递，就总会有人只传数字，而账上就再也分不出
「估的」与「实测的」。

## 怎么用

```go
c := tokens.NewCounter()
// 服务启动时调一次。返回值是每种编码的加载耗时，用来回答
// 「那 25ms 是启动时花的还是第一个请求时花的」。
// 失败不是致命的：不预热只是慢，不是不能用。
durations, err := c.Prepare(context.Background())
// durations 是每种编码各花了多久，c.Loaded() 是实际解开了哪几种。
slog.Info("词表已加载", "耗时", durations, "已加载", c.Loaded())
slog.Info("本机能精确计数", "encodings", tokens.AvailableEncodings())

// 单段文本
n, _ := c.CountText(ctx, model, text)

// 一次流式调用的兜底
est := tokens.NewStreamEstimator(c, upstreamModel)
est.ObserveRequest(requestText, len(messages), len(tools))
for ev := range events {
    switch ev.Kind {
    case "delta":
        est.ObserveDelta(ev.Text)
    case "tool_use":
        est.ObserveToolCall(ev.ToolCall.ID, ev.ToolCall.Name, ev.ToolCall.Arguments)
    }
}
u := est.Result()
```

流式调用必须走 `StreamEstimator`：**上游的 usage 只在最后一个分片里返回**，
客户端中途断开时那一片永远不会到达，而费用已经产生。

它只负责「拿不到上游实测 usage」那一半。上游在流里报了 usage 时不经过它：
实测值优先那条判据由调用方持有（`internal/gateway/stream.go` 的 `streamUsage`），
因为流里的 usage 分着输入与输出、还带缓存与推理的明细，而 `Count` 只有一个
总数——压成一个数之后计费再也拆不回来。

`Result()` 返回的 `Usage` 把输入与输出分成两个 `Count`：两者的来源可以不同
（输入由 `ObserveRequest` 一次性给出，输出是逐片累加的），而累加过程中最不
可信的那一段决定输出侧整体的来源。`Usage.Total()` 取两者中较不可信的那个。

不需要维持一个累加器、只想问「这段文本大概多少 token」时，用
`EstimatedTokens(model, text)` 或 `CountTextEstimated`。后者强制走估算，
用途是上游换了词表而我们还没跟上时——一个基于过期词表的「精确」数字
比明说是估算更危险。

### 缓存口径归一在哪

**不在本包。** 上游 usage → `channel.Usage`（`PromptTokens` **含**缓存，
缓存明细另立字段）的换算只有一处实现：`internal/relay` 的
`UsageFromRelaykit`。本包只做「一段文本有多少 token」，不认上游报的字段名。

三家口径互不相容，而写错的后果是重复计费或漏计：

| 上游 / `UsageSemantic` | 输入总量 | 缓存字段 |
|---|---|---|
| OpenAI（`openai`） | `prompt_tokens`（**已含**缓存） | `prompt_tokens_details.cached_tokens` 是它的子集 |
| DeepSeek 一类（也走 `openai`） | 同上 | 只报 `prompt_cache_hit_tokens`，适配器把它填进 `cached_tokens` |
| Anthropic（`anthropic`） | `input_tokens` + `cache_read` + `cache_creation` | 分列，`input_tokens` **不含**缓存 |
| Gemini（`gemini`） | 同 OpenAI | 同 OpenAI；relaykit 侧的口径，本项目暂无适配器 |

哪一家成立由上游声明的 `dto.Usage.UsageSemantic` 决定，不由数字反推——
整段 prompt 命中缓存时 Anthropic 的 `input_tokens` 就是 0。逐条口径与
依据写在 `UsageFromRelaykit` 的注释里。

Anthropic 的 `cache_creation` 还有 5m / 1h 拆分，它们**已经包含在**
`cache_creation_input_tokens` 里，只影响单价、不进总量——`UsageFromRelaykit`
把它们放进 `UnitCounts`（`cache_write_5m` / `cache_write_1h`）而不是
`PromptTokens`。

上游已经给了权威数字（usage 字段、或 `count_tokens` 端点）时，来源标记由
**拿到那个数字的那一层**打：本包不给它做一个「包成 `Count`」的入口——
上游报的用量不是一个总数（见上文「缓存口径归一在哪」），而调用方手上
本来就是 `channel.Usage`，它比 `Count` 宽。让调用方自己构造 `Count`
只在「确实只有一个数」时才是对的，而那种上游今天不存在。

### 输入侧的非正文部分：工具定义、图片与历史里的工具调用参数

上游报的输入量**含**这三项，而正文口径（`channel.Request.PromptText`，
只拼 `Content[].Text`）一项都不含。要在请求发出前估一个输入量，它们必须各自
补上——预留是**上界**，漏一项就是「预留 < 结算」，差额由平台承担。三个入口：

| 函数 / 常量 | 估的是什么 | 依据 |
|---|---|---|
| `ToolDefinitionInputTokens(model, json)` | 一条工具定义 | JSON 序列化后的字节：启发式估算与 `0.35 token/字节` 的地板取较大者，再加 `ToolDefinitionReserveFrameTokens`（26）的包装 |
| `ImageInputTokens` | 一张图片 | 两家公开公式在 1024×1024 上的较大者：Anthropic `w×h/750 ≈ 1398`、OpenAI `85 + 170×tile = 765` |
| `ToolUseInputTokens(model, json)` | 历史消息里**一块** `tool_use` 的参数 | 与工具定义同一条式子（按字节估 + 包装），包装项是 `ToolUseReserveFrameTokens`（12） |

这一栏的三个包装常数**都不是**计费侧那两个（`ToolDefinitionFrameTokens` = 8、
`ToolCallFrameTokens` = 6，见 `stream.go`）：那边是拿不到实测值时**向用户计费**的
下界，估高了就是一笔凭空费用；这边是**冻余额**，估低的代价由平台承担。同一个包装
在两条路上代价方向相反，所以刻意不是同一个数。

`ToolUseInputTokens` 那一项**随轮数增长**：多轮工具调用的会话里，每一轮调用的参数
都会随后续请求重复出现在历史里，不是常数。

三者的方向都是**偏保守**（宁可多冻），理由与包注释里那条一致。两条已知边界，
别当成 bug：

- **比 1024×1024 更大的图片仍然会低估**（Anthropic 在 1568×1568 上是 3277）。
  图片在两种形态下都拿不到分辨率：URL 只有一个链接，base64 的字节数与分辨率
  对应关系很差（同分辨率的 PNG 与 JPEG 能差十倍），所以只能取常数，不造假分档。
- **图标级的小图会多冻一点**，那部分只在请求期间存在（预留结束即释放）。

它们**不是**给整体估算加的余量：按实际出现的那几项计，一项都没有时口径与没有
这几个数时逐字相同。改这几个数先看 `internal/api` 的 `inputTokensOf`——那是今天
唯一的消费者（预留），值被钉在 `payload_test.go` 与
`internal/api/input_tokens_test.go` 里。

## 怎么改

### 加一个能精确计数的族

1. 在 `internal/tokens/vocab.go` 的 `Family` 常量里加上族名。
2. 若该族的词表可以编译进二进制（tiktoken 格式），在 `encodingForModel`
   里映射到对应编码；若需要 `tokenizer.json`（HuggingFace 格式，
   DeepSeek / Qwen / GLM / MiniMax 属这一类），先引入
   `github.com/sugarme/tokenizer` 并按族附带词表文件。
3. 在 `FamilyOf` 的前缀表里加上识别规则。**用前缀**：上游每个版本都
   会在名字后面加日期或版本号，精确匹配会随上游发版而失效，而失效的
   表现是「悄悄退化成估算」——不报错，只是账变差。

### 调整估算式

权重与偏置在 `internal/tokens/estimate.go` 的表里。改之前先读下面的
标定方法：**表中每个数字都应当能用一组可复现的测量重新算出来**，
否则它只是一串没有依据的常数。

### 词表加载策略

`NewCounter` 不加载词表，`Prepare` 加载。这个划分是有意的：

- 词表以 Go 字符串常量的形式编译进二进制（o200k 约 7.9MB、cl100k 约
  3.9MB 的源码），进程启动时它们只是映射进来的页，不计入堆。
- 真正占堆的是 `tokenizer.Get` 时惰性构建的 map：实测 o200k 约 7MB 堆、
  首次构建约 25ms；cl100k 约 3.4MB、约 9ms。
- 所以「启动就解 16MB」不会发生，但**二进制体积**会固定增加约 16MB。
  对一个服务可接受（一次构建，分发时多 16MB），对 CLI 子命令不划算——
  这就是不把它放进 `NewCounter` 的原因。

`Prepare` 预解 `encodingForModel` **值域里的全部编码**（o200k、cl100k、r50k，
合计约 43ms、常驻 +21.3MB），其余编码按需惰性加载。为什么是这个集合、
每种各要多少代价，见 `vocab.go` 里 `preparedCodecs` 的注释。

生产侧由 `cmd/larkax` 在**监听端口之前**调一次 `Prepare`（见那里的
`warmVocab`），让那 25ms 不落在第一个用户请求上。失败只记日志：
不预热只是慢，不是不能用。`Loaded()` 与 `Prepare` 的返回值（每种编码的
加载耗时）一起打进启动日志，回答「到底是启动时解的还是第一个请求解的」。

## 估算式

没有词表时（Claude、未接入的族）按以下步骤算：

1. 按 Unicode 类别把文本切成若干段，连续的同类字符为一段。七类：
   拉丁字母、数字、中日韩、其他字母、空白、标点、符号。
2. 每段贡献 `max(权重[类] × 段长, 1)`。下限 1 是必须的：BPE 对任何
   非空段至少产生一个 token，而空白段的密度算出来会远小于 1。
3. 对长度 ≥ 24 且不中断的字母数字串（密钥、摘要、base64 的形态）
   施加额外下限：每字符至少 0.65 个 token。BPE 词表里没有这类串的
   片段，分词会退化成逐字符。
4. 乘以该族的偏置。偏置的取值使训练语料上约 90% 的样本
   「估算值 ≥ 真实值」。

### 为什么不用「字符数 ÷ 4」

「英文 4 字符 ≈ 1 token」只对英文散文成立。实测（o200k_base）：

| 文本类型 | token/字符 |
|---|---|
| 英文散文 | 0.19 |
| Markdown | 0.22 |
| JSON / 代码 | 0.26 ~ 0.29 |
| 西里尔 / 阿拉伯 | 0.30 |
| 数字、时间戳 | 0.46 ~ 0.59 |
| 中文 | 0.73 ~ 0.82 |
| 长密钥 / 摘要串 | 0.65 ~ 0.67 |
| emoji | 1.1 ~ 2.1 |

最大与最小差十倍。用一个常数，等于对中文低估三倍、对英文高估两倍。

### 实测的每类边际密度

| 类别 | o200k_base | cl100k_base |
|---|---|---|
| 拉丁字母 | 0.114 ~ 0.23 | 0.114 ~ 0.23 |
| 数字 | 0.46 ~ 0.59 | 0.46 ~ 0.59 |
| 汉字 | 0.78 | 1.17 |
| 假名 | 0.55 | 0.90 |
| 谚文 | 0.63 | 1.25 |
| 西里尔 | 0.42 | 0.58 |
| 阿拉伯 | 0.48 | 0.83 |
| 标点 | 0.63 | 0.66 |
| 空白 | 0.03 | 0.01 |
| emoji | 1.14 | 2.14 |

拉丁字母的区间跨度很大，是因为同一个字符数下「常见词」与「随机串」
差 6 倍（`latin-words` 0.114 vs `latin-rand` 0.654）。权重表取的是
在真实文本分布上拟合出的值，不是这两端。

## 误差范围

| 族 | 最小比例 | 中位比例 | 最大比例 | 依据 |
|---|---|---|---|---|
| OpenAI | 0.70 | 1.38 | 2.7 | 实测 |
| Claude | 0.70 | 1.8 | 3.5 | 推理（未实测） |
| 未知族 | 0.70 | 1.8 | 3.5 | 推理 |

比例 = 估算值 / 真实值。程序里对应的常量是 `OpenAIBounds` /
`ClaudeBounds` / `UnknownModelBounds`，每组都带一行 `Basis` 说明它的
来源——**这一行是数据的一部分**，不要只把依据写在注释或本文里。

短输入的额外说明见 `ShortInputNote`：不足约 16 字符时比例误差更宽
（实测低至 0.35），但绝对误差只有个位数 token。这句话在展示误差的地方
一律引用该常量，不要抄一份——抄那份会与这里的标定数字各自漂移。

### 标定方法

`OpenAIBounds` 是这样测出来的，可以复现：

1. **语料**：把七类字符各写成 8~10 条片段（真实形态：英文散文、中文、
   Go 代码、JSON、时间戳、UUID、base64、URL、emoji、西里尔/阿拉伯/
   假名/谚文），再随机拼接成 1~60 段的样本，每类字符的出现比例随机。
   用固定随机种子生成 1500~2000 条。
2. **真值**：`github.com/tiktoken-go/tokenizer` 的 `o200k_base` 与
   `cl100k_base` 分词结果。
3. **拟合**：对「每类字符数 → 真实 token 数」做最小二乘（列先标准化，
   保证数值稳定）。**不要**把「每段至少 1」放进回归——它是非线性项，
   会让拟合发散。迭代式处理：先按当前权重扣掉下限超出部分的残差，
   再回归。
4. **偏置**：在训练集上取分位数，使约 90% 的样本满足
   `估算 × bias ≥ 真实`。
5. **验证**：在**不同随机种子、不同长度分布**的留出集上重测
   （`holdout` 与 `short` 两组），确认比例区间稳定。
6. **逐长度分层**再测一遍：极短输入（<16 字符）的比例误差明显更宽，
   而绝对误差只有个位数 token——所以 `ErrorBounds` 描述的是长输入。

方向的选择（估算偏高）见包注释：估少了等于少收钱，且这个差额
永远不会被发现。

### Claude 那一列为什么是推理

Anthropic 不公开词表，本包拿不到任何可用于标定的真值。已知的两件事是：

- Claude 4.7+ 换了新 tokenizer，同文本的 token 数约为旧词表的 1.3 倍。
- 旧词表也不公开。

所以 `tableClaude` 是把未知族表乘 1.3 再留余量得到的，`ClaudeBounds`
同样。使用方应把它当作「量级正确、具体数字不可依赖」。
精确的替代途径是上游的 `POST /v1/messages/count_tokens`——那是调用方的
事（本包不发网络请求），拿到结果后用 `SourceMeasured` 记录。

## 尚未接入的族

| 族 | 词表可得性 | 现状 |
|---|---|---|
| OpenAI | 开源，随二进制分发 | 精确计数 |
| DeepSeek / Qwen | HuggingFace `tokenizer.json` | 估算（可接入） |
| Kimi | `tiktoken.model` | 估算（可接入） |
| GLM | 可离线，精度未确认 | 估算（可接入） |
| Claude | 不公开 | 估算 |

## 边界

本包**只做计数与估算**，不做这些：

- **不发网络请求**。Claude 那唯一精确的途径是上游的
  `POST /v1/messages/count_tokens`，那由调用方发起，拿到结果后自己标上
  `SourceMeasured`。本包不持有凭证也不该持有。
- **不算钱**。价格表与公式在 `internal/billing`。
- **不碰数据库**。谁持久化这些数字由调用方决定。

接入 HF 词表需要引入 `github.com/sugarme/tokenizer` 并按族附带词表
文件（每份几 MB）。做之前先问：这一族的流量占比是否值得多几十 MB 的
二进制与一份要跟着上游更新的文件。
