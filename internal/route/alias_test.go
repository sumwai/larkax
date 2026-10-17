package route

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 这个文件测的是「规范名 → 上游名」这一跳：选路之后、编码请求之前，
// 按选中的渠道翻一次。
//
// 判据只有一条，而且只能从**执行器收到的那份请求**上看：发出去的是不是
// 上游名。断言内部某个字段「对了」不算——内部字段对了而发出去的仍是规范名，
// 正是这个缺陷原来的形状（见 #338）。

// identityAliases 是「规范名就是上游名」的来源。
//
// 它代表**一条别名都没配**的部署：大多数部署都是这样，而它们必须照旧能打通。
// 用在不需要关心别名的用例上（选路、打分、重试那些）。
type identityAliases struct{}

func (identityAliases) Alias(_ context.Context, _ int64, canonical string) (string, map[string]any, error) {
	return canonical, noOverrides, nil
}

// aliasSource 是一个可配的别名来源，按渠道给出上游名。
//
// 按渠道而不是全局：别名本来就是逐渠道配的（同一个规范名在这家叫 vendor/x、
// 在那家叫 x），而「换渠道时按新渠道重翻一次」这条判据只有在两个渠道给出
// 不同的名字时才分辨得出来。
type aliasSource struct {
	// byChannel 是「渠道 id → 上游名」。没有的渠道按同名回落。
	byChannel map[int64]string
	// overrides 是「渠道 id → 这条别名上配的参数覆盖」。没有的渠道就是没配。
	//
	// 逐渠道而不是全局：覆盖与上游名来自**同一条**别名行，所以「换渠道之后
	// 覆盖也跟着换」这条判据只有在两个渠道各配一份时才分辨得出来。
	overrides map[int64]map[string]any
	// errs 是「按渠道给出的失败」，用来复现「这个规范名配过别名，但没有一条
	// 覆盖**这个**渠道」——所以它是逐渠道的，不是全局一个开关：全局开关
	// 会让「第一个候选翻得出来、第二个翻不出来」这种组合造不出来，而那正是
	// 「换渠道之后才失败」那条用例要的形状。
	errs map[int64]error
	// calls 按顺序记下问过谁，用来断言「翻了几次、按哪个渠道翻的」。
	calls []aliasQuery
}

type aliasQuery struct {
	channelID int64
	canonical string
}

func (a *aliasSource) Alias(_ context.Context, channelID int64, canonical string) (string, map[string]any, error) {
	a.calls = append(a.calls, aliasQuery{channelID: channelID, canonical: canonical})
	if err, ok := a.errs[channelID]; ok {
		return "", noOverrides, err
	}
	name, ok := a.byChannel[channelID]
	if !ok {
		name = canonical
	}
	override, ok := a.overrides[channelID]
	if !ok {
		override = noOverrides
	}
	return name, override, nil
}

// noOverrides 是「这条别名没配覆盖」。
//
// 用空对象而不是 nil，与库里那一列的写法一致（没配时存的是 `{}`）：nil 与
// 空对象在 Go 里判起来一样（len 都是 0），但两个来源给出两种零值的话，
// 「读出来是 nil 还是 {}」会变成一件要靠调试才知道的事。
var noOverrides = map[string]any{}

// errNoAlias 模拟 internal/catalog 的 ErrAliasNotFound：这个规范名配过别名，
// 但没有一条覆盖这个渠道。
var errNoAlias = errors.New("没有对应的上游模型名")

func routerWithAliases(seed int64, aliases ModelAliases) *Router {
	return &Router{Rand: rand.New(rand.NewSource(seed)), Aliases: aliases}
}

// 上游收到的必须是**上游名**。这是唯一能证明别名真的生效的断言。
func TestChatSendsUpstreamModelName(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{byChannel: map[int64]string{1: "vendor/my-model"}}
	r := routerWithAliases(1, aliases)

	if _, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model")}, "my-model"); err != nil {
		t.Fatalf("出错: %v", err)
	}

	if len(exec.sent) != 1 {
		t.Fatalf("执行器收到 %d 份请求，期望 1", len(exec.sent))
	}
	if got := exec.sent[0].Model; got != "vendor/my-model" {
		t.Errorf("上游收到的 model = %q，期望 %q（别名配了就要生效）", got, "vendor/my-model")
	}
	if len(aliases.calls) != 1 || aliases.calls[0].channelID != 1 || aliases.calls[0].canonical != "my-model" {
		t.Errorf("查询记录 = %+v，期望按渠道 1 查 my-model", aliases.calls)
	}
}

