package pack

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Golden identities of the fixture package.
const (
	fixtureRoot   = "testdata/pkg"
	goldenID      = "JuliusBrussee/caveman"
	goldenName    = "caveman"
	goldenOwner   = "JuliusBrussee"
	goldenVisible = "caveman@JuliusBrussee"
	goldenVersion = "1.2.3"
)

// schema URLs rendered into the chimera.
const (
	pluginSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	mcpSchema    = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// fixtureInput is the golden Input: two skills, one agent, two commands, two
// MCP servers and two hooks, rooted in testdata/pkg.
func fixtureInput(t *testing.T) Input {
	t.Helper()

	return Input{
		ID:          goldenID,
		Name:        goldenName,
		Owner:       goldenOwner,
		Version:     goldenVersion,
		Description: "Caveman toolkit.",
		License:     "MIT",
		Keywords:    []string{"agents", "tools"},
		Format:      manifest.FormatClaude,
		Root:        fixtureRoot,
		Components: []manifest.Component{
			skillComponent(t, "alpha"),
			skillComponent(t, "beta"),
			fileComponent(t, manifest.KindAgent, "reviewer", "agents/reviewer.md"),
			fileComponent(t, manifest.KindCommand, "dev", "commands/dev.md"),
			fileComponent(t, manifest.KindCommand, "empty", "commands/empty.md"),
		},
		MCP: []manifest.MCPServer{
			{
				Name:      "caveman-fs",
				Transport: "stdio",
				Command:   []string{"node", "${PLUGIN_ROOT}/server.js"},
				Env:       map[string]string{"TOKEN": "{secret:FS_TOKEN}"}, //nolint:gosec // G101: a fixture secret reference, not a credential
			},
			{
				Name:      "caveman-web",
				Transport: "sse",
				URL:       "https://example.com/mcp",
				Headers:   map[string]string{"Authorization": "Bearer {secret:WEB_TOKEN}"},
			},
		},
		Hooks: []manifest.Hook{
			{Event: manifest.EventPreTool, Matcher: "Bash", Command: `node "${CLAUDE_PLUGIN_ROOT}/hooks/guard.js"`, Timeout: 5, Origin: manifest.FormatClaude},
			{Event: manifest.EventPostTool, Matcher: "*", Command: "echo done", Origin: manifest.FormatClaude},
		},
	}
}

// skillComponent digits the skill tree below the fixture root.
func skillComponent(t *testing.T, name string) manifest.Component {
	t.Helper()

	rel := "skills/" + name

	sum, err := digest.Tree(filepath.Join(fixtureRoot, rel))
	if err != nil {
		t.Fatalf("digest %s: %v", rel, err)
	}

	return manifest.Component{Kind: manifest.KindSkill, Name: name, Path: rel, Digest: sum}
}

// fileComponent digests one fixture file as a component.
func fileComponent(t *testing.T, kind manifest.Kind, name, rel string) manifest.Component {
	t.Helper()

	sum, err := digest.File(filepath.Join(fixtureRoot, rel))
	if err != nil {
		t.Fatalf("digest %s: %v", rel, err)
	}

	return manifest.Component{Kind: kind, Name: name, Path: rel, Digest: sum}
}

// allFormats is the canonical chimera format order.
func allFormats() []manifest.Format {
	return []manifest.Format{
		manifest.FormatClaude,
		manifest.FormatCodex,
		manifest.FormatGemini,
		manifest.FormatAgentPlugins,
	}
}

// sortedPaths returns the sorted artifact file paths.
func sortedPaths(files map[string][]byte) []string {
	out := make([]string, 0, len(files))

	for rel := range files {
		out = append(out, rel)
	}

	slices.Sort(out)

	return out
}

// materialize writes an artifact into dir with mode 0600.
func materialize(t *testing.T, art Artifact, dir string) {
	t.Helper()

	for rel, data := range art.Files {
		target := filepath.Join(dir, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(target), err)
		}

		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
	}
}

// renderError asserts err is a *RenderError.
func renderError(t *testing.T, err error) *RenderError {
	t.Helper()

	typed, ok := errors.AsType[*RenderError](err)
	if !ok {
		t.Fatalf("want *RenderError, got %T (%v)", err, err)
	}

	return typed
}

