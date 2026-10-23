import type { ReactNode } from 'react'
import * as AlertDialogPrimitive from '@radix-ui/react-alert-dialog'
import { Button } from './button'

/**
 * 破坏性动作的确认：与普通确认框不同——遮罩不可点掉，Esc 可以（那是明确的
 * 「不做了」）。按钮固定为 取消 + 危险确认 的顺序与颜色。
 */
export function AlertDialog({
  trigger,
  title,
  body,
  cancelLabel = '取消',
  confirmLabel,
  busy,
  onConfirm,
}: {
  trigger: ReactNode
  title: ReactNode
  body?: ReactNode
  cancelLabel?: ReactNode
  confirmLabel: ReactNode
  busy?: boolean
  onConfirm: () => void
}) {
  return (
    <AlertDialogPrimitive.Root>
      <AlertDialogPrimitive.Trigger asChild>{trigger}</AlertDialogPrimitive.Trigger>
      <AlertDialogPrimitive.Portal>
        <AlertDialogPrimitive.Overlay className="fixed inset-0 z-modal bg-overlay" />
        <AlertDialogPrimitive.Content className="fixed top-1/2 left-1/2 z-modal w-[calc(100vw-32px)] max-w-sm -translate-x-1/2 -translate-y-1/2 rounded-md border border-border bg-popover p-4 shadow-lg outline-none">
          <AlertDialogPrimitive.Title className="text-md font-semibold tracking-tight">
            {title}
          </AlertDialogPrimitive.Title>
          {body ? (
            <AlertDialogPrimitive.Description className="mt-1.5 text-sm text-muted-foreground">
              {body}
            </AlertDialogPrimitive.Description>
          ) : null}
          <div className="mt-4 flex items-center justify-end gap-2">
            <AlertDialogPrimitive.Cancel asChild>
              <Button variant="secondary" disabled={busy}>
                {cancelLabel}
              </Button>
            </AlertDialogPrimitive.Cancel>
            <AlertDialogPrimitive.Action asChild>
              <Button variant="danger" busy={busy} onClick={onConfirm}>
                {confirmLabel}
              </Button>
            </AlertDialogPrimitive.Action>
          </div>
        </AlertDialogPrimitive.Content>
      </AlertDialogPrimitive.Portal>
    </AlertDialogPrimitive.Root>
  )
}
