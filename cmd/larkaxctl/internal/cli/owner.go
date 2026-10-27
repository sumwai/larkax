package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/auth"
)

// ResolveOwnerID 把「用户 ID 或用户名」解析成用户 ID。
//
// 0 是一个真实的取值——「不挂用户」，库里存 NULL（平台自营）。所以
// `--user 0` 直接返回 0，不去查库：库里没有 id 为 0 的用户，查了只会得到
// 一句「找不到用户」，而那句话对「我就是想改回平台自营」这个意图是误导的。
//
// 其余取值交给 auth.ResolveUser，与 apikey create --user 走同一条路
// （纯数字按 ID、其余按用户名）：两处各写一份解析口径时，
// 「名字里带数字的用户」会在其中一处解析错，而那不报错。
func ResolveOwnerID(ctx context.Context, db *sql.DB, ref string) (int64, error) {
	if strings.TrimSpace(ref) == "0" {
		return 0, nil
	}
	user, err := auth.New(db).ResolveUser(ctx, ref)
	if err != nil {
		return 0, fmt.Errorf("解析用户失败: %w", err)
	}
	return user.ID, nil
}
