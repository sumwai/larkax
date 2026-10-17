package store

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/testdb"
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
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

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

// base 是本次运行的起点，取自纳秒时间戳。它有 10 亿个取值。
var base = time.Now().UnixNano() % 1_000_000_000

// nextSeq 取下一个用例序号。
//
// 序号只由调用顺序决定，而 `go test` 每次都按同一个顺序跑同一批用例，所以
// **同一个用例在两次运行里拿到的是同一个序号**。两次运行的同一个用例之间，
// 唯一的差别就是 base——slotAt 的时段依赖这一点（见 slotAtFrom）。
func nextSeq() int64 {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return seq.n
}

// nextID 由本次运行的起点与序号拼出一个标识。
//
// 两次运行拼出同一个标识，需要 (base1-base2)*1000 == n2-n1；序号是本次运行里
// 的调用次序，三位数封顶（每个用例只用几个），于是只剩「序号相同、base 也
// 相同」这一条路，也就是十亿分之一的概率。时段那边不是这样：它当初把 base
// 的低位整段丢掉了（见 slotAtFrom 的注释）。
func nextID() int64 { return base*1000 + nextSeq() }

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
//
// ctx 由调用方传进来，不在这里造一个 context.Background()：这一写属于哪次
// 用例、受不受那次用例的超时约束，是调用方才知道的事（contextcheck 报的
// 就是这一条，见 clientCanceledAt）。自己造一个的话，用例超时了这次插入
// 还挂在库里，而挂着的是一条 INSERT。
func seedAPIKey(ctx context.Context, t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(ctx, `
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

// upstreamModelOf 读一条账单记下的**上游**模型名。没记则 Valid 为 false。
//
// 与 channelOf 同一类：这一列也只有选路之后才知道，所以预留那一行必然是
// NULL，值由结算补上。返回 NullString 而不是 string，是为了让「没记」与
// 「记了个空串」在用例里分得开——后者是落库层的写法出了问题。
func upstreamModelOf(t *testing.T, db *sql.DB, requestID string) sql.NullString {
	t.Helper()
	var model sql.NullString
	qerr := db.QueryRowContext(context.Background(),
		`SELECT upstream_model FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&model)
	if qerr != nil {
		t.Fatalf("读账单的上游模型名失败: %v", qerr)
	}
	return model
}

// upstreamFailureOf 读一条账单上「上游失败的依据」那两列。
//
// 两个都返回可空类型：它们恒为 NULL 是**正常**的一种取值（这一行没有上游
// 失败的证据），而不是「没读到」。用例要分辨的是「NULL」与「写了个 0 /
// 空串进去」——后者是落库层的写法出了问题，而按 string 读回来时两者长得
// 一模一样（见 Release 里 NULLIF 那一步的注释）。
func upstreamFailureOf(t *testing.T, db *sql.DB, requestID string) (sql.NullInt64, sql.NullString) {
	t.Helper()
	var (
		status sql.NullInt64
		code   sql.NullString
	)
	qerr := db.QueryRowContext(context.Background(),
		`SELECT upstream_status, error_code FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&status, &code)
	if qerr != nil {
		t.Fatalf("读账单的上游失败依据失败: %v", qerr)
	}
	return status, code
}

// channelAndModelOf 读一条账单上「这次打到了哪儿」那两列。
//
// 两个都返回可空类型，理由与 upstreamFailureOf 相同：NULL 是**正常**的一种
// 取值（这一次没有选中渠道），而用例要分辨的正是「NULL」与「写了个 0 /
// 空串进去」——0 在库里读成「渠道 0」，那个 id 不存在。
func channelAndModelOf(t *testing.T, db *sql.DB, requestID string) (sql.NullInt64, sql.NullString) {
	t.Helper()
	var (
		channelID sql.NullInt64
		model     sql.NullString
	)
	qerr := db.QueryRowContext(context.Background(),
		`SELECT channel_id, upstream_model FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&channelID, &model)
	if qerr != nil {
		t.Fatalf("读账单的渠道与上游模型名失败: %v", qerr)
	}
	return channelID, model
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

// pricedAt 是测试里给 Charge.PricedAt 用的受理时刻。
//
// 它必须**非零**：落库层拒绝漏设（见 ErrMissingPricedAt），而零值落库是
// 公元 1 年——生产里这个值一定来自受理时刻（pipeline.Reserve 取 p.now()，
// Settle 复用它），所以零值造出来的是库不可能出现的数据。理由与本包里
// reservation() 给 RuleMatchedAt 填 time.Now() 是同一条。
func pricedAt() time.Time { return time.Now() }

// consumeEntries 造一组「一次消费」的分录：用户扣钱、平台收入增加。
//
// 用它而不是 ledger.Consume 是因为后者要商户分成等参数，
// 而本包的测试关心的是**持久化**而不是分录怎么造。
//
// billID 是这组分录关联的 billing_requests.id，0 表示不关联（落库成 NULL）。
// 口径与 ledger.Consume 的 ConsumeParams.BillingRequestID 一致（`> 0` 才写），
// 而生产路径上它**一定非零**：网关在 begin 里把预留拿到的 id 写进
// pipeline.Request.BillingRequestID，pipeline.Settle 再原样带进分录。
//
// 所以传 0 是在造生产造不出来的数据，而这件事在这张表上有实际后果：
// billing_request_id 是「这次请求动了哪些钱」唯一的线索（见 ledger.Entry
// 的注释），而那一列是 NULL 时按它查**恒为空**——回滚有没有发生都看不出
// 区别。这正是 TestUsageEventFailureRollsBackSettlement 曾经的问题。
func consumeEntries(t *testing.T, userID int64, amount string, billID int64) []ledger.Entry {
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
	entries := []ledger.Entry{user, platform}

	if billID > 0 {
		id := billID
		for i := range entries {
			entries[i].BillingRequestID = &id
		}
	}
	return entries
}
