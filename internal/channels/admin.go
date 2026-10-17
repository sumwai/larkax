package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
)

// 渠道状态里运维能设置的两个取值。
//
// 表里的 CHECK 还允许 'drained'，但它**不在**这里：那是观测到的状态
// （上游说自己没额度了，由额度监控写入），不是运维的决定。让管理面能把它设上，
// 等于允许把一条观测记录伪造成配置。
const (
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
)

// ErrChannelNotFound 表示要改的那条渠道不在库里。
//
// 与「改不动」分开：前者是 id 写错了或者那条已经被删了，后者是数据库的问题。
// 没有这个区分的话，一次拼错 id 的调用会静默成功，而人以为改完了。
var ErrChannelNotFound = errors.New("渠道不存在")

// ErrNoChanges 表示这次修改没有任何字段要改。
//
// 单独报错而不是发一条只动 updated_at 的 UPDATE：后者会让「调用方拼错了
// 结构体」看起来像一次成功的修改。
var ErrNoChanges = errors.New("没有要改的字段")

// Admin 是渠道的管理面：创建、修改、列举、启停。
//
// 它不在请求热路径上，也不改变 Source.Candidates 的行为——两者读的是同一套列，
// 由同一批辅助函数解码，所以「写进去的」与「读出来的」不会各自漂移。
//
// # 为什么是单独一个类型，而不是挂在 Source 上
//
// Source 在请求热路径上——每个请求都会调 Candidates。管理面只在运维动作里
// 被调用，而它需要的东西（适配器注册表）读路径根本不需要。分成两个类型之后，
// 热路径那个类型**拿不到**写入能力：这是编译期的事实，不是一句约定。
//
// # 凭证只封不开
//
// 本类型不调用、也不该调用 credential.Sealer.Open。全仓库唯一解出明文凭证的
// 地方是 Source.decrypt，那是调用上游时的必需步骤；写入侧只需要 Seal。
// 每多一处能拿到明文的地方，日志与错误信息的去向就更难控制。
//
// 明文在这里的生命周期只有三步：交给 Seal → 拿到密文 → 落库。它不进日志、
// 不进错误信息、不进返回值；报错一律说「哪个字段不对」，不回显值。
type Admin struct {
	db       *sql.DB
	sealer   *credential.Sealer
	registry *channel.Registry
}

// NewAdmin 包装数据库句柄、主密钥封装器与适配器注册表。
//
// registry 为 nil 时创建与改类型都会报错，而不是 panic：一个拼错构造参数的
// 管理面调用不该把进程带走。
func NewAdmin(db *sql.DB, sealer *credential.Sealer, registry *channel.Registry) *Admin {
	return &Admin{db: db, sealer: sealer, registry: registry}
}

// CreateRequest 是新建一条渠道所需的全部字段。
type CreateRequest struct {
	// OwnerUserID 为 0 表示平台自营，库里存 NULL（route 把两者都看成 0）。
	OwnerUserID int64

	Name string
	// Type 必须是一个已注册的适配器类型，见 internal/channel 的注册表。
	Type    string
	BaseURL string

	// Credential 是**明文**凭证。它只用于封装，不落日志、不进错误信息、
	// 不进返回值。
	Credential string

	// Models 是该渠道支持的规范模型名，至少要有一个。
	Models []string

	// Weight 为 0 表示用默认值 1。
	Weight int

	Priority int
	Setting  map[string]any
}

// Create 新建一条渠道，返回它的 id。
//
// 新建的渠道一律是 enabled：读取路径只取 enabled 的行，一条建出来就不可选的
// 渠道除了占位没有别的用途。要建完立刻停用，就再调一次 SetStatus。
func (a *Admin) Create(ctx context.Context, req CreateRequest) (int64, error) {
	name, err := normalizeName(req.Name)
	if err != nil {
		return 0, err
	}
	if err = a.validateType(req.Type); err != nil {
		return 0, err
	}
	baseURL, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		return 0, err
	}
	models, err := normalizeModels(req.Models)
	if err != nil {
		return 0, err
	}
	weight, err := normalizeWeight(req.Weight)
	if err != nil {
		return 0, err
	}
	setting, err := encodeSetting(req.Setting)
	if err != nil {
		return 0, err
	}
	// 封凭证放在所有校验之后：校验失败时这次调用根本没碰到明文。
	envelope, err := a.sealCredential(req.Credential)
	if err != nil {
		return 0, err
	}

	// owner_user_id 是外键且可空。用 any 传值而不是传 0：
	// 0 会去匹配 id 为 0 的用户，而那个用户不存在——外键会直接拒绝，
	// 于是「平台自营」这条路径根本走不通。
	var owner any
	if req.OwnerUserID != 0 {
		owner = req.OwnerUserID
	}

	var id int64
	err = a.db.QueryRowContext(ctx, `
		INSERT INTO channels
			(owner_user_id, name, type, base_url, credential_enc, credential_hint,
			 status, models, weight, priority, setting)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		RETURNING id`,
		owner, name, req.Type, baseURL, envelope, credential.Hint(req.Credential),
		StatusEnabled, models, weight, req.Priority, string(setting)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("创建渠道失败: %w", err)
	}
	return id, nil
}

