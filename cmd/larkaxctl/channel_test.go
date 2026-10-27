package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
)

// 用例里用的假上游地址与假凭证。真实的域名与凭证绝不进这个仓库。
const (
	testBaseURL  = "https://api.example.com/v1"
	testSecret   = "<CHANNEL_CREDENTIAL>"
	testChanType = "openai"
)

// TestChannelCreateRejectsBadFlags 走的是**不碰数据库**的那几条路径。
//
// 参数检查必须发生在连库之前：一个写错的命令不该先连上生产库再报错。
// 这些用例也因此在没有 DSN 的机器上也能跑——它们一旦开始碰库，就会在
// 本地通过、在 CI 上跳过，而「跳过」在输出里和「通过」长得一样。
func TestChannelCreateRejectsBadFlags(t *testing.T) {
	base := []string{
		"--name", "c1", "--type", testChanType, "--base-url", testBaseURL,
		"--models", "m1", "--credential-stdin",
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --name", without(base, "--name"), "缺少 --name"},
		// 没给凭证时协议必须点名：给了凭证才交给探测定（见 probeProtocolsForAdd）。
		{"没有 --type 也没有凭证", without(without(base, "--type"), "--credential-stdin"), "缺少 --type"},
		{"没有 --base-url", without(base, "--base-url"), "缺少 --base-url"},
		// --models 现在可以不给（空白名单是合法中间态），但给了就不能是空的。
		{"--models 给空串", []string{
			"--name", "c1", "--type", testChanType, "--base-url", testBaseURL,
			"--models", "", "--credential-stdin",
		}, "--models 是空的"},
		{"两个凭证来源都给", append(base, "--credential-env", "SOME_VAR"), "只能给一个"},
		{"多出位置参数", append(base, "extra"), "不接受多余的参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runChannelAdd(tc.args) })
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

// TestChannelUpdateRejectsBadFlags 钉住 update 的两条参数判据。
func TestChannelUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --id", []string{"--priority", "3"}, "缺少 --id"},
		{"没有要改的字段", []string{"--id", "1"}, "至少要改一项"},
		{"多出位置参数", []string{"--id", "1", "--priority", "3", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runChannelUpdate(tc.args) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望报错且含 %q，得到 %v", tc.want, err)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestChannelBaseURLQueryOrFragmentIsRejected 钉住「带 query / fragment 的地址在
// 配置期就被拒」，create 与 update 两条写入路径都要。
//
// 为什么必须拒：拼接点是字符串拼接，端点路径会被接到 query 后面（`#` 之后的部分
// 干脆不发给上游），而它是**安静的**坏——配置写错的人要等到请求失败、或者请求
// 成功却打到了别处，才知道。
//
// 它跑在连库之前的那一层，于是三条判据同时成立：退出码是参数错（2）、报错里点出
// 是哪一部分不合法、而且这条用例在没有 DSN 的机器上也真的跑（碰库的用例在本地是
// 跳过的，而「跳过」在输出里和「通过」长得一样）。
func TestChannelBaseURLQueryOrFragmentIsRejected(t *testing.T) {
	rejected := []struct {
		name string
		raw  string
		want string
	}{
		{"query", "https://api.example.com/v1?tenant=abc", "不能带 query"},
		{"空的 query", "https://api.example.com/v1?", "不能带 query"},
		{"fragment", "https://api.example.com/v1#frag", "不能带 fragment"},
		{"空的 fragment", "https://api.example.com/v1#", "不能带 fragment"},
	}

	for _, tc := range rejected {
		t.Run("create/"+tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error {
				return runChannelAdd([]string{
					"--name", "c1", "--type", testChanType, "--base-url", tc.raw,
					"--models", "m1", "--credential-stdin",
				})
			})
			assertUsageRefusal(t, err, tc.raw, tc.want)
		})
		t.Run("update/"+tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error {
				return runChannelUpdate([]string{"--id", "1", "--base-url", tc.raw})
			})
			assertUsageRefusal(t, err, tc.raw, tc.want)
		})
	}
}

// TestChannelBaseURLNormalFormsParseThrough 是上面那条的护栏：正常的写法一条都不许
// 被拒（含路径、含端口、结尾带不带斜杠都算）。没有这一臂的话，「一律拒掉」也能让
// 上面那条通过。
//
// 判据落在**解析函数**上而不是整条命令上：解析之后命令就去连库了，而「连不上库」
// 的报错会把它盖过去——那样这条用例证明的只是「本机没有库」。
func TestChannelBaseURLNormalFormsParseThrough(t *testing.T) {
	// 凭证走环境变量而不是标准输入：标准输入是个一次性的读口，这条用例里
	// 每一次 parse 都要读一遍。
	const envName = "LARKAX_TEST_CHANNEL_CREDENTIAL"
	t.Setenv(envName, testSecret)

	accepted := []string{
		"https://api.example.com/v1",
		"https://api.example.com/v1/",         // 结尾带斜杠
		"https://api.example.com",             // 没有路径
		"https://api.example.com:8443/v1",     // 带端口
		"http://127.0.0.1:8080",               // http、本机地址
		"https://api.example.com/v1/tenant-a", // 路径里有连字符
	}
	for _, raw := range accepted {
		createReq, _, err := parseChannelCreate([]string{
			"--name", "c1", "--type", testChanType, "--base-url", raw,
			"--models", "m1", "--credential-env", envName,
		})
		if err != nil {
			t.Errorf("create 不该拒 %q: %v", raw, err)
		} else if createReq.BaseURL != raw {
			t.Errorf("create 的 base_url = %q，期望 %q", createReq.BaseURL, raw)
		}

		updateReq, err := parseChannelUpdate([]string{"--id", "1", "--base-url", raw})
		if err != nil {
			t.Errorf("update 不该拒 %q: %v", raw, err)
			continue
		}
		if updateReq.BaseURL == nil || *updateReq.BaseURL != raw {
			t.Errorf("update 的 base_url = %v，期望 %q", updateReq.BaseURL, raw)
		}
	}
}

// assertUsageRefusal 断言一条命令以「参数错」被拒，且报错里点出了是哪一段。
func assertUsageRefusal(t *testing.T, err error, raw, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%q 应当被拒", raw)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("错误 %q 里没有点出 %q", err.Error(), want)
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
	}
}

