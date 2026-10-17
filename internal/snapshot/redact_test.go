package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// mustNew 建一个脱敏器，建不出来就直接失败。
func mustNew(t *testing.T, root Root, opts ...Option) *Redactor {
	t.Helper()
	r, err := New(root, opts...)
	if err != nil {
		t.Fatalf("建不出 %q 脱敏器: %v", root, err)
	}
	return r
}

// mustDecode 解析一份报文，失败就直接失败。
func mustDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	m, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("报文解析失败: %v", err)
	}
	return m
}

// mustRedact 脱敏一份已解析的结构，出错就直接失败。
func mustRedact(t *testing.T, r *Redactor, v any) *Result {
	t.Helper()
	res, err := r.RedactRaw(v)
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	return res
}

// TestContentNeverSurvives 是规格里点名的那条：喂一份含密钥、邮箱、私钥、
// 工具名、提示词与图片数据的样本，断言落库结果里一个都不出现。
//
// 样本取值刻意写成「形似但不构成真密钥」的形态：这个仓库是公开的，
// 而形似真密钥的字符串会被代码托管平台的密钥扫描拦下——
// 那件工具处理的正是这一类字符串，所以它的测试样本不能自己去触发那条规则。
func TestContentNeverSurvives(t *testing.T) {
	const (
		prompt    = "帮我分析一下这份文档，重点看第三章"
		email     = "user@example.com"
		keyValue  = "placeholder-key-value"
		teamName  = "internal-billing-audit-pipeline"
		schemaKey = "customer_account_identifier"
		enumValue = "settlement-approved"
		mediaBody = "cGxhY2Vob2xkZXItaW1hZ2UtYnl0ZXMtcGxhY2Vob2xkZXI="
		errorEcho = "invalid tool name: internal-billing-audit-pipeline"
	)

	// 私钥样本的 PEM 头是拼出来的：scripts/check-redlines.sh 按形态抓「私钥头」，
	// 而它抓到的**片段**里不含任何占位词，所以把整段写成字面量会让这个仓库的
	// 自查工具报出它自己的一条测试用例。样本的用途是证明这条链路会把私钥形态的
	// 内容挡掉，取值本身无所谓（LESSONS 里同类问题记过两次）。
	privateKeyValue := "-----BEGIN " + "PRIVATE KEY-----" +
		" placeholder-body -----END " + "PRIVATE KEY-----"

	raw := fmt.Sprintf(`{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 4096,
		"temperature": 0.2,
		"stop": ["%s"],
		"user": "%s",
		"metadata": {"trace": "%s", "note": "%s"},
		"headers": {
			"authorization": "Bearer %s",
			"x-api-key": "%s",
			"x-larkax-tenant": "%s"
		},
		"messages": [
			{"role": "system", "name": "%s", "content": "%s"},
			{"role": "user", "content": [
				{"type": "text", "text": "%s"},
				{"type": "image", "media_type": "image/png", "data": "%s"},
				{"type": "thinking", "thinking": "%s", "signature": "%s"}
			]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "call-placeholder-0001", "name": "%s", "input": {"account": "%s", "amount": 1200, "flagged": true}}
			]}
		],
		"tools": [
			{"type": "function", "function": {
				"name": "%s",
				"description": "%s",
				"parameters": {
					"type": "object",
					"properties": {"%s": {"type": "string", "enum": ["%s"]}},
					"required": ["%s"]
				}
			}}
		],
		"vendor_specific": {"note": "%s"}
	}`,
		keyValue, email, teamName, privateKeyValue,
		keyValue, keyValue, teamName,
		teamName, prompt,
		prompt, mediaBody, prompt, privateKeyValue,
		teamName, schemaKey,
		teamName, prompt, schemaKey, enumValue, schemaKey,
		errorEcho)

	// 注意：`customer_account_identifier`（schema 里的参数名）**故意不在**这份
	// 清单里。保留参数名是显式的设计取舍——schema 校验类错误必须看到键名才能复现，
	// 而它的代价就是参数名本身会落库。这一条测试盯的是内容：提示词、密钥、
	// 邮箱、私钥、工具名、枚举取值、图片字节、错误回显。
	sensitive := []string{
		prompt, email, privateKeyValue, keyValue, teamName,
		enumValue, mediaBody, errorEcho,
		"Bearer", "authorization", "x-api-key",
	}

	for _, root := range Roots() {
		t.Run(string(root), func(t *testing.T) {
			r := mustNew(t, root)
			res := mustRedact(t, r, mustDecode(t, raw))
			for _, s := range sensitive {
				assertAbsent(t, res.Skeleton, s)
			}
			// 骨架本身也要序列化一次：落库的是序列化后的字节（落库那一侧
			// 用的就是这个 json.Marshal，见 snapshotwire 的 Store.Insert），
			// 只检查内存里的 map 会漏掉「序列化时又被写回去」这类路径。
			encoded, err := json.Marshal(res.Skeleton)
			if err != nil {
				t.Fatalf("骨架序列化失败: %v", err)
			}
			for _, s := range sensitive {
				if strings.Contains(string(encoded), s) {
					t.Fatalf("序列化后的骨架里出现了 %q", s)
				}
			}
		})
	}
}

// TestRequestSkeletonShape 确认请求侧的骨架留下了该留的形状。
func TestRequestSkeletonShape(t *testing.T) {
	r := mustNew(t, RootRequest)
	req := &channel.Request{
		Model:           "claude-sonnet-4-20250514",
		MaxTokens:       8192,
		ReasoningEffort: "high",
		Stream:          true,
		Messages: []channel.Message{
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "数一下这段有几个字"}}},
			{Role: "assistant", Content: []channel.ContentBlock{{
				Type: "tool_use", ToolUseID: "call-placeholder-0001",
				ToolName:  "internal-billing-audit-pipeline",
				ToolInput: map[string]any{"account": "placeholder-account", "amount": 1200},
			}}},
			{Role: "tool", Content: []channel.ContentBlock{{
				Type: "tool_result", ToolUseID: "call-placeholder-0001",
				ToolInput: map[string]any{"status": "placeholder-status"},
			}}},
		},
		Tools: []channel.Tool{{
			Name:        "internal-billing-audit-pipeline",
			Description: "查询某个账户的结算状态",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"account": map[string]any{"type": "string", "enum": []any{"placeholder-account"}},
				},
				"required": []any{"account"},
			},
		}},
		Headers: map[string]string{"x-larkax-tenant": "private-tenant-name"},
	}

	res, err := r.RedactRequest(req)
	if err != nil {
		t.Fatalf("请求脱敏失败: %v", err)
	}

	// 形状：模型名、参数、消息条数与角色序列都在。
	if got := res.Skeleton["model"]; got != "claude-sonnet-4-20250514" {
		t.Errorf("模型名没有被放行: %#v", got)
	}
	if !sameNumber(res.Skeleton["max_tokens"], 8192) {
		t.Errorf("max_tokens 没有被放行: %#v", res.Skeleton["max_tokens"])
	}
	messages, ok := res.Skeleton["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("消息条数没有保留: %#v", res.Skeleton["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("角色序列没有保留: %#v", first)
	}

	// 内容：提示词只留长度，按 rune 计数。
	prompt, _ := first["content"].([]any)
	if len(prompt) != 1 {
		t.Fatalf("内容块数量不对: %#v", first["content"])
	}
	block, _ := prompt[0].(map[string]any)
	shape, _ := block["text"].(map[string]any)
	if shape[fieldType] != typeText || !sameNumber(shape[fieldChars], len([]rune("数一下这段有几个字"))) {
		t.Errorf("提示词没有被换成按字符计的占位: %#v", block["text"])
	}
	assertAbsent(t, res.Skeleton, "数一下这段有几个字")

	// 工具：定义与调用都引用化成同一个编号，对应关系不丢。
	tools, _ := res.Skeleton["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("工具定义数量不对: %#v", res.Skeleton["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "T1" {
		t.Errorf("工具名没有被引用化: %#v", tool["name"])
	}
	useBlocks, _ := messages[1].(map[string]any)["content"].([]any)
	use, _ := useBlocks[0].(map[string]any)
	if use["name"] != "T1" {
		t.Errorf("tool_use 的名字与定义不是同一个引用: %#v", use["name"])
	}
	resultBlocks, _ := messages[2].(map[string]any)["content"].([]any)
	result, _ := resultBlocks[0].(map[string]any)
	// 工具调用 id 与工具名各自一套编号：id 是 C 开头。
	if result["tool_use_id"] != "C1" {
		t.Errorf("tool_result 没有对上同一个调用引用: %#v", result["tool_use_id"])
	}
	useID, _ := use["id"].(string)
	if useID != "C1" {
		t.Errorf("tool_use 的 id 与 tool_result 不是同一个引用: %#v", use["id"])
	}
	assertAbsent(t, res.Skeleton, "internal-billing-audit-pipeline")

	// 工具参数的键名留着，值只留描述符。
	input, _ := use["input"].(map[string]any)
	if _, ok := input["account"]; !ok {
		t.Errorf("工具参数的键名没有保留: %#v", input)
	}
	argShape, _ := input["account"].(map[string]any)
	if argShape[fieldType] != typeText {
		t.Errorf("工具参数的值不是文本占位: %#v", input["account"])
	}
	if _, isNum := input["amount"].(float64); isNum {
		t.Errorf("工具参数的数值取值不该原样保留: %#v", input["amount"])
	}

	// 头的取值一个都不留，头名也只剩哈希。
	assertAbsent(t, res.Skeleton, "private-tenant-name")
	headers, _ := res.Skeleton["headers"].(map[string]any)
	if _, leaked := headers["x-larkax-tenant"]; leaked {
		t.Errorf("自定义头名不该原样留下: %#v", headers)
	}
	if _, ok := headers[fieldHashed].([]any); !ok {
		t.Errorf("头名哈希没有留下: %#v", headers)
	}
}

