package secret_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

func TestExtractJSONEmptyDocuments(t *testing.T) {
	Convey("Given a table of documents without secrets", t, func() {
		tests := []string{
			`{}`,
			`[]`,
			`null`,
			`{"name":"acme","count":3}`,
			`{"value":""}`,
			`{"ref":"{secret:TOKEN}"}`,
			`{"env":"{env:TOKEN}"}`,
		}

		for _, input := range tests {
			Convey("When extracting from "+input, func() {
				out, changed, err := secret.ExtractJSON([]byte(input), newTestStore(t))

				Convey("Then the document is untouched", func() {
					So(err, ShouldBeNil)
					So(changed, ShouldBeFalse)
					So(string(out), ShouldEqual, input)
				})
			})
		}
	})
}

func TestExtractJSONNested(t *testing.T) {
	Convey("Given a nested MCP document with hinted values", t, func() {
		store := newTestStore(t)

		input := `{"mcpServers":{"acme":{"command":"npx","args":["-y","acme-mcp"],` +
			`"env":{"ACME_API_KEY":"` + genericKey + `","ACME_REGION":"us-east-1"},` +
			`"headers":{"Authorization":"Bearer ` + ghpFixture + `"}}}}`

		out, changed, err := secret.ExtractJSON([]byte(input), store)

		Convey("When it is extracted", func() {
			want := `{"mcpServers":{"acme":{"command":"npx","args":["-y","acme-mcp"],` +
				`"env":{"ACME_API_KEY":"{secret:ACME_API_KEY}","ACME_REGION":"us-east-1"},` +
				`"headers":{"Authorization":"{secret:AUTHORIZATION}"}}}}`

			Convey("Then only secret-like strings become refs", func() {
				So(err, ShouldBeNil)
				So(changed, ShouldBeTrue)
				So(string(out), ShouldEqualJSON, want)

				So(store.Names(), ShouldResemble, []string{"ACME_API_KEY", "AUTHORIZATION"})

				value, ok := store.Get("ACME_API_KEY")
				So(ok, ShouldBeTrue)
				So(value, ShouldEqual, genericKey)

				header, ok := store.Get("AUTHORIZATION")
				So(ok, ShouldBeTrue)
				So(header, ShouldEqual, "Bearer "+ghpFixture)
			})
		})

		Convey("When it is extracted twice", func() {
			once, changed, err := secret.ExtractJSON([]byte(input), store)
			So(err, ShouldBeNil)

			again, changedAgain, err := secret.ExtractJSON(once, store)
			So(err, ShouldBeNil)

			Convey("Then the second pass is a no-op", func() {
				So(changed, ShouldBeTrue)
				So(changedAgain, ShouldBeFalse)
				So(string(again), ShouldEqual, string(once))
			})
		})
	})
}

func TestExtractJSONEnvPromotion(t *testing.T) {
	Convey("Given a document with a known and an unknown env ref", t, func() {
		store := newTestStore(t)
		store.Set("KNOWN", "value")

		input := `{"env":{"KNOWN":"{env:KNOWN}","UNKNOWN":"{env:UNKNOWN}"}}`

		out, changed, err := secret.ExtractJSON([]byte(input), store)

		Convey("When it is extracted", func() {
			Convey("Then only the known ref is canonicalized", func() {
				So(err, ShouldBeNil)
				So(changed, ShouldBeTrue)
				So(string(out), ShouldEqualJSON, `{"env":{"KNOWN":"{secret:KNOWN}","UNKNOWN":"{env:UNKNOWN}"}}`)
			})
		})
	})
}

func TestExtractJSONEmbeddedEnvRefUntouched(t *testing.T) {
	Convey("Given a header with an embedded env ref", t, func() {
		store := newTestStore(t)

		input := `{"headers":{"Authorization":"Bearer {env:TOKEN}"}}`

		out, changed, err := secret.ExtractJSON([]byte(input), store)

		Convey("When it is extracted", func() {
			Convey("Then the ref is untouched and nothing is stored", func() {
				So(err, ShouldBeNil)
				So(changed, ShouldBeFalse)
				So(string(out), ShouldEqual, input)
				So(store.Len(), ShouldEqual, 0)
			})
		})
	})
}

