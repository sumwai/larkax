#!/usr/bin/env python3
"""larkax 契约测试：用官方 SDK 当客户端，验我们对外响应的**形状**。

# 它验什么，不验什么

仓库里的端到端用例用的是自己写的假上游与自己构造的请求，证明了「我们的响应
我们自己能解析」。它**没有**证明「真实的客户端 SDK 能解析我们的响应」——
而形状上最容易错的那几处（必填字段漏了、可空字段写成 null 还是省略、
finish_reason 的取值集合、流式事件的顺序与终止条件、用量在流里的位置、
错误体的 error.type）恰好是自测看不出来的。那几种错，自测全绿，客户端一接就崩。

它**不**验上游的兼容性（那是各渠道适配器自己的事），也不做负载测试。

# 断言怎么写

一律走 SDK 的对象模型：`resp.choices[0].finish_reason`、
`chunk.choices[0].delta.content`、`resp.usage.prompt_tokens`……

**不要用 `model_dump()` 之类绕过解析器。** 绕过去就测不到「SDK 认不认这个形状」，
而那正是这条测试的全部意义——`model_dump()` 只是把服务端发来的 JSON 原样搬一遍，
它永远成功，所以它永远证明不了任何事。

# 怎么跑

见同目录的 README.md。退出码：全部通过 0，有任何一条失败 1。
"""

from __future__ import annotations

import argparse
import json
import sys
import traceback

# ── 与 Go 那一侧约定好的取值 ──────────────────────────────────────────
#
# 假上游由 internal/assembly/contract_test.go 起，它按**请求里的模型名**决定
# 回什么。模型名由装配侧生成一个前缀，两边按同一套后缀推出来，于是这里不必
# 把三个名字当成参数传进来，也就不会出现「传错了但两边都自洽」的情形。

TEXT_SUFFIX = "text"
TOOLS_SUFFIX = "tools"
FAIL_SUFFIX = "fail"
# 余额在流的中间耗尽那条路。它配的价目让几个阈值段就把余额花光，
# 而假上游备了一条很长的流——切断因此是**余额**造成的（见 contract_test.go
# 的 contractDrainRule 与 contractDrainChunks）。
DRAIN_SUFFIX = "drain"

# 假上游回的正文。分成两片，这样流式用例能验「增量要拼起来」。
TEXT_PARTS = ("契约测试", "的正文")
TEXT = "".join(TEXT_PARTS)

# 假上游报的用量。
PROMPT_TOKENS = 12
COMPLETION_TOKENS = 7
TOTAL_TOKENS = 19

# 输入侧缓存量与输出侧推理量。**故意非零**：它们是 Responses 那两个 details
# 子对象里唯一会被断言的数，而 0 恰好是「字段在不在」都一样的取值——
# 缺了整个对象与报了 0 在读出来之后分不出来（见 responses_nonstream）。
CACHED_TOKENS = 5
REASONING_TOKENS = 2

# 假上游报的工具调用。
TOOL_ID = "call_contract"
TOOL_NAME = "get_weather"
TOOL_INPUT = {"city": "Paris"}

# 假上游在响应体里回报的模型名，与调用方要的**不是**同一个（真实上游也常常
# 这样，见 contract_test.go 的 upstreamModelName）。我们对外透出的是
# **调用方要的那个规范名**，不是上游回报的名字——所以断言写成「等于调用方
# 要的名字」，而假上游刻意报一个不同的值：这样「透出的是哪一个」才是被测的，
# 而不是两边恰好相同、怎么写都通过。

# 认证用例用的一把假 Key。它不需要存在于库里——鉴权那一步就会拒掉它，
# 请求走不到上游，而这条用例要验的正是「401 的错误体形状」。
BAD_API_KEY = "sk-larkax-contract-invalid-placeholder"


class CaseFailure(Exception):
    """一条用例没通过。消息里写清「哪个字段、拿到什么、期望什么」。"""


def check(label: str, got, want) -> None:
    if got != want:
        raise CaseFailure(f"{label}：得到 {got!r}，期望 {want!r}")


def check_true(label: str, cond: bool) -> None:
    if not cond:
        raise CaseFailure(label)


def error_type_of(exc: BaseException):
    """取 SDK 认出来的 `error.type`。

    两家 SDK 的取法不一样，把差异收在这一处，别在每条断言里各写一遍：

    - openai 的 `_make_status_error` 会先把响应体里的 `error` 那一层拆出来
      再交给异常，所以 `exc.type` 就是 `error.type`。
    - anthropic 把整个响应体原样交给异常，`exc.type` 拿到的是**顶层**的
      `type`（恒为 `"error"`），不是 `error.type`。

    所以先试属性，取不到再退回 SDK 解析出来的 body。两边读到的都是
    「SDK 解析之后」的值，不是我们自己去 `json.loads` 原始字节。
    """
    value = getattr(exc, "type", None)
    if isinstance(value, str) and value and value != "error":
        return value
    body = getattr(exc, "body", None)
    if isinstance(body, dict):
        inner = body.get("error")
        if isinstance(inner, dict):
            return inner.get("type")
    return None


def error_code_of(exc: BaseException):
    """取 SDK 认出来的 `error.code`（只有 OpenAI 那套形状里有）。"""
    value = getattr(exc, "code", None)
    if isinstance(value, str) and value:
        return value
    body = getattr(exc, "body", None)
    if isinstance(body, dict):
        inner = body.get("error")
        if isinstance(inner, dict):
            return inner.get("code")
    return None


