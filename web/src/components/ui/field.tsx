import { forwardRef, useId, type InputHTMLAttributes, type ReactNode, type TextareaHTMLAttributes } from 'react'
import { cn } from './lib'

/**
 * 字段的四态：默认、提示（hint）、错误（error）、禁用。
 * id 由组件生成，label 与控件、错误与控件用 aria 关联，不靠手接。
 */
export function Field({
  label,
  hint,
  error,
  children,
  className,
  required,
}: {
  label: ReactNode
  hint?: ReactNode
  error?: ReactNode
  children: (ids: { inputId: string; messageId: string; describedBy?: string }) => ReactNode
  className?: string
  required?: boolean
}) {
  const inputId = useId()
  const messageId = useId()
  const describedBy = hint || error ? messageId : undefined
  return (
    <div className={cn('flex flex-col gap-1.5', className)}>
      <label htmlFor={inputId} className="text-sm font-medium text-foreground">
        {label}
        {required ? (
          <span className="text-danger" aria-hidden="true">
            {' '}
            *
          </span>
        ) : null}
      </label>
      {children({ inputId, messageId, describedBy })}
      {hint && !error ? (
        <p id={messageId} className="text-xs text-faint">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={messageId} className="text-xs font-medium text-danger-strong" role="alert">
          {error}
        </p>
      ) : null}
    </div>
  )
}

/* 聚焦不画环：输入框自带一条描边就够，聚焦时只保留光标。 */
const controlBase =
  'w-full rounded-sm border bg-card px-3 text-base text-foreground transition-colors placeholder:text-faint focus-visible:outline-none disabled:cursor-not-allowed disabled:bg-muted disabled:text-muted-foreground'

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement> & { invalid?: boolean }>(
  function Input({ className, invalid, ...props }, ref) {
    return (
      <input
        ref={ref}
        aria-invalid={invalid || undefined}
        className={cn(
          controlBase,
          'h-9',
          invalid ? 'border-danger' : 'border-input',
          className,
        )}
        {...props}
      />
    )
  },
)

export const Textarea = forwardRef<
  HTMLTextAreaElement,
  TextareaHTMLAttributes<HTMLTextAreaElement> & { invalid?: boolean }
>(function Textarea({ className, invalid, rows = 3, ...props }, ref) {
  return (
    <textarea
      ref={ref}
      rows={rows}
      aria-invalid={invalid || undefined}
      className={cn(controlBase, 'py-2 leading-5', invalid ? 'border-danger' : 'border-input', className)}
      {...props}
    />
  )
})

/**
 * 金额与单位：金额是字符串，不做浮点；单位切换改变的是含义，不是数值。
 * 右对齐、等宽字型是表格里金额的固定形态。
 */
export function MoneyInput({
  className,
  suffix,
  ...props
}: InputHTMLAttributes<HTMLInputElement> & { suffix?: ReactNode }) {
  return (
    <div className="relative">
      <input
        inputMode="decimal"
        className={cn(controlBase, 'h-9 border-input pr-14 text-right font-mono', className)}
        {...props}
      />
      {suffix ? (
        <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-faint">
          {suffix}
        </span>
      ) : null}
    </div>
  )
}
