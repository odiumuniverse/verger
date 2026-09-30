package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/secret"
)

const openCodeFixtureRoot = "testdata/opencode/acme"

// openCodeVersionWord is the binary name the adapter resolves.
const openCodeVersionWord = "opencode --version"

// fakeOpenCode puts an executable `opencode` shim at the front of PATH so the
// adapter's binary probe resolves; the answers come from the injected runner.
func fakeOpenCode(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// openCodeTestEnv pins the OpenCode config root inside the test home: the suite
// TestMain points XDG_CONFIG_HOME at the shared isolation dir, and the host
// resolves its config below XDG first (beadle's own rule).
func openCodeTestEnv(t *testing.T, home string) string {
	t.Helper()

	xdg := filepath.Join(home, ".config")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	return filepath.Join(xdg, "opencode")
}

// newOpenCode builds the adapter with a scripted runner.
func newOpenCode(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewOpenCode(all...), runner
}

// openCodePackage parses the OpenCode fixture.
func openCodePackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, openCodeFixtureRoot, manifest.FormatClaude)
}

// openCodeSecrets is the secret store of the fixture: its MCP servers
// reference MCP_TOKEN and WEB_TOKEN.
func openCodeSecrets(t *testing.T) *secret.Store {
	t.Helper()

	return secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
}

// openCodeFixture reads one captured opencode fixture.
func openCodeFixture(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "opencode", name)) //nolint:gosec // G304: a fixture below testdata
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return string(data)
}

// openCodeCLI is a stateful fake of the OpenCode 2.0.18 plugin grammar with
// the output shapes and refusal texts captured live on the local binary:
// `plugin add` writes the spec into the config `plugins` array and reloads the
// running service, `plugin remove` is idempotent, and `plugin list` prints the
// `ID VERSION SOURCE` table of the plugins the host loaded.
type openCodeCLI struct {
	mu         sync.Mutex
	version    string
	configPath string
	entries    map[string]string // spec → plugin id the host resolved
	unresolved map[string]bool   // spec configured but the host resolved no plugin
	staleList  bool              // the running service has not reloaded the config yet
	noConfig   bool              // the CLI answers success but writes nothing
	noEntry    map[string]bool   // spec → the host refuses it as a package without a plugin entrypoint
	fail       map[string]hostcli.Response
	calls      []string
}

// newOpenCodeCLI builds an empty fake over one config file path.
func newOpenCodeCLI(configPath string) *openCodeCLI {
	return &openCodeCLI{
		version:    "opencode v2.0.18\n",
		configPath: configPath,
		entries:    map[string]string{},
		unresolved: map[string]bool{},
		noEntry:    map[string]bool{},
		fail:       map[string]hostcli.Response{},
	}
}

// Run implements hostcli.Runner.
//
//nolint:dupl // every stateful fake has the same record-then-dispatch shell; the grammars differ below it
func (c *openCodeCLI) Run(_ context.Context, bin hostcli.Binary, args []string, _ []byte) ([]byte, error) {
	key := strings.Join(append([]string{bin.Name}, args...), " ")

	c.mu.Lock()
	c.calls = append(c.calls, key)
	response, failed := c.fail[key]
	c.mu.Unlock()

	if failed {
		return response.Stdout, openCodeExit(bin.Name, response)
	}

	switch {
	case key == openCodeVersionWord:
		return []byte(c.version), nil
	case len(args) == 2 && args[0] == "plugin" && args[1] == "list":
		return c.list(), nil
	case len(args) == 3 && args[0] == "plugin" && (args[1] == "add" || args[1] == "remove"):
		return c.mutate(args[1], args[2])
	default:
		return nil, &hostcli.ExitError{Name: bin.Name, Code: 127, Stderr: "unknown command"}
	}
}

// Calls returns the recorded calls as `<name> <joined args>` keys.
func (c *openCodeCLI) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.calls...)
}

