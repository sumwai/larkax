import { useState } from 'react'
import { isMockMode } from '../api'
import { useI18n } from '../i18n'
import { Banner, Button } from './ui'
import { X } from 'lucide-react'

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
    <Banner tone="warning" role="status">
      <span className="min-w-0 flex-1 truncate">{t('mock.banner')}</span>
      <Button
        variant="ghost"
        size="icon"
        className="size-6 text-warning-strong hover:bg-warning-border"
        aria-label={t('common.close')}
        onClick={() => setDismissed(true)}
      >
        <X size={15} />
      </Button>
    </Banner>
  )
}
