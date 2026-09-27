package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// ---- fake host ---------------------------------------------------------------

// fakeFile is one artifact the fake host writes.
type fakeFile struct {
	Path string
	Data string
}

// fakeHost is a minimal scripted host.Host for CLI tests.
type fakeHost struct {
	id host.ID

	mu             sync.Mutex
	files          []fakeFile
	listed         []host.Installed
	oracleErr      error
	deliverErr     error
	delivers       int
	uninstall      int
	probe          string
	probeOK        map[string]bool
	lastAllowHooks bool
}

func newFakeHost(id host.ID) *fakeHost {
	return &fakeHost{id: id, probeOK: map[string]bool{}}
}

// ID implements host.Host.
func (f *fakeHost) ID() host.ID { return f.id }

// Detect implements host.Host.
func (f *fakeHost) Detect(string) bool { return true }

// Oracle implements host.Host.
func (f *fakeHost) Oracle() host.Oracle { return &fakeOracle{host: f} }

// Deliver implements host.Host.
func (f *fakeHost) Deliver(_ context.Context, _ string, d host.Delivery) (host.Result, error) {
	f.mu.Lock()
	f.delivers++
	f.lastAllowHooks = d.AllowHooks
	targets := append([]fakeFile(nil), f.files...)
	err := f.deliverErr

	if f.probe != "" {
		_, statErr := os.Lstat(f.probe)
		f.probeOK[d.Package.ID] = statErr == nil
	}
	f.mu.Unlock()

	if err != nil {
		return host.Result{}, err
	}

	if d.DryRun {
		result := host.Result{Strategy: d.Strategy, Notes: []string{"dry-run"}, Observed: host.OracleResult{Verified: true}}

		for _, target := range targets {
			result.Artifacts = append(result.Artifacts, receipt.Artifact{
				Kind: "skill", Name: filepath.Base(target.Path), Path: target.Path, Digest: digest.Bytes([]byte(target.Data)),
			})
			result.RMA = append(result.RMA, receipt.Op{
				Kind: receipt.OpWriteFile, Path: target.Path, Digest: digest.Bytes([]byte(target.Data)), Mode: 0o600,
			})
		}

		return result, nil
	}

	result := host.Result{Strategy: d.Strategy, Observed: host.OracleResult{Verified: true}}

	for _, target := range targets {
		if err := fsutil.EnsureDir(filepath.Dir(target.Path), 0o700); err != nil {
			return host.Result{}, err
		}

		if err := fsutil.WriteFileAtomic(target.Path, []byte(target.Data), 0o600); err != nil {
			return host.Result{}, err
		}

		sum := digest.Bytes([]byte(target.Data))
		result.Artifacts = append(result.Artifacts, receipt.Artifact{
			Kind: "skill", Name: filepath.Base(target.Path), Path: target.Path, Digest: sum,
		})
		result.RMA = append(result.RMA, receipt.Op{
			Kind: receipt.OpWriteFile, Path: target.Path, Digest: sum, Mode: 0o600,
		})
	}

	return result, nil
}

// Uninstall implements host.Host.
func (f *fakeHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	f.mu.Lock()
	f.uninstall++
	f.mu.Unlock()

	return host.Result{}, nil
}

// fakeOracle is the fake host's oracle.
type fakeOracle struct {
	host *fakeHost
}

// List implements host.Oracle.
func (o *fakeOracle) List(context.Context) ([]host.Installed, error) {
	o.host.mu.Lock()
	defer o.host.mu.Unlock()

	return append([]host.Installed(nil), o.host.listed...), o.host.oracleErr
}

// Validate implements host.Oracle.
func (o *fakeOracle) Validate(context.Context, string) ([]string, error) { return nil, nil }

// ---- world -------------------------------------------------------------------

// fixedNow is the deterministic clock the CLI tests use.
func fixedNow() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }

// world is one CLI test world: temp HOME/XDG, a project dir and a fake host.
type world struct {
	root    string
	homeDir string // <home>/.verger
	dataDir string // $XDG_DATA_HOME/verger
	project string

	fake  *fakeHost
	hosts []host.Host
	tty   bool
	in    string

	out bytes.Buffer
	err bytes.Buffer
}

