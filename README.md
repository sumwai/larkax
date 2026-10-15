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
- Runtime settings loaded from the database and validated against the keys the code declares
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
cmd/larkax/                       the service binary
cmd/larkaxctl/                    migrations and operations CLI
internal/billing/                 turning usage into money: reserve, settle, breakdown
internal/billing/calendaradapter/  bridges internal/calendar to the pricing engine
internal/billing/expr/            the expression engine that evaluates pricing rules
internal/calendar/                workday / holiday / peak-hour judgement per region
internal/channel/                 adapter interface, registry, unified request/response types
internal/channel/anthropicadapter/  Anthropic Messages upstream
internal/channel/httpx/           HTTP details shared by adapters
internal/channel/openaiadapter/   OpenAI Chat Completions upstream
internal/config/                  startup configuration (environment variables)
internal/credential/              envelope encryption for channel credentials
internal/db/                      database connection pool
internal/ledger/                  double-entry bookkeeping: construct and verify entries
internal/migrate/                 embedded migrations (internal/migrate/sql/)
internal/money/                   fixed-point money; the only place rounding happens
internal/observe/                 logging, request IDs, health and readiness endpoints
internal/pipeline/                wires the billing stages into one request flow
internal/quota/                   upstream quota tracking: probe, self-account, merge
internal/relay/                   the only place that imports relaykit; protocol conversion
internal/route/                   channel selection and failover
internal/settings/                runtime configuration, stored in the database
internal/tokens/                  token counting and stream-cancel estimation
scripts/                          repository tooling
```

Layering, so the data plane never depends on the control plane:

```
gateway  ──►  route  ──►  channel.Adapter  ──►  upstream
                │                │
                │                └── protocol conversion only
                └── selection, retry, failover

gateway  ──►  pipeline  ──►  billing  ──►  money
                   │             │
                   │             └── rules evaluated by billing/expr
                   └── ledger entries handed back to the caller
```

Two rules hold across the tree, and both are checked by eye because they cannot
be checked by the compiler:

- **`internal/relay` is the only package that imports
  [relaykit](https://github.com/QuantumNous/new-api).** Others use its own types.
- **`internal/money` is the only place rounding happens.** Everything else is
  integer arithmetic on a fixed-point value.

The billing path is deliberately split so each stage can be tested without a
database or a network: `money` (arithmetic) → `billing` (usage × price) →
`billing/expr` (rules) → `pipeline` (the order of operations).

Not yet written: `internal/gateway`, `internal/plugin`, `plugins/`, and `web/`.

## Versioning

Two separate things, deliberately:

- the **release version** (`v0.1.0`), which changes on every release
- the **API version** (`/v1` in the path), which changes only on a breaking change

Shipping `v1.4.2` does not move the API; clients keep calling `/v1`.
See [docs/versioning.md](docs/versioning.md).

## Building

Requires Go 1.26 or newer.

```sh
go build ./...
go test ./...

# the version comes from the git tag, injected at build time
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/larkax
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
