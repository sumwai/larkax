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
	"strings"
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

	// 输入侧缓存量与输出侧推理量，**故意非零**：Responses 的响应体里那两个
	// details 子对象是 SDK 的必填字段，而 0 恰好是「字段在不在」都一样的
	// 取值——Python 那一侧要断言的是具体数，不是「有就行」。
	//
	// 它们不改变结算口径：缓存量含在 prompt_tokens 里（见 channel.Usage），
	// 推理量含在 completion_tokens 里。
	contractCachedTokens    = 5
	contractReasoningTokens = 2
)

// 四个模型名的后缀。假上游按**请求里的模型名**决定回什么，
// Python 那一侧按同一组后缀推名字，于是「四种场景」不必各起一个服务。
const (
	contractSuffixText  = "text"
	contractSuffixTools = "tools"
	contractSuffixFail  = "fail"

	// drain 是**余额在流的中间耗尽**那条路（见 #498 的 streamFreeze）。
	//
	// 它必须由真的余额不足触发：假上游没法让网关走进那条路，所以这个后缀
	// 配的是一份把输出侧单价定得极高的规则（见 contractDrainRule），
	// 于是一段没跑完的流就把余额花光了。
	contractSuffixDrain = "drain"

	// models 是 GET /v1/models 那条只读路径。它只**被列出来**，不会被调用：
	// 清单里的名字来自目录行与渠道的 models 声明，与上游无关（见 #872）。
	contractSuffixModels = "models"
)

// contractModelsVendor 是模型清单那一行的 vendor。
//
// 取一个与 larkax 不同的值，这样「owned_by 取的是目录里的 vendor」才是
// 被测的；写成默认值的话，缺了目录行也读到同一个数。
const contractModelsVendor = "contract-vendor"

// contractDrainChunks 是 drain 那条流准备的分片数。
//
// 要**远多于**切断点，否则「流被切断了」可能只是因为「上游先没词了」——
// 那两种情形在客户端看到的帧序列上一模一样。切断点按规则与阈值段推出来是
// 二三十片（见 contractDrainRule），400 片留出十倍量级的余量。
const contractDrainChunks = 400

// contractDrainRule 是 drain 模型的价目。
//
// 输出侧 0.5/Token：初始那一段（一个阈值段 64 个 Token）冻住 32，之后每补
// 一段再冻 32，而契约用例的余额是 100——两三次补冻之后余额见底，流在中途
// 被切断。取 0.5 而不是刚好够切的数，是为了让「切成几段」留出余量：
// 切断点随本地估算的分片粒度浮动，卡在边界上的话这条用例会变得时红时绿。
//
// 输入侧与正文那三条用例同一个数（而且很小），于是流没被切断时那部分的账
// 与别的用例一致。
const contractDrainRule = `{"prompt": 0.00001, "completion": 0.5,` +
	`"cache_read": 0.00001, "cache_write": 0.00001, "reasoning": 0.5}`

// TestContractOpenAI 用官方 openai SDK 走一遍 /v1/chat/completions。
func TestContractOpenAI(t *testing.T) {
	runContract(t, channel.ProtocolOpenAI)
}

// TestContractAnthropic 用官方 anthropic SDK 走一遍 /v1/messages。
func TestContractAnthropic(t *testing.T) {
	runContract(t, channel.ProtocolAnthropic)
}

