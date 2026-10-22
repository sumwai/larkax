import { useI18n } from '../../i18n'
import type { ConfirmHandle } from './common-helpers'

/** 从打开时的那份值算起，这一页有没有被改过。 */
export function isDirty<T>(current: T, initial: T): boolean {
  return JSON.stringify(current) !== JSON.stringify(initial)
}

/**
 * 关闭表单前先问一句。
 *
 * Esc、点遮罩、右上角的关闭钮最后都走 Modal 的 onClose，拦在这一处就都拦住了。
 */
export function useCloseGuard(confirm: ConfirmHandle, dirty: boolean, close: () => void) {
  const { t } = useI18n()
  return () => {
    if (!dirty) return close()
    confirm.request({
      title: t('ops.discardTitle'),
      body: t('ops.discardBody'),
      label: t('ops.discardConfirm'),
      danger: true,
      run: async () => close(),
    })
  }
}
