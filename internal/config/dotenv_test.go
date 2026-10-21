package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// envStub 是「进程环境」的替身：用例用它代替 os.LookupEnv / os.Setenv。
//
// 为什么不用真实的进程环境：用例要验的正是「没设置的键被填上」，而要让某个键
// 「没设置」就得先把它从进程环境里删掉——那会改到别的用例也在用的全局状态，
// 而且真实的 cwd 也只有一个（用 os.Chdir 摆布它会让并行跑的用例互相干扰）。
type envStub struct {
	values map[string]string
	setErr error
}

func newEnvStub(initial map[string]string) *envStub {
	m := make(map[string]string, len(initial))
	for k, v := range initial {
		m[k] = v
	}
	return &envStub{values: m}
}

func (e *envStub) lookup(key string) (string, bool) {
	v, ok := e.values[key]
	return v, ok
}

func (e *envStub) set(key, value string) error {
	if e.setErr != nil {
		return e.setErr
	}
	e.values[key] = value
	return nil
}

// stubOptions 把替身装进一份依赖里，起点是 dir。
func stubOptions(env *envStub, dir string) dotenvOptions {
	return dotenvOptions{dir: dir, lookup: env.lookup, set: env.set}
}

// writeEnvFile 在 dir 下写一份 .env，返回它的路径。
func writeEnvFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, envFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
	return path
}

// makeGitRoot 把 dir 变成 git 仓库根（.git 是目录的形态）。
//
// 本包判断边界只看「有没有 .git」，不必真的初始化一个仓库——那会给测试机
// 加上「装了 git」这个前提。另一种形态（.git 是文件，worktree 与子模块）
// 见 TestGitRootMayBeFile。
func makeGitRoot(t *testing.T, dir string) {
	t.Helper()
	dotGit := filepath.Join(dir, ".git")
	if err := os.Mkdir(dotGit, 0o700); err != nil {
		t.Fatalf("建 %s 失败: %v", dotGit, err)
	}
}

// mkdir 建一个子目录（多级）。
func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建 %s 失败: %v", dir, err)
	}
	return dir
}

// swapProcessEnv 把「进程环境」换成替身，用例结束自动还原。
//
// 替身只管「某个键在不在」这个判断（它不知道运行测试的机器上原本有什么），
// 而写入**同时落到真实的进程环境**里：Load 与 LoadOps 读取变量走的是
// os.Getenv（本包没有把「读」也做成可注入的），只有写进那里它们才看得见
// .env 填上的值——而「两个入口都看得见同一次加载」正是要验的东西。
//
// 落进去的值由 t.Cleanup 还原（键本来没有就删掉），不漏给后面的用例。
func swapProcessEnv(t *testing.T, env *envStub, dir string) {
	t.Helper()

	prev := processEnv
	processEnv = func() dotenvOptions {
		return dotenvOptions{dir: dir, lookup: env.lookup, set: func(key, value string) error {
			if err := env.set(key, value); err != nil {
				return err
			}
			old, had := os.LookupEnv(key)
			t.Cleanup(func() {
				if had {
					_ = os.Setenv(key, old)
					return
				}
				_ = os.Unsetenv(key)
			})
			return os.Setenv(key, value)
		}}
	}
	t.Cleanup(func() { processEnv = prev })
}

// resetEnvLoad 让「只加载一次」那一步可以重新跑一遍。
//
// dotenv.go 里那几个包级状态（一次性开关、它记住的错误、显式指定的路径）
// 从包外没有别的办法复位。只有真的要走 Load / LoadOps 的用例需要它——
// 直接调 apply 的用例不碰全局。跑完再复位一次，免得一次显式指定漏给后面的用例。
func resetEnvLoad(t *testing.T) {
	t.Helper()
	clearEnvLoad()
	t.Cleanup(clearEnvLoad)
}

func clearEnvLoad() {
	envOnce = sync.Once{}
	envLoadErr = nil
	envExplicitFile = ""
	envLoaded = false
}