def body_error_message(exc: BaseException) -> str:
    """取错误体里的 message，用于断言措辞里确实提到了出问题的那个字段。

    两家 SDK 的取法不一样，与 error_type_of 同一个原因：

    - openai 的异常 `body` 已经拆掉了 `error` 那一层，拿到的就是内层对象。
    - anthropic 的 `body` 是整个响应体，message 在 `error.message` 里。

    所以两种形状都试：先看 body 自己有没有 message，再看嵌套的那一层。
    只按嵌套结构取的话，openai 那边会一路退回到 `str(exc)`——断言仍然
    「碰巧」能过（异常文本里带着整段 JSON），但它读的不是 SDK 解析出来的
    那个字段，换一版 SDK 就可能静默失效（见 LESSONS.md 第 16 条）。
    """
    body = getattr(exc, "body", None)
    if isinstance(body, dict):
        if isinstance(body.get("message"), str):
            return body["message"]
        inner = body.get("error")
        if isinstance(inner, dict) and isinstance(inner.get("message"), str):
            return inner["message"]
    return str(exc)


# ── 上下文 ────────────────────────────────────────────────────────────


class Ctx:
    def __init__(self, client, bad_client, prefix: str, protocol: str) -> None:
        self.client = client
        # 用一把假 Key 的同一个客户端，专门给 401 那条用例用。
        self.bad_client = bad_client
        self.prefix = prefix
        self.protocol = protocol

    def model(self, suffix: str) -> str:
        return f"{self.prefix}-{suffix}"


# ── OpenAI 协议 ───────────────────────────────────────────────────────


def openai_nonstream(ctx: Ctx) -> str:
    """非流式：正文、结束原因、用量都要能被 SDK 的对象模型读出来。"""
    resp = ctx.client.chat.completions.create(
        model=ctx.model(TEXT_SUFFIX),
        messages=[{"role": "user", "content": "你好"}],
        max_tokens=64,
    )
    check("object", resp.object, "chat.completion")
    check("model", resp.model, ctx.model(TEXT_SUFFIX))
    check("choices 条数", len(resp.choices), 1)

    choice = resp.choices[0]
    check("choices[0].index", choice.index, 0)
    check("choices[0].finish_reason", choice.finish_reason, "stop")
    check("choices[0].message.role", choice.message.role, "assistant")
    check("choices[0].message.content", choice.message.content, TEXT)
    # 没有工具调用时 tool_calls 应当是**缺省**（我们用了 omitempty），
    # 而不是空数组——两者在 SDK 里都是 None，这里断言的是 SDK 读到的结果。
    check("choices[0].message.tool_calls", choice.message.tool_calls, None)

    check_true("usage 不是 None", resp.usage is not None)
    check("usage.prompt_tokens", resp.usage.prompt_tokens, PROMPT_TOKENS)
    check("usage.completion_tokens", resp.usage.completion_tokens, COMPLETION_TOKENS)
    check("usage.total_tokens", resp.usage.total_tokens, TOTAL_TOKENS)

    return f"finish_reason={choice.finish_reason} usage={resp.usage.prompt_tokens}/{resp.usage.completion_tokens}"


def openai_stream(ctx: Ctx) -> str:
    """流式：增量要拼得起来，结束原因与用量都要读得到。

    终止条件：OpenAI 的 `[DONE]` 由 SDK 的 SSE 解码器**吃掉**，不会作为
    分片交给调用方，所以从 SDK 这一侧看不到它本身。能看到的正向证据是
    「迭代正常结束」加上「最后一个带 choices 的分片给出了 finish_reason」。
    `[DONE]` 有没有写出去由 Go 那侧的流式用例钉着（见 README 的说明）。
    """
    stream = ctx.client.chat.completions.create(
        model=ctx.model(TEXT_SUFFIX),
        messages=[{"role": "user", "content": "你好"}],
        max_tokens=64,
        stream=True,
    )

    text = ""
    reasons = []
    usage = None
    chunks = 0
    for chunk in stream:
        chunks += 1
        check("chunk.object", chunk.object, "chat.completion.chunk")
        check("chunk.model", chunk.model, ctx.model(TEXT_SUFFIX))
        if chunk.usage is not None:
            usage = chunk.usage
        for choice in chunk.choices:
            if choice.delta.content:
                text += choice.delta.content
            if choice.finish_reason:
                reasons.append(choice.finish_reason)

    check("拼起来的正文", text, TEXT)
    check("出现的 finish_reason", reasons, ["stop"])
    check_true(f"流式响应里读不到 usage（收到 {chunks} 个分片）", usage is not None)
    check("usage.prompt_tokens", usage.prompt_tokens, PROMPT_TOKENS)
    check("usage.completion_tokens", usage.completion_tokens, COMPLETION_TOKENS)
    check("usage.total_tokens", usage.total_tokens, TOTAL_TOKENS)

    return f"{chunks} 个分片，finish_reason={reasons}，usage 在流里读到"