// TestContractResponses 用官方 openai SDK 的 responses 资源走一遍 /v1/responses。
//
// 与上面两条同一套装配、同一个假上游，只换入口协议与假上游那一侧说的协议。
// 它单独占一条用例而不是塞进 TestContractOpenAI：两条入口的**请求体与响应体
// 都不同**（input/output[] 对 messages/choices），合在一起会让「哪一条挂了」
// 在输出里分不出来。
func TestContractResponses(t *testing.T) {
	runContract(t, channel.ProtocolOpenAIResponses)
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
		protocol:    protocol,
		textModel:   prefix + "-" + contractSuffixText,
		toolModel:   prefix + "-" + contractSuffixTools,
		failModel:   prefix + "-" + contractSuffixFail,
		drainModel:  prefix + "-" + contractSuffixDrain,
		modelsModel: prefix + "-" + contractSuffixModels,
	}

	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	// 规则里把五项 token 单价**全写上**，虽然只有 Responses 那一侧的假上游
	// 会报出缓存量与推理量。
	//
	// 理由是结算层的判据：规则里没写的计量项、而用量里非零，结算会失败
	// （见 billing.Validate——「没写」与「写了 0」是两件事，后者是「这项免费」）。
	// 只写 prompt / completion 的话，Responses 那两条用例会拿到 500，
	// 而流式那一条更隐蔽：响应照常发完，结算在后台失败，客户端看到的是
	// 一个完全正常的流。两类都得靠把规则写全来避免，而不是靠假上游少报。
	//
	// 单价与 prompt / completion 取同一个数，是为了不动另外两条用例的账：
	// 预留上界按输入侧最贵的那一项估（见 billing 的 worst 估算），
	// cache_read 与 prompt 相等时它不变。
	const rule = `{"prompt": 0.00001, "completion": 0.00003,` +
		`"cache_read": 0.00001, "cache_write": 0.00001, "reasoning": 0.00003}`
	for _, model := range []string{fake.textModel, fake.toolModel, fake.failModel} {
		seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, string(protocol), 0)
		seedRule(t, db, model, rule)
	}

	// drain 那条模型名单独一条渠道与一份价目：它的输出侧单价被定得极高
	// （见 contractDrainRule），于是余额会在流的中间耗尽。混在上面那份规则里
	// 会把另外三条用例的账一起改掉。
	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, fake.drainModel, string(protocol), 0)
	seedRule(t, db, fake.drainModel, contractDrainRule)

	// 模型清单那条（见 #872）：**目录行**与**在供的渠道**两样都要，清单才
	// 列得出它——只有渠道没有目录行时不列（判据是两者都在），而 vendor 也
	// 来自目录行。渠道指同一个假上游，但这条用例不会给它发请求。
	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, fake.modelsModel, string(protocol), 0)
	insertCatalogModel(t, db, fake.modelsModel, contractModelsVendor)
	// 清单里的模型要带价：契约验的是 offers 的单价表与 price 同源。
	// 「没有规则就不带 offers」是另一条边界，由 internal/api 的用例钉住。
	seedRule(t, db, fake.modelsModel, rule)

	// 阈值段压到 64 个 Token。
	//
	// 出厂的 4096 也能切，但假上游就得备几万片才够那条流跑到切断点。真正
	// 与这条用例有关的是**段与产出量的比例**，不是段有多大——压小它既不动
	// 另外几条用例的断言（它们只看响应的形状），也让模型名后加的这条用例
	// 跑得起来。见 internal/gateway 的 streamFreeze。
	app, err := buildAppWithSettings(t, db, map[string]string{streamFreezeStepKey: "64"})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
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
// 于是 Python 那一侧在一次运行里就能把六种场景都走一遍。
//
// 与真实上游一样，它读请求体；但它**不校验**请求体里除了模型名与 stream
// 之外的东西——契约测试验的是我们对外响应的形状，不是我们发出去的请求。
type contractUpstream struct {
	protocol channel.Protocol

	textModel  string
	toolModel  string
	failModel  string
	drainModel string
	// modelsModel 只用于 GET /v1/models 那条用例：它由目录行与渠道声明拼
	// 出来，请求不会打到上游。
	modelsModel string

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
		case u.drainModel:
			u.writeDrain(w, req.Stream, req.Model)
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

// writeFailure 回一个错误响应。
//
// Responses 与非流式 OpenAI **共用同一个错误体形状**（`{"error":{...}}`，
// 两家共用同一个官方 SDK 的错误解析器），所以它走下面那条兜底分支，
// 不另写一份——多一份一模一样的定义只会在改一处时漏掉另一处。
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

// contractResponsesUsage 是假上游按 Responses 口径报的用量。
//
// 缓存量与推理量都报非零，而且用 **Responses 自己的字段名**
// （input_tokens_details.cached_tokens / output_tokens_details.reasoning_tokens）。
// 这正是 relaykit 那个 Chat→Responses 转换器做不到的地方：它把推理量写进
// chat 名字的 completion_tokens_details，而 SDK 要读的是 Responses 名字的
// 那一个（见 internal/api/responses.go 的说明）。报 0 的话「字段在不在」
// 在断言里分不出来。
func contractResponsesUsage() string {
	return fmt.Sprintf(
		`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},`+
			`"output_tokens":%d,"output_tokens_details":{"reasoning_tokens":%d},`+
			`"total_tokens":%d}`,
		contractPromptTokens, contractCachedTokens,
		contractCompletionTokens, contractReasoningTokens,
		contractPromptTokens+contractCompletionTokens)
}

// responsesEnvelope 拼一个 Responses 的 response 对象。
//
// 它同时是**非流式响应体**与流式事件里嵌的那个对象——真实上游就是同一个东西，
// 假上游也照同一个来。usage 由调用方给：开场事件里是 null（那时还没有用量），
// 终态事件里才是真值。
func contractResponsesEnvelope(model, status, output, usage string) string {
	return fmt.Sprintf(
		`{"id":"resp_contract","object":"response","created_at":1700000000,"status":%q,`+
			`"model":%q,"output":%s,"usage":%s}`,
		status, model, output, usage)
}

// responsesMessageItem 是一条助手消息在 output[] 里的样子。
func contractResponsesMessageItem(status string) string {
	return fmt.Sprintf(
		`{"type":"message","id":"msg_contract","status":%q,"role":"assistant",`+
			`"content":[{"type":"output_text","text":%q,"annotations":[]}]}`,
		status, contractTextPartA+contractTextPartB)
}

// responsesFunctionCallItem 是一次工具调用在 output[] 里的样子。
//
// 关联 id 叫 call_id（不是 chat 的 id），参数是**字符串**形态的 JSON。
func contractResponsesFunctionCallItem(status string) string {
	return fmt.Sprintf(
		`{"type":"function_call","id":"fc_contract","status":%q,`+
			`"call_id":%q,"name":%q,"arguments":%q}`,
		status, contractToolID, contractToolName, contractToolArgs)
}

// writeText 回一次纯正文的响应。
func (u *contractUpstream) writeText(w http.ResponseWriter, stream bool, model string) {
	if u.protocol == channel.ProtocolOpenAIResponses {
		if stream {
			u.responsesTextStream(w, model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, contractResponsesEnvelope(
			upstreamModelName(model), "completed",
			"["+contractResponsesMessageItem("completed")+"]", contractResponsesUsage()))
		return
	}

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
	if u.protocol == channel.ProtocolOpenAIResponses {
		if stream {
			u.responsesToolStream(w, model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, contractResponsesEnvelope(
			upstreamModelName(model), "completed",
			"["+contractResponsesFunctionCallItem("completed")+"]", contractResponsesUsage()))
		return
	}

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

// responsesTextStream 按 Responses 的顺序吐事件。
//
// 形状与另外两套都不同：每一帧都带**事件名**，正文项有「开 / 增量 / 完」
// 三段，终态事件里嵌着一整个 response 对象——**用量就在那里**，不像 chat
// 那样单独占一帧。中间那几帧的 output_index / item_id 是客户端把增量拼回
// 项里的依据，所以它们不能省。
func (u *contractUpstream) responsesTextStream(w http.ResponseWriter, model string) {
	_, event := u.sse(w)
	name := upstreamModelName(model)

	event("response.created", fmt.Sprintf(
		`{"type":"response.created","sequence_number":0,"response":%s}`,
		contractResponsesEnvelope(name, "in_progress", "[]", "null")))
	event("response.output_item.added",
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,`+
			`"item":{"type":"message","id":"msg_contract","status":"in_progress",`+
			`"role":"assistant","content":[]}}`)
	for _, part := range []string{contractTextPartA, contractTextPartB} {
		event("response.output_text.delta", fmt.Sprintf(
			`{"type":"response.output_text.delta","sequence_number":2,"output_index":0,`+
				`"content_index":0,"item_id":"msg_contract","delta":%q}`, part))
	}
	event("response.output_text.done", fmt.Sprintf(
		`{"type":"response.output_text.done","sequence_number":3,"output_index":0,`+
			`"content_index":0,"item_id":"msg_contract","text":%q}`,
		contractTextPartA+contractTextPartB))
	event("response.output_item.done", fmt.Sprintf(
		`{"type":"response.output_item.done","sequence_number":4,"output_index":0,`+
			`"item":%s}`, contractResponsesMessageItem("completed")))
	event("response.completed", fmt.Sprintf(
		`{"type":"response.completed","sequence_number":5,"response":%s}`,
		contractResponsesEnvelope(name, "completed",
			"["+contractResponsesMessageItem("completed")+"]", contractResponsesUsage())))
}

// responsesToolStream 吐一次工具调用：项开始时给 call_id 与名字，参数按
// function_call_arguments.delta 分片给（真实上游就是分片的，中途解析必然失败）。
//
// 终态事件的 output[] 里要有那个 function_call 项：结束原因（tool_calls）
// 就是从它推出来的（见 responsesadapter 的 finishReason）。
func (u *contractUpstream) responsesToolStream(w http.ResponseWriter, model string) {
	_, event := u.sse(w)
	name := upstreamModelName(model)

	event("response.created", fmt.Sprintf(
		`{"type":"response.created","sequence_number":0,"response":%s}`,
		contractResponsesEnvelope(name, "in_progress", "[]", "null")))
	event("response.output_item.added", fmt.Sprintf(
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,`+
			`"item":{"type":"function_call","id":"fc_contract","status":"in_progress",`+
			`"call_id":%q,"name":%q,"arguments":""}}`, contractToolID, contractToolName))
	for _, part := range []string{`{"city":`, `"Paris"}`} {
		event("response.function_call_arguments.delta", fmt.Sprintf(
			`{"type":"response.function_call_arguments.delta","sequence_number":2,`+
				`"output_index":0,"item_id":"fc_contract","delta":%q}`, part))
	}
	event("response.function_call_arguments.done", fmt.Sprintf(
		`{"type":"response.function_call_arguments.done","sequence_number":3,`+
			`"output_index":0,"item_id":"fc_contract","arguments":%q}`, contractToolArgs))
	event("response.output_item.done", fmt.Sprintf(
		`{"type":"response.output_item.done","sequence_number":4,"output_index":0,`+
			`"item":%s}`, contractResponsesFunctionCallItem("completed")))
	event("response.completed", fmt.Sprintf(
		`{"type":"response.completed","sequence_number":5,"response":%s}`,
		contractResponsesEnvelope(name, "completed",
			"["+contractResponsesFunctionCallItem("completed")+"]", contractResponsesUsage())))
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
	// message_delta 不带 input_tokens：真上游两种形状都发过（带的是
	// adapter_test.go:594 那份，不带的是 native_test.go:772 那份），而契约测试
	// 断言 message_delta 里这一处等于上游报的数（见 contract/check.py）。
	// 让假上游**不带**，是把网关那一层（缺了就补上游在 message_start 报过的
	// 真值）测进去——带上的话，那一层在契约测试里是死代码。
	event("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},`+
			`"usage":{"output_tokens":%d}}`, contractCompletionTokens))
	event("message_stop", `{"type":"message_stop"}`)
}

// ── drain：余额在流的中间耗尽 ────────────────────────────────────────
//
// 这一组假上游只做一件事：回一条**很长的**正文流。它与上面几组分开写，
// 因为要验的行为不同——上面几组验「响应的形状」，这一组验的是「流被我们
// 按余额切断时，客户端那一边看到什么」。
//
// 长度是判据的一部分。短了就会在余额耗尽之前正常结束，那时 Python 那一侧
// 的「SDK 抛异常」会失败——失败的方式是对的，但原因是假上游不给力，
// 而那与被测行为无关。所以分片数由 contractDrainChunks 定，留出余量。

// writeDrain 回一条很长的流。
func (u *contractUpstream) writeDrain(w http.ResponseWriter, stream bool, model string) {
	if !stream {
		// 非流式没有「流内错误帧」这回事，那条用例跑不起来。按非流式回一段
		// 很长的正文只会把「上游没问题」与「用例写错了」搅在一起，
		// 所以走普通正文那条路，让这个分支至少是自洽的。
		u.writeText(w, false, model)
		return
	}
	switch u.protocol {
	case channel.ProtocolAnthropic:
		u.anthropicDrainStream(w, model)
	case channel.ProtocolOpenAIResponses:
		u.responsesDrainStream(w, model)
	default:
		u.openAIDrainStream(w, model)
	}
}

// contractDrainParts 给出 drain 那几条流的分片内容。
//
// 分片固定用正文那一片常量：它只要「非空且每片都一样大」，具体是什么不参与
// 任何断言（判据是「收到过正文」与「被切断在哪儿」，两者都不看内容）。
// 每片必须**远小于一个阈值段**——一片就撞穿整段时，切断点由检查窗口的粒度
// 决定，而那测的是另一件事（见 internal/gateway 的 streamFreeze）。
func contractDrainParts(n int) []string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, contractTextPartA)
	}
	return parts
}

