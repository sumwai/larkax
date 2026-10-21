# 管理面（`/admin/v1/*`）

内部监听上的管理接口：改渠道、模型目录、别名与计价规则。它只挂在
`LARKAX_INTERNAL_ADDR` 那个端口上，**不**出现在公共监听，也不属于任何调用协议。
装配与挂载点见 [development.md](development.md) 的「当前能做什么」一节。

公共监听的鉴权是用户 API Key，那一把 Key 的语义是「以某个用户的名义发推理请求」。
管理面改的是全局数据，所以它用**单独一套管理令牌**，不复用 API Key。

## 令牌

```sh
larkaxctl admin token create    # 生成一枚，明文只在这一次打印
larkaxctl admin token revoke    # 撤销当前令牌
```

`create` 生成 32 字节随机令牌（base64url，43 个字符）。库里只存它的 SHA-256
（64 位小写十六进制），**明文只有 stdout 那一行**，丢了只能重新生成。再
`create` 一次直接替换旧令牌，不留两枚都有效的窗口。`revoke` 之后任何令牌都
匹配不上。

`revoke` 在**从没创建过**令牌的库上是一次空操作：它不改那一行，直接报「没有
配置过管理令牌」。空串表示「这个部署还没启用管理面」（回 503），而撤销表示
「换一枚令牌」（回 401）——把前者写成 401 会让运维去换一枚他从来没创建过的
令牌。

令牌存在 `settings` 表的 `admin.token_sha256` 那一行，由上面两条命令写入，不接受
`settings set`（那一列 `editable` 为 false）。

**每次请求都读一次库**，所以生成与撤销立刻生效，不必重启。管理面的流量很小，
一次主键查询不值得为它引入缓存与失效逻辑；**它不在请求热路径上**，也不该被当成
热路径使用——推理请求不会经过这里。

请求头：

```
Authorization: Bearer <令牌>
```

## 状态码

| 情形 | 状态码 | 正文 |
|---|---|---|
| 没有配置过令牌（`admin.token_sha256` 是空串） | `503` | 只说管理面没启用、提示先 `larkaxctl admin token create`；不透露是哪一项没配 |
| 没带令牌，或令牌不对（含撤销之后） | `401` | 两者逐字相同 |
| 这个实例没有装配管理面（worker 角色） | `404` | 路径根本没有注册 |

「没配置」与「没授权」要分开：前者的下一步是先把服务配起来，后者是换一枚令牌。
「没装配」与「没授权」也要分开：worker 角色上 `/admin/v1/` 回 404，不是 401。

## 接口面

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/admin/v1/channels` | 渠道清单 |
| `POST` | `/admin/v1/channels` | 建渠道 |
| `GET` | `/admin/v1/channels/{id}` | 单条渠道 |
| `PATCH` | `/admin/v1/channels/{id}` | 改渠道 |
| `GET` | `/admin/v1/models` | 模型目录 |
| `POST` | `/admin/v1/models` | 建模型 |
| `PATCH` | `/admin/v1/models/{name}` | 改模型（`{name}` 是规范名） |
| `DELETE` | `/admin/v1/models/{name}` | 删模型 |
| `GET` | `/admin/v1/aliases` | 别名清单；`?channel_id=N` 收紧到某条渠道 |
| `POST` | `/admin/v1/aliases` | 建别名 |
| `PATCH` | `/admin/v1/aliases/{id}` | 改别名 |
| `DELETE` | `/admin/v1/aliases/{id}` | 删别名 |
| `GET` | `/admin/v1/pricing/rules` | 售价规则清单 |
| `POST` | `/admin/v1/pricing/rules` | 建售价规则 |
| `PATCH` | `/admin/v1/pricing/rules/{id}` | 改售价规则 |
| `GET` | `/admin/v1/pricing/costs` | 成本规则清单 |
| `POST` | `/admin/v1/pricing/costs` | 建成本规则 |
| `PATCH` | `/admin/v1/pricing/costs/{id}` | 改成本规则 |

别名清单不带 `channel_id` 时给的是**平台级**那一段（`channel_id = 0`），与
`larkaxctl alias list` 的默认口径一致。

**用户管理不在这里**：`/api/v1/admin/users` 只挂在控制台的会话面（见
[api.md](api.md) 的「控制台管理接口：用户」）。它的判据里有「不能改自己」
「不能撤掉最后一个 active 管理员」，而运维令牌没有「自己」这个概念。内部
管理面的资源组是一份显式清单（`adminapi.InternalGroups`），所以
`/admin/v1/users` 回 `404`，不是 `401`。

这一层是薄层：写入语义与校验全在 `internal/channels`、`internal/catalog`、
`internal/pricing` 的 admin 类型里，接口只做「解请求 → 调它 → 翻成 JSON」。
字段名用 snake_case，与 CLI 的 `--flag` 一一对应（`base_url` ↔ `--base-url`），
完整字段见各资源的请求结构（`internal/adminapi/resources.go`）与
[cli-reference.md](cli-reference.md) 的对应命令。

## 请求与响应

- 正文是 JSON，上限 1 MiB。**未定义的字段会被拒绝**（`400`），不是静默忽略：
  管理面的写错一个字段名应当当场知道。
- 清单回数组（`200`，空清单是 `[]`），建回 `201` 与 `{"id": N}`，改回 `200`
  与 `{"ok": true}`，删回 `204`。
- `PATCH` 只改给出的字段：没出现的键表示不改。少数可空字段另有「清空」的写法，
  见 `resources.go` 里各更新结构的注释（例如模型的 `knowledge_cutoff` 给空串）。
- 渠道的凭证**只写不读**：`credential` 是写入字段，读回的是 `credential_keys`
  与 `credential_hint`（打了码的提示），任何路径都不回明文。

## 错误体

与公共监听同一个形状（`internal/api` 的 `WriteError`，OpenAI 那一套）：

```json
{"error": {"message": "…", "type": "…", "param": null, "code": null}}
```

| 状态码 | 什么时候 |
|---|---|
| `400` | 请求体不是 JSON、有未定义的字段、校验失败 |
| `401` | 令牌缺失或不对 |
| `404` | 目标不存在 |
| `409` | 约束冲突（重名、仍被引用、别名撞车） |
| `500` | 数据库报错 |
| `503` | 没配置令牌、或数据库连不上 |

消息是给调用方看的一句话，不带上游原文与内部路径；需要细节时去查服务端日志。
`500` 与 `503` 的正文是**固定文案**：数据库驱动与连接错误可能带着连接串、主机名
与表名，那些只写进日志，不回给调用方。
