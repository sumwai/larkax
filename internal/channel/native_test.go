package channel

import (
	"bytes"
	"testing"
)

// 直通用的三个字段都是普通字段，没有构造函数。
//
// 这条用例钉三件事：带值的 Request.NativeBody 能原样取回；Kind == "native"
// 的事件带得出 NativeFormat 与 Frame；三个字段的零值分别是 nil、""、nil。
// 零值那一条尤其要钉——哪天把 NativeBody 换成必须经构造函数生成的类型
// （例如带校验的封装），nil 就不再表示「没有」，跨协议那条路会凭空多出
// 一个「零值还是没设」的分支。
func TestNativePassthroughFields(t *testing.T) {
	t.Parallel()

	t.Run("原生请求体原样可取", func(t *testing.T) {
		t.Parallel()
		body := []byte(`{"model":"example-model","messages":[{"role":"user","content":"你好"}]}`)
		req := Request{Model: "example-model", NativeBody: body}
		if !bytes.Equal(req.NativeBody, body) {
			t.Fatalf("NativeBody = %q，期望 %q", req.NativeBody, body)
		}
	})

	t.Run("原生流式帧原样可取", func(t *testing.T) {
		t.Parallel()
		// 载荷里没有 `data:` 前缀，也没有行尾换行：字段就是 `data:` 行
		// trim 之后的字节（见 StreamEvent.Frame）。
		frame := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`)
		ev := StreamEvent{Kind: "native", NativeFormat: ProtocolAnthropic, Frame: frame}
		if ev.Kind != "native" {
			t.Fatalf("Kind = %q，期望 native", ev.Kind)
		}
		if ev.NativeFormat != ProtocolAnthropic {
			t.Errorf("NativeFormat = %q，期望 %q", ev.NativeFormat, ProtocolAnthropic)
		}
		if !bytes.Equal(ev.Frame, frame) {
			t.Errorf("Frame = %q，期望 %q", ev.Frame, frame)
		}
	})

	t.Run("零值即未设", func(t *testing.T) {
		t.Parallel()
		var req Request
		if req.NativeBody != nil {
			t.Errorf("Request.NativeBody 零值 = %q，期望 nil", req.NativeBody)
		}
		var ev StreamEvent
		if ev.NativeFormat != "" {
			t.Errorf("StreamEvent.NativeFormat 零值 = %q，期望空串", ev.NativeFormat)
		}
		if ev.Frame != nil {
			t.Errorf("StreamEvent.Frame 零值 = %q，期望 nil", ev.Frame)
		}
	})
}
