// Package migrate 把 SQL 迁移文件嵌入二进制并对外提供 goose 的调用入口。
//
// 迁移文件随二进制一起发布，部署时不需要额外投放 SQL——镜像里有什么版本，
// 能跑的迁移就是什么版本，不会出现「二进制是新的、SQL 是旧的」这种错配。
package migrate

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed sql/*.sql
var embedded embed.FS

// FS 返回只含迁移文件的文件系统。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "sql")
	if err != nil {
		// embed 的内容在编译期就固定了，走到这里说明目录名写错了，
		// 那是构建期错误，不是运行期状况。
		panic(fmt.Sprintf("migrate: 内嵌迁移目录不可用: %v", err))
	}
	return sub
}

// SetDialect 必须在任何 goose 调用之前执行一次。
func SetDialect(dialect string) error {
	return goose.SetDialect(dialect)
}
