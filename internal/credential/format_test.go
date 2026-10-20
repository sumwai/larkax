package credential

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// decode 的边界用例。
//
// 每一组都贴着同一条判据的两侧各取一个点：正好相等、差一个字节。
// 断言的是错误文案里的具体数字，而不只是 err != nil——守卫写宽一字节、
// 写窄一字节时，后者全都过得去，那正是这类变异存活的原因。
//
// 构造输入时刻意绕开 encode：被测的正是解析器对长度字段的态度，
// 从 encode 走一遍就只能造出合法的密文。

// 拼字节的小工具。长度字段直接写进字节里，不做任何校验。
func be16(n int) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(n))
	return b[:]
}

func be32(n int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return b[:]
}

func repeatByte(n int, fill byte) []byte {
	return bytes.Repeat([]byte{fill}, n)
}

func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// head 是版本与副本份数两个字节。
func head(count int) []byte { return []byte{version, byte(count)} }

// oneWrap 拼一个完整的副本：标识 + nonce + 数据长度 + 数据。
func oneWrap(keyID string, nonce, data []byte) []byte {
	return join(
		[]byte{byte(len(keyID))}, []byte(keyID),
		nonce,
		be16(len(data)), data,
	)
}

// tailOf 拼 dek_nonce + 正文长度 + 正文。
func tailOf(dekNonce, body []byte) []byte {
	return join(dekNonce, be32(len(body)), body)
}

// goodRaw 返回一份结构合法的单副本密文。
func goodRaw(keyID string, nonce, data, dekNonce, body []byte) []byte {
	return join(head(1), oneWrap(keyID, nonce, data), tailOf(dekNonce, body))
}

// wrapPrefix 返回头部 + 一个完整副本，长度 34 字节。
// 后面的用例在它屁股上接各种尾巴。
func wrapPrefix() []byte {
	return join(head(1), oneWrap("k", repeatByte(12, 0xAA), repeatByte(16, 0x01)))
}

func mustDecode(t *testing.T, raw []byte) *envelope {
	t.Helper()
	env, err := decode(raw)
	if err != nil {
		t.Fatalf("应能解析，实际报错: %v", err)
	}
	return env
}

func wantDecodeError(t *testing.T, raw []byte, want string) {
	t.Helper()
	_, err := decode(raw)
	if err == nil {
		t.Fatalf("应拒绝，期望错误包含 %q", want)
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("错误应归属 ErrMalformed: %v", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("错误为 %q，期望包含 %q", err, want)
	}
}

// 头部判据 len(raw) < headerSize 的边界。
//
// 恰好两个字节时头部是完整的，接下来该说「第一个副本缺少标识长度」；
// 只有少一个字节才该说「不足头部」。两边的错误文案不同，所以都能钉住。
func TestDecodeHeaderBoundary(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"空输入", nil, "长度 0 不足头部"},
		{"只有一个字节", []byte{version}, "长度 1 不足头部"},
		{"恰好头部长度", []byte{version, 1}, "第 1 个副本缺少标识长度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantDecodeError(t, tc.raw, tc.want)
		})
	}
}

// 标识长度字段本身占一个字节：pos+1 恰好到达末尾时字段存在，值为 0，
// 该报「长度不合法」而不是「字段缺席」。
func TestDecodeIDLengthFieldBoundary(t *testing.T) {
	wantDecodeError(t, []byte{version, 1, 0}, "第 1 个副本的标识长度 0 不合法")
}

// 错误信息里的副本序号从 1 起。
//
// 只让第一个副本出错时，i+1 改成 i-1 都落在错误分支里，没人比对数字；
// 让第二个副本出错并断言「第 2 个」，这个位移才暴露出来。
func TestDecodeReportsSecondWrapIndex(t *testing.T) {
	first := oneWrap("k", repeatByte(12, 0xAA), repeatByte(16, 0x01))
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"缺少标识长度", join(head(2), first), "第 2 个副本缺少标识长度"},
		{"标识长度为 0", join(head(2), first, []byte{0}), "第 2 个副本的标识长度 0 不合法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantDecodeError(t, tc.raw, tc.want)
		})
	}
}

