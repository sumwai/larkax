# larkax

A model router. One API surface in front of many upstream providers, with a plugin
system and a billing engine that can express real-world pricing.

`larkax` speaks both the OpenAI and Anthropic wire protocols, so existing SDKs work
against it unchanged. Requests are routed to upstream channels selected by health,
latency, cost, and quota; usage is metered and billed per request.

## Status

Early development — the repository currently contains the service skeleton and the
database schema, not a working router. The inference entry point exists in
`internal/api`, but it is not wired into the binary yet, so every path still returns
`501`. Nothing here is ready to run in production.

What works today:

- Configuration loading, structured logging, `/healthz` and `/readyz`
- Runtime settings loaded from the database and validated against the keys the code declares
- Migrations, applied and rolled back through `larkaxctl`
- The full initial database schema
- The ledger store and the reconcile job that checks the wallet snapshot against it
- The public HTTP entry: path-to-protocol dispatch, request parsing, and per-protocol
  response and error shapes (not yet wired into `cmd/larkax`)

See [docs/development.md](docs/development.md) for how to build and run it.
The reconciliation job is documented in [docs/reconciliation.md](docs/reconciliation.md).

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
internal/api/                     the public HTTP entry: paths, parsing, response shapes
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
internal/gateway/                 the order of a request: reserve, route, call, settle
internal/ledger/                  double-entry bookkeeping: construct and verify entries
internal/ledger/store/            persisting ledger entries, wallets and reservations
internal/migrate/                 embedded migrations (internal/migrate/sql/)
internal/money/                   fixed-point money; the only place rounding happens
internal/observe/                 logging, request IDs, health and readiness endpoints
internal/pipeline/                wires the billing stages into one request flow
internal/plugin/                  the five hooks: registry, dispatch, failure policy
internal/plugin/host/             host capabilities injected into plugins (log, kv, calendar)
internal/plugin/sandbox/          moejs runtime, limits, interruption
internal/quota/                   upstream quota tracking: probe, self-account, merge
internal/relay/                   protocol conversion through relaykit
internal/route/                   channel selection and failover
internal/settings/                runtime configuration, stored in the database
internal/snapshot/                structure-only capture of requests, with redaction
internal/snapshotwire/            reads snapshot settings, writes skeletons, drops expired partitions
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
                   ├── ledger entries handed back to the caller
                   └── usage normalized from channel.Usage, tokens only as a label
```

`gateway` is the entry layer, and `internal/api` is its HTTP half; `cmd/` sits
above them and `money`, `calendar`, `relay`, `observe` and `migrate` are leaves.
Nothing else reaches back into `gateway` — the two exceptions are named below.

A few rules hold across the tree, and `scripts/check-layering.sh` checks them in
CI rather than by eye — it lists the packages allowed to import relaykit and
flags `float64(` used on money outside `internal/money`:

- **Only three packages import
  [relaykit](https://github.com/QuantumNous/new-api):** `internal/relay`, which is
  the seam itself, and the two protocol adapters, which need the DTO type to talk
  to it. Everything else uses larkax's own types.
- **`internal/money` is the only place rounding happens.** Everything else is
  integer arithmetic on a fixed-point value.
- **Nothing below the entry layer imports `internal/gateway`.** The two
  exceptions are named in `scripts/check-layering.sh`: `internal/api`, which is
  the HTTP half of the entry layer and calls `gateway.Chat`, and
  `internal/snapshotwire`, which adapts the snapshot package to the interfaces
  the gateway declares.

The billing path is deliberately split so each stage can be tested without a
database or a network: `money` (arithmetic) → `billing` (usage × price) →
`billing/expr` (rules) → `pipeline` (the order of operations).

`internal/gateway` holds the order of a whole request — reserve before calling
upstream, do not switch channels once a stream has emitted, never let the
snapshot path slow down the request. It is not wired into the binary yet, and
neither is `internal/plugin`. Still absent: `plugins/` and `web/`.

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

## Running it locally

Requires Go 1.26 or newer, plus Docker with `docker compose` v2.1 or newer and `make`.
One command brings up PostgreSQL, applies the migrations, and runs the service in the
foreground:

```sh
make run
```

On the first run it writes `.env` with a freshly generated master key, and it never
overwrites an existing one — that key decrypts the stored credentials, so replacing it
would destroy them. `make up`, `make down`, `make migrate`, `make test` and `make clean`
each do one step; `make clean` returns the working copy to its freshly cloned state.

If 5432 is already taken, `make run PG_PORT=55432`. The port and database name are
baked into `.env` when it is created, so set them before the first run or after
`make clean`.

The service listens on `:8080`, where every path returns `501` until the inference API
exists, and serves `/healthz` and `/readyz` on `:9090`. Building alone is not enough to
run it — see [docs/development.md](docs/development.md) for the full picture.

## License

AGPL-3.0. See [LICENSE](LICENSE).

This project depends on [new-api](https://github.com/QuantumNous/new-api), which is
also AGPL-3.0 and carries additional terms under Section 7. Those terms are retained
in [NOTICE](NOTICE), along with attribution for other dependencies.

## Contributing

See [AGENTS.md](AGENTS.md) for development conventions, the workflow, and the rules
this repository follows. Read [LESSONS.md](LESSONS.md) before starting work — it
records problems already encountered and how they were resolved.
