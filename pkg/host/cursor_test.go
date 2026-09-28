package host_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/secret"
)

const cursorFixtureRoot = "testdata/cursor/acme"

// fakeCursor puts an executable `cursor-agent` shim at the front of PATH.
func fakeCursor(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "cursor-agent"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newCursor builds the Cursor adapter with a scripted runner.
func newCursor(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewCursor(all...), runner
}

// cursorPackage parses the Cursor fixture and adds the rule component the
// manifest format does not carry.
func cursorPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, cursorFixtureRoot, manifest.FormatClaude)
}

// cursorHome is the Cursor agent home of one test home.
func cursorHome(home string) string {
	return filepath.Join(home, ".cursor")
}

// cursorFixture reads one captured cursor-agent fixture.
func cursorFixture(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "cursor", name)) //nolint:gosec // G304: a fixture below testdata
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return string(data)
}

// cursorSecrets is the secret store of the Cursor fixture: its MCP servers
// reference MCP_TOKEN and WEB_TOKEN.
func cursorSecrets(t *testing.T) *secret.Store {
	t.Helper()

	return secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
}

func TestCursorDetect(t *testing.T) {
	Convey("Given a home without a Cursor agent home", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())

		h, _ := newCursor(t, home, nil)

		Convey("When neither the home nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When ~/.cursor exists", func() {
			if err := os.MkdirAll(cursorHome(home), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the cursor-agent binary on PATH", t, func() {
		fakeCursor(t)

		home := t.TempDir()
		h, _ := newCursor(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

func TestCursorLooseGolden(t *testing.T) {
	Convey("Given a Cursor package and a temp home", t, func() {
		fakeCursor(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := cursorPackage(t)

		h, runner := newCursor(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(cursorSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then skills, the rule wrapper, the agent and the MCP document land; nothing else", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".cursor/agents/reviewer.md",
					".cursor/mcp.json",
					".cursor/skills/alpha/SKILL.md",
					".cursor/skills/alpha/scripts/run.sh",
					".cursor/skills/beta/SKILL.md",
					".cursor/skills/rule-caveman/SKILL.md",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then the surfaces the host does not have are skipped with their reason", func() {
				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, "command")
				So(notes, ShouldContainSubstring, "has no directory on cursor; skipped")
				So(notes, ShouldContainSubstring, "1 hook(s) skipped")
				So(notes, ShouldContainSubstring, "camelCase events in hooks.json")
			})

			Convey("Then the MCP servers land in mcp.json and the approval gate is named", func() {
				So(readTestFile(t, filepath.Join(cursorHome(home), "mcp.json")), ShouldEqualJSON, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "node", "args": ["`+pkg.Root+`/server.js"], "env": {"TOKEN": "s3cr3t-token"}},
    "web": {"type": "http", "url": "https://example.com/mcp", "headers": {"Authorization": "Bearer web-token"}}
  }
}`)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "cursor-agent mcp enable")
			})

			Convey("Then the RMA carries a file op per artifact and no host-install op", func() {
				So(res.RMA, ShouldHaveLength, len(res.Artifacts))

				for _, op := range res.RMA {
					So(op.Kind, ShouldNotEqual, receipt.OpHostInstall)
				}
			})
		})
	})
}

// TestCursorNoInstaller pins the strategy ladder of a host without a package
// manager: native and synth are refused with the reason and no CLI call runs.
func TestCursorNoInstaller(t *testing.T) {
	Convey("Given a package with a marketplace ref", t, func() {
		fakeCursor(t)

		home := t.TempDir()
		pkg := cursorPackage(t)
		pkg.Marketplace = "https://github.com/acme/plugins.git"

		for _, strategy := range []host.Strategy{host.Native, host.Synth} {
			Convey("When "+string(strategy)+" is requested", func() {
				h, runner := newCursor(t, home, nil)

				pkg := pkg
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy})

				Convey("Then it is refused with the missing installer named", func() {
					unsupported, ok := errors.AsType[*host.NotSupportedError](err)
					So(ok, ShouldBeTrue)
					So(unsupported.Operation, ShouldContainSubstring, "no plugin, extension or marketplace command")
					So(runner.Calls(), ShouldBeEmpty)
				})
			})
		}
	})
}

func TestCursorLooseCollisions(t *testing.T) {
	Convey("Given a foreign agent file", t, func() {
		fakeCursor(t)

		home := t.TempDir()
		foreign := filepath.Join(cursorHome(home), "agents", "reviewer.md")
		writeFixtureFile(t, foreign, "someone else's agent\n", 0o600)

		h, _ := newCursor(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(cursorSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: cursorPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the foreign file is untouched and the cell is a collision", func() {
				collision, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(collision.Path, ShouldEqual, foreign)
				So(readTestFile(t, foreign), ShouldEqual, "someone else's agent\n")
			})
		})
	})
}

// TestCursorOracleList pins the real `cursor-agent mcp list` shape captured for
// 2026.06.15: one `<identifier>: <status>` line per server, with `disabled` the
// only state that makes an entry not enabled.
func TestCursorOracleList(t *testing.T) {
	Convey("Given the captured cursor-agent MCP listing", t, func() {
		fakeCursor(t)

		h, runner := newCursor(t, t.TempDir(), map[string]hostcli.Response{
			"cursor-agent mcp list": response(cursorFixture(t, "mcp-list-2026.06.15.txt")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then every configured server is reported with its state", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"cursor-agent mcp list"})
				So(listed, ShouldHaveLength, 2)

				byName := map[string]host.Installed{}
				for _, entry := range listed {
					byName[entry.Name] = entry
				}

				So(byName["probe"].Enabled, ShouldBeFalse)
				So(byName["second"].Enabled, ShouldBeTrue)
			})
		})
	})
}

// TestCursorOracleEmpty pins the captured empty listing: cursor-agent answers a
// sentence, not a table, and no server may be invented from it.
func TestCursorOracleEmpty(t *testing.T) {
	Convey("Given a host with no configured MCP server", t, func() {
		fakeCursor(t)

		h, _ := newCursor(t, t.TempDir(), map[string]hostcli.Response{
			"cursor-agent mcp list": response(cursorFixture(t, "mcp-list-empty-2026.06.15.txt")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the list is empty, not an invented server", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldBeEmpty)
			})
		})
	})
}

// TestCursorOracleValidate pins the unsupported verdict (no package-level
// validation command exists in cursor-agent 2026.06.15).
func TestCursorOracleValidate(t *testing.T) {
	Convey("Given the Cursor oracle", t, func() {
		fakeCursor(t)

		h, runner := newCursor(t, t.TempDir(), nil)

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

// TestCursorUninstall pins the removal of a file-only receipt: cursor records
// no host-install op, so nothing is run against the host and the recorded file
// ops stay pkg/apply's work.
func TestCursorUninstall(t *testing.T) {
	Convey("Given a Cursor receipt without a host-install op", t, func() {
		fakeCursor(t)

		h, runner := newCursor(t, t.TempDir(), nil)

		r := receipt.Receipt{Strategy: string(host.Loose), RMA: []receipt.Op{
			{Kind: receipt.OpWriteFile, Path: filepath.Join(t.TempDir(), "agent.md")},
		}}

		res, err := h.Uninstall(t.Context(), "", r)

		Convey("When it is uninstalled", func() {
			Convey("Then the host is never called and there is nothing to note", func() {
				So(err, ShouldBeNil)
				So(res.Notes, ShouldBeEmpty)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestCursorDigestOfFixture keeps the fixture honest: its skills are real trees
// with content, not empty directories.
func TestCursorDigestOfFixture(t *testing.T) {
	Convey("Given the Cursor fixture", t, func() {
		sum, err := digest.Tree(filepath.Join(cursorFixtureRoot, "skills", "alpha"))

		Convey("Then its skill tree digests", func() {
			So(err, ShouldBeNil)
			So(sum.Valid(), ShouldBeTrue)
		})
	})
}
