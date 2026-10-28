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
# 同协议直通那条。上游名 = 规范名 + 这个后缀（见 contract_test.go 的
# contractNativeUpstreamSuffix）：两个名字必须不同，`model` 是覆写项之一。
NATIVE_SUFFIX = "native"
NATIVE_UPSTREAM_SUFFIX = "-upstream"

# 模型清单那条只读路径用的模型名后缀与 vendor。
#
# 它与上面几个不同：不会有请求打到上游，清单里的名字来自目录行与渠道声明
# （见 internal/api/models.go）。vendor 取一个与 larkax 不同的值，于是
# 「owned_by 取的是目录里的 vendor」才是被测的。
MODELS_SUFFIX = "models"
MODELS_VENDOR = "contract-vendor"

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


def openai_models(ctx: Ctx) -> str:
    """模型清单：官方 SDK 在启动时拉一次清单，走的就是这两条路径。

    `client.models.list()` 验的是「SDK 认不认 `{object: list, data: [...]}`
    这个形状」——自测里我们自己解这个体，认不认是两回事。单条那条验的是
    不可用时 SDK 会不会抛 `NotFoundError`，以及 `error.code` 是不是它按码
    分支时要读的那个值。清单还验 offers 的单位与数值：`prices.prompt` 与
    `price` 都是「1000 个输入 token」的金额，必须等于种子规则算出来的那个数
    ——只断两者相等是拦不住整体错一个 1000 倍的。
    """
    import openai

    expected = ctx.model(MODELS_SUFFIX)
    listing = ctx.client.models.list()
    check("object", listing.object, "list")
    # 顶层币种：金额有了单位，调用方不必猜。读 model_extra 而不是
    # getattr(listing, "currency", None)——SDK 丢弃未知字段时后者也是 None，
    # 这条断言就永远是空的。这里断的是**默认值**：契约环境不覆盖
    # billing.default_asset，用的是迁移里那条种子（CNY）。
    listing_extras = getattr(listing, "model_extra", None) or {}
    check("currency", listing_extras.get("currency"), "CNY")
    by_id = {item.id: item for item in listing.data}
    check_true(f"models.list() 里没有 {expected}：{sorted(by_id)}", expected in by_id)

    item = by_id[expected]
    check("id", item.id, expected)
    check("object", item.object, "model")
    check("owned_by", item.owned_by, MODELS_VENDOR)
    # created 固定 0：目录里没有「模型创建时间」这一列（见 internal/api/models.go）。
    check("created", item.created, 0)
    # 这条用例的模型配了计价规则，清单必须带上 offers。
    #
    # 读 model_extra 而不是 getattr(item, "offers", None)：后者在 SDK 丢弃
    # 未知字段时也返回 None，这条断言就永远是空的。model_extra 是 SDK 收集
    # 未建模字段的地方，字段存在时它一定看得见。
    extras = getattr(item, "model_extra", None) or {}
    offers = extras.get("offers")
    check_true(f"有计价规则时清单必须带 offers：{sorted(extras)}",
               isinstance(offers, list) and len(offers) > 0)

    # 钉**绝对值**而不是「两个字段相等」：服务端这两个字段本来就来自同一个
    # 数（`offers.go` 的 Price 取 Prices["prompt"]），报价整体错一个 1000 倍
    # 时等式照样成立，那种断言拦不住任何东西。种子规则的 prompt 是
    # 0.00001/token，1000 个输入 token 就是 0.01000000。
    offer = offers[0]
    prices = offer.get("prices") or {}
    check("offers[0].prices.prompt", prices.get("prompt"), "0.01000000")
    check("offers[0].price", offer.get("price"), "0.01000000")

    one = ctx.client.models.retrieve(expected)
    check("retrieve.id", one.id, expected)
    check("retrieve.owned_by", one.owned_by, MODELS_VENDOR)

    # 不可用：404，而且 SDK 按 `error.code` 分得出「换一个模型」。
    missing = ctx.model("missing")
    try:
        ctx.client.models.retrieve(missing)
    except openai.NotFoundError as exc:
        check("status_code", exc.status_code, 404)
        check("error.type", error_type_of(exc), "invalid_request_error")
        check("error.code", error_code_of(exc), "model_not_found")
    else:
        raise CaseFailure(f"不可用的模型 {missing} 没有抛 NotFoundError，而清单里没有它")

    return f"{len(listing.data)} 个模型，retrieve({missing}) -> NotFoundError"


