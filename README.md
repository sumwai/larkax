# larkax

A model router. One API surface in front of many upstream providers, with a plugin
system and a billing engine that can express real-world pricing.

`larkax` speaks both the OpenAI and Anthropic wire protocols, so existing SDKs work
against it unchanged. Requests are routed to upstream channels selected by health,
latency, cost, and quota; usage is metered and billed per request.

## Status

Early development — the repository currently contains the service skeleton and the
database schema, not a working router. The inference API is not implemented and
returns `501` on every path. Nothing here is ready to run in production.

What works today:

- Configuration loading, structured logging, `/healthz` and `/readyz`
- Migrations, applied and rolled back through `larkaxctl`
- The full initial database schema

See [docs/development.md](docs/development.md) for how to build and run it.

## What it will do

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
  reproduce an error; prompt and response content is not. See
  [docs/request-retention.md](docs/request-retention.md).

## Repository layout

```
cmd/larkax/            the service binary
cmd/larkaxctl/         migrations and operations CLI
internal/config/       startup configuration
internal/migrate/      embedded migrations (internal/migrate/sql/)
internal/observe/      logging, health and readiness endpoints
scripts/               repository tooling
```

Later milestones add `internal/gateway`, `internal/route`, `internal/billing`,
`internal/quota`, `internal/ledger`, `internal/plugin`, `plugins/`, and `web/`.

## Building

Requires Go 1.26 or newer.

```sh
go build ./...
go test ./...
```

Building is not enough to run the service — it also needs a PostgreSQL database and
several environment variables. Follow [docs/development.md](docs/development.md).

## License

AGPL-3.0. See [LICENSE](LICENSE).

This project depends on [new-api](https://github.com/QuantumNous/new-api), which is
also AGPL-3.0 and carries additional terms under Section 7. Those terms are retained
in [NOTICE](NOTICE), along with attribution for other dependencies.

## Contributing

See [AGENTS.md](AGENTS.md) for development conventions, the workflow, and the rules
this repository follows. Read [LESSONS.md](LESSONS.md) before starting work — it
records problems already encountered and how they were resolved.
