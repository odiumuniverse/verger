package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// claudeRoot returns a temp Claude plugin root with the given manifest name.
func claudeRoot(t *testing.T, name string) string {
	t.Helper()

	root := t.TempDir()
	writeAt(t, root, ".claude-plugin/plugin.json", `{"name": "`+name+`"}`)

	return root
}

func TestSkillDiscovery(t *testing.T) {
	Convey("Given a skills directory with mixed entries", t, func() {
		root := claudeRoot(t, "discover")

		writeAt(t, root, "skills/valid/SKILL.md", "# valid\n")
		writeAt(t, root, "skills/nested/inner/SKILL.md", "# nested\n")
		writeAt(t, root, "skills/no-root/README.md", "# no root\n")
		writeAt(t, root, "skills/readme.md", "# a file\n")
		writeAt(t, root, "skills/empty/notes.txt", "notes\n")
		writeAt(t, root, "skills/skills/SKILL.md", "# nested name\n")

		if err := os.Symlink(filepath.Join(root, "skills", "valid"), filepath.Join(root, "skills", "linked")); err != nil {
			t.Fatalf("symlink skill dir: %v", err)
		}

		outside := t.TempDir()
		writeAt(t, outside, "SKILL.md", "# outside\n")

		if err := os.MkdirAll(filepath.Join(root, "skills", "linked-root"), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(root, "skills", "linked-root", "SKILL.md")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then only immediate regular directories with a regular SKILL.md are skills", func() {
				So(err, ShouldBeNil)

				names := []string{}
				for _, component := range pkg.Components {
					names = append(names, component.Path)
				}

				So(names, ShouldResemble, []string{"skills/skills", "skills/valid"})
				So(pkg.Warnings, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a plugin without a skills directory", t, func() {
		root := claudeRoot(t, "empty-payload")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then no components and no warnings are produced", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldBeEmpty)
				So(pkg.Warnings, ShouldBeEmpty)
			})
		})
	})
}

func TestSkillJunkSkipped(t *testing.T) {
	Convey("Given two copies of a skill, one carrying junk", t, func() {
		clean := copyFixture(t, "claude/acme")
		noisy := copyFixture(t, "claude/acme")

		writeAt(t, noisy, "skills/alpha/.DS_Store", "junk\n")
		writeAt(t, noisy, "skills/alpha/Thumbs.db", "junk\n")
		writeAt(t, noisy, "skills/alpha/node_modules/dep/index.js", "junk\n")
		writeAt(t, noisy, "skills/alpha/__pycache__/mod.pyc", "junk\n")
		writeAt(t, noisy, "skills/alpha/.git/config", "junk\n")

		Convey("When both are parsed", func() {
			cleanPkg, err := manifest.Parse(clean, manifest.FormatClaude)
			So(err, ShouldBeNil)

			noisyPkg, err := manifest.Parse(noisy, manifest.FormatClaude)

			Convey("Then the junk never changes the skill digest", func() {
				So(err, ShouldBeNil)
				So(noisyPkg.Components, ShouldResemble, cleanPkg.Components)
			})
		})
	})
}

func TestSkillDigestChanges(t *testing.T) {
	Convey("Given a skill whose content changes", t, func() {
		root := copyFixture(t, "claude/acme")

		before, err := manifest.Parse(fixture(t, "claude/acme"), manifest.FormatClaude)
		So(err, ShouldBeNil)

		writeAt(t, root, "skills/beta/SKILL.md", "# beta changed\n")

		Convey("When it is parsed again", func() {
			after, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then only the changed skill digest moved", func() {
				So(err, ShouldBeNil)
				So(after.Components, ShouldHaveLength, len(before.Components))

				for i, component := range after.Components {
					if component.Path == "skills/beta" {
						So(component.Digest, ShouldNotEqual, before.Components[i].Digest)
					} else {
						So(component.Digest, ShouldEqual, before.Components[i].Digest)
					}
				}
			})
		})
	})
}

