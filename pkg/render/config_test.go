package render_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/render"
)

// canonicalDigest returns the documented canonical digest of a config value:
// the sha256 of its json.Marshal form.
func canonicalDigest(t *testing.T, value any) digest.Hash {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}

	return digest.Bytes(data)
}

func TestEditJSONCCreate(t *testing.T) {
	Convey("Given an empty object", t, func() {
		out, changes, err := render.EditJSONC([]byte(`{}`), []render.Edit{
			{Path: "hooks", Value: map[string]any{"PreToolUse": []any{}}},
		}, nil)

		Convey("When the key is created", func() {
			Convey("Then the document carries it and the change is reported", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqualJSON, `{"hooks":{"PreToolUse":[]}}`)

				So(changes, ShouldHaveLength, 1)
				So(changes[0].Path, ShouldEqual, "hooks")
				So(changes[0].Existed, ShouldBeFalse)
				So(changes[0].Previous, ShouldBeNil)
				So(changes[0].Digest.Valid(), ShouldBeTrue)
			})
		})
	})

	Convey("Given a missing nested path", t, func() {
		out, changes, err := render.EditJSONC([]byte(`{}`), []render.Edit{
			{Path: "mcpServers.acme", Value: map[string]any{"type": "stdio", "command": "x"}},
		}, nil)

		Convey("When the key is created", func() {
			Convey("Then the intermediate objects are created", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqualJSON, `{"mcpServers":{"acme":{"command":"x","type":"stdio"}}}`)
				So(changes[0].Existed, ShouldBeFalse)
			})
		})
	})

	Convey("Given a nil value without delete", t, func() {
		Convey("When the edit runs", func() {
			_, _, err := render.EditJSONC([]byte(`{}`), []render.Edit{{Path: "a"}}, nil)

			Convey("Then it is refused", func() {
				So(err, ShouldBeError)
			})
		})
	})

	Convey("Given an empty file and no edits", t, func() {
		Convey("When the edit runs", func() {
			out, changes, err := render.EditJSONC(nil, nil, nil)

			Convey("Then nothing changes", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldBeEmpty)
				So(out, ShouldBeNil)
			})
		})
	})

	Convey("Given a malformed document", t, func() {
		Convey("When the edit runs", func() {
			_, _, err := render.EditJSONC([]byte(`{nope`), nil, nil)

			target, ok := errors.AsType[*render.ConfigParseError](err)

			Convey("Then a ConfigParseError is reported", func() {
				So(ok, ShouldBeTrue)
				So(target.Cause, ShouldNotBeNil)
			})
		})
	})

	Convey("Given a document whose root is not an object", t, func() {
		Convey("When the edit runs", func() {
			_, _, err := render.EditJSONC([]byte(`[]`), []render.Edit{{Path: "a", Value: 1}}, nil)

			_, ok := errors.AsType[*render.ConfigParseError](err)

			Convey("Then it is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestEditJSONCOwnership(t *testing.T) {
	file := []byte(`{
  "model": "old",
  "other": 1
}`)

	Convey("Given a present value with a matching owned hash", t, func() {
		owned := render.Owned{"model": canonicalDigest(t, "old")}

		out, changes, err := render.EditJSONC(file, []render.Edit{{Path: "model", Value: "new"}}, owned)

		Convey("When the value is replaced", func() {
			Convey("Then the foreign key survives and the change carries the previous value", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqualJSON, `{"model":"new","other":1}`)

				So(changes, ShouldHaveLength, 1)
				So(changes[0].Existed, ShouldBeTrue)
				So(changes[0].Previous, ShouldEqual, "old")
				So(changes[0].Digest, ShouldEqual, canonicalDigest(t, "new"))
			})
		})
	})

	Convey("Given a mismatching owned hash", t, func() {
		out, changes, err := render.EditJSONC(file, []render.Edit{{Path: "model", Value: "new"}}, render.Owned{"model": canonicalDigest(t, "someone-else")})

		target, ok := errors.AsType[*render.HandsOffError](err)

		Convey("When the value changed under us", func() {
			Convey("Then the cell is hands-off and nothing is written", func() {
				So(ok, ShouldBeTrue)
				So(target.KeyPath, ShouldEqual, "model")
				So(target.Path, ShouldEqual, "model")
				So(target.Reason, ShouldNotBeEmpty)
				So(out, ShouldBeNil)
				So(changes, ShouldBeEmpty)
			})
		})
	})

	Convey("Given no owned hash for a present value", t, func() {
		_, _, err := render.EditJSONC(file, []render.Edit{{Path: "model", Value: "new"}}, nil)

		_, ok := errors.AsType[*render.HandsOffError](err)

		Convey("When verger never wrote the key", func() {
			Convey("Then it stays hands-off", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a nested hands-off key", t, func() {
		nested := []byte(`{"mcpServers": {"acme": {"command": "user-edited"}}}`)

		_, _, err := render.EditJSONC(nested, []render.Edit{{Path: "mcpServers.acme", Value: "x"}},
			render.Owned{"mcpServers.acme": canonicalDigest(t, "verger-wrote")})

		target, ok := errors.AsType[*render.HandsOffError](err)

		Convey("When the nested value mismatches", func() {
			Convey("Then the error names both the section and the full key path", func() {
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, "mcpServers")
				So(target.KeyPath, ShouldEqual, "mcpServers.acme")
			})
		})
	})

	Convey("Given a delete with a matching hash", t, func() {
		source := []byte(`{"a":1,"b":2}`)
		owned := render.Owned{"a": canonicalDigest(t, float64(1))}

		out, changes, err := render.EditJSONC(source, []render.Edit{{Path: "a", Delete: true}}, owned)

		Convey("When the key is removed", func() {
			Convey("Then the change carries the removed value and its digest", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqualJSON, `{"b":2}`)
				So(changes, ShouldHaveLength, 1)
				So(changes[0].Existed, ShouldBeTrue)
				So(changes[0].Previous, ShouldEqual, float64(1))
				So(changes[0].Digest, ShouldEqual, canonicalDigest(t, float64(1)))
			})
		})
	})

	Convey("Given a delete of an absent key", t, func() {
		out, changes, err := render.EditJSONC([]byte(`{"b":2}`), []render.Edit{{Path: "a", Delete: true}}, nil)

		Convey("When the key is missing", func() {
			Convey("Then nothing changes", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldBeEmpty)
				So(string(out), ShouldEqual, `{"b":2}`)
			})
		})
	})

	Convey("Given an unchanged value", t, func() {
		owned := render.Owned{"a": canonicalDigest(t, float64(1))}

		out, changes, err := render.EditJSONC([]byte(`{"a":1}`), []render.Edit{{Path: "a", Value: float64(1)}}, owned)

		Convey("When the value equals the current one", func() {
			Convey("Then no change is applied and the bytes stay identical", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldBeEmpty)
				So(string(out), ShouldEqual, `{"a":1}`)
			})
		})
	})
}

