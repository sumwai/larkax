package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/catalog"
)

// TestModelAddRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestModelAddRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --canonical", []string{"--display", "展示名"}, "缺少 --canonical"},
		{"没有 --display", []string{"--canonical", "m1"}, "缺少 --display"},
		{"日期不是 YYYY-MM-DD", []string{
			"--canonical", "m1", "--display", "展示名",
			"--knowledge-cutoff", "2026/01/31",
		}, "YYYY-MM-DD"},
		{"多出位置参数", []string{"--canonical", "m1", "--display", "展示名", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runModelAdd(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestModelAddThenList 钉住规范模型的写入与列举。
//
// 写进去的值直接从读取路径读回来核对——命令行这一层只是把字符串变成包要的
// 结构，所以「写对了」这件事只能由「读出来对得上」证明。
func TestModelAddThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	display := canonical + "-展示名"

	stdout, _ := captureStreams(t, func() {
		if err := runModelAdd([]string{
			"--canonical", canonical, "--display", display,
			"--vendor", "example-vendor", "--family", "example-family",
			"--context-limit", "200000", "--output-limit", "64000",
			"--tools", "--vision", "--cache=false",
			"--modalities-input", "text,image", "--modalities-output", "text",
			"--knowledge-cutoff", "2026-01-31",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是模型 ID：%q（%v）", stdout, err)
	}

	got, err := catalog.New(db).Model(context.Background(), canonical)
	if err != nil {
		t.Fatalf("读回刚写进去的模型失败: %v", err)
	}
	if got.ID != id {
		t.Errorf("读回来的 ID = %d，期望 %d", got.ID, id)
	}
	if got.DisplayName != display {
		t.Errorf("展示名 = %q，期望 %q", got.DisplayName, display)
	}
	if got.Vendor != "example-vendor" || got.Family != "example-family" {
		t.Errorf("厂商/家族 = %q/%q", got.Vendor, got.Family)
	}
	if got.ContextLimit != 200000 || got.OutputLimit != 64000 {
		t.Errorf("上下文/输出上限 = %d/%d", got.ContextLimit, got.OutputLimit)
	}
	// 没给 --input-limit：0 表示「未知」，落库为 NULL，读回来还是 0。
	if got.InputLimit != 0 {
		t.Errorf("没填的输入上限应当是 0（未知），得到 %d", got.InputLimit)
	}
	// 给了的开关落成「支持」/「不支持」，没给的落成**未知**（库里 NULL），
	// 不是 false。这一行是这次改动的核心：`model add` 不勾的开关不能变成
	// 「明确不支持」——那会让带工具的请求选不到一条刚建好的记录（见 #485）。
	want := "reasoning=未知 tools=支持 vision=支持 audio=未知 cache=不支持 batch=未知"
	if gotState := declaredText(got.Capabilities); gotState != want {
		t.Errorf("能力 = %s，期望 %s", gotState, want)
	}
	if len(got.ModalitiesInput) != 2 || got.ModalitiesInput[0] != "text" || got.ModalitiesInput[1] != "image" {
		t.Errorf("输入模态 = %v", got.ModalitiesInput)
	}
	if got.KnowledgeCutoff == nil || got.KnowledgeCutoff.Format("2006-01-02") != "2026-01-31" {
		t.Errorf("知识截止日期 = %v", got.KnowledgeCutoff)
	}

	listOut, _ := captureStreams(t, func() {
		if listErr := runModelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	for _, want := range []string{canonical, display, "example-vendor",
		"tools=支持 vision=支持 cache=不支持", "text,image→text"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("清单里没有 %q：\n%s", want, listOut)
		}
	}
	// 清单那一栏只列已声明的项：没配过的打不出来，六项都没配过时打 -。
	// 「未知」这个词只该出现在 show 里——清单里铺六项会把这一栏撑得比其余
	// 九栏加起来还宽（见 capabilitySummary）。
	if strings.Contains(listOut, "未知") {
		t.Errorf("清单不该打出「未知」：\n%s", listOut)
	}
}

// TestModelUpdateRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestModelUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --canonical", []string{"--display", "展示名"}, "缺少 --canonical"},
		{"没有要改的字段", []string{"--canonical", "m1"}, "至少要改一项"},
		{"日期不是 YYYY-MM-DD", []string{
			"--canonical", "m1", "--release-date", "2026/01/31",
		}, "YYYY-MM-DD"},
		{"多出位置参数", []string{
			"--canonical", "m1", "--vendor", "v", "extra",
		}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runModelUpdate(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestModelUpdateMergesCapabilities 钉住「只给一个能力标志不会抹掉另外五个」。
//
// 写入路径对那六列是一次**全量覆盖**（见 catalog.modelUpdateSets），所以
// 命令这一层必须先读出当前那一行再合并。少了那一步，`model update --reasoning`
// 会把 tools / vision / cache 一起关掉——而它是一次静默的能力删除：
// 命令成功、退出码 0、库里少了三个能力，只有下一个用到它们的请求会失败。
func TestModelUpdateMergesCapabilities(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{
			"--canonical", canonical, "--display", "展示名",
			"--tools", "--vision", "--cache",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})

	// 只给 --reasoning：另外三个能力原样留着，而没配过的两项**仍是未知**
	// ——不是被这次全量覆盖折成 false。
	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{"--canonical", canonical, "--reasoning"}); err != nil {
			t.Fatalf("update 失败: %v", err)
		}
	})
	want := "reasoning=支持 tools=支持 vision=支持 audio=未知 cache=支持 batch=未知"
	if got := declaredText(readModel(t, db, canonical).Capabilities); got != want {
		t.Errorf("能力 = %s，期望 %s：只给 --reasoning 不该动另外五个", got, want)
	}

	// --tools=false 关掉一个：其余五个不动。
	//
	// 这一条同时钉住另一件事：`--tools=false` 与「没提 --tools」必须分得开，
	// 而 flag 包的零值分不出它们——判据是 fs.Visit 记下的清单。判据写错时，
	// 这个标志会静默地永远算「没给」，表现是「关不掉一个能力」。
	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{"--canonical", canonical, "--tools=false"}); err != nil {
			t.Fatalf("update --tools=false 失败: %v", err)
		}
	})
	want = "reasoning=支持 tools=不支持 vision=支持 audio=未知 cache=支持 batch=未知"
	if got := declaredText(readModel(t, db, canonical).Capabilities); got != want {
		t.Errorf("能力 = %s，期望 %s：--tools=false 应当只关掉 tools", got, want)
	}
}

// TestModelUpdateClearsFields 钉住「空串与 0 在这里是有含义的写法」。
//
// update 的每一栏都要能表达「清空」，而 nil 在那个位置上已经被「不改这一项」
// 占着了。所以清空走的是值本身：--vendor ” 与 --context-limit 0 落成 NULL，
// --knowledge-cutoff ” 落成 NULL，--modalities-input ” 落成空数组。
// 同时钉住反面：没提到的字段一个都不许动。
func TestModelUpdateClearsFields(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	display := canonical + "-展示名"
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{
			"--canonical", canonical, "--display", display,
			"--vendor", "example-vendor", "--family", "example-family",
			"--context-limit", "200000", "--modalities-input", "text,image",
			"--knowledge-cutoff", "2026-01-31",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})

	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{
			"--canonical", canonical,
			"--vendor", "", "--context-limit", "0",
			"--modalities-input", "", "--knowledge-cutoff", "",
		}); err != nil {
			t.Fatalf("update 失败: %v", err)
		}
	})

	got := readModel(t, db, canonical)
	if got.Vendor != "" {
		t.Errorf("厂商应当被清空，得到 %q", got.Vendor)
	}
	if got.ContextLimit != 0 {
		t.Errorf("上下文上限应当被清空（0 表示未知），得到 %d", got.ContextLimit)
	}
	if len(got.ModalitiesInput) != 0 {
		t.Errorf("输入模态应当被清空，得到 %v", got.ModalitiesInput)
	}
	if got.KnowledgeCutoff != nil {
		t.Errorf("知识截止日期应当被清空，得到 %v", got.KnowledgeCutoff)
	}
	if got.DisplayName != display {
		t.Errorf("展示名 = %q，期望 %q（没提到它就不该改）", got.DisplayName, display)
	}
	if got.Family != "example-family" {
		t.Errorf("家族 = %q，期望 example-family（没提到它就不该改）", got.Family)
	}
}