// TestResponseSkeletonShape 确认响应侧的骨架留下了该留的枚举与用量。
func TestResponseSkeletonShape(t *testing.T) {
	r := mustNew(t, RootResponse)
	resp := &channel.Response{
		UpstreamModel: "claude-sonnet-4-20250514",
		Message: channel.Message{
			Role: "assistant",
			Content: []channel.ContentBlock{
				{Type: "text", Text: "这段是模型的回答"},
				{Type: "tool_use", ToolUseID: "call-placeholder-0001",
					ToolName:  "internal-billing-audit-pipeline",
					ToolInput: map[string]any{"account": "placeholder-account"}},
			},
		},
		FinishReason: "tool_calls",
		Usage: channel.Usage{
			PromptTokens: 1200, CompletionTokens: 340,
			CacheReadTokens: 1000, ReasoningTokens: 128,
			UnitCounts: map[string]int64{"searches": 2},
		},
	}

	res, err := r.RedactResponse(resp)
	if err != nil {
		t.Fatalf("响应脱敏失败: %v", err)
	}
	assertAbsent(t, res.Skeleton, "这段是模型的回答")
	assertAbsent(t, res.Skeleton, "internal-billing-audit-pipeline")

	if res.Skeleton["finish_reason"] != "tool_calls" {
		t.Errorf("枚举字段没有被放行: %#v", res.Skeleton["finish_reason"])
	}
	if res.Skeleton["upstream_model"] != "claude-sonnet-4-20250514" {
		t.Errorf("上游实际模型名没有被放行: %#v", res.Skeleton["upstream_model"])
	}
	usage, _ := res.Skeleton["usage"].(map[string]any)
	for key, want := range map[string]int{
		"prompt_tokens":           1200,
		"completion_tokens":       340,
		"cache_read_input_tokens": 1000,
		"reasoning_tokens":        128,
	} {
		if !sameNumber(usage[key], want) {
			t.Errorf("用量 %q 没有被放行: %#v", key, usage[key])
		}
	}
	t.Logf("响应骨架: %#v", res.Skeleton)
}

// TestErrorMessageIsContent 是铁律之三：上游的错误正文经常回显用户输入，
// 所以正文整体脱敏，只留长度与枚举字段。
func TestErrorMessageIsContent(t *testing.T) {
	r := mustNew(t, RootResponse)
	const echo = "invalid tool name: internal-billing-audit-pipeline"
	res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(`{
		"status": 400,
		"error": {
			"type": "invalid_request_error",
			"code": "invalid_tool_name",
			"param": "tools.0.function.name",
			"message": "%s"
		}
	}`, echo)))

	assertAbsent(t, res.Skeleton, echo)
	errObj, _ := res.Skeleton["error"].(map[string]any)
	message, _ := errObj["message"].(map[string]any)
	if message[fieldType] != typeText || !sameNumber(message[fieldChars], len([]rune(echo))) {
		t.Fatalf("错误正文没有被换成长度占位: %#v", errObj["message"])
	}
	// 枚举字段正是排障需要的，必须留下。
	for key, want := range map[string]string{
		"type":  "invalid_request_error",
		"code":  "invalid_tool_name",
		"param": "tools.0.function.name",
	} {
		if errObj[key] != want {
			t.Errorf("错误对象的 %q 是枚举，应当保留：%#v", key, errObj[key])
		}
	}
	if !sameNumber(res.Skeleton["status"], 400) {
		t.Errorf("状态码没有被放行: %#v", res.Skeleton["status"])
	}
}

// TestErrorMessageOnHostileInput 确认错误正文里塞进一段非枚举取值时，
// 同一位置上的枚举规则不会把它放过去。
//
// 这一条针对的是「放行 = 把上游发来的字符串原样写进库」这种写法：
// 上游往一个写着「枚举」的位置回显用户输入时，那种写法会静默放行。
func TestErrorMessageOnHostileInput(t *testing.T) {
	r := mustNew(t, RootResponse)
	const hostile = "用户的名字是张三，请据此调整"
	res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(`{
		"error": {"code": "%s", "param": "%s"}
	}`, hostile, hostile)))

	assertAbsent(t, res.Skeleton, hostile)
	errObj, _ := res.Skeleton["error"].(map[string]any)
	for _, key := range []string{"code", "param"} {
		shape, ok := errObj[key].(map[string]any)
		if !ok || shape[fieldType] != typeText {
			t.Fatalf("非枚举取值没有被按内容处理（%q）: %#v", key, errObj[key])
		}
	}
}

// TestErrorBlockIsContent 确认内容块里的错误块整体按内容处理。
func TestErrorBlockIsContent(t *testing.T) {
	r := mustNew(t, RootResponse)
	const echo = "上游拒绝了这次调用：提示词里出现了不该出现的词"
	res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(`{
		"content": [{"type": "error", "message": "%s", "code": "invalid_request_error"}]
	}`, echo)))

	assertAbsent(t, res.Skeleton, echo)
	blocks, _ := res.Skeleton["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("内容块数量不对: %#v", res.Skeleton["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block[fieldType] != typeText {
		t.Fatalf("错误块没有被换成占位: %#v", block)
	}
	if !sameNumber(block[fieldChars], len([]rune(echo))) {
		t.Errorf("错误块没有留下长度: %#v", block)
	}
	if block["code"] != "invalid_request_error" {
		t.Errorf("错误块的枚举字段应当保留: %#v", block)
	}
}

