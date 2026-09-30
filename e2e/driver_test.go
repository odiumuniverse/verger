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
	Package string `json:"package"`
	Host    string `json:"host"`
	Scope   string `json:"scope"`
	Status  string `json:"status"`
	// Detail is the internal code beside the user-facing word: `status` says
	// "delivered", `detail` says "current". A cell struct carrying only
	// `status` read the word and compared it to a code - which is why the
	// gemini canon leg failed at assertStatus on one platform and passed on
	// another (N045): the two runs were different driver revisions, not
	// different platforms.
	Detail   string   `json:"detail,omitempty"`
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

// driverFingerprint identifies the build of this file that is running. A leg
// that logs its receipt kinds carries it, so a green run on one platform and
// a red run on another can be compared without first suspecting the product:
// different fingerprints mean different drivers, not different hosts.
//
// It is a hand-bumped constant rather than a checksum of the source on disk,
// because a checksum would describe the TREE while the thing that decides a
// leg is the BINARY — and those two diverging is exactly the failure this
// exists to make visible. Bump it whenever this file's assertions change.
const driverFingerprint = "e2e-driver-2026-09-29a"

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

	// -buildvcs=false: in the CI container the checkout belongs to another
	// uid, git refuses it ("dubious ownership") and VCS stamping fails the build.
	//
	//nolint:gosec // G204: fixed argv, the driver builds ./cmd/verger
	cmd := exec.CommandContext(context.Background(), "go", "build", "-mod=vendor", "-buildvcs=false", "-o", bin, "./cmd/verger")
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

	env := Env{
		Home:    home,
		Bin:     os.Getenv("E2E_VERGER_BIN"),
		Host:    spec.id,
		Version: spec.version(),
		Work:    work,
	}

	// E2E_PATH prepends directories to the child PATH: a host that ships from
	// a second toolchain (dsh lives in another node version here) needs it on
	// PATH without touching the developer's shell.
	if extra := strings.TrimSpace(os.Getenv("E2E_PATH")); extra != "" {
		env.PATH = extra + string(os.PathListSeparator) + os.Getenv("PATH")
	}

	return env
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

	if _, err := lookPath(env, spec.binary); err == nil {
		return
	}

	if os.Getenv("E2E_NPM_INSTALL") == "1" && len(spec.installRefs()) > 0 {
		t.Logf("e2e: installing %s from npm", strings.Join(spec.installRefs(), " "))

		out, code := runHost(t, env, "npm", append([]string{"install", "-g"}, spec.installRefs()...)...)
		if code != 0 {
			t.Fatalf("e2e: npm install -g %s -> %d\n%s", strings.Join(spec.installRefs(), " "), code, out)
		}

		if _, err := lookPath(env, spec.binary); err == nil {
			return
		}
	}

	t.Skipf("e2e: %s CLI is not installed for E2E_HOST=%s (%s)", spec.binary, spec.id, spec.installHint())
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

	if len(spec.listArgs) == 0 {
		t.Fatalf("e2e: %s has no listing oracle argv; the caller must honour spec.noOracleReason", spec.id)
	}

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
	// The internal code, not the word: every caller asks for "current", which
	// is what `detail` says. `status` is what the user reads.
	if cell.Detail != want {
		t.Fatalf("e2e: %s cell detail = %q, want %q (status %q, notes %v, cell %+v)", host, cell.Detail, want, cell.Status, cell.Notes, cell)
	}

	return cell
}

// hostOwnedFiles are files a host CLI owns outright inside the scanned config
// dirs (matched by base name). verger never writes them, so a fixture mention
// in them is the host's saved state, not residue of a verger delivery:
//
//   - gemini-cli 0.61.0's ~/.gemini/extension_integrity.json: an HMAC-SHA256
//     store signed with a key in the macOS Keychain, exposed without a delete
//     API (ExtensionIntegrityManager has only verify/store). Editing it
//     surgically would invalidate the signature for every installed extension
//     and deleting it would drop other tools' records, so verger must leave it
//     alone — including on remove (pkg/host,
//     TestGeminiUninstallLeavesHostOwnedIntegrityStore).
var hostOwnedFiles = map[string]string{
	"extension_integrity.json": "gemini-cli owns this signed store; verger never writes or deletes it",
}

