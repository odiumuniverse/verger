package render_test

import (
	"errors"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/render"
)

func TestParseAgentMarkdown(t *testing.T) {
	Convey("Given a full agent document", t, func() {
		data := []byte(`---
name: reviewer
description: Reviews diffs
mode: primary
model: opus
tools:
  - Read
  - Grep
disallowedTools: [Write]
permissionMode: plan
maxTurns: 12
skills:
  - review
mcpServers:
  - context7
background: true
isolation: worktree
color: blue
hidden: false
unknown: dropped
---
Review carefully.
`)

		Convey("When it is parsed", func() {
			agent, err := render.ParseAgentMarkdown(data)

			Convey("Then the canonical fields are read", func() {
				So(err, ShouldBeNil)
				So(agent.Name, ShouldEqual, "reviewer")
				So(agent.Description, ShouldEqual, "Reviews diffs")
				So(agent.Mode, ShouldEqual, "primary")
				So(agent.Model, ShouldEqual, "opus")
				So(agent.Tools, ShouldResemble, []string{"Read", "Grep"})
				So(agent.DisallowedTools, ShouldResemble, []string{"Write"})
				So(agent.PermissionMode, ShouldEqual, "plan")
				So(agent.MaxTurns, ShouldEqual, 12)
				So(agent.Skills, ShouldResemble, []string{"review"})
				So(agent.MCPServers, ShouldResemble, []any{"context7"})
				So(agent.Background, ShouldResemble, new(true))
				So(agent.Isolation, ShouldEqual, "worktree")
				So(agent.Color, ShouldEqual, "blue")
				So(agent.Hidden, ShouldResemble, new(false))
				So(agent.Body, ShouldEqual, "Review carefully.\n")
			})
		})
	})

	Convey("Given a body-only agent document", t, func() {
		Convey("When it is parsed", func() {
			agent, err := render.ParseAgentMarkdown([]byte("Just a prompt\n"))

			Convey("Then only the body is set", func() {
				So(err, ShouldBeNil)
				So(agent, ShouldResemble, render.Agent{Body: "Just a prompt\n"})
			})
		})
	})

	Convey("Given mcpServers holding maps", t, func() {
		data := []byte("---\nname: a\nmcpServers:\n  - server: context7\n    type: http\n---\nBody\n")

		Convey("When it is parsed", func() {
			agent, err := render.ParseAgentMarkdown(data)

			Convey("Then the untyped entries survive", func() {
				So(err, ShouldBeNil)
				So(agent.MCPServers, ShouldResemble, []any{map[string]any{"server": "context7", "type": "http"}})
			})
		})
	})

	Convey("Given malformed frontmatter", t, func() {
		Convey("When it is parsed", func() {
			_, err := render.ParseAgentMarkdown([]byte("---\ntools: [unclosed\n---\nBody\n"))

			Convey("Then an error is reported", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestAgentClaudeMarkdown(t *testing.T) {
	Convey("Given a canonical agent", t, func() {
		agent := render.Agent{
			Name:            "reviewer",
			Description:     "Reviews diffs",
			Mode:            "primary",
			Model:           "opus",
			Tools:           []string{"Read", "Grep"},
			DisallowedTools: []string{"Write"},
			PermissionMode:  "plan",
			MaxTurns:        12,
			Skills:          []string{"review"},
			MCPServers:      []any{"context7"},
			Background:      new(true),
			Isolation:       "worktree",
			Color:           "blue",
			Hidden:          new(false),
			Body:            "Review carefully.\n",
		}

		Convey("When it is rendered", func() {
			out := agent.ClaudeMarkdown()

			Convey("Then the golden document carries the canonical order", func() {
				So(string(out), ShouldEqual, `---
name: reviewer
description: Reviews diffs
mode: primary
model: opus
tools: [Read, Grep]
disallowedTools: [Write]
permissionMode: plan
maxTurns: 12
skills: [review]
mcpServers: [context7]
background: true
isolation: worktree
color: blue
hidden: false
---
Review carefully.
`)
			})
		})

		Convey("When it is parsed back", func() {
			back, err := render.ParseAgentMarkdown(agent.ClaudeMarkdown())

			Convey("Then the document round-trips", func() {
				So(err, ShouldBeNil)
				So(back, ShouldResemble, agent)
			})
		})
	})

	Convey("Given an empty agent", t, func() {
		Convey("When it is rendered", func() {
			Convey("Then only the body is written", func() {
				So(string(render.Agent{Body: "Body\n"}.ClaudeMarkdown()), ShouldEqual, "Body\n")
			})
		})
	})
}

func TestAgentGeminiMarkdown(t *testing.T) {
	Convey("Given an agent with canonical tools", t, func() {
		agent := render.Agent{
			Name:        "reviewer",
			Description: "Reviews diffs",
			Model:       "gemini-3-pro",
			Tools:       []string{"Read", "Write", "Edit", "Bash", "Grep", "Glob", "WebFetch", "WebSearch", "Skill", "Task", "NotebookEdit"},
			MaxTurns:    5,
			Body:        "Review carefully.\n",
		}

		Convey("When the Gemini markdown is rendered", func() {
			out := agent.GeminiMarkdown()

			Convey("Then the tool vocabulary is translated and unmappable tools kept", func() {
				So(string(out), ShouldEqual, `---
name: reviewer
description: Reviews diffs
model: gemini-3-pro
tools: [read_file, write_file, replace, run_shell_command, search_file_content, glob, web_fetch, google_web_search, activate_skill, Task, NotebookEdit]
maxTurns: 5
---
Review carefully.
`)

				back, err := render.ParseAgentMarkdown(out)
				So(err, ShouldBeNil)
				So(back.Tools, ShouldResemble, []string{
					"read_file", "write_file", "replace", "run_shell_command",
					"search_file_content", "glob", "web_fetch", "google_web_search",
					"activate_skill", "Task", "NotebookEdit",
				})
			})
		})
	})

	Convey("Given MCP tools", t, func() {
		agent := render.Agent{Tools: []string{"mcp__context7__query", "mcp__bad_server__x"}}

		Convey("When the Gemini markdown is rendered", func() {
			out := agent.GeminiMarkdown()

			Convey("Then underscore-server MCP names are kept unmapped", func() {
				So(strings.Contains(string(out), "mcp_context7_query"), ShouldBeTrue)
				So(strings.Contains(string(out), "mcp__bad_server__x"), ShouldBeTrue)
			})
		})
	})
}

// inexpressibleFields collects every Field of the InexpressibleErrors in a
// (possibly joined) error tree.
func inexpressibleFields(err error) []string {
	var fields []string

	var visit func(error)

	visit = func(current error) {
		if current == nil {
			return
		}

		if join, ok := current.(interface{ Unwrap() []error }); ok {
			for _, inner := range join.Unwrap() {
				visit(inner)
			}

			return
		}

		if target, ok := errors.AsType[*render.InexpressibleError](current); ok && target.Field != "" {
			fields = append(fields, target.Field)
		}
	}

	visit(err)

	return fields
}

func TestAgentCodexTOML(t *testing.T) {
	Convey("Given a canonical agent", t, func() {
		agent := render.Agent{
			Name:        "reviewer",
			Description: "Reviews diffs",
			Model:       "gpt-5-codex",
			Body:        "Review carefully.\n",
		}

		Convey("When the Codex TOML is rendered", func() {
			out, err := agent.CodexTOML()

			Convey("Then the golden TOML carries the five keys", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `name = "reviewer"
description = "Reviews diffs"
developer_instructions = """
Review carefully.
"""
model = "gpt-5-codex"
`)
			})
		})
	})

	Convey("Given an agent whose tools allow writes", t, func() {
		agent := render.Agent{Name: "writer", Model: "gpt-5-codex", Tools: []string{"Read", "Write"}, Body: "Write.\n"}

		Convey("When the Codex TOML is rendered", func() {
			out, err := agent.CodexTOML()

			Convey("Then no read-only sandbox is written and the allowlist is reported", func() {
				So(strings.Contains(string(out), "sandbox_mode"), ShouldBeFalse)
				So(inexpressibleFields(err), ShouldContain, "tools")
			})
		})
	})

	Convey("Given an agent with a read-only allowlist", t, func() {
		agent := render.Agent{Name: "reader", Tools: []string{"Read", "Grep"}, Body: "Read only.\n"}

		Convey("When the Codex TOML is rendered", func() {
			out, err := agent.CodexTOML()

			Convey("Then the sandbox is read-only and the allowlist loss is reported", func() {
				So(strings.Contains(string(out), `sandbox_mode = "read-only"`), ShouldBeTrue)
				So(inexpressibleFields(err), ShouldContain, "tools")
			})
		})
	})

	Convey("Given an agent denying every write tool", t, func() {
		agent := render.Agent{Name: "reader", Body: "Read only.\n", DisallowedTools: []string{"Write", "Edit", "NotebookEdit"}}

		Convey("When the Codex TOML is rendered", func() {
			out, err := agent.CodexTOML()

			Convey("Then the sandbox is read-only and nothing is reported", func() {
				So(err, ShouldBeNil)
				So(strings.Contains(string(out), `sandbox_mode = "read-only"`), ShouldBeTrue)
			})
		})
	})

	Convey("Given a body-less agent", t, func() {
		Convey("When the Codex TOML is rendered", func() {
			out, err := render.Agent{Name: "empty"}.CodexTOML()

			target, ok := errors.AsType[*render.InexpressibleError](err)

			Convey("Then it is inexpressible for Codex", func() {
				So(ok, ShouldBeTrue)
				So(target.Kind, ShouldEqual, "agent")
				So(target.Name, ShouldEqual, "empty")
				So(target.Field, ShouldEqual, "developer_instructions")
				So(out, ShouldBeNil)
			})
		})
	})

	Convey("Given an agent without a name", t, func() {
		Convey("When the Codex TOML is rendered", func() {
			_, err := render.Agent{Body: "Body\n"}.CodexTOML()

			target, ok := errors.AsType[*render.RenderError](err)

			Convey("Then the malformed input is reported", func() {
				So(ok, ShouldBeTrue)
				So(target.Kind, ShouldEqual, "agent")
			})
		})
	})

	Convey("Given an agent with Claude aliases and inexpressible fields", t, func() {
		agent := render.Agent{
			Name:           "reviewer",
			Model:          "sonnet",
			PermissionMode: "plan",
			Skills:         []string{"review"},
			Body:           "Body\n",
		}

		Convey("When the Codex TOML is rendered", func() {
			out, err := agent.CodexTOML()

			Convey("Then the bytes are usable and the dropped fields are reported", func() {
				So(string(out), ShouldContainSubstring, `name = "reviewer"`)
				So(strings.Contains(string(out), "model ="), ShouldBeFalse)
				So(strings.Contains(string(out), "sandbox_mode"), ShouldBeFalse)

				fields := inexpressibleFields(err)
				So(fields, ShouldContain, "model")
				So(fields, ShouldContain, "permissionMode")
				So(fields, ShouldContain, "skills")
			})
		})
	})
}

func TestAgentCodexTOMLRoundTrip(t *testing.T) {
	Convey("Given a rendered Codex TOML", t, func() {
		agent := render.Agent{
			Name:            "reviewer",
			Description:     "Reviews diffs",
			Model:           "gpt-5-codex",
			DisallowedTools: []string{"Write", "Edit", "NotebookEdit"},
			Body:            "Body with \"quotes\" and снег\n",
		}

		out, err := agent.CodexTOML()
		So(err, ShouldBeNil)

		Convey("When it is parsed as TOML", func() {
			var doc struct {
				Name                  string `toml:"name"`
				Description           string `toml:"description"`
				DeveloperInstructions string `toml:"developer_instructions"`
				Model                 string `toml:"model"`
				SandboxMode           string `toml:"sandbox_mode"`
			}

			unmarshalErr := toml.Unmarshal(out, &doc)

			Convey("Then every key round-trips", func() {
				So(unmarshalErr, ShouldBeNil)
				So(doc.Name, ShouldEqual, agent.Name)
				So(doc.Description, ShouldEqual, agent.Description)
				So(doc.DeveloperInstructions, ShouldEqual, agent.Body)
				So(doc.Model, ShouldEqual, agent.Model)
				So(doc.SandboxMode, ShouldEqual, "read-only")
			})
		})
	})
}
