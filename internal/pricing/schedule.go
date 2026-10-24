package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
)

// Schedule 是一张**可枚举**的定价表：一张基础单价表，加上若干条带条件的
// 覆盖。写入时它被编译成一条普通表达式（见 Validate），读取、求值、预留
// 全部走既有的 expr 路径，不新增第二个求值器。
//
// # 为什么要有这一层
//
// 手写表达式表达力够，但不可枚举：`len <= 32000 ? (isPeak("08:00-22:00") ? A : B) : C`
// 求值没问题，却回答不了「这条规则有几个档、每档什么价」。两个维度就要手写
// 四支，三个维度八支，而每一对交叉的优先级都要作者当场决定一遍。
//
// 表把「写规则」变成列条件：每行只关心一个维度，交叉点由 position 统一裁决。
// 编译成 per-key 三元链之后，`EvalWorstCase` 照旧走遍每一支、逐键取最大，
// 预留上界那套逻辑一行都不用改。
type Schedule struct {
	// TZ 是这张表解释钟点与星期用的时区，必填。
	//
	// 不从 Env.Region 推断，与 hour(tz) 同一条理由：同一地区的两条渠道可以
	// 有各自的峰谷，改地区偏好不该悄悄改价。
	TZ string `json:"tz"`

	// Base 是兜底单价表，键与表达式的计价项同名（prompt、completion……）。
	// 至少一项；覆盖里出现的键必须在这里有值。
	Base map[string]float64 `json:"base"`

	// Overrides 是条件覆盖。每一条只写它关心的维度与它要改的单价。
	Overrides []ScheduleOverride `json:"overrides,omitempty"`
}

// ScheduleOverride 是一条条件覆盖。
//
// 价格项与条件项被打平在同一层 JSON 里（见问题里的示例），所以
// UnmarshalJSON 把不是保留字的键当成单价收进 Prices。
type ScheduleOverride struct {
	// Position 是这条覆盖的优先级，**大的优先**，与 pricing_rules.priority
	// 同名同义同方向。跨维度的两条覆盖可能同时命中同一笔请求，那时必须由
	// 显式 position 决定谁赢——按数组顺序决胜会让「移动一个元素」改价。
	Position *int `json:"position,omitempty"`

	// Weekdays 是适用星期，取值 sun/mon/tue/wed/thu/fri/sat。
	Weekdays []string `json:"weekdays,omitempty"`

	// Window 是适用时段，写法与 isPeak 相同（HH:MM-HH:MM，多段用逗号）。
	Window string `json:"window,omitempty"`

	// DayKind 是「工作日 / 休息日」，取值 work 或 rest。
	DayKind string `json:"day_kind,omitempty"`

	// Holidays 是适用的节假日名，例如 ["春节", "国庆节"]。它只匹配**放假
	// 的那几天**：调休上班日也带着节日名（例如春节前后挪用周末上班的那天），
	// 编译出来的条件因此还要 `!isWorkday()`。
	Holidays []string `json:"holidays,omitempty"`

	// MinContextTokens 是输入侧上下文长度的下界（len >= 该值即命中）。
	// len 含缓存读写，所以名字用 context 而不是 prompt。
	MinContextTokens *int64 `json:"min_context_tokens,omitempty"`

	// Prices 是这条覆盖改写的单价。它打平在 override 对象的顶层，
	// 不另占一个 JSON 字段。
	Prices map[string]float64 `json:"-"`
}

// scheduleSlots 是结构化定价表能写的单价键。
//
// token 槽位从 billing.TokenSlots 派生，不另抄一份；fixed/minimum 是两项
// 一次性费用。units 不在这里：它是一张计量项表，没有「单值」可以排成一条
// 链，而表和表达式混用又会让「可枚举的那一半」不完整。
var scheduleSlots = func() map[string]struct{} {
	out := make(map[string]struct{}, len(billing.TokenSlots)+2)
	for _, slot := range billing.TokenSlots {
		out[slot.Name] = struct{}{}
	}
	out["fixed"] = struct{}{}
	out["minimum"] = struct{}{}
	return out
}()

