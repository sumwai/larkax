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
		{Key: "quota.exhausted_cooldown_seconds", Value: []byte(`900`)},
		{Key: "billing.default_asset", Value: []byte(`"CNY"`)},
		{Key: "billing.max_output_tokens", Value: []byte(`65536`)},
		{Key: "billing.stream_freeze_step", Value: []byte(`4096`)},
		{Key: "billing.merchant_share_bp", Value: []byte(`0`)},
		{Key: "calendar.default_region", Value: []byte(`"CN"`)},
		{Key: "idempotency.retention_hours", Value: []byte(`24`)},
		{Key: "console.session_retention_days", Value: []byte(`30`)},
		{Key: "plugin.enabled", Value: []byte(`false`)},
		{Key: "plugin.call_timeout_ms", Value: []byte(`5000`)},
		{Key: "plugin.memory_limit_bytes", Value: []byte(`67108864`)},
		{Key: "plugin.max_payload_bytes", Value: []byte(`4194304`)},
		{Key: "reconcile.auto_repair", Value: []byte(`false`)},
		{Key: "reconcile.repair_mode", Value: []byte(`"resync_snapshot"`)},
		{Key: "reconcile.difference_account", Value: []byte(`"reconcile_difference"`)},
		{Key: "reconcile.shard_size", Value: []byte(`1000`)},
		{Key: "reconcile.full_scan_interval_hours", Value: []byte(`168`)},
		{Key: "payment.fake_enabled", Value: []byte(`false`)},
		{Key: "plans.charge_order", Value: []byte(`"plan_first"`)},
		{Key: "plans.carryover_bp", Value: []byte(`4000`)},
		{Key: "admin.token_sha256", Value: []byte(`""`)},
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

// 未接线的键是一份**显式清单**。
//
// 这一条钉的是清单本身，不是「谁真的没有读者」——那件事没有自动判据
// （见 Spec.Unwired 的注释）。它失败只有两种原因，两种都该看一眼：
// 有人接线之后忘了把 Unwired 清掉（那一行会继续说着不成立的话），
// 或者有人新加了一个还没接线的键却忘了标——后者正是这个清单要防的事：
// 一个没人读的键，在命令行上和一个真的生效的键长得一模一样。
func TestUnwiredKeysAreDeclared(t *testing.T) {
	// 与 Registry 里的 Unwired 逐字对应。改这里之前先确认对应的那条线
	// 真的接上了（或真的还没接）。
	//
	// 四个 plugin.* 曾经在这份清单里，接线之后删掉了——它们的读者在
	// internal/plugin 的 LoadConfig，装配用例钉着「配错会让装配失败」。
	// capture.consent_max_window_hours 是最后一个，它的读者是
	// `larkaxctl consent grant`（申请时长上限），所以这份清单今天是空的。
	//
	// 空清单不等于这道门没用了：下面那两圈仍然在跑——谁新标一个未接线的键，
	// 就得先把它写进这里，否则第二圈当场报出来。
	want := map[string]string{}

	got := map[string]string{}
	for _, s := range Registry {
		if s.Unwired != "" {
			got[s.Key] = s.Unwired
		}
	}

	for key, note := range want {
		switch {
		case got[key] == "":
			t.Errorf("settings.%s 的未接线说明没了：接上了就把它从这份清单里删掉，"+
				"没接上就补回来", key)
		case got[key] != note:
			t.Errorf("settings.%s 的未接线说明 = %q，期望 %q", key, got[key], note)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("settings.%s 被标成了未接线，但不在清单里：新标一个键要有理由，"+
				"写进这份清单才算数", key)
		}
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
