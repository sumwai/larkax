package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/migrate"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 为什么这一层不能用内存实现测
//
// 本包要保证的性质全都**只在真库上存在**：
//
//   - jsonb 列：值按 string 传过去，Postgres 把它解析成 jsonb 再读回来，
//     「写进去的字节与读出来的字节是不是同一个值」只有真库能回答
//   - UPDATE ... WHERE editable 的零行分支：区分「没有这个键」与
//     「这个键不允许改」，靠的是真库给的行数
//   - updated_at 的 now() 与 timestamptz 往返
//
// 没有 LARKAX_DB_DSN 时跳过，而不是失败：本机的开发环境未必起着库。
// CI 里那个环境变量一直在，所以这个跳过的分支不会掩盖问题。
func testDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}

	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if err := handle.PingContext(ctx); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	// goose 自己记录版本，重复调用是幂等的。
	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(ctx, handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

// entryFor 从一份清单里取出一行。找不到就报错，而不是返回零值——
// 零值会让「键不见了」表现为「值不对」，排查方向整个偏掉。
func entryFor(t *testing.T, entries []Entry, key string) Entry {
	t.Helper()

	for _, e := range entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("清单里没有 %q", key)
	return Entry{}
}

// TestListReadsDescriptionColumn 钉住 List 真的读了 description 那一列，
// 且每一个键在库里都有一句非空的说明。
//
// 这一列是配置说明的**唯一**来源（Registry 里没有第二份，见 settings.go 的
// Registry 注释），而「它有没有被读出来」是唯一一件不会自己暴露的事：
// 把这一列从 List 的 SELECT 里去掉，其余用例全绿——写入路径比对的是
// 「写前写后一致」，两边都是空串也算一致；printSettingsList 那个用例用的是
// 手工构造的 Entry。表现则是 settings list 的说明一行不剩，
// 而那正是这一列存在的意义。
//
// 「每个键都非空」同时是删掉 Registry 那份说明的前提：库里是空的话，
// 那个键的说明就彻底没有了。
func TestListReadsDescriptionColumn(t *testing.T) {
	ctx := t.Context()
	src := NewPgSource(testDB(ctx, t))

	entries := mustList(ctx, t, src)
	if len(entries) == 0 {
		t.Fatal("settings 表是空的，这个用例的落点没了")
	}
	for _, e := range entries {
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("settings.%s 读出来的 description 是空的："+
				"要么 List 没读那一列，要么迁移里没给这个键写说明", e.Key)
		}
	}
}

// TestSetWritesAndReadsBack 是写入路径的落点：写进去，再从库里读回来，
// 断言**每一栏都是具体值**。
//
// 「读回来的值与写进去的一样」这一条看起来天经地义，而它恰恰是
// jsonb 那一列最容易出错的地方：值按 string 传还是按 []byte 传、
// 传进去的是 JSON 原文还是被 Go 转义过一遍的字符串，都只有在真库上
// 往返一次才看得出来。
func TestSetWritesAndReadsBack(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	src := NewPgSource(db)

	const key = "request_snapshot.retention_days"
	before := saveSetting(ctx, t, db, src, key)

	// 32 是迁移里的默认值，这里写成 7——一个与默认值不同的数，
	// 否则「没写进去」与「写进去了」读出来一模一样。
	if err := src.Set(ctx, key, json.RawMessage(`7`), 4242); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got := entryFor(t, mustList(ctx, t, src), key)

	if string(got.Value) != `7` {
		t.Errorf("value = %s，期望 7", got.Value)
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != 4242 {
		t.Errorf("updated_by = %s，期望 4242", showPtr(got.UpdatedBy))
	}
	if !got.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at = %v，没有晚于写入前的 %v", got.UpdatedAt, before.UpdatedAt)
	}
	// description 必须**原样保留**：写入只改 value / updated_by / updated_at。
	// 拿 Registry 里那句短说明覆盖掉迁移里更长的那一句，是把信息丢掉，
	// 而这一列存在的意义正是「日久之后还有人知道这个键是干嘛的」。
	if got.Description != before.Description {
		t.Errorf("description 被改动了：\n  写入前 %q\n  写入后 %q", before.Description, got.Description)
	}
	if !got.Editable {
		t.Errorf("editable 被改成了 false")
	}
}

