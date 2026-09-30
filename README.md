# verger

verger installs and removes one set of packages — skills, MCP servers, agents, commands and hooks
— across every coding agent you use, and keeps every machine matching that set.

You describe what you want in a spec; verger works out how each agent wants to receive it, writes
it there, and records what it did so the next run is a no-op.

Only need plugins? You are in the right place. Want every agent's whole configuration — rules,
memory, skills, subagents, inbox — kept in sync across machines too?
→ [beadle](https://github.com/odiumuniverse/beadle), which uses verger as its plugin manager.

## Install

```
brew install odiumuniverse/tap/verger
```

The formula installs shell completion for you, and ships a man page:

```
verger completion zsh > "$(brew --prefix)/share/zsh-completion/_verger"
man "$(brew --prefix)/share/man/man1/verger.1"
```

## Guides

Run `verger` with nothing else. On a machine with no verger home yet it shows which agents it
found and asks once before creating anything. Once a home exists, a bare `verger` prints help.

### Commands

| command | what it does |
|---|---|
| `verger install <ref>...` | install packages everywhere |
| `verger remove <id>` | remove a package everywhere |
| `verger sync` | make the machine match the spec |
| `verger update [id]` | re-apply the spec's packages |
| `verger status` | show the package × host matrix |
| `verger outdated` | list version skew and missing cells |
| `verger info <id>` | show what one package is and where it is installed |
| `verger why <id> <host>` | explain one cell: strategy, version, blockers |
| `verger adopt <host:ref>` | adopt a package a host installed natively |
| `verger enable <id>` | turn one package's delivery on in the spec |
| `verger disable <id>` | turn one package's delivery off in the spec |
| `verger pin <id>@<version>` | pin a package to a version |
| `verger unpin <id>` | drop the version pin |
| `verger restore <id>` | restore a removed package from the trash (≤30 days) |
| `verger gc` | purge trash entries older than the retention window |
| `verger approve <id>` | approve a package's hooks |
| `verger revoke <id>` | revoke that approval |
| `verger trust [path]` | trust the project spec at its current content hash |
| `verger untrust [path]` | forget that trust record |
| `verger source` | manage spec sources, in priority order |
| `verger propagate` | show or set the propagation policy |
| `verger secret` | manage secrets; values are never printed |
| `verger watch` | reconcile the scope whenever an agent's files change |
| `verger service` | install, remove and inspect the background watch service |
| `verger doctor` | check the machine, stores, keyring and hosts |
| `verger lint <path>` | validate a package payload, for package authors |
| `verger pack <path>` | render a package into a store directory |
| `verger completion <shell>` | print the completion script for bash, zsh, fish or powershell |
| `verger version` | print the version — `--version` and `-v` do the same |

Subcommands:

```
verger source add|rm|list          spec sources, in priority order
verger propagate show|set          the propagation policy
verger secret set|rm|list|backend  secrets; set reads the value from stdin
verger service install|uninstall|status   the background watch unit
```

### Your edits

A file you changed by hand is yours. verger leaves it exactly as you left it, and says so instead
of reporting success:

```
$ verger sync
verger: your edit left alone in 1 cell(s): local:caveman@claude; rerun with --force to
overwrite, keeping your copy
```

That exits 3, "conflict", so a script can tell "synced" from "declined".

`--force` overwrites on purpose and keeps what you wrote first: your version is copied under
`state/backups/<timestamp>/` and the report prints where it went, so an overwrite is
recoverable rather than destructive. If the backup cannot be written, verger says so instead of
overwriting anyway.

`-y` does not imply `--force`. Accepting defaults never authorises destroying someone's edit, so
a conflict that destroys hand-edited files still asks, and still needs `--force`.

### Secrets

Package manifests reference secrets by name; the values live in one place. verd ger picks the
backend and tells you which:

```
$ verger secret backend
keyring
a keychain is available on this machine
```

`auto` — the default — uses the keychain when there is one and the file backend when there is
not. You can pin it either way, and switching carries every value across:

```
verger secret backend keyring
verger secret backend file
verger secret backend auto
```

**On a machine with no keychain — a headless Linux box, a container, CI — use `file`.** verger
says so rather than falling back silently, and that exits 6, "host unavailable": a fact about
the machine, not a mistake in the command. The `file` backend keeps values in a document under
the verger home, so treat that file as the secret it is.

verger never opens a keychain to *ask* whether one is there. The check reads the filesystem and
the environment, because on some systems opening a keychain to look can raise a system dialog.

### Watching for changes

```
verger watch
verger watch --project --hosts claude,codex
verger watch --debounce 5s
```

Package manifests reference secrets by name; the values live in one place. verger picks the
set — `verger` or `beadle` — so a lease file never names something nobody recognises, and a second
watcher on the same home is turned away naming the holder.

To have it run for you:

```
verger service install     # install the unit that runs `verger watch`
verger service status      # is it installed and loaded?
verger service uninstall   # remove the unit
```

`--label` renames the unit. A login service outlives the shell that started it, so
`service install` refuses a temporary home — anything under `/tmp` or the per-boot directory
macOS hands out — and exits 2 rather than pinning a unit to a tree that will be gone.

### Doctor

```
verger doctor            # what needs attention, and the command that fixes it
verger doctor --json     # the same findings, machine-readable
verger doctor --fix      # apply the fixes verger owns, after one confirmation
```

Every finding carries a subject, a message, and — where one exists — a ready-to-run command.
Plain `doctor` only reads; it never writes. `--fix` applies the repairs that change nothing you
wrote, asks once for the whole set, and stops at the first failure. `-y` skips the question.

### Machine-readable output

`--json` works on the read-only commands and prints a versioned envelope, so a consumer never
parses prose:

```
$ verger status --json
{"schema":{"name":"verger.status","version":1},"home":"…/.verger","cells":[]}
```

### Flags

Every command accepts these:

```
--home string   override the verger home directory
--json          emit machine-readable JSON
--log-json      structured JSON logs
--verbose       verbose output
-v, --version   print the version and exit
```

`install`, `remove`, `sync`, `update` and `adopt` also take:

```
--dry-run       plan without writing
--force         overwrite files you edited; the previous version is kept and reported
--hosts strings only these hosts (comma separated)
--except strings   exclude these hosts
--project       use the project scope
-y, --yes       accept defaults; never resolves a destructive conflict
```

`--dry-run` is on every command that writes. `verger install` also takes `--hooks ask|yes|no`
and `--pin <version>`; `verger sync` also takes `--locked`; `verger watch` takes `--debounce`,
`--owner`, `--hosts`, `--except` and `--project`; `verger doctor` takes `--fix`; `verger service`
takes `--label`; `verger status` takes `--outdated-only`.

## Exit codes

The same numbers on every command, so a script can branch on the situation rather than the
message.

| code | meaning |
|---|---|
| 0 | success |
| 1 | an error with no more specific class |
| 2 | usage: a wrong flag, a wrong argument, an unknown command |
| 3 | conflict: a destructive conflict you must settle |
| 4 | a managed-settings policy forbids this |
| 5 | consent needed: a package needs approval before it runs |
| 6 | host unavailable: the agent cannot be reached |
| 7 | the spec was written by a newer version |

## verger and beadle

beadle is a coding agent's own configuration — rules, memory, skills, subagents, inbox, vault —
and it uses verger as its plugin manager, so the two share one spec, one lock, one set of exit
codes and one secret store. Use beadle when you want an agent's behaviour managed along with its
plugins, and verger on its own when you only want plugins.
They work side by side rather than instead of each other: beadle drives verger over a shared home
at `~/.beadle/verger`, and the two share the single watch lease, so there is still exactly one
watcher per machine. `beadle plugins eject` moves the plugin home out of the vault to `~/.verger`,
and from then on `verger status`, `verger sync` and the rest work on it directly, with everything
else in `~/.beadle` untouched.
