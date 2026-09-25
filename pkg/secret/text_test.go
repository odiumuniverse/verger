package secret_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

// Synthetic scanner fixtures; the values are public and meaningless.
const (
	ghoToken   = "gho_abcdefghijklmnopqrstuvwxyz012345"
	patToken   = "github_pat_11ABCDEFG0123456789_abcdefgh" //nolint:gosec // G101: synthetic token for scanner tests
	glpatToken = "glpat-abcdefghijklmnop1234"
	slackToken = "xoxb-123456789012-abcdefghijklmnop"
	slackUser  = "xoxp-123456789012-abcdefghijklmnop"
	awsKey     = "AKIAIOSFODNN7EXAMPLE"            //nolint:gosec // G101: synthetic token for scanner tests
	googleTok  = "ya29.a0AfH6SMBx1234567890abcdef" //nolint:gosec // G101: synthetic token for scanner tests
	skToken    = "sk-proj-abcdefghijklmnopqrstuvwxyz"
)

// valuesOf returns the hit values, nil for no hits.
func valuesOf(hits []secret.TextHit) []string {
	if len(hits) == 0 {
		return nil
	}

	out := make([]string, 0, len(hits))

	for _, hit := range hits {
		out = append(out, hit.Value)
	}

	return out
}

func TestScanTextValueHints(t *testing.T) {
	Convey("Given a table of texts with value hints", t, func() {
		tests := []struct {
			name string
			text string
			want []string
		}{
			{name: "bearer", text: "Authorization: Bearer " + ghpFixture, want: []string{ghpFixture}},
			{name: "token prefix", text: "token " + genericKey, want: []string{genericKey}},
			{name: "basic", text: "basic dXNlcjpwYXNzd29yZA==", want: []string{"dXNlcjpwYXNzd29yZA=="}},
			{name: "github", text: "use " + ghpFixture + " now", want: []string{ghpFixture}},
			{name: "github oauth", text: ghoToken, want: []string{ghoToken}},
			{name: "github pat", text: patToken, want: []string{patToken}},
			{name: "gitlab", text: glpatToken, want: []string{glpatToken}},
			{name: "slack bot", text: slackToken, want: []string{slackToken}},
			{name: "slack user", text: slackUser, want: []string{slackUser}},
			{name: "aws", text: awsKey, want: []string{awsKey}},
			{name: "google", text: "token " + googleTok, want: []string{googleTok}},
			{name: "openai", text: skToken, want: []string{skToken}},
			{name: "short token ignored", text: "ghp_short"},
			{name: "mid-word prefix ignored", text: "task-" + strings.Repeat("a", 20)},
			{name: "bare hex ignored", text: "commit deadbeefcafebabe1234567890abcdef"},
		}

		for _, tt := range tests {
			Convey("When scanning "+tt.name, func() {
				Convey("Then the hits match", func() {
					So(valuesOf(secret.ScanText([]byte(tt.text))), ShouldResemble, tt.want)
				})
			})
		}
	})
}

func TestScanTextKeyHints(t *testing.T) {
	Convey("Given a table of key-hint texts", t, func() {
		tests := []struct {
			name  string
			text  string
			value string
		}{
			{name: "equals", text: "api_key=" + genericKey, value: genericKey},
			{name: "colon", text: "api_key: " + genericKey, value: genericKey},
			{name: "dashed key", text: "api-key: " + genericKey, value: genericKey},
			{name: "uppercase", text: "PASSWORD: " + genericKey, value: genericKey},
			{name: "export", text: "export GITHUB_TOKEN=" + genericKey, value: genericKey},
			{name: "quoted", text: `secret = "` + genericKey + `"`, value: genericKey},
			{name: "query string", text: "curl 'https://x.test/?access_token=" + genericKey + "'", value: genericKey},
			{name: "comment", text: "# api_key: " + genericKey, value: genericKey},
		}

		for _, tt := range tests {
			Convey("When scanning "+tt.name, func() {
				hits := secret.ScanText([]byte(tt.text))

				Convey("Then one named hit is found", func() {
					So(hits, ShouldHaveLength, 1)
					So(hits[0].Value, ShouldEqual, tt.value)
					So(hits[0].Name, ShouldNotBeEmpty)
				})
			})
		}
	})
}

