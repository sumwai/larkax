package expiry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestForever 钉住「永久」的唯一取值。
//
// 字面量写死在这里是故意的：拿 ForeverRFC3339 自己断言自己，常量写错了
// 这条用例照样绿。本文件也是全仓唯一允许出现这个字面量的测试文件。
func TestForever(t *testing.T) {
	if got := Forever().Format(time.RFC3339); got != "9999-12-31T23:59:59Z" {
		t.Errorf("Forever() = %s，期望 %s", got, ForeverRFC3339)
	}
	if Forever().Location() != time.UTC {
		t.Errorf("Forever() 的时区是 %v，期望 UTC", Forever().Location())
	}
}

// TestIsForever 钉住「不过期」的判据：NULL 与远期值是同一件事。
func TestIsForever(t *testing.T) {
	exact := Forever()
	before := exact.Add(-time.Second)
	after := exact.Add(time.Second)
	shanghai := time.FixedZone("UTC+8", 8*60*60)
	sameInstantElsewhere := exact.In(shanghai)
	zero := time.Time{}

	cases := []struct {
		name string
		at   *time.Time
		want bool
	}{
		{"nil 表示不过期", nil, true},
		{"恰好是远期值", &exact, true},
		{"同一时刻换个时区", &sameInstantElsewhere, true},
		{"远期值前一秒", &before, false},
		{"远期值后一秒", &after, false},
		{"零值不是远期值", &zero, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsForever(tc.at); got != tc.want {
				t.Errorf("IsForever(%v) = %v，期望 %v", tc.at, got, tc.want)
			}
		})
	}
}

// TestForeverLiteralDefinedOnce 钉住「远期上限只在一处定义」。
//
// 帮助文本、控制台或别处各自把远期值抄一遍时，改常量只改到其中一处，
// 两个名字从那天起各说各的时刻——而没有任何东西会报错。所以扫全仓 .go：
// 非测试源码里这个值只能出现在 expiry.go 的常量定义里；测试里只能出现在
// 本文件 TestForever 的那一条断言里。
//
// 本文件里那条例外不是「测试里可以随便抄」的口子：断言的那个字面量正是
// 用来发现常量被改错的钉子，其余地方（包括别的测试）一律要走常量。
func TestForeverLiteralDefinedOnce(t *testing.T) {
	// 不把这个值直接写在这里：那样扫描器自己就成了第二处。
	needle := strings.Split(ForeverRFC3339, "T")[0]
	const owner = "internal/expiry/expiry.go"
	const testException = "internal/expiry/expiry_test.go"

	// time.Date(9999, 12, 31, …) 绕开了常量，同样要红。
	dateCall := regexp.MustCompile(`time\.Date\(\s*9999\s*,\s*12\s*,\s*31`)

	var (
		productionTotal int
		productionSites []string
		testTotal       int
		testSites       []string
		cliHits         []string
		dateSites       []string
	)

	root := "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if strings.HasSuffix(d.Name(), "_test.go") {
			if n := strings.Count(string(src), needle); n > 0 {
				testTotal += n
				testSites = append(testSites, fmt.Sprintf("%s×%d", rel, n))
			}
			return nil
		}

		if n := strings.Count(string(src), needle); n > 0 {
			productionTotal += n
			productionSites = append(productionSites, fmt.Sprintf("%s×%d", rel, n))
			if strings.HasPrefix(rel, "cmd/larkaxctl/") {
				cliHits = append(cliHits, rel)
			}
		}
		if dateCall.Match(src) {
			dateSites = append(dateSites, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描仓库失败: %v", err)
	}

	t.Run("非测试源码只有一处定义", func(t *testing.T) {
		if productionTotal != 1 || len(productionSites) != 1 ||
			productionSites[0] != owner+"×1" {
			t.Errorf("字面量 %q 在非测试源码里出现 %d 次（%v），期望恰好 1 次且在 %s 的 ForeverRFC3339",
				needle, productionTotal, productionSites, owner)
		}
	})

	t.Run("CLI 不抄这个值", func(t *testing.T) {
		if len(cliHits) > 0 {
			t.Errorf("cmd/larkaxctl 的非测试源码里出现了字面量 %q（%v）：别把值抄回 CLI，引用 expiry.ForeverRFC3339",
				needle, cliHits)
		}
	})

	t.Run("测试里只有断言那一行", func(t *testing.T) {
		if testTotal != 1 || len(testSites) != 1 ||
			testSites[0] != testException+"×1" {
			t.Errorf("字面量 %q 在测试文件里出现 %d 次（%v），期望只有 %s 的 TestForever 那一条断言",
				needle, testTotal, testSites, testException)
		}
	})

	t.Run("不用 time.Date 绕开常量", func(t *testing.T) {
		if len(dateSites) > 0 {
			t.Errorf("非测试源码用 time.Date(9999, 12, 31, …) 绕开了常量（%v），请改走 expiry.Forever()",
				dateSites)
		}
	})
}