func TestSkillSymlinkInsideTree(t *testing.T) {
	Convey("Given a skill tree carrying a symlink", t, func() {
		clean := copyFixture(t, "claude/acme")
		linked := copyFixture(t, "claude/acme")

		outside := t.TempDir()
		writeAt(t, outside, "target.sh", "outside content that must not join the digest\n")

		if err := os.Symlink(filepath.Join(outside, "target.sh"), filepath.Join(linked, "skills", "beta", "link.sh")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		Convey("When both are parsed", func() {
			cleanPkg, err := manifest.Parse(clean, manifest.FormatClaude)
			So(err, ShouldBeNil)

			linkedPkg, err := manifest.Parse(linked, manifest.FormatClaude)

			Convey("Then the symlink is skipped by the tree digest", func() {
				So(err, ShouldBeNil)
				So(linkedPkg.Components, ShouldResemble, cleanPkg.Components)
			})
		})
	})
}

func TestFileComponents(t *testing.T) {
	Convey("Given agents and commands with foreign files", t, func() {
		root := claudeRoot(t, "files")

		writeAt(t, root, "agents/keep.md", "keep\n")
		writeAt(t, root, "agents/skip.txt", "skip\n")
		writeAt(t, root, "agents/nested/deep.md", "deep\n")
		writeAt(t, root, "commands/keep.md", "keep\n")
		writeAt(t, root, "commands/legacy.toml", "skip\n")

		outside := t.TempDir()
		writeAt(t, outside, "target.md", "target\n")

		if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "agents", "linked.md")); err != nil {
			t.Fatalf("symlink agent: %v", err)
		}

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then only regular markdown files with stems as names are components", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindAgent, Name: "keep", Path: "agents/keep.md",
						Digest: fileDigest(t, filepath.Join(root, "agents", "keep.md")),
					},
					{
						Kind: manifest.KindCommand, Name: "keep", Path: "commands/keep.md",
						Digest: fileDigest(t, filepath.Join(root, "commands", "keep.md")),
					},
				})
			})
		})
	})

	Convey("Given a Gemini commands directory", t, func() {
		root := t.TempDir()
		writeAt(t, root, "gemini-extension.json", `{"name": "gemini-files"}`)
		writeAt(t, root, "commands/run.toml", "prompt = \"x\"\n")
		writeAt(t, root, "commands/legacy.md", "skip\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatGemini)

			Convey("Then TOML commands are named by stem", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindCommand, Name: "run", Path: "commands/run.toml",
						Digest: fileDigest(t, filepath.Join(root, "commands", "run.toml")),
					},
				})
			})
		})
	})
}

