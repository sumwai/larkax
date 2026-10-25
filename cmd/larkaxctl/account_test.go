package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channels"
)

// ── 参数解析（不连库） ──────────────────────────────────────────────────

// TestParseAccountAdd 钉住 add 的参数：位置参数、--channel、--weight。
//
// --weight 用指针传下去，所以「没给」与「给了 0」必须是两种结果：0 是合法的
// 「先别用它」，缺省是 1。分不出来时，用户写 `--weight 0` 会被悄悄改成 1，
// 而那条渠道会继续往一个他明确停掉的 key 上发流量。
func TestParseAccountAdd(t *testing.T) {
	zero, two := 0, 2
	cases := []struct {
		name        string
		args        []string
		wantName    string
		wantChannel string
		wantWeight  *int
		wantErr     string
	}{
		{"只给名字", []string{"main"}, "main", "", nil, ""},
		{"名字在前、选项在后", []string{"main", "--channel", "deepseek-official", "--weight", "2"},
			"main", "deepseek-official", &two, ""},
		{"只给渠道", []string{"--channel", "7"}, "", "7", nil, ""},
		{"选项在前、名字在后", []string{"--channel", "7", "main"}, "main", "7", nil, ""},
		{"权重 0 是合法的", []string{"main", "--channel", "7", "--weight", "0"},
			"main", "7", &zero, ""},
		{"权重没给渠道", []string{"main", "--weight", "2"}, "", "", nil, "--weight"},
		{"权重为负", []string{"main", "--channel", "7", "--weight", "-1"}, "", "", nil, "不能为负"},
		{"两个名字", []string{"a", "b"}, "", "", nil, "只收一个账号名"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStdin(t, "sk-test")
			name, channelRef, weight, plaintext, err := parseAccountAdd(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %v，期望包含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if name != tc.wantName || channelRef != tc.wantChannel {
				t.Errorf("name/channel = %q/%q，期望 %q/%q",
					name, channelRef, tc.wantName, tc.wantChannel)
			}
			switch {
			case tc.wantWeight == nil && weight != nil:
				t.Errorf("没给 --weight 时应当传 nil，得到 %d", *weight)
			case tc.wantWeight != nil && weight == nil:
				t.Errorf("期望权重 %d，得到 nil", *tc.wantWeight)
			case tc.wantWeight != nil && *weight != *tc.wantWeight:
				t.Errorf("权重 = %d，期望 %d", *weight, *tc.wantWeight)
			}
			if plaintext != "sk-test" {
				t.Errorf("凭证 = %q，期望从标准输入读到的那一份", plaintext)
			}
		})
	}
}

// TestParseAccountRemove 钉住 remove 的参数：一个必填的账号名与 --force。
func TestParseAccountRemove(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantName  string
		wantForce bool
		wantErr   string
	}{
		{"只给名字", []string{"main"}, "main", false, ""},
		{"带 --force", []string{"main", "--force"}, "main", true, ""},
		{"选项在前", []string{"--force", "main"}, "main", true, ""},
		{"没有名字", nil, "", false, "缺少账号名"},
		{"两个名字", []string{"a", "b"}, "", false, "只收一个账号名"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, force, err := parseAccountRemove(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %v，期望包含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if name != tc.wantName || force != tc.wantForce {
				t.Errorf("name/force = %q/%v，期望 %q/%v",
					name, force, tc.wantName, tc.wantForce)
			}
		})
	}
}

// TestAccountHelpCoversRemoveAndWeight 钉住帮助里写了 remove / --weight。
//
// 短版与长版各查一遍：短版是参数写错时唯一会看到的那一页，长版是 `help account`
// 那一页。两处都漏掉时，用户不会知道有 --force 这条路。
func TestAccountHelpCoversRemoveAndWeight(t *testing.T) {
	for name, text := range map[string]string{
		"短版": accountShortUsage,
		"长版": accountUsage,
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{"account remove", "--weight", "--channel"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s里没有 %q", name, want)
				}
			}
		})
	}

	// 长版要说清两件行为的后果：还挂着别的渠道时要 --force，在飞请求不受影响。
	for _, want := range []string{"--force", "正在进行的请求不受影响"} {
		if !strings.Contains(accountUsage, want) {
			t.Errorf("长版里没有 %q", want)
		}
	}

	// 动作清单里要有 remove：补全与「动作都有分派」都读它（见 completion_test.go）。
	found := false
	for _, action := range accountActions {
		if action == "remove" {
			found = true
		}
	}
	if !found {
		t.Errorf("accountActions 里没有 remove：%v", accountActions)
	}
}

// ── 连库的路径 ──────────────────────────────────────────────────────────

