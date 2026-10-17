package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// SupportedAPIVersion 是当前支持的插件契约版本。
//
// 只支持 1：留出「同时支持 N 与 N-1」的位置，但**不预先支持不存在的版本**。
// 一个写着 apiVersion: 2 的插件，在宿主实现 2 之前拒绝它比接受它好——
// 接受之后它会以 1 的语义运行，而作者以为自己用的是 2 的契约。
const SupportedAPIVersion = 1

// Meta 是插件在源码里声明的元数据。
//
// 它是**作者写的输入**，所以每个字段都要校验。校验的落点是这里而不是
// 「用到的时候」：一个 models 写成字符串的插件，如果在请求路径上才发现，
// 表现是「这个插件的路由偶尔不生效」。
type Meta struct {
	// Key 是插件标识，形如 "acme-vision"。它同时是 KV 的命名空间前缀
	// 与日志里的 plugin 字段，所以不能为空、不能带空白。
	Key string `json:"key"`
	// Name 是给人看的名字。
	Name string `json:"name"`
	// Version 是插件自己的版本号，与 apiVersion 不是一回事。
	Version string `json:"version"`
	// Description 是一句话说明。
	Description string `json:"description"`
	// APIVersion 是契约版本。**必填**：没有它就没法判断该按哪套语义跑。
	APIVersion int `json:"apiVersion"`
	// Models 是这个插件声称支持的规范模型名。
	//
	// 空表示「所有模型」——一个平台级的插件（例如统一加系统提示词）
	// 无法穷举模型名。这个默认是宽的，所以**它只影响「插件是否参与」，
	// 不影响插件能做什么**：能做什么由能力白名单与补丁字段表决定。
	Models []string `json:"models"`
	// Protocol 是插件期望的协议。空表示不挑。
	Protocol string `json:"protocol"`
	// Blocking 声明这个插件是阻断型：它失败时**请求失败**。
	//
	// 默认 false。阻断是插件作者主动要的语义（例如「这个模型的请求必须
	// 经过我的内容检查」），而不是宿主猜的。
	Blocking bool `json:"blocking"`
	// Capabilities 是插件申请的能力名。**只能收窄，不能放宽**：
	// 写进来的名字必须是宿主真能提供的一个子集（见 CompileWith 的 provided）。
	// 写进来的名字若不在里面，注册时就报错而不是静默忽略。
	//
	// 空表示「宿主提供什么就要什么」。
	Capabilities []string `json:"capabilities"`
}

// 元数据校验的错误。
var (
	// ErrMetaMissingKey 表示插件没有声明 key。
	ErrMetaMissingKey = errors.New("插件元数据缺少 key")
	// ErrMetaAPIVersion 表示契约版本不受支持。
	ErrMetaAPIVersion = errors.New("插件契约版本不受支持")
	// ErrMetaCapability 表示申请了宿主不提供的能力。
	ErrMetaCapability = errors.New("插件申请了未开放的能力")
)

// 这里曾有 DefaultCapabilities——一份手写的、写死 ["log","calendar","kv"] 的
// 允许集。宿主**真能提供**哪些能力是另一处算出来的（按依赖是否接上），
// 两份清单各自维护，于是插件申请 kv 注册成功、运行期拿到 undefined，而文档
// 承诺的是「能力未启用」。现在允许集由装配算出来（host.Available）并作为参数
// 传进来（CompileWith / PublishPartial），这个包不再持有任何能力名。

// Registration 是一次注册。
type Registration struct {
	// Key 是插件标识，必须与 meta.key 一致。
	//
	// 两处都写是为了让「注册用的名字」与「插件自己声明的名字」对上：
	// 不一致时 KV 命名空间、日志字段与启用记录会分属两个名字，
	// 而那种错法在界面上看不出来。
	Key string
	// Source 是插件源码。
	Source string
	// Priority 是启用记录的优先级。同优先级时按 Key 升序取一个，
	// 保证同样的注册集合每次选出同一个插件。
	Priority int
}

// Plugin 是一个编译并校验过的插件。
type Plugin struct {
	Key      string
	Priority int
	Meta     Meta
	Module   *sandbox.Module
	// Capabilities 是最终生效的能力清单：申请与上限的交集。
	Capabilities []string
}

// Registry 是插件的注册表。
//
// 它是**不可变发布**的：注册一批插件得到一个新的 Registry，
// 旧的继续被在途请求使用。这与插件热更新的要求一致——
// 编译失败时线上不受影响，编译成功时切换是原子的。
//
// 一份 Registry 构造完成之后不再改动，所以读它不需要加锁。
type Registry struct {
	plugins []*Plugin
	index   map[string]*Plugin
}

// Empty 返回一个空注册表。
func Empty() *Registry { return &Registry{index: map[string]*Plugin{}} }