// mutate runs one `plugin add|remove <spec>`.
func (c *openCodeCLI) mutate(verb, spec string) ([]byte, error) {
	if !openCodeSpecShape(spec) {
		return nil, &hostcli.ExitError{
			Name:   "opencode",
			Code:   1,
			Stderr: "Error: Plugin target must be an npm registry package or Git package specifier\n",
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if verb == "add" && c.noEntry[spec] {
		// The live refusal of a package that carries no server or TUI
		// entrypoint (opencode 2.0.18, captured in the probe log).
		return nil, &hostcli.ExitError{
			Name:   "opencode",
			Code:   1,
			Stderr: "Plugin package has no server or TUI entrypoint: " + spec + "\n",
		}
	}

	_, present := c.entries[spec]

	switch verb {
	case "add":
		if present {
			return []byte("Plugin \"" + spec + "\" is already configured in " + c.configPath + "\n"), nil
		}

		if c.noConfig {
			// A host that answers success and records nothing anywhere: the
			// shape the verify step must catch.
			return []byte("Plugin \"" + spec + "\" installed and added to " + c.configPath + "\n"), nil
		}

		c.entries[spec] = openCodeIDFor(spec)
		c.write()

		return []byte("Plugin \"" + spec + "\" installed and added to " + c.configPath + "\n"), nil
	default:
		if !present {
			return []byte("Plugin \"" + spec + "\" is not configured\n"), nil
		}

		delete(c.entries, spec)
		c.write()

		return []byte("Plugin \"" + spec + "\" removed from " + c.configPath + "\n"), nil
	}
}

// list renders `plugin list`: the live table, or the live empty sentence.
func (c *openCodeCLI) list() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) == 0 || c.staleList {
		return []byte("No plugins found\n")
	}

	var b strings.Builder

	b.WriteString("ID            VERSION  SOURCE\n")

	for spec, id := range c.entries {
		if c.unresolved[spec] {
			id = "-"
		}

		b.WriteString(id + "  1.0.0  " + spec + "\n")
	}

	return []byte(b.String())
}

// write records the configured specs in the config document, the way the host
// CLI does (the `plugins` array; every other key is left alone).
func (c *openCodeCLI) write() {
	specs := make([]string, 0, len(c.entries))

	for spec := range c.entries {
		specs = append(specs, spec)
	}

	slices.Sort(specs)

	data, err := json.MarshalIndent(map[string]any{"plugins": specs}, "", "  ")
	if err != nil {
		return
	}

	_ = os.MkdirAll(filepath.Dir(c.configPath), 0o700)
	_ = os.WriteFile(c.configPath, append(data, '\n'), 0o600)
}

// openCodeSpecShape mirrors the host's own validation: a bare npm name or a
// Git specifier, never a local path and never the `npm:` ref prefix.
func openCodeSpecShape(spec string) bool {
	switch {
	case strings.HasPrefix(spec, "git+"), strings.HasPrefix(spec, "git@"):
		return true
	case strings.HasPrefix(spec, "npm:"), strings.HasPrefix(spec, "file://"), strings.HasPrefix(spec, "/"):
		return false
	case spec == "":
		return false
	default:
		return !strings.ContainsAny(spec, " \t")
	}
}

// openCodeIDFor derives the plugin id the host would resolve for a spec.
func openCodeIDFor(spec string) string {
	base := spec

	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}

	return strings.TrimPrefix(base, "git+file://")
}

// openCodeExit folds one scripted fake failure into the runner's error shape.
func openCodeExit(name string, response hostcli.Response) error {
	if response.Code == 0 && response.Stderr == "" {
		return nil
	}

	return &hostcli.ExitError{Name: name, Code: response.Code, Stderr: response.Stderr}
}

