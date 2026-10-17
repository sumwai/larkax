package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 这个文件钉住「五个 hook 的返回值一律严格解析」。
//
// 它存在的理由是**宽松解析的表现与「插件没生效」完全一样**：插件返回
// `{ channeId: 7 }` 时宿主把它丢掉、这次不决策，作者看到的只有「我的插件没
// 生效」，没有任何一条日志或错误指向那个多出来的字母。这类静默在五个 hook 上
// 长得一模一样，所以判据也要在五个 hook 上各钉一遍——
// 只钉其中几个的话，剩下那几个会随下一次改动悄悄退回宽松。

// ── 解析层：五个返回值类型各自拒掉契约之外的字段 ────────────────────────

// 拼错的字段名、越权字段、想报的金额，三者都要报错，而且报的是同一个哨兵。
//
// 每个类型各喂属于它自己的那几行：`{"dorp":true}` 喂给 RouteDecision 也会报错，
// 但那证明不了 StreamDecision 是严格的。
func TestEveryHookReturnValueRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// wantField 是报错里该出现的那个字段名（字面量，不从被测类型上取）。
		wantField string
		// wantListed 是报错里该列出的可改字段（同样写死）。
		wantListed string
		// decode 是这个类型的解码入口，与真实路径用的是同一个解码器。
		decode func(string) error
	}{
		{
			name:       "外层字段拼错",
			raw:        `{"bodyy":{"temperature":0.1}}`,
			wantField:  `"bodyy"`,
			wantListed: "body、headers",
			decode: func(raw string) error {
				var v RequestPatch
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "多写一个外层字段",
			raw:        `{"body":{"temperature":0.1},"model":"更贵的模型"}`,
			wantField:  `"model"`,
			wantListed: "body、headers",
			decode: func(raw string) error {
				var v RequestPatch
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "RouteDecision 拼错字段名",
			raw:        `{"channeId":7}`,
			wantField:  `"channeId"`,
			wantListed: "channelId、reason",
			decode: func(raw string) error {
				var v RouteDecision
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "RouteDecision 试图给一个价格",
			raw:        `{"channelId":7,"price":5}`,
			wantField:  `"price"`,
			wantListed: "channelId、reason",
			decode: func(raw string) error {
				var v RouteDecision
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "StreamDecision 拼错字段名",
			raw:        `{"dorp":true}`,
			wantField:  `"dorp"`,
			wantListed: "drop、replace、terminate",
			decode: func(raw string) error {
				var v StreamDecision
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "ResponsePatch 拼错字段名",
			raw:        `{"textt":"改写"}`,
			wantField:  `"textt"`,
			wantListed: "text、metadata",
			decode: func(raw string) error {
				var v ResponsePatch
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "ResponsePatch 试图改用量",
			raw:        `{"text":"改写","usage":{"promptTokens":1}}`,
			wantField:  `"usage"`,
			wantListed: "text、metadata",
			decode: func(raw string) error {
				var v ResponsePatch
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "UsageFacts 拼错字段名",
			raw:        `{"promptToken":5}`,
			wantField:  `"promptToken"`,
			wantListed: "promptTokens、completionTokens",
			decode: func(raw string) error {
				var v UsageFacts
				return json.Unmarshal([]byte(raw), &v)
			},
		},
		{
			name:       "UsageFacts 试图报金额",
			raw:        `{"completionTokens":10,"amount":5}`,
			wantField:  `"amount"`,
			wantListed: "promptTokens、completionTokens",
			decode: func(raw string) error {
				var v UsageFacts
				return json.Unmarshal([]byte(raw), &v)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.decode(tc.raw)
			if !errors.Is(err, ErrPatchScope) {
				t.Fatalf("本该被拒，实际 err=%v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantField) {
				t.Errorf("报错里该点出字段 %s，实际 %q", tc.wantField, msg)
			}
			// 报错要顺带把能用的字段列出来：作者的下一个动作就是改名字。
			if !strings.Contains(msg, tc.wantListed) {
				t.Errorf("报错里该列出可改字段 %q，实际 %q", tc.wantListed, msg)
			}
		})
	}
}

// 上报的用量为负时在**解码层**就拒，而不是留给下游各自钳。
//
// 两个下游各挡一半，正是这件事该在这里判的理由：声明了「completion 含推理」
// 时 billing.FromChannelUsage 把负数钳到 0（输出费用被整个抹平，金额侧没有
// 信号，只有一条「推理量多于输出总量」的告警会响），没声明时负数进
// billing.Validate、整次结算失败。一次上报不该有这两种结局里的任何一种。
//
// 五个字段各喂一遍：只钉其中一个的话，剩下四个会随下一次改动悄悄放宽。
// Units 也各喂几遍，且用**插件自定义的键**——它的键名不受契约约束，
// 按固定清单去查的写法在这几条上会挂。
func TestUsageFactsRejectsNegativeCounts(t *testing.T) {
	cases := []struct {
		raw       string
		wantField string
		wantValue string
	}{
		{`{"promptTokens":-1}`, "promptTokens", "-1"},
		{`{"completionTokens":-2}`, "completionTokens", "-2"},
		{`{"cacheReadTokens":-3}`, "cacheReadTokens", "-3"},
		{`{"cacheWriteTokens":-4}`, "cacheWriteTokens", "-4"},
		{`{"reasoningTokens":-5}`, "reasoningTokens", "-5"},
		// Units 的负数由 billing.Validate 拦，但代价与上面几条一样：
		// 整次结算失败，而上游那次调用已经成功、token 已经花掉。
		{`{"units":{"searches":-6}}`, `units["searches"]`, "-6"},
		// 键名是插件自己定的，检查不能照着某个固定清单做。
		{`{"units":{"任意计量":-7}}`, `units["任意计量"]`, "-7"},
		// 两个都是负的：报出来的该是**按键排序后**的第一个。
		// map 的遍历顺序是随机的，不排序的话同一份上报每次报的键都不一样。
		{`{"units":{"b":-1,"a":-8}}`, `units["a"]`, "-8"},
	}
	for _, tc := range cases {
		t.Run(tc.wantField, func(t *testing.T) {
			var f UsageFacts
			err := json.Unmarshal([]byte(tc.raw), &f)
			if !errors.Is(err, ErrPatchScope) {
				t.Fatalf("负数该被拒，实际 err=%v", err)
			}
			msg := err.Error()
			// 报错要指到**是哪个字段**为负：五个字段共用「用量为负」这句
			// 话时，作者只能靠猜。
			if !strings.Contains(msg, tc.wantField) {
				t.Errorf("报错该点出字段 %s，实际 %q", tc.wantField, msg)
			}
			// 值也要在里面：-1 与 -1000000 是两种不同的 bug。
			if !strings.Contains(msg, tc.wantValue) {
				t.Errorf("报错该带上那个负数 %s，实际 %q", tc.wantValue, msg)
			}
		})
	}

	// 零是合法的：上游返回了空回答、这次一次搜索都没调，那是事实而不是缺值。
	var f UsageFacts
	if err := json.Unmarshal([]byte(`{"completionTokens":0,"units":{"searches":0,"任意计量":0}}`), &f); err != nil {
		t.Fatalf("0 不该被拒: %v", err)
	}
	if f.CompletionTokens == nil || *f.CompletionTokens != 0 {
		t.Errorf("0 该原样解出来，实际 %v", f.CompletionTokens)
	}
	if _, ok := f.Units["searches"]; !ok {
		t.Errorf("Units 的 0 该原样解出来，实际 %v", f.Units)
	}
}

// 严格不等于把字段拒之门外：契约里的字段必须照常解出来。
//
// 漏掉一个的表现是「插件报了但没生效」，与宽松解析造成的静默一模一样——
// 所以这一条不能只靠「解析没报错」来测，要真的看值落没落到结构体上。
func TestStrictDecodeStillFillsContractFields(t *testing.T) {
	cases := []struct {
		name string
		// landed 解一次自己的那份补丁，报告契约里的字段有没有真的落到结构体上。
		landed func() bool
	}{
		{"RequestPatch", func() bool {
			var v RequestPatch
			raw := `{"body":{"temperature":0.1},"headers":{"x-larkax-t":"t"}}`
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				return false
			}
			return v.Body != nil && v.Body.Temperature != nil && *v.Body.Temperature == 0.1 &&
				v.Headers["x-larkax-t"] == "t"
		}},
		{"RouteDecision", func() bool {
			var v RouteDecision
			if err := json.Unmarshal([]byte(`{"channelId":42,"reason":"到期最近"}`), &v); err != nil {
				return false
			}
			return v.ChannelID == 42 && v.Reason == "到期最近"
		}},
		{"StreamDecision", func() bool {
			var v StreamDecision
			if err := json.Unmarshal([]byte(`{"drop":true,"replace":"改过","terminate":true}`), &v); err != nil {
				return false
			}
			return v.Drop && v.Terminate && v.Replace != nil && *v.Replace == "改过"
		}},
		{"ResponsePatch", func() bool {
			var v ResponsePatch
			if err := json.Unmarshal([]byte(`{"text":"改写","metadata":{"k":1}}`), &v); err != nil {
				return false
			}
			return v.Text != nil && *v.Text == "改写" && v.Metadata["k"] == float64(1)
		}},
		{"UsageFacts", func() bool {
			var v UsageFacts
			raw := `{"promptTokens":10,"units":{"searches":2},"completionIncludesReasoning":true}`
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				return false
			}
			return v.PromptTokens != nil && *v.PromptTokens == 10 && v.Units["searches"] == 2 &&
				v.CompletionIncludesReasoning != nil && *v.CompletionIncludesReasoning
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.landed() {
				t.Fatalf("%s 的契约字段没有全部落到结构体上", tc.name)
			}
		})
	}
}

// 嵌套的补丁只报一次错，报的是出错那一层的那份字段清单。
//
// `body` 里面那一层由 RequestBodyPatch 自己报（它一直是严格的），外层再包一次
// 的话报错里会出现两个「可改的字段只有」——一份是请求体里能改的字段，
// 一份是 onRequest 能给的字段。两份都列出来看着更全，实际上把「错在哪一层」
// 这件事抹掉了，而作者要做的动作取决于这个：错在 body 里就改 body 里的字段名。
func TestNestedPatchErrorIsReportedOnce(t *testing.T) {
	var v RequestPatch
	err := json.Unmarshal([]byte(`{"body":{"maxTokens":64}}`), &v)
	if !errors.Is(err, ErrPatchScope) {
		t.Fatalf("本该被拒，实际 err=%v", err)
	}
	msg := err.Error()
	if n := strings.Count(msg, "可改的字段只有"); n != 1 {
		t.Errorf("字段清单该只出现一次，实际 %d 次：%q", n, msg)
	}
	// 出现的那一份该是出错那一层的（请求体白名单），不是外层的。
	if !strings.Contains(msg, "messages、system、max_tokens、temperature、top_p、stop、tools、tool_choice") {
		t.Errorf("报错里该列出请求体能改的字段，实际 %q", msg)
	}
	if !strings.Contains(msg, `"maxTokens"`) {
		t.Errorf("报错里该点出拼错的字段，实际 %q", msg)
	}
}

// 自由形状的那几项：严格解析管的是结构体自己的字段名，不是 map 的键。
//
// 把它们一起拒掉是一次越权——`metadata` 与 `units` 的内容由插件自己定，
// 内核不认识那些键，也没有立场认识。
func TestStrictDecodeLeavesFreeFormKeysAlone(t *testing.T) {
	var rp ResponsePatch
	raw := `{"text":"x","metadata":{"任意键":1,"嵌套":{"a":[1,2]}},"metadata2":{}}`
	if err := json.Unmarshal([]byte(raw), &rp); !errors.Is(err, ErrPatchScope) {
		t.Fatalf("metadata 之外的那个字段该被拒，实际 err=%v", err)
	}
	raw = `{"text":"x","metadata":{"任意键":1,"嵌套":{"a":[1,2]}}}`
	if err := json.Unmarshal([]byte(raw), &rp); err != nil {
		t.Fatalf("metadata 里的键由插件自己定，不该被拒：%v", err)
	}
	if _, ok := rp.Metadata["任意键"]; !ok {
		t.Fatalf("metadata 的键没解出来：%v", rp.Metadata)
	}

	var uf UsageFacts
	raw = `{"units":{"searches":2,"任意计量":3}}`
	if err := json.Unmarshal([]byte(raw), &uf); err != nil {
		t.Fatalf("units 里的键由插件自己定，不该被拒：%v", err)
	}
	if uf.Units["任意计量"] != 3 {
		t.Fatalf("units 的键没解出来：%v", uf.Units)
	}
}

// 字段名比较**不区分大小写**，这是宿主 JSON 解码器的行为。
//
// 写这一条是因为它容易被当成「严格解析会挡掉大小写变体」：`channelID` 与
// `channelId` 都能解出来，所以大小写写错**不会**被拒。会静默失效的是拼错的
// 字母（`channeId`）与下划线写法（`channel_id`），见上一条测试。
// 这一条钉的是现状而不是承诺：契约里字段名仍然只有一个写法。
func TestFieldNamesMatchCaseInsensitively(t *testing.T) {
	var d RouteDecision
	if err := json.Unmarshal([]byte(`{"channelID":7}`), &d); err != nil {
		t.Fatalf("大小写变体是宿主解码器的行为，当前不该报错，实际 %v", err)
	}
	if d.ChannelID != 7 {
		t.Fatalf("channelID 该解到 ChannelID 上，实际 %d", d.ChannelID)
	}
}

// ── 真实路径：拼错字段之后这次请求怎么处置 ──────────────────────────────

// callOutcome 把五个 hook 的返回值归一成一份可比较的东西。
//
// Result[T] 是泛型，五个 hook 各是一个实例化，没法放进同一张表里；
// 而归一之后要断言的三件事在五个 hook 上完全相同。
type callOutcome struct {
	err     error
	hasPat  bool
	failure *sandbox.Failure
}

// 五个 hook 各写一个契约之外的字段，看请求侧发生了什么。
//
// 期望在五个 hook 上完全一致：**插件失败，而不是请求失败**。
// 非阻断插件失败不让请求失败是硬约束，而「补丁写错了」是一种插件失败，
// 不是另一种东西——按「忽略这个补丁」处置的话，作者永远等不到那条错误。
func TestUnknownFieldFailsPluginNotRequest(t *testing.T) {
	cases := []struct {
		name  string
		hook  string
		hooks string
		run   func(*Session) callOutcome
	}{
		{
			name:  "onRequest 外层字段拼错",
			hook:  HookRequest,
			hooks: `onRequest(){ return { bodyy: { temperature: 0.1 } }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
		{
			name:  "onRequest 请求体字段拼错",
			hook:  HookRequest,
			hooks: `onRequest(){ return { body: { maxTokens: 64 } }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
		{
			name:  "onRoute 字段拼错",
			hook:  HookRoute,
			hooks: `onRoute(){ return { channeId: 7 }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: []RouteCandidate{{ChannelID: 7}}})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
		{
			name:  "onStream 字段拼错",
			hook:  HookStream,
			hooks: `onStream(){ return { dorp: true }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnStream(t.Context(), StreamContext{RequestID: "r1"}, StreamInput{})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
		{
			name:  "onResponse 字段拼错",
			hook:  HookResponse,
			hooks: `onResponse(){ return { textt: "改写" }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnResponse(t.Context(), ResponseContext{RequestID: "r1", Status: 200})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
		{
			name:  "onUsage 字段拼错",
			hook:  HookUsage,
			hooks: `onUsage(){ return { promptToken: 9 }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnUsage(t.Context(), UsageContext{RequestID: "r1", Protocol: "openai"})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
		{
			// 负数用量是**同一个形状**的契约违规：它不改金额字段、不改身份，
			// 只是一个不能用的值。所以处置也一样——按插件失败记下来，
			// 而不是让它流到计费侧（那边两个方向都错：钳掉是静默少收，
			// 不钳是整次结算失败）。
			name:  "onUsage 上报负数",
			hook:  HookUsage,
			hooks: `onUsage(){ return { completionTokens: -1 }; }`,
			run: func(s *Session) callOutcome {
				r := s.OnUsage(t.Context(), UsageContext{RequestID: "r1", Protocol: "openai"})
				return callOutcome{err: r.Err(), hasPat: r.Patch != nil, failure: r.Failure}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionFor(t, minimalMeta, tc.hooks, sandbox.Host{}, sandbox.Limits{})
			got := tc.run(s)

			if got.err != nil {
				t.Fatalf("非阻断插件写错字段不该让请求失败，实际 %v", got.err)
			}
			if got.hasPat {
				t.Fatal("失败时整份补丁都该丢掉，不给「有的字段生效了」这种没法解释的状态")
			}
			f := got.failure
			if f == nil {
				t.Fatal("写错字段该记一条失败——没有它，作者只看到「插件没生效」")
			}
			if !errors.Is(f, sandbox.ErrBadResult) {
				t.Errorf("该认出是「返回值不符合契约」，实际 kind=%v", f.Kind)
			}
			if !errors.Is(f, ErrPatchScope) {
				t.Errorf("该认出具体是不符合契约，实际 cause=%v", f.Cause)
			}
			if f.Plugin != "p1" || f.Hook != tc.hook {
				t.Errorf("失败里该带上是哪个插件的哪个 hook，实际 %+v", f)
			}
			// 被吞掉的失败也要被看见：一个每次都写错字段的插件，
			// 只从请求结果上是完全看不出来的。
			fails := s.Failures()
			if len(fails) != 1 || fails[0].Hook != tc.hook {
				t.Errorf("失败该被记进会话的失败清单，实际 %v", fails)
			}
		})
	}
}

// 阻断型插件写错字段时请求失败。
//
// 这一条覆盖 onResponse 与 onUsage：它们跑在上游已经回过响应之后，
// 让请求失败意味着「上游已经计了费、客户端却拿到错误」。处置仍然与前两个
// hook 相同——**是否阻断由插件自己声明**，而不是由 hook 的位置决定。
// 理由是同一个性质在 onStream 上更极端（正文已经吐给客户端了），
// 而契约没有为它开特例；给这两个开特例会造出第二把没人看得见的旋钮。
//
// 代价是真实的，所以它写在 docs/plugin-contract.md 里：
// 声明 blocking 的插件在上游调用之后失败，这次请求就是失败的。
func TestUnknownFieldBlocksWhenDeclared(t *testing.T) {
	const blockingMeta = `key:"p1", apiVersion:1, blocking:true,`
	cases := []struct {
		name  string
		hooks string
		run   func(*Session) error
	}{
		{"onRequest", `onRequest(){ return { bodyy: { temperature: 0.1 } }; }`,
			func(s *Session) error { return s.OnRequest(t.Context(), RequestContext{RequestID: "r1"}).Err() }},
		{"onRoute", `onRoute(){ return { channeId: 7 }; }`,
			func(s *Session) error {
				return s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: []RouteCandidate{{ChannelID: 7}}}).Err()
			}},
		{"onStream", `onStream(){ return { dorp: true }; }`,
			func(s *Session) error { return s.OnStream(t.Context(), StreamContext{}, StreamInput{}).Err() }},
		{"onResponse", `onResponse(){ return { textt: "改写" }; }`,
			func(s *Session) error { return s.OnResponse(t.Context(), ResponseContext{RequestID: "r1"}).Err() }},
		{"onUsage", `onUsage(){ return { promptToken: 9 }; }`,
			func(s *Session) error { return s.OnUsage(t.Context(), UsageContext{RequestID: "r1"}).Err() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionFor(t, blockingMeta, tc.hooks, sandbox.Host{}, sandbox.Limits{})
			err := tc.run(s)
			if err == nil {
				t.Fatal("阻断型插件写错字段时请求该失败")
			}
			if !errors.Is(err, ErrPatchScope) {
				t.Errorf("错误里该能认出是不符合契约，实际 %v", err)
			}
		})
	}
}

// 契约里的字段照常走完真实路径：严格解析不该把任何一个 hook 的正常返回挡掉。
//
// 这一条与 TestEndToEndFiveHooks 的区别是它只喂**边界值**——空对象、零值、
// 空字符串，这些是「合法但看着像没给」的返回，最容易被严格解析误伤。
func TestStrictDecodeDoesNotBreakNormalReturns(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onRequest(){ return { body: {} }; },
		 onRoute(){ return {}; },
		 onStream(){ return { replace: "", drop: false }; },
		 onResponse(){ return {}; },
		 onUsage(){ return {}; }`, sandbox.Host{}, sandbox.Limits{})

	steps := map[string]func() error{
		"onRequest":  func() error { return s.OnRequest(t.Context(), RequestContext{}).Err() },
		"onRoute":    func() error { return s.OnRoute(t.Context(), RouteContext{}).Err() },
		"onStream":   func() error { return s.OnStream(t.Context(), StreamContext{}, StreamInput{}).Err() },
		"onResponse": func() error { return s.OnResponse(t.Context(), ResponseContext{}).Err() },
		"onUsage":    func() error { return s.OnUsage(t.Context(), UsageContext{}).Err() },
	}
	for name, call := range steps {
		if err := call(); err != nil {
			t.Errorf("%s 的空补丁是合法的，不该失败：%v", name, err)
		}
	}
	if fails := s.Failures(); len(fails) != 0 {
		t.Errorf("这一轮不该有任何失败，实际 %v", fails)
	}
}
