package catalog

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 本文件是不需要数据库的那一半：写入侧的值归一化，以及读取侧的入参校验。
//
// 它们拿 nil 句柄跑——校验一律发生在碰数据库之前，所以这几条用例在没有库的
// 机器上也跑得起来，而「参数不对却先连了库」会在第一行就 panic 露出来。

func TestNormalizeCanonicalName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
		// errText 非空时，错误信息里必须出现它：报错要指到原因，不能只说
		// 「非法名字」。
		errText string
	}{
		{"去掉首尾空白", "  acme-vision  ", "acme-vision", false, ""},
		{"空串", "", "", true, ""},
		{"只有空白", "   ", "", true, ""},
		// 带空白的名字不会等于任何请求里的模型名，写进去就是一条谁都命中不了的
		// 配置——它的表现是「这个模型配了却永远选不中」。
		{"中间有空格", "acme vision", "", true, ""},
		{"中间有制表符", "acme\tvision", "", true, ""},

		// 空路径段与 `.` / `..` 段：这些名字写进目录不报错，读的时候会被
		// URL 规范化折叠或解析掉，于是要么 301、要么落到另一个模型上。
		{"中间空段", "a//b", "", true, "路径段为空"},
		{"开头空段", "/a", "", true, "路径段为空"},
		{"中间单点段", "a/./b", "", true, "`.` 与 `..`"},
		{"中间双点段", "a/../b", "", true, "`.` 与 `..`"},
		{"开头单点段", "./a", "", true, "`.` 与 `..`"},
		{"开头双点段", "../a", "", true, "`.` 与 `..`"},
		{"末尾空段", "a/", "", true, "路径段为空"},
		{"只有单点", ".", "", true, "`.` 与 `..`"},
		{"只有双点", "..", "", true, "`.` 与 `..`"},
		{"末尾单点段", "a/.", "", true, "`.` 与 `..`"},
		{"末尾双点段", "a/..", "", true, "`.` 与 `..`"},

		// `..` 只在**整段**等于 `.` 或 `..` 时才非法：出现在一个段内部时
		// 它只是一个普通的字符，URL 解析不会碰它。
		{"单段", "alpha", "alpha", false, ""},
		{"两段", "vendor/alpha", "vendor/alpha", false, ""},
		{"三段", "vendor/a/b", "vendor/a/b", false, ""},
		{"段内双点", "a..b", "a..b", false, ""},
		{"段内单点", "a.b", "a.b", false, ""},
		{"段首双点", "a/..b", "a/..b", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeCanonicalName(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeCanonicalName(%q) = %q，期望报错", tc.input, got)
				}
				if tc.errText != "" && !strings.Contains(err.Error(), tc.errText) {
					t.Errorf("normalizeCanonicalName(%q) 的错误信息 = %q，期望含 %q",
						tc.input, err, tc.errText)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeCanonicalName(%q) 出错: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("normalizeCanonicalName(%q) = %q，期望 %q", tc.input, got, tc.want)
			}
		})
	}
}

// Delete 认的名字与写入侧存的名字是同一个：同一个输入交给严格版与宽松版，
// 两处得到**逐字相同**的规整结果。
//
// 它证明的是两个入口对**两版都接受的**输入给出同一个答案，不是严格版
// 在实现上建在宽松版之上——共用底座看的是 normalizeCanonicalName 里
// 调用 normalizeExistingCanonicalName 的那一句；把那一句换成一份会分岔的
// 内联实现、而两版仍给出同样答案，这条用例不会红。
//
// 两份各自 trim 时返回值一样只是碰巧，没有东西挡得住它们以后分岔；这条
// 用例红，说明 Delete 认的名字与写入侧存的名字已经不是同一个。
func TestNormalizeCanonicalNameAgreesWithLenient(t *testing.T) {
	for _, input := range []string{"  alpha ", "vendor/alpha", "\talpha\n"} {
		strict, err := normalizeCanonicalName(input)
		if err != nil {
			t.Fatalf("normalizeCanonicalName(%q) 出错: %v", input, err)
		}
		lenient, err := normalizeExistingCanonicalName(input)
		if err != nil {
			t.Fatalf("normalizeExistingCanonicalName(%q) 出错: %v", input, err)
		}
		if strict != lenient {
			t.Errorf("%q 在两处规整结果不同：严格 %q、宽松 %q", input, strict, lenient)
		}
	}
}