OPENAI_CASES = [
    ("非流式", openai_nonstream),
    ("流式", openai_stream),
    ("流式·余额耗尽", openai_balance_exhausted),
    ("工具调用", openai_tool_call),
    ("客户端错误 400", openai_bad_request),
    ("认证错误 401", openai_auth_error),
    ("上游错误 5xx", openai_upstream_error),
    ("模型清单", openai_models),
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
    # **它钉的是这台假上游的形状，不是所有上游的行为。** 假上游自己在这一帧写了
    # `input_tokens`（见 contract_test.go 的 anthropicTextStream），断言的是那个值
    # 被原样送达。另有上游在这一帧报 0、把真值只放在 `message_delta` 里——那种形状
    # 契约测试覆盖不到，也不能靠把断言放宽成「> 0」来覆盖（上面那段说的就是这件事）。
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


# ── 同协议直通 ────────────────────────────────────────────────────────


def native_upstream_name(ctx: Ctx) -> str:
    """直通那条模型在**上游那一侧**的名字。

    调用方写规范名（`ctx.model(NATIVE_SUFFIX)`），渠道上配了一条别名把它翻成
    这一个。两个名字必须不同——`model` 是直通的覆写项之一，同名的话「上游名
    被写进去了」与「原文没被动过」是同一件事，断言分不出来。
    """
    return ctx.model(NATIVE_SUFFIX) + NATIVE_UPSTREAM_SUFFIX


def native_request() -> dict:
    """一份**真形状**的 Anthropic 请求参数，带着统一结构读不到的键。

    `thinking` / `cache_control` / `top_k` / `stop_sequences` 都是调用方会发、
    而本项目的统一结构里没有对应位置的东西。它们能不能到上游，只有直通那条路
    做得到——所以这一份同时是「直通在不在」的判据。

    **不带 `temperature` / `top_p`**：开启扩展思考时 Anthropic 要求这两项保持
    缺省，带上会被上游拒。夹具造一份生产上会被拒的报文，测的就不是真实那条路。
    """
    return {
        "max_tokens": 1024,
        "system": [
            {
                "type": "text",
                "text": "你是助手。",
                "cache_control": {"type": "ephemeral"},
            }
        ],
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "text",
                        "text": "北京天气怎么样？",
                        "cache_control": {"type": "ephemeral"},
                    }
                ],
            }
        ],
        "thinking": {"type": "enabled", "budget_tokens": 512},
        "stop_sequences": ["\n\nHuman:"],
        "top_k": 40,
    }


def native_create_kwargs(params: dict) -> dict:
    """把线上报文形状的参数转成这个 SDK 版本的 `create()` 参数。

    `top_k` 不在 pinned 版本的 `create()` 签名里（它是**线上**的合法字段，
    只是这个 SDK 没有显式参数），所以走 `extra_body` —— 发出去的请求体里
    它仍然是一个顶层键，与 `native_request()` 描述的形状一致。

    **不多加别的**：这里只搬这一个键，而不是「反正 extra_body 能塞什么都塞
    进去」。塞进去的键绕过了 SDK 的类型检查，而这条用例要证的正是「调用方
    原文里那些键能不能到上游」——夹具自己造一个 SDK 根本发不出的键，测的是
    一条不存在的路。
    """
    kwargs = dict(params)
    top_k = kwargs.pop("top_k", None)
    if top_k is not None:
        kwargs["extra_body"] = {"top_k": top_k}
    return kwargs


def upstream_echo(resp) -> dict:
    """取出假上游回显进响应里的请求体。

    假上游把收到的报文塞进一个**它自己的**字段（`native_echo`），SDK 容忍未知
    字段并原样留着。于是「上游到底收到了什么」在客户端这边成了可断言的——
    而这正是直通的另一半（发出去的那份是不是原文），只看得见响应的客户端本来
    验不到它。
    """
    echo = getattr(resp, "native_echo", None)
    if not isinstance(echo, dict):
        raise CaseFailure(
            f"响应里没有 native_echo（得到 {echo!r}）——假上游回显请求体那条路"
            "没走到：这次请求多半没走同协议直通")
    return echo


