import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ReactNode,
  type RefObject,
} from 'react'
import type { MessageKey } from '../i18n/zh'
import { useI18n } from '../i18n'
import { useIsMobile } from '../hooks/useMediaQuery'
import { formatBalance, formatMoney, formatSpend, moneySymbol } from '../lib/format'
import type { Money } from '../api/types'
import { IconAlert, IconCheck, IconCopy, IconChevronDown, IconInfo, IconX } from './icons'

/* ── 按钮 ─────────────────────────────────────────────────────────── */

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  size?: 'md' | 'sm' | 'icon'
  loading?: boolean
  icon?: ReactNode
}

export function Button({
  variant = 'primary',
  size = 'md',
  loading = false,
  icon,
  className,
  children,
  disabled,
  ...rest
}: ButtonProps) {
  const classes = [
    'btn',
    variant === 'primary' ? '' : variant,
    size === 'sm' ? 'sm' : '',
    size === 'icon' ? 'icon' : '',
    className ?? '',
  ]
    .filter(Boolean)
    .join(' ')
  return (
    <button className={classes} disabled={disabled || loading} {...rest}>
      {loading ? <span className="spin" aria-hidden="true" /> : icon}
      {children}
    </button>
  )
}

/* ── 状态 ─────────────────────────────────────────────────────────── */

type Tone = 'ok' | 'neutral' | 'warn' | 'bad' | 'accent'

export function Pill({ tone = 'neutral', dot = false, children }: { tone?: Tone; dot?: boolean; children: ReactNode }) {
  return (
    <span className={`pill ${tone}`}>
      {dot && <span className="dot" aria-hidden="true" />}
      {children}
    </span>
  )
}

const STATUS: Record<string, { tone: Tone; key: MessageKey }> = {
  active: { tone: 'ok', key: 'status.active' },
  // 渠道的启停用 `enabled` / `disabled` 两个值；少了这一条，启用的渠道会把
  // 原始英文值直接显示出来。
  enabled: { tone: 'ok', key: 'status.enabled' },
  disabled: { tone: 'neutral', key: 'status.disabled' },
  revoked: { tone: 'bad', key: 'status.revoked' },
  pending: { tone: 'warn', key: 'status.pending' },
  ok: { tone: 'ok', key: 'status.ok' },
  settled: { tone: 'ok', key: 'status.settled' },
  released: { tone: 'neutral', key: 'status.released' },
  refunded: { tone: 'accent', key: 'status.refunded' },
  failed: { tone: 'bad', key: 'status.failed' },
  measured: { tone: 'accent', key: 'status.measured' },
  estimated: { tone: 'warn', key: 'status.estimated' },
  fallback: { tone: 'neutral', key: 'status.fallback' },
}

export function StatusPill({ status }: { status: string }) {
  const { t } = useI18n()
  const meta = STATUS[status]
  return (
    <Pill tone={meta?.tone ?? 'neutral'} dot>
      {meta ? t(meta.key) : status}
    </Pill>
  )
}

/* ── 金额 ─────────────────────────────────────────────────────────── */

export function MoneyText({
  value,
  currency,
  variant,
}: {
  value: Money | null | undefined
  currency?: string
  /** `balance` 是余额（2 位截断），`spend` 是消费（3 位截断）；不传按价格口径。 */
  variant?: 'balance' | 'spend'
}) {
  const symbol = moneySymbol(currency)
  const text = variant === 'balance' ? formatBalance(value) : variant === 'spend' ? formatSpend(value) : formatMoney(value)
  // `<0.001` / `>-0.001` 里的比较符要落在币种符号之前，否则读成「$ 小于」。
  const marker = text.startsWith('<') || text.startsWith('>') ? text[0] : ''
  const amount = marker ? text.slice(1) : text
  return (
    <>
      {marker}
      {symbol && <small>{symbol}</small>}
      {amount}
    </>
  )
}

/* ── 反馈 ─────────────────────────────────────────────────────────── */

type AlertTone = 'info' | 'ok' | 'warn' | 'bad'

export function Alert({ tone = 'info', title, children }: { tone?: AlertTone; title?: ReactNode; children?: ReactNode }) {
  const Glyph = tone === 'bad' || tone === 'warn' ? IconAlert : tone === 'ok' ? IconCheck : IconInfo
  return (
    <div className={`alert ${tone}`} role={tone === 'bad' ? 'alert' : 'status'}>
      <Glyph className="ico" size={18} />
      <div>
        {title && <b>{title}</b>}
        {children && <p>{children}</p>}
      </div>
    </div>
  )
}

export function Empty({
  icon,
  title,
  body,
  action,
}: {
  icon?: ReactNode
  title: ReactNode
  body?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="empty">
      {icon && <div className="ico">{icon}</div>}
      <h3>{title}</h3>
      {body && <p>{body}</p>}
      {action}
    </div>
  )
}

