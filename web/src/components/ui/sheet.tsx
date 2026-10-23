import type { ReactNode } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { cn } from './lib'
import { Button } from './button'

/**
 * 移动端底部抽屉。三档高度对齐旧 ActionSheet 的语义：
 * sm 30%（确认类）、md 50%（选择类）、lg 80%（近全屏表单）。
 * 行为归 Radix Dialog：焦点困在抽屉里、Esc 关闭、关闭后归还焦点。
 */
export const SHEET_SIZES = { sm: 30, md: 50, lg: 80 } as const
export type SheetSize = keyof typeof SHEET_SIZES

export function Sheet({
  open,
  onOpenChange,
  title,
  children,
  size = 'md',
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  children: ReactNode
  size?: SheetSize
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-sheet bg-overlay" />
        <DialogPrimitive.Content
          className={cn(
            'fixed inset-x-0 bottom-0 z-sheet flex max-h-[100dvh] flex-col rounded-t-lg border-t border-border bg-popover text-popover-foreground shadow-sheet outline-none',
          )}
          style={{ height: `${SHEET_SIZES[size]}dvh` }}
        >
          <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
            <DialogPrimitive.Title className="text-md font-semibold tracking-tight">
              {title}
            </DialogPrimitive.Title>
            <DialogPrimitive.Close asChild>
              <Button variant="ghost" size="icon" className="size-8" aria-label="关闭">
                ✕
              </Button>
            </DialogPrimitive.Close>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-4 py-4">
            {children}
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

/**
 * 动作面板：一列竖排的大按钮，破坏性动作染 danger，最底下是取消。
 */
export function ActionSheet({
  open,
  onOpenChange,
  actions,
  title,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title?: ReactNode
  actions: { label: ReactNode; onSelect: () => void; danger?: boolean; disabled?: boolean }[]
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-sheet bg-overlay" />
        <DialogPrimitive.Content className="fixed inset-x-0 bottom-0 z-sheet rounded-t-lg border-t border-border bg-popover p-3 pb-(--safe-bottom,12px) shadow-sheet outline-none">
          {title ? (
            <DialogPrimitive.Title className="px-2 pb-2 text-xs text-faint">{title}</DialogPrimitive.Title>
          ) : (
            <DialogPrimitive.Title className="sr-only">操作</DialogPrimitive.Title>
          )}
          <div className="flex flex-col gap-1">
            {actions.map((action) => (
              <button
                key={String(action.label)}
                type="button"
                disabled={action.disabled}
                onClick={() => {
                  onOpenChange(false)
                  action.onSelect()
                }}
                className={cn(
                  'h-11 rounded-sm px-3 text-left text-base font-medium transition-colors disabled:opacity-50',
                  action.danger
                    ? 'bg-danger-soft text-danger-strong'
                    : 'bg-muted text-foreground active:bg-secondary',
                )}
              >
                {action.label}
              </button>
            ))}
            <DialogPrimitive.Close asChild>
              <Button variant="secondary" className="mt-1 h-11">
                取消
              </Button>
            </DialogPrimitive.Close>
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
