package render_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/render"
)

// decodeHooks decodes a rendered hooks object.
func decodeHooks(t *testing.T, data []byte) map[string]any {
	t.Helper()

	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode hooks: %v", err)
	}

	return out
}

func TestPlanHooksClaude(t *testing.T) {
	Convey("Given a settings document with foreign hooks", t, func() {
		existing := []byte(`{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "ours.sh"}]},
      {"matcher": "Write", "hooks": [{"type": "command", "command": "foreign.sh"}]}
    ],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "oldsession.sh"}]}
    ]
  }
}`)

		hooks := []manifest.Hook{
			{Event: manifest.EventPreTool, Matcher: "Bash", Command: "ours.sh", Timeout: 10},
			{Event: manifest.EventSessionStart, Matcher: "*", Command: "start.sh"},
		}

		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, existing, hooks)

			Convey("Then our entries are re-rendered, foreign ones kept", func() {
				So(err, ShouldBeNil)
				So(plan.Rendered, ShouldEqual, 2)
				So(plan.Owned, ShouldResemble, []string{"ours.sh", "start.sh"})
				So(plan.File, ShouldNotBeNil)

				doc := decodeHooks(t, plan.File)

				pre, ok := doc["PreToolUse"].([]any)
				So(ok, ShouldBeTrue)
				So(pre, ShouldHaveLength, 2)
				So(pre[0], ShouldResemble, map[string]any{
					"matcher": "Write",
					"hooks":   []any{map[string]any{"type": "command", "command": "foreign.sh"}},
				})
				So(pre[1], ShouldResemble, map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": "ours.sh", "timeout": float64(10)}},
				})

				start, ok := doc["SessionStart"].([]any)
				So(ok, ShouldBeTrue)
				So(start, ShouldHaveLength, 2)
				So(start[0], ShouldResemble, map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "oldsession.sh"}},
				})
				So(start[1], ShouldResemble, map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "start.sh"}},
				})
			})

			Convey("Then the output is strict JSON with a trailing newline", func() {
				So(strings.HasSuffix(string(plan.File), "\n"), ShouldBeTrue)
				So(json.Valid(plan.File), ShouldBeTrue)
			})

			Convey("Then planning again is a no-op", func() {
				second, secondErr := render.PlanHooks(manifest.FormatClaude, plan.File, hooks)

				So(secondErr, ShouldBeNil)
				So(second.File, ShouldBeNil)
				So(second.Rendered, ShouldEqual, 2)
			})
		})
	})
}

func TestPlanHooksClaudeEdges(t *testing.T) {
	Convey("Given no existing document", t, func() {
		hooks := []manifest.Hook{{Event: manifest.EventPreTool, Matcher: "Bash", Command: "run.sh"}}

		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, nil, hooks)

			Convey("Then the hooks object is created", func() {
				So(err, ShouldBeNil)
				So(decodeHooks(t, plan.File), ShouldResemble, map[string]any{
					"PreToolUse": []any{map[string]any{
						"matcher": "Bash",
						"hooks":   []any{map[string]any{"type": "command", "command": "run.sh"}},
					}},
				})
			})
		})
	})

	Convey("Given a hook command with a host variable", t, func() {
		hooks := []manifest.Hook{
			{Event: manifest.EventPreTool, Command: "node \"${CLAUDE_PLUGIN_ROOT}/hook.js\"", Timeout: 5},
		}

		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, nil, hooks)

			Convey("Then the variable is preserved verbatim, never expanded or refused", func() {
				So(err, ShouldBeNil)

				doc := decodeHooks(t, plan.File)
				groups, ok := doc["PreToolUse"].([]any)
				So(ok, ShouldBeTrue)

				group, ok := groups[0].(map[string]any)
				So(ok, ShouldBeTrue)

				handlers, ok := group["hooks"].([]any)
				So(ok, ShouldBeTrue)

				handler, ok := handlers[0].(map[string]any)
				So(ok, ShouldBeTrue)

				So(handler["command"], ShouldEqual, "node \"${CLAUDE_PLUGIN_ROOT}/hook.js\"")
				So(handler["timeout"], ShouldEqual, float64(5))
			})
		})
	})

	Convey("Given an empty hooks list and no existing document", t, func() {
		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, nil, nil)

			Convey("Then nothing is written", func() {
				So(err, ShouldBeNil)
				So(plan.File, ShouldBeNil)
				So(plan.Rendered, ShouldEqual, 0)
				So(plan.Warnings, ShouldBeEmpty)
			})
		})
	})

	Convey("Given an existing entry that is ours with an outdated shape", t, func() {
		existing := []byte(`{"hooks": {"PreToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "ours.sh", "timeout": 99}]}]}}`)

		Convey("When the same hook is planned without the timeout", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, existing, []manifest.Hook{
				{Event: manifest.EventPreTool, Matcher: "Write", Command: "ours.sh"},
			})

			Convey("Then the entry is replaced in place, never duplicated", func() {
				So(err, ShouldBeNil)
				So(decodeHooks(t, plan.File), ShouldResemble, map[string]any{
					"PreToolUse": []any{map[string]any{
						"matcher": "Write",
						"hooks":   []any{map[string]any{"type": "command", "command": "ours.sh"}},
					}},
				})
			})
		})
	})

	Convey("Given an existing entry identical to the render", t, func() {
		existing := []byte(`{"hooks": {"PreToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "ours.sh"}]}]}}`)

		Convey("When the same hook is planned again", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, existing, []manifest.Hook{
				{Event: manifest.EventPreTool, Matcher: "Write", Command: "ours.sh"},
			})

			Convey("Then nothing is rewritten", func() {
				So(err, ShouldBeNil)
				So(plan.File, ShouldBeNil)
				So(plan.Rendered, ShouldEqual, 1)
			})
		})
	})
}