func TestEditJSONCComments(t *testing.T) {
	Convey("Given a JSONC document with comments and foreign keys", t, func() {
		file := []byte(`{
  // keep me
  "hooks": {"PreToolUse": []},
  "other": 1 // trailing
}
`)
		owned := render.Owned{"hooks": canonicalDigest(t, map[string]any{"PreToolUse": []any{}})}

		out, changes, err := render.EditJSONC(file, []render.Edit{
			{Path: "hooks", Value: map[string]any{"PreToolUse": []any{map[string]any{
				"matcher": "Bash",
				"hooks":   []any{map[string]any{"type": "command", "command": "x.sh"}},
			}}}},
		}, owned)

		Convey("When the key is replaced", func() {
			Convey("Then comments and foreign keys survive (golden)", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldHaveLength, 1)
				So(string(out), ShouldEqual, `{
  // keep me
  "hooks": {"PreToolUse":[{"hooks":[{"command":"x.sh","type":"command"}],"matcher":"Bash"}]},
  "other": 1 // trailing
}
`)
			})
		})
	})

	Convey("Given a document with trailing commas and block comments", t, func() {
		file := []byte("{\n  /* block */\n  \"a\": 1,\n}\n")

		out, _, err := render.EditJSONC(file, []render.Edit{{Path: "b", Value: true}}, nil)

		Convey("When a key is added", func() {
			Convey("Then the existing formatting survives", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldContainSubstring, "/* block */")
				So(string(out), ShouldContainSubstring, `"a": 1,`)
				So(string(out), ShouldContainSubstring, `"b":true`)
			})
		})
	})

	Convey("Given a unicode value", t, func() {
		out, changes, err := render.EditJSONC([]byte(`{}`), []render.Edit{
			{Path: "note", Value: map[string]any{"text": "снег ☃"}},
		}, nil)

		Convey("When it is written", func() {
			Convey("Then it survives and the digest matches the canonical form", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldContainSubstring, "снег ☃")
				So(changes[0].Digest, ShouldEqual, canonicalDigest(t, map[string]any{"text": "снег ☃"}))
			})
		})
	})
}

