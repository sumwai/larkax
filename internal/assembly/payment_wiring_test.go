package assembly

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/api"
	ledgerstore "github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/payment"
	"github.com/sumwai/larkax/internal/payment/fakechannel"
	"github.com/sumwai/larkax/internal/settings"
)

// loadPaymentSettings 从库里重新读一份配置快照。
//
// 每次装配都重读，而不是在用例开头读一次再改内存里的值：装配期读的就是
// 库里那一刻的样子，这条用例要验的正是「改了这一行，入口挂不挂」。
func loadPaymentSettings(t *testing.T, ctx context.Context, db *sql.DB) *settings.Values {
	t.Helper()
	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		t.Fatalf("读 settings 失败: %v", err)
	}
	return values
}

// TestPaymentCallbackIsMountedOnlyWhenFakeChannelEnabled 验装配期的开关。
//
// 关着时回调路径不挂，落回推理入口的未知路径（501）；打开后那条路径换成
// 支付回调处理器。两种状态都要各验一次回调路径，否则「没挂上」与「挂上了
// 但没生效」在输出里分不开。
func TestPaymentCallbackIsMountedOnlyWhenFakeChannelEnabled(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 零值 Handler 对未知路径回 501（见 api.Handler.ServeHTTP），
	// 正好用来认出「这条路径没有被支付回调接住」。
	inference := &api.Handler{}
	ledgerStore := ledgerstore.New(db)

	build := func() http.Handler {
		t.Helper()
		handler, err := buildPublicHandler(Config{
			DB: db, Settings: loadPaymentSettings(t, ctx, db),
			Logger: logger, Version: "test-version",
		}, inference, ledgerStore, logger)
		if err != nil {
			t.Fatalf("装配公共 handler 失败: %v", err)
		}
		return handler
	}
	request := func(h http.Handler, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, method, path, nil))
		return rec
	}

	callback := payment.CallbackPath(fakechannel.Name)

	// 默认关：回调路径没挂，落回推理入口的未知路径。
	disabled := build()
	if rec := request(disabled, http.MethodPost, callback); rec.Code != http.StatusNotImplemented ||
		!strings.Contains(rec.Body.String(), "not implemented") {
		t.Fatalf("通道关着时回调路径应当落回推理入口（501），得到 %d: %s",
			rec.Code, rec.Body.String())
	}

	// 打开开关后重新装配：回调路径换成支付回调处理器（GET 得到 405），
	// 其余未知路径仍然交给推理入口。
	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = 'true' WHERE key = 'payment.fake_enabled'`); err != nil {
		t.Fatalf("打开假支付通道失败: %v", err)
	}
	enabled := build()
	rec := request(enabled, http.MethodGet, callback)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("通道打开后回调路径应当由支付回调处理器接住（405），得到 %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(payment.HeaderVersion); got != "test-version" {
		t.Errorf("%s = %q，期望 test-version", payment.HeaderVersion, got)
	}
	if other := request(enabled, http.MethodGet, "/v1/no-such-path"); other.Code != http.StatusNotImplemented {
		t.Fatalf("其余未知路径应当仍交给推理入口（501），得到 %d", other.Code)
	}
}