def openai_balance_exhausted(ctx: Ctx) -> str:
    """余额在流的中间耗尽时，官方 SDK 要把流内那条错误帧抛成异常。

    三件事一起断言，缺一不可：

    1. **异常真的被抛出来了**。这一条同时覆盖「有没有伪造结束标记」——
       openai 的 SDK 见到 `[DONE]` 就 break，伪造一个会让它安静地正常迭代完，
       于是调用方把半截回答当成完整回答；
    2. 抛的是 SDK 自己解析出来的那个错误对象，`error.message` 是我们写的那句
       （不是从 `str(exc)` 里碰巧带上的 JSON 文本里读出来的）；
    3. 异常之前已经收到了正文、而且**没有**出现过任何正常结束的迹象
       （没有 `finish_reason`）——失败发生在流的中间，不是刚开始。

    它测的是**返回路径之外**的一件事：状态码早在第一个事件之前就写出去了
    （见 internal/api/stream.go 的说明），所以流中途的失败只能从帧序列里
    看出来。客户端能不能看出来，只有真的 SDK 说了算。
    """
    import openai

    stream = ctx.client.chat.completions.create(
        model=ctx.model(DRAIN_SUFFIX),
        messages=[{"role": "user", "content": "你好"}],
        max_tokens=100000,
        stream=True,
    )

    text = ""
    reasons = []
    chunks = 0
    try:
        for chunk in stream:
            chunks += 1
            for choice in chunk.choices:
                if choice.delta.content:
                    text += choice.delta.content
                if choice.finish_reason:
                    reasons.append(choice.finish_reason)
    except openai.APIError as exc:
        check_true("异常之前收到过正文（失败发生在流的中间）", text != "")
        check("异常之前出现过的 finish_reason", reasons, [])
        check_true("error.message 提到余额", "balance" in body_error_message(exc))
        return (f"{type(exc).__name__}：{body_error_message(exc)}"
                f"（此前已收到 {chunks} 个分片、{len(text)} 个字符）")
    except Exception as exc:
        # 抛出来的东西不对也算失败，但要说清是「类型不对」而不是「没抛」：
        # 两者的原因不同（前者是那条帧的形状让 SDK 认不出来，后者是压根没写）。
        raise CaseFailure(
            f"余额耗尽的流抛的是 {type(exc).__name__}，不是 openai.APIError：{exc}"
        ) from exc

    raise CaseFailure(
        f"余额耗尽的流没有抛异常：SDK 正常迭代完了（收到 {chunks} 个分片、"
        f"{len(reasons)} 个 finish_reason）"
    )


def openai_tool_call(ctx: Ctx) -> str:
    """含工具调用的往返：SDK 要能把 arguments 当成 JSON 字符串解出来。"""
    resp = ctx.client.chat.completions.create(
        model=ctx.model(TOOLS_SUFFIX),
        messages=[{"role": "user", "content": "巴黎天气如何"}],
        max_tokens=64,
        tools=[
            {
                "type": "function",
                "function": {
                    "name": TOOL_NAME,
                    "description": "查一个城市的天气",
                    "parameters": {
                        "type": "object",
                        "properties": {"city": {"type": "string"}},
                        "required": ["city"],
                    },
                },
            }
        ],
    )

    choice = resp.choices[0]
    check("choices[0].finish_reason", choice.finish_reason, "tool_calls")
    # 纯工具调用时 OpenAI 给的是 null 而不是空串，两者在客户端里会被区别对待。
    check("choices[0].message.content", choice.message.content, None)

    calls = choice.message.tool_calls
    check_true("tool_calls 不是 None", calls is not None)
    check("tool_calls 条数", len(calls), 1)

    call = calls[0]
    check("tool_calls[0].id", call.id, TOOL_ID)
    check("tool_calls[0].type", call.type, "function")
    check("tool_calls[0].function.name", call.function.name, TOOL_NAME)
    # arguments 是**字符串形态**的 JSON，不是对象。SDK 不做这一步解析，
    # 所以这里解一遍——解不开就说明我们发的不是合法 JSON 字符串。
    check("tool_calls[0].function.arguments 解出来的对象",
          json.loads(call.function.arguments), TOOL_INPUT)

    return f"tool={call.function.name} args={call.function.arguments}"


def openai_bad_request(ctx: Ctx) -> str:
    """客户端错误：400 要落到 SDK 的 BadRequestError 上，type 要认得出。

    触发方式是 messages 为空——那是**请求本身的缺陷**，与上游无关，
    服务端会在调上游之前就拒掉（见 internal/api/request.go 的 checkRequest）。
    """
    import openai

    try:
        ctx.client.chat.completions.create(
            model=ctx.model(TEXT_SUFFIX),
            messages=[],
            max_tokens=64,
        )
    except openai.BadRequestError as exc:
        check("status_code", exc.status_code, 400)
        check("error.type", error_type_of(exc), "invalid_request_error")
        check_true("message 提到 messages",
                   "messages" in body_error_message(exc))
        return f"BadRequestError type={error_type_of(exc)}"
    raise CaseFailure("messages 为空的请求没有报错——服务端本该回 400")


def openai_upstream_error(ctx: Ctx) -> str:
    """上游错误：假上游回 500，我们对外回 502，SDK 要能当成 5xx 认出来。

    502 在 OpenAI 那套里没有专门的名字，SDK 把它归到 InternalServerError
    （`APIStatusError` 的子类）——所以断言落在**基类 + 状态码**上，
    而不是某个具体的异常名：名字是 SDK 的内部选择，状态码才是契约。

    `error.code` 是上游这一档的名字（`upstream_http_error`，与账单、快照同一套
    词汇，见 internal/api 的 httpStatusOfUpstream）：本地判定的失败不落这一档，
    它们的码是另两个。
    """
    import openai

    try:
        ctx.client.chat.completions.create(
            model=ctx.model(FAIL_SUFFIX),
            messages=[{"role": "user", "content": "你好"}],
            max_tokens=64,
        )
    except openai.APIStatusError as exc:
        check("status_code", exc.status_code, 502)
        check("error.type", error_type_of(exc), "api_error")
        check("error.code", error_code_of(exc), "upstream_http_error")
        return f"{type(exc).__name__} status={exc.status_code} type={error_type_of(exc)}"
    raise CaseFailure("上游 500 时我们本该回 502，但请求没有报错")


