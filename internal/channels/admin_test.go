package channels

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/quota"
)

// ── 测试台 ──────────────────────────────────────────────────────────────

// stubAdapter 是注册表里占位用的最小实现：写入路径只问「这个类型有人实现吗」，
// 不问它怎么发请求。
//
// protocol 留空表示 OpenAI——既有用例都按那个协议写的，而端点判据要按类型
// 算出一条路径，所以这里必须能表达「这个替身说哪套协议」（见
// TestUpdateJudgesByNewTypeWhenBothChange）。
type stubAdapter struct {
	typ      string
	protocol channel.Protocol
}

func (s *stubAdapter) Type() string { return s.typ }

func (s *stubAdapter) Protocol() channel.Protocol {
	if s.protocol == "" {
		return channel.ProtocolOpenAI
	}
	return s.protocol
}

// Endpoint 按协议推：这个替身只用来占住一个类型名，没有真实的上游路径
// ——但注册表要求它与 Protocol 对得上（见 channel.Registry.Register）。
func (s *stubAdapter) Endpoint() channel.Endpoint {
	return s.Protocol().Endpoint()
}

func (s *stubAdapter) Chat(context.Context, channel.Channel, *channel.Request) (*channel.Response, error) {
	return nil, channel.ErrUnsupported
}

func (s *stubAdapter) ChatStream(context.Context, channel.Channel, *channel.Request) (<-chan channel.StreamEvent, error) {
	return nil, channel.ErrUnsupported
}

func (s *stubAdapter) ListModels(context.Context, channel.Channel) ([]string, error) {
	return nil, channel.ErrUnsupported
}

func (s *stubAdapter) Quota(context.Context, channel.Channel) (quota.State, error) {
	return quota.State{}, channel.ErrUnsupported
}

// stubType 是注册表里会有的那个类型。用它而不是真实适配器的类型名：
// 这一层测的是「类型经注册表校验」，与某个具体适配器无关。
//
// 它只能取协议取值——注册表拒收别的名字（见 channel.Registry.Register 的
// 类型名自检），所以它现在是 systemone。取哪一个都行，选 systemone 是为了
// 不与下面那两个常量撞：那两个要拿 openai 与 anthropic 去测端点判据。
const stubType = "systemone"

// endpointChatType / endpointMessagesType 是两个类型名，它们与 channel.Protocol
// 的取值**同名**（"openai" / "anthropic"）。
//
// 端点判据按 channel.Protocol(<渠道 type>) 算这条渠道打哪条端点（见
// channels.Source 的候选构造与 Admin.checkModelEndpoints），所以测这条判据就
// 得用协议取值当类型名——注册表也本来就只收协议名。两个常量单列出来，是为了
// 让用例里读到的类型名与它期望的那条端点对得上。
const (
	endpointChatType     = "openai"
	endpointMessagesType = "anthropic"
)

func testRegistry(t *testing.T) *channel.Registry {
	t.Helper()
	r := channel.NewRegistry()
	if err := r.Register(&stubAdapter{typ: stubType, protocol: channel.ProtocolSystemOne}); err != nil {
		t.Fatalf("注册 stub 适配器失败: %v", err)
	}
	// 另外两个按协议取值命名：端点判据要按类型算路径，见上面那段说明。
	if err := r.Register(&stubAdapter{typ: endpointChatType}); err != nil {
		t.Fatalf("注册 %q 适配器失败: %v", endpointChatType, err)
	}
	if err := r.Register(&stubAdapter{typ: endpointMessagesType, protocol: channel.ProtocolAnthropic}); err != nil {
		t.Fatalf("注册 %q 适配器失败: %v", endpointMessagesType, err)
	}
	return r
}

func newAdmin(t *testing.T, db *sql.DB, sealer *credential.Sealer) *Admin {
	t.Helper()
	return NewAdmin(db, sealer, testRegistry(t))
}

// baseRequest 是一份除了被测字段之外都合法的创建请求。
func baseRequest(name string) CreateRequest {
	return CreateRequest{
		Name:       name,
		Type:       stubType,
		BaseURL:    "https://api.example.com/v1",
		Credential: "test-key-placeholder",
		Models:     []string{uniqueModel()},
		Weight:     3,
		Priority:   7,
		Setting:    map[string]any{"supports_tools": true},
	}
}

// rawCredentialEnc 直接读库里渠道主账号的密文，用来断言「写入侧到底存了什么」。
//
// 00067 之后渠道行没有凭证列：一条渠道的凭证挂在它挂着的账号上，主账号是
// 账号 id 最小的那一个（与选路同一条判据）。
func rawCredentialEnc(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	var enc []byte
	if err := db.QueryRowContext(context.Background(), `
		SELECT a.credential_enc
		  FROM channel_accounts ca
		  JOIN accounts a ON a.id = ca.account_id
		 WHERE ca.channel_id = $1
		 ORDER BY a.id
		 LIMIT 1`, id).Scan(&enc); err != nil {
		t.Fatalf("读 credential_enc 失败: %v", err)
	}
	return enc
}

// rawCredentialHint 直接读库里渠道主账号的提示，用来断言「写入侧到底存了什么」。
func rawCredentialHint(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var hint sql.NullString
	if err := db.QueryRowContext(context.Background(), `
		SELECT a.credential_hint
		  FROM channel_accounts ca
		  JOIN accounts a ON a.id = ca.account_id
		 WHERE ca.channel_id = $1
		 ORDER BY a.id
		 LIMIT 1`, id).Scan(&hint); err != nil {
		t.Fatalf("读 credential_hint 失败: %v", err)
	}
	return hint.String
}

// listInfo 从管理面的列举结果里挑出指定 id 的那条。
func listInfo(t *testing.T, admin *Admin, id int64) ChannelInfo {
	t.Helper()
	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	for _, info := range infos {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("List 里没有渠道 %d（共 %d 条）", id, len(infos))
	return ChannelInfo{}
}

func rawOwner(t *testing.T, db *sql.DB, id int64) sql.NullInt64 {
	t.Helper()
	var owner sql.NullInt64
	if err := db.QueryRowContext(context.Background(),
		`SELECT owner_user_id FROM channels WHERE id = $1`, id).Scan(&owner); err != nil {
		t.Fatalf("读 owner_user_id 失败: %v", err)
	}
	return owner
}

func rawWeight(t *testing.T, db *sql.DB, id int64) int {
	t.Helper()
	var weight int
	if err := db.QueryRowContext(context.Background(),
		`SELECT weight FROM channels WHERE id = $1`, id).Scan(&weight); err != nil {
		t.Fatalf("读 weight 失败: %v", err)
	}
	return weight
}

func countByName(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channels WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("按名字计数失败: %v", err)
	}
	return n
}

// ── 创建 ────────────────────────────────────────────────────────────────

// 这条是本 issue 最要紧的一条：创建之后要能**走现有读取路径读回来**，
// 而不只是「行插进去了」。它证明写入侧与读取侧对同一套列的假设一致。
func TestCreateThenCandidatesReadsItBack(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	merchant := seedMerchant(t, db)
	model := uniqueModel()

	req := baseRequest("ch-create-" + itoa(nextID()))
	req.OwnerUserID = merchant
	req.Models = []string{model, "other-" + model}

	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}

	c := got[0]
	if c.Channel.ID != id {
		t.Errorf("Channel.ID = %d，期望 %d", c.Channel.ID, id)
	}
	if c.Channel.Credential != req.Credential {
		t.Errorf("凭证没解回原值，得到 %q", c.Channel.Credential)
	}
	if c.Channel.Type != stubType {
		t.Errorf("Type = %q，期望 %q", c.Channel.Type, stubType)
	}
	if c.Channel.BaseURL != req.BaseURL {
		t.Errorf("BaseURL = %q，期望 %q", c.Channel.BaseURL, req.BaseURL)
	}
	if !reflect.DeepEqual(c.Channel.Models, req.Models) {
		t.Errorf("Models = %v，期望 %v", c.Channel.Models, req.Models)
	}
	if c.Channel.Setting["supports_tools"] != true {
		t.Errorf("Setting = %v，期望取回写入的 jsonb", c.Channel.Setting)
	}
	if c.OwnerUserID != merchant {
		t.Errorf("OwnerUserID = %d，期望 %d", c.OwnerUserID, merchant)
	}
	if c.Priority != 7 || c.Weight != 3 {
		t.Errorf("Priority/Weight = %d/%d，期望 7/3", c.Priority, c.Weight)
	}
	if ok, reason := c.Eligible(); !ok {
		t.Errorf("刚建出来的渠道不可选：%s", reason)
	}
}