// TestSetRefusesNotEditable 钉住 editable = false 的拦截。
//
// 这一列的含义是「改错会造成严重后果、不欢迎随手调整」，而它生效的地方
// 只能是写入路径——没有别的东西会去读它。
func TestSetRefusesNotEditable(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	src := NewPgSource(db)

	// billing.default_asset 在迁移 00002 里就是 editable = false。
	const key = "billing.default_asset"
	before := saveSetting(ctx, t, db, src, key)
	if before.Editable {
		t.Fatalf("%s 的 editable 是 true，这个用例的落点没了", key)
	}

	err := src.Set(ctx, key, json.RawMessage(`"USD"`), 4242)
	if !errors.Is(err, ErrNotEditable) {
		t.Fatalf("期望 ErrNotEditable，得到 %v", err)
	}
	// 错误信息要能让人知道下一步做什么：这条路径不是坏了，是这个键
	// 本来就不欢迎随手改。
	for _, want := range []string{key, "editable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 里没有 %q", err.Error(), want)
		}
	}

	after := entryFor(t, mustList(ctx, t, src), key)
	if string(after.Value) != string(before.Value) {
		t.Errorf("被拒绝的写入改动了值：%s → %s", before.Value, after.Value)
	}
	if after.UpdatedBy != nil {
		t.Errorf("被拒绝的写入改动了 updated_by：%v", *after.UpdatedBy)
	}
}

// TestSetRefusesKeyMissingFromTable 钉住「键在 Registry 里声明过、但库里
// 没有这一行」这一支。
//
// 它到不了 Load（Load 会因为缺键而报错），但写入路径会走到，而它给出的
// 下一步动作与「这个键不允许改」完全不同：一个是去补迁移，一个是去写迁移
// 里那一列。合成一句话，两条路就分不开了。
func TestSetRefusesKeyMissingFromTable(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	src := NewPgSource(db)

	// 挑一行删掉再放回去。saveSetting 登记的是 upsert，所以「行被删掉了」
	// 这件事它照样还原得回来——而用例中途 Fatal 时它也一样会跑。
	const key = "capture.consent_max_window_hours"
	saveSetting(ctx, t, db, src, key)
	if _, err := db.ExecContext(ctx, `DELETE FROM settings WHERE key = $1`, key); err != nil {
		t.Fatalf("删行失败: %v", err)
	}

	err := src.Set(ctx, key, json.RawMessage(`48`), 4242)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("期望 ErrUnknownKey，得到 %v", err)
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("错误信息 %q 里没有键名", err.Error())
	}
}

// restoreSetting 把一行改回原样。
//
// 这些用例改的是迁移写下的初始配置，而同一个包里的其他用例、以及同一个
// 数据库上的其他包，都指望那些值是原样。改完不还原会让「另一个用例挂了」
// 这件事看起来像它自己的问题。
//
// 用 upsert 而不是 UPDATE：有一处用例把整行删掉了，而 UPDATE 改不到
// 不存在的行时**一行都不改、也不报错**——「还原了」于是变成一件静默地
// 没发生的事。写完之后自己数一遍行数，正是为了挡住这个。
func restoreSetting(ctx context.Context, t *testing.T, db *sql.DB, e Entry) {
	t.Helper()

	var updatedBy any
	if e.UpdatedBy != nil {
		updatedBy = *e.UpdatedBy
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO settings (key, value, description, editable, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (key) DO UPDATE
		   SET value = EXCLUDED.value, description = EXCLUDED.description,
		       editable = EXCLUDED.editable, updated_by = EXCLUDED.updated_by,
		       updated_at = EXCLUDED.updated_at`,
		e.Key, string(e.Value), e.Description, e.Editable, updatedBy, e.UpdatedAt)
	if err != nil {
		t.Fatalf("还原 settings.%s 失败: %v", e.Key, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("还原 settings.%s 写了 %d 行（%v），期望 1 行", e.Key, n, err)
	}
}

// saveSetting 记下这一行现在长什么样，并登记用例结束时把它放回去。
//
// 返回的 Entry 是**写之前**的快照，用例可以拿它断言写入没有动到不该动的列。
func saveSetting(ctx context.Context, t *testing.T, db *sql.DB, src *PgSource, key string) Entry {
	t.Helper()

	before := entryFor(t, mustList(ctx, t, src), key)
	t.Cleanup(func() { restoreSetting(context.WithoutCancel(ctx), t, db, before) })
	return before
}

func mustList(ctx context.Context, t *testing.T, src *PgSource) []Entry {
	t.Helper()

	entries, err := src.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	return entries
}

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}
