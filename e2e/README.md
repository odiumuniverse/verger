# e2e — container end-to-end driver (T1.13)

Oracle-based e2e for the three Ф1 hosts (Claude, Codex, Gemini): the real CLIs
are installed from npm in a `node:22-bookworm` container, the driver builds
`verger`, runs install → status → host list → remove → status on a temp HOME,
and asserts the host's own JSON output. **No LLM calls** — only
`plugin/extensions list --json` oracles and file assertions.

## Run locally (Docker)

```sh
./e2e/run.sh                # claude leg (builds the image once)
./e2e/run.sh gemini         # claude|codex|gemini
./e2e/run.sh claude noimage # reuse an existing verger-e2e:local image
./e2e/run.sh claude negative # negative leg: no host CLI on PATH
./e2e/run.sh claude check   # preflight only: assert the scenario tests exist
E2E_LOCAL=1 ./e2e/run.sh claude check  # the same without Docker (local Go)
```

Every mode runs a **preflight** first: `go test -tags e2e -list` must select the
leg's expected scenarios (`TestE2ELocalFixtureScenario` +
`TestE2ERemoteArchiveScenario`, or `TestE2ENegative`). A zero/partial match
aborts with exit 1 and the `-list` output, so a renamed or deleted scenario can
never turn into a green `[no tests to run]`. A missing host CLI still skips the
scenario with an explicit reason — the preflight guards names, not host
presence. The CI jobs run the same guard before their test steps.

Or in a tmux pane without Docker, when the host CLI is installed locally:

```sh
E2E_LOCAL=1 ./e2e/run.sh claude   # same preflight + scenario, local Go/host CLI
E2E=1 E2E_HOST=claude go test -tags e2e -count=1 -timeout 25m -v -run 'TestE2E(Local|Remote)' ./e2e/...
```

Two scenarios: the local fixture (loose → oracle must not see it; synth →
oracle must see it) and the loopback-pinned remote archive (always
host-registering → oracle must see it).

Without `E2E=1` every scenario skips itself with a reason; a missing host CLI
also skips (with the exact `npm install -g` line to fix it), so the suite is
safe in any workspace.

## Pinned versions (bump deliberately — OQ-T1.13.1)

| Host   | npm package                  | pin     |
|--------|------------------------------|---------|
| claude | `@anthropic-ai/claude-code`  | 2.1.283 |
| codex  | `@openai/codex`              | 0.157.1 |
| gemini | `@google/gemini-cli`         | 0.61.0  |

Pins live in `e2e/hosts.go`, `e2e/Dockerfile` and `.github/workflows/e2e.yml`;
`E2E_VERSION` / `E2E_<HOST>_VERSION` override them for experiments.

## Known gaps

- **codex adopt leg** is skipped: the `codex plugin` subcommand/JSON grammar is
  unverified (OQ-T1.7.1). The install/remove/status scenario still runs.
- **gemini `extensions link` semantics** (absolute path vs copy) are confirmed
  by the CI run (OQ-T1.8.1).
- `claude plugin list --json` output shape is confirmed by the CI run
  (OQ-T1.6.2).
- The driver's residue check is name-based over the host's config dirs; a
  byte-exact home diff is a T1.9/T1.12 unit-test concern, not an e2e one.
