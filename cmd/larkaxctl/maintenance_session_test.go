package main

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// `maintenance session` 的集成用例。
//
// 这一层要证明的是「命令行把 settings 里的天数换成判据、把结果按约定打了出来」；
// 判据本身（哪一类行该删）在 internal/auth 的用例里。所以这里重点是三条：
//
//   - 干跑与真删报的是同一个数，且干跑一行都没动；
//   - 第二次跑删 0 条、退出 0；
//   - 留存期从 settings 读，改成非正数时拒绝清理。

func TestMaintenanceSessionPurgesExpiredAndOldRevoked(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)

	// 留存窗口收窄到 1 天，跑完还原：这一条要的是「窗口内与窗口外各有一条」，
	// 而默认的 30 天造不出来（造条吊销 31 天的行也行，但 1 天更直观）。
	restore := setSessionRetentionDays(t, db, 1)
	t.Cleanup(restore)

	// 先清掉其它用例留下的可清理行：下面要断言「第二次删 0 条」。
	mustStream(t, func() error {
		return runMaintenanceSession(nil)
	})

	valid := insertCLISession(t, db, userID, "+168 hours", "")
	expired := insertCLISession(t, db, userID, "-1 second", "")
	revokedRecent := insertCLISession(t, db, userID, "+168 hours", "1 hour")
	revokedOld := insertCLISession(t, db, userID, "+168 hours", "48 hours")

	// 干跑：报的是「将删除」，且一行都没动。
	dryOut, _ := mustStream(t, func() error {
		return runMaintenanceSession([]string{"--dry-run"})
	})
	if !strings.Contains(dryOut, "将删除") {
		t.Errorf("干跑的输出应当说明是干跑，实际为 %q", dryOut)
	}
	dryCount := firstNumber(t, dryOut)
	if dryCount != 2 {
		t.Errorf("干跑报 %d 条，期望 2（已过期的 1 条 + 窗口外已吊销的 1 条）", dryCount)
	}
	if !cliSessionExists(t, db, expired) {
		t.Error("干跑不该真的删除")
	}

	// 真删：数与干跑一致。
	out, _ := mustStream(t, func() error {
		return runMaintenanceSession(nil)
	})
	if !strings.Contains(out, "已删除") {
		t.Errorf("真删的输出应当说明删了多少，实际为 %q", out)
	}
	if realCount := firstNumber(t, out); realCount != dryCount {
		t.Errorf("干跑报 %d 条、真删 %d 条：两处的判据不一致", dryCount, realCount)
	}

	for _, tc := range []struct {
		name   string
		id     int64
		exists bool
	}{
		{"有效会话", valid, true},
		{"窗口内已吊销的会话", revokedRecent, true},
		{"已过期的会话", expired, false},
		{"窗口外已吊销的会话", revokedOld, false},
	} {
		if got := cliSessionExists(t, db, tc.id); got != tc.exists {
			t.Errorf("%s（id=%d）存在 = %v，期望 %v", tc.name, tc.id, got, tc.exists)
		}
	}

	// 第二次跑：删 0 条，退出 0。
	again, _ := mustStream(t, func() error {
		return runMaintenanceSession(nil)
	})
	if n := firstNumber(t, again); n != 0 {
		t.Errorf("第二次清理删除 %d 条，期望 0", n)
	}
}

