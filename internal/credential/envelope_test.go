package credential

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/config"
)

func key(id string, fill byte) config.MasterKey {
	k := make([]byte, config.KeySize)
	for i := range k {
		k[i] = fill
	}
	return config.MasterKey{ID: id, Key: k}
}

// testKeys 返回两把主密钥，第一把是当前用于包装的。
func testKeys() []config.MasterKey {
	return []config.MasterKey{key("current", 0x11), key("previous", 0x22)}
}

func mustSealer(t *testing.T, keys []config.MasterKey) *Sealer {
	t.Helper()
	s, err := NewSealer(keys)
	if err != nil {
		t.Fatalf("NewSealer 出错: %v", err)
	}
	return s
}

func TestNewSealerValidation(t *testing.T) {
	cases := []struct {
		name string
		keys []config.MasterKey
	}{
		{"空列表", nil},
		{"缺少标识", []config.MasterKey{{Key: make([]byte, config.KeySize)}}},
		{"长度不对", []config.MasterKey{{ID: "a", Key: make([]byte, 16)}}},
		{"标识重复", []config.MasterKey{key("dup", 1), key("dup", 2)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSealer(tc.keys); err == nil {
				t.Error("应报错")
			}
		})
	}
}

func TestPrimaryKeyIDIsFirst(t *testing.T) {
	s := mustSealer(t, testKeys())
	if got := s.PrimaryKeyID(); got != "current" {
		t.Errorf("PrimaryKeyID() = %q，期望列表首位的 current", got)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := mustSealer(t, testKeys())

	cases := [][]byte{
		[]byte("placeholder-credential-value"),
		bytes.Repeat([]byte("x"), 4096),
		[]byte("含中文与 emoji 🎯 的凭证"),
	}
	for _, plaintext := range cases {
		sealed, err := s.Seal(plaintext)
		if err != nil {
			t.Fatalf("Seal 出错: %v", err)
		}
		got, err := s.Open(sealed)
		if err != nil {
			t.Fatalf("Open 出错: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("往返后得到 %q，期望 %q", got, plaintext)
		}
	}
}

// 同一份明文两次封装必须得到不同密文，否则密文本身就泄漏了
// 「两个渠道配了同一个 key」这类信息。
func TestSealIsNonDeterministic(t *testing.T) {
	s := mustSealer(t, testKeys())
	plaintext := []byte("same-secret")

	a, err := s.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("两次封装得到相同密文")
	}

	// 但两者的解密结果必须相同
	for _, sealed := range [][]byte{a, b} {
		got, err := s.Open(sealed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Error("解密结果不一致")
		}
	}
}

// 密文里不应出现明文——这是最基础的检查，但它挡住的是「误用了
// 编码而非加密」这类会让整套机制形同虚设的错误。
func TestCiphertextDoesNotContainPlaintext(t *testing.T) {
	s := mustSealer(t, testKeys())
	plaintext := []byte("super-secret-api-key-value")

	sealed, err := s.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("密文里含明文")
	}
	// 主密钥材料也不该出现在密文里
	if bytes.Contains(sealed, s.keys[0].Key) {
		t.Fatal("密文里含主密钥")
	}
}

