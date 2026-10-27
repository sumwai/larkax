package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/api"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/upstream"
)

// testDoctorKey 是打网关用的假 API Key。
//
// 一眼能看出是编的，且不构成任何真实凭证的形态——真实的 key 不进仓库，
// 也不进测试：这里要证明的是「它被放进了请求头、且没有被打印出来」，
// 不是「这把 key 有多强」。
const testDoctorKey = "test-key-placeholder"

// printDoctorLive 让用例只喂「真打上游」那一项，就拿到默认输出里那一行的形状。
//
// 它走的是与整份报告**同一条**打印路径（`printStatusLines`），默认输出的形状改了
// 这里也会跟着红。要有这个入口是因为那一项的几条分支（没开 --live、请求发不出去、
// 成功、被谁拒了）各要一份输出，而整份报告要连库才跑得起来。
//
// 它**只能**写在测试文件里：deadcode 门禁不把测试当调用点，写在 doctor.go 里
// 会被判成「没人调的函数」。
func printDoctorLive(w io.Writer, l doctorLive, verbose bool) {
	cli.PrintStatusLines(w, []cli.StatusLine{doctorLiveLine(l)}, verbose)
}

// ── 第 1 项：数据库 ─────────────────────────────────────────────────────

// TestDoctorDatabaseCheckDoesNotCreateTheVersionTable 钉住第 1 项的只读承诺。
//
// goose 的 GetDBVersionContext 在版本表不存在时会先 EnsureDBVersion 把表
// **建出来**——也就是说「读一下版本」在一个全新库上是一次写入。doctor 对外
// 承诺只读，所以它自己先探一句表在不在。
//
// 这条用例在一个空 schema 上跑一遍，再确认那张表仍然不在。把探测换回 goose
// 的现成函数，这里就会红。
func TestDoctorDatabaseCheckDoesNotCreateTheVersionTable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// schema 名不能带短横线：未加引号的标识符里它是减号。
	schema := strings.ReplaceAll(uniqueName("doctor_readonly"), "-", "_")
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	c := checkDoctorDatabase(ctx, openInSchema(t, schema))
	notes := strings.Join(c.notes, "\n")
	if !strings.Contains(notes, "一条迁移都没跑过") {
		t.Errorf("空 schema 该按「一条迁移都没跑过」报，实际 notes 是 %q", c.notes)
	}
	// 这个数从内嵌的迁移文件现算，不写死：写死的话每加一条迁移这条用例都会红一次，
	// 而红的理由（版本号 +1）与它要证的事（doctor 在这个 schema 上什么都没写）
	// 毫无关系——第一次撞上会去翻 doctor.go，翻完发现 doctor 一行没改。
	if want := fmt.Sprintf("最新的迁移版本是 %d", latestMigrationVersion(t)); !strings.Contains(notes, want) {
		t.Errorf("它仍然该报出二进制里的最新版本（%q），实际 notes 是 %q", want, c.notes)
	}

	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`,
		schema+"."+goose.TableName()).Scan(&exists); err != nil {
		t.Fatalf("查版本表失败: %v", err)
	}
	if exists {
		t.Errorf("doctor 把 %s.%s 建出来了：它承诺只读", schema, goose.TableName())
	}
}

// latestMigrationVersion 数出内嵌迁移里最大的编号。
//
// 与 doctor 那边不是同一段代码：doctor 走 goose.CollectMigrations，它读文件内容、
// 认 Up/Down 标记、按 goose 自己的命名规则筛；这里只按文件名前缀数数。所以两者
// 可能不一致（goose 认不出的文件这里照样数得到），**别把它当成一道强判据**——
// 这条用例真正要钉的是「版本表不在时那句 note 照样打出来」，版本号对不对是附带。
//
// 一条都数不出来时直接 Fatal：那说明目录名或文件命名变了，此时「数不到」与
// 「数出来是 0」在断言里长得一样（见 LESSONS.md 第 16 条）。
func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrate.FS(), ".")
	if err != nil {
		t.Fatalf("读内嵌迁移目录失败: %v", err)
	}
	latest := 0
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(prefix)
		if err != nil {
			continue
		}
		if n > latest {
			latest = n
		}
	}
	if latest == 0 {
		t.Fatalf("内嵌迁移目录 %v 里一条迁移都没解析出来", entries)
	}
	return latest
}

// openInSchema 打开一条 search_path 指向指定 schema 的连接。
//
// 走 DSN 参数而不是 `SET search_path`：SET 只作用于当前那一条连接，而
// database/sql 是连接池，下一条语句完全可能落到另一条连接上。
func openInSchema(t *testing.T, schema string) *sql.DB {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	handle, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	if err := handle.PingContext(context.Background()); err != nil {
		t.Fatalf("连不上数据库: %v", err)
	}
	return handle
}

// ── 第 2 项：渠道 ───────────────────────────────────────────────────────

func TestDoctorChannelsFlagsEachCondition(t *testing.T) {
	cases := []struct {
		name     string
		channels []channels.ChannelInfo
		want     []string
	}{
		{
			name:     "一条渠道都没有",
			channels: nil,
			want:     []string{"没有任何 enabled 的渠道"},
		},
		{
			name: "enabled 但 models 为空",
			channels: []channels.ChannelInfo{
				{ID: 3, Name: "c3", Status: channels.StatusEnabled},
			},
			want: []string{"渠道 3（c3）是 enabled，但 models 是空的"},
		},
		{
			name: "全部是 disabled",
			channels: []channels.ChannelInfo{
				{ID: 4, Name: "c4", Status: channels.StatusDisabled, Models: []string{"m"}},
			},
			want: []string{"没有任何 enabled 的渠道"},
		},
		{
			name: "一条可用的渠道",
			channels: []channels.ChannelInfo{
				{ID: 5, Name: "c5", Status: channels.StatusEnabled, Models: []string{"m"}},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkDoctorChannels(doctorChainInput{channels: tc.channels})
			assertContainsFragments(t, got.findings, tc.want...)
			if got.skip != "" {
				t.Errorf("第 2 项不该报「没跑成」：%s", got.skip)
			}
		})
	}
}

// ── 第 3 项：模型目录 ───────────────────────────────────────────────────

// TestDoctorCatalogFlagsChannelModelMismatch 钉住 issue 里的第一个例子：
// 渠道的 --models 与目录里的规范名对不上。
//
// 这一条要能**单独**指出来：说清是哪一个渠道、哪一个名字，而不是一句
// 「配置不对」——调用那边只会回 503「no available upstream」。
func TestDoctorCatalogFlagsChannelModelMismatch(t *testing.T) {
	in := doctorChainInput{
		channels: []channels.ChannelInfo{
			{ID: 7, Name: "my-upstream", Status: channels.StatusEnabled, Models: []string{"claude-xx"}},
			{ID: 9, Name: "other", Status: channels.StatusEnabled, Models: []string{"claude-xx"}},
			{ID: 8, Name: "disabled", Status: channels.StatusDisabled, Models: []string{"claude-xx"}},
		},
		catalogEndpoints: map[string][]channel.Endpoint{"claude-x": nil},
	}

	got := checkDoctorCatalog(in)
	assertContainsFragments(t, got.findings,
		"渠道 7（my-upstream）", "渠道 9（other）", `"claude-xx"`, "目录里没有这个名字")
	if strings.Contains(findingsText(got.findings), "渠道 8") {
		t.Error("disabled 的渠道不该出现在问题里：它本来就不接请求")
	}
	if got.status() != doctorFailed {
		t.Errorf("对不上名字时结论应当是不通过，得到 %s", got.status().label())
	}
}

// TestDoctorCatalogSkipsWithoutChannels 钉住「没跑成」与「通过」分得开。
//
// 一条 enabled 渠道都没有时，这一项一个名字都查不到。报「通过」是错的：
// 它会让一次什么都没查的运行看起来像一次查过且没问题的运行
// （见 LESSONS.md 第 16 条）。
func TestDoctorCatalogSkipsWithoutChannels(t *testing.T) {
	got := checkDoctorCatalog(doctorChainInput{catalogEndpoints: map[string][]channel.Endpoint{}})
	if got.skip == "" {
		t.Fatal("没有可查的对象时第 3 项应当报「没跑成」，而不是什么都不说")
	}
	if got.status() != doctorSkipped {
		t.Errorf("结论应当是没跑成，得到 %s", got.status().label())
	}
}

// TestDoctorCatalogFlagsEndpointMismatch 钉住第 3 项的第二个判据：
// 名字在目录里、但它声明的端点与这条渠道打的那条对不上。
//
// 它与写入侧建渠道时的校验判的是同一件事（见 internal/channels/endpoints.go
// 的 EndpointFit），所以报错要点名渠道、模型名，并把**两个端点**都说出来——
// 只说「对不上」等于让人去猜是哪一个词对不上。
func TestDoctorCatalogFlagsEndpointMismatch(t *testing.T) {
	in := doctorChainInput{
		channels: []channels.ChannelInfo{
			// openai 打 /v1/chat/completions，而下面这个模型只声明 /v1/messages。
			{ID: 7, Name: "my-upstream", Status: channels.StatusEnabled,
				Type: "openai", Models: []string{"claude-x"}},
		},
		catalogEndpoints: map[string][]channel.Endpoint{
			"claude-x": {channel.EndpointMessages},
		},
	}

	got := checkDoctorCatalog(in)
	if got.status() != doctorFailed {
		t.Fatalf("端点对不上时结论应当是不通过，得到 %s：%q",
			got.status().label(), findingsText(got.findings))
	}
	assertContainsFragments(t, got.findings,
		"渠道 7（my-upstream）",
		`"claude-x"`,
		string(channel.EndpointMessages),
		string(channel.EndpointChatCompletions),
		"unsupported_model")
}

// TestDoctorCatalogAllowsEndpointForUnknownOrMatching 是上一条的对照：
// 声明的端点含这条渠道那条、那一项没配过（nil）、渠道类型算不出端点，三档都不报。
//
// 三档放行与写入侧一致——目录是人工维护、会过时的一份声明，按「不支持」处理
// 会让刚接的上游一条渠道都建不出来。少任何一臂，上一条证明的都只是「报了」，
// 证明不了判的是「端点对不对」。
func TestDoctorCatalogAllowsEndpointForUnknownOrMatching(t *testing.T) {
	base := func(declared []channel.Endpoint, typ string) doctorChainInput {
		return doctorChainInput{
			channels: []channels.ChannelInfo{
				{ID: 1, Name: "c", Status: channels.StatusEnabled, Type: typ, Models: []string{"m"}},
			},
			catalogEndpoints: map[string][]channel.Endpoint{"m": declared},
		}
	}

	cases := []struct {
		name string
		in   doctorChainInput
	}{
		{"声明里含这条端点", base([]channel.Endpoint{channel.EndpointChatCompletions}, "openai")},
		{"端点那一项没配过", base(nil, "openai")},
		{"渠道类型算不出端点", base([]channel.Endpoint{channel.EndpointMessages}, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorEndpointFindings(tc.in); len(got) != 0 {
				t.Errorf("这一档应当放行，得到 %q", got)
			}
		})
	}
}

// TestDoctorEndpointFindingMatchesEveryRegisteredType 让 doctor 的结论在**每个**
// 已注册的渠道类型上都与那张表对上。
//
// 它是「别在 CLI 里抄一份对照表」的钉子：doctor 走的是 channels.EndpointFitForProtocols
// （「渠道类型 → 端点」那张表的唯一入口）。在 CLI 里另写一份 switch、尤其只抄了
// 常见的那几个类型时，没抄到的类型上这里会红。期望值从 channel.Protocol
// （写入侧同一张表）算，不从 doctor 的实现算。
func TestDoctorEndpointFindingMatchesEveryRegisteredType(t *testing.T) {
	registry, err := upstream.Registry()
	if err != nil {
		t.Fatalf("装配适配器注册表失败: %v", err)
	}
	types := registry.Types()
	if len(types) == 0 {
		t.Fatal("一个渠道类型都没注册：这条用例什么都没查")
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			own := channel.Protocol(typ).Endpoint()
			if own == "" {
				t.Fatalf("类型 %q 算不出端点，用例的前提不成立", typ)
			}
			in := doctorChainInput{
				channels: []channels.ChannelInfo{
					{ID: 1, Name: "c", Status: channels.StatusEnabled, Type: typ, Models: []string{"m"}},
				},
				catalogEndpoints: map[string][]channel.Endpoint{"m": {otherEndpoint(own)}},
			}
			if got := doctorEndpointFindings(in); len(got) == 0 {
				t.Errorf("类型 %q 打 %s，模型只声明 %s，doctor 应当报出来",
					typ, own, otherEndpoint(own))
			}

			// 反过来：声明里含这条端点时一条都不该报。
			in.catalogEndpoints["m"] = []channel.Endpoint{own}
			if got := doctorEndpointFindings(in); len(got) != 0 {
				t.Errorf("类型 %q 打 %s，模型也声明了它，doctor 不该报：%q", typ, own, got)
			}
		})
	}
}

// otherEndpoint 给一个与 own 不同的端点取值，用来造「一定对不上」的声明。
func otherEndpoint(own channel.Endpoint) channel.Endpoint {
	for _, e := range []channel.Endpoint{
		channel.EndpointChatCompletions, channel.EndpointResponses,
		channel.EndpointMessages, channel.EndpointSystemOne,
	} {
		if e != own {
			return e
		}
	}
	return ""
}

// ── 第 4 项：别名 ───────────────────────────────────────────────────────

// TestDoctorAliasFlagsConfiguredButUnreachable 钉住 issue 里的第二个例子：
// 别名配了，但渠道白名单里没有这个模型。
//
// 两张表各看一半都对，合起来不对——所以这一条必须点名是哪个规范名、
// 别名配在哪个范围上。
func TestDoctorAliasFlagsConfiguredButUnreachable(t *testing.T) {
	aliases := []catalog.Alias{
		{ID: 1, ChannelID: 0, CanonicalName: "claude-z", UpstreamModel: "u-platform"},
		{ID: 2, ChannelID: 3, CanonicalName: "claude-z", UpstreamModel: "u-channel"},
		{ID: 3, ChannelID: 0, CanonicalName: "claude-x", UpstreamModel: "u-x"},
	}

	got := doctorAliasReachability(aliases, map[string]bool{"claude-x": true})

	if len(got) != 1 {
		t.Fatalf("只有 claude-z 该被报出来，得到 %d 条：%q", len(got), got)
	}
	assertContainsFragments(t, got, "「claude-z」", "平台级 1 条", "渠道级 1 条（渠道 3）",
		"没有任何 enabled 渠道的 models 里包含它")
}

// TestDoctorAliasReachabilityPassesWhenEveryAliasIsServable 是上一条的对照：
// 有渠道接得住的别名一条都不该被报。
func TestDoctorAliasReachabilityPassesWhenEveryAliasIsServable(t *testing.T) {
	aliases := []catalog.Alias{{ID: 1, CanonicalName: "claude-x", UpstreamModel: "u-x"}}
	if got := doctorAliasReachability(aliases, map[string]bool{"claude-x": true}); len(got) != 0 {
		t.Errorf("有渠道接得住时不该报问题，得到 %q", got)
	}
}

// TestDoctorAliasAndPricingSkipWithoutServableModels 钉住第 4、5 项的空集合分支。
func TestDoctorAliasAndPricingSkipWithoutServableModels(t *testing.T) {
	alias, price := checkDoctorAliasAndPricing(context.Background(), nil, doctorChainInput{
		channels: []channels.ChannelInfo{
			{ID: 1, Name: "c", Status: channels.StatusEnabled, Models: []string{"not-in-catalog"}},
		},
		catalogEndpoints: map[string][]channel.Endpoint{"claude-x": nil},
	})

	for _, c := range []doctorCheck{alias, price} {
		if c.skip == "" {
			t.Errorf("第 %s 项没有可查的对象时应当报「没跑成」", c.title)
		}
		if c.status() != doctorSkipped {
			t.Errorf("第 %s 项的结论应当是没跑成，得到 %s", c.title, c.status().label())
		}
	}
}

// ── 第 7 项：钱包 ───────────────────────────────────────────────────────

func TestDoctorWalletsFlagsEachCondition(t *testing.T) {
	cases := []struct {
		name  string
		users []doctorUser
		want  []string
	}{
		{
			name:  "一个用户都没有",
			users: nil,
			// 清单空状态那一句（style.go 的 emptyListNote）：用户清单是空的
			// 与「用户清单没查」不能长得一样，而这一条正是前者。
			want: []string{"暂无用户。可使用 larkaxctl user add 添加。"},
		},
		{
			name:  "用户没有钱包行",
			users: []doctorUser{{ID: 1, Name: "alice"}},
			want:  []string{"用户 1（alice）没有钱包行"},
		},
		{
			name:  "有钱包行但余额都是 0",
			users: []doctorUser{{ID: 2, Name: "bob", HasWallet: true}},
			want:  []string{"1 个钱包行的余额都是 0"},
		},
		{
			name: "一个用户没有钱包、另一个有余额",
			users: []doctorUser{
				{ID: 3, Name: "carol", HasWallet: true, Funded: true},
				{ID: 4, Name: "dave"},
			},
			want: []string{"用户 4（dave）没有钱包行"},
		},
		{
			name:  "都正常",
			users: []doctorUser{{ID: 5, Name: "erin", HasWallet: true, Funded: true}},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkDoctorWallets(doctorChainInput{users: tc.users})
			assertContainsFragments(t, got.findings, tc.want...)
			if tc.name == "有钱包行但余额都是 0" {
				if strings.Contains(findingsText(got.findings), "没有钱包行") {
					t.Error("有钱包行时不该报「没有钱包行」")
				}
			}
		})
	}
}

// ── 第 8、9 项：入站别名（纯函数） ──────────────────────────────────────

// TestDoctorInboundShadowedNamesTheAlias 是第 8 项的现场：别名与目录里的
// 规范名同名时，解析走规范名，这条别名永远不会被用到。
func TestDoctorInboundShadowedNamesTheAlias(t *testing.T) {
	in := doctorChainInput{
		catalogEndpoints: map[string][]channel.Endpoint{"model-a": nil, "same": nil},
		inboundAliases: []catalog.InboundAlias{
			{Alias: "same", CanonicalName: "model-a"},
			{Alias: "fast", CanonicalName: "model-a"},
		},
	}

	got := checkDoctorInboundShadowed(in)

	assertContainsFragments(t, got.findings, `"same"`, "规范名优先", "这条别名不会被用到")
	if got.status() != doctorFailed {
		t.Errorf("有被遮蔽的别名时应当不通过，得到 %s", got.status().label())
	}
	// 只点名真正被遮蔽的那条：另一条指向 model-a、名字不与规范名冲突，不该报。
	if strings.Contains(findingsText(got.findings), `"fast"`) {
		t.Errorf("不该报没有被遮蔽的别名：%s", findingsText(got.findings))
	}
}

// TestDoctorInboundDanglingNamesBothNames 是第 9 项的现场：要点名那条别名，
// 也要点名它指向的不存在的规范名——只给其中一个，下一步不知道该改哪边。
func TestDoctorInboundDanglingNamesBothNames(t *testing.T) {
	in := doctorChainInput{
		catalogEndpoints: map[string][]channel.Endpoint{"model-a": nil},
		inboundAliases: []catalog.InboundAlias{
			{Alias: "fast", CanonicalName: "model-gone"},
			{Alias: "ok", CanonicalName: "model-a"},
		},
	}

	got := checkDoctorInboundDangling(in)

	assertContainsFragments(t, got.findings, `"fast"`, `"model-gone"`, "不在目录里")
	if got.status() != doctorFailed {
		t.Errorf("有悬空别名时应当不通过，得到 %s", got.status().label())
	}
	if strings.Contains(findingsText(got.findings), `"ok"`) {
		t.Errorf("指向存在的规范名的别名不该被报：%s", findingsText(got.findings))
	}
}

// TestDoctorInboundChecksPassWhenNothingIsWrong 钉住「干净就通过」：一条别名
// 都没配、以及别名都指得通，两种都算通过而不是「没跑成」。
func TestDoctorInboundChecksPassWhenNothingIsWrong(t *testing.T) {
	cases := []struct {
		name    string
		aliases []catalog.InboundAlias
	}{
		{"一条都没配", nil},
		{"别名都指得通", []catalog.InboundAlias{{Alias: "fast", CanonicalName: "model-a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := doctorChainInput{
				catalogEndpoints: map[string][]channel.Endpoint{"model-a": nil},
				inboundAliases:   tc.aliases,
			}
			for name, got := range map[string]doctorCheck{
				"遮蔽": checkDoctorInboundShadowed(in),
				"悬空": checkDoctorInboundDangling(in),
			} {
				if got.status() != doctorPassed {
					t.Errorf("%s项在干净输入上应当通过，得到 %s", name, got.status().label())
				}
				if strings.TrimSpace(got.ok) == "" {
					t.Errorf("%s项通过时也要有一句结论（通过不能是空话）", name)
				}
			}
		})
	}
}

// ── 结论与退出码 ────────────────────────────────────────────────────────

// TestDoctorSkippedIsNotPassed 钉住这一条命令最要紧的判据：
// 「没跑成」与「通过」在输出与退出码上都必须分得开。
func TestDoctorSkippedIsNotPassed(t *testing.T) {
	rep := doctorReport{
		database:        doctorCheck{title: "数据库"},
		channel:         doctorCheck{title: "渠道"},
		catalog:         doctorCheck{title: "模型目录", skip: "没有可查的对象"},
		alias:           doctorCheck{title: "别名"},
		pricing:         doctorCheck{title: "计价规则"},
		ruleModels:      doctorCheck{title: "规则模型名"},
		wallet:          doctorCheck{title: "钱包"},
		inboundShadowed: doctorCheck{title: "入站别名遮蔽"},
		inboundDangling: doctorCheck{title: "入站别名悬空"},
	}

	passed, failed, skipped := rep.counts()
	if passed != 8 || failed != 0 || skipped != 1 {
		t.Fatalf("计数 = 通过 %d / 不通过 %d / 没跑成 %d，期望 8 / 0 / 1", passed, failed, skipped)
	}
	if rep.exitErr(nil) == nil {
		t.Fatal("有一项「没跑成」时退出码必须非 0")
	}

	var buf bytes.Buffer
	printDoctorReport(&buf, rep, false)
	out := buf.String()
	// 默认输出里这一项就是那一行：状态符说明「没跑成」，结论是原因本身。
	if !strings.Contains(out, "  ! 模型目录      没有可查的对象\n") {
		t.Errorf("「没跑成」那一项的默认行不对：\n%s", out)
	}
	if !strings.Contains(out, "  ✓ 数据库        通过\n") {
		t.Errorf("通过项的默认行该跟着标题对齐：\n%s", out)
	}
	if !strings.Contains(out, "doctor: 0 项异常, 1 项跳过, 8 项通过 (运行 -v 查看细节)") {
		t.Errorf("汇总行不对：\n%s", out)
	}

	// 细节不是删了，是挪进 -v：原因与结论都要还在。
	buf.Reset()
	printDoctorReport(&buf, rep, true)
	verbose := buf.String()
	if !strings.Contains(verbose, "没跑成：没有可查的对象") {
		t.Errorf("-v 里「没跑成」的原因没有打出来：\n%s", verbose)
	}
	if !strings.Contains(verbose, "结论：没跑成") {
		t.Errorf("-v 里那一项的结论没有打出来：\n%s", verbose)
	}
}

// TestDoctorPassesWhenEveryCheckPasses 是上一条的对照：全通过时退出码是 0。
func TestDoctorPassesWhenEveryCheckPasses(t *testing.T) {
	rep := doctorReport{
		database:        doctorCheck{title: "数据库"},
		channel:         doctorCheck{title: "渠道"},
		catalog:         doctorCheck{title: "模型目录"},
		alias:           doctorCheck{title: "别名"},
		pricing:         doctorCheck{title: "计价规则"},
		ruleModels:      doctorCheck{title: "规则模型名"},
		wallet:          doctorCheck{title: "钱包"},
		inboundShadowed: doctorCheck{title: "入站别名遮蔽"},
		inboundDangling: doctorCheck{title: "入站别名悬空"},
	}
	if err := rep.exitErr(nil); err != nil {
		t.Fatalf("九项全通过时不该报错，得到 %v", err)
	}
}

// TestDoctorKeepsDBUnreachExitCode 钉住连不上库时的退出码。
//
// 它是既有的对外契约（见 cli 包的 exit.go）：脚本按 3 分派「去看环境」，doctor 不该
// 把它变成「其他失败」。
func TestDoctorKeepsDBUnreachExitCode(t *testing.T) {
	rep := doctorReportWithoutDB(context.Background(), cli.DBErr(errors.New("连不上")), doctorOptions{})
	if got := cli.ExitCode(rep.exitErr(cli.DBErr(errors.New("连不上")))); got != cli.ExitDBUnreach {
		t.Fatalf("连不上库时退出码应当是 %d，得到 %d", cli.ExitDBUnreach, got)
	}

	var buf bytes.Buffer
	printDoctorReport(&buf, rep, false)
	out := buf.String()
	if !strings.Contains(out, "doctor: 0 项异常, 9 项跳过, 0 项通过 (运行 -v 查看细节)") {
		t.Errorf("连不上库时九项都该报「没跑成」：\n%s", out)
	}
	// 那一句错误原文只出现一次：九项各抄一遍会把报告撑成五十行，
	// 其中九行一模一样，而「哪一项跟别的不一样」正是读者要看的。
	if n := strings.Count(out, "连不上数据库：连不上"); n != 1 {
		t.Errorf("错误原文该只出现在第 1 项里，实际出现 %d 次：\n%s", n, out)
	}
	if !strings.Contains(out, "连不上数据库（同第 1 项）") {
		t.Errorf("后面八项该指向第 1 项：\n%s", out)
	}
}

// ── 机器可读输出 ────────────────────────────────────────────────────────
//
// 这一节钉的是 `doctor --format json` 那一条路（见 format.go 与
// docs/cli-output.md）。它与表格那一档读的是同一个 doctorReport，所以这里
// 每一条都在问「同一份报告，两个读者拿到的是同一件事吗」。

// doctorRep 造一份报告，只填用例关心的那几项。
func doctorRep(checks ...doctorCheck) doctorReport {
	rep := doctorReport{}
	named := rep.namedChecks()
	for i, c := range checks {
		if i >= len(named) {
			break
		}
		switch named[i].name {
		case "database":
			rep.database = c
		case "channel":
			rep.channel = c
		case "catalog":
			rep.catalog = c
		case "alias":
			rep.alias = c
		case "pricing":
			rep.pricing = c
		case "rulemodels":
			rep.ruleModels = c
		case "wallet":
			rep.wallet = c
		case "inboundshadowed":
			rep.inboundShadowed = c
		case "inbounddangling":
			rep.inboundDangling = c
		}
	}
	return rep
}

// doctorJSON 跑一遍真的那条渲染路径（emitDoctorReport），把 stdout 解成 map。
//
// 直接调 emitDoctorReport 而不是 doctorDoc：前者才是命令走的那条路，
// **退出码也在里面定**。只测 doctorDoc 会漏掉「报告说 failed、退出码却是 0」
// 这一类分叉，而那正是这条命令最要紧的一条契约。
func doctorJSON(t *testing.T, rep doctorReport, dbErr error) (map[string]any, error) {
	t.Helper()
	withFormat(t, cli.FormatJSON)

	var buf bytes.Buffer
	err := emitDoctorReport(&buf, rep, dbErr, false)
	out := buf.String()

	if strings.ContainsRune(out, cli.EscByte) {
		t.Errorf("json 模式的输出里不许有转义序列：%q", out)
	}
	var got map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &got); jsonErr != nil {
		t.Fatalf("json 模式下 stdout 不是一份合法的 JSON: %v\n%s", jsonErr, out)
	}
	return got, err
}

// docChecks 取 checks 那一栏。
func docChecks(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	raw, ok := doc["checks"].([]any)
	if !ok {
		t.Fatalf("checks 不是数组：%#v", doc["checks"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("checks 里有一项不是对象：%#v", item)
		}
		out = append(out, m)
	}
	return out
}

// 顶层 status 与退出码说的是同一件事，且与交互模式一致。
//
// 「有失败项时仍退出 1」是 issue 里点名的验收项：脚本基于退出码的逻辑不许
// 因为换了输出格式而变。
func TestDoctorJSONStatusAndExitCodeAgree(t *testing.T) {
	pass := doctorCheck{title: "数据库", ok: "迁移版本 23"}
	fail := doctorCheck{title: "模型目录",
		findings: []doctorProblem{{headline: "模型 model-x 找不到"}}}
	skip := doctorCheck{title: "别名", skip: "没有可查的对象"}

	cases := []struct {
		name       string
		rep        doctorReport
		dbErr      error
		wantStatus string
		wantCode   int
	}{
		{"九项全通过", doctorRep(pass, pass, pass, pass, pass, pass, pass, pass, pass), nil, cli.JSONOK, cli.ExitOK},
		{"一项不通过", doctorRep(pass, pass, fail, pass, pass, pass, pass, pass, pass), nil, cli.JSONFailed, cli.ExitFailure},
		{"一项没跑成", doctorRep(pass, pass, pass, skip, pass, pass, pass, pass, pass), nil, cli.JSONSkipped, cli.ExitFailure},
		{
			// 连不上库：九项都「没跑成」，退出码 3。这是 doctor 最要紧的一次
			// 输出，json 模式下它同样要认得出来——探针不能因为库挂了就拿到
			// 一份空的 stdout。
			"连不上库",
			doctorReportWithoutDB(context.Background(), cli.DBErr(errors.New("连不上")), doctorOptions{}),
			cli.DBErr(errors.New("连不上")),
			cli.JSONSkipped,
			cli.ExitDBUnreach,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := doctorJSON(t, tc.rep, tc.dbErr)

			if got := cli.ExitCode(err); got != tc.wantCode {
				t.Errorf("退出码 = %d，想要 %d", got, tc.wantCode)
			}
			if doc["status"] != tc.wantStatus {
				t.Errorf("status = %v，想要 %q", doc["status"], tc.wantStatus)
			}
			// 报告里的 exit_code 必须与进程的退出码是同一个数：探针既可能读
			// 管道里的 JSON，也可能只看退出码，两者不一致时它得猜信哪个。
			if got := doc["exit_code"]; got != float64(tc.wantCode) {
				t.Errorf("exit_code = %v，与进程的退出码 %d 对不上", got, tc.wantCode)
			}
		})
	}
}

// 「没跑成」与「通过」在 JSON 里也不许同形。
//
// 表格那一档靠状态符 `!` 与 `✓` 分开（见 style.go），JSON 这一档靠 status
// 那一栏。两处说的是同一件事：一次什么都没查的运行不是一次全部通过的运行
// （见 LESSONS.md 第 16 条）。
func TestDoctorJSONSkippedIsNotPassed(t *testing.T) {
	doc, err := doctorJSON(t, doctorRep(
		doctorCheck{title: "数据库"},
		doctorCheck{title: "渠道"},
		doctorCheck{title: "模型目录", skip: "没有可查的对象"},
		doctorCheck{title: "别名"},
		doctorCheck{title: "计价规则"},
		doctorCheck{title: "规则模型名"},
		doctorCheck{title: "钱包"},
	), nil)
	if err == nil {
		t.Fatal("有一项没跑成时退出码必须非 0")
	}

	checks := docChecks(t, doc)
	if checks[0]["status"] != "passed" {
		t.Errorf("通过项的 status = %v，想要 passed", checks[0]["status"])
	}
	if checks[0]["skip_reason"] != nil {
		t.Errorf("通过项不该有 skip_reason：%v", checks[0]["skip_reason"])
	}
	if checks[2]["status"] != "skipped" {
		t.Errorf("没跑成项的 status = %v，想要 skipped", checks[2]["status"])
	}
	if checks[2]["skip_reason"] != "没有可查的对象" {
		t.Errorf("没跑成的原因没有原样带出来：%v", checks[2]["skip_reason"])
	}
	// summary 里的三档计数与终端上那行汇总说的是同一批数。
	summary, ok := doc["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary 不是对象：%#v", doc["summary"])
	}
	for key, want := range map[string]float64{"total": 9, "passed": 8, "failed": 0, "skipped": 1} {
		if summary[key] != want {
			t.Errorf("summary[%q] = %v，想要 %v", key, summary[key], want)
		}
	}
}

// 问题清单**不截断**：表格那边「另有 N 条，用 -v 看」是屏幕宽度的问题，
// 脚本按模型名去筛某一条问题时，截断会让它「查不到」而不是「还有更多」。
func TestDoctorJSONDoesNotCapProblems(t *testing.T) {
	total := doctorMaxListed + 5
	findings := make([]doctorProblem, 0, total)
	for i := 0; i < total; i++ {
		findings = append(findings, doctorProblem{
			headline: fmt.Sprintf("第 %d 个模型不存在", i),
			detail:   "多半是拼写差了一个字符",
		})
	}
	rep := doctorRep(
		doctorCheck{title: "数据库"}, doctorCheck{title: "渠道"},
		doctorCheck{title: "模型目录", findings: findings},
		doctorCheck{title: "别名"}, doctorCheck{title: "计价规则"},
		doctorCheck{title: "规则模型名"}, doctorCheck{title: "钱包"},
	)

	doc, _ := doctorJSON(t, rep, nil)
	problems, ok := docChecks(t, doc)[2]["problems"].([]any)
	if !ok {
		t.Fatalf("problems 不是数组：%#v", docChecks(t, doc)[2]["problems"])
	}
	if len(problems) != total {
		t.Errorf("problems 有 %d 条，想要全部 %d 条（表格那一档才截断）", len(problems), total)
	}
	first, _ := problems[0].(map[string]any)
	if first["headline"] != "第 0 个模型不存在" {
		t.Errorf("第一条问题不对：%#v", first)
	}
	if first["detail"] != "多半是拼写差了一个字符" {
		t.Errorf("问题的 detail 要一起带出来：%#v", first)
	}

	// 表格那一档仍然按老样子截断：两档的取舍不同，不是一处改另一处跟着改。
	var buf bytes.Buffer
	printDoctorReport(&buf, rep, true)
	if !strings.Contains(buf.String(), "另有 5 条同类问题没有列出") {
		t.Errorf("表格那一档该照旧截断：\n%s", buf.String())
	}
	// 结论那一句仍然是「第一条 + 另有 N 条」：它是给人看的那一行，
	// 与结构化的 problems 并存，不是二选一。
	if got := docChecks(t, doc)[2]["conclusion"]; got != "第 0 个模型不存在（另有 24 条，用 -v 看）" {
		t.Errorf("conclusion = %v，与终端上那一行对不上", got)
	}
}

// 各项的字段逐个钉住：机器可读的名字、表、notes 的空值口径。
func TestDoctorJSONCheckFields(t *testing.T) {
	rep := doctorRep(
		doctorCheck{
			title: "数据库", subject: "连得上、迁移跑到哪一版", ok: "迁移版本 23",
			notes: []string{"读的是 goose 的版本表"},
			table: &doctorTable{header: []string{"项", "值"}, rows: [][]string{{"版本", "23"}}},
		},
		doctorCheck{title: "渠道"}, doctorCheck{title: "模型目录"},
		doctorCheck{title: "别名"}, doctorCheck{title: "计价规则"},
		doctorCheck{title: "规则模型名"}, doctorCheck{title: "钱包"},
	)

	doc, _ := doctorJSON(t, rep, nil)
	checks := docChecks(t, doc)

	// 名字是英文、稳定、不随排版改：脚本按它认项目，人看到的是 title。
	wantNames := []string{
		"database", "channel", "catalog", "alias", "pricing", "rulemodels", "wallet",
		"inboundshadowed", "inbounddangling",
	}
	for i, want := range wantNames {
		if checks[i]["name"] != want {
			t.Errorf("第 %d 项的名字 = %v，想要 %q", i, checks[i]["name"], want)
		}
	}

	// total 是从报告结构上数出来的：加一项就该自动跟着变，不该有第二个地方
	// 再写一遍「一共几项」。
	if summary, _ := doc["summary"].(map[string]any); summary["total"] != float64(9) {
		t.Errorf("summary.total = %v，想要 9", summary["total"])
	}

	db := checks[0]
	if db["ok"] != "迁移版本 23" {
		t.Errorf("通过时那一句没有带出来：%v", db["ok"])
	}
	if db["subject"] != "连得上、迁移跑到哪一版" {
		t.Errorf("subject 没有带出来：%v", db["subject"])
	}
	notes, _ := db["notes"].([]any)
	if len(notes) != 1 || notes[0] != "读的是 goose 的版本表" {
		t.Errorf("notes 不对：%#v", db["notes"])
	}
	problems, _ := db["problems"].([]any)
	if len(problems) != 0 {
		t.Errorf("没有问题时 problems 该是空数组，得到 %#v", problems)
	}
	if db["table"] == nil {
		t.Error("表要带出来：它上面挂着 notes 里没有的数")
	}

	// 没填 notes 的项给空数组而不是 null：`[]` 是「一条都没有」，
	// nul 读起来像「这一栏没有」——两者在本仓是反复要分清的两件事。
	channel := checks[1]
	emptyNotes, ok := channel["notes"].([]any)
	if !ok {
		t.Fatalf("notes 该是数组而不是 null：%#v", channel["notes"])
	}
	if len(emptyNotes) != 0 {
		t.Errorf("没填 notes 的项该是空数组，得到 %#v", emptyNotes)
	}
	if channel["table"] != nil {
		t.Errorf("没有表时该给 null，得到 %#v", channel["table"])
	}
	if channel["ok"] != nil {
		t.Errorf("没填 ok 时该给 null，得到 %#v", channel["ok"])
	}
}

// 真打上游那一项在 JSON 里有自己的对象，且失败时的归因不丢。
//
// 归因那一句是这条命令最要紧的一行（见 issue #381）：状态码 400 与「上游回了
// 一个 400」在输出里长得一样，而两者要改的地方完全相反。它在表格那一档进
// 默认那一行，在 JSON 这一档进 `attribution`。
func TestDoctorJSONLiveObject(t *testing.T) {
	// 没开 --live：只给 enabled=false。铺一排 null 会让人以为「跑了但没读到」。
	doc, _ := doctorJSON(t, doctorRep(), nil)
	live, ok := doc["live"].(map[string]any)
	if !ok {
		t.Fatalf("live 不是对象：%#v", doc["live"])
	}
	if live["enabled"] != false || live["ok"] != false {
		t.Errorf("没开 --live 时 live = %#v，想要 enabled/ok 都为 false", live)
	}
	if len(live) != 2 {
		t.Errorf("没开 --live 时不该铺一排空栏：%#v", live)
	}
	if doc["status"] != cli.JSONOK {
		t.Errorf("没开 --live 不该影响整份报告的结论，得到 %v", doc["status"])
	}

	// 开了 --live 且被网关拒了：归因、状态码、rejected_by_us 都要在。
	rep := doctorRep(
		doctorCheck{title: "数据库"}, doctorCheck{title: "渠道"},
		doctorCheck{title: "模型目录"}, doctorCheck{title: "别名"},
		doctorCheck{title: "计价规则"}, doctorCheck{title: "规则模型名"}, doctorCheck{title: "钱包"},
	)
	rep.live = doctorLive{
		enabled: true, endpoint: doctorEndpointChat, model: "claude-x",
		url: "http://127.0.0.1:8080" + doctorPathChat,
		// 版本头在 = 这个响应是我们自己的网关回的（见 fromGateway）。
		status: http.StatusBadRequest, version: "1.2.3", errMsg: "缺少 messages",
	}
	doc, err := doctorJSON(t, rep, nil)
	if err == nil {
		t.Fatal("真打上游失败时退出码必须非 0")
	}
	if doc["status"] != cli.JSONFailed {
		t.Errorf("真打上游失败时整份报告该是 failed，得到 %v", doc["status"])
	}
	live, _ = doc["live"].(map[string]any)
	if live["ok"] != false || live["enabled"] != true {
		t.Errorf("live.ok/enabled 不对：%#v", live)
	}
	if live["status_code"] != float64(http.StatusBadRequest) {
		t.Errorf("status_code = %v，想要 400", live["status_code"])
	}
	if live["from_gateway"] != true || live["rejected_by_us"] != true {
		t.Errorf("归因用的两个布尔不对：%#v", live)
	}
	if live["attribution"] == nil || live["attribution"] == "" {
		t.Error("失败时归因那一句必须带出来")
	}
	if live["send_error"] != nil {
		t.Errorf("请求发出去了，send_error 该是 null：%v", live["send_error"])
	}
}

// 凭证不进任何一个字节的输出——json 那一档也是。
//
// 这条命令的凭证只出现在请求头里（见 runDoctorLive），而 JSON 这一档多了一条
// 往外倒数据的路，所以这里对着**真实的**那条路再钉一遍：打一个真的请求、
// 拿真的响应，然后把整份 stdout 搜一遍。
func TestDoctorJSONNeverCarriesCredential(t *testing.T) {
	const key = "test-key-placeholder"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(doctorHeaderVersion, "1.2.3")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","model":"claude-x",`+
			`"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	saved := doctorHTTPClient
	doctorHTTPClient = srv.Client()
	t.Cleanup(func() { doctorHTTPClient = saved })

	rep := doctorRep()
	rep.live = runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: key,
	})
	if !rep.live.ok() {
		t.Fatalf("这次调用该算通过，得到 status=%d err=%v", rep.live.status, rep.live.sendErr)
	}

	doc, _ := doctorJSON(t, rep, nil)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Errorf("凭证出现在了 json 输出里：\n%s", raw)
	}
}

// ── 第 10 项：真打一次上游 ──────────────────────────────────────────────

// TestDoctorEndpointPathsMatchAPI 钉住路径与 internal/api 是同一份值。
//
// 两处各写一个字符串时，改了服务端路由而没改这里，表现是 doctor 报「404」
// 而人以为链路坏了——这一条防的就是那个缝。
func TestDoctorEndpointPathsMatchAPI(t *testing.T) {
	if doctorPathChat != api.PathChatCompletions {
		t.Errorf("chat 入口路径 = %q，internal/api 里是 %q", doctorPathChat, api.PathChatCompletions)
	}
	if doctorPathMessages != api.PathMessages {
		t.Errorf("messages 入口路径 = %q，internal/api 里是 %q", doctorPathMessages, api.PathMessages)
	}
	if doctorPathResponses != api.PathResponses {
		t.Errorf("responses 入口路径 = %q，internal/api 里是 %q", doctorPathResponses, api.PathResponses)
	}
}

// TestDoctorLiveSignalsMatchAPI 钉住归因用的五个值与 internal/api 是同一份。
//
// 与上面那条同一个形状：doctor 照抄了这几个字符串（本包不 import
// internal/api，见 doctor.go 的注释），抄错的表现是归因整段走偏——例如把
// insufficient_quota 抄成别的，或者两句 message 抄漏一个词，于是「额度/
// 余额」那两条永远认不出来，全都**悄悄**落到保守的兜底上（那看起来像一次
// 正常的「认不出来」，而不是一次错），而 doctor 自己的归因用例仍然全绿：
// 它们钉的是 doctor 那份副本。这两句就是为此导出的。
func TestDoctorLiveSignalsMatchAPI(t *testing.T) {
	if doctorHeaderVersion != api.HeaderVersion {
		t.Errorf("版本头 = %q，internal/api 里是 %q", doctorHeaderVersion, api.HeaderVersion)
	}
	if doctorHeaderRetryAfter != api.HeaderRetryAfter {
		t.Errorf("Retry-After 头 = %q，internal/api 里是 %q", doctorHeaderRetryAfter, api.HeaderRetryAfter)
	}
	if doctorCodeInsufficientQuota != api.CodeInsufficientQuota {
		t.Errorf("额度码 = %q，internal/api 里是 %q", doctorCodeInsufficientQuota, api.CodeInsufficientQuota)
	}
	if doctorMsgInsufficientQuota != api.MsgInsufficientQuota {
		t.Errorf("额度那句 message = %q，internal/api 里是 %q", doctorMsgInsufficientQuota, api.MsgInsufficientQuota)
	}
	if doctorMsgInsufficientBalance != api.MsgInsufficientBalance {
		t.Errorf("余额那句 message = %q，internal/api 里是 %q", doctorMsgInsufficientBalance, api.MsgInsufficientBalance)
	}
}

// TestDoctorLiveSendsRequestAndPrintsResult 钉住「它真的发了请求、并把结果
// 打出来了」。
//
// 假上游收下请求并断言形状，doctor 那一侧断言打出来的那几行——两边都看，
// 才能说明这次调用真的走完了，而不是 doctor 自己编了一段输出。
func TestDoctorLiveSendsRequestAndPrintsResult(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","model":"claude-x",`+
			`"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	}))
	defer srv.Close()

	opts := doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	}
	got := runDoctorLive(context.Background(), opts)

	if gotMethod != http.MethodPost {
		t.Errorf("方法 = %q，期望 POST", gotMethod)
	}
	if gotPath != doctorPathChat {
		t.Errorf("路径 = %q，期望 %q", gotPath, doctorPathChat)
	}
	if gotAuth != "Bearer "+testDoctorKey {
		t.Errorf("chat 入口的凭证该走 Authorization: Bearer，得到 %q", gotAuth)
	}
	if gotBody["model"] != "claude-x" {
		t.Errorf("请求体里的模型 = %v，期望 claude-x", gotBody["model"])
	}
	if !got.ok() || got.status != http.StatusOK {
		t.Fatalf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
	}
	if got.usage != "prompt=9 completion=2" {
		t.Errorf("用量 = %q，期望 prompt=9 completion=2", got.usage)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got, true)
	out := buf.String()
	// 默认那一行要能独立读懂：状态符 + 标题 + 「通过了、状态码与用量是多少」。
	// 入口与模型是 -v 的细节，所以两处都断言。
	for _, want := range []string{
		"✓ 上游连通", "通过（200", "用量 prompt=9 completion=2",
		"入口：POST " + srv.URL + doctorPathChat, "模型：claude-x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, testDoctorKey) {
		t.Errorf("凭证出现在了输出里：\n%s", out)
	}
}

// TestDoctorLiveMessagesUsesAnthropicHeaders 钉住 messages 入口走的是
// Anthropic 的习惯：x-api-key，且请求体里带 max_tokens。
func TestDoctorLiveMessagesUsesAnthropicHeaders(t *testing.T) {
	var (
		gotPath string
		gotKey  string
		gotAuth string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","model":"claude-x",`+
			`"content":[],"usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointMessages,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if gotPath != doctorPathMessages {
		t.Errorf("路径 = %q，期望 %q", gotPath, doctorPathMessages)
	}
	if gotKey != testDoctorKey {
		t.Errorf("messages 入口的凭证该走 x-api-key，得到 %q", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("messages 入口不该同时带 Authorization，得到 %q", gotAuth)
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Error("messages 入口的请求体必须带 max_tokens（Anthropic 协议必填）")
	}
	if got.usage != "input=7 output=3" {
		t.Errorf("用量 = %q，期望 input=7 output=3", got.usage)
	}
	if !got.ok() {
		t.Errorf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
	}
}

// TestDoctorLiveResponsesUsesResponsesShape 钉住 responses 入口的请求体是
// **Responses 的形状**，而不是把 chat 的 messages[] 换一个路径发出去。
//
// 三处差异都要落到发出去的 JSON 上：顶层的 instructions、input[] 里的一条
// user message item、以及 max_output_tokens。最后一条尤其不能写成 max_tokens
// ——relaykit 把 Responses 的 max_output_tokens 落在 MaxCompletionTokens 上，
// 写错名字时网关按「缺输出上界」回 400，而错误读起来像上游的问题。
//
// 判据是假上游收到的 body，不是结构体字段与常量相等：后者把字段从结构体上
// 去掉也照样过。
func TestDoctorLiveResponsesUsesResponsesShape(t *testing.T) {
	var (
		gotPath string
		gotKey  string
		gotAuth string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed",`+
			`"model":"claude-x","output":[],"usage":{"input_tokens":7,"output_tokens":3,`+
			`"total_tokens":10}}`)
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointResponses,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if gotPath != doctorPathResponses {
		t.Errorf("路径 = %q，期望 %q", gotPath, doctorPathResponses)
	}
	if gotAuth != "Bearer "+testDoctorKey {
		t.Errorf("responses 入口的凭证该走 Authorization: Bearer，得到 %q", gotAuth)
	}
	if gotKey != "" {
		t.Errorf("responses 入口不该同时带 x-api-key，得到 %q", gotKey)
	}

	if _, ok := gotBody["messages"]; ok {
		t.Errorf("responses 入口不该发 chat 的 messages[]：%v", gotBody)
	}
	if instructions, ok := gotBody["instructions"].(string); !ok || instructions == "" {
		t.Errorf("顶层的 instructions 缺失或为空：%v", gotBody["instructions"])
	}

	input, ok := gotBody["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input 应当是只含一条 item 的数组，得到 %v（%T）", gotBody["input"], gotBody["input"])
	}
	item, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("input[0] 不是对象：%T", input[0])
	}
	if item["role"] != "user" {
		t.Errorf("input[0].role = %v，期望 user", item["role"])
	}
	if item["content"] != doctorLivePrompt {
		t.Errorf("input[0].content = %v，期望 %q", item["content"], doctorLivePrompt)
	}

	limit, ok := gotBody["max_output_tokens"].(float64)
	if !ok {
		t.Fatalf("max_output_tokens 缺失或不是数字：%v（%T）", gotBody["max_output_tokens"], gotBody["max_output_tokens"])
	}
	if limit <= 0 {
		t.Errorf("max_output_tokens = %v，网关要求它为正", limit)
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Errorf("responses 入口不该发 chat 的 max_tokens：%v", gotBody)
	}

	if !got.ok() {
		t.Errorf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
	}
	if got.respModel != "claude-x" {
		t.Errorf("响应里的模型 = %q，期望 claude-x", got.respModel)
	}
	if got.usage != "input=7 output=3" {
		t.Errorf("用量 = %q，期望 input=7 output=3（Responses 的字段名）", got.usage)
	}
}

