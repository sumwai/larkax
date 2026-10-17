# larkax

A model router. One API surface in front of many upstream providers, with a plugin
system and a billing engine that can express real-world pricing.

`larkax` speaks both the OpenAI and Anthropic wire protocols, so existing SDKs work
against it unchanged. Requests are routed to upstream channels selected by health,
latency, cost, and quota; usage is metered and billed per request.

## Status

Early development, but the request path is in place: `cmd/larkax` assembles the
gateway through `internal/assembly` and serves the inference API on the public
port, speaking both the OpenAI and the Anthropic protocol, streaming included.
A request is authenticated, rate limited, routed, reserved, forwarded upstream,
settled, and written to the ledger.

Nothing here is ready to run in production.

What works today:

- Configuration loading, structured logging, `/healthz` and `/readyz`
- Prometheus metrics on the internal listener (`/metrics`), see [docs/metrics.md](docs/metrics.md)
- Runtime settings loaded from the database and validated against the keys the code declares
- Migrations, applied and rolled back through `larkaxctl`
- The full initial database schema
- The public HTTP entry: path-to-protocol dispatch, request parsing, and per-protocol
  response and error shapes
- The request path end to end, including streaming responses
- Request-level idempotency through the `Idempotency-Key` header, see
  [docs/idempotency.md](docs/idempotency.md)
- The billing engine: the expression-based pricing rules, time-of-day and tiered
  pricing, reservation and settlement
- `larkaxctl` management commands for API keys (including per-key rate limits and
  per-key spending limits), users, runtime settings, snapshot and idempotency
  retention, master key rotation, calendar data, and channels, models, aliases and
  pricing rules
- The reconcile job that checks the wallet snapshot against the ledger
- The plugin system: JavaScript plugins loaded from the `plugins` table at startup,
  with all five hooks (`onRequest` / `onRoute` / `onStream` / `onResponse` /
  `onUsage`) running on the request path, see
  [docs/plugin-contract.md](docs/plugin-contract.md)

Not wired yet:

- Payments and payouts — the ledger transactions exist, the entry points do not
- The web console (`web/`)
- Most of the metrics worth having — `/metrics` is served, but only the
  settlement counters are wired to real increment points, see
  [docs/metrics.md](docs/metrics.md)

See [docs/development.md](docs/development.md) for how to build and run it.
The reconciliation job is documented in [docs/reconciliation.md](docs/reconciliation.md),
the calendar data behind peak/off-peak pricing in [docs/calendar-data.md](docs/calendar-data.md),
the health and quota signals in [docs/channel-health.md](docs/channel-health.md),
the per-key limits in [docs/limits.md](docs/limits.md),
the table partitions and the job that pre-creates them in
[docs/partitions.md](docs/partitions.md),
and the plugin contract in [docs/plugin-contract.md](docs/plugin-contract.md).

## What it will do

- **Dual protocol.** `/v1/chat/completions` and `/v1/messages` on the same host. The
  official OpenAI and Anthropic SDKs both work by pointing `base_url` at larkax.
- **Channel routing.** Multiple upstream accounts per model. Failed upstreams are
  retried on another channel and are not billed.
- **Real billing.** Peak/off-peak rates, context-length tiers, cache pricing, and
  subscription plans, expressed as versioned rules rather than hardcoded branches.
- **Channel health and quota.** Every upstream call writes back its outcome, so
  routing can prefer the channel that expires soonest or has the most balance left.
  Two of the three quota sources — querying an upstream balance endpoint and
  inferring from upstream errors — are not wired yet; see
  [docs/channel-health.md](docs/channel-health.md).
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
internal/assembly/                wires the entry layer into a runnable service
internal/billing/                 turning usage into money: reserve, settle, breakdown
internal/billing/calendaradapter/  bridges internal/calendar to the pricing engine
internal/billing/expr/            the expression engine that evaluates pricing rules
internal/calendar/                workday / holiday / peak-hour judgement per region
internal/calendar/store/          the calendar tables, and importing a year of data
internal/channel/                 adapter interface, registry, unified request/response types
internal/channel/anthropicadapter/  Anthropic Messages upstream
internal/channel/httpx/           HTTP details shared by adapters
internal/channel/openaiadapter/   OpenAI Chat Completions upstream
internal/channels/                candidates out of the database, call outcomes back in
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
internal/pricing/                 picking the pricing rule that applies to a request
internal/quota/                   upstream quota state: observations, self-accounting, confidence
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
                   │             └── rules come from billing/expr, which builds billing.Prices
                   ├── ledger entries handed back to the caller
                   └── usage normalized from channel.Usage, tokens only as a label