// 留存期是 0 或负数时拒绝清理，且一行都不删。
func TestMaintenanceSessionRejectsNonPositiveRetention(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)

	// 0 / 负数走「不是正数」；106752 天换算成 Duration 回绕成负数，
	// 213505 天回绕成一个很小的正数——三条走的是同一个上界判据，
	// 而 213505 那一条在加上界之前会被放行，然后按一天窗口真删。
	for _, days := range []int{0, -1, 106752, 213505} {
		restore := setSessionRetentionDays(t, db, days)
		id := insertCLISession(t, db, userID, "+168 hours", "48 hours")
		err := runMaintenanceSession(nil)
		restore()
		if err == nil {
			t.Errorf("留存期 %d 天时应当报错", days)
			continue
		}
		if !strings.Contains(err.Error(), "拒绝清理") {
			t.Errorf("留存期 %d 天时报错没点出拒绝清理：%v", days, err)
		}
		// 设置里的值坏了是运行期失败（退出 1），不是命令写错（退出 2）。
		if code := cli.ExitCode(err); code != cli.ExitFailure {
			t.Errorf("留存期 %d 天的退出码 = %d，期望 %d（运行期失败，不是用法错误）",
				days, code, cli.ExitFailure)
		}
		if !cliSessionExists(t, db, id) {
			t.Errorf("留存期 %d 天时被拒绝的清理不该删掉行", days)
		}
	}
}

// 位置参数一律拒掉：`maintenance session dry-run`（漏了横线）会被 flag 留在
// fs.Args() 里，不拦就成了真删。这是用法错误（退出 2），且在连库之前判出来。
func TestMaintenanceSessionRejectsPositionalArgs(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")
	err := runMaintenanceSession([]string{"dry-run"})
	if err == nil {
		t.Fatal("位置参数 dry-run 应当报参数错，而不是被当成干跑或真删")
	}
	if !strings.Contains(err.Error(), "dry-run") {
		t.Errorf("报错应当提示 --dry-run，得到 %v", err)
	}
	if code := cli.ExitCode(err); code != cli.ExitUsage {
		t.Errorf("退出码 = %d，期望 %d（用法错误）", code, cli.ExitUsage)
	}
}

// `--format json` 下写类命令直接报错（与 maintenance idempotency 同一条约定）。
func TestMaintenanceSessionRejectsJSON(t *testing.T) {
	saved := cli.OutputFormat
	cli.OutputFormat = cli.FormatJSON
	defer func() { cli.OutputFormat = saved }()

	if err := runMaintenanceSession(nil); err == nil {
		t.Error("--format json 下 maintenance session 应当报错")
	}
}

// setSessionRetentionDays 改 settings 里的留存天数，返回还原函数。
//
// 直接改那一行而不是走 `settings set`：那条路要一个记名的操作人，而这里要的
// 只是「库里是几」。还原写回的是 30（迁移里的默认值），不用先读一遍——这条
// 用例只在这个包自己的库上跑，而那个库刚被迁移建出来。
func setSessionRetentionDays(t *testing.T, db *sql.DB, days int) func() {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE settings SET value = $1::jsonb WHERE key = 'console.session_retention_days'`,
		strconv.Itoa(days)); err != nil {
		t.Fatalf("改会话留存天数失败: %v", err)
	}
	return func() {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE settings SET value = '30'::jsonb WHERE key = 'console.session_retention_days'`); err != nil {
			t.Errorf("还原会话留存天数失败: %v", err)
		}
	}
}

// insertCLISession 造一行会话。expiresIn 是相对现在的时间段（如 "+168 hours"），
// revokedAge 为空表示没被吊销，否则是「多久以前吊销的」（如 "48 hours"）。
func insertCLISession(t *testing.T, db *sql.DB, userID int64, expiresIn, revokedAge string) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO user_sessions (user_id, token_hash, csrf_token, expires_at, revoked_at)
		VALUES ($1, $2, 'csrf-test',
		        now() + $3::interval,
		        CASE WHEN $4 = '' THEN NULL ELSE now() - $4::interval END)
		RETURNING id`,
		userID, []byte(uniqueName("session-token")), expiresIn, revokedAge).Scan(&id)
	if err != nil {
		t.Fatalf("造会话失败（expires %s / revoked %q）: %v", expiresIn, revokedAge, err)
	}
	return id
}

// cliSessionExists 报告一行会话还在不在。
func cliSessionExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM user_sessions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("查会话 %d 失败: %v", id, err)
	}
	return n > 0
}
