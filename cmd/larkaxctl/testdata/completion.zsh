#compdef larkaxctl
# larkaxctl 的 zsh 补全。由 `larkaxctl completion zsh` 生成，不要手工修改。
_larkaxctl() {
	local cur prev path w
	local i
	cur="${words[CURRENT]}"
	prev="${words[CURRENT-1]}"
	path=""
	for ((i = 2; i < CURRENT; i++)); do
		w="${words[i]}"
		case "$w" in -*) break ;; esac
		path="${path:+$path }$w"
	done

	case "$path|$prev" in
		"apikey create|--status") compadd -- active disabled revoked; return ;;
		"apikey update|--status") compadd -- active disabled revoked; return ;;
		"channel create|--protocols") compadd -- anthropic openai openai-responses systemone; return ;;
		"channel create|--type") compadd -- anthropic openai openai-responses systemone; return ;;
		"channel update|--protocols") compadd -- anthropic openai openai-responses systemone; return ;;
		"channel update|--type") compadd -- anthropic openai openai-responses systemone; return ;;
		"consent grant|--scope") compadd -- single_request time_window; return ;;
		"doctor|--endpoint") compadd -- chat messages responses; return ;;
		"ledger balance|--asset") compadd -- CNY; return ;;
		"plan create|--asset") compadd -- CNY; return ;;
		"plan void|--status") compadd -- expired canceled; return ;;
		"price create|--scope") compadd -- platform merchant channel; return ;;
		"price create|--unit") compadd -- mtok token; return ;;
		"price update|--scope") compadd -- platform merchant channel; return ;;
		"price update|--unit") compadd -- mtok token; return ;;
		"snapshot allowlist|--direction") compadd -- request response; return ;;
		"snapshot allowlist|--surface") compadd -- chat decision; return ;;
		"snapshot get|--direction") compadd -- request response; return ;;
		"snapshot get|--surface") compadd -- chat decision; return ;;
		"snapshot redact|--direction") compadd -- request response; return ;;
		"snapshot redact|--surface") compadd -- chat decision; return ;;
		"user create|--role") compadd -- user merchant admin; return ;;
		"user list|--role") compadd -- user merchant admin; return ;;
		"user list|--status") compadd -- active suspended banned; return ;;
		"user update|--role") compadd -- user merchant admin; return ;;
		"user update|--status") compadd -- active suspended banned; return ;;
		"user wallet adjust|--asset") compadd -- CNY; return ;;
		"user wallet credit|--asset") compadd -- CNY; return ;;
		"user wallet refund|--asset") compadd -- CNY; return ;;
		"wallet adjust|--asset") compadd -- CNY; return ;;
		"wallet create|--asset") compadd -- CNY; return ;;
		"wallet credit|--asset") compadd -- CNY; return ;;
		"wallet payment create|--asset") compadd -- CNY; return ;;
		"wallet payout request|--asset") compadd -- CNY; return ;;
		"wallet refund|--asset") compadd -- CNY; return ;;
		*"|--format") compadd -- table json; return ;;
	esac

	case "$path|$prev" in
		"alias create|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias create|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias create|--overrides")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias create|--upstream")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias delete|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias list|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias list|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias resolve|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias resolve|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias update|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias update|--overrides")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"alias update|--upstream")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey create|--expires-at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey create|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey create|--quota")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey create|--rpm")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey create|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey create|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey get|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey list|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey update|--expires-at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey update|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey update|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey update|--quota")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey update|--rpm")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"apikey update|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--base-url")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--credential-env")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--setting")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel create|--weight")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel delete|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel disable|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel enable|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel get|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel health|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel health|--since")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel map add|--overrides")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel map list|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel probe|--base-url")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel probe|--credential-env")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel probe|--type")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel restore|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel sync-models|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--base-url")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--credential-env")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--setting")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel update|--weight")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"channel upstream-models|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent grant|--expires-at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent grant|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent grant|--request-id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent grant|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent grant|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent grant|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"consent list|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"doctor|--base-url")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"doctor|--credential-env")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"doctor|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"ledger balance|--account")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"ledger runs|--limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"ledger verify|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"ledger verify|--max-txns")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"ledger verify|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--context-limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--display")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--endpoints")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--family")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--input-limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--input-modalities")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--knowledge-cutoff")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--output-limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--output-modalities")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--release-date")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model create|--vendor")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model delete|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model get|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model onboard|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model onboard|--cost-expr")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model onboard|--display")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model onboard|--price-expr")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model onboard|--upstream")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model trace|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--context-limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--display")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--endpoints")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--family")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--input-limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--input-modalities")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--knowledge-cutoff")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--output-limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--output-modalities")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--release-date")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"model update|--vendor")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"partition check|--months")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"partition ensure|--months")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--description")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--face-value")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--quota")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan create|--unit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan disable|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan enable|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan get|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan grant|--expires-at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan grant|--plan")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan grant|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan grant|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan grant|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan key set|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan key set|--plans")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan key show|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan order set|--order")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan order set|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan order show|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan subscriptions|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan void|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plan void|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv delete|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv delete|--plugin")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv delete|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv get|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv get|--plugin")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv get|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv set|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv set|--plugin")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv set|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv set|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin kv set|--value")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin publish|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin publish|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"plugin publish|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price calendar sync|--year")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--cache-read")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--cache-write")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--completion")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--effective-from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--effective-to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--expr")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--prompt")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--reasoning")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--schedule-file")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price create|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price explain|--at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price explain|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price explain|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price explain|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--cache-read")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--cache-write")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--completion")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--effective-from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--effective-to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--expr")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--prompt")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--reasoning")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--schedule-file")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"price update|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"rotate|--batch-size")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"secret rotate|--batch-size")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"settings get|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"settings set|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"settings set|--key")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"settings set|--value")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot access-log|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot access-log|--limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot access-log|--request-id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot access-log|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot access-log|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot get|--actor")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot get|--reason")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"snapshot redact|--max-bytes")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost create|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost create|--effective-from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost create|--effective-to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost create|--expr")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost create|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost create|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost list|--at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost list|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost list|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--channel")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--effective-from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--effective-to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--expr")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--models")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"upstream-cost update|--priority")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage events|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage events|--limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage events|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage events|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--since")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--status")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage requests|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage summary|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage summary|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"usage summary|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user create|--email")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user create|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user delete purge|--confirm")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user delete purge|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user delete request|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user delete revert|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user get|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key create|--expires-at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key create|--name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key create|--quota")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key create|--rpm")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key create|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key create|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key list|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key plans set|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key plans set|--plans")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user key plans show|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user password|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan grant|--expires-at")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan grant|--plan")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan grant|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan grant|--ttl")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan grant|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan list|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan order set|--order")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan order set|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan order show|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan void|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan void|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user plan void|--status")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user unlock|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user update|--display-name")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user update|--email")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user update|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage events|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage events|--limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage events|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage events|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--from")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--limit")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--model")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--since")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--status")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--to")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user usage requests|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet adjust|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet adjust|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet adjust|--memo")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet adjust|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet adjust|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet credit|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet credit|--gateway")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet credit|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet credit|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--memo")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--merchant")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--merchant-share")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--upstream-cost")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet refund|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"user wallet show|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet adjust|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet adjust|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet adjust|--memo")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet adjust|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet adjust|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet create|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet credit|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet credit|--gateway")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet credit|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet credit|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet list|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payment create|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payment create|--notify-url")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payment create|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payment create|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payment status|--order")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout approve|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout approve|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout approve|--note")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout paid|--gateway")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout paid|--gateway-ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout paid|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout reject|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout reject|--id")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout reject|--note")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout request|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout request|--destination")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout request|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout request|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout settle|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout settle|--by")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout settle|--destination")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout settle|--note")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout settle|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet payout settle|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--amount")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--memo")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--merchant")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--merchant-share")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--ref")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--upstream-cost")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		"wallet refund|--user")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
		*"|--env-file")
			case "$cur" in
			-*) ;;
			*) _files; return ;;
			esac
			;;
	esac

	case "$cur" in
	-*)
		case "$path" in
			"alias create") compadd -- --channel --env-file --format --model --overrides --quiet --upstream --verbose ;;
			"alias delete") compadd -- --env-file --format --id --quiet --verbose ;;
			"alias list") compadd -- --channel --env-file --format --model --quiet --verbose ;;
			"alias resolve") compadd -- --channel --env-file --format --model --quiet --verbose ;;
			"alias update") compadd -- --env-file --format --id --overrides --quiet --upstream --verbose ;;
			"apikey create") compadd -- --env-file --expires-at --forever --format --name --quiet --quota --rpm --ttl --user --verbose ;;
			"apikey get") compadd -- --env-file --format --id --quiet --verbose ;;
			"apikey list") compadd -- --env-file --format --quiet --user --verbose ;;
			"apikey update") compadd -- --clear-quota --clear-rpm --env-file --expires-at --forever --format --id --name --quiet --quota --rpm --status --ttl --verbose ;;
			"channel create") compadd -- --base-url --credential-env --credential-stdin --env-file --format --models --name --priority --protocols --quiet --setting --type --user --verbose --weight ;;
			"channel delete") compadd -- --env-file --format --id --quiet --verbose ;;
			"channel disable") compadd -- --env-file --format --id --quiet --verbose ;;
			"channel enable") compadd -- --env-file --format --id --quiet --verbose ;;
			"channel get") compadd -- --env-file --format --id --quiet --verbose ;;
			"channel health") compadd -- --all --env-file --format --id --quiet --since --verbose ;;
			"channel list") compadd -- --all --env-file --format --quiet --verbose ;;
			"channel map add") compadd -- --env-file --format --overrides --quiet --verbose ;;
			"channel map list") compadd -- --env-file --format --model --quiet --verbose ;;
			"channel probe") compadd -- --base-url --credential-env --credential-stdin --env-file --format --quiet --type --verbose ;;
			"channel restore") compadd -- --env-file --format --id --quiet --verbose ;;
			"channel sync-models") compadd -- --env-file --format --id --quiet --verbose ;;
			"channel update") compadd -- --base-url --credential-env --credential-stdin --env-file --format --id --models --name --priority --protocols --quiet --setting --type --verbose --weight ;;
			"channel upstream-models") compadd -- --env-file --format --id --quiet --verbose ;;
			"consent grant") compadd -- --agree --env-file --expires-at --forever --format --from --quiet --request-id --scope --to --ttl --user --verbose ;;
			"consent list") compadd -- --env-file --format --quiet --user --verbose ;;
			"doctor") compadd -- --base-url --credential-env --credential-stdin --endpoint --env-file --format --live --model --quiet --verbose ;;
			"idempotency cleanup") compadd -- --dry-run --env-file --format --quiet --verbose ;;
			"ledger balance") compadd -- --account --all --asset --env-file --format --quiet --verbose ;;
			"ledger runs") compadd -- --env-file --format --limit --quiet --verbose ;;
			"ledger verify") compadd -- --env-file --format --from --max-txns --quiet --to --verbose ;;
			"maintenance idempotency") compadd -- --dry-run --env-file --format --quiet --verbose ;;
			"maintenance session") compadd -- --dry-run --env-file --format --quiet --verbose ;;
			"model create") compadd -- --audio --batch --cache --context-limit --display --endpoints --env-file --family --format --input-limit --input-modalities --knowledge-cutoff --model --output-limit --output-modalities --quiet --reasoning --release-date --tools --vendor --verbose --vision ;;
			"model delete") compadd -- --env-file --format --model --quiet --verbose ;;
			"model get") compadd -- --env-file --format --model --quiet --verbose ;;
			"model onboard") compadd -- --channel --cost-expr --display --dry-run --env-file --format --price-expr --quiet --update --upstream --verbose ;;
			"model trace") compadd -- --channel --env-file --format --quiet --verbose ;;
			"model update") compadd -- --audio --batch --cache --context-limit --display --endpoints --env-file --family --format --input-limit --input-modalities --knowledge-cutoff --model --output-limit --output-modalities --quiet --reasoning --release-date --tools --vendor --verbose --vision ;;
			"partition check") compadd -- --env-file --format --months --quiet --verbose ;;
			"partition ensure") compadd -- --env-file --format --months --quiet --verbose ;;
			"plan create") compadd -- --asset --description --env-file --face-value --format --key --models --name --quiet --quota --unit --verbose ;;
			"plan disable") compadd -- --env-file --format --key --quiet --verbose ;;
			"plan enable") compadd -- --env-file --format --key --quiet --verbose ;;
			"plan get") compadd -- --env-file --format --key --quiet --verbose ;;
			"plan grant") compadd -- --auto-ref --dry-run --env-file --expires-at --forever --format --plan --quiet --ref --ttl --user --verbose ;;
			"plan key set") compadd -- --all --env-file --format --id --plans --quiet --verbose ;;
			"plan key show") compadd -- --env-file --format --id --quiet --verbose ;;
			"plan order set") compadd -- --clear --env-file --format --order --quiet --user --verbose ;;
			"plan order show") compadd -- --env-file --format --quiet --user --verbose ;;
			"plan subscriptions") compadd -- --env-file --format --quiet --user --verbose ;;
			"plan void") compadd -- --auto-ref --dry-run --env-file --format --id --quiet --ref --status --verbose ;;
			"plugin kv delete") compadd -- --env-file --format --key --plugin --quiet --user --verbose ;;
			"plugin kv get") compadd -- --env-file --format --key --plugin --quiet --user --verbose ;;
			"plugin kv set") compadd -- --env-file --format --key --plugin --quiet --ttl --user --value --verbose ;;
			"plugin publish") compadd -- --by --env-file --format --key --priority --quiet --verbose ;;
			"price calendar sync") compadd -- --env-file --format --quiet --verbose --year ;;
			"price create") compadd -- --by --cache-read --cache-write --channel --completion --disabled --effective-from --effective-to --env-file --expr --format --models --name --priority --prompt --quiet --reasoning --schedule-file --scope --unit --user --verbose ;;
			"price explain") compadd -- --at --channel --env-file --format --model --quiet --user --verbose ;;
			"price update") compadd -- --by --cache-read --cache-write --channel --clear-effective-to --clear-schedule --completion --disable --effective-from --effective-to --enable --env-file --expr --format --id --models --name --priority --prompt --quiet --reasoning --schedule-file --scope --unit --user --verbose ;;
			"rotate") compadd -- --apply --batch-size --env-file --format --quiet --verbose --verify ;;
			"secret rotate") compadd -- --apply --batch-size --env-file --format --quiet --verbose --verify ;;
			"settings get") compadd -- --env-file --format --key --quiet --verbose ;;
			"settings set") compadd -- --by --env-file --format --key --quiet --value --verbose ;;
			"snapshot access-log") compadd -- --env-file --format --from --limit --quiet --request-id --to --user --verbose ;;
			"snapshot allowlist") compadd -- --direction --env-file --format --quiet --surface --verbose ;;
			"snapshot cleanup") compadd -- --dry-run --env-file --format --quiet --verbose ;;
			"snapshot get") compadd -- --actor --direction --env-file --format --quiet --reason --verbose ;;
			"snapshot redact") compadd -- --direction --env-file --format --max-bytes --quiet --surface --verbose ;;
			"upstream-cost create") compadd -- --channel --effective-from --effective-to --env-file --expr --format --models --priority --quiet --verbose ;;
			"upstream-cost list") compadd -- --at --channel --env-file --format --model --quiet --verbose ;;
			"upstream-cost update") compadd -- --channel --clear-effective-to --effective-from --effective-to --env-file --expr --format --id --models --priority --quiet --verbose ;;
			"usage events") compadd -- --env-file --format --from --limit --quiet --to --user --verbose ;;
			"usage requests") compadd -- --env-file --format --from --limit --model --quiet --since --status --to --user --verbose ;;
			"usage summary") compadd -- --env-file --format --from --quiet --this-month --to --today --user --verbose ;;
			"user create") compadd -- --email --env-file --format --generate-password --name --quiet --role --verbose ;;
			"user delete purge") compadd -- --confirm --env-file --format --quiet --user --verbose ;;
			"user delete request") compadd -- --env-file --format --quiet --user --verbose ;;
			"user delete revert") compadd -- --env-file --format --quiet --user --verbose ;;
			"user get") compadd -- --env-file --format --quiet --user --verbose ;;
			"user key create") compadd -- --env-file --expires-at --forever --format --name --quiet --quota --rpm --ttl --user --verbose ;;
			"user key list") compadd -- --env-file --format --quiet --user --verbose ;;
			"user key plans set") compadd -- --all --env-file --format --id --plans --quiet --verbose ;;
			"user key plans show") compadd -- --env-file --format --id --quiet --verbose ;;
			"user list") compadd -- --env-file --format --quiet --role --status --verbose ;;
			"user password") compadd -- --env-file --format --generate --quiet --stdin --user --verbose ;;
			"user plan grant") compadd -- --auto-ref --dry-run --env-file --expires-at --forever --format --plan --quiet --ref --ttl --user --verbose ;;
			"user plan list") compadd -- --env-file --format --quiet --user --verbose ;;
			"user plan order set") compadd -- --clear --env-file --format --order --quiet --user --verbose ;;
			"user plan order show") compadd -- --env-file --format --quiet --user --verbose ;;
			"user plan void") compadd -- --auto-ref --dry-run --env-file --format --id --quiet --ref --status --verbose ;;
			"user unlock") compadd -- --env-file --format --quiet --user --verbose ;;
			"user update") compadd -- --admin --display-name --email --env-file --format --merchant --no-admin --no-merchant --quiet --status --user --verbose ;;
			"user usage events") compadd -- --env-file --format --from --limit --quiet --to --user --verbose ;;
			"user usage requests") compadd -- --env-file --format --from --limit --model --quiet --since --status --to --user --verbose ;;
			"user wallet adjust") compadd -- --amount --auto-ref --by --dry-run --env-file --format --memo --quiet --ref --user --verbose ;;
			"user wallet credit") compadd -- --amount --asset --auto-ref --dry-run --env-file --format --gateway --quiet --ref --user --verbose ;;
			"user wallet refund") compadd -- --amount --asset --auto-ref --dry-run --env-file --format --memo --merchant --merchant-share --quiet --ref --upstream-cost --user --verbose ;;
			"user wallet show") compadd -- --env-file --format --quiet --user --verbose ;;
			"wallet adjust") compadd -- --amount --auto-ref --by --dry-run --env-file --format --memo --quiet --ref --user --verbose ;;
			"wallet create") compadd -- --asset --env-file --format --quiet --user --verbose ;;
			"wallet credit") compadd -- --amount --asset --auto-ref --dry-run --env-file --format --gateway --quiet --ref --user --verbose ;;
			"wallet list") compadd -- --env-file --format --quiet --user --verbose ;;
			"wallet payment create") compadd -- --amount --asset --auto-ref --dry-run --env-file --format --notify-url --quiet --ref --user --verbose ;;
			"wallet payment status") compadd -- --env-file --format --order --quiet --verbose ;;
			"wallet payout approve") compadd -- --by --env-file --format --id --note --quiet --verbose ;;
			"wallet payout paid") compadd -- --env-file --format --gateway --gateway-ref --id --quiet --verbose ;;
			"wallet payout reject") compadd -- --by --env-file --format --id --note --quiet --verbose ;;
			"wallet payout request") compadd -- --amount --asset --auto-ref --destination --dry-run --env-file --format --quiet --ref --user --verbose ;;
			"wallet payout settle") compadd -- --amount --auto-ref --by --destination --dry-run --env-file --format --note --quiet --ref --user --verbose ;;
			"wallet refund") compadd -- --amount --asset --auto-ref --dry-run --env-file --format --memo --merchant --merchant-share --quiet --ref --upstream-cost --user --verbose ;;
			*) compadd -- --env-file --format --quiet --verbose ;;
		esac
		return
		;;
	esac

	case "$path" in
		"") compadd -- doctor settings admin version completion channel model alias price upstream-cost user apikey wallet plan usage ledger migrate partition secret maintenance snapshot consent plugin reconcile rotate idempotency calendar ;;
		"admin") compadd -- token ;;
		"admin token") compadd -- create revoke ;;
		"alias") compadd -- create list update delete resolve ;;
		"apikey") compadd -- create update get list ;;
		"calendar") compadd -- import ;;
		"channel") compadd -- create list get health update enable disable delete restore sync-models upstream-models adapters probe map ;;
		"channel map") compadd -- add list remove ;;
		"consent") compadd -- grant list revoke ;;
		"idempotency") compadd -- cleanup ;;
		"ledger") compadd -- balance verify reconcile runs ;;
		"maintenance") compadd -- idempotency session ;;
		"migrate") compadd -- up down down-to up-to status version create ;;
		"model") compadd -- create list get update delete onboard alias trace ;;
		"model alias") compadd -- add remove list ;;
		"partition") compadd -- check ensure ;;
		"plan") compadd -- create list get enable disable grant void subscriptions key order ;;
		"plan key") compadd -- show set ;;
		"plan order") compadd -- show set ;;
		"plugin") compadd -- list get publish enable disable delete kv ;;
		"plugin kv") compadd -- get set delete ;;
		"price") compadd -- create list show update enable disable explain calendar ;;
		"price calendar") compadd -- import sync list ;;
		"secret") compadd -- rotate ;;
		"settings") compadd -- list get set ;;
		"snapshot") compadd -- cleanup allowlist get access-log redact ;;
		"upstream-cost") compadd -- create list update disable ;;
		"usage") compadd -- events requests summary ;;
		"user") compadd -- create list get inspect update password unlock delete plan wallet key usage ;;
		"user delete") compadd -- request revert purge ;;
		"user key") compadd -- list create plans ;;
		"user key plans") compadd -- show set ;;
		"user plan") compadd -- grant list void order ;;
		"user plan order") compadd -- show set ;;
		"user usage") compadd -- events requests ;;
		"user wallet") compadd -- show credit adjust refund ;;
		"wallet") compadd -- create credit adjust refund payout payment list ;;
		"wallet payment") compadd -- create status ;;
		"wallet payout") compadd -- request settle approve reject paid ;;
	esac
}
if [ "$funcstack[1]" = "_larkaxctl" ]; then
	_larkaxctl "$@"
else
	compdef _larkaxctl larkaxctl
fi
