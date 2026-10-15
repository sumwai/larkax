// Package config 从环境变量加载配置。
//
// 约定：所有配置项都带 LARKAX_ 前缀，一律通过环境变量提供。
// 配置文件与命令行参数暂不支持——部署形态是容器，环境变量是最直接的注入方式，
// 且天然不会把值写进镜像层。
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Role 是同一个二进制承担的服务角色。
//
// 数据面（RoleGateway）绝不能依赖控制面（RoleControl）在线，
// 因此三者是同一份代码的不同启动模式，而不是三个服务。
type Role string

const (
	RoleGateway Role = "gateway"
	RoleControl Role = "control"
	RoleWorker  Role = "worker"
)

func (r Role) valid() bool {
	switch r {
	case RoleGateway, RoleControl, RoleWorker:
		return true
	}
	return false
}

// Config 是服务的完整配置。
type Config struct {
	Env  string
	Role Role

	HTTPAddr    string
	MetricsAddr string
	LogLevel    string
	LogFormat   string

	DB     DB
	Redis  Redis
	Secret Secret

	ShutdownTimeout time.Duration
}

// DB 是 PostgreSQL 连接配置。
type DB struct {
	DSN          string
	MaxConns     int32
	MinConns     int32
	ConnLifetime time.Duration
}

// Redis 是 Redis 连接配置。
type Redis struct {
	Addr     string
	Password string
	DB       int
}

// Secret 持有解密渠道凭证所需的主密钥。
//
// 这个值只应来自密钥管理服务，不应出现在配置文件或镜像里。
// String 方法有意不输出内容，避免日志与错误信息里带出密钥。
type Secret struct {
	CredentialMasterKey []byte
}

func (s Secret) String() string {
	if len(s.CredentialMasterKey) == 0 {
		return "Secret{credential_master_key: <unset>}"
	}
	return "Secret{credential_master_key: <redacted>}"
}

// Load 读取环境变量并校验。任何缺失或非法的必填项都会让服务拒绝启动：
// 带着半截配置跑起来比启动失败更难排查。
//
// 解析失败一律报错而不回落默认值。静默回落会让「配置写错了」表现为
// 「行为跟预期不一样」，排查成本远高于启动时直接失败。
func Load() (*Config, error) {
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	cfg := &Config{
		Env:         env("LARKAX_ENV", "development"),
		Role:        Role(env("LARKAX_ROLE", string(RoleGateway))),
		HTTPAddr:    env("LARKAX_HTTP_ADDR", ":8080"),
		LogLevel:    env("LARKAX_LOG_LEVEL", "info"),
		LogFormat:   env("LARKAX_LOG_FORMAT", ""),
		MetricsAddr: env("LARKAX_METRICS_ADDR", ":9090"),
		DB: DB{
			DSN: os.Getenv("LARKAX_DB_DSN"),
		},
		Redis: Redis{
			Addr:     env("LARKAX_REDIS_ADDR", ""),
			Password: os.Getenv("LARKAX_REDIS_PASSWORD"),
		},
	}

	if !cfg.Role.valid() {
		fail("LARKAX_ROLE 必须是 gateway/control/worker 之一，当前为 %q", cfg.Role)
	}
	if cfg.DB.DSN == "" {
		fail("LARKAX_DB_DSN 未设置")
	}

	if n, err := envInt("LARKAX_DB_MAX_CONNS", 20); err != nil {
		fail("%v", err)
	} else {
		cfg.DB.MaxConns = int32(n)
	}
	if n, err := envInt("LARKAX_DB_MIN_CONNS", 2); err != nil {
		fail("%v", err)
	} else {
		cfg.DB.MinConns = int32(n)
	}
	if cfg.DB.MaxConns < 1 {
		fail("LARKAX_DB_MAX_CONNS 必须大于 0，当前为 %d", cfg.DB.MaxConns)
	}
	if cfg.DB.MinConns < 0 || cfg.DB.MinConns > cfg.DB.MaxConns {
		fail("LARKAX_DB_MIN_CONNS 必须在 0 与 LARKAX_DB_MAX_CONNS 之间，当前为 %d", cfg.DB.MinConns)
	}

	if d, err := envDuration("LARKAX_DB_CONN_LIFETIME", time.Hour); err != nil {
		fail("%v", err)
	} else {
		cfg.DB.ConnLifetime = d
	}
	if d, err := envDuration("LARKAX_SHUTDOWN_TIMEOUT", 30*time.Second); err != nil {
		fail("%v", err)
	} else {
		cfg.ShutdownTimeout = d
	}
	if n, err := envInt("LARKAX_REDIS_DB", 0); err != nil {
		fail("%v", err)
	} else {
		cfg.Redis.DB = n
	}

	// 渠道凭证主密钥只在需要解密的角色上强制要求。
	// worker 里的日历同步、对账任务不碰凭证，要求它就等于无谓地扩大密钥的暴露面。
	//
	// 判据写成「不等于 worker」而非「等于 gateway 或 control」：角色写错时
	// 按需要密钥处理，失败在安全一侧。
	if cfg.Role != RoleWorker {
		raw := os.Getenv("LARKAX_CREDENTIAL_MASTER_KEY")
		switch {
		case raw == "":
			fail("LARKAX_CREDENTIAL_MASTER_KEY 未设置")
		default:
			key, err := decodeKey(raw)
			if err != nil {
				fail("LARKAX_CREDENTIAL_MASTER_KEY 无效: %v", err)
			} else {
				cfg.Secret.CredentialMasterKey = key
			}
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("配置无效:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return cfg, nil
}

// IsProduction 决定若干默认行为的松紧，例如日志格式与错误详情的暴露程度。
func (c *Config) IsProduction() bool { return c.Env == "production" }

// KeySize 是渠道凭证加密所用主密钥的字节长度（AES-256）。
const KeySize = 32

// decodeKey 解析 base64 编码的主密钥。
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
		key, err := enc.DecodeString(strings.TrimSpace(raw))
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，当前为 %q", key, v)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是时长（如 30s、5m），当前为 %q", key, v)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s 不能为负，当前为 %q", key, v)
	}
	return d, nil
}