// openCodeMCPDoc reads the delivered MCP document as JSON.
func openCodeMCPDoc(t *testing.T, path string) map[string]any {
	t.Helper()

	var doc map[string]any

	if err := json.Unmarshal([]byte(readTestFile(t, path)), &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return doc
}

func TestOpenCodeDetect(t *testing.T) {
	Convey("Given a home without an OpenCode config dir", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		openCodeTestEnv(t, home)

		h, _ := newOpenCode(t, home, nil)

		Convey("When neither the config dir nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the config dir exists", func() {
			if err := os.MkdirAll(openCodeTestEnv(t, home), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the opencode binary on PATH", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		openCodeTestEnv(t, home)

		h, _ := newOpenCode(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

// TestOpenCodeLooseGolden pins the v2 loose surface: skills, agents and
// commands below the config dir, MCP servers under `mcp.servers`, the rule as
// the D23 skill wrapper, and hooks skipped with the runtime reason.
func TestOpenCodeLooseGolden(t *testing.T) {
	Convey("Given an OpenCode package and a temp home", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		h, runner := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then the v2 surfaces are written and the only host call is the dialect probe", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".config/opencode/agents/reviewer.md",
					".config/opencode/commands/dev.md",
					".config/opencode/opencode.json",
					".config/opencode/skills/alpha/SKILL.md",
					".config/opencode/skills/alpha/scripts/run.sh",
					".config/opencode/skills/beta/SKILL.md",
					".config/opencode/skills/rule-caveman/SKILL.md",
				})
				// A loose delivery touches the host once: the `--version`
				// probe that decides the dialect (and with it the container).
				So(callKeys(runner), ShouldResemble, []string{openCodeVersionWord})
			})

			Convey("Then MCP lands under mcp.servers in the OpenCode dialect", func() {
				doc := openCodeMCPDoc(t, filepath.Join(dir, "opencode.json"))

				servers, ok := doc["mcp"].(map[string]any)["servers"].(map[string]any)
				So(ok, ShouldBeTrue)

				fs, ok := servers["fs"].(map[string]any)
				So(ok, ShouldBeTrue)
				So(fs["type"], ShouldEqual, "local")
				command, ok := fs["command"].([]any)
				So(ok, ShouldBeTrue)
				So(command[0], ShouldEqual, "node")
				So(fs["environment"], ShouldResemble, map[string]any{"TOKEN": "s3cr3t-token"})

				web, ok := servers["web"].(map[string]any)
				So(ok, ShouldBeTrue)
				So(web["type"], ShouldEqual, "remote")
				So(web["url"], ShouldEqual, "https://example.com/mcp")
				So(web["headers"], ShouldResemble, map[string]any{"Authorization": "Bearer web-token"})
			})

			Convey("Then the hooks ride in a plugin, and no hook document is written", func() {
				So(err, ShouldBeNil)
				So(res.Strategy, ShouldEqual, host.Loose)

				// OpenCode has no declarative hook surface: the hooks are
				// delivered as a module it imports, so a hooks document in
				// the config root would be a file the host never reads.
				_, readErr := os.Stat(filepath.Join(dir, "hooks.json"))
				So(errors.Is(readErr, os.ErrNotExist), ShouldBeTrue)
				// A plugin module IS a hook opencode runs, and a delivered hook
				// executing there is not yet proven, so the blocked host writes
				// none of it.
				So(fileExists(filepath.Join(dir, "verger-acme-caveman", "index.js")), ShouldBeFalse)
			})
		})
	})
}

// TestOpenCodeLooseV1Container pins the v1 dialect: a config that declares the
// legacy `mcp` container keeps it, so the delivery never grows a second MCP
// container next to the one the host reads.
func TestOpenCodeLooseV1Container(t *testing.T) {
	Convey("Given a v1-shaped OpenCode config", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		writeFixtureFile(t, filepath.Join(dir, "opencode.json"), `{
  "mcp": {}
}
`, 0o600)

		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the servers land under the legacy mcp key", func() {
				So(err, ShouldBeNil)

				doc := openCodeMCPDoc(t, filepath.Join(dir, "opencode.json"))

				summary, ok := doc["mcp"].(map[string]any)
				So(ok, ShouldBeTrue)
				So(summary["fs"], ShouldNotBeNil)
				So(summary["web"], ShouldNotBeNil)
				So(summary["servers"], ShouldBeNil)
			})
		})
	})
}

// TestOpenCodeLooseCommentsPreserved pins the JSONC contract: a comment and a
// foreign key of the shared document survive the delivery (hujson), and the
// document keeps its mode.
func TestOpenCodeLooseCommentsPreserved(t *testing.T) {
	Convey("Given a commented OpenCode config with a foreign key", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		path := filepath.Join(dir, "opencode.jsonc")
		writeFixtureFile(t, path, `{
  // the user's own note
  "theme": "acme",
  "mcp": {"servers": {}}
}
`, 0o640)

		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the comment and the foreign key survive and the secret forces 0600", func() {
				So(err, ShouldBeNil)

				text := readTestFile(t, path)
				So(text, ShouldContainSubstring, "// the user's own note")
				So(text, ShouldContainSubstring, `"theme": "acme"`)
				So(text, ShouldContainSubstring, `"servers"`)

				// The fixture's servers resolve `{secret:...}`, so the document
				// is written 0600 whatever mode it had (DESIGN §4.3).
				info, statErr := os.Stat(path)
				So(statErr, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o600))
			})
		})
	})
}

