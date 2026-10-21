package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sumwai/larkax/internal/adminapi"
	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/textshape"
)

// 本文件用一个零数据库的装配验 HTTP 层的行为：状态码、错误码、CSRF、
// 归属边界、三态字段与响应形状。
//
// 数据库那一侧（会话过期、锁定的 SQL、聚合口径）由各包自己的集成测试覆盖。
// 这里的替身只实现「判据用得到的那几列」，不模拟 SQL——模拟出来的 SQL 与
// 真 SQL 分岔时，这组用例会一起说谎。

// testNow 是所有用例的固定时刻。
var testNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// testKDF 与 auth 包里那一份同参：盐与输出长度取生产值，只调小轮数。
var testKDF = auth.KDFParams{Iterations: 1000, SaltBytes: 16, KeyBytes: 32}

// ── 替身 ────────────────────────────────────────────────────────────────

type fakeSessions struct {
	byToken   map[string]auth.Session
	created   []auth.Session
	revoked   []int64
	touchErr  error
	lookupErr error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{byToken: make(map[string]auth.Session)}
}

func (f *fakeSessions) add(token string, s auth.Session) {
	f.byToken[token] = s
}

func (f *fakeSessions) CreateSession(_ context.Context, userID int64, _ string, _ net.IP) (auth.Session, string, error) {
	s := auth.Session{
		ID:         int64(len(f.created) + 1),
		UserID:     userID,
		CSRFToken:  "csrf-for-session",
		CreatedAt:  testNow,
		LastSeenAt: testNow,
		ExpiresAt:  testNow.Add(auth.SessionIdleTTL),
		UserStatus: auth.UserStatusActive,
	}
	f.created = append(f.created, s)
	token := "token-" + strconv.FormatInt(s.ID, 10)
	f.byToken[token] = s
	return s, token, nil
}

func (f *fakeSessions) SessionByToken(_ context.Context, token string) (auth.Session, error) {
	if f.lookupErr != nil {
		return auth.Session{}, f.lookupErr
	}
	s, ok := f.byToken[token]
	if !ok {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	return s, nil
}

func (f *fakeSessions) TouchSession(_ context.Context, _ auth.Session, _ time.Time) error {
	return f.touchErr
}

func (f *fakeSessions) RevokeSession(_ context.Context, id int64) error {
	f.revoked = append(f.revoked, id)
	return nil
}

type fakeAccounts struct {
	byName     map[string]auth.LoginUser
	infos      map[int64]auth.UserInfo
	failures   int
	cleared    int
	rehashed   int
	passwords  map[int64]string
	updateFail error
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		byName:    make(map[string]auth.LoginUser),
		infos:     make(map[int64]auth.UserInfo),
		passwords: make(map[int64]string),
	}
}

func (f *fakeAccounts) put(u auth.LoginUser, info auth.UserInfo) {
	f.byName[u.Username] = u
	f.infos[u.ID] = info
}

