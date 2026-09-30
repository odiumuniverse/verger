// busOf resolves the event bus out of whatever the host handed the extension.
//
// The bus is not the same object on every host of the pi dialect, and guessing
// wrong is a silent no-op rather than an error: the module loads, the
// heartbeat is written, and no hook ever runs.
//
//   pi   the API itself, with on() on it
//   omp  a context of {pi, extension, runtime, cwd, events, …} with no
//        top-level on; pi's API is a module namespace there and the bus
//        lives under events (probed live on omp 18.4.3)
//
// The generated shim resolves the same shapes, but synchronously and from
// generated code, because the host fires its first event while the extension's
// default export is still on the stack. TestShimBusResolutionAgreesWithTheRuntime
// runs both over the same shapes so a change to one without the other fails.

// Bus is the registration surface: on(key, handler) per host event name.
export interface Bus {
  on?: (event: string, handler: (event: unknown) => unknown) => unknown
}

// busOf returns the object that carries on(), or undefined when the context
// carries none - which is a host whose dialect this runtime does not know,
// and is left to load without hooks rather than throwing at boot.
export function busOf(api: unknown): Bus | undefined {
  const ctx = (api ?? {}) as { on?: unknown; events?: unknown; pi?: unknown }

  if (typeof ctx.on === "function") {
    return ctx as Bus
  }

  for (const candidate of [ctx.events, ctx.pi]) {
    const bus = candidate as Bus | undefined
    if (typeof bus?.on === "function") {
      return bus
    }
  }

  return undefined
}
