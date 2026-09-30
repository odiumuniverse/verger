package spec

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// A spec is the one file the user edits by hand. Every command that writes one
// — install, remove, pin, enable, disable, source add — rewrote the whole
// document from a `map[string]any`, and a TOML tree has no comment nodes, so
// every comment in the user's file was destroyed by a command that changed
// one line. Losing a comment is losing the user's own note about why a line is
// there; losing a dozen is losing the file.
//
// The first and cheapest law: a load and a save with nothing changed in
// between must give back the very same bytes. Not an equivalent document —
// the same bytes. If this holds, every writer built on it is safe by
// construction, and a writer that cannot hold it has a bug worth naming.
func TestRoundTripPreservesBytes(t *testing.T) {
	Convey("Given a spec carrying the parts a re-encode drops", t, func() {
		original := `# the canon, kept in step with beadle's
schema = 1

# the review agent, per the incident of 2026-06
[[package]]
id = "caveman" # the only one that earns its keep
channel = "stable"
`
		dir := t.TempDir()
		path := filepath.Join(dir, "verger.toml")
		So(os.WriteFile(path, []byte(original), 0o600), ShouldBeNil)

		Convey("When it is loaded and saved without any change", func() {
			doc, err := ParseFile(path)
			So(err, ShouldBeNil)

			So(doc.Save(path), ShouldBeNil)

			after, err := os.ReadFile(path) //nolint:gosec // G304: the test wrote this path
			So(err, ShouldBeNil)

			Convey("Then the bytes are identical, comments and all", func() {
				So(string(after), ShouldEqual, original)
			})
		})
	})
}

// userSpec is the document a human actually keeps: a comment above every
// table, double quotes where go-toml would print single ones, blank lines
// between sections, a trailing comment on a value, a multi-line array, a
// table verger does not know and a key verger does not know inside a table
// it does. Every byte of it belongs to the user.
const userSpec = `# verger's canon, kept in step with beadle's
schema = 1
top_future = "keep me"

# what to do on a first install
[defaults]
hooks = "ask"
cooldown = "24h"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep
future_field = 3

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
except = [
  "gemini",
  "cursor",
]

[unknown_table]
note = "keep me"
`

// userSpecThree is the same shape with three packages, so the middle one can
// be removed while both neighbours carry a comment of their own.
const userSpecThree = `# canon
schema = 1

# the first one
[[package]]
id = "acme/one"

# the second one
[[package]]
id = "acme/two"
version = "1.0.0"

# the third one
[[package]]
id = "acme/three"
`

func TestRoundTripPreservesTheUsersDocument(t *testing.T) {
	Convey("Given the document a user keeps", t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "verger.toml")
		So(os.WriteFile(path, []byte(userSpec), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)
		So(doc.Save(path), ShouldBeNil)

		Convey("When it is loaded and saved with nothing changed", func() {
			Convey("Then not one byte of it moved", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, userSpec)
			})
		})

		Convey("When the file uses CRLF line endings", func() {
			crlf := filepath.Join(dir, "crlf.toml")
			So(os.WriteFile(crlf, []byte(strings.ReplaceAll(userSpec, "\n", "\r\n")), 0o600), ShouldBeNil)

			crlfDoc, err := ParseFile(crlf)
			So(err, ShouldBeNil)
			So(crlfDoc.Save(crlf), ShouldBeNil)

			Convey("Then the CRLF endings come back untouched", func() {
				So(string(mustReadFile(t, crlf)), ShouldEqual, strings.ReplaceAll(userSpec, "\n", "\r\n"))
			})
		})

		Convey("When the file carries a UTF-8 BOM", func() {
			bom := filepath.Join(dir, "bom.toml")
			So(os.WriteFile(bom, withBOM(userSpec), 0o600), ShouldBeNil)

			bomDoc, err := ParseFile(bom)
			So(err, ShouldBeNil)
			So(bomDoc.Save(bom), ShouldBeNil)

			Convey("Then the BOM and the bytes behind it both survive", func() {
				So(mustReadFile(t, bom), ShouldResemble, withBOM(userSpec))
			})
		})

		Convey("When it is saved twice", func() {
			doc.Packages[0].Version = "9.9.9"
			So(doc.Save(path), ShouldBeNil)
			first := mustReadFile(t, path)

			doc.Packages[0].Version = "8.8.8"
			So(doc.Save(path), ShouldBeNil)

			Convey("Then the second save edits the first result, not the file it was read from", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, strings.Replace(string(first), `9.9.9`, `8.8.8`, 1))
			})
		})
	})
}

