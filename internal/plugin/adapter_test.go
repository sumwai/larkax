package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// **凭证不能进插件的视野**，这条是靠签名保证的：RequestContextFrom 收的是
// *channel.Request，而 channel.Channel（凭证在它身上）根本不在参数里。
//
// 这条测试钉住的是「翻译出来的上下文里没有任何凭证字段」——
// 加一个字段就会让它失败，于是「给插件多一点上下文」这件事必须被显式决定。
func TestRequestContextCarriesNoCredentials(t *testing.T) {
	cred := "真的凭证字符串" // 只是本地变量，不会出现在插件的上下文里
	req := &channel.Request{
		Model:     "upstream-name",
		MaxTokens: 64,
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "你好"}}}},
	}
	rc := RequestContextFrom("r1", req, "canonical", "openai")

	// 遍历整个上下文，确认没有任何一个值是凭证。
	seen := walkStrings(t, rc)
	if len(seen) == 0 {
		t.Fatal("遍历没拿到任何字符串，测试本身可能失效")
	}
	for _, s := range seen {
		if s == cred {
			t.Fatalf("上下文里出现了凭证: %v", seen)
		}
	}
	// 上下文里也不该有凭证类的**键名**——有键名说明有人开始往里放了。
	//
	// 用精确匹配而不是子串：`max_tokens` 里有 "token" 但它是一个正当的
	// 请求参数。子串匹配会把合法的字段名报成违规，于是这条检查很快
	// 就会被人忽略（LESSONS 第 12 条记过这件事）。
	for _, key := range walkKeys(t, rc) {
		if credentialKeys[strings.ToLower(key)] {
			t.Errorf("上下文里出现了凭证相关的键 %q", key)
		}
	}
	_ = cred
}

// onRequest 在选路之前跑，而上游名是逐渠道配的、选路之后才翻得出来
// （见 internal/route 的 upstreamRequest）。所以 onRequest 拿到的请求里
// Model 是规范名，插件看到的两个模型名字段也只能是同一个值。
//
// 这条用例钉的是**现实**：UpstreamModel 拿不到上游名不是漏填，而是在
// onRequest 时刻原理上不可知。语义见 docs/plugin-contract.md 的
// 「`onRequest` 看到的 `model` 是翻译前的名字」。
func TestRequestContextModelNamesBeforeAlias(t *testing.T) {
	// 线上到达 RequestContextFrom 的请求就是没翻过的那份：Model 是规范名。
	rc := RequestContextFrom("r1", &channel.Request{Model: "acme/vision"}, "acme/vision", "openai")
	if rc.CanonicalModel != "acme/vision" {
		t.Fatalf("canonicalModel = %q，期望规范名", rc.CanonicalModel)
	}
	if rc.UpstreamModel != rc.CanonicalModel {
		t.Errorf("upstreamModel = %q，onRequest 时刻上游名不可知，该与规范名同值", rc.UpstreamModel)
	}
	if rc.Body["model"] != rc.CanonicalModel {
		t.Errorf("body.model = %v，onRequest 时刻它是翻译前的规范名", rc.Body["model"])
	}
}

// 凭据不在，渠道的 base_url 也不在：base_url 足以让插件推断出上游是谁。
func TestRouteCandidateCarriesNoBaseURL(t *testing.T) {
	cands := []route.Candidate{{
		Channel: channel.Channel{ID: 7, BaseURL: "https://api.example.com/v1", Credential: "真的凭证字符串"},
	}}
	rc := RouteContextFrom("r1", "m", cands)
	seen := walkStrings(t, rc)
	for _, s := range seen {
		if s == "https://api.example.com/v1" || s == "真的凭证字符串" {
			t.Fatalf("候选里出现了 base_url 或凭证: %v", seen)
		}
	}
	if rc.Candidates[0].ChannelID != 7 {
		t.Errorf("渠道 ID 该给，实际 %+v", rc.Candidates[0])
	}
}