func TestPlanHooksClaudeMixed(t *testing.T) {
	Convey("Given a mixed matcher group", t, func() {
		existing := []byte(`{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [
			{"type": "command", "command": "ours.sh"},
			{"type": "command", "command": "foreign.sh"}
		]}]}}`)

		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, existing, []manifest.Hook{
				{Event: manifest.EventPreTool, Matcher: "Bash", Command: "ours.sh", Timeout: 5},
			})

			Convey("Then the foreign handler stays and the canon one is re-rendered", func() {
				So(err, ShouldBeNil)

				doc := decodeHooks(t, plan.File)
				groups, ok := doc["PreToolUse"].([]any)
				So(ok, ShouldBeTrue)

				So(groups, ShouldHaveLength, 2)
				So(groups[0], ShouldResemble, map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": "foreign.sh"}},
				})
				So(groups[1], ShouldResemble, map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": "ours.sh", "timeout": float64(5)}},
				})

				So(len(plan.Warnings), ShouldBeGreaterThan, 0)
			})
		})
	})
}

func TestPlanHooksClaudeMalformed(t *testing.T) {
	Convey("Given a malformed event value", t, func() {
		existing := []byte(`{"hooks": {"PreToolUse": {"broken": true}}}`)

		Convey("When hooks for the malformed event and a healthy one are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, existing, []manifest.Hook{
				{Event: manifest.EventPreTool, Matcher: "Bash", Command: "run.sh"},
				{Event: manifest.EventSessionStart, Command: "start.sh"},
			})

			Convey("Then the malformed event is left untouched and the change is only the healthy event", func() {
				So(err, ShouldBeNil)
				So(plan.File, ShouldNotBeNil)
				So(plan.Rendered, ShouldEqual, 2)

				doc := decodeHooks(t, plan.File)
				So(doc["PreToolUse"], ShouldResemble, map[string]any{"broken": true})
				So(doc["SessionStart"], ShouldResemble, []any{map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "start.sh"}},
				}})

				warned := false

				for _, warn := range plan.Warnings {
					if strings.Contains(warn, "PreToolUse") && strings.Contains(warn, "not an array") {
						warned = true
					}
				}

				So(warned, ShouldBeTrue)
			})
		})
	})

	Convey("Given a malformed event and nothing else to change", t, func() {
		existing := []byte(`{"hooks": {"PreToolUse": {"broken": true}}}`)

		Convey("When only the malformed event is planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, existing, []manifest.Hook{
				{Event: manifest.EventPreTool, Command: "run.sh"},
			})

			Convey("Then nothing changes and the file stays absent", func() {
				So(err, ShouldBeNil)
				So(plan.File, ShouldBeNil)
			})
		})
	})

	Convey("Given a matcher on a non-matcher event", t, func() {
		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatClaude, nil, []manifest.Hook{
				{Event: manifest.EventSessionStart, Matcher: "Bash", Command: "start.sh"},
				{Event: manifest.EventStop, Matcher: "Bash", Command: "stop.sh"},
			})

			Convey("Then the matchers are dropped with warnings", func() {
				So(err, ShouldBeNil)

				doc := decodeHooks(t, plan.File)
				So(doc["SessionStart"], ShouldResemble, []any{map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "start.sh"}},
				}})
				So(doc["Stop"], ShouldResemble, []any{map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "stop.sh"}},
				}})
				So(len(plan.Warnings), ShouldEqual, 2)
			})
		})
	})
}

