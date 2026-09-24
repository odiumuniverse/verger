# verger

Install once, everywhere: verger delivers agent packages (plugins, skills, MCP
servers, subagents, commands, hooks, rules, host-native modules) to every
supported coding agent — Claude Code, Codex, Gemini CLI, Antigravity, Cursor,
OpenCode, Kilo, Pi, DeepSeek Harness.

- Desired state in `verger.toml`, exact versions and hashes in `verger.lock`,
  what actually landed on disk in receipts.
- Delivery per host is a pure function of the host's capabilities:
  `native → synth → loose → silenced(reason)`.
- Manual installs in any agent are adopted automatically; manual removals
  propagate with guardrails; the newest version wins.
- The public registry (`getverger/registry`) is data only — a signed, sparse
  snapshot built by a nightly crawler. No server.

The design lives in [docs/DESIGN.md](docs/DESIGN.md); the work breakdown is in
[docs/TASKS.md](docs/TASKS.md).

## Status

Ф0 (repository skeleton: home, spec/lock, receipts, lease) — in progress.

## Development

```
make fmt lint test    # what CI runs
make build            # bin/verger
make mod              # go mod tidy + vendor
```
