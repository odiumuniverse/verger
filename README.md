# verger

Install once, everywhere: verger delivers agent packages (plugins, skills, MCP
servers, subagents, commands, hooks, rules, host-native modules) to the coding
agents it has adapters for. The table below is the fact, not a promise: an id
without an adapter is accepted by `--hosts` and refused with
`no adapter in this build` instead of pretending the host is merely undetected.

| Host | Adapter | Live e2e |
|---|---|---|
| Claude Code | native/synth/loose | green on claude 2.1.283 |
| omp | native/synth/loose | green on omp 18.4.1 |
| Codex | native/synth/loose | e2e leg exists; the last local run on this machine had no codex binary, so only unit tests and captured CLI output pin it |
| Gemini CLI | native/synth/loose | e2e leg exists; same — unit tests and captured CLI output only |
| Antigravity, Cursor, OpenCode, Kilo, Pi, DeepSeek Harness | not implemented (Ф2, `docs/TASKS.md` T2.2) | — |

- Desired state in `verger.toml`, exact versions and hashes in `verger.lock`,
  what actually landed on disk in receipts.
- Delivery per host is a pure function of the host's capabilities:
  `native → synth → loose → silenced(reason)`.
- A package installed by hand in an agent is adopted with `verger adopt
  <host:ref>`; the automatic spread to the other hosts (`verger watch`,
  guarded auto-remove) arrives in Ф2 — `watch` is a stub that says so today.
- No registry of its own: packages resolve straight from their source —
  `owner/repo`, `plugin@owner/repo`, a git/npm/MCP ref or a link to a skills or
  marketplace site — and install natively through each agent's own CLI grammar
  wherever the repo carries that agent's format.

## Status

Ф0 done; Ф1 (Claude/Codex/Gemini adapters, apply, CLI, e2e) — in review. The
`omp` adapter landed after Ф1 and its e2e leg is green on omp 18.4.1. Ф2 — the
remaining six adapters, `pkg/caps`/`pkg/plan`, watcher, TUI — has not started
(`docs/TASKS.md`), and the hosts it covers are not claimed above.

## Development

```
make fmt lint test    # what CI runs
make build            # bin/verger
make mod              # go mod tidy + vendor
```
