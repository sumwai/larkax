# larkaxctl 的 fish 补全。由 `larkaxctl completion fish` 生成，不要手工修改。
complete -c larkaxctl -f

complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'doctor' -d '系统健康与链路连通性体检'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'settings' -d '运行期动态配置项管理（settings 表）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'admin' -d '内部监听上 admin API 的令牌（create、revoke）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'version' -d '打印版本号与编译元数据'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'completion' -d '打印 bash / zsh / fish 的补全脚本'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'channel' -d '上游渠道管理（网关、凭证、关联模型与出站映射）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'model' -d '规范模型目录（catalog 基础定义）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'alias' -d '出站映射的旧入口（配置命令在 channel map）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'price' -d '模型计价规则与日历管理（含 calendar 子命令）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'upstream-cost' -d '上游成本规则（按渠道与模型记成本价）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'user' -d '系统用户管理（创建、查询、修改与销户）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'apikey' -d 'API Key 访问密钥、状态与额度管理'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'wallet' -d '用户钱包账户与入账管理'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'plan' -d '额度包、订阅与扣费顺序管理'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'usage' -d '用量与计费的只读查询（事件、请求与汇总）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'ledger' -d '财务账本管理（余额查询、分录校验、对账、对账历史）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'migrate' -d '数据库版本迁移（up、down、status、version）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'partition' -d '数据库月度分区维护（check、ensure）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'secret' -d '安全凭证与主密钥维护（rotate 密钥轮换）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'maintenance' -d '数据留存与过期清理（idempotency 幂等记录、session 控制台会话）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'snapshot' -d '请求结构快照管理（cleanup、allowlist、get、redact）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'consent' -d '原样采集请求正文的授权（grant、list、revoke）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'plugin' -d '动态插件系统管理（list、get、publish、enable、disable、delete、kv）'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'reconcile'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'rotate'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'idempotency'
complete -c larkaxctl -n 'not __fish_seen_subcommand_from doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar' -a 'calendar'
complete -c larkaxctl -n '__fish_seen_subcommand_from admin; and not __fish_seen_subcommand_from token' -a 'token'
complete -c larkaxctl -n '__fish_seen_subcommand_from admin; and __fish_seen_subcommand_from token; and not __fish_seen_subcommand_from create revoke' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from admin; and __fish_seen_subcommand_from token; and not __fish_seen_subcommand_from create revoke' -a 'revoke'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from create delete list resolve update' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from create delete list resolve update' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from create delete list resolve update' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from create delete list resolve update' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from create delete list resolve update' -a 'resolve'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and not __fish_seen_subcommand_from create get list update' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and not __fish_seen_subcommand_from create get list update' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and not __fish_seen_subcommand_from create get list update' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and not __fish_seen_subcommand_from create get list update' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from calendar; and not __fish_seen_subcommand_from import' -a 'import'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'health'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'restore'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'sync-models'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'upstream-models'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'adapters'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'probe'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and not __fish_seen_subcommand_from adapters create delete disable enable get health list map probe restore sync-models update upstream-models' -a 'map'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from map; and not __fish_seen_subcommand_from add list remove' -a 'add'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from map; and not __fish_seen_subcommand_from add list remove' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from map; and not __fish_seen_subcommand_from add list remove' -a 'remove'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and not __fish_seen_subcommand_from grant list revoke' -a 'grant'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and not __fish_seen_subcommand_from grant list revoke' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and not __fish_seen_subcommand_from grant list revoke' -a 'revoke'
complete -c larkaxctl -n '__fish_seen_subcommand_from idempotency; and not __fish_seen_subcommand_from cleanup' -a 'cleanup'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and not __fish_seen_subcommand_from balance reconcile runs verify' -a 'balance'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and not __fish_seen_subcommand_from balance reconcile runs verify' -a 'verify'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and not __fish_seen_subcommand_from balance reconcile runs verify' -a 'reconcile'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and not __fish_seen_subcommand_from balance reconcile runs verify' -a 'runs'
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance; and not __fish_seen_subcommand_from idempotency session' -a 'idempotency'
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance; and not __fish_seen_subcommand_from idempotency session' -a 'session'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'up'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'down'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'down-to'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'up-to'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'status'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'version'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate; and not __fish_seen_subcommand_from create down down-to status up up-to version' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'onboard'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'alias'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and not __fish_seen_subcommand_from alias create delete get list onboard trace update' -a 'trace'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from add list remove' -a 'add'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from add list remove' -a 'remove'
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from alias; and not __fish_seen_subcommand_from add list remove' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from partition; and not __fish_seen_subcommand_from check ensure' -a 'check'
complete -c larkaxctl -n '__fish_seen_subcommand_from partition; and not __fish_seen_subcommand_from check ensure' -a 'ensure'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'grant'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'void'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'subscriptions'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'key'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from create disable enable get grant key list order subscriptions void' -a 'order'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from key; and not __fish_seen_subcommand_from set show' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from key; and not __fish_seen_subcommand_from set show' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and not __fish_seen_subcommand_from set show' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and not __fish_seen_subcommand_from set show' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'publish'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and not __fish_seen_subcommand_from delete disable enable get kv list publish' -a 'kv'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and not __fish_seen_subcommand_from delete get set' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and not __fish_seen_subcommand_from delete get set' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and not __fish_seen_subcommand_from delete get set' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'add'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'explain'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and not __fish_seen_subcommand_from add calendar disable enable explain list show update' -a 'calendar'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from calendar; and not __fish_seen_subcommand_from import list' -a 'import'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from calendar; and not __fish_seen_subcommand_from import list' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from secret; and not __fish_seen_subcommand_from rotate' -a 'rotate'
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and not __fish_seen_subcommand_from get list set' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and not __fish_seen_subcommand_from get list set' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and not __fish_seen_subcommand_from get list set' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and not __fish_seen_subcommand_from access-log allowlist cleanup get redact' -a 'cleanup'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and not __fish_seen_subcommand_from access-log allowlist cleanup get redact' -a 'allowlist'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and not __fish_seen_subcommand_from access-log allowlist cleanup get redact' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and not __fish_seen_subcommand_from access-log allowlist cleanup get redact' -a 'access-log'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and not __fish_seen_subcommand_from access-log allowlist cleanup get redact' -a 'redact'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and not __fish_seen_subcommand_from create disable list update' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and not __fish_seen_subcommand_from create disable list update' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and not __fish_seen_subcommand_from create disable list update' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and not __fish_seen_subcommand_from create disable list update' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and not __fish_seen_subcommand_from events requests summary' -a 'events'
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and not __fish_seen_subcommand_from events requests summary' -a 'requests'
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and not __fish_seen_subcommand_from events requests summary' -a 'summary'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'inspect'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'password'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'unlock'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'plan'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'wallet'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'key'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and not __fish_seen_subcommand_from create delete get inspect key list password plan unlock update usage wallet' -a 'usage'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and not __fish_seen_subcommand_from purge request revert' -a 'request'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and not __fish_seen_subcommand_from purge request revert' -a 'revert'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and not __fish_seen_subcommand_from purge request revert' -a 'purge'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and not __fish_seen_subcommand_from create list plans' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and not __fish_seen_subcommand_from create list plans' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and not __fish_seen_subcommand_from create list plans' -a 'plans'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from plans; and not __fish_seen_subcommand_from set show' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from plans; and not __fish_seen_subcommand_from set show' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from grant list order void' -a 'grant'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from grant list order void' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from grant list order void' -a 'void'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and not __fish_seen_subcommand_from grant list order void' -a 'order'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and not __fish_seen_subcommand_from set show' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and not __fish_seen_subcommand_from set show' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and not __fish_seen_subcommand_from events requests' -a 'events'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and not __fish_seen_subcommand_from events requests' -a 'requests'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust credit refund show' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust credit refund show' -a 'credit'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust credit refund show' -a 'adjust'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust credit refund show' -a 'refund'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'credit'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'adjust'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'refund'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'payout'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'payment'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and not __fish_seen_subcommand_from adjust create credit list payment payout refund' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and not __fish_seen_subcommand_from create status' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and not __fish_seen_subcommand_from create status' -a 'status'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and not __fish_seen_subcommand_from approve paid reject request settle' -a 'request'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and not __fish_seen_subcommand_from approve paid reject request settle' -a 'settle'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and not __fish_seen_subcommand_from approve paid reject request settle' -a 'approve'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and not __fish_seen_subcommand_from approve paid reject request settle' -a 'reject'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and not __fish_seen_subcommand_from approve paid reject request settle' -a 'paid'
complete -c larkaxctl -l env-file -r
complete -c larkaxctl -l format
complete -c larkaxctl -l quiet
complete -c larkaxctl -l verbose
complete -c larkaxctl -l format -x -a 'table json'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from create' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from create' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from create' -l overrides -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from create' -l upstream -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from delete' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from list' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from list' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from resolve' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from resolve' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from update' -l overrides -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from update' -l upstream -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l rpm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from get' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l clear-quota
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l clear-rpm
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l rpm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l protocols
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l setting -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l type
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l weight -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from delete' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from disable' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from enable' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from get' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from health' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from health' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from health' -l since -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from list' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from map; and __fish_seen_subcommand_from add' -l overrides -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from map; and __fish_seen_subcommand_from list' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from probe' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from probe' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from probe' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from probe' -l type -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from restore' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from sync-models' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l protocols
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l setting -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l type
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l weight -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from upstream-models' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l agree
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l request-id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l scope
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l endpoint
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l live
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from idempotency; and __fish_seen_subcommand_from cleanup' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from balance' -l account -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from balance' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from balance' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from runs' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from verify' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from verify' -l max-txns -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from verify' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance; and __fish_seen_subcommand_from idempotency' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance; and __fish_seen_subcommand_from session' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l audio
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l batch
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l cache
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l context-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l display -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l endpoints -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l family -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l input-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l input-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l knowledge-cutoff -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l output-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l output-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l reasoning
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l release-date -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l tools
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l vendor -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from create' -l vision
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from delete' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from get' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l cost-expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l display -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l price-expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l update
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from onboard' -l upstream -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from trace' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l audio
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l batch
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l cache
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l context-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l display -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l endpoints -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l family -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l input-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l input-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l knowledge-cutoff -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l output-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l output-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l reasoning
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l release-date -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l tools
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l vendor -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model; and __fish_seen_subcommand_from update' -l vision
complete -c larkaxctl -n '__fish_seen_subcommand_from partition; and __fish_seen_subcommand_from check' -l months -r
complete -c larkaxctl -n '__fish_seen_subcommand_from partition; and __fish_seen_subcommand_from ensure' -l months -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l description -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l face-value -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l unit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from disable' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from enable' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from get' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l plan -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from set' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from set' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from set' -l plans -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from show' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from set' -l clear
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from set' -l order -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from set' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from show' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from subscriptions' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from delete' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from delete' -l plugin -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from delete' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from get' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from get' -l plugin -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from get' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from set' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from set' -l plugin -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from set' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from set' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from kv; and __fish_seen_subcommand_from set' -l value -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from publish' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from publish' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin; and __fish_seen_subcommand_from publish' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l cr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l cw -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l disabled
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l in -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l out -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l reasoning -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l schedule-file -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l scope
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l unit
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from explain' -l at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from explain' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from explain' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from explain' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l clear-effective-to
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l clear-schedule
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l cr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l cw -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l disable
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l enable
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l in -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l out -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l reasoning -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l schedule-file -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l scope
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l unit
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from rotate' -l apply
complete -c larkaxctl -n '__fish_seen_subcommand_from rotate' -l batch-size -r
complete -c larkaxctl -n '__fish_seen_subcommand_from rotate' -l verify
complete -c larkaxctl -n '__fish_seen_subcommand_from secret; and __fish_seen_subcommand_from rotate' -l apply
complete -c larkaxctl -n '__fish_seen_subcommand_from secret; and __fish_seen_subcommand_from rotate' -l batch-size -r
complete -c larkaxctl -n '__fish_seen_subcommand_from secret; and __fish_seen_subcommand_from rotate' -l verify
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and __fish_seen_subcommand_from get' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and __fish_seen_subcommand_from set' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and __fish_seen_subcommand_from set' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from settings; and __fish_seen_subcommand_from set' -l value -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from access-log' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from access-log' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from access-log' -l request-id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from access-log' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from access-log' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from allowlist' -l direction
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from allowlist' -l surface
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from cleanup' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from get' -l actor -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from get' -l direction
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from get' -l reason -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from redact' -l direction
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from redact' -l max-bytes -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from redact' -l surface
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from create' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from create' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from create' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from create' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from create' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from list' -l at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from list' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from list' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l clear-effective-to
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost; and __fish_seen_subcommand_from update' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l since -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l status -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from summary' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from summary' -l this-month
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from summary' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from summary' -l today
complete -c larkaxctl -n '__fish_seen_subcommand_from usage; and __fish_seen_subcommand_from summary' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from create' -l email -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from create' -l generate-password
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from create' -l role
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and __fish_seen_subcommand_from purge' -l confirm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and __fish_seen_subcommand_from purge' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and __fish_seen_subcommand_from request' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from delete; and __fish_seen_subcommand_from revert' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from get' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l rpm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from plans; and __fish_seen_subcommand_from set' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from plans; and __fish_seen_subcommand_from set' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from plans; and __fish_seen_subcommand_from set' -l plans -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from key; and __fish_seen_subcommand_from plans; and __fish_seen_subcommand_from show' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from list' -l role
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from list' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from password' -l generate
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from password' -l stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from password' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l plan -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from grant' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from set' -l clear
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from set' -l order -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from set' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from order; and __fish_seen_subcommand_from show' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l status -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from unlock' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l admin
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l display-name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l email -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l merchant
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l no-admin
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l no-merchant
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from events' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l since -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l status -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from usage; and __fish_seen_subcommand_from requests' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l gateway -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l merchant -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l merchant-share -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l upstream-cost -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from show' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from create' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l gateway -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l notify-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from status' -l order -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from approve' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from approve' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from approve' -l note -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from paid' -l gateway -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from paid' -l gateway-ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from paid' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from reject' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from reject' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from reject' -l note -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l destination -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l destination -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l note -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from settle' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l merchant -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l merchant-share -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l upstream-cost -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from create' -l status -x -a 'active disabled revoked'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey; and __fish_seen_subcommand_from update' -l status -x -a 'active disabled revoked'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l protocols -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from create' -l type -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l protocols -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel; and __fish_seen_subcommand_from update' -l type -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent; and __fish_seen_subcommand_from grant' -l scope -x -a 'single_request time_window'
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l endpoint -x -a 'chat messages responses'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from balance' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from create' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan; and __fish_seen_subcommand_from void' -l status -x -a 'expired canceled'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l scope -x -a 'platform merchant channel'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from add' -l unit -x -a 'mtok token'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from create' -l scope -x -a 'platform merchant channel'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from create' -l unit -x -a 'mtok token'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l scope -x -a 'platform merchant channel'
complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from update' -l unit -x -a 'mtok token'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from allowlist' -l direction -x -a 'request response'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from allowlist' -l surface -x -a 'chat decision'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from get' -l direction -x -a 'request response'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from get' -l surface -x -a 'chat decision'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from redact' -l direction -x -a 'request response'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot; and __fish_seen_subcommand_from redact' -l surface -x -a 'chat decision'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from create' -l role -x -a 'user merchant admin'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from list' -l role -x -a 'user merchant admin'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from list' -l status -x -a 'active suspended banned'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l role -x -a 'user merchant admin'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from update' -l status -x -a 'active suspended banned'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from user; and __fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from adjust' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from create' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from credit' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payment; and __fish_seen_subcommand_from create' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from payout; and __fish_seen_subcommand_from request' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet; and __fish_seen_subcommand_from refund' -l asset -x -a 'CNY'
