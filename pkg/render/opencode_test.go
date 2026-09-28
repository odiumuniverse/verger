package render_test

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/render"
)

// TestMCPPrefix pins the key path of every shared-config MCP dialect: the
// prefix is the one thing pkg/host cannot derive from the value shape, and the
// OpenCode v1/v2 containers differ only in it.
func TestMCPPrefix(t *testing.T) {
	Convey("Given the MCP config dialects", t, func() {
		Convey("When each prefix is asked for", func() {
			Convey("Then every dialect names its container key", func() {
				So(render.MCPPrefix(manifest.FormatClaude), ShouldEqual, "mcpServers.")
				So(render.MCPPrefix(manifest.FormatGemini), ShouldEqual, "mcpServers.")
				So(render.MCPPrefix(manifest.FormatCodex), ShouldEqual, "mcp_servers.")
				So(render.MCPPrefix(manifest.FormatOpenCode), ShouldEqual, "mcp.")
			})
		})
	})
}

// TestMCPEditsUnder pins the explicit-prefix edit builder: the OpenCode v2
// container (`mcp.servers`) and the v1 one (`mcp`) carry the same values under
// different key paths.
func TestMCPEditsUnder(t *testing.T) {
	servers := []manifest.MCPServer{
		{Name: "web", Transport: "sse", URL: "https://acme.test/sse"},
		{Name: "fs", Transport: "stdio", Command: []string{"node", "server.js"}},
	}

	Convey("Given two servers in unsorted order", t, func() {
		Convey("When the OpenCode v2 container is edited", func() {
			edits, err := render.MCPEditsUnder(manifest.FormatOpenCode, "mcp.servers.", servers)

			Convey("Then the paths are sorted and carry the explicit prefix", func() {
				So(err, ShouldBeNil)
				So(paths(edits), ShouldResemble, []string{"mcp.servers.fs", "mcp.servers.web"})
			})
		})

		Convey("When the OpenCode v1 container is edited", func() {
			edits, err := render.MCPEditsUnder(manifest.FormatOpenCode, "mcp.", servers)

			Convey("Then the same values land under the legacy container", func() {
				So(err, ShouldBeNil)
				So(paths(edits), ShouldResemble, []string{"mcp.fs", "mcp.web"})
			})
		})

		Convey("When the dialect default is used", func() {
			edits, err := render.MCPEdits(manifest.FormatOpenCode, servers)

			Convey("Then it is the v1 container", func() {
				So(err, ShouldBeNil)
				So(paths(edits), ShouldResemble, []string{"mcp.fs", "mcp.web"})
			})
		})
	})
}

