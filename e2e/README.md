# e2e — end-to-end driver for all ten hosts (W3-E2E10)

Oracle-based e2e for every host of U4: `claude codex gemini agy cursor opencode
kilo pi dsh omp`. The real CLIs are installed the way a user installs them (npm
where they ship there, the release archive for opencode 2.x, cursor.com/install
for cursor-agent), the driver builds `verger`, runs install → status → the
host's own listing → file assertions → remove → status on a temp HOME, and
asserts what the host itself reports. **No LLM calls** — only list oracles
(JSON where the host has one), the config documents, and file assertions.

## The three legs

| Leg | What it proves | Test |
|---|---|---|
| fixture | a `./local` payload goes loose (never registers with the host CLI), then remove leaves no residue, then adopt of a host-installed copy | `TestE2ELocalFixtureScenario` |
| archive | a loopback-pinned remote archive reaches a host-registering rung (synth/native) and the host's own list sees it, then not after remove | `TestE2ERemoteArchiveScenario` |
| canon | a directory package carrying **skills + agents + commands + MCP + hooks** — the shape beadle's canon will have (DESIGN §9.3) — lands what the host can take, the receipt claims it, the files match the receipt, the hooks document carries the hook or the documented skip note, and remove cleans every value | `TestE2ECanonPackageScenario` |

Every leg ends with the same negative evidence: `verger status` is no longer
`current`, the host's own listing does not mention the package, and a name scan
of the host's config dirs finds no residue.

## Run locally

```sh
./e2e/run.sh                     # claude, all legs, in the container
./e2e/run.sh gemini              # any of the ten hosts
./e2e/run.sh kilo canon          # one leg: positive (default) | canon | negative | check
./e2e/run.sh claude noimage      # reuse an existing verger-e2e:local image
E2E_LOCAL=1 ./e2e/run.sh claude  # run on this machine instead of the container
E2E_LOCAL=1 ./e2e/run.sh claude check  # preflight only
```

Every mode runs a **preflight** first: `go test -tags e2e -list` must select the
leg's expected scenarios (`TestE2ELocalFixtureScenario` +
`TestE2ERemoteArchiveScenario`, `TestE2ECanonPackageScenario`, or
`TestE2ENegative`). A zero/partial match aborts with exit 1 and the `-list`
output, so a renamed or deleted scenario can never turn into a green
`[no tests to run]`. A missing host CLI still skips the scenario with an
explicit reason — the preflight guards names, not host presence.

Straight `go test`, without the script:

```sh
E2E=1 E2E_HOST=kilo go test -mod=vendor -tags e2e -count=1 -timeout 25m -v \
  -run 'TestE2E(Local|Remote|Canon)' ./e2e/...
```

Without `E2E=1` every scenario skips itself with a reason; a missing host CLI
also skips (with the exact install line to fix it), so the suite is safe in any
workspace. `E2E_PATH` prepends a directory to the child `PATH` (a host that
ships from another toolchain, e.g. dsh in a second node version).

### Linux

- **OrbStack VM** (fastest): all ten CLIs are already installed there. Copy the
  tree and a `linux/arm64` binary in, then run the matrix script with
  `E2E_LOCAL=1`-style env (`E2E=1 E2E_VERGER_BIN=<bin> E2E_HOST=<host>`); this is
  what `W3-E2E10-1.md` §2 reports.
- **Container**: `./e2e/run.sh <host>` builds `e2e/Dockerfile` (node 24 + Go +
  all ten CLIs) and runs the leg inside it.

### macOS

`E2E_LOCAL=1` against the installed hosts in an isolated HOME (never the real
home: `assertTempHome` refuses a temp dir outside `$TMPDIR` and refuses the
process HOME). `E2E_PATH` is the way to reach a host that lives in another node
version's `bin`.

## Hosts, installs and what CI can prove

| Host | Installed from | CI | Why |
|---|---|---|---|
| claude | npm `@anthropic-ai/claude-code` | ubuntu + macos | |
| codex | npm `@openai/codex` | ubuntu + macos | |
| gemini | npm `@google/gemini-cli` | ubuntu + macos | |
| cursor | cursor.com/install | ubuntu + macos | no npm distribution |
| opencode | `opencode.ai/files/bin/2.0.18/…` (digest-checked) | ubuntu + macos | 2.x is not on npm; the npm package `opencode-ai` is the 1.x line |
| kilo | npm `@kilocode/cli` | ubuntu + macos | |
| pi | npm `@earendil-works/pi-coding-agent` | ubuntu + macos | |
| dsh | npm `@deepseek-ai/dsh` | ubuntu + macos | needs Node ≥ 24 (the job sets it up) |
| omp | npm `@oh-my-pi/pi-coding-agent` + `bun` | ubuntu + macos | its launcher is a bun program |
| agy | — | **no job** | Antigravity is a desktop application; no `agy` binary exists on either OS, so the leg exists, skips with that reason and the report says so |

A host with no CLI on `PATH` skips with the install line; a host with no
*listing* oracle the driver can trust (opencode's service-backed table, dsh's
missing verb, agy's filesystem scan) carries `noOracleReason` and the leg
asserts receipts and files instead — `hostSpec.noOracleReason`, printed by the
scenario.

## Pinned versions (bump deliberately — OQ-T1.13.1)

Pins live in `e2e/hosts.go` (`*VersionPin`), `e2e/Dockerfile` (`ARG …`) and
`.github/workflows/e2e.yml` (the matrix); `E2E_VERSION` / `E2E_<HOST>_VERSION`
override them for experiments. The opencode archive digests are the ones the
`anomalyco/tap/opencode-v2` formula publishes.

## Known gaps

- The adopt leg runs with a `PATH` that resolves only the host under test:
  `verger adopt` installs the adopted package into **every detected host** and
  exits non-zero when one refuses it (finding F2 in `W3-E2E10-1.md`). Hosts whose
  oracle names a path (pi) are adopted by that path; hosts whose oracle carries
  no version (pi, dsh, opencode) skip the leg with the reason.
- gemini's receipt records no `mcp` artifact when the MCP server and the hooks
  share one document (`~/.gemini/settings.json`): the leg therefore asserts the
  MCP claim through the document itself (finding F3).
- `verger install` accepts only a `./relative` local ref; an absolute path,
  `local:<abs>` and `file:<abs>` are rejected (finding F1) — the canon leg
  installs `./canon` for that reason.
- The driver's residue check is name-based over the host's config dirs; a
  byte-exact home diff is a unit-test concern, not an e2e one. It skips
  host-owned files (`hostOwnedFiles`, today gemini's signed
  `extension_integrity.json`) and host-owned runtime subtrees
  (`hostSpec.residueSkip`, e.g. opencode's log and kilo's state dirs).
- Hooks are delivered only where the host has a hooks document (claude, codex,
  gemini, cursor); the other six record the documented skip note
  (`canonHooksBlocked`) and the leg asserts the hook never appears in their
  files.
