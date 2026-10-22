import { expect, test, type Page } from '@playwright/test'

/**
 * 窄屏下的触控热区。
 *
 * 44×44 是触控的通行下限：比它小的可点元素在手机上点不中，还会误触邻居。
 * 热区由 `components.css` 在 `max-width: 519.98px` 断点里用一层透明的
 * `::after` 撑出来，不改视觉尺寸——所以伪元素不体现在
 * `getBoundingClientRect` 里，命中矩形要自己按 `inset` 把它并上去。
 *
 * 只在真实浏览器里量：jsdom 不算布局，`getComputedStyle` 也不按特异度与
 * `!important` 走，量出来的数不能用。
 *
 * 账号与其他端到端用例一样由外部给：`E2E_USER_USER` / `E2E_USER_PASS`。
 */
const USER = {
  user: process.env.E2E_USER_USER ?? '',
  pass: process.env.E2E_USER_PASS ?? '',
}

const MIN_TOUCH = 44

/** 窄屏下要量的可点控件：复制键、按钮（含 `.sm`）、顶栏的开抽屉与头像。 */
const MOBILE_TARGETS = ['.code-copy', '.btn', '.app-topbar .side-toggle', '.app-topbar .avatar']

interface Box {
  left: number
  top: number
  right: number
  bottom: number
}

interface HitBox {
  className: string
  text: string
  rect: Box
  hit: Box
}

async function login(page: Page) {
  await page.goto('/login')
  await page.fill('#login-username', USER.user)
  await page.fill('#login-password', USER.pass)
  await page.getByRole('button', { name: '登录' }).click()
  await page.waitForURL(/\/app(\/|$)/)
}

/**
 * 元素的命中矩形：自身矩形与 `::after` 热区矩形取并集。
 *
 * 伪元素是绝对定位的，包含块是元素自己的 padding box——左边是「元素左边 +
 * 边框 + `left`」，右边是「元素右边 - 边框 - `right`」。
 */
async function hitBoxes(page: Page, selector: string): Promise<HitBox[]> {
  return page.$$eval(selector, (els) => {
    const toPx = (value: string) => (value.endsWith('px') ? Number.parseFloat(value) : null)
    return els.map((el) => {
      const rect = el.getBoundingClientRect()
      const after = getComputedStyle(el, '::after')
      const own: Box = { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom }
      const hit: Box = { ...own }
      if (after.content !== 'none' && after.position === 'absolute') {
        const left = toPx(after.left)
        const right = toPx(after.right)
        const top = toPx(after.top)
        const bottom = toPx(after.bottom)
        const paddingLeft = rect.left + el.clientLeft
        const paddingTop = rect.top + el.clientTop
        if (left !== null) hit.left = Math.min(hit.left, paddingLeft + left)
        if (top !== null) hit.top = Math.min(hit.top, paddingTop + top)
        if (right !== null) hit.right = Math.max(hit.right, paddingLeft + el.clientWidth - right)
        if (bottom !== null) hit.bottom = Math.max(hit.bottom, paddingTop + el.clientHeight - bottom)
      }
      return {
        className: typeof el.className === 'string' ? el.className : el.tagName,
        text: (el.textContent ?? '').trim().slice(0, 12),
        rect: own,
        hit,
      }
    })
  })
}

const size = (box: Box) => ({ width: box.right - box.left, height: box.bottom - box.top })

function expectTouchTarget(box: HitBox) {
  const { width, height } = size(box.hit)
  expect(width, `${box.className}「${box.text}」的命中宽度`).toBeGreaterThanOrEqual(MIN_TOUCH)
  expect(height, `${box.className}「${box.text}」的命中高度`).toBeGreaterThanOrEqual(MIN_TOUCH)
}

interface RingProbe {
  side: string
  /** 打点位置离视觉矩形边缘的距离，px。 */
  distance: number
  /** true/false 是打点结果；null 表示这个方向不需要或不该打点。 */
  hitSelf: boolean | null
  reason: string
}

