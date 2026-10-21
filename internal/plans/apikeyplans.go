package plans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/sumwai/larkax/internal/dberr"
)

// 本文件是「一把 key 能用用户的哪些额度包订阅」的读侧与写入侧。
//
// # 空集合 = 全部
//
// 没有行的 key 用得了用户名下**全部**订阅。这是 issue #860 定下的语义，读侧
// 照它解释（APIKeyPlans 在没有行时返回用户全部订阅）。不引入哨兵行，也不加
// 「这把 key 受不受限」的开关列：两种表示会让读的人猜哪一种是「全部」。
//
// # 这一层不做请求路径的选源
//
// 它回答的是「允许集是什么」，不判断订阅是否过期、面值是否还有、模型范围是否
// 覆盖本次请求——那些是请求路径上挑候选时的事（见 #860 的下一步）。按状态过滤
// 把「已作废」从清单里藏起来，而运维要能看见「这把 key 挂着一份已经作废的订阅」。

// ErrNoAPIKey 表示按 id 找不到那把 key。
var ErrNoAPIKey = errors.New("没有这个 API Key")

// ErrUserPlanNotOwned 表示名单里有一条订阅不属于这把 key 的用户。
//
// 它必须在写入侧就报出来：留到调用时才拒，等于「这把 key 建好了，但用它发的
// 请求全失败」，而那时运维已经不再看着这条命令的输出。
var ErrUserPlanNotOwned = errors.New("订阅不属于这把 key 的用户")

// APIKeyPlans 读一把 key 能用的订阅。
//
// 没有关联行时返回用户名下的全部订阅（空集合 = 全部）；有行时只返回名单里的
// 那些，按订阅 id 升序。两种结果都是「这个 key 允许的集合」，调用方不需要再
// 判断「到底是没配还是配了空」。
func (s *Service) APIKeyPlans(ctx context.Context, apiKeyID int64) ([]UserPlan, error) {
	if apiKeyID <= 0 {
		return nil, fmt.Errorf("API Key id 非法: %d", apiKeyID)
	}
	userID, err := s.apiKeyOwner(ctx, apiKeyID)
	if err != nil {
		return nil, err
	}

	ids, err := s.apiKeyPlanIDs(ctx, s.db, apiKeyID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return s.UserPlans(ctx, userID)
	}

	// 只按 id 取不够：名单行理论上可能指向别的用户的订阅（手工修数、外键只保证
	// 订阅存在）。user_id 这一条谓词让读侧在这种情况下返回的是「这个用户实际
	// 允许的集合」，而不是把它漏出去。
	rows, err := s.db.QueryContext(ctx,
		userPlanQuery+` WHERE up.user_id = $1 AND up.id = ANY($2) ORDER BY up.id`,
		userID, ids)
	if err != nil {
		return nil, fmt.Errorf("读 API Key %d 的额度包订阅失败: %w", apiKeyID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]UserPlan, 0, len(ids))
	for rows.Next() {
		up, err := scanUserPlan(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读 API Key %d 的额度包订阅失败: %w", apiKeyID, err)
		}
		out = append(out, up)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 API Key %d 的额度包订阅失败: %w", apiKeyID, err)
	}
	return out, nil
}

