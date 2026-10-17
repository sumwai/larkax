package snapshotwire

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
)

// 决策那条表面的快照要真的落进 request_snapshots，两个方向各一行。
//
// # 为什么这一条必须用真库
//
// 上面那几条用例验的是「适配层摊出来的东西对不对」，它们走的是内存里的 map。
// 而这一条要回答的是另外三件事，每一件都只在真库上成立：
//
//   - 两行真的落下来了，且落在**同一个 request_id** 下（判据是库里的那一列，
//     不是内存里的那个结构体字段）；
//   - 方向列（request / response）与骨架里的 surface 标量一起把这一行认出来；
//   - JSON 序列化之后，内容那一侧的标记在**任何一列**里都找不到。
//
// 第三件尤其只能在这里验：骨架是 jsonb，它怎么被序列化是数据库说了算。
//
// # 用它自己的那一天
//
// 与 store_integration_test.go 同理：写入指向一个合成的未来日期，用例自己
// 建、自己删那一天的分区，不碰当天分区（见 LESSONS.md 第 26 条）。
func TestIntegrationDecisionSnapshotsLandBothDirections(t *testing.T) {
	handle := testDB(t)
	collector := newCollector(t, testConfig(), Wrap(handle))
	collector.store.now = func() time.Time { return ownDay }
	ctx := context.Background()

	day := PartitionName(ownDay)
	dropRequestSnapshotPartition(ctx, t, handle, day)
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, day)
	})

	requestID := "wire-decision-" + time.Now().UTC().Format("20060102150405.000000000")
	// 请求标识走观测层塞进 context，与生产那条路同源：网关那一侧自己取不到
	// 它（两个包用了不同的 context 键），落库前由本包补上（见 Save）。
	saveCtx := observe.WithRequestID(ctx, observe.RequestID(requestID))
	const userID = int64(4242)

	status := 503
	kind := "upstream_http_error"
	requestSnap, err := collector.RedactDecisionRequest(&gateway.DecisionTrace{
		Model: "probe-decision-model",
		Candidates: []gateway.DecisionCandidate{
			fullDecisionCandidate(7), fullDecisionCandidate(8),
		},
	})
	if err != nil {
		t.Fatalf("脱敏请求方向失败: %v", err)
	}
	requestSnap.UserID = userID
	requestSnap.Direction = "request"

	responseSnap, err := collector.RedactDecisionResponse(&gateway.DecisionOutcome{
		Model: "probe-decision-model",
		Candidates: []gateway.DecisionCandidate{
			fullDecisionCandidate(7), fullDecisionCandidate(8),
		},
		Succeeded:         true,
		SelectedChannelID: 8,
		UpstreamModel:     "vendor/probe-decision-model",
		Attempts: []gateway.DecisionAttempt{
			{ChannelID: 7, Succeeded: false, Retryable: true, UpstreamStatus: &status, ErrorKind: &kind},
			{ChannelID: 8, Succeeded: true, Duration: 30 * time.Millisecond},
		},
	})
	if err != nil {
		t.Fatalf("脱敏响应方向失败: %v", err)
	}
	responseSnap.UserID = userID
	responseSnap.Direction = "response"
	responseSnap.UpstreamStatus = &status
	responseSnap.ErrorKind = &kind

	for _, snap := range []*gateway.Snapshot{requestSnap, responseSnap} {
		if err := collector.Save(saveCtx, snap); err != nil {
			t.Fatalf("落库失败（方向 %s）: %v", snap.Direction, err)
		}
	}

	rows := loadDecisionRows(ctx, t, handle, requestID)
	if len(rows) != 2 {
		t.Fatalf("落下来 %d 行，期望 2（请求方向与响应方向各一行）：%+v", len(rows), rows)
	}
	byDirection := map[string]decisionRow{}
	for _, r := range rows {
		byDirection[r.direction] = r
	}

	// 两行都挂在同一个 request_id 下，方向与 user_id 各就各位。
	// request_id 是与账单关联的那个键，所以它必须与计费那一侧同源——
	// 都来自观测层（见 Save）。
	for _, dir := range []string{"request", "response"} {
		row, ok := byDirection[dir]
		if !ok {
			t.Fatalf("没有方向为 %s 的行：%+v", dir, rows)
		}
		if row.userID != userID {
			t.Errorf("方向 %s 的 user_id = %d，期望 %d", dir, row.userID, userID)
		}
		if row.version != 1 {
			t.Errorf("方向 %s 的 skeleton_version = %d，期望 1", dir, row.version)
		}
		if got := row.skeleton["surface"]; got != "decision" {
			t.Errorf("方向 %s 的 surface = %v，期望 decision", dir, got)
		}
		// 这一份骨架里每一个键都被允许表认下来了：有一格配不上档位时它会
		// 变成描述符并出现在 redacted_paths 里。
		if len(row.redacted) != 0 {
			t.Errorf("方向 %s 有判据被脱敏了 %v", dir, row.redacted)
		}
	}

	// 请求方向记的是「当时手上有哪些候选」。
	req := byDirection["request"]
	// 从 jsonb 读回来的数字一律是 float64：这一列存的是 JSON，
	// 不是 Go 的 int。
	if got := req.skeleton["candidates_count"]; got != float64(2) {
		t.Errorf("请求方向的候选数 = %#v，期望 2", got)
	}
	if _, ok := req.skeleton["selected"]; ok {
		t.Error("请求方向不该有 selected：那时还没有选中项")
	}
	if _, ok := req.skeleton["attempts"]; ok {
		t.Error("请求方向不该有 attempts：一次尝试都还没发生")
	}

	// 响应方向记的是「选了谁、为什么」。
	resp := byDirection["response"]
	selected, ok := resp.skeleton["selected"].(map[string]any)
	if !ok {
		t.Fatalf("响应方向没有 selected 段：%#v", resp.skeleton)
	}
	if selected["channel_id"] != float64(8) {
		t.Errorf("选中的渠道 = %#v，期望 8", selected["channel_id"])
	}
	if selected["upstream_model"] != "vendor/probe-decision-model" {
		t.Errorf("上游模型名 = %#v", selected["upstream_model"])
	}
	cands, ok := resp.skeleton["candidates"].([]any)
	if !ok || len(cands) != 2 {
		t.Fatalf("响应方向的候选集 = %#v，期望两个", resp.skeleton["candidates"])
	}
	// 「选中」与「淘汰」在同一张清单里：读一行就能回答「为什么是它」。
	if cands[0].(map[string]any)["selected"] != false ||
		cands[1].(map[string]any)["selected"] != true {
		t.Errorf("候选上的 selected 记错了：%#v", cands)
	}
	attempts, ok := resp.skeleton["attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("尝试记录 = %#v，期望两条", resp.skeleton["attempts"])
	}
	first := attempts[0].(map[string]any)
	if first["succeeded"] != false || first["retryable"] != true ||
		first["error_kind"] != "upstream_http_error" ||
		first["upstream_status"] != float64(503) {
		t.Errorf("第一条尝试的降级原因不全: %#v", first)
	}
}

