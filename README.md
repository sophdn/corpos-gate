# corpos-gate

A stack-agnostic gate orchestrator. You describe a repo's quality gate once, as
data in a `gate.yml`, and one tool runs it: formatting, vetting, linting,
building, tests, a coverage floor, and a vulnerability scan. It drives the
repo's own tools — `gofmt` and `go test` for Go, `tsc`, `eslint`, and `vitest`
or `jest` for TypeScript, `ruff` and `pytest` for Python — so the gate matches
how the project is already built.

It replaces the bespoke `precommit.sh` that every repo grows on its own. Instead
of a shell script per project, there is one orchestrator and a small adapter per
language.

## Why it exists

Four repos, four hand-written pre-commit scripts, each drifting from the others.
A coverage check here, a lint rule there, the same secret-scanning grep copied
into all of them. corpos-gate is the one place that logic lives. A repo declares
which checks it wants and at which stage, and the orchestrator turns that into an
ordered plan and runs it.

## The design

**One declarative `gate.yml`.** It names the stack, the build units, and the
enabled checks with their stage. Parsing is strict: an unknown key is an error,
not a silent no-op. A short config for a Go repo:

```yaml
stack: go
units:
  - dir: .
checks:
  format: { enabled: true, tier: pre-commit }
  vet:    { enabled: true, tier: pre-commit }
  build:  { enabled: true, tier: pre-commit }
  test:   { enabled: true, tier: pre-push }
```

**Three tiers, nested.** `pre-commit` is the fast gate you keep under a few
seconds. `pre-push` is a superset: every pre-commit check plus the slower ones,
the full test suite and the coverage floor. `ci` adds the slowest checks, such
as mutation testing. A `pre-push` run always includes the pre-commit checks, so
a check can never be skipped by running a higher tier.

**Per-stack adapters that run the app's own tools.** The Go adapter runs
`gofmt`, `go vet`, `golangci-lint`, `go build`, and `go test` with a coverage
floor. The TypeScript adapter runs `tsc`, `eslint`, and the project's `vitest`
or `jest`, gating on the four istanbul coverage metrics. The Python adapter runs
`ruff` and `pytest` with a coverage floor. A missing tool fails loudly rather
than skipping quietly, so a green line always means the check ran.

**Stack-agnostic git checks.** Some checks guard any repo regardless of
language: a secret or private key entering a diff, a repo-local git identity that
would misattribute every commit, and `shellcheck` over tracked shell files. One
of them, `gate-tamper`, compares the staged `gate.yml` against the committed one
and refuses a commit that weakens the gate — a removed check or a lowered
coverage floor. Weakening the gate is the cheapest path to green, so the gate
guards itself.

**`corpos-gate init` aligns to the app you point it at.** It detects the stack,
writes a starter `gate.yml` that drives that stack's tools, and seeds the
coverage floor from the app's own config where one exists: a `jest`
`coverageThreshold`, or `coverage.py`'s `fail_under`. Where no such setting
exists, it leaves the floor unset rather than inventing a number. Then it prints
what it detected and where each threshold came from.

**A sans-IO core.** The orchestrator injects the command runner, so every
check's command construction is unit-tested with a fake runner that records the
calls and spawns no process. The one external dependency is a YAML parser. The
suite holds itself to a 95% coverage floor, aggregate and per-package (currently
97.6% overall; `internal/gate` 98.1%, `cmd/corpos-gate` 95.7%, `internal/gitenv`
100%).

## Prerequisites

corpos-gate drives the app's own tools rather than bundling its own, so the
tools a repo's enabled checks call must be on `PATH`. corpos-gate itself needs
only Go to build.

- **Go lint** — the `lint` check runs `golangci-lint`. Install the version CI
  pins:

  ```sh
  go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.2
  ```

  The lint check finds it on `PATH` or under `$(go env GOPATH)/bin`; if it is
  absent the check SKIPs with an install hint rather than failing.
- **TypeScript** — the adapter drives `tsc` (typecheck), `eslint` (lint), and
  the project's `vitest` or `jest` (coverage), each via `npx --no-install`.
- **Python** — the adapter drives `ruff` (format + lint), `mypy` (typecheck),
  and `pytest` with `pytest-cov` (coverage).

## Quickstart

Install the binary:

```sh
go install github.com/sophdn/corpos-gate/cmd/corpos-gate@latest
```

Point it at a repo. `init` detects the stack, writes a starter `gate.yml`, and
installs gate-only git hooks:

```sh
cd your-project
corpos-gate init
```

See the plan without running it, then run a tier:

```sh
corpos-gate plan --tier=pre-push
corpos-gate run  --tier=pre-commit
```

Emergency bypass is git-native (`git commit --no-verify`); the hooks do nothing
to defeat it.

## Demo

`examples/demo.sh` needs nothing but Go and the built binary. It creates
throwaway projects in a temp directory and shows corpos-gate working on each:

- a Go module, where it catches a formatting error, fails, and passes once the
  file is fixed — a real red-then-green run;
- a TypeScript project whose `package.json` carries a `jest` coverage threshold,
  where `init` infers that threshold into the starter config;
- a Python project whose `pyproject.toml` sets `fail_under`, where `init` infers
  that floor.

```sh
go build -o /tmp/corpos-gate ./cmd/corpos-gate
PATH="/tmp:$PATH" ./examples/demo.sh
```

## Packages

| Package | What it holds |
|---|---|
| `internal/gate` | The orchestrator: config parsing, the tier model, the check plan, the Go/TypeScript/Python adapters, the stack-agnostic git checks, and the declarative rule checks. |
| `internal/gitenv` | One definition of the git-environment scrubbing every git-invoking call needs, so a command runs against the directory it names. |
| `cmd/corpos-gate` | The CLI: `run`, `plan`, and `init`. |

## Provenance

This is a self-contained extract from a larger private agent system. The
orchestrator, the three adapters, and the CLI were lifted out as a coherent
subset and given a clean module path so the tool stands on its own. The one
change to the extracted logic is `init`, which now infers coverage thresholds
from an app's own config.