// accountWeightOn 读一个账号挂在某条渠道上的权重。
func accountWeightOn(t *testing.T, db *sql.DB, channelID int64, accountName string) (weight int, found bool) {
	t.Helper()
	err := db.QueryRowContext(context.Background(), `
		SELECT ca.weight
		  FROM channel_accounts ca
		  JOIN accounts a ON a.id = ca.account_id
		 WHERE ca.channel_id = $1 AND a.name = $2`, channelID, accountName).Scan(&weight)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读账号权重失败: %v", err)
	}
	return weight, true
}

// TestAccountAddWithWeight 钉住 `account add --channel X --weight 2` 的落库。
//
// 权重挂在**关联**上（迁移 00067），不是 accounts 上的一列：同一个账号挂到
// 两条渠道上时可以各算各的。这条用例读的就是关联表。
func TestAccountAddWithWeight(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID := createTestChannel(t, uniqueName("channel"), uniqueName("model"))

	withStdin(t, "sk-pool-key")
	stdout, stderr := captureStreams(t, func() {
		if err := runAccountAdd([]string{"--channel", strconv.FormatInt(channelID, 10), "--weight", "2"}); err != nil {
			t.Fatalf("account add 失败: %v", err)
		}
	})
	name := strings.TrimSpace(stdout)
	if name == "" {
		t.Fatal("stdout 里没有账号名")
	}
	if !strings.Contains(stderr, "已挂到渠道") {
		t.Errorf("stderr 没提到挂到渠道：%q", stderr)
	}

	weight, found := accountWeightOn(t, db, channelID, name)
	if !found {
		t.Fatalf("账号 %s 没有挂到渠道 %d 上", name, channelID)
	}
	if weight != 2 {
		t.Errorf("权重 = %d，期望 2", weight)
	}

	// 不给 --weight 时是 1（关联列的默认值），而不是 0——0 的意思是「先别用它」。
	withStdin(t, "sk-pool-key-2")
	stdout, _ = captureStreams(t, func() {
		if err := runAccountAdd([]string{"--channel", strconv.FormatInt(channelID, 10)}); err != nil {
			t.Fatalf("account add 失败: %v", err)
		}
	})
	plain, found := accountWeightOn(t, db, channelID, strings.TrimSpace(stdout))
	if !found {
		t.Fatal("第二个账号没有挂到渠道上")
	}
	if plain != 1 {
		t.Errorf("没给 --weight 时权重 = %d，期望 1", plain)
	}
}

// TestAccountListFiltersByChannel 钉住 `account list --channel X`。
//
// 只列挂在**那条渠道**上的账号：没挂任何渠道的账号不该出现在这里（它还在
// 全量清单里——那条清单是「库里有谁」，这条是「这条渠道上有谁」）。
func TestAccountListFiltersByChannel(t *testing.T) {
	testDB(t)
	setupEnv(t)
	channelID := createTestChannel(t, uniqueName("channel"), uniqueName("model"))

	withStdin(t, "sk-on-channel")
	stdout, _ := captureStreams(t, func() {
		if err := runAccountAdd([]string{"--channel", strconv.FormatInt(channelID, 10)}); err != nil {
			t.Fatalf("account add 失败: %v", err)
		}
	})
	onChannel := strings.TrimSpace(stdout)

	withStdin(t, "sk-orphan")
	stdout, _ = captureStreams(t, func() {
		if err := runAccountAdd(nil); err != nil {
			t.Fatalf("account add 失败: %v", err)
		}
	})
	orphan := strings.TrimSpace(stdout)

	// 不过滤：两条都在。
	all, _ := captureStreams(t, func() {
		if err := runAccountList(nil); err != nil {
			t.Fatalf("account list 失败: %v", err)
		}
	})
	for _, want := range []string{onChannel, orphan} {
		if !strings.Contains(all, want) {
			t.Errorf("全量清单里没有 %s：\n%s", want, all)
		}
	}

	// 按渠道过滤：只有挂在那条渠道上的那条。
	filtered, _ := captureStreams(t, func() {
		if err := runAccountList([]string{"--channel", strconv.FormatInt(channelID, 10)}); err != nil {
			t.Fatalf("account list --channel 失败: %v", err)
		}
	})
	if !strings.Contains(filtered, onChannel) {
		t.Errorf("按渠道过滤的清单里没有 %s：\n%s", onChannel, filtered)
	}
	if strings.Contains(filtered, orphan) {
		t.Errorf("没挂在这条渠道上的账号 %s 不该出现：\n%s", orphan, filtered)
	}
}

