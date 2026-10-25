package channels

// 本文件是「拉取上游模型清单并落库」的写入侧。
//
// # 出口为什么在这里
//
// 拉这份清单要用**明文**上游凭证，而全仓库解明文的地方只该在 channels 包内
// （见包注释与 Admin 的说明）。所以命令不自己解密，而是把渠道 id 交给这里：
// 本类型自己从库里取密文、解密、按渠道类型找适配器、调用、落库。明文在
// 本文件里的生命周期只有一次适配器调用，不进日志、不进错误信息、不进返回值。
//
// # 它不参与选路
//
// 落库的是上游自己的模型名，不是 channels.models 里的规范名；路由与展示都
// 不读这两张表（见 00043 的注释）。这里只记下一份可查的事实，要不要把它变成
// 别名、要不要写进渠道的模型白名单，仍是人的决定。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
)

// 同步状态的取值。
//
// ModelSyncSynced / ModelSyncUnsupported 是库里的取值；ModelSyncNever 只出现
// 在读回的结果里，表示 channel_model_syncs 没有这一行——「从未同步过」不落库，
// 否则「没配过」与「同步过、结果是空」在库里会长得一样。
const (
	ModelSyncSynced      = "synced"
	ModelSyncUnsupported = "unsupported"
	ModelSyncNever       = "never"
)

// ModelSync 从上游拉取某条渠道的模型清单并落库。
//
// 它需要主密钥（解密凭证）与适配器注册表（按渠道类型分发）。读侧
// （StoredModels）两样都不用，构造时传 nil 即可。
type ModelSync struct {
	db       *sql.DB
	sealer   *credential.Sealer
	registry *channel.Registry
}

// NewModelSync 包装数据库、凭证封装器与适配器注册表。
func NewModelSync(db *sql.DB, sealer *credential.Sealer, registry *channel.Registry) *ModelSync {
	return &ModelSync{db: db, sealer: sealer, registry: registry}
}

// StoredModel 是库里记下的一条上游模型。
type StoredModel struct {
	// UpstreamModel 是上游自己的模型名，原样保存。
	UpstreamModel string
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
}

// ChannelModels 是某条渠道的模型清单与同步状态。
type ChannelModels struct {
	ChannelID   int64
	ChannelName string
	// Status 是 ModelSyncSynced / ModelSyncUnsupported / ModelSyncNever。
	Status string
	// AttemptedAt 是最近一次尝试的时刻；从未同步过时为零值。
	AttemptedAt time.Time
	// SyncedAt 是最近一次**成功**同步的时刻；从未成功过时为零值。
	SyncedAt time.Time
	// Models 是库里记着的清单，按模型名排序。从未同步过时是空切片。
	Models []StoredModel
}

// ModelSyncResult 是一次同步的结果。
//
// 它是**这一次做了什么**，不是库里的全貌：要全貌就读 StoredModels。
type ModelSyncResult struct {
	ChannelID   int64
	ChannelName string
	// Status 是 ModelSyncSynced 或 ModelSyncUnsupported。上游不支持时命令仍然
	// 算成功——「这家没有这个接口」是正常情况，不是故障。
	Status string
	// Added / Removed 是这一次相对上一次的增删，Total 是这次拉到的条数。
	// 连续同步两次且清单没变时，前两个都是空、Total 不变。
	Added   []string
	Removed []string
	Total   int
	// AttemptedAt / SyncedAt 与 ChannelModels 上那两栏同义。
	AttemptedAt time.Time
	SyncedAt    time.Time
}

