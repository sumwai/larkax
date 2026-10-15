package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const validKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 字节

// setEnv 用 t.Setenv 设置环境变量，测试结束后自动还原。
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// clearAll 把本包读取的全部环境变量置空，保证测试之间不互相污染。
func clearAll(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LARKAX_ENV", "LARKAX_ROLE", "LARKAX_HTTP_ADDR", "LARKAX_LOG_LEVEL",
		"LARKAX_LOG_FORMAT", "LARKAX_METRICS_ADDR",
		"LARKAX_DB_DSN", "LARKAX_DB_MAX_CONNS", "LARKAX_DB_MIN_CONNS",
		"LARKAX_DB_CONN_LIFETIME",
		"LARKAX_REDIS_ADDR", "LARKAX_REDIS_PASSWORD", "LARKAX_REDIS_DB",
		"LARKAX_CREDENTIAL_MASTER_KEY", "LARKAX_SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN":                "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_CREDENTIAL_MASTER_KEY": validKey,
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() 出错: %v", err)
	}

	if cfg.Env != "development" {
		t.Errorf("Env = %q，期望 development", cfg.Env)
	}
	if cfg.Role != RoleGateway {
		t.Errorf("Role = %q，期望 gateway", cfg.Role)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q，期望 :8080", cfg.HTTPAddr)
	}
	if cfg.DB.MaxConns != 20 {
		t.Errorf("DB.MaxConns = %d，期望 20", cfg.DB.MaxConns)
	}
	if cfg.DB.MinConns != 2 {
		t.Errorf("DB.MinConns = %d，期望 2", cfg.DB.MinConns)
	}
	if cfg.DB.ConnLifetime != time.Hour {
		t.Errorf("DB.ConnLifetime = %v，期望 1h", cfg.DB.ConnLifetime)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v，期望 30s", cfg.ShutdownTimeout)
	}
	if len(cfg.Secret.CredentialMasterKey) != KeySize {
		t.Errorf("主密钥长度 = %d，期望 %d", len(cfg.Secret.CredentialMasterKey), KeySize)
	}
	if cfg.IsProduction() {
		t.Error("development 环境下 IsProduction() 应为 false")
	}
}

func TestLoadMissingDSN(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{"LARKAX_CREDENTIAL_MASTER_KEY": validKey})

	_, err := Load()
	if err == nil {
		t.Fatal("缺少 LARKAX_DB_DSN 时应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_DB_DSN") {
		t.Errorf("错误信息未提及缺失的变量: %v", err)
	}
}

func TestLoadInvalidRole(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN":                "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_ROLE":                  "router",
		"LARKAX_CREDENTIAL_MASTER_KEY": validKey,
	})

	_, err := Load()
	if err == nil {
		t.Fatal("非法角色应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_ROLE") {
		t.Errorf("错误信息未提及 LARKAX_ROLE: %v", err)
	}
}

// worker 角色不碰渠道凭证，不应要求主密钥。
func TestLoadWorkerDoesNotRequireMasterKey(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN": "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_ROLE":   "worker",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("worker 角色不应要求主密钥，却报错: %v", err)
	}
	if len(cfg.Secret.CredentialMasterKey) != 0 {
		t.Error("未提供主密钥时不应有值")
	}
}

func TestLoadGatewayRequiresMasterKey(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN": "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_ROLE":   "gateway",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("gateway 角色缺少主密钥时应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_CREDENTIAL_MASTER_KEY") {
		t.Errorf("错误信息未提及主密钥: %v", err)
	}
}

func TestLoadBadMasterKeyLength(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN":                "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_CREDENTIAL_MASTER_KEY": base64.StdEncoding.EncodeToString([]byte("too-short")),
	})

	_, err := Load()
	if err == nil {
		t.Fatal("主密钥长度不足时应报错")
	}
	if !strings.Contains(err.Error(), "字节") {
		t.Errorf("错误信息应说明长度不符: %v", err)
	}
}

