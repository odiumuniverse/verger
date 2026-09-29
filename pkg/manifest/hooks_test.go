package manifest

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestValidEvent(t *testing.T) {
	Convey("Given a table of event names", t, func() {
		tests := []struct {
			event string
			want  bool
		}{
			{EventNotification, true},
			{EventPostTool, true},
			{EventPreTool, true},
			{EventSessionStart, true},
			{EventStop, true},
			{"PreToolUse", false},
			{"", false},
			{"pre-tool ", false},
		}

		for _, tt := range tests {
			Convey("When checking "+tt.event, func() {
				Convey("Then validity matches the canon", func() {
					So(ValidEvent(tt.event), ShouldEqual, tt.want)
				})
			})
		}
	})

	Convey("Given the canonical event constants", t, func() {
		Convey("Then their values are the shared vocabulary", func() {
			So(EventNotification, ShouldEqual, "notification")
			So(EventPostTool, ShouldEqual, "post-tool")
			So(EventPreTool, ShouldEqual, "pre-tool")
			So(EventSessionStart, ShouldEqual, "session-start")
			So(EventStop, ShouldEqual, "stop")
		})
	})
}

func TestCanonEvent(t *testing.T) {
	Convey("Given a table of dialect event names", t, func() {
		tests := []struct {
			format Format
			host   string
			canon  string
			ok     bool
		}{
			{FormatClaude, "PreToolUse", EventPreTool, true},
			{FormatClaude, "PostToolUse", EventPostTool, true},
			{FormatClaude, "SessionStart", EventSessionStart, true},
			{FormatClaude, "Stop", EventStop, true},
			{FormatClaude, "Notification", EventNotification, true},
			{FormatClaude, "UserPromptSubmit", "", false},
			{FormatClaude, "BeforeTool", "", false},
			{FormatCodex, "PreToolUse", EventPreTool, true},
			{FormatCodex, "PostToolUse", EventPostTool, true},
			{FormatCodex, "SessionStart", EventSessionStart, true},
			{FormatCodex, "Stop", EventStop, true},
			{FormatCodex, "Notification", EventNotification, true},
			{FormatCodex, "BeforeTool", "", false},
			{FormatGemini, "BeforeTool", EventPreTool, true},
			{FormatGemini, "AfterTool", EventPostTool, true},
			{FormatGemini, "SessionStart", EventSessionStart, true},
			{FormatGemini, "Notification", EventNotification, true},
			{FormatGemini, "Stop", "", false},
			{FormatGemini, "PreToolUse", "", false},
			{FormatAgentPlugins, "PreToolUse", "", false},
			{FormatAgentPlugins, "BeforeTool", "", false},
			{Format("unknown"), "PreToolUse", "", false},
		}

		for _, tt := range tests {
			Convey("When mapping "+string(tt.format)+" "+tt.host, func() {
				canon, ok := CanonEvent(tt.format, tt.host)

				Convey("Then the canonical event matches the dialect table", func() {
					So(ok, ShouldEqual, tt.ok)
					So(canon, ShouldEqual, tt.canon)
				})
			})
		}
	})
}

func TestHostEvent(t *testing.T) {
	Convey("Given a table of canonical events", t, func() {
		tests := []struct {
			format Format
			canon  string
			host   string
			ok     bool
		}{
			{FormatClaude, EventPreTool, "PreToolUse", true},
			{FormatClaude, EventPostTool, "PostToolUse", true},
			{FormatClaude, EventSessionStart, "SessionStart", true},
			{FormatClaude, EventStop, "Stop", true},
			{FormatClaude, EventNotification, "Notification", true},
			{FormatCodex, EventPreTool, "PreToolUse", true},
			{FormatCodex, EventStop, "Stop", true},
			{FormatGemini, EventPreTool, "BeforeTool", true},
			{FormatGemini, EventPostTool, "AfterTool", true},
			{FormatGemini, EventSessionStart, "SessionStart", true},
			{FormatGemini, EventNotification, "Notification", true},
			{FormatGemini, EventStop, "", false},
			{FormatAgentPlugins, EventPreTool, "", false},
			{Format("unknown"), EventPreTool, "", false},
		}

		for _, tt := range tests {
			Convey("When mapping "+string(tt.format)+" "+tt.canon, func() {
				host, ok := HostEvent(tt.format, tt.canon)

				Convey("Then the host event matches the dialect table", func() {
					So(ok, ShouldEqual, tt.ok)
					So(host, ShouldEqual, tt.host)
				})
			})
		}
	})

	Convey("Given every dialect event", t, func() {
		Convey("Then CanonEvent and HostEvent round-trip", func() {
			for _, format := range []Format{FormatClaude, FormatCodex, FormatGemini} {
				for _, canon := range []string{EventPreTool, EventPostTool, EventSessionStart, EventStop, EventNotification} {
					host, ok := HostEvent(format, canon)
					if !ok {
						continue
					}

					back, backOK := CanonEvent(format, host)

					So(backOK, ShouldBeTrue)
					So(back, ShouldEqual, canon)
				}
			}
		})
	})
}