// weekdayNames 把规则里的三个字母缩写折成 time.Weekday。
//
// 只认小写缩写：表是给人看也给人改的，`Monday` 与 `mon` 两种写法并存时，
// 同一份表里两种写法各出现一次就分不清是不是笔误。
var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// ParseSchedule 解析一张结构化定价表。
//
// # 重复键必须在这里挡下
//
// encoding/json 对同一个对象里的重复键默认**静默取后者**，于是作者以为自己
// 写了两条覆盖，实际只有一条进了库，而表看起来没有任何问题。先用
// json.Decoder 逐层扫一遍结构，任何一层出现重复键都拒绝。
//
// 未知的顶层字段也拒绝：`overide` 这种拼错如果被忽略，作者会得到一张
// 静默少了覆盖的表，而清单与编译结果都正常。
func ParseSchedule(data []byte) (*Schedule, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("定价表为空")
	}
	if err := rejectDuplicateKeys(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return nil, fmt.Errorf("定价表解析失败: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	var s Schedule
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("定价表解析失败: %w", err)
	}
	// 顶层之后还有内容（比如两份表粘在一起）时 Decode 不会报错，
	// 静默忽略前半份之外的整段比报错更坏。
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("定价表解析失败：第一个对象之后还有多余的内容")
	}
	return &s, nil
}

// maxScheduleDepth 是 JSON 嵌套深度的上限。
//
// 这张表是运维从本地文件导入的，正常只有两层（表 → overrides → 条目）。
// 设上限是为了让一份手滑或恶意构造的文件不会把递归栈打穿——那会变成一次
// 进程级故障，而不是一次「这个文件不对」。
const maxScheduleDepth = 32

// rejectDuplicateKeys 逐层扫描一个 JSON 文档，遇到重复键就报错。
//
// 它只看结构，不解释内容：具体字段的含义与类型由后面的 Decode 管。
func rejectDuplicateKeys(dec *json.Decoder, depth int) error {
	if depth > maxScheduleDepth {
		return fmt.Errorf("JSON 嵌套超过 %d 层", maxScheduleDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// 标量：没有键可重复。
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, keyErr := dec.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("对象的键不是字符串: %v", keyTok)
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("重复的键 %q：同一个对象里一个键只能出现一次", key)
			}
			seen[key] = struct{}{}
			if subErr := rejectDuplicateKeys(dec, depth+1); subErr != nil {
				return subErr
			}
		}
		_, err = dec.Token() // '}'
		return err
	case '[':
		for dec.More() {
			if subErr := rejectDuplicateKeys(dec, depth+1); subErr != nil {
				return subErr
			}
		}
		_, err = dec.Token() // ']'
		return err
	default:
		return fmt.Errorf("无法识别的 JSON 分隔符 %q", delim)
	}
}

// UnmarshalJSON 读一张表，把三个字段的类型与未知字段讲清楚。
//
// 不用 DisallowUnknownFields 一个理由：base 是 map[string]float64，遇到
// `"units": {...}` 时 Go 只会报「不能把对象解进 float64」，而作者要知道的
// 是「这张表不支持 units」。自己逐键解一遍，报错才能指向真正的问题。
func (s *Schedule) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var out Schedule
	for key, value := range raw {
		switch key {
		case "tz":
			if err := json.Unmarshal(value, &out.TZ); err != nil {
				return fmt.Errorf("tz 必须是字符串: %w", err)
			}
		case "base":
			base, err := unmarshalSchedulePrices(value)
			if err != nil {
				return fmt.Errorf("base: %w", err)
			}
			out.Base = base
		case "overrides":
			if err := json.Unmarshal(value, &out.Overrides); err != nil {
				return fmt.Errorf("overrides: %w", err)
			}
		default:
			return fmt.Errorf("未知的字段 %q：定价表只有 tz、base、overrides", key)
		}
	}
	*s = out
	return nil
}

