import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { I18nProvider } from '../i18n'
import { ExprEditor } from './ExprEditor'

/**
 * 把编辑器接在一个最小的宿主上：表达式与错误都打印出来，用例只读这两处。
 */
function renderEditor(initial = '') {
  const changes: string[] = []
  const errors: Array<string | null> = []
  function Host() {
    return (
      <ExprEditor
        value={initial}
        onChange={(expr) => changes.push(expr)}
        onErrorChange={(error) => errors.push(error)}
      />
    )
  }
  render(
    <I18nProvider>
      <Host />
    </I18nProvider>,
  )
  return {
    lastExpr: () => changes[changes.length - 1] ?? initial,
    lastError: () => errors[errors.length - 1] ?? null,
  }
}

function row(name: string): HTMLInputElement {
  return screen.getByLabelText(name) as HTMLInputElement
}

function unitOf(name: string): HTMLSelectElement {
  return screen.getByLabelText(`${name} 单位`) as HTMLSelectElement
}

function switchTo(name: '表单' | '表达式') {
  fireEvent.click(screen.getByRole('button', { name }))
}

describe('计价表达式输入', () => {
  it('每 100 万 token 的 3.5 写成每 token 0.0000035，不经过浮点', () => {
    const editor = renderEditor()

    fireEvent.change(row('输入'), { target: { value: '3.5' } })
    expect(editor.lastExpr()).toBe('{"prompt": 0.0000035}')

    // 15 在浮点乘法里会写成 0.000014999999999999999；定点搬小数点不会。
    fireEvent.change(row('输出'), { target: { value: '15' } })
    expect(editor.lastExpr()).toBe('{"prompt": 0.0000035, "completion": 0.000015}')
  })

  it('反向解析已有表达式：0.0000015 显示成每百万 1.5', () => {
    renderEditor('{"prompt": 0.0000015}')

    expect(row('输入').value).toBe('1.5')
    expect(unitOf('输入').value).toBe('million')
    expect(screen.getByRole('button', { name: '表单' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('带条件的表达式只能留在表达式模式，并说明原因', () => {
    renderEditor('len <= 32000 ? {"prompt": 0.0000015} : {"prompt": 0.000003}')

    expect(screen.getByRole('button', { name: '表达式' })).toHaveAttribute('aria-pressed', 'true')
    switchTo('表单')

    expect(screen.getByRole('alert')).toHaveTextContent('这条表达式含条件，只能用表达式模式编辑。')
    expect(screen.getByRole('button', { name: '表达式' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByLabelText('表达式')).toBeInTheDocument()
  })

  it('一项都没填时回报「至少要有一项」', () => {
    const editor = renderEditor()

    expect(editor.lastError()).toBe('至少要有一项单价，否则这次请求结算时会失败。')

    fireEvent.change(row('输入'), { target: { value: '3' } })
    expect(editor.lastError()).toBeNull()
  })

  it('非法数字、负数与超精度各自报错，并指出是哪一行', () => {
    const editor = renderEditor()

    fireEvent.change(row('输入'), { target: { value: '1.2.3' } })
    expect(editor.lastError()).toBe('第 1 行：只能填非负的十进制小数。')

    fireEvent.change(row('输入'), { target: { value: '-1' } })
    expect(editor.lastError()).toBe('第 1 行：只能填非负的十进制小数。')

    fireEvent.change(row('输入'), { target: { value: '3' } })
    fireEvent.change(row('缓存写'), { target: { value: '0.000000001' } })
    fireEvent.change(unitOf('缓存写'), { target: { value: 'token' } })
    expect(editor.lastError()).toBe('第 4 行：换算成每 token 后超过 8 位小数。')
  })

  it('表单与表达式来回切一次，数值不变', () => {
    const editor = renderEditor()

    fireEvent.change(row('输入'), { target: { value: '3.5' } })
    fireEvent.change(row('推理'), { target: { value: '12' } })
    const expr = editor.lastExpr()

    switchTo('表达式')
    expect(screen.getByLabelText('表达式')).toHaveValue(expr)

    switchTo('表单')
    expect(row('输入').value).toBe('3.5')
    expect(row('推理').value).toBe('12')
    expect(editor.lastExpr()).toBe(expr)
  })

  it('单位改成每 1000 token 时，同样按十进制搬小数点', () => {
    const editor = renderEditor()

    fireEvent.change(row('输入'), { target: { value: '0.0035' } })
    fireEvent.change(unitOf('输入'), { target: { value: 'thousand' } })
    expect(editor.lastExpr()).toBe('{"prompt": 0.0000035}')
  })

  it('其它键的表达式不猜，停在表达式模式', () => {
    renderEditor('{"prompt": 0.0000015, "units": {"search": 0.01}}')
    switchTo('表单')

    expect(screen.getByRole('alert')).toHaveTextContent(
      '这条表达式不是五项固定单价的写法，只能用表达式模式编辑。',
    )
    expect(screen.getByRole('button', { name: '表达式' })).toHaveAttribute('aria-pressed', 'true')
  })
})
