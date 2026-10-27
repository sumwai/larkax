package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/channels"
)

// fakeModelList 是一条假上游的模型列表端点。
//
// 用例要断言的是「命令把上游给的这一份清单原样落进了哪张表」，所以上游那一侧
// 必须是可控的：换清单、回错误、记下收到的凭证，都在这里做。域名与模型名一律
// 示例值（见 AGENTS.md 的红线）。
type fakeModelList struct {
	mu     sync.Mutex
	models []string
	status int
	auth   string
}

func (f *fakeModelList) set(models ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append([]string(nil), models...)
}

func (f *fakeModelList) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeModelList) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth
}

// handler 只认 /v1/models：openai 适配器打的就是这条路径。别的路径回 404，
// 拼错端点时用例会立刻红，而不是拿到一份空清单继续往下走。
func (f *fakeModelList) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = r.Header.Get("Authorization")
		status := f.status
		models := append([]string(nil), f.models...)
		f.mu.Unlock()

		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream failed"}}`))
			return
		}

		data := make([]map[string]string, 0, len(models))
		for _, model := range models {
			data = append(data, map[string]string{"id": model})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
}

// createModelSyncChannel 建一条指向假上游的渠道，返回渠道 ID 与它的规范模型白名单。
//
// 白名单这一项会单独断言「同步没有改写它」——上游客商模型名与选路用的规范名
// 是两份东西，混写会让一批调不动的名字变成「声称支持」。
func createModelSyncChannel(t *testing.T, baseURL, chanType string) (int64, string) {
	t.Helper()
	canonical := uniqueName("canonical")
	withStdin(t, testSecret)
	out, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", chanType,
			"--base-url", baseURL, "--models", canonical, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", out, err)
	}
	return id, canonical
}

