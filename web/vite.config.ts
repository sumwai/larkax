import { loadEnv } from 'vite'
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

/**
 * 从网关的监听地址里取端口。
 *
 * 这个变量写的是 `host:port`，主机部分可能省略（`:8081`）、可能是 IPv4 或
 * IPv6。解析不出来时退回默认端口——代理目标写错，开发服务器还能起来看报错，
 * 比整个 dev server 起不来好排查。
 */
function portFromAddr(addr: string | undefined, fallback: number): number {
  const matched = addr?.trim().match(/(?:^|:)(\d{1,5})$/)
  const port = matched ? Number(matched[1]) : Number.NaN
  return Number.isInteger(port) && port > 0 && port < 65536 ? port : fallback
}

// 前端与 API 同源；开发时把 /api 转给本机网关。
export default defineConfig(({ mode, command }) => {
  const env = loadEnv(mode, '.', 'VITE_')
  // 网关的监听地址通常写在仓库根的 .env 里（web/ 的上一层）。这里只读它，
  // 不带前缀的变量不会进浏览器包；`loadEnv` 让环境变量覆盖文件里的值。
  const rootEnv = loadEnv(mode, '..', '')
  const target =
    env.VITE_API_PROXY ||
    `http://127.0.0.1:${portFromAddr(rootEnv.LARKAX_HTTP_ADDR, 8080)}`

  if (command === 'serve') {
    console.log(`[vite] /api -> ${target}`)
  }

  return {
    plugins: [react()],
    server: {
      port: 5173,
      proxy: {
        '/api': {
          target,
          changeOrigin: false,
        },
      },
    },
    build: {
      outDir: 'dist',
      emptyOutDir: true,
    },
    test: {
      environment: 'jsdom',
      setupFiles: './src/test/setup.ts',
      css: false,
      include: ['src/**/*.test.{ts,tsx}'],
    },
  }
})
