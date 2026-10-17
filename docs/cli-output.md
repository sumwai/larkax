# 命令行的输出

`larkaxctl` 的输出只有两种读者：**人**（在终端里扫一眼）与**别的程序**
（`| grep`、写进文件、被脚本读走）。这一节写这两边都照顾到的几条约定，以及
加一个新的检查项、新的清单时要照什么形状写。

## 状态符

诊断类命令（`doctor` 这一类）每项一行，行首是状态符：

| 符号 | 含义 |
|---|---|
| `✓` | 通过 |
| `✗` | 不通过：查了，有问题 |
| `!` | 没跑成：依赖没满足，这一项**没查** |
| `-` | 没跑 / 不适用：这一项本来就不在这次范围内（例如 `doctor` 没给 `--live`） |

**`!` 与 `-` 是两件事，`!` 与 `✓` 更是。** 「没跑成」与「通过」在安静的终端里
长得一样，是这个项目反复踩的坑（见 [LESSONS.md](../LESSONS.md) 第 16 条），
所以它们各有各的符号，也各自影响退出码。

体检类命令（`doctor`、`partition check`、`ledger verify`、`secret rotate
--verify`）共用同一条渲染路径 `printStatusLines`（`cmd/larkaxctl/style.go`）：
每一项一行「状态符 标题 一句结论」，标题那一栏按最宽的那个对齐，细节只在 `-v`
时跟着各自那一行缩进打出来。默认一屏收敛的是**篇幅**，不是信息量——原先那些
长句一条都没删，只是挪进了 `-v`。

调用点登记在 `cmd/larkaxctl/renderpaths_test.go` 的
`TestEveryStatusRenderPathIsRegistered` 里：加一条同类命令、或者某条命令不再走
这条路，那张表对不上就红。**别在调用点自己拼一行状态符**：那样它就不在这条
计数里，颜色也绕过了下面那一节说的唯一出口。

## 颜色什么时候出现

三条里任何一条成立就**一个 ANSI 转义序列都不输出**：

- 输出目标不是字符设备（重定向到文件、接进管道、被别的程序读走）；
- `NO_COLOR` 非空（<https://no-color.org>）；
- `TERM=dumb`。

不上色不是「颜色变成透明」，而是那几段字节根本不存在：`| grep` 与写进文件的
输出里不会夹着控制码。

样式只有三种：关键实体（表头、主键、出错的对象）加粗，辅助描述（版本号、耗时、
时间戳）置灰，失败项的状态符是加粗红。都收在 `cmd/larkaxctl/style.go` 里，
**调用点不要自己拼转义序列**：转义序列是唯一样式出口 `palette` 的产物，自己拼
就绕过了上面那三个判断，`| grep` 里会夹进控制码。这一点按字面量钉在
`renderpaths_test.go` 的 `TestOnlyThePalettePaints` 里（用法文本那几段是例外：
它们举例子解释输出长什么样，自己不打印）。

## 表格按显示宽度对齐

`newTableWriter`（`cmd/larkaxctl/table.go`）按**显示列**补空格：汉字、假名、
谚文与全角标点算两列。样式码不占列，所以先上色再对齐与先对齐再上色结果一样。

不要改回标准库的 `tabwriter`：它按 rune 数补空格，同一栏里既有 `ID` 又有
`采集源 ID` 时，补出来的 rune 数一样、看起来却差好几列。

走表格的调用点登记在 `renderpaths_test.go` 的
`TestEveryTableRenderPathIsRegistered` 里，每条路径的样本喂给
`style_test.go` 的 `TestListTablesStyleWithoutChangingLayout`（后者比对「上色
那一档剥掉样式码之后与不上色那一档逐字相同」）。

## 加一个检查项

`doctor` 的一项是一个 `checkDoctorXxx`，返回 `doctorCheck`：

1. 在 `doctor.go` 里加一个 `checkDoctorXxx`，返回 `doctorCheck`；
2. **通过时填 `ok`**：那是默认输出里跟在标题后面的一句。只有查的人知道该写什么
   （迁移版本、几个可用渠道、几组能解析出上游名），从 notes 里反推是猜；留空会
   打出一句笼统的「通过」；
