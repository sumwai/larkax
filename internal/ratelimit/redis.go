package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore 把计数放在 Redis 里，于是同一份计数被所有实例共用。
//
// # 键
//
// 键名是 rl:<keyID>:<窗口起点的 Unix 秒>，见 windowKey。键名里带窗口起点
// 是这个设计的一半：一个窗口一份计数，读的人也只会读当前窗口那一个键。
//
// # 过期
//
// **每次调用都续一次 EXPIRE**，取值是两倍窗口（见 keyTTL）。每次都续，而不是
// 只在第一次写的时候设一次，换来两件事：
//
//   - 自愈。上一次调用没设上过期时间（下面那一段）时，下一次调用会补上。
//   - 键的存活时间与它最后一次被用到的时间挂钩，而不是与窗口起点挂钩。
//
// # INCR 成功而 EXPIRE 失败
//
// 这是这条路径上最值得单独说的一种失败：命令是分两次发的，第一次落库、
// 第二次没落库，于是留下一个**没有过期时间**的键。它记的是这个窗口的次数，
// 键名里带着窗口起点，所以它在下一个窗口里不会再被读到——但**它永远留着**，
// 既不会被读到也不会自己消失，是这一层唯一会无限增长的形态。
//
// 处置是补偿删除（见 compensate）：删掉之后取到的计数从 1 重新开始（判定偏松，
// 与降级同一个方向），而不是留一个永远不消失的键。删不掉也不会永久卡住这个
// Key（键名带窗口起点，下一个窗口是另一个键），代价是 Redis 里多一个孤键。
type RedisStore struct {
	// cmds 是这条路径用到的命令集。抽成一个窄接口是为了让「INCR 成功、
	// EXPIRE 失败」那条路径可以被测到：真实 Redis 上没法只让其中一条失败，
	// 而它恰恰是这里最需要被钉住的失败。*redis.Client 满足它。
	cmds redisCmds
}

// redisCmds 是 RedisStore 用到的命令。方法集与 *redis.Client 一致，
// 所以生产侧直接填客户端，测试侧填一个只让指定命令失败的替身。
type redisCmds interface {
	Incr(ctx context.Context, key string) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Ping(ctx context.Context) *redis.StatusCmd
}

// OpenRedis 按 DSN 造一个共享计数器。
//
// DSN 的 scheme（redis://、rediss://、unix://）在 internal/config 里已经校验过，
// 这里再解析一遍是两件不同的事：那边管的是「这个值像不像一个 DSN」，这里管的是
// 「这个值能不能变成一个客户端」。解析失败是**配置错误**，由调用方拒绝启动；
// 而解析成功但连不上是**故障**，那时降级而不是拒绝启动（见 Allow）。
//
// 连接池、AUTH、TLS 都由客户端库处理（rediss:// 就是 TLS）。
func OpenRedis(dsn string) (*RedisStore, error) {
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 Redis DSN 失败: %w", err)
	}
	// 关掉重试（-1 而不是 0，0 表示用库的默认值 3 次）。理由与超时同一个：
	// 一次判定只做那两条命令，重试把时间花在等待上，而这段时间是用户请求在等。
	// 抖一下的正确处置是降级——那一次判定偏松——而不是让请求多等几百毫秒。
	opt.MaxRetries = -1

	// 把库自己的诊断日志接到 slog 上，见 redisLogAdapter。
	redis.SetLogger(redisLogAdapter{})

	return &RedisStore{cmds: redis.NewClient(opt)}, nil
}

// redisLogAdapter 把 Redis 客户端库自己的诊断日志转到 slog 上。
//
// 为什么要管它：库默认往 stderr 打**非结构化**的一行行，而它的触发点恰好
// 是最坏的那一种场景——Redis 连不上时每次拨号都会打一条。我们的降级路径
// 已经把这件事变成了两个指标与一条 warn，这里要做的只是别让第三份没有
// 格式的拷贝刷满日志。
//
// 打到 debug 级：这些行是排查连接细节（拨号、连接池、流水线）时才要看的，
// 默认级别下不该出现——而「Redis 连不上」本身有自己的 warn 与指标兜着，
// 不会因为这里降到 debug 而被埋掉。
type redisLogAdapter struct{}

// Printf 是 go-redis 的日志接口，把库自己的诊断行接到 slog 的 debug 上。
// 为什么要有这个适配器、为什么是 debug 级，见上面 redisLogAdapter 的说明。
func (redisLogAdapter) Printf(ctx context.Context, format string, args ...any) {
	slog.Default().DebugContext(ctx, "redis 客户端", "msg", fmt.Sprintf(format, args...))
}

// Incr 把 key 的计数加一并返回加后的值。
func (s *RedisStore) Incr(ctx context.Context, key string) (int64, error) {
	n, err := s.cmds.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if err := s.cmds.Expire(ctx, key, keyTTL).Err(); err != nil {
		// 走到这里说明计数已经加上去了、过期时间没设上（见类型注释）。
		// 先补偿删除，再把错误报上去——调用方会按「存储不可用」降级，
		// 于是这一次判定不会用一个可能已经不对的数去拒人。
		s.compensate(ctx, key)
		return 0, fmt.Errorf("INCR 之后续 EXPIRE 失败: %w", err)
	}
	return n, nil
}

// compensate 尽力删掉那个没有过期时间的键。
//
// 它从一个**掐掉取消信号**的 context 派生新的超时，而不是直接复用传进来那个：
// EXPIRE 失败最常见的原因正是那个 context 已经到点了（超时），复用它的话这次
// 删除必然也发不出去。WithoutCancel 保留值、去掉取消，于是父 context 的截止
// 时间不影响这里，而它仍然是同一个调用链上的子 context。
//
// 失败只记账不报错：删不掉时上面那条错误照样要报上去，而这里没有第二个
// 处置可做——键名带窗口起点，下一个窗口用的是另一个键，所以最坏的结果是
// Redis 里多一个读不到的孤键，而不是某个 Key 被永久卡住。
func (s *RedisStore) compensate(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	_ = s.cmds.Del(ctx, key).Err()
}

// Get 读 key 的当前值。键不存在时返回 (0, false, nil)。
//
// 它只读：不 INCR、不续过期时间，也就不可能把一个只查用量的人算进额度里，
// 也不会因为查了一下而让键活得比它该活的久。
func (s *RedisStore) Get(ctx context.Context, key string) (int64, bool, error) {
	n, err := s.cmds.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// Ping 探活。给启动期那句「共享存储能不能用」用。
func (s *RedisStore) Ping(ctx context.Context) error { return s.cmds.Ping(ctx).Err() }

// Close 释放连接池。
func (s *RedisStore) Close() error {
	type closer interface{ Close() error }
	if c, ok := s.cmds.(closer); ok {
		return c.Close()
	}
	return nil
}
