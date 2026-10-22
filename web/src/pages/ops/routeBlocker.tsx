import { useEffect, useRef } from 'react'
import { useBlocker } from 'react-router-dom'
import { useI18n } from '../../i18n'
import { useConfirm } from './common-helpers'

/**
 * 站内跳转的阻止器。
 *
 * 只在 data router 下渲染（见 `useUnsavedGuard`）。被拦下时借同一个确认框问一句：
 * 「放弃」才放行，「留下」就把这次跳转作废、停在原页。
 */
export function RouteBlocker() {
  const { t } = useI18n()
  const confirm = useConfirm()
  const blocker = useBlocker(true)
  const { state } = blocker
  // 确认框的按钮回调晚于这一次渲染才触发，那时闭包里的 blocker 已经旧了：
  // 每次渲染把最新的那个塞进 ref，回调取到的才是当前的阻止器。
  const blockerRef = useRef(blocker)
  blockerRef.current = blocker
  const requestRef = useRef(confirm.request)
  requestRef.current = confirm.request

  useEffect(() => {
    if (state !== 'blocked') return
    requestRef.current({
      title: t('ops.discardTitle'),
      body: t('ops.discardBody'),
      label: t('ops.discardConfirm'),
      danger: true,
      run: async () => {
        const current = blockerRef.current
        if (current.state === 'blocked') current.proceed()
      },
      // 留下、Esc、点遮罩都是「不走了」：这次跳转要作废，否则下一个跳转不会
      // 再经过阻止器。
      onCancel: () => {
        const current = blockerRef.current
        if (current.state === 'blocked') current.reset()
      },
    })
  }, [state, t])

  return <>{confirm.dialog}</>
}
