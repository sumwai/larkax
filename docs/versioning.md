# 版本与发布

## 两条不同的「版本」

| | 是什么 | 什么时候变 |
|---|---|---|
| **发布版本** | 二进制与镜像的版本，形如 `v0.1.0` | 每次发版 |
| **API 版本** | 路径里的 `/v1` | 只在破坏性变更时变，且要新开一条路径 |

**发布版本不等于 API 版本。** 今天发 `v1.4.2`，客户端仍然调 `/v1`。
把两者绑在一起，会让「修一个 bug」看起来像「换了一套 API」，
而客户端没有任何理由因此改代码。

## 发布版本用语义化版本

`vMAJOR.MINOR.PATCH`，含义按**使用者**的感受定，不是按代码量：

- `PATCH`：修 bug，行为与契约不变
- `MINOR`：加功能，向后兼容
- `MAJOR`：破坏性变更

GA 之前（`v0.x`）这条放宽：`MINOR` 可以有破坏性变更，`PATCH` 只修 bug。
所以 `v0.x` 期间的升级说明要写清有没有破坏性改动，不能只说「小版本」。

## 版本号从哪来

**只有一处来源：Git tag。** 构建时用 `-ldflags` 注入，代码里不写死：

```sh
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/larkax
```

两个二进制各构建一次即可（`./cmd/larkax` 与 `./cmd/larkaxctl`）；漏掉包路径
的话 `go build` 会在仓库根上找 main 包，那里没有。

未注入时是 `dev`——这样「本地随手构建的二进制」与「正式发布的二进制」
在 `larkax --version` 的输出里能一眼分开。排查线上问题时，
「这是不是正式发布的那个二进制」是第一个要问的问题。

`larkaxctl` 也走同一套注入（同一个变量名），两个二进制的版本必须能对上。

## API 契约的冻结

路径里的 `/v1` 是**永久承诺**。一旦公开就不能改语义，只能加。

**允许（非破坏性）：** 新增可选请求参数、新增响应字段、新增端点、
新增枚举值（客户端必须容忍未知值）、新增错误码。

**不允许（破坏性，需 `/v2`）：** 删除或重命名字段、改字段类型或语义、
把可选参数变必填、**改默认值**（会静默改变已有客户端的行为）、改错误响应结构。

「改默认值」这一条最容易被当成非破坏性——它不删字段也不改类型，
但客户端的行为会变，而且是在没有任何提示的情况下变。

错误格式与调用协议绑定：用 OpenAI 格式调用就返回 OpenAI 形状的错误，
Anthropic 格式同理。这条不能混。

## 升级说明

对外可见的契约变更合并时在这里留一条：**改了什么、调用方要做什么**。

破坏性改动按上文「API 契约的冻结」的判据认定，在 PR 上打 `breaking` 标签，并在
发行说明里逐条点名（见下文「发一个版本要做什么」）。点名指到这里：发行说明列出
哪几条变了，这一节写每一条具体要改哪一行。

### 引用标注的形状（`/v1/responses`）

**改了什么**：非流式响应里 `output[].content[].annotations` 的 `url_citation`
改成 Responses 协议本身定义的那一份形状。它不是上游给的那一份，
也不是转换层折出来的中间产物。字段与 `type` 并排：

```json
{"type":"url_citation","url":"https://example.com/a","start_index":0,"end_index":12}
```

改之前那一份多包了一层，是转换层折出来的中间形状：

```json
{"type":"url_citation","url_citation":{"url":"…","start_index":0,"end_index":12}}
```

流式那一侧给的一直是扁平那一份：`response.output_text.annotation.added` 事件，
以及收尾事件里的同一批标注。现在两条路是同一个形状。

**这一条算破坏性改动**：字段的形状变了，按「API 契约的冻结」的判据，PR 上要带
`breaking` 标签。它不需要 `/v2`：`/v1` 至今没有发布过任何版本——仓库里没有 tag，
也没有 release——「一旦公开就不能改语义」这条冻结还没开始。

**调用方要做什么**：按外层 `url_citation` 取值的解析改成从标注本身取值——
`annotation["url_citation"]["url"]` 变成 `annotation["url"]`，`title`、
`start_index`、`end_index` 同理，它们现在都与 `type` 并排。按协议写的解析不用改，
扁平那一份就是协议对 `url_citation` 的定义。

## 发一个版本要做什么

前置：`main` 上的 CI 全绿。

1. **在 `main` 上打 tag**：`git tag -a v0.1.0 -m "..."`，然后推 tag。tag 的形状是
   `vX.Y.Z`——触发条件是 `v*`，而名字要拼进 tarball 的文件名，带 `/` 的 tag
   （例如 `v1.0/hotfix`）会在打包那一步直接失败
2. CI 由 tag 触发，按平台构建并打包发布（包里有什么见下一节）
3. Release 说明由 `gh release create --generate-notes` 生成（分组规则见
   `.github/release.yml`，按合并进这一版的 PR 上的标签归类），再人工过一遍——
   自动生成的分组只保证「按标签归类」，不保证「说人话」

