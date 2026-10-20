# larkax

**A model router: one API surface in front of many upstream providers, with a plugin
system and a billing engine that can express real-world pricing.**

larkax speaks the OpenAI, Anthropic, OpenAI Responses and System One wire protocols, so
existing SDKs work against it once `base_url` points here. Six paths are implemented —
`POST /v1/chat/completions`, `/v1/messages`, `/v1/responses` and `/v1/systemone`, plus
`GET /v1/models` and `GET /v1/models/{id}`, which list the models the calling key can
use. Every other path answers `501` with a JSON body that says so. Requests are routed by
health, latency, cost and quota; usage is metered and billed per request. Early
development: the request path is in place, but nothing here is ready for production.

## Quickstart

From a clone to a real completion. It needs an upstream account of your own: larkax
ships none, and `https://api.example.com/v1` below is a placeholder you replace.

Prerequisites: Go 1.26 or newer, Docker with `docker compose` v2.1 or newer, `make`, and
`openssl`. Ports `8080`, `9090` and `5432` have to be free; `LARKAX_HTTP_ADDR`,
`LARKAX_INTERNAL_ADDR` and `PG_PORT` move them.

**The command output is Chinese today.** The commands, exit codes and printed values are
stable; the prose around them is not translated yet.

```sh
git clone https://github.com/sumwai/larkax
cd larkax
make run      # PostgreSQL, the migrations, and the service, in the foreground

# Leave it running, and use a second shell for the rest.
# The upstream credential (kept outside the clone, where it cannot be committed
# by accident), and the model ids your upstream actually serves.
printf '%s\n' '<your-upstream-key>' > ~/upstream-key.txt
curl -s https://api.example.com/v1/models \
  -H "Authorization: Bearer $(cat ~/upstream-key.txt)"

# A canonical model, a channel, the alias mapping one to the other, a user, and a price.
go run ./cmd/larkaxctl model create --model claude-x --display 'Example model' \
  --vendor example --context-limit 200000 --tools --vision
cat ~/upstream-key.txt | go run ./cmd/larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-stdin
go run ./cmd/larkaxctl alias create --model claude-x --upstream <upstream-model-id>
go run ./cmd/larkaxctl user create --name alice
go run ./cmd/larkaxctl price create --scope platform --name example-price --by alice \
  --expr '{"prompt": 0.0000015, "completion": 0.0000075,
           "cache_read": 0.0000002, "cache_write": 0.0000019,
           "reasoning": 0.0000090}'

# A wallet with money in it, and an API key.
go run ./cmd/larkaxctl wallet create --user alice
go run ./cmd/larkaxctl wallet credit --user alice --amount 100 --ref ORD-1
KEY=$(go run ./cmd/larkaxctl apikey create --user alice)

# The call. `model` is the canonical name, not the upstream id.
curl -sS 127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}'
```

A `200` comes back with the assistant's text and the usage the bill was computed from. Four tips:

- **Every usage slot needs a price** — missing one fails the request rather than charging less.
- **Credentials never go on the command line** — `ps` and shell history would keep them.
- **`--base-url` stops before the endpoint** — larkax appends `/chat/completions`, or
  `/messages` for an `anthropic` channel.
- **A fresh wallet holds 0** — the reservation step rejects requests until it does not.

When the names do not line up, `larkaxctl doctor` names the channel and the missing
name — the request's own answer never does. `larkaxctl ledger balance --account user:1`
answers "did my money move" from the ledger.

The same walkthrough, at length, is in [docs/development.md](docs/development.md) — in
Chinese, as is everything under `docs/`.

## Status

The request path works end to end: authenticated, rate limited, routed, reserved,
forwarded, settled and written to the ledger. What works today:

- All four protocols, streaming on the first three; the decision surface takes typed
  judgement questions instead of a conversation, never streams, bills through the pipeline
- Configuration from the environment or the database, structured logging, health,
  readiness and Prometheus metrics; migrations through `larkaxctl`
- The billing engine: expression-based pricing, time-of-day and tiered rates,
  reservation and settlement, with double-entry bookkeeping as the record
- `larkaxctl`: API keys (including per-key rate and spending limits), users, settings,
  retention, key rotation, channels, models, aliases and prices