// TestModelShow 钉住 show 打出来的那一份。
func TestModelShow(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{
			"--canonical", canonical, "--display", "展示名",
			"--vendor", "example-vendor", "--context-limit", "200000",
			"--tools", "--vision=false", "--knowledge-cutoff", "2026-01-31",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})
	id := readModel(t, db, canonical).ID

	out, _ := captureStreams(t, func() {
		if err := runModelShow([]string{"--canonical", canonical}); err != nil {
			t.Fatalf("show 失败: %v", err)
		}
	})
	for _, want := range []string{
		strconv.FormatInt(id, 10), canonical, "展示名", "example-vendor",
		"200000", "2026-01-31",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show 的输出里没有 %q：\n%s", want, out)
		}
	}
	// 能力那一栏逐项打三态，六项都打出来。三种状态在这一份输出里齐了：
	// tools 支持、vision 不支持、其余四项未知。少了「未知」这一档，一份
	// 只填了名字的记录看起来就和「六项都不支持」一样（见 capabilityDetail）。
	for _, want := range []string{"tools=支持", "vision=不支持", "reasoning=未知", "batch=未知"} {
		if !strings.Contains(out, want) {
			t.Errorf("show 的能力那一栏里没有 %q：\n%s", want, out)
		}
	}
	// 没填的那几栏打的是「-」而不是 0：打 0 会让人以为那是一个量出来的上限。
	if !strings.Contains(out, "输入上限") {
		t.Errorf("show 的输出里应当有「输入上限」那一行：\n%s", out)
	}
}

