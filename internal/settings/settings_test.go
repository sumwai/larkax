package settings

import (
	"context"
	"strings"
	"testing"
)

// fakeSource 让校验逻辑脱离数据库测试：出错分支（缺键、类型不对、
// 非法 JSON、未声明的键）恰好是最需要覆盖的部分，而它们在真库上很难构造。
type fakeSource struct {
	rows []Row
	err  error
}

func (f *fakeSource) Load(context.Context) ([]Row, error) { return f.rows, f.err }

// fullRows 是为了让 Registry 里每一项都存在而构造的完整行集。
// 它的值类型必须与 Registry 声明一致，否则 Load 会报错。
func fullRows() []Row {
	return []Row{
		{Key: "request_snapshot.retention_days", Value: []byte(`30`)},
		{Key: "request_snapshot.enabled", Value: []byte(`true`)},
		{Key: "request_snapshot.max_bytes", Value: []byte(`65536`)},
		{Key: "capture.consent_max_window_hours", Value: []byte(`24`)},
		{Key: "quota.probe_interval_seconds", Value: []byte(`300`)},
		{Key: "billing.default_asset", Value: []byte(`"CNY"`)},
		{Key: "calendar.default_region", Value: []byte(`"CN"`)},
		{Key: "idempotency.retention_hours", Value: []byte(`24`)},
		{Key: "plugin.enabled", Value: []byte(`false`)},
		{Key: "plugin.call_timeout_ms", Value: []byte(`5000`)},
		{Key: "plugin.memory_limit_bytes", Value: []byte(`67108864`)},
		{Key: "plugin.max_payload_bytes", Value: []byte(`4194304`)},
		{Key: "reconcile.auto_repair", Value: []byte(`false`)},
		{Key: "reconcile.repair_mode", Value: []byte(`"resync_snapshot"`)},
		{Key: "reconcile.difference_account", Value: []byte(`"reconcile_difference"`)},
		{Key: "reconcile.shard_size", Value: []byte(`1000`)},
		{Key: "reconcile.full_scan_interval_hours", Value: []byte(`168`)},
	}
}

func TestLoadAndRead(t *testing.T) {
	v, err := Load(context.Background(), &fakeSource{rows: fullRows()})
	if err != nil {
		t.Fatalf("Load 出错: %v", err)
	}

	if n, err := v.Int("request_snapshot.retention_days"); err != nil || n != 30 {
		t.Errorf("retention_days = %d, %v；期望 30", n, err)
	}
	if b, err := v.Bool("request_snapshot.enabled"); err != nil || !b {
		t.Errorf("enabled = %v, %v；期望 true", b, err)
	}
	if s, err := v.String("billing.default_asset"); err != nil || s != "CNY" {
		t.Errorf("default_asset = %q, %v；期望 CNY", s, err)
	}
}

func TestKeysSorted(t *testing.T) {
	v, err := Load(context.Background(), &fakeSource{rows: fullRows()})
	if err != nil {
		t.Fatalf("Load 出错: %v", err)
	}

	keys := v.Keys()
	if len(keys) != len(Registry) {
		t.Fatalf("Keys() 返回 %d 项，期望 %d", len(keys), len(Registry))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Errorf("Keys() 未排序：%q 在 %q 之前", keys[i-1], keys[i])
		}
	}
}

// 库里出现 Registry 未声明的键时必须报错。
// 否则写错键名会安静地生效为一个不存在的配置，改动看起来做了，其实没做。
func TestLoadRejectsUndeclaredKey(t *testing.T) {
	rows := append(fullRows(), Row{Key: "typo.retentoin_days", Value: []byte(`7`)})

	_, err := Load(context.Background(), &fakeSource{rows: rows})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "typo.retentoin_days") {
		t.Errorf("错误信息未指出是哪个键：%v", err)
	}
}

// Registry 里有而库里没有，同样是配置脱节，必须报出来。
//
// 按**键名**删一行，而不是砍掉最后一行：后者在往 Registry 末尾加配置项时
// 会悄悄换掉被测的那个键，于是失败信息指向一个与改动无关的键名。
func TestLoadRejectsMissingKey(t *testing.T) {
	const missing = "calendar.default_region"
	rows := make([]Row, 0, len(fullRows()))
	for _, r := range fullRows() {
		if r.Key == missing {
			continue
		}
		rows = append(rows, r)
	}

	_, err := Load(context.Background(), &fakeSource{rows: rows})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误信息未指出缺哪个键：%v", err)
	}
}

// 类型写错要在启动时发现，而不是第一次读到它时。
func TestLoadRejectsWrongType(t *testing.T) {
	rows := fullRows()
	for i := range rows {
		if rows[i].Key == "request_snapshot.retention_days" {
			rows[i].Value = []byte(`"三十天"`)
		}
	}

	_, err := Load(context.Background(), &fakeSource{rows: rows})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "retention_days") {
		t.Errorf("错误信息未指出是哪个键：%v", err)
	}
}

// 字符串必须写成带引号的 JSON。写成裸的 CNY 是常见笔误，
// 报错时要提示这一点，否则使用者只会看到一句 json 语法错。
func TestStringWithoutQuotesExplains(t *testing.T) {
	rows := fullRows()
	for i := range rows {
		if rows[i].Key == "billing.default_asset" {
			rows[i].Value = []byte(`CNY`)
		}
	}

	_, err := Load(context.Background(), &fakeSource{rows: rows})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "引号") {
		t.Errorf("错误信息未提示需要引号：%v", err)
	}
}

func TestLoadPropagatesSourceError(t *testing.T) {
	_, err := Load(context.Background(), &fakeSource{err: errBoom})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("原始错误被吞掉了：%v", err)
	}
}

// 未声明的键即使不经过 Load 也不该被读到。
func TestLookupRejectsUndeclaredAndWrongKind(t *testing.T) {
	v, err := Load(context.Background(), &fakeSource{rows: fullRows()})
	if err != nil {
		t.Fatalf("Load 出错: %v", err)
	}

	if _, err := v.Int("nope"); err == nil {
		t.Error("未声明的键应报错")
	}
	// 声明为 bool 的项按 int 读，是调用方的错，必须报出来而不是猜
	if _, err := v.Int("request_snapshot.enabled"); err == nil {
		t.Error("类型不匹配应报错")
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

// 重复声明会让后一条安静地覆盖前一条，而两处代码看起来都「写了」。
func TestIndexRejectsDuplicateKey(t *testing.T) {
	saved := Registry
	t.Cleanup(func() { Registry = saved })
	Registry = append(append([]Spec{}, saved...), saved[0])

	if _, err := index(); err == nil {
		t.Fatal("重复声明的键应报错")
	}
}

func TestIndexRejectsEmptyKey(t *testing.T) {
	saved := Registry
	t.Cleanup(func() { Registry = saved })
	Registry = append(append([]Spec{}, saved...), Spec{Key: "", Kind: KindInt})

	if _, err := index(); err == nil {
		t.Fatal("空键名应报错")
	}
}
