import { forwardRef, type ReactNode } from 'react'
import * as SelectPrimitive from '@radix-ui/react-select'
import { Check, ChevronDown } from 'lucide-react'
import { cn } from './lib'

/**
 * Select：行为归 Radix（键盘走完、类型ahead、Esc 关闭并归还焦点）。
 * 选项少于四项时页面该用分段，多于八项换带搜索的 Combobox。
 */
export interface SelectOption {
  value: string
  label: ReactNode
  disabled?: boolean
}

export interface SelectProps {
  options: SelectOption[]
  value?: string
  onValueChange?: (value: string) => void
  placeholder?: string
  /** 无障碍名：可视 label 由页面用 Field 给，这里给屏幕阅读器兜底。 */
  ariaLabel?: string
  id?: string
  disabled?: boolean
  className?: string
  size?: 'sm' | 'md'
}

export const Select = forwardRef<HTMLButtonElement, SelectProps>(function Select(
  { options, value, onValueChange, placeholder, ariaLabel, id, disabled, className, size = 'md' },
  ref,
) {
  return (
    <SelectPrimitive.Root value={value} onValueChange={onValueChange} disabled={disabled}>
      <SelectPrimitive.Trigger
        ref={ref}
        id={id}
        aria-label={ariaLabel}
        className={cn(
          'flex w-full items-center justify-between gap-2 rounded-sm border border-input bg-card px-3 text-left text-foreground transition-colors outline-none data-[placeholder]:text-faint focus-visible:outline-2 focus-visible:outline-offset-0 focus-visible:outline-ring disabled:cursor-not-allowed disabled:bg-muted disabled:text-muted-foreground',
          size === 'sm' ? 'h-7 text-sm' : 'h-9 text-base',
          className,
        )}
      >
        <SelectPrimitive.Value placeholder={placeholder} />
        <SelectPrimitive.Icon>
          <ChevronDown className="size-4 shrink-0 text-faint" aria-hidden="true" />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          position="popper"
          sideOffset={4}
          className="z-dropdown max-h-72 min-w-(--radix-select-trigger-width) overflow-hidden rounded-sm border border-border bg-popover text-popover-foreground shadow-md"
        >
          <SelectPrimitive.Viewport className="p-1">
            {options.map((option) => (
              <SelectPrimitive.Item
                key={option.value}
                value={option.value}
                disabled={option.disabled}
                className={cn(
                  'flex cursor-default items-center justify-between gap-2 rounded-xs px-2.5 py-1.5 text-sm outline-none data-[disabled]:pointer-events-none data-[disabled]:opacity-50 data-[highlighted]:bg-muted',
                )}
              >
                <SelectPrimitive.ItemText>{option.label}</SelectPrimitive.ItemText>
                <SelectPrimitive.ItemIndicator>
                  <Check className="size-3.5 text-foreground" aria-hidden="true" />
                </SelectPrimitive.ItemIndicator>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  )
})
