package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"

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