// TestAccountListRejectsUnknownChannel 钉住「渠道名认不出来时报错，不是空清单」。
//
// 空清单与「这条渠道一个账号都没有」长得一样，而后者是一个正常状态——把
// 拼错的渠道名当成空清单，用户会去挂一个新账号，而不是改对名字。
func TestAccountListRejectsUnknownChannel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runAccountList([]string{"--channel", uniqueName("no-such-channel")})
	})
	if err == nil {
		t.Fatal("渠道不存在时应当报错，却通过了")
	}
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Fatalf("期望 ErrChannelNotFound，得到 %v", err)
	}
}

// TestAccountRemoveSingleChannel 钉住「只挂在一条渠道上时直接删」。
//
// 删完那条渠道一个账号都不剩，所以 stderr 必须把这件事说出来：零账号的渠道
// 会退出选路，而「删掉一个账号」与「有一条渠道因此下线」不是同一个后果。
func TestAccountRemoveSingleChannel(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID := createTestChannel(t, uniqueName("channel"), uniqueName("model"))

	// channel create 会顺手建一个匿名账号挂上去。先把它删掉，让这条渠道只剩
	// 这一次要删的那个——否则「删完这条渠道空了」那一句根本不会出现，而它正是
	// 本条用例要钉的东西。
	var autoName string
	if err := db.QueryRowContext(context.Background(), `
		SELECT a.name
		  FROM channel_accounts ca JOIN accounts a ON a.id = ca.account_id
		 WHERE ca.channel_id = $1`, channelID).Scan(&autoName); err != nil {
		t.Fatalf("读建渠道时挂上的账号失败: %v", err)
	}
	if err := runCaptured(t, func() error { return runAccountRemove([]string{autoName}) }); err != nil {
		t.Fatalf("删掉匿名账号失败: %v", err)
	}

	withStdin(t, "sk-to-remove")
	stdout, _ := captureStreams(t, func() {
		if err := runAccountAdd([]string{"--channel", strconv.FormatInt(channelID, 10)}); err != nil {
			t.Fatalf("account add 失败: %v", err)
		}
	})
	name := strings.TrimSpace(stdout)

	// account remove 的 stdout 必须是空的：没有新值要被接走。
	out, errOut := captureStreams(t, func() {
		if err := runAccountRemove([]string{name}); err != nil {
			t.Fatalf("account remove 失败: %v", err)
		}
	})
	if out != "" {
		t.Errorf("remove 的 stdout = %q，期望空", out)
	}
	if !strings.Contains(errOut, "已删除账号 "+name) {
		t.Errorf("stderr 没说删了谁：%q", errOut)
	}
	if !strings.Contains(errOut, "现在没有账号了") {
		t.Errorf("stderr 没说那条渠道变空了：%q", errOut)
	}

	// 账号行与关联行都不在了。
	var rows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM accounts WHERE name = $1`, name).Scan(&rows); err != nil {
		t.Fatalf("数账号行失败: %v", err)
	}
	if rows != 0 {
		t.Error("账号行还在库里")
	}
}

// TestAccountRemoveInUseNeedsForce 钉住「还挂在别的渠道上时要 --force」。
//
// 退出码走参数错那一档（2）：补救动作是改命令重发，而不是去看库里的数据。
func TestAccountRemoveInUseNeedsForce(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	first := createTestChannel(t, uniqueName("channel"), uniqueName("model"))
	second := createTestChannel(t, uniqueName("channel"), uniqueName("model"))

	withStdin(t, "sk-shared")
	stdout, _ := captureStreams(t, func() {
		if err := runAccountAdd([]string{"--channel", strconv.FormatInt(first, 10)}); err != nil {
			t.Fatalf("account add 失败: %v", err)
		}
	})
	name := strings.TrimSpace(stdout)

	// 再挂到第二条渠道上：同一个账号两处生效。
	var accountID int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT id FROM accounts WHERE name = $1`, name).Scan(&accountID); err != nil {
		t.Fatalf("查账号 id 失败: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_accounts (channel_id, account_id, weight) VALUES ($1, $2, 1)`,
		second, accountID); err != nil {
		t.Fatalf("挂到第二条渠道上失败: %v", err)
	}

	err := runCaptured(t, func() error { return runAccountRemove([]string{name}) })
	if !errors.Is(err, channels.ErrAccountInUse) {
		t.Fatalf("期望 ErrAccountInUse，得到 %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d（改命令加 --force 就能过）", got, exitUsage)
	}
	if !strings.Contains(err.Error(), secondName(t, db, second)) {
		t.Errorf("错误里没带回另一条渠道的名字：%v", err)
	}

	// 加了 --force 之后照删。
	if err := runCaptured(t, func() error { return runAccountRemove([]string{name, "--force"}) }); err != nil {
		t.Fatalf("--force 时应当成功，得到 %v", err)
	}
	var rows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channel_accounts WHERE account_id = $1`, accountID).Scan(&rows); err != nil {
		t.Fatalf("数关联行失败: %v", err)
	}
	if rows != 0 {
		t.Errorf("关联行还剩 %d 条", rows)
	}
}

// secondName 读一条渠道的名字，供错误文案的断言用。
func secondName(t *testing.T, db *sql.DB, channelID int64) string {
	t.Helper()
	var name string
	if err := db.QueryRowContext(context.Background(),
		`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&name); err != nil {
		t.Fatalf("读渠道名失败: %v", err)
	}
	return name
}