def openai_auth_error(ctx: Ctx) -> str:
    """认证错误：401 要落到 AuthenticationError 上，`error.code` 要认得出。

    401 在 errorKind 里映射成 `invalid_api_key`，普通的 400 给的是 null。所以
    「error.code 对不对」能在这里验一次；上游那一档的码在
    `openai_upstream_error` 里另验一条（幂等那三条 409 不进契约测试，它们要
    先造一次冲突请求，由 `internal/assembly/idempotency_e2e_test.go` 钉着）。
    """
    import openai

    try:
        ctx.bad_client.chat.completions.create(
            model=ctx.model(TEXT_SUFFIX),
            messages=[{"role": "user", "content": "你好"}],
            max_tokens=64,
        )
    except openai.AuthenticationError as exc:
        check("status_code", exc.status_code, 401)
        # OpenAI 把凭证问题归在 invalid_request_error 下，靠 code 区分。
        check("error.type", error_type_of(exc), "invalid_request_error")
        check("error.code", error_code_of(exc), "invalid_api_key")
        return f"AuthenticationError type={error_type_of(exc)} code={error_code_of(exc)}"
    raise CaseFailure("凭证无效时本该回 401，但请求没有报错")


OPENAI_CASES = [
    ("非流式", openai_nonstream),
    ("流式", openai_stream),
    ("流式·余额耗尽", openai_balance_exhausted),
    ("工具调用", openai_tool_call),
    ("客户端错误 400", openai_bad_request),
    ("认证错误 401", openai_auth_error),
    ("上游错误 5xx", openai_upstream_error),
]


# ── Anthropic 协议 ────────────────────────────────────────────────────


def anthropic_nonstream(ctx: Ctx) -> str:
    resp = ctx.client.messages.create(
        model=ctx.model(TEXT_SUFFIX),
        max_tokens=64,
        messages=[{"role": "user", "content": "你好"}],
    )
    check("type", resp.type, "message")
    check("role", resp.role, "assistant")
    check("model", resp.model, ctx.model(TEXT_SUFFIX))
    check("stop_reason", resp.stop_reason, "end_turn")
    # 我们不用自定义停止序列，但这个字段在 Anthropic 的响应里始终存在，
    # 缺了它有些客户端会当成字段丢失。所以断言它是 null，而不是缺省。
    check("stop_sequence", resp.stop_sequence, None)

    check("content 块数", len(resp.content), 1)
    block = resp.content[0]
    check("content[0].type", block.type, "text")
    check("content[0].text", block.text, TEXT)

    check("usage.input_tokens", resp.usage.input_tokens, PROMPT_TOKENS)
    check("usage.output_tokens", resp.usage.output_tokens, COMPLETION_TOKENS)

    return f"stop_reason={resp.stop_reason} usage={resp.usage.input_tokens}/{resp.usage.output_tokens}"


def anthropic_stream(ctx: Ctx) -> str:
    """流式：事件顺序、终止条件（message_stop）、用量跨两个事件。

    Anthropic 的用量是**跨事件**的：输入在 message_start 里，输出在
    message_delta 里。只在一个事件里给会让「实测用量」那条路走不到，
    所以两个都要断言。
    """
    stream = ctx.client.messages.create(
        model=ctx.model(TEXT_SUFFIX),
        max_tokens=64,
        messages=[{"role": "user", "content": "你好"}],
        stream=True,
    )

    seen = []
    text = ""
    start_input_tokens = None
    delta_input_tokens = None
    output_tokens = None
    stop_reason = None

    for event in stream:
        seen.append(event.type)
        if event.type == "message_start":
            check("message_start.message.type", event.message.type, "message")
            check("message_start.message.role", event.message.role, "assistant")
            start_input_tokens = event.message.usage.input_tokens
        elif event.type == "content_block_delta":
            check("content_block_delta.delta.type", event.delta.type, "text_delta")
            text += event.delta.text
        elif event.type == "message_delta":
            stop_reason = event.delta.stop_reason
            delta_input_tokens = event.usage.input_tokens
            output_tokens = event.usage.output_tokens

    # 顺序是契约的一部分：正文要包在 content_block 的开与关之间，
    # message_delta 在 message_stop 之前。顺序反了，按事件渲染的界面会把
    # 「说一句话然后调用工具」显示成「调用工具然后说话」。
    check("事件顺序", seen, [
        "message_start",
        "content_block_start",
        "content_block_delta",
        "content_block_delta",
        "content_block_stop",
        "message_delta",
        "message_stop",
    ])
    check("拼起来的正文", text, TEXT)
    check("message_delta 里的 usage.input_tokens", delta_input_tokens, PROMPT_TOKENS)
    check("message_delta 里的 usage.output_tokens", output_tokens, COMPLETION_TOKENS)
    check("message_delta.delta.stop_reason", stop_reason, "end_turn")

    # 断言钉在**那个数**上，不是「是个正数」。
    #
    # 这里曾经写的是 `> 0`，注释把它说成「已知偏差，不是放宽断言」——那个宽
    # 断言对「估算 3」与「真值 12」一视同仁，于是这条偏差在契约测试里长期
    # 看不见（打印里露得出来，断言看不见）。**一个够宽的断言会把缺陷一直放过去**，
    # 这与「跳过」在输出里和「通过」长得一样是同一类问题（见 LESSONS 第 16、23 条）。
    #
    # Anthropic 上游的第一帧就是 message_start 且带 usage.input_tokens，渲染器
    # 在第一个事件到达时才写这一帧，所以这里拿到的就是上游报的值。跨协议那条路
    # （OpenAI 上游）仍是估算——它的用量在结束分片里，第一帧不能等；那条路由
    # internal/assembly 的流式 e2e 钉着，见 contract/README.md 的「已知偏差」。
    check("message_start 里的 usage.input_tokens", start_input_tokens, PROMPT_TOKENS)

    return (f"{len(seen)} 个事件，末事件={seen[-1]}；"
            f"input_tokens 首帧 {start_input_tokens} → message_delta {delta_input_tokens}")