export function Skeleton({ width = '100%', height = 12 }: { width?: number | string; height?: number | string }) {
  return <span className="sk" style={{ display: 'block', width, height }} aria-hidden="true" />
}

/* ── 浮层 ─────────────────────────────────────────────────────────── */

/**
 * bottom sheet 的三档高度上限，单位是视口高度的百分比。
 *
 * 只有这一处写数值：改档位时改这里，弹窗与用例都跟着变。档位是**上限**，
 * 内容少的弹窗按内容高，不留空白。
 */
export const SHEET_SIZES = { sm: 30, md: 50, lg: 80 } as const

export type SheetSize = keyof typeof SHEET_SIZES

/** 往下拖过这个距离才关，免得手抖碰一下就把正在填的表单关掉。 */
const SHEET_DISMISS_PX = 96

/**
 * 弹层里能接住 Tab 的元素。`[hidden]` 里的（比如 Tabs 未选中的面板）与
 * `inert` 子树要排掉：它们拿不到焦点，算进来会让循环落在一个看不见的元素上。
 */
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableIn(root: HTMLElement): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)].filter(
    (element) => !element.closest('[hidden], [inert]'),
  )
}

export function Modal({
  title,
  description,
  onClose,
  children,
  footer,
  sheet = false,
  sheetSize = 'md',
  focusTarget,
  focusToken,
}: {
  title: ReactNode
  description?: ReactNode
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  sheet?: boolean
  /** 高度档位；窄屏上所有弹窗都按它限高，桌面只有 `sheet` 弹窗用。 */
  sheetSize?: SheetSize
  /**
   * 打开之后焦点该落在谁身上。给一个 ref，或者一个「现在该聚焦哪个元素」的
   * 取值函数——后者用在弹窗内容自己会变的情形（比如校验失败时切到出错的那一组，
   * 目标元素这一刻才挂上）。取不到就退回聚焦弹窗本身。
   */
  focusTarget?: RefObject<HTMLElement | null> | (() => HTMLElement | null)
  /**
   * 这个值每变一次，就按 `focusTarget` 重新聚焦一次。打开时也会跑一次，
   * 所以不传 `focusTarget` 的弹窗行为与以前一样：焦点落在弹窗上。
   */
  focusToken?: unknown
}) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  // 目标可能在每次渲染时是新函数，放进 ref 里，effect 只认 token。
  const targetRef = useRef(focusTarget)
  targetRef.current = focusTarget
  const titleId = useId()
  const { t } = useI18n()
  // 窄屏上所有弹窗都从底部弹出（`.overlay` 的移动端样式本来就贴在底下），
  // 所以拖动把手与高度档位也一并给上，不只给显式声明 `sheet` 的那几个。
  const mobile = useIsMobile()
  const asSheet = sheet || mobile
  // 从上沿往下拖的位移。松手时超过阈值就关掉，否则弹回原位。
  const [dragY, setDragY] = useState(0)
  const dragStart = useRef<number | null>(null)
  const dragYRef = useRef(0)
  dragYRef.current = dragY

  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const bodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        closeRef.current()
        return
      }
      if (event.key !== 'Tab') return
      // `aria-modal` 只告诉读屏「背景不用读」，拦不住浏览器把焦点 Tab 到遮罩
      // 后面。首尾两个可聚焦元素处自己绕回来，焦点才真的留在面板里。
      const dialog = dialogRef.current
      if (!dialog) return
      const items = focusableIn(dialog)
      if (items.length === 0) {
        event.preventDefault()
        dialog.focus()
        return
      }
      const first = items[0]
      const last = items[items.length - 1]
      const active = document.activeElement
      const inside = active instanceof Node && dialog.contains(active)
      if (event.shiftKey && (!inside || active === first || active === dialog)) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && (!inside || active === last)) {
        event.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = bodyOverflow
      previous?.focus()
    }
  }, [])

  useEffect(() => {
    const target = targetRef.current
    const element = typeof target === 'function' ? target() : target?.current
    // 放在挂载之后：切组时目标元素与 token 在同一次提交里更新，effect 跑到时
    // 它已经在 DOM 里了。
    ;(element ?? dialogRef.current)?.focus()
  }, [focusToken])

  return (
    <div
      className={asSheet ? 'overlay sheet-overlay' : 'overlay'}
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose()
      }}
    >
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        ref={dialogRef}
        style={
          asSheet
            ? {
                maxHeight: `${SHEET_SIZES[sheetSize]}dvh`,
                transform: dragY > 0 ? `translateY(${dragY}px)` : undefined,
                transition: dragStart.current === null ? 'transform 0.16s ease' : undefined,
              }
            : undefined
        }
      >
        {/*
         * 拖动把手只给手指用：键盘与读屏用户有 Esc 和关闭钮，不依赖这条只有
         * 指针事件才能触发的路。`touch-action: none` 让纵向拖动不被页面滚动抢走。
         */}
        {asSheet && (
          <div
            className="sheet-grab"
            aria-hidden="true"
            onPointerDown={(event) => {
              dragStart.current = event.clientY
              dragYRef.current = 0
              setDragY(0)
              ;(event.currentTarget as HTMLElement).setPointerCapture?.(event.pointerId)
            }}
            onPointerMove={(event) => {
              if (dragStart.current === null) return
              setDragY(Math.max(0, event.clientY - dragStart.current))
            }}
            onPointerUp={() => {
              if (dragStart.current === null) return
              dragStart.current = null
              if (dragYRef.current >= SHEET_DISMISS_PX) closeRef.current()
              setDragY(0)
            }}
            // 指针被系统收走（来电、切到别的应用、系统手势）时不会有 pointerup：
            // 不复位的话弹层会停在一半的位置，过渡也一并丢掉。
            onPointerCancel={() => {
              dragStart.current = null
              setDragY(0)
            }}
          >
            <span />
          </div>
        )}
        <div className="m-head">
          <div>
            <h2 id={titleId}>{title}</h2>
            {description && <p>{description}</p>}
          </div>
          <button className="close" onClick={onClose} aria-label={t('common.close')}>
            <IconX size={16} />
          </button>
        </div>
        {children}
        {footer && <div className="form-actions">{footer}</div>}
      </div>
    </div>
  )
}

