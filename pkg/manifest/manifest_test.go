package manifest_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// fixture returns the absolute path of a testdata tree.
func fixture(t *testing.T, rel string) string {
	t.Helper()

	abs, err := filepath.Abs(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatalf("resolve fixture %s: %v", rel, err)
	}

	return abs
}

// copyFixture copies a testdata tree into a fresh temp dir.
func copyFixture(t *testing.T, rel string) string {
	t.Helper()

	target := filepath.Join(t.TempDir(), "pkg")

	if err := fsutil.CopyTree(t.Context(), fixture(t, rel), target); err != nil {
		t.Fatalf("copy fixture %s: %v", rel, err)
	}

	return target
}

// writeAt writes a file below root, creating parents.
func writeAt(t *testing.T, root, rel, content string) string {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// readAt reads a file below root.
func readAt(t *testing.T, root, rel string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // G304: tests read their own temp trees
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	return string(data)
}

// fileDigest returns the canonical file digest.
func fileDigest(t *testing.T, path string) digest.Hash {
	t.Helper()

	sum, err := digest.File(path)
	if err != nil {
		t.Fatalf("digest %s: %v", path, err)
	}

	return sum
}

// treeDigest returns the canonical tree digest without junk skipping.
func treeDigest(t *testing.T, path string) digest.Hash {
	t.Helper()

	sum, err := digest.TreeWithSkip(path, nil)
	if err != nil {
		t.Fatalf("tree digest %s: %v", path, err)
	}

	return sum
}

// treeSnapshot records every file below root: slash path -> mode + sha256.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}

			out[rel] = "symlink:" + target

			return nil
		}

		if entry.IsDir() {
			out[rel] = "dir"

			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec // G304: tests read their own temp trees
		if err != nil {
			return err
		}

		out[rel] = entry.Type().String() + ":" + digest.Bytes(data).String()

		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	return out
}

// hasWarningPrefix reports whether any warning carries the prefix.
func hasWarningPrefix(warnings []string, prefix string) bool {
	for _, warn := range warnings {
		if strings.HasPrefix(warn, prefix) {
			return true
		}
	}

	return false
}

func TestKindConstants(t *testing.T) {
	Convey("Given the component kind constants", t, func() {
		Convey("Then their values match the shared vocabulary", func() {
			So(string(manifest.KindSkill), ShouldEqual, "skill")
			So(string(manifest.KindMCP), ShouldEqual, "mcp")
			So(string(manifest.KindAgent), ShouldEqual, "agent")
			So(string(manifest.KindCommand), ShouldEqual, "command")
			So(string(manifest.KindHook), ShouldEqual, "hook")
			So(string(manifest.KindRule), ShouldEqual, "rule")
			So(string(manifest.KindLSP), ShouldEqual, "lsp")
			So(string(manifest.KindStyle), ShouldEqual, "style")
			So(string(manifest.KindTheme), ShouldEqual, "theme")
			So(string(manifest.KindNativeJS), ShouldEqual, "native-js")
			So(string(manifest.KindNativeTS), ShouldEqual, "native-ts")
			So(string(manifest.KindNativeOther), ShouldEqual, "native-other")
		})
	})
}

func TestFormatConstants(t *testing.T) {
	Convey("Given the format constants", t, func() {
		Convey("Then their values match the registry vocabulary", func() {
			So(string(manifest.FormatClaude), ShouldEqual, "claude")
			So(string(manifest.FormatCodex), ShouldEqual, "codex")
			So(string(manifest.FormatGemini), ShouldEqual, "gemini")
			So(string(manifest.FormatAgentPlugins), ShouldEqual, "agent-plugins")
		})
	})
}

