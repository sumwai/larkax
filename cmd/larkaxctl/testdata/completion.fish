# larkaxctl 的 fish 补全。由 `larkaxctl completion fish` 生成，不要手工修改。
complete -c larkaxctl -f

complete -c larkaxctl -n '__fish_use_subcommand' -a 'doctor' -d '系统健康与链路连通性体检'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'settings' -d '运行期动态配置项管理（settings 表）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'admin' -d '内部监听上 admin API 的令牌（create、revoke）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'version' -d '打印版本号与编译元数据'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'completion' -d '打印 bash / zsh / fish 的补全脚本'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'channel' -d '上游渠道管理（网关、凭证、关联模型与出站映射）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'model' -d '规范模型目录（catalog 基础定义）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'alias' -d '出站映射的旧入口（配置命令在 channel map）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'price' -d '模型计价规则与日历管理（含 calendar 子命令）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'upstream-cost' -d '上游成本规则（按渠道与模型记成本价）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'user' -d '系统用户管理（创建、查询、修改与销户）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'apikey' -d 'API Key 访问密钥、状态与额度管理'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'wallet' -d '用户钱包账户与入账管理'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'plan' -d '额度包、订阅与扣费顺序管理'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'usage' -d '用量与计费的只读查询（事件、请求与汇总）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'ledger' -d '财务账本管理（余额查询、分录校验、对账、对账历史）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'migrate' -d '数据库版本迁移（up、down、status、version）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'partition' -d '数据库月度分区维护（check、ensure）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'secret' -d '安全凭证与主密钥维护（rotate 密钥轮换）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'maintenance' -d '数据留存与过期清理（idempotency 幂等记录、session 控制台会话）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'snapshot' -d '请求结构快照管理（cleanup、allowlist、get、redact）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'consent' -d '原样采集请求正文的授权（grant、list、revoke）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'plugin' -d '动态插件系统管理（list、get、publish、enable、disable、delete、kv）'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'reconcile'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'rotate'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'idempotency'
complete -c larkaxctl -n '__fish_use_subcommand' -a 'calendar'
complete -c larkaxctl -n '__fish_seen_subcommand_from admin' -a 'token'
complete -c larkaxctl -n '__fish_seen_subcommand_from admin and __fish_seen_subcommand_from token' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from admin and __fish_seen_subcommand_from token' -a 'revoke'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias' -a 'resolve'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from calendar' -a 'import'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'health'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'restore'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'sync-models'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'upstream-models'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'adapters'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'probe'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel' -a 'map'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from map' -a 'add'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from map' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from map' -a 'remove'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent' -a 'grant'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent' -a 'revoke'
complete -c larkaxctl -n '__fish_seen_subcommand_from idempotency' -a 'cleanup'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger' -a 'balance'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger' -a 'verify'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger' -a 'reconcile'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger' -a 'runs'
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance' -a 'idempotency'
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance' -a 'session'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'up'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'down'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'down-to'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'up-to'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'status'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'version'
complete -c larkaxctl -n '__fish_seen_subcommand_from migrate' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'onboard'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'alias'
complete -c larkaxctl -n '__fish_seen_subcommand_from model' -a 'trace'
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from alias' -a 'add'
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from alias' -a 'remove'
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from alias' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from partition' -a 'check'
complete -c larkaxctl -n '__fish_seen_subcommand_from partition' -a 'ensure'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'grant'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'void'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'subscriptions'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'key'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan' -a 'order'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from key' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from key' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from order' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from order' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'publish'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin' -a 'kv'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'enable'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'explain'
complete -c larkaxctl -n '__fish_seen_subcommand_from price' -a 'calendar'
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from calendar' -a 'import'
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from calendar' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from secret' -a 'rotate'
complete -c larkaxctl -n '__fish_seen_subcommand_from settings' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from settings' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from settings' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot' -a 'cleanup'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot' -a 'allowlist'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot' -a 'access-log'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot' -a 'redact'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost' -a 'disable'
complete -c larkaxctl -n '__fish_seen_subcommand_from usage' -a 'events'
complete -c larkaxctl -n '__fish_seen_subcommand_from usage' -a 'requests'
complete -c larkaxctl -n '__fish_seen_subcommand_from usage' -a 'summary'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'get'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'inspect'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'update'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'password'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'unlock'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'delete'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'plan'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'wallet'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'key'
complete -c larkaxctl -n '__fish_seen_subcommand_from user' -a 'usage'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete' -a 'request'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete' -a 'revert'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete' -a 'purge'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key' -a 'plans'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from plans' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from plans' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan' -a 'grant'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan' -a 'void'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan' -a 'order'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from order' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from order' -a 'set'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage' -a 'events'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage' -a 'requests'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet' -a 'show'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet' -a 'credit'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet' -a 'adjust'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet' -a 'refund'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'credit'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'adjust'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'refund'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'payout'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'payment'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet' -a 'list'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment' -a 'create'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment' -a 'status'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout' -a 'request'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout' -a 'settle'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout' -a 'approve'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout' -a 'reject'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout' -a 'paid'
complete -c larkaxctl -l env-file -r
complete -c larkaxctl -l format
complete -c larkaxctl -l quiet
complete -c larkaxctl -l verbose
complete -c larkaxctl -l format -x -a 'table json'
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from create' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from create' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from create' -l overrides -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from create' -l upstream -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from delete' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from list' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from list' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from resolve' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from resolve' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from update' -l overrides -r
complete -c larkaxctl -n '__fish_seen_subcommand_from alias and __fish_seen_subcommand_from update' -l upstream -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l rpm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from get' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l clear-quota
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l clear-rpm
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l rpm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l protocols
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l setting -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l type
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l weight -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from delete' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from disable' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from enable' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from get' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from health' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from health' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from health' -l since -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from list' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from map and __fish_seen_subcommand_from add' -l overrides -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from map and __fish_seen_subcommand_from list' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from probe' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from probe' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from probe' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from probe' -l type -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from restore' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from sync-models' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l protocols
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l setting -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l type
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l weight -r
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from upstream-models' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l agree
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l request-id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l scope
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l base-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l credential-env -r
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l credential-stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l endpoint
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l live
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from idempotency and __fish_seen_subcommand_from cleanup' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from balance' -l account -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from balance' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from balance' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from runs' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from verify' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from verify' -l max-txns -r
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from verify' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance and __fish_seen_subcommand_from idempotency' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from maintenance and __fish_seen_subcommand_from session' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l audio
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l batch
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l cache
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l context-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l display -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l endpoints -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l family -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l input-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l input-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l knowledge-cutoff -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l output-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l output-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l reasoning
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l release-date -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l tools
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l vendor -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from create' -l vision
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from delete' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from get' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l cost-expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l display -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l price-expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l update
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from onboard' -l upstream -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from trace' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l audio
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l batch
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l cache
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l context-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l display -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l endpoints -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l family -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l input-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l input-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l knowledge-cutoff -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l output-limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l output-modalities -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l reasoning
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l release-date -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l tools
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l vendor -r
complete -c larkaxctl -n '__fish_seen_subcommand_from model and __fish_seen_subcommand_from update' -l vision
complete -c larkaxctl -n '__fish_seen_subcommand_from partition and __fish_seen_subcommand_from check' -l months -r
complete -c larkaxctl -n '__fish_seen_subcommand_from partition and __fish_seen_subcommand_from ensure' -l months -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l description -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l face-value -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l unit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from disable' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from enable' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from get' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l plan -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from key and __fish_seen_subcommand_from set' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from key and __fish_seen_subcommand_from set' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from key and __fish_seen_subcommand_from set' -l plans -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from key and __fish_seen_subcommand_from show' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from set' -l clear
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from set' -l order -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from set' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from show' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from subscriptions' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from delete' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from delete' -l plugin -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from delete' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from get' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from get' -l plugin -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from get' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from set' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from set' -l plugin -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from set' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from set' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from kv and __fish_seen_subcommand_from set' -l value -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from publish' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from publish' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from plugin and __fish_seen_subcommand_from publish' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l disabled
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l schedule-file -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l scope
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from explain' -l at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from explain' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from explain' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from explain' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l clear-effective-to
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l clear-schedule
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l disable
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l enable
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l schedule-file -r
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l scope
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from rotate' -l apply
complete -c larkaxctl -n '__fish_seen_subcommand_from rotate' -l batch-size -r
complete -c larkaxctl -n '__fish_seen_subcommand_from rotate' -l verify
complete -c larkaxctl -n '__fish_seen_subcommand_from secret and __fish_seen_subcommand_from rotate' -l apply
complete -c larkaxctl -n '__fish_seen_subcommand_from secret and __fish_seen_subcommand_from rotate' -l batch-size -r
complete -c larkaxctl -n '__fish_seen_subcommand_from secret and __fish_seen_subcommand_from rotate' -l verify
complete -c larkaxctl -n '__fish_seen_subcommand_from settings and __fish_seen_subcommand_from get' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from settings and __fish_seen_subcommand_from set' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from settings and __fish_seen_subcommand_from set' -l key -r
complete -c larkaxctl -n '__fish_seen_subcommand_from settings and __fish_seen_subcommand_from set' -l value -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from access-log' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from access-log' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from access-log' -l request-id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from access-log' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from access-log' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from allowlist' -l direction
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from allowlist' -l surface
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from cleanup' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from get' -l actor -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from get' -l direction
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from get' -l reason -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from redact' -l direction
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from redact' -l max-bytes -r
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from redact' -l surface
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from create' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from create' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from create' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from create' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from create' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from create' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from list' -l at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from list' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from list' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l channel -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l clear-effective-to
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l effective-from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l effective-to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l expr -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l models -r
complete -c larkaxctl -n '__fish_seen_subcommand_from upstream-cost and __fish_seen_subcommand_from update' -l priority -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l since -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l status -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from summary' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from summary' -l this-month
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from summary' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from summary' -l today
complete -c larkaxctl -n '__fish_seen_subcommand_from usage and __fish_seen_subcommand_from summary' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from create' -l email -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from create' -l generate-password
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from create' -l role
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete and __fish_seen_subcommand_from purge' -l confirm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete and __fish_seen_subcommand_from purge' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete and __fish_seen_subcommand_from request' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from delete and __fish_seen_subcommand_from revert' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from get' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l quota -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l rpm -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from plans and __fish_seen_subcommand_from set' -l all
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from plans and __fish_seen_subcommand_from set' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from plans and __fish_seen_subcommand_from set' -l plans -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from key and __fish_seen_subcommand_from plans and __fish_seen_subcommand_from show' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from list' -l role
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from list' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from password' -l generate
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from password' -l stdin
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from password' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l expires-at -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l forever
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l plan -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l ttl -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from grant' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from set' -l clear
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from set' -l order -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from set' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from order and __fish_seen_subcommand_from show' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l status -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from unlock' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l admin
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l display-name -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l email -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l merchant
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l no-admin
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l no-merchant
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l status
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from events' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l from -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l limit -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l model -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l since -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l status -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l to -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from usage and __fish_seen_subcommand_from requests' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l gateway -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l merchant -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l merchant-share -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l upstream-cost -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from show' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from create' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l gateway -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from list' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l notify-url -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from status' -l order -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from approve' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from approve' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from approve' -l note -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from paid' -l gateway -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from paid' -l gateway-ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from paid' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from reject' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from reject' -l id -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from reject' -l note -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l destination -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l by -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l destination -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l note -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from settle' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l amount -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l asset
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l auto-ref
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l dry-run
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l memo -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l merchant -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l merchant-share -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l ref -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l upstream-cost -r
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l user -r
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from create' -l status -x -a 'active disabled revoked'
complete -c larkaxctl -n '__fish_seen_subcommand_from apikey and __fish_seen_subcommand_from update' -l status -x -a 'active disabled revoked'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l protocols -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from create' -l type -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l protocols -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from channel and __fish_seen_subcommand_from update' -l type -x -a 'anthropic openai openai-responses systemone'
complete -c larkaxctl -n '__fish_seen_subcommand_from consent and __fish_seen_subcommand_from grant' -l scope -x -a 'single_request time_window'
complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l endpoint -x -a 'chat messages responses'
complete -c larkaxctl -n '__fish_seen_subcommand_from ledger and __fish_seen_subcommand_from balance' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from create' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from plan and __fish_seen_subcommand_from void' -l status -x -a 'expired canceled'
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from create' -l scope -x -a 'platform merchant channel'
complete -c larkaxctl -n '__fish_seen_subcommand_from price and __fish_seen_subcommand_from update' -l scope -x -a 'platform merchant channel'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from allowlist' -l direction -x -a 'request response'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from allowlist' -l surface -x -a 'chat decision'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from get' -l direction -x -a 'request response'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from get' -l surface -x -a 'chat decision'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from redact' -l direction -x -a 'request response'
complete -c larkaxctl -n '__fish_seen_subcommand_from snapshot and __fish_seen_subcommand_from redact' -l surface -x -a 'chat decision'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from create' -l role -x -a 'user merchant admin'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from list' -l role -x -a 'user merchant admin'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from list' -l status -x -a 'active suspended banned'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l role -x -a 'user merchant admin'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from update' -l status -x -a 'active suspended banned'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from user and __fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from adjust' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from create' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from credit' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payment and __fish_seen_subcommand_from create' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from payout and __fish_seen_subcommand_from request' -l asset -x -a 'CNY'
complete -c larkaxctl -n '__fish_seen_subcommand_from wallet and __fish_seen_subcommand_from refund' -l asset -x -a 'CNY'