func TestScanTextExactSpans(t *testing.T) {
	Convey("Given a table of short texts with hand-computed spans", t, func() {
		tests := []struct {
			name string
			text string
			want secret.TextHit
		}{
			{
				name: "bare key equals value",
				text: "api_key=" + genericKey,
				want: secret.TextHit{Start: 8, End: 24, Name: "API_KEY", Value: genericKey},
			},
			{
				name: "bearer header",
				text: "Authorization: Bearer " + ghpFixture,
				want: secret.TextHit{Start: 22, End: 58, Name: "", Value: ghpFixture},
			},
			{
				name: "quoted key",
				text: `secret = "` + genericKey + `"`,
				want: secret.TextHit{Start: 10, End: 26, Name: "SECRET", Value: genericKey},
			},
			{
				name: "colon key",
				text: "password: " + genericKey,
				want: secret.TextHit{Start: 10, End: 26, Name: "PASSWORD", Value: genericKey},
			},
		}

		for _, tt := range tests {
			Convey("When scanning "+tt.name, func() {
				data := []byte(tt.text)
				hits := secret.ScanText(data)

				Convey("Then the single hit has the exact span", func() {
					So(hits, ShouldResemble, []secret.TextHit{tt.want})
					So(string(data[hits[0].Start:hits[0].End]), ShouldEqual, tt.want.Value)
				})
			})
		}
	})
}

func TestScanTextPEM(t *testing.T) {
	Convey("Given a text holding a PEM block", t, func() {
		pem := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("b3BlbnNzaC1rZXkK\n", 3) + "-----END OPENSSH PRIVATE KEY-----"

		hits := secret.ScanText([]byte("note:\n" + pem + "\nrest"))

		Convey("When scanned", func() {
			Convey("Then the whole PEM block is one hit", func() {
				So(hits, ShouldHaveLength, 1)
				So(hits[0].Value, ShouldEqual, pem)
			})
		})
	})
}

func TestScanTextOrdinaryText(t *testing.T) {
	Convey("Given a note with dates, a commit trailer and a session id", t, func() {
		text := []byte(`---
name: db-notes
description: schema notes
---
Audit columns: authorizedBy = "user-42", authorizedAt = "2026-01-01".
Commit trailer: Co-Authored-By: Someone <x@example.com>
Session field: originSessionId = "9f1c2d3e-aaaa-bbbb-cccc-ddddeeeeffff"
`)

		Convey("When it is scanned", func() {
			Convey("Then nothing is treated as a secret", func() {
				So(secret.ScanText(text), ShouldBeEmpty)
			})
		})
	})
}

func TestScanTextNegatives(t *testing.T) {
	Convey("Given a table of texts that must not yield hits", t, func() {
		tests := []struct {
			name string
			text string
		}{
			{name: "secret ref", text: "api_key: {secret:API_KEY}"},
			{name: "env ref", text: "api_key: {env:API_KEY}"},
			{name: "embedded env ref", text: "token: before {env:TOKEN} after"},
			{name: "redacted", text: "api_key: [redacted]"},
			{name: "shell variable", text: "token: ${GITHUB_TOKEN}"},
			{name: "short value", text: "api_key: short"},
			{name: "pure number", text: "api_key: 123456789"},
			{name: "url host", text: "token: api.example.com"},
			{name: "url", text: "token: https://example.com/path"},
			{name: "unterminated ref", text: "api_key: {secret:API_KEY"},
			{name: "empty value", text: "api_key: "},
			{name: "plain text", text: "just a sentence about tokens and keys"},
		}

		for _, tt := range tests {
			Convey("When scanning "+tt.name, func() {
				Convey("Then nothing is found", func() {
					So(secret.ScanText([]byte(tt.text)), ShouldBeEmpty)
				})
			})
		}
	})
}