func TestDetect(t *testing.T) {
	Convey("Given a table of fixture trees", t, func() {
		tests := []struct {
			name string
			rel  string
			want []manifest.Format
		}{
			{"claude", "claude/acme", []manifest.Format{manifest.FormatClaude}},
			{"codex portable", "codex/acme", []manifest.Format{manifest.FormatCodex}},
			{"codex legacy overlay", "codex/legacy", []manifest.Format{manifest.FormatCodex}},
			{"gemini", "gemini/acme", []manifest.Format{manifest.FormatGemini}},
			{"agent plugins is also a codex portable package", "agentplugins/acme", []manifest.Format{manifest.FormatCodex, manifest.FormatAgentPlugins}},
			{"chimera", "chimera/acme", []manifest.Format{manifest.FormatClaude, manifest.FormatCodex, manifest.FormatGemini, manifest.FormatAgentPlugins}},
		}

		for _, tt := range tests {
			Convey("When detecting "+tt.name, func() {
				Convey("Then the detected formats match the fixed order", func() {
					So(manifest.Detect(fixture(t, tt.rel)), ShouldResemble, tt.want)
				})
			})
		}
	})

	Convey("Given a directory without manifests", t, func() {
		Convey("When formats are detected", func() {
			Convey("Then nothing is detected", func() {
				So(manifest.Detect(t.TempDir()), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a missing root", t, func() {
		Convey("When formats are detected", func() {
			Convey("Then nothing is detected", func() {
				So(manifest.Detect(filepath.Join(t.TempDir(), "nope")), ShouldBeEmpty)
			})
		})
	})
}

func TestParseClaude(t *testing.T) {
	Convey("Given the Claude plugin fixture", t, func() {
		root := fixture(t, "claude/acme")

		pkg, err := manifest.Parse(root, manifest.FormatClaude)

		Convey("When it is parsed", func() {
			Convey("Then identity and id are normalized", func() {
				So(err, ShouldBeNil)
				So(pkg.ID, ShouldEqual, "local:acme-toolkit")
				So(pkg.Name, ShouldEqual, "acme-toolkit")
				So(pkg.Version, ShouldEqual, "1.2.3")
				So(pkg.Description, ShouldEqual, "Acme toolkit")
				So(pkg.Format, ShouldEqual, manifest.FormatClaude)
				So(pkg.Root, ShouldEqual, root)
			})

			Convey("Then components are sorted with recomputed digests", func() {
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindAgent, Name: "reviewer", Path: "agents/reviewer.md",
						Digest: fileDigest(t, filepath.Join(root, "agents", "reviewer.md")),
					},
					{
						Kind: manifest.KindCommand, Name: "dev", Path: "commands/dev.md",
						Digest: fileDigest(t, filepath.Join(root, "commands", "dev.md")),
					},
					{
						Kind: manifest.KindSkill, Name: "alpha", Path: "skills/alpha",
						Digest: treeDigest(t, filepath.Join(root, "skills", "alpha")),
					},
					{
						Kind: manifest.KindSkill, Name: "beta", Path: "skills/beta",
						Digest: treeDigest(t, filepath.Join(root, "skills", "beta")),
					},
				})
			})

			Convey("Then the file hooks win over the inline manifest hooks", func() {
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventPreTool, Matcher: "Bash", Command: `node "${CLAUDE_PLUGIN_ROOT}/hook.js"`, Timeout: 30, Origin: manifest.FormatClaude},
				})
			})

			Convey("Then the MCP document wins per name and inline servers merge in", func() {
				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{
						Name: "fetch", Transport: "stdio", Command: []string{"npx", "-y", "@acme/fetch-mcp"},
						Env: map[string]string{"ACME_API_KEY": "{secret:ACME_API_KEY}"},
					},
					{Name: "inline", Transport: "stdio", Command: []string{"inline-mcp"}},
					{Name: "search", Transport: "streamable-http", URL: "https://search.acme.test/mcp"},
				})
			})

			Convey("Then the precedence choices are reported as warnings", func() {
				So(pkg.Warnings, ShouldResemble, []string{
					"claude: hooks/hooks.json: both hooks/hooks.json and inline manifest hooks are present; using the file",
					"claude: .mcp.json: mcp server fetch is declared inline and in .mcp.json; using the file",
				})
			})
		})
	})
}

func TestParseCodex(t *testing.T) {
	Convey("Given the portable Codex fixture with a string skills field", t, func() {
		root := fixture(t, "codex/acme")

		pkg, err := manifest.Parse(root, manifest.FormatCodex)

		Convey("When it is parsed", func() {
			Convey("Then identity comes from the root plugin.json", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "codex-acme")
				So(pkg.Version, ShouldEqual, "0.9.0")
				So(pkg.Description, ShouldEqual, "Codex acme")
				So(pkg.Format, ShouldEqual, manifest.FormatCodex)
			})

			Convey("Then the declared skills subdirectory is scanned", func() {
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindAgent, Name: "codex-agent", Path: "agents/codex-agent.md",
						Digest: fileDigest(t, filepath.Join(root, "agents", "codex-agent.md")),
					},
					{
						Kind: manifest.KindCommand, Name: "codex-cmd", Path: "commands/codex-cmd.md",
						Digest: fileDigest(t, filepath.Join(root, "commands", "codex-cmd.md")),
					},
					{
						Kind: manifest.KindSkill, Name: "codex-skill", Path: "skills/codex-skill",
						Digest: treeDigest(t, filepath.Join(root, "skills", "codex-skill")),
					},
				})
			})

			Convey("Then hooks and MCP use the Codex dialects", func() {
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventPostTool, Matcher: "", Command: "codex-hook.sh", Timeout: 0, Origin: manifest.FormatCodex},
				})
				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "codex-server", Transport: "stdio", Command: []string{"codex-mcp", "--serve"}},
				})
				So(pkg.Warnings, ShouldBeEmpty)
			})
		})
	})

	Convey("Given the legacy Codex overlay fixture", t, func() {
		root := fixture(t, "codex/legacy")

		pkg, err := manifest.Parse(root, manifest.FormatCodex)

		Convey("When it is parsed", func() {
			Convey("Then identity and hooks come from .codex-plugin/plugin.json", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "legacy-acme")
				So(pkg.Version, ShouldEqual, "0.1.0")
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.Components[0].Path, ShouldEqual, "skills/legacy-skill")
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventSessionStart, Matcher: "", Command: "legacy-hook.sh", Timeout: 0, Origin: manifest.FormatCodex},
				})
			})
		})
	})
}

