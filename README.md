# larkax

A model router. One API surface in front of many upstream providers, with a plugin
system and a billing engine that can express real-world pricing.

`larkax` speaks the OpenAI, Anthropic, OpenAI Responses and System One wire
protocols, so existing SDKs work against it once `base_url` points here. Four POST
paths are implemented — `/v1/chat/completions`, `/v1/messages`, `/v1/responses` and
`/v1/systemone`; every other path, `GET /v1/models` included, answers `501` with a
JSON body that says so. Requests are routed to upstream channels selected by
health, latency, cost, and quota; usage is metered and billed per request.

## Quickstart

From a clone to a real completion. It needs an upstream account of your own —
larkax ships no upstream, and `https://api.example.com/v1` below is a placeholder
you replace with yours. Step 6 is a real `200` from a real model; step 7 says which
link is missing when it is not.

Prerequisites: Go 1.26 or newer, Docker with `docker compose` v2.1 or newer,
`make`, and `openssl` (`make` generates the master key with it).

Three ports have to be free, each with a variable that moves it:

| what | default | change it with |
|---|---|---|
| the inference API | `8080` | `LARKAX_HTTP_ADDR` |
| health and metrics | `9090` | `LARKAX_INTERNAL_ADDR` |
| the PostgreSQL container | `5432` | `PG_PORT` |

`make run` pre-checks the first two only, so a busy `5432` gets past it and fails
later, inside `docker compose up`, with a different bind error. The two HTTP
defaults are `127.0.0.1:8080` and `127.0.0.1:9090`; those variables take a full
`host:port`, as in `LARKAX_HTTP_ADDR=127.0.0.1:8081 make run`.

**The command output is Chinese today.** The commands, the exit codes and the
printed values are stable; the prose around them is not translated yet.

```sh
git clone https://github.com/sumwai/larkax
cd larkax

# 1. PostgreSQL, the migrations, and the service, in the foreground.
make run
```

It ends with a line naming the public address:

```text
time=2026-10-18T10:00:00.000+08:00 level=INFO msg=开始监听 server=public addr=127.0.0.1:8080
```

Everything below assumes the default ports, `8080` and `9090`; if either one
changes, the ports in the commands below have to change with it.

Leave `make run` in that shell and use a second one for the rest.

```sh
# 2. Three answers that together mean "up".
curl -i 127.0.0.1:9090/healthz                # 200
curl -s 127.0.0.1:9090/readyz                 # {"checks":{"postgres":"ok","tokens":"ok"},"ready":true}
curl -i 127.0.0.1:8080/v1/chat/completions    # 405 — the inference paths are POST

# 3. The upstream credential, and the model ids your upstream actually serves.
#    The credential is the API key from your account on the vendor's side; it goes
#    to `~/upstream-key.txt`, outside the clone (a credential inside the working
#    tree can be committed by accident), and step 4 reads it from there. Step 4
#    also needs one of the ids below; the vendor's page name is often not it.
printf '%s\n' '<your-upstream-key>' > ~/upstream-key.txt
curl -s https://api.example.com/v1/models \
  -H "Authorization: Bearer $(cat ~/upstream-key.txt)"

# 4. A canonical model, a channel to the upstream, the alias mapping one to the
#    other, and a price. The canonical name is the key routing and billing both use,
#    and it is what the request body sends.
#    `--context-limit` comes from step 3: the upstream model's `context_window`.
go run ./cmd/larkaxctl model create --model claude-x --display 'Example model' \
  --vendor example --context-limit 200000 --tools --vision

# Credentials never go on the command line — they would land in `ps` and in shell
# history — so they come from a file or from the environment.
cat ~/upstream-key.txt | go run ./cmd/larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-stdin

go run ./cmd/larkaxctl alias create --model claude-x --upstream <upstream-model-id>

# A price records who set it, so the user it names has to exist first — the same
# `alice` step 5 gives a wallet to.
go run ./cmd/larkaxctl user create --name alice

# Every usage slot the upstream can report needs a price. Leaving one out is not a
# smaller charge: it fails the whole request. Write all five, 0 where you charge
# nothing.
go run ./cmd/larkaxctl price create --scope platform --name example-price --by alice \
  --expr '{"prompt": 0.0000015, "completion": 0.0000075,
           "cache_read": 0.0000002, "cache_write": 0.0000019,
           "reasoning": 0.0000090}'

# 5. A wallet for the user from step 4, money in it, and an API key. A fresh wallet
#    holds 0, and the reservation step rejects every request until it does not.
go run ./cmd/larkaxctl wallet create --user alice
go run ./cmd/larkaxctl wallet credit --user alice --amount 100 --ref ORD-1
KEY=$(go run ./cmd/larkaxctl apikey create --user alice)
```

`--base-url` is the upstream's base **without** the endpoint path: larkax appends
`/chat/completions` (or `/messages`, for an `anthropic` channel) itself. Either
`https://api.example.com/v1` or `https://api.example.com` works — the version
segment is not repeated when the base already ends with it. What must not be there
is the endpoint: `https://api.example.com/v1/chat/completions` is wrong.

