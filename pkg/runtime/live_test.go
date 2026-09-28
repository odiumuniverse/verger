//go:build live

// Live proofs for the host runtime. They need the real host CLIs on PATH and
// an isolated HOME per host; they never call a model. Run with:
//
//	go test -tags live -run TestLive ./pkg/runtime/ -v
//
// The proof is the runtime's own heartbeat: the shim is placed where the host
// discovers it, the host is asked to load it, and the heartbeat document the
// bundle writes on load is what the test reads back — not a marker the test
// planted.
package runtime_test

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

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/runtime"
	"github.com/odiumuniverse/verger/pkg/store"
)

// liveHome prepares an isolated HOME for one host and returns it.
func liveHome(t *testing.T, host string) string {
	t.Helper()

	home := t.TempDir()

	for _, dir := range []string{
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".config", "kilo", "plugin"),
		filepath.Join(home, ".pi", "agent", "extensions"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// opencode starts a background service; a port of its own keeps it from
	// colliding with the developer's instance and with a previous live run
	// whose service outlived its HOME.
	if host == "opencode" {
		port := 49500 + rand.IntN(400) //nolint:gosec // G404: a port picker, not a secret
		runLive(t, home, 30*time.Second, "opencode", "service", "set", "port", strconv.Itoa(port))

		t.Cleanup(func() { runLive(t, home, 30*time.Second, "opencode", "service", "stop") })
	}

	return home
}

// runLive runs one host CLI in an isolated HOME and returns its output.
func runLive(t *testing.T, home string, timeout time.Duration, name string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// The whole XDG set is pinned: the suite's TestMain isolates the test
	// process, and a host that inherited XDG_DATA_HOME from it would keep its
	// service state and logs outside this HOME.
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
	)
	cmd.Stdin = strings.NewReader("")

	out, err := cmd.CombinedOutput()

	return string(out) + "\n" + errString(err)
}

// errString renders a command error without hiding the output.
func errString(err error) string {
	if err == nil {
		return ""
	}

	return "error: " + err.Error()
}

// waitHeartbeat waits for the runtime's own heartbeat inside dir.
func waitHeartbeat(t *testing.T, dir string, timeout time.Duration) runtime.Heartbeat {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		hb, ok, err := runtime.ReadHeartbeat(dir)
		if err == nil && ok {
			return hb
		}

		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("no heartbeat in %s after %s", dir, timeout)

	return runtime.Heartbeat{}
}

// liveRuntime installs the runtime and writes one manifest with a hook.
func liveRuntime(t *testing.T, host, dialect string) (*store.Store, string, string) {
	t.Helper()

	st := openStore(t)

	dir, err := runtime.Install(t.Context(), st, host, runtime.BundleVersion)
	if err != nil {
		t.Fatalf("install runtime: %v", err)
	}

	script := filepath.Join(t.TempDir(), "guard.js")
	if err := os.WriteFile(script, []byte("process.exit(0)\n"), 0o700); err != nil { //nolint:gosec // G306: a hook script the host executes
		t.Fatalf("write hook script: %v", err)
	}

	hooks := []manifest.Hook{{Event: "pre-tool", Matcher: "Bash", Command: "node " + script, Timeout: 5}}

	m := runtime.NewManifest("acme/live-fixture", "1.0.0", hooks, nil)

	manifestPath, manifestSum, err := runtime.WriteManifest(dir, m)
	if err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	consent, err := os.ReadFile(filepath.Join("..", "..", "runtime", "testdata", "consent-golden.json"))
	if err != nil {
		t.Fatalf("read consent fixture: %v", err)
	}

	var golden struct {
		Hash string `json:"hash"`
	}

	if err := json.Unmarshal(consent, &golden); err != nil {
		t.Fatalf("decode consent fixture: %v", err)
	}

	// The live shim carries the hash of the *fixture* hooks: the runtime will
	// report a consent mismatch in its log, which is fine here — the proof is
	// that the host loaded it at all. A delivery passes its own hash.
	shim, err := runtime.RenderShim(runtime.ShimOptions{
		Host: host, Dialect: dialect, Package: "acme/live-fixture", Version: "1.0.0",
		RuntimeDir: dir, ManifestPath: manifestPath, ManifestSHA256: manifestSum.String(), Consent: golden.Hash,
		LogPath: filepath.Join(dir, "verger-runtime.log"),
	}, mustTable(t))
	if err != nil {
		t.Fatalf("render shim: %v", err)
	}

	return st, dir, string(shim)
}

// mustTable loads the embedded table.
func mustTable(t *testing.T) runtime.Table {
	t.Helper()

	table, err := runtime.LoadTable()
	if err != nil {
		t.Fatalf("load table: %v", err)
	}

	return table
}

