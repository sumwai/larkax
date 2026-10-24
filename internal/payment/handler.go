package payment

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sumwai/larkax/internal/observe"
)

// HeaderVersion 是响应头里回带发布版本的字段名，与推理接口那一条同名。
const HeaderVersion = "X-Larkax-Version"

// maxCallbackBodyBytes 是回调请求体的上限。
//
// 通道通知是几十个字段的小 JSON，64 KiB 已经远超合理范围；在解析之前挡住
// 超大请求体，是因为它会被整段读进内存，而这条路径对公网开放。
const maxCallbackBodyBytes = 64 << 10

// Handler 是某一条通道的支付回调入口。
//
// 一条通道一个 Handler、挂在 `CallbackPath(通道名)` 上：路径本身就说明了
// 这份通知属于谁，于是解析时不必再从一个字段里猜通道。
type Handler struct {
	service     *Service
	channelName string
	version     string
}

// NewHandler 构造一条通道的回调入口。
//
// version 写进每个响应的 X-Larkax-Version 头。
func NewHandler(service *Service, channelName, version string) *Handler {
	return &Handler{
		service:     service,
		channelName: channelName,
		version:     version,
	}
}

// ServeHTTP 处理一次通道通知。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.version != "" {
		w.Header().Set(HeaderVersion, h.version)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeCallbackError(w, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		writeCallbackError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	if h.service == nil {
		writeCallbackError(w, http.StatusNotFound, ErrChannelDisabled.Error())
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBodyBytes))
	if err != nil {
		writeCallbackError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}

	notification, err := h.notification(r, body)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	result, err := h.service.Notify(r.Context(), h.channelName, notification)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeCallbackJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"order_no": result.Order.OrderNo,
		"applied":  result.Applied,
		"ignored":  result.Ignored,
		"status":   string(result.Order.Status),
	})
}

// notification 让通道自己校验签名并解析请求体。
//
// 签名校验放在通道里而不是这里：每一家通道的签名方案都不同，本包不该认识
// 其中任何一家。
func (h *Handler) notification(r *http.Request, body []byte) (Notification, error) {
	channel, err := h.service.channel(h.channelName)
	if err != nil {
		return Notification{}, err
	}
	return channel.ParseNotification(r.Context(), NotificationRequest{
		Header: r.Header,
		Body:   body,
	})
}

// writeServiceError 把本包的几类错误映射成状态码。
func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrBadSignature):
		// 签名不对时**不回显**解析出来的字段：那会让伪造通知的人拿到一份
		// 「哪一步差一点就对了」的提示。
		writeCallbackError(w, http.StatusUnauthorized, "signature mismatch")
	case errors.Is(err, ErrInvalidNotification):
		writeCallbackError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrOrderNotFound):
		writeCallbackError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrChannelDisabled):
		writeCallbackError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrOrderConflict):
		writeCallbackError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrGatewayTxnConflict):
		// 也是 409：两份通知本身都合法，只是这一笔流水号已经记在另一张单上，
		// 重发不会让它变得可处理。与上一条分开，是因为下一步不同——那一条看
		// 下单参数，这一条要人工核这两张单。记一条日志：它不该出现，通道正常
		// 不会把同一个流水号发给两张单。
		observe.FromContext(r.Context()).Error("支付回调的通道流水号冲突", "err", err)
		writeCallbackError(w, http.StatusConflict, err.Error())
	default:
		observe.FromContext(r.Context()).Error("处理支付通知失败", "err", err)
		writeCallbackError(w, http.StatusInternalServerError, "failed to process payment notification")
	}
}

// writeCallbackError 写一个错误响应。
func writeCallbackError(w http.ResponseWriter, status int, message string) {
	writeCallbackJSON(w, status, map[string]string{"error": message})
}

// writeCallbackJSON 写一个 JSON 响应。
//
// 编码失败只记日志：响应头已经发出去了，此时再写一个错误体只会拼出半截 JSON。
func writeCallbackJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().Error("写支付回调响应失败", "err", err)
	}
}

// isJSON 判断 Content-Type 是不是 JSON（允许带 charset 参数）。
func isJSON(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
}
