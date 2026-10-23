package catalog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
)

// containsName 判断一条错误文案里有没有点名某个名字。
//
// 按子串判而不是按整句：文案会随措辞调整，而「点没点名」是这些用例真正
// 要钉住的东西。
func containsName(err error, name string) bool {
	return err != nil && strings.Contains(err.Error(), name)
}

// 这个文件钉住入站别名的两条语义与写入侧的四条拒绝。
//
// # 为什么这些必须在真库上跑
//
// 遮蔽那条判据是一句 SQL（LEFT JOIN + `m.canonical_name IS NULL`）：
// 「这个名字既是规范名又是别名时，别名不生效」在 Go 里读起来与「先查目录、
// 没查到再查别名」一模一样，而后者会在两次查询之间留一个窗口，也会在最常见
// 的部署上多一次往返。只有真库能证明那句话在 SQL 层是对的。
//
// 其余几条撞的是主键与目录查询，同样是 SQL 的事。

// createTestModel 在目录里建一个规范模型，返回它的名字。
func createTestModel(t *testing.T, db *sql.DB, canonical string) {
	t.Helper()
	_, err := NewAdmin(db).Create(context.Background(), CreateModelRequest{
		CanonicalName: canonical,
		DisplayName:   "展示名 " + canonical,
	})
	if err != nil {
		t.Fatalf("建规范模型 %q 失败: %v", canonical, err)
	}
}

// 命中一条别名时返回规范名。
func TestResolveInboundReturnsCanonical(t *testing.T) {
	db := testDB(t)
	canonical := uniqueName("model")
	createTestModel(t, db, canonical)
	alias := uniqueName("short")

	if err := NewAdmin(db).CreateInboundAlias(context.Background(), alias, canonical); err != nil {
		t.Fatalf("建入站别名失败: %v", err)
	}

	got, err := New(db).ResolveInbound(context.Background(), alias)
	if err != nil {
		t.Fatalf("ResolveInbound 出错: %v", err)
	}
	if got != canonical {
		t.Errorf("ResolveInbound(%q) = %q，期望 %q", alias, got, canonical)
	}

	// 规范名自己原样返回：一条别名都没有的模型也要能过。
	got, err = New(db).ResolveInbound(context.Background(), canonical)
	if err != nil {
		t.Fatalf("ResolveInbound 出错: %v", err)
	}
	if got != canonical {
		t.Errorf("ResolveInbound(%q) = %q，期望原样", canonical, got)
	}
}

// 没命中任何别名时原样返回，不报错。
//
// 「查不到就报错」会把一个一条别名都没配的部署的每一次请求都打死——那是把
// 一个能用的部署变成完全不能用。
func TestResolveInboundFallsBackToIdentity(t *testing.T) {
	db := testDB(t)
	name := uniqueName("nothing")

	got, err := New(db).ResolveInbound(context.Background(), name)
	if err != nil {
		t.Fatalf("ResolveInbound 出错: %v", err)
	}
	if got != name {
		t.Errorf("ResolveInbound(%q) = %q，期望原样", name, got)
	}

	// 遮蔽口径也要说「没有」。
	shadowed, err := New(db).HasShadowedInboundAlias(context.Background(), name)
	if err != nil {
		t.Fatalf("HasShadowedInboundAlias 出错: %v", err)
	}
	if shadowed {
		t.Errorf("%q 连别名都没有，谈不上被遮蔽", name)
	}
}