- Money movement through `larkaxctl`: manual credit and adjustment, refunds, merchant
  payouts (request → approve → paid), and payment orders credited when the channel
  confirms them. Only a fake channel ships today, off by default (`payment.fake_enabled`).
- Plugins (JavaScript, loaded from the plugins table, all five hooks on the request
  path), structure-only request capture, and request-level idempotency through the
  `Idempotency-Key` header
- The browser console in [`web/`](web/), against an in-memory mock by default and the
  account API with `VITE_API_MODE=http`

Not wired yet: a real payment channel — the callback path and the ledger entries are in
place, but the only channel implementation is the fake one.

## What it will do

- **Four paths on one host.** `/v1/chat/completions`, `/v1/messages`, `/v1/responses`
  and `/v1/systemone`. The official OpenAI, Anthropic and OpenAI Responses SDKs work by
  pointing `base_url` at larkax. System One is larkax's own shape — typed judgement
  questions answered with probabilities rather than a conversation.
- **Channel routing.** Multiple upstream accounts per model. Failed upstreams are
  retried on another channel and are not billed.
- **Real billing.** Peak/off-peak rates, context-length tiers, cache pricing, and
  subscription plans, expressed as versioned rules rather than hardcoded branches.
- **Channel health and quota.** Every upstream call writes back its outcome, so routing
  can prefer the channel that expires soonest or has the most balance left. Two of the
  three quota sources — the upstream balance endpoint and upstream errors — are not
  wired yet.
- **Plugin system.** Sandboxed JavaScript hooks around the request lifecycle. Plugins
  cannot read channel credentials and cannot make network requests.
- **Structure-only request capture.** Enough of each request's shape is kept to
  reproduce an error; prompt and response content is not.

## Documentation

Everything under `docs/` is Chinese; [docs/README.md](docs/README.md) indexes it.

| Document | What is in it |
|---|---|
| [docs/development.md](docs/development.md) | How to build it, run it, and get a request through; the checks and gates |
| [docs/api.md](docs/api.md) | The HTTP surface: paths, headers, status codes, error bodies, streaming events |
| [docs/cli-reference.md](docs/cli-reference.md) | Every `larkaxctl` command |
| [docs/metrics.md](docs/metrics.md) | The metrics the service exports, and how to alert on each |
| [docs/plugin-contract.md](docs/plugin-contract.md) | What a plugin may do, and the five hooks |
| [docs/versioning.md](docs/versioning.md) | Release version vs API version — a new release never moves `/v1` |
| [LESSONS.md](LESSONS.md) | Problems already encountered here, and how they were resolved |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The repository's conventions and red lines are
in [AGENTS.md](AGENTS.md).

## Acknowledgements

larkax builds on other people's work:

- [new-api](https://github.com/QuantumNous/new-api) — `relaykit` provides the protocol
  DTOs and the OpenAI ↔ Anthropic conversion; AGPL-3.0 with terms retained in [NOTICE](NOTICE).
- [moejs](https://github.com/Calcium-Ion/moejs) — the JavaScript runtime the plugin
  sandbox runs on.
- [expr-lang/expr](https://github.com/expr-lang/expr) — the expression engine for
  pricing rules.
- [holiday-cn](https://github.com/NateScarlet/holiday-cn) — mainland China holiday
  calendar data (planned).
- [models.dev](https://github.com/sst/models.dev) — model metadata (planned).

## Building and running

Requires Go 1.26 or newer (inside the module, `go` downloads the toolchain `go.mod` asks
for), plus Docker with `docker compose` v2.1 or newer. `go build ./...` builds every
package, and `make run` brings up PostgreSQL, the migrations and the service. `go test
./...` on its own silently skips the integration tests when `LARKAX_DB_DSN` is unset —
those packages still print `ok`, so a green run means they were not tested at all.
Everything else — the `make` targets, `.env` and the master key, the listen addresses,
the gates — is in [docs/development.md](docs/development.md).

## License

AGPL-3.0. See [LICENSE](LICENSE). This project depends on
[new-api](https://github.com/QuantumNous/new-api), also AGPL-3.0, with additional terms
under Section 7 retained in [NOTICE](NOTICE) along with attribution for other
dependencies.