// TestChannelCredentialIsNotACommandLineArgument 钉住「凭证不进 argv」。
//
// 命令行参数会出现在 ps 的输出与 shell 历史里，所以这条命令**没有**一条从
// 参数取凭证的路径。这里从两个方向证明它：
//
//  1. 给一个 --credential <值> 会被当成未知标志拒掉——没有任何标志能承载
//     凭证的值，这是结构上的事实，不是一句约定；
//  2. 那条错误信息里不会出现凭证本身（否则它还是被打印到了终端与日志里）。
func TestChannelCredentialIsNotACommandLineArgument(t *testing.T) {
	args := []string{
		"create", "--name", "c1", "--type", testChanType, "--base-url", testBaseURL,
		"--models", "m1", "--credential", testSecret,
	}

	err := runCaptured(t, func() error { return runChannel(args) })
	if err == nil {
		t.Fatal("--credential 不该被接受：凭证只能从标准输入或环境变量读")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Errorf("期望「未知标志」这类错误，得到 %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("错误信息里出现了凭证本身：%v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("未知标志是参数错，退出码应当是 %d，得到 %d", cli.ExitUsage, got)
	}
}

// TestChannelCredentialSourcesRejectEmpty 钉住两个来源各自「读到空」的情形。
//
// 空凭证写进库是一条谁都调不通的渠道，而它的表现是「请求全都失败」，
// 离「当初那把 key 没传进来」已经很远。所以两个来源都要当场拒绝。
func TestChannelCredentialSourcesRejectEmpty(t *testing.T) {
	withStdin(t, "\n")
	err := runCaptured(t, func() error {
		return runChannelAdd([]string{
			"--name", "c1", "--type", testChanType, "--base-url", testBaseURL,
			"--models", "m1", "--credential-stdin",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "标准输入里没有凭证") {
		t.Fatalf("空的标准输入应当被拒，得到 %v", err)
	}

	err = runCaptured(t, func() error {
		return runChannelAdd([]string{
			"--name", "c1", "--type", testChanType, "--base-url", testBaseURL,
			"--models", "m1", "--credential-env", "LARKAX_TEST_UNSET_CREDENTIAL",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "未设置或为空") {
		t.Fatalf("空的环境变量应当被拒，得到 %v", err)
	}
}

// TestChannelCreateListUpdateRoundTrip 是这条命令的主干用例。
//
// 它一次钉住四件事：create 写进去的值读回来对得上、凭证真的被主密钥封装了、
// update 改到的字段生效、list 里没有密文。
func TestChannelCreateListUpdateRoundTrip(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("channel")
	model := uniqueName("model")

	// 末尾带一个换行：`echo "$SECRET" | ...` 会多带一个，而写进库的凭证
	// 不该比运维手里那把多一个字节。这条断言就是那个「去掉末尾换行」。
	withStdin(t, testSecret+"\n")

	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", name, "--type", testChanType, "--base-url", testBaseURL,
			"--models", model, "--credential-stdin", "--priority", "5",
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	info := findChannel(t, db, id)
	if info.Name != name {
		t.Errorf("渠道名 = %q，期望 %q", info.Name, name)
	}
	if info.BaseURL != testBaseURL {
		t.Errorf("上游地址 = %q，期望 %q", info.BaseURL, testBaseURL)
	}
	if len(info.Models) != 1 || info.Models[0] != model {
		t.Errorf("模型清单 = %v，期望 [%q]", info.Models, model)
	}
	if info.Priority != 5 {
		t.Errorf("优先级 = %d，期望 5", info.Priority)
	}
	if info.Status != channels.StatusEnabled {
		t.Errorf("新建的渠道应当是 enabled，得到 %q", info.Status)
	}
	if len(info.CredentialKeys) != 1 || info.CredentialKeys[0] != "test" {
		t.Errorf("凭证应当挂在主密钥 test 下，得到 %v（%v）", info.CredentialKeys, info.CredentialErr)
	}

	// 密文能被同一把钥匙解开，而且解出来的就是那把凭证：这一条才真正证明
	// 「从标准输入读到的字符串」与「库里那一行」是同一个东西。
	plaintext, err := testSealer(t).Open(channelEnvelope(t, db, id))
	if err != nil {
		t.Fatalf("解不开刚写进去的凭证: %v", err)
	}
	if string(plaintext) != testSecret {
		t.Errorf("解出来的凭证 = %q，期望 %q", string(plaintext), testSecret)
	}

	// update 只改提到的字段：优先级与名字变了，而**凭证一个字节都不动**
	//（没给 --credential-* 时写入路径的 SQL 里根本不出现 credential_enc）。
	before := channelEnvelope(t, db, id)
	renamed := name + "-renamed"
	_, _ = captureStreams(t, func() {
		if updateErr := runChannelUpdate([]string{
			"--id", strconv.FormatInt(id, 10), "--priority", "9", "--name", renamed,
		}); updateErr != nil {
			t.Fatalf("update 失败: %v", updateErr)
		}
	})

	info = findChannel(t, db, id)
	if info.Priority != 9 {
		t.Errorf("改完之后优先级 = %d，期望 9", info.Priority)
	}
	if info.Name != renamed {
		t.Errorf("改完之后渠道名 = %q，期望 %q", info.Name, renamed)
	}
	if after := channelEnvelope(t, db, id); string(after) != string(before) {
		t.Error("没给凭证的 update 不该重新封装密文：重封一次会换掉数据密钥，" +
			"「这条渠道的凭证什么时候换过」就再也答不出来了")
	}

	// list 的输出里有这条渠道与它挂在哪把钥匙下，但没有明文凭证。
	listOut, _ := captureStreams(t, func() {
		if listErr := runChannelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	for _, want := range []string{renamed, testBaseURL, model, "test"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("清单里没有 %q：\n%s", want, listOut)
		}
	}
	if strings.Contains(listOut, testSecret) {
		t.Errorf("清单里出现了明文凭证：\n%s", listOut)
	}
}

// TestChannelCredentialFromEnv 钉住环境变量这条来源。
func TestChannelCredentialFromEnv(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	const envName = "LARKAX_TEST_CHANNEL_CREDENTIAL"
	t.Setenv(envName, testSecret)

	name := uniqueName("channel")
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", name, "--type", testChanType, "--base-url", testBaseURL,
			"--models", uniqueName("model"), "--credential-env", envName,
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}

	plaintext, err := testSealer(t).Open(channelEnvelope(t, db, id))
	if err != nil {
		t.Fatalf("解不开凭证: %v", err)
	}
	if string(plaintext) != testSecret {
		t.Errorf("解出来的凭证 = %q，期望 %q", string(plaintext), testSecret)
	}
}

// TestChannelEnableDisable 钉住启停。
//
// 停用之后的渠道不出现在候选集里（读取路径只取 enabled 的行），所以这一对
// 命令的落点是 status 那一列——用例直接读那一列，而不是看命令打了什么。
func TestChannelEnableDisable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	withStdin(t, testSecret)
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", uniqueName("model"),
			"--credential-stdin",
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}
	arg := strconv.FormatInt(id, 10)

	_, _ = captureStreams(t, func() {
		if disableErr := runChannel([]string{"disable", "--id", arg}); disableErr != nil {
			t.Fatalf("disable 失败: %v", disableErr)
		}
	})
	if got := findChannel(t, db, id).Status; got != channels.StatusDisabled {
		t.Errorf("停用之后状态 = %q，期望 %q", got, channels.StatusDisabled)
	}

	_, _ = captureStreams(t, func() {
		if enableErr := runChannel([]string{"enable", "--id", arg}); enableErr != nil {
			t.Fatalf("enable 失败: %v", enableErr)
		}
	})
	if got := findChannel(t, db, id).Status; got != channels.StatusEnabled {
		t.Errorf("启用之后状态 = %q，期望 %q", got, channels.StatusEnabled)
	}

	// 一个不存在的渠道要报「不存在」，而不是安静地什么都不做。
	err = runCaptured(t, func() error {
		return runChannel([]string{"enable", "--id", "999999999"})
	})
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Fatalf("启用一个不存在的渠道应当报 ErrChannelNotFound，得到 %v", err)
	}
}

// TestChannelCreateRejectsUnknownType 钉住「类型必须在适配器注册表里」。
//
// 库里存一个没人实现的类型，表现是这条渠道永远选不中——一条看起来配好了、
// 实际不可用的渠道。所以它必须被写入路径拒掉，而命令这一层要做的是把注册表
// 交给它（漏了的话每个 create 都会报「没有适配器注册表」）。
func TestChannelCreateRejectsUnknownType(t *testing.T) {
	testDB(t)
	setupEnv(t)
	withStdin(t, testSecret)

	err := runCaptured(t, func() error {
		return runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", "not-a-real-adapter",
			"--base-url", testBaseURL, "--models", "m1", "--credential-stdin",
		})
	})
	if err == nil {
		t.Fatal("未知的渠道类型应当被拒")
	}
	if !strings.Contains(err.Error(), "渠道类型无效") {
		t.Fatalf("错误信息应当是「渠道类型无效」，得到 %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("错误信息里出现了凭证本身：%v", err)
	}
}

// findChannel 从清单里取出指定渠道。
func findChannel(t *testing.T, db *sql.DB, id int64) channels.ChannelInfo {
	t.Helper()
	infos, err := channels.NewAdmin(db, nil, nil).List(context.Background())
	if err != nil {
		t.Fatalf("列举渠道失败: %v", err)
	}
	for _, info := range infos {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("清单里没有渠道 %d", id)
	return channels.ChannelInfo{}
}

// channelEnvelope 直接读出渠道主账号的凭证密文。
//
// 清单里没有这一栏（也不该有），所以只能自己查——用例要证明的正是
// 「密文能被解开」，而不是「清单里没打印密文」。凭证在账号上（见 00067），
// 主账号是账号 id 最小的那一个，与选路同一条判据。
func channelEnvelope(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	var envelope []byte
	err := db.QueryRowContext(context.Background(), `
		SELECT a.credential_enc
		  FROM channel_accounts ca
		  JOIN accounts a ON a.id = ca.account_id
		 WHERE ca.channel_id = $1
		 ORDER BY a.id
		 LIMIT 1`, id).Scan(&envelope)
	if err != nil {
		t.Fatalf("读渠道 %d 的密文失败: %v", id, err)
	}
	return envelope
}

// without 去掉 args 里 name 及其后面的一个取值。
//
// 直接写一份「少给一个标志」的参数列表也行，但那会让被删掉的那一对藏在一串
// 参数里看不出来，而用例的名字说的正是它。
func without(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			i++ // 跳过它的值
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// TestChannelSettingPrintsUnreadNote 检查「写进去的键没有读者时，提示真的打出来了」。
//
// 这是 #360 在管理面上的落点：setting 写得到、读得到，而没有东西按它做判断。
// 从命令行看，「配了但不生效」与「配了就生效」长得一模一样——两边都成功、
// 清单里都有那一行。所以不生效这件事必须在**写入的那一刻**说出来。
//
// 五条判据，缺一条这句话都会以某种方式失效：
//
//  1. 给了 --setting 且键没有读者时提示出现（create 与 update 两条写入路径都要）；
//  2. 它走 stderr——stdout 上那一行（create 是 ID）要能被脚本直接接走；
//  3. 没碰 setting 也不碰 type 的 update 不打，否则它很快变成背景音；
//  4. list 把这样的行点出来——否则事后核对配置时完全看不出来；
//  5. 内层键同样点名，且配置照原样落库——它写错时命令返回 0、输出与写对时
//     一样，这句话是唯一能看见的信号。
//
// 「有没有读者」按渠道类型判（见 TestChannelSettingNoteJudgesByType），所以
// 这里刻意用 openai 渠道配一个它不认识的键：这条用例不依赖某个键今天恰好
// 是死是活。
func TestChannelSettingPrintsUnreadNote(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	const unknownKey = "supports_optional_param"
	if note := channel.SettingUnreadNote(testChanType, map[string]any{unknownKey: true}); note == "" {
		t.Fatalf("%s 对 %s 居然有读者了：换一个没有读者的键再跑这条用例", unknownKey, testChanType)
	}

	// 一、create 给了 --setting。
	id, stdout, stderr := createChannelWithSetting(t, testChanType, `{"supports_optional_param":true}`)
	if !strings.Contains(stderr, "注意：") || !strings.Contains(stderr, "「"+unknownKey+"」") {
		t.Errorf("create 没有点出没有读者的键：\n%s", stderr)
	}
	if strings.Contains(stdout, "注意：") {
		t.Errorf("提示混进了 stdout，那条链就断了：\n%s", stdout)
	}

	// 写进去的键仍然照原样落库：提示说的是「没有读者」，不是「不存」。
	idArg := strconv.FormatInt(id, 10)
	if got := findChannel(t, db, id).Setting; got[unknownKey] != true {
		t.Errorf("setting = %v，期望原样落库", got)
	}

	// 二、update 给了 --setting。
	_, stderr = captureStreams(t, func() {
		if updateErr := runChannelUpdate([]string{
			"--id", idArg, "--setting", `{"supports_optional_param":false}`,
		}); updateErr != nil {
			t.Fatalf("update 失败: %v", updateErr)
		}
	})
	if !strings.Contains(stderr, "「"+unknownKey+"」") {
		t.Errorf("update 没有点出没有读者的键：\n%s", stderr)
	}

	// 三、两样都没碰的 update 不打。
	_, stderr = captureStreams(t, func() {
		if updateErr := runChannelUpdate([]string{"--id", idArg, "--priority", "7"}); updateErr != nil {
			t.Fatalf("update 失败: %v", updateErr)
		}
	})
	if strings.Contains(stderr, "注意：") {
		t.Errorf("没碰 setting 也没碰 type 的那次 update 不该打这句：\n%s", stderr)
	}

	// 四、list 点出这条渠道。清单里有别的用例建的渠道，所以判据是「这条渠道
	// 的 ID 出现在那句话里」，不是整份输出相等。
	listOut, _ := captureStreams(t, func() {
		if listErr := runChannelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	if !strings.Contains(listOut, "配了 setting") || !strings.Contains(listOut, idArg) {
		t.Errorf("list 没有点出渠道 %s 配了 setting：\n%s", idArg, listOut)
	}
	if !strings.Contains(listOut, "「"+unknownKey+"」") {
		t.Errorf("list 只说了这条渠道配过 setting，没点名是哪个键没有读者：\n%s", listOut)
	}

	// 五、内层键同样要在写入成功之后被点出来，而配置照原样落库——提示说的
	// 是「没有读者」，不是「不存」。内层键写错时，命令返回 0、输出与写对时
	// 没有区别，所以这句话是唯一能看见的信号。
	innerID, _, innerStderr := createChannelWithSetting(t, "openai",
		`{"usage":{"completion_include_reasoning":false}}`)
	const innerUnknown = "usage.completion_include_reasoning"
	if !strings.Contains(innerStderr, "「"+innerUnknown+"」") {
		t.Errorf("create 没有点出拼错的内层键：\n%s", innerStderr)
	}
	block, ok := findChannel(t, db, innerID).Setting["usage"].(map[string]any)
	if !ok || block["completion_include_reasoning"] != false {
		t.Errorf("内层配置没有原样落库：%v", findChannel(t, db, innerID).Setting)
	}
}

// TestChannelListNamesUnreadInnerSettingKey 检查清单对**内层**键的记号。
//
// 单开一条用例是为了让红证落在这一格上：写入路径与清单用的是同一句话
// （见 channel.SettingUnreadNote），但那句话被内层覆盖之前，清单这一侧
// 只会说出顶层的键——一条内层键拼错的渠道在库里、在清单上都与一条配对了
// 的一模一样。
//
// 两个方向各建一条渠道、各自断言：拼错的内层键要点名，写对的内层键不能被
// 误报。只钉正向的话，「把所有内层键都当成没声明」的实现在清单这一侧照样
// 绿，而写对的内层键被点名比不说更坏——读的人会照着把生效的配置删掉。
func TestChannelListNamesUnreadInnerSettingKey(t *testing.T) {
	testDB(t)
	setupEnv(t)

	badID, _, _ := createChannelWithSetting(t, "openai",
		`{"usage":{"completion_include_reasoning":false}}`)
	goodID, _, _ := createChannelWithSetting(t, "openai",
		`{"usage":{"completion_includes_reasoning":false}}`)
	badArg := strconv.FormatInt(badID, 10)
	goodArg := strconv.FormatInt(goodID, 10)
	listOut, _ := captureStreams(t, func() {
		if listErr := runChannelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	// 正向：拼错的那条渠道被点出来，点名的是点号路径。
	const badKey = "usage.completion_include_reasoning"
	if !strings.Contains(listOut, "渠道 "+badArg+" 配了 setting") {
		t.Errorf("list 没有点出内层键拼错的渠道 %s：\n%s", badArg, listOut)
	}
	if !strings.Contains(listOut, "「"+badKey+"」") {
		t.Errorf("list 没有点名拼错的内层键 %s：\n%s", badKey, listOut)
	}
	// 反向：写对的那条渠道不该有记号，整份清单里也不该出现它那个键名。
	// 名字只差一个 s，所以这两句不会互相顶替（见 badKey）。
	const goodKey = "usage.completion_includes_reasoning"
	if strings.Contains(listOut, "渠道 "+goodArg+" 配了 setting") {
		t.Errorf("写对的内层键被误报成没有读者（渠道 %s）：\n%s", goodArg, listOut)
	}
	if strings.Contains(listOut, "「"+goodKey+"」") {
		t.Errorf("清单里对合法的内层键 %s 打了点名：\n%s", goodKey, listOut)
	}
}

// TestChannelSettingNoteJudgesByType 检查判据落在这条渠道的 type 上。
//
// 先前它是一个全局布尔：只要有适配器读过 setting，提示就对所有渠道消失。
// 于是为一条渠道打开提示等于替所有渠道关掉它——下面这几种配置因此全都
// 静默成功：别的 type 的键、拼错的键、将来才有的键。
func TestChannelSettingNoteJudgesByType(t *testing.T) {
	testDB(t)
	setupEnv(t)

	const knownKey = "reasoning_effort_map"
	const knownSetting = `{"reasoning_effort_map":{"none":"off"}}`

	cases := []struct {
		name     string
		typ      string
		setting  string
		wantKeys []string // 提示里必须逐个点名的键；空表示不该有提示
	}{
		{
			name:    "openai 认的键",
			typ:     "openai",
			setting: knownSetting,
		},
		{
			// 第二个读者：两个适配器共用同一个翻译函数。对一条已经生效的
			// 配置说「没有读者」，比不说更坏——使用者会照着把它删掉。
			name:    "openai-responses 也认这个键",
			typ:     "openai-responses",
			setting: knownSetting,
		},
		{
			name:     "别的 type 的键",
			typ:      "anthropic",
			setting:  knownSetting,
			wantKeys: []string{knownKey},
		},
		{
			name:     "拼错的键名",
			typ:      "openai",
			setting:  `{"reasoning_effortmap":{"none":"off"}}`,
			wantKeys: []string{"reasoning_effortmap"},
		},
		{
			name:     "多个键里有一个不认识",
			typ:      "openai",
			setting:  `{"reasoning_effort_map":{"none":"off"},"future_key":true}`,
			wantKeys: []string{"future_key"},
		},
		{
			// 内层键写对时零提示：这句话在生效的配置上也冒出来，用户很快
			// 就会学会无视它。
			name:    "usage 的内层键写对",
			typ:     "openai",
			setting: `{"usage":{"completion_includes_reasoning":false}}`,
		},
		{
			// 少一个 s：命令照样成功，但要当场点名，点的是点号路径。
			name:     "usage 的内层键拼错",
			typ:      "openai",
			setting:  `{"usage":{"completion_include_reasoning":false}}`,
			wantKeys: []string{"usage.completion_include_reasoning"},
		},
		{
			// 值的形状没有判据：整块不是对象时按「没声明」处理，不新增
			// 报错，也不点名。
			name:    "usage 整块不是对象",
			typ:     "openai",
			setting: `{"usage":"completion_includes_reasoning=false"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, stderr := createChannelWithSetting(t, tc.typ, tc.setting)
			if len(tc.wantKeys) == 0 {
				if strings.Contains(stderr, "注意：") {
					t.Fatalf("这个键属于该 type，提示却出现了：\n%s", stderr)
				}
				return
			}
			for _, key := range tc.wantKeys {
				if !strings.Contains(stderr, "「"+key+"」") {
					t.Fatalf("提示没有点名没有读者的键 %s：\n%s", key, stderr)
				}
			}
			// 被认的键不能也进名单：那会让使用者以为生效的那部分同样没生效。
			if !slices.Contains(tc.wantKeys, knownKey) && strings.Contains(stderr, "「"+knownKey+"」") {
				t.Errorf("提示把有读者的键 %s 也点成了没有读者：\n%s", knownKey, stderr)
			}
		})
	}

	// 只改 type 也要重判：库里的键对旧类型有读者，对新类型未必有。
	id, _, stderr := createChannelWithSetting(t, "openai", knownSetting)
	if strings.Contains(stderr, "注意：") {
		t.Fatalf("openai 渠道配 reasoning_effort_map 不该有提示：\n%s", stderr)
	}
	idArg := strconv.FormatInt(id, 10)
	_, stderr = captureStreams(t, func() {
		if updateErr := runChannelUpdate([]string{"--id", idArg, "--type", "anthropic"}); updateErr != nil {
			t.Fatalf("改 type 失败: %v", updateErr)
		}
	})
	if !strings.Contains(stderr, "「"+knownKey+"」") {
		t.Errorf("改成 anthropic 之后，库里的键没有读者，却没有任何提示：\n%s", stderr)
	}
	_, stderr = captureStreams(t, func() {
		if updateErr := runChannelUpdate([]string{"--id", idArg, "--type", "openai"}); updateErr != nil {
			t.Fatalf("改回 type 失败: %v", updateErr)
		}
	})
	if strings.Contains(stderr, "注意：") {
		t.Errorf("改回 openai 之后这个键又有读者了，提示不该再打：\n%s", stderr)
	}
}

// createChannelWithSetting 建一条带 setting 的渠道，返回它的 ID 与两条流。
//
// 名字与模型名都现取，用例共用一个库。
func createChannelWithSetting(t *testing.T, typ, settingJSON string) (id int64, stdout, stderr string) {
	t.Helper()
	withStdin(t, testSecret)
	stdout, stderr = captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", typ,
			"--base-url", testBaseURL, "--models", uniqueName("model"),
			"--credential-stdin", "--setting", settingJSON,
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})
	parsed, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 应当只有渠道 ID，得到 %q（%v）", stdout, err)
	}
	return parsed, stdout, stderr
}

// ── 软删 ────────────────────────────────────────────────────────────────

// containsID 判断清单里有没有**这个 id 那一行**。
//
// 不能直接用 strings.Contains：清单里还有别的用例建的渠道，而删除时刻
// （RFC3339）里的数字、别的渠道的 id 都可能碰巧包含这个子串——一个只在
// 累积了一定量数据之后才红的断言，比没有更坏。
func containsID(listOut string, id int64) bool {
	want := strconv.FormatInt(id, 10)
	for _, line := range strings.Split(listOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == want {
			return true
		}
	}
	return false
}

// deletedNoteNamesID 判断「渠道 3、7 已删除：…」那一行里点了这个 id。
//
// 与 containsID 同一条理由：用例共用一个库，同一张清单里可能不止一条已删的
// 渠道，所以那一行给出的是一串 id。判据是「这条 id 在名单里」，不是「名单里
// 只有它」——后者只在别处还没删过渠道时才成立，换一次执行顺序就红。
//
// 那一行整个不在时也返回 false：提示没了与没点到这一条，都是这里要看的事。
func deletedNoteNamesID(listOut string, id int64) bool {
	want := strconv.FormatInt(id, 10)
	for _, line := range strings.Split(listOut, "\n") {
		at := strings.Index(line, " 已删除：它们不再参与选路")
		if at < 0 {
			continue
		}
		named, ok := strings.CutPrefix(line[:at], "渠道 ")
		if !ok {
			return false
		}
		for _, got := range strings.Split(named, "、") {
			if got == want {
				return true
			}
		}
	}
	return false
}

// createTestChannel 用命令行建一条渠道，返回它的 ID。
func createTestChannel(t *testing.T, name, model string) int64 {
	t.Helper()
	withStdin(t, testSecret)
	stdout, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", name, "--type", testChanType, "--base-url", testBaseURL,
			"--models", model, "--credential-stdin",
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", stdout, err)
	}
	return id
}

// TestChannelDeleteSoftDeletes 钉住 delete 的四条契约：
//
//  1. 删掉之后 list 不含它，list --all 含它且「已删」那栏有值；
//  2. 再删一次不报错，打印「已经是删除状态」，退出码 0；
//  3. 删一个不存在的 id 报错，退出码走「要改的那一行不存在」；
//  4. 行还在库里（软删不是 DELETE），密文也还在。
func TestChannelDeleteSoftDeletes(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := createTestChannel(t, uniqueName("channel"), uniqueName("model"))
	idArg := strconv.FormatInt(id, 10)

	// 回显走 stderr（stdout 只放能被脚本接走的值，而 delete 没有新值要给出去）。
	out, note := captureStreams(t, func() {
		if err := runChannelDelete([]string{"--id", idArg}); err != nil {
			t.Fatalf("delete 失败: %v", err)
		}
	})
	if !strings.Contains(note, "已删除渠道 "+idArg) {
		t.Errorf("delete 的回显不对：\n%s", note)
	}
	if strings.Contains(out, "已删除渠道") {
		t.Errorf("回显不该混进 stdout：\n%s", out)
	}

	// 一、默认清单不含它，--all 含它并标出删除时刻。
	listOut, _ := captureStreams(t, func() {
		if err := runChannelList(nil); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	if containsID(listOut, id) {
		t.Errorf("默认清单里出现了已删的渠道 %d：\n%s", id, listOut)
	}
	if strings.Contains(listOut, "已删") {
		t.Errorf("默认清单不该有「已删」那一栏：\n%s", listOut)
	}

	allOut, _ := captureStreams(t, func() {
		if err := runChannelList([]string{"--all"}); err != nil {
			t.Fatalf("list --all 失败: %v", err)
		}
	})
	if !strings.Contains(allOut, "已删") {
		t.Errorf("list --all 缺少「已删」那一栏：\n%s", allOut)
	}
	if !containsID(allOut, id) {
		t.Errorf("list --all 里没有已删的渠道 %d：\n%s", id, allOut)
	}
	if !deletedNoteNamesID(allOut, id) {
		t.Errorf("list --all 没有点出 %s 已删除：\n%s", idArg, allOut)
	}

	// 这一行的「已删」栏必须是时刻，不是空：删除时刻是事后与历史账单对上的
	// 唯一线索。
	var deletedAt sql.NullTime
	if err := db.QueryRowContext(context.Background(),
		`SELECT deleted_at FROM channels WHERE id = $1`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("读回 deleted_at 失败: %v", err)
	}
	if !deletedAt.Valid {
		t.Fatal("deleted_at 是 NULL：软删没有落库")
	}
	if !strings.Contains(allOut, deletedAt.Time.UTC().Format(time.RFC3339)) {
		t.Errorf("list --all 的「已删」栏里没有删除时刻 %s：\n%s",
			deletedAt.Time.UTC().Format(time.RFC3339), allOut)
	}

	// 四、行与密文都还在。
	if len(channelEnvelope(t, db, id)) == 0 {
		t.Error("删除不该清掉凭证密文：历史账单还要能解释自己")
	}

	// 二、再删一次是幂等的。
	repeatOut, repeatNote := captureStreams(t, func() {
		if err := runChannelDelete([]string{"--id", idArg}); err != nil {
			t.Fatalf("再删一次不该报错（幂等），得到 %v", err)
		}
	})
	// 这一句是确认，不是值：走 stderr（见 docs/cli-output.md 的「提示与警告」）。
	if !strings.Contains(repeatNote, "已经是删除状态") {
		t.Errorf("重复删除的输出不对：\nstdout:\n%s\nstderr:\n%s", repeatOut, repeatNote)
	}
	if strings.Contains(repeatOut, "已经是删除状态") {
		t.Errorf("重复删除的确认混进了 stdout：\n%s", repeatOut)
	}

	// 三、不存在的 id 仍然报错，退出码走「要改的那一行不存在」。
	err := runCaptured(t, func() error {
		return runChannelDelete([]string{"--id", "999999999"})
	})
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Fatalf("删不存在的 id 应当报 ErrChannelNotFound，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("删不存在的 id 的退出码 = %d，期望 %d", got, cli.ExitConflict)
	}
}

// TestChannelWriteCommandsRejectDeleted 钉住命令行这一层对已删渠道的拒绝：
// update / enable / disable 都要报 ErrChannelDeleted，而不是静默改一行。
func TestChannelWriteCommandsRejectDeleted(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := createTestChannel(t, uniqueName("channel"), uniqueName("model"))
	idArg := strconv.FormatInt(id, 10)
	if err := runCaptured(t, func() error {
		return runChannelDelete([]string{"--id", idArg})
	}); err != nil {
		t.Fatalf("delete 失败: %v", err)
	}

	var before time.Time
	if err := db.QueryRowContext(context.Background(),
		`SELECT updated_at FROM channels WHERE id = $1`, id).Scan(&before); err != nil {
		t.Fatalf("读回 updated_at 失败: %v", err)
	}

	cases := map[string][]string{
		"update":  {"--id", idArg, "--priority", "9"},
		"enable":  {"--id", idArg},
		"disable": {"--id", idArg},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var err error
			_, _ = captureStreams(t, func() {
				switch name {
				case "update":
					err = runChannelUpdate(args)
				case "enable":
					err = runChannelSetStatus(args, channels.StatusEnabled)
				case "disable":
					err = runChannelSetStatus(args, channels.StatusDisabled)
				}
			})
			if !errors.Is(err, channels.ErrChannelDeleted) {
				t.Fatalf("err = %v，期望 ErrChannelDeleted", err)
			}
		})
	}

	var after time.Time
	if err := db.QueryRowContext(context.Background(),
		`SELECT updated_at FROM channels WHERE id = $1`, id).Scan(&after); err != nil {
		t.Fatalf("读回 updated_at 失败: %v", err)
	}
	if !after.Equal(before) {
		t.Errorf("已删渠道的 updated_at 从 %s 变成了 %s：有写路径改动了它", before, after)
	}
}

// ── 机器可读输出（--format json）─────────────────────────────────────────
//
// 下面几个是这一组命令（channel / model / alias / price 四个文件）的用例
// 共用的助手，放在这一组的第一个文件里。withFormat 由主控的 format_test.go
// 提供，这里不再定义一份。

// jsonRoundTrip 把一份文档过一遍 json.Marshal → json.Unmarshal。
//
// 走一遍编码而不是直接断言那份 map：混进一个编不出来的值（chan、函数）、
// 一个非法的键，都只有在这一步才现形，而机器读的输出里这些是最不能有的。
func jsonRoundTrip(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("文档编不成 JSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("文档编出来的不是合法 JSON: %v\n%s", err, raw)
	}
	return got
}

// assertJSONField 断言解出来之后某个键的取值。
//
// want 用的是 json.Unmarshal 之后的形态：数字是 float64、null 是 nil、
// 数组是 []any、对象是 map[string]any。形态写错本身也是线索——它说明这个值
// 在 JSON 里不是我以为什么类型（`"weight": "3"` 与 `"weight": 3` 在这里现形）。
//
// 要 null 的键**必须存在**：`"expires_at": null` 与根本没有这一栏是两件事。
func assertJSONField(t *testing.T, doc map[string]any, key string, want any) {
	t.Helper()
	got, ok := doc[key]
	if !ok {
		t.Errorf("文档里没有 %q（要 null 也得有这个键）：%v", key, doc)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v（%T），期望 %#v（%T）", key, got, got, want, want)
	}
}

// jsonEntries 取一个数组字段，并断言它确实是数组。
//
// `null` 解出来是 nil 接口，过不了这里的类型断言——所以这一句同时钉住
// 「空清单要给 []，不是 null」。
func jsonEntries(t *testing.T, doc map[string]any, key string) []any {
	t.Helper()
	raw, ok := doc[key]
	if !ok {
		t.Fatalf("文档里没有 %q：%v", key, doc)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("%q 应当是数组，拿到 %T（%#v）——空清单要给 []，不是 null", key, raw, raw)
	}
	return list
}

// jsonObjectField 取一个对象字段。
func jsonObjectField(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := doc[key]
	if !ok {
		t.Fatalf("文档里没有 %q：%v", key, doc)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%q 应当是对象，拿到 %T（%#v）", key, raw, raw)
	}
	return obj
}

// assertJSONStdout 断言一段 stdout 整体是一份合法的 JSON，且不含终端转义。
//
// 这是 `--format json` 那一档的核心契约：stdout 只给管道下游，混进一行提示
// 或一个着色转义就会让 `| jq` 报错，而报错的是 jq，看起来像 jq 的问题。
func assertJSONStdout(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if strings.ContainsRune(stdout, cli.EscByte) {
		t.Errorf("stdout 里有 ANSI 转义（0x%x）:\n%s", cli.EscByte, stdout)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout 不是一份合法的 JSON：%v\n%s", err, stdout)
	}
	return got
}

// TestChannelListDocIsMachineReadable 钉住 channel list 的机器可读形状。
func TestChannelListDocIsMachineReadable(t *testing.T) {
	infos := []channels.ChannelInfo{
		{
			ID: 7, Status: channels.StatusEnabled, Type: testChanType, Name: "c1",
			BaseURL: testBaseURL, Models: []string{"m1", "m2"},
			Weight: 3, Priority: 5,
			CredentialKeys: []string{"test"},
			Hint:           "cdef",
			Setting:        map[string]any{"region": "sg"},
		},
		{
			// 平台自营（owner_user_id 为 0）、凭证密文结构坏掉、已软删的一条：
			// 「平台」与「已删」这两个占位符最容易被顺手带进 JSON，所以专门
			// 盯住这一行。它也没有模型清单，正好钉住「空数组不是 null」。
			ID: 8, Status: channels.StatusDisabled, Type: testChanType, Name: "c2",
			BaseURL:       testBaseURL,
			CredentialErr: errors.New("信封头部不合法"),
			DeletedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	}

	got := jsonRoundTrip(t, channelListDoc(infos, true))
	assertJSONField(t, got, "status", cli.JSONOK)

	summary := jsonObjectField(t, got, "summary")
	assertJSONField(t, summary, "total", float64(2))
	assertJSONField(t, summary, "deleted", float64(1))

	list := jsonEntries(t, got, "channels")
	if len(list) != 2 {
		t.Fatalf("渠道数 = %d，期望 2", len(list))
	}
	first := list[0].(map[string]any)
	assertJSONField(t, first, "id", float64(7))
	assertJSONField(t, first, "status", channels.StatusEnabled)
	assertJSONField(t, first, "type", testChanType)
	assertJSONField(t, first, "name", "c1")
	assertJSONField(t, first, "base_url", testBaseURL)
	assertJSONField(t, first, "models", []any{"m1", "m2"})
	assertJSONField(t, first, "owner_user_id", nil)
	assertJSONField(t, first, "weight", float64(3))
	assertJSONField(t, first, "priority", float64(5))
	assertJSONField(t, first, "credential_keys", []any{"test"})
	assertJSONField(t, first, "credential_hint", "cdef")
	assertJSONField(t, first, "credential_error", nil)
	assertJSONField(t, first, "setting", map[string]any{"region": "sg"})
	assertJSONField(t, first, "deleted_at", nil)

	second := list[1].(map[string]any)
	assertJSONField(t, second, "models", []any{})
	assertJSONField(t, second, "owner_user_id", nil)
	assertJSONField(t, second, "credential_keys", []any{})
	assertJSONField(t, second, "credential_hint", nil)
	assertJSONField(t, second, "credential_error", "信封头部不合法")
	assertJSONField(t, second, "setting", nil)
	assertJSONField(t, second, "deleted_at", "2026-01-02T03:04:05Z")

	// 密文与凭证明文一栏都不许出现：这份输出会进管道、进日志。
	for key := range first {
		if strings.Contains(key, "credential_enc") || strings.Contains(key, "secret") {
			t.Errorf("清单里出现了不该有的栏：%q", key)
		}
	}

	t.Run("空清单是空数组，不是 null", func(t *testing.T) {
		empty := jsonRoundTrip(t, channelListDoc(nil, false))
		assertJSONField(t, empty, "status", cli.JSONOK)
		assertJSONField(t, jsonObjectField(t, empty, "summary"), "total", float64(0))
		if len(jsonEntries(t, empty, "channels")) != 0 {
			t.Error("空清单应当是空数组")
		}
		// 没带 --all 时那个计数恒为 0，给出来只是一栏背景音。
		if _, ok := jsonObjectField(t, empty, "summary")["deleted"]; ok {
			t.Error("没带 --all 时 summary 里不该有 deleted")
		}
	})
}

// TestChannelWriteCommandsRejectJSONBeforeDoingAnything 钉住写类命令的守卫位置。
//
// 给的参数本身就不成立（缺 --name / 缺 --id），而要看到的是**不支持 json**
// 那条错：它证明守卫在参数解析与连库之前，而不是「先把事情做完再报不支持」。
// 写类命令上后者的代价是一次真的写入，而脚本只看到一句参数错。
func TestChannelWriteCommandsRejectJSONBeforeDoingAnything(t *testing.T) {
	withFormat(t, cli.FormatJSON)

	cases := []struct {
		command string
		run     func() error
	}{
		{"channel add", func() error { return runChannelAdd(nil) }},
		{"channel update", func() error { return runChannelUpdate(nil) }},
		{"channel enable", func() error {
			return runChannelSetStatus(nil, channels.StatusEnabled)
		}},
		{"channel disable", func() error {
			return runChannelSetStatus(nil, channels.StatusDisabled)
		}},
		{"channel delete", func() error { return runChannelDelete(nil) }},
		{"channel restore", func() error { return runChannelRestore(nil) }},
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
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望参数错的 %d", got, cli.ExitUsage)
			}
		})
	}
}

// TestChannelListJSONStdoutIsPure 拿真命令跑一遍 json 那一档。
//
// 文档的组装函数是纯函数（上面那条钉住了它的形状），而这一条钉的是**命令
// 那一层**：stdout 上从头到尾只有一份 JSON、没有表格文本、没有转义序列。
func TestChannelListJSONStdoutIsPure(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	withFormat(t, cli.FormatJSON)
	stdout, _ := captureStreams(t, func() {
		if err := runChannelList(nil); err != nil {
			t.Fatalf("channel list 失败: %v", err)
		}
	})

	got := assertJSONStdout(t, stdout)
	assertJSONField(t, got, "status", cli.JSONOK)
	if len(jsonEntries(t, got, "channels")) == 0 && got["summary"] == nil {
		t.Error("清单与 summary 至少要有一个")
	}
	summary := jsonObjectField(t, got, "summary")
	total, _ := summary["total"].(float64)
	if int(total) != len(jsonEntries(t, got, "channels")) {
		t.Errorf("summary.total = %v 与 channels 的长度 %d 对不上",
			summary["total"], len(jsonEntries(t, got, "channels")))
	}
}

// TestChannelGetJSONStdoutIsPure 拿真命令跑一遍 get 的 json 那一档。
//
// 与 list / health 那两条同一形状：纯函数那条钉字段形状，这一条钉**命令那一层**
// ——stdout 上从头到尾只有一份 JSON，没有表格文本、没有转义序列、没有被回显掺进来。
func TestChannelGetJSONStdoutIsPure(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	// 夹具要在切到 json 之前造好：json 档下写类命令直接报不支持。
	id := createTestChannel(t, uniqueName("get-json"), uniqueName("model"))

	withFormat(t, cli.FormatJSON)
	stdout, stderr := captureStreams(t, func() {
		if err := runChannelGet([]string{"--id", strconv.FormatInt(id, 10)}); err != nil {
			t.Fatalf("channel get 失败: %v", err)
		}
	})
	if stderr != "" {
		t.Errorf("json 档的 stderr 应当是空的，得到 %q", stderr)
	}

	got := assertJSONStdout(t, stdout)
	assertJSONField(t, got, "status", cli.JSONOK)
	entry := jsonObjectField(t, got, "channel")
	assertJSONField(t, entry, "id", float64(id))
	if _, ok := entry["created_at"]; !ok {
		t.Error("get 的 json 里应当有 created_at")
	}
}

// ── get / health / restore ──────────────────────────────────────────────

// TestChannelGetHealthRestoreRejectBadFlags 钉住这三个动词的参数判据。
//
// 它们全在**连库之前**：写错的命令不该先连上生产库再报错。三条用例因此在没有
// DSN 的机器上也真的跑（碰库的用例在本地是跳过的，而「跳过」在输出里和「通过」
// 长得一样）。
func TestChannelGetHealthRestoreRejectBadFlags(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"get 没有 --id", func() error { return runChannelGet(nil) }, "缺少 --id"},
		{"get 的 --id 是零", func() error { return runChannelGet([]string{"--id", "0"}) }, "缺少 --id"},
		{"get 多出位置参数", func() error {
			return runChannelGet([]string{"--id", "1", "extra"})
		}, "不接受位置参数"},

		{"health 的 --id 是负数", func() error {
			return runChannelHealth([]string{"--id", "-2"})
		}, "--id 必须是正数"},
		{"health 多出位置参数", func() error {
			return runChannelHealth([]string{"extra"})
		}, "不接受位置参数"},
		{"health 的 --since 认不出来", func() error {
			return runChannelHealth([]string{"--since", "昨天"})
		}, "既不是一段时长"},
		{"health 的 --since 不是正的", func() error {
			return runChannelHealth([]string{"--since=-1h"})
		}, "时长必须为正"},
		{"health 的 --since 是零", func() error {
			return runChannelHealth([]string{"--since", "0s"})
		}, "时长必须为正"},
		{"health 的 --since 在将来", func() error {
			return runChannelHealth([]string{"--since", future})
		}, "在将来"},

		{"restore 没有 --id", func() error { return runChannelRestore(nil) }, "缺少 --id"},
		{"restore 多出位置参数", func() error {
			return runChannelRestore([]string{"--id", "1", "extra"})
		}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.run)
			if err == nil {
				t.Fatal("期望报错，却通过了")
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

// TestChannelHealthEmptyListWordingIsPinned 逐字钉住空清单那一句。
//
// 空清单与「命令因为某个原因没跑起来」在安静的终端里长得一样（见 LESSONS.md
// 第 16 条），所以这一句是一个要说出口的结论，不是「没什么可打」。它走的是
// style.go 的 emptyListNote，与 channel list 同一句——两个动词对「还没有渠道」
// 这一件事给出两种说法时，读的人分不出它们是同一件事还是两个不同的状态。
func TestChannelHealthEmptyListWordingIsPinned(t *testing.T) {
	var buf bytes.Buffer
	printChannelHealthList(&buf, nil, false, time.Time{})

	const want = "暂无渠道。可使用 larkaxctl channel create 添加。\n"
	if buf.String() != want {
		t.Errorf("空清单的文案 = %q，期望 %q", buf.String(), want)
	}
}

// TestChannelHealthHoursEmptyWordingIsPinned 逐字钉住「窗口里没有桶」那一句。
//
// 它**不套**空清单那一句：这里缺的不是「还没有渠道」这个可以添加的东西，而是
// 「这段时间它一次都没被调用过」这个结论，给不出下一步命令。
func TestChannelHealthHoursEmptyWordingIsPinned(t *testing.T) {
	var buf bytes.Buffer
	printChannelHealthHours(&buf, nil)

	const want = "窗口内没有逐小时的数据：这段时间它一次都没被调用过。\n"
	if buf.String() != want {
		t.Errorf("空明细的文案 = %q，期望 %q", buf.String(), want)
	}
}

// TestChannelHealthDocIsMachineReadable 钉住 health 那一档 JSON 的字段形状。
//
// 纯函数、不连库：这一档要防的是「表格里的占位符被顺手带进 JSON」——`-`、
// `从未` 在机器读的那一档里都是解析不了的值（见 docs/cli-output.md 的
// 「值的约定」），缺值一律是 null。
func TestChannelHealthDocIsMachineReadable(t *testing.T) {
	ms50, ms95 := 80, 240
	cost := "1.50000000"
	probe := time.Date(2026, 10, 17, 4, 25, 15, 0, time.UTC)
	since := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	// 渲染时刻显式给死：凭证池那一栏要判「哪些账号此刻可用」，而 drained 的账号
	// 过一段冷却期会重新参选——用 time.Now() 的话这条用例会随运行时刻变结论。
	now := time.Date(2026, 10, 17, 5, 0, 0, 0, time.UTC)
	freshFailure := now.Add(-time.Minute)

	rows := []channels.ChannelHealth{
		{
			ID: 7, Name: "c1", Health: "healthy", LastProbeAt: &probe,
			Requests: 40, Successes: 39, Failures: 1,
			CacheHitTokens: 300, CacheMissTokens: 700,
			LatencyP50ms: &ms50, LatencyP95ms: &ms95, Cost: &cost,
			Accounts: []channels.AccountHealth{
				{ID: 21, Name: "main", Weight: 2, Health: "healthy", Conclusion: "成功"},
				{
					// 刚被上游拒绝的那个：冷却期没过，此刻不可用——但它在池子里，
					// 所以整体是 degraded 而不是 drained。
					ID: 22, Name: "backup", Weight: 1, Health: "drained",
					ConsecutiveFailures: 1, LastFailureAt: &freshFailure,
					Conclusion: "凭证被上游拒绝",
				},
			},
		},
		{
			// 一条什么数据都没有的渠道：这一行专门盯住「缺值是 null，不是 `-`
			// 也不是 0」。它同时是已软删的那一条（--all 才看得见）。
			ID: 8, Name: "c2", Health: channels.HealthUnknown,
			DeletedAt: time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC),
		},
	}
	buckets := []channels.HealthBucket{{
		Bucket:   time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC),
		Requests: 40, Successes: 39, Failures: 1,
		CacheHitTokens: 300, CacheMissTokens: 700,
		LatencyP95ms: &ms95, Cost: &cost,
	}}

	got := jsonRoundTrip(t, channelHealthDoc(rows, now, since, true, buckets, true))
	assertJSONField(t, got, "status", cli.JSONOK)

	summary := jsonObjectField(t, got, "summary")
	assertJSONField(t, summary, "total", float64(2))
	assertJSONField(t, summary, "deleted", float64(1))
	assertJSONField(t, summary, "buckets", float64(1))
	// 窗口起点必须进文档：不给它的话 `requests: 40` 说不清是哪段时间里的 40。
	assertJSONField(t, summary, "since", "2026-10-16T04:00:00Z")

	list := jsonEntries(t, got, "channels")
	first := list[0].(map[string]any)
	assertJSONField(t, first, "id", float64(7))
	assertJSONField(t, first, "name", "c1")
	assertJSONField(t, first, "health", "healthy")
	assertJSONField(t, first, "last_probe_at", "2026-10-17T04:25:15Z")
	assertJSONField(t, first, "requests", float64(40))
	assertJSONField(t, first, "successes", float64(39))
	assertJSONField(t, first, "failures", float64(1))
	assertJSONField(t, first, "failure_rate", 1.0/40.0)
	// 缓存 token 是数字，不是表格里的标签；0 表示窗口里没有样本（与 requests 同一档）。
	assertJSONField(t, first, "cache_hit_tokens", float64(300))
	assertJSONField(t, first, "cache_miss_tokens", float64(700))
	// 分位数与成本是**数字**与**定点字符串**，不是表格里那种带单位的写法：
	// `"240ms"` 与 `"1.50"` 都会让按数字解析它的一方拿到一个解析不了的值。
	assertJSONField(t, first, "latency_p50_ms", float64(80))
	assertJSONField(t, first, "latency_p95_ms", float64(240))
	assertJSONField(t, first, "cost", "1.50000000")
	assertJSONField(t, first, "deleted_at", nil)
	// 凭证池三项：整体结论、此刻可用个数、逐账号读数。判据与选路同一个
	// （见 channels.AccountHealth.Usable）——命令行上说可用，就是选路会挑它。
	assertJSONField(t, first, "pool", "degraded")
	assertJSONField(t, first, "accounts_usable", float64(1))
	accounts := jsonEntries(t, first, "accounts")
	if len(accounts) != 2 {
		t.Fatalf("账号 %d 个，期望 2 个", len(accounts))
	}
	healthy := accounts[0].(map[string]any)
	assertJSONField(t, healthy, "id", float64(21))
	assertJSONField(t, healthy, "name", "main")
	assertJSONField(t, healthy, "weight", float64(2))
	assertJSONField(t, healthy, "health", "healthy")
	assertJSONField(t, healthy, "consecutive_failures", float64(0))
	assertJSONField(t, healthy, "last_failure_at", nil)
	assertJSONField(t, healthy, "conclusion", "成功")
	assertJSONField(t, healthy, "usable", true)
	drained := accounts[1].(map[string]any)
	assertJSONField(t, drained, "weight", float64(1))
	assertJSONField(t, drained, "health", "drained")
	assertJSONField(t, drained, "consecutive_failures", float64(1))
	// 冷却期没过：状态是 drained，usable 是 false。两个字段一起才答得出
	// 「它为什么没被选路挑中」。
	assertJSONField(t, drained, "last_failure_at", "2026-10-17T04:59:00Z")
	assertJSONField(t, drained, "usable", false)
	assertJSONField(t, drained, "conclusion", "凭证被上游拒绝")

	second := list[1].(map[string]any)
	// 断言用字面量而不是 channels.HealthUnknown：夹具里那格也是用同一个常量填的，
	// 常量改了名或改了值，两边一起变，这条就什么都没钉住。
	assertJSONField(t, second, "health", "unknown")
	assertJSONField(t, second, "last_probe_at", nil)
	assertJSONField(t, second, "requests", float64(0))
	assertJSONField(t, second, "failure_rate", nil)
	assertJSONField(t, second, "latency_p50_ms", nil)
	assertJSONField(t, second, "latency_p95_ms", nil)
	assertJSONField(t, second, "cost", nil)
	// 一条什么数据都没有的渠道：缓存 token 与 requests 一样是 0，不是 null——
	// 「窗口里没有样本」在选路侧读的也是 0（见 cacheHitRate）。
	assertJSONField(t, second, "cache_hit_tokens", float64(0))
	assertJSONField(t, second, "cache_miss_tokens", float64(0))
	assertJSONField(t, second, "deleted_at", "2026-10-15T01:02:03Z")
	// 一个账号都没挂：池的状态是 empty（不是 drained），可用个数 0，清单是空数组
	// 而不是 null——与 channels 那一栏同一条（见 format.go 的 jsonList）。
	assertJSONField(t, second, "pool", "empty")
	assertJSONField(t, second, "accounts_usable", float64(0))
	if n := len(jsonEntries(t, second, "accounts")); n != 0 {
		t.Errorf("没挂账号的渠道应当打到空数组，得到 %d 个账号", n)
	}

	bs := jsonEntries(t, got, "buckets")
	if len(bs) != 1 {
		t.Fatalf("逐小时明细 %d 行，期望 1 行", len(bs))
	}
	bucket := bs[0].(map[string]any)
	assertJSONField(t, bucket, "bucket", "2026-10-17T04:00:00Z")
	assertJSONField(t, bucket, "requests", float64(40))
	assertJSONField(t, bucket, "cache_hit_tokens", float64(300))
	assertJSONField(t, bucket, "cache_miss_tokens", float64(700))
	assertJSONField(t, bucket, "latency_p50_ms", nil)
	assertJSONField(t, bucket, "latency_p95_ms", float64(240))
	assertJSONField(t, bucket, "cost", "1.50000000")

	t.Run("清单那一档不给 buckets", func(t *testing.T) {
		listOnly := jsonRoundTrip(t, channelHealthDoc(rows, now, since, false, nil, false))
		if _, ok := listOnly["buckets"]; ok {
			t.Error("没带 --id 时不该有逐小时明细")
		}
		// 没带 --all 时那个计数恒为 0，给出来只是一栏背景音。
		if _, ok := jsonObjectField(t, listOnly, "summary")["deleted"]; ok {
			t.Error("没带 --all 时 summary 里不该有 deleted")
		}
	})

	t.Run("空清单是空数组，不是 null", func(t *testing.T) {
		empty := jsonRoundTrip(t, channelHealthDoc(nil, now, since, false, nil, true))
		if len(jsonEntries(t, empty, "channels")) != 0 {
			t.Error("空清单应当是空数组")
		}
		if len(jsonEntries(t, empty, "buckets")) != 0 {
			t.Error("没有桶时 buckets 应当是空数组")
		}
	})
}

// TestChannelHealthShowsCacheTokens 钉住缓存 token 真的出现在三张表上。
//
// 它钉的是 `channel_health_rollup` 的 cache_hit_tokens / cache_miss_tokens——选路会
// 读、而命令行原先一处都打不出来的那两列。判据不是「库里读得出来」（那是
// internal/channels 那侧的用例），而是**三个视口里都看得见**：汇总表、--id 的读数、
// 逐小时明细。只打在其中一处时，另一处的读者仍然够不着。
func TestChannelHealthShowsCacheTokens(t *testing.T) {
	row := channels.ChannelHealth{
		ID: 7, Name: "c1", Health: "healthy",
		Requests: 40, Successes: 39, Failures: 1,
		CacheHitTokens: 300, CacheMissTokens: 700,
	}
	bucket := channels.HealthBucket{
		Bucket:   time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC),
		Requests: 40, Successes: 39, Failures: 1,
		CacheHitTokens: 300, CacheMissTokens: 700,
	}

	views := []struct {
		name  string
		print func(w io.Writer)
	}{
		{"汇总表", func(w io.Writer) {
			printChannelHealthList(w, []channels.ChannelHealth{row}, false, time.Time{})
		}},
		{"单条读数", func(w io.Writer) { printChannelHealthDetail(w, row, time.Time{}) }},
		{"逐小时明细", func(w io.Writer) {
			printChannelHealthHours(w, []channels.HealthBucket{bucket})
		}},
	}

	for _, v := range views {
		t.Run(v.name, func(t *testing.T) {
			var buf bytes.Buffer
			v.print(&buf)
			got := buf.String()
			for _, want := range []string{"缓存命中", "缓存未命中", "300", "700"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s 里没有 %q：\n%s", v.name, want, got)
				}
			}
			// 打的是原始 token 数，不是表格里那种带单位的写法——按数字接走它的一方
			// 解析不了 `300 tokens`。
			if strings.Contains(got, "300 tokens") {
				t.Errorf("%s 给 token 数加了单位：\n%s", v.name, got)
			}
		})
	}
}

// TestChannelGetDocIsMachineReadable 钉住单条详情那一档把两个时刻也带上。
func TestChannelGetDocIsMachineReadable(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

	got := jsonRoundTrip(t, channelGetDoc(channels.ChannelInfo{
		ID: 7, Name: "c1", Type: testChanType, BaseURL: testBaseURL,
		Status: channels.StatusEnabled, Models: []string{"m1"},
		CredentialKeys: []string{"test"},
		CreatedAt:      created, UpdatedAt: updated,
	}))

	assertJSONField(t, got, "status", cli.JSONOK)
	entry := jsonObjectField(t, got, "channel")
	assertJSONField(t, entry, "id", float64(7))
	assertJSONField(t, entry, "name", "c1")
	assertJSONField(t, entry, "created_at", "2026-10-01T00:00:00Z")
	assertJSONField(t, entry, "updated_at", "2026-10-02T00:00:00Z")
	// 清单那几栏在这一档里一个不少：详情是「清单那几栏 + 两个时刻」。
	assertJSONField(t, entry, "models", []any{"m1"})
	assertJSONField(t, entry, "credential_keys", []any{"test"})
}

// ── 这三条要真库 ────────────────────────────────────────────────────────

// insertHealthBucket 直接往 channel_health_rollup 里写一行。
//
// 走 SQL 而不是命令：这张表只由网关的调用收尾处写，命令行上没有产生它的路径，
// 而这几条用例要钉的正是「窗口筛选把哪几行算了进去」。
func insertHealthBucket(t *testing.T, db *sql.DB, id int64, bucket time.Time,
	requests, failures int, p95 any, cost string, cacheHit, cacheMiss int) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_health_rollup
			(channel_id, bucket, requests, successes, failures, latency_p95_ms, cost,
			 cache_hit_tokens, cache_miss_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (channel_id, model, bucket) DO UPDATE
		   SET requests = EXCLUDED.requests, successes = EXCLUDED.successes,
		       failures = EXCLUDED.failures, latency_p95_ms = EXCLUDED.latency_p95_ms,
		       cost = EXCLUDED.cost,
		       cache_hit_tokens = EXCLUDED.cache_hit_tokens,
		       cache_miss_tokens = EXCLUDED.cache_miss_tokens`,
		id, bucket, requests, requests-failures, failures, p95, cost, cacheHit, cacheMiss)
	if err != nil {
		t.Fatalf("插入渠道 %d 的健康桶失败: %v", id, err)
	}
}

// setQuotaHealth 写一行额度状态（health 与最近一次探测时刻）。
func setQuotaHealth(t *testing.T, db *sql.DB, id int64, health string, probeAt time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_quota_state (channel_id, health, last_probe_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (channel_id) DO UPDATE
		   SET health = EXCLUDED.health, last_probe_at = EXCLUDED.last_probe_at`,
		id, health, probeAt)
	if err != nil {
		t.Fatalf("写渠道 %d 的额度状态失败: %v", id, err)
	}
}

// healthJSON 跑一遍 channel health -o json，把那份文档解回来。
func healthJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	withFormat(t, cli.FormatJSON)
	stdout, _ := captureStreams(t, func() {
		if err := runChannelHealth(args); err != nil {
			t.Fatalf("channel health %v 失败: %v", args, err)
		}
	})
	return assertJSONStdout(t, stdout)
}

// onlyChannelEntries 从 health 的输出里挑出属于某个渠道的那一条。
func onlyChannelEntries(t *testing.T, doc map[string]any, id int64) map[string]any {
	t.Helper()
	for _, e := range jsonEntries(t, doc, "channels") {
		entry := e.(map[string]any)
		if entry["id"] == float64(id) {
			return entry
		}
	}
	t.Fatalf("health 的输出里没有渠道 %d", id)
	return nil
}

// TestChannelHealthWindowFiltersBuckets 钉住窗口筛选：两条不同小时的数据，
// 只有落进窗口的那一条被算进汇总与明细。
//
// 造两行**相隔 30 小时**的桶，用三个起点各跑一遍：
//
//   - `--since 24h`（默认那一档的口径）只算得进最近那一桶；
//   - `--since 40h` 两桶都算进去；
//   - `--since <40 小时前的 RFC3339>` 与上一条结果相同——两种写法是同一个窗口。
//
// 顺带钉住分位数的口径：汇总里的 p95 取**最近一个非空桶**的值，不是两桶的平均，
// 也不是最差的那个（分位数不可累加，理由见 internal/channels 的 healthFrom）。
func TestChannelHealthWindowFiltersBuckets(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := createTestChannel(t, uniqueName("health-window"), uniqueName("model"))
	probe := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	setQuotaHealth(t, db, id, "degraded", probe)

	recent := hourBucketOf(t, 1)
	old := hourBucketOf(t, 30)
	insertHealthBucket(t, db, id, recent, 10, 1, 200, "1.50000000", 300, 700)
	insertHealthBucket(t, db, id, old, 100, 50, 900, "9.00000000", 1000, 1000)

	t.Run("24 小时只算得进最近那一桶", func(t *testing.T) {
		doc := healthJSON(t, "--id", strconv.FormatInt(id, 10), "--since", "24h")
		entry := onlyChannelEntries(t, doc, id)

		assertJSONField(t, entry, "requests", float64(10))
		assertJSONField(t, entry, "failures", float64(1))
		assertJSONField(t, entry, "failure_rate", 0.1)
		assertJSONField(t, entry, "latency_p95_ms", float64(200))
		assertJSONField(t, entry, "cost", "1.50000000")
		// 缓存 token 与请求数同一档：窗口内各小时之和。
		assertJSONField(t, entry, "cache_hit_tokens", float64(300))
		assertJSONField(t, entry, "cache_miss_tokens", float64(700))
		// health 与最近一次探测来自 channel_quota_state，与窗口无关。
		assertJSONField(t, entry, "health", "degraded")
		assertJSONField(t, entry, "last_probe_at", probe.Format(time.RFC3339))

		buckets := jsonEntries(t, doc, "buckets")
		if len(buckets) != 1 {
			t.Fatalf("明细 %d 行，期望只剩窗口内的那 1 行", len(buckets))
		}
		assertJSONField(t, buckets[0].(map[string]any), "bucket", recent.Format(time.RFC3339))
	})

	t.Run("40 小时两桶都算进去", func(t *testing.T) {
		doc := healthJSON(t, "--id", strconv.FormatInt(id, 10), "--since", "40h")
		entry := onlyChannelEntries(t, doc, id)

		assertJSONField(t, entry, "requests", float64(110))
		assertJSONField(t, entry, "failures", float64(51))
		assertJSONField(t, entry, "cost", "10.50000000")
		assertJSONField(t, entry, "cache_hit_tokens", float64(1300))
		assertJSONField(t, entry, "cache_miss_tokens", float64(1700))
		// 最近的桶是 recent，所以汇总的 p95 是它那一个，不是 900（最差的那个），
		// 也不是两桶的某种平均。
		assertJSONField(t, entry, "latency_p95_ms", float64(200))

		if got := len(jsonEntries(t, doc, "buckets")); got != 2 {
			t.Fatalf("明细 %d 行，期望 2 行", got)
		}
	})

	t.Run("RFC3339 时刻与时长是同一个窗口", func(t *testing.T) {
		start := time.Now().Add(-40 * time.Hour).UTC().Format(time.RFC3339)
		doc := healthJSON(t, "--id", strconv.FormatInt(id, 10), "--since", start)
		entry := onlyChannelEntries(t, doc, id)
		assertJSONField(t, entry, "requests", float64(110))
		assertJSONField(t, doc["summary"].(map[string]any), "since", start)
	})
}

// TestChannelHealthSkipsDeletedUnlessAll 钉住读侧对已软删渠道的处置。
//
// 清单形状默认不列已删的、--all 才列；按 id 的单条读取一律打得出来并标出
// 「已删」——`--all` 管的是「清单要不要变长」，不是「单条能不能看」。
func TestChannelHealthSkipsDeletedUnlessAll(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := createTestChannel(t, uniqueName("health-deleted"), uniqueName("model"))
	idArg := strconv.FormatInt(id, 10)
	if err := runCaptured(t, func() error {
		return runChannelDelete([]string{"--id", idArg})
	}); err != nil {
		t.Fatalf("delete 失败: %v", err)
	}

	listOut, _ := captureStreams(t, func() {
		if err := runChannelHealth(nil); err != nil {
			t.Fatalf("health 失败: %v", err)
		}
	})
	if containsID(listOut, id) {
		t.Errorf("默认清单里出现了已删的渠道 %d：\n%s", id, listOut)
	}
	if strings.Contains(listOut, "已删") {
		t.Errorf("默认清单不该有「已删」那一栏：\n%s", listOut)
	}

	allOut, _ := captureStreams(t, func() {
		if err := runChannelHealth([]string{"--all"}); err != nil {
			t.Fatalf("health --all 失败: %v", err)
		}
	})
	if !containsID(allOut, id) {
		t.Errorf("health --all 里没有已删的渠道 %d：\n%s", id, allOut)
	}
	if !deletedNoteNamesID(allOut, id) {
		t.Errorf("health --all 没有点出 %s 已删除：\n%s", idArg, allOut)
	}

	// 单条读取：不带 --all 也打得出来，并在「已删」那一栏标出删除时刻。
	detailOut, _ := captureStreams(t, func() {
		if err := runChannelHealth([]string{"--id", idArg}); err != nil {
			t.Fatalf("health --id 失败: %v", err)
		}
	})
	var deletedAt sql.NullTime
	if err := db.QueryRowContext(context.Background(),
		`SELECT deleted_at FROM channels WHERE id = $1`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("读回 deleted_at 失败: %v", err)
	}
	if !strings.Contains(detailOut, "已删") ||
		!strings.Contains(detailOut, deletedAt.Time.UTC().Format(time.RFC3339)) {
		t.Errorf("health --id 没有标出已删与删除时刻：\n%s", detailOut)
	}
}

// TestChannelHealthUnknownIDIsNotFound 钉住「按 id 读一条不存在的渠道」。
//
// 与 get / restore 同一档：id 指的是库里没有的那一条时报 ErrChannelNotFound
// （退出 4），而不是打一张空表——空表读起来像「这条渠道现在没有读数」，
// 而事实是根本没有这一条。
func TestChannelHealthUnknownIDIsNotFound(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runChannelHealth([]string{"--id", "999999999"})
	})
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Fatalf("读一条不存在的渠道应当报 ErrChannelNotFound，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("退出码 = %d，期望 %d", got, cli.ExitConflict)
	}
}

// TestChannelGetReadsOneChannel 钉住 get 的三条：正常路径、已删的照打、找不到报 4。
func TestChannelGetReadsOneChannel(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	name := uniqueName("get-channel")
	model := uniqueName("model")
	id := createTestChannel(t, name, model)
	idArg := strconv.FormatInt(id, 10)

	out, note := captureStreams(t, func() {
		if err := runChannelGet([]string{"--id", idArg}); err != nil {
			t.Fatalf("get 失败: %v", err)
		}
	})
	for _, want := range []string{idArg, name, testBaseURL, model, "openai", "平台", "test"} {
		if !strings.Contains(out, want) {
			t.Errorf("详情里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, testSecret) {
		t.Errorf("详情里出现了明文凭证：\n%s", out)
	}
	if strings.Contains(out, "已删") {
		t.Errorf("没删的渠道不该有「已删」那一行：\n%s", out)
	}
	if note != "" {
		t.Errorf("get 不该往 stderr 上写东西：%q", note)
	}

	t.Run("已删的也打得出来", func(t *testing.T) {
		if err := runCaptured(t, func() error {
			return runChannelDelete([]string{"--id", idArg})
		}); err != nil {
			t.Fatalf("delete 失败: %v", err)
		}
		deletedOut, _ := captureStreams(t, func() {
			if err := runChannelGet([]string{"--id", idArg}); err != nil {
				t.Fatalf("get 已删的渠道失败: %v", err)
			}
		})
		if !strings.Contains(deletedOut, "已删") {
			t.Errorf("get 一条已删的渠道应当标出它：\n%s", deletedOut)
		}
	})

	t.Run("找不到那条渠道走退出码 4", func(t *testing.T) {
		err := runCaptured(t, func() error {
			return runChannelGet([]string{"--id", "999999999"})
		})
		if !errors.Is(err, channels.ErrChannelNotFound) {
			t.Fatalf("期望 ErrChannelNotFound，得到 %v", err)
		}
		if got := cli.ExitCode(err); got != cli.ExitConflict {
			t.Errorf("退出码 = %d，期望 %d", got, cli.ExitConflict)
		}
	})
}

// TestChannelRestoreRoundTrip 是这一组里唯一的端到端：
// 软删 → restore → 又出现在 channel list 里，且 health 回落成 unknown。
//
// 「health 回落」这一条必须先造出一个**不是** unknown 的结论才有意义——一条
// 从来没被探测过、health 本来就是 unknown 的渠道，恢复了也还是 unknown，那样
// 这条断言什么都没钉住。所以先把 health 写成 drained。
func TestChannelRestoreRoundTrip(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := createTestChannel(t, uniqueName("restore-channel"), uniqueName("model"))
	idArg := strconv.FormatInt(id, 10)
	setQuotaHealth(t, db, id, "drained", time.Now().Add(-time.Hour).UTC())

	if err := runCaptured(t, func() error {
		return runChannelDelete([]string{"--id", idArg})
	}); err != nil {
		t.Fatalf("delete 失败: %v", err)
	}

	out, note := captureStreams(t, func() {
		if err := runChannelRestore([]string{"--id", idArg}); err != nil {
			t.Fatalf("restore 失败: %v", err)
		}
	})
	// 回显走 stderr，而且必须点出 health 回落这件事：恢复之后的第一次 health
	// 会报 unknown，而 unknown 与「这条渠道没问题」很容易被读成一回事。
	if !strings.Contains(note, "已恢复渠道 "+idArg) {
		t.Errorf("restore 的回显不对：\n%s", note)
	}
	if !strings.Contains(note, channels.HealthUnknown) {
		t.Errorf("restore 的回显没有点出 health 回落成 %s：\n%s", channels.HealthUnknown, note)
	}
	if strings.Contains(out, "已恢复渠道") {
		t.Errorf("回显不该混进 stdout：\n%s", out)
	}

	// 一、它又出现在默认清单里，而且回到可用状态。
	listOut, _ := captureStreams(t, func() {
		if err := runChannelList(nil); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	if !containsID(listOut, id) {
		t.Errorf("恢复之后默认清单里应当有渠道 %d：\n%s", id, listOut)
	}
	var (
		deleted sql.NullTime
		status  string
		health  string
	)
	if err := db.QueryRowContext(context.Background(),
		`SELECT deleted_at, status FROM channels WHERE id = $1`, id).
		Scan(&deleted, &status); err != nil {
		t.Fatalf("读回渠道行失败: %v", err)
	}
	if deleted.Valid {
		t.Error("恢复之后 deleted_at 应当是 NULL")
	}
	if status != channels.StatusEnabled {
		// 「恢复回可用」是字面意思：选路要求 status = 'enabled'，恢复成 disabled
		// 的渠道会出现在清单上却选不中。
		t.Errorf("恢复之后 status = %q，期望 %q", status, channels.StatusEnabled)
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT health FROM channel_quota_state WHERE channel_id = $1`, id).Scan(&health); err != nil {
		t.Fatalf("读回额度状态失败: %v", err)
	}
	if health != channels.HealthUnknown {
		t.Errorf("恢复之后 health = %q，期望 %q", health, channels.HealthUnknown)
	}

	// 二、`channel health` 上看到的也是 unknown——这是使用者真正会撞上的那一面，
	// 库里那一列对了而输出没跟上，等于没做。
	healthOut, _ := captureStreams(t, func() {
		if err := runChannelHealth([]string{"--id", idArg}); err != nil {
			t.Fatalf("health 失败: %v", err)
		}
	})
	if !strings.Contains(healthOut, channels.HealthUnknown) {
		t.Errorf("恢复之后 health 的输出里没有 %s：\n%s", channels.HealthUnknown, healthOut)
	}

	// 三、凭证密文一个字节都没动：恢复不是重新封装。
	if len(channelEnvelope(t, db, id)) == 0 {
		t.Error("恢复不该动凭证密文，而它现在是空的")
	}

	// 四、再恢复一次是幂等的。
	repeatOut, repeatNote := captureStreams(t, func() {
		if err := runChannelRestore([]string{"--id", idArg}); err != nil {
			t.Fatalf("重复 restore 不该报错（幂等），得到 %v", err)
		}
	})
	if !strings.Contains(repeatNote, "没有处于删除状态") {
		t.Errorf("重复恢复的输出不对：\nstdout:\n%s\nstderr:\n%s", repeatOut, repeatNote)
	}
	if strings.Contains(repeatNote, "已恢复渠道") {
		t.Errorf("没恢复的那一次不该说「已恢复」：\n%s", repeatNote)
	}

	// 五、不存在的 id 仍然报错，退出码走「要改的那一行不存在」。
	err := runCaptured(t, func() error {
		return runChannelRestore([]string{"--id", "999999999"})
	})
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Fatalf("恢复不存在的 id 应当报 ErrChannelNotFound，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("恢复不存在的 id 的退出码 = %d，期望 %d", got, cli.ExitConflict)
	}
}

// TestChannelHealthJSONStdoutIsPure 拿真命令跑一遍 json 那一档。
func TestChannelHealthJSONStdoutIsPure(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	got := healthJSON(t, "--since", "24h")
	assertJSONField(t, got, "status", cli.JSONOK)
	summary := jsonObjectField(t, got, "summary")
	total, _ := summary["total"].(float64)
	if int(total) != len(jsonEntries(t, got, "channels")) {
		t.Errorf("summary.total = %v 与 channels 的长度 %d 对不上",
			summary["total"], len(jsonEntries(t, got, "channels")))
	}
}

// hourBucketOf 返回距今最近的那个整点桶，n 为回退的小时数。
//
// 与 internal/channels 的 integration_test 里那一个同形：桶是 UTC 整点，
// 而窗口筛选比的就是这个时刻。
func hourBucketOf(t *testing.T, n int) time.Time {
	t.Helper()
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(n) * time.Hour)
}

// TestChannelHealthAccountsTable 钉住逐账号那张表的列，以及「可用」这一列的判据。
//
// 两行都可以写着 drained：冷却期没过的那个不可用，过了的那个已经重新参选。
// 只打「状态」的话这两种处境在表上一模一样，而它们该做的补救动作不同
// ——所以「可用」必须真的按 now 算出来，不能把参数收下不用。
func TestChannelHealthAccountsTable(t *testing.T) {
	withTerminal(t, false)
	at := time.Date(2026, 10, 17, 5, 0, 0, 0, time.UTC)
	cooling := at.Add(-time.Minute) // 冷却期没过
	cooled := at.Add(-time.Hour)    // 冷却期已过
	h := channels.ChannelHealth{
		ID: 7, Name: "c1",
		Accounts: []channels.AccountHealth{
			{ID: 21, Name: "main", Weight: 2, Health: channels.AccountHealthHealthy, Conclusion: "成功"},
			{ID: 22, Name: "cooling", Weight: 1, Health: channels.AccountHealthDrained,
				ConsecutiveFailures: 1, LastFailureAt: &cooling, Conclusion: "凭证被上游拒绝"},
			{ID: 23, Name: "cooled", Weight: 1, Health: channels.AccountHealthDrained,
				ConsecutiveFailures: 1, LastFailureAt: &cooled, Conclusion: "凭证被上游拒绝"},
		},
	}

	var buf bytes.Buffer
	printChannelHealthAccounts(&buf, h, at)
	out := buf.String()
	if !strings.Contains(out, "可用") {
		t.Fatalf("表头里没有「可用」一列：\n%s", out)
	}

	// 按行判：名字不会出现在别的列里，所以「这一行里有 是 / 否」就是这一列。
	for _, tc := range []struct {
		account string
		want    string
	}{
		{"main", "是"},
		{"cooling", "否"},
		{"cooled", "是"},
	} {
		line := ""
		for _, candidate := range strings.Split(out, "\n") {
			if strings.Contains(candidate, tc.account) {
				line = candidate
				break
			}
		}
		if line == "" {
			t.Fatalf("表里没有账号 %s：\n%s", tc.account, out)
		}
		if !strings.Contains(line, tc.want) {
			t.Errorf("账号 %s 的「可用」=%q，期望「%s」那一格：%s",
				tc.account, line, tc.want, out)
		}
	}
}

// TestChannelHealthAccountsEmptyHint 钉住空池那句提示——它必须是一条能直接
// 照抄的命令，且已删的渠道不能被指去 account add（那条路会拒绝已删渠道）。
func TestChannelHealthAccountsEmptyHint(t *testing.T) {
	withTerminal(t, false)
	at := time.Date(2026, 10, 17, 5, 0, 0, 0, time.UTC)

	var live bytes.Buffer
	printChannelHealthAccounts(&live, channels.ChannelHealth{ID: 7, Name: "c1"}, at)
	got := live.String()
	if !strings.Contains(got, "account add <账号> --channel 7") {
		t.Errorf("空池提示不是一条能照抄的命令：%q", got)
	}

	var deleted bytes.Buffer
	printChannelHealthAccounts(&deleted, channels.ChannelHealth{
		ID: 7, Name: "c1", DeletedAt: at}, at)
	got = deleted.String()
	if !strings.Contains(got, "channel restore --id 7") {
		t.Errorf("已删渠道的提示没指向 restore：%q", got)
	}
	// 恢复之后还要挂账号，所以「account add」出现在这里是对的；但顺序不能反
	// ——先 add 会被拒（account add 不认已删渠道）。
	if i, j := strings.Index(got, "channel restore"), strings.Index(got, "account add"); i < 0 || j < i {
		t.Errorf("已删渠道的提示顺序不对（应先 restore 再 add）：%q", got)
	}
}
