import type { ChannelAdapter } from '../../api/types'

/** 模型目录里输入与输出两栏模态。 */
export interface Modalities {
  input: string[]
  output: string[]
}

/**
 * 选定适配器之后，表单能自动带入的那部分。
 *
 * `modalities` 为 null 表示按这套协议推不出模态，调用方要把字段退回手填，
 * 并把原因写在界面上——静默留空会让人以为「目录里本来就没有」。
 */
export interface AdapterDefaults {
  /** 适配器实际打的那条路径，与目录 `capabilities.endpoints` 是同一套取值。 */
  endpoint: string
  modalities: Modalities | null
}

/**
 * 协议 → 模态默认值。
 *
 * 这是一条**按协议推定**的规则，不是适配器自报的能力：适配器清单（服务端的
 * `adapterJSON`）只有 `type` / `protocol` / `endpoint`，没有任何模态字段。
 * 三套对话协议收发的都是文本，所以推出来的只有 text 这一条基线；图片、音频
 * 取决于模型自己，不取决于协议，由填表的人补。界面上写明了这是推定值。
 *
 * `systemone` 不在表里：它是本平台自己的判定入口，目录不描述它承载什么模态。
 * 缺一行就是「推不出来」，退回手填（见 Catalog 的模态说明）。
 */
const MODALITIES_BY_PROTOCOL: Record<string, Modalities> = {
  openai: { input: ['text'], output: ['text'] },
  'openai-responses': { input: ['text'], output: ['text'] },
  anthropic: { input: ['text'], output: ['text'] },
}

/** 端点由适配器这一份清单回答；模态按协议推定，推不出时给 null。 */
export function deriveAdapterDefaults(adapter: ChannelAdapter): AdapterDefaults {
  return {
    endpoint: adapter.endpoint,
    modalities: MODALITIES_BY_PROTOCOL[adapter.protocol] ?? null,
  }
}
