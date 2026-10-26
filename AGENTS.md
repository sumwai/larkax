# larkax 规则

本文件记录 larkax 的长期约定：**这个仓库是什么样**——代码与文档要求、红线，以及其余文档在哪。

**本机的 agent 协作协议不在这个仓库里。** 角色分工、派活与回报协议、评审怎么收敛，
都在工作目录下一个已 gitignore 的本机目录里，不随仓库发布。它们属于某个人的工作方式，
不属于项目资产；公开仓库里的约定只认本文件一处。协作者克隆这个仓库时拿不到那套协议，
也不需要——`AGENTS.md` 与 `docs/` 足以支撑单独工作。

开发中遇到的问题与解决方案按条拆在 [`lessons/`](lessons/)，根目录的 [LESSONS.md](LESSONS.md) 是索引。
**开始任何工作前先读索引**，按「什么时候读」那一列找到与手上的活相关的条目，再点编号进正文——
那里有踩过的坑，能避免重犯，但不必整份读一遍。

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
功能分支 → PR → CI → review → 合入 main
```

**CI 的门禁清单不写在本文件**——有哪些门、各自挡什么、本地怎么跑、契约测试为什么是
独立一个 job，见 [docs/development.md](docs/development.md) 的「CI 跑什么」一节。
**本仓库不做部署**：`main` 合入即是流水线的终点。发布由 tag 触发 `release.yml`——
门禁 → `make dist` → 把制品传到 GitHub Release。制品放到哪台机器上属于使用方的事，
不在本仓库范围内。

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
PR 描述按 [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md) 的几节写，
**用 `Closes #NNN` 关 issue**——编号只写在标题里不会关它。

## 测试与门禁

本地怎么跑、每道门挡什么、CI 跑哪些门、变异测试的分数怎么读，见
[docs/development.md](docs/development.md) 的「常用检查」「CI 跑什么」「变异测试」三节。
留在这里的只有四条判据：

- **改了哪个门禁脚本，就跑它自己的自测**（`scripts/check-*.test.sh`）。扫描器坏掉时的
  输出和「干净」一模一样，没有自测就无法证明它还在工作（见 [LESSONS.md](LESSONS.md) 第 2、7 条）。
- **「跳过」与「通过」在输出里长得一样**，本仓反复踩这个坑（见 [LESSONS.md](LESSONS.md)
  第 16 条）：缺 `LARKAX_DB_DSN` 时依赖数据库的用例整包 `t.Skip`，所以 CI 里另有一道门
  专门确认它们真的跑了，被 `t.Skip` 时报错。新增这类测试包时要往
  `.github/actions/check-integration-tests` 补上包名——那是这份清单的唯一来源。
- **测试通过不等于行为被钉住**：覆盖率只说明「这一行被执行过」。计费、账本、金额的
  改动要过变异测试。
- 白名单（`scripts/*-allowlist.txt`）的条目只许减不许增，**不要为了把它刷空而硬接一条
  假的调用方**——那比留着一条白名单更坏。

## 配置的分界线

新增配置项前先问：**它是不是必须在连上数据库之前拿到？**

- 是 → 环境变量，目前只有九个（数据库 DSN、主密钥、角色、日志级别、环境名、
  Redis 连接串、两个监听地址、控制台静态目录）。完整清单以 `internal/config` 的
  `Load` 为准。
- 否 → 数据库的 `settings` 表；密钥类配置也不进 `settings`——那里的值会明文落盘
  并进入备份，它们走 `channels` 表的信封加密。

**环境变量要少**：它必须跟着数据一起走，备份恢复、换机器、切环境时漏掉一个变量，
数据就可能整体不可用，而这类错误往往几周后才暴露；放在库里则与数据同生共死。
所以加一个要有明确的「必须在连库前拿到」的理由。加配置项的两步见
[docs/development.md](docs/development.md) 的「加一个配置项」一节。

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

**引一句失败文案时，要说清它属于哪一侧**：调用方拿到的响应体、我们自己的服务端
日志，还是上游那一侧。三者常常不是同一句话——同一类失败，响应体里可能是
`upstream call failed`，而点出原因的那一句只在日志里。引文挨着状态码写而不说归属，
读者会把它当成自己会收到的那一句。

**写「你会得到什么」时，要写出谁提供它**：不由网关提供的，把那个主体点出来——
留存期由清理命令兑现，幂等那一段等待的长度取决于调用方自己设的超时。只写性质
不写提供者，读者会把它当成网关的既有行为。

两条是同一件事：**文档里每个断言都要能指出它的来源**。

## 其他文档在哪

本文件不把别处的清单抄第二份：抄第二份的那一天，就是两份开始分岔的那一天
（见 [LESSONS.md](LESSONS.md) 第 29 条）。

| 要找什么 | 在哪 |
|---|---|
| 本地开发环境搭建、跑通一次请求 | [docs/development.md](docs/development.md) |
| lint 与门禁命令、每道门挡什么、变异测试怎么跑怎么读、各包现状 | [docs/development.md](docs/development.md) |
| CI 跑哪些门、契约测试怎么加一条、有哪些已知偏差 | [docs/development.md](docs/development.md) → [contract/README.md](contract/README.md) |
| 开发中踩过的坑（按条，含「什么时候读」） | [LESSONS.md](LESSONS.md) 索引 → [lessons/](lessons/) 正文 |
| 新开 issue、写 PR 描述的格式 | [.github/ISSUE_TEMPLATE/](.github/ISSUE_TEMPLATE/) 与 [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md) |
| 发布流程的操作细节 | [docs/versioning.md](docs/versioning.md) |
| 命令行输出的形状约定（颜色、表格、空清单怎么打） | [docs/cli-output.md](docs/cli-output.md) |
| 技术栈选型 | 本文件的「代码约定」一节 |

**唯一没写的是「选型理由」**，而且它**不该**写进本仓库：本文件的「文档约定」一节
写着仓库里的文档只写「是什么、怎么用、怎么改」，不写「为什么要这么做」的讨论过程。
选型理由属于那一类，留在仓库外。
