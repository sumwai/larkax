package channels

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 本文件覆盖「每个协议可以有自己的上游地址」（迁移 00069）。
//
// 判据的三条与 issue 一一对应：
//
//  1. 双地址：两个协议各打各的地址——这里是「候选上的地址对」，真正发出去
//     打到哪条路径由 internal/assembly 的端到端用例判（读回值相等不证明
//     执行器用的是它）；
//  2. 没给地址的协议沿用渠道 base_url——迁移之后 base_url 为 NULL 的老行
//     走的就是这一格；
//  3. 换渠道 base_url 之后，没有专属地址的协议跟着变，有专属地址的不变。

// protocolURLsOf 读回一条渠道每个协议自己的地址；NULL 的不进表（与读取侧
// 同一个口径，见 channel.ProtocolBaseURL）。
func protocolURLsOf(t *testing.T, db *sql.DB, channelID int64) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT protocol, base_url FROM channel_protocols WHERE channel_id = $1 ORDER BY protocol`,
		channelID)
	if err != nil {
		t.Fatalf("查协议地址失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var (
			p       string
			baseURL sql.NullString
		)
		if err := rows.Scan(&p, &baseURL); err != nil {
			t.Fatalf("读协议地址失败: %v", err)
		}
		if baseURL.Valid && baseURL.String != "" {
			out[p] = baseURL.String
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历协议地址失败: %v", err)
	}
	return out
}

// splitChannelRequest 是一条声明两套协议、只有 anthropic 配了专属地址的请求。
func splitChannelRequest(name, baseURL, anthropicURL, model string) CreateRequest {
	return CreateRequest{
		Name:             name,
		Type:             endpointChatType,
		Protocols:        []string{endpointChatType, endpointMessagesType},
		ProtocolBaseURLs: map[string]string{endpointMessagesType: anthropicURL},
		BaseURL:          baseURL,
		Credential:       "test-key-placeholder",
		Models:           []string{model},
		Weight:           1,
	}
}

// 建渠道时写进去的分址：落库的是两条协议各自的事实，候选与管理面都读得回来。
func TestCreateWritesPerProtocolBaseURLs(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	const (
		channelURL   = "https://api.example.com/v1"
		anthropicURL = "https://api.example.com/anthropic"
	)
	id, err := admin.Create(context.Background(), splitChannelRequest(
		"ch-split-"+itoa(nextID()), channelURL, anthropicURL, model))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	// 只有配了地址的那一行有值：另一行是 NULL，含义是「沿用渠道地址」，
	// 不是「还没填」。
	got := protocolURLsOf(t, db, id)
	if len(got) != 1 || got[endpointMessagesType] != anthropicURL {
		t.Errorf("channel_protocols 的地址 = %v，期望只有 %s=%s",
			got, endpointMessagesType, anthropicURL)
	}

	// 候选读回来带分址：执行器按它定地址（见 channel.Channel.BaseURLFor）。
	cands, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选 %d 条，期望 1 条", len(cands))
	}
	ch := cands[0].Channel
	if got := ch.BaseURLFor(channel.ProtocolOpenAI); got != channelURL {
		t.Errorf("openai 的地址 = %q，期望渠道地址 %q", got, channelURL)
	}
	if got := ch.BaseURLFor(channel.ProtocolAnthropic); got != anthropicURL {
		t.Errorf("anthropic 的地址 = %q，期望 %q", got, anthropicURL)
	}
	// 没声明的协议也退回渠道地址：判据不挑协议是否在集合里。
	if got := ch.BaseURLFor(channel.ProtocolOpenAIResponses); got != channelURL {
		t.Errorf("未配置协议的地址 = %q，期望渠道地址 %q", got, channelURL)
	}

	// 管理面逐个协议给出实际生效的地址：anthropic 在前（按协议名升序）。
	info, err := admin.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get 出错: %v", err)
	}
	want := []ProtocolAddress{
		{Protocol: channel.ProtocolAnthropic, BaseURL: anthropicURL},
		{Protocol: channel.ProtocolOpenAI, BaseURL: channelURL},
	}
	if got := info.ProtocolAddresses(); !slices.Equal(got, want) {
		t.Errorf("ProtocolAddresses = %v，期望 %v", got, want)
	}
	if !info.HasProtocolAddresses() {
		t.Error("配了分址的渠道应当打出「每个协议的实际地址」")
	}
}

// 只给分址、不给 Type / Protocols：给了地址就等于把该协议绑上。
func TestCreateProtocolAddressAloneBindsProtocol(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-url-only-" + itoa(nextID()))
	req.Type = ""
	req.Protocols = nil
	req.ProtocolBaseURLs = map[string]string{
		endpointMessagesType: "https://api.example.com/anthropic",
	}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	if got := protocolsOf(t, db, id); !slices.Equal(got, []string{endpointMessagesType}) {
		t.Errorf("channel_protocols = %v，期望只有 %q", got, endpointMessagesType)
	}
}

// 分址点的协议不在显式给的集合里：参数错，什么都别写。
func TestCreateRejectsProtocolAddressOutsideSet(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-url-mismatch-" + itoa(nextID()))
	req.Type = ""
	req.Protocols = []string{endpointChatType}
	req.ProtocolBaseURLs = map[string]string{
		endpointMessagesType: "https://api.example.com/anthropic",
	}
	_, err := admin.Create(context.Background(), req)
	if err == nil {
		t.Fatal("集合外的分址应当被拒")
	}
	for _, want := range []string{endpointMessagesType, endpointChatType} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%v", want, err)
		}
	}
	// 什么都没有写进去：只落了一半的渠道在清单上看起来完全正常。
	var n int
	if scanErr := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channels WHERE name = $1`, req.Name).Scan(&n); scanErr != nil {
		t.Fatalf("数渠道失败: %v", scanErr)
	}
	if n != 0 {
		t.Errorf("被拒的创建留下了 %d 条渠道", n)
	}
}