interface RingRow {
  className: string
  text: string
  probes: RingProbe[]
}

/**
 * 视觉矩形外侧的环带探针：每个方向打一个 `elementFromPoint`，命中的必须是
 * 控件自己或它的子孙。
 *
 * `hitBoxes` 是**算**出来的——`::after` 的 `inset` 写得再大，被祖先的
 * `overflow: hidden/auto` 裁掉一半时那个数字照样是 44。热区被裁是这条改动
 * 最容易被静默破坏的地方，所以另外拿真实命中结果对一次。
 *
 * 只在该轴视觉尺寸不足 44 的方向上要求热区存在；横向只补 1px 的那一圈是
 * 为了盖住圆角，1.5px 处已经落到热区外面，量到的会是探针自己，因此
 * 热区不足 2px 的方向不打点。视觉尺寸本来就够 44 的方向也不打点。
 */
async function ringProbes(page: Page, selector: string): Promise<RingRow[]> {
  return page.$$eval(selector, (els) => {
    const PROBE = 1.5
    const MIN_MARGIN = 2
    const toPx = (value: string) => (value.endsWith('px') ? Number.parseFloat(value) : null)
    return els.map((el) => {
      el.scrollIntoView({ block: 'center' })
      const rect = el.getBoundingClientRect()
      const after = getComputedStyle(el, '::after')
      const hit = { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom }
      if (after.content !== 'none' && after.position === 'absolute') {
        const left = toPx(after.left)
        const right = toPx(after.right)
        const top = toPx(after.top)
        const bottom = toPx(after.bottom)
        const paddingLeft = rect.left + el.clientLeft
        const paddingTop = rect.top + el.clientTop
        if (left !== null) hit.left = Math.min(hit.left, paddingLeft + left)
        if (top !== null) hit.top = Math.min(hit.top, paddingTop + top)
        if (right !== null) hit.right = Math.max(hit.right, paddingLeft + el.clientWidth - right)
        if (bottom !== null) hit.bottom = Math.max(hit.bottom, paddingTop + el.clientHeight - bottom)
      }
      const midX = (rect.left + rect.right) / 2
      const midY = (rect.top + rect.bottom) / 2
      const needH = rect.width < 44
      const needV = rect.height < 44
      const sides = [
        { side: '左', need: needH, margin: rect.left - hit.left, x: rect.left - PROBE, y: midY },
        { side: '右', need: needH, margin: hit.right - rect.right, x: rect.right + PROBE, y: midY },
        { side: '上', need: needV, margin: rect.top - hit.top, x: midX, y: rect.top - PROBE },
        { side: '下', need: needV, margin: hit.bottom - rect.bottom, x: midX, y: rect.bottom + PROBE },
      ]
      const probes = sides.map((side) => {
        if (!side.need) {
          return { side: side.side, distance: PROBE, hitSelf: null, reason: '该轴视觉尺寸已够 44，不需要热区' }
        }
        if (side.margin < MIN_MARGIN) {
          return {
            side: side.side,
            distance: PROBE,
            hitSelf: false,
            reason: `该侧热区只有 ${side.margin.toFixed(2)}px，视觉矩形外 ${PROBE}px 处不是热区`,
          }
        }
        if (side.x <= 0 || side.y <= 0 || side.x >= window.innerWidth - 1 || side.y >= window.innerHeight - 1) {
          return { side: side.side, distance: PROBE, hitSelf: null, reason: '打点落在视口外' }
        }
        const found = document.elementFromPoint(side.x, side.y)
        const hitSelf = found !== null && (found === el || el.contains(found))
        return {
          side: side.side,
          distance: PROBE,
          hitSelf,
          reason: hitSelf ? '' : `命中的是 ${found ? found.className || found.tagName : 'null'}，热区被祖先裁掉了`,
        }
      })
      return {
        className: typeof el.className === 'string' ? el.className : el.tagName,
        text: (el.textContent ?? '').trim().slice(0, 12),
        probes: rect.width === 0 ? [] : probes,
      }
    })
  })
}