func TestParseGemini(t *testing.T) {
	Convey("Given the Gemini extension fixture", t, func() {
		root := fixture(t, "gemini/acme")

		pkg, err := manifest.Parse(root, manifest.FormatGemini)

		Convey("When it is parsed", func() {
			Convey("Then identity comes from gemini-extension.json", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "gemini-acme")
				So(pkg.Version, ShouldEqual, "2.0.0")
				So(pkg.Description, ShouldEqual, "Gemini acme")
				So(pkg.Format, ShouldEqual, manifest.FormatGemini)
			})

			Convey("Then TOML commands are named by stem and skills by directory", func() {
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindAgent, Name: "gemini-agent", Path: "agents/gemini-agent.md",
						Digest: fileDigest(t, filepath.Join(root, "agents", "gemini-agent.md")),
					},
					{
						Kind: manifest.KindCommand, Name: "run", Path: "commands/run.toml",
						Digest: fileDigest(t, filepath.Join(root, "commands", "run.toml")),
					},
					{
						Kind: manifest.KindSkill, Name: "gemini-skill", Path: "skills/gemini-skill",
						Digest: treeDigest(t, filepath.Join(root, "skills", "gemini-skill")),
					},
				})
			})

			Convey("Then the bare hook map is canonical and Stop is skipped with a warning", func() {
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventNotification, Matcher: "", Command: "note.sh", Timeout: 0, Origin: manifest.FormatGemini},
					{Event: manifest.EventPostTool, Matcher: "", Command: "after.sh", Timeout: 0, Origin: manifest.FormatGemini},
					{Event: manifest.EventPreTool, Matcher: "write", Command: "gemini-hook.sh", Timeout: 0, Origin: manifest.FormatGemini},
					{Event: manifest.EventSessionStart, Matcher: "", Command: "start.sh", Timeout: 0, Origin: manifest.FormatGemini},
				})
				So(pkg.Warnings, ShouldResemble, []string{
					"gemini: hooks/hooks.json: hook event Stop has no canonical mapping; skipped",
				})
			})

			Convey("Then inline mcpServers are synthesized into servers", func() {
				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{
						Name: "gemini-server", Transport: "streamable-http", URL: "https://gemini.acme.test/mcp",
						Headers: map[string]string{"X-Token": "{secret:GEMINI_TOKEN}"},
					},
				})
			})
		})
	})
}

func TestParseAgentPlugins(t *testing.T) {
	Convey("Given the Agent Plugins fixture", t, func() {
		root := fixture(t, "agentplugins/acme")

		pkg, err := manifest.Parse(root, manifest.FormatAgentPlugins)

		Convey("When it is parsed", func() {
			Convey("Then identity comes from the closed plugin.json and unknown keys are ignored", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "agent-acme")
				So(pkg.Version, ShouldEqual, "1.0.0")
				So(pkg.Description, ShouldEqual, "Agent acme")
				So(pkg.Format, ShouldEqual, manifest.FormatAgentPlugins)
			})

			Convey("Then skills and the transport union are read", func() {
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindSkill, Name: "agent-skill", Path: "skills/agent-skill",
						Digest: treeDigest(t, filepath.Join(root, "skills", "agent-skill")),
					},
				})

				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "agent-remote", Transport: "sse", URL: "https://agent.acme.test/sse"},
					{Name: "agent-server", Transport: "stdio", Command: []string{"agent-mcp", "--x"}},
				})

				So(pkg.Warnings, ShouldResemble, []string{
					`agent-plugins: mcp.json: mcp server agent-ws has unsupported transport "ws"; skipped`,
				})
			})

			Convey("Then Agent Plugins carry no hooks", func() {
				So(pkg.Hooks, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a plugin.json without the Agent Plugins schema", t, func() {
		root := t.TempDir()
		writeAt(t, root, "plugin.json", `{"name": "not-agent-plugins"}`)

		Convey("When the format is requested", func() {
			_, err := manifest.Parse(root, manifest.FormatAgentPlugins)

			target, ok := errors.AsType[*manifest.ParseError](err)

			Convey("Then the primary manifest is rejected", func() {
				So(ok, ShouldBeTrue)
				So(target.Format, ShouldEqual, manifest.FormatAgentPlugins)
				So(target.Cause, ShouldNotBeNil)
			})
		})
	})
}

