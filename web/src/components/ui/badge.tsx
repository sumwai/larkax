import type { HTMLAttributes } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from './lib'

/**
 * Badge：状态与元信息。色调对齐语义 token；`dot` 画出状态圆点，
 * 供「在供 / 余额不足」这类读扫的状态用。成功/警告/危险的文字用
 * strong 档——soft 底上的正色过不了 AA。
 */
const badgeVariants = cva(
  'inline-flex items-center gap-1.5 rounded-full border px-2 py-px text-xs font-medium whitespace-nowrap',
  {
    variants: {
      tone: {
        neutral: 'border-border bg-muted text-muted-foreground',
        success: 'border-success-border bg-success-soft text-success-strong',
        warning: 'border-warning-border bg-warning-soft text-warning-strong',
        danger: 'border-danger-border bg-danger-soft text-danger-strong',
        info: 'bg-info-soft text-info-strong border-transparent',
      },
    },
    defaultVariants: { tone: 'neutral' },
  },
)

export interface BadgeProps
  extends HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badgeVariants> {
  dot?: boolean
}

export function Badge({ className, tone, dot, children, ...props }: BadgeProps) {
  return (
    <span className={cn(badgeVariants({ tone }), className)} {...props}>
      {dot ? (
        <span
          aria-hidden="true"
          className={cn(
            'size-1.5 rounded-full',
            tone === 'success' && 'bg-success',
            tone === 'warning' && 'bg-warning',
            tone === 'danger' && 'bg-danger',
            tone === 'info' && 'bg-info',
            (!tone || tone === 'neutral') && 'bg-faint',
          )}
        />
      ) : null}
      {children}
    </span>
  )
}

export { badgeVariants }