def check_native_sent_body(echo: dict, stream: bool) -> str:
    """上游收到的报文与调用方原文**逐字段等价**（`model` 与 `stream` 除外）。

    这两处是覆写项，判据在调用方那一侧另外写死：

    - `model` 到这里必须是别名翻出来的**上游名**（调用方写的是规范名）；
    - `stream` 由入口决定，原文里没有这个键，适配器总是写它。

    所以两边都按期望值填好再整体比，而不是「两边都抹平」——抹平会让
    「适配器把 stream 写反了」与「原文没动」长得一样。
    """
    want = dict(native_request())
    want["model"] = "upstream-name-placeholder"
    want["stream"] = stream

    got = dict(echo)
    got["model"] = "upstream-name-placeholder"
    if got != want:
        missing = sorted(set(want) - set(got))
        extra = sorted(set(got) - set(want))
        raise CaseFailure(
            f"上游收到的报文与调用方原文不等价：少 {missing}，多 {extra}\n"
            f" got={got!r}\nwant={want!r}")
    return "报文逐字段等价"


def anthropic_native_nonstream(ctx: Ctx) -> str:
    """同协议直通·非流式：原文进、原文出。

    三条一起判，缺一条这条用例就测不到直通：

    - **上游收到的报文**是调用方原文（`model` 与 `stream` 除外）；
    - **响应里的 model** 是调用方写的规范名，不是上游回报的那个；
    - **统一结构不认识的响应字段**（`container`、`usage.service_tier`、非空的
      `stop_sequence`）原样到了调用方。枢纽那条路按 dto 裁剪，这三处会消失
      ——所以它们到了，就是这次真的走了直通。
    """
    params = native_request()
    # 走 with_raw_response：id 那条判据要把响应体里的 id 与响应头 x-request-id
    # 对照，而 SDK 解析后的 Message 对象不带响应头。
    raw = ctx.client.messages.with_raw_response.create(
        model=ctx.model(NATIVE_SUFFIX), **native_create_kwargs(params))
    resp = raw.parse()
    headers = raw.http_response.headers

    echo = upstream_echo(resp)
    check("上游收到的 model", echo.get("model"), native_upstream_name(ctx))
    detail = check_native_sent_body(echo, stream=False)

    check("model", resp.model, ctx.model(NATIVE_SUFFIX))
    check("type", resp.type, "message")
    check("role", resp.role, "assistant")
    check("stop_reason", resp.stop_reason, "end_turn")
    check("stop_sequence", resp.stop_sequence, "\n\nHuman:")
    check("content 块数", len(resp.content), 1)
    check("content[0].text", resp.content[0].text, TEXT)
    check("usage.input_tokens", resp.usage.input_tokens, PROMPT_TOKENS)
    check("usage.output_tokens", resp.usage.output_tokens, COMPLETION_TOKENS)
    # 响应标识由**网关**决定，不取上游那份（上游假设回的是 `msg_native`）：
    # 它必须等于本次请求的关联标识，也就是响应头 x-request-id。非流式直通与
    # 流式直通口径相同，流式那一侧见 anthropic_native_stream。
    #
    # SDK 的 `.request_id` 读的是 OpenAI 家的 `request-id` 头，我们的叫
    # `x-request-id`，所以直接读原始头。
    check("上游的 id 没被原样转发", resp.id != "msg_native", True)
    check("id 等于响应头 x-request-id", resp.id, headers.get("x-request-id"))
    # 这两个是**直通的判据**，不是可有可无的花边：枢纽那条路读不出来。
    check("usage.service_tier", getattr(resp.usage, "service_tier", None), "standard")
    container = getattr(resp, "container", None)
    check_true("container 在", container is not None)
    check("container.id", container.id, "container_contract")

    return f"{detail}；container 与 usage.service_tier 原样到达"