// UpdateRequest 描述一次修改。
//
// 每个字段都是指针：nil 表示「不改这一项」，非 nil 表示「改成这个值」。
// 用指针而不是零值是因为零值本身有意义——priority 改成 0、setting 改成空对象
// 都是合法修改，与「没提到这个字段」必须区分开。
type UpdateRequest struct {
	ID int64

	Name     *string
	Type     *string
	BaseURL  *string
	Models   *[]string
	Weight   *int
	Priority *int
	Setting  *map[string]any

	// Credential 非 nil 时重新封装并替换密文。
	//
	// **为 nil 时生成的 SQL 里根本不出现 credential_enc**，密文一个字节都不动。
	// 这一条是有意的：重封一次会换掉 DEK，密文变了而凭证没变，事后审计时
	// 「这条渠道的凭证什么时候换过」就再也答不出来。
	Credential *string
}

// Update 按 UpdateRequest 修改一条渠道。
func (a *Admin) Update(ctx context.Context, req UpdateRequest) error {
	if req.ID <= 0 {
		return errors.New("渠道 id 无效")
	}

	// 只把真正要改的列拼进 SET：没提到的列连现在是什么值都不必知道。
	var sets []string
	var args []any
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}

	if req.Name != nil {
		name, err := normalizeName(*req.Name)
		if err != nil {
			return err
		}
		add("name", name)
	}
	if req.Type != nil {
		if err := a.validateType(*req.Type); err != nil {
			return err
		}
		add("type", *req.Type)
	}
	if req.BaseURL != nil {
		baseURL, err := normalizeBaseURL(*req.BaseURL)
		if err != nil {
			return err
		}
		add("base_url", baseURL)
	}
	if req.Models != nil {
		models, err := normalizeModels(*req.Models)
		if err != nil {
			return err
		}
		add("models", models)
	}
	if req.Weight != nil {
		weight, err := normalizeWeight(*req.Weight)
		if err != nil {
			return err
		}
		add("weight", weight)
	}
	if req.Priority != nil {
		add("priority", *req.Priority)
	}
	if req.Setting != nil {
		setting, err := encodeSetting(*req.Setting)
		if err != nil {
			return err
		}
		add("setting", string(setting))
	}
	if req.Credential != nil {
		envelope, err := a.sealCredential(*req.Credential)
		if err != nil {
			return err
		}
		add("credential_enc", envelope)
		// 提示与密文一起换：留着旧提示会让清单上的「末四位」指向
		// 一把已经不在这条渠道上的凭证——界面拿它做辨识，而它认错了人。
		add("credential_hint", credential.Hint(*req.Credential))
	}

	if len(sets) == 0 {
		return ErrNoChanges
	}
	// updated_at 每次都动，于是「改了几个字段」不影响这条语句的行为，
	// 下面也就能用影响行数判断这条渠道在不在。
	sets = append(sets, "updated_at = now()")
	args = append(args, req.ID)

	query := "UPDATE channels SET " + strings.Join(sets, ", ") +
		" WHERE id = $" + strconv.Itoa(len(args))
	res, err := a.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("修改渠道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认修改结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：id %d", ErrChannelNotFound, req.ID)
	}
	return nil
}

