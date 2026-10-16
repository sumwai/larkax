package pricing

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/pipeline"
)

// 没有模型名的查询直接报错，不查库。
//
// 这一条不需要数据库：它挡在查询之前。空模型名匹配不到任何规则，
// 而「匹配不到」与「请求本身不完整」是两回事——前者是配置缺失，
// 后者是调用方的 bug，混成一个 ErrNoRule 会让排查方向错。
func TestMatchRejectsEmptyModel(t *testing.T) {
	s := New(nil)

	_, err := s.Match(context.Background(), pipeline.RuleQuery{CanonicalModel: ""})
	if err == nil {
		t.Fatal("空模型名应当报错")
	}
	if errors.Is(err, pipeline.ErrNoRule) {
		t.Error("空模型名不该报成「没有匹配的规则」——那是请求不完整")
	}
}
