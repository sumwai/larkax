package store

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/money"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 为什么这一层不能用内存实现测
//
// 本包要保证的性质全都**只在真库上存在**：
//
//   - 行锁与加锁顺序（死锁是并发拿锁才有的现象）
//   - `UPDATE ... WHERE` 的影响行数（用它做「够不够」的判断）
//   - 唯一键挡下的重复插入（幂等的落点）
//   - `numeric` 的精度（金额从 Go 的定点转过去再转回来）
//
// 拿内存假装这些，测的是「我以为数据库会怎么做」，而不是它真的会怎么做。
//
// 没有 LARKAX_DB_DSN 时跳过，而不是失败：本机的开发环境未必起着库。
// CI 里那个环境变量一直在，所以这个跳过的分支不会掩盖问题。
func testDB(t *testing.T) *sql.DB {
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

	if err := handle.PingContext(context.Background()); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	// 每个测试文件跑一次迁移。goose 自己记录版本，重复调用是幂等的。
	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(context.Background(), handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

// 每个用例用独立的用户与请求标识，避免彼此干扰。
//
// 不用「每个用例清空全库」：那会让用例之间产生顺序依赖，而且清空
// 与迁移的交错很容易变成一场调试噩梦。用唯一标识更简单也更稳。
// 标识用「时间戳 + 进程内计数」而不是纯计数。
//
// 纯计数在同一个数据库上跑第二次 `go test` 时会从头开始，
// 撞上上一次留下的行——而测试之间不该有跨运行的依赖。
var seq struct {
	sync.Mutex
	n int64
}

// base 是本次运行的起点，取自纳秒时间戳。
var base = time.Now().UnixNano() % 1_000_000_000

func nextID() int64 {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return base*1000 + seq.n
}

// seed 建一个用户，并给他一笔充值，返回用户 id。
//
// # 充值必须走账本，不能直接写余额
//
// 直接把数字塞进 user_wallets.balance 会造出一个**账本里不存在**的余额，
// 于是「钱包等于账本推出来的值」这条不变量在测试的一开始就不成立——
// 而那正是本包要保证的东西。
//
// 真实流程里余额也是这么来的：一笔 topup 分录，然后快照跟着更新。
// 测试数据必须走同一条路，否则测的是一个假世界。
func seed(t *testing.T, db *sql.DB, balance string) int64 {
	t.Helper()
	ctx := context.Background()
	id := 100000 + nextID()

	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "u"+itoa(id), "u"+itoa(id)+"@example.com")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	amt, err := money.Parse(balance)
	if err != nil {
		t.Fatalf("解析初始余额失败: %v", err)
	}

	// 充值：借用户、贷支付通道。与 ledger.Topup 造出来的形状一致。
	txn := ledger.NewTxnID()
	entries := []ledger.Entry{
		{
			TxnID: txn, Account: ledger.UserAccount(id), Asset: ledger.AssetCNY,
			Amount: amt, EntryType: ledger.EntryTopup,
		},
		{
			TxnID: txn, Account: ledger.PaymentGatewayAccount("placeholder-gateway"),
			Asset: ledger.AssetCNY, Amount: amt.Neg(), EntryType: ledger.EntryTopup,
		},
	}
	for _, e := range entries {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
			VALUES ($1, $2, $3, $4::numeric, $5)`,
			string(e.TxnID), e.Account.String(), string(e.Asset),
			e.Amount.String(), string(e.EntryType)); err != nil {
			t.Fatalf("写充值分录失败: %v", err)
		}
	}

	if _, walletErr := db.ExecContext(ctx,
		`INSERT INTO user_wallets (user_id, asset, balance, frozen) VALUES ($1, 'CNY', $2::numeric, 0)`,
		id, balance); walletErr != nil {
		t.Fatalf("建钱包失败: %v", walletErr)
	}
	return id
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// seedChannel 插一条渠道，返回它的 id。
//
// billing_requests.channel_id 有外键指向 channels，所以「这一列写没写对」
// 只能在一个真有渠道行的世界里断言。
func seedChannel(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels (name, type, base_url, credential_enc, status, models)
		VALUES ($1, 'openai', 'https://api.example.com', '\x00'::bytea, 'enabled', ARRAY['gpt-4o'])
		RETURNING id`, "ch"+itoa(nextID())).Scan(&id); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	return id
}

// seedAPIKey 插一把 API Key，返回 api_keys.id。
//
// 直接写一行而不是走 internal/auth 的签发流程：本包要的是
// idempotency_keys.api_key_id 的外键能成立，而这把 key **从不用于鉴权**，
// 所以哈希口径在这里没有意义。把它拉进来会让账本层的测试依赖鉴权层，
// 而那条依赖除了满足一个外键之外什么也不证明。
func seedAPIKey(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO api_keys (user_id, name, key_hash, key_prefix)
		VALUES ($1, 'ledger-test', $2, 'placeholder')
		RETURNING id`,
		userID, "hash-"+itoa(nextID())).Scan(&id); err != nil {
		t.Fatalf("建 API Key 失败: %v", err)
	}
	return id
}

// walletOf 读一个钱包的当前状态。
func walletOf(t *testing.T, db *sql.DB, userID int64) (balance, frozen string) {
	t.Helper()
	qerr := db.QueryRowContext(context.Background(),
		`SELECT balance, frozen FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&balance, &frozen)
	if qerr != nil {
		t.Fatalf("读钱包失败: %v", qerr)
	}
	return balance, frozen
}

