import { useState } from 'react'
import { isMockMode } from '../api'
import { useI18n } from '../i18n'
import { IconInfo, IconX } from './icons'

/**
 * 演示数据横幅。
 *
 * 只在 mock 模式下出现；真实模式下组件直接返回 null，不参与布局。关闭状态
 * 只留在本次渲染里：刷新后重新提示是合理的——那时用户又可能忘了自己在看
 * 演示数据。判据与 `api` 的选型同源（`isMockMode`），不另写一份环境判断。
 */
export function MockBanner() {
  const { t } = useI18n()
  const [dismissed, setDismissed] = useState(false)
  if (!isMockMode || dismissed) return null

  return (
    <div className="alert warn mock-banner" role="status">
      <IconInfo className="ico" size={18} />
      <span className="mock-banner-text">{t('mock.banner')}</span>
      <button
        type="button"
        className="mock-banner-close"
        aria-label={t('common.close')}
        onClick={() => setDismissed(true)}
      >
        <IconX size={15} />
      </button>
    </div>
  )
}