// editCase is one writing command's worth of change: what it does to the model,
// and which lines of the document that may touch.
type editCase struct {
	name    string
	source  bool
	apply   func(*Spec)
	added   []string
	removed []string
	want    string
}

// editCases is the matrix of writing commands: every one of them, run against
// the same hand-written document, with the lines each may touch.
//
//nolint:funlen // the matrix of writing commands is one table of data
func editCases() []editCase {
	return []editCase{
		{
			name:    "a pin on a package that has none",
			apply:   func(s *Spec) { s.Packages[0].Version = "1.0.0" },
			added:   []string{`version = "1.0.0"`},
			removed: []string{},
			want: `# verger's canon, kept in step with beadle's
schema = 1
top_future = "keep me"

# what to do on a first install
[defaults]
hooks = "ask"
cooldown = "24h"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep
future_field = 3
version = "1.0.0"

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
except = [
  "gemini",
  "cursor",
]

[unknown_table]
note = "keep me"
`,
		},
		{
			name:    "a value change under a trailing comment",
			apply:   func(s *Spec) { s.Packages[0].Channel = "next" },
			added:   []string{`channel = "next" # earns its keep`},
			removed: []string{`channel = "stable" # earns its keep`},
		},
		{
			name:    "an unpin",
			apply:   func(s *Spec) { s.Packages[1].Version = "" },
			added:   []string{},
			removed: []string{`version = "1.0.0"`},
		},
		{
			name:    "a disable",
			apply:   func(s *Spec) { s.Packages[0].Disabled = true },
			added:   []string{"disabled = true"},
			removed: []string{},
		},
		{
			name:    "a defaults answer",
			apply:   func(s *Spec) { s.Defaults.Hooks = HooksYes },
			added:   []string{`hooks = "yes"`},
			removed: []string{`hooks = "ask"`},
		},
		{
			name:    "a cooldown",
			apply:   func(s *Spec) { s.Defaults.Cooldown = Duration(time.Hour) },
			added:   []string{`cooldown = "1h"`},
			removed: []string{`cooldown = "24h"`},
		},
		{
			name: "an added package",
			apply: func(s *Spec) {
				s.Packages = append(s.Packages, Package{ID: "acme/new"})
			},
			added:   []string{"", "[[package]]", `id = "acme/new"`},
			removed: []string{},
		},
		{
			name: "two packages added at once",
			apply: func(s *Spec) {
				s.Packages = append(s.Packages, Package{ID: "acme/zeta"}, Package{ID: "acme/alpha"})
			},
			added: []string{
				"", "", "[[package]]", `id = "acme/zeta"`, "[[package]]", `id = "acme/alpha"`,
			},
			removed: []string{},
			want: `# verger's canon, kept in step with beadle's
schema = 1
top_future = "keep me"

# what to do on a first install
[defaults]
hooks = "ask"
cooldown = "24h"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep
future_field = 3

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
except = [
  "gemini",
  "cursor",
]

[[package]]
id = "acme/zeta"

[[package]]
id = "acme/alpha"

[unknown_table]
note = "keep me"
`,
		},
		{
			name: "a pin and a new package in one command",
			apply: func(s *Spec) {
				s.Packages[0].Version = "1.0.0"
				s.Packages = append(s.Packages, Package{ID: "acme/new"})
			},
			added:   []string{`version = "1.0.0"`, "", "[[package]]", `id = "acme/new"`},
			removed: []string{},
		},
		{
			name: "an added source",
			apply: func(s *Spec) {
				s.Sources = append(s.Sources, Source{Name: "acme", URL: "https://github.com/acme/x"})
			},
			added:   []string{"", "[[source]]", `name = "acme"`, `url = "https://github.com/acme/x"`},
			removed: []string{},
		},
		{
			name:   "a second source",
			source: true,
			apply: func(s *Spec) {
				s.Sources = append(s.Sources, Source{Name: "second", URL: "https://example.com/x"})
			},
			added:   []string{"", "[[source]]", `name = "second"`, `url = "https://example.com/x"`},
			removed: []string{},
		},
		{
			name:   "a removed source",
			source: true,
			apply: func(s *Spec) {
				s.Sources = nil
			},
			added:   []string{},
			removed: []string{"", "[[source]]", `name = "getverger"`, `url = "https://github.com/vmkteam/getverger"`},
		},
		{
			name:    "a removed first package",
			apply:   func(s *Spec) { s.Packages = s.Packages[1:] },
			added:   []string{},
			removed: []string{"", "# the review agent, per the incident of 2026-06", "[[package]]", `id = "acme/caveman"`, `channel = "stable" # earns its keep`, "future_field = 3"},
		},
		{
			name:    "a removed last package",
			apply:   func(s *Spec) { s.Packages = s.Packages[:1] },
			added:   []string{},
			removed: []string{"", "# a hooks-only package", "[[package]]", `id = "acme/hooks"`, `version = "1.0.0"`, "except = [", `  "gemini",`, `  "cursor",`, "]"},
		},
		{
			name:    "a multi-line value",
			apply:   func(s *Spec) { s.Packages[1].Except = []string{"gemini"} },
			added:   []string{`except = ["gemini"]`},
			removed: []string{"except = [", `  "gemini",`, `  "cursor",`, "]"},
		},
		{
			name: "a host switch",
			apply: func(s *Spec) {
				off := false
				s.Hosts = map[string]HostSettings{"claude": {Runtime: &off}}
			},
			added:   []string{"", "[hosts]", "[hosts.claude]", "runtime = false"},
			removed: []string{},
		},
		{
			name: "a package-level policy",
			apply: func(s *Spec) {
				s.Packages[0].Propagate = &Propagate{Install: ModeAll}
			},
			added:   []string{"", "[package.propagate]", `install = "all"`},
			removed: []string{},
		},
		{
			name: "a policy dropped from a package",
			apply: func(s *Spec) {
				s.Packages[0].Propagate = &Propagate{Install: ModeAll}
				s.Packages[0].Propagate = nil
			},
			added:   []string{},
			removed: []string{},
		},
	}
}