def anthropic_balance_exhausted(ctx: Ctx) -> str:
    """同 OpenAI 那一条，但两套 SDK 认的是**不同**的东西。

    anthropic 的 SDK 不走「data 里有没有 `error` 字段」那条判据，它认的是
    **事件名** `event: error`。所以这两条用例钉的不是同一处渲染代码：
    两套协议的流内错误帧由两个分支分别写出来（见 internal/api 的
    streamErrorFrame），一个分支写坏了只有对应的那一条会红。

    断言与那一条同一组：抛异常了、抛的是 SDK 自己的错误类型、
    异常之前收到过正文且没有正常结束的迹象。
    """
    import anthropic

    stream = ctx.client.messages.create(
        model=ctx.model(DRAIN_SUFFIX),
        max_tokens=100000,
        messages=[{"role": "user", "content": "你好"}],
        stream=True,
    )

    text = ""
    events = 0
    stop_reasons = []
    saw_message_stop = False
    try:
        for event in stream:
            events += 1
            if event.type == "content_block_delta" and event.delta.type == "text_delta":
                text += event.delta.text
            if event.type == "message_delta" and event.delta.stop_reason:
                stop_reasons.append(event.delta.stop_reason)
            if event.type == "message_stop":
                saw_message_stop = True
    except anthropic.APIStatusError as exc:
        check_true("异常之前收到过正文（失败发生在流的中间）", text != "")
        check("异常之前出现过的 stop_reason", stop_reasons, [])
        check_true("异常之前没有 message_stop", not saw_message_stop)
        # 状态码仍是 200：失败只能从流里看出来（响应头在第一个事件之前就发出去了）。
        check("status_code", exc.status_code, 200)
        check("error.type", error_type_of(exc), "api_error")
        check_true("error.message 提到余额", "balance" in body_error_message(exc))
        return (f"{type(exc).__name__}：{body_error_message(exc)}"
                f"（此前已收到 {events} 个事件、{len(text)} 个字符）")
    except Exception as exc:
        raise CaseFailure(
            f"余额耗尽的流抛的是 {type(exc).__name__}，不是 anthropic.APIStatusError：{exc}"
        ) from exc

    raise CaseFailure(
        f"余额耗尽的流没有抛异常：SDK 正常迭代完了（收到 {events} 个事件、"
        f"{len(stop_reasons)} 个 stop_reason）"
    )


def anthropic_tool_call(ctx: Ctx) -> str:
    """含工具调用的往返：input 是**对象**（与 OpenAI 的字符串正好相反）。"""
    resp = ctx.client.messages.create(
        model=ctx.model(TOOLS_SUFFIX),
        max_tokens=64,
        messages=[{"role": "user", "content": "巴黎天气如何"}],
        tools=[
            {
                "name": TOOL_NAME,
                "description": "查一个城市的天气",
                "input_schema": {
                    "type": "object",
                    "properties": {"city": {"type": "string"}},
                    "required": ["city"],
                },
            }
        ],
    )

    check("stop_reason", resp.stop_reason, "tool_use")
    check("content 块数", len(resp.content), 1)

    block = resp.content[0]
    check("content[0].type", block.type, "tool_use")
    check("content[0].id", block.id, TOOL_ID)
    check("content[0].name", block.name, TOOL_NAME)
    # Anthropic 的 input 是对象；SDK 会把它解析成 dict。
    check("content[0].input", block.input, TOOL_INPUT)

    return f"tool={block.name} input={block.input}"


def anthropic_bad_request(ctx: Ctx) -> str:
    import anthropic

    try:
        ctx.client.messages.create(
            model=ctx.model(TEXT_SUFFIX),
            max_tokens=64,
            messages=[],
        )
    except anthropic.BadRequestError as exc:
        check("status_code", exc.status_code, 400)
        check("error.type", error_type_of(exc), "invalid_request_error")
        check_true("message 提到 messages",
                   "messages" in body_error_message(exc))
        return f"BadRequestError type={error_type_of(exc)}"
    raise CaseFailure("messages 为空的请求没有报错——服务端本该回 400")


def anthropic_upstream_error(ctx: Ctx) -> str:
    """上游错误：假上游回 500，我们对外回 502，SDK 要能当成 5xx 认出来。

    Anthropic 那套没有 `error.code`，我们给的码放在 `error.type` 上（见
    writeErrorCode），所以这一栏读到的是码本身 `upstream_http_error`：
    502 单独推出来的 `api_error` 只是没有码时的落点。
    """
    import anthropic

    try:
        ctx.client.messages.create(
            model=ctx.model(FAIL_SUFFIX),
            max_tokens=64,
            messages=[{"role": "user", "content": "你好"}],
        )
    except anthropic.APIStatusError as exc:
        check("status_code", exc.status_code, 502)
        check("error.type", error_type_of(exc), "upstream_http_error")
        return f"{type(exc).__name__} status={exc.status_code} type={error_type_of(exc)}"
    raise CaseFailure("上游 500 时我们本该回 502，但请求没有报错")


def anthropic_auth_error(ctx: Ctx) -> str:
    """认证错误：401 在 Anthropic 那套里有自己的类型名 `authentication_error`。

    与 OpenAI 的差别值得单独钉住：那边 401 归在 `invalid_request_error` 下，
    这边是独立的一个词。调用方按类型分支（「凭证错了就别重试」）时，
    两套读到的不是同一个字符串。
    """
    import anthropic

    try:
        ctx.bad_client.messages.create(
            model=ctx.model(TEXT_SUFFIX),
            max_tokens=64,
            messages=[{"role": "user", "content": "你好"}],
        )
    except anthropic.AuthenticationError as exc:
        check("status_code", exc.status_code, 401)
        check("error.type", error_type_of(exc), "authentication_error")
        return f"AuthenticationError type={error_type_of(exc)}"
    raise CaseFailure("凭证无效时本该回 401，但请求没有报错")