// TestEncodeMCPOpenCode pins the OpenCode/Kilo entry shape: `local` with the
// command as one array, `remote` with the url, and the environment map spelled
// `environment` (the keys the host reads, mcp.servers entries).
func TestEncodeMCPOpenCode(t *testing.T) {
	Convey("Given a stdio server", t, func() {
		server := manifest.MCPServer{
			Name:      "fs",
			Transport: "stdio",
			Command:   []string{"node", "${PLUGIN_ROOT}/server.js"},
			Env:       map[string]string{"TOKEN": "{env:TOKEN}", "SECRET": "{secret:ACME}"}, //nolint:gosec // G101: synthetic fixture values
		}

		Convey("When the OpenCode value is encoded", func() {
			value, err := render.EncodeMCP(manifest.FormatOpenCode, server)

			Convey("Then it is a local entry with an environment map and verbatim refs", func() {
				So(err, ShouldBeNil)
				So(value, ShouldResemble, map[string]any{
					"type":    "local",
					"command": []string{"node", "${PLUGIN_ROOT}/server.js"},
					//nolint:gosec // G101: synthetic fixture values
					"environment": map[string]string{"TOKEN": "{env:TOKEN}", "SECRET": "{secret:ACME}"},
				})
			})
		})
	})

	Convey("Given remote servers", t, func() {
		Convey("When an sse server is encoded", func() {
			server := manifest.MCPServer{
				Name: "web", Transport: "sse", URL: "https://acme.test/sse",
				Headers: map[string]string{"Authorization": "Bearer {secret:WEB}"},
			}

			value, err := render.EncodeMCP(manifest.FormatOpenCode, server)

			Convey("Then it is a remote entry with the url and headers", func() {
				So(err, ShouldBeNil)
				So(value, ShouldResemble, map[string]any{
					"type":    "remote",
					"url":     "https://acme.test/sse",
					"headers": map[string]string{"Authorization": "Bearer {secret:WEB}"},
				})
			})
		})

		Convey("When a streamable-http server is encoded", func() {
			server := manifest.MCPServer{Name: "web", Transport: "streamable-http", URL: "https://acme.test/mcp"}

			value, err := render.EncodeMCP(manifest.FormatOpenCode, server)

			Convey("Then the transport collapses to remote", func() {
				So(err, ShouldBeNil)
				So(value, ShouldResemble, map[string]any{"type": "remote", "url": "https://acme.test/mcp"})
			})
		})
	})

	Convey("Given an unencodable server", t, func() {
		Convey("When a server has neither command nor url", func() {
			_, err := render.EncodeMCP(manifest.FormatOpenCode, manifest.MCPServer{Name: "empty"})

			Convey("Then the canonical validation still refuses it", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

// TestOpenCodeAgentMarkdown pins the OpenCode/Kilo agent dialect, verified
// live on kilo 7.8.1 (`kilo debug agent`): `mode` outside primary|subagent|all,
// a non-hex `color` or a non-map `tools` make the host drop the whole file,
// and a Claude-style `tools` list is such a non-map.
func TestOpenCodeAgentMarkdown(t *testing.T) {
	Convey("Given a canonical agent inside the dialect", t, func() {
		agent := render.Agent{
			Name: "reviewer", Description: "Reviews diffs.", Mode: "subagent",
			Model: "anthropic/claude-sonnet", MaxTurns: 12, Color: "#FF5733",
			Hidden: new(false), Tools: []string{"Read", "Grep", "Bash"},
			DisallowedTools: []string{"Write"}, Body: "Review the diff.\n",
		}

		Convey("When it is rendered", func() {
			data, err := agent.OpenCodeMarkdown()

			Convey("Then the host keys are written in canonical order, with permissions and no name", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldEqual, `---
description: Reviews diffs.
mode: subagent
model: anthropic/claude-sonnet
steps: 12
color: '#FF5733'
hidden: false
permissions:
  - action: '*'
    resource: '*'
    effect: deny
  - action: read
    resource: '*'
    effect: allow
  - action: grep
    resource: '*'
    effect: allow
  - action: shell
    resource: '*'
    effect: allow
  - action: edit
    resource: '*'
    effect: deny
---
Review the diff.
`)
			})
		})
	})

	Convey("Given an agent the host would reject", t, func() {
		Convey("When the mode is not one of the host's three", func() {
			data, err := render.Agent{Name: "a", Mode: "weird", Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then the safest explicit mode is written and the drop is reported", func() {
				So(err, ShouldBeError)
				So(string(data), ShouldContainSubstring, "mode: subagent")
				So(inexpressible(t, err), ShouldContain, "mode")
				So(string(data), ShouldNotContainSubstring, "weird")
			})
		})

		Convey("When the color is not a hex color", func() {
			data, err := render.Agent{Name: "a", Mode: "primary", Color: "blue", Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then the color is dropped and reported", func() {
				So(err, ShouldBeError)
				So(inexpressible(t, err), ShouldContain, "color")
				So(string(data), ShouldNotContainSubstring, "color")
			})
		})

		Convey("When the model is a bare alias", func() {
			data, err := render.Agent{Name: "a", Mode: "subagent", Model: "sonnet", Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then the host-specific alias is dropped and reported", func() {
				So(err, ShouldBeError)
				So(inexpressible(t, err), ShouldContain, "model")
				So(string(data), ShouldNotContainSubstring, "model")
			})
		})

		Convey("When a tool has no action in the host dialect", func() {
			data, err := render.Agent{Name: "a", Mode: "subagent", Tools: []string{"Read", "CustomTool"}, Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then the unmappable tool is dropped and reported", func() {
				So(err, ShouldBeError)
				So(inexpressible(t, err), ShouldContain, "CustomTool")
				So(string(data), ShouldContainSubstring, "action: read")
				So(string(data), ShouldNotContainSubstring, "CustomTool")
			})
		})

		Convey("When the allowlist carries no write tool", func() {
			data, err := render.Agent{Name: "a", Mode: "subagent", Tools: []string{"Read", "Grep"}, Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then the agent is read-only: the edit action is denied", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldContainSubstring, "action: edit\n    resource: '*'\n    effect: deny")
			})
		})

		Convey("When a tool is an MCP tool", func() {
			data, err := render.Agent{Name: "a", Mode: "subagent", Tools: []string{"mcp__probe__do"}, Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then it maps to the host's <server>_<tool> action", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldContainSubstring, "action: probe_do")
			})
		})
	})

	Convey("Given an agent with no mode", t, func() {
		Convey("When it is rendered", func() {
			data, err := render.Agent{Name: "a", Description: "D.", Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then the host default is written explicitly as subagent", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldContainSubstring, "mode: subagent")
			})
		})
	})
}