func TestScanTextUnicodeOffsets(t *testing.T) {
	Convey("Given a table of texts with non-ASCII characters before the secret", t, func() {
		tests := []struct {
			name string
			text string
			want []string
		}{
			{name: "dotted capital I before bearer", text: "İ Bearer " + ghpFixture, want: []string{ghpFixture}},
			{name: "sharp s before token", text: "ẞ token " + genericKey, want: []string{genericKey}},
			{name: "unicode body", text: "İstanbul: api_key: " + genericKey, want: []string{genericKey}},
			{name: "non ascii before pem", text: "İ\n-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----", want: []string{"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"}},
		}

		for _, tt := range tests {
			Convey("When scanning "+tt.name, func() {
				data := []byte(tt.text)
				hits := secret.ScanText(data)

				Convey("Then offsets slice the exact value bytes", func() {
					So(valuesOf(hits), ShouldResemble, tt.want)

					for i, hit := range hits {
						So(string(data[hit.Start:hit.End]), ShouldEqual, tt.want[i])
						So(hit.Value, ShouldEqual, tt.want[i])
					}
				})
			})
		}
	})
}

func TestScanTextLaterKeyPair(t *testing.T) {
	Convey("Given a JSON body whose first key has an empty value", t, func() {
		text := []byte(`{"token":"","password":"hunter2secret"}`)

		hits := secret.ScanText(text)

		Convey("When scanned", func() {
			Convey("Then the later key pair is found", func() {
				So(hits, ShouldHaveLength, 1)
				So(hits[0].Value, ShouldEqual, "hunter2secret")
				So(hits[0].Name, ShouldEqual, "PASSWORD")
			})
		})
	})
}

func TestScanTextFixtureFile(t *testing.T) {
	Convey("Given the scanner fixture file", t, func() {
		data, err := os.ReadFile(filepath.Join("testdata", "text_fixture.txt")) //nolint:gosec // G304: test reads its own fixture
		So(err, ShouldBeNil)

		pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXkK\n-----END OPENSSH PRIVATE KEY-----"

		hits := secret.ScanText(data)

		Convey("When it is scanned", func() {
			Convey("Then the api_key, sk- token and PEM block are the only hits", func() {
				So(hits, ShouldHaveLength, 3)
				So(hits[0].Name, ShouldEqual, "API_KEY")
				So(hits[0].Value, ShouldEqual, genericKey)
				So(hits[1].Name, ShouldBeEmpty)
				So(hits[1].Value, ShouldEqual, skToken)
				So(hits[2].Name, ShouldBeEmpty)
				So(hits[2].Value, ShouldEqual, pem)

				for _, hit := range hits {
					So(string(data[hit.Start:hit.End]), ShouldEqual, hit.Value)
				}
			})
		})

		Convey("When it is extracted and resolved back", func() {
			store := newTestStore(t)

			extracted, names, changed, extractErr := secret.ExtractText(data, store)
			So(extractErr, ShouldBeNil)
			So(changed, ShouldBeTrue)
			So(names, ShouldHaveLength, 3)

			So(string(extracted), ShouldNotContainSubstring, genericKey)
			So(string(extracted), ShouldNotContainSubstring, skToken)
			So(string(extracted), ShouldNotContainSubstring, "b3BlbnNzaC1rZXkK")
			So(string(extracted), ShouldContainSubstring, "https://example.com/docs")
			So(string(extracted), ShouldContainSubstring, "prose word")

			So(store.Save(), ShouldBeNil)

			reloaded, loadErr := secret.Load(store.Path())
			So(loadErr, ShouldBeNil)

			resolved, missing := secret.ResolveText(extracted, reloaded)

			Convey("Then the file round-trips byte-exactly", func() {
				So(missing, ShouldBeEmpty)
				So(string(resolved), ShouldEqual, string(data))
			})
		})
	})
}

