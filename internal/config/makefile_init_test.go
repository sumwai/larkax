package config

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Makefile 的 init 在生成 .env 之前先探 openssl，生成之后又复查主密钥真的
// 落进去了。两件事都只发生在「.env 不存在」的那条路上，而它们坏掉时的表现
// 是**静默**的：没有 openssl 时 `$$(openssl rand -base64 32)` 展开成空串，
// 命令替换失败并不会让 sed 那一行失败——于是 .env 照样被写出来、主密钥是空的、
// 末尾还打印「已生成」。而 .env 一旦存在就不再重建，那份空的会一直留着，
// 报错点（服务说「主密钥无效」）离真实原因隔了一层。
//
// 这条用例真的跑一次 `make init`：在临时目录里放一份 .env.example 的拷贝，
// 先用一份把 openssl 排除在外的 PATH 跑，再用本机的 PATH 跑。前者钉「缺工具时
// 在生成之前停下、且不留下 .env」，后者钉「生成出来的主密钥真是一把钥匙」——
// 后者同时钉住 .env.example 的占位符拼写与 Makefile 里那个变量是同一串：
// 两处改一处不改另一处时替换不会命中，而那时的默认表现正是产出一份坏的 .env。
//
// 仓库里的 .env 不会被碰：`make -C <临时目录>` 让整件事在那边发生。
//
// **拦不住什么**：Makefile 其余目标（up / migrate / run / test）不在这条用例的
// 范围内，它们各自还有别的现场；`.env` 已存在时那条「只同步 DSN」的分支也不在
// 这里（它要的是换端口之后的行为，见 makefile_ports_test.go 与 docs/development.md）。
func TestMakeInitGeneratesEnvOrSaysWhyNot(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("本机没有 make，跳过：这条用例跑的就是 Makefile 的 init 目标本身")
	}
	// 用例的工作目录是包目录，Makefile 与 .env.example 在仓库根。
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("算仓库根失败: %v", err)
	}
	makefile := filepath.Join(repoRoot, "Makefile")

	t.Run("没有 openssl 时在生成之前停下", func(t *testing.T) {
		dir := envExampleCopy(t, repoRoot)
		out, ok := runMakeInit(t, makeBin, makefile, dir, binDirWithoutOpenssl(t))

		if ok {
			t.Errorf("没有 openssl 时 make init 应当失败，实际成功了；输出:\n%s", out)
		}
		if !strings.Contains(out, "openssl") {
			t.Errorf("报错要点名缺的是 openssl（否则看到的人得从 sed 的报错去倒推）；输出:\n%s", out)
		}
		// 最关键的一条：不留下那份文件。它一旦存在就不再重建，而里面是空密钥
		// ——「静默产出空值」在这条路上是**永久**的。
		envPath := filepath.Join(dir, ".env")
		switch content, err := os.ReadFile(envPath); {
		case err == nil:
			t.Errorf("没有 openssl 时不该写出 .env（写出来之后就不再重建），实际内容:\n%s", content)
		case !errors.Is(err, os.ErrNotExist):
			t.Fatalf("看不了临时目录里的 .env: %v", err)
		}
	})

	t.Run("有 openssl 时写出可用的主密钥", func(t *testing.T) {
		if _, err := exec.LookPath("openssl"); err != nil {
			t.Skip("本机没有 openssl，跳过生成路径（它本该在 README 的「需要什么」那一节里装着）")
		}
		dir := envExampleCopy(t, repoRoot)
		out, ok := runMakeInit(t, makeBin, makefile, dir, os.Getenv("PATH"))
		if !ok {
			t.Fatalf("有 openssl 时 make init 应当成功；输出:\n%s", out)
		}
		raw, err := os.ReadFile(filepath.Join(dir, ".env"))
		if err != nil {
			t.Fatalf("init 成功了却没有 .env: %v", err)
		}
		content := string(raw)

		// 占位符没被换掉，就是那份「看起来生成了、其实没有」的 .env。
		for _, ph := range []string{"<MASTER_KEY>", "<DB_PASSWORD>"} {
			if strings.Contains(content, ph) {
				t.Errorf("生成出来的 .env 里还留着占位符 %s:\n%s", ph, content)
			}
		}

		// 契约是「base64 的 32 字节」，见 docs/development.md 的「主密钥」。
		keys := envValue(t, content, "LARKAX_MASTER_KEYS")
		decoded, err := base64.StdEncoding.DecodeString(keys)
		if err != nil {
			t.Fatalf("LARKAX_MASTER_KEYS = %q，不是 base64: %v", keys, err)
		}
		if len(decoded) != 32 {
			t.Errorf("主密钥解出来是 %d 字节，期望 32（那才是 openssl rand -base64 32 的输出）", len(decoded))
		}

		// 第三条替换（DSN 里的端口与库名）同样要落到：它与 .env.example 的默认
		// 值绑定，是这两份文件之间的另一处耦合。
		if dsn := envValue(t, content, "LARKAX_DB_DSN"); !strings.Contains(dsn, ":5432/larkax") {
			t.Errorf("LARKAX_DB_DSN = %q，示例 DSN 的默认值（:5432/larkax）没被换掉", dsn)
		}
	})
}

// envExampleCopy 造一个只放 .env.example 拷贝的临时目录，返回它的路径。
//
// init 的两条路都按**相对路径**读 $(TEMPLATE)、写 $(ENV_FILE)，所以临时目录里
// 有那一份就够，仓库本身一个字节都不动。
func envExampleCopy(t *testing.T, repoRoot string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".env.example"))
	if err != nil {
		t.Fatalf("读不到 .env.example: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), raw, 0o644); err != nil {
		t.Fatalf("写临时 .env.example 失败: %v", err)
	}
	return dir
}

// runMakeInit 在临时目录里跑一次 `make -C <dir> -f <Makefile> init`。
//
// 用 t.Context()：用例结束时那个上下文被取消，跑飞的 make 会跟着被中止，
// 而不是把 `go test` 挂在那里等它。
func runMakeInit(t *testing.T, makeBin, makefile, dir, path string) (string, bool) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), makeBin, "-C", dir, "-f", makefile, "init")
	cmd.Env = envWithPath(path)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// envWithPath 把进程环境里的 PATH 换掉。
//
// **不能直接 append**：同一个变量名出现两次时子进程取哪一个是未定义的
// （glibc 的 getenv 取第一个），而 os.Environ() 里已经有一条 PATH——
// 那样写出来的用例会拿真实 PATH 跑，于是「没有 openssl」这一格永远绿。
func envWithPath(path string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "PATH="+path)
}

// binDirWithoutOpenssl 造一个**确定**没有 openssl 的 PATH。
//
// 不靠改写 PATH 去把它藏起来：它在哪取决于发行版与包管理器（/usr/bin、
// /usr/local/bin 都可能）。这里反过来只放 init 的配方真正会用到的外部命令
// ——sed / grep / rm / mv（echo、printf、command、[ 都是 shell 内建），
// 于是这份 PATH 里必然没有 openssl，与它装在哪儿无关。
func binDirWithoutOpenssl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"sed", "grep", "rm", "mv"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("本机没有 %s，这份 PATH 造不出来: %v", name, err)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatalf("建 %s 的软链失败: %v", name, err)
		}
	}
	return dir
}

// envValue 取出 .env 文本里一个键的值（去掉两侧的引号）。
func envValue(t *testing.T, content, key string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if rest, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(rest, `"`)
		}
	}
	t.Fatalf(".env 里找不到 %s:\n%s", key, content)
	return ""
}
