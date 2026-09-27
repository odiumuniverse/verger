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
- No registry of its own: packages resolve straight from their source —
  `owner/repo`, `plugin@owner/repo`, a git/npm/MCP ref or a link to a skills or
  marketplace site — and install natively through each agent's own CLI grammar
  wherever the repo carries that agent's format. Install a plugin in any agent
  the usual way and `verger watch` spreads it to the rest.

## Status

Ф0 done; Ф1 (Claude/Codex/Gemini adapters, apply, CLI, e2e) — in review.

## Development

```
make fmt lint test    # what CI runs
make build            # bin/verger
make mod              # go mod tidy + vendor
```
