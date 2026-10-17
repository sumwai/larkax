package sandbox

import (
	"regexp"
	"testing"
)

// capabilityCallRE 抓 sdkSource 里 `capability("名字")` 那几处**字面量**调用。
//
// `capability(name)`（那两行函数定义与它自己）不匹配：它收的是变量，
// 而这里要数的是「SDK 到底用了哪几个能力名」。
var capabilityCallRE = regexp.MustCompile(`capability\("([A-Za-z0-9_]+)"\)`)

// 判据：Go 这边那份能力名清单与 sdkSource 里用到的名字**一一对应**。
//
// 两个方向都要判，因为它们坏起来是两件事：
//
//   - 源码里用了而清单里没有：插件调用拿到「宿主能力 X 未启用」，而宿主其实
//     实现了它——排查的人会去看实现，而问题在清单。
//   - 清单里有而源码里没用到：注册期会放行一个插件申请不到的命名空间，
//     于是它申请成功、运行期拿到 undefined（正是这一片要修的那个坑）。
//
// 两处都在同一个包、同一个文件里，但它们是**两份字面量**：编译器不会因为
// 一边改了另一边没改而报错。
func TestCapabilityNamesMatchSDKSource(t *testing.T) {
	matches := capabilityCallRE.FindAllStringSubmatch(sdkSource, -1)
	// 一个都解析不出来时这份判据等于没跑：正则写坏、sdkSource 改名都会落到
	// 这里，而那时两个空集合「完全一致」（见 LESSONS.md 第 16 条）。
	if len(matches) == 0 {
		t.Fatal("sdkSource 里一个 capability(\"…\") 都没解析出来——判据本身失效了")
	}

	inSource := make(map[string]bool, len(matches))
	for _, m := range matches {
		inSource[m[1]] = true
	}
	inGo := make(map[string]bool, len(capabilityNames))
	for _, name := range CapabilityNames() {
		inGo[name] = true
	}

	for name := range inSource {
		if !inGo[name] {
			t.Errorf("SDK 源码里用了能力 %q，而 CapabilityNames() 里没有它", name)
		}
	}
	for name := range inGo {
		if !inSource[name] {
			t.Errorf("CapabilityNames() 里有 %q，而 SDK 源码里没有用到它："+
				"插件申请这个能力会注册成功、运行期拿到一个 undefined", name)
		}
	}
}

// CapabilityNames 返回的是一份副本：调用方排序或切片时不该动到包级那一份。
func TestCapabilityNamesReturnsCopy(t *testing.T) {
	first := CapabilityNames()
	if len(first) == 0 {
		t.Fatal("能力名清单是空的")
	}
	first[0] = "被改掉了"

	again := CapabilityNames()
	for _, name := range again {
		if name == "被改掉了" {
			t.Fatal("改动返回值影响到了包级那一份")
		}
	}
	if len(again) != len(capabilityNames) {
		t.Errorf("两次返回的个数不同: %d 与 %d", len(again), len(capabilityNames))
	}
}
