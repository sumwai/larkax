package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/payment"
	"github.com/sumwai/larkax/internal/pipeline"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
	"github.com/sumwai/larkax/internal/pricing"
)

// ── 触发条件 → 退出码 → 是否打用法 ──────────────────────────────────────
//
// 「用户输入错」这一类（参数名、取值、必填缺失、标志组合、flag 解析失败）
// 一律退出 exitUsage，而整段用法只在「还没开始用」的那三处打：缺子命令、
// 未知子命令、flag 解析失败。写错一个值的人已经会用了，再糊一屏用法文本
// 只会把他真正要看的那一句挤走。
//
// 这张表是那条契约的可执行版本：加一条命令就往里加一行，漏了就对不上。

// parseCase 是一个「喂坏参数」的场景。
type parseCase struct {
	// command 是用例名，也是这一条在表里唯一的标识。
	command string
	run     func() error
	// wantUsage 表示这次失败**应不应该**打整段用法。
	wantUsage bool
	// hint 是这条命令用法文本里的一句独特子串，用来判断 stderr 里有没有它。
	hint string
}

const (
	settingsHint = "larkaxctl settings"
	apikeyHint   = "larkaxctl apikey"
	userHint     = "larkaxctl user"
	migrateHint  = "larkaxctl migrate"
	rotateHint   = "larkaxctl secret rotate"
	calendarHint = "larkaxctl price calendar"
	pluginHint   = "larkaxctl plugin"
	ledgerHint   = "larkaxctl ledger"
	aliasHint    = "larkaxctl alias"
	modelHint    = "larkaxctl model"
	channelHint  = "larkaxctl channel"
	priceHint    = "larkaxctl price"
	walletHint   = "larkaxctl wallet"
	snapshotHint = "larkaxctl snapshot"
)

