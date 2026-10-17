# 契约测试

用**官方客户端 SDK** 当客户端，验我们对外响应的**形状**。

## 它补的是哪个缺口

仓库里的端到端用例用的是自己写的假上游与自己构造的请求，证明了「我们的请求
发得出去、我们的响应我们自己能解析」。它**没有**证明「真实的客户端 SDK 能解析
我们的响应」。

形状上最容易错的那几处，恰好是自测看不出来的：

- 必填字段漏了；可空字段写成了 `null` 还是省略（SDK 的解析器对这两者反应不同）
- `finish_reason` / `stop_reason` 的**取值集合**（写了一个 SDK 不认的值）
- 流式事件的**顺序与终止条件**（少了 `[DONE]`、`message_stop` 发晚了、
  最后一个 chunk 里 usage 的位置不对）
- 错误响应的形状（HTTP 状态码对了，但 body 的 `error.type` 不是 SDK 认的那些）

这几种错，自测全绿，而客户端一接就崩。

## 覆盖范围

三套协议（`openai` / `anthropic` / `openai-responses`）各跑七条；Responses
另多一条**空 `input`** 的 400（共八条），因为它的 400 文案要点的是另一个字段：

| 用例 | 验什么 |
|---|---|
| 非流式 | 正文、`finish_reason` / `stop_reason` / `status`、`usage` 三项（Responses 另加两个 `*_tokens_details`） |
| 流式 | 增量能拼起来、事件顺序、**终止条件**、用量在流里的位置 |
| 流式·余额耗尽 | 服务端按余额切断一条流时客户端看到什么：OpenAI 与 Anthropic 抛异常、异常之前没有正常结束的迹象；Responses 收到一条 `error` 事件且没有终态事件（见下） |
| 工具调用 | `tool_calls` / `tool_use` / `function_call` 三边的形状（`arguments` 是字符串、Anthropic 的 `input` 是对象） |
| 客户端错误 400 | SDK 抛 `BadRequestError`，`error.type` 是 `invalid_request_error`；Responses 另有「缺 `max_output_tokens`」与「空 `input`」两条，文案分别点名这两个字段 |
| 认证错误 401 | SDK 抛 `AuthenticationError`；OpenAI 与 Responses 的 `error.code` 是 `invalid_api_key`，Anthropic 的 `error.type` 是 `authentication_error` |
| 上游错误 5xx | 上游回 500 时我们回 502，SDK 抛 `APIStatusError`，`error.type` 是 `api_error` |

「流式·余额耗尽」那一条需要一个**真的**余额不足：假上游没法让服务端走进那条路，
所以它配了单独一份价目（几个阈值段就把余额花光）与一条很长的流
（见 `contract_test.go` 的 `contractDrainRule` 与 `contractDrainChunks`）。
流短了这条用例会红在一个与它无关的原因上——上游先没词了。

### 流内错误帧：三套协议在客户端那一边长得不一样

响应头在第一个事件之前就发出去了，所以流开始之后的失败**只能**作为帧表达
（见 `internal/api/stream.go`）。这条用例验的是「客户端认不认那条帧」，
而三套协议的答案不同：

| 入口 | 客户端看到什么 | 判据 |
|---|---|---|
| openai | 抛 `openai.APIError`，`error.message` 可读 | SDK 认的是 data 里有没有 `error` 字段 |
| anthropic | 抛 `anthropic.APIStatusError`（状态码仍是 200） | SDK 认的是**事件名** `event: error` |
| openai-responses | **不抛**，交给调用方一个 `ResponseErrorEvent`（`code` / `message` 可读） | 官方 Responses 协议的流内失败就是这样一个**平铺的事件**，SDK 把它当事件模型解析 |

两条抛异常的用例钉的不是同一处渲染代码：两套协议的帧由 `streamErrorFrame`
的两个分支分别写出来，一个分支写坏了只有对应的那一条会红。

**Responses 那一条的第三组断言只在它这里有意义。** 「没有伪造正常结束标记」
在 OpenAI / Anthropic 那两条上验不出来——SDK 在异常处就停止迭代了，错误帧
**之后**还有什么帧它一概看不到。所以那两条只断言「异常之前没出现过
`finish_reason` / `message_stop`」，而「整条帧序列里没有 `[DONE]`」由 Go 那侧
的帧序列用例钉着（`internal/assembly` 的流式冻结用例与 `internal/api` 的
渲染器用例）。Responses 那一条不一样：SDK 不抛异常、会一路读完，所以
「没有 `response.completed`」这条断言真的在它那里生效——把终态事件写到错误
事件之后，它会红（实测）。

401 那条是**唯一**能验 `error.code` 的地方：OpenAI 那套形状里只有它会把
`error.code` 填上一个具体值（`invalid_api_key`），普通的 400 给的是 `null`
（见 `internal/api/error.go` 的 `errorKind`）。

