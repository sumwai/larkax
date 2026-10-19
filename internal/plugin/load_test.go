package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
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

// allCaps 是宿主认识的全部能力名。
//
// 这一批用例测的是「跳过」的机制本身，不是能力收窄，所以给一份最宽的清单——
// 收窄那一条另有用例（见 TestPublishPartialSkipsPluginAskingUnprovidedCapability）。
func allCaps() []string { return sandbox.CapabilityNames() }

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
	reg, err := PublishPartial(regs, allCaps(), func(r Registration, _ error) { skipped = append(skipped, r) })
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
	}, allCaps(), nil)
	if err != nil {
		t.Fatalf("全部编译不过时不该返回错误: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("一个都不该上线，实际 %d 个", reg.Len())
	}
}

// 空输入：注册表是空的，没有错误。库里的 plugins 表是空的时候走的就是这一条。
func TestPublishPartialEmpty(t *testing.T) {
	reg, err := PublishPartial(nil, allCaps(), nil)
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("空输入的注册表该是空的，实际 %d 个", reg.Len())
	}
}

// onSkip 为 nil 时不能崩：只有测试会这么用，但崩掉的位置会离原因很远。
func TestPublishPartialNilCallback(t *testing.T) {
	reg, err := PublishPartial([]Registration{{Key: "bad", Source: brokenSource}}, allCaps(), nil)
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
	}, allCaps(), nil)
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
	}, allCaps(), nil)
	if err == nil || !strings.Contains(err.Error(), "注册了两次") {
		t.Fatalf("同名两条该报错，实际 %v", err)
	}
}

// 判据：插件申请了这份装配**没有**的能力时，它在注册期就被跳过，
// 而不是注册成功、运行期拿到一个 undefined。
//
// 这一条是「允许集由装配推导」在加载这一层的落点：provided 是唯一判据，
// 这里给一份只有 log 的清单（模拟一个没接日历与 KV 的装配），于是申请 kv 的
// 插件进不来，而错误里说得出是哪个能力、这一份清单里有什么。
//
// 正向的另一半在同一条用例里：同样的源码，清单里给了 kv 就上线。少了它，
// 「一个都不发布」的实现也能过。
func TestPublishPartialSkipsPluginAskingUnprovidedCapability(t *testing.T) {
	regs := []Registration{{
		Key:    "wants-kv",
		Source: src(`key:"wants-kv", apiVersion:1, capabilities:["kv"],`, `onUsage(){ return {}; }`),
	}}

	var (
		skipped []Registration
		cause   error
	)
	reg, err := PublishPartial(regs, []string{sandbox.CapabilityLog},
		func(r Registration, e error) { skipped = append(skipped, r); cause = e })
	if err != nil {
		t.Fatalf("插件申请了没提供的能力，不该让整批失败: %v", err)
	}
	if _, ok := reg.Get("wants-kv"); ok {
		t.Error("申请了这份装配没提供的能力的插件不该上线")
	}
	if len(skipped) != 1 || skipped[0].Key != "wants-kv" {
		t.Fatalf("该恰好跳过 wants-kv，实际 %v", keysOf(skipped))
	}
	// 这一类要能与「源码编译不过」分开：申请了没提供的能力是**部署**的问题
	// （把那条线接上），语法错是**插件**的问题（改源码），两者的下一步动作不同。
	if !errors.Is(cause, ErrMetaCapability) {
		t.Errorf("跳过的原因 = %v，期望包住 ErrMetaCapability", cause)
	}
	for _, want := range []string{"kv", sandbox.CapabilityLog} {
		if cause == nil || !strings.Contains(cause.Error(), want) {
			t.Errorf("原因里没有 %q：%v（要能看出缺的是哪个能力、这份清单里有什么）", want, cause)
		}
	}

	// 清单里给了 kv 就上线，且生效的能力清单就是它申请的那一个。
	reg, err = PublishPartial(regs, []string{sandbox.CapabilityLog, sandbox.CapabilityKV}, nil)
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	p, ok := reg.Get("wants-kv")
	if !ok {
		t.Fatal("清单里提供了 kv，这个插件该上线")
	}
	if len(p.Capabilities) != 1 || p.Capabilities[0] != sandbox.CapabilityKV {
		t.Errorf("生效的能力 = %v，期望 [%s]", p.Capabilities, sandbox.CapabilityKV)
	}
}

