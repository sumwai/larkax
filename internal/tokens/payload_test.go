package tokens

import (
	"strings"
	"testing"
)

// 本文件里的期望值**都是字面量**，不是拿别的函数算出来的：这一层要钉住的
// 就是「这个数是多少」。每个数后面写着它由哪条式子来、夹具多少字节，
// 改夹具或改估算式时按那句话重算。

// ── 工具定义 ────────────────────────────────────────────────────────

// 短键、枚举值多的 schema：这一类形状的启发式估算落在 JSON 实测密度
// （0.26 ~ 0.29 token/字符）的下沿，所以由**按字节的地板**决定结果。
//
// 变异：payloadTokens 里 `floor > n` 改成 `>=`。这一处是**等价变异**：
// 相等那一格（地板正好等于启发式估算）两版都返回同一个数——原版走
// `return n`，变异版走 `return floor`，而 `floor == n`。这不是「挑不出
// 刚好相等的输入」，是相等那一格两边返回的就是同一个值。
func TestToolDefinitionInputTokensFloorLiftsShortKeySchema(t *testing.T) {
	def := []byte(`{"name":"set_mode","description":"Set the operating mode.",` +
		`"parameters":{"type":"object","properties":{"mode":{"type":"string",` +
		`"enum":["fast","slow","balanced","auto","manual","hybrid","legacy","strict"]}}}}`)

	// ceil(0.35 × 字节数) 就是地板那一项。
	if got, want := len(def), 207; got != want {
		t.Fatalf("夹具长度 = %d，期望 %d（改夹具就要重算下面的数）", got, want)
	}
	if got, want := int(ToolDefinitionInputTokens("m-1", def)), 99; got != want {
		t.Errorf("ToolDefinitionInputTokens = %d，期望 %d", got, want)
	}
	// 地板确实赢了，否则这条用例测的是另一条路。
	if est := EstimatedTokens("m-1", string(def)); est >= 73 {
		t.Errorf("启发式估算 = %d，不再低于地板 73——这条用例已经测不到地板", est)
	}
}

// 长英文描述的 schema：这一侧由启发式估算决定（它比地板高），地板仍要参与比较。
func TestToolDefinitionInputTokensKeepsLongDescription(t *testing.T) {
	def := []byte(`{"name":"search_docs","description":"` +
		strings.Repeat("Look up the matching documents and return the best hits. ", 12) +
		`","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}`)

	if got, want := len(def), 795; got != want {
		t.Fatalf("夹具长度 = %d，期望 %d（改夹具就要重算下面的数）", got, want)
	}
	if got, want := int(ToolDefinitionInputTokens("m-1", def)), 327; got != want {
		t.Errorf("ToolDefinitionInputTokens = %d，期望 %d", got, want)
	}
	// 这一条反过来：启发式高于地板 ceil(0.35 × 795) = 279。
	if est := EstimatedTokens("m-1", string(def)); est <= 279 {
		t.Errorf("启发式估算 = %d，不再高于地板 279——这条用例已经测不到启发式那一侧", est)
	}
}

// 没有内容时仍留着包装开销：上游照样要为一条空声明的框架付 token。
func TestToolDefinitionInputTokensEmptyKeepsFrame(t *testing.T) {
	if got := ToolDefinitionInputTokens("m-1", nil); got != int64(ToolDefinitionReserveFrameTokens) {
		t.Errorf("空定义 = %d，期望只有框架开销 %d", got, ToolDefinitionReserveFrameTokens)
	}
	// 框架开销本身也钉一次：它被顺手改掉时这里要知道。
	if ToolDefinitionReserveFrameTokens != 26 {
		t.Errorf("ToolDefinitionReserveFrameTokens = %d，期望 26", ToolDefinitionReserveFrameTokens)
	}
	// **两条路刻意不是同一个数**：流式中断那条路拿它去计费（估高=凭空费用），
	// 预留拿它去冻余额（估低=差额由平台承担），方向相反。谁把两者「统一」成
	// 一个数，就必然让其中一边吃错方向——这条断言就是为了让那一次改动红。
	if ToolDefinitionReserveFrameTokens <= ToolDefinitionFrameTokens {
		t.Errorf("预留侧的包装开销 %d 不大于计费侧的下界 %d，两者已经不是一贯的保守方向",
			ToolDefinitionReserveFrameTokens, ToolDefinitionFrameTokens)
	}
}

