import { useState, type ReactNode } from 'react'
import { ApiError } from '../../api'
import { useI18n } from '../../i18n'
import type { MessageKey } from '../../i18n/zh'
import { Alert, Button, Modal } from '../../components/ui'

/** 这几个状态码的正文是给调用方看的英文短句，换成我们自己的话。 */
const LOCALIZED_WORDS: Partial<Record<number, MessageKey>> = {
  401: 'ops.error.unauthorized',
  403: 'ops.error.forbidden',
  404: 'ops.error.notFound',
}

/**
 * 把一次失败分成「参数不对」与「服务端故障」两类。
 *
 * 其余的 4xx 正文由服务端给（校验失败），原样显示；5xx 与网络错误转成一句
 * 我们自己的话——那种错误的服务端文案通常是给运维看的。
 */
export function describeError(
  error: unknown,
  t: (key: MessageKey) => string,
  fallback: MessageKey,
): { input: boolean; message: string } {
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) {
    const word = LOCALIZED_WORDS[error.status]
    return { input: true, message: word ? t(word) : error.message }
  }
  return { input: false, message: t(fallback) }
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
      setError(describeError(failure, t, 'ops.serverError').message)
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

export type ConfirmHandle = ReturnType<typeof useConfirm>
