package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestValidateDisplayNameRejectsControlChars 钉住展示名的形状判据。
//
// 判在 trim 之后的值上：两头的空白会被写入路径去掉，所以 `" a\nb "` 里那个
// 换行仍然在落库的串里；而只有空白的值表示「清掉这一列」，不是坏值。
func TestValidateDisplayNameRejectsControlChars(t *testing.T) {
	for _, value := range []string{"a\nb", "a\tb", "a\x1b[31mb", " a\nb "} {
		if err := ValidateDisplayName(value); !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("ValidateDisplayName(%q) = %v，期望 ErrControlChar", value, err)
		}
	}

	for _, value := range []string{"", "   ", "张三", "Alice Smith"} {
		if err := ValidateDisplayName(value); err != nil {
			t.Errorf("ValidateDisplayName(%q) = %v，期望通过", value, err)
		}
	}
}

// TestUserUpdateSetsRejectsControlCharDisplayName 钉住写入路径自己也判一遍。
//
// 命令行那一层已经挡过一次；这里证明绕过命令行的调用方（将来的管理面、
// 脚本）从 UpdateUser 这一头同样写不进一个带换行的展示名。
func TestUserUpdateSetsRejectsControlCharDisplayName(t *testing.T) {
	name := "probe\nop\x1b[31mX"
	_, _, err := userUpdateSets(UpdateUserParams{DisplayName: &name})
	if !errors.Is(err, textshape.ErrControlChar) {
		t.Fatalf("userUpdateSets = %v，期望 ErrControlChar", err)
	}
}

// TestValidateKeyNameRejectsControlChars 钉住 API Key 备注的形状判据。
//
// 这一列不去空白（写入路径也不去），所以判原值：空串照样通过——那是
// 「没有备注」，与创建时不给 --name 是同一个意思。
func TestValidateKeyNameRejectsControlChars(t *testing.T) {
	for _, value := range []string{"a\nb", "a\tb", "a\x1b[31mb"} {
		if err := ValidateKeyName(value); !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("ValidateKeyName(%q) = %v，期望 ErrControlChar", value, err)
		}
	}

	for _, value := range []string{"", "   ", "生产环境", "ci runner"} {
		if err := ValidateKeyName(value); err != nil {
			t.Errorf("ValidateKeyName(%q) = %v，期望通过", value, err)
		}
	}
}

// TestCreateKeyRejectsControlCharNameBeforeDB 钉住建 key 的写入路径也判一次。
//
// 传一个 nil *sql.DB：判据排在第一条语句之前，所以这里不需要真库——如果哪天
// 有人把它挪到插入之后，这个用例会直接 panic，而不是静默变成一条「只有命令行
// 会拦」的判据。
func TestCreateKeyRejectsControlCharNameBeforeDB(t *testing.T) {
	_, err := New(nil).Create(context.Background(), CreateParams{UserID: 1, Name: "a\nb"})
	if !errors.Is(err, textshape.ErrControlChar) {
		t.Fatalf("Create = %v，期望 ErrControlChar", err)
	}
}

// TestUpdateKeyRejectsControlCharNameBeforeDB 与上一条同一个形状，走改 key 那条路。
func TestUpdateKeyRejectsControlCharNameBeforeDB(t *testing.T) {
	_, err := New(nil).UpdateKey(context.Background(), 1, KeyParams{SetName: true, Name: "a\nb"})
	if !errors.Is(err, textshape.ErrControlChar) {
		t.Fatalf("UpdateKey = %v，期望 ErrControlChar", err)
	}
}
