import type { ReactNode } from 'react'
import * as TabsPrimitive from '@radix-ui/react-tabs'
import { cn } from './lib'

/**
 * Tab 换内容，分段换参数——两件事，两种控件。
 * 带错标记的 tab 用 `error` 属性：目录表单用它指「哪一组没填对」。
 */
export const Tabs = TabsPrimitive.Root

export function TabsList({
  className,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.List>) {
  return (
    <TabsPrimitive.List
      className={cn('flex items-center gap-1 border-b border-border', className)}
      {...props}
    />
  )
}

export function TabsTrigger({
  className,
  error,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Trigger> & { error?: boolean }) {
  return (
    <TabsPrimitive.Trigger
      className={cn(
        'relative -mb-px inline-flex items-center gap-1.5 border-b-2 border-transparent px-3 py-2 text-sm font-medium text-muted-foreground transition-colors outline-none hover:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-4 focus-visible:outline-ring data-[state=active]:border-primary data-[state=active]:text-foreground',
        error && 'text-danger-strong',
        className,
      )}
      {...props}
    />
  )
}

export function TabsContent({
  className,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Content>) {
  return (
    <TabsPrimitive.Content
      className={cn('pt-4 outline-none focus-visible:outline-2 focus-visible:outline-ring', className)}
      {...props}
    />
  )
}

/**
 * 分段控件：换参数（视图、区间、单位）。少于四项用它，多于四项用 Select。
 */
export function Segmented<T extends string>({
  value,
  onValueChange,
  options,
  ariaLabel,
  size = 'md',
  className,
}: {
  value: T
  onValueChange: (value: T) => void
  options: { value: T; label: ReactNode }[]
  ariaLabel?: string
  size?: 'sm' | 'md'
  className?: string
}) {
  return (
    <div
      role="tablist"
      aria-label={ariaLabel}
      className={cn(
        'inline-flex items-center gap-0.5 rounded-sm bg-muted p-0.5',
        className,
      )}
    >
      {options.map((option) => {
        const active = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onValueChange(option.value)}
            className={cn(
              'inline-flex items-center justify-center rounded-[6px] font-medium transition-colors outline-none focus-visible:outline-2 focus-visible:outline-ring',
              size === 'sm' ? 'h-6 px-2 text-xs' : 'h-7 px-3 text-sm',
              active
                ? 'bg-control-active-bg text-control-active-fg'
                : 'text-muted-foreground hover:text-foreground',
            )}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}
