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

// TestAliasAddRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestAliasAddRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --canonical", []string{"--upstream", "u1"}, "缺少 --canonical"},
		{"没有 --upstream", []string{"--canonical", "m1"}, "缺少 --upstream"},
		{"--channel 是负数", []string{"--canonical", "m1", "--upstream", "u1", "--channel", "-2"}, "无效"},
		{"多出位置参数", []string{"--canonical", "m1", "--upstream", "u1", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runAliasAdd(tc.args) })
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

// TestAliasListRejectsBothModes 钉住 --channel 与 --canonical 互斥。
//
// 两个一起给时，命令没法同时按两种范围列，而「按其中一个悄悄列出来」会让
// 使用者以为看到的是另一种——所以它是参数错，不是「随便挑一个」。
func TestAliasListRejectsBothModes(t *testing.T) {
	err := runCaptured(t, func() error {
		return runAliasList([]string{"--channel", "1", "--canonical", "m1"})
	})
	if err == nil || !strings.Contains(err.Error(), "只能给一个") {
		t.Fatalf("两个都给应当被拒，得到 %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码应当是 %d，得到 %d", exitUsage, got)
	}
}

// TestAliasAddThenList 钉住两种范围各自的清单。
//
// 平台级别名对全部渠道生效，渠道级别名只对那个渠道生效并盖过平台级那条——
// 两种范围混在一条清单里会让人以为渠道级那条对所有渠道都生效，所以
// `alias list` 默认只列平台级，按渠道列要显式给 --channel。
func TestAliasAddThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	platformUpstream := uniqueName("upstream")
	channelUpstream := uniqueName("upstream")

	// 别名要挂在一个已经存在的规范名上，所以先建模型。
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{"--canonical", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})

	// 渠道级别名要挂在一个已经存在的渠道上。
	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelCreate([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", canonical, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID, err := strconv.ParseInt(strings.TrimSpace(channelOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", channelOut, err)
	}

	platformOut, _ := captureStreams(t, func() {
		if addErr := runAliasAdd([]string{
			"--canonical", canonical, "--upstream", platformUpstream,
		}); addErr != nil {
			t.Fatalf("平台级别名 add 失败: %v", addErr)
		}
	})
	platformID, err := strconv.ParseInt(strings.TrimSpace(platformOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是别名 ID：%q（%v）", platformOut, err)
	}

	channelAliasOut, _ := captureStreams(t, func() {
		if addErr := runAliasAdd([]string{
			"--canonical", canonical, "--upstream", channelUpstream,
			"--channel", strconv.FormatInt(channelID, 10),
		}); addErr != nil {
			t.Fatalf("渠道级别名 add 失败: %v", addErr)
		}
	})
	channelAliasID, err := strconv.ParseInt(strings.TrimSpace(channelAliasOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是别名 ID：%q（%v）", channelAliasOut, err)
	}

	// 写进去的两条都在库里，且各自挂在正确的范围上。
	aliases, err := catalog.New(db).Aliases(context.Background(), 0)
	if err != nil {
		t.Fatalf("列举平台级别名失败: %v", err)
	}
	if !hasAlias(aliases, platformID) {
		t.Errorf("平台级别名 id=%d 不在平台级清单里：%+v", platformID, aliases)
	}
	channelAliases, err := catalog.New(db).Aliases(context.Background(), channelID)
	if err != nil {
		t.Fatalf("列举渠道级别名失败: %v", err)
	}
	if !hasAlias(channelAliases, channelAliasID) {
		t.Errorf("渠道级别名 id=%d 不在渠道 %d 的清单里：%+v", channelAliasID, channelID, channelAliases)
	}

	// 默认列平台级：只有平台级那条，渠道级那条不该混进来。
	platformList, _ := captureStreams(t, func() {
		if listErr := runAliasList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	if !strings.Contains(platformList, platformUpstream) {
		t.Errorf("平台级清单里没有 %q：\n%s", platformUpstream, platformList)
	}
	if strings.Contains(platformList, channelUpstream) {
		t.Errorf("平台级清单里混进了渠道级别名：\n%s", platformList)
	}

	// 按渠道列：只有那个渠道那条。
	scopedList, _ := captureStreams(t, func() {
		if listErr := runAliasList([]string{"--channel", strconv.FormatInt(channelID, 10)}); listErr != nil {
			t.Fatalf("list --channel 失败: %v", listErr)
		}
	})
	if !strings.Contains(scopedList, channelUpstream) {
		t.Errorf("渠道 %d 的清单里没有 %q：\n%s", channelID, channelUpstream, scopedList)
	}
	if strings.Contains(scopedList, platformUpstream) {
		t.Errorf("渠道清单里混进了平台级别名：\n%s", scopedList)
	}

	// 按规范名列：两种范围一起出现。
	byCanonical, _ := captureStreams(t, func() {
		if listErr := runAliasList([]string{"--canonical", canonical}); listErr != nil {
			t.Fatalf("list --canonical 失败: %v", listErr)
		}
	})
	for _, want := range []string{platformUpstream, channelUpstream, "平台", "渠道"} {
		if !strings.Contains(byCanonical, want) {
			t.Errorf("按规范名列的清单里没有 %q：\n%s", want, byCanonical)
		}
	}
}

// TestAliasAddRejectsDuplicateCanonicalInScope 钉住「一个范围里一个规范名
// 只能有一个上游名」。
//
// 两个上游名指向同一个规范名时，选路无从取舍——选错就是差价，而账单看上去
// 完全正常。写入路径把它挡在唯一键上，命令这一层要把冲突翻译成退出码 4。
func TestAliasAddRejectsDuplicateCanonicalInScope(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{"--canonical", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})

	add := func(upstream string) error {
		return runCaptured(t, func() error {
			return runAliasAdd([]string{"--canonical", canonical, "--upstream", upstream})
		})
	}
	if err := add(uniqueName("upstream")); err != nil {
		t.Fatalf("第一次 add 应当成功: %v", err)
	}
	err := add(uniqueName("upstream"))
	if err == nil {
		t.Fatal("同一个范围里第二个上游名应当被拒")
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码应当是 %d，得到 %d（%v）", exitConflict, got, err)
	}
}

// TestAliasUpdateRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestAliasUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --id", []string{"--upstream", "u1"}, "缺少 --id"},
		{"没有要改的字段", []string{"--id", "1"}, "至少要改一项"},
		{"多出位置参数", []string{"--id", "1", "--upstream", "u1", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runAliasUpdate(tc.args) })
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

// TestAliasResolveRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestAliasResolveRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --canonical", []string{"--channel", "1"}, "缺少 --canonical"},
		{"--channel 是负数", []string{"--canonical", "m1", "--channel", "-1"}, "无效"},
		{"多出位置参数", []string{"--canonical", "m1", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runAliasResolve(tc.args) })
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

// TestAliasUpdateAndDelete 钉住改一条别名与删一条别名。
//
// 改的是上游名与覆盖这两栏，而渠道范围与规范名**一个字节都不动**：它们是
// 这条别名的身份，改了就是另一条配置（见 catalog.UpdateAliasRequest）。
// 这里顺带把它钉住——读取路径读回来的规范名还是原来那个。
func TestAliasUpdateAndDelete(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{"--canonical", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})
	out, _ := captureStreams(t, func() {
		if err := runAliasAdd([]string{
			"--canonical", canonical, "--upstream", uniqueName("upstream"),
			"--overrides", `{"temperature":0.5}`,
		}); err != nil {
			t.Fatalf("alias add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是别名 ID：%q（%v）", out, err)
	}

	// 换上游名并清空覆盖：两件事一次做完。
	renamed := uniqueName("upstream")
	_, _ = captureStreams(t, func() {
		if updateErr := runAliasUpdate([]string{
			"--id", strconv.FormatInt(id, 10),
			"--upstream", renamed, "--overrides", "",
		}); updateErr != nil {
			t.Fatalf("alias update 失败: %v", updateErr)
		}
	})

	got := findAlias(t, db, 0, id)
	if got.UpstreamModel != renamed {
		t.Errorf("上游模型名 = %q，期望 %q", got.UpstreamModel, renamed)
	}
	if len(got.Overrides) != 0 {
		t.Errorf("覆盖应当被清空，得到 %v", got.Overrides)
	}
	if got.CanonicalName != canonical {
		t.Errorf("规范名 = %q，期望 %q（它不可改）", got.CanonicalName, canonical)
	}

	_, _ = captureStreams(t, func() {
		if deleteErr := runAliasDelete([]string{"--id", strconv.FormatInt(id, 10)}); deleteErr != nil {
			t.Fatalf("alias delete 失败: %v", deleteErr)
		}
	})
	aliases, err := catalog.New(db).Aliases(context.Background(), 0)
	if err != nil {
		t.Fatalf("列举平台级别名失败: %v", err)
	}
	if hasAlias(aliases, id) {
		t.Errorf("删完之后别名 %d 还在清单里", id)
	}

	// 再删一次：手里这个 id 已经不对了，处置是重新列一遍拿到现存的 id。
	err = runCaptured(t, func() error {
		return runAliasDelete([]string{"--id", strconv.FormatInt(id, 10)})
	})
	if !errors.Is(err, catalog.ErrAliasIDNotFound) {
		t.Errorf("重复删除期望 ErrAliasIDNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("重复删除的退出码应当是 %d，得到 %d", exitConflict, got)
	}
}

// TestAliasResolvePrefersChannelScope 钉住 resolve 回答的是选路那个问题。
//
// list 把两种范围各列一遍，不回答「谁赢」；resolve 走的是请求热路径上同一条
// 查询，所以「渠道级别名盖过平台级那条」在这里看得见。stdout 只有那个上游
// 模型名——它是要被直接接走的值，多一句提示语这条链就断了。
func TestAliasResolvePrefersChannelScope(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	platformUpstream := uniqueName("upstream")
	channelUpstream := uniqueName("upstream")

	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{"--canonical", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})
	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelCreate([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", canonical, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID, err := strconv.ParseInt(strings.TrimSpace(channelOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", channelOut, err)
	}

	_, _ = captureStreams(t, func() {
		if err := runAliasAdd([]string{"--canonical", canonical, "--upstream", platformUpstream}); err != nil {
			t.Fatalf("平台级别名 add 失败: %v", err)
		}
	})
	_, _ = captureStreams(t, func() {
		if err := runAliasAdd([]string{
			"--canonical", canonical, "--upstream", channelUpstream,
			"--channel", strconv.FormatInt(channelID, 10),
		}); err != nil {
			t.Fatalf("渠道级别名 add 失败: %v", err)
		}
	})

	stdout, _ := captureStreams(t, func() {
		if err := runAliasResolve([]string{
			"--canonical", canonical, "--channel", strconv.FormatInt(channelID, 10),
		}); err != nil {
			t.Fatalf("resolve 失败: %v", err)
		}
	})
	if got := strings.TrimSpace(stdout); got != channelUpstream {
		t.Errorf("stdout = %q，期望渠道级别名 %q", got, channelUpstream)
	}
	// 恰好一行：那一行是要被直接接走的值，多一行（比如一句提示语）这条链就断了。
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout 应当恰好一行，得到 %q", stdout)
	}

	// 不针对具体渠道：平台级那条兜底。
	stdout, _ = captureStreams(t, func() {
		if err := runAliasResolve([]string{"--canonical", canonical}); err != nil {
			t.Fatalf("resolve 失败: %v", err)
		}
	})
	if got := strings.TrimSpace(stdout); got != platformUpstream {
		t.Errorf("stdout = %q，期望平台级别名 %q", got, platformUpstream)
	}
}

// TestAliasResolveNotConfiguredFallsBackToCanonical 钉住「一条都没配上」这一档。
//
// 别名表表达的是「哪些模型的上游叫法与规范名不同」，一条都没配过的模型不在
// 这个集合里——选路时原样发出，所以 resolve 也应当原样报出规范名。
//
// 这一档**不报错**，是因为报错就是在向使用者描述一条与请求热路径不同的规则：
// 两边各有一套判据时，漂移的表现是「工具说会报错，而线上照常发出去了」。
func TestAliasResolveNotConfiguredFallsBackToCanonical(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	var err error
	stdout, _ := captureStreams(t, func() {
		err = runAliasResolve([]string{"--canonical", canonical})
	})
	if err != nil {
		t.Fatalf("一条别名都没配时应当原样返回规范名，得到 %v", err)
	}
	if got := strings.TrimSpace(stdout); got != canonical {
		t.Errorf("stdout = %q，期望规范名 %q", got, canonical)
	}
	// 恰好一行：那一行是要被直接接走的值。
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout 应当恰好一行，得到 %q", stdout)
	}
}

// TestAliasResolveMissingForThisChannel 钉住「配了一半」这一档的退出码。
//
// 它与「一条都没配」分开：别名是**逐渠道**配的，某个渠道漏了的时候静默回落成
// 规范名，会让它表现为「上游说这个模型不存在」——排查方向指向对方。所以这一档
// 报 ErrAliasNotFound，退出码走「配置缺失」那一档。
//
// 它与「查询失败」也分开：配置缺失的下一步是去配一条，查询失败是数据库的问题。
// 两者都退化成一个笼统的错误时，脚本没法分派处置。
func TestAliasResolveMissingForThisChannel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	_, _ = captureStreams(t, func() {
		if err := runModelAdd([]string{"--canonical", canonical, "--display", "展示名"}); err != nil {
			t.Fatalf("model add 失败: %v", err)
		}
	})
	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelCreate([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", canonical, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID, err := strconv.ParseInt(strings.TrimSpace(channelOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", channelOut, err)
	}
	// 别名只配在这一个渠道上：别的渠道查它时命中不了，但这个规范名确实配过。
	_, _ = captureStreams(t, func() {
		if addErr := runAliasAdd([]string{
			"--canonical", canonical, "--upstream", uniqueName("upstream"),
			"--channel", strconv.FormatInt(channelID, 10),
		}); addErr != nil {
			t.Fatalf("渠道级别名 add 失败: %v", addErr)
		}
	})

	// 不指定渠道 = 只看平台级那条，而平台级没有这一条。
	err = runCaptured(t, func() error {
		return runAliasResolve([]string{"--canonical", canonical})
	})
	if err == nil {
		t.Fatal("配过别名、却没有一条覆盖这个渠道时应当报错")
	}
	if !errors.Is(err, catalog.ErrAliasNotFound) {
		t.Errorf("期望 catalog.ErrAliasNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码应当是 %d，得到 %d", exitConflict, got)
	}
}

// findAlias 按 id 读回一条别名，读不到就直接失败。
func findAlias(t *testing.T, db *sql.DB, channelID, id int64) catalog.Alias {
	t.Helper()
	aliases, err := catalog.New(db).Aliases(context.Background(), channelID)
	if err != nil {
		t.Fatalf("列举渠道 %d 的别名失败: %v", channelID, err)
	}
	for _, a := range aliases {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("渠道 %d 的清单里没有别名 %d", channelID, id)
	return catalog.Alias{}
}

func hasAlias(aliases []catalog.Alias, id int64) bool {
	for _, a := range aliases {
		if a.ID == id {
			return true
		}
	}
	return false
}
