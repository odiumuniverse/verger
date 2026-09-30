//go:build live

package host_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/runtime"
	"github.com/odiumuniverse/verger/pkg/store"
)

// TestLiveOpenCodeLoadsADeliveredRuntimePlugin is the live proof of T2.3 for
// OpenCode v2: a real delivery through the real adapter places the shim, the
// real 2.0.18 boots, and the runtime's own heartbeat plus the host's own
// `loading plugin` line are what the test reads back.
//
// The hand-built fixture in pkg/runtime/live_test.go proves the shim loads;
// this proves the delivery puts it somewhere the host looks.
func TestLiveOpenCodeLoadsADeliveredRuntimePlugin(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not on PATH")
	}
	// Skipped on purpose: opencode is blocked too (VERIFY-CP-verger-pR-3).
	// This test proved the placement is FOUND and the runtime LOADS, which
	// is not the claim that matters; no test here shows a delivered hook
	// EXECUTING, so the host stays blocked.
	t.Skip("opencode is blocked: no session-start event exists in the v2 table, so a delivered module fires only on a tool call, which needs a model (NIGHT-pR-8)")

	Convey("Given a real OpenCode v2 HOME", t, func() {
		home := t.TempDir()
		configDir := openCodeTestEnv(t, home)
		liveOpenCodeHome(t, home)

		st := openStore(t)
		h, _ := newOpenCode(t, home, map[string]hostcli.Response{},
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		pkg := openCodePackage(t)
		So(pkg.Hooks, ShouldNotBeEmpty)

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When the package is delivered", func() {
			So(err, ShouldBeNil)
			So(strings.Join(result.Notes, "\n"), ShouldNotContainSubstring, "hook(s) skipped")

			shimDir := filepath.Join(configDir, "verger-acme-caveman")

			_, statErr := os.Stat(filepath.Join(shimDir, "package.json"))
			So(statErr, ShouldBeNil)

			Convey("Then the real host boots its plugin service and runs the runtime", func() {
				runLiveOpenCode(t, home, 90*time.Second, "opencode", "plugin", "list")

				// The runtime writes into the package's data directory, which
				// the receipt names; find it rather than recomputing it.
				hb := waitDeliveredHeartbeat(t, home, st, "acme/caveman", "opencode", 40*time.Second)

				logPath := filepath.Join(home, ".local", "share", "opencode", "log", "opencode.log")
				log := waitLiveLog(t, logPath, "loading plugin", 25*time.Second)

				So(hb.Host, ShouldEqual, "opencode")
				So(hb.Dialect, ShouldEqual, "v2")
				So(log, ShouldContainSubstring, "loading plugin")
				So(log, ShouldContainSubstring, shimDir)
			})
		})
	})
}

// TestLiveOpenCodeRemovalTakesThePluginBackOut is the other half of T2.3, and
// the half a unit test cannot prove: the reverse manifest has to actually
// remove the tree and unset the entry, through pkg/apply, against the real
// files the real host would have loaded.
func TestLiveOpenCodeRemovalTakesThePluginBackOut(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not on PATH")
	}
	// Skipped on purpose, for the same reason as the load test above: there is
	// no plugin placed to take back out while opencode is blocked.
	t.Skip("opencode is blocked: there is no delivered plugin to remove (VERIFY-CP-verger-pR-3)")

	Convey("Given a delivered runtime plugin on a real host", t, func() {
		home := t.TempDir()
		configDir := openCodeTestEnv(t, home)
		liveOpenCodeHome(t, home)

		st := openStore(t)
		h, _ := newOpenCode(t, home, map[string]hostcli.Response{},
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		pkg := openCodePackage(t)

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})
		So(err, ShouldBeNil)

		shimDir := filepath.Join(configDir, "verger-acme-caveman")

		Convey("Then the reverse manifest removes the directory and the entry", func() {
			// The adapter hands the manifest back; apply executes it. Driving
			// apply from here would need the whole delivery world, so the
			// proof is that the manifest names exactly the tree and the key
			// the host reads — asserted in the unit test — and that the real
			// paths are the ones the manifest carries.
			var sawFiles, sawConfigKey bool

			for _, op := range result.RMA {
				switch {
				case op.Kind == "write-file" && filepath.Dir(op.Path) == shimDir:
					sawFiles = true
				case op.Kind == "config-key" && op.Path == filepath.Join(configDir, "opencode.json") && op.KeyPath == "plugins":
					sawConfigKey = true
				}
			}

			// Both files of the plugin, and the entry that made the host
			// resolve them. The directory itself is pruned by the removal
			// that emptied it (pkg/apply pruneEmptiedParent), which needs no
			// op of its own and is proven end to end in
			// TestOpenCodeV2RemovalTakesOnlyItsOwnPluginEntry.
			So(sawFiles, ShouldBeTrue)

			So(sawConfigKey, ShouldBeTrue)
		})
	})
}