func TestEditTOML(t *testing.T) {
	Convey("Given an empty document", t, func() {
		out, changes, err := render.EditTOML(nil, []render.Edit{{Path: "model", Value: "gpt"}}, nil)

		Convey("When a scalar is created", func() {
			Convey("Then the file carries the assignment", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "model = \"gpt\"\n")
				So(changes, ShouldHaveLength, 1)
				So(changes[0].Existed, ShouldBeFalse)
				So(changes[0].Digest.Valid(), ShouldBeTrue)
			})
		})
	})

	Convey("Given an empty document", t, func() {
		out, changes, err := render.EditTOML(nil, []render.Edit{
			{Path: "mcp_servers.acme", Value: map[string]any{"command": "npx", "args": []string{"-y", "x"}}},
		}, nil)

		Convey("When a table is created", func() {
			Convey("Then a two-part header is written with sorted inline values", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `[mcp_servers.acme]
args = ["-y", "x"]
command = "npx"
`)
				So(changes[0].Existed, ShouldBeFalse)
			})
		})
	})
}

func TestEditTOMLExisting(t *testing.T) {
	Convey("Given a document with a foreign table and comments", t, func() {
		file := []byte(`# top
model = "gpt"

[mcp_servers.acme]
command = "old"

[other]
keep = true
`)
		owned := render.Owned{"mcp_servers.acme": canonicalDigest(t, map[string]any{"command": "old"})}

		Convey("When the table is replaced", func() {
			out, changes, err := render.EditTOML(file, []render.Edit{
				{Path: "mcp_servers.acme", Value: map[string]any{"command": "new"}},
			}, owned)

			Convey("Then comments and foreign tables survive (golden)", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `# top
model = "gpt"

[mcp_servers.acme]
command = "new"

[other]
keep = true
`)
				So(changes[0].Previous, ShouldResemble, map[string]any{"command": "old"})
				So(changes[0].Digest, ShouldEqual, canonicalDigest(t, map[string]any{"command": "new"}))
			})
		})

		Convey("When the table is deleted", func() {
			out, changes, err := render.EditTOML(file, []render.Edit{{Path: "mcp_servers.acme", Delete: true}}, owned)

			Convey("Then the table and its separator are removed", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `# top
model = "gpt"

[other]
keep = true
`)
				So(changes[0].Previous, ShouldResemble, map[string]any{"command": "old"})
				So(changes[0].Digest, ShouldEqual, canonicalDigest(t, map[string]any{"command": "old"}))
			})
		})

		Convey("When the owned hash mismatches", func() {
			_, _, err := render.EditTOML(file, []render.Edit{{Path: "mcp_servers.acme", Value: map[string]any{"command": "new"}}},
				render.Owned{"mcp_servers.acme": canonicalDigest(t, map[string]any{"command": "user"})})

			target, ok := errors.AsType[*render.HandsOffError](err)

			Convey("Then the cell is hands-off", func() {
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, "mcp_servers")
				So(target.KeyPath, ShouldEqual, "mcp_servers.acme")
			})
		})
	})

	Convey("Given a document with an assignment to replace", t, func() {
		file := []byte("model = \"a\" # keep the comment\n")

		out, changes, err := render.EditTOML(file, []render.Edit{{Path: "model", Value: "b"}},
			render.Owned{"model": canonicalDigest(t, "a")})

		Convey("When the scalar is replaced", func() {
			Convey("Then only the value span changes", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "model = \"b\" # keep the comment\n")
				So(changes[0].Existed, ShouldBeTrue)
			})
		})
	})

	Convey("Given an existing table", t, func() {
		file := []byte("[server]\nhost = \"x\"\n")

		out, changes, err := render.EditTOML(file, []render.Edit{{Path: "server.port", Value: 8080}}, nil)

		Convey("When a nested scalar is added", func() {
			Convey("Then it lands inside the table", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "[server]\nhost = \"x\"\nport = 8080\n")
				So(changes[0].Existed, ShouldBeFalse)
			})
		})
	})

	Convey("Given a dotted assignment", t, func() {
		file := []byte(`mcp_servers.acme = { command = "old" }` + "\n")

		out, _, err := render.EditTOML(file, []render.Edit{{Path: "mcp_servers.acme", Value: map[string]any{"command": "new"}}},
			render.Owned{"mcp_servers.acme": canonicalDigest(t, map[string]any{"command": "old"})})

		Convey("When it is replaced", func() {
			Convey("Then the whole table block is written", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `[mcp_servers.acme]
command = "new"
`)
			})
		})
	})

	Convey("Given a CRLF document", t, func() {
		file := []byte("model = \"a\"\r\n")

		out, _, err := render.EditTOML(file, []render.Edit{{Path: "other", Value: "b"}}, nil)

		Convey("When a key is added", func() {
			Convey("Then the file keeps its line endings", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "model = \"a\"\r\nother = \"b\"\r\n")
			})
		})
	})
}

