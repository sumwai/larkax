import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeEach } from 'vitest'

// jsdom 没有 matchMedia；侧边栏按它切换宽度，测试里当作宽屏。
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
  window.matchMedia = (query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      addListener: () => undefined,
      removeListener: () => undefined,
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList
}

// Node 22 起，加 `--experimental-webstorage` 会让 `globalThis` 上出现
// `localStorage` / `sessionStorage`；Node 25 起默认就有。不传 `--localstorage-file`
// 时它们不可用——22 上读一下就抛 `ERR_INVALID_ARG_VALUE`，25 上是个没有 `setItem`
// 这些方法的空对象。vitest 的 jsdom 环境只在 `globalThis` 还没有这个属性时才把
// jsdom 的实现接上去（`populateGlobal` 按 `k in global` 过滤），于是新版 Node 上
// 裸名字拿到的是 Node 那份，`beforeEach` 在第一个用例之前就抛。
//
// jsdom 的实现挂在它自己的 window 上，而 vitest 把这个 window 留在
// `globalThis.jsdom.window`——不能取 `window.localStorage`，因为 jsdom 环境下
// `window === globalThis`，取到的还是 Node 那份。
if (typeof window !== 'undefined') {
  const jsdomWindow = (globalThis as unknown as { jsdom: { window: Window } }).jsdom.window
  for (const key of ['localStorage', 'sessionStorage'] as const) {
    Object.defineProperty(globalThis, key, {
      value: jsdomWindow[key],
      configurable: true,
    })
  }
}

beforeEach(() => {
  // 默认中文，断言文案时不用每次指定语言。
  localStorage.setItem('larkax.lang', 'zh')
})

afterEach(() => {
  cleanup()
  localStorage.clear()
  sessionStorage.clear()
  document.documentElement.removeAttribute('data-theme')
})
