//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// The fixture identity: the manifest name carries one `/` so
// manifest.ParseAny yields an owner-bearing ID (acme/e2e-fixture) and the
// synth rung is reachable. An owner-less name resolves to local:<name> and can
// only ever go loose — the [B1] trap from T1.13-review-p3.
const (
	fixtureName  = "e2e-fixture"
	fixtureOwner = "acme"
	fixtureID    = fixtureOwner + "/" + fixtureName
)

// fixtureManifestName is the `name` written into every fixture manifest.
func fixtureManifestName() string { return fixtureID }

// commandTimeout bounds every external command the driver runs.
const commandTimeout = 5 * time.Minute

// Env carries one driver run.
type Env struct {
	Home    string // temp HOME for the host CLI and verger
	Bin     string // built verger binary (E2E_VERGER_BIN)
	Host    string // claude|codex|gemini
	Version string // host CLI version under test
	// Work is the fixture parent; `verger install ./fixture` runs with this
	// as its working directory.
	Work string
	// PATH overrides the child PATH; empty inherits the driver's PATH.
	PATH string
}

// cellDoc mirrors the stable `--json` cell document (pkg/cli §8); the driver
// parses it as an external contract and never imports pkg/cli.
type cellDoc struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// cellsDoc covers both `verger status --json` ({home,cells}) and the executed
// plan documents ({cells,notes}).
type cellsDoc struct {
	Home  string    `json:"home,omitempty"`
	Cells []cellDoc `json:"cells"`
}

// TestMain builds the verger binary once per e2e run. Without E2E=1 nothing is
// built and every scenario test skips itself with a reason.
func TestMain(m *testing.M) {
	builtDir := ""

	if os.Getenv("E2E") == "1" && os.Getenv("E2E_VERGER_BIN") == "" {
		bin, err := buildVerger()
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build verger: %v\n", err)
			os.Exit(1)
		}

		_ = os.Setenv("E2E_VERGER_BIN", bin)

		builtDir = filepath.Dir(bin)
	}

	code := m.Run()

	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}

	os.Exit(code)
}

// repoRoot walks up from the working directory to the module root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}

		dir = parent
	}
}

// buildVerger builds ./cmd/verger into a temp dir.
func buildVerger() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "verger-e2e-bin-")
	if err != nil {
		return "", err
	}

	bin := filepath.Join(dir, "verger")

	//nolint:gosec // G204: fixed argv, the driver builds ./cmd/verger
	cmd := exec.CommandContext(context.Background(), "go", "build", "-mod=vendor", "-o", bin, "./cmd/verger")
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build ./cmd/verger: %w\n%s", err, out)
	}

	return bin, nil
}

// requireE2E skips the scenario unless the e2e gate is on.
func requireE2E(t *testing.T) {
	t.Helper()

	if os.Getenv("E2E") != "1" {
		t.Skip("e2e: set E2E=1 and run with -tags e2e (the container/CI gate)")
	}
}

// requireHostFromEnv resolves E2E_HOST.
func requireHostFromEnv(t *testing.T) hostSpec {
	t.Helper()

	id := strings.TrimSpace(os.Getenv("E2E_HOST"))
	if id == "" {
		t.Skip("e2e: set E2E_HOST=claude|codex|gemini")
	}

	spec, ok := hostSpecByID(id)
	if !ok {
		t.Fatalf("e2e: unknown E2E_HOST %q", id)
	}

	return spec
}

// newEnv creates the isolated run environment: a temp HOME and work dir.
func newEnv(t *testing.T, spec hostSpec) Env {
	t.Helper()

	home := t.TempDir()
	work := t.TempDir()

	for _, rel := range []string{".config", ".local/share", "tmp"} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(rel)), 0o700); err != nil {
			t.Fatalf("e2e: prepare home: %v", err)
		}
	}

	return Env{
		Home:    home,
		Bin:     os.Getenv("E2E_VERGER_BIN"),
		Host:    spec.id,
		Version: spec.version(),
		Work:    work,
	}
}

