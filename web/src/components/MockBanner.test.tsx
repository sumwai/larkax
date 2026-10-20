import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { MockBanner } from './MockBanner'

describe('MockBanner', () => {
  afterEach(() => {
    vi.unstubAllEnvs()
    vi.resetModules()
  })

  it('mock 模式下显示演示数据提示，关闭后消失', async () => {
    const user = userEvent.setup()
    render(
      <I18nProvider>
        <MockBanner />
      </I18nProvider>,
    )

    expect(screen.getByRole('status')).toHaveTextContent('演示数据')
    await user.click(screen.getByRole('button', { name: '关闭' }))
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('http 模式下不渲染', async () => {
    vi.stubEnv('VITE_API_MODE', 'http')
    // 横幅读的是模块级常量，环境改完要重新加载模块才生效。
    vi.resetModules()
    const [{ MockBanner: Banner }, { I18nProvider: Provider }] = await Promise.all([
      import('./MockBanner'),
      import('../i18n'),
    ])

    render(
      <Provider>
        <Banner />
      </Provider>,
    )
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })
})
