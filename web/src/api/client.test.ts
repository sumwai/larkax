import { describe, expect, it, vi } from 'vitest'
import { ApiError, HttpApiClient } from './client'

interface Call {
  url: string
  init: RequestInit
}

/** 只实现被测代码用到的两个成员；响应体与状态码由各用例给。 */
function respond(
  status: number,
  body: unknown,
  options: { statusText?: string; headers?: Record<string, string> } = {},
): Response {
  const headers = new Map(
    Object.entries(options.headers ?? {}).map(([key, value]) => [key.toLowerCase(), value]),
  )
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: options.statusText ?? '',
    headers: { get: (name: string) => headers.get(name.toLowerCase()) ?? null },
    json: async () => {
      if (body === undefined) throw new SyntaxError('Unexpected end of JSON input')
      return body
    },
  } as unknown as Response
}

function clientWith(handler: (call: Call) => Response) {
  const calls: Call[] = []
  const fetchImpl = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = { url: String(input), init: init ?? {} }
    calls.push(call)
    return Promise.resolve(handler(call))
  }) as unknown as typeof fetch
  return { client: new HttpApiClient({ baseUrl: '/api/v1/', fetchImpl }), calls }
}

describe('HttpApiClient 请求的形状', () => {
  it('基址去掉末尾斜杠，读请求带 Accept 与同源凭证，并解开 {user} 包装', async () => {
    const { client, calls } = clientWith(() => respond(200, { user: { id: 7, username: 'alice' } }))

    const user = await client.me()

    expect(user).toMatchObject({ id: 7, username: 'alice' })
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/me')
    expect(calls[0].init.method).toBe('GET')
    expect(calls[0].init.credentials).toBe('same-origin')
    expect(calls[0].init.headers).toEqual({ Accept: 'application/json' })
    expect(calls[0].init.body).toBeUndefined()
  })

  it('没给基址时用 /api/v1', async () => {
    const calls: Call[] = []
    const fetchImpl = ((input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init: init ?? {} })
      return Promise.resolve(respond(200, { site_name: 'larkax' }))
    }) as unknown as typeof fetch

    await new HttpApiClient({ fetchImpl }).meta()

    expect(calls[0].url).toBe('/api/v1/meta')
  })

  it('查询串跳过空值，数组参数各出现一次', async () => {
    const { client, calls } = clientWith(() => respond(200, { items: [], count: 0 }))

    await client.usageRequests({ page: 2, limit: 20, status: 'settled', key_id: undefined })
    await client.walletEntries({ type: ['topup', 'refund'] })

    expect(calls[0].url).toBe('/api/v1/usage/requests?page=2&limit=20&status=settled')
    expect(calls[1].url).toBe('/api/v1/wallet/entries?type=topup&type=refund')
  })

  it('204 不读响应体，直接当成功', async () => {
    const { client, calls } = clientWith(() =>
      respond(204, undefined, { statusText: 'No Content' }),
    )

    await expect(client.admin.deleteModel('gpt-4.1')).resolves.toBeUndefined()

    expect(calls[0].url).toBe('/api/v1/admin/models/gpt-4.1')
    expect(calls[0].init.method).toBe('DELETE')
  })

  it('写请求带上会话拿到的 CSRF 令牌，读请求不带', async () => {
    const { client, calls } = clientWith((call) =>
      call.url.endsWith('/auth/login')
        ? respond(200, { user: { id: 1, username: 'alice' }, csrf_token: 'fresh-token' })
        : respond(200, { items: [], next_cursor: null }),
    )

    await client.login('alice', 'secret')
    // 登录成功响应里的 csrf_token 会被收下，下一条写请求就带上它。
    await client.updateMe({ display_name: 'Ada' })
    await client.listKeys()

    expect(calls[1].init.headers).toMatchObject({
      'Content-Type': 'application/json',
      'X-CSRF-Token': 'fresh-token',
    })
    expect(calls[1].init.body).toBe('{"display_name":"Ada"}')
    expect(calls[2].init.headers).not.toHaveProperty('X-CSRF-Token')
  })
})

describe('HttpApiClient 的错误映射', () => {
  it('非 2xx 带出状态码、error 体里的 code / message / request_id', async () => {
    const { client } = clientWith(() =>
      respond(404, {
        error: { code: 'model_not_found', message: 'no such model', request_id: 'req_42' },
      }),
    )

    const error = await client.models().catch((cause: unknown) => cause)

    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({
      status: 404,
      code: 'model_not_found',
      message: 'no such model',
      requestId: 'req_42',
    })
  })

  it('响应体不是 JSON 时退回状态文本', async () => {
    const { client } = clientWith(() => respond(502, undefined, { statusText: 'Bad Gateway' }))

    const error = await client.models().catch((cause: unknown) => cause)

    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({ status: 502, code: 'http_error', message: 'Bad Gateway' })
  })

  it('429 只接受非负整数的 Retry-After', async () => {
    const { client } = clientWith((call) =>
      respond(
        429,
        { error: { code: 'rate_limited', message: 'slow down', request_id: '' } },
        {
          headers: { 'Retry-After': call.url.includes('/wallet') ? '7' : 'soon' },
        },
      ),
    )

    const first = await client.wallet().catch((cause: unknown) => cause)
    const second = await client.usageSummary().catch((cause: unknown) => cause)

    expect(first).toMatchObject({ status: 429, retryAfter: 7 })
    // 'soon' 解析不出秒数，当「没给」，界面自己说「稍后再试」。
    expect(second).toMatchObject({ status: 429, retryAfter: null })
  })

  it('连不上时是 status 0 的网络错误，不是抛原始异常', async () => {
    const fetchImpl = (() => Promise.reject(new Error('offline'))) as unknown as typeof fetch

    const error = await new HttpApiClient({ fetchImpl }).meta().catch((cause: unknown) => cause)

    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({ status: 0, code: 'network_error', message: 'offline' })
  })

  it('业务请求的 401 只通知一次，登录失败的 401 不通知', async () => {
    const { client } = clientWith(() =>
      respond(401, { error: { code: 'unauthorized', message: 'no session', request_id: '' } }),
    )
    const onUnauthorized = vi.fn()
    client.setUnauthorizedHandler(onUnauthorized)

    await expect(client.listKeys()).rejects.toBeInstanceOf(ApiError)
    await expect(client.listKeys()).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)

    // 登录接口上的 401 是「口令不对」，不是会话失效，不能再把人踢回登录页。
    await expect(client.login('alice', 'wrong')).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })
})