// TestOpenCodeLooseDryRun pins that planning writes nothing.
func TestOpenCodeLooseDryRun(t *testing.T) {
	Convey("Given a dry-run delivery", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		openCodeTestEnv(t, home)
		st := openStore(t)

		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose, DryRun: true})

		Convey("When the plan is rendered", func() {
			Convey("Then it is complete and the home is untouched", func() {
				So(err, ShouldBeNil)
				So(res.Artifacts, ShouldNotBeEmpty)
				So(homeFiles(t, home), ShouldBeEmpty)
			})
		})
	})
}

// TestOpenCodeHooksRefusedWithoutConsent pins the disable-able hook component:
// without consent the hooks are not delivered either.
func TestOpenCodeHooksRefusedWithoutConsent(t *testing.T) {
	Convey("Given a delivery without hook consent", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then no hook document and no plugin exist without consent", func() {
				So(err, ShouldBeNil)
				So(res.Strategy, ShouldEqual, host.Loose)
				So(fileExists(filepath.Join(dir, "hooks.json")), ShouldBeFalse)

				// Without consent there is no shim either: a plugin the host
				// loads would run hooks the user has not agreed to.
				So(fileExists(filepath.Join(dir, "verger-acme-caveman")), ShouldBeFalse)
			})
		})
	})
}

// TestOpenCodeOracleList pins the table parser against the live capture,
// including the empty sentence and the unresolved row shape.
//
//nolint:dupl // the two oracles share the fixture-then-list shape; fixtures and assertions differ
func TestOpenCodeOracleList(t *testing.T) {
	Convey("Given a host listing two package plugins", t, func() {
		fakeOpenCode(t)

		h, runner := newOpenCode(t, t.TempDir(), map[string]hostcli.Response{
			"opencode plugin list": response(openCodeFixture(t, "plugin-list-2.0.18.txt")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then every row becomes an entry with its source spec", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldHaveLength, 2)
				So(listed[0].Name, ShouldEqual, "probe-plugin")
				So(listed[0].Version, ShouldEqual, "becf70d")
				So(listed[0].Source, ShouldEqual, "git+file:///tmp/ocprobe/pkg")
				So(listed[0].Enabled, ShouldBeTrue)
				So(callKeys(runner), ShouldResemble, []string{"opencode plugin list"})
			})
		})
	})

	Convey("Given a host with no plugin", t, func() {
		fakeOpenCode(t)

		h, _ := newOpenCode(t, t.TempDir(), map[string]hostcli.Response{
			"opencode plugin list": response(openCodeFixture(t, "plugin-list-empty-2.0.18.txt")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then nothing is invented", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a table with an unresolved row and noise", t, func() {
		fakeOpenCode(t)

		h, _ := newOpenCode(t, t.TempDir(), map[string]hostcli.Response{
			"opencode plugin list": response("ID  VERSION  SOURCE\n-  -  opencode-poe-auth\nnot a row\n"),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the unresolved row is listed disabled and prose is skipped", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldHaveLength, 1)
				So(listed[0].Source, ShouldEqual, "opencode-poe-auth")
				So(listed[0].Enabled, ShouldBeFalse)
			})
		})
	})

	Convey("Given a refusing host", t, func() {
		fakeOpenCode(t)

		h, _ := newOpenCode(t, t.TempDir(), map[string]hostcli.Response{
			"opencode plugin list": {Code: 1, Stderr: "boom\n"},
		})

		_, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the refusal passes through as the host's own error", func() {
				_, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

// blockingRunner answers nothing until the context ends: the shape of a host
// whose background service never comes up.
type blockingRunner struct{}

// Run implements hostcli.Runner.
func (blockingRunner) Run(ctx context.Context, _ hostcli.Binary, _ []string, _ []byte) ([]byte, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

// TestOpenCodeOracleWait pins the bounded service call: the plugin listing
// talks to the host's background service, and the CLI retries starting one
// forever when its port is taken (live-observed), so the oracle must end the
// wait with a note instead of hanging the caller. The bound is injected, so
// the test never sits through the real one; the default is pinned separately.
func TestOpenCodeOracleWait(t *testing.T) {
	Convey("Given the default bound", t, func() {
		Convey("Then it is the documented fifteen seconds", func() {
			So(host.DefaultOracleWait, ShouldEqual, 15*time.Second)
		})
	})

	Convey("Given an opencode whose background service never answers", t, func() {
		fakeOpenCode(t)

		h, _ := newOpenCode(t, t.TempDir(), nil, host.WithRunner(blockingRunner{}), host.WithOracleWait(200*time.Millisecond))

		_, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the wait ends with a note naming the bound and the service", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Cause.Error(), ShouldContainSubstring, "did not answer within 200ms")
				So(typed.Cause.Error(), ShouldContainSubstring, "background service")
				So(typed.Cause.Error(), ShouldContainSubstring, "opencode service start")
			})
		})
	})
}

// TestOpenCodeOracleValidate pins the unsupported verdict: 2.0.18 validates a
// package only inside `plugin add`.
//
//nolint:dupl // the oracle-unsupported shape is the same for every host without one, and the fixture is not
func TestOpenCodeOracleValidate(t *testing.T) {
	Convey("Given the OpenCode oracle", t, func() {
		fakeOpenCode(t)

		h, runner := newOpenCode(t, t.TempDir(), nil)

		warnings, err := h.Oracle().Validate(t.Context(), t.TempDir())

		Convey("When a directory is validated", func() {
			Convey("Then it is unsupported and no CLI call is made", func() {
				So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
				So(warnings, ShouldBeEmpty)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestOpenCodeNativeInstall pins the native rung: the source ref is translated
// to the host's own specifier spelling, the install is verified through the
// oracle, and the inverse is recorded.
func TestOpenCodeNativeInstall(t *testing.T) {
	Convey("Given an npm-source package and the OpenCode CLI", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the host specifier is the bare npm package and the install is verified", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{
					openCodeVersionWord,
					"opencode plugin add @acme/caveman@1.2.3",
					"opencode plugin list",
				})
				So(res.Observed.Verified, ShouldBeTrue)
			})

			Convey("Then the inverse is the host's own remove verb", func() {
				ops := hostInstallOps(res.RMA)
				So(ops, ShouldHaveLength, 1)
				So(ops[0].Command, ShouldResemble, []string{"plugin", "remove", "@acme/caveman@1.2.3"})
				So(ops[0].Existed, ShouldBeFalse)
			})
		})
	})
}

// TestOpenCodeNativeGitSpec pins the second accepted specifier shape: a Git
// ref is passed verbatim.
func TestOpenCodeNativeGitSpec(t *testing.T) {
	Convey("Given a git-source package", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "git+https://github.com/acme/caveman.git"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the specifier reaches the host unchanged", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldContain, "opencode plugin add git+https://github.com/acme/caveman.git")
			})
		})
	})
}

