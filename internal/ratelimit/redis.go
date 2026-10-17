package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
//   - 自愈。键在别处被设丢过期时间时（见下），下一次调用会补上。
//   - 键的存活时间与它最后一次被用到的时间挂钩，而不是与窗口起点挂钩。
//
// # 计数是一步
//
// 加一与设过期合成**一次 EVAL**（见 incrScript），而不是先 INCR 再 EXPIRE 两条
// 命令、失败再补偿删除。分两步时那两条命令之间有两条缝：
//
//   - 进程死在两条命令中间（INCR 落了库、EXPIRE 还没发出去）；
//   - EXPIRE 失败，而随后的补偿删除也失败。
//
// 两种都留下一个**没有过期时间的键**（TTL = -1）：它记的是这个窗口的次数、
// 键名带窗口起点所以不会再被读到，但**永远占着内存**——这是这一层唯一会无限
// 增长的形态。合成一次 EVAL 之后，Redis 保证两条命令要么都生效、要么都不生效，
// 两条缝一起消失，补偿删除也随之不需要了。
//
// 副产品是热路径的往返数从 2 降到 1：一次判定只发这一条命令。
type RedisStore struct {
	// cmds 是这条路径用到的命令集。抽成一个窄接口是为了让「存储命令失败」
	// 那条路径可以被测到：真实 Redis 上没法稳定地造出「连得上但报错」，
	// 而它恰恰是这里最需要被钉住的失败。*redis.Client 满足它。
	cmds redisCmds
}

// redisCmds 是 RedisStore 用到的命令。方法集是 *redis.Client 的子集，
// 所以生产侧直接填客户端，测试侧填一个受控的替身。
//
// 只有三个方法：计数那条路径只有一条命令（Eval），另两条是只读查询（Get）
// 与探活（Ping）。
type redisCmds interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Ping(ctx context.Context) *redis.StatusCmd
}

// incrScript 是热路径上那一次原子计数：加一、续上过期时间，返回加后的值。
//
// 为什么合成一次 EVAL、它堵住的那两条缝是什么，见 RedisStore 的「计数是一步」
// 一段。
//
// **续期每次都做**，与合并之前逐字一致：每次调用都把键的过期时间设回两倍窗口
// （ARGV[1] 是秒数）。只在 `TTL < 0` 时才设是**不够**的——那样一个持续被用的
// 键会在**第一次写入**之后的两倍窗口处准时消失，键的寿命就与第一次写入挂钩，
// 而不是与最后一次使用挂钩。`TTL < 0` 时当然也要设（那是自愈），但自愈只是
// 「每次续」的一个子集，不是它的替代；这里无条件 EXPIRE，于是两者都成立。
const incrScript = `
local n = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[1])
return n
`

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
	// 一次判定只做那一条命令，重试把时间花在等待上，而这段时间是用户请求在等。
	// 抖一下的正确处置是降级——那一次判定偏松——而不是让请求多等几百毫秒。
	opt.MaxRetries = -1

	// 把库自己的诊断日志接到 slog 上，见 redisLogAdapter。
	setRedisLogger()

	return &RedisStore{cmds: redis.NewClient(opt)}, nil
}

// setRedisLogger 每个进程只把库的日志接一次。
//
// go-redis 的 Logger 是**包级**变量，而读它的是库自己的 goroutine（拨号失败后的
// 诊断路径）。每调一次 OpenRedis 就写一次，与那些还活着的 goroutine 构成数据竞争：
// `-race -count=5` 下稳定报出来，而 `-count=1`（门禁与 CI 的口径）连跑五次全绿——
// 也就是说这道门本身照不到它。生产上 OpenRedis 只在装配期调一次，窗口很窄，
// 但「很窄」不是「没有」。
//
// 接一次就够：适配器是无状态的，所有客户端共用同一个。
var redisLoggerOnce sync.Once

func setRedisLogger() {
	redisLoggerOnce.Do(func() { redis.SetLogger(redisLogAdapter{}) })
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

// Incr 原子地把 key 的计数加一、续上过期时间，并返回加后的值。
//
// 一次调用就是**一次往返**（一条 EVAL，见 incrScript）。命令失败时返回错误，
// 调用方据此按「存储不可用」降级（见 Limiter.Allow），于是这一次判定不会拿
// 一个可能已经不对的数去拒人。又因为两条命令是原子的，失败时存储里不会留下
// 任何需要清理的残留——没有「INCR 成功而 EXPIRE 失败」这个中间态。
func (s *RedisStore) Incr(ctx context.Context, key string) (int64, error) {
	n, err := s.cmds.Eval(ctx, incrScript, []string{key}, int(keyTTL/time.Second)).Int64()
	if err != nil {
		return 0, fmt.Errorf("原子计数失败: %w", err)
	}
	return n, nil
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
