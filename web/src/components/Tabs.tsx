import { useId, useRef, type KeyboardEvent, type ReactNode } from 'react'

export interface TabItem {
  id: string
  label: ReactNode
  panel: ReactNode
  /** 标签后面的附加标记，比如「这一组有校验错误」的圆点。 */
  adornment?: ReactNode
  /** 这一组有错：`aria-invalid` 让样式与读屏都能看出是哪个 tab。 */
  invalid?: boolean
  /** 面板自己的类名。 */
  panelClassName?: string
}

/**
 * 一组 tab：`tablist` / `tab` / `tabpanel` 三者互相指对，键盘按 APG 的约定走。
 *
 * 键盘与焦点：
 * - roving tabindex——只有选中的 tab 在 Tab 序列里（`0`），其余是 `-1`；
 *   进入这一组按一次 Tab 就够，组内换用方向键。
 * - 左右方向键在两端回绕，Home / End 到首尾（自动激活：焦点跟着切换走）。
 *
 * 面板只有选中的那个渲染内容，其余是空的：切走的输入框若留在 DOM 里，
 * 隐藏时仍可能被读屏念到；而 `aria-controls` 指向的节点必须存在，所以
 * 面板容器本身不卸载，用 `hidden` 藏起来（见 components.css 里 `.tab-panel[hidden]`
 * 对 `display` 的兜底）。
 */
export function Tabs({
  items,
  value,
  onChange,
  label,
  idPrefix,
  className = 'tabs',
}: {
  items: TabItem[]
  value: string
  onChange: (id: string) => void
  /** 这组 tab 的可及名字。 */
  label: string
  /** id 的前缀，默认由 `useId` 生成。需要按 id 找 tab 的调用方可以指名。 */
  idPrefix?: string
  className?: string
}) {
  const generated = useId()
  const prefix = idPrefix ?? generated
  const buttons = useRef<Array<HTMLButtonElement | null>>([])

  const select = (index: number) => {
    const item = items[index]
    if (!item) return
    onChange(item.id)
    // 自动激活：切到哪个就把焦点送过去，读屏紧接着念出新的那一个。
    buttons.current[index]?.focus()
  }

  const onKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    const last = items.length - 1
    let next: number | null = null
    if (event.key === 'ArrowRight') next = index === last ? 0 : index + 1
    else if (event.key === 'ArrowLeft') next = index === 0 ? last : index - 1
    else if (event.key === 'Home') next = 0
    else if (event.key === 'End') next = last
    if (next === null) return
    // 方向键在 tablist 里不该滚动页面，也不该由浏览器移动光标。
    event.preventDefault()
    select(next)
  }

  return (
    <>
      <div className={className} role="tablist" aria-label={label}>
        {items.map((item, index) => {
          const selected = item.id === value
          return (
            <button
              key={item.id}
              ref={(element) => {
                buttons.current[index] = element
              }}
              type="button"
              role="tab"
              id={`${prefix}-tab-${item.id}`}
              aria-controls={`${prefix}-panel-${item.id}`}
              aria-selected={selected}
              aria-invalid={item.invalid || undefined}
              tabIndex={selected ? 0 : -1}
              onClick={() => select(index)}
              onKeyDown={(event) => onKeyDown(event, index)}
            >
              {item.label}
              {item.adornment}
            </button>
          )
        })}
      </div>
      {items.map((item) => {
        const selected = item.id === value
        return (
          <div
            key={item.id}
            role="tabpanel"
            id={`${prefix}-panel-${item.id}`}
            aria-labelledby={`${prefix}-tab-${item.id}`}
            hidden={!selected}
            className={
              item.panelClassName ? `tab-panel ${item.panelClassName}` : 'tab-panel'
            }
          >
            {selected ? item.panel : null}
          </div>
        )
      })}
    </>
  )
}
