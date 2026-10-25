package channels

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/textshape"
)

// 账号是一份凭证的独立实体（见迁移 00067）。一条渠道通过 channel_accounts
// 挂零个、一个或多个账号：零个是合法的中间态（选路跳过），多个是同一个上游
// 开了好几把 key 的池子。
//
// 这些操作与渠道管理同属一个写入面，所以方法挂在 Admin 上：它们共用主密钥
// 封装器与事务边界，`channel create --credential-env` 那种「建渠道 + 建账号 +
// 挂池」才能落在同一个事务里。

const (
	// generatedAccountNamePrefix 是缺省账号名的前缀。
	generatedAccountNamePrefix = "account-"
	// generatedAccountNameLength 是缺省名里随机部分的长度。
	// 36^6 ≈ 2.1e9，撞名要靠重试兜住，而不是靠这个数够大。
	generatedAccountNameLength = 6
	// accountNameAlphabet 是随机部分的字符集。
	accountNameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	// accountNameAttempts 是生成名撞上唯一键时的重试次数。
	accountNameAttempts = 8
)

// ErrAccountNotFound 表示按名字找不到账号。
var ErrAccountNotFound = errors.New("账号不存在")

// ErrAccountExists 表示这个名字已经被另一个账号占了。
var ErrAccountExists = errors.New("账号已存在")

// ErrNoAccount 表示这条渠道一个账号都没挂。
//
// 零账号是合法的中间态（见 CreateRequest.Credential），所以它不是一个
// 「数据坏了」的错误；要探测上游、拉模型清单这类**必须拿明文**的动作
// 才会撞上它。
var ErrNoAccount = errors.New("渠道还没有挂账号")

// AccountChannel 是一个账号挂在哪条渠道上、权重多少。
type AccountChannel struct {
	ID     int64
	Name   string
	Weight int
}