func TestParseNameFallback(t *testing.T) {
	Convey("Given a manifest without a name", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"version": "1.0.0"}`)
		writeAt(t, root, "skills/one/SKILL.md", "# one\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the directory base name is used", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, filepath.Base(root))
				So(pkg.ID, ShouldEqual, "local:"+filepath.Base(root))
			})
		})
	})
}

func TestParseIDDefaults(t *testing.T) {
	Convey("Given plugin names in different shapes", t, func() {
		tests := []struct {
			name string
			want string
		}{
			{"owner/name", "owner/name"},
			{"acme-toolkit", "local:acme-toolkit"},
			{"a/b/c", "local:a/b/c"},
			{"/name", "local:/name"},
			{"owner/", "local:owner/"},
			{"владелец/имя", "владелец/имя"},
		}

		for _, tt := range tests {
			Convey("When the name is "+tt.name, func() {
				root := t.TempDir()
				writeAt(t, root, ".claude-plugin/plugin.json", `{"name": `+fmt.Sprintf("%q", tt.name)+`}`)

				pkg, err := manifest.Parse(root, manifest.FormatClaude)

				Convey("Then the default id matches the rule", func() {
					So(err, ShouldBeNil)
					So(pkg.ID, ShouldEqual, tt.want)
				})
			})
		}
	})
}

func TestParseWithID(t *testing.T) {
	Convey("Given a WithID override", t, func() {
		root := fixture(t, "claude/acme")

		Convey("When Parse runs with it", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude, manifest.WithID("acme/toolkit"))

			Convey("Then the id is the override", func() {
				So(err, ShouldBeNil)
				So(pkg.ID, ShouldEqual, "acme/toolkit")
				So(pkg.Name, ShouldEqual, "acme-toolkit")
			})
		})

		Convey("When ParseAny runs with it", func() {
			pkg, err := manifest.ParseAny(fixture(t, "chimera/acme"), manifest.WithID("acme/chimera"))

			Convey("Then the id is the override", func() {
				So(err, ShouldBeNil)
				So(pkg.ID, ShouldEqual, "acme/chimera")
			})
		})
	})
}

func TestParseErrors(t *testing.T) {
	Convey("Given an empty directory for every format", t, func() {
		tests := []struct {
			format manifest.Format
			path   string
		}{
			{manifest.FormatClaude, ".claude-plugin/plugin.json"},
			{manifest.FormatCodex, "plugin.json"},
			{manifest.FormatGemini, "gemini-extension.json"},
			{manifest.FormatAgentPlugins, "plugin.json"},
		}

		for _, tt := range tests {
			Convey("When "+string(tt.format)+" is parsed", func() {
				root := t.TempDir()

				_, err := manifest.Parse(root, tt.format)

				target, ok := errors.AsType[*manifest.ParseError](err)

				Convey("Then the missing primary manifest is a ParseError", func() {
					So(ok, ShouldBeTrue)
					So(target.Format, ShouldEqual, tt.format)
					So(target.Path, ShouldEqual, filepath.Join(root, filepath.FromSlash(tt.path)))
					So(target.Cause, ShouldNotBeNil)
				})
			})
		}
	})

	Convey("Given a malformed primary manifest", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "x",`)

		Convey("When it is parsed", func() {
			_, err := manifest.Parse(root, manifest.FormatClaude)

			target, ok := errors.AsType[*manifest.ParseError](err)

			Convey("Then the JSON syntax error is a ParseError", func() {
				So(ok, ShouldBeTrue)
				So(target.Cause, ShouldNotBeNil)
			})
		})
	})

	Convey("Given a missing root", t, func() {
		Convey("When it is parsed", func() {
			_, err := manifest.Parse(filepath.Join(t.TempDir(), "nope"), manifest.FormatClaude)

			_, ok := errors.AsType[*manifest.ParseError](err)

			Convey("Then the root is rejected", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a root that is a file", t, func() {
		path := writeAt(t, t.TempDir(), "file", "not a directory\n")

		Convey("When it is parsed", func() {
			_, err := manifest.Parse(path, manifest.FormatClaude)

			_, ok := errors.AsType[*manifest.ParseError](err)

			Convey("Then the root is rejected", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given an unknown format", t, func() {
		Convey("When it is parsed", func() {
			_, err := manifest.Parse(t.TempDir(), manifest.Format("cursor"))

			_, ok := errors.AsType[*manifest.ParseError](err)

			Convey("Then the format is rejected", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestParseOptionalFailuresAreWarnings(t *testing.T) {
	Convey("Given a Claude plugin with broken optional payloads", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "broken"}`)
		writeAt(t, root, "hooks/hooks.json", `{"hooks": {`)
		writeAt(t, root, ".mcp.json", `{"mcpServers": `)
		writeAt(t, root, "agents", "agents is a file\n")
		writeAt(t, root, "commands", "commands is a file\n")
		writeAt(t, root, "skills/ok/SKILL.md", "# ok\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the parse succeeds and every failure is a warning", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "broken")
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.Warnings, ShouldHaveLength, 4)

				for _, prefix := range []string{
					"claude: hooks/hooks.json:",
					"claude: .mcp.json:",
					"claude: agents:",
					"claude: commands:",
				} {
					So(hasWarningPrefix(pkg.Warnings, prefix), ShouldBeTrue)
				}
			})
		})
	})
}

func TestParseDeterministic(t *testing.T) {
	Convey("Given a chimera fixture", t, func() {
		Convey("When it is parsed twice with every format", func() {
			roots := map[manifest.Format]string{
				manifest.FormatClaude:       fixture(t, "claude/acme"),
				manifest.FormatCodex:        fixture(t, "codex/acme"),
				manifest.FormatGemini:       fixture(t, "gemini/acme"),
				manifest.FormatAgentPlugins: fixture(t, "agentplugins/acme"),
			}

			Convey("Then both results are deeply equal", func() {
				for format, root := range roots {
					first, err := manifest.Parse(root, format)
					So(err, ShouldBeNil)

					second, err := manifest.Parse(root, format)
					So(err, ShouldBeNil)

					So(second, ShouldResemble, first)
				}
			})
		})

		Convey("When ParseAny runs twice", func() {
			Convey("Then both results are deeply equal", func() {
				first, err := manifest.ParseAny(fixture(t, "chimera/acme"))
				So(err, ShouldBeNil)

				second, err := manifest.ParseAny(fixture(t, "chimera/acme"))
				So(err, ShouldBeNil)

				So(second, ShouldResemble, first)
			})
		})
	})
}

func TestParseDoesNotWrite(t *testing.T) {
	Convey("Given the chimera fixture and a canary home", t, func() {
		root := fixture(t, "chimera/acme")

		canary := filepath.Join(t.TempDir(), "canary-home")
		if err := os.MkdirAll(canary, 0o700); err != nil {
			t.Fatalf("mkdir canary: %v", err)
		}

		t.Setenv("HOME", canary)

		before := treeSnapshot(t, root)

		Convey("When every read-only entry point runs", func() {
			_, err := manifest.Parse(root, manifest.FormatClaude)
			So(err, ShouldBeNil)

			_, err = manifest.Parse(root, manifest.FormatAgentPlugins)
			So(err, ShouldBeNil)

			_, err = manifest.ParseAny(root)
			So(err, ShouldBeNil)

			So(manifest.Detect(root), ShouldHaveLength, 4)

			data := []byte(readAt(t, root, "plugin.json"))

			_, err = manifest.References(data, root, t.TempDir())
			So(err, ShouldBeNil)

			Convey("Then the tree is byte-identical and the canary home stays empty", func() {
				So(treeSnapshot(t, root), ShouldResemble, before)
				So(treeSnapshot(t, canary), ShouldResemble, map[string]string{".": "dir"})
			})
		})
	})
}

func TestParseSymlinksNotFollowed(t *testing.T) {
	Convey("Given a plugin whose payload carries symlinks", t, func() {
		root := copyFixture(t, "claude/acme")

		outside := t.TempDir()
		writeAt(t, outside, "linked-skill/SKILL.md", "# outside\n")
		writeAt(t, outside, "linked-agent.md", "outside agent\n")
		writeAt(t, outside, "plugin.json", `{"name": "outside"}`)
		writeAt(t, outside, "hooks.json", `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "outside.sh"}]}]}}`)

		if err := os.Symlink(filepath.Join(outside, "linked-skill"), filepath.Join(root, "skills", "linked")); err != nil {
			t.Fatalf("symlink skill: %v", err)
		}

		if err := os.Symlink(filepath.Join(outside, "linked-agent.md"), filepath.Join(root, "agents", "linked.md")); err != nil {
			t.Fatalf("symlink agent: %v", err)
		}

		if err := os.Symlink(filepath.Join(outside, "linked-agent.md"), filepath.Join(root, "commands", "linked.md")); err != nil {
			t.Fatalf("symlink command: %v", err)
		}

		if err := os.Symlink(filepath.Join(outside, "linked-agent.md"), filepath.Join(root, "skills", "alpha", "scripts", "link.sh")); err != nil {
			t.Fatalf("symlink inside skill: %v", err)
		}

		if err := os.Remove(filepath.Join(root, "hooks", "hooks.json")); err != nil {
			t.Fatalf("remove hooks file: %v", err)
		}

		if err := os.Symlink(filepath.Join(outside, "hooks.json"), filepath.Join(root, "hooks", "hooks.json")); err != nil {
			t.Fatalf("symlink hooks: %v", err)
		}

		Convey("When the clean and the symlinked copies are parsed", func() {
			clean, err := manifest.Parse(fixture(t, "claude/acme"), manifest.FormatClaude)
			So(err, ShouldBeNil)

			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then symlinked entries are skipped and the skill digest is unchanged", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldResemble, clean.Components)
				So(pkg.MCP, ShouldResemble, clean.MCP)

				// The symlinked hooks file is skipped, so the inline manifest
				// hooks apply and the outside file is never read.
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventSessionStart, Matcher: "", Command: "node inline.js", Timeout: 0, Origin: manifest.FormatClaude},
				})
				So(hasWarningPrefix(pkg.Warnings, "claude: hooks/hooks.json:"), ShouldBeTrue)
			})
		})

		Convey("When a symlinked primary manifest is parsed", func() {
			linkedRoot := t.TempDir()

			if err := os.MkdirAll(filepath.Join(linkedRoot, ".claude-plugin"), 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			if err := os.Symlink(filepath.Join(outside, "plugin.json"), filepath.Join(linkedRoot, ".claude-plugin", "plugin.json")); err != nil {
				t.Fatalf("symlink manifest: %v", err)
			}

			_, err := manifest.Parse(linkedRoot, manifest.FormatClaude)

			Convey("Then the primary manifest is refused, never followed", func() {
				target, ok := errors.AsType[*manifest.ParseError](err)
				So(ok, ShouldBeTrue)
				So(target.Cause, ShouldNotBeNil)
			})
		})
	})
}

func TestParseHugeAndDeepInputs(t *testing.T) {
	Convey("Given an oversized skill and an oversized unknown field", t, func() {
		root := t.TempDir()
		huge := strings.Repeat("A", 1<<20)

		writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "huge", "junk": "`+huge+`"}`)
		writeAt(t, root, "skills/big/SKILL.md", "# big\n"+huge+"\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the payload parses and digests are valid", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.Components[0].Digest.Valid(), ShouldBeTrue)
			})
		})
	})

	Convey("Given a deeply nested JSON document", t, func() {
		depth := 100_001

		deep := strings.Repeat("[", depth) + strings.Repeat("]", depth)

		Convey("When the primary manifest holds it", func() {
			root := t.TempDir()
			writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "deep", "junk": `+deep+`}`)

			_, err := manifest.Parse(root, manifest.FormatClaude)

			_, ok := errors.AsType[*manifest.ParseError](err)

			Convey("Then the unparsable manifest is reported, not a crash", func() {
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When an optional hooks document holds it", func() {
			root := t.TempDir()
			writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "deep"}`)
			writeAt(t, root, "hooks/hooks.json", `{"junk": `+deep+`}`)

			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the failure is a warning", func() {
				So(err, ShouldBeNil)
				So(pkg.Warnings, ShouldHaveLength, 1)
				So(pkg.Warnings[0], ShouldContainSubstring, "claude: hooks/hooks.json:")
			})
		})
	})
}