// 宽松版只放宽路径段：空白、空串与控制字符照拒。Delete 认的是一个存量
// 标识符，不是「现在能不能写进去」。
func TestNormalizeExistingCanonicalNameStillRejectsShapeErrors(t *testing.T) {
	for _, input := range []string{"", "   ", "a b", "a\x1b[31mb"} {
		if got, err := normalizeExistingCanonicalName(input); err == nil {
			t.Errorf("normalizeExistingCanonicalName(%q) = %q，期望报错", input, got)
		}
	}
	// 路径段不判：这正是它与写入侧那条规则的唯一差别。
	if got, err := normalizeExistingCanonicalName("a//b"); err != nil || got != "a//b" {
		t.Errorf("normalizeExistingCanonicalName(%q) = %q, %v，期望原样通过", "a//b", got, err)
	}
}

// 0 表示「未知」，落成 NULL：存 0 会让读取侧的「未知」与一个测出来的 0
// 分不开，而读取侧本来就把 NULL 折成 0。
func TestNormalizeLimit(t *testing.T) {
	got, err := normalizeLimit("context_limit", 0)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got != nil {
		t.Errorf("0 应落成 NULL，实际 %v", got)
	}

	got, err = normalizeLimit("context_limit", 200000)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got != 200000 {
		t.Errorf("200000 = %v", got)
	}

	if _, err := normalizeLimit("context_limit", -1); err == nil {
		t.Error("负数应当被拒绝")
	}
}

// 日期落成 `YYYY-MM-DD` 文本：交给驱动当 timestamptz 发的话，
// timestamptz → date 会按会话时区算，同一个瞬间在不同时区下可能是两天。
func TestDateOrNull(t *testing.T) {
	if got := dateOrNull(nil); got != nil {
		t.Errorf("nil 应落成 NULL，实际 %v", got)
	}
	if got := dateOrNull(&time.Time{}); got != nil {
		t.Errorf("零值应落成 NULL，实际 %v", got)
	}

	value := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	if got := dateOrNull(&value); got != "2024-05-01" {
		t.Errorf("dateOrNull = %v，期望 2024-05-01", got)
	}
}

// 空串落成 NULL：读取侧把这两者折成同一个状态，所以库里不该有两种。
func TestTextOrNull(t *testing.T) {
	if got := textOrNull("  "); got != nil {
		t.Errorf("空串应落成 NULL，实际 %v", got)
	}
	if got := textOrNull(" acme "); got != "acme" {
		t.Errorf("textOrNull = %v，期望去掉空白的 acme", got)
	}
}

func TestDecodeStrings(t *testing.T) {
	got, err := decodeStrings(`["text","image"]`)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Errorf("decodeStrings = %v", got)
	}

	// 空数组与空列都要给出空切片，而不是 nil：调用方拿它当「没有这类模态」，
	// 而 nil 会让 range 与 len 都对、比较却不对。
	for _, raw := range []string{`[]`, ``} {
		got, err = decodeStrings(raw)
		if err != nil {
			t.Fatalf("decodeStrings(%q) 出错: %v", raw, err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("decodeStrings(%q) = %v，期望空切片", raw, got)
		}
	}

	if _, err := decodeStrings(`{"a":1}`); err == nil {
		t.Error("不是数组时应当报错")
	}
}

// 读取路径的入参校验：空名字与负数渠道 id 都不该落到 SQL 上。
func TestReadPathValidatesInput(t *testing.T) {
	if _, _, err := New(nil).Alias(context.Background(), 1, ""); err == nil {
		t.Error("空的规范名应当被拒绝")
	}
	if _, err := New(nil).Model(context.Background(), ""); err == nil {
		t.Error("空的规范名应当被拒绝")
	}
	if _, err := New(nil).UpstreamModels(context.Background(), ""); err == nil {
		t.Error("空的规范名应当被拒绝")
	}
	if _, err := New(nil).Aliases(context.Background(), -1); err == nil {
		t.Error("负数的渠道 id 应当被拒绝")
	}
}
