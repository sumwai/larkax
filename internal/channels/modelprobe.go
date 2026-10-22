package channels

// 本文件是「拿一份还没保存的地址与凭证去问上游有哪些模型」的读侧。
//
// # 为什么在 channels 包里
//
// 编辑一条已存渠道时，探测不该要求前端把凭证明文再传一次：那条路径要从库里
// 取密文、解密，而全仓库解明文的地方只该在这里（见包注释）。只给地址与凭证
// 的形态同样走这个类型，于是两种调用共用同一段「按渠道类型找适配器」的代码，
// 不会各自漂移。
//
// # 它不落库
//
// 与 ModelSync 的区别只有这一点：那边把清单写进 channel_upstream_models，
// 这边只回给调用方。探测是「我想看看」，不是一次同步；把它记下来会让
// 「上次同步的那一刻」变得不可信。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
)

// credentialPlaceholder 替换掉上游错误体里可能回显的凭证。
//
// 与公开场域那些占位符同形：人一眼看出这里被换过，也不会把它当成钥匙。
const credentialPlaceholder = "<redacted>"

// ProbeModelLimit 是一次探测返回的模型条数上限。
//
// 探测的用途是给渠道表单做多选，几千条已经远超能用的范围；再多的部分
// 截掉并置 Truncated，让调用方知道「这不是全部」。
const ProbeModelLimit = 2000

// ProbeRequest 是一次探测的输入。
//
// # 两种形态
//
//   - 给了 ChannelID：类型、地址、凭证默认取库里那一行。BaseURL 非空时覆盖
//     （编辑中的地址），Credential 非空时覆盖（试一把新钥匙）。
//   - 没给 ChannelID：Type 与 BaseURL 必填，Credential 可以为空（无鉴权的
//     自建上游）。
//
// 覆盖只发生在归属判据之后：先按 OwnerID 确认这条渠道是自己的，再谈换成
// 什么凭证。反过来会让「给 id + 换凭证」变成绕过归属的口子。
type ProbeRequest struct {
	ChannelID  int64
	Type       string
	BaseURL    string
	Credential string
	// OwnerID 非空表示商家会话：目标渠道的 owner_user_id 必须是它。
	// 为 nil 表示管理员或内部管理面，不限定归属。
	OwnerID *int64
}

// ProbeResult 是一次探测的结果。
type ProbeResult struct {
	// Supported 表示上游提供了模型列表。false 时 Models 是空切片。
	Supported bool
	// Models 是去重、按名字升序的模型名（最多 ProbeModelLimit 条）。
	Models []string
	// Total 是截断**之前**的条数：Models 被截断时，调用方还能知道上游
	// 一共给了多少。
	Total int
	// Truncated 表示清单超过 ProbeModelLimit 被截断。它说的是清单，不是
	// 响应体——响应体超限会作为错误返回。
	Truncated bool
}

// ModelProbe 向上游问一次模型清单。
type ModelProbe struct {
	db       *sql.DB
	sealer   *credential.Sealer
	registry *channel.Registry
}

// NewModelProbe 包装数据库、凭证封装器与适配器注册表。
//
// 只给地址与凭证的形态三种都用不到 db 与 sealer，构造时传 nil 即可。
func NewModelProbe(db *sql.DB, sealer *credential.Sealer, registry *channel.Registry) *ModelProbe {
	return &ModelProbe{db: db, sealer: sealer, registry: registry}
}

// Probe 发起一次探测。
//
// 三档结果：拿到清单、上游不支持（ErrUnsupported 不往上抛，折成
// Supported=false）、其他错误原样返回。
func (p *ModelProbe) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if p.registry == nil {
		return ProbeResult{}, errors.New("没有适配器注册表，无法探测上游模型")
	}
	target, err := p.target(ctx, req)
	if err != nil {
		return ProbeResult{}, err
	}
	adapter, err := p.registry.Lookup(target.channelType)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("渠道类型无效: %w", err)
	}

	models, err := adapter.ListModels(ctx, channel.Channel{
		ID:         req.ChannelID,
		Type:       target.channelType,
		BaseURL:    target.baseURL,
		Credential: target.credential,
	})
	if errors.Is(err, channel.ErrUnsupported) {
		return ProbeResult{Supported: false, Models: []string{}}, nil
	}
	if err != nil {
		return ProbeResult{}, fmt.Errorf("探测上游模型失败: %w", redactUpstream(err, target.credential))
	}
	return normalizeProbeModels(models), nil
}

