package expr

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
)

// let 的结果（`let x = ...; <结果>`）挂在 AST 的 VariableDeclaratorNode.Expr 上，
// 不在顶层节点里。walkShape 原先只查声明值、不查结果，于是给规则加一个 let
// 前缀，结果形状与用量变量的位置这两类检查就一起失效了——写错的规则照样编得过，
// 问题推迟到求值期才出现。这一组用例覆盖那条路径。

// TestCompileRejectsIllegalLetResult 验证 let 结果里的写法照样按位置检查。
func TestCompileRejectsIllegalLetResult(t *testing.T) {
	cases := []struct {
		name   string
		source string
		// wantShape 为 true 表示这条由形状检查给出（ReasonShape）；
		// 为 false 表示 expr 的类型检查先一步拦住（ReasonSyntax），
		// walkShape 走不到它。
		wantShape bool
		wantMsg   string
	}{
		{
			// 结果不是一张表就拒。这一条 expr 的 AsKind(Map) 先挡住了：
			// 结果声明的类型与实际的 float64 对不上。列出来是因为
			// 「加个 let 前缀换掉结果」是最省事的绕过尝试，值得有一条用例
			// 说明它拦得住——只是拦它的不是 walkShape。
			name:    "裸数值结果",
			source:  `let x = 1.0; 2.0`,
			wantMsg: "expected map",
		},
		{
			// 三元一支不是表：expr 认这个形状（两支类型不同也放行），
			// 能拦住它的只有 walkShape 的根位置判据。
			name:      "三元的假支不是表",
			source:    `let x = 1.0; c > 100 ? {"prompt": 1.0} : 2.0`,
			wantShape: true,
			wantMsg:   "结果必须是一张单价表",
		},
		{
			// 用量变量落在价格位置：单价随用量变化会破坏预留的上界，
			// 同样只有 walkShape 拦得住。
			name:      "价格位置上的用量乘数",
			source:    `let x = 1.0; {"prompt": p * 2.0}`,
			wantShape: true,
			wantMsg:   `价格位置不能出现用量 "p"`,
		},
		{
			// 结果是绑定的数值、不是表：同样是 expr 的 AsKind(Map) 先挡住。
			// 列出来是为了把「绑定名字不等于放过」钉在明面上——只有绑定的值
			// 是一张表时，根位置的标识符才按那张表检查（见
			// TestLetBindsWholeTable）。
			name:    "绑定的数值结果",
			source:  `let x = 1.0; x`,
			wantMsg: "expected map",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.source)
			if err == nil {
				t.Fatalf("%q 应当编不过", tc.source)
			}
			var ce *CompileError
			if !errors.As(err, &ce) {
				t.Fatalf("应当是 *CompileError，得到 %T: %v", err, err)
			}
			want := ReasonSyntax
			if tc.wantShape {
				want = ReasonShape
			}
			if ce.Reason != want {
				t.Errorf("原因 = %s，期望 %s（%v）", ce.Reason, want, ce.Err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("错误里应当出现 %q，得到 %q", tc.wantMsg, err.Error())
			}
		})
	}
}

// TestCompileAcceptsLegalLetResult 是对照：let 前缀本身不改变合法写法的判定，
// 别把「用 let 抽一个常量」这种正常写法一起拦掉。
func TestCompileAcceptsLegalLetResult(t *testing.T) {
	for name, source := range map[string]string{
		"let 常量配单价表": `let x = 1.0; {"prompt": x}`,
		"let 常量配三元两支表": `let x = 1.0;
			c > 100 ? {"prompt": x} : {"prompt": 2.0}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(source); err != nil {
				t.Errorf("%q 应当编得过: %v", source, err)
			}
		})
	}
}

// TestLetBindsWholeTable 验证 `let price = {...}; price` 仍然是合法写法：
// 根位置看到的是一个绑定的名字，检查要按它绑定的那张表走；Keys 与结算侧的
// Declared 也应当一致——否则给规则加个 let 前缀又会从另一头坏掉。
func TestLetBindsWholeTable(t *testing.T) {
	source := `let price = {"prompt": 1.5, "completion": 2.0}; price`
	p := mustCompile(t, source)

	usage := billing.Usage{PromptTokens: 10, CompletionTokens: 5}
	res, err := p.Eval(Env{Usage: usage, At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}

	declared := make([]string, 0, len(res.Prices.Declared))
	for name := range res.Prices.Declared {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	keys := p.Keys()
	if strings.Join(declared, ",") != strings.Join(keys, ",") {
		t.Errorf("Declared = %v，Keys = %v，两者必须相等", declared, keys)
	}
	if !res.Prices.Declared["prompt"] || !res.Prices.Declared["completion"] {
		t.Errorf("绑定的表里的两项都算定了价: %v", res.Prices.Declared)
	}
}