// assertTempHome pins the isolation contract before any run: HOME is a temp
// dir and the built binary exists.
func assertTempHome(t *testing.T, env Env) {
	t.Helper()

	if env.Home == "" || !filepath.IsAbs(env.Home) || !strings.HasPrefix(filepath.Clean(env.Home), filepath.Clean(os.TempDir())) {
		t.Fatalf("e2e: HOME %q is not below the temp dir %q", env.Home, os.TempDir())
	}

	if filepath.Clean(env.Home) == filepath.Clean(os.Getenv("HOME")) {
		t.Fatalf("e2e: refusing to run against the process HOME %q", env.Home)
	}

	info, err := os.Stat(env.Bin)
	if err != nil {
		t.Fatalf("e2e: verger binary %q: %v (E2E=1 builds it in TestMain)", env.Bin, err)
	}

	if info.Mode()&0o111 == 0 {
		t.Fatalf("e2e: verger binary %q is not executable", env.Bin)
	}
}

// childEnv is the hermetic environment every child process sees: the temp
// HOME, no VERGER_HOME/BEADLE_HOME, no host profile overrides.
func childEnv(env Env) []string {
	path := env.PATH
	if path == "" {
		path = os.Getenv("PATH")
	}

	return []string{
		"HOME=" + env.Home,
		"PATH=" + path,
		"XDG_CONFIG_HOME=" + filepath.Join(env.Home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(env.Home, ".local", "share"),
		"TMPDIR=" + filepath.Join(env.Home, "tmp"),
		"TERM=dumb",
		"NO_COLOR=1",
		"CI=1",
		"LANG=C.UTF-8",
	}
}

// vergerArgv is the pure argv builder: the temp home is always pinned with
// --home so no child can read a developer's real home.
func vergerArgv(env Env, args ...string) []string {
	return append([]string{"--home", filepath.Join(env.Home, ".verger")}, args...)
}

// runVerger runs the binary argv-only and returns stdout plus the exit code.
func runVerger(t *testing.T, env Env, args ...string) (string, int) {
	t.Helper()

	stdout, _, code := runVergerFull(t, env, nil, args...)

	return stdout, code
}

// runVergerFull is runVerger with stderr and extra env entries (negative leg).
func runVergerFull(t *testing.T, env Env, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	//nolint:gosec // G204: argv-only, the built verger binary
	cmd := exec.CommandContext(ctx, env.Bin, vergerArgv(env, args...)...)
	cmd.Dir = env.Work
	cmd.Env = append(childEnv(env), extraEnv...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	code := 0

	if err != nil {
		exit := &exec.ExitError{}
		if !errors.As(err, &exit) {
			t.Fatalf("e2e: run verger %v: %v", args, err)
		}

		code = exit.ExitCode()
	}

	if code != 0 || testing.Verbose() {
		t.Logf("e2e: verger %v -> exit %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout.String(), stderr.String())
	}

	return stdout.String(), stderr.String(), code
}

// runHost runs one host CLI argv-only and returns combined output plus code.
func runHost(t *testing.T, env Env, name string, args ...string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	//nolint:gosec // G204: argv-only host CLI, arguments are driver constants
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = env.Work
	cmd.Env = childEnv(env)

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out

	err := cmd.Run()

	code := 0

	if err != nil {
		exit := &exec.ExitError{}
		if !errors.As(err, &exit) {
			t.Fatalf("e2e: run %s %v: %v", name, args, err)
		}

		code = exit.ExitCode()
	}

	return out.String(), code
}

// parseCells parses one stable --json document.
func parseCells(data []byte) (cellsDoc, error) {
	doc := cellsDoc{}

	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return cellsDoc{}, fmt.Errorf("parse verger JSON: %w", err)
	}

	return doc, nil
}

// oracleHasName reports whether raw oracle output mentions name
// (case-insensitive; the host JSON shapes differ per host).
func oracleHasName(raw []byte, name string) bool {
	return strings.Contains(strings.ToLower(string(raw)), strings.ToLower(name))
}

// ensureHost makes sure the host CLI is reachable, installing it from npm when
// explicitly asked; an absent CLI skips the scenario with an explicit reason.
func ensureHost(t *testing.T, env Env, spec hostSpec) {
	t.Helper()

	if _, err := exec.LookPath(spec.binary); err == nil {
		return
	}

	if os.Getenv("E2E_NPM_INSTALL") == "1" {
		t.Logf("e2e: installing %s from npm", spec.moduleRef())

		out, code := runHost(t, env, "npm", "install", "-g", spec.moduleRef())
		if code != 0 {
			t.Fatalf("e2e: npm install -g %s -> %d\n%s", spec.moduleRef(), code, out)
		}

		if _, err := exec.LookPath(spec.binary); err == nil {
			return
		}
	}

	t.Skipf("e2e: %s CLI is not installed for E2E_HOST=%s (npm install -g %s)", spec.binary, spec.id, spec.moduleRef())
}

// makeFixture writes the host's fixture package below the work dir.
func makeFixture(t *testing.T, env Env, spec hostSpec) string {
	t.Helper()

	fixture := filepath.Join(env.Work, "fixture")

	writeFile(t, filepath.Join(fixture, filepath.FromSlash(spec.fixtureManifest)), fmt.Sprintf(`{
  "name": %q,
  "version": "1.0.0",
  "description": "verger e2e fixture"
}
`, fixtureManifestName()))

	writeFile(t, filepath.Join(fixture, "skills", "e2e-skill", "SKILL.md"),
		"---\nname: e2e-skill\ndescription: e2e fixture skill\n---\n\nFixture body.\n")

	return fixture
}

// writeFile writes one fixture file, creating parents.
func writeFile(t *testing.T, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("e2e: mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("e2e: write %s: %v", path, err)
	}
}

// hostList runs the host's own JSON list oracle.
func hostList(t *testing.T, env Env, spec hostSpec) string {
	t.Helper()

	out, code := runHost(t, env, spec.binary, spec.listArgs...)
	if code != 0 {
		t.Fatalf("e2e: %s %v -> %d\n%s", spec.binary, spec.listArgs, code, out)
	}

	return out
}

// requireOracle asserts the host's own oracle sees (or no longer sees) pkg.
func requireOracle(t *testing.T, env Env, spec hostSpec, pkg string, want bool) {
	t.Helper()

	raw := hostList(t, env, spec)

	if got := oracleHasName([]byte(raw), pkg); got != want {
		t.Fatalf("e2e: oracle %s %v sees %q = %v, want %v; output:\n%s",
			spec.binary, spec.listArgs, pkg, got, want, raw)
	}
}

// statusCell runs `verger status --json` and returns the host's cell.
func statusCell(t *testing.T, env Env, host string) cellDoc {
	t.Helper()

	out, code := runVerger(t, env, "status", "--json")
	if code != 0 {
		t.Fatalf("e2e: verger status --json -> %d\n%s", code, out)
	}

	doc, err := parseCells([]byte(out))
	if err != nil {
		t.Fatalf("e2e: %v\n%s", err, out)
	}

	for _, cell := range doc.Cells {
		if cell.Host == host {
			return cell
		}
	}

	t.Fatalf("e2e: status has no %s cell; cells: %+v", host, doc.Cells)

	return cellDoc{}
}

// assertStatus fails when the host cell does not carry want.
func assertStatus(t *testing.T, env Env, host, want string) cellDoc {
	t.Helper()

	cell := statusCell(t, env, host)
	if cell.Status != want {
		t.Fatalf("e2e: %s cell status = %q, want %q (notes %v, cell %+v)", host, cell.Status, want, cell.Notes, cell)
	}

	return cell
}

// assertNoResidue scans the host's own directories for surviving mentions of
// the fixture after remove.
func assertNoResidue(t *testing.T, env Env, spec hostSpec) {
	t.Helper()

	for _, rel := range spec.configDirs {
		root := filepath.Join(env.Home, filepath.FromSlash(rel))

		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fs.SkipAll
				}

				return err
			}

			if entry.IsDir() {
				return nil
			}

			info, statErr := entry.Info()
			if statErr != nil || info.Size() > 8<<20 {
				return nil //nolint:nilerr // an unreadable or huge entry is not a residue signal
			}

			data, readErr := os.ReadFile(path) //nolint:gosec // G304: host config below the temp HOME
			if readErr != nil {
				return nil //nolint:nilerr // an unreadable entry is not a residue signal
			}

			if strings.Contains(string(data), fixtureName) {
				t.Errorf("e2e: residue after remove: %s still mentions %q", path, fixtureName)
			}

			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
			t.Fatalf("e2e: scan %s: %v", root, walkErr)
		}
	}
}

