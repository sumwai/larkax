import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Check, Copy } from 'lucide-react'
import { cn } from './lib'
import { Button } from './button'

/** 骨架：形状跟页面真实元素，不用一排灰条。读屏只听到「加载中」。 */
export function Skeleton({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      aria-hidden="true"
      className={cn('animate-pulse rounded-xs bg-muted', className)}
      {...props}
    />
  )
}

export function Spinner({ className }: { className?: string }) {
  return (
    <span
      role="status"
      aria-label="加载中"
      className={cn(
        'inline-block size-4 animate-spin-slow rounded-full border-2 border-current border-r-transparent',
        className,
      )}
    />
  )
}

/**
 * 空态的两种：真的没有东西（empty）与筛选没匹配（no-match）。
 * 下一步动作不一样：前者引导创建，后者引导清筛选。
 */
export function Empty({
  kind = 'empty',
  title,
  description,
  action,
  className,
}: {
  kind?: 'empty' | 'no-match'
  title?: ReactNode
  description?: ReactNode
  action?: ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center gap-2 rounded-md border border-dashed border-border-strong px-6 py-12 text-center',
        className,
      )}
    >
      <p className="text-md font-semibold">{title}</p>
      {description ? <p className="max-w-sm text-sm text-muted-foreground">{description}</p> : null}
      {action ? <div className="mt-2">{action}</div> : null}
      <span className="sr-only">{kind === 'no-match' ? '没有匹配的结果' : '暂无数据'}</span>
    </div>
  )
}

/** 进度：额度占用、上游余额。不知道百分比时传 indeterminate。 */
export function Progress({
  value,
  indeterminate,
  tone = 'neutral',
  className,
  label,
}: {
  /** 0–100。 */
  value?: number
  indeterminate?: boolean
  tone?: 'neutral' | 'success' | 'warning' | 'danger'
  className?: string
  label?: string
}) {
  const fill =
    tone === 'danger'
      ? 'bg-danger'
      : tone === 'warning'
        ? 'bg-warning'
        : tone === 'success'
          ? 'bg-success'
          : 'bg-primary'
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={indeterminate ? undefined : 0}
      aria-valuemax={indeterminate ? undefined : 100}
      aria-valuenow={indeterminate ? undefined : Math.round(value ?? 0)}
      className={cn('h-2 w-full overflow-hidden rounded-full bg-muted', className)}
    >
      <div
        className={cn('h-full rounded-full', fill, indeterminate && 'w-1/3 animate-pulse')}
        style={indeterminate ? undefined : { width: `${Math.min(100, Math.max(0, value ?? 0))}%` }}
      />
    </div>
  )
}

/** 环形进度：指标卡上的额度占用。 */
export function Ring({
  value,
  size = 44,
  label,
}: {
  /** 0–100。 */
  value: number
  size?: number
  label?: string
}) {
  const stroke = 4
  const r = (size - stroke) / 2
  const c = 2 * Math.PI * r
  const clamped = Math.min(100, Math.max(0, value))
  return (
    <div className="relative inline-grid place-items-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} aria-hidden="true" className="-rotate-90">
        <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="var(--muted)" strokeWidth={stroke} />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={r}
          fill="none"
          stroke="var(--progress-fill)"
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={c}
          strokeDashoffset={c * (1 - clamped / 100)}
        />
      </svg>
      <span className="absolute text-2xs font-semibold num" aria-hidden="true">
        {Math.round(clamped)}%
      </span>
      {label ? <span className="sr-only">{label}</span> : null}
    </div>
  )
}

/** 键帽：界面上出现快捷键时用它。 */
export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd
      className="inline-flex h-5 min-w-5 items-center justify-center rounded-[5px] border border-kbd-border bg-kbd-bg px-1 font-mono text-2xs font-medium text-kbd-fg"
      style={{ height: 'var(--kbd-h)' }}
    >
      {children}
    </kbd>
  )
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

/** 复制按钮：成功打勾自复位。文案由调用方给，默认沿用中文。 */
export function CopyButton({
  value,
  className,
  label,
  copiedLabel,
}: {
  value: string
  className?: string
  label?: string
  copiedLabel?: string
}) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const idle = label ?? '复制'
  const done = copiedLabel ?? '已复制'
  return (
    <Button
      variant="ghost"
      size="sm"
      className={className}
      aria-label={idle}
      onClick={async () => {
        if (await copyText(value)) {
          setCopied(true)
          timer.current = window.setTimeout(() => setCopied(false), 1500)
        }
      }}
    >
      {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
      {copied ? done : idle}
    </Button>
  )
}

/**
 * 代码块：语言 tab + 复制，行数不折行。快速开始的几套示例共用它。
 */
export function CodeBlock({
  code,
  languages,
  activeLang,
  onLangChange,
  className,
}: {
  code: string
  /** 有多套示例时给语言 tab；单套就省略。 */
  languages?: string[]
  activeLang?: string
  onLangChange?: (lang: string) => void
  className?: string
}) {
  return (
    <div className={cn('overflow-hidden rounded-md bg-code-bg text-code-fg', className)}>
      <div className="flex items-center gap-1 border-b border-white/10 px-2 py-1.5">
        {languages?.map((lang) => (
          <button
            key={lang}
            type="button"
            role="tab"
            aria-selected={lang === activeLang}
            onClick={() => onLangChange?.(lang)}
            className={cn(
              'rounded-xs px-2 py-1 font-mono text-xs transition-colors',
              lang === activeLang
                ? 'bg-white/15 text-code-fg'
                : 'text-code-muted hover:text-code-fg',
            )}
          >
            {lang}
          </button>
        ))}
        <span className="flex-1" />
        <CopyButton
          value={code}
          className="text-code-muted hover:bg-white/10 hover:text-code-fg"
        />
      </div>
      <pre className="overflow-x-auto p-4 font-mono text-xs leading-5">
        <code>{code}</code>
      </pre>
    </div>
  )
}
