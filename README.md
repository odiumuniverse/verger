<div align="center">

  <h1>verger</h1>

  <p><b>One manifest for every coding agent.</b><br>
  Each host gets what it can actually take — and what it cannot says why.</p>

  <p>
    <a href="https://github.com/odiumuniverse/verger/releases"><img alt="release" src="https://img.shields.io/github/v/release/odiumuniverse/verger?style=flat-square&color=3E6E5C&labelColor=1F2328"></a>
    <a href="https://github.com/odiumuniverse/verger/actions/workflows/ci.yml"><img alt="ci" src="https://img.shields.io/github/actions/workflow/status/odiumuniverse/verger/ci.yml?branch=master&style=flat-square&label=ci&color=3E6E5C&labelColor=1F2328"></a>
    <img alt="go" src="https://img.shields.io/badge/go-1.27-3E6E5C?style=flat-square&labelColor=1F2328">
    <img alt="license" src="https://img.shields.io/badge/license-MIT-3E6E5C?style=flat-square&labelColor=1F2328">
  </p>

</div>

[Why](#why) · [Install](#install) · [Quick start](#quick-start) ·
[Supported hosts](#supported-hosts) · [How it works](#how-it-works) ·
[Safety](#safety) · [Roadmap](#roadmap) · [Development](#development)

## Why

Every coding agent keeps its own plugins, skills, MCP servers, subagents,
commands and hooks — in its own directory, in its own dialect. Install the same
package into three agents and you maintain three copies by hand until they
drift, and nothing tells you which agent is missing what.

verger does that job once. You describe the packages you want in
**`verger.toml`**, the exact versions and hashes land in **`verger.lock`**, and
`verger install` delivers each package to every host it has an adapter for,
through the first strategy that host actually supports. What landed on disk is
recorded in **receipts**, so `verger status`, `verger why` and `verger remove`
answer from evidence instead of guessing.

The table of hosts is the fact, not a promise: a host with no adapter is refused
with `no adapter in this build` rather than pretending to be "not detected".

## Install

```bash
brew install odiumuniverse/tap/verger
```

The formula is **created by the first tagged release**: the release workflow
builds the tarball, then writes `Formula/verger.rb` in `odiumuniverse/homebrew-tap`
from that artifact's own url and sha256. The tap is never seeded with a
placeholder, so until a `v*` tag is pushed there is nothing for Homebrew to
install yet.

Or build the binary yourself — the module is `github.com/odiumuniverse/verger`
and the binary is `verger`:

```bash
go install github.com/odiumuniverse/verger/cmd/verger@latest
```

There are no tags yet, so `@latest` resolves to the tip of `master` as a Go
pseudo-version; it needs access to the repository. From a checkout:

```bash
make build          # bin/verger
```

Releases are macOS universal binaries (arm64 + amd64, signed); Linux builds from
source and runs — CI builds, vets and tests on ubuntu and macos.

## Quick start

A package is a directory that carries at least one agent's manifest. Here is a
minimal Claude Code plugin:

```
my-plugin/
├── .claude-plugin/plugin.json     { "name": "caveman", "version": "1.2.3", … }
└── skills/
    └── caveman/
        └── SKILL.md               frontmatter with name + description
```

```bash
verger install ./my-plugin        # to every detected host, asking before hooks
verger install ./my-plugin --hosts claude -y
```

```
PACKAGE                  HOST     SCOPE    STRATEGY   VERSION
local:caveman            claude   user     loose      1.2.3
PACKAGE                  HOST     SCOPE    STATUS     STRATEGY   VERSION
local:caveman            claude   user     current    loose      1.2.3
```

`verger install` writes the manifest entry and then reconciles. The file it
wrote:

```toml
# ~/.verger/verger.toml — verger writes it, and hand-editing is fine
schema = 1

[[source]]
name = 'my-plugin'
url = './my-plugin'

[[package]]
id = 'local:caveman'
```

Then ask what is on the machine, and take it back off:

```bash
verger status                     # the package × host matrix
verger status --json              # same data, stable field names
verger why local:caveman claude   # why this cell is current, silenced or missing
verger remove local:caveman       # tombstone + reverse operations
verger restore local:caveman      # within 30 days, from the trash
```

```
PACKAGE                  HOST     LEVEL        SCOPE    STATUS     STRATEGY   VERSION
local:caveman            claude   stable       user     current    loose      1.2.3
```

`LEVEL` is the maturity of the adapter behind that cell — `experimental`, `beta`
or `stable` — promoted only by a green run, never by the adapter merely existing.
`verger doctor` checks the machine, the stores, the keyring and every host, and
`verger sync` makes the machine match the manifest (it installs what the manifest
gained and removes what it lost) without resolving anything new.

### Source references

`verger install` takes one reference per package:

| Reference | Example |
|---|---|
| a local directory | `./my-plugin` |
| an archive pinned by hash | `https://host/pkg.tar.gz#sha256=…` (a pin is required) |
| a GitHub repository or subdirectory | `owner/repo`, `owner/repo//skills/foo` |
| git and npm | `git+https://…`, `npm:@scope/pkg@1.2.3` |
| a package already in an agent | `claude:caveman` (see **adopt**) |

The local directory and the pinned archive are the two forms wired end to end
today. `owner/repo`, git and npm references are parsed and fetched, and covered
by `pkg/source`'s own tests, but the remote install path is not finished: it
currently fails while planning a package whose host-shaped version cannot be
derived, so a remote package that does not arrive as a pinned archive is not
installable yet. Resolving a remote source — including marketplace and short-name
lookups — is `pkg/resolve` and lands with T3.1. There is no registry of verger's
own; the plan is to read every package straight from its source.

## Supported hosts

| Host | What lands there | Verified |
|---|---|---|
| **Claude Code** | plugin registered through a Claude marketplace (native) or a generated local marketplace (synth); loose: `~/.claude/{skills,agents,commands}`, hooks in `settings.json`, MCP via `claude mcp add --scope user` | `stable` — full install / remove / adopt cycle green on 2.1.283 |
| **omp** (oh-my-pi) | plugin via `omp plugin marketplace add` + `omp plugin install` (native/synth); loose: `~/.agents/skills`, `<agentDir>/{agents,commands,rules}`, `<agentDir>/mcp.json`, hook modules under `<agentDir>/hooks/` | `stable` — e2e green locally on 18.4.1 |
| **Codex CLI** | plugin from a marketplace (native) or a local marketplace record (synth); loose: `~/.agents/skills`, `~/.codex/agents/*.toml`, `hooks.json`, `config.toml [mcp_servers]` | `beta` — both e2e scenarios green on codex-cli 0.157.1; the adopt leg is enabled and waits for a green CI run |
| **Gemini CLI** | extension via `extensions install` (native) or `extensions link` (synth); loose: `~/.gemini/{skills,agents,commands/*.toml}`, settings `mcpServers`/hooks | `experimental` — the 0.61.0 grammar was verified live, but the only recorded e2e run failed on the host-owned integrity store, and a green run is what promotes a host |
| **Cursor** | **loose only**: `~/.cursor/{skills,agents,commands}`, `~/.cursor/mcp.json`; rules travel as a skill wrapper; hooks are silenced (a flat camelCase dialect verger does not render). Native/synth are refused with the reason: no `cursor-agent` subcommand registers a package | `experimental` — live-verified on cursor-agent 2026.06.15; no CI leg, because the CLI ships from cursor.com/install rather than npm |
| **Antigravity, OpenCode, Kilo, Pi, DeepSeek Harness** | **not implemented yet** (Ф2) — the ids are known and `--hosts` refuses them with `no adapter in this build` instead of pretending | — |

A host is `experimental` until a green e2e run promotes it to `beta`, and a full
install / remove / adopt cycle on a live machine to `stable`. The evidence table
is `pkg/cli/maturity.go`; landing an adapter promotes nothing by itself.

## How it works

```
verger.toml   desired state: sources, packages, pins, propagation
     │
     ▼
  reconcile ──▶ per host: the first strategy that works
     │              native  → the host's own installer, from the host's own source
     │              synth   → a package verger renders in the store, installed by the
     │                        host's own mechanism from a local path
     │              loose   → components in the host's own user-level dirs and configs
     │              silenced(reason) → not possible now: kept in the desired state
     │                        with the reason, delivered later when a rung appears
     ▼
verger.lock   exact versions, hashes, strategies — every cell
receipts      what actually landed, and the reverse operations to take it back
```

- **Delivery is a pure function** of the package, the host's capabilities and
  your policy: the same desired state re-plans when a host is installed, updated
  or gains an adapter.
- **A chimera package** is one directory that is valid as a `.claude-plugin`,
  `gemini-extension.json`, `.codex-plugin` and more at once, so one render
  reaches several hosts. The host sees the **author's** name (`name@owner`),
  never "verger".
- **The oracle decides.** After a delivery, the host's own JSON listing
  (`claude plugin list --json`, `gemini extensions list -o json`, `codex plugin
  list --json`, `omp`) has to show the package. A host that did not see it drops
  the plan a rung and notes it. No LLM calls are ever made.
- **`silenced` is a state, not an error.** A component a host cannot take stays
  in the desired state with its reason and hint, and `verger why` prints both.
- **Removal is symmetric.** Every delivery records its reverse operations, so
  `verger remove` retires the package everywhere it went and puts the artifacts
  in the trash for 30 days.

`~/.verger` holds `verger.toml`, `verger.lock` and `state/` (receipts,
`journal.jsonl`, consent, trust, tombstones); the package store — fetched
sources, rendered packages, the trash — lives in `~/.local/share/verger`, because
hosts hold paths into it and it never moves. `--home` overrides the home.

## Safety

- **Host-owned state is never written.** verger leaves files a host CLI owns
  outright alone: gemini's signed `~/.gemini/extension_integrity.json` is exactly
  as it was after an uninstall (removal goes through the CLI alone), and omp's
  own logs and prebuilt natives are not treated as residue. A user's
  `AGENTS.md`/`GEMINI.md` is not rewritten either — rules are delivered as a
  skill wrapper instead, and the delivery says so.
- **The host's policy outranks verger.** What a host's managed settings forbid
  (marketplace allow/deny lists, admin-controlled plugins, enterprise Codex
  policy) is not delivered by any rung, native or not; the cell is
  `blocked-by-policy` with the rule that blocked it.
- **A package you installed by hand is adopted, not duplicated.**
  `verger adopt <host:ref>` takes a package an agent already knows, records it in
  the manifest and delivers it to the other hosts. Adopt never installs hooks.
- **Hooks need consent.** `verger install` asks before installing hooks
  (`[Y/n]`, default yes under `-y`), and the answer is remembered against the
  hooks' content hash — a changed hook asks again. `verger approve` and
  `verger revoke` manage that consent.
- **omp hooks are code, and omp does not sandbox them.** They are TS/JS modules
  under `<agentDir>/hooks/{pre,post}/`; omp has no trust gate for them, so a
  delivered module runs with your full rights. **Restart omp after a delivery** —
  a live session keeps the modules it loaded at startup, and reloading plugins
  does not reload them.
- **A project manifest must be trusted.** The first run against a project
  `verger.toml` asks you to trust it by content hash (`verger trust`); a changed
  file asks again.
- **Secrets stay out of the manifest.** `verger secret` keeps values in the OS
  keychain; a host that needs the value literally gets it in a `0600` config
  file, and it never reaches `verger.toml`, `verger.lock` or the logs.
- **Removal is guarded.** Only a targeted disappearance, with the host alive,
  its registry parsed and a matching receipt, counts as a removal — not an
  orphan sweep, not a rename, not a host reset.

## Roadmap

**Ф2** — `verger watch` (auto-spread a hand-installed package to the other
hosts, with guarded auto-remove), the remaining adapters (Antigravity, OpenCode,
Kilo, Pi, DeepSeek Harness), `pkg/caps` + `pkg/plan` and the TUI — has not
started; today `verger watch` is a stub that says so. **Ф3** is the source
resolver and live search.

## Development

```bash
make fmt lint test      # what CI runs
make build              # bin/verger
make mod                # go mod tidy + vendor
```

Tests use GoConvey GWS, assert against the real filesystem in an isolated home,
and never call an LLM. The host adapters have a container end-to-end driver —
see [`e2e/README.md`](e2e/README.md).

## Release

Pushing a `v*` tag is the whole procedure. The workflow builds
`./cmd/verger` for darwin/arm64 and darwin/amd64, merges them with `lipo`, signs
the result, packs `verger-<version>-darwin-universal.tar.gz`, creates the GitHub
release and bumps `Formula/verger.rb` in the tap from that artifact's url and
sha256 — the first tag creates the formula, later tags only rewrite its url and
hash.

Architecture notes and the task board live in `docs/`; they are internal
working documents and are not part of this tree yet.

MIT — see [LICENSE](LICENSE).