// 额度查不到与额度为零必须分开：前者不给数字，后者给 0。
//
// 混在一起会让插件用「优先选额度快用完的」规则时，
// 把所有不支持查额度的上游都排到最前面。
func TestRouteCandidateDistinguishesUnknownQuotaFromZero(t *testing.T) {
	rc := RouteContextFrom("r1", "m", []route.Candidate{
		{Channel: channel.Channel{ID: 1}, Quota: quota.State{}},
		{Channel: channel.Channel{ID: 2}, Quota: quota.State{Remaining: ptrFloat64(0)}},
		{Channel: channel.Channel{ID: 3}, Quota: quota.State{Remaining: ptrFloat64(100), Estimated: true}},
	})
	if rc.Candidates[0].Remaining != nil {
		t.Errorf("查不到额度时不该给数字，实际 %v", *rc.Candidates[0].Remaining)
	}
	if rc.Candidates[0].Confidence != 0 {
		t.Errorf("查不到额度时可信度该是 0，实际 %v", rc.Candidates[0].Confidence)
	}
	if rc.Candidates[1].Remaining == nil || *rc.Candidates[1].Remaining != 0 {
		t.Errorf("额度为零该如实给 0，实际 %s", showPtr(rc.Candidates[1].Remaining))
	}
	if rc.Candidates[2].Confidence != 0.7 {
		t.Errorf("估算值该打折到 0.7，实际 %v", rc.Candidates[2].Confidence)
	}
}

// 额度余量只在「这一组数可比」时才给。
//
// # 为什么这一档要给 null 而不是给数
//
// 插件对 Remaining 唯一说得通的用法是**跨候选比较**（挑一个先用掉），
// 而「可不可比」是整个候选集的性质：按 token 计的套餐与按元计的账户之间，
// 「还剩 100」谁更该先用掉没有答案，比出来的次序完全由单位决定。
// 所以单位混在一起时**一个数都不给**——让插件看到「这里没有可用的信息」。
//
// # 每一档都钉住具体取值
//
// 不写「A 比 B 大」这类相对断言：把 null 换成 0、或把判据反过来，
// 相对断言照样通过，而那正是要挡住的两类改坏。
func TestRouteCandidateRemainingOnlyWhenUnitsComparable(t *testing.T) {
	known := func(id int64, remaining float64, unit string) route.Candidate {
		return route.Candidate{
			Channel: channel.Channel{ID: id},
			Quota:   quota.State{Remaining: ptrFloat64(remaining), Unit: unit},
		}
	}
	noQuota := func(id int64) route.Candidate {
		return route.Candidate{Channel: channel.Channel{ID: id}}
	}

	cases := []struct {
		name string
		in   []route.Candidate
		want []*float64
	}{
		{
			name: "全同单位：数照给",
			in:   []route.Candidate{known(1, 0.5, "money"), known(2, 300, "money")},
			want: []*float64{ptrFloat64(0.5), ptrFloat64(300)},
		},
		{
			name: "单候选：只有一种单位，数照给",
			in:   []route.Candidate{known(1, 42, "tokens")},
			want: []*float64{ptrFloat64(42)},
		},
		{
			name: "两种已知单位：全 null",
			in:   []route.Candidate{known(1, 1_000_000, "tokens"), known(2, 5, "money")},
			want: []*float64{nil, nil},
		},
		{
			// 单位为空**单独算一类**，不与已知单位合并：一个「不知道按什么计」
			// 的渠道与一个「按元计」的渠道之间同样不可比。所以这一档也是全 null。
			name: "一种已知 + 单位未知：未知算第二类，全 null",
			in:   []route.Candidate{known(1, 5, "money"), known(2, 5, "")},
			want: []*float64{nil, nil},
		},
		{
			name: "三种已知单位：全 null",
			in: []route.Candidate{
				known(1, 1, "tokens"), known(2, 2, "requests"), known(3, 3, "money"),
			},
			want: []*float64{nil, nil, nil},
		},
		{
			name: "全空单位：只有一类，数照给",
			in:   []route.Candidate{known(1, 7, ""), known(2, 8, "")},
			want: []*float64{ptrFloat64(7), ptrFloat64(8)},
		},
		{
			name: "全查不到额度：全 null",
			in:   []route.Candidate{noQuota(1), noQuota(2)},
			want: []*float64{nil, nil},
		},
		{
			// 查不到额度的渠道没有单位可言，不该把查得到的那些也拖成 null：
			// 那等于让「不知道」污染「知道」（见 route.MixedQuotaUnits）。
			name: "查不到额度的渠道不参与数单位",
			in:   []route.Candidate{known(1, 5, "money"), noQuota(2)},
			want: []*float64{ptrFloat64(5), nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := RouteContextFrom("r1", "m", tc.in)
			if len(rc.Candidates) != len(tc.want) {
				t.Fatalf("候选数 = %d，期望 %d", len(rc.Candidates), len(tc.want))
			}
			for i, want := range tc.want {
				got := rc.Candidates[i].Remaining
				switch {
				case want == nil && got != nil:
					t.Errorf("候选 %d 的 Remaining = %v，期望 null", i, *got)
				case want != nil && got == nil:
					t.Errorf("候选 %d 的 Remaining = null，期望 %v", i, *want)
				case want != nil && *got != *want:
					t.Errorf("候选 %d 的 Remaining = %v，期望 %v", i, *got, *want)
				}
			}
		})
	}
}

