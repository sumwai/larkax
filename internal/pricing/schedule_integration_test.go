package pricing

import (
	"context"
	"strings"
	"testing"
)

// mustSchedule 解析一份定价表，不成立就结束用例。
func mustSchedule(t *testing.T, raw string) *Schedule {
	t.Helper()
	sched, err := ParseSchedule([]byte(raw))
	if err != nil {
		t.Fatalf("解析定价表失败: %v", err)
	}
	return sched
}

// createSchedule 通过管理面建一条带定价表的规则，并登记用完删掉。
//
// 不用 createRule：它会给空表达式填一个默认值，而带定价表的规则必须让
// Expr 为空（两者只能给一个）。
func createSchedule(t *testing.T, admin *Admin, name string, sched *Schedule) int64 {
	t.Helper()
	if name == "" {
		name = unique("sched")
	}
	id, err := admin.Create(context.Background(), CreateRequest{
		Scope:    ScopePlatform,
		Name:     name,
		Schedule: sched,
	})
	if err != nil {
		t.Fatalf("带定价表创建失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(context.Background(),
			`DELETE FROM pricing_rules WHERE id = $1`, id)
	})
	return id
}

// TestScheduleAdminCreateStoresBoth 钉住不变式：schedule 非空时 expr 恰好是
// 它编译出来的结果，而且两栏都真的落了库。
func TestScheduleAdminCreateStoresBoth(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewAdmin(db)

	sched := mustSchedule(t, exampleSchedule)
	id := createSchedule(t, admin, "", sched)

	info, err := admin.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if info.Schedule == nil {
		t.Fatal("读回来的规则没有定价表")
	}
	if info.Schedule.TZ != "Asia/Shanghai" {
		t.Errorf("读回来的 tz = %q", info.Schedule.TZ)
	}
	if len(info.Schedule.Overrides) != 5 {
		t.Errorf("读回来的覆盖有 %d 条，期望 5 条", len(info.Schedule.Overrides))
	}

	// expr 必须就是这张表编译出来的那一份，不能是空串或别的什么。
	want, err := info.Schedule.Validate()
	if err != nil {
		t.Fatalf("读回来的表校验失败: %v", err)
	}
	if info.Expr != want {
		t.Errorf("落库的 expr 与表编译结果不一致：\n落库：%s\n编译：%s", info.Expr, want)
	}

	// 列里确实是 jsonb，不是某个内存里的假象。
	var raw string
	if err := db.QueryRowContext(ctx,
		`SELECT schedule::text FROM pricing_rules WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("读 schedule 列失败: %v", err)
	}
	if !strings.Contains(raw, `"tz"`) {
		t.Errorf("schedule 列 = %s", raw)
	}
}

// TestScheduleAdminCreateRejectsBoth 钉住「表和表达式只能给一个」。
func TestScheduleAdminCreateRejectsBoth(t *testing.T) {
	admin := NewAdmin(testDB(t))

	_, err := admin.Create(context.Background(), CreateRequest{
		Scope:    ScopePlatform,
		Name:     unique("both"),
		Expr:     `{"prompt": 1}`,
		Schedule: mustSchedule(t, exampleSchedule),
	})
	if err == nil || !strings.Contains(err.Error(), "只能给一个") {
		t.Fatalf("两者同时给的错误 = %v，期望「只能给一个」", err)
	}
}

// TestScheduleAdminNeedsCalendar 钉住「要用日历但实例没接日历就不让存」。
//
// 没接日历的管理面用 &Admin{db: db} 构造：NewAdmin 会自己接上日历，
// 而这一格要证明的正是「接了日历」与「没接」在写入路径上不是同一回事。
func TestScheduleAdminNeedsCalendar(t *testing.T) {
	db := testDB(t)
	noCalendar := &Admin{db: db}

	calendarSchedule := mustSchedule(t, `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [{"day_kind": "rest", "prompt": 2}]
	}`)
	_, err := noCalendar.Create(context.Background(), CreateRequest{
		Scope:    ScopePlatform,
		Name:     unique("nocale"),
		Schedule: calendarSchedule,
	})
	if err == nil || !strings.Contains(err.Error(), "没有接日历") {
		t.Fatalf("需要日历却没有日历时的错误 = %v", err)
	}

	// 不用日历的维度（星期、时段、门槛）不受影响。
	plain := mustSchedule(t, `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [{"weekdays": ["fri"], "prompt": 2}]
	}`)
	id := createSchedule(t, noCalendar, "", plain)
	if _, err := noCalendar.Get(context.Background(), id); err != nil {
		t.Fatalf("不需要日历的表被拒了: %v", err)
	}
}

// TestScheduleAdminUpdateExprNeedsClear 钉住「带表的规则改 expr 必须显式清表」。
func TestScheduleAdminUpdateExprNeedsClear(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewAdmin(db)
	id := createSchedule(t, admin, "", mustSchedule(t, exampleSchedule))

	handwritten := `{"prompt": 0.5}`
	err := admin.Update(ctx, UpdateRequest{ID: id, Expr: &handwritten})
	if err == nil || !strings.Contains(err.Error(), "clear-schedule") {
		t.Fatalf("带表的规则直接改 expr 的错误 = %v，期望提示 --clear-schedule", err)
	}

	// 表还在，价格没被改动。
	info, err := admin.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if info.Schedule == nil {
		t.Fatal("失败的修改把表清掉了")
	}

	// 显式清表 + 给表达式才放行。
	if updateErr := admin.Update(ctx, UpdateRequest{
		ID: id, Expr: &handwritten, ClearSchedule: true,
	}); updateErr != nil {
		t.Fatalf("--clear-schedule 之后改 expr 失败: %v", updateErr)
	}
	info, err = admin.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if info.Schedule != nil {
		t.Error("清表之后 Schedule 仍非 nil")
	}
	if info.Expr != handwritten {
		t.Errorf("表达式 = %q，期望 %q", info.Expr, handwritten)
	}

	// 版本历史留了这一版：表达式真的变了。
	versions := ruleVersions(t, db, id)
	if len(versions) != 2 || versions[1] != handwritten {
		t.Errorf("版本历史 = %v，期望最后一条是 %q", versions, handwritten)
	}
}

// TestScheduleAdminUpdateReplacesTable 钉住「换成另一张表时 expr 跟着变」。
func TestScheduleAdminUpdateReplacesTable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewAdmin(db)
	id := createSchedule(t, admin, "", mustSchedule(t, exampleSchedule))

	replacement := mustSchedule(t, `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 0.25, "completion": 0.5}
	}`)
	if err := admin.Update(ctx, UpdateRequest{ID: id, Schedule: replacement}); err != nil {
		t.Fatalf("换表失败: %v", err)
	}

	info, err := admin.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if info.Schedule == nil || len(info.Schedule.Overrides) != 0 {
		t.Errorf("换表之后的覆盖 = %+v", info.Schedule)
	}
	if want := `{"completion": 0.5, "prompt": 0.25}`; info.Expr != want {
		t.Errorf("换表之后的 expr = %q，期望 %q", info.Expr, want)
	}
	versions := ruleVersions(t, db, id)
	if len(versions) != 2 {
		t.Errorf("版本历史有 %d 版，期望 2 版（建 + 换表）", len(versions))
	}
}

// TestScheduleAdminClearKeepsPrice 钉住「只清表不改价」。
func TestScheduleAdminClearKeepsPrice(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewAdmin(db)
	id := createSchedule(t, admin, "", mustSchedule(t, exampleSchedule))

	before, err := admin.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if updateErr := admin.Update(ctx, UpdateRequest{ID: id, ClearSchedule: true}); updateErr != nil {
		t.Fatalf("清表失败: %v", updateErr)
	}
	after, err := admin.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if after.Schedule != nil {
		t.Error("清表之后 Schedule 仍非 nil")
	}
	if after.Expr != before.Expr {
		t.Errorf("只清表却改了表达式：%q → %q", before.Expr, after.Expr)
	}
	// 表达式没变就不该多留一版：版本历史记的是成交价的依据。
	if versions := ruleVersions(t, db, id); len(versions) != 1 {
		t.Errorf("只清表却留了 %d 版历史，期望 1 版", len(versions))
	}
}

// TestScheduleAdminUpdateBothRejected 钉住 update 上「表与表达式二选一」。
func TestScheduleAdminUpdateBothRejected(t *testing.T) {
	admin := NewAdmin(testDB(t))
	id := createSchedule(t, admin, "", mustSchedule(t, exampleSchedule))

	expr := `{"prompt": 1}`
	err := admin.Update(context.Background(), UpdateRequest{
		ID: id, Expr: &expr, Schedule: mustSchedule(t, exampleSchedule),
	})
	if err == nil || !strings.Contains(err.Error(), "只能给一个") {
		t.Fatalf("两者同时给的错误 = %v，期望「只能给一个」", err)
	}
}

// TestScheduleReadRejectsBrokenJSON 钉住「库里的表坏了整次报错，不当没有表」。
//
// 绕过写入路径改过的行不能让清单声称它是一条手写规则——expr 里可能正是
// 那张表的产物，两份说法分家之后没人分得清以谁为准。
func TestScheduleReadRejectsBrokenJSON(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewAdmin(db)
	id := createSchedule(t, admin, "", mustSchedule(t, exampleSchedule))

	if _, err := db.ExecContext(ctx,
		`UPDATE pricing_rules SET schedule = '{"tz": 1}'::jsonb WHERE id = $1`, id); err != nil {
		t.Fatalf("造坏行失败: %v", err)
	}
	_, err := admin.Get(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "不是合法的定价表") {
		t.Fatalf("读坏表的错误 = %v", err)
	}
}