// unmarshalSchedulePrices 读一张单价表，并给 units 一句能照着改的话。
func unmarshalSchedulePrices(data []byte) (map[string]float64, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("必须是单价对象: %w", err)
	}
	out := make(map[string]float64, len(raw))
	for key, value := range raw {
		if key == "units" {
			return nil, errors.New("不支持 units：计量项表没有单值可以排成一条链，" +
				"要按计量项定价请写表达式")
		}
		var price float64
		if err := json.Unmarshal(value, &price); err != nil {
			return nil, fmt.Errorf("%q 必须是数值单价: %w", key, err)
		}
		out[key] = price
	}
	return out, nil
}

// UnmarshalJSON 把一条覆盖读成「条件字段 + 单价」。
//
// json 的重复键检查已经在 ParseSchedule 里做过，这里直接按 map 读是安全的；
// 不在这里做是为了让自定义解码器只管形状转换，不重复实现一遍扫描。
func (o *ScheduleOverride) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := ScheduleOverride{Prices: map[string]float64{}}
	for key, value := range raw {
		switch key {
		case "position":
			var v int
			if err := json.Unmarshal(value, &v); err != nil {
				return fmt.Errorf("position 必须是整数: %w", err)
			}
			out.Position = &v
		case "weekdays":
			if err := json.Unmarshal(value, &out.Weekdays); err != nil {
				return fmt.Errorf("weekdays 必须是星期缩写数组: %w", err)
			}
		case "window":
			if err := json.Unmarshal(value, &out.Window); err != nil {
				return fmt.Errorf("window 必须是字符串: %w", err)
			}
		case "day_kind":
			if err := json.Unmarshal(value, &out.DayKind); err != nil {
				return fmt.Errorf("day_kind 必须是字符串: %w", err)
			}
		case "holidays":
			if err := json.Unmarshal(value, &out.Holidays); err != nil {
				return fmt.Errorf("holidays 必须是字符串数组: %w", err)
			}
		case "min_context_tokens":
			var v int64
			if err := json.Unmarshal(value, &v); err != nil {
				return fmt.Errorf("min_context_tokens 必须是整数: %w", err)
			}
			out.MinContextTokens = &v
		case "units":
			return errors.New("不支持 units：计量项表没有单值可以排成一条链，" +
				"要按计量项定价请写表达式")
		default:
			var v float64
			if err := json.Unmarshal(value, &v); err != nil {
				return fmt.Errorf("字段 %q 既不是条件也不是数值单价；"+
					"可用的条件字段是 position、weekdays、window、day_kind、holidays、min_context_tokens，"+
					"单价键是 %s", key, scheduleSlotList())
			}
			out.Prices[key] = v
		}
	}
	*o = out
	return nil
}

// MarshalJSON 把单价打平回顶层，与读入的写法对称。
func (o ScheduleOverride) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(o.Prices)+6)
	for k, v := range o.Prices {
		out[k] = v
	}
	if o.Position != nil {
		out["position"] = *o.Position
	}
	if len(o.Weekdays) > 0 {
		out["weekdays"] = o.Weekdays
	}
	if o.Window != "" {
		out["window"] = o.Window
	}
	if o.DayKind != "" {
		out["day_kind"] = o.DayKind
	}
	if len(o.Holidays) > 0 {
		out["holidays"] = o.Holidays
	}
	if o.MinContextTokens != nil {
		out["min_context_tokens"] = *o.MinContextTokens
	}
	return json.Marshal(out)
}

