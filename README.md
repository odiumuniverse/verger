<div align="center">

  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/verger-dark.svg">
    <img src="assets/verger-light.svg" width="72" height="72" alt="verger">
  </picture>

  <h1>verger</h1>

  <p><b>One plugin spec for every AI coding agent.</b><br>
  Delivered as real files, from one spec you keep in version control.</p>

  <p>
    <a href="https://github.com/odiumuniverse/verger/releases"><img alt="release" src="https://img.shields.io/github/v/release/odiumuniverse/verger?style=flat-square&color=3E6E5C&labelColor=1F2328"></a>
    <a href="https://github.com/odiumuniverse/verger/actions/workflows/ci.yml"><img alt="ci" src="https://img.shields.io/github/actions/workflow/status/odiumuniverse/verger/ci.yml?branch=master&style=flat-square&labelColor=1F2328"></a>
    <img alt="go" src="https://img.shields.io/badge/go-1.27-3E6E5C?style=flat-square&color=3E6E5C&labelColor=1F2328">
    <img alt="license" src="https://img.shields.io/badge/license-MIT-3E6E5C?style=flat-square&color=3E6E5C&labelColor=1F2328">
  </p>

</div>

Ten agents, ten separate plugin formats, and one machine that has to keep all of them in step. You
copy a plugin into one agent, discover three others are missing it, and the set drifts. verger
takes **one spec you write and keep**, resolves every package against it, and writes **real files
each agent already reads** — never a symlink into a store you have to understand, and never a
silent overwrite of something you edited.

```
~/.verger/                   your plugin home
├── verger.toml                the spec: sources in priority order, packages, channels, pins
├── verger.lock                the exact revision each package resolved to
├── trash/                     removed packages, until `verger gc` purges them
└── state/
    ├── receipts/<pkg>/        what was written, where, at which version
    ├── backups/<timestamp>/   your version, kept whenever --force overwrote one
    └── journal.jsonl          the run log
        ▼                                  ▲
        │  writes                           │  the agent reads its own
   ┌────┴─────┬─────────┬─────────┬──────────┴────┐
 Claude     Codex    Gemini    Cursor    OpenCode   Kilo
  + Antigravity + Pi + DeepSeek Harness + oh-my-pi
```