// 库里存的是信封密文：不是明文，且能解开回原文。
//
// 测试里允许 Open——生产代码里不许（见 admin.go 的文件注释）。
func TestCreateStoresSealedCredential(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	plaintext := "sk-credential-" + itoa(nextID())

	req := baseRequest("ch-sealed-" + itoa(nextID()))
	req.Credential = plaintext
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	enc := rawCredentialEnc(t, db, id)
	if string(enc) == plaintext {
		t.Fatal("credential_enc 里存的是明文")
	}
	if strings.Contains(string(enc), plaintext) {
		t.Error("credential_enc 里含明文片段")
	}
	opened, err := sealer.Open(enc)
	if err != nil {
		t.Fatalf("Open 出错（写入侧封出来的密文应当能解开）: %v", err)
	}
	if string(opened) != plaintext {
		t.Errorf("解出来的凭证是 %q，期望原文", string(opened))
	}
}

// 凭证提示（末四位）要在写入路径上落库，而这是**唯一**的时机：密文一旦生成，
// 明文就再也拿不到了，事后补不了（没有回填的可能）。
//
// 断言写的是具体字符。断言「长度是 4」的话，取成前四位、取成末三位都能过，
// 而清单上显示的是另一个凭证的尾巴——它看起来同样正常，运维会照它去比对。
func TestCreateStoresCredentialHint(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	const plaintext = "test-key-AAAABBBBCCCCdddd"

	req := baseRequest("ch-hint-" + itoa(nextID()))
	req.Credential = plaintext
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	if got := rawCredentialHint(t, db, id); got != "dddd" {
		t.Errorf("credential_hint = %q，期望 dddd（明文末四位）", got)
	}

	// 管理面要读得到它。只写不读的列，界面拿不到，等于没接。
	if got := listInfo(t, admin, id).Hint; got != "dddd" {
		t.Errorf("List 的 Hint = %q，期望 dddd", got)
	}
}

// 换凭证时提示要跟着换。
//
// 留着旧提示会让清单上的「末四位」指向一把**已经不在这条渠道上**的凭证，
// 而界面拿它做辨识——它认错了人，比没有提示更坏。
//
// 另一半同样要钉：不给凭证的 update 里根本不该出现这两列（与 credential_enc
// 同一条规则），否则重封一次会换掉数据密钥，事后答不出「凭证什么时候换过」。
func TestUpdateRefreshesCredentialHint(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-hint-upd-" + itoa(nextID()))
	req.Credential = "test-key-oldoldoldeeee"
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	// 换凭证：提示跟着换。
	replaced := "test-key-newnewnewffff"
	if err := admin.Update(context.Background(), UpdateRequest{
		ID: id, Credential: &replaced,
	}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	if got := rawCredentialHint(t, db, id); got != "ffff" {
		t.Errorf("换凭证后 credential_hint = %q，期望 ffff", got)
	}

	// 不给凭证：提示与密文都不动。
	name := "ch-hint-upd-renamed-" + itoa(nextID())
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, Name: &name}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	if got := rawCredentialHint(t, db, id); got != "ffff" {
		t.Errorf("没给凭证的 update 动了 credential_hint：%q，期望仍是 ffff", got)
	}
}

// owner_user_id 传 0 是「平台自营」，库里要存 NULL 而不是 0——
// 0 会去匹配一个不存在的用户，外键直接拒绝。
func TestCreatePlatformChannelStoresNullOwner(t *testing.T) {

	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	req := baseRequest("ch-platform-" + itoa(nextID()))
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	if owner := rawOwner(t, db, id); owner.Valid {
		t.Errorf("owner_user_id = %d，期望 NULL", owner.Int64)
	}
	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].OwnerUserID != 0 {
		t.Errorf("OwnerUserID = %d，期望 0（平台自营）", got[0].OwnerUserID)
	}
}

// weight 传 0 落成 1：表默认值如此，route 也把 <= 0 当成 1。
func TestCreateZeroWeightStoresOne(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-weight-" + itoa(nextID()))
	req.Weight = 0
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	if got := rawWeight(t, db, id); got != 1 {
		t.Errorf("weight = %d，期望 1", got)
	}
}

// 没人实现的类型不许进库：选路时它会表现为「这个渠道永远选不中」。
func TestCreateRejectsUnregisteredType(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-badtype-" + itoa(nextID()))
	req.Type = "no-such-adapter"
	if _, err := admin.Create(context.Background(), req); err == nil {
		t.Fatal("未注册的渠道类型应当被拒")
	}
	if n := countByName(t, db, req.Name); n != 0 {
		t.Errorf("被拒的请求留下了 %d 行", n)
	}
}

// 未知的协议名要在写入时被拒，**并且说出已知的那几个**。
//
// 只报「未知的协议」不够：写错一个名字的人下一步要的是「那我该写什么」。
// 清单取自适配器注册表（见 validateProtocol），所以加一个协议时这里不必改。
func TestCreateRejectsUnknownProtocol(t *testing.T) {
	// 不连库：协议校验排在全部库操作之前（registry 已经够判）。
	admin := NewAdmin(nil, nil, testRegistry(t))

	req := baseRequest("ch-badproto-" + itoa(nextID()))
	req.Type = ""
	req.Protocols = []string{endpointChatType, "no-such-protocol"}

	_, err := admin.Create(context.Background(), req)
	if err == nil {
		t.Fatal("未注册的协议应当被拒")
	}
	for _, want := range []string{`"no-such-protocol"`, "已知协议", string(channel.ProtocolAnthropic),
		string(channel.ProtocolOpenAI), stubType} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没出现 %q：%v", want, err)
		}
	}
}

// --type 与 --protocols 同时给时，主协议必须在集合里。
//
// 不在里面等于说两件互相矛盾的事：「这条渠道的类型是 X」与「它声明的协议里
// 没有 X」。写进库之后，读取侧按集合分派、channel get 按 type 展示，两边
// 各说一套。
func TestCreateRejectsTypeOutsideProtocols(t *testing.T) {
	admin := NewAdmin(nil, nil, testRegistry(t))

	req := baseRequest("ch-mismatch-" + itoa(nextID()))
	req.Type = endpointMessagesType
	req.Protocols = []string{endpointChatType}

	_, err := admin.Create(context.Background(), req)
	if err == nil {
		t.Fatal("--type 不在 --protocols 里应当被拒")
	}
	if !strings.Contains(err.Error(), "不在 --protocols") {
		t.Errorf("报错应当是「不在 --protocols」，得到 %v", err)
	}
}

// 多协议渠道：只声明一套协议时，写入仍按单协议那一套走（一行、与 type 相同）。
func TestCreateTypeOnlyWritesSingleProtocol(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-single-" + itoa(nextID()))
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	if got := protocolsOf(t, db, id); !slices.Equal(got, []string{req.Type}) {
		t.Errorf("channel_protocols = %v，期望只有 type %q 一个", got, req.Type)
	}
	var typ string
	if scanErr := db.QueryRowContext(context.Background(),
		`SELECT type FROM channels WHERE id = $1`, id).Scan(&typ); scanErr != nil {
		t.Fatalf("读 type 失败: %v", scanErr)
	}
	if typ != req.Type {
		t.Errorf("channels.type = %q，期望 %q", typ, req.Type)
	}
}