3. **有问题时填 `findings`**，每条拆两截：
   - `headline` 说清「哪个对象、坏在哪里」，进默认那一行；
   - `detail` 写后果与排查方向，`-v` 时才打。

   两截都不许省：默认那一眼要的是对象与根因，出问题时人要的是下面那句为什么。
4. **没查成时填 `skip`**，并且报「没跑成」而不是「通过」；
5. 细节（检查范围、读到的数、表）放 `notes` 与 `table`，它们只在 `-v` 时打。

默认那一行是 `状态符 标题 结论`，标题那一栏按最宽的那个对齐。一项超过一条问题时
默认只打第一条，其余的折成「（另有 N 条，用 -v 看）」。

`doctor` 之外的体检命令（`partition check`、`ledger verify`、`secret rotate
--verify`）自己收一组 `statusLine` 交给 `printStatusLines`，不要另起一套渲染：
状态符、对齐、`-v` 缩进、以及那句「N 项异常、N 项通过」的汇总都从这条路出来。
汇总那一句要用 `countStatusLines` 从**打出去的行**里数，不要从上游的数据结构再
数一遍——两处各数一遍时，多打或少打一行就会让那句汇总说谎。

## 加一个清单命令

- 一律**固定列宽的表格**，不要拼长单行 `key=value`：字段一多，长行会在窄终端里
  自己折行，折出来的行与下一条记录分不开。新加一条走表格的路径要登记进
  `renderpaths_test.go`（见上面「表格按显示宽度对齐」那一节）。
- 表头用中文；主键与核心标识加粗，时间戳一类的次要属性置灰。
- 时间戳**截到分钟**（`01-02 15:04`）：清单回答的是「哪一条最近动过」，整条
  RFC3339 会把一栏撑到 25 列。要精确到秒的时候看单个实体的回显，它仍然打完整时刻。
- 空清单打一句 **`暂无<实体>。可使用 larkaxctl <子命令> 添加。`**，不要什么都不
  输出：空清单与「命令没跑起来」在安静的终端里长得一样。这句模板只有
  `style.go` 的 `emptyListNote` 一处，调用点传实体名与添加它的子命令；
  **没有「添加」这个动作的清单不走它**（给不出可执行的下一步时硬造一个命令名
  更坏，例如 `ledger balance --all` 的科目由分录产生）。逐条钉在
  `style_test.go` 的 `TestEmptyListMessages` 里，模板的唯一性钉在
  `renderpaths_test.go` 的 `TestEmptyStateWordingHasASingleSource` 里。
- 一个实体**只点一次身份**：写名字就不挂 ID（`用户 alice`，不是
  `用户 alice(id=7)`）——名字已经认出这条记录了，括号里的 ID 只是让按名字读的人
  多跳一次。这条命令自己创建的那个对象上，ID 单独成一个字段
  （`已创建 API Key id=5 prefix=… 用户=alice`），方便脚本接着用；行里其余的位置
  仍然只写名字。挂括号的写法由 `TestNoParenthesisedIDInHumanOutput` 按字面量钉住。

`settings list` 是例外：settings 表是空的与「还没有配过」不是一回事（那些键来自
Registry，加不了），它留着自己的说法。

## 安静模式与详细模式

`-q` / `--quiet` 与 `-v` / `--verbose` 是两个**全局选项**，和 `--format` 同一档：
写在子命令前面或后面都认。前者压掉输出，后者加细节。

`--quiet` 压掉的是**「给人看」那一档的全部输出**：stdout 上的结果表、stderr 上的
提示与警告，都不再打。它要的是「这一次只管退出码」——放进 cron 或 CI 里跑的一串
`larkaxctl`，输出没人看，攒起来只会淹掉真正的报错。

压掉的边界有三条：

- **致命错误照旧输出**。命令返回的那个错误不归它管，仍然打在 stderr 上、退出码
  照旧。压掉它就等于让失败也一声不吭，那是这个项目反复防的那类问题
  （见 [LESSONS.md](../LESSONS.md) 第 16 条）。
- **`--format json` 不受影响**。机器可读那一档是**另一个**选项要的，两个一起给
  时安静模式只压掉「给人看」的那份。`larkaxctl model list -q -o json` 仍然在
  stdout 上给一份完整的 JSON。
- **退出码不变**。`-q` 只影响写不写，不影响算什么。