// 调用方那份请求**不能被改动**：它还要用来渲染响应，而报给调用方的模型名
// 是**他要的那个**（见 internal/api 的 renderSuccess）。
//
// 就地改 Model 的表现是响应里的 model 变成上游名，而请求本身照样成功、
// 账单照样正确——只有客户端看到的名字不对。
func TestChatDoesNotTouchCallerRequest(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	r := routerWithAliases(1, &aliasSource{byChannel: map[int64]string{1: "vendor/my-model"}})

	req := &channel.Request{Model: "my-model"}
	if _, err := r.Chat(context.Background(), exec, req, []Candidate{candidate(1, "my-model")}, "my-model"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if req.Model != "my-model" {
		t.Errorf("调用方那份请求的 model 被改成了 %q", req.Model)
	}
}

// 换渠道时**每个候选各翻一次**：别名逐渠道配，第二个渠道收到的必须是它
// 自己的名字。
//
// 翻一次、所有候选共用的表现是第二个渠道收到第一个渠道的名字——那是一次
// 静默发错：两家叫法偶尔相同，于是请求成功、账单正常，而「用的是别家的
// 名字」没有任何地方报出来。
func TestChatTranslatesPerCandidateOnFailover(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}, resp: &channel.Response{}}
	aliases := &aliasSource{byChannel: map[int64]string{
		1: "vendor-a/my-model",
		2: "vendor-b/my-model",
	}}
	r := routerWithAliases(1, aliases)

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model", withPriority(10)), candidate(2, "my-model", withPriority(1))}, "my-model")
	if err != nil {
		t.Fatalf("应换渠道成功: %v", err)
	}
	if res.Channel.ID != 2 {
		t.Fatalf("应落在第二个候选上，实际 %d", res.Channel.ID)
	}
	if len(exec.sent) != 2 {
		t.Fatalf("执行器收到 %d 份请求，期望 2", len(exec.sent))
	}
	if exec.sent[0].Model != "vendor-a/my-model" {
		t.Errorf("第一次尝试发的是 %q，期望 %q", exec.sent[0].Model, "vendor-a/my-model")
	}
	if exec.sent[1].Model != "vendor-b/my-model" {
		t.Errorf("第二次尝试发的是 %q，期望 %q（按新渠道重翻一次）", exec.sent[1].Model, "vendor-b/my-model")
	}
	if res.UpstreamModel != "vendor-b/my-model" {
		t.Errorf("结果里的上游名 = %q，期望最后成功那次用的名字", res.UpstreamModel)
	}
}

// 别名查不到时**报错，不静默回落成规范名**。
//
// 回落的表现是「上游说这个模型不存在」，排查方向指向对方——而真正要改的是
// 我们这一侧的别名配置。所以这里同时钉两件事：错误能认出 ErrAliasNotFound，
// 以及**一次上游调用都没发生**（回落会真的发出去）。
func TestChatFailsWhenAliasMissing(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{errs: map[int64]error{1: fmt.Errorf("%w：渠道 1 的 \"my-model\"", errNoAlias)}}
	r := routerWithAliases(1, aliases)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if err == nil {
		t.Fatal("别名查不到应报错")
	}
	if !errors.Is(err, errNoAlias) {
		t.Errorf("错误应仍能认出别名缺失: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("不该发起任何上游调用，实际调用了 %v", exec.calls)
	}
}

// 别名查不到时**不换下一个候选**：那是配置缺陷，不是上游抖动。
//
// 换下去会把「这个渠道没配别名」说成「所有候选都试过了」，而后者看起来像
// 上游全挂了——排查方向正好反了。所以这里断言第二个候选一次都没被问过。
func TestChatDoesNotFailOverOnAliasMissing(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	r := routerWithAliases(1, &aliasSource{errs: map[int64]error{1: errNoAlias}})

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model", withPriority(10)), candidate(2, "my-model", withPriority(1))}, "my-model")
	if !errors.Is(err, errNoAlias) {
		t.Fatalf("错误 = %v，期望别名缺失", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("不该发起任何上游调用，实际调用了 %v", exec.calls)
	}
}

// 别名缺失时，**之前失败的那几次尝试仍要带回去**：那些调用真的发生过，
// 调用方要按它们记渠道健康度。返回一个 nil 结果会让它们静默消失。
func TestChatKeepsEarlierAttemptsWhenAliasMissing(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}, resp: &channel.Response{}}
	// 第一个候选翻得出来（否则轮不到第二次尝试），第二个翻不出来。
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor-a/my-model"},
		errs:      map[int64]error{2: errNoAlias},
	}
	r := routerWithAliases(1, aliases)

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model", withPriority(10)), candidate(2, "my-model", withPriority(1))}, "my-model")
	if !errors.Is(err, errNoAlias) {
		t.Fatalf("错误 = %v，期望别名缺失", err)
	}
	if res == nil {
		t.Fatal("失败时也要把已经发生过的尝试带回去")
	}
	if len(res.Attempts) != 1 || res.Attempts[0].ChannelID != 1 || res.Attempts[0].Err == nil {
		t.Errorf("尝试记录 = %+v，期望保留渠道 1 的那次失败", res.Attempts)
	}
}

