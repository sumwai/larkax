package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
)

// DefaultMaxBytes 是单条骨架的字节上限，与 request_snapshot.max_bytes 的默认值同源。
//
// 由调用方从 settings 读出后经 WithMaxBytes 覆盖。超限的处置分两层：
// 先降级为「只留顶层标量」的摘要层并置 Truncated，仍然超限才丢快照。
const DefaultMaxBytes = 65536

// maxSchemaDepth 是 schema 递归的深度上限。
//
// JSON Schema 允许自引用，线格式里也可能出现很深的嵌套。超过这个深度就把
// 剩余部分换成形状描述符——这个上限保护的是内存与时间，不是安全性。
const maxSchemaDepth = 24

// rateLimitHeaderPrefixes 是响应侧放行真实取值的响应头前缀。
//
// 额度监控要用它们（剩余额度、重置时间），而它们是上游生成的数字与时间戳，
// 不承载用户内容。其余响应头一律不采，包括自定义头。
var rateLimitHeaderPrefixes = []string{
	"x-ratelimit-",
	"ratelimit-",
	"anthropic-ratelimit-",
}

// rateLimitHeaderExact 是同样放行的精确头名。
var rateLimitHeaderExact = map[string]struct{}{
	"retry-after": {},
}

// deniedHeaders 是最先被排除的一批头名：它们自身就是凭证。
//
// 单独列出（而不是靠「未知头名一律不采」兜住）是为了让「永不采集」这件事
// 在表里看得见。它们连存在与否都不记——「这个请求带了 authorization」
// 也是一条信息，而它不该落进快照。
var deniedHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"api-key":             {},
	"x-goog-api-key":      {},
	"cookie":              {},
	"set-cookie":          {},
}

// Option 调整脱敏器的行为。
type Option func(*Redactor) error

// WithMaxBytes 设置单条骨架的字节上限。非正值会被拒绝：
// 「不限大小」在快照链路上等于允许一条记录把库撑坏。
func WithMaxBytes(n int) Option {
	return func(r *Redactor) error {
		if n <= 0 {
			return fmt.Errorf("骨架字节上限必须为正数，收到 %d", n)
		}
		r.maxBytes = n
		return nil
	}
}

// Redactor 按方向脱敏。
//
// 它持有的是编译后的允许表，本身没有可变状态：一次调用与下一次调用之间不共享
// 工具引用编号，因此同一个实例可以被多个请求并发使用。
type Redactor struct {
	root     Root
	table    fieldTable
	maxBytes int
}

// New 建立一个脱敏器。
//
// 未知方向与「表本身有问题」（重复路径、空路径、空 Note、对具体头写了放行规则）
// 都在这里返回错误。把这些检查放在构造期而不是脱敏期：表写错的代价本该是启动时
// 一次明确的失败，而在脱敏期暴露会表现为「每条请求的骨架都没了」，
// 排查方向与真正的原因隔着一层。
func New(root Root, opts ...Option) (*Redactor, error) {
	rules, ok := allowlists[root]
	if !ok {
		return nil, redactionError("", "未知的快照方向 %q", root)
	}
	if err := validateRules(rules); err != nil {
		return nil, err
	}
	r := &Redactor{root: root, table: buildTable(rules), maxBytes: DefaultMaxBytes}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, redactionError("", "选项无效: %v", err)
		}
	}
	return r, nil
}

// Root 返回这个脱敏器服务的方向，可直接落进 request_snapshots.direction。
func (r *Redactor) Root() Root { return r.root }

// MaxBytes 返回当前的骨架字节上限。
func (r *Redactor) MaxBytes() int { return r.maxBytes }

// validateRules 检查允许表自身的一致性。
//
// 表是数据，数据不会因为写错而编译失败：重复路径让后一条静默覆盖前一条
// （表现为「加了字段还是被脱敏」却没有任何提示），空的 Note 让这张表退化成
// 一份没人知道为什么这么写的清单，而这两个后果都要在几个月后才显形。
func validateRules(rules []fieldRule) error {
	if len(rules) == 0 {
		return redactionError("", "允许表为空")
	}
	seen := make(map[string]struct{}, len(rules))
	for i, rule := range rules {
		switch {
		case strings.TrimSpace(rule.Path) == "":
			return redactionError("", "允许表第 %d 条没有路径", i)
		case strings.TrimSpace(rule.Note) == "":
			return redactionError("", "允许表 %q 没有写明放行或脱敏的理由", rule.Path)
		}
		if _, dup := seen[rule.Path]; dup {
			return redactionError("", "允许表重复定义了 %q", rule.Path)
		}
		seen[rule.Path] = struct{}{}
		// 请求头统一走「头名哈希 + 明确拒绝」两条路，不允许对某个具体头
		// 另开一条放行规则：那条规则一旦写错，就是把凭证头写进了骨架。
		if strings.HasPrefix(rule.Path, "headers.") && !deniesKeep(rule.Kind) {
			return redactionError("", "允许表把 %q 写成了放行；头字段只允许 deny 规则", rule.Path)
		}
	}
	return nil
}

// Transport 是一次上游调用的传输层信息。
//
// 单独一个类型而不是塞进 channel.Response：状态码与响应头由 HTTP 客户端给出，
// 不在归一化的响应结构里。它们必须**显式**传进来——
// 顺手把一个 http.Response 交给脱敏器，等于让一个内部结构不受控的对象
// 靠近白名单。
type Transport struct {
	// Status 是上游返回的 HTTP 状态码。0 表示没有这一项。
	Status int
	// Headers 是上游返回的响应头。额度监控靠它（x-ratelimit-* 这一组），
	// 其余响应头只留头名的短哈希。
	Headers map[string]string
}

