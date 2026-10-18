// Package config 处理进程启动所需的配置。
//
// 配置分三类，只有两类进本包：
//
//  1. 启动必需 —— 环境变量。没有它进程起不来或解不开存量数据，也就是
//     必须在连上数据库之前拿到。数据库 DSN、主密钥、角色三项。
//  2. 实例相关 —— 环境变量，全部有默认值。监听地址、日志级别、环境名、
//     Redis 连接串。每个实例可以不同，丢了只影响行为，不影响数据。
//  3. 数据相关 —— 数据库的 settings 表，不在本包。留存期、默认模型、
//     配额策略这些必须跟着数据一起走：换环境或做恢复时不需要把一堆
//     环境变量照抄一遍，也不会因为漏了一个变量就导致数据不可用。
//
// 因此新增环境变量的门槛很高：只有当这个值必须在连上数据库之前拿到时才允许。
// 其余一律进 settings 表。
//
// 两个加载入口，面向两类调用方：Load 给服务进程，LoadOps 给运维命令
// （cmd/larkaxctl）。两者的差别不只是「读哪些变量」，而是**校验哪些项**——
// 运维命令不属于任何角色，所以角色与主密钥的校验不适用于它。
//
// 两个入口在读取环境变量之前都会先试着加载 .env 文件（见 dotenv.go），
// 且**系统环境变量优先**：.env 只填补尚未设置的键，绝不覆盖已有的。
// 加载发生在两个入口内部而不是它们的调用方，是为了让「.env 生效」这件事
// 与「谁在调」无关——今天有几十处调用点，漏掉一处的表现是那条命令
// 安静地用了另一份配置。
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// defaultHTTPAddr 是主服务没配 LARKAX_HTTP_ADDR 时听在哪儿。
//
// 提成一个常量，是因为这个值有两个读取方：服务进程（Load）与运维命令
// （LoadOps 的 HTTPAddr，`doctor --live` 靠它算默认往哪儿打请求）。两处各写一份
// 字面量时，改了一处而没改另一处的表现是 doctor 朝一个没人监听的端口发请求。
// Makefile 的 check-ports 里还有一份兜底值，那一份与这里由
// makefile_ports_test.go 对在一起。
const defaultHTTPAddr = "127.0.0.1:8080"

// Role 是同一个二进制承担的服务角色。
//
// 数据面（RoleGateway）绝不能依赖控制面（RoleControl）在线，
// 因此三者是同一份代码的不同启动模式，而不是三个服务。
type Role string

// 三个角色的取值，与 LARKAX_ROLE 环境变量一致。
const (
	RoleGateway Role = "gateway"
	RoleControl Role = "control"
	RoleWorker  Role = "worker"
)

// NeedsMasterKey 表示该角色需要解开渠道凭证，因而必须配置主密钥。
//
// 判据写成「不等于 worker」而非「等于 gateway 或 control」：
// 角色写错时按需要密钥处理，失败在安全一侧。
func (r Role) NeedsMasterKey() bool { return r != RoleWorker }

func (r Role) valid() bool {
	switch r {
	case RoleGateway, RoleControl, RoleWorker:
		return true
	}
	return false
}

// Config 是进程启动所需的最小配置。
type Config struct {
	Env          string
	Role         Role
	HTTPAddr     string
	InternalAddr string
	LogLevel     string

	// DBDSN 与 RedisDSN 用 DSN 形式承载地址与凭证，而不是拆成
	// 主机、端口、用户名、密码、库名五个变量。少四个变量就少四处不同步的可能。
	DBDSN    string
	RedisDSN string // 空表示不启用 Redis

	MasterKeys []MasterKey
}

// MasterKey 是一把用于解开数据密钥（DEK）的主密钥（KEK）。
//
// 为什么要多把：凭证密文是用 DEK 加密的，DEK 又用每把 KEK 各包一份存在库里。
// 任何一把 KEK 都能把 DEK 解出来，于是——
//   - 轮换密钥时新旧并存，不需要一次性重加密全部数据；
//   - 弄丢其中一把（环境变量被覆盖、密钥管理服务迁移出错）不会让数据变成废纸；
//   - 可以留一把离线保管作为恢复密钥。
//
// ID 记录在密文的包装信息里，用来判断该用哪把钥匙去解，所以必须保持稳定
// ——改名等于丢失对应数据的可读性。只有一把钥匙时可以不写前缀，那时按
// UnsetMasterKeyID 处理（见 parseMasterKeys）。
type MasterKey struct {
	ID  string
	Key []byte
}