// 没接别名来源时**每个请求都失败**，而不是静默按规范名发出去。
//
// 静默回落会让「忘接了」与「这个部署不用别名」在响应、日志和账单上完全
// 一样，而前者意味着整个功能没生效。这一条钉的是那个选择。
func TestChatWithoutAliasSourceFails(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	r := routerWithAliases(1, nil)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if !errors.Is(err, ErrNoAliasSource) {
		t.Fatalf("错误 = %v，期望 ErrNoAliasSource", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("不该发起任何上游调用，实际调用了 %v", exec.calls)
	}
}

// 结果里要带上**实际发出去**的上游名，供上层落进 billing_requests。
// 事后重查一次别名答不出这个问题：配置可能已经改过。
func TestChatResultCarriesUpstreamModel(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	r := routerWithAliases(1, &aliasSource{byChannel: map[int64]string{1: "vendor/my-model"}})

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model"},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.UpstreamModel != "vendor/my-model" {
		t.Errorf("结果里的上游名 = %q，期望 %q", res.UpstreamModel, "vendor/my-model")
	}
}

// ── 参数覆盖 ────────────────────────────────────────────────────────────
//
// 覆盖与上游名来自**同一条**别名行，一起在 upstreamRequest 里应用到那次尝试
// 的请求副本上。判据同样只能从**执行器收到的那份请求**上看。
//
// 这里的用例钉的是「谁赢」这三条语义的**应用**那一步；白名单与取值合法性在
// internal/overrides 里有各自的用例，不在这里重测一遍。

// 调用方**没给** temperature 时，覆盖值生效。
//
// 这是这一列存在的意义：商家配一个默认采样参数，让不传它的调用方也走这个值。
func TestChatAppliesOverrideWhenCallerOmittedIt(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"temperature": 0.2, "top_p": 0.9}},
	}
	r := routerWithAliases(1, aliases)

	if _, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model", MaxTokens: 64},
		[]Candidate{candidate(1, "my-model")}, "my-model"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(exec.sent) != 1 {
		t.Fatalf("执行器收到 %d 份请求，期望 1", len(exec.sent))
	}
	got := exec.sent[0]
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("上游收到的 temperature = %v，期望 0.2（调用方没给，取覆盖值）", ptrText(got.Temperature))
	}
	if got.TopP == nil || *got.TopP != 0.9 {
		t.Errorf("上游收到的 top_p = %v，期望 0.9（调用方没给，取覆盖值）", ptrText(got.TopP))
	}
}

// 调用方**给了** temperature 时，**调用方赢**。
//
// 反过来（覆盖赢）的表现是「我设的 temperature 没生效」——一件无法解释的事，
// 而且与「这一列根本没读」在调用方看来一模一样。
func TestChatCallerWinsOnSamplingParams(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"temperature": 0.2, "top_p": 0.9}},
	}
	r := routerWithAliases(1, aliases)

	req := &channel.Request{Model: "my-model", MaxTokens: 64, Temperature: f64(0.7), TopP: f64(0.5)}
	if _, err := r.Chat(context.Background(), exec, req, []Candidate{candidate(1, "my-model")}, "my-model"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := exec.sent[0]
	if *got.Temperature != 0.7 {
		t.Errorf("上游收到的 temperature = %v，期望 0.7（调用方给了就以它为准）", *got.Temperature)
	}
	if *got.TopP != 0.5 {
		t.Errorf("上游收到的 top_p = %v，期望 0.5（调用方给了就以它为准）", *got.TopP)
	}
}