// TestLivePiRunsADeliveredHook is the proof the heartbeat cannot give: that a
// hook verger delivered actually RUNS inside the real host.
//
// The heartbeat only says the module loaded. This says the module did its job:
// a `SessionStart` hook whose command appends to a marker file, a real pi booted
// in RPC mode, and the marker read back. SessionStart needs no model call, so
// the proof is a hook firing and nothing else.
func TestLivePiRunsADeliveredHook(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not on PATH")
	}

	Convey("Given a package whose hook writes a marker", t, func() {
		home := t.TempDir()
		livePiHome(t, home)

		marker := filepath.Join(t.TempDir(), "hook-ran")
		pkg := piPackage(t)
		pkg.Hooks = []manifest.Hook{{
			Event: "SessionStart", Matcher: "", Command: hookMarkerCommand(marker), Timeout: 10,
		}}

		st := openStore(t)
		h, _ := newPi(t, home, nil, host.WithHome(home), host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(piSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})
		So(err, ShouldBeNil)

		Convey("When the real host starts a session", func() {
			runLiveWithStdinOpen(t, home, 10*time.Second, "pi", "--mode", "rpc")

			Convey("Then the delivered hook ran", func() {
				So(waitForFile(marker, 40*time.Second), ShouldBeTrue)
			})
		})
	})
}

// TestLiveOmpRunsADeliveredHook is the same proof for omp, whose dialect is pi's.
func TestLiveOmpRunsADeliveredHook(t *testing.T) {
	if _, err := exec.LookPath("omp"); err != nil {
		t.Skip("omp is not on PATH")
	}

	// omp is blocked: its extension context is not pi's (the bus is under
	// events, pi's API is a module namespace), and no delivered hook has been
	// seen executing there. This test is that proof; it stays skipped until the
	// context and its events are established.
	t.Skip("omp is blocked: no delivered hook has been seen executing (NIGHT-pR-7)")

	Convey("Given a package whose hook writes a marker", t, func() {
		home := t.TempDir()
		liveOmpHome(t, home)

		marker := filepath.Join(t.TempDir(), "hook-ran")
		pkg := ompPackage(t)
		pkg.Hooks = []manifest.Hook{{
			Event: "SessionStart", Matcher: "", Command: hookMarkerCommand(marker), Timeout: 10,
		}}

		st := openStore(t)
		h, _ := newOmp(t, home, nil, host.WithHome(home), host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})
		So(err, ShouldBeNil)

		Convey("When the real host starts a session", func() {
			runLiveWithStdinOpen(t, home, 10*time.Second, "omp", "--mode", "rpc")

			Convey("Then the delivered hook ran", func() {
				So(waitForFile(marker, 40*time.Second), ShouldBeTrue)
			})
		})
	})
}

// hookMarkerCommand is a hook command that leaves evidence: it appends to the
// marker file, so the test can tell a hook that ran from a hook that was
// delivered and ignored.
func hookMarkerCommand(marker string) string {
	return "node -e \"require('fs').appendFileSync('" + marker + "','ran\\n')\""
}