// componentNames lists the component names of one kind.
func componentNames(pkg *manifest.Package, kind manifest.Kind) []string {
	var names []string

	for _, component := range pkg.Components {
		if component.Kind == kind {
			names = append(names, component.Name)
		}
	}

	return names
}

// goldenChimeraFiles is the exact file list of the golden package.
func goldenChimeraFiles() []string {
	return []string{
		".claude-plugin/marketplace.json",
		".claude-plugin/plugin.json",
		".codex-plugin/plugin.json",
		".mcp.json",
		"agents/reviewer.md",
		"commands/dev.md",
		"commands/dev.toml",
		"commands/empty.md",
		"gemini-extension.json",
		"hooks/hooks.json",
		"mcp.json",
		"plugin.json",
		"skills/alpha/SKILL.md",
		"skills/alpha/scripts/run.sh",
		"skills/beta/SKILL.md",
	}
}

func TestRenderGoldenChimera(t *testing.T) {
	Convey("Given the golden package", t, func() {
		art, err := Render(fixtureInput(t))

		Convey("When it is rendered", func() {
			Convey("Then the artifact carries exactly the chimera file set", func() {
				So(err, ShouldBeNil)
				So(sortedPaths(art.Files), ShouldResemble, goldenChimeraFiles())
				So(art.Formats, ShouldResemble, allFormats())
			})

			Convey("Then the Agent Plugins manifest is golden", func() {
				So(string(art.Files["plugin.json"]), ShouldEqualJSON, goldenPluginManifest)
			})

			Convey("Then the Claude manifest is golden", func() {
				So(string(art.Files[".claude-plugin/plugin.json"]), ShouldEqualJSON, goldenClaudeManifest)
			})

			Convey("Then the Codex overlay is golden", func() {
				So(string(art.Files[".codex-plugin/plugin.json"]), ShouldEqualJSON, goldenCodexManifest)
			})

			Convey("Then the Gemini manifest is golden", func() {
				So(string(art.Files["gemini-extension.json"]), ShouldEqualJSON, goldenGeminiManifest)
			})

			Convey("Then the Claude marketplace is golden", func() {
				So(string(art.Files[".claude-plugin/marketplace.json"]), ShouldEqualJSON, goldenMarketplace)
			})

			Convey("Then the MCP documents are golden", func() {
				So(string(art.Files[".mcp.json"]), ShouldEqualJSON, goldenClaudeMCP)
				So(string(art.Files["mcp.json"]), ShouldEqualJSON, goldenAgentPluginsMCP)
			})

			Convey("Then the hooks document unions the Claude and Gemini dialects", func() {
				So(string(art.Files["hooks/hooks.json"]), ShouldEqualJSON, goldenHooks)
			})

			Convey("Then components round-trip through the T1.4 renderers", func() {
				assertRenderedComponents(t, art)
			})

			Convey("Then digests follow the invariant", func() {
				in := fixtureInput(t)
				So(art.Digests["skill/alpha"], ShouldEqual, in.Components[0].Digest)
				So(art.Digests["skill/beta"], ShouldEqual, in.Components[1].Digest)
				So(art.Digests["agent/reviewer"], ShouldEqual, digest.Bytes(art.Files["agents/reviewer.md"]))
				So(art.Digests["command/dev"], ShouldEqual, digest.Bytes(art.Files["commands/dev.md"]))
				So(art.Digests["command/empty"], ShouldEqual, digest.Bytes(art.Files["commands/empty.md"]))
			})

			Convey("Then the only warning is the Gemini-inexpressible command", func() {
				So(art.Warnings, ShouldResemble, []string{`gemini: command "empty" cannot express "prompt"; skipped`})
			})
		})
	})
}

// Golden documents of the fixture package. Formatting is not compared: the
// tests match parsed JSON values via ShouldEqualJSON.
const goldenPluginManifest = `{
  "$schema": "` + pluginSchema + `",
  "name": "caveman@JuliusBrussee",
  "version": "1.2.3",
  "description": "Caveman toolkit.",
  "author": {"name": "JuliusBrussee"},
  "license": "MIT",
  "keywords": ["agents", "tools"]
}`

