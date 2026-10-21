import {
  cloneElement,
  isValidElement,
  useId,
  useState,
  type ReactElement,
  type ReactNode,
} from 'react'
import { ApiError } from '../../api'
import { useI18n } from '../../i18n'
import { Alert, Button, Modal } from '../../components/ui'

/**
 * 把一次失败分成「参数不对」与「服务端故障」两类。
 *
 * 4xx 的正文由服务端给（校验失败、越权、别人家的渠道），原样显示；5xx 与
 * 网络错误转成一句我们自己的话——那种错误的服务端文案通常是给运维看的。
 */
export function describeError(
  error: unknown,
  fallback: string,
): { input: boolean; message: string } {
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) {
    return { input: true, message: error.message }
  }
  return { input: false, message: fallback }
}

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

/** 列表字段统一用「每行一项」，逗号也认——两种写法都常见。 */
export function linesToList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

export function listToLines(items: string[] | null | undefined): string {
  return (items ?? []).join('\n')
}

interface ConfirmConfig {
  title: string
  body: ReactNode
  label: string
  /** 删除、停用这类动作的确认钮用危险色，别和「保存」长一个样。 */
  danger?: boolean
  run: () => Promise<void>
}

/**
 * 写动作的确认步骤。
 *
 * 每个写动作都要先经过这里：直接点一下就把渠道停掉、把模型删掉太容易了。
 * 确认之后才发请求；失败留在同一个弹窗里，不把用户刚填的表单一起关掉。
 */
export function useConfirm() {
  const { t } = useI18n()
  const [config, setConfig] = useState<ConfirmConfig | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const request = (next: ConfirmConfig) => {
    setError(null)
    setConfig(next)
  }

  const close = () => {
    if (pending) return
    setConfig(null)
    setError(null)
  }

  const confirm = async () => {
    if (!config) return
    setPending(true)
    setError(null)
    try {
      await config.run()
      setConfig(null)
    } catch (failure) {
      setError(describeError(failure, t('ops.serverError')).message)
    } finally {
      setPending(false)
    }
  }

  const dialog = config ? (
    <Modal
      title={config.title}
      onClose={close}
      footer={
        <>
          <Button variant="secondary" onClick={close} disabled={pending}>
            {t('common.cancel')}
          </Button>
          <Button onClick={confirm} loading={pending} variant={config.danger ? 'danger' : 'primary'}>
            {config.label}
          </Button>
        </>
      }
    >
      <p style={{ margin: '0 0 12px', color: 'var(--muted)' }}>{config.body}</p>
      {error && <Alert tone="bad" title={error} />}
    </Modal>
  ) : null

  return { request, dialog, pending }
}

/** 页面级的加载/空态/错误态判据，放在一处避免各页写得不一样。 */
export function OpsLoadError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const { t } = useI18n()
  // 越权与「不是你的」都属于 4xx：正文是服务端那句话，原样显示。
  const { message } = describeError(error, t('ops.loadFailed'))
  return (
    <Alert tone="bad" title={message}>
      <Button variant="secondary" size="sm" onClick={onRetry} style={{ marginTop: 8 }}>
        {t('common.retry')}
      </Button>
    </Alert>
  )
}

export function InputError({ message }: { message: string | null }) {
  if (!message) return null
  return <span className="hint error">{message}</span>
}