// 可信度跟着数字走：没有数字时它必须是 0。
//
// 留一个「可信度 1 而数字是 null」的组合会读成「我很确信这里没有数」，
// 而插件按 confidence > 0 判断「这个数能不能用」时就踩空了。
func TestRouteCandidateConfidenceFollowsRemaining(t *testing.T) {
	rc := RouteContextFrom("r1", "m", []route.Candidate{
		{Channel: channel.Channel{ID: 1}, Quota: quota.State{Remaining: ptrFloat64(1_000_000), Unit: "tokens"}},
		{Channel: channel.Channel{ID: 2}, Quota: quota.State{Remaining: ptrFloat64(5), Unit: "money"}},
	})
	for i, c := range rc.Candidates {
		if c.Remaining != nil {
			t.Errorf("候选 %d 单位混合时不该给数，实际 %v", i, *c.Remaining)
		}
		if c.Confidence != 0 {
			t.Errorf("候选 %d 没有数时可信度 = %v，期望 0", i, c.Confidence)
		}
	}

	// 单位一致时两者都照旧：这一条与上面相对，说明 0 来自混合单位这个判断，
	// 而不是可信度恒为 0。
	same := RouteContextFrom("r1", "m", []route.Candidate{
		{Channel: channel.Channel{ID: 3}, Quota: quota.State{Remaining: ptrFloat64(5), Unit: "money"}},
		{Channel: channel.Channel{ID: 4}, Quota: quota.State{Remaining: ptrFloat64(5), Unit: "money", Estimated: true}},
	})
	if got := same.Candidates[0]; got.Remaining == nil || *got.Remaining != 5 || got.Confidence != 1 {
		t.Errorf("实测值该给 5 / 可信度 1，实际 %+v", got)
	}
	if got := same.Candidates[1]; got.Remaining == nil || *got.Remaining != 5 || got.Confidence != 0.7 {
		t.Errorf("估算值该给 5 / 可信度 0.7，实际 %+v", got)
	}
}

func TestRouteCandidateCarriesExpiry(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rc := RouteContextFrom("r1", "m", []route.Candidate{
		{Channel: channel.Channel{ID: 1}, Quota: quota.State{Remaining: ptrFloat64(5), ResetsAt: &at}},
	})
	if rc.Candidates[0].ResetsAt == nil || *rc.Candidates[0].ResetsAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("到期时间该给，实际 %s", showPtr(rc.Candidates[0].ResetsAt))
	}
}

