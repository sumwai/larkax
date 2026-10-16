package snapshot

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// 占位描述符里出现的那几个键。
//
// 名字前面带下划线：真实上游已经用过 "type"、"text"、"content" 这些短键，
// 撞名会让「这是占位还是真实字段」在骨架里分不出来，而分不出来的那条记录，
// 恰恰是复查白名单时最需要看清的那条。
const (
	fieldType   = "_t"
	fieldChars  = "chars"
	fieldBytes  = "bytes"
	fieldHashed = "_hashed"
)

// 占位的取值类型，落进 fieldType。
const (
	typeText    = "text"
	typeBlob    = "blob"
	typeString  = "string"
	typeNumber  = "number"
	typeBool    = "bool"
	typeNull    = "null"
	typeObject  = "object"
	typeArray   = "array"
	typeUnknown = "unknown"
)

// textPlaceholder 是文本类内容的占位：只留类型与字符数。
//
// 长度是复现所需的最小信息（截断类错误、上下文超限都靠它），而内容不是。
// 按 rune 计数而不是字节：中文内容的字节数是字符数三倍，
// 按字节记会让「这条消息有多长」整体偏大。
func textPlaceholder(s string) map[string]any {
	return map[string]any{fieldType: typeText, fieldChars: len([]rune(s))}
}

// blobPlaceholder 是二进制数据的占位。
//
// 形状是 `{"type":"image","media_type":"image/png","bytes":204800}`：
// 块的类型留在它自己的键名下，而不是折进 `_t`。
// 理由是块类型序列属于「形状」——复现时要看的第一件事就是
// 「第几块是图片、第几块是文本」，把类型藏进占位符会让这份序列读不出来。
//
// base64 的字符数是编码后的长度，与真实数据量差一个常数因子，
// 排查「上传的图到底多大」时对不上账，所以这里换算成字节数。
func blobPlaceholder(blockType, mediaType, data string) map[string]any {
	out := map[string]any{
		"type":     blockType,
		fieldBytes: len([]byte(mediaType)) + decodedLen(data),
	}
	if mediaType != "" {
		out["media_type"] = mediaType
	}
	return out
}

// describeScalar 把任意标量换成「类型 + 长度」的描述符。
//
// 这是工具参数里每个值走的路：键名保留（schema 校验类错误必须看到键名才能复现），
// 值不留。数字与布尔的取值本身也与类型分不开，整体换成类型描述符。
func describeScalar(v any) map[string]any {
	switch t := v.(type) {
	case nil:
		return map[string]any{fieldType: typeNull}
	case bool:
		return map[string]any{fieldType: typeBool}
	case string:
		return textPlaceholder(t)
	default:
		return map[string]any{fieldType: scalarKind(v)}
	}
}

// scalarKind 给非字符串标量一个归一种类。
//
// 数字不细分整数与浮点：那点差别对复现没有帮助，而 JSON 数字解析后本来就是
// float64，再分一次只会让人以为拿到的精度是可信的。
func scalarKind(v any) string {
	switch v.(type) {
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return typeNumber
	default:
		// 走到这里说明遇到了一种本包不认识的 JSON 标量。不报错：
		// 它的类型名本身就是有用的形状信息，而报错会让整条快照白白丢掉。
		return typeUnknown
	}
}

// isScalar 判断取值是否是 JSON 标量。
func isScalar(v any) bool {
	switch v.(type) {
	case nil, bool, string:
		return true
	default:
		return scalarKind(v) == typeNumber
	}
}

// shapeOf 给出一个不保留取值的形状。
//
// 容器返回「类型 + 元素/键数量」而不是空容器：空容器会被读成
// 「这里本来就没有东西」，而实际上是我们没留。
func shapeOf(v any) map[string]any {
	switch t := v.(type) {
	case nil:
		return map[string]any{fieldType: typeNull}
	case map[string]any:
		return map[string]any{fieldType: typeObject, "keys": len(t), "redacted": true}
	case []any:
		return map[string]any{fieldType: typeArray, "items": len(t), "redacted": true}
	default:
		return describeScalar(v)
	}
}

// redactedMask 返回通用脱敏记号的一份副本。
//
// 每次返回副本而不是共享同一个 map：骨架会被调用方拿去改、拿去序列化，
// 共享一份会让一处改动出现在所有字段上。
func redactedMask() map[string]any {
	return map[string]any{fieldType: typeString, "redacted": true}
}

// contentKeyNames 是会自己承载字符串内容的键名。
//
// 这是跨协议的兜底：只要键叫这几种之一，不管它出现在链路的哪一层、
// 允许表里有没有点名，一律按内容处理。上游换个地方回显用户输入时不必等
// 白名单跟着改，而「这个键名在别处见过，放行吧」正是漏掉内容的那一步。
var contentKeyNames = map[string]struct{}{
	"text":        {},
	"content":     {},
	"message":     {},
	"reasoning":   {},
	"thinking":    {},
	"input":       {},
	"arguments":   {},
	"description": {},
	"prompt":      {},
	"instruction": {},
}

// isContentKey 判断一个键名是否按内容处理。
func isContentKey(name string) bool {
	_, ok := contentKeyNames[strings.ToLower(name)]
	return ok
}

// decodedLen 是 base64 文本对应的字节数。
//
// 认不出 base64 时退回编码长度而不是 0：宁可这个数字不准，
// 也不能让「认不出来」表现为「内容为空」——后者会被当成一条正常记录看过去。
func decodedLen(s string) int {
	if s == "" {
		return 0
	}
	if trimmed := strings.TrimRight(s, "="); trimmed != "" && isBase64Text(trimmed) {
		return base64.StdEncoding.DecodedLen(len(s))
	}
	return len(s)
}

// isBase64Text 判断字符串是否整体落在 base64 的字符集里。
func isBase64Text(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+' || c == '/' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// charCount 是任意值的字符数：字符串数 rune，其余序列化为 JSON 后数 rune。
//
// 第二个返回值为 false 表示数不出来（取值里有无法序列化的东西）。
// 这时返回 0 而不是报错：调用方已经把这个字段记成脱敏，
// 长度不准不影响安全，报错却会让整条快照丢掉。
func charCount(v any) (int, bool) {
	if v == nil {
		return 0, true
	}
	if s, ok := v.(string); ok {
		return len([]rune(s)), true
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	return len([]rune(string(raw))), true
}

// isEnumWord 判断一段文本是否像枚举取值。
//
// 用于 error.code、finish_reason 这类位置：`invalid_request_error` 是复现
// 需要的取值，而同一位置上出现含空格、过长或含非 ASCII 的字符串时，
// 更可能是上游把用户内容回显到了那里——那种取值不能放行。
func isEnumWord(s string) bool {
	if s == "" || len(s) > 64 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '/' || r == ':':
		default:
			return false
		}
	}
	return true
}

// shortHash 是字符串的短哈希，用于表示一个不该原样留下的名字。
//
// 哈希不是加密，对低熵的名字（字典里的词）几乎没有保护作用；它在这里的作用是
// 让「两次请求带了同一组头」可以比对，同时不把头名直接写进骨架。
// 凭证类头名走的是另一条路——它们连哈希都不进（见 deniedHeaders）。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}
