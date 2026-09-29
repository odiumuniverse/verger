# verger

verger installs and removes one set of **plugins** — skills, MCP servers, agents, commands and
hooks — across every supported coding agent, and then keeps every machine matching that set.

You describe what you want in a spec. verger works out how each agent wants to receive it, writes
it there, and records what it did so the next run is a no-op.

## Install

```
brew install odi/universe/verger
```

## First run

Run `verger` with nothing else. On a machine that has no verger home yet it shows which coding
agents it found, and asks once before creating anything:

```
$ verger
  No ~/.verger yet. Here is what I found.

  AGENT              STATE     EVIDENCE
  claude             found     ~/.claude
  codex              found     ~/.codex
  ...
  agy                not found not installed
```

`-y` creates the home without asking, which is what you want in a script. Once a home exists, a
bare `verger` just prints the help screen.

## Commands

| command | what it does |
|---|---|
| `verger install <spec…>` | install packages everywhere |
| `verger remove <id>` | remove a package everywhere |
| `verger sync` | make the machine match the spec |
| `verger update [id]` | re-apply the spec's packages |
| `verger status` | show the package × host matrix |
| `verger outdated` | list version skew and missing cells |
| `verger why <id> <host>` | explain one cell: strategy, version, blockers |
| `verger enable <id>` | turn one package's delivery on in the spec |
| `verger disable <id>` | turn one package's delivery off in the spec |
| `verger adopt <host:ref>` | adopt a package the host installed natively |
| `verger pin <id>@<version>` | pin a package to a version in the spec |
| `verger unpin <id>` | drop the version pin |
| `verger restore <id>` | restore a removed package from the trash (≤30 days) |
| `verger gc` | purge trash entries older than the retention window |
| `verger approve <id> [ref]` | approve a package's hooks |
| `verger revoke <id>` | revoke a package's hooks approval |
| `verger source` | manage spec sources (order is priority) |
| `verger propagate` | show or set the propagation policy |
| `verger secret` | manage secrets (values are never printed) |
| `verger pack <path>` | render a package into a store directory |
| `verger lint <path>` | validate a package payload |
| `verger trust [path]` | trust the project spec at its current content hash |
| `verger untrust [path]` | forget the trust record of the project spec |
| `verger doctor` | check the machine, stores, keyring and hosts |
| `verger watch` | reconcile the scope whenever a host's files change |
| `verger service` | install, remove and inspect the background watch service |
| `verger completion <shell>` | print the completion script for bash, zsh, fish or powershell |
| `verger version` | print the verger version |

Subcommands:

```
verger source add|rm|list          spec sources, in priority order
verger propagate show|set          the propagation policy
verger secret set|rm|list|backend  secrets; set reads the value from stdin
verger service install|uninstall|status   the background watch unit
```

## Your edits

If a file verger wrote has been changed by hand, verger will not overwrite it. The cell is reported
as `hands-off` and the file is left exactly as you left it:

```
$ verger sync
verger: your edit left alone in 1 cell(s): local:caveman@claude; rerun with --force to
overwrite, keeping your copy
```

That exits 3, "conflict" — a refusal that leaves work undone is not a success, so a script can
tell "synced" from "declined". Nothing is written, and verger keeps its record of the file, so a
later `--force` can still do its job.

When you want your version replaced, `--force` says so explicitly:

```
verger sync --force
```

The previous version is kept, not discarded, and the report names the path it was written to, so
you can get it back. `--force` is never implied by `-y`: accepting defaults does not authorise
destroying someone's edit. If the backup itself cannot be written, verger says so instead of
overwriting anyway.

`--force` exists on `install`, `remove`, `sync`, `update` and `adopt`.

## Restored from lock

A cell that arrives from the lock with no receipt of its own — because the lock was written
elsewhere, or by a run that did not record a receipt — is reported as `restored` rather than
`delivered`. Nothing was installed from a fetch on this machine, and the report says so.

## Secrets

Package manifests reference secrets by name; the values live in one place, in your platform's
keychain:

```
verger secret set token      # reads the value from stdin, never echoes it
verger secret list           # names only — values are never printed
verger secret rm token
```

verger picks the backend for you and tells you which one it chose:

```
$ verger secret backend
keyring
a keychain is available on this machine
```

`auto` — the default — uses the keychain when a keychain is there and the file backend when one is
not. You can pin it either way:

```
verger secret backend keyring
verger secret backend file
verger secret backend auto
```

Switching carries every value across. Nothing is printed and nothing is dropped.

**On a machine with no keychain — a headless Linux box, a container, CI — use `file`.** There is
no secret service to talk to, and verger will tell you rather than fall back silently:

```
$ verger secret backend keyring
verger: no keychain available here — use: verger secret backend file
```

