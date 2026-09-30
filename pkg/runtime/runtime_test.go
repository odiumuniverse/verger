package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/runtime"
	"github.com/odiumuniverse/verger/pkg/store"
)

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular()
}

// dirExists reports whether path is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}

// openStore opens a store in a temp dir.
func openStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return st
}

// consentGolden is the shared cross-language fixture: the bundle's hook hash
// and the Go hash must be the same for the same manifest.
type consentGolden struct {
	Package string         `json:"package"`
	Version string         `json:"version"`
	Hooks   []runtime.Hook `json:"hooks"`
	Hash    string         `json:"hash"`
}

// TestTableMatchesTheBundleSource pins the embedded table to the source the
// bundle imports: two copies that drift would map hooks differently on each
// side of the same delivery.
func TestTableMatchesTheBundleSource(t *testing.T) {
	Convey("Given the embedded runtime table", t, func() {
		embedded, err := runtime.TableBytes()
		So(err, ShouldBeNil)

		source, readErr := os.ReadFile(filepath.Join("..", "..", "runtime", "events.json"))
		So(readErr, ShouldBeNil)

		Convey("When it is compared with runtime/events.json", func() {
			Convey("Then the bytes are identical", func() {
				So(bytes.Equal(embedded, source), ShouldBeTrue)
			})
		})
	})
}

// TestTableMappings pins the dialect mapping the runtime and the adapters read:
// a change here is a change in what hooks a host can run.
func TestTableMappings(t *testing.T) {
	Convey("Given the embedded table", t, func() {
		table, err := runtime.LoadTable()
		So(err, ShouldBeNil)

		Convey("When the blocking events are asked for per dialect", func() {
			Convey("Then every dialect maps pre-tool and only pre-tool blocks", func() {
				So(table.BlockingKeys("v1"), ShouldResemble, []string{"tool.execute.before"})
				So(table.BlockingKeys("v2"), ShouldResemble, []string{"execute.before"})
				So(table.BlockingKeys("pi"), ShouldResemble, []string{"tool_call"})
			})
		})

		Convey("When an event a dialect does not have is asked for", func() {
			key, ok := table.HookKey("v1", "notification")

			Convey("Then it is absent, never guessed", func() {
				So(ok, ShouldBeFalse)
				So(key, ShouldBeEmpty)
			})
		})

		Convey("When the stop event is asked for", func() {
			key, ok := table.HookKey("pi", "stop")

			Convey("Then pi uses its own turn_end", func() {
				So(ok, ShouldBeTrue)
				So(key, ShouldEqual, "turn_end")
			})
		})
	})
}

// TestConsentGoldenCrossLanguage pins the D5 hash algorithm on both sides of
// the bundle: the Go hash of the shared fixture equals the hash the TS test
// asserts against.
func TestConsentGoldenCrossLanguage(t *testing.T) {
	Convey("Given the shared consent fixture", t, func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "runtime", "testdata", "consent-golden.json"))
		So(err, ShouldBeNil)

		var golden consentGolden
		So(json.Unmarshal(data, &golden), ShouldBeNil)

		Convey("When the canonical hooks are hashed in Go", func() {
			hooks := make([]manifest.Hook, 0, len(golden.Hooks))

			for _, hook := range golden.Hooks {
				hooks = append(hooks, manifest.Hook{Event: hook.Event, Matcher: hook.Matcher, Command: hook.Command, Timeout: hook.Timeout})
			}

			sum := consent.HookHash(golden.Package, golden.Version, hooks)

			Convey("Then it equals the hash the bundle's test pins", func() {
				So(sum.String(), ShouldEqual, golden.Hash)
			})
		})
	})
}

