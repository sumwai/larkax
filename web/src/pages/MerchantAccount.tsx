import { useI18n } from '../i18n'
import { PageHead } from '../components/ui'
import { AccountSections } from './Settings'

/**
 * 商家视图的「账号」页：账户信息与修改口令。
 *
 * 段落本身来自 `Settings` 的同一个导出——与用户、管理员的设置页共用一份，
 * 不在这里另写一遍表单。会话列表尚未建，这一页暂时只有这两块；退出登录
 * 仍只在顶栏账户菜单与移动端抽屉里，不在这里重复一份。
 */
export function MerchantAccount() {
  const { t } = useI18n()
  return (
    <>
      <PageHead title={t('nav.account')} subtitle={t('settings.accountSubtitle')} />
      <AccountSections />
    </>
  )
}
