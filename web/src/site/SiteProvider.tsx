import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { api } from '../api'
import type { Meta } from '../api/types'

const fallback: Meta = {
  site_name: 'larkax',
  api_base_url: `${typeof window === 'undefined' ? '' : window.location.origin}/v1`,
  currency: 'USD',
  timezone: 'UTC',
  locales: ['en', 'zh'],
  features: { registration: false, password_reset: 'admin', topup: false },
}

interface SiteValue {
  meta: Meta
  ready: boolean
}

const SiteContext = createContext<SiteValue>({ meta: fallback, ready: false })

/** 站点名、base URL、币种与时区。取不到时用本地默认值，页面照常渲染。 */
export function SiteProvider({ children }: { children: ReactNode }) {
  const [meta, setMeta] = useState<Meta>(fallback)
  const [ready, setReady] = useState(false)

  useEffect(() => {
    let alive = true
    api
      .meta()
      .then((next) => {
        if (alive) {
          setMeta(next)
          setReady(true)
        }
      })
      .catch(() => {
        if (alive) setReady(true)
      })
    return () => {
      alive = false
    }
  }, [])

  const value = useMemo(() => ({ meta, ready }), [meta, ready])
  return <SiteContext.Provider value={value}>{children}</SiteContext.Provider>
}

export function useSite(): SiteValue {
  return useContext(SiteContext)
}
