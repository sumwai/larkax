/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** `mock`（默认）或 `http`。见 README 的环境变量一节。 */
  readonly VITE_API_MODE?: string
  /** API 前缀，默认 `/api/v1`。 */
  readonly VITE_API_BASE?: string
  /** 开发服务器的代理目标，仅 vite.config.ts 读取。 */
  readonly VITE_API_PROXY?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
