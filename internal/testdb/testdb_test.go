package testdb

import (
	"regexp"
	"strings"
	"testing"
)

// 库名是隔离的**唯一**依据：两个包算出同一个名字，隔离就没了，而且没有任何
// 一条用例会因此变红——它们只是又回到了共用库上。所以这几个判据要钉住。

var validIdent = regexp.MustCompile(`^[a-z0-9_]+$`)

func TestDatabaseNameIsDerivedFromPackagePath(t *testing.T) {
	cases := map[string]string{
		"internal/ledger/store":   "larkax_test_internal_ledger_store",
		"internal/plugin/store":   "larkax_test_internal_plugin_store",
		"internal/calendar/store": "larkax_test_internal_calendar_store",
		"cmd/larkaxctl":           "larkax_test_cmd_larkaxctl",
		".":                       "larkax_test_root",
	}
	for rel, want := range cases {
		if got := databaseNameFor(rel); got != want {
			t.Errorf("databaseNameFor(%q) = %q，期望 %q", rel, got, want)
		}
	}
}

// 三个 `store` 包必须得到三个不同的库名：包名在仓库里重名，路径不重名。
func TestPackagesWithSameNameGetDifferentDatabases(t *testing.T) {
	names := []string{
		databaseNameFor("internal/ledger/store"),
		databaseNameFor("internal/plugin/store"),
		databaseNameFor("internal/calendar/store"),
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			t.Errorf("库名重复：%q", n)
		}
		seen[n] = true
	}
}

// 超长路径要截断，但不能截出撞名，也不能超过 PostgreSQL 的 63 字节上限。
func TestLongPathsAreTruncatedAndDistinct(t *testing.T) {
	prefix := strings.Repeat("very-long-package-segment/", 8)
	a := databaseNameFor(prefix + "alpha")
	b := databaseNameFor(prefix + "beta")

	if len(a) > identMaxBytes {
		t.Errorf("库名 %q 长度 %d，超过上限 %d", a, len(a), identMaxBytes)
	}
	if len(b) > identMaxBytes {
		t.Errorf("库名 %q 长度 %d，超过上限 %d", b, len(b), identMaxBytes)
	}
	if a == b {
		t.Errorf("截断之后撞名了：%q", a)
	}
	if !validIdent.MatchString(a) {
		t.Errorf("库名 %q 不是合法标识符", a)
	}
}

// 名字只由包路径决定：同一份代码在任何机器、任何 worktree 上算出来的库名
// 必须一样（锁定、清理都靠这个名字对应）。
func TestDatabaseNameIsStable(t *testing.T) {
	first := databaseNameFor("internal/assembly")
	second := databaseNameFor("internal/assembly")
	if first != second {
		t.Errorf("同一个包算出了两个名字：%q 与 %q", first, second)
	}
}

// 大写与分隔符统一成下划线，保证输出能直接放进未加引号的标识符位置
// （我们也确实加了引号，但那是第二道保险，不是唯一一道）。
func TestSanitizeSlug(t *testing.T) {
	cases := map[string]string{
		"internal/foo-bar": "internal_foo_bar",
		"Internal/Foo.Bar": "internal_foo_bar",
		"":                 "root",
		".":                "root",
	}
	for in, want := range cases {
		if got := sanitizeSlug(in); got != want {
			t.Errorf("sanitizeSlug(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 引号转义：即使名字来自别处（以后有人给这里加参数），也不能拼出第二条 SQL。
func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent = %q", got)
	}
}

// advisory lock 的键由库名决定：同名库必须算出同一个键（否则并发运行不会
// 排队），不同库名必须不同（否则两个包会互相阻塞）。
func TestAdvisoryKey(t *testing.T) {
	const name = "larkax_test_internal_auth"
	first := advisoryKey(name)
	second := advisoryKey(name)
	if first != second {
		t.Error("同名库算出了不同的锁键")
	}
	if first == advisoryKey("larkax_test_internal_catalog") {
		t.Error("不同库算出了同一个锁键")
	}
}
