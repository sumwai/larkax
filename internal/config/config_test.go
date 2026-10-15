package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// rawKey 是 32 字节的测试密钥明文，解码后即 KeySize 长度。
var rawKey = []byte("0123456789abcdef0123456789abcdef")

func validKeyB64() string {
	return base64.StdEncoding.EncodeToString(rawKey)
}

// setEnv 只设置给定变量并将本包读取的其他变量清空，保证测试之间互不污染。
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range allEnvVars {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

var allEnvVars = []string{
	"LARKAX_ENV", "LARKAX_ROLE", "LARKAX_HTTP_ADDR", "LARKAX_INTERNAL_ADDR",
	"LARKAX_LOG_LEVEL",
	"LARKAX_DB_DSN", "LARKAX_REDIS_DSN", "LARKAX_MASTER_KEYS",
}

// baseEnv 是让 Load 成功的最小变量集合。
func baseEnv(extra map[string]string) map[string]string {
	m := map[string]string{
		"LARKAX_DB_DSN":      "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_MASTER_KEYS": "k1:" + validKeyB64(),
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, baseEnv(nil))

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
	if cfg.InternalAddr != ":9090" {
		t.Errorf("InternalAddr = %q，期望 :9090", cfg.InternalAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q，期望 info", cfg.LogLevel)
	}
	if len(cfg.MasterKeys) != 1 || cfg.MasterKeys[0].ID != "k1" {
		t.Errorf("MasterKeys = %+v，期望一把标识为 k1 的密钥", cfg.MasterKeys)
	}
	if cfg.IsProduction() {
		t.Error("development 环境下 IsProduction() 应为 false")
	}
}

func TestLoadMissingDSN(t *testing.T) {
	setEnv(t, map[string]string{"LARKAX_MASTER_KEYS": validKeyB64()})

	_, err := Load()
	if err == nil {
		t.Fatal("缺少 LARKAX_DB_DSN 时应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_DB_DSN") {
		t.Errorf("错误信息未提及缺失的变量: %v", err)
	}
}

func TestLoadInvalidRole(t *testing.T) {
	setEnv(t, baseEnv(map[string]string{"LARKAX_ROLE": "router"}))

	_, err := Load()
	if err == nil {
		t.Fatal("非法角色应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_ROLE") {
		t.Errorf("错误信息未提及 LARKAX_ROLE: %v", err)
	}
}

// Redis 是可选的。给了就要是合法 DSN，不给就不启用。
func TestLoadRedisDSN(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"未设置则不启用", "", false},
		{"redis 方案", "redis://localhost:6379/0", false},
		{"rediss 方案", "rediss://localhost:6379/0", false},
		{"unix 套接字", "unix:///var/run/redis.sock", false},
		{"缺方案前缀", "localhost:6379", true},
		{"方案写错", "postgres://localhost:5432/redis", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, baseEnv(map[string]string{"LARKAX_REDIS_DSN": tc.dsn}))

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("应报错但成功了")
				}
				if !strings.Contains(err.Error(), "LARKAX_REDIS_DSN") {
					t.Errorf("错误信息未提及 LARKAX_REDIS_DSN: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if cfg.RedisDSN != tc.dsn {
				t.Errorf("RedisDSN = %q，期望 %q", cfg.RedisDSN, tc.dsn)
			}
		})
	}
}

// worker 不碰渠道凭证，不应要求主密钥。
func TestLoadWorkerDoesNotRequireMasterKey(t *testing.T) {
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN": "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_ROLE":   "worker",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("worker 角色不应要求主密钥，却报错: %v", err)
	}
	if len(cfg.MasterKeys) != 0 {
		t.Error("未提供主密钥时不应有值")
	}
}

func TestLoadGatewayRequiresMasterKey(t *testing.T) {
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN": "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_ROLE":   "gateway",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("gateway 角色缺少主密钥时应报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_MASTER_KEYS") {
		t.Errorf("错误信息未提及主密钥: %v", err)
	}
}

