// The shared hook event table. The same JSON is embedded by pkg/runtime in
// Go, so the dialect mapping has exactly one definition; a test on each side
// pins the mapping it reads.
import table from "../events.json"

export type Dialect = "v1" | "v2" | "pi"

export type Hook = {
  event: string
  matcher: string
  command: string
  timeout: number
}

export type Script = { path: string; sha256: string }

export type Manifest = {
  schema: number
  package: string
  version: string
  hooks: Hook[]
  scripts: Script[]
}

export type HookTable = {
  dialects: Dialect[]
  events: Record<string, Record<Dialect, string | null> & { blocking: boolean }>
  v2Namespaces: Record<string, string>
  tools: Record<string, string>
  aliases: Record<string, string>
  uncategorised: string
}

export const hookTable = table as unknown as HookTable

// claudeEvents maps the Claude-dialect event names onto the canonical events.
const claudeEvents: Record<string, string> = {
  PreToolUse: "pre-tool",
  PostToolUse: "post-tool",
  Stop: "stop",
  SessionStart: "session-start",
  Notification: "notification",
}

// claudeEvent is the reverse map, for the hook stdin payload the scripts read.
export function claudeEvent(canon: string): string {
  for (const [name, value] of Object.entries(claudeEvents)) {
    if (value === canon) {
      return name
    }
  }

  return canon
}

// hookKey returns the host hook key a canonical event is registered under, or
// null when the dialect has no verified event for it (never guessed).
export function hookKey(dialect: Dialect, canon: string): string | null {
  const row = hookTable.events[canon]
  if (row === undefined) {
    return null
  }

  return row[dialect] ?? null
}

// canonicalTool maps a host tool name onto the canonical tool name through the
// alias table; null means the table does not classify it (an MCP tool, or a
// tool a host version added).
export function canonicalTool(name: string): string | null {
  if (name === "") {
    return null
  }

  for (const candidate of [name, name.toLowerCase()]) {
    if (Object.hasOwn(hookTable.tools, candidate)) {
      return candidate
    }

    if (Object.hasOwn(hookTable.aliases, candidate)) {
      return hookTable.aliases[candidate]
    }
  }

  return null
}

// matcherMatches reports whether a canonical matcher matches a tool. The
// Claude matcher dialect is an alternation of names with `*` globs; an empty
// matcher or `*` is broad and matches everything.
export function matcherMatches(matcher: string, tool: string, canonical: string | null): boolean {
  const trimmed = matcher.trim()

  if (trimmed === "" || trimmed === "*") {
    return true
  }

  for (const part of trimmed.split("|")) {
    const pattern = part.trim()
    if (pattern === "") {
      continue
    }

    if (globMatches(pattern, tool)) {
      return true
    }

    if (canonical !== null && globMatches(pattern, canonical)) {
      return true
    }
  }

  return false
}

// globMatches matches one name against a glob with `*` wildcards.
export function globMatches(pattern: string, name: string): boolean {
  if (pattern === name) {
    return true
  }

  const parts = pattern.split("*")
  let rest = name

  for (const [index, part] of parts.entries()) {
    if (part === "") {
      continue
    }

    const at = rest.indexOf(part)
    if (at < 0) {
      return false
    }

    if (index === 0 && !pattern.startsWith("*") && at !== 0) {
      return false
    }

    rest = rest.slice(at + part.length)
  }

  return true
}

// denyByDefault reports whether an unclassified tool must be denied under
// blocking hooks: the coverage rule of DESIGN 11.5 — a blocking matcher that
// was written to catch everything must not let an unknown tool through.
export function denyByDefault(canonical: string | null, matchers: string[]): boolean {
  if (canonical !== null || hookTable.uncategorised !== "deny-under-blocking-matcher") {
    return false
  }

  return matchers.some((matcher) => {
    const trimmed = matcher.trim()

    return trimmed === "" || trimmed === "*"
  })
}
