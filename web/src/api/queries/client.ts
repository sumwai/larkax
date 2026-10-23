import { QueryClient } from '@tanstack/react-query'
import { api } from '../index'

/**
 * 数据层的唯一 QueryClient。失败不自动重试（写动作的错误要原样给人看）；
 * 窗口聚焦不重拉（列表页自己决定什么时候刷新）。
 */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 0,
      refetchOnWindowFocus: false,
      staleTime: 15_000,
    },
  },
})

export { api }
