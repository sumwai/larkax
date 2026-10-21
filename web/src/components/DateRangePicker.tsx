import { useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import { addDays, diffDays, todayInZone } from '../lib/format'
import { IconCalendar, IconChevronLeft, IconChevronRight } from './icons'

const MAX_DAYS = 366

function monthOf(date: string): string {
  return `${date.slice(0, 7)}-01`
}

function addMonths(month: string, delta: number): string {
  const value = new Date(`${month}T00:00:00Z`)
  value.setUTCMonth(value.getUTCMonth() + delta)
  return value.toISOString().slice(0, 10)
}

interface DayCell {
  date: string
  inMonth: boolean
}

/** 周一开头，固定 6 行 42 格。 */
function monthGrid(month: string): DayCell[] {
  const first = new Date(`${month}T00:00:00Z`)
  const offset = (first.getUTCDay() + 6) % 7
  const start = addDays(month, -offset)
  return Array.from({ length: 42 }, (_, index) => {
    const date = addDays(start, index)
    return { date, inMonth: date.slice(0, 7) === month.slice(0, 7) }
  })
}

function monthLabel(month: string, lang: 'zh' | 'en'): string {
  const value = new Date(`${month}T00:00:00Z`)
  return new Intl.DateTimeFormat(lang === 'zh' ? 'zh-CN' : 'en-US', {
    year: 'numeric',
    month: 'long',
    timeZone: 'UTC',
  }).format(value)
}

export interface DateRangePickerProps {
  from: string
  to: string
  onChange: (from: string, to: string) => void
}

export function DateRangePicker({ from, to, onChange }: DateRangePickerProps) {
  const { t, lang } = useI18n()
  const { meta } = useSite()
  const [open, setOpen] = useState(false)
  const [month, setMonth] = useState(() => monthOf(from))
  const [pending, setPending] = useState<string | null>(null)
  const [message, setMessage] = useState<string | null>(null)
  const wrapRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    setMonth(monthOf(from))
  }, [from])

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(event.target as Node)) {
        setOpen(false)
        setPending(null)
      }
    }
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpen(false)
        setPending(null)
      }
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const today = todayInZone(meta.timezone)
  const dow = lang === 'zh' ? ['一', '二', '三', '四', '五', '六', '日'] : ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su']

  const select = (date: string) => {
    setMessage(null)
    if (pending === null) {
      setPending(date)
      return
    }
    const start = pending <= date ? pending : date
    const end = pending <= date ? date : pending
    if (diffDays(start, end) + 1 > MAX_DAYS) {
      setMessage(t('usage.rangeTooLong'))
      return
    }
    setPending(null)
    onChange(start, end)
    setOpen(false)
  }

  const setPreset = (days: number) => {
    const end = today
    const start = days === 1 ? today : addDays(today, -(days - 1))
    setPending(null)
    setMessage(null)
    onChange(start, end)
    setOpen(false)
  }

  const handleGridKeys = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    const delta =
      event.key === 'ArrowLeft'
        ? -1
        : event.key === 'ArrowRight'
          ? 1
          : event.key === 'ArrowUp'
            ? -7
            : event.key === 'ArrowDown'
              ? 7
              : 0
    if (delta === 0) return
    const active = document.activeElement
    if (!(active instanceof HTMLElement)) return
    const currentDate = active.dataset.date
    if (!currentDate) return
    event.preventDefault()
    const next = addDays(currentDate, delta)
    const target = document.querySelector<HTMLButtonElement>(`[data-date="${next}"]`)
    if (target) {
      target.focus()
      return
    }
    setMonth(monthOf(next))
    window.setTimeout(() => {
      document.querySelector<HTMLButtonElement>(`[data-date="${next}"]`)?.focus()
    }, 0)
  }

  const months = useMemo(() => [month, addMonths(month, 1)], [month])

  const rangeStart = pending ?? from
  const rangeEnd = pending ? pending : to

  return (
    <div className="wrap-rel range-wrap" ref={wrapRef}>
      <div className="daterange">
        <button
          type="button"
          className="dr"
          aria-haspopup="dialog"
          aria-expanded={open}
          aria-label={`${t('usage.from')}：${rangeStart}`}
          onClick={() => {
            setOpen((previous) => !previous)
            setPending(null)
          }}
        >
          <IconCalendar />
          {rangeStart}
        </button>
        <span className="sep" aria-hidden="true">
          –
        </span>
        <button
          type="button"
          className="dr"
          aria-label={`${t('usage.to')}：${rangeEnd}`}
          onClick={() => {
            setOpen((previous) => !previous)
            setPending(null)
          }}
        >
          {rangeEnd}
        </button>
      </div>

      {open && (
        <div className="popover date-pop" role="dialog" aria-label={t('usage.range')}>
          <div className="presets">
            <button type="button" className="chipbtn" onClick={() => setPreset(7)}>
              {t('usage.preset7')}
            </button>
            <button type="button" className="chipbtn" onClick={() => setPreset(30)}>
              {t('usage.preset30')}
            </button>
            <button type="button" className="chipbtn" onClick={() => setPreset(1)}>
              {t('usage.presetToday')}
            </button>
          </div>

          <div className="cal-pair">
            {months.map((value, index) => (
              <div key={value} className={index === 1 ? 'second' : undefined}>
                <div className="cal-head">
                  <button
                    type="button"
                    className="cal-nav"
                    aria-label={t('usage.prevMonth')}
                    onClick={() => setMonth(addMonths(month, -1))}
                  >
                    <IconChevronLeft size={15} />
                  </button>
                  <span className="cal-title">{monthLabel(value, lang)}</span>
                  <button
                    type="button"
                    className="cal-nav"
                    aria-label={t('usage.nextMonth')}
                    onClick={() => setMonth(addMonths(month, 1))}
                  >
                    <IconChevronRight size={15} />
                  </button>
                </div>
                <div className="cal-grid">
                  {dow.map((label) => (
                    <span key={label} className="cal-dow">
                      {label}
                    </span>
                  ))}
                </div>
                <div className="cal-grid" onKeyDown={handleGridKeys}>
                  {monthGrid(value).map((cell) => {
                    const classes = ['cal-day']
                    if (!cell.inMonth) classes.push('out')
                    if (cell.date === today) classes.push('today')
                    const inRange =
                      rangeEnd > rangeStart && cell.date > rangeStart && cell.date < rangeEnd
                    if (inRange) classes.push('in-range')
                    if (cell.date === rangeStart) classes.push('start')
                    if (cell.date === rangeEnd) classes.push('end')
                    return (
                      <button
                        key={cell.date}
                        type="button"
                        data-date={cell.date}
                        className={classes.join(' ')}
                        aria-pressed={cell.date === rangeStart || cell.date === rangeEnd}
                        onClick={() => {
                          setMonth(monthOf(cell.date))
                          select(cell.date)
                        }}
                      >
                        {Number(cell.date.slice(8))}
                      </button>
                    )
                  })}
                </div>
              </div>
            ))}
          </div>

          {message && <div className="hint error" style={{ marginTop: 8 }}>{message}</div>}

          <div className="cal-foot">
            <span className="tiny">{`${rangeStart} – ${rangeEnd}`}</span>
            <div className="row" style={{ gap: 6 }}>
              <button
                type="button"
                className="btn ghost sm"
                onClick={() => {
                  setPending(null)
                  setMessage(null)
                  onChange(today, today)
                }}
              >
                {t('common.today')}
              </button>
              <button
                type="button"
                className="btn ghost sm"
                onClick={() => {
                  setPending(null)
                  setMessage(null)
                  onChange(today, today)
                  setOpen(false)
                }}
              >
                {t('common.clear')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
