// Package credential 负责渠道凭证的信封加密。
//
// 为什么是信封而不是直接用主密钥加密：主密钥（KEK）来自环境变量，改动它意味着
// 要重加密全部存量数据。中间加一层数据密钥（DEK）之后，轮换主密钥只需要把 DEK
// 重新包一遍——存量密文一个字节都不用动。
//
// 每个凭证一个独立的 DEK，而不是全局共用一个：
//   - 泄漏一个 DEK 只影响一个渠道，不是一个库；
//   - 重加密某个凭证不需要主密钥参与；
//   - 代价只是每行多几十字节。
//
// 密文格式自描述，记录着「用哪把 KEK 包的」。解密时按标识挑钥匙，
// 于是新旧主密钥可以长期并存，不会因为一次轮换就让旧数据变成废纸。
//
// 本包是渠道凭证唯一的解密点。插件层永远拿不到明文，也拿不到 DEK——
// 它在设计上就不该有能力接触上游身份。
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"

	"github.com/sumwai/larkax/internal/config"
)

const (
	// version 是密文格式版本。写进密文头部，以便将来换算法时能分辨旧数据。
	version byte = 1

	dekSize   = 32 // AES-256
	nonceSize = 12 // GCM 标准
	tagSize   = 16

	// maxWraps 限制一份密文里能带多少份 DEK 副本。
	// 主密钥不会有很多把（轮换期间两三把），设个上限是为了让畸形输入
	// 在解析阶段就被拒绝，而不是让长度字段去驱动一次巨大的分配。
	maxWraps = 16

	// maxCiphertext 是**正文密文**的长度上限，不是整条密文的：整条密文还要
	// 加上头部与 DEK 副本，所以可以比它更长。decode 只用它卡 body_len。
	// 凭证是 API Key 或 OAuth token，几百字节量级；1 MiB 已经远超合理范围，
	// 而它挡住的是「长度字段被构造成 4 GiB」这类输入。
	maxCiphertext = 1 << 20

	// maxKeyID 是主密钥标识的长度上限。
	//
	// 写入侧把标识长度写成一个字节，能表示到 255，但读取侧只认 1..maxKeyID
	// （见 format.go 的 decode），所以真正的上界是这里而不是 255。
	// NewSealer 按同一个常量拒绝过长的标识，两边不会漂移。
	maxKeyID = 64
)

// ErrMalformed 表示密文本身结构不合法——长度字段越界、版本未知等。
//
// 与「解不开」分开：前者说明这行数据坏了或被篡改，后者说明钥匙不对。
// 两者的排查方向完全不同，运维看到的信息也必须能区分。
var ErrMalformed = errors.New("凭证密文格式不合法")

// ErrNoKey 表示这份密文是用本进程没有的主密钥包的。
//
// 最常见的成因是轮换后把旧主密钥从环境变量里删得太早。
// 单独一个错误类型是为了让调用方能给出「缺哪把钥匙」而不是笼统的失败。
var ErrNoKey = errors.New("没有能解开这份密文的主密钥")

// ErrWrapLimit 表示这份密文已经带满数据密钥副本，补不进当前主密钥了。
//
// 与 ErrNoKey 分开的理由和 ErrMalformed 一样：处置动作完全不同。
// 解不开的处置是「把缺的那把钥匙加回配置」，而这一条的处置是
// 「把不再使用的主密钥从配置里删掉」——照前者的方向做只会让副本更多，
// 于是每一次轮换都在原地打转。
var ErrWrapLimit = errors.New("数据密钥副本已达上限")

// Sealer 用一组主密钥封装与解开凭证。并发安全：内部无可变状态。
type Sealer struct {
	keys    []config.MasterKey
	byID    map[string]config.MasterKey
	primary config.MasterKey
}

