package channel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 四个协议各配哪条端点要钉住，期望写成**字面路径**。
//
// 不能写成 EndpointXxx 那些常量：常量就是表里的那个值，拿它当期望等于拿表判表
// ——把 anthropic 与 openai 两条端点调个位置，用例照样绿，而调换之后两侧对不上
// 的那次比对会判成另一个字符串。
func TestProtocolEndpoint(t *testing.T) {
	want := map[Protocol]Endpoint{
		ProtocolOpenAI:          "/v1/chat/completions",
		ProtocolAnthropic:       "/v1/messages",
		ProtocolOpenAIResponses: "/v1/responses",
		ProtocolSystemOne:       "/v1/systemone",
	}
	for p, expected := range want {
		got := p.Endpoint()
		if got != expected {
			t.Errorf("Protocol(%q).Endpoint() = %q，期望 %q", p, got, expected)
		}
		if !got.Valid() {
			t.Errorf("协议 %q 算出的端点 %q 不在合法取值里", p, got)
		}
	}
	// 每一行都要指向一个合法端点。表里的路径是手写的，写错一个字母时那条协议
	// 算出来的端点谁也匹配不上——模型目录里的取值是 ParseEndpoint 解析出来的，
	// 只能是四个合法值之一——于是那个模型在选路里永远「不在被服务的端点上」。
	for p, e := range protocolEndpoints {
		if !e.Valid() {
			t.Errorf("表里 %q 配的端点 %q 不是合法取值（认的是 %s、%s、%s、%s）",
				p, e, EndpointChatCompletions, EndpointResponses, EndpointMessages, EndpointSystemOne)
		}
	}
	// 清单是从表推导的，推导写错会掉一条或重一条；它出现在报错信息里，顺序也要稳。
	if len(protocolNames) != len(protocolEndpoints) {
		t.Errorf("协议名清单 %v 与表（%d 行）条目数不同：清单是推导出来的，两边必须一一对上",
			protocolNames, len(protocolEndpoints))
	}
	if !slices.IsSorted(protocolNames) {
		t.Errorf("协议名清单 %v 没排序：它按这个顺序出现在报错信息里", protocolNames)
	}

	// 未知协议给空串，那是「不知道」而不是「一个叫空串的端点」：调用方
	// （route 的端点评据）据此放行。
	if got := Protocol("gemini").Endpoint(); got != "" {
		t.Errorf("未知协议的端点 = %q，期望空串", got)
	}
	if Endpoint("").Valid() {
		t.Error("空串不该是合法端点：它在本包的意思是「不知道」，而 Valid 说的是「是不是那几条之一」")
	}
}

// 源码里出现的每一个 Protocol 取值都必须在表里有一行。
//
// 表是唯一的来源之后，这条轴只剩一个走偏的走法：**新加一个协议取值，忘了在表里
// 给它端点**。它偏了之后是静默的——那个协议算不出端点（空串 = 不知道 = 放行），
// 注册与选路都不会响。
//
// 所以这里扫一遍本包的非测试源码，而不是写一张硬编码的清单：清单少抄一个新常量时
// 用例照样绿，而「照样绿」正是要被挡住的那件事（见 LESSONS.md 第 2、16 条）。
func TestEveryProtocolConstantIsInTable(t *testing.T) {
	values := protocolValuesInSource(t)
	// 一条都没扫到时判「没跑成」而不是「都合格」：扫描器坏掉时的输出和干净
	// 一模一样（LESSONS.md 第 16 条）。
	if len(values) == 0 {
		t.Fatal("一个 Protocol 取值都没扫到：多半是扫描本身坏了，而不是仓库里真的没有")
	}
	for name, value := range values {
		if _, ok := protocolEndpoints[Protocol(value)]; !ok {
			t.Errorf("%s = %q 不在 protocolEndpoints 表里：加了协议取值就要在表里给它一条端点，"+
				"否则它算出来的端点是空串，而空串在端点评据里等于放行", name, value)
		}
	}
}

