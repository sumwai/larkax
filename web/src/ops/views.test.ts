import { describe, expect, it } from 'vitest'
import { homeOf, subjectOf, viewAllowed, viewOfPath, viewsFor } from './views'

describe('控制台视图', () => {
  it('角色决定拥有哪些视图', () => {
    expect(viewsFor('user')).toEqual(['user'])
    expect(viewsFor('merchant')).toEqual(['user', 'merchant'])
    // 管理员也有商家视图，但只有「同一批页面、两种主语」的那三页。
    expect(viewsFor('admin')).toEqual(['user', 'merchant', 'admin'])
    // 旧响应没有 role 字段时按 user 处理。
    expect(viewsFor(undefined)).toEqual(['user'])
  })

  it('路径决定当前视图，且每个视图有各自的首页', () => {
    expect(viewOfPath('/app')).toBe('user')
    expect(viewOfPath('/app/keys')).toBe('user')
    expect(viewOfPath('/app/merchant/channels')).toBe('merchant')
    expect(viewOfPath('/app/admin/models')).toBe('admin')
    expect(viewOfPath('/app/administer')).toBe('user')
    expect(homeOf('user')).toBe('/app')
    expect(homeOf('merchant')).toBe('/app/merchant/overview')
    expect(homeOf('admin')).toBe('/app/admin/channels')
  })

  it('两种主语只有一处判断：管理员是全站，其余是自己名下', () => {
    expect(subjectOf('admin')).toBe('site')
    expect(subjectOf('merchant')).toBe('mine')
    expect(subjectOf('user')).toBe('mine')
    expect(subjectOf(undefined)).toBe('mine')
  })

  // 判定按地址段走：渠道、计价、设置写的都是「你名下」，管理员没有这层归属；
  // 前缀混淆（`overviewish`）与未知子路径也必须拒绝。
  const ADMIN_MERCHANT: Array<[string, boolean]> = [
    ['/app/merchant', true],
    ['/app/merchant/', true],
    ['/app/merchant/overview', true],
    ['/app/merchant/models', true],
    ['/app/merchant/upstreams', true],
    ['/app/merchant/channels', false],
    ['/app/merchant/pricing', false],
    ['/app/merchant/settings', false],
    ['/app/merchant/settings/profile', false],
    ['/app/merchant/foo', false],
    ['/app/merchant/overviewish', false],
  ]

  it.each(ADMIN_MERCHANT)('管理员对 %s 的判定是 %s', (path, allowed) => {
    expect(viewAllowed('admin', path)).toBe(allowed)
  })

  it('其余角色按视图归属判定', () => {
    expect(viewAllowed('merchant', '/app/merchant/overview')).toBe(true)
    expect(viewAllowed('user', '/app/merchant/overview')).toBe(false)
    expect(viewAllowed('admin', '/app/admin/channels')).toBe(true)
  })
})
