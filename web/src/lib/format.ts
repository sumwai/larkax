import type { Money } from '../api/types'

interface ParsedMoney {
  negative: boolean
  /** 按位数截断后的整数单位，1 单位 = 10^-places。 */
  units: bigint
  /** 原值里是否有非零数字。截断成 0 时，用它区分「真的是零」与「小于显示精度」。 */
  nonzero: boolean
}

/**
 * 把金额字符串解析成定点整数，小数位按 `places` 直接截断。
 *
 * 全程只碰字符串与 BigInt，不经过 double——钱不该出现 0.1 + 0.2 那种误差。
 * 格式不对（含空值）返回 null，由调用方决定显示什么。
 */
function parseMoney(value: Money | null | undefined, places: number): ParsedMoney | null {
  if (value === null || value === undefined) return null
  const text = String(value).trim()
  if (!/^-?\d+(\.\d+)?$/.test(text)) return null

  const negative = text.startsWith('-')
  const unsigned = negative ? text.slice(1) : text
  const [whole, fraction = ''] = unsigned.split('.')
  const digits = (fraction + '0'.repeat(places)).slice(0, places)

  return {
    negative,
    units: BigInt(whole + digits),
    nonzero: /[1-9]/.test(whole + fraction),
  }
}

/** 把定点整数渲染回带千分位的金额；负的零不显示 `-0.00`。 */
function renderUnits(units: bigint, places: number, negative: boolean): string {
  const scale = 10n ** BigInt(places)
  const whole = (units / scale).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const fraction = (units % scale).toString().padStart(places, '0')
  const sign = negative && units > 0n ? '-' : ''
  return `${sign}${whole}.${fraction}`
}

/**
 * 金额格式化：接口给 8 位小数字符串，界面显示 2 位，第三位四舍五入。
 *
 * 用于价格与额度上限这类「约定值」；余额与消费各用自己的口径，见
 * `formatBalance` 与 `formatSpend`。
 */
export function formatMoney(value: Money | null | undefined): string {
  const parsed = parseMoney(value, 3)
  if (!parsed) return '—'

  // 先截到 3 位，再看第 3 位决定前两位是否进位。
  let cents = parsed.units / 10n
  if (parsed.units % 10n >= 5n) cents += 1n
  return renderUnits(cents, 2, parsed.negative)
}

/**
 * 余额：2 位小数，直接截断。
 *
 * 余额不能因为四舍五入显示成比实际更多的钱（`0.99877330` 显示 `0.99`）。
 */
export function formatBalance(value: Money | null | undefined): string {
  const parsed = parseMoney(value, 2)
  if (!parsed) return '—'
  return renderUnits(parsed.units, 2, parsed.negative)
}

/**
 * 消费：3 位小数，直接截断；非零但不足 0.001 的显示 `<0.001`。
 *
 * 单次请求的消费常在 0.0001 量级，舍成 2 位会全是 `0.00`；截断而不是
 * 四舍五入，避免把「还没花到」显示成「花到了」。
 */
export function formatSpend(value: Money | null | undefined): string {
  const parsed = parseMoney(value, 3)
  if (!parsed) return '—'
  if (parsed.units === 0n) {
    if (!parsed.nonzero) return '0.000'
    return parsed.negative ? '>-0.001' : '<0.001'
  }
  return renderUnits(parsed.units, 3, parsed.negative)
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
