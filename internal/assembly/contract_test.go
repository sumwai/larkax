//go:build contract

// 契约测试：用**官方 SDK** 当客户端跑一遍，两套协议各一遍。
//
// # 它补的是哪个缺口
//
// 本包既有的端到端用例用的是仓库自己写的假上游与自己构造的请求，证明了
// 「我们的请求发得出去、我们的响应我们自己能解析」。它**没有**证明
// 「真实的客户端 SDK 能解析我们的响应」——而形状上最容易错的那几处
// （必填字段漏了、可空字段写成 null 还是省略、finish_reason 的取值集合、
// 流式事件的顺序与终止条件、错误体的 error.type）恰好是自测看不出来的。
//
// # 为什么复用本包的装配，而不是另起一套
//
// 这里要的正是「真库 + 假上游 + 生产接线」，而本包的 harness 已经就是这个
// 形状（testDB / seedChannelOfType / newApp）。另起一套装配等于把同一件事
// 写第二遍，而两份拷贝迟早分叉——分叉的那一份不会报错。
//
// # 为什么用 build tag 而不是 t.Skip
//
// 这一条依赖 Python 与两个官方 SDK，而 `go test ./...` 必须能在没有 Python
// 的环境里跑完。用 t.Skip 挡的话，「跳过」在输出里与「通过」长得一样，
// 退出码也都是 0（见 LESSONS.md 第 16 条）。build tag 让它在默认构建里
// **根本不存在**——不存在的东西不会被误读成通过。
//
// 于是：**Python 不在时这里必须是失败而不是跳过。** 这一条只在
// `-tags=contract` 下编译，而那个 CI job 一定会装 Python 与依赖；
// 找不到解释器说明 job 配错了，不是「环境缺依赖」。下面的检查因此一律
// t.Fatal，不 t.Skip。
package assembly

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 与 contract/check.py 约定好的取值。改这里就要改那里，两边靠同一组常量
// 描述同一个响应，改单边会让断言与实际不符——而那种不符是「测试挂了」，
// 不是「静默通过」，所以还找得回来。
const (
	contractTextPartA = "契约测试"
	contractTextPartB = "的正文"

	contractToolID   = "call_contract"
	contractToolName = "get_weather"
	contractToolArgs = `{"city":"Paris"}`

	contractPromptTokens     = 12
	contractCompletionTokens = 7
)

// 三个模型名的后缀。假上游按**请求里的模型名**决定回什么，
// Python 那一侧按同一组后缀推名字，于是「三种场景」不必各起一个服务。
const (
	contractSuffixText  = "text"
	contractSuffixTools = "tools"
	contractSuffixFail  = "fail"
)

// TestContractOpenAI 用官方 openai SDK 走一遍 /v1/chat/completions。
func TestContractOpenAI(t *testing.T) {
	runContract(t, channel.ProtocolOpenAI)
}

// TestContractAnthropic 用官方 anthropic SDK 走一遍 /v1/messages。
func TestContractAnthropic(t *testing.T) {
	runContract(t, channel.ProtocolAnthropic)
}

func runContract(t *testing.T, protocol channel.Protocol) {
	t.Helper()

	// 缺 DSN 时 testDB 会 t.Skip，而这里要的是**失败**：这个构建标签只在
	// CI 的契约 job 下打开，那个 job 一定带着 DSN。先自己挡一道，
	// 免得「job 配错了」表现成「跳过」（与 LESSONS.md 第 16 条同一个形状）。
	if os.Getenv("LARKAX_DB_DSN") == "" {
		t.Fatal("契约测试需要 LARKAX_DB_DSN：-tags=contract 只在 CI 的契约 job 下打开，" +
			"而那个 job 一定带着它。缺了它说明 job 配错了，不是环境缺依赖。")
	}

	python := contractPython(t)

	db := testDB(t)
	sealer := testSealer(t)

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	prefix := unique("contract-" + string(protocol))
	fake := &contractUpstream{
		protocol:  protocol,
		textModel: prefix + "-" + contractSuffixText,
		toolModel: prefix + "-" + contractSuffixTools,
		failModel: prefix + "-" + contractSuffixFail,
	}

	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	for _, model := range []string{fake.textModel, fake.toolModel, fake.failModel} {
		seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, string(protocol), 0)
		seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	}

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	cmd := exec.CommandContext(t.Context(), python, contractScript(t),
		"--base-url", public.URL,
		"--api-key", key,
		"--protocol", string(protocol),
		"--model-prefix", prefix,
	)
	cmd.Dir = contractRepoRoot(t)

	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		// 子进程的 stdout/stderr 原样打出来：断言失败时那里面写着
		// 「哪个字段、拿到什么、期望什么」，重打一遍等于把它丢掉。
		t.Fatalf("契约测试子进程失败（%v）：\n%s", runErr, out)
	}
	// 成功路径也打一遍：它是「真的跑了」的正向证据。只有失败才输出的东西
	// 与「没跑」在日志里分不出来。
	t.Logf("契约测试输出：\n%s", out)
}

