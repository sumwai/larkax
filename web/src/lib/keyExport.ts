/**
 * 把一把密钥导出成第三方客户端能直接导入的一段文本。
 *
 * 三种格式各自钉在对方的导入入口上：
 * - magpie：`magpie://import?…` 链接，参数见 magpie 的 internal/provider/importurl.go
 * - CC Switch：`ccswitch://v1/import?resource=provider&…`，参数见其文档 docs/user-manual/zh/5-faq/5.3-deeplink.md
 * - Lody：`lody://provider/import?v=1&data=` 后面接 base64url 的 provider JSON
 *
 * 三处都从同一个站点地址派生：`root` 是不带版本号的站点根，
 * `openai` 是它的 `/v1`。
 */

export interface KeySnippets {
  magpie: string
  ccSwitch: string
  lody: string
}

/** base64url（无补位）：链接里的 data 参数按这个编码解。 */
function base64url(text: string): string {
  const bytes = new TextEncoder().encode(text)
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function siteRoot(apiBase: string): string {
  return apiBase.replace(/\/+$/, '').replace(/\/v1$/, '')
}

export function keySnippets(secret: string, apiBase: string, siteName: string): KeySnippets {
  const root = siteRoot(apiBase)
  const openai = `${root}/v1`
  const models = `${openai}/models`

  const magpieQuery = new URLSearchParams({
    name: siteName,
    chat: openai,
    responses: openai,
    anthropic: root,
    key: secret,
    website: root,
  })

  const ccSwitchQuery = new URLSearchParams({
    resource: 'provider',
    app: 'claude',
    apiFormat: 'anthropic',
    name: siteName,
    endpoint: root,
    apiKey: secret,
    homepage: root,
  })

  const lody = JSON.stringify({
    kind: 'custom',
    id: siteName,
    name: siteName,
    auth: { method: 'apiKey', apiKey: secret },
    endpoints: [
      {
        protocol: 'anthropic-messages',
        baseUrl: root,
        targets: ['claude-code'],
        modelsUrl: models,
      },
      {
        protocol: 'openai-chat',
        baseUrl: openai,
        targets: ['pi', 'codex', 'crush', 'opencode'],
        modelsUrl: models,
      },
    ],
  })

  return {
    magpie: `magpie://import?${magpieQuery.toString()}`,
    ccSwitch: `ccswitch://v1/import?${ccSwitchQuery.toString()}`,
    lody: `lody://provider/import?v=1&data=${base64url(lody)}`,
  }
}