// 标识长度只接受 1..64：0 与 65 两侧都必须拒，恰好 64 必须收。
//
// 两侧各一个用例，是为了钉住这两个条件的析取：把 || 改成 && 之后，
// 「恰好为零」与「恰好超界」都会被放过去，继续按错位的字节解析。
func TestDecodeKeyIDLengthBounds(t *testing.T) {
	data := repeatByte(16, 0x02)
	body := repeatByte(16, 0x03)
	tailPart := tailOf(repeatByte(12, 0xBB), body)

	t.Run("恰好上界", func(t *testing.T) {
		id := strings.Repeat("i", 64)
		env := mustDecode(t, goodRaw(id, repeatByte(12, 0xAA), data, repeatByte(12, 0xBB), body))
		if len(env.wraps) != 1 || env.wraps[0].keyID != id {
			t.Fatalf("标识应为 64 字节的原样内容，实际 %q", env.wraps[0].keyID)
		}
	})

	t.Run("上界加一", func(t *testing.T) {
		id := strings.Repeat("i", 65)
		raw := join(head(1), []byte{65}, []byte(id), repeatByte(12, 0xAA), be16(16), data, tailPart)
		wantDecodeError(t, raw, "第 1 个副本的标识长度 65 不合法")
	})

	t.Run("为零", func(t *testing.T) {
		// 零长度标识后面接一份结构完全合法的剩余部分：若守卫被绕过，
		// 解析器会一路走到末尾并成功返回。
		raw := join(head(1), []byte{0}, repeatByte(12, 0xAA), be16(16), data, tailPart)
		wantDecodeError(t, raw, "第 1 个副本的标识长度 0 不合法")
	})
}

// 标识是否被截断：恰好贴到密文末尾算「没截断」，只是后面缺 nonce。
// 少一个字节才算「标识被截断」。
func TestDecodeTruncatedKeyIDBoundary(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		// 长度字段说是 1，正好 1 个字节，之后没有任何东西。
		{"恰好贴到末尾", []byte{version, 1, 1, 'k'}, "第 1 个副本缺少 nonce 或长度"},
		// 长度字段说是 2，只有 1 个字节。
		{"少一个字节", []byte{version, 1, 2, 'k'}, "第 1 个副本的标识被截断"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantDecodeError(t, tc.raw, tc.want)
		})
	}
}

// nonce + 数据长度这 14 个字节的边界。
//
// 恰好 14 字节：nonce 与长度字段都在，长度字段声明 16 字节数据但数据不在，
// 该报「数据被截断」；少一个字节才该报「缺少 nonce 或长度」。
func TestDecodeNonceOrLengthBoundary(t *testing.T) {
	prefix := join(head(1), []byte{1, 'k'}) // 4 字节

	cases := []struct {
		name string
		rest []byte
		want string
	}{
		{"恰好 14 字节", join(repeatByte(12, 0xAA), be16(16)), "第 1 个副本的数据被截断"},
		{"少一个字节", join(repeatByte(12, 0xAA), []byte{0x00}), "第 1 个副本缺少 nonce 或长度"},
		{"只剩 4 字节", repeatByte(4, 0xAA), "第 1 个副本缺少 nonce 或长度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantDecodeError(t, join(prefix, tc.rest), tc.want)
		})
	}
}

// 副本数据长度：16 是最小合法值（认证标签），15 必须拒；
// 数据恰好填满密文时下一道守卫报缺尾巴，多一个字节才算「数据被截断」。
func TestDecodeDataLengthBoundary(t *testing.T) {
	body := repeatByte(16, 0x03)
	data := repeatByte(16, 0x02)
	dekNonce := repeatByte(12, 0xBB)

	t.Run("恰好最小合法值", func(t *testing.T) {
		env := mustDecode(t, goodRaw("k", repeatByte(12, 0xAA), data, dekNonce, body))
		if !bytes.Equal(env.wraps[0].data, data) {
			t.Errorf("数据应为 16 字节原样内容")
		}
	})

	t.Run("小于认证标签", func(t *testing.T) {
		raw := join(head(1), []byte{1, 'k'}, repeatByte(12, 0xAA), be16(15), repeatByte(15, 0x02), tailOf(dekNonce, body))
		wantDecodeError(t, raw, "第 1 个副本的数据长度 15 小于认证标签")
	})

	t.Run("恰好贴到末尾", func(t *testing.T) {
		raw := join(head(1), oneWrap("k", repeatByte(12, 0xAA), data))
		wantDecodeError(t, raw, "缺少 dek_nonce 或正文长度")
	})

	t.Run("多声明一个字节", func(t *testing.T) {
		raw := join(head(1), []byte{1, 'k'}, repeatByte(12, 0xAA), be16(17), repeatByte(16, 0x02))
		wantDecodeError(t, raw, "第 1 个副本的数据被截断")
	})
}

