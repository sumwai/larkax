package adminapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/settings"
)

const (
	// tokenBytes 是一枚令牌的熵。32 字节 = 256 位，base64url 之后 43 个字符。
	tokenBytes = 32

	// hashHexLen 是 SHA-256 十六进制的长度，也是库里那一列合法值的长度。
	hashHexLen = sha256.Size * 2
)

// ErrNoTokenRow 表示 settings 里没有管理令牌那一行。
//
// 它只会在迁移没跑到时出现：Registry 声明了这个键，缺行时服务自己就起不来。
// 单独一个错误而不是让调用方看驱动原文，是因为下一步动作很明确——先跑迁移。
var ErrNoTokenRow = errors.New("settings 里没有管理令牌那一行，先跑 larkaxctl migrate up")

// ErrNoTokenConfigured 表示这个部署从没创建过管理令牌。
//
// 它与「撤销之后」是两件事：撤销是「换一枚」，从没配置是「先创建一枚」。
// 命令照这两个下一步给出不同的提示。
var ErrNoTokenConfigured = errors.New("没有配置过管理令牌，先跑 larkaxctl admin token create")

// TokenStore 读写管理令牌。令牌本身只在 Create 的返回值里出现一次，库里
// 存的是它的 SHA-256。
type TokenStore struct {
	db *sql.DB
}

// NewTokenStore 包装一个已打开的数据库句柄。
func NewTokenStore(db *sql.DB) *TokenStore { return &TokenStore{db: db} }

// Create 生成一枚新令牌，把它的哈希写进 settings，并返回**明文**。
//
// 明文只有这一次机会：库里没有它，也没有任何路径能从哈希反推出来。
// 调用方必须在返回之后立刻把它交给使用者，不得写日志、写文件或存进任何表。
//
// 新令牌替换旧令牌是**同一条 UPDATE**：生成与写入之间没有「先撤销再新建」
// 的窗口，所以不会出现一个两个都有效的瞬间，也不会两个都失效。
func (s *TokenStore) Create(ctx context.Context) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成管理令牌失败: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	encoded, err := json.Marshal(hex.EncodeToString(sum[:]))
	if err != nil {
		// hex 编码后的字符串只会是合法的 JSON 值，这条分支不可达；留着它
		// 是为了让「Marshal 之后才有值」这个顺序在代码上成立。
		return "", fmt.Errorf("编码管理令牌失败: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE settings SET value = $2::jsonb, updated_at = now()
		 WHERE key = $1`, settings.KeyAdminToken, string(encoded))
	if err != nil {
		return "", fmt.Errorf("写入管理令牌失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("确认管理令牌写入失败: %w", err)
	}
	if affected == 0 {
		return "", ErrNoTokenRow
	}
	return token, nil
}

// revokedHash 是撤销之后写进库里的值：64 个 0。
//
// # 为什么撤销不是「写回空串」
//
// 空串是「这个部署从来没有配置过令牌」，它对应 503（运维得先 create 一枚）。
// 撤销是另一件事：令牌**配置过**，只是现在没有一只是有效的，所以请求该得到
// 401——拿旧令牌来的人下一步是「换一枚」，而不是「去把服务配起来」。
//
// 全零的 SHA-256 是不可达的：没有任何 32 字节令牌能算出这个值，所以它不会
// 误放行任何令牌；它只是一个「已撤销」的标记。删掉那一行做不到这件事：
// settings 要求 Registry 里每个键都有一行，缺行时服务自己就起不来。
const revokedHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Revoke 撤销当前令牌：写入全零的 verifier，任何请求于是都得到 401。
//
// # 「从没配置过」不是一次撤销
//
// 那一行是空串时**不动它**，回 ErrNoTokenConfigured：空串对应 503（先去创建
// 一枚），而撤销对应 401（换一枚）。把前者写成哨兵，等于把「这个部署还没开
// 管理面」悄悄说成「管理面开着、只是没有有效令牌」——运维会照着 401 的提示
// 去换令牌，而实际上他从来没创建过。
//
// 对已经撤销过的令牌再撤销一次不报错：那时值已经是哨兵，写回同一个值。
func (s *TokenStore) Revoke(ctx context.Context) error {
	encoded, err := json.Marshal(revokedHash)
	if err != nil {
		return fmt.Errorf("编码已撤销令牌失败: %w", err)
	}

	// 条件保证的是「库里本来就没有令牌时不写哨兵」：空串维持成空串，admin API
	// 继续回 503，而不是变成 401。它**不**解决 create 与 revoke 并发的先后——
	// 后到的那条按自己看到的最新行处理（READ COMMITTED 下会重新求值），
	// 谁赢取决于提交顺序。
	res, err := s.db.ExecContext(ctx, `
		UPDATE settings SET value = $2::jsonb, updated_at = now()
		 WHERE key = $1 AND value <> '""'::jsonb`,
		settings.KeyAdminToken, string(encoded))
	if err != nil {
		return fmt.Errorf("撤销管理令牌失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认管理令牌撤销失败: %w", err)
	}
	if affected == 0 {
		// 没有更新到行有两种原因，对应两种下一步：缺行（先跑迁移）与空串
		// （先创建一枚）。分清楚才不至于让人去修一个不是问题的东西。
		var exists bool
		err := s.db.QueryRowContext(ctx,
			`SELECT true FROM settings WHERE key = $1`, settings.KeyAdminToken).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoTokenRow
		}
		if err != nil {
			return fmt.Errorf("确认管理令牌那一行是否存在失败: %w", err)
		}
		return ErrNoTokenConfigured
	}
	return nil
}

// Hash 读出当前令牌的 verifier。第二个返回值为 false 表示还没配置。
//
// # 为什么每次请求都读库
//
// settings 里其余的值在装配期读一次，改完要重启。令牌不一样：轮换凭证的
// 时候最不希望出现「撤销了但旧令牌还能用到重启为止」——那正是撤销最该生效
// 的一段窗口。管理面流量很小，一次主键查询不值得为它引入缓存与失效逻辑。
func (s *TokenStore) Hash(ctx context.Context) ([sha256.Size]byte, bool, error) {
	var zero [sha256.Size]byte

	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = $1`, settings.KeyAdminToken).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, ErrNoTokenRow
	}
	if err != nil {
		return zero, false, fmt.Errorf("读取管理令牌失败: %w", err)
	}

	var stored string
	if err = json.Unmarshal(raw, &stored); err != nil {
		return zero, false, fmt.Errorf("settings.%s 不是字符串: %w", settings.KeyAdminToken, err)
	}
	if stored == "" {
		return zero, false, nil
	}
	if len(stored) != hashHexLen {
		return zero, false, fmt.Errorf("settings.%s 不是 %d 位十六进制", settings.KeyAdminToken, hashHexLen)
	}
	decoded, err := hex.DecodeString(stored)
	if err != nil {
		return zero, false, fmt.Errorf("settings.%s 不是十六进制: %w", settings.KeyAdminToken, err)
	}

	var sum [sha256.Size]byte
	copy(sum[:], decoded)
	return sum, true, nil
}
