# larkax 规则

本文件记录 larkax 项目的长期约定：开发流程、协作规则、代码与文档要求。

开发中遇到的问题与解决方案记录在 [LESSONS.md](LESSONS.md)。**开始任何工作前先读它**，那里有踩过的坑，能避免重犯。

## 沟通语言

面向用户的输出用中文。代码、标识符、命令、文件路径、必须保留原文的技术术语（`new-api`、`moejs`、`relaykit`、hook 名等）保持英文。

## 公开仓库的红线

这是一个公开的开源仓库。**源码中不得出现任何非公开内容**，包括：

| 不得出现 | 说明 |
|---|---|
| 会话上下文 | 来自对话的临时信息、讨论过程、决策理由、需求原文 |
| 私有工具名与主机信息 | 本机私有工具的名称、端口号、路径、内部服务地址 |
| 密钥与凭证 | API Key、token、密码、私钥、webhook secret。**一律用占位符** |
| 真实上游信息 | 真实测试上游的域名、账号、模型清单、套餐细节 |
| 个人隐私 | 用户名、邮箱、真实姓名、任何可定位到个人的信息 |

占位符约定：

```
<API_KEY>          <CHANNEL_CREDENTIAL>     <WEBHOOK_SECRET>
https://api.example.com/v1                  user@example.com
test-key-placeholder                        localhost:5432
```

**提交前自查**：跑 `scripts/check-redlines.sh`（无参数扫暂存区，`--all` 扫全树）。
它按形态抓密钥、私有主机、本机路径与真实邮箱，比手写 grep 覆盖得全。
改了它本身就跑 `scripts/check-redlines.test.sh`——扫描器坏掉时的输出和「干净」一模一样，
没有自测就无法证明它还在工作（见 [LESSONS.md](LESSONS.md) 第 2、7 条）。

设计文档、调研笔记、规划过程**不进仓库**。它们留在仓库外的私有笔记区。仓库里的文档只写「是什么、怎么用、怎么改」，不写「为什么要这么做」的讨论过程与需求来源。

## 开发流程

GitHub Flow：`main` 受保护，只能通过 PR 合入。

```
功能分支 → PR → CI → review → 合入 main → 部署 staging
```

CI 目前包含：`gofmt` 检查、`go vet`、单测（含 `-race`）、覆盖率摘要、
迁移 up→down→up 往返（真 PostgreSQL）、红线扫描及其自测。
**集成测试、契约测试与自动部署尚未接入**，各自落地时补进这条流水线。

分支命名：`feat/`、`fix/`、`refactor/`、`docs/`、`test/`、`chore/` 前缀 + 简短描述。

**开分支后立刻确认父提交是 main**：

```sh
git switch main && git pull --ff-only && git switch -c feat/xxx
git log --oneline main..HEAD | wc -l    # 必须是 0
```

这一条是踩出来的，不是想出来的（见 [LESSONS.md](LESSONS.md) 第 11 条）。
在别人的分支上工作过之后就地开新分支，会把那个分支的未合并提交一起带进去，
表现为 PR 的标题与 diff 不符，也让合并难以回溯。**只靠记得检查是不够的**，
所以把它写成开分支的固定动作。

同理，`git branch -d` 批量清理时先看一眼列表里有没有当前分支——
`git branch --merged main | xargs git branch -d` 会把 `main` 自己也扫进去。

Commit 用 Conventional Commits。提交信息与 PR 描述里**不得出现会话上下文与私有信息**。

## 协作角色

用多个 agent 并行工作，至少包含两个专职角色：测试与 review。

**但不要每改一次就叫一轮。** 起一个 agent 要花几分钟、烧掉不少 token，而它做的大部分事
（跑 `gofmt`、`go vet`、`go test`、读 diff、看排版）谁都在做。所以：

| 改动性质 | 怎么做 |
|---|---|
| 日常小改（改文案、加个函数、修拼写） | 自己跑命令检查，不起 agent |
| 一次功能改动完成 | 自己检查；只有触及计费、账本、协议转换这类高风险区才起 review agent |
| 准备发新版本 | 起完整的测试与 review agent，此时不省 |

自己动手时，把下面两个角色清单当成自查表逐条过一遍——它们的价值在清单本身，
不在「由另一个 agent 来执行」。

### 测试角色

**日常开发**：跟随改动写针对性的测试即可，不要求全量 e2e。

**发布新版本时**：跑完整测试，包含：

