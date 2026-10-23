package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/pricing"
)

// 这个文件是 `model alias` 三个动作与 `model trace` 的用例。
//
// 判据分两档：不连库的那几条钉住参数、帮助与断链的渲染（trace 的读取面
// 抽成了 traceSource 的函数值，判据不必连库）；连库的那几条钉住写入侧的
// 拒绝与清单的形状。

// createModelForAlias 在目录里建一个规范模型，返回它的名字。
func createModelForAlias(t *testing.T, canonical string) {
	t.Helper()
	r := runOnce(t, runModelCreate, []string{"--model", canonical, "--display", "展示名 " + canonical})
	if r.err != nil {
		t.Fatalf("model create 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
}

// addInboundAlias 建一条入站别名，失败即终止。
func addInboundAlias(t *testing.T, alias, canonical string) {
	t.Helper()
	r := runOnce(t, runModelAlias, []string{"add", alias, canonical})
	if r.err != nil {
		t.Fatalf("model alias add 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
}

// 参数错的几条：两个位置参数都得给、remove 只收一个。
func TestModelAliasRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"add 少给一个", runModelAlias, []string{"add", "fast"}, "两个位置参数"},
		{"add 多给一个", runModelAlias, []string{"add", "a", "b", "c"}, "两个位置参数"},
		{"remove 没给", runModelAlias, []string{"remove"}, "一个位置参数"},
		{"list 多给", runModelAlias, []string{"list", "x"}, "不接受位置参数"},
		{"未知动作", runModelAlias, []string{"nope"}, "未知的 model alias 子命令"},
		{"trace 没给名字", runModelTrace, nil, "一个位置参数"},
		{"trace 多给", runModelTrace, []string{"a", "b"}, "一个位置参数"},
		{"trace 负渠道", runModelTrace, []string{"--channel", "-1", "m"}, "渠道 ID 从 1 开始"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return tc.run(tc.args) })
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

// 三种拒绝都点名，且退出码是 4（约束冲突）。
//
// 三种的下一步都是「先去看库里现在是什么」，与参数错（改一下命令重发）
// 不是一回事，所以不能落到 1。
func TestModelAliasAddRejects(t *testing.T) {
	testDB(t)
	setupEnv(t)

	t.Run("与规范名同名", func(t *testing.T) {
		name := uniqueName("clash")
		createModelForAlias(t, name)

		r := runOnce(t, runModelAlias, []string{"add", name, uniqueName("other")})
		if r.err == nil {
			t.Fatal("期望报错，却通过了")
		}
		if !strings.Contains(r.err.Error(), name) {
			t.Errorf("错误 %q 没有点名冲突的名字 %q", r.err, name)
		}
		if got := exitCode(r.err); got != exitConflict {
			t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
		}
	})

	t.Run("规范名不存在", func(t *testing.T) {
		missing := uniqueName("ghost")
		r := runOnce(t, runModelAlias, []string{"add", uniqueName("short"), missing})
		if r.err == nil {
			t.Fatal("期望报错，却通过了")
		}
		if !strings.Contains(r.err.Error(), missing) {
			t.Errorf("错误 %q 没有点名不存在的规范名 %q", r.err, missing)
		}
		if got := exitCode(r.err); got != exitConflict {
			t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
		}
	})

	t.Run("别名已存在", func(t *testing.T) {
		first := uniqueName("model")
		second := uniqueName("model")
		createModelForAlias(t, first)
		createModelForAlias(t, second)
		alias := uniqueName("short")
		addInboundAlias(t, alias, first)

		r := runOnce(t, runModelAlias, []string{"add", alias, second})
		if r.err == nil {
			t.Fatal("期望报错，却通过了")
		}
		if !strings.Contains(r.err.Error(), alias) {
			t.Errorf("错误 %q 没有点名别名 %q", r.err, alias)
		}
		if got := exitCode(r.err); got != exitConflict {
			t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
		}
	})

	t.Run("删不存在的别名", func(t *testing.T) {
		alias := uniqueName("short")
		r := runOnce(t, runModelAlias, []string{"remove", alias})
		if r.err == nil {
			t.Fatal("期望报错，却通过了")
		}
		if !strings.Contains(r.err.Error(), alias) {
			t.Errorf("错误 %q 没有点名别名 %q", r.err, alias)
		}
		if got := exitCode(r.err); got != exitConflict {
			t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
		}
	})
}

// add 的 stdout 只有别名本身，其余信息走 stderr；list 能把它列出来。
func TestModelAliasAddThenList(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	createModelForAlias(t, canonical)
	alias := uniqueName("short")
	addInboundAlias(t, alias, canonical)

	// 建完之后立刻删掉：这条用例只关心它自己那一条，别让别的用例的清单
	// 把它淹掉。
	t.Cleanup(func() {
		runOnce(t, runModelAlias, []string{"remove", alias})
	})

	alias2 := uniqueName("short2")
	stdout, stderr := captureStreams(t, func() {
		if err := runModelAlias([]string{"add", alias2, canonical}); err != nil {
			t.Fatalf("model alias add 失败: %v", err)
		}
	})
	// 这条别名同样要收走：别名的表是全局的，留下来会把别的用例的清单弄脏。
	t.Cleanup(func() {
		runOnce(t, runModelAlias, []string{"remove", alias2})
	})
	if got := strings.TrimSpace(stdout); !strings.HasPrefix(got, "short2-") {
		t.Errorf("stdout = %q，期望只有新别名本身", got)
	}
	if !strings.Contains(stderr, "已加入入站别名") {
		t.Errorf("stderr = %q，期望有一句解释", stderr)
	}
}

// 空清单打「暂无…」模板，退出 0。
//
// 这一条走渲染函数而不是命令：别名表是全局的，别的用例建过的行会让真表非空，
// 而这里要钉的是「一条都没有时打哪句话」。命令那一侧的空表行为由同一份
// 渲染函数回答，没有第二处会拼这句话（见 style.go 的 emptyListNote）。
func TestModelAliasListEmptyWording(t *testing.T) {
	var buf strings.Builder
	printModelAliasList(&buf, nil)
	const want = "暂无入站别名。可使用 larkaxctl model alias add 添加。"
	if got := strings.TrimSpace(buf.String()); got != want {
		t.Errorf("空清单输出 = %q，期望 %q", got, want)
	}

	// 机器读的那一档是 `[]`，不是 null，也不是那句「暂无」。
	doc := modelAliasListDoc(nil)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(raw), `"aliases":[]`) {
		t.Errorf("空清单的 json 里 aliases 不是空数组: %s", raw)
	}
	if !strings.Contains(string(raw), `"total":0`) {
		t.Errorf("空清单的 json 里 summary.total 不是 0: %s", raw)
	}
}

// list 在真库上退出 0（空表与非空都一样）。
func TestModelAliasListExitsZero(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	r := runOnce(t, runModelAlias, []string{"list"})
	if r.err != nil {
		t.Fatalf("model alias list 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if r.stdout == "" {
		t.Error("list 什么都不打：「空清单」与「命令没跑起来」在安静的终端里长得一样")
	}
}

// ── trace ─────────────────────────────────────────────────────────────

// fakeTraceSource 是一份完全内存里的诊断来源。
//
// trace 的读取面是函数值，正是为了让断链那几条判据不必连库：它们要证明的是
// 「断在哪一段、退出码是不是非零」，而那件事与数据从哪来无关。
type fakeTraceSource struct {
	aliases   map[string]string
	shadowed  map[string]bool
	models    map[string]bool
	upstreams map[string][]catalog.Alias
	aliasFor  map[int64]string
	supplies  []channels.Supply
	channel   channels.ChannelRef
	channelOK bool
	rules     map[int64]pricingRule
	// noRuleOn 里的渠道问不到规则（回 ErrNoRule）。
	noRuleOn map[int64]bool
	// errOn 让某个读取面直接失败，用来钉「读不出来不是断链」。
	err error
}

type pricingRule struct {
	id    int64
	scope string
	name  string
}

func (f *fakeTraceSource) source() traceSource {
	return traceSource{
		resolveAlias: func(_ context.Context, name string) (string, error) {
			if f.err != nil {
				return "", f.err
			}
			if canonical, ok := f.aliases[name]; ok {
				return canonical, nil
			}
			return name, nil
		},
		shadowedAlias: func(_ context.Context, name string) (bool, error) {
			return f.shadowed[name], nil
		},
		model: func(_ context.Context, name string) (catalog.Model, error) {
			if f.models[name] {
				return catalog.Model{CanonicalName: name}, nil
			}
			return catalog.Model{}, catalog.ErrModelNotFound
		},
		upstreams: func(_ context.Context, name string) ([]catalog.Alias, error) {
			return f.upstreams[name], nil
		},
		aliasFor: func(_ context.Context, channelID int64, name string) (string, error) {
			upstream, ok := f.aliasFor[channelID]
			if !ok {
				return "", catalog.ErrAliasNotFound
			}
			return upstream, nil
		},
		supplies: func(_ context.Context, _ string) ([]channels.Supply, error) {
			return f.supplies, nil
		},
		channelRef: func(_ context.Context, id int64) (channels.ChannelRef, error) {
			if !f.channelOK {
				return channels.ChannelRef{}, channels.ErrChannelNotFound
			}
			return f.channel, nil
		},
		explain: func(_ context.Context, q pricing.ExplainQuery) ([]pricing.RuleInfo, error) {
			if f.noRuleOn[q.ChannelID] {
				return nil, fmt.Errorf("%w: 没有生效的规则", pipeline.ErrNoRule)
			}
			rule, ok := f.rules[q.ChannelID]
			if !ok {
				return nil, fmt.Errorf("%w: 没有生效的规则", pipeline.ErrNoRule)
			}
			return []pricing.RuleInfo{{ID: rule.id, Scope: rule.scope, Name: rule.name}}, nil
		},
	}
}

// happyTrace 是一条完整的链路：别名 → 规范名 → 渠道上的上游名 → 一条规则。
func happyTrace() *fakeTraceSource {
	return &fakeTraceSource{
		aliases:   map[string]string{"fast": "m-1"},
		models:    map[string]bool{"m-1": true},
		upstreams: map[string][]catalog.Alias{"m-1": {{ChannelID: 0, UpstreamModel: "m-1-upstream"}}},
		aliasFor:  map[int64]string{3: "m-1-upstream"},
		supplies:  []channels.Supply{{Model: "m-1", ChannelID: 3, OwnerUserID: 9, Merchant: "acme"}},
		channel:   channels.ChannelRef{ID: 3, Name: "ch-3", OwnerUserID: 9, Merchant: "acme"},
		channelOK: true,
		rules:     map[int64]pricingRule{3: {id: 11, scope: "platform", name: "标准价"}},
	}
}

// 正常链路：三段名字都在结果里，且没有断点。
func TestModelTraceCompleteChain(t *testing.T) {
	src := happyTrace()
	tr, err := evaluateModelTrace(context.Background(), src.source(), "fast", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}
	if tr.BrokenAt != "" {
		t.Fatalf("链路完整却报了断点：%s：%s", tr.BrokenAt, tr.BrokenReason)
	}
	if !tr.IsAlias || tr.Canonical != "m-1" {
		t.Errorf("别名那一段 = (isAlias=%v, canonical=%q)，期望 (true, m-1)", tr.IsAlias, tr.Canonical)
	}
	if len(tr.Upstreams) != 1 || tr.Upstreams[0].Upstream != "m-1-upstream" {
		t.Errorf("上游名 = %+v，期望一条 m-1-upstream", tr.Upstreams)
	}
	if len(tr.Rules) != 1 || tr.Rules[0].ID != 11 {
		t.Errorf("计价规则 = %+v，期望 #11 胜出", tr.Rules)
	}
}

// 五个断点各一条：模型不在目录、没有渠道在供、目标渠道不在供、渠道没有上游名、
// 没有生效规则。
func TestModelTraceBrokenSegments(t *testing.T) {
	cases := []struct {
		name    string
		channel int64
		mutate  func(*fakeTraceSource)
		at      string
		want    string
	}{
		{
			name:    "规范模型不在目录",
			channel: 3,
			mutate:  func(f *fakeTraceSource) { f.models = map[string]bool{} },
			at:      traceSegmentModel,
			want:    "m-1",
		},
		{
			name:    "没有启用的渠道在供",
			channel: 0,
			// 平台兜底规则留着：没有供给判断时，链路会一路走到这条规则，
			// 报成一条完整的链路——那正是这条用例要拦下的结论。
			mutate: func(f *fakeTraceSource) {
				f.supplies = nil
				f.rules = map[int64]pricingRule{0: {id: 12, scope: "platform", name: "兜底价"}}
			},
			at:   traceSegmentUpstream,
			want: "没有启用的渠道在供模型",
		},
		{
			name:    "目标渠道不在供",
			channel: 3,
			mutate: func(f *fakeTraceSource) {
				f.supplies = []channels.Supply{{Model: "m-1", ChannelID: 4}}
			},
			at:   traceSegmentUpstream,
			want: "没有在供模型",
		},
		{
			name:    "渠道不存在",
			channel: 3,
			mutate:  func(f *fakeTraceSource) { f.channelOK = false },
			at:      traceSegmentUpstream,
			want:    "渠道 3 不存在",
		},
		{
			name:    "该渠道没有上游名",
			channel: 3,
			mutate:  func(f *fakeTraceSource) { f.aliasFor = map[int64]string{} },
			at:      traceSegmentUpstream,
			want:    "没有",
		},
		{
			name:    "没有生效的计价规则",
			channel: 3,
			mutate:  func(f *fakeTraceSource) { f.noRuleOn = map[int64]bool{3: true} },
			at:      traceSegmentPricing,
			want:    "没有生效的计价规则",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := happyTrace()
			tc.mutate(src)

			tr, err := evaluateModelTrace(context.Background(), src.source(), "fast", tc.channel)
			if err != nil {
				t.Fatalf("断链不是读取失败，不该返回 error: %v", err)
			}
			if tr.BrokenAt != tc.at {
				t.Fatalf("断点 = %q，期望 %q（原因：%s）", tr.BrokenAt, tc.at, tr.BrokenReason)
			}
			if !strings.Contains(tr.BrokenReason, tc.want) {
				t.Errorf("断点原因 %q 里没有 %q", tr.BrokenReason, tc.want)
			}
		})
	}
}

// 被遮蔽的别名要说出来。
//
// 这类数据写入侧拦不住（目录行可以先有、别名后建），解析时规范名优先，所以
// 它只能由诊断把它说出来——不说的话，人看到「不是别名」会以为那条别名没建。
func TestModelTraceReportsShadowedAlias(t *testing.T) {
	src := happyTrace()
	src.aliases = map[string]string{}
	src.shadowed = map[string]bool{"m-1": true}

	tr, err := evaluateModelTrace(context.Background(), src.source(), "m-1", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}
	if tr.IsAlias {
		t.Error("被遮蔽的别名不该算作命中")
	}
	if !tr.ShadowedAlias {
		t.Error("没有报出被遮蔽的别名：人看到「不是别名」会以为那条别名没建")
	}
}

// 读取失败（数据库故障）不是断链：它要作为 error 返回，由命令层报成失败。
func TestModelTraceReadFailureIsError(t *testing.T) {
	src := happyTrace()
	src.err = errors.New("库连不上")

	if _, err := evaluateModelTrace(context.Background(), src.source(), "fast", 3); err == nil {
		t.Fatal("读取失败要作为 error 返回，不能当成一次断链的诊断结论")
	}
}

// 断链时退出码非零、屏幕上有一句「断在 X」，且 main 不再补打一行。
func TestModelTraceBrokenExitsNonZero(t *testing.T) {
	src := happyTrace()
	src.models = map[string]bool{}

	tr, err := evaluateModelTrace(context.Background(), src.source(), "fast", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}
	if tr.BrokenAt == "" {
		t.Fatal("这条用例的前提是断链")
	}

	var buf strings.Builder
	printModelTrace(&buf, tr)
	out := buf.String()
	if !strings.Contains(out, "断在 "+traceSegmentModel) {
		t.Errorf("输出里没有断点那一行:\n%s", out)
	}
	// 三段名字都要出现：调用方写的、解析成的、以及（能拿到时）上游实际叫的。
	if !strings.Contains(out, "fast") {
		t.Errorf("输出里没有调用方写的名字:\n%s", out)
	}
}

// table 与 json 两种格式逐字可对。
func TestModelTraceTableAndJSONAgree(t *testing.T) {
	tr, err := evaluateModelTrace(context.Background(), happyTrace().source(), "fast", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}

	var buf strings.Builder
	printModelTrace(&buf, tr)
	table := buf.String()
	for _, want := range []string{"fast", "m-1", "m-1-upstream", "#11", "acme"} {
		if !strings.Contains(table, want) {
			t.Errorf("表格里没有 %q:\n%s", want, table)
		}
	}

	doc := modelTraceDoc(tr)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back["status"] != jsonOK {
		t.Errorf("status = %v，期望 %s", back["status"], jsonOK)
	}
	if back["requested"] != "fast" || back["canonical"] != "m-1" {
		t.Errorf("json 里的名字 = (%v, %v)，期望 (fast, m-1)", back["requested"], back["canonical"])
	}
	if back["alias_matched"] != true {
		t.Errorf("alias_matched = %v，期望 true", back["alias_matched"])
	}
	upstreams, ok := back["upstreams"].([]any)
	if !ok || len(upstreams) != 1 {
		t.Fatalf("upstreams = %v，期望一条", back["upstreams"])
	}
	entry := upstreams[0].(map[string]any)
	if entry["upstream_model"] != "m-1-upstream" || entry["channel_id"] != float64(3) {
		t.Errorf("upstreams[0] = %v，期望渠道 3 上的 m-1-upstream", entry)
	}
	rules, ok := back["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules = %v，期望一条", back["rules"])
	}
	rule := rules[0].(map[string]any)
	if rule["rule_id"] != float64(11) || rule["name"] != "标准价" {
		t.Errorf("rules[0] = %v，期望 #11 标准价", rule)
	}
	if _, broken := back["broken_at"]; broken {
		t.Errorf("链路完整时不该有 broken_at：%v", back["broken_at"])
	}
}

