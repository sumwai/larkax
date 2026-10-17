# 插件契约

插件是挂到请求生命周期上的一段 JS，跑在 moejs 沙箱里。它能看到请求、候选渠道
与响应的**一部分**，而能改的比能看的更少。

**契约的权威定义是 `internal/plugin/contract.go`**：那里每个类型就是一个 hook 的
上下文或补丁，字段表即权限边界——没有出现在 Patch 里的东西插件改不了，也没有
第二处代码需要记得去过滤。这一份讲「怎么用」，以及几处光看字段名看不出来的语义。

> 现状：插件系统尚未接进主服务（`internal/plugin` 还没有生产调用方，见 README）。
> 这一份描述的是契约本身。

## 五个 hook

| hook | 什么时候跑 | 返回值能做什么 |
|---|---|---|
| `onRequest` | 请求发出前 | 改请求体的白名单字段、加请求头 |
| `onRoute` | 选渠道时 | 在候选集里指定一个渠道 |
| `onStream` | 流式响应的每一块 | 丢掉或替换这一块、提前终止 |
| `onResponse` | 非流式响应拿到后 | 改正文与元数据 |
| `onUsage` | 上报用量时 | 报**数量**（token 数、次数），不能报金额 |

hook 返回 `undefined`（或什么都不返回）表示「这次不改」。

字段名要写准。**五个 hook 的返回值都是严格解析的**：出现契约之外的字段
（包括拼错的字段名）会报错，而不是被静默丢掉——静默丢掉的表现是「我改了但没
生效」，那是这类问题里最难查的一种。

报错按**插件失败**处置，与插件抛异常走同一条路：`meta` 里没声明 `blocking` 时
请求照常继续，只是这次补丁整个不生效，而失败记进这次会话的失败清单
（`Session.Failures()`）；声明了 `blocking` 时请求失败。

契约里是自由形状的那几项不受影响：`metadata` 与 `units` 的**键**由插件自己定，
严格解析管的是 `text` / `metadata`、`promptTokens` / `units` 这些名字本身。

（字段名的比较不区分大小写，`channelID` 与 `channelId` 都能解出来。那是宿主
JSON 解码器的行为，不是契约的一部分——把名字写准仍然是要做的事。）

一个请求只跑一个插件：匹配上的多个按优先级取一个，其余不参与。

插件从 `"larkax"` 这个虚拟模块导入 `definePlugin`、`log`、`calendar`、`kv`。
没有 `fetch`、没有 `fs`，也没有渠道凭证——宿主根本没把它们放进插件能碰到的地方。

## 插件长什么样

```js
import { definePlugin, log } from "larkax";

export const meta = {
  key: "acme-vision",
  name: "视觉模型选路",
  apiVersion: 1,
  models: ["acme/vision"],   // 空表示所有模型
  capabilities: ["log"],     // 申请的能力，只能是宿主开放范围的子集
};

export default definePlugin({
  onRoute(ctx) {
    log("info", "候选", { n: ctx.candidates.length });
    // 不决策：交给内核按健康度与优先级排序
    return undefined;
  },
});
```

## `onRoute` 的候选集

`ctx.candidates` 是这次请求可以走的渠道，每个候选的字段：

| 字段 | 含义 |
|---|---|
| `channelId` | 渠道 ID。决策里的 `channelId` **必须在候选集之内** |
| `ownerUserId` | 渠道归属的用户 |
| `weight` / `priority` | 商家配的权重与优先级 |
| `health` | 四档：`healthy` / `degraded` / `drained` / `unknown` |
| `remainingEstimate` | 剩余额度，**只在本次候选集内可比时才给**，见下一节 |
| `confidence` | 上面那个数的可信度，`0..1`；它与 `remainingEstimate` 同生共死 |
| `upstreamCostHint` | 最近观察到的单位成本；缺省表示未知 |
| `resetsAt` | 额度重置时间，RFC3339；缺省表示没有 |

候选里**没有** `base_url`，也没有凭证：`base_url` 足以让插件推断出上游是谁。

`health` 是已经判过的结论而不是原始计数。`unknown` 表示「没有样本」，
与 `healthy` 不是一回事——按后者处理会把一个从没跑过的渠道排到最前面。

## 额度余量：什么时候是 null

`remainingEstimate` 是 `null` 的两种情况：

1. **这个渠道查不到额度。** 它与「额度为零」不同，后者给的是 `0`。
2. **候选集里出现了两种以上额度单位。** 单位可以是按 token 计、按次计、按金额计
   （见 [channel-health.md](channel-health.md) 的「单位」一节）。按 token 计的套餐
   与按元计的账户之间，「还剩 100」谁更该先用掉取决于我们不知道的定价与消耗速度，
   比出来的次序完全由单位决定。此时**所有**候选一律为 `null`，包括单位相同的
   那几个：只给一部分会让「有的有数、有的没数」看起来像那几家查额度失败了。