- **端到端测试**：从客户端 SDK 到上游的完整链路（OpenAI 与 Anthropic 两套协议各一遍）
- **变异测试**：对核心计费与账本代码做变异测试。计费算错是这套系统最贵的 bug，变异测试能发现「测试通过了但逻辑是错的」这类问题

**扮演首次使用者**：测试时让 agent 扮演**第一次使用 larkax 的用户**，给他一个目标（例如「我要用 larkax 调 Claude 模型」），**不带任何已有上下文**，让他从文档开始自己摸索。任何让他卡住、需要猜、找不到的地方都是缺陷。

**老用户视角**：如果改动会影响既有用户的使用习惯（改配置格式、改参数名、改默认值、改界面路径），**另起一轮**让 agent 以老用户视角测试，确认不会造成困扰或静默的行为变化。

### Review 角色

Review 不只看代码逻辑。让 agent **扮演对项目一无所知的 reviewer** 审核 diff，检查：

| 维度 | 具体项 |
|---|---|
| 代码 | 逻辑正确性、边界条件、错误处理、并发安全、资源释放 |
| 可读性 | 命名是否达意、是否有不必要的复杂度、注释是否解释「为什么」 |
| 文档 | 与代码是否一致、示例能否直接运行、是否有过时描述 |
| 注释 | 是否准确、有无残留的调试代码与 TODO |
| 用词 | 措辞是否清晰、术语是否一致、有无中英混用不一致 |
| 格式与排版 | 空行、换行符、缩进、行尾空白、文件末尾换行、全角/半角标点 |
| Lint | 所有 lint 与格式化工具的输出必须干净 |
| 公开性 | 是否混入非公开内容（见上文红线） |

「让人看起来不顺眼」本身就是问题，要提出来。格式与排版的一致性是这个项目的一部分。

## 配置的分界线

新增配置项前先问：**它是不是必须在连上数据库之前拿到？**

- 是 → 环境变量。目前只有数据库 DSN、主密钥、角色、监听地址、日志级别。
- 否 → 数据库的 `settings` 表。

**环境变量要少。** 它必须跟着数据一起走：备份恢复、换机器、切环境时漏掉一个变量，
数据就可能整体不可用，而这类错误往往几周后才暴露。配置放在库里则与数据同生共死。

因此环境变量的门槛很高，加一个要有明确的「必须在连库前拿到」的理由。
密钥类配置（上游 API Key、支付凭证）也不进 `settings`——那里的值会明文落盘并进入备份，
它们走 `channels` 表的信封加密。

## 代码约定

- 后端 Go，遵循 `gofmt` + `go vet` + 项目 lint 配置
- 前端 TypeScript，遵循项目 ESLint + Prettier 配置
- 注释用中文或英文均可，但**同一文件内保持一致**
- 不提交生成的产物、构建缓存、编辑器配置、`.env` 文件

## 文档约定

仓库内文档只写：

- **是什么**：这个模块/接口/工具做什么
- **怎么用**：如何调用、如何配置、如何运行
- **怎么改**：扩展点在哪、约定的边界在哪

不写：需求来源、讨论过程、决策理由的来龙去脉、会话中产生的上下文。

面向外部使用者的文档（`docs.larkax.com` 的内容）单独维护，语气与结构面向「第一次接触这个项目的人」。

中文文档里的引号统一用「」，不用 ASCII 双引号。代码块内不受此限。

`README.md` 与 `.env.example` 用英文，其余文档用中文：前者面向国际使用者与
只想跑起来的人，后者面向参与开发的协作者。

## 待补充

以下内容随开发推进补入本文件：

- 具体的技术栈选型与理由
- 本地开发环境搭建步骤
- 测试与 lint 的具体命令
- 发布流程的操作细节

## 用子代理并行工作

主会话负责：拆解任务、定接口边界、集成、决策、管 PR。实现细节交给子代理——
把读文件、试错、跑测试吃进主会话的上下文，后续轮次就只能在被压缩过的摘要上
做判断，而判断质量会随之下滑。

| 什么样的活 | 谁做 |
|---|---|
| 开新包、重写已有包、跨多文件调研 | 子代理 |
| 定接口、写决策、合并 PR、修一行 | 主会话 |

**每轮开始时先规划**：把这一轮的活拆成**互不重叠的文件集**，让多个子代理
能同时开工，而不是串行等一个做完再想下一个。

给子代理的任务里必须写清楚：