// scheduleSlotList 列出结构化定价表可用的单价键，用于错误信息。
func scheduleSlotList() string {
	names := make([]string, 0, len(scheduleSlots))
	for name := range scheduleSlots {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// NeedsCalendar 回答这张表编译出的表达式会不会用日历。
//
// 它的消费者是写入路径：表里有 day_kind 或 holidays 时，实例没接日历
// （expr.Env.Calendar 为 nil）就不该让它进库，否则第一次计价才会以
// ErrCalendarRequired 失败，而那条失败在流式路径上是一分钱不收。
func (s *Schedule) NeedsCalendar() bool {
	if s == nil {
		return false
	}
	for _, o := range s.Overrides {
		if o.DayKind != "" || len(o.Holidays) > 0 {
			return true
		}
	}
	return false
}

// Validate 校验这张表并返回它编译出的表达式源码。
//
// 顺序与判据（前者失败就不再往下走）：
//
//  1. 重复键——在 ParseSchedule 里，JSON 层的事；
//  2. 每一条覆盖自洽；
//  3. 同一维度重叠——静态判得出来的，直接拒绝；
//  4. 跨维度要求 position——判不出来谁赢的，要求作者写明白；
//  5. 生成表达式；
//  6. expr.Compile——生成物必须能过引擎自己的门。
//
// 它同时把星期、节假日名、时区归一化（去空白、去重、排序），于是落库的
// 那份与编译用的那份是同一个值，而不是「看起来差不多」的两份。
func (s *Schedule) Validate() (string, error) {
	if s == nil {
		return "", errors.New("定价表为空")
	}
	tz := strings.TrimSpace(s.TZ)
	if tz == "" {
		return "", errors.New("定价表缺少 tz：时区必须显式给出，不从地区推断")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "", fmt.Errorf("定价表的 tz %q 无法解析（例如 Asia/Shanghai）: %w", tz, err)
	}
	s.TZ = tz

	if len(s.Base) == 0 {
		return "", errors.New("定价表的 base 不能为空：至少给一项单价")
	}
	for key, value := range s.Base {
		if err := validateSchedulePrice("base", key, value); err != nil {
			return "", err
		}
	}

	rules := make([]scheduleRule, 0, len(s.Overrides))
	for i := range s.Overrides {
		rule, err := s.Overrides[i].normalize(i+1, s.Base)
		if err != nil {
			return "", err
		}
		rules = append(rules, rule)
	}

	if err := checkScheduleConflicts(rules); err != nil {
		return "", err
	}
	order := orderScheduleRules(rules)

	src := compileSchedule(tz, s.Base, order)
	if _, err := expr.Compile(src); err != nil {
		return "", fmt.Errorf("生成的表达式编译不过（%d 条覆盖、%d 个键，引擎有规模上限）：%w",
			len(rules), len(s.Base), err)
	}
	return src, nil
}

// validateSchedulePrice 校验一项单价。
//
// 负单价与 NaN/Inf 都在这里拦：它们进了金额之后，比较与求和的行为就由
// IEEE 决定而不是由业务决定，「预留 ≥ 结算」那条不变量会静默失效。
func validateSchedulePrice(where, key string, value float64) error {
	if _, ok := scheduleSlots[key]; !ok {
		return fmt.Errorf("%s 里的键 %q 不是计价项；可用的键是 %s", where, key, scheduleSlotList())
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s 里的 %s 不是有限数值", where, key)
	}
	if value < 0 {
		return fmt.Errorf("%s 里的 %s 非法: 单价不能为负", where, key)
	}
	return nil
}

// scheduleRule 是归一化之后的一条覆盖。它不导出：调用方看的是
// ScheduleOverride，这里只服务于校验与编译。
type scheduleRule struct {
	index    int // 第几条覆盖，从 1 开始，用来说清报错指向哪一条
	position *int
	weekdays []int // 已排序去重；nil 表示不限
	window   []expr.PeakWindow
	dayKind  string
	holidays []string // 已排序去重
	minTok   *int64
	prices   map[string]float64
}

// normalize 把一条覆盖折成内部形状，并检查它自己是否自洽。
func (o *ScheduleOverride) normalize(index int, base map[string]float64) (scheduleRule, error) {
	where := func(field string) string {
		return fmt.Sprintf("第 %d 条 override 的 %s", index, field)
	}
	rule := scheduleRule{index: index, dayKind: o.DayKind, prices: o.Prices}

	if o.Position != nil {
		if *o.Position < 0 {
			return scheduleRule{}, fmt.Errorf("%s 非法: position 不能为负", where("position"))
		}
		pos := *o.Position
		rule.position = &pos
	}
	if o.MinContextTokens != nil {
		if *o.MinContextTokens <= 0 {
			return scheduleRule{}, fmt.Errorf("%s 非法: 门槛必须大于 0", where("min_context_tokens"))
		}
		minTok := *o.MinContextTokens
		rule.minTok = &minTok
	}

	seen := map[time.Weekday]struct{}{}
	for _, raw := range o.Weekdays {
		name := strings.ToLower(strings.TrimSpace(raw))
		day, ok := weekdayNames[name]
		if !ok {
			return scheduleRule{}, fmt.Errorf("%s 非法: %q 不是星期缩写（sun/mon/tue/wed/thu/fri/sat）",
				where("weekdays"), raw)
		}
		if _, dup := seen[day]; dup {
			continue
		}
		seen[day] = struct{}{}
		rule.weekdays = append(rule.weekdays, int(day))
	}
	sort.Ints(rule.weekdays)
	// 归一化之后写回，落库的那份与编译用的那份是同一个值。
	o.Weekdays = o.Weekdays[:0]
	for _, day := range rule.weekdays {
		o.Weekdays = append(o.Weekdays, strings.ToLower(time.Weekday(day).String()[:3]))
	}

	if strings.TrimSpace(o.Window) != "" {
		windows, err := expr.ParsePeakWindows(strings.TrimSpace(o.Window))
		if err != nil {
			return scheduleRule{}, fmt.Errorf("%s 非法: %w", where("window"), err)
		}
		for _, w := range windows {
			if w.StartMinute%60 != 0 || w.EndMinute%60 != 0 {
				return scheduleRule{}, fmt.Errorf("%s 非法: 窗口 %q 不是整点边界。"+
					"结构化定价表按 hour 生成条件，带分钟的窗口会算错一截；"+
					"需要精确到分钟请写表达式", where("window"), o.Window)
			}
			if w.StartMinute == w.EndMinute {
				return scheduleRule{}, fmt.Errorf("%s 非法: 窗口 %q 覆盖全天，"+
					"去掉它、把这条单价写进 base", where("window"), o.Window)
			}
		}
		rule.window = windows
		o.Window = strings.TrimSpace(o.Window)
	}

	switch o.DayKind {
	case "", "work", "rest":
	default:
		return scheduleRule{}, fmt.Errorf("%s 非法: %q 不是取值（work 或 rest）", where("day_kind"), o.DayKind)
	}

	holidaySeen := map[string]struct{}{}
	for _, raw := range o.Holidays {
		name := strings.TrimSpace(raw)
		if name == "" {
			return scheduleRule{}, fmt.Errorf("%s 里有空名字", where("holidays"))
		}
		if _, dup := holidaySeen[name]; dup {
			continue
		}
		holidaySeen[name] = struct{}{}
		rule.holidays = append(rule.holidays, name)
	}
	sort.Strings(rule.holidays)
	o.Holidays = rule.holidays

	// holidays 只匹配放假的那几天（编译时带 !isWorkday()），与 work 是
	// 一对自相矛盾的条件。
	if o.DayKind == "work" && len(rule.holidays) > 0 {
		return scheduleRule{}, fmt.Errorf("%s 自相矛盾: holidays 只匹配放假的那几天"+
			"（编译时带 !isWorkday()），与 day_kind=\"work\" 不能同时出现", where("条件"))
	}

	if len(rule.weekdays) == 0 && len(rule.window) == 0 && rule.dayKind == "" &&
		len(rule.holidays) == 0 && rule.minTok == nil {
		return scheduleRule{}, fmt.Errorf("%s 没有任何条件：无条件的一档应当写进 base",
			where("条件"))
	}
	if len(o.Prices) == 0 {
		return scheduleRule{}, fmt.Errorf("%s 没有任何单价：这条覆盖不会改变任何价格",
			where("单价"))
	}
	for key, value := range o.Prices {
		if _, ok := base[key]; !ok {
			return scheduleRule{}, fmt.Errorf("%s 里的键 %q 不在 base 里："+
				"base 必须为每一档给出兜底单价", where("单价"), key)
		}
		if err := validateSchedulePrice(where("单价"), key, value); err != nil {
			return scheduleRule{}, err
		}
	}
	return rule, nil
}

// checkScheduleConflicts 检查覆盖之间的冲突。
//
// 只做写入时判定：编译成一条表达式之后运行时只剩一条，没有「谁盖谁」的
// 第二份判据。判据分三层：
//
//   - 静态可判且确实重叠的**同一维度**两条 → 拒绝（写重了，多半是笔误）；
//   - 跨维度的两条可能同时命中 → 必须显式 position，且不能相同；
//   - 静态可判不重叠的两条 → 互不干扰，不必给 position。
func checkScheduleConflicts(rules []scheduleRule) error {
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			a, b := rules[i], rules[j]
			pair := fmt.Sprintf("第 %d 条与第 %d 条 override", a.index, b.index)

			switch scheduleRelation(a, b) {
			case relationDisjoint:
				continue
			case relationSameDimension:
				return fmt.Errorf("%s 在同一维度上重叠：同一个维度写两次时，"+
					"先后没有唯一答案——门槛与门槛之间会自动按大小排，但门槛与同维度的条件"+
					"暂不支持一起分层；合并成一条，或者拆到互不重叠的取值上", pair)
			case relationOrderConflict:
				return fmt.Errorf("%s 的 position 与 min_context_tokens 门槛方向相反："+
					"门槛高的那一档更具体、应当先命中，所以它的 position 也要更大", pair)
			}

			// 跨维度：可能同时命中。position 必须显式给出，且不能相同——
			// 相同的 position 加确实重叠就是一次没有答案的决胜。
			if a.position == nil || b.position == nil {
				return fmt.Errorf("%s 的条件跨维度重叠，静态判不出谁先命中："+
					"两条都要显式给 position（大的优先）", pair)
			}
			if *a.position == *b.position {
				return fmt.Errorf("%s 的 position 相同（%d）且条件可能同时命中："+
					"给其中一条换一个 position", pair, *a.position)
			}
		}
	}
	return nil
}