// newWorld builds the temp world and pins the home-shaped environment.
func newWorld(t *testing.T) *world {
	t.Helper()

	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	data := filepath.Join(root, "data")
	project := filepath.Join(root, "project")

	for _, dir := range []string{userHome, data, project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	for name, value := range map[string]string{
		"HOME":              userHome,
		"XDG_DATA_HOME":     data,
		"XDG_CONFIG_HOME":   filepath.Join(root, "config"),
		"VERGER_HOME":       "",
		"BEADLE_HOME":       "",
		"CLAUDE_CONFIG_DIR": "",
		"CODEX_HOME":        "",
	} {
		t.Setenv(name, value)
	}

	fake := newFakeHost(host.Claude)

	return &world{
		root:    root,
		homeDir: filepath.Join(userHome, ".verger"),
		dataDir: filepath.Join(data, "verger"),
		project: project,
		fake:    fake,
		hosts:   []host.Host{fake},
	}
}

// options renders the CLI options of one test run.
func (w *world) options() Options {
	return Options{
		Version: "test",
		Out:     &w.out,
		Err:     &w.err,
		In:      strings.NewReader(w.in),
		TTY:     func(*os.File) bool { return w.tty },
		Now:     fixedNow,
		openOpts: []verger.Option{
			verger.WithHosts(w.hosts...),
		},
	}
}

// run executes one command line and returns the combined streams and error.
func (w *world) run(args ...string) (string, error) {
	w.out.Reset()
	w.err.Reset()

	root := NewRootCmd(w.options())
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()

	return w.out.String(), err
}

// mustRun runs one command line and fails the test on error.
func (w *world) mustRun(t *testing.T, args ...string) string {
	t.Helper()

	stdout, err := w.run(args...)
	if err != nil {
		t.Fatalf("verger %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, stdout, w.err.String())
	}

	return stdout
}

// chdir switches to a directory for the duration of one test.
func (w *world) chdir(t *testing.T, dir string) {
	t.Helper()

	t.Chdir(dir)
}

// warmHome materializes the home state dir and its flock file so a snapshot
// taken afterwards measures the command, not the lock creation.
func (w *world) warmHome(t *testing.T) {
	t.Helper()

	client, err := verger.Open(context.Background(), w.options().openOpts...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := client.Home().Ensure(); err != nil {
		t.Fatalf("ensure home: %v", err)
	}

	unlock, err := client.Home().Lock(context.Background())
	if err != nil {
		t.Fatalf("lock home: %v", err)
	}

	if err := unlock(); err != nil {
		t.Fatalf("unlock home: %v", err)
	}
}

// fixture writes a Claude-format package with one skill and one hook under the
// world root and returns the local ref.
func (w *world) fixture(t *testing.T) string {
	t.Helper()

	w.fixtureIn(t, w.root)

	return "./fixture"
}

// fixtureIn writes the fixture package inside dir.
func (w *world) fixtureIn(t *testing.T, dir string) {
	t.Helper()

	manifestDoc := `{
  "name": "caveman",
  "version": "1.2.3",
  "description": "Caveman toolkit.",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo hello"}]}
    ]
  }
}`

	pkgDir := filepath.Join(dir, "fixture")
	writeWorldFile(t, filepath.Join(pkgDir, ".claude-plugin", "plugin.json"), manifestDoc)
	writeWorldFile(t, filepath.Join(pkgDir, "skills", "one", "SKILL.md"), "# one\n")
}

// noHooksFixture writes a hooks-less Claude-format package under the world
// root and returns the local ref.
func (w *world) noHooksFixture(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(w.root, "nohooks")

	writeWorldFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{
  "name": "nohooks",
  "version": "1.0.0"
}`)
	writeWorldFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

	return "./nohooks"
}

// target configures the fake host's written artifact.
func (w *world) target(t *testing.T, name, data string) string {
	t.Helper()

	path := filepath.Join(w.root, "host", name)

	w.fake.mu.Lock()
	w.fake.files = []fakeFile{{Path: path, Data: data}}
	w.fake.mu.Unlock()

	return path
}

// writeWorldFile writes one fixture file.
func writeWorldFile(t *testing.T, path, data string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readWorldFile reads one fixture file.
func readWorldFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// snapshot hashes every file below root.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // G304: test snapshot
		if readErr != nil {
			return readErr
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		out[rel] = digest.Bytes(data).String()

		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	return out
}

// ---- command tree ------------------------------------------------------------

func TestCommandTree(t *testing.T) {
	required := []string{
		"install", "remove", "status", "sync", "adopt", "why", "doctor", "update",
		"pin", "restore", "secret", "source", "marketplace", "trust", "untrust",
		"approve", "revoke", "propagate", "watch", "pack", "lint",
		"self-update", "version",
	}

	Convey("Given the verger command tree", t, func() {
		root := NewRootCmd(Options{})

		Convey("When the commands are looked up", func() {
			Convey("Then every §8 verb exists", func() {
				names := map[string]bool{}

				for _, cmd := range root.Commands() {
					names[cmd.Name()] = true
				}

				for _, name := range required {
					So(names, ShouldContainKey, name)
				}
			})

			Convey("Then there is no registry command (D30: no own registry)", func() {
				names := map[string]bool{}

				for _, cmd := range root.Commands() {
					names[cmd.Name()] = true
				}

				So(names, ShouldNotContainKey, "registry")
			})

			Convey("Then the global persistent flags are registered", func() {
				flags := root.PersistentFlags()

				for _, name := range []string{"home", "json", "verbose", "log-json"} {
					So(flags.Lookup(name), ShouldNotBeNil)
				}
			})

			Convey("Then -y is a shorthand on the write commands", func() {
				for _, name := range []string{"install", "remove", "sync", "adopt", "trust"} {
					cmd, _, err := root.Find([]string{name})
					So(err, ShouldBeNil)

					flag := cmd.Flags().ShorthandLookup("y")
					So(flag, ShouldNotBeNil)
					So(flag.Name, ShouldEqual, "yes")
				}
			})
		})
	})
}

func TestUsageErrors(t *testing.T) {
	Convey("Given an unknown command", t, func() {
		w := newWorld(t)
		_, err := w.run("bogus")

		Convey("Then it is a *UsageError", func() {
			_, ok := errors.AsType[*UsageError](err)
			So(ok, ShouldBeTrue)
		})
	})

	Convey("Given an unknown flag", t, func() {
		w := newWorld(t)
		_, err := w.run("status", "--bogus")

		Convey("Then it is a *UsageError", func() {
			_, ok := errors.AsType[*UsageError](err)
			So(ok, ShouldBeTrue)
		})
	})

	Convey("Given a command with bad arguments", t, func() {
		w := newWorld(t)
		_, err := w.run("why")

		Convey("Then it is a *UsageError", func() {
			_, ok := errors.AsType[*UsageError](err)
			So(ok, ShouldBeTrue)
		})
	})
}

func TestExitCodeTaxonomy(t *testing.T) {
	Convey("Given the CLI error taxonomy", t, func() {
		Convey("When errors are mapped to exit codes", func() {
			Convey("Then every failure is exit 1 and success is 0", func() {
				So(exitCode(nil), ShouldEqual, 0)
				So(exitCode(errors.New("boom")), ShouldEqual, 1)
				So(exitCode(&UsageError{Cause: errors.New("args")}), ShouldEqual, 1)
				So(exitCode(ErrConfirmationRequired), ShouldEqual, 1)
				So(exitCode(&TrustError{Path: "spec", Cause: errors.New("untrusted")}), ShouldEqual, 1)
				So(exitCode(&NotAvailableError{Feature: "registry"}), ShouldEqual, 1)
			})
		})
	})
}

func TestHomeOverrideAndUnicode(t *testing.T) {
	Convey("Given an explicit --home", t, func() {
		w := newWorld(t)
		alt := filepath.Join(w.root, "альт")

		_, err := w.run("--home", alt, "status")

		Convey("When status runs", func() {
			Convey("Then it uses the alternate home and stays read-only", func() {
				So(err, ShouldBeNil)
				So(fileExists(alt), ShouldBeFalse)
			})
		})
	})

	Convey("Given unicode arguments", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "skill ☃.md", "# снег\n")
		w.fixture(t)

		stdout, err := w.run("install", "./fixture", "-y", "--json")

		Convey("When install runs", func() {
			Convey("Then the unicode target is written", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# снег\n")
				So(stdout, ShouldContainSubstring, "caveman")
			})
		})
	})
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}
