package snapshot

import (
	"strings"
	"sync"
)

// upstreamErrorKey 是上游错误体在骨架里占的键名。
const upstreamErrorKey = "upstream_error"

// upstreamErrorRedactor 专门处理 upstream_error 这棵子树。
//
// 它用 chat 响应方向的允许表建：那三个字段的规则就住在 responseRules 里
// （见 allowlist.go 的 upstreamErrorRules），所以挑选要保留的键与逐格处理
// 读的是同一份数据。输入只有 upstream_error 一个键，表里其余条目在这里不会
// 被走到。
//
// 只建一次：允许表是包级数据，每条快照都验一遍表是白费的。建不出来时返回
// nil，调用方按「不保留」处理——这一格是附加信息，不值得为它丢掉整条快照。
var upstreamErrorRedactor = sync.OnceValue(func() *Redactor {
	r, err := NewFor(SurfaceChat, RootResponse)
	if err != nil {
		return nil
	}
	return r
})

// UpstreamErrorSkeleton 把上游错误体脱敏成骨架里的 upstream_error 子对象。
//
// 第二个返回值是被替换掉的字段路径，已带 upstream_error. 前缀，调用方把它
// 并进快照的 redacted_paths。返回的 map 为空表示这一条正文什么也不保留，
// 调用方应当整个键都不写。**空对象不行**：它读起来像是上游回了一个错误，
// 只是那个错误没有任何字段。
//
// # 哪些字段留下
//
// 由 upstreamErrorRules 决定，这里只按那张表挑键：表里没有的既不会被挑进来，
// 也就谈不上「进了脱敏器又被替换掉」。挑选与处理读同一份表，所以加一个字段
// 只需要改那一处。
//
// # 什么情况下不保留
//
// 空正文、不是 JSON 对象、对象里一个可枚举字段都没有、报文是 JSON null，
// 这几种都返回空。**截断过的报文也走这条路**：上游错误体在适配器那一层
// 已经按 httpx.MaxErrorBody 截断过，截断之后多半不再是合法 JSON，那时不保留，
// 而不是试着补全半截——半截 JSON 里剩下的内容未必还受得住「只留可枚举字段」
// 这条，而补全它等于替上游编一段它没说过的话。
func UpstreamErrorSkeleton(body string) (map[string]any, []string) {
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	parsed, err := Decode([]byte(body))
	if err != nil {
		return nil, nil
	}
	obj := upstreamErrorObject(parsed)

	picked := make(map[string]any, len(upstreamErrorRules))
	for _, rule := range upstreamErrorRules {
		key, ok := strings.CutPrefix(rule.Path, upstreamErrorKey+".")
		if !ok {
			continue
		}
		if v, present := obj[key]; present && v != nil {
			picked[key] = v
		}
	}
	if len(picked) == 0 {
		return nil, nil
	}

	r := upstreamErrorRedactor()
	if r == nil {
		return nil, nil
	}
	res, err := r.RedactRaw(map[string]any{upstreamErrorKey: picked})
	if err != nil {
		return nil, nil
	}
	sub, ok := res.Skeleton[upstreamErrorKey].(map[string]any)
	if !ok || len(sub) == 0 {
		return nil, nil
	}
	return sub, res.Redacted
}

// upstreamErrorObject 挑出承载错误字段的那个对象。
//
// OpenAI、Anthropic 与 Responses 都把字段包在 error 里；也有上游直接平铺在
// 顶层。只认对象：error 是字符串时本来就没有可枚举的字段，按「不保留」处理。
func upstreamErrorObject(body map[string]any) map[string]any {
	if inner, ok := body["error"].(map[string]any); ok {
		return inner
	}
	return body
}