// max_tokens 反过来：**覆盖赢**，哪怕调用方给了、哪怕覆盖值更大。
//
// 与上一条成对钉着，因为两者是**相反**的：只钉一条的话，把两条判据写成同一个
// 方向（都让调用方赢，或都让覆盖赢）在用例里看不出来。max_tokens 归商家是因为
// 它是成本的闸门，而调用方给的数管的是他自己这次想要多长。
func TestChatMerchantWinsOnMaxTokens(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"max_tokens": 2048}},
	}
	r := routerWithAliases(1, aliases)

	// 覆盖值比调用方给的大——预留上界要按它算（见 internal/gateway 的
	// outputBound），所以这一条同时是那条上界的输入。
	if _, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model", MaxTokens: 64},
		[]Candidate{candidate(1, "my-model")}, "my-model"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got := exec.sent[0].MaxTokens; got != 2048 {
		t.Errorf("上游收到的 max_tokens = %d，期望 2048（这个字段商家赢）", got)
	}
}

// 换渠道时覆盖**跟着新渠道走**：第二个渠道收到的是它自己那条别名的覆盖。
//
// 只翻一次、所有候选共用一份覆盖的表现是第二个渠道收到了第一个渠道的配置
// ——采样参数错一位通常看不出来，而它正是「配了 A 渠道却影响了 B 渠道」。
func TestChatOverridesFollowTheChannelOnFailover(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor-a/my-model", 2: "vendor-b/my-model"},
		overrides: map[int64]map[string]any{
			1: {"temperature": 0.1},
			2: {"temperature": 0.9},
		},
	}
	r := routerWithAliases(1, aliases)

	if _, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model", MaxTokens: 64},
		[]Candidate{candidate(1, "my-model", withPriority(10)), candidate(2, "my-model", withPriority(1))}, "my-model"); err != nil {
		t.Fatalf("应换渠道成功: %v", err)
	}
	if len(exec.sent) != 2 {
		t.Fatalf("执行器收到 %d 份请求，期望 2", len(exec.sent))
	}
	if got := exec.sent[0].Temperature; got == nil || *got != 0.1 {
		t.Errorf("第一个渠道收到的 temperature = %v，期望 0.1", ptrText(got))
	}
	if got := exec.sent[1].Temperature; got == nil || *got != 0.9 {
		t.Errorf("第二个渠道收到的 temperature = %v，期望 0.9（按新渠道重新取覆盖）", ptrText(got))
	}
}

// 覆盖**不能改动调用方那份请求**：它还要用来渲染响应，而采样参数是调用方
// 自己声明的东西（见 internal/api）。
//
// 这条与 TestChatDoesNotTouchCallerRequest 是同一个形状，判据落在 temperature
// 与 max_tokens 上：temperature / top_p 在 Apply 里是**换指针**，所以「就地改
// 调用方那份」的表现是调用方请求上的字段跟着变，而请求本身照样成功。
func TestChatOverridesDoNotTouchCallerRequest(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"temperature": 0.2, "max_tokens": 2048}},
	}
	r := routerWithAliases(1, aliases)

	req := &channel.Request{Model: "my-model", MaxTokens: 64}
	if _, err := r.Chat(context.Background(), exec, req, []Candidate{candidate(1, "my-model")}, "my-model"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if req.Temperature != nil {
		t.Errorf("调用方那份请求被写上了 temperature = %v", *req.Temperature)
	}
	if req.MaxTokens != 64 {
		t.Errorf("调用方那份请求的 max_tokens 被改成了 %d，期望 64", req.MaxTokens)
	}
}

// 别名上的覆盖非法时**报错，不静默跳过**。
//
// 「写的时候校验过」不足以排除这种情况：库里可能有绕过写入路径写进去的历史行，
// 而静默跳过的表现是「这条配置配了不生效」——正是这一列要消掉的东西。
func TestChatFailsOnInvalidOverride(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"model": "vendor/other"}},
	}
	r := routerWithAliases(1, aliases)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model", MaxTokens: 64},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if err == nil {
		t.Fatal("覆盖里有不允许的键时应报错，而不是静默跳过")
	}
	if !strings.Contains(err.Error(), `"model"`) {
		t.Errorf("错误里要说得出是哪个键: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("不该发起任何上游调用，实际调用了 %v", exec.calls)
	}
}

