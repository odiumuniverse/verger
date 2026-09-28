<div align="center">

  <h1>verger</h1>

  <p><b>One manifest for every coding agent.</b><br>
  Each host gets what it can actually take — and what it cannot says why.</p>

  <p>
    <a href="https://github.com/odiumuniverse/verger/releases"><img alt="release" src="https://img.shields.io/github/v/release/odiumuniverse/verger?style=flat-square&color=3E6E5C&labelColor=1F2328"></a>
    <a href="https://github.com/odiumuniverse/verger/actions/workflows/ci.yml"><img alt="ci" src="https://img.shields.io/github/actions/workflow/status/odiumuniverse/verger/ci.yml?branch=master&style=flat-square&color=3E6E5C&labelColor=1F2328"></a>
    <img alt="go" src="https://img.shields.io/badge/go-1.27-3E6E5C?style=flat-square&color=3E6E5C&labelColor=1F2328">
    <img alt="license" src="https://img.shields.io/badge/license-MIT-3E6E5C?style=flat-square&color=3E6E5C&labelColor=1F2328">
  </p>

</div>

[Why](#why) · [Install](#install) · [Quick start](#quick-start) · [Sources](#sources) ·
[Supported hosts](#supported-hosts) · [Commands](#commands) · [How a package reaches a host](#how-a-package-reaches-a-host) ·
[Safety](#safety) · [With beadle](#with-beadle)

## Why

Every coding agent keeps its own plugins, skills, MCP servers, subagents,
commands and hooks — in its own directory, in its own dialect. Install the same
package into three agents and you maintain three copies by hand until they
drift, and nothing tells you which agent is missing what.

verger does that job once. You describe the packages you want in
**`verger.toml`**, the exact versions land in **`verger.lock`**, and
`verger install` delivers each package to every host it has an adapter for,
through the first way that host actually supports. What landed on disk is
recorded, so `verger status`, `verger why` and `verger remove` answer from
evidence instead of guessing.

The table of hosts is the fact, not a promise: a host with no adapter is refused
by name instead of being quietly skipped.

## Install

**macOS** — Homebrew:

```bash
brew install odiumuniverse/tap/verger
```

**macOS or Linux** — Go, from a checkout or anywhere:

```bash
go install github.com/odiumuniverse/verger/cmd/verger@latest
```

**Linux** — build from source:

```bash
git clone https://github.com/odiumuniverse/verger
cd verger
go build -o verger ./cmd/verger
```

The module is `github.com/odiumuniverse/verger` and the binary is `verger`. It
needs a writable home (`~/.verger`) and, for the hosts that register plugins
themselves, the host's own CLI on `PATH`.

## Quick start

A package is a directory carrying at least one agent's manifest. A minimal
Claude Code plugin:

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
PACKAGE                  HOST     SCOPE    STATUS     STRATEGY   VERSION
local:caveman            claude   user     current    loose      1.2.3
local:caveman            omp      user     current    loose      1.2.3
  …one row per host that takes the package
```

`verger install` records the package in the manifest and then reconciles. The
file it wrote:

```toml
# ~/.verger/verger.toml — verger writes it, and hand-editing is fine
schema = 1

[[package]]
id = 'local:caveman'

[[source]]
name = 'my-plugin'
url = './my-plugin'
```

Then ask what is on the machine, and take it back off:

```bash
verger status                     # the package × host matrix
verger status --json              # same data, stable field names
verger why local:caveman claude   # why this cell is current, silenced or missing
verger remove local:caveman       # reverse every recorded operation
verger restore local:caveman      # bring it back from the trash
```

`verger sync` makes the machine match the manifest — it installs what the
manifest gained and removes what it lost — without resolving anything new.
`verger doctor` checks the machine, the stores, the keyring and every host.

## Sources

`verger source add` registers where packages come from; order is priority, first
match wins. A package reference is one of:

| Reference | Example |
|---|---|
| a local directory | `./my-plugin` |
| an archive pinned by hash | `https://host/pkg.tar.gz#sha256=…` (a pin is required) |
| a GitHub repository or subdirectory | `owner/repo`, `owner/repo//skills/foo` |
| git and npm | `git+https://…`, `npm:@scope/pkg@1.2.3` |
| a package already in an agent | `claude:caveman` (see `verger adopt`) |

Only a local directory and a hash-pinned archive install end to end. The other
forms are fetched — a GitHub clone, an npm tarball — but a delivery cannot be
completed from them, so `verger install` stops and names the cell it could not
plan. Nothing is written in that case:

```bash
verger install ./my-plugin                   # a directory on disk
verger install ./pkg.tar.gz#sha256=<digest>  # an archive, hash required
```

verger keeps no registry of its own: a package is read straight from the source
you name, and `verger source add` records several with the first match winning.

To freeze one version instead of following the source:

```bash
verger pin local:caveman@1.2.3
verger unpin local:caveman
```

## Supported hosts

| Host | What lands there |
|---|---|
| **Claude Code** | plugin registered through a Claude marketplace, or through a local marketplace verger generates; otherwise `~/.claude/skills`, `~/.claude/agents`, `~/.claude/commands`, hooks in `settings.json`, MCP servers added with `claude mcp add` |
| **omp** (oh-my-pi) | plugin via `omp plugin marketplace add` + `omp plugin install`; otherwise `~/.agents/skills`, and `<agentDir>/{agents,commands,rules}` with MCP servers in `<agentDir>/mcp.json`; hook modules under `<agentDir>/hooks/{pre,post}/` |
| **Codex CLI** | plugin from a marketplace; otherwise `~/.agents/skills`, `~/.codex/agents/*.toml`, hooks in `~/.codex/hooks.json`, MCP servers in `~/.codex/config.toml` |
| **Gemini CLI** | extension via `gemini extensions install`, or `gemini extensions link` for a rendered one; otherwise `~/.gemini/skills`, `~/.gemini/agents`, `~/.gemini/commands/*.toml`, MCP servers and hooks in `settings.json` |
| **Cursor** | `~/.cursor/skills`, `~/.cursor/agents`, `~/.cursor/commands`, hooks in `~/.cursor/hooks.json`, MCP servers in `~/.cursor/mcp.json`. A rule travels as a skill, because Cursor's own rules live in the project |
| **OpenCode** | `skills/`, `agents/`, `commands/` and MCP servers in `opencode.jsonc` (or `opencode.json`), under the config root: `OPENCODE_CONFIG_DIR` when you set it, else `$XDG_CONFIG_HOME/opencode`, else `~/.config/opencode`. Hooks are not written: OpenCode hooks are plugin modules, not files |
| **Kilo Code** | `skills/`, `agents/`, `commands/` and MCP servers in `kilo.jsonc` (or `kilo.json`), under the config root: `KILO_CONFIG_DIR` when you set it, else `$XDG_CONFIG_HOME/kilo`, else `~/.config/kilo`. Hooks are not written: a Kilo hook is a module in the plugin list, not a file |
| **Antigravity CLI** | marked `experimental` in `verger status`. Plugin through `agy plugin install`, taken from the first of `~/.gemini/antigravity-cli/plugins`, `~/.gemini/config/plugins` and `~/.agents/plugins` that carries it; otherwise `~/.agents/skills` and `~/.gemini/config/agents`, MCP servers in `~/.gemini/config/mcp_config.json`. Hooks are not written: Antigravity's `hooks.json` is keyed by top-level plugin name with no hooks wrapper, a shape verger does not render |
| **Pi** | plugin through `pi install <dir>`; otherwise `skills` and slash commands in `prompts` under the agent dir, which follows `PI_CODING_AGENT_DIR` and is otherwise `~/.pi/agent`. MCP servers go in `mcp.json` next to them — **pi has no MCP of its own**, so only the third-party `pi-mcp-adapter` (`pi install npm:pi-mcp-adapter`) reads that file, and the delivery says so on the cell. No subagents, no hooks |
| **DeepSeek Harness** | delivered loose, never through an installer: the harness has no package registry, and its `plugin` command is a pnpm passthrough into the profile rather than an install verger can drive. Skills in `skills` and MCP servers in the home patch layer `cordis.patch.yml`, under `DSH_HOME` when you set it and otherwise `~/.dsh`. Subagents, permissions and slash commands are runtime state and code there rather than files, so they are not written |

A host you name with `--hosts` and do not get is always told apart from a host
you have: the first says that id is not a host verger knows, the second says the
adapter is there but nothing on this machine looks like that agent. All ten are
delivered to in the order the table lists above, and `verger status` prints the
result for every one of them after every run — including the ones whose adapter
is still marked `experimental`.

## Commands

| Command | What it does |
|---|---|
| `verger install <ref>` | fetch, plan, ask, apply — then record in the manifest |
| `verger sync` | make the machine match the manifest; resolves nothing new |
| `verger remove <id>` | reverse every recorded operation for that package |
| `verger restore <id>` | bring a removed package back from the trash |
| `verger status` | the package × host matrix |
| `verger outdated` | only the cells that are skewed or missing |
| `verger why <id> <host>` | why one cell is current, silenced or missing |
| `verger adopt <host:ref>` | take over a package an agent already has, and deliver it to the others |
| `verger source add\|rm\|list` | where packages come from, in priority order |
| `verger pin` / `unpin` | freeze or release a version in the manifest |
| `verger update` | re-apply what the manifest asks for |
| `verger trust` / `untrust` | record that you trust a project manifest at its current content |
| `verger approve` / `revoke` | grant or withdraw hook consent for a package |
| `verger secret set\|rm\|list` | store values for packages that need them; values are never printed |
| `verger propagate show\|set` | how a component reaches the other hosts |
| `verger pack <path>` | render a package into the store |
| `verger lint` | check a package payload before you publish it |
| `verger doctor` | check the machine, the stores, the keyring and every host |
| `verger version` | print the version |

Every write command takes `-y` to accept the defaults and `--dry-run` to plan
without writing. `-y` never resolves a destructive conflict: it declines them.
`--json` gives machine-readable output on any command. `--project` works on the
commands that can target a project manifest instead of your user one.

## How a package reaches a host

verger tries the ways a host can take a package, in order, and uses the first
one that works: the host's own installer, taken from the host's own marketplace;
a package verger renders into the store and the host installs from a local path;
or the individual skills, agents, commands, rules and MCP servers written into
the host's own user directories and configuration files. When a host cannot take
something, that cell is kept as *silenced* with the reason printed next to it —
never dropped, never faked. `verger why` shows the reason and the hint for any
cell. Removal is the mirror image: every delivery records how to undo it, so
`verger remove` takes the package back off every host it went to and keeps the
replaced files in the trash.

## Safety

- **Host-owned state is never written.** Files a host CLI owns outright are left
  exactly as they were — Gemini's signed extension integrity store, omp's own
  logs and prebuilt natives. Removal goes through the host's own uninstall.
- **Your instructions are not rewritten.** A rule is delivered as a skill rather
  than merged into your `AGENTS.md` or `GEMINI.md`, and the delivery says so.
- **The host's policy outranks verger.** What a host's managed settings forbid —
  marketplace allow/deny lists, admin-controlled plugins, enterprise policy — is
  not delivered by any route. The cell is reported as blocked by policy, with the
  rule that blocked it. An unreadable policy document is treated as a refusal.
- **A package you installed by hand is adopted, not duplicated.** `verger adopt`
  takes a package an agent already knows and delivers it to the others. Adopt
  never installs hooks.
- **Hooks need consent.** `verger install` asks before installing hooks, and the
  answer is remembered against the hooks' content — changed hooks ask again.
  `verger approve` and `verger revoke` manage that consent.
- **omp hooks are code, and omp does not sandbox them.** They are TS/JS modules
  under `<agentDir>/hooks/{pre,post}/` and run with your full rights.
  **Restart omp after a delivery** — a live session keeps the modules it loaded
  at startup, and reloading plugins does not reload them.
- **A project manifest must be trusted.** The first run against a project
  `verger.toml` asks you to trust it by content hash (`verger trust`); a changed
  file asks again. There is no way to skip that question.
- **Secrets stay out of the manifest.** `verger secret` stores values for
  packages that need them, and a value never reaches `verger.toml`,
  `verger.lock` or the logs. A secret referenced from a hook command is refused
  outright, because that command lands in a settings file and its arguments are
  visible to every process on the machine.
- **Removal is guarded.** Only a targeted disappearance, with the host alive, its
  registry parsed and a matching record, counts as a removal — not an orphan
  sweep, not a rename, not a host reset. Replaced files go to the trash, not to
  the bin.

## With beadle

[beadle](https://github.com/odiumuniverse/beadle) manages your rules, skills,
MCP servers, subagents, commands and permissions across the same agents, and
embeds verger for plugin management. Install plugins with either tool, or both:
beadle delivers plugins through verger, so a package installed from beadle and a
package installed from your shell land the same way, in the same place, and
`verger status` sees both. Switching between them changes nothing on disk —
neither tool rewrites what the other wrote, and the two of them serialise on
the one file they share.

MIT — see [LICENSE](LICENSE).
