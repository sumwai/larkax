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
	Key         string
	Kind        Kind
	Description string
}

// Registry 是全部已知配置项。
//
// 新增配置项时：先加迁移里的 INSERT，再在这里加一行。两处都要改是有意的，
// 它保证「能读到」和「知道它是什么」不会脱节。
var Registry = []Spec{
	{Key: "request_snapshot.retention_days", Kind: KindInt,
		Description: "请求结构快照的留存天数。到期按分区整块删除。"},
	{Key: "request_snapshot.enabled", Kind: KindBool,
		Description: "是否采集请求结构快照。"},
	{Key: "request_snapshot.max_bytes", Kind: KindInt,
		Description: "单条结构骨架的字节上限。"},
	{Key: "capture.consent_max_window_hours", Kind: KindInt,
		Description: "用户授权原样采集时，单次授权的最长有效时长。"},
	{Key: "quota.probe_interval_seconds", Kind: KindInt,
		Description: "额度探测的默认间隔。"},
	{Key: "billing.default_asset", Kind: KindString,
		Description: "新用户钱包的默认资产品种。"},
	{Key: "calendar.default_region", Kind: KindString,
		Description: "分时计价默认使用的节假日地区。"},
	{Key: "idempotency.retention_hours", Kind: KindInt,
		Description: "幂等记录的留存小时数。到期由 larkaxctl idempotency cleanup 删除。"},
	{Key: "plugin.enabled", Kind: KindBool,
		Description: "是否启用插件系统。关闭时插件不注册、不执行。"},
	{Key: "plugin.call_timeout_ms", Kind: KindInt,
		Description: "单次插件 hook 调用的时限（毫秒）。"},
	{Key: "plugin.memory_limit_bytes", Kind: KindInt,
		Description: "单个插件运行时的内存上限（字节）。"},
	{Key: "plugin.max_payload_bytes", Kind: KindInt,
		Description: "插件单次调用的入参与出参字节上限。"},
	{Key: "reconcile.auto_repair", Kind: KindBool,
		Description: "对账发现不一致时是否自动修正。"},
	{Key: "reconcile.repair_mode", Kind: KindString,
		Description: "自动修正的方向：resync_snapshot 以账本为准，adjust_ledger 以快照为准。"},
	{Key: "reconcile.difference_account", Kind: KindString,
		Description: "adjust_ledger 口径下修正分录的对家科目。"},
	{Key: "reconcile.shard_size", Kind: KindInt,
		Description: "一批比对的钱包行数上限。"},
	{Key: "reconcile.full_scan_interval_hours", Kind: KindInt,
		Description: "两次全量对账之间的最长间隔小时数，0 表示每次都全量。"},
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
