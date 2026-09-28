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
| Cursor | loose only (native/synth refused: no `cursor-agent` subcommand registers a package, and verger does not deliver the documented `~/.cursor/plugins/local/<name>` form yet; hooks silenced — a flat camelCase dialect verger does not render) | live-verified on cursor-agent 2026.06.15: skills → `~/.cursor/skills`, agents → `~/.cursor/agents`, slash commands → `~/.cursor/commands`, MCP → `~/.cursor/mcp.json`, rules as the D23 skill wrapper (the delivery says so); no CI leg — the CLI ships from cursor.com/install, not npm |
| Antigravity, OpenCode, Kilo, Pi, DeepSeek Harness | not implemented (Ф2, `docs/TASKS.md` T2.2) | — |

- Desired state in `verger.toml`, exact versions and hashes in `verger.lock`,
  what actually landed on disk in receipts.
- Delivery per host is a pure function of the host's capabilities:
  `native → synth → loose → silenced(reason)`.
- `verger status` prints the maturity of each host next to its cells
  (`experimental` → `beta` after a green e2e run → `stable` after a full cycle
  on a live machine): the evidence table is `pkg/cli/maturity.go` and its
  default is `experimental`, so landing an adapter promotes nothing by itself.
- A package installed by hand in an agent is adopted with `verger adopt
  <host:ref>`; the automatic spread to the other hosts (`verger watch`,
  guarded auto-remove) arrives in Ф2 — `watch` is a stub that says so today.
- No registry of its own: packages resolve straight from their source —
  `owner/repo`, `plugin@owner/repo`, a git/npm/MCP ref or a link to a skills or
  marketplace site — and install natively through each agent's own CLI grammar
  wherever the repo carries that agent's format.

## Status

Ф0 done; Ф1 (Claude/Codex/Gemini adapters, apply, CLI, e2e) — in review. The
`omp` and `cursor` adapters landed after Ф1: the `omp` e2e leg is green on
omp 18.4.1, `cursor` is live-verified on cursor-agent 2026.06.15 (loose only: no
`cursor-agent` subcommand registers a package, so the CLI strata are refused with
that reason). Ф2 — the remaining
five adapters (Antigravity, OpenCode, Kilo, Pi, DeepSeek Harness), `pkg/caps` /
`pkg/plan`, watcher, TUI — has not started (`docs/TASKS.md`), and the hosts it
covers are not claimed above.

## Development

```
make fmt lint test    # what CI runs
make build            # bin/verger
make mod              # go mod tidy + vendor
```

## Release

Pushing a tag is the whole procedure:

```
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` (tags `v*`, `contents: write`, `macos-latest`)
then:

1. builds `./cmd/verger` for darwin/arm64 and darwin/amd64 with
   `-ldflags "-X main.version=<tag without v>"`, merges them with `lipo`, and
   code-signs the merged binary — `MACOS_SIGN_P12` / `MACOS_SIGN_P12_PASSWORD` /
   `MACOS_SIGN_KEYCHAIN_PASSWORD` when they are set, otherwise ad-hoc
   (`com.odiumuniverse.verger` is the identifier);
2. packs `dist/verger-<version>-darwin-universal.tar.gz`, whose archive root holds
   the bare `verger` binary — exactly what `bin.install "verger"` expects;
3. creates the GitHub release for the tag (or reuses it) and uploads the tarball
   with `--clobber`;
4. clones `odiumuniverse/homebrew-tap` with `secrets.TAP_TOKEN`, rewrites
   `Formula/verger.rb` — creating it from a template on the first tag, since a
   formula is never seeded with a placeholder sha256 — commits as
   `github-actions[bot]` and pushes. The tap is written only by this step;
   the tag's artifact is the single source of the url and sha256.

Required repository secret: `TAP_TOKEN`, a token that may push to
`odiumuniverse/homebrew-tap` (contents: write). Without it the bump step fails
immediately with `TAP_TOKEN is not set`. The three `MACOS_SIGN_*` secrets are
optional and only switch the build from ad-hoc to a stable signature.

The tap's formula is therefore never hand-edited and never holds a version that
was not released: `brew install odiumuniverse/tap/verger` trails the newest tag
by one workflow run.

