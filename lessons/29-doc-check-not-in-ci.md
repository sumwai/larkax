# 29 · 文档里写着、而 CI 没跑的那条检查，等于没跑

**编号** 29 · **日期** 2026-10-17 · **摘要**：手抄两份清单必然漂移，而漂移只会往「CI 少一道」这个方向积累。

**现象**

`AGENTS.md` 的 Lint 一节列了三条契约测试相关的命令：

```sh
pip install -r contract/requirements.txt
go test -tags=contract -count=1 -v ./internal/assembly/ -run Contract
go vet -tags=contract ./internal/assembly/
```

而 `.github/workflows/ci.yml` 的 `契约` job 里只有前两条。第三条从写下那天起
**一次都没跑过**——`go vet` 默认不看 build tag 后面的文件，所以 `go` job 里那句
`go vet ./...` 也照不到它。

**原因**

**清单是手抄的，而且是两份。** `ci.yml` 一份、`AGENTS.md` 一份，两边的条目各写各的。
漂移的方向是不对称的：

- `AGENTS.md` 多一条 → **没有任何人会发现**。文档不是执行器，多写的命令不会失败，
  也不会有人去比对它和 CI 的差集。
- `ci.yml` 多一条 → 下一轮 CI 就会红，立刻可见。

所以漂移只会往「CI 少一道」这个方向积累，而那一侧恰恰是**唯一的执行者**。

**怎么防**

1. **能抽出来执行的清单，就不要手抄第二份。** 本仓已经这么做过一次：集成测试那道门
   在 `ci.yml` 里是 `./.github/actions/check-integration-tests`，包清单只在
   `action.yml` 里写一遍，本地要跑同一道门就**把脚本从 `action.yml` 里抽出来执行**
   （见 `internal/assembly` 那条门禁的形状），而不是把包名再抄一遍。
   抄第二份的那一天，就是两份开始分岔的那一天。
2. **加了 CI 步骤，回头看一眼文档里对应的命令。** 反过来也一样：文档里写了
   而 CI 没跑的，要么补进 CI，要么删掉——**留着的那条是在承诺一件不会发生的事**。
3. 这一条的判据与[第 22 条](22-implemented-tested-ungated.md)（「有实现、有测试、没有门」）是同一个形状，只是那一处的
   「门」是测试，这一处的「门」是 CI 步骤：**两边都在，才算接上。**
