import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { Tabs, type TabItem } from './Tabs'

const ITEMS: TabItem[] = [
  { id: 'one', label: '甲', panel: <p>甲的内容</p> },
  { id: 'two', label: '乙', panel: <p>乙的内容</p> },
  { id: 'three', label: '丙', panel: <p>丙的内容</p> },
]

function Harness() {
  const [value, setValue] = useState('one')
  return <Tabs label="示例分组" items={ITEMS} value={value} onChange={setValue} />
}

const tabOf = (name: string) => screen.getByRole('tab', { name })

/** 选中的那个 tab 同时是 `aria-selected` 与 roving 的落点。 */
function expectSelected(name: string) {
  const selected = tabOf(name)
  expect(selected).toHaveAttribute('aria-selected', 'true')
  expect(selected).toHaveAttribute('tabindex', '0')
  for (const other of ['甲', '乙', '丙'].filter((label) => label !== name)) {
    expect(tabOf(other)).toHaveAttribute('aria-selected', 'false')
    expect(tabOf(other)).toHaveAttribute('tabindex', '-1')
  }
}

describe('Tabs', () => {
  it('方向键在两端回绕，Home 与 End 到首尾，选中状态与 tabindex 一起走', async () => {
    const user = userEvent.setup()
    render(<Harness />)

    // 组本身有名字，读屏进来先听到它。
    expect(screen.getByRole('tablist', { name: '示例分组' })).toBeInTheDocument()
    expectSelected('甲')

    tabOf('甲').focus()
    await user.keyboard('{ArrowRight}')
    expectSelected('乙')
    expect(tabOf('乙')).toHaveFocus()

    await user.keyboard('{ArrowRight}')
    expectSelected('丙')

    // 最右再按一次回到最左。
    await user.keyboard('{ArrowRight}')
    expectSelected('甲')

    await user.keyboard('{ArrowLeft}')
    expectSelected('丙')

    await user.keyboard('{Home}')
    expectSelected('甲')
    expect(tabOf('甲')).toHaveFocus()

    await user.keyboard('{End}')
    expectSelected('丙')
    expect(tabOf('丙')).toHaveFocus()
  })

  it('点击与键盘切换后，aria-controls 指的就是那个可见面板', async () => {
    const user = userEvent.setup()
    render(<Harness />)

    const visiblePanel = () => {
      const selected = screen.getByRole('tab', { selected: true })
      const panel = document.getElementById(selected.getAttribute('aria-controls') ?? '')
      expect(panel).not.toBeNull()
      expect(panel).toBeVisible()
      expect(panel).toHaveAttribute('aria-labelledby', selected.id)
      return panel as HTMLElement
    }

    expect(visiblePanel()).toHaveTextContent('甲的内容')

    await user.click(tabOf('乙'))
    expect(visiblePanel()).toHaveTextContent('乙的内容')

    tabOf('乙').focus()
    await user.keyboard('{ArrowRight}')
    expect(visiblePanel()).toHaveTextContent('丙的内容')

    // 没选中的面板还在 DOM 里（aria-controls 不能指空），但藏着。
    for (const name of ['甲', '乙']) {
      const panel = document.getElementById(tabOf(name).getAttribute('aria-controls') ?? '')
      expect(panel).not.toBeVisible()
    }
  })

  it('切走的分组不渲染内容', async () => {
    const user = userEvent.setup()
    render(<Harness />)

    expect(screen.getByText('甲的内容')).toBeInTheDocument()
    expect(screen.queryByText('乙的内容')).not.toBeInTheDocument()

    await user.click(tabOf('乙'))
    expect(screen.queryByText('甲的内容')).not.toBeInTheDocument()
    expect(screen.getByText('乙的内容')).toBeInTheDocument()
  })
})