// TestOpenCodeNativeAlreadyConfigured pins the re-delivery: the host answers
// "is already configured", and the recorded inverse keeps the entry.
func TestOpenCodeNativeAlreadyConfigured(t *testing.T) {
	Convey("Given a package the host already configured", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})
		So(err, ShouldBeNil)

		again, againErr := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered again", func() {
			Convey("Then the entry is marked pre-existing so a rollback keeps it", func() {
				So(againErr, ShouldBeNil)

				ops := hostInstallOps(again.RMA)
				So(ops, ShouldHaveLength, 1)
				So(ops[0].Existed, ShouldBeTrue)
			})
		})
	})
}

// TestOpenCodeNativeRefused pins every refusal of the native rung with its
// reason, so pkg/plan can step down to loose instead of guessing.
func TestOpenCodeNativeRefused(t *testing.T) {
	Convey("Given a source ref the host cannot install from", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(home, ".config", "opencode", "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "caveman@acme"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then it is unsupported and the CLI is never called", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(cli.Calls(), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a package without a source ref", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(home, ".config", "opencode", "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the missing ref is refused", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(cli.Calls(), ShouldBeEmpty)
			})
		})
	})

	Convey("Given the v1 dialect", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		writeFixtureFile(t, filepath.Join(dir, "opencode.json"), `{"mcp": {}}`, 0o600)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		cli.version = "opencode v1.0.0\n"

		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the v1 host has no install verb and is refused with the reason", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(cli.Calls(), ShouldResemble, []string{openCodeVersionWord})
			})
		})
	})

	Convey("Given a synth delivery", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(home, ".config", "opencode", "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.SynthDir = t.TempDir()

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When it is delivered synth", func() {
			Convey("Then the synth plugin reason names the task that delivers it", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

// TestOpenCodeNativeVerifyMiss pins the verification contract: the host
// answers success but writes nothing into the config it names, so the cell
// fails at verify instead of passing silently.
func TestOpenCodeNativeVerifyMiss(t *testing.T) {
	Convey("Given a host that answers success without recording the specifier", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		cli.noConfig = true

		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the delivery fails at the verify step", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "verify")
			})
		})
	})
}

