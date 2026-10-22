package channels

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
)

// 端点对照：写入路径与 doctor 共用的**唯一一份**判据。
//
// 模型目录里可以声明「这个模型只在这些端点上被服务」（见 catalog.Capabilities
// 的 Endpoints），而一条渠道的 type 决定它把请求发到哪条端点（见
// channel.Protocol.Endpoint）。两边对不上时请求会在**上游**失败：400
// unsupported_model，从我们这边看与「模型不存在」长得一样，而那条渠道自己
// 看不出任何问题（status 是 enabled、模型在 models 白名单里）。
//
// 所以挡在写入侧。写入侧（Admin.checkModelEndpoints）与 doctor 都按这一份判：
// 两处各写一份的症状是「建的时候放行、doctor 报错」（或反过来），比不校验更坏
// ——两句话说的是同一件事，就得是同一份来源。
//
// 判据与措辞都收在 EndpointFit 上，两个调用方从同一份数据说同一句话。

// EndpointFit 是「模型在目录里声明的端点」与「这条渠道打的那条端点」的对照结果。
//
// 它不是「能不能服务」的一个布尔量：判据有三档（未知、明确不支持、按集合判），
// 而调用方要能说出**是哪两条端点对不上**，所以这里把两侧取值都带出来，
// 由 Summary/Reason 渲染成一句人话。
//
// 字段一律不导出，只能经 EndpointFitFor 构造：渠道打哪条端点由那张表算
// （channel.Protocol），**不让调用方自己填**——填一个就等于绕开了唯一入口，
// 而绕开正是这条判据要防的事。模型名不在里面：判据只与「渠道类型」和「目录
// 声明的端点」有关，名字是调用方的上下文，由它自己拼。
type EndpointFit struct {
	// channelTypes / channelEndpoints 是这条渠道声明的协议与它们各自打的端点，
	// 两片一一对应。单协议渠道只有一个元素，多协议时每个协议一条。
	channelTypes     []string
	channelEndpoints []channel.Endpoint
	declared         []channel.Endpoint
}

// EndpointFitFor 组装一份对照：**只声明一种协议**的渠道走这里。
//
// declared 的三态原样传入（见 DeclaredEndpoints）；渠道打哪条端点由
// channel.Protocol 算——**它是「渠道类型 → 端点」那张表的唯一入口**，任何
// 调用方都不该自己再写一份 switch：写一份就等于抄了半张表，其余几档会随
// 类型的新增而漂移。
func EndpointFitFor(channelType string, declared []channel.Endpoint) EndpointFit {
	return EndpointFitForProtocols([]string{channelType}, declared)
}

// EndpointFitForProtocols 组装一份对照：渠道声明了**一组**协议。
//
// 写渠道（Admin.checkModelEndpoints）与 doctor 都用它：前者判「这次写进去的
// 协议集合」，后者判「库里那一条声明的集合」。单元素切片与 EndpointFitFor
// 等价，包括报错措辞。
//
// # 只有全部协议都对不上才算对不上
//
// 多协议渠道只要有一条端点被目录声明，这个模型就能被服务；拦下它是把一个
// 能用的配置拒掉。所以 Mismatch 判的是「一条都不在其中」。
func EndpointFitForProtocols(protocols []string, declared []channel.Endpoint) EndpointFit {
	endpoints := make([]channel.Endpoint, 0, len(protocols))
	for _, p := range protocols {
		endpoints = append(endpoints, channel.Protocol(p).Endpoint())
	}
	return EndpointFit{
		channelTypes:     protocols,
		channelEndpoints: endpoints,
		declared:         declared,
	}
}

// Mismatch 报告这份声明与这条渠道**明确**对不上。
//
// 两档未知一律不算对不上（放行）：
//
//   - 渠道类型算不出端点（这个类型没有对应的协议）；
//   - 目录里没有这个模型（declared 为 nil）。
//
// 与选路侧的端点评据同一条处置：不知道就别拦，否则刚接一家上游、目录还空着时
// 一条渠道都建不出来。
func (f EndpointFit) Mismatch() bool {
	if f.declared == nil || len(f.channelEndpoints) == 0 {
		return false
	}
	// 任一协议的端点算不出来（本版本不认识那个协议）都算「不知道」：这条
	// 渠道可能正是靠那一条端点服务这个模型的，拦下它比放行更糟。
	for _, endpoint := range f.channelEndpoints {
		if endpoint == "" {
			return false
		}
	}
	for _, endpoint := range f.channelEndpoints {
		if slices.Contains(f.declared, endpoint) {
			return false
		}
	}
	return true
}

