import { defineConfig } from '@playwright/test'

/**
 * 控制台的端到端配置。
 *
 * 服务不由这里拉起：网关与控制台跑在别处，连不上就该直接失败，而不是悄悄
 * 起一个空的开发服务器让用例对着 mock 通过。地址走 `E2E_BASE_URL`。
 *
 * 被指向的地址必须是 https：会话 Cookie 带 `__Host-` 前缀，浏览器只在安全
 * 来源上收它，http 下登录看起来成功、下一页就退回登录页。开发服务器要自己
 * 起在 TLS 上，并把 `/api` 转发到网关；转发时带上 `X-Forwarded-Proto` 与
 * `X-Forwarded-Host`，否则网关按连接推出来的「本站」是 http，登录会被按
 * 跨站请求拒掉（403）。
 */
export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  expect: { timeout: 8_000 },
  reporter: [['list']],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? 'https://localhost:3000',
    // 自签证书要放行；理由见文件开头的 https 那段。
    ignoreHTTPSErrors: true,
    // 断言按中文文案写：控制台会跟随浏览器语言。
    locale: 'zh-CN',
    headless: true,
    // 截图、录像与 trace 都会落成文件，有被误提交的风险；诊断靠断言输出。
    screenshot: 'off',
    video: 'off',
    trace: 'off',
  },
})