// 健康度四档：「没有样本」是 unknown 而不是 healthy。
//
// 后者会让插件用「优先选健康的」这类规则把一个从没跑过的渠道排到前面。
func TestHealthNameMapping(t *testing.T) {
	cases := []struct {
		name string
		h    route.Health
		want string
	}{
		{"连续失败三次", route.Health{ConsecutiveFailures: 3}, "drained"},
		{"连续失败一次", route.Health{ConsecutiveFailures: 1}, "degraded"},
		{"成功率偏低", route.Health{SuccessRate: 0.5}, "degraded"},
		{"成功率高且有延迟样本", route.Health{SuccessRate: 0.99, LatencyP95: time.Second}, "healthy"},
		{"没有任何样本", route.Health{}, "unknown"},
		{"成功率高但没延迟样本", route.Health{SuccessRate: 1}, "unknown"},
	}
	for _, tc := range cases {
		if got := healthName(tc.h); got != tc.want {
			t.Errorf("%s: healthName = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

// 用量事实里没有金额：UsageFacts 的字段表就是「只能报数量」这条的实现。
func TestUsageFactsCarryCountsOnly(t *testing.T) {
	facts := UsageFactsFrom(channel.Usage{
		PromptTokens: 10, CompletionTokens: 20, CacheReadTokens: 3,
		UnitCounts: map[string]int64{"searches": 2},
	})
	if *facts.PromptTokens != 10 || *facts.CompletionTokens != 20 || *facts.CacheReadTokens != 3 {
		t.Fatalf("数量该原样给出，实际 %+v", facts)
	}
	// 零值项归一成 nil：0 与「上游没报这一项」是两件事。
	if facts.CacheWriteTokens != nil || facts.ReasoningTokens != nil {
		t.Errorf("没报的项该是 nil，实际 %+v", facts)
	}
	if facts.Units["searches"] != 2 {
		t.Errorf("非 token 计量该给，实际 %v", facts.Units)
	}
}

// 重建回协议层用量时，计费口径必须跟着一起重建。
//
// 这条断言的是**具体金额**：同一批 token 被收两次时，金额只是偏高，
// 而「不双收的那次更便宜」在把 60 写成 100 时照样成立——相对断言抓不住它。
func TestChannelUsageKeepsReasoningScope(t *testing.T) {
	// 内核报的量：100 个输出，其中 40 个是推理。插件上报了它看到的补全量，
	// 但**没有声明口径**——于是口径只能来自重建时显式给出的那一个。
	merged, err := MergeUsage(
		UsageFacts{CompletionTokens: ptrInt64(100), ReasoningTokens: ptrInt64(40)},
		&UsageFacts{CompletionTokens: ptrInt64(100)},
		// 上界恰好等于上报值：边界算在界内（判据是「超过」而不是「达到」）。
		UsageBound{MaxOutputTokens: 100},
	)
	if err != nil {
		t.Fatalf("合并用量失败: %v", err)
	}
	prices := billing.Prices{
		Completion: money.MustParse("0.000008"),
		Reasoning:  money.MustParse("0.00001"),
	}

	// 声明「已含」：拆成 60 个输出 + 40 个推理。
	// 60*8e-6 + 40*1e-5 = 0.00088
	got := billing.ComputeWithCharges(billing.FromChannelUsage(merged.ChannelUsage(true)), prices, billing.Charges{})
	if got.Total.Cmp(money.MustParse("0.00088")) != 0 {
		t.Errorf("声明「已含」时总额 = %s，期望 0.00088", got.Total)
	}

	// 声明「不含」：两项各算各的，100*8e-6 + 40*1e-5 = 0.0012。
	// 多出来的 0.00032 正是那 40 个 token 被收的第二遍——口径丢成零值时
	// 得到的就是这个数，而它不会报错，只会在账单上偏高。
	got = billing.ComputeWithCharges(billing.FromChannelUsage(merged.ChannelUsage(false)), prices, billing.Charges{})
	if got.Total.Cmp(money.MustParse("0.0012")) != 0 {
		t.Errorf("声明「不含」时总额 = %s，期望 0.0012", got.Total)
	}
}

// 插件自己声明了口径就以它为准：它比内核更懂自己代理的那个上游。
func TestChannelUsagePrefersDeclaredScope(t *testing.T) {
	cases := []struct {
		name     string
		core     *bool
		reported *bool
		fallback bool
		want     bool
	}{
		{"都没声明，沿用内核口径", nil, nil, true, true},
		{"都没声明，内核口径为不含", nil, nil, false, false},
		{"插件声明已含，覆盖内核的不含", ptrBool(false), ptrBool(true), false, true},
		{"插件声明不含，覆盖内核的已含", ptrBool(true), ptrBool(false), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := MergeUsage(UsageFacts{CompletionIncludesReasoning: tc.core}, &UsageFacts{CompletionIncludesReasoning: tc.reported}, UsageBound{MaxOutputTokens: 64})
			if err != nil {
				t.Fatalf("只声明口径的上报不该越界: %v", err)
			}
			if got := merged.ChannelUsage(tc.fallback); got.CompletionIncludesReasoning != tc.want {
				t.Errorf("口径 = %v，期望 %v", got.CompletionIncludesReasoning, tc.want)
			}
		})
	}
}

// 重建出来的用量与事实不共用那张计量表：事实还可能被调用方持有，
// 而对一边的改动不该静默出现在另一边。
func TestChannelUsageCopiesUnits(t *testing.T) {
	facts := UsageFacts{Units: map[string]int64{"searches": 2}}
	got := facts.ChannelUsage(false)
	got.UnitCounts["searches"] = 5
	if facts.Units["searches"] != 2 {
		t.Errorf("事实里的计量项被重建结果改掉了: %v", facts.Units)
	}
}

func ptrFloat64(v float64) *float64 { return &v }

func ptrInt64(v int64) *int64 { return &v }

func ptrBool(v bool) *bool { return &v }

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

// 上游没报用量时给一个各项为 nil 的对象，而不是 nil 本身：
// 插件要能区分「上游没报」与「用量是零」。
func TestResponseContextAlwaysHasUsageObject(t *testing.T) {
	rc := ResponseContextFrom("r1", &channel.Response{UpstreamModel: "m", FinishReason: "stop"})
	if rc.Usage == nil {
		t.Fatal("usage 不该是 nil 对象")
	}
	if rc.Usage.PromptTokens != nil {
		t.Errorf("上游没报时各项该是 nil，实际 %v", *rc.Usage.PromptTokens)
	}
	// resp 为 nil 时也不 panic。
	if got := ResponseContextFrom("r1", nil); got.RequestID != "r1" || got.Usage != nil {
		t.Errorf("nil 响应该给出只有 requestId 的上下文，实际 %+v", got)
	}
}

// 补丁应用：只落标量字段，且不改动无关字段。
func TestApplyRequestPatchScalars(t *testing.T) {
	req := &channel.Request{Model: "上游名", Stream: true, MaxTokens: 100}
	mt, temp, topP := int64(50), 0.3, 0.8
	rejected, err := ApplyRequestPatch(req, &RequestPatch{Body: &RequestBodyPatch{
		MaxTokens: &mt, Temperature: &temp, TopP: &topP,
	}})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(rejected) != 0 {
		t.Errorf("没有头要被拒，实际 %v", rejected)
	}
	if req.MaxTokens != 50 || *req.Temperature != 0.3 || *req.TopP != 0.8 {
		t.Fatalf("标量字段没落上: %+v", req)
	}
	// 模型与 stream 改不了：RequestBodyPatch 里没有它们。
	if req.Model != "上游名" || !req.Stream {
		t.Errorf("模型与 stream 不该被改动: %+v", req)
	}
}

func TestApplyRequestPatchRejectsBadMaxTokens(t *testing.T) {
	req := &channel.Request{MaxTokens: 100}
	mt := int64(0)
	if _, err := ApplyRequestPatch(req, &RequestPatch{Body: &RequestBodyPatch{MaxTokens: &mt}}); err == nil {
		t.Fatal("max_tokens 为 0 时该报错")
	}
	if req.MaxTokens != 100 {
		t.Errorf("报错时不该改动请求，实际 %d", req.MaxTokens)
	}
}

// max_tokens 只许调小，这条路径（补丁 → channel.Request）也得拦。
//
// 校验在两条应用路径上各有一份：这一条与 patch_test.go 里那条是**同一个
// 判据的两处落点**，不是重复。只拦一条的话，接线时用哪条就把洞开在哪条上
// ——而这条路径写的正是随后进计费的那个 channel.Request。
func TestApplyRequestPatchMaxTokensOnlyShrinks(t *testing.T) {
	cases := []struct {
		name     string
		original int
		patched  int64
		wantErr  bool
	}{
		{"调小放行", 100, 40, false},
		{"持平放行", 100, 100, false},
		{"调大被拒", 100, 101, true},
		{"客户端没给 max_tokens 时补丁给一个也算调大", 0, 10, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &channel.Request{Model: "m", MaxTokens: tc.original}
			mt := tc.patched
			_, err := ApplyRequestPatch(req, &RequestPatch{Body: &RequestBodyPatch{MaxTokens: &mt}})
			if tc.wantErr {
				if !errors.Is(err, ErrPatchScope) {
					t.Fatalf("本该被拒，实际 err=%v", err)
				}
				if !strings.Contains(err.Error(), "只许调小") {
					t.Errorf("报错该说清只许调小，实际 %q", err)
				}
				if req.MaxTokens != tc.original {
					t.Errorf("报错时不该改动请求，实际 %d", req.MaxTokens)
				}
				return
			}
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if req.MaxTokens != int(tc.patched) {
				t.Errorf("max_tokens = %d，期望 %d", req.MaxTokens, tc.patched)
			}
		})
	}
}

// 认得但不应用的 5 个字段在**接线用的这条路径**上同样被拒。
//
// 与 patch_test.go 里那条（ApplyRequestBodyPatch）是同一个判定的两处落点：
// 只拦一条的话，接线时用哪条就把洞开在哪条上。而这两条路径今天**覆盖并不
// 相同**——宽的那条（协议中立视图）曾经会写 messages / tools / tool_choice，
// 窄的这条一个都不写、也不报错，于是同一个补丁在两条路上是两个结果。
//
// 这里直接构造 RequestBodyPatch 而不是从 JSON 解：要证明的是**应用这一步**
// 的判定，解析那一步的行为由 patch_test.go 钉住。
func TestApplyRequestPatchRejectsUnappliedFields(t *testing.T) {
	system := "你是助手"
	cases := map[string]*RequestBodyPatch{
		"messages":    {Messages: []any{map[string]any{"role": "user"}}},
		"system":      {System: &system},
		"stop":        {Stop: []string{"\n\n"}},
		"tools":       {Tools: []any{map[string]any{"name": "t"}}},
		"tool_choice": {ToolChoice: "auto"},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req := &channel.Request{Model: "m", MaxTokens: 1000}
			_, err := ApplyRequestPatch(req, &RequestPatch{Body: body})
			if !errors.Is(err, ErrPatchScope) {
				t.Fatalf("%s 该被拒，实际 err=%v", name, err)
			}
			// 报错要点出字段名——插件作者拿到的就是这句话。
			if !strings.Contains(err.Error(), name) {
				t.Errorf("报错该点出 %s，实际 %q", name, err)
			}
			if req.MaxTokens != 1000 || req.Temperature != nil || req.TopP != nil {
				t.Errorf("报错时不该改动请求，实际 %+v", req)
			}
		})
	}
}