// 名字同时是规范模型与别名时：规范名赢、别名被遮蔽、不报错。
//
// 这条是写入侧拒绝的反面：写入侧不许建这种别名，但目录行可以先有、别名
// 后建（或者目录行是绕过写入路径进来的），所以读路径必须有一个确定的答案，
// 而且不能是「报错」——那会让一次请求因为它用了一个合法名字而失败。
func TestResolveInboundShadowedByCanonical(t *testing.T) {
	db := testDB(t)
	name := uniqueName("both")
	createTestModel(t, db, name)

	// 绕过写入侧的校验直接插：那种数据只能这么造出来，而它正是要测的东西。
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO inbound_model_aliases (alias, canonical_name)
		VALUES ($1, $2)`, name, uniqueName("elsewhere")); err != nil {
		t.Fatalf("插入被遮蔽的别名失败: %v", err)
	}

	got, err := New(db).ResolveInbound(context.Background(), name)
	if err != nil {
		t.Fatalf("ResolveInbound 出错: %v", err)
	}
	if got != name {
		t.Errorf("ResolveInbound(%q) = %q，期望它自己——规范名优先，别名被遮蔽", name, got)
	}

	// 诊断口径要能把这件事说出来：它是一条**被遮蔽**的别名。少了这一句，
	// trace 只能打一个含糊的「不是别名」，读的人会以为那条别名没建。
	shadowed, err := New(db).HasShadowedInboundAlias(context.Background(), name)
	if err != nil {
		t.Fatalf("HasShadowedInboundAlias 出错: %v", err)
	}
	if !shadowed {
		t.Errorf("%q 有一条同名别名且它自己是规范名，HasShadowedInboundAlias 应当说是", name)
	}
}

// 写入侧拒绝与规范名同名的别名，且点名那个名字。
func TestCreateInboundAliasRejectsCanonicalName(t *testing.T) {
	db := testDB(t)
	name := uniqueName("clash")
	createTestModel(t, db, name)

	err := NewAdmin(db).CreateInboundAlias(context.Background(), name, uniqueName("other"))
	if !errors.Is(err, ErrAliasIsCanonical) {
		t.Fatalf("错误 = %v，期望 ErrAliasIsCanonical", err)
	}
	if !containsName(err, name) {
		t.Errorf("错误文案 %q 没有点名冲突的名字 %q：下一步是改哪一个名字，全看这一句", err, name)
	}
}

// 写入侧拒绝指向不存在规范名的别名，且点名那个名字。
//
// 不拦的话，每个用它的请求都会在选路处报「查无此模型」，而那条消息不会
// 提到别名——排查方向指向目录，而错在别名。
func TestCreateInboundAliasRejectsUnknownCanonical(t *testing.T) {
	db := testDB(t)
	alias := uniqueName("short")
	missing := uniqueName("ghost")

	err := NewAdmin(db).CreateInboundAlias(context.Background(), alias, missing)
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("错误 = %v，期望 ErrModelNotFound", err)
	}
	if !containsName(err, missing) {
		t.Errorf("错误文案 %q 没有点名不存在的规范名 %q", err, missing)
	}
}

// 已存在的别名不静默覆盖，且点名那个别名。
//
// 静默覆盖的现场是：一个已经在用的短名某天起指向另一个模型，所有调用方都
// 看不出来。
func TestCreateInboundAliasRejectsDuplicate(t *testing.T) {
	db := testDB(t)
	first := uniqueName("model")
	second := uniqueName("model")
	createTestModel(t, db, first)
	createTestModel(t, db, second)
	alias := uniqueName("short")

	if err := NewAdmin(db).CreateInboundAlias(context.Background(), alias, first); err != nil {
		t.Fatalf("第一次建入站别名失败: %v", err)
	}
	err := NewAdmin(db).CreateInboundAlias(context.Background(), alias, second)
	if !errors.Is(err, ErrInboundAliasExists) {
		t.Fatalf("错误 = %v，期望 ErrInboundAliasExists", err)
	}
	if !containsName(err, alias) {
		t.Errorf("错误文案 %q 没有点名别名 %q", err, alias)
	}

	// 指向没被改掉：撞了之后原来那条还在，仍指第一个模型。
	got, err := New(db).ResolveInbound(context.Background(), alias)
	if err != nil {
		t.Fatalf("ResolveInbound 出错: %v", err)
	}
	if got != first {
		t.Errorf("别名现在指向 %q，期望仍是 %q", got, first)
	}
}

// 删一条不存在的别名报错并点名；删掉之后它不再解析。
func TestDeleteInboundAlias(t *testing.T) {
	db := testDB(t)
	canonical := uniqueName("model")
	createTestModel(t, db, canonical)
	alias := uniqueName("short")

	err := NewAdmin(db).DeleteInboundAlias(context.Background(), alias)
	if !errors.Is(err, ErrInboundAliasNotFound) {
		t.Fatalf("删一条不存在的别名，错误 = %v，期望 ErrInboundAliasNotFound", err)
	}
	if !containsName(err, alias) {
		t.Errorf("错误文案 %q 没有点名别名 %q", err, alias)
	}

	if createErr := NewAdmin(db).CreateInboundAlias(context.Background(), alias, canonical); createErr != nil {
		t.Fatalf("建入站别名失败: %v", createErr)
	}
	if deleteErr := NewAdmin(db).DeleteInboundAlias(context.Background(), alias); deleteErr != nil {
		t.Fatalf("删除入站别名失败: %v", deleteErr)
	}
	got, err := New(db).ResolveInbound(context.Background(), alias)
	if err != nil {
		t.Fatalf("ResolveInbound 出错: %v", err)
	}
	if got != alias {
		t.Errorf("删掉之后 ResolveInbound(%q) = %q，期望原样", alias, got)
	}
}

// 清单按别名排序，空表返回空切片（不是 nil）。
func TestInboundAliasesListing(t *testing.T) {
	db := testDB(t)
	canonical := uniqueName("model")
	createTestModel(t, db, canonical)

	first := uniqueName("a-short")
	second := uniqueName("b-short")
	admin := NewAdmin(db)
	if err := admin.CreateInboundAlias(context.Background(), second, canonical); err != nil {
		t.Fatalf("建入站别名失败: %v", err)
	}
	if err := admin.CreateInboundAlias(context.Background(), first, canonical); err != nil {
		t.Fatalf("建入站别名失败: %v", err)
	}

	all, err := New(db).InboundAliases(context.Background())
	if err != nil {
		t.Fatalf("InboundAliases 出错: %v", err)
	}
	if all == nil {
		t.Fatal("空清单要返回空切片，不能是 nil")
	}
	// 这两条都在清单里，且 first 排在 second 前面（按 alias 排序）。
	posFirst, posSecond := -1, -1
	for i, a := range all {
		switch a.Alias {
		case first:
			posFirst = i
		case second:
			posSecond = i
		}
	}
	if posFirst < 0 || posSecond < 0 {
		t.Fatalf("清单里少了刚建的两条：%v", all)
	}
	if posFirst > posSecond {
		t.Errorf("清单没按别名排序：%q 排在了 %q 后面", first, second)
	}
	if all[posFirst].CanonicalName != canonical {
		t.Errorf("%q 的规范名 = %q，期望 %q", first, all[posFirst].CanonicalName, canonical)
	}
}

// 还挂着入站别名时不许删规范模型。
//
// # 漏了这一路的现场
//
// 先建入站别名、再删规范模型：countReferences 数不到这张表（它没有外键），
// 删除成功，别名于是指向一个不存在的名字。之后每个用这条别名的请求都会在
// 选路处报「查无此模型」——那条消息不会提到别名，排查方向指向目录。
func TestDeleteModelRefusesWhenInboundAliased(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	createTestModel(t, db, model)
	alias := uniqueName("short")
	if err := NewAdmin(db).CreateInboundAlias(context.Background(), alias, model); err != nil {
		t.Fatalf("建入站别名失败: %v", err)
	}

	err := NewAdmin(db).Delete(context.Background(), model)
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("err = %v，期望 ErrModelInUse", err)
	}
	// 报错要能指出被什么挡住了。这一句里的「1 条入站别名」是本单补的那一路：
	// 只数出站别名与计价规则时，这里的数字会是 0 与 0，而删除仍然成功。
	if !strings.Contains(err.Error(), "1 条入站别名") {
		t.Errorf("报错没有点名入站别名的条数：%s", err)
	}
	if _, err := New(db).Model(context.Background(), model); err != nil {
		t.Errorf("被拒之后模型不该消失: %v", err)
	}

	// 解除引用之后删得掉：拒绝不是永久的。
	if err := NewAdmin(db).DeleteInboundAlias(context.Background(), alias); err != nil {
		t.Fatalf("删入站别名失败: %v", err)
	}
	if err := NewAdmin(db).Delete(context.Background(), model); err != nil {
		t.Fatalf("解除引用之后应当删得掉: %v", err)
	}
}

// countReferences 的 SQL 里必须问过 inbound_model_aliases。
//
// # 为什么这条只能读源文本
//
// 变异器碰不到字符串里的 SQL：把那条查询整段删掉，编译照过、其它用例照绿
// （它们要么不连库，要么只建了出站别名），而「删模型时数不到入站别名」这条
// 路径要在真库上撞一个特定的前置状态才现形。所以这里直接读源码，钉住那三处
// 查询里有这张表的名字。
//
// 判据只问表名在不在，不数出现几次：次数会随实现微调漂移，而这条要钉的是
// 「这一路引用检查有没有被想起来」。
func TestCountReferencesQueriesInboundAliases(t *testing.T) {
	raw, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatalf("读 admin.go 失败: %v", err)
	}
	src := string(raw)

	start := strings.Index(src, "func (a *Admin) countReferences(")
	if start < 0 {
		t.Fatal("admin.go 里找不到 countReferences：函数改名或搬走了，这条判据要跟着搬")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "inbound_model_aliases") {
		t.Errorf("countReferences 里没有问过 inbound_model_aliases：删模型时数不到入站别名，"+
			"别名会悬空指向一个不存在的规范名。函数体现在是：\n%s", body)
	}
}