// String 与 GoString 让密钥在被打印时只留下长度，不留下内容。
//
// MasterKey 会跟着 Config 一起出现在日志与调试输出里，默认展开就是明文。
// 长度是有意保留的：排查配置问题时「解出来多少字节」是唯一有用的信息，
// 而它不足以还原密钥。
func (k MasterKey) String() string {
	return fmt.Sprintf("config.MasterKey{ID:%q Key:%d 字节已隐藏}", k.ID, len(k.Key))
}

// GoString 让 %#v 也走 String。两者不会互相退化，缺一个就等于开了个口子。
func (k MasterKey) GoString() string { return k.String() }

// KeySize 是主密钥的字节长度（AES-256）。
const KeySize = 32

// UnsetMasterKeyID 是省略 ID 时使用的默认标识。
const UnsetMasterKeyID = "default"

// Load 读取环境变量并校验。任何缺失或非法的必填项都会让服务拒绝启动：
// 带着半截配置跑起来比启动失败更难排查。
//
// 解析失败一律报错而不回落到默认值。静默回落会让「配置写错了」表现为
// 「行为跟预期不一样」，排查成本远高于启动时直接失败。
func Load() (*Config, error) {
	// .env 先在读取之前填好，两个入口共用同一次加载（见 dotenv.go）。
	if err := ensureEnvLoaded(); err != nil {
		return nil, err
	}

	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	cfg := &Config{
		Env:  env("LARKAX_ENV", "development"),
		Role: Role(env("LARKAX_ROLE", string(RoleGateway))),
		// 两个监听地址默认只绑回环：内部端点（/healthz、/readyz、/metrics）与业务入口
		// 共用这套默认值，而它们都不需要对外。要对外时显式写，例如容器里写 ":8080"。
		HTTPAddr:     env("LARKAX_HTTP_ADDR", defaultHTTPAddr),
		InternalAddr: env("LARKAX_INTERNAL_ADDR", "127.0.0.1:9090"),
		LogLevel:     env("LARKAX_LOG_LEVEL", "info"),
		DBDSN:        os.Getenv("LARKAX_DB_DSN"),
		RedisDSN:     os.Getenv("LARKAX_REDIS_DSN"),
	}

	if !cfg.Role.valid() {
		fail("LARKAX_ROLE 必须是 gateway/control/worker 之一，当前为 %q", cfg.Role)
	}
	if cfg.DBDSN == "" {
		fail("LARKAX_DB_DSN 未设置")
	}
	if cfg.RedisDSN != "" && !hasScheme(cfg.RedisDSN, "redis", "rediss", "unix") {
		fail("LARKAX_REDIS_DSN 必须以 redis://、rediss:// 或 unix:// 开头")
	}

	if cfg.Role.NeedsMasterKey() {
		keys, err := MasterKeysFromEnv()
		if err != nil {
			fail("LARKAX_MASTER_KEYS 无效: %v", err)
		} else {
			cfg.MasterKeys = keys
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("配置无效:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return cfg, nil
}

// OpsConfig 是**运维命令**所需的配置面。
//
// 它与 Config 分开，因为两者回答的不是同一个问题。Config 描述「一个服务实例
// 以什么身份启动」——角色决定它要不要解渠道凭证，主密钥跟着角色走。运维命令
// 不属于任何角色：它要的是「连哪个库」，加上打日志要的级别，以及下面那个
// 监听地址。
//
// 做成一个具名的类型，是为了让这个面**只有这几项**。想给运维命令加配置时改的是
// 这里，而服务那一侧不受影响；反过来，加进 Config 的字段也不会悄悄变成
// 「运维命令也依赖它」。
type OpsConfig struct {
	Env      string
	LogLevel string
	DBDSN    string

	// HTTPAddr 是主服务的**监听**地址，取值与默认值都跟 Config 的同名字段
	// 一致（LARKAX_HTTP_ADDR）。
	//
	// 它进运维命令的配置面，是因为 `doctor --live` 要朝自己的网关打一次真请求：
	// 那个默认地址只有配置知道，写死一个端口时，在 .env 里改过端口的人拿到的是
	// 「连不上」，而那句话读起来像服务没起来。
	//
	// 它是监听地址，不是能连的地址——`:8080` 这种不写主机的「所有网卡」写法
	// 不是能给 http.Client 的值，怎么把它变成能连的地址是调用方的事。
	HTTPAddr string
}

// IsProduction 与 Config 的同名方法一致：只用来选日志格式。
func (c *OpsConfig) IsProduction() bool { return c.Env == "production" }

// LoadOps 读取运维命令所需的配置。它是**运维命令唯一的加载入口**，
// 与 Load 分开。
//
// # 它为什么不校验 LARKAX_ROLE 与 LARKAX_MASTER_KEYS
//
// 这两项描述的是服务角色——谁来解渠道凭证、以什么身份接流量。运维命令不属于
// 任何角色，所以把「命令跑不跑得起来」绑到它们身上，会让一个与本次操作无关的
// 变量决定命令的成败。实测的两种形态：
//
//	LARKAX_ROLE=worker larkaxctl rotate      # 主密钥设好了，却因为角色而失败
//	larkaxctl migrate up                     # 不设角色反而要求主密钥，而建表不需要它
//
// 第二种尤其难查：它只在手工部署、或者只用运维命令的路径上出现，而报错信息
// 不会提示「这一步其实不需要主密钥」。
//
// 命令真需要主密钥时（轮换、写渠道）自己调 MasterKeysFromEnv，与
// cmd/larkaxctl/channel.go 的 masterKeySealer 走同一条路。
//
// 同理不校验 LARKAX_REDIS_DSN：运维命令不连 Redis。
//
// # 服务那一侧的校验强度不受影响
//
// Load 一行没动，它该报的错一条不少：gateway / control 缺主密钥就拒绝启动，
// 那是数据面的防线，与这里放宽的是两件事。
func LoadOps() (*OpsConfig, error) {
	// 与 Load 走**同一次** .env 加载（sync.Once 在 dotenv.go 里）。
	// 两个入口各加载一次的话，第二次是静默无效的，而「哪一次算数」
	// 取决于调用顺序。
	if err := ensureEnvLoaded(); err != nil {
		return nil, err
	}

	cfg := &OpsConfig{
		Env:      env("LARKAX_ENV", "development"),
		LogLevel: env("LARKAX_LOG_LEVEL", "info"),
		DBDSN:    os.Getenv("LARKAX_DB_DSN"),
		// 同一个变量、同一个默认值，读法与 Load 一致（见 defaultHTTPAddr）。
		// 它不参与下面的校验：一个运维命令不会因为监听地址写得怪而跑不起来。
		HTTPAddr: env("LARKAX_HTTP_ADDR", defaultHTTPAddr),
	}
	if cfg.DBDSN == "" {
		// 措辞与 Load 保持一致：两者都是「配置无效」这一类，而缺 DSN 是
		// 运维命令唯一真正必需的一项。
		return nil, fmt.Errorf("配置无效:\n  - LARKAX_DB_DSN 未设置")
	}
	return cfg, nil
}

// IsProduction 目前只用来选日志格式（开发是文本、生产是 JSON）。
//
// 它本意是「若干默认行为的松紧」的开关，但错误详情的暴露程度尚未接进来，
// 所以不要指望设了它就能收紧错误输出。
func (c *Config) IsProduction() bool { return c.Env == "production" }

// String 与 GoString 让 Config 在被打印时自动隐藏敏感字段。
//
// 为什么必须实现：Config 里装着数据库口令与主密钥，而打印一个结构体是
// 顺手就会做的事——`slog.Any("config", cfg)`、`fmt.Printf("%+v")`、
// 测试失败时 t.Log(cfg)。不实现这两个方法，这些再普通不过的写法
// 就会把主密钥原样写进日志与 CI 输出。默认实现是按字段原样展开，
// 也就是说「什么都不做」等于主动泄密。
//
// 保留非敏感字段是为了可用性：排查配置问题时要能看见角色、环境、地址。
func (c *Config) String() string {
	return c.safeView()
}

// GoString 覆盖 %#v。%#v 不走 String，会直接展开结构体，必须单独挡住。
func (c *Config) GoString() string { return c.safeView() }

// LogValue 覆盖 slog 的取值路径。slog 处理 Any 值时优先用它，其次才看
// String/GoString；显式实现可以避免以后 slog 的取值规则变化。
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", c.Env),
		slog.String("role", string(c.Role)),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("internal_addr", c.InternalAddr),
		slog.String("log_level", c.LogLevel),
		slog.String("db_dsn", RedactDSN(c.DBDSN)),
		slog.String("redis_dsn", RedactDSN(c.RedisDSN)),
		slog.Int("master_keys", len(c.MasterKeys)),
	)
}

func (c *Config) safeView() string {
	return fmt.Sprintf(
		"config.Config{Env:%q Role:%q HTTPAddr:%q InternalAddr:%q LogLevel:%q DBDSN:%q RedisDSN:%q MasterKeys:%d}",
		c.Env, c.Role, c.HTTPAddr, c.InternalAddr, c.LogLevel,
		RedactDSN(c.DBDSN), RedactDSN(c.RedisDSN), len(c.MasterKeys),
	)
}

// RedactDSN 抹掉连接串里的口令，其余部分保留。
//
// 保留主机与库名是有意的：排查「连错环境」时这几个字段是关键线索，
// 而口令一旦进了日志就等于泄露，且日志的留存与传播范围远大于密钥本身。
func RedactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	const mask = "REDACTED"

	// URL 形式：postgres://user:pass@host/db、redis://:pass@host/0
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), mask)
		}
		return u.String()
	}

	// 关键字形式：host=... user=... password=... dbname=...
	// 逐词扫描而不是正则替换：这里不追求看懂语法，只要不把值原样带出去。
	fields := strings.Fields(dsn)
	for i, f := range fields {
		name, _, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(name) {
		case "password", "passwd", "pass", "pwd":
			fields[i] = name + "=" + mask
		}
	}
	return strings.Join(fields, " ")
}

