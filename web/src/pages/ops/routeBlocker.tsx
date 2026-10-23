import { useCallback, useEffect, useRef } from 'react'
import { useBlocker, useLocation, useNavigate } from 'react-router-dom'
import { loginPath, useOptionalAuth } from '../../auth/AuthProvider'
import { useI18n } from '../../i18n'
import { useConfirm } from './common-helpers'
import { useUnsavedLeaveRef } from './guard'

/**
 * 未保存改动的阻止器。
 *
 * 只在 data router 下渲染（见 `useUnsavedGuard`）。两个来源共用这一个确认框：
 *
 * - 站内跳转：被 `useBlocker` 拦下；
 * - 会话失效：`AuthProvider` 把状态降成匿名，门槛因为这一页有未保存的改动而
 *   不直接跳走（见 `RequireAuth`），由这里先问一句「去登录还是留下」。
 *
 * 两个来源落在同一份 `useConfirm` 状态上，所以同一时刻只会有这一个框：失效恰好
 * 赶在一次被拦下的跳转上时，后到的会话失效说明会顶掉「放弃修改」那一句。
 */
export function RouteBlocker() {
  const { t } = useI18n()
  const confirm = useConfirm()
  const navigate = useNavigate()
  const location = useLocation()
  // 页面单测里没有 `AuthProvider`：那就只管站内跳转这一路。主动退出登录不算
  // 过期——那条路仍由「未保存的改动」那一句来问。
  const expired = useOptionalAuth()?.sessionExpired === true

  // 「去登录」本身也是一次站内导航，不先让开的话会被自己拦下来——确认框会再弹
  // 一次，人永远走不掉。用 ref 而不是 state：确认框的回调触发时，阻止器判定要
  // 读到「刚刚」这个值。
  const skipRef = useRef(false)
  const blocker = useBlocker(() => !skipRef.current)
  const { state } = blocker
  // 确认框的按钮回调晚于这一次渲染才触发，那时闭包里的 blocker 已经旧了：
  // 每次渲染把最新的那个塞进 ref，回调取到的才是当前的阻止器。
  const blockerRef = useRef(blocker)
  blockerRef.current = blocker
  const requestRef = useRef(confirm.request)
  requestRef.current = confirm.request
  // 同一次失效只问一次；重新登录（状态回到 authenticated）之后再来一次还要能问。
  const askedRef = useRef(false)

  const leaveForLogin = useCallback(() => {
    // 被拦下的那次跳转已经不是用户想去的地方了：作废它，改去登录页。
    const current = blockerRef.current
    if (current.state === 'blocked') current.reset()
    skipRef.current = true
    navigate(loginPath(location), { replace: true })
  }, [navigate, location])

  // 横幅上那个「去登录」也要走同一个入口——它不能直接 navigate，否则会被这一层
  // 拦下来。卸载时撤回，免得留下一个指向已卸载组件的回调。
  const leaveRef = useUnsavedLeaveRef()
  leaveRef.current = leaveForLogin
  useEffect(
    () => () => {
      leaveRef.current = null
    },
    [leaveRef],
  )

  // 让开阻止器只该管「去登录这一次」。这条复位是防御性的：去登录那次导航成功
  // 之后组件就随页面卸载了，端到端层**观测不到**它是否真的跑过（用例只能钉住
  // 「取消之后下一次站内跳转仍被拦」，而取消这条路上 `skipRef` 从来没被置真）。
  // 留着是为了那种「导航没走成、组件还活着」的情况：不复位的话此后每一次站内
  // 跳转都会被静默放行。将来若有人让守卫在离开后多活一次渲染，这一行就变成
  // 可观测的，届时补用例——现在别假装它被钉住了。
  useEffect(() => {
    if (state === 'unblocked') skipRef.current = false
  }, [state])

  useEffect(() => {
    if (expired && !askedRef.current) {
      askedRef.current = true
      requestRef.current({
        // 这一句不能只说「未保存的修改」：用户得先知道为什么突然要走。
        title: t('ops.sessionExpiredTitle'),
        body: t('ops.sessionExpiredBody'),
        label: t('ops.discardConfirm'),
        danger: true,
        run: async () => leaveForLogin(),
        // 留下、Esc、点遮罩都是「不走了」：页面原地保留，被拦的那次跳转作废。
        // 首页那条横幅（`SessionExpiredNotice`）会一直挂在那里。
        onCancel: () => {
          skipRef.current = false
          const current = blockerRef.current
          if (current.state === 'blocked') current.reset()
        },
      })
      return
    }
    if (!expired) askedRef.current = false
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
  }, [expired, state, t, leaveForLogin])

  return <>{confirm.dialog}</>
}
