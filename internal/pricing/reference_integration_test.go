package pricing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
)

// 一条平台规则取不出价，只跳过命中它的模型，其余模型照常给参考价。
//
// 这是 #952 的判据：某个模型配错一条平台规则，不该让整页模型清单都打不开。
// 跳过要留一条日志（规则 id + 原因），否则外部只剩下「这个模型没有参考价」，
// 而那与「没有平台规则」长得一模一样。
func TestReferencePricesSkipBrokenRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	good := unique("ref-good")
	broken := unique("ref-broken")

	insertRule(t, ctx, db, ruleSpec{patterns: []string{good}, expr: `{"prompt": 0.000002}`})
	// 绕过写入路径直接插一条编译不过的规则：Create 会先编译，所以这类行
	// 只能来自历史数据、直接写库，或编译器收紧之后的旧表达式。
	brokenID := insertRule(t, ctx, db, ruleSpec{patterns: []string{broken}, expr: `{"prompt": notAThing + }`})

	logs := captureReferenceLogs(t)

	got, err := NewReferences(db, "Asia/Shanghai", nil).ReferencePrices(ctx,
		[]string{good, broken}, time.Now().UTC())
	if err != nil {
		t.Fatalf("一条规则取不出价就让整次取参考价失败: %v", err)
	}

	price, ok := got[good]
	if !ok || price == nil {
		t.Fatalf("正常模型的参考价丢了：%+v", got)
	}
	if want := money.MustParse("2.00000000"); price.InputPerMTok != want {
		t.Errorf("输入价 = %s，期望 %s（每百万 token）", price.InputPerMTok, want)
	}
	if _, ok := got[broken]; ok {
		t.Errorf("坏规则的模型拿到了参考价：%+v", got[broken])
	}

	logged := logs.String()
	if !strings.Contains(logged, "rule_id="+strconv.FormatInt(brokenID, 10)) {
		t.Errorf("日志里没有规则 id %d：%s", brokenID, logged)
	}
	if !strings.Contains(logged, "规则取价失败") {
		t.Errorf("日志里没有取价失败的说明：%s", logged)
	}
	// 表达式全文可能很长，日志只该留错误的第一行。
	if strings.Contains(logged, "notAThing") {
		t.Errorf("日志里带上了表达式全文：%s", logged)
	}
}

// 全部模型都取不出参考价时返回空 map 而不是错误：模型清单仍然列得出来，
// 只是每一项的参考价为 null。
func TestReferencePricesAllBrokenYieldEmptyMap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	first := unique("ref-broken-a")
	second := unique("ref-broken-b")

	firstID := insertRule(t, ctx, db, ruleSpec{patterns: []string{first}, expr: `{"prompt": notAThing + }`})
	secondID := insertRule(t, ctx, db, ruleSpec{patterns: []string{second}, expr: `{"prompt": "x" + }`})

	logs := captureReferenceLogs(t)

	got, err := NewReferences(db, "Asia/Shanghai", nil).ReferencePrices(ctx,
		[]string{first, second}, time.Now().UTC())
	if err != nil {
		t.Fatalf("全部模型都取不出参考价时返回了错误: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("参考价 = %+v，期望非 nil 的空 map", got)
	}

	logged := logs.String()
	if n := strings.Count(logged, "规则取价失败"); n != 2 {
		t.Errorf("日志条数 = %d，期望 2（每个模型一条）：%s", n, logged)
	}
	for _, id := range []int64{firstID, secondID} {
		if !strings.Contains(logged, "rule_id="+strconv.FormatInt(id, 10)) {
			t.Errorf("日志里没有规则 id %d：%s", id, logged)
		}
	}
	if strings.Contains(logged, "notAThing") {
		t.Errorf("日志里带上了表达式全文：%s", logged)
	}
}

// 规则取不出价只跳过那个模型；别的失败（查询、扫描）仍让整次取价失败。
//
// 这一条不连库：把取价函数换成替身，分界本身才钉得住——集成用例只能证明
// 「编译失败被归成了规则失败」，证明不了「连库失败没有被一起吞掉」。
func TestCollectPricesSkipsRuleFailures(t *testing.T) {
	ctx := context.Background()
	logs := captureReferenceLogs(t)

	got, err := collectPrices(ctx, []string{"broken", "good"}, time.Now().UTC(),
		func(_ context.Context, model string, _ time.Time) (*ReferencePrice, error) {
			if model == "broken" {
				return nil, &rulePriceError{ruleID: 42,
					err: errors.New("编译模型 \"broken\" 的平台计价规则失败: 第 1 行第 5 列\n{\"prompt\": nope}")}
			}
			return &ReferencePrice{InputPerMTok: money.MustParse("1.00000000")}, nil
		})
	if err != nil {
		t.Fatalf("规则取不出价让整次取价失败: %v", err)
	}
	if len(got) != 1 || got["good"] == nil {
		t.Errorf("参考价 = %+v，期望只有 good", got)
	}

	logged := logs.String()
	if !strings.Contains(logged, "rule_id=42") || !strings.Contains(logged, "规则取价失败") {
		t.Errorf("日志里没有规则 id 与原因：%s", logged)
	}
	if strings.Contains(logged, "nope") {
		t.Errorf("日志里带上了表达式全文：%s", logged)
	}
}

// 非规则失败（查询、扫描）不能被当成「这个模型没有参考价」：它必须原样
// 变成整次取价的错误，否则库连不上时页面上只会看到一排 null。
func TestCollectPricesKeepsQueryFailuresFatal(t *testing.T) {
	queryErr := errors.New("查询模型 \"m\" 的平台计价规则失败: 连接被拒绝")

	got, err := collectPrices(context.Background(), []string{"m"}, time.Now().UTC(),
		func(context.Context, string, time.Time) (*ReferencePrice, error) {
			return nil, queryErr
		})
	if !errors.Is(err, queryErr) {
		t.Fatalf("查询失败被吞掉了：err = %v，期望原样返回", err)
	}
	if got != nil {
		t.Errorf("查询失败时返回了半份结果：%+v", got)
	}
}

// captureReferenceLogs 把这些用例期间打的日志收进缓冲区。
func captureReferenceLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(observe.NewLoggerWithWriter(&buf, "debug", "text", false))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
