package consoleapi

import (
	"net"
	"testing"
	"time"
)

// 本文件钉住登录节流的令牌桶：连续尝试的额度、按时间补充、成功登录后的
// 清零。时间由用例给（allow/reset 都收 now），不读真实时钟。

func TestIPLimiterAllowsBurstThenWaits(t *testing.T) {
	l := newIPLimiter()
	ip := net.ParseIP("203.0.113.5")
	at := testNow

	for i := 0; i < loginBurst; i++ {
		if wait, ok := l.allow(ip, at); !ok {
			t.Fatalf("第 %d 次尝试就被拦了（还要等 %s）", i+1, wait)
		}
	}

	// 额度用完：拦下，并给出补满一个令牌要等多久。
	wait, ok := l.allow(ip, at)
	if ok {
		t.Fatal("额度用完之后仍然放行")
	}
	if wait != loginRefill {
		t.Errorf("建议等待 %s，期望 %s", wait, loginRefill)
	}

	// 时间不够：仍然拦。
	if _, ok := l.allow(ip, at.Add(loginRefill-time.Second)); ok {
		t.Fatal("还没补满一个令牌就放行了")
	}

	// 补满一个令牌：放行一次，之后又拦。
	if _, ok := l.allow(ip, at.Add(loginRefill)); !ok {
		t.Fatal("补满一个令牌后应当放行")
	}
	if _, ok := l.allow(ip, at.Add(loginRefill)); ok {
		t.Fatal("补出来的那一个令牌用了两次")
	}
}

func TestIPLimiterRefillCapsAtBurst(t *testing.T) {
	l := newIPLimiter()
	ip := net.ParseIP("203.0.113.6")
	at := testNow

	for i := 0; i < loginBurst; i++ {
		l.allow(ip, at)
	}
	// 放很久：桶只补到上限，不会攒出「离线一小时能连打几百次」的额度。
	later := at.Add(24 * time.Hour)
	for i := 0; i < loginBurst; i++ {
		if _, ok := l.allow(ip, later); !ok {
			t.Fatalf("补满之后第 %d 次应当放行", i+1)
		}
	}
	if _, ok := l.allow(ip, later); ok {
		t.Fatalf("补满之后放行了 %d 次以上", loginBurst)
	}
}

func TestIPLimiterResetClearsTheBucket(t *testing.T) {
	l := newIPLimiter()
	ip := net.ParseIP("203.0.113.7")
	at := testNow

	for i := 0; i < loginBurst; i++ {
		l.allow(ip, at)
	}
	if _, ok := l.allow(ip, at); ok {
		t.Fatal("额度用完之后仍然放行")
	}

	// 登录成功：同一个来源重新拿到整份额度。
	l.reset(ip)
	for i := 0; i < loginBurst; i++ {
		if _, ok := l.allow(ip, at); !ok {
			t.Fatalf("reset 之后第 %d 次应当放行", i+1)
		}
	}
}

func TestIPLimiterSeparatesSources(t *testing.T) {
	l := newIPLimiter()
	at := testNow
	exhausted := net.ParseIP("203.0.113.8")
	other := net.ParseIP("203.0.113.9")
	// IPv4-mapped IPv6 与 IPv4 是同一个来源：换个写法不该多出额度。
	mapped := net.ParseIP("::ffff:203.0.113.8")

	for i := 0; i < loginBurst; i++ {
		l.allow(exhausted, at)
	}
	if _, ok := l.allow(mapped, at); ok {
		t.Fatal("IPv4-mapped 形式被当成了另一个来源")
	}
	if _, ok := l.allow(other, at); !ok {
		t.Fatal("另一个来源被连坐")
	}
}

func TestIPLimiterLetsUnknownSourceThrough(t *testing.T) {
	l := newIPLimiter()
	for i := 0; i < loginBurst*2; i++ {
		if _, ok := l.allow(nil, testNow); !ok {
			t.Fatalf("来源解析不出来时第 %d 次被拦了", i+1)
		}
	}
}