// 多协议渠道：集合原样落库，主协议取第一个；候选与管理面都读得回来。
func TestCreateMultiProtocolWritesSet(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	req := baseRequest("ch-multi-" + itoa(nextID()))
	req.Type = ""
	req.Protocols = []string{endpointMessagesType, endpointChatType}
	req.Models = []string{model}

	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	// 读回来按 SQL 的 ORDER BY 排序（anthropic 在 openai 之前），与调用方
	// 给的顺序无关——顺序只影响主协议（channels.type），集合本身是无序的。
	want := []string{endpointMessagesType, endpointChatType}
	if got := protocolsOf(t, db, id); !slices.Equal(got, want) {
		t.Errorf("channel_protocols = %v，期望 %v", got, want)
	}
	// 主协议取集合的第一个（调用方给的顺序，不是排序后的）。
	var typ string
	if scanErr := db.QueryRowContext(context.Background(),
		`SELECT type FROM channels WHERE id = $1`, id).Scan(&typ); scanErr != nil {
		t.Fatalf("读 type 失败: %v", scanErr)
	}
	if typ != endpointMessagesType {
		t.Errorf("channels.type = %q，期望 %q（集合的第一个）", typ, endpointMessagesType)
	}

	// 候选读回来带集合：执行器靠它按入口协议分派。
	cands, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选 %d 条，期望 1 条", len(cands))
	}
	if got := cands[0].Channel.Protocols; !slices.Equal(got, []channel.Protocol{
		channel.ProtocolAnthropic, channel.ProtocolOpenAI}) {
		t.Errorf("候选的 Protocols = %v，期望 openai 与 anthropic", got)
	}

	// 管理面读回来也带集合，但输出形状不变（见 cmd 的逐字用例）。
	info, err := admin.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get 出错: %v", err)
	}
	if got := info.DeclaredProtocols(); !slices.Equal(got, want) {
		t.Errorf("DeclaredProtocols = %v，期望 %v", got, want)
	}
}

// update 整体替换协议集合：主协议跟着变，旧的行不残留。
func TestUpdateReplacesProtocolSet(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	id, err := admin.Create(context.Background(), baseRequest("ch-update-proto-"+itoa(nextID())))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	both := []string{endpointChatType, endpointMessagesType}
	// 读回来按 SQL 的 ORDER BY 排序，与写入时给的顺序无关。
	stored := []string{endpointMessagesType, endpointChatType}
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, Protocols: &both}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	var typ string
	if scanErr := db.QueryRowContext(context.Background(),
		`SELECT type FROM channels WHERE id = $1`, id).Scan(&typ); scanErr != nil {
		t.Fatalf("读 type 失败: %v", scanErr)
	}
	if typ != endpointChatType {
		t.Errorf("channels.type = %q，期望 %q（集合的第一个）", typ, endpointChatType)
	}
	if got := protocolsOf(t, db, id); !slices.Equal(got, stored) {
		t.Errorf("channel_protocols = %v，期望 %v", got, stored)
	}

	// 只给 --type：集合收敛成它一个，多余的那一行必须删掉。
	only := endpointMessagesType
	if err := admin.Update(context.Background(), UpdateRequest{ID: id, Type: &only}); err != nil {
		t.Fatalf("Update(--type) 出错: %v", err)
	}
	if got := protocolsOf(t, db, id); !slices.Equal(got, []string{endpointMessagesType}) {
		t.Errorf("channel_protocols = %v，期望只剩 %q", got, endpointMessagesType)
	}
}

// 没有这份声明时按 type 一种协议判：迁移之前的老行、夹具直接插的行都走这格。
func TestDeclaredProtocolsFallsBackToType(t *testing.T) {
	info := ChannelInfo{Type: endpointMessagesType}
	if got := info.DeclaredProtocols(); !slices.Equal(got, []string{endpointMessagesType}) {
		t.Errorf("DeclaredProtocols = %v，期望退回 type", got)
	}
}

// protocolsOf 按 SQL 的排序读回一条渠道声明了哪些协议。
func protocolsOf(t *testing.T, db *sql.DB, channelID int64) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT protocol FROM channel_protocols WHERE channel_id = $1 ORDER BY protocol`, channelID)
	if err != nil {
		t.Fatalf("查协议失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("读协议失败: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历协议失败: %v", err)
	}
	return out
}

// 各种字段校验失败都要在**落库之前**停下，且报错不回显字段值。
func TestCreateRejectsInvalidFields(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	cases := []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{"渠道名为空", func(r *CreateRequest) { r.Name = "   " }},
		// 凭证不在这张表里：空表示「这次不建账号」，是合法的中间态
		// （见 CreateRequest.Credential 与迁移 00067）。models 也不在这张表里：
		// 空白名单是合法中间态（见 TestCreateWithoutModelsStaysUnselectable）。
		{"models 里有空项", func(r *CreateRequest) { r.Models = []string{"a", " "} }},
		{"base_url 为空", func(r *CreateRequest) { r.BaseURL = "" }},
		{"base_url 没有 scheme", func(r *CreateRequest) { r.BaseURL = "api.example.com/v1" }},
		{"base_url 不是 http", func(r *CreateRequest) { r.BaseURL = "ftp://api.example.com/v1" }},
		{"base_url 没有主机名", func(r *CreateRequest) { r.BaseURL = "https:///v1" }},
		{"base_url 带 query", func(r *CreateRequest) { r.BaseURL = "https://api.example.com/v1?tenant=abc" }},
		{"base_url 带 fragment", func(r *CreateRequest) { r.BaseURL = "https://api.example.com/v1#frag" }},
		{"weight 为负", func(r *CreateRequest) { r.Weight = -1 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest("ch-invalid-" + itoa(nextID()))
			tc.mutate(&req)

			_, err := admin.Create(context.Background(), req)
			if err == nil {
				t.Fatal("非法字段应当被拒")
			}
			// 报错只说哪个字段不对，不回显值。凭证字段的原文尤其不能出现。
			if strings.Contains(err.Error(), req.Credential) && req.Credential != "" {
				t.Errorf("报错里出现了凭证原文：%v", err)
			}
			if n := countByName(t, db, req.Name); n != 0 {
				t.Errorf("被拒的请求留下了 %d 行", n)
			}
		})
	}
}

// TestCreateWithoutModelsStaysUnselectable 钉住「空白名单是合法中间态」这条
// 新语义的两端：行真的落进库（白名单是空数组，不是 NULL），而选路一条都选
// 不中它。
//
// 两端缺一不可：只断言「创建成功」会漏掉落成 NULL（那一列是 NOT NULL，
// 读回来会是另一回事），只断言「选不中」的话，一条没建出来的渠道同样选不中。
func TestCreateWithoutModelsStaysUnselectable(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)

	req := baseRequest("ch-no-models-" + itoa(nextID()))
	req.Models = nil // 显式给 nil，与命令行「没给 --models」同一条路径
	model := uniqueModel()

	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("空白名单应当建得出来：%v", err)
	}

	// 用 array_to_json 接 text[]：database/sql 不认 text[] 这个 driver.Value，
	// 与候选集查询同一条理由（见 candidateRow.modelsJSON）。
	// 期望值是字面量 "[]"：落成 NULL 时这里是 "null"，那正是这一条要抓的。
	var stored string
	if err := db.QueryRowContext(context.Background(),
		`SELECT array_to_json(models)::text FROM channels WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("读模型白名单失败: %v", err)
	}
	if stored != "[]" {
		t.Errorf("落库的 models = %s，期望空数组 []", stored)
	}

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	for _, c := range got {
		if c.Channel.ID == id {
			t.Fatalf("空白名单的渠道被选成了候选：%d", id)
		}
	}
}