// RedactResponseWithTransport 脱敏响应，并把传输层信息并进骨架。
func (r *Redactor) RedactResponseWithTransport(resp *channel.Response, tr Transport) (*Result, error) {
	if resp == nil {
		return nil, redactionError("", "响应为 nil")
	}
	if r.root != RootResponse {
		return nil, redactionError("", "脱敏器是 %q 方向，不能处理响应", r.root)
	}
	m := responseToMap(resp)
	if tr.Status != 0 {
		m["status"] = tr.Status
	}
	if len(tr.Headers) > 0 {
		headers := make(map[string]any, len(tr.Headers))
		for k, v := range tr.Headers {
			headers[k] = v
		}
		m["headers"] = headers
	}
	return r.RedactRaw(m)
}

// Result 是一次脱敏的产出。
type Result struct {
	// Skeleton 可直接落进 request_snapshots.skeleton。
	Skeleton map[string]any
	// Redacted 是被替换掉的路径，已排序去重，落进 redacted_paths。
	Redacted []string
	// Truncated 表示骨架超限、已降级为只含顶层标量的摘要层。
	Truncated bool
	// Version 是生成这条骨架时的 SkeletonVersion。
	Version int
}

// Decode 把一段 JSON 报文解析成脱敏器的输入。
//
// 单独提供它而不是让调用方自己 json.Unmarshal：这里多做两件事，而两件都直接
// 关系到快照可信不可信——数字用 json.Number 接住（大整数被静默转成浮点后
// 与原值不一致，而错的那一份会被当成「上游就是这么回的」），
// 以及拒绝「一个对象后面还跟着别的东西」的报文（那种报文说明我们解析的不是
// 一条完整消息，硬按第一条解析会留下一条误导性的快照）。
func Decode(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, redactionError("", "报文为空，无法解析")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		// 只带错误类型，不带原文：解析失败的报文经常就是用户内容本身。
		return nil, redactionError("", "报文不是 JSON 对象（%T）", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, redactionError("", "报文在首个 JSON 对象之后还有内容")
	}
	if out == nil {
		return nil, redactionError("", "报文是 JSON null，不是对象")
	}
	return out, nil
}

// RedactRaw 脱敏一份已经解析好的结构。
//
// 这是唯一的核心入口：请求侧与响应侧的入口都先把自己摊成结构再走这里，
// 于是「白名单放行、默认脱敏」这条规则只有一份实现。
//
// 任何错误都带 ErrRedaction，调用方拿到错误就丢弃整条快照。
func (r *Redactor) RedactRaw(v any) (*Result, error) {
	if r == nil {
		return nil, redactionError("", "脱敏器为 nil")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, redactionError("", "顶层不是 JSON 对象而是 %T", v)
	}
	if err := r.checkTable(); err != nil {
		return nil, err
	}

	w := &walker{redactor: r, refs: newToolRefs()}
	res := &Result{
		Skeleton: w.walkObject(m, ""),
		Redacted: w.sortedPaths(),
		Version:  SkeletonVersion,
	}
	// 遍历期间记下的「脱敏器自身出问题」在这里统一上报。
	// 它必须走到这一层才报：中途的每一层递归都要多一个 error 返回值，
	// 而那些返回值的唯一用途就是被逐层转手。
	if w.failure != nil {
		return nil, w.failure
	}
	if err := r.applyBudget(res); err != nil {
		return nil, err
	}
	return res, nil
}

// checkTable 确认允许表仍然可用。
//
// 表是包级数据，正常情况下不会变；这一步针对的是「表在运行期被改坏」这类
// 不该发生却也无法从类型上排除的情况。表坏掉时唯一的正确处理就是丢快照——
// 拿着一张认不出来的表去脱敏，等于在没有白名单的情况下放行。
func (r *Redactor) checkTable() error {
	if len(r.table) == 0 {
		return redactionError("", "允许表为空，无法脱敏")
	}
	return nil
}

// RedactRequest 脱敏一次统一格式的请求。
func (r *Redactor) RedactRequest(req *channel.Request) (*Result, error) {
	if req == nil {
		return nil, redactionError("", "请求为 nil")
	}
	if r.root != RootRequest {
		return nil, redactionError("", "脱敏器是 %q 方向，不能处理请求", r.root)
	}
	return r.RedactRaw(requestToMap(req))
}

// RedactResponse 脱敏一次统一格式的响应。
//
// 只脱敏归一化后的字段，**不摊** Response.Raw 与 Usage.Raw：那两个是上游原样
// 返回的对象，内部结构不受本项目控制，顺手把一整个未知对象带进骨架等于把
// 「上游加了什么字段」交给运气。需要它们时调用方单独传给 RedactRaw——
// 那时讨论范围明确。
func (r *Redactor) RedactResponse(resp *channel.Response) (*Result, error) {
	if resp == nil {
		return nil, redactionError("", "响应为 nil")
	}
	if r.root != RootResponse {
		return nil, redactionError("", "脱敏器是 %q 方向，不能处理响应", r.root)
	}
	return r.RedactRaw(responseToMap(resp))
}

// SkeletonJSON 返回骨架的 JSON 编码。
func (res *Result) SkeletonJSON() ([]byte, error) {
	if res == nil {
		return nil, redactionError("", "结果为空")
	}
	raw, err := json.Marshal(res.Skeleton)
	if err != nil {
		return nil, redactionError("", "骨架无法序列化（%T）", err)
	}
	return raw, nil
}

