import { describe, expect, it } from 'vitest'
import {
  addDays,
  exclusiveEnd,
  formatBalance,
  formatCount,
  formatDate,
  formatDateTime,
  formatMoney,
  formatSpend,
  formatUnitPrice,
  moneySymbol,
} from './format'

describe('formatMoney', () => {
  it('把 8 位小数的金额收成 2 位', () => {
    expect(formatMoney('12.34000000')).toBe('12.34')
    expect(formatMoney('0.00000000')).toBe('0.00')
  })

  it('按千分位分组，不丢整数位', () => {
    expect(formatMoney('1234.56780000')).toBe('1,234.57')
    expect(formatMoney('1234567.89120000')).toBe('1,234,567.89')
  })

  it('第三位小数四舍五入', () => {
    expect(formatMoney('0.00500000')).toBe('0.01')
    expect(formatMoney('0.00499999')).toBe('0.00')
    expect(formatMoney('1.99900000')).toBe('2.00')
  })

  it('负数保留符号，但不会出现 -0.00', () => {
    expect(formatMoney('-1.00500000')).toBe('-1.01')
    expect(formatMoney('-0.00400000')).toBe('0.00')
  })

  it('缺值与坏数据都显示破折号，而不是 NaN', () => {
    expect(formatMoney(null)).toBe('—')
    expect(formatMoney(undefined)).toBe('—')
    expect(formatMoney('abc')).toBe('—')
    expect(formatMoney('1.2.3')).toBe('—')
  })

  it('不经过 double：大额金额的最后一位仍然准确', () => {
    expect(formatMoney('9007199254740993.12000000')).toBe('9,007,199,254,740,993.12')
  })
})

describe('formatBalance', () => {
  it('2 位小数直接截断，不四舍五入', () => {
    expect(formatBalance('0.99877330')).toBe('0.99')
    expect(formatBalance('1.99900000')).toBe('1.99')
    expect(formatBalance('0.00900000')).toBe('0.00')
  })

  it('保留千分位，负的零不带符号', () => {
    expect(formatBalance('1234567.89120000')).toBe('1,234,567.89')
    expect(formatBalance('-1.99900000')).toBe('-1.99')
    expect(formatBalance('-0.00900000')).toBe('0.00')
  })

  it('缺值与坏数据都显示破折号', () => {
    expect(formatBalance(null)).toBe('—')
    expect(formatBalance(undefined)).toBe('—')
    expect(formatBalance('abc')).toBe('—')
    expect(formatBalance('')).toBe('—')
  })
})

describe('formatSpend', () => {
  it('3 位小数直接截断，不四舍五入', () => {
    expect(formatSpend('0.00190000')).toBe('0.001')
    expect(formatSpend('1.99990000')).toBe('1.999')
  })

  it('非零但不足 0.001 给区间下界，零显示 0.000', () => {
    expect(formatSpend('0.00010000')).toBe('<0.001')
    expect(formatSpend('0.00000001')).toBe('<0.001')
    expect(formatSpend('0.00100000')).toBe('0.001')
    expect(formatSpend('0')).toBe('0.000')
    expect(formatSpend('0.00000000')).toBe('0.000')
    expect(formatSpend('-0.00000000')).toBe('0.000')
  })

  it('负的小额对称显示 >-0.001，够 0.001 后带符号', () => {
    expect(formatSpend('-0.00010000')).toBe('>-0.001')
    expect(formatSpend('-0.00100000')).toBe('-0.001')
    expect(formatSpend('-2.34590000')).toBe('-2.345')
  })

  it('缺值与坏数据都显示破折号', () => {
    expect(formatSpend(null)).toBe('—')
    expect(formatSpend(undefined)).toBe('—')
    expect(formatSpend('abc')).toBe('—')
    expect(formatSpend('1.2.3')).toBe('—')
  })
})

describe('moneySymbol', () => {
  it('常见币种给符号，未知币种退成代码', () => {
    expect(moneySymbol('USD')).toBe('$')
    expect(moneySymbol('CNY')).toBe('¥')
    expect(moneySymbol('EUR')).toBe('€')
    expect(moneySymbol('CHF')).toBe('CHF ')
    expect(moneySymbol(undefined)).toBe('')
  })
})

describe('formatCount', () => {
  it('整数带千分位，缺值给破折号', () => {
    expect(formatCount(1234567, 'en')).toBe('1,234,567')
    expect(formatCount(null)).toBe('—')
  })
})

describe('时间与日期', () => {
  it('绝对时间按站点时区渲染成 YYYY-MM-DD HH:mm', () => {
    expect(formatDateTime('2026-10-21T03:04:05Z', 'UTC')).toBe('2026-10-21 03:04')
    expect(formatDateTime('2026-10-21T20:04:05Z', 'Asia/Shanghai')).toBe('2026-10-22 04:04')
    expect(formatDateTime(null)).toBe('—')
  })

  it('日期只保留日期部分', () => {
    expect(formatDate('2026-10-21T23:59:59Z', 'UTC')).toBe('2026-10-21')
  })

  it('区间上界加一天，接口的 to 不含当天', () => {
    expect(addDays('2026-10-21', 1)).toBe('2026-10-22')
    expect(addDays('2026-12-31', 1)).toBe('2027-01-01')
    expect(exclusiveEnd('2026-10-21')).toBe('2026-10-22')
  })
})

describe('formatUnitPrice', () => {
  it('保留至少 4 位小数，去掉末尾多余的 0', () => {
    expect(formatUnitPrice('0.00350000')).toBe('0.0035')
    expect(formatUnitPrice('0.00400000')).toBe('0.0040')
    expect(formatUnitPrice('12.34000000')).toBe('12.3400')
  })

  it('小于 0.0001 的价不被收成 0', () => {
    expect(formatUnitPrice('0.00000025')).toBe('0.00000025')
  })

  it('零元显示 0.0000，而不是破折号', () => {
    expect(formatUnitPrice('0.00000000')).toBe('0.0000')
  })

  it('负数保留符号；缺值与坏数据显示破折号', () => {
    expect(formatUnitPrice('-0.00350000')).toBe('-0.0035')
    expect(formatUnitPrice(null)).toBe('—')
    expect(formatUnitPrice('abc')).toBe('—')
  })
})
