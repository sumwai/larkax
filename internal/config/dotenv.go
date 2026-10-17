// 本文件负责在读取配置之前把 .env 里的值填进进程环境。
//
// 为什么由进程自己读，而不是要求使用者先在 shell 里 `set -a; . ./.env; set +a`：
// 那是 Bash 语法，在 fish、PowerShell 里直接失效；而且每开一个终端都要重做一遍，
// 漏掉一次的表现是「第一条命令就报 LARKAX_DB_DSN 未设置」，排查方向还指向数据库。
//
// 两条规则决定了这里的形状：
//
//  1. **系统环境变量优先**——.env 只填补**尚未设置**的键。生产容器把配置注入成
//     环境变量，而镜像里、挂载的代码目录里可能留着一份开发用的 .env；
//     覆盖就等于生产连上开发库、或者拿开发的主密钥去解生产数据。
//  2. **只加载一次**——两个入口（Load 与 LoadOps）共用同一次加载。各自加载时
//     第二次是**静默无效**的（键都已经在环境里了），于是「哪一份生效」取决于
//     调用顺序，而这种错要靠「服务与命令行读到的配置不一样」才现形。
//
// 解析交给 github.com/joho/godotenv：引号、转义、`export` 前缀、行尾注释这些
// 边角自己写很容易漏，而漏掉的表现是「值里多了一个引号」这类安静的错误。
//
// 上面这一节与 `package config` 之间空一行：紧贴着的注释组会被当成**包注释**，
// 而本包的包注释在 config.go 里（revive 的 package-comments 要求它以
// 「Package config」开头，本节不是那个形状）。空一行之后它只是本文件的文件头。

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/joho/godotenv"
)

// envFileName 是自动查找时认的文件名。
const envFileName = ".env"

// dotenvOptions 是一次 .env 加载要碰的外部东西。
//
// 做成字段而不是在实现里直接调 os：用例要能指定查找根、并伪造「系统环境变量」，
// 否则只能靠 os.Chdir 与 os.Setenv 去摆布进程的全局状态——前者会让并行跑的
// 用例互相干扰（整个进程只有一个当前目录），后者会把值漏给后面的用例。
type dotenvOptions struct {
	// explicit 是显式指定的文件（--env-file）。非空时不再自动查找：
	// 使用者点名要哪一份时，再去猜另一份是多余且危险的。
	explicit string
	// dir 是自动查找的起点，通常是进程的当前工作目录。
	dir string
	// lookup 读系统环境变量：写入之前用它判断该键是不是已经有了。
	lookup func(string) (string, bool)
	// set 写入进程环境变量。
	set func(string, string) error
}

// processEnv 给出面向真实进程的那一份依赖。
//
// 它是变量而不是函数，为的是让本包的用例能把它换成替身：Load 与 LoadOps
// 没有参数位可以传替身，而这两个入口正是「.env 填上了没有」要验的地方。
// 除了用例，没有任何地方会改它。
var processEnv = func() dotenvOptions {
	dir, err := os.Getwd()
	if err != nil {
		// 取不到当前目录只影响自动查找（显式指定路径时不看它），
		// 而为此让整个进程起不来是得不偿失的。
		dir = ""
	}
	return dotenvOptions{dir: dir, lookup: os.LookupEnv, set: os.Setenv}
}

// apply 读取 .env，把其中**尚未设置**的键填进环境。
//
// 文件找不到时静默跳过：.env 是**可选**的便利设施，没有它环境变量仍然是一条
// 完整的配置路径，而在一台用环境变量配好的机器上（容器、CI）为「没有 .env」
// 报错，等于把一条正常路径变成失败。反过来，**显式指定**的文件是使用者明确
// 要的，找不到必须报错——那多半是路径敲错了，静默跳过会让命令带着另一份
// 配置跑下去。
func (o dotenvOptions) apply() error {
	path := o.explicit
	if path == "" {
		path = o.find()
		if path == "" {
			return nil
		}
	}

	values, err := godotenv.Read(path)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	// 自己写这段循环而不用 godotenv.Load：Load 把「不覆盖已有的」这条规则
	// 埋在库内部，而它恰恰是本文件最该被钉住的一条（见 dotenv_test.go 的
	// 系统环境变量优先）。用 Read 拿到键值、由这里决定写不写，规则就留在
	// 本文件里，也能被用例直接验。
	for key, value := range values {
		if _, exists := o.lookup(key); exists {
			continue
		}
		if err := o.set(key, value); err != nil {
			return fmt.Errorf("设置 %s 失败: %w", key, err)
		}
	}
	return nil
}