// applyBudget 在骨架超限时降级为摘要层，仍然超限则报错。
//
// 降级保留的是顶层标量：模型名、max_tokens、消息条数这类「一眼能看出这次请求
// 长什么样」的信息；丢掉的是逐条消息与工具定义的细节——它们是骨架膨胀的来源，
// 而深入一层的问题需要另一条快照去补。
//
// 第二层仍然超限时返回错误而不是继续裁剪：那时剩下的已经是几十个标量，
// 超限说明上限配置本身有问题（比如被配成了 1）。静默产出一条几乎空的快照
// 会让「上限配错了」这件事永远不显形。
func (r *Redactor) applyBudget(res *Result) error {
	if r.maxBytes <= 0 {
		return redactionError("", "骨架字节上限未设置")
	}
	size, err := jsonSize(res.Skeleton)
	if err != nil {
		return redactionError("", "骨架无法序列化（%T）", err)
	}
	if size <= r.maxBytes {
		return nil
	}

	res.Skeleton = topScalars(res.Skeleton)
	res.Truncated = true
	size, err = jsonSize(res.Skeleton)
	if err != nil {
		return redactionError("", "摘要层无法序列化（%T）", err)
	}
	if size > r.maxBytes {
		return redactionError("", "摘要层仍然超过字节上限 %d（实际 %d），丢弃快照",
			r.maxBytes, size)
	}
	return nil
}

// topScalars 只留顶层标量，丢掉各级容器。
func topScalars(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if isScalar(v) {
			out[k] = v
		}
		// 容器整体丢掉而不是换成空容器：空对象会被读成
		// 「这次请求没有消息」，与「被截断了」是两件事。
	}
	out["_truncated"] = true
	return out
}

