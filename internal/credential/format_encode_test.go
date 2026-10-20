package credential

import (
	"bytes"
	"testing"
)

// format.go 里 encode 与 tail 的容量计算只用于 make([]byte, 0, size) 的 cap 参数：
//
//	size += 1 + len(w.keyID) + nonceSize + 2 + len(w.data)   // 79:13/28/40
//	make([]byte, 0, len(dekNonce)+4+len(body))                // 96:38
//
// 输出字节由 append 决定：cap 不够时 append 自己扩容，cap 更大只是少几次扩容，
// 拼出来的序列完全相同。所以这几处 ARITHMETIC_BASE 变异体没有区分输入：
// 把某个 `+` 换成 `-`，值也只进 cap，杀不掉，也不该为了杀它去断言分配次数
// 或 cap；GC 与内存峰值不是本函数的可观测契约。
//
// 等价还依赖另一个前提：改写后的 size 在可达输入上保持非负。
// 同一行**最后**那个 `+ len(w.data)` 不在这份清单里——换成 `-` 之后，
// size 会随副本份数递减：单副本、data 48、body 16 时仍是 2；
// 两份副本各 48 字节、body 16 时则是 -30；make([]byte, 0, size)
// 直接 panic。它因此是被 panic 杀掉的，不是等价存活。
//
// 能钉住的是布局：下面用字面量把各段字节数写死，让 encode 写出的东西与
// decode 读得回的东西对得上。上面两条前提缺一不可；若将来改成按 size 切片翻写，
// 或开始断言 cap，等价性就不再成立，说明与用例要一起重看。
//
// 期望里刻意不出现 headerSize / nonceSize / fixedTailLen：拿被测常量去算期望，
// 常量改错时断言跟着一起错，等于没测。
func TestEncodeLayoutMatchesLiteralSizes(t *testing.T) {
	nonce := func(fill byte) []byte { return bytes.Repeat([]byte{fill}, 12) }

	cases := []struct {
		name  string
		wraps []wrapped
	}{
		{
			name: "一份副本",
			wraps: []wrapped{{
				keyID: "current",
				nonce: nonce(0xA1),
				data:  bytes.Repeat([]byte{0xB2}, 48),
			}},
		},
		{
			name: "两份副本",
			wraps: []wrapped{
				{keyID: "current", nonce: nonce(0xA1), data: bytes.Repeat([]byte{0xB2}, 48)},
				{keyID: "previous", nonce: nonce(0xC3), data: bytes.Repeat([]byte{0xD4}, 64)},
			},
		},
	}

	dekNonce := nonce(0xE5)
	body := bytes.Repeat([]byte{0xF6}, 80)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := encode(c.wraps, tail(dekNonce, body))

			// 头部 2 字节、尾部 12（dek_nonce）+ 4（正文长度）+ 正文，
			// 每份副本 1（标识长度）+ 标识 + 12（nonce）+ 2（数据长度）+ 数据。
			wantLen := 2 + 12 + 4 + len(body)
			for _, w := range c.wraps {
				wantLen += 1 + len(w.keyID) + 12 + 2 + len(w.data)
			}
			if len(raw) != wantLen {
				t.Fatalf("密文长度 %d，按布局应为 %d", len(raw), wantLen)
			}
			// 版本号写死字面量：拿 version 常量去测 version 等于没测。
			if raw[0] != 1 {
				t.Errorf("首字节 = %d，期望版本 1", raw[0])
			}
			if got := int(raw[1]); got != len(c.wraps) {
				t.Errorf("副本份数字节 = %d，期望 %d", got, len(c.wraps))
			}

			env, err := decode(raw)
			if err != nil {
				t.Fatalf("encode 写出的密文应能被 decode 读回: %v", err)
			}
			if len(env.wraps) != len(c.wraps) {
				t.Fatalf("读回 %d 份副本，期望 %d", len(env.wraps), len(c.wraps))
			}
			for i, want := range c.wraps {
				got := env.wraps[i]
				if got.keyID != want.keyID ||
					!bytes.Equal(got.nonce, want.nonce) ||
					!bytes.Equal(got.data, want.data) {
					t.Errorf("第 %d 份副本读回后不一致", i+1)
				}
			}
			if !bytes.Equal(env.dekNonce, dekNonce) {
				t.Error("dek_nonce 读回后不一致")
			}
			if !bytes.Equal(env.body, body) {
				t.Error("正文读回后不一致")
			}
		})
	}
}
