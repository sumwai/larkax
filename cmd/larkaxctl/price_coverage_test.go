package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/pricing"
)

// fullTokenExpr 是一条把五个 token 计价项都写上单价的规则。
const fullTokenExpr = `{
  "prompt": 0.0000015, "completion": 0.0000075,
  "cache_read": 0.0000001, "cache_write": 0.0000002,
  "reasoning": 0.000003
}`

// A：五项齐全时不产生提示。
func TestPriceCoverageHintFullCoverage(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(fullTokenExpr)
	if err != nil {
		t.Fatalf("表达式应当能编译：%v", err)
	}
	if hint := priceCoverageHint("这条规则", missing); hint != "" {
		t.Errorf("五项都写了，不该有提示，得到：\n%s", hint)
	}
}

// B：只写了两项的常量表，逐字点名缺的三项。
func TestPriceCoverageHintNamesMissingItems(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(testExpr)
	if err != nil {
		t.Fatalf("表达式应当能编译：%v", err)
	}
	hint := priceCoverageHint("这条规则", missing)

	// 先断言那三个名字逐字出现：换一种缺项算法时，红的第一条就是它。
	if !strings.Contains(hint, "cache_read、cache_write、reasoning") {
		t.Errorf("提示里没有逐字写出缺的三个计价项：\n%s", hint)
	}
	const want = `提示：这条规则没有覆盖 3 个计价项的单价：cache_read、cache_write、reasoning。
上游报了这几项用量时，这一笔的结算会失败：非流式请求，调用方拿到 500；
流式请求的流已经推出去，失败只落在服务端日志里（「用量里有规则未定价的计量项」）。
用不到就把单价显式写成 0——写 0 是「免费」，与「没写」不是一回事。
`
	if hint != want {
		t.Errorf("提示文案变了：\n得到：\n%s期望：\n%s", hint, want)
	}
}

// C：没有用量变量的常量表，写了的键照样算覆盖。
//
// 按「源码里出现过的标识符」判会把 prompt 报成缺项；这条常量表正是要
// 钉住「按键判」的地方。
func TestPriceCoverageHintConstTableCoversKeys(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(`{"prompt": 0.0000015}`)
	if err != nil {
		t.Fatalf("表达式应当能编译：%v", err)
	}
	want := []string{"completion", "cache_read", "cache_write", "reasoning"}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Errorf("缺项 = %v，期望 %v（prompt 已经写了，不该出现在缺项里）", missing, want)
	}
}

// D：用量变量只出现在条件里，prompt 仍然算覆盖。
func TestPriceCoverageHintConditionVariableKeepsSlot(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(`{"prompt": p > 0 ? 1.5 : 0}`)
	if err != nil {
		t.Fatalf("表达式应当能编译：%v", err)
	}
	want := []string{"completion", "cache_read", "cache_write", "reasoning"}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Errorf("缺项 = %v，期望 %v", missing, want)
	}
}

// D2：条件里引用了 c，不代表 completion 被定价。
//
// 覆盖看的是结果表里出现过的键：这条规则的结果表只有 prompt，所以
// completion 仍然缺，而 prompt 不能出现在缺项里。
func TestPriceCoverageHintConditionVariableDoesNotPriceOtherSlot(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(`{"prompt": c > 100 ? 1.5 : 0}`)
	if err != nil {
		t.Fatalf("表达式应当能编译：%v", err)
	}
	for _, name := range missing {
		if name == "prompt" {
			t.Errorf("prompt 定了价，缺项里不能有它：得到 %v", missing)
		}
	}
	hasCompletion := false
	for _, name := range missing {
		if name == "completion" {
			hasCompletion = true
		}
	}
	if !hasCompletion {
		t.Errorf("条件里出现的 c 不代表 completion 定了价，缺项里必须有它：得到 %v", missing)
	}
}

// E：fixed / minimum / units 不是 token 计价项，既不补缺项也不挡判定。
func TestPriceCoverageHintIgnoresNonTokenKeys(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(`{
		"prompt": 0.0000015, "fixed": 0.01, "minimum": 0.1,
		"units": {"image": 0.02}
	}`)
	if err != nil {
		t.Fatalf("表达式应当能编译：%v", err)
	}
	want := []string{"completion", "cache_read", "cache_write", "reasoning"}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Errorf("缺项 = %v，期望 %v", missing, want)
	}
}

// G：分支键集不一致的规则在编译期就被拒，覆盖提示不会按并集给出一份
// 看起来齐全的结论。
//
// 这条原先钉的是「键取所有分支的并集，分支各自返回不同的表时两项都算覆盖」。
// 现在那种写法编不过了（结算只认命中那一支的键，按并集看覆盖齐全正是要消灭
// 的假象），所以这里改成钉住新的分界：提示路径拿到的是编译错误，错误里点名
// 那个只在部分分支出现的键。
func TestPriceCoverageHintRejectsMismatchedBranchKeys(t *testing.T) {
	missing, err := pricing.UncoveredTokenItems(
		`c > 100 ? {"prompt": 1.5, "completion": 7.5} : {"prompt": 1.5}`)
	if err == nil {
		t.Fatalf("分支键集不一致的规则应当编译失败，得到缺项 %v", missing)
	}
	if missing != nil {
		t.Fatalf("报错时不该给出缺项，得到 %v", missing)
	}
	if !strings.Contains(err.Error(), `"completion"`) {
		t.Errorf("错误里应当点名那个只在部分分支出现的键：%v", err)
	}
}