// jsonSize 是值序列化后的字节数。
func jsonSize(v any) (int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

// toolRefs 在**一次调用内**给工具名与工具调用 id 分配稳定的引用号。
//
// 两个名字空间分开：工具名是 T1、T2…，工具调用 id 是 C1、C2…。
// 不共用一套编号的理由有两条——
//
//   - **确定性**。一次请求里名称与 id 出现在同一层对象里（tool_use 块同时带
//     id 与 name），而 JSON 对象在 Go 里是 map，遍历顺序不确定。
//     共用一套编号时同一份请求两次脱敏会得到两个不同的编号，
//     而 redacted_paths 与骨架都要能逐字比对。
//   - **两件事就是两件事**。工具名是用户命名的，调用 id 是上游生成的；
//     把它们编在同一串里，读骨架的人会以为两者有对应关系，
//     而真正的对应关系是 tool_use.id 与 tool_result.tool_use_id 之间的，
//     那一条靠 id 名字空间里的同一个编号保住。
//
// 编号跨请求不复用、也不持久化：跨请求关联工具名会让「T1 是什么」变成
// 一个问题需要第二张表才能回答，而快照的定位是自解释的排障辅助。
type toolRefs struct {
	names map[string]string
	ids   map[string]string
}

func newToolRefs() *toolRefs {
	return &toolRefs{names: map[string]string{}, ids: map[string]string{}}
}

// ref 取一个字符串的引用号。
//
// isName 为 true 时在工具名的名字空间里编号（前缀 T），
// 为 false 时在调用 id 的名字空间里编号（前缀 C）。
func (t *toolRefs) ref(s string, isName bool) string {
	space, prefix := t.ids, "C"
	if isName {
		space, prefix = t.names, "T"
	}
	if v, ok := space[s]; ok {
		return v
	}
	ref := prefix + strconv.Itoa(len(space)+1)
	space[s] = ref
	return ref
}

// walker 是单次调用的遍历状态。
type walker struct {
	redactor *Redactor
	refs     *toolRefs
	paths    []string
	// failure 记录遍历中发现的「脱敏器自身出问题」。
	//
	// 这类问题不是某条请求的数据问题，而是脱敏器或它的表出了问题，
	// 所以它一旦非空，整条快照都要丢掉（铁律之二）。记录而不是就地返回，
	// 是为了让递归的每一层不必各自往外传错误。
	failure error
}

// fail 记下第一个「脱敏器自身出问题」。
//
// 只留第一个：后面那些多半是同一个原因的余波，把它们都记下来会让
// 真正的第一条淹在噪音里。
func (w *walker) fail(path, reason string, args ...any) {
	if w.failure == nil {
		w.failure = redactionError(path, reason, args...)
	}
}

// mark 记录一个被替换掉的路径。
func (w *walker) mark(path string) {
	if path != "" {
		w.paths = append(w.paths, path)
	}
}

// markIndexed 记录数组元素级别的路径。
func (w *walker) markIndexed(path string, i int) {
	w.mark(indexPath(path, i))
}

// sortedPaths 返回排序去重后的脱敏路径。
//
// 排序是有意的：map 的遍历顺序不确定，不排序的话同一条请求两次脱敏会得到两个
// 不同的 redacted_paths，既没法比对也没法写断言。
func (w *walker) sortedPaths() []string {
	out := make([]string, 0, len(w.paths))
	seen := make(map[string]struct{}, len(w.paths))
	for _, p := range w.paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// walkObject 处理一层对象：允许表里有的按规则，没有的不留取值。
//
// 这里是「白名单放行」的落地处。没有规则的键名走两条路——键名本身是内容类
// （text、content、input 之类）时按内容处理；其余保留形状（键名、类型、
// 嵌套结构），取值一律不留。
func (w *walker) walkObject(m map[string]any, path string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		child := joinPath(path, k)
		kind, declared := w.redactor.table.lookup(child, k)
		if !declared {
			if isContentKey(k) {
				w.mark(child)
				out[k] = shapeOf(v)
				continue
			}
			out[k] = w.undeclared(v, child)
			continue
		}
		if kind == deny {
			// 永不采集：连键名都不留。
			continue
		}
		out[k] = w.apply(kind, v, child)
	}
	return out
}

// undeclared 处理允许表里没有的键名的取值。
//
// 容器按形状继续走（键名与嵌套结构可以留，值不可能留到最后一层），
// 标量一律换成描述符。这一段是整个包的默认档：上游新增字段时它默认被脱敏，
// 直到有人主动把字段加进允许表。
func (w *walker) undeclared(v any, path string) any {
	switch t := v.(type) {
	case map[string]any:
		return w.walkObject(t, path)
	case []any:
		return w.walkArray(t, path)
	case string:
		w.mark(path)
		return textPlaceholder(t)
	default:
		w.mark(path)
		return describeScalar(v)
	}
}

// walkArray 处理一层数组：元素逐个按对象、数组或标量处理。
func (w *walker) walkArray(arr []any, path string) []any {
	out := make([]any, len(arr))
	for i, elem := range arr {
		elemPath := indexPath(path, i)
		switch t := elem.(type) {
		case map[string]any:
			// 元素本身不登记路径，只有它内部被替换的字段才登记：
			// 登记 `messages[0]` 会让人以为整条消息都丢了。
			out[i] = w.walkObject(t, elemPath)
		case []any:
			out[i] = w.walkArray(t, elemPath)
		case string:
			w.mark(elemPath)
			out[i] = textPlaceholder(t)
		default:
			w.mark(elemPath)
			out[i] = describeScalar(elem)
		}
	}
	return out
}

// apply 按规则处理一个取值。
//
// 这里集中体现那条贯穿全包的规则：**放行一个字段，不等于放行上游往这个位置
// 发来的任何取值**。所以除 allowRaw 之外，每一个放行档都自带取值检查，
// 检查不过就退到「按内容处理」——只留长度或只留形状。
//
// 不返回错误：走到这里时「该不该丢快照」已经不再取决于取值本身，
// 而取决于允许表。表坏掉的情况由 fail 记录，等这一遍走完统一上报——
// 那比在递归中途逐层往外传错误更短，也让每一层的签名只说自己的事。
func (w *walker) apply(kind fieldKind, v any, path string) any {
	switch kind {
	case allowBool:
		// 只放行布尔。别的一律按内容处理——布尔字段的位置收到字符串，
		// 我们无从判断那串是什么。
		if b, ok := v.(bool); ok {
			return b
		}
		if v == nil {
			return nil
		}
		w.mark(path)
		if s, ok := v.(string); ok {
			return textPlaceholder(s)
		}
		return shapeOf(v)

	case allowUint:
		// 只放行非负整数。
		//
		// 落进骨架的是具体类型的整数而不是 json.Number：后者对非法取值的问题
		// 会在整条骨架序列化那一刻才暴露，现场离出问题的那一格很远。
		if n, ok := asInt64(v); ok && n >= 0 {
			return n
		}
		w.mark(path)
		if s, ok := v.(string); ok {
			return textPlaceholder(s)
		}
		return shapeOf(v)

	case allowFloat:
		// 只放行有限浮点。NaN 与 Inf 序列化出来不是合法 JSON，
		// 会让整条骨架在落库那一步失败，而失败点离这一格很远。
		if f, ok := asFloat64(v); ok && !isBadFloat(f) {
			return f
		}
		w.mark(path)
		if s, ok := v.(string); ok {
			return textPlaceholder(s)
		}
		return shapeOf(v)

	case allowEnumWord:
		// 只放行「看起来像枚举词」的字符串。取值不像枚举时按内容处理：
		// 上游往一个写着「枚举」的位置回显用户输入，是这条链路最现实的失手方式。
		if s, ok := v.(string); ok {
			if isEnumWord(s) {
				return s
			}
			w.mark(path)
			return textPlaceholder(s)
		}
		w.mark(path)
		return describeScalar(v)

	case allowNumberMap:
		return w.numberMap(v, path)

	case keepShape:
		return w.container(v, path)

	case hideText:
		return w.textShape(v, path)

	case hideValue, hideSubtree:
		w.mark(path)
		return redactedMask()

	case hideKeysKeepShape:
		return w.keysOnly(v, path)

	case toolRef, toolRefID:
		// 工具名与调用 id 都换成请求内的引用号；分别用 T 与 C 两套编号，
		// 理由见 toolRefs 的注释。
		s, ok := v.(string)
		if !ok {
			w.mark(path)
			return shapeOf(v)
		}
		w.mark(path)
		return w.refs.ref(s, kind == toolRef)

	case hideStringList:
		return w.textList(v, path)

	case keepEnumList:
		return w.enumList(v, path)

	case roleField:
		return w.role(v, path)

	case blockListField:
		return w.blocks(v, path)

	case schemaField:
		return w.schema(v, path, 0)

	case headersField:
		return w.headers(v, path, false)

	case responseHeadersField:
		return w.headers(v, path, true)

	case extraField:
		return w.extra(v, path)

	case deny:
		// allowlists 里的 deny 条目不该走到取值处理（调用方会先跳过），
		// 走到这里说明表或调用方有一方写错了。
		w.fail(path, "deny 规则走到了取值处理，允许表结构有问题")
		return shapeOf(v)

	default:
		// 表里有一档本函数不认识的处理方式。这不能只把这一格标成脱敏就放过：
		// 「判定规则自身出了问题」意味着这一遍的判定不再可信，
		// 整条快照都要丢掉（铁律之二）。
		w.fail(path, "允许表用了未知的处理方式 %d", kind)
		return shapeOf(v)
	}
}

// asInt64 把一个 JSON 数字认成整数。
//
// 覆盖 json.Number（Decode 用 UseNumber 解析出来的形态）与 Go 侧的整数类型
// （调用方直接构造结构时会出现）。小数不算整数——token 数、条数都不该有小数部分。
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case int:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		if t != math.Trunc(t) || isBadFloat(t) {
			return 0, false
		}
		return int64(t), true
	default:
		return 0, false
	}
}