// scheduleRelationKind 是两条覆盖的关系。
type scheduleRelationKind int

const (
	// relationDisjoint 是静态确定「不可能同时命中」，或者是 min 门槛的
	// 包含关系——后者的先后由门槛方向唯一确定，不需要 position。
	relationDisjoint scheduleRelationKind = iota
	// relationSameDimension 是同一维度上的重叠：可静态判定，写重了。
	relationSameDimension
	// relationOrderConflict 是门槛方向与 position 方向相反。
	relationOrderConflict
	// relationCross 是跨维度的可能重叠：交给 position 裁决。
	relationCross
)

// scheduleRelation 判定两条覆盖的关系。
//
// 维度之间的可判关系：
//
//	星期 × 星期      交集为空即不重叠
//	时段 × 时段      按分钟集合判，跨零点也按同一套算
//	节假日 × 节假日  名字交集
//	工作日/休息日     work 与 rest 不相交
//	节假日 × work     holidays 只匹配休息日，与 work 不相交
//	星期 × 休息日     不确定（调休补班把某个周六变成工作日）
//	星期 × 节假日     不确定（节假日可以落在任何星期）
//	用量门槛、时段与其余维度   相容，不构成「不相交」的依据
func scheduleRelation(a, b scheduleRule) scheduleRelationKind {
	if len(a.weekdays) > 0 && len(b.weekdays) > 0 {
		if !intersectsInts(a.weekdays, b.weekdays) {
			return relationDisjoint
		}
	}
	if len(a.window) > 0 && len(b.window) > 0 {
		if !windowsOverlap(a.window, b.window) {
			return relationDisjoint
		}
	}
	if len(a.holidays) > 0 && len(b.holidays) > 0 {
		if !intersectsStrings(a.holidays, b.holidays) {
			return relationDisjoint
		}
	}
	if a.dayKind != "" && b.dayKind != "" && a.dayKind != b.dayKind {
		return relationDisjoint
	}
	// holidays 只匹配休息日（编译时带 !isWorkday()），与 work 的两条永远
	// 碰不到一起。
	if len(a.holidays) > 0 && b.dayKind == "work" {
		return relationDisjoint
	}
	if len(b.holidays) > 0 && a.dayKind == "work" {
		return relationDisjoint
	}

	// 走到这里说明没有一处静态为空。共享的维度如果相交，那就是
	// 「同一维度重叠」——两条覆盖在各自的取值上写重了。
	if len(a.weekdays) > 0 && len(b.weekdays) > 0 {
		return relationSameDimension
	}
	if len(a.window) > 0 && len(b.window) > 0 {
		return relationSameDimension
	}
	if len(a.holidays) > 0 && len(b.holidays) > 0 {
		return relationSameDimension
	}
	if a.dayKind != "" && b.dayKind != "" {
		return relationSameDimension
	}
	if a.minTok != nil && b.minTok != nil {
		// 门槛的包含关系可以自动化简：门槛高的那一档更具体，排前面。
		// 方向唯一，所以不算「没有答案的重叠」。
		if *a.minTok == *b.minTok {
			return relationSameDimension
		}
		// position 与门槛方向打架时，两条排序规则会给出矛盾的次序。
		if a.position != nil && b.position != nil && *a.position != *b.position {
			higherMin := *a.minTok > *b.minTok
			higherPos := *a.position > *b.position
			if higherMin != higherPos {
				return relationOrderConflict
			}
		}
		return relationDisjoint
	}
	return relationCross
}

