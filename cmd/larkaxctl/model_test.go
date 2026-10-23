package main

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
)

// TestModelCreateRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestModelCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --model", []string{"--display", "展示名"}, "缺少 --model"},
		{"没有 --display", []string{"--model", "m1"}, "缺少 --display"},
		{"日期不是 YYYY-MM-DD", []string{
			"--model", "m1", "--display", "展示名",
			"--knowledge-cutoff", "2026/01/31",
		}, "YYYY-MM-DD"},
		{"多出位置参数", []string{"--model", "m1", "--display", "展示名", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runModelCreate(tc.args) })
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

// TestModelCreateThenList 钉住规范模型的写入与列举。
//
// 写进去的值直接从读取路径读回来核对——命令行这一层只是把字符串变成包要的
// 结构，所以「写对了」这件事只能由「读出来对得上」证明。
func TestModelCreateThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	display := canonical + "-展示名"

	stdout, _ := captureStreams(t, func() {
		if err := runModelCreate([]string{
			"--model", canonical, "--display", display,
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
		{"没有 --model", []string{"--display", "展示名"}, "缺少 --model"},
		{"没有要改的字段", []string{"--model", "m1"}, "至少要改一项"},
		{"日期不是 YYYY-MM-DD", []string{
			"--model", "m1", "--release-date", "2026/01/31",
		}, "YYYY-MM-DD"},
		{"多出位置参数", []string{
			"--model", "m1", "--vendor", "v", "extra",
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
		if err := runModelCreate([]string{
			"--model", canonical, "--display", "展示名",
			"--tools", "--vision", "--cache",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})

	// 只给 --reasoning：另外三个能力原样留着，而没配过的两项**仍是未知**
	// ——不是被这次全量覆盖折成 false。
	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{"--model", canonical, "--reasoning"}); err != nil {
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
		if err := runModelUpdate([]string{"--model", canonical, "--tools=false"}); err != nil {
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
		if err := runModelCreate([]string{
			"--model", canonical, "--display", display,
			"--vendor", "example-vendor", "--family", "example-family",
			"--context-limit", "200000", "--modalities-input", "text,image",
			"--knowledge-cutoff", "2026-01-31",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})

	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{
			"--model", canonical,
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

// TestModelEndpointsFlagThreeStates 钉住 --endpoints 的三态（create 这一侧）。
//
// 这一栏上「没配过」与「明确一条都不支持」的结论相反：前者不参与选路判据，
// 后者筛掉所有候选。而 flag 包的零值分不出「没给这一项」与「给了空串」——
// 判据是 fs.Visit 记下的清单，判据写错时表现是「清空静默失效」。
func TestModelEndpointsFlagThreeStates(t *testing.T) {
	// 没给：这一栏留在零值 nil 上，写入路径落 NULL。
	req, err := parseModelCreate([]string{"--model", "m1", "--display", "展示名"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Capabilities.Endpoints != nil {
		t.Errorf("没给 --endpoints 时不该写这一栏，得到 %v", req.Capabilities.Endpoints)
	}

	// 给了空串：明确一条都不支持，落空切片——不是 nil。
	req, err = parseModelCreate([]string{"--model", "m1", "--display", "展示名", "--endpoints", ""})
	if err != nil {
		t.Fatal(err)
	}
	if req.Capabilities.Endpoints == nil {
		t.Error("--endpoints '' 是「明确一条都不支持」，应当落空切片而不是 nil")
	}
	if len(req.Capabilities.Endpoints) != 0 {
		t.Errorf("--endpoints '' 应当是空清单，得到 %v", req.Capabilities.Endpoints)
	}

	// 给了路径：按给的顺序留着。
	req, err = parseModelCreate([]string{
		"--model", "m1", "--display", "展示名",
		"--endpoints", "/v1/messages," + string(channel.EndpointResponses),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []channel.Endpoint{channel.EndpointMessages, channel.EndpointResponses}
	if !reflect.DeepEqual(req.Capabilities.Endpoints, want) {
		t.Errorf("端点 = %v，期望 %v", req.Capabilities.Endpoints, want)
	}

	// 取值写错：在连库之前就报错（这条路径不需要库）。
	if _, err := parseModelCreate([]string{
		"--model", "m1", "--display", "展示名", "--endpoints", "/v1/embeddings",
	}); err == nil {
		t.Error("写错的端点取值应当被拒")
	}
}

// TestModelUpdateEndpointsThreeStates 钉住 --endpoints 在 update 这一侧的三态，
// 以及它与六个能力**同组**这件事。
//
// 写入路径对这一组是一次全量覆盖（见 catalog.modelUpdateSets），所以命令
// 只给其中一项时必须先读出当前那一行再合并。合并这一步在这里直接测：
// 「只给 --endpoints」不能把六个能力抹掉，「只给 --tools」也不能把端点抹掉。
func TestModelUpdateEndpointsThreeStates(t *testing.T) {
	base := catalog.Capabilities{
		Tools:     declared(true),
		Endpoints: []channel.Endpoint{channel.EndpointMessages},
	}

	// 没给这一项：这一组没被动过，整组不写。
	_, edit, err := parseModelUpdate([]string{"--model", "m1", "--display", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if edit.given() {
		t.Error("只给了 --display 时，能力那一组不该算被动过")
	}

	// 给了路径：端点换成新的，六个能力原样留着。
	_, edit, err = parseModelUpdate([]string{
		"--model", "m1", "--endpoints", string(channel.EndpointResponses) + ",/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !edit.given() {
		t.Error("给了 --endpoints 就该算动过这一组")
	}
	merged := edit.apply(base)
	want := []channel.Endpoint{channel.EndpointResponses, channel.EndpointChatCompletions}
	if !reflect.DeepEqual(merged.Endpoints, want) {
		t.Errorf("端点 = %v，期望 %v", merged.Endpoints, want)
	}
	if merged.Tools == nil || !*merged.Tools {
		t.Error("只给 --endpoints 不该动六个能力")
	}

	// 给了六个能力里的一个：端点原样留着。
	_, edit, err = parseModelUpdate([]string{"--model", "m1", "--tools=false"})
	if err != nil {
		t.Fatal(err)
	}
	merged = edit.apply(base)
	if !reflect.DeepEqual(merged.Endpoints, base.Endpoints) {
		t.Errorf("只给 --tools 时端点应当原样留着，得到 %v", merged.Endpoints)
	}
	if merged.Tools == nil || *merged.Tools {
		t.Error("--tools=false 应当把 tools 置成不支持")
	}

	// 给了空串：明确一条都不支持——空切片，不是「不改这一栏」。
	_, edit, err = parseModelUpdate([]string{"--model", "m1", "--endpoints", ""})
	if err != nil {
		t.Fatal(err)
	}
	merged = edit.apply(base)
	if merged.Endpoints == nil {
		t.Error("--endpoints '' 应当落成空切片，而不是把库里那一栏留着")
	}
	if len(merged.Endpoints) != 0 {
		t.Errorf("--endpoints '' 应当是空清单，得到 %v", merged.Endpoints)
	}
}

// TestModelEndpointsFlagRejectsUnknownValue 钉住写错取值时的那条报错。
//
// 取值只有四种，报错里要把它们列全：这一栏没有「尽量读一个能用的值」这一说
// （见 channel.ParseEndpoint），而写错的人需要当场看到该写什么。两个写命令
// 都要如此——它们各自解析一次，漏一处的那一边就变成「静默地不生效」。
func TestModelEndpointsFlagRejectsUnknownValue(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
	}{
		{"create", func() error {
			return runModelCreate([]string{
				"--model", "m1", "--display", "展示名", "--endpoints", "/v1/embeddings",
			})
		}},
		{"update", func() error {
			return runModelUpdate([]string{"--model", "m1", "--endpoints", "/v1/embeddings"})
		}},
	}

	// 取值清单从 channel 包那份取，不在用例里再抄一遍——抄的那一份在清单变动
	// 之后不会跟着变，于是这条用例会替**漏改的那一处**开绿灯。
	valid := channel.EndpointValues

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.run)
			if err == nil {
				t.Fatal("写错的端点取值应当报错")
			}
			for _, endpoint := range valid {
				if !strings.Contains(err.Error(), string(endpoint)) {
					t.Errorf("报错里应当列出 %s，得到 %v", endpoint, err)
				}
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码应当是 %d（参数错），得到 %d", exitUsage, got)
			}
		})
	}
}

// TestModelUsageListsEveryEndpointValue 钉住用法文本里列了那四种取值。
//
// 命令的用法文本是使用者查「--endpoints 能写什么」的地方（另一处是解析失败
// 的那条报错，那句由 channel.ParseEndpoint 拼）。文本里少列一条，写的人就
// 只能靠试。
func TestModelUsageListsEveryEndpointValue(t *testing.T) {
	for _, endpoint := range []channel.Endpoint{
		channel.EndpointChatCompletions,
		channel.EndpointResponses,
		channel.EndpointMessages,
		channel.EndpointSystemOne,
	} {
		if !strings.Contains(modelUsage, string(endpoint)) {
			t.Errorf("model 的用法文本里没有列出端点取值 %s", endpoint)
		}
	}
}

// TestModelUsageEndpointListIsDerived 判用法文本里那一串取值是不是**从清单派生**的。
//
// 与上一条的分工：上一条判「那几条取值都出现在文本里」——手抄一遍也能过，而它
// 留着正好：清单本身不该由用例替它说话（那份清单在 internal/channel，那边有
// 用例判它与常量声明一一对上）。这一条判的是那一串**就是清单本身**：独占一行、
// 逐项、按顺序、不多不少。
//
// 顺序与条数都要判：「都在里面」判不出「顺序变了」与「多列了一条」，而那两种
// 正是两份清单分岔时最先出现的形状。
func TestModelUsageEndpointListIsDerived(t *testing.T) {
	// 清单**独占一行**：夹在长句里会被折行与相邻的字搅在一起，而它正是使用者
	// 要照着抄的那一串。
	onOwnLine := false
	for _, line := range strings.Split(modelUsage, "\n") {
		if strings.TrimSpace(line) == channel.EndpointValueList {
			onOwnLine = true
		}
	}
	if !onOwnLine {
		t.Errorf("用法文本里没有一行正好是清单 %q", channel.EndpointValueList)
	}

	// 每一条取值在整段用法文本里**只出现一次**：多出来的那一次就是别处又抄了
	// 一遍，而那一份不会跟着清单变。
	for _, e := range channel.EndpointValues {
		if n := strings.Count(modelUsage, string(e)); n != 1 {
			t.Errorf("用法文本里 %s 出现了 %d 次，期望 1 次——别处又列了一遍取值", e, n)
		}
	}
}

// 三种状态必须打得能分辨：「没配过」不参与选路判据，「明确一条都不支持」
// 筛掉所有候选。都打成 - 或都打成空，使用者就分不出自己手上这一条是哪种。
func TestModelEndpointDetailThreeStates(t *testing.T) {
	cases := []struct {
		name      string
		endpoints []channel.Endpoint
		want      string
	}{
		{"没配过", nil, "未知"},
		{"明确一条都不支持", []channel.Endpoint{}, "无（明确一条都不支持）"},
		{
			"支持两条",
			[]channel.Endpoint{channel.EndpointMessages, channel.EndpointResponses},
			"/v1/messages,/v1/responses",
		},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		if got := endpointDetail(tc.endpoints); got != tc.want {
			t.Errorf("%s：端点那一栏 = %q，期望 %q", tc.name, got, tc.want)
		}
		// 哨兵：三态打出来的词必须互不相同，否则「分得开」这条就没了。
		if seen[tc.want] {
			t.Errorf("三态里有两态打成了同一个词 %q", tc.want)
		}
		seen[tc.want] = true
	}
}

// TestModelGet 钉住 show 打出来的那一份。
func TestModelGet(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelCreate([]string{
			"--model", canonical, "--display", "展示名",
			"--vendor", "example-vendor", "--context-limit", "200000",
			"--tools", "--vision=false", "--knowledge-cutoff", "2026-01-31",
			"--endpoints", string(channel.EndpointChatCompletions),
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})
	id := readModel(t, db, canonical).ID

	out, _ := captureStreams(t, func() {
		if err := runModelGet([]string{"--model", canonical}); err != nil {
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
	// 端点那一栏同理：声明过的端点要看得到，这是命令里唯一能看到它的地方
	// （list 那张表不列它）。
	if want := string(channel.EndpointChatCompletions); !strings.Contains(out, want) {
		t.Errorf("show 的输出里应当有声明过的端点 %s：\n%s", want, out)
	}
	// 没填的那几栏打的是「-」而不是 0：打 0 会让人以为那是一个量出来的上限。
	if !strings.Contains(out, "输入上限") {
		t.Errorf("show 的输出里应当有「输入上限」那一行：\n%s", out)
	}
}

// TestModelUpdateEndpoints 钉住端点那一栏的读写往返，以及它与六个能力
// **同组**这件事。
//
// 命令行这一层只把字符串变成结构，所以「设进去了」只能由「写进去再读出来」
// 证明（与 TestModelCreateThenList 同一条理由）。三件事一起钉：
// 只改端点不动六个能力、只改能力不动端点、空串落成「明确一条都不支持」
// 而不是「没配过」——最后这一条正是这一列的三态里最容易写丢的一档。
func TestModelUpdateEndpoints(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelCreate([]string{
			"--model", canonical, "--display", "展示名",
			"--tools", "--endpoints", string(channel.EndpointChatCompletions),
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})

	got := readModel(t, db, canonical).Capabilities
	if !reflect.DeepEqual(got.Endpoints, []channel.Endpoint{channel.EndpointChatCompletions}) {
		t.Errorf("create 之后端点 = %v，期望 [/v1/chat/completions]", got.Endpoints)
	}

	// 只改端点：六个能力原样留着。
	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{
			"--model", canonical,
			"--endpoints", "/v1/responses,/v1/messages",
		}); err != nil {
			t.Fatalf("update --endpoints 失败: %v", err)
		}
	})
	got = readModel(t, db, canonical).Capabilities
	wantEndpoints := []channel.Endpoint{channel.EndpointResponses, channel.EndpointMessages}
	if !reflect.DeepEqual(got.Endpoints, wantEndpoints) {
		t.Errorf("端点 = %v，期望 %v", got.Endpoints, wantEndpoints)
	}
	if got.Tools == nil || !*got.Tools {
		t.Error("只改端点不该动六个能力，tools 却变了")
	}

	// 只改能力：端点原样留着。
	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{"--model", canonical, "--reasoning"}); err != nil {
			t.Fatalf("update --reasoning 失败: %v", err)
		}
	})
	got = readModel(t, db, canonical).Capabilities
	if !reflect.DeepEqual(got.Endpoints, wantEndpoints) {
		t.Errorf("只改能力时端点应当原样留着，得到 %v，期望 %v", got.Endpoints, wantEndpoints)
	}
	if got.Reasoning == nil || !*got.Reasoning {
		t.Error("--reasoning 应当被写上")
	}

	// 空串：明确一条都不支持。读回来必须是空清单而不是 nil——nil 是
	// 「没配过」，两者在选路里的结论相反（见 catalog.Capabilities.Endpoints）。
	_, _ = captureStreams(t, func() {
		if err := runModelUpdate([]string{"--model", canonical, "--endpoints", ""}); err != nil {
			t.Fatalf("update --endpoints '' 失败: %v", err)
		}
	})
	got = readModel(t, db, canonical).Capabilities
	if got.Endpoints == nil {
		t.Error("--endpoints '' 落库应当是空数组（明确一条都不支持），读回来却是 nil（没配过）")
	}
	if len(got.Endpoints) != 0 {
		t.Errorf("--endpoints '' 之后端点 = %v，期望空清单", got.Endpoints)
	}

	// 那一栏真的读得出来才算数：端点是端点，不是被折成「没配过」。
	out, _ := captureStreams(t, func() {
		if err := runModelGet([]string{"--model", canonical}); err != nil {
			t.Fatalf("show 失败: %v", err)
		}
	})
	if !strings.Contains(out, "端点") || !strings.Contains(out, "无（明确一条都不支持）") {
		t.Errorf("show 的输出里端点那一栏应当是「无（明确一条都不支持）」：\n%s", out)
	}
}

// TestModelGetMissing 钉住「要查的那一行不存在」的退出码。
//
// 它必须与「其他失败」分开：前者是名字写错了或者模型被删了，下一步是去看
// 目录里现在有什么；后者是环境问题。混成一个码，脚本只能去匹配错误文本。
func TestModelGetMissing(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runModelGet([]string{"--model", uniqueName("nonexistent")})
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
		if err := runModelCreate([]string{"--model", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})
	aliasOut, _ := captureStreams(t, func() {
		if err := runAliasCreate([]string{
			"--model", canonical, "--upstream", uniqueName("upstream"),
		}); err != nil {
			t.Fatalf("alias add 失败: %v", err)
		}
	})
	aliasID, err := strconv.ParseInt(strings.TrimSpace(aliasOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是别名 ID：%q（%v）", aliasOut, err)
	}

	delErr := runCaptured(t, func() error {
		return runModelDelete([]string{"--model", canonical})
	})
	if delErr == nil {
		t.Fatal("还被别名引用着的规范名不该删得掉")
	}
	if !errors.Is(delErr, catalog.ErrModelInUse) {
		t.Errorf("期望 catalog.ErrModelInUse，得到 %v", delErr)
	}
	// 三类引用各断一次：夹具里是 1 / 0 / 0（只建了一条出站别名），所以第一条
	// 用非零值钉住「数字与标签的配对」，后两条各钉住标签本身在不在——旧文案
	// 「N 条别名」若只改了措辞、把入站或计价那一段写丢，后两条会红。
	// 入站与计价在这里同为 0，彼此换位看不出来；它们的非零计数由
	// internal/catalog 的用例钉住（TestDeleteModelRefusesWhenInboundAliased
	// 与 TestDeleteModelRefusesWhenPriced）。
	// 消息的形状是「N 条出站别名、M 条入站别名与 K 条计价规则」。
	for _, want := range []string{"1 条出站别名", "0 条入站别名", "0 条计价规则"} {
		if !strings.Contains(delErr.Error(), want) {
			t.Errorf("错误里应当报出 %q：%v", want, delErr)
		}
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
		if deleteErr := runModelDelete([]string{"--model", canonical}); deleteErr != nil {
			t.Fatalf("解除引用之后应当删得掉: %v", deleteErr)
		}
	})
	if _, readErr := catalog.New(db).Model(context.Background(), canonical); !errors.Is(readErr, catalog.ErrModelNotFound) {
		t.Errorf("删完之后读回来应当是 ErrModelNotFound，得到 %v", readErr)
	}

	// 再删一次：那一行已经不在了。
	err = runCaptured(t, func() error {
		return runModelDelete([]string{"--model", canonical})
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

// TestModelCreateRejectsDuplicateCanonical 钉住重复的规范名。
//
// 它是选路与计费共用的键，重复的键会让「这次请求按哪个模型算」没有答案，
// 所以必须撞在唯一键上，而不是静默地多出一行。
func TestModelCreateRejectsDuplicateCanonical(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	add := func() error {
		return runCaptured(t, func() error {
			return runModelCreate([]string{"--model", canonical, "--display", "展示名"})
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

// ── 机器可读输出（--format json）─────────────────────────────────────────

// TestModelListDocIsMachineReadable 钉住 model list 的机器可读形状。
func TestModelListDocIsMachineReadable(t *testing.T) {
	models := []catalog.Model{
		{
			ID: 21, CanonicalName: "m1", DisplayName: "模型一",
			Vendor: "example", Family: "fam",
			ContextLimit:     200000,
			Capabilities:     catalog.Capabilities{Tools: ptrTo(true), Vision: ptrTo(true)},
			ModalitiesInput:  []string{"text", "image"},
			ModalitiesOutput: []string{"text"},
		},
		{
			// 三个上限都没设（Go 侧是 0，库里是 NULL）、厂商与家族是空串、
			// 模态是空数组的一条：表格那一档把它们打成 `-`，而机器读的
			// 那一档必须打成 null 与 `[]`——「没设」与「设成了 0」在 JSON
			// 里要分得开（0 不是任何模型的合法上限）。
			ID: 22, CanonicalName: "m2", DisplayName: "模型二",
		},
	}

	got := jsonRoundTrip(t, modelListDoc(models))
	assertJSONField(t, got, "status", jsonOK)
	assertJSONField(t, jsonObjectField(t, got, "summary"), "total", float64(2))

	list := jsonEntries(t, got, "models")
	if len(list) != 2 {
		t.Fatalf("模型数 = %d，期望 2", len(list))
	}
	first := list[0].(map[string]any)
	assertJSONField(t, first, "id", float64(21))
	assertJSONField(t, first, "canonical_name", "m1")
	assertJSONField(t, first, "display_name", "模型一")
	assertJSONField(t, first, "vendor", "example")
	assertJSONField(t, first, "family", "fam")
	assertJSONField(t, first, "context_limit", float64(200000))
	assertJSONField(t, first, "input_limit", nil)
	assertJSONField(t, first, "output_limit", nil)
	assertJSONField(t, first, "modalities_input", []any{"text", "image"})
	assertJSONField(t, first, "modalities_output", []any{"text"})

	caps := jsonObjectField(t, first, "capabilities")
	assertJSONField(t, caps, "tools", true)
	assertJSONField(t, caps, "vision", true)
	// 没声明过的能力是 null，不是 false：库里那一列是 NULL（「从没配过」），
	// 与「明确配成不支持」是两回事（见迁移 00025）。JSON 里也得分得开，
	// 否则下游会把「不知道」当成「做不了」。
	assertJSONField(t, caps, "reasoning", nil)
	assertJSONField(t, caps, "audio", nil)
	assertJSONField(t, caps, "cache", nil)
	assertJSONField(t, caps, "batch", nil)

	// 清单里那条不带 show 的那几栏：采集源的元信息是给单条详情用的。
	if _, ok := first["knowledge_cutoff"]; ok {
		t.Error("清单不该带 knowledge_cutoff：那是 model show 多出来的几栏之一")
	}

	second := list[1].(map[string]any)
	assertJSONField(t, second, "vendor", nil)
	assertJSONField(t, second, "family", nil)
	assertJSONField(t, second, "context_limit", nil)
	assertJSONField(t, second, "input_limit", nil)
	assertJSONField(t, second, "output_limit", nil)
	assertJSONField(t, second, "modalities_input", []any{})
	assertJSONField(t, second, "modalities_output", []any{})

	t.Run("空清单是空数组，不是 null", func(t *testing.T) {
		empty := jsonRoundTrip(t, modelListDoc(nil))
		assertJSONField(t, jsonObjectField(t, empty, "summary"), "total", float64(0))
		if len(jsonEntries(t, empty, "models")) != 0 {
			t.Error("空清单应当是空数组")
		}
	})
}

// TestModelGetDocIsMachineReadable 钉住 model show 的机器可读形状。
func TestModelGetDocIsMachineReadable(t *testing.T) {
	cutoff := time.Date(2025, 4, 30, 0, 0, 0, 0, time.UTC)
	released := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)

	t.Run("采集源那几栏都在", func(t *testing.T) {
		m := catalog.Model{
			ID: 21, CanonicalName: "m1", DisplayName: "模型一",
			CanonicalModelID: "example/m1",
			KnowledgeCutoff:  &cutoff,
			ReleaseDate:      &released,
			UpdatedAt:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}

		got := jsonRoundTrip(t, modelGetDoc(m))
		assertJSONField(t, got, "status", jsonOK)
		model := jsonObjectField(t, got, "model")
		assertJSONField(t, model, "canonical_name", "m1")
		// 表上那两列是 date（无时区），所以只打日子；把无时区的日期按某个
		// 时区渲染成时刻，会让同一个值在不同机器上打出来不一样。
		assertJSONField(t, model, "knowledge_cutoff", "2025-04-30")
		assertJSONField(t, model, "release_date", "2026-02-03")
		assertJSONField(t, model, "canonical_model_id", "example/m1")
		// 时刻那一栏按 UTC 打 RFC3339，不在本地时区上漂。
		assertJSONField(t, model, "updated_at", "2026-01-02T03:04:05Z")
	})

	t.Run("没采集过的几栏是 null", func(t *testing.T) {
		m := catalog.Model{
			ID: 22, CanonicalName: "m2", DisplayName: "模型二",
			UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}

		model := jsonObjectField(t, jsonRoundTrip(t, modelGetDoc(m)), "model")
		assertJSONField(t, model, "knowledge_cutoff", nil)
		assertJSONField(t, model, "release_date", nil)
		assertJSONField(t, model, "canonical_model_id", nil)
	})

	t.Run("端点那一栏的三态打得开", func(t *testing.T) {
		// 这一列可空，而 NULL（没配过）与 `[]`（明确一条都不支持）在选路里
		// 结论相反，所以 JSON 里也不能都折成空数组——与能力那六个三态同一条
		// 理由（见 nullableEndpoints）。
		cases := []struct {
			name      string
			endpoints []channel.Endpoint
			want      any
		}{
			{"没配过", nil, nil},
			{"明确一条都不支持", []channel.Endpoint{}, []any{}},
			{
				"支持两条",
				[]channel.Endpoint{channel.EndpointMessages, channel.EndpointResponses},
				[]any{"/v1/messages", "/v1/responses"},
			},
		}
		for _, tc := range cases {
			m := catalog.Model{
				ID: 23, CanonicalName: "m3", DisplayName: "模型三",
				Capabilities: catalog.Capabilities{Endpoints: tc.endpoints},
				UpdatedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			}
			model := jsonObjectField(t, jsonRoundTrip(t, modelGetDoc(m)), "model")
			assertJSONField(t, model, "endpoints", tc.want)
		}
	})
}

// TestModelWriteCommandsRejectJSONBeforeDoingAnything 钉住写类命令的守卫位置。
//
// 参数本身就不成立（缺 --model 一类），而要看到的是**不支持 json** 那条错
// ——它证明守卫在参数解析与连库之前。
func TestModelWriteCommandsRejectJSONBeforeDoingAnything(t *testing.T) {
	withFormat(t, formatJSON)

	cases := []struct {
		command string
		run     func() error
	}{
		{"model create", func() error { return runModelCreate(nil) }},
		{"model update", func() error { return runModelUpdate(nil) }},
		{"model delete", func() error { return runModelDelete(nil) }},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s 在 json 模式下应当被拒", tc.command)
			}
			if !strings.Contains(err.Error(), tc.command+" 还不支持 --format json") {
				t.Fatalf("错误信息应当指出 %s 不支持 json，得到 %q", tc.command, err.Error())
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望参数错的 %d", got, exitUsage)
			}
		})
	}
}

// TestModelListJSONStdoutIsPure 拿真命令跑一遍 json 那一档。
//
// 它比文档组装那一条多证明一件事：**库里读回来的值**也照这套口径落进 JSON
// ——没给过的 limit 在库里是 NULL，读出来必须还是 null，而不是表格里的 `-`。
func TestModelListJSONStdoutIsPure(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelCreate([]string{"--model", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})

	withFormat(t, formatJSON)
	stdout, _ := captureStreams(t, func() {
		if err := runModelList(nil); err != nil {
			t.Fatalf("model list 失败: %v", err)
		}
	})

	got := assertJSONStdout(t, stdout)
	list := jsonEntries(t, got, "models")

	var found map[string]any
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if ok && entry["canonical_name"] == canonical {
			found = entry
		}
	}
	if found == nil {
		t.Fatalf("清单里没有刚加进去的 %s：\n%s", canonical, stdout)
	}
	assertJSONField(t, found, "context_limit", nil)
	assertJSONField(t, found, "input_limit", nil)
	assertJSONField(t, found, "output_limit", nil)
	assertJSONField(t, found, "vendor", nil)
	assertJSONField(t, found, "modalities_input", []any{})
	assertJSONField(t, found, "display_name", "展示名")
}
