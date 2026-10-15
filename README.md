# larkax

A model router. One API surface in front of many upstream providers, with a plugin
system and a billing engine that can express real-world pricing.

`larkax` speaks both the OpenAI and Anthropic wire protocols, so existing SDKs work
against it unchanged. Requests are routed to upstream channels selected by health,
latency, cost, and quota; usage is metered and billed per request.

## Status

Early development. The public API is not yet live and nothing here is ready to run
in production.

## What it does

- **Dual protocol.** `/v1/chat/completions` and `/v1/messages` on the same host. The
  official OpenAI and Anthropic SDKs both work by pointing `base_url` at larkax.
- **Channel routing.** Multiple upstream accounts per model. Failed upstreams are
  retried on another channel and are not billed.
- **Real billing.** Peak/off-peak rates, context-length tiers, cache pricing, and
  subscription plans, expressed as versioned rules rather than hardcoded branches.
- **Quota monitoring.** Upstream accounts are probed and tracked so routing can
  prefer the channel that expires soonest or has the most balance left.
- **Plugin system.** Sandboxed JavaScript hooks around the request lifecycle. Plugins
  cannot read channel credentials and cannot make network requests.
- **Structure-only request capture.** Enough of each request's shape is kept to
  reproduce an error; prompt and response content is not.

## Repository layout

```
cmd/larkax/         the service binary
cmd/larkaxctl/      migrations and operations CLI
internal/gateway/   ingress: protocols, auth, rate limits, request IDs
internal/route/     channel selection and failover
internal/billing/   pricing rules, calendar, tokenizers
internal/quota/     upstream quota probing and scheduling
internal/ledger/    double-entry ledger, wallets, payouts
internal/plugin/    plugin host and contract
migrations/         database migrations
web/                console frontend
```

## Building

Requires Go 1.26 or newer.

```sh
go build ./...
go test ./...
```

## License

AGPL-3.0. See [LICENSE](LICENSE).

This project depends on [new-api](https://github.com/QuantumNous/new-api), which is
also AGPL-3.0 and carries additional terms under Section 7. Those terms are retained
in [NOTICE](NOTICE), along with attribution for other dependencies.

## Contributing

See [AGENTS.md](AGENTS.md) for development conventions, the workflow, and the rules
this repository follows. Read [LESSONS.md](LESSONS.md) before starting work — it
records problems already encountered and how they were resolved.
