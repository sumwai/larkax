import { Component, useMemo, type ErrorInfo, type ReactNode } from 'react'
import { useLocation, useRouteError } from 'react-router-dom'
import { useI18n } from '../i18n'
import { detectLang } from '../i18n/lang'
import { en } from '../i18n/en'
import { zh } from '../i18n/zh'
import { CopyButton } from './ui'

export interface BoundaryDetail {
  /** 被抛出的原值。`throw` 不要求是 Error，所以类型是 unknown。 */
  error: unknown
  /** 捕获到错误的时间，取自 getDerivedStateFromError；不随重渲染漂移。 */
  at: Date
}

interface BoundaryProps {
  children: ReactNode
  fallback: (detail: BoundaryDetail) => ReactNode
}

interface BoundaryState {
  caught: BoundaryDetail | null
}

/**
 * 渲染期异常的兜底。
 *
 * 只接 React 渲染、生命周期与构造函数里抛出的异常。事件处理器里的异常、
 * Promise 的 rejection、以及这个边界自己渲染 fallback 时抛的错都不在它的
 * 范围内——最后一种要靠更外层的边界接。
 *
 * React 18 会把懒加载 chunk 的下载失败当作渲染期异常抛回来，所以这一类
 * 会被它接住；本项目目前没有懒路由，这条只是说明适用范围。
 */
export class ErrorBoundary extends Component<BoundaryProps, BoundaryState> {
  state: BoundaryState = { caught: null }

  static getDerivedStateFromError(error: unknown): BoundaryState {
    return { caught: { error, at: new Date() } }
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    // 控制台保留 React 给的完整错误与组件栈，不做 preventDefault 之类的吞掉。
    console.error(error, info)
  }

  render() {
    if (this.state.caught) return this.props.fallback(this.state.caught)
    return this.props.children
  }
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

function diagnosticLines(detail: BoundaryDetail, pathname: string, labels: DiagnosticLabels) {
  return [
    `${labels.time}: ${detail.at.toISOString()}`,
    `${labels.route}: ${pathname}`,
    `${labels.message}: ${messageOf(detail.error)}`,
  ].join('\n')
}

interface DiagnosticLabels {
  time: string
  route: string
  message: string
}

/**
 * 路由内容的边界。
 *
 * 这条必须挂在外壳内部、只包住 `<Outlet />`：出错时侧栏与顶栏还在，用户能
 * 直接切去别的页面。`key={pathname}` 让每次路由变化都换一个新的边界实例，
 * 旧路由的错误状态随之丢掉——不用在 componentDidUpdate 里手比地址。
 */
export function RouteErrorBoundary({ children }: { children: ReactNode }) {
  const { pathname } = useLocation()
  return (
    <ErrorBoundary
      key={pathname}
      fallback={(detail) => <RouteErrorPage detail={detail} pathname={pathname} />}
    >
      {children}
    </ErrorBoundary>
  )
}

function RouteErrorPage({ detail, pathname }: { detail: BoundaryDetail; pathname: string }) {
  const { t } = useI18n()
  const diagnostics = diagnosticLines(detail, pathname, {
    time: t('errors.time'),
    route: t('errors.route'),
    message: t('errors.message'),
  })

  return (
    <div className="card panel error-page" role="alert">
      <div>
        <h1 className="error-page-title">{t('errors.renderTitle')}</h1>
        <p className="error-page-body">{t('errors.renderBody')}</p>
      </div>
      <div className="error-page-actions">
        <button type="button" className="btn" onClick={() => window.location.reload()}>
          {t('errors.reload')}
        </button>
        <CopyButton text={diagnostics} label={t('errors.copyDiagnostics')} />
      </div>
      <div>
        <div className="error-page-label">{t('errors.diagnostics')}</div>
        <pre className="error-page-diagnostics">{diagnostics}</pre>
      </div>
    </div>
  )
}

/**
 * 最外层边界：包住 App 的整棵 Providers 子树。
 *
 * 它不能依赖任何 context——I18nProvider 自己就可能正在抛错。所以文案直接从
 * 语言包取值、语言由 `detectLang()` 判定（与 Provider 共用同一份实现），
 * 不做 `useI18n()`，也不渲染依赖 toast / 主题的组件。
 */
export function FatalErrorBoundary({ children }: { children: ReactNode }) {
  return (
    <ErrorBoundary fallback={(detail) => <FatalErrorPage detail={detail} />}>
      {children}
    </ErrorBoundary>
  )
}

function FatalErrorPage({ detail }: { detail: BoundaryDetail }) {
  const text = detectLang() === 'zh' ? zh : en
  const pathname = typeof window === 'undefined' ? '' : window.location.pathname
  const diagnostics = diagnosticLines(detail, pathname, {
    time: text['errors.time'],
    route: text['errors.route'],
    message: text['errors.message'],
  })

  return (
    <div className="auth-page">
      <div className="card auth-card" role="alert">
        <div className="auth-head">
          <h1 className="error-page-title">{text['errors.fatalTitle']}</h1>
          <p className="error-page-body">{text['errors.fatalBody']}</p>
        </div>
        <div className="error-page-actions">
          <button type="button" className="btn" onClick={() => window.location.reload()}>
            {text['errors.reload']}
          </button>
        </div>
        <div>
          <div className="error-page-label">{text['errors.diagnostics']}</div>
          <pre className="error-page-diagnostics">{diagnostics}</pre>
        </div>
      </div>
    </div>
  )
}

/**
 * data router 根路由的错误页。
 *
 * 根级页面（落地页、登录页、404）没有内层边界，渲染期异常会被路由器的
 * `errorElement` 接住；这里把它交回与最外层边界同一个致命错误页，免得
 * 转成 data router 之后换了一张 router 自带的错误页。
 *
 * 只能用在路由的 `errorElement` 里：`useRouteError` 在别处拿不到值。
 */
export function RootRouteError() {
  const error = useRouteError()
  // 时间只在错误值真的换了时重取，不跟着重渲染漂。
  const detail = useMemo<BoundaryDetail>(() => ({ error, at: new Date() }), [error])
  return <FatalErrorPage detail={detail} />
}