func TestSaveTouchesOnlyWhatItChanged(t *testing.T) {
	cases := editCases()

	Convey("Given a spec the user wrote by hand", t, func() {
		for _, tc := range cases {
			Convey("When "+tc.name+" is written", func() {
				before := userSpec
				if tc.source {
					before = userSpecWithSource
				}

				path := filepath.Join(t.TempDir(), "verger.toml")
				So(os.WriteFile(path, []byte(before), 0o600), ShouldBeNil)

				doc, err := ParseFile(path)
				So(err, ShouldBeNil)

				tc.apply(doc)
				So(doc.Save(path), ShouldBeNil)

				added, removed := lineDiff(before, string(mustReadFile(t, path)))

				Convey("Then exactly the changed lines differ and nothing else", func() {
					So(sortedLines(added), ShouldResemble, sortedLines(tc.added))
					So(sortedLines(removed), ShouldResemble, sortedLines(tc.removed))
				})

				if tc.want != "" {
					Convey("Then the result is the document with those lines in place", func() {
						So(string(mustReadFile(t, path)), ShouldEqual, tc.want)
					})
				}
			})
		}
	})
}

// userSpecWithSource is userSpec with a source already declared, so an added
// one has a place to go and a removed one has neighbours.
const userSpecWithSource = `# canon
schema = 1

[[source]]
name = "getverger"
url = "https://github.com/vmkteam/getverger"

# the review agent
[[package]]
id = "acme/caveman"
`

func TestRemoveKeepsNeighbourComments(t *testing.T) {
	Convey("Given three packages, each introduced by its own comment", t, func() {
		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(userSpecThree), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		doc.Packages = append(doc.Packages[:1], doc.Packages[2:]...)
		So(doc.Save(path), ShouldBeNil)

		Convey("When the middle one is removed", func() {
			Convey("Then it takes its own comment and nothing else", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, `# canon
schema = 1

# the first one
[[package]]
id = "acme/one"

# the third one
[[package]]
id = "acme/three"
`)
			})
		})
	})
}