**从同一个 commit 构建，不要先构建再打 tag。** 否则 tag 指向的源码与
发布的二进制不是同一个东西，而这一点在出问题时才被发现。

**破坏性改动在那条 PR 上打 `breaking` 标签**（改字段语义、改默认值这类，见上文
「API 契约的冻结」）。分组写在 `.github/release.yml` 里：`breaking` 归「破坏性改动」
一类，其余标签归「其他改动」。没有这个标签时，一处改了错误码的取值会和一处文案修正
落进同一类，读的人分不出哪一条要动手——客户端按文档里那个取值写分支，改了就掉进
`else`，本该重试的调用会被上报成失败。

发行说明里还要**逐条点名** `breaking` 下的每一条，不能只让它待在自动分组里：
分组只说「有这一类」，每一条要改什么写在「升级说明」里（见上文），点名时指过去。

## 发布包里有什么

一次 tag 发布（`v*`）带三样东西：每个平台一个二进制包、一份控制台包、一份镜像。

### 二进制包

每个平台一个 tar.gz，名字形如 `larkax_v0.1.0_linux_amd64.tar.gz`。解开是一个与包同名
的目录，里面是：

- `larkax` 与 `larkaxctl`：两个二进制，可执行位保留
- `LICENSE`、`README.md`
- `man/man1/` 下的手册

顶层那个目录是必要的：解到同一个位置的两个版本不会互相覆盖。翻手册要把 `MANPATH`
指到解出来的那份 `man/` 上（`man` 只认它下面的 `man1/` 这类子目录）：

```sh
tar xzf larkax_v0.1.0_linux_amd64.tar.gz
MANPATH=larkax_v0.1.0_linux_amd64/man man larkaxctl-channel
```

在源码树里翻的写法见 [cli-reference.md](cli-reference.md)。

### 控制台包

`larkax-web_v0.1.0.tar.gz` 里是一个与包同名的目录，里面是 `dist/`（Vite 产物：
`index.html`、`assets/` 与其它静态文件）和 `LICENSE`。把这个目录指给服务的
`LARKAX_WEB_DIR`，公开监听就同时提供 API 与页面；前端路由（例如
`/console/channels`）由 `index.html` 接管，而 `/v1/` 仍走推理入口。

`LARKAX_WEB_DIR` 留空时公开监听只提供 API，其余路径照旧回 `501`。

### 镜像

```sh
docker pull ghcr.io/sumwai/larkax:v0.1.0
docker run --rm -p 8080:8080 \
  -e LARKAX_DB_DSN='postgres://user:password@db:5432/larkax' \
  -e LARKAX_MASTER_KEYS="k1:<base64>" \
  ghcr.io/sumwai/larkax:v0.1.0
```

- 标签是 tag 本身，另有一个 `latest`；架构 `linux/amd64` 与 `linux/arm64`。
- 镜像里已经带着控制台，`LARKAX_WEB_DIR=/srv/web`，两个监听地址也写成了 `:8080`
  与 `:9090`（不写宽则只绑容器自己的回环，端口映射接不进来）。
- 以 uid 10001 运行；`larkaxctl` 也在镜像里（`docker run --entrypoint larkaxctl`），
  迁移可以在部署里跑。
- 数据库与 Redis 都是外部依赖，镜像不带数据目录。
- **不需要单独的镜像仓库账号**：推送用工作流自带的 `GITHUB_TOKEN`（`packages: write`）。
  第一次发布后这个包默认是私有的，要在包的设置里手动改成公开；否则拉取要 `docker login`。

### 校验和

`SHA256SUMS` 与所有 tarball 一起挂在 Release 上，核对用
`sha256sum -c --ignore-missing SHA256SUMS`：只下自己那几份时，`--ignore-missing`
跳过其它行，不会因为一个「No such file or directory」而以非零退出。镜像不在校验和
里：它的完整性由 registry 的 digest 保证，`docker pull` 拿到的就是那个 digest。

**打包只有一份配方**：`Makefile` 的 `release` 目标（`dist` + `web` + 校验和）。
CI 调的也是它，发布那一步只留 CI 自己的东西（`setup-go`、`setup-node`、
`gh release create`）：

```sh
make release VERSION=v0.1.0 ARCHS="amd64 arm64"   # 发布时走的就是这一条
make release VERSION=v0.1.0                       # 本地演练：默认只打 amd64
```

`make dist` 会把刚构建出来的二进制**真执行**一次，比对它自称的版本是否就是那个
tag——二进制自称 `dev` 这件事，只有用户报问题时才会被发现。跑不起来的架构
（不是本机架构）不静默跳过，而是打印一句「跳过执行校验（架构不同）」：静默跳过
与「校验通过」在日志里长得一样（`LESSONS.md` 第 16 条）。本机架构按
`go env GOHOSTARCH` 判定，所以在 amd64 主机上发布时，验的是 amd64 那一份。

`make web` 构建完会查一次产物里有没有只有 mock 客户端才有的固定用户名
（`演示账号`）：`VITE_API_MODE` 没生效时构建照样成功、页面照样能开，只是数字全是
演示数据——那一查是唯一会响的地方。