// insideHostRuntime reports whether path is below one of the host-owned runtime
// subtrees a spec lists (hostSpec.residueSkip): state the host writes about its
// own operations, where a plugin name is the host's record, not verger residue.
func insideHostRuntime(home, path string, skip []string) bool {
	for _, rel := range skip {
		root := filepath.Join(home, filepath.FromSlash(rel))

		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}

	return false
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
				if insideHostRuntime(env.Home, path, spec.residueSkip) {
					t.Logf("e2e: residue scan skips host-owned %s", path)

					return fs.SkipDir
				}

				return nil
			}

			if reason, owned := hostOwnedFiles[entry.Name()]; owned {
				t.Logf("e2e: residue scan skips host-owned %s (%s)", path, reason)

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

	// `verger adopt` installs the adopted package into **every detected host**
	// (D4) and exits non-zero when one of them refuses it — on a machine with
	// ten CLIs that is the normal case (finding F2 in W3-E2E10-1.md). The leg
	// therefore runs adopt with a PATH that resolves only the host under test,
	// which is also the honest isolation for "the user has this one host".
	restore := env.PATH
	env.PATH = hostOnlyPath(t, env, spec)

	// the oracle lists the bare plugin name; pkg/cli matches that form (M2).
	// A host whose oracle names paths (pi) is adopted by that path.
	ref := spec.id + ":" + fixtureName
	if spec.adoptByPath {
		ref = spec.id + ":" + filepath.Join(env.Work, "fixture")
	}

	out, code := runVerger(t, env, "adopt", ref, "--json")

	env.PATH = restore
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
	case "codex":
		manualInstallCodex(t, env)
	case "gemini":
		manualInstallGemini(t, env)
	case "omp":
		manualInstallOmp(t, env)
	case "pi":
		manualInstallPi(t, env)
	default:
		t.Fatalf("e2e: no manual install for %s", spec.id)
	}
}