func parseCases() []parseCase {
	return []parseCase{
		// ── 还没开始用：缺子命令 / 未知子命令 / flag 解析失败，用法要打 ──
		{"settings 缺子命令", func() error { return runSettings(nil) }, true, settingsHint},
		{"settings flag 解析失败", func() error { return runSettings([]string{"--nope"}) }, true, settingsHint},
		{"apikey 缺子命令", func() error { return runAPIKey(nil) }, true, apikeyHint},
		{"apikey flag 解析失败", func() error { return runAPIKey([]string{"--nope"}) }, true, apikeyHint},
		{"user 缺子命令", func() error { return runUser(nil) }, true, userHint},
		{"user flag 解析失败", func() error { return runUser([]string{"--nope"}) }, true, userHint},
		{"migrate 缺子命令", func() error { return runMigrate(nil) }, true, migrateHint},
		{"migrate flag 解析失败", func() error { return runMigrate([]string{"--nope"}) }, true, migrateHint},
		{"secret rotate flag 解析失败", func() error { return runRotate([]string{"--nope"}) }, true, rotateHint},
		{"price calendar import flag 解析失败", func() error { return runCalendarImport([]string{"--nope"}) }, true, calendarHint},
		{"plugin 缺子命令", func() error { return runPlugin(nil) }, true, pluginHint},
		{"plugin 未知子命令", func() error { return runPlugin([]string{"bogus"}) }, true, pluginHint},
		{"price calendar 缺子命令", func() error { return runPriceCalendar(nil) }, true, calendarHint},
		{"ledger reconcile flag 解析失败", func() error { return runReconcile([]string{"--nope"}) }, true, ledgerHint},
		{"alias 未知子命令", func() error { return runAlias([]string{"bogus"}) }, true, aliasHint},
		{"model 未知子命令", func() error { return runModel([]string{"bogus"}) }, true, modelHint},
		{"channel 未知子命令", func() error { return runChannel([]string{"bogus"}) }, true, channelHint},
		{"price 未知子命令", func() error { return runPrice([]string{"bogus"}) }, true, priceHint},
		{"wallet 未知子命令", func() error { return runWallet([]string{"bogus"}) }, true, walletHint},
		{"snapshot 未知子命令", func() error { return runSnapshot([]string{"bogus"}) }, true, snapshotHint},

		// ── 已经会用了、只是写错了：不打整段用法 ──
		{"settings 未知子命令", func() error { return runSettings([]string{"bogus"}) }, false, settingsHint},
		{"settings set 缺 --key", func() error { return runSettings([]string{"set", "--by", "ops"}) }, false, settingsHint},
		{"settings set 缺 --by", func() error { return runSettings([]string{"set", "--key", "plugin.enabled"}) }, false, settingsHint},
		{"settings set 位置参数多余", func() error {
			return runSettings([]string{"set", "--key", "plugin.enabled", "--by", "ops", "extra"})
		}, false, settingsHint},
		{"settings list 位置参数多余", func() error { return runSettingsList([]string{"extra"}) }, false, settingsHint},

		{"apikey create 缺 --user", func() error { return runAPIKeyCreate(nil) }, false, apikeyHint},
		{"apikey update 缺 --id", func() error { return runAPIKeyUpdate(nil) }, false, apikeyHint},
		{"apikey update 位置参数多余", func() error { return runAPIKeyUpdate([]string{"extra"}) }, false, apikeyHint},
		{"apikey update 一个字段都没给", func() error {
			return runAPIKeyUpdate([]string{"--id", "1"})
		}, false, apikeyHint},
		{"apikey update --status 取值不认识", func() error {
			return runAPIKeyUpdate([]string{"--id", "1", "--status", "suspended"})
		}, false, apikeyHint},
		{"apikey get 缺 --id", func() error { return runAPIKeyGet(nil) }, false, apikeyHint},
		{"apikey get 位置参数多余", func() error { return runAPIKeyGet([]string{"extra"}) }, false, apikeyHint},
		{"apikey list 位置参数多余", func() error { return runAPIKeyList([]string{"extra"}) }, false, apikeyHint},

		{"user create 缺 --name", func() error { return runUserCreate(nil) }, false, userHint},
		{"user create --name 是纯数字", func() error {
			return runUserCreate([]string{"--name", "123"})
		}, false, userHint},
		// 同一条路，另一条判据：邮箱写不成一个地址（见 auth.ValidateEmail）。
		{"user create --email 不成邮箱", func() error {
			return runUserCreate([]string{"--name", "alice", "--email", "not-an-email"})
		}, false, userHint},

		{"migrate create 缺名称", func() error { return runMigrate([]string{"create"}) }, false, migrateHint},
		{"migrate 未知子命令", func() error { return runMigrate([]string{"bogus"}) }, false, migrateHint},

		{"secret rotate 位置参数多余", func() error { return runRotate([]string{"extra"}) }, false, rotateHint},
		{"secret rotate 标志组合", func() error { return runRotate([]string{"--apply", "--verify"}) }, false, rotateHint},
		{"secret rotate 取值非法", func() error { return runRotate([]string{"--batch-size", "0"}) }, false, rotateHint},

		{"price calendar import 缺文件", func() error { return runCalendarImport(nil) }, false, calendarHint},
		{"price calendar import 位置参数多余", func() error { return runCalendarImport([]string{"a", "b"}) }, false, calendarHint},

		{"plugin publish 缺源文件", func() error { return runPluginPublish(nil) }, false, pluginHint},

		{"ledger reconcile 位置参数多余", func() error { return runReconcile([]string{"extra"}) }, false, ledgerHint},

		// 规则扫全的其余几条命令：同一条判据，各文件各留一行代表。
		{"alias create 缺 --model", func() error { return runAliasCreate(nil) }, false, aliasHint},
		{"model create 缺 --model", func() error { return runModelCreate(nil) }, false, modelHint},
		{"channel create 缺 --name", func() error { return runChannelCreate(nil) }, false, channelHint},
		{"price create 缺 --scope", func() error { return runPriceCreate(nil) }, false, priceHint},
		{"wallet create 缺 --user", func() error { return runWalletCreate(nil) }, false, walletHint},
		{"wallet refund 缺 --user", func() error { return runWalletRefund(nil) }, false, walletHint},
		{"wallet refund 缺 --ref", func() error {
			return runWalletRefund([]string{"--user", "1", "--amount", "1"})
		}, false, walletHint},
		{"wallet payout 缺子命令", func() error { return runWalletPayout(nil) }, true, walletHint},
		{"wallet payout 未知子命令", func() error {
			return runWalletPayout([]string{"bogus"})
		}, true, walletHint},
		{"wallet payout request 缺 --ref", func() error {
			return runPayoutRequest([]string{"--user", "1", "--amount", "1"})
		}, false, walletHint},
		{"wallet payout approve 缺 --by", func() error {
			return runPayoutApprove([]string{"--id", "1"})
		}, false, walletHint},
		{"wallet payout paid 缺 --gateway", func() error {
			return runPayoutPaid([]string{"--id", "1"})
		}, false, walletHint},
		{"snapshot allowlist 位置参数多余", func() error { return runSnapshotAllowlist([]string{"extra"}) }, false, snapshotHint},
		{"snapshot redact 缺 --direction", func() error { return runSnapshotRedact(nil) }, false, snapshotHint},
	}
}

