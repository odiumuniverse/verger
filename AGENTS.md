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
- **Go files: `edit` only.** Never `sed -i`, `python -c` or any scripted rewrite
  of a `.go` file, including your own — after a compaction the anchor is gone
  and a blind rewrite corrupts a neighbour's line.
- **Hermetic tests are mandatory.** A test must pass with nothing of the
  developer's machine behind it:
  `B=$(mktemp -d); ln -s $(which go) $B/go; ln -s $(which git) $B/git; env -i
  HOME=$(mktemp -d) TMPDIR=/tmp PATH=$B:/usr/bin:/bin go test -count=1 ./...`
  A test that passes only because an agent binary happens to be installed is a
  broken test: put a shim on PATH instead, or skip when the binary is absent.
  A test that asserts only the *absence* of a phrase passes vacuously when the
  code under it never ran — add a positive assertion.
- **KEYCHAIN RULE.** No test and no diagnostic may touch the real OS secret
  store. On macOS an isolated HOME makes the keychain look missing and the OS
  answers with a modal "Reset To Defaults" — a keyring is triggered by a tool
  deciding where to keep a string. Inject a panicking fake; never probe the real
  keychain even read-only.
- **INSTALL RULE.** Never install anything, and never write outside the repo or
  a `mktemp -d` tree. No package managers, no `go install`, no `brew`.
- `docs/` is local only and is **not committed**. It is the design record and
  the report trail; treat it as scratch you may write but never as tracked state.
- No `Co-Authored-By` trailer and no AI attribution anywhere in the tree.
- Git state belongs to ops: no `git commit`, `push`, `reset`, `stash` or
  `checkout` of others' work. Changes are left in the working tree and handed
  over for the commit.
- Report format: write the full report to a file, reply with at most 10 lines
  (status, files touched, evidence commands + results, open questions).

## Gates (all of them, every change)

```
gofmt -l pkg/ cmd/                                     empty
go vet ./...
golangci-lint run ./pkg/...                            0 issues
GOOS=linux golangci-lint run ./pkg/...                 0 issues
hermetic go test -count=1 ./...                         no FAIL
docs/tasks/repro-g4.sh <built binary>                   17/17
```

`golangci-lint` refuses to run in parallel with itself: if it reports a
parallel run, wait and re-run rather than assuming a clean result.