const goldenClaudeManifest = `{
  "name": "caveman@JuliusBrussee",
  "version": "1.2.3",
  "description": "Caveman toolkit.",
  "author": {"name": "JuliusBrussee"},
  "license": "MIT",
  "keywords": ["agents", "tools"],
  "mcpServers": {
    "caveman-fs": {
      "type": "stdio",
      "command": "node",
      "args": ["${PLUGIN_ROOT}/server.js"],
      "env": {"TOKEN": "{secret:FS_TOKEN}"}
    },
    "caveman-web": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer {secret:WEB_TOKEN}"}
    }
  }
}`

const goldenCodexManifest = `{
  "name": "caveman@JuliusBrussee",
  "version": "1.2.3",
  "description": "Caveman toolkit."
}`

const goldenGeminiManifest = `{
  "name": "caveman@JuliusBrussee",
  "version": "1.2.3",
  "description": "Caveman toolkit.",
  "mcpServers": {
    "caveman-fs": {
      "command": "node",
      "args": ["${PLUGIN_ROOT}/server.js"],
      "env": {"TOKEN": "{secret:FS_TOKEN}"}
    },
    "caveman-web": {
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer {secret:WEB_TOKEN}"}
    }
  }
}`

const goldenMarketplace = `{
  "name": "caveman@JuliusBrussee",
  "owner": {"name": "JuliusBrussee"},
  "description": "Caveman toolkit.",
  "plugins": [{"name": "caveman", "source": "./"}]
}`

const goldenClaudeMCP = `{
  "mcpServers": {
    "caveman-fs": {
      "type": "stdio",
      "command": "node",
      "args": ["${PLUGIN_ROOT}/server.js"],
      "env": {"TOKEN": "{secret:FS_TOKEN}"}
    },
    "caveman-web": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer {secret:WEB_TOKEN}"}
    }
  }
}`

const goldenAgentPluginsMCP = `{
  "$schema": "` + mcpSchema + `",
  "mcpServers": {
    "caveman-fs": {
      "type": "stdio",
      "command": "node",
      "args": ["${PLUGIN_ROOT}/server.js"],
      "env": {"TOKEN": "{secret:FS_TOKEN}"}
    },
    "caveman-web": {
      "type": "sse",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer {secret:WEB_TOKEN}"}
    }
  }
}`

const goldenHooks = `{
  "hooks": {
    "AfterTool": [{"hooks": [{"type": "command", "command": "echo done"}]}],
    "BeforeTool": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"${CLAUDE_PLUGIN_ROOT}/hooks/guard.js\"", "timeout": 5}]}],
    "PostToolUse": [{"hooks": [{"type": "command", "command": "echo done"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"${CLAUDE_PLUGIN_ROOT}/hooks/guard.js\"", "timeout": 5}]}]
  }
}`

const goldenHooksWithStop = `{
  "hooks": {
    "AfterTool": [{"hooks": [{"type": "command", "command": "echo done"}]}],
    "BeforeTool": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"${CLAUDE_PLUGIN_ROOT}/hooks/guard.js\"", "timeout": 5}]}],
    "PostToolUse": [{"hooks": [{"type": "command", "command": "echo done"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"${CLAUDE_PLUGIN_ROOT}/hooks/guard.js\"", "timeout": 5}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "echo bye"}]}]
  }
}`

// assertRenderedComponents checks that agent/command bytes come from the
// dialect renderers rather than the raw source files.
func assertRenderedComponents(t *testing.T, art Artifact) {
	t.Helper()

	agentSrc, err := os.ReadFile(filepath.Join(fixtureRoot, "agents/reviewer.md"))
	if err != nil {
		t.Fatalf("read agent: %v", err)
	}

	agent, err := render.ParseAgentMarkdown(agentSrc)
	if err != nil {
		t.Fatalf("parse agent: %v", err)
	}

	if got := string(art.Files["agents/reviewer.md"]); got != string(agent.ClaudeMarkdown()) {
		t.Errorf("agent bytes are not the ClaudeMarkdown render")
	}

	devSrc, err := os.ReadFile(filepath.Join(fixtureRoot, "commands/dev.md"))
	if err != nil {
		t.Fatalf("read command: %v", err)
	}

	dev, err := render.ParseCommandMarkdown(devSrc)
	if err != nil {
		t.Fatalf("parse command: %v", err)
	}

	if got := string(art.Files["commands/dev.md"]); got != string(dev.Markdown()) {
		t.Errorf("command markdown is not the canonical render")
	}

	toml, err := dev.GeminiTOML()
	if err != nil {
		t.Fatalf("render gemini command: %v", err)
	}

	if got := string(art.Files["commands/dev.toml"]); got != string(toml) {
		t.Errorf("command toml is not the Gemini render")
	}
}

