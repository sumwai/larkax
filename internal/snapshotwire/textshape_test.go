package snapshotwire

import (
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestConsentValidateRejectsControlCharRequestID 钉住授权申请里 request_id
// 的形状判据。
//
// request_id 会进 grant 的回执与 `consent list` 的 RANGE 那一栏：换行会把
// 一栏拆成两行，ESC 这类转义序列会原样进终端。判在写入路径上（Grant 的
// validate），命令行那一层只是提前报同一个错。
func TestConsentValidateRejectsControlCharRequestID(t *testing.T) {
	now := time.Now()
	for _, bad := range []string{"req\n1", "req\t1", "req\x1b[31m1"} {
		req := ConsentRequest{
			UserID:    1,
			Scope:     ConsentScopeSingleRequest,
			RequestID: bad,
			ExpiresAt: now.Add(time.Hour),
		}
		if err := req.validate(now, 24*time.Hour); !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("validate(%q) = %v，期望 ErrControlChar", bad, err)
		}
	}

	req := ConsentRequest{
		UserID:    1,
		Scope:     ConsentScopeSingleRequest,
		RequestID: "req-abc-123",
		ExpiresAt: now.Add(time.Hour),
	}
	if err := req.validate(now, 24*time.Hour); err != nil {
		t.Errorf("正常 request_id 被拒: %v", err)
	}
}