字段的含义因此是**「本次候选集内，这个数可以与其他候选的 `remainingEstimate`
直接比较」**，而不只是「还剩多少」。

判断「可不可比」是**集合级**的，构造候选集的一方已经做完了（判据与内核打分
共用一处实现，见 `route.MixedQuotaUnits`）。插件不需要自己判断，只需要在
`null` 时把它当作「这里没有可用的信息」。

### 正例

```js
import { definePlugin } from "larkax";

export default definePlugin({
  onRoute(ctx) {
    // null = 「没有可用的信息」。先挑出有数的那些，一个都没有就不决策。
    const 可比的 = ctx.candidates.filter((c) => c.remainingEstimate !== null);
    if (可比的.length === 0) {
      return undefined; // 交给内核排序
    }
    const 选 = 可比的.reduce((a, b) =>
      b.remainingEstimate < a.remainingEstimate ? b : a);
    return { channelId: 选.channelId, reason: "剩余额度最少" };
  },
});
```

### 反例

```js
// 不要这样写：跨候选直接比
export default definePlugin({
  onRoute(ctx) {
    const 排好的 = [...ctx.candidates].sort(
      (a, b) => b.remainingEstimate - a.remainingEstimate);
    return { channelId: 排好的[0].channelId, reason: "剩余最多" };
  },
});
```

**它不会报错，只会静默地给出一个没有依据的决策。** 两种情况：

- 单位混合时全是 `null`，`b - a` 恒为 `0`，排序一动不动——插件却照样返回一个
  `reason`，报告一件它没有做过的事。
- 部分候选查不到额度时是 `null`，而 JS 里 `null` 参与算术会变成 `0`：那些渠道
  被当成「余量为零」，排到最前或最后，取决于相减的方向。

## 限额与宿主能力的失败

插件跑在限额之内：一次 hook 调用与**请求期的顶层求值**共用同一个上限
（默认 5 秒，见 `internal/plugin/sandbox` 的 `DefaultLimits`）。超了就失败，
插件拿不到结果。

宿主能力失败时，插件 `catch` 到的 `e.message` 是「**分类码: 固定文案**」，
例如 `unavailable: 读取插件 KV 失败`、`host_timeout: 宿主能力超时`。
**原文不会跨过来**——它可能带连接串、驱动细节或上游地址，只进宿主日志。
所以按码分支，不要按文案分支：

| 码 | 含义 |
|---|---|
| `bad_argument` | 你给的参数不合法 |
| `unavailable` | 宿主依赖不可用（存储、日历数据源等） |
| `host_timeout` | 这次调用的预算已经用尽 |
| `canceled` | 请求已经结束 |
| `misconfigured` | 集成方把能力表配错了（某个成员不是函数） |
| `capability_failed` | 宿主没给这个错误分类（默认拒绝），原文只在宿主日志里 |

`bad_argument` 与 `misconfigured` 的文案里会带上出问题的参数名或能力名，
那是插件本来就拿得到的东西；其余文案是固定的，别依赖它的措辞。

### 顶层代码

模块顶层会被求值**两次**，两次都是插件代码，两次都在限额之内：

1. **编译期**（上传/注册时）：读出 `meta` 与这个插件导出了哪几个 hook。
   这一次**没有注入任何宿主能力**，顶层调 `log(...)` 只会拿到
   `宿主能力未注入：这个插件运行在没有宿主能力的环境里`。
2. **请求期**（每次请求建 runtime 时）：这一次**能力已经注入**，所以顶层
   **可以**调宿主能力——它受的是限额的约束，不是「拿不到能力」。

顶层只该声明常量。一个死循环、一次慢的宿主调用，或者几十兆的分配，会让
**整个请求没有插件**（顶层跑不完就没有 runtime），而不是「这个 hook 失败」；
重活放进 hook 里。

## 怎么改

- **加字段就是扩大插件的可见范围**，要当成一次契约变更来做，而不是顺手加一个。
  上下文类型是独立的快照类型而不是内核类型的别名，正是为了让这件事必须显式发生。
- **补丁类型的字段表就是权限边界**。往 Patch 里加字段前先问：这个改动会不会改价、
  改身份、改「这次回答是否完整」的判定？会的话它不该出现在这里。
- 契约版本在 `meta.apiVersion`。改动语义（而不只是加字段）时它要跟着动。
