package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 本文件是控制台口令的哈希与校验。
//
// # 为什么用 PBKDF2 而不是快哈希
//
// API Key 是 256 位随机串，没有可猜的字典，快哈希足够（见 key.go 的 HashKey）。
// 口令不一样：它是人选出来的，低熵、有字典、会被撞库。慢哈希正是为这一侧
// 准备的，所以同一个仓库里两种哈希函数并存是有道理的，而不是不一致。
//
// # 为什么参数随串走
//
// 轮数会随硬件变便宜而上调。参数写死在代码里的话，上调的那一刻所有旧口令
// 立刻无法验证；写在串里，旧串按它自己的参数验证，登录成功后再按当前参数
// 重算一遍并静默替换（渐进重哈希）。
//
// # 为什么参数是依赖注入而不是常量
//
// 集成测试要连真库跑登录。60 万轮一次约 100ms，一条用例里登录几次就把测试
// 拖成分钟级；而测试注入 i=1000 时，**重哈希也必须用 1000**——否则一条快哈希
// 在第一次登录成功之后被升级成 60 万轮，后面的用例照样慢，且慢得莫名其妙。
// 所以「当前参数」是一个值，不是全局常量：谁装配谁给（见 consoleapi.Config）。
const (
	// passwordHashScheme 是哈希串的方案前缀。
	passwordHashScheme = "$pbkdf2-sha256$"

	// passwordMaxIterations 是解析旧串时允许的轮数上限。
	//
	// 一行坏数据不该把一次登录变成 CPU 炸弹。10^7 轮约 2 秒，已经远高于任何
	// 现实参数，而它仍然是一个有限值。
	passwordMaxIterations = 10_000_000

	// passwordMaxKeyBytes / passwordMaxSaltBytes 是解析时允许的分量与盐长度上限。
	// 与轮数同一个理由：串来自库，而库里的值不经过本包的写入路径。
	passwordMaxKeyBytes  = 1024
	passwordMaxSaltBytes = 1024
)

// 口令策略的边界（见 ValidatePassword）。
const (
	PasswordMinRunes = 12
	PasswordMaxRunes = 128
)

// KDFParams 是一次 PBKDF2 计算的参数。
//
// 它是「当前参数」：验证旧串时按串里的参数走，判断要不要重哈希时与这里的
// 值比；重哈希时用这里的值。生产默认见 DefaultKDFParams。
type KDFParams struct {
	Iterations int
	SaltBytes  int
	KeyBytes   int
}

// DefaultKDFParams 是生产参数：60 万轮、16 字节盐、32 字节输出。
//
// 60 万是 NIST SP 800-63B 对 PBKDF2-HMAC-SHA256 的建议量级。测试不要用它
// （慢），注入一个小的（见 consoleapi.Config 的 KDF）。
var DefaultKDFParams = KDFParams{Iterations: 600_000, SaltBytes: 16, KeyBytes: 32}

// Validate 检查参数是否落在可计算、可解析的范围内。
func (p KDFParams) Validate() error {
	switch {
	case p.Iterations < 1 || p.Iterations > passwordMaxIterations:
		return fmt.Errorf("PBKDF2 轮数 %d 不在 1..%d 之内", p.Iterations, passwordMaxIterations)
	case p.SaltBytes < 8 || p.SaltBytes > passwordMaxSaltBytes:
		return fmt.Errorf("PBKDF2 盐长度 %d 不在 8..%d 之内", p.SaltBytes, passwordMaxSaltBytes)
	case p.KeyBytes < 16 || p.KeyBytes > passwordMaxKeyBytes:
		return fmt.Errorf("PBKDF2 输出长度 %d 不在 16..%d 之内", p.KeyBytes, passwordMaxKeyBytes)
	}
	return nil
}

// storedKDFParams 是一条哈希串里写着的参数。
type storedKDFParams struct {
	iterations int
	keyBytes   int
	salt       []byte
	hash       []byte
}

// HashPassword 按给定参数把口令算成可落库的串。
//
// 形如 `$pbkdf2-sha256$i=600000,l=32$<salt base64url>$<hash base64url>`。
// 返回的串里带着参数，验证时不必再问一次「这是按多少轮算的」。
func HashPassword(p KDFParams, password string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	salt := make([]byte, p.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成口令盐失败: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, p.Iterations, p.KeyBytes)
	if err != nil {
		return "", fmt.Errorf("计算口令哈希失败: %w", err)
	}

	var b strings.Builder
	b.WriteString(passwordHashScheme)
	b.WriteString("i=")
	b.WriteString(strconv.Itoa(p.Iterations))
	b.WriteString(",l=")
	b.WriteString(strconv.Itoa(p.KeyBytes))
	b.WriteByte('$')
	b.WriteString(base64.RawURLEncoding.EncodeToString(salt))
	b.WriteByte('$')
	b.WriteString(base64.RawURLEncoding.EncodeToString(key))
	return b.String(), nil
}

// PasswordVerifyResult 是一次口令校验的结果。
type PasswordVerifyResult struct {
	// OK 表示口令对不对。
	OK bool
	// NeedsRehash 表示这条串是按旧参数算的，登录成功后应当按当前参数重算。
	// 只有 OK 为真时它才有意义。
	NeedsRehash bool
}

