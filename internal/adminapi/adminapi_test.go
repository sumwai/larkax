package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/pricing"
)

// fakeTokens 是鉴权用例里的令牌来源：不连库，直接给一份 verifier。
type fakeTokens struct {
	hash       [sha256.Size]byte
	configured bool
	err        error
}

func (f fakeTokens) Hash(context.Context) ([sha256.Size]byte, bool, error) {
	return f.hash, f.configured, f.err
}

// verifierOf 算出某个令牌的 verifier，与生产路径同一个函数（sha256.Sum256）。
func verifierOf(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// TestAuthorize 钉住鉴权的四条判据：没配置 503、缺令牌 401、错令牌 401、
// 对令牌放行。
//
// # 为什么「缺令牌」与「错令牌」要判到响应体逐字相同
//
// 判到状态码为止的话，把两条文案改成不一样不会有任何用例失败——而那句话
// 会告诉猜令牌的人「这一枚差一点」。响应体是这条判据唯一看得见的地方，
// 所以这里把它整段比一遍。
func TestAuthorize(t *testing.T) {
	ok := &API{tokens: fakeTokens{hash: verifierOf("good-token"), configured: true}}
	missing := &API{tokens: fakeTokens{configured: false}}
	broken := &API{tokens: fakeTokens{err: errors.New("库连不上")}}

	handle := func(a *API) http.Handler {
		return a.authorize(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("reached"))
		}))
	}

	serve := func(h http.Handler, auth string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, PathPrefix+"channels", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("没有配置令牌时一律 503", func(t *testing.T) {
		// 连 Authorization 都不带也要 503：未配置的部署对匿名请求回 401 的话，
		// 会让人以为「给一枚令牌就能进」——而实际上给了也进不去。
		for _, auth := range []string{"", "Bearer whatever"} {
			rec := serve(handle(missing), auth)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("Authorization=%q 时状态码 = %d，期望 503", auth, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "not enabled") {
				t.Errorf("503 的正文没说管理面没启用：%s", rec.Body)
			}
			// 不透露配置细节：是哪一项没配、存的是什么，都不该出现在给调用方的
			// 响应里。
			if strings.Contains(rec.Body.String(), "sha256") || strings.Contains(rec.Body.String(), "settings") {
				t.Errorf("503 的正文透露了配置细节：%s", rec.Body)
			}
		}
	})

	t.Run("读不出 verifier 时 503 且不转述底层错误", func(t *testing.T) {
		rec := serve(handle(broken), "Bearer good-token")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("状态码 = %d，期望 503", rec.Code)
		}
		// 底层错误可能带出连接串、主机与 settings 键名；响应里只能有一句固定的话，
		// 原因进日志。
		if strings.Contains(rec.Body.String(), "库连不上") {
			t.Errorf("503 的正文转述了底层错误：%s", rec.Body)
		}
		if !strings.Contains(rec.Body.String(), msgUnavailable) {
			t.Errorf("503 的正文不是那句固定的话：%s", rec.Body)
		}
	})

	t.Run("缺令牌与错令牌都是 401 且正文逐字相同", func(t *testing.T) {
		noHeader := serve(handle(ok), "")
		wrong := serve(handle(ok), "Bearer wrong-token")

		for name, rec := range map[string]*httptest.ResponseRecorder{
			"没给 Authorization": noHeader,
			"给了错的令牌":           wrong,
		} {
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s：状态码 = %d，期望 401", name, rec.Code)
			}
		}
		if noHeader.Body.String() != wrong.Body.String() {
			t.Errorf("两种失败的正文不一样，等于告诉猜令牌的人哪一枚接近了：\n%s\n%s",
				noHeader.Body, wrong.Body)
		}
	})

	t.Run("正确的令牌放行", func(t *testing.T) {
		rec := serve(handle(ok), "Bearer good-token")
		if rec.Code != http.StatusOK || rec.Body.String() != "reached" {
			t.Errorf("状态码 = %d，正文 = %q，期望 200/reached", rec.Code, rec.Body)
		}
	})

	t.Run("方案名大小写不敏感、写法不对就 401", func(t *testing.T) {
		for _, tc := range []struct {
			auth string
			want int
		}{
			{"bearer good-token", http.StatusOK},
			{"BEARER good-token", http.StatusOK},
			{"Basic good-token", http.StatusUnauthorized},
			{"Bearer", http.StatusUnauthorized},
			{"Bearer ", http.StatusUnauthorized},
			{"Bearer good-token extra", http.StatusUnauthorized},
			{" good-token", http.StatusUnauthorized},
		} {
			if got := serve(handle(ok), tc.auth).Code; got != tc.want {
				t.Errorf("Authorization=%q 时状态码 = %d，期望 %d", tc.auth, got, tc.want)
			}
		}
	})
}