// TestInstall pins the runtime installation: the embedded bundle and table land
// in the store, the write is atomic and idempotent, and a new version lands in
// a new directory instead of overwriting a runtime a host has loaded.
func TestInstall(t *testing.T) {
	Convey("Given a store", t, func() {
		st := openStore(t)

		dir, err := runtime.Install(t.Context(), st, "opencode", runtime.BundleVersion)

		Convey("When the runtime is installed", func() {
			Convey("Then the bundle, the table and the packages dir are there", func() {
				So(err, ShouldBeNil)
				So(dir, ShouldEqual, filepath.Join(st.Root(), "runtime", "opencode", runtime.BundleVersion))
				So(runtime.Installed(st, "opencode", runtime.BundleVersion), ShouldBeTrue)
				So(fileExists(filepath.Join(dir, "index.js")), ShouldBeTrue)
				So(fileExists(filepath.Join(dir, "events.json")), ShouldBeTrue)
				So(dirExists(filepath.Join(dir, "packages")), ShouldBeTrue)
			})

			Convey("Then installing again keeps the same directory", func() {
				again, againErr := runtime.Install(t.Context(), st, "opencode", runtime.BundleVersion)

				So(againErr, ShouldBeNil)
				So(again, ShouldEqual, dir)
			})
		})

		Convey("When a host the store refuses is asked for", func() {
			_, err := runtime.Install(t.Context(), st, "not a host", runtime.BundleVersion)

			Convey("Then the store grammar refuses it", func() {
				So(err, ShouldBeError)
			})
		})

		Convey("When the caller's context is already done", func() {
			fresh := openStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := runtime.Install(ctx, fresh, "opencode", runtime.BundleVersion)

			Convey("Then nothing is written", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(runtime.Installed(fresh, "opencode", runtime.BundleVersion), ShouldBeFalse)
			})
		})
	})
}

// TestManifest pins the per-package manifest: the hooks the runtime runs and
// the script digests the consent covers.
func TestManifest(t *testing.T) {
	Convey("Given a runtime directory and a package with hooks", t, func() {
		st := openStore(t)

		dir, err := runtime.Install(t.Context(), st, "kilo", runtime.BundleVersion)
		So(err, ShouldBeNil)

		hooks := []manifest.Hook{
			{Event: "pre-tool", Matcher: "Bash", Command: "node guard.js", Timeout: 5},
			{Event: "stop", Command: "node bye.js"},
		}

		scripts := map[string]digest.Hash{"/abs/guard.js": digest.Bytes([]byte("guard"))}

		m := runtime.NewManifest("acme/caveman", "1.2.3", hooks, scripts)
		path, _, writeErr := runtime.WriteManifest(dir, m)

		Convey("When it is written", func() {
			Convey("Then it lands below packages/ with the runtime's field names", func() {
				So(writeErr, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(dir, "packages", "acme_caveman.json"))

				data, readErr := os.ReadFile(path) //nolint:gosec // G304: the path is inside the test's own temp store
				So(readErr, ShouldBeNil)
				So(string(data), ShouldContainSubstring, `"event": "pre-tool"`)
				So(string(data), ShouldContainSubstring, `"sha256"`)
			})

			Convey("Then it reads back as the same manifest", func() {
				back, readErr := runtime.ReadManifest(path)

				So(readErr, ShouldBeNil)
				So(back.Package, ShouldEqual, "acme/caveman")
				So(back.Hooks, ShouldHaveLength, 2)
				So(back.Scripts, ShouldHaveLength, 1)
			})

			Convey("Then removing it is idempotent", func() {
				So(runtime.RemoveManifest(dir, "acme/caveman"), ShouldBeNil)
				So(runtime.RemoveManifest(dir, "acme/caveman"), ShouldBeNil)
				So(fileExists(path), ShouldBeFalse)
			})
		})
	})
}