// contractPython 找到 python3，并确认两个 SDK 都在。
//
// 依赖没装是最常见的一种「跑不成」，而它的表现与「跑成了」很像——
// 子进程会以 ImportError 退出，日志里只有一行栈。在这里先查一遍并
// 把安装命令写进错误里，比让下一个人去猜快得多。
func contractPython(t *testing.T) string {
	t.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("找不到 python3：契约测试需要它。这一条只在 -tags=contract 下编译，"+
			"而那个 job 一定会装 Python，所以找不到说明 job 配错了：%v", err)
	}

	probe := exec.Command(python, "-c", "import openai, anthropic")
	out, probeErr := probe.CombinedOutput()
	if probeErr != nil {
		t.Fatalf("python3 里读不到 openai / anthropic：先装依赖\n"+
			"  pip install -r contract/requirements.txt\n"+
			"（%v）\n%s", probeErr, out)
	}
	return python
}

// contractRepoRoot 由本文件的位置推仓库根目录。
//
// 用 runtime.Caller 而不是相对路径 `../..`：测试的工作目录是**包目录**，
// 相对路径能对上，但一旦有人从别处跑、或者包被移动，它就会指错地方，
// 而指错的表现是「找不到脚本」——那与「脚本坏了」很容易混。
func contractRepoRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到本文件路径，无法定位 contract/check.py")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func contractScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(contractRepoRoot(t), "contract", "check.py")
}

// ── 假上游 ────────────────────────────────────────────────────────────

// contractUpstream 是契约测试专用的假上游。
//
// 与包内既有的 fakeUpstream / streamUpstream 分开，而不是给它们加字段：
// 那两者服务的是别的用例，它们的响应体形状与失败注入方式都被那些用例钉着，
// 给它们加一个「按 model 名分支」的开关会把两种用途搅在一起。
//
// 这里要的是另一件事：**按请求里的模型名与 stream 标志**决定回什么，
// 于是 Python 那一侧在一次运行里就能把五种场景都走一遍。
//
// 与真实上游一样，它读请求体；但它**不校验**请求体里除了模型名与 stream
// 之外的东西——契约测试验的是我们对外响应的形状，不是我们发出去的请求。
type contractUpstream struct {
	protocol channel.Protocol

	textModel string
	toolModel string
	failModel string

	mu     sync.Mutex
	calls  int
	bodies [][]byte
}

func (u *contractUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.record(body)

		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)

		switch req.Model {
		case u.failModel:
			u.writeFailure(w)
		case u.toolModel:
			u.writeToolCall(w, req.Stream, req.Model)
		case u.textModel:
			u.writeText(w, req.Stream, req.Model)
		default:
			// 认不出的模型名说明装配侧与 Python 侧的取名对不上。这正是
			// 「契约测试其实什么都没测到」的一种形态，所以让它显式地响，
			// 而不是回一个看起来正常的响应。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w,
				`{"error":{"message":"契约假上游不认识的模型名","type":"server_error"}}`)
		}
	})
}

func (u *contractUpstream) record(body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.bodies = append(u.bodies, body)
}

// upstreamModelName 是上游回报给我们的模型名。
//
// 与调用方要的那个不同（真实上游也常常这样），于是 Python 那一侧能断言
// 「我们透出的是上游回报的名字」这件事本身。
func upstreamModelName(model string) string { return model + "-upstream" }

func (u *contractUpstream) writeFailure(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	if u.protocol == channel.ProtocolAnthropic {
		_, _ = w.Write([]byte(
			`{"type":"error","error":{"type":"api_error","message":"placeholder upstream failure"}}`))
		return
	}
	_, _ = w.Write([]byte(
		`{"error":{"message":"placeholder upstream failure","type":"server_error"}}`))
}

