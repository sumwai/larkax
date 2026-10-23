import { describe, expect, it } from 'vitest'
import { formatCompact, formatTps } from './format'

/**
 * 请求日志的 token 列用 `formatCompact`：万级往上的四五个零在列宽里读不出
 * 差别，所以一万起换成 K / M / B；一千到九千九百九十九仍写带千分位的整数。
 */
describe('formatCompact：token 列的紧凑单位', () => {
  it('一千到九千九百九十九仍是带千分位的整数', () => {
    expect(formatCompact(999)).toBe('999')
    expect(formatCompact(1000)).toBe('1,000')
    expect(formatCompact(9999)).toBe('9,999')
  })

  it('一万起换 K，百万换 M，十亿换 B', () => {
    expect(formatCompact(10_000)).toBe('10K')
    expect(formatCompact(2_500_000)).toBe('2.5M')
    expect(formatCompact(1_000_000_000)).toBe('1B')
  })

  it('没有值时留占位符', () => {
    expect(formatCompact(null)).toBe('—')
    expect(formatCompact(undefined)).toBe('—')
    expect(formatCompact(Number.NaN)).toBe('—')
  })
})

/**
 * TPS 的口径：（输出 + 推理）÷（耗时 − TTFT），单位 token/秒。
 * 分母不含排队与 TTFT——那段是等待，不是生成。
 */
describe('formatTps：生成速度', () => {
  it('推理 token 也算进产出', () => {
    // (800 + 200) ÷ (3.0s − 250ms) = 1000 ÷ 2.75 = 363.63…/s
    expect(formatTps(800, 200, 3000, 250)).toBe('363.6')
    // 只算正文会得到 290.9，少掉一截；这一格钉住「推理也算」
    expect(formatTps(800, 0, 3000, 250)).toBe('290.9')
  })

  it('分母为零或为负时算不出来', () => {
    expect(formatTps(500, 0, 250, 250)).toBeNull()
    expect(formatTps(500, 0, 200, 250)).toBeNull()
  })

  it('缺 TTFT 或缺耗时（非流式）时算不出来', () => {
    expect(formatTps(500, 0, 2000, null)).toBeNull()
    expect(formatTps(500, 0, null, 200)).toBeNull()
  })

  it('没有产出时算不出来', () => {
    expect(formatTps(0, 0, 2000, 200)).toBeNull()
  })
})
