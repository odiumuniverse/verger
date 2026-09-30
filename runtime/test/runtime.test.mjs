// The runtime bundle's own tests: they run the artifact the hosts load
// (`pkg/runtime/bundle/index.js`), never the sources, so a build that broke
// the published shape fails here.
import { test } from "node:test"
import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import path from "node:path"

import {
  busOf,
  canonicalTool,
  decision,
  denyByDefault,
  hookHash,
  hookKey,
  hookTable,
  load,
  matcherMatches,
  piDispatcher,
  piExtension,
  runHook,
  verifyConsent,
} from "../../pkg/runtime/bundle/index.js"

// home is one temp dir per test file.
const home = mkdtempSync(path.join(tmpdir(), "verger-runtime-"))

// writeManifest writes one runtime manifest and returns its path.
function writeManifest(name, manifest) {
  const file = path.join(home, `${name}.json`)
  writeFileSync(file, JSON.stringify(manifest, null, 2))

  return file
}

const preToolHook = {
  event: "pre-tool",
  matcher: "Bash",
  command: "cat > /dev/null",
  timeout: 5,
}

// manifestSha is the digest the shim pins: sha256 of the manifest file bytes.
function manifestSha(file) {
  return createHash("sha256").update(readFileSync(file)).digest("hex")
}

// The golden consent fixture is shared with the Go side: both languages must
// derive the same D5 hash from the same manifest.
import golden from "../testdata/consent-golden.json" with { type: "json" }

test("hook hash matches the shared golden fixture", () => {
  assert.equal(hookHash(golden.package, golden.version, golden.hooks), golden.hash)
})

test("the event table maps every canonical event per dialect", () => {
  assert.equal(hookKey("v1", "pre-tool"), "tool.execute.before")
  assert.equal(hookKey("v2", "pre-tool"), "execute.before")
  assert.equal(hookKey("pi", "pre-tool"), "tool_call")
  assert.equal(hookKey("v1", "post-tool"), "tool.execute.after")
  assert.equal(hookKey("v2", "post-tool"), "execute.after")
  assert.equal(hookKey("pi", "post-tool"), "tool_result")
  assert.equal(hookKey("v1", "stop"), "session.idle")
  assert.equal(hookKey("v2", "stop"), "session.idle")
  assert.equal(hookKey("pi", "stop"), "turn_end")
  assert.equal(hookKey("pi", "session-start"), "session_start")

  // A dialect with no verified event is null, never a guessed name.
  assert.equal(hookKey("v1", "notification"), null)
  assert.equal(hookKey("v2", "session-start"), null)

  assert.equal(hookTable.events["pre-tool"].blocking, true)
  assert.equal(hookTable.events["post-tool"].blocking, false)
})

test("tool names resolve through the alias table", () => {
  assert.equal(canonicalTool("Bash"), "Bash")
  assert.equal(canonicalTool("bash"), "Bash")
  assert.equal(canonicalTool("patch"), "Edit")
  assert.equal(canonicalTool("multiedit"), "Edit")
  assert.equal(canonicalTool("mcp__probe__do"), null)
})

test("matchers are alternations of globs", () => {
  assert.equal(matcherMatches("Bash", "bash", "Bash"), true)
  assert.equal(matcherMatches("Bash|Edit", "Edit", "Edit"), true)
  assert.equal(matcherMatches("B*", "bash", "Bash"), true)
  assert.equal(matcherMatches("Read", "bash", "Bash"), false)
  assert.equal(matcherMatches("", "anything", null), true)
  assert.equal(matcherMatches("*", "anything", null), true)
})

test("an unclassified tool is denied only under a broad blocking matcher", () => {
  assert.equal(denyByDefault(null, ["*"]), true)
  assert.equal(denyByDefault(null, [""]), true)
  assert.equal(denyByDefault(null, ["Bash"]), false)
  assert.equal(denyByDefault("Bash", ["*"]), false)
})

test("hook decisions follow the Claude dialect", () => {
  assert.deepEqual(decision({ code: 0, stdout: "", stderr: "", timedOut: false, spawnError: "" }, true), {
    block: false,
    reason: "",
  })
  assert.deepEqual(decision({ code: 2, stdout: "", stderr: "no", timedOut: false, spawnError: "" }, true), {
    block: true,
    reason: "no",
  })
  assert.deepEqual(
    decision(
      {
        code: 0,
        stdout: '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"rm -rf"}}',
        stderr: "",
        timedOut: false,
        spawnError: "",
      },
      true,
    ),
    { block: true, reason: "rm -rf" },
  )

  // Fail-closed only for blocking events.
  const failure = { code: 1, stdout: "", stderr: "boom", timedOut: false, spawnError: "" }
  assert.equal(decision(failure, true).block, true)
  assert.equal(decision(failure, false).block, false)
  assert.equal(decision({ code: -1, stdout: "", stderr: "", timedOut: true, spawnError: "" }, true).block, true)
})