test.describe('窄屏触控热区（375×667）', () => {
  test.use({ viewport: { width: 375, height: 667 } })

  test('按钮与复制键的命中矩形至少 44×44', async ({ page }) => {
    await login(page)
    await page.goto('/app/quickstart')
    await expect(page.locator('.code-copy').first()).toBeVisible()

    // 顶栏的两个图标按钮（`.side-toggle` 开抽屉、`.avatar` 进设置）是窄屏下
    // 最常按的两个控件，一并量。
    const rows = []
    for (const selector of MOBILE_TARGETS) {
      rows.push(...(await hitBoxes(page, selector)))
    }
    expect(rows.length).toBeGreaterThan(0)
    for (const row of rows) {
      if (size(row.rect).width === 0) continue // 断点下隐藏的桌面表格
      expectTouchTarget(row)
    }
  })

  test('热区在视觉矩形外也点得中（防祖先裁切）', async ({ page }) => {
    await login(page)
    await page.goto('/app/quickstart')
    await expect(page.locator('.code-copy').first()).toBeVisible()

    let probed = 0
    for (const selector of MOBILE_TARGETS) {
      for (const row of await ringProbes(page, selector)) {
        for (const probe of row.probes) {
          if (probe.hitSelf === null) continue
          probed += 1
          expect(
            probe.hitSelf,
            `${row.className}「${row.text}」的${probe.side}侧：视觉矩形外 ${probe.distance}px 处点不中自己——${probe.reason}`,
          ).toBe(true)
        }
      }
    }
    // 一条都没打点说明选择器全落空了，绿了也不算数。
    expect(probed).toBeGreaterThan(0)
  })

  test('相邻按钮的热区不相交', async ({ page }) => {
    await login(page)
    await page.goto('/app/usage')
    await page.locator('.daterange .dr').first().click()

    const boxes = await hitBoxes(page, '.cal-foot .btn')
    expect(boxes).toHaveLength(2)
    const [left, right] = boxes
    // 两个矩形相交才算重叠：它们并排，纵向本来就落在同一段里。
    const overlapX = Math.min(left.hit.right, right.hit.right) - Math.max(left.hit.left, right.hit.left)
    const overlapY = Math.min(left.hit.bottom, right.hit.bottom) - Math.max(left.hit.top, right.hit.top)
    expect(overlapX > 0 && overlapY > 0, '「今天」与「清除」的热区相交').toBe(false)
  })
})

test.describe('桌面档（1280×800）', () => {
  test.use({ viewport: { width: 1280, height: 800 } })

  test('热区不外扩，尺寸与改前一致', async ({ page }) => {
    await login(page)
    await page.goto('/app/quickstart')
    await expect(page.locator('.btn').first()).toBeVisible()

    const rows = [...(await hitBoxes(page, '.code-copy')), ...(await hitBoxes(page, '.btn'))]
    expect(rows.length).toBeGreaterThan(0)
    for (const row of rows) {
      if (size(row.rect).width === 0) continue
      // 命中矩形等于自身矩形：伪元素热区只在窄屏断点里存在。
      expect(size(row.hit).width).toBeCloseTo(size(row.rect).width, 1)
      expect(size(row.hit).height).toBeCloseTo(size(row.rect).height, 1)
    }

    // 视觉尺寸没被撑大：`.btn` 38px、`.btn.sm` 32px。
    for (const row of await hitBoxes(page, '.btn')) {
      if (size(row.rect).width === 0) continue
      const expected = row.className.includes('sm') ? 32 : 38
      expect(size(row.rect).height, `${row.className}「${row.text}」的视觉高度`).toBeCloseTo(expected, 0)
    }
  })
})