That exits 6, "host unavailable", which is what it is: a fact about the machine, not a mistake in
the command. The `file` backend keeps values in a document under the verger home, so treat that file
as the secret it is — it is the one place your values are readable in the clear.

verger never opens a keychain to *ask* whether one is there. The check reads the filesystem and the
environment, because on some systems opening a keychain to look can raise a system dialog.

On macOS the same check fires under a HOME that is not your account's home — a test run, a CI
runner, a throwaway directory. The keychain the Security framework would open is not yours, so
verger names that rather than pretending a keychain is there.

## Watching for changes

`verger watch` reconciles the scope whenever a host's files change:

```
verger watch
verger watch --project --hosts claude,codex
verger watch --debounce 5s
```

`--owner` names the lease (default `verger`) so two watchers on one machine do not fight. The
name is a closed set — `verger` or `beadle` — so a lease file never ends up naming something
nobody recognises. A second watcher on the same home is turned away, naming the holder.

To have it run for you:

```
verger service install     # install the unit that runs `verger watch`
verger service status      # is it installed and loaded?
verger service uninstall   # remove the unit
```

`--label` renames the unit (default `dev.odiumuniverse.verger.watch`).

A login service outlives the shell that started it, so `service install` refuses a temporary home
— anything under `/tmp`, `/private/tmp` or the per-boot directory macOS hands out — rather than
pinning a unit to a tree that will be gone:

```
$ verger service install
verger: refusing to install a login service for a temporary home (/tmp/whatever): it would
outlive the tree it was built in; use a real home directory
```

That exits 2 and writes nothing. The check follows symlinks, so a link into a temporary tree is
refused the same way a direct path is.

## Doctor

```
verger doctor            # what needs attention, and the command that fixes it
verger doctor --json     # the same findings, machine-readable
```

Every finding carries a stable subject, a message you can read, and — where one exists — a
ready-to-run command. Plain `doctor` only reads; it never writes.

`verger doctor --fix` applies the fixes verger owns outright — creating a directory, re-registering
the service, nothing you wrote — after a single confirmation. `-y` skips that question for
scripts. Anything that would touch your files, or that needs your consent, is printed with its
command instead of applied.

## Shell completion

```
verger completion zsh > "$(brew --prefix)/share/zsh-completion/_verger"
```

`bash`, `fish` and `powershell` work the same way. The generated script asks the binary for its
commands at completion time, so it never lists a command that does not exist.

## Man pages

```
man $(brew --prefix)/share/man/man1/verger.1
```

The pages are generated from the same command tree the binary uses, so they cannot drift from it.

## Flags

Every command accepts these:

```
--home string   override the verger home directory
--json          emit machine-readable JSON
--log-json      structured JSON logs
--verbose       verbose output
```

`verger version`, `verger --version` and `verger -v` all print the same one line.

`install`, `remove`, `sync`, `update` and `adopt` also take:

```
--dry-run       plan without writing
--force         overwrite files you edited; the previous version is kept and reported
--hosts strings only these hosts (comma separated)
--except strings   exclude these hosts
--project       use the project scope
-y, --yes       accept defaults; never resolve destructive conflicts
```

`--yes` accepts defaults. It never resolves a destructive conflict for you — a conflict that
destroys hand-edited files still asks, and still needs `--force`.

`--dry-run` is on every command that writes. `pin`, `unpin`, `enable`, `disable`, `approve`,
`revoke`, `pack`, `source` and `propagate` take no `-y`, because nothing in them asks.

`verger install` additionally takes `--hooks ask|yes|no` and `--pin <version>`.

`verger doctor` takes `--fix` and `-y`. `verger watch` takes `--debounce`, `--hosts`,
`--except`, `--project` and `--owner`.

`verger why <id> <host>` needs both halves of the cell; `verger status` shows which host a package
is installed for.

## Exit codes

| code | name | meaning |
|---|---|---|
| 0 | ok | success |
| 1 | unexpected | an error with no more specific class |
| 2 | usage | wrong flag, wrong argument, unknown command |
| 3 | conflict | a destructive conflict you must settle |
| 4 | policy refusal | a managed-settings policy forbids this |
| 5 | consent needed | a package needs approval before it runs |
| 6 | host unavailable | the host cannot be reached or has no schema |
| 7 | schema newer | the spec was written by a newer version |

## beadle and verger

beadle is a coding agent's own configuration — rules, memory, skills, subagents, inbox, vault — and
it uses verger as its plugin manager, so the two share one spec, one lock and one set of exit
codes. Use beadle when you want an agent's behaviour managed along with its plugins, and verger on
its own when you only want plugins. `beadle plugins eject` moves the plugin home out of the vault
to `~/.verger`, and from then on `verger status`, `verger sync` and the rest work on it directly,
with everything else in `~/.beadle` untouched.