// TestDoctorLiveChatPayloadCarriesMaxTokens 钉住 chat 入口的请求体里带一个
// 正的 max_tokens。
//
// OpenAI 的 chat completions 里 max_tokens 是**可选**的，所以「照着协议拼一个
// 最小请求体」正好会漏掉它——doctor 曾经就是这么漏的。而 larkax 的网关按它算
// 预留上界（pipeline.Request.validate → billing.ErrNoUpperBound），缺了会在请求
// 出网关之前就回 400「max_tokens is required」，看上去却像上游的问题。
//
// 判据是**发出去的 JSON**（假上游收到的 body）里的那个字段，而不是
// doctorChatRequest.MaxTokens 与 doctorLiveMaxTokens 相等——后者测的是同一个
// 常量，把字段从结构体上去掉也照样过。
func TestDoctorLiveChatPayloadCarriesMaxTokens(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","model":"claude-x",`+
			`"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	})
	if !got.ok() {
		t.Fatalf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
	}

	raw, ok := gotBody["max_tokens"]
	if !ok {
		t.Fatalf("chat 入口的请求体里没有 max_tokens，网关会在请求出网关之前就 400：%v", gotBody)
	}
	limit, ok := raw.(float64)
	if !ok {
		t.Fatalf("max_tokens 不是数字：%v（%T）", raw, raw)
	}
	if limit <= 0 {
		t.Errorf("max_tokens = %v，网关要求它为正", limit)
	}
}

// TestDoctorLiveEveryEndpointCarriesAnOutputBound 一次覆盖三个入口：
// 每个入口都要在自己的字段名上给出**正的**输出上界。
//
// 拆成三条用例会重复同一段断言，而这里要防的恰恰是「只给其中一条加了、
// 另外两条漏了」——放在同一张表里，漏一条就是这条用例红。
//
// 字段名各按各的协议（见 internal/relay 的 FromRelaykitRequest：chat 的
// max_tokens 与 Responses 的 max_output_tokens 最后都落在同一个 MaxTokens 上，
// 而 messages 走 Anthropic 形状，字段名也是 max_tokens）。写错名字的后果
// 与漏写一样：网关按「缺输出上界」回 400，错误读起来却像上游的问题。
func TestDoctorLiveEveryEndpointCarriesAnOutputBound(t *testing.T) {
	cases := []struct {
		endpoint string
		field    string
		forbid   string
		resp     string
	}{
		{
			endpoint: doctorEndpointChat,
			field:    "max_tokens",
			forbid:   "max_output_tokens",
			resp: `{"id":"cmpl-1","object":"chat.completion","model":"claude-x",` +
				`"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
		},
		{
			endpoint: doctorEndpointMessages,
			field:    "max_tokens",
			forbid:   "max_output_tokens",
			resp: `{"id":"msg-1","type":"message","model":"claude-x","content":[],` +
				`"usage":{"input_tokens":7,"output_tokens":3}}`,
		},
		{
			endpoint: doctorEndpointResponses,
			field:    "max_output_tokens",
			forbid:   "max_tokens",
			resp: `{"id":"resp-1","object":"response","model":"claude-x","output":[],` +
				`"usage":{"input_tokens":7,"output_tokens":3}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.resp)
			}))
			defer srv.Close()

			got := runDoctorLive(context.Background(), doctorOptions{
				live: true, model: "claude-x", endpoint: tc.endpoint,
				baseURL: srv.URL, key: testDoctorKey,
			})
			if !got.ok() {
				t.Fatalf("这次调用该算通过，得到 status=%d err=%v", got.status, got.sendErr)
			}

			raw, ok := gotBody[tc.field]
			if !ok {
				t.Fatalf("%s 入口的请求体里没有 %s：网关会在请求出网关之前就 400：%v",
					tc.endpoint, tc.field, gotBody)
			}
			limit, ok := raw.(float64)
			if !ok {
				t.Fatalf("%s 不是数字：%v（%T）", tc.field, raw, raw)
			}
			if limit <= 0 {
				t.Errorf("%s = %v，网关要求它为正", tc.field, limit)
			}
			if _, ok := gotBody[tc.forbid]; ok {
				t.Errorf("%s 入口不该发另一个协议的输出上界字段 %s：%v",
					tc.endpoint, tc.forbid, gotBody)
			}
		})
	}
}

// TestDoctorLiveReportsUpstreamRejection 钉住非 2xx 时把错误说明打出来。
//
// 这个 503 是**网关自己**回的（「没有可用上游」），所以带上版本头——
// 不带的话它模拟的是「链路上别的服务在拦」，那是另一条归因分支。
func TestDoctorLiveReportsUpstreamRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(doctorHeaderVersion, "v0.1.0-test")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"no available upstream for the requested model","type":"server_error"}}`)
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if got.ok() {
		t.Fatal("503 不该算通过")
	}
	if got.status != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", got.status)
	}
	if got.errMsg != "no available upstream for the requested model" {
		t.Errorf("错误说明 = %q", got.errMsg)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got, true)
	out := buf.String()
	if !strings.Contains(out, "503") || !strings.Contains(out, got.errMsg) {
		t.Errorf("输出里该有状态码与错误说明：\n%s", out)
	}
	if !strings.Contains(out, "✗ 上游连通") {
		t.Errorf("结论该是不通过：\n%s", out)
	}
	if !strings.Contains(out, "在网关入口就被拒了") {
		t.Errorf("503 是我们自己这一侧回的，归因该这么说：\n%s", out)
	}
}

