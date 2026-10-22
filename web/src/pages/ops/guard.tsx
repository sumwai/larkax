import { useContext, useEffect, type ReactNode } from 'react'
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

/**
 * 还没保存的改动要挡住的两个出口：刷新/关标签页，与站内跳转。
 *
 * 返回的东西挂到树上去（与弹窗自己的 `confirm.dialog` 同级）。没有改动、或者
 * 当前不在 data router 下时返回 null。
 */
export function useUnsavedGuard(dirty: boolean): ReactNode {
  useBeforeUnload(dirty)
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
