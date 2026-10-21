import { expect, test, type Page } from '@playwright/test'

/**
 * 控制台运营页的端到端用例。
 *
 * 账号与库都由外部准备（staging 上的临时实例），口令只经环境变量传进来：
 *
 *   E2E_MERCHANT_USER / E2E_MERCHANT_PASS   商家账号
 *   E2E_USER_USER     / E2E_USER_PASS       普通账号
 *   E2E_FOREIGN_CHANNEL_ID                  另一个商家名下的渠道 ID
 */
const MERCHANT = {
  user: process.env.E2E_MERCHANT_USER ?? '',
  pass: process.env.E2E_MERCHANT_PASS ?? '',
}
const USER = {
  user: process.env.E2E_USER_USER ?? '',
  pass: process.env.E2E_USER_PASS ?? '',
}
const FOREIGN_CHANNEL_ID = Number(process.env.E2E_FOREIGN_CHANNEL_ID ?? '0')

async function login(page: Page, user: string, pass: string) {
  await page.goto('/login')
  await page.fill('#login-username', user)
  await page.fill('#login-password', pass)
  await page.getByRole('button', { name: '登录' }).click()
  await page.waitForURL(/\/app(\/|$)/)
}

test.describe('运营视图', () => {
  test('商家：切换视图、新建渠道、停用（确认钮是危险色）', async ({ page }) => {
    await login(page, MERCHANT.user, MERCHANT.pass)

    // 视图切换只列账号拥有的：有「商家」，没有「管理」。
    await expect(page.getByRole('button', { name: '商家' })).toBeVisible()
    await expect(page.getByRole('button', { name: '管理' })).toHaveCount(0)
    await page.getByRole('button', { name: '商家' }).click()
    await expect(page).toHaveURL(/\/app\/merchant\/channels/)

    const name = `e2e-channel-${Date.now()}`
    await page.getByRole('button', { name: '新建渠道' }).first().click()
    const form = page.getByRole('dialog', { name: '新建渠道' })
    await form.getByLabel('名称').fill(name)
    await form.getByLabel('上游地址').fill('https://api.example.com/v1')
    await form.getByLabel('凭证').fill('<CHANNEL_CREDENTIAL>')
    await form.getByLabel('支持的模型（每行一个）').fill(process.env.E2E_MODEL ?? 'e2e-model')
    await form.getByRole('button', { name: '创建' }).click()

    // 表单浮层与确认浮层同时在树上，按标题认那一个。
    const createDialog = page.getByRole('dialog', { name: '确认创建' })
    await expect(createDialog).toContainText(`创建渠道「${name}」`)
    await createDialog.getByRole('button', { name: '创建' }).click()

    const row = page.locator('.channel-row', { hasText: name })
    await expect(row).toBeVisible()

    await row.getByRole('button', { name: '停用' }).click()
    const disableDialog = page.getByRole('dialog', { name: '停用' })
    const confirm = disableDialog.getByRole('button', { name: '确认' })
    await expect(confirm).toBeVisible()
    // 危险动作的确认钮要和「保存」长得不一样。
    await expect(confirm).toHaveClass(/danger/)
    await confirm.click()

    await expect(row.getByText('已停用')).toBeVisible()
  })

  test('商家访问管理页：看到无权限，且不发 admin 请求', async ({ page }) => {
    const adminCalls: string[] = []
    page.on('request', (request) => {
      const path = new URL(request.url()).pathname
      if (path.startsWith('/api/v1/admin/')) adminCalls.push(path)
    })

    await login(page, MERCHANT.user, MERCHANT.pass)
    await page.goto('/app/admin/channels')

    await expect(page.getByText('这个视图不在你的权限范围内。')).toBeVisible()
    expect(adminCalls).toEqual([])
  })

  test('商家读别家渠道：接口 404，页面给的是后端那句话', async ({ page }) => {
    test.skip(FOREIGN_CHANNEL_ID <= 0, '没有提供 E2E_FOREIGN_CHANNEL_ID')
    await login(page, MERCHANT.user, MERCHANT.pass)

    const direct = await page.evaluate(async (id) => {
      const response = await fetch(`/api/v1/merchant/channels/${id}`, {
        credentials: 'same-origin',
      })
      return { status: response.status, body: await response.text() }
    }, FOREIGN_CHANNEL_ID)
    expect(direct.status).toBe(404)

    // 列表里塞进这条「别人的」渠道，点「查看」，看错误是怎么显示的。
    await page.route('**/api/v1/merchant/channels', async (route) => {
      if (route.request().method() !== 'GET') return route.continue()
      const response = await route.fetch()
      const rows = (await response.json()) as unknown[]
      rows.push({
        id: FOREIGN_CHANNEL_ID,
        owner_user_id: -1,
        name: 'other-merchant-channel',
        type: 'openai',
        base_url: 'https://api.example.com/v1',
        status: 'enabled',
        models: [process.env.E2E_MODEL ?? 'e2e-model'],
        weight: 1,
        priority: 0,
        setting: {},
        credential_keys: [],
        credential_hint: '',
        credential_error: null,
        deleted_at: null,
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
      })
      await route.fulfill({ response, json: rows })
    })

    await page.goto('/app/merchant/channels')
    await page.locator('.channel-row', { hasText: 'other-merchant-channel' })
      .getByRole('button', { name: '查看' })
      .click()

    const dialog = page.getByRole('dialog', { name: '渠道详情' })
    // 4xx 的正文由服务端决定，前端原样显示（见 describeError）。这里钉住的是
    // 「原样」——后端给什么，页面上就是什么。
    const message = (JSON.parse(direct.body) as { error: { message: string } }).error.message
    expect(message).not.toBe('')
    await expect(dialog).toContainText(message)
  })

  test('普通用户：模型列表里的提供方与价格', async ({ page }) => {
    test.skip(true, '依赖 #888：模型目录还没有展示提供该模型的商家与价格')
    await login(page, USER.user, USER.pass)
    await page.goto('/app/models')
    await expect(page.getByText('提供该模型的商家')).toBeVisible()
  })
})