// protocolValuesInSource 扫本包的非测试源码，返回 `名 Protocol = "取值"` 的
// 声明（常量与变量都收）。
func protocolValuesInSource(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列源文件失败: %v", err)
	}
	got := map[string]string{}
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		scanned++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			decl, ok := n.(*ast.GenDecl)
			if !ok || (decl.Tok != token.CONST && decl.Tok != token.VAR) {
				return true
			}
			for _, spec := range decl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				// 只看显式声明了 Protocol 类型的那几条：`Endpoint` 那几个常量
				// 也是字符串字面量，但它们是端点不是协议。
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Protocol" {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						break
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s 里 %s 的取值解析失败: %v", file, name.Name, err)
					}
					got[name.Name] = value
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("一个源文件都没扫到：多半是工作目录不对，而不是本包没有非测试文件")
	}
	return got
}

func TestParseEndpoint(t *testing.T) {
	for _, raw := range []string{
		string(EndpointChatCompletions), string(EndpointResponses),
		string(EndpointMessages), string(EndpointSystemOne),
	} {
		got, err := ParseEndpoint(raw)
		if err != nil {
			t.Errorf("ParseEndpoint(%q) 出错: %v", raw, err)
		}
		if string(got) != raw {
			t.Errorf("ParseEndpoint(%q) = %q", raw, got)
		}
	}

	// 解析不出来要报错，**不能**折成空串：空串在端点评据里是「不知道」
	// （放行），而这里要表达的是「这一串是错的」——折平会让一行写错的端点
	// 声明在选路里表现成「这一行没配过」，从此静默不生效。
	for _, raw := range []string{"", "/v1/embeddings", "chat/completions", "openai"} {
		got, err := ParseEndpoint(raw)
		if err == nil {
			t.Errorf("ParseEndpoint(%q) 应报错，得到 %q", raw, got)
			continue
		}
		if got != "" {
			t.Errorf("报错时应当返回空串，得到 %q", got)
		}
		if raw != "" && !strings.Contains(err.Error(), raw) {
			t.Errorf("报错里没提那个非法的取值 %q：%v", raw, err)
		}
	}
}

// 注册表自检：适配器自述的端点必须与它声明的协议算出来的那条一致。
//
// 抄错的表现是静默的——选路按**协议**算出端点去比对目录，而请求实际发去了
// 另一条路径，于是那次比对判的是另一个字符串；上游那边回 404/400，看不出是
// 我们这边配错了。所以挡在注册这一步，进程启动时就报出来。
func TestRegistryRejectsEndpointProtocolMismatch(t *testing.T) {
	r := NewRegistry()
	err := r.Register(&fakeAdapter{
		typ:      string(ProtocolOpenAI),
		protocol: ProtocolOpenAI,
		endpoint: EndpointResponses,
	})
	if err == nil {
		t.Fatal("协议说 openai、端点说 responses 时注册应当失败")
	}
	// 报错要同时说出两处各是什么，否则看日志的人不知道该改哪一边。
	for _, want := range []string{
		string(ProtocolOpenAI), string(EndpointResponses), string(EndpointChatCompletions),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没出现 %q：%v", want, err)
		}
	}
	// 失败之后那一条**没有**被注册进去：注册表里留一条自检不过的适配器，
	// 等于把「启动时报错」变成「这台机器上一直有一条配错的渠道」。
	if _, lookupErr := r.Lookup(string(ProtocolOpenAI)); lookupErr == nil {
		t.Error("自检失败的适配器不该留在注册表里")
	}
}

// 按协议推端点的适配器（真实适配器走的就是这条路）要能通过自检。
//
// 类型名取的就是协议取值本身：注册表不收别的名字（见 Registry.Register 的
// 类型名自检），而真实适配器本来也是这么命名的。
func TestRegistryAcceptsMatchingEndpoint(t *testing.T) {
	for _, p := range []Protocol{
		ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAIResponses, ProtocolSystemOne,
	} {
		r := NewRegistry()
		if err := r.Register(&fakeAdapter{typ: string(p), protocol: p}); err != nil {
			t.Errorf("协议 %q 的适配器注册失败: %v", p, err)
		}
	}
}
