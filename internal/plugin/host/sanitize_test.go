package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 本文件钉宿主能力的**错误出口**的宿主这一侧。
//
// 依赖错误的原文（连接串、驱动细节、上游地址）对插件没有用，也不该离开宿主，
// 所以它只进 Host.OnError，插件拿到的是分类码加一句固定文案。
// sandbox/sanitize_test.go 钉的是那条边界的机制，这里钉的是**这个包有没有
// 按它来**：机制正确而调用方仍用 fmt.Errorf 原样返回，效果是一样的。
//
// 记号是一眼看得出不该外泄的串，不是像真凭证的东西：断言的是「它没出现」，
// 用真形态的取值反而会让红线扫描器报警。
const 宿主原文记号 = "dsn-fragment-MUST-NOT-LEAK"

// 调用 跑一个宿主函数，只取错误——本文件只关心错误出口。
func 调用(fn func(context.Context, []moejs.Value) (moejs.Value, error), args []moejs.Value) error {
	_, err := fn(context.Background(), args)
	return err
}

// 断言分类 检查一个错误：分类码正确、原文不在 Error() 里、文案非空。
//
// 原文传 nil 表示这个错误没有 Cause（校验失败那一类），此时跳过 errors.Is。
func 断言分类(t *testing.T, err error, code string, 原文 error) {
	t.Helper()
	if err == nil {
		t.Fatal("期望报错，实际没有")
	}
	var ce *sandbox.CapabilityError
	if !errors.As(err, &ce) {
		t.Fatalf("错误 %v 不是 CapabilityError：插件会直接看到它的原文", err)
	}
	if ce.Code != code {
		t.Errorf("分类码 = %q，想要 %q", ce.Code, code)
	}
	if strings.Contains(err.Error(), 宿主原文记号) {
		t.Errorf("插件会看到宿主原文: %q", err.Error())
	}
	if ce.Message == "" {
		t.Error("固定文案不能为空：插件至少要能看出这是哪一类问题")
	}
	// 分类不是把信息丢掉：上层要按原始错误决定重试与记账。
	if 原文 != nil && !errors.Is(err, 原文) {
		t.Errorf("分类之后 errors.Is 断了：%v 不再包住 %v", err, 原文)
	}
}

// KV 的三个函数在依赖失败时都要分类，而不是把存储层的话原样转给插件。
func TestKV依赖错误不回显原文(t *testing.T) {
	原文 := errors.New(宿主原文记号)
	store := newFakeKV()
	store.err = 原文
	env := kvEnv(store, "p", 7)

	cases := []struct {
		name string
		args []moejs.Value
	}{
		{"get", js("k")},
		{"set", js("k", "v")},
		{"del", js("k")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			断言分类(t, 调用(kvFunc(t, env, tc.name), tc.args),
				sandbox.CodeUnavailable, 原文)
		})
	}
}

// 日历的六个函数同理。它们在插件侧是同一个命名空间下的不同问题，
// 但依赖失败的原因与处置是同一个。
func TestCalendar依赖错误不回显原文(t *testing.T) {
	原文 := errors.New(宿主原文记号)
	cal := &fakeCalendar{err: 原文, peakErr: 原文}
	env := calEnv(cal)

	cases := []struct {
		name string
		args []moejs.Value
	}{
		{"kind", js(int64(testInstant.Unix()), "CN")},
		{"isOffDay", js(int64(testInstant.Unix()), "CN")},
		{"isMakeupWorkday", js(int64(testInstant.Unix()), "CN")},
		{"holidayName", js(int64(testInstant.Unix()), "CN")},
		{"hourIn", js("CN")},
		{"weekdayIn", js("CN")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			断言分类(t, 调用(capability(t, Standard(env), capCalendar).Funcs[tc.name], tc.args),
				sandbox.CodeUnavailable, 原文)
		})
	}
}

// 校验失败的文案要保留：那是「插件写错了」，文案得指到具体是哪一处，
// 否则插件作者只能看到一个「参数不合法」。
//
// 它与依赖失败的区别在**分类码**上，而不在「能不能说清楚」上——
// 消毒不等于把信息全丢了。
func Test校验失败的文案保留给插件(t *testing.T) {
	env := kvEnv(newFakeKV(), "p", 7)

	cases := []struct {
		name string
		args []moejs.Value
		want string
	}{
		{"键为空", js("", "v"), "键"},
		{"键超长", js(strings.Repeat("k", kvKeyMaxBytes+1), "v"), "上限"},
		{"值超长", js("k", strings.Repeat("v", kvValueMaxBytes+1)), "上限"},
		{"ttl 为负", js("k", "v", int64(-1)), "负数"},
		{"ttl 不是整数", js("k", "v", "abc"), "ttlSeconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := 调用(kvFunc(t, env, "set"), tc.args)
			断言分类(t, err, sandbox.CodeBadArgument, nil)

			var ce *sandbox.CapabilityError
			errors.As(err, &ce)
			if !strings.Contains(ce.Message, tc.want) {
				t.Errorf("文案 %q 里没有 %q：插件看不出错在哪", ce.Message, tc.want)
			}
		})
	}
}

// 时间戳缺失/非整数走的是同一个出口，文案里要带上它叫什么：
// 「时间戳 不是整数」比「第 1 个参数不是整数」有用得多。
func Test日历参数校验的文案带参数名(t *testing.T) {
	env := calEnv(&fakeCalendar{})

	for _, tc := range []struct {
		name string
		args []moejs.Value
		want string
	}{
		{"缺时间戳", js(), "时间戳"},
		{"时间戳非整数", js("昨天"), "时间戳"},
		{"地区为空", js(int64(testInstant.Unix()), ""), "地区"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := 调用(capability(t, Standard(env), capCalendar).Funcs["kind"], tc.args)
			断言分类(t, err, sandbox.CodeBadArgument, nil)

			var ce *sandbox.CapabilityError
			errors.As(err, &ce)
			if !strings.Contains(ce.Message, tc.want) {
				t.Errorf("文案 %q 里没有 %q", ce.Message, tc.want)
			}
		})
	}
}
