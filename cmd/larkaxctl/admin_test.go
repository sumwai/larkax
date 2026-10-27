package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/adminapi"
	"github.com/sumwai/larkax/internal/settings"
)

// admin 这一组先测不碰库的那几条路径：缺子命令、未知子命令与 --help。
//
// 「用法写错」属于命令行的契约：它决定人按错一个词时看到的是用法还是
// 一段要连上库才判断得出来的错误。
func TestAdminSubcommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"没有子命令", nil, "需要子命令"},
		{"未知子命令", []string{"bogus"}, "未知的 admin 子命令"},
		{"token 没有子命令", []string{"token"}, "admin token 需要子命令"},
		{"token 未知子命令", []string{"token", "bogus"}, "未知的 admin token 子命令"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runAdmin(tc.args) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望错误里含 %q，得到 %v", tc.want, err)
			}
		})
	}

	for _, args := range [][]string{{"-h"}, {"--help"}, {"token", "-h"}, {"token", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stderr := captureStreams(t, func() {
				if err := runAdmin(args); err != nil {
					t.Errorf("--help 应当成功，得到 %v", err)
				}
			})
			if stderr != "" {
				t.Errorf("--help 写到了 stderr：%q", stderr)
			}
		})
	}
}

// TestAdminTokenRejectsExtraArgs 钉住「这两条命令不接受位置参数」。
//
// 它们在连库之前就该拒：把 `create extra` 当成一次成功的轮换是最坏的结果
// ——旧令牌已经失效，而人多半以为参数只是被忽略了。
func TestAdminTokenRejectsExtraArgs(t *testing.T) {
	for _, args := range [][]string{{"token", "create", "extra"}, {"token", "revoke", "extra"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := runCaptured(t, func() error { return runAdmin(args) })
			if err == nil || !strings.Contains(err.Error(), "不接受位置参数") {
				t.Fatalf("期望「不接受位置参数」，得到 %v", err)
			}
			if code := cli.ExitCode(err); code != 2 {
				t.Errorf("退出码 = %d，期望 2（用法错）", code)
			}
		})
	}
}

// TestAdminTokenCreateAndRevoke 走一遍 create → 库里是哈希 → revoke → 全零哨兵。
//
// # 判据为什么落在库里的那一列上
//
// 「明文不进库」在命令的返回值里看不出来：它返回错误也照样可能已经把明文
// 写下去了。唯一能证明的是 settings 里那个键的值——64 位十六进制、等于这枚
// 令牌的 SHA-256、且不等于令牌本身。
func TestAdminTokenCreateAndRevoke(t *testing.T) {
	requireDB(t)
	db := testDB(t)

	stored := func(t *testing.T) string {
		t.Helper()
		var raw []byte
		if err := db.QueryRowContext(t.Context(),
			`SELECT value FROM settings WHERE key = $1`, settings.KeyAdminToken).Scan(&raw); err != nil {
			t.Fatalf("读令牌那一行失败: %v", err)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("令牌那一列不是字符串: %v（原文 %s）", err, raw)
		}
		return value
	}

	stdout, stderr := captureStreams(t, func() {
		if err := runAdmin([]string{"token", "create"}); err != nil {
			t.Errorf("create 失败: %v", err)
		}
	})

	// stdout 只有一行：这枚令牌。它的用途是被接走
	// （`larkaxctl admin token create | head -1`），混进一句话就得多剥一层。
	line, rest, hasMore := strings.Cut(strings.TrimRight(stdout, "\n"), "\n")
	if line == "" {
		t.Fatalf("stdout 里没有令牌：%q", stdout)
	}
	if hasMore && strings.TrimSpace(rest) != "" {
		t.Errorf("stdout 不止一行，接令牌的人会连说明一起拿到：%q", stdout)
	}
	if strings.Contains(line, " ") {
		t.Errorf("令牌那一行里有多余的字：%q", stdout)
	}

	token := line
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("令牌不是 base64url：%v（%q）", err, token)
	}
	if len(raw) != 32 {
		t.Errorf("令牌是 %d 字节，期望 32", len(raw))
	}

	// stderr 讲清楚「只有这一次」：这是唯一一处会告诉使用者它丢了就没了的
	// 地方，别处都只有哈希。
	if !strings.Contains(stderr, "唯一的明文") {
		t.Errorf("stderr 没说明明文只有这一次：%q", stderr)
	}

	sum := sha256.Sum256([]byte(token))
	if got := stored(t); got != hex.EncodeToString(sum[:]) {
		t.Errorf("库里那一列不是这枚令牌的 SHA-256：%q", got)
	}
	if got := stored(t); got == token {
		t.Errorf("库里存了明文令牌")
	}

	// 再生成一枚：写入是覆盖，不是追加。
	second, _ := captureStreams(t, func() {
		if err := runAdmin([]string{"token", "create"}); err != nil {
			t.Errorf("第二次 create 失败: %v", err)
		}
	})
	second = strings.TrimSpace(second)
	if second == "" || second == token {
		t.Fatalf("第二次 create 没给出新令牌：%q", second)
	}
	secondSum := sha256.Sum256([]byte(second))
	if got := stored(t); got != hex.EncodeToString(secondSum[:]) {
		t.Errorf("第二次 create 之后库里不是新令牌的哈希")
	}

	// 撤销：写全零的哨兵。它不是空串——空串表示「从没配置过」，那对应 503；
	// 撤销之后请求该得到 401。
	revokeOut, _ := captureStreams(t, func() {
		if err := runAdmin([]string{"token", "revoke"}); err != nil {
			t.Errorf("revoke 失败: %v", err)
		}
	})
	if strings.TrimSpace(revokeOut) == "" {
		t.Errorf("revoke 什么都没打：人不知道它生效了没有")
	}
	if got := stored(t); got != strings.Repeat("0", 64) {
		t.Errorf("撤销之后库里的值是 %q，期望 64 个 0", got)
	}

	// 幂等：再撤销一次不报错，值也还是全零。
	if err := runCaptured(t, func() error { return runAdmin([]string{"token", "revoke"}) }); err != nil {
		t.Errorf("重复撤销应当成功，得到 %v", err)
	}
	if got := stored(t); got != strings.Repeat("0", 64) {
		t.Errorf("重复撤销之后库里的值是 %q", got)
	}

	// 复原成迁移里的默认值，别把状态留给同一包里的其他用例。
	if _, err := db.ExecContext(t.Context(),
		`UPDATE settings SET value = '""'::jsonb WHERE key = $1`, settings.KeyAdminToken); err != nil {
		t.Fatalf("复原令牌那一行失败: %v", err)
	}
}

