import type { ComponentType } from 'react'
import Baichuan from '@lobehub-icons/Baichuan/components/Mono.mjs'
import ByteDance from '@lobehub-icons/ByteDance/components/Mono.mjs'
import Claude from '@lobehub-icons/Claude/components/Mono.mjs'
import DeepSeek from '@lobehub-icons/DeepSeek/components/Mono.mjs'
import Gemini from '@lobehub-icons/Gemini/components/Mono.mjs'
import Hunyuan from '@lobehub-icons/Hunyuan/components/Mono.mjs'
import Kimi from '@lobehub-icons/Kimi/components/Mono.mjs'
import Meta from '@lobehub-icons/Meta/components/Mono.mjs'
import Minimax from '@lobehub-icons/Minimax/components/Mono.mjs'
import Mistral from '@lobehub-icons/Mistral/components/Mono.mjs'
import OpenAI from '@lobehub-icons/OpenAI/components/Mono.mjs'
import Qwen from '@lobehub-icons/Qwen/components/Mono.mjs'
import Stepfun from '@lobehub-icons/Stepfun/components/Mono.mjs'
import XAI from '@lobehub-icons/XAI/components/Mono.mjs'
import Zhipu from '@lobehub-icons/Zhipu/components/Mono.mjs'

/**
 * 模型名 → 厂商图标。
 *
 * 图标来自 @lobehub/icons，只取每家的 Mono 组件：它画的是纯路径、颜色跟着
 * `currentColor`，深浅两个主题都不用另外换色。整包 entry 会连带 require
 * `@lobehub/ui`（这里没装），所以按目录别名逐个别引，见 vite.config.ts。
 */
type IconComponent = ComponentType<{ size?: number | string; className?: string }>

const VENDORS: { match: RegExp; icon: IconComponent }[] = [
  { match: /^deepseek/i, icon: DeepSeek },
  { match: /^(gpt|chatgpt|codex|davinci|o[1-9])/i, icon: OpenAI },
  { match: /^claude/i, icon: Claude },
  { match: /^gemini/i, icon: Gemini },
  { match: /^(qwen|qwq|qwen3)/i, icon: Qwen },
  { match: /^(glm|chatglm)/i, icon: Zhipu },
  { match: /^(kimi|moonshot)/i, icon: Kimi },
  { match: /^grok/i, icon: XAI },
  { match: /^(mistral|mixtral|codestral|magistral)/i, icon: Mistral },
  { match: /^(llama|meta)/i, icon: Meta },
  { match: /^(doubao|ep-)/i, icon: ByteDance },
  { match: /^hunyuan/i, icon: Hunyuan },
  { match: /^(minimax|abab)/i, icon: Minimax },
  { match: /^step-/i, icon: Stepfun },
  { match: /^(ernie|wenxin)/i, icon: Baichuan },
]

/** 查不到厂商时返回 null，调用方不画图标，不留一个空框。 */
export function modelIcon(model: string): IconComponent | null {
  const found = VENDORS.find((vendor) => vendor.match.test(model))
  return found?.icon ?? null
}
