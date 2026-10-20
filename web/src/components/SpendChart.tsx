import { useState } from 'react'
import type { UsageSeriesPoint } from '../api/types'
import { useI18n } from '../i18n'
import { formatDate, formatMoney, moneySymbol } from '../lib/format'

/**
 * 近 7 天花费：单序列柱图。
 *
 * 一条序列不需要图例，标题就说明了它是什么；每根柱子悬停给出当天金额，
 * 数据表放在图下方供读屏与逐日核对。
 */
export function SpendChart({
  points,
  currency,
  timezone,
}: {
  points: UsageSeriesPoint[]
  currency?: string
  /** 桶的起点带站点偏移，标签按同一时区取日期，否则凌晨会差一天。 */
  timezone?: string
}) {
  const { t } = useI18n()
  const [hovered, setHovered] = useState<number | null>(null)
  const symbol = moneySymbol(currency)

  const values = points.map((point) => Number.parseFloat(point.cost) || 0)
  const max = Math.max(...values, 0)
  const total = values.reduce((sum, value) => sum + value, 0)

  return (
    <div className="chart-wrap">
      <div
        className="bars"
        role="img"
        aria-label={`${t('overview.last7Days')}: ${symbol}${formatMoney(String(total))}`}
      >
        {points.map((point, index) => {
          const percent = max > 0 ? (values[index] / max) * 100 : 0
          const day = formatDate(point.start, timezone)
          return (
            <div
              key={point.start}
              className="bar-col"
              onMouseEnter={() => setHovered(index)}
              onMouseLeave={() => setHovered(null)}
              onFocus={() => setHovered(index)}
              onBlur={() => setHovered(null)}
              tabIndex={0}
              aria-label={`${day}: ${symbol}${formatMoney(point.cost)}`}
            >
              <div className="bar-track">
                <div
                  className={hovered === index ? 'bar is-active' : 'bar'}
                  style={{ height: `${percent}%` }}
                />
              </div>
              {hovered === index && (
                <div className="chart-tooltip" style={{ bottom: `calc(${percent}% + 10px)` }}>
                  <span className="num">
                    {day.slice(5)} · {symbol}
                    {formatMoney(point.cost)}
                  </span>
                </div>
              )}
              <span className="bar-label num">{day.slice(5)}</span>
            </div>
          )
        })}
      </div>

      <details className="chart-data">
        <summary>{t('usage.dataTable')}</summary>
        <table>
          <thead>
            <tr>
              <th scope="col">{t('overview.time')}</th>
              <th scope="col">{t('usage.requests')}</th>
              <th scope="col">{t('usage.cost')}</th>
            </tr>
          </thead>
          <tbody>
            {points.map((point) => (
              <tr key={point.start}>
                <td>{formatDate(point.start, timezone)}</td>
                <td className="num">{point.requests}</td>
                <td className="num">
                  {symbol}
                  {formatMoney(point.cost)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </div>
  )
}
