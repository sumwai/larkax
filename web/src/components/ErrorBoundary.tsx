import { Component, useMemo, type ErrorInfo, type ReactNode } from 'react'
import { useRouteError } from 'react-router-dom'
import { detectLang } from '../i18n/lang'
import { en } from '../i18n/en'
import { zh } from '../i18n/zh'
import { Button, Card } from './ui'

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
    <div className="grid min-h-dvh place-items-center bg-background px-4">
      <Card role="alert" className="w-full max-w-md p-6">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">{text['errors.fatalTitle']}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{text['errors.fatalBody']}</p>
        </div>
        <div className="mt-4">
          <Button onClick={() => window.location.reload()}>{text['errors.reload']}</Button>
        </div>
        <div className="mt-4">
          <div className="pb-1 text-caps text-faint">{text['errors.diagnostics']}</div>
          <pre className="overflow-x-auto rounded-sm bg-muted p-3 font-mono text-xs leading-5">
            {diagnostics}
          </pre>
        </div>
      </Card>
    </div>
  )
}

/**
 * 路由树的错误页。
 *
 * 整棵路由只在挂 `errorElement` 的那一层有兜底，任何一页的渲染期异常都冒到这里；
 * 这里把它交回与最外层边界同一个致命错误页，免得转成 data router 之后换了一张
 * router 自带的错误页。
 *
 * 只能用在路由的 `errorElement` 里：`useRouteError` 在别处拿不到值。
 */
export function RootRouteError() {
  const error = useRouteError()
  // 时间只在错误值真的换了时重取，不跟着重渲染漂。
  const detail = useMemo<BoundaryDetail>(() => ({ error, at: new Date() }), [error])
  return <FatalErrorPage detail={detail} />
}
