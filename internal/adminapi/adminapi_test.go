package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
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

// strPtr 把字符串常量变成指针，给表驱动里 *string 类型的用例字段用。
func strPtr(s string) *string { return &s }

// parseInt64 只收正整数：0 与负数都报错。
//
// `id <= 0` 上的边界变异（写成 `id < 0`）**只在 0 这一格**有差别：`/models/0`
// 会从 400 变成「放行，去查一条 id=0 的记录」。所以这里必须有一条 0 的用例——
// 只测 -1 与 abc 杀不掉它。
func TestParseInt64RejectsNonPositive(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{raw: "7", want: 7},
		{raw: "0", wantErr: true},
		{raw: "-1", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "abc", wantErr: true},
	} {
		got, err := parseInt64(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseInt64(%q) = %d, nil；期望报错", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseInt64(%q) = %d, %v；期望 %d", tc.raw, got, err, tc.want)
		}
	}
}

// parseDate 要把三种情况分开：没有值（nil 或空串）回 nil 而不是错，
// 合法日期解出来，坏日期连字段名一起报。
//
// `raw == nil || *raw == ""` 里第二个 `==` 被取反（写成 `*raw != ""`）时，
// 空串会掉进 time.Parse 变成一个 400，而合法日期会被整段跳过、回 nil——
// 在更新路径上那等于「清空」。两个方向都要有：只测 nil 那一格杀不掉它。
func TestParseDate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     *string
		want    string // 空串表示期望 nil
		wantErr bool
	}{
		{name: "nil 是这一栏没有值", raw: nil},
		{name: "空串也是没有值", raw: strPtr("")},
		{name: "合法日期解出来", raw: strPtr("2024-03-01"), want: "2024-03-01"},
		{name: "日历上不存在的日期", raw: strPtr("2024-02-30"), wantErr: true},
		{name: "形状不对", raw: strPtr("03/01/2024"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDate("knowledge_cutoff", tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s：parseDate = %v, nil；期望报错", tc.name, got)
				}
				if !strings.Contains(err.Error(), "knowledge_cutoff") {
					t.Errorf("错误里没有字段名，调用方不知道是哪一栏坏了: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s：parseDate 报错: %v", tc.name, err)
			}
			if tc.want == "" {
				if got != nil {
					t.Errorf("%s：parseDate = %v；期望 nil", tc.name, got)
				}
				return
			}
			if got == nil || got.Format("2006-01-02") != tc.want {
				t.Errorf("%s：parseDate = %v；期望 %s", tc.name, got, tc.want)
			}
		})
	}
}

// 端点那一列的三态在响应里要各自保住：库里是 NULL 时编成 null，
// 明确一条都不支持时编成 []，声明了哪几条就列出哪几条。
//
// `model.Capabilities.Endpoints != nil` 被取反（写成 `== nil`）时，两版在
// **每一条模型上都把 null 与 [] 交换了**：没声明过的变成「明确一条都不支持」，
// 声明过的变成「没配过」。列表接口的调用方正是靠 null 区分这两件事；
// 把没配过的当成「一条都不支持」，会让一条只填了名字的记录选不到任何渠道。
//
// 断言落在编码后的 JSON 上，并且取的是 capabilities.endpoints 这一个字段，
// 不是对整条响应做子串匹配：null 与 [] 的区别只在线上存在，Go 里两者都
// len()==0；而整条子串匹配只能说明「这串字在某个地方出现过」，说明不了是哪一栏。
func TestModelViewKeepsEndpointTriState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoints []channel.Endpoint
		want      string
	}{
		{name: "未声明是 null", endpoints: nil, want: `null`},
		{name: "明确一条都不支持是空数组", endpoints: []channel.Endpoint{}, want: `[]`},
		{
			name:      "声明了端点就照列",
			endpoints: []channel.Endpoint{channel.EndpointChatCompletions},
			want:      `["/v1/chat/completions"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := modelViewOf(catalog.Model{
				Capabilities: catalog.Capabilities{Endpoints: tc.endpoints},
			})
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatalf("编码 modelView 失败: %v", err)
			}
			var wire struct {
				Capabilities struct {
					Endpoints json.RawMessage `json:"endpoints"`
				} `json:"capabilities"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("解码 modelView 失败: %v\n%s", err, raw)
			}
			if got := string(wire.Capabilities.Endpoints); got != tc.want {
				t.Errorf("capabilities.endpoints = %s，期望 %s（整条响应：%s）", got, tc.want, raw)
			}
		})
	}
}

