import { type ReactNode } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import { cn } from './lib'
import { Button } from './button'

/**
 * 对话框：行为归 Radix（焦点落在第一个可编辑控件、Esc 关闭、Tab 在框内环绕、
 * 打开时锁滚动），外观归这里的 token。
 */
export const Dialog = DialogPrimitive.Root
export const DialogTrigger = DialogPrimitive.Trigger
export const DialogClose = DialogPrimitive.Close

const overlayClass = 'fixed inset-0 z-modal bg-overlay'

export interface DialogContentProps {
  title: ReactNode
  description?: ReactNode
  /** 正文；确认框这类只有标题的可以不给。 */
  children?: ReactNode
  /** 触发钮；给了它就把这个钮放进同一个 Root，不能另起一个。 */
  trigger?: ReactNode
  /** 底部操作区；调用方给取消与确认按钮。 */
  footer?: ReactNode
  /** 宽度档；表单类用 md，业务大卡用 lg。超高时内容区自己滚动。 */
  size?: 'sm' | 'md' | 'lg'
  /** 点遮罩是否关闭。内容只出现一次时置 false，误点不会把它弄丢。 */
  dismissOnOutsideClick?: boolean
  open?: boolean
  onOpenChange?: (open: boolean) => void
}

/* 宽度按档位固定：宽度不可插值，auto 与 auto 之间没法做过渡，一改内容就会
   横向跳一下。窄的确认框仍然贴内容（auto + min/max），表单类给满档位。 */
const sizeClass = {
  sm: 'w-auto min-w-72 max-w-sm',
  md: 'w-full max-w-md',
  lg: 'w-full max-w-2xl',
} as const

export function DialogContent({
  title,
  description,
  children,
  trigger,
  footer,
  size = 'md',
  dismissOnOutsideClick = true,
  open,
  onOpenChange,
}: DialogContentProps) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      {trigger ? <DialogPrimitive.Trigger asChild>{trigger}</DialogPrimitive.Trigger> : null}
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className={overlayClass} />
        <DialogPrimitive.Content
          onInteractOutside={(event) => {
            if (!dismissOnOutsideClick) event.preventDefault()
          }}
          className={cn(
            'fixed top-1/2 left-1/2 z-modal flex max-h-[calc(100dvh-32px)] max-w-[calc(100vw-32px)] -translate-x-1/2 -translate-y-1/2 flex-col rounded-md border border-border bg-popover text-popover-foreground shadow-lg outline-none [interpolate-size:allow-keywords] transition-[width,height] duration-(--dur)',
            sizeClass[size],
          )}
        >
          <div className="flex items-start justify-between gap-3 border-b border-border px-4 py-3">
            <div className="min-w-0">
              <DialogPrimitive.Title className="text-md font-semibold tracking-tight">
                {title}
              </DialogPrimitive.Title>
              {description ? (
                <DialogPrimitive.Description className="mt-0.5 text-sm text-muted-foreground">
                  {description}
                </DialogPrimitive.Description>
              ) : null}
            </div>
            <DialogPrimitive.Close asChild>
              <Button
                variant="ghost"
                size="icon"
                className="-mr-1.5 -mt-1 size-8"
                aria-label="关闭"
              >
                <X className="size-4" />
              </Button>
            </DialogPrimitive.Close>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">{children}</div>
          {footer ? (
            <div className="flex items-center justify-end gap-2 border-t border-border px-4 py-3">
              {footer}
            </div>
          ) : null}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

/**
 * 确认对话框：破坏性动作前的最后一问。确认钮用 danger 变体。
 */
export function ConfirmDialog({
  trigger,
  title,
  description,
  confirmLabel,
  cancelLabel = '取消',
  danger,
  busy,
  onConfirm,
}: {
  trigger: ReactNode
  title: ReactNode
  description?: ReactNode
  confirmLabel: ReactNode
  cancelLabel?: ReactNode
  danger?: boolean
  busy?: boolean
  onConfirm: () => void
}) {
  return (
    <DialogContent
      size="sm"
      trigger={trigger}
      title={title}
      description={description}
      footer={
        <>
          <DialogClose asChild>
            <Button variant="secondary">{cancelLabel}</Button>
          </DialogClose>
          <DialogClose asChild>
            <Button variant={danger ? 'danger' : 'primary'} busy={busy} onClick={onConfirm}>
              {confirmLabel}
            </Button>
          </DialogClose>
        </>
      }
    />
  )
}