// asFloat64 把一个 JSON 数字认成浮点。
func asFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return 0, false
	}
}

// isBadFloat 判断一个浮点是不是不能落进 JSON 的那种。
//
// NaN 与 Inf 序列化时会让整个骨架的序列化失败，而失败发生在很远的地方
// （落库那一步），现场只会是一句「骨架序列化失败」。在这里挡住它们，
// 那个字段退化成描述符，其余骨架照常。
func isBadFloat(f float64) bool {
	return math.IsNaN(f) || math.IsInf(f, 0)
}

// numberMap 处理「键名保留、值必须是数字」的字典。
//
// logit_bias 与 usage.unit_counts 走这条路：键名（token id、单位名）与值都不是
// 内容，而键名本身有复现价值。值不是数字时换描述符：那一刻说明实际结构与我们
// 以为的不一样，而不一样的那个结构里可能装着字符串。
func (w *walker) numberMap(v any, path string) any {
	m, ok := v.(map[string]any)
	if !ok {
		w.mark(path)
		return shapeOf(v)
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		child := joinPath(path, k)
		if f, ok := asFloat64(val); ok && !isBadFloat(f) {
			out[k] = f
			continue
		}
		// 值不是数字：说明实际结构与我们对它的认识不一样，
		// 而那个结构里可能装着内容。换成描述符，并记下路径。
		w.mark(child)
		out[k] = describeScalar(val)
	}
	return out
}

// container 处理按形状放行的字段。
//
// 数组与对象继续递归；落到这一档却是标量，说明线格式与预期不符，
// 那就按未知取值处理——不报错，也不原样返回。
func (w *walker) container(v any, path string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return w.walkObject(t, path)
	case []any:
		return w.walkArray(t, path)
	default:
		w.mark(path)
		return shapeOf(v)
	}
}

// textShape 把任意取值换成文本描述符。
//
// 非字符串的取值按序列化后的字符数记：那个数字仍然能回答「这段有多长」，
// 而内容不会落出。
func (w *walker) textShape(v any, path string) any {
	w.mark(path)
	if s, ok := v.(string); ok {
		return textPlaceholder(s)
	}
	n, _ := charCount(v)
	return map[string]any{fieldType: typeText, fieldChars: n}
}

// textList 处理字符串数组：每个元素只留长度。
//
// 单个字符串按「只有一个元素的列表」处理，取值同样只留长度——线格式里同一个
// 字段有 `stop: "END"` 与 `stop: ["END"]` 两种写法，两种都不该留下取值。
func (w *walker) textList(v any, path string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		w.mark(path)
		return textPlaceholder(t)
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			elemPath := indexPath(path, i)
			w.mark(elemPath)
			if s, ok := elem.(string); ok {
				out[i] = textPlaceholder(s)
				continue
			}
			out[i] = shapeOf(elem)
		}
		return out
	default:
		w.mark(path)
		return shapeOf(v)
	}
}

// enumList 处理取值是枚举词的字符串数组。
//
// 只放行字符串元素：数组里混进对象时那个对象按未知取值处理，
// 而不是「整条列表都因为是枚举列表而放行」。
func (w *walker) enumList(v any, path string) any {
	if s, ok := v.(string); ok {
		if isEnumWord(s) {
			return s
		}
		w.mark(path)
		return textPlaceholder(s)
	}
	arr, ok := v.([]any)
	if !ok {
		w.mark(path)
		return shapeOf(v)
	}
	out := make([]any, len(arr))
	for i, elem := range arr {
		if s, ok := elem.(string); ok {
			if isEnumWord(s) {
				out[i] = s
				continue
			}
			w.markIndexed(path, i)
			out[i] = textPlaceholder(s)
			continue
		}
		w.markIndexed(path, i)
		out[i] = shapeOf(elem)
	}
	return out
}

// keysOnly 保留键名与嵌套结构，标量取值换成描述符。
//
// 工具参数走这条路。保留参数键名是有意的：schema 校验类错误（漏传必填项、
// 类型不符）必须能看到键名才能复现。这是一个显式的策略取舍——
// 想收紧的接入方把允许表里的 hideKeysKeepShape 换成 hideSubtree 即可，
// 代价是那类错误变得难以复现。
func (w *walker) keysOnly(v any, path string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = w.keysOnly(val, joinPath(path, k))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			out[i] = w.keysOnly(elem, indexPath(path, i))
		}
		return out
	case string:
		w.mark(path)
		return textPlaceholder(t)
	default:
		w.mark(path)
		return describeScalar(v)
	}
}

// role 处理消息角色。
//
// 角色是形状的一部分（user / assistant / tool 的序列是复现对话类错误的第一现场），
// 所以已知的角色词原样留。例外是数组写法：OpenAI 允许 `role: [名, 工具名]`，
// 其中**没有**名字时工具名会被放进角色位置——那种取值按工具引用处理，
// 而不是当作角色名放行。
func (w *walker) role(v any, path string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if isRoleWord(t) {
			return t
		}
		w.mark(path)
		return textPlaceholder(t)
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			elemPath := indexPath(path, i)
			s, isStr := elem.(string)
			if !isStr {
				w.mark(elemPath)
				out[i] = shapeOf(elem)
				continue
			}
			if isRoleWord(s) {
				out[i] = s
				continue
			}
			w.mark(elemPath)
			// 这一格装的是一次工具**名**（不是调用 id）：
			// 没有名字时 OpenAI 的形状会把工具名放到 role 位置。
			out[i] = w.refs.ref(s, true)
		}
		return out
	case map[string]any:
		return w.walkObject(t, path)
	default:
		w.mark(path)
		return shapeOf(v)
	}
}