// NewSealer 用配置里的主密钥构造封装器。
//
// 列表中的第一把作为「包装新 DEK」用的钥匙，其余用于解开存量密文。
// 空列表直接报错而不是构造出一个用不了的对象：那会让「忘了配主密钥」
// 推迟到第一次写凭证时才暴露。
//
// 标识的长度也在这里校验，而不是等写凭证的时候。密文布局把标识长度写成
// 一个字节，读取侧只认 1..maxKeyID：超长的标识写得进去、读不出来，
// 而故障要等到那条渠道被选中解密时才暴露——那时存量密文已经写坏了。
// 代价是某些原本能启动的配置从此拒绝启动，这是有意的：启动期失败
// 比「某天开始所有凭证都解不开」容易排查得多。
func NewSealer(keys []config.MasterKey) (*Sealer, error) {
	if len(keys) == 0 {
		return nil, errors.New("至少需要一把主密钥")
	}
	byID := make(map[string]config.MasterKey, len(keys))
	for _, k := range keys {
		if k.ID == "" {
			// 读取侧同样拒绝 idLen == 0，所以空标识与超长标识是同一类错误：
			// 写得出、读不回。这里报的是「缺少」，因为配置里省略前缀时
			// parseMasterKeys 会补上默认标识，走到这一步就是真的没写。
			return nil, errors.New("主密钥缺少标识")
		}
		if len(k.ID) > maxKeyID {
			// 长度与上界都写进错误信息：只说「太长了」的话，
			// 排查的人不知道是超出多少、也不知道该缩到多少。
			return nil, fmt.Errorf("主密钥标识 %q 长度为 %d 字节，超过上限 %d", k.ID, len(k.ID), maxKeyID)
		}
		if len(k.Key) != config.KeySize {
			return nil, fmt.Errorf("主密钥 %q 长度为 %d 字节，需要 %d", k.ID, len(k.Key), config.KeySize)
		}
		if _, dup := byID[k.ID]; dup {
			return nil, fmt.Errorf("主密钥标识 %q 重复", k.ID)
		}
		byID[k.ID] = k
	}
	return &Sealer{keys: keys, byID: byID, primary: keys[0]}, nil
}

// PrimaryKeyID 返回当前用于包装新凭证的那把主密钥的标识。
// 控制台与运维命令用它提示「现在写进去的密文挂在哪把钥匙下」。
func (s *Sealer) PrimaryKeyID() string { return s.primary.ID }

// Seal 把明文凭证封装成自描述的密文。
//
// 每次调用生成一个全新的 DEK，因此同一份明文两次封装得到完全不同的密文。
// 这不是缺点：它意味着密文本身不泄漏「两个渠道用了同一个 key」这类信息。
func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, errors.New("凭证为空")
	}
	// 上限量的是密文，而正文密文比明文多一个认证标签，所以判据要把标签算进去：
	// 漏算这 16 字节，最上面的那一档就会封出自己读不回来的密文——decode
	// 用同一个 maxCiphertext 校验正文长度，标签同样计入。
	if len(plaintext)+tagSize > maxCiphertext {
		return nil, fmt.Errorf("凭证过长：%d 字节（上限 %d 字节）", len(plaintext), maxCiphertext-tagSize)
	}

	dek, err := randomBytes(dekSize)
	if err != nil {
		return nil, err
	}

	// 用 DEK 加密凭证正文
	dekNonce, err := randomBytes(nonceSize)
	if err != nil {
		return nil, err
	}
	body, err := sealWith(dek, dekNonce, plaintext)
	if err != nil {
		return nil, err
	}

	// 用主密钥包住 DEK。只包一份：新数据不该一上来就带多份副本，
	// 副本是按需（轮换时）添加的。
	w, err := s.wrapDEK(dek)
	if err != nil {
		return nil, err
	}

	return encode([]wrapped{w}, tail(dekNonce, body)), nil
}

// Open 解开密文。
//
// 依次尝试密文里记录的每一份 DEK 副本，用本进程有的主密钥去解。
// 只要有一把能用就能打开——这正是「弄丢一把主密钥不至于让数据变成废纸」的兑现点。
func (s *Sealer) Open(envelope []byte) ([]byte, error) {
	env, err := decode(envelope)
	if err != nil {
		return nil, err
	}

	var tried []string
	for _, w := range env.wraps {
		kek, ok := s.byID[w.keyID]
		if !ok {
			continue
		}
		tried = append(tried, w.keyID)

		dek, err := openWith(kek.Key, w.nonce, w.data)
		if err != nil {
			// 这一把解不开，试下一份。可能这份副本是用别的钥匙包的。
			continue
		}
		plaintext, err := openWith(dek, env.dekNonce, env.body)
		if err != nil {
			// DEK 解出来了但正文解不开：说明密文被改过。
			// 这与「钥匙不对」不同，不该继续静默尝试。
			return nil, fmt.Errorf("%w: 正文校验失败，密文可能已被篡改", ErrMalformed)
		}
		return plaintext, nil
	}

	if len(tried) == 0 {
		ids := env.KeyIDs()
		sort.Strings(ids)
		return nil, fmt.Errorf("%w: 密文由 %v 包装，本进程只有 %v", ErrNoKey, ids, s.KeyIDs())
	}
	return nil, fmt.Errorf("%w: 试过 %v 都解不开", ErrNoKey, tried)
}