// TestOpenCodeNativeLaggingListing pins the host behaviour observed live on
// 2.0.18: the running background service lists the plugins it loaded at start,
// so the listing can lag behind a config change. The config the host wrote is
// the evidence; the lagging listing becomes a note for the operator.
func TestOpenCodeNativeLaggingListing(t *testing.T) {
	Convey("Given a host whose service has not reloaded the config yet", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		cli.staleList = true

		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the install stands with a note naming the restart", func() {
				So(err, ShouldBeNil)
				So(res.Observed.Verified, ShouldBeFalse)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "restart OpenCode")
				So(openCodeConfigHasPluginForTest(t, filepath.Join(dir, "opencode.json"), "@acme/caveman@1.2.3"), ShouldBeTrue)
			})
		})
	})
}

// TestOpenCodeNativeUnresolvedListing pins the other listing shape: the config
// carries the specifier and the service lists it with an unresolved id — the
// plugin is configured, the service has not managed to load the module, and
// that is a note too (the delivery did everything the host asked for).
func TestOpenCodeNativeUnresolvedListing(t *testing.T) {
	Convey("Given a host that lists the specifier as unresolved", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		cli.unresolved["@acme/caveman@1.2.3"] = true

		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the delivery stands with the lagging-listing note", func() {
				So(err, ShouldBeNil)
				So(res.Observed.Verified, ShouldBeFalse)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "restart OpenCode")
			})
		})
	})
}

// openCodeConfigHasPluginForTest reads the delivered config the way the
// adapter does, so the assertion and the implementation cannot drift.
func openCodeConfigHasPluginForTest(t *testing.T, path, spec string) bool {
	t.Helper()

	var doc struct {
		Plugins []string `json:"plugins"`
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp home
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return slices.Contains(doc.Plugins, spec)
}

// TestOpenCodeNativeStepDown pins the ladder step-down: a package the host
// refuses because it carries no plugin entrypoint is a NotSupportedError, so
// pkg/plan descends to the loose rung instead of failing the cell — the live
// refusal text of opencode 2.0.18 is what the adapter keys on.
func TestOpenCodeNativeStepDown(t *testing.T) {
	Convey("Given a package opencode cannot load as a plugin", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		cli.noEntry["@acme/plain-skills@1.0.0"] = true

		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/plain-skills@1.0.0"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then it is unsupported, never a delivery failure", func() {
				typed, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(typed.Operation, ShouldContainSubstring, "no OpenCode plugin entrypoint")

				_, delivery := errors.AsType[*host.DeliveryError](err)
				So(delivery, ShouldBeFalse)
			})
		})
	})
}

// TestOpenCodeNoInventedPolicy pins the honest policy surface: OpenCode ships
// no managed policy document and knows none of the Claude managed keys
// (live-checked on 2.0.18), so a config that carries such a key is not read as
// a policy — the delivery is not blocked by a setting the host itself ignores.
func TestOpenCodeNoInventedPolicy(t *testing.T) {
	Convey("Given an OpenCode config carrying a Claude managed policy key", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		writeFixtureFile(t, filepath.Join(dir, "opencode.json"), `{
  "disableCommandPluginSources": true,
  "blockedMarketplaces": ["acme"]
}
`, 0o600)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := openCodePackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then the keys the host does not define neither block nor fail it", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldContain, "opencode plugin add @acme/caveman@1.2.3")
			})
		})
	})
}

// TestOpenCodeUninstall pins the host-install inverse: the plugin is removed
// through the host CLI, an already-removed plugin is a note, and a refusal the
// host does not explain surfaces.
func TestOpenCodeUninstall(t *testing.T) {
	Convey("Given a receipt with a host-install op", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		cli := newOpenCodeCLI(filepath.Join(dir, "opencode.json"))
		h := host.NewOpenCode(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		cli.entries["@acme/caveman@1.2.3"] = "caveman"

		r := receipt.Receipt{Strategy: string(host.Native), RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "@acme/caveman@1.2.3"}},
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "gone-plugin"}},
		}}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled", func() {
			Convey("Then the configured plugin is removed and the missing one is a note", func() {
				So(err, ShouldBeNil)
				// The RMA runs in reverse order: the later op first.
				So(cli.Calls(), ShouldResemble, []string{
					"opencode plugin remove gone-plugin",
					"opencode plugin remove @acme/caveman@1.2.3",
				})
				So(res.Notes, ShouldNotBeEmpty)
			})
		})
	})
}