// F：-q 之下提示一个字都不出。
func TestPriceCoverageHintQuietModeIsSilent(t *testing.T) {
	saved := cli.QuietOutput
	cli.QuietOutput = true
	t.Cleanup(func() { cli.QuietOutput = saved })

	_, stderr := captureStreams(t, func() {
		printPriceCoverageHint(cli.HumanStderr(), testExpr, nil)
	})
	if stderr != "" {
		t.Errorf("-q 之下提示应当静默，得到 %q", stderr)
	}
}

// H1：create 成功后 stdout 仍然只有规则 ID，提示落在 stderr 上。
func TestPriceCreatePrintsCoverageHint(t *testing.T) {
	testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
			"--models", uniqueName("model"), "--by", by,
		})
	})
	if err != nil {
		t.Fatalf("create 失败：%v", err)
	}

	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout 该只有规则 ID 一行，得到 %q", stdout)
	}
	if _, parseErr := strconv.ParseInt(lines[0], 10, 64); parseErr != nil {
		t.Errorf("stdout 该是规则 ID，得到 %q（%v）", stdout, parseErr)
	}
	for _, want := range []string{"提示：", "cache_read", "cache_write", "reasoning"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr 里没有 %q：\n%s", want, stderr)
		}
	}
}

// H2：五项齐全的表达式不产生提示。
func TestPriceCreateFullCoveragePrintsNoHint(t *testing.T) {
	testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	var err error
	_, stderr := captureStreams(t, func() {
		err = runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", fullTokenExpr,
			"--models", uniqueName("model"), "--by", by,
		})
	})
	if err != nil {
		t.Fatalf("create 失败：%v", err)
	}
	if strings.Contains(stderr, "提示：") {
		t.Errorf("五项都写了，不该有提示：\n%s", stderr)
	}
}

// H3：定价表的 base 缺项时提示，补齐后不提示。
func TestPriceCreateSchedulePrintsCoverageHint(t *testing.T) {
	testDB(t)
	setupEnv(t)

	// cliSchedule 的 base 只有 prompt / completion。
	by := createTestUserID(t)
	var err error
	_, stderr := captureStreams(t, func() {
		err = runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"),
			"--schedule-file", writeScheduleFile(t, cliSchedule),
			"--models", uniqueName("model"), "--by", by,
		})
	})
	if err != nil {
		t.Fatalf("create 失败：%v", err)
	}
	if !strings.Contains(stderr, "这张定价表的 base 没有覆盖 3 个计价项的单价") {
		t.Errorf("stderr 里没有缺项提示：\n%s", stderr)
	}
	for _, want := range []string{"cache_read", "cache_write", "reasoning"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr 里没有 %q：\n%s", want, stderr)
		}
	}

	_, stderr = captureStreams(t, func() {
		err = runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"),
			"--schedule-file", writeScheduleFile(t, fullBaseSchedule),
			"--models", uniqueName("model"), "--by", by,
		})
	})
	if err != nil {
		t.Fatalf("create 失败：%v", err)
	}
	if strings.Contains(stderr, "提示：") {
		t.Errorf("base 五项都写了，不该有提示：\n%s", stderr)
	}
}

// fullBaseSchedule 与 cliSchedule 同形，只是 base 补齐了五项。
const fullBaseSchedule = `{
  "tz": "Asia/Shanghai",
  "base": {
    "prompt": 0.0000015, "completion": 0.0000075,
    "cache_read": 0.0000001, "cache_write": 0.0000002,
    "reasoning": 0.000003
  },
  "overrides": [ { "weekdays": ["fri"], "prompt": 0.000001 } ]
}`

// H4：update 只在这次写了计价时提示；只改优先级不提示。
func TestPriceUpdatePrintsHintOnlyWhenPricingChanges(t *testing.T) {
	testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
		"--models", uniqueName("model"),
	})
	idArg := strconv.FormatInt(id, 10)

	var err error
	_, stderr := captureStreams(t, func() {
		err = runPriceUpdate([]string{"--id", idArg, "--expr", testExpr, "--by", by})
	})
	if err != nil {
		t.Fatalf("update 失败：%v", err)
	}
	for _, want := range []string{"提示：", "cache_read", "cache_write", "reasoning"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr 里没有 %q：\n%s", want, stderr)
		}
	}

	_, stderr = captureStreams(t, func() {
		err = runPriceUpdate([]string{"--id", idArg, "--priority", "9", "--by", by})
	})
	if err != nil {
		t.Fatalf("update 失败：%v", err)
	}
	if strings.Contains(stderr, "提示：") {
		t.Errorf("这次没写计价，不该有提示：\n%s", stderr)
	}
}

// H5：-q 之下 create 的提示也不打。
func TestPriceCreateQuietSuppressesCoverageHint(t *testing.T) {
	testDB(t)
	setupEnv(t)

	// 先建操作人再切安静模式：--by 要解析成一个真的存在的用户，而那条命令
	// 把自己的 ID 打在 stdout 上，切了之后就读不到了。
	by := createTestUserID(t)
	saved := cli.QuietOutput
	cli.QuietOutput = true
	t.Cleanup(func() { cli.QuietOutput = saved })

	var err error
	_, stderr := captureStreams(t, func() {
		err = runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
			"--models", uniqueName("model"), "--by", by,
		})
	})
	if err != nil {
		t.Fatalf("create 失败：%v", err)
	}
	if stderr != "" {
		t.Errorf("-q 之下 stderr 该是空的，得到 %q", stderr)
	}
}