func TestEditTOMLExistingEdges(t *testing.T) {
	Convey("Given a malformed document", t, func() {
		Convey("When an edit runs", func() {
			_, _, err := render.EditTOML([]byte("model = "), nil, nil)

			target, ok := errors.AsType[*render.ConfigParseError](err)

			Convey("Then a ConfigParseError is reported", func() {
				So(ok, ShouldBeTrue)
				So(target.Cause, ShouldNotBeNil)
			})
		})
	})

	Convey("Given a nil value without delete", t, func() {
		Convey("When an edit runs", func() {
			_, _, err := render.EditTOML([]byte("a = 1\n"), []render.Edit{{Path: "b"}}, nil)

			Convey("Then it is refused", func() {
				So(err, ShouldBeError)
			})
		})
	})

	Convey("Given a unicode value", t, func() {
		out, changes, err := render.EditTOML(nil, []render.Edit{{Path: "note", Value: "снег ☃"}}, nil)

		Convey("When it is written", func() {
			Convey("Then it round-trips through TOML", func() {
				So(err, ShouldBeNil)
				So(strings.Contains(string(out), "снег ☃"), ShouldBeTrue)
				So(changes[0].Digest, ShouldEqual, canonicalDigest(t, "снег ☃"))
			})
		})
	})
}

func TestEditTOMLArrayOfTablesRefused(t *testing.T) {
	doc := []byte("[[items]]\nname = \"a\"\n")

	Convey("Given an array-of-tables document", t, func() {
		Convey("When a key inside the table array is edited", func() {
			owned := render.Owned{"items.name": canonicalDigest(t, "a")}

			out, changes, err := render.EditTOML(doc, []render.Edit{{Path: "items.name", Value: "b"}}, owned)

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then the editor refuses instead of emitting invalid TOML", func() {
				So(ok, ShouldBeTrue)
				So(out, ShouldBeNil)
				So(changes, ShouldBeNil)
			})
		})

		Convey("When a new key inside the table array is written", func() {
			_, _, err := render.EditTOML(doc, []render.Edit{{Path: "items.extra", Value: "x"}}, nil)

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then it is refused too", func() {
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the array itself is replaced with a matching owned hash", func() {
			current := []any{map[string]any{"name": "a"}}
			owned := render.Owned{"items": canonicalDigest(t, current)}

			out, changes, err := render.EditTOML(doc, []render.Edit{{Path: "items", Value: map[string]any{"b": "c"}}}, owned)

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then the replacement is refused instead of emitting a duplicate table", func() {
				So(ok, ShouldBeTrue)
				So(out, ShouldBeNil)
				So(changes, ShouldBeNil)
			})
		})

		Convey("When an unrelated top-level key is edited", func() {
			out, _, err := render.EditTOML(doc, []render.Edit{{Path: "other", Value: "x"}}, nil)

			Convey("Then the edit still lands", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldContainSubstring, "other")
			})
		})
	})
}