// writeShim writes one shim file (or package) and returns its path.
func writeShim(t *testing.T, path, shim string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(shim), 0o600); err != nil {
		t.Fatalf("write shim: %v", err)
	}
}

// TestLiveOpenCodeLoadsShim proves opencode 2.0.18 loads the generated shim as
// a server plugin: the module is declared in the isolated config, the host is
// asked to boot its service, and the runtime's own heartbeat plus the host's
// own `loading plugin` log line are what the test reads back.
//
// The host's `plugin list` is deliberately not asserted: in an isolated HOME it
// answers from the registry of the service that was already running, and both
// the CLI listing and `/api/plugin` were observed to lag or drop a plugin the
// log had just loaded (W1-A §3.4 has the same observation for deliveries).
func TestLiveOpenCodeLoadsShim(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not on PATH")
	}

	Convey("Given an isolated opencode 2.0.18 HOME with a verger shim", t, func() {
		home := liveHome(t, "opencode")
		_, dir, shim := liveRuntime(t, "opencode", "v2")

		configDir := filepath.Join(home, ".config", "opencode")
		shimDir := filepath.Join(configDir, "verger-live-fixture")

		writeShim(t, filepath.Join(shimDir, "index.js"), shim)
		writeShim(t, filepath.Join(shimDir, "package.json"),
			`{"name":"verger-live-fixture","version":"1.0.0","type":"module","main":"index.js"}`)

		writeShim(t, filepath.Join(configDir, "opencode.json"),
			`{"plugins":["`+shimDir+`"]}`)

		out := runLive(t, home, 90*time.Second, "opencode", "plugin", "list")
		t.Logf("plugin list: %s", strings.TrimSpace(out))

		Convey("When the host boots its plugin service", func() {
			hb := waitHeartbeat(t, dir, 30*time.Second)

			logPath := filepath.Join(home, ".local", "share", "opencode", "log", "opencode.log")
			log := waitLog(t, logPath, "loading plugin", 20*time.Second)

			Convey("Then the runtime ran inside the host and the host reports loading it", func() {
				So(hb.Host, ShouldEqual, "opencode")
				So(hb.Dialect, ShouldEqual, "v2")
				So(log, ShouldContainSubstring, "loading plugin")
				So(log, ShouldContainSubstring, shimDir)
			})
		})
	})
}

// waitLog waits for one host log line; the host's service writes its log
// asynchronously, so a line can land after the CLI that started it returned.
func waitLog(t *testing.T, path, needle string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var seen string

	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // G304: the path is inside the test's own temp HOME
		if err == nil {
			seen = string(data)

			if strings.Contains(seen, needle) {
				return seen
			}
		}

		time.Sleep(250 * time.Millisecond)
	}

	return seen
}

// TestLiveKiloLoadsShim proves kilo 7.8.1 loads the generated v1 shim from its
// plugin directory.
func TestLiveKiloLoadsShim(t *testing.T) {
	if _, err := exec.LookPath("kilo"); err != nil {
		t.Skip("kilo is not on PATH")
	}

	Convey("Given an isolated kilo 7.8.1 HOME with a verger shim", t, func() {
		home := liveHome(t, "kilo")
		_, dir, shim := liveRuntime(t, "kilo", "v1")

		writeShim(t, filepath.Join(home, ".config", "kilo", "plugin", "verger-live-fixture.ts"), shim)

		runLive(t, home, 90*time.Second, "kilo", "debug", "config")

		Convey("When the host resolves its configuration", func() {
			hb := waitHeartbeat(t, dir, 30*time.Second)

			Convey("Then the runtime ran inside the host", func() {
				So(hb.Host, ShouldEqual, "kilo")
				So(hb.Dialect, ShouldEqual, "v1")
			})
		})
	})
}

// TestLivePiLoadsShim proves pi 0.74.2 loads the generated extension. RPC mode
// boots the extension host and exits on a closed stdin without any model call.
func TestLivePiLoadsShim(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not on PATH")
	}

	Convey("Given an isolated pi HOME with a verger extension", t, func() {
		home := liveHome(t, "pi")
		_, dir, shim := liveRuntime(t, "pi", "pi")

		writeShim(t, filepath.Join(home, ".pi", "agent", "extensions", "verger-live-fixture.ts"), shim)

		runLive(t, home, 60*time.Second, "pi", "--mode", "rpc")

		Convey("When pi boots its extension host", func() {
			hb := waitHeartbeat(t, dir, 30*time.Second)

			Convey("Then the runtime ran inside the host", func() {
				So(hb.Host, ShouldEqual, "pi")
				So(hb.Dialect, ShouldEqual, "pi")
			})
		})
	})
}