// TestManifestRecordsScriptDigests pins the Go half of the script gate: the
// manifest the runtime reads carries every hook script with its content
// digest, so a file that changes on disk after approval no longer matches.
func TestManifestRecordsScriptDigests(t *testing.T) {
	Convey("Given a hook script on disk", t, func() {
		dir := t.TempDir()
		script := filepath.Join(dir, "guard.js")

		So(os.WriteFile(script, []byte("process.exit(0)\n"), 0o600), ShouldBeNil)

		sum, err := digest.File(script)
		So(err, ShouldBeNil)

		m := runtime.NewManifest("acme/caveman", "1.0.0",
			[]manifest.Hook{{Event: "pre-tool", Matcher: "Bash", Command: "node " + script, Timeout: 5}},
			map[string]digest.Hash{script: sum})

		path, manifestSum, writeErr := runtime.WriteManifest(dir, m)
		So(writeErr, ShouldBeNil)

		Convey("Then the write returns the digest of the bytes on disk", func() {
			data, readErr := os.ReadFile(path) //nolint:gosec // G304: the path is inside the test's own temp store
			So(readErr, ShouldBeNil)
			So(manifestSum, ShouldEqual, digest.Bytes(data))
		})

		Convey("When it is written and read back", func() {
			back, readErr := runtime.ReadManifest(path)

			Convey("Then the script is recorded with its content digest", func() {
				So(readErr, ShouldBeNil)
				So(back.Scripts, ShouldHaveLength, 1)
				So(back.Scripts[0].Path, ShouldEqual, script)
				So(back.Scripts[0].SHA256, ShouldEqual, sum.String())
			})
		})

		Convey("When the script changes on disk after the manifest was written", func() {
			So(os.WriteFile(script, []byte("process.exit(2)\n"), 0o600), ShouldBeNil)

			changed, digestErr := digest.File(script)
			back, readErr := runtime.ReadManifest(path)

			Convey("Then the manifest still records the approved digest and the file no longer matches it", func() {
				So(digestErr, ShouldBeNil)
				So(readErr, ShouldBeNil)
				So(back.Scripts, ShouldHaveLength, 1)
				So(back.Scripts[0].SHA256, ShouldEqual, sum.String())
				So(back.Scripts[0].SHA256, ShouldNotEqual, changed.String())
			})
		})
	})
}

// TestShimGolden pins the generated shim of every dialect: the absolute runtime
// import, the manifest the runtime reads, and the inline deny fallback over the
// blocking keys of that dialect.
func TestShimGolden(t *testing.T) {
	Convey("Given a v2 shim", t, func() {
		table, err := runtime.LoadTable()
		So(err, ShouldBeNil)

		opts := runtime.ShimOptions{
			Host: "opencode", Dialect: "v2", Package: "acme/caveman", Version: "1.2.3",
			RuntimeDir: "/store/runtime/opencode/0.1.0", ManifestPath: "/store/runtime/opencode/0.1.0/packages/acme_caveman.json",
			ManifestSHA256: "abc123", Consent: "abc", LogPath: "/store/runtime/opencode/verger-runtime.log",
		}

		data, renderErr := runtime.RenderShim(opts, table)

		Convey("When it is rendered", func() {
			Convey("Then it imports the versioned runtime and carries both plugin forms", func() {
				So(renderErr, ShouldBeNil)

				shim := string(data)
				So(shim, ShouldContainSubstring, `const RUNTIME = "/store/runtime/opencode/0.1.0/index.js"`)
				So(shim, ShouldContainSubstring, `"manifestPath":"/store/runtime/opencode/0.1.0/packages/acme_caveman.json"`)
				So(shim, ShouldContainSubstring, `"manifestSha":"abc123"`)
				So(shim, ShouldContainSubstring, "async setup(ctx)")
				So(shim, ShouldContainSubstring, "async server()")
				So(shim, ShouldContainSubstring, `"execute.before"`)
				So(shim, ShouldContainSubstring, `"tool.execute.before"`)
				So(shim, ShouldContainSubstring, "export default plugin")
			})
		})

		Convey("When the manifest digest is missing", func() {
			unsigned := opts
			unsigned.ManifestSHA256 = ""

			_, renderErr := runtime.RenderShim(unsigned, table)

			Convey("Then the shim is refused: an unauthenticated record must not ship", func() {
				So(renderErr, ShouldBeError)
				So(renderErr.Error(), ShouldContainSubstring, "manifest sha256")
			})
		})

		Convey("When a relative path is handed in", func() {
			broken := opts
			broken.ManifestPath = "packages/acme_caveman.json"

			_, renderErr := runtime.RenderShim(broken, table)

			Convey("Then the shim is refused: the host resolves it from its own cwd", func() {
				So(renderErr, ShouldBeError)
			})
		})
	})

	Convey("Given a pi shim", t, func() {
		table, err := runtime.LoadTable()
		So(err, ShouldBeNil)

		data, renderErr := runtime.RenderShim(runtime.ShimOptions{
			Host: "pi", Dialect: "pi", Package: "acme/caveman", Version: "1.2.3",
			RuntimeDir: "/store/runtime/pi/0.1.0", ManifestPath: "/store/runtime/pi/0.1.0/packages/acme_caveman.json",
			ManifestSHA256: "abc123",
		}, table)

		Convey("When it is rendered", func() {
			Convey("Then it registers every dialect key synchronously and defers only the call", func() {
				So(renderErr, ShouldBeNil)

				shim := string(data)
				So(shim, ShouldContainSubstring, "export default function (pi)")
				So(shim, ShouldContainSubstring, "runtime.piDispatcher(OPTIONS)")

				// The registration loop is what has to be synchronous: the
				// host fires session_start while this function is still on
				// the stack, so a handler that only appears after the import
				// resolves misses it, and the miss is invisible.
				So(shim, ShouldContainSubstring, "bus?.on?.(key")

				// omp hands an extension {pi, extension, runtime, …} with no
				// top-level on, so the shim resolves the bus before it
				// registers: on the context itself it is a silent no-op.
				So(shim, ShouldContainSubstring, `typeof pi?.on === "function" ? pi : pi?.events ?? pi?.pi`)

				So(shim, ShouldContainSubstring, "await ready")

				// Both keys of the pi dialect, blocking or not: a hook on a
				// non-blocking event still has to be registered to run.
				So(shim, ShouldContainSubstring, `"tool_call"`)
				So(shim, ShouldContainSubstring, `"session_start"`)
				So(shim, ShouldContainSubstring, "block: true")
			})
		})
	})
}