func TestMatcherEvent(t *testing.T) {
	Convey("Given a table of canonical events", t, func() {
		tests := []struct {
			format Format
			canon  string
			want   bool
		}{
			{FormatClaude, EventPreTool, true},
			{FormatClaude, EventPostTool, true},
			{FormatClaude, EventSessionStart, false},
			{FormatClaude, EventStop, false},
			{FormatClaude, EventNotification, false},
			{FormatCodex, EventPreTool, true},
			{FormatCodex, EventPostTool, true},
			{FormatCodex, EventSessionStart, false},
			{FormatGemini, EventPreTool, true},
			{FormatGemini, EventPostTool, true},
			{FormatGemini, EventSessionStart, false},
			{FormatGemini, EventNotification, false},
			{FormatAgentPlugins, EventPreTool, false},
			{FormatAgentPlugins, EventPostTool, false},
		}

		for _, tt := range tests {
			Convey("When checking "+string(tt.format)+" "+tt.canon, func() {
				Convey("Then matchers are meaningful for pre/post-tool only", func() {
					So(MatcherEvent(tt.format, tt.canon), ShouldEqual, tt.want)
				})
			})
		}
	})
}

func TestHooksFromDocumentClaude(t *testing.T) {
	Convey("Given a Claude hooks document with groups, a flat handler and an unmapped event", t, func() {
		data := []byte(`{
  "$schema": "https://example.com/hooks.schema.json",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "run.sh", "timeout": 30}]},
      {"matcher": "*", "hooks": [{"type": "command", "command": "star.sh"}]},
      {"command": "flat.sh", "matcher": "Write"}
    ],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "start.sh", "statusMessage": "loading", "shell": "bash", "args": []}]}
    ],
    "PostToolUse": [
      {"matcher": "Write", "hooks": [{"type": "command", "command": "async.sh", "async": true}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "nope.sh"}]}
    ]
  }
}`)

		hooks, warns, err := hooksFromDocument(FormatClaude, "hooks/hooks.json", data)

		Convey("When the document is parsed", func() {
			Convey("Then the expressible handlers become canonical hooks in sorted order", func() {
				So(err, ShouldBeNil)

				So(hooks, ShouldResemble, []Hook{
					{Event: EventPreTool, Matcher: "", Command: "star.sh", Timeout: 0, Origin: FormatClaude},
					{Event: EventPreTool, Matcher: "Bash", Command: "run.sh", Timeout: 30, Origin: FormatClaude},
					{Event: EventPreTool, Matcher: "Write", Command: "flat.sh", Timeout: 0, Origin: FormatClaude},
					{Event: EventSessionStart, Matcher: "", Command: "start.sh", Timeout: 0, Origin: FormatClaude},
				})
			})

			Convey("Then unmapped events and unsupported fields are skipped with warnings", func() {
				So(warns, ShouldResemble, []string{
					"claude: hooks/hooks.json: hook post-tool handler has unsupported field(s) async; skipped",
					"claude: hooks/hooks.json: hook event UserPromptSubmit has no canonical mapping; skipped",
				})
			})
		})
	})
}

