# Contributing to larkax

Thanks for taking the time to contribute. This file covers how to get involved;
the repository's own conventions and red lines are in [AGENTS.md](AGENTS.md).

## Getting started

- Build it, run it, and get a request through: [docs/development.md](docs/development.md).
  The [README](README.md) has the shortest path, from a clone to a real completion.
- Before changing anything, read the [LESSONS.md](LESSONS.md) index. Each entry says
  when it is worth reading; they record problems already encountered here, so you do
  not run into them again.
- Everything under `docs/` is written in Chinese. `README.md`, this file and
  `.env.example` are in English.

## Issues and pull requests

- Open an issue before a change that is not a bug fix, so the approach can be agreed
  on first. The templates in [`.github/ISSUE_TEMPLATE/`](.github/ISSUE_TEMPLATE/) ask
  for what is needed.
- Work on a branch off `main` — `main` only takes pull requests. Branch names use a
  prefix: `feat/`, `fix/`, `refactor/`, `docs/`, `test/` or `chore/`.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org/).
- Fill in the [pull request template](.github/PULL_REQUEST_TEMPLATE.md): what the
  change does, what you ran, and what is left out. Write `Closes #123` to close the
  issue — the number in the title does not.
- Run the checks the change touches before opening the pull request. Which ones, and
  how, is in [docs/development.md](docs/development.md) under "常用检查" and
  "CI 跑什么".

## Labels

Issues carry three kinds of labels:

- `area:*` — which part of the code it touches.
- `priority:*` — `p0-critical` through `p3-low`.
- `status:*` — where it stands:
  - `status:ready` — actionable now, not scheduled yet.
  - `status:working` — someone is on it.
  - `status:tracking` — a registered gap. Nobody is waiting on anything in
    particular; the entry exists so the gap is not rediscovered next month.
  - `status:blocked` — cannot proceed, and not because of another issue here.

Waiting on something specific gets its own label, so it does not hide inside
`status:tracking`:

- `wait:#123` — cannot start until #123 lands. Add it when the dependency
  appears; remove it when #123 closes, and re-judge the status at the same time.
  Delete the label itself once no issue carries it.
- `wait:decision` — waiting on a decision or a fact from outside this repository,
  with no issue to point at.

The reason for the separate label is that the issue list shows labels but not
blocked-by relationships: `status:tracking` says a gap exists, `wait:#123` says
why it is not moving.

## Red lines

This is a public repository. Source, commit messages, issues and pull requests must
not contain credentials, private hosts or paths, real upstream accounts, or personal
information. The exact list and the placeholder conventions are in
[AGENTS.md](AGENTS.md), and `scripts/check-redlines.sh --all` scans for them.
