package expiry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// foreverNeedle 是扫 .go 那条用例用的日期前缀。它**手写、不跟常量走**：
// 改了常量、旧拷贝还留在别处，正是它要抓的场景——从常量取 needle，扫描
// 范围会跟着常量一起挪走，旧拷贝反而落在范围外，那条扫描就悄悄失效了。
//
// 别顺手把它改成派生：docs 那条扫描（TestForeverLiteralInDocs）用的是
// 从常量派生出来的 needle，两条形态相反是有意的，理由各自写在定义处。
//
// 拼接写法让扫描器自己不算第二处。
const foreverNeedle = "9999-" + "12-31"

// TestForever 钉住「永久」的唯一取值。
//
// 字面量写死在这里是故意的：拿 ForeverRFC3339 自己断言自己，常量写错了
// 这条用例照样绿。本文件也是 Go 侧唯一允许出现这个字面量的测试文件。
func TestForever(t *testing.T) {
	// 断言与失败信息用同一个字面量：常量写错了，报出来的期望值才不会
	// 跟着常量一起错，读的人一眼能看出两边不一致。
	const want = "9999-12-31T23:59:59Z"
	if got := Forever().Format(time.RFC3339); got != want {
		t.Errorf("Forever() = %s，期望 %s", got, want)
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
// 本文件 TestForever 的那个期望值里。
//
// 本文件里那条例外不是「测试里可以随便抄」的口子：那个期望值正是用来发现
// 常量被改错的钉子，其余地方（包括别的测试）一律要走常量。
func TestForeverLiteralDefinedOnce(t *testing.T) {
	needle := foreverNeedle
	if !strings.HasPrefix(ForeverRFC3339, needle) {
		t.Fatalf("ForeverRFC3339 = %q 不再以 %q 开头：换年份时这条扫描要跟着改", ForeverRFC3339, needle)
	}
	const owner = "internal/expiry/expiry.go"
	const testException = "internal/expiry/expiry_test.go"

	// time.Date(9999, 12, 31, …) 与 time.Date(9999, time.December, 31, …) 都
	// 绕开了常量，同样要红。只认这两种参数写法：time.Unix 换算出来的时刻、
	// 或者先解析再加减出来的值，它挡不住，那类写法靠审查。
	dateCall := regexp.MustCompile(`time\.Date\(\s*9999\s*,\s*(12|time\.December)\s*,\s*31`)

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

	t.Run("测试里只有期望值那一处", func(t *testing.T) {
		if testTotal != 1 || len(testSites) != 1 ||
			testSites[0] != testException+"×1" {
			t.Errorf("字面量 %q 在测试文件里出现 %d 次（%v），期望只有 %s 的 TestForever 那个期望值",
				needle, testTotal, testSites, testException)
		}
	})

	t.Run("不用 time.Date 绕开常量", func(t *testing.T) {
		if len(dateSites) > 0 {
			t.Errorf("非测试源码用 time.Date(9999, 12 或 time.December, 31, …) 绕开了常量（%v），请改走 expiry.Forever()",
				dateSites)
		}
	})
}

// docsScanFloor 是 docs/ 下至少要扫到的 .md 文件数。
//
// 它抓的是「目录找错、仓库根推错」，不是断言文档条数：现在 docs/ 下不到
// 二十个 .md，取一个明显低于当前值的数——合并几篇文档不该让它红，一个
// 文件都没扫到则必须红。
const docsScanFloor = 10

// foreverYearFloor 是远期值年份的量级下限。
//
// docs 里正常的示例时间都在 2031 以内；四位数年份到了 9000，只可能是给人
// 看的远期值。年份扫描按「≥ 这个下限，或不早于常量自己的年份」取候选——
// 后半句让常量换到 9000 年以内时，扫描自动退化成「常量那一年及以后」，不必
// 靠一条要人手工解除的红灯，也还看得见往相邻年份的漂移。
const foreverYearFloor = 9000

// isoDate 匹配 YYYY-MM-DD 形态的日期。前缀 \b 防止从更长的数字中间起
// 匹配；年份是否算远期值，在调用处按 foreverYearFloor 与常量年份判断。
var isoDate = regexp.MustCompile(`\b([0-9]{4})-[0-9]{2}-[0-9]{2}`)

// TestForeverLiteralInDocs 检查文档里印给用户的远期值与常量逐字一致。
//
// TestForeverLiteralDefinedOnce 只扫 .go；读者照着 docs/ 里的字面量去比对
// 响应时，常量一改、文档继续印旧值，两边就各说各的时刻，而没有任何东西会
// 报错。这里扫 docs/ 下的 .md，判据分成两条、互不遮蔽：
//
//   - 处数下限：两个关键文件里远期值日期前缀的出现次数不低于当前值；
//   - 年份扫描：每一处远期值日期的后面必须是 ForeverRFC3339 原文，只有
//     日期没有时刻也算不一致。
//
// 分开是有原因的：处数被别处凑够时，藏着错误年份的那一行仍要被年份扫描
// 单独点出来。这里不设白名单——某处文档确实该写旧值时，改成引用常量或
// 另想办法。
func TestForeverLiteralInDocs(t *testing.T) {
	// 仓库根不从 os.Getwd() 或 "../.." 推：go test 的工作目录是包目录，
	// 相对路径在本机与 CI 碰巧能过，包一旦被移动或换个目录跑就静默扫不到
	// 文件——而「扫不到」与「全都对」在输出上一样。本测试文件在
	// internal/expiry/ 下，上溯三层就是仓库根。
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("拿不到本测试文件的路径，无法定位仓库根")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	docsDir := filepath.Join(root, "docs")

	// docs 这一侧的 needle **从常量派生，别把它改成手写**：年份扫描（年份
	// ≥9000 或不早于常量年份）负责兜住旧拷贝，所以这里可以从常量派生；手写
	// 反而会在常量换年份后要么过期、要么把范围钉在老年份上。扫 .go 那侧的
	// foreverNeedle 手写，理由与这里正好相反。
	if len(ForeverRFC3339) < 10 {
		t.Fatalf("ForeverRFC3339 = %q 短于 10 个字节，取不出日期前缀", ForeverRFC3339)
	}
	docsNeedle := ForeverRFC3339[:10]

	var foreverYear int
	if len(ForeverRFC3339) >= 4 {
		foreverYear, _ = strconv.Atoi(ForeverRFC3339[:4])
	}

	var (
		files      int
		seen       = map[string]bool{}
		needleHits = map[string]int{}
		yearHits   = map[string]int{}
		mismatches []string
	)
	err := filepath.WalkDir(docsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
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
		files++
		seen[rel] = true
		needleHits[rel] = strings.Count(string(src), docsNeedle)

		for _, m := range isoDate.FindAllSubmatchIndex(src, -1) {
			// 远期值的候选：年份到了量级下限，或不早于常量自己的年份。前者
			// 抓住任意年份的远期值；后者让常量换到 9000 年以内时，扫描自动
			// 退化成「常量那一年及以后」——写成「等于常量年份」就看不见往
			// 相邻年份的漂移，而那正是要给出行号的那一类。
			at := m[0]
			year, convErr := strconv.Atoi(string(src[m[2]:m[3]]))
			if convErr != nil || (year < foreverYearFloor && year < foreverYear) {
				continue
			}
			yearHits[rel]++

			// 从这一处起按常量的长度取一串与常量逐字比：只认日期前缀会漏掉
			// 只有日期、没有时刻的写法；剩余长度不足时按不匹配报出，不 panic。
			end := at + len(ForeverRFC3339)
			if end > len(src) {
				end = len(src)
			}
			got := string(src[at:end])
			if got == ForeverRFC3339 {
				continue
			}
			line, col := lineCol(src, at)
			mismatches = append(mismatches, fmt.Sprintf("%s:%d:%d: 这一行是远期值，应当与常量逐字相等"+
				"（取到 %q，常量 %q）；要改的是文档或常量，不是这条判据；该行：%s",
				rel, line, col, got, ForeverRFC3339, lineText(src, at)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 docs/ 失败: %v", err)
	}

	// 扫不到文件、扫错目录、needle 与文档形态对不上时，上面那轮比对一处都
	// 不做，结果和「全都对」一模一样。下面三条下限把这种状态变成红的。
	if files < docsScanFloor {
		t.Errorf("docs/ 下只扫到 %d 个 .md（下限 %d）：仓库根或扫描目录不对，先确认 needle 与文档形态是否还对得上",
			files, docsScanFloor)
	}

	// 命中数下限是**断言**，不是白名单：删掉一处文档的 PR 会看见它红；新增一处
	// 不红（这是下限不是等式），但新增的那一处仍要过年份扫描。把数字一起改掉
	// 就是我们要的人为确认。所以文案要说清这是「去改数字」，不是「文档丢了一处」。
	for _, want := range []struct {
		file string
		min  int
	}{
		{"docs/api.md", 2},
		{"docs/cli-reference.md", 3},
	} {
		switch {
		case !seen[want.file]:
			t.Errorf("%s 不存在或没被扫到：仓库根或扫描目录不对，先确认 needle 与文档形态是否还对得上", want.file)
		case needleHits[want.file] < want.min:
			t.Errorf("%s 里这个值现在有 %d 处，这里要求 ≥%d 处。"+
				"这是在要你确认并修改这里的数字，不是发现了缺陷：你刚删掉一处、"+
				"或把某处换成了别的写法时，把 %d 改成当前实际处数即可；"+
				"若你刚改过 ForeverRFC3339 而文档还是旧值，命中的是旧字面量，"+
				"这里的 0 是提示你先改文档；若常量没动、数字却少了，那就是真丢了一处",
				want.file, needleHits[want.file], want.min, want.min)
		}
	}

	for _, m := range mismatches {
		t.Errorf("%s", m)
	}

	// 远期值本身得是一次有效的远期：年份不晚于今年时，它写进库里就是个
	// 已经过期的时刻，docs 里与它同年的示例时间也会被年份扫描当成远期值。
	// 这不是扫描的下限（见 foreverYearFloor），守的是值本身。
	if foreverYear <= time.Now().Year() {
		t.Errorf("ForeverRFC3339 = %q 的年份不晚于今年（%d）：它已经不是一个远期时刻，"+
			"写进库里的「永久」会立刻过期", ForeverRFC3339, time.Now().Year())
	}

	rels := make([]string, 0, len(yearHits))
	for rel := range yearHits {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	counts := make([]string, 0, len(rels))
	for _, rel := range rels {
		counts = append(counts, fmt.Sprintf("%s×%d", rel, yearHits[rel]))
	}
	t.Logf("扫描了 %d 个 docs/*.md，找到 %s", files, strings.Join(counts, "、"))
}

// lineCol 把字节偏移换算成 1 起的行号与列号（列按字节算）。
func lineCol(src []byte, off int) (line, col int) {
	line, col = 1, 1
	for _, b := range src[:off] {
		if b == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}

// lineText 取偏移所在那一行的原文，去掉行尾换行。
func lineText(src []byte, off int) string {
	start := off
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	end := off
	for end < len(src) && src[end] != '\n' {
		end++
	}
	return string(src[start:end])
}