func TestExtractJSONEscapedStrings(t *testing.T) {
	Convey("Given a document whose secret holds escapes and unicode", t, func() {
		store := newTestStore(t)

		value := "sk-proj-abcdefghijklmnopqrstuvwxyz\"quote\\slash\u2603"

		encoded, err := json.Marshal(map[string]string{"TOKEN": value})
		So(err, ShouldBeNil)

		out, changed, err := secret.ExtractJSON(encoded, store)

		Convey("When it is extracted", func() {
			decoded := map[string]string{}
			unmarshalErr := json.Unmarshal(out, &decoded)

			Convey("Then the ref is stored and the value round-trips through escaping", func() {
				So(err, ShouldBeNil)
				So(changed, ShouldBeTrue)
				So(unmarshalErr, ShouldBeNil)
				So(decoded["TOKEN"], ShouldEqual, secret.Ref("TOKEN"))

				stored, ok := store.Get("TOKEN")
				So(ok, ShouldBeTrue)
				So(stored, ShouldEqual, value)
			})
		})
	})
}

func TestExtractJSONNonStringsUntouched(t *testing.T) {
	Convey("Given a document with non-string and numeric-keyed values", t, func() {
		store := newTestStore(t)

		input := `{"123":"plain","n":1.5,"b":false,"z":null,"arr":[],"obj":{},"nested":{"deep":{"TOKEN":"` + genericKey + `"}}}`

		out, changed, err := secret.ExtractJSON([]byte(input), store)

		Convey("When it is extracted", func() {
			want := `{"123":"plain","n":1.5,"b":false,"z":null,"arr":[],"obj":{},"nested":{"deep":{"TOKEN":"{secret:TOKEN}"}}}`

			Convey("Then only the secret-like string changed", func() {
				So(err, ShouldBeNil)
				So(changed, ShouldBeTrue)
				So(string(out), ShouldEqualJSON, want)
			})
		})
	})
}

func TestExtractJSONInvalidDocument(t *testing.T) {
	Convey("Given an invalid JSON document", t, func() {
		Convey("When it is extracted", func() {
			_, _, err := secret.ExtractJSON([]byte(`{"TOKEN": "`+fixtureValue+`"`), newTestStore(t))

			Convey("Then the decode error is reported without the value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldNotContainSubstring, fixtureValue)
			})
		})
	})
}

func TestResolveJSONLiteral(t *testing.T) {
	Convey("Given a document holding whole-string refs", t, func() {
		store := newTestStore(t)
		store.Set("TOKEN", "the-value")

		input := `{"Authorization":"{secret:TOKEN}","note":"x {secret:TOKEN} y","plain":"nothing"}`

		out, missing, err := secret.ResolveJSON([]byte(input), store, secret.ModeLiteral)

		Convey("When it is resolved in literal mode", func() {
			Convey("Then only the whole-string ref expands", func() {
				So(err, ShouldBeNil)
				So(missing, ShouldBeEmpty)
				So(string(out), ShouldEqualJSON, `{"Authorization":"the-value","note":"x {secret:TOKEN} y","plain":"nothing"}`)
			})
		})
	})
}

func TestResolveJSONEnvMode(t *testing.T) {
	Convey("Given a document holding a ref", t, func() {
		store := newTestStore(t)
		store.Set("TOKEN", "the-value")

		input := `{"headers":{"Authorization":"{secret:TOKEN}"},"list":["{secret:TOKEN}"]}`

		out, missing, err := secret.ResolveJSON([]byte(input), store, secret.ModeEnv)

		Convey("When it is resolved in env mode", func() {
			Convey("Then the env ref is rendered, never the literal", func() {
				So(err, ShouldBeNil)
				So(missing, ShouldBeEmpty)
				So(string(out), ShouldEqualJSON, `{"headers":{"Authorization":"{env:TOKEN}"},"list":["{env:TOKEN}"]}`)
				So(string(out), ShouldNotContainSubstring, "the-value")
			})
		})
	})
}

func TestResolveJSONMissingRedacted(t *testing.T) {
	Convey("Given a document holding unknown refs", t, func() {
		store := newTestStore(t)

		input := `{"b":"{secret:ALSO_MISSING}","a":"{secret:NOPE}","plain":"x"}`

		out, missing, err := secret.ResolveJSON([]byte(input), store, secret.ModeLiteral)

		Convey("When it is resolved", func() {
			Convey("Then missing names are reported sorted and render [redacted]", func() {
				So(err, ShouldBeNil)
				So(missing, ShouldResemble, []string{"ALSO_MISSING", "NOPE"})
				So(string(out), ShouldEqualJSON, `{"b":"[redacted]","a":"[redacted]","plain":"x"}`)
				So(string(out), ShouldNotContainSubstring, `"a":""`)
			})
		})
	})
}

