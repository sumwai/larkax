import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { ActionSheet } from './ActionSheet'
import { Modal, SHEET_SIZES, type SheetSize } from './ui'
import { mockViewport } from '../test/media'

function renderSheet(size: SheetSize, onClose = () => undefined) {
  return render(
    <I18nProvider>
      <Modal sheet sheetSize={size} title="编辑" onClose={onClose}>
        内容
      </Modal>
    </I18nProvider>,
  )
}

describe('bottom sheet 高度档位', () => {
  it('三档都从同一个常量取上限，不是三处手写的百分比', () => {
    const sizes: SheetSize[] = ['sm', 'md', 'lg']
    for (const size of sizes) {
      const { unmount } = renderSheet(size)
      const dialog = screen.getByRole('dialog')
      expect(dialog.style.maxHeight).toBe(`${SHEET_SIZES[size]}dvh`)
      // 档位是上限：写出固定高度的话，内容少的弹窗会留一大片空白。
      expect(dialog.style.height).toBe('')
      unmount()
    }
    expect(SHEET_SIZES.sm).toBeLessThan(SHEET_SIZES.md)
    expect(SHEET_SIZES.md).toBeLessThan(SHEET_SIZES.lg)
  })

  it('往下拖过上沿的临界距离就关闭，拖得不够则弹回', () => {
    const onClose = vi.fn()
    renderSheet('md', onClose)
    const grab = document.querySelector('.sheet-grab') as HTMLElement
    expect(grab).not.toBeNull()

    // jsdom 没有 PointerEvent，坐标要自己挂到事件上；否则 `clientY` 是
    // undefined，拖动距离算不出来，用例会「怎么都关不掉」。
    const pointer = (type: string, clientY: number) => {
      const event = new Event(type, { bubbles: true })
      Object.defineProperty(event, 'clientY', { value: clientY })
      Object.defineProperty(event, 'pointerId', { value: 1 })
      return event
    }

    fireEvent(grab, pointer('pointerdown', 100))
    fireEvent(grab, pointer('pointermove', 140))
    fireEvent(grab, pointer('pointerup', 140))
    expect(onClose).not.toHaveBeenCalled()

    fireEvent(grab, pointer('pointerdown', 100))
    fireEvent(grab, pointer('pointermove', 260))
    fireEvent(grab, pointer('pointerup', 260))
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})

describe('编辑类弹窗在窄屏上也是 bottom sheet', () => {
  it('没有显式声明 sheet 的弹窗，窄屏下同样有档位与拖动把手', () => {
    const restore = mockViewport(375, 667)
    try {
      const { unmount } = render(
        <I18nProvider>
          <Modal title="编辑渠道" sheetSize="lg" onClose={() => undefined}>
            内容
          </Modal>
        </I18nProvider>,
      )
      const dialog = screen.getByRole('dialog')
      expect(dialog.style.maxHeight).toBe(`${SHEET_SIZES.lg}dvh`)
      expect(document.querySelector('.sheet-grab')).not.toBeNull()
      expect((dialog.parentElement as HTMLElement).className).toContain('sheet-overlay')
      unmount()
    } finally {
      restore()
    }
  })

  it('桌面上同一弹窗保持居中、没有拖动把手', () => {
    const { unmount } = render(
      <I18nProvider>
        <Modal title="编辑渠道" sheetSize="lg" onClose={() => undefined}>
          内容
        </Modal>
      </I18nProvider>,
    )
    const dialog = screen.getByRole('dialog')
    expect(dialog.style.maxHeight).toBe('')
    expect(document.querySelector('.sheet-grab')).toBeNull()
    expect((dialog.parentElement as HTMLElement).className).not.toContain('sheet-overlay')
    unmount()
  })
})

describe('操作菜单', () => {
  it('危险项同时有红色样式与读屏文案', () => {
    const onSelect = vi.fn()
    const onClose = vi.fn()
    render(
      <I18nProvider>
        <ActionSheet
          title="acme"
          items={[
            { key: 'view', label: '查看', onSelect: vi.fn() },
            { key: 'disable', label: '停用', danger: true, onSelect },
          ]}
          onClose={onClose}
        />
      </I18nProvider>,
    )

    const disable = screen.getByRole('button', { name: /停用/ })
    expect(disable.className).toContain('danger')
    // 颜色之外的第二个通道：可及名里明说这是危险操作。
    expect(disable).toHaveAccessibleName(/危险操作/)
    expect(screen.getByRole('button', { name: '查看' }).className).not.toContain('danger')

    fireEvent.click(disable)
    expect(onClose).toHaveBeenCalledTimes(1)
    expect(onSelect).toHaveBeenCalledTimes(1)
  })
})
