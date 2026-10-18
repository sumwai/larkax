package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/ledger"
)

// seedBareUser 只建 users 那一行，不建钱包、不写分录。
//
// 与 helpers_test.go 的 seed 分开：那个建的是「已经充过值的老用户」，
// 而开户用例要的正是**还没有钱包行**的用户。拿 seed 来测等于在一个
// 钱包已经存在的世界里测开户，最要紧的那条路径反而没走到。
func seedBareUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id := 200000 + nextID()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "w"+itoa(id), "w"+itoa(id)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// 开户落下的钱包：余额与冻结都是 0，且**账本里没有任何分录**。
//
// 最后一条是这个用例的重点：0 余额的户不需要一条金额为零的分录来「留痕」
// ——账本拒绝零金额分录，而对账要拿「分录之和」与快照比。这里没有分录，
// 所以权威值与快照值都是 0，两者自洽。
func TestOpenWalletCreatesZeroBalanceWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("开户失败: %v", err)
	}
	if !created {
		t.Fatal("第一次开户应当返回 created=true")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "0.00000000" || frozen != "0.00000000" {
		t.Errorf("余额=%s 冻结=%s，期望都是 0", balance, frozen)
	}

	// 变量名不复用 err：上面那个 err 还在作用域里，这里再 `err :=` 是遮蔽，
	// govet 的 shadow 检查器会报。
	var entries int
	if countErr := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()).Scan(&entries); countErr != nil {
		t.Fatalf("数分录失败: %v", countErr)
	}
	if entries != 0 {
		t.Errorf("开户写下了 %d 条分录，期望 0 条", entries)
	}

	// 权威值（分录之和）也是 0：快照与账本因此对得上，不需要对账去修。
	authoritative, err := s.AccountBalance(ctx, ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("读权威余额失败: %v", err)
	}
	if !authoritative.IsZero() {
		t.Errorf("账本侧余额 = %s，期望 0", authoritative)
	}
}

// 对同一个用户开两次户：第二次什么都不做，余额一分不动。
func TestOpenWalletIsIdempotent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	if created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY); err != nil || !created {
		t.Fatalf("第一次开户: created=%v err=%v", created, err)
	}
	created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("第二次开户不该报错: %v", err)
	}
	if created {
		t.Error("第二次开户返回 created=true，期望 false（已存在）")
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	if rows != 1 {
		t.Errorf("钱包行数 = %d，期望 1", rows)
	}
}

// 并发开户只有一行落库，且恰好一个调用拿到 created=true。
//
// 这条钉的是 OpenWallet 注释里那句「并发由主键 (user_id, asset) 兜底」——
// 把 `ON CONFLICT DO NOTHING` 换成「先查再插」，几个 goroutine 会各自看到
// 「不存在」而都去插，后到的撞唯一键报错，这条用例立刻红。串行的
// 「开两次」用例照不到这一点（见 TestOpenWalletIsIdempotent）。
//
// 要让这条用例真的钉住东西，靠的是三件事：**预热**、闸门、够宽的并发数。
// 前两件少一件，它就变成「看着通过、其实一条都没钉住」——四种组合都实测过，
// 跑的都是「先查再插」那版实现，各三轮：
//
//	预热 + 闸门    → 三轮全红（下面用的就是这一种）
//	预热、无闸门   → 三轮全红
//	无预热 + 闸门  → 三轮全绿（一条都没钉住）
//	无预热、无闸门 → 两绿一红（靠运气）
//
// 决定性的是**预热**：database/sql 懒建连接，而 MaxIdleConns 默认只有 2。
// 池子里没有现成连接时，先拿到连接的那一两个把行插完，其余几个还在建连接
// ——它们看到的都是「已经存在」，竞态窗口一次都撞不上。闸门只是把「同时出发」
// 写进用例里，不依赖调度顺序；没有预热时它反而最坏，goroutine 排好队一个
// 接一个跑完。
//
// 并发数也是同一件事：n=8 时窗口太窄，换成「先查再插」十轮里能漏一轮；n=16
// 四十轮里三十九轮红。漏的那一轮是竞态用例的固有性质——判据是「窗口真的被
// 撞上」，而这要调度恰好把几个 goroutine 叠在一起，不是每次都能。反过来是
// 确定的：真实实现（ON CONFLICT）跑多少轮都绿，所以这条用例不会误报。
//
// 所以下面先把池子开到 n 条并预热，再放闸门。
func TestOpenWalletConcurrent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	const n = 16
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
	prewarmPool(t, db, n)

	start := make(chan struct{})
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		existed  int
		firstErr error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			case ok:
				created++
			default:
				existed++
			}
		}()
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("并发开户出错: %v", firstErr)
	}
	if created != 1 {
		t.Errorf("created=true 的有 %d 个，期望恰好 1 个", created)
	}
	if existed != n-1 {
		t.Errorf("created=false 的有 %d 个，期望 %d 个", existed, n-1)
	}

	var rows int
	if countErr := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); countErr != nil {
		t.Fatalf("数钱包行失败: %v", countErr)
	}
	if rows != 1 {
		t.Errorf("钱包行数 = %d，期望 1", rows)
	}
}

