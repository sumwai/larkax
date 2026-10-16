package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 settings 表的**写入**路径，与 pg.go 的读取路径成对。
//
// # 为什么写入必须按键名对照 Registry
//
// 库里出现一个 Registry 没声明的键，表现是**配置被静默忽略**：Load 在启动时
// 报错（见 Load 的注释），而写下它的人当场看不到任何反馈，只会觉得
// 「我明明设了」。所以未知的键在这里就拒掉，让它根本进不了库。
//
// 这一条同时也是「密钥类配置不进 settings」的落点：写入只认 Registry 里
// 声明过的键，而那里一个凭证类的键都没有。想往里塞 `upstream.api_key`
// 的人会被挡在门外，挡他的不是一张黑名单，是这张白名单。
//
// # 为什么不动 description 列
//
// 写入只改 value / updated_by / updated_at。description 由迁移负责，
// 那里写的是完整说明；Registry 里的 Description 是同一件事的短句。
// 拿短句覆盖长句是把信息丢掉，而这一列存在的意义正是
// 「日久之后还有人知道这个键是干嘛的」。

// 写入被拒绝的两种原因。
//
// 分开是因为下一步动作完全不同：ErrUnknownKey 要改键名（或先去 Registry 里
// 声明它），ErrNotEditable 要去写一条迁移。合成一句「写入失败」的话，
// 这两种情况看起来一样，而它们该做的事没有一件是共同的。
var (
	// ErrUnknownKey 表示这个键在库里没有对应的行。
	ErrUnknownKey = errors.New("settings 里没有这个键")
	// ErrNotEditable 表示这一行的 editable 为 false，不接受这条路径的写入。
	ErrNotEditable = errors.New("这个键不允许改")
)

// Setter 写入一个配置项。
//
// 与 Source 分开而不是合成一个接口：读的调用方（启动路径、对账）不该顺手
// 拿到写的入口，而「谁能写」这个问题在签名上就答完了，不必去读实现。
type Setter interface {
	// Set 把 key 的值改成 value，并记下是谁改的。
	//
	// value 是 jsonb 的原文，由 EncodeValue 从明文值编码而来。
	//
	// updatedBy 是改这个配置的 users.id，必须是正整数。这一列存在的意义是
	// 审计，而允许留空的话，最省事的用法就是永远不填——那一列于是永远是
	// 空的，等于没有。
	Set(ctx context.Context, key string, value json.RawMessage, updatedBy int64) error
}

// EncodeValue 把一个明文值按 key 声明的类型编码成 jsonb 的值。
//
// 命令行走这个函数，是为了让参数检查发生在**连库之前**：一个写错的命令
// 不该先连上生产库再报错，而「连上库之后才发现参数不对」还有一个更坏的
// 可能——它没报错。
//
// 收明文而不是 JSON：让人手写 `"CNY"` 这种带引号的值，等于给每个人一次
// 把字符串配置写成裸 CNY 的机会，而那个笔误要到服务下一次重启、Load 报错
// 时才现形——离写下它的那一刻已经很远了。
func EncodeValue(key, plain string) (json.RawMessage, error) {
	spec, ok := Lookup(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q。键名要先在 settings.Registry 里声明", ErrUnknownKey, key)
	}

	switch spec.Kind {
	case KindInt:
		n, err := strconv.ParseInt(strings.TrimSpace(plain), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("settings.%s 是整数，%q 不是", key, plain)
		}
		return json.Marshal(n)

	case KindBool:
		// 只认 true / false 两个词，不认 1 / 0。这一列旁边就是整数配置，
		// 让 1 与 0 在两种类型上都成立，读的人就得多想一步才知道在看什么。
		switch strings.TrimSpace(plain) {
		case "true":
			return json.Marshal(true)
		case "false":
			return json.Marshal(false)
		}
		return nil, fmt.Errorf("settings.%s 是布尔值，只接受 true 或 false，得到 %q", key, plain)

	case KindString:
		return json.Marshal(plain)
	}

	// Registry 里声明了一个本函数不认识的类型。走到这里说明加配置项时
	// 只加了声明、没加编码——报出来，别退回一个「大概能用」的值。
	return nil, fmt.Errorf("settings.%s 声明的类型 %q 未知", key, spec.Kind)
}
