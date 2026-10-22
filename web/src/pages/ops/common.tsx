import {
  cloneElement,
  isValidElement,
  useId,
  type ReactElement,
  type ReactNode,
} from 'react'
import { useI18n } from '../../i18n'
import { Alert, Button } from '../../components/ui'
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

/** 列表的搜索框：只负责输入与回调，查不查、什么时候查由调用方决定。 */
export function SearchBox({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const { t } = useI18n()
  return (
    <input
      className="input"
      type="search"
      value={value}
      onChange={(event) => onChange(event.target.value)}
      placeholder={t('ops.search')}
      aria-label={t('ops.search')}
      style={{ maxWidth: 280, marginBottom: 12 }}
    />
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
