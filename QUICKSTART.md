# From a fresh instance to one successful request

This is the shortest path through the five objects a request needs: a canonical model,
a channel pointing at an upstream, the outbound name mapping, a price rule, and a
wallet with an API key. It assumes a running instance — the container command is in
[README.md](README.md#run-it).

`larkaxctl` below is the installed binary. From a clone, replace it with
`go run ./cmd/larkaxctl`; inside the image, use
`docker run --rm --entrypoint larkaxctl ...`.

## What the upstream actually calls its models

Ask the upstream first. `--models` and `channel map add` take the **upstream's own model
id**, which is often not the name on its marketing page:

```sh
printf '%s\n' '<your-upstream-key>' > ~/upstream-key.txt
curl -s https://api.example.com/v1/models \
  -H "Authorization: Bearer $(cat ~/upstream-key.txt)"
```

## The five steps

```sh
# 1. A canonical model. This name is the key shared by routing and billing.
larkaxctl model create --model claude-x --display 'Example model' \
  --vendor example --context-limit 200000 --tools --vision

# 2. A channel. The credential comes from stdin or an environment variable, never
#    from an argument: arguments show up in `ps` output and shell history.
cat ~/upstream-key.txt | larkaxctl channel create \
  --name my-upstream --type openai --base-url https://api.example.com/v1 \
  --models claude-x --credential-stdin

# 3. The outbound mapping: canonical name -> the upstream's model id.
larkaxctl channel map add --all-channels claude-x <upstream-model-id>

# 4. A user, then a price rule. A rule records who defined it, so the user comes first.
larkaxctl user create --name alice
larkaxctl price add example-price --scope platform --by alice \
  --in 1.5 --out 7.5 --cr 0.2 --cw 1.9 --reasoning 9

# 5. A funded wallet, then a key.
larkaxctl wallet create --user alice
larkaxctl wallet credit --user alice --amount 100 --ref ORD-1
KEY=$(larkaxctl apikey create --user alice)
```

The five price flags are prices **per million tokens**, the way provider price sheets
state them: `--in 1.5` means 1.5 per million input tokens. The rule stores the per-token
rate. `--unit token` takes per-token rates directly, and `--expr` remains for rules with
tiers or time-of-day rates. All five items are written into the rule; the ones you leave
out are written as 0, and `--in` / `--out` must be given at least one.

A fresh wallet holds 0, and the reservation step rejects requests until it does not.

## The call

```sh
curl -sS 127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}'
```

A `200` carries the assistant text and the usage the bill was computed from. `model` is
the canonical name, not the upstream id.

## When it answers 503

`503 no available upstream for the requested model` does not name which piece is wrong.
`larkaxctl doctor` does: it reports the channel and the missing name.

Four things are worth knowing before debugging anything else:

- **Every usage slot needs a price.** A missing slot fails the request rather than
  charging less.
- **`--base-url` stops before the endpoint.** larkax appends `/chat/completions`, or
  `/messages` on an `anthropic` channel. Including the endpoint produces a doubled path
  and an upstream `404`, which reaches the caller as `502`.
- **`--type` has to match the upstream protocol.** `openai` in the example is a
  placeholder; an Anthropic upstream needs `--type anthropic`.
- **`larkaxctl ledger balance --account user:1`** answers "did the money move" from the
  ledger, not from the wallet's display value.

The long form, with the reasoning behind each step and every failure mode, is
[docs/development.md](docs/development.md) — in Chinese, as is everything under `docs/`.
