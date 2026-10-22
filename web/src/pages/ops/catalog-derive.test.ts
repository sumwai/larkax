import { describe, expect, it } from 'vitest'
import { deriveAdapterDefaults } from './catalog-derive'

function adapter(type: string, protocol: string, endpoint: string) {
  return { type, protocol, endpoint }
}

describe('适配器带入', () => {
  it.each([
    ['openai', 'openai', '/v1/chat/completions'],
    ['openai-responses', 'openai-responses', '/v1/responses'],
    ['anthropic', 'anthropic', '/v1/messages'],
  ])('%s：端点照抄注册表，模态按协议推定文本基线', (type, protocol, endpoint) => {
    expect(deriveAdapterDefaults(adapter(type, protocol, endpoint))).toEqual({
      endpoint,
      modalities: { input: ['text'], output: ['text'] },
    })
  })

  it('systemone：推不出模态，但端点仍然带出', () => {
    expect(deriveAdapterDefaults(adapter('systemone', 'systemone', '/v1/systemone'))).toEqual({
      endpoint: '/v1/systemone',
      modalities: null,
    })
  })

  it('未知协议：同样是端点带出、模态推不出', () => {
    expect(deriveAdapterDefaults(adapter('custom', 'custom', '/v1/custom'))).toEqual({
      endpoint: '/v1/custom',
      modalities: null,
    })
  })
})