// 报错要点出**第几项**，且与人的数法一致（从 1 开始）。
//
// 名单可以很长，只说「有个模型名为空」等于让人自己去找。这一条同时管着报文里
// 的 i+1：抄成 i-1 时会打出「第 0 个」，而那种输出没人会多看一眼。
func TestNormalizeModelsReportsOneBasedPosition(t *testing.T) {
	_, err := normalizeModels([]string{"model-a", " "})
	if err == nil {
		t.Fatal("空模型名应当被拒")
	}
	if want := "第 2 个模型名为空"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v，期望包含 %q", err, want)
	}
}

// TestNormalizeBaseURLJudgesQueryAndFragment 把「带 query / fragment 的地址一律拒」
// 钉在判据本身上。它不碰库，所以在没有 DSN 的机器上也真的跑（不是 SKIP）。
//
// 为什么要拒：httpx.Endpoint 是字符串拼接，只看结尾斜杠，看不见 `?` 与 `#`——
// 端点路径会被接到 query 后面，拼出来的地址指着另一个地方。而它是**安静的**坏：
// 配置写错的人要等到请求失败（或者请求成功、打的却不是那个端点）才知道。所以
// 挡在写入侧。错误文本必须点出是哪一部分不合法：「非法 URL」那句话对改配置的人
// 没有用，他手里的地址看着挺正常。
func TestNormalizeBaseURLJudgesQueryAndFragment(t *testing.T) {
	rejected := []struct {
		name string
		raw  string
		want string
	}{
		{"query", "https://api.example.com/v1?tenant=abc", "不能带 query"},
		{"空的 query", "https://api.example.com/v1?", "不能带 query"},
		{"fragment", "https://api.example.com/v1#frag", "不能带 fragment"},
		{"空的 fragment", "https://api.example.com/v1#", "不能带 fragment"},
	}
	for _, tc := range rejected {
		t.Run("拒 "+tc.name, func(t *testing.T) {
			if _, err := NormalizeBaseURL(tc.raw); err == nil {
				t.Fatalf("%q 应当被拒", tc.raw)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %q 里没有点出 %q", err.Error(), tc.want)
			}
		})
	}

	// 护栏：正常写法一条都不许被拒。没有这一臂的话，「一律拒掉」也能让上面那条过。
	accepted := []string{
		"https://api.example.com/v1",
		"https://api.example.com/v1/",         // 结尾带斜杠
		"https://api.example.com",             // 没有路径
		"https://api.example.com:8443/v1",     // 带端口
		"http://127.0.0.1:8080",               // http、本机地址
		"https://api.example.com/v1/tenant-a", // 路径里有连字符
	}
	for _, raw := range accepted {
		got, err := NormalizeBaseURL(raw)
		if err != nil {
			t.Errorf("%q 不该被拒: %v", raw, err)
			continue
		}
		if got != raw {
			t.Errorf("NormalizeBaseURL(%q) = %q，期望原样返回", raw, got)
		}
	}
}

// 本进程没有主密钥时，创建要报 ErrNoMasterKey（与读取路径同一个错误），
// 而不是把一条解不开的渠道写进库。
func TestCreateWithoutMasterKey(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db, nil, testRegistry(t))

	req := baseRequest("ch-nokey-" + itoa(nextID()))
	_, err := admin.Create(context.Background(), req)
	if !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("err = %v，期望 ErrNoMasterKey", err)
	}
	if n := countByName(t, db, req.Name); n != 0 {
		t.Errorf("没有主密钥时仍然写进去了 %d 行", n)
	}
}

// 没有注册表时报错而不是 panic：一个拼错的构造参数不该把进程带走。
func TestCreateWithoutRegistry(t *testing.T) {
	admin := NewAdmin(nil, nil, nil)
	_, err := admin.Create(context.Background(), baseRequest("ch-noreg"))
	if err == nil {
		t.Fatal("没有注册表时应当报错")
	}
}

// 外键约束（owner_user_id 指向不存在的用户）要原样报出来，不能被吞掉，
// 而且要带上跨包的兜底哨兵。
//
// channels 表上没有别的唯一键：**渠道名不是唯一的**（见 00001_init.sql，
// 只有 owner 与 status 两条普通索引），所以没有「名字冲突」这条路径可测。
// 数据库层的完整性失败就只剩这一条外键，而它没有具名哨兵——命令行那边
// 靠 dberr.ErrConstraintViolation 把它归到「约束冲突」。
func TestCreateForeignKeyViolationSurfaces(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-badowner-" + itoa(nextID()))
	req.OwnerUserID = 1 << 40

	_, err := admin.Create(context.Background(), req)
	if err == nil {
		t.Fatal("不存在的 owner 应当被外键拒掉")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v，期望 *pgconn.PgError", err)
	}
	if pgErr.Code != "23503" {
		t.Errorf("SQLSTATE = %s（%s），期望 23503 foreign_key_violation", pgErr.Code, pgErr.Message)
	}
	if !errors.Is(err, dberr.ErrConstraintViolation) {
		t.Errorf("错误链里没有 dberr.ErrConstraintViolation：%v", err)
	}
	if n := countByName(t, db, req.Name); n != 0 {
		t.Errorf("被拒的请求留下了 %d 行", n)
	}
}

// ── 列举 ────────────────────────────────────────────────────────────────

// 列举**不返回密文**。
//
// 断言分三层：字段集合被逐个钉住（加字段会先让这条失败）、没有任何
// []byte 字段（密文在 Go 里唯一的形态）、渲染出来的文本里既没有密文
// 的十六进制也没有明文。
func TestListReturnsNoCiphertext(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	plaintext := "sk-listed-" + itoa(nextID())

	req := baseRequest("ch-list-" + itoa(nextID()))
	req.Credential = plaintext
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}

	var info ChannelInfo
	found := false
	for _, candidate := range infos {
		if candidate.ID == id {
			info, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatalf("List 里没有刚建的渠道 %d（共 %d 条）", id, len(infos))
	}

	// 字段集合：改这里之前先问「这个新字段该不该出现在管理面的返回值里」。
	want := []string{
		"ID", "OwnerUserID", "Name", "Type", "Protocols", "BaseURL", "Status", "Models",
		"Weight", "Priority", "Setting", "CreatedAt", "UpdatedAt",
		"Accounts", "CredentialKeys", "Hint", "CredentialErr", "DeletedAt",
	}
	typ := reflect.TypeOf(ChannelInfo{})
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("ChannelInfo 的字段 = %v，期望 %v", names, want)
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 {
			t.Errorf("字段 %s 是 []byte：密文不该有能落进返回值里的形态", f.Name)
		}
	}

	rendered := fmt.Sprintf("%+v", info)
	if strings.Contains(rendered, plaintext) {
		t.Error("列举结果里出现了明文凭证")
	}
	if hexStr := hex.EncodeToString(rawCredentialEnc(t, db, id)); strings.Contains(rendered, hexStr) {
		t.Error("列举结果里出现了密文")
	}

	// 该给的是元信息：这份密文挂在哪几把主密钥下。
	if !reflect.DeepEqual(info.CredentialKeys, sealer.KeyIDs()) {
		t.Errorf("CredentialKeys = %v，期望 %v", info.CredentialKeys, sealer.KeyIDs())
	}
	if info.Name != req.Name || info.Status != StatusEnabled || info.Priority != 7 || info.Weight != 3 {
		t.Errorf("列举出来的字段不对: %+v", info)
	}
	if info.Setting["supports_tools"] != true {
		t.Errorf("Setting = %v，期望取回写入的 jsonb", info.Setting)
	}
}