// noPosition 是「没有 position」在排序里的占位值。position 不允许为负，
// 所以它必然排在所有显式 position 之后。
const noPosition = -1

// orderScheduleRules 按编译进三元链的顺序排列：前面的先命中。
//
// position 大的优先，与 pricing_rules.priority 同方向。门槛相同的两条在
// checkScheduleConflicts 里已经被拒；门槛不同的两条按门槛从高到低排，
// 而校验保证了这个方向与 position 一致，所以两种排序不会打架。
//
// 没有 position 的条目只可能是与谁都不同时命中的那些（跨维度的重叠已经
// 被要求给 position），它们之间的顺序不影响价格；排在最后是为了让输出稳定。
func orderScheduleRules(rules []scheduleRule) []scheduleRule {
	out := make([]scheduleRule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.minTok != nil && b.minTok != nil && *a.minTok != *b.minTok {
			return *a.minTok > *b.minTok
		}
		ap, bp := noPosition, noPosition
		if a.position != nil {
			ap = *a.position
		}
		if b.position != nil {
			bp = *b.position
		}
		if ap != bp {
			return ap > bp
		}
		return a.index < b.index
	})
	return out
}

// compileSchedule 把表编译成一条普通表达式。
//
// 每个键各自一条三元链，链长等于「为这个键定价的覆盖条数」，不是 2^N 的
// 笛卡尔展开。这是「先编译再求值」的关键收益：EvalWorstCase 照旧把每个
// 三元换成 __worst(真, 假)，走遍每一支、逐键取最大，预留上界逻辑不用改。
func compileSchedule(tz string, base map[string]float64, rules []scheduleRule) string {
	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		chain := formatSchedulePrice(base[key])
		// 从优先级最低的一条开始往外套：每套一层，这条覆盖就排在
		// 当前链的前面（即更早命中）。
		for i := len(rules) - 1; i >= 0; i-- {
			rule := rules[i]
			price, ok := rule.prices[key]
			if !ok {
				continue
			}
			chain = fmt.Sprintf("%s ? %s : %s",
				rule.condition(tz), formatSchedulePrice(price), chain)
		}
		pairs = append(pairs, fmt.Sprintf("%q: %s", key, chain))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// condition 把一条覆盖的条件折成一个布尔表达式。
//
// 各维度之间是「并且」：一条覆盖同时写了 weekdays 与 window，指的是
// 「这些星期里的这些时段」。不同覆盖之间的先后由 position 决定。
func (r scheduleRule) condition(tz string) string {
	quotedTZ := strconv.Quote(tz)
	parts := make([]string, 0, 4)
	if len(r.weekdays) > 0 {
		days := make([]string, 0, len(r.weekdays))
		for _, day := range r.weekdays {
			days = append(days, strconv.Itoa(day))
		}
		parts = append(parts, fmt.Sprintf("weekday(%s) in [%s]", quotedTZ, strings.Join(days, ", ")))
	}
	if len(r.window) > 0 {
		segments := make([]string, 0, len(r.window))
		for _, w := range r.window {
			start, end := w.StartMinute/60, w.EndMinute/60
			if start < end {
				segments = append(segments, fmt.Sprintf(
					"(hour(%s) >= %d && hour(%s) < %d)", quotedTZ, start, quotedTZ, end))
				continue
			}
			// 跨零点：[start, 24) 与 [0, end) 两段并起来。
			segments = append(segments, fmt.Sprintf(
				"(hour(%s) >= %d || hour(%s) < %d)", quotedTZ, start, quotedTZ, end))
		}
		parts = append(parts, "("+strings.Join(segments, " || ")+")")
	}
	switch r.dayKind {
	case "rest":
		parts = append(parts, "!isWorkday()")
	case "work":
		parts = append(parts, "isWorkday()")
	}
	if len(r.holidays) > 0 {
		quoted := make([]string, 0, len(r.holidays))
		for _, name := range r.holidays {
			quoted = append(quoted, strconv.Quote(name))
		}
		// 只比名字不够：调休上班日也带着节日名（例如春节前后挪用周末
		// 上班的那天），那些天是工作日。holidays 指的是「放假的那几天」，
		// 所以再加一道 !isWorkday()。
		parts = append(parts, fmt.Sprintf(
			"(holidayName() in [%s] && !isWorkday())", strings.Join(quoted, ", ")))
	}
	if r.minTok != nil {
		parts = append(parts, fmt.Sprintf("len >= %d", *r.minTok))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " && ") + ")"
}

// formatSchedulePrice 把单价写成表达式里的浮点字面量。
//
// 'g' 保留最短可回读的形式（1.5e-06），expr 的词法认科学计数法。
func formatSchedulePrice(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// intersectsInts 判两个升序整数切片有没有交集。
func intersectsInts(a, b []int) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			return true
		}
	}
	return false
}