// 用例 1：当前目录有 .env，其中未设置的键被填上。
func TestEnvFileFillsUnsetKeysFromWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, "LARKAX_DB_DSN=postgres://from-file\nLARKAX_LOG_LEVEL=debug\n")

	env := newEnvStub(nil)
	if err := stubOptions(env, dir).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got := env.values["LARKAX_DB_DSN"]; got != "postgres://from-file" {
		t.Errorf("LARKAX_DB_DSN = %q，期望从 .env 填上", got)
	}
	if got := env.values["LARKAX_LOG_LEVEL"]; got != "debug" {
		t.Errorf("LARKAX_LOG_LEVEL = %q，期望 debug", got)
	}
}

// 用例 2：当前目录没有，git 仓库根上有——找得到。
//
// 这是「进到子目录里跑命令」的形状，也是这个功能真正省事的地方。
func TestEnvFileFoundInGitRootAbove(t *testing.T) {
	root := t.TempDir()
	makeGitRoot(t, root)
	writeEnvFile(t, root, "LARKAX_LOG_LEVEL=debug\n")
	sub := mkdir(t, filepath.Join(root, "internal", "config"))

	env := newEnvStub(nil)
	if err := stubOptions(env, sub).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got := env.values["LARKAX_LOG_LEVEL"]; got != "debug" {
		t.Errorf("LARKAX_LOG_LEVEL = %q，期望从仓库根的 .env 填上", got)
	}
}

// .git 在 worktree 与子模块里是文件而不是目录，两种都要算仓库根。
//
// 本仓自己就有一堆 worktree，这条不是假想的情形。
func TestGitRootMayBeFile(t *testing.T) {
	root := t.TempDir()
	dotGit := filepath.Join(root, ".git")
	if err := os.WriteFile(dotGit, []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600); err != nil {
		t.Fatalf("写入 %s 失败: %v", dotGit, err)
	}
	writeEnvFile(t, root, "LARKAX_LOG_LEVEL=debug\n")
	sub := mkdir(t, filepath.Join(root, "pkg"))

	env := newEnvStub(nil)
	if err := stubOptions(env, sub).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got := env.values["LARKAX_LOG_LEVEL"]; got != "debug" {
		t.Errorf(".git 是文件时没被当成仓库根：LARKAX_LOG_LEVEL = %q", got)
	}
}

// 用例 3：上一级有 .env，但它隔着 git 仓库根（在仓库外）——不越界，不用它。
//
// 越界的坏法不是「读不到配置」，而是「读到别人的配置」：共享机器上工作目录的
// 某个祖先里可能躺着另一套服务的 .env。
func TestEnvFileSearchStopsAtGitRoot(t *testing.T) {
	outer := t.TempDir()
	writeEnvFile(t, outer, "LARKAX_LOG_LEVEL=from-outside\n")
	root := mkdir(t, filepath.Join(outer, "repo"))
	makeGitRoot(t, root)
	sub := mkdir(t, filepath.Join(root, "pkg", "deep"))

	env := newEnvStub(nil)
	if err := stubOptions(env, sub).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got, ok := env.values["LARKAX_LOG_LEVEL"]; ok {
		t.Errorf("仓库根之外的 .env 不该被读到，却填上了 %q", got)
	}
}