// 一行密文坏了不影响别的行：坏的那行照样列出来，原因写在它的 CredentialErr 上。
//
// 这一条钉的是「整次报错」那个更早的写法——它有真实的代价：库里有**一行**
// 手工写进去的密文（占位符），整个清单就一条都看不到。
func TestListSurvivesCorruptEnvelope(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)

	broken := insertChannel(t.Context(), t, db, sealer, channelSpec{
		model: uniqueModel(), credential: []byte{0x99},
	})
	req := baseRequest("ch-good-" + itoa(nextID()))
	good, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("一行密文坏了不该让整次列举失败: %v", err)
	}

	byID := map[int64]ChannelInfo{}
	for _, info := range infos {
		byID[info.ID] = info
	}
	badRow, ok := byID[broken]
	if !ok {
		t.Fatalf("密文坏了的那一行没被列出来（共 %d 条）", len(infos))
	}
	if badRow.CredentialErr == nil {
		t.Error("密文坏了却没在 CredentialErr 上说明")
	}
	if len(badRow.CredentialKeys) != 0 {
		t.Errorf("密文解析不出来却给出了钥匙清单：%v", badRow.CredentialKeys)
	}

	okRow, ok := byID[good]
	if !ok {
		t.Fatalf("好的那一行没被列出来（共 %d 条）", len(infos))
	}
	if okRow.CredentialErr != nil {
		t.Errorf("好的那一行被报了错: %v", okRow.CredentialErr)
	}
	if !reflect.DeepEqual(okRow.CredentialKeys, sealer.KeyIDs()) {
		t.Errorf("CredentialKeys = %v，期望 %v", okRow.CredentialKeys, sealer.KeyIDs())
	}
}

// ── 修改 ────────────────────────────────────────────────────────────────

// 改其他字段时密文一个字节都不动。
//
// 这一条钉的是「重封一次会换掉 DEK」：密文变了而凭证没变的话，
// 事后审计「这条渠道的凭证什么时候换过」就答不出来。
func TestUpdateWithoutCredentialKeepsCiphertext(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	req := baseRequest("ch-keep-" + itoa(nextID()))
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	before := rawCredentialEnc(t, db, id)

	newName := "ch-renamed-" + itoa(nextID())
	newWeight := 5
	err = admin.Update(context.Background(), UpdateRequest{
		ID: id, Name: &newName, Weight: &newWeight,
	})
	if err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	if after := rawCredentialEnc(t, db, id); !reflect.DeepEqual(before, after) {
		t.Error("只改了名字，密文却变了")
	}

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Channel.Name != newName || got[0].Weight != newWeight {
		t.Errorf("改动没生效: Name=%q Weight=%d", got[0].Channel.Name, got[0].Weight)
	}
	if got[0].Channel.Credential != req.Credential {
		t.Errorf("凭证变了：%q", got[0].Channel.Credential)
	}
}

// 改凭证时重新封：密文变了，解出来是新凭证，旧凭证再也解不出来。
func TestUpdateCredentialReseals(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	req := baseRequest("ch-rotate-" + itoa(nextID()))
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	before := rawCredentialEnc(t, db, id)

	rotated := "sk-rotated-" + itoa(nextID())
	if err = admin.Update(context.Background(), UpdateRequest{ID: id, Credential: &rotated}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	after := rawCredentialEnc(t, db, id)
	if reflect.DeepEqual(before, after) {
		t.Error("换了凭证，密文却没变")
	}
	opened, err := sealer.Open(after)
	if err != nil {
		t.Fatalf("Open 出错: %v", err)
	}
	if string(opened) != rotated {
		t.Errorf("解出来的是 %q，期望新凭证", string(opened))
	}

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 || got[0].Channel.Credential != rotated {
		t.Errorf("读取路径拿到的不是新凭证: %+v", got)
	}
}

// 每一个可改字段都要能改，并且改完从读取路径看得到。
//
// 单独一条而不是并进上面那条：UPDATE 里 text[] 与 jsonb 的传参与 INSERT 是
// 两条不同的语句，只测过 INSERT 的话，「改 models 会写坏这一行」不会被发现。
func TestUpdateEveryFieldReadsBack(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	oldModel := uniqueModel()
	newModel := uniqueModel()

	req := baseRequest("ch-fields-" + itoa(nextID()))
	req.Models = []string{oldModel}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	newName := "ch-all-fields-" + itoa(nextID())
	newType := stubType
	newBaseURL := "https://api.example.org/v2"
	newModels := []string{newModel, "extra-" + newModel}
	newWeight := 9
	newPriority := 0 // 零值是合法修改，必须与「没提到」区分开
	newSetting := map[string]any{"supports_tools": false, "note": "updated"}

	err = admin.Update(context.Background(), UpdateRequest{
		ID:       id,
		Name:     &newName,
		Type:     &newType,
		BaseURL:  &newBaseURL,
		Models:   &newModels,
		Weight:   &newWeight,
		Priority: &newPriority,
		Setting:  &newSetting,
	})
	if err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), newModel)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("按新模型名取到的候选数 = %d，期望 1", len(got))
	}
	c := got[0]
	if c.Channel.Name != newName {
		t.Errorf("Name = %q，期望 %q", c.Channel.Name, newName)
	}
	if c.Channel.BaseURL != newBaseURL {
		t.Errorf("BaseURL = %q，期望 %q", c.Channel.BaseURL, newBaseURL)
	}
	if !reflect.DeepEqual(c.Channel.Models, newModels) {
		t.Errorf("Models = %v，期望 %v", c.Channel.Models, newModels)
	}
	if c.Weight != newWeight || c.Priority != newPriority {
		t.Errorf("Weight/Priority = %d/%d，期望 %d/%d", c.Weight, c.Priority, newWeight, newPriority)
	}
	if c.Channel.Setting["supports_tools"] != false || c.Channel.Setting["note"] != "updated" {
		t.Errorf("Setting = %v，期望整个对象被替换", c.Channel.Setting)
	}

	// 旧模型名不该再命中：models 是整列替换，不是追加。
	old, err := src.Candidates(context.Background(), oldModel)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(old) != 0 {
		t.Errorf("按旧模型名仍取到 %d 个候选，期望 0（models 要整列替换）", len(old))
	}

	// 列举侧读出来的是同一套值。
	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	for _, info := range infos {
		if info.ID != id {
			continue
		}
		if info.Name != newName || info.BaseURL != newBaseURL || info.Weight != newWeight ||
			info.Priority != newPriority || !reflect.DeepEqual(info.Models, newModels) {
			t.Errorf("列举出来的与写入的不一致: %+v", info)
		}
	}
}

// 改一个不存在的 id 要报 ErrChannelNotFound，不能静默成功。
func TestUpdateUnknownChannel(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	name := "ch-nowhere-" + itoa(nextID())
	err := admin.Update(context.Background(), UpdateRequest{ID: 1 << 40, Name: &name})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("err = %v，期望 ErrChannelNotFound", err)
	}
}

// 一个字段都没提到时直接报错：发一条只动 updated_at 的 UPDATE 会让
// 「结构体拼错了」看起来像一次成功的修改。
func TestUpdateWithoutChanges(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	if err := admin.Update(context.Background(), UpdateRequest{ID: 1}); !errors.Is(err, ErrNoChanges) {
		t.Errorf("err = %v，期望 ErrNoChanges", err)
	}
}

// 改类型同样要过注册表校验。
func TestUpdateRejectsUnregisteredType(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()

	req := baseRequest("ch-retype-" + itoa(nextID()))
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	bad := "no-such-adapter"
	if err = admin.Update(context.Background(), UpdateRequest{ID: id, Type: &bad}); err == nil {
		t.Fatal("未注册的渠道类型应当被拒")
	}

	// 原类型没被改坏。
	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	for _, info := range infos {
		if info.ID == id && info.Type != stubType {
			t.Errorf("被拒的修改还是改了库：type = %q", info.Type)
		}
	}
}

