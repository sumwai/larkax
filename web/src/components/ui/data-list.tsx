import type { ReactNode } from 'react'
import { cn } from './lib'

/**
 * 列表的双形态：桌面表格与移动卡片，二选一渲染不是两份状态——
 * 同一份数据、同一个容器，断点由容器宽度而不是视口决定。
 * Tailwind v4 的容器查询在核心里，`@container` + `@md:` 直接可用。
 */
export function DataList({
  desktop,
  mobile,
  className,
}: {
  desktop: ReactNode
  mobile: ReactNode
  className?: string
}) {
  return (
    <div className={cn('@container', className)}>
      <div className="hidden @2xl:block">{desktop}</div>
      <div className="@2xl:hidden">{mobile}</div>
    </div>
  )
}
