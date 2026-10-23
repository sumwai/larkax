import type { HTMLAttributes, ReactNode } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { CircleCheck, CircleX, Info, TriangleAlert } from 'lucide-react'
import { cn } from './lib'

/**
 * 提示条：四种色调。它是非阻断的——一屏最多一条，需要选择时用对话框。
 */
const alertVariants = cva('flex gap-3 rounded-md border p-3.5 text-sm', {
  variants: {
    tone: {
      info: 'border-transparent bg-info-soft text-info-strong',
      success: 'border-success-border bg-success-soft text-success-strong',
      warning: 'border-warning-border bg-warning-soft text-warning-strong',
      danger: 'border-danger-border bg-danger-soft text-danger-strong',
    },
  },
  defaultVariants: { tone: 'info' },
})

const alertIcons = {
  info: Info,
  success: CircleCheck,
  warning: TriangleAlert,
  danger: CircleX,
} as const

export interface AlertProps
  extends Omit<HTMLAttributes<HTMLDivElement>, 'title'>,
    VariantProps<typeof alertVariants> {
  title?: ReactNode
}

export function Alert({ className, tone, title, children, ...props }: AlertProps) {
  const Icon = alertIcons[tone ?? 'info']
  return (
    <div role="alert" className={cn(alertVariants({ tone }), className)} {...props}>
      <Icon className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <div className="min-w-0 flex-1">
        {title ? <p className="font-semibold">{title}</p> : null}
        {children ? <div className={cn(title && 'mt-1 opacity-90')}>{children}</div> : null}
      </div>
    </div>
  )
}

/**
 * 浏览器横幅：贴在顶栏下面，不遮内容。演示数据、会话失效两种。
 */
export function Banner({
  tone = 'info',
  className,
  children,
  ...props
}: Omit<AlertProps, 'title'> & { tone?: NonNullable<AlertProps['tone']> }) {
  const Icon = alertIcons[tone]
  return (
    <div
      role="status"
      className={cn(
        'flex items-center gap-2.5 border-b px-4 py-2 text-sm',
        tone === 'warning' && 'border-warning-border bg-warning-soft text-warning-strong',
        tone === 'info' && 'bg-info-soft text-info-strong border-transparent',
        tone === 'danger' && 'border-danger-border bg-danger-soft text-danger-strong',
        tone === 'success' && 'border-success-border bg-success-soft text-success-strong',
        className,
      )}
      {...props}
    >
      <Icon className="size-4 shrink-0" aria-hidden="true" />
      <div className="flex min-w-0 flex-1 items-center gap-2">{children}</div>
    </div>
  )
}

export { alertVariants }