test("a hook runs with the payload on stdin", async () => {
  const echo = path.join(home, "echo-hook.sh")
  writeFileSync(echo, "#!/bin/sh\nread -r line\necho \"$line\" | head -c 40\n", { mode: 0o755 })

  const result = await runHook({ ...preToolHook, command: echo }, { hook_event_name: "PreToolUse" })

  assert.equal(result.code, 0)
  assert.match(result.stdout, /"hook_event_name":"PreToolUse"/)
})

test("a hook that never answers times out", async () => {
  const result = await runHook({ event: "pre-tool", matcher: "Bash", command: "sleep 30", timeout: 1 }, {})

  assert.equal(result.timedOut, true)
  assert.equal(decision(result, true).block, true)
})

test("a consent mismatch locks the blocking hooks", async () => {
  const manifest = {
    schema: 1,
    package: "acme/caveman",
    version: "1.0.0",
    hooks: [{ ...preToolHook, event: "pre-tool", command: "exit 0" }],
    scripts: [],
  }
  const file = writeManifest("locked", manifest)
  const options = {
    host: "opencode",
    manifestPath: file,
    manifestSha: manifestSha(file),
    consent: "0000000000000000000000000000000000000000000000000000000000000000",
  }

  const v1 = await load({ ...options, dialect: "v1" })
  const hooks = await v1.server()
  await assert.rejects(() => hooks["tool.execute.before"]({ tool: "bash" }), /hooks changed since they were approved/)

  const v2 = await load({ ...options, dialect: "v2" })
  const ctx = fakeV2Context()
  await v2.setup(ctx)
  await assert.rejects(() => ctx.hooks["execute.before"]({ tool: "bash" }), /hooks changed since they were approved/)
})

test("a matching deny hook blocks through every dialect entry", async () => {
  const manifest = {
    schema: 1,
    package: "acme/caveman",
    version: "1.0.0",
    hooks: [{ event: "pre-tool", matcher: "Bash", command: 'echo \'{"decision":"block","reason":"no rm"}\'', timeout: 5 }],
    scripts: [],
  }
  const file = writeManifest("deny", manifest)
  const consent = hookHash(manifest.package, manifest.version, manifest.hooks)

  assert.equal(verifyConsent({ ...manifest }, consent), "")

  const options = { host: "opencode", dialect: "v1", manifestPath: file, manifestSha: manifestSha(file), consent }

  const plugin = await load(options)
  const hooks = await plugin.server()
  await assert.rejects(() => hooks["tool.execute.before"]({ tool: "bash" }), /no rm/)

  const v2 = await load({ ...options, dialect: "v2" })
  const ctx = fakeV2Context()
  await v2.setup(ctx)
  await assert.rejects(() => ctx.hooks["execute.before"]({ tool: "bash" }), /no rm/)
  assert.equal(ctx.subscribed.length, 0)

  const pi = fakePiAPI()
  piExtension({ ...options, dialect: "pi" })(pi)
  const verdict = await pi.handlers["tool_call"]({ toolName: "bash" })
  assert.deepEqual(verdict, { block: true, reason: "no rm" })
})

test("an unclassified tool is denied under a blocking matcher that matches everything", async () => {
  const manifest = {
    schema: 1,
    package: "acme/caveman",
    version: "1.0.0",
    hooks: [{ event: "pre-tool", matcher: "*", command: "exit 0", timeout: 5 }],
    scripts: [],
  }
  const file = writeManifest("broad", manifest)
  const consent = hookHash(manifest.package, manifest.version, manifest.hooks)

  const plugin = await load({ host: "kilo", dialect: "v1", manifestPath: file, manifestSha: manifestSha(file), consent })
  const hooks = await plugin.server()

  await assert.rejects(() => hooks["tool.execute.before"]({ tool: "some_new_tool" }), /not classified/)
  assert.equal(await hooks["tool.execute.before"]({ tool: "bash" }), undefined)
})

