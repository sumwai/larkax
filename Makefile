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
#   make dist     打发布用的 tar.gz（默认只打 amd64；CI 发布调的也是它）
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

# .env.example 里那几个占位符的拼写只写在这里：下面的 sed（替换）、grep（提示
# 「拷来的 .env 里占位符还在」）与生成之后那次复查都从它派生。
#
# 各写一份的话，「改了 .env.example 而没改 Makefile」这条路上没有任何东西会响：
# 替换对不上，.env 里留着占位符或一个空值，而那份文件写出来之后就不再重建
# （见 init 的注释）——坏掉的那份会一直躺在那里，直到有人对着服务那句
# 「主密钥无效」去翻它。
DB_PASSWORD_PLACEHOLDER := <DB_PASSWORD>
MASTER_KEY_PLACEHOLDER  := <MASTER_KEY>
MASTER_KEYS_PLACEHOLDER := current:$(MASTER_KEY_PLACEHOLDER),previous:$(MASTER_KEY_PLACEHOLDER)

# sed 的替换串里 & 是整个匹配、\ 是转义、| 是这里用作分隔符的字符。
# 口令里带上它们会生成一份语法没错但内容不对的 .env——最难发现的那种坏法。
# 反斜杠要先转，否则后面几步加进去的反斜杠会被自己再转一遍。
#
# ' 是给 shell 准备的：下面每条 sed 表达式都用单引号包住（否则 shell 会先吃掉
# 一层反斜杠，口令里的 \ 就没了）。单引号串里没法直接写 '，标准写法是
# 收尾、转义、重开 —— '\'' 。它必须放在最后一步：先做的话，它自己带进来的
# 反斜杠会被上一步再转一遍。
#
# 唯一的例外是主密钥那一条：它带一个 shell 变量（$$key），所以用双引号。
# 那不涉及上面这套转义——base64 的字母表里没有 sed 的特殊字符（& \ | '），
# 于是双引号替 shell 吃掉一层反斜杠这件事在那里无从发生。口令那一条不能
# 跟着改成双引号。
ESCAPED_DB_PASSWORD := $(subst ','\'',$(subst |,\|,$(subst &,\&,$(subst \,\\,$(DB_PASSWORD)))))

.PHONY: help init up down migrate run check-ports test dist clean web web-build release print-ldflags

