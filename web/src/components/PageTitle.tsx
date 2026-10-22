import { createContext, useContext, useLayoutEffect, type ReactNode } from 'react'
import { useLocation } from 'react-router-dom'

/** 页面报上来的标题连同它所属的地址；外壳只在地址对得上时采用。 */
export interface ReportedTitle {
  path: string
  title: string
}

type Report = (value: ReportedTitle | null) => void

const PageTitleContext = createContext<Report | null>(null)

/**
 * 把「报告标题」的入口交给外壳的子树。
 *
 * 只有页面自己才知道的名字（例如模型展示名）走这条路；由导航项反推的页面名
 * 仍由外壳算，`document.title` 与顶栏都从外壳那一个值来。
 */
export function PageTitleReporter({ report, children }: { report: Report; children: ReactNode }) {
  return <PageTitleContext.Provider value={report}>{children}</PageTitleContext.Provider>
}

/**
 * 页面把自己的标题交给外壳，卸载时撤回。
 *
 * 地址一起报上去：外壳按地址取用，换页瞬间不用等这一条先被清掉，也就不会
 * 先闪一下父级的名字。用 `useLayoutEffect` 是为了让重新报告赶在绘制之前。
 */
export function useReportPageTitle(title: string | null) {
  const report = useContext(PageTitleContext)
  const { pathname } = useLocation()
  useLayoutEffect(() => {
    if (!report) return
    report(title ? { path: pathname, title } : null)
    return () => report(null)
  }, [report, pathname, title])
}

/**
 * 写浏览器标签页的标题。
 *
 * 控制台由外壳统一写（含站点后缀），公开页各自写自己那一句；其余地方不要
 * 再碰 `document.title`，否则又多了一份映射。
 */
export function useDocumentTitle(title: string) {
  useLayoutEffect(() => {
    document.title = title
  }, [title])
}
