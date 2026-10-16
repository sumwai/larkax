package credential

import (
	"encoding/binary"
	"fmt"
)

// 密文布局（version 1）：
//
//	┌──────────────────────────────────────────────────────────┐
//	│ version           1 字节                                  │
//	│ wrap_count        1 字节，DEK 副本份数                     │
//	│ 每个副本：                                                 │
//	│   key_id_len      1 字节                                  │
//	│   key_id          key_id_len 字节（UTF-8）                 │
//	│   nonce           12 字节                                 │
//	│   wrapped_len     2 字节（大端），被包的 DEK 长度           │
//	│   wrapped         wrapped_len 字节                        │
//	│ dek_nonce         12 字节                                 │
//	│ body_len          4 字节（大端）                           │
//	│ body              body_len 字节，凭证正文密文              │
//	└──────────────────────────────────────────────────────────┘
//
// 为什么自描述而不是固定布局：DEK 副本的份数会随轮换变化，
// 主密钥标识的长度也不固定。固定布局就要预留最大长度，
// 那会让每条密文都多背几十字节的填充，而凭证数量与渠道数同阶。
//
// 读取侧逐个校验每个长度字段：解析器面对的是数据库里的字节，而数据库里的
// 字节可能来自任何写入路径——一个被构造的长度字段会让后续分配吃掉几个 GB，
// 所以边界的检查不能省。
//
// 写入侧不重复做这套校验：长度都来自受控输入。唯一的例外是 key_id——
// encode 直接把它写成 1 字节，而读取侧只接受不超过 maxKeyID 的取值，
// 所以主密钥标识过长时写出来的密文自己读不回来。

const (
	headerSize   = 2
	fixedTailLen = nonceSize + 4
)

type wrapped struct {
	keyID string
	nonce []byte
	data  []byte
}

type envelope struct {
	wraps    []wrapped
	dekNonce []byte
	body     []byte
}

func (e *envelope) hasKey(id string) bool {
	for _, w := range e.wraps {
		if w.keyID == id {
			return true
		}
	}
	return false
}

// KeyIDs 返回密文里包含数据密钥副本的主密钥标识。
//
// 用于排查「这份密文该用哪把钥匙解」：标识对不上时，错误信息里给出的
// 就是这里返回的清单。
func (e *envelope) KeyIDs() []string {
	ids := make([]string, 0, len(e.wraps))
	for _, w := range e.wraps {
		ids = append(ids, w.keyID)
	}
	return ids
}

// encode 按布局拼出密文。tail 是 dek_nonce + body_len + body 三段。
func encode(wraps []wrapped, tail []byte) []byte {
	size := headerSize + len(tail)
	for _, w := range wraps {
		size += 1 + len(w.keyID) + nonceSize + 2 + len(w.data)
	}

	buf := make([]byte, 0, size)
	buf = append(buf, version, byte(len(wraps)))
	for _, w := range wraps {
		buf = append(buf, byte(len(w.keyID)))
		buf = append(buf, w.keyID...)
		buf = append(buf, w.nonce...)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(w.data)))
		buf = append(buf, w.data...)
	}
	return append(buf, tail...)
}

// tail 拼出 encode 期望的最后一段。
func tail(dekNonce, body []byte) []byte {
	out := make([]byte, 0, len(dekNonce)+4+len(body))
	out = append(out, dekNonce...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...)
}

func decode(raw []byte) (*envelope, error) {
	if len(raw) < headerSize {
		return nil, fmt.Errorf("%w: 长度 %d 不足头部", ErrMalformed, len(raw))
	}
	if raw[0] != version {
		return nil, fmt.Errorf("%w: 版本 %d 未知", ErrMalformed, raw[0])
	}

	count := int(raw[1])
	if count == 0 {
		return nil, fmt.Errorf("%w: 没有任何数据密钥副本", ErrMalformed)
	}
	if count > maxWraps {
		return nil, fmt.Errorf("%w: 副本份数 %d 超过上限 %d", ErrMalformed, count, maxWraps)
	}

	pos := headerSize
	wraps := make([]wrapped, 0, count)
	for i := 0; i < count; i++ {
		if pos+1 > len(raw) {
			return nil, fmt.Errorf("%w: 第 %d 个副本缺少标识长度", ErrMalformed, i+1)
		}
		idLen := int(raw[pos])
		pos++
		if idLen == 0 || idLen > maxKeyID {
			return nil, fmt.Errorf("%w: 第 %d 个副本的标识长度 %d 不合法", ErrMalformed, i+1, idLen)
		}
		if pos+idLen > len(raw) {
			return nil, fmt.Errorf("%w: 第 %d 个副本的标识被截断", ErrMalformed, i+1)
		}
		keyID := string(raw[pos : pos+idLen])
		pos += idLen

		if pos+nonceSize+2 > len(raw) {
			return nil, fmt.Errorf("%w: 第 %d 个副本缺少 nonce 或长度", ErrMalformed, i+1)
		}
		nonce := append([]byte(nil), raw[pos:pos+nonceSize]...)
		pos += nonceSize
		dataLen := int(binary.BigEndian.Uint16(raw[pos : pos+2]))
		pos += 2

		if dataLen < tagSize {
			return nil, fmt.Errorf("%w: 第 %d 个副本的数据长度 %d 小于认证标签", ErrMalformed, i+1, dataLen)
		}
		if pos+dataLen > len(raw) {
			return nil, fmt.Errorf("%w: 第 %d 个副本的数据被截断", ErrMalformed, i+1)
		}
		data := append([]byte(nil), raw[pos:pos+dataLen]...)
		pos += dataLen

		wraps = append(wraps, wrapped{keyID: keyID, nonce: nonce, data: data})
	}

	if pos+fixedTailLen > len(raw) {
		return nil, fmt.Errorf("%w: 缺少 dek_nonce 或正文长度", ErrMalformed)
	}
	dekNonce := append([]byte(nil), raw[pos:pos+nonceSize]...)
	pos += nonceSize
	bodyLen := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
	pos += 4

	if bodyLen < tagSize {
		return nil, fmt.Errorf("%w: 正文长度 %d 小于认证标签", ErrMalformed, bodyLen)
	}
	if bodyLen > maxCiphertext {
		return nil, fmt.Errorf("%w: 正文长度 %d 超过上限 %d", ErrMalformed, bodyLen, maxCiphertext)
	}
	if pos+bodyLen != len(raw) {
		return nil, fmt.Errorf("%w: 正文长度 %d 与实际剩余 %d 不符", ErrMalformed, bodyLen, len(raw)-pos)
	}

	return &envelope{
		wraps:    wraps,
		dekNonce: dekNonce,
		body:     append([]byte(nil), raw[pos:]...),
	}, nil
}
