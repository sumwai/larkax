import type { ReactNode } from 'react'
import { IconChevronRight } from './icons'

/**
 * 窄屏清单：一叠卡片，替代桌面表格。
 *
 * 与表格是**二选一渲染**，不是两套都挂在 DOM 里靠 CSS 隐藏——表格带
 * `role="table"` 时，两套同时在页面上会被读屏当成同一份数据读两遍。
 */
export function MobileList({ label, children }: { label: string; children: ReactNode }) {
  return (
    <section className="mcards" aria-label={label}>
      {children}
    </section>
  )
}

export type MobileField = {
  /** 字段名；不传就只显示值，适合已经能看懂的短值。 */
  label?: ReactNode
  value: ReactNode
  /** 等宽显示：地址、模型名、邮箱这类逐字阅读的值。 */
  mono?: boolean
}

/**
 * 一张卡片：主标题、状态、两三个关键字段，末尾是操作入口。
 *
 * 操作是一整张卡（`onPress`）时渲染成按钮，读屏读到的是「对这条记录做什么」；
 * 操作是几个链接时用 `actions`，卡片本身不是控件。
 */
export function MobileCard({
  title,
  subtitle,
  status,
  fields,
  actions,
  onPress,
  pressLabel,
}: {
  title: ReactNode
  subtitle?: ReactNode
  status?: ReactNode
  fields?: MobileField[]
  actions?: ReactNode
  onPress?: () => void
  /** 整卡是按钮时的可及名。只写标题不够：同名的记录会分不清点的是哪一条。 */
  pressLabel?: string
}) {
  const body = (
    <>
      <span className="nm">{title}</span>
      {status && <span className="st">{status}</span>}
      {subtitle && <span className="sub">{subtitle}</span>}
      {fields?.map((field, index) => (
        <span className={field.mono ? 'kv mono' : 'kv'} key={index}>
          {field.label && <span className="k">{field.label}</span>}
          <span className="v">{field.value}</span>
        </span>
      ))}
      {actions && <div className="m-actions">{actions}</div>}
      {onPress && (
        <span className="chev" aria-hidden="true">
          <IconChevronRight size={18} />
        </span>
      )}
    </>
  )

  if (onPress) {
    return (
      <button type="button" className="mcard" onClick={onPress} aria-label={pressLabel}>
        {body}
      </button>
    )
  }
  return <div className="mcard">{body}</div>
}
