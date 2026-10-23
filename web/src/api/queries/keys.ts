/**
 * 查询键的唯一出口：域内键统一从这里拼，失效（invalidateQueries）时
 * 才能精确到域而不必全量作废。
 */
export const qk = {
  overview: () => ['overview'] as const,
  keys: () => ['keys'] as const,
  usage: (range: string) => ['usage', range] as const,
  models: () => ['models'] as const,
  model: (name: string) => ['model', name] as const,
  settings: () => ['settings'] as const,
  account: () => ['account'] as const,
  channels: (scope: string) => ['channels', scope] as const,
  catalog: () => ['catalog'] as const,
  aliases: () => ['aliases'] as const,
  pricing: (scope: string) => ['pricing', scope] as const,
  merchantModels: () => ['merchant-models'] as const,
  merchantUpstreams: () => ['merchant-upstreams'] as const,
  users: () => ['users'] as const,
  user: (id: string) => ['user', id] as const,
}