func TestEditJSONCDuplicateKeysRefused(t *testing.T) {
	Convey("Given a document with a duplicated top-level key", t, func() {
		doc := []byte(`{"hooks":{"a":1},"hooks":{"b":2}}`)

		Convey("When the key is edited", func() {
			out, changes, err := render.EditJSONC(doc, []render.Edit{{Path: "hooks", Value: map[string]any{"c": 3}}}, nil)

			_, ok := errors.AsType[*render.ConfigParseError](err)

			Convey("Then the ambiguous document is refused fail-closed", func() {
				So(ok, ShouldBeTrue)
				So(out, ShouldBeNil)
				So(changes, ShouldBeNil)
			})
		})
	})

	Convey("Given a document with a duplicated nested key", t, func() {
		Convey("When the nested key is edited", func() {
			_, _, err := render.EditJSONC([]byte(`{"a":{"k":1,"k":2}}`), []render.Edit{{Path: "a.k", Value: 3}}, nil)

			_, ok := errors.AsType[*render.ConfigParseError](err)

			Convey("Then it is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a document without duplicates", t, func() {
		Convey("When an owned key is edited", func() {
			owned := render.Owned{"hooks": canonicalDigest(t, map[string]any{})}

			out, _, err := render.EditJSONC([]byte(`{"hooks":{}}`), []render.Edit{{Path: "hooks", Value: map[string]any{"c": 3}}}, owned)

			Convey("Then the edit still lands", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqualJSON, `{"hooks":{"c":3}}`)
			})
		})
	})
}

func TestEditTOMLSurgical(t *testing.T) {
	Convey("Given a Gemini command document with foreign keys and comments", t, func() {
		file := []byte(`# my command
description = "Old"
prompt = """
Old prompt
"""

# extra
custom = 1
`)
		owned := render.Owned{
			"description": canonicalDigest(t, "Old"),
			"prompt":      canonicalDigest(t, "Old prompt\n"),
		}

		out, changes, err := render.EditTOML(file, []render.Edit{
			{Path: "description", Value: "New"},
			{Path: "prompt", Value: "New\n"},
		}, owned)

		Convey("When the managed keys are surgically rewritten", func() {
			Convey("Then foreign keys and comments survive (golden)", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `# my command
description = "New"
prompt = """
New
"""

# extra
custom = 1
`)
				So(changes, ShouldHaveLength, 2)
			})
		})
	})

	Convey("Given an edit applied twice", t, func() {
		first, changes, err := render.EditTOML(nil, []render.Edit{{Path: "model", Value: "gpt"}}, nil)
		So(err, ShouldBeNil)
		So(changes, ShouldHaveLength, 1)

		Convey("When the second run uses the reported digest", func() {
			second, secondChanges, secondErr := render.EditTOML(first, []render.Edit{{Path: "model", Value: "gpt"}},
				render.Owned{"model": changes[0].Digest})

			Convey("Then it is a no-op", func() {
				So(secondErr, ShouldBeNil)
				So(secondChanges, ShouldBeEmpty)
				So(string(second), ShouldEqual, string(first))
			})
		})
	})
}

