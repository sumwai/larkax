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

  it('管理员只能进商家视图里的概览、模型与上游', () => {
    for (const path of ['/app/merchant', '/app/merchant/overview', '/app/merchant/models', '/app/merchant/upstreams']) {
      expect(viewAllowed('admin', path)).toBe(true)
    }
    // 渠道、计价与设置写的都是「你名下」，管理员没有这层归属。
    for (const path of [
      '/app/merchant/channels',
      '/app/merchant/pricing',
      '/app/merchant/settings',
    ]) {
      expect(viewAllowed('admin', path)).toBe(false)
    }
    expect(viewAllowed('merchant', '/app/merchant/overview')).toBe(true)
    expect(viewAllowed('user', '/app/merchant/overview')).toBe(false)
    expect(viewAllowed('admin', '/app/admin/channels')).toBe(true)
  })
})