// Sync 拉取 channelID 的上游模型清单并落库。
//
// # 三档结果
//
//   - 拉到清单：整份替换这条渠道的清单（不在新清单里的名字删掉），
//     同步状态记为 synced。空清单也是一份清单，会把旧行全部删掉。
//   - 适配器返回 ErrUnsupported：状态记为 unsupported，**已有清单一行不动**。
//     「上游不提供这个接口」不构成「清单是空的」的证据。
//   - 其他错误：原样返回，库里什么都不改。
//
// # 幂等
//
// 行按 (channel_id, upstream_model) 唯一。同一份清单再来一次不会多出行，
// 只会把每行的 last_seen_at 刷新到这一次；first_seen_at 保留第一次见到的时刻。
// 于是「这份清单里有哪些名字」由行本身回答，「它们还在不在」由 last_seen_at
// 回答。
func (m *ModelSync) Sync(ctx context.Context, channelID int64) (ModelSyncResult, error) {
	if channelID <= 0 {
		return ModelSyncResult{}, fmt.Errorf("渠道 id 无效：%d", channelID)
	}
	if m.registry == nil {
		return ModelSyncResult{}, errors.New("没有适配器注册表，无法按渠道类型拉取模型清单")
	}

	var (
		name          string
		chanType      string
		baseURL       string
		protocolsJSON string
		envelope      []byte
		deletedAt     sql.NullTime
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT c.name, c.type, c.base_url,
		       `+protocolsColumnSQL+`,
		       a.credential_enc, c.deleted_at
		FROM channels c
		LEFT JOIN LATERAL (
		      SELECT ac.credential_enc
		        FROM channel_accounts ca
		        JOIN accounts ac ON ac.id = ca.account_id
		       WHERE ca.channel_id = c.id
		       ORDER BY ac.id
		       LIMIT 1
		     ) a ON true
		WHERE c.id = $1`, channelID).
		Scan(&name, &chanType, &baseURL, &protocolsJSON, &envelope, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelSyncResult{}, fmt.Errorf("%w：id %d", ErrChannelNotFound, channelID)
	}
	if err != nil {
		return ModelSyncResult{}, fmt.Errorf("读取渠道 %d 失败: %w", channelID, err)
	}
	if deletedAt.Valid {
		// 与 update / enable / disable 同一条口径：软删的渠道不再接受写入。
		return ModelSyncResult{}, fmt.Errorf("%w：渠道 %d", ErrChannelDeleted, channelID)
	}
	if len(envelope) == 0 {
		return ModelSyncResult{}, fmt.Errorf("%w：渠道 %d", ErrNoAccount, channelID)
	}

	adapter, err := m.registry.Lookup(chanType)
	if err != nil {
		return ModelSyncResult{}, fmt.Errorf("渠道 %d: %w", channelID, err)
	}
	plaintext, err := openCredential(m.sealer, envelope)
	if err != nil {
		return ModelSyncResult{}, fmt.Errorf("渠道 %d: %w", channelID, err)
	}

	// 同步走这条渠道**主协议**的地址：type 指向哪个适配器，就打哪个协议的
	// 地址。判据与执行器、探测共用 channel.ProtocolBaseURL。
	_, protocolBaseURLs, decodeErr := decodeProtocols(channelID, protocolsJSON)
	if decodeErr != nil {
		return ModelSyncResult{}, decodeErr
	}
	models, err := adapter.ListModels(ctx, channel.Channel{
		ID:         channelID,
		Name:       name,
		Type:       chanType,
		BaseURL:    channel.ProtocolBaseURL(channel.Protocol(chanType), protocolBaseURLs, baseURL),
		Credential: plaintext,
	})
	if errors.Is(err, channel.ErrUnsupported) {
		return m.recordUnsupported(ctx, channelID, name)
	}
	if err != nil {
		return ModelSyncResult{}, fmt.Errorf("渠道 %d 拉取上游模型清单失败: %w", channelID, err)
	}

	return m.recordSynced(ctx, channelID, name, models)
}

// recordUnsupported 记下「上游不提供模型列表」。
//
// 已有清单与它的同步时刻都不动：那一份清单仍然是我们最后一次真正看到的事实，
// 而这次尝试发生在更晚。attempted_at 因此与 synced_at 可以指向两个不同的时刻。
func (m *ModelSync) recordUnsupported(ctx context.Context, channelID int64, name string) (ModelSyncResult, error) {
	var (
		attemptedAt time.Time
		syncedAt    sql.NullTime
	)
	err := m.db.QueryRowContext(ctx, `
		INSERT INTO channel_model_syncs (channel_id, status, attempted_at, synced_at)
		VALUES ($1, 'unsupported', now(), NULL)
		ON CONFLICT (channel_id) DO UPDATE
			SET status = 'unsupported', attempted_at = now()
		RETURNING attempted_at, synced_at`, channelID).
		Scan(&attemptedAt, &syncedAt)
	if err != nil {
		return ModelSyncResult{}, fmt.Errorf("记录渠道 %d 的同步状态失败: %w", channelID, err)
	}

	var total int
	if err := m.db.QueryRowContext(ctx,
		`SELECT count(*) FROM channel_upstream_models WHERE channel_id = $1`, channelID).
		Scan(&total); err != nil {
		return ModelSyncResult{}, fmt.Errorf("读取渠道 %d 的已有清单失败: %w", channelID, err)
	}

	return ModelSyncResult{
		ChannelID:   channelID,
		ChannelName: name,
		Status:      ModelSyncUnsupported,
		Total:       total,
		AttemptedAt: attemptedAt,
		SyncedAt:    syncedAt.Time,
	}, nil
}

// recordSynced 把一次成功拉到的清单整份写进库里。
func (m *ModelSync) recordSynced(ctx context.Context, channelID int64, name string, models []string) (ModelSyncResult, error) {
	// 去重与排序都发生在落库之前：上游偶尔会回重复的 id，而「同一份清单
	// 两次同步得到同样多的行」是幂等的判据之一。排序让增删清单与输出稳定，
	// 断言也就不必跟着上游的返回顺序走。
	seen := make(map[string]struct{}, len(models))
	names := make([]string, 0, len(models))
	for _, model := range models {
		if model == "" {
			continue
		}
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		names = append(names, model)
	}
	sort.Strings(names)

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelSyncResult{}, fmt.Errorf("开启同步渠道 %d 的事务失败: %w", channelID, err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := storedNames(ctx, tx, channelID)
	if err != nil {
		return ModelSyncResult{}, err
	}

	added := make([]string, 0)
	for _, model := range names {
		if _, ok := existing[model]; !ok {
			added = append(added, model)
		}
	}
	removed := make([]string, 0)
	for model := range existing {
		if _, ok := seen[model]; !ok {
			removed = append(removed, model)
		}
	}
	sort.Strings(removed)

	for _, model := range names {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_upstream_models (channel_id, upstream_model)
			VALUES ($1, $2)
			ON CONFLICT (channel_id, upstream_model) DO UPDATE
				SET last_seen_at = now()`, channelID, model); err != nil {
			return ModelSyncResult{}, fmt.Errorf("写入渠道 %d 的模型 %q 失败: %w", channelID, model, err)
		}
	}
	for _, model := range removed {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM channel_upstream_models
			WHERE channel_id = $1 AND upstream_model = $2`, channelID, model); err != nil {
			return ModelSyncResult{}, fmt.Errorf("删除渠道 %d 的模型 %q 失败: %w", channelID, model, err)
		}
	}

	var (
		attemptedAt time.Time
		syncedAt    time.Time
	)
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO channel_model_syncs (channel_id, status, attempted_at, synced_at)
		VALUES ($1, 'synced', now(), now())
		ON CONFLICT (channel_id) DO UPDATE
			SET status = 'synced', attempted_at = now(), synced_at = now()
		RETURNING attempted_at, synced_at`, channelID).
		Scan(&attemptedAt, &syncedAt); err != nil {
		return ModelSyncResult{}, fmt.Errorf("记录渠道 %d 的同步状态失败: %w", channelID, err)
	}

	if err := tx.Commit(); err != nil {
		return ModelSyncResult{}, fmt.Errorf("提交渠道 %d 的同步失败: %w", channelID, err)
	}

	return ModelSyncResult{
		ChannelID:   channelID,
		ChannelName: name,
		Status:      ModelSyncSynced,
		Added:       added,
		Removed:     removed,
		Total:       len(names),
		AttemptedAt: attemptedAt,
		SyncedAt:    syncedAt,
	}, nil
}