// writeText 回一次纯正文的响应。
func (u *contractUpstream) writeText(w http.ResponseWriter, stream bool, model string) {
	if u.protocol == channel.ProtocolAnthropic {
		if stream {
			u.anthropicTextStream(w, model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_contract","type":"message","role":"assistant","model":%q,`+
			`"content":[{"type":"text","text":%q}],`+
			`"stop_reason":"end_turn","stop_sequence":null,`+
			`"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			upstreamModelName(model), contractTextPartA+contractTextPartB,
			contractPromptTokens, contractCompletionTokens)
		return
	}

	if stream {
		u.openAITextStream(w, model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-contract","object":"chat.completion","created":1700000000,`+
		`"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},`+
		`"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		upstreamModelName(model), contractTextPartA+contractTextPartB,
		contractPromptTokens, contractCompletionTokens,
		contractPromptTokens+contractCompletionTokens)
}

// writeToolCall 回一次纯工具调用的响应。
func (u *contractUpstream) writeToolCall(w http.ResponseWriter, stream bool, model string) {
	if u.protocol == channel.ProtocolAnthropic {
		if stream {
			u.anthropicToolStream(w, model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_contract","type":"message","role":"assistant","model":%q,`+
			`"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],`+
			`"stop_reason":"tool_use","stop_sequence":null,`+
			`"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			upstreamModelName(model), contractToolID, contractToolName, contractToolArgs,
			contractPromptTokens, contractCompletionTokens)
		return
	}

	if stream {
		u.openAIToolStream(w, model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// 纯工具调用时 content 是 null（不是空串），arguments 是**字符串**形态的
	// JSON（不是对象）——这两处正是两家的形状差异所在。
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-contract","object":"chat.completion","created":1700000000,`+
		`"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":null,`+
		`"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},`+
		`"finish_reason":"tool_calls"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		upstreamModelName(model), contractToolID, contractToolName, contractToolArgs,
		contractPromptTokens, contractCompletionTokens,
		contractPromptTokens+contractCompletionTokens)
}

// ── 假上游的流式分支 ──────────────────────────────────────────────────

// sse 起一个 SSE 响应，返回写帧的函数。
//
// 每一帧都 Flush：不 flush 的话帧会攒在缓冲区里，客户端在流结束前读不到，
// 于是「流式」退化成「一次性」，而那种退化在断言上完全看不出来。
func (u *contractUpstream) sse(w http.ResponseWriter) (data func(string), event func(string, string)) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	data = func(payload string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		flush()
	}
	event = func(name, payload string) {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
		flush()
	}
	return data, event
}

// openAITextStream 按 OpenAI 的顺序吐帧：正文分片 → 结束分片 → 用量分片 → [DONE]。
func (u *contractUpstream) openAITextStream(w http.ResponseWriter, model string) {
	data, _ := u.sse(w)
	name := upstreamModelName(model)

	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, name))
	for _, part := range []string{contractTextPartA, contractTextPartB} {
		data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
			`"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, name, part))
	}
	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, name))
	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		name, contractPromptTokens, contractCompletionTokens,
		contractPromptTokens+contractCompletionTokens))
	data("[DONE]")
}

// openAIToolStream 吐一次工具调用：先给 id 与名字，再分两片给参数字符串。
func (u *contractUpstream) openAIToolStream(w http.ResponseWriter, model string) {
	data, _ := u.sse(w)
	name := upstreamModelName(model)

	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, name))
	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function",`+
		`"function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, name, contractToolID, contractToolName))
	for _, part := range []string{`{"city":`, `"Paris"}`} {
		data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,`+
			`"function":{"arguments":%q}}]},"finish_reason":null}]}`, name, part))
	}
	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, name))
	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		name, contractPromptTokens, contractCompletionTokens,
		contractPromptTokens+contractCompletionTokens))
	data("[DONE]")
}

// anthropicTextStream 按 Anthropic 的顺序吐事件。
//
// 用量跨两个事件：输入在 message_start，输出在 message_delta——适配器就是
// 这么累加的，只在一个事件里给会让「实测用量」那条路走不到。
func (u *contractUpstream) anthropicTextStream(w http.ResponseWriter, model string) {
	_, event := u.sse(w)
	name := upstreamModelName(model)

	event("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_contract","type":"message","role":"assistant",`+
			`"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":0}}}`,
		name, contractPromptTokens))
	event("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, part := range []string{contractTextPartA, contractTextPartB} {
		event("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, part))
	}
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},`+
			`"usage":{"output_tokens":%d}}`, contractCompletionTokens))
	event("message_stop", `{"type":"message_stop"}`)
}

// anthropicToolStream 吐一次工具调用：块开始时给 id 与名字，参数按
// input_json_delta 分片给（真实上游就是分片的，中途解析必然失败）。
func (u *contractUpstream) anthropicToolStream(w http.ResponseWriter, model string) {
	_, event := u.sse(w)
	name := upstreamModelName(model)

	event("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_contract","type":"message","role":"assistant",`+
			`"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":0}}}`,
		name, contractPromptTokens))
	event("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use",`+
			`"id":%q,"name":%q,"input":{}}}`, contractToolID, contractToolName))
	for _, part := range []string{`{"city":`, `"Paris"}`} {
		event("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,`+
				`"delta":{"type":"input_json_delta","partial_json":%q}}`, part))
	}
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},`+
			`"usage":{"output_tokens":%d}}`, contractCompletionTokens))
	event("message_stop", `{"type":"message_stop"}`)
}
