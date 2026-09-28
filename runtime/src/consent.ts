// Consent verification: the runtime recomputes the D5 content hash of the
// hooks it is about to run and refuses to run them when it does not match the
// hash the user approved. The algorithm mirrors pkg/consent.HookHash exactly
// (a cross-language golden test pins both sides).
import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import type { Hook, Manifest, Script } from "./table.ts"

// sha256 is the lowercase hex digest of one value.
export function sha256(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex")
}

// hookHash is the canonical hook hash of a package version: one
// "<pkg>@<version>" line, then one sorted "<event>\x00<matcher>\x00<command>
// \x00<timeout>" line per hook. A package without hooks still hashes its
// version header.
export function hookHash(pkg: string, version: string, hooks: Hook[]): string {
  const lines = hooks.map((hook) =>
    [hook.event, hook.matcher, hook.command, String(hook.timeout)].join("\u0000"),
  )
  lines.sort()

  let canonical = `${pkg}@${version}\n`
  for (const line of lines) {
    canonical += `${line}\n`
  }

  return sha256(canonical)
}

// scriptMismatch names the first script whose content does not match its
// recorded digest; an empty string means every script is intact.
export function scriptMismatch(scripts: Script[]): string {
  for (const script of scripts) {
    let data: Buffer
    try {
      data = readFileSync(script.path)
    } catch {
      return script.path
    }

    if (sha256(data) !== script.sha256) {
      return script.path
    }
  }

  return ""
}

// verifyManifest authenticates the record itself: the shim pins the sha256 of
// the manifest file, so editing the file — the script digests included — no
// longer re-points what the user approved. An empty reason means the bytes are
// the ones the shim was rendered for.
export function verifyManifest(raw: string | Uint8Array, manifestSha: string): string {
  const actual = sha256(raw)
  if (actual !== manifestSha) {
    return `the runtime manifest changed since the shim was written (${actual} != ${manifestSha})`
  }

  return ""
}

// verifyConsent returns the reason the manifest may not run, or an empty string
// when the hooks are exactly the ones the user approved and every script is
// intact. A manifest that does not declare its scripts at all is refused: the
// gate would otherwise be vacuous for the one shape an attacker can produce by
// deleting a key.
export function verifyConsent(manifest: Manifest, consent: string): string {
  const expected = hookHash(manifest.package, manifest.version, manifest.hooks)
  if (expected !== consent) {
    return `hooks changed since they were approved (${expected} != ${consent})`
  }

  if (manifest.scripts === undefined) {
    return "the manifest declares no hook scripts; nothing can be verified"
  }

  const mismatch = scriptMismatch(manifest.scripts)
  if (mismatch !== "") {
    return `hook script ${mismatch} changed since it was approved`
  }

  return ""
}
