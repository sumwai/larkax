import { useRef, type FormEvent, type ReactNode } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { Modal } from './ui'

function renderModal(ui: ReactNode) {
  return render(<I18nProvider>{ui}</I18nProvider>)
}

describe('弹窗打开后的焦点落点', () => {
  it('落在第一个输入框，不是面板', () => {
    renderModal(
      <Modal title="编辑" onClose={() => undefined}>
        <input aria-label="名称" defaultValue="" />
        <input aria-label="备注" defaultValue="" />
      </Modal>,
    )

    // 焦点停在面板上正是这条要修的行为：键盘用户得先 Tab 一趟才够得着输入框。
    expect(document.activeElement?.tagName).not.toBe('DIV')
    expect(screen.getByLabelText('名称')).toHaveFocus()
  })

  it('不碰鼠标就能接着打字', async () => {
    const user = userEvent.setup()
    renderModal(
      <Modal title="编辑" onClose={() => undefined}>
        <input aria-label="名称" defaultValue="" />
        <input aria-label="备注" defaultValue="" />
      </Modal>,
    )

    // 这条不先断焦点：字有没有真的进这一格，是「不用先摸鼠标」的唯一证据。
    // 焦点若落在面板上，这些字会打到 `<body>` 上，输入框仍是空的。
    await user.keyboard('abc')
    expect(screen.getByLabelText('名称')).toHaveValue('abc')
  })

  it('在第一个输入框按回车就走表单提交', async () => {
    const onSubmit = vi.fn((event: FormEvent) => event.preventDefault())
    const user = userEvent.setup()
    renderModal(
      <Modal title="编辑" onClose={() => undefined}>
        <form onSubmit={onSubmit}>
          <input aria-label="名称" defaultValue="" />
          <button type="submit">保存</button>
        </form>
      </Modal>,
    )

    await user.keyboard('abc{Enter}')
    expect(onSubmit).toHaveBeenCalledTimes(1)
  })

  it('跳过 disabled、readonly 与看不见的控件，落到第一个能改的', () => {
    renderModal(
      <Modal title="编辑" onClose={() => undefined}>
        <input aria-label="禁用" disabled />
        <input aria-label="只读" readOnly />
        <div hidden>
          <input aria-label="hidden 属性" />
        </div>
        <div style={{ display: 'none' }}>
          <input aria-label="display:none" />
        </div>
        <input type="hidden" aria-label="hidden 类型" />
        <select aria-label="渠道" defaultValue="a">
          <option value="a">甲</option>
        </select>
        <textarea aria-label="备注" />
      </Modal>,
    )

    expect(screen.getByLabelText('渠道')).toHaveFocus()
  })

  it('没有可编辑控件的确认框落在面板上，不抢关闭钮或「取消」，Esc 仍能关', async () => {
    const onClose = vi.fn()
    const user = userEvent.setup()
    renderModal(
      <Modal
        title="停用"
        onClose={onClose}
        footer={
          <>
            <button type="button">取消</button>
            <button type="button">停用</button>
          </>
        }
      >
        <p>停用后这个渠道的请求会立刻失败。</p>
      </Modal>,
    )

    // 面板上有头部关闭钮与 footer 两颗按钮。焦点若给了按钮，第一颗会是
    // 头部关闭钮（DOM 顺序在「取消」之前）；它和「取消」一样比面板更糟。
    expect(document.activeElement?.tagName).toBe('DIV')
    expect(screen.getByRole('dialog')).toHaveFocus()

    await user.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('显式 focusTarget 优先于默认落点', () => {
    function Host() {
      const second = useRef<HTMLInputElement>(null)
      return (
        <Modal title="编辑" onClose={() => undefined} focusTarget={second}>
          <input aria-label="名称" />
          <input aria-label="备注" ref={second} />
        </Modal>
      )
    }

    renderModal(<Host />)
    expect(screen.getByLabelText('备注')).toHaveFocus()
  })

  it('focusTarget 探测不到时退回默认落点，不是面板', () => {
    renderModal(
      <Modal title="编辑" onClose={() => undefined} focusTarget={() => null}>
        <input aria-label="名称" />
      </Modal>,
    )
    expect(screen.getByLabelText('名称')).toHaveFocus()
  })
})