func TestParseAnyChimera(t *testing.T) {
	Convey("Given a chimera valid as four formats", t, func() {
		root := fixture(t, "chimera/acme")

		pkg, err := manifest.ParseAny(root)

		Convey("When it is parsed", func() {
			Convey("Then identity comes from the first format that declares a name", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "chimera-acme")
				So(pkg.Version, ShouldEqual, "1.0.0")
				So(pkg.Description, ShouldEqual, "Chimera acme")
				So(pkg.Format, ShouldEqual, manifest.FormatClaude)
				So(pkg.ID, ShouldEqual, "local:chimera-acme")
				So(pkg.Root, ShouldEqual, root)
			})

			Convey("Then equal components and MCP servers collapse to one", func() {
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.Components[0].Path, ShouldEqual, "skills/shared")

				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "shared", Transport: "stdio", Command: []string{"shared-mcp"}},
				})
			})

			Convey("Then duplicate hooks collapse by identity", func() {
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventPreTool, Matcher: "Bash", Command: "chimera-hook.sh", Timeout: 0, Origin: manifest.FormatClaude},
				})
			})

			Convey("Then warnings from every format are merged", func() {
				So(pkg.Warnings, ShouldResemble, []string{
					"gemini: hooks/hooks.json: hook event PreToolUse has no canonical mapping; skipped",
				})
			})
		})
	})

	Convey("Given a directory with two nameless manifests and one named", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"version": "1"}`)
		writeAt(t, root, "gemini-extension.json", `{"name": "gemini-named"}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.ParseAny(root)

			Convey("Then the first declaring format provides the identity", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "gemini-named")
				So(pkg.Format, ShouldEqual, manifest.FormatGemini)
			})
		})
	})
}