// adoptLeg manually installs the fixture with the host CLI and adopts it.
func adoptLeg(t *testing.T, env Env, spec hostSpec) {
	t.Helper()

	if spec.adoptReason != "" {
		t.Logf("e2e: adopt leg skipped for %s: %s", spec.id, spec.adoptReason)

		return
	}

	manualInstall(t, env, spec)

	// Snapshot after the manual install: the host CLI itself edits its settings
	// (extraKnownMarketplaces, enabledPlugins); the check is that adopt adds no
	// hooks (D4), not that the host never writes.
	hooksPath := filepath.Join(env.Home, filepath.FromSlash(spec.hooksFile))
	hooksBefore, hooksBeforeOK := snapshotFile(hooksPath)

	// the oracle lists the bare plugin name; pkg/cli matches that form (M2)
	out, code := runVerger(t, env, "adopt", spec.id+":"+fixtureName, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger adopt %s:%s -> %d\n%s", spec.id, fixtureName, code, out)
	}

	specPath := filepath.Join(env.Home, ".verger", "verger.toml")

	body, err := os.ReadFile(specPath) //nolint:gosec // G304: home below the temp dir
	if err != nil {
		t.Fatalf("e2e: read spec %s: %v", specPath, err)
	}

	// semantic TOML check: the CLI writes single-quoted TOML (M1)
	doc := map[string]any{}
	if err := toml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("e2e: decode spec %s: %v\n%s", specPath, err, body)
	}

	if !specHasAdoptedFrom(doc, spec.id) {
		t.Fatalf("e2e: spec has no adopted_from=%q after adopt:\n%s", spec.id, body)
	}

	hooksAfter, hooksAfterOK := snapshotFile(hooksPath)
	if hooksBeforeOK != hooksAfterOK || !bytes.Equal(hooksBefore, hooksAfter) {
		t.Fatalf("e2e: adopt installed hooks in %s:\nbefore: %s\nafter:  %s", hooksPath, hooksBefore, hooksAfter)
	}

	// §T1.13 rule 2 asserts the spec and the hooks only; how status shows the
	// origin cell of an adopted package is open (F9, T2.5), so it is logged.
	out, _ = runVerger(t, env, "status", "--json")
	t.Logf("e2e: status after adopt: %s", out)
}