ANTHROPIC_CASES = [
    ("非流式", anthropic_nonstream),
    ("流式", anthropic_stream),
    ("流式·余额耗尽", anthropic_balance_exhausted),
    ("工具调用", anthropic_tool_call),
    ("客户端错误 400", anthropic_bad_request),
    ("认证错误 401", anthropic_auth_error),
    ("上游错误 5xx", anthropic_upstream_error),
]


# ── OpenAI Responses 协议 ─────────────────────────────────────────────


def responses_nonstream(ctx: Ctx) -> str:
    """非流式：正文在 output[] 的项里，用量带两个 details 子对象。

    这一条盯的正是**响应形状最容易被将就过去的地方**：`usage` 里那两个
    `*_tokens_details` 是官方 SDK 的必填字段，而 relaykit 那个
    Chat→Responses 转换器填不出来（推理量被写进 chat 名字的
    completion_tokens_details，缓存写入量根本不写）。照它序列化，这里读到的
    会是一个不存在的属性——`resp.usage.output_tokens_details` 直接抛
    AttributeError。所以这两条断言不是「多测一点」，它们就是这条用例存在的理由。
    """
    resp = ctx.client.responses.create(
        model=ctx.model(TEXT_SUFFIX),
        input="你好",
        max_output_tokens=64,
    )
    check("object", resp.object, "response")
    check("status", resp.status, "completed")
    check("model", resp.model, ctx.model(TEXT_SUFFIX))

    check("output 项数", len(resp.output), 1)
    item = resp.output[0]
    check("output[0].type", item.type, "message")
    check("output[0].role", item.role, "assistant")
    check("output[0].content 块数", len(item.content), 1)
    block = item.content[0]
    check("output[0].content[0].type", block.type, "output_text")
    check("output[0].content[0].text", block.text, TEXT)

    check_true("usage 不是 None", resp.usage is not None)
    check("usage.input_tokens", resp.usage.input_tokens, PROMPT_TOKENS)
    check("usage.output_tokens", resp.usage.output_tokens, COMPLETION_TOKENS)
    check("usage.total_tokens", resp.usage.total_tokens, TOTAL_TOKENS)
    check("usage.input_tokens_details.cached_tokens",
          resp.usage.input_tokens_details.cached_tokens, CACHED_TOKENS)
    check("usage.output_tokens_details.reasoning_tokens",
          resp.usage.output_tokens_details.reasoning_tokens, REASONING_TOKENS)

    return (f"status={resp.status} usage={resp.usage.input_tokens}/{resp.usage.output_tokens}"
            f"（缓存 {resp.usage.input_tokens_details.cached_tokens}、"
            f"推理 {resp.usage.output_tokens_details.reasoning_tokens}）")


def responses_stream(ctx: Ctx) -> str:
    """流式：一串**带事件名**的事件，正文按增量拼，终态事件里带用量。

    与另外两套协议的流式差别在形状上：这里每一帧都有事件名（OpenAI 的
    chat 流是裸 delta），正文增量与「这一段说完了」是两个事件，终态事件
    （`response.completed`）里嵌着一整个 response 对象——用量就在那里，
    不像 chat 那样单独占一帧。

    顺序是契约的一部分：`response.created` 一定在最先、终态一定在最后。
    中间那几帧的顺序由 Go 那侧的流式用例逐帧钉着（那里的断言更细），
    这里只钉 SDK 读得到什么。
    """
    stream = ctx.client.responses.create(
        model=ctx.model(TEXT_SUFFIX),
        input="你好",
        max_output_tokens=64,
        stream=True,
    )

    seen = []
    text = ""
    completed = None
    for event in stream:
        seen.append(event.type)
        if event.type == "response.output_text.delta":
            text += event.delta
        elif event.type == "response.completed":
            completed = event.response

    check_true("一个事件都没收到", bool(seen))
    check("第一个事件", seen[0], "response.created")
    check("最后一个事件", seen[-1], "response.completed")
    check("拼起来的正文", text, TEXT)
    check_true("流里没有终态事件", completed is not None)
    check("终态的 status", completed.status, "completed")
    check("终态的 usage.output_tokens", completed.usage.output_tokens, COMPLETION_TOKENS)
    check("终态的 usage.output_tokens_details.reasoning_tokens",
          completed.usage.output_tokens_details.reasoning_tokens, REASONING_TOKENS)
    # 终态里嵌的那个 response 对象就是非流式的响应体，output[] 要完整。
    check("终态的 output[0].content[0].text",
          completed.output[0].content[0].text, TEXT)

    return f"{len(seen)} 个事件，末事件={seen[-1]}，usage 在终态里读到"


