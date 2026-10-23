import { fileURLToPath } from 'node:url'
import { loadEnv, type ProxyOptions } from 'vite'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

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

/**
 * 开发代理的公共选项。
 *
 * 反代终结 TLS 后，后端要能算出「本站」是谁：登录的 CSRF 校验拿请求的 Origin
 * 与它比。少了这两个头，浏览器从 https 来、后端却按自己那条明文连接推成
 * `http://127.0.0.1:<port>`，登录会稳定 403。
 */
function proxyOptions(target: string): ProxyOptions {
  return {
    target,
    changeOrigin: false,
    configure: (proxy) => {
      proxy.on('proxyReq', (proxyReq, req) => {
        const host = req.headers.host
        if (host) proxyReq.setHeader('X-Forwarded-Host', host)
        // 反代把 TLS 终结在外面，这一跳自己看到的是明文。带域名进来的请求
        // 按 https 算，本机直连（localhost / 回环地址）按 http 算。
        const forwarded = req.headers['x-forwarded-proto']
        const proto =
          forwarded ?? (/^(localhost|127\.0\.0\.1)(:|$)/.test(host ?? '') ? 'http' : 'https')
        proxyReq.setHeader('X-Forwarded-Proto', proto)
      })
    },
  }
}

// 前端与 API 同源；开发时把 /api 与 /v1 转给本机网关。
export default defineConfig(({ mode, command }) => {
  const env = loadEnv(mode, '.', 'VITE_')
  // 网关的监听地址通常写在仓库根的 .env 里（web/ 的上一层）。这里只读它，
  // 不带前缀的变量不会进浏览器包；`loadEnv` 让环境变量覆盖文件里的值。
  const rootEnv = loadEnv(mode, '..', '')
  const target =
    env.VITE_API_PROXY || `http://127.0.0.1:${portFromAddr(rootEnv.LARKAX_HTTP_ADDR, 8080)}`

  if (command === 'serve') {
    console.log(`[vite] /api,/v1 -> ${target}`)
  }

  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: {
        // @lobehub/icons 的 exports 只声明了少数子路径，而图标都在 es/<名字>/components/ 下。
        // 用目录别名把这一层露出来，才能只引一个图标而不是整包（整包会拖进 @lobehub/ui）。
        '@lobehub-icons': fileURLToPath(
          new URL('./node_modules/@lobehub/icons/es', import.meta.url),
        ),
      },
    },
    server: {
      port: 5173,
      proxy: {
        // 控制台与推理接口在生产里同源：`/api/v1` 是控制台，`/v1` 是调模型的入口。
        // 开发时两条都转给本机网关，页面里用整把密钥调 `/v1` 才验得通。
        '/api': proxyOptions(target),
        '/v1': proxyOptions(target),
      },
      allowedHosts: ['larkax.dev'],
    },
    build: {
      outDir: 'dist',
      emptyOutDir: true,
      rollupOptions: {
        output: {
          // 路由分包：consumer / merchant / admin 三块各自成 chunk，
          // 避免单个入口把 19 个页面全部装进一个文件。
          manualChunks(id: string) {
            if (!id.includes('/src/pages/')) return undefined
            if (id.includes('/pages/ops/') || id.includes('/pages/admin/')) return 'ops'
            return 'app'
          },
        },
      },
    },
  }
})