// intersectsStrings 判两个字符串切片有没有交集。
func intersectsStrings(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := set[s]; ok {
			return true
		}
	}
	return false
}

// windowsOverlap 判两组时段有没有共同的分钟。
//
// 按分钟集合判而不是按区间端点比大小：段可以跨零点，肉眼比端点时很容易把
// [22:00, 02:00) 与 [01:00, 03:00) 判成不相交，而它们其实重叠。
func windowsOverlap(a, b []expr.PeakWindow) bool {
	const minutesPerDay = 24 * 60
	var covered [minutesPerDay]bool
	for _, w := range a {
		markWindow(&covered, w)
	}
	for _, w := range b {
		start, end := w.StartMinute, w.EndMinute
		if start < end {
			for m := start; m < end; m++ {
				if covered[m] {
					return true
				}
			}
			continue
		}
		for m := start; m < minutesPerDay; m++ {
			if covered[m] {
				return true
			}
		}
		for m := 0; m < end; m++ {
			if covered[m] {
				return true
			}
		}
	}
	return false
}

// markWindow 把一段时段标进分钟集合，跨零点按两段处理。
func markWindow(covered *[24 * 60]bool, w expr.PeakWindow) {
	start, end := w.StartMinute, w.EndMinute
	if start < end {
		for m := start; m < end; m++ {
			covered[m] = true
		}
		return
	}
	for m := start; m < 24*60; m++ {
		covered[m] = true
	}
	for m := 0; m < end; m++ {
		covered[m] = true
	}
}
