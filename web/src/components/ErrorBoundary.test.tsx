import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Link, MemoryRouter, Outlet, Route, Routes } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { ErrorBoundary, FatalErrorBoundary, RouteErrorBoundary } from './ErrorBoundary'

/** 只在渲染期抛错：事件处理器里的 throw 不在错误边界的范围内。 */
function Boom({ error }: { error: Error }): never {
  throw error
}

function brokenFallback(): never {
  throw new Error('fallback exploded')
}

describe('ErrorBoundary', () => {
  let logged: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    logged = vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    logged.mockRestore()
  })

  it('路由内容抛错时显示说明、重新加载与可复制的诊断，且诊断里没有堆栈', () => {
    const boom = new Error('render exploded')
    const { container } = render(
      <I18nProvider>
        <MemoryRouter initialEntries={['/app/models/vendor/model']}>
          <RouteErrorBoundary>
            <Boom error={boom} />
          </RouteErrorBoundary>
        </MemoryRouter>
      </I18nProvider>,
    )

    expect(screen.getByRole('heading', { name: '这个页面出错了' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新加载' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '复制诊断信息' })).toBeInTheDocument()

    const pre = container.querySelector('pre')
    expect(pre).not.toBeNull()
    const lines = pre!.textContent!.split('\n')
    expect(lines).toHaveLength(3)
    expect(lines[0]).toMatch(/^发生时间: \d{4}-\d{2}-\d{2}T/)
    expect(lines[1]).toBe('当前路由: /app/models/vendor/model')
    expect(lines[2]).toBe('错误信息: render exploded')
    // 终端用户只看到 error.message，堆栈留给控制台。
    expect(pre!.textContent).not.toMatch(/\n\s+at /)
  })

  it('复制按钮交出的是那三行诊断', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })
    const boom = new Error('copy me')
    const { container } = render(
      <I18nProvider>
        <MemoryRouter initialEntries={['/app/usage']}>
          <RouteErrorBoundary>
            <Boom error={boom} />
          </RouteErrorBoundary>
        </MemoryRouter>
      </I18nProvider>,
    )

    const diagnostics = container.querySelector('pre')!.textContent!
    fireEvent.click(screen.getByRole('button', { name: '复制诊断信息' }))
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(diagnostics))
    expect(diagnostics).toContain('当前路由: /app/usage')
    expect(diagnostics).toContain('错误信息: copy me')
  })

  it('控制台保留原 error 对象与组件栈', () => {
    const boom = new Error('keep me')
    render(
      <I18nProvider>
        <MemoryRouter initialEntries={['/app']}>
          <RouteErrorBoundary>
            <Boom error={boom} />
          </RouteErrorBoundary>
        </MemoryRouter>
      </I18nProvider>,
    )

    expect(logged).toHaveBeenCalledWith(
      boom,
      expect.objectContaining({ componentStack: expect.any(String) }),
    )
  })

  it('路由变化后丢掉旧的错误状态', async () => {
    function MiniShell() {
      return (
        <div>
          <nav>
            <Link to="/app">坏页面</Link>
            <Link to="/app/ok">好页面</Link>
          </nav>
          <main>
            <RouteErrorBoundary>
              <Outlet />
            </RouteErrorBoundary>
          </main>
        </div>
      )
    }

    render(
      <I18nProvider>
        <MemoryRouter initialEntries={['/app']}>
          <Routes>
            <Route path="/app" element={<MiniShell />}>
              <Route index element={<Boom error={new Error('boom')} />} />
              <Route path="ok" element={<p>恢复正常</p>} />
            </Route>
          </Routes>
        </MemoryRouter>
      </I18nProvider>,
    )

    expect(await screen.findByRole('heading', { name: '这个页面出错了' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('link', { name: '好页面' }))
    expect(await screen.findByText('恢复正常')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '这个页面出错了' })).toBeNull()
  })

  it('内层边界自己的 fallback 抛错时，外层边界接住它', async () => {
    render(
      <ErrorBoundary fallback={() => <p>外层兜底</p>}>
        <ErrorBoundary fallback={brokenFallback}>
          <Boom error={new Error('boom')} />
        </ErrorBoundary>
      </ErrorBoundary>,
    )

    expect(await screen.findByText('外层兜底')).toBeInTheDocument()
  })

  it('最外层兜底不依赖 I18nProvider', async () => {
    render(
      <FatalErrorBoundary>
        <Boom error={new Error('provider down')} />
      </FatalErrorBoundary>,
    )

    expect(await screen.findByRole('heading', { name: '应用出错了' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新加载' })).toBeInTheDocument()
    expect(screen.getByText(/provider down/)).toBeInTheDocument()
  })

  it('最外层兜底的文案跟随已保存的语言', async () => {
    localStorage.setItem('larkax.lang', 'en')
    render(
      <FatalErrorBoundary>
        <Boom error={new Error('provider down')} />
      </FatalErrorBoundary>,
    )

    expect(await screen.findByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument()
  })
})