// Len 是已注册的插件数。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.plugins)
}

// Plugins 返回全部插件，按优先级降序、同优先级按 key 升序。
func (r *Registry) Plugins() []*Plugin {
	if r == nil {
		return nil
	}
	return append([]*Plugin(nil), r.plugins...)
}

// Get 按 key 取一个插件。
func (r *Registry) Get(key string) (*Plugin, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.index[key]
	return p, ok
}

// Compile 编译一个插件并校验它的元数据。它**不注册**，只回答「这份源码能不能用」。
//
// 上传路径（larkaxctl plugin publish）与加载路径都经过它，于是「命令行说
// 能用」与「线上真能用」走的是同一段代码——两份校验迟早会不一致。
//
// # 它按「宿主认识的能力」校验，而不是「这次装配提供了哪些」
//
// 这个入口不知道调用它的进程会被装到什么样的部署上（上传就是一个这样的进程：
// 命令行只连库，不知道那份装配接上了什么）。所以它判的是**名字存不存在**——
// 拼错的能力名当场报错。想知道「这次装配真的提供了它吗」，走 CompileWith：
// 那条路由装配把能力清单算出来传进来，是**注册**该用的入口。
//
// 注册名必须由调用方给出（reg.Key）：这个入口的用途是「按这个名字装一个插件」，
// 名字由谁定是调用方的事。源码里已经写了自己 key 的那条路走 CompileSource。
func Compile(reg Registration) (*Plugin, error) {
	return CompileWith(reg, sandbox.CapabilityNames())
}

// CompileWith 与 Compile 相同，但按**这次装配真的提供的能力**收窄。
//
// provided 来自宿主那一侧（host.Available 按接上的依赖算出来），是按请求装配
// 时唯一说得出口的那份清单。插件申请了不在里面的能力时，这里直接报错——
// **注册期失败**，而不是注册成功、运行期拿到一个 undefined。那两种错要改的
// 地方完全不同：前者改 capabilities，后者要去把宿主那条线接上。
func CompileWith(reg Registration, provided []string) (*Plugin, error) {
	if strings.TrimSpace(reg.Key) == "" {
		return nil, ErrMetaMissingKey
	}
	mod, err := sandbox.Compile(reg.Key, reg.Source)
	if err != nil {
		return nil, err
	}
	p, err := fromModule(mod, reg.Priority, provided)
	if err != nil {
		return nil, err
	}
	if p.Key != reg.Key {
		return nil, fmt.Errorf("注册名 %q 与元数据里的 key %q 不一致", reg.Key, p.Key)
	}
	return p, nil
}

// CompileSource 编译一段源码，注册名取自源码里声明的 meta.key。
//
// 上传路径用它：插件已经在源码里写了一遍自己的 key，命令行不必再写第二遍，
// 而写两遍就会出现「两处不一致」这种只能靠人记住的错误。name 只进错误信息
// （通常是文件名），不是注册名。
//
// 能力那一侧与 Compile 同一口径（按宿主**认识**的名字校验，见 Compile 的注释）。
func CompileSource(name, source string, priority int) (*Plugin, error) {
	mod, err := sandbox.Compile(name, source)
	if err != nil {
		return nil, err
	}
	return fromModule(mod, priority, sandbox.CapabilityNames())
}

// fromModule 是两条编译路径共用的尾巴：读元数据、校验、收窄能力。
//
// 抽出来是为了让 Compile 与 CompileSource 只差「注册名从哪来」这一步，
// 校验与能力收窄只有一处实现。
func fromModule(mod *sandbox.Module, priority int, provided []string) (*Plugin, error) {
	meta, err := ParseMeta(mod.Meta())
	if err != nil {
		return nil, err
	}
	caps, err := narrowCapabilities(meta.Capabilities, provided)
	if err != nil {
		return nil, err
	}
	return &Plugin{
		Key:          meta.Key,
		Priority:     priority,
		Meta:         meta,
		Module:       mod,
		Capabilities: caps,
	}, nil
}