// SetAPIKeyPlans 用一份名单替换一把 key 的订阅限制。
//
// 空名单表示「全部」：删掉这把 key 的关联行，读侧按「没有行 = 全部」解释。
// 它不做增量：一次调用给出的就是这把 key 之后的完整名单——增量接口在重放到达
// 时不知道该补哪几次，而名单本身只有几个元素，整体替换更不容易写错。
//
// 写入前校验每一条订阅都属于这把 key 的用户（ErrUserPlanNotOwned）。重复的 id
// 去重；顺序不落库，读侧按 id 升序返回。
//
// 返回替换之后的允许集（与 APIKeyPlans 同一份解释）。
func (s *Service) SetAPIKeyPlans(ctx context.Context, apiKeyID int64, userPlanIDs []int64) ([]UserPlan, error) {
	if apiKeyID <= 0 {
		return nil, fmt.Errorf("API Key id 非法: %d", apiKeyID)
	}
	userID, err := s.apiKeyOwner(ctx, apiKeyID)
	if err != nil {
		return nil, err
	}

	ids, err := normalizeUserPlanIDs(userPlanIDs)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(ids) > 0 {
		// 一条语句问全：哪几条 id 不存在、哪几条属于别人。分开问的话，
		// 「不存在」与「不是你的」会对同一条 id 给出两个不同的说法，而它们
		// 对操作者的意思一样——这条 id 不能放进名单里。
		rows, err := tx.QueryContext(ctx,
			`SELECT id, user_id FROM user_plans WHERE id = ANY($1)`, ids)
		if err != nil {
			return nil, fmt.Errorf("核对额度包订阅失败: %w", err)
		}
		owned := make(map[int64]bool, len(ids))
		for rows.Next() {
			var id, owner int64
			if err := rows.Scan(&id, &owner); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("核对额度包订阅失败: %w", err)
			}
			if owner == userID {
				owned[id] = true
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("核对额度包订阅失败: %w", err)
		}
		_ = rows.Close()

		var missing []int64
		for _, id := range ids {
			if !owned[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
			return nil, fmt.Errorf("%w：%v（API Key %d 属于用户 %d）",
				ErrUserPlanNotOwned, missing, apiKeyID, userID)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM api_key_plans WHERE api_key_id = $1`, apiKeyID); err != nil {
		return nil, dberr.ConstraintFailure(fmt.Errorf("清 API Key %d 的额度包名单失败: %w", apiKeyID, err))
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO api_key_plans (api_key_id, user_plan_id) VALUES ($1, $2)`,
			apiKeyID, id); err != nil {
			return nil, dberr.ConstraintFailure(fmt.Errorf("写 API Key %d 的额度包名单失败: %w", apiKeyID, err))
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交 API Key %d 的额度包名单失败: %w", apiKeyID, err)
	}
	return s.APIKeyPlans(ctx, apiKeyID)
}

// APIKeyPlanIDs 读一把 key 的名单原文（按 id 升序）。
//
// 空切片既表示「没有行」，也读出「全部」这个语义——调用方拿到空切片时按
// 「没有限制」解释。它与 APIKeyPlans 分开，是因为有些调用方（例如展示「这把
// key 是受限的还是全部的」）要的正是**有没有配过**，而不是展开后的集合。
func (s *Service) APIKeyPlanIDs(ctx context.Context, apiKeyID int64) ([]int64, error) {
	if apiKeyID <= 0 {
		return nil, fmt.Errorf("API Key id 非法: %d", apiKeyID)
	}
	if _, err := s.apiKeyOwner(ctx, apiKeyID); err != nil {
		return nil, err
	}
	return s.apiKeyPlanIDs(ctx, s.db, apiKeyID)
}

// apiKeyPlanIDs 在一个查询句柄上读名单行。
func (s *Service) apiKeyPlanIDs(ctx context.Context, q queryer, apiKeyID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT user_plan_id FROM api_key_plans WHERE api_key_id = $1 ORDER BY user_plan_id`,
		apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("读 API Key %d 的额度包名单失败: %w", apiKeyID, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读 API Key %d 的额度包名单失败: %w", apiKeyID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 API Key %d 的额度包名单失败: %w", apiKeyID, err)
	}
	return ids, nil
}

// apiKeyOwner 读一把 key 的归属用户。
func (s *Service) apiKeyOwner(ctx context.Context, apiKeyID int64) (int64, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM api_keys WHERE id = $1`, apiKeyID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: id = %d", ErrNoAPIKey, apiKeyID)
	}
	if err != nil {
		return 0, fmt.Errorf("读 API Key %d 的归属用户失败: %w", apiKeyID, err)
	}
	return userID, nil
}

// normalizeUserPlanIDs 去掉重复与非法 id，按升序返回。
func normalizeUserPlanIDs(ids []int64) ([]int64, error) {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("额度包订阅 id 非法: %d", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
