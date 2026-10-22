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
  user: User | null
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
  setUser: (user: User) => void
}

const AuthContext = createContext<AuthValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>('loading')
  const [user, setUser] = useState<User | null>(null)
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
      setStatus('anonymous')
    })
    return () => api.setUnauthorizedHandler(null)
  }, [])

  const login = useCallback(async (username: string, password: string) => {
    const session = await api.login(username, password)
    api.setCsrfToken(session.csrf_token)
    setUser(session.user)
    setStatus('authenticated')
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      api.setCsrfToken(null)
      setUser(null)
      setStatus('anonymous')
    }
  }, [])

  const value = useMemo(
    () => ({ status, user, login, logout, setUser }),
    [status, user, login, logout],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthValue {
  const context = useContext(AuthContext)
  if (!context) throw new Error('useAuth must be used inside AuthProvider')
  return context
}
