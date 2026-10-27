import { useLayoutEffect } from 'react'

/**
 * 写浏览器标签页的标题。
 *
 * 由页面自己写；后缀也由各页自己拼，多数取 `meta.site_name`。其余地方不要
 * 再碰 `document.title`，否则又多了一份映射。
 */
export function useDocumentTitle(title: string) {
  useLayoutEffect(() => {
    document.title = title
  }, [title])
}
