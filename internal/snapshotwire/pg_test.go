package snapshotwire

import (
	"context"
	"database/sql"
	"testing"

	// 驱动在这里显式注册：这条用例要自己造一个 *sql.DB，
	// 不能指望别的测试文件顺带把驱动带进来。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// 适配层要把**查询失败**交出来，不能当成成功。
//
// 变异（gremlins CONDITIONALS_NEGATION，把 `if err != nil` 的 `!=` 改成 `==`）：
// 错误分支被反过来，查询失败时返回 `(nil, nil)`——调用方拿到一个 nil 的 Rows
// 加一个 nil 的错误，会当成「查到了零行」继续往下走，而不是报错。
//
// 它没出现在那一轮的存活清单里，是因为它只在真连库那条路上被走到：
// 集成用例没有 DSN 时整包 skip，变异工具按「未覆盖」把它排除在分母之外。
//
// 不需要真库：`sql.Open` 只构造句柄、不连库，`Close` 之后再查，
// database/sql 自己就会从这一句返回「句柄已关闭」。
func TestPgDBQueryContextReportsErrorFromClosedHandle(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://user@localhost:5432/db?sslmode=disable")
	if err != nil {
		t.Fatalf("构造句柄失败: %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatalf("关闭句柄失败: %v", err)
	}

	rows, err := Wrap(db).QueryContext(context.Background(), "select 1")
	if err == nil {
		t.Fatal("句柄已关闭，查询必须报错，不能返回 (nil, nil)")
	}
	if rows != nil {
		t.Errorf("报错时不该交出 Rows，得到 %#v", rows)
	}
}
