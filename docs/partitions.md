# 分区

larkax 有两张表按时间分区，粒度不同，维护方式也不同。

| 表 | 粒度 | 分区键 | 谁预建 | 留存 |
|---|---|---|---|---|
| `usage_events` | 月 | `created_at` | `larkaxctl partition ensure`（外部调度器调用） | 无留存期，只追加 |
| `request_snapshots` | 天 | `created_at` | 写入路径自己按天补建 | 以天计，默认 30 天 |

## 为什么粒度不同

`usage_events` 是**只追加的财务底稿**：每一行记的是「这次结算按什么用量、哪个
时刻、哪条口径算出来的」。它没有留存期，所以不需要「按块删除」这个能力，
月粒度足够——一个月一张表，一年十二张。

`request_snapshots` 的留存期**以天计**。按天分区是因为清理靠 `DROP TABLE`：
整天的数据在一个分区里，到期直接删掉整张分区，不需要 `DELETE`。月分区做不到
这一点——它只能整月留或整月删，「30 天」就无从谈起。

## 兜底分区

两张表各有一个 `DEFAULT` 分区：`usage_events_default` 与
`request_snapshots_default`。

它按设计存在于 schema 里，作用是让**越界写入不会失败**。但落进兜底分区的行
有两个后果：

1. **没有到期日。** 它不属于任何按时间切出来的分区，所以「按分区整块删除」
   这条留存承诺对它不成立。
2. **它挡着那个时间段的分区建不出来。** 实测（PostgreSQL 16）：一旦有行落进
   `usage_events_default`，补建那个月的分区会直接失败：

   ```
   ERROR: updated partition constraint for default partition "usage_events_default"
          would be violated by some row        (SQLSTATE 23514)
   ```

   要人工先把那些行搬走，那个月的分区才建得出来。

所以**兜底分区非空就是告警**：它说明预建漏跑了，而且已经产生了不受分区约束的
数据。两张表的兜底分区都是这个口径。

## 谁预建

### `usage_events`：`larkaxctl partition ensure`

```sh
larkaxctl partition ensure [--months N]   建出缺失的月分区
larkaxctl partition check  [--months N]   只读体检，有问题时非零退出
```

`--months` 是「从当月起算几个月」，默认 **3**（含当月）。它决定提前量：给 1
只保证当月，预建任务停摆超过一个月，下一个月的分区就不存在了；给 3 留出两个月
的缓冲。

`ensure` 打印它**真正建出来**的分区名，一行一个；没有缺失时打印一行
「没有缺失的分区，本次没有建任何东西。」——「一天都没建」必须与「根本没跑成」
区分得开，调度器日志里才有得看。

`check` 只读，检查三件事，任何一件不满足就非零退出，并把原因打到 stderr：

1. `usage_events` 缺哪些应当存在的月分区；
2. `usage_events_default` 的行数（非 0 即异常）；
3. `request_snapshots_default` 的行数（同上）。

输出里带上表名与行数。`check` **不建**任何东西：体检与处置分开，调度器才能
在「只是看一眼」与「动手改库」之间自己选。

写入侧也会保证**当月**那一个分区存在（见下），所以 `check` 报出「缺月分区」
说明预建任务漏跑了，而不是写入路径坏了。

### `request_snapshots`：写入路径按天补建

这张表**没有**预建命令。`internal/snapshotwire/store.go` 在写入一条快照之前
按当天补建分区（`CREATE TABLE IF NOT EXISTS ... PARTITION OF ...`），带
「今天已确认过」的进程内缓存，所以热路径上一天只查一次。

这么安排是因为快照的写入频率高、且必须落进当天分区才能兑现留存承诺；
而 `usage_events` 的写入方是结算，那条路径上做 DDL 的代价要大得多——所以它
只补当月一个，其余交给调度器。

### 写入侧的当月补建

`internal/ledger/store` 的 `Store.Save` 在开事务**之前**调一次
`EnsureUsageEventPartitions`，只保证当月。

- **必须在事务之外**：`CREATE TABLE ... PARTITION OF` 会拿父表的
  `ACCESS EXCLUSIVE` 锁，在结算事务里做会把锁持有到提交，那等于给所有并发的
  结算排一次队。放在 `BeginTx` 之前，锁的持有期只有那一条语句。
