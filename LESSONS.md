# LESSONS

开发中遇到的问题与解决方案，**一条一个文件**，放在 [`lessons/`](lessons/) 下。
**开始任何工作前先读下面这张索引**，按「什么时候读」那一列找到与手上的活相关的条目，
再点编号进正文——不必把全部条目读一遍。

编号是稳定标识符：**文件名前缀就是编号**，代码、脚本、CI 与文档里的
「`LESSONS.md` 第 N 条」按编号引用它们，所以**编号不重排**，新增条目往后编号。

格式：每条记录 `现象 → 原因 → 解决`，并标注发现日期。已失效的条目不要删除，
标注「已过时」并说明取代它的是什么。

---

## 索引

| # | 主题 | 什么时候读 | 状态 |
|---|---|---|---|
| [1](lessons/01-private-content-in-repo.md) | 仓库中混入了非公开内容 | 往仓库里加文档、注释、示例或任何文本时 | 活 |
| [2](lessons/02-grep-combined-flags-silent-miss.md) | `grep -nP` 组合标志导致检查脚本静默漏报 | 写或改检查脚本、排查「脚本报通过但手工能命中」时 | 活 |
| [3](lessons/03-env-vars-decouple-config.md) | 环境变量过多会让数据与配置脱钩 | 加环境变量或配置项时 | 活 |
| [4](lessons/04-readiness-empty-checks-503.md) | 就绪检查在无检查项时返回 503 | 写「全部满足」这类判定的空集合分支时 | 活 |
| [5](lessons/05-goose-down-to-off-by-one.md) | goose 的 down-to/up-to 会停在目标版本的相邻位置 | 包装第三方命令、写迁移与回滚逻辑时 | 活 |
| [6](lessons/06-doc-recipe-vs-example-file.md) | 文档推荐的做法配着自己的示例文件必然报错 | 写文档里的命令、改 `.env.example` 这类示例文件时 | 活 |
| [7](lessons/07-redline-line-filter-bypass.md) | 红线扫描按整行过滤，会让真密钥搭占位符的便车溜过去 | 改红线扫描器，或写任何按行过滤的白名单时 | 活 |
| [8](lessons/08-config-struct-prints-secrets.md) | 配置结构体打印出来就是明文密钥 | 定义持有密钥的结构体、加日志字段时 | 活 |
| [9](lessons/09-fake-keys-blocked-by-push-protection.md) | 自测样本里的假密钥会被 GitHub 当成真密钥拦下 | 写扫描器样本、首次推送仓库时 | 活 |
| [10](lessons/10-all-flag-tracked-only-blind-spot.md) | `--all` 只扫已跟踪文件，新增文件成了自查盲区 | 改自查工具扫描的文件范围时 | 活 |
| [11](lessons/11-branch-off-foreign-branch.md) | 在别人的分支上开新分支，把两个改动并进了一个 PR | 需要切到别人的分支复现问题时 | 活 |
| [12](lessons/12-shape-allowlist-false-positives.md) | 形态匹配的白名单会被代码反复误报 | 往形态白名单里加放行模式时 | 已被[第 13 条](lessons/13-scope-by-file-type.md)取代 |
| [13](lessons/13-scope-by-file-type.md) | 按文件类型限定检查范围，而不是继续补形态白名单 | 一个检查规则反复误报、想再加一条例外时 | 活 |
| [14](lessons/14-planning-numbers-leaked.md) | 规划期的编号泄漏进了源码与文档 | 写注释、issue 标题、文档措辞时 | 活 |
| [15](lessons/15-tuning-params-to-fit.md) | 用调参数的方式去适配一个解释不通的现象 | 想给某个包或某次运行单独调参数时 | 活 |
| [16](lessons/16-silent-check-failure.md) | 检查工具「没跑成」时看起来和「跑成了」一模一样 | 写检查脚本、加 CI 门禁、看到工具报「0 issues」时 | 活 |
| [17](lessons/17-number-copied-without-conditions.md) | 一个数抄进文档时，条件被丢掉了 | 把实测数字写进文档或表里时 | 活 |
| [18](lessons/18-git-operations-that-look-right.md) | git 里两处「看着对了」的操作 | `git add` 之后又改了文件、用 `checkout --ours` 整文件取一侧时 | 活 |
| [19](lessons/19-fixture-impossible-data.md) | 测试夹具造出生产不可能出现的数据，新判据一上来就命中它 | 给某一列加新判据、写测试夹具时 | 活 |
| [20](lessons/20-checklist-not-a-linter-run.md) | 按清单自查不能替代真的跑一遍 linter | 报告里要写「lint 干净」时 | 活 |
| [21](lessons/21-invariant-proves-nothing-about-tier.md) | 「没撞上界」测不出「用了哪一档」 | 写分时 / 分档定价的断言、看到不变式断言时 | 活 |
| [22](lessons/22-implemented-tested-ungated.md) | 有实现、有测试、没有门 | 写完一个库构件、报告里要写「已完成」时 | 活 |
| [23](lessons/23-untested-guard.md) | 一个守卫没人钉住，等于它没被证明在工作 | 给检查脚本写自测时 | 活 |
| [24](lessons/24-defect-in-the-seam.md) | 两处各自有测试，缺陷长在两处之间的缝里 | 同一件事在两处各做一次时 | 活 |
| [25](lessons/25-each-branch-green-merge-red.md) | 两条分支各自的 CI 都绿，合起来不绿 | 一条分支收紧或新增必填约束、准备合并时 | 活 |
| [26](lessons/26-test-schema-damage-shared-db.md) | 测试在共用的库上做 schema 级破坏，把自己后面的用例全毁了 | 写会连数据库的测试时 | 活 |
| [27](lessons/27-control-arms-shared-channel.md) | 对照实验的两臂共用了一条没检查的通道 | 做对照实验、给实验用的资源起名字时 | 活 |
| [28](lessons/28-limit-covers-one-segment.md) | 限额覆盖了「一次调用」，而插件代码有三段 | 给插件或沙箱加限额、改 ctx 的生命周期时 | 活 |
| [29](lessons/29-doc-check-not-in-ci.md) | 文档里写着、而 CI 没跑的那条检查，等于没跑 | 往文档里加命令、往 CI 里加步骤时 | 活 |
| [30](lessons/30-empty-loop-assertion-passes.md) | 遍历一堆东西的断言，东西为空时全部静默通过 | 写「遍历一堆东西逐个断言」的用例时 | 活 |
| [31](lessons/31-struct-field-no-gate.md) | 结构体字段的死活，三道门都照不到——而「有没有读者」不是判据 | 判断一个结构体字段能不能删时 | 活 |
| [32](lessons/32-shared-function-two-contracts.md) | 共用函数的判据改了，另一条调用路径还钉着旧行为 | 改一个函数的判据（不只是实现）之前 | 活 |
| [33](lessons/33-fixture-shape-not-pinned.md) | 夹具按想象造，真实形状不同——而「行为碰巧一致」不会让任何人发现 | 写「不认识就忽略」这类处置的用例、按文档造夹具时 | 活 |
| [34](lessons/34-gate-is-not-overlap.md) | 并发用例只加闸门，不保证 goroutine 真的重叠 | 写并发/竞态用例时 | 活 |
| [35](lessons/35-hardcoded-maximum.md) | 用例里写死的「当前最大值」，会在它增长时无理由地红 | 断言里要写一个由仓库自身增长的数时 | 活 |
| [36](lessons/36-test-entry-in-production-file.md) | 给用例留的入口写在生产文件里，deadcode 判它没人调 | 为用例加一个包装、为测试抽一段共用逻辑时 | 活 |
| [37](lessons/37-migration-down-names-the-latest.md) | 用「退最近一次迁移」验回滚的用例，会被下一条迁移悄悄换掉对象 | 写「回滚最近一次」的用例、往仓库里加迁移时 | 活 |
| [38](lessons/38-migration-number-allocated-upfront.md) | 迁移编号靠「先发后写」，扫盘面看不见已分配但未落盘的号 | 往仓库里加迁移、同时有多条分支要加迁移时 | 活 |
| [39](lessons/39-wording-floor-of-shared-strings.md) | 一个字符串被两个入口共用时，措辞下限由最窄的那个入口决定 | 写一句会经多个入口返回给使用者的话（错误、提示）时 | 活 |
| [40](lessons/40-spec-says-intent-not-state.md) | 任务书写的是意图不是现状，建对象之前先核对库 | 写迁移建表/建索引/加列、按任务书去建一个可能已经存在的东西时 | 活 |
| [41](lessons/41-same-name-masking-can-be-removed-elsewhere.md) | 同名掩护可以被一次与它无关的重构拿掉，门禁在 main 上才红 | 门禁突然冒出一条旧符号、改共用输出函数的名字时 | 活 |
| [42](lessons/42-design-context-does-not-belong-in-source.md) | 设计上下文（需求来源、讨论过程、「本轮范围」）会被顺手夹带进源码，而它读起来像一条正常的注释；脚本只判得了「面向使用者的文本」那一半 | 写注释、文档、帮助文本，或派 review agent 时 | 活 |
| [43](lessons/43-equivalence-claim-needs-distinguishing-input.md) | 变异存活的分诊：把「我想不出用例」当成「没有输入能区分」；同一比较器的两支共用一句理由 | 判变异存活、写等价标注、派分诊或评审任务时 | 活 |
| [44](lessons/44-insert-splits-doc-comment.md) | 新函数插在前一个函数的注释块与它的 `func` 之间，那段注释就归了新函数 | 往已有文件里插入一个新函数（用例也算）时 | 活 |
