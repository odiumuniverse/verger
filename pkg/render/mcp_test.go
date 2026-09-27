package render_test

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/render"
)

func TestEncodeMCP(t *testing.T) {
	Convey("Given a stdio server", t, func() {
		server := manifest.MCPServer{
			Name:      "acme",
			Transport: "stdio",
			Command:   []string{"npx", "-y", "@acme/mcp"},
			Env:       map[string]string{"ACME_KEY": "value", "TOKEN": "{env:TOKEN}", "SECRET": "{secret:ACME}"}, //nolint:gosec // G101: synthetic fixture values
		}

		Convey("When the Claude value is encoded", func() {
			value, err := render.EncodeMCP(manifest.FormatClaude, server)

			Convey("Then the Claude key set is used with ${NAME} env refs", func() {
				So(err, ShouldBeNil)
				So(value, ShouldResemble, map[string]any{
					"type":    "stdio",
					"command": "npx",
					"args":    []string{"-y", "@acme/mcp"},
					"env":     map[string]string{"ACME_KEY": "value", "TOKEN": "${TOKEN}", "SECRET": "{secret:ACME}"},
				})
			})
		})

		Convey("When the Codex value is encoded", func() {
			value, err := render.EncodeMCP(manifest.FormatCodex, server)

			Convey("Then the Codex key set is used with verbatim refs", func() {
				So(err, ShouldBeNil)
				So(value, ShouldResemble, map[string]any{
					"command": "npx",
					"args":    []string{"-y", "@acme/mcp"},
					"env":     map[string]string{"ACME_KEY": "value", "TOKEN": "{env:TOKEN}", "SECRET": "{secret:ACME}"}, //nolint:gosec // G101: synthetic fixture values
				})
			})
		})

		Convey("When the Gemini value is encoded", func() {
			value, err := render.EncodeMCP(manifest.FormatGemini, server)

			Convey("Then the Gemini key set is used with ${NAME} env refs", func() {
				So(err, ShouldBeNil)
				So(value, ShouldResemble, map[string]any{
					"command": "npx",
					"args":    []string{"-y", "@acme/mcp"},
					"env":     map[string]string{"ACME_KEY": "value", "TOKEN": "${TOKEN}", "SECRET": "{secret:ACME}"},
				})
			})
		})
	})

	Convey("Given remote servers", t, func() {
		headers := map[string]string{"Authorization": "Bearer {env:TOKEN}", "X-Secret": "{secret:KEY}"}

		Convey("When an sse server is encoded", func() {
			server := manifest.MCPServer{Name: "r", Transport: "sse", URL: "https://acme.test/sse", Headers: headers}

			claude, claudeErr := render.EncodeMCP(manifest.FormatClaude, server)
			codex, codexErr := render.EncodeMCP(manifest.FormatCodex, server)
			gemini, geminiErr := render.EncodeMCP(manifest.FormatGemini, server)

			Convey("Then sse maps to http for Claude/Codex and to url for Gemini", func() {
				So(claudeErr, ShouldBeNil)
				So(claude, ShouldResemble, map[string]any{
					"type": "http", "url": "https://acme.test/sse",
					"headers": map[string]string{"Authorization": "Bearer ${TOKEN}", "X-Secret": "{secret:KEY}"},
				})

				So(codexErr, ShouldBeNil)
				So(codex, ShouldResemble, map[string]any{"url": "https://acme.test/sse"})

				So(geminiErr, ShouldBeNil)
				So(gemini, ShouldResemble, map[string]any{
					"url":     "https://acme.test/sse",
					"headers": map[string]string{"Authorization": "Bearer ${TOKEN}", "X-Secret": "{secret:KEY}"},
				})
			})
		})

		Convey("When a streamable-http server is encoded", func() {
			server := manifest.MCPServer{Name: "r", Transport: "streamable-http", URL: "https://acme.test/mcp"}

			claude, claudeErr := render.EncodeMCP(manifest.FormatClaude, server)
			gemini, geminiErr := render.EncodeMCP(manifest.FormatGemini, server)

			Convey("Then Gemini uses httpUrl and Claude type http", func() {
				So(claudeErr, ShouldBeNil)
				So(claude, ShouldResemble, map[string]any{"type": "http", "url": "https://acme.test/mcp"})

				So(geminiErr, ShouldBeNil)
				So(gemini, ShouldResemble, map[string]any{"httpUrl": "https://acme.test/mcp"})
			})
		})
	})
}