// ── 历史消息里的 tool_use ────────────────────────────────────────────

// 短键、枚举值多的参数：与工具定义那一类同形，结果同样由**按字节的地板**决定。
func TestToolUseInputTokensFloorLiftsShortKeyArguments(t *testing.T) {
	block := []byte(`{"id":"call_1","name":"set_mode","input":{"auto":true,"balanced":true,` +
		`"fast":true,"hybrid":true,"legacy":true,"manual":true,"slow":true,"strict":true}}`)

	if got, want := len(block), 151; got != want {
		t.Fatalf("夹具长度 = %d，期望 %d（改夹具就要重算下面的数）", got, want)
	}
	// 12 是每块的包装（ToolUseReserveFrameTokens），53 是地板 ceil(0.35 × 151)。
	if got, want := int(ToolUseInputTokens("m-1", block)), 65; got != want {
		t.Errorf("ToolUseInputTokens = %d，期望 %d", got, want)
	}
	// 地板确实赢了，否则这条用例测的是另一条路。
	if est := EstimatedTokens("m-1", string(block)); est >= 53 {
		t.Errorf("启发式估算 = %d，不再低于地板 53——这条用例已经测不到地板", est)
	}
}

// 长文本参数：这一侧由启发式估算决定（它比地板高），地板仍要参与比较。
func TestToolUseInputTokensKeepsLongArguments(t *testing.T) {
	block := []byte(`{"id":"call_1","name":"set_mode","input":{"note":"` +
		strings.Repeat("Set the operating mode now. ", 20) + `"}}`)

	if got, want := len(block), 613; got != want {
		t.Fatalf("夹具长度 = %d，期望 %d（改夹具就要重算下面的数）", got, want)
	}
	// 252 是启发式估算，地板 ceil(0.35 × 613) = 215 低于它。
	if got, want := int(ToolUseInputTokens("m-1", block)), 264; got != want {
		t.Errorf("ToolUseInputTokens = %d，期望 %d", got, want)
	}
	if est := EstimatedTokens("m-1", string(block)); est <= 215 {
		t.Errorf("启发式估算 = %d，不再高于地板 215——这条用例已经测不到启发式那一侧", est)
	}
}

// 没有参数（工具不带参数）时仍留着包装开销：上游照样要为 id 与 name 计 token。
func TestToolUseInputTokensEmptyKeepsFrame(t *testing.T) {
	if got := ToolUseInputTokens("m-1", nil); got != int64(ToolUseReserveFrameTokens) {
		t.Errorf("空参数 = %d，期望只有包装开销 %d", got, ToolUseReserveFrameTokens)
	}
	// 包装开销本身也钉一次：它被顺手改掉时这里要知道。
	if ToolUseReserveFrameTokens != 12 {
		t.Errorf("ToolUseReserveFrameTokens = %d，期望 12", ToolUseReserveFrameTokens)
	}
	// **两条路刻意不是同一个数**：流式中断那条路拿它去计费（估高=凭空费用），
	// 预留拿它去冻余额（估低=差额由平台承担），方向相反。谁把两者「统一」成
	// 一个数，就必然让其中一边吃错方向——这条断言就是为了让那一次改动红。
	if ToolUseReserveFrameTokens <= ToolCallFrameTokens {
		t.Errorf("预留侧的包装开销 %d 不大于计费侧的下界 %d，两者已经不是一贯的保守方向",
			ToolUseReserveFrameTokens, ToolCallFrameTokens)
	}
}

// ── 图片 ────────────────────────────────────────────────────────────

// 这个数是一个常数，所以断言只能是被钉住的字面量。它的依据（两家在
// 1024×1024 上的公开公式）写在常量自己的注释里；这里钉的是它没有被顺手改掉，
// 以及它高过那一档的口径。
func TestImageInputTokensPinned(t *testing.T) {
	if ImageInputTokens != 1400 {
		t.Errorf("ImageInputTokens = %d，期望 1400", ImageInputTokens)
	}
	if ImageInputTokens <= 765 {
		t.Errorf("ImageInputTokens = %d，低于 OpenAI 在 1024×1024 上的 765", ImageInputTokens)
	}
}
