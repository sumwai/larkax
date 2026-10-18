package gateway

// 这个文件只钉 #560 加的那一层判断：哪一次结局配得上在**已计费的行**上留一条
// 上游侧的依据（见 stream.go 的 billedStreamEvidence）。
//
// 它与既有的流式用例分开写，是因为判据不是同一件事：那些钉的是快照说了什么
// （两表里只有快照那一侧曾经有值），这里钉的是账单说了什么。两表可以各说各的
// 而不报错，所以必须分开钉；而「已计费的行上该不该有值」这条判断错一格，除了
// 这里没有别的地方会露出来——快照那份判据是另一个函数，两边都写宽了仍然互相对
// 得上。
//
// 落库那一侧（markSettled 真的把这两列写进去）不在这一层：那要连库，见
// internal/assembly 的 TestStreamTruncatedAfterOutputRecordsSameClassification
// 与 internal/ledger/store 的 TestSettlementWritesUpstreamEvidenceVerbatim。

import (
	"context"
	"net/http"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 只有静默截断那一种结局在账单上有依据，其余一律给零值（落库时折成 NULL）。
//
// 五种情形都列出来，是因为它们在这一层的差别正是那条判断的全部内容：漏掉一种
// 就等于把「上游出过事」与「只有账单知道的事」混成一件事。
func TestBilledStreamEvidenceOnlyForTruncation(t *testing.T) {
	const upstreamOK = http.StatusOK

	cases := []struct {
		name string
		// streamErr 与那三位布尔量的含义见 streamTruncated。
		streamErr           error
		started, done, ours bool
		wantStatus          int
		wantCode            string
	}{
		{
			name:       "静默截断：吐过字、没有结束标记、没有报错、不是我们停的",
			started:    true,
			wantStatus: upstreamOK,
			wantCode:   pipeline.ErrorUpstreamStreamTruncated,
		},
		{
			name:    "正常结束：上游给了结束标记",
			started: true,
			done:    true,
		},
		{
			// 这一次不计费，依据走 Release 那条路（见 pumpStream 前半段），
			// 到不了这里；这里给零值是为了让「走错了路也不会把依据写歪」。
			name: "一个字都没吐就结束：不计费的结局",
		},
		{
			// 客户端断开、插件终止、余额不足的中断都是这一种：停下这条流的
			// 是我们。写进去等于把我们的动作记到上游头上。
			name:    "客户端断开：停下这条流的是我们（ours）",
			started: true,
			ours:    true,
		},
		{
			// 这一种**有**依据可写（错误里带着上游的状态码），但仍然不写：
			// 错误帧当时就送到调用方手里了，账单不必再讲一遍。
			name:      "吐字之后上游在流里报错：有依据，但账单不写",
			streamErr: &channel.ErrUpstream{StatusCode: http.StatusInternalServerError, GotResponse: true},
			started:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := billedStreamEvidence(upstreamOK, tc.streamErr, tc.started, tc.done, tc.ours)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Errorf("账单上的依据 = (%d, %q)，期望 (%d, %q)",
					status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// 截断那一次，网关交给落库层的那份 Charge 上带着依据。
//
// 上面那条钉的是判断本身，这条钉的是**它有没有被交出去**：判断对了而
// result.Charge 上还是零值的话，落库层照样写不出东西来——那正是 #560 修掉的
// 形状（判断在，落点没接）。
func TestStreamTruncationCarriesEvidenceToSettlement(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			delta("半句"), // 没有 done，也没有 error：连接就这样没了
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if res.Charge == nil {
		t.Fatal("吐过字之后截断要计费，却没有结算")
	}
	if res.Charge.ErrorCode != pipeline.ErrorUpstreamStreamTruncated {
		t.Errorf("交给落库层的 error_code = %q，期望 %q",
			res.Charge.ErrorCode, pipeline.ErrorUpstreamStreamTruncated)
	}
	// 状态码也要一起过去：那一列说的是「上游当时回了什么」，而这次它回过 200
	// （已经吐了内容）。落了空会让这一行读成「没记录到状态码」。
	if res.Charge.UpstreamStatus != http.StatusOK {
		t.Errorf("交给落库层的 upstream_status = %d，期望 %d",
			res.Charge.UpstreamStatus, http.StatusOK)
	}
}

// 正常结束的那一次，交给落库层的两列是零值。
//
// 与上面那条成对：只钉截断那一侧的话，把依据无条件写成
// 「这次的状态码 + 某个分类」也能过，而那样每一笔已计费的账单上都会有值——
// 库里再也分不出「这一行有一件只有账单知道的事」与
// 「这一行什么也没发生」。
func TestStreamNormalEndCarriesNoEvidenceToSettlement(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			delta("你好"),
			{Kind: "done", FinishReason: "stop"},
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if res.Charge == nil {
		t.Fatal("正常结束要计费，却没有结算")
	}
	if res.Charge.ErrorCode != "" || res.Charge.UpstreamStatus != 0 {
		t.Errorf("正常结束的两列应当是零值，得到 (%d, %q)",
			res.Charge.UpstreamStatus, res.Charge.ErrorCode)
	}
}