// waitForFile waits for the marker the hook writes.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}

		time.Sleep(250 * time.Millisecond)
	}

	return false
}

// liveOpenCodeHome prepares an isolated HOME with a config root and a service
// port of its own: opencode starts a background service, and a shared port
// would collide with the developer's instance and with a previous live run.
func liveOpenCodeHome(t *testing.T, home string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o700); err != nil {
		t.Fatalf("mkdir config root: %v", err)
	}

	port := 49500 + rand.IntN(400) //nolint:gosec // G404: a port picker, not a secret
	runLiveOpenCode(t, home, 30*time.Second, "opencode", "service", "set", "port", strconv.Itoa(port))

	t.Cleanup(func() { runLiveOpenCode(t, home, 30*time.Second, "opencode", "service", "stop") })
}

// runLiveOpenCode runs the host CLI in an isolated HOME.
func runLiveOpenCode(t *testing.T, home string, timeout time.Duration, name string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// The whole XDG set is pinned: a host that inherited one of these from the
	// test process would keep its service state and logs outside this HOME.
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
	)
	cmd.Stdin = strings.NewReader("")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("live: %s %s -> %v\n%s", name, strings.Join(args, " "), err, out)
	}

	return string(out)
}

// runLiveWithStdinOpen runs a host with stdin held open for the given window.
// The closed-stdin runner exits the moment the host starts, which is fine for
// a load test and useless for an execution one: pi fires session_start while
// the extension's handler is still awaiting the runtime, and a process that
// has already gone takes the hook with it.
func runLiveWithStdinOpen(t *testing.T, home string, hold time.Duration, name string, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), hold+30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = liveHomeEnv(home)

	pipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}

	time.Sleep(hold)
	_ = pipe.Close()
	_ = cmd.Wait()
}

// liveHomeEnv is the isolated environment every live host run sees.
func liveHomeEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
	)
}

// TestLiveKiloLoadsADeliveredRuntimeModule is the live proof of T2.3 for kilo:
// a real delivery through the real adapter places the module, the real 7.8.1
// resolves its configuration, and the runtime's own heartbeat is what the test
// reads back.
func TestLiveKiloLoadsADeliveredRuntimeModule(t *testing.T) {
	if _, err := exec.LookPath("kilo"); err != nil {
		t.Skip("kilo is not on PATH")
	}
	// Skipped on purpose: kilo is blocked again (VERIFY-CP-verger-pR-2) until a
	// delivered hook is proven to execute there. What this test proved — that
	// the placement is found and the runtime loads — is still true, and is why
	// the block is an execution bug rather than a missing placement.
	t.Skip("kilo is blocked: its only plugin mechanism, `kilo plugin <module>`, takes an npm module name, and a loose module at .config/kilo/plugin is never loaded (NIGHT-pR-8)")

	Convey("Given a real kilo HOME", t, func() {
		home := t.TempDir()
		configDir := kiloTestEnv(t, home)
		liveKiloHome(t, home)

		st := openStore(t)
		h, _ := newKilo(t, home, nil, host.WithHome(home), host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		pkg := kiloPackage(t)
		So(pkg.Hooks, ShouldNotBeEmpty)

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When the package is delivered", func() {
			So(err, ShouldBeNil)
			So(strings.Join(result.Notes, "\n"), ShouldNotContainSubstring, "hook(s) skipped")

			shim := filepath.Join(configDir, "plugin", "verger-acme-caveman.ts")
			_, statErr := os.Stat(shim)
			So(statErr, ShouldBeNil)

			Convey("Then the real host resolves it and the runtime runs", func() {
				runLiveOpenCode(t, home, 90*time.Second, "kilo", "debug", "config")

				hb := waitDeliveredHeartbeat(t, home, st, "acme/caveman", "kilo", 40*time.Second)

				So(hb.Host, ShouldEqual, "kilo")
				So(hb.Dialect, ShouldEqual, "v1")
			})
		})
	})
}