- 要建/改的确切路径，以及**不要碰什么**
- 禁止任何改动 git 状态的命令（`add`/`commit`/`checkout`/`stash`/`switch`）
  与 `go mod tidy`——多个代理共用一棵工作树时，这些会互相破坏
- 只跑与自己目录相关的 `go build` / `go vet` / `go test` / `gofmt`
- **PR 的创建与合并由主会话做**，保证 GitHub 流程一致，
  也避免多个代理同时操作远端

## Lint

```sh
gofmt -l .                          # 格式
go vet ./...                        # 标准检查
golangci-lint run ./...             # 跨文件与语义检查（配置见 .golangci.yml）
go test ./...                       # 单测（有 LARKAX_DB_DSN 时连集成测试一起跑）

bash scripts/check-redlines.sh      # 公开红线：密钥、私有主机、本机路径
bash scripts/check-layering.sh      # 分层约定：谁 import 谁
scripts/check-mutation.sh           # 变异测试（改动计费/账本时）
```

`scripts/check-redlines.sh --all` 扫全树而不是暂存区。改了扫描器本身要跑
`scripts/check-redlines.test.sh`。

**集成测试需要 `LARKAX_DB_DSN`**。没设时它们会跳过，而**「跳过」在输出里和
「通过」长得一样**——所以 CI 里专门有一道检查确认它们真的跑了（见
[LESSONS.md](LESSONS.md) 第 16 条）。变异测试也依赖它：`internal/ledger/store`
被整包跳过后那一包等于没测，脚本因此会拒绝运行（见 [LESSONS.md](LESSONS.md)
第 17 条）。本地跑集成测试：

```sh
docker run -d --name larkax-pg -e POSTGRES_USER=larkax \
  -e POSTGRES_PASSWORD=placeholder -e POSTGRES_DB=larkax \
  -p 127.0.0.1:5432:5432 postgres:16-alpine

export LARKAX_DB_DSN="postgres://larkax:placeholder@127.0.0.1:5432/larkax?sslmode=disable"
go run ./cmd/larkaxctl migrate up
go test ./internal/ledger/store/
```

`golangci-lint` **只开有价值的检查器，不做「全部打开」**——一份报上百条无关警告的
报告，第二次就没人看了。配置里每一项都有它对应的理由。

版本上有两个坑，都实际撞过：

1. **用比 `go.mod` 更旧的 Go 构建的 golangci-lint 会直接拒绝运行**。
   release 二进制通常比当前 Go 旧，所以本地要用 `go install` 一份，
   CI 里用 `install-mode: goinstall`。
2. **它自带的 typechecker 也要够新**。旧版读不了新版 Go 的导出数据格式，
   报 `export data version N is greater than maximum supported version M`——
   这个错看起来像代码问题，其实是工具版本问题。

所以升级 Go 或 golangci-lint 时，两者要一起看。

## 变异测试

核心计费与账本代码的测试**必须**用变异测试验证。覆盖率只说明「这一行被
执行过」，不说明「这一行的每个分支都被钉住了」——把 `>=` 改成 `>`、
把分支方向反过来，覆盖率一点不变，而测试照样通过。

```sh
scripts/check-mutation.sh              # 全部包
scripts/check-mutation.sh money route  # 只跑指定的
```

**并发与超时系数是同一个旋钮，而并发是更该先调的那个。** gremlins 给每个
变异的超时预算 = 「跑一次测试集的时间 × 系数」，而那个基准是**串行**测出来
的；并发跑变异时机器上同时有 N 个 `go test` 在编译与执行，超时按墙钟算，
于是并发越高、越多变异被判超时。

实测（`internal/money`，系数固定 10）：

| workers | 结果 |
|---|---|
| 1 | `Killed 80, Timed out 0` |
| 2 | `Killed 80, Timed out 0` |
| **4** | `Killed 3, Timed out 92` |

所以**调大系数是在补并发造成的短缺**，而那会让「这个包需要多大系数」
变成一个随机器负载变化的**假属性**——同一份代码两次运行给出截然不同的
分数，一次「0.00% efficacy」看起来像真实结论。

`scripts/check-mutation.sh` 因此固定 `workers=1`、系数 10。串行慢，
但变异测试不是每次提交都跑的东西（见上文协作节奏），**慢而可信比快而误导好**。

### 一个分数要连着它的条件一起读

固定了 workers 还不够。同一个包在两种条件下给出的不是同一个数：