// openAIDrainStream 按 OpenAI 的顺序吐一条长流。
//
// 结尾那三帧（结束分片、用量分片、`[DONE]`）在正常情况下到不了——这条流会
// 先被余额切断。留着它们是为了让「没被切断」这件事有一个明确的形状：
// 那时 SDK 会正常迭代完，用例因此**红在「没抛异常」那一条断言上**，
// 而不是红在一个看起来像上游坏掉的错误上。
func (u *contractUpstream) openAIDrainStream(w http.ResponseWriter, model string) {
	data, _ := u.sse(w)
	name := upstreamModelName(model)

	data(fmt.Sprintf(`{"id":"chatcmpl-contract","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, name))
	for _, part := range contractDrainParts(contractDrainChunks) {
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

// anthropicDrainStream 同上一份，按 Anthropic 的顺序。
func (u *contractUpstream) anthropicDrainStream(w http.ResponseWriter, model string) {
	_, event := u.sse(w)
	name := upstreamModelName(model)

	event("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_contract","type":"message","role":"assistant",`+
			`"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":0}}}`,
		name, contractPromptTokens))
	event("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, part := range contractDrainParts(contractDrainChunks) {
		event("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, part))
	}
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	// message_delta 不带 input_tokens：真上游两种形状都发过（带的是
	// adapter_test.go:594 那份，不带的是 native_test.go:772 那份），而契约测试
	// 断言 message_delta 里这一处等于上游报的数（见 contract/check.py）。
	// 让假上游**不带**，是把网关那一层（缺了就补上游在 message_start 报过的
	// 真值）测进去——带上的话，那一层在契约测试里是死代码。
	event("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},`+
			`"usage":{"output_tokens":%d}}`, contractCompletionTokens))
	event("message_stop", `{"type":"message_stop"}`)
}