两个目标配方用的是 GNU tar 的那几个选项，macOS 自带的 bsdtar 不认。

## 从源码树安装

`make install` 把两个二进制装进 `~/.local/bin`，同时建两个短名链接，把
`man/man1/` 下的手册装进去，并按各 shell 的约定装好三个补全文件：

```sh
make install                       # 默认装到 ~/.local，版本号是 dev
make install PREFIX=/usr/local      # 换落点
make install VERSION=v0.1.0         # 打上版本号，与 make dist 同一份注入规则
make install ALIASES=""             # 只要两个二进制，不建短名链接
make uninstall                      # 删掉上面装进去的那些名字
```

落点由 `PREFIX` 派生：`BINDIR` 是 `$(PREFIX)/bin`，`MANDIR` 是
`$(PREFIX)/share/man/man1`。补全各自走那个 shell 认的目录：

| shell | 文件 | 那个 shell 怎么找到它 |
|---|---|---|
| bash | `$(PREFIX)/share/bash-completion/completions/larkaxctl` | bash-completion 2 读 `$XDG_DATA_HOME/bash-completion/completions/`，`PREFIX=~/.local` 正好对应 |
| fish | `$(PREFIX)/share/fish/vendor_completions.d/larkaxctl.fish` | fish 读 `$XDG_DATA_HOME/fish/vendor_completions.d/`，同上 |
| zsh | `$(PREFIX)/share/zsh/site-functions/_larkaxctl` | zsh 没有这种目录约定，这个路径不在默认 `fpath` 里 |

zsh 那一行要在 `compinit` 之前把目录加进 `fpath`，否则补全不会生效：

```sh
fpath=(~/.local/share/zsh/site-functions $fpath)   # 写进 zsh 的启动文件（zshrc），在 compinit 之前
```

三个补全文件由刚装好的二进制现场生成（`larkaxctl completion <shell>`），仓库里
不存静态副本：副本会与命令注册表分叉。生成物为空时 `make install` 直接失败——
空脚本能被所有 shell 无错加载，而它一条候选也不给。

短名链接是 `laxctl` → `larkaxctl`、`lax` → `larkax`。用符号链接而不是 shell
`alias`：一份覆盖所有 shell 与非交互脚本，也不必往任何 rc 文件里写字。两个
CLI 的帮助文本里写死了 `larkaxctl`，不读 `argv[0]`，所以改名不会把用法说明改坏。
补全只针对 `larkaxctl`：`laxctl` 是同一个二进制的另一个名字，但 shell 的补全按
命令名登记，别名下按 Tab 不会命中那一份。

装进去之前会先执行一次 `--version` 比对，与 `make dist` 的 `VERIFY_EXEC` 同一个
思路：版本注入失手时二进制照样能跑，唯一的差别是自称 `dev`。`install` 构建的是
本机架构，所以这里没有「架构不同，跳过校验」那一支。

与 tarball 的分工：tarball 是发布物，带 `SHA256SUMS`，装到哪台机器上属于使用方
的事；`make install` 面向从源码树直接用的场景——版本号默认是 `dev`，没有校验和，
也没有 `LICENSE` 与 `README.md`。手册装了之后 `man larkaxctl-channel` 才有内容，
`man` 不认 `~/.local/share/man` 时用 `MANPATH=~/.local/share/man` 指过去。

## 版本号出现在哪里

| 位置 | 内容 |
|---|---|
| `larkax --version` | 发布版本 |
| `larkaxctl --version` | 发布版本（必须与上面一致） |
| 启动日志 | 发布版本、角色、环境 |
| HTTP 响应头 `X-Larkax-Version` | 发布版本 |

响应头带版本是**有意的**：用户报问题时，「你调的是哪个版本」是最该
先确认的事，而让他去翻部署记录比让他贴一个响应头麻烦得多。
它不泄漏任何东西——开源项目的版本号是公开信息。

## 每个响应都带请求标识

除了版本，**每一个响应**都带一个 `X-Request-Id` 头——成功的、失败的、流式的
（流式那份在第一个事件之前就把头发出去了）。（响应头的完整清单见
[api.md](api.md) 的「响应头」一节。）错误正文刻意不回上游的错误原文、
也不回内部路径与标识，所以出问题时该查的就是这个标识：报问题时**附上它**，
我们拿它去日志里找这一次请求。

- 成功响应体里的 `id` 与这个头是**同一个值**，贴哪个都指向同一批日志。
- 客户端可以自己带一个同名请求头，我们沿用你那一个——它得**非空、不超过
  128 字节、且全是可见 ASCII**。有一条不满足就整条丢弃并重新生成，不
  做「洗掉非法字符再沿用」：一个带换行的标识会伪造出额外的日志行。
- 它只用于**关联**，不用于鉴权，也不是幂等键：同一个标识出现两次，我们照样当成
  两次请求。要幂等用 `Idempotency-Key`（见 [idempotency.md](idempotency.md)）。