`openai-responses` 与 `openai` 用**同一个** Python 客户端（`openai` 这个包），
差别只在调 `client.responses.create` 还是 `client.chat.completions.create`。
它的非流式错误体与 OpenAI 那一套**逐字相同**（两家共用同一个官方 SDK 的错误
解析器，见 `internal/api/error.go` 的说明），所以 400 / 401 / 5xx 三条的断言
也相同。

`openai-responses` 的非流式那一条不是「照抄一遍」：它断言的
`usage.input_tokens_details.cached_tokens` 与
`usage.output_tokens_details.reasoning_tokens` 是**官方 SDK 的必填字段**，
而 relaykit 的 Chat→Responses 转换器填不出来（推理量被写进 chat 名字的
`completion_tokens_details`，缓存写入量根本不写）。照它序列化，客户端读到的
是一个不存在的属性——实测把这两个字段去掉，SDK 直接抛
`AttributeError: 'NoneType' object has no attribute 'reasoning_tokens'`。
所以那条用例是**这个入口存在的形状理由**的判据，不是重复覆盖。

**一律打假上游。** 假上游由 `internal/assembly/contract_test.go` 起，按**请求里的
模型名**决定回什么，所以三种场景（正文 / 工具调用 / 失败）共用一个服务。
它按渠道协议说三种不同的话（OpenAI Chat / Anthropic Messages / OpenAI
Responses），所以同一个模型名在三条入口下回的响应体形状不同。

## 断言怎么写

一律走 SDK 的对象模型：

```python
resp.choices[0].finish_reason
chunk.choices[0].delta.content
resp.usage.prompt_tokens
event.delta.stop_reason
```

**不要用 `resp.model_dump()` 之类绕过解析器。** 绕过去就测不到「SDK 认不认这个
形状」——`model_dump()` 只是把服务端发来的 JSON 原样搬一遍，它永远成功，
所以它永远证明不了任何事。这一条是这份测试的全部意义所在。

## 怎么跑

### 本地 / CI

契约测试由 `internal/assembly/contract_test.go` 驱动，它带 `//go:build contract`：

```sh
pip install -r contract/requirements.txt

export LARKAX_DB_DSN="postgres://larkax:placeholder@127.0.0.1:5432/larkax?sslmode=disable"
export LARKAX_ROLE=worker

go test -tags=contract -count=1 -v ./internal/assembly/ -run Contract
```

**为什么要 build tag 而不是 `t.Skip`**：这一条依赖 Python 与两个 SDK，而
`go test ./...` 必须能在没有 Python 的环境里跑完。用 `t.Skip` 挡的话，「跳过」
在输出里与「通过」长得一样，退出码也都是 0（见 [LESSONS.md](../LESSONS.md) 第 16 条）。
build tag 让它在默认构建里**根本不存在**——不存在的东西不会被误读成通过。

于是反过来：**Python 不在时这里必须是失败而不是跳过。** 这一条只在
`-tags=contract` 下编译，而那个 CI job 一定会装 Python 与依赖；找不到解释器
说明 job 配错了，不是「环境缺依赖」。所以那几处检查一律 `t.Fatal`。

`go vet` **默认不看 build tag 后面的文件**，所以要单独带 tag 跑一遍：

```sh
go vet -tags=contract ./internal/assembly/
```

### 单独跑 check.py

`check.py` 也可以脱离 Go 单独跑——调试形状时比每次都起一遍服务快：

```sh
python3 contract/check.py \
    --base-url http://127.0.0.1:8080 \
    --api-key <一把可用的 API Key 明文> \
    --protocol openai \
    --model-prefix <假上游认得的那三个模型名的前缀>
```

它要求上游已经能按 `<前缀>-text` / `<前缀>-tools` / `<前缀>-fail` 三个模型名
回出对应的响应；脱离 `contract_test.go` 跑时，那份上游要自己准备。

## 怎么加一条用例

1. 在 `check.py` 里写一个 `def xxx_case(ctx: Ctx) -> str:`——返回一句话摘要，
   失败就抛 `CaseFailure`（用 `check()` / `check_true()` 抛）。
2. 把它加进 `OPENAI_CASES` / `ANTHROPIC_CASES` / `RESPONSES_CASES`
   （三张表由 `CASES_BY_PROTOCOL` 按 `--protocol` 选中）。
3. 如果它需要假上游换一种响应，在 `internal/assembly/contract_test.go` 的
   `contractUpstream` 上加一个模型名后缀与对应的分支。
4. 本地跑一遍 `go test -tags=contract ...`，再跑一遍「把它改坏会红」——
   见下面「怎么确认这条测试真的在工作」。

### 怎么确认这条测试真的在工作

一条永远不可能失败的断言与一条真的在工作的断言，在输出里长得一样
（见 [LESSONS.md](../LESSONS.md) 第 16、23 条）。所以新加断言时顺手做两件事：

