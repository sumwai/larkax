package channels

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
)

// ── 测试台 ──────────────────────────────────────────────────────────────

// stubAdapter 是注册表里占位用的最小实现：写入路径只问「这个类型有人实现吗」，
// 不问它怎么发请求。
type stubAdapter struct{ typ string }

func (s *stubAdapter) Type() string               { return s.typ }
func (s *stubAdapter) Protocol() channel.Protocol { return channel.ProtocolOpenAI }
func (s *stubAdapter) Chat(context.Context, channel.Channel, *channel.Request) (*channel.Response, error) {
	return nil, channel.ErrUnsupported
}

func (s *stubAdapter) ChatStream(context.Context, channel.Channel, *channel.Request) (<-chan channel.StreamEvent, error) {
	return nil, channel.ErrUnsupported
}

func (s *stubAdapter) ListModels(context.Context, channel.Channel) ([]string, error) {
	return nil, channel.ErrUnsupported
}

func (s *stubAdapter) Quota(context.Context, channel.Channel) (channel.Quota, error) {
	return channel.Quota{}, channel.ErrUnsupported
}

// stubType 是注册表里会有的那个类型。用它而不是真实适配器的类型名：
// 这一层测的是「类型经注册表校验」，与某个具体适配器无关。
const stubType = "stub-adapter"

func testRegistry(t *testing.T) *channel.Registry {
	t.Helper()
	r := channel.NewRegistry()
	if err := r.Register(&stubAdapter{typ: stubType}); err != nil {
		t.Fatalf("注册 stub 适配器失败: %v", err)
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

// rawCredentialEnc 直接读库里的密文列，用来断言「写入侧到底存了什么」。
func rawCredentialEnc(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	var enc []byte
	if err := db.QueryRowContext(context.Background(),
		`SELECT credential_enc FROM channels WHERE id = $1`, id).Scan(&enc); err != nil {
		t.Fatalf("读 credential_enc 失败: %v", err)
	}
	return enc
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

// 各种字段校验失败都要在**落库之前**停下，且报错不回显字段值。
func TestCreateRejectsInvalidFields(t *testing.T) {
	db := testDB(t)
	admin := newAdmin(t, db, testSealer(t))

	cases := []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{"渠道名为空", func(r *CreateRequest) { r.Name = "   " }},
		{"凭证为空", func(r *CreateRequest) { r.Credential = "" }},
		{"models 为空", func(r *CreateRequest) { r.Models = nil }},
		{"models 里有空项", func(r *CreateRequest) { r.Models = []string{"a", " "} }},
		{"base_url 为空", func(r *CreateRequest) { r.BaseURL = "" }},
		{"base_url 没有 scheme", func(r *CreateRequest) { r.BaseURL = "api.example.com/v1" }},
		{"base_url 不是 http", func(r *CreateRequest) { r.BaseURL = "ftp://api.example.com/v1" }},
		{"base_url 没有主机名", func(r *CreateRequest) { r.BaseURL = "https:///v1" }},
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

// 外键约束（owner_user_id 指向不存在的用户）要原样报出来，不能被吞掉。
//
// channels 表上没有别的唯一键：**渠道名不是唯一的**（见 00001_init.sql，
// 只有 owner 与 status 两条普通索引），所以没有「名字冲突」这条路径可测。
// 数据库层的完整性失败就只剩这一条外键。
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
		"ID", "OwnerUserID", "Name", "Type", "BaseURL", "Status", "Models",
		"Weight", "Priority", "Setting", "CreatedAt", "UpdatedAt",
		"CredentialKeys", "CredentialErr",
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

	broken := insertChannel(t, db, sealer, channelSpec{
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
