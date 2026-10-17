package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
		{"没有 --type", without(base, "--type"), "缺少 --type"},
		{"没有 --base-url", without(base, "--base-url"), "缺少 --base-url"},
		{"没有 --models", without(base, "--models"), "缺少 --models"},
		{"没有凭证", without(base, "--credential-stdin"), "缺少凭证"},
		{"两个凭证来源都给", append(base, "--credential-env", "SOME_VAR"), "只能给一个"},
		{"多出位置参数", append(base, "extra"), "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runChannelCreate(tc.args) })
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
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
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
	if got := exitCode(err); got != exitUsage {
		t.Errorf("未知标志是参数错，退出码应当是 %d，得到 %d", exitUsage, got)
	}
}

// TestChannelCredentialSourcesRejectEmpty 钉住两个来源各自「读到空」的情形。
//
// 空凭证写进库是一条谁都调不通的渠道，而它的表现是「请求全都失败」，
// 离「当初那把 key 没传进来」已经很远。所以两个来源都要当场拒绝。
func TestChannelCredentialSourcesRejectEmpty(t *testing.T) {
	withStdin(t, "\n")
	err := runCaptured(t, func() error {
		return runChannelCreate([]string{
			"--name", "c1", "--type", testChanType, "--base-url", testBaseURL,
			"--models", "m1", "--credential-stdin",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "标准输入里没有凭证") {
		t.Fatalf("空的标准输入应当被拒，得到 %v", err)
	}

	err = runCaptured(t, func() error {
		return runChannelCreate([]string{
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
		if err := runChannelCreate([]string{
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
		if err := runChannelCreate([]string{
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
		if err := runChannelCreate([]string{
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
		return runChannelCreate([]string{
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

// channelEnvelope 直接读出某一行的凭证密文。
//
// 清单里没有这一栏（也不该有），所以只能自己查——用例要证明的正是
// 「密文能被解开」，而不是「清单里没打印密文」。
func channelEnvelope(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	var envelope []byte
	err := db.QueryRowContext(context.Background(),
		`SELECT credential_enc FROM channels WHERE id = $1`, id).Scan(&envelope)
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

// TestChannelSettingPrintsUnreadNote 钉住「给了 --setting 时那句话真的打出来了」。
//
// 这是 #360 在管理面上的落点：setting 写得到、读得到，而没有任何东西按它
// 做判断。从命令行看，「配了但不生效」与「配了就生效」长得一模一样——两边
// 都成功、清单里都有那一行。所以不生效这件事必须在**写入的那一刻**说出来。
//
// 四条判据，缺一条这句话都会以某种方式失效：
//
//  1. 给了 --setting 时提示出现（create 与 update 两条写入路径都要）；
//  2. 它走 stderr——stdout 上那一行（create 是 ID）要能被脚本直接接走；
//  3. 没给 --setting 时不出现，否则它很快变成所有输出里的背景音；
//  4. list 把带着 setting 的行点出来——否则事后核对配置时完全看不出来。
//
// 声明改成「已经有适配器读它」之后，这四条按相反方向判（提示必须消失），
// 而不是跳过：跳过在输出里和通过一样，而那句提示消失本身正是要钉住的事。
func TestChannelSettingPrintsUnreadNote(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	// 空串表示适配器已经开始读 setting：那时提示**不该**再出现。
	note := channel.SettingUnreadNote()
	wantNote := func(t *testing.T, got, what string) {
		t.Helper()
		if note == "" {
			if strings.Contains(got, "注意：") || strings.Contains(got, "配了 setting") {
				t.Errorf("%s：setting 已经有读者，提示却还在打：\n%s", what, got)
			}
			return
		}
		if !strings.Contains(got, note) {
			t.Errorf("%s：提示没打出来：\n%s", what, got)
		}
	}

	// 一、create 给了 --setting。
	const settingJSON = `{"supports_optional_param":true}`
	withStdin(t, testSecret)
	stdout, stderr := captureStreams(t, func() {
		if err := runChannelCreate([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", uniqueName("model"),
			"--credential-stdin", "--setting", settingJSON,
		}); err != nil {
			t.Fatalf("create 失败: %v", err)
		}
	})
	wantNote(t, stderr, "create 给了 --setting")
	if note != "" && strings.Contains(stdout, note) {
		t.Errorf("提示混进了 stdout，那条链就断了：\n%s", stdout)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 应当只有渠道 ID，得到 %q（%v）", stdout, err)
	}

	// 写进去的键仍然照原样落库：提示说的是「没有读者」，不是「不存」。
	idArg := strconv.FormatInt(id, 10)
	if got := findChannel(t, db, id).Setting; got["supports_optional_param"] != true {
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
	wantNote(t, stderr, "update 给了 --setting")

	// 三、update 没给 --setting 时不打。
	_, stderr = captureStreams(t, func() {
		if updateErr := runChannelUpdate([]string{"--id", idArg, "--priority", "7"}); updateErr != nil {
			t.Fatalf("update 失败: %v", updateErr)
		}
	})
	if strings.Contains(stderr, "注意：") {
		t.Errorf("没碰 setting 的那次 update 不该打这句：\n%s", stderr)
	}

	// 四、list 点出带着 setting 的渠道。清单里有别的用例建的渠道，
	// 所以判据是「这条渠道的 ID 出现在那句话里」，不是整份输出相等。
	listOut, _ := captureStreams(t, func() {
		if listErr := runChannelList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	if note == "" {
		if strings.Contains(listOut, "配了 setting") {
			t.Errorf("setting 已经有读者，list 却还在点出它：\n%s", listOut)
		}
		return
	}
	if !strings.Contains(listOut, "配了 setting") || !strings.Contains(listOut, idArg) {
		t.Errorf("list 没有点出渠道 %s 配了 setting：\n%s", idArg, listOut)
	}
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

// createTestChannel 用命令行建一条渠道，返回它的 ID。
func createTestChannel(t *testing.T, name, model string) int64 {
	t.Helper()
	withStdin(t, testSecret)
	stdout, _ := captureStreams(t, func() {
		if err := runChannelCreate([]string{
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

	out, _ := captureStreams(t, func() {
		if err := runChannelDelete([]string{"--id", idArg}); err != nil {
			t.Fatalf("delete 失败: %v", err)
		}
	})
	if !strings.Contains(out, "已删除渠道 "+idArg) {
		t.Errorf("delete 的输出不对：\n%s", out)
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
	if !strings.Contains(allOut, "渠道 "+idArg+" 已删除") {
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
	out, _ = captureStreams(t, func() {
		if err := runChannelDelete([]string{"--id", idArg}); err != nil {
			t.Fatalf("再删一次不该报错（幂等），得到 %v", err)
		}
	})
	if !strings.Contains(out, "已经是删除状态") {
		t.Errorf("重复删除的输出不对：\n%s", out)
	}

	// 三、不存在的 id 仍然报错，退出码走「要改的那一行不存在」。
	err := runCaptured(t, func() error {
		return runChannelDelete([]string{"--id", "999999999"})
	})
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Fatalf("删不存在的 id 应当报 ErrChannelNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("删不存在的 id 的退出码 = %d，期望 %d", got, exitConflict)
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
	if strings.ContainsRune(stdout, escByte) {
		t.Errorf("stdout 里有 ANSI 转义（0x%x）:\n%s", escByte, stdout)
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
	assertJSONField(t, got, "status", jsonOK)

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
		assertJSONField(t, empty, "status", jsonOK)
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
	withFormat(t, formatJSON)

	cases := []struct {
		command string
		run     func() error
	}{
		{"channel create", func() error { return runChannelCreate(nil) }},
		{"channel update", func() error { return runChannelUpdate(nil) }},
		{"channel enable", func() error {
			return runChannelSetStatus(nil, channels.StatusEnabled)
		}},
		{"channel disable", func() error {
			return runChannelSetStatus(nil, channels.StatusDisabled)
		}},
		{"channel delete", func() error { return runChannelDelete(nil) }},
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

// TestChannelListJSONStdoutIsPure 拿真命令跑一遍 json 那一档。
//
// 文档的组装函数是纯函数（上面那条钉住了它的形状），而这一条钉的是**命令
// 那一层**：stdout 上从头到尾只有一份 JSON、没有表格文本、没有转义序列。
func TestChannelListJSONStdoutIsPure(t *testing.T) {
	_ = testDB(t)
	setupEnv(t)

	withFormat(t, formatJSON)
	stdout, _ := captureStreams(t, func() {
		if err := runChannelList(nil); err != nil {
			t.Fatalf("channel list 失败: %v", err)
		}
	})

	got := assertJSONStdout(t, stdout)
	assertJSONField(t, got, "status", jsonOK)
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
