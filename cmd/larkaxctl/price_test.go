package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/pricing"
)

// testExpr 是一条能编译过的计价表达式：一组单价。
//
// 表达式在写入前会被编译一次，编译不过的命令直接失败——所以用例里用的这条
// 必须是合法的，否则测的就不是命令那一层了。
const testExpr = `{"prompt": 0.0000015, "completion": 0.0000075}`

// TestPriceAddRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestPriceAddRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --scope", []string{"--name", "n1", "--expr", testExpr}, "缺少 --scope"},
		{"没有 --name", []string{"--scope", "platform", "--expr", testExpr}, "缺少 --name"},
		{"没有 --expr", []string{"--scope", "platform", "--name", "n1"}, "缺少 --expr"},
		{"--channel 是负数", []string{
			"--scope", "channel", "--name", "n1", "--expr", testExpr, "--channel", "-1",
		}, "无效"},
		{"--effective-from 不是 RFC3339", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr,
			"--effective-from", "2026-01-01",
		}, "RFC3339"},
		{"多出位置参数", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr, "extra",
		}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPriceAdd(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestPriceAddThenList 钉住计价规则的写入与列举。
func TestPriceAddThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("rule")
	model := uniqueName("model")

	stdout, _ := captureStreams(t, func() {
		if err := runPriceAdd([]string{
			"--scope", "platform", "--name", name, "--expr", testExpr,
			"--models", model, "--priority", "7",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是规则 ID：%q（%v）", stdout, err)
	}

	rules, err := pricing.NewAdmin(db).List(context.Background())
	if err != nil {
		t.Fatalf("列举规则失败: %v", err)
	}
	got := findRule(t, rules, id)
	if got.Name != name {
		t.Errorf("规则名 = %q，期望 %q", got.Name, name)
	}
	if got.Scope != pricing.ScopePlatform {
		t.Errorf("作用域 = %q，期望 %q", got.Scope, pricing.ScopePlatform)
	}
	if got.Priority != 7 {
		t.Errorf("优先级 = %d，期望 7", got.Priority)
	}
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("模型清单 = %v，期望 [%q]", got.ModelPatterns, model)
	}
	if !got.Enabled {
		t.Error("没给 --disabled 时规则应当是启用的")
	}
	if got.ExprErr != nil {
		t.Errorf("表达式应当能编译：%v", got.ExprErr)
	}
	if len(got.DeclaredKeys) != 2 || got.DeclaredKeys[0] != "completion" || got.DeclaredKeys[1] != "prompt" {
		t.Errorf("声明的单价键 = %v，期望 [completion prompt]", got.DeclaredKeys)
	}

	listOut, _ := captureStreams(t, func() {
		if listErr := runPriceList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	for _, want := range []string{name, model, "platform", "启用", "completion,prompt"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("清单里没有 %q：\n%s", want, listOut)
		}
	}
}

// TestPriceAddDisabledAndUnlimited 钉住两个「零值不是默认值」的地方。
//
// --disabled 是「明确要一条停用的规则」，不给它时是启用；--models 留空是
// 「适用于全部模型」（兜底规则），与 channels 的 models 正好相反。
// 这两处都是「没填」与「填了」必须分得开的地方。
func TestPriceAddDisabledAndUnlimited(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("rule")
	stdout, _ := captureStreams(t, func() {
		if err := runPriceAdd([]string{
			"--scope", "platform", "--name", name, "--expr", testExpr, "--disabled",
		}); err != nil {
			t.Fatalf("add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是规则 ID：%q（%v）", stdout, err)
	}

	rules, err := pricing.NewAdmin(db).List(context.Background())
	if err != nil {
		t.Fatalf("列举规则失败: %v", err)
	}
	got := findRule(t, rules, id)
	if got.Enabled {
		t.Error("给了 --disabled 的规则应当是停用的")
	}
	if len(got.ModelPatterns) != 0 {
		t.Errorf("没给 --models 时应当适用于全部模型（空清单），得到 %v", got.ModelPatterns)
	}
}

// TestPriceAddRejectsBadExpression 钉住「编译不过的表达式进不了库」。
//
// 一条编译不过的规则进了库，表现是每个用到它的请求都失败，而失败点在请求
// 路径上——排查的人会先去怀疑请求。所以它必须被写入路径当场拒掉。
func TestPriceAddRejectsBadExpression(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runPriceAdd([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", "p * ",
		})
	})
	if err == nil {
		t.Fatal("编译不过的表达式应当被拒")
	}
	if !strings.Contains(err.Error(), "编译失败") {
		t.Errorf("错误信息应当指出是编译不过，得到 %v", err)
	}
}

// TestPriceAddRejectsWildcardModel 钉住通配符模型名。
//
// 读取路径按元素逐字匹配，`claude-*` 只是一个普通字符串——配了通配的规则
// 一条都匹配不上，而它不报错，只是静默退到别的价上。
func TestPriceAddRejectsWildcardModel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runPriceAdd([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
			"--models", "claude-*",
		})
	})
	if err == nil {
		t.Fatal("带通配符的模型名应当被拒")
	}
	if !strings.Contains(err.Error(), "通配符") {
		t.Errorf("错误信息应当指出通配符的问题，得到 %v", err)
	}
}

func findRule(t *testing.T, rules []pricing.RuleInfo, id int64) pricing.RuleInfo {
	t.Helper()
	for _, rule := range rules {
		if rule.ID == id {
			return rule
		}
	}
	t.Fatalf("清单里没有规则 %d", id)
	return pricing.RuleInfo{}
}
