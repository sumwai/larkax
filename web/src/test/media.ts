/**
 * 让 `useMediaQuery` 按一块给定尺寸的视口作答。
 *
 * jsdom 没有真实的布局，`matchMedia` 只是被调用方；用例要验窄屏分支，就得让
 * 查询真的随宽度与高度变。只认 `max-width` / `max-height` 两种形态——hooks
 * 里现在只用这两种，多解析一种等于给不存在的用法写代码。
 */
export function mockViewport(width: number, height: number): () => void {
  const previous = window.matchMedia
  window.matchMedia = ((query: string) => {
    const maxWidth = /max-width:\s*([\d.]+)px/.exec(query)
    const maxHeight = /max-height:\s*([\d.]+)px/.exec(query)
    const matches = maxWidth
      ? width <= Number(maxWidth[1])
      : maxHeight
        ? height <= Number(maxHeight[1])
        : false
    return {
      matches,
      media: query,
      onchange: null,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      addListener: () => undefined,
      removeListener: () => undefined,
      dispatchEvent: () => false,
    } as unknown as MediaQueryList
  }) as typeof window.matchMedia
  return () => {
    window.matchMedia = previous
  }
}
