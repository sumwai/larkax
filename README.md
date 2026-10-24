# larkax

One API surface in front of many upstream providers: routing, metering, and a billing
engine that can express real-world pricing. Existing OpenAI, Anthropic and OpenAI
Responses SDKs work by pointing `base_url` here.

[中文](README.zh-CN.md)

## Install

| What | Where | Notes |
|---|---|---|
| Container image | `ghcr.io/sumwai/larkax:<tag>` | API and console in one image, `linux/amd64` and `linux/arm64` |
| Binaries | `larkax_<tag>_linux_<arch>.tar.gz` | `larkax`, `larkaxctl`, man pages, `LICENSE`, `README.md` |
| Console only | `larkax-web_<tag>.tar.gz` | static files; point `LARKAX_WEB_DIR` at them |
| From source | this repository | needs Go 1.26+ and Docker; Node 20+ only for the console — see [Build from source](#build-from-source) |

Every release also ships `SHA256SUMS` covering the tarballs; check a download with
`sha256sum -c --ignore-missing SHA256SUMS`. The image is verified by its digest at pull
time instead — `docker pull` output prints it.

## Run it

```sh
docker run --rm -p 8080:8080 \
  -e LARKAX_DB_DSN='postgres://user:password@host:5432/larkax' \
  -e LARKAX_MASTER_KEYS="k1:<base64-of-32-bytes>" \
  ghcr.io/sumwai/larkax:<tag>
```

That serves the console at `http://127.0.0.1:8080/` and the API under `/v1/`. Migrations
are not automatic: run them first with the same image,
`docker run --rm --entrypoint larkaxctl ... migrate up`. PostgreSQL is the only required
dependency; Redis is optional and only moves rate-limit counters out of process.

```sh
curl -sS 127.0.0.1:8080/v1/models -H "Authorization: Bearer $KEY"
```

A fresh instance has no models, channels, prices or keys, so that call answers `401`
until a key exists. The walkthrough that creates all five — model, channel, outbound
mapping, price, wallet, key — is in [QUICKSTART.md](QUICKSTART.md); the long form with
the reasoning behind each step is [docs/development.md](docs/development.md) (Chinese).

The command output is Chinese today. Commands, exit codes and printed values are stable;
the prose around them is not translated yet.

## Build from source

Go 1.26+, Docker with `docker compose` v2.1+, `make`, `openssl`. Ports `8080`, `9090`
and `5432` have to be free; `LARKAX_HTTP_ADDR`, `LARKAX_INTERNAL_ADDR` and `PG_PORT` move
them. Node 20+ is needed only for the console (`make web`) and for `make release`.

```sh
make run       # PostgreSQL, migrations, service in the foreground
make test      # needs the dependencies up: `make test` starts them
make install   # binaries, completions for bash/zsh/fish, man pages → ~/.local
make release   # the release assets: binaries + console + SHA256SUMS
```

`go test ./...` on its own skips the integration tests when `LARKAX_DB_DSN` is unset —
those packages still print `ok`, so a green run there means they were not tested at all.
`make test` is the one that starts the database first.

## Configuration

Required before the service starts: `LARKAX_DB_DSN`, and `LARKAX_MASTER_KEYS` on the
`gateway` and `control` roles (a `worker` needs no master key). Everything else has a
default. The full list, what each one is for, and the rule for deciding whether a new
setting belongs in the environment or in the database is in
[docs/development.md](docs/development.md#环境变量).

The console is served only when `LARKAX_WEB_DIR` points at an unpacked console bundle;
without it the public listener serves the API alone and answers `501` for other paths.

## Status

Works end to end: authentication, rate limiting, routing, reservation, settlement, and a
ledger that records every movement. Four wire protocols (three of them streaming), a
second entry point for typed judgement questions, expression-based pricing with
time-of-day and context-length tiers, a browser console, and a JavaScript plugin system.

Not there yet: a real payment channel — the callback path and ledger entries exist, but
the only implementation is a fake one, off by default. Two of the three quota sources are
unwired. And nothing here is production-hardened: no HA story, no operator runbook, no
load testing.

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

See [CONTRIBUTING.md](CONTRIBUTING.md). The repository's conventions and red lines are in
[AGENTS.md](AGENTS.md).

## License

AGPL-3.0. See [LICENSE](LICENSE).

## Acknowledgements

larkax builds on other people's work:

- [new-api](https://github.com/QuantumNous/new-api) — `relaykit` provides the protocol
  DTOs and the OpenAI ↔ Anthropic conversion; AGPL-3.0 with terms retained in [NOTICE](NOTICE).
- [moejs](https://github.com/Calcium-Ion/moejs) — the JavaScript runtime the plugin
  sandbox runs on.
- [expr-lang/expr](https://github.com/expr-lang/expr) — the expression engine for
  pricing rules.
- [holiday-cn](https://github.com/NateScarlet/holiday-cn) — mainland China holiday
  calendar data.
- [models.dev](https://github.com/sst/models.dev) — model metadata (planned).