// manualInstall registers the fixture with the host itself for the adopt leg.
func manualInstall(t *testing.T, env Env, spec hostSpec) {
	t.Helper()

	switch spec.id {
	case "claude":
		manualInstallClaude(t, env)
	case "gemini":
		manualInstallGemini(t, env)
	default:
		t.Fatalf("e2e: no manual install for %s", spec.id)
	}
}

// manualInstallClaude adds a local marketplace and installs from it. Claude
// rejects a plugin source outside the marketplace root ("../fixture" is
// "source: Invalid input"), so the fixture is copied under the root.
func manualInstallClaude(t *testing.T, env Env) {
	t.Helper()

	const marketplace = "e2e-market"

	root := filepath.Join(env.Work, "marketplace")
	if err := os.CopyFS(filepath.Join(root, "fixture"), os.DirFS(filepath.Join(env.Work, "fixture"))); err != nil {
		t.Fatalf("e2e: copy fixture under the marketplace root: %v", err)
	}

	writeFile(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), fmt.Sprintf(`{
  "name": %q,
  "owner": {"name": "e2e"},
  "plugins": [
    {"name": %q, "source": "./fixture", "description": "verger e2e fixture"}
  ]
}
`, marketplace, fixtureName))

	out, code := runHost(t, env, "claude", "plugin", "marketplace", "add", root)
	if code != 0 {
		t.Fatalf("e2e: claude plugin marketplace add -> %d\n%s", code, out)
	}

	ref := fixtureName + "@" + marketplace

	out, code = runHost(t, env, "claude", "plugin", "install", ref)
	if code != 0 {
		t.Fatalf("e2e: claude plugin install %s -> %d\n%s", ref, code, out)
	}
}

// manualInstallGemini links the fixture as an extension.
func manualInstallGemini(t *testing.T, env Env) {
	t.Helper()

	out, code := runHost(t, env, "gemini", "extensions", "link", filepath.Join(env.Work, "fixture"))
	if code != 0 {
		t.Fatalf("e2e: gemini extensions link -> %d\n%s", code, out)
	}
}

// snapshotFile reads a file; missing is (nil,false).
func snapshotFile(path string) ([]byte, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: home below the temp dir
	if err != nil {
		return nil, false
	}

	return data, true
}