// TestPrefixMustBeASubtree 钉住 Prefix 的形状校验：以 / 开头、以 / 结尾。
//
// 两半必须同时判。只判一半的实现下，`x/` 与 `/x` 都能通过，而它们在路由表
// 里指向的路径与调用方以为的不是同一条——症状只是 404。
func TestPrefixMustBeASubtree(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		panics bool
	}{
		{name: "默认前缀", prefix: ""},
		{name: "子树", prefix: "/api/v1/merchant/"},
		{name: "缺前导斜杠", prefix: "x/", panics: true},
		{name: "缺结尾斜杠", prefix: "/x", panics: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				New(Config{Prefix: tc.prefix})
			}()
			if tc.panics && recovered == nil {
				t.Errorf("Prefix %q 没有 panic", tc.prefix)
			}
			if !tc.panics && recovered != nil {
				t.Errorf("Prefix %q panic 了: %v", tc.prefix, recovered)
			}
		})
	}
}

// TestOwnerContextRejectsNonPositive 钉住归属上下文只认正整数：
// 0 与负数都读成「没有归属」，而不是「归属是 0」。
func TestOwnerContextRejectsNonPositive(t *testing.T) {
	ctx := context.Background()

	if _, ok := OwnerFromContext(ctx); ok {
		t.Error("空 context 被判成有归属")
	}
	for _, id := range []int64{0, -1} {
		if got, ok := OwnerFromContext(WithOwner(ctx, id)); ok {
			t.Errorf("WithOwner(%d) 读回 (%d, true)，期望 (0, false)", id, got)
		}
	}
	if got, ok := OwnerFromContext(WithOwner(ctx, 7)); !ok || got != 7 {
		t.Errorf("WithOwner(7) 读回 (%d, %v)，期望 (7, true)", got, ok)
	}
}

// TestFailScopedOnlyRewritesNotFound 钉住归属受限的 404 改写只发生在「有归属
// 且确实是 ErrChannelNotFound」这两条同时成立时。
//
// 判成「或」的话，内部管理面（没有归属）的不存在渠道也会丢掉 id 那一句，
// 运维就分不清是 id 打错了还是路径写错了；判成「与」之外再松一格，受归属
// 限制的其它错误会被说成 not found，真正的原因只剩日志。
func TestFailScopedOnlyRewritesNotFound(t *testing.T) {
	notFound := fmt.Errorf("%w：id 42", channels.ErrChannelNotFound)
	owner := int64(7)
	cases := []struct {
		name     string
		owner    *int64
		wantBody string
		absent   string
	}{
		{name: "受归属限制", owner: &owner, wantBody: msgNotFound, absent: "42"},
		{name: "内部管理面", wantBody: "渠道不存在", absent: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.owner != nil {
				ctx = WithOwner(ctx, *tc.owner)
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, PathPrefix+"channels/42", nil)
			rec := httptest.NewRecorder()

			(&API{}).failScoped(rec, req, notFound)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("正文 = %s，期望包含 %q", body, tc.wantBody)
			}
			if tc.absent != "" && strings.Contains(body, tc.absent) {
				t.Errorf("正文 = %s，不该包含 %q", body, tc.absent)
			}
		})
	}
}

// TestRuleViewCarriesScheduleError 钉住控制台接口也看得见解不开的定价表。
//
// 清单对坏表是降级的（那一行照常出），Web 上如果不带原因，这一行看起来就是
// 一条手写表达式规则——而降级正是为了不把那行藏起来。与 expr_error 同一个
// 形状：有错才出现，没错时这一栏整个不出现。
func TestRuleViewCarriesScheduleError(t *testing.T) {
	reason := errors.New("规则 9 的 schedule 不是合法的定价表: 定价表解析失败")
	data, err := json.Marshal(ruleViewOf(pricing.RuleInfo{ID: 9, ScheduleErr: reason}))
	if err != nil {
		t.Fatalf("编码规则视图失败: %v", err)
	}
	if !strings.Contains(string(data), `"schedule_error":`) {
		t.Errorf("规则视图里没有 schedule_error：%s", data)
	}
	if !strings.Contains(string(data), "不是合法的定价表") {
		t.Errorf("规则视图里没有解不开的原因：%s", data)
	}

	clean, err := json.Marshal(ruleViewOf(pricing.RuleInfo{ID: 10}))
	if err != nil {
		t.Fatalf("编码规则视图失败: %v", err)
	}
	if strings.Contains(string(clean), "schedule_error") {
		t.Errorf("没有坏表时这一栏不该出现：%s", clean)
	}
}