def responses_balance_exhausted(ctx: Ctx) -> str:
    """Responses 那套的流内错误帧——**官方 SDK 不抛异常**，它交给调用方一个事件。

    这与 chat / messages 那两条不一样，而且不是我们的偏差：Responses 协议
    自己的流内失败就是一个**平铺的事件**（type / code / message 在顶层），
    官方 SDK 把它解析成 `ResponseErrorEvent` 交给调用方（实测：我们那条帧
    被解析成 `ResponseErrorEvent(code='server_error', message=...)`）。
    所以这一条断言的是那个形状：

    1. 收到一条 error 事件，而且是从 SDK 的对象模型上读出来的 `code` 与
       `message`（不是从原始 JSON 里抠）；
    2. error 事件之前收到过正文——失败发生在流的中间；
    3. **没有** `response.completed`：Responses 那套的正常结束标志就是它，
       没有它就说明这条流没跑完。

    第 3 条是这三条用例里唯一能验「没有伪造结束标记」的那种写法，因为
    这一套没有 `[DONE]`：调用方判断「答完了」的依据就是终态事件。

    **迭代过程中抛异常要当失败报出来**：它说明我们那条帧的形状变了
    （比如被包进了一个 `error` 外层），或者 SDK 换了处置方式——两种都要
    重新读一遍这一条，而不是当成「反正它也报错了」。
    """
    stream = ctx.client.responses.create(
        model=ctx.model(DRAIN_SUFFIX),
        input="你好",
        max_output_tokens=100000,
        stream=True,
    )

    text = ""
    events = 0
    err_event = None
    saw_completed = False
    try:
        for event in stream:
            events += 1
            if event.type == "response.output_text.delta":
                text += event.delta
            if event.type == "error":
                err_event = event
            if event.type == "response.completed":
                saw_completed = True
    except Exception as exc:
        raise CaseFailure(
            f"Responses 那套的流内错误不该抛异常：官方 SDK 把它当作一个事件交给"
            f"调用方（它认的是 `type: error` 这个事件类型，不是 chat 那套的 error "
            f"字段）。抛出来的是 {type(exc).__name__}：{exc}"
        ) from exc

    check_true("error 事件之前收到过正文（失败发生在流的中间）", text != "")
    check_true("收到了一条 error 事件", err_event is not None)
    check("error 事件的 code", err_event.code, "server_error")
    check_true("error 事件的 message 提到余额", "balance" in err_event.message)
    check_true("没有 response.completed（没有正常结束的迹象）", not saw_completed)

    return (f"{events} 个事件，error(code={err_event.code})：{err_event.message}"
            f"（此前已收到 {len(text)} 个字符）")


def responses_tool_call(ctx: Ctx) -> str:
    """含工具调用的往返：工具声明是**平铺**的，参数是字符串形态的 JSON。"""
    resp = ctx.client.responses.create(
        model=ctx.model(TOOLS_SUFFIX),
        input="巴黎天气如何",
        max_output_tokens=64,
        tools=[
            {
                "type": "function",
                "name": TOOL_NAME,
                "description": "查一个城市的天气",
                "parameters": {
                    "type": "object",
                    "properties": {"city": {"type": "string"}},
                    "required": ["city"],
                },
            }
        ],
    )

    calls = [item for item in resp.output if item.type == "function_call"]
    check("function_call 项数", len(calls), 1)
    call = calls[0]
    check("function_call.call_id", call.call_id, TOOL_ID)
    check("function_call.name", call.name, TOOL_NAME)
    # arguments 是**字符串形态**的 JSON（与 chat 一致，与 Anthropic 的对象相反）。
    check("function_call.arguments 解出来的对象",
          json.loads(call.arguments), TOOL_INPUT)

    return f"tool={call.name} args={call.arguments}"


def responses_bad_request(ctx: Ctx) -> str:
    """客户端错误：不给输出上界要落到 400 上，而不是被算成一笔没有上界的账。

    触发方式与另外两条入口不同，这是有意的：不给 `max_output_tokens` 是服务端
    必须拒的——预留要有输出上界才做得出来（见 internal/api 的
    httpStatusOfUpstream）。

    这条同时是「max_output_tokens 在本项目是必填的」这条偏差的判据，
    见 README 的「已知偏差」。
    """
    import openai

    try:
        ctx.client.responses.create(
            model=ctx.model(TEXT_SUFFIX),
            input="你好",
        )
    except openai.BadRequestError as exc:
        check("status_code", exc.status_code, 400)
        check("error.type", error_type_of(exc), "invalid_request_error")
        check_true("message 提到 max_tokens",
                   "max_tokens" in body_error_message(exc))
        return f"BadRequestError type={error_type_of(exc)}"
    raise CaseFailure("不给 max_output_tokens 的请求没有报错——服务端本该回 400")


def responses_empty_input(ctx: Ctx) -> str:
    """客户端错误：空的 `input` 回 400，且文案点名 **input**。

    判据（转换之后一条消息都没有）三条入口共用，措辞却不能共用：字段名是
    各协议自己的。Responses 的调用方从来没有写过 `messages`，指着它会让
    调用方去改一个不存在的字段。

    这条与上一条是**两个不同的字段**：那条缺的是 `max_output_tokens`，这条
    缺的是输入。合在一起就只能验到一个字段名。

    `input=[]` 会被 SDK 原样发出去（对应的 pydantic 类型不拦空列表），所以
    这里仍走 SDK 的对象模型，不必绕到裸 HTTP。
    """
    import openai

    try:
        ctx.client.responses.create(
            model=ctx.model(TEXT_SUFFIX),
            input=[],
            max_output_tokens=64,
        )
    except openai.BadRequestError as exc:
        check("status_code", exc.status_code, 400)
        check("error.type", error_type_of(exc), "invalid_request_error")
        message = body_error_message(exc)
        check_true(f"message 要点名 input（实际：{message!r}）", "input" in message)
        # 只断言「出现 input」的话，一句同时提到两个字段的文案照样能过——
        # 而「指着调用方没写过的字段」正是这条用例要防的那件事。
        check_true(f"message 不该提到 messages——Responses 没有这个字段（实际：{message!r}）",
                   "messages" not in message)
        return f"BadRequestError type={error_type_of(exc)} message={message!r}"
    raise CaseFailure("空 input 的请求没有报错——服务端本该回 400")