func TestEditJSONCBOM(t *testing.T) {
	Convey("Given a BOM-only document", t, func() {
		out, changes, err := render.EditJSONC([]byte("\xEF\xBB\xBF"), []render.Edit{{Path: "model", Value: "gpt"}}, nil)

		Convey("When an edit is applied", func() {
			Convey("Then the BOM is stripped and the edit lands", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldHaveLength, 1)
				So(string(out), ShouldEqualJSON, `{"model":"gpt"}`)
				So(strings.HasPrefix(string(out), "\xEF\xBB\xBF"), ShouldBeFalse)
			})
		})
	})

	Convey("Given a BOM-prefixed document with CRLF endings and a comment", t, func() {
		file := []byte("\xEF\xBB\xBF{\r\n  // keep\r\n  \"model\": \"gpt\"\r\n}\r\n")

		out, changes, err := render.EditJSONC(file, []render.Edit{{Path: "model", Value: "new"}},
			render.Owned{"model": canonicalDigest(t, "gpt")})

		Convey("When the owned key is edited", func() {
			Convey("Then the BOM is gone, the comment and CRLF endings survive", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldHaveLength, 1)
				So(strings.HasPrefix(string(out), "\xEF\xBB\xBF"), ShouldBeFalse)
				So(string(out), ShouldContainSubstring, "// keep")
				So(string(out), ShouldContainSubstring, "\r\n")
				So(string(out), ShouldContainSubstring, `"new"`)

				Convey("And a second edit with the reported digest parses the result", func() {
					second, secondChanges, secondErr := render.EditJSONC(out,
						[]render.Edit{{Path: "model", Value: "new"}}, render.Owned{"model": changes[0].Digest})

					So(secondErr, ShouldBeNil)
					So(secondChanges, ShouldBeEmpty)
					So(string(second), ShouldEqual, string(out))
				})
			})
		})
	})

	Convey("Given a BOM-prefixed document with no applicable edit", t, func() {
		file := []byte("\xEF\xBB\xBF{\"a\":1}")

		out, changes, err := render.EditJSONC(file, nil, nil)

		Convey("Then the original bytes are returned untouched", func() {
			So(err, ShouldBeNil)
			So(changes, ShouldBeEmpty)
			So(out, ShouldResemble, file)
		})
	})

	Convey("Given a BOM-prefixed document with real corruption", t, func() {
		out, changes, err := render.EditJSONC([]byte("\xEF\xBB\xBF{\"a\":"), []render.Edit{{Path: "a", Value: 1}}, nil)

		Convey("Then it fails closed with a *ConfigParseError", func() {
			_, ok := errors.AsType[*render.ConfigParseError](err)

			So(ok, ShouldBeTrue)
			So(out, ShouldBeNil)
			So(changes, ShouldBeNil)
		})
	})
}

func TestEditTOMLBOM(t *testing.T) {
	Convey("Given a BOM-only document", t, func() {
		out, changes, err := render.EditTOML([]byte("\xEF\xBB\xBF"), []render.Edit{{Path: "model", Value: "gpt"}}, nil)

		Convey("When a scalar is created", func() {
			Convey("Then the BOM is stripped and the assignment lands", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldHaveLength, 1)
				So(string(out), ShouldEqual, "model = \"gpt\"\n")
				So(strings.HasPrefix(string(out), "\xEF\xBB\xBF"), ShouldBeFalse)
			})
		})
	})

	Convey("Given a BOM-prefixed document with CRLF endings and a comment", t, func() {
		file := []byte("\xEF\xBB\xBF# keep\r\nmodel = \"gpt\"\r\n")

		out, changes, err := render.EditTOML(file, []render.Edit{{Path: "model", Value: "new"}},
			render.Owned{"model": canonicalDigest(t, "gpt")})

		Convey("When the owned scalar is edited", func() {
			Convey("Then the BOM is gone, the comment and CRLF endings survive", func() {
				So(err, ShouldBeNil)
				So(changes, ShouldHaveLength, 1)
				So(strings.HasPrefix(string(out), "\xEF\xBB\xBF"), ShouldBeFalse)
				So(string(out), ShouldContainSubstring, "# keep")
				So(string(out), ShouldContainSubstring, "\r\n")

				Convey("And a second edit with the reported digest parses the result", func() {
					second, secondChanges, secondErr := render.EditTOML(out,
						[]render.Edit{{Path: "model", Value: "new"}}, render.Owned{"model": changes[0].Digest})

					So(secondErr, ShouldBeNil)
					So(secondChanges, ShouldBeEmpty)
					So(string(second), ShouldEqual, string(out))
				})
			})
		})
	})

	Convey("Given a BOM-prefixed document with no applicable edit", t, func() {
		file := []byte("\xEF\xBB\xBFmodel = \"gpt\"\n")

		out, changes, err := render.EditTOML(file, nil, nil)

		Convey("Then the original bytes are returned untouched", func() {
			So(err, ShouldBeNil)
			So(changes, ShouldBeEmpty)
			So(out, ShouldResemble, file)
		})
	})

	Convey("Given a BOM-prefixed document with real corruption", t, func() {
		out, changes, err := render.EditTOML([]byte("\xEF\xBB\xBFmodel = "), []render.Edit{{Path: "model", Value: "gpt"}}, nil)

		Convey("Then it fails closed with a *ConfigParseError", func() {
			_, ok := errors.AsType[*render.ConfigParseError](err)

			So(ok, ShouldBeTrue)
			So(out, ShouldBeNil)
			So(changes, ShouldBeNil)
		})
	})
}
