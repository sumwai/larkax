import { forwardRef, type ReactNode } from 'react'
import * as SwitchPrimitive from '@radix-ui/react-switch'
import * as CheckboxPrimitive from '@radix-ui/react-checkbox'
import { Check, Minus } from 'lucide-react'
import { cn } from './lib'

/**
 * Switch：启用/停用的状态表达。状态与动作分开——开关只改状态，
 * 有副作用的动作仍走确认。
 */
export const Switch = forwardRef<
  React.ComponentRef<typeof SwitchPrimitive.Root>,
  React.ComponentProps<typeof SwitchPrimitive.Root>
>(function Switch({ className, ...props }, ref) {
  return (
    <SwitchPrimitive.Root
      ref={ref}
      className={cn(
        'peer inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-transparent transition-colors outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:bg-control-active-bg data-[state=unchecked]:bg-border-strong',
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        className={cn(
          'pointer-events-none block size-4 rounded-full shadow-xs transition-transform data-[state=checked]:translate-x-[18px] data-[state=unchecked]:translate-x-0.5',
        )}
        style={{ background: 'var(--switch-thumb)' }}
      />
    </SwitchPrimitive.Root>
  )
})

/**
 * Checkbox：行选择与批量操作。半选态由表格的全选逻辑给。
 */
export const Checkbox = forwardRef<
  React.ComponentRef<typeof CheckboxPrimitive.Root>,
  React.ComponentProps<typeof CheckboxPrimitive.Root> & { indeterminate?: boolean }
>(function Checkbox({ className, indeterminate, checked, ...props }, ref) {
  return (
    <CheckboxPrimitive.Root
      ref={ref}
      checked={indeterminate ? 'indeterminate' : checked}
      className={cn(
        'peer size-4 shrink-0 rounded-[4px] border border-border-strong bg-card outline-none transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:border-control-active-bg data-[state=checked]:bg-control-active-bg data-[state=indeterminate]:border-control-active-bg data-[state=indeterminate]:bg-control-active-bg',
        className,
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator className="flex items-center justify-center text-white">
        {indeterminate ? (
          <Minus className="size-3" />
        ) : (
          <Check className="size-3" />
        )}
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
})

/**
 * 三态能力：目录里每项能力有「未知 / 支持 / 不支持」三种，不能糊成一个开关。
 * 值域是 true / false / null，null 就是未知。
 */
export type TriStateValue = boolean | null

export function TriState({
  value,
  onValueChange,
  labels = { yes: '支持', no: '不支持', unknown: '未知' },
  disabled,
  className,
}: {
  value: TriStateValue
  onValueChange: (value: TriStateValue) => void
  labels?: { yes: ReactNode; no: ReactNode; unknown: ReactNode }
  disabled?: boolean
  className?: string
}) {
  const options: { value: TriStateValue; label: ReactNode }[] = [
    { value: true, label: labels.yes },
    { value: false, label: labels.no },
    { value: null, label: labels.unknown },
  ]
  return (
    <div
      role="radiogroup"
      aria-disabled={disabled}
      className={cn('inline-flex items-center gap-0.5 rounded-sm bg-muted p-0.5', className)}
    >
      {options.map((option) => {
        const active = value === option.value
        return (
          <button
            key={String(option.value)}
            type="button"
            role="radio"
            aria-checked={active}
            disabled={disabled}
            onClick={() => onValueChange(option.value)}
            className={cn(
              'inline-flex h-6 items-center rounded-[6px] px-2 text-xs font-medium transition-colors outline-none focus-visible:outline-2 focus-visible:outline-ring disabled:opacity-50',
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
