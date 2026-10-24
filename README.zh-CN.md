# larkax

将多个上游渠道统一封装在一个 API 后面，集中处理路由、计量与计价。现有的 OpenAI、Anthropic、OpenAI Responses SDK 只需修改 `base_url` 即可接入使用。

[English](README.md)

## 安装

| 获取方式 | 位置 | 说明 |
|---|---|---|
| 容器镜像 | `ghcr.io/sumwai/larkax:<tag>` | API 与控制台打包在同一镜像内，支持 `linux/amd64` 与 `linux/arm64` |
| 二进制包 | `larkax_<tag>_linux_<arch>.tar.gz` | 包含 `larkax`、`larkaxctl`、手册、`LICENSE`、`README.md` |
| 仅控制台 | `larkax-web_<tag>.tar.gz` | 静态文件包，解压后将 `LARKAX_WEB_DIR` 指向该目录 |
| 从源码构建 | 本仓库 | 需要 Go 1.26+ 与 Docker；Node 20+ 仅在构建控制台时需要 |

每个版本均附带一份 `SHA256SUMS`，涵盖上述全部 tar 包。下载后可执行 `sha256sum -c --ignore-missing SHA256SUMS` 校验文件完整性。容器镜像不使用校验和文件，`docker pull` 打印的 digest 即为镜像唯一标识。

## 运行

```sh
docker run --rm -p 8080:8080 \
  -e LARKAX_DB_DSN='postgres://user:password@host:5432/larkax' \
  -e LARKAX_MASTER_KEYS="k1:<base64-of-32-bytes>" \
  ghcr.io/sumwai/larkax:<tag>
```

控制台地址为 `http://127.0.0.1:8080/`，API 路径位于 `/v1/` 下。服务启动时不会自动执行数据库迁移。启动前需先使用同一镜像运行 `docker run --rm --entrypoint larkaxctl ... migrate up`。PostgreSQL 是唯一必需的外部依赖。Redis 为可选项，仅用于将限流计数移出服务进程。

```sh
curl -sS 127.0.0.1:8080/v1/models -H "Authorization: Bearer $KEY"
```

全新实例尚未配置模型、渠道、价格和 key，因此上述请求会返回 `401`。配置模型、渠道、出站映射、价格、钱包和 key 的完整步骤记录在 [docs/development.md](docs/development.md#一次跑通)，仓库内仅维护这一处说明；英文版步骤见 [QUICKSTART.md](QUICKSTART.md)。

命令行提示信息目前为中文。命令、退出码与打印的值均已稳定，但周边的说明文字尚未翻译为英文。

## 从源码构建

构建环境需要 Go 1.26+、Docker（`docker compose` v2.1+）、`make` 和 `openssl`。构建与运行默认使用 `8080`、`9090`、`5432` 端口，需确保这些端口空闲。可通过 `LARKAX_HTTP_ADDR`、`LARKAX_INTERNAL_ADDR`、`PG_PORT` 修改默认端口。Node 20+ 只在构建控制台（`make web`）与 `make release` 时需要。

```sh
make run       # 起 PostgreSQL、跑迁移、前台跑服务
make test      # 依赖起着才跑得完整，这个目标会先起依赖
make install   # 二进制、bash/zsh/fish 补全、手册装到 ~/.local
make release   # 发布资产：二进制包 + 控制台包 + SHA256SUMS
```

直接执行 `go test ./...` 时，未读取到 `LARKAX_DB_DSN` 的集成测试会整包跳过，但在终端结果中仍显示 `ok`。如需运行完整测试，应执行 `make test`。

## 配置

启动服务前必须配置两项环境变量：`LARKAX_DB_DSN`，以及 `gateway` 与 `control` 角色下的 `LARKAX_MASTER_KEYS`（`worker` 角色不需要主密钥）。其余配置项均有默认值。完整配置列表、各项用途，以及新配置项应放入环境变量还是数据库的判断标准，详见 [docs/development.md](docs/development.md#环境变量)。

将 `LARKAX_WEB_DIR` 指向解压后的控制台目录，服务才会提供控制台页面。未设置该变量时，公开监听端口仅提供 API 服务，其余路径一律返回 `501`。

## 状态

目前已实现的功能包括：鉴权、限流、选路、预留、结算，以及记录每笔资金变动的账本。系统已支持四种协议（其中三种支持流式传输）、一个处理类型化判断题的入口、支持分时与按上下文长度分档的表达式计价、浏览器控制台，以及 JavaScript 插件。

尚未完成的部分包括：真实支付渠道。回调逻辑和账本分录均已就绪，但目前仅有模拟通道，且默认关闭。三个额度来源中仍有两项尚未接入。系统整体尚未达到生产可用状态，缺少高可用方案、运维手册和压力测试。

## 文档

`docs/` 目录下的文档均为中文，文档索引见 [docs/README.md](docs/README.md)。

| 文档 | 内容说明 |
|---|---|
| [docs/development.md](docs/development.md) | 如何构建、如何启动、如何跑通一次请求；本地检查与门禁 |
| [docs/api.md](docs/api.md) | HTTP 接口：路径、请求头、状态码、错误体、流式事件 |
| [docs/cli-reference.md](docs/cli-reference.md) | 每一条 `larkaxctl` 命令的说明 |
| [docs/metrics.md](docs/metrics.md) | 服务导出的指标，以及各项指标的告警规则 |
| [docs/plugin-contract.md](docs/plugin-contract.md) | 插件的功能范围与五个钩子的位置 |
| [docs/versioning.md](docs/versioning.md) | 发布版本与 API 版本的关系：发布新版本不改动 `/v1` |
| [LESSONS.md](LESSONS.md) | 仓库遇到过的问题与当时的处理方式 |

## 贡献

贡献说明见 [CONTRIBUTING.md](CONTRIBUTING.md)。仓库的长期约定与红线见 [AGENTS.md](AGENTS.md)。

## 许可证

采用 AGPL-3.0 许可证，详见 [LICENSE](LICENSE)。

## 致谢

- [new-api](https://github.com/QuantumNous/new-api)：`relaykit` 提供协议 DTO 与 OpenAI ↔ Anthropic 转换；采用 AGPL-3.0 许可证，附加条款保留在 [NOTICE](NOTICE)。
- [moejs](https://github.com/Calcium-Ion/moejs)：提供插件沙箱运行的 JavaScript 运行时。
- [expr-lang/expr](https://github.com/expr-lang/expr)：提供计价规则的表达式引擎。
- [holiday-cn](https://github.com/NateScarlet/holiday-cn)：提供中国大陆节假日数据。
- [models.dev](https://github.com/sst/models.dev)：提供模型元数据（计划中）。
