# 9 · 自测样本里的假密钥会被 GitHub 当成真密钥拦下

**编号** 9 · **日期** 2026-10-16 · **摘要**：本地扫描器与 GitHub push protection 判据不同，自测样本的取值要同时满足两边。

**现象**

首次把仓库推上 GitHub 被直接拒绝，两个 job 都没跑起来：

```
remote: error: GH013: Repository rule violations found for refs/heads/main.
remote:   —— Stripe API Key ————
remote:     commit: 9a4495c
remote:     path: scripts/check-redlines.test.sh:52
remote:   —— Slack API Token ————
remote:     path: scripts/check-redlines.test.sh:60
```

而那两行里面是扫描器自测用的样本，取值是编的。

**原因**

GitHub 的 push protection 与本地扫描器**判据不同**，且互不可见：

| | 看什么 | 白名单 |
|---|---|---|
| 本仓库的 `check-redlines.sh` | 形态 + 行内占位词 | 我们自己的 `ALLOW` |
| GitHub push protection | providers 的真实正则 | 不认识我们的白名单 |

所以「我们自己知道那是假的」完全不构成理由。它扫的是**整段提交历史**，
所以修好工作区不够——必须重写历史。

更绕的是白名单与判据的冲突：本地扫描器为了不放过厂商文档里的样例值
（`AKIA…EXAMPLE`），刻意**不把 `EXAMPLE` 放进白名单**；而 push protection
恰恰会拦下这类样例值。于是「按本地规则写得很对」的样本，正好是推不上去的样本。

**解决**

样本的用途是证明**模式**能命中，取值本身无所谓。规则改成：

1. 前缀保留（那是被测的东西），后面跟明显是占位的字母序列
2. 不出现厂商文档里的样例值
3. 确需真实形态的用例，先本地确认能命中，再确认推送不被拒

已经写进历史的用 `git filter-branch --tree-filter` 逐提交替换，
然后删掉 `refs/original/*`、expire reflog、`git gc --prune=now`，
最后用 `git log --all -p | grep` 确认旧取值一个都不剩。
（这一次是在首次推送之前做的，没有别人克隆过；一旦推上去就只能走
GitHub 的 unblock 流程或强推重写，代价完全不同。）

顺带撞到第二道门：gh token 缺 `workflow` scope 时，GitHub 拒绝**任何**
包含 `.github/workflows/` 的推送。这次先把工作流摘出来推上去、
等 scope 补上再单独一个 PR 恢复，比卡在门口快。

**预防**

- 检查工具的样本要**同时**满足两边：本地扫描器能命中，且不触发 push protection
- 样本取值的编写规则写进 `check-redlines.test.sh` 顶部，改样本前先看
- 首次推送前先在本地跑一遍 `git log --all -p` 的密钥扫描，别等地推上去才发现
- 推新仓库前先确认 token 有 `workflow` scope