def anthropic_native_stream(ctx: Ctx) -> str:
    """同协议直通·流式：真 SDK 能读完这条原生流，且改写只落在那两处承诺上。

    逐字节那条判据（帧序列与上游字节相同、零合成帧）在 Go 那一侧
    （`internal/assembly` 的直通用例），这里验的是**SDK 认不认**：

    - 扩展思考的 `thinking_delta` 到了调用方——统一事件流把它当中间产物扔掉，
      所以它出现即证明走的是直通；
    - `message_start.message.model` 回显的是调用方写的名字（上游报的是它自己的）；
    - `message_start.message.id` 是网关给的响应标识（等于响应头 x-request-id），
      不是上游的 `msg_native`——与非流式直通同一条口径；
    - `message_delta.usage.input_tokens` 原样是上游报的那个数——直通不补、不改。
    """
    params = native_request()
    stream = ctx.client.messages.create(
        model=ctx.model(NATIVE_SUFFIX), stream=True, **native_create_kwargs(params))
    # 响应头里带着本次请求的关联标识：id 改写那条判据要拿它当期望值。
    # Stream 对象把原始响应挂在 `.response` 上（见 anthropic/_streaming.py）。
    stream_headers = stream.response.headers

    seen = []
    text = ""
    thinking = ""
    echo = None
    start_id = None
    start_model = None
    start_input_tokens = None
    delta_input_tokens = None
    out_tokens = None

    with stream:
        for event in stream:
            seen.append(event.type)
            if event.type == "message_start":
                start_id = event.message.id
                start_model = event.message.model
                start_input_tokens = event.message.usage.input_tokens
                echo = getattr(event.message, "native_echo", None)
            elif event.type == "content_block_delta":
                if event.delta.type == "text_delta":
                    text += event.delta.text
                elif event.delta.type == "thinking_delta":
                    thinking += event.delta.thinking
            elif event.type == "message_delta":
                delta_input_tokens = event.usage.input_tokens
                out_tokens = event.usage.output_tokens

    if not isinstance(echo, dict):
        raise CaseFailure(
            f"message_start 里没有 native_echo（得到 {echo!r}）——这次请求多半"
            "没走同协议直通")
    check("上游收到的 model", echo.get("model"), native_upstream_name(ctx))
    detail = check_native_sent_body(echo, stream=True)

    check("拼起来的正文", text, TEXT)
    # 思考增量只在直通下到得了调用方：统一事件流不转发它。
    check("思考增量", thinking, "想一想。")
    check("message_start.message.model", start_model, ctx.model(NATIVE_SUFFIX))
    # 响应标识：与非流式直通同一条口径——网关给的那个，不是上游的
    # `msg_native`（假上游在 message_start 里报的就是它）。
    check("message_start.message.id 不是上游的",
          start_id != "msg_native", True)
    check("message_start.message.id 等于响应头 x-request-id",
          start_id, stream_headers.get("x-request-id"))
    check("message_start 的 input_tokens", start_input_tokens, PROMPT_TOKENS)
    # 上游的 message_delta **自己带** input_tokens，这一处原样到达调用方：
    # 直通不补、不改上游报的数（补过的那个版本拿本地估算顶上，而那个数只进
    # 了客户端、没进账本。见 internal/api 的 nativeStreamPassthrough.event）。
    check("message_delta 的 input_tokens", delta_input_tokens, PROMPT_TOKENS)
    check("message_delta 的 output_tokens", out_tokens, COMPLETION_TOKENS)

    # 事件顺序：思考块在正文块之前（与上游发的一致），终止是 message_stop。
    check("首事件", seen[0], "message_start")
    check("末事件", seen[-1], "message_stop")

    return f"{detail}；{len(seen)} 个事件，思考增量 {thinking!r}"


ANTHROPIC_CASES = [
    ("非流式", anthropic_nonstream),
    ("流式", anthropic_stream),
    ("流式·余额耗尽", anthropic_balance_exhausted),
    ("工具调用", anthropic_tool_call),
    ("客户端错误 400", anthropic_bad_request),
    ("认证错误 401", anthropic_auth_error),
    ("上游错误 5xx", anthropic_upstream_error),
    ("同协议直通·非流式", anthropic_native_nonstream),
    ("同协议直通·流式", anthropic_native_stream),
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
