// Package auth 从请求里取出 API Key，并解析出它属于哪个用户。
//
// # 凭证从哪来
//
// 两个头都认：`Authorization: Bearer <key>`（OpenAI 的习惯）与
// `x-api-key: <key>`（Anthropic 的习惯）。调用方按哪个协议发，这里就收哪个，
// 于是「按调用协议给形状」这件事在鉴权这一层就消化掉了，
// 上面的公共入口不必为两套协议各写一份取凭证的代码。
//
// # 只存哈希
//
// 明文不落库也不写日志，库里只有 key_hash（口径见 HashKey）。
// 创建时返回一次明文，此后无法取回——丢了只能重建一把。
//
// # 对外不区分，日志要区分
//
// key 不存在、已停用、已吊销、已过期，以及 key 的主人被封禁，对外都是同一个 401。
// 区分它们等于告诉试探者「这个 key 存在，只是过期了」。
// 但排查时必须能区分，所以失败原因装在 Error.Reason 里，它只给日志看。
//
// # 错误分两类
//
//   - errors.Is(err, ErrUnauthenticated)：凭证本身的问题，回 401
//   - 其它错误：查询失败了（连不上库、超时），回 5xx
//
// 这个区分不能省。把后者也当成 401，一次数据库抖动就会表现为
// 「全站密钥突然失效」，而那是两个方向完全相反的排查。
package auth

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

// User 是认证通过后的用户身份。
//
// 只带鉴权与计费真正要用到的字段：这里的每一个字段都来自热路径上的一次查询，
// 多一个字段就是多一列要读、多一处要维护的语义。
type User struct {
	// ID 是 users.id。
	ID int64
	// Username 是 users.username，用于日志与展示。
	Username string
	// KeyID 是这次请求用的 api_keys.id，落 billing_requests.api_key_id 用。
	//
	// 它和 ID 不是一回事：一个用户可以有多个 key，账单要能追到**哪一把**在用。
	KeyID int64
	// RateLimitRPM 是这把 key 的每分钟请求额度，即 api_keys.rate_limit_rpm
	// 的**原样**值：nil 表示列值为 NULL。
	//
	// 这里不解释 NULL 与 0 各是什么意思——那是限流的事（见 internal/ratelimit）。
	// 本包只负责把列值带上来：在两处各解释一遍，两处迟早会不一致，
	// 而不一致时没有任何东西会报错。
	RateLimitRPM *int
}

// Authenticator 是公共 HTTP 入口对鉴权层的要求。
//
// 这里再声明一遍而不是去 import 那一侧，是因为依赖方向只能是一个方向：
// 公共入口 import 本包，本包不能反过来 import 它——否则包间成环。
// 两侧各声明一份形状相同的接口，正好让「实现还对得上」在编译期就能验证：
// 那边写 `var _ api.Authenticator = auth.New(db)`，签名一改就编不过。
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (User, error)
}

// Store 是 API Key 的鉴权与创建入口。
//
// 创建与校验在同一个类型上，是因为它们**必须**共用同一份哈希口径
// （见 HashKey）。分成两个类型各写一遍，就会有「刚建的 key 立刻说无效」
// 这种只在对端才暴露的错配。
type Store struct {
	db *sql.DB
}

// New 包装一个已打开的数据库句柄。
func New(db *sql.DB) *Store { return &Store{db: db} }

// 编译期断言：Store 满足公共入口要的那份接口。
var _ Authenticator = (*Store)(nil)

// Authenticate 从请求头取出 API Key，解析出它属于哪个用户。
//
// 头里取不到凭证时立即返回，不查库——一个不带凭证的请求不该产生一次数据库往返。
func (s *Store) Authenticate(ctx context.Context, r *http.Request) (User, error) {
	plaintext, err := credentialFrom(r)
	if err != nil {
		return User{}, err
	}
	return s.lookup(ctx, plaintext)
}

// credentialFrom 按调用协议从两个头里取凭证。
//
// Authorization 优先，而且它**一旦出现就不再回退**到 x-api-key：
// 同时发两个头本身就是客户端的配置错误，静默改用另一个会让
// 「为什么换了 key 还是不生效」变成一场没有线索的排查。
func credentialFrom(r *http.Request) (string, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		// 只接受 `Bearer <token>` 两段。多段说明 token 里带了空格、
		// 少段说明只写了方案名——两者都是发错了东西，而不是「key 不对」，
		// 所以归到 malformed 而不是 not_found。
		//
		// 方案名按大小写不敏感比较（RFC 7235 里 scheme 不区分大小写），
		// 客户端写 bearer 是合法的。
		fields := strings.Fields(h)
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			return "", &Error{Reason: ReasonMalformedCredential}
		}
		return fields[1], nil
	}

	// 只去掉首尾空白：key 的字符集里没有空白，出现了就是发错了，
	// 而它会在查库那一步表现为「查不到」——对外同样是 401。
	if k := strings.TrimSpace(r.Header.Get("x-api-key")); k != "" {
		return k, nil
	}

	return "", &Error{Reason: ReasonMissingCredential}
}
