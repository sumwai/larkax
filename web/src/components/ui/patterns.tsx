import type { ReactNode } from 'react'
import { CircleAlert, Inbox, ShieldX } from 'lucide-react'
import { cn } from './lib'
import { Button } from './button'

/**
 * 页级状态：403 / 404 / 空 三张整页。只有文字提示不够——
 * 每一种都要给下一步动作，且动作不一样。
 */
export function PageState({
  kind,
  title,
  description,
  action,
  className,
}: {
  kind: 'forbidden' | 'not-found' | 'empty'
  title: ReactNode
  description?: ReactNode
  action?: ReactNode
  className?: string
}) {
  const Icon = kind === 'forbidden' ? ShieldX : kind === 'not-found' ? CircleAlert : Inbox
  return (
    <div
      className={cn(
        'flex min-h-[50dvh] flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border-strong bg-card/40 px-6 py-16 text-center',
        className,
      )}
    >
      <Icon className="size-8 text-faint" aria-hidden="true" />
      <p className="text-md font-semibold">{title}</p>
      {description ? <p className="max-w-sm text-sm text-muted-foreground">{description}</p> : null}
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  )
}

/** 403：没有权限看这一页。动作通常是「回概览」。 */
export function ForbiddenState({
  title,
  description,
  actionLabel,
  onAction,
}: {
  title: ReactNode
  description?: ReactNode
  actionLabel: ReactNode
  onAction: () => void
}) {
  return (
    <PageState
      kind="forbidden"
      title={title}
      description={description}
      action={
        <Button variant="secondary" onClick={onAction}>
          {actionLabel}
        </Button>
      }
    />
  )
}

/**
 * 排行榜：名次 + 占比条 + 变化。变化是区间对比的差值方向，
 * 只表方向不表数值时传 sign。
 */
export function Leaderboard({
  items,
  formatValue,
  className,
}: {
  items: {
    id: string
    rank: number
    label: ReactNode
    sub?: ReactNode
    value: number
    /** 0–100，占榜首的比例。 */
    share: number
    change?: { direction: 'up' | 'down' | 'flat'; label?: ReactNode }
  }[]
  formatValue: (value: number) => string
  className?: string
}) {
  return (
    <ol className={cn('flex flex-col divide-y divide-border', className)}>
      {items.map((item) => (
        <li key={item.id} className="flex items-center gap-3 py-2.5">
          <span className="num w-6 shrink-0 text-center text-sm font-semibold text-faint">
            {item.rank}
          </span>
          <div className="min-w-0 flex-1">
            <div className="flex items-baseline justify-between gap-2">
              <span className="min-w-0 truncate text-sm font-medium">{item.label}</span>
              <span className="num shrink-0 font-mono text-sm">{formatValue(item.value)}</span>
            </div>
            <div className="mt-1 flex items-center gap-2">
              <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
                <div
                  className="h-full rounded-full bg-chart-4"
                  style={{ width: `${Math.max(2, Math.min(100, item.share))}%` }}
                />
              </div>
              {item.sub ? <span className="shrink-0 text-2xs text-faint">{item.sub}</span> : null}
              {item.change ? (
                <span
                  className={cn(
                    'num shrink-0 text-2xs',
                    item.change.direction === 'up' && 'text-success-strong',
                    item.change.direction === 'down' && 'text-danger-strong',
                    item.change.direction === 'flat' && 'text-faint',
                  )}
                >
                  {item.change.direction === 'up' ? '↑' : item.change.direction === 'down' ? '↓' : '→'}
                  {item.change.label}
                </span>
              ) : null}
            </div>
          </div>
        </li>
      ))}
    </ol>
  )
}

/**
 * 批量操作条：吸底，浮在内容之上、底栏之下。部分选中时全选框是半选态
 * （那是表格的事），这里只报数量与动作。
 */
export function BatchBar({
  count,
  children,
  onClear,
  label = '已选',
}: {
  count: number
  children: ReactNode
  onClear: () => void
  label?: string
}) {
  if (count === 0) return null
  return (
    <div
      role="toolbar"
      aria-label="批量操作"
      className={cn(
        'fixed inset-x-0 bottom-(--bottomnav-h,64px) z-sticky mx-auto flex w-fit max-w-[calc(100vw-24px)] items-center gap-2 rounded-md border border-border bg-popover px-3 py-2 shadow-lg md:bottom-4',
      )}
    >
      <span className="num text-sm text-muted-foreground">
        {label} {count} 项
      </span>
      <span className="h-5 w-px bg-border" aria-hidden="true" />
      {children}
      <Button variant="ghost" size="sm" onClick={onClear}>
        清除选择
      </Button>
    </div>
  )
}