// dumpRuntime writes the failure report into the job log and, when
// E2E_LOG_DIR is set, into files the workflow uploads (rule 6: listings only,
// never the home itself).
func dumpRuntime(t *testing.T, env Env, spec hostSpec) {
	t.Helper()

	t.Logf("e2e dump: host=%s version=%s home=%s", spec.id, env.Version, env.Home)

	statusOut, _, code := runVergerQuiet(t, env, "status", "--json")
	t.Logf("e2e dump: verger status --json (exit %d):\n%s", code, statusOut)

	var listings strings.Builder

	for _, rel := range spec.configDirs {
		fmt.Fprintf(&listings, "== %s ==\n%s\n", rel, listing(filepath.Join(env.Home, filepath.FromSlash(rel))))
	}

	t.Logf("e2e dump: host config listings:\n%s", listings.String())

	if dir := os.Getenv("E2E_LOG_DIR"); dir != "" {
		//nolint:gosec // G703: the CI-provided workspace log dir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Logf("e2e: create log dir: %v", err)

			return
		}

		//nolint:gosec // G703: the CI-provided workspace log dir
		_ = os.WriteFile(filepath.Join(dir, "status-"+spec.id+".json"), []byte(statusOut), 0o600)
		//nolint:gosec // G703: the CI-provided workspace log dir
		_ = os.WriteFile(filepath.Join(dir, "listing-"+spec.id+".txt"), []byte(listings.String()), 0o600)
	}
}

// runVergerQuiet is runVergerFull without logging or fatal errors.
func runVergerQuiet(t *testing.T, env Env, args ...string) (string, string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	//nolint:gosec // G204: argv-only, the built verger binary
	cmd := exec.CommandContext(ctx, env.Bin, vergerArgv(env, args...)...)
	cmd.Dir = env.Work
	cmd.Env = childEnv(env)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	code := 0

	if err != nil {
		exit := &exec.ExitError{}
		if !errors.As(err, &exit) {
			return stdout.String(), stderr.String(), -1
		}

		code = exit.ExitCode()
	}

	return stdout.String(), stderr.String(), code
}

// listing renders a directory tree as "rel  size" lines.
func listing(root string) string {
	var out strings.Builder

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		if entry.IsDir() {
			fmt.Fprintf(&out, "%s/\n", rel)

			return nil
		}

		info, statErr := entry.Info()
		if statErr != nil {
			fmt.Fprintf(&out, "%s\n", rel)

			return nil //nolint:nilerr // a size-less entry is still listed
		}

		fmt.Fprintf(&out, "%s  %d\n", rel, info.Size())

		return nil
	})
	if walkErr != nil {
		return "<unreadable: " + walkErr.Error() + ">"
	}

	return out.String()
}

// TestE2ELocalFixtureScenario covers the local-ref fixture. The strategy is
// read from the status cell and the oracle expectation follows it: a loose
// delivery writes host user files only and never appears in the host's own
// list, while synth (the ladder rung once pkg/cli renders local payloads —
// B1(a), T1.12 owner) registers with the host CLI. remove → not current →
// oracle must not see it → no residue → adopt leg.
func TestE2ELocalFixtureScenario(t *testing.T) {
	requireE2E(t)

	spec := requireHostFromEnv(t)
	env := newEnv(t, spec)
	assertTempHome(t, env)

	t.Cleanup(func() {
		if t.Failed() {
			dumpRuntime(t, env, spec)
		}
	})

	ensureHost(t, env, spec)
	makeFixture(t, env, spec)

	out, code := runVerger(t, env, "install", "./fixture", "-y", "--hosts", spec.id, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger install -> %d\n%s", code, out)
	}

	cell := assertStatus(t, env, spec.id, "current")
	if !strings.Contains(cell.Package, fixtureName) {
		t.Fatalf("e2e: status cell package %q does not name the fixture", cell.Package)
	}

	switch cell.Strategy {
	case "loose":
		// causal oracle expectation: loose never registers with the host CLI
		assertLooseMarker(t, env, spec)
		requireOracle(t, env, spec, fixtureName, false)
	case "synth", "native":
		requireOracle(t, env, spec, fixtureName, true)
	default:
		t.Fatalf("e2e: unexpected strategy %q for the local fixture", cell.Strategy)
	}

	out, code = runVerger(t, env, "remove", cell.Package, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger remove %s -> %d\n%s", cell.Package, code, out)
	}

	assertNotCurrent(t, env, spec.id)
	requireOracle(t, env, spec, fixtureName, false)
	assertNoResidue(t, env, spec)

	adoptLeg(t, env, spec)
}

