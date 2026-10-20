package auth

import (
	"strings"
	"testing"
)

// 测试用的 KDF 参数。
//
// 与生产默认值（60 万轮）只差轮数：盐与输出长度取同一组，于是「串里写了哪些
// 参数」「重哈希的判据」「解析的上限」这几条在测试里走的是与生产相同的代码。
// 用 1000 轮是为了让几十条用例跑得完——而**重哈希必须也用这一组**，否则一条
// i=1000 的串在第一次验证之后会被升级成 60 万轮（见 consoleapi.Config）。
var testKDF = KDFParams{Iterations: 1000, SaltBytes: 16, KeyBytes: 32}

func TestHashPasswordFormat(t *testing.T) {
	hash, err := HashPassword(testKDF, "correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword 报错: %v", err)
	}
	if !strings.HasPrefix(hash, passwordHashScheme) {
		t.Fatalf("哈希串 %q 没有方案前缀 %q", hash, passwordHashScheme)
	}
	parts := strings.Split(strings.TrimPrefix(hash, passwordHashScheme), "$")
	if len(parts) != 3 {
		t.Fatalf("哈希串应当分三段（参数、盐、值），得到 %d 段: %q", len(parts), hash)
	}
	if parts[0] != "i=1000,l=32" {
		t.Errorf("参数段 = %q，期望 i=1000,l=32", parts[0])
	}
	// 口令本身不能出现在串里——它是哈希，不是编码。
	if strings.Contains(hash, "correct") {
		t.Fatalf("哈希串里出现了明文口令: %q", hash)
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	first, err := HashPassword(testKDF, "correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword 报错: %v", err)
	}
	second, err := HashPassword(testKDF, "correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword 报错: %v", err)
	}
	if first == second {
		t.Fatal("同一个口令两次哈希得到了同一个串：盐没有起作用")
	}
}

func TestVerifyPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"
	hash, err := HashPassword(testKDF, password)
	if err != nil {
		t.Fatalf("HashPassword 报错: %v", err)
	}

	result, err := VerifyPassword(testKDF, hash, password)
	if err != nil {
		t.Fatalf("VerifyPassword 报错: %v", err)
	}
	if !result.OK {
		t.Fatal("正确的口令没有被接受")
	}
	if result.NeedsRehash {
		t.Fatal("按当前参数生成的串被判成需要重哈希")
	}

	wrong, err := VerifyPassword(testKDF, hash, password+"x")
	if err != nil {
		t.Fatalf("VerifyPassword 报错: %v", err)
	}
	if wrong.OK {
		t.Fatal("错误的口令被接受了")
	}
}

// TestVerifyPasswordNeedsRehash 钉住「参数不同就要重算」这条判据。
//
// 两种差异都验：轮数变了（调参）与输出长度变了（换参数集）。盐不在判据里
// ——它是每次随机生成的，拿它比会让每一条口令都立刻需要重哈希。
func TestVerifyPasswordNeedsRehash(t *testing.T) {
	const password = "correct horse battery staple"

	oldParams := KDFParams{Iterations: 1000, SaltBytes: 16, KeyBytes: 32}
	hash, err := HashPassword(oldParams, password)
	if err != nil {
		t.Fatalf("HashPassword 报错: %v", err)
	}

	cases := map[string]KDFParams{
		"轮数变多": {Iterations: 2000, SaltBytes: 16, KeyBytes: 32},
		"输出变长": {Iterations: 1000, SaltBytes: 16, KeyBytes: 48},
		"都不变":  oldParams,
	}
	for name, current := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := VerifyPassword(current, hash, password)
			if err != nil {
				t.Fatalf("VerifyPassword 报错: %v", err)
			}
			if !result.OK {
				t.Fatal("正确的口令没有被接受")
			}
			wantRehash := current != oldParams
			if result.NeedsRehash != wantRehash {
				t.Errorf("NeedsRehash = %v，期望 %v", result.NeedsRehash, wantRehash)
			}
		})
	}
}

// TestVerifyPasswordRejectsMalformed 钉住解析侧的拒绝，而不是「尽力而为」。
//
// 每一条都是库里可能出现（或被人写进去）的坏串。判据是「报错」，不是
// 「当成口令不对」：前者能让运维去看数据，后者让一次数据问题看起来像
// 用户记错了口令。
func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	const password = "correct horse battery staple"

	bad := map[string]string{
		"空串":         "",
		"别的方案":       "$argon2id$i=1000,l=32$c2FsdA$aGFzaA",
		"段数不对":       "$pbkdf2-sha256$i=1000,l=32$c2FsdA",
		"缺轮数":        "$pbkdf2-sha256$l=32$c2FsdA$aGFzaA",
		"缺输出长度":      "$pbkdf2-sha256$i=1000$c2FsdA$aGFzaA",
		"未知参数":       "$pbkdf2-sha256$i=1000,l=32,x=1$c2FsdA$aGFzaA",
		"非整数参数":      "$pbkdf2-sha256$i=many,l=32$c2FsdA$aGFzaA",
		"轮数为零":       "$pbkdf2-sha256$i=0,l=32$c2FsdA$aGFzaA",
		"轮数超上限":      "$pbkdf2-sha256$i=10000001,l=32$c2FsdA$aGFzaA",
		"输出过短":       "$pbkdf2-sha256$i=1000,l=8$c2FsdA$aGFzaA",
		"盐不是 base64": "$pbkdf2-sha256$i=1000,l=32$!!!!$aGFzaA",
		"盐为空":        "$pbkdf2-sha256$i=1000,l=32$$aGFzaA",
		"值长度不符":      "$pbkdf2-sha256$i=1000,l=32$c2FsdA$aGFzaA",
	}
	for name, stored := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyPassword(testKDF, stored, password); err == nil {
				t.Fatalf("坏串 %q 没有报错", stored)
			}
		})
	}
}

// TestValidatePassword 钉住口令策略的三条判据。
func TestValidatePassword(t *testing.T) {
	const username = "jdoe"
	const ok = "correct horse battery staple"

	if err := ValidatePassword(username, ok); err != nil {
		t.Fatalf("合规口令被拒: %v", err)
	}
	// 中文按 rune 数：12 个汉字是合规的，哪怕它有 36 字节。
	if err := ValidatePassword(username, strings.Repeat("口", PasswordMinRunes)); err != nil {
		t.Fatalf("12 个汉字的口令被拒: %v", err)
	}

	tooShort := strings.Repeat("a", PasswordMinRunes-1)
	tooLong := strings.Repeat("a", PasswordMaxRunes+1)
	bad := map[string]string{
		"太短":         tooShort,
		"太长":         tooLong,
		"等于用户名":      username,
		"大小写不同也算同一个": strings.ToUpper(username),
		"含换行":        "correct horse\nbattery",
		"含制表符":       "correct horse\tbattery",
		"含 DEL":      "correct horse\x7fbattery",
	}
	for name, password := range bad {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePassword(username, password); err == nil {
				t.Fatalf("不合规口令 %q 没有被拒", password)
			}
		})
	}
}
