// Package settings 读取数据库中的运行期配置。
//
// 为什么不放环境变量：环境变量必须跟着数据一起走。备份恢复、换机器、切环境时
// 漏了一个变量，数据就可能不可用，而这类错误往往几周后才被发现。
// 放进 settings 表后，配置与数据同生共死，恢复数据就把配置带回来了。
//
// 什么放这里：拿不到数据库时不需要的值（留存期、功能开关、额度策略）。
// 什么不放这里：连库本身需要的值（DSN、主密钥），以及每个实例可以不同的值
// （监听地址、日志级别）。密钥类配置也不放——这里的值明文落盘并进备份。
//
// 默认值只在迁移文件里写一份，代码里不再写第二份。两处默认值迟早会不一致，
// 而那种不一致极难发现：改了数据库，代码里的那份还在悄悄生效。
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Kind 是配置项的值类型。声明类型是为了在加载时就发现写错的值，
// 而不是等到某个深夜第一次读到它时才炸。
type Kind string

// 配置项的类型取值，与迁移里写入 settings 表的值对应。
const (
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	KindString Kind = "string"
)

// Spec 描述一个已知的配置项。
//
// 这份清单是代码与迁移之间的契约：迁移里插入了清单之外的键会被报出来，
// 清单里有而库里没有的键也会被报出来。两个方向都要查——
// 只查一边的话，写错键名会安静地生效为一个不存在的配置。
type Spec struct {
	Key  string
	Kind Kind

	// Unwired 非空表示这个键在代码里**还没有读者**：改了它不会有任何效果。
	// 内容是一句话说明为什么还没接上。
	//
	// 它必须出现在 settings list 的输出里。理由与整个 Registry 的用途是同一个：
	// 没有它，「改了没反应」与「改了就生效」在命令行上长得一模一样——两边都回
	// 一句「已写入」，而库里那一行也确实变了。接上线之后把它清空，
	// 那一行说明就跟着消失。
	//
	// 它是一份**声明**，不是一条自动判据：没有任何检查能证明某个键真的被读了。
	// 写在这里是因为它与键的定义在同一处——接线的人本来就在改这几行，
	// 而写在别处（例如迁移里的 description）就会在接线之后留在原地，
	// 继续说着已经不成立的话。
	Unwired string
}

// KeyAdminToken 是内部监听上管理令牌的 verifier：令牌明文的 SHA-256，
// 小写十六进制。空串表示还没配置过——那时 admin API 一律回 503，而不是
// 「没配就放开」。
//
// 它放这里而不是散在调用方，是因为 Registry 要用同一个字面量：两处各写一份
// 时，键名拼错的表现是「启动时报缺少配置项」，而那时没人会先怀疑拼写。
//
// 它**不**走 settings.Setter：写入要求一个记名的用户，而管理令牌不对应任何
// 用户；`larkaxctl admin token create|revoke` 自己写这一行。
const KeyAdminToken = "admin.token_sha256"

// Registry 是全部已知配置项。
//
// 新增配置项时：先加迁移里的 INSERT，再在这里加一行。两处都要改是有意的，
// 它保证「能读到」和「知道它是什么」不会脱节。
//
// 这里**没有**说明字段：一个键的说明只写在迁移里（settings.description 列），
// 由 settings list 打出来。再在这里补一份的话，两份文本改了一处另一处不动，
// 而改错了不会有任何反馈——那种没人读的说明比没有说明更坏。改说明就改迁移。
var Registry = []Spec{
	{Key: "request_snapshot.retention_days", Kind: KindInt},
	{Key: "request_snapshot.enabled", Kind: KindBool},
	{Key: "request_snapshot.max_bytes", Kind: KindInt},
	{Key: "capture.consent_max_window_hours", Kind: KindInt},
	{Key: "quota.probe_interval_seconds", Kind: KindInt},
	{Key: "quota.exhausted_cooldown_seconds", Kind: KindInt},
	{Key: "billing.default_asset", Kind: KindString},
	{Key: "billing.max_output_tokens", Kind: KindInt},
	{Key: "billing.stream_freeze_step", Kind: KindInt},
	{Key: "calendar.default_region", Kind: KindString},
	{Key: "idempotency.retention_hours", Kind: KindInt},
	{Key: "plugin.enabled", Kind: KindBool},
	{Key: "plugin.call_timeout_ms", Kind: KindInt},
	{Key: "plugin.memory_limit_bytes", Kind: KindInt},
	{Key: "plugin.max_payload_bytes", Kind: KindInt},
	{Key: "reconcile.auto_repair", Kind: KindBool},
	{Key: "reconcile.repair_mode", Kind: KindString},
	{Key: "reconcile.difference_account", Kind: KindString},
	{Key: "reconcile.shard_size", Kind: KindInt},
	{Key: "reconcile.full_scan_interval_hours", Kind: KindInt},
	{Key: "payment.fake_enabled", Kind: KindBool},
	{Key: KeyAdminToken, Kind: KindString},
}

// Row 是从存储层读出的一行。
type Row struct {
	Key   string
	Value []byte // jsonb 原文
}

// Source 提供配置行。把它抽成接口是为了让校验逻辑可以脱离数据库测试——
// 出错的分支（缺键、类型不对、非法 JSON）恰好是最需要测的部分。
type Source interface {
	Load(ctx context.Context) ([]Row, error)
}

// Values 是加载并校验过的配置快照。
//
// 只读且不导出内部 map：调用方拿到的是某个时刻的一致视图，
// 刷新时整体替换而不是就地修改，避免读到改了一半的配置。
type Values struct {
	raw   map[string]json.RawMessage
	byKey map[string]Spec
}