// TestShimBusResolutionAgreesWithTheRuntime is the guard on the one piece of
// host knowledge the shim cannot borrow: the bus must be resolved
// synchronously, because the host fires its first event while the extension's
// default export is still on the stack. runtime/bus.ts holds the same shapes
// for the runtime's own piExtension, and the node test runs both over the same
// three contexts - so changing one without the other fails there.
func TestShimBusResolutionAgreesWithTheRuntime(t *testing.T) {
	table, tableErr := runtime.LoadTable()

	Convey("Given a renderable pi shim", t, func() {
		So(tableErr, ShouldBeNil)

		shim, err := runtime.RenderShim(runtime.ShimOptions{
			Host: "pi", Dialect: "pi", Package: "acme/probe", Version: "1.0.0",
			RuntimeDir: "/store/runtime/pi/0.1.0", ManifestPath: "/store/runtime/pi/0.1.0/package.json",
			ManifestSHA256: "abc123", Consent: "abc", LogPath: "/store/runtime/pi/verger-runtime.log",
		}, table)
		So(err, ShouldBeNil)
		So(string(shim), ShouldContainSubstring, `const bus = typeof pi?.on === "function" ? pi : pi?.events ?? pi?.pi`)

		Convey("Then the dialect is registered on the resolved bus, never on the context", func() {
			// Registering on the context is the original bug and it is silent:
			// omp's context has no top-level on, so the optional call is a no-op
			// and the hook simply never runs.
			So(string(shim), ShouldNotContainSubstring, "pi?.on?.(")
			So(string(shim), ShouldContainSubstring, "bus?.on?.(key,")

			// and the resolution precedes the registration: done after the import
			// it would miss the host's first event.
			So(strings.Index(string(shim), "const bus =") < strings.Index(string(shim), "for (const [key, canon]"), ShouldBeTrue)
		})
	})
}