func TestSaveEditsInlineTablesInPlace(t *testing.T) {
	Convey("Given an inline table in place of a section", t, func() {
		const document = `schema = 1
defaults = { hooks = "ask" }

# the review agent
[[package]]
id = "acme/caveman"
`

		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(document), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		Convey("When one answer inside it changes", func() {
			doc.Defaults.Hooks = HooksYes
			So(doc.Save(path), ShouldBeNil)

			Convey("Then that value is rewritten in place and the rest of the file is not", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, `schema = 1
defaults = {hooks = "yes"}

# the review agent
[[package]]
id = "acme/caveman"
`)
			})
		})
	})

	Convey("Given an inline table nested inside an inline table", t, func() {
		const document = `schema = 1
propagate = { kind = { skill = { install = "ask" } } }

# the review agent
[[package]]
id = "acme/caveman"
`

		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(document), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		Convey("When the innermost answer changes", func() {
			policy := doc.Propagate.Kind["skill"]
			policy.Install = ModeOrigin
			doc.Propagate.Kind["skill"] = policy
			So(doc.Save(path), ShouldBeNil)

			Convey("Then the whole inline value is rewritten and nothing else moves", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, `schema = 1
propagate = {kind = {skill = {install = "origin"}}}

# the review agent
[[package]]
id = "acme/caveman"
`)
			})
		})
	})

	Convey("Given an environment written as an inline table", t, func() {
		const document = `schema = 1

# the review agent
[[package]]
id = "acme/caveman"
env = { FOO = "bar" }
`

		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(document), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		Convey("When a second environment entry arrives", func() {
			doc.Packages[0].Env = map[string]string{"FOO": "bar", "BAZ": "qux"}
			So(doc.Save(path), ShouldBeNil)

			Convey("Then the value is rewritten in place, in the file's own quotes", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, `schema = 1

# the review agent
[[package]]
id = "acme/caveman"
env = {BAZ = "qux", FOO = "bar"}
`)
			})
		})
	})
}

// A change that cannot be spelled as an edit of the bytes a table already has
// is not a reason to re-encode the file. The table itself is re-encoded, in the
// file's own quotes, and every other byte stays where the user left it.
func TestSaveRewritesOnlyTheTableItCouldNotSplice(t *testing.T) {
	Convey("Given an environment the document gives a section of its own", t, func() {
		const document = "schema = 1\n" +
			"\n" +
			"# the review agent\n" +
			"[[package]]\n" +
			"id = \"acme/caveman\"\n" +
			"\n" +
			"[package.env]\n" +
			"FOO = \"bar\"\n"

		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(document), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		Convey("When a second environment entry arrives", func() {
			doc.Packages[0].Env = map[string]string{"FOO": "bar", "BAZ": "qux"}
			So(doc.Save(path), ShouldBeNil)

			Convey("Then that table is re-encoded and the package above it is not", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, strings.Replace(document, "FOO = \"bar\"\n", "BAZ = \"qux\"\nFOO = \"bar\"\n", 1))
			})
		})
	})

	Convey("Given a root table the document writes as a dotted key", t, func() {
		const document = "schema = 1\n" +
			"defaults.hooks = \"ask\"\n" +
			"\n" +
			"# the review agent\n" +
			"[[package]]\n" +
			"id = \"acme/caveman\"\n"

		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(document), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		Convey("When the answer inside it changes", func() {
			doc.Defaults.Hooks = HooksYes
			So(doc.Save(path), ShouldBeNil)

			Convey("Then the document is re-encoded, the root table having no header to re-encode around", func() {
				So(string(mustReadFile(t, path)), ShouldEqual, "schema = 1\n"+
					"\n"+
					"[defaults]\n"+
					"hooks = 'yes'\n"+
					"\n"+
					"[[package]]\n"+
					"id = 'acme/caveman'\n")
			})
		})
	})
}

func TestSaveRefusesBytesThatDoNotParseBack(t *testing.T) {
	Convey("Given a producer of bytes that do not parse into the model", t, func() {
		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(userSpec), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		Convey("When it is asked to write them", func() {
			err := doc.save(path, func() ([]byte, error) {
				return []byte("schema = 1\n[[package]]\nid = \"acme/typo\"\n"), nil
			})

			Convey("Then it refuses and the file on disk is untouched", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "does not parse back")
				So(string(mustReadFile(t, path)), ShouldEqual, userSpec)
			})
		})

		Convey("When the producer returns something that is not a spec at all", func() {
			err := doc.save(path, func() ([]byte, error) {
				return []byte("schema = 1\n[[package]\n"), nil
			})

			Convey("Then it refuses and the file on disk is untouched", func() {
				So(err, ShouldBeError)
				So(string(mustReadFile(t, path)), ShouldEqual, userSpec)
			})
		})
	})
}

