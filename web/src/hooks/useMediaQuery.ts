import { useEffect, useState } from 'react'

/** 与 CSS 断点保持同一组数值：520 与 760。 */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false
    return window.matchMedia(query).matches
  })

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const list = window.matchMedia(query)
    const onChange = () => setMatches(list.matches)
    onChange()
    if (typeof list.addEventListener === 'function') {
      list.addEventListener('change', onChange)
      return () => list.removeEventListener('change', onChange)
    }
    // 老 Safari 只有 addListener。
    list.addListener(onChange)
    return () => list.removeListener(onChange)
  }, [query])

  return matches
}

export function useIsNarrow(): boolean {
  return useMediaQuery('(max-width: 759.98px)')
}

/**
 * 与 `components.css` 里收掉桌面表格的断点一致：清单该换成卡片列表了。
 *
 * 单独一个 720 的谓词，是因为 `useIsNarrow` 的 760 是给侧栏用的——两者相差
 * 40px，混用会让 720~760 之间出现「表格已隐藏、卡片还没出现」的空清单。
 */
export function useIsCompact(): boolean {
  return useMediaQuery('(max-width: 720px)')
}

export function useIsMobile(): boolean {
  return useMediaQuery('(max-width: 519.98px)')
}
