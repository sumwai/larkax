package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/catalog"
)

// ── 出站映射搬到 channel map 之后，旧的 alias 路径与它写同一张表、同一行 ────

// aliasConfigRow 是 model_aliases 里一行**配置列**的取值。
//
// 不含 id 与 created_at：前者是自增主键，后者是写入时刻，两条路径各跑一次时
// 这两个数必然不同。除它们之外的每一列都要逐列相同。
type aliasConfigRow struct {
	ChannelID     sql.NullInt64
	UpstreamModel string
	CanonicalName string
	Overrides     string
}

// readAliasConfigRow 按 id 从 model_aliases 读回一行。
func readAliasConfigRow(t *testing.T, db *sql.DB, id int64) aliasConfigRow {
	t.Helper()

	var row aliasConfigRow
	err := db.QueryRowContext(context.Background(), `
		SELECT channel_id, upstream_model, canonical_name, overrides::text
		  FROM model_aliases
		 WHERE id = $1`, id).
		Scan(&row.ChannelID, &row.UpstreamModel, &row.CanonicalName, &row.Overrides)
	if err != nil {
		t.Fatalf("读回映射 %d 失败: %v", id, err)
	}
	return row
}

// TestChannelMapAddWritesSameRowAsAliasCreate 逐列对拍新旧两条写入路径。
//
// 判据是「同一份输入，两条命令在 model_aliases 里各写一行，除自增 id 与写入
// 时刻之外的每一列都相同」。同一份输入意味着同一个规范名，而唯一索引不允许
// 同一个范围里两条并存，所以顺序是：新命令写 → 读回 → 新命令删 → 旧命令写 →
// 读回 → 比较。删掉那一步用的是新命令的 remove，顺带把它也走到。
//
// 两个范围都覆盖：带 <渠道> 的渠道级，以及 --all-channels 的平台级。旧路径
// 表达平台级的方式是省略 --channel，两边的 channel_id 都必须落在 NULL 上。
func TestChannelMapAddWritesSameRowAsAliasCreate(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	// 渠道的 models 白名单要一个真的规范名；它对拍用的那几个与它无关。
	channelModel := uniqueName("model")
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", channelModel, "--display", "展示名"})
	}); err != nil {
		t.Fatalf("model create 失败: %v", err)
	}

	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", channelModel, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID := idFromStdout(t, "渠道", channelOut)
	channelArg := strconv.FormatInt(channelID, 10)

	cases := []struct {
		name string
		// newScope 是 channel map 的范围写法，oldScope 是 alias create 的。
		newScope  []string
		oldScope  []string
		overrides string
	}{
		{
			name:      "渠道级",
			newScope:  []string{channelArg},
			oldScope:  []string{"--channel", channelArg},
			overrides: `{"temperature":0.5}`,
		},
		{
			name:     "平台级",
			newScope: []string{"--all-channels"},
			oldScope: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每条子用例用自己的规范名：一个范围里一个规范名只能有一条，
			// 而两条子用例都是「先写一行、删掉、再写一行」——共用一个名字时
			// 前一条失败会留下状态，后一条看到的是它的后果。
			canonical := uniqueName("model")
			if err := runCaptured(t, func() error {
				return runModelCreate([]string{"--model", canonical, "--display", "展示名"})
			}); err != nil {
				t.Fatalf("model create 失败: %v", err)
			}
			upstream := uniqueName("upstream")

			// 新路径写一行。
			newArgs := append([]string{"add"}, tc.newScope...)
			newArgs = append(newArgs, canonical, upstream)
			if tc.overrides != "" {
				newArgs = append(newArgs, "--overrides", tc.overrides)
			}
			newOut, newErrOut := captureStreams(t, func() {
				if err := runChannelMap(newArgs); err != nil {
					t.Fatalf("channel map add 失败: %v", err)
				}
			})
			if !strings.Contains(newErrOut, "已加入映射") {
				t.Errorf("add 的确认信息没有落在 stderr 上：%q", newErrOut)
			}
			newID := idFromStdout(t, "新映射", newOut)
			newRow := readAliasConfigRow(t, db, newID)

			// 用新命令删掉，下一轮旧命令才能用同一份输入再写一次。
			removeArgs := append([]string{"remove"}, tc.newScope...)
			removeArgs = append(removeArgs, canonical)
			if err := runCaptured(t, func() error { return runChannelMap(removeArgs) }); err != nil {
				t.Fatalf("channel map remove 失败: %v", err)
			}
			if rowExists(t, db, newID) {
				t.Fatalf("remove 之后映射 %d 还在库里", newID)
			}

			// 旧路径用同一份输入写一行。
			oldArgs := append([]string{"create", "--model", canonical, "--upstream", upstream}, tc.oldScope...)
			if tc.overrides != "" {
				oldArgs = append(oldArgs, "--overrides", tc.overrides)
			}
			oldOut, oldErrOut := captureStreams(t, func() {
				if err := runAlias(oldArgs); err != nil {
					t.Fatalf("alias create 失败: %v", err)
				}
			})
			if !strings.Contains(oldErrOut, "channel map") {
				t.Errorf("旧路径没有在 stderr 上指向新路径：%q", oldErrOut)
			}
			oldID := idFromStdout(t, "旧别名", oldOut)
			oldRow := readAliasConfigRow(t, db, oldID)

			if newRow != oldRow {
				t.Errorf("两条路径写出的行不同：\nchannel map add  %+v\nalias create     %+v",
					newRow, oldRow)
			}
		})
	}
}