// Summary 说清对不上的两侧：模型在目录里声明的端点、这条渠道的类型与端点。
//
// 写作「在目录里声明的端点是 X，而这条渠道的类型是 T（它把请求发到 Y）」，
// 主语由调用方各自补：写入侧补「模型 "m"」，doctor 补「渠道 N（名字）的
// models 里有 "m"」。措辞不在这里写主语，是为了让两处说的是同一句话的同一截。
//
// 单协议渠道的措辞与加多协议之前逐字相同；多协议那条另说一句，把整组协议
// 与它们各自打的端点都列出来——只说「类型是 X」会让读的人以为还有别的协议
// 没被考虑。
func (f EndpointFit) Summary() string {
	if len(f.channelTypes) == 1 {
		return fmt.Sprintf("在目录里声明的端点是 %s，而这条渠道的类型是 %q（它把请求发到 %s）",
			declaredEndpointsText(f.declared), f.channelTypes[0], f.channelEndpoints[0])
	}
	return fmt.Sprintf("在目录里声明的端点是 %s，而这条渠道声明的协议是 %s（它们把请求发到 %s）",
		declaredEndpointsText(f.declared), quotedTypes(f.channelTypes), endpointListText(f.channelEndpoints))
}

// quotedTypes 把一组协议名渲染成 `"openai"、"anthropic"` 这样的句子。
func quotedTypes(types []string) string {
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, strconv.Quote(t))
	}
	return strings.Join(parts, "、")
}

// endpointListText 把一组端点渲染成 `/v1/chat/completions、/v1/messages`。
func endpointListText(endpoints []channel.Endpoint) string {
	parts := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		parts = append(parts, string(e))
	}
	return strings.Join(parts, "、")
}

// Reason 说清为什么会被拦、下一步怎么办。
//
// 这句同时经命令行与管理面（HTTP）返回给使用者，所以只写两个入口都成立的东西
// （见 LESSONS.md 第 39 条）：没有命令名、没有 `--参数`、没有退出码——管理面的
// 调用方读到的是一个它没有的东西。doctor 报问题时用同一句。
func (f EndpointFit) Reason() string {
	return "往这条端点发，上游会回 unsupported_model，且从我们这边看与「模型不存在」无从分辨。" +
		"要么给这个模型补上这条端点（改模型目录里这一条声明），要么把它从这条渠道的 models 里去掉"
}

// DeclaredEndpoints 读模型目录对这个模型声明的端点清单。
//
// 它是写入侧与 doctor 读目录的**同一处**：三态原样交回调用方，不折平
// （nil=未知、空切片=明确一条都不支持、非空=按集合判，见 EndpointFit）。
// 折平任意两个都会让「没配过」与「配了说没有」重新合成一个值，而那正是这条三态
// 要分开的东西。
//
// 目录里没有这个模型时返回 nil（未知，放行）——与 Source.profile 同一处置。
// 读目录失败**报错而不降级成未知**（与 Source.profile 同一条理由）：降级看起来
// 更宽容，实际是把一次查询故障变成「判据静默失效」，而失效与「这个部署没配目录」
// 在日志里分不开。
func DeclaredEndpoints(ctx context.Context, cat *catalog.Catalog, model string) ([]channel.Endpoint, error) {
	cons, err := cat.Constraints(ctx, model)
	if errors.Is(err, catalog.ErrModelNotFound) {
		// 目录里没有这个模型：未知，放行。nil 是「未知」这个合法状态，
		// 不是「出错了但没报」。
		return nil, nil //nolint:nilnil // 见上
	}
	if err != nil {
		return nil, fmt.Errorf("读取模型 %q 的目录声明失败: %w", model, err)
	}
	return cons.Capabilities.Endpoints, nil
}

// declaredEndpointsText 把目录声明的那几条端点渲染成一句人话，供报错用。
//
// 空清单（`{}`：明确一条都不支持）单独说一句——「只在这些端点上服务：」
// 后面空着读不出任何意思，而这一档恰恰意味着**任何渠道**都服务不了它。
func declaredEndpointsText(endpoints []channel.Endpoint) string {
	if len(endpoints) == 0 {
		return "「一条都不支持」"
	}
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parts = append(parts, string(endpoint))
	}
	return strings.Join(parts, ", ")
}
