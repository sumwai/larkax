package plans

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sumwai/larkax/internal/dberr"
)

// 本文件是**扣费顺序**：一次请求按什么次序挑从哪儿扣。
//
// 三档就够：先额度包后余额、先余额后额度包、只走余额。平台给一个默认值
// （settings 表），用户可以在自己那一行上覆盖（users.charge_order，NULL 表示
// 跟随平台）。这一层只回答「这个用户的有效顺序是什么」，**不做选源**——按这个
// 顺序挑哪一个包、够不够覆盖预留，是请求路径的事（见 #860 的下一步）。

// ChargeOrder 是一次请求挑选扣费来源的次序。
//
// 取值与 users.charge_order 的 CHECK 约束、settings 的 plans.charge_order
// 一一对应。
type ChargeOrder string

const (
	// ChargeOrderPlanFirst 先额度包后余额：有未过期、有余额的订阅就从它扣，
	// 都不够时回落到账户余额。这是平台默认值。
	ChargeOrderPlanFirst ChargeOrder = "plan_first"
	// ChargeOrderBalanceFirst 先余额后额度包：账户余额够就整单从余额扣，
	// 不够时才找额度包。
	ChargeOrderBalanceFirst ChargeOrder = "balance_first"
	// ChargeOrderBalanceOnly 只走余额：额度包存在也不参与这次请求的扣费。
	ChargeOrderBalanceOnly ChargeOrder = "balance_only"
)

// Valid 报告这个取值是不是三档之一。
func (o ChargeOrder) Valid() bool {
	switch o {
	case ChargeOrderPlanFirst, ChargeOrderBalanceFirst, ChargeOrderBalanceOnly:
		return true
	}
	return false
}

// String 让 ChargeOrder 在日志与报错里直接读得出来。
func (o ChargeOrder) String() string { return string(o) }

// ChargeOrderSource 说明这个有效顺序是从哪儿读出来的。
//
// 调用方要能说清「你现在看到的是自己设的，还是跟着平台默认」：两者在输出里
// 长得一样，而改后者要去找 `settings`，改前者只需一条命令。
type ChargeOrderSource string

const (
	// ChargeOrderSourceUser 表示用户自己那一行上有值。
	ChargeOrderSourceUser ChargeOrderSource = "user"
	// ChargeOrderSourcePlatform 表示用户那一行是 NULL（或认不出、已回落到平台默认）。
	ChargeOrderSourcePlatform ChargeOrderSource = "platform"
)

// ErrChargeOrder 表示一次写入给了一个认不出的扣费顺序。
var ErrChargeOrder = errors.New("扣费顺序的取值认不出")

// PlatformChargeOrderKey 是平台默认扣费顺序在 settings 表里的键。
//
// 与迁移 00051 里的那一条 INSERT、settings.Registry 里的声明是同一个键名。
const PlatformChargeOrderKey = "plans.charge_order"

// DefaultChargeOrder 是兜底值：settings 里那一行缺失、读不出来、或者取值认不出
// 时用它，并记一条日志。
//
// 它不是「平台的默认值」——那个值在迁移 00051 的 INSERT 里，改默认值就改那里。
// 这一份只在库里的默认值坏掉时避免请求路径拿到空字符串，所以两者**当前必须
// 一致**（plan_first）；迁移里改了默认值，这里跟着改。
const DefaultChargeOrder = ChargeOrderPlanFirst

