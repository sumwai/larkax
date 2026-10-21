// @ts-expect-error node:fs 只在 vitest 的 Node 运行时里用，不进构建产物
import { readFileSync } from 'node:fs'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { SiteProvider } from '../site/SiteProvider'
import { DateRangePicker } from './DateRangePicker'

const mocks = vi.hoisted(() => ({ meta: vi.fn() }))

vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  return {
    ...actual,
    api: {
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const stylesCss = readFileSync('src/styles/components.css', 'utf8')

/** 取出某个媒体查询的整块声明：花括号配对，避免把块外的同名选择器算进来。 */
function mediaBlock(query: string): string {
  const start = stylesCss.indexOf(`@media ${query}`)
  expect(start).toBeGreaterThanOrEqual(0)
  const open = stylesCss.indexOf('{', start)
  let depth = 0
  for (let index = open; index < stylesCss.length; index += 1) {
    if (stylesCss[index] === '{') depth += 1
    else if (stylesCss[index] === '}') {
      depth -= 1
      if (depth === 0) return stylesCss.slice(open, index + 1)
    }
  }
  return ''
}

function renderPicker() {
  mocks.meta.mockResolvedValue({
    site_name: 'larkax',
    api_base_url: 'https://api.example.com/v1',
    currency: 'USD',
    timezone: 'Asia/Shanghai',
    locales: ['zh', 'en'],
    features: { registration: false, password_reset: 'admin', topup: false },
  })
  return render(
    <I18nProvider>
      <SiteProvider>
        <DateRangePicker from="2026-10-09" to="2026-10-15" onChange={() => undefined} />
      </SiteProvider>
    </I18nProvider>,
  )
}

describe('日期范围选择器', () => {
  beforeEach(() => {
    mocks.meta.mockReset()
  })

  it('起止两个按钮各有自己的可及名，不都叫「时间范围」', () => {
    renderPicker()
    expect(screen.getByRole('button', { name: '开始日期：2026-10-09' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '结束日期：2026-10-15' })).toBeInTheDocument()
  })

  it('英文下可及名用 ASCII 冒号，与中文的「：」各走各的键', () => {
    localStorage.setItem('larkax.lang', 'en')
    renderPicker()
    expect(screen.getByRole('button', { name: 'Start date: 2026-10-09' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'End date: 2026-10-15' })).toBeInTheDocument()
  })

  it('窄屏下起止各占一行，日期串不折行', () => {
    const block = mediaBlock('(max-width: 519.98px)')
    // 两行：容器竖排，原来那个「至」的分隔符不再占位。
    expect(block).toMatch(/\.daterange\s*\{[^}]*flex-direction:\s*column/)
    expect(block).toMatch(/\.daterange \.sep\s*\{[^}]*display:\s*none/)
    // 每个按钮独占整宽，日期是整串显示而不是在连字符处折行。
    expect(block).toMatch(/\.daterange \.dr\s*\{[^}]*width:\s*100%/)
    expect(block).toMatch(/\.daterange \.dr\s*\{[^}]*white-space:\s*nowrap/)
  })
})