/* ── Toast ────────────────────────────────────────────────────────── */

const ToastContext = createContext<{ show: (message: string) => void } | null>(null)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState<string | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const show = useCallback((next: string) => {
    setMessage(next)
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => setMessage(null), 2600)
  }, [])

  useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current)
    }
  }, [])

  const value = useMemo(() => ({ show }), [show])
  return (
    <ToastContext.Provider value={value}>
      {children}
      {message && (
        <div className="toast-wrap" role="status" aria-live="polite">
          <div className="toast">
            <IconCheck size={16} />
            {message}
          </div>
        </div>
      )}
    </ToastContext.Provider>
  )
}

export function useToast() {
  const context = useContext(ToastContext)
  if (!context) throw new Error('useToast must be used inside ToastProvider')
  return context
}

/* ── 下拉 ─────────────────────────────────────────────────────────── */

export interface SelectOption<T extends string> {
  value: T
  label: string
}

export function Select<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
}: {
  value: T
  options: Array<SelectOption<T>>
  onChange: (value: T) => void
  ariaLabel: string
}) {
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(() => Math.max(0, options.findIndex((o) => o.value === value)))
  const wrapRef = useRef<HTMLDivElement>(null)
  const listId = useId()
  const current = options.find((option) => option.value === value) ?? options[0]

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    return () => document.removeEventListener('mousedown', onPointerDown)
  }, [open])

  const choose = (next: T) => {
    onChange(next)
    setOpen(false)
  }

  return (
    <div className="wrap-rel" ref={wrapRef}>
      <button
        type="button"
        className="selectbtn"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={ariaLabel}
        onClick={() => {
          setActive(Math.max(0, options.findIndex((option) => option.value === value)))
          setOpen((previous) => !previous)
        }}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault()
            if (!open) {
              setOpen(true)
              return
            }
            const delta = event.key === 'ArrowDown' ? 1 : -1
            setActive((index) => (index + delta + options.length) % options.length)
          } else if (event.key === 'Enter' && open) {
            event.preventDefault()
            choose(options[active].value)
          } else if (event.key === 'Escape') {
            setOpen(false)
          }
        }}
      >
        <span className="val">{current?.label}</span>
        <IconChevronDown className="chev" />
      </button>
      {open && (
        <div className="menu" role="listbox" id={listId} aria-label={ariaLabel}>
          {options.map((option, index) => (
            <button
              key={option.value}
              type="button"
              role="option"
              aria-selected={option.value === value}
              className={index === active ? 'menu-item is-active' : 'menu-item'}
              onMouseEnter={() => setActive(index)}
              onClick={() => choose(option.value)}
            >
              {option.label}
              {option.value === value && <IconCheck className="tick" size={14} />}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

/* ── 复制 ─────────────────────────────────────────────────────────── */

export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // 降级到 execCommand。
  }
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(area)
    return ok
  } catch {
    return false
  }
}

export function CopyButton({
  text,
  label,
  className = 'btn secondary sm',
}: {
  text: string
  label?: string
  className?: string
}) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current)
    }
  }, [])

  return (
    <button
      type="button"
      className={className}
      aria-label={label ?? t('common.copy')}
      onClick={async () => {
        const ok = await copyText(text)
        if (!ok) return
        setCopied(true)
        if (timer.current) clearTimeout(timer.current)
        timer.current = setTimeout(() => setCopied(false), 2000)
      }}
    >
      {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
      {copied ? t('common.copied') : (label ?? t('common.copy'))}
    </button>
  )
}

/* ── 页头 ─────────────────────────────────────────────────────────── */

export function PageHead({
  title,
  subtitle,
  actions,
}: {
  title: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {subtitle && <p>{subtitle}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  )
}