// 尾部 dek_nonce + 正文长度这 16 个字节的边界。
//
// 恰好 16 字节：两段都在，正文长度读出来是 0，该报「正文长度 0 小于认证标签」；
// 少一个字节才该报「缺少 dek_nonce 或正文长度」。
func TestDecodeFixedTailBoundary(t *testing.T) {
	prefix := wrapPrefix()

	cases := []struct {
		name string
		rest []byte
		want string
	}{
		{"恰好 16 字节", join(repeatByte(12, 0xBB), be32(0)), "正文长度 0 小于认证标签"},
		{"少一个字节", join(repeatByte(12, 0xBB), []byte{0, 0, 0}), "缺少 dek_nonce 或正文长度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantDecodeError(t, join(prefix, tc.rest), tc.want)
		})
	}
}

// 正文长度的边界：16 是最小合法值，15 必须拒；
// 1 MiB（1048576）是上限，多一个字节必须拒。
func TestDecodeBodyLengthBounds(t *testing.T) {
	data := repeatByte(16, 0x02)
	dekNonce := repeatByte(12, 0xBB)

	t.Run("恰好最小合法值", func(t *testing.T) {
		body := repeatByte(16, 0x03)
		env := mustDecode(t, goodRaw("k", repeatByte(12, 0xAA), data, dekNonce, body))
		if !bytes.Equal(env.body, body) || len(env.body) != 16 {
			t.Errorf("正文应为 16 字节原样内容，实际 %d 字节", len(env.body))
		}
	})

	t.Run("小于认证标签", func(t *testing.T) {
		raw := join(head(1), oneWrap("k", repeatByte(12, 0xAA), data), dekNonce, be32(15), repeatByte(15, 0x03))
		wantDecodeError(t, raw, "正文长度 15 小于认证标签")
	})

	t.Run("恰好等于上限", func(t *testing.T) {
		body := repeatByte(1<<20, 0x03)
		env := mustDecode(t, goodRaw("k", repeatByte(12, 0xAA), data, dekNonce, body))
		if len(env.body) != 1048576 {
			t.Fatalf("正文应为 1048576 字节，实际 %d", len(env.body))
		}
	})

	t.Run("上限加一", func(t *testing.T) {
		raw := join(head(1), oneWrap("k", repeatByte(12, 0xAA), data), dekNonce, be32(1<<20+1))
		wantDecodeError(t, raw, "正文长度 1048577 超过上限 1048576")
	})
}

// 正文长度与实际剩余不符时，报错要给出真实的剩余字节数。
//
// 剩余数写成 len(raw)-pos：写成加法或被取反之后，同一个输入会打出
// 117 或 -17 而不是 17。这里断言字面量，两种错法都留不下来。
func TestDecodeBodyLengthMismatchMessage(t *testing.T) {
	prefix := wrapPrefix()
	data := repeatByte(16, 0x02)

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"声明 16 实际 17", join(prefix, repeatByte(12, 0xBB), be32(16), repeatByte(17, 0x03)), "正文长度 16 与实际剩余 17 不符"},
		{"声明 17 实际 16", join(prefix, repeatByte(12, 0xBB), be32(17), data), "正文长度 17 与实际剩余 16 不符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantDecodeError(t, tc.raw, tc.want)
		})
	}
}

// 完整往返一份合法密文，确认上面那些守卫没有把正常输入挡掉。
func TestDecodeValidEnvelope(t *testing.T) {
	nonce := repeatByte(12, 0xAA)
	dekNonce := repeatByte(12, 0xBB)
	keyID := "current"
	data := bytes.Repeat([]byte{0x11, 0x22}, 20)
	body := bytes.Repeat([]byte{0x33, 0x44}, 40)

	env := mustDecode(t, goodRaw(keyID, nonce, data, dekNonce, body))
	if len(env.wraps) != 1 {
		t.Fatalf("副本份数应为 1，实际 %d", len(env.wraps))
	}
	if env.wraps[0].keyID != keyID {
		t.Errorf("标识为 %q，期望 %q", env.wraps[0].keyID, keyID)
	}
	if !bytes.Equal(env.wraps[0].nonce, nonce) {
		t.Error("nonce 与写入的不一致")
	}
	if !bytes.Equal(env.wraps[0].data, data) {
		t.Error("副本数据与写入的不一致")
	}
	if !bytes.Equal(env.dekNonce, dekNonce) {
		t.Error("dek_nonce 与写入的不一致")
	}
	if !bytes.Equal(env.body, body) {
		t.Error("正文与写入的不一致")
	}
}