// TestE2ERemoteArchiveScenario pins the host-registering rung deterministically:
// the fixture is served as a pinned tar.gz over loopback, so the fetched payload
// is a remote kind with an owner and the ladder reaches synth (native when the
// payload carries a marketplace). The host's own oracle must see it, and must
// not after remove.
func TestE2ERemoteArchiveScenario(t *testing.T) {
	requireE2E(t)

	spec := requireHostFromEnv(t)
	env := newEnv(t, spec)
	assertTempHome(t, env)

	t.Cleanup(func() {
		if t.Failed() {
			dumpRuntime(t, env, spec)
		}
	})

	ensureHost(t, env, spec)
	makeFixture(t, env, spec)

	ref := serveFixtureArchive(t, filepath.Join(env.Work, "fixture"))

	out, code := runVerger(t, env, "install", ref, "-y", "--hosts", spec.id, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger install %s -> %d\n%s", ref, code, out)
	}

	cell := assertStatus(t, env, spec.id, "current")
	if !strings.Contains(cell.Package, fixtureName) {
		t.Fatalf("e2e: status cell package %q does not name the fixture", cell.Package)
	}

	if !oracleExpectRegistered(cell.Strategy) {
		t.Fatalf("e2e: remote archive chose %q, want a host-registering strategy (synth/native)", cell.Strategy)
	}

	requireOracle(t, env, spec, fixtureName, true)

	out, code = runVerger(t, env, "remove", cell.Package, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger remove %s -> %d\n%s", cell.Package, code, out)
	}

	assertNotCurrent(t, env, spec.id)
	requireOracle(t, env, spec, fixtureName, false)
	assertNoResidue(t, env, spec)
}

// TestE2ENegative is the missing-host leg: with no host CLI on PATH the CLI
// must fail cleanly, never panic.
func TestE2ENegative(t *testing.T) {
	requireE2E(t)

	if os.Getenv("E2E_NEGATIVE") != "1" {
		t.Skip("e2e: set E2E_NEGATIVE=1 to run the PATH-stripped negative leg")
	}

	spec := requireHostFromEnv(t)
	env := newEnv(t, spec)
	assertTempHome(t, env)

	t.Cleanup(func() {
		if t.Failed() {
			dumpRuntime(t, env, spec)
		}
	})

	makeFixture(t, env, spec)

	env.PATH = t.TempDir() // no host CLI reachable at all

	stdout, stderr, code := runVergerFull(t, env, nil, "install", "./fixture", "-y", "--hosts", spec.id)
	combined := stdout + stderr

	if code == 0 {
		t.Fatalf("e2e: install with %s absent exited 0, want non-zero\n%s", spec.id, combined)
	}

	if strings.Contains(combined, "panic:") {
		t.Fatalf("e2e: missing host caused a panic:\n%s", combined)
	}

	lower := strings.ToLower(combined)

	missMarkers := []string{"not found", "no hosts", "no detected hosts", "no available adapter", "not registered", "not detected"}
	if !slices.ContainsFunc(missMarkers, func(m string) bool { return strings.Contains(lower, m) }) {
		t.Fatalf("e2e: missing-host failure does not name the miss:\n%s", combined)
	}

	if !strings.Contains(lower, spec.id) {
		t.Fatalf("e2e: missing-host failure does not name the host %q:\n%s", spec.id, combined)
	}
}

