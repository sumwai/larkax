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
# 反斜杠要先转，否则后面几步加进去的反斜杠会被自己再转一遍。
#
# ' 是给 shell 准备的：下面每条 sed 表达式都用单引号包住（否则 shell 会先吃掉
# 一层反斜杠，口令里的 \ 就没了）。单引号串里没法直接写 '，标准写法是
# 收尾、转义、重开 —— '\'' 。它必须放在最后一步：先做的话，它自己带进来的
# 反斜杠会被上一步再转一遍。
ESCAPED_DB_PASSWORD := $(subst ','\'',$(subst |,\|,$(subst &,\&,$(subst \,\\,$(DB_PASSWORD)))))

.PHONY: help init up down migrate run check-ports test clean

help:
	@echo "make run      准备 .env → 起依赖 → 迁移 → 前台跑服务"
	@echo "make up       只起依赖（PostgreSQL）"
	@echo "make down     只停依赖，保留数据卷"
	@echo "make migrate  只跑迁移"
	@echo "make test     跑测试（先确保依赖起着）"
	@echo "make clean    清容器、数据卷与生成的 .env"
	@echo ""
	@echo "端口被占用时：make run PG_PORT=55432"

# 生成 .env。**已存在时不重建**：重建等于毁掉已加密的存量数据，
# 主密钥一变密文就解不开了。所以这里不写成文件的依赖规则——
# 那条规则会在 .env.example 变新之后重建 .env，正是这里要防的行为。
#
# 但「不重建」不等于「什么都不看」。DSN 里的口令、端口、库名是
# DB_PASSWORD / PG_PORT / PG_DB 的第二份拷贝，只在生成那一刻同步过一次；
# 此后不再同步的话，`make run PG_PORT=55432` 会起在 55432 而服务连到 5432——
# 连不上还算好的，最坏是 5432 上恰好有另一个 larkax 库、口令也相同，
# 迁移就安静地打到那里去了。所以已存在时**只同步 DSN 那一行**：
# 指向本机（127.0.0.1 / localhost）才动，指向远端的说明是故意配的，只警告。
# 主密钥与 .env 里的其它内容一个字不碰。
#
# 生成分支的三条替换分别填上密码、主密钥，以及示例 DSN 里的端口与库名。
# 第三条与 .env.example 的默认值（:5432/larkax）绑定：改了那边的默认值，
# 这里也要跟着改。下面紧接着 grep 一次确认它真的命中了——
# 替换没命中时 .env 会安静地留着旧端口，而服务就连不上 compose 起的那个库。
init:
	@if [ -f $(ENV_FILE) ]; then \
	  echo "$(ENV_FILE) 已存在，不重建"; \
	  if grep -qE '(<MASTER_KEY>|<DB_PASSWORD>)' $(ENV_FILE); then \
	    echo "警告：它是从 $(TEMPLATE) 直接拷来的，占位符还在，服务会因主密钥无效而拒绝启动"; \
	  fi; \
	  before=$$(grep -m1 '^LARKAX_DB_DSN=' $(ENV_FILE) || true); \
	  case "$$before" in \
	    '') echo "错误：$(ENV_FILE) 里找不到 LARKAX_DB_DSN 那一行"; exit 1 ;; \
	    *@127.0.0.1:*|*@localhost:*) \
	      sed -E 's|^(LARKAX_DB_DSN="[a-z]+://[^:]+:)[^@]*(@[^:]+:)[0-9]+/[^?]*|\1$(ESCAPED_DB_PASSWORD)\2$(PG_PORT)/$(PG_DB)|' $(ENV_FILE) > $(ENV_FILE).tmp \
	        && mv $(ENV_FILE).tmp $(ENV_FILE) \
	        || { rm -f $(ENV_FILE).tmp; exit 1; }; \
	      after=$$(grep -m1 '^LARKAX_DB_DSN=' $(ENV_FILE) || true); \
	      if ! printf '%s' "$$after" | grep -qF ":$(PG_PORT)/$(PG_DB)"; then \
	        echo "错误：DSN 没能同步到 :$(PG_PORT)/$(PG_DB)，它现在是"; \
	        echo "      $$after"; \
	        exit 1; \
	      fi; \
	      if [ "$$before" = "$$after" ]; then \
	        echo "DSN 已指向 127.0.0.1:$(PG_PORT)/$(PG_DB)"; \
	      else \
	        echo "已同步 DSN 到 127.0.0.1:$(PG_PORT)/$(PG_DB)，原为 $$before"; \
	      fi ;; \
	    *) \
	      echo "注意：DSN 不指向本机，compose 起的库不会被它连上"; \
	      echo "      $$before" ;; \
	  esac; \
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

# 起服务前先看两个监听地址在不在。占用时服务会以一句 bind 错误收尾，而那句话
# 本身虽然清楚，却没说**怎么换**——换的入口（LARKAX_HTTP_ADDR /
# LARKAX_INTERNAL_ADDR）在 .env 里是注释掉的，得翻文件才知道。
#
# 9090 是常被占的那个：Clash / mihomo 的 external-controller 默认就是它，
# Prometheus 也是。撞上之后服务**完全起不来**，不是降级。
#
# 读的是加载 .env 之后的值，所以写在 .env 里的覆盖也算数。放在 up 之前：
# 端口不通就没必要先把数据库拉起来。
#
# 下面的兜底值与 internal/config 的默认值保持一致（这里只取冒号后面那段端口，
# 但两处写得不一样会让人以为默认值不同）。
check-ports: init
	@set -a; . ./$(ENV_FILE); set +a; \
	busy=0; \
	for spec in "公共入口 $${LARKAX_HTTP_ADDR:-127.0.0.1:8080} LARKAX_HTTP_ADDR" \
	            "内部端点 $${LARKAX_INTERNAL_ADDR:-127.0.0.1:9090} LARKAX_INTERNAL_ADDR"; do \
	  set -- $$spec; \
	  port=$${2##*:}; \
	  if ss -ltn 2>/dev/null | awk '{print $$4}' | grep -qE "[:.]$$port$$"; then \
	    busy=1; \
	    echo "  $$1：端口 $$port 已被占用（用 $$3 换一个）"; \
	  fi; \
	done; \
	if [ "$$busy" = 1 ]; then \
	  echo "监听地址被占了，服务起不来。用上面点到的那个变量换一个空闲端口（主机部分照写 127.0.0.1），例如:"; \
	  echo "  LARKAX_HTTP_ADDR=127.0.0.1:8081 make run"; \
	  echo "  LARKAX_INTERNAL_ADDR=127.0.0.1:8082 make run"; \
	  echo "也可以写进 $(ENV_FILE)，那样只需改一次。"; \
	  exit 1; \
	fi

run: init check-ports up migrate
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
