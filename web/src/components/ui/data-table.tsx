import { useEffect, useMemo, useState, type ReactNode } from 'react'
import {
  flexRender,
  getCoreRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
  type RowSelectionState,
  type VisibilityState,
} from '@tanstack/react-table'
import { ArrowDown, ArrowUp, ArrowUpDown, Columns3 } from 'lucide-react'
import { cn } from './lib'
import { Button } from './button'
import { Checkbox } from './controls'
import { Empty } from './misc'
import { Segmented } from './tabs'
import { Pagination } from './table'
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem } from './menu'

/**
 * 表格用列定义，不手写网格：列宽、排序、空态、行选择、密度、列开关都从同一份
 * 定义出。窄屏整表折成卡片形态，每格行首标签来自列定义的 meta.label。
 *
 * 选择列由调用方决定要不要：传 `enableRowSelection` 时第一列自动是 Checkbox，
 * 选中行从 `onRowSelectionChange` 回给页面，吸底操作条由页面组合（BatchBar）。
 */
/**
 * 列定义的类型别名：TanStack 的值型参数对渲染函数没用（cell 拿到值直接渲染），
 * 调用方不必要地为每列标注值型，统一 any，收敛在这一处。
 */
/* eslint-disable-next-line @typescript-eslint/no-explicit-any */
export type DataTableColumn<T> = ColumnDef<T, any>

export interface DataTableProps<T> {
  data: T[]
  columns: DataTableColumn<T>[]
  /** 数据列的 grid-template-columns；不含选择列。不给时每列均分。 */
  cols?: string
  empty?: { title: ReactNode; description?: ReactNode; action?: ReactNode }
  enableRowSelection?: boolean | ((row: T) => boolean)
  onRowSelectionChange?: (selected: T[]) => void
  densityToggle?: boolean
  columnToggle?: boolean
  pageSize?: number
  className?: string
  'aria-label'?: string
}