// TestParseErrorsAreUsageAndPrintTheRightUsageText 把两张判据并成一条：
// 这一类一律退出 exitUsage，而用法文本的出现与否与表里标的一致。
//
// 合并是有意的：「用法配 1」与「该不该打用法」是同一件事的两半，分开写时
// 有一半会悄悄漂走——而漂走的表现是脚本按退出码分派时走了另一条分支。
func TestParseErrorsAreUsageAndPrintTheRightUsageText(t *testing.T) {
	for _, tc := range parseCases() {
		t.Run(tc.command, func(t *testing.T) {
			var err error
			_, stderr := captureStreams(t, func() { err = tc.run() })
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d（%v）", exitUsage, got, err)
			}
			gotUsage := strings.Contains(stderr, tc.hint)
			if gotUsage != tc.wantUsage {
				t.Errorf("用法文本出现与否：期望 %v，实际 %v（用法是给还没开始用的人看的）\nstderr=%q",
					tc.wantUsage, gotUsage, stderr)
			}
		})
	}
}

// ── B1：连不上库一律 exitDBUnreach ───────────────────────────────────────

// TestDBUnreachableEntriesExitThree 用一条不可达的 DSN 打每个连库入口。
//
// 这些入口以前各写各的 sql.Open + Ping，失败时裸返回落 1，与「参数错」撞在
// 一个码上——调度器看到 1 会当成「别再试了」，而它其实是「库暂时连不上」。
func TestDBUnreachableEntriesExitThree(t *testing.T) {
	setupEnv(t)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	doc := writePendingCalendarDoc(t)
	cases := []struct {
		name string
		run  func() error
	}{
		{"channel list", func() error { return runChannelList(nil) }},
		{"apikey list", func() error { return runAPIKeyList(nil) }},
		{"apikey get", func() error { return runAPIKeyGet([]string{"--id", "1"}) }},
		{"settings list", func() error { return runSettingsList(nil) }},
		{"ledger reconcile", func() error { return runReconcile(nil) }},
		{"migrate up", func() error { return runMigrate([]string{"up"}) }},
		{"secret rotate", func() error { return runRotate(nil) }},
		{"price calendar import", func() error { return runCalendarImport([]string{doc}) }},
		// 提现那一族与退款走的是同一个连库入口：连不上时也该是 3，而不是落到 1
		// ——脚本对 1 的处置是「别再试了」，对 3 是「去看环境」。
		{"wallet refund", func() error {
			return runWalletRefund([]string{"--user", "1", "--amount", "1", "--ref", "R"})
		}},
		{"wallet payout request", func() error {
			return runPayoutRequest([]string{"--user", "1", "--amount", "1", "--ref", "R"})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.run)
			if err == nil {
				t.Fatal("连不上库时应当报错")
			}
			if got := exitCode(err); got != exitDBUnreach {
				t.Errorf("连不上库的退出码应当是 %d，得到 %d（%v）", exitDBUnreach, got, err)
			}
		})
	}
}

