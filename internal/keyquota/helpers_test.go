package keyquota

import (
	"database/sql"
	"os"
	"testing"

	"github.com/sumwai/larkax/internal/testdb"
)

// 本包原先只有纯函数用例，所以没有 TestMain。SpentMany 的口径是 spentSQL
// 的批量版本，而 CASE / COALESCE / GROUP BY 的语义只有真库给得出来——与
// internal/ledger/store 引这一条的理由相同。

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}