// isRoleWord 判断一个取值是否为已知的角色词。
//
// 只认固定的一小组：其余取值出现在角色位置时，更可能是「被塞进角色位置的
// 工具名」而不是新角色，按内容处理。
func isRoleWord(s string) bool {
	switch strings.ToLower(s) {
	case "user", "assistant", "system", "tool", "developer", "function":
		return true
	default:
		return false
	}
}

// blocks 处理内容块序列。
//
// 三种线格式都要接住：块数组（两套协议的现代写法）、纯字符串
// （content 直接是提示词，老写法）、单个对象（部分上游的自定义格式）。
func (w *walker) blocks(v any, path string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		w.mark(path)
		return textPlaceholder(t)
	case map[string]any:
		return w.block(t, path)
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			elemPath := indexPath(path, i)
			block, ok := elem.(map[string]any)
			if !ok {
				w.mark(elemPath)
				if s, isStr := elem.(string); isStr {
					out[i] = textPlaceholder(s)
					continue
				}
				out[i] = shapeOf(elem)
				continue
			}
			out[i] = w.block(block, elemPath)
		}
		return out
	default:
		w.mark(path)
		return shapeOf(v)
	}
}

// block 处理一个内容块。
//
// 默认沿用对象规则（块的类型、顺序、媒体类型都是形状）。两处例外：
//
//   - 图片/音频这类二进制块不保留内部结构，整块换成字节数。内部的数据字段名与
//     编码方式是上游自定义的，逐字段走只会让「数据到底在哪」变成一次猜测，
//     而猜错的那一次就是把 base64 留进了库。
//   - 错误块整体按内容处理：这种块的正文是上游的错误描述，
//     而错误描述经常回显用户输入。
func (w *walker) block(block map[string]any, path string) any {
	blockType := blockTypeOf(block)
	switch {
	case isBinaryBlock(blockType):
		w.mark(path)
		return blobPlaceholder(blockType, mediaTypeOf(block), binaryDataOf(block))
	case isErrorBlock(blockType):
		w.mark(path)
		out := map[string]any{fieldType: typeText, "kind": blockType}
		if msg, ok := block["message"]; ok {
			n, _ := charCount(msg)
			out[fieldChars] = n
		}
		// code 是枚举位置，取值看起来像枚举词才留。
		if code, ok := block["code"].(string); ok && isEnumWord(code) {
			out["code"] = code
		}
		return out
	default:
		return w.walkObject(block, path)
	}
}

// schema 处理 JSON Schema：形状保留，字符串枚举脱敏。
//
// 保留的是键名、类型、required 与嵌套结构。脱敏的是 enum / const / default
// 这类携带具体取值的分支——它们的字符串往往是业务枚举（状态码、订单类型），
// 形状相同但取值本身就是内容。
//
// required 里的名字与 properties 的键名是同一件东西，保留它不增加暴露面，
// 而「漏传必填项」正是最需要复现的一类错误。
func (w *walker) schema(v any, path string, depth int) any {
	if depth > maxSchemaDepth {
		w.mark(path)
		return shapeOf(v)
	}
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			child := joinPath(path, k)
			out[k] = w.schemaKey(k, val, child, depth)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			out[i] = w.schema(elem, indexPath(path, i), depth+1)
		}
		return out
	default:
		w.mark(path)
		return shapeOf(v)
	}
}

// schemaKey 处理 schema 里的一个关键字。
//
// 分三类：
//
//   - **结构关键字**（type、properties、items、anyOf 之类）继续按 schema 递归，
//     `required` 里的名字原样留——它说的就是属性名，而属性名本来就留着，
//     两处一致才算「schema 校验类错误能复现」。
//   - **{参数名: 子 schema} 型关键字**（properties、$defs、definitions、
//     patternProperties、dependentSchemas）的**子键名是属性名**，保留；
//     其余递归。
//   - **携带具体取值的分支**（enum、const、default、examples）换成形状：
//     它们的字符串往往是业务枚举（状态码、订单类型），形状相同但取值是内容。
//
// 认不出的关键字按「容器继续走、标量只留类型」处理——这一支存在的意义正是
// 上游加了新关键字，而那时我们既不想漏掉结构，也不想赌它的取值。
func (w *walker) schemaKey(key string, val any, path string, depth int) any {
	switch key {
	case "type", "required":
		return val
	case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
		return w.namedSchemas(val, path, depth)
	case "items", "additionalProperties", "anyOf", "oneOf", "allOf",
		"not", "if", "then", "else":
		return w.schema(val, path, depth+1)
	case "enum", "const", "default", "examples":
		w.mark(path)
		if arr, ok := val.([]any); ok {
			shapes := make([]any, len(arr))
			for i, elem := range arr {
				shapes[i] = shapeOf(elem)
			}
			return shapes
		}
		return shapeOf(val)
	default:
		if isContainer(val) {
			return w.schema(val, path, depth+1)
		}
		w.mark(path)
		return shapeOf(val)
	}
}

// namedSchemas 处理「键名是用户命名的标识（参数名、定义名）」的那一层。
//
// 这一层的键名保留是有意的：schema 校验类错误（漏传必填项、类型不符）
// 必须能看到参数名才能复现，而 schema 校验错误是工具调用里最常见的一类。
// 保留的只是名字，值仍然按 schema 规则逐层脱敏。
func (w *walker) namedSchemas(v any, path string, depth int) any {
	m, ok := v.(map[string]any)
	if !ok {
		w.mark(path)
		return shapeOf(v)
	}
	out := make(map[string]any, len(m))
	for name, sub := range m {
		out[name] = w.schema(sub, joinPath(path, name), depth+1)
	}
	return out
}