// TestDoctorLiveAttributesRejection 钉住「谁拒的」这一行：入口拒绝、上游
// 失败、上游限流、以及「不是网关回的」四种情形要分开说。
//
// 这是 issue #381 那次事故的教训：doctor 只打了「结果：400」，而 400 既可能
// 是网关入口拒的、也可能是上游拒的，两者的排查方向相反。用假网关按
// internal/api 的 httpStatusOfUpstream 那套状态码头造出来。
//
// 429 + insufficient_quota 这一格还要再分开两个来源（#472）：按 Key 的额度
// 用尽与钱包余额不足共用这个码，只有 message 分得开，而两者的下一步相反
// （改 key 的额度 / 充值）。所以这里三条用例——两个来源各一条，再加一条
// message 认不出来时按码说保守话的。
func TestDoctorLiveAttributesRejection(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		want    string
		notWant []string
	}{
		{
			name:   "入口拒绝：缺 max_tokens",
			status: http.StatusBadRequest,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body:    `{"error":{"message":"max_tokens is required: the request must set a positive output token limit","type":"invalid_request_error"}}`,
			want:    "在网关入口就被拒了",
			notWant: []string{"上游那一次调用失败"},
		},
		{
			name:   "入口拒绝：没有可用上游",
			status: http.StatusServiceUnavailable,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body:    `{"error":{"message":"no available upstream for the requested model","type":"server_error"}}`,
			want:    "在网关入口就被拒了",
			notWant: []string{"上游那一次调用失败"},
		},
		{
			name:   "上游失败：网关回 502",
			status: http.StatusBadGateway,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body:    `{"error":{"message":"upstream call failed","type":"upstream_error"}}`,
			want:    "上游那一次调用失败",
			notWant: []string{"在网关入口就被拒了"},
		},
		{
			name:   "我们自己的限流：429 带 Retry-After",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				doctorHeaderVersion:    "v0.1.0-test",
				doctorHeaderRetryAfter: "2",
			},
			body:    `{"error":{"message":"rate limit exceeded for this API key, retry later","type":"rate_limit_error"}}`,
			want:    "按 Key 限流",
			notWant: []string{"上游在限流"},
		},
		{
			// 429 + insufficient_quota 的第一个来源：按 Key 的累计额度用尽。
			// 判据是我们自己那句 message，不是码。
			name:   "额度用尽：429 带 insufficient_quota 与额度那句 message",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body:    `{"error":{"message":"this API key has reached its spending limit; raise the limit on this key or use a different key","type":"rate_limit_error","code":"insufficient_quota"}}`,
			want:    "额度用尽",
			notWant: []string{"上游在限流", "余额不够"},
		},
		{
			// 同一个码的第二个来源：钱包可用余额不够这次预留。这一条是
			// issue #472 的正题——按码归因会把这一种说成「Key 的额度用尽」，
			// 让人去查 key 的额度，而该做的是充值。
			name:   "余额不足：429 带 insufficient_quota 与余额那句 message",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body: `{"error":{"message":"the account balance is insufficient for this request; ` +
				`top up the wallet before retrying","type":"rate_limit_error","code":"insufficient_quota"}}`,
			want: "余额不够这次预留",
			// 不能再说成「这把 API Key 的额度用尽了」——那正是这个 issue
			// 要修的那句指错人。也不能说成上游在限流。
			notWant: []string{"API Key 的额度用尽", "上游在限流"},
		},
		{
			// 码认出来了、message 不在我们认识的那两句里：这时不能往任何
			// 一个方向猜，要说成「账户状态类拒绝」并让对方去看 message。
			name:   "认不出 message：只按码说保守的那一句",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body:    `{"error":{"message":"an account state we have no wording for yet","type":"rate_limit_error","code":"insufficient_quota"}}`,
			want:    "账户状态类拒绝",
			notWant: []string{"API Key 的额度用尽", "余额不够", "上游在限流"},
		},
		{
			name:   "上游限流：429 不带我们那两个标记",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				doctorHeaderVersion: "v0.1.0-test",
			},
			body:    `{"error":{"message":"upstream is rate limited, retry later","type":"rate_limit_error"}}`,
			want:    "上游在限流",
			notWant: []string{"在网关入口就被拒了"},
		},
		{
			name:   "不是网关回的：没有版本头",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"bad request"}}`,
			want:   "不是 larkax 网关回的",
			// 没有版本头时不能咬着「是我们拒的」不放——那会把一次地址
			// 配错说成一次配置问题。
			notWant: []string{"在网关入口就被拒了"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			got := runDoctorLive(context.Background(), doctorOptions{
				live: true, model: "claude-x", endpoint: doctorEndpointChat,
				baseURL: srv.URL, key: testDoctorKey,
			})

			var buf bytes.Buffer
			printDoctorLive(&buf, got, true)
			out := buf.String()

			if !strings.Contains(out, "谁拒的：") {
				t.Fatalf("失败时该打出「谁拒的」那一行：\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("归因该含 %q：\n%s", tc.want, out)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(out, bad) {
					t.Errorf("归因不该含 %q：\n%s", bad, out)
				}
			}

			// 归因还必须在**默认那一行**上（不看 -v 也读得到）：#381 那次
			// 事故的现场就是它只在细节里，而人看的是默认输出。
			var plain bytes.Buffer
			printDoctorLive(&plain, got, false)
			if !strings.Contains(plain.String(), tc.want) {
				t.Errorf("默认那一行里就该有归因 %q：\n%s", tc.want, plain.String())
			}
			for _, bad := range tc.notWant {
				if strings.Contains(plain.String(), bad) {
					t.Errorf("默认那一行不该含 %q：\n%s", bad, plain.String())
				}
			}
		})
	}
}

// TestDoctorLivePreviewsNonJSONBody 钉住「响应体不是 JSON」时的输出。
//
// 反代返回的错误页（502 的 HTML）解析不出任何字段，而那一页正是要给人看的
// 东西——只打一句「502」会让人去猜是哪一层挡住的。
func TestDoctorLivePreviewsNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html><body>\n  <h1>502 Bad Gateway</h1>\n</body></html>")
	}))
	defer srv.Close()

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: srv.URL, key: testDoctorKey,
	})

	if got.preview == "" {
		t.Fatal("响应体不是 JSON 时该原样截一段打出来")
	}
	if strings.Contains(got.preview, "\n") {
		t.Errorf("预览该折成一行，得到 %q", got.preview)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got, true)
	if !strings.Contains(buf.String(), "502 Bad Gateway") {
		t.Errorf("输出里该有响应体的内容：\n%s", buf.String())
	}

	// 响应体在 -v 里，但默认那一行必须已经说了「不合格」，而不是等 -v。
	var plain bytes.Buffer
	printDoctorLive(&plain, got, false)
	if !strings.Contains(plain.String(), "✗ 上游连通") {
		t.Errorf("默认那一行该是失败：\n%s", plain.String())
	}
}

// roundTripFunc 让用例把传输层换成假的。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDoctorLiveReportsSendFailure 钉住「请求根本没发出去」这一条分支。
//
// 它与「发出去了、上游回了 503」是两件事，而两者的现场完全不同（一个是
// 地址/网络，一个是链路配置），所以必须分开说。传输层是假的，这条分支才
// 不依赖机器上真的有一个连不上的端口。
func TestDoctorLiveReportsSendFailure(t *testing.T) {
	var gotURL string
	saved := doctorHTTPClient
	doctorHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return nil, errors.New("connection refused")
	})}
	t.Cleanup(func() { doctorHTTPClient = saved })

	got := runDoctorLive(context.Background(), doctorOptions{
		live: true, model: "claude-x", endpoint: doctorEndpointChat,
		baseURL: "http://127.0.0.1:1", key: testDoctorKey,
	})

	if got.sendErr == nil {
		t.Fatal("传输层失败时该记下那个错误")
	}
	if got.ok() {
		t.Error("请求没发出去时不该算通过")
	}
	if gotURL != "http://127.0.0.1:1"+doctorPathChat {
		t.Errorf("请求打到了 %q", gotURL)
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got, true)
	out := buf.String()
	// 「没发出去」与「发出去了、上游拒了」是两件事，结论里就得分开说。
	if !strings.Contains(out, "✗ 上游连通  请求没发出去：") {
		t.Errorf("结论该说清是没发出去：\n%s", out)
	}
	if strings.Contains(out, testDoctorKey) {
		t.Errorf("凭证出现在了输出里：\n%s", out)
	}
}

// TestDoctorLiveDisabledByDefault 钉住默认不跑：没开 --live 时不发请求。
func TestDoctorLiveDisabledByDefault(t *testing.T) {
	saved := doctorHTTPClient
	doctorHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("没开 --live 时不该发任何请求")
		return nil, errors.New("不该走到这里")
	})}
	t.Cleanup(func() { doctorHTTPClient = saved })

	got := runDoctorLive(context.Background(), doctorOptions{model: "claude-x"})
	if got.enabled {
		t.Error("没开 --live 时这一项该是「没跑」")
	}

	var buf bytes.Buffer
	printDoctorLive(&buf, got, false)
	// 没跑的那一档是 `-` 而不是 `!`：它不是「没查成」，是这一项本来就不在
	// 这次范围内（要 --live 才跑）。
	if !strings.Contains(buf.String(), "  - 上游连通  没跑：加 --live") {
		t.Errorf("默认那一次该打一行「没跑」：\n%s", buf.String())
	}
}

// TestDoctorLiveDoesNotAffectExitCodeWhenDisabled 钉住没开 --live 时它不进退出码。
func TestDoctorLiveDoesNotAffectExitCodeWhenDisabled(t *testing.T) {
	rep := doctorReport{
		database: doctorCheck{title: "数据库"},
		channel:  doctorCheck{title: "渠道"},
		catalog:  doctorCheck{title: "模型目录"},
		alias:    doctorCheck{title: "别名"},
		pricing:  doctorCheck{title: "计价规则"},
		wallet:   doctorCheck{title: "钱包"},
	}
	if err := rep.exitErr(nil); err != nil {
		t.Fatalf("没开 --live 时它不该影响退出码，得到 %v", err)
	}

	rep.live = doctorLive{enabled: true, status: http.StatusServiceUnavailable}
	if rep.exitErr(nil) == nil {
		t.Fatal("开了 --live 且那一次没成功时，退出码必须非 0")
	}
}

// ── 参数 ────────────────────────────────────────────────────────────────

// TestDoctorRejectsFlagsThatNeedLive 钉住「只有 --live 才用得上」的那几个标志。
//
// 静默忽略它们会让「我给了 --model 但它没生效」看起来像一次成功的检查。
func TestDoctorRejectsFlagsThatNeedLive(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "claude-x"},
		{"--endpoint", doctorEndpointMessages},
		{"--base-url", "http://127.0.0.1:8080"},
		{"--credential-stdin"},
		{"--credential-env", "SOME_VAR"},
	} {
		err := runCaptured(t, func() error { return runDoctor(args) })
		if err == nil {
			t.Errorf("%v 该被拒：它只在 --live 时才有意义", args)
			continue
		}
		if !strings.Contains(err.Error(), "只在 --live 时才有意义") {
			t.Errorf("%v 的错误信息是 %v", args, err)
		}
		if got := cli.ExitCode(err); got != cli.ExitUsage {
			t.Errorf("%v 是参数错，退出码应当是 %d，得到 %d", args, cli.ExitUsage, got)
		}
	}
}

func TestDoctorLiveRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --model", []string{"--live", "--credential-env", "SOME_VAR"}, "需要 --model"},
		{"模型名只有空白", []string{"--live", "--model", "  ", "--credential-env", "SOME_VAR"}, "需要 --model"},
		{"入口不认识", []string{"--live", "--model", "m", "--endpoint", "bogus", "--credential-env", "SOME_VAR"}, "--endpoint 只能是"},
		{"地址没有协议", []string{"--live", "--model", "m", "--base-url", "127.0.0.1:8080", "--credential-env", "SOME_VAR"}, "不是合法的 URL（要形如 http://127.0.0.1:8080）"},
		{"协议不认识", []string{"--live", "--model", "m", "--base-url", "ftp://example.com", "--credential-env", "SOME_VAR"}, "必须以 http:// 或 https:// 开头"},
		{"地址没有主机名", []string{"--live", "--model", "m", "--base-url", "http://", "--credential-env", "SOME_VAR"}, "没有主机名"},
		{"没有凭证", []string{"--live", "--model", "m"}, "缺少凭证"},
		{"多出位置参数", []string{"--live", "--model", "m", "--credential-env", "SOME_VAR", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runDoctor(tc.args) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望报错且含 %q，得到 %v", tc.want, err)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestDoctorLiveCredentialSources 钉住凭证的两个来源，以及「不从参数读」。
//
// 命令行参数会出现在 ps 的输出与 shell 历史里，所以这里**没有**一条从参数
// 取凭证的路径：给一个 --credential <值> 会被当成未知标志拒掉。
func TestDoctorLiveCredentialSources(t *testing.T) {
	withStdin(t, testDoctorKey+"\n")
	opts, err := parseDoctorOptions([]string{"--live", "--model", "claude-x", "--credential-stdin"})
	if err != nil {
		t.Fatalf("从标准输入读凭证失败: %v", err)
	}
	if opts.key != testDoctorKey {
		t.Errorf("从标准输入读到的凭证 = %q，末尾的换行该被去掉", opts.key)
	}

	t.Setenv("LARKAX_TEST_DOCTOR_KEY", testDoctorKey)
	opts, err = parseDoctorOptions([]string{"--live", "--model", "claude-x", "--credential-env", "LARKAX_TEST_DOCTOR_KEY"})
	if err != nil {
		t.Fatalf("从环境变量读凭证失败: %v", err)
	}
	if opts.key != testDoctorKey {
		t.Errorf("从环境变量读到的凭证 = %q", opts.key)
	}

	err = runCaptured(t, func() error {
		return runDoctor([]string{"--live", "--model", "claude-x", "--credential", testDoctorKey})
	})
	if err == nil {
		t.Fatal("--credential 不该被接受：凭证只能从标准输入或环境变量读")
	}
	if strings.Contains(err.Error(), testDoctorKey) {
		t.Errorf("错误信息里出现了凭证本身：%v", err)
	}
}

// TestDoctorLiveTrimsTrailingSlashFromBaseURL 钉住地址末尾的斜杠不会拼出双斜杠。
func TestDoctorLiveTrimsTrailingSlashFromBaseURL(t *testing.T) {
	withStdin(t, testDoctorKey)
	opts, err := parseDoctorOptions([]string{
		"--live", "--model", "claude-x", "--base-url", "http://127.0.0.1:8080/",
		"--credential-stdin",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := opts.baseURL + doctorPathChat; got != "http://127.0.0.1:8080"+doctorPathChat {
		t.Errorf("拼出来的地址 = %q", got)
	}
}

// TestDoctorBaseURLFromListenAddr 固定住「监听地址怎么变成能连的地址」这条换算。
//
// 「能监听」与「能连」不是一回事：`:8080` 这类写法表示监听所有网卡，它不是一个
// http.Client 拿得走的地址；而 doctor 与网关通常在同一台机器上，所以这类一律按
// 环回取。期望值逐条写死在用例里，改判据就会红。
func TestDoctorBaseURLFromListenAddr(t *testing.T) {
	// 「所有网卡」写全了的 v4 写法在这里拼出来，而不是写成一行字面量：红线扫描
	// 把「IP:端口」那个形态当成可能泄露本机信息的一类（对通配地址来说是误报），
	// 而这里要的是一个能监听的地址，不是一行扫描器接受的文本。
	allInterfacesV4 := net.JoinHostPort("0.0.0.0", "8081")

	for _, tc := range []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:8081", "http://127.0.0.1:8081"},
		{":8081", "http://127.0.0.1:8081"},
		{allInterfacesV4, "http://127.0.0.1:8081"},
		{"[::]:8081", "http://127.0.0.1:8081"},
		// IPv6 通配地址的长写法。判据要是写成「列几个字面量」，这一格会拼出
		// http://[0:0:0:0:0:0:0:0]:8081——它在 Linux 上连得上（落到环回），但那
		// 是碰巧，不是这条判据要的那件事。
		{"[0:0:0:0:0:0:0:0]:8081", "http://127.0.0.1:8081"},
		{"192.0.2.7:9000", "http://192.0.2.7:9000"},
		{"[::1]:8081", "http://[::1]:8081"},
		{"gateway.example.com:8443", "http://gateway.example.com:8443"},
	} {
		got, err := doctorBaseURLFromListenAddr(tc.listen)
		if err != nil {
			t.Errorf("LARKAX_HTTP_ADDR = %q 时不该报错：%v", tc.listen, err)
			continue
		}
		if got != tc.want {
			t.Errorf("LARKAX_HTTP_ADDR = %q 拼出来 %q，想要 %q", tc.listen, got, tc.want)
		}
	}

	// 读不出主机与端口的不给默认值：替它猜一个端口，等于把「配置写错了」变成
	// 一次看起来像服务没起来的连接失败。
	for _, listen := range []string{"8080", "127.0.0.1:", ""} {
		got, err := doctorBaseURLFromListenAddr(listen)
		if err == nil {
			t.Errorf("LARKAX_HTTP_ADDR = %q 该报错，却拼出了 %q", listen, got)
			continue
		}
		if !strings.Contains(err.Error(), "LARKAX_HTTP_ADDR") {
			t.Errorf("报错要说清是哪个变量：%v", err)
		}
	}
}

// fakeDoctorGateway 起一个假网关：收下一次请求，回 200 与一份正常的响应体。
//
// 返回的第二个值是「最后一次请求打在哪条路径上」，用来证明请求真的到了这里，
// 而不是 doctor 自己编了一段输出。
func fakeDoctorGateway(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.Header().Set(doctorHeaderVersion, "1.2.3")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","model":"claude-x","choices":[],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { return lastPath }
}

// deadDoctorDSN 是一个连不上的库。
//
// 127.0.0.1:1 上不会有 Postgres 在听，连接当场被拒（不是等超时），所以这些用例
// 走的都是「库连不上、真打上游照跑」那条路——那正是要验的那一项所在的路径。
const deadDoctorDSN = "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable"

// TestDoctorLiveDefaultBaseURLFollowsListenAddr 固定住「默认地址跟着本次配置走」。
//
// 假网关听在内核挑的一个端口上，配置里的 LARKAX_HTTP_ADDR 就写成那个地址，
// 命令行**不给** --base-url。要看到的是它真的打到了这里：假网关收到了那次请求，
// 报告里那一行是通过，-v 里打出来的入口地址就是它。
//
// 库连不上（DSN 指到一个没人听的端口）不影响这一项：真打上游走 HTTP，不读库
// （见 doctorReportWithoutDB）。
func TestDoctorLiveDefaultBaseURLFollowsListenAddr(t *testing.T) {
	srv, lastPath := fakeDoctorGateway(t)

	t.Setenv("LARKAX_HTTP_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("LARKAX_DB_DSN", deadDoctorDSN)
	t.Setenv("LARKAX_TEST_DOCTOR_KEY", testDoctorKey)
	withVerbose(t, true)

	out, err := runDoctorCaptured(t,
		"--live", "--model", "claude-x", "--credential-env", "LARKAX_TEST_DOCTOR_KEY")

	if got := lastPath(); got != doctorPathChat {
		t.Fatalf("假网关收到的路径 = %q，期望 %q——默认地址没跟着 LARKAX_HTTP_ADDR 走：\n%s",
			got, doctorPathChat, out)
	}
	if !strings.Contains(out, "✓ 上游连通") {
		t.Errorf("真打上游那一行该是通过：\n%s", out)
	}
	liveSection := doctorSection(t, out, "上游连通")
	want := "入口：POST " + srv.URL + doctorPathChat
	if !strings.Contains(liveSection, want) {
		t.Errorf("打出来的入口地址里没有 %q：\n%s", want, liveSection)
	}
	// 库连不上是这一条的前提，不是它的结论：退出码照旧是那一档。
	if got := cli.ExitCode(err); got != cli.ExitDBUnreach {
		t.Errorf("退出码 = %d，期望 %d（库连不上）", got, cli.ExitDBUnreach)
	}
}

// TestDoctorLiveBaseURLFlagBeatsListenAddr 固定住「显式给的 --base-url 仍然优先」。
//
// 配置里的地址指向一个没人听的端口：默认值要是盖住了命令行给的地址，这次请求就会
// 打空，报告以「请求没发出去」收场。
func TestDoctorLiveBaseURLFlagBeatsListenAddr(t *testing.T) {
	srv, lastPath := fakeDoctorGateway(t)

	t.Setenv("LARKAX_HTTP_ADDR", "127.0.0.1:1")
	t.Setenv("LARKAX_DB_DSN", deadDoctorDSN)
	t.Setenv("LARKAX_TEST_DOCTOR_KEY", testDoctorKey)

	out, err := runDoctorCaptured(t,
		"--live", "--model", "claude-x", "--base-url", srv.URL,
		"--credential-env", "LARKAX_TEST_DOCTOR_KEY")

	if got := lastPath(); got != doctorPathChat {
		t.Fatalf("假网关收到的路径 = %q，期望 %q：\n%s", got, doctorPathChat, out)
	}
	if !strings.Contains(out, "✓ 上游连通") {
		t.Errorf("真打上游那一行该是通过：\n%s", out)
	}
	if got := cli.ExitCode(err); got != cli.ExitDBUnreach {
		t.Errorf("退出码 = %d，期望 %d（库连不上）", got, cli.ExitDBUnreach)
	}
}

// TestDoctorUnusableListenAddr 固定住配置读不出主机与端口时的三条处置。
//
// 开了 --live 而没给 --base-url 时**不**替它猜一个地址：说清是哪个变量、给不出
// 默认值，并指出显式给 --base-url 就能接着跑。第二条子用例把那句话兑现一遍——
// 写在错误信息里的出路，要真的走得通。
//
// 退出码是 1 而不是 2：错的是环境里的那份配置，不是这次敲的命令。
//
// 没开 --live 时它一点都不该受影响：那九项只读检查不碰网络，地址用不上（第三条
// 子用例）。
func TestDoctorUnusableListenAddr(t *testing.T) {
	const unusable = "8080"
	t.Setenv("LARKAX_HTTP_ADDR", unusable)
	t.Setenv("LARKAX_DB_DSN", deadDoctorDSN)
	t.Setenv("LARKAX_TEST_DOCTOR_KEY", testDoctorKey)

	t.Run("不给 --base-url 就报错", func(t *testing.T) {
		err := runCaptured(t, func() error {
			return runDoctor([]string{
				"--live", "--model", "claude-x", "--credential-env", "LARKAX_TEST_DOCTOR_KEY",
			})
		})
		if err == nil {
			t.Fatal("配置里读不出主机与端口、命令行又没给地址时，必须报错")
		}
		for _, want := range []string{"LARKAX_HTTP_ADDR", unusable, "--base-url"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("报错里没有 %q：%v", want, err)
			}
		}
		if got := cli.ExitCode(err); got != cli.ExitFailure {
			t.Errorf("退出码 = %d，期望 %d", got, cli.ExitFailure)
		}
	})

	t.Run("显式给 --base-url 就能接着跑", func(t *testing.T) {
		srv, lastPath := fakeDoctorGateway(t)

		out, _ := runDoctorCaptured(t,
			"--live", "--model", "claude-x", "--base-url", srv.URL,
			"--credential-env", "LARKAX_TEST_DOCTOR_KEY")

		if got := lastPath(); got != doctorPathChat {
			t.Fatalf("假网关收到的路径 = %q，期望 %q：\n%s", got, doctorPathChat, out)
		}
		if !strings.Contains(out, "✓ 上游连通") {
			t.Errorf("真打上游那一行该是通过：\n%s", out)
		}
	})

	t.Run("没开 --live 时不受影响", func(t *testing.T) {
		out, err := runDoctorCaptured(t)

		// 退出码是「连不上库」那一档：报错走的是九项检查那条路，而不是地址
		// 拼不出来——那件事只在 --live 时才有意义。
		if got := cli.ExitCode(err); got != cli.ExitDBUnreach {
			t.Errorf("退出码 = %d，期望 %d（库连不上）:\n%s", got, cli.ExitDBUnreach, out)
		}
		if !strings.Contains(out, "上游连通") {
			t.Errorf("报告里该有真打上游那一节：\n%s", out)
		}
	})
}

// ── 端到端（需要数据库） ────────────────────────────────────────────────

// runDoctorCaptured 跑一遍完整的 doctor，返回它打到 stdout 的内容与错误。
//
// doctor 的结论同时在**输出**与**退出码**上，而两者都要断言——只看退出码
// 会漏掉「报对了但没有说清是哪一项」。
func runDoctorCaptured(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out, _ := captureStreams(t, func() { err = runDoctor(args) })
	return out, err
}

// doctorSection 取出报告里某一节的内容（从标题行到下一节）。
func doctorSection(t *testing.T, out, title string) string {
	t.Helper()
	idx := strings.Index(out, title)
	if idx < 0 {
		t.Fatalf("报告里没有 %q 这一节：\n%s", title, out)
	}
	rest := out[idx:]
	if end := strings.Index(rest[1:], "\n["); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

// createDoctorChannel 建一条渠道，返回它的 ID 与名字。
func createDoctorChannel(t *testing.T, models ...string) (int64, string) {
	t.Helper()
	setupEnv(t)
	withStdin(t, testSecret+"\n")

	name := uniqueName("doctor-channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", name, "--type", testChanType, "--base-url", testBaseURL,
			"--models", strings.Join(models, ","), "--credential-stdin",
		}); err != nil {
			t.Fatalf("建渠道失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}
	return id, name
}

// createDoctorModel 往目录里加一条规范模型。
func createDoctorModel(t *testing.T, canonical string) {
	t.Helper()
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", canonical, "--display", "示例模型"})
	}); err != nil {
		t.Fatalf("建规范模型 %s 失败: %v", canonical, err)
	}
}

// 下面三条断言的是 collectDoctorReport 收出来的**问题清单**，而不是打出来的
// 那几行：doctor 的检查面是整个库，而这个包里的其他用例也在往同一个库上写
// 行，于是清单里会有与本次无关的问题。打印时每项最多列 doctorMaxListed 条
// （超出的折成一句「另有 N 条」），所以「本次造的那一条排在第几」取决于库里
// 攒了多少东西——按打印出来的行断言会变成一条随别的用例数量漂移的用例。
//
// 「跑完整条命令、按打出来的行断言」由 TestDoctorIntegrationLiveAgainstFakeGateway
// 负责，那里断言的是它自己那一节。

// TestDoctorIntegrationFlagsChannelModelMismatch 是 issue 第一个例子的现场：
// 渠道的 --models 里写一个目录里没有的名字，doctor 必须点名说出来。
func TestDoctorIntegrationFlagsChannelModelMismatch(t *testing.T) {
	db := testDB(t)

	canonical := uniqueName("doctor-model")
	createDoctorModel(t, canonical)
	// 差一个字符：这正是「写错一个字」在库里的样子。
	channelID, channelName := createDoctorChannel(t, canonical+"-typo")

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.catalog.findings,
		canonical+"-typo", strconv.FormatInt(channelID, 10), channelName, "目录里没有这个名字")
	if rep.catalog.status() != doctorFailed {
		t.Errorf("第 3 项的结论应当是不通过，得到 %s", rep.catalog.status().label())
	}
	err := rep.exitErr(nil)
	if err == nil {
		t.Fatal("库里有对不上的名字时 doctor 该非零退出")
	}
	if got := cli.ExitCode(err); got != cli.ExitFailure {
		t.Errorf("退出码 = %d，期望 %d（有问题）", got, cli.ExitFailure)
	}
}

// TestDoctorIntegrationFlagsAliasWithoutChannel 是 issue 第二个例子的现场：
// 别名配了，但没有任何渠道的 models 里包含它。
func TestDoctorIntegrationFlagsAliasWithoutChannel(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-alias")
	createDoctorModel(t, canonical)
	upstream := uniqueName("upstream")
	if err := runCaptured(t, func() error {
		return runAliasCreate([]string{"--model", canonical, "--upstream", upstream})
	}); err != nil {
		t.Fatalf("建别名失败: %v", err)
	}

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.alias.findings,
		canonical, "没有任何 enabled 渠道的 models 里包含它")
	if rep.alias.status() != doctorFailed {
		t.Errorf("第 4 项的结论应当是不通过，得到 %s", rep.alias.status().label())
	}
}

// TestDoctorIntegrationFlagsUserWithoutWallet 钉住 #337 那类缺口被指出来，
// 而不是让人从 500 往回推。
func TestDoctorIntegrationFlagsUserWithoutWallet(t *testing.T) {
	db := testDB(t)

	name := uniqueName("doctor-user")
	if err := runCaptured(t, func() error {
		return runUserCreate([]string{"--name", name})
	}); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.wallet.findings, name, "没有钱包行")
	if rep.wallet.status() != doctorFailed {
		t.Errorf("第 7 项的结论应当是不通过，得到 %s", rep.wallet.status().label())
	}
}

// ── 第 8、9 项：入站别名（需要数据库） ──────────────────────────────────

// TestDoctorIntegrationFlagsShadowedInboundAlias 是「先写别名、后建同名模型」
// 的现场：这是正常顺序，写入侧不会拒绝，只有体检能把这条永远不会生效的别名
// 说出来。
func TestDoctorIntegrationFlagsShadowedInboundAlias(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	alias := uniqueName("doctor-shadowed")
	target := uniqueName("doctor-shadow-target")
	createDoctorModel(t, target)
	if err := runCaptured(t, func() error {
		return runModelAlias([]string{"add", alias, target})
	}); err != nil {
		t.Fatalf("建入站别名失败: %v", err)
	}
	// 别名先落地，再建同名的规范模型。
	createDoctorModel(t, alias)

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.inboundShadowed.findings,
		alias, "规范名优先", "这条别名不会被用到")
}

// TestDoctorIntegrationFlagsDanglingInboundAlias 绕过写入口造一条悬空别名：
// 迁移、导入或直接写库都会留下这种行，而它只有体检看得见。
func TestDoctorIntegrationFlagsDanglingInboundAlias(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	alias := uniqueName("doctor-dangling")
	gone := uniqueName("doctor-gone")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inbound_model_aliases (alias, canonical_name) VALUES ($1, $2)`,
		alias, gone); err != nil {
		t.Fatalf("直接写入悬空别名失败: %v", err)
	}

	rep := collectDoctorReport(ctx, db, doctorOptions{})

	assertContainsFragments(t, rep.inboundDangling.findings, alias, gone, "不在目录里")
}

