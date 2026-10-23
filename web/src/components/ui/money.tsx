import { moneySymbol, formatBalance, formatSpend, formatMoney } from '../../lib/format'
import type { Money } from '../../api/types'
import { cn } from './lib'

/**
 * 金额展示：金额是字符串，不做浮点。币种符号前置；`<0.001` 这类下界的
 * 比较符落在符号之前，否则读成「$ 小于」。
 */
export function Money({
  value,
  currency,
  variant,
  className,
}: {
  value: Money | null | undefined
  currency?: string
  /** `balance` 是余额（2 位截断），`spend` 是消费（3 位截断）；不传按价格口径。 */
  variant?: 'balance' | 'spend'
  className?: string
}) {
  const symbol = moneySymbol(currency)
  const text =
    variant === 'balance' ? formatBalance(value) : variant === 'spend' ? formatSpend(value) : formatMoney(value)
  const marker = text.startsWith('<') || text.startsWith('>') ? text[0] : ''
  const amount = marker ? text.slice(1) : text
  return (
    <span className={cn('num font-mono', className)}>
      {marker}
      {symbol ? <small>{symbol}</small> : null}
      {amount}
    </span>
  )
}
