package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Makefile 的 check-ports 在变量没设时会拿一份兜底值去判断端口占用，而这两个
// 默认值的唯一来源是 Load。两份抄写之间原本没有任何门：只改一边的表现是预检
// 与真实监听地址不一致，报出来的方向是错的——说「端口空着」而服务起不来，
// 或者说「被占了」而其实没占。
//
// 这条用例把两份值钉在一起。Makefile 那边**抽不到兜底值同样算失败**：抽不到
// 就跳过，与「比对过了、两边一致」在输出里长得一样（见 LESSONS.md 第 16 条）。
func TestMakefileCheckPortsFallbackMatchesDefaults(t *testing.T) {
	// env() 走的是 os.Getenv，空串与未设置同义；Makefile 的 ${VAR:-x} 也在
	// 空串时取 x。两侧语义一致，所以「设成空串」就是这里要的「没设」。
	setEnv(t, baseEnv(nil))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() 出错: %v", err)
	}

	httpAddr, internalAddr := makefileFallbackPorts(t)
	if httpAddr != cfg.HTTPAddr {
		t.Errorf("Makefile 里 LARKAX_HTTP_ADDR 的兜底值 = %q，config 的默认值 = %q；两处要一起改", httpAddr, cfg.HTTPAddr)
	}
	if internalAddr != cfg.InternalAddr {
		t.Errorf("Makefile 里 LARKAX_INTERNAL_ADDR 的兜底值 = %q，config 的默认值 = %q；两处要一起改", internalAddr, cfg.InternalAddr)
	}
}

// makefileFallbackPorts 读出 Makefile 里 check-ports 两个监听地址的兜底值。
func makefileFallbackPorts(t *testing.T) (httpAddr, internalAddr string) {
	t.Helper()

	// 用例的工作目录是包目录，Makefile 在仓库根。
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("读不到仓库根的 Makefile: %v", err)
	}

	recipe, ok := checkPortsRecipe(string(raw))
	if !ok {
		t.Fatal("Makefile 里找不到 check-ports 目标，或者它的配方是空的")
	}

	return fallbackOf(t, recipe, "LARKAX_HTTP_ADDR"), fallbackOf(t, recipe, "LARKAX_INTERNAL_ADDR")
}

// checkPortsRecipe 取出 check-ports 目标的配方行。配方行一律以制表符开头，
// 遇到第一个不以制表符开头的行就是目标结束。
//
// 只看这段而不是整个文件：同一个变量名在别处（例如给用户的示例命令里）
// 出现时，不该被当成 check-ports 的兜底值。
func checkPortsRecipe(makefile string) (string, bool) {
	lines := strings.Split(makefile, "\n")

	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "check-ports:") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}

	var b strings.Builder
	for _, line := range lines[start:] {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// fallbackOf 抽出 ${NAME:-<兜底值>} 里的兜底值。
//
// 命中数必须恰好是 1：0 处说明 Makefile 换了写法，多处则不知道哪一处算数。
// 两种都失败，否则用例会在没真正比对的情况下通过。
func fallbackOf(t *testing.T, recipe, name string) string {
	t.Helper()

	re := regexp.MustCompile(`\$\{` + regexp.QuoteMeta(name) + `:-([^}]+)\}`)
	matches := re.FindAllStringSubmatch(recipe, -1)
	if len(matches) != 1 {
		t.Fatalf("Makefile 的 check-ports 里找不到 %s 的兜底值（命中 %d 处，期望 1 处）", name, len(matches))
	}
	return matches[0][1]
}
