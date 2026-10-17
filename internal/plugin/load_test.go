package plugin

import (
	"strings"
	"testing"
)

// 一段编译不过的源码。语法错是最常见的一种，而它在 PublishPartial 眼里
// 与「元数据不合法」是同一类处置：这个插件不上，别的照常。
const brokenSource = "export default { onUsage() { "

// goodReg 造一条能编译过的注册。
func goodReg(key string, priority int) Registration {
	return Registration{
		Key:      key,
		Source:   src(`key:"`+key+`", apiVersion:1,`, `onUsage(){ return { promptTokens: 1 }; }`),
		Priority: priority,
	}
}

// 判据：一个坏插件不能让整批都上不了线。
//
// 这是启动路径选「跳过」而不是「整体拒绝」的落点。整批拒绝时这个用例会挂在
// 「注册失败」上——而那正是线上会发生的事：库里有一条坏插件，服务起不来。
func TestPublishPartialSkipsBrokenPlugin(t *testing.T) {
	regs := []Registration{
		goodReg("good-a", 0),
		{Key: "broken", Source: brokenSource},
		goodReg("good-b", 0),
	}

	var skipped []Registration
	reg, err := PublishPartial(regs, func(r Registration, _ error) { skipped = append(skipped, r) })
	if err != nil {
		t.Fatalf("坏插件不该让整批失败: %v", err)
	}

	// 好的两个都要在。
	for _, key := range []string{"good-a", "good-b"} {
		if _, ok := reg.Get(key); !ok {
			t.Errorf("插件 %s 该上线，实际没在注册表里", key)
		}
	}
	// 坏的必须不在，而且要能被调用方点名（日志与 plugin list 靠它）。
	if _, ok := reg.Get("broken"); ok {
		t.Error("编译不过的插件不该进注册表")
	}
	if len(skipped) != 1 || skipped[0].Key != "broken" {
		t.Fatalf("该恰好跳过一个名为 broken 的插件，实际 %v", keysOf(skipped))
	}
}

// 全都是坏的：注册表是空的，但**不是错误**。
//
// 这一条分辨的是「跳过」与「整体拒绝」两种实现：拒绝那一版在这里会返回错误，
// 而跳过这一版返回一份空表。空表与「一个插件都没配」在下游长得一样，
// 所以加载器必须另有一句计数日志（见 store.LoadRegistry）。
func TestPublishPartialAllBroken(t *testing.T) {
	reg, err := PublishPartial([]Registration{
		{Key: "bad-a", Source: brokenSource},
		{Key: "bad-b", Source: brokenSource},
	}, nil)
	if err != nil {
		t.Fatalf("全部编译不过时不该返回错误: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("一个都不该上线，实际 %d 个", reg.Len())
	}
}

// 空输入：注册表是空的，没有错误。库里的 plugins 表是空的时候走的就是这一条。
func TestPublishPartialEmpty(t *testing.T) {
	reg, err := PublishPartial(nil, nil)
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("空输入的注册表该是空的，实际 %d 个", reg.Len())
	}
}

// onSkip 为 nil 时不能崩：只有测试会这么用，但崩掉的位置会离原因很远。
func TestPublishPartialNilCallback(t *testing.T) {
	reg, err := PublishPartial([]Registration{{Key: "bad", Source: brokenSource}}, nil)
	if err != nil {
		t.Fatalf("onSkip 为 nil 不该报错: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("坏插件不该上线，实际 %d 个", reg.Len())
	}
}

// 优先级来自库里那一行，跳过几个坏插件不该把它弄丢。
func TestPublishPartialKeepsPriority(t *testing.T) {
	reg, err := PublishPartial([]Registration{
		goodReg("low", 1),
		{Key: "broken", Source: brokenSource},
		goodReg("high", 9),
	}, nil)
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	got := reg.Plugins()
	if len(got) != 2 {
		t.Fatalf("该有 2 个插件，实际 %d 个", len(got))
	}
	if got[0].Key != "high" || got[1].Key != "low" {
		t.Fatalf("顺序该按优先级降序，实际 %s、%s", got[0].Key, got[1].Key)
	}
}

// 过滤之后剩下同名的两个：那是库里不可能出现的状态（UNIQUE (key, version)
// 与「每个 key 取代次最高的一行」一起挡着），但真出现了要报错而不是
// 悄悄留一个——留哪个取决于遍历顺序，而那是随机的。
func TestPublishPartialRejectsDuplicateKeys(t *testing.T) {
	_, err := PublishPartial([]Registration{
		goodReg("dup", 0),
		{Key: "dup", Source: src(`key:"dup", apiVersion:1,`, `onUsage(){ return {}; }`)},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "注册了两次") {
		t.Fatalf("同名两条该报错，实际 %v", err)
	}
}

func keysOf(regs []Registration) string {
	keys := make([]string, 0, len(regs))
	for _, r := range regs {
		keys = append(keys, r.Key)
	}
	return strings.Join(keys, "、")
}