// AccountInfo 是管理面看到的一个账号。**它不含密文，也不含明文凭证。**
type AccountInfo struct {
	ID   int64
	Name string
	// Hint 是凭证末四位（见 credential.Hint），供界面辨识。
	Hint string
	// CredentialKeys 是这份密文被哪些主密钥包过（见 credential.WrappedWith）。
	CredentialKeys []string
	// CredentialErr 说明 CredentialKeys 为什么是空的：密文结构不合法。
	CredentialErr error
	// Channels 是挂了这条账号的渠道，按渠道 id 升序。
	// 已软删的渠道不在里面：关联行还在库里（恢复渠道时要用），但它已经
	// 不参与选路，列出来会让人以为这把 key 还在服役。
	Channels  []AccountChannel
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChannelAccount 是挂在**一条渠道**上的一个账号，供 channel list / get 展示。
type ChannelAccount struct {
	ID     int64
	Name   string
	Weight int
	Hint   string
	// CredentialKeys / CredentialErr 与 AccountInfo 上同名字段的含义相同。
	CredentialKeys []string
	CredentialErr  error
}

// CreateAccountRequest 是新建一个账号所需的字段。
type CreateAccountRequest struct {
	// Name 为空表示生成 `account-` 加六位随机串；撞名时重试。
	Name string

	// Credential 是**明文**凭证。它只用于封装，不落日志、不进错误信息、
	// 不进返回值。
	Credential string

	// ChannelID 非 0 时，创建之后立刻挂到这条渠道上。
	ChannelID int64

	// Weight 是挂到渠道上时的池内权重，nil 表示 1。
	//
	// 用指针而不是 0 值代表「没给」：0 是合法取值（「先别用」，见迁移 00067
	// 的 CHECK），把它与「没给」合成一个数，会让 `account add --weight 0`
	// 与 `account add` 在库里长得一样——而前者是运维明确写下的决定。
	//
	// 它只在 ChannelID 非 0 时有意义：权重挂在渠道与账号的关联上，没挂到
	// 任何渠道上的账号没有权重可写。
	Weight *int
}

// CreateAccount 新建一个账号。
//
// 名字由调用方给定时**不重试**：撞名是调用方该知道的事实，不是可以悄悄
// 换一个名字盖过去的事。生成的名字撞名时重试。
func (a *Admin) CreateAccount(ctx context.Context, req CreateAccountRequest) (AccountInfo, error) {
	envelope, err := a.sealCredential(req.Credential)
	if err != nil {
		return AccountInfo{}, err
	}
	hint := credential.Hint(req.Credential)

	name := strings.TrimSpace(req.Name)
	generated := name == ""
	if !generated {
		if nameErr := validateAccountName(name); nameErr != nil {
			return AccountInfo{}, nameErr
		}
	}
	if req.Weight != nil {
		if *req.Weight < 0 {
			return AccountInfo{}, fmt.Errorf("账号权重不能为负，得到 %d", *req.Weight)
		}
		if req.ChannelID == 0 {
			return AccountInfo{}, errors.New(
				"权重挂在渠道与账号的关联上：给权重时要同时指定挂到哪条渠道")
		}
	}

	tx, ownTx, err := a.begin(ctx)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("创建账号失败: %w", err)
	}
	if ownTx {
		defer func() { _ = tx.Rollback() }()
	}

	var (
		id        int64
		createdAt time.Time
		updatedAt time.Time
	)
	id, name, createdAt, updatedAt, err = insertAccount(ctx, tx, name, hint, envelope, generated)
	if err != nil {
		return AccountInfo{}, err
	}

	info := AccountInfo{
		ID:        id,
		Name:      name,
		Hint:      hint,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	// 新建时信封刚封出来，结构必然合法；这里仍走同一个解析函数，让
	// 「哪些主密钥包过它」只有一条算法。
	keys, keyErr := credential.WrappedWith(envelope)
	if keyErr != nil {
		info.CredentialErr = fmt.Errorf("凭证密文不合法: %w", keyErr)
	} else {
		info.CredentialKeys = keys
	}

	if req.ChannelID != 0 {
		weight := 1
		if req.Weight != nil {
			weight = *req.Weight
		}
		if err := attachAccountInTx(ctx, tx, req.ChannelID, id, weight); err != nil {
			return AccountInfo{}, err
		}
		channels, err := accountChannels(ctx, tx, id)
		if err != nil {
			return AccountInfo{}, err
		}
		info.Channels = channels
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return AccountInfo{}, fmt.Errorf("创建账号失败: %w", err)
		}
	}
	return info, nil
}