// channelOf 读一条账单记下的渠道。没记则 Valid 为 false。
func channelOf(t *testing.T, db *sql.DB, requestID string) sql.NullInt64 {
	t.Helper()
	var id sql.NullInt64
	qerr := db.QueryRowContext(context.Background(),
		`SELECT channel_id FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&id)
	if qerr != nil {
		t.Fatalf("读账单的渠道失败: %v", qerr)
	}
	return id
}

// clientCanceledAt 读账单上「客户端断开」那一刻。
//
// 返回 NULL 有两种来历，这一列分不开也不该分：没取消（结算路径必然知道），
// 或者这一行还没结算过（那时它根本没被写过）。
//
// ctx 由调用方传进来，不在这里造一个 context.Background()：这一读属于哪次
// 用例、受不受那次用例的超时约束，是调用方才知道的事（contextcheck 报的
// 就是这一条）。自己造一个的话，用例超时了这次查询还挂在库里。
func clientCanceledAt(ctx context.Context, t *testing.T, db *sql.DB, requestID string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	qerr := db.QueryRowContext(ctx,
		`SELECT client_canceled_at FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&at)
	if qerr != nil {
		t.Fatalf("读账单的客户端取消时刻失败: %v", qerr)
	}
	return at
}

// ledgerBalance 从**账本**算出余额——这是权威值。
func ledgerBalance(t *testing.T, db *sql.DB, userID int64) money.Amount {
	t.Helper()
	var total string
	qerr := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(amount), 0)::text
		  FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`,
		ledger.UserAccount(userID).String()).Scan(&total)
	if qerr != nil {
		t.Fatalf("汇总账本失败: %v", qerr)
	}
	amt, err := money.Parse(total)
	if err != nil {
		t.Fatalf("解析汇总金额 %q 失败: %v", total, err)
	}
	return amt
}

// consumeEntries 造一组「一次消费」的分录：用户扣钱、平台收入增加。
//
// 用它而不是 ledger.Consume 是因为后者要商户分成等参数，
// 而本包的测试关心的是**持久化**而不是分录怎么造。
func consumeEntries(t *testing.T, userID int64, amount string) []ledger.Entry {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	txn := ledger.NewTxnID()

	user := ledger.Entry{
		TxnID: txn, Account: ledger.UserAccount(userID), Asset: ledger.AssetCNY,
		Amount: amt.Neg(), EntryType: ledger.EntryConsume,
	}
	platform := ledger.Entry{
		TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
		Amount: amt, EntryType: ledger.EntryConsume,
	}
	return []ledger.Entry{user, platform}
}