// MasterKeysFromEnv 解析环境变量里的主密钥，**与角色无关**。
//
// Load 只在需要主密钥的角色上用它（worker 不解析），LoadOps 完全不碰它。
// 「要不要主密钥」这件事由**服务角色**决定，而运维命令不属于任何角色：它要写
// 一条渠道（或轮换一次凭证）就得拿到主密钥，与 LARKAX_ROLE 取什么值无关。
// 需要它的命令（channel、rotate）直接调这里，好过让
// `LARKAX_ROLE=worker larkaxctl channel create` 因为一个与本次操作无关的
// 变量而失败，也好过在命令行里再抄一份 base64 解析。
func MasterKeysFromEnv() ([]MasterKey, error) {
	return parseMasterKeys(os.Getenv("LARKAX_MASTER_KEYS"))
}

// parseMasterKeys 解析 "<id>:<base64>,<id>:<base64>" 形式的密钥列表。
//
// 省略 ID 时（整段就是裸的 base64）按 UnsetMasterKeyID 处理，
// 让「只有一把钥匙」的简单情形不必写前缀。
func parseMasterKeys(raw string) ([]MasterKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("未设置")
	}

	var keys []MasterKey
	seen := make(map[string]bool)

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		id, encoded := UnsetMasterKeyID, part
		if idx := strings.Index(part, ":"); idx >= 0 {
			id = strings.TrimSpace(part[:idx])
			encoded = strings.TrimSpace(part[idx+1:])
		}
		if id == "" {
			return nil, fmt.Errorf("密钥标识不能为空")
		}
		if seen[id] {
			return nil, fmt.Errorf("密钥标识 %q 重复", id)
		}

		key, err := decodeKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("密钥 %q: %w", id, err)
		}
		seen[id] = true
		keys = append(keys, MasterKey{ID: id, Key: key})
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("未解析出任何密钥")
	}
	return keys, nil
}

// decodeKey 解析 base64 编码的密钥。
//
// 有意同时接受标准与 URL-safe 两种字母表，以及带不带填充的写法：
// 密钥由运维用不同工具生成（openssl、云厂商 CLI、密码管理器），
// 因为字母表不匹配而启动失败是纯粹的浪费。
func decodeKey(raw string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		key, err := enc.DecodeString(raw)
		if err != nil {
			continue
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("解码后为 %d 字节，需要 %d 字节", len(key), KeySize)
		}
		return key, nil
	}
	return nil, fmt.Errorf("不是合法的 base64 编码")
}

func hasScheme(dsn string, schemes ...string) bool {
	for _, s := range schemes {
		if strings.HasPrefix(dsn, s+"://") {
			return true
		}
	}
	return false
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
