// verger's host runtime.
//
// One ESM module that carries a package's Claude-dialect hooks into the host
// that loaded it: OpenCode 1.x/Kilo (dialect "v1"), OpenCode 2.x ("v2") and Pi
// ("pi"). Everything host-specific is data: the event table in events.json, the
// dialect, and the manifest the shim points at.
//
// Fail-closed: a hook that cannot run — the runtime cannot read its manifest,
// the consent hash does not match, a blocking hook times out or cannot start —
// blocks the tool call instead of letting it through (DESIGN 4.6). A tool the
// table does not classify is blocked under a blocking matcher that was written
// to catch everything (DESIGN 11.5).
import { appendFileSync, readFileSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import type { Dialect, Hook, Manifest } from "./table.ts"
import { busOf } from "./bus.ts"
import { canonicalEvent, canonicalTool, claudeEvent, denyByDefault, hookKey, hookTable, matcherMatches } from "./table.ts"
import { verifyConsent, verifyManifest } from "./consent.ts"
import { decision, runHook } from "./hooks.ts"

export type Options = {
  /** reporting name of the host ("opencode", "kilo", "pi") */
  host: string
  dialect: Dialect
  /** absolute path of the package manifest the shim installed */
  manifestPath: string
  /** the D5 consent hash the shim carries */
  consent: string
  /** sha256 of the manifest file, pinned by the shim */
  manifestSha: string
  /** working directory handed to hook commands */
  cwd?: string
  /** JSONL log of what the runtime decided */
  logPath?: string
  /** extra environment for hook commands */
  env?: Record<string, string>
  /** plugin id reported to the host; defaults to verger.<package> */
  id?: string
}

export type Plugin = {
  id: string
  setup(ctx: unknown): Promise<void>
  server(): Promise<Record<string, unknown>>
}

// readManifest loads the runtime manifest, or throws with the reason.
export function readManifest(path: string): Manifest {
  let raw: string
  try {
    raw = readFileSync(path, "utf8")
  } catch (err) {
    throw new Error(`cannot read runtime manifest ${path}: ${(err as Error).message}`)
  }

  return JSON.parse(raw) as Manifest
}

// pluginID is the identifier the host sees; package ids carry "/", plugin ids
// should not.
function pluginID(explicit: string | undefined, pkg: string): string {
  if (explicit !== undefined && explicit !== "") {
    return explicit
  }

  return `verger.${pkg.replaceAll("/", ".")}`
}

// Runtime carries one manifest, the consent decision and the hook table for one
// host process.
class Runtime {
  #options: Options
  #manifest: Manifest | null = null
  #lockReason: string

  constructor(options: Options) {
    this.#options = options
    this.#lockReason = ""

    try {
      const raw = readFileSync(options.manifestPath)
      this.#manifest = JSON.parse(raw.toString("utf8")) as Manifest
      this.#lockReason =
        verifyManifest(raw, options.manifestSha) || verifyConsent(this.#manifest, options.consent)
    } catch (err) {
      this.#lockReason = (err as Error).message
    }

    if (this.#lockReason !== "") {
      this.#log({ event: "locked", reason: this.#lockReason })
    }

    this.#heartbeat()
  }

  // #heartbeat records that this runtime executed inside a host. The Go side
  // reads the same document to tell a healthy runtime from one no host ever
  // loaded, and to roll back to the previous version (DESIGN 4.6).
  #heartbeat(): void {
    const dir = dirname(dirname(this.#options.manifestPath))

    try {
      writeFileSync(
        join(dir, "heartbeat.json"),
        `${JSON.stringify(
          {
            at: new Date().toISOString(),
            pid: process.pid,
            host: this.#options.host,
            dialect: this.#options.dialect,
            package: this.#manifest?.package ?? "",
          },
          null,
          2,
        )}\n`,
      )
    } catch {
      // a heartbeat that cannot be written never changes a verdict
    }
  }

  #log(entry: Record<string, unknown>): void {
    const path = this.#options.logPath
    if (path === undefined || path === "") {
      return
    }

    try {
      appendFileSync(path, `${JSON.stringify({ at: new Date().toISOString(), ...entry })}\n`)
    } catch {
      // a log that cannot be written is not a reason to change a verdict
    }
  }

  // packageID is the manifest's package id, for the plugin identity.
  get packageID(): string {
    return this.#manifest?.package ?? "unknown"
  }

  // #hooks is the manifest's hooks in CANONICAL event names. The manifest
  // speaks the Claude dialect — what a package author writes and what
  // `pkg/manifest` stores — while everything downstream (which handler to
  // register, which hook a received event selects) is keyed by the canonical
  // name. Comparing the two without normalising meant `SessionStart` never
  // matched `session-start`: no handler was registered, no candidate was ever
  // selected, and the plugin loaded and wrote its heartbeat while doing
  // nothing at all.
  #hooks(): Hook[] {
    return (this.#manifest?.hooks ?? []).map((hook) => ({
      ...hook,
      event: canonicalEvent(hook.event),
    }))
  }

  // handle runs every hook of one canonical event for one tool call and folds
  // their verdicts: any deny blocks.
  async handle(canon: string, payload: Record<string, unknown>): Promise<{ block: boolean; reason: string }> {
    const hostTool = String(payload["tool"] ?? payload["toolName"] ?? "")
    const canonical = canonicalTool(hostTool) ?? ""
    const input = payload["input"] ?? payload["args"] ?? {}

    const candidates = this.#hooks().filter(
      (hook) => hook.event === canon && matcherMatches(hook.matcher, hostTool, canonical === "" ? null : canonical),
    )

    const blocking = hookTable.events[canon]?.blocking === true

    // The two decisions that used to be silent, and are exactly the ones that
    // make a broken dispatch diagnosable: that the event arrived at all, and
    // that nothing matched it. Without them a host that never calls handle and
    // a hook whose event name is wrong look identical from outside — an empty
    // log, with the plugin loaded and its heartbeat written.
    this.#log({
      event: canon,
      tool: hostTool,
      canonical,
      phase: "received",
      candidates: candidates.length,
    })

    if (this.#lockReason !== "") {
      // The manifest or a hook script changed since approval: no hook of this
      // plugin runs, so unapproved code never executes. A blocking event
      // denies; a non-blocking one stays open and leaves the reason to the log
      // (it cannot refuse the call anyway, and refusing it would break the
      // host's own flow).
      const reason = `verger: ${this.#lockReason}`
      this.#log({ event: canon, tool: hostTool, decision: blocking ? "deny" : "allow", reason })

      return { block: blocking, reason: blocking ? reason : "" }
    }

    if (blocking && denyByDefault(canonical === "" ? null : canonical, candidates.map((hook) => hook.matcher))) {
      const reason = `verger: ${hostTool} is not classified by the hook table; a blocking hook that matches every tool refuses it`
      this.#log({ event: canon, tool: hostTool, decision: "deny", reason })

      return { block: true, reason }
    }

    // The four dispatch decisions, one log line each. They used to be one
    // line written after the hook returned, which is the wrong shape for
    // diagnosis: a hook that never launched, a hook that launched and failed,
    // and a hook that was never selected all look the same from outside. Each
    // phase is its own line, so a missing one names itself.
    if (candidates.length === 0) {
      this.#log({ event: canon, tool: hostTool, phase: "no-match", decision: "allow" })
    } else {
      this.#log({ event: canon, tool: hostTool, phase: "matched", hooks: candidates.length })
    }

    for (const hook of candidates) {
      this.#log({ event: canon, tool: hostTool, phase: "launched", command: hook.command })

      const result = await runHook(hook, this.#payload(canon, hostTool, input), {
        cwd: this.#options.cwd,
        env: this.#options.env,
      })
      const verdict = decision(result, blocking)

      this.#log({
        event: canon,
        tool: hostTool,
        phase: "exit",
        command: hook.command,
        code: result.code,
        timedOut: result.timedOut,
        spawnError: result.spawnError,
        decision: verdict.block ? "deny" : "allow",
        reason: verdict.reason,
      })

      if (verdict.block) {
        return verdict
      }
    }

    return { block: false, reason: "" }
  }

  // #payload builds the Claude-dialect hook input: the shape the hook scripts
  // are written against, whatever dialect the host speaks.
  #payload(canon: string, hostTool: string, input: unknown): Record<string, unknown> {
    return {
      session_id: "",
      transcript_path: "",
      cwd: this.#options.cwd ?? process.cwd(),
      hook_event_name: claudeEvent(canon),
      tool_name: canonicalTool(hostTool) ?? hostTool,
      tool_input: input,
    }
  }

  // setup registers the runtime on a v2 plugin context.
  async setup(ctx: unknown): Promise<void> {
    const context = ctx as {
      tool?: { hook?: (key: string, handler: (payload: unknown) => unknown) => unknown }
      event?: { subscribe?: (key: string, handler: (payload: unknown) => unknown) => unknown }
    }

    for (const canon of Object.keys(hookTable.events)) {
      const key = hookKey("v2", canon)
      if (key === null || !this.#hooks().some((hook) => hook.event === canon)) {
        continue
      }

      const namespace = hookTable.v2Namespaces[key] ?? "tool"
      const handler = async (payload: unknown) => {
        const verdict = await this.handle(canon, (payload ?? {}) as Record<string, unknown>)
        if (verdict.block) {
          throw new Error(verdict.reason)
        }
      }

      if (namespace === "event") {
        context.event?.subscribe?.(key, handler)

        continue
      }

      context.tool?.hook?.(key, handler)
    }
  }

  // server is the v1 plugin form: the hook map the host calls, blocking by
  // throwing.
  async server(): Promise<Record<string, unknown>> {
    const hooks: Record<string, unknown> = {}

    for (const canon of Object.keys(hookTable.events)) {
      const key = hookKey("v1", canon)
      if (key === null || !this.#hooks().some((hook) => hook.event === canon)) {
        continue
      }

      hooks[key] = async (payload: unknown) => {
        const verdict = await this.handle(canon, (payload ?? {}) as Record<string, unknown>)
        if (verdict.block) {
          throw new Error(verdict.reason)
        }
      }
    }

    return hooks
  }

  // pi registers the runtime on a Pi ExtensionAPI: tool_call blocks by
  // returning { block, reason }, the other events only observe.
  pi(api: unknown): void {
    const bus = busOf(api)
    for (const canon of Object.keys(hookTable.events)) {
      const key = hookKey("pi", canon)
      if (key === null || !this.#hooks().some((hook) => hook.event === canon)) {
        continue
      }

      bus?.on?.(key, async (event: unknown) => {
        const verdict = await this.handle(canon, (event ?? {}) as Record<string, unknown>)
        if (verdict.block && canon === "pre-tool") {
          return { block: true, reason: verdict.reason }
        }
      })
    }
  }
}