// 混着被应用与被拒字段的补丁在这条路径上也是**整份不生效**：
// 能应用的那几个也不写下去。
//
// 「先写一半再报错」会让调用方手上留着一份被改了一半的请求，而它随后
// 要么进计费、要么被重试——两种都会把「插件到底改了什么」变成一个
// 要看执行顺序才能回答的问题。
func TestApplyRequestPatchRejectsMixedPatchWholesale(t *testing.T) {
	mt, temp := int64(10), 0.1
	req := &channel.Request{Model: "m", MaxTokens: 1000}
	_, err := ApplyRequestPatch(req, &RequestPatch{Body: &RequestBodyPatch{
		MaxTokens:   &mt,
		Temperature: &temp,
		Messages:    []any{map[string]any{"role": "user"}},
	}})
	if !errors.Is(err, ErrPatchScope) {
		t.Fatalf("混了被拒字段的补丁该被拒，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "messages") {
		t.Errorf("报错该点出被拒的字段，实际 %q", err)
	}
	if req.MaxTokens != 1000 || req.Temperature != nil {
		t.Errorf("整份不生效：max_tokens 与 temperature 都不该落上，实际 %+v", req)
	}
}

// 补丁里的头经白名单过滤后落到请求上，被拒的报出来。
func TestApplyRequestPatchHeaders(t *testing.T) {
	req := &channel.Request{Headers: map[string]string{"X-Larkax-Host": "宿主"}}
	rejected, err := ApplyRequestPatch(req, &RequestPatch{Headers: map[string]string{
		"x-larkax-tenant": "t1",
		"authorization":   "冒充",
	}})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(rejected) != 1 || rejected[0] != "authorization" {
		t.Fatalf("被拒清单 = %v", rejected)
	}
	if req.Headers["x-larkax-tenant"] != "t1" || req.Headers["X-Larkax-Host"] != "宿主" {
		t.Errorf("头合并结果 = %v", req.Headers)
	}
}

func TestApplyRequestPatchNil(t *testing.T) {
	req := &channel.Request{Model: "m"}
	rejected, err := ApplyRequestPatch(req, nil)
	if err != nil || rejected != nil || req.Model != "m" {
		t.Fatalf("nil 补丁该什么都不做: %v %v %+v", rejected, err, req)
	}
	// nil 请求也不能 panic。
	if _, err := ApplyRequestPatch(nil, &RequestPatch{}); err != nil {
		t.Errorf("nil 请求不该报错: %v", err)
	}
}

// ── 遍历辅助 ──────────────────────────────────────────────────────────

// credentialKeys 是**不允许出现在插件上下文里**的键名。
//
// 精确匹配：`max_tokens` 是一个正当的请求参数，子串匹配会把它误报，
// 而误报的代价是这条检查第三次之后就没人在看了（LESSONS 第 12 条）。
var credentialKeys = map[string]bool{
	"credential":    true,
	"credentials":   true,
	"apikey":        true,
	"api_key":       true,
	"key":           true,
	"secret":        true,
	"password":      true,
	"authorization": true,
	"token":         true,
	"access_token":  true,
}

// contextJSON 把上下文序列化成插件真正会看到的那份 JSON。
//
// 用它而不是反射去数字段：**插件看到的就是这份文本**，所以「文本里没有
// 凭证」才是要断言的那件事。反射会漏掉「结构体里有一个字段但值是零值」
// 这种情况，而那种情况恰恰是「加了个凭证字段但暂时没填」——
// 正是要被发现的那一种。
func contextJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("上下文无法序列化: %v", err)
	}
	return string(b)
}

// walkKeys 取出序列化文本里出现的全部键名。
func walkKeys(t *testing.T, v any) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(contextJSON(t, v)), &doc); err != nil {
		t.Fatalf("上下文不是一个对象: %v", err)
	}
	var out []string
	var collect func(any)
	collect = func(x any) {
		switch y := x.(type) {
		case map[string]any:
			for k, sub := range y {
				out = append(out, k)
				collect(sub)
			}
		case []any:
			for _, sub := range y {
				collect(sub)
			}
		}
	}
	collect(doc)
	return out
}

// walkStrings 取出序列化文本里的全部字符串值。
func walkStrings(t *testing.T, v any) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(contextJSON(t, v)), &doc); err != nil {
		t.Fatalf("上下文无法序列化: %v", err)
	}
	var out []string
	var collect func(any)
	collect = func(x any) {
		switch y := x.(type) {
		case string:
			out = append(out, y)
		case map[string]any:
			for _, sub := range y {
				collect(sub)
			}
		case []any:
			for _, sub := range y {
				collect(sub)
			}
		}
	}
	collect(doc)
	return out
}