// headers 处理请求头与响应头。
//
// 请求侧只留头名的短哈希，不留取值：自定义头的头名可能带私有信息，
// 取值里则可能有凭证。响应侧额外放行额度监控需要的 x-ratelimit-*——
// 那是上游生成的数字与时间戳，不承载用户内容。
//
// 哈希不是加密：它对低熵的头名几乎没有保护作用。它在这里的作用是让
// 「两次请求带了同一组头」可以比对，同时不把头名直接写进快照。
// 凭证类头名在 deniedHeaders 里被单列出来，连哈希都不进。
func (w *walker) headers(v any, path string, response bool) any {
	pairs, ok := headerPairs(v)
	if !ok {
		w.mark(path)
		return shapeOf(v)
	}

	out := make(map[string]any, len(pairs))
	hashed := make([]any, 0, len(pairs))
	for _, pair := range pairs {
		name := strings.ToLower(pair[0])
		if _, deny := deniedHeaders[name]; deny {
			continue
		}
		if response && w.rateLimitHeader(name) {
			out[name] = pair[1]
			continue
		}
		hashed = append(hashed, shortHash(name))
	}
	if len(hashed) > 0 {
		sort.Slice(hashed, func(i, j int) bool {
			return hashed[i].(string) < hashed[j].(string)
		})
		out[fieldHashed] = hashed
		// 头容器整层都换过了（只留哈希），所以记容器路径而不是逐个头名：
		// 逐个记会把一份可能很长的头名清单写进 redacted_paths，
		// 而那份清单的用途只是回答「白名单是不是漏了该留的头」。
		w.mark(path)
	}
	return out
}

// rateLimitHeader 判断一个响应头是否是额度监控需要的。
func (w *walker) rateLimitHeader(name string) bool {
	if _, ok := rateLimitHeaderExact[name]; ok {
		return true
	}
	for _, prefix := range rateLimitHeaderPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// headerPairs 把几种头容器拉平成键值对。
//
// 统一的 Request.Headers 是 map[string]string，原样解析的报文里可能是
// map[string]any 或 map[string][]string（HTTP 头允许同名多值，
// 而“同一个头出现了两次”本身就是一种要复现的现象）。
func headerPairs(v any) ([][2]string, bool) {
	switch t := v.(type) {
	case map[string]string:
		out := make([][2]string, 0, len(t))
		for k, val := range t {
			out = append(out, [2]string{k, val})
		}
		return out, true
	case map[string][]string:
		out := make([][2]string, 0, len(t))
		for k, val := range t {
			out = append(out, [2]string{k, strings.Join(val, ", ")})
		}
		return out, true
	case map[string]any:
		out := make([][2]string, 0, len(t))
		for k, val := range t {
			out = append(out, [2]string{k, flattenHeaderValue(val)})
		}
		return out, true
	default:
		return nil, false
	}
}

// flattenHeaderValue 把头的取值收敛成字符串。
func flattenHeaderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []string:
		return strings.Join(t, ", ")
	case []any:
		parts := make([]string, 0, len(t))
		for _, elem := range t {
			if s, ok := elem.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// extra 处理上游特有参数（Request.Extra）。
//
// 这里的键名**不留**：上游特有参数的键名由配置方自定，往往就是业务名词
// （供应商代号、线路名），而它们对复现几乎没有帮助。保留的是每个取值的形状，
// 按序号排列而不是按哈希——哈希对低熵键名（试一遍私有名字典就能撞开）
// 起不到保护作用，而序号不加掩饰地表示「这里有一个键」。
//
// 表里点名放行的键照常按各自的规则处理（例如客户端塞进来的 extra_body
// 保留键名形状）：那些键的位置是已知的，也就不必靠序号。
func (w *walker) extra(v any, path string) any {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		w.mark(path)
		return shapeOf(v)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]any, len(m))
	for i, k := range keys {
		child := joinPath(path, k)
		kind, declared := w.redactor.table.lookup(child, k)
		if declared {
			if kind == deny {
				continue
			}
			out[k] = w.apply(kind, m[k], child)
			continue
		}
		w.mark(child)
		out["_k"+strconv.Itoa(i)] = shapeOf(m[k])
	}
	return out
}

// isContainer 判断取值是否为容器。
func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any, map[string]string, map[string][]string:
		return true
	default:
		return false
	}
}

// blockTypeOf 取块的类型名。
func blockTypeOf(block map[string]any) string {
	if s, ok := block["type"].(string); ok {
		return s
	}
	return typeUnknown
}

// isBinaryBlock 判断块是否是二进制数据块。
func isBinaryBlock(blockType string) bool {
	switch strings.ToLower(blockType) {
	case "image", "image_url", "input_image", "audio", "video", "document",
		"file", "input_audio", "input_file":
		return true
	default:
		return false
	}
}

// isErrorBlock 判断块是否是错误块。
func isErrorBlock(blockType string) bool {
	return strings.EqualFold(blockType, "error")
}

// mediaTypeOf 取块的媒体类型。
//
// 媒体类型不是数据本身，处置上仍按「先看有没有」处理：它有时只出现在
// source 子对象里，而那个子对象在二进制块里会被整块丢掉。
func mediaTypeOf(block map[string]any) string {
	if s, ok := block["media_type"].(string); ok {
		return s
	}
	if s, ok := block["mime_type"].(string); ok {
		return s
	}
	if m, ok := block["source"].(map[string]any); ok {
		if s, ok := m["media_type"].(string); ok {
			return s
		}
	}
	return ""
}