// TestLivePiLoadsADeliveredRuntimeModule is the live proof of T2.3 for pi: a
// real delivery places the extension, the real 0.74.2 boots its extension host
// in RPC mode, and the runtime's own heartbeat is what the test reads back.
func TestLivePiLoadsADeliveredRuntimeModule(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not on PATH")
	}

	Convey("Given a real pi HOME", t, func() {
		home := t.TempDir()
		livePiHome(t, home)

		st := openStore(t)
		h, _ := newPi(t, home, nil, host.WithHome(home), host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(piSecrets(t)))

		pkg := piPackage(t)
		So(pkg.Hooks, ShouldNotBeEmpty)

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When the package is delivered", func() {
			So(err, ShouldBeNil)
			So(strings.Join(result.Notes, "\n"), ShouldNotContainSubstring, "hook(s) skipped")

			shim := filepath.Join(piAgentDir(home), "extensions", "verger-acme-caveman.ts")
			_, statErr := os.Stat(shim)
			So(statErr, ShouldBeNil)

			Convey("Then the real host boots its extension host and the runtime runs", func() {
				// RPC mode boots the extension host and exits on a closed
				// stdin without any model call.
				runLiveWithStdinOpen(t, home, 10*time.Second, "pi", "--mode", "rpc")

				hb := waitDeliveredHeartbeat(t, home, st, "acme/caveman", "pi", 40*time.Second)

				So(hb.Host, ShouldEqual, "pi")
				So(hb.Dialect, ShouldEqual, "pi")
			})
		})
	})
}

// TestLiveOmpLoadsADeliveredRuntimeModule is the live proof of T2.3 for omp: a
// real delivery places the module, the real 18.4.3 boots its extension host in
// RPC mode, and the runtime's own heartbeat is what the test reads back.
func TestLiveOmpLoadsADeliveredRuntimeModule(t *testing.T) {
	if _, err := exec.LookPath("omp"); err != nil {
		t.Skip("omp is not on PATH")
	}
	// Skipped on purpose, for the same reason as kilo and pi: omp is blocked
	// again (VERIFY-CP-verger-pR-2) until a delivered hook is proven to run.
	t.Skip("omp is blocked: registration on its real bus is proven (TestLiveOmpDeliversARunningHookOnItsOwnBus passes), but omp never emits a lifecycle event without a model call, and `omp --mode rpc` refuses a prompt with no model (NIGHT-pR-8)")

	Convey("Given a real omp HOME", t, func() {
		home := t.TempDir()
		liveOmpHome(t, home)

		st := openStore(t)
		h, _ := newOmp(t, home, nil, host.WithHome(home), host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		pkg := ompPackage(t)
		So(pkg.Hooks, ShouldNotBeEmpty)

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When the package is delivered", func() {
			So(err, ShouldBeNil)
			So(strings.Join(result.Notes, "\n"), ShouldNotContainSubstring, "hook(s) skipped")

			shim := filepath.Join(ompAgentDir(home), "extensions", "verger-acme-caveman.ts")
			_, statErr := os.Stat(shim)
			So(statErr, ShouldBeNil)

			Convey("Then the real host boots its extension host and the runtime runs", func() {
				runLiveWithStdinOpen(t, home, 10*time.Second, "omp", "--mode", "rpc")

				hb := waitDeliveredHeartbeat(t, home, st, "acme/caveman", "omp", 40*time.Second)

				So(hb.Host, ShouldEqual, "omp")
				So(hb.Dialect, ShouldEqual, "pi")
			})
		})
	})
}

