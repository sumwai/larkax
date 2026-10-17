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

两套协议（`openai` / `anthropic`）各跑六条：

| 用例 | 验什么 |
|---|---|
| 非流式 | 正文、`finish_reason` / `stop_reason`、`usage` 三项 |
| 流式 | 增量能拼起来、事件顺序、**终止条件**、用量在流里的位置 |
| 工具调用 | `tool_calls` / `tool_use` 两边的形状（`arguments` 是字符串、`input` 是对象） |
| 客户端错误 400 | SDK 抛 `BadRequestError`，`error.type` 是 `invalid_request_error` |
| 认证错误 401 | SDK 抛 `AuthenticationError`；OpenAI 的 `error.code` 是 `invalid_api_key`，Anthropic 的 `error.type` 是 `authentication_error` |
| 上游错误 5xx | 上游回 500 时我们回 502，SDK 抛 `APIStatusError`，`error.type` 是 `api_error` |

401 那条是**唯一**能验 `error.code` 的地方：OpenAI 那套形状里只有它会把
`error.code` 填上一个具体值（`invalid_api_key`），普通的 400 给的是 `null`
（见 `internal/api/error.go` 的 `errorKind`）。

**一律打假上游。** 假上游由 `internal/assembly/contract_test.go` 起，按**请求里的
模型名**决定回什么，所以三种场景（正文 / 工具调用 / 失败）共用一个服务。

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
2. 把它加进 `OPENAI_CASES` 或 `ANTHROPIC_CASES`。
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

**Anthropic 流式的 `message_start.message.usage.input_tokens` 是估算值，
不是上游报的真值。** 实测（假上游报 12）：那一帧给的是 `3`。

原因是时序：这一帧必须在我们读到上游的用量**之前**写出去——上游的用量在它
自己的 `message_stop` 才攒齐（见 `internal/channel/anthropicadapter` 的
`emitUsage`），而 `message_start` 是流的第一帧，不能等。Anthropic 的 SDK 把
`message_start.message.usage.input_tokens` 标成必填，所以那里必须放一个数，
放的就是请求进来时按分词估出来的那个（见 `internal/api` 的 `inputTokensOf`）。

真值在 `message_delta.usage.input_tokens` 里，契约测试断言的就是它。所以
**以最后一个 usage 为准**的客户端读到的是对的；**只读 `message_start` 的
客户端会读到错数**（成本统计、用量面板是常见的那类）。

契约测试因此对 `message_start` 只断言「是个正数」，对 `message_delta` 断言
真值——这不是放宽断言，是把「我们实际保证的是什么」写清楚。修掉这条偏差要
改的是第一帧的写出时机，不在契约测试这一层。

**OpenAI 流式的 `[DONE]` 从 SDK 这一侧看不到。** 它由 SDK 的 SSE 解码器吃掉，
不会作为分片交给调用方。能看到的正向证据是「迭代正常结束」加上「最后一个带
`choices` 的分片给出了 `finish_reason`」。`[DONE]` 有没有真的写出去由 Go 那侧的
流式用例钉着（`internal/assembly/stream_e2e_test.go`）。

**我们总是给流式响应带上 usage。** 真实 OpenAI 只在调用方传了
`stream_options: {"include_usage": true}` 时才给。多给是超集，SDK 容忍它。

## 版本

`requirements.txt` 里两个 SDK 都是**精确钉住**的（`==`）。理由与 `gremlins` /
`deadcode` 那两次一样：不钉版本的工具，它的结论不可比。钉的是哪个版本、
为什么是它，写在 `requirements.txt` 的注释里。
