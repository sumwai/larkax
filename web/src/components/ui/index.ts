/**
 * 组件库出口。页面从这里 import，不深入各文件。
 * 命名与形状对齐 shadcn 的约定；行为层归 Radix / sonner，外观归 token。
 */
export { cn } from './lib'

export { Button, buttonVariants, type ButtonProps } from './button'
export { Badge, badgeVariants, type BadgeProps } from './badge'
export { Alert, Banner, alertVariants } from './alert'
export { Card, CardHeader, CardTitle, CardBody, CardFooter } from './card'
export { Field, Input, Textarea, MoneyInput } from './field'
export {
  Dialog,
  DialogTrigger,
  DialogClose,
  DialogContent,
  ConfirmDialog,
  type DialogContentProps,
} from './dialog'
export { Select, type SelectOption, type SelectProps } from './select'
export { Tabs, TabsList, TabsTrigger, TabsContent, Segmented } from './tabs'
export {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuLabel,
  Popover,
  PopoverTrigger,
  PopoverClose,
  PopoverContent,
  TooltipProvider,
  Tooltip,
  TooltipTrigger,
  TooltipContent,
  Hint,
} from './menu'
export { Sheet, ActionSheet, SHEET_SIZES, type SheetSize } from './sheet'
export { Table, TableHead, Th, TableRow, Td, Pagination } from './table'
export { LineChart, BarChart, Sparkline } from './charts'
export { AppToaster, toast } from './toast'
export { PageHead, Breadcrumbs } from './page-head'
export { Money } from './money'
export {
  Skeleton,
  Spinner,
  Empty,
  Progress,
  Ring,
  Kbd,
  copyText,
  CopyButton,
  CodeBlock,
} from './misc'
