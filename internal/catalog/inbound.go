package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/dberr"
)

// InboundAlias 是一条入站别名：调用方请求体里可以写 Alias，API 入口把它解析成
// CanonicalName，之后全链路（选路、计价、记账、插件）都按规范名走。
//
// # 与 Alias 是两个方向
//
// Alias（读取面）回答「这个规范名发给某个渠道的上游时叫什么」，方向是出站；
// 这里回答「调用方写这个名字时指的是哪个规范名」，方向是入站。两者落在两张表
// 上（model_aliases / inbound_model_aliases），因为唯一键的范围不同：出站映射
// 按渠道分范围，入站别名全局只有一条。
type InboundAlias struct {
	Alias         string
	CanonicalName string
	CreatedAt     time.Time
}

// 入站别名写入面上的分类错误。
//
// 三个都归「约束冲突」（命令行退出码 4）：下一步都是先去看库里现在是什么，
// 改一下命令重发不解决问题。
var (
	// ErrInboundAliasExists 表示这个别名已经有一条映射了。
	//
	// 不静默覆盖：把一个已经在用的短名改指另一个模型，是一次所有调用方都
	// 看不见的换模型。
	ErrInboundAliasExists = errors.New("入站别名已存在")

	// ErrInboundAliasNotFound 表示要删的入站别名不在库里。
	ErrInboundAliasNotFound = errors.New("入站别名不存在")

	// ErrAliasIsCanonical 表示这个别名与一个已存在的规范模型同名。
	//
	// 这种别名不允许建：解析时规范名优先、别名会被遮蔽，「这个名字到底指
	// 它自己还是指别的」于是没有唯一答案。错在写入侧出声，而不是让每次请求
	// 都去猜。
	ErrAliasIsCanonical = errors.New("别名与规范模型同名")
)

// ResolveInbound 把调用方写的模型名解析成规范名。
//
// # 语义
//
// 命中一条别名 → 那条的规范名；没命中 → 原样返回（返回的名字与入参相同）。
// 同名既是规范模型、又有一条入站别名时，**规范名优先、别名被遮蔽**，同样
// 原样返回。这条判据写在一条 SQL 里（LEFT JOIN + `m.canonical_name IS NULL`），
// 不是「先查目录再查别名」两次往返；两次查询还会在两次之间留一个窗口，
// 「这条别名此刻算不算被遮蔽」会在两个答案之间漂移。
//
// # 一次请求一次查询，没有缓存
//
// 与 Catalog 的其他读取路径同一个取舍：主键查询的成本可以接受，而缓存要回答
// 「别名表变了之后谁来失效」。将来若要缓存，失效通道是独立设计——不要在这里
// 顺手加一个 map：那会多出第二份没人负责失效的状态。
//
// # SQL 失败要往上抛
//
// 不能吞成 `name, nil`：那会把一个数据库故障变成「这个别名不存在」，于是
// 请求拿别名去选路，症状是「查无此模型」——排查方向整个指向另一边。
func (c *Catalog) ResolveInbound(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("模型名为空")
	}

	var canonical string
	err := c.db.QueryRowContext(ctx, `
		SELECT a.canonical_name
		  FROM inbound_model_aliases a
		  LEFT JOIN model_catalog m ON m.canonical_name = a.alias
		 WHERE a.alias = $1
		   AND m.canonical_name IS NULL`, name).Scan(&canonical)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 没有命中，或命中但被同名规范模型遮蔽：原样返回。
		return name, nil
	case err != nil:
		return "", fmt.Errorf("查询入站别名失败: %w", err)
	}
	return canonical, nil
}

// HasShadowedInboundAlias 回答「这个名字是不是同时有一条被遮蔽的入站别名」。
//
// 遮蔽 = 同名的那一行在 inbound_model_aliases 里，同时 name 又是一个存在的
// 规范模型。它是 model trace 顺手能看见的一类数据：解析时规范名赢、别名不
// 生效，写入侧不拦（判定要跨两张表，而目录行是可后建的），所以只能由诊断
// 把它说出来。
func (c *Catalog) HasShadowedInboundAlias(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, errors.New("模型名为空")
	}

	var one int
	err := c.db.QueryRowContext(ctx, `
		SELECT 1
		  FROM inbound_model_aliases a
		  JOIN model_catalog m ON m.canonical_name = a.alias
		 WHERE a.alias = $1`, name).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("查询被遮蔽的入站别名失败: %w", err)
	}
	return true, nil
}