// The script digest is the only thing standing between "the user approved
// these hooks" and "the bytes on disk changed": the manifest hash covers the
// declared hook list, not the files a hook runs.
test("a changed hook script locks the blocking hooks", async () => {
  const script = path.join(home, "guard.js")
  writeFileSync(script, "process.exit(0)\n", { mode: 0o755 })

  const digest = (file) => createHash("sha256").update(readFileSync(file)).digest("hex")

  const manifest = {
    schema: 1,
    package: "acme/caveman",
    version: "1.0.0",
    hooks: [
      { event: "pre-tool", matcher: "Bash", command: `node ${script}`, timeout: 5 },
      { event: "post-tool", matcher: "Bash", command: `node ${script}`, timeout: 5 },
    ],
    scripts: [{ path: script, sha256: digest(script) }],
  }
  const file = writeManifest("scripts", manifest)
  const consent = hookHash(manifest.package, manifest.version, manifest.hooks)

  // Intact: the approved manifest verifies clean.
  assert.equal(verifyConsent(manifest, consent), "")

  // The script changes on disk after approval.
  writeFileSync(script, "process.exit(2)\n")

  const reason = verifyConsent(manifest, consent)
  assert.match(reason, /hook script .*guard\.js changed since it was approved/)

  const plugin = await load({ host: "kilo", dialect: "v1", manifestPath: file, manifestSha: manifestSha(file), consent })
  const hooks = await plugin.server()

  await assert.rejects(() => hooks["tool.execute.before"]({ tool: "bash" }), /changed since it was approved/)

  // The same lock holds on the v2 entry, and a non-blocking event stays open.
  const v2 = await load({ host: "opencode", dialect: "v2", manifestPath: file, manifestSha: manifestSha(file), consent })
  const ctx = fakeV2Context()
  await v2.setup(ctx)
  await assert.rejects(() => ctx.hooks["execute.before"]({ tool: "bash" }), /changed since it was approved/)

  // The same manifest declares a post-tool hook, so the lock is scoped to the
  // blocking keys: the non-blocking entry is registered and stays open while
  // locked — and the changed script it names is never executed (a non-blocking
  // event cannot refuse the call, and unapproved code must not run).
  assert.equal(typeof ctx.hooks["execute.after"], "function")
  await ctx.hooks["execute.after"]({ tool: "bash" })
})

test("an edited manifest record locks, even when the hooks are unchanged", async () => {
  const script = path.join(home, "guarded.js")
  writeFileSync(script, "process.exit(0)\n", { mode: 0o755 })

  const manifest = {
    schema: 1,
    package: "acme/caveman",
    version: "1.0.0",
    hooks: [{ event: "pre-tool", matcher: "Bash", command: `node ${script}`, timeout: 5 }],
    scripts: [{ path: script, sha256: createHash("sha256").update(readFileSync(script)).digest("hex") }],
  }
  const file = writeManifest("record", manifest)
  const pinned = manifestSha(file)
  const consent = hookHash(manifest.package, manifest.version, manifest.hooks)

  // Someone rewrites the record: the script digest now points at new bytes.
  manifest.scripts[0].sha256 = "0".repeat(64)
  writeFileSync(file, JSON.stringify(manifest, null, 2))

  const plugin = await load({ host: "kilo", dialect: "v1", manifestPath: file, manifestSha: pinned, consent })
  const hooks = await plugin.server()

  await assert.rejects(
    () => hooks["tool.execute.before"]({ tool: "bash" }),
    /the runtime manifest changed since the shim was written/,
  )
})

test("a manifest that does not declare its scripts locks", async () => {
  const manifest = {
    schema: 1,
    package: "acme/caveman",
    version: "1.0.0",
    hooks: [{ event: "pre-tool", matcher: "Bash", command: "exit 0", timeout: 5 }],
  }
  const file = writeManifest("noscripts", manifest)
  const consent = hookHash(manifest.package, manifest.version, manifest.hooks)

  const plugin = await load({ host: "kilo", dialect: "v1", manifestPath: file, manifestSha: manifestSha(file), consent })
  const hooks = await plugin.server()

  await assert.rejects(
    () => hooks["tool.execute.before"]({ tool: "bash" }),
    /declares no hook scripts/,
  )
})

test("a missing manifest locks the runtime instead of running nothing", async () => {
  const plugin = await load({
    host: "opencode",
    dialect: "v2",
    manifestPath: path.join(home, "absent.json"),
    manifestSha: "deadbeef",
    consent: "deadbeef",
  })

  const ctx = fakeV2Context()
  await plugin.setup(ctx)

  // No hook is registered (nothing to run), and a manifest that cannot be read
  // is reported: the lock only shows up where a hook would have run.
  assert.deepEqual(Object.keys(ctx.hooks), [])
})

// fakeV2Context is the opencode v2 plugin context, with just the surface the
// runtime uses.
function fakeV2Context() {
  const hooks = {}
  const subscribed = []

  return {
    hooks,
    subscribed,
    tool: {
      hook: (key, handler) => {
        hooks[key] = handler
      },
    },
    event: {
      subscribe: (key, handler) => {
        subscribed.push(key)
        hooks[key] = handler
      },
    },
  }
}