// TestAdminTokenRejectsJSONOutput 钉住两条命令都不支持 --format json。
//
// 它们输出的是给人接走的一行值，不是一份报告。不拦的话 -o json 下会把中文
// 提醒写到 stdout，而 stdout 的契约是「只放能被直接接走的值」；文档里也写着
// 这两条不支持 json——两处必须说同一件事。
//
// 判据要在碰库之前成立：参数形式不对不该先连上生产库再报错。
func TestAdminTokenRejectsJSONOutput(t *testing.T) {
	for _, sub := range []string{"create", "revoke"} {
		t.Run(sub, func(t *testing.T) {
			withFormat(t, cli.FormatJSON)
			err := runCaptured(t, func() error { return runAdmin([]string{"token", sub}) })
			if err == nil || !strings.Contains(err.Error(), "--format json") {
				t.Fatalf("期望「不支持 --format json」，得到 %v", err)
			}
			if code := cli.ExitCode(err); code != 2 {
				t.Errorf("退出码 = %d，期望 2（参数错）", code)
			}
		})
	}
}

// TestAdminTokenRevokeWithoutToken 钉住「从没配置过」时撤销是一次空操作 +
// 报错，而不是把空串写成全零哨兵。
//
// 写成哨兵的话，那个部署会从 503（还没启用）变成 401（令牌不对），而运维
// 会照着 401 去「换一枚」——他从来没创建过。
func TestAdminTokenRevokeWithoutToken(t *testing.T) {
	requireDB(t)
	db := testDB(t)
	if _, err := db.ExecContext(t.Context(),
		`UPDATE settings SET value = '""'::jsonb WHERE key = $1`, settings.KeyAdminToken); err != nil {
		t.Fatalf("重置令牌那一行失败: %v", err)
	}

	err := runCaptured(t, func() error { return runAdmin([]string{"token", "revoke"}) })
	if !errors.Is(err, adminapi.ErrNoTokenConfigured) {
		t.Fatalf("期望 ErrNoTokenConfigured，得到 %v", err)
	}
	if code := cli.ExitCode(err); code != 4 {
		t.Errorf("退出码 = %d，期望 4（要先去创建一枚）", code)
	}

	var stored string
	if err := db.QueryRowContext(t.Context(),
		`SELECT value #>> '{}' FROM settings WHERE key = $1`, settings.KeyAdminToken).Scan(&stored); err != nil {
		t.Fatalf("读令牌那一行失败: %v", err)
	}
	if stored != "" {
		t.Errorf("撤销之后那一列变成了 %q，期望仍是空串（从没配置过）", stored)
	}
}

// TestPrintAdminTokenKeepsStdoutSingleLine 单独钉住渲染：stdout 只有令牌，
// 说明一律去 stderr，且两档（TTY / 非 TTY）剥掉样式码之后逐字相同。
func TestPrintAdminTokenKeepsStdoutSingleLine(t *testing.T) {
	colorEnv(t)
	var stdout, stderr bytes.Buffer
	withTerminal(t, false)
	printAdminToken(&stdout, &stderr, "token-placeholder")

	if got := strings.TrimRight(stdout.String(), "\n"); got != "token-placeholder" {
		t.Errorf("stdout = %q，期望只有令牌本身", got)
	}
	if !strings.Contains(stderr.String(), "Bearer") {
		t.Errorf("stderr 没告诉人怎么带上它：%q", stderr.String())
	}
	if strings.Contains(stderr.String(), "token-placeholder") {
		t.Errorf("stderr 里重复了一遍令牌：%q", stderr.String())
	}

	withTerminal(t, true)
	var styledOut, styledErr bytes.Buffer
	printAdminToken(&styledOut, &styledErr, "token-placeholder")
	if !strings.Contains(styledOut.String(), "\x1b[") {
		t.Errorf("TTY 下令牌没加粗：%q", styledOut.String())
	}
	if stripSGR(styledOut.String()) != stdout.String() {
		t.Errorf("上色改变了 stdout 的排版：\n%q\n%q", stripSGR(styledOut.String()), stdout.String())
	}
	if stripSGR(styledErr.String()) != stderr.String() {
		t.Errorf("上色改变了 stderr 的排版")
	}
}

// 令牌存储的两种错误要能走到退出码：缺那一行时提示先跑迁移，
// 而不是让运维面对一句驱动原文。
func TestAdminTokenMissingRowIsClassified(t *testing.T) {
	if got := cli.ExitCode(adminapi.ErrNoTokenRow); got != 4 {
		t.Errorf("ErrNoTokenRow 的退出码 = %d，期望 4", got)
	}
}