func TestRenderManifestsParseBack(t *testing.T) {
	Convey("Given the golden chimera materialized on disk", t, func() {
		art, err := Render(fixtureInput(t))
		So(err, ShouldBeNil)

		dir := t.TempDir()
		materialize(t, art, dir)

		Convey("When the formats are detected", func() {
			Convey("Then all four are found in canonical order", func() {
				So(manifest.Detect(dir), ShouldResemble, allFormats())
			})
		})

		Convey("When Claude parses the chimera", func() {
			pkg, parseErr := manifest.Parse(dir, manifest.FormatClaude)

			Convey("Then identity, components, MCP and canonical hooks come back", func() {
				So(parseErr, ShouldBeNil)
				So(pkg.Name, ShouldEqual, goldenVisible)
				So(pkg.Version, ShouldEqual, goldenVersion)
				So(componentNames(pkg, manifest.KindSkill), ShouldResemble, []string{"alpha", "beta"})
				So(componentNames(pkg, manifest.KindAgent), ShouldResemble, []string{"reviewer"})
				So(componentNames(pkg, manifest.KindCommand), ShouldResemble, []string{"dev", "empty"})
				So(pkg.MCP, ShouldHaveLength, 2)
				So(hookEvents(pkg), ShouldResemble, []string{"post-tool", "pre-tool"})
			})
		})

		Convey("When Codex parses the chimera", func() {
			pkg, parseErr := manifest.Parse(dir, manifest.FormatCodex)

			Convey("Then it sees the same payload", func() {
				So(parseErr, ShouldBeNil)
				So(pkg.Name, ShouldEqual, goldenVisible)
				So(componentNames(pkg, manifest.KindSkill), ShouldResemble, []string{"alpha", "beta"})
				So(componentNames(pkg, manifest.KindCommand), ShouldResemble, []string{"dev", "empty"})
				So(pkg.MCP, ShouldHaveLength, 2)
				So(hookEvents(pkg), ShouldResemble, []string{"post-tool", "pre-tool"})
			})
		})

		Convey("When Gemini parses the chimera", func() {
			pkg, parseErr := manifest.Parse(dir, manifest.FormatGemini)

			Convey("Then it sees the Gemini command render and its hooks", func() {
				So(parseErr, ShouldBeNil)
				So(pkg.Name, ShouldEqual, goldenVisible)
				So(componentNames(pkg, manifest.KindSkill), ShouldResemble, []string{"alpha", "beta"})
				So(componentNames(pkg, manifest.KindCommand), ShouldResemble, []string{"dev"})
				So(pkg.MCP, ShouldHaveLength, 2)
				So(hookEvents(pkg), ShouldResemble, []string{"post-tool", "pre-tool"})
			})
		})

		Convey("When Agent Plugins parses the chimera", func() {
			pkg, parseErr := manifest.Parse(dir, manifest.FormatAgentPlugins)

			Convey("Then the closed manifest gates the payload", func() {
				So(parseErr, ShouldBeNil)
				So(pkg.Name, ShouldEqual, goldenVisible)
				So(componentNames(pkg, manifest.KindSkill), ShouldResemble, []string{"alpha", "beta"})
				So(pkg.MCP, ShouldHaveLength, 2)
			})
		})
	})
}

// hookEvents lists the canonical hook events of a package, sorted.
func hookEvents(pkg *manifest.Package) []string {
	out := make([]string, 0, len(pkg.Hooks))

	for _, hook := range pkg.Hooks {
		out = append(out, hook.Event)
	}

	slices.Sort(out)

	return out
}

