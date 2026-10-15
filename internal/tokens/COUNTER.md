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
// 「那 40ms 是启动时花的还是第一个请求时花的」。
durations, err := c.Prepare(context.Background())
slog.Info("词表已加载", "耗时", durations, "可精确计数", tokens.AvailableEncodings())

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
    case "usage":
        est.ObserveMeasured(tokens.Count{Tokens: ev.Usage.PromptTokens + ev.Usage.CompletionTokens})
    case "done":
        est.ObserveDone()
    }
}
u := est.Result()
if u.RequiresReview() {
    // 输出侧是估算值且量不小，值得人工看一眼
}
```

流式调用必须走 `StreamEstimator`：**上游的 usage 只在最后一个分片里返回**，
客户端中途断开时那一片永远不会到达，而费用已经产生。

`Result()` 返回的 `Usage` 有 `Complete` 字段：为 `false` 表示输出侧是本地
算的（断开、报错、或上游结束但没带 usage），这笔账要按估算处理。
`RequiresReview()` 进一步筛出「估算值且量不小」的那些。

不需要维持一个累加器、只想问「这段文本大概多少 token」时，用
`EstimatedTokens(model, text)` 或 `CountTextEstimated`。后者强制走估算，
用途是上游换了词表而我们还没跟上时——一个基于过期词表的「精确」数字
比明说是估算更危险。

### 缓存口径归一

`tokens.Normalize` 把各家上报的 usage 归一到 `channel.Usage`
（`PromptTokens` **含**缓存，缓存明细另立字段）。三家口径互不相容：

| 上游 | 输入总量 | 缓存字段 |
|---|---|---|
| OpenAI | `prompt_tokens`（**已含**缓存） | `prompt_tokens_details.cached_tokens` 是它的子集 |
| Anthropic | `input_tokens` + `cache_read` + `cache_creation` | 分列，`input_tokens` **不含**缓存 |
| DeepSeek | `prompt_cache_hit_tokens` + `prompt_cache_miss_tokens` | 分列 |

把 Anthropic 的缓存当成「额外的量」加两次、或把 OpenAI 的缓存再加一遍，
都会重复计费，而它不会报错。`normalize_test.go` 里逐条固定了这三个口径。

Anthropic 的 `cache_creation` 还有 5m / 1h 拆分，它们**已经包含在**
`cache_creation_input_tokens` 里，只影响单价、不进总量——所以走
`CacheWriteDetail` 而不是 `Normalize`。

上游已经给了权威数字（usage 字段、或 `count_tokens` 端点）时用
`RecordMeasured`：它只负责打对来源标记。让调用方自己构造 `Count`
等于把「别忘了标来源」交给每个调用点。

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
  首次构建约 40ms；cl100k 约 3.5MB、约 15ms。
- 所以「启动就解 16MB」不会发生，但**二进制体积**会固定增加约 16MB。
  对一个服务可接受（一次构建，分发时多 16MB），对 CLI 子命令不划算——
  这就是不把它放进 `NewCounter` 的原因。

生产环境在启动阶段调一次 `Prepare`，让那 40ms 不落在第一个用户请求上。
`Loaded()` 与 `Prepare` 的返回值（每种编码的加载耗时）用来在启动日志里
回答「到底是启动时解的还是第一个请求解的」。

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
（实测低至 0.35），但绝对误差只有个位数 token。

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
  `POST /v1/messages/count_tokens`，那由调用方发起，拿到结果后用
  `RecordMeasured` 记录。本包不持有凭证也不该持有。
- **不算钱**。价格表与公式在 `internal/billing`。
- **不碰数据库**。谁持久化这些数字由调用方决定。

接入 HF 词表需要引入 `github.com/sugarme/tokenizer` 并按族附带词表
文件（每份几 MB）。做之前先问：这一族的流量占比是否值得多几十 MB 的
二进制与一份要跟着上游更新的文件。