// manualInstallOmp registers a local marketplace with omp and installs the
// fixture from it (omp 18.4.1). The catalog uses the Claude-compatible
// fallback path omp accepts, and the marketplace document names the
// marketplace omp registers.
func manualInstallOmp(t *testing.T, env Env) {
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

	out, code := runHost(t, env, "omp", "plugin", "marketplace", "add", root)
	if code != 0 {
		t.Fatalf("e2e: omp plugin marketplace add -> %d\n%s", code, out)
	}

	ref := fixtureName + "@" + marketplace

	out, code = runHost(t, env, "omp", "plugin", "install", ref)
	if code != 0 {
		t.Fatalf("e2e: omp plugin install %s -> %d\n%s", ref, code, out)
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

// manualInstallCodex registers a local marketplace and adds the fixture from
// it, the grammar the adapter runs for codex-cli 0.157.1: `plugin marketplace
// add <dir> --json` (Codex reads the marketplace document at
// .claude-plugin/marketplace.json, like Claude) then `plugin add
// <plugin>@<marketplace>`. The fixture is copied under the marketplace root
// because a plugin source must be a dir inside it.
func manualInstallCodex(t *testing.T, env Env) {
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

	out, code := runHost(t, env, "codex", "plugin", "marketplace", "add", root, "--json")
	if code != 0 {
		t.Fatalf("e2e: codex plugin marketplace add -> %d\n%s", code, out)
	}

	ref := fixtureName + "@" + marketplace

	out, code = runHost(t, env, "codex", "plugin", "add", ref)
	if code != 0 {
		t.Fatalf("e2e: codex plugin add %s -> %d\n%s", ref, code, out)
	}
}

// manualInstallGemini links the fixture as an extension. --consent mirrors
// the adapter's flagConsent: without it gemini-cli asks for workspace trust
// and hangs a non-interactive run.
//
// The fixture is staged first: its manifest name is the owner-bearing package
// id (`acme/e2e-fixture`), which the synth rung needs, while gemini-cli 0.61.0
// refuses such a name in its own grammar — "Invalid extension name:
// "acme/e2e-fixture". Only letters (a-z, A-Z), numbers (0-9), and dashes (-)
// are allowed." The staged copy carries the bare name the oracle then lists,
// which is what `adopt gemini:<name>` looks up.
func manualInstallGemini(t *testing.T, env Env) {
	t.Helper()

	root := filepath.Join(env.Work, "gemini-extension")

	if err := os.CopyFS(root, os.DirFS(filepath.Join(env.Work, "fixture"))); err != nil {
		t.Fatalf("e2e: stage the fixture for gemini: %v", err)
	}

	writeFile(t, filepath.Join(root, "gemini-extension.json"), fmt.Sprintf(`{
  "name": %q,
  "version": "1.0.0",
  "description": "verger e2e fixture"
}
`, fixtureName))

	out, code := runHost(t, env, "gemini", "extensions", "link", root, "--consent")
	if code != 0 {
		t.Fatalf("e2e: gemini extensions link -> %d\n%s", code, out)
	}
}

// manualInstallPi registers the fixture directory with pi's own installer
// (`pi install <ABS_DIR>`, dist/package-manager-cli.js:105-150): pi records the
// resolved path in settings.json, user scope, and lists it back by that path.
func manualInstallPi(t *testing.T, env Env) {
	t.Helper()

	dir := filepath.Join(env.Work, "fixture")

	out, code := runHost(t, env, "pi", "install", dir)
	if code != 0 {
		t.Fatalf("e2e: pi install %s -> %d\n%s", dir, code, out)
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

		if spec.noOracleReason == "" {
			requireOracle(t, env, spec, fixtureName, false)
		} else {
			t.Logf("e2e: %s: oracle check skipped: %s", spec.id, spec.noOracleReason)
		}
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

	if spec.noOracleReason == "" {
		requireOracle(t, env, spec, fixtureName, false)
	}

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

	if spec.noRegisterReason != "" {
		t.Skipf("e2e: %s has no host-registering stratum: %s", spec.id, spec.noRegisterReason)
	}
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

	if spec.noOracleReason == "" {
		requireOracle(t, env, spec, fixtureName, false)
	}

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
	return looseSkillMarkerFor(home, host, "e2e-skill")
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

// TestUnitHostSpecs pins the matrix shape: all ten hosts of U4, pins, oracles
// (or the documented reason there is none), and an install path for each (npm,
// or a reason why there is none).
func TestUnitHostSpecs(t *testing.T) {
	specs := hostSpecs()

	want := []string{"claude", "codex", "gemini", "agy", "cursor", "opencode", "kilo", "pi", "dsh", "omp"}
	if len(specs) != len(want) {
		t.Fatalf("hostSpecs length = %d, want %d", len(specs), len(want))
	}

	for i, id := range want {
		if specs[i].id != id {
			t.Fatalf("hostSpecs[%d] = %q, want %q", i, specs[i].id, id)
		}
	}

	for _, spec := range specs {
		if spec.id == "" || spec.binary == "" {
			t.Fatalf("incomplete host spec: %+v", spec)
		}

		if spec.npm != "" && spec.pin == "" {
			t.Fatalf("host spec %s installs from npm without a pin", spec.id)
		}

		if len(spec.configDirs) == 0 || spec.fixtureManifest == "" {
			t.Fatalf("host spec %s lacks config/fixture data", spec.id)
		}

		// An oracle is either usable, or its absence carries the reason the
		// scenarios print instead of shelling out.
		if len(spec.listArgs) == 0 && spec.noOracleReason == "" {
			t.Fatalf("host spec %s has no listing oracle and no reason", spec.id)
		}

		if len(spec.canonKinds) == 0 {
			t.Fatalf("host spec %s has no canon component kinds", spec.id)
		}

		if spec.canonHooksBlocked == "" && spec.hooksFile == "" {
			t.Fatalf("host spec %s takes hooks but names no hooks document", spec.id)
		}

		if slices.Contains(spec.canonKinds, "mcp") && spec.mcpFile == "" {
			t.Fatalf("host spec %s takes MCP but names no MCP document", spec.id)
		}

		if spec.canonHooksBlocked == "" && !slices.Contains(spec.canonKinds, "hook") {
			t.Fatalf("host spec %s has no hook component and no blocked reason", spec.id)
		}

		if spec.adoptByPath && spec.adoptReason == "" && spec.noOracleReason == "" && len(spec.listArgs) == 0 {
			t.Fatalf("host spec %s adopts by path without a listing oracle", spec.id)
		}

		switch {
		case spec.npm == "" && spec.installNote == "":
			t.Fatalf("host spec %s has neither an npm package nor a reason", spec.id)
		case spec.npm != "" && spec.installNote != "":
			t.Fatalf("host spec %s installs from npm and still carries a note", spec.id)
		}

		refs := spec.installRefs()
		if spec.npm != "" && (len(refs) == 0 || refs[0] != spec.npm+"@"+spec.pin) {
			t.Fatalf("host spec %s install refs = %v, want the pinned npm package first", spec.id, refs)
		}

		if spec.npm == "" && len(refs) != 0 {
			t.Fatalf("host spec %s has no npm package but install refs %v", spec.id, refs)
		}
	}
}

// TestUnitCursorSpec pins the cursor entry: a host with a real CLI but no npm
// distribution and no installer, so its leg needs the binary already present.
func TestUnitCursorSpec(t *testing.T) {
	spec, ok := hostSpecByID("cursor")
	if !ok {
		t.Fatal("the matrix has no cursor entry")
	}

	if spec.npm != "" || spec.installNote == "" {
		t.Fatalf("cursor spec = npm %q, note %q; want no npm package and a reason", spec.npm, spec.installNote)
	}

	if spec.adoptReason == "" {
		t.Fatal("cursor has no manual install; the adopt leg needs a reason")
	}

	if !slices.Contains(spec.listArgs, "mcp") {
		t.Fatalf("cursor list args = %v, want the mcp listing", spec.listArgs)
	}

	captured, err := os.ReadFile(filepath.Join("..", "pkg", "host", "testdata", "cursor", "version-2026.06.15.txt"))
	if err != nil {
		t.Fatalf("read the captured cursor-agent version: %v", err)
	}

	if pin := strings.TrimSpace(string(captured)); pin != spec.pin {
		t.Fatalf("cursor pin = %q, want the live-captured %q", spec.pin, pin)
	}

	if marker := filepath.ToSlash(looseSkillMarker("/home", "cursor")); !strings.HasSuffix(marker, "/.cursor/skills/e2e-skill/SKILL.md") {
		t.Fatalf("cursor loose marker = %q", marker)
	}
}

// TestUnitOmpSpec pins the omp matrix entry: the host has no binary
// distribution named after it, so the package and its bun runtime are pinned
// together, and the shared ~/.agents root is scanned beside ~/.omp.
func TestUnitOmpSpec(t *testing.T) {
	spec, ok := hostSpecByID("omp")
	if !ok {
		t.Fatal("the matrix has no omp entry")
	}

	if spec.npm != "@oh-my-pi/pi-coding-agent" || spec.pin != "18.4.1" {
		t.Fatalf("omp spec pins %s@%s, want @oh-my-pi/pi-coding-agent@18.4.1", spec.npm, spec.pin)
	}

	if !slices.Contains(spec.installRefs(), "bun") {
		t.Fatalf("omp install refs = %v, want the bun launcher", spec.installRefs())
	}

	if !slices.Contains(spec.configDirs, ".agents") {
		t.Fatalf("omp config dirs = %v, want the shared ~/.agents root", spec.configDirs)
	}

	if marker := filepath.ToSlash(looseSkillMarker("/home", "omp")); !strings.HasSuffix(marker, "/.agents/skills/e2e-skill/SKILL.md") {
		t.Fatalf("omp loose marker = %q, want the shared ~/.agents skill tree", marker)
	}
}

// TestUnitGeminiStagedName pins the gemini manual install against the real
// grammar: gemini-cli 0.61.0 refuses an extension name carrying anything but
// letters, digits and dashes ("Invalid extension name: \"acme/e2e-fixture\""),
// so the adopt leg stages a copy named after the fixture and adopts that name.
func TestUnitGeminiStagedName(t *testing.T) {
	if strings.ContainsAny(fixtureName, "/\\ :@") {
		t.Fatalf("fixtureName %q is not a gemini-legal extension name", fixtureName)
	}

	for _, r := range fixtureName {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}

		t.Fatalf("fixtureName %q carries %q, which gemini-cli 0.61.0 rejects", fixtureName, r)
	}

	if !strings.ContainsAny(fixtureManifestName(), "/") {
		t.Fatalf("fixtureManifestName() = %q, want the owner-bearing id the synth rung needs", fixtureManifestName())
	}
}

// TestUnitCursorSpec also pins that the cursor leg skips the rung it cannot
// have, so a remote payload is never silently reported as a successful
// registration.
func TestUnitNoRegisterSpecsHaveAReason(t *testing.T) {
	for _, spec := range hostSpecs() {
		if spec.noRegisterReason == "" {
			continue
		}

		if spec.adoptReason == "" {
			t.Fatalf("host spec %s skips the archive scenario but claims a manual install", spec.id)
		}

		if strings.TrimSpace(spec.noRegisterReason) == "" {
			t.Fatalf("host spec %s carries a blank reason", spec.id)
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

// ---- the canon package leg (W3-E2E10) --------------------------------------

// canonName is the fixture package of the canon leg: one directory carrying
// every component beadle's canon bundle has (skills + agents + commands + MCP +
// hooks), the shape DESIGN §9.3 publishes as `local:<vault>/bundle`.
const (
	canonName  = "e2e-canon"
	canonID    = fixtureOwner + "/" + canonName
	canonSkill = "e2e-canon-skill"
	canonAgent = "e2e-canon-agent"
	canonCmd   = "e2e-canon-command"
	canonMCP   = "e2e-canon-mcp"
	canonHook  = "e2e-canon-hook"
)

// artifactDoc mirrors one receipt artifact (pkg/receipt §Artifact): the driver
// reads the receipt as an on-disk contract and never imports pkg/receipt.
type artifactDoc struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// receiptDoc mirrors the fields of one receipt the canon leg asserts on.
type receiptDoc struct {
	Schema    int           `json:"schema"`
	Package   string        `json:"package"`
	Host      string        `json:"host"`
	Scope     string        `json:"scope"`
	Strategy  string        `json:"strategy"`
	Artifacts []artifactDoc `json:"artifacts"`
}

// makeCanonFixture writes the canon-shaped directory package: a claude-format
// payload carrying all five components at once (the claude reader is the only
// one that yields skills + agents + commands + hooks + MCP, pkg/manifest
// claude.go:44-48; the Agent Plugins format carries skills and MCP only,
// agentplugins.go:53-54).
func makeCanonFixture(t *testing.T, env Env) string {
	t.Helper()

	dir := filepath.Join(env.Work, "canon")

	writeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), fmt.Sprintf(`{
  "name": %q,
  "version": "1.0.0",
  "description": "verger e2e canon fixture"
}
`, canonID))

	writeFile(t, filepath.Join(dir, "skills", canonSkill, "SKILL.md"),
		"---\nname: "+canonSkill+"\ndescription: e2e canon skill\n---\n\nCanon fixture body.\n")

	writeFile(t, filepath.Join(dir, "agents", canonAgent+".md"),
		"---\nname: "+canonAgent+"\ndescription: e2e canon agent\n---\n\nCanon agent body.\n")

	writeFile(t, filepath.Join(dir, "commands", canonCmd+".md"),
		"---\nname: "+canonCmd+"\ndescription: e2e canon command\n---\n\nCanon command body.\n")

	// One stdio server with no secret reference: a payload that needs a secret
	// fails the delivery with MissingSecretsError, which is a different leg.
	writeFile(t, filepath.Join(dir, ".mcp.json"), fmt.Sprintf(`{
  "mcpServers": {
    %q: {"command": "echo", "args": ["canon"]}
  }
}
`, canonMCP))

	// One approved-by-flag command hook in the claude dialect.
	writeFile(t, filepath.Join(dir, "hooks", "hooks.json"), fmt.Sprintf(`{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo %s"}]}
    ]
  }
}
`, canonHook))

	return dir
}

// canonReceiptPath is the receipt file of one cell: <home>/state/receipts/<pkg
// as directories>/<host>-<scope>.json (pkg/receipt cellPath).
func canonReceiptPath(env Env, host string) string {
	return filepath.Join(env.Home, ".verger", "state", "receipts",
		filepath.FromSlash(fixtureOwner), canonName, host+"-user.json")
}

// readReceipt loads one cell's receipt.
func readReceipt(t *testing.T, env Env, host string) receiptDoc {
	t.Helper()

	path := canonReceiptPath(env, host)

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is inside the driver's temp home
	if err != nil {
		t.Fatalf("e2e: read receipt %s: %v", path, err)
	}

	doc := receiptDoc{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("e2e: parse receipt %s: %v\n%s", path, err, data)
	}

	return doc
}

// receiptKinds is the set of component kinds a receipt records. Record-level
// artifacts collapse onto their component (an `mcp-record` is the per-record
// ownership claim of an `mcp` server, dsh writes those instead of one entry),
// and the document-level `patch-document` claim is not a component.
func receiptKinds(doc receiptDoc) []string {
	kinds := make([]string, 0, len(doc.Artifacts))

	for _, artifact := range doc.Artifacts {
		kind := strings.TrimSuffix(artifact.Kind, "-record")
		if kind == "patch-document" {
			continue
		}

		kinds = append(kinds, kind)
	}

	slices.Sort(kinds)

	return slices.Compact(kinds)
}

// assertReceiptKinds fails when the receipt is missing a kind the host spec
// expects, or carries a kind the host cannot take.
func assertReceiptKinds(t *testing.T, doc receiptDoc, spec hostSpec) {
	t.Helper()

	got := receiptKinds(doc)
	want := slices.Clone(spec.canonKinds)
	slices.Sort(want)

	// Always log the observed kinds, on every platform and whether or not the
	// assertions below pass. A kind-set difference between two platforms is
	// the first question anyone asks, and it cannot be answered from a failure
	// message when the other platform went green. The fingerprint says WHICH
	// build of this file ran, so a stale driver can no longer be mistaken for
	// a platform difference — which is not hypothetical: a Linux leg once went
	// green against a driver source nine lines older than the tree and read
	// exactly like a platform bug.
	t.Logf("e2e: %s receipt kinds %v (spec expects %v) [%s]", spec.id, got, want, driverFingerprint)

	for _, kind := range want {
		if !slices.Contains(got, kind) {
			t.Fatalf("e2e: %s receipt kinds %v are missing %q (artifacts %+v)", spec.id, got, kind, doc.Artifacts)
		}
	}

	for _, kind := range got {
		if kind == "rule" {
			continue // a rules component the canon fixture does not carry
		}

		if !slices.Contains(want, kind) {
			t.Fatalf("e2e: %s receipt records kind %q, which its host spec does not list (kinds %v)", spec.id, kind, got)
		}
	}
}

// assertArtifactsOnDisk proves every recorded artifact still exists, and for a
// whole file that verger wrote alone, that its bytes still match the receipt.
// A config-document artifact (mcp|hook|patch-document) records the digest of
// the value verger wrote inside a host document the user may also edit
// (pkg/host/loose.go:1385-1392), so its claim is the value's presence, not the
// file's bytes.
func assertArtifactsOnDisk(t *testing.T, doc receiptDoc) {
	t.Helper()

	for _, artifact := range doc.Artifacts {
		if artifact.Path == "" {
			continue // record ops carry no filesystem target
		}

		// A record artifact names a value inside a document, not a path: the
		// scheme-shaped target (dsh://patch/…, cursor://hooks/…) is asserted
		// through the document itself.
		if !filepath.IsAbs(artifact.Path) {
			continue
		}

		if !fileExists(artifact.Path) {
			t.Fatalf("e2e: receipt artifact %s %q is missing on disk: %s", artifact.Kind, artifact.Name, artifact.Path)
		}

		switch artifact.Kind {
		case "skill", "agent", "command", "rule":
			// A skill is recorded as its directory (a tree digest), the other
			// kinds as one file.
			info, err := os.Stat(artifact.Path)
			if err != nil {
				t.Fatalf("e2e: stat %s: %v", artifact.Path, err)
			}

			if info.IsDir() {
				assertTreeNotEmpty(t, artifact.Path)

				continue
			}

			if artifact.Digest == "" {
				continue
			}

			got, err := digestFile(artifact.Path)
			if err != nil {
				t.Fatalf("e2e: digest %s: %v", artifact.Path, err)
			}

			if got != artifact.Digest {
				t.Fatalf("e2e: %s digest = %s, receipt records %s", artifact.Path, got, artifact.Digest)
			}
		case "mcp":
			// The receipt records a synthetic URI for an MCP server, so the
			// host's own document is the fact (assertMCPDocument).
		default:
			// hook and patch-document records name the document itself; the
			// value-level assertion lives in assertHookSurface.
		}
	}
}

// assertMCPDocument proves the fixture's MCP server landed in the document the
// host itself reads, and is gone after remove.
func assertMCPDocument(t *testing.T, env Env, spec hostSpec, want bool) {
	t.Helper()

	if spec.mcpFile == "" {
		return
	}

	file := filepath.Join(env.Home, filepath.FromSlash(spec.mcpFile))

	if !want {
		if !fileExists(file) {
			return
		}

		data, err := os.ReadFile(file) //nolint:gosec // G304: temp home
		if err != nil {
			t.Fatalf("e2e: read %s: %v", file, err)
		}

		if strings.Contains(string(data), canonMCP) {
			t.Fatalf("e2e: %s still carries the MCP server %s after remove:\n%s", file, canonMCP, data)
		}

		return
	}

	data, err := os.ReadFile(file) //nolint:gosec // G304: temp home
	if err != nil {
		t.Fatalf("e2e: read MCP document %s: %v", file, err)
	}

	if !strings.Contains(string(data), canonMCP) {
		t.Fatalf("e2e: %s does not carry the MCP server %s:\n%s", file, canonMCP, data)
	}
}

// assertTreeNotEmpty fails when a delivered skill directory carries no file at
// all (the receipt records the tree, so the tree must still hold something).
func assertTreeNotEmpty(t *testing.T, dir string) {
	t.Helper()

	found := false

	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // a missing subtree is reported by the caller
		}

		found = true

		return fs.SkipAll
	})

	if !found {
		t.Fatalf("e2e: delivered skill tree %s is empty", dir)
	}
}

// digestFile is the sha256 of a file, hex encoded (the receipt digest shape).
func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller passes a receipt path
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// assertHookSurface proves the hook component either landed in the host's hooks
// document or was refused with the reason the host spec documents.
func assertHookSurface(t *testing.T, env Env, spec hostSpec, notes []string, want bool) {
	t.Helper()

	if spec.canonHooksBlocked != "" {
		if !want {
			return
		}

		// The host takes no hook component: the install report must say so
		// (pkg/host/loose.go:909-912) and the hook must not appear anywhere.
		joined := strings.Join(notes, "\n")
		if !strings.Contains(joined, "hook") {
			t.Fatalf("e2e: %s blocks hooks (%s) but no note says so; notes:\n%s", spec.id, spec.canonHooksBlocked, joined)
		}

		if strings.Contains(joined, "1 hook(s) skipped") && !strings.Contains(joined, spec.canonHooksBlocked) {
			t.Fatalf("e2e: %s note does not carry the documented reason %q:\n%s", spec.id, spec.canonHooksBlocked, joined)
		}

		file := filepath.Join(env.Home, filepath.FromSlash(spec.hooksFile))
		if fileExists(file) {
			data, err := os.ReadFile(file) //nolint:gosec // G304: temp home
			if err != nil {
				t.Fatalf("e2e: read %s: %v", file, err)
			}

			if strings.Contains(string(data), canonHook) {
				t.Fatalf("e2e: %s claims to block hooks but %s carries %s", spec.id, file, canonHook)
			}
		}

		return
	}

	file := filepath.Join(env.Home, filepath.FromSlash(spec.hooksFile))

	if !want {
		if fileExists(file) {
			data, err := os.ReadFile(file) //nolint:gosec // G304: temp home
			if err != nil {
				t.Fatalf("e2e: read %s: %v", file, err)
			}

			if strings.Contains(string(data), canonHook) {
				t.Fatalf("e2e: %s still mentions %s after remove:\n%s", file, canonHook, data)
			}
		}

		return
	}

	data, err := os.ReadFile(file) //nolint:gosec // G304: temp home
	if err != nil {
		t.Fatalf("e2e: read hooks document %s: %v", file, err)
	}

	if !strings.Contains(string(data), canonHook) {
		t.Fatalf("e2e: %s does not carry the canon hook %s:\n%s", file, canonHook, data)
	}
}

// TestE2ECanonPackageScenario delivers the canon-shaped directory package (a
// `local:` payload: skills + agents + commands + MCP + hooks) to one host,
// asserts what the receipt claims and what is on disk, then removes it and
// proves the host is clean again. It is the W3 leg for GAP-16: beadle's canon
// becomes exactly this package (DESIGN §9.3).
func TestE2ECanonPackageScenario(t *testing.T) {
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

	makeCanonFixture(t, env)

	cell, rec := installCanon(t, env, spec)
	assertCanonLanded(t, env, spec, cell, rec)
	assertCanonRemoved(t, env, spec, cell, rec)
}

// installCanon installs the canon fixture and returns the status cell and the
// receipt it wrote. The ref is `./canon`: the grammar accepts only a relative
// local path — an absolute path, `local:<abs>` and `file:<abs>` are rejected
// (finding F1 in W3-E2E10-1.md).
func installCanon(t *testing.T, env Env, spec hostSpec) (cellDoc, receiptDoc) {
	t.Helper()

	out, code := runVerger(t, env, "install", "./canon", "-y", "--hooks", "yes", "--hosts", spec.id, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger install ./canon -> %d\n%s", code, out)
	}

	cell := assertStatus(t, env, spec.id, "current")
	if cell.Strategy != "loose" {
		t.Fatalf("e2e: a local payload chose %q, want loose (DESIGN §2.1)", cell.Strategy)
	}

	if !strings.Contains(cell.Package, canonName) {
		t.Fatalf("e2e: status cell package %q does not name the canon fixture", cell.Package)
	}

	// The delivery notes live in the executed-plan document, not in the
	// receipt-backed status, so carry them on the returned cell.
	cell.Notes = canonNotes(t, []byte(out), spec.id)

	return cell, readReceipt(t, env, spec.id)
}

// canonNotes reads the delivery notes of one host from an executed-plan
// document: they live in the install report, not in the receipt-backed status.
func canonNotes(t *testing.T, out []byte, host string) []string {
	t.Helper()

	doc, err := parseCells(out)
	if err != nil {
		t.Fatalf("e2e: parse install report: %v\n%s", err, out)
	}

	for _, cell := range doc.Cells {
		if cell.Host == host {
			return cell.Notes
		}
	}

	return nil
}

// assertCanonLanded proves what the receipt claims and what the host holds.
func assertCanonLanded(t *testing.T, env Env, spec hostSpec, cell cellDoc, rec receiptDoc) {
	t.Helper()

	assertReceiptKinds(t, rec, spec)
	assertArtifactsOnDisk(t, rec)
	assertLooseMarkerFor(t, env, spec, canonSkill)

	if spec.noOracleReason == "" {
		requireOracle(t, env, spec, canonID, false)
	}

	assertHookSurface(t, env, spec, cell.Notes, true)

	if slices.Contains(spec.canonKinds, "mcp") {
		assertMCPDocument(t, env, spec, true)
	}
}

// assertCanonRemoved removes the package and proves every trace is gone.
func assertCanonRemoved(t *testing.T, env Env, spec hostSpec, cell cellDoc, rec receiptDoc) {
	t.Helper()

	out, code := runVerger(t, env, "remove", cell.Package, "--json")
	if code != 0 {
		t.Fatalf("e2e: verger remove %s -> %d\n%s", cell.Package, code, out)
	}

	assertNotCurrent(t, env, spec.id)

	if fileExists(canonReceiptPath(env, spec.id)) {
		t.Fatalf("e2e: the receipt survived remove: %s", canonReceiptPath(env, spec.id))
	}

	for _, artifact := range rec.Artifacts {
		if artifact.Path == "" || !filepath.IsAbs(artifact.Path) {
			continue
		}

		switch artifact.Kind {
		case "skill", "agent", "command", "rule":
			if fileExists(artifact.Path) {
				t.Fatalf("e2e: remove left the %s artifact %s", artifact.Kind, artifact.Path)
			}
		}
	}

	assertHookSurface(t, env, spec, nil, false)

	if slices.Contains(spec.canonKinds, "mcp") {
		assertMCPDocument(t, env, spec, false)
	}

	assertNoResidue(t, env, spec)
}

// assertLooseMarkerFor proves one named loose skill landed for the host.
func assertLooseMarkerFor(t *testing.T, env Env, spec hostSpec, skill string) {
	t.Helper()

	marker := looseSkillMarkerFor(env.Home, spec.id, skill)
	if marker == "" {
		t.Skipf("e2e: %s has no loose skills surface", spec.id)
	}

	if !fileExists(marker) {
		t.Fatalf("e2e: loose delivery did not write %s", marker)
	}
}

// looseSkillMarkerFor is the host-loose path of one named skill: the single
// table both fixture scenarios read, so a host cannot be covered by one leg and
// missed by the other.
func looseSkillMarkerFor(home, host, skill string) string {
	rel := map[string]string{
		"claude":   ".claude/skills/%s/SKILL.md",
		"codex":    ".agents/skills/%s/SKILL.md",
		"gemini":   ".gemini/skills/%s/SKILL.md",
		"omp":      ".agents/skills/%s/SKILL.md",
		"cursor":   ".cursor/skills/%s/SKILL.md",
		"opencode": ".config/opencode/skills/%s/SKILL.md",
		"kilo":     ".config/kilo/skills/%s/SKILL.md",
		"pi":       ".pi/agent/skills/%s/SKILL.md",
		"agy":      ".agents/skills/%s/SKILL.md",
		"dsh":      ".dsh/skills/%s/SKILL.md",
	}[host]

	if rel == "" {
		return ""
	}

	return filepath.Join(home, filepath.FromSlash(fmt.Sprintf(rel, skill)))
}

// hostOnlyPath builds a PATH with exactly one host CLI in it: a symlink to the
// binary under test in a fresh directory. Every other adapter then fails to
// detect its host, which is what an adopt fan-out needs to stay scoped.
func hostOnlyPath(t *testing.T, env Env, spec hostSpec) string {
	t.Helper()

	bin, err := lookPath(env, spec.binary)
	if err != nil {
		t.Skipf("e2e: %s CLI is not installed (%s)", spec.binary, spec.installHint())
	}

	dir := t.TempDir()

	if err := os.Symlink(bin, filepath.Join(dir, spec.binary)); err != nil {
		t.Fatalf("e2e: symlink %s: %v", spec.binary, err)
	}

	// A host CLI is a script with an interpreter (`#!/usr/bin/env node` for the
	// npm ones, `#!/usr/bin/env bun` for omp's launcher), so the interpreter
	// must stay reachable — but its own directory holds every npm-installed
	// host (kilo, pi and dsh live in the same bin as node), so only symlinks to
	// the interpreters go into the isolated directory. /usr/bin and /bin carry
	// the shell utilities and no host CLI.
	for _, runtime := range []string{"node", "bun"} {
		path, err := lookPath(env, runtime)
		if err != nil {
			continue
		}

		if linkErr := os.Symlink(path, filepath.Join(dir, runtime)); linkErr != nil {
			t.Fatalf("e2e: symlink %s: %v", runtime, linkErr)
		}
	}

	return strings.Join([]string{dir, "/usr/bin", "/bin"}, string(os.PathListSeparator))
}

// lookPath resolves a host binary the way the child will: env.PATH when the
// driver pins one (a leg that needs a specific node toolchain), the driver's
// own PATH otherwise.
func lookPath(env Env, name string) (string, error) {
	if env.PATH == "" {
		return exec.LookPath(name)
	}

	for _, dir := range filepath.SplitList(env.PATH) {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}

	return "", exec.ErrNotFound
}
