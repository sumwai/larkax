package host

import (
	"context"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// kvKeyMaxBytes 是键的字节上限。键由插件给出，不设界时它可以用一个
// 巨长的键把存储与日志撑大。
const kvKeyMaxBytes = 512

// kvValueMaxBytes 是值的字节上限。上限的意义在于 KV 是**共享资源**：
// 一个插件写进去的字节数会影响同一张表上所有人的成本。
const kvValueMaxBytes = 64 << 10

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
		if err := env.KV.Set(ctx, env.Plugin, env.UserID, key, value, time.Duration(ttl)*time.Second); err != nil {
			return moejs.Undefined(), unavailable("写入插件 KV 失败", err)
		}
		// 不返回值：SDK 的契约里 set 没有返回值，凭空多给一个会让插件
		// 依赖上一个没写在契约里的东西。
		return moejs.Undefined(), nil
	}
}

// kvDel 删一个键，返回是否真的删掉了（删之前它存在）。
func kvDel(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		key, err := kvKey(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		// 先问「在不在」再删：KVStore.Del 只回错误码，而插件要的是
		// 「我到底删掉了什么」。多一次查询换来一个有内容的布尔值，
		// 比返回一个恒定的值有用——恒定的值等于没有这个返回值。
		//
		// 两次调用之间不是原子的，所以并发的两次 del 可能都报 true。
		// 需要改的话要动 KVStore 的签名（让它把结果一起回），
		// 而不是在这一层加锁：锁只能挡住同一个进程里的两个插件实例。
		_, found, err := env.KV.Get(ctx, env.Plugin, env.UserID, key)
		if err != nil {
			return moejs.Undefined(), unavailable("读取插件 KV 失败", err)
		}
		if err := env.KV.Del(ctx, env.Plugin, env.UserID, key); err != nil {
			return moejs.Undefined(), unavailable("删除插件 KV 失败", err)
		}
		return moejs.Bool(found), nil
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
