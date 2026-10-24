package channels

// 本文件是模型探测里**只有真库才能证明**的那一半：给了渠道 id 时从库里
// 取类型、地址与凭证，覆盖只发生在归属判据之后，软删的渠道当作不存在。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/credential"
)

// insertProbeChannel 插一条探测用的渠道：类型与地址都由调用方给。
//
// 与 insertChannel 分开：那个夹具写死 type='openai-compatible' 与一个
// 固定的地址（健康度那批用例要的是「同一种渠道」），而探测要的正是
// 「类型、地址、凭证各是什么」。
func insertProbeChannel(t *testing.T, db *sql.DB, sealer *credential.Sealer,
	owner int64, chanType, baseURL, plaintext string,
) int64 {
	t.Helper()
	envelope, err := sealer.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("封装凭证失败: %v", err)
	}
	var ownerID any
	if owner != 0 {
		ownerID = owner
	}
	var id int64
	err = db.QueryRowContext(context.Background(), `
		INSERT INTO channels (owner_user_id, name, type, base_url, status, models)
		VALUES ($1, $2, $3, $4, 'enabled', ARRAY['probe-model'])
		RETURNING id`,
		ownerID, "probe-"+itoa(nextID()), chanType, baseURL).Scan(&id)
	if err != nil {
		t.Fatalf("插入探测渠道失败: %v", err)
	}
	// 凭证在账号上（见 00067），探测必须先把它取出来再打上游。
	var accountID int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO accounts (name, credential_enc, credential_hint)
		VALUES ($1, $2, $3) RETURNING id`,
		"probe-acc-"+itoa(nextID()), envelope, credential.Hint(plaintext)).Scan(&accountID); err != nil {
		t.Fatalf("插入探测账号失败: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_accounts (channel_id, account_id) VALUES ($1, $2)`,
		id, accountID); err != nil {
		t.Fatalf("把探测账号挂到渠道上失败: %v", err)
	}
	return id
}

// TestProbeChannelIDForm 给了 id 时的四条：按库里的类型与凭证打、
// 地址与凭证都能局部覆盖、别人的渠道与软删除的渠道都当作不存在。
func TestProbeChannelIDForm(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	owner := seedMerchant(t, db)
	other := seedMerchant(t, db)

	adapter := &listingAdapter{models: []string{"m1"}}
	probe := NewModelProbe(db, sealer, probeRegistryWith(t, endpointChatType, adapter))

	const storedKey = "stored-key-placeholder"
	id := insertProbeChannel(t, db, sealer, owner, endpointChatType,
		"https://api.example.com/v1", storedKey)

	// 只给 id：类型、地址、凭证全从库里来。
	res, err := probe.Probe(t.Context(), ProbeRequest{ChannelID: id, OwnerID: &owner})
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if !res.Supported || len(res.Models) != 1 {
		t.Errorf("结果 = %+v，期望一条模型", res)
	}
	if adapter.got.Type != endpointChatType || adapter.got.BaseURL != "https://api.example.com/v1" {
		t.Errorf("适配器拿到的渠道 = %+v，期望库里的类型与地址", adapter.got)
	}
	if adapter.got.Credential != storedKey {
		t.Errorf("适配器拿到的凭证 = %q，期望库里的那一把", adapter.got.Credential)
	}

	// 覆盖地址与凭证：编辑中的表单还没保存，探测要能打新值。
	_, err = probe.Probe(t.Context(), ProbeRequest{
		ChannelID:  id,
		OwnerID:    &owner,
		BaseURL:    "https://new.example.com/v1/",
		Credential: "new-key-placeholder",
	})
	if err != nil {
		t.Fatalf("带覆盖的探测失败: %v", err)
	}
	// NormalizeBaseURL 只校验、不改写：结尾斜杠原样留着，拼接由 httpx.Endpoint
	// 统一处理。这里钉住「没有偷偷改写用户的地址」。
	if adapter.got.BaseURL != "https://new.example.com/v1/" {
		t.Errorf("覆盖后的地址 = %q，期望提交的新地址原样传下去", adapter.got.BaseURL)
	}
	if adapter.got.Credential != "new-key-placeholder" {
		t.Errorf("覆盖后的凭证 = %q，期望提交的那一把", adapter.got.Credential)
	}

	// 别人的渠道：按不存在处理，覆盖凭证也不能成为绕过归属的入口——
	// 适配器一次都不该被打到。
	callsBefore := adapter.gotCall
	_, err = probe.Probe(t.Context(), ProbeRequest{
		ChannelID: id, OwnerID: &other, Credential: "attacker-placeholder",
	})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("别人的渠道 = %v，期望 ErrChannelNotFound", err)
	}
	if adapter.gotCall != callsBefore {
		t.Error("归属判据之前就把凭证交给了适配器")
	}

	// 软删的渠道：与不存在同一件事。
	if _, execErr := db.ExecContext(t.Context(),
		`UPDATE channels SET deleted_at = now() WHERE id = $1`, id); execErr != nil {
		t.Fatalf("软删渠道失败: %v", execErr)
	}
	_, err = probe.Probe(t.Context(), ProbeRequest{ChannelID: id, OwnerID: &owner})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("软删的渠道 = %v，期望 ErrChannelNotFound", err)
	}

	// 不存在：db 查不到行时也要给同一个哨兵。
	_, err = probe.Probe(t.Context(), ProbeRequest{ChannelID: id + 1000000, OwnerID: &owner})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("不存在的渠道 = %v，期望 ErrChannelNotFound", err)
	}
}

// TestProbeOwnershipBeforeDecryption 钉住归属判据在解密**之前**。
//
// 现有两条用例证明的是「适配器一次都没被打到」，那在归属判据挪到解密之后
// 仍然成立。真正的差别只出现在「别人的渠道 + 本进程没有主密钥」这一格：
// 现在回 ErrChannelNotFound（404），挪了以后会先解不开密文、回
// ErrNoMasterKey（503）——那条 503 等于告诉调用方「这一行存在」。
func TestProbeOwnershipBeforeDecryption(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	owner := seedMerchant(t, db)
	other := seedMerchant(t, db)

	adapter := &listingAdapter{models: []string{"m1"}}
	id := insertProbeChannel(t, db, sealer, owner, endpointChatType,
		"https://api.example.com/v1", "stored-key-placeholder")

	// 本进程**没有**主密钥：sealer 传 nil。归属判据若在解密之后，这里会
	// 先报 ErrNoMasterKey。
	probe := NewModelProbe(db, nil, probeRegistryWith(t, endpointChatType, adapter))
	_, err := probe.Probe(t.Context(), ProbeRequest{ChannelID: id, OwnerID: &other})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("别人的渠道 = %v，期望 ErrChannelNotFound（归属判据在解密之前）", err)
	}
	if errors.Is(err, ErrNoMasterKey) {
		t.Errorf("错误链里有 ErrNoMasterKey：解密发生在归属判据之前，503 泄漏了这一行存在")
	}
	if adapter.gotCall != 0 {
		t.Error("归属判据之前就把凭证交给了适配器")
	}
}

// TestProbeWithoutDatabase 给了 id 却没有库连接：报错而不是 panic。
func TestProbeWithoutDatabase(t *testing.T) {
	probe := NewModelProbe(nil, nil, probeRegistryWith(t, endpointChatType, &listingAdapter{}))
	_, err := probe.Probe(t.Context(), ProbeRequest{ChannelID: 1})
	if err == nil || !strings.Contains(err.Error(), "数据库") {
		t.Errorf("错误 = %v，期望说明没有数据库连接", err)
	}
}