func TestRenderNameInvisibility(t *testing.T) {
	Convey("Given the golden chimera", t, func() {
		art, err := Render(fixtureInput(t))
		So(err, ShouldBeNil)

		Convey("When every file is searched", func() {
			Convey("Then no file mentions verger", func() {
				for rel, data := range art.Files {
					if bytes.Contains(data, []byte("verger")) {
						t.Errorf("%s mentions verger", rel)
					}
				}
			})

			Convey("Then the author name is visible in every manifest and the marketplace", func() {
				for _, rel := range []string{
					"plugin.json",
					".claude-plugin/plugin.json",
					".codex-plugin/plugin.json",
					"gemini-extension.json",
					".claude-plugin/marketplace.json",
				} {
					if !bytes.Contains(art.Files[rel], []byte(goldenVisible)) {
						t.Errorf("%s does not carry %s", rel, goldenVisible)
					}
				}
			})
		})
	})
}

func TestRenderDeterminism(t *testing.T) {
	Convey("Given one input", t, func() {
		in := fixtureInput(t)

		Convey("When it is rendered twice", func() {
			first, firstErr := Render(in)
			second, secondErr := Render(in)

			Convey("Then the artifacts are byte-identical", func() {
				So(firstErr, ShouldBeNil)
				So(secondErr, ShouldBeNil)
				So(sortedPaths(first.Files), ShouldResemble, sortedPaths(second.Files))

				for rel, data := range first.Files {
					So(second.Files[rel], ShouldResemble, data)
				}

				So(second.Digests, ShouldResemble, first.Digests)
				So(second.Warnings, ShouldResemble, first.Warnings)
				So(second.Formats, ShouldResemble, first.Formats)
			})
		})
	})
}

func TestRenderDialectSkips(t *testing.T) {
	Convey("Given a package with a rule, a Codex TOML agent and an LSP component", t, func() {
		in := fixtureInput(t)
		in.Components = append(in.Components,
			fileComponent(t, manifest.KindRule, "caveman", "rules/caveman.md"),
			manifest.Component{Kind: manifest.KindAgent, Name: "codexer", Path: "agents/codexer.toml"},
			manifest.Component{Kind: manifest.KindLSP, Name: "gopls", Path: "lsp/gopls.json"},
		)

		art, err := Render(in)

		Convey("When it is rendered", func() {
			Convey("Then the rule becomes a skill", func() {
				So(err, ShouldBeNil)

				skill, ok := art.Files["skills/rule-caveman/SKILL.md"]
				So(ok, ShouldBeTrue)
				So(string(skill), ShouldContainSubstring, "name: rule-caveman")
				So(string(skill), ShouldContainSubstring, "Speak like caveman.")
				So(art.Digests["rule/caveman"], ShouldEqual, digest.Bytes(skill))
			})

			Convey("Then every inexpressible component is skipped with a warning", func() {
				So(art.Files, ShouldNotContainKey, "rules/caveman.md")
				So(art.Files, ShouldNotContainKey, "agents/codexer.md")
				So(art.Files, ShouldNotContainKey, "agents/codexer.toml")
				So(art.Warnings, ShouldContain, `codex: agent "codexer": source "agents/codexer.toml" is not markdown; skipped`)
				So(art.Warnings, ShouldContain, `lsp: component "gopls" is not expressible in the chimera; skipped`)
			})

			Convey("Then an empty command renders markdown only", func() {
				So(art.Files, ShouldContainKey, "commands/empty.md")
				So(art.Files, ShouldNotContainKey, "commands/empty.toml")
				So(art.Warnings, ShouldContain, `gemini: command "empty" cannot express "prompt"; skipped`)
			})
		})
	})
}

func TestRenderRuleError(t *testing.T) {
	Convey("Given a rule component with an empty document", t, func() {
		in := fixtureInput(t)
		in.Components = append(in.Components,
			manifest.Component{Kind: manifest.KindRule, Name: "blank", Path: "rules/blank.md"},
		)

		Convey("When it is rendered", func() {
			_, err := Render(in)

			Convey("Then the broken payload is a *RenderError", func() {
				typed := renderError(t, err)
				So(typed.Component, ShouldEqual, "blank")
			})
		})
	})
}

