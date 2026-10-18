package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
)

// TestUserCreateRejectsBadFlags 走的是**不碰数据库**的那几条路径。
//
// 与 apikey create 同一条约定：参数检查必须发生在连库之前。
// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
// 还有一个更坏的可能——它没报错。
//
// 「不碰库」也是这些用例能在没有 DSN 的机器上跑起来的原因：
// 它们一旦开始碰库，就会在本地通过、在 CI 上跳过，而「跳过」在输出里
// 和「通过」长得一样。
func TestUserCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --name", []string{}, "缺少 --name"},
		{"--name 为空", []string{"--name", ""}, "缺少 --name"},
		{"--name 只有空白", []string{"--name", "   "}, "缺少 --name"},
		{"多出位置参数", []string{"--name", "alice", "extra"}, "不接受位置参数"},
		{"--role 不认识", []string{"--name", "alice", "--role", "root"}, "未知的角色"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runUserCreate(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestUserUnknownSubcommand(t *testing.T) {
	err := runUser([]string{})
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runUser([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 user 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestPrintCreatedUserWritesOnlyIDToStdout 钉住输出分流这件事。
//
// `larkaxctl apikey create --user "$(larkaxctl user create --name alice)"`
// 是造完用户之后最自然的下一步。stdout 里一旦混进提示语，命令替换拿到的
// 就不是用户标识，而那条命令会报「找不到用户」——断点离真正的原因很远。
// 所以「stdout 只有 ID」是一条契约，不是排版偏好。
func TestPrintCreatedUserWritesOnlyIDToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer

	printCreatedUser(&stdout, &stderr, auth.User{ID: 7, Username: "user-placeholder"})

	if got, want := stdout.String(), "7\n"; got != want {
		t.Errorf("stdout = %q，期望只有 ID 加一个换行 %q", got, want)
	}
	if !strings.Contains(stderr.String(), "user-placeholder") {
		t.Errorf("stderr 里没有用户名，实际内容：\n%s", stderr.String())
	}
}

// ── user list / user get ────────────────────────────────────────────────

// TestUserListRejectsBadFilters 走的是**不碰数据库**的那几条路径。
//
// 筛选条件不合法时必须当场报错，不回落成「不筛」：回落的表现是打出一份看起来
// 正常、其实一行都没筛过的清单，而它照样退出 0——那时读的人会以为筛过了。
func TestUserListRejectsBadFilters(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"--status 不认识", []string{"--status", "frozen"}, "未知的用户状态"},
		{"--role 不认识", []string{"--role", "root"}, "未知的角色"},
		{"多出位置参数", []string{"alice"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runUserList(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// TestUserGetRejectsBadFlags 同上：缺标识与多参数都在连库之前拦下。
func TestUserGetRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", nil, "缺少 --user"},
		{"--user 只有空白", []string{"-u", "   "}, "缺少 --user"},
		{"多出位置参数", []string{"--user", "1", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runUserGet(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// userRow 在清单里按用户名找到那一行，返回它的各栏。
//
// 表格是按显示宽度对齐的，栏与栏之间是空格填充，所以按 Fields 切开就是各栏的
// 值——不去数列宽，那样一改表格就整条用例跟着坏。
func userRow(t *testing.T, stdout, username string) []string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		cells := strings.Fields(line)
		if len(cells) >= 2 && cells[1] == username {
			return cells
		}
	}
	t.Fatalf("清单里没有 %q 那一行：\n%s", username, stdout)
	return nil
}

// TestUserListShowsEveryColumn 过真命令、连真库：刚建的用户在清单里出现，
// 五栏逐字是库里那一行的值。
func TestUserListShowsEveryColumn(t *testing.T) {
	setupEnv(t)

	name := uniqueName("listed")
	email := name + "@example.com"
	id := createUserWithRole(t, name, email, "merchant")

	r := runOnce(t, runUser, []string{"list"})
	if r.err != nil {
		t.Fatalf("user list 失败: %v\nstderr=%s", r.err, r.stderr)
	}

	got := userRow(t, r.stdout, name)
	want := []string{strconv.FormatInt(id, 10), name, "active", "merchant", email}
	if !slices.Equal(got, want) {
		t.Errorf("这一行是 %q，想要 %q", got, want)
	}
}

// TestUserListRoleFilterMatchesTheRoleColumn 钉住筛选项与清单里那一栏是同一套映射。
//
// 三档角色各建一个用户，然后按每一档筛一遍：被筛出来的每一行，它那一栏必须
// 正好是筛的那个角色。两个方向都要看——只断言「筛出来的行对」的话，一个
// 什么都不筛的实现照样通过；只断言「别的角色没被筛出来」的话，一个筛不出
// 任何东西的实现也照样通过。
func TestUserListRoleFilterMatchesTheRoleColumn(t *testing.T) {
	setupEnv(t)

	want := map[string]string{}
	for _, role := range []string{"user", "merchant", "admin"} {
		name := uniqueName("role-" + role)
		createUserWithRole(t, name, "", role)
		want[name] = role
	}

	for _, role := range []string{"user", "merchant", "admin"} {
		t.Run(role, func(t *testing.T) {
			r := runOnce(t, runUser, []string{"list", "--role", role})
			if r.err != nil {
				t.Fatalf("user list --role %s 失败: %v", role, r.err)
			}

			seen := 0
			for _, line := range strings.Split(r.stdout, "\n") {
				cells := strings.Fields(line)
				if len(cells) < 4 {
					continue
				}
				got, ok := want[cells[1]]
				if !ok {
					continue
				}
				seen++
				if cells[3] != role {
					t.Errorf("--role %s 筛出了 %s（角色栏是 %s）", role, cells[1], cells[3])
				}
				if got != role {
					t.Errorf("用例自己造错了：%s 建的时候是 %s", cells[1], got)
				}
			}
			// 三个用户各属于一档，所以每一档恰好筛出一个。
			if seen != 1 {
				t.Errorf("--role %s 筛出 %d 个本用例建的用户，想要 1 个：\n%s",
					role, seen, r.stdout)
			}
		})
	}
}

// TestUserListStatusFilterSelectsOnlyThatStatus 钉住 --status 真的接在 SQL 上。
//
// 夹具直接用 SQL 把两个用户改成不同的状态，不走 `user update --status`：这条用例
// 测的是筛，夹具借道另一条命令时，红的那一次分不清是筛坏了还是改坏了。
func TestUserListStatusFilterSelectsOnlyThatStatus(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	active := uniqueName("status-active")
	banned := uniqueName("status-banned")
	createUserWithRole(t, active, "", "user")
	bannedID := createUserWithRole(t, banned, "", "user")

	if _, err := db.ExecContext(context.Background(),
		`UPDATE users SET status = $1 WHERE id = $2`, auth.UserStatusBanned, bannedID); err != nil {
		t.Fatalf("改用户状态失败: %v", err)
	}

	r := runOnce(t, runUser, []string{"list", "--status", auth.UserStatusBanned})
	if r.err != nil {
		t.Fatalf("user list --status banned 失败: %v", r.err)
	}

	got := userRow(t, r.stdout, banned)
	if got[2] != auth.UserStatusBanned {
		t.Errorf("状态栏是 %q，想要 %q", got[2], auth.UserStatusBanned)
	}
	if strings.Contains(r.stdout, active) {
		t.Errorf("--status banned 里不该出现 %s：\n%s", active, r.stdout)
	}
}

// TestUserGetResolvesIDAndUsernameToTheSameUser 钉住两条标识走的是同一处解析。
//
// 同一个人按 ID 与按用户名各读一次，两次必须是同一行：两条路各写一份解析的话，
// 它们迟早会指向不同的人，而那两条命令都会成功。
func TestUserGetResolvesIDAndUsernameToTheSameUser(t *testing.T) {
	setupEnv(t)

	name := uniqueName("get")
	id := createUserWithRole(t, name, name+"@example.com", "admin")

	byID := runOnce(t, runUser, []string{"get", "--user", strconv.FormatInt(id, 10)})
	if byID.err != nil {
		t.Fatalf("user get --user <id> 失败: %v\nstderr=%s", byID.err, byID.stderr)
	}
	byName := runOnce(t, runUser, []string{"get", "-u", name})
	if byName.err != nil {
		t.Fatalf("user get -u <名字> 失败: %v\nstderr=%s", byName.err, byName.stderr)
	}
	if byID.stdout != byName.stdout {
		t.Errorf("同一个人的两次详情不同：\n按 ID：\n%s\n按名字：\n%s", byID.stdout, byName.stdout)
	}

	for _, want := range []string{
		strconv.FormatInt(id, 10), name, "active", "admin", name + "@example.com",
	} {
		if !strings.Contains(byID.stdout, want) {
			t.Errorf("详情里没有 %q：\n%s", want, byID.stdout)
		}
	}
}

// TestUserGetMissingUserIsAConflict 钉住「要查的那一行不在」的退出码。
//
// 它与「连不上库」必须分开：前者的下一步是换一个标识或先建用户，后者是去看
// 报错本身。两者落进同一个码时，脚本没法按码分派。
func TestUserGetMissingUserIsAConflict(t *testing.T) {
	setupEnv(t)

	r := runOnce(t, runUser, []string{"get", "--user", uniqueName("nobody")})
	if r.err == nil {
		t.Fatal("读一个不存在的用户应当报错")
	}
	if !errors.Is(r.err, auth.ErrUserNotFound) {
		t.Errorf("错误 %v 不是 ErrUserNotFound", r.err)
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，想要 %d", code, exitConflict)
	}
}

// TestUserListJSONShape 过真命令断言机器可读那一档：金额之外的那几栏、
// 空邮箱是 null、以及 stdout 整份能被 json.Unmarshal 收下。
func TestUserListJSONShape(t *testing.T) {
	setupEnv(t)

	name := uniqueName("json")
	id := createUserWithRole(t, name, "", "user")

	withFormat(t, formatJSON)
	r := runOnce(t, runUser, []string{"list"})
	if r.err != nil {
		t.Fatalf("user list -o json 失败: %v", r.err)
	}
	assertOnlyJSON(t, r.stdout)

	var got struct {
		Status string `json:"status"`
		Users  []struct {
			ID       int64   `json:"id"`
			Username string  `json:"username"`
			Status   string  `json:"status"`
			Role     string  `json:"role"`
			Email    *string `json:"email"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}

	var found bool
	for _, u := range got.Users {
		if u.ID != id {
			continue
		}
		found = true
		if u.Username != name || u.Status != auth.UserStatusActive || u.Role != "user" {
			t.Errorf("那一行是 %+v", u)
		}
		// 没填邮箱的用户在库里存的是 NULL，这一档必须给 null 而不是 ""：
		// 空串是一个具体的值，脚本分不出它与「没填」。
		if u.Email != nil {
			t.Errorf("没填过的邮箱应当是 null，得到 %q", *u.Email)
		}
	}
	if !found {
		t.Fatalf("清单里没有 id=%d 那一行：\n%s", id, r.stdout)
	}
}

// TestUserGetJSONShape 断言 get 那一档的字段名与形状。
func TestUserGetJSONShape(t *testing.T) {
	setupEnv(t)

	name := uniqueName("jsonget")
	id := createUserWithRole(t, name, name+"@example.com", "merchant")

	withFormat(t, formatJSON)
	r := runOnce(t, runUser, []string{"get", "--user", strconv.FormatInt(id, 10)})
	if r.err != nil {
		t.Fatalf("user get -o json 失败: %v", r.err)
	}
	assertOnlyJSON(t, r.stdout)

	var got struct {
		User struct {
			ID          int64   `json:"id"`
			Username    string  `json:"username"`
			Status      string  `json:"status"`
			Role        string  `json:"role"`
			Email       string  `json:"email"`
			DisplayName *string `json:"display_name"`
			CreatedAt   string  `json:"created_at"`
			UpdatedAt   string  `json:"updated_at"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}

	u := got.User
	if u.ID != id || u.Username != name || u.Role != "merchant" ||
		u.Email != name+"@example.com" {
		t.Errorf("详情对不上：%+v", u)
	}
	// 没填过的展示名是 null；两个时刻必须是能解析的 RFC3339。
	if u.DisplayName != nil {
		t.Errorf("没填过的展示名应当是 null，得到 %q", *u.DisplayName)
	}
	for _, at := range []string{u.CreatedAt, u.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			t.Errorf("时刻 %q 不是 RFC3339：%v", at, err)
		}
	}
}

// TestUserListEmptyJSONIsAnArray 钉住空清单在机器可读那一档是 `[]` 而不是 null。
//
// nil 切片编出来是 null，读起来像「这一栏没有」；空切片才是「这一栏有，一条
// 都没有」（见 format.go 的 jsonList）。库里清空不了（同一个包别的用例正在建
// 用户），所以直接喂 nil 给组装文档的那个纯函数——它正是决定这个形状的地方。
func TestUserListEmptyJSONIsAnArray(t *testing.T) {
	doc := userListDoc(nil)

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("编不出来: %v", err)
	}
	var got struct {
		Users []map[string]any `json:"users"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, raw)
	}
	if got.Users == nil {
		t.Errorf("空清单编成了 null，想要 []：%s", raw)
	}
	if len(got.Users) != 0 || got.Total != 0 {
		t.Errorf("空清单应当是 0 条，得到 %d 条（total=%d）", len(got.Users), got.Total)
	}
}

// createUserWithRole 建一个用户并返回它的 ID，邮箱与角色可以留空。
//
// 与 createTestUser 的区别只有一处：那两个字段也要能指定。名字仍然由调用方
// 保证唯一（user create 不做幂等）。
func createUserWithRole(t *testing.T, name, email, role string) int64 {
	t.Helper()

	args := []string{"--name", name, "--role", role}
	if email != "" {
		args = append(args, "--email", email)
	}
	stdout, _ := captureStreams(t, func() {
		if err := runUserCreate(args); err != nil {
			t.Fatalf("user create %s 失败: %v", name, err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是用户 ID：%q（%v）", stdout, err)
	}
	return id
}

// ── user update ────────────────────────────────────────────────────────

// describeParams 把折出来的参数渲染成一行，用作出错时的对照。
//
// 不用 %+v：那几个字段都是指针，格式化出来是地址——地址对不上时看不出差的是哪
// 一格，更看不出差在「没给」（nil）还是「给了假」。
func describeParams(p auth.UpdateUserParams) string {
	var parts []string
	text := func(label string, v *string) {
		if v != nil {
			parts = append(parts, fmt.Sprintf("%s=%q", label, *v))
		}
	}
	flag := func(label string, v *bool) {
		if v != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", label, *v))
		}
	}
	text("status", p.Status)
	text("email", p.Email)
	text("display_name", p.DisplayName)
	flag("is_merchant", p.IsMerchant)
	flag("is_admin", p.IsAdmin)
	return strings.Join(parts, " ")
}

// TestUserUpdateParamsMapsFlagsToColumns 是四条标记旗标的判据。
//
// 它不碰数据库：折旗标这一步在连库之前，也正是「一个要改的字段都没给」与那三组
// 矛盾输入要当场报错的地方。逐格钉的是**指针的 nil**，不是布尔值——「没给这一列」
// 必须落成 nil，落成 false 就等于每次 update 都顺手把那一列改掉。
func TestUserUpdateParamsMapsFlagsToColumns(t *testing.T) {
	cases := []struct {
		name  string
		seen  []string
		flags userUpdateFlags
		want  string
	}{
		{"--merchant", []string{"merchant"}, userUpdateFlags{merchant: true}, "is_merchant=true"},
		{"--no-merchant", []string{"no-merchant"}, userUpdateFlags{noMerchant: true}, "is_merchant=false"},
		// admin 天然也是 merchant：这一条是两列，取值取自 auth 的 Role.Flags。
		{"--admin", []string{"admin"}, userUpdateFlags{admin: true}, "is_merchant=true is_admin=true"},
		// --no-admin 只动 is_admin：merchant 那一栏根本不出现。
		{"--no-admin", []string{"no-admin"}, userUpdateFlags{noAdmin: true}, "is_admin=false"},
		{"--admin 与 --merchant 一起给（同一个意思）", []string{"admin", "merchant"},
			userUpdateFlags{admin: true, merchant: true}, "is_merchant=true is_admin=true"},
		{"--merchant 之后撤 admin", []string{"merchant", "no-admin"},
			userUpdateFlags{merchant: true, noAdmin: true}, "is_merchant=true is_admin=false"},
		{"--status", []string{"status"}, userUpdateFlags{status: "banned"}, `status="banned"`},
		// 状态两头空白会被去掉，取值本身照旧要认识——与 --role 同一口径
		// （ParseRole 也 trim，那边有 "  admin  " 一条用例）。
		{"--status 两头空白", []string{"status"}, userUpdateFlags{status: " banned "},
			`status="banned"`},
		// 空串是**清空**，不是「没给」：两者取值都是空串，只有 seen 分得开。
		{"--display-name 空串", []string{"display-name"},
			userUpdateFlags{displayName: ""}, `display_name=""`},
		{"--email 空串", []string{"email"}, userUpdateFlags{email: ""}, `email=""`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, name := range tc.seen {
				seen[name] = true
			}

			got, err := userUpdateParams(seen, tc.flags)
			if err != nil {
				t.Fatalf("应当通过，却报错: %v", err)
			}
			if rendered := describeParams(got); rendered != tc.want {
				t.Errorf("折出来的列 = %q，期望 %q", rendered, tc.want)
			}
		})
	}
}

// TestUserUpdateParamsRejectsContradictions 钉住三组互相打架的输入。
//
// 挑一个落库比报错省事，但那样命令照样退出 0，而库里那一行是不是想要的样子只能
// 再读一遍才知道——「改身份」这类操作不该有这种结果。
func TestUserUpdateParamsRejectsContradictions(t *testing.T) {
	cases := []struct {
		name  string
		seen  []string
		flags userUpdateFlags
		want  string
	}{
		{"--merchant 与 --no-merchant", []string{"merchant", "no-merchant"},
			userUpdateFlags{merchant: true, noMerchant: true}, "--merchant 与 --no-merchant"},
		{"--admin 与 --no-admin", []string{"admin", "no-admin"},
			userUpdateFlags{admin: true, noAdmin: true}, "--admin 与 --no-admin"},
		// 这一组是「--admin 连 merchant 一起写」的另一面：一个要置真、一个要置假。
		{"--admin 与 --no-merchant", []string{"admin", "no-merchant"},
			userUpdateFlags{admin: true, noMerchant: true}, "--admin 与 --no-merchant 矛盾"},
		{"一个字段都没给", nil, userUpdateFlags{}, "一个要改的字段都没给"},
		{"--status 不认识", []string{"status"}, userUpdateFlags{status: "frozen"}, "未知的用户状态"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, name := range tc.seen {
				seen[name] = true
			}

			_, err := userUpdateParams(seen, tc.flags)
			if err == nil {
				t.Fatalf("应当报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestUserUpdateRejectsBadFlags 走的是**不碰数据库**的那几条路径。
//
// 与 create 同一条约定：参数检查必须发生在连库之前。这里连 DSN 都没设，
// 所以「先连库再报错」的实现会以另一种失败收场（连不上库），而不是这些文案。
func TestUserUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", []string{"--display-name", "x"}, "缺少 --user"},
		{"--user 只有空白", []string{"--user", "   ", "--display-name", "x"}, "缺少 --user"},
		{"一个字段都没给", []string{"--user", "alice"}, "一个要改的字段都没给"},
		{"多出位置参数", []string{"--user", "alice", "extra", "--display-name", "x"}, "不接受位置参数"},
		{"--merchant 与 --no-merchant", []string{"--user", "alice", "--merchant", "--no-merchant"},
			"--merchant 与 --no-merchant"},
		{"--admin 与 --no-admin", []string{"--user", "alice", "--admin", "--no-admin"},
			"--admin 与 --no-admin"},
		{"--admin 与 --no-merchant", []string{"--user", "alice", "--admin", "--no-merchant"},
			"--admin 与 --no-merchant 矛盾"},
		{"--status 不认识", []string{"--user", "alice", "--status", "frozen"}, "未知的用户状态"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runUserUpdate(tc.args)
			if err == nil {
				t.Fatalf("应当报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// user update 也在 json 模式下**连库之前**被拦下（同 apikey 那一组）。
//
// 钉的是顺序而不是那句话本身：DSN 指向一个没人听的端口，于是「先连库、再报不
// 支持」的实现在这里会拿到一句「连接数据库失败」（退出码 3），而不是「还不支持
// --format json」（退出码 2）——而写类命令上先做后报，意味着库里那一行已经改了。
func TestUserUpdateRejectsJSONFormatBeforeTouchingTheDB(t *testing.T) {
	withFormat(t, formatJSON)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	var err error
	stdout, _ := captureStreams(t, func() {
		err = runUserUpdate([]string{"--user", "1", "--display-name", "x"})
	})
	if err == nil {
		t.Fatal("json 模式下应当把还没接上的写类命令拦下来")
	}
	if !strings.Contains(err.Error(), "不支持 --format json") {
		t.Fatalf("错误信息 %q 里没有「不支持 --format json」——是不是先连库了", err.Error())
	}
	if !strings.Contains(err.Error(), "user update") {
		t.Errorf("错误信息里要点名是哪条命令：%v", err)
	}
	if code := exitCode(err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("被拦下时 stdout 应当为空，得到 %q", stdout)
	}
}

// TestUserUpdateEchoesTheDiff 钉住回显的形状。
//
// 首行点出改的是谁，之后一行一个**真的变了**的字段，「旧值 → 新值」。打这个
// 而不是一句「已更新」：改身份这一类操作的下一步常常是核对，只给终点时核对的人
// 只能自己再读一遍，而那时起点已经没了。
func TestUserUpdateEchoesTheDiff(t *testing.T) {
	setupEnv(t)

	name := uniqueName("update")
	id := createUserWithRole(t, name, name+"@example.com", "user")

	r := runOnce(t, runUser, []string{"update", "--user", name, "--display-name", "改过的名字"})
	if r.err != nil {
		t.Fatalf("update 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "已改用户 "+name+"\n") {
		t.Errorf("首行应当是「已改用户 %s」，实际：\n%s", name, r.stdout)
	}
	// 改之前那一行没有展示名（库里是 NULL），所以旧值那一栏是 `-`——与
	// `user get` 那一侧同一套占位符。
	if !strings.Contains(r.stdout, "展示名") || !strings.Contains(r.stdout, "- → 改过的名字") {
		t.Errorf("没打出展示名那一格的变化：\n%s", r.stdout)
	}
	// 没变的字段不打：它们既不是这次改动的结果，要看当前状态用 user get。
	for _, unwanted := range []string{"状态", "角色", "邮箱"} {
		if strings.Contains(r.stdout, unwanted) {
			t.Errorf("没变的 %s 也进了回显：\n%s", unwanted, r.stdout)
		}
	}

	get := runOnce(t, runUser, []string{"get", "--user", strconv.FormatInt(id, 10)})
	if get.err != nil {
		t.Fatalf("get 失败: %v", get.err)
	}
	if !strings.Contains(get.stdout, "改过的名字") {
		t.Errorf("库里的展示名没改成：\n%s", get.stdout)
	}
}

// TestUserUpdateSameValuePrintsOnlyTheLeadLine 钉住「没变就不打」。
//
// 第二遍给同一个值：命令仍然成功（它不是幂等命令，只是这一次没有字段真的变），
// 而 stdout 上只剩首行——回显说的是「库里的值变了」，不是「我请求过」。
func TestUserUpdateSameValuePrintsOnlyTheLeadLine(t *testing.T) {
	setupEnv(t)

	name := uniqueName("update-noop")
	createUserWithRole(t, name, "", "user")

	for i := 1; i <= 2; i++ {
		r := runOnce(t, runUser, []string{"update", "--user", name, "--display-name", "同一个名字"})
		if r.err != nil {
			t.Fatalf("第 %d 次 update 失败: %v\nstderr:\n%s", i, r.err, r.stderr)
		}
		wantArrow := i == 1
		if strings.Contains(r.stdout, "→") != wantArrow {
			t.Errorf("第 %d 次的回显不对（第 1 次应当有一格变化，第 2 次不该有）：\n%s",
				i, r.stdout)
		}
	}
}

// TestUserUpdateMissingUserIsAConflict 钉住「要改的那一行不在」的退出码。
//
// 与读那一侧同一个哨兵、同一个码：脚本按码分派时，「换一个标识或先建用户」与
// 「去看这次的报错本身」是两种处置。
func TestUserUpdateMissingUserIsAConflict(t *testing.T) {
	setupEnv(t)

	r := runOnce(t, runUser, []string{"update", "--user", uniqueName("nobody"), "--display-name", "x"})
	if r.err == nil {
		t.Fatal("改一个不存在的用户应当报错")
	}
	if !errors.Is(r.err, auth.ErrUserNotFound) {
		t.Errorf("错误 %v 不是 ErrUserNotFound", r.err)
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，想要 %d", code, exitConflict)
	}
}

// TestUserUpdateEmailConflictIsAConflict 钉住撞唯一键那一档的退出码与文案。
//
// 退出码表把唯一键算作 4；错误信息里要带上撞上的那个值——否则使用者只知道
// 「邮箱被占了」，不知道被谁占了。
func TestUserUpdateEmailConflictIsAConflict(t *testing.T) {
	setupEnv(t)

	taken := uniqueName("taken") + "@example.com"
	createUserWithRole(t, uniqueName("update-owner"), taken, "user")
	name := uniqueName("update-victim")
	createUserWithRole(t, name, "", "user")

	r := runOnce(t, runUser, []string{"update", "--user", name, "--email", taken})
	if r.err == nil {
		t.Fatal("撞上别人的邮箱应当报错")
	}
	if !errors.Is(r.err, auth.ErrEmailTaken) {
		t.Errorf("错误 %v 不是 ErrEmailTaken", r.err)
	}
	if !strings.Contains(r.err.Error(), taken) {
		t.Errorf("错误信息里要带上撞上的那个值（%q）：%v", taken, r.err)
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，想要 %d", code, exitConflict)
	}
	if r.stdout != "" {
		t.Errorf("失败时 stdout 应当为空，得到 %q", r.stdout)
	}
}

// userMarkersRow 是命令行这一侧要问的那三格：两个角色标记与 updated_at。
type userMarkersRow struct {
	isMerchant bool
	isAdmin    bool
	updatedAt  time.Time
}

// readUserMarkers 直接读那一行，不走 `user get`。
//
// 那个命令只给折回来的称呼，而 roleOf 先看 is_admin：(merchant=false, admin=true)
// 与 (true, true) 在它眼里都是 admin——「只动了哪一列」在这一侧看不出来，只有
// 按列读回来才分得清。
func readUserMarkers(t *testing.T, db *sql.DB, id int64) userMarkersRow {
	t.Helper()

	var row userMarkersRow
	if scanErr := db.QueryRowContext(t.Context(),
		`SELECT is_merchant, is_admin, updated_at FROM users WHERE id = $1`, id,
	).Scan(&row.isMerchant, &row.isAdmin, &row.updatedAt); scanErr != nil {
		t.Fatalf("读角色标记失败: %v", scanErr)
	}
	return row
}

// TestUserUpdateMarkersLandOnTheRightColumns 钉住四条标记旗标落进哪几列。
//
// 断言必须问库，不能只看 `user get` 的角色栏：roleOf 先看 is_admin，所以
// `--admin` 只写了 is_admin 的话折出来同样是 admin——「两列都写了」在命令行这
// 一侧看不出来，而它正是「改出来的 admin 与建出来的 admin 是同一种数据」那条
// 不变式。
func TestUserUpdateMarkersLandOnTheRightColumns(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("update-role")
	id := createUserWithRole(t, name, "", "user")

	// 每一步都从**上一步留下的那一行**起步，所以后两条的顺序不是随意的：
	// --no-admin 只有从一个真 admin 出发才证明得了「merchant 不动」，而它之后
	// 那一条 --no-merchant 又是「结果不能是反例」那条校验的正面判据——
	// is_admin 已经置假的一行，关 merchant 要能过，不然那道校验就把好数据也挡了。
	steps := []struct {
		args                    []string
		wantMerchant, wantAdmin bool
	}{
		{[]string{"--admin"}, true, true},
		{[]string{"--no-admin"}, true, false},
		{[]string{"--no-merchant"}, false, false},
		{[]string{"--merchant"}, true, false},
	}

	for _, step := range steps {
		args := append([]string{"update", "--user", name}, step.args...)
		if r := runOnce(t, runUser, args); r.err != nil {
			t.Fatalf("%v 失败: %v\nstderr:\n%s", step.args, r.err, r.stderr)
		}
		got := readUserMarkers(t, db, id)
		if got.isMerchant != step.wantMerchant || got.isAdmin != step.wantAdmin {
			t.Errorf("%v 之后库里是 (merchant=%v, admin=%v)，期望 (%v, %v)",
				step.args, got.isMerchant, got.isAdmin, step.wantMerchant, step.wantAdmin)
		}
	}
}

// TestUserUpdateResultMarkersReadThroughTheRole 钉住「改完之后那两个标记是什么」
// 这一步的算法（不连库）。
//
// 它是那条「结果不能是反例」的校验要算的那一步：**没提到的列**按读回来的称呼推，
// 而 roleOf 先看 is_admin——所以一个真 admin 上单给 --no-merchant，算出来是
// (false, true)，正是要拒的那一格。反过来，从 --no-admin 那一侧走过来的每一格
// （单独撤称呼、两个一起给）都该放行——两边的格一起，钉住了它既没漏判也没一刀切。
func TestUserUpdateResultMarkersReadThroughTheRole(t *testing.T) {
	yes, no := true, false

	cases := []struct {
		name                    string
		before                  auth.Role
		params                  auth.UpdateUserParams
		wantMerchant, wantAdmin bool
	}{
		{"user 上什么都不给", auth.RoleUser, auth.UpdateUserParams{}, false, false},
		{"admin 上什么都不给", auth.RoleAdmin, auth.UpdateUserParams{}, true, true},
		{"merchant 上什么都不给", auth.RoleMerchant, auth.UpdateUserParams{}, true, false},
		// 要拒的那一格：admin 的 merchant 被单独置假。
		{"admin 上关 merchant", auth.RoleAdmin,
			auth.UpdateUserParams{IsMerchant: &no}, false, true},
		// 放行的相邻那一格：先把 admin 撤了，merchant 那一列不动，仍是真。
		{"admin 上撤 admin", auth.RoleAdmin,
			auth.UpdateUserParams{IsAdmin: &no}, true, false},
		// 撤完 admin 之后关 merchant：这一行读出来已经是 merchant，关得下去。
		{"merchant 上关 merchant", auth.RoleMerchant,
			auth.UpdateUserParams{IsMerchant: &no}, false, false},
		// 报错文案给的两条出路之一：一条命令一步落到普通用户——两个标记一起改，
		// 算出来 is_admin 已经是假，所以不落进要拒的那一格。
		{"admin 上一步落到普通用户", auth.RoleAdmin,
			auth.UpdateUserParams{IsMerchant: &no, IsAdmin: &no}, false, false},
		{"user 上升 merchant", auth.RoleUser,
			auth.UpdateUserParams{IsMerchant: &yes}, true, false},
		{"user 上升 admin（两列一起）", auth.RoleUser,
			auth.UpdateUserParams{IsMerchant: &yes, IsAdmin: &yes}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isMerchant, isAdmin := userUpdateResultMarkers(tc.before, tc.params)
			if isMerchant != tc.wantMerchant || isAdmin != tc.wantAdmin {
				t.Errorf("算出来是 (merchant=%v, admin=%v)，期望 (%v, %v)",
					isMerchant, isAdmin, tc.wantMerchant, tc.wantAdmin)
			}
		})
	}
}

// TestUserUpdateNoMerchantOnAdminIsRefused 钉住这条校验，以及它拒的方式。
//
// 三组矛盾输入挡的是**输入**，这一条挡的是**结果**：admin 的 is_merchant 被单独
// 置假之后，库里留下 (merchant=false, admin=true) 而 roleOf 仍把它读成 admin——
// 于是那次真改了库的调用在回显里一片空白，看起来像什么都没做。
//
// 所以判据不只是「报错」，还有「那一行一个字都没动」：两次读回来对比，连
// updated_at 一起比——它是「这一行被写过」的痕迹，比两个标记更宽，万一哪天变成
// 「命令报了错、写还是写了」，两个标记未必碰巧不变，这个时间戳一定会动。
func TestUserUpdateNoMerchantOnAdminIsRefused(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("update-demote")
	id := createUserWithRole(t, name, "", "user")

	// 先升成 admin：反例只有从一个真 admin 出发才造得出来。
	if r := runOnce(t, runUser, []string{"update", "--user", name, "--admin"}); r.err != nil {
		t.Fatalf("升成 admin 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	before := readUserMarkers(t, db, id)
	if !before.isMerchant || !before.isAdmin {
		t.Fatalf("升完之后库里是 (merchant=%v, admin=%v)，想要 (true, true)",
			before.isMerchant, before.isAdmin)
	}

	r := runOnce(t, runUser, []string{"update", "--user", name, "--no-merchant"})
	if r.err == nil {
		t.Fatal("--no-merchant 打在 admin 上应当报错，却通过了")
	}
	if code := exitCode(r.err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（这是参数错，重发一次就能成）", code, exitUsage)
	}
	// 文案要把出路说出来：只讲「矛盾」的话，手上这条命令还是不知道该怎么发。
	// 两条路都要在：分两次发（先撤称呼），或者两个旗标一起给。
	for _, want := range []string{"--no-merchant", "先 --no-admin", "--no-admin --no-merchant"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("报错里要提到 %q：%v", want, r.err)
		}
	}
	if r.stdout != "" {
		t.Errorf("被拒的那次不该有回显，stdout 实际是 %q", r.stdout)
	}

	after := readUserMarkers(t, db, id)
	if after.isMerchant != before.isMerchant || after.isAdmin != before.isAdmin {
		t.Errorf("被拒之后那两个标记变了：(merchant=%v, admin=%v) → (merchant=%v, admin=%v)",
			before.isMerchant, before.isAdmin, after.isMerchant, after.isAdmin)
	}
	if !after.updatedAt.Equal(before.updatedAt) {
		t.Errorf("被拒的那次不该写库，updated_at 却动了：%v → %v",
			before.updatedAt, after.updatedAt)
	}

	// 出路之一真的走得通：先撤 admin，再关 merchant。
	if ok := runOnce(t, runUser, []string{"update", "--user", name, "--no-admin"}); ok.err != nil {
		t.Fatalf("--no-admin 应当通过: %v\nstderr:\n%s", ok.err, ok.stderr)
	}
	if ok := runOnce(t, runUser, []string{"update", "--user", name, "--no-merchant"}); ok.err != nil {
		t.Fatalf("撤完 admin 之后 --no-merchant 应当通过: %v\nstderr:\n%s", ok.err, ok.stderr)
	}
	final := readUserMarkers(t, db, id)
	if final.isMerchant || final.isAdmin {
		t.Errorf("两步之后应当是 (merchant=false, admin=false)，实际 (merchant=%v, admin=%v)",
			final.isMerchant, final.isAdmin)
	}

	// 出路之二：另一个 admin 上一条命令两个旗标一起给。守卫算的是结果，所以它
	// 不该把这条命令也挡掉——挡掉的话文案里给的这条路就成了空话。
	oneStep := uniqueName("update-demote-onestep")
	oneStepID := createUserWithRole(t, oneStep, "", "admin")
	got := runOnce(t, runUser,
		[]string{"update", "--user", oneStep, "--no-admin", "--no-merchant"})
	if got.err != nil {
		t.Fatalf("--no-admin --no-merchant 一步应当通过: %v\nstderr:\n%s", got.err, got.stderr)
	}
	oneStepAfter := readUserMarkers(t, db, oneStepID)
	if oneStepAfter.isMerchant || oneStepAfter.isAdmin {
		t.Errorf("一步之后应当是 (merchant=false, admin=false)，实际 (merchant=%v, admin=%v)",
			oneStepAfter.isMerchant, oneStepAfter.isAdmin)
	}
}

// TestPrintUserUpdateShowsOnlyRealChanges 钉住渲染这一半（不连库）。
//
// 两条判据：变了的那几格按「旧值 → 新值」打出来，没变的一格都不打；空值那一栏
// 走 `-`（与 user get 同一套占位符）——不然「清空了」与「本来就是空的」在输出里
// 分不出来，而它们的意思正相反。
func TestPrintUserUpdateShowsOnlyRealChanges(t *testing.T) {
	before := auth.UserInfo{
		ID: 7, Username: "alice", Email: "old@example.com",
		Status: auth.UserStatusActive, Role: auth.RoleUser,
	}
	after := before
	after.Email = ""               // 清成 NULL
	after.DisplayName = "新的展示名"    // 从没填到填上
	after.Role = auth.RoleMerchant // 标记变了

	var buf bytes.Buffer
	printUserUpdate(&buf, before, after)
	got := buf.String()

	// 首行先打：它在渲染器的缓冲之外，打在后面就会排到表后面去。
	if !strings.HasPrefix(got, "已改用户 alice\n") {
		t.Errorf("首行应当是「已改用户 alice」，实际：\n%s", got)
	}
	for _, want := range []string{
		"邮箱", "old@example.com → -",
		"展示名", "- → 新的展示名",
		"角色", "user → merchant",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("回显里没有 %q：\n%s", want, got)
		}
	}
	// 状态两行都一样，所以这一栏一次都不该出现。
	if strings.Contains(got, "状态") {
		t.Errorf("没变的状态也进了回显：\n%s", got)
	}
}