| 条件 | `internal/ledger` | Mutator coverage |
|---|---|---|
| `LARKAX_DB_DSN` 有效 | `Killed 132, Lived 11, Not covered 27` | 84.12% |
| `LARKAX_DB_DSN` 不存在 | `Killed 94, Lived 7, Not covered 69` | 59.41% |

**变的是「有多少变异体被测到」，不是「杀掉的比例」。** 两个 Test efficacy
几乎一样（92.31% 与 93.07%），差得多的是 Mutator coverage——所以
**只看 efficacy 会把这件事完全盖住**，要连着 Mutator coverage 一起看。

原因：gremlins 收集覆盖时用的是**整个模块**的测试集，而被测包只是其中一个。
`internal/ledger/store` 的集成测试要 `LARKAX_DB_DSN`，没设时整包 `t.Skip`，
于是它给 `internal/ledger` 贡献的那部分覆盖也没了——可测的变异体从 143 个
掉到 101 个，那 42 个从「被杀/存活」挪进了「不可达」。

修法是**把 DSN 变成硬前提**，而不是去收窄覆盖范围：脚本缺 `LARKAX_DB_DSN`
时直接拒绝运行。收窄成 `--coverpkg=./<pkg>/` 是错的方向——那是把「被别的包
测试覆盖到」的部分白白丢掉（可测变异体 143 → 101），数字看着稳了、信息量少了。

另一条来源是机器上的别的负载——它挡不住，只能按下面的判据识别：

| 包 | 机器空闲 | 有别的负载 |
|---|---|---|
| `internal/billing` | `Killed 145, Timed out 0` | `Killed 136, Timed out 10` |
| `internal/tokens` | `Killed 100, Timed out 0` | `Killed 95, Timed out 5` |

**判据是超时条数，不是百分比。** 超时不为 0 就说明这一轮受了干扰，
那几个数不与历史比较、也不抄进表里。

### 对存活的变异逐个判断

只有两类可放过：

- **等价变异**：改法与改后行为不可区分（例如 `f >= unit` 与 `f > unit`
  在「小数部分恰好填满一个单位」时结果相同）
- **不可达分支**：前置条件已经保证走不到，例如「商超出 int64 时它不可能为 0」

放过时在测试里写一行说明，否则下一个人会以为是漏了。

### 用常量测常量等于没测

`internal/quota` 里曾经写成

```go
if got := score(selfAccountingStaleAfter); got != baseScore { ... }
```

被测的阈值与喂进去的输入是同一个常量。把它从 15 分钟改成 15 秒，
用例照样通过——测的是「相对关系」而不是这个数。要钉住一个数就用字面量：
`score(14 * time.Minute)`。

## 各包的现状

由 `scripts/check-mutation.sh` 跑出（workers=1、系数 10、带 `LARKAX_DB_DSN`）。
**超时列不为 0 的包，这一轮的分数不与历史比较**——见上文。
改动过某个包的测试之后重跑并在表里更新。

| 包 | 覆盖率 | Test efficacy | 超时 |
|---|---|---|---|
| `internal/gateway` | 96.4% | 100% | 0 |
| `internal/pipeline` | 86.8% | 96.0% | 0 |
| `internal/quota` | 95.6% | 93.9% | 0 |
| `internal/calendar` | 97.8% | 93.6% | 1 |
| `internal/ledger` | 96.5% | 92.3% | 0 |
| `internal/ledger/store` | 79.8% | 90.2% | 1 |
| `internal/money` | 95.9% | 84.2% | 0 |
| `internal/route` | 93.1% | 81.2% | 0 |
| `internal/billing` | 97.5% | 81.0% | 0 |
| `internal/tokens` | 97.4% | 74.6% | 4 |
| `internal/snapshot` | 88.6% | 70.8% | 7 |

**覆盖率高而 efficacy 低，说明测试断言的是「相对关系」而不是「具体边界」。**
`route` 与 `snapshot` 两个包都是这个形状：原用例写的是「A 明显比 B 好」
这种比较，于是改 `<=` 为 `<`、去掉分母的加数、改常量，只要顺序不变就测不出来。

**不要为了把 efficacy 刷上去而写对着分支的琐碎断言。** 它会让数字好看，
但不会让行为更可信。`snapshot` 的遍历器那一层还没补，因为**它需要按功能
场景补而不是按函数补**（例如「一次含工具往返的请求走完整流程」）。