func TestRenderTraversalRefused(t *testing.T) {
	bad := []struct {
		name      string
		component manifest.Component
	}{
		{"escaping path", manifest.Component{Kind: manifest.KindAgent, Name: "evil", Path: "../evil.md"}},
		{"absolute path", manifest.Component{Kind: manifest.KindAgent, Name: "evil", Path: "/tmp/evil.md"}},
		{"escaping skill name", manifest.Component{Kind: manifest.KindSkill, Name: "../evil", Path: "skills/alpha"}},
		{"slashed skill name", manifest.Component{Kind: manifest.KindSkill, Name: "a/b", Path: "skills/alpha"}},
	}

	Convey("Given hostile component paths", t, func() {
		for _, item := range bad {
			Convey("When component "+item.name+" is rendered", func() {
				in := fixtureInput(t)
				in.Components = []manifest.Component{item.component}

				_, err := Render(in)

				Convey("Then rendering refuses with a *RenderError", func() {
					_ = renderError(t, err)
				})
			})
		}
	})

	Convey("Given two components that share one output path", t, func() {
		in := fixtureInput(t)
		in.Components = append(in.Components, skillComponent(t, "alpha"))

		Convey("When the package is rendered", func() {
			_, err := Render(in)

			Convey("Then the duplicate is refused", func() {
				_ = renderError(t, err)
			})
		})
	})
}

func TestRenderHooksDialectUnion(t *testing.T) {
	Convey("Given hooks including a Gemini-unmappable stop event", t, func() {
		in := fixtureInput(t)
		in.Hooks = append(in.Hooks, manifest.Hook{
			Event: manifest.EventStop, Command: "echo bye", Origin: manifest.FormatClaude,
		})

		art, err := Render(in)

		Convey("When it is rendered", func() {
			Convey("Then the document keeps the Claude-only event and warns for Gemini", func() {
				So(err, ShouldBeNil)
				So(string(art.Files["hooks/hooks.json"]), ShouldEqualJSON, goldenHooksWithStop)
				So(art.Warnings, ShouldContain, "gemini hooks: hook event stop has no gemini event; skipped")
			})
		})
	})
}

func TestRenderSecretRefsVerbatim(t *testing.T) {
	Convey("Given MCP servers carrying secret references", t, func() {
		art, err := Render(fixtureInput(t))
		So(err, ShouldBeNil)

		Convey("When the documents are rendered", func() {
			Convey("Then the refs stay verbatim in every dialect", func() {
				for rel, want := range map[string][]string{
					".mcp.json":                  {"{secret:FS_TOKEN}", "{secret:WEB_TOKEN}"},
					"mcp.json":                   {"{secret:FS_TOKEN}", "{secret:WEB_TOKEN}"},
					"gemini-extension.json":      {"{secret:FS_TOKEN}", "{secret:WEB_TOKEN}"},
					".claude-plugin/plugin.json": {"{secret:FS_TOKEN}", "{secret:WEB_TOKEN}"},
				} {
					for _, ref := range want {
						if !bytes.Contains(art.Files[rel], []byte(ref)) {
							t.Errorf("%s lost %s", rel, ref)
						}
					}
				}

				So(bytes.Contains(art.Files[".mcp.json"], []byte("redacted")), ShouldBeFalse)
			})

			Convey("Then an unknown host variable is preserved, not rewritten", func() {
				in := fixtureInput(t)
				in.MCP[0].Command[1] = "${FOO_BAR}/server.js"

				other, renderErr := Render(in)
				So(renderErr, ShouldBeNil)
				So(bytes.Contains(other.Files[".mcp.json"], []byte("${FOO_BAR}/server.js")), ShouldBeTrue)
			})
		})
	})
}

func TestRenderMCPDialectSkips(t *testing.T) {
	Convey("Given MCP servers outside some dialect unions", t, func() {
		in := fixtureInput(t)
		in.MCP = []manifest.MCPServer{
			{Name: "empty-server"},
			{Name: "bad-transport", Transport: "carrier-pigeon", Command: []string{"node"}},
			{Name: "plain-http", Transport: "streamable-http", URL: "http://example.com/mcp"},
			{Name: "escaping", Transport: "stdio", Command: []string{"node", "../../escape.js"}},
		}

		art, err := Render(in)

		Convey("When it is rendered", func() {
			Convey("Then each dialect keeps only what it can express", func() {
				So(err, ShouldBeNil)

				claude, ok := art.Files[".mcp.json"]
				So(ok, ShouldBeTrue)
				So(bytes.Contains(claude, []byte("plain-http")), ShouldBeTrue)
				So(bytes.Contains(claude, []byte("escaping")), ShouldBeTrue)
				So(bytes.Contains(claude, []byte("empty-server")), ShouldBeFalse)
				So(bytes.Contains(claude, []byte("bad-transport")), ShouldBeFalse)

				So(art.Files, ShouldNotContainKey, "mcp.json")
				So(art.Warnings, ShouldNotBeEmpty)
				So(strings.Join(art.Warnings, "\n"), ShouldContainSubstring, "carrier-pigeon")
				So(strings.Join(art.Warnings, "\n"), ShouldContainSubstring, "climbs above the package root")
				So(strings.Join(art.Warnings, "\n"), ShouldContainSubstring, "neither a command nor a url")
			})
		})
	})
}

