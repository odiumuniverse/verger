// Running one hook and reading its decision: the Claude dialect is the
// canonical one (stdin JSON, exit 0 = allow, exit 2 = block with stderr as the
// reason), and every host dialect is mapped onto it.
import type { ChildProcess } from "node:child_process"
import { spawn } from "node:child_process"
import type { Hook } from "./table.ts"

// RunResult is what one hook process did.
export type RunResult = {
  code: number
  stdout: string
  stderr: string
  timedOut: boolean
  spawnError: string
}

// Decision is the verdict of one hook.
export type Decision = { block: boolean; reason: string }

// hookTimeout is the fallback when a hook carries no timeout of its own.
export const hookTimeout = 30_000

// withResolvers is Promise.withResolvers for Node 20: the API arrived in Node
// 22, and the hosts run Node 20 and Bun, so the runtime carries its own rather
// than dropping either.
function withResolvers<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => {}

  const promise = new Promise<T>((res) => {
    resolve = res
  })

  return { promise, resolve }
}

// runHook executes one hook command with the payload on stdin and a bounded
// wait. A command that never answers is a failed hook, not a hang.
export function runHook(
  hook: Hook,
  payload: unknown,
  options: { cwd?: string; env?: Record<string, string>; timeoutMs?: number } = {},
): Promise<RunResult> {
  const timeout = (hook.timeout > 0 ? hook.timeout * 1000 : 0) || options.timeoutMs || hookTimeout
  const { promise, resolve } = withResolvers<RunResult>()
  // detached makes the child a process-group leader so the timeout can kill
  // the whole group: `shell: true` puts a shell in front of the command, and
  // SIGKILL on the shell alone leaves a grandchild (and the pipes it inherited)
  // alive — on Linux that kept the runtime's own process alive for as long as
  // the grandchild ran.
  const child = spawn(hook.command, {
    shell: true,
    detached: true,
    cwd: options.cwd,
    env: { ...process.env, ...(options.env ?? {}) },
    stdio: ["pipe", "pipe", "pipe"],
  })

  let stdout = ""
  let stderr = ""
  let settled = false

  const finish = (result: RunResult) => {
    if (settled) {
      return
    }

    settled = true
    clearTimeout(timer)
    resolve(result)
  }

  const timer = setTimeout(() => {
    killGroup(child)
    releasePipes(child)
    finish({ code: -1, stdout, stderr, timedOut: true, spawnError: "" })
  }, timeout)

  child.stdout.on("data", (chunk: Buffer) => {
    stdout += chunk.toString()
  })
  child.stderr.on("data", (chunk: Buffer) => {
    stderr += chunk.toString()
  })

  // A hook that never reads its stdin — `exit 2`, `true`, a script that
  // answers from the environment — closes the pipe under the payload write.
  // Node raises that as an unhandled stream error (EPIPE on Linux, where the
  // child wins the race far more often than on macOS), which used to escape
  // the runtime and leave the timeout pending. The write is an offer, not a
  // contract: the exit code carries the verdict, so a broken pipe is ignored
  // on every stdio stream and the child's own events settle the promise.
  const swallowPipeError = () => {}

  child.stdin.on("error", swallowPipeError)
  child.stdout.on("error", swallowPipeError)
  child.stderr.on("error", swallowPipeError)

  child.on("error", (err: Error) => {
    finish({ code: -1, stdout, stderr, timedOut: false, spawnError: err.message })
  })
  child.on("close", (code: number | null) => {
    finish({ code: code ?? -1, stdout, stderr, timedOut: false, spawnError: "" })
  })

  try {
    child.stdin.end(JSON.stringify(payload))
  } catch {
    // The pipe was already gone when the write started; the child's exit code
    // is still the verdict, and 'close' settles the promise.
  }

  return promise
}

// killGroup SIGKILLs the child and, when it leads one, its whole process group:
// the shell `shell: true` puts in front of the command execs the command
// itself for a simple line, but a pipeline or a background child would survive
// a kill aimed at the shell alone. ESRCH (already gone) is not an error.
function killGroup(child: ChildProcess): void {
  if (child.pid === undefined) {
    return
  }

  try {
    process.kill(-child.pid, "SIGKILL")
  } catch {
    // No group (the child already reaped it) — fall back to the direct kill.
  }

  child.kill("SIGKILL")
}

// releasePipes drops the stdio streams of a killed child. The pipes are what
// keep the event loop alive: a grandchild that inherited them can outlive the
// kill by its own runtime, and the runtime would then wait for it.
function releasePipes(child: ChildProcess): void {
  child.stdin?.destroy()
  child.stdout?.destroy()
  child.stderr?.destroy()
}

// decision reads one hook result. A blocking event treats every failure —
// non-zero exit, timeout, a command that cannot start — as a block
// (fail-closed, DESIGN 4.6); a non-blocking event lets the call through and
// leaves the reason to the caller's log.
export function decision(result: RunResult, blocking: boolean): Decision {
  if (result.spawnError !== "") {
    return { block: blocking, reason: result.spawnError }
  }

  if (result.timedOut) {
    return { block: blocking, reason: "hook timed out" }
  }

  if (result.code === 2) {
    return { block: true, reason: result.stderr.trim() || "hook blocked the call" }
  }

  if (result.code !== 0) {
    return { block: blocking, reason: result.stderr.trim() || `hook failed with exit ${result.code}` }
  }

  const verdict = parseVerdict(result.stdout)
  if (verdict !== null) {
    return verdict
  }

  return { block: false, reason: "" }
}

// parseVerdict reads the Claude-dialect JSON verdict of a hook that exited 0:
// `hookSpecificOutput.permissionDecision` (the current key) or the legacy
// top-level `decision`. Anything unparsable is not a verdict and never blocks
// on its own.
function parseVerdict(stdout: string): Decision | null {
  const trimmed = stdout.trim()
  if (trimmed === "") {
    return null
  }

  let doc: Record<string, unknown>
  try {
    doc = JSON.parse(trimmed) as Record<string, unknown>
  } catch {
    return null
  }

  const specific = doc["hookSpecificOutput"] as Record<string, unknown> | undefined
  const verdict = (specific?.["permissionDecision"] ?? doc["permissionDecision"] ?? doc["decision"]) as
    | string
    | undefined
  const reason = String(specific?.["permissionDecisionReason"] ?? doc["reason"] ?? "")

  switch (verdict) {
    case "deny":
    case "block":
      return { block: true, reason: reason || "hook denied the call" }
    case "ask":
      return { block: true, reason: reason || "hook escalated the call" }
    default:
      return { block: false, reason }
  }
}