// writePendingCalendarDoc 写一份能过校验、又不带数据行的日历文件，返回路径。
//
// 取 pending：那一年还没有定论，所以不带 days 也不带 source，是这份文件格式里
// 最小的一份。用它是因为这条用例只关心「文件过了校验之后连库那一步」。
func writePendingCalendarDoc(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calendar.json")
	doc := `{"region":"CN","year":2031,"status":"pending"}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("写日历文件失败: %v", err)
	}
	return path
}

// ── B2：「那一行不存在」一律 exitConflict ────────────────────────────────

// TestConflictSentinelsExitFour 逐条钉住 conflictErrors 覆盖的哨兵。
//
// 加一个新 store 包、或在某个包里新添一个 ErrXxxNotFound 时，往这张表里加
// 一行——漏登记的表现是它悄悄退回 exitFailure，而 1 与 4 在脚本里是两种处置。
func TestConflictSentinelsExitFour(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"auth.ErrUserNotFound", auth.ErrUserNotFound},
		{"auth.ErrUsernameTaken", auth.ErrUsernameTaken},
		{"auth.ErrEmailTaken", auth.ErrEmailTaken},
		{"auth.ErrKeyNotFound", auth.ErrKeyNotFound},
		{"store.ErrNoSuchUser", store.ErrNoSuchUser},
		{"store.ErrNoWallet", store.ErrNoWallet},
		{"store.ErrTxnConflict", store.ErrTxnConflict},
		{"dberr.ErrConstraintViolation", dberr.ErrConstraintViolation},
		{"channels.ErrChannelNotFound", channels.ErrChannelNotFound},
		{"catalog.ErrModelExists", catalog.ErrModelExists},
		{"catalog.ErrCanonicalTaken", catalog.ErrCanonicalTaken},
		{"catalog.ErrUpstreamTaken", catalog.ErrUpstreamTaken},
		{"catalog.ErrModelInUse", catalog.ErrModelInUse},
		{"catalog.ErrModelNotFound", catalog.ErrModelNotFound},
		{"catalog.ErrAliasIDNotFound", catalog.ErrAliasIDNotFound},
		{"catalog.ErrAliasNotFound", catalog.ErrAliasNotFound},
		{"catalog.ErrChannelNotFound", catalog.ErrChannelNotFound},
		{"pricing.ErrRuleNotFound", pricing.ErrRuleNotFound},
		{"pipeline.ErrNoRule", pipeline.ErrNoRule},
		{"pluginstore.ErrPluginNotFound", pluginstore.ErrPluginNotFound},
		{"errSettingsNotFound", errSettingsNotFound},
		{"errPluginKVKeyNotFound", errPluginKVKeyNotFound},
		{"snapshot.ErrSnapshotNotFound", errSnapshotNotFound},
		{"payment.ErrOrderNotFound", payment.ErrOrderNotFound},
		{"payment.ErrOrderConflict", payment.ErrOrderConflict},
	}

	for _, s := range sentinels {
		t.Run(s.name, func(t *testing.T) {
			err := fmt.Errorf("操作失败: %w", s.err)
			if got := exitCode(err); got != exitConflict {
				t.Errorf("%s 应当落 %d，得到 %d：它没被登记进 conflictErrors", s.name, exitConflict, got)
			}
		})
	}
}

// TestSnapshotNotFoundExitsFour 钉住读路径的「那一行不存在」也落 4。
//
// 它与写路径的约束冲突是同一个形状：下一步都是先去看数据里现在是什么。
// 它原先裸返回落 1，与同类语义在别的 store 包落 4 不一致。
func TestSnapshotNotFoundExitsFour(t *testing.T) {
	cases := map[string]snapshotRead{
		"点名了方向": {RequestID: "req-x", Directions: []string{"chat"}, Filtered: true},
		"没点名方向": {RequestID: "req-x"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			err := snapshotNotFound(req)
			if err == nil {
				t.Fatal("一条都没读到时应当报错")
			}
			if !errors.Is(err, errSnapshotNotFound) {
				t.Errorf("错误链里没有 errSnapshotNotFound：%v", err)
			}
			if got := exitCode(err); got != exitConflict {
				t.Errorf("要读的快照不存在应当落 %d，得到 %d（%v）", exitConflict, got, err)
			}
		})
	}
}

// ── D：体检结论不再经错误出口打第二行 ────────────────────────────────────
//
// 体检类命令（doctor / ledger verify / partition check / secret rotate
// --verify / ledger reconcile）跑完之后，结论就在它们打出来的那份报告里。
// 它们返回的错误只是给调度器看的退出码，所以标成「报告已说明」——main 因此
// 不再补打一行 `larkaxctl: …`（那一行形状像崩溃，且与报告说的是同一件事），
// 而退出码照旧非零。
//
// 这条门按 output_test.go 那批用例的形状写：每种结论各造一个，断言它既
// 「不再被 main 打一遍」又「退出码非零」。少了后者，体检就变成了沉默的 0。
func TestReportOnlyConclusionsAreNotEchoedButKeepANonZeroCode(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
	}{
		{"doctor 不通过", func() error {
			return doctorReport{database: doctorCheck{
				findings: []doctorProblem{{headline: "库里没有 venues 表"}},
			}}.exitErr(nil)
		}},
		{"doctor --live 没打通", func() error {
			return doctorReport{live: doctorLive{enabled: true}}.exitErr(nil)
		}},
		{"ledger verify 一条都没校验到", func() error {
			return ledgerVerifyCause(&store.VerifyReport{}, nil)
		}},
		{"ledger verify 有不合法", func() error {
			return ledgerVerifyCause(&store.VerifyReport{
				CheckedTxns: 3,
				Findings:    []store.VerifyFinding{{Err: errors.New("借贷不平")}},
			}, nil)
		}},
		{"partition check 不通过", func() error {
			return partitionCheckErr([]string{"缺 1 个月的分区"})
		}},
		{"secret rotate --verify 有漏网", func() error {
			return rotateVerifyErr(&channels.VerifyReport{Missing: []channels.RotateRow{{ID: 7}}})
		}},
		{"ledger reconcile 有不一致", func() error {
			return reconcileFindingsErr(&store.Report{
				RunID:    9,
				Findings: []store.Finding{{Kind: store.FindingBalanceMismatch}},
			})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("这一轮体检有问题，应当返回一个非零结论")
			}
			if got := exitCode(err); got == 0 {
				t.Errorf("体检结论要保留非零退出码，得到 0（%v）", err)
			}
			if !reportedOnly(err) {
				t.Errorf("结论应当标成「报告已说明」，main 才不会补打那一行：%v", err)
			}
		})
	}
}

// TestOrdinaryFailuresAreStillEchoed 钉住反面：普通的失败照旧要被 main 打出来。
//
// 上一半的改动只该落在体检结论上。要是标宽了（把参数错、连不上库也标成
// 「报告已说明」），失败就变成一声不吭——那正是这个项目反复防的那类问题。
func TestOrdinaryFailuresAreStillEchoed(t *testing.T) {
	cases := map[string]error{
		"参数错":  usageErr(errors.New("缺少 --id")),
		"连不上库": dbErr(errors.New("连接数据库失败")),
		"约束冲突": fmt.Errorf("启用渠道失败: %w", channels.ErrChannelNotFound),
		"内部错误": internalErr(errors.New("序列化 JSON 输出失败")),
		"其它失败": errors.New("读文件失败"),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if reportedOnly(err) {
				t.Errorf("普通失败不该被判成「报告已说明」，它会一声不吭：%v", err)
			}
		})
	}
}

// TestInternalErrorCarriesThePrefix 钉住 C 类的统一前缀。
func TestInternalErrorCarriesThePrefix(t *testing.T) {
	err := internalErr(errors.New("序列化 JSON 输出失败: boom"))
	if got := err.Error(); !strings.Contains(got, "内部错误: ") {
		t.Errorf("内部故障的那句话要带「内部错误: 」前缀，得到 %q", got)
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("内部故障的退出码应当是 %d，得到 %d", exitFailure, got)
	}
}
