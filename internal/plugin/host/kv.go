package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// kvKeyMaxBytes 是键的字节上限。键由插件给出，不设界时它可以用一个
// 巨长的键把存储与日志撑大。
const kvKeyMaxBytes = 512

// kvValueMaxBytes 是值的字节上限。上限的意义在于 KV 是**共享资源**：
// 一个插件写进去的字节数会影响同一张表上所有人的成本。
//
// 它是**单次**的上限，累计的另有两个（见 KVMaxKeys / KVMaxBytes）。
const kvValueMaxBytes = 64 << 10

// kvTTLMaxSeconds 是 ttlSeconds 的上限。
//
// 需要它是因为过期时刻在存储里由 `now() + ttl` 算出来，而插件给一个天文数字
// （把毫秒当秒、或者存心捣乱）时 SQL 的 timestamp 会溢出并报错——那个错经
// unavailable 出去，看起来像宿主的存储坏了，而其实是一个参数问题。
//
// 十年远超插件私有空间的任何合理用途；想长期保留就该写 0（不过期）。
const kvTTLMaxSeconds = 10 * 365 * 24 * 60 * 60

// kvFuncs 实现 SDK 的 kv 命名空间。
//
// 所有操作都走 env.KV（那层负责按 plugin + userID 做命名空间隔离，
// 见 KVStore 的签名），这个包不自己拼键前缀：让一处负责隔离，
// 就不会出现「有的调用带前缀、有的没带」。
func kvFuncs(env Env) map[string]sandbox.HostFunc {
	return map[string]sandbox.HostFunc{
		"get": hostFunc(kvGet(env)),
		"set": hostFunc(kvSet(env)),
		"del": hostFunc(kvDel(env)),
	}
}

// kvGet 读一个键。键不存在时返回 null，而不是空串——
// 空串是一个具体的值，与「没有这个键」是两件事。
func kvGet(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		key, err := kvKey(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		value, found, err := env.KV.Get(ctx, env.Plugin, env.UserID, key)
		if err != nil {
			return moejs.Undefined(), unavailable("读取插件 KV 失败", err)
		}
		if !found {
			return moejs.Null(), nil
		}
		return moejs.String(value), nil
	}
}

// kvSet 写一个键。ttlSeconds 为 0（或缺省）表示不过期。
//
// value 一律当字符串：SDK 侧已经 String() 过一次，宿主不再解释它，
// 于是「插件存了什么」与「宿主存了什么」是同一串字节。
func kvSet(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		key, err := kvKey(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		value := argString(args, 1)
		if len(value) > kvValueMaxBytes {
			return moejs.Undefined(), badArgument("值超过 %d 字节上限", kvValueMaxBytes)
		}
		ttl, err := argInt64Or(args, 2, "ttlSeconds", 0)
		if err != nil {
			return moejs.Undefined(), err
		}
		if ttl < 0 {
			// 负数没有明确含义（是「立刻过期」还是「永不过期」？），
			// 猜一个等于让插件的一次写变成不可预期的事。
			return moejs.Undefined(), badArgument("ttlSeconds 不能为负数: %d", ttl)
		}
		if ttl > kvTTLMaxSeconds {
			return moejs.Undefined(), badArgument(
				"ttlSeconds 超过 %d 秒上限: %d（想长期保留请写 0，那表示不过期）",
				kvTTLMaxSeconds, ttl)
		}
		if err := env.KV.Set(ctx, env.Plugin, env.UserID, key, value, time.Duration(ttl)*time.Second); err != nil {
			return moejs.Undefined(), kvWriteError(err)
		}
		// 不返回值：SDK 的契约里 set 没有返回值，凭空多给一个会让插件
		// 依赖上一个没写在契约里的东西。
		return moejs.Undefined(), nil
	}
}

// kvWriteError 把一次失败的写入翻成插件看得懂的那一类。
//
// 「配额到顶」必须与「存储坏了」分开：前者插件能做点什么（先删掉一些旧键），
// 后者做什么都没用，而两者的下一步动作不同。配额那一支的 Cause 是**本包的
// 哨兵**（`ErrKVQuotaExceeded`），不是存储侧交上来的原文——存储的原文
// （现在有多少字节）对插件没有用，它要的是「写不进去了」。
func kvWriteError(err error) error {
	if errors.Is(err, ErrKVQuotaExceeded) {
		return &sandbox.CapabilityError{
			Code: sandbox.CodeQuotaExceeded,
			Message: fmt.Sprintf("插件 KV 的配额已用尽（每个插件每个用户最多 %d 个键、%d 字节）；先删掉一些不再用的键再写",
				KVMaxKeys, KVMaxBytes),
			Cause: err,
		}
	}
	return unavailable("写入插件 KV 失败", err)
}

// kvDel 删一个键，返回是否真的删掉了（删之前它存在）。
//
// 结果由 KVStore.Del 一次回（见那个接口的注释）：早先这里先 Get 再 Del，
// 两次调用之间不是原子的，并发的两次 del 会都报 true。现在两个问题一起没了
// ——准的布尔值，少一次往返。
func kvDel(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		key, err := kvKey(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		deleted, err := env.KV.Del(ctx, env.Plugin, env.UserID, key)
		if err != nil {
			return moejs.Undefined(), unavailable("删除插件 KV 失败", err)
		}
		return moejs.Bool(deleted), nil
	}
}

// kvKey 取出并校验键。
func kvKey(args []moejs.Value) (string, error) {
	key := argString(args, 0)
	if key == "" {
		return "", badArgument("键不能为空")
	}
	if len(key) > kvKeyMaxBytes {
		return "", badArgument("键超过 %d 字节上限", kvKeyMaxBytes)
	}
	return key, nil
}