func TestManifestDeclaredSkills(t *testing.T) {
	Convey("Given a Codex manifest declaring a list of skill paths", t, func() {
		root := t.TempDir()
		writeAt(t, root, "plugin.json", `{"name": "declared", "skills": ["one", "two"]}`)
		writeAt(t, root, "one/SKILL.md", "# one\n")
		writeAt(t, root, "two/a/SKILL.md", "# a\n")
		writeAt(t, root, "two/b/SKILL.md", "# b\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatCodex)

			Convey("Then a skill root is one component and a container is scanned", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{
						Kind: manifest.KindSkill, Name: "a", Path: "two/a",
						Digest: treeDigest(t, filepath.Join(root, "two", "a")),
					},
					{
						Kind: manifest.KindSkill, Name: "b", Path: "two/b",
						Digest: treeDigest(t, filepath.Join(root, "two", "b")),
					},
					{
						Kind: manifest.KindSkill, Name: "one", Path: "one",
						Digest: treeDigest(t, filepath.Join(root, "one")),
					},
				})
			})
		})
	})

	Convey("Given a Codex manifest declaring a missing directory", t, func() {
		root := t.TempDir()
		writeAt(t, root, "plugin.json", `{"name": "declared", "skills": "nowhere"}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatCodex)

			Convey("Then nothing is produced and no warning is raised", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldBeEmpty)
				So(pkg.Warnings, ShouldBeEmpty)
			})
		})
	})
}

func TestMCPDocumentInference(t *testing.T) {
	Convey("Given an MCP document with every shape", t, func() {
		root := claudeRoot(t, "mcp-shapes")

		writeAt(t, root, ".mcp.json", `{"mcpServers": {
			"explicit-stdio": {"type": "stdio", "command": "mcp", "args": ["--a", "--b"]},
			"explicit-http": {"type": "streamable-http", "url": "https://a.test/mcp"},
			"explicit-sse": {"type": "sse", "url": "https://b.test/sse"},
			"alias-http": {"type": "http", "url": "https://c.test/mcp"},
			"http-url": {"httpUrl": "https://d.test/mcp"},
			"bare-url": {"url": "https://e.test/mcp"},
			"bare-command": {"command": "plain-mcp"},
			"list-command": {"command": ["list-mcp", "--x"], "args": ["--y"]},
			"headers-env": {"type": "stdio", "command": "env-mcp", "env": {"KEY": "{secret:KEY}"}, "headers": {"X": "{secret:X}"}},
			"unknown-type": {"type": "ws", "url": "https://f.test"},
			"neither": {"name": "nothing"},
			"empty": {}
		}}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then transports are inferred and unusable servers are skipped with warnings", func() {
				So(err, ShouldBeNil)

				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "alias-http", Transport: "streamable-http", URL: "https://c.test/mcp"},
					{Name: "bare-command", Transport: "stdio", Command: []string{"plain-mcp"}},
					{Name: "bare-url", Transport: "streamable-http", URL: "https://e.test/mcp"},
					{Name: "explicit-http", Transport: "streamable-http", URL: "https://a.test/mcp"},
					{Name: "explicit-sse", Transport: "sse", URL: "https://b.test/sse"},
					{Name: "explicit-stdio", Transport: "stdio", Command: []string{"mcp", "--a", "--b"}},
					{
						Name: "headers-env", Transport: "stdio", Command: []string{"env-mcp"},
						Env: map[string]string{"KEY": "{secret:KEY}"}, Headers: map[string]string{"X": "{secret:X}"},
					},
					{Name: "http-url", Transport: "streamable-http", URL: "https://d.test/mcp"},
					{Name: "list-command", Transport: "stdio", Command: []string{"list-mcp", "--x", "--y"}},
				})

				So(pkg.Warnings, ShouldResemble, []string{
					"claude: .mcp.json: mcp server empty has neither command nor url; skipped",
					"claude: .mcp.json: mcp server neither has neither command nor url; skipped",
					`claude: .mcp.json: mcp server unknown-type has unsupported transport "ws"; skipped`,
				})
			})
		})
	})
}

func TestMCPDocumentShapes(t *testing.T) {
	Convey("Given a mcpServers value that is not an object", t, func() {
		root := claudeRoot(t, "mcp-shape")
		writeAt(t, root, ".mcp.json", `{"mcpServers": []}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the document is ignored with a warning", func() {
				So(err, ShouldBeNil)
				So(pkg.MCP, ShouldBeEmpty)
				So(pkg.Warnings, ShouldResemble, []string{
					"claude: .mcp.json: mcpServers is not an object; ignored",
				})
			})
		})
	})

	Convey("Given a top-level value that is not an object", t, func() {
		root := claudeRoot(t, "mcp-shape")
		writeAt(t, root, ".mcp.json", `[]`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the document is ignored with a warning", func() {
				So(err, ShouldBeNil)
				So(pkg.MCP, ShouldBeEmpty)
				So(pkg.Warnings, ShouldResemble, []string{
					"claude: .mcp.json: mcp document is not an object; ignored",
				})
			})
		})
	})

	Convey("Given a document without mcpServers", t, func() {
		root := claudeRoot(t, "mcp-shape")
		writeAt(t, root, ".mcp.json", `{}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then it is a valid empty document", func() {
				So(err, ShouldBeNil)
				So(pkg.MCP, ShouldBeEmpty)
				So(pkg.Warnings, ShouldBeEmpty)
			})
		})
	})
}