def responses_auth_error(ctx: Ctx) -> str:
    """认证错误：与非流式那两套共用同一个错误体形状（同一批断言）。

    它的非流式错误体与 OpenAI 那一套**逐字相同**（两家共用同一个官方 SDK 的
    错误解析器），所以 `error.code` 也是 `invalid_api_key`——这一条同时验了
    「我们确实按 OpenAI 的形状回，而不是另写了一套长得像的」。
    """
    import openai

    try:
        ctx.bad_client.responses.create(
            model=ctx.model(TEXT_SUFFIX),
            input="你好",
            max_output_tokens=64,
        )
    except openai.AuthenticationError as exc:
        check("status_code", exc.status_code, 401)
        check("error.type", error_type_of(exc), "invalid_request_error")
        check("error.code", error_code_of(exc), "invalid_api_key")
        return f"AuthenticationError type={error_type_of(exc)} code={error_code_of(exc)}"
    raise CaseFailure("凭证无效时本该回 401，但请求没有报错")


def responses_upstream_error(ctx: Ctx) -> str:
    """上游错误：假上游回 500，我们对外回 502，`error.code` 要认得出。

    Responses 是独立的一条协议常量（`channel.ProtocolOpenAIResponses`），
    而「它的非流式错误体与 OpenAI 那一套逐字相同」是我们自己下的结论——
    形状上真的一样吗，得在这个协议上也走一遍才算验过。所以这一条与上面的
    认证错误一样，自己断言自己的 `error.code`。
    """
    import openai

    try:
        ctx.client.responses.create(
            model=ctx.model(FAIL_SUFFIX),
            input="你好",
            max_output_tokens=64,
        )
    except openai.APIStatusError as exc:
        check("status_code", exc.status_code, 502)
        check("error.type", error_type_of(exc), "api_error")
        check("error.code", error_code_of(exc), "upstream_http_error")
        return f"{type(exc).__name__} status={exc.status_code} type={error_type_of(exc)}"
    raise CaseFailure("上游 500 时我们本该回 502，但请求没有报错")


RESPONSES_CASES = [
    ("非流式", responses_nonstream),
    ("流式·余额耗尽", responses_balance_exhausted),
    ("流式", responses_stream),
    ("工具调用", responses_tool_call),
    ("客户端错误 400", responses_bad_request),
    ("客户端错误 400（空 input）", responses_empty_input),
    ("认证错误 401", responses_auth_error),
    ("上游错误 5xx", responses_upstream_error),
]

CASES_BY_PROTOCOL = {
    "openai": OPENAI_CASES,
    "anthropic": ANTHROPIC_CASES,
    "openai-responses": RESPONSES_CASES,
}


# ── 入口 ──────────────────────────────────────────────────────────────


def build_client(protocol: str, base_url: str, api_key: str):
    """构造 SDK 客户端。

    两个 SDK 拼路径的方式不同，base_url 要跟着不一样：

    - openai 的默认 base_url 是 `https://api.openai.com/v1`，它在后面接
      `/chat/completions`，所以这里要把 `/v1` 带上。
    - anthropic 的默认 base_url 是 `https://api.anthropic.com`，它自己接
      `/v1/messages`，所以这里只给服务根地址。

    Responses 那一条与 openai 共用同一个客户端：`responses.create` 也是在
    同一个 `/v1` 基址后面接 `/responses`。**协议不同不等于 SDK 不同**——
    两者都是 openai 这个包，靠调哪个资源方法区分。

    `max_retries=0`：默认的重试会在 429/5xx 上多打几次，而契约测试要的是
    「这一次响应的形状对不对」——重试会把失败变慢、也会把状态码搅乱。
    """
    if protocol in ("openai", "openai-responses"):
        import openai

        return openai.OpenAI(
            base_url=base_url.rstrip("/") + "/v1",
            api_key=api_key,
            max_retries=0,
        )
    if protocol == "anthropic":
        import anthropic

        return anthropic.Anthropic(
            base_url=base_url.rstrip("/"),
            api_key=api_key,
            max_retries=0,
        )
    raise SystemExit(
        f"不认识的协议：{protocol!r}（只支持 openai / anthropic / openai-responses）")


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="larkax 契约测试")
    parser.add_argument("--base-url", required=True, help="服务的根地址，例如 http://127.0.0.1:8080")
    parser.add_argument("--api-key", required=True, help="一把可用的 API Key 明文")
    parser.add_argument("--protocol", required=True,
                        choices=sorted(CASES_BY_PROTOCOL))
    parser.add_argument("--model-prefix", required=True,
                        help="三个模型名的公共前缀，由装配侧生成")
    args = parser.parse_args(argv)

    client = build_client(args.protocol, args.base_url, args.api_key)
    bad_client = build_client(args.protocol, args.base_url, BAD_API_KEY)
    ctx = Ctx(client, bad_client, args.model_prefix, args.protocol)
    cases = CASES_BY_PROTOCOL[args.protocol]

    print(f"契约测试 · {args.protocol} · 模型前缀 {args.model_prefix}")
    print(f"服务地址 {args.base_url}")

    failures = 0
    for name, fn in cases:
        try:
            detail = fn(ctx)
        except CaseFailure as exc:
            failures += 1
            print(f"  FAIL  {name}：{exc}")
        except Exception as exc:  # noqa: BLE001 —— 任何异常都是这条用例没过
            failures += 1
            print(f"  FAIL  {name}：{type(exc).__name__}: {exc}")
            # 打完整栈：SDK 抛的解析错误往往在深层，只留一行看不到形状差在哪。
            traceback.print_exc(file=sys.stdout)
        else:
            print(f"  ok    {name}（{detail}）")

    print(f"{args.protocol}：{len(cases) - failures}/{len(cases)} 条通过")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
