/**
 * 规范名放进 URL 路径的编码。
 *
 * 规范名可以带 `/`（`vendor/model`），服务端那条路由是多段通配，所以按段
 * 编码、把斜杠保留下来。`encodeURI` 不转义 `?` 与 `#`：这两个字符在路径里
 * 会被当成查询串与片段的起点，页面会静默落到另一个地址上。
 */
export function encodeModelName(name: string): string {
  return name.split('/').map(encodeURIComponent).join('/')
}