// 祖先链上没有任何 .git 时只看起点：那时没有边界可用，往上找等于翻别人的目录。
func TestEnvFileWithoutGitRootOnlyLooksAtStart(t *testing.T) {
	parent := t.TempDir()
	writeEnvFile(t, parent, "LARKAX_LOG_LEVEL=from-parent\n")
	child := mkdir(t, filepath.Join(parent, "child"))

	// 前提：临时目录不在任何仓库里。它要是落在仓库里，边界就挪到那里去了，
	// 这条用例问的也就不是同一个问题——先把它说清楚，而不是给一个看不懂的红。
	for dir := filepath.Clean(child); ; {
		if isGitRoot(dir) {
			t.Fatalf("%s 是 git 仓库根，而这条用例假定临时目录不在任何仓库里", dir)
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}

	env := newEnvStub(nil)
	if err := stubOptions(env, child).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got, ok := env.values["LARKAX_LOG_LEVEL"]; ok {
		t.Errorf("没有 git 仓库根时不该往上找，却填上了 %q", got)
	}
}

// 用例 4：同名时**系统环境变量赢**。
//
// 这是这一整件事里最要紧的一条，也是安全属性：生产容器把配置注入成环境变量，
// 而镜像里或挂载的代码目录里可能留着一份开发用的 .env。覆盖它等于生产连上
// 开发库、或者拿开发的主密钥去解生产数据。
func TestSystemEnvironmentWinsOverEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, "LARKAX_DB_DSN=postgres://from-file\nLARKAX_LOG_LEVEL=debug\n")

	env := newEnvStub(map[string]string{"LARKAX_DB_DSN": "postgres://from-system"})
	if err := stubOptions(env, dir).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got := env.values["LARKAX_DB_DSN"]; got != "postgres://from-system" {
		t.Errorf("系统环境变量被 .env 覆盖了：LARKAX_DB_DSN = %q", got)
	}
	// 同一条用例同时钉住「填补」那一侧：一个键都不填也能让上面那句通过，
	// 而那是「.env 根本不生效」，不是「系统环境变量优先」。
	if got := env.values["LARKAX_LOG_LEVEL"]; got != "debug" {
		t.Errorf("未设置的键仍应被填上：LARKAX_LOG_LEVEL = %q", got)
	}
}

// 系统环境里设成**空串**也算已设置，.env 不该把它填上。
//
// 判据是「这个键在不在」，不是「它的值空不空」：空值常常是有意义的
// （LARKAX_REDIS_DSN 为空就是「不启用 Redis」），而按值判断会让
// 「显式设成空」与「压根没设」混成一种，使用者就没法用它关掉 .env 里的一项。
func TestEmptySystemVariableStillWins(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, "LARKAX_REDIS_DSN=redis://from-file:6379/0\n")

	env := newEnvStub(map[string]string{"LARKAX_REDIS_DSN": ""})
	if err := stubOptions(env, dir).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got := env.values["LARKAX_REDIS_DSN"]; got != "" {
		t.Errorf("设成空串的变量被 .env 覆盖了：LARKAX_REDIS_DSN = %q", got)
	}
}

// 用例 5：没有 .env 时静默跳过，不报错、不填任何键。
func TestMissingEnvFileIsSilent(t *testing.T) {
	dir := t.TempDir()

	env := newEnvStub(nil)
	if err := stubOptions(env, dir).apply(); err != nil {
		t.Fatalf("没有 .env 时不该报错: %v", err)
	}
	if len(env.values) != 0 {
		t.Errorf("不该填任何键，得到 %v", env.values)
	}
}

// 用例 6：--env-file 指定的文件不存在时报错，且错误里要点名那个路径。
//
// 与上一条的差别是「谁选的这份文件」：自动查找没找到是正常情形（环境变量
// 可能就是完整的一份配置），显式指定没找到多半是路径敲错了，静默跳过会让
// 命令带着另一份配置继续跑。
func TestExplicitEnvFileMissingIsError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.env")

	env := newEnvStub(nil)
	opts := stubOptions(env, dir)
	opts.explicit = missing

	err := opts.apply()
	if err == nil {
		t.Fatal("显式指定的文件不存在时应报错")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误信息未点名那个路径: %v", err)
	}
}

// 用例 7：显式指定的文件与自动查找的候选不同时，用指定的那一份。
func TestExplicitEnvFileWinsOverSearch(t *testing.T) {
	auto := t.TempDir()
	writeEnvFile(t, auto, "LARKAX_LOG_LEVEL=from-auto\n")

	explicitDir := t.TempDir()
	explicit := filepath.Join(explicitDir, "staging.env")
	if err := os.WriteFile(explicit, []byte("LARKAX_LOG_LEVEL=from-explicit\n"), 0o600); err != nil {
		t.Fatalf("写入 %s 失败: %v", explicit, err)
	}

	env := newEnvStub(nil)
	opts := stubOptions(env, auto)
	opts.explicit = explicit
	if err := opts.apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	if got := env.values["LARKAX_LOG_LEVEL"]; got != "from-explicit" {
		t.Errorf("LARKAX_LOG_LEVEL = %q，期望用 --env-file 指定的那一份", got)
	}
}