func TestResolveJSONNoRefsUnchanged(t *testing.T) {
	Convey("Given a document without refs", t, func() {
		Convey("When it is resolved", func() {
			input := `{"a":"` + fixtureValue + `","b":[1,2]}`

			out, missing, err := secret.ResolveJSON([]byte(input), newTestStore(t), secret.ModeLiteral)

			Convey("Then it is returned byte-identically", func() {
				So(err, ShouldBeNil)
				So(missing, ShouldBeEmpty)
				So(string(out), ShouldEqual, input)
			})
		})
	})
}

func TestResolveJSONEscapedRef(t *testing.T) {
	Convey("Given a document whose ref arrives as a unicode escape", t, func() {
		store := newTestStore(t)
		store.Set("TOKEN", "the-value")

		input := `{"a":"\u007bsecret:TOKEN\u007d"}`

		out, missing, err := secret.ResolveJSON([]byte(input), store, secret.ModeLiteral)

		Convey("When it is resolved", func() {
			Convey("Then the decoded ref expands", func() {
				So(err, ShouldBeNil)
				So(missing, ShouldBeEmpty)
				So(string(out), ShouldEqualJSON, `{"a":"the-value"}`)
			})
		})
	})
}

func TestResolveJSONEmptyDocument(t *testing.T) {
	Convey("Given empty documents", t, func() {
		for _, input := range []string{`{}`, `[]`, `null`} {
			Convey("When resolving "+input, func() {
				out, missing, err := secret.ResolveJSON([]byte(input), newTestStore(t), secret.ModeLiteral)

				Convey("Then nothing changes", func() {
					So(err, ShouldBeNil)
					So(missing, ShouldBeEmpty)
					So(string(out), ShouldEqual, input)
				})
			})
		}
	})
}

func TestResolveJSONInvalidDocument(t *testing.T) {
	Convey("Given an invalid JSON document", t, func() {
		Convey("When it is resolved", func() {
			_, _, err := secret.ResolveJSON([]byte(`{"TOKEN": "`+fixtureValue+`"`), newTestStore(t), secret.ModeLiteral)

			Convey("Then the decode error is reported without the value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldNotContainSubstring, fixtureValue)
			})
		})
	})
}

func TestRefsJSON(t *testing.T) {
	Convey("Given a nested document with several refs", t, func() {
		input := `{"a":"{secret:Z}","b":["{secret:A}","{secret:Z}","x"],"c":{"d":"{env:E}"},"e":"{secret:lowercase}"}`

		Convey("When refs are listed", func() {
			refs, err := secret.RefsJSON([]byte(input))

			Convey("Then they are sorted, deduplicated and true refs only", func() {
				So(err, ShouldBeNil)
				So(refs, ShouldResemble, []string{"A", "Z", "lowercase"})
			})
		})
	})

	Convey("Given a document without refs", t, func() {
		Convey("When refs are listed", func() {
			refs, err := secret.RefsJSON([]byte(`{"a":"` + fixtureValue + `"}`))

			Convey("Then the list is empty", func() {
				So(err, ShouldBeNil)
				So(refs, ShouldBeEmpty)
			})
		})
	})

	Convey("Given an invalid JSON document", t, func() {
		Convey("When refs are listed", func() {
			_, err := secret.RefsJSON([]byte(`{"a":`))

			Convey("Then the decode error is reported", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestJSONFixtureFile(t *testing.T) {
	Convey("Given the JSON fixture file", t, func() {
		data, err := os.ReadFile(filepath.Join("testdata", "json_fixture.json")) //nolint:gosec // G304: test reads its own fixture
		So(err, ShouldBeNil)

		Convey("When it is extracted and resolved back", func() {
			store := newTestStore(t)

			before, refsErr := secret.RefsJSON(data)
			So(refsErr, ShouldBeNil)
			So(before, ShouldBeEmpty)

			extracted, changed, extractErr := secret.ExtractJSON(data, store)
			So(extractErr, ShouldBeNil)
			So(changed, ShouldBeTrue)

			refs, refsErr := secret.RefsJSON(extracted)
			So(refsErr, ShouldBeNil)

			So(store.Save(), ShouldBeNil)

			reloaded, loadErr := secret.Load(store.Path())
			So(loadErr, ShouldBeNil)

			resolved, missing, resolveErr := secret.ResolveJSON(extracted, reloaded, secret.ModeLiteral)

			Convey("Then the literal values never stay in the extracted document and the round-trip restores it", func() {
				So(refs, ShouldResemble, []string{"ACME_API_KEY", "AUTHORIZATION"})
				So(string(extracted), ShouldNotContainSubstring, genericKey)
				So(string(extracted), ShouldNotContainSubstring, ghpFixture)

				So(resolveErr, ShouldBeNil)
				So(missing, ShouldBeEmpty)
				So(string(resolved), ShouldEqualJSON, string(data))
			})
		})
	})
}