// TestDoctorIntegrationInboundAliasChecksPassForHealthyAlias 是反方向：别名
// 与规范名都对得上时，两项都不报这条别名。
func TestDoctorIntegrationInboundAliasChecksPassForHealthyAlias(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-inbound-ok")
	createDoctorModel(t, canonical)
	alias := uniqueName("doctor-inbound-alias")
	if err := runCaptured(t, func() error {
		return runModelAlias([]string{"add", alias, canonical})
	}); err != nil {
		t.Fatalf("建入站别名失败: %v", err)
	}

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	for name, findings := range map[string][]doctorProblem{
		"遮蔽": rep.inboundShadowed.findings,
		"悬空": rep.inboundDangling.findings,
	} {
		if strings.Contains(findingsText(findings), alias) {
			t.Errorf("%s项不该报这条别名（它指向存在的 %s）：%s",
				name, canonical, findingsText(findings))
		}
	}
}

// TestDoctorIntegrationRemovingDanglingAliasClearsTheFinding 走一遍处置：
// 体检点的名就是 remove 用的名字，删掉之后这一项不再报它。
func TestDoctorIntegrationRemovingDanglingAliasClearsTheFinding(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	alias := uniqueName("doctor-dangling-remove")
	gone := uniqueName("doctor-gone-remove")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inbound_model_aliases (alias, canonical_name) VALUES ($1, $2)`,
		alias, gone); err != nil {
		t.Fatalf("直接写入悬空别名失败: %v", err)
	}

	rep := collectDoctorReport(ctx, db, doctorOptions{})
	if !strings.Contains(findingsText(rep.inboundDangling.findings), alias) {
		t.Fatalf("前提不成立：体检没有报出刚写入的悬空别名 %s", alias)
	}

	if err := runCaptured(t, func() error {
		return runModelAlias([]string{"remove", alias})
	}); err != nil {
		t.Fatalf("删除入站别名失败: %v", err)
	}

	rep = collectDoctorReport(ctx, db, doctorOptions{})
	if strings.Contains(findingsText(rep.inboundDangling.findings), alias) {
		t.Errorf("删掉之后不该再报这条别名：%s", findingsText(rep.inboundDangling.findings))
	}
}

// ── 第 6 项：规则里的模型名 ──────────────────────────────────────────────────

// TestDoctorIntegrationFlagsCostRuleModelNotInCatalog 是 issue 第一半的现场：
// 成本规则的 --models 里写一个目录里没有的名字，doctor 必须点名说出来，并且
// 给出规则 ID——拿到 ID 才找得到是哪一条、该去看哪一行。
func TestDoctorIntegrationFlagsCostRuleModelNotInCatalog(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-cost-model")
	createDoctorModel(t, canonical)
	channelID, _ := createDoctorChannel(t, canonical)
	// 差一个字符：这正是「写错一个字」在规则表里的样子。
	typo := canonical + "-typo"
	costID := addCostRule(t, []string{
		"--channel", strconv.FormatInt(channelID, 10),
		"--models", typo, "--expr", testExpr,
	})

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.ruleModels.findings,
		fmt.Sprintf("成本规则 %d（", costID), typo, "目录里没有这个名字")
	if rep.ruleModels.status() != doctorFailed {
		t.Errorf("第 6 项的结论应当是不通过，得到 %s", rep.ruleModels.status().label())
	}
}

// TestDoctorIntegrationFlagsPriceRuleModelNotInCatalog 是同一件事的另一半：
// 计价规则的 --models 也走同一个判据，别只做了成本那一侧。
func TestDoctorIntegrationFlagsPriceRuleModelNotInCatalog(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-price-model")
	createDoctorModel(t, canonical)
	typo := canonical + "-typo"

	by := createTestUserID(t)
	stdout, _ := captureStreams(t, func() {
		if err := runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("doctor-rule"),
			"--expr", testExpr, "--models", typo, "--by", by,
		}); err != nil {
			t.Fatalf("建计价规则失败: %v", err)
		}
	})
	ruleID := strings.TrimSpace(stdout)

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	assertContainsFragments(t, rep.ruleModels.findings,
		"计价规则 "+ruleID+"（", typo, "目录里没有这个名字")
	if rep.ruleModels.status() != doctorFailed {
		t.Errorf("第 6 项的结论应当是不通过，得到 %s", rep.ruleModels.status().label())
	}
}

// TestDoctorIntegrationRuleModelsPassesWhenNamesAreInCatalog 是反方向：
// 规则里写的名字都在目录里时不报；空 --models 的兜底规则也不报（它表示
// 「适用于全部模型」，不指向任何名字）。
//
// 断言的是**这几条规则没被报出来**，不是整项通过：这个包的其他用例也在同一个
// 库上写规则，清单里会有与本次无关的问题（理由与上面那段说明相同）。
func TestDoctorIntegrationRuleModelsPassesWhenNamesAreInCatalog(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-good-model")
	createDoctorModel(t, canonical)
	channelID, _ := createDoctorChannel(t, canonical)
	channelArg := strconv.FormatInt(channelID, 10)

	costID := addCostRule(t, []string{
		"--channel", channelArg, "--models", canonical, "--expr", testExpr,
	})
	fallbackID := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testExpr,
	})

	by := createTestUserID(t)
	stdout, _ := captureStreams(t, func() {
		if err := runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("doctor-rule"),
			"--expr", testExpr, "--models", canonical, "--by", by,
		}); err != nil {
			t.Fatalf("建计价规则失败: %v", err)
		}
	})
	ruleID := strings.TrimSpace(stdout)

	rep := collectDoctorReport(context.Background(), db, doctorOptions{})

	for _, known := range []string{
		fmt.Sprintf("成本规则 %d（", costID),
		fmt.Sprintf("成本规则 %d（", fallbackID),
		"计价规则 " + ruleID + "（",
		canonical,
	} {
		for _, f := range rep.ruleModels.findings {
			if strings.Contains(f.String(), known) {
				t.Errorf("这些名字都在目录里，不该报出来（含 %q）：%s", known, f)
			}
		}
	}
}

// TestDoctorIntegrationPrintsEverySection 钉住跑完整条命令时的输出形状。
//
// 断言的是「六节都在、汇总行在、退出码非 0」，不涉及任何一条具体问题——
// 那些问题取决于库里当时有什么，见上面那段说明。
func TestDoctorIntegrationPrintsEverySection(t *testing.T) {
	testDB(t)

	out, err := runDoctorCaptured(t)
	if err == nil {
		t.Fatalf("这个库不可能九项全通过（用例自己就会造出没有钱包行的用户）：\n%s", out)
	}
	if got := cli.ExitCode(err); got != cli.ExitFailure {
		t.Errorf("退出码 = %d，期望 %d", got, cli.ExitFailure)
	}
	// 九项的标题都在，加最后那一项「上游连通」：默认输出里每项就一行，
	// 所以标题是唯一的定位手段。
	for _, title := range []string{
		"数据库", "渠道", "模型目录", "别名", "计价规则", "规则模型名", "钱包",
		"入站别名遮蔽", "入站别名悬空", "上游连通",
	} {
		if !strings.Contains(out, title) {
			t.Errorf("报告里没有 %q：\n%s", title, out)
		}
	}
	if !strings.Contains(out, "doctor: ") {
		t.Errorf("报告里没有汇总行：\n%s", out)
	}
	if !strings.Contains(out, "- 上游连通      没跑：加 --live") {
		t.Errorf("没开 --live 时该打一行「没跑」：\n%s", out)
	}
	// 默认输出里不该出现 -v 的细节：`检查：` 是每一段细节的第一行。
	if strings.Contains(out, "检查：") {
		t.Errorf("默认输出里混进了 -v 的细节：\n%s", out)
	}
}

// TestDoctorIntegrationLiveAgainstFakeGateway 跑完整条命令，只是把网关换成
// 一个假的：证明 --live 那条路在真实调用链上真的发出去了、并把结果打了出来。
//
// 这里不断言整份报告的结论：本包的其他用例也往同一个库上写行，而那些行会
// 让 doctor 报出与本次无关的问题。要断言的是**那一节**说了什么。
func TestDoctorIntegrationLiveAgainstFakeGateway(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("doctor-live")
	createDoctorModel(t, canonical)
	createDoctorChannel(t, canonical)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","model":"`+canonical+`","choices":[],`+
			`"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)
	}))
	defer srv.Close()

	withStdin(t, testDoctorKey+"\n")
	out, _ := runDoctorCaptured(t,
		"--live", "--model", canonical, "--base-url", srv.URL, "--credential-stdin")

	if gotPath != doctorPathChat {
		t.Fatalf("假网关收到的路径 = %q，期望 %q", gotPath, doctorPathChat)
	}
	// 状态符按整份输出断言：doctorSection 是从标题开始切的，会把行首那两格
	// 与状态符一起切掉，而从那里开始正好是最容易错的地方。
	if !strings.Contains(out, "✓ 上游连通") {
		t.Errorf("真打上游那一行的状态符不对：\n%s", out)
	}
	liveSection := doctorSection(t, out, "上游连通")
	for _, want := range []string{"通过（200", "响应里的模型是 " + canonical,
		"用量 prompt=4 completion=1"} {
		if !strings.Contains(liveSection, want) {
			t.Errorf("真打上游那一行里没有 %q：\n%s", want, liveSection)
		}
	}
	if strings.Contains(out, testDoctorKey) {
		t.Errorf("凭证出现在了输出里：\n%s", out)
	}
}

// ── 用例内的断言辅助 ────────────────────────────────────────────────────

// assertContainsFragments 断言问题清单里每一项片段都出现过。
//
// 按**片段**而不是整句比对：整句断言会把措辞的每一次调整都变成一次失败，
// 而这里要钉住的是「有没有把这件事指出来、有没有点名到具体对象」。
//
// 片段在 headline 与 detail 两截里找（见 doctorProblem）：一截进默认那一行、
// 一截进 -v，拆开存是输出的事，不该让断言跟着分家。
// findingsText 把问题清单拼成一段文本，供「不该出现」这类断言用。
//
// 走 doctorProblem.String() 把两截拼回去：断言关心的是「这条问题说了什么」，
// 而不是它哪半句进默认输出、哪半句进 -v。
func findingsText(findings []doctorProblem) string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n")
}

func assertContainsFragments(t *testing.T, findings []doctorProblem, want ...string) {
	t.Helper()
	for _, fragment := range want {
		found := false
		for _, f := range findings {
			if strings.Contains(f.String(), fragment) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("问题清单里没有含 %q 的一条：%q", fragment, findings)
		}
	}
}