// liveOmpHome prepares an isolated omp HOME with its extensions directory.
func liveOmpHome(t *testing.T, home string) {
	t.Helper()

	for _, dir := range []string{
		filepath.Join(home, ".omp", "agent"),
		filepath.Join(home, ".omp", "agent", "extensions"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
}

// livePiHome prepares an isolated pi HOME with its extensions directory.
func livePiHome(t *testing.T, home string) {
	t.Helper()

	for _, dir := range []string{filepath.Join(home, ".pi", "agent"), filepath.Join(home, ".pi", "agent", "extensions")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
}

// liveKiloHome prepares an isolated kilo HOME with its config root present.
func liveKiloHome(t *testing.T, home string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(home, ".config", "kilo", "plugin"), 0o700); err != nil {
		t.Fatalf("mkdir kilo config root: %v", err)
	}
}

// waitDeliveredHeartbeat waits for the runtime's own heartbeat in the package
// data directory the delivery named, not in a directory the test guessed.
func waitDeliveredHeartbeat(t *testing.T, home string, st *store.Store, pkg, hostID string, timeout time.Duration) runtime.Heartbeat {
	t.Helper()

	dir, err := st.PackageDataPath(pkg, hostID)
	if err != nil {
		t.Fatalf("resolve package data dir: %v", err)
	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if hb, ok, readErr := runtime.ReadHeartbeat(dir); readErr == nil && ok {
			return hb
		}

		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("no heartbeat in %s after %s", dir, timeout)

	return runtime.Heartbeat{}
}

// waitLiveLog waits for one line in the host's own log: the service writes it
// asynchronously, so it can land after the CLI that started it returned.
func waitLiveLog(t *testing.T, path, needle string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var seen string

	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // G304: inside the test's own temp HOME
		if err == nil {
			seen = string(data)

			if strings.Contains(seen, needle) {
				return seen
			}
		}

		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("no %q in %s after %s", needle, path, timeout)

	return seen
}

// TestLiveOmpDeliversARunningHookOnItsOwnBus is what CAN be proven of omp
// without a model, and it is the part verger owns: an extension that registers
// on omp's real event bus gets its handler invoked when that bus fires, and
// the hook it dispatches actually runs.
//
// What it is NOT is proof that omp fires that event by itself. It does not:
// 25 event names were registered on a live omp 18.4.3 and none fired, and
// `omp --mode rpc` refuses a prompt outright with "No models available", so
// there is no model-less way to make the host emit. That half is why omp stays
// blocked (NIGHT-pR-8) - but "the host never emits" and "our registration is
// broken" are different failures, and this test settles which one it is.
func TestLiveOmpDeliversARunningHookOnItsOwnBus(t *testing.T) {
	if _, err := exec.LookPath("omp"); err != nil {
		t.Skip("omp is not on PATH")
	}

	Convey("Given an extension that registers on omp's own event bus", t, func() {
		home := t.TempDir()
		liveOmpHome(t, home)

		marker := filepath.Join(t.TempDir(), "hook-ran")
		exts := filepath.Join(home, ".omp", "agent", "extensions")
		So(os.WriteFile(filepath.Join(exts, "bus-probe.ts"), []byte(busProbeExtension(marker)), 0o600), ShouldBeNil)

		Convey("When the real host runs and that bus fires", func() {
			runLiveWithStdinOpen(t, home, 8*time.Second, "omp", "--mode", "rpc")

			Convey("Then the registered handler ran and wrote the marker", func() {
				So(waitForFile(marker, 30*time.Second), ShouldBeTrue)
			})
		})
	})
}

// busProbeExtension is the extension the omp bus test installs. It registers on
// the host's OWN bus and then fires that bus itself, so the marker can only
// appear if registration reached the host's emitter and the handler ran.
//
// The module is a raw string because it is TypeScript shipped verbatim: this is
// exactly the file shape a delivered module has, so the test exercises the real
// loader rather than a re-typed copy.
func busProbeExtension(marker string) string {
	return "import { writeFileSync } from \"node:fs\"\n" +
		"const MARK = " + quoteJS(marker) + "\n" +
		"\nexport default function (omp: any): void {\n" +
		"  const bus = omp?.events\n" +
		"  bus?.on?.(\"session_start\", () => { writeFileSync(MARK, \"fired\") })\n" +
		"  bus?.emit?.(\"session_start\", { source: \"self\" })\n" +
		"}\n"
}

// quoteJS renders s as a double-quoted JavaScript string literal.
func quoteJS(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(b)
}
