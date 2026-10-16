package main

import (
	"context"
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

func hasAlias(aliases []catalog.Alias, id int64) bool {
	for _, a := range aliases {
		if a.ID == id {
			return true
		}
	}
	return false
}