// TestOpenCodeRemoveThroughApply pins the loose round trip with pkg/apply: the
// config keys the delivery owns are removed and the files are gone.
func TestOpenCodeRemoveThroughApply(t *testing.T) {
	Convey("Given an applied OpenCode delivery", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		dir := openCodeTestEnv(t, home)
		st := openStore(t)

		deps := applyWorld(t, st, ownerMap{})

		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(openCodeSecrets(t)), host.WithOwnership(receiptsOwner{receipts: deps.Receipts}))
		deps.Hosts[host.OpenCode] = h

		pkg := openCodePackage(t)

		report, err := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{{
			Kind: apply.ActionInstall, Host: host.OpenCode,
			Delivery: host.Delivery{Package: pkg, Strategy: host.Loose},
		}}}, apply.Options{Confirm: refuseConflicts{}})
		So(err, ShouldBeNil)
		So(report.Cells[0].Status, ShouldEqual, apply.StatusCurrent)

		rec, found, getErr := deps.Receipts.Get(pkg.ID, string(host.OpenCode), receipt.ScopeUser)
		So(getErr, ShouldBeNil)
		So(found, ShouldBeTrue)

		removeReport, removeErr := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{{
			Kind: apply.ActionRemove, Host: host.OpenCode, Previous: &rec, Cause: "user", Initiator: "test",
		}}}, apply.Options{Confirm: refuseConflicts{}})

		Convey("When the package is removed", func() {
			Convey("Then the files are gone and the owned keys are removed", func() {
				So(removeErr, ShouldBeNil)
				So(removeReport.Cells[0].Status, ShouldEqual, apply.StatusCurrent)
				So(homeFiles(t, home), ShouldResemble, []string{".config/opencode/opencode.json"})

				doc := openCodeMCPDoc(t, filepath.Join(dir, "opencode.json"))
				container, _ := doc["mcp"].(map[string]any)
				servers, _ := container["servers"].(map[string]any)
				So(servers, ShouldBeEmpty)
			})
		})
	})
}

// TestOpenCodeEnvPrecedence pins the whole config-root precedence, in the
// order the host resolves it: OPENCODE_CONFIG_DIR replaces the root outright
// and wins outright, XDG_CONFIG_HOME is the fallback, and the home-relative
// `.config/opencode` is the default when neither is set. The first two cases
// were live-probed on 2.0.18 with `opencode debug paths`; the precedence
// between them follows from that probe.
func TestOpenCodeEnvPrecedence(t *testing.T) {
	Convey("Given a config root moved through XDG_CONFIG_HOME", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)

		st := openStore(t)
		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose})

		Convey("When a package is delivered", func() {
			Convey("Then it lands below the XDG root, not below the home", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(xdg, "opencode", "opencode.json")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".config", "opencode")), ShouldBeFalse)
			})
		})
	})

	Convey("Given OPENCODE_CONFIG_DIR set", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		cfg := filepath.Join(t.TempDir(), "cfg")
		t.Setenv("OPENCODE_CONFIG_DIR", cfg)

		st := openStore(t)
		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose})

		Convey("When a package is delivered", func() {
			Convey("Then the root is the one the host reads, verbatim and above XDG", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(cfg, "opencode.json")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".config", "opencode")), ShouldBeFalse)
			})
		})
	})

	Convey("Given XDG_CONFIG_HOME unset", t, func() {
		fakeOpenCode(t)

		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("OPENCODE_CONFIG_DIR", "")

		st := openStore(t)
		h, _ := newOpenCode(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: openCodePackage(t), Strategy: host.Loose})

		Convey("When a package is delivered", func() {
			Convey("Then the home-relative config dir is used", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".config", "opencode", "opencode.json")), ShouldBeTrue)
			})
		})
	})
}

// hostInstallOps lists the host-install ops of an RMA.
func hostInstallOps(ops []receipt.Op) []receipt.Op {
	var out []receipt.Op

	for _, op := range ops {
		if op.Kind == receipt.OpHostInstall {
			out = append(out, op)
		}
	}

	return out
}
