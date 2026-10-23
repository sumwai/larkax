import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Pagination } from './table'

/**
 * 分页底栏三段的边界：当前区间与总数、`summary` 换掉默认说明、
 * 以及上一页 / 下一页的可点状态。文案是用户唯一能读到的东西，逐字断言。
 */
describe('Pagination', () => {
  it('默认说明给出当前区间与总数', () => {
    render(<Pagination page={2} pageSize={20} total={45} onPageChange={() => {}} />)

    const nav = screen.getByRole('navigation', { name: '分页' })

    expect(within(nav).getByText('第 21–40 条，共 45 条')).toBeInTheDocument()
    expect(within(nav).getByText('2 / 3')).toBeInTheDocument()
  })

  it('summary 与 aria 文案原样出现在 DOM 里，翻页回调带的是目标页码', async () => {
    const onPageChange = vi.fn()
    const user = userEvent.setup()
    render(
      <Pagination
        page={1}
        pageSize={20}
        total={45}
        onPageChange={onPageChange}
        ariaLabel="翻页"
        prevLabel="上一页"
        nextLabel="下一页"
        summary={<span>本页 20 条</span>}
      />,
    )

    const nav = screen.getByRole('navigation', { name: '翻页' })

    expect(within(nav).getByText('本页 20 条')).toBeInTheDocument()
    const prev = within(nav).getByRole('button', { name: '上一页' })
    const next = within(nav).getByRole('button', { name: '下一页' })
    expect(prev).toBeDisabled()
    expect(next).toBeEnabled()

    await user.click(next)
    expect(onPageChange).toHaveBeenCalledWith(2)
  })

  it('没有下一页时按钮不可点，总数为零时区间是 0–0', () => {
    render(<Pagination page={1} pageSize={20} total={0} onPageChange={() => {}} />)

    const nav = screen.getByRole('navigation', { name: '分页' })

    expect(within(nav).getByText('第 0–0 条，共 0 条')).toBeInTheDocument()
    expect(within(nav).getByText('1 / 1')).toBeInTheDocument()
    expect(within(nav).getByRole('button', { name: '上一页' })).toBeDisabled()
    expect(within(nav).getByRole('button', { name: '下一页' })).toBeDisabled()
  })
})
