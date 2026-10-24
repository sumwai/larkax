package main

import "testing"

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