func TestExtractText(t *testing.T) {
	Convey("Given a store and a text with a secret", t, func() {
		store := newTestStore(t)

		text := []byte("line one\napi_key: " + genericKey + "\nline three\n")

		out, names, changed, err := secret.ExtractText(text, store)
		So(err, ShouldBeNil)

		Convey("When it is extracted and then extracted again", func() {
			again, namesAgain, changedAgain, err := secret.ExtractText(out, store)
			So(err, ShouldBeNil)

			Convey("Then the value is replaced once and re-extraction is a noop", func() {
				So(changed, ShouldBeTrue)
				So(names, ShouldHaveLength, 1)
				So(string(out), ShouldContainSubstring, "{secret:"+names[0]+"}")
				So(string(out), ShouldNotContainSubstring, genericKey)
				So(store.Changed(), ShouldBeTrue)
				So(store.Has(names[0]), ShouldBeTrue)

				So(changedAgain, ShouldBeFalse)
				So(namesAgain, ShouldBeEmpty)
				So(string(again), ShouldEqual, string(out))
			})
		})

		Convey("When the same value is extracted with the same key elsewhere", func() {
			store2 := newTestStore(t)

			_, sameName, _, err := secret.ExtractText([]byte("API_KEY="+genericKey), store2)

			Convey("Then the name is reused", func() {
				So(err, ShouldBeNil)
				So(sameName[0], ShouldEqual, names[0])
			})
		})
	})
}

func TestExtractTextEmptyInput(t *testing.T) {
	Convey("Given a text without secrets", t, func() {
		store := newTestStore(t)

		Convey("When it is extracted", func() {
			out, names, changed, err := secret.ExtractText(nil, store)

			Convey("Then nothing changes", func() {
				So(err, ShouldBeNil)
				So(out, ShouldBeNil)
				So(names, ShouldBeEmpty)
				So(changed, ShouldBeFalse)
				So(store.Changed(), ShouldBeFalse)
			})
		})
	})
}

func TestExtractTextNames(t *testing.T) {
	Convey("Given a store", t, func() {
		store := newTestStore(t)

		Convey("When a keyed value is extracted", func() {
			_, names, _, err := secret.ExtractText([]byte("GITHUB_TOKEN="+genericKey), store)

			Convey("Then the key names the secret", func() {
				So(err, ShouldBeNil)
				So(names, ShouldResemble, []string{"GITHUB_TOKEN"})
			})
		})

		Convey("When a bearer value is extracted", func() {
			_, names, _, err := secret.ExtractText([]byte("Bearer "+ghpFixture), store)

			Convey("Then it is named SECRET", func() {
				So(err, ShouldBeNil)
				So(names, ShouldResemble, []string{"SECRET"})
			})
		})

		Convey("When two different values collide on the same key", func() {
			store2 := newTestStore(t)

			_, names, _, err := secret.ExtractText([]byte("api_key: "+genericKey), store2)
			So(err, ShouldBeNil)
			So(names, ShouldHaveLength, 1)

			_, names, _, err = secret.ExtractText([]byte("api_key: "+strings.Repeat("z", 20)), store2)

			Convey("Then the second gets a fingerprint suffix", func() {
				So(err, ShouldBeNil)
				So(names, ShouldHaveLength, 1)
				So(names[0], ShouldNotEqual, "API_KEY")
			})
		})

		Convey("When an oversized value is extracted", func() {
			oversized := strings.Repeat("Z", 1<<16)
			text := []byte("api_key: " + oversized + "\n")

			out, names, changed, err := secret.ExtractText(text, store)
			So(err, ShouldBeNil)

			Convey("Then the whole value becomes one ref", func() {
				So(changed, ShouldBeTrue)
				So(names, ShouldHaveLength, 1)
				So(string(out), ShouldEqual, "api_key: "+secret.Ref(names[0])+"\n")

				value, ok := store.Get(names[0])
				So(ok, ShouldBeTrue)
				So(value, ShouldEqual, oversized)
			})
		})
	})
}