// TestHeartbeat pins the health signal: a runtime that ran is healthy for a
// window, and the previous version is the rollback target.
func TestHeartbeat(t *testing.T) {
	Convey("Given two installed runtime versions", t, func() {
		st := openStore(t)
		now := time.Now()

		current, err := runtime.Install(t.Context(), st, "opencode", "0.2.0")
		So(err, ShouldBeNil)

		previous, prevErr := runtime.Install(t.Context(), st, "opencode", "0.1.0")
		So(prevErr, ShouldBeNil)

		Convey("When neither has run", func() {
			Convey("Then neither is healthy and there is no rollback target", func() {
				So(runtime.Healthy(current, now, time.Hour), ShouldBeFalse)

				_, ok := runtime.PreviousVersion(st, "opencode", "0.2.0", now, time.Hour)
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When the previous version ran a minute ago", func() {
			So(runtime.WriteHeartbeat(previous, runtime.Heartbeat{
				At: now.Add(-time.Minute), PID: 42, Host: "opencode", Dialect: "v2", Package: "acme/caveman",
			}), ShouldBeNil)

			Convey("Then it is the rollback target and the new one is not healthy", func() {
				version, ok := runtime.PreviousVersion(st, "opencode", "0.2.0", now, time.Hour)

				So(ok, ShouldBeTrue)
				So(version, ShouldEqual, "0.1.0")
				So(runtime.Healthy(previous, now, time.Hour), ShouldBeTrue)
				So(runtime.Healthy(current, now, time.Hour), ShouldBeFalse)
			})
		})

		Convey("When the new version ran and the previous one is stale", func() {
			So(runtime.WriteHeartbeat(current, runtime.Heartbeat{At: now, Host: "opencode"}), ShouldBeNil)
			So(runtime.WriteHeartbeat(previous, runtime.Heartbeat{At: now.Add(-48 * time.Hour), Host: "opencode"}), ShouldBeNil)

			Convey("Then the stale runtime is not offered as a healthy rollback", func() {
				version, ok := runtime.PreviousVersion(st, "opencode", "0.2.0", now, time.Hour)

				So(ok, ShouldBeTrue)
				So(version, ShouldEqual, "0.1.0")
				So(runtime.Healthy(previous, now, time.Hour), ShouldBeFalse)
			})
		})
	})
}

// TestBundleIsSelfContained pins the two shapes the bundle must never grow:
// imports of a package it does not carry, and a require() call. The hosts load
// it with Bun and with Node without an install step.
func TestBundleIsSelfContained(t *testing.T) {
	Convey("Given the embedded bundle", t, func() {
		data, err := runtime.Bundle()
		So(err, ShouldBeNil)

		bundle := string(data)

		Convey("When its imports are listed", func() {
			Convey("Then only node: builtins are imported", func() {
				for line := range strings.SplitSeq(bundle, "\n") {
					trimmed := strings.TrimSpace(line)

					if !strings.HasPrefix(trimmed, "import ") {
						continue
					}

					So(trimmed, ShouldContainSubstring, "node:")
				}
			})

			Convey("Then it never requires anything at runtime", func() {
				So(bundle, ShouldNotContainSubstring, "require(")
			})
		})
	})
}

// TestTheBundleTestsAreActuallyRunnable pins the wiring, not the code: the
// runtime's own tests run the BUILT bundle and are the only tests that can see
// a build which broke the published shape. Their npm script used a glob this
// node does not expand, so `npm test` found no file at all, exited quietly, and
// every mutation to the dispatch log survived a green gate. A test that only
// exists but never runs is not a test.
func TestTheBundleTestsAreActuallyRunnable(t *testing.T) {
	Convey("Given the runtime package", t, func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "runtime", "package.json"))
		So(err, ShouldBeNil)

		var doc struct {
			Scripts map[string]string `json:"scripts"`
		}

		So(json.Unmarshal(raw, &doc), ShouldBeNil)
		So(doc.Scripts, ShouldContainKey, "test")

		Convey("Then the test script names a path node resolves", func() {
			// A quoted glob is the trap: node 20 takes it literally, finds
			// nothing, and reports success.
			test := doc.Scripts["test"]
			So(test, ShouldNotContainSubstring, "**")
			So(test, ShouldContainSubstring, "test/")
		})

		Convey("Then make test runs them", func() {
			makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
			So(err, ShouldBeNil)

			// A target nothing calls is a target that does not run.
			So(string(makefile), ShouldContainSubstring, "test: test-runtime")
		})
	})
}