// prewarmPool 让连接池里先有 n 条打开的连接，用完放回池子。
//
// 目的是把「建连接」这段耗时移出并发用例的竞态窗口：调用方与库都在本机，
// 一次查询是几十微秒，而建一条连接是它的若干倍——不预热的话，谁先拿到连接
// 谁就把整件事做完了，剩下的连窗口都进不去（见 TestOpenWalletConcurrent）。
//
// 拿 n 条连接要真的**同时**持有再一起放回：一条一条拿、拿完就还，池子只会
// 复用同一条，预热不到 n 条。
func prewarmPool(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()

	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("预热连接池失败（第 %d 条）: %v", i+1, err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("归还预热的连接失败: %v", err)
		}
	}
}

// 已经有钱包（余额非 0）的用户：开户是一个空操作，不重置余额。
//
// 这条比「开两次」更关键：把已存在的行重建成 0 会让用户的钱凭空消失，
// 而命令照样退出 0。
func TestOpenWalletKeepsExistingBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")

	created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("开户失败: %v", err)
	}
	if created {
		t.Error("已经有钱包行，不该报告「刚建出来」")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("余额变成 %s，期望保持 100", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("冻结额 = %s，期望 0", frozen)
	}
}

func TestOpenWalletRejectsUnsupportedAsset(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	if _, err := s.OpenWallet(ctx, userID, ledger.Asset("USDT")); err == nil {
		t.Fatal("不受支持的资产应当报错")
	} else if !strings.Contains(err.Error(), "不受支持") {
		t.Errorf("错误信息 %q 里没有「不受支持」", err)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	if rows != 0 {
		t.Errorf("被拒的开户留下了 %d 行钱包", rows)
	}
}

func TestOpenWalletRejectsNonPositiveUser(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	for _, id := range []int64{0, -1} {
		if _, err := s.OpenWallet(ctx, id, ledger.AssetCNY); err == nil {
			t.Errorf("用户 id %d 应当被拒", id)
		}
	}
}

// 非法的用户 id 要在**碰库之前**被挡下，而且不能被分到任何一类库上的拒绝里。
//
// 变异：把 `userID <= 0` 改成 `< 0`，0 就被放行、一路走到外键，于是被分类成
// ErrNoSuchUser。命令行按哨兵给退出码（见 cmd/larkaxctl 的 conflictErrors），
// 这一改会把「id 写成了 0」从退出码 1 变成退出码 4——报成一次约束冲突。
// 上面那条用例断言过「0 与负数会被拒」，但它要连库（没有 LARKAX_DB_DSN 时
// 整包跳过），而且只判「有没有错」，判不出错误被分到哪一类。
//
// 这里给的 Store **没有数据库句柄**，而这正是断言的一部分：判据生效时它在
// 碰 s.db 之前就返回，句柄是空的也走得通；判据一旦不生效，这次调用会直接撞上
// 空句柄。于是「非法 id 不碰库」这件事不连库就钉得住。
func TestOpenWalletRejectsNonPositiveUserBeforeTouchingDB(t *testing.T) {
	s := New(nil)
	ctx := context.Background()

	for _, id := range []int64{0, -1} {
		_, err := s.OpenWallet(ctx, id, ledger.AssetCNY)
		if err == nil {
			t.Fatalf("用户 id %d 应当被拒", id)
		}
		// 报的是「id 非法」这件参数问题，不是库那边发生了什么。
		if !strings.Contains(err.Error(), "非法") {
			t.Errorf("用户 id %d 的错误信息 %q 里没有说清是 id 非法", id, err)
		}
		if errors.Is(err, ErrNoSuchUser) {
			t.Errorf("用户 id %d 被分类成了「用户不存在」：%v", id, err)
		}
		if errors.Is(err, ErrConstraintViolation) {
			t.Errorf("用户 id %d 被分类成了「数据库约束冲突」：%v", id, err)
		}
	}
}

// 用户不存在时，错误要指认「哪个用户不存在」，而不是一句外键冲突原文。
//
// 命令行的正常路径会先解析用户、走不到这里；这条覆盖的是绕过解析的调用方
// 与「解析之后用户被删」那个窗口。
//
// **哨兵与文本都要判**：文本是给人看的，哨兵是给调用方分类用的——命令行按它
// 给退出码（见 cmd/larkaxctl 的 conflictErrors）。只判文本的话，把哨兵从错误链
// 里拿掉这条照样绿，而那条路径会悄悄退回退出码 1。
func TestOpenWalletUnknownUser(t *testing.T) {
	db := testDB(t)
	s := New(db)

	const ghost = int64(900000000000000)
	_, err := s.OpenWallet(context.Background(), ghost, ledger.AssetCNY)
	if err == nil {
		t.Fatal("不存在的用户应当报错")
	}
	if !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("错误链里没有 ErrNoSuchUser：%v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息 %q 里没有说清用户不存在", err)
	}
	if !strings.Contains(err.Error(), itoa(ghost)) {
		t.Errorf("错误信息 %q 里没有指认是哪个用户", err)
	}

	// 驱动给的原始错误仍在链里：SQLSTATE 与约束名是现场唯一拿得到的依据，
	// 加一个哨兵是给它加名字，不是替掉它。
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if pgErr.Code != pgForeignKeyViolation {
		t.Errorf("SQLSTATE = %q，期望 %q", pgErr.Code, pgForeignKeyViolation)
	}
	if pgErr.ConstraintName != userWalletsUserIDConstraint {
		t.Errorf("约束名 = %q，期望 %q", pgErr.ConstraintName, userWalletsUserIDConstraint)
	}
}