```sh
# 6. The call. `model` is the canonical name from step 4, not the upstream id, and
#    `max_tokens` is required here even though the OpenAI protocol makes it optional
#    — it is the upper bound the reservation is computed from.
curl -sS 127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}'
```

A `200`, carrying the assistant's text and the usage the bill was computed from
(abridged — the real body carries the full OpenAI fields):

```json
{"choices":[{"message":{"role":"assistant","content":"Hello there friend"}}],
 "usage":{"prompt_tokens":37,"completion_tokens":65,"total_tokens":102}
}
```

```sh
# 7. Which of the links above is missing, if any.
go run ./cmd/larkaxctl doctor
#    Six checks, plus one line for the upstream that stays `-` unless you pass
#    --live. The wallet check passes only after step 5 credited it.
```

When the names do not line up, which answer you get depends on which link is
missing, and the answer never says which name was wrong — `larkaxctl doctor` does,
and it names both the channel and the name:

- **A model no channel serves** — check 3, and the request gets `503 no available
  upstream for the requested model`.
- **No alias at all** for the canonical name — check 4 passes: there is nothing to
  translate, so the name goes upstream unchanged. If the upstream does not know it
  either, the request gets `502 upstream call failed`, a message that names the
  upstream although the missing mapping is ours to add.
- **Some aliases, none covering this channel** — check 4 fails and names the
  channel and the name; the request gets `500 internal error`.
- **A missing price rule** — check 5 fails; the request gets `500 internal error`
  too, and which rule was missing goes to our server log, not to the response.

That command also answers "did my money move": the credit and the cost are both
ledger entries, and `larkaxctl ledger balance --account user:1` reads them back.
The `1` is the user id that `user create` printed in step 4 — the first user in a
fresh database.

The same chain at length, and everything else under `docs/`, is Chinese. Four of
those documents are written for callers rather than contributors — `limits.md`,
`max-tokens.md`, `idempotency.md` and `versioning.md` — and there is no English
version of them yet. [docs/development.md](docs/development.md) is the entry point
for contributors; [docs/README.md](docs/README.md) indexes the whole directory.

## Status

Early development, but the request path is in place: `cmd/larkax` assembles the
gateway through `internal/assembly` and serves the inference API on the public
port, speaking four protocols — OpenAI, Anthropic, OpenAI Responses and System One
— with streaming on the first three. A request is authenticated, rate limited,
routed, reserved, forwarded upstream, settled, and written to the ledger.

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
- The decision surface (`POST /v1/systemone`): typed judgement questions instead of a
  conversation, never streamed, billed through the same pipeline
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

See [docs/development.md](docs/development.md) for how to build and run it, and
[docs/README.md](docs/README.md) for every other document — that index is organised
by what you are trying to do rather than by module. The HTTP surface — paths,
response headers, status codes, error bodies, streaming event sequences — is in
[docs/api.md](docs/api.md). The `larkaxctl` command reference is in
[docs/cli-reference.md](docs/cli-reference.md); the metrics the service exports,
and how to alert on each, in [docs/metrics.md](docs/metrics.md).

## What it will do

- **Four paths on one host.** `/v1/chat/completions`, `/v1/messages`, `/v1/responses`
  and `/v1/systemone`. The official OpenAI, Anthropic and OpenAI Responses SDKs all
  work by pointing `base_url` at larkax. System One is larkax's own shape — typed
  judgement questions answered with probabilities rather than a conversation — and
  is documented in [docs/development.md](docs/development.md).
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
internal/auth/                    turns a request's credential into the user it belongs to
internal/billing/                 turning usage into money: reserve, settle, breakdown
internal/billing/calendaradapter/  bridges internal/calendar to the pricing engine
internal/billing/expr/            the expression engine that evaluates pricing rules
internal/calendar/                workday / holiday / peak-hour judgement per region
internal/calendar/store/          the calendar tables, and importing a year of data
internal/catalog/                 the model directory: canonical names and upstream aliases
internal/channel/                 adapter interface, registry, unified request/response types
internal/channel/anthropicadapter/  Anthropic Messages upstream
internal/channel/httpx/           HTTP details shared by adapters
internal/channel/openaiadapter/   OpenAI Chat Completions upstream
internal/channel/responsesadapter/  OpenAI Responses upstream
internal/channels/                candidates out of the database, call outcomes back in
internal/config/                  startup configuration (environment variables)
internal/credential/              envelope encryption for channel credentials
internal/db/                      database connection pool
internal/dberr/                  Postgres constraint violations as one shared sentinel
internal/gateway/                 the order of a request: reserve, route, call, settle
internal/idempotency/             the Idempotency-Key header: run a repeated request once
internal/keyquota/                whether a key's cumulative spend has passed its limit
internal/ledger/                  double-entry bookkeeping: construct and verify entries
internal/ledger/store/            persisting ledger entries, wallets and reservations
internal/manpage/                 renders the man pages from the CLI registry and docs/cli-reference.md
internal/migrate/                 embedded migrations (internal/migrate/sql/)
internal/money/                   fixed-point money; the only place rounding happens
internal/observe/                 logging, request IDs, health and readiness endpoints
internal/overrides/               the upstream parameter overrides configured on an alias
internal/pipeline/                wires the billing stages into one request flow
internal/plugin/                  the five hooks: registry, dispatch, failure policy
internal/plugin/host/             host capabilities injected into plugins (log, kv, calendar)
internal/plugin/sandbox/          moejs runtime, limits, interruption
internal/pricing/                 picking the pricing rule that applies to a request
internal/quota/                   upstream quota state: observations, self-accounting, confidence
internal/ratelimit/               per-key requests per minute, counted in Redis when configured
internal/relay/                   protocol conversion through relaykit
internal/route/                   channel selection and failover
internal/settings/                runtime configuration, stored in the database
internal/snapshot/                structure-only capture of requests, with redaction
internal/snapshotwire/            reads snapshot settings, writes skeletons, drops expired partitions
internal/systemone/               the decision surface's own request and response shape
internal/testdb/                  test-only: one database per test package
internal/tokens/                  token counting and stream-cancel estimation
internal/upstream/                assembling the protocol adapters, dispatch by channel type
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