func TestParseMasterKeys(t *testing.T) {
	other := base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))

	cases := []struct {
		name    string
		raw     string
		wantIDs []string
		wantErr string
	}{
		{
			name:    "单把带标识",
			raw:     "k1:" + validKeyB64(),
			wantIDs: []string{"k1"},
		},
		{
			name:    "单把不带标识",
			raw:     validKeyB64(),
			wantIDs: []string{UnsetMasterKeyID},
		},
		{
			name:    "多把并存",
			raw:     "old:" + validKeyB64() + ", new:" + other,
			wantIDs: []string{"old", "new"},
		},
		{
			name:    "容忍空白",
			raw:     "  k1 : " + validKeyB64() + "  ",
			wantIDs: []string{"k1"},
		},
		{
			name:    "跳过空段",
			raw:     "k1:" + validKeyB64() + ",,",
			wantIDs: []string{"k1"},
		},
		{
			name:    "空字符串",
			raw:     "",
			wantErr: "未设置",
		},
		{
			name:    "只有逗号",
			raw:     " , , ",
			wantErr: "未解析出任何密钥",
		},
		{
			name:    "标识重复",
			raw:     "k1:" + validKeyB64() + ",k1:" + other,
			wantErr: "重复",
		},
		{
			name:    "标识为空",
			raw:     ":" + validKeyB64(),
			wantErr: "标识不能为空",
		},
		{
			name:    "长度不足",
			raw:     "k1:" + base64.StdEncoding.EncodeToString([]byte("short")),
			wantErr: "字节",
		},
		{
			name:    "不是 base64",
			raw:     "k1:not base64!!",
			wantErr: "base64",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := parseMasterKeys(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("应报错（含 %q）但成功了", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("错误信息未包含 %q: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			var gotIDs []string
			for _, k := range keys {
				gotIDs = append(gotIDs, k.ID)
			}
			if strings.Join(gotIDs, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("标识 = %v，期望 %v", gotIDs, tc.wantIDs)
			}
			for _, k := range keys {
				if len(k.Key) != KeySize {
					t.Errorf("密钥 %q 长度为 %d，期望 %d", k.ID, len(k.Key), KeySize)
				}
			}
		})
	}
}

// 不同工具生成的 base64 字母表与填充习惯不同，四种写法都要接受。
func TestDecodeKeyAcceptsCommonEncodings(t *testing.T) {
	encodings := map[string]*base64.Encoding{
		"标准带填充":     base64.StdEncoding,
		"标准无填充":     base64.RawStdEncoding,
		"URL 安全带填充": base64.URLEncoding,
		"URL 安全无填充": base64.RawURLEncoding,
	}
	for name, enc := range encodings {
		t.Run(name, func(t *testing.T) {
			key, err := decodeKey(enc.EncodeToString(rawKey))
			if err != nil {
				t.Fatalf("decodeKey 出错: %v", err)
			}
			if string(key) != string(rawKey) {
				t.Error("解码结果与原文不符")
			}
		})
	}
}

// PrimaryMasterKey 取列表第一把，用于包装新生成的数据密钥。
func TestPrimaryMasterKey(t *testing.T) {
	setEnv(t, baseEnv(map[string]string{
		"LARKAX_MASTER_KEYS": "new:" + validKeyB64() + ",old:" + validKeyB64(),
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() 出错: %v", err)
	}
	primary, ok := cfg.PrimaryMasterKey()
	if !ok {
		t.Fatal("配置了主密钥却报告没有")
	}
	if primary.ID != "new" {
		t.Errorf("PrimaryMasterKey().ID = %q，期望 new", primary.ID)
	}
}

// worker 角色没有主密钥是正常状态，取值时不应 panic。
func TestPrimaryMasterKeyAbsent(t *testing.T) {
	setEnv(t, map[string]string{
		"LARKAX_DB_DSN": "postgres://user:placeholder@localhost:5432/larkax",
		"LARKAX_ROLE":   "worker",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() 出错: %v", err)
	}
	if _, ok := cfg.PrimaryMasterKey(); ok {
		t.Error("未配置主密钥时应报告没有")
	}
}

// 多个问题应一次性全部报出，而不是修一个发现一个。
func TestLoadReportsAllErrorsAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		"LARKAX_ROLE":        "bogus",
		"LARKAX_REDIS_DSN":   "localhost:6379",
		"LARKAX_MASTER_KEYS": "not base64!!",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	for _, want := range []string{"LARKAX_ROLE", "LARKAX_DB_DSN", "LARKAX_REDIS_DSN", "LARKAX_MASTER_KEYS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息未包含 %s:\n%s", want, msg)
		}
	}
}