// 改 base_url 走的是同一条判据，所以在写入路径上也拒一次：绕过命令行的调用方
// （脚本、将来的管理面）同样写不进一个会被拼坏的地址。
//
// 判据本身的形状由 TestNormalizeBaseURLJudgesQueryAndFragment 钉住，这里盯的是
// **落库这一侧**：拒绝之后库里那一列一个字节都没动——「报错了但已经写进去了」
// 是这类校验最坏的失败方式。
func TestUpdateRejectsBaseURLWithQueryOrFragment(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-badbase-" + itoa(nextID()))
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	for _, bad := range []string{
		"https://api.example.com/v1?tenant=abc",
		"https://api.example.com/v1#frag",
	} {
		t.Run(bad, func(t *testing.T) {
			value := bad
			if err := admin.Update(context.Background(), UpdateRequest{ID: id, BaseURL: &value}); err == nil {
				t.Fatalf("%q 应当被拒", bad)
			}

			var got string
			if err := db.QueryRowContext(context.Background(),
				`SELECT base_url FROM channels WHERE id = $1`, id).Scan(&got); err != nil {
				t.Fatalf("读 base_url 失败: %v", err)
			}
			if got != req.BaseURL {
				t.Errorf("被拒的修改还是改了库：base_url = %q，期望 %q", got, req.BaseURL)
			}
		})
	}
}

// ── 启停 ────────────────────────────────────────────────────────────────

// 停用之后读取路径就看不见它了，启用之后又能看见。
func TestSetStatusTogglesVisibility(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	req := baseRequest("ch-status-" + itoa(nextID()))
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	src := New(db, sealer)
	candidates := func() int {
		t.Helper()
		got, listErr := src.Candidates(context.Background(), model)
		if listErr != nil {
			t.Fatalf("Candidates 出错: %v", listErr)
		}
		return len(got)
	}

	if n := candidates(); n != 1 {
		t.Fatalf("刚建出来时候选数 = %d，期望 1", n)
	}

	if err = admin.SetStatus(context.Background(), id, StatusDisabled); err != nil {
		t.Fatalf("停用出错: %v", err)
	}
	if n := candidates(); n != 0 {
		t.Errorf("停用后候选数 = %d，期望 0", n)
	}

	if err = admin.SetStatus(context.Background(), id, StatusEnabled); err != nil {
		t.Fatalf("启用出错: %v", err)
	}
	if n := candidates(); n != 1 {
		t.Errorf("启用后候选数 = %d，期望 1", n)
	}
}

// 'drained' 是额度监控写进去的**观测**结果，不是运维能设的状态：
// 管理面放行它等于允许把一条观测伪造成配置。
func TestSetStatusRejectsObservedState(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	req := baseRequest("ch-drained-" + itoa(nextID()))
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	if err = admin.SetStatus(context.Background(), id, "drained"); err == nil {
		t.Fatal("管理面不该能设置 drained")
	}
	if err = admin.SetStatus(context.Background(), id, "whatever"); err == nil {
		t.Fatal("未知状态应当被拒")
	}
}

func TestSetStatusUnknownChannel(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	err := admin.SetStatus(context.Background(), 1<<40, StatusDisabled)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("err = %v，期望 ErrChannelNotFound", err)
	}
}

// 非正数 id 在碰库之前就被拒：0 与负数一样，都不是「查不到的那一行」。
//
// 判据不能只看「返回了错误」——这些方法对不存在的 id 也会报错。差别在报文：
// id 为 0 是调用方算错了，不是库里少了一行，两者的排查方向不同。只判 err != nil
// 的话，把 id 校验里的 <= 写成 < 也没有一条用例会红（变异测试实测：四种写路径
// 与 HealthOne 的 <= 0 改成 < 0 之后全部还活着）。
func TestAdminMethodsRejectNonPositiveID(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	name := "ch-zero"
	cases := []struct {
		name string
		call func() error
	}{
		{"Update", func() error {
			return admin.Update(context.Background(), UpdateRequest{ID: 0, Name: &name})
		}},
		{"SetStatus", func() error {
			return admin.SetStatus(context.Background(), 0, StatusEnabled)
		}},
		{"SoftDelete", func() error {
			_, err := admin.SoftDelete(context.Background(), 0)
			return err
		}},
		{"Restore", func() error {
			_, err := admin.Restore(context.Background(), 0)
			return err
		}},
		{"HealthOne", func() error {
			_, _, err := admin.HealthOne(context.Background(), 0, time.Now())
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("id 为 0 应当被拒")
			}
			if !strings.Contains(err.Error(), "渠道 id 无效") {
				t.Errorf("err = %v，期望点名「渠道 id 无效」而不是把它当成查不到的行", err)
			}
			if errors.Is(err, ErrChannelNotFound) {
				t.Errorf("err = %v，id 为 0 不该报 ErrChannelNotFound", err)
			}
		})
	}
}

// ── 软删 ────────────────────────────────────────────────────────────────

// TestSoftDeleteIsIdempotent 钉住删除的两条结果：第一次返回 true，第二次返回
// false 且**不报错**（幂等），而不存在的 id 仍然报 ErrChannelNotFound。
//
// 三者混在一起是危险的：把「已经删了」报成成功会让拿错 id 的调用悄悄过去，
// 反过来把重跑报成失败会让脚本没法重复执行。
func TestSoftDeleteIsIdempotent(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	id, err := admin.Create(context.Background(), baseRequest("ch-del-"+itoa(nextID())))
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	deleted, err := admin.SoftDelete(context.Background(), id)
	if err != nil {
		t.Fatalf("第一次 SoftDelete 出错: %v", err)
	}
	if !deleted {
		t.Error("第一次 SoftDelete 应当返回 true")
	}

	deleted, err = admin.SoftDelete(context.Background(), id)
	if err != nil {
		t.Fatalf("再删一次不该报错（幂等），得到 %v", err)
	}
	if deleted {
		t.Error("再删一次应当返回 false（已经是删除状态）")
	}

	// 删除时刻不该被第二次调用刷新：它说「什么时候删的」。
	var deletedAt sql.NullTime
	if scanErr := db.QueryRowContext(context.Background(),
		`SELECT deleted_at FROM channels WHERE id = $1`, id).Scan(&deletedAt); scanErr != nil {
		t.Fatalf("读回 deleted_at 失败: %v", scanErr)
	}
	if !deletedAt.Valid {
		t.Fatal("deleted_at 仍是 NULL：删除没落库")
	}

	_, err = admin.SoftDelete(context.Background(), 1<<40)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("删一个不存在的 id 应当报 ErrChannelNotFound，得到 %v", err)
	}

	// 删除把状态一并置成 disabled：那是给漏网读路径的兜底。
	var status string
	if scanErr := db.QueryRowContext(context.Background(),
		`SELECT status FROM channels WHERE id = $1`, id).Scan(&status); scanErr != nil {
		t.Fatalf("读回 status 失败: %v", scanErr)
	}
	if status != StatusDisabled {
		t.Errorf("删除后的 status = %q，期望 %q", status, StatusDisabled)
	}

	// 选路看不见它。
	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	for _, c := range got {
		if c.Channel.ID == id {
			t.Errorf("已删的渠道 %d 仍出现在候选集里", id)
		}
	}
}

