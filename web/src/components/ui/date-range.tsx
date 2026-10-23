import { useState } from 'react'
import { type DateRange as DateRangeValue } from 'react-day-picker'
import { formatDate } from '../../lib/format'
import * as PopoverPrimitive from '@radix-ui/react-popover'
import { DayPicker } from 'react-day-picker'
import { CalendarDays } from 'lucide-react'
import { cn } from './lib'
import { Button } from './button'
import { zhCN } from 'react-day-picker/locale'

/**
 * 日期区间：预设在上、双月在下的顺序；底部写出当前选中的区间与时区。
 * 时区由调用方给（用量数据的聚合口径），这里只负责展示，不做换算。
 */
export interface DateRangePickerProps {
  value: DateRangeValue | undefined
  onChange: (value: DateRangeValue | undefined) => void
  /** 预设区间，按天算。 */
  presets?: { label: string; days: number }[]
  timeZone?: string
  className?: string
}

const defaultPresets = [
  { label: '近 7 天', days: 7 },
  { label: '近 30 天', days: 30 },
  { label: '近 90 天', days: 90 },
]

function fmt(date: Date, timeZone?: string): string {
  return formatDate(date.toISOString(), timeZone)
}

export function DateRangePicker({
  value,
  onChange,
  presets = defaultPresets,
  timeZone,
  className,
}: DateRangePickerProps) {
  const [open, setOpen] = useState(false)
  const today = new Date()
  const label =
    value?.from && value?.to
      ? `${fmt(value.from, timeZone)} – ${fmt(value.to, timeZone)}`
      : value?.from
        ? `${fmt(value.from, timeZone)} – …`
        : '选择区间'

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={setOpen}>
      <PopoverPrimitive.Trigger
        className={cn(
          'inline-flex h-9 items-center gap-2 rounded-sm border border-input bg-card px-3 text-sm outline-none transition-colors focus-visible:outline-2 focus-visible:outline-offset-0 focus-visible:outline-ring',
          className,
        )}
      >
        <CalendarDays className="size-4 text-faint" aria-hidden="true" />
        <span className="num">{label}</span>
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Portal>
        <PopoverPrimitive.Content
          sideOffset={4}
          align="start"
          className="z-dropdown w-auto rounded-md border border-border bg-popover p-3 shadow-md"
        >
          <div className="mb-2 flex flex-wrap items-center gap-1">
            {presets.map((preset) => (
              <Button
                key={preset.label}
                variant="secondary"
                size="sm"
                onClick={() => {
                  onChange({
                    from: new Date(today.getFullYear(), today.getMonth(), today.getDate() - preset.days + 1),
                    to: today,
                  })
                }}
              >
                {preset.label}
              </Button>
            ))}
          </div>
          <DayPicker
            mode="range"
            selected={value}
            onSelect={onChange}
            numberOfMonths={2}
            locale={zhCN}
            disabled={{ after: today }}
            classNames={{
              months: 'flex flex-col sm:flex-row gap-3',
              month_caption: 'text-sm font-semibold px-1 pb-1',
              nav: 'flex items-center gap-1',
              weekday: 'text-2xs text-faint w-8',
              day: 'size-8 text-center text-sm',
              day_button: 'size-8 rounded-xs hover:bg-muted',
              selected: '[&>button]:bg-control-active-bg [&>button]:text-control-active-fg',
              range_start: '[&>button]:rounded-r-none',
              range_end: '[&>button]:rounded-l-none',
              range_middle: '[&>button]:bg-muted [&>button]:text-foreground',
              today: '[&>button]:underline',
            }}
          />
          <div className="mt-2 border-t border-border pt-2 text-xs text-faint">
            {value?.from && value?.to ? (
              <>
                当前区间：{fmt(value.from, timeZone)} – {fmt(value.to, timeZone)}
                {timeZone ? `（${timeZone}）` : ''}
              </>
            ) : (
              '尚未选择区间'
            )}
          </div>
        </PopoverPrimitive.Content>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  )
}