// storedNames 读出这条渠道当前记着的模型名集合。
func storedNames(ctx context.Context, tx *sql.Tx, channelID int64) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT upstream_model FROM channel_upstream_models WHERE channel_id = $1`, channelID)
	if err != nil {
		return nil, fmt.Errorf("读取渠道 %d 的已有清单失败: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]struct{})
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("读取渠道 %d 的已有清单失败: %w", channelID, err)
		}
		out[model] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取渠道 %d 的已有清单失败: %w", channelID, err)
	}
	return out, nil
}

// StoredModels 读回一条渠道的上游模型清单与同步状态。
//
// 已软删的渠道照读：清单是一份历史事实，删除渠道不等于它从没同步过。
// 渠道整个不存在时才报 ErrChannelNotFound。
func (m *ModelSync) StoredModels(ctx context.Context, channelID int64) (ChannelModels, error) {
	if channelID <= 0 {
		return ChannelModels{}, fmt.Errorf("渠道 id 无效：%d", channelID)
	}

	out := ChannelModels{
		ChannelID: channelID,
		Status:    ModelSyncNever,
		Models:    []StoredModel{},
	}
	err := m.db.QueryRowContext(ctx, `SELECT name FROM channels WHERE id = $1`, channelID).
		Scan(&out.ChannelName)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelModels{}, fmt.Errorf("%w：id %d", ErrChannelNotFound, channelID)
	}
	if err != nil {
		return ChannelModels{}, fmt.Errorf("读取渠道 %d 失败: %w", channelID, err)
	}

	var (
		status      string
		attemptedAt sql.NullTime
		syncedAt    sql.NullTime
	)
	err = m.db.QueryRowContext(ctx, `
		SELECT status, attempted_at, synced_at
		FROM channel_model_syncs
		WHERE channel_id = $1`, channelID).
		Scan(&status, &attemptedAt, &syncedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 从未同步过：上面那几栏保持零值。
	case err != nil:
		return ChannelModels{}, fmt.Errorf("读取渠道 %d 的同步状态失败: %w", channelID, err)
	default:
		out.Status = status
		out.AttemptedAt = attemptedAt.Time
		out.SyncedAt = syncedAt.Time
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT upstream_model, first_seen_at, last_seen_at
		FROM channel_upstream_models
		WHERE channel_id = $1
		ORDER BY upstream_model`, channelID)
	if err != nil {
		return ChannelModels{}, fmt.Errorf("读取渠道 %d 的模型清单失败: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var model StoredModel
		if err := rows.Scan(&model.UpstreamModel, &model.FirstSeenAt, &model.LastSeenAt); err != nil {
			return ChannelModels{}, fmt.Errorf("读取渠道 %d 的模型清单失败: %w", channelID, err)
		}
		out.Models = append(out.Models, model)
	}
	if err := rows.Err(); err != nil {
		return ChannelModels{}, fmt.Errorf("读取渠道 %d 的模型清单失败: %w", channelID, err)
	}
	return out, nil
}