```

`gateway` is the entry layer, and `internal/api` is its HTTP half; `cmd/` sits
above them and `money`, `calendar`, `observe` and `migrate` are leaves. `relay`
is not one — it depends on `internal/channel`'s types. Nothing else reaches back
into `gateway` — the two exceptions are named below.

A few rules hold across the tree, and `scripts/check-layering.sh` checks them in
CI rather than by eye — it lists the packages allowed to import relaykit and
flags `float64(` used on money outside `internal/money` (a hint for a human to
read, not a failure):

- **Only three packages import
  [relaykit](https://github.com/QuantumNous/new-api):** `internal/relay`, which is
  the seam itself, and the two protocol adapters, which need the DTO type to talk
  to it. Everything else uses larkax's own types.
- **`internal/money` is the only place rounding happens.** Everything else is
  integer arithmetic on a fixed-point value.
- **Nothing below the entry layer imports `internal/gateway`.** The three
  exceptions are named in `scripts/check-layering.sh`: `internal/api`, which is
  the HTTP half of the entry layer and calls `gateway.Chat`; `internal/snapshotwire`,
  which adapts the snapshot package to the interfaces the gateway declares; and
  `internal/assembly`, which constructs the gateway and is the only place that
  sees both the entry layer and the data plane.

The billing path is deliberately split so each stage can be tested without a
database or a network: `money` (arithmetic) → `billing` (usage × price) →
`billing/expr` (rules) → `pipeline` (the order of operations).

`internal/gateway` holds the order of a whole request — reserve before calling
upstream, do not switch channels once a stream has emitted, never let the
snapshot path slow down the request. `internal/assembly` builds it into the
service binary, including the plugin registry it loads from the database. Still
absent: `plugins/` (bundled plugin sources) and `web/`.

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

On the first run it writes `.env` with a freshly generated master key. An existing
`.env` is never replaced — that key decrypts the stored credentials, so replacing it
would destroy them — but the `LARKAX_DB_DSN` line is kept in sync with
`PG_PORT` / `PG_DB` / `DB_PASSWORD` on every run. `make up`, `make down`, `make migrate`,
`make test` and `make clean` each do one step; `make clean` returns the working copy to
its freshly cloned state.

If 5432 is already taken, `make run PG_PORT=55432` — the DSN line follows, so there is
no need to delete `.env` first.

The service serves the inference API on `:8080` and `/healthz` and `/readyz` on `:9090`.
Both listen addresses default to loopback (`127.0.0.1:8080` and `127.0.0.1:9090`), so
nothing outside the host can reach them. Set `LARKAX_HTTP_ADDR` / `LARKAX_INTERNAL_ADDR`
explicitly to widen the bind — for example `:8080` in a container.
A deployment running the `worker` role does not serve inference traffic and answers `501`
on the public port by design — the response names the role, so a misconfigured deployment
is obvious rather than mysterious. Building alone is not enough to run it — see
[docs/development.md](docs/development.md) for the full picture.

## License

AGPL-3.0. See [LICENSE](LICENSE).

This project depends on [new-api](https://github.com/QuantumNous/new-api), which is
also AGPL-3.0 and carries additional terms under Section 7. Those terms are retained
in [NOTICE](NOTICE), along with attribution for other dependencies.

## Contributing

See [AGENTS.md](AGENTS.md) for development conventions, the workflow, and the rules
this repository follows. Before starting work, read the [LESSONS.md](LESSONS.md)
index and open the entries it points at in [`lessons/`](lessons/) — they record
problems already encountered and how they were resolved.