// ListAccounts 列出账号，按名字排序。channelID 非 0 时只列挂在那条渠道上的。
//
// 列举只解析信封头部，不解密正文，所以不需要主密钥。
func (a *Admin) ListAccounts(ctx context.Context, channelID int64) ([]AccountInfo, error) {
	if channelID < 0 {
		return nil, errors.New("渠道 id 无效")
	}
	rows, err := a.handle().QueryContext(ctx, `
		SELECT a.id, a.name, a.credential_enc, a.credential_hint,
		       a.created_at, a.updated_at,
		       (SELECT COALESCE(json_agg(json_build_object(
		                    'id', c.id, 'name', c.name, 'weight', ca.weight
		                ) ORDER BY c.id), '[]'::json)::text
		          FROM channel_accounts ca
		          JOIN channels c ON c.id = ca.channel_id
		         WHERE ca.account_id = a.id AND c.deleted_at IS NULL) AS channels
		  FROM accounts a
		 WHERE $1 = 0
		    OR EXISTS (SELECT 1 FROM channel_accounts f
		                WHERE f.account_id = a.id AND f.channel_id = $1)
		 ORDER BY a.name`, channelID)
	if err != nil {
		return nil, fmt.Errorf("查询账号失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始：一个账号都没有是正常结果（新库），不是「没有值」。
	out := []AccountInfo{}
	for rows.Next() {
		var (
			info       AccountInfo
			envelope   []byte
			hint       sql.NullString
			channelsJS string
		)
		if err := rows.Scan(&info.ID, &info.Name, &envelope, &hint,
			&info.CreatedAt, &info.UpdatedAt, &channelsJS); err != nil {
			return nil, fmt.Errorf("读取账号行失败: %w", err)
		}
		info.Hint = hint.String
		info.CredentialKeys, info.CredentialErr = envelopeKeys(envelope)
		if err := json.Unmarshal([]byte(channelsJS), &info.Channels); err != nil {
			return nil, fmt.Errorf("账号 %d 的渠道清单不是合法的 JSON 数组: %w", info.ID, err)
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历账号失败: %w", err)
	}
	return out, nil
}

// UpdateAccount 换掉一个账号的凭证。
//
// 挂在同一条账号上的渠道一个都不用动：它们引用的是账号 id，换 key 改的是
// 这一行的密文。旧密文不保留副本——「历史不断」指的是渠道与账号的关联、
// 以及 updated_at 上的换 key 时刻，不是留一份能解开的旧凭证。
func (a *Admin) UpdateAccount(ctx context.Context, name, plaintext string) (AccountInfo, error) {
	name = strings.TrimSpace(name)
	if err := validateAccountName(name); err != nil {
		return AccountInfo{}, err
	}
	envelope, err := a.sealCredential(plaintext)
	if err != nil {
		return AccountInfo{}, err
	}

	var (
		info      AccountInfo
		createdAt time.Time
		updatedAt time.Time
	)
	err = a.handle().QueryRowContext(ctx, `
		UPDATE accounts
		   SET credential_enc = $2, credential_hint = $3, updated_at = now()
		 WHERE name = $1
		 RETURNING id, created_at, updated_at`,
		name, envelope, credential.Hint(plaintext)).
		Scan(&info.ID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountInfo{}, fmt.Errorf("%w：%s", ErrAccountNotFound, name)
	}
	if err != nil {
		return AccountInfo{}, fmt.Errorf("更新账号失败: %w", err)
	}

	// 换了 key 之后，旧 key 攒下的失败记录不再说明任何问题。删掉状态行就等于
	// 回到「从没用过」，账号立刻重新参与选路，不用等 drained 的冷却期。
	//
	// 两步不是一个事务，方向是安全的：万一这一步失败，账号仍停在旧状态，
	// 只是白等一个冷却期，不会把一把已知被拒的 key 放回池子。
	//
	// 并发那一半也是同一个方向：换 key 之前发出的一次调用，它的失败可能在
	// 这行 DELETE 之后才落库，于是刚换上的新 key 又被标成 drained。代价
	// 同样是新 key 白等一个冷却期——要消除它得把「哪一把 key 的结局」也写进
	// account_state，那已经超出这张表的职责。
	if _, delErr := a.handle().ExecContext(ctx,
		`DELETE FROM account_state WHERE account_id = $1`, info.ID); delErr != nil {
		return AccountInfo{}, fmt.Errorf("重置账号健康状态失败: %w", delErr)
	}
	info.Name = name
	info.Hint = credential.Hint(plaintext)
	info.CreatedAt = createdAt
	info.UpdatedAt = updatedAt
	info.CredentialKeys, info.CredentialErr = envelopeKeys(envelope)

	channels, err := accountChannels(ctx, a.handle(), info.ID)
	if err != nil {
		return AccountInfo{}, err
	}
	info.Channels = channels
	return info, nil
}

// ErrAccountNotOnChannel 表示这个账号没有挂在那条渠道上。
//
// 权重挂在关联上（迁移 00067），没有那一对关联就没有地方放权重。它与
// ErrAccountNotFound 分开：一个要换一条渠道（或换一个账号），一个要核对名字。
var ErrAccountNotOnChannel = errors.New("账号没有挂在这条渠道上")

// AccountNotOnChannelError 带着「这个账号实际挂在哪些渠道上」。
//
// 与 AccountInUseError 同一条理由：只说「没挂在这条上」而不说是哪几条，运维
// 还得自己再查一遍，而那条查询正是本类型刚刚做过的。
type AccountNotOnChannelError struct {
	Name        string
	ChannelID   int64
	ChannelName string
	// Channels 是它实际挂着的渠道（未软删的），按渠道 id 升序。
	Channels []AccountChannel
}

func (e *AccountNotOnChannelError) Error() string {
	if len(e.Channels) == 0 {
		return fmt.Sprintf("%s：账号 %s 没有挂在渠道 %d（%s）上，它现在没有挂任何渠道。"+
			"权重挂在渠道与账号的关联上，没有关联就没有地方放它。",
			ErrAccountNotOnChannel, e.Name, e.ChannelID, e.ChannelName)
	}
	parts := make([]string, 0, len(e.Channels))
	for _, ch := range e.Channels {
		parts = append(parts, fmt.Sprintf("%d（%s）", ch.ID, ch.Name))
	}
	return fmt.Sprintf("%s：账号 %s 没有挂在渠道 %d（%s）上。它挂在：%s。",
		ErrAccountNotOnChannel, e.Name, e.ChannelID, e.ChannelName, strings.Join(parts, "、"))
}

// Unwrap 让 errors.Is(err, ErrAccountNotOnChannel) 成立。
func (e *AccountNotOnChannelError) Unwrap() error { return ErrAccountNotOnChannel }

// SetAccountWeight 改一个账号挂在某条渠道上的池内权重。
//
// 权重挂在关联上，所以「改权重」问的是渠道与账号的那一对：同一个账号挂在两条
// 渠道上时，这里只动被点名的那一条。0 表示「先别用」（见迁移 00067 的 CHECK 与
// accountUsable），负数在这里就被拒——表上也有 CHECK，但那条错误说的是数据库
// 约束，而这件事是调用方的参数错了。
//
// 它不碰 accounts.updated_at，也不碰健康状态：改的是「往这条渠道派多少流量」，
// 不是这份凭证本身。换 key 才重置健康（见 UpdateAccount）。
func (a *Admin) SetAccountWeight(ctx context.Context, name string, channelID int64, weight int) (AccountChannel, error) {
	name = strings.TrimSpace(name)
	if err := validateAccountName(name); err != nil {
		return AccountChannel{}, err
	}
	if weight < 0 {
		return AccountChannel{}, fmt.Errorf("账号权重不能为负，得到 %d", weight)
	}
	if channelID <= 0 {
		return AccountChannel{}, fmt.Errorf("渠道 id 无效：%d", channelID)
	}

	var accountID int64
	err := a.handle().QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE name = $1`, name).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountChannel{}, fmt.Errorf("%w：%s", ErrAccountNotFound, name)
	}
	if err != nil {
		return AccountChannel{}, fmt.Errorf("查询账号失败: %w", err)
	}

	// 一次读带出三件事，省的是一条语句一种含义各查一遍：
	//   - 渠道不在 / 已软删：与 explainNoRow 那两档同一个分法；
	//   - 这一对关联的当前值不在：账号没挂在这条渠道上——参数错，要指出它
	//     实际挂在哪儿。
	var (
		channelName string
		deleted     bool
		current     sql.NullInt64
	)
	err = a.handle().QueryRowContext(ctx, `
		SELECT c.name, c.deleted_at IS NOT NULL, ca.weight
		  FROM channels c
		  LEFT JOIN channel_accounts ca
		         ON ca.channel_id = c.id AND ca.account_id = $2
		 WHERE c.id = $1`, channelID, accountID).
		Scan(&channelName, &deleted, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountChannel{}, fmt.Errorf("%w：id %d", ErrChannelNotFound, channelID)
	}
	if err != nil {
		return AccountChannel{}, fmt.Errorf("查渠道 %d 失败: %w", channelID, err)
	}
	if deleted {
		return AccountChannel{}, fmt.Errorf("%w：id %d", ErrChannelDeleted, channelID)
	}
	if !current.Valid {
		attached, listErr := accountChannels(ctx, a.handle(), accountID)
		if listErr != nil {
			return AccountChannel{}, listErr
		}
		return AccountChannel{}, &AccountNotOnChannelError{
			Name:        name,
			ChannelID:   channelID,
			ChannelName: channelName,
			Channels:    attached,
		}
	}

	if _, err := a.handle().ExecContext(ctx, `
		UPDATE channel_accounts SET weight = $3
		 WHERE channel_id = $1 AND account_id = $2`,
		channelID, accountID, weight); err != nil {
		return AccountChannel{}, dberr.ConstraintFailure(
			fmt.Errorf("改账号 %s 在渠道 %d 上的权重失败: %w", name, channelID, err))
	}
	return AccountChannel{ID: channelID, Name: channelName, Weight: weight}, nil
}

// ErrAccountInUse 表示这个账号还挂在多于一条渠道上，不能不加确认地删。
//
// 它与 ErrAccountNotFound 分开：一个要改命令（加 --force），一个要核对名字。
var ErrAccountInUse = errors.New("账号还挂在多条渠道上")

// AccountInUseError 带着「还挂在哪些渠道上」的清单。
//
// 清单必须能带到调用方：只说「还挂着」而不说是哪几条，运维还得自己再查一遍，
// 而那条查询正是本类型刚刚做过的。
type AccountInUseError struct {
	Name     string
	Channels []AccountChannel
}

func (e *AccountInUseError) Error() string {
	parts := make([]string, 0, len(e.Channels))
	for _, ch := range e.Channels {
		parts = append(parts, fmt.Sprintf("%d（%s）", ch.ID, ch.Name))
	}
	return fmt.Sprintf("%s：账号 %s 还挂在 %d 条渠道上（%s）。"+
		"删掉它会让这些渠道各少一个账号；确认要删请加 --force。",
		ErrAccountInUse, e.Name, len(e.Channels), strings.Join(parts, "、"))
}

// Unwrap 让 errors.Is(err, ErrAccountInUse) 成立。
func (e *AccountInUseError) Unwrap() error { return ErrAccountInUse }

// RemoveAccountResult 是一次 account remove 的结果。
type RemoveAccountResult struct {
	Name string
	// Channels 是删之前挂着它的渠道（未软删的），按渠道 id 升序。
	Channels []AccountChannel
	// EmptiedChannels 是删完之后一个账号都不剩的渠道。
	//
	// 那些渠道从此不参与选路（零账号是合法的中间态）。调用方要把它说出来
	// ——「删了一个账号」与「有一条渠道因此下线」不是同一个后果。
	EmptiedChannels []AccountChannel
}

// RemoveAccount 删除一个账号，连同它在各条渠道上的关联。
//
// # 还挂在**别的**渠道上时默认不删
//
// 账号与渠道是多对多（见迁移 00067）：删一行会让每一条挂着它的渠道少一把
// key，而那条渠道的池子可能就只剩这一个账号。这种后果不该由一次「删一个
// 账号」隐式带出来，所以挂在多于一条渠道上时要求 force——拒绝时把渠道清单
// 带回去，人不必再查一遍。
//
// 只挂在一条渠道上时照删。那条渠道因此可能变成零账号，结果里单独列出来。
//
// # 在飞请求
//
// 不等、也不打断：正在跑的请求在选出候选时就把明文凭证拿到手了（见
// Source.Candidates），删这一行不影响它们；新的请求从此选不到它。
// 所以这是一次单行 DELETE，不需要任何锁，也不需要「先摘关联再等一会儿」。
func (a *Admin) RemoveAccount(ctx context.Context, name string, force bool) (RemoveAccountResult, error) {
	name = strings.TrimSpace(name)
	if err := validateAccountName(name); err != nil {
		return RemoveAccountResult{}, err
	}

	tx, ownTx, err := a.begin(ctx)
	if err != nil {
		return RemoveAccountResult{}, fmt.Errorf("删除账号失败: %w", err)
	}
	if ownTx {
		defer func() { _ = tx.Rollback() }()
	}

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return RemoveAccountResult{}, fmt.Errorf("%w：%s", ErrAccountNotFound, name)
	}
	if err != nil {
		return RemoveAccountResult{}, fmt.Errorf("查询账号失败: %w", err)
	}

	channels, err := accountChannels(ctx, tx, id)
	if err != nil {
		return RemoveAccountResult{}, err
	}
	if len(channels) > 1 && !force {
		return RemoveAccountResult{}, &AccountInUseError{Name: name, Channels: channels}
	}

	// 「删完这条渠道还剩几个账号」要**在删之前**数：删除之后关联行已经没了，
	// 数出来的是删完之后的结果；而这里问的是「这次删除会不会让某条渠道
	// 变成零账号」，那个判断需要知道删之前有几个。
	emptied := []AccountChannel{}
	for _, ch := range channels {
		var rest int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM channel_accounts
			 WHERE channel_id = $1 AND account_id <> $2`, ch.ID, id).Scan(&rest); err != nil {
			return RemoveAccountResult{}, fmt.Errorf("统计渠道 %d 的账号池失败: %w", ch.ID, err)
		}
		if rest == 0 {
			emptied = append(emptied, ch)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, id); err != nil {
		return RemoveAccountResult{}, dberr.ConstraintFailure(
			fmt.Errorf("删除账号失败: %w", err))
	}
	if ownTx {
		if err := tx.Commit(); err != nil {
			return RemoveAccountResult{}, fmt.Errorf("删除账号失败: %w", err)
		}
	}
	return RemoveAccountResult{Name: name, Channels: channels, EmptiedChannels: emptied}, nil
}

// ChannelIDByRef 把命令行给的渠道引用解析成 id。
//
// 只认数字与**未软删渠道的名字**：软删的渠道不能再当写入目标，把它解析
// 出来只会让后面的外键或选路给出更难懂的错误。同名渠道多于一条时报错并
// 要求改用 id——挑一条最老的会把请求悄悄挂到一个调用方没说的地方。
//
// 名字这一档的候选除了 id 还带上类型与地址：同一次按名字查出来的候选，名字
// 必然全都一样，只列名字等于没列；类型与地址才是能让人认出手里那条渠道的两个
// 属性。软删的渠道不进候选，也不进「有多条」那一档——只命中已删渠道时说的是
// 「没有未删除的这条」，而不是「有多条」。
//
// 返回的 id 可能指向一行不存在的渠道（数字那一档不查库）：调用方紧接着的读取
// 或写入会撞外键/找不到，那才是它自己的判据。
func (a *Admin) ChannelIDByRef(ctx context.Context, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, errors.New("渠道不能为空")
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if id <= 0 {
			return 0, fmt.Errorf("渠道 id 无效：%s", ref)
		}
		return id, nil
	}

	rows, err := a.handle().QueryContext(ctx,
		`SELECT id, type, base_url FROM channels WHERE name = $1 AND `+channelAlive+` ORDER BY id`, ref)
	if err != nil {
		return 0, fmt.Errorf("按名字查渠道失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, 1)
	lines := make([]string, 0, 1)
	for rows.Next() {
		var id int64
		var channelType, baseURL string
		if err := rows.Scan(&id, &channelType, &baseURL); err != nil {
			return 0, fmt.Errorf("读取渠道 id 失败: %w", err)
		}
		ids = append(ids, id)
		lines = append(lines, fmt.Sprintf("  id=%d 类型=%s 地址=%s", id, channelType, baseURL))
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("遍历同名渠道失败: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, fmt.Errorf("%w：没有叫 %q 的未删除渠道（用 channel list 看现有的名字）",
			ErrChannelNotFound, ref)
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("有 %d 条渠道叫 %q，改用渠道 ID 指定：\n%s",
			len(ids), ref, strings.Join(lines, "\n"))
	}
}

// replaceChannelCredential 换掉一条渠道主账号的凭证。
//
// 「主账号」是池里账号 id 最小的那一个：`channel update` 的 `--credential-*`
// 认的就是它。**选路不再按同一条判据取账号**——它按权重与健康从池子里挑
// （见 accountRotation），所以「主账号」在这里回答的是「这条兼容命令改哪一行」，
// 不是「下一次请求会用哪一把 key」。
//
// 这一行可能同时挂在别的渠道上：那些渠道不需要改任何引用，下一次读到这一行
// 就是新 key——这就是账号实体存在的理由。
//
// 零账号的渠道没有主账号可换，这里按 `channel create --credential-env` 的
// 兼容捷径补一个匿名账号并挂上（权重 1，与表上的默认值一致）。
//
// 换掉的那一行连同它的健康状态一起重置：这条判据与 `account update` 共用，
// 见下面 DELETE 处的注释。
func replaceChannelCredential(ctx context.Context, tx *sql.Tx, channelID int64,
	envelope []byte, hint string) error {
	var accountID int64
	err := tx.QueryRowContext(ctx, `
		UPDATE accounts
		   SET credential_enc = $2, credential_hint = $3, updated_at = now()
		 WHERE id = (SELECT ca.account_id
		               FROM channel_accounts ca
		              WHERE ca.channel_id = $1
		              ORDER BY ca.account_id
		              LIMIT 1)
		 RETURNING id`, channelID, envelope, hint).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		newID, _, _, _, insertErr := insertAccount(ctx, tx, "", hint, envelope, true)
		if insertErr != nil {
			return insertErr
		}
		return attachAccountInTx(ctx, tx, channelID, newID, 1)
	}
	if err != nil {
		return fmt.Errorf("更新渠道 %d 的主账号失败: %w", channelID, err)
	}

	// 与 UpdateAccount 同一条判据：换 key 之后旧 key 攒下的失败记录不再说明
	// 任何问题，删掉状态行就等于回到「从没用过」。留着它的表现是换好的 key
	// 仍被 drained 挡在候选之外，要白等一个冷却期。
	//
	// 与那次 UPDATE 在同一个事务里：两半要么都落，要么都不落。分开写的话，
	// 凭证换成了新的、状态却没清，正是上面那种要被避免的中间态。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_state WHERE account_id = $1`, accountID); err != nil {
		return fmt.Errorf("重置账号健康状态失败: %w", err)
	}
	return nil
}

// insertAccount 插入一行账号，必要时为生成的名字重试。
//
// generated 为 false 时名字是调用方给的，撞名直接报 ErrAccountExists；
// 为 true 时每次撞名换一个随机名重试。返回最终落库的名字——生成的与调用方
// 最初拿到的可能不同（见撞名重试）。
func insertAccount(ctx context.Context, h dbHandle, name, hint string,
	envelope []byte, generated bool) (int64, string, time.Time, time.Time, error) {
	var (
		id        int64
		createdAt time.Time
		updatedAt time.Time
	)
	for attempt := 0; ; attempt++ {
		if generated {
			generatedName, err := generateAccountName()
			if err != nil {
				return 0, "", time.Time{}, time.Time{}, err
			}
			name = generatedName
		}
		err := h.QueryRowContext(ctx, `
			INSERT INTO accounts (name, credential_enc, credential_hint)
			VALUES ($1, $2, $3)
			RETURNING id, created_at, updated_at`,
			name, envelope, hint).Scan(&id, &createdAt, &updatedAt)
		if err == nil {
			return id, name, createdAt, updatedAt, nil
		}
		if !dberr.IsConstraintViolation(err) {
			return 0, "", time.Time{}, time.Time{},
				fmt.Errorf("创建账号失败: %w", err)
		}
		if !generated {
			return 0, "", time.Time{}, time.Time{},
				fmt.Errorf("%w：%s", ErrAccountExists, name)
		}
		if attempt+1 >= accountNameAttempts {
			return 0, "", time.Time{}, time.Time{},
				fmt.Errorf("生成账号名连续撞名 %d 次", accountNameAttempts)
		}
	}
}

// attachAccountInTx 把账号挂到渠道上，并在渠道不可用时给出具名错误。
//
// 用 INSERT ... SELECT 而不是先查后插：渠道在两步之间被软删时，语句会
// 因为 WHERE 不成立而写 0 行，调用方拿到的是同一条「渠道不存在/已删」。
//
// weight 是池内权重（0 表示「先别用」，见迁移 00067）。它由调用方显式给：
// 表上有 DEFAULT 1，但让这条语句省略它意味着「没给权重」与 1 在代码里
// 分不开，而 0 与「没给」恰好是两件不同的事。
func attachAccountInTx(ctx context.Context, tx *sql.Tx, channelID, accountID int64, weight int) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO channel_accounts (channel_id, account_id, weight)
		SELECT c.id, $2, $3 FROM channels c
		 WHERE c.id = $1 AND c.`+channelAlive,
		channelID, accountID, weight)
	if err != nil {
		return fmt.Errorf("把账号挂到渠道 %d 上失败: %w", channelID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认账号关联结果失败: %w", err)
	}
	if affected == 0 {
		var deleted bool
		scanErr := tx.QueryRowContext(ctx,
			`SELECT deleted_at IS NOT NULL FROM channels WHERE id = $1`,
			channelID).Scan(&deleted)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return fmt.Errorf("%w：id %d", ErrChannelNotFound, channelID)
		}
		if scanErr != nil {
			return fmt.Errorf("查渠道 %d 的删除状态失败: %w", channelID, scanErr)
		}
		if deleted {
			return fmt.Errorf("%w：渠道 %d", ErrChannelDeleted, channelID)
		}
		return fmt.Errorf("%w：id %d", ErrChannelNotFound, channelID)
	}
	return nil
}

// accountChannels 读一个账号挂着的渠道（未软删的），按渠道 id 升序。
func accountChannels(ctx context.Context, h dbHandle, accountID int64) ([]AccountChannel, error) {
	rows, err := h.QueryContext(ctx, `
		SELECT c.id, c.name, ca.weight
		  FROM channel_accounts ca
		  JOIN channels c ON c.id = ca.channel_id
		 WHERE ca.account_id = $1 AND c.deleted_at IS NULL
		 ORDER BY c.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("读取账号 %d 的渠道失败: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []AccountChannel{}
	for rows.Next() {
		var ch AccountChannel
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Weight); err != nil {
			return nil, fmt.Errorf("读取账号 %d 的渠道行失败: %w", accountID, err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历账号 %d 的渠道失败: %w", accountID, err)
	}
	return out, nil
}

// validateAccountName 校验账号名。
//
// 判据与渠道名同一套：非空、不含控制字符。账号名会进 `account list` 的
// 表格与命令行参数，换行或 ESC 在两个地方都会出问题。
func validateAccountName(name string) error {
	if name == "" {
		return errors.New("账号名不能为空")
	}
	if err := textshape.Validate("账号名", name); err != nil {
		return err
	}
	return nil
}

// generateAccountName 生成 `account-` 加六位 [a-z0-9]。
//
// 取模带来的偏差（256 不能被 36 整除）在这里无所谓：名字只需要够难撞，
// 不需要不可预测。
func generateAccountName() (string, error) {
	raw := make([]byte, generatedAccountNameLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成账号名失败: %w", err)
	}
	var b strings.Builder
	b.Grow(len(generatedAccountNamePrefix) + generatedAccountNameLength)
	b.WriteString(generatedAccountNamePrefix)
	for _, v := range raw {
		b.WriteByte(accountNameAlphabet[int(v)%len(accountNameAlphabet)])
	}
	return b.String(), nil
}

// envelopeKeys 解析一份密文挂着哪些主密钥，并把它包成列举用的两种结果。
func envelopeKeys(envelope []byte) ([]string, error) {
	keys, err := credential.WrappedWith(envelope)
	if err != nil {
		return nil, fmt.Errorf("凭证密文不合法: %w", err)
	}
	return keys, nil
}
