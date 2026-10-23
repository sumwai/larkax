import { Toaster as SonnerToaster, toast } from 'sonner'
import { useTheme } from '../../theme/ThemeProvider'

/** 跟随系统时解析出实际主题，给 sonner 选配色。 */
function useResolvedTheme(): 'light' | 'dark' {
  const { theme } = useTheme()
  if (theme !== 'system') return theme
  return typeof window !== 'undefined' && window.matchMedia('(prefers-color-scheme: dark)').matches
    ? 'dark'
    : 'light'
}

/**
 * 轻提示：只报告结果，不让人做选择；需要选择的用对话框。
 * 全站只有一个 <AppToaster />，页面里 import { toast } 直接调。
 */
export { toast }

export function AppToaster() {
  const resolved = useResolvedTheme()
  return (
    <SonnerToaster
      theme={resolved}
      position="top-center"
      className="z-toast"
      toastOptions={{
        classNames: {
          toast:
            '!rounded-sm !border !border-border !bg-popover !text-popover-foreground !text-sm !shadow-md',
          description: '!text-muted-foreground',
        },
      }}
    />
  )
}