// binaryDataOf 取出块里的二进制负载，只用于算字节数。
//
// 编码方式与字段名随上游而异，这里按常见形态找一遍；找不到就返回空串
// （字节数为 0），而不是把整块当成负载——后者会让字节数变成一个没有意义的
// 巨大数字。
func binaryDataOf(block map[string]any) string {
	for _, key := range []string{
		"data", "base64", "image_base64", "audio_base64",
		"b64_json", "file_data", "b64",
	} {
		if s, ok := block[key].(string); ok && s != "" {
			return s
		}
	}
	// OpenAI 的图片写法：image_url 可以直接是 data URI。
	if s, ok := block["image_url"].(string); ok && strings.HasPrefix(s, "data:") {
		if i := strings.Index(s, ","); i >= 0 {
			return s[i+1:]
		}
	}
	// Anthropic 的图片写法：source.data。
	if m, ok := block["source"].(map[string]any); ok {
		if s, ok := m["data"].(string); ok {
			return s
		}
	}
	return ""
}

// requestToMap 把统一格式的请求摊成线格式结构，再走同一条脱敏路径。
//
// 摊开的键名取两套协议的并集（OpenAI 的 max_tokens、Anthropic 的
// input_schema 之类）。统一结构本身不保留「原始线格式」的标记，所以骨架的键名
// 不能与客户端发来的字节逐一对应——它对应的是**解析后的请求**，
// 这一点对读骨架的人是必要说明。
func requestToMap(req *channel.Request) map[string]any {
	m := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		m["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		m["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		m["reasoning_effort"] = req.ReasoningEffort
	}

	if len(req.Messages) > 0 {
		messages := make([]any, 0, len(req.Messages))
		for _, msg := range req.Messages {
			messages = append(messages, map[string]any{
				"role":    msg.Role,
				"content": blocksToMap(msg.Content),
			})
		}
		m["messages"] = messages
	}

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			entry := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				entry["description"] = tool.Description
			}
			if tool.InputSchema != nil {
				entry["input_schema"] = tool.InputSchema
			}
			tools = append(tools, entry)
		}
		m["tools"] = tools
	}

	if len(req.Extra) > 0 {
		m["extra"] = req.Extra
	}
	if len(req.Headers) > 0 {
		headers := make(map[string]any, len(req.Headers))
		for k, v := range req.Headers {
			headers[k] = v
		}
		m["headers"] = headers
	}
	return m
}

// blocksToMap 把内容块序列摊成线格式结构。
func blocksToMap(blocks []channel.ContentBlock) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, blockToMap(b))
	}
	return out
}

// blockToMap 把一个内容块摊成线格式结构，字段名取两套协议的并集。
//
// 类型缺失时写 unknown：空串在骨架里与「字段没写」分不开，
// 而「上游没给类型」本身是一条值得看见的信息。
func blockToMap(b channel.ContentBlock) map[string]any {
	out := map[string]any{"type": blockTypeName(b)}

	switch b.Type {
	case "text":
		out["text"] = b.Text
	case "image":
		// URL 与 Base64 二选一，只摊出实际用的那一个：
		// 两个字段都写会让骨架里出现一个空的二进制占位。
		switch {
		case b.ImageBase64 != "":
			out["data"] = b.ImageBase64
		case b.ImageURL != "":
			out["url"] = b.ImageURL
		default:
			out["data"] = ""
		}
		if b.ImageMediaType != "" {
			out["media_type"] = b.ImageMediaType
		}
	case "tool_use":
		out["id"] = b.ToolUseID
		out["name"] = b.ToolName
		out["input"] = b.ToolInput
	case "tool_result":
		out["tool_use_id"] = b.ToolUseID
		out["content"] = b.ToolInput
	case "thinking":
		out["thinking"] = b.Text
	default:
		// 未知块类型：把可能承载内容的字段都摊出来，让允许表逐字段决定
		// 怎么处理，而不是在这里猜哪一个是内容。
		if b.Text != "" {
			out["text"] = b.Text
		}
		if b.ToolInput != nil {
			out["input"] = b.ToolInput
		}
		if b.ImageBase64 != "" {
			out["data"] = b.ImageBase64
		}
		if b.ImageURL != "" {
			out["url"] = b.ImageURL
		}
		if b.ImageMediaType != "" {
			out["media_type"] = b.ImageMediaType
		}
		if b.ToolUseID != "" {
			out["id"] = b.ToolUseID
		}
		if b.ToolName != "" {
			out["name"] = b.ToolName
		}
	}
	return out
}

// blockTypeName 取块的类型名，缺失时返回 unknown。
func blockTypeName(b channel.ContentBlock) string {
	if b.Type != "" {
		return b.Type
	}
	return typeUnknown
}

// responseToMap 把统一格式的响应摊成线格式结构。
func responseToMap(resp *channel.Response) map[string]any {
	m := map[string]any{
		"upstream_model": resp.UpstreamModel,
		"finish_reason":  resp.FinishReason,
	}
	if len(resp.Message.Content) > 0 {
		m["message"] = map[string]any{
			"role":    resp.Message.Role,
			"content": blocksToMap(resp.Message.Content),
		}
	}
	m["usage"] = usageToMap(resp.Usage)
	return m
}

// usageToMap 把归一化用量摊成线格式结构。
//
// 用量是计费依据，全部按真实值处理：算错 token 就是算错钱，而它本身不含用户
// 内容。取值为零的分项不写进骨架，让「这个渠道不报缓存明细」与「缓存明细是 0」
// 在骨架里能分开看。
func usageToMap(u channel.Usage) map[string]any {
	m := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
	}
	if u.CacheReadTokens != 0 {
		m["cache_read_input_tokens"] = u.CacheReadTokens
	}
	if u.CacheWriteTokens != 0 {
		m["cache_creation_input_tokens"] = u.CacheWriteTokens
	}
	if u.ReasoningTokens != 0 {
		m["reasoning_tokens"] = u.ReasoningTokens
	}
	if len(u.UnitCounts) > 0 {
		units := make(map[string]any, len(u.UnitCounts))
		for k, v := range u.UnitCounts {
			units[k] = v
		}
		m["unit_counts"] = units
	}
	return m
}