// InboundAliases 列出入站别名，按 alias 排序。
//
// 空表返回空切片而不是 nil：一条都没配是正常状态（大多数部署都没有别名），
// 调用方拿到的清单要能直接遍历、直接序列化成 `[]`。
func (c *Catalog) InboundAliases(ctx context.Context) ([]InboundAlias, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT alias, canonical_name, created_at
		  FROM inbound_model_aliases
		 ORDER BY alias`)
	if err != nil {
		return nil, fmt.Errorf("查询入站别名失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []InboundAlias{}
	for rows.Next() {
		var a InboundAlias
		if scanErr := rows.Scan(&a.Alias, &a.CanonicalName, &a.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("读取入站别名失败: %w", scanErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历入站别名失败: %w", err)
	}
	return out, nil
}

// CreateInboundAlias 新建一条入站别名。
//
// # 三条拒绝
//
//   - 两个名字都不能为空（命令行退出 1；与 model create 同一处置——裸
//     errors.New，不在 usageErr / conflictErr 里）；
//   - 别名不能是一个已存在的规范名（退出 4，点名冲突的名字）——否则解析时
//     规范名赢、别名被遮蔽，「这个名字指谁」没有唯一答案；
//   - 规范名必须存在（退出 4，点名名字）——别名指向一个不存在的模型时，
//     每个用它的请求都会在选路处报「查无此模型」，而那条消息不会提到别名。
//
// 最后一条是**写入时**的校验，不在读路径上：目录行可以事后被删，那种断链
// 是 model trace 要报出来的状态，不是它不该存在的状态。
//
// # 已存在的别名不静默覆盖
//
// 用主键冲突回答，不先查再插：先查再插会在两次之间留一个窗口，而成败仍由
// 主键决定——两处判据。撞上时点名别名，让下一步是「先 remove 再 add」。
func (a *Admin) CreateInboundAlias(ctx context.Context, alias, canonical string) error {
	aliasName, err := normalizeCanonicalName(alias)
	if err != nil {
		return fmt.Errorf("别名无效: %w", err)
	}
	canonicalName, err := normalizeCanonicalName(canonical)
	if err != nil {
		return fmt.Errorf("规范名无效: %w", err)
	}

	isCanonical, err := a.modelExists(ctx, aliasName)
	if err != nil {
		return err
	}
	if isCanonical {
		return fmt.Errorf("%w：%q", ErrAliasIsCanonical, aliasName)
	}

	canonicalExists, err := a.modelExists(ctx, canonicalName)
	if err != nil {
		return err
	}
	if !canonicalExists {
		return fmt.Errorf("%w：%q", ErrModelNotFound, canonicalName)
	}

	_, err = a.db.ExecContext(ctx, `
		INSERT INTO inbound_model_aliases (alias, canonical_name)
		VALUES ($1, $2)`, aliasName, canonicalName)
	if err != nil {
		if dberr.IsConstraintViolation(err) {
			return fmt.Errorf("%w：%q", ErrInboundAliasExists, aliasName)
		}
		return fmt.Errorf("写入入站别名失败: %w", err)
	}
	return nil
}

// DeleteInboundAlias 删除一条入站别名。
//
// 按 alias 定位：别名是这张表的主键，全局只有一条，不需要先 list 一遍拿 ID。
// 不存在时点名那个名字，不静默成功——「删了」与「本来就没有」在脚本里是
// 两件事。
func (a *Admin) DeleteInboundAlias(ctx context.Context, alias string) error {
	name, err := normalizeCanonicalName(alias)
	if err != nil {
		return fmt.Errorf("别名无效: %w", err)
	}

	res, err := a.db.ExecContext(ctx,
		`DELETE FROM inbound_model_aliases WHERE alias = $1`, name)
	if err != nil {
		return fmt.Errorf("删除入站别名失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取删除结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：%q", ErrInboundAliasNotFound, name)
	}
	return nil
}

// modelExists 判断目录里有没有这个规范名。
func (a *Admin) modelExists(ctx context.Context, canonicalName string) (bool, error) {
	var one int
	err := a.db.QueryRowContext(ctx,
		`SELECT 1 FROM model_catalog WHERE canonical_name = $1`, canonicalName).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("查询规范模型失败: %w", err)
	}
	return true, nil
}