// TestModelShowMissing 钉住「要查的那一行不存在」的退出码。
//
// 它必须与「其他失败」分开：前者是名字写错了或者模型被删了，下一步是去看
// 目录里现在有什么；后者是环境问题。混成一个码，脚本只能去匹配错误文本。
func TestModelShowMissing(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runModelShow([]string{"--canonical", uniqueName("nonexistent")})
	})
	if err == nil {
		t.Fatal("不存在的规范名应当报错")
	}
	if !errors.Is(err, catalog.ErrModelNotFound) {
		t.Errorf("期望 catalog.ErrModelNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码应当是 %d，得到 %d", exitConflict, got)
	}
}

// TestModelDeleteRefusesWhenReferenced 钉住删除的引用检查。
//
// 规范名是计价规则挂靠的键，所以被引用时删除要被拒，而且那条错误里要带着
// 「各有多少条」——不然使用者只知道删不掉，不知道下一步该去哪解引用。
// 命令这一层不许把它换成一句泛泛的失败，也不许绕过它直接删。
func TestModelDeleteRefusesWhenReferenced(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{"--canonical", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})
	aliasOut, _ := captureStreams(t, func() {
		if err := runAliasAdd([]string{
			"--canonical", canonical, "--upstream", uniqueName("upstream"),
		}); err != nil {
			t.Fatalf("alias add 失败: %v", err)
		}
	})
	aliasID, err := strconv.ParseInt(strings.TrimSpace(aliasOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是别名 ID：%q（%v）", aliasOut, err)
	}

	delErr := runCaptured(t, func() error {
		return runModelDelete([]string{"--canonical", canonical})
	})
	if delErr == nil {
		t.Fatal("还被别名引用着的规范名不该删得掉")
	}
	if !errors.Is(delErr, catalog.ErrModelInUse) {
		t.Errorf("期望 catalog.ErrModelInUse，得到 %v", delErr)
	}
	if !strings.Contains(delErr.Error(), "1 条别名") {
		t.Errorf("错误里应当报出被多少条别名引用：%v", delErr)
	}
	if got := exitCode(delErr); got != exitConflict {
		t.Errorf("退出码应当是 %d，得到 %d", exitConflict, got)
	}

	// 解掉引用之后删得掉。
	_, _ = captureStreams(t, func() {
		if deleteErr := runAliasDelete([]string{"--id", strconv.FormatInt(aliasID, 10)}); deleteErr != nil {
			t.Fatalf("alias delete 失败: %v", deleteErr)
		}
	})
	_, _ = captureStreams(t, func() {
		if deleteErr := runModelDelete([]string{"--canonical", canonical}); deleteErr != nil {
			t.Fatalf("解除引用之后应当删得掉: %v", deleteErr)
		}
	})
	if _, readErr := catalog.New(db).Model(context.Background(), canonical); !errors.Is(readErr, catalog.ErrModelNotFound) {
		t.Errorf("删完之后读回来应当是 ErrModelNotFound，得到 %v", readErr)
	}

	// 再删一次：那一行已经不在了。
	err = runCaptured(t, func() error {
		return runModelDelete([]string{"--canonical", canonical})
	})
	if !errors.Is(err, catalog.ErrModelNotFound) {
		t.Errorf("重复删除期望 ErrModelNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("重复删除的退出码应当是 %d，得到 %d", exitConflict, got)
	}
}

// readModel 按规范名读回一条模型，读不到就直接失败。
func readModel(t *testing.T, db *sql.DB, canonical string) catalog.Model {
	t.Helper()
	model, err := catalog.New(db).Model(context.Background(), canonical)
	if err != nil {
		t.Fatalf("读回模型 %q 失败: %v", canonical, err)
	}
	return model
}

// declared 造一项**已声明**的能力：true 支持、false 明确不支持。
//
// 「没配过」是第三个状态，直接写 nil（不经过这个助手）——用例里必须能把
// 这三态分开写出来，否则证明不了它们没被合并成一个零值（见 #485）。
func declared(v bool) *bool { return &v }

// declaredText 把六项三态打成一行（顺序固定），供用例按值比较。
//
// 不能拿这份 struct 直接 `!=`：它装的是指针，那比的是**地址**，两份内容
// 相同的声明会因为地址不同而判不等。也不能拿渲染函数（capabilityDetail）
// 比：那是被测代码的一部分，它错了断言会跟着一起错。所以这份小渲染写在
// 用例里，当独立的那把尺子。
func declaredText(c catalog.Capabilities) string {
	state := func(item *bool) string {
		switch {
		case item == nil:
			return "未知"
		case *item:
			return "支持"
		default:
			return "不支持"
		}
	}
	return "reasoning=" + state(c.Reasoning) +
		" tools=" + state(c.Tools) +
		" vision=" + state(c.Vision) +
		" audio=" + state(c.Audio) +
		" cache=" + state(c.Cache) +
		" batch=" + state(c.Batch)
}

// TestModelAddRejectsDuplicateCanonical 钉住重复的规范名。
//
// 它是选路与计费共用的键，重复的键会让「这次请求按哪个模型算」没有答案，
// 所以必须撞在唯一键上，而不是静默地多出一行。
func TestModelAddRejectsDuplicateCanonical(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	add := func() error {
		return runCaptured(t, func() error {
			return runModelAdd([]string{"--canonical", canonical, "--display", "展示名"})
		})
	}

	if err := add(); err != nil {
		t.Fatalf("第一次 add 应当成功: %v", err)
	}
	err := add()
	if err == nil {
		t.Fatal("重复的规范名应当被拒")
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("重复的规范名是约束冲突，退出码应当是 %d，得到 %d（%v）", exitConflict, got, err)
	}
}
