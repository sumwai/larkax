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
		if err := attachAccountInTx(ctx, tx, req.ChannelID, id); err != nil {
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

// ListAccounts 列出全部账号，按名字排序。
//
// 列举只解析信封头部，不解密正文，所以不需要主密钥。
func (a *Admin) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
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
		 ORDER BY a.name`)
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

// ChannelIDByRef 把命令行给的渠道引用解析成 id。
//
// 只认数字与**未软删渠道的名字**：软删的渠道不能再当写入目标，把它解析
// 出来只会让后面的外键或选路给出更难懂的错误。同名渠道多于一条时报错并
// 要求改用 id——挑一条最老的会把请求悄悄挂到一个调用方没说的地方。
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
		`SELECT id FROM channels WHERE name = $1 AND `+channelAlive+` ORDER BY id`, ref)
	if err != nil {
		return 0, fmt.Errorf("按名字查渠道失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("读取渠道 id 失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("遍历同名渠道失败: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, fmt.Errorf("%w：%s", ErrChannelNotFound, ref)
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("有 %d 条渠道叫 %q，请改用渠道 ID（%v）", len(ids), ref, ids)
	}
}

// replaceChannelCredential 换掉一条渠道主账号的凭证。
//
// 主账号是挂池里账号 id 最小的那个，与选路取的那一个同一条判据（见
// candidateQuery）。它可能同时挂在别的渠道上：那些渠道不需要改任何引用，
// 下一次读到这一行就是新 key——这就是账号实体存在的理由。
//
// 零账号的渠道没有主账号可换，这里按 `channel create --credential-env` 的
// 兼容捷径补一个匿名账号并挂上。
func replaceChannelCredential(ctx context.Context, tx *sql.Tx, channelID int64,
	envelope []byte, hint string) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE accounts
		   SET credential_enc = $2, credential_hint = $3, updated_at = now()
		 WHERE id = (SELECT ca.account_id
		               FROM channel_accounts ca
		              WHERE ca.channel_id = $1
		              ORDER BY ca.account_id
		              LIMIT 1)`, channelID, envelope, hint)
	if err != nil {
		return fmt.Errorf("更新渠道 %d 的主账号失败: %w", channelID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认主账号更新结果失败: %w", err)
	}
	if affected > 0 {
		return nil
	}
	accountID, _, _, _, err := insertAccount(ctx, tx, "", hint, envelope, true)
	if err != nil {
		return err
	}
	return attachAccountInTx(ctx, tx, channelID, accountID)
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
func attachAccountInTx(ctx context.Context, tx *sql.Tx, channelID, accountID int64) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO channel_accounts (channel_id, account_id)
		SELECT c.id, $2 FROM channels c
		 WHERE c.id = $1 AND c.`+channelAlive,
		channelID, accountID)
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