// 轮换的兑现点：新主密钥在首位，旧密文仍能用旧钥匙打开。
func TestOpenWithOlderKey(t *testing.T) {
	oldOnly := mustSealer(t, []config.MasterKey{key("old", 0x33)})
	sealed, err := oldOnly.Seal([]byte("rotated-secret"))
	if err != nil {
		t.Fatal(err)
	}

	// 轮换后：新钥匙在前，旧钥匙留着
	rotated := mustSealer(t, []config.MasterKey{key("new", 0x44), key("old", 0x33)})

	got, err := rotated.Open(sealed)
	if err != nil {
		t.Fatalf("轮换后应仍能打开旧密文: %v", err)
	}
	if string(got) != "rotated-secret" {
		t.Errorf("得到 %q", got)
	}

	// 轮换后新写的密文只挂在新钥匙下
	fresh, err := rotated.Seal([]byte("after-rotation"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := WrappedWith(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "new" {
		t.Errorf("新密文挂在 %v，期望只有 new", ids)
	}
}

// 轮换中途：把旧钥匙从环境变量里删早了，报错必须说清是缺钥匙。
func TestOpenReportsMissingKey(t *testing.T) {
	oldOnly := mustSealer(t, []config.MasterKey{key("old", 0x33)})
	sealed, err := oldOnly.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	wrong := mustSealer(t, []config.MasterKey{key("new", 0x44)})
	_, err = wrong.Open(sealed)
	if err == nil {
		t.Fatal("应报错")
	}
	if !errors.Is(err, ErrNoKey) {
		t.Errorf("期望 ErrNoKey，得到 %v", err)
	}
	// 错误信息要能指出缺哪把钥匙，否则运维无从下手
	if !strings.Contains(err.Error(), "old") {
		t.Errorf("错误信息未指出缺少的主密钥标识: %v", err)
	}
}

// Rewrap 之后，只剩新钥匙的进程也能打开——这是轮换能收尾的前提。
func TestRewrapEnablesRemovingOldKey(t *testing.T) {
	oldOnly := mustSealer(t, []config.MasterKey{key("old", 0x33)})
	sealed, err := oldOnly.Seal([]byte("to-rewrap"))
	if err != nil {
		t.Fatal(err)
	}

	rotated := mustSealer(t, []config.MasterKey{key("new", 0x44), key("old", 0x33)})
	rewrapped, err := rotated.Rewrap(sealed)
	if err != nil {
		t.Fatalf("Rewrap 出错: %v", err)
	}

	ids, err := WrappedWith(rewrapped)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("重包装后有 %v 份副本，期望 2 份", ids)
	}

	// 现在去掉旧钥匙，仍应能打开
	newOnly := mustSealer(t, []config.MasterKey{key("new", 0x44)})
	got, err := newOnly.Open(rewrapped)
	if err != nil {
		t.Fatalf("重包装后应能用新钥匙打开: %v", err)
	}
	if string(got) != "to-rewrap" {
		t.Errorf("得到 %q", got)
	}
}

// 轮换任务会重跑，重复 Rewrap 不能无限叠加副本。
func TestRewrapIsIdempotent(t *testing.T) {
	oldOnly := mustSealer(t, []config.MasterKey{key("old", 0x33)})
	sealed, _ := oldOnly.Seal([]byte("idem"))

	rotated := mustSealer(t, []config.MasterKey{key("new", 0x44), key("old", 0x33)})

	once, err := rotated.Rewrap(sealed)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := rotated.Rewrap(once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Error("重复 Rewrap 应原样返回")
	}

	ids, _ := WrappedWith(twice)
	if len(ids) != 2 {
		t.Errorf("重复 Rewrap 后副本份数为 %d，期望仍为 2", len(ids))
	}
}

// 密文被改动一个字节就必须拒绝——这是认证加密存在的全部意义。
func TestTamperIsRejected(t *testing.T) {
	s := mustSealer(t, testKeys())
	sealed, err := s.Seal([]byte("tamper-me"))
	if err != nil {
		t.Fatal(err)
	}

	positions := []int{0, 1, len(sealed) - 1, len(sealed) / 2}
	for _, pos := range positions {
		damaged := append([]byte(nil), sealed...)
		damaged[pos] ^= 0x01
		if _, err := s.Open(damaged); err == nil {
			t.Errorf("改动第 %d 字节后仍能解开", pos)
		}
	}
}

// 截断与超长都要被识破，不能被当成合法的短密文。
func TestTruncatedCiphertextIsRejected(t *testing.T) {
	s := mustSealer(t, testKeys())
	sealed, err := s.Seal([]byte("truncate-me"))
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []int{0, 1, 2, len(sealed) - 1} {
		if _, err := s.Open(sealed[:n]); err == nil {
			t.Errorf("截断到 %d 字节后仍能解开", n)
		}
	}
	// 尾部多出字节同样不合法
	if _, err := s.Open(append(append([]byte(nil), sealed...), 0x00)); err == nil {
		t.Error("尾部多出字节仍能解开")
	}
}

// 构造一个声明了超大长度字段的密文。若解析器信了这个字段去分配内存，
// 一条几字节的输入就能让进程吃掉几个 GB。
func TestHugeLengthFieldsAreRejected(t *testing.T) {
	cases := map[string][]byte{
		"未知版本":   {0xFF, 0x01},
		"零副本":    {version, 0x00},
		"副本数超限":  {version, 0xFF},
		"正文长度超限": buildRaw(0x01, 0x00, 0x01, 0x00, 0xFF),
		"空输入":    {},
		"只有一字节":  {version},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(raw); err == nil {
				t.Error("畸形密文应被拒绝")
			}
		})
	}
}

// buildRaw 拼一段头部 + 若干字节，用于喂给解析器。
func buildRaw(parts ...byte) []byte { return parts }

func TestSealRejectsEmptyAndOversized(t *testing.T) {
	s := mustSealer(t, testKeys())

	if _, err := s.Seal(nil); err == nil {
		t.Error("空凭证应被拒绝")
	}
	if _, err := s.Seal(make([]byte, maxCiphertext+1)); err == nil {
		t.Error("超长凭证应被拒绝")
	}
}

// WrappedWith 只是读头部，不该需要任何钥匙。
func TestWrappedWithNeedsNoKey(t *testing.T) {
	s := mustSealer(t, testKeys())
	sealed, err := s.Seal([]byte("probe"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := WrappedWith(sealed)
	if err != nil {
		t.Fatalf("WrappedWith 出错: %v", err)
	}
	if len(ids) != 1 || ids[0] != "current" {
		t.Errorf("得到 %v，期望 [current]", ids)
	}

	if _, err := WrappedWith([]byte("not-an-envelope")); err == nil {
		t.Error("非密文输入应报错")
	}
}

// Sealer 会被多个 goroutine 共用（每个请求都可能解密凭证）。
func TestSealerIsConcurrencySafe(t *testing.T) {
	s := mustSealer(t, testKeys())
	plaintext := []byte("concurrent-secret")

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sealed, err := s.Seal(plaintext)
			if err != nil {
				t.Errorf("Seal 出错: %v", err)
				return
			}
			got, err := s.Open(sealed)
			if err != nil {
				t.Errorf("Open 出错: %v", err)
				return
			}
			if !bytes.Equal(got, plaintext) {
				t.Error("并发往返结果不一致")
			}
		}()
	}
	wg.Wait()
}

