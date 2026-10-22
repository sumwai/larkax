import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { PageHead } from './ui'

const HIDDEN = 'page-head-title-narrow-hidden'

describe('PageHead 窄屏标题', () => {
  it('默认隐藏：标题挂上窄屏视觉隐藏的类，副标题照常渲染', () => {
    render(<PageHead title="概览" subtitle="账户余额与最近的调用情况。" />)
    expect(screen.getByRole('heading', { level: 1, name: '概览' })).toHaveClass(HIDDEN)
    expect(screen.getByText('账户余额与最近的调用情况。')).toBeInTheDocument()
  })

  it('keepTitleOnNarrow 的页面不挂隐藏类：标题与顶栏不同文时要留着', () => {
    render(<PageHead title="Claude Sonnet 4.5" keepTitleOnNarrow />)
    expect(screen.getByRole('heading', { level: 1, name: 'Claude Sonnet 4.5' })).not.toHaveClass(HIDDEN)
  })

  it('page-actions 不受隐藏影响', () => {
    render(<PageHead title="用量" actions={<button type="button">导出</button>} />)
    expect(screen.getByRole('button', { name: '导出' })).toBeInTheDocument()
  })
})
