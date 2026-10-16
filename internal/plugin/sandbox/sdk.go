package sandbox

import (
	"sync"

	"github.com/Calcium-Ion/moejs"
)

// sdkModuleName 是插件 `import { ... } from "<这个名字>"` 时用的模块名。
//
// 它是**虚拟**模块：宿主用 Link 的 resolver 直接返回一个已编译的模块，
// 不读文件系统，也不发网络请求。所以插件能导入的东西就是这个文件里
// 导出的名字，别的名字一律解析失败。
const sdkModuleName = "larkax"

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
