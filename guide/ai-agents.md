# verger, for AI agents

What an agent may rely on when it touches plugins through verger, and what it must not assume.

![How verger delivers a package](../assets/guide-architecture.svg)

## The short version

- verger writes **real files** into the host's own layout. You can read them, edit them, and hand
  them to a human.
- verger never overwrites a file you edited. It refuses and exits 3.
- Every command answers `--json`, and every document starts with the same envelope.
- The exit code tells you the class of the failure before you read a word of the message.

## Always use `--dry-run` first

`--dry-run` plans and writes nothing. It is the cheapest way to find out what a command would do,
and it costs no state.

```
verger install owner/name --dry-run --json
```

Read the `cells` in the answer before you run the real thing. A plan that surprises you is cheaper
than a delivery that does.

## Read the exit code, not the prose

| code | name | what you should do |
|---|---|---|
| 0 | ok | nothing |
| 1 | unexpected | report it; there is no more specific class to act on |
| 2 | usage | you passed something wrong; the message names the flag or the command |
| 3 | conflict | only a human can settle it. Report the file and stop. |
| 4 | policy refusal | a managed policy forbids this. Do not try another way. |
| 5 | consent needed | a package needs approval before it runs. Ask. |
| 6 | host unavailable | the host is not there or has no schema. Report; do not substitute. |
| 7 | schema newer | the spec is from a newer verger. Do not rewrite it. |

Exit 3 and exit 5 are the two an agent must **not** resolve on its own. Both mean a human owns
the next step, and both have a flag that would step over that: `--force` for the conflict,
`--hooks yes` for the consent. Neither is implied by `-y`.

## The JSON envelope

```
{"schema":{"name":"verger.status","version":1}, "home":"…", "cells":[]}
```

Read the `schema.name` and switch on it. A consumer that reads one document and switches on the
name will keep working when a command grows a field; one that assumes a fixed key order will not.

`--json` is global — before or after the verb, it is the same command.

## Never assume you can force your way through

`--force` overwrites a file a human edited, after copying their version to
`state/backups/<timestamp>/`. It is the user's work, and using it to clear an obstacle is how an
agent destroys a change nobody asked it to touch.

`-y` is not a softer `--force`. It answers the prompts a command asks. It never resolves a
destructive conflict, and no command implies `--force` from it.

If a command exits 3, the correct behaviour is to report the path and stop.

## What verger guarantees about a file it delivered

- it is a regular file, not a symlink into a store you cannot read
- its bytes and its mode are what the receipt records
- if a human changed it, verger leaves it alone and says so
- if it is gone, verger reinstalls it; a missing file is not a conflict

## Changing a package

A package is delivered from a spec entry. To change one, change the spec — the CLI edits the same
file you read, so there is no second source of truth.

```
verger pin owner/name 1.2.3     # record a version
verger unpin owner/name         # drop it, follow the channel again
verger disable owner/name       # stop delivering it, keep the entry
verger enable owner/name        # deliver it again
```

Prefer these over editing `verger.toml` by hand when you are scripting, so the change is written
the same way every time.

## Watching

`verger watch` reconciles when a host's files change. Do not start one next to a beadle watcher:
there is one lease, and the second process exits 3 naming the holder. That is not an error to
retry — it is the answer to "who is already watching".

## Secrets

Values are read from stdin and never printed. If you need a value, ask for it by name and let
verger place it; do not read it and write it into a command line, where it lands in shell history.

```
verger secret set TOKEN          # the value is read from stdin
verger secret list                # names only
```

`verger never opens a keychain to find out whether it has one. With no keychain, `auto` resolves
to the `file` backend and says so, and asking for `keyring` exits 6.

## If something is wrong

`verger doctor` is the first call. It only reads, it reports each finding with the command that
fixes it, and `--json` makes it machine-readable. Use `--fix` only for the findings it says it
owns — the ones that change nothing a human wrote.
