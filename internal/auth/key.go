package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// API Key 明文的形态：`larkax_` 加上 32 字节随机数的 base64url 编码（43 字符）。
//
// 带方案前缀不是为了安全，是为了让人一眼认出手里这串是什么——日志、截图、
// 粘贴板里的一长串字符需要能自证身份。
//
// 随机部分是 256 位。这个数决定了下面为什么可以用快哈希：它不是人选的口令，
// 没有可猜的字典，也不存在「弱 key」。
const (
	keyScheme      = "larkax_"
	keyRandomBytes = 32

	// keyPrefixLen 是存进 api_keys.key_prefix 的字符数。
	//
	// 它只用于「在列表里认出是哪把钥匙」，不参与查找（查找走 key_hash）。
	// 12 个字符里随机部分占 5 个（约 30 位），足够区分同一个用户的几把 key，
	// 而剩下的 27 个随机字节仍然完好——前缀泄露不了可用的熵。
	keyPrefixLen = 12
)

// NewKey 生成一把新的 API Key 明文。
//
// 熵源失败时返回错误而不是退回一个可预测的值：可预测的 key 等于没有鉴权，
// 而它不会报错，只会安静地生效。
func NewKey() (string, error) {
	var b [keyRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成随机字节失败: %w", err)
	}
	return keyScheme + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashKey 是 API Key 的存储哈希口径：SHA-256 的十六进制（小写，64 字符）。
//
// **创建与校验必须都走这一个函数。** 两边各写一遍的表现是
// 「刚建的 key 立刻就说无效」——那是最费时的一类排查，
// 因为两端各自看都对，而且它在测试里不出现（测试若也各写一遍，就一起错）。
//
// # 为什么用快哈希而不是 bcrypt / argon2
//
//   - API Key 是 256 位随机串，不是人选的口令。慢哈希要防的是
//     「拿到哈希后反推低熵口令」，而这里没有可反推的东西：
//     字典与暴力都不成立，快慢与否不影响这一点。
//   - 慢哈希没法按值查。每次请求都要把候选值完整算一遍才能比对，
//     于是「按 key 定位一行」变成「把表里每一行都验一遍」——
//     而鉴权在每一个请求的热路径上，这条路径上放一次 KDF 是拿可用性换一个不存在的威胁。
//   - 逐条加盐会让每行的哈希都不同，那就既建不了唯一索引，也没法一次查询定位；
//     而 key_hash 上的 UNIQUE 正是「同一把 key 不会被建两次」的落点。
//
// 所以这里要的恰恰是「快且确定」：一次索引查找定位一行。
//
// 代价是库被拖走就等于所有 key 泄露。这一条由数据库的访问控制与备份策略承担，
// 不是哈希函数能解决的——bcrypt 也拦不住拿到整个库的人（他可以直接改数据）。
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// KeyPrefix 取明文的前 keyPrefixLen 个字符，用于展示。
//
// 按字节切而不是按字符：key 的字符集是 ASCII（方案前缀 + base64url），
// 不存在把多字节字符切成半个的情况。
func KeyPrefix(plaintext string) string {
	if len(plaintext) <= keyPrefixLen {
		return plaintext
	}
	return plaintext[:keyPrefixLen]
}