// TestCredentialsAreNeverCollected 确认凭证类头名连存在与否都不记。
func TestCredentialsAreNeverCollected(t *testing.T) {
	r := mustNew(t, RootRequest)
	res := mustRedact(t, r, mustDecode(t, `{
		"headers": {
			"authorization": "placeholder-credential-value",
			"x-api-key": "placeholder-credential-value",
			"cookie": "placeholder-credential-value",
			"content-type": "application/json"
		}
	}`))

	headers, _ := res.Skeleton["headers"].(map[string]any)
	for _, name := range []string{"authorization", "x-api-key", "cookie"} {
		if _, ok := headers[name]; ok {
			t.Errorf("凭证类头名 %q 不该进骨架: %#v", name, headers)
		}
	}
	for _, s := range collectStrings(headers) {
		if s == shortHash("authorization") || s == shortHash("x-api-key") || s == shortHash("cookie") {
			t.Errorf("凭证类头名连哈希都不该留: %#v", headers)
		}
	}
	// 只有一个非凭证头，所以哈希列表里应当正好一项。
	hashes, _ := headers[fieldHashed].([]any)
	if len(hashes) != 1 {
		t.Fatalf("非凭证头的哈希数量不对: %#v", headers)
	}
}

// TestRedactedPathsFormat 确认被脱敏路径的写法是规格里给的那种，
// 并且同一条请求两次脱敏得到同一份列表（顺序确定）。
func TestRedactedPathsFormat(t *testing.T) {
	r := mustNew(t, RootRequest)
	body := `{
		"messages": [{"role": "user", "content": [{"type": "text", "text": "内容"}]}],
		"stop": ["结束词"],
		"headers": {"x-tenant": "placeholder-tenant"}
	}`
	first := mustRedact(t, r, mustDecode(t, body))
	second := mustRedact(t, r, mustDecode(t, body))

	for _, want := range []string{
		"messages[0].content[0].text",
		"stop[0]",
		"headers",
	} {
		assertMarked(t, first, want)
	}
	if strings.Join(first.Redacted, ",") != strings.Join(second.Redacted, ",") {
		t.Fatalf("两次脱敏的路径列表不一致：%v / %v", first.Redacted, second.Redacted)
	}
	if !sortStrings(first.Redacted) {
		t.Fatalf("路径列表没有排序: %v", first.Redacted)
	}
}

// TestToolRefsArePerCall 确认工具引用编号只在一次调用内稳定。
//
// 跨请求复用编号会让「T1 是什么」变成一个问题需要第二张表才能回答，
// 而快照的定位是自解释的排障辅助。
func TestToolRefsArePerCall(t *testing.T) {
	r := mustNew(t, RootRequest)
	body := `{"tools": [{"name": "probe-tool-alpha"}, {"name": "probe-tool-beta"}]}`

	first := mustRedact(t, r, mustDecode(t, body))
	firstRefs := toolNames(t, first)

	second := mustRedact(t, r, mustDecode(t, body))
	secondRefs := toolNames(t, second)

	if fmt.Sprint(firstRefs) != fmt.Sprint([]string{"T1", "T2"}) {
		t.Fatalf("工具引用编号不是按出现顺序从 T1 开始: %v", firstRefs)
	}
	if fmt.Sprint(secondRefs) != fmt.Sprint(firstRefs) {
		t.Fatalf("同一条请求两次脱敏的编号不一致: %v / %v", secondRefs, firstRefs)
	}
}