func TestParseAnyConflicts(t *testing.T) {
	Convey("Given two formats declaring the same MCP server differently", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "conflict"}`)
		writeAt(t, root, "gemini-extension.json", `{"name": "conflict", "mcpServers": {"srv": {"command": "from-gemini"}}}`)
		writeAt(t, root, ".mcp.json", `{"mcpServers": {"srv": {"type": "stdio", "command": "from-claude"}}}`)

		Convey("When it is parsed", func() {
			_, err := manifest.ParseAny(root)

			target, ok := errors.AsType[*manifest.MergeConflictError](err)

			Convey("Then the disagreement is a MergeConflictError", func() {
				So(ok, ShouldBeTrue)
				So(target.Kind, ShouldEqual, manifest.KindMCP)
				So(target.Name, ShouldEqual, "srv")
				So(target.Formats, ShouldResemble, []manifest.Format{manifest.FormatClaude, manifest.FormatGemini})
			})
		})
	})

	Convey("Given two formats declaring the same MCP server identically", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "agree"}`)
		writeAt(t, root, "gemini-extension.json", `{"name": "agree", "mcpServers": {"srv": {"command": "same"}}}`)
		writeAt(t, root, ".mcp.json", `{"mcpServers": {"srv": {"type": "stdio", "command": "same"}}}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.ParseAny(root)

			Convey("Then the server collapses without conflict", func() {
				So(err, ShouldBeNil)
				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "srv", Transport: "stdio", Command: []string{"same"}},
				})
			})
		})
	})

	Convey("Given a MergeConflictError value", t, func() {
		// Component-digest disagreement is structurally unreachable in Ф1:
		// every reader resolves paths below one root, so an identical
		// (Kind, Path) key always carries one digest. The type contract is
		// pinned directly until a reader with a second content root exists.
		target := &manifest.MergeConflictError{
			Kind:    manifest.KindSkill,
			Name:    "shared",
			Formats: []manifest.Format{manifest.FormatClaude, manifest.FormatGemini},
		}

		Convey("When the error renders", func() {
			Convey("Then it names the kind, the name and both formats", func() {
				So(target.Error(), ShouldContainSubstring, "skill")
				So(target.Error(), ShouldContainSubstring, "shared")
				So(target.Error(), ShouldContainSubstring, "claude")
				So(target.Error(), ShouldContainSubstring, "gemini")
			})
		})
	})
}

func TestParseAnyNoFormats(t *testing.T) {
	Convey("Given a directory without manifests", t, func() {
		Convey("When ParseAny runs", func() {
			_, err := manifest.ParseAny(t.TempDir())

			Convey("Then ErrUnknownFormat is reported", func() {
				So(errors.Is(err, manifest.ErrUnknownFormat), ShouldBeTrue)
			})
		})
	})
}

func TestParseTraversalIgnored(t *testing.T) {
	Convey("Given a manifest declaring paths outside the root", t, func() {
		root := t.TempDir()
		writeAt(t, root, "plugin.json", `{"name": "traversal", "skills": ["../outside", "/abs/skill"]}`)
		writeAt(t, t.TempDir(), "outside/SKILL.md", "# outside\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatCodex)

			Convey("Then the escaping paths are ignored with warnings", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldBeEmpty)
				So(pkg.Warnings, ShouldResemble, []string{
					`codex: plugin.json: skill path "../outside" escapes the package root; ignored`,
					`codex: plugin.json: skill path "/abs/skill" escapes the package root; ignored`,
				})
			})
		})
	})
}

func TestParseUnicodePayload(t *testing.T) {
	Convey("Given a package with unicode names and values", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "кит-инструменты", "description": "набор"}`)
		writeAt(t, root, "skills/навык/SKILL.md", "# навык\n")
		writeAt(t, root, "agents/агент.md", "агент\n")
		writeAt(t, root, ".mcp.json", `{"mcpServers": {"сервер": {"type": "stdio", "command": "сервер-mcp"}}}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then unicode names survive normalization", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "кит-инструменты")
				So(pkg.Components, ShouldHaveLength, 2)
				So(pkg.Components[0].Name, ShouldEqual, "агент")
				So(pkg.Components[1].Name, ShouldEqual, "навык")
				So(pkg.MCP[0].Name, ShouldEqual, "сервер")
			})
		})
	})
}

func TestParseBOM(t *testing.T) {
	Convey("Given a manifest and a hooks file with a UTF-8 BOM", t, func() {
		root := t.TempDir()
		writeAt(t, root, ".claude-plugin/plugin.json", "\ufeff{\"name\": \"bom\"}")
		writeAt(t, root, "hooks/hooks.json", "\ufeff{\"hooks\": {\"PreToolUse\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"bom.sh\"}]}]}}")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the BOM is stripped and the document parses", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "bom")
				So(pkg.Hooks, ShouldResemble, []manifest.Hook{
					{Event: manifest.EventPreTool, Matcher: "", Command: "bom.sh", Timeout: 0, Origin: manifest.FormatClaude},
				})
			})
		})
	})
}

func TestSharedDigestsAcrossFormats(t *testing.T) {
	Convey("Given the chimera fixture holding one shared skill", t, func() {
		root := fixture(t, "chimera/acme")

		formats := []manifest.Format{
			manifest.FormatClaude,
			manifest.FormatCodex,
			manifest.FormatGemini,
			manifest.FormatAgentPlugins,
		}

		Convey("When every format parses it", func() {
			digests := map[manifest.Format]digest.Hash{}

			for _, format := range formats {
				pkg, err := manifest.Parse(root, format)
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.Components[0].Path, ShouldEqual, "skills/shared")

				digests[format] = pkg.Components[0].Digest
			}

			Convey("Then a single shared file yields equal component digests", func() {
				for _, format := range formats {
					So(digests[format], ShouldEqual, digests[manifest.FormatClaude])
				}
			})
		})
	})
}

func TestParseAnyAgentPluginsIsPortableCodex(t *testing.T) {
	Convey("Given an Agent Plugins package that is also a portable Codex one", t, func() {
		root := fixture(t, "agentplugins/acme")

		Convey("When it is parsed", func() {
			So(manifest.Detect(root), ShouldResemble, []manifest.Format{manifest.FormatCodex, manifest.FormatAgentPlugins})

			pkg, err := manifest.ParseAny(root)

			Convey("Then the two formats agree and merge", func() {
				So(err, ShouldBeNil)
				So(pkg.Name, ShouldEqual, "agent-acme")
				So(pkg.Format, ShouldEqual, manifest.FormatCodex)
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.MCP, ShouldHaveLength, 2)

				// Each reader reports its own skip of the ws server.
				So(pkg.Warnings, ShouldResemble, []string{
					`codex: mcp.json: mcp server agent-ws has unsupported transport "ws"; skipped`,
					`agent-plugins: mcp.json: mcp server agent-ws has unsupported transport "ws"; skipped`,
				})
			})
		})
	})
}

func TestParseConcurrent(t *testing.T) {
	Convey("Given every format fixture", t, func() {
		fixtures := map[manifest.Format]string{
			manifest.FormatClaude:       fixture(t, "claude/acme"),
			manifest.FormatCodex:        fixture(t, "codex/acme"),
			manifest.FormatGemini:       fixture(t, "gemini/acme"),
			manifest.FormatAgentPlugins: fixture(t, "agentplugins/acme"),
		}
		chimera := fixture(t, "chimera/acme")

		Convey("When they are parsed concurrently", func() {
			const n = 8

			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
			)

			for range n {
				wg.Go(func() {
					for format, root := range fixtures {
						pkg, err := manifest.Parse(root, format)
						if err != nil {
							mu.Lock()
							defer mu.Unlock()

							errs = append(errs, err)

							return
						}

						if len(pkg.Components) == 0 {
							mu.Lock()
							defer mu.Unlock()

							errs = append(errs, fmt.Errorf("%s: no components", format))

							return
						}
					}

					if _, err := manifest.ParseAny(chimera); err != nil {
						mu.Lock()
						defer mu.Unlock()

						errs = append(errs, err)
					}
				})
			}

			wg.Wait()

			Convey("Then every parse succeeds under the race detector", func() {
				So(errs, ShouldBeEmpty)
			})
		})
	})
}

func TestReferences(t *testing.T) {
	const (
		home = "/home/u"
		root = "/home/u/.claude/plugins/mkt/p"
	)

	Convey("Given a table of JSON documents referencing plugin paths", t, func() {
		tests := []struct {
			name string
			data string
			want []manifest.Reference
		}{
			{
				name: "absolute path",
				data: `{"command": "node ` + root + `/1.0/x.cjs run"}`,
				want: []manifest.Reference{{Pointer: "/command", Path: root + "/1.0/x.cjs"}},
			},
			{
				name: "tilde path resolves against home",
				data: `{"command": "cat ~/.claude/plugins/mkt/p/1.0/run.sh"}`,
				want: []manifest.Reference{{Pointer: "/command", Path: root + "/1.0/run.sh"}},
			},
			{
				name: "two refs are reported in pointer order",
				data: `{"b": "` + root + `/b.sh", "a": "` + root + `/a.sh"}`,
				want: []manifest.Reference{
					{Pointer: "/a", Path: root + "/a.sh"},
					{Pointer: "/b", Path: root + "/b.sh"},
				},
			},
			{
				name: "duplicate pointer and path reported once",
				data: `{"command": "` + root + `/x.sh ` + root + `/x.sh"}`,
				want: []manifest.Reference{{Pointer: "/command", Path: root + "/x.sh"}},
			},
			{
				name: "nested pointer",
				data: `{"hooks": {"PreToolUse": [{"hooks": [{"command": "` + root + `/h.sh"}]}]}}`,
				want: []manifest.Reference{{Pointer: "/hooks/PreToolUse/0/hooks/0/command", Path: root + "/h.sh"}},
			},
			{
				name: "pointer escaping",
				data: `{"a/b~c": "` + root + `/x.sh"}`,
				want: []manifest.Reference{{Pointer: "/a~1b~0c", Path: root + "/x.sh"}},
			},
			{
				name: "placeholder tokens are not refs",
				data: `{"command": "node ${CLAUDE_PLUGIN_ROOT}/x.cjs"}`,
			},
			{
				name: "paths outside the root are not refs",
				data: `{"command": "cat /etc/passwd"}`,
			},
			{
				name: "climbing tokens are reported verbatim, not resolved",
				data: `{"command": "` + root + `/../../etc/passwd"}`,
				want: []manifest.Reference{{Pointer: "/command", Path: root + "/../../etc/passwd"}},
			},
		}

		for _, tt := range tests {
			Convey("When scanning "+tt.name, func() {
				refs, err := manifest.References([]byte(tt.data), root, home)

				Convey("Then the references match", func() {
					So(err, ShouldBeNil)
					So(refs, ShouldResemble, tt.want)
				})
			})
		}
	})
}

func TestReferencesEdges(t *testing.T) {
	const (
		home = "/home/u"
		root = "/home/u/.claude/plugins/mkt/p"
	)

	Convey("Given a root outside home", t, func() {
		const otherRoot = "/opt/plug"

		refs, err := manifest.References([]byte(`{"command": "/opt/plug/run.sh"}`), otherRoot, "/home/u")

		Convey("When an absolute root path is scanned", func() {
			Convey("Then it is reported without a tilde form", func() {
				So(err, ShouldBeNil)
				So(refs, ShouldResemble, []manifest.Reference{{Pointer: "/command", Path: "/opt/plug/run.sh"}})
			})
		})
	})

	Convey("Given invalid JSON", t, func() {
		Convey("When references are scanned", func() {
			_, err := manifest.References([]byte("{nope"), root, home)

			Convey("Then the parse error is reported", func() {
				So(err, ShouldBeError)
			})
		})
	})

	Convey("Given an empty root", t, func() {
		Convey("When references are scanned", func() {
			_, err := manifest.References([]byte(`{}`), "", home)

			Convey("Then the empty root is rejected", func() {
				So(err, ShouldBeError)
			})
		})
	})

	Convey("Given a symlink inside the root", t, func() {
		dir := t.TempDir()
		outside := t.TempDir()
		writeAt(t, outside, "target.sh", "outside\n")

		link := filepath.Join(dir, "link.sh")

		if err := os.Symlink(filepath.Join(outside, "target.sh"), link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		Convey("When a document references it", func() {
			refs, err := manifest.References([]byte(`{"command": "`+link+`"}`), dir, t.TempDir())

			Convey("Then the symlink path is reported, never resolved", func() {
				So(err, ShouldBeNil)
				So(refs, ShouldResemble, []manifest.Reference{{Pointer: "/command", Path: link}})
			})
		})
	})
}