func TestResolveText(t *testing.T) {
	Convey("Given a store with a value", t, func() {
		store := newTestStore(t)

		store.Set("API_KEY", genericKey)

		Convey("When a secret ref is resolved", func() {
			out, missing := secret.ResolveText([]byte("key: {secret:API_KEY}"), store)

			Convey("Then it expands", func() {
				So(missing, ShouldBeEmpty)
				So(string(out), ShouldEqual, "key: "+genericKey)
			})
		})

		Convey("When an unknown ref is resolved", func() {
			out, missing := secret.ResolveText([]byte("key: {secret:NOPE}"), store)

			Convey("Then it reports missing and redacts, never empties", func() {
				So(missing, ShouldResemble, []string{"NOPE"})
				So(string(out), ShouldEqual, "key: [redacted]")
			})
		})

		Convey("When an embedded ref is resolved", func() {
			out, missing := secret.ResolveText([]byte("prefix {secret:API_KEY} suffix"), store)

			Convey("Then the span inside the text expands", func() {
				So(missing, ShouldBeEmpty)
				So(string(out), ShouldEqual, "prefix "+genericKey+" suffix")
			})
		})

		Convey("When an env ref and a bare ref are resolved", func() {
			env := []byte("key: {env:API_KEY}")
			out, missing := secret.ResolveText(env, store)
			So(missing, ShouldBeEmpty)
			So(string(out), ShouldEqual, string(env))

			bare := []byte("key: {secret:no closing")
			out, missing = secret.ResolveText(bare, store)
			So(missing, ShouldBeEmpty)
			So(string(out), ShouldEqual, string(bare))

			empty := []byte("key: {secret:}")
			out, missing = secret.ResolveText(empty, store)
			So(missing, ShouldBeEmpty)
			So(string(out), ShouldEqual, string(empty))
		})

		Convey("When the same missing ref appears twice", func() {
			out, missing := secret.ResolveText([]byte("{secret:NOPE} and {secret:NOPE}"), store)

			Convey("Then the name is reported once and both spans redact", func() {
				So(missing, ShouldResemble, []string{"NOPE"})
				So(string(out), ShouldEqual, "[redacted] and [redacted]")
			})
		})
	})
}

func TestTextRoundTrip(t *testing.T) {
	Convey("Given a text with a token", t, func() {
		store := newTestStore(t)

		original := []byte("token: " + ghpFixture + "\nnote without secrets\n")

		extracted, _, _, err := secret.ExtractText(original, store)
		So(err, ShouldBeNil)

		Convey("When extracted and resolved back", func() {
			resolved, missing := secret.ResolveText(extracted, store)

			Convey("Then the original text is restored", func() {
				So(missing, ShouldBeEmpty)
				So(string(resolved), ShouldEqual, string(original))
			})
		})
	})
}

func TestRefsText(t *testing.T) {
	Convey("Given a text with several ref-like fragments", t, func() {
		text := []byte("a {secret:API_KEY} b {secret:B_TOKEN} c {env:C} d {secret:lowercase} e {secret:1BAD} f {secret:} g {secret:UNCLOSED")

		Convey("When refs are listed", func() {
			Convey("Then only valid secret names are returned, sorted", func() {
				So(secret.RefsText(text), ShouldResemble, []string{"API_KEY", "B_TOKEN", "lowercase"})
			})
		})

		Convey("When the text carries no refs", func() {
			Convey("Then the list is empty", func() {
				So(secret.RefsText([]byte("nothing here")), ShouldBeEmpty)
			})
		})
	})
}

func TestScanTextConcurrently(t *testing.T) {
	Convey("Given the scanner fixture", t, func() {
		data, err := os.ReadFile(filepath.Join("testdata", "text_fixture.txt")) //nolint:gosec // G304: test reads its own fixture
		So(err, ShouldBeNil)

		Convey("When many goroutines scan it", func() {
			const n = 16

			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				bad  []int
				want = len(secret.ScanText(data))
			)

			for range n {
				wg.Go(func() {
					if got := len(secret.ScanText(data)); got != want {
						mu.Lock()
						defer mu.Unlock()

						bad = append(bad, got)
					}
				})
			}

			wg.Wait()

			Convey("Then every scan is deterministic", func() {
				So(bad, ShouldBeEmpty)
				So(want, ShouldEqual, 3)
			})
		})
	})
}
