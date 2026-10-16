package settings

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestEncodeValue 逐个类型钉住编码结果。
//
// 断言的是**编码后的字节**，不是「解析回来能对上」：这一层的产物要直接
// 写进 jsonb 列，而字符串少一对引号正是最该在这里现形的笔误——
// 让它漏过去，就要等到服务下一次重启、Load 报错时才有人知道。
func TestEncodeValue(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		plain string
		want  string
	}{
		{"整数", "request_snapshot.retention_days", "7", `7`},
		{"整数带前后空白", "request_snapshot.retention_days", " 7 ", `7`},
		{"整数取零", "request_snapshot.retention_days", "0", `0`},
		{"负整数", "request_snapshot.retention_days", "-1", `-1`},
		{"布尔真", "plugin.enabled", "true", `true`},
		{"布尔假", "plugin.enabled", "false", `false`},
		{"字符串要加引号", "billing.default_asset", "CNY", `"CNY"`},
		{"字符串里的引号要转义", "billing.default_asset", `a"b`, `"a\"b"`},
		{"空字符串也是一个合法的字符串", "billing.default_asset", "", `""`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeValue(tc.key, tc.plain)
			if err != nil {
				t.Fatalf("编码失败: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("EncodeValue(%q, %q) = %s，期望 %s", tc.key, tc.plain, got, tc.want)
			}
			// 编出来的必须是合法 JSON：它下一步直接进 jsonb 列。
			if !json.Valid(got) {
				t.Errorf("%s 不是合法 JSON", got)
			}
		})
	}
}

// TestEncodeValueRejects 钉住每一类拒绝，以及拒绝时错误信息里有没有
// 那个能让人知道下一步做什么的东西。
func TestEncodeValueRejects(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		plain string
		want  string
	}{
		{"未声明的键", "typo.retentoin_days", "7", "Registry"},
		{"整数写成了小数", "request_snapshot.retention_days", "7.5", "整数"},
		{"整数写成了文字", "request_snapshot.retention_days", "七天", "整数"},
		{"整数带了千分位", "request_snapshot.retention_days", "1,000", "整数"},
		// 布尔只认 true / false 两个词：这一列旁边就是整数配置，
		// 让 1 与 0 在两种类型上都成立，读的人就得多想一步。
		{"布尔写成了 1", "plugin.enabled", "1", "true 或 false"},
		{"布尔写成了 0", "plugin.enabled", "0", "true 或 false"},
		{"布尔写成了 yes", "plugin.enabled", "yes", "true 或 false"},
		{"布尔写成了大写", "plugin.enabled", "TRUE", "true 或 false"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeValue(tc.key, tc.plain)
			if err == nil {
				t.Fatalf("期望报错，却通过了（编出了 %s）", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// 未声明的键要能被 errors.Is 认出来，而不是只靠错误文本。
//
// 命令行拿它区分「键名写错了」与「库出问题了」，两者的下一步动作不同：
// 一个去改键名，一个去看数据库。
func TestEncodeValueUnknownKeyIsSentinel(t *testing.T) {
	_, err := EncodeValue("typo.retentoin_days", "7")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("期望 ErrUnknownKey，得到 %v", err)
	}
}

// Registry 里声明的每一个键都必须能被编码。
//
// 加配置项时只往 Registry 里加了一行、忘了在 EncodeValue 里加一个分支，
// 表现是「这个键写不进去」，而报错说的是「类型未知」——那句话会把
// 排查引向声明，而问题在编码。
func TestEveryRegistryKeyIsEncodable(t *testing.T) {
	plain := map[Kind]string{
		KindInt:    "1",
		KindBool:   "true",
		KindString: "x",
	}

	for _, spec := range Registry {
		value, ok := plain[spec.Kind]
		if !ok {
			t.Fatalf("%s 的类型 %q 没有对应的样例值，这个用例要跟着补", spec.Key, spec.Kind)
		}
		if _, err := EncodeValue(spec.Key, value); err != nil {
			t.Errorf("Registry 里的 %s（%s）编不出来: %v", spec.Key, spec.Kind, err)
		}
	}
}

// Lookup 是写入路径判断「这个键认不认识」的地方，两个方向都要对。
func TestLookup(t *testing.T) {
	spec, ok := Lookup("plugin.enabled")
	if !ok {
		t.Fatal("plugin.enabled 应当已声明")
	}
	if spec.Kind != KindBool {
		t.Errorf("plugin.enabled 的类型 = %q，期望 %q", spec.Kind, KindBool)
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("未声明的键不该被认出来")
	}
}
