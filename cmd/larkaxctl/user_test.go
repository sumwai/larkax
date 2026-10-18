package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// 状态没有写入命令（这一侧只能读），所以直接用 SQL 把两个用户改成不同的状态：
// 这是唯一能让「筛 active」与「筛 banned」分开的造法。
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