// Rewrap 为密文补上当前主密钥的 DEK 副本。
//
// 轮换的做法：把新主密钥放进列表首位、旧密钥留在后面，然后对存量密文调用它。
// 补完之后这份密文新旧钥匙都能开，于是可以把旧钥匙从列表里去掉。
//
// 已经包过的不重复添加，因此对同一份密文重复调用是幂等的——
// 轮换任务可以放心地重跑，不必自己记录进度。
func (s *Sealer) Rewrap(envelopeBytes []byte) ([]byte, error) {
	env, err := decode(envelopeBytes)
	if err != nil {
		return nil, err
	}
	if env.hasKey(s.primary.ID) {
		return envelopeBytes, nil
	}

	// 先想办法把 DEK 弄出来。这里不直接用 Open，因为 Open 会顺带解密正文，
	// 而重包装只需要 DEK——少一次解密也就少一处可能失败的地方。
	var dek []byte
	for _, w := range env.wraps {
		kek, ok := s.byID[w.keyID]
		if !ok {
			continue
		}
		if dek, err = openWith(kek.Key, w.nonce, w.data); err == nil {
			break
		}
	}
	if dek == nil {
		return nil, fmt.Errorf("%w: 无法解出数据密钥", ErrNoKey)
	}

	w, err := s.wrapDEK(dek)
	if err != nil {
		return nil, err
	}
	if len(env.wraps) >= maxWraps {
		// 加上这一份就会超出解析器接受的上限，产出自己读不回来的密文。
		// 报错而不是静默丢弃：这说明主密钥列表该清理了。
		return nil, fmt.Errorf("%w: 已有 %d 份，请先清理不再使用的主密钥", ErrWrapLimit, maxWraps)
	}
	env.wraps = append(env.wraps, w)
	return encode(env.wraps, tail(env.dekNonce, env.body)), nil
}

// KeyIDs 返回本进程持有的全部主密钥标识，顺序与配置一致。
// 打印它不泄漏任何密钥材料，用于启动日志确认配置读对了。
func (s *Sealer) KeyIDs() []string {
	ids := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		ids = append(ids, k.ID)
	}
	return ids
}

// WrappedWith 返回这份密文挂在哪些主密钥下，供轮换进度审计。
// 它不是解密，不需要任何钥匙。
func WrappedWith(envelopeBytes []byte) ([]string, error) {
	env, err := decode(envelopeBytes)
	if err != nil {
		return nil, err
	}
	return env.KeyIDs(), nil
}

func (s *Sealer) wrapDEK(dek []byte) (wrapped, error) {
	nonce, err := randomBytes(nonceSize)
	if err != nil {
		return wrapped{}, err
	}
	sealed, err := sealWith(s.primary.Key, nonce, dek)
	if err != nil {
		return wrapped{}, err
	}
	return wrapped{keyID: s.primary.ID, nonce: nonce, data: sealed}, nil
}

// sealWith 用 key 加密 plaintext。nonce 由调用方给出并负责不重复。
//
// GCM 的 nonce 绝不能重复。本包的两处调用各自生成新随机 nonce，
// 且每个 DEK 只用于一次加密，因此落在随机 96 位 nonce 的安全范围内。
func sealWith(key, nonce, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, aad), nil
}

// openWith 解密。认证失败时返回的错误不包含明文或密钥材料——
// 错误信息会进日志，而日志的去向比密钥本身更难控制。
func openWith(key, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != nonceSize {
		return nil, fmt.Errorf("%w: nonce 长度 %d", ErrMalformed, len(nonce))
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("解密失败")
	}
	return plaintext, nil
}

// aad 是把密文绑定到「用途」的附加认证数据。
//
// 它防的不是读库的人（那种人本来就拿不到主密钥），而是**密文被搬到别处使用**：
// 例如把某个渠道的凭证密文挪到另一个用途的列里，期望它被当成别的什么东西解开。
// 绑定一个固定的用途串即可，不必绑定行标识——绑定行标识会要求先插入拿到 id
// 再回填加密结果，而它挡不住「整份信封被对调」这类攻击，那要在更上层解决。
var aad = []byte("larkax/channel-credential/v1")

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != dekSize {
		return nil, fmt.Errorf("密钥长度为 %d 字节，需要 %d", len(key), dekSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造密码失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("构造 GCM 失败: %w", err)
	}
	return gcm, nil
}

func randomBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("生成随机数失败: %w", err)
	}
	return buf, nil
}
