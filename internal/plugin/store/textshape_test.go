package store

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestKVSetRejectsControlCharKey 钉住插件 KV 键的形状判据。
//
// 键会被 `plugin kv get` 打成「键」那一行，所以写入时就拒。判据排在开事务
// 之前，所以这里用 nil *sql.DB 也能走到：如果哪天有人把它挪到 SQL 之后，
// 这个用例会直接 panic，而不是静默变成「只有命令行会拦」。
//
// 值不在这条判据里，理由见 Set 的注释：它按设计排在表外原样打。
func TestKVSetRejectsControlCharKey(t *testing.T) {
	for _, bad := range []string{"a\nb", "a\tb", "a\x1b[31mb"} {
		err := (&KV{}).Set(context.Background(), "acme", 1, bad, "value", 0)
		if !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("Set(key=%q) = %v，期望 ErrControlChar", bad, err)
		}
	}
}