// TestChannelMapListScopes 钉住 list 的三种范围各列什么。
func TestChannelMapListScopes(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", canonical, "--display", "展示名"})
	}); err != nil {
		t.Fatalf("model create 失败: %v", err)
	}

	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", canonical, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID := idFromStdout(t, "渠道", channelOut)
	channelArg := strconv.FormatInt(channelID, 10)

	platformUpstream := uniqueName("upstream")
	channelUpstream := uniqueName("upstream")
	for _, args := range [][]string{
		{"add", "--all-channels", canonical, platformUpstream},
		{"add", channelArg, canonical, channelUpstream},
	} {
		if err := runCaptured(t, func() error { return runChannelMap(args) }); err != nil {
			t.Fatalf("channel map add %v 失败: %v", args, err)
		}
	}

	list := func(args ...string) string {
		t.Helper()
		var err error
		stdout, _ := captureStreams(t, func() { err = runChannelMap(append([]string{"list"}, args...)) })
		if err != nil {
			t.Fatalf("channel map list %v 失败: %v", args, err)
		}
		return stdout
	}

	platformOnly := list()
	if !strings.Contains(platformOnly, platformUpstream) {
		t.Errorf("不带参数的 list 里没有平台级映射 %q：\n%s", platformUpstream, platformOnly)
	}
	if strings.Contains(platformOnly, channelUpstream) {
		t.Errorf("不带参数的 list 混进了渠道级映射：\n%s", platformOnly)
	}

	scoped := list(channelArg)
	if !strings.Contains(scoped, channelUpstream) {
		t.Errorf("渠道 %d 的清单里没有 %q：\n%s", channelID, channelUpstream, scoped)
	}
	if strings.Contains(scoped, platformUpstream) {
		t.Errorf("渠道清单里混进了平台级映射：\n%s", scoped)
	}

	byModel := list("--model", canonical)
	for _, want := range []string{platformUpstream, channelUpstream, "平台", "渠道"} {
		if !strings.Contains(byModel, want) {
			t.Errorf("--model 的清单里没有 %q：\n%s", want, byModel)
		}
	}

	// 平台级的显式写法与不带参数是同一份清单。
	if explicit := list("--all-channels"); explicit != platformOnly {
		t.Errorf("--all-channels 与不带参数的清单不同：\n%q\n%q", explicit, platformOnly)
	}
}

// TestChannelMapRemoveBothScopes 钉住 remove 按「范围 + 规范名」删。
func TestChannelMapRemoveBothScopes(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", canonical, "--display", "展示名"})
	}); err != nil {
		t.Fatalf("model create 失败: %v", err)
	}

	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", canonical, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID := idFromStdout(t, "渠道", channelOut)
	channelArg := strconv.FormatInt(channelID, 10)

	add := func(args ...string) int64 {
		t.Helper()
		var stdout string
		var err error
		stdout, _ = captureStreams(t, func() { err = runChannelMap(append([]string{"add"}, args...)) })
		if err != nil {
			t.Fatalf("channel map add %v 失败: %v", args, err)
		}
		return idFromStdout(t, "映射", stdout)
	}
	platformID := add("--all-channels", canonical, uniqueName("upstream"))
	channelIDOfRow := add(channelArg, canonical, uniqueName("upstream"))

	remove := func(args ...string) error {
		t.Helper()
		return runCaptured(t, func() error { return runChannelMap(append([]string{"remove"}, args...)) })
	}

	if err := remove(channelArg, canonical); err != nil {
		t.Fatalf("删除渠道级映射失败: %v", err)
	}
	if rowExists(t, db, channelIDOfRow) {
		t.Errorf("渠道级映射 %d 还在库里", channelIDOfRow)
	}
	if !rowExists(t, db, platformID) {
		t.Errorf("删渠道级映射时把平台级 %d 一起删了", platformID)
	}

	// 再删一次：那一行已经不在，处置是退出码 4。
	err := remove(channelArg, canonical)
	if !errors.Is(err, catalog.ErrAliasNotFound) {
		t.Errorf("重复删除期望 ErrAliasNotFound，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("重复删除的退出码应当是 %d，得到 %d", cli.ExitConflict, got)
	}

	if err := remove("--all-channels", canonical); err != nil {
		t.Fatalf("删除平台级映射失败: %v", err)
	}
	if rowExists(t, db, platformID) {
		t.Errorf("平台级映射 %d 还在库里", platformID)
	}
}