// TestAccountUpdateMissing 钉住「换 key 时名字打错」的退出码也是 4。
//
// ErrAccountNotFound 在 conflictErrors 里（见 exit.go），所以 update / remove
// 两条路都落在「要查的那一行不存在」那一档。update 先有这个行为、后来才被
// 记成用例：没有它时，把那条分类去掉照样全绿，而用户拿到的会是 1。
func TestAccountUpdateMissing(t *testing.T) {
	testDB(t)
	setupEnv(t)

	withStdin(t, "sk-nowhere")
	err := runCaptured(t, func() error {
		return runAccountUpdate([]string{uniqueName("no-such-account")})
	})
	if !errors.Is(err, channels.ErrAccountNotFound) {
		t.Fatalf("期望 ErrAccountNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
	}
}

// TestAccountRemoveMissing 钉住「名字打错时的退出码」。
//
// 走「要查的那一行不存在」那一档（4），不是参数错：补救动作是先跑一遍 list
// 看库里现在有什么。
func TestAccountRemoveMissing(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runAccountRemove([]string{uniqueName("no-such-account")})
	})
	if !errors.Is(err, channels.ErrAccountNotFound) {
		t.Fatalf("期望 ErrAccountNotFound，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
	}
}

// TestAccountRemoveRejectsBadArgs 钉住参数错的几条路：都该在连库之前报出来。
func TestAccountRemoveRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有名字", nil, "缺少账号名"},
		{"两个名字", []string{"a", "b"}, "只收一个账号名"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runAccountRemove(tc.args) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
			}
		})
	}
}

// TestAccountAddResolvesChannelByName 钉住 `account add --channel` 认名字。
//
// 它是 channels.Admin.ChannelIDByRef 的既有调用点：channel map / unmap 也走
// 同一个函数，所以这条同时是「名字解析在这条老路径上没有退化」的证据。
// 落空那一半断两件事：
//
//   - 错误链里仍有 channels.ErrChannelNotFound —— exit.go 按这个哨兵把它归进
//     「约束冲突」那一档，包装丢了会静默改掉退出码；
//   - exitCode 是**字面量 4**。不从 exitConflict 常量取期望值：那是拿被测代码
//     自己的表去比自己。
func TestAccountAddResolvesChannelByName(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	channelName := uniqueName("account-channel")
	channelID := createMapChannel(t, channelName, uniqueName("seed-model"))
	accountName := uniqueName("account")

	withStdin(t, testSecret)
	if err := runCaptured(t, func() error {
		return runAccount([]string{"add", accountName, "--channel", channelName})
	}); err != nil {
		t.Fatalf("account add --channel <名字> 失败: %v", err)
	}

	accounts, err := channels.NewAdmin(db, nil, nil).ListAccounts(context.Background(), 0) // 0 = 不按渠道过滤
	if err != nil {
		t.Fatalf("列举账号失败: %v", err)
	}
	var channelIDs []int64
	found := false
	for _, account := range accounts {
		if account.Name != accountName {
			continue
		}
		found = true
		for _, ch := range account.Channels {
			channelIDs = append(channelIDs, ch.ID)
		}
	}
	if !found {
		t.Fatalf("账号列表里没有刚建的 %q", accountName)
	}
	if len(channelIDs) != 1 || channelIDs[0] != channelID {
		t.Fatalf("账号 %q 挂的渠道 = %v，期望 [%d]", accountName, channelIDs, channelID)
	}

	withStdin(t, testSecret)
	var addErr error
	_, _ = captureStreams(t, func() {
		addErr = runAccount([]string{"add", uniqueName("account"), "--channel", uniqueName("no-such-channel")})
	})
	if addErr == nil {
		t.Fatal("不存在的渠道名应当被拒")
	}
	if !errors.Is(addErr, channels.ErrChannelNotFound) {
		t.Fatalf("错误链里没有 channels.ErrChannelNotFound：%v", addErr)
	}
	if got := exitCode(addErr); got != 4 {
		t.Fatalf("渠道名找不到的退出码 = %d，期望 4（%v）", got, addErr)
	}
}