// TestOpenCodeCommandMarkdown pins the command dialect: description and model
// only (the keys beadle's OpenCode command codec manages); the Claude-only
// keys are reported as dropped instead of written into a document the host
// reads with its own schema.
func TestOpenCodeCommandMarkdown(t *testing.T) {
	Convey("Given a canonical command with Claude-only fields", t, func() {
		disabled := true

		cmd := render.Command{
			Name: "dev", Description: "Runs the dev loop.", ArgumentHint: "[target]",
			Arguments: []string{"target"}, Model: "anthropic/claude-sonnet",
			DisableModelInvocation: &disabled, Body: "Run the dev loop.\n",
		}

		Convey("When it is rendered", func() {
			data, err := cmd.OpenCodeMarkdown()

			Convey("Then only the host keys are written and the rest is reported", func() {
				So(err, ShouldBeError)
				So(string(data), ShouldEqual, `---
description: Runs the dev loop.
model: anthropic/claude-sonnet
---
Run the dev loop.
`)
				So(inexpressible(t, err), ShouldContain, "argument-hint")
				So(inexpressible(t, err), ShouldContain, "arguments")
				So(inexpressible(t, err), ShouldContain, "disable-model-invocation")
			})
		})
	})

	Convey("Given a bare command", t, func() {
		Convey("When it is rendered", func() {
			data, err := render.Command{Name: "dev", Description: "D.", Body: "B.\n"}.OpenCodeMarkdown()

			Convey("Then it is description and body alone", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldEqual, "---\ndescription: D.\n---\nB.\n")
			})
		})
	})
}

// paths lists the edit key paths.
func paths(edits []render.Edit) []string {
	out := make([]string, 0, len(edits))

	for _, edit := range edits {
		out = append(out, edit.Path)
	}

	return out
}

// inexpressible flattens the joined *render.InexpressibleError fields.
func inexpressible(t *testing.T, err error) []string {
	t.Helper()

	var out []string

	for _, joined := range flatten(err) {
		if field, ok := errors.AsType[*render.InexpressibleError](joined); ok {
			out = append(out, field.Field)
		}
	}

	return out
}

// flatten splits an errors.Join tree into its leaves.
func flatten(err error) []error {
	if err == nil {
		return nil
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error

		for _, child := range joined.Unwrap() {
			out = append(out, flatten(child)...)
		}

		return out
	}

	return []error{err}
}
