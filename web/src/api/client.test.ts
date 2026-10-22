import { describe, expect, it, vi } from 'vitest'
import { ApiError, HttpApiClient } from './client'

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: status === 204 ? 'No Content' : String(status),
    json: async () => body,
    headers: new Headers(headers),
  } as unknown as Response
}

function makeClient(response: Response) {
  const fetchImpl = vi.fn<
    (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>
  >(async () => response)
  const client = new HttpApiClient({
    baseUrl: '/api/v1',
    fetchImpl: fetchImpl as unknown as typeof fetch,
  })
  return { client, fetchImpl }
}

describe('HttpApiClient 的 CSRF 头', () => {
  it('写请求带上 X-CSRF-Token，GET 不带', async () => {
    const { client, fetchImpl } = makeClient(jsonResponse(200, { items: [], next_cursor: null }))
    client.setCsrfToken('token-1')

    await client.createKey({ name: 'CI' })
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/v1/keys')
    expect(init?.method).toBe('POST')
    expect((init?.headers as Record<string, string>)['X-CSRF-Token']).toBe('token-1')
    expect(init?.credentials).toBe('same-origin')

    fetchImpl.mockClear()
    await client.models()
    const [, readInit] = fetchImpl.mock.calls[0]
    expect((readInit?.headers as Record<string, string>)['X-CSRF-Token']).toBeUndefined()
  })

  it('登录响应里的 csrf_token 立即生效，不用等下一次会话恢复', async () => {
    const { client, fetchImpl } = makeClient(
      jsonResponse(200, {
        user: {
          id: 1,
          username: 'demo',
          display_name: null,
          email: null,
          status: 'active',
          created_at: '',
        },
        csrf_token: 'fresh-token',
        expires_at: '2026-10-28T00:00:00Z',
      }),
    )

    await client.login('demo', 'password')
    fetchImpl.mockResolvedValue(jsonResponse(200, { id: 1 }))
    await client.updateKey(1, { name: 'renamed' })

    const [, init] = fetchImpl.mock.calls[1]
    expect((init?.headers as Record<string, string>)['X-CSRF-Token']).toBe('fresh-token')
  })

  it('登出后清掉令牌，后续写请求不再带', async () => {
    const { client, fetchImpl } = makeClient(jsonResponse(204, null))
    client.setCsrfToken('token-1')
    await client.logout()

    fetchImpl.mockResolvedValue(jsonResponse(200, { id: 1 }))
    await client.revokeKey(1)
    const [, init] = fetchImpl.mock.calls[1]
    expect((init?.headers as Record<string, string>)['X-CSRF-Token']).toBeUndefined()
  })

  it('查询参数只带非空值', async () => {
    const { client, fetchImpl } = makeClient(jsonResponse(200, { items: [], next_cursor: null }))
    await client.listKeys({ status: 'active', limit: 20, cursor: null })
    const [url] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/v1/keys?status=active&limit=20')
  })

  it('模型名按段编码：斜杠保留，?、#、% 与空格转义', async () => {
    const { client, fetchImpl } = makeClient(jsonResponse(200, {}))
    await client.modelDetail('acme/weird?name#1 %2')
    const [url] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/v1/models/acme/weird%3Fname%231%20%252')
  })
})

describe('HttpApiClient 的 429', () => {
  it('把 Retry-After 解析成整数秒', async () => {
    const { client } = makeClient(
      jsonResponse(429, { error: { code: 'rate_limited', message: 'slow down' } }, { 'Retry-After': '7' }),
    )
    await expect(client.models()).rejects.toMatchObject({ status: 429, retryAfter: 7 })
  })

  it('没有这个头时 retryAfter 是 null，不瞎猜', async () => {
    const { client } = makeClient(
      jsonResponse(429, { error: { code: 'rate_limited', message: 'slow down' } }),
    )
    await expect(client.models()).rejects.toMatchObject({ status: 429, retryAfter: null })
  })
})

describe('HttpApiClient 的错误与会话', () => {
  it('把错误体解成 ApiError，带 code 与 request_id', async () => {
    const { client } = makeClient(
      jsonResponse(403, {
        error: { code: 'csrf_failed', message: 'missing csrf token', request_id: 'req-9' },
      }),
    )
    client.setCsrfToken('stale')

    await expect(client.revokeKey(3)).rejects.toMatchObject({
      status: 403,
      code: 'csrf_failed',
      requestId: 'req-9',
    })
  })

  it('会话接口 401 返回 null，其他错误照常抛出', async () => {
    const unauthorized = makeClient(jsonResponse(401, { error: { code: 'unauthenticated' } }))
    await expect(unauthorized.client.session()).resolves.toBeNull()

    const broken = makeClient(jsonResponse(500, { error: { code: 'boom', message: 'boom' } }))
    await expect(broken.client.session()).rejects.toBeInstanceOf(ApiError)
  })

  it('204 返回 undefined', async () => {
    const { client } = makeClient(jsonResponse(204, null))
    await expect(client.logout()).resolves.toBeUndefined()
  })
})

describe('HttpApiClient 的会话失效通知', () => {
  const unauthorized = () => jsonResponse(401, { error: { code: 'unauthenticated' } })
  const session = {
    user: {
      id: 1,
      username: 'demo',
      display_name: null,
      email: null,
      status: 'active',
      created_at: '',
    },
    csrf_token: 'fresh-token',
    expires_at: '2026-10-28T00:00:00Z',
  }

  it('业务请求收到 401 时通知，错误照常抛出', async () => {
    const { client } = makeClient(unauthorized())
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await expect(client.models()).rejects.toMatchObject({ status: 401 })
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('403 不通知：会话是好的，只是这次操作没权限', async () => {
    const { client } = makeClient(jsonResponse(403, { error: { code: 'forbidden' } }))
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await expect(client.revokeKey(3)).rejects.toMatchObject({ status: 403 })
    expect(handler).not.toHaveBeenCalled()
  })

  it('会话探测的 401 不通知：匿名态本来就会拿到它', async () => {
    const { client } = makeClient(unauthorized())
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await expect(client.session()).resolves.toBeNull()
    expect(handler).not.toHaveBeenCalled()
  })

  it('登录失败的 401 不通知：口令不对不是会话失效', async () => {
    const { client } = makeClient(unauthorized())
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await expect(client.login('demo', 'wrong')).rejects.toMatchObject({ status: 401 })
    expect(handler).not.toHaveBeenCalled()
  })

  it('三个请求同时 401 只通知一次', async () => {
    const { client } = makeClient(unauthorized())
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await Promise.allSettled([client.models(), client.wallet(), client.listKeys()])
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('登录成功后复位：下一次会话失效照样通知', async () => {
    const { client, fetchImpl } = makeClient(unauthorized())
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await expect(client.models()).rejects.toMatchObject({ status: 401 })
    fetchImpl.mockResolvedValueOnce(jsonResponse(200, session))
    await client.login('demo', 'password')
    fetchImpl.mockResolvedValue(unauthorized())
    await expect(client.models()).rejects.toMatchObject({ status: 401 })

    expect(handler).toHaveBeenCalledTimes(2)
  })

  it('会话恢复成功也复位：刷新页面之后仍是新的一轮', async () => {
    const { client, fetchImpl } = makeClient(unauthorized())
    const handler = vi.fn()
    client.setUnauthorizedHandler(handler)

    await expect(client.models()).rejects.toMatchObject({ status: 401 })
    fetchImpl.mockResolvedValueOnce(jsonResponse(200, session))
    await client.session()
    fetchImpl.mockResolvedValue(unauthorized())
    await expect(client.models()).rejects.toMatchObject({ status: 401 })

    expect(handler).toHaveBeenCalledTimes(2)
  })

  it('后注册的回调覆盖前一个，注销后不再通知', async () => {
    const { client } = makeClient(unauthorized())
    const first = vi.fn()
    const second = vi.fn()
    client.setUnauthorizedHandler(first)
    client.setUnauthorizedHandler(second)

    await expect(client.models()).rejects.toMatchObject({ status: 401 })
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)

    const { client: quiet } = makeClient(unauthorized())
    quiet.setUnauthorizedHandler(first)
    quiet.setUnauthorizedHandler(null)
    await expect(quiet.models()).rejects.toMatchObject({ status: 401 })
    expect(first).not.toHaveBeenCalled()
  })
})
