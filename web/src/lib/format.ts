import type { Money } from '../api/types'

/**
 * 金额格式化：接口给 8 位小数字符串，界面显示 2 位。
 *
 * 全程按字符串与 BigInt 计算，不经过 double——钱不该出现 0.1 + 0.2 那种误差。
 * 第三位小数用「四舍五入」；负数的零不显示成 `-0.00`。
 */
export function formatMoney(value: Money | null | undefined): string {
  if (value === null || value === undefined) return '—'
  const text = String(value).trim()
  if (!/^-?\d+(\.\d+)?$/.test(text)) return '—'

  const negative = text.startsWith('-')
  const unsigned = negative ? text.slice(1) : text
  const [integerPart, fractionPart = ''] = unsigned.split('.')
  const digits = (fractionPart + '000').slice(0, 3)

  let cents = BigInt(integerPart + digits.slice(0, 2))
  if (digits[2] >= '5') cents += 1n

  const whole = cents / 100n
  const fraction = (cents % 100n).toString().padStart(2, '0')
  const grouped = whole.toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const sign = negative && cents > 0n ? '-' : ''
  return `${sign}${grouped}.${fraction}`
}

export function moneySymbol(currency: string | undefined): string {
  switch ((currency ?? '').toUpperCase()) {
    case 'USD':
      return '$'
    case 'CNY':
    case 'RMB':
      return '¥'
    case 'EUR':
      return '€'
    case 'GBP':
      return '£'
    case 'JPY':
      return '¥'
    default:
      return currency ? `${currency} ` : ''
  }
}

/** 给 KPI 这类整数字段用，带千分位。 */
export function formatCount(value: number | null | undefined, lang: 'zh' | 'en' = 'zh'): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '—'
  return new Intl.NumberFormat(lang === 'zh' ? 'zh-CN' : 'en-US').format(value)
}

/** 大数换成一个更短的写法，用在卡片上。 */
export function formatCompact(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '—'
  if (Math.abs(value) < 10_000) return formatCount(value)
  return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(value)
}

function partsInZone(iso: string, timeZone: string): Intl.DateTimeFormatPart[] | null {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return null
  try {
    return new Intl.DateTimeFormat('en-CA', {
      timeZone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    }).formatToParts(date)
  } catch {
    return null
  }
}

function pick(parts: Intl.DateTimeFormatPart[], type: Intl.DateTimeFormatPartTypes): string {
  return parts.find((part) => part.type === type)?.value ?? ''
}

/** 绝对时间统一 `YYYY-MM-DD HH:mm`，按站点时区。 */
export function formatDateTime(iso: string | null | undefined, timeZone = 'UTC'): string {
  if (!iso) return '—'
  const parts = partsInZone(iso, timeZone)
  if (!parts) return '—'
  const hour = pick(parts, 'hour') === '24' ? '00' : pick(parts, 'hour')
  return `${pick(parts, 'year')}-${pick(parts, 'month')}-${pick(parts, 'day')} ${hour}:${pick(parts, 'minute')}`
}

export function formatDate(iso: string | null | undefined, timeZone = 'UTC'): string {
  if (!iso) return '—'
  const parts = partsInZone(iso, timeZone)
  if (!parts) return '—'
  return `${pick(parts, 'year')}-${pick(parts, 'month')}-${pick(parts, 'day')}`
}

/** 站点时区里的今天，`YYYY-MM-DD`。 */
export function todayInZone(timeZone = 'UTC'): string {
  const parts = partsInZone(new Date().toISOString(), timeZone)
  if (!parts) return new Date().toISOString().slice(0, 10)
  return `${pick(parts, 'year')}-${pick(parts, 'month')}-${pick(parts, 'day')}`
}

/** `YYYY-MM-DD` 的日期加减；只做日期算术，不涉及时分秒。 */
export function addDays(date: string, days: number): string {
  const value = new Date(`${date}T00:00:00Z`)
  value.setUTCDate(value.getUTCDate() + days)
  return value.toISOString().slice(0, 10)
}

export function diffDays(from: string, to: string): number {
  const start = new Date(`${from}T00:00:00Z`).getTime()
  const end = new Date(`${to}T00:00:00Z`).getTime()
  return Math.round((end - start) / 86_400_000)
}

/** 把界面上的 `to`（含当天）换成请求里的开区间上界（`to` 不含）。 */
export function exclusiveEnd(to: string): string {
  return addDays(to, 1)
}
