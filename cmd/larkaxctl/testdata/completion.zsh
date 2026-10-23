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
		"ledger balance|--asset") compadd -- CNY; return ;;
		"plan create|--asset") compadd -- CNY; return ;;
		"plan void|--status") compadd -- expired canceled; return ;;
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
			"price create") compadd -- --by --channel --disabled --effective-from --effective-to --env-file --expr --format --models --name --priority --quiet --schedule-file --scope --user --verbose ;;
			"price explain") compadd -- --at --channel --env-file --format --model --quiet --user --verbose ;;
			"price update") compadd -- --by --channel --clear-effective-to --clear-schedule --disable --effective-from --effective-to --enable --env-file --expr --format --id --models --name --priority --quiet --schedule-file --scope --user --verbose ;;
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
			"usage requests") compadd -- --env-file --format --from --limit --quiet --to --user --verbose ;;
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
			"user usage requests") compadd -- --env-file --format --from --limit --quiet --to --user --verbose ;;
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
		"model") compadd -- create list get update delete alias trace ;;
		"partition") compadd -- check ensure ;;
		"plan") compadd -- create list get enable disable grant void subscriptions key order ;;
		"plan key") compadd -- show set ;;
		"plan order") compadd -- show set ;;
		"plugin") compadd -- list get publish enable disable delete kv ;;
		"plugin kv") compadd -- get set delete ;;
		"price") compadd -- create list show update enable disable explain calendar ;;
		"price calendar") compadd -- import list ;;
		"secret") compadd -- rotate ;;
		"settings") compadd -- list get set ;;
		"snapshot") compadd -- cleanup allowlist get access-log redact ;;
		"upstream-cost") compadd -- create list update disable ;;
		"usage") compadd -- events requests summary ;;
		"user") compadd -- create list get update password unlock delete plan wallet key usage ;;
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
