import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** 合并 className：条件值交给 clsx，冲突的工具类交给 tailwind-merge。 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs))
}
