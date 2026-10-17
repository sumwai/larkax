# 26 · 测试在共用的库上做 schema 级破坏，把自己后面的用例全毁了

**编号** 26 · **日期** 2026-10-17 · **摘要**：删掉当月分区再恢复，在多进程共用库下恢复会失败，而后果是别人的用例挂。

**现象**

`usage_events` 按月分区，兜底分区 `usage_events_default` 接着所有落不到具体月份的
行。分区预建（#247）的用例要证明「写入侧会补建当月分区」，于是它**删掉当月分区**
再跑一次结算，用 `t.Cleanup` 恢复。

单跑那两个包时全绿。跑 `go test -race ./...` 时挂了一片：

```
--- FAIL: TestPartitionEnsureReportsWhatItCreated
    ensure 失败: 建分区 usage_events_2026_10 失败:
      ERROR: updated partition constraint for default partition
      "usage_events_default" would be violated by some row (SQLSTATE 23514)
--- FAIL: TestSaveEnsuresUsageEventPartition
    这次结算有 1 行落进了 usage_events_default
```

**根因不是实现，是测试在共用的库上做了一件会永久毁掉它的事。**

`go test ./...` 是**多进程并行**的，所有包共用同一个库（`LARKAX_DB_DSN`）。
删掉当月分区之后、恢复之前的那一小段窗口里，**别的包**写的用量事件落进了
兜底分区。而**一旦兜底分区有了行，那个月的分区就再也建不回来了**（23514）
——正是这一条要修的那个缺陷本身。于是：

- 那条用例的 `t.Cleanup` 恢复不了（它自己报的错就是 23514）；
- 后面**所有**跟分区有关的断言全部失败，包括别的包里那些自己完全正确的用例。

门禁每次 `DROP DATABASE` + `CREATE DATABASE`，所以这不是残留污染，
是**单次 run 内的自伤**——单跑一个包永远复现不出来。

**为什么这不是[第 16 条](16-silent-check-failure.md)（跳过与通过长得一样）的重复**

[第 16 条](16-silent-check-failure.md)是「用例没跑到，而输出看不出」。这一条是「用例跑了、也过了、
但它在库里留下的状态把别人弄挂了」，而且**受害者的名字出现在失败列表里**，
排查方向一开始就指错了人。

**同一个形状的第二个实例**：`request_snapshots` 按天分区，`internal/snapshotwire`
的 `TestIntegrationInsertLandsInDayPartition` 要证明的同样是「写入侧会补建缺的那天
分区」，它删的也同样是**真正的当天**（`PartitionName(time.Now().UTC())`），
`t.Cleanup` 里再删一次。它比上面这个实例还多错一处：兜底分区那条断言写的是
`SELECT count(*) FROM request_snapshots_default`，一条**全局**判据。

受害方是 `internal/assembly` 的端到端用例，它们按 `request_id` 查
`request_snapshots`、等快照落库。当天分区被删掉的那段窗口里：

- 它们刚写的行跟着被删掉，于是等不到，报「没有等到请求快照落库」；
- 那段窗口里写进来的行落进兜底分区，于是这条用例**自己**的全局计数断言变成非 0，
  报「兜底分区有 N 行，说明有写入绕过了补建」——而真凶是它自己刚删了分区。

两个报错都指向错误的地方：一条指向隔壁包，一条指向 `ensurePartition`。

**当时的教训没有被系统地应用。** 这一条当时落地的预防里，有两条（换一个别人碰不到
的目标、把它做成护栏）都只加在了**遇到问题的那张表**上——`usage_events` 一侧有
`dropUsageEventPartition` 这道护栏，`request_snapshots` 一侧一条都没有，用例照旧
删当天分区。所以修一个实例时要多问一句：**还有哪张表是同一个形状的？** 按形状找
（分区表、共用库上的 schema 级破坏、跨进程可见的全局判据），而不是按当时报错的
那张表去找。

**预防**

- **测试不许对共用的库做不可逆的 schema 变更。** 「删掉它，`t.Cleanup` 再建回来」
  在单进程下是干净的，在多进程共用库下不是——**恢复这一步可能失败，
  而失败的后果是别人挂**。
- 要做这类破坏时，选一个**别人碰不到的**目标：换一个合成的月份 / 名字 / 分区，
  而不是「真正的当月」。判据是问一句：**这个窗口里，别的进程会不会往它里面写？**
  会写 → 不能删。按天分区的表同理：换一个合成的日期，取未来的那天更省事
  （它一定还没有分区，删掉它也不产生任何副作用）。
- 把这条规矩做成**护栏**，不要靠人记得：`dropUsageEventPartition`（`usage_events`）
  与 `dropRequestSnapshotPartition`（`request_snapshots`）在名字等于「当月 / 当天
  分区」时直接 `t.Fatal`，且删分区的动作只走这两个函数。护栏本身要有判据
  （传当月 / 当天进去必须挂）。
- **断言不许对着全库共享的表做全局计数。** 「兜底分区为空」要收窄到自己写进去的
  那一行（例如按 `request_id`），否则隔壁进程的行会被算到这条用例头上，
  而报出来的方向指向别处。
- 只跑自己那一个包**证明不了**这类问题。改动了分区 / 迁移 / 共享表结构之后，
  判据必须是**全量** `go test -race ./...`（在 staging 上跑，本机内存不够）。
  这与[第 25 条](25-each-branch-green-merge-red.md)同源：**单分支、单包的视角照不到这一整类。**
