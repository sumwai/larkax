package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/observe"
)

// 端到端：一次 `/v1/systemone` 请求在 request_snapshots 里留下**两行判据**，
// 且它们与账单挂在同一个 request_id 上。
//
// # 它为什么必须在装配层测
//
// 网关那几条用例验的是「交给脱敏器的是什么」，snapshotwire 那几条验的是
// 「脱敏器交回来的骨架对不对」。只有这一条把两段接起来走**生产用的那条装配
// 路径**（assembly.Build → api → gateway → snapshotwire → 真库），于是下面
// 三件事才有判据：
//
//   - 装配层真的把决策脱敏器接上了（少接一个字段的表现是「这条路不采快照」，
//     不报错、不影响计费，安静得像一切正常）；
//   - 落库那一列 request_id 与计费那一行是**同一个值**——它由观测层给出，
//     两条链路各取一次，取岔了不会报错，只是永远关联不上；
//   - 内容那一侧的原文在库里任何一列都找不到，而这要看的是**真的那份请求体**
//     （state 与 questions 都在里面），不是内存里的一个结构体。
//
// # 为什么用响应头里的 request_id
//
// 客户端不带 x-request-id 时由服务端生成，而响应头里回的就是它（见
// internal/observe 的 Middleware）。拿它去查库，查到的就是这次请求。
func TestSystemOneEndToEndLeavesDecisionSnapshots(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-decision-snap")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &systemoneUpstream{
		db: db, userID: user, model: model,
		inputTokens: 387, outputTokens: 72,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "systemone", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 请求体里带上三个标记：工单原文、要模型回答的问题、以及一段像凭证的串。
	// 前两个是这条表面真正的内容（state / questions），第三个是候选渠道身上
	// 那类不该进快照的东西。
	const (
		stateMarker = "工单正文标记-snapshot-e2e-付款后没到账"
		question    = "问题指令标记-snapshot-e2e-这是账单问题吗"
	)
	body := decisionBodyWithMarkers(model, stateMarker, question)

	status, _, headers := postChatFull(ctx, t, public.URL+"/v1/systemone", key, body)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	requestID := headers.Get(observe.HeaderRequestID)
	if requestID == "" {
		t.Fatal("响应头里没有请求标识——拿不到它，下面就没有可与账单对齐的键")
	}

	// 一、两行都落了，方向各一个，且都挂在这个 request_id 下。
	rows := waitForSnapshotsOf(t, db, requestID, 2)
	byDirection := map[string]snapshotRowView{}
	for _, r := range rows {
		byDirection[r.direction] = r
	}
	reqRow, ok := byDirection["request"]
	if !ok {
		t.Fatalf("没有请求方向的行：%+v", rows)
	}
	respRow, ok := byDirection["response"]
	if !ok {
		t.Fatalf("没有响应方向的行：%+v", rows)
	}

	// 二、request_id 与计费那一行是同一个值。
	//
	// 这是这份快照唯一的用途：拿着账单上的 request_id 要能查到这两行。
	// 两条链路各取一次标识，取岔了不会报错，只是永远关联不上。
	var billed int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_requests WHERE request_id = $1`, requestID).Scan(&billed); err != nil {
		t.Fatalf("查账单行失败: %v", err)
	}
	if billed != 1 {
		t.Fatalf("按快照的 request_id 查到 %d 条账单行，期望 1——两边的标识不是同一个", billed)
	}

	// 三、哪条表面由骨架里的 surface 认出来（direction 只有 request / response）。
	for dir, row := range byDirection {
		if got := row.skeleton["surface"]; got != "decision" {
			t.Errorf("方向 %s 的 surface = %v，期望 decision", dir, got)
		}
		if row.userID != user {
			t.Errorf("方向 %s 的 user_id = %d，期望 %d（发起这次请求的用户）", dir, row.userID, user)
		}
	}

	// 四、请求方向记判据（候选集合），响应方向记「选了谁」。
	if got := reqRow.skeleton["candidates_count"]; got == nil {
		t.Error("请求方向没有候选数——判据的主体就没了")
	}
	if _, present := reqRow.skeleton["selected"]; present {
		t.Error("请求方向不该有 selected：选路之前还没有选中项")
	}
	selected, ok := respRow.skeleton["selected"].(map[string]any)
	if !ok {
		t.Fatalf("响应方向没有 selected 段：%#v", respRow.skeleton)
	}
	if selected["channel_id"] == nil {
		t.Error("选中项里没有渠道 id：擦不到与账单对上的那个数")
	}
	if selected["upstream_model"] != model {
		t.Errorf("上游模型名 = %v，期望 %q", selected["upstream_model"], model)
	}
	// 每一次尝试都要在，含成功的那一次。
	attempts, ok := respRow.skeleton["attempts"].([]any)
	if !ok || len(attempts) != 1 {
		t.Fatalf("尝试记录 = %#v，期望一条", respRow.skeleton["attempts"])
	}
	if attempts[0].(map[string]any)["succeeded"] != true {
		t.Errorf("那一次尝试成功了，该记成 succeeded: %#v", attempts[0])
	}

	// 五、内容一个都不能出现在库里。
	//
	// 走 to_jsonb(s) 把**整行**倒出来，而不是逐列列举：新增一列时这条判据
	// 自动跟上，而逐列列举的写法在加列之后会静默漏掉新那一列。
	dump := dumpSnapshotsOf(t, db, requestID)
	for _, marker := range []string{stateMarker, question} {
		if strings.Contains(dump, marker) {
			t.Errorf("库里出现了请求内容 %q：%s", marker, dump)
		}
	}
	if strings.Contains(dump, "placeholder-channel-credential") {
		t.Errorf("库里出现了渠道凭证：%s", dump)
	}
}

// decisionBodyWithMarkers 组装一份带标记的决策请求体。
//
// 形状与 decisionBody 相同（三个问题、三种类型），只是把 state 与问题的
// instructions 换成能一眼认出来的标记：这条用例要断言的是它们**不在**库里，
// 所以标记必须足够独特，不会与任何别的字符串撞上。
func decisionBodyWithMarkers(model, state, instructions string) []byte {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"state": state,
		"questions": map[string]any{
			"q_noul": map[string]any{
				"type": "noul", "instructions": instructions,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// snapshotRowView 是从库里读回来的一行快照。
type snapshotRowView struct {
	direction string
	userID    int64
	skeleton  map[string]any
}

// waitForSnapshotsOf 等到这个 request_id 下的快照达到 want 行。
//
// 落库是异步的（见 gateway.snapshotAsync），所以「一行都没等到」与「还没写完」
// 必须分开——直接查一次就断言的话，失败信息会指向快照本身而不是时序
// （见 e2e_test.go 里 waitForSnapshot 的同一段说明）。
func waitForSnapshotsOf(t *testing.T, db *sql.DB, requestID string, want int) []snapshotRowView {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var last []snapshotRowView
	for time.Now().Before(deadline) {
		last = snapshotsOf(t, db, requestID)
		if len(last) >= want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等到超时：request_id = %s 下只落了 %d 行，期望 %d 行", requestID, len(last), want)
	return nil
}

// snapshotsOf 按 request_id 读出快照行。
func snapshotsOf(t *testing.T, db *sql.DB, requestID string) []snapshotRowView {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT direction, user_id, skeleton::text
		  FROM request_snapshots
		 WHERE request_id = $1
		 ORDER BY direction`, requestID)
	if err != nil {
		t.Fatalf("查询快照失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshotRowView
	for rows.Next() {
		var (
			r        snapshotRowView
			skeleton string
		)
		if err := rows.Scan(&r.direction, &r.userID, &skeleton); err != nil {
			t.Fatalf("扫描快照行失败: %v", err)
		}
		if err := json.Unmarshal([]byte(skeleton), &r.skeleton); err != nil {
			t.Fatalf("骨架不是 JSON 对象: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历快照失败: %v", err)
	}
	return out
}

// dumpSnapshotsOf 把这几行的**全部列**倒成一段文本，供内容扫描使用。
func dumpSnapshotsOf(t *testing.T, db *sql.DB, requestID string) string {
	t.Helper()

	var dumped string
	err := db.QueryRowContext(context.Background(),
		`SELECT coalesce(string_agg(to_jsonb(s)::text, E'\n'), '')
		   FROM request_snapshots s WHERE request_id = $1`,
		requestID).Scan(&dumped)
	if err != nil {
		t.Fatalf("倒出快照内容失败: %v", err)
	}
	if strings.TrimSpace(dumped) == "" {
		t.Fatal("一行快照都没倒出来——这一步没跑成，不是「内容没出现」")
	}
	return dumped
}