- **进程内缓存「这个月已经确认过」**：没有缓存就是每个结算一次 DDL。
- **补建失败不让结算失败**：那一行会落进兜底分区，账还是记下来了——而那正是
  `check` 要报出来的事。失败按 ERROR 级别记一条日志，且**不写缓存**，下次
  结算还会再试。

## 接调度器

`ensure` 与 `check` 都是给外部调度器用的：**退出码就是告警信号**，0 表示
这一次没有问题。这与 `larkaxctl ledger reconcile` 的约定一致（见
[docs/reconciliation.md](reconciliation.md)）。

建议两条独立的调度：一条定期 `ensure` 铺满未来若干个月，一条定期 `check`
并在非零退出时告警。分成两条是因为它们的动作不同——一条改库、一条只读，
混在一起时「体检发现了问题」会顺手被同一条命令修掉，而告警也就不会响了。

`cron`：

```cron
# 每天 03:17 铺满未来 3 个月
17 3 * * *  /usr/local/bin/larkaxctl partition ensure --months 3
# 每小时体检一次，非零退出时告警
23 * * * *  /usr/local/bin/larkaxctl partition check --months 3 || /usr/local/bin/notify-oncall
```

Kubernetes `CronJob`：

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: larkax-partition-ensure
spec:
  schedule: "17 3 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: partition-ensure
              image: <LARKAX_IMAGE>
              args: ["partition", "ensure", "--months", "3"]
              envFrom:
                - secretRef: {name: larkax-env}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: larkax-partition-check
spec:
  schedule: "23 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: partition-check
              image: <LARKAX_IMAGE>
              args: ["partition", "check", "--months", "3"]
              envFrom:
                - secretRef: {name: larkax-env}
```

### 为什么告警走退出码，不接 `/metrics`

`internal/observe` 的指标面是**手写计数器、进程级、不查库**的（见
`internal/observe/metrics.go` 的文件头）。「兜底分区非空」是库里的状态，
要把它做成指标就得在抓取路径上查库——那是另一件事，牵扯到抓取超时、连接池
占用与缓存口径，不该顺带塞进这一条。

退出码这条路已经够用：调度器本来就盯着它，而 `check` 的输出里带着表名与行数，
足够定位。

## 怎么改

- **分区逻辑**：`internal/ledger/store/partition.go`。导出的是
  `EnsureUsageEventPartitions` 与 `MissingUsageEventPartitions`，两个调用方
  （写入侧与命令行）共用同一份实现。
- **命令行**：`cmd/larkaxctl/partition.go`。
- **快照的按天补建**：`internal/snapshotwire/store.go`。
- **表结构**：`internal/migrate/sql/`。分区与兜底分区建在 `00001_init.sql` 里。

`internal/migrate/sql/00001_init.sql` 里 `usage_events` 那段写着「分区由
worker 定时预建，这里只铺种子」——**那个 worker 就是这个命令**，由外部调度器
调用。那句话不改（goose 不做校验和比对，重写已应用的迁移会让老库与新库静默
分叉），所以在此处点明它指的是什么。

改分区粒度时要注意的：分区名是**拼进 SQL 的字符串**（标识符不能参数化）。
`UsageEventPartitionName` 只产出 `[a-z0-9_]`，边界是 RFC3339 时间串，
所以没有注入面——加新的命名规则时这条要跟着成立。

## 已知的边界

- **分区存在但边界不对，`ensure` 认不出来。** 它按名字判断缺不缺，不比对
  分区实际的 `FOR VALUES`。名字对而边界错的分区会让那个月的行落进兜底分区，
  而那会被 `check` 的第 2 项抓到——只是报出来的是「兜底分区非空」，不是
  「分区边界错了」。
- **写入侧只补当月。** 落库的 `created_at` 取的是数据库的 `now()`，而补建
  按应用算出的月份走。两者在 UTC 月末的零点附近可能差一个月，那时那一行会
  落进兜底分区。窗口极小，而兜底分区非空是告警——所以这件事不会静默。
