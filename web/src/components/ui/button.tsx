import { forwardRef, type ButtonHTMLAttributes } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { Loader2 } from 'lucide-react'
import { cn } from './lib'

/**
 * 按钮的四个变体覆盖原先 className="btn …" 的全部组合：
 * 主操作 primary、次操作 secondary、低调 ghost、破坏性 danger。
 * 尺寸 sm 用于表格行内与密集区，icon 是正方形图标按钮。
 */
const buttonVariants = cva(
  // 基线：行内布局、控件圆角、过渡；触控高度在窄屏由 .touch 抬到 44px。
  'inline-flex select-none items-center justify-center gap-1.5 whitespace-nowrap rounded-sm font-medium transition-colors duration-(--dur-fast) outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50',
  {
    variants: {
      variant: {
        primary: 'bg-primary text-primary-foreground hover:bg-primary-hover',
        secondary:
          'border border-border-strong bg-card text-secondary-foreground hover:bg-secondary',
        ghost: 'text-muted-foreground hover:bg-muted hover:text-foreground',
        danger: 'bg-danger text-white hover:bg-danger-hover',
      },
      size: {
        sm: 'h-7 px-2.5 text-sm',
        md: 'h-9 px-3.5 text-base',
        lg: 'h-10 px-5 text-md',
        icon: 'size-9 p-0',
      },
    },
    defaultVariants: { variant: 'primary', size: 'md' },
  },
)

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  /** 为真时显示转圈并禁用点击；提交中的按钮用它，不用改文案。 */
  busy?: boolean
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { className, variant, size, busy, disabled, children, type = 'button', ...props },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      className={cn(buttonVariants({ variant, size }), className)}
      disabled={disabled || busy}
      {...props}
    >
      {busy ? <Loader2 className="size-4 animate-spin-slow" aria-hidden="true" /> : null}
      {children}
    </button>
  )
})

export { buttonVariants }