// update 换协议集合时，留下来的协议各自的地址不能丢。
//
// 换集合走的是「删掉整批再写回来」（见 Admin.update）：不带过来的话，一次
// --protocols 就会把已有的分址静默抹掉，而它看起来只是改了个集合。
func TestUpdateKeepsProtocolAddressesWhenSetReplaced(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()

	const (
		channelURL   = "https://api.example.com/v1"
		anthropicURL = "https://api.example.com/anthropic"
	)
	id, err := admin.Create(context.Background(), splitChannelRequest(
		"ch-keep-url-"+itoa(nextID()), channelURL, anthropicURL, model))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	// 集合换一遍（同样的两个协议）：地址原样保留。
	same := []string{endpointMessagesType, endpointChatType}
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, Protocols: &same}); err != nil {
		t.Fatalf("Update(--protocols) 出错: %v", err)
	}
	if got := protocolURLsOf(t, db, id); got[endpointMessagesType] != anthropicURL {
		t.Errorf("换集合之后地址丢了：%v", got)
	}

	// 收成只剩 anthropic：它自己的地址保留，openai 那一行连同它的地址一起走。
	only := endpointMessagesType
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, Type: &only}); err != nil {
		t.Fatalf("Update(--type) 出错: %v", err)
	}
	if got := protocolURLsOf(t, db, id); len(got) != 1 || got[endpointMessagesType] != anthropicURL {
		t.Errorf("收成单协议之后地址 = %v，期望只剩 %s=%s", got, endpointMessagesType, anthropicURL)
	}
}

