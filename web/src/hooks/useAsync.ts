import { useCallback, useEffect, useRef, useState, type DependencyList } from 'react'

interface AsyncState<T> {
  data: T | null
  error: Error | null
  loading: boolean
}

/**
 * 读一次数据，参数变化时重读。
 *
 * 参数按 JSON 比较（都是字符串、数字、数组），页面不需要自己写 memo。
 */
export function useAsync<T>(loader: () => Promise<T>, deps: DependencyList) {
  const loaderRef = useRef(loader)
  loaderRef.current = loader

  const [state, setState] = useState<AsyncState<T>>({ data: null, error: null, loading: true })
  const [reloadToken, setReloadToken] = useState(0)
  const key = JSON.stringify(deps)

  useEffect(() => {
    let alive = true
    setState((previous) => ({ ...previous, loading: true, error: null }))
    loaderRef
      .current()
      .then((data) => {
        if (alive) setState({ data, error: null, loading: false })
      })
      .catch((error: unknown) => {
        if (!alive) return
        setState((previous) => ({
          ...previous,
          error: error instanceof Error ? error : new Error(String(error)),
          loading: false,
        }))
      })
    return () => {
      alive = false
    }
  }, [key, reloadToken])

  const reload = useCallback(() => setReloadToken((token) => token + 1), [])
  return { ...state, reload }
}