// SetStatus 启用或停用一条渠道。
//
// 只接受 StatusEnabled 与 StatusDisabled，见那两个常量的说明。
func (a *Admin) SetStatus(ctx context.Context, id int64, status string) error {
	if id <= 0 {
		return errors.New("渠道 id 无效")
	}
	if status != StatusEnabled && status != StatusDisabled {
		return fmt.Errorf("渠道状态 %q 不是管理面能设置的取值（只接受 %q 与 %q）",
			status, StatusEnabled, StatusDisabled)
	}

	res, err := a.db.ExecContext(ctx,
		`UPDATE channels SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("设置渠道状态失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认设置结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w：id %d", ErrChannelNotFound, id)
	}
	return nil
}

// ChannelInfo 是管理面看到的一条渠道。**它不含密文，也不含明文凭证。**
//
// 字段集合由 admin_test.go 的一条用例逐个钉住：加字段会先让那条用例失败，
// 于是「这个新字段该不该出现在管理面的返回值里」会被问一次。
// 这正是本类型存在的意义——列举的目的是核对配置，不是把密钥搬来搬去。
type ChannelInfo struct {
	ID          int64
	OwnerUserID int64
	Name        string
	Type        string
	BaseURL     string
	Status      string
	Models      []string
	Weight      int
	Priority    int
	Setting     map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// CredentialKeys 是这份密文被哪些主密钥包过，见 credential.WrappedWith。
	//
	// 它回答的是运维真正会问的问题：「这条渠道挂在哪把钥匙下、轮换补到哪一步了」。
	// 列出它不需要任何密钥材料，也不需要解密。
	CredentialKeys []string

	// Hint 是凭证的末四位（见 credential.Hint），供界面辨识。
	//
	// 它与 CredentialKeys 回答的是两个不同的问题：那一栏说「挂在哪把钥匙下」，
	// 这一栏说「挂的是我刚发的那个凭证吗」。空表示这一行没有提示——建渠道时
	// 没写、或凭证短到没有末四位可辨。
	//
	// 它**不是**凭证的一部分：库里存的仍然只有密文，这一列是明文尾部几个字符，
	// 由写入路径在封装之前顺手取下来（密文一旦生成就再也取不到了）。
	Hint string

	// CredentialErr 说明 CredentialKeys 为什么是空的。为 nil 表示信封头部解析正常。
	//
	// 它记的是**结构**问题：密文被截断、被手工改过。列举不碰钥匙，
	// 所以「解不开」在这里根本不会发生。
	CredentialErr error
}

// List 列出全部渠道，按 id 排序。
//
// # 一行密文坏了不影响别的行
//
// WrappedWith 只解析信封头部，不碰钥匙，所以它失败只可能是这一行的密文本身
// 不合法。那种行仍然列出来，原因写在那一行的 CredentialErr 上——**不跳过、
// 也不整次报错**。
//
// 整次报错是最先写的那一版，实际用起来是错的：库里有**一行**手工写进去的
// 密文（那种 `'\x00'::bytea` 占位符），整个清单就一条都看不到，而那正是最需要
// 看清单的时候。这与读取路径对凭证解不开的处置一致（见 Candidates：
// 一个渠道的密文坏了不该让同模型下别人的渠道一起不可用），区别只在于
// 这里连日志都不必——调用方手里就有那一行。
//
// 同一个函数里 setting 解析失败却是整次报错（见 scanInfo），不是双标：
// setting 是 jsonb，**合法性由列类型保证**，解不开说明这一行根本不是按
// 表定义写进去的；credential_enc 是 bytea，没有任何约束拦得住一行垃圾，
// 而它坏掉只影响它自己那一栏。
func (a *Admin) List(ctx context.Context) ([]ChannelInfo, error) {
	rows, err := a.db.QueryContext(ctx, listQuery)
	if err != nil {
		return nil, fmt.Errorf("查询渠道失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：没有渠道是正常结果（还没配），不是「没有值」。
	out := []ChannelInfo{}
	for rows.Next() {
		info, err := scanInfo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历渠道失败: %w", err)
	}
	return out, nil
}

// listQuery 取回管理面要看的全部列。
//
// credential_enc 也在里面，但只用来算 CredentialKeys，不会离开本包：
// 解码之后它变成一串主密钥标识，密文本体到此为止。
//
// credential_hint 是唯一一列**明文**的凭证信息（末四位），它本来就是给界面
// 看的，所以照原样带出去。这一条与「只存密文」不矛盾：密文仍然是唯一的
// 凭证本体，提示是写入时顺手取下的尾部字符，解不出任何东西。
//
// 列名与 candidateQuery 保持同一套（models 同样用 array_to_json 转成 JSON 文本，
// 因为 database/sql 不认 text[]）——两处对同一列的读法不一致时，
// 症状是「写入路径存进去的值读出来不对」，而它只在某些行上出现。
//
// 提示只有 listQuery 取，candidateQuery 不取：候选集是推理热路径，
// 多带一列明文尾部没有任何用处。
const listQuery = `
SELECT
    c.id,
    COALESCE(c.owner_user_id, 0),
    c.name,
    c.type,
    c.base_url,
    c.status,
    array_to_json(c.models)::text,
    c.weight,
    c.priority,
    c.setting,
    c.credential_enc,
    c.credential_hint,
    c.created_at,
    c.updated_at
  FROM channels c
 ORDER BY c.id`

func scanInfo(rows *sql.Rows) (ChannelInfo, error) {
	var (
		info       ChannelInfo
		modelsJSON string
		settingRaw []byte
		envelope   []byte
		hint       sql.NullString
	)
	err := rows.Scan(
		&info.ID, &info.OwnerUserID, &info.Name, &info.Type, &info.BaseURL,
		&info.Status, &modelsJSON, &info.Weight, &info.Priority, &settingRaw,
		&envelope, &hint, &info.CreatedAt, &info.UpdatedAt,
	)
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("读取渠道行失败: %w", err)
	}
	// NULL 与空串在这里合并成同一个值：这一列没有「空提示」与「没有提示」
	// 的区分——凭证短到不足四位时 credential.Hint 原样返回，那种串也不会是
	// 空的（写入路径拒绝空凭证）。
	info.Hint = hint.String

	models, err := decodeModels(info.ID, modelsJSON)
	if err != nil {
		return ChannelInfo{}, err
	}
	info.Models = models

	setting, err := decodeSetting(settingRaw)
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("渠道 %d 的 setting 不是合法 JSON: %w", info.ID, err)
	}
	info.Setting = setting

	keys, err := credential.WrappedWith(envelope)
	if err != nil {
		info.CredentialErr = fmt.Errorf("凭证密文不合法: %w", err)
	} else {
		info.CredentialKeys = keys
	}

	return info, nil
}

// ── 校验与归一化 ────────────────────────────────────────────────────────
//
// 这一批函数是写入路径与读取路径「同一套列」的兑现方式：值的形态在写入侧
// 定下来，读取侧按同一个假设解。校验失败时只报**哪个字段不对**，不回显值——
// 凭证字段固然不能回显，其余字段也用同一套写法，免得后来把某个字段换成
// 凭证时忘了改这一处。

// validateType 确认这个渠道类型有对应的适配器。
func (a *Admin) validateType(channelType string) error {
	if a.registry == nil {
		return errors.New("没有适配器注册表，无法校验渠道类型")
	}
	if _, err := a.registry.Lookup(channelType); err != nil {
		// 库里存一个没人实现的类型，选路时会表现为「这个渠道永远选不中」——
		// 一条看起来配好了、实际不可用的渠道。所以挡在写入侧。
		return fmt.Errorf("渠道类型无效: %w", err)
	}
	return nil
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("渠道名不能为空")
	}
	return name, nil
}

// normalizeBaseURL 校验上游地址。
//
// 只认 http/https：没有 scheme 的地址会一路走到 httpx.Endpoint，在那里拼出
// 一个说不清哪里错的 URL（见那个函数的注释），报错时离「配置写错了」已经很远，
// 而排查要从几百个渠道里找出是哪一个。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("base_url 不能为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("base_url 不是合法的 URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base_url 必须以 http:// 或 https:// 开头")
	}
	if parsed.Host == "" {
		return "", errors.New("base_url 里没有主机名")
	}
	return raw, nil
}

// normalizeModels 校验模型名列表。
//
// # 空数组不是「支持全部模型」
//
// 读取路径把 models 当白名单用：`models @> ARRAY[规范名]`。空数组匹配不到任何
// 模型名，于是这条渠道永远选不中。读取路径里**没有**「空即全部」这个语义，
// 所以这里不能替它发明一个——放进去的行会是一条安静的废配置。
func normalizeModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, errors.New("models 至少要有一个模型名：空数组匹配不到任何模型，这条渠道永远选不中")
	}
	out := make([]string, 0, len(models))
	for i, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, fmt.Errorf("models 里第 %d 个模型名为空", i+1)
		}
		out = append(out, m)
	}
	return out, nil
}

// normalizeWeight 校验分流权重。
//
// 表默认值是 1，route 也把 <= 0 当成 1（见 route.Candidate.Weight）。与其存一个
// 0 让读取侧去解释，不如在这里就落成 1：库里的值与它的实际含义一致，
// 看库的人不必再去翻代码。
func normalizeWeight(weight int) (int, error) {
	if weight < 0 {
		return 0, errors.New("weight 不能为负数")
	}
	if weight == 0 {
		return 1, nil
	}
	return weight, nil
}

// encodeSetting 把 setting 编成 jsonb 的文本形式。
func encodeSetting(setting map[string]any) ([]byte, error) {
	if len(setting) == 0 {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(setting)
	if err != nil {
		return nil, fmt.Errorf("setting 不是可序列化的 JSON 对象: %w", err)
	}
	return raw, nil
}

// sealCredential 把明文凭证封成信封密文。
//
// 明文只在参数与返回值之间流动。凭证本身是否合法（非空、不过长）由
// credential.Sealer.Seal 判定，这里不重复一遍：两个地方各写一份判据，
// 改了一处而没改另一处时，「封出来的密文」与「判定为合法的凭证」就不一致了。
// Seal 的错误只说长度，不含明文；这里再包一层，不回显任何东西。
func (a *Admin) sealCredential(plaintext string) ([]byte, error) {
	if a.sealer == nil {
		// 与读取路径同一个错误：两种情况下要处置的都是「这台机器没配主密钥」。
		return nil, fmt.Errorf("%w：本进程没有配置主密钥", ErrNoMasterKey)
	}
	envelope, err := a.sealer.Seal([]byte(plaintext))
	if err != nil {
		return nil, fmt.Errorf("封装凭证失败: %w", err)
	}
	return envelope, nil
}
