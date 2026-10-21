import { describe, expect, it } from 'vitest'
import { homeOf, viewOfPath, viewsFor } from './views'

describe('控制台视图', () => {
  it('角色决定拥有哪些视图', () => {
    expect(viewsFor('user')).toEqual(['user'])
    expect(viewsFor('merchant')).toEqual(['user', 'merchant'])
    // 管理员没有商家视图：商家页列的是「调用者名下」的资源，管理员没有这层归属。
    expect(viewsFor('admin')).toEqual(['user', 'admin'])
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
    expect(homeOf('merchant')).toBe('/app/merchant/channels')
    expect(homeOf('admin')).toBe('/app/admin/channels')
  })
})