func TestRenderGeminiCommandLifted(t *testing.T) {
	Convey("Given a Gemini TOML command component", t, func() {
		in := fixtureInput(t)
		in.Components = []manifest.Component{
			fileComponent(t, manifest.KindCommand, "plan", "commands/plan.toml"),
		}

		art, err := Render(in)

		Convey("When it is rendered", func() {
			Convey("Then both dialects are emitted from the lifted command", func() {
				So(err, ShouldBeNil)
				So(art.Files, ShouldContainKey, "commands/plan.md")
				So(art.Files, ShouldContainKey, "commands/plan.toml")
				So(string(art.Files["commands/plan.toml"]), ShouldContainSubstring, "Plan $ARGUMENTS.")
			})
		})
	})
}

func TestRenderEmptyPackage(t *testing.T) {
	Convey("Given a package without components, MCP servers or hooks", t, func() {
		in := Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Description: "Empty.", Root: t.TempDir(),
		}

		art, err := Render(in)

		Convey("When it is rendered", func() {
			Convey("Then only the manifests and the marketplace are emitted", func() {
				So(err, ShouldBeNil)
				So(sortedPaths(art.Files), ShouldResemble, []string{
					".claude-plugin/marketplace.json",
					".claude-plugin/plugin.json",
					".codex-plugin/plugin.json",
					"gemini-extension.json",
					"plugin.json",
				})
				So(art.Warnings, ShouldBeEmpty)
			})

			Convey("Then the manifests still parse back", func() {
				dir := t.TempDir()
				materialize(t, art, dir)
				So(manifest.Detect(dir), ShouldResemble, allFormats())
			})
		})
	})
}

func TestRenderIdentityDerivedFromID(t *testing.T) {
	Convey("Given only the package id", t, func() {
		in := fixtureInput(t)
		in.Name = ""
		in.Owner = ""

		Convey("When the package is rendered", func() {
			art, err := Render(in)

			Convey("Then name and owner come from the id", func() {
				So(err, ShouldBeNil)
				So(bytes.Contains(art.Files["plugin.json"], []byte(goldenVisible)), ShouldBeTrue)
			})
		})
	})

	Convey("Given an anonymous id without an owner", t, func() {
		in := fixtureInput(t)
		in.ID = "local:anon"
		in.Name = "anon"
		in.Owner = ""

		Convey("When the package is rendered", func() {
			_, err := Render(in)

			Convey("Then rendering refuses without an author", func() {
				_ = renderError(t, err)
			})
		})
	})
}

func TestRenderBoundary(t *testing.T) {
	Convey("Given a skill tree with a 0-byte file and unicode names", t, func() {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "skills", "умение", "SKILL.md"), "---\nname: умение\n---\n\nBody.\n")
		writeFile(t, filepath.Join(root, "skills", "умение", "empty.bin"), "")
		writeFile(t, filepath.Join(root, "agents", "агент.md"), "---\nname: агент\n---\n\nAgent body.\n")
		writeFile(t, filepath.Join(root, "commands", "нуль.md"), "")

		in := Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Root: root,
			Components: []manifest.Component{
				treeComponent(t, root, manifest.KindSkill, "умение", "skills/умение"),
				fileComponentAt(t, root, manifest.KindAgent, "агент", "agents/агент.md"),
				fileComponentAt(t, root, manifest.KindCommand, "нуль", "commands/нуль.md"),
			},
		}

		art, err := Render(in)

		Convey("When it is rendered", func() {
			Convey("Then unicode paths and empty files survive", func() {
				So(err, ShouldBeNil)
				So(art.Files, ShouldContainKey, "skills/умение/SKILL.md")
				So(art.Files, ShouldContainKey, "skills/умение/empty.bin")
				So(art.Files["skills/умение/empty.bin"], ShouldEqual, []byte{})
				So(art.Files, ShouldContainKey, "agents/агент.md")
				So(art.Files, ShouldContainKey, "commands/нуль.md")
				So(art.Files, ShouldNotContainKey, "commands/нуль.toml")
			})
		})
	})

	Convey("Given a large skill tree", t, func() {
		root := t.TempDir()
		large := bytes.Repeat([]byte("caveman\n"), 200_000)

		writeFile(t, filepath.Join(root, "skills", "big", "SKILL.md"), "---\nname: big\n---\n\nBig.\n")
		writeFile(t, filepath.Join(root, "skills", "big", "data.bin"), string(large))

		art, err := Render(Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Root:       root,
			Components: []manifest.Component{treeComponent(t, root, manifest.KindSkill, "big", "skills/big")},
		})

		Convey("When it is rendered", func() {
			Convey("Then the payload is copied byte for byte", func() {
				So(err, ShouldBeNil)
				So(art.Files["skills/big/data.bin"], ShouldResemble, large)
			})
		})
	})
}