func TestMCPFilePreference(t *testing.T) {
	Convey("Given both Codex MCP documents", t, func() {
		root := t.TempDir()
		writeAt(t, root, "plugin.json", `{"name": "codex-pref"}`)
		writeAt(t, root, "mcp.json", `{"mcpServers": {"from-mcp": {"command": "a"}}}`)
		writeAt(t, root, ".mcp.json", `{"mcpServers": {"from-dot-mcp": {"command": "b"}}}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatCodex)

			Convey("Then the portable mcp.json wins", func() {
				So(err, ShouldBeNil)
				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "from-mcp", Transport: "stdio", Command: []string{"a"}},
				})
			})
		})
	})

	Convey("Given only the dot MCP document for Codex", t, func() {
		root := t.TempDir()
		writeAt(t, root, "plugin.json", `{"name": "codex-pref"}`)
		writeAt(t, root, ".mcp.json", `{"mcpServers": {"from-dot-mcp": {"command": "b"}}}`)

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatCodex)

			Convey("Then the dot file is used", func() {
				So(err, ShouldBeNil)
				So(pkg.MCP, ShouldResemble, []manifest.MCPServer{
					{Name: "from-dot-mcp", Transport: "stdio", Command: []string{"b"}},
				})
			})
		})
	})
}

func TestComponentOrdering(t *testing.T) {
	Convey("Given payload entries in unlucky filesystem order", t, func() {
		root := claudeRoot(t, "ordering")

		writeAt(t, root, "skills/zeta/SKILL.md", "# zeta\n")
		writeAt(t, root, "skills/alpha/SKILL.md", "# alpha\n")
		writeAt(t, root, "agents/z.md", "z\n")
		writeAt(t, root, "agents/a.md", "a\n")
		writeAt(t, root, "commands/z.md", "z\n")
		writeAt(t, root, "commands/a.md", "a\n")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then components are sorted by kind, name and path", func() {
				So(err, ShouldBeNil)

				keys := []string{}
				for _, component := range pkg.Components {
					keys = append(keys, string(component.Kind)+"/"+component.Name+"/"+component.Path)
				}

				So(keys, ShouldResemble, []string{
					"agent/a/agents/a.md",
					"agent/z/agents/z.md",
					"command/a/commands/a.md",
					"command/z/commands/z.md",
					"skill/alpha/skills/alpha",
					"skill/zeta/skills/zeta",
				})
			})
		})
	})
}

func TestZeroByteFiles(t *testing.T) {
	Convey("Given zero-byte payload files", t, func() {
		root := claudeRoot(t, "zero")

		writeAt(t, root, "skills/empty/SKILL.md", "")
		writeAt(t, root, "agents/empty.md", "")
		writeAt(t, root, "commands/empty.md", "")

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then presence is enough for every kind", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldResemble, []manifest.Component{
					{Kind: manifest.KindAgent, Name: "empty", Path: "agents/empty.md", Digest: digest.Bytes(nil)},
					{Kind: manifest.KindCommand, Name: "empty", Path: "commands/empty.md", Digest: digest.Bytes(nil)},
					{
						Kind: manifest.KindSkill, Name: "empty", Path: "skills/empty",
						Digest: treeDigest(t, filepath.Join(root, "skills", "empty")),
					},
				})
			})
		})
	})
}

func TestDeepSkillTrees(t *testing.T) {
	Convey("Given a deeply nested skill tree", t, func() {
		root := claudeRoot(t, "deep")

		depth := 50
		rel := "skills/deep/SKILL.md"

		writeAt(t, root, rel, "# deep\n")

		for i := range depth {
			rel = "skills/deep/" + strings.Repeat("level/", i+1) + "file.txt"
			writeAt(t, root, rel, "content\n")
		}

		Convey("When it is parsed", func() {
			pkg, err := manifest.Parse(root, manifest.FormatClaude)

			Convey("Then the whole tree digests", func() {
				So(err, ShouldBeNil)
				So(pkg.Components, ShouldHaveLength, 1)
				So(pkg.Components[0].Digest, ShouldEqual, treeDigest(t, filepath.Join(root, "skills", "deep")))
			})
		})
	})
}