// assertNotCurrent accepts both post-remove shapes: a non-current cell (spec
// §T1.13 rule 1 wants `missing`) or no cell at all — pkg/cli currently drops
// the tombstoned cell (B2 in T1.13-review-p3 is a pkg/cli fix owned by T1.12;
// this tolerance is deliberate and documented in docs/e2e.md).
func assertNotCurrent(t *testing.T, env Env, host string) {
	t.Helper()

	out, code := runVerger(t, env, "status", "--json")
	if code != 0 {
		t.Fatalf("e2e: verger status --json -> %d\n%s", code, out)
	}

	doc, err := parseCells([]byte(out))
	if err != nil {
		t.Fatalf("e2e: %v\n%s", err, out)
	}

	for _, cell := range doc.Cells {
		if cell.Host != host {
			continue
		}

		if cell.Status == "current" {
			t.Fatalf("e2e: %s cell still current after remove: %+v", host, cell)
		}

		return
	}

	t.Logf("e2e: %s cell absent after remove (pkg/cli drops it; §T1.13 rule 1 wants `missing` — B2 open)", host)
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// assertLooseMarker checks the host-loose skill tree of the strategy ladder's
// final rung, so the loose scenario proves delivery even though the oracle
// never sees it.
func assertLooseMarker(t *testing.T, env Env, spec hostSpec) {
	t.Helper()

	marker := looseSkillMarker(env.Home, spec.id)
	if marker == "" {
		t.Fatalf("e2e: no loose skill marker for %s", spec.id)
	}

	if !fileExists(marker) {
		t.Fatalf("e2e: loose delivery did not write %s", marker)
	}
}

// looseSkillMarker is the host-loose skill path of the fixture skill.
func looseSkillMarker(home, host string) string {
	rel := map[string]string{
		"claude": ".claude/skills/e2e-skill/SKILL.md",
		"codex":  ".agents/skills/e2e-skill/SKILL.md",
		"gemini": ".gemini/skills/e2e-skill/SKILL.md",
	}[host]

	if rel == "" {
		return ""
	}

	return filepath.Join(home, filepath.FromSlash(rel))
}

// serveFixtureArchive packs the fixture into a single-rooted tar.gz and serves
// it over loopback, returning the pinned archive ref.
func serveFixtureArchive(t *testing.T, dir string) string {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	walkErr := filepath.WalkDir(dir, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(dir, current)
		if relErr != nil {
			return relErr
		}

		if rel == "." {
			return nil
		}

		return appendTarEntry(tw, current, rel, entry)
	})
	if walkErr != nil {
		t.Fatalf("e2e: pack fixture: %v", walkErr)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("e2e: close tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("e2e: close gzip: %v", err)
	}

	data := buf.Bytes()
	sum := sha256.Sum256(data)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture.tar.gz" {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	return srv.URL + "/fixture.tar.gz#sha256=" + hex.EncodeToString(sum[:])
}

// appendTarEntry writes one walked fixture entry into the tarball.
func appendTarEntry(tw *tar.Writer, current, rel string, entry fs.DirEntry) error {
	header := &tar.Header{Name: "fixture/" + filepath.ToSlash(rel), Mode: 0o600}

	if entry.IsDir() {
		header.Typeflag = tar.TypeDir
		header.Mode = 0o700
	} else {
		info, err := entry.Info()
		if err != nil {
			return err
		}

		header.Typeflag = tar.TypeReg
		header.Size = info.Size()
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	if entry.IsDir() {
		return nil
	}

	data, err := os.ReadFile(current) //nolint:gosec // G304: the fixture below the temp work dir
	if err != nil {
		return err
	}

	_, err = tw.Write(data)

	return err
}

// oracleExpectRegistered reports whether a strategy registers the package with
// the host CLI, i.e. whether the host's own oracle must list it.
func oracleExpectRegistered(strategy string) bool {
	switch strategy {
	case "native", "synth":
		return true
	default:
		return false
	}
}

// adoptedFromValues collects every adopted_from string in a decoded spec.
func adoptedFromValues(value any) []string {
	var out []string

	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == "adopted_from" {
				if text, ok := item.(string); ok {
					out = append(out, text)
				}

				continue
			}

			out = append(out, adoptedFromValues(item)...)
		}
	case []any:
		for _, item := range typed {
			out = append(out, adoptedFromValues(item)...)
		}
	}

	return out
}

// specHasAdoptedFrom reports whether the spec records the host as the adoption
// source, independent of the TOML quoting style.
func specHasAdoptedFrom(doc map[string]any, host string) bool {
	return slices.Contains(adoptedFromValues(doc), host)
}

// TestUnitVergerArgv pins the argv builder: the temp home is always explicit.
func TestUnitVergerArgv(t *testing.T) {
	env := Env{Home: filepath.Join(string(filepath.Separator), "tmp", "e2e-home")}

	got := vergerArgv(env, "install", "./fixture", "-y")
	want := []string{"--home", filepath.Join(env.Home, ".verger"), "install", "./fixture", "-y"}

	if !slices.Equal(got, want) {
		t.Fatalf("vergerArgv = %q, want %q", got, want)
	}
}