`--verbose` 是它的反面。两个一起给报参数错（退出码 2）而不是「后者赢」：它们要的
是相反的东西，安静地采纳一个会让另一个看起来没生效。详细模式目前由 `doctor` 与
`partition check` 用（每一项多打一段细节），其余命令接受它但不使用。

## 机器可读输出

上面几节说的都是「给人看」那一档。给别的程序读的那一档是 `--format json`
（`-o json` 是同一个选项的简写）：

```sh
larkaxctl doctor --format json | jq -r '.checks[] | select(.status != "passed") | .name'
larkaxctl channel list -o json | jq -r '.channels[] | select(.status == "enabled") | .id'
```

它是**全局选项**，写在子命令前面或后面都认（`larkaxctl doctor -o json` 与
`larkaxctl -o json doctor` 一样）。不给就是 `table`，也就是前面几节说的那一档。
取值不认识时报参数错（退出码 2），**不静默回落成表格**——回落的表现是脚本拿到
一段文本，而报错的是下游的 `jq`，看日志的人会去查 jq 的用法。

### stdout 的契约

- 指定 `--format json` 时，stdout 上**恰好一份 JSON 文档**，以一个换行结尾。
  没有提示、没有前缀、没有 ANSI 转义，也没有表格。
- 提示与警告一律走 stderr（例如 `settings list` 那句「警告：这套配置服务加载
  不起来」）。管道下游读的是 stdout，混进一行提示就会让 `jq` 报错。
- 命令在**产出结果之前**就失败时（连不上库、参数错），stdout 是**空的**，
  错误在 stderr、退出码非 0。这一条与上一条不矛盾：那份文档回答的是「这一次
  查到了什么」，而那时还没有「查到了什么」。**接管道的一方要先看退出码**。

顶层形状对所有子命令一致：

```json
{
  "status": "ok",
  "summary": { "total": 6, "passed": 4, "failed": 1, "skipped": 1 },
  "checks": [ ... ],
  "exit_code": 0
}
```

- `status` 三档：`ok`（全部通过）、`failed`（至少一项查出问题）、
  `skipped`（一项都没查出问题，但有项目**没跑成**）。`skipped` 不是 `ok` 的
  近义词，理由与状态符那一节完全相同。
- `summary` 是这一档的计数，没有可数的东西时这个键不出现。
- 条目数组的键名是英文复数（`checks`、`channels`、`models`、`rules`……），
  一屏看不完的那种。
- `exit_code` 是**这一次运行该用的退出码**。

### 退出码

**与交互模式完全一致**，脚本基于退出码的逻辑不因为换了输出格式而变：
`doctor` 有失败项时 json 模式仍然退出 1，连不上库时仍然是 3。
`exit_code` 那一栏与进程真正退出的码是同一个数（从同一个错误算出来）；
两者万一不一致，以**退出码**为准——它是命令行对外的老契约。

### 值的约定

- **金额是十进制定点字符串**（`"12.34000000"`），不是 JSON 数字：过一遍
  `float64` 会丢精度，而丢掉的恰好是账单要精确到的那几位。
- **时间是 RFC3339 字符串，按 UTC 打**（`2026-10-17T01:02:03.001Z`），
  不截到秒——截到分钟是表格那一档的排版需要，不是数据的精度。没有值时是
  `null`。库里是无时区的 `date` 列（模型的知识截止、发布日期）照 `2006-01-02`
  打，给它补一个时区等于凭空编一个事实。
- **缺值一律是 `null`，不打占位符**。表格里的 `-`、`不过期`、`从未`、`平台`、
  `继承` 一个都不进 JSON：`"expires_at": "不过期"` 会让按时间解析它的一方拿到
  一个解析不了的值。「平台自营」是 `"owner_user_id": null`，不是 `0`。
- **空清单是 `[]`**，不是 `null`，也不是那句「暂无渠道。可使用……添加。」——
  后者是给终端里的人的。
- 库里本来就是 JSON 的列（渠道的 `setting`、别名的 `overrides`、快照的
  `skeleton`）原样放进去，不再包一层。

### 时刻与时区

**一条规矩：机器读的时刻一律 UTC，终端显示的时刻按本机时区。** 同一个瞬间
在两个地方需要不同的写法，这不是不一致——两条路各自服务一类读者。