// TestWritePathsRejectDeletedChannel 钉住「已删的渠道不能再改」。
//
// 判据不只是「返回了错误」：**那一行必须一个字节都没变**。静默改一行用户
// 看不见的数据，会让「我已经删掉它了」与「它又被改回来了」在库里分不出来，
// 而后者没有任何东西会报出来。
func TestWritePathsRejectDeletedChannel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)

	req := baseRequest("ch-frozen-" + itoa(nextID()))
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	if _, err = admin.SoftDelete(context.Background(), id); err != nil {
		t.Fatalf("SoftDelete 出错: %v", err)
	}

	before := readChannelRaw(t, db, id)

	newName := "renamed-after-delete"
	newPriority := 99
	cases := map[string]func() error{
		"Update 名字": func() error {
			return admin.Update(context.Background(), UpdateRequest{ID: id, Name: &newName})
		},
		"Update 优先级": func() error {
			return admin.Update(context.Background(), UpdateRequest{ID: id, Priority: &newPriority})
		},
		"Enable": func() error {
			return admin.SetStatus(context.Background(), id, StatusEnabled)
		},
		"Disable": func() error {
			return admin.SetStatus(context.Background(), id, StatusDisabled)
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, ErrChannelDeleted) {
				t.Fatalf("err = %v，期望 ErrChannelDeleted", err)
			}
		})
	}

	// 一组调用之后整行不变——逐个字段比对，而不是只看 deleted_at。
	after := readChannelRaw(t, db, id)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("已删的渠道被改动了：\n之前 %+v\n之后 %+v", before, after)
	}
}

// channelRaw 是一行的全部可写列，用来证明「一个字节都没动」。
type channelRaw struct {
	Name        string
	Status      string
	Priority    int
	Weight      int
	Setting     string
	UpdatedAt   time.Time
	DeletedAt   sql.NullTime
	CredentialE string
}

func readChannelRaw(t *testing.T, db *sql.DB, id int64) channelRaw {
	t.Helper()
	var (
		row channelRaw
		enc []byte
		set []byte
	)
	err := db.QueryRowContext(context.Background(), `
		SELECT name, status, priority, weight, setting, updated_at, deleted_at,
		       (SELECT a.credential_enc
		          FROM channel_accounts ca
		          JOIN accounts a ON a.id = ca.account_id
		         WHERE ca.channel_id = channels.id
		         ORDER BY a.id
		         LIMIT 1)
		  FROM channels WHERE id = $1`, id).
		Scan(&row.Name, &row.Status, &row.Priority, &row.Weight, &set,
			&row.UpdatedAt, &row.DeletedAt, &enc)
	if err != nil {
		t.Fatalf("读渠 %d 失败: %v", id, err)
	}
	row.Setting = string(set)
	row.CredentialE = hex.EncodeToString(enc)
	return row
}

// ── 端点判据：写入侧 ────────────────────────────────────────────────────
//
// 模型目录里可以声明「这个模型只在这些端点上被服务」（见
// catalog.Capabilities 的 Endpoints）。把这样一个模型挂到一条端点数对不上
// 的渠道上，请求会在**上游**那里失败（400 unsupported_model，从我们这边看
// 与「模型不存在」长得一样），而那条渠道自己看不出任何问题：status 是
// enabled、模型在 models 白名单里、别的渠道也在用同一份配置。
//
// 所以挡在写入侧。判据的另外两档（目录里没有这个模型、端点那一项没配过）
// 一律放行——目录是人工维护、会过时的一份声明，按「不支持」处理会让刚接的
// 上游一条渠道都建不出来。

// seedCatalogEndpoints 在模型目录里建一条记录并声明它的端点。
//
// endpoints 传 nil 表示那一列留 NULL（没配过 = 未知），传空切片表示
// `{}`（明确一条都不支持）——两者在**选路**里处置相反（前者放行、后者让任何
// 候选出局），所以夹具必须能把它们分开写出来；在**写入**这一侧它们也分开判
// （见 TestCreateAllowsModelsWithUnknownEndpoints 与
// TestCreateRejectsModelDeclaredOnNoEndpoint）。
func seedCatalogEndpoints(t *testing.T, db *sql.DB, name string, endpoints []channel.Endpoint) {
	t.Helper()
	if _, err := catalog.NewAdmin(db).Create(context.Background(), catalog.CreateModelRequest{
		CanonicalName: name,
		DisplayName:   name,
		Capabilities:  catalog.Capabilities{Endpoints: endpoints},
	}); err != nil {
		t.Fatalf("建目录记录失败: %v", err)
	}
}

// stub 渠道打的是 chat completions（见 stubAdapter），所以「对不上」的目录
// 声明就是一条不含 /v1/chat/completions 的清单。
func TestCreateRejectsModelOnUndeclaredEndpoint(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()
	seedCatalogEndpoints(t, db, model, []channel.Endpoint{channel.EndpointMessages})

	req := baseRequest("ch-endpoint-" + itoa(nextID()))
	req.Type = endpointChatType
	req.Models = []string{model}

	_, err := admin.Create(context.Background(), req)
	if err == nil {
		t.Fatal("目录说这个模型只在另一条端点上服务，Create 应当被拒绝")
	}
	// 报错要同时说清四件事，否则运维拿着这句话不知道该改哪一个：
	// 模型名、目录声明的端点、这条渠道的端点、这条渠道的 type。
	for _, want := range []string{
		model, string(channel.EndpointMessages), endpointChatType, string(channel.EndpointChatCompletions),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没出现 %q：%v", want, err)
		}
	}
	// 一个字段不对就整条不写。
	if n := countByName(t, db, req.Name); n != 0 {
		t.Errorf("被拒绝的 Create 留下了 %d 行", n)
	}
}

// 多协议渠道：模型挂在**任意一条**被声明的端点上就能被服务。
//
// 这一条是 EndpointFitForProtocols 的存在理由：只按主协议判的话，一条同时
// 说 openai 与 anthropic 的渠道会被「目录只声明了 /v1/messages」拒掉，
// 而它其实正好能经由 anthropic 那一条服务这个模型。
func TestCreateMultiProtocolAcceptsModelOnEitherEndpoint(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()
	seedCatalogEndpoints(t, db, model, []channel.Endpoint{channel.EndpointMessages})

	req := baseRequest("ch-multi-endpoint-" + itoa(nextID()))
	req.Type = endpointChatType // 主协议打 chat，目录里没有它
	req.Protocols = []string{endpointChatType, endpointMessagesType}
	req.Models = []string{model}

	if _, err := admin.Create(context.Background(), req); err != nil {
		t.Fatalf("模型在 anthropic 端点上有声明，多协议渠道应当可以创建: %v", err)
	}

	// 同样的模型、只声明 chat 的渠道仍然要被拒——判据没有放松成「多协议就放行」。
	single := baseRequest("ch-single-endpoint-" + itoa(nextID()))
	single.Type = endpointChatType
	single.Models = []string{model}
	if _, err := admin.Create(context.Background(), single); err == nil {
		t.Fatal("只声明 chat 的渠道仍然应当被拒")
	}
}

// `{}`（明确一条都不支持）也要拦：它不是「没配过」，而是「哪条渠道都服务
// 不了这个模型」。把两者合成一档会让这条声明静默地什么都不做。
func TestCreateRejectsModelDeclaredOnNoEndpoint(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()
	seedCatalogEndpoints(t, db, model, []channel.Endpoint{})

	req := baseRequest("ch-endpoint-none-" + itoa(nextID()))
	req.Type = endpointChatType
	req.Models = []string{model}

	_, err := admin.Create(context.Background(), req)
	if err == nil {
		t.Fatal("目录说这个模型一条端点都不支持，Create 应当被拒绝")
	}
	if !strings.Contains(err.Error(), model) {
		t.Errorf("报错里没提模型名：%v", err)
	}
}