// rowExists 判断 model_aliases 里还有没有这一行。
func rowExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()

	var left int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM model_aliases WHERE id = $1`, id).Scan(&left); err != nil {
		t.Fatalf("确认删除结果失败: %v", err)
	}
	return left > 0
}

// TestChannelMapRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestChannelMapRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		// run 为空时走 channel map；unmap 是 channel 上的另一条动词，单独给。
		run  func([]string) error
		args []string
		want string
	}{
		{"map 没有参数", nil, nil, "需要渠道"},
		{"动词写法缺规范名", nil, []string{"3"}, "缺少 <规范名>"},
		{"动词写法渠道不是正整数", nil, []string{"0", "m1", "u1"}, "无效：渠道 ID 从 1 开始"},
		{"动词写法多出参数", nil, []string{"3", "m1", "u1", "extra"}, "map 不接受多余的参数"},
		// 保留词只判第一个位置参数：这里的 list 是规范名，走的是动词那条路。
		{"第二个位置参数的 list 不是子命令", nil, []string{"3", "list", "u1", "extra"},
			"map 不接受多余的参数"},
		// 平台级的自映射不改变任何翻译结果，所以这一档要上游名，不做直通缺省。
		{"动词写法 --all 缺上游名", nil, []string{"--all", "m1"}, "要写 <上游模型名>"},
		{"add 没有参数", nil, []string{"add"}, "缺少 <渠道>"},
		{"add 缺规范名", nil, []string{"add", "3"}, "缺少 <规范名>"},
		{"add 的 --all 多给了一个渠道", nil, []string{"add", "--all", "3", "m1", "u1"},
			"只给两个位置参数"},
		{"add 的 --all-channels 多给了一个渠道", nil, []string{"add", "--all-channels", "3", "m1", "u1"},
			"只给两个位置参数"},
		// 兼容别名与动词同一条解析，所以这一档同样要上游名。
		{"add 的 --all 缺上游名", nil, []string{"add", "--all", "m1"}, "要写 <上游模型名>"},
		{"add 的渠道不是正整数", nil, []string{"add", "0", "m1", "u1"}, "无效：渠道 ID 从 1 开始"},
		{"add 的 --all 带了取值", nil, []string{"add", "--all=false", "m1", "u1"}, "不带取值"},
		{"add 的 --all-channels 带了取值", nil, []string{"add", "--all-channels=false", "m1", "u1"},
			"不带取值"},
		{"list 同时给了渠道与 --all", nil, []string{"list", "3", "--all"}, "只能给一个"},
		{"list 同时给了渠道与 --all-channels", nil, []string{"list", "3", "--all-channels"}, "只能给一个"},
		{"list 同时给了 --model 与渠道", nil, []string{"list", "3", "--model", "m1"}, "只能给一个"},
		{"list 同时给了 --model 与 --all", nil, []string{"list", "--model", "m1", "--all"}, "只能给一个"},
		{"remove 缺规范名", nil, []string{"remove", "3"}, "缺少 <规范名>"},
		{"remove 的 --all 缺规范名", nil, []string{"remove", "--all"}, "缺少 <规范名>"},
		{"remove 多出参数", nil, []string{"remove", "3", "m1", "extra"}, "不接受多余的参数"},
		{"unmap 缺渠道", runChannelUnmap, nil, "缺少 <渠道>"},
		{"unmap 缺规范名", runChannelUnmap, []string{"3"}, "缺少 <规范名>"},
		{"unmap 多出参数", runChannelUnmap, []string{"3", "m1", "extra"}, "unmap 不接受多余的参数"},
		// 同理：unmap 第二个位置参数上的 list 是规范名。
		{"unmap 第二个位置参数的 list 不是子命令", runChannelUnmap, []string{"3", "list", "extra"},
			"unmap 不接受多余的参数"},
		{"unmap 与 --all 同时给了渠道", runChannelUnmap, []string{"--all", "m1", "c1"},
			"unmap 不接受多余的参数"},
		{"unmap 的渠道不是正整数", runChannelUnmap, []string{"0", "m1"}, "无效：渠道 ID 从 1 开始"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			if run == nil {
				run = runChannelMap
			}
			err := runCaptured(t, func() error { return run(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestLegacyAliasPathsPointToChannelMap 钉住旧路径仍在、且点名新路径。
//
// 判据来自 issue：旧命令仍可用；`alias -h` 与 `alias resolve` 的输出里点名
// 新命令。成功与失败都提示由 runLegacyUserPath 决定（参数错不打），这里两条
// 都走一遍。
func TestLegacyAliasPathsPointToChannelMap(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", canonical, "--display", "展示名"})
	}); err != nil {
		t.Fatalf("model create 失败: %v", err)
	}
	upstream := uniqueName("upstream")

	createOut, createErr := captureStreams(t, func() {
		if err := runAlias([]string{"create", "--model", canonical, "--upstream", upstream}); err != nil {
			t.Fatalf("alias create 失败: %v", err)
		}
	})
	if !strings.Contains(createErr, "larkaxctl channel map") {
		t.Errorf("alias create 的 stderr 没有点名新命令：%q", createErr)
	}
	id := idFromStdout(t, "别名", createOut)

	// alias add 是同一条旧路径的另一个名字，同样要提示。
	otherCanonical := uniqueName("model")
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", otherCanonical, "--display", "展示名"})
	}); err != nil {
		t.Fatalf("model create 失败: %v", err)
	}
	_, addErr := captureStreams(t, func() {
		if err := runAlias([]string{"add", "--model", otherCanonical, "--upstream", uniqueName("upstream")}); err != nil {
			t.Fatalf("alias add 失败: %v", err)
		}
	})
	if !strings.Contains(addErr, "larkaxctl channel map") {
		t.Errorf("alias add 的 stderr 没有点名新命令：%q", addErr)
	}

	_, listErr := captureStreams(t, func() {
		if err := runAlias([]string{"list"}); err != nil {
			t.Fatalf("alias list 失败: %v", err)
		}
	})
	if !strings.Contains(listErr, "larkaxctl channel map") {
		t.Errorf("alias list 的 stderr 没有点名新命令：%q", listErr)
	}

	// resolve 留在原处。配置去向那句只在 -v 下打：它在每次成功运行里都会出现，
	// 按 streams_test.go 的判据不属于默认输出（那种话属于 -v 或 --help）。
	const wantPointer = "出站映射的配置命令在 larkaxctl channel map（map / unmap / list）。"

	resolve := func(t *testing.T) (string, string) {
		t.Helper()
		var err error
		stdout, stderr := captureStreams(t, func() {
			err = runAlias([]string{"resolve", "--model", canonical})
		})
		if err != nil {
			t.Fatalf("alias resolve 失败: %v", err)
		}
		return stdout, stderr
	}

	// 默认档：stdout 仍是上游名，stderr 不出现配置去向。
	resolveOut, resolveErr := resolve(t)
	if strings.TrimSpace(resolveOut) != upstream {
		t.Errorf("resolve 的 stdout = %q，期望 %q", resolveOut, upstream)
	}
	if strings.Contains(resolveErr, "larkaxctl channel map") {
		t.Errorf("默认档不该点名新路径：%q", resolveErr)
	}

	// -v：多打那一句，按行逐字相等（不只看包含关系）。
	savedVerbose := cli.VerboseOutput
	cli.VerboseOutput = true
	verboseOut, verboseErr := resolve(t)
	cli.VerboseOutput = savedVerbose
	if strings.TrimSpace(verboseOut) != upstream {
		t.Errorf("resolve -v 的 stdout = %q，期望 %q", verboseOut, upstream)
	}
	pointerLines := 0
	for _, line := range strings.Split(verboseErr, "\n") {
		if line == wantPointer {
			pointerLines++
		}
	}
	if pointerLines != 1 {
		t.Errorf("-v 的 stderr 里那句出现了 %d 次，期望恰好 1 次：\n%s",
			pointerLines, verboseErr)
	}

	// -q：默认与 -v 两种都不打（人读那一档整档丢弃）。
	savedQuiet := cli.QuietOutput
	cli.QuietOutput = true
	_, quietErr := resolve(t)
	cli.VerboseOutput = true
	_, quietVerboseErr := resolve(t)
	cli.VerboseOutput = savedVerbose
	cli.QuietOutput = savedQuiet
	for name, stderr := range map[string]string{
		"默认": quietErr,
		"-v": quietVerboseErr,
	} {
		if stderr != "" {
			t.Errorf("-q（%s）下 stderr 应当是空的，得到 %q", name, stderr)
		}
	}

	// --format json：stdout 是原来那份信封，逐字节不变；stderr 不出现这句。
	savedFormat := cli.OutputFormat
	cli.OutputFormat = cli.FormatJSON
	jsonOut, jsonErr := resolve(t)
	cli.OutputFormat = savedFormat
	wantJSON := "{\n" +
		"  \"canonical\": \"" + canonical + "\",\n" +
		"  \"channel_id\": null,\n" +
		"  \"exit_code\": 0,\n" +
		"  \"status\": \"ok\",\n" +
		"  \"upstream_model\": \"" + upstream + "\"\n" +
		"}\n"
	if jsonOut != wantJSON {
		t.Errorf("--format json 的 stdout 变了：\n得到 %q\n期望 %q", jsonOut, wantJSON)
	}
	if strings.Contains(jsonErr, wantPointer) {
		t.Errorf("--format json 的 stderr 不该出现配置去向：%q", jsonErr)
	}

	_, deleteErr := captureStreams(t, func() {
		if err := runAlias([]string{"delete", "--id", strconv.FormatInt(id, 10)}); err != nil {
			t.Fatalf("alias delete 失败: %v", err)
		}
	})
	if !strings.Contains(deleteErr, "larkaxctl channel unmap") {
		t.Errorf("alias delete 的 stderr 没有点名新命令：%q", deleteErr)
	}

	// 失败（非参数错）也要提示：判据与 runLegacyUserPath 相同，这里用一个
	// 一定不存在的 id，不能碰巧命中同包别的用例留下的行。
	missingID := strconv.FormatInt(1<<62, 10)
	var missingDeleteErr error
	_, missingStderr := captureStreams(t, func() {
		missingDeleteErr = runAlias([]string{"delete", "--id", missingID})
	})
	if missingDeleteErr == nil {
		t.Fatal("删除不存在的别名应当报错")
	}
	if got := cli.ExitCode(missingDeleteErr); got != cli.ExitConflict {
		t.Errorf("删除不存在的别名的退出码应当是 %d，得到 %d（%v）",
			cli.ExitConflict, got, missingDeleteErr)
	}
	if !strings.Contains(missingStderr, "larkaxctl channel unmap") {
		t.Errorf("失败路径没有点名新命令：%q", missingStderr)
	}

	// 参数错（退出码 2）不打：那一屏已经是用法的位置。
	_, usageStderr := captureStreams(t, func() {
		parseErr := runAlias([]string{"create", "--model", canonical})
		if parseErr == nil {
			t.Fatal("缺少 --upstream 应当被拒")
		}
		if got := cli.ExitCode(parseErr); got != cli.ExitUsage {
			t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
		}
	})
	if strings.Contains(usageStderr, "larkaxctl channel map") {
		t.Errorf("参数错时不该打搬家提示：%q", usageStderr)
	}

	// 帮助页也要点名。
	helpOut, _ := captureStreams(t, func() {
		if err := runAlias([]string{"-h"}); err != nil && !cli.HelpRequested(err) {
			t.Fatalf("alias -h 失败: %v", err)
		}
	})
	if !strings.Contains(helpOut, "larkaxctl channel map") {
		t.Errorf("alias -h 没有点名新命令：\n%s", helpOut)
	}
	var longHelp string
	var err error
	longHelp, _ = captureStreams(t, func() { err = dispatch([]string{"help", "alias"}) })
	if err != nil {
		t.Fatalf("help alias 失败: %v", err)
	}
	if !strings.Contains(longHelp, "larkaxctl channel map") {
		t.Errorf("alias 长版帮助没有点名新命令：\n%s", longHelp)
	}
}

// TestChannelMapAddRejectsControlChars 钉住上游模型名在连库之前就被检查。
func TestChannelMapAddRejectsControlChars(t *testing.T) {
	err := runCaptured(t, func() error {
		return runChannelMap([]string{"add", "3", "m1", "u\x01p"})
	})
	if err == nil {
		t.Fatal("上游模型名里的控制字符应当被拒")
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("退出码应当是 %d，得到 %d（%v）", cli.ExitUsage, got, err)
	}
}

// ── 映射即上架：渠道级 map 与渠道白名单落在同一个事务里 ──────────────────

// createMapChannel 建一条映射用例用的渠道，返回它的 ID。
//
// 名字由调用方给：按名字解析的用例要能自己拼一个已知的名字。models 是建渠道时
// 的白名单——用例要分清「本来就有的」与「map 加进去的」，起点就不能是空的。
func createMapChannel(t *testing.T, name string, models ...string) int64 {
	t.Helper()

	withStdin(t, testSecret)
	out, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", name, "--type", testChanType,
			"--base-url", testBaseURL, "--models", strings.Join(models, ","),
			"--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	return idFromStdout(t, "渠道", out)
}

// createMapModel 把规范名建进模型目录。
//
// model_aliases.canonical_name 有指向 model_catalog 的外键：规范名不在目录里时
// 建映射会被「先把模型建进 model_catalog」拒掉。渠道白名单那一步也要读目录。
func createMapModel(t *testing.T, canonical string) {
	t.Helper()

	if err := runCaptured(t, func() error {
		return runModelCreate([]string{"--model", canonical, "--display", "展示名"})
	}); err != nil {
		t.Fatalf("model create %q 失败: %v", canonical, err)
	}
}

// mapByVerb 走动词写法写一条渠道级映射，返回新映射的 ID。
func mapByVerb(t *testing.T, channelID int64, canonical string, upstream ...string) int64 {
	t.Helper()

	args := append([]string{strconv.FormatInt(channelID, 10), canonical}, upstream...)
	var stdout string
	var err error
	stdout, _ = captureStreams(t, func() { err = runChannelMap(args) })
	if err != nil {
		t.Fatalf("channel map %v 失败: %v", args, err)
	}
	return idFromStdout(t, "新映射", stdout)
}

// TestChannelMapOnboardsIntoWhitelist 钉住「映射即上架」：渠道白名单里原本没有
// 这个规范名，map 之后它在里面，且映射行也落了库。
func TestChannelMapOnboardsIntoWhitelist(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	seed := uniqueName("seed-model")
	channelID := createMapChannel(t, uniqueName("map-channel"), seed)
	canonical := uniqueName("map-canonical")
	createMapModel(t, canonical)
	upstream := uniqueName("upstream")

	id := mapByVerb(t, channelID, canonical, upstream)

	// 逐字断言整个白名单：种子在前、map 加进去的在后，不多不少。
	wantModels := []string{seed, canonical}
	if got := channelModels(t, db, channelID); !slices.Equal(got, wantModels) {
		t.Fatalf("map 之后渠道 %d 的 models = %v，期望 %v", channelID, got, wantModels)
	}
	row := readAliasConfigRow(t, db, id)
	if !row.ChannelID.Valid || row.ChannelID.Int64 != channelID {
		t.Fatalf("映射 %d 的渠道 = %v，期望 %d", id, row.ChannelID, channelID)
	}
	if row.CanonicalName != canonical || row.UpstreamModel != upstream {
		t.Fatalf("映射 %d = %+v，期望 %s ← %s", id, row, canonical, upstream)
	}
}

// TestChannelMapWhitelistIsIdempotent 钉住幂等的那一半：白名单里已经有这个规范名
// 时不重复写，命令照样成功，映射照常建立。
func TestChannelMapWhitelistIsIdempotent(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("map-canonical")
	createMapModel(t, canonical)
	channelID := createMapChannel(t, uniqueName("map-channel"), canonical)

	id := mapByVerb(t, channelID, canonical, uniqueName("upstream"))

	if got := channelModels(t, db, channelID); !slices.Equal(got, []string{canonical}) {
		t.Fatalf("重复的规范名被写进了白名单：%v", got)
	}
	if !rowExists(t, db, id) {
		t.Fatalf("白名单已有这个名字时映射 %d 没落库", id)
	}
}

// TestChannelUnmapKeepsWhitelist 钉住「unmap 只删映射，不删白名单条目」。
func TestChannelUnmapKeepsWhitelist(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	seed := uniqueName("seed-model")
	channelID := createMapChannel(t, uniqueName("map-channel"), seed)
	canonical := uniqueName("map-canonical")
	createMapModel(t, canonical)
	id := mapByVerb(t, channelID, canonical, uniqueName("upstream"))

	if err := runCaptured(t, func() error {
		return runChannelUnmap([]string{strconv.FormatInt(channelID, 10), canonical})
	}); err != nil {
		t.Fatalf("channel unmap 失败: %v", err)
	}

	if rowExists(t, db, id) {
		t.Fatalf("unmap 之后映射 %d 还在库里", id)
	}
	wantModels := []string{seed, canonical}
	if got := channelModels(t, db, channelID); !slices.Equal(got, wantModels) {
		t.Fatalf("unmap 改动了白名单：%v，期望 %v", got, wantModels)
	}
}

// TestChannelMapDefaultsUpstreamToCanonical 钉住「上游名缺省 = 规范名」，并断言
// 直通形态与显式写一个同名上游名落库的行逐字相同。
func TestChannelMapDefaultsUpstreamToCanonical(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	channelID := createMapChannel(t, uniqueName("map-channel"), uniqueName("seed-model"))
	canonical := uniqueName("map-canonical")
	createMapModel(t, canonical)

	implicitID := mapByVerb(t, channelID, canonical)
	implicit := readAliasConfigRow(t, db, implicitID)
	if implicit.UpstreamModel != canonical {
		t.Fatalf("省略上游名时落库的上游名是 %q，期望规范名 %q", implicit.UpstreamModel, canonical)
	}

	// 删掉再显式写同一个名字，两行除自增 id 与写入时刻之外的每一列都要相同。
	if err := runCaptured(t, func() error {
		return runChannelUnmap([]string{strconv.FormatInt(channelID, 10), canonical})
	}); err != nil {
		t.Fatalf("channel unmap 失败: %v", err)
	}
	explicit := readAliasConfigRow(t, db, mapByVerb(t, channelID, canonical, canonical))

	if implicit != explicit {
		t.Fatalf("直通形态与显式同名的行不同：\n直通 %+v\n显式 %+v", implicit, explicit)
	}
}

// TestChannelMapAllLeavesWhitelist 钉住「全局映射只做翻译，不进任何渠道白名单」。
func TestChannelMapAllLeavesWhitelist(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	seed := uniqueName("seed-model")
	channelID := createMapChannel(t, uniqueName("map-channel"), seed)
	canonical := uniqueName("map-canonical")
	createMapModel(t, canonical)
	upstream := uniqueName("upstream")

	var stdout string
	var err error
	stdout, _ = captureStreams(t, func() {
		err = runChannelMap([]string{"--all", canonical, upstream})
	})
	if err != nil {
		t.Fatalf("channel map --all 失败: %v", err)
	}
	id := idFromStdout(t, "平台级映射", stdout)

	if got := channelModels(t, db, channelID); !slices.Equal(got, []string{seed}) {
		t.Fatalf("全局映射改动了渠道白名单：%v", got)
	}
	row := readAliasConfigRow(t, db, id)
	if row.ChannelID.Valid {
		t.Fatalf("全局映射的 channel_id = %v，期望 NULL", row.ChannelID)
	}
	if row.CanonicalName != canonical || row.UpstreamModel != upstream {
		t.Fatalf("全局映射 %d = %+v，期望 %s ← %s", id, row, canonical, upstream)
	}
}

// TestChannelMapRollsBackWhenWhitelistRejected 让白名单写入被端点评据拒掉，断言
// 映射表里也没有这一行——两条写入在同一个事务里，失败是整笔回滚。
//
// 判据落在库上而不是退出码上：把事务拆成两笔独立写入时，命令照样失败（白名单
// 那一步照样报错），只有查表才看得见映射已经先落了库。
func TestChannelMapRollsBackWhenWhitelistRejected(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	// 目录里声明只支持 /v1/responses，而这条渠道是 openai（打 /v1/chat/completions）。
	canonical := uniqueName("map-wrong-endpoint")
	if err := runCaptured(t, func() error {
		return runModelCreate([]string{
			"--model", canonical, "--display", "展示名", "--endpoints", "/v1/responses",
		})
	}); err != nil {
		t.Fatalf("model create 失败: %v", err)
	}

	seed := uniqueName("seed-model")
	channelID := createMapChannel(t, uniqueName("map-channel"), seed)

	var mapErr error
	_, _ = captureStreams(t, func() {
		mapErr = runChannelMap([]string{strconv.FormatInt(channelID, 10), canonical, uniqueName("upstream")})
	})
	if mapErr == nil {
		t.Fatal("端点对不上的模型被写进了白名单，命令却成功了")
	}
	// 先确认失败发生在端点评据上：换个原因失败，这条用例证明的就不是回滚。
	if !strings.Contains(mapErr.Error(), "在目录里声明的端点是") {
		t.Fatalf("失败原因不是端点评据拒绝的：%v", mapErr)
	}

	aliases, aliasErr := catalog.New(db).Aliases(context.Background(), channelID)
	if aliasErr != nil {
		t.Fatalf("读映射失败: %v", aliasErr)
	}
	if len(aliases) != 0 {
		t.Fatalf("白名单写入失败后映射还在库里：%+v", aliases)
	}
	if got := channelModels(t, db, channelID); !slices.Equal(got, []string{seed}) {
		t.Fatalf("失败的 map 改动了白名单：%v", got)
	}
}

// TestChannelMapResolvesChannelByName 钉住渠道操作数认名字，以及三种落空：
// 没有这条、只有已软删的一条、同名多条（报错里带可执行候选）。
func TestChannelMapResolvesChannelByName(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("map-named")
	channelID := createMapChannel(t, name, uniqueName("seed-model"))
	canonical := uniqueName("map-canonical")
	createMapModel(t, canonical)
	upstream := uniqueName("upstream")

	// 正面：按名字写，映射挂到那条渠道上，白名单也跟着写进去。
	var stdout string
	var err error
	stdout, _ = captureStreams(t, func() {
		err = runChannelMap([]string{name, canonical, upstream})
	})
	if err != nil {
		t.Fatalf("按名字 map 失败: %v", err)
	}
	id := idFromStdout(t, "新映射", stdout)
	row := readAliasConfigRow(t, db, id)
	if !row.ChannelID.Valid || row.ChannelID.Int64 != channelID {
		t.Fatalf("按名字 map 落到了渠道 %v，期望 %d", row.ChannelID, channelID)
	}
	if got := channelModels(t, db, channelID); !slices.Contains(got, canonical) {
		t.Fatalf("按名字 map 没有把规范名写进白名单：%v", got)
	}

	// list 认名字：同一份解析，列出来的就是这条渠道的映射。
	listOut, _ := captureStreams(t, func() {
		err = runChannelMap([]string{"list", name})
	})
	if err != nil {
		t.Fatalf("按名字 list 失败: %v", err)
	}
	if !strings.Contains(listOut, upstream) {
		t.Fatalf("按名字 list 的清单里没有 %q：\n%s", upstream, listOut)
	}

	// 落空一：没有这个名字。
	missing := uniqueName("no-such-channel")
	missingErr := runCaptured(t, func() error {
		return runChannelMap([]string{missing, canonical, upstream})
	})
	if missingErr == nil {
		t.Fatal("不存在的渠道名应当被拒")
	}
	if !strings.Contains(missingErr.Error(), "没有叫") ||
		!strings.Contains(missingErr.Error(), "未删除渠道") {
		t.Fatalf("找不到渠道时的报错里没有点名：%v", missingErr)
	}

	// 落空二：只命中一条已软删的渠道——说「没有未删除的这条」，不说「有多条」。
	deletedName := uniqueName("map-deleted")
	deletedID := createMapChannel(t, deletedName, uniqueName("seed-model"))
	if err := runCaptured(t, func() error {
		return runChannelDelete([]string{"--id", strconv.FormatInt(deletedID, 10)})
	}); err != nil {
		t.Fatalf("软删渠道失败: %v", err)
	}
	deletedErr := runCaptured(t, func() error {
		return runChannelMap([]string{deletedName, canonical, upstream})
	})
	if deletedErr == nil {
		t.Fatal("已软删渠道的名字应当被拒")
	}
	if !strings.Contains(deletedErr.Error(), "没有叫") ||
		strings.Contains(deletedErr.Error(), "条渠道叫") {
		t.Fatalf("只命中已删渠道时该说「没有未删除的这条」，得到：%v", deletedErr)
	}

	// 落空三：同名两条。判据是报错里两个候选都带 id，人能直接抄回去用。
	dupName := uniqueName("map-dup")
	firstID := createMapChannel(t, dupName, uniqueName("seed-model"))
	secondID := createMapChannel(t, dupName, uniqueName("seed-model"))
	dupErr := runCaptured(t, func() error {
		return runChannelMap([]string{dupName, canonical, upstream})
	})
	if dupErr == nil {
		t.Fatal("同名渠道应当要求改用 ID")
	}
	for _, want := range []string{
		"有 2 条渠道叫",
		"id=" + strconv.FormatInt(firstID, 10),
		"id=" + strconv.FormatInt(secondID, 10),
	} {
		if !strings.Contains(dupErr.Error(), want) {
			t.Fatalf("同名报错里没有 %q：%v", want, dupErr)
		}
	}
}