// EffectiveChargeOrder 给出一个用户实际使用的扣费顺序，以及它的出处。
//
// 优先级：users.charge_order 上的值 > settings.plans.charge_order > DefaultChargeOrder。
// **不 panic**：库里的未知取值（手工修数、更早版本写进去的、未来版本回滚留下的）
// 按平台默认走，并记一条 Warn——请求路径不该因为一行配置认不出就整单失败。
//
// 读不动库时返回错误，兜底与否由调用方决定：这一层分不清「配置写坏了」与「库现在
// 读不了」。两个调用方的处置因此不同——`plan order show` 把错误如实打给操作人；
// 请求路径走 RequestChargeOrder，按两级兜底继续（见下），不因为一行配置读不出来
// 而拒绝服务。
func (s *Service) EffectiveChargeOrder(ctx context.Context, userID int64) (ChargeOrder, ChargeOrderSource, error) {
	if userID <= 0 {
		return "", "", fmt.Errorf("用户 id 非法: %d", userID)
	}

	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT charge_order FROM users WHERE id = $1`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("用户 %d 不存在", userID)
	}
	if err != nil {
		return "", "", fmt.Errorf("读用户 %d 的扣费顺序失败: %w", userID, err)
	}

	order := ChargeOrder(raw.String)
	if raw.Valid && order.Valid() {
		return order, ChargeOrderSourceUser, nil
	}
	platform, err := s.platformChargeOrder(ctx)
	if err != nil {
		return "", "", err
	}
	if raw.Valid {
		s.logger().Warn("用户上的扣费顺序取值认不出，按平台默认走",
			"user_id", userID, "value", raw.String, "fallback", platform)
	}
	return platform, ChargeOrderSourcePlatform, nil
}

// platformChargeOrder 读平台默认的扣费顺序。
//
// 缺行、不是 JSON 字符串、取值认不出三种情况都回落到 DefaultChargeOrder 并记
// 日志。数据库本身读不动（连接断了、权限没了）时把错误交给调用方：那不是配置
// 写错，而是这一层现在读不了库。
func (s *Service) platformChargeOrder(ctx context.Context) (ChargeOrder, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = $1`, PlatformChargeOrderKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		s.logger().Warn("settings 里没有平台默认的扣费顺序，按兜底值走",
			"key", PlatformChargeOrderKey, "fallback", DefaultChargeOrder)
		return DefaultChargeOrder, nil
	}
	if err != nil {
		return "", fmt.Errorf("读平台默认的扣费顺序失败: %w", err)
	}

	var plain string
	if err := json.Unmarshal(raw, &plain); err != nil {
		s.logger().Warn("平台默认的扣费顺序不是 JSON 字符串，按兜底值走",
			"key", PlatformChargeOrderKey, "value", string(raw), "fallback", DefaultChargeOrder)
		return DefaultChargeOrder, nil
	}
	order := ChargeOrder(plain)
	if !order.Valid() {
		s.logger().Warn("平台默认的扣费顺序取值认不出，按兜底值走",
			"key", PlatformChargeOrderKey, "value", plain, "fallback", DefaultChargeOrder)
		return DefaultChargeOrder, nil
	}
	return order, nil
}

// SetUserChargeOrder 给一个用户设扣费顺序，覆盖平台默认。
//
// 写入侧校验：认不出的取值当场报错，而不是等读侧回落到默认——后者会让一次
// 写错的配置安静地不生效，而写的人以为它生效了。
//
// 它天然幂等：重复设成同一个值返回同样的结果。
func (s *Service) SetUserChargeOrder(ctx context.Context, userID int64, order ChargeOrder) error {
	if userID <= 0 {
		return fmt.Errorf("用户 id 非法: %d", userID)
	}
	if !order.Valid() {
		return fmt.Errorf("%w: %q（只有 %s / %s / %s）",
			ErrChargeOrder, order, ChargeOrderPlanFirst, ChargeOrderBalanceFirst, ChargeOrderBalanceOnly)
	}

	var id int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE users SET charge_order = $2, updated_at = now() WHERE id = $1 RETURNING id`,
		userID, string(order)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("用户 %d 不存在", userID)
	}
	if err != nil {
		return dberr.ConstraintFailure(fmt.Errorf("设用户 %d 的扣费顺序失败: %w", userID, err))
	}
	return nil
}

// ClearUserChargeOrder 把用户上的覆盖清掉，回到「跟随平台默认」。
//
// 与 SetUserChargeOrder 分开成一个方法，而不是让调用方传一个空串：空串是一个
// 认不出的取值（会被写入侧拒掉），「清掉覆盖」是另一件事。
func (s *Service) ClearUserChargeOrder(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return fmt.Errorf("用户 id 非法: %d", userID)
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE users SET charge_order = NULL, updated_at = now() WHERE id = $1 RETURNING id`,
		userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("用户 %d 不存在", userID)
	}
	if err != nil {
		return fmt.Errorf("清用户 %d 的扣费顺序失败: %w", userID, err)
	}
	return nil
}

// RequestChargeOrder 是请求路径用的那一个：任何读失败都不拒绝服务。
//
// 与 EffectiveChargeOrder 的分工：那个把错误交给调用方（`plan order show`
// 要如实报告读不动库），这个按 #918 定的两级兜底走完——
//
//  1. 用户级读失败 → 跟随平台默认，记一条日志；
//  2. 平台默认也读失败 → DefaultChargeOrder，再记一条日志。
//
// 两次都记，是因为它们指向不同的处置：第一条多半是 users 那一行的读取出了
// 问题，第二条是 settings 或连接本身出问题。只记最终值的话，两者在日志里
// 长得一样。
//
// 它满足 internal/ledger/store 声明的 ChargeOrderSource，装配层把它接到
// 预留那条路上。
func (s *Service) RequestChargeOrder(ctx context.Context, userID int64) string {
	order, _, err := s.EffectiveChargeOrder(ctx, userID)
	if err == nil {
		return order.String()
	}
	s.logger().Warn("读取用户扣费顺序失败，请求路径跟随平台默认",
		"user_id", userID, "err", err)

	platform, platformErr := s.platformChargeOrder(ctx)
	if platformErr == nil {
		return platform.String()
	}
	s.logger().Warn("读取平台默认扣费顺序也失败，请求路径按兜底值走",
		"err", platformErr, "fallback", DefaultChargeOrder)
	return DefaultChargeOrder.String()
}

// logger 返回这一层用的日志句柄。
func (s *Service) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}