func TestHooksFromDocumentClaudeFilters(t *testing.T) {
	Convey("Given a hooks document with a non-string matcher and a non-command handler", t, func() {
		data := []byte(`{
  "PreToolUse": [
    {"matcher": 5, "hooks": [{"type": "command", "command": "ok.sh"}]},
    {"matcher": "Bash", "hooks": [{"type": "prompt", "command": "prompt.sh"}]},
    {"matcher": "Bash", "hooks": [{"type": "command"}]}
  ]
}`)

		hooks, warns, err := hooksFromDocument(FormatClaude, "hooks/hooks.json", data)

		Convey("When the document is parsed", func() {
			// Fail-closed: a matcher that cannot be read is not the same as no
			// matcher, so its entry is dropped rather than widened to every
			// tool call. The two command-shaped entries still decode.
			Convey("Then the entry with the unreadable matcher is dropped, and the other two decode to nothing", func() {
				So(err, ShouldBeNil)
				So(hooks, ShouldBeEmpty)

				So(warns, ShouldResemble, []string{
					"claude: hooks/hooks.json: hook matcher is not a string; hook ignored",
					"claude: hooks/hooks.json: hook pre-tool handler type \"prompt\" is not a command; skipped",
					"claude: hooks/hooks.json: hook pre-tool handler has no command; skipped",
				})
			})
		})
	})

	Convey("Given malformed hook shapes", t, func() {
		tests := []struct {
			name string
			data string
			warn string
		}{
			{
				name: "event is not a list",
				data: `{"PreToolUse": {}}`,
				warn: "claude: hooks/hooks.json: hook event PreToolUse is not a list of matcher groups; ignored",
			},
			{
				name: "entry is not an object",
				data: `{"PreToolUse": ["x"]}`,
				warn: "claude: hooks/hooks.json: hook entry is not an object; ignored",
			},
			{
				name: "entry is neither group nor handler",
				data: `{"PreToolUse": [{"foo": 1}]}`,
				warn: "claude: hooks/hooks.json: hook entry is neither a matcher group nor a handler; ignored",
			},
			{
				name: "negative timeout becomes zero",
				data: `{"PreToolUse": [{"hooks": [{"type": "command", "command": "x.sh", "timeout": -5}]}]}`,
				warn: "",
			},
			{
				name: "overflowing timeout is refused",
				data: `{"PreToolUse": [{"hooks": [{"type": "command", "command": "x.sh", "timeout": 1e12}]}]}`,
				warn: "claude: hooks/hooks.json: hook pre-tool handler has unsupported field(s) timeout; skipped",
			},
		}

		for _, tt := range tests {
			Convey("When parsing "+tt.name, func() {
				hooks, warns, err := hooksFromDocument(FormatClaude, "hooks/hooks.json", []byte(tt.data))

				Convey("Then nothing is guessed and the warning names the file", func() {
					So(err, ShouldBeNil)

					if tt.warn == "" {
						So(warns, ShouldBeEmpty)
						So(hooks, ShouldHaveLength, 1)
						So(hooks[0].Timeout, ShouldEqual, 0)
					} else {
						So(hooks, ShouldBeEmpty)
						So(warns, ShouldResemble, []string{tt.warn})
					}
				})
			})
		}
	})

	Convey("Given a hooks document that is not JSON", t, func() {
		Convey("When the reader runs", func() {
			_, _, err := hooksFromDocument(FormatClaude, "hooks/hooks.json", []byte("{nope"))

			Convey("Then the parse error is reported", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "parse hooks")
			})
		})
	})
}

func TestHooksFromDocumentGemini(t *testing.T) {
	Convey("Given a Gemini bare event map", t, func() {
		data := []byte(`{
  "BeforeTool": [{"matcher": "write", "hooks": [{"type": "command", "command": "before.sh"}]}],
  "AfterTool": [{"hooks": [{"type": "command", "command": "after.sh"}]}],
  "Stop": [{"hooks": [{"type": "command", "command": "stop.sh"}]}]
}`)

		hooks, warns, err := hooksFromDocument(FormatGemini, "hooks/hooks.json", data)

		Convey("When the document is parsed", func() {
			Convey("Then mapped events are canonical and Stop is skipped with a warning", func() {
				So(err, ShouldBeNil)
				So(hooks, ShouldResemble, []Hook{
					{Event: EventPostTool, Matcher: "", Command: "after.sh", Timeout: 0, Origin: FormatGemini},
					{Event: EventPreTool, Matcher: "write", Command: "before.sh", Timeout: 0, Origin: FormatGemini},
				})
				So(warns, ShouldResemble, []string{
					"gemini: hooks/hooks.json: hook event Stop has no canonical mapping; skipped",
				})
			})
		})
	})

	Convey("Given a Codex wrapped document", t, func() {
		data := []byte(`{"hooks": {"PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "post.sh"}]}]}}`)

		hooks, warns, err := hooksFromDocument(FormatCodex, "hooks/hooks.json", data)

		Convey("When the document is parsed", func() {
			Convey("Then the Codex table maps the event", func() {
				So(err, ShouldBeNil)
				So(warns, ShouldBeEmpty)
				So(hooks, ShouldResemble, []Hook{
					{Event: EventPostTool, Matcher: "Bash", Command: "post.sh", Timeout: 0, Origin: FormatCodex},
				})
			})
		})
	})
}

func TestHooksMetaKeys(t *testing.T) {
	Convey("Given a bare document with meta keys", t, func() {
		data := []byte(`{"$schema": "x", "description": "y", "version": 2, "PreToolUse": [{"hooks": [{"type": "command", "command": "x.sh"}]}]}`)

		hooks, warns, err := hooksFromDocument(FormatClaude, "hooks/hooks.json", data)

		Convey("When the document is parsed", func() {
			Convey("Then meta keys are ignored without warnings", func() {
				So(err, ShouldBeNil)
				So(warns, ShouldBeEmpty)
				So(hooks, ShouldHaveLength, 1)
			})
		})
	})
}

