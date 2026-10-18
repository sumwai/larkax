package snapshot

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

const (
	verbatimPrompt = "把这段话原样留下：第三页的报价是 1200"
	verbatimTool   = "查库存"
	verbatimAnswer = "报价那一段我看了：1200 是税前"
)

func verbatimRequest() *channel.Request {
	return &channel.Request{
		Model:     "gpt-4o-2024",
		MaxTokens: 512,
		Messages: []channel.Message{{
			Role:    "user",
			Content: []channel.ContentBlock{{Type: "text", Text: verbatimPrompt}},
		}},
		Tools: []channel.Tool{{Name: verbatimTool, Description: "查一下库存"}},
	}
}

func verbatimResponse() *channel.Response {
	return &channel.Response{
		UpstreamModel: "gpt-4o-2024",
		Message: channel.Message{
			Role:    "assistant",
			Content: []channel.ContentBlock{{Type: "text", Text: verbatimAnswer}},
		},
		Usage: channel.Usage{PromptTokens: 100, CompletionTokens: 20},
	}
}

// 「原样」这一档留下的必须是原文——这是它与默认那一档唯一的差别，所以
// **两档要一起判**：只判「原文留下来了」的话，两档都留原文也能过。
func TestVerbatimRequestKeepsContent(t *testing.T) {
	r := mustNew(t, RootRequest)

	res, err := r.RedactRequestVerbatim(verbatimRequest())
	if err != nil {
		t.Fatalf("原样采集失败: %v", err)
	}
	assertPresent(t, res.Skeleton, verbatimPrompt)
	assertPresent(t, res.Skeleton, verbatimTool)
	if len(res.Redacted) != 0 {
		t.Errorf("原样那一档一个字段都没替换，redacted_paths 应当是空的，实际 %v", res.Redacted)
	}
	if res.Truncated {
		t.Error("这一份没有超限，不该标成降级")
	}
	if res.Version != SkeletonVersion {
		t.Errorf("skeleton_version = %d，期望 %d", res.Version, SkeletonVersion)
	}

	// 同一份请求走默认那一档：原文一个都不该出现。
	plain, err := r.RedactRequest(verbatimRequest())
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	assertAbsent(t, plain.Skeleton, verbatimPrompt)
	assertAbsent(t, plain.Skeleton, verbatimTool)
	if len(plain.Redacted) == 0 {
		t.Error("默认那一档应当记下被替换掉的路径")
	}
}

func TestVerbatimResponseKeepsContent(t *testing.T) {
	r := mustNew(t, RootResponse)

	res, err := r.RedactResponseVerbatim(verbatimResponse())
	if err != nil {
		t.Fatalf("原样采集失败: %v", err)
	}
	assertPresent(t, res.Skeleton, verbatimAnswer)

	plain, err := r.RedactResponse(verbatimResponse())
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	assertAbsent(t, plain.Skeleton, verbatimAnswer)
}

// 原文超过字节上限时降级成摘要层，并把 Truncated 标出来。
//
// 这一条是**判据**而不是巧合：原文比骨架大得多（骨架把内容换成了占位描述符），
// 所以「上限对原文一样管用」这件事必须被钉住——不管的话，一条超长提示词就能
// 把行撑到任意大，而它看起来完全正常。
//
// 方向也要钉住：降级**不是**静默丢掉（那样读的人会以为原文就这么多），
// 而是留下顶层标量 + 一个看得见的 Truncated。
func TestVerbatimOverBudgetDegradesToSummary(t *testing.T) {
	r := mustNew(t, RootRequest, WithMaxBytes(200))

	const long = "很长的一段原文"
	req := verbatimRequest()
	req.Messages[0].Content = []channel.ContentBlock{{Type: "text", Text: strings.Repeat(long, 50)}}

	res, err := r.RedactRequestVerbatim(req)
	if err != nil {
		t.Fatalf("超限时应当降级而不是报错: %v", err)
	}
	if !res.Truncated {
		t.Fatal("超限的原文必须标成降级——它是读的人唯一的出口")
	}
	assertAbsent(t, res.Skeleton, long)
	if _, ok := res.Skeleton["model"]; !ok {
		t.Errorf("摘要层要留下顶层标量，实际为 %v", res.Skeleton)
	}
}

// 摘要层仍然超限时丢整条，与骨架那条路同源判据。
func TestVerbatimSummaryOverBudgetIsError(t *testing.T) {
	r := mustNew(t, RootRequest, WithMaxBytes(1))

	if _, err := r.RedactRequestVerbatim(verbatimRequest()); !errors.Is(err, ErrRedaction) {
		t.Errorf("摘要层都放不下时应当报 ErrRedaction，得到 %v", err)
	}
}

// nil 与方向错配的判据与默认那一档逐条对齐：这两件事都是调用方的 bug，
// 报错而不是给一条空快照。
func TestVerbatimRejectsMismatchedInput(t *testing.T) {
	req := mustNew(t, RootRequest)
	resp := mustNew(t, RootResponse)

	if _, err := req.RedactRequestVerbatim(nil); !errors.Is(err, ErrRedaction) {
		t.Errorf("nil 请求应当报 ErrRedaction，得到 %v", err)
	}
	if _, err := req.RedactResponseVerbatim(verbatimResponse()); !errors.Is(err, ErrRedaction) {
		t.Errorf("请求方向的脱敏器不该接受响应，得到 %v", err)
	}
	if _, err := resp.RedactRequestVerbatim(verbatimRequest()); !errors.Is(err, ErrRedaction) {
		t.Errorf("响应方向的脱敏器不该接受请求，得到 %v", err)
	}
}

// assertPresent 断言一段内容出现在骨架里——「原样那一档」的判据。
//
// 按「在不在里面」判而不是按路径去取某个字段：原文落在哪个键上由允许表决定，
// 而这里要判的是**它有没有被替换掉**，那件事与它落在哪个键上无关。
// 与 assertAbsent 同一套走法（见 allowlist_test.go）。
func assertPresent(t *testing.T, skeleton map[string]any, marker string) {
	t.Helper()
	for _, s := range collectStrings(skeleton) {
		if strings.Contains(s, marker) {
			return
		}
	}
	t.Fatalf("骨架里没有出现 %q，实际为 %v", marker, skeleton)
}