// ── 交回发出去的那份请求 ────────────────────────────────────────────────
//
// Result / StreamResult 把**执行器收到的那一个对象**交回上层。上层有两个用它
// 的地方（见 internal/gateway 的 snapshotRequest）：请求快照要记的是发出去的那
// 份，而它在选路之前根本算不出来——上游名与参数覆盖都是逐渠道的。

// 交回来的是**执行器收到的那一个对象**，不是照着它再拼一份。
//
// 判据是同一个指针，不是「内容相等」：内容相等在「复制一份、再按同一套判据算
// 一遍覆盖」的实现上也成立，而那种实现正是要挡的——覆盖的判据只该有一处
// （见 internal/overrides），拼第二份就是第二处。
func TestResultCarriesTheVeryRequestSentToUpstream(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"temperature": 0.2}},
	}
	r := routerWithAliases(1, aliases)

	caller := &channel.Request{Model: "my-model", MaxTokens: 64}
	res, err := r.Chat(context.Background(), exec, caller, []Candidate{candidate(1, "my-model")}, "my-model")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(exec.sentRefs) != 1 {
		t.Fatalf("执行器被调用 %d 次，期望 1 次", len(exec.sentRefs))
	}
	if res.OutboundRequest != exec.sentRefs[0] {
		t.Error("结果里那份请求与执行器收到的不是同一个对象——它该是发出去的那个副本本身")
	}
	// 也不能是调用方那份：调用方那份的 model 是规范名、覆盖也没写上去。
	if res.OutboundRequest == caller {
		t.Error("结果里交回的是调用方那份请求——就地改它会把响应里的 model 也变成上游名")
	}
	if got := res.OutboundRequest.Model; got != "vendor/my-model" {
		t.Errorf("交回来那份请求的 model = %q，期望上游名 %q", got, "vendor/my-model")
	}
	if got := res.OutboundRequest.Temperature; got == nil || *got != 0.2 {
		t.Errorf("交回来那份请求的 temperature = %v，期望覆盖之后的 0.2", ptrText(got))
	}
	if caller.Temperature != nil {
		t.Errorf("调用方那份请求被写上了 temperature = %v", *caller.Temperature)
	}
}