func (f *fakeAccounts) LoginUserByName(_ context.Context, username string) (auth.LoginUser, error) {
	u, ok := f.byName[username]
	if !ok {
		return auth.LoginUser{}, auth.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeAccounts) LoginUserByID(_ context.Context, userID int64) (auth.LoginUser, error) {
	for _, u := range f.byName {
		if u.ID == userID {
			return u, nil
		}
	}
	return auth.LoginUser{}, auth.ErrUserNotFound
}

func (f *fakeAccounts) RecordLoginFailure(_ context.Context, _ int64, _ time.Time) (int, *time.Time, error) {
	f.failures++
	return f.failures, nil, nil
}

func (f *fakeAccounts) ClearLoginFailures(_ context.Context, _ int64) error {
	f.cleared++
	return nil
}

func (f *fakeAccounts) RehashPassword(_ context.Context, userID int64, hash string) error {
	f.rehashed++
	f.passwords[userID] = hash
	return nil
}

func (f *fakeAccounts) SetPassword(_ context.Context, userID int64, hash string, _ *int64) error {
	f.passwords[userID] = hash
	return nil
}

func (f *fakeAccounts) UserByRef(_ context.Context, ref string) (auth.UserInfo, error) {
	if id, err := parseID(ref); err == nil {
		info, ok := f.infos[id]
		if !ok {
			return auth.UserInfo{}, auth.ErrUserNotFound
		}
		return info, nil
	}
	for _, info := range f.infos {
		if info.Username == ref {
			return info, nil
		}
	}
	return auth.UserInfo{}, auth.ErrUserNotFound
}

func (f *fakeAccounts) UpdateUser(ctx context.Context, ref string, p auth.UpdateUserParams) (auth.UserInfo, error) {
	if f.updateFail != nil {
		return auth.UserInfo{}, f.updateFail
	}
	info, err := f.UserByRef(ctx, ref)
	if err != nil {
		return auth.UserInfo{}, err
	}
	if p.DisplayName != nil {
		info.DisplayName = *p.DisplayName
	}
	if p.Email != nil {
		info.Email = *p.Email
	}
	f.infos[info.ID] = info
	return info, nil
}

type fakeKeys struct {
	keys       map[int64]auth.Key
	nextID     int64
	createFail error
	updateFail error
	lastPageQ  auth.KeyListQuery
	lastUpdate struct {
		id     int64
		params auth.KeyParams
	}
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{keys: make(map[int64]auth.Key), nextID: 1}
}

func (f *fakeKeys) put(k auth.Key) {
	f.keys[k.ID] = k
	if k.ID >= f.nextID {
		f.nextID = k.ID + 1
	}
}

func (f *fakeKeys) Create(_ context.Context, p auth.CreateParams) (auth.Created, error) {
	if f.createFail != nil {
		return auth.Created{}, f.createFail
	}
	id := f.nextID
	f.nextID++
	f.put(auth.Key{
		ID: id, UserID: p.UserID, Username: "jdoe", Name: p.Name,
		Prefix: "sk-a1b2c3d4", Suffix: "x9Yz2K", Status: "active",
		RateLimitRPM: p.RateLimitRPM, QuotaLimit: p.QuotaLimit,
		ExpiresAt: p.ExpiresAt, CreatedAt: testNow,
	})
	return auth.Created{
		ID: id, Prefix: "sk-a1b2c3d4", Suffix: "x9Yz2K",
		Plaintext: "sk-a1b2c3d4-placeholder-x9Yz2K",
	}, nil
}

func (f *fakeKeys) UpdateKey(_ context.Context, keyID int64, p auth.KeyParams) (auth.Key, error) {
	key, ok := f.keys[keyID]
	if !ok {
		return auth.Key{}, auth.ErrKeyNotFound
	}
	if f.updateFail != nil {
		return auth.Key{}, f.updateFail
	}
	f.lastUpdate.id, f.lastUpdate.params = keyID, p
	if p.SetName {
		key.Name = p.Name
	}
	if p.SetStatus {
		key.Status = p.Status
	}
	if p.SetQuota {
		key.QuotaLimit = p.QuotaLimit
	}
	if p.SetRateLimit {
		key.RateLimitRPM = p.RateLimitRPM
	}
	if p.SetExpiresAt {
		key.ExpiresAt = p.ExpiresAt
	}
	f.keys[keyID] = key
	return key, nil
}

func (f *fakeKeys) KeyForUser(_ context.Context, userID, keyID int64) (auth.Key, error) {
	key, ok := f.keys[keyID]
	if !ok || key.UserID != userID {
		return auth.Key{}, auth.ErrKeyNotFound
	}
	return key, nil
}

func (f *fakeKeys) ListKeysPage(_ context.Context, q auth.KeyListQuery) (auth.KeyListPage, error) {
	f.lastPageQ = q
	var page auth.KeyListPage
	for _, id := range sortedIDs(f.keys) {
		key := f.keys[id]
		if key.UserID != q.UserID {
			continue
		}
		if q.Status != "" && key.Status != q.Status {
			continue
		}
		page.Keys = append(page.Keys, key)
	}
	return page, nil
}

type fakeUsage struct {
	summary   store.UsageSummary
	entries   store.UserEntryPage
	requests  store.UsageRequestPage
	wallets   []store.Wallet
	lastEntry store.UserEntryQuery
	lastReq   store.UsageRequestQuery
}

func (f *fakeUsage) UsageSummary(_ context.Context, _ store.UsageWindow) (store.UsageSummary, error) {
	return f.summary, nil
}

func (f *fakeUsage) UsageSeries(_ context.Context, _ store.UsageWindow, _ string, _ *time.Location) ([]store.UsagePoint, error) {
	return []store.UsagePoint{{Start: testNow, UsageTotals: f.summary.Totals}}, nil
}

func (f *fakeUsage) UsageBreakdown(_ context.Context, _ store.UsageWindow, groupBy string, _ int) (store.UsageBreakdown, error) {
	if groupBy == "bogus" {
		return store.UsageBreakdown{}, store.ErrInvalidQuery
	}
	return store.UsageBreakdown{}, nil
}

func (f *fakeUsage) UsageRequests(_ context.Context, q store.UsageRequestQuery) (store.UsageRequestPage, error) {
	f.lastReq = q
	return f.requests, nil
}

func (f *fakeUsage) Wallets(_ context.Context, _ *int64) ([]store.Wallet, error) {
	return f.wallets, nil
}

func (f *fakeUsage) UserEntries(_ context.Context, q store.UserEntryQuery) (store.UserEntryPage, error) {
	f.lastEntry = q
	return f.entries, nil
}

type fakeModels struct {
	names []string
}

func (f *fakeModels) SuppliedModels(_ context.Context) ([]string, error) {
	return f.names, nil
}

type fakeCatalog struct {
	models []catalog.Model
}

func (f *fakeCatalog) Models(_ context.Context) ([]catalog.Model, error) {
	return f.models, nil
}

type fakePrices struct {
	prices map[string]*pricing.ReferencePrice
}

func (f *fakePrices) ReferencePrices(_ context.Context, _ []string, _ time.Time) (map[string]*pricing.ReferencePrice, error) {
	return f.prices, nil
}

type fakeSupplies struct {
	supplies []channels.Supply
}

func (f *fakeSupplies) Supplies(_ context.Context) ([]channels.Supply, error) {
	return f.supplies, nil
}

type fakeOffers struct {
	base    money.Amount
	byOwner map[int64]money.Amount
}

func (f *fakeOffers) Prices(_ context.Context, _ time.Time, candidates []pricing.Candidate) ([]pricing.CandidatePrice, error) {
	out := []pricing.CandidatePrice{}
	for _, candidate := range candidates {
		price := f.base
		if override, ok := f.byOwner[candidate.OwnerUserID]; ok {
			price = override
		}
		out = append(out, pricing.CandidatePrice{
			Model:       candidate.Model,
			ChannelID:   candidate.ChannelID,
			OwnerUserID: candidate.OwnerUserID,
			Merchant:    candidate.Merchant,
			Price:       price,
		})
	}
	return out, nil
}

// ── 装配 ────────────────────────────────────────────────────────────────

// consoleFixture 是一次装配的全部替身。
type consoleFixture struct {
	handler  http.Handler
	sessions *fakeSessions
	accounts *fakeAccounts
	keys     *fakeKeys
	usage    *fakeUsage
	models   *fakeModels
	supplies *fakeSupplies
	catalog  *fakeCatalog
	prices   *fakePrices
	offers   *fakeOffers
	spent    map[int64]money.Amount
	spentN   int
	// 两组角色路由的资源 handler 是替身：用例验的是角色中间件，不是资源
	// handler（后者在 internal/adminapi 用自己的集成测试验）。这三项记下
	// 替身被调用了几次、以及 context 里的归属是什么。
	merchantAdminCalls int
	adminAdminCalls    int
	merchantOwner      int64
	adminOwner         int64
	// clock 是 handler 读到的「现在」。用例改它的值来模拟时间流逝。
	clock *time.Time
}

// newFixture 装出一份 handler 与它的替身。
//
// 默认有一个 active、已设口令的用户 jdoe（id=7），以及一个属于他的会话
// （cookie 值 session-token）。
func newFixture(t *testing.T) *consoleFixture {
	t.Helper()

	sessions := newFakeSessions()
	accounts := newFakeAccounts()
	keys := newFakeKeys()
	usage := &fakeUsage{}

	hash, err := auth.HashPassword(testKDF, "correct horse battery staple")
	if err != nil {
		t.Fatalf("生成测试口令哈希失败: %v", err)
	}
	accounts.put(auth.LoginUser{
		ID: 7, Username: "jdoe", Status: auth.UserStatusActive,
		PasswordHash: hash,
	}, auth.UserInfo{
		ID: 7, Username: "jdoe", DisplayName: "J. Doe",
		Email: "user@example.com", Status: auth.UserStatusActive,
		Role: auth.RoleUser, CreatedAt: testNow.Add(-60 * 24 * time.Hour),
	})
	sessions.add("session-token", auth.Session{
		ID: 1, UserID: 7, CSRFToken: "csrf-token", CreatedAt: testNow,
		LastSeenAt: testNow, ExpiresAt: testNow.Add(auth.SessionIdleTTL),
		UserStatus: auth.UserStatusActive, Role: auth.RoleUser,
	})

	f := &consoleFixture{
		sessions: sessions, accounts: accounts, keys: keys, usage: usage,
		models:   &fakeModels{},
		supplies: &fakeSupplies{},
		catalog:  &fakeCatalog{},
		prices:   &fakePrices{prices: map[string]*pricing.ReferencePrice{}},
		offers:   &fakeOffers{},
		spent:    map[int64]money.Amount{},
	}
	now := testNow
	f.clock = &now
	f.handler = newAPI(Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		KDF:      testKDF,
		SiteName: "larkax",
		Currency: "CNY",
		Now:      func() time.Time { return now },
	}, deps{
		sessions:      sessions,
		accounts:      accounts,
		keys:          keys,
		usage:         usage,
		models:        f.models,
		supplies:      f.supplies,
		catalog:       f.catalog,
		prices:        f.prices,
		offers:        f.offers,
		merchantAdmin: stubAdmin(&f.merchantAdminCalls, &f.merchantOwner),
		adminAdmin:    stubAdmin(&f.adminAdminCalls, &f.adminOwner),
		spent: func(_ context.Context, ids []int64) (map[int64]money.Amount, error) {
			f.spentN++
			out := make(map[int64]money.Amount, len(ids))
			for _, id := range ids {
				out[id] = f.spent[id]
			}
			return out, nil
		},
	})
	return f
}

