package main

import (
	"regexp"
	"strings"
	"testing"
)

// TestDoctorLookupEndpointNames 用字面量整张断言 --endpoint 的取值清单。
//
// 期望值写成字面量，而不是从 doctorEndpointNames() 拼出来：拼出来的期望值等于
// 自己比自己，从清单里删掉一条时两边同时少一条，没有用例会红。这里要防的正是
// 「清单少了一条或多了一条，而报错文案没有跟着变」。
func TestDoctorLookupEndpointNames(t *testing.T) {
	want := []string{"chat", "messages", "responses"}
	got := doctorEndpointNames()
	if len(got) != len(want) {
		t.Fatalf("入口清单有 %d 项，期望 %d 项：%q", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("第 %d 项 = %q，期望 %q", i, got[i], name)
		}
	}
}

// TestDoctorLookupEndpointRejectsUnknown 逐字断言不认识的入口报什么。
//
// 整句比对而不是只看前缀：报错里列的那几个名字也由清单渲染，前缀对得上而清单
// 少一条时，前缀断言照样是绿的。
func TestDoctorLookupEndpointRejectsUnknown(t *testing.T) {
	_, err := lookupDoctorEndpoint("bogus")
	if err == nil {
		t.Fatal("不认识的入口该报错，得到 nil")
	}
	const want = `--endpoint 只能是 chat、messages 或 responses，当前为 "bogus"`
	if err.Error() != want {
		t.Errorf("报错 = %q，期望 %q", err.Error(), want)
	}
}

// TestDoctorLookupEndpointPaths 确认三个入口都查得到，且各自带着自己的路径与
// 请求构造。
func TestDoctorLookupEndpointPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{doctorEndpointChat, doctorPathChat},
		{doctorEndpointMessages, doctorPathMessages},
		{doctorEndpointResponses, doctorPathResponses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := lookupDoctorEndpoint(tc.name)
			if err != nil {
				t.Fatalf("查 %q 失败：%v", tc.name, err)
			}
			if spec.name != tc.name {
				t.Errorf("spec.name = %q，期望 %q", spec.name, tc.name)
			}
			if spec.path != tc.path {
				t.Errorf("%q 的路径 = %q，期望 %q", tc.name, spec.path, tc.path)
			}
			if spec.payload == nil || spec.setAuth == nil {
				t.Error("payload 与 setAuth 都要有，缺了真打上游那一步会失败")
			}
		})
	}
}

// TestDoctorLookupEndpointChoices 确认报错里那一串的渲染：顿号分隔、末项前是
// 「或」、名字不带引号。
func TestDoctorLookupEndpointChoices(t *testing.T) {
	const want = "chat、messages 或 responses"
	if got := doctorEndpointChoices(); got != want {
		t.Errorf("doctorEndpointChoices() = %q，期望 %q", got, want)
	}
}

// doctorEndpointEnumRE 认帮助文本里那串手写的枚举：`[--endpoint chat|messages|responses]`。
var doctorEndpointEnumRE = regexp.MustCompile(`\[--endpoint ([^\]]+)\]`)

// TestDoctorUsageMatchesEndpointTable 让帮助文本与入口表互相钉住。
//
// 帮助里那串枚举是手写的第二份（表不是它的来源，改 usage 要顺手改表，反之亦然），
// 没有这条用例时「给表加了一条、帮助没改」不会红任何东西；这里把两处逐字对上：
// 期望值由表拼出来，实际值必须原样出现在短版与长版 usage 里。
func TestDoctorUsageMatchesEndpointTable(t *testing.T) {
	want := strings.Join(doctorEndpointNames(), "|")
	usages := []struct {
		name string
		text string
	}{
		{"短版", doctorShortUsage},
		{"长版", doctorUsage},
	}
	for _, usage := range usages {
		matches := doctorEndpointEnumRE.FindAllStringSubmatch(usage.text, -1)
		if len(matches) != 1 {
			t.Fatalf("%s帮助里 --endpoint 的枚举写法应恰好一处，找到 %d 处", usage.name, len(matches))
		}
		if got := matches[0][1]; got != want {
			t.Errorf("%s帮助里 --endpoint 的取值 = %q，入口表是 %q：只改一处时这里必须红",
				usage.name, got, want)
		}
	}
}

// TestDoctorUsageDescribesEveryEndpoint 让 doctorUsage 说明段与入口表对上。
//
// 说明段里每个入口都写成「<名字> 是 POST /v1/…」。只给表加一条、两处方括号串也
// 同步了、却没改说明段时，TestDoctorUsageMatchesEndpointTable 是绿的；这条会红。
func TestDoctorUsageDescribesEveryEndpoint(t *testing.T) {
	for _, name := range doctorEndpointNames() {
		pattern := regexp.MustCompile(regexp.QuoteMeta(name) + `\s+是\s+POST\s+/v1/`)
		if !pattern.MatchString(doctorUsage) {
			t.Errorf("doctorUsage 的说明段里没有 %q 的「%s 是 POST /v1/…」描述：加入口时说明段漏改了", name, name)
		}
	}
}