Only need plugins? You are in the right place. Want every agent's whole configuration — rules,
memory, skills, subagents, inbox — kept in sync across machines too?
→ [beadle](https://github.com/odiumuniverse/beadle), which embeds verger as its plugin manager.

[Why use it](#why-use-it) · [Supported agents](#supported-agents) · [Install](#install) ·
[Quick start](#quick-start) · [Guides](#guides) · [Doctor](#doctor) · [Packages](#packages) ·
[Hooks and trust](#hooks-and-trust) · [Sources](#sources) · [Secrets](#secrets) ·
[Watch](#watch) · [Authoring](#authoring) · [Reference](#reference) ·
[verger and beadle](#verger-and-beadle) · [Principles](#principles) · [License](#license)

## Guides

- **[For humans](guide/humans.md)** — the spec, the sources, how a package travels, receipts,
  conflicts, the watch lease and secrets.
- **[For AI agents](guide/ai-agents.md)** — what an agent may rely on when it touches plugins, and
  the three exit codes it must not resolve on its own.

![How verger delivers a package](assets/guide-architecture.svg)

## Why use it

- **One spec, ten agents.** `verger.toml` names every package once. `verger sync` makes each
  machine match it, in each agent's own format, without you writing the same list ten times.
- **Real files, your edits are yours.** Nothing is a symlink into a store, and a file you changed
  after it was delivered is never overwritten — the run refuses and exits 3. `--force` keeps your
  version under `state/backups/` first.
- **A receipt makes the next run free.** Every delivery records what was written, where and at
  which version, so a package that has not changed costs nothing and a package that vanished is
  reinstalled rather than reported as done.
- **Reproducible by the lock.** A channel decides the version when the lock is written; the lock
  is what is read back, so two machines on the same spec deliver the same bytes.

## Supported agents

Ten agents, named the same way everywhere: `claude`, `codex`, `gemini`, `agy`, `cursor`,
`opencode`, `kilo`, `pi`, `dsh`, `omp`. Older names still work wherever you type one —
`claude-code` for `claude`, `gemini-cli` for `gemini`, `antigravity-cli` for `agy`,
`deepseek-harness` for `dsh`.

| Agent | Skills | MCP | Agents | Commands | Hooks |
|---|---|---|---|---|---|
| Claude Code | ✓ `~/.claude/skills` | ✓ `~/.claude.json` | ✓ `~/.claude/agents` | ✓ `~/.claude/commands` | ✓ `~/.claude/settings.json` |
| Codex | ✓ reads `~/.agents/skills` | ✓ `~/.codex/config.toml` | ✓ `~/.codex/agents/*.toml` | ✓ `~/.codex/prompts` | ✓ `~/.codex/config.toml` |
| Gemini | ✓ `~/.gemini/skills` | ✓ `~/.gemini/settings.json` | ✓ `~/.gemini/agents` | ✓ `~/.gemini/commands` | ✓ `~/.gemini/settings.json` |
| Antigravity (`agy`) | — | ✓ `mcp_config.json` | ✓ `agents` | — | ✗ blocked |
| Cursor | ✓ `~/.cursor/skills` | ✓ `~/.cursor/mcp.json` | ✓ `~/.cursor/agents` | ✓ `~/.cursor/commands` | ✓ `~/.cursor/hooks.json` |
| OpenCode | ✓ reads `~/.agents/skills` | ✓ `opencode.jsonc` | ✓ `agents` | ✓ `commands` | ✗ blocked |
| Kilo | ✓ `skills` | ✓ `kilo.jsonc` | ✓ `agents` | ✓ `commands` | ✗ blocked |
| Pi | ✓ reads `~/.agents/skills` | ✓ `mcp.json` | — | ✓ `prompts` | ✓ extensions module |
| DeepSeek Harness (`dsh`) | ✓ `~/.dsh/skills` | ✓ `cordis.patch.yml` | ✓ plugin-provided | ✓ plugin-provided | ✗ blocked |
| oh-my-pi (`omp`) | ✓ reads `~/.agents/skills` | ✓ `mcp.json` | ✓ `agents` | ✓ `commands` | ✗ blocked |

A ✓ means verger writes that surface; `reads …` means the agent reads a shared skills directory
itself, so those packages are delivered to the shared `~/.agents/skills` hub rather than to the
agent's own directory.

**Five hosts do not take hooks yet, and verger says so instead of guessing.** Each refusal carries
its reason in the cell:

- **omp** — its extension API has no lifecycle event stream; during a real authenticated model call
  nothing a delivered module was given ever fired.
- **opencode** — no session-start event to register on; the v2 event table carries null there, so a
  delivered hook has nothing to hook into.
- **kilo** — its only plugin mechanism is `kilo plugin <module>`, which takes an npm module name
  rather than a local path, so a rendered directory cannot be handed to it.
- **dsh** — it has no hook document at all; hooks reach it only through a Claude Code plugin's
  config string.
- **agy** — its `hooks.json` is owner-keyed (top-level plugin names, no hooks wrapper), a dialect
  verger does not render.

Hooks on the other five are delivered in the host's own shape, and a package carrying hooks does
not run them until you `verger approve` the content hash.

## Install

**macOS or Linux** — Homebrew:

```bash
brew tap odiumuniverse/tap
brew trust odiumuniverse/tap
brew install verger
```

The formula serves the macOS build on macOS, and on Linux the native `arm64` or `amd64` one, so
the same three commands work with Linuxbrew. It installs the binary, the shell completions and the
man page, so there is nothing to wire up afterwards:

```
man verger
verger completion zsh    # the formula already put these where your shell finds them
```

**macOS or Linux** — build from source (needs the Go toolchain):

```bash
git clone https://github.com/odiumuniverse/verger.git
cd verger
go build -o verger ./cmd/verger
```

The module is `github.com/odiumuniverse/verger` and the binary is `verger`. It needs a writable
home for its state — `~/.local/share/verger` under XDG, or whatever `--home` names.

## Quick start

```bash
verger doctor                 # what needs attention on this machine
verger install owner/name     # install one package everywhere
verger status                 # the package × host matrix
verger why <id> <host>        # one cell: strategy, version, blockers
verger outdated               # what is behind
verger sync                   # make the machine match the spec
verger update                 # re-apply the spec, picking up newer versions
```

To keep it in the background — `launchd` on macOS, a `systemd` user unit on Linux:

```bash
verger service install
```

## Doctor

```
verger doctor            # what needs attention, and the command that fixes it
verger doctor --json     # the same findings, machine-readable
verger doctor --fix      # apply the fixes verger owns, after one confirmation
```

Every finding carries a subject, a message and — where one exists — a ready-to-run command. Plain
`doctor` only reads; it never writes. `--fix` applies only the fixes verger owns, and those are the
ones that change nothing you wrote; it prints the plan **once** and asks once, `-y` skipping the
question. Anything touching something you wrote, or needing your consent, is left for you to run.

## Packages

| command | what it does |
|---|---|
| `verger install <ref>...` | install packages by reference, everywhere |
| `verger remove <id>` | remove one package from every host, keeping it in the trash |
| `verger status` | show the package × host matrix |
| `verger info <id>` | show what one package is and where it is installed |
| `verger search <query>` | find packages by name, and say which are already installed |
| `verger why <id> <host>` | explain one cell: strategy, version, blockers |
| `verger outdated` | list skew and missing cells |
| `verger update [id]` | re-apply the spec, picking up newer versions |
| `verger sync` | make the machine match the spec |
| `verger adopt <ref>` | adopt a package a host installed natively |
| `verger enable <id>` | turn one package's delivery on in the spec |
| `verger disable <id>` | turn one package's delivery off in the spec |
| `verger pin <id>@<version>` | pin one package to a version |
| `verger unpin <id>` | drop the version pin |
| `verger restore <id>` | restore a removed package from the trash (≤30 days) |
| `verger gc` | purge trash entries older than the retention window |

`status --outdated-only` shows only skew and missing cells; `install` and `update` take
`--hooks ask|yes|no`; `install --pin` records a version; `gc --dry-run` shows what it would purge.
Without `--allow-downgrade`, a channel that resolves to an older version skips the package and
prints the command that would take it.

`search` never reaches the network. It answers from the spec, the lock, the receipts and the
offers of `local:` sources, and names every other source under `skipped` rather than passing over
it in silence — a list you can trust is a list that says what it could not read.

`install` and `remove` never overwrite a file you edited since it was delivered — they refuse and
tell you so. `--force` proceeds, and keeps what you wrote first: your version is copied under
`state/backups/<timestamp>/` and the path is printed, so an overwrite is recoverable, not
destructive. `-y` does not imply `--force`.

## Hooks and trust

| command | what it does |
|---|---|
| `verger approve` | approve the current hooks content hash, so hooks may run |
| `verger revoke` | withdraw that approval |
| `verger trust` | trust the project spec at its current content hash |
| `verger untrust` | forget the trust record of the project spec |

Hooks do not run until the hash is approved, and a project spec is ignored until it is trusted.
Both are refused loudly rather than skipped quietly.

## Sources

| command | what it does |
|---|---|
| `verger source add <name> <url>` | add a named source to the spec |
| `verger source rm <name>` | remove a source by name |
| `verger source list` | list the declared sources in priority order |
| `verger propagate show` | show the effective propagation policy |
| `verger propagate set <rule> <value>` | set one propagation rule (all, origin, ask) |

Sources are tried in the order the spec lists them, so a ref resolves against the first that offers
it. A package is fetched from a git ref, an npm spec, an archive with its sha256, or a local
directory. A channel (`stable`, `beta`, an npm dist-tag) picks the version when the lock is
written; a channel the source does not offer is refused with the list of the ones it does.

## Secrets

| command | what it does |
|---|---|
| `verger secret set <name>` | read a value from stdin and store it under that name |
| `verger secret rm <name>` | delete the stored value |
| `verger secret list` | list stored names, never values |
| `verger secret backend` | show the storage backend, or switch to the named one |

The backend is `auto`, `keyring` or `file`. Without a keychain, `auto` resolves to `file` and says
so, and asking for `keyring` exits 6. verger never opens a keychain to find out whether it has one.
The spec refers to a value by name, and `env = { TOKEN = "{secret:TOKEN}" }` is how a package
receives it.

## Watch

| command | what it does |
|---|---|
| `verger watch` | reconcile the scope whenever a host's files change |
| `verger service install` | install the unit that runs `verger watch` in the background |
| `verger service status` | report whether that unit is installed and loaded |
| `verger service uninstall` | remove it again |

One lease is shared with beadle, so a second watcher never competes: it says which watcher holds it
and by which pid, and exits 3. `watch` takes `--debounce` and `--owner`; run either tool's watcher,
not both.

## Authoring

| command | what it does |
|---|---|
| `verger lint <path>` | validate a package payload before publishing it |
| `verger pack <path>` | render a package into a directory the store can serve locally |

`lint` checks a payload against the format the hosts read; `pack` renders it so a local source
resolves without a remote one.

## Reference

Global flags:

```
--home string   override the verger home directory
--json          emit machine-readable JSON
-y, --yes       answer yes to the prompts this command asks
--log-json      structured JSON logs
--verbose       verbose output
-v, --version   print the verger version and exit
```

Write flags — what lands on disk:

```
--dry-run             plan without writing
--force               overwrite a file you edited; your version is kept and reported
--hosts a,b           deliver only to these hosts
--except a,b          deliver to every host except these
--project             use the project scope instead of the user scope
--hooks ask|yes|no    the hooks decision: ask (default), yes, no
--pin <version>       record a version in the spec
--locked              fail when the result would change the lock
--allow-downgrade     let a channel take a version older than the installed one
--outdated-only       show only skew and missing cells
```

Not every command takes every flag: `--hosts` narrows a delivery, `--hooks` belongs to the commands
that install hooks, `--allow-downgrade` to `update`. `--json` and `-y` are global: put them before
or after the verb.

Every document starts with the same envelope, so a consumer can read one and switch on the name:

```
{"schema":{"name":"verger.status","version":1}, "home":"…", "cells":[]}
```

`-y` never means "discard what you wrote". It answers the prompts a command asks and never resolves
a destructive conflict; overwriting a file you edited is always its own flag, `--force`.

### Exit codes

verger and beadle use the same classes, so a script can treat them alike.

| code | name | meaning |
|---|---|---|
| 0 | ok | success |
| 1 | unexpected | an error with no more specific class |
| 2 | usage | wrong flag, wrong argument, unknown command |
| 3 | conflict | a conflict only you can settle: a `sync` that ended with one open, or a watcher that already holds the lease |
| 4 | policy refusal | a managed-settings policy forbids this |
| 5 | consent needed | a package needs approval before it runs |
| 6 | host unavailable | the host cannot be reached or has no schema |
| 7 | schema newer | the spec was written by a newer version |

## verger and beadle

verger is the plugin manager — plugins, skills, MCP servers, agents, commands and hooks — and
beadle embeds it, so the two share one spec, one lock and one set of exit codes. Use verger when
you only want plugins, and beadle when you want an agent's behaviour managed along with them.
They are the same program underneath, which is what makes the two arrangements safe to combine:

- **One home.** `~/.beadle` and `~/.verger` are read by both. What beadle writes to an agent,
  standalone `verger` sees, and the package set each reports is the same set.
- **One watcher.** `beadle watch` and `verger watch` take the same lease. The second does not
  compete: it names the holder and its pid, and exits 3. Run one, not both.
- **Eject.** `beadle plugins eject` moves the plugin home out of the vault to `~/.verger`, keeping
  every installed package. From then on `verger status` and `verger sync` work on it directly, with
  everything else in `~/.beadle` untouched — and both can still be run on the same home, because
  after the eject there is one plugin home, not two.
- **Absorb.** The other direction needs no command: `beadle init` moves a standalone `~/.verger`
  into the vault, file for file, and the old directory is gone afterwards. `beadle plugins eject`
  sends it back out.

## Principles

- **Real files, never symlinks.** A delivered package is a directory of files the agent reads on
  its own; there is no indirection to lose, and no store to keep in sync with what is on disk.
- **Your edits are yours.** A file that changed after verger delivered it is never overwritten. The
  run refuses and exits 3, and `--force` copies your version into `state/backups/` before it
  replaces anything.
- **One spec, one lock.** The spec says what you want; the lock says which bytes that resolved to.
  Nothing is fetched from a name the spec did not declare.
- **Silence has a reason.** When verger delivers nothing for a cell, the reason and the hint are
  part of the answer — a refusal that cannot be acted on is not a useful one.

## License

MIT.
