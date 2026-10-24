# syntax=docker/dockerfile:1

# 发布用的镜像：两个二进制、控制台静态产物、手册。
#
# 构建阶段跑在构建机架构上（`$BUILDPLATFORM`），二进制按目标架构交叉编译
# （`$TARGETARCH`）。CI 在 amd64 上跑一次就能同时出 amd64 与 arm64 两个镜像，
# 不必为 arm64 再找一台机器。
#
# 版本号只有一个来源：构建参数 VERSION（CI 传 tag）。ldflags 从 Makefile 的
# `print-ldflags` 取，不在这个文件里再写一份 `-X main.version=`——那样镜像里的
# 二进制与 `make dist` 出的那一份就有了两个注入点，漂了只能靠用户报问题才发现。

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS go

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN apk add --no-cache make

WORKDIR /src

# 先只拷依赖清单：源码没变时这一层命中缓存，改一行 Go 代码不必重新下依赖。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# make 只用来读 LDFLAGS。两个二进制各编一次，与 `make dist` 用的是同一组参数。
RUN set -e; \
    ldflags="$(make -s print-ldflags VERSION=$VERSION)"; \
    for pair in "larkax:./cmd/larkax" "larkaxctl:./cmd/larkaxctl"; do \
      name="${pair%%:*}"; pkg="${pair#*:}"; \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
        go build -trimpath -ldflags "$ldflags" -o "/out/$name" "$pkg"; \
    done

# 控制台产物。Node 版本跟着仓库里的 .nvmrc 走（见 web/package.json 的 engines），
# 写死一个数字的话，改 .nvmrc 时这里不会跟着变。
#
# npm ci、构建与「产物里不含演示数据」那一查都调 Makefile 的 web-build：
# 镜像里的页面与 `make web` 出的包因此来自同一份配方。
FROM --platform=$BUILDPLATFORM node:20-alpine AS web

ARG VITE_API_MODE=http

RUN apk add --no-cache make

WORKDIR /src

COPY Makefile ./
COPY web/ ./web/

RUN make -s web-build VITE_API_MODE=$VITE_API_MODE

# 证书与时区数据在构建平台上装好再拷进去：装包这一步要在目标架构上跑，
# 就得上 qemu 模拟；而这两份都是与架构无关的数据文件，拷过去就够了。
# 少了 ca-certificates，调上游时每一个 TLS 连接都验不过；少了 tzdata，
# 按地区判断工作日与峰谷的计价规则会退回 UTC。
FROM --platform=$BUILDPLATFORM alpine:3.22 AS runtime-data

RUN apk add --no-cache ca-certificates tzdata

FROM alpine:3.22

COPY --from=runtime-data /etc/ssl/certs/ /etc/ssl/certs/
COPY --from=runtime-data /usr/share/zoneinfo/ /usr/share/zoneinfo/

COPY --from=go /out/larkax /usr/local/bin/larkax
COPY --from=go /out/larkaxctl /usr/local/bin/larkaxctl
COPY --from=go /src/man/man1/ /usr/local/share/man/man1/
COPY --from=web /src/web/dist /srv/web

# 监听地址必须写宽：镜像里的默认值 127.0.0.1 只监听容器自己的回环，端口映射
# 接不进来。LARKAX_WEB_DIR 指向控制台产物，公开监听因此同时提供 API 与页面。
ENV LARKAX_WEB_DIR=/srv/web \
    LARKAX_HTTP_ADDR=:8080 \
    LARKAX_INTERNAL_ADDR=:9090

EXPOSE 8080 9090

# 非 root 运行（数值形，镜像里不必建用户）。数据库与 Redis 都是外部依赖，
# 镜像不带任何数据目录。
USER 10001:10001

ENTRYPOINT ["/usr/local/bin/larkax"]