| 路径 | 谁在用 | 怎么打 | 为什么 |
|---|---|---|---|
| `--format json` | 脚本、探针（`\| jq`） | `jsonTime` / `jsonTimePtr` / `jsonTimeOrNull`（`cmd/larkaxctl/format.go`），恒 `t.UTC()`、精度到纳秒 | 同一行在两台机器上必须编出**同一个**字符串，比对、缓存键、对账的一方不会去猜写它的机器在哪个时区 |
| 终端表格 / 状态行 | 在终端里扫一眼的人 | 各命令自己的 `Format(time.RFC3339)`，按进程时区 | 「这是几点」按本机时区读才是对的 |
| 内部观测面的 HTTP JSON | 别的程序 | `internal/api/ratelimitusage.go` 的时间栏、`internal/plugin/adapter.go` 给插件的 `resets_at`，两者恒 `.UTC()` | 同上：机器读的不能带本机时区 |

**为什么这件事值得单列一节**：库里那几列是 `timestamptz`，而 pgx 交回来的
`time.Time` 带的是**进程时区**（`time.Local`）——同一行在一台 UTC 的机器与
一台 +08:00 的机器上，`t.Format(time.RFC3339)` 出来是两个不同的字符串。CI
默认跑在 UTC，本地开发机也常是 UTC，于是这类偏差**不会自己冒出来**：修的是
被发现的实例，不是这一类。

所以出 JSON 的时刻只有 `jsonTime` 一条路，**别在任何一处再手工 `Format`**：
多一处就多一处要记得带 `.UTC()` 的地方。`cmd/larkaxctl` 里这条规矩由
`TestJSONTimeIsUTCWhateverTheProcessZone`（`format_test.go`）钉住——它在
`TZ=Asia/Shanghai` 下会显形、在 UTC 下不会，`gates.yml` 因此单开一步在非 UTC
时区下把受影响的包再跑一遍，并先证明那个时区真的生效了（tzdata 缺失时
`time.Local` 会静默回落成 UTC，那样的绿是假的）。

### 哪些命令接上了

**只读类**（`list` / `get` / `resolve` / `explain` / `balance` / `verify` /
`check` 以及 `doctor`、`version`）已经接上。**写类子命令还没接**：给它们
`-o json` 会报参数错，而不是打出一段表格——写类命令那时事情已经做完了，
脚本却只看到一句解析错误，于是重跑一次。

几个读起来像只读、但同样还没接的：

| 命令 | 为什么 |
|---|---|
| `migrate status` / `migrate version` | 输出是 goose 自己往 stdout 打的，要接 JSON 得先把它那份 logger 收住 |
| `secret rotate --verify` | 与 `secret rotate --apply` 共用同一条命令，先按整条命令拒掉 |
| `snapshot redact` | 它的 stdout 本来就是产物本身（脱敏后那段骨架的 JSON），套不上一层信封 |

### 加一条机器可读输出

1. 写一个**纯函数** `xxxDoc(数据…) map[string]any`：不连库、不写输出，
   只把已经拿到的数据折成文档。放在那条命令的打印函数旁边。
2. 返回值用 `jsonDoc(status)` 起头，往里加 `summary` 与条目数组。
   `exit_code` **不要自己填**：`emitJSON` 从「该用的那个错误」算出来。
3. 在调用点并排加一条分支，`printXxx` 那一行一个字都不要改：

   ```go
   if jsonOutput() {
       return emitJSON(os.Stdout, xxxDoc(data), cause)
   }
   printXxx(os.Stdout, data)
   return cause
   ```

   **不要**把表格转成 JSON：转出来的金额是字符串、缺值是占位符，而这两件事
   正是这一档最不能有的。
4. 写类等还没接的命令，在函数体**第一行**、连库之前
   `if err := jsonUnsupported("<命令名>"); err != nil { return err }`。
   放在写库之后就成了「先把事情做完，再报不支持」。
5. 用例一律 `json.Unmarshal` 之后断言字段，**不靠人眼看**；空清单、`null`
   的那几栏、以及退出码各钉一条。命令级再补一条：跑一遍真命令，断言 stdout
   整份能被 `json.Unmarshal` 收下且不含 `\x1b`。
