package consoleapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是请求侧的三样小工具：三态字段、游标、时间窗。
//
// 它们各自都只有一种正确写法，而错误写法**不会报错**——三态里漏掉「清空」
// 会变成「设成空串」，游标里少一列会漏行，时间窗里按 UTC 解释 `YYYY-MM-DD`
// 会让同一份查询在两个人手里差八小时。所以收在一处，只写一遍。

// optionalString 是「缺省 / null / 给值」三态里的字符串。
//
// 用指针套一层而不是直接用 *string：JSON 的 null 与「字段不出现」在
// encoding/json 里都落到 nil，而这两件事在本包的写方法里处置相反——
// 一个是不改，一个是清空。
//
// 请求结构体里**直接用它做值**，不要写成指针：encoding/json 对 `*T` 字段
// 遇到 null 时只把指针置 nil，不会调用它的 UnmarshalJSON——于是「缺省」与
// 「null」又混成同一个 nil，三态退化成两态。值类型字段则会收到一次
// `UnmarshalJSON("null")`，而这正是区分两者的地方。
type optionalString struct {
	Set   bool
	Value *string
}

// UnmarshalJSON 记录「字段出现过」，并区分 null 与字符串。
func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("必须是字符串或 null: %w", err)
	}
	o.Value = &s
	return nil
}

// optionalInt 是「缺省 / null / 给值」三态里的整数。
//
// 与 optionalString 同一个形状。`rate_limit_rpm` 的 null 是「不限」、
// 0 是「一个都不放行」，所以三态在这里同样不能用零值代替。
type optionalInt struct {
	Set   bool
	Value *int
}

// UnmarshalJSON 记录「字段出现过」，并区分 null 与整数。
func (o *optionalInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	// 先解成 json.Number 再 Atoi：直接解 int 会把 1e3 这类写法也收下，
	// 而一个「1000 写成 1e3」的额度看起来正常，实际可能是另一个人手误。
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("必须是整数或 null: %w", err)
	}
	v, err := strconv.Atoi(n.String())
	if err != nil {
		return fmt.Errorf("必须是整数或 null: %w", err)
	}
	o.Value = &v
	return nil
}

// optionalMoney 是「缺省 / null / 给值」三态里的金额。
//
// 请求里的金额是十进制字符串（与响应同一形状），在这里就解析成定点数：
// 让一个畸形金额走到下层，等于把「这个数读不出来」推迟到写库那一刻。
type optionalMoney struct {
	Set   bool
	Value *money.Amount
}

// UnmarshalJSON 记录「字段出现过」，并区分 null 与十进制字符串。
func (o *optionalMoney) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("金额必须是十进制字符串或 null: %w", err)
	}
	amount, err := money.Parse(s)
	if err != nil {
		return fmt.Errorf("金额 %q 无法解析: %w", s, err)
	}
	o.Value = &amount
	return nil
}

// cursorPrefix 是游标里两段之间的分隔符。
const cursorPrefix = ":"

// encodeCursor 把 (created_at, id) 编成一个不透明串。
//
// 时间取 Unix 微秒而不是秒：同一秒里建的两行会让游标退化成「按 id」，
// 而 id 与 created_at 的次序不一定一致（导入的历史行尤其如此）。
//
// base64url 不是加密：游标里没有机密，只有两列排序键。编一下是为了让
// 客户端不要解析它——它今天的样子不是接口的一部分。
func encodeCursor(at time.Time, id int64) string {
	raw := strconv.FormatInt(at.UTC().UnixMicro(), 10) + cursorPrefix + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 解一个游标。
//
// 解不出来一律报错而不是从第一页开始：一个坏游标静默返回第一页，会让
// 「翻到底又跳回开头」看起来像数据在循环。
func decodeCursor(raw string) (time.Time, int64, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, 0, errors.New("游标格式不正确")
	}
	micro, id, ok := strings.Cut(string(decoded), cursorPrefix)
	if !ok {
		return time.Time{}, 0, errors.New("游标格式不正确")
	}
	atMicro, err := strconv.ParseInt(micro, 10, 64)
	if err != nil {
		return time.Time{}, 0, errors.New("游标格式不正确")
	}
	keyID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || keyID <= 0 {
		return time.Time{}, 0, errors.New("游标格式不正确")
	}
	return time.UnixMicro(atMicro).UTC(), keyID, nil
}

// 用量窗口的边界。
const (
	// usageWindowDefault 是没给 from/to 时的默认跨度：最近 30 天。
	usageWindowDefault = 30 * 24 * time.Hour
	// usageWindowMax 是跨度上限：366 天，覆盖一个闰年。
	usageWindowMax = 366 * 24 * time.Hour
)

// parseWindow 解析用量与钱包分录的时间窗。
//
// from 含、to 不含。接受 RFC3339 或 `YYYY-MM-DD`；后者按**站点时区**解释成
// 当天 00:00——用户说「10 月 1 日」指的是他那里的一天，按 UTC 解释会在
// 时区非零的部署上整体偏几个小时，而两个人都看着同一份数据却对不上账。
func (a *API) parseWindow(r *http.Request, now time.Time) (from, to time.Time, err error) {
	q := r.URL.Query()
	to = now.UTC().Truncate(time.Second)
	from = to.Add(-usageWindowDefault)

	if raw := q.Get("to"); raw != "" {
		to, err = a.parseTimeValue(raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to %w", err)
		}
	}
	if raw := q.Get("from"); raw != "" {
		from, err = a.parseTimeValue(raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from %w", err)
		}
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New("from 必须早于 to")
	}
	if to.Sub(from) > usageWindowMax {
		return time.Time{}, time.Time{}, fmt.Errorf("时间窗跨度不能超过 %d 天",
			int(usageWindowMax/(24*time.Hour)))
	}
	return from, to, nil
}

// parseTimeValue 解析一个时间参数。
func (a *API) parseTimeValue(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, a.loc); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, errors.New("必须写成 RFC3339 或 YYYY-MM-DD")
}

// queryKeyID 解析可选的 key_id 过滤。
//
// 没给这个参数时返回 (nil, nil)：nil 在这里是「没有这个过滤条件」这个
// 合法取值，不是「出错了但没报」——调用方看指针判是否加过滤。
func queryKeyID(r *http.Request) (*int64, error) {
	raw := r.URL.Query().Get("key_id")
	if raw == "" {
		return nil, nil //nolint:nilnil // nil 表示「没有 key_id 过滤」，是合法取值
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("key_id 必须是正整数")
	}
	return &id, nil
}