// ParseMeta 把 JS 里的 meta 对象翻成 Meta 并校验。
//
// 导出它是给**读库**的那条路用的：`plugins.meta` 存的是发布时那份声明的快照，
// 而快照可以坏（列被手工改过、更早的版本写进去的形状不一样）。管理面要能
// 把「这一行的 meta 有问题」报出来而不是崩掉，用的必须是与编译期同一份校验
// ——两份校验迟早会不一致，而那时界面上说没问题、线上一加载就失败。
func ParseMeta(raw map[string]any) (Meta, error) {
	if raw == nil {
		return Meta{}, fmt.Errorf("%w: 插件必须在源码里 `export const meta`", ErrMetaMissingKey)
	}
	// 走一遍 JSON 而不是逐字段读 map：字段是作者写的输入，
	// 类型判断交给编码层，一处覆盖全部字段（含将来新增的）。
	blob, err := json.Marshal(raw)
	if err != nil {
		return Meta{}, fmt.Errorf("元数据无法序列化: %w", err)
	}
	var meta Meta
	dec := json.NewDecoder(strings.NewReader(string(blob)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		return Meta{}, fmt.Errorf("元数据字段不合法: %w", err)
	}

	if strings.TrimSpace(meta.Key) == "" {
		return Meta{}, ErrMetaMissingKey
	}
	if strings.ContainsAny(meta.Key, " \t\r\n") {
		return Meta{}, fmt.Errorf("%w: key %q 含空白", ErrMetaMissingKey, meta.Key)
	}
	switch meta.APIVersion {
	case SupportedAPIVersion:
	case 0:
		return Meta{}, fmt.Errorf("%w: 缺少 apiVersion，当前为 %d", ErrMetaAPIVersion, SupportedAPIVersion)
	default:
		return Meta{}, fmt.Errorf("%w: 声明 %d，宿主支持 %d", ErrMetaAPIVersion, meta.APIVersion, SupportedAPIVersion)
	}
	// 多返回一个字段就报错（DisallowUnknownFields）是有意的：元数据是作者
	// 对宿主的声明，写错的字段名静默失效会让作者以为它生效了。
	return meta, nil
}

// narrowCapabilities 把申请的能力收窄到**这次装配真的提供**的那些，结果按名字排序。
//
// # 报错而不是忽略
//
// 申请列表写错了是作者的 bug（名字拼错了、用了这次部署没有的能力），要在注册期
// 就告诉他。静默忽略更坏：作者以为自己申请到了，运行期却拿到一个 undefined，
// 而那在插件侧是 JS 的 TypeError——与「宿主没提供」是两种错，要改的地方也不同。
//
// 排序是为了让「同一份申请每次得到同一份清单」：不排序的话清单顺序取决于 map
// 遍历，而它会出现在日志与界面里，于是两次注册看起来不同。
func narrowCapabilities(requested, provided []string) ([]string, error) {
	// 提供的名单排一份出来：错误文案里要列它，而顺序固定才让两次报错长得一样。
	// 调用方传进来的顺序不作要求（装配那一侧按依赖遍历出来的）。
	available := append([]string(nil), provided...)
	sort.Strings(available)

	if len(requested) == 0 {
		// 空表示「宿主提供什么就要什么」：一个平台级插件（例如统一加系统提示词）
		// 没必要把宿主当前的能力名抄进源码——抄了之后宿主加一个能力，它要跟着改。
		return available, nil
	}
	allowed := make(map[string]bool, len(provided))
	for _, c := range provided {
		allowed[c] = true
	}
	var out []string
	seen := make(map[string]bool, len(requested))
	for _, c := range requested {
		if !allowed[c] {
			// **报错而不是忽略**：静默忽略会让作者以为自己申请到了。
			// 申请列表写错了是作者的 bug，要在注册期就告诉他。
			return nil, fmt.Errorf("%w: %q 不在宿主提供的能力里（%s）",
				ErrMetaCapability, c, strings.Join(available, "、"))
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// Publish 用一批注册换出一份新的注册表。旧的注册表不受影响。
//
// 这是「编译全部通过才发布」的落点：全部 Compile 成功才返回新表，
// 任一个失败就返回错误、旧表照旧。于是不会出现半批插件生效的状态——
// 而「一半生效」在界面上看起来与「全都生效」一样。
//
// 能力那一侧与 Compile 同一口径：按宿主**认识**的名字校验（理由见 Compile
// 的注释——这个入口同样不知道调用它的进程会被装到什么样的部署上）。按这次
// 装配真的提供了什么校验的是 PublishPartial，那是启动路径用的那个。
//
// # 今天谁来调它
//
// **生产代码没有调用方，只有测试。** 两条生产路径都不经过它：
//
//   - 启动：store.LoadRegistry 用 PublishPartial（跳过坏的，见 load.go）；
//   - 上传：larkaxctl plugin publish 自己 Compile 一个插件，再把结果写库
//     （见 cmd/larkaxctl/plugin.go），不建内存注册表。
//
// 留着的理由是它回答一个今天还没有入口的问题：一次上传**一批**插件时，
// 是整批拒绝还是逐条接收。单条上传没有这个选择；等那个入口出现时，
// 「全部通过才换表」的语义已经在这里、也有测试钉着了。
//
// 别为了「把它接上」让启动路径改用它：那会把一个坏插件升级成整个服务起不来
// （理由见 PublishPartial 的注释）。
func Publish(regs []Registration) (*Registry, error) {
	compiled := make([]*Plugin, 0, len(regs))
	for _, reg := range regs {
		p, err := Compile(reg)
		if err != nil {
			// 错误里带上 key：一批十几个插件里只报「元数据不合法」没法排查。
			return nil, fmt.Errorf("插件 %s 无法注册: %w", reg.Key, err)
		}
		compiled = append(compiled, p)
	}
	return assemble(compiled)
}

// assemble 把已经编译好的插件装成一份注册表。
//
// 它是 Publish 与 PublishPartial 共用的那一步：重复检测与排序只写一遍，
// 于是「哪条路径装出来的表，顺序都一样」不需要靠各处各自记得。
func assemble(compiled []*Plugin) (*Registry, error) {
	next := Empty()
	for _, p := range compiled {
		if _, dup := next.index[p.Key]; dup {
			return nil, fmt.Errorf("插件 %s 注册了两次", p.Key)
		}
		next.index[p.Key] = p
		next.plugins = append(next.plugins, p)
	}
	sort.Slice(next.plugins, func(i, j int) bool {
		if next.plugins[i].Priority != next.plugins[j].Priority {
			return next.plugins[i].Priority > next.plugins[j].Priority
		}
		return next.plugins[i].Key < next.plugins[j].Key
	})
	return next, nil
}

// Candidates 挑出这次请求可能用到的插件，按优先级降序、同优先级按 key 升序。
//
// 匹配只看 meta 里声明的：模型名与协议。**不看请求内容**——
// 「按内容决定跑哪个插件」会让同一个模型的两条请求跑到不同的插件上，
// 而那种不确定性在排查时几乎无法还原。
func (r *Registry) Candidates(canonicalModel, protocol string) []*Plugin {
	if r == nil {
		return nil
	}
	var out []*Plugin
	for _, p := range r.plugins {
		if !p.supportsModel(canonicalModel) {
			continue
		}
		if p.Meta.Protocol != "" && p.Meta.Protocol != protocol {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (p *Plugin) supportsModel(canonicalModel string) bool {
	if len(p.Meta.Models) == 0 {
		return true
	}
	for _, m := range p.Meta.Models {
		if m == canonicalModel {
			return true
		}
	}
	return false
}

// ErrNoPlugin 表示这次请求没有匹配的插件。
var ErrNoPlugin = errors.New("没有匹配的插件")

// SessionRequest 是挑选插件需要的请求特征。
type SessionRequest struct {
	CanonicalModel string
	Protocol       string
}

// HostBuilder 按**已选中的那个插件**构造这次请求的宿主能力。
//
// # 为什么是一个函数而不是一个建好的 sandbox.Host
//
// 宿主能力里的日志字段与 KV 命名空间都要带上插件 key，而「这次跑哪个插件」
// 是在 NewSession 里面定的。传一个建好的 Host 进来，那个字段只能是空的，
// 或者由调用方自己再挑一遍——后者会让「挑哪个插件」有两条实现，
// 而它们迟早不一致，表现是宿主日志里的 plugin 与真正跑的那个对不上。
//
// 返回零值 Host 表示这次请求不给插件任何宿主能力：插件侧的调用会得到
// 「能力未启用」，而不是一个永远失败的空壳（见 host.Build）。
type HostBuilder func(p *Plugin) sandbox.Host

// NewSession 为一次请求挑一个插件并建好运行时。
//
// **只取一个。** 多插件如何串联（后一个看到前一个改过的，还是都看到原始数据）
// 是一个未定的契约问题：它决定插件作者的调试体验，也决定失败时的回退语义。
// 收窄成「一个请求一个插件」是可逆的（放宽比收紧容易），
// 而猜错链式语义会让已经写好的插件行为变化。
//
// 没有匹配的插件时返回 ErrNoPlugin——调用方据此走「没有插件」的路径，
// 而不是拿到一个空 Session 却以为插件跑过了。
//
// 建出来的 Session **必须由同一个 goroutine 用完再关**（见 Session 的注释）。
func (r *Registry) NewSession(ctx context.Context, req SessionRequest, host HostBuilder, limits sandbox.Limits) (*Session, error) {
	cands := r.Candidates(req.CanonicalModel, req.Protocol)
	if len(cands) == 0 {
		return nil, ErrNoPlugin
	}
	p := cands[0]
	var h sandbox.Host
	if host != nil {
		h = host(p)
	}
	rt, err := p.Module.NewRuntime(ctx, h, p.Capabilities, limits)
	if err != nil {
		return nil, fmt.Errorf("插件 %s 的运行时构造失败: %w", p.Key, err)
	}
	return &Session{
		Plugin: p,
		rt:     rt,
		// 候选集之外的插件这次不参与。记录下来是为了让「明明配了三个插件
		// 为什么只跑了一个」有答案。
		Shadowed: cands[1:],
	}, nil
}