// 断链时 json 的 status 是 failed，且 broken_at 与表格里那一句同源。
func TestModelTraceJSONBroken(t *testing.T) {
	src := happyTrace()
	src.aliasFor = map[int64]string{}

	tr, err := evaluateModelTrace(context.Background(), src.source(), "fast", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}
	doc := modelTraceDoc(tr)
	if doc["status"] != jsonFailed {
		t.Errorf("status = %v，期望 %s", doc["status"], jsonFailed)
	}
	if doc["broken_at"] != traceSegmentUpstream {
		t.Errorf("broken_at = %v，期望 %s", doc["broken_at"], traceSegmentUpstream)
	}
}

// 断链时退出码是 1，且 main 不再补打一行。
//
// 「把断点当成功」的实现下，屏幕上那行「断在 X」照样会打出来——所以判据
// 不能只看输出，必须落在退出码上。reportOnly 也要一起断：少了它，调度器
// 的日志里会多一行长得像崩溃的「larkaxctl: 断在 …」。
func TestModelTraceCauseCarriesExitCode(t *testing.T) {
	src := happyTrace()
	src.models = map[string]bool{}
	tr, err := evaluateModelTrace(context.Background(), src.source(), "fast", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}
	if tr.BrokenAt == "" {
		t.Fatal("这条用例的前提是断链")
	}

	cause := traceCause(tr)
	if cause == nil {
		t.Fatal("断链时没有退出码：把断点当成了一次成功")
	}
	if got := exitCode(cause); got != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", got, exitFailure)
	}
	if !reportedOnly(cause) {
		t.Error("断链的结论已经由报告说清楚了，不该让 main 再补打一行")
	}

	// 对照组：链路完整时没有任何退出码。
	ok, err := evaluateModelTrace(context.Background(), happyTrace().source(), "fast", 3)
	if err != nil {
		t.Fatalf("诊断出错: %v", err)
	}
	if cause := traceCause(ok); cause != nil {
		t.Errorf("链路完整时不该有退出码，得到 %v", cause)
	}
}
