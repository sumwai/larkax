export type Lang = 'zh' | 'en'

/** 语言偏好的存储键；Provider 写入与 `detectLang()` 读取共用同一个。 */
export const LANG_STORAGE_KEY = 'larkax.lang'

/**
 * 语言偏好的来源。
 *
 * 单独放一个模块，是因为顶层错误边界不能用 `useI18n()`——I18nProvider 自己
 * 可能就是抛错的那一个。边界退而读这里，与 Provider 共用同一份判定，不另写
 * 一份 localStorage / navigator 的判断。
 */
export function detectLang(): Lang {
  try {
    const saved = localStorage.getItem(LANG_STORAGE_KEY)
    if (saved === 'zh' || saved === 'en') return saved
  } catch {
    // localStorage 可能被禁用，退回浏览器语言。
  }
  const tag = typeof navigator === 'undefined' ? 'en' : navigator.language
  return tag.toLowerCase().startsWith('zh') ? 'zh' : 'en'
}
