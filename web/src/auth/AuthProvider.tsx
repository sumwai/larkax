import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { api } from '../api'
import type { User } from '../api/types'

type AuthStatus = 'loading' | 'anonymous' | 'authenticated'

interface AuthValue {
  status: AuthStatus
  /**
   * 是不是「会话过期」掉下来的匿名，而不是用户自己点了退出登录。
   *
   * 只有 401 把它置 true，`login` / `logout` 都置 false。两者的去向不同：退出
   * 登录是用户的决定，页面直接换掉；会话过期时表单里可能还有没保存的东西，得先
   * 问一句。若把退出登录也算成过期，界面上会冒出「会话已过期」，还会与
   * `AppShell` 自己那句 `navigate('/login')` 抢同一次导航。
   *
   * 门槛（`RequireAuth`）的判据是 **`sessionExpired` 且这一页有未保存的改动**，
   * 两个条件缺一不可——只读过期标志就当成「凡过期都留下」，干净页面会被无故留在
   * 原地；反过来只读脏状态，退出登录也会走成过期那条路。
   */
  sessionExpired: boolean
  user: User | null
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
  setUser: (user: User) => void
}

const AuthContext = createContext<AuthValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>('loading')
  const [user, setUser] = useState<User | null>(null)
  const [sessionExpired, setSessionExpired] = useState(false)
  // 回调只在挂载时注册一次，闭包里读不到后来的状态；用 ref 读「现在」。
  // 否则登录之后它还把自己当匿名，下一次会话失效就不降级了。
  const statusRef = useRef(status)
  statusRef.current = status

  useEffect(() => {
    let alive = true
    api
      .session()
      .then((session) => {
        if (!alive) return
        if (session) {
          api.setCsrfToken(session.csrf_token)
          setUser(session.user)
          setStatus('authenticated')
        } else {
          setStatus('anonymous')
        }
      })
      .catch(() => {
        if (alive) setStatus('anonymous')
      })
    return () => {
      alive = false
    }
  }, [])

  useEffect(() => {
    api.setUnauthorizedHandler(() => {
      // 只有真有一个会话来失效，才降级。匿名态收到 401 不稀奇：登出前发出、
      // 晚到的请求，落地页的公开接口，都可能。会话还在恢复（loading）时同理，
      // 这时改状态会让门槛提前把人送去登录页。
      if (statusRef.current !== 'authenticated') return
      // 令牌已经没用了，留着只会让下一条写请求带着一个必然过不去的值。
      api.setCsrfToken(null)
      setUser(null)
      setSessionExpired(true)
      setStatus('anonymous')
    })
    return () => api.setUnauthorizedHandler(null)
  }, [])

  const login = useCallback(async (username: string, password: string) => {
    const session = await api.login(username, password)
    api.setCsrfToken(session.csrf_token)
    setUser(session.user)
    setSessionExpired(false)
    setStatus('authenticated')
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      api.setCsrfToken(null)
      setUser(null)
      // 主动退出不是过期：脏页面的去向由原来的那条路（未保存改动）决定。
      setSessionExpired(false)
      setStatus('anonymous')
    }
  }, [])

  const value = useMemo(
    () => ({ status, sessionExpired, user, login, logout, setUser }),
    [status, sessionExpired, user, login, logout],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthValue {
  const context = useContext(AuthContext)
  if (!context) throw new Error('useAuth must be used inside AuthProvider')
  return context
}

/**
 * 读得到会话就读，读不到当没有。
 *
 * 给那些「有会话时要多做一件事、没有也照常工作」的组件用：比如页面里的
 * 未保存改动阻止器，页面单测裸渲染时没有 `AuthProvider`，不该因此报错。
 */
export function useOptionalAuth(): AuthValue | null {
  return useContext(AuthContext)
}

/**
 * 会话失效后去登录页的地址。
 *
 * 原地址带在 `?next=` 上而不是路由 state 里：会话失效是被动发生的，用户可能
 * 顺手刷新一次登录页，state 一刷就没了，`next` 还在。
 */
export function loginPath(location: { pathname: string; search: string; hash: string }): string {
  const next = `${location.pathname}${location.search}${location.hash}`
  return `/login?next=${encodeURIComponent(next)}`
}
