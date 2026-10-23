import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { UNSAFE_DataRouterContext } from 'react-router-dom'
import { useI18n } from '../../i18n'
import type { ConfirmHandle } from './common-helpers'
import { RouteBlocker } from './routeBlocker'

/** 从打开时的那份值算起，这一页有没有被改过。 */
export function isDirty<T>(current: T, initial: T): boolean {
  return JSON.stringify(current) !== JSON.stringify(initial)
}

/**
 * 关闭表单前先问一句。
 *
 * Esc、点遮罩、右上角的关闭钮最后都走 Modal 的 onClose，拦在这一处就都拦住了。
 */
export function useCloseGuard(confirm: ConfirmHandle, dirty: boolean, close: () => void) {
  const { t } = useI18n()
  return () => {
    if (!dirty) return close()
    confirm.request({
      title: t('ops.discardTitle'),
      body: t('ops.discardBody'),
      label: t('ops.discardConfirm'),
      danger: true,
      run: async () => close(),
    })
  }
}

interface UnsavedChanges {
  dirty: boolean
  setDirty: (dirty: boolean) => void
  /**
   * 会话失效后「去登录」的入口。
   *
   * 由页面里的 `RouteBlocker` 登记：只有它知道怎么先让开自己的阻止器，否则这次
   * 导航会被它自己拦下来。横幅（`SessionExpiredNotice`）只是调用它。
   */
  leaveRef: { current: (() => void) | null }
}

/**
 * 脏状态登记处。
 *
 * 页面自己知道有没有没保存的改动，门槛（`RequireAuth`）却不知道——而会话失效时
 * 正是门槛要把人送走。跳走会连带卸载页面，确认框根本没机会出现，所以页面先把
 * 脏状态登记在这里，门槛据此决定：干净就直接跳，脏就留在原地让 `RouteBlocker`
 * 先问一句。
 *
 * 没有默认值：忘了挂 provider 等于「永远不脏」，会话失效时静默丢表单，而这在
 * 开发时看不出来。只有测试环境给一个落空实现——页面单测是裸渲染的，不该为此
 * 多铺一层 provider；其余环境一概抛错。
 */
const MISSING_PROVIDER_FALLBACK: UnsavedChanges = {
  dirty: false,
  setDirty: () => {},
  leaveRef: { current: null },
}

const UnsavedChangesContext = createContext<UnsavedChanges | null>(null)

function useRegistry(consumer: string): UnsavedChanges {
  const registry = useContext(UnsavedChangesContext)
  if (registry) return registry
  if (import.meta.env.MODE !== 'test') {
    throw new Error(`${consumer} must be used inside UnsavedGuardProvider`)
  }
  return MISSING_PROVIDER_FALLBACK
}

export function UnsavedGuardProvider({ children }: { children: ReactNode }) {
  const [dirty, setDirty] = useState(false)
  const leaveRef = useRef<(() => void) | null>(null)
  // `setDirty` 自己就是稳定的，value 只随 `dirty` 换，读它的人不会白渲染。
  const value = useMemo(() => ({ dirty, setDirty, leaveRef }), [dirty])
  return <UnsavedChangesContext.Provider value={value}>{children}</UnsavedChangesContext.Provider>
}

/** 当前这一页有没有没保存的改动。 */
export function useUnsavedChanges(): boolean {
  return useRegistry('useUnsavedChanges').dirty
}

/** 会话失效横幅要用的入口（见 `UnsavedChanges.leaveRef`）。 */
export function useUnsavedLeaveRef(): { current: (() => void) | null } {
  return useRegistry('useUnsavedLeaveRef').leaveRef
}

/**
 * 还没保存的改动要挡住的两个出口：刷新/关标签页，与站内跳转。
 *
 * 会话失效是第三个出口，同一条确认路径，见 `RouteBlocker`。脏状态同时登记给
 * 门槛用（见 `UnsavedChangesContext`）。
 *
 * 返回的东西挂到树上去（与弹窗自己的 `confirm.dialog` 同级）。没有改动、或者
 * 当前不在 data router 下时返回 null。
 */
export function useUnsavedGuard(dirty: boolean): ReactNode {
  useBeforeUnload(dirty)
  const { setDirty } = useRegistry('useUnsavedGuard')
  useEffect(() => {
    setDirty(dirty)
  }, [setDirty, dirty])
  // 卸载时一定要撤回：下一任页面会被上一任的脏状态卡在门槛上。
  useEffect(() => () => setDirty(false), [setDirty])
  // 阻止器只在 data router 下存在；页面单测直接渲染页面时没有它，`useBlocker`
  // 会抛错，所以先认 context、再有条件地挂上 `<RouteBlocker />`。
  //
  // 为什么用 `UNSAFE_DataRouterContext` 而不是自己搭一层 provider：自己搭的话，
  // App 忘了包那层 provider 时拦截会静默失效——没有任何地方会报错。这个 context
  // 是 react-router 自己判断「这里有没有 data router」用的那一份；若上游把它删掉，
  // tsc 会直接报错，升级 react-router 前先确认它还在。
  const dataRouter = useContext(UNSAFE_DataRouterContext)
  if (!dataRouter || !dirty) return null
  return <RouteBlocker />
}

/**
 * 刷新、关标签页时让浏览器自己问一句。
 *
 * 只在真的改过时挂监听：没改过的页面刷新不该被打断。
 */
function useBeforeUnload(dirty: boolean) {
  useEffect(() => {
    if (!dirty) return
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      // 旧浏览器只认 returnValue 才会弹；现代浏览器忽略这里给的自定义文案。
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])
}