// .env 的语法交给 godotenv：引号、`export` 前缀、行尾注释这些边角自己写
// 很容易漏，而漏掉的表现是「值里多了一个引号」这类安静的错误。
//
// 这条用例问的不是「我们的解析器对不对」，而是「解析这件事还在库那边」——
// 换成自己写的实现时，下面几条会红。
func TestEnvFileSyntaxComesFromGodotenv(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, strings.Join([]string{
		"# 一行注释",
		"",
		`export LARKAX_LOG_LEVEL="debug"`,
		`LARKAX_ENV='production'`,
		"LARKAX_REDIS_DSN=redis://127.0.0.1:6379/0 # 行尾注释",
	}, "\n")+"\n")

	env := newEnvStub(nil)
	if err := stubOptions(env, dir).apply(); err != nil {
		t.Fatalf("apply() 出错: %v", err)
	}

	want := map[string]string{
		"LARKAX_LOG_LEVEL": "debug",      // export 前缀与双引号都要剥掉
		"LARKAX_ENV":       "production", // 单引号同理
		"LARKAX_REDIS_DSN": "redis://127.0.0.1:6379/0",
	}
	for key, wantValue := range want {
		if got := env.values[key]; got != wantValue {
			t.Errorf("%s = %q，期望 %q", key, got, wantValue)
		}
	}
}

// 用例 8：服务端入口（Load）与运维命令入口（LoadOps）走同一次加载。
//
// 两个入口自己加载一次的话，第二次是静默无效的（键都已经在环境里了），
// 于是「哪一份生效」取决于调用顺序——那种错要靠「服务与命令行读到的配置
// 不一样」才现形。
func TestLoadAndLoadOpsShareOneEnvFileLoad(t *testing.T) {
	resetEnvLoad(t)
	// 先把本包读的那几个变量清空（结束时会还原），让结果只取决于 .env。
	setEnv(t, nil)

	// ROLE=worker 是刻意的：Load 在 worker 下不要求主密钥，于是这一条不必
	// 再往 .env 里塞一把密钥就能走完整条路径。
	root := t.TempDir()
	makeGitRoot(t, root)
	writeEnvFile(t, root, "LARKAX_DB_DSN=postgres://from-file\nLARKAX_ROLE=worker\nLARKAX_LOG_LEVEL=debug\n")
	sub := mkdir(t, filepath.Join(root, "sub"))

	swapProcessEnv(t, newEnvStub(nil), sub)

	// 起点是子目录，.env 在仓库根：这一条同时走了一遍自动查找。
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() 出错: %v", err)
	}
	if cfg.DBDSN != "postgres://from-file" {
		t.Errorf("服务端入口没读到 .env 的 DSN: %q", cfg.DBDSN)
	}
	if cfg.Role != RoleWorker {
		t.Errorf("Role = %q，期望 worker（来自 .env）", cfg.Role)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q，期望 debug（来自 .env）", cfg.LogLevel)
	}

	ops, err := LoadOps()
	if err != nil {
		t.Fatalf("LoadOps() 出错: %v", err)
	}
	if ops.DBDSN != "postgres://from-file" || ops.LogLevel != "debug" {
		t.Errorf("运维入口读到的不是同一份：%+v", ops)
	}

	// 第二个入口若自己去读了一遍文件，下面这一项就会被它看见（第一次读到的
	// 那份里没有它）。改文件是唯一能看出「读了几次」的办法——不覆盖已有键
	// 这条规则会让「又读了一遍」在环境里不留痕迹。
	writeEnvFile(t, root, "LARKAX_DB_DSN=postgres://from-file\nLARKAX_ROLE=worker\nLARKAX_LOG_LEVEL=debug\nLARKAX_ENV=production\n")
	again, err := LoadOps()
	if err != nil {
		t.Fatalf("第三次 LoadOps() 出错: %v", err)
	}
	if again.Env != "development" {
		t.Errorf("第二个入口又读了一遍 .env：Env = %q，期望默认值 development", again.Env)
	}
}

