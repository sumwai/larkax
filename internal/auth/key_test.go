package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestHashKeyIsSHA256Hex 钉住哈希**口径本身**。
//
// 期望值是一个字面量，不是「再算一遍 sha256 比一比」：那样写等于用实现
// 验证实现，换了算法照样通过。这里的用例要求「SHA-256、十六进制、小写」，
// 所以改算法（哪怕改成同样安全的 SHA-512）必须让这条测试失败——
// 因为那会让库里已有的哈希全部失效。
func TestHashKeyIsSHA256Hex(t *testing.T) {
	// SHA-256("abc") 的标准测试向量。
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	if got := HashKey("abc"); got != want {
		t.Fatalf("HashKey(\"abc\") = %q，期望 %q", got, want)
	}

	// 长度与大小写也一并钉住：十六进制的大小写与长度都影响「同一把 key
	// 能不能查得到」，而它们不影响上面那条向量的可比性——
	// 换一种编码（比如 base64）时长度先变。
	got := HashKey("test-key-placeholder")
	if len(got) != 64 {
		t.Errorf("哈希长度 = %d，期望 64", len(got))
	}
	if got != strings.ToLower(got) {
		t.Errorf("哈希不是小写: %q", got)
	}
}

func TestHashKeyIsDeterministic(t *testing.T) {
	const key = "test-key-placeholder"

	// 第二次调用经过一个独立的变量。写成 `HashKey(key) != HashKey(key)`
	// 会被 staticcheck 判成「左右是同一个表达式，恒为假」（SA4000），
	// 而这条用例要的正是「同一个输入、两次独立调用得到同一个值」——
	// 所以要让两次调用在语法上真的不同，而不是把断言删掉。
	same := key
	if HashKey(key) != HashKey(same) {
		t.Fatal("同一个输入两次哈希结果不同")
	}
	if HashKey(key) == HashKey(key+"x") {
		t.Fatal("不同输入得到同一个哈希")
	}
}

func TestNewKeyFormat(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey 失败: %v", err)
	}

	if !strings.HasPrefix(key, keyScheme) {
		t.Fatalf("key %q 没有方案前缀 %q", key, keyScheme)
	}

	body := strings.TrimPrefix(key, keyScheme)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("key 的随机部分不是无填充 base64url: %v", err)
	}
	if len(raw) != keyRandomBytes {
		t.Fatalf("随机部分解码后 %d 字节，期望 %d", len(raw), keyRandomBytes)
	}
	// 补上填充也能解码，说明它确实是标准 base64url 而不是某种自定义编码；
	// 而 key 本身不该带 `=`——带填充的串在 URL 与 shell 里更容易被截断。
	if strings.ContainsAny(key, "=+/") {
		t.Fatalf("key %q 含 base64 填充或非 URL 安全字符", key)
	}
}

// TestNewKeyIsUnique 是随机性唯一的可测形式。
//
// 它抓不住「熵不足」这种整体性问题（真正的判据是它来自 crypto/rand），
// 但能抓住「随机部分被写死」或「长度退化」这类把生成器改坏的改动。
func TestNewKeyIsUnique(t *testing.T) {
	const n = 2000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		key, err := NewKey()
		if err != nil {
			t.Fatalf("NewKey 失败: %v", err)
		}
		if _, dup := seen[key]; dup {
			t.Fatalf("生成 %d 把 key 时出现重复", n)
		}
		seen[key] = struct{}{}
	}
}

func TestKeyPrefix(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey 失败: %v", err)
	}

	prefix := KeyPrefix(key)
	if len(prefix) != keyPrefixLen {
		t.Fatalf("前缀长度 = %d，期望 %d", len(prefix), keyPrefixLen)
	}
	if !strings.HasPrefix(key, prefix) {
		t.Fatalf("前缀 %q 不是 %q 的开头", prefix, key)
	}
	// 前缀必须能自证是哪种 key：只看前缀的人要能认出它来自本系统。
	if !strings.HasPrefix(prefix, keyScheme) {
		t.Errorf("前缀 %q 不含方案前缀", prefix)
	}

	// 比前缀还短的输入原样返回，而不是越界或补零。
	if got := KeyPrefix("short"); got != "short" {
		t.Errorf("短输入 KeyPrefix = %q，期望原样返回", got)
	}
}