// 密文设计上要比明文长，但不应长到离谱——凭证数量与渠道数同阶，
// 每条多背几百字节会实打实地压在库上。
func TestEnvelopeOverheadIsSmall(t *testing.T) {
	s := mustSealer(t, testKeys())
	plaintext := bytes.Repeat([]byte("k"), 128)
	sealed, err := s.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(sealed) - len(plaintext)
	// 头部 + 一份副本 + 两个 GCM 标签 + 两个 nonce + 各长度字段 ≈ 100 上下
	if overhead > 200 {
		t.Errorf("每条密文额外开销 %d 字节，超出预期", overhead)
	}
}

// 轮换时若连一份能解的副本都没有，必须明确报「缺钥匙」而不是笼统失败。
func TestRewrapWithoutUsableKey(t *testing.T) {
	oldOnly := mustSealer(t, []config.MasterKey{key("old", 0x33)})
	sealed, err := oldOnly.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	wrong := mustSealer(t, []config.MasterKey{key("new", 0x44)})
	_, err = wrong.Rewrap(sealed)
	if !errors.Is(err, ErrNoKey) {
		t.Errorf("期望 ErrNoKey，得到 %v", err)
	}
}

// DEK 解得出来但正文解不开，说明密文被改过——这与「钥匙不对」不是一回事，
// 不能继续静默地试下一把。
func TestCorruptedBodyIsReportedAsTampering(t *testing.T) {
	s := mustSealer(t, testKeys())
	sealed, err := s.Seal([]byte("body-integrity"))
	if err != nil {
		t.Fatal(err)
	}

	// 只在正文段上做改动，保留包装段完整，于是 DEK 仍能解出。
	// 直接改最后一个字节——它必然落在正文密文里。
	damaged := append([]byte(nil), sealed...)
	damaged[len(damaged)-1] ^= 0x01

	_, err = s.Open(damaged)
	if err == nil {
		t.Fatal("应报错")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("期望 ErrMalformed（结构合法但校验失败），得到 %v", err)
	}
}

// 主密钥标识出现在错误信息里是必要的：运维要靠它知道该找回哪把钥匙。
// 但密钥材料本身绝不能出现在任何错误或输出里。
func TestErrorsNeverLeakKeyMaterial(t *testing.T) {
	oldOnly := mustSealer(t, []config.MasterKey{key("old", 0xAB)})
	sealed, _ := oldOnly.Seal([]byte("leak-check"))

	wrong := mustSealer(t, []config.MasterKey{key("new", 0xCD)})
	_, err := wrong.Open(sealed)
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	if strings.Contains(msg, string(wrong.keys[0].Key)) || strings.Contains(msg, string(oldOnly.keys[0].Key)) {
		t.Errorf("错误信息里含密钥材料: %s", msg)
	}

	// 密钥自身的打印形式也不该含材料
	if strings.Contains(wrong.keys[0].String(), string(wrong.keys[0].Key)) {
		t.Error("MasterKey 的打印形式含密钥材料")
	}
}

// 副本份数一旦超过解析器接受的上限，就会产出自己读不回来的密文。
// 这比「拒绝」更糟：数据写进去了，但永远打不开。
func TestRewrapRefusesToExceedWrapLimit(t *testing.T) {
	// 先造一份已经带满副本的密文
	keys := make([]config.MasterKey, 0, maxWraps)
	for i := 0; i < maxWraps; i++ {
		keys = append(keys, key(fmt.Sprintf("k%02d", i), byte(i+1)))
	}
	// 用第一把造一份密文，然后逐把补齐到上限
	sealed, err := mustSealer(t, keys[:1]).Seal([]byte("grow"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxWraps; i++ {
		// 每一轮：新钥匙放首位用于包装，原来的钥匙留在后面用于解开
		step := mustSealer(t, []config.MasterKey{keys[i], keys[0]})
		if sealed, err = step.Rewrap(sealed); err != nil {
			t.Fatalf("补第 %d 份副本出错: %v", i, err)
		}
	}

	ids, err := WrappedWith(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != maxWraps {
		t.Fatalf("副本份数为 %d，期望 %d", len(ids), maxWraps)
	}

	// 再加一把全新钥匙，应当被拒绝而不是写出一份读不回来的密文
	extra := mustSealer(t, []config.MasterKey{key("extra", 0x99)})
	if _, err := extra.Rewrap(sealed); err == nil {
		t.Error("超过副本上限时应报错")
	}
}
