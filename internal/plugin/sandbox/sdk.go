package sandbox

import (
	"sort"
	"sync"

	"github.com/Calcium-Ion/moejs"
)

// sdkModuleName 是插件 `import { ... } from "<这个名字>"` 时用的模块名。
//
// 它是**虚拟**模块：宿主用 Link 的 resolver 直接返回一个已编译的模块，
// 不读文件系统，也不发网络请求。所以插件能导入的东西就是这个文件里
// 导出的名字，别的名字一律解析失败。
const sdkModuleName = "larkax"

// 能力名。它们是 sdkSource 里 `capability("<名字>")` 用的那三个字符串，
// 也是插件 `import { log, calendar, kv } from "larkax"` 拿到的那三样东西。
//
// **放在这里而不是各自的实现包里**，因为它们是契约面：插件作者能看到什么
// 由这份源码决定，宿主侧的实现（internal/plugin/host）与注册期校验「申请了
// 哪些能力」的那一处（internal/plugin）都该以它为准。三处各写一份字面量的
// 代价是插件申请得到一个名字、而宿主没有对应的实现——那种错法不报错，
// 只在插件运行期表现为一次 TypeError。
const (
	// CapabilityLog 是结构化日志。
	CapabilityLog = "log"
	// CapabilityCalendar 是日历判定。
	CapabilityCalendar = "calendar"
	// CapabilityKV 是插件按用户隔离的私有存储。
	CapabilityKV = "kv"
)

// capabilityNames 是上面那三个能力名。
//
// 它这里写成什么顺序都行：CapabilityNames 会排序。顺序固定是有用的——
// 错误文案里会把这个名单列出来（「不在宿主提供的能力里（calendar、kv、log）」），
// 而它由排序保证，不靠写的人记得按顺序摆。
//
// 是数组而不是切片：这一份是契约，长度在编译期就定下来，`append` 到它上面
// 是编译错误而不是悄悄多出一个名字。
var capabilityNames = [...]string{CapabilityLog, CapabilityCalendar, CapabilityKV}

// CapabilityNames 返回这份 SDK 导出的全部能力名，按字典序。
//
// 返回一份副本：调用方排序或切片时不该动到包级那一份。
//
// 它与 sdkSource 的一致性由用例钉住（两处对不上时编译器不会报错）：
// 源码里多一个 `capability("…")`、Go 这边少一个名字，插件调用就会拿到
// 「能力未启用」，而那是这道改动里最难查的一种失配。
func CapabilityNames() []string {
	out := make([]string, len(capabilityNames))
	copy(out, capabilityNames[:])
	sort.Strings(out)
	return out
}

// sdkSource 是虚拟 SDK 的源码。
//
// 它是**契约面**：插件作者看到的全部能力都在这里被列举出来。
// 没有 fetch、没有 fs、没有 require、没有 process，也没有渠道密钥——
// 宿主根本没有把它们放进 globalThis。
//
// 为什么在 SDK 里再包一层而不是让插件直接摸 globalThis：宿主注入的全局名
// 是实现细节，可以改；插件作者面对的是这份源码里的名字，改动它是一次契约变更。
const sdkSource = `
export function definePlugin(hooks) {
  return hooks;
}

function host() {
  const h = globalThis.__larkax;
  if (!h) {
    throw new Error("宿主能力未注入：这个插件运行在没有宿主能力的环境里");
  }
  return h;
}

function capability(name) {
  const c = host()[name];
  if (!c) {
    throw new Error("宿主能力 " + name + " 未启用");
  }
  return c;
}

export function log(level, message, fields) {
  capability("log").log(level, String(message), fields === undefined ? null : fields);
}

export const calendar = {
  isOffDay(ts, region) {
    return capability("calendar").isOffDay(ts, region);
  },
  isMakeupWorkday(ts, region) {
    return capability("calendar").isMakeupWorkday(ts, region);
  },
  holidayName(ts, region) {
    return capability("calendar").holidayName(ts, region);
  },
  kind(ts, region) {
    return capability("calendar").kind(ts, region);
  },
  peak(ts, region, window) {
    return capability("calendar").peak(ts, region, window);
  },
  hourIn(region) {
    return capability("calendar").hourIn(region);
  },
  weekdayIn(region) {
    return capability("calendar").weekdayIn(region);
  },
};

export const kv = {
  get(key) {
    return capability("kv").get(String(key));
  },
  set(key, value, ttlSeconds) {
    return capability("kv").set(String(key), String(value), ttlSeconds === undefined ? 0 : ttlSeconds);
  },
  del(key) {
    return capability("kv").del(String(key));
  },
};
`

var (
	sdkOnce sync.Once
	sdkMod  *moejs.Module
	sdkErr  error
)

// sdk 编译一次虚拟 SDK 并缓存。Module 不可变，可被任意多 runtime 并发 Load。
func sdk() (*moejs.Module, error) {
	sdkOnce.Do(func() {
		sdkMod, sdkErr = moejs.Compile(sdkModuleName, sdkSource)
	})
	return sdkMod, sdkErr
}