// TestRedactorIsSafeForConcurrentUse 让同一个脱敏器被并发使用。
//
// 快照写入是旁路的，同一个实例会被多条请求同时用；引用编号属于单次调用的状态，
// 共享它会让两条请求的编号互相污染。这条测试配合 -race 跑。
func TestRedactorIsSafeForConcurrentUse(t *testing.T) {
	r := mustNew(t, RootRequest)
	body := `{"model": "probe-model", "tools": [{"name": "probe-tool-alpha"}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "内容"}]}]}`

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				res, err := r.RedactRaw(mustDecode(t, body))
				if err != nil {
					t.Errorf("并发脱敏失败: %v", err)
					return
				}
				refs := toolNames(t, res)
				if len(refs) != 1 || refs[0] != "T1" {
					t.Errorf("并发调用之间的编号串了: %v", refs)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// toolNames 取骨架里工具定义的引用编号。
func toolNames(t *testing.T, res *Result) []string {
	t.Helper()
	tools, _ := res.Skeleton["tools"].([]any)
	out := make([]string, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		name, ok := tool["name"].(string)
		if !ok {
			t.Fatalf("工具名不是引用字符串: %#v", tool["name"])
		}
		out = append(out, name)
	}
	return out
}

// sortStrings 判断字符串切片是否已排序。
func sortStrings(list []string) bool {
	for i := 1; i < len(list); i++ {
		if list[i-1] > list[i] {
			return false
		}
	}
	return true
}

// TestDecodeRejectsUnusableBodies 覆盖 Decode 的每一个失败分支。
//
// 每一条都断言「返回错误」；没有任何一条是「回落到解析点什么」——
// 脱敏链路上一旦出现半解析的结果，它落库后就与一条完整的旧快照分不开。
func TestDecodeRejectsUnusableBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "空报文", body: "   \n"},
		{name: "不是 JSON", body: "model=gpt-4\n"},
		{name: "JSON null", body: "null"},
		{name: "顶层是数组", body: `[1,2,3]`},
		{name: "对象之后还有内容", body: `{"model":"probe-model"}{"model":"probe-model"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode([]byte(tc.body))
			if err == nil {
				t.Fatalf("这样的报文应当被拒绝，实际拿到了 %#v", got)
			}
			if !errors.Is(err, ErrRedaction) {
				t.Fatalf("错误没有带 ErrRedaction，调用方无从判断该不该丢快照: %v", err)
			}
			if got != nil {
				t.Fatalf("报错时不该同时返回结构: %#v", got)
			}
		})
	}
}

// TestDecodeKeepsNumbersExact 确认大整数不会被静默改成浮点。
//
// 数字字段（时间戳、计数）被改精度之后，错的那一份会被当成「上游就是这么回的」。
func TestDecodeKeepsNumbersExact(t *testing.T) {
	got := mustDecode(t, `{"created": 1760000000123456789}`)
	n, ok := got["created"].(json.Number)
	if !ok {
		t.Fatalf("数字没有被 json.Number 接住: %T", got["created"])
	}
	if n.String() != "1760000000123456789" {
		t.Fatalf("数字被改动了: %s", n)
	}
}

// TestConstructionRejectsBadInput 覆盖 New 的失败分支。
func TestConstructionRejectsBadInput(t *testing.T) {
	if _, err := New(Root("不在的方向")); !errors.Is(err, ErrRedaction) {
		t.Fatalf("未知方向应当被拒绝: %v", err)
	}
	if _, err := New(RootRequest, WithMaxBytes(0)); !errors.Is(err, ErrRedaction) {
		t.Fatalf("非正的字节上限应当被拒绝: %v", err)
	}
	if _, err := New(RootRequest, WithMaxBytes(-1)); !errors.Is(err, ErrRedaction) {
		t.Fatalf("负的字节上限应当被拒绝: %v", err)
	}
	if got := mustNew(t, RootRequest, WithMaxBytes(4096)).MaxBytes(); got != 4096 {
		t.Fatalf("字节上限没有被选项覆盖: %d", got)
	}
	if got := mustNew(t, RootRequest).Root(); got != RootRequest {
		t.Fatalf("方向不对: %q", got)
	}
}

// TestRedactorRejectsWrongUse 覆盖 RedactRaw / 入口函数用错时的分支。
//
// 用错方向时返回错误而不是「按另一个方向的表处理」：后者会让一份响应骨架
// 被当成请求骨架解释，而两者的字段含义并不相同。
func TestRedactorRejectsWrongUse(t *testing.T) {
	req := mustNew(t, RootRequest)
	resp := mustNew(t, RootResponse)

	if _, err := req.RedactResponse(&channel.Response{}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("请求方向的脱敏器不该处理响应: %v", err)
	}
	if _, err := resp.RedactRequest(&channel.Request{}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("响应方向的脱敏器不该处理请求: %v", err)
	}
	if _, err := req.RedactRequest(nil); !errors.Is(err, ErrRedaction) {
		t.Fatalf("nil 请求应当被拒绝: %v", err)
	}
	if _, err := resp.RedactResponse(nil); !errors.Is(err, ErrRedaction) {
		t.Fatalf("nil 响应应当被拒绝: %v", err)
	}
	if _, err := req.RedactRaw([]any{1, 2}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("顶层不是对象时应当被拒绝: %v", err)
	}
	if _, err := req.RedactRaw(map[string]any{"model": "probe-model"}); err != nil {
		t.Fatalf("正常结构不该失败: %v", err)
	}

	var nilRedactor *Redactor
	if _, err := nilRedactor.RedactRaw(map[string]any{}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("nil 脱敏器应当被拒绝: %v", err)
	}
}

// TestRedactorFailsClosedOnBrokenTable 覆盖「脱敏器自身出问题」的分支。
//
// 这一条是铁律之二的直接检验：表坏掉时返回错误让调用方丢快照，
// 绝不回落成「那就把取值留下吧」。
func TestRedactorFailsClosedOnBrokenTable(t *testing.T) {
	t.Run("允许表为空", func(t *testing.T) {
		r := &Redactor{surface: SurfaceChat, root: RootRequest, table: fieldTable{}, maxBytes: DefaultMaxBytes}
		_, err := r.RedactRaw(map[string]any{"model": "probe-model"})
		if !errors.Is(err, ErrRedaction) {
			t.Fatalf("空表应当导致丢快照: %v", err)
		}
	})

	t.Run("表里有认不出的处理方式", func(t *testing.T) {
		r := &Redactor{
			surface:  SurfaceChat,
			root:     RootRequest,
			table:    fieldTable{"model": fieldKind(200)},
			maxBytes: DefaultMaxBytes,
		}
		res, err := r.RedactRaw(map[string]any{"model": "probe-model"})
		if !errors.Is(err, ErrRedaction) {
			t.Fatalf("认不出的处理方式应当导致丢快照: %v", err)
		}
		if res != nil {
			t.Fatalf("报错时不该同时返回结果: %#v", res)
		}
	})

	t.Run("字节上限未设置", func(t *testing.T) {
		r := &Redactor{surface: SurfaceChat, root: RootRequest, table: buildTable(requestRules)}
		_, err := r.RedactRaw(map[string]any{"model": "probe-model"})
		if !errors.Is(err, ErrRedaction) {
			t.Fatalf("上限未设置应当导致丢快照: %v", err)
		}
	})
}

// TestBudgetDegradesThenFails 覆盖超限的两种处置。
//
// 超限先降级为「只留顶层标量」，那时仍然超限就丢快照——
// 而不是继续裁剪出一份几乎空的骨架：那会让「上限配错了」永远不显形。
func TestBudgetDegradesThenFails(t *testing.T) {
	body := `{
		"model": "probe-model",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "内容"}]}],
		"tools": [{"name": "probe-tool-alpha", "description": "描述"}]
	}`

	t.Run("降级为摘要层", func(t *testing.T) {
		r := mustNew(t, RootRequest, WithMaxBytes(80))
		res := mustRedact(t, r, mustDecode(t, body))
		if !res.Truncated {
			t.Fatal("超限之后没有置 Truncated")
		}
		if res.Skeleton["model"] != "probe-model" {
			t.Fatalf("摘要层没有留住顶层标量: %#v", res.Skeleton)
		}
		for _, key := range []string{"messages", "tools", "headers"} {
			if _, ok := res.Skeleton[key]; ok {
				t.Fatalf("摘要层里不该有容器 %q: %#v", key, res.Skeleton)
			}
		}
		raw, err := json.Marshal(res.Skeleton)
		if err != nil {
			t.Fatalf("摘要层序列化失败: %v", err)
		}
		if len(raw) > 80 {
			t.Fatalf("摘要层仍然超过上限: %d 字节", len(raw))
		}
	})

	t.Run("摘要层也超限则丢快照", func(t *testing.T) {
		r := mustNew(t, RootRequest, WithMaxBytes(4))
		res, err := r.RedactRaw(mustDecode(t, body))
		if !errors.Is(err, ErrRedaction) {
			t.Fatalf("摘要层超限应当导致丢快照: %v", err)
		}
		if res != nil {
			t.Fatalf("报错时不该同时返回结果: %#v", res)
		}
	})

	t.Run("默认上限下不截断", func(t *testing.T) {
		res := mustRedact(t, mustNew(t, RootRequest), mustDecode(t, body))
		if res.Truncated {
			t.Fatal("小骨架不该被截断")
		}
	})
}

// TestUnserializableValueDoesNotLeak 确认不可序列化的取值既不会进骨架、
// 也不会让脱敏回落成「那就留着吧」。
//
// 「序列化不出来的骨架必须变成错误，而不是照样落库」那条判据在落库那一侧
// （见 snapshotwire 的 TestInsertFailsOnUnserializableSkeleton）：序列化
// 只有那一处真的发生。
func TestUnserializableValueDoesNotLeak(t *testing.T) {
	r := mustNew(t, RootRequest)
	res := mustRedact(t, r, map[string]any{
		"model":      "probe-model",
		"unexpected": make(chan string),
		"nested":     map[string]any{"holder": func() {}},
	})
	if _, ok := res.Skeleton["unexpected"]; !ok {
		t.Fatalf("未知字段至少要留下形状: %#v", res.Skeleton)
	}
	shape, _ := res.Skeleton["unexpected"].(map[string]any)
	if shape[fieldType] != typeUnknown {
		t.Fatalf("认不出的取值类型没有记成 unknown: %#v", res.Skeleton["unexpected"])
	}
	// 整个骨架必须还能序列化——落库那一步就在序列化上。
	if _, err := json.Marshal(res.Skeleton); err != nil {
		t.Fatalf("骨架不该因为一个认不出的取值而无法序列化: %v", err)
	}
}

// TestResponseIgnoresRawPayloads 确认上游原样对象不会被顺手带进骨架。
//
// Response.Raw 与 Usage.Raw 的内部结构不受本项目控制，
// 把它们摊进骨架等于把「上游加了什么字段」交给运气。
func TestResponseIgnoresRawPayloads(t *testing.T) {
	r := mustNew(t, RootResponse)
	const echo = "上游原样响应里回显的用户内容"
	resp := &channel.Response{
		UpstreamModel: "probe-model",
		Raw:           map[string]any{"echo": echo},
		Usage:         channel.Usage{PromptTokens: 10, CompletionTokens: 5, Raw: map[string]any{"echo": echo}},
	}
	res := mustRedact(t, r, responseToMap(resp))
	assertAbsent(t, res.Skeleton, echo)

	// 调用方要排查 Raw 时显式传进来：那时讨论范围明确。
	res = mustRedact(t, r, resp.Raw)
	assertAbsent(t, res.Skeleton, echo)
}

// TestBinaryBlocksKeepOnlySize 确认图片与音频块只留字节数。
//
// 内部的字段名与编码方式是上游自定义的，逐字段走会让「数据到底在哪」
// 变成一次猜测，而猜错的那一次就是把 base64 留进了库。
func TestBinaryBlocksKeepOnlySize(t *testing.T) {
	r := mustNew(t, RootRequest)
	// 24 个 base64 字符 → 18 字节。
	const body = "placeholder-image-bytes"

	for _, blockType := range []string{"image", "image_url", "input_audio"} {
		t.Run(blockType, func(t *testing.T) {
			res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(
				`{"messages": [{"role": "user", "content": [
					{"type": %q, "media_type": "image/png", "data": %q,
					 "vendor_ext": {"hint": "上游自定的内部字段"}}
				]}]}`, blockType, body)))

			blocks, _ := res.Skeleton["messages"].([]any)
			first, _ := blocks[0].(map[string]any)
			content, _ := first["content"].([]any)
			block, _ := content[0].(map[string]any)
			assertAbsent(t, block, body)
			// 形状是 {"type":"image","media_type":"image/png","bytes":N}：
			// 块类型留在自己的键名下，因为它属于「形状」那一类信息。
			if block["type"] != blockType {
				t.Fatalf("二进制块的类型应当保留: %#v", block)
			}
			if block["media_type"] != "image/png" {
				t.Fatalf("媒体类型应当保留: %#v", block)
			}
			if _, ok := block[fieldType]; ok {
				t.Fatalf("二进制块不该带占位描述符的 _t 键: %#v", block)
			}
			if _, ok := block[fieldBytes]; !ok {
				t.Fatalf("字节数没有留下: %#v", block)
			}
			if _, ok := block["vendor_ext"]; ok {
				t.Fatalf("二进制块的内部结构不该留下: %#v", block)
			}
		})
	}
}

// TestAnonymousContentKeysAreContent 确认内容类键名的兜底规则。
//
// 上游换个地方回显用户输入时不必等白名单跟着改：只要键名是那几种之一，
// 取值就按内容处理。这是「默认脱敏」在键名层面的落地。
func TestAnonymousContentKeysAreContent(t *testing.T) {
	r := mustNew(t, RootRequest)
	const echo = "用户写在提示词里的原话"
	res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(`{
		"vendor_block": {"text": "%s", "instruction": "%s", "prompt": "%s"}
	}`, echo, echo, echo)))

	assertAbsent(t, res.Skeleton, echo)
	block, _ := res.Skeleton["vendor_block"].(map[string]any)
	for _, key := range []string{"text", "instruction", "prompt"} {
		shape, ok := block[key].(map[string]any)
		if !ok || shape[fieldType] != typeText {
			t.Fatalf("内容类键名 %q 的取值没有被按内容处理: %#v", key, block[key])
		}
	}
}

// TestToolDescriptionAndSchemaEnum 确认工具描述只留长度、
// schema 里的枚举取值不留原文。
func TestToolDescriptionAndSchemaEnum(t *testing.T) {
	r := mustNew(t, RootRequest)
	const (
		desc = "查询某个账户的结算状态并返回明细"
		enum = "settlement-approved"
		prop = "customer_account_identifier"
	)
	res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(`{
		"tools": [{"name": "probe-tool-alpha", "description": %q, "input_schema": {
			"type": "object",
			"properties": {%q: {"type": "string", "enum": [%q]}},
			"required": [%q]
		}}]
	}`, desc, prop, enum, prop)))

	assertAbsent(t, res.Skeleton, desc)
	assertAbsent(t, res.Skeleton, enum)
	assertAbsent(t, res.Skeleton, "probe-tool-alpha")

	tools, _ := res.Skeleton["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	shape, _ := tool["description"].(map[string]any)
	if !sameNumber(shape[fieldChars], len([]rune(desc))) {
		t.Errorf("描述没有按字符数留长度: %#v", tool["description"])
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("schema 的 type 应当保留: %#v", schema)
	}
	// 参数名是复现「漏传必填项」的关键，必须留下。
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props[prop]; !ok {
		t.Errorf("schema 的参数名应当保留: %#v", props)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != prop {
		t.Errorf("required 应当保留: %#v", schema["required"])
	}
	field, _ := props[prop].(map[string]any)
	enumShape, _ := field["enum"].([]any)
	if len(enumShape) != 1 {
		t.Fatalf("enum 的形状应当保留: %#v", field["enum"])
	}
	if _, ok := enumShape[0].(map[string]any); !ok {
		t.Errorf("enum 的取值应当换成描述符: %#v", enumShape[0])
	}
}

// TestStopIsRedacted 确认 stop 这一条规格里点名的例外：
// 字段有形状（字符串或一串），取值可承载任意用户字符串，所以只留长度。
func TestStopIsRedacted(t *testing.T) {
	r := mustNew(t, RootRequest)
	const stop = "用户随手写的一句话"
	for _, body := range []string{
		fmt.Sprintf(`{"stop": %q}`, stop),
		fmt.Sprintf(`{"stop": [%q] }`, stop),
		fmt.Sprintf(`{"stop_sequences": [%q]}`, stop),
	} {
		res := mustRedact(t, r, mustDecode(t, body))
		assertAbsent(t, res.Skeleton, stop)
	}
}

// TestUsageZeroFieldsAreAbsent 确认取值为零的用量分项不写进骨架，
// 让「这个渠道不报缓存明细」与「缓存明细是 0」能分开看。
func TestUsageZeroFieldsAreAbsent(t *testing.T) {
	r := mustNew(t, RootResponse)
	res := mustRedact(t, r, responseToMap(&channel.Response{
		Usage: channel.Usage{PromptTokens: 10, CompletionTokens: 4},
	}))
	usage, _ := res.Skeleton["usage"].(map[string]any)
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens", "reasoning_tokens"} {
		if _, ok := usage[key]; ok {
			t.Errorf("零值分项 %q 不该写进骨架: %#v", key, usage)
		}
	}
	if !sameNumber(usage["prompt_tokens"], 10) {
		t.Errorf("用量没有放行: %#v", usage)
	}
}

// TestRoleFieldHandlesToolNameInRolePosition 覆盖角色字段的异常写法。
//
// 没有名字时，OpenAI 的形状会把工具名放进 role 位置。那种取值按引用处理，
// 而不是当作角色名放行。
func TestRoleFieldHandlesToolNameInRolePosition(t *testing.T) {
	r := mustNew(t, RootRequest)
	const toolName = "internal-billing-audit-pipeline"
	res := mustRedact(t, r, mustDecode(t, fmt.Sprintf(`{
		"messages": [{"role": ["user", %q], "content": "内容"}]
	}`, toolName)))

	assertAbsent(t, res.Skeleton, toolName)
	messages, _ := res.Skeleton["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	roles, _ := first["role"].([]any)
	if len(roles) != 2 || roles[0] != "user" {
		t.Fatalf("角色序列没有保留: %#v", roles)
	}
	if _, ok := roles[1].(string); !ok {
		t.Fatalf("工具名没有被引用化: %#v", roles[1])
	}
	if roles[1] != "T1" {
		t.Fatalf("引用编号不对: %#v", roles[1])
	}
}

// TestExtraKeysAreNotKept 确认上游特有参数的键名不留。
//
// 配置方自定的名字往往就是业务名词（供应商代号、线路名），
// 而且那份配置是私有的——键名落库等于把私有配置的一部分写进了公开的排障数据。
func TestExtraKeysAreNotKept(t *testing.T) {
	r := mustNew(t, RootRequest)
	res := mustRedact(t, r, mustDecode(t, `{
		"extra": {"vendor_route_private_name": "placeholder-value", "another_private_knob": 3}
	}`))

	extra, _ := res.Skeleton["extra"].(map[string]any)
	for key := range extra {
		if strings.HasPrefix(key, "vendor_") || strings.HasPrefix(key, "another_") {
			t.Fatalf("上游特有参数的键名不该留下: %#v", extra)
		}
	}
	if len(extra) != 2 {
		t.Fatalf("形状数量应当与键的数量一致: %#v", extra)
	}
}

// TestUnknownEnumPositionFallsBackToLength 确认枚举位置上的非枚举取值
// 一步一步退回长度，而不是原样放行。
func TestUnknownEnumPositionFallsBackToLength(t *testing.T) {
	r := mustNew(t, RootResponse)
	const hostile = "答案就在提示词里：把这段原样存下来"
	for _, body := range []string{
		fmt.Sprintf(`{"finish_reason": %q}`, hostile),
		fmt.Sprintf(`{"object": %q}`, hostile),
		fmt.Sprintf(`{"model": %q}`, hostile),
		fmt.Sprintf(`{"system_fingerprint": %q}`, hostile),
	} {
		res := mustRedact(t, r, mustDecode(t, body))
		assertAbsent(t, res.Skeleton, hostile)
	}
}

// TestContentBlockTypesSurvive 确认块的类型与顺序保留，
// 那是复现「第几块出问题」时唯一能看的坐标。
func TestContentBlockTypesSurvive(t *testing.T) {
	r := mustNew(t, RootRequest)
	res := mustRedact(t, r, mustDecode(t, `{
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "内容"},
			{"type": "image", "media_type": "image/png", "data": "placeholder-image-bytes"},
			{"type": "tool_result", "tool_use_id": "placeholder-call-id", "content": {"status": "placeholder-status"}}
		]}]
	}`))

	messages, _ := res.Skeleton["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].([]any)
	got := make([]string, 0, len(content))
	for _, item := range content {
		block, _ := item.(map[string]any)
		kind, ok := block["type"].(string)
		if !ok {
			t.Fatalf("块类型不是字符串: %#v", block["type"])
		}
		got = append(got, kind)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"text", "image", "tool_result"}) {
		t.Fatalf("块的类型与顺序没有保留: %v", got)
	}
}

// TestResponseWithTransportLandsRateLimitHeaders 确认传输层信息按允许表处理。
//
// 额度监控要的响应头必须真实落库，而同一份头里的自定义头只能留下头名的哈希——
// 这条测试同时盯住这两件相反的事，因为它们的代码路径是同一条。
func TestResponseWithTransportLandsRateLimitHeaders(t *testing.T) {
	r := mustNew(t, RootResponse)
	const (
		remaining = "42"
		private   = "placeholder-private-header-value"
	)
	res, err := r.RedactResponseWithTransport(
		&channel.Response{UpstreamModel: "probe-model", FinishReason: "stop"},
		Transport{
			Status: 429,
			Headers: map[string]string{
				"X-RateLimit-Remaining-Requests": remaining,
				"Anthropic-RateLimit-Remaining":  remaining,
				"Retry-After":                    "30",
				"X-Placeholder-Private-Header":   private,
				"Set-Cookie":                     private,
			},
		})
	if err != nil {
		t.Fatalf("响应脱敏失败: %v", err)
	}

	if !sameNumber(res.Skeleton["status"], 429) {
		t.Errorf("上游状态码没有被放行: %#v", res.Skeleton["status"])
	}
	headers, _ := res.Skeleton["headers"].(map[string]any)
	for _, name := range []string{
		"x-ratelimit-remaining-requests",
		"anthropic-ratelimit-remaining",
		"retry-after",
	} {
		if headers[name] != remaining && headers[name] != "30" {
			t.Errorf("额度监控要的响应头 %q 没有被放行: %#v", name, headers)
		}
	}
	assertAbsent(t, headers, private)
	if _, leaked := headers["x-placeholder-private-header"]; leaked {
		t.Errorf("自定义响应头名不该原样留下: %#v", headers)
	}
	if _, leaked := headers["set-cookie"]; leaked {
		t.Errorf("Set-Cookie 是凭证类响应头，不该留下: %#v", headers)
	}
	if _, ok := headers[fieldHashed].([]any); !ok {
		t.Errorf("其余响应头应当留下头名哈希: %#v", headers)
	}
}

// TestResponseWithTransportRejectsWrongUse 覆盖新入口的失败分支。
func TestResponseWithTransportRejectsWrongUse(t *testing.T) {
	req := mustNew(t, RootRequest)
	if _, err := req.RedactResponseWithTransport(&channel.Response{}, Transport{}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("请求方向的脱敏器不该处理响应: %v", err)
	}
	resp := mustNew(t, RootResponse)
	if _, err := resp.RedactResponseWithTransport(nil, Transport{}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("nil 响应应当被拒绝: %v", err)
	}
}

// TestUintFieldsRejectNegativeAndFraction 确认计数类字段的取值检查。
//
// token 数、条数、状态码都不是负数也不是小数；遇到这类取值说明这一格收到的
// 不是本来的那个东西，按内容处理而不是照样写进库。
func TestUintFieldsRejectNegativeAndFraction(t *testing.T) {
	r := mustNew(t, RootResponse)
	res := mustRedact(t, r, mustDecode(t, `{
		"status": -1,
		"usage": {"prompt_tokens": 12.5, "completion_tokens": "上游把口径改了"},
		"choices": [{"index": 2}]
	}`))

	if _, ok := res.Skeleton["status"].(float64); ok {
		t.Errorf("负数状态码不该原样放行: %#v", res.Skeleton["status"])
	}
	usage, _ := res.Skeleton["usage"].(map[string]any)
	if _, ok := usage["prompt_tokens"].(float64); ok {
		t.Errorf("小数 token 数不该原样放行: %#v", usage["prompt_tokens"])
	}
	if _, ok := usage["completion_tokens"].(string); ok {
		t.Errorf("字符串 token 数不该原样放行: %#v", usage["completion_tokens"])
	}
	choices, _ := res.Skeleton["choices"].([]any)
	index, _ := choices[0].(map[string]any)
	if !sameNumber(index["index"], 2) {
		t.Errorf("正整数下标应当放行: %#v", index["index"])
	}
}

// TestSamplingParamsRejectBadFloats 确认采样参数拒绝 NaN / Inf。
//
// 那两个值序列化出来不是合法 JSON，会让整条骨架在落库那一步失败——
// 失败点离出问题的字段很远。在这里挡住它们，其余骨架照常。
func TestSamplingParamsRejectBadFloats(t *testing.T) {
	r := mustNew(t, RootRequest)
	res := mustRedact(t, r, map[string]any{
		"model":       "probe-model",
		"temperature": math.Inf(1),
		"top_p":       math.NaN(),
	})
	for _, key := range []string{"temperature", "top_p"} {
		if _, ok := res.Skeleton[key].(float64); ok {
			t.Errorf("%s 收到了不能落进 JSON 的浮点，不该原样放行: %#v", key, res.Skeleton[key])
		}
	}
	if _, err := json.Marshal(res.Skeleton); err != nil {
		t.Fatalf("其余骨架应当照常序列化: %v", err)
	}
	assertMarked(t, res, "temperature")
	assertMarked(t, res, "top_p")
}

// TestWireShapesFromRawJSON 覆盖「原样解析出来的报文」里几种不常见的线格式。
//
// 这些分支平时不会走到，但它们的存在是为了「上游换了写法时仍然只留形状」——
// 那正是最需要它们工作的时候，而那时没人会想起来补测试。
func TestWireShapesFromRawJSON(t *testing.T) {
	req := mustNew(t, RootRequest)
	resp := mustNew(t, RootResponse)

	t.Run("头容器是名字到多值", func(t *testing.T) {
		// http.Header 就是 map[string][]string：同名头出现两次是合法写法，
		// 而调用方可能直接把 it 传进来。
		res := mustRedact(t, req, map[string]any{
			"headers": map[string][]string{
				"X-Placeholder-Private": {"placeholder-value", "placeholder-value"},
				"Content-Type":          {"application/json"},
			},
		})
		headers, _ := res.Skeleton["headers"].(map[string]any)
		assertAbsent(t, headers, "placeholder-value")
		if _, leaked := headers["x-placeholder-private"]; leaked {
			t.Fatalf("自定义头名不该留下: %#v", headers)
		}
	})

	t.Run("头容器是名字到任意值", func(t *testing.T) {
		res := mustRedact(t, req, mustDecode(t, `{
			"headers": {"x-a": ["placeholder-value"], "x-b": {"nested": "placeholder-value"}}
		}`))
		headers, _ := res.Skeleton["headers"].(map[string]any)
		assertAbsent(t, headers, "placeholder-value")
	})

	t.Run("头容器形态不对", func(t *testing.T) {
		res := mustRedact(t, req, mustDecode(t, `{"headers": "placeholder-value"}`))
		shape, ok := res.Skeleton["headers"].(map[string]any)
		if !ok || shape[fieldType] != typeText {
			t.Fatalf("形态不对的头应当只留形状: %#v", res.Skeleton["headers"])
		}
		assertAbsent(t, res.Skeleton, "placeholder-value")
	})

	t.Run("内容是纯字符串", func(t *testing.T) {
		// 老写法：content 直接是提示词，不是块数组。
		res := mustRedact(t, req, mustDecode(t,
			`{"messages": [{"role": "user", "content": "placeholder-prompt-body"}]}`))
		assertAbsent(t, res.Skeleton, "placeholder-prompt-body")
		messages, _ := res.Skeleton["messages"].([]any)
		first, _ := messages[0].(map[string]any)
		if _, ok := first["content"].(map[string]any); !ok {
			t.Fatalf("纯字符串内容应当换成占位: %#v", first["content"])
		}
	})

	t.Run("内容是单个对象", func(t *testing.T) {
		res := mustRedact(t, req, mustDecode(t,
			`{"messages": [{"role": "user", "content": {"type": "text", "text": "placeholder-prompt-body"}}]}`))
		assertAbsent(t, res.Skeleton, "placeholder-prompt-body")
	})

	t.Run("内容块数组里混着字符串", func(t *testing.T) {
		res := mustRedact(t, req, mustDecode(t,
			`{"messages": [{"role": "user", "content": ["placeholder-prompt-body"]}]}`))
		assertAbsent(t, res.Skeleton, "placeholder-prompt-body")
		messages, _ := res.Skeleton["messages"].([]any)
		first, _ := messages[0].(map[string]any)
		content, _ := first["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("块数量不对: %#v", first["content"])
		}
		assertMarked(t, res, "messages[0].content[0]")
	})

	t.Run("枚举列表里混着非字符串", func(t *testing.T) {
		res := mustRedact(t, req, mustDecode(t,
			`{"modalities": ["text", {"vendor": "placeholder-value"}]}`))
		assertAbsent(t, res.Skeleton, "placeholder-value")
		modalities, _ := res.Skeleton["modalities"].([]any)
		if len(modalities) != 2 || modalities[0] != "text" {
			t.Fatalf("枚举列表的形状不对: %#v", modalities)
		}
		assertMarked(t, res, "modalities[1]")
	})

	t.Run("数字字典里混着非数字", func(t *testing.T) {
		res := mustRedact(t, resp, mustDecode(t,
			`{"usage": {"unit_counts": {"images": 3, "searches": "placeholder-value", "bad": null}}}`))
		assertAbsent(t, res.Skeleton, "placeholder-value")
		usage, _ := res.Skeleton["usage"].(map[string]any)
		units, _ := usage["unit_counts"].(map[string]any)
		if !sameNumber(units["images"], 3) {
			t.Fatalf("数字取值应当放行: %#v", units)
		}
		assertMarked(t, res, "usage.unit_counts.searches")
		assertMarked(t, res, "usage.unit_counts.bad")
	})

	t.Run("字符数数不出来时退化为零", func(t *testing.T) {
		res := mustRedact(t, req, map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "prompt"},
				map[string]any{"role": "assistant", "content": make(chan int)}},
		})
		messages, _ := res.Skeleton["messages"].([]any)
		second, _ := messages[1].(map[string]any)
		shape, ok := second["content"].(map[string]any)
		if !ok {
			t.Fatalf("认不出的取值应当只留形状: %#v", second["content"])
		}
		if _, ok := shape[fieldType]; !ok {
			t.Fatalf("形状里没有类型: %#v", shape)
		}
		if _, err := json.Marshal(res.Skeleton); err != nil {
			t.Fatalf("其余骨架应当照常序列化: %v", err)
		}
	})

	t.Run("schema 嵌套超过深度上限", func(t *testing.T) {
		// 深度上限保护的是内存与时间：JSON Schema 允许自引用，
		// 而线格式里可能有一份很深的结构。
		deep := map[string]any{"type": "string"}
		for i := 0; i < maxSchemaDepth+8; i++ {
			deep = map[string]any{"type": "object", "properties": map[string]any{"level": deep}}
		}
		res := mustRedact(t, req, map[string]any{
			"tools": []any{map[string]any{"name": "probe-tool", "input_schema": deep}},
		})
		if _, err := json.Marshal(res.Skeleton); err != nil {
			t.Fatalf("深 schema 不该让骨架无法序列化: %v", err)
		}
	})

	t.Run("schema 的未知关键字", func(t *testing.T) {
		res := mustRedact(t, req, mustDecode(t, `{
			"tools": [{"name": "probe-tool", "input_schema": {
				"type": "object",
				"x-vendor-note": "placeholder-value",
				"x-vendor-structure": {"child": "placeholder-value"}
			}}]
		}`))
		assertAbsent(t, res.Skeleton, "placeholder-value")
	})

	t.Run("二进制块的几种写法", func(t *testing.T) {
		for name, block := range map[string]string{
			"source.data": `{"type": "image", "source": {"type": "base64",
				"media_type": "image/png", "data": "placeholder-image-bytes"}}`,
			"image_url 是 data URI": `{"type": "image_url",
				"image_url": "data:image/png;base64,cGxhY2Vob2xkZXItaW1hZ2UtYnl0ZXM="}`,
			"mime_type 写法": `{"type": "input_image", "mime_type": "image/png",
				"b64": "placeholder-image-bytes"}`,
			"没有任何负载": `{"type": "audio", "vendor_field": "placeholder-value"}`,
		} {
			t.Run(name, func(t *testing.T) {
				res := mustRedact(t, req, mustDecode(t, fmt.Sprintf(
					`{"messages": [{"role": "user", "content": [%s]}]}`, block)))
				assertAbsent(t, res.Skeleton, "placeholder-value")
				assertAbsent(t, res.Skeleton, "placeholder-image-bytes")
				messages, _ := res.Skeleton["messages"].([]any)
				first, _ := messages[0].(map[string]any)
				content, _ := first["content"].([]any)
				got, _ := content[0].(map[string]any)
				if _, ok := got[fieldBytes]; !ok {
					t.Fatalf("二进制块没有留下字节数: %#v", got)
				}
			})
		}
	})

	t.Run("响应头里的重复头名", func(t *testing.T) {
		res := mustRedact(t, resp, map[string]any{
			"headers": map[string][]string{
				"X-RateLimit-Remaining-Requests": {"10", "9"},
				"X-Placeholder-Private":          {"placeholder-value"},
			},
		})
		headers, _ := res.Skeleton["headers"].(map[string]any)
		if headers["x-ratelimit-remaining-requests"] == nil {
			t.Fatalf("额度监控要的响应头没有被放行: %#v", headers)
		}
		assertAbsent(t, headers, "placeholder-value")
	})
}

// TestSeenShapesSurviveDecode 确认未知字段在不同类型的容器里都只留形状。
//
// 覆盖的是「上层收到一段没见过的 JSON」这条默认路径——
// 它才是上游新增字段时真正跑的那条。
func TestSeenShapesSurviveDecode(t *testing.T) {
	r := mustNew(t, RootRequest)
	res := mustRedact(t, r, mustDecode(t, `{
		"vendor_object": {"a": "placeholder-value", "b": [1, 2]},
		"vendor_array": [{"c": "placeholder-value"}, "placeholder-value", 3, null],
		"vendor_scalar": "placeholder-value",
		"vendor_number": 7
	}`))

	assertAbsent(t, res.Skeleton, "placeholder-value")
	obj, isObj := res.Skeleton["vendor_object"].(map[string]any)
	if !isObj {
		t.Fatalf("未知对象的形状没有保留: %#v", res.Skeleton["vendor_object"])
	}
	if _, ok := obj["b"].([]any); !ok {
		t.Fatalf("未知对象里的数组没有保留: %#v", obj)
	}
	arr, isArr := res.Skeleton["vendor_array"].([]any)
	if !isArr || len(arr) != 4 {
		t.Fatalf("未知数组的形状没有保留: %#v", res.Skeleton["vendor_array"])
	}
	if _, ok := arr[0].(map[string]any); !ok {
		t.Fatalf("数组里的对象没有保留形状: %#v", arr[0])
	}
	if _, ok := res.Skeleton["vendor_number"].(map[string]any); !ok {
		t.Fatalf("未知数字应当只留描述符: %#v", res.Skeleton["vendor_number"])
	}
}

// TestUnifiedToWireCoversEveryBlockField 走一遍统一结构到骨架的摊平路径，
// 覆盖每一种内容块与请求字段的映射。
//
// 这条路径与「原样解析的报文」是两条入口，而它才是本项目的主路径：
// 请求与响应在管道里已经是 channel 的类型，快照要能反映那些类型里的每个字段。
func TestUnifiedToWireCoversEveryBlockField(t *testing.T) {
	// 图上这段 URL 与这段 base64 都是占位形态：这里要确认的是
	// 「哪个字段被摊到了骨架的哪一格」，不是它们的取值。
	const (
		imageURL    = "https://images.example.com/placeholder.png"
		imageBase64 = "placeholder-image-bytes"
		mediaType   = "image/png"
		longText    = "这是一段占位正文"
	)

	req := mustNew(t, RootRequest)
	got, err := req.RedactRequest(&channel.Request{
		Model: "probe-model",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{
			{Type: "text", Text: longText},
			{Type: "image", ImageURL: imageURL, ImageMediaType: mediaType},
			{Type: "image", ImageBase64: imageBase64, ImageMediaType: mediaType},
			{Type: "image"},
			{Type: "thinking", Text: longText},
			{Type: "tool_use", ToolUseID: "placeholder-call-id", ToolName: "probe-tool", ToolInput: map[string]any{"k": "placeholder-value"}},
			{Type: "tool_result", ToolUseID: "placeholder-call-id", ToolInput: map[string]any{"k": "placeholder-value"}},
			{Type: "", Text: longText, ToolName: "probe-tool", ImageMediaType: mediaType},
			{Type: "vendor_block", Text: longText, ToolInput: map[string]any{"k": "placeholder-value"},
				ImageBase64: imageBase64, ImageURL: imageURL, ImageMediaType: mediaType,
				ToolUseID: "placeholder-call-id", ToolName: "probe-tool"},
		}}},
		Temperature:     new(float64),
		TopP:            new(float64),
		ReasoningEffort: "high",
		Extra:           map[string]any{"vendor_knob": "placeholder-value"},
		Headers:         map[string]string{"x-placeholder-private": "placeholder-value"},
	})
	if err != nil {
		t.Fatalf("请求脱敏失败: %v", err)
	}

	for _, secret := range []string{longText, imageURL, imageBase64, "placeholder-value",
		"placeholder-call-id", "probe-tool", "x-placeholder-private"} {
		assertAbsent(t, got.Skeleton, secret)
	}
	if got.Skeleton["model"] != "probe-model" {
		t.Errorf("模型名应当放行: %#v", got.Skeleton["model"])
	}

	// 图片的三种写法都要落到同一个形状上：字节数 + 媒体类型。
	messages, _ := got.Skeleton["messages"].([]any)
	content, _ := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 9 {
		t.Fatalf("块数量不对: %d", len(content))
	}
	for i := 1; i <= 3; i++ {
		block, isMap := content[i].(map[string]any)
		if !isMap {
			t.Fatalf("第 %d 块不是对象: %#v", i, content[i])
		}
		if _, ok := block[fieldBytes]; !ok {
			t.Errorf("第 %d 块（图片）没有留下字节数: %#v", i, block)
		}
	}
	// url 形态的图片：字节数是编码长度，媒体类型仍然要留。
	urlBlock, _ := content[1].(map[string]any)
	if urlBlock["media_type"] != mediaType {
		t.Errorf("url 形态的图片应当留下媒体类型: %#v", urlBlock)
	}
	// 未给类型的块按 unknown 记，而不是留空串。
	untyped, _ := content[7].(map[string]any)
	if untyped["type"] != typeUnknown {
		t.Errorf("未给类型的块应当记成 unknown: %#v", untyped)
	}
	assertMarked(t, got, "messages[0].content[7].text")

	// 未知块类型：把可能承载内容的字段都摊出来，逐格按规则处理。
	vendor, _ := content[8].(map[string]any)
	if vendor["type"] != "vendor_block" {
		t.Errorf("未知块的类型应当保留: %#v", vendor)
	}
	for _, key := range []string{"text", "input", "media_type"} {
		if _, ok := vendor[key]; !ok {
			t.Errorf("未知块应当摊出 %q 让允许表处理: %#v", key, vendor)
		}
	}

	// 指针为零值的采样参数要摊出来（0 是合法取值，与「未指定」不同）。
	if _, ok := got.Skeleton["temperature"]; !ok {
		t.Errorf("显式传入的 temperature=0 应当保留: %#v", got.Skeleton)
	}
	// Extra 的键名不留，只留形状。
	extra, _ := got.Skeleton["extra"].(map[string]any)
	assertAbsent(t, extra, "vendor_knob")

	// 响应侧：工具调用与文本块一起出现。
	resp := mustNew(t, RootResponse)
	res, err := resp.RedactResponse(&channel.Response{
		Message: channel.Message{Role: "assistant", Content: []channel.ContentBlock{
			{Type: "text", Text: longText},
			{Type: "tool_use", ToolUseID: "placeholder-call-id", ToolName: "probe-tool"},
			{Type: "thinking", Text: longText},
		}},
	})
	if err != nil {
		t.Fatalf("响应脱敏失败: %v", err)
	}
	for _, secret := range []string{longText, "placeholder-call-id", "probe-tool"} {
		assertAbsent(t, res.Skeleton, secret)
	}
	blocks, _ := res.Skeleton["message"].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("响应块的形状没有保留: %#v", res.Skeleton["message"])
	}
	thinking, _ := blocks[2].(map[string]any)
	shape, _ := thinking["thinking"].(map[string]any)
	if shape[fieldType] != typeText {
		t.Errorf("思考内容应当只留长度: %#v", thinking["thinking"])
	}
}