// TestUnitParseCells pins the status/report JSON contract the driver relies on.
func TestUnitParseCells(t *testing.T) {
	raw := []byte(`{"home":"/tmp/h","cells":[{"package":"local:e2e-fixture","host":"claude","scope":"user","status":"current","version":"1.0.0"}]}`)

	doc, err := parseCells(raw)
	if err != nil {
		t.Fatalf("parseCells: %v", err)
	}

	if len(doc.Cells) != 1 || doc.Cells[0].Host != "claude" || doc.Cells[0].Status != "current" {
		t.Fatalf("parseCells = %+v", doc)
	}

	// the executed-plan shape has no home key and parses too
	if _, err := parseCells([]byte(`{"cells":[],"notes":["x"]}`)); err != nil {
		t.Fatalf("parseCells report: %v", err)
	}

	if _, err := parseCells([]byte("not json")); err == nil {
		t.Fatal("parseCells accepted non-JSON")
	}
}

// TestUnitOracleHasName pins the tolerant oracle matcher.
func TestUnitOracleHasName(t *testing.T) {
	raw := []byte(`[{"name":"e2e-fixture@e2e-market","version":"1.0.0"}]`)

	if !oracleHasName(raw, "e2e-fixture") {
		t.Fatal("oracleHasName missed the fixture")
	}

	if oracleHasName(raw, "someone-else") {
		t.Fatal("oracleHasName matched an absent package")
	}
}

// TestUnitHostSpecs pins the matrix shape: three hosts, npm pins, oracles.
func TestUnitHostSpecs(t *testing.T) {
	specs := hostSpecs()
	if len(specs) != 3 {
		t.Fatalf("hostSpecs length = %d, want 3", len(specs))
	}

	for _, spec := range specs {
		if spec.id == "" || spec.binary == "" || spec.npm == "" || spec.pin == "" {
			t.Fatalf("incomplete host spec: %+v", spec)
		}

		if len(spec.listArgs) == 0 || len(spec.configDirs) == 0 || spec.fixtureManifest == "" {
			t.Fatalf("host spec %s lacks oracle/config/fixture data", spec.id)
		}
	}
}

// TestUnitOracleExpectation pins the strategy → oracle causality: only the
// host-registering strategies appear in the host's own list.
func TestUnitOracleExpectation(t *testing.T) {
	cases := map[string]bool{
		"native": true,
		"synth":  true,
		"loose":  false,
		"":       false,
	}

	for strategy, want := range cases {
		if got := oracleExpectRegistered(strategy); got != want {
			t.Fatalf("oracleExpectRegistered(%q) = %v, want %v", strategy, got, want)
		}
	}
}

// TestUnitAdoptedFromParsing pins the semantic TOML assertion (the CLI writes
// single-quoted TOML; a substring check for double quotes is a false negative).
func TestUnitAdoptedFromParsing(t *testing.T) {
	raw := []byte("[[packages]]\nid = 'e2e-fixture'\nadopted_from = 'claude'\nversion = '1.0.0'\n")

	doc := map[string]any{}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	values := adoptedFromValues(doc)
	if len(values) != 1 || values[0] != "claude" {
		t.Fatalf("adoptedFromValues = %v, want [claude]", values)
	}

	if !specHasAdoptedFrom(doc, "claude") {
		t.Fatal("specHasAdoptedFrom missed the single-quoted value")
	}

	if specHasAdoptedFrom(doc, "gemini") {
		t.Fatal("specHasAdoptedFrom matched a wrong host")
	}
}

// TestUnitFixtureIdentity pins the fixture owner: the manifest name carries one
// `/`, so manifest.ParseAny yields an owner-bearing ID and synth is reachable.
func TestUnitFixtureIdentity(t *testing.T) {
	if fixtureID != fixtureOwner+"/"+fixtureName {
		t.Fatalf("fixtureID = %q, want %q", fixtureID, fixtureOwner+"/"+fixtureName)
	}

	if fixtureManifestName() != fixtureID {
		t.Fatalf("fixtureManifestName = %q, want %q", fixtureManifestName(), fixtureID)
	}
}
