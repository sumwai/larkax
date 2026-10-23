import { useEffect, type ReactNode } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import * as PopoverPrimitive from '@radix-ui/react-popover'
import { Command } from 'cmdk'
import { ChevronDown } from 'lucide-react'
import { cn } from './lib'

/**
 * 命令面板：mod+K 打开。路由、动作、设置项都进同一份注册表，由调用方分组给。
 * 键盘提示统一走 Kbd 的 token。
 */
export function CommandPalette({
  open,
  onOpenChange,
  groups,
  placeholder = '搜索页面与动作…',
  empty = '没有匹配项',
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  groups: { label: ReactNode; items: { id: string; label: ReactNode; keywords?: string; onSelect: () => void; shortcut?: string[] }[] }[]
  placeholder?: string
  empty?: string
}) {
  // mod+K 全局开关；输入框聚焦时也拦得住，因为监听在 document 层。
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() === 'k' && (event.metaKey || event.ctrlKey)) {
        event.preventDefault()
        onOpenChange(!open)
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [open, onOpenChange])

  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-modal bg-overlay" />
        <DialogPrimitive.Content className="fixed top-[15dvh] left-1/2 z-modal w-[calc(100vw-32px)] max-w-lg -translate-x-1/2 overflow-hidden rounded-md border border-border bg-popover shadow-lg outline-none">
          <DialogPrimitive.Title className="sr-only">命令面板</DialogPrimitive.Title>
          <Command label="命令面板" className="flex flex-col">
            <div className="border-b border-border">
              <Command.Input
                autoFocus
                placeholder={placeholder}
                className="h-11 w-full bg-transparent px-4 text-base outline-none placeholder:text-faint"
              />
            </div>
            <Command.List className="max-h-80 overflow-y-auto p-1.5">
              <Command.Empty className="px-3 py-6 text-center text-sm text-muted-foreground">
                {empty}
              </Command.Empty>
              {groups.map((group) => (
                <Command.Group
                  key={String(group.label)}
                  heading={
                    <span className="px-2 pt-2 pb-1 text-2xs font-semibold text-faint">
                      {group.label}
                    </span>
                  }
                  className="[&_[cmdk-group-heading]]:px-2"
                >
                  {group.items.map((item) => (
                    <Command.Item
                      key={item.id}
                      value={`${item.id} ${item.keywords ?? ''}`}
                      onSelect={() => {
                        onOpenChange(false)
                        item.onSelect()
                      }}
                      className="flex cursor-default items-center gap-2 rounded-xs px-2.5 py-2 text-sm outline-none data-[selected=true]:bg-muted"
                    >
                      <span className="min-w-0 flex-1 truncate">{item.label}</span>
                      {item.shortcut ? (
                        <span className="flex shrink-0 items-center gap-0.5 text-2xs text-faint">
                          {item.shortcut.map((key) => (
                            <kbd
                              key={key}
                              className="rounded-[5px] border border-kbd-border bg-kbd-bg px-1 font-mono text-kbd-fg"
                            >
                              {key}
                            </kbd>
                          ))}
                        </span>
                      ) : null}
                    </Command.Item>
                  ))}
                </Command.Group>
              ))}
            </Command.List>
          </Command>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

/**
 * 可搜索下拉：选项超过八项时替换普通 Select。输入即过滤，键盘仍可走完。
 */
export function Combobox({
  value,
  onValueChange,
  options,
  placeholder = '选择…',
  searchPlaceholder = '搜索…',
  empty = '没有匹配项',
  ariaLabel,
  disabled,
  className,
}: {
  value?: string
  onValueChange: (value: string) => void
  options: { value: string; label: ReactNode; keywords?: string }[]
  placeholder?: string
  searchPlaceholder?: string
  empty?: string
  ariaLabel?: string
  disabled?: boolean
  className?: string
}) {
  const selected = options.find((option) => option.value === value)
  return (
    <PopoverCommand className={className} ariaLabel={ariaLabel} disabled={disabled} label={selected?.label ?? placeholder}>
      <Command className="flex flex-col">
        <div className="border-b border-border">
          <Command.Input
            autoFocus
            placeholder={searchPlaceholder}
            className="h-9 w-full bg-transparent px-3 text-sm outline-none placeholder:text-faint"
          />
        </div>
        <Command.List className="max-h-64 overflow-y-auto p-1">
          <Command.Empty className="px-2.5 py-4 text-center text-xs text-muted-foreground">
            {empty}
          </Command.Empty>
          {options.map((option) => (
            <Command.Item
              key={option.value}
              value={`${option.value} ${option.keywords ?? ''}`}
              onSelect={() => {
                onValueChange(option.value)
              }}
              className="flex cursor-default items-center justify-between gap-2 rounded-xs px-2.5 py-1.5 text-sm outline-none data-[selected=true]:bg-muted"
            >
              <span className="min-w-0 truncate">{option.label}</span>
              {option.value === value ? (
                <span aria-hidden="true" className="text-2xs text-muted-foreground">
                  ✓
                </span>
              ) : null}
            </Command.Item>
          ))}
        </Command.List>
      </Command>
    </PopoverCommand>
  )
}

/**
 * Combobox 的面板壳：trigger 与 popover 的组合。
 * 单独拆出来是因为 cmdk 的过滤要在 Popover 打开时才工作。
 */
function PopoverCommand({
  label,
  ariaLabel,
  disabled,
  className,
  children,
}: {
  label: ReactNode
  ariaLabel?: string
  disabled?: boolean
  className?: string
  children: ReactNode
}) {
  return (
    <PopoverPrimitive.Root>
      <PopoverPrimitive.Trigger
        aria-label={ariaLabel}
        disabled={disabled}
        className={cn(
          'flex h-9 w-full items-center justify-between gap-2 rounded-sm border border-input bg-card px-3 text-left text-base outline-none transition-colors focus-visible:outline-2 focus-visible:outline-offset-0 focus-visible:outline-ring disabled:cursor-not-allowed disabled:bg-muted disabled:text-muted-foreground',
          className,
        )}
      >
        <span className="min-w-0 truncate">{label}</span>
        <ChevronDown className="size-4 shrink-0 text-faint" aria-hidden="true" />
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Portal>
        <PopoverPrimitive.Content
          sideOffset={4}
          className="z-dropdown w-(--radix-popover-trigger-width) overflow-hidden rounded-sm border border-border bg-popover shadow-md"
        >
          {children}
        </PopoverPrimitive.Content>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  )
}
