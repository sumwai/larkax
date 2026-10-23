import type { ReactNode } from 'react'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { cn } from './lib'
import { Button } from './button'

/**
 * 表格：表头与数据行共用同一套 grid 列宽——列名由调用方给一份 `cols`
 * 模板，两处都从它出。窄屏下每格的 `data-label` 撑出卡片形态的行首标签。
 */
export function Table({
  cols,
  className,
  children,
  'aria-label': ariaLabel,
}: {
  /** grid-template-columns 的值，表头与每行共用。 */
  cols: string
  className?: string
  children: ReactNode
  'aria-label'?: string
}) {
  return (
    <div
      role="table"
      aria-label={ariaLabel}
      className={cn('w-full', className)}
      style={{ '--table-cols': cols } as React.CSSProperties}
    >
      {children}
    </div>
  )
}

const rowCols = { gridTemplateColumns: 'var(--table-cols)' }

export function TableHead({ className, children }: { className?: string; children: ReactNode }) {
  return (
    <div
      role="row"
      className={cn(
        'sticky top-(--table-sticky,0) z-sticky hidden border-b border-border bg-card text-2xs font-semibold text-muted-foreground md:grid',
        className,
      )}
      style={rowCols}
    >
      {children}
    </div>
  )
}

export function Th({
  className,
  right,
  sortDir,
  onSort,
  children,
}: {
  className?: string
  right?: boolean
  sortDir?: 'asc' | 'desc' | false
  onSort?: () => void
  children: ReactNode
}) {
  const content = (
    <>
      {children}
      {sortDir !== undefined ? (
        <span aria-hidden="true" className={cn('ml-1 text-2xs', !sortDir && 'opacity-40')}>
          {sortDir === 'desc' ? '▼' : '▲'}
        </span>
      ) : null}
    </>
  )
  return (
    <div
      role="columnheader"
      className={cn('flex items-center gap-1 px-3 py-2', right && 'justify-end', className)}
      {...(sortDir !== undefined
        ? ({
            'aria-sort': sortDir ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none',
          } as const)
        : {})}
    >
      {onSort ? (
        <button
          type="button"
          onClick={onSort}
          className="inline-flex items-center rounded-xs hover:text-foreground"
        >
          {content}
        </button>
      ) : (
        content
      )}
    </div>
  )
}

export function TableRow({
  className,
  children,
  selected,
}: {
  className?: string
  children: ReactNode
  selected?: boolean
}) {
  return (
    <div
      role="row"
      className={cn(
        'border-b border-border transition-colors last:border-b-0 hover:bg-muted/50 md:grid',
        selected && 'bg-primary-soft/50',
        className,
      )}
      style={rowCols}
    >
      {children}
    </div>
  )
}

export function Td({
  className,
  right,
  label,
  strong,
  mono,
  children,
}: {
  className?: string
  right?: boolean
  /** 窄屏卡片形态下行首的列名。 */
  label?: string
  strong?: boolean
  mono?: boolean
  children: ReactNode
}) {
  return (
    <div
      role="cell"
      data-label={label}
      className={cn(
        'flex items-center gap-2 px-3 py-2.5 text-sm md:min-w-0',
        right && 'md:justify-end',
        strong && 'font-medium',
        mono && 'font-mono num',
        className,
      )}
    >
      {children}
    </div>
  )
}

/**
 * 分页：总数、每页、页码三段。上百条的列表不靠浏览器内查找。
 */
export function Pagination({
  page,
  pageSize,
  total,
  onPageChange,
  className,
  summary,
  ariaLabel = '分页',
  prevLabel = '上一页',
  nextLabel = '下一页',
}: {
  page: number
  pageSize: number
  total: number
  onPageChange: (page: number) => void
  className?: string
  /** 左侧的说明文字；不给用内置的「第 X–Y 条，共 Z 条」。 */
  summary?: ReactNode
  ariaLabel?: string
  prevLabel?: string
  nextLabel?: string
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1
  const to = Math.min(total, page * pageSize)
  return (
    <nav
      aria-label={ariaLabel}
      className={cn(
        'flex items-center justify-between gap-3 pt-3 text-sm text-muted-foreground',
        className,
      )}
    >
      {summary ?? (
        <span className="num text-xs">
          第 {from}–{to} 条，共 {total} 条
        </span>
      )}
      <div className="flex items-center gap-1">
        <Button
          variant="ghost"
          size="icon"
          className="size-8"
          aria-label={prevLabel}
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          <ChevronLeft className="size-4" />
        </Button>
        <span className="num min-w-14 text-center text-xs">
          {page} / {pages}
        </span>
        <Button
          variant="ghost"
          size="icon"
          className="size-8"
          aria-label={nextLabel}
          disabled={page >= pages}
          onClick={() => onPageChange(page + 1)}
        >
          <ChevronRight className="size-4" />
        </Button>
      </div>
    </nav>
  )
}
