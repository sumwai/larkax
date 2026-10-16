package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrAliasNotFound 表示没有从这个渠道到这个规范模型名的映射。
//
// 它与「查询失败」分开：没有别名是**配置缺失**（这个渠道还没配这个模型，
// 或者只配了平台级的默认名而这里查的是渠道级），而查询失败是数据库的问题。
// 两者的处置完全不同，所以不能都退化成一个笼统的错误。
var ErrAliasNotFound = errors.New("没有对应的上游模型名")

// UpstreamModel 把一个规范模型名翻成某个渠道上的上游模型名。
//
// # 为什么查渠道级优先于平台级
//
// model_aliases 里 channel_id 为 NULL 的行是平台级别名，对全部渠道生效；
// 有 channel_id 的是渠道级覆盖。**渠道级优先**——商家配一条渠道级别的
// 别名，意思就是「这家上游的叫法与别家不同」，平台级那条只是兜底。
//
// channelID 传 0 表示「不针对具体渠道」，此时只会命中平台级的行
// （id 从 1 开始，0 不会等于任何真实的 channel_id）。
//
// # 同一渠道可能有多条别名指向同一个规范名
//
// 表上的唯一键是 (channel_id, upstream_model)，它拦不住
// 「两个上游名都映射到同一个规范名」。真出现这种数据时这里按上游名排序取
// 第一条，结果是确定的，但**不代表哪条更对**——那要靠数据本身的约定。
func (s *Source) UpstreamModel(ctx context.Context, channelID int64, canonicalModel string) (string, error) {
	if canonicalModel == "" {
		return "", errors.New("规范模型名为空")
	}

	var upstream string
	err := s.db.QueryRowContext(ctx, `
		SELECT upstream_model
		  FROM model_aliases
		 WHERE canonical_name = $2
		   AND (channel_id = $1 OR channel_id IS NULL)
		 ORDER BY (channel_id IS NULL), upstream_model
		 LIMIT 1`, channelID, canonicalModel).Scan(&upstream)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w：渠道 %d 的 %q", ErrAliasNotFound, channelID, canonicalModel)
	}
	if err != nil {
		return "", fmt.Errorf("查询模型别名失败: %w", err)
	}
	return upstream, nil
}
