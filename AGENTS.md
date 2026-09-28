# verger — project conventions

## Go skills (always load)

Before writing or reviewing Go code, load the `golang-how-to` orchestrator skill and
`use-modern-go`. Follow the returned guidelines.

`docs/DESIGN.md` is the source of truth for design. `docs/TASKS.md` is the task board;
each task's spec lives in `docs/tasks/`.

## TDD (mandatory)

1. Write the full test matrix first: happy paths, error paths, edge cases, boundary
   values, concurrency where relevant.
2. Run the tests and capture the red output.
3. Implement until green.
4. `make fmt lint test` must pass before a task is considered implemented.

A task is not done until an agent other than its author verified it (see `docs/tasks/`
verification protocol) and a reviewer other than its author reviewed the diff.

## Commands

- build: `make build`
- test: `make test`
- lint: `make lint`
- fmt: `make fmt`
- module changes: `make mod` (only after adding a new import)

## Testing style (GoConvey GWS)

- Tests use GoConvey Given-When-Should: `Convey("Given …", t, func() { Convey("When …", func() { Convey("Then …", func() { So(…) }) }) })`.
- `So(actual, matcher, expected)` — Convey matcher order; matchers take no description string (only the expected value where the matcher requires one: `ShouldHaveLength`/`ShouldEqual`/`ShouldResemble`/`ShouldContain` — one; `ShouldBeNil`/`ShouldBeTrue`/`ShouldBeFalse`/`ShouldBeError`/`ShouldBeEmpty`/`ShouldNotBeNil` — none).
- Scalars/strings → `ShouldEqual`; structs/slices/maps → `ShouldResemble`; JSON → `ShouldEqualJSON`; wrapped errors → `errors.Is(err, target)` with `ShouldBeTrue`.
- No `t.Parallel()` (GoConvey is incompatible); no `t.Run` (use `Convey`); keep `TestXxx` names.
- Helpers stay stdlib (`t.Helper`, `t.Fatalf`/`t.Fatal`); never call `So` outside a Convey block.
- testify is not used anywhere — do not add it.

## Test home isolation

Any package that touches the home directory, the store or agents' configs must pin
`HOME`, `VERGER_HOME`, `BEADLE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME` to a temp dir
in `TestMain` and carry a `TestSuiteHomeIsolation` pin test asserting isolation is
active (pattern: beadle `pkg/cli/home_isolation_test.go`). A test that forgets its own
`t.Setenv` must never be able to touch the developer's real home.

## Layout

- `cmd/verger` — entry point
- `pkg/verger` — public facade
- `pkg/cli` — cobra command tree
- `pkg/tui` — bubbletea
- `pkg/home` — home discovery, absorb/eject, flock, lease
- `pkg/spec`, `pkg/lock` — desired state (TOML) and exact state (JSON)
- `pkg/resolve` — input → source: site adapters, repo classification, live search (no own registry, D30)
- `pkg/source` — fetch: github/git/url/npm/local/mcp-registry
- `pkg/manifest` — format parsers → normalized Package
- `pkg/pack` — chimera render (synth + `verger pack`)
- `pkg/render` — hooks/agents/commands/mcp/rules→skill per dialect
- `pkg/host` — Host interface + all ten adapters (claude, codex, gemini, omp, cursor, opencode, kilo, pi, agy, dsh); agy/pi/dsh are `experimental` until a live green run
- `pkg/hostcli` — host CLI runner, probe, typed errors
- `pkg/caps` — HostCaps, probe, capsHash
- `pkg/plan` — pure plan function
- `pkg/apply` — executor: two phases, trash, RMA, circuit breaker
- `pkg/receipt` — receipts, journal, tombstones
- `pkg/consent` — hooks consent, project trust
- `pkg/secret` — OS keychain + `{secret:NAME}`
- `pkg/fsutil` — atomic writes, path helpers
- `pkg/digest` — sha256 and tree digests
- `pkg/store` — package store, trash, runtime, cache
- `pkg/watch` — fsnotify + debounce + lease
- `pkg/runtime` — embedded host runtime: bundle install, shim rendering, consent, heartbeat
- `runtime/` — TypeScript runtime adapters (OpenCode/Kilo/Pi) → go:embed

## Conventions

- No `internal/` — all packages live under `pkg/`; entry point in `cmd/verger`.
- Dependencies are vendored: after adding an import run `make mod`.
- Mandated libraries: `samber/lo`, `samber/oops`, `vmkteam/embedlog`, `tailscale/hujson`, `pmezard/go-difflib`, `hashicorp/go-set/v3`.
- Go 1.27 idioms: `errors.AsType`, `sync.WaitGroup.Go`, `t.Context()`, `b.Loop()`, `json:omitzero`, `min`/`max`, `slices`/`maps`.
- No `init()` side effects, functional options for constructors, doc comments on exported symbols.
- Platforms: linux and macOS only — Windows is out of scope.

## Agent rules (orchestrated sessions)

- Work only inside `/Users/universe/my/verger`. Never modify other directories
  (`beadle`, `topscan`, …) — read-only reference at most.
- Never run `git commit`, `git push`, `git checkout` of others' work, `git reset`,
  or `git stash`: the orchestrator owns git state. Leave changes in the working tree.
- Never delete or rewrite another task's files; append-only reports under
  `docs/reviews/`.
- Report format: write the full report to a file, reply with at most 10 lines
  (status, files touched, evidence commands + results, open questions).