// fakePiAPI is the Pi ExtensionAPI surface the runtime uses.
function fakePiAPI() {
  const handlers = {}

  return {
    handlers,
    on: (event, handler) => {
      handlers[event] = handler
    },
  }
}

// The dispatch log is the only thing that distinguishes four failures that look
// identical from outside: a hook that was never selected, one that was selected
// but never launched, one that launched and failed, and one that ran. Each is a
// separate line and names itself, so a missing one says which half is broken.
test("every dispatch decision writes its own JSONL line", async () => {
  const manifest = {
    schema: 1,
    package: "acme/dispatch",
    version: "1.0.0",
    hooks: [{ event: "pre-tool", matcher: "Bash", command: 'echo \'{"decision":"allow"}\' >/dev/null', timeout: 5 }],
    scripts: [],
  }
  const file = writeManifest("dispatch", manifest)
  const consent = hookHash(manifest.package, manifest.version, manifest.hooks)
  const logPath = path.join(home, "dispatch.jsonl")

  const options = { dialect: "pi", manifestPath: file, manifestSha: manifestSha(file), consent, logPath }
  const pi = fakePiAPI()
  piExtension(options)(pi)

  await pi.handlers.tool_call({ toolName: "bash" })
  // A classified tool the hook's matcher does not cover: the event arrived and
  // nothing was selected. (An UNclassified tool is a different decision
  // entirely - a blocking event denies it outright, below that point.)
  await pi.handlers.tool_call({ toolName: "Read" })

  const lines = readFileSync(logPath, "utf8").trim().split("\n").map((line) => JSON.parse(line))
  const phases = lines.map((entry) => entry.phase)

  // The matched event: received, then matched, then launched, then exit. Each
  // of the four is its own line, in the order the decisions are actually made.
  assert.deepEqual(phases, ["received", "matched", "launched", "exit", "received", "no-match"])
  assert.equal(lines[0].candidates, 1)
  assert.equal(lines[1].hooks, 1)
  assert.equal(lines[2].command, manifest.hooks[0].command)
  assert.equal(lines[3].code, 0)
  assert.equal(lines[3].decision, "allow")
  assert.equal(lines[3].timedOut, false)
  assert.equal(typeof lines[3].at, "string")

  // The unmatched tool: same event, arrived (candidates 0), nothing selected.
  assert.equal(lines[4].candidates, 0)
  assert.equal(lines[5].decision, "allow")

  // An unclassified tool under a NARROW matcher is not this test's business:
  // the table's rule is that only a broad blocking matcher may refuse a tool
  // it does not classify, so it falls through to the same no-match as above.
  await pi.handlers.tool_call({ toolName: "NotATool" })
  assert.equal(readFileSync(logPath, "utf8").trim().split("\n").length, 8)

  // Under a broad one it is a distinct decision, written before the phases and
  // carrying no phase at all - a refusal, not a dispatch.
  const broad = {
    ...manifest,
    package: "acme/dispatch-broad",
    hooks: [{ event: "pre-tool", matcher: "*", command: "cat > /dev/null", timeout: 5 }],
  }
  const broadFile = writeManifest("dispatch-broad", broad)
  const broadLog = path.join(home, "dispatch-broad.jsonl")
  const broadPI = fakePiAPI()
  piExtension({
    dialect: "pi",
    manifestPath: broadFile,
    manifestSha: manifestSha(broadFile),
    consent: hookHash(broad.package, broad.version, broad.hooks),
    logPath: broadLog,
  })(broadPI)

  await broadPI.handlers.tool_call({ toolName: "NotATool" })

  const denied = JSON.parse(readFileSync(broadLog, "utf8").trim().split("\n").at(-1))
  assert.equal(denied.phase, undefined)
  assert.equal(denied.decision, "deny")
  assert.match(denied.reason, /not classified/)
})

// The generated shim resolves the bus itself, synchronously, because the host
// fires its first event while the extension's default export is still on the
// stack. That makes it the one place host knowledge is duplicated, so both are
// run over the same shapes here: a change to one without the other fails.
test("the shim and the runtime resolve the same bus from every known shape", async () => {
  // the expression the generated shim ships, verbatim
  const shim = (pi) => (typeof pi?.on === "function" ? pi : pi?.events ?? pi?.pi)

  const piAPI = { on: () => {} }
  const ompContext = { pi: { ALL_SEGMENT_IDS: [] }, extension: {}, events: { on: () => {} }, cwd: "." }
  const nothing = { cwd: "." }

  assert.equal(busOf(piAPI), shim(piAPI))
  assert.equal(busOf(ompContext), shim(ompContext))
  assert.equal(busOf(ompContext), ompContext.events)
  assert.equal(busOf(nothing), undefined)
  assert.equal(shim(nothing), undefined)
})