// responsesDrainStream 同上一份，按 Responses 的顺序。
func (u *contractUpstream) responsesDrainStream(w http.ResponseWriter, model string) {
	_, event := u.sse(w)
	name := upstreamModelName(model)

	event("response.created", fmt.Sprintf(
		`{"type":"response.created","sequence_number":0,"response":%s}`,
		contractResponsesEnvelope(name, "in_progress", "[]", "null")))
	event("response.output_item.added",
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,`+
			`"item":{"type":"message","id":"msg_contract","status":"in_progress",`+
			`"role":"assistant","content":[]}}`)
	for _, part := range contractDrainParts(contractDrainChunks) {
		event("response.output_text.delta", fmt.Sprintf(
			`{"type":"response.output_text.delta","sequence_number":2,"output_index":0,`+
				`"content_index":0,"item_id":"msg_contract","delta":%q}`, part))
	}
	event("response.output_text.done", fmt.Sprintf(
		`{"type":"response.output_text.done","sequence_number":3,"output_index":0,`+
			`"content_index":0,"item_id":"msg_contract","text":%q}`,
		strings.Repeat(contractTextPartA, contractDrainChunks)))
	event("response.output_item.done", fmt.Sprintf(
		`{"type":"response.output_item.done","sequence_number":4,"output_index":0,`+
			`"item":%s}`, contractResponsesMessageItem("completed")))
	event("response.completed", fmt.Sprintf(
		`{"type":"response.completed","sequence_number":5,"response":%s}`,
		contractResponsesEnvelope(name, "completed",
			"["+contractResponsesMessageItem("completed")+"]", contractResponsesUsage())))
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
