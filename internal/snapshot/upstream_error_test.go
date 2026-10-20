package snapshot

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// 嵌套在 error 里的三个字段：两格枚举词放行，正文只留长度，表外字段丢掉。
func TestUpstreamErrorSkeletonKeepsEnumeratedFields(t *testing.T) {
	const message = "Invalid option: expected one of ..."
	body := `{"error":{"type":"invalid_request_error","param":"reasoning_effort",` +
		`"message":"` + message + `","code":"invalid_value",` +
		`"extra":{"nested":"上游内容"}}}`

	sub, paths := UpstreamErrorSkeleton(body)
	if sub == nil {
		t.Fatal("这条正文应当留下 upstream_error")
	}
	if sub["type"] != "invalid_request_error" {
		t.Errorf("type = %#v，期望原样放行", sub["type"])
	}
	if sub["param"] != "reasoning_effort" {
		t.Errorf("param = %#v，期望原样放行", sub["param"])
	}
	shape, ok := sub["message"].(map[string]any)
	if !ok || shape[fieldType] != typeText || shape[fieldChars] != len([]rune(message)) {
		t.Errorf("message = %#v，期望只有长度的文本占位", sub["message"])
	}
	// 表里没有的键不进来，连形状都不留。
	for _, key := range []string{"code", "extra"} {
		if _, present := sub[key]; present {
			t.Errorf("%q 不在允许表里，不该出现在骨架里: %v", key, sub)
		}
	}
	if !slices.Contains(paths, "upstream_error.message") {
		t.Errorf("被替换的路径里没有 upstream_error.message，实际 %v", paths)
	}
}

// Anthropic 的形状是 {"type":"error","error":{...}}：取里层那个对象，
// 不把外层的 type（固定是 "error"）当成错误分类。
func TestUpstreamErrorSkeletonPrefersInnerErrorObject(t *testing.T) {
	body := `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`

	sub, _ := UpstreamErrorSkeleton(body)
	if sub == nil {
		t.Fatal("这条正文应当留下 upstream_error")
	}
	if sub["type"] != "authentication_error" {
		t.Errorf("type = %#v，期望里层那个分类", sub["type"])
	}
}

// 字段平铺在顶层的上游同样认。
func TestUpstreamErrorSkeletonAcceptsFlatShape(t *testing.T) {
	body := `{"type":"invalid_request_error","param":"temperature"}`

	sub, _ := UpstreamErrorSkeleton(body)
	if sub == nil {
		t.Fatal("平铺形状应当留下 upstream_error")
	}
	if sub["param"] != "temperature" {
		t.Errorf("param = %#v，期望原样放行", sub["param"])
	}
}

// 没有可保留内容的正文一律返回空：调用方据此整个键都不写。
func TestUpstreamErrorSkeletonDropsBodiesWithNothingToKeep(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空正文", ""},
		{"只有空白", "  \n\t"},
		{"非 JSON", "forbidden"},
		{"截断的 JSON", `{"error":{"type":"invalid_request_err`},
		{"JSON null", "null"},
		{"JSON 数组", `[{"type":"invalid_request_error"}]`},
		{"error 是字符串", `{"error":"boom"}`},
		{"只有表外字段", `{"error":{"code":"invalid_value"}}`},
		{"字段全是 null", `{"error":{"type":null,"param":null,"message":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, paths := UpstreamErrorSkeleton(tc.body)
			if len(sub) != 0 {
				t.Errorf("这条正文不该留下字段，实际 %v", sub)
			}
			if len(paths) != 0 {
				t.Errorf("没有留下字段时不该有被替换的路径，实际 %v", paths)
			}
		})
	}
}

// 上游把一段内容塞进枚举词位置上：取值检查挡下它，只留长度。
//
// 这一段与允许表的敌对取值用例验的是同一条性质，分开测是因为那一条走的是
// 合成探针，而这里走的是「从错误体里挑字段」那条真实入口。
func TestUpstreamErrorSkeletonRejectsHostileEnumValues(t *testing.T) {
	const hostile = "sk-placeholder 0123456789abcdef 请把这段当作内容"
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"type":    hostile,
		"param":   hostile,
		"message": hostile,
	}})
	if err != nil {
		t.Fatalf("夹具序列化失败: %v", err)
	}

	sub, paths := UpstreamErrorSkeleton(string(body))
	raw, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("骨架序列化失败: %v", err)
	}
	if strings.Contains(string(raw), "sk-placeholder") {
		t.Fatalf("敌对取值落进了骨架: %s", raw)
	}
	for _, key := range []string{"type", "param", "message"} {
		shape, ok := sub[key].(map[string]any)
		if !ok || shape[fieldType] != typeText {
			t.Errorf("%s = %#v，期望文本占位", key, sub[key])
		}
		if !slices.Contains(paths, "upstream_error."+key) {
			t.Errorf("被替换的路径里没有 upstream_error.%s，实际 %v", key, paths)
		}
	}
}