Requires Go 1.26 or newer. An older Go on the machine still builds: inside the
module, `go` downloads the toolchain `go.mod` asks for. `go version` outside the
module prints the local one, and `go env GOTOOLCHAIN` says whether that download is
allowed — check both before installing a Go by hand.

```sh
go build ./...

# the version comes from the git tag, injected at build time
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/larkax
```

**`go test ./...` alone silently skips the integration tests** when `LARKAX_DB_DSN`
is unset: the packages that need a database still print `ok`, so a green run there
means the skipped packages were not tested at all. `make test` brings the database
up and loads `.env` first:

```sh
make test
```

The rate-limit integration tests need Redis as well, on top of the database —
`make test` does not provide it. See
[docs/development.md](docs/development.md) for that and for the rest of the gates.

## Running it locally

Requires Go 1.26 or newer, plus Docker with `docker compose` v2.1 or newer, `make`,
and `openssl` (`make` uses it to generate the master key). One command brings up
PostgreSQL, applies the migrations, and runs the service in the foreground — the
[Quickstart](#quickstart) above is this command followed by the rest of the chain:

```sh
make run
```

On the first run it writes `.env` with a freshly generated master key. An existing
`.env` is never replaced — that key decrypts the stored credentials, so replacing it
would destroy them — but the `LARKAX_DB_DSN` line is kept in sync with
`PG_PORT` / `PG_DB` / `DB_PASSWORD` on every run. `make up`, `make down`, `make migrate`,
`make test` and `make clean` each do one step — `make migrate` and `make test` make sure
`.env` exists first, since both need the DSN — and `make clean` returns the working copy
to its freshly cloned state.

If 5432 is already taken, `make run PG_PORT=55432` — the DSN line follows, so there is
no need to delete `.env` first.

The service and `larkaxctl` read `.env` themselves — variables already set in the
environment always win, so there is no need to export it by hand before
`go run ./cmd/larkaxctl ...`; `--env-file <path>` selects a different file. The lookup
rules and the rest of the global conventions are in
[docs/cli-reference.md](docs/cli-reference.md).

The service serves the inference API on `:8080`. The internal listener on `:9090`
carries `/healthz` and `/readyz`, the Prometheus `/metrics` endpoint, and the
read-only `/ratelimit/usage?key_id=<id>`. Both listen addresses default to loopback
(`127.0.0.1:8080` and `127.0.0.1:9090`), so nothing outside the host can reach them.
Set `LARKAX_HTTP_ADDR` / `LARKAX_INTERNAL_ADDR` explicitly to widen the bind — for
example `:8080` in a container.
A deployment running the `worker` role does not serve inference traffic and answers `501`
on the public port by design — the response names the role, so a misconfigured deployment
is obvious rather than mysterious. Building alone is not enough to run it — see
[docs/development.md](docs/development.md) for the full picture.

A healthy service is not the same as a successful inference call. The call also
needs an upstream account, the model / channel / alias / price chain, a user with
a wallet, and an API key — and money in that wallet. The [Quickstart](#quickstart)
above is that chain; [docs/development.md](docs/development.md) walks through it at
length, and through everything around it — checks, gates, retention, partitions.

## License

AGPL-3.0. See [LICENSE](LICENSE).

This project depends on [new-api](https://github.com/QuantumNous/new-api), which is
also AGPL-3.0 and carries additional terms under Section 7. Those terms are retained
in [NOTICE](NOTICE), along with attribution for other dependencies.

## Contributing

See [AGENTS.md](AGENTS.md) for this repository's conventions and rules, and
[`.claude/skills/larkax-workflow/SKILL.md`](.claude/skills/larkax-workflow/SKILL.md)
for the development workflow — roles, how work is handed off, and the
issue → PR → merge path. Before starting work, read the [LESSONS.md](LESSONS.md)
index and open the entries it points at in [`lessons/`](lessons/) — they record
problems already encountered and how they were resolved.
