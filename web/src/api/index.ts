import { HttpApiClient } from './client'
import { MockApiClient } from './mock'
import type { ApiClient } from './types'

/**
 * 是否跑在演示数据上。
 *
 * 这是选 client 的唯一判据，界面上的「演示数据」横幅也读它——两处各写一份
 * `import.meta.env` 判断，改了一处就会给出自相矛盾的界面。
 */
export const isMockMode = import.meta.env.VITE_API_MODE !== 'http'

/**
 * 页面唯一的数据入口。后端就绪后把 `VITE_API_MODE` 设成 `http` 即可切换，
 * 页面代码不动。
 */
export const api: ApiClient = isMockMode
  ? new MockApiClient()
  : new HttpApiClient({ baseUrl: import.meta.env.VITE_API_BASE || '/api/v1' })

export { ApiError, HttpApiClient } from './client'
export type * from './types'