// 内容那一侧的标记在库里**任何一列**都找不到。
//
// 逐列扫而不是只看骨架：判据是「这次请求的任何一种留存形态里都没有那段原文」，
// 而骨架只是其中一列（还有 redacted_paths 这个数组）。
//
// # 它为什么仍然要写
//
// 决策这条表面上的内容不是「被脱敏器挡住了」，是**根本没进这条链路**——
// DecisionTrace / DecisionOutcome 上没有装它的字段，候选也是 DecisionCandidate
// （不含 channel.Channel 的凭证与地址）。所以这条用例今天是恒真的。
//
// 留着它，是因为它挡的是**将来**：有人给那几个类型加一个 Note 字段、
// 或者把候选换回 route.Candidate 的话，标记会立刻出现在库里。而那时如果
// 没有这条用例，「凭证开始进快照」这件事没有任何症状——它不报错、不改变
// 任何一步的返回，也不影响计费。
func TestIntegrationDecisionRowsCarryNoContent(t *testing.T) {
	handle := testDB(t)
	collector := newCollector(t, testConfig(), Wrap(handle))
	collector.store.now = func() time.Time { return ownDay }
	ctx := context.Background()

	day := PartitionName(ownDay)
	dropRequestSnapshotPartition(ctx, t, handle, day)
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, day)
	})

	requestID := "wire-decision-content-" + time.Now().UTC().Format("20060102150405.000000000")
	saveCtx := observe.WithRequestID(ctx, observe.RequestID(requestID))

	// 这三个标记分别代表：请求里的用户原文、要模型回答的问题、渠道凭证。
	// 前两者属于 state / questions，最后一个属于 route.Candidate.Channel。
	const (
		stateMarker = "工单正文：付款之后一直没到账，订单号 placeholder-order-1"
		credMarker  = "sk-placeholder-credential-must-not-leak"
	)
	snapshots := []*gateway.Snapshot{}
	for _, dir := range []string{"request", "response"} {
		s, err := collector.RedactDecisionRequest(&gateway.DecisionTrace{
			Model:      "probe-decision-model",
			Candidates: []gateway.DecisionCandidate{fullDecisionCandidate(7)},
		})
		if err != nil {
			t.Fatalf("脱敏失败: %v", err)
		}
		s.UserID = 1
		s.Direction = dir
		snapshots = append(snapshots, s)
	}
	for _, s := range snapshots {
		if err := collector.Save(saveCtx, s); err != nil {
			t.Fatalf("落库失败: %v", err)
		}
	}

	rows := loadDecisionRows(ctx, t, handle, requestID)
	if len(rows) == 0 {
		t.Fatal("一行都没落下来——这一条就没跑成")
	}
	// 序列化后的整行都看一遍，而不只是骨架那几个键。
	raw := dumpRows(ctx, t, handle, requestID)
	for _, marker := range []string{stateMarker, credMarker, "这是账单问题吗"} {
		// 子串查找而不是 containsString：那一个是**逐层相等**的比较，
		// 用在一整行文本上没有意义。
		if strings.Contains(raw, marker) {
			t.Errorf("库里出现了 %q：%s", marker, raw)
		}
	}
}

