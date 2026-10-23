import { useMemo, useState } from 'react'
import { cn } from './lib'

/**
 * 图表只自绘 SVG：坐标轴用 HTML 文字，不跟 SVG 缩放变形；
 * 颜色读 --chart-*。数据点是真按钮，可悬停、可 Tab 聚焦。
 */

export interface SeriesPoint {
  label: string
  value: number
}

/** 折线图：两条序列、若干天。图例在上，轴标在下。 */
export function LineChart({
  series,
  formatValue,
  className,
}: {
  /** 每条序列：名字、颜色 token 下标、数据点。 */
  series: { name: string; color: 1 | 2 | 3 | 4 | 5 | 6; points: SeriesPoint[] }[]
  formatValue?: (value: number) => string
  className?: string
}) {
  const [active, setActive] = useState<string | null>(null)
  const n = series[0]?.points.length ?? 0
  const max = useMemo(
    () => Math.max(1, ...series.flatMap((s) => s.points.map((p) => p.value))),
    [series],
  )
  const w = 100
  const h = 100
  const x = (i: number) => (n <= 1 ? w / 2 : (i / (n - 1)) * w)
  const y = (v: number) => h - (v / max) * (h - 8) - 4
  const first = series[0]?.points ?? []

  return (
    <div className={cn('w-full', className)}>
      <div className="mb-2 flex flex-wrap items-center gap-3">
        {series.map((s) => (
          <span key={s.name} className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
            <span
              aria-hidden="true"
              className={cn(
                'size-2 rounded-full',
                s.color === 1 && 'bg-chart-1',
                s.color === 2 && 'bg-chart-2',
                s.color === 3 && 'bg-chart-3',
                s.color === 4 && 'bg-chart-4',
                s.color === 5 && 'bg-chart-5',
                s.color === 6 && 'bg-chart-6',
              )}
            />
            {s.name}
          </span>
        ))}
      </div>
      <div className="relative h-40">
        <svg
          viewBox={`0 0 ${w} ${h}`}
          preserveAspectRatio="none"
          className="absolute inset-0 h-full w-full overflow-visible"
          aria-hidden="true"
        >
          {[0, 0.5, 1].map((t) => (
            <line
              key={t}
              x1={0}
              x2={w}
              y1={4 + t * (h - 8)}
              y2={4 + t * (h - 8)}
              stroke="var(--border)"
              strokeWidth={0.5}
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {series.map((s) => (
            <polyline
              key={s.name}
              points={s.points.map((p, i) => `${x(i)},${y(p.value)}`).join(' ')}
              fill="none"
              stroke={`var(--chart-${s.color})`}
              strokeWidth={1.5}
              vectorEffect="non-scaling-stroke"
              strokeLinejoin="round"
            />
          ))}
        </svg>
        {/* 数据点：真按钮，可悬停可聚焦。 */}
        <div className="absolute inset-0">
          {first.map((p, i) => {
            const values = series.map((s) => s.points[i]?.value ?? 0)
            const tip = `${p.label}：${series
              .map((s, si) => `${s.name} ${formatValue ? formatValue(values[si]) : values[si]}`)
              .join('，')}`
            return (
              <button
                key={p.label + i}
                type="button"
                aria-label={tip}
                onMouseEnter={() => setActive(p.label + i)}
                onMouseLeave={() => setActive(null)}
                onFocus={() => setActive(p.label + i)}
                onBlur={() => setActive(null)}
                className="absolute size-4 -translate-x-1/2 -translate-y-1/2 rounded-full outline-none focus-visible:ring-2 focus-visible:outline-ring"
                style={{ left: `${x(i)}%`, top: `${(y(p.value) / h) * 100}%` }}
              >
                <span
                  aria-hidden="true"
                  className={cn(
                    'absolute top-1/2 left-1/2 size-1.5 -translate-x-1/2 -translate-y-1/2 rounded-full bg-chart-4 transition-transform',
                    active === p.label + i && 'scale-150',
                  )}
                />
              </button>
            )
          })}
        </div>
        {active ? (
          <div className="pointer-events-none absolute top-0 right-0 rounded-xs bg-gray-900 px-2 py-1 text-2xs whitespace-nowrap text-white">
            {(() => {
              const idx = first.findIndex((p, j) => p.label + j === active)
              const point = first[idx]
              if (!point) return null
              return `${point.label}：${series
                .map((s) => `${s.name} ${formatValue ? formatValue(s.points[idx]?.value ?? 0) : s.points[idx]?.value}`)
                .join('，')}`
            })()}
          </div>
        ) : null}
      </div>
      <div className="mt-1 flex justify-between text-2xs text-faint">
        <span>{first[0]?.label}</span>
        <span>{first[first.length - 1]?.label}</span>
      </div>
    </div>
  )
}

/** 柱图：单序列。每根可悬停读值。 */
export function BarChart({
  points,
  formatValue,
  className,
}: {
  points: SeriesPoint[]
  formatValue?: (value: number) => string
  className?: string
}) {
  const [active, setActive] = useState<number | null>(null)
  const max = Math.max(1, ...points.map((p) => p.value))
  return (
    <div className={cn('w-full', className)}>
      <div className="flex h-40 items-end gap-1.5">
        {points.map((p, i) => (
          <button
            key={p.label}
            type="button"
            className="group relative flex h-full min-w-0 flex-1 cursor-default items-end rounded-xs outline-none focus-visible:outline-2 focus-visible:-outline-offset-4 focus-visible:outline-ring"
            aria-label={`${p.label}：${formatValue ? formatValue(p.value) : p.value}`}
            onMouseEnter={() => setActive(i)}
            onMouseLeave={() => setActive(null)}
            onFocus={() => setActive(i)}
            onBlur={() => setActive(null)}
          >
            <span
              aria-hidden="true"
              className={cn(
                'w-full rounded-xs bg-chart-1 transition-colors group-hover:bg-chart-4 group-focus-visible:bg-chart-4',
                active === i && 'bg-chart-4',
              )}
              style={{ height: `${Math.max(2, (p.value / max) * 100)}%` }}
            />
            {active === i ? (
              <span className="pointer-events-none absolute -top-7 left-1/2 -translate-x-1/2 rounded-xs bg-gray-900 px-1.5 py-0.5 text-2xs whitespace-nowrap text-white">
                {p.label} · {formatValue ? formatValue(p.value) : p.value}
              </span>
            ) : null}
          </button>
        ))}
      </div>
      <div className="mt-1 flex justify-between text-2xs text-faint">
        <span>{points[0]?.label}</span>
        <span>{points[points.length - 1]?.label}</span>
      </div>
    </div>
  )
}

/**
 * 迷你走势：指标卡右下角。只表达方向，不带坐标轴，不放数值。
 */
export function Sparkline({
  values,
  color = 1,
  className,
}: {
  values: number[]
  color?: 1 | 2 | 3 | 4 | 5 | 6
  className?: string
}) {
  const max = Math.max(1, ...values)
  const n = values.length
  const pts = values
    .map((v, i) => `${(i / Math.max(1, n - 1)) * 100},${28 - (v / max) * 24 - 2}`)
    .join(' ')
  return (
    <svg
      viewBox="0 0 100 28"
      preserveAspectRatio="none"
      className={cn('h-7 w-full', className)}
      aria-hidden="true"
    >
      <polyline
        points={pts}
        fill="none"
        stroke={`var(--chart-${color})`}
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
        strokeLinejoin="round"
      />
    </svg>
  )
}