// 判据：PublishAll 是**整批拒绝**——一条编译不过，整批都不发布。
//
// 这一条与 TestPublishPartialSkipsBrokenPlugin 成对：同一批数据在两条路上给出
// 相反的结论，而结论正是热更新与启动路径要分开的那个判据。好的两个也必须不在
// 返回的注册表里（此时返回 nil），否则「整批拒绝」就成了「跳过坏的、留下一半」。
func TestPublishAllRejectsWholeBatch(t *testing.T) {
	regs := []Registration{
		goodReg("good-a", 0),
		{Key: "broken", Source: brokenSource},
		goodReg("good-b", 0),
	}

	var failed []Registration
	reg, err := PublishAll(regs, allCaps(), func(r Registration, _ error) { failed = append(failed, r) })
	if err == nil {
		t.Fatal("有一条编译不过时该返回错误：这批不能发布")
	}
	if reg != nil {
		t.Fatalf("失败时不该交出注册表，实际拿到 %d 个插件", reg.Len())
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("错误信息 %q 里没有坏插件的 key", err.Error())
	}
	if len(failed) != 1 || failed[0].Key != "broken" {
		t.Fatalf("该恰好报一条 broken 的失败，实际 %s", keysOf(failed))
	}
}

// 判据：PublishAll 按调用方给的能力清单收窄，与启动路径同一口径。
//
// 与 PublishPartial 那条用例的区别只在「失败时整批不发布」；能力判据本身
// 不能因此分成两份。
func TestPublishAllNarrowsByProvided(t *testing.T) {
	regs := []Registration{{
		Key:    "wants-kv",
		Source: src(`key:"wants-kv", apiVersion:1, capabilities:["kv"],`, `onUsage(){ return { promptTokens: 1 }; }`),
	}}

	reg, err := PublishAll(regs, []string{sandbox.CapabilityLog}, nil)
	if err == nil {
		t.Fatal("这份清单没提供 kv，申请 kv 的插件该让整批失败")
	}
	if reg != nil {
		t.Fatalf("失败时不该交出注册表，实际拿到 %d 个插件", reg.Len())
	}
	if !strings.Contains(err.Error(), "kv") {
		t.Errorf("错误信息 %q 里没有缺的能力名", err.Error())
	}

	reg, err = PublishAll(regs, []string{sandbox.CapabilityLog, sandbox.CapabilityKV}, nil)
	if err != nil {
		t.Fatalf("清单里给了 kv，这一批该发布: %v", err)
	}
	p, ok := reg.Get("wants-kv")
	if !ok {
		t.Fatal("清单里给了 kv，这个插件该在注册表里")
	}
	if len(p.Capabilities) != 1 || p.Capabilities[0] != sandbox.CapabilityKV {
		t.Errorf("生效的能力 = %v，期望 [%s]", p.Capabilities, sandbox.CapabilityKV)
	}
}

// 空输入：一份空注册表，不是错误。库里一个插件都没有时热更新走的就是这一条。
func TestPublishAllEmpty(t *testing.T) {
	reg, err := PublishAll(nil, allCaps(), nil)
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if reg == nil || reg.Len() != 0 {
		t.Fatalf("空输入该得到一份空注册表，实际 %v", reg)
	}
}

func keysOf(regs []Registration) string {
	keys := make([]string, 0, len(regs))
	for _, r := range regs {
		keys = append(keys, r.Key)
	}
	return strings.Join(keys, "、")
}