// find 从起点往上找第一个 .env，返回它的路径；没找到返回空串。
//
// 上界是 git 仓库根（含它自己），不越过它。两个方向都要照顾：
//
//   - 往上找是必要的：命令常在子目录里跑（进到某个包下面 `go run` 很常见），
//     而 .env 一般放在仓库根。
//   - 不越过仓库根也是必要的：共享机器与容器里，工作目录的某个祖先可能躺着
//     另一套服务的 .env（例如 /srv 下别人的部署），越过边界就等于让它影响
//     本进程——那种故障的排查方向会指向我们自己的代码。
func (o dotenvOptions) find() string {
	for _, dir := range o.candidates() {
		path := filepath.Join(dir, envFileName)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// candidates 按由近及远给出候选目录：往上走到最近的 .git 为止，把沿途每一层
// 记下来；找不到 .git 时只留起点那一层。
//
// 链上没有 .git 时只看起点：那时没有任何边界可用，继续往上找只会把 /tmp、
// /home 这类与项目无关的目录翻一遍，而那里的 .env 属于别人。所以同一趟
// 往上走承担了两件事——找边界，以及记下边界之内的每一层。
func (o dotenvOptions) candidates() []string {
	if o.dir == "" {
		return nil
	}

	dir := filepath.Clean(o.dir)
	var chain []string
	for {
		chain = append(chain, dir)
		if isGitRoot(dir) {
			return chain
		}
		parent := filepath.Dir(dir)
		if parent == dir { // 到文件系统根（或到了相对路径退不动的 "." ）
			return chain[:1]
		}
		dir = parent
	}
}

// isGitRoot 判断 dir 是不是 git 仓库根，判据是「它下面有 .git」。
//
// 不调 git 命令：本包不引 git 依赖，也不该把「这台机器装没装 git」变成进程
// 启动的前提。.git 在普通仓库里是目录，在 worktree 与子模块里是文件，
// 两种都要算数——用 os.Stat（它会跟随符号链接，对目录与文件一视同仁）。
func isGitRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// 「只加载一次」的那一步。
//
// 共用一次加载是结构上的保证，而不是靠调用顺序的约定：Load 与 LoadOps
// 各自加载时，第二次读到的键一个也填不进去（要么已经设置、要么本来就该被
// 系统环境变量压住），于是「哪一次算数」取决于谁先跑。
var (
	envOnce         sync.Once
	envLoadErr      error
	envExplicitFile string
	envLoaded       bool
)

// ensureEnvLoaded 加载 .env，多个入口只真正执行一次。
func ensureEnvLoaded() error {
	envOnce.Do(func() {
		opts := processEnv()
		opts.explicit = envExplicitFile
		envLoadErr = opts.apply()
		envLoaded = true
	})
	return envLoadErr
}

// UseEnvFile 指定要加载的 .env 文件（对应 larkaxctl 的 --env-file）并立即加载，
// 之后 Load 与 LoadOps 不再去找自动查找到的那一份。
//
// **必须在任何一次配置加载之前调用。** 已经加载过之后再调是**错误**而不是
// 空操作：那时显式文件里的键一个也填不进去（同一次加载已经用掉了），而静默
// 接受会让人以为 --env-file 生效了——接着看到的是「命令用了另一份配置」这种
// 最难查的表现。
//
// 空路径同样是错误：空串在内部是「没指定」的意思，放过它就等于悄悄回落成
// 自动查找，而这与「点名要哪一份」的意图正好相反。
func UseEnvFile(path string) error {
	if path == "" {
		return fmt.Errorf("--env-file 的路径不能为空")
	}
	if envLoaded {
		return fmt.Errorf("--env-file 必须在任何配置加载之前指定")
	}
	envExplicitFile = path
	return ensureEnvLoaded()
}
