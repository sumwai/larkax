package catalog

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 这个文件是不连库的那几条判据：参数校验，以及**读路径的失败方向**。

// brokenDB 返回一个查询必然失败的句柄。
//
// 端口 1 上没有 PostgreSQL，驱动会在连接阶段就失败——这条用例要的是「查不出来」
// 这件事，而不是某个具体的库错误。用真驱动而不是桩：ResolveInbound 的判据是
// 「错误有没有被往上抛」，而桩会把「错误从哪来」这件事一起造出来。
func brokenDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("打开句柄失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 读不出来时**返回错误**，不吞成「这个名字没有别名」。
//
// # 吞掉的现场
//
// 返回 `name, nil` 看起来与「没命中」一模一样，于是请求拿别名去选路，报
// 「查无此模型」——排查方向整个指向目录，而真正的原因（一次数据库故障）
// 没有任何人看见。这条用例钉的就是这个方向：失败要往上走。
func TestResolveInboundPropagatesDBError(t *testing.T) {
	ctx := context.Background()
	cat := New(brokenDB(t))

	got, err := cat.ResolveInbound(ctx, "fast")
	if err == nil {
		t.Fatalf("查询失败却返回了 (%q, nil)：那会把一次数据库故障变成「这个名字没有别名」", got)
	}
	if got != "" {
		t.Errorf("失败时不该同时交出一个名字，得到 %q", got)
	}

	// 遮蔽口径同一个方向：失败就说失败，不能说「没有遮蔽」。
	if _, err := cat.HasShadowedInboundAlias(ctx, "fast"); err == nil {
		t.Error("HasShadowedInboundAlias 把查询失败吞成了「没有遮蔽」")
	}
	if _, err := cat.InboundAliases(ctx); err == nil {
		t.Error("InboundAliases 把查询失败吞成了「空清单」")
	}
}

// 空名字在碰库之前就被拒。
//
// 空名字查库没有意义（它不可能是一条别名的键），而让它走到 SQL 那一步的
// 表现是一句驱动原文，读的人不知道错在哪。
func TestInboundReadPathsRejectEmptyName(t *testing.T) {
	ctx := context.Background()
	cat := New(brokenDB(t))

	if _, err := cat.ResolveInbound(ctx, ""); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("ResolveInbound(\"\") 的错误 = %v，期望在碰库之前就报名字为空", err)
	}
	if _, err := cat.HasShadowedInboundAlias(ctx, ""); err == nil {
		t.Error("HasShadowedInboundAlias(\"\") 应当报错")
	}
}

// 写入侧的空名字与非法形状在碰库之前就被拒（命令行退出码 1）。
func TestCreateInboundAliasValidatesBeforeDB(t *testing.T) {
	ctx := context.Background()
	admin := NewAdmin(brokenDB(t))

	cases := []struct {
		name      string
		alias     string
		canonical string
		want      string
	}{
		{"别名为空", "", "m-1", "别名无效"},
		{"规范名为空", "fast", "", "规范名无效"},
		{"别名带空白", "fa st", "m-1", "别名无效"},
		{"规范名路径段为空", "fast", "a//b", "规范名无效"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := admin.CreateInboundAlias(ctx, tc.alias, tc.canonical)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，期望含 %q", err, tc.want)
			}
		})
	}

	if err := admin.DeleteInboundAlias(ctx, ""); err == nil {
		t.Error("删一条空名字的别名应当报错")
	}
}

// ResolveInbound 的 SQL 里有遮蔽判据。
//
// # 为什么这条要读源文本
//
// 遮蔽那条判据是一句 `AND m.canonical_name IS NULL`：把它删掉，编译照过，
// 而「这个名字既是规范名又是别名时，别名不生效」会悄悄变成「别名赢」——
// 只有在真库上撞到那种数据才现形（TestResolveInboundShadowedByCanonical
// 覆盖了语义，但它没有 DSN 时会跳过）。变异器碰不到字符串里的 SQL，所以这里
// 直接读源码把那一句钉住。
//
// 判据只问「LEFT JOIN 与那个 IS NULL 条件在不在」，不数字符：字数会随排版
// 漂移，而这条要钉的是判据本身。
func TestResolveInboundSQLMasksShadowedAliases(t *testing.T) {
	raw, err := os.ReadFile("inbound.go")
	if err != nil {
		t.Fatalf("读 inbound.go 失败: %v", err)
	}
	src := string(raw)

	start := strings.Index(src, "func (c *Catalog) ResolveInbound(")
	if start < 0 {
		t.Fatal("inbound.go 里找不到 ResolveInbound：函数改名或搬走了，这条判据要跟着搬")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}

	// 先确认取到的是函数体而不是整个文件：少了这一段，下面的「含不含」会
	// 在别的函数里碰巧命中，用例就什么都钉不住了。
	if !strings.Contains(body, "inbound_model_aliases") {
		t.Fatalf("取到的片段里没有那张表，这条判据取错了地方：\n%s", body)
	}
	if !strings.Contains(body, "LEFT JOIN model_catalog") {
		t.Errorf("ResolveInbound 没有 LEFT JOIN 目录表：遮蔽判据靠它才成立。函数体现在是：\n%s", body)
	}
	if !strings.Contains(body, "m.canonical_name IS NULL") {
		t.Errorf("ResolveInbound 少了 `m.canonical_name IS NULL`：删掉它之后，名字同时是规范名与别名时"+
			"别名会赢，而判据要求规范名优先。函数体现在是：\n%s", body)
	}
}