// decisionRow 是从库里读回来的一行。
type decisionRow struct {
	direction string
	userID    int64
	version   int
	skeleton  map[string]any
	redacted  []string
}

// loadDecisionRows 按 request_id 读出这个用例写进去的行。
//
// 按 request_id 收窄，而不是数整张表的行数：这张表是全库共享的，
// 别的包在同一台库上并行跑（见 LESSONS.md 第 26 条）。
func loadDecisionRows(ctx context.Context, t *testing.T, db *sql.DB, requestID string) []decisionRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT direction, user_id, skeleton_version, skeleton,
		       to_json(redacted_paths)::text
		  FROM request_snapshots
		 WHERE request_id = $1
		 ORDER BY direction`, requestID)
	if err != nil {
		t.Fatalf("查询快照失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []decisionRow
	for rows.Next() {
		var (
			r        decisionRow
			skeleton []byte
			redacted []byte
		)
		if err := rows.Scan(&r.direction, &r.userID, &r.version, &skeleton, &redacted); err != nil {
			t.Fatalf("扫描快照行失败: %v", err)
		}
		if err := json.Unmarshal(skeleton, &r.skeleton); err != nil {
			t.Fatalf("骨架不是 JSON 对象: %v", err)
		}
		// 列是 text[]，走 to_json 换成 JSON 数组再解：直接扫进 []byte 拿到的是
		// PostgreSQL 的数组字面量（`{}`），那不是 JSON。
		if err := json.Unmarshal(redacted, &r.redacted); err != nil {
			t.Fatalf("redacted_paths 不是 JSON 数组: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历快照行失败: %v", err)
	}
	return out
}

// dumpRows 把这一行序列化之后的**全部列**倒出来，供标记扫描使用。
//
// 走 to_jsonb 而不是逐列扫：新增一列时这条判据自动跟上，而「逐列列举」的
// 写法在加列之后会静默漏掉新那一列——那正是内容会溜进去的地方。
func dumpRows(ctx context.Context, t *testing.T, db *sql.DB, requestID string) string {
	t.Helper()
	var dumped string
	err := db.QueryRowContext(ctx,
		`SELECT coalesce(string_agg(to_jsonb(s)::text, E'\n'), '')`+
			`  FROM request_snapshots s WHERE request_id = $1`,
		requestID).Scan(&dumped)
	if err != nil {
		t.Fatalf("倒出行内容失败: %v", err)
	}
	if dumped == "" {
		t.Fatalf("request_id = %s 一行都没有——这一条就没跑成", requestID)
	}
	return dumped
}