help:
	@echo "make run      准备 .env → 起依赖 → 迁移 → 前台跑服务"
	@echo "make up       只起依赖（PostgreSQL）"
	@echo "make down     只停依赖，保留数据卷"
	@echo "make migrate  只跑迁移"
	@echo "make test     跑测试（先确保依赖起着）"
	@echo "make dist     打发布用的 tar.gz（默认只打 amd64，可 ARCHS=\"amd64 arm64\"）"
	@echo "make web      构建控制台并打成 dist/larkax-web_<版本>.tar.gz（要 Node）"
	@echo "make release  dist + web + SHA256SUMS，发布用的全部资产"
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
# 这里也要跟着改。后两条各自紧跟着一次复查，复查不过就把刚写出来的 .env 删掉
# ——替换没命中是**静默**的：DSN 那一条留着旧端口（服务连不上 compose 起的
# 库），主密钥那一条留着占位符或一个空值（服务以主密钥无效拒绝启动），
# 而 .env 已经落在磁盘上、之后不再重建，于是坏掉的那份会一直留着。
#
# 生成之前先探 openssl。没有它时 `$$(openssl rand -base64 32)` 展开成**空串**，
# 而命令替换失败并不会让 sed 那一行失败——于是 .env 照样被写出来、主密钥是空的、
# 末尾还打印「已生成」。报错点与真实原因隔了一层，这里把它挪回真实原因那一处
# （docs/development.md 的「需要什么」一节也这么写）。
init:
	@if [ -f $(ENV_FILE) ]; then \
	  echo "$(ENV_FILE) 已存在，不重建"; \
	  if grep -qE '($(MASTER_KEY_PLACEHOLDER)|$(DB_PASSWORD_PLACEHOLDER))' $(ENV_FILE); then \
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
	  command -v openssl >/dev/null 2>&1 || { \
	    echo "错误：生成主密钥需要 openssl，这个命令不在 PATH 里"; \
	    echo "      装上它再重跑（Debian/Ubuntu: apt install openssl）。$(ENV_FILE) 没有创建"; \
	    exit 1; \
	  }; \
	  key=$$(openssl rand -base64 32) || { \
	    echo "错误：openssl rand 没能生成主密钥。$(ENV_FILE) 没有创建"; \
	    exit 1; \
	  }; \
	  sed -e 's|$(DB_PASSWORD_PLACEHOLDER)|$(ESCAPED_DB_PASSWORD)|' \
	      -e "s|$(MASTER_KEYS_PLACEHOLDER)|$$key|" \
	      -e "s|:5432/larkax|:$(PG_PORT)/$(PG_DB)|" \
	      $(TEMPLATE) > $(ENV_FILE); \
	  grep -q ":$(PG_PORT)/$(PG_DB)" $(ENV_FILE) || { \
	    echo "$(TEMPLATE) 的 DSN 里没有 \":5432/larkax\"，端口与库名没能替换进去"; \
	    rm -f $(ENV_FILE); \
	    exit 1; \
	  }; \
	  grep -qE '^LARKAX_MASTER_KEYS="[A-Za-z0-9+/]{40,}={0,2}"' $(ENV_FILE) || { \
	    echo "错误：主密钥没能生成进 $(ENV_FILE)，那份文件已删掉"; \
	    echo "      查 $(TEMPLATE) 里主密钥的占位符与 Makefile 顶部的 MASTER_KEYS_PLACEHOLDER 是不是同一串"; \
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
# 下面的兜底值由 internal/config/makefile_ports_test.go 与 config 的默认值钉在
# 一起：只改一边 CI 会红。（这里只取冒号后面那段端口。）
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

# 发布产物：构建 → 校验版本注入 → 打包 → 删掉裸二进制 → 算校验和。
#
# **发布配方只有这一份。** .github/workflows/release.yml 不再自己列一遍这些步骤，
# 只调这个目标（`make dist VERSION=<tag> ARCHS="amd64 arm64"`）。原先两处各写一份，
# 改一处得记得改另一处，而漂移之后没有任何门禁会响：本地演练出来的包与真发出去的
# 包不一样，只有在用户下载之后才发现。
#
# ARCHS：打哪些架构，默认只打 amd64（本地演练够用）。
#
# VERIFY_EXEC：要不要把刚打出来的二进制**真执行**一次，比对它自称的版本。只有
# 「正在打的架构就是本机架构、且本机是 linux」时执行得起来，其余架构打印一句
# 「跳过执行校验（架构不同）」——静默跳过与「校验通过」在日志里长得一样
# （LESSONS.md 第 16 条）。整个关掉用 VERIFY_EXEC=0。
#
# 校验必须在**打包之前**：验的是裸二进制，而它打完包就删了。漏掉这一步的话，
# 发布出去的二进制会自称 dev，而这只有在用户报问题时才会被发现。
#
# 同一个 tag 打两次要得到同一个包：四样都不许随环境变——成员时间戳（tar 默认取
# 文件自己的 mtime，那是打包那一刻）、权限位（tar 默认取文件自己的，而那是 cp 与
# mkdir 时的 umask 留下的）、条目顺序（默认按目录顺序）、属主（默认带上构建机的
# uid/gid）。它们不固定时，包的内容一样而 sha256 不同，校验和就只能证明「下载没
# 坏」，证明不了「手上这一份与发布的那一份是同一个」。所以下面固定 umask，并把
# 那五个选项交给 tar。顶层再套一个以包名命名的目录：解到同一个位置的两个版本
# 不会互相覆盖。
#
# 包里除两个二进制之外还有 LICENSE、README.md 与 man/man1/ 下的手页。手页是
# **用户面文档**的一部分：只躺在源码树里的话，装了发布版的人
# `man larkaxctl-channel` 翻不到，而那多半会被他当成「自己没装对」。
#
# 手页取仓库里**已提交**的那一份（`man/man1/*.1`），不在这里跑生成器：手页是
# 产物，它与源文件是不是同一次生成由 scripts/check-man-pages.sh 保证。发布时再
# 生成一次，等于给要发出去的那一包多一个变量，而这一次生成后面没有第二道门。
#
# VERSION 默认 dev：未注入的二进制也自称 dev（见 docs/versioning.md），于是
# 「忘了给版本」在本地演练里就现形，而不是等发布出去才发现。
#
# 发布物只有 tarball 一条安装路径（裸二进制删掉，见下），校验和才盖得住发出去的
# 东西：两份产物等于两条安装故事，而「有校验和的那个」与「用户脚本里可能正在
# curl 的那个」不是同一个东西时，校验和就成了摆设。
#
# tar 用的是 GNU 的那几个选项（`--sort=name`、`--mtime=@0` …），macOS 自带的
# bsdtar 不认；在那类主机上跑要先装 GNU tar。
#
# 开头那句 `rm -rf dist` 连本地放在 dist/ 里的别的东西一起删；dist/ 在
# .gitignore 里，删它不脏树，但别把要留着的东西放在那儿。
#
# 跑完之后 dist/ 里就是发布那份资产清单：每个架构一个 tarball 与一份 SHA256SUMS
# ——发布那一步发的是 `dist/*`，本地看到的与线上的一致。
VERSION     ?= dev
ARCHS       ?= amd64
VERIFY_EXEC ?= 1

# 版本注入只有这一行。dist、install 与 Dockerfile 都用它（最后那个经
# `print-ldflags` 取）：三处各写一份的话，漂移的症状是「二进制自称 dev」，
# 而它只在对着版本号排查时才现形。
LDFLAGS := -s -w -X main.version=$(VERSION)

# 本机是什么，决定哪个架构能真执行。取 GOHOSTARCH / GOHOSTOS 而不是 GOARCH /
# GOOS：后两者会被环境里的交叉编译设置改掉，那就正好把「能不能跑」判断反了
# ——判断反了不会报错，只会少校验一次而看起来一切正常。
HOSTARCH := $(shell go env GOHOSTARCH)
HOSTOS   := $(shell go env GOHOSTOS)

dist:
	@rm -rf dist
	@mkdir -p dist
	@set -e; \
	umask 022; \
	root=$$(mktemp -d); \
	trap 'rm -rf "$$root"' EXIT; \
	for arch in $(ARCHS); do \
	  for pair in "larkax:./cmd/larkax" "larkaxctl:./cmd/larkaxctl"; do \
	    name=$${pair%%:*}; pkg=$${pair#*:}; \
	    GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
	      -ldflags "$(LDFLAGS)" \
	      -o "dist/$${name}_linux_$${arch}" "$$pkg"; \
	  done; \
	  if [ "$(VERIFY_EXEC)" = 0 ]; then \
	    echo "跳过执行校验（VERIFY_EXEC=0）：dist/larkax_linux_$${arch}"; \
	  elif [ "$$arch" = "$(HOSTARCH)" ] && [ "$(HOSTOS)" = linux ]; then \
	    if ! dist/larkax_linux_$${arch} --version | grep -qx "$(VERSION)"; then \
	      echo "版本注入失败：二进制自称 $$(dist/larkax_linux_$${arch} --version)，期望 $(VERSION)"; \
	      exit 1; \
	    fi; \
	  else \
	    echo "跳过执行校验（架构不同）：dist/larkax_linux_$${arch} 在 $(HOSTOS)/$(HOSTARCH) 上跑不起来"; \
	  fi; \
	  stage="$$root/larkax_$(VERSION)_linux_$${arch}"; \
	  mkdir -p "$$stage/man/man1"; \
	  cp "dist/larkax_linux_$${arch}" "$$stage/larkax"; \
	  cp "dist/larkaxctl_linux_$${arch}" "$$stage/larkaxctl"; \
	  chmod 0755 "$$stage/larkax" "$$stage/larkaxctl"; \
	  cp LICENSE README.md "$$stage/"; \
	  cp man/man1/*.1 "$$stage/man/man1/"; \
	  tar -czf "dist/larkax_$(VERSION)_linux_$${arch}.tar.gz" \
	    --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
	    -C "$$root" "larkax_$(VERSION)_linux_$${arch}"; \
	done; \
	rm -f dist/larkax_linux_* dist/larkaxctl_linux_*; \
	cd dist && sha256sum *.tar.gz > SHA256SUMS; \
	echo "已打出："; \
	ls -1 *.tar.gz

# 控制台的发布产物：`npm ci` → Vite 构建 → 打成与 dist 同形状的 tar.gz。
#
# 与 dist 分开成一个目标，是因为它要 Node，而 `make dist` 只要能跑 Go 的环境。
# 只想练二进制时不必先把 node_modules 装上。
#
# VITE_API_MODE 默认 http：控制台不显式给 http 就用 mock 客户端，而那份产物
# 照样能跑、只是数字是演示数据。所以构建完立刻查一次产物里有没有只有 mock
# 才有的固定用户名（`演示账号`），比看构建日志可靠。
#
# 打包参数与 dist 同一个理由：同一个版本打两次要得到同一个包。
VITE_API_MODE ?= http
NPM           ?= npm

# 只构建控制台，不打包。Dockerfile 的控制台阶段调这一条：镜像里的页面
# 与 `make web` 出的包必须来自同一份配方，否则「镜像里是旧前端」这种问题
# 只能靠用户报。
web-build:
	@cd web && $(NPM) ci && VITE_API_MODE=$(VITE_API_MODE) $(NPM) run build
	@if [ "$(VITE_API_MODE)" = http ] && grep -rqs '演示账号' web/dist/assets; then \
	  echo "产物里带着演示数据（MockApiClient），VITE_API_MODE 没生效"; \
	  exit 1; \
	fi

web: web-build
	@mkdir -p dist
	@set -e; \
	umask 022; \
	root=$$(mktemp -d); \
	trap 'rm -rf "$$root"' EXIT; \
	stage="$$root/larkax-web_$(VERSION)"; \
	mkdir -p "$$stage"; \
	cp -R web/dist "$$stage/dist"; \
	cp LICENSE "$$stage/"; \
	tar -czf "dist/larkax-web_$(VERSION).tar.gz" \
	  --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
	  -C "$$root" "larkax-web_$(VERSION)"; \
	echo "已打出：dist/larkax-web_$(VERSION).tar.gz"

# 发布用的全部资产：两个二进制与控制台，外加覆盖两者的校验和。
#
# 顺序有意义：dist 开头会 `rm -rf dist`，所以它必须先跑完；web 之后才补上
# 第三个 tarball，然后一次重算校验和。反过来先 web 再 dist，那份控制台包会
# 被删掉，而日志里没有任何一步失败。
release: dist web
	@cd dist && sha256sum *.tar.gz > SHA256SUMS && echo "已打出：" && ls -1 *.tar.gz

# 打印 ldflags，给 Dockerfile 用。版本注入点只有一处（见 LDFLAGS）：镜像里的
# 二进制与 `make dist` 出的那一份必须自称同一个版本，而 Dockerfile 里再写一份
# `-X main.version=` 就是第二个注入点。
print-ldflags:
	@echo '$(LDFLAGS)'

clean:
	@docker compose down --volumes --remove-orphans
	@rm -f $(ENV_FILE)
	@echo "已清理：容器、数据卷与 $(ENV_FILE)"