// writeHookDoc writes a document below root.
func writeHookDoc(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCodexHookPrecedence(t *testing.T) {
	Convey("Given extensions hooks, manifest hooks and a hooks file", t, func() {
		root := t.TempDir()
		writeHookDoc(t, root, "plugin.json", `{
  "name": "precedence",
  "extensions": {"com.openai": {"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "ext.sh"}]}]}}},
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "manifest.sh"}]}]}
}`)
		writeHookDoc(t, root, "hooks/hooks.json", `{"hooks": {"PostToolUse": [{"hooks": [{"type": "command", "command": "file.sh"}]}]}}`)

		result, err := parseCodex(root)

		Convey("When the plugin is parsed", func() {
			Convey("Then the extensions declaration wins", func() {
				So(err, ShouldBeNil)
				So(result.hooks, ShouldResemble, []Hook{
					{Event: EventPreTool, Matcher: "", Command: "ext.sh", Timeout: 0, Origin: FormatCodex},
				})
			})
		})
	})

	Convey("Given manifest hooks and a hooks file", t, func() {
		root := t.TempDir()
		writeHookDoc(t, root, "plugin.json", `{
  "name": "precedence",
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "manifest.sh"}]}]}
}`)
		writeHookDoc(t, root, "hooks/hooks.json", `{"hooks": {"PostToolUse": [{"hooks": [{"type": "command", "command": "file.sh"}]}]}}`)

		result, err := parseCodex(root)

		Convey("When the plugin is parsed", func() {
			Convey("Then the manifest declaration wins over the file", func() {
				So(err, ShouldBeNil)
				So(result.hooks, ShouldResemble, []Hook{
					{Event: EventSessionStart, Matcher: "", Command: "manifest.sh", Timeout: 0, Origin: FormatCodex},
				})
			})
		})
	})

	Convey("Given an extensions hooks path", t, func() {
		root := t.TempDir()
		writeHookDoc(t, root, "plugin.json", `{"name": "precedence", "extensions": {"com.openai": {"hooks": "hooks/openai.json"}}}`)
		writeHookDoc(t, root, "hooks/openai.json", `{"hooks": {"Notification": [{"hooks": [{"type": "command", "command": "openai.sh"}]}]}}`)

		result, err := parseCodex(root)

		Convey("When the plugin is parsed", func() {
			Convey("Then the referenced document is read", func() {
				So(err, ShouldBeNil)
				So(result.hooks, ShouldResemble, []Hook{
					{Event: EventNotification, Matcher: "", Command: "openai.sh", Timeout: 0, Origin: FormatCodex},
				})
			})
		})
	})

	Convey("Given an extensions hooks path that escapes the root", t, func() {
		root := t.TempDir()
		writeHookDoc(t, root, "plugin.json", `{"name": "precedence", "extensions": {"com.openai": {"hooks": "../outside.json"}}}`)

		result, err := parseCodex(root)

		Convey("When the plugin is parsed", func() {
			Convey("Then the path is ignored with a warning", func() {
				So(err, ShouldBeNil)
				So(result.hooks, ShouldBeEmpty)
				So(result.warnings, ShouldResemble, []string{
					`codex: plugin.json: hook path "../outside.json" escapes the package root; ignored`,
				})
			})
		})
	})
}

// TestHookDialectTableIsSingleSourced pins the one-table rule: every canonical
// event a dialect speaks is a valid event, every host event round-trips back
// to the canonical event it maps to, and the reverse lookup never depends on
// map iteration order.
func TestHookDialectTableIsSingleSourced(t *testing.T) {
	Convey("Given the dialects the manifest knows", t, func() {
		Convey("When each mapping is checked in both directions", func() {
			for _, format := range []Format{FormatClaude, FormatCodex, FormatGemini} {
				events := canonEventsFor(format)
				So(events, ShouldNotBeEmpty)

				for hostEvent, canon := range events {
					So(ValidEvent(canon), ShouldBeTrue)

					back, ok := HostEvent(format, canon)
					So(ok, ShouldBeTrue)
					So(back, ShouldEqual, hostEvent)
				}
			}
		})

		Convey("Then the reverse lookup is stable across repeated calls", func() {
			for range 50 {
				host, ok := HostEvent(FormatClaude, EventStop)
				So(ok, ShouldBeTrue)
				So(host, ShouldEqual, "Stop")
			}
		})

		Convey("Then Cursor keeps its documented asymmetry", func() {
			// Cursor speaks matchers but contributes no host event names in
			// Ф1: the matcher is meaningful, the reverse mapping is not.
			So(MatcherEvent(FormatCursor, EventPreTool), ShouldBeTrue)

			_, ok := HostEvent(FormatCursor, EventPreTool)
			So(ok, ShouldBeFalse)
		})

		Convey("Then an event no dialect speaks is not canonical", func() {
			So(ValidEvent("nonsense"), ShouldBeFalse)
		})
	})
}