func TestRenderDoesNotWrite(t *testing.T) {
	Convey("Given a rendered-free fixture tree", t, func() {
		before := snapshotTree(t, fixtureRoot)

		Convey("When Render runs", func() {
			_, err := Render(fixtureInput(t))

			Convey("Then nothing below the root changed", func() {
				So(err, ShouldBeNil)
				So(snapshotTree(t, fixtureRoot), ShouldResemble, before)
			})
		})
	})
}

func TestRenderSymlinkedSkillFile(t *testing.T) {
	Convey("Given a skill tree holding a symlink", t, func() {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "skills", "linked", "SKILL.md"), "---\nname: linked\n---\n\nBody.\n")

		link := filepath.Join(root, "skills", "linked", "escape.md")
		if err := os.Symlink("../../../outside.md", link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		art, err := Render(Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Root:       root,
			Components: []manifest.Component{treeComponent(t, root, manifest.KindSkill, "linked", "skills/linked")},
		})

		Convey("When it is rendered", func() {
			Convey("Then the link is recorded and its target is never read", func() {
				So(err, ShouldBeNil)
				So(art.Files, ShouldNotContainKey, "skills/linked/escape.md")
				So(art.symlinks["skills/linked/escape.md"], ShouldEqual, "../../../outside.md")
			})
		})
	})
}

func TestRenderConcurrent(t *testing.T) {
	Convey("Given one input rendered from many goroutines", t, func() {
		in := fixtureInput(t)

		results := make([]Artifact, 8)
		errs := make([]error, 8)

		var wg sync.WaitGroup

		for i := range results {
			wg.Go(func() {
				results[i], errs[i] = Render(in)
			})
		}

		wg.Wait()

		Convey("When all renders finish", func() {
			Convey("Then every artifact is byte-equal", func() {
				for i := range results {
					So(errs[i], ShouldBeNil)
					So(sortedPaths(results[i].Files), ShouldResemble, sortedPaths(results[0].Files))
				}
			})
		})
	})
}

// writeFile writes a test fixture file, creating parents.
func writeFile(t *testing.T, path, data string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// treeComponent digests a tree below root.
func treeComponent(t *testing.T, root string, kind manifest.Kind, name, rel string) manifest.Component {
	t.Helper()

	sum, err := digest.Tree(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("digest %s: %v", rel, err)
	}

	return manifest.Component{Kind: kind, Name: name, Path: rel, Digest: sum}
}

// fileComponentAt digests one file below root.
func fileComponentAt(t *testing.T, root string, kind manifest.Kind, name, rel string) manifest.Component {
	t.Helper()

	sum, err := digest.File(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("digest %s: %v", rel, err)
	}

	return manifest.Component{Kind: kind, Name: name, Path: rel, Digest: sum}
}

// snapshotTree records every path below root with its kind, mode and content
// digest for a before/after comparison.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		switch {
		case entry.IsDir():
			out[rel] = "dir"
		case entry.Type()&os.ModeSymlink != 0:
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}

			out[rel] = "link:" + target
		default:
			sum, sumErr := digest.File(path)
			if sumErr != nil {
				return sumErr
			}

			mode := entry.Type().Perm()
			out[rel] = "file:" + mode.String() + ":" + sum.String()
		}

		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	return out
}