// storedUpstreamModels 读回库里记着的那份清单，按模型名排序。
func storedUpstreamModels(t *testing.T, db *sql.DB, channelID int64) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT upstream_model FROM channel_upstream_models WHERE channel_id = $1 ORDER BY upstream_model`,
		channelID)
	if err != nil {
		t.Fatalf("读上游模型清单失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			t.Fatalf("读上游模型清单失败: %v", err)
		}
		out = append(out, model)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读上游模型清单失败: %v", err)
	}
	return out
}

// runSyncModels 跑一次同步并返回它的回显。
func runSyncModels(t *testing.T, id int64) string {
	t.Helper()
	idArg := strconv.FormatInt(id, 10)
	_, note := captureStreams(t, func() {
		if err := runChannelSyncModels([]string{"--id", idArg}); err != nil {
			t.Fatalf("channel sync-models 失败: %v", err)
		}
	})
	return note
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestChannelSyncModelsStoresUpstreamList 钉住一次同步的完整结果：
// 清单落库、同步时刻落库、凭证真的解出来发给了上游、选路白名单没被改写。
func TestChannelSyncModelsStoresUpstreamList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-b", "example-model-a")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, canonical := createModelSyncChannel(t, srv.URL, testChanType)

	note := runSyncModels(t, id)
	for _, want := range []string{"已同步", "共 2 个模型", "新增 2", "下线 0"} {
		if !strings.Contains(note, want) {
			t.Errorf("同步回显里应当出现 %q，实际为 %q", want, note)
		}
	}

	if got, want := storedUpstreamModels(t, db, id), []string{"example-model-a", "example-model-b"}; !equalStrings(got, want) {
		t.Errorf("落库的清单 = %v，期望 %v", got, want)
	}

	var (
		status   string
		syncedAt sql.NullTime
	)
	if err := db.QueryRowContext(t.Context(),
		`SELECT status, synced_at FROM channel_model_syncs WHERE channel_id = $1`, id).
		Scan(&status, &syncedAt); err != nil {
		t.Fatalf("读同步状态失败: %v", err)
	}
	if status != channels.ModelSyncSynced {
		t.Errorf("同步状态 = %q，期望 %q", status, channels.ModelSyncSynced)
	}
	if !syncedAt.Valid {
		t.Error("同步成功时 synced_at 应当有值")
	}

	// 凭证解出来了才会带上这个头。密文没解开时上游拿到的是空凭证，这一条会红。
	if got, want := upstream.lastAuth(), "Bearer "+testSecret; got != want {
		t.Errorf("上游收到的 Authorization = %q，期望 %q", got, want)
	}

	// channels.models 是选路白名单（规范名），同步不该碰它。
	// 用 array_to_string 读回：database/sql 这一层没有 text[] 的通用 Scan。
	var models string
	if err := db.QueryRowContext(t.Context(),
		`SELECT array_to_string(models, ',') FROM channels WHERE id = $1`, id).Scan(&models); err != nil {
		t.Fatalf("读渠道白名单失败: %v", err)
	}
	if models != canonical {
		t.Errorf("channels.models = %q，期望仍是 %q", models, canonical)
	}
}

// TestChannelSyncModelsFollowsUpstreamChanges 钉住「上游增删一个模型，再同步
// 库里跟着变」——这是这份清单有意义的唯一理由。
func TestChannelSyncModelsFollowsUpstreamChanges(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-a", "example-model-b")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	runSyncModels(t, id)

	upstream.set("example-model-b", "example-model-c")
	note := runSyncModels(t, id)
	for _, want := range []string{"新增 1", "下线 1", "共 2 个模型"} {
		if !strings.Contains(note, want) {
			t.Errorf("第二次同步的回显里应当出现 %q，实际为 %q", want, note)
		}
	}

	if got, want := storedUpstreamModels(t, db, id), []string{"example-model-b", "example-model-c"}; !equalStrings(got, want) {
		t.Errorf("第二次同步后的清单 = %v，期望 %v", got, want)
	}
}

// TestChannelSyncModelsIsIdempotent 钉住「同一份清单连续同步两次不多出行」。
//
// 行按 (channel_id, upstream_model) 唯一；第二次只该刷新 last_seen_at，
// first_seen_at 保留第一次见到的时刻。
func TestChannelSyncModelsIsIdempotent(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-a", "example-model-b")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	runSyncModels(t, id)

	var firstSeen sql.NullTime
	if err := db.QueryRowContext(t.Context(),
		`SELECT first_seen_at FROM channel_upstream_models WHERE channel_id = $1 AND upstream_model = 'example-model-a'`,
		id).Scan(&firstSeen); err != nil {
		t.Fatalf("读 first_seen_at 失败: %v", err)
	}

	note := runSyncModels(t, id)
	for _, want := range []string{"新增 0", "下线 0", "共 2 个模型"} {
		if !strings.Contains(note, want) {
			t.Errorf("第二次同步的回显里应当出现 %q，实际为 %q", want, note)
		}
	}

	var count int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM channel_upstream_models WHERE channel_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("数清单行数失败: %v", err)
	}
	if count != 2 {
		t.Errorf("同一份清单同步两次后 %d 行，期望 2 行（唯一键没起作用）", count)
	}

	var firstSeenAgain sql.NullTime
	if err := db.QueryRowContext(t.Context(),
		`SELECT first_seen_at FROM channel_upstream_models WHERE channel_id = $1 AND upstream_model = 'example-model-a'`,
		id).Scan(&firstSeenAgain); err != nil {
		t.Fatalf("读 first_seen_at 失败: %v", err)
	}
	if !firstSeenAgain.Time.Equal(firstSeen.Time) {
		t.Errorf("first_seen_at 从 %v 变成了 %v：第二次同步不该改写它", firstSeen.Time, firstSeenAgain.Time)
	}
}

// TestChannelSyncModelsUnsupportedKeepsExistingList 钉住那条最容易被写错的
// 分支：上游说「没有这个接口」时，命令成功、状态记成 unsupported、已有清单
// 一行都不能少。
//
// 用「同步成功过，再把渠道类型换成不支持清单的 systemone」造出这个状态：
// 先有一份真实的清单，才证明得了「没有被误删」。
func TestChannelSyncModelsUnsupportedKeepsExistingList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-a", "example-model-b")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	runSyncModels(t, id)

	// systemone 适配器按设计不提供模型列表（见 systemoneadapter.ListModels）。
	if _, err := db.ExecContext(t.Context(), `UPDATE channels SET type = 'systemone' WHERE id = $1`, id); err != nil {
		t.Fatalf("改渠道类型失败: %v", err)
	}

	note := runSyncModels(t, id)
	for _, want := range []string{"不提供模型列表接口", "已有清单没有改动", "共 2 个模型"} {
		if !strings.Contains(note, want) {
			t.Errorf("不支持时的回显里应当出现 %q，实际为 %q", want, note)
		}
	}

	if got, want := storedUpstreamModels(t, db, id), []string{"example-model-a", "example-model-b"}; !equalStrings(got, want) {
		t.Errorf("「不支持」之后清单 = %v，期望原样保留 %v", got, want)
	}

	var (
		status   string
		syncedAt sql.NullTime
	)
	if err := db.QueryRowContext(t.Context(),
		`SELECT status, synced_at FROM channel_model_syncs WHERE channel_id = $1`, id).
		Scan(&status, &syncedAt); err != nil {
		t.Fatalf("读同步状态失败: %v", err)
	}
	if status != channels.ModelSyncUnsupported {
		t.Errorf("同步状态 = %q，期望 %q", status, channels.ModelSyncUnsupported)
	}
	if !syncedAt.Valid {
		t.Error("之前成功同步过，synced_at 应当留着那个时刻")
	}
}

// TestChannelUpstreamModelsReadsStoredList 钉住读侧：它只读库、不碰上游，
// 并把「上游模型名不参与选路」这句话说在输出里。
func TestChannelUpstreamModelsReadsStoredList(t *testing.T) {
	testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-a", "example-model-b")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	runSyncModels(t, id)

	idArg := strconv.FormatInt(id, 10)
	out, _ := captureStreams(t, func() {
		if err := runChannelUpstreamModels([]string{"--id", idArg}); err != nil {
			t.Fatalf("channel upstream-models 失败: %v", err)
		}
	})
	for _, want := range []string{
		"已同步", "example-model-a", "example-model-b",
		"首次见到", "最近见到", "不参与选路",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("读侧输出里应当出现 %q，实际为 %q", want, out)
		}
	}
}

// TestChannelUpstreamModelsNeverSynced 钉住「从未同步过」这一档：它不是错误，
// 输出要说清下一步敲什么。
func TestChannelUpstreamModelsNeverSynced(t *testing.T) {
	testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	idArg := strconv.FormatInt(id, 10)
	out, _ := captureStreams(t, func() {
		if err := runChannelUpstreamModels([]string{"--id", idArg}); err != nil {
			t.Fatalf("channel upstream-models 失败: %v", err)
		}
	})
	for _, want := range []string{"未同步", "channel sync-models"} {
		if !strings.Contains(out, want) {
			t.Errorf("从未同步时的输出里应当出现 %q，实际为 %q", want, out)
		}
	}
}

// TestChannelUpstreamModelsJSON 钉住机器可读那一档的字段：状态、时刻与清单。
func TestChannelUpstreamModelsJSON(t *testing.T) {
	testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-a")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	runSyncModels(t, id)

	// 渠道建好之后再切到 json 档：channel create 是写类命令，本来就拒绝
	// --format json，而那个拒绝与这里要测的读侧无关。
	withFormat(t, cli.FormatJSON)

	idArg := strconv.FormatInt(id, 10)
	out, _ := captureStreams(t, func() {
		if err := runChannelUpstreamModels([]string{"--id", idArg}); err != nil {
			t.Fatalf("channel upstream-models --format json 失败: %v", err)
		}
	})

	var doc struct {
		Channel struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"channel"`
		Sync struct {
			Status      string  `json:"status"`
			AttemptedAt *string `json:"attempted_at"`
			SyncedAt    *string `json:"synced_at"`
		} `json:"sync"`
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Models []struct {
			UpstreamModel string `json:"upstream_model"`
			FirstSeenAt   string `json:"first_seen_at"`
			LastSeenAt    string `json:"last_seen_at"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	if doc.Channel.ID != id {
		t.Errorf("channel.id = %d，期望 %d", doc.Channel.ID, id)
	}
	if doc.Sync.Status != channels.ModelSyncSynced {
		t.Errorf("sync.status = %q，期望 %q", doc.Sync.Status, channels.ModelSyncSynced)
	}
	if doc.Sync.AttemptedAt == nil || doc.Sync.SyncedAt == nil {
		t.Error("同步成功之后 attempted_at 与 synced_at 都不该是 null")
	}
	if doc.Summary.Total != 1 || len(doc.Models) != 1 || doc.Models[0].UpstreamModel != "example-model-a" {
		t.Errorf("清单那一段不对：total=%d models=%+v", doc.Summary.Total, doc.Models)
	}
}

// TestChannelSyncModelsUpstreamErrorKeepsExistingList 钉住「上游这次失败」与
// 「上游不支持」的区别：前者要让命令失败，而且库里一条都不改。
func TestChannelSyncModelsUpstreamErrorKeepsExistingList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	upstream.set("example-model-a")
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	runSyncModels(t, id)

	upstream.fail(http.StatusInternalServerError)
	idArg := strconv.FormatInt(id, 10)
	err := runCaptured(t, func() error { return runChannelSyncModels([]string{"--id", idArg}) })
	if err == nil {
		t.Fatal("上游 500 时应当失败")
	}
	if cli.ExitCode(err) != cli.ExitFailure {
		t.Errorf("上游故障的退出码应当是 %d，得到 %d", cli.ExitFailure, cli.ExitCode(err))
	}
	if got, want := storedUpstreamModels(t, db, id), []string{"example-model-a"}; !equalStrings(got, want) {
		t.Errorf("上游失败之后的清单 = %v，期望原样保留 %v", got, want)
	}
}

// TestChannelSyncModelsRejectsBadFlags 走不碰数据库的那几条路径。
func TestChannelSyncModelsRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --id", nil, "缺少 --id"},
		{"负数 --id", []string{"--id", "-1"}, "从 1 开始"},
		{"多出位置参数", []string{"--id", "1", "extra"}, "不接受位置参数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runChannelSyncModels(tc.args) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望报错且含 %q，得到 %v", tc.want, err)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestChannelSyncModelsUnknownChannel 钉住「渠道不存在」走约束冲突那一档。
func TestChannelSyncModelsUnknownChannel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error { return runChannelSyncModels([]string{"--id", "9223372036854775807"}) })
	if err == nil {
		t.Fatal("不存在的渠道应当报错")
	}
	if !errors.Is(err, channels.ErrChannelNotFound) {
		t.Errorf("错误链里应当有 ErrChannelNotFound：%v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("渠道不存在的退出码应当是 %d，得到 %d", cli.ExitConflict, got)
	}
}

// TestChannelSyncModelsDeletedChannel 钉住软删的渠道不接受同步（与 update /
// enable / disable 同一条口径）。
func TestChannelSyncModelsDeletedChannel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	upstream := &fakeModelList{}
	srv := httptest.NewServer(upstream.handler())
	defer srv.Close()

	id, _ := createModelSyncChannel(t, srv.URL, testChanType)
	idArg := strconv.FormatInt(id, 10)
	_, _ = captureStreams(t, func() {
		if err := runChannelDelete([]string{"--id", idArg}); err != nil {
			t.Fatalf("channel delete 失败: %v", err)
		}
	})

	err := runCaptured(t, func() error { return runChannelSyncModels([]string{"--id", idArg}) })
	if !errors.Is(err, channels.ErrChannelDeleted) {
		t.Fatalf("错误链里应当有 ErrChannelDeleted：%v", err)
	}
}