// index 把 Registry 转成按键索引，并挡住重复声明。
//
// 重复声明不报的话，后一条会安静地覆盖前一条：调用方按前者的类型读，
// 拿到的却是后者的值，而两处代码看起来都「写了」。
func index() (map[string]Spec, error) {
	byKey := make(map[string]Spec, len(Registry))
	for _, s := range Registry {
		if s.Key == "" {
			return nil, fmt.Errorf("settings.Registry 里有空的键名")
		}
		if _, dup := byKey[s.Key]; dup {
			return nil, fmt.Errorf("settings.Registry 里 %q 声明了多次", s.Key)
		}
		byKey[s.Key] = s
	}
	return byKey, nil
}

// Lookup 返回一个配置键的声明。第二个返回值为 false 表示 Registry 里没有它。
//
// 线性扫一遍 Registry，而不是复用 index()：index() 把重复声明当成错误，
// 而它唯一的调用点（Load）已经在启动时查过一遍了。这里要的是一个不返回
// 错误的查询——写入路径拿它的结果去校验，而「键不存在」与「Registry 自己
// 坏了」是两件事，混进同一个 error 就分不清该改键名还是该改代码。
func Lookup(key string) (Spec, bool) {
	for _, s := range Registry {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// UnwiredNote 给出「这个键还没有读者」那句说明。键已接线、或 Registry 里
// 根本没有它时返回空串。
//
// 给只需要这一句、不需要整份声明的调用方用（`larkaxctl settings set` 写完
// 一个键之后要打它）。`settings list` 那边手上已经有整份 Spec，直接读
// spec.Unwired，不必再查一遍。
//
// 它取自 Registry 而不是库里那一行 description：接不接线是**代码**的事实，
// 而接线的人改的正是 Registry 里那几行。
func UnwiredNote(key string) string {
	spec, ok := Lookup(key)
	if !ok {
		return ""
	}
	return spec.Unwired
}

// Load 读取全部配置并与 Registry 对照校验。
//
// 任何不一致都返回错误而不回落到默认值：静默回落会让「配置写错了」
// 表现为「行为跟预期不一样」，排查成本远高于启动时直接失败。
func Load(ctx context.Context, src Source) (*Values, error) {
	byKey, err := index()
	if err != nil {
		return nil, err
	}

	rows, err := src.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 settings 失败: %w", err)
	}

	raw := make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		spec, known := byKey[r.Key]
		if !known {
			return nil, fmt.Errorf("settings 里有未声明的配置项 %q：先在 settings.Registry 里声明它", r.Key)
		}
		if !json.Valid(r.Value) {
			// jsonb 列本该只存合法 JSON，走到这里说明值是被别的手段写进去的。
			// 字符串配置最常见的笔误是漏了引号（写 CNY 而不是 "CNY"），
			// 单独点一句，否则使用者只看到一句无从下手的语法错。
			if spec.Kind == KindString {
				return nil, fmt.Errorf("settings.%s 不是合法 JSON；字符串要写成带引号的 JSON，如 \"%s\"", r.Key, r.Value)
			}
			return nil, fmt.Errorf("settings.%s 不是合法 JSON", r.Key)
		}
		raw[r.Key] = json.RawMessage(r.Value)
	}

	var missing []string
	for _, s := range Registry {
		if _, ok := raw[s.Key]; !ok {
			missing = append(missing, s.Key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("settings 缺少配置项 %v：写一条迁移把这些键插进去", missing)
	}

	v := &Values{raw: raw, byKey: byKey}
	if err := v.validate(); err != nil {
		return nil, err
	}
	return v, nil
}

// validate 逐个按声明的类型解析一遍，把类型错误提前到启动时。
func (v *Values) validate() error {
	for _, s := range Registry {
		var err error
		switch s.Kind {
		case KindInt:
			_, err = v.Int(s.Key)
		case KindBool:
			_, err = v.Bool(s.Key)
		case KindString:
			_, err = v.String(s.Key)
		default:
			return fmt.Errorf("settings.%s 声明的类型 %q 未知", s.Key, s.Kind)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Keys 返回全部配置键，已排序。用于启动日志与调试输出。
func (v *Values) Keys() []string {
	keys := make([]string, 0, len(v.raw))
	for k := range v.raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Int 读取整数配置。
//
// 返回错误而不是零值：调用方漏了错误处理时，零值会安静地生效——
// 比如留存期变成 0 天，快照刚写入就被删。
func (v *Values) Int(key string) (int64, error) {
	raw, err := v.lookup(key, KindInt)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("settings.%s 期望整数: %w", key, err)
	}
	return n, nil
}

// Bool 读取布尔配置。
func (v *Values) Bool(key string) (bool, error) {
	raw, err := v.lookup(key, KindBool)
	if err != nil {
		return false, err
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("settings.%s 期望布尔值: %w", key, err)
	}
	return b, nil
}

// String 读取字符串配置。
func (v *Values) String(key string) (string, error) {
	raw, err := v.lookup(key, KindString)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("settings.%s 期望字符串（注意要写成带引号的 JSON）: %w", key, err)
	}
	return s, nil
}

func (v *Values) lookup(key string, want Kind) (json.RawMessage, error) {
	spec, known := v.byKey[key]
	if !known {
		return nil, fmt.Errorf("settings.%s 未在 Registry 里声明", key)
	}
	if spec.Kind != want {
		return nil, fmt.Errorf("settings.%s 声明为 %s，不能按 %s 读取", key, spec.Kind, want)
	}
	raw, ok := v.raw[key]
	if !ok {
		return nil, fmt.Errorf("settings.%s 不存在", key)
	}
	return raw, nil
}