- **把服务端那条改坏**（在本地工作树里改，改完还原），用例必须红。
- **把响应里某个必填字段去掉**，SDK 必须报错。如果它不报错，说明那条断言
  绕过了解析器，或者 SDK 对这个字段不敏感——两种情况都要在 PR 里说清楚。

## 已知偏差

**跨协议流式的 `message_start.message.usage.input_tokens` 仍是估算值。**
Anthropic 客户端配 OpenAI 上游时，那一帧给的是本地估算，不是上游报的真值。

原因是时序：OpenAI 上游的用量在**结束分片**里，而 `message_start` 是流的
第一帧，不能等到那里。所以渲染器在**第一个事件到达时**写这一帧——事件带用量
就用真值，不带就用请求进来时按分词估出来的那个（见 `internal/api` 的
`inputTokensOf`）。Anthropic 上游的第一帧就是 `message_start` 且带
`usage.input_tokens`，所以那条路拿到的是真值；OpenAI 上游的第一个事件是正文帧，
拿到的是估算。

无论哪条路，`message_delta.usage.input_tokens` 都是真值。所以**以最后一个
usage 为准**的客户端读到的是对的；**只读 `message_start` 的客户端**在跨协议
那条路上会读到偏低的数（成本统计、用量面板是常见的那类）。

契约测试那条 Anthropic 流式用例**断言 `message_start` 的 `input_tokens` 等于
上游报的值**——它曾经只断言「是个正数」，而那个宽断言让上面这条偏差长期看不见
（实测：上游报 12，那一帧给 3，用例照样绿）。跨协议那条路的估算由
`internal/assembly/stream_e2e_test.go` 钉着。

**OpenAI 流式的 `[DONE]` 从 SDK 这一侧看不到。** 它由 SDK 的 SSE 解码器吃掉，
不会作为分片交给调用方。能看到的正向证据是「迭代正常结束」加上「最后一个带
`choices` 的分片给出了 `finish_reason`」。`[DONE]` 有没有真的写出去由 Go 那侧的
流式用例钉着（`internal/assembly/stream_e2e_test.go`）。

**我们总是给流式响应带上 usage。** 真实 OpenAI 只在调用方传了
`stream_options: {"include_usage": true}` 时才给。多给是超集，SDK 容忍它。

### Responses 入口上的四条

这四条都是**有意的**，不是没做完；每一条都有一条用例或一处判据钉着。

**`max_output_tokens` 在本项目是必填的。** 官方 API 里它可选，这里不行——
预留要有输出上界才做得出来（见 `internal/api` 的 `httpStatusOfUpstream` 与
`billing.ErrNoUpperBound`）。不给它回 400，`error.message` 里点名 `max_tokens`。
契约测试那条「客户端错误 400」用的就是这个触发方式。

另一条 400 是**空 `input`**：判据（转换之后一条消息都没有）三条入口共用，
但文案必须点名调用方自己那套协议里的字段。Responses 的调用方发的是顶层
`input`，收到「messages is required」会去改一个它没写过的字段。所以
Responses 那一条断言 `message` 里出现 `input`、且**不出现** `messages`。
`input=[]` 会被 SDK 原样发出去（它不拦空列表），这条因此仍走 SDK 的对象模型。

**有状态的字段被拒（400），不是被丢弃。** `previous_response_id` /
`conversation` / `context_management` 会让输入里含**上游持有的历史**，
而本项目的输入量是**本地估**出来的（见 `internal/tokens`）。低估的后果是预留
不足、结算超支，所以宁可让请求失败。这一条由 `internal/relay` 的用例钉着。

**`store` 被丢弃，且不报错。** 它只表达「上游可以留存这次请求」，既不影响我们
这边的输入量，也不影响上游这次要算什么。丢弃的代价是调用方以为留了痕而实际
没有——而它真正会用到的续话手段是 `previous_response_id`，那一条已经被拒。

**非 function 的工具声明被拒（400）。** 远端 MCP（`type: mcp`）与内置工具
（`web_search` / `file_search` 等）在本项目送不出去：`channel.Tool` 只有名字、
描述与 schema，没有承载类型的字段。**不拒的表现是静默消失**——relaykit 的转换
先经 toolconv 抽出工具、再按 chat 格式挂回去，表达不了的类型直接丢掉，请求
照常成功（实测：发一个 `type: mcp` 的请求得到 200，工具不见了）。所以判据放在
入口解析层，那里还看得见请求体里原始的 `type`（见 `internal/relay` 的
`checkResponsesTools`）。它与另外两条入口共用 `checkToolType`，只差一档：
**Responses 这边缺 `type` 也要拒**，因为缺了它整条声明同样会被丢掉。

## 版本

`requirements.txt` 里两个 SDK 都是**精确钉住**的（`==`）。理由与 `gremlins` /
`deadcode` 那两次一样：不钉版本的工具，它的结论不可比。钉的是哪个版本、
为什么是它，写在 `requirements.txt` 的注释里。