// load builds the plugin object a shim exports. The shim may also call the
// named exports directly when it targets Pi.
export async function load(options: Options): Promise<Plugin> {
  const runtime = new Runtime(options)

  return {
    id: pluginID(options.id, runtime.packageID),
    setup: (ctx: unknown) => runtime.setup(ctx),
    server: () => runtime.server(),
  }
}

// piDispatcher is the per-event entry point a Pi shim proxies to. It is
// SYNCHRONOUS to create: `new Runtime` only reads the options, and the
// manifest is read on first use, so nothing here waits on I/O. That is the
// point — the host fires session_start while the extension's default export is
// still on the stack, so a shim that registers its handlers from a resolved
// import misses it. The shim registers synchronously and defers only the CALL.
export function piDispatcher(options: Options): (canon: string, event: unknown) => Promise<Decision> {
  const runtime = new Runtime(options)

  return (canon, event) => runtime.handle(canon, (event ?? {}) as Record<string, unknown>)
}

// piExtension builds the default export of a Pi extension module.
export function piExtension(options: Options): (api: unknown) => void {
  const runtime = new Runtime(options)

  return (api: unknown) => {
    runtime.pi(api as { on?: (event: string, handler: (event: unknown) => unknown) => unknown })
  }
}

// runtime exposes the pieces a host adapter (and its tests) needs.
export { canonicalTool, claudeEvent, denyByDefault, hookKey, hookTable, matcherMatches } from "./table.ts"
export { busOf } from "./bus.ts"
export { decision, runHook } from "./hooks.ts"
export { hookHash, verifyConsent, verifyManifest } from "./consent.ts"