// redactUpstream 把上游错误体里可能回显的凭证换掉。
//
// 明文的生命周期只在本包里（见 ProbeRequest 与包注释），所以「不许进错误
// 信息」也在本包落地：调用方拿到的 ErrUpstream.Body 已经不含这次用的凭证，
// 而它的错误链（网络错误、超时）本来就不带凭证。
func redactUpstream(err error, credential string) error {
	if credential == "" {
		return err
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) || up.Body == "" {
		return err
	}
	clone := *up
	clone.Body = strings.ReplaceAll(clone.Body, credential, credentialPlaceholder)
	return &clone
}

// probeTarget 是这次探测实际要打的东西。
type probeTarget struct {
	channelType string
	baseURL     string
	credential  string
}

// target 定出这次探测打的地址、类型与凭证。
func (p *ModelProbe) target(ctx context.Context, req ProbeRequest) (probeTarget, error) {
	if req.ChannelID <= 0 {
		if req.Type == "" {
			return probeTarget{}, errors.New("没有给渠道 id 时必须给 type")
		}
		baseURL, err := NormalizeBaseURL(req.BaseURL)
		if err != nil {
			return probeTarget{}, err
		}
		return probeTarget{
			channelType: req.Type,
			baseURL:     baseURL,
			credential:  req.Credential,
		}, nil
	}
	if p.db == nil {
		return probeTarget{}, errors.New("没有数据库连接，无法读取渠道")
	}

	var (
		chanType    string
		storedURL   string
		envelope    []byte
		ownerUserID sql.NullInt64
	)
	err := p.db.QueryRowContext(ctx, `
		SELECT type, base_url, credential_enc, owner_user_id
		FROM channels
		WHERE id = $1 AND `+channelAlive, req.ChannelID).
		Scan(&chanType, &storedURL, &envelope, &ownerUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return probeTarget{}, fmt.Errorf("%w：id %d", ErrChannelNotFound, req.ChannelID)
	}
	if err != nil {
		return probeTarget{}, fmt.Errorf("读取渠道 %d 失败: %w", req.ChannelID, err)
	}
	// 软删的渠道与不存在在调用方看来是同一件事：探测一个已经删掉的地址没有
	// 意义，而区分两者会告诉商家「这条曾经存在」。
	//
	// 这里用的是同一个 WHERE 片段（channelAlive），所以不必再判一次 deleted_at。
	if req.OwnerID != nil && (!ownerUserID.Valid || ownerUserID.Int64 != *req.OwnerID) {
		return probeTarget{}, fmt.Errorf("%w：id %d", ErrChannelNotFound, req.ChannelID)
	}

	out := probeTarget{channelType: chanType, baseURL: storedURL}
	if req.BaseURL != "" {
		baseURL, err := NormalizeBaseURL(req.BaseURL)
		if err != nil {
			return probeTarget{}, err
		}
		out.baseURL = baseURL
	}
	switch {
	case req.Credential != "":
		out.credential = req.Credential
	default:
		plaintext, err := openCredential(p.sealer, envelope)
		if err != nil {
			return probeTarget{}, fmt.Errorf("渠道 %d: %w", req.ChannelID, err)
		}
		out.credential = plaintext
	}
	return out, nil
}

// normalizeProbeModels 去重、排序并截断清单。
//
// 空名丢掉：上游偶尔会回一条 id 为空的行，把它带进多选只会让前端多出一个
// 点不动的选项。
func normalizeProbeModels(models []string) ProbeResult {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		if model == "" {
			continue
		}
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	slices.Sort(out)
	total := len(out)
	truncated := total > ProbeModelLimit
	if truncated {
		out = out[:ProbeModelLimit]
	}
	return ProbeResult{Supported: true, Models: out, Total: total, Truncated: truncated}
}