// 流式那条路交回的是同一个东西（口径见 Result.OutboundRequest）。
func TestStreamResultCarriesTheVeryRequestSentToUpstream(t *testing.T) {
	exec := &fakeStreamExecutor{results: map[int64]error{}}
	aliases := &aliasSource{
		byChannel: map[int64]string{1: "vendor/my-model"},
		overrides: map[int64]map[string]any{1: {"max_tokens": 2048}},
	}
	r := routerWithAliases(1, aliases)

	res, err := r.ChatStream(context.Background(), exec,
		&channel.Request{Model: "my-model", MaxTokens: 64, Stream: true},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(exec.sentRefs) != 1 {
		t.Fatalf("执行器被调用 %d 次，期望 1 次", len(exec.sentRefs))
	}
	if res.OutboundRequest != exec.sentRefs[0] {
		t.Error("流式结果里那份请求与执行器收到的不是同一个对象")
	}
	if got := res.OutboundRequest.MaxTokens; got != 2048 {
		t.Errorf("交回来那份请求的 max_tokens = %d，期望覆盖之后的 2048", got)
	}
}

// 一次都没发出去时它是 **nil**，调用方据此分辨「没有发出去的那份」。
//
// nil 而不是一份空的请求：上层要按这个值决定快照退回调用方那份（见
// internal/gateway 的 snapshotRequest）。给一份空的请求会让「这次请求根本没
// 发出去」与「发出去的那份恰好是零值」在调用方那里同形，而前者的快照只能是
// 调用方发来的那份。
func TestResultOutboundRequestIsNilWhenNothingWasSent(t *testing.T) {
	t.Run("别名翻不出来", func(t *testing.T) {
		exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
		r := routerWithAliases(1, &aliasSource{errs: map[int64]error{1: errNoAlias}})

		res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model", MaxTokens: 64},
			[]Candidate{candidate(1, "my-model")}, "my-model")
		if !errors.Is(err, errNoAlias) {
			t.Fatalf("错误 = %v，期望别名缺失", err)
		}
		if res == nil {
			t.Fatal("失败时也要把结果带回来（上层按它拿「打到了哪儿」）")
		}
		if res.OutboundRequest != nil {
			t.Errorf("一份都没发出去，OutboundRequest 应当是 nil，实际 %+v", res.OutboundRequest)
		}
	})

	t.Run("context 已取消", func(t *testing.T) {
		exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
		r := routerWithAliases(1, &aliasSource{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		res, err := r.Chat(ctx, exec, &channel.Request{Model: "my-model", MaxTokens: 64},
			[]Candidate{candidate(1, "my-model")}, "my-model")
		if err == nil {
			t.Fatal("context 已取消时应报错")
		}
		if res == nil || res.OutboundRequest != nil {
			t.Errorf("一次都没尝试时 OutboundRequest 应当是 nil，实际 %+v", res)
		}
		if len(exec.calls) != 0 {
			t.Errorf("不该发起任何上游调用，实际调用了 %v", exec.calls)
		}
	})

	// 第一个候选真的发出去过、第二个翻不出别名：留着**第一次那份**。
	//
	// 「失败的那次也算尝试」这条口径说的就是它：那次调用发生过，快照要记的正是
	// 上游实际收到的东西。把整个字段清成 nil 的话，这一次请求在快照里会退成
	// 「调用方发来的那份」，而上游明明收到过别的东西。
	t.Run("第二次尝试翻不出来时留着第一次那份", func(t *testing.T) {
		exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}, resp: &channel.Response{}}
		aliases := &aliasSource{
			byChannel: map[int64]string{1: "vendor-a/my-model"},
			errs:      map[int64]error{2: errNoAlias},
		}
		r := routerWithAliases(1, aliases)

		res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "my-model", MaxTokens: 64},
			[]Candidate{candidate(1, "my-model", withPriority(10)), candidate(2, "my-model", withPriority(1))}, "my-model")
		if !errors.Is(err, errNoAlias) {
			t.Fatalf("错误 = %v，期望别名缺失", err)
		}
		if res == nil || res.OutboundRequest == nil {
			t.Fatal("第一次尝试真的发出去过，OutboundRequest 不该是 nil")
		}
		if got := res.OutboundRequest.Model; got != "vendor-a/my-model" {
			t.Errorf("交回来那份请求的 model = %q，期望第一次真的发出去的那个名字", got)
		}
		if res.OutboundRequest != exec.sentRefs[0] {
			t.Error("交回来的该是执行器收到的那一个对象")
		}
	})
}

// f64 取一个 float64 的指针。用法是 f64(0.7)，stdio 上没有更短的写法。
func f64(v float64) *float64 { return &v }

// ptrText 把一个可空的采样参数渲染成能读的文本。
func ptrText(p *float64) string {
	if p == nil {
		return "nil（调用方没给，覆盖也没生效）"
	}
	return fmt.Sprintf("%v", *p)
}

// ── 流式 ────────────────────────────────────────────────────────────────

// 流式那条路与非流式同一个位置翻：上游收到的是上游名。
func TestChatStreamSendsUpstreamModelName(t *testing.T) {
	exec := &fakeStreamExecutor{results: map[int64]error{}}
	r := routerWithAliases(1, &aliasSource{byChannel: map[int64]string{1: "vendor/my-model"}})

	res, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "my-model", Stream: true},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(exec.sent) != 1 {
		t.Fatalf("执行器收到 %d 份请求，期望 1", len(exec.sent))
	}
	if got := exec.sent[0].Model; got != "vendor/my-model" {
		t.Errorf("上游收到的 model = %q，期望 %q", got, "vendor/my-model")
	}
	if res.UpstreamModel != "vendor/my-model" {
		t.Errorf("结果里的上游名 = %q，期望 %q", res.UpstreamModel, "vendor/my-model")
	}
}

// 流式那条路上别名缺失同样是失败，而且**一个字都没发出去**。
func TestChatStreamFailsWhenAliasMissing(t *testing.T) {
	exec := &fakeStreamExecutor{results: map[int64]error{}}
	r := routerWithAliases(1, &aliasSource{errs: map[int64]error{1: errNoAlias}})

	_, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "my-model", Stream: true},
		[]Candidate{candidate(1, "my-model")}, "my-model")
	if !errors.Is(err, errNoAlias) {
		t.Fatalf("错误 = %v，期望别名缺失", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("不该发起任何上游调用，实际调用了 %v", exec.calls)
	}
}