func TestLoadBadMasterKeyEncoding(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN":                "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_CREDENTIAL_MASTER_KEY": "not base64 at all!!",
	})

	if _, err := Load(); err == nil {
		t.Fatal("非法 base64 应报错")
	}
}

// 不同工具生成的 base64 字母表与填充习惯不同，四种写法都要接受。
func TestDecodeKeyAcceptsCommonEncodings(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 字节
	if len(raw) != KeySize {
		t.Fatalf("测试样本长度 %d，需要 %d", len(raw), KeySize)
	}

	encodings := map[string]*base64.Encoding{
		"标准带填充":   base64.StdEncoding,
		"标准无填充":   base64.RawStdEncoding,
		"URL 带填充": base64.URLEncoding,
		"URL 无填充": base64.RawURLEncoding,
	}
	for name, enc := range encodings {
		t.Run(name, func(t *testing.T) {
			key, err := decodeKey(enc.EncodeToString(raw))
			if err != nil {
				t.Fatalf("decodeKey 出错: %v", err)
			}
			if string(key) != string(raw) {
				t.Error("解码结果与原文不符")
			}
		})
	}
}

// 密钥值绝不能出现在 fmt 输出里，否则会随日志泄露。
func TestSecretStringRedacts(t *testing.T) {
	s := Secret{CredentialMasterKey: []byte("super-secret-key-material-32byte")}

	if got := s.String(); strings.Contains(got, "super-secret") {
		t.Errorf("String() 泄露了密钥内容: %s", got)
	}
	if got := (&Secret{}).String(); !strings.Contains(got, "unset") {
		t.Errorf("未设置时应显示 unset，实际为 %s", got)
	}
}

func TestLoadRejectsInvalidNumbers(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "连接数不是整数",
			env:  map[string]string{"LARKAX_DB_MAX_CONNS": "many"},
			want: "LARKAX_DB_MAX_CONNS",
		},
		{
			name: "连接数为零",
			env:  map[string]string{"LARKAX_DB_MAX_CONNS": "0"},
			want: "LARKAX_DB_MAX_CONNS",
		},
		{
			name: "最小连接数超过最大",
			env:  map[string]string{"LARKAX_DB_MAX_CONNS": "5", "LARKAX_DB_MIN_CONNS": "10"},
			want: "LARKAX_DB_MIN_CONNS",
		},
		{
			name: "时长为负",
			env:  map[string]string{"LARKAX_SHUTDOWN_TIMEOUT": "-5s"},
			want: "LARKAX_SHUTDOWN_TIMEOUT",
		},
		{
			name: "时长不是时长",
			env:  map[string]string{"LARKAX_DB_CONN_LIFETIME": "一小时"},
			want: "LARKAX_DB_CONN_LIFETIME",
		},
		{
			name: "Redis DB 不是整数",
			env:  map[string]string{"LARKAX_REDIS_DB": "first"},
			want: "LARKAX_REDIS_DB",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAll(t)
			base := map[string]string{
				"LARKAX_DB_DSN":                "postgres://user:placeholder@localhost:5432/larkax",
				"LARKAX_CREDENTIAL_MASTER_KEY": validKey,
			}
			for k, v := range tc.env {
				base[k] = v
			}
			setEnv(t, base)

			_, err := Load()
			if err == nil {
				t.Fatal("应报错但成功了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息未提及 %s: %v", tc.want, err)
			}
		})
	}
}

// 多个问题应一次性全部报出，而不是修一个发现一个。
func TestLoadReportsAllErrorsAtOnce(t *testing.T) {
	clearAll(t)
	setEnv(t, map[string]string{
		"LARKAX_ROLE":         "bogus",
		"LARKAX_DB_MAX_CONNS": "0",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	for _, want := range []string{"LARKAX_ROLE", "LARKAX_DB_DSN", "LARKAX_DB_MAX_CONNS", "LARKAX_CREDENTIAL_MASTER_KEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息未包含 %s:\n%s", want, msg)
		}
	}
}
