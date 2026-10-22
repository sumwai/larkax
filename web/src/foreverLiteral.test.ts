// @ts-expect-error node:fs 只在 vitest 的 Node 运行时里用，不进构建产物
import { readdirSync, readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

// 「不过期」的字面量只属于服务端：页面拿到的只有 never_expires 这个布尔。
// 生产源码里写死日期，等于把值的出处又搬回前端，改常量时这一处不会跟着变。
//
// 本文件与 Keys.test.tsx 的夹具是仅有的两处允许出现这个字面量的测试文件：
// 一处证明页面不看它，一处证明源码里没有它。
const FOREVER_LITERAL = '9999-12-31'
const SRC = 'src'

// node:fs 的类型不在前端依赖里，这里只用到 name 与 isDirectory 两样。
type DirEntry = { name: string; isDirectory: () => boolean }

function isTestFile(path: string): boolean {
  if (/\.(test|spec)\.tsx?$/.test(path)) return true
  if (path.startsWith(`${SRC}/test/`)) return true
  return path.includes('/__tests__/')
}

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry: DirEntry) => {
    const path = `${dir}/${entry.name}`
    if (entry.isDirectory()) return sourceFiles(path)
    if (!/\.tsx?$/.test(entry.name) || isTestFile(path)) return []
    return [path]
  })
}

describe('「不过期」的字面量', () => {
  it('生产源码里不出现 9999-12-31', () => {
    const files = sourceFiles(SRC)
    // 扫到零个文件与「扫过且干净」长得一样，先钉住扫描目标不为空。
    expect(files).toContain('src/pages/Keys.tsx')
    const hits = files.filter((path) => readFileSync(path, 'utf8').includes(FOREVER_LITERAL)).sort()
    expect(hits).toEqual([])
  })
})