// TestFailMapsKnownErrors 钉住错误到状态码的映射。
//
// 这一层最容易出的错是「所有错误都 400」或「所有错误都 500」：两种都能让
// 正常路径看起来是对的，而调用方拿到的是错误的处置提示。所以每一类各取
// 一个代表，把 404 / 409 / 400 分开钉。
func TestFailMapsKnownErrors(t *testing.T) {
	// 日志打给 discard：这里只量状态码与正文，不让几条预期间的错误日志混进
	// 测试输出里。
	a := &API{log: slog.New(slog.DiscardHandler)}

	// 驱动错误。映射的顺序就是这条用例要钉的那件事：catalog 与 dberr 的约束
	// 错误都把 *pgconn.PgError 留在链上，先判 pgErr 的话下面那三条会变成 500。
	pgErr := &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "model_alias_canonical_channel_key"`,
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"渠道不在", channels.ErrChannelNotFound, http.StatusNotFound},
		{"模型不在", catalog.ErrModelNotFound, http.StatusNotFound},
		{"别名不在", catalog.ErrAliasIDNotFound, http.StatusNotFound},
		{"售价规则不在", pricing.ErrRuleNotFound, http.StatusNotFound},
		{"成本规则不在", pricing.ErrCostNotFound, http.StatusNotFound},
		{"模型已存在", catalog.ErrModelExists, http.StatusConflict},
		{"别名撞车", catalog.ErrCanonicalTaken, http.StatusConflict},
		{"模型还被引用", catalog.ErrModelInUse, http.StatusConflict},
		{"没有要改的字段", channels.ErrNoChanges, http.StatusBadRequest},
		{"校验失败", errors.New("名称不能为空"), http.StatusBadRequest},
		// 被驱动错误包着的哨兵：真库上返回的正是这个形状。用例喂裸哨兵时是
		// 绿的，所以这一格必须单独钉。
		{"别名撞车被 pg 包着", errors.Join(catalog.ErrCanonicalTaken, pgErr), http.StatusConflict},
		{"约束哨兵被 pg 包着", errors.Join(dberr.ErrConstraintViolation, pgErr), http.StatusConflict},
		{"模型不在被 pg 包着", errors.Join(catalog.ErrModelNotFound, pgErr), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			a.fail(rec, tc.err)
			if rec.Code != tc.want {
				t.Errorf("状态码 = %d，期望 %d（%v）", rec.Code, tc.want, tc.err)
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("错误体不是 JSON: %v\n%s", err, rec.Body)
			}
			if body.Error.Message == "" || body.Error.Type == "" {
				t.Errorf("错误体缺字段：%+v", body.Error)
			}
		})
	}

	// 纯驱动错误：500，且正文只有一句固定的话。转述驱动原文会把连接串、主机
	// 与表名交给调用方——那些只该进日志。
	t.Run("纯驱动错误 500 且不转述原文", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.fail(rec, pgErr)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("状态码 = %d，期望 500", rec.Code)
		}
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("错误体不是 JSON: %v\n%s", err, rec.Body)
		}
		if body.Error.Message != msgDatabase {
			t.Errorf("正文 = %q，期望固定文案 %q", body.Error.Message, msgDatabase)
		}
		if strings.Contains(rec.Body.String(), "duplicate key") {
			t.Errorf("500 的正文转述了驱动原文：%s", rec.Body)
		}
	})
}

// TestQueryIDAcceptsZero 钉住 `?channel_id=0` 是合法值：它指平台级那一段。
//
// 拒绝 0 的话，「平台级」这个口径就没有一种显式写法——CLI 的 `--channel 0`
// 收，而 HTTP 这边不收，同一件事两种行为。
func TestQueryIDAcceptsZero(t *testing.T) {
	for _, tc := range []struct {
		query   string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"?channel_id=0", 0, false},
		{"?channel_id=7", 7, false},
		{"?channel_id=-1", 0, true},
		{"?channel_id=abc", 0, true},
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/aliases"+tc.query, nil)
		got, err := queryID(req, "channel_id")
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q：expect 报错，得到 %d", tc.query, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q：得到 %d, %v；期望 %d", tc.query, got, err, tc.want)
		}
	}
}