func TestSaveFallsBackWhenTheOrderCannotBeKept(t *testing.T) {
	Convey("Given a spec whose packages are reordered in the model", t, func() {
		path := filepath.Join(t.TempDir(), "verger.toml")
		So(os.WriteFile(path, []byte(userSpecThree), 0o600), ShouldBeNil)

		doc, err := ParseFile(path)
		So(err, ShouldBeNil)

		doc.Packages[0], doc.Packages[2] = doc.Packages[2], doc.Packages[0]
		So(doc.Save(path), ShouldBeNil)

		Convey("Then the file is rewritten canonically rather than kept out of order", func() {
			again, err := ParseFile(path)
			So(err, ShouldBeNil)
			So(again.Packages[0].ID, ShouldEqual, "acme/three")
			So(again.Packages[2].ID, ShouldEqual, "acme/one")
		})
	})
}

func FuzzSpecRoundTrip(f *testing.F) {
	for _, seed := range []string{
		userSpec,
		userSpecThree,
		userSpecWithSource,
		"# only a comment\n",
		"schema = 1\n",
		"schema = 1\r\n[defaults]\r\nhooks = \"ask\"\r\n",
		"schema = 1\n[defaults]\nhooks = { a = 1 }\n",
		"schema = 1\n[[package]]\nid = \"a/b\"\nexcept = [\n  \"x\",\n]\n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, document string) {
		parsed, err := Parse([]byte(document))
		if err != nil {
			return
		}

		path := filepath.Join(t.TempDir(), "verger.toml")
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatalf("write spec: %v", err)
		}

		loaded, err := ParseFile(path)
		if err != nil {
			t.Fatalf("parse file: %v", err)
		}

		if err := loaded.Save(path); err != nil {
			t.Fatalf("save: %v", err)
		}

		if got := string(mustReadFile(t, path)); got != document {
			t.Fatalf("round trip changed the document:\n--- got ---\n%s\n--- want ---\n%s", got, document)
		}

		parsed.Packages = append(parsed.Packages, Package{ID: "acme/added"})

		if err := parsed.Save(path); err != nil {
			t.Fatalf("save with an added package: %v", err)
		}

		edited, err := ParseFile(path)
		if err != nil {
			t.Fatalf("parse edited: %v", err)
		}

		if len(edited.Packages) != len(parsed.Packages) {
			t.Fatalf("got %d packages, want %d", len(edited.Packages), len(parsed.Packages))
		}
	})
}

// sortedLines puts a line list in a fixed order, so an expectation can be
// written in the order the document reads and compared without regard to it.
func sortedLines(lines []string) []string {
	return slices.Sorted(slices.Values(lines))
}

// lineDiff reports which lines a reader of the file would see appear and
// disappear: a multiset difference, so a line that merely moved is not
// mistaken for a line that changed.
func lineDiff(before, after string) (added, removed []string) {
	beforeLeft := countLines(before)
	afterLeft := countLines(after)

	for _, line := range splitLines(after) {
		if beforeLeft[line] > 0 {
			beforeLeft[line]--

			continue
		}

		added = append(added, line)
	}

	for _, line := range splitLines(before) {
		if afterLeft[line] > 0 {
			afterLeft[line]--

			continue
		}

		removed = append(removed, line)
	}

	slices.Sort(added)
	slices.Sort(removed)

	if added == nil {
		added = []string{}
	}

	if removed == nil {
		removed = []string{}
	}

	return added, removed
}

func countLines(document string) map[string]int {
	lines := make(map[string]int)

	for _, line := range splitLines(document) {
		lines[line]++
	}

	return lines
}

func splitLines(document string) []string {
	return strings.Split(strings.ReplaceAll(document, "\r\n", "\n"), "\n")
}

func withBOM(document string) []byte {
	return append([]byte{0xEF, 0xBB, 0xBF}, document...)
}