// VerifyPassword 按串里写着的参数校验口令，并与当前参数比对是否需要重哈希。
//
// 串解析失败时报错（那是库里的坏数据，不是「口令不对」）；口令不对不是错误，
// 是 OK=false。两者分开，调用方才能把前者当成服务端故障、后者当成 401。
func VerifyPassword(current KDFParams, stored, password string) (PasswordVerifyResult, error) {
	parsed, err := parsePasswordHash(stored)
	if err != nil {
		return PasswordVerifyResult{}, err
	}
	key, err := pbkdf2.Key(sha256.New, password, parsed.salt,
		parsed.iterations, len(parsed.hash))
	if err != nil {
		return PasswordVerifyResult{}, fmt.Errorf("计算口令哈希失败: %w", err)
	}
	if subtle.ConstantTimeCompare(key, parsed.hash) != 1 {
		return PasswordVerifyResult{}, nil
	}
	return PasswordVerifyResult{
		OK: true,
		// 只有两项都相同才不需要重算。盐是每次随机生成的，这里不比盐。
		NeedsRehash: parsed.iterations != current.Iterations || len(parsed.hash) != current.KeyBytes,
	}, nil
}

// parsePasswordHash 解析一条哈希串。
//
// 上限在**解析**这一侧判，不只信写入路径：串来自库，而库里的值可以由别的
// 进程、别的人写进去。轮数没有上限时，一行坏数据就是一次 CPU 炸弹。
func parsePasswordHash(stored string) (storedKDFParams, error) {
	if !strings.HasPrefix(stored, passwordHashScheme) {
		return storedKDFParams{}, errors.New("口令哈希的方案不是 pbkdf2-sha256")
	}
	rest := strings.TrimPrefix(stored, passwordHashScheme)
	parts := strings.Split(rest, "$")
	if len(parts) != 3 {
		return storedKDFParams{}, errors.New("口令哈希的段数不是三段")
	}

	var (
		out      storedKDFParams
		haveIter bool
		haveLen  bool
	)
	for _, field := range strings.Split(parts[0], ",") {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return storedKDFParams{}, fmt.Errorf("口令哈希的参数 %q 不是 name=value", field)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return storedKDFParams{}, fmt.Errorf("口令哈希的参数 %q 不是整数", field)
		}
		switch name {
		case "i":
			out.iterations, haveIter = n, true
		case "l":
			out.keyBytes, haveLen = n, true
		default:
			return storedKDFParams{}, fmt.Errorf("口令哈希里有未知参数 %q", name)
		}
	}
	if !haveIter || !haveLen {
		return storedKDFParams{}, errors.New("口令哈希缺少轮数或输出长度")
	}
	switch {
	case out.iterations < 1 || out.iterations > passwordMaxIterations:
		return storedKDFParams{}, fmt.Errorf("口令哈希的轮数 %d 不在 1..%d 之内",
			out.iterations, passwordMaxIterations)
	case out.keyBytes < 16 || out.keyBytes > passwordMaxKeyBytes:
		return storedKDFParams{}, fmt.Errorf("口令哈希的输出长度 %d 不在 16..%d 之内",
			out.keyBytes, passwordMaxKeyBytes)
	}

	salt, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return storedKDFParams{}, fmt.Errorf("口令哈希的盐不是 base64url: %w", err)
	}
	if len(salt) == 0 || len(salt) > passwordMaxSaltBytes {
		return storedKDFParams{}, fmt.Errorf("口令哈希的盐长度 %d 不在 1..%d 之内",
			len(salt), passwordMaxSaltBytes)
	}
	hash, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return storedKDFParams{}, fmt.Errorf("口令哈希的值不是 base64url: %w", err)
	}
	if len(hash) != out.keyBytes {
		return storedKDFParams{}, fmt.Errorf("口令哈希的值长度 %d 与参数里的 %d 不一致",
			len(hash), out.keyBytes)
	}

	out.salt = salt
	out.hash = hash
	return out, nil
}

// ValidatePassword 检查一个新口令是否合策略。
//
// 三条判据，都不看字符组合（NIST SP 800-63B 的口径：组合规则让人选
// `Passw0rd!` 这类可预测的串，长度才是真的门槛）：
//
//  1. 12–128 个字符（按 rune 数，不按字节：中文口令的字符数才是人看到的那个）。
//  2. 不得等于用户名。允许它等于用户名等于让「用户名泄漏」直接变成登录。
//  3. 不得含控制字符。它会被原样送进 KDF，而控制字符在终端与日志里看不见，
//     用户不知道自己错在哪；这一条同时也是对「粘贴进来一段二进制」的早期拒绝。
//
// 返回的错误不带口令本身：错误可能进日志。
func ValidatePassword(username, password string) error {
	n := utf8.RuneCountInString(password)
	if n < PasswordMinRunes {
		return fmt.Errorf("口令至少 %d 个字符（当前 %d 个）", PasswordMinRunes, n)
	}
	if n > PasswordMaxRunes {
		return fmt.Errorf("口令至多 %d 个字符（当前 %d 个）", PasswordMaxRunes, n)
	}
	if strings.EqualFold(strings.TrimSpace(password), strings.TrimSpace(username)) {
		return errors.New("口令不能与用户名相同")
	}
	for _, r := range password {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("口令不能含控制字符 %s", describeControlChar(r))
		}
	}
	return nil
}
