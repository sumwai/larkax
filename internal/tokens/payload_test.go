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
func TestToolDefinitionInputTokensFloorLiftsShortKeySchema(t *testing.T) {
	def := []byte(`{"name":"set_mode","description":"Set the operating mode.",` +
		`"parameters":{"type":"object","properties":{"mode":{"type":"string",` +
		`"enum":["fast","slow","balanced","auto","manual","hybrid","legacy","strict"]}}}}`)

	// ceil(0.35 × 字节数) 就是地板那一项。
	if got, want := len(def), 207; got != want {
		t.Fatalf("夹具长度 = %d，期望 %d（改夹具就要重算下面的数）", got, want)
	}
	if got, want := int(ToolDefinitionInputTokens("m-1", def)), 81; got != want {
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
	if got, want := int(ToolDefinitionInputTokens("m-1", def)), 309; got != want {
		t.Errorf("ToolDefinitionInputTokens = %d，期望 %d", got, want)
	}
	// 这一条反过来：启发式高于地板 ceil(0.35 × 795) = 279。
	if est := EstimatedTokens("m-1", string(def)); est <= 279 {
		t.Errorf("启发式估算 = %d，不再高于地板 279——这条用例已经测不到启发式那一侧", est)
	}
}

// 没有内容时仍留着包装开销：上游照样要为一条空声明的框架付 token。
func TestToolDefinitionInputTokensEmptyKeepsFrame(t *testing.T) {
	if got := ToolDefinitionInputTokens("m-1", nil); got != int64(ToolDefinitionFrameTokens) {
		t.Errorf("空定义 = %d，期望只有框架开销 %d", got, ToolDefinitionFrameTokens)
	}
	// 框架开销本身也钉一次：它是与流式中断那条路共用的数，被改动时这里要知道。
	if ToolDefinitionFrameTokens != 8 {
		t.Errorf("ToolDefinitionFrameTokens = %d，期望 8", ToolDefinitionFrameTokens)
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
