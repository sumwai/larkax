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

export function useIsMobile(): boolean {
  return useMediaQuery('(max-width: 519.98px)')
}