// 排序器按 (user_id, asset) 排，**asset 是次键**这件事要钉在断言里。
//
// # 为什么不能只断言「排过序了」
//
// sortWalletKeys 存在的唯一理由就是那个次键（见它的注释：只按 user_id 排的话，
// 同一个用户的两种币种之间顺序不定）。而「两次输出一致」这种断言抓不住它：
// 把比较器改成整体倒序，输出同样每次都一样，死锁那一条它也照样满足——这条
// 用例就什么都没钉住。所以这里断言的是**具体的次序**。
//
// 输入故意把 7 号用户的两种资产倒着放（USD 在前）：这样「只按 user_id 挪位置、
// 资产原样不动」也会露出来。
//
// 不需要数据库：比较器只碰传进来的那个切片。
func TestSortWalletKeysPutsAssetSecond(t *testing.T) {
	keys := []WalletKey{
		{UserID: 7, Asset: "USD"},
		{UserID: 3, Asset: "CNY"},
		{UserID: 5, Asset: "USD"},
		{UserID: 3, Asset: "USD"},
		{UserID: 7, Asset: "CNY"},
		{UserID: 5, Asset: "CNY"},
	}
	want := []WalletKey{
		{UserID: 3, Asset: "CNY"},
		{UserID: 3, Asset: "USD"},
		{UserID: 5, Asset: "CNY"},
		{UserID: 5, Asset: "USD"},
		{UserID: 7, Asset: "CNY"},
		{UserID: 7, Asset: "USD"},
	}

	sortWalletKeys(keys)

	if !slices.Equal(keys, want) {
		t.Errorf("排序结果 = %v，期望 %v（先按用户，同一个用户再按资产）", keys, want)
	}
}

// 等价变异（gremlins CONDITIONALS_BOUNDARY，把上面那个比较器里
// `keys[i].UserID < keys[j].UserID` 的 `<` 改成 `<=`）：第一支只在两个 UserID
// 不等时进得来，而两侧不等时 `<` 与 `<=` 逐点相同——没有输入分得出这两版。
//
// 同一个变异器打的**另一支**（`keys[i].Asset < keys[j].Asset` 的 `<` 改成 `<=`）
// 也是等价变异，但**理由不同，别把上面那段抄过来**：这一支没有 `!=` 守卫，两版
// 确实会在「两个键的 UserID 相同、Asset 也相同」时取值不同（`<` 给 false、
// `<=` 给 true）。可那种情形下交换的是**同一个值**，而 sort.Slice 的输出只是
// 输入的一个排列——值相同的两个元素谁在前，从输出里看不出来，WalletKey 又只有
// (UserID, Asset) 这两个字段，没有第三个字段能把「谁原本在前」记下来。所以没有
// 输入分得出这两版，分不开的原因是**输出对值相同的元素不敏感**，而不是
// 「这一支碰不到」。
//
// 穷举印证（把两支抄进一个临时模块跑，不在仓库里）：字母表取 {1A,1B,2A,2B}、
// 长度 0..6 的全部 5461 个输入（含重复键与任意初始次序），逐一排序比对，
// 第二支改 `<=` 后输出不同的输入 0 个。
