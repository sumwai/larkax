import { expect, test, type Page } from '@playwright/test'

/**
 * 模型清单页的滚动。
 *
 * 账号与其他端到端用例一样由外部给：`E2E_USER_USER` / `E2E_USER_PASS`。
 * 两条用例分别在桌面与手机宽度下滚到页面底部，确认最后一个模型卡片
 * 完整落在视口里，且没有被底部导航挡住。
 */
const USER = {
  user: process.env.E2E_USER_USER ?? '',
  pass: process.env.E2E_USER_PASS ?? '',
}

async function openModels(page: Page) {
  await page.goto('/login')
  await page.fill('#login-username', USER.user)
  await page.fill('#login-password', USER.pass)
  await page.getByRole('button', { name: '登录' }).click()
  await page.waitForURL(/\/app(\/|$)/)
  await page.goto('/app/models')
  await expect(page.locator('.model-card').first()).toBeVisible()
}

async function scrollToBottom(page: Page) {
  await page.evaluate(() => window.scrollTo(0, document.scrollingElement?.scrollHeight ?? 0))
  // 地址栏收放与固定底栏都要等一次布局落定。
  await page.waitForTimeout(200)
}

async function expectLastCardReachable(page: Page) {
  const last = page.locator('.model-card').last()
  await expect(last).toBeVisible()
  const card = await last.boundingBox()
  expect(card).not.toBeNull()
  expect(card!.y + card!.height).toBeLessThanOrEqual(page.viewportSize()!.height + 1)

  const nav = page.locator('.bottomnav')
  if (await nav.isVisible()) {
    const bar = await nav.boundingBox()
    expect(bar).not.toBeNull()
    expect(card!.y + card!.height).toBeLessThanOrEqual(bar!.y + 1)
  }
}

test.describe('模型清单的滚动', () => {
  test('桌面宽度：滚到底能看到最后一个模型', async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 })
    await openModels(page)
    await scrollToBottom(page)
    await expectLastCardReachable(page)
  })

  test('手机宽度：底栏不挡住最后一个模型', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await openModels(page)
    await scrollToBottom(page)
    await expect(page.locator('.bottomnav')).toBeVisible()
    await expectLastCardReachable(page)
  })
})
