import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '../i18n'
import { Modal, type SheetSize } from './ui'

/**
 * 从底部弹出的一组动作，每项一行。
 *
 * 停用渠道这类「点了就生效」的动作在窄屏上走这里，而不是贴底的确认弹窗：
 * 一行一个目标，误点的范围小得多。危险项同时给颜色与文字两条通道——只靠
 * 红色，色觉障碍的用户读不出「这一项和别的不一样」。
 */
export type ActionSheetItem = {
  key: string
  label: string
  description?: string
  icon?: ReactNode
  danger?: boolean
  /** 跳到另一页；给了就不调 `onSelect`。 */
  to?: string
  onSelect?: () => void
}

export function ActionSheet({
  title,
  description,
  items,
  onClose,
  // 四五个动作行在 30% 里会被裁掉，默认给到半屏；调用方可以按项数改档。
  size = 'md',
}: {
  title: ReactNode
  description?: ReactNode
  items: ActionSheetItem[]
  onClose: () => void
  size?: SheetSize
}) {
  const { t } = useI18n()

  const body = (item: ActionSheetItem) => (
    <>
      {item.icon && (
        <span className="ico" aria-hidden="true">
          {item.icon}
        </span>
      )}
      <span className="txt">
        <b>{item.label}</b>
        {item.description && <span>{item.description}</span>}
      </span>
      {item.danger && <span className="sr-only">{t('common.dangerAction')}</span>}
    </>
  )

  const className = (item: ActionSheetItem) =>
    item.danger ? 'action-sheet-item danger' : 'action-sheet-item'

  return (
    <Modal sheet sheetSize={size} title={title} description={description} onClose={onClose}>
      <ul className="action-sheet">
        {items.map((item) => (
          <li key={item.key}>
            {item.to ? (
              <Link className={className(item)} to={item.to} onClick={onClose}>
                {body(item)}
              </Link>
            ) : (
              <button
                type="button"
                className={className(item)}
                onClick={() => {
                  onClose()
                  item.onSelect?.()
                }}
              >
                {body(item)}
              </button>
            )}
          </li>
        ))}
        <li>
          <button type="button" className="action-sheet-item cancel" onClick={onClose}>
            {t('common.cancel')}
          </button>
        </li>
      </ul>
    </Modal>
  )
}
