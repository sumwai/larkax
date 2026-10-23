import {
  cloneElement,
  isValidElement,
  useId,
  useRef,
  type ReactElement,
  type ReactNode,
} from 'react'
import { useI18n } from '../../i18n'
import { Alert, Button } from '../../components/ui'
import { IconSearch, IconX } from '../../components/icons'
import { exprPerMillion } from '../../components/ExprEditor'
import { describeError } from './common-helpers'

/**
 * 一行表单字段：标签与控件用 id 关联起来。
 *
 * 关联不是为了好看——没有它，读屏软件念不出这一栏叫什么，按标签找控件也找不到。
 */
export function Field({
  label,
  hint,
  children,
}: {
  label: ReactNode
  hint?: ReactNode
  children: ReactNode
}) {
  const generated = useId()
  const ownId =
    isValidElement(children) && typeof children.props.id === 'string' ? children.props.id : null
  const id = ownId ?? generated
  const control =
    ownId === null && isValidElement(children)
      ? cloneElement(children as ReactElement<{ id?: string }>, { id })
      : children

  return (
    <div className="field">
      <label htmlFor={isValidElement(children) ? id : undefined}>{label}</label>
      {control}
      {hint && <span className="hint">{hint}</span>}
    </div>
  )
}

/**
 * 列表的搜索框：只负责输入与回调，查不查、什么时候查由调用方决定。
 *
 * 清空是自绘的 `×`，不用 `type="search"` 的原生清除控件：那个控件在 Chromium
 * 与 Safari 上外观、位置都不一样，也没法写样式。原生那个在 CSS 里关掉
 * （`.search-box` 那段），否则会出现两个清除钮。
 *
 * 清空只把值设为空串，再额外触发一次查询是调用方的事（配上防抖的页面自己
 * 会去发请求）。焦点跟回输入框：点完按钮它自己会被卸载，焦点落到 `<body>`
 * 上，接着打字就落空了。
 */
export function SearchBox({
  value,
  onChange,
  id,
  placeholder,
  icon = false,
  className,
}: {
  value: string
  onChange: (value: string) => void
  id?: string
  /** 不给时用通用的「搜索」。列表页有更具体的位置时传自己的文案。 */
  placeholder?: string
  /** 左侧的放大镜。列表页原本就有，运营页没有，所以默认关。 */
  icon?: boolean
  /** 调用方自己的类名：尺寸这类页面级约束挂在它上面，别改默认的那几个。 */
  className?: string
}) {
  const { t } = useI18n()
  const inputRef = useRef<HTMLInputElement>(null)
  const label = placeholder ?? t('ops.search')
  return (
    <div
      className={['wrap-rel search-box', icon ? 'has-icon' : '', className ?? '']
        .filter(Boolean)
        .join(' ')}
      data-testid="search-box"
    >
      {icon && (
        <span className="search-box-icon" aria-hidden="true">
          <IconSearch size={15} />
        </span>
      )}
      <input
        ref={inputRef}
        id={id}
        className="input"
        type="search"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={label}
        aria-label={label}
      />
      {value !== '' && (
        <button
          type="button"
          className="search-clear"
          aria-label={t('ops.searchClear')}
          onClick={() => {
            onChange('')
            inputRef.current?.focus()
          }}
        >
          <IconX size={14} />
        </button>
      )}
    </div>
  )
}

/** 页面级的加载/空态/错误态判据，放在一处避免各页写得不一样。 */
export function OpsLoadError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const { t } = useI18n()
  // 越权与「不是你的」由 describeError 换成本地文案；其余 4xx 仍原样显示。
  const { message } = describeError(error, t, 'ops.loadFailed')
  return (
    <Alert tone="bad" title={message}>
      <Button variant="secondary" size="sm" onClick={onRetry} style={{ marginTop: 8 }}>
        {t('common.retry')}
      </Button>
    </Alert>
  )
}

/**
 * 表单底部的错误句。
 *
 * 容器一直在、只换文本，而不是「有错才插进来」：`role="alert"` 的播报发生在
 * 已经挂载的元素内容变化时，随内容一起插入的元素在部分读屏软件里会被漏掉。
 * 没有句子时容器是空的，靠 `components.css` 里的 `.hint.error:empty` 把它从
 * 布局里摘出去——父容器多用 `gap` 排版，空元素也会多占一档间距。
 */
export function InputError({ message }: { message: string | null }) {
  return (
    <span className="hint error" role="alert">
      {message}
    </span>
  )
}

/**
 * 计价规则与成本规则清单里 `expr` 那一列。
 *
 * 能换算成一份固定单价表时显示每 100 万 token 的金额；带条件、带别的键的表达式
 * 换算不出来，按原文显示，并在旁边挂一个「原文」角标。两种行同处一列，数字差六个
 * 数量级，不标出来读者会以为原文那一行也是每百万的金额。
 *
 * 角标放在截断之外：`cell-ellipsis` 会把长表达式截掉，角标若跟着一起被截，
 * 越是长到需要标记的行越看不见标记。整段表达式挂在 `title` 上。
 */
export function PriceCell({ expr }: { expr: string }) {
  const { t } = useI18n()
  const perMillion = exprPerMillion(expr)
  if (perMillion !== null) {
    return (
      <span className="mono tiny cell-ellipsis" title={perMillion}>
        {perMillion}
      </span>
    )
  }
  return (
    <span className="expr-cell">
      <span className="mono tiny cell-ellipsis" title={expr}>
        {expr}
      </span>
      <span className="tag" title={t('ops.pricing.exprVerbatimHint')}>
        {t('ops.pricing.exprVerbatim')}
      </span>
    </span>
  )
}
