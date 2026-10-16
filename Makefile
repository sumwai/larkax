# larkax 本地开发的入口。
#
# app 不进容器：compose 只负责依赖（PostgreSQL）的生命周期，服务本身留在
# 宿主上 go run，改一行即时生效，不必 rebuild 镜像。完整说明见 docs/development.md。
#
#   make run      一把梭：准备 .env → 起依赖 → 等就绪 → 迁移 → 前台跑服务
#   make up       只起依赖
#   make down     只停依赖（保留数据卷）
#   make migrate  只跑迁移
#   make test     跑测试（先确保依赖起着）
#   make clean    清容器、数据卷与生成的 .env，回到刚克隆的状态
#
# 端口被占用（5432 是常事）就换一个：make run PG_PORT=55432

# 目标之间是有先后的（up 必须在 migrate 之前），并行跑会破坏这个顺序。
.NOTPARALLEL:

# 密码只有一个来源：这里。compose 的插值与 .env 里的 DSN 都由它替换。
# 两处各写一份的话，改了一处就会连上另一个库——而那看起来像启动正常。
DB_PASSWORD ?= placeholder
PG_PORT     ?= 5432
PG_DB       ?= larkax
export DB_PASSWORD PG_PORT PG_DB

ENV_FILE := .env
TEMPLATE := .env.example

# sed 的替换串里 & 是整个匹配、\ 是转义、| 是这里用作分隔符的字符。
# 口令里带上它们会生成一份语法没错但内容不对的 .env——最难发现的那种坏法。
# 反斜杠要先转，否则后面两步加进去的反斜杠会被自己再转一遍。
ESCAPED_DB_PASSWORD := $(subst |,\|,$(subst &,\&,$(subst \,\\,$(DB_PASSWORD))))

.PHONY: help init up down migrate run test clean

help:
	@echo "make run      准备 .env → 起依赖 → 迁移 → 前台跑服务"
	@echo "make up       只起依赖（PostgreSQL）"
	@echo "make down     只停依赖，保留数据卷"
	@echo "make migrate  只跑迁移"
	@echo "make test     跑测试（先确保依赖起着）"
	@echo "make clean    清容器、数据卷与生成的 .env"
	@echo ""
	@echo "端口被占用时：make run PG_PORT=55432"

# 生成 .env。**已存在时一律不动**：覆盖等于毁掉已加密的存量数据，
# 主密钥一变密文就解不开了。所以这里不写成文件的依赖规则——
# 那条规则会在 .env.example 变新之后重建 .env，正是这里要防的行为。
#
# 三条替换分别填上密码、主密钥，以及示例 DSN 里的端口与库名。
# 第三条与 .env.example 的默认值（:5432/larkax）绑定：改了那边的默认值，
# 这里也要跟着改。下面紧接着 grep 一次确认它真的命中了——
# 替换没命中时 .env 会安静地留着旧端口，而服务就连不上 compose 起的那个库。
init:
	@if [ -f $(ENV_FILE) ]; then \
	  echo "$(ENV_FILE) 已存在，保持不动"; \
	  if grep -qE '(<MASTER_KEY>|<DB_PASSWORD>)' $(ENV_FILE); then \
	    echo "警告：它是从 $(TEMPLATE) 直接拷来的，占位符还在，服务会因主密钥无效而拒绝启动"; \
	  fi; \
	else \
	  sed -e 's|<DB_PASSWORD>|$(ESCAPED_DB_PASSWORD)|' \
	      -e "s|current:<MASTER_KEY>,previous:<MASTER_KEY>|$$(openssl rand -base64 32)|" \
	      -e "s|:5432/larkax|:$(PG_PORT)/$(PG_DB)|" \
	      $(TEMPLATE) > $(ENV_FILE); \
	  grep -q ":$(PG_PORT)/$(PG_DB)" $(ENV_FILE) || { \
	    echo "$(TEMPLATE) 的 DSN 里没有 \":5432/larkax\"，端口与库名没能替换进去"; \
	    rm -f $(ENV_FILE); \
	    exit 1; \
	  }; \
	  echo "已生成 $(ENV_FILE)：PostgreSQL 在 127.0.0.1:$(PG_PORT)，库 $(PG_DB)"; \
	fi

# --wait 靠 healthcheck 等到库真能接受连接再返回。
# 直接 up -d 接迁移会 race，这是本地跑挂最常见的原因。
up:
	@docker compose up -d --wait

down:
	@docker compose down

migrate: init
	@set -a; . ./$(ENV_FILE); set +a; go run ./cmd/larkaxctl migrate up

run: init up migrate
	@echo "前台运行服务，Ctrl-C 退出（数据库仍在跑，用 make down 停）"
	@set -a; . ./$(ENV_FILE); set +a; go run ./cmd/larkax

# 先起依赖再跑测试：集成测试拿不到 LARKAX_DB_DSN 时会**跳过**，
# 而「跳过」在输出里和「通过」长得一样（见 LESSONS.md 第 16 条）。
test: init up
	@set -a; . ./$(ENV_FILE); set +a; go test ./...

clean:
	@docker compose down --volumes --remove-orphans
	@rm -f $(ENV_FILE)
	@echo "已清理：容器、数据卷与 $(ENV_FILE)"