// stubAdmin 是角色组资源 handler 的替身：记下调用次数与 context 里的归属，
// 回 204。204 而不是 200 是为了让「中间件放行了」与「资源 handler 返回了
// 一个成功体」在断言里分得开。
func stubAdmin(calls *int, owner *int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		*owner = 0
		if id, ok := adminapi.OwnerFromContext(r.Context()); ok {
			*owner = id
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// request 发一次请求。
//
// RemoteAddr 固定成 loopback：那让口令端点在不带 TLS 的测试里也过传输校验
// （本地开发的例外，见 transportSecure）。要验 400 insecure_transport 的用例
// 自己把远端改成公网地址。
func (f *consoleFixture) request(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.requestAs(t, "127.0.0.1:8080", method, path, body)
}

func (f *consoleFixture) requestAs(t *testing.T, remote, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.requestAsWith(t, remote, nil, method, path, body)
}

// requestAsWith 与 requestAs 相同，另外带上若干请求头。
func (f *consoleFixture) requestAsWith(t *testing.T, remote string, headers map[string]string,
	method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	req.RemoteAddr = remote
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// authed 带上会话 Cookie 发请求。
func (f *consoleFixture) authed(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.authedWith(t, "session-token", nil, method, path, body)
}

func (f *consoleFixture) authedWith(t *testing.T, cookie string, headers map[string]string,
	method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.authedAsWith(t, "127.0.0.1:8080", cookie, headers, method, path, body)
}

// authedAsWith 与 authedWith 相同，但来源地址由调用方给。
func (f *consoleFixture) authedAsWith(t *testing.T, remote, cookie string,
	headers map[string]string, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	req.RemoteAddr = remote
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// writeHeaders 是写方法要带的 CSRF 头。
func writeHeaders() map[string]string {
	return map[string]string{"X-CSRF-Token": "csrf-token"}
}

// errorCode 解出错误体里的 code。
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON（%d）: %s", rec.Code, rec.Body.String())
	}
	return body.Error.Code
}

// ── 用例 ────────────────────────────────────────────────────────────────

func TestSessionRequired(t *testing.T) {
	f := newFixture(t)

	rec := f.request(t, http.MethodGet, PathPrefix+"me", "")
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != codeUnauthenticated {
		t.Fatalf("没有 Cookie 的 /me 应当 401 unauthenticated，得到 %d: %s",
			rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("响应缺少 X-Request-ID")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", rec.Header().Get("Cache-Control"))
	}

	// 已过期：行还在，但不能再放行。
	f.sessions.add("expired", auth.Session{
		ID: 2, UserID: 7, CSRFToken: "csrf-token",
		ExpiresAt: testNow.Add(-time.Minute), UserStatus: auth.UserStatusActive,
	})
	if got := f.authedWith(t, "expired", nil, http.MethodGet, PathPrefix+"me", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("过期会话应当 401，得到 %d", got.Code)
	}

	// 用户被停用：403，与「没有会话」分开。
	f.sessions.add("disabled", auth.Session{
		ID: 3, UserID: 7, CSRFToken: "csrf-token",
		ExpiresAt: testNow.Add(time.Hour), UserStatus: auth.UserStatusSuspended,
	})
	rec = f.authedWith(t, "disabled", nil, http.MethodGet, PathPrefix+"me", "")
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != codeAccountDisabled {
		t.Fatalf("停用用户的会话应当 403 account_disabled，得到 %d: %s",
			rec.Code, rec.Body.String())
	}

	// 库故障不是「登录过期」：503，不是 401。
	f.sessions.lookupErr = errors.New("connection refused")
	if got := f.authedWith(t, "session-token", nil, http.MethodGet, PathPrefix+"me", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("会话查询失败应当 503，得到 %d", got.Code)
	}
}

func TestCSRFOnWriteMethods(t *testing.T) {
	f := newFixture(t)

	rec := f.authed(t, http.MethodPatch, PathPrefix+"me", `{"display_name":"X"}`)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != codeCSRFFailed {
		t.Fatalf("缺 CSRF 的 PATCH 应当 403 csrf_failed，得到 %d: %s",
			rec.Code, rec.Body.String())
	}

	headers := writeHeaders()
	headers["Origin"] = "https://evil.example.com"
	if got := f.authedWith(t, "session-token", headers, http.MethodPatch, PathPrefix+"me",
		`{"display_name":"X"}`); got.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 的写请求应当 403，得到 %d", got.Code)
	}

	headers = writeHeaders()
	headers["Sec-Fetch-Site"] = "cross-site"
	if got := f.authedWith(t, "session-token", headers, http.MethodPatch, PathPrefix+"me",
		`{"display_name":"X"}`); got.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site: cross-site 的写请求应当 403，得到 %d", got.Code)
	}

	// 同源（没有 Origin / Sec-Fetch-Site 的非浏览器客户端也算）放行。
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch, PathPrefix+"me",
		`{"display_name":"X"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("带 CSRF 的 PATCH 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginFailures(t *testing.T) {
	f := newFixture(t)
	const body = `{"username":"jdoe","password":"wrong password"}`

	// 明文 HTTP（公网对端）直接拒，口令不进 KDF。
	rec := f.requestAs(t, "203.0.113.9:5555", http.MethodPost, PathPrefix+"auth/login", body)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeInsecureTransport {
		t.Fatalf("明文传输应当 400 insecure_transport，得到 %d: %s",
			rec.Code, rec.Body.String())
	}
	if len(f.sessions.created) != 0 {
		t.Fatal("明文传输的登录仍然建了会话")
	}

	// 用户不存在：401，且不建会话。
	if got := f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"nobody","password":"wrong password"}`); got.Code != http.StatusUnauthorized ||
		errorCode(t, got) != codeInvalidCredential {
		t.Fatalf("不存在的用户应当 401 invalid_credentials，得到 %d: %s",
			got.Code, got.Body.String())
	}

	// 口令不对：401，并记一次失败。
	if got := f.request(t, http.MethodPost, PathPrefix+"auth/login", body); got.Code != http.StatusUnauthorized {
		t.Fatalf("错误口令应当 401，得到 %d", got.Code)
	}
	if f.accounts.failures != 1 {
		t.Errorf("失败计数 = %d，期望 1", f.accounts.failures)
	}

	// 锁定期间不验证口令：429 + Retry-After。
	locked := f.accounts.byName["jdoe"]
	until := testNow.Add(2 * time.Minute)
	locked.LockedUntil = &until
	f.accounts.byName["jdoe"] = locked
	rec = f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"jdoe","password":"correct horse battery staple"}`)
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != codeRateLimited {
		t.Fatalf("锁定期间应当 429 rate_limited，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 缺少 Retry-After")
	}
	if len(f.sessions.created) != 0 {
		t.Fatal("锁定期间仍然建了会话")
	}

	// 还没设口令：403 account_not_ready，不是 401。
	locked.LockedUntil = nil
	locked.PasswordHash = ""
	f.accounts.byName["jdoe"] = locked
	if rec := f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"jdoe","password":"correct horse battery staple"}`); rec.Code != http.StatusForbidden ||
		errorCode(t, rec) != codeAccountNotReady {
		t.Fatalf("未设口令应当 403 account_not_ready，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginSuccess(t *testing.T) {
	f := newFixture(t)
	rec := f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"jdoe","password":"correct horse battery staple"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应当只写一个 Cookie，得到 %d 个", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != auth.SessionCookieName || cookie.Value == "" {
		t.Fatalf("Cookie 名/值不对: %q=%q", cookie.Name, cookie.Value)
	}
	if !cookie.Secure || !cookie.HttpOnly {
		t.Errorf("Cookie 缺 Secure 或 HttpOnly: %+v", cookie)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v，期望 Lax", cookie.SameSite)
	}

	var body sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.CSRFToken == "" || body.User.Username != "jdoe" {
		t.Errorf("响应缺 csrf_token 或用户名: %+v", body)
	}
	if f.accounts.cleared != 1 {
		t.Errorf("登录成功没有清零失败计数")
	}

	// 停用的账号：口令对了也不发会话。
	info := f.accounts.infos[7]
	info.Status = auth.UserStatusBanned
	f.accounts.infos[7] = info
	login := f.accounts.byName["jdoe"]
	login.Status = auth.UserStatusBanned
	f.accounts.byName["jdoe"] = login
	before := len(f.sessions.created)
	rec = f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"jdoe","password":"correct horse battery staple"}`)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != codeAccountDisabled {
		t.Fatalf("被停用的账号应当 403 account_disabled，得到 %d: %s",
			rec.Code, rec.Body.String())
	}
	if len(f.sessions.created) != before {
		t.Fatal("被停用的账号仍然建了会话")
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	f := newFixture(t)

	// 没有 Cookie：204，且不报错。
	rec := f.request(t, http.MethodPost, PathPrefix+"auth/logout", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("无 Cookie 的登出应当 204，得到 %d", rec.Code)
	}

	// 有 Cookie 但没有 CSRF：403。
	if got := f.authed(t, http.MethodPost, PathPrefix+"auth/logout", ""); got.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 的登出应当 403，得到 %d", got.Code)
	}

	// 正常登出：204，并吊销那一行。
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"auth/logout", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("登出应当 204，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if len(f.sessions.revoked) != 1 || f.sessions.revoked[0] != 1 {
		t.Errorf("吊销的会话 = %v，期望 [1]", f.sessions.revoked)
	}
}

func TestKeyOwnershipIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.keys.put(auth.Key{
		ID: 42, UserID: 99, Prefix: "sk-other", Status: "active", CreatedAt: testNow,
	})

	rec := f.authed(t, http.MethodGet, PathPrefix+"keys/42", "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != codeNotFound {
		t.Fatalf("别人的 key 应当 404 not_found，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyOwnershipFoldIntoQuery(t *testing.T) {
	// 归属检查必须在查询里：Handler 拿到的只有「当前用户 + id」，
	// 它没有机会在读出来之后比 user_id。这一条钉住的是那个调用形状。
	f := newFixture(t)
	f.keys.put(auth.Key{ID: 42, UserID: 7, Status: "active", CreatedAt: testNow})
	rec := f.authed(t, http.MethodGet, PathPrefix+"keys/42", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("自己的 key 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPatchKeyTriState(t *testing.T) {
	f := newFixture(t)
	f.keys.put(auth.Key{
		ID: 42, UserID: 7, Name: "Production", Status: "active",
		QuotaLimit: ptrAmount(t, "200.00000000"), RateLimitRPM: ptrInt(60),
		CreatedAt: testNow,
	})

	// null = 清空（不限），0 = 一个都不放行。两者不能合并。
	rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/42", `{"quota_limit":null,"rate_limit_rpm":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH key 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	got := f.keys.lastUpdate.params
	if !got.SetQuota || got.QuotaLimit != nil {
		t.Errorf("quota_limit=null 应当 SetQuota=true 且值为 nil，得到 %+v", got)
	}
	if !got.SetRateLimit || got.RateLimitRPM == nil || *got.RateLimitRPM != 0 {
		t.Errorf("rate_limit_rpm=0 应当 SetRateLimit=true 且值为 0，得到 %+v", got)
	}
	if got.SetName {
		t.Error("没有给 name 时 SetName 应当为 false")
	}

	// 0 是「一分不放行」，与 null 的「不限」是两件事；负数才是形状错误。
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/42", `{"quota_limit":"0.00000000"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("quota_limit=0 应当 200（一分不放行），得到 %d: %s",
			rec.Code, rec.Body.String())
	}
	got = f.keys.lastUpdate.params
	if !got.SetQuota || got.QuotaLimit == nil || !got.QuotaLimit.IsZero() {
		t.Errorf("quota_limit=0 应当 SetQuota=true 且值为 0，得到 %+v", got)
	}
	if rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/42", `{"quota_limit":"-1.00000000"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("负额度应当 400，得到 %d", rec.Code)
	}

	// 没有字段：400。
	if rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/42", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 PATCH 应当 400，得到 %d", rec.Code)
	}

	// revoked 只能由吊销端点产生。
	if rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/42", `{"status":"revoked"}`); rec.Code != http.StatusConflict {
		t.Fatalf("写入 revoked 应当 409，得到 %d", rec.Code)
	}

	// 已吊销的 key 拒绝一切修改。
	revoked := f.keys.keys[42]
	revoked.Status = "revoked"
	f.keys.keys[42] = revoked
	if rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/42", `{"name":"CI"}`); rec.Code != http.StatusConflict {
		t.Fatalf("改已吊销的 key 应当 409，得到 %d", rec.Code)
	}
}