func TestEncodeMCPEdge(t *testing.T) {
	Convey("Given host-specific variables in env values", t, func() {
		server := manifest.MCPServer{
			Name:      "vars",
			Transport: "stdio",
			Command:   []string{"node", "${PLUGIN_ROOT}/server.js"},
			Env: map[string]string{
				"ROOT":  "${CLAUDE_PLUGIN_ROOT}/bin",
				"DATA":  "${CLAUDE_PLUGIN_DATA}/cache",
				"PLUGS": "${extensionPath}/x",
				"PLAIN": "${PLUGIN_ROOT}",
			},
		}

		Convey("When every dialect encodes it", func() {
			claude, claudeErr := render.EncodeMCP(manifest.FormatClaude, server)
			codex, codexErr := render.EncodeMCP(manifest.FormatCodex, server)
			gemini, geminiErr := render.EncodeMCP(manifest.FormatGemini, server)

			Convey("Then host variables are never rewritten or refused", func() {
				So(claudeErr, ShouldBeNil)
				So(codexErr, ShouldBeNil)
				So(geminiErr, ShouldBeNil)

				for _, value := range []any{claude, codex, gemini} {
					entry, ok := value.(map[string]any)
					So(ok, ShouldBeTrue)

					env, ok := entry["env"].(map[string]string)
					So(ok, ShouldBeTrue)
					So(env["ROOT"], ShouldEqual, "${CLAUDE_PLUGIN_ROOT}/bin")
					So(env["DATA"], ShouldEqual, "${CLAUDE_PLUGIN_DATA}/cache")
					So(env["PLUGS"], ShouldEqual, "${extensionPath}/x")
					So(env["PLAIN"], ShouldEqual, "${PLUGIN_ROOT}")
				}

				claudeEntry, ok := claude.(map[string]any)
				So(ok, ShouldBeTrue)
				So(claudeEntry["args"], ShouldResemble, []string{"${PLUGIN_ROOT}/server.js"})
			})
		})
	})

	Convey("Given a server without a command or url", t, func() {
		Convey("When it is encoded", func() {
			_, err := render.EncodeMCP(manifest.FormatClaude, manifest.MCPServer{Name: "empty"})

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then the malformed server is reported", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given an unsupported transport", t, func() {
		Convey("When it is encoded", func() {
			_, err := render.EncodeMCP(manifest.FormatCodex, manifest.MCPServer{Name: "ws", Transport: "ws", URL: "ws://x"})

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then it is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given the Agent Plugins format", t, func() {
		Convey("When a server is encoded", func() {
			_, err := render.EncodeMCP(manifest.FormatAgentPlugins, manifest.MCPServer{Name: "a", Command: []string{"x"}})

			Convey("Then the format is refused", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestEncodeMCPTransportContradictions(t *testing.T) {
	Convey("Given an explicit stdio transport with a url", t, func() {
		server := manifest.MCPServer{Name: "mixed", Transport: "stdio", URL: "https://x.test/mcp"}

		Convey("When it is encoded", func() {
			_, err := render.EncodeMCP(manifest.FormatClaude, server)

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then the contradictory transport is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given an explicit remote transport with a command", t, func() {
		cases := []struct {
			name      string
			transport string
		}{
			{"sse", "sse"},
			{"streamable-http", "streamable-http"},
		}

		for _, tc := range cases {
			server := manifest.MCPServer{Name: "mixed", Transport: tc.transport, Command: []string{"node", "x.js"}}

			Convey("When the "+tc.name+" transport is encoded", func() {
				for _, format := range []manifest.Format{manifest.FormatClaude, manifest.FormatCodex, manifest.FormatGemini} {
					_, err := render.EncodeMCP(format, server)

					_, ok := errors.AsType[*render.RenderError](err)

					Convey("Then it is refused on "+string(format), func() {
						So(ok, ShouldBeTrue)
					})
				}
			})
		}
	})

	Convey("Given an unset transport", t, func() {
		Convey("When a command or a url picks the shape", func() {
			command := manifest.MCPServer{Name: "cmd", Command: []string{"node", "x.js"}}
			url := manifest.MCPServer{Name: "url", URL: "https://x.test/mcp"}

			Convey("Then both still encode", func() {
				_, commandErr := render.EncodeMCP(manifest.FormatClaude, command)
				_, urlErr := render.EncodeMCP(manifest.FormatClaude, url)

				So(commandErr, ShouldBeNil)
				So(urlErr, ShouldBeNil)
			})
		})
	})
}

func TestMCPEdits(t *testing.T) {
	Convey("Given servers for the JSONC dialects", t, func() {
		servers := []manifest.MCPServer{
			{Name: "zeta", Command: []string{"z"}, Transport: "stdio"},
			{Name: "alpha", URL: "https://a.test", Transport: "sse"},
		}

		Convey("When Claude edits are built", func() {
			edits, err := render.MCPEdits(manifest.FormatClaude, servers)

			Convey("Then they are sorted and keyed by mcpServers.<name>", func() {
				So(err, ShouldBeNil)
				So(edits, ShouldHaveLength, 2)
				So(edits[0].Path, ShouldEqual, "mcpServers.alpha")
				So(edits[1].Path, ShouldEqual, "mcpServers.zeta")
				So(edits[0].Value, ShouldResemble, map[string]any{"type": "http", "url": "https://a.test"})
			})
		})

		Convey("When Codex edits are built", func() {
			edits, err := render.MCPEdits(manifest.FormatCodex, servers)

			Convey("Then they are keyed by mcp_servers.<name>", func() {
				So(err, ShouldBeNil)
				So(edits[0].Path, ShouldEqual, "mcp_servers.alpha")
				So(edits[1].Path, ShouldEqual, "mcp_servers.zeta")
			})
		})

		Convey("When Gemini edits are built", func() {
			edits, err := render.MCPEdits(manifest.FormatGemini, servers)

			Convey("Then they are keyed by mcpServers.<name>", func() {
				So(err, ShouldBeNil)
				So(edits[0].Path, ShouldEqual, "mcpServers.alpha")
			})
		})
	})

	Convey("Given duplicate server names", t, func() {
		Convey("When edits are built", func() {
			_, err := render.MCPEdits(manifest.FormatClaude, []manifest.MCPServer{
				{Name: "dup", Command: []string{"a"}},
				{Name: "dup", Command: []string{"b"}},
			})

			Convey("Then the duplicates are refused", func() {
				So(err, ShouldBeError)
			})
		})
	})

	Convey("Given invalid server names", t, func() {
		for _, name := range []string{"", "bad name", "bad/name", "ключ", "bad:name"} {
			Convey("When the name is "+name, func() {
				_, err := render.MCPEdits(manifest.FormatClaude, []manifest.MCPServer{{Name: name, Command: []string{"x"}}})

				Convey("Then it is refused", func() {
					So(err, ShouldBeError)
				})
			})
		}
	})

	Convey("Given names from the allowed alphabet", t, func() {
		for _, name := range []string{"a", "a.b", "a_b", "a-b", "A1"} {
			Convey("When the name is "+name, func() {
				_, err := render.MCPEdits(manifest.FormatClaude, []manifest.MCPServer{{Name: name, Command: []string{"x"}}})

				Convey("Then it is accepted", func() {
					So(err, ShouldBeNil)
				})
			})
		}
	})

	Convey("Given the Agent Plugins format", t, func() {
		Convey("When edits are built", func() {
			_, err := render.MCPEdits(manifest.FormatAgentPlugins, []manifest.MCPServer{{Name: "a", Command: []string{"x"}}})

			Convey("Then the format is refused", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestClaudeMCPArgs(t *testing.T) {
	Convey("Given a stdio server", t, func() {
		server := manifest.MCPServer{
			Name:      "acme",
			Transport: "stdio",
			Command:   []string{"npx", "-y", "@acme/mcp"},
			Env:       map[string]string{"TOKEN": "{env:TOKEN}"}, //nolint:gosec // G101: synthetic fixture value
		}

		Convey("When the add args are built", func() {
			args, err := render.ClaudeMCPAddArgs(server)

			Convey("Then the user-scope stdio argv is built with env flags", func() {
				So(err, ShouldBeNil)
				So(args, ShouldResemble, []string{
					"mcp", "add", "--scope", "user", "--transport", "stdio", "acme",
					"npx", "-y", "@acme/mcp",
					"--env", "TOKEN=${TOKEN}",
				})
			})
		})
	})

	Convey("Given a remote server with headers", t, func() {
		server := manifest.MCPServer{
			Name:      "remote",
			Transport: "sse",
			URL:       "https://acme.test/sse",
			Headers:   map[string]string{"Authorization": "Bearer {env:TOKEN}"},
		}

		Convey("When the add args are built", func() {
			args, err := render.ClaudeMCPAddArgs(server)

			Convey("Then the sse transport and header flags are built", func() {
				So(err, ShouldBeNil)
				So(args, ShouldResemble, []string{
					"mcp", "add", "--scope", "user", "--transport", "sse", "remote",
					"https://acme.test/sse",
					"--header", "Authorization: Bearer ${TOKEN}",
				})
			})
		})
	})

	Convey("Given a streamable-http server", t, func() {
		Convey("When the add args are built", func() {
			args, err := render.ClaudeMCPAddArgs(manifest.MCPServer{Name: "r", Transport: "streamable-http", URL: "https://x"})

			Convey("Then the http transport is used", func() {
				So(err, ShouldBeNil)
				So(args, ShouldResemble, []string{"mcp", "add", "--scope", "user", "--transport", "http", "r", "https://x"})
			})
		})
	})

	Convey("Given a server without a command or url", t, func() {
		Convey("When the add args are built", func() {
			_, err := render.ClaudeMCPAddArgs(manifest.MCPServer{Name: "empty"})

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then it is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a server without a name", t, func() {
		Convey("When the add args are built", func() {
			_, err := render.ClaudeMCPAddArgs(manifest.MCPServer{Command: []string{"x"}})

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then it is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a server name", t, func() {
		Convey("When the remove args are built", func() {
			Convey("Then the user-scope removal is built", func() {
				So(render.ClaudeMCPRemoveArgs("acme"), ShouldResemble, []string{"mcp", "remove", "--scope", "user", "acme"})
			})
		})
	})
}