// 「只加载一次」：第二次调用不去读文件。
//
// 验法与上一条的末尾同一个思路——把文件改掉，看第二次会不会拿到新加的那一项。
func TestEnvFileIsLoadedOnlyOnce(t *testing.T) {
	resetEnvLoad(t)
	setEnv(t, nil)

	root := t.TempDir()
	makeGitRoot(t, root)
	writeEnvFile(t, root, "LARKAX_DB_DSN=postgres://from-file\n")

	swapProcessEnv(t, newEnvStub(nil), root)

	if _, err := LoadOps(); err != nil {
		t.Fatalf("第一次 LoadOps() 出错: %v", err)
	}

	writeEnvFile(t, root, "LARKAX_DB_DSN=postgres://from-file\nLARKAX_LOG_LEVEL=debug\n")

	cfg, err := LoadOps()
	if err != nil {
		t.Fatalf("第二次 LoadOps() 出错: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("第二次调用又读了 .env：LogLevel = %q，期望默认值 info", cfg.LogLevel)
	}
}

// 已经加载过之后再指定 --env-file 是错误，不是空操作。
//
// 空操作会让使用者以为选的文件生效了，接着看到的是「命令用了另一份配置」——
// 那是这一整块里最难查的表现。
func TestUseEnvFileAfterLoadIsError(t *testing.T) {
	resetEnvLoad(t)

	dir := t.TempDir()
	swapProcessEnv(t, newEnvStub(nil), dir)

	first := writeEnvFile(t, dir, "LARKAX_LOG_LEVEL=debug\n")
	if err := UseEnvFile(first); err != nil {
		t.Fatalf("第一次 UseEnvFile() 出错: %v", err)
	}

	second := filepath.Join(dir, "other.env")
	if err := os.WriteFile(second, []byte("LARKAX_LOG_LEVEL=warn\n"), 0o600); err != nil {
		t.Fatalf("写入 %s 失败: %v", second, err)
	}
	err := UseEnvFile(second)
	if err == nil {
		t.Fatal("已经加载过之后再指定 --env-file 应报错，而不是静默无效")
	}
	if !strings.Contains(err.Error(), "--env-file") {
		t.Errorf("错误信息没说清是哪一步出的问题: %v", err)
	}
}

// 空路径也是错误：空串在内部是「没指定」，放过它就等于回落成自动查找，
// 而那与「点名要哪一份」的意图正好相反。
func TestUseEnvFileRejectsEmptyPath(t *testing.T) {
	resetEnvLoad(t)

	dir := t.TempDir()
	writeEnvFile(t, dir, "LARKAX_LOG_LEVEL=debug\n")
	env := newEnvStub(nil)
	swapProcessEnv(t, env, dir)

	if err := UseEnvFile(""); err == nil {
		t.Fatal("空路径应报错，而不是回落成自动查找")
	}
	if _, ok := env.values["LARKAX_LOG_LEVEL"]; ok {
		t.Error("报错的同时不该加载任何文件")
	}
}

// 写入失败要说出来，而不是留下一半填好的环境继续跑。
func TestEnvFileWriteErrorIsReported(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, "LARKAX_LOG_LEVEL=debug\n")

	env := newEnvStub(nil)
	env.setErr = fmt.Errorf("写入被拒")
	opts := stubOptions(env, dir)

	err := opts.apply()
	if err == nil {
		t.Fatal("写入失败时应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_LOG_LEVEL") {
		t.Errorf("错误信息未点名那个键: %v", err)
	}
}

// processEnv 要给出真实的当前目录：自动查找 .env 就是从它出发的，
// 这里返回空串的话，当前目录里的 .env 永远找不到——而那条路径正是
// 「在仓库里直接跑命令」最常见的用法。
func TestProcessEnvReportsWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	if got := processEnv().dir; got != wd {
		t.Errorf("processEnv().dir = %q，期望当前目录 %q", got, wd)
	}
}
