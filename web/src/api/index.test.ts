import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * `VITE_API_MODE` 是选 client 的唯一判据，页面上的「演示数据」横幅也读它。
 * 两个分支各钉一条：判据写反了，界面会拿演示数据冒充真实账目。
 *
 * `import.meta.env` 在模块求值时被读走，所以每个用例都清一次模块表，
 * 再动态引入入口与它实际拿到的那个 client 类——静态 import 拿到的是旧那一份，
 * `toBeInstanceOf` 会因两份类对象不是同一个而假失败。
 */
describe('api 入口按 VITE_API_MODE 选 client', () => {
  beforeEach(() => {
    vi.resetModules()
  })

  afterEach(() => {
    vi.unstubAllEnvs()
  })

  it('VITE_API_MODE=http 时用真实 HTTP 客户端', async () => {
    vi.stubEnv('VITE_API_MODE', 'http')
    vi.stubEnv('VITE_API_BASE', 'https://api.example.com/v1')

    const { api, isMockMode } = await import('./index')
    const { HttpApiClient } = await import('./client')

    expect(isMockMode).toBe(false)
    expect(api).toBeInstanceOf(HttpApiClient)
    expect(api.mode).toBe('http')
  })

  it('VITE_API_MODE=mock 时用演示数据', async () => {
    vi.stubEnv('VITE_API_MODE', 'mock')

    const { api, isMockMode } = await import('./index')
    const { MockApiClient } = await import('./mock')

    expect(isMockMode).toBe(true)
    expect(api).toBeInstanceOf(MockApiClient)
    expect(api.mode).toBe('mock')
  })
})
