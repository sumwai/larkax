# larkax web console

The user-facing console for larkax: balance, usage, API keys, model pricing and
quickstart snippets. React + Vite + TypeScript, served from the same origin as
`/api/v1`.

## Requirements

- Node.js 18 or newer
- npm 9 or newer

## Run it

```sh
npm install
npm run dev          # Vite prints the local URL (default port 5173)
```

`npm run dev` proxies `/api` to the gateway's port. The port follows
`LARKAX_HTTP_ADDR` — read from the environment first, then from the repository's
root `.env` (the file above `web/`) — because that is where the gateway's listen
address lives. `":8081"`, `127.0.0.1:8081` and `0.0.0.0:8081` all yield port
8081; an address without a parsable port falls back to 8080. `VITE_API_PROXY`
overrides the whole target, and the dev server prints the chosen target when it
starts.

By default the app runs against an in-memory mock of the console API, so the UI
works before the Go side is ready. Set `VITE_API_MODE=http` to call the real
endpoints:

```sh
VITE_API_MODE=http npm run dev
```

Mock notes: any non-empty password signs in; keys created in the mock disappear
on a full page reload, while the signed-in session survives in `sessionStorage`.
The top of every page carries a dismissible demo-data banner; closing it hides it
for the session, and a reload shows it again.

## Build

```sh
npm ci
npm run build        # type-checks, then writes web/dist
npm run preview      # serve the built files locally
```

`web/dist` is the static bundle behind `/`, `/login` and `/app/*`. It has to be
served from the same origin as `/api/v1/*` (which is `internal/consoleapi`) and
with an SPA fallback: unknown paths return `index.html`, or a refresh on
`/app/keys` is a 404. The Go process does not serve this bundle — it only answers
the API and the payment callbacks — so where the bundle is hosted is a
deployment decision, not something this repository wires up. Do not commit it.

## Environment variables

| Name | Default | Meaning |
| --- | --- | --- |
| `VITE_API_MODE` | `mock` | `mock` for the in-memory fixture, `http` for the real API |
| `VITE_API_BASE` | `/api/v1` | API prefix; keep same-origin in production |
| `VITE_API_PROXY` | derived | Dev-server proxy target; overrides the derived one (not bundled) |
| `LARKAX_HTTP_ADDR` | `127.0.0.1:8080` | Gateway listen address, read from the environment or the root `.env`; only its port feeds the dev proxy default (not bundled) |

Only `VITE_*` variables reach the browser bundle; never put secrets here. The
proxy target defaults to `http://127.0.0.1:<port of LARKAX_HTTP_ADDR>`.

## Checks

```sh
npm run lint         # ESLint
npm run test         # Vitest (jsdom)
npm run format       # Prettier
```

The tests cover money formatting, CSRF header behaviour, login form validation,
and the keys list (empty state plus the one-time secret dialog).

## Routes

| Path | Page |
| --- | --- |
| `/` | Public landing page: positioning, base URL, sign-in |
| `/login` | Username and password (accounts are created by operators) |
| `/app` | Overview: balance, today's requests / tokens / cost, last 7 days |
| `/app/keys` | API keys: list, create, enable/disable, revoke, quota and rate limits |
| `/app/usage` | Usage by time window, model/key breakdown, billing path, request log |
| `/app/models` | Model catalogue and reference prices (read-only) |
| `/app/quickstart` | cURL, OpenAI SDK and Anthropic SDK examples |
| `/app/settings` | Display name, email, password, language, theme, sign out |

`/app/*` redirects to `/login` when there is no session.

## Layout

- `src/api/` — the only place that talks HTTP. `types.ts` is the contract,
  `client.ts` is the fetch client (CSRF header on writes, same-origin
  credentials), `mock.ts` is the same-shaped fixture. Pages import `api` from
  `src/api/index.ts`; swapping the implementation is a one-line change there.
- `src/styles/tokens.css` — design tokens. The visual reference is
  <https://p.larkax.com/components.html>; components read tokens and never
  hard-code colors.
- `src/components/` — UI primitives (`ui.tsx`, `icons.tsx`), app shell, date
  range picker, chart and code card.
- `src/pages/` — one file per route.
- `src/i18n/` — Chinese and English dictionaries; the browser language picks
  the default, and the settings page persists the choice.

Responsive behaviour: the sidebar is 232px at 760px and wider, collapses to a
64px icon rail between 520px and 760px, and is replaced by a top menu plus
bottom navigation below 520px. Tables become cards below 720px.