func TestPlanHooksClaudeErrors(t *testing.T) {
	Convey("Given a broken JSON document", t, func() {
		Convey("When the hooks are planned", func() {
			_, err := render.PlanHooks(manifest.FormatClaude, []byte("{nope"), nil)

			_, ok := errors.AsType[*render.ConfigParseError](err)

			Convey("Then a ConfigParseError is reported", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestPlanHooksGemini(t *testing.T) {
	Convey("Given Gemini hooks", t, func() {
		hooks := []manifest.Hook{
			{Event: manifest.EventPreTool, Matcher: "write", Command: "before.sh"},
			{Event: manifest.EventStop, Command: "stop.sh"},
		}

		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatGemini, nil, hooks)

			Convey("Then Gemini event names are used and Stop is skipped", func() {
				So(err, ShouldBeNil)
				So(plan.Rendered, ShouldEqual, 1)
				So(plan.Owned, ShouldResemble, []string{"before.sh"})

				doc := decodeHooks(t, plan.File)
				So(doc, ShouldResemble, map[string]any{
					"BeforeTool": []any{map[string]any{
						"matcher": "write",
						"hooks":   []any{map[string]any{"type": "command", "command": "before.sh"}},
					}},
				})

				found := false

				for _, warn := range plan.Warnings {
					if strings.Contains(warn, "stop") {
						found = true
					}
				}

				So(found, ShouldBeTrue)
			})
		})
	})

	Convey("Given a bare Gemini hooks object", t, func() {
		existing := []byte(`{"BeforeTool": [{"hooks": [{"type": "command", "command": "keep.sh"}]}]}`)

		Convey("When an unrelated hook is planned", func() {
			plan, err := render.PlanHooks(manifest.FormatGemini, existing, []manifest.Hook{
				{Event: manifest.EventSessionStart, Command: "start.sh"},
			})

			Convey("Then the bare shape is understood and the foreign entry kept", func() {
				So(err, ShouldBeNil)

				doc := decodeHooks(t, plan.File)
				So(doc["BeforeTool"], ShouldHaveLength, 1)
				So(doc["SessionStart"], ShouldHaveLength, 1)
			})
		})
	})
}

func TestPlanHooksCodex(t *testing.T) {
	Convey("Given Codex hooks", t, func() {
		hooks := []manifest.Hook{
			{Event: manifest.EventPostTool, Matcher: "*", Command: "after.sh"},
		}

		Convey("When the hooks are planned", func() {
			plan, err := render.PlanHooks(manifest.FormatCodex, nil, hooks)

			Convey("Then Codex event names are used and the wildcard matcher omitted", func() {
				So(err, ShouldBeNil)
				So(decodeHooks(t, plan.File), ShouldResemble, map[string]any{
					"PostToolUse": []any{map[string]any{
						"hooks": []any{map[string]any{"type": "command", "command": "after.sh"}},
					}},
				})
			})
		})
	})
}

func TestPlanHooksUnsupportedFormat(t *testing.T) {
	Convey("Given the Agent Plugins format", t, func() {
		Convey("When hooks are planned", func() {
			_, err := render.PlanHooks(manifest.FormatAgentPlugins, nil, nil)

			Convey("Then the format is rejected", func() {
				So(err, ShouldBeError)
			})
		})
	})
}
