import type { ReactNode } from 'react'
import { Controller, type Control, type FieldPath, type FieldValues } from 'react-hook-form'
import { Field as FieldShell } from './field'

/**
 * RHF 的表单字段：id 关联、错误播报由这一层出，页面不再手接 id。
 * 控件通过 `render` 拿到受控的 value/onChange，配 ui 里的 Input/Textarea/
 * Select/Switch 组合。
 */
export function FormField<TValues extends FieldValues, TName extends FieldPath<TValues>>({
  control,
  name,
  label,
  hint,
  required,
  render,
  className,
}: {
  control: Control<TValues>
  name: TName
  label: ReactNode
  hint?: ReactNode
  required?: boolean
  render: (field: {
    value: unknown
    onChange: (value: unknown) => void
    onBlur: () => void
    invalid: boolean
    disabled: boolean
  }) => ReactNode
  className?: string
}) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => (
        <FieldShell label={label} hint={hint} error={fieldState.error?.message} required={required} className={className}>
          {() =>
            render({
              value: field.value,
              onChange: field.onChange,
              onBlur: field.onBlur,
              invalid: !!fieldState.error,
              disabled: !!field.disabled,
            })
          }
        </FieldShell>
      )}
    />
  )
}

/** 表单底部的错误句：容器常驻，内容为空时从布局里摘出去。 */
export function FormMessage({ message }: { message?: string | null }) {
  return (
    <span className="text-xs font-medium text-danger-strong empty:hidden" role="alert">
      {message ?? ''}
    </span>
  )
}