// update 分址：给了地址就写下去，给空串就清回 NULL（沿用渠道地址）。
func TestUpdateSetsAndClearsProtocolAddress(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	const anthropicURL = "https://api.example.com/anthropic"
	req := baseRequest("ch-set-url-" + itoa(nextID()))
	req.Type = ""
	req.Protocols = []string{endpointChatType, endpointMessagesType}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	if got := protocolURLsOf(t, db, id); len(got) != 0 {
		t.Fatalf("刚建的渠道不该有专属地址：%v", got)
	}

	set := map[string]string{endpointMessagesType: anthropicURL}
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, ProtocolBaseURLs: &set}); err != nil {
		t.Fatalf("Update(设置地址) 出错: %v", err)
	}
	if got := protocolURLsOf(t, db, id); got[endpointMessagesType] != anthropicURL {
		t.Errorf("设置之后地址 = %v，期望 %s=%s", got, endpointMessagesType, anthropicURL)
	}
	// 集合没被这一次改动：一条地址的增删不该顺手删掉别的协议。
	if got := protocolsOf(t, db, id); !slices.Equal(got, []string{endpointMessagesType, endpointChatType}) {
		t.Errorf("设置地址之后 channel_protocols = %v，期望两条都还在", got)
	}

	clear := map[string]string{endpointMessagesType: ""}
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, ProtocolBaseURLs: &clear}); err != nil {
		t.Fatalf("Update(清掉地址) 出错: %v", err)
	}
	if got := protocolURLsOf(t, db, id); len(got) != 0 {
		t.Errorf("清掉之后地址 = %v，期望一个都不留", got)
	}
}

// update 只给分址、不给集合：不在库里的协议被补进集合（给了地址等于绑上）。
func TestUpdateProtocolAddressBindsNewProtocol(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	id, err := admin.Create(context.Background(), baseRequest("ch-bind-"+itoa(nextID())))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	set := map[string]string{endpointMessagesType: "https://api.example.com/anthropic"}
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, ProtocolBaseURLs: &set}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	if got := protocolsOf(t, db, id); !slices.Equal(got, []string{endpointMessagesType, stubType}) {
		t.Errorf("channel_protocols = %v，期望补进 %q", got, endpointMessagesType)
	}
}

// 清一个没声明过的协议的地址是空操作，报出来，不静默成功。
func TestUpdateClearUndeclaredProtocolAddressRejected(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	id, err := admin.Create(context.Background(), baseRequest("ch-clear-miss-"+itoa(nextID())))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	clear := map[string]string{endpointMessagesType: ""}
	err = admin.Update(context.Background(), UpdateRequest{ID: id, ProtocolBaseURLs: &clear})
	if err == nil {
		t.Fatal("清一个没有声明的协议的地址应当报错，而不是悄悄什么都不做")
	}
	if got := protocolsOf(t, db, id); !slices.Equal(got, []string{stubType}) {
		t.Errorf("被拒的 update 改了协议集合：%v", got)
	}
}

// 换渠道 base_url：没有专属地址的协议跟着变，有专属地址的不变。
func TestUpdateBaseURLMovesOnlyProtocolsWithoutOwnAddress(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	const (
		firstURL     = "https://api.example.com/v1"
		anthropicURL = "https://api.example.com/anthropic"
		movedURL     = "https://api.example.com/v2"
	)
	id, err := admin.Create(context.Background(), splitChannelRequest(
		"ch-move-url-"+itoa(nextID()), firstURL, anthropicURL, model))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	moved := movedURL
	if err = admin.Update(context.Background(), UpdateRequest{ID: id, BaseURL: &moved}); err != nil {
		t.Fatalf("Update(--base-url) 出错: %v", err)
	}

	cands, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选 %d 条，期望 1 条", len(cands))
	}
	ch := cands[0].Channel
	if got := ch.BaseURLFor(channel.ProtocolOpenAI); got != movedURL {
		t.Errorf("没有专属地址的 openai 仍打在 %q，期望 %q", got, movedURL)
	}
	if got := ch.BaseURLFor(channel.ProtocolAnthropic); got != anthropicURL {
		t.Errorf("有专属地址的 anthropic 变成了 %q，期望不变（%q）", got, anthropicURL)
	}
}