export function DataTable<T>({
  data,
  columns,
  cols,
  empty,
  enableRowSelection,
  onRowSelectionChange,
  densityToggle,
  columnToggle,
  pageSize = 20,
  className,
  'aria-label': ariaLabel,
}: DataTableProps<T>) {
  const [sorting, setSorting] = useState<SortingState>([])
  const [rowSelection, setRowSelection] = useState<RowSelectionState>({})
  const [columnVisibility, setColumnVisibility] = useState<VisibilityState>({})
  const [density, setDensity] = useState<'cozy' | 'compact'>('cozy')
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize })

  const selectionColumn = useMemo<DataTableColumn<T>>(
    () => ({
      id: '__select',
      enableSorting: false,
      enableHiding: false,
      header: ({ table }) => (
        <Checkbox
          aria-label="全选本页"
          checked={table.getIsAllPageRowsSelected()}
          indeterminate={table.getIsSomePageRowsSelected() && !table.getIsAllPageRowsSelected()}
          onCheckedChange={(value) => table.toggleAllPageRowsSelected(!!value)}
        />
      ),
      cell: ({ row }) => (
        <Checkbox
          aria-label="选择这一行"
          checked={row.getIsSelected()}
          onCheckedChange={(value) => row.toggleSelected(!!value)}
        />
      ),
    }),
    [],
  )

  const allColumns = useMemo(
    () => (enableRowSelection ? [selectionColumn, ...columns] : columns),
    [enableRowSelection, selectionColumn, columns],
  )

  const tableInstance = useReactTable({
    data,
    columns: allColumns,
    state: { sorting, rowSelection, columnVisibility, pagination },
    onSortingChange: setSorting,
    onRowSelectionChange: setRowSelection,
    onColumnVisibilityChange: setColumnVisibility,
    onPaginationChange: setPagination,
    enableRowSelection:
      typeof enableRowSelection === 'function'
        ? (row) => enableRowSelection(row.original)
        : (enableRowSelection ?? false),
    getRowId: (_row, index) => String(index),
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
  })

  // 选中集合回给页面：吸底操作条读它。用签名比对，避免每次渲染都触发回调。
  const selectedRows = tableInstance.getSelectedRowModel().rows.map((row) => row.original)
  const selectionKey = tableInstance.getSelectedRowModel().rows.map((row) => row.id).join(',')
  useEffect(() => {
    if (!onRowSelectionChange) return
    onRowSelectionChange(selectedRows)
    // selectedRows 由 selectionKey 唯一决定；直接依赖它会每次渲染都回调。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectionKey])

  const dataColCount = Math.max(1, tableInstance.getVisibleLeafColumns().length - (enableRowSelection ? 1 : 0))
  const dataGrid = cols ?? `repeat(${dataColCount}, minmax(0, 1fr))`
  const gridCols = enableRowSelection ? `36px ${dataGrid}` : dataGrid
  const cellPad = density === 'compact' ? 'py-1.5' : 'py-2.5'
  const rows = tableInstance.getRowModel().rows

  const hideColumn = (id: string) =>
    tableInstance.setColumnVisibility((prev) => ({ ...prev, [id]: !prev[id] }))

  return (
    <div className={cn('flex flex-col gap-3', className)}>
      {(densityToggle || columnToggle) && (
        <div className="flex items-center justify-end gap-1.5">
          {columnToggle && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="sm">
                  <Columns3 className="size-3.5" />
                  列
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent>
                {tableInstance
                  .getAllLeafColumns()
                  .filter((column) => column.getCanHide())
                  .map((column) => (
                    <DropdownMenuItem
                      key={column.id}
                      onSelect={(event) => {
                        event.preventDefault()
                        hideColumn(column.id)
                      }}
                    >
                      <Checkbox
                        checked={column.getIsVisible()}
                        onCheckedChange={() => hideColumn(column.id)}
                      />
                      {String((column.columnDef.meta as { label?: string } | undefined)?.label ?? column.id)}
                    </DropdownMenuItem>
                  ))}
              </DropdownMenuContent>
            </DropdownMenu>
          )}
          {densityToggle && (
            <Segmented
              aria-label="密度"
              size="sm"
              value={density}
              onValueChange={setDensity}
              options={[
                { value: 'cozy', label: '舒适' },
                { value: 'compact', label: '紧凑' },
              ]}
            />
          )}
        </div>
      )}

      <div
        role="table"
        aria-label={ariaLabel}
        className="w-full"
        style={{ '--table-cols': gridCols } as React.CSSProperties}
      >
        <div
          role="row"
          className="sticky top-(--table-sticky,0) z-sticky hidden border-b border-border bg-card text-2xs font-semibold text-muted-foreground md:grid"
          style={{ gridTemplateColumns: 'var(--table-cols)' }}
        >
          {tableInstance.getHeaderGroups()[0]?.headers.map((header) => {
            const meta = header.column.columnDef.meta as { label?: string; right?: boolean } | undefined
            const canSort = header.column.getCanSort()
            const dir = header.column.getIsSorted()
            return (
              <div
                key={header.id}
                role="columnheader"
                aria-sort={dir ? (dir === 'asc' ? 'ascending' : 'descending') : 'none'}
                className={cn('flex items-center gap-1 px-3 py-2', meta?.right && 'justify-end')}
              >
                {canSort ? (
                  <button
                    type="button"
                    className="inline-flex items-center gap-1 rounded-xs hover:text-foreground"
                    onClick={header.column.getToggleSortingHandler()}
                  >
                    {flexRender(header.column.columnDef.header, header.getContext())}
                    {dir === 'asc' ? (
                      <ArrowUp className="size-3" />
                    ) : dir === 'desc' ? (
                      <ArrowDown className="size-3" />
                    ) : (
                      <ArrowUpDown className="size-3 opacity-40" />
                    )}
                  </button>
                ) : (
                  flexRender(header.column.columnDef.header, header.getContext())
                )}
              </div>
            )
          })}
        </div>

        {rows.length === 0 ? (
          <div className="pt-3">
            <Empty title={empty?.title} description={empty?.description} action={empty?.action} />
          </div>
        ) : (
          rows.map((row) => (
            <div
              key={row.id}
              role="row"
              className={cn(
                'border-b border-border transition-colors last:border-b-0 hover:bg-muted/50 md:grid',
                row.getIsSelected() && 'bg-primary-soft/50',
              )}
              style={{ gridTemplateColumns: 'var(--table-cols)' }}
            >
              {row.getVisibleCells().map((cell) => {
                const meta = cell.column.columnDef.meta as
                  | { label?: string; right?: boolean; mono?: boolean }
                  | undefined
                return (
                  <div
                    key={cell.id}
                    role="cell"
                    data-label={meta?.label}
                    className={cn(
                      'flex min-w-0 items-center gap-2 px-3 text-sm',
                      cellPad,
                      meta?.right && 'md:justify-end',
                      meta?.mono && 'font-mono num',
                    )}
                  >
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </div>
                )
              })}
            </div>
          ))
        )}
      </div>

      {tableInstance.getRowCount() > pageSize ? (
        <Pagination
          page={tableInstance.getState().pagination.pageIndex + 1}
          pageSize={tableInstance.getState().pagination.pageSize}
          total={tableInstance.getRowCount()}
          onPageChange={(page) => tableInstance.setPageIndex(page - 1)}
        />
      ) : null}
    </div>
  )
}
