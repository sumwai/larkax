import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { ChevronRight } from 'lucide-react'
import { cn } from './lib'

/**
 * 页头：标题、说明、右侧动作区。面包屑与返回不混用——
 * 详情页用面包屑，返回目标写在地址里，刷新不丢。
 */
export function PageHead({
  title,
  description,
  actions,
  className,
}: {
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <header className={cn('flex flex-wrap items-start justify-between gap-3', className)}>
      <div className="min-w-0">
        <h1 className="text-xl font-semibold tracking-tight text-balance">{title}</h1>
        {description ? (
          <p className="mt-1 max-w-2xl text-sm text-muted-foreground">{description}</p>
        ) : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </header>
  )
}

/** 面包屑：末段是当前页，不可点。 */
export function Breadcrumbs({
  items,
  className,
}: {
  items: { label: ReactNode; to?: string }[]
  className?: string
}) {
  return (
    <nav aria-label="面包屑" className={cn('flex min-w-0 items-center gap-1 text-sm text-muted-foreground', className)}>
      {items.map((item, index) => {
        const last = index === items.length - 1
        return (
          <span key={index} className="flex min-w-0 items-center gap-1">
            {index > 0 ? <ChevronRight className="size-3.5 shrink-0 text-faint" aria-hidden="true" /> : null}
            {last || !item.to ? (
              <span className="truncate text-foreground" aria-current={last ? 'page' : undefined}>
                {item.label}
              </span>
            ) : (
              <Link to={item.to} className="truncate hover:text-foreground hover:underline">
                {item.label}
              </Link>
            )}
          </span>
        )
      })}
    </nav>
  )
}