func TestCreateAllowsModelsWithUnknownEndpoints(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	cases := []struct {
		name string
		seed func(model string)
	}{
		{"目录里没有这个模型", func(string) {}},
		{"目录里端点那一项没配过", func(model string) {
			seedCatalogEndpoints(t, db, model, nil)
		}},
		{"目录里声明了这条端点", func(model string) {
			seedCatalogEndpoints(t, db, model, []channel.Endpoint{channel.EndpointChatCompletions})
		}},
		{"目录里声明了两条，含这条", func(model string) {
			seedCatalogEndpoints(t, db, model, []channel.Endpoint{
				channel.EndpointMessages, channel.EndpointChatCompletions})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := uniqueModel()
			tc.seed(model)

			req := baseRequest("ch-allow-" + itoa(nextID()))
			req.Type = endpointChatType
			req.Models = []string{model}

			if _, err := admin.Create(context.Background(), req); err != nil {
				t.Fatalf("应当放行，得到: %v", err)
			}
		})
	}
}

// 只改 models（不带 type）同样要按**库里当前那条**类型校验：把一个模型加进
// 一条走别的端点的渠道，与建一条新渠道挂错模型是同一件事。
func TestUpdateRejectsModelOnUndeclaredEndpoint(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()

	req := baseRequest("ch-update-endpoint-" + itoa(nextID()))
	req.Type = endpointChatType
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	// 建完之后目录里才出现这个模型，且它只在另一条端点上被服务。
	seedCatalogEndpoints(t, db, model, []channel.Endpoint{channel.EndpointMessages})

	models := []string{model, "extra-" + model}
	err = admin.Update(context.Background(), UpdateRequest{ID: id, Models: &models})
	if err == nil {
		t.Fatal("只改 models 也要按当前类型校验端点")
	}
	for _, want := range []string{model, string(channel.EndpointMessages), endpointChatType} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没出现 %q：%v", want, err)
		}
	}

	// 被拒绝的修改一行都不动。
	if got := listInfo(t, admin, id).Models; !reflect.DeepEqual(got, req.Models) {
		t.Errorf("被拒绝的 Update 改了 models：%v，期望 %v", got, req.Models)
	}
}

// 一起改 type 与 models 时按**新的**类型判：从 anthropic（messages）换成
// openai（chat completions）之后，一个只在 messages 上被服务的模型就该被拦下。
//
// 判据用哪一侧的类型不能想当然：用库里那个旧的会把这次修改放过去，而放过去
// 之后这条渠道就一直在往一条错端点上发请求。
func TestUpdateJudgesByNewTypeWhenBothChange(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))
	model := uniqueModel()
	seedCatalogEndpoints(t, db, model, []channel.Endpoint{channel.EndpointMessages})

	// 先按 anthropic 建一条：端点对得上，放行。
	req := baseRequest("ch-update-type-" + itoa(nextID()))
	req.Type = endpointMessagesType
	req.Models = []string{model}
	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	// 换成 chat completions，模型不动——端点就对不上了。
	newType := endpointChatType
	models := []string{model}
	err = admin.Update(context.Background(), UpdateRequest{ID: id, Type: &newType, Models: &models})
	if err == nil {
		t.Fatal("换成一个端点对不上的类型应当被拒绝")
	}
	for _, want := range []string{model, string(channel.EndpointChatCompletions), endpointChatType} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没出现 %q：%v", want, err)
		}
	}
	// 类型也没被改掉：两列在同一个 SET 里，被拒绝时一行都不写。
	if got := listInfo(t, admin, id).Type; got != endpointMessagesType {
		t.Errorf("被拒绝的 Update 改了 type：%q", got)
	}
}

// 只改 type（不给 models）同样要把**已有的那批模型**重判一遍。
//
// 这条渠道会把它们从一条端点整体挪到另一条上，而光看这份改动看不出模型的
// 落点变了——它比「只改 models」更隐蔽：不需要动白名单，也不改模型的集合。
//
// 两臂都在：只留拒绝那一臂的话，它证明的只是「改 type 会被拒」，
// 证明不了判的是「端点对不对」。
func TestUpdateJudgesEndpointsWhenOnlyTypeChanges(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	t.Run("端点对得上时放行", func(t *testing.T) {
		model := uniqueModel()
		seedCatalogEndpoints(t, db, model, []channel.Endpoint{
			channel.EndpointMessages, channel.EndpointChatCompletions,
		})

		req := baseRequest("ch-type-only-ok-" + itoa(nextID()))
		req.Type = endpointMessagesType
		req.Models = []string{model}
		id, err := admin.Create(context.Background(), req)
		if err != nil {
			t.Fatalf("Create 出错: %v", err)
		}

		newType := endpointChatType
		if err := admin.Update(context.Background(), UpdateRequest{ID: id, Type: &newType}); err != nil {
			t.Fatalf("模型两条端点都支持，只改 type 应当放行: %v", err)
		}
		if got := listInfo(t, admin, id).Type; got != endpointChatType {
			t.Errorf("type 没改成功：%q", got)
		}
	})

	t.Run("端点对不上时拒绝", func(t *testing.T) {
		model := uniqueModel()
		seedCatalogEndpoints(t, db, model, []channel.Endpoint{channel.EndpointMessages})

		req := baseRequest("ch-type-only-bad-" + itoa(nextID()))
		req.Type = endpointMessagesType
		req.Models = []string{model}
		id, err := admin.Create(context.Background(), req)
		if err != nil {
			t.Fatalf("Create 出错: %v", err)
		}

		newType := endpointChatType
		err = admin.Update(context.Background(), UpdateRequest{ID: id, Type: &newType})
		if err == nil {
			t.Fatal("只改 type 就把已有模型挪到服务不了的端点上，应当被拒绝")
		}
		// 报错里要有模型名：这次的教训是「改动里根本没提这个模型」，
		// 不说出是哪一个，读的人无从下手。
		if !strings.Contains(err.Error(), model) {
			t.Errorf("报错里没出现模型名 %q：%v", model, err)
		}
		if got := listInfo(t, admin, id).Type; got != endpointMessagesType {
			t.Errorf("被拒绝的 Update 改了 type：%q", got)
		}
	})
}

// TestOwnedMethodsRejectNonPositiveIDs 钉住归属与渠道 id 的下界：0 是非法值，
// 不是「不限制」。
//
// 这几个判据都在碰库之前，但用例照样走真库：把 `<= 0` 放宽成 `< 0` 之后，
// 调用会继续往下跑到 SQL，那一侧的回答是 ErrChannelNotFound（或者一个空
// 清单），与这里期望的「id 无效」不是同一句话——只用 panic 或「非 nil 错误」
// 判的话，两者分不开。
//
// 三处等价变异不在这里补用例，也不为它们改实现：
//
//   - admin.go 的 UPDATE 里第二道归属条件：checkOwner 已经判过一次，而今天
//     没有改 owner_user_id 的写路径，所以「判完到写之间换了主人」造不出来。
//   - channels.go 的 consecutiveFailures 首个 `streak <= 0`：写入侧把计数与
//     时间戳一起写，库里不存在「负计数 + 新鲜时间戳」这种行。
func TestOwnedMethodsRejectNonPositiveIDs(t *testing.T) {
	admin := NewAdmin(testDB(t), testSealer(t), nil)
	ctx := context.Background()
	req := UpdateRequest{ID: 1}

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "UpdateOwned 归属为 0",
			call: func() error { return admin.UpdateOwned(ctx, 0, req) },
			want: "用户 id 无效",
		},
		{
			name: "GetOwned 归属为 0",
			call: func() error { _, err := admin.GetOwned(ctx, 0, 1); return err },
			want: "用户 id 无效",
		},
		{
			name: "GetOwned 渠道为 0",
			call: func() error { _, err := admin.GetOwned(ctx, 1, 0); return err },
			want: "渠道 id 无效",
		},
		{
			name: "ListOwned 归属为 0",
			call: func() error { _, err := admin.ListOwned(ctx, 0); return err },
			want: "用户 id 无效",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("错误为 nil，期望 %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}
