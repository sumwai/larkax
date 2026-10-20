import { HttpApiClient } from './client'
import { MockApiClient } from './mock'
import type { ApiClient } from './types'

/**
 * 页面唯一的数据入口。后端就绪后把 `VITE_API_MODE` 设成 `http` 即可切换，
 * 页面代码不动。
 */
export const api: ApiClient =
  import.meta.env.VITE_API_MODE === 'http'
    ? new HttpApiClient({ baseUrl: import.meta.env.VITE_API_BASE || '/api/v1' })
    : new MockApiClient()

export { ApiError, HttpApiClient } from './client'
export type * from './types'
