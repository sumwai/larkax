import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// vitest 没开 globals，Testing Library 的自动清理挂不上；每个用例结束后自己卸干净。
// 语言偏好与主题这类存进 localStorage 的选择一并清掉，免得用例之间互相带。
afterEach(() => {
  cleanup()
  localStorage.clear()
})