func TestCreateKeyValidation(t *testing.T) {
	f := newFixture(t)

	cases := []string{
		`{"quota_limit":"0.00000000"}`,
		`{"quota_limit":"-1.00000000"}`,
		`{"quota_limit":null}`,
		`{"rate_limit_rpm":-1}`,
		`{"expires_at":"2020-01-01T00:00:00Z"}`,
		`{"expires_at":"tomorrow"}`,
		`{"unknown":"x"}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
				PathPrefix+"keys", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 应当 400，得到 %d: %s", body, rec.Code, rec.Body.String())
			}
		})
	}

	rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"keys", `{"name":"CI","quota_limit":"200.00000000"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("合法的创建应当 201，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != PathPrefix+"keys/1" {
		t.Errorf("Location = %q，期望 %skeys/1", got, PathPrefix)
	}
	var body struct {
		Key    keyJSON `json:"key"`
		Secret string  `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Secret == "" || !strings.HasPrefix(body.Secret, "sk-") {
		t.Errorf("响应里没有一次性明文: %+v", body)
	}
	if body.Key.QuotaLimit == nil || *body.Key.QuotaLimit != "200.00000000" {
		t.Errorf("quota_limit = %v，期望 200.00000000", body.Key.QuotaLimit)
	}
	if body.Key.QuotaUsed != "0.00000000" {
		t.Errorf("新 key 的 quota_used = %q，期望 0.00000000", body.Key.QuotaUsed)
	}
}

func TestListKeysUsesBatchSpentAndCursor(t *testing.T) {
	f := newFixture(t)
	f.keys.put(auth.Key{ID: 2, UserID: 7, Status: "active", CreatedAt: testNow})
	f.keys.put(auth.Key{ID: 1, UserID: 7, Status: "active", CreatedAt: testNow.Add(-time.Hour)})
	f.spent[1] = mustAmount(t, "1.23000000")
	f.spent[2] = mustAmount(t, "4.56000000")

	rec := f.authed(t, http.MethodGet, PathPrefix+"keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列 key 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if f.spentN != 1 {
		t.Errorf("quota_used 的批量查询调了 %d 次，期望 1 次", f.spentN)
	}
	var page keyPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("列出 %d 把 key，期望 2", len(page.Items))
	}
	if page.Items[0].ID != 2 || page.Items[0].QuotaUsed != "4.56000000" {
		t.Errorf("第一项 = %+v，期望 id=2、quota_used=4.56000000", page.Items[0])
	}
	if page.NextCursor != nil {
		t.Errorf("More=false 时 next_cursor 应当是 null，得到 %q", *page.NextCursor)
	}

	// 游标：坏串 400，好串能解回同一对 (created_at, id)。
	if rec := f.authed(t, http.MethodGet, PathPrefix+"keys?cursor=not-a-cursor", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏游标应当 400，得到 %d", rec.Code)
	}
	cur := encodeCursor(testNow, 2)
	if rec := f.authed(t, http.MethodGet, PathPrefix+"keys?cursor="+cur, ""); rec.Code != http.StatusOK {
		t.Fatalf("好游标应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if f.keys.lastPageQ.Cursor == nil || f.keys.lastPageQ.Cursor.ID != 2 ||
		!f.keys.lastPageQ.Cursor.CreatedAt.Equal(testNow) {
		t.Errorf("游标解回 = %+v", f.keys.lastPageQ.Cursor)
	}

	// 非法 status 在解析参数时就拒。
	if rec := f.authed(t, http.MethodGet, PathPrefix+"keys?status=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 应当 400，得到 %d", rec.Code)
	}
}

func TestMaskedKeyShape(t *testing.T) {
	if got := maskKey("sk-a1b2c3d4", "x9Yz2K"); got != "sk-a1b2c3d4******************x9Yz2K" {
		t.Errorf("maskKey = %q", got)
	}
	// 老行没有后缀：退化成前缀加省略号，不猜一个。
	if got := maskKey("larkax_abcd", ""); got != "larkax_abcd…" {
		t.Errorf("无后缀的 maskKey = %q", got)
	}
}

func TestUsageShapes(t *testing.T) {
	f := newFixture(t)
	f.usage.summary = store.UsageSummary{
		Totals: store.UsageTotals{
			Cost: mustAmount(t, "12.34000000"), Requests: 183,
			PromptTokens: 120340, CompletionTokens: 88120, ReasoningTokens: 2100,
		},
		Paths: []store.UsagePathTotal{
			{Path: "measured", Cost: mustAmount(t, "12.10000000"), Requests: 176},
			{Path: "estimated", Cost: mustAmount(t, "0.24000000"), Requests: 7},
			{Path: "fallback", Cost: money.Zero, Requests: 0},
		},
	}

	rec := f.authed(t, http.MethodGet, PathPrefix+"usage/summary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("summary 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body usageSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Totals.Cost != "12.34000000" || body.Totals.Requests != 183 {
		t.Errorf("totals = %+v", body.Totals)
	}
	if body.Totals.Tokens.Input != 120340 || body.Totals.Tokens.Output != 88120 ||
		body.Totals.Tokens.Reasoning != 2100 {
		t.Errorf("tokens = %+v", body.Totals.Tokens)
	}
	if len(body.BillingPaths) != 3 || body.BillingPaths[2].Path != "fallback" {
		t.Errorf("billing_paths = %+v", body.BillingPaths)
	}
	// 时间一律 UTC：站点时区只用来解释 YYYY-MM-DD 的输入。
	if !strings.HasSuffix(body.From, "Z") || !strings.HasSuffix(body.To, "Z") {
		t.Errorf("from/to 不是 UTC: %q / %q", body.From, body.To)
	}

	// 序列的桶起点同样是 UTC；tz 字段说明按哪个时区切的天。
	rec = f.authed(t, http.MethodGet, PathPrefix+"usage/series", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("series 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var series usageSeriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &series); err != nil {
		t.Fatalf("序列响应不是 JSON: %v", err)
	}
	if len(series.Points) == 0 || !strings.HasSuffix(series.Points[0].Start, "Z") {
		t.Errorf("桶起点不是 UTC: %+v", series.Points)
	}
	if series.TZ != "Asia/Shanghai" {
		t.Errorf("tz = %q，期望站点时区", series.TZ)
	}

	// 非法时间窗：400。
	if rec := f.authed(t, http.MethodGet, PathPrefix+"usage/summary?from=2026-10-20&to=2026-10-01", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("from 晚于 to 应当 400，得到 %d", rec.Code)
	}
	// 超过 366 天：400。
	if rec := f.authed(t, http.MethodGet, PathPrefix+"usage/summary?from=2020-01-01&to=2026-01-02", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("超长窗口应当 400，得到 %d", rec.Code)
	}

	// 分组：group_by 必填；store 的 ErrInvalidQuery 归 400。
	if rec := f.authed(t, http.MethodGet, PathPrefix+"usage/breakdown", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 group_by 应当 400，得到 %d", rec.Code)
	}
	if rec := f.authed(t, http.MethodGet, PathPrefix+"usage/breakdown?group_by=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("store 判参数错时应当 400，得到 %d", rec.Code)
	}
}

func TestUsageRequestCursor(t *testing.T) {
	f := newFixture(t)
	f.usage.requests = store.UsageRequestPage{
		Requests: []store.UsageRequestRow{{
			ID: 9001, RequestID: "req_01", Model: "claude-sonnet-4-5",
			KeyID: ptrInt64(42), KeyName: "Production", Status: "settled",
			Cost: mustAmount(t, "0.04200000"), Asset: "USD",
			Streamed: true, Protocol: "openai",
			PromptTokens: 1200, CompletionTokens: 300,
			BillingPath: ptrString("measured"), StartedAt: testNow.Add(-time.Minute),
		}},
		More: true,
	}
	rec := f.authed(t, http.MethodGet, PathPrefix+"usage/requests", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("requests 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body usageRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items 长度 = %d", len(body.Items))
	}
	item := body.Items[0]
	if item.Cost != "0.04200000" || item.Currency != "USD" ||
		item.Key == nil || item.Key.ID != 42 || item.Key.Name != "Production" {
		t.Errorf("item = %+v", item)
	}
	if item.Tokens.Input != 1200 || item.Tokens.Output != 300 {
		t.Errorf("tokens = %+v", item.Tokens)
	}
	if body.NextCursor == nil {
		t.Error("More=true 时应当给 next_cursor")
	}
}

func TestWalletZeroWithoutRowAndRejectsBadAmounts(t *testing.T) {
	f := newFixture(t)

	// 没有钱包行：200 + 两个 0，不是 500。GET 不建行，updated_at 留空。
	rec := f.authed(t, http.MethodGet, PathPrefix+"wallet", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("没有钱包行应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var empty walletJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if empty.Asset != "CNY" || empty.Balance != "0.00000000" ||
		empty.Frozen != "0.00000000" || empty.UpdatedAt != "" {
		t.Errorf("没有钱包行时的响应 = %+v", empty)
	}

	f.usage.wallets = []store.Wallet{{
		UserID: 7, Asset: "CNY",
		Balance: mustAmount(t, "88.42000000"), Frozen: mustAmount(t, "0.34000000"),
		UpdatedAt: testNow,
	}}
	rec = f.authed(t, http.MethodGet, PathPrefix+"wallet", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("钱包应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body walletJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Balance != "88.42000000" || body.Frozen != "0.34000000" {
		t.Errorf("钱包 = %+v", body)
	}

	// 金额解析失败：500，不回 0。
	f.usage.wallets[0].BalanceErr = errors.New("numeric out of range")
	if rec := f.authed(t, http.MethodGet, PathPrefix+"wallet", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("金额解析失败应当 500，得到 %d", rec.Code)
	}
}

func TestModelsPricingAndETag(t *testing.T) {
	f := newFixture(t)
	f.models.names = []string{"claude-sonnet-4-5", "no-price"}
	f.catalog.models = []catalog.Model{{
		CanonicalName: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5",
		Vendor: "Anthropic", ContextLimit: 200000, OutputLimit: 64000,
		ModalitiesInput: []string{"text", "image"}, ModalitiesOutput: []string{"text"},
		Capabilities: catalog.Capabilities{Tools: ptrBool(true), Vision: ptrBool(false)},
	}, {
		CanonicalName: "no-price",
	}}
	price := &pricing.ReferencePrice{
		InputPerMTok:  mustAmount(t, "12.00000000"),
		OutputPerMTok: mustAmount(t, "60.00000000"),
	}
	f.prices.prices["claude-sonnet-4-5"] = price

	rec := f.request(t, http.MethodGet, PathPrefix+"models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("models 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("models 缺少 ETag")
	}

	var body modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items 长度 = %d，期望 2", len(body.Items))
	}
	first := body.Items[0]
	if first.Pricing == nil || first.Pricing.InputPerMTok != "12.00000000" {
		t.Errorf("第一个模型的价格 = %+v", first.Pricing)
	}
	if first.Capabilities["vision"] != false {
		t.Errorf("vision=false 应当出现，得到 %+v", first.Capabilities)
	}
	if _, ok := first.Capabilities["audio"]; ok {
		t.Errorf("未采集的能力不该出现: %+v", first.Capabilities)
	}
	// 没有平台规则：pricing 为 null，不伪造一个数。
	if body.Items[1].Pricing != nil {
		t.Errorf("没有平台规则时 pricing 应当为 null，得到 %+v", body.Items[1].Pricing)
	}

	// 带上同一个 ETag：304，不重发正文。
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, PathPrefix+"models", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	f.handler.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("同一个 ETag 应当 304，得到 %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 不应带正文，得到 %q", second.Body.String())
	}

	// reference_at 每次请求都不同，但它不进 ETag：时间走一秒，同一个
	// If-None-Match 仍然必须命中 304。
	*f.clock = testNow.Add(time.Second)
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, PathPrefix+"models", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	req.Header.Set("If-None-Match", etag)
	third := httptest.NewRecorder()
	f.handler.ServeHTTP(third, req)
	if third.Code != http.StatusNotModified {
		t.Fatalf("reference_at 变了不该影响 ETag，得到 %d: %s",
			third.Code, third.Body.String())
	}
}

// 模型页要回答「谁在供、多少钱」：商家名与平台自营（null）都在，
// 按价格升序；单位是每 1000 个输入 token（与 /v1/models 一致）。
func TestModelsOffersShowWhoSells(t *testing.T) {
	f := newFixture(t)
	f.models.names = []string{"alpha"}
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2},
	}
	f.offers.base = mustAmount(t, "0.00400000")
	f.offers.byOwner = map[int64]money.Amount{11: mustAmount(t, "0.00350000")}

	rec := f.request(t, http.MethodGet, PathPrefix+"models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("models 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items 长度 = %d，期望 1", len(body.Items))
	}
	want := []offerJSON{
		{Merchant: ptrString("acme"), Price: "0.00350000"},
		{Price: "0.00400000"},
	}
	if !reflect.DeepEqual(body.Items[0].Offers, want) {
		t.Errorf("offers = %+v，期望 %+v", body.Items[0].Offers, want)
	}
}

func TestMeta(t *testing.T) {
	f := newFixture(t)
	rec := f.request(t, http.MethodGet, PathPrefix+"meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("meta 应当 200，得到 %d", rec.Code)
	}
	var body metaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.APIBaseURL != "http://example.com/v1" {
		t.Errorf("api_base_url = %q，期望按请求推出 http://example.com/v1", body.APIBaseURL)
	}
	if body.Currency != "CNY" || body.Timezone != "Asia/Shanghai" {
		t.Errorf("meta = %+v", body)
	}
	if body.Features.Registration || body.Features.Topup || body.Features.PasswordReset != "admin" {
		t.Errorf("features = %+v", body.Features)
	}
}

func TestMeUpdateDisplayNameTriState(t *testing.T) {
	f := newFixture(t)

	// null 清空。
	rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"me", `{"display_name":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /me 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.accounts.infos[7].DisplayName; got != "" {
		t.Errorf("display_name = %q，期望清空", got)
	}

	// 空串不是清空：null 才是。
	if rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"me", `{"display_name":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 display_name 应当 400，得到 %d", rec.Code)
	}

	// username 不可改：未知字段直接拒。
	if rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"me", `{"username":"other"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("改 username 应当 400，得到 %d", rec.Code)
	}
}

// ── 下层故障与输入边界 ──────────────────────────────────────────────────

func TestStoreFailuresAreServiceUnavailable(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("connection reset by peer")

	// 改资料：校验类哨兵 400，其余 503。把库故障说成 400 会让用户去改
	// 一个本来没错的字段。
	f.accounts.updateFail = boom
	rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"me", `{"display_name":"X"}`)
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != codeUnavailable {
		t.Fatalf("改资料时库故障应当 503，得到 %d: %s", rec.Code, rec.Body.String())
	}
	f.accounts.updateFail = auth.ErrInvalidEmail
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"me", `{"email":"bad@example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("邮箱校验失败应当 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	f.accounts.updateFail = nil

	// 创建 key：同一套判据。
	f.keys.createFail = boom
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"keys", `{"name":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("创建 key 时库故障应当 503，得到 %d: %s", rec.Code, rec.Body.String())
	}
	f.keys.createFail = textshape.ErrControlChar
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"keys", `{"name":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("备注含控制字符应当 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	f.keys.createFail = nil

	// 改 key：同一套判据。
	f.keys.put(auth.Key{ID: 3, UserID: 7, Status: "active", CreatedAt: testNow})
	f.keys.updateFail = boom
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/3", `{"name":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("改 key 时库故障应当 503，得到 %d: %s", rec.Code, rec.Body.String())
	}
	f.keys.updateFail = textshape.ErrControlChar
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/3", `{"name":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("备注含控制字符应当 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginRejectsCrossSiteOrigin(t *testing.T) {
	f := newFixture(t)
	const body = `{"username":"jdoe","password":"correct horse battery staple"}`

	// 跨站表单：403，且不建会话——它会把受害者登进攻击者的账号。
	rec := f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Origin": "https://evil.example.com"},
		http.MethodPost, PathPrefix+"auth/login", body)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != codeCSRFFailed {
		t.Fatalf("跨站登录应当 403 csrf_failed，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if len(f.sessions.created) != 0 {
		t.Fatal("跨站登录仍然建了会话")
	}

	// Sec-Fetch-Site: cross-site 同样拒。
	rec = f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Sec-Fetch-Site": "cross-site"},
		http.MethodPost, PathPrefix+"auth/login", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site 登录应当 403，得到 %d", rec.Code)
	}

	// 同源放行。
	rec = f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Origin": "http://example.com"},
		http.MethodPost, PathPrefix+"auth/login", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("同源登录应当放行，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// 两个头都缺省（curl 之类）：放行，它们不是 CSRF 的目标。
	rec = f.request(t, http.MethodPost, PathPrefix+"auth/login", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("不带 Origin 的登录应当放行，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTruncateUserAgentKeepsValidUTF8(t *testing.T) {
	// 200 个三字节字符 = 600 字节：按字节切前 512 个会剩下半截字符。
	ua := strings.Repeat("あ", 200)
	got := truncateUserAgent(ua)
	if len(got) > 512 {
		t.Errorf("截断后 %d 字节，超过 512", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if !strings.HasPrefix(ua, got) || len(got) != 510 {
		t.Errorf("截断结果 %d 字节，期望 510", len(got))
	}

	// 短于上限的原样返回。
	if short := truncateUserAgent("curl/8.0"); short != "curl/8.0" {
		t.Errorf("短 UA 被改了: %q", short)
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	f := newFixture(t)
	huge := `{"username":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	rec := f.request(t, http.MethodPost, PathPrefix+"auth/login", huge)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != codePayloadTooLarge {
		t.Fatalf("超限请求体应当 413 payload_too_large，得到 %d: %s",
			rec.Code, rec.Body.String())
	}
}

func TestPatchKeyExpiresAtMustBeFuture(t *testing.T) {
	f := newFixture(t)
	f.keys.put(auth.Key{ID: 5, UserID: 7, Status: "active", CreatedAt: testNow})

	past := testNow.Add(-time.Minute).Format(time.RFC3339)
	rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/5", `{"expires_at":"`+past+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("过去的 expires_at 应当 400，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// null 是「清空、不过期」，仍然放行。
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPatch,
		PathPrefix+"keys/5", `{"expires_at":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 expires_at 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWalletEntriesCarryAsset(t *testing.T) {
	f := newFixture(t)
	f.usage.entries = store.UserEntryPage{Entries: []store.UserEntry{{
		ID: 11, CreatedAt: testNow, Type: "consume", Asset: "CNY",
		Amount: mustAmount(t, "-0.50000000"), Memo: "gpt-4o",
	}}}

	rec := f.authed(t, http.MethodGet, PathPrefix+"wallet/entries", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wallet/entries 应当 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body entriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items 长度 = %d", len(body.Items))
	}
	item := body.Items[0]
	if item.Asset != "CNY" || item.Amount != "-0.50000000" || item.Type != "consume" {
		t.Errorf("item = %+v", item)
	}
	if !strings.HasSuffix(item.CreatedAt, "Z") {
		t.Errorf("created_at 不是 UTC: %q", item.CreatedAt)
	}
}

func TestChangePassword(t *testing.T) {
	f := newFixture(t)
	const current = "correct horse battery staple"
	const updated = "a whole new passphrase 9"
	body := func(old, new string) string {
		return `{"current_password":"` + old + `","new_password":"` + new + `"}`
	}

	// 成功：204，库里换成新口令的哈希（当前会话保留）。
	rec := f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"me/password", body(current, updated))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("改口令应当 204，得到 %d: %s", rec.Code, rec.Body.String())
	}
	hash := f.accounts.passwords[7]
	if hash == "" {
		t.Fatal("改口令之后库里没有新哈希")
	}
	result, err := auth.VerifyPassword(testKDF, hash, updated)
	if err != nil || !result.OK {
		t.Fatalf("新口令验证失败: %v / %+v", err, result)
	}

	// 新口令与旧口令相同：409，不是「弱口令」。
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"me/password", body(current, current))
	if rec.Code != http.StatusConflict {
		t.Fatalf("新旧口令相同应当 409，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// 账号还没设过口令：403 account_not_ready，而不是 401。
	stored := f.accounts.byName["jdoe"]
	stored.PasswordHash = ""
	f.accounts.byName["jdoe"] = stored
	rec = f.authedWith(t, "session-token", writeHeaders(), http.MethodPost,
		PathPrefix+"me/password", body(current, updated))
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != codeAccountNotReady {
		t.Fatalf("未激活账号应当 403 account_not_ready，得到 %d: %s",
			rec.Code, rec.Body.String())
	}

	// 明文 HTTP（公网对端）：400 insecure_transport，口令不进 KDF。
	g := newFixture(t)
	rec = g.authedAsWith(t, "203.0.113.9:5555", "session-token", writeHeaders(),
		http.MethodPost, PathPrefix+"me/password", body(current, updated))
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeInsecureTransport {
		t.Fatalf("明文传输的改口令应当 400 insecure_transport，得到 %d: %s",
			rec.Code, rec.Body.String())
	}
	if _, ok := g.accounts.passwords[7]; ok {
		t.Fatal("明文传输的改口令仍然写了新哈希")
	}
}

func TestLoginRehashesOldParameters(t *testing.T) {
	f := newFixture(t)
	const password = "correct horse battery staple"

	// 库里那一条是按 500 轮算的；当前参数是 testKDF（1000 轮）。
	oldHash, err := auth.HashPassword(
		auth.KDFParams{Iterations: 500, SaltBytes: 16, KeyBytes: 32}, password)
	if err != nil {
		t.Fatalf("算旧哈希失败: %v", err)
	}
	stored := f.accounts.byName["jdoe"]
	stored.PasswordHash = oldHash
	f.accounts.byName["jdoe"] = stored

	rec := f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"jdoe","password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("旧参数的口令应当能登录，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// 登录成功后按当前参数重算一次并写回，同一句口令在新哈希下仍然成立。
	if f.accounts.rehashed != 1 {
		t.Fatalf("重哈希次数 = %d，期望 1", f.accounts.rehashed)
	}
	newHash := f.accounts.passwords[7]
	if newHash == "" || newHash == oldHash {
		t.Fatalf("没有写回新哈希（%q）", newHash)
	}
	result, err := auth.VerifyPassword(testKDF, newHash, password)
	if err != nil || !result.OK {
		t.Fatalf("新哈希验证失败: %v / %+v", err, result)
	}
	if result.NeedsRehash {
		t.Error("重算之后仍然是旧参数")
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func parseID(ref string) (int64, error) {
	var id int64
	for _, r := range ref {
		if r < '0' || r > '9' {
			return 0, errors.New("不是数字")
		}
		id = id*10 + int64(r-'0')
	}
	if ref == "" {
		return 0, errors.New("空串")
	}
	return id, nil
}

func sortedIDs(keys map[int64]auth.Key) []int64 {
	ids := make([]int64, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] > ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

func ptrAmount(t *testing.T, raw string) *money.Amount {
	t.Helper()
	amount := mustAmount(t, raw)
	return &amount
}

func mustAmount(t *testing.T, raw string) money.Amount {
	t.Helper()
	amount, err := money.Parse(raw)
	if err != nil {
		t.Fatalf("金额 %q 解析失败: %v", raw, err)
	}
	return amount
}

func ptrInt(v int) *int { return &v }

func ptrInt64(v int64) *int64 { return &v }

func ptrBool(v bool) *bool { return &v }

func ptrString(v string) *string { return &v }
