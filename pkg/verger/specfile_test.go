package verger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// TestAddSpecSourceKeepsALocalRefPortable pins FAIL4-A: `verger install
// local:/abs/path` used to write that absolute path into the spec, so a vault
// cloned to another machine pointed at a path that does not exist there.
//
// A path under the spec directory is stored relative to it — which is exactly
// what source.Parse already resolves — and a path outside keeps its absolute
// spelling and is reported by NonPortableSources.
func TestAddSpecSourceKeepsALocalRefPortable(t *testing.T) {
	Convey("Given a spec directory and a local ref under it", t, func() {
		specDir := t.TempDir()
		inside := filepath.Join(specDir, "vault", "caveman")

		So(os.MkdirAll(inside, 0o700), ShouldBeNil)

		Convey("Then the spec stores a path relative to the spec directory", func() {
			doc := spec.New()
			So(AddSpecSourceAt(doc, localRefAt(t, inside, specDir), specDir), ShouldBeTrue)

			So(doc.Sources, ShouldHaveLength, 1)
			So(doc.Sources[0].URL, ShouldEqual, "./vault/caveman")

			Convey("And it carries no absolute path at all", func() {
				So(doc.Sources[0].URL, ShouldNotStartWith, "/")
			})

			Convey("And nothing is reported as non-portable", func() {
				So(NonPortableSources(doc, specDir), ShouldBeEmpty)
			})

			Convey("And a spec round-trip through TOML keeps the relative form", func() {
				// The rewrite is only worth anything if it survives the
				// write/read the vault actually does.
				path := filepath.Join(specDir, "verger.toml")
				So(SaveSpec(path, doc), ShouldBeNil)

				read, _, err := LoadSpec(path)
				So(err, ShouldBeNil)
				So(read.Sources, ShouldHaveLength, 1)
				So(read.Sources[0].URL, ShouldEqual, "./vault/caveman")
			})
		})
	})

	Convey("Given a local ref outside the spec directory", t, func() {
		specDir := filepath.Join(t.TempDir(), "project")
		outside := filepath.Join(t.TempDir(), "elsewhere", "caveman")

		So(os.MkdirAll(outside, 0o700), ShouldBeNil)

		Convey("Then the spec keeps the absolute path", func() {
			// A relative spelling would need "..", which a local ref is not
			// allowed to contain, so there is nothing better to write.
			doc := spec.New()
			So(AddSpecSourceAt(doc, localRefAt(t, outside, specDir), specDir), ShouldBeTrue)

			So(doc.Sources, ShouldHaveLength, 1)
			So(doc.Sources[0].URL, ShouldEqual, outside)
		})

		Convey("Then the caller is told it will not survive a clone", func() {
			doc := spec.New()
			So(AddSpecSourceAt(doc, localRefAt(t, outside, specDir), specDir), ShouldBeTrue)

			So(NonPortableSources(doc, specDir), ShouldResemble, []string{outside})
		})
	})
}

// TestNonPortableSourcesIgnoresOtherKinds pins that the warning is about
// local paths only: a git or npm source is portable by construction and must
// not be reported.
func TestNonPortableSourcesIgnoresOtherKinds(t *testing.T) {
	Convey("Given a spec with no local source", t, func() {
		doc := spec.New()
		doc.Sources = []spec.Source{
			{Name: "acme", URL: "git+ssh://git@github.com/acme/repo"},
			{Name: "pkg", URL: "npm:some-package"},
		}

		Convey("Then nothing is reported", func() {
			So(NonPortableSources(doc, t.TempDir()), ShouldBeEmpty)
		})
	})
}

// localRefAt builds the ref `verger install local:<dir>` produces for a package
// living at dir.
func localRefAt(t *testing.T, dir, _ string) source.Ref {
	t.Helper()

	parsed, err := source.Parse(string(source.KindLocal) + ":" + dir)
	So(err, ShouldBeNil)

	return parsed
}

// userSpec is a document a person kept by hand: a comment above every table,
// double quotes, a blank line between sections and a trailing comment on a
// value. Every command that writes a spec goes through the five helpers below,
// so this is where the promise is checked once for all of them.
const userSpec = `# the canon, kept in step with beadle's
schema = 1

# what to do on a first install
[defaults]
hooks = "ask"

[[source]]
name = "getverger"
url = "https://github.com/vmkteam/getverger"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
`

// writeCase is one writing command: what it does to the model, and the document
// it must leave behind.
type writeCase struct {
	name  string
	apply func(t *testing.T, doc *spec.Spec, dir string) error
	want  string
}

// writeCases is the matrix of every command that writes a spec, run against
// the same hand-written document.
//
//nolint:funlen // the matrix of writing commands is one table of data
func writeCases() []writeCase {
	return []writeCase{
		{
			name: "an install adds its package",
			apply: func(_ *testing.T, doc *spec.Spec, _ string) error {
				AddSpecPackage(doc, spec.Package{ID: "acme/new", Version: "1.0.0"})

				return nil
			},
			want: userSpec + `
[[package]]
id = "acme/new"
version = "1.0.0"
`,
		},
		{
			name: "an adopt records where the package came from",
			apply: func(_ *testing.T, doc *spec.Spec, _ string) error {
				AddSpecPackage(doc, spec.Package{ID: "acme/caveman", AdoptedFrom: "acme/caveman"})

				return nil
			},
			want: userSpec,
		},
		{
			name: "a remove takes the package and its own comment",
			apply: func(_ *testing.T, doc *spec.Spec, _ string) error {
				if RemoveSpecPackage(doc, "acme/hooks") != 1 {
					return errUnexpectedCount
				}

				return nil
			},
			want: `# the canon, kept in step with beadle's
schema = 1

# what to do on a first install
[defaults]
hooks = "ask"

[[source]]
name = "getverger"
url = "https://github.com/vmkteam/getverger"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep
`,
		},
		{
			name: "a pin adds one line",
			apply: func(_ *testing.T, doc *spec.Spec, _ string) error {
				if _, err := SetSpecPin(doc, "acme/caveman", "1.0.0"); err != nil {
					return err
				}

				return nil
			},
			want: `# the canon, kept in step with beadle's
schema = 1

# what to do on a first install
[defaults]
hooks = "ask"

[[source]]
name = "getverger"
url = "https://github.com/vmkteam/getverger"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep
version = "1.0.0"

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
`,
		},
		{
			name: "an unpin takes one line",
			apply: func(_ *testing.T, doc *spec.Spec, _ string) error {
				if _, err := SetSpecPin(doc, "acme/hooks", ""); err != nil {
					return err
				}

				return nil
			},
			want: `# the canon, kept in step with beadle's
schema = 1

# what to do on a first install
[defaults]
hooks = "ask"

[[source]]
name = "getverger"
url = "https://github.com/vmkteam/getverger"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep

# a hooks-only package
[[package]]
id = "acme/hooks"
`,
		},
		{
			name: "a source add joins the sources already there",
			apply: func(t *testing.T, doc *spec.Spec, dir string) error {
				t.Helper()

				inside := filepath.Join(dir, "vault", "caveman")
				if err := os.MkdirAll(inside, 0o700); err != nil {
					return err
				}

				AddSpecSourceAt(doc, localRefAt(t, inside, dir), dir)

				return nil
			},
			want: `# the canon, kept in step with beadle's
schema = 1

# what to do on a first install
[defaults]
hooks = "ask"

[[source]]
name = "getverger"
url = "https://github.com/vmkteam/getverger"

[[source]]
name = "caveman"
url = "./vault/caveman"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
`,
		},
		{
			name: "a source remove takes the source and its block",
			apply: func(_ *testing.T, doc *spec.Spec, _ string) error {
				if RemoveSpecSource(doc, "getverger") != 1 {
					return errUnexpectedCount
				}

				return nil
			},
			want: `# the canon, kept in step with beadle's
schema = 1

# what to do on a first install
[defaults]
hooks = "ask"

# the review agent, per the incident of 2026-06
[[package]]
id = "acme/caveman"
channel = "stable" # earns its keep

# a hooks-only package
[[package]]
id = "acme/hooks"
version = "1.0.0"
`,
		},
	}
}

func TestSpecWritesKeepTheUsersBytes(t *testing.T) {
	cases := writeCases()

	Convey("Given a spec the user wrote by hand", t, func() {
		for _, tc := range cases {
			Convey("When "+tc.name+" writes it", func() {
				dir := t.TempDir()
				path := filepath.Join(dir, "verger.toml")
				So(os.WriteFile(path, []byte(userSpec), 0o600), ShouldBeNil)

				doc, present, err := LoadSpec(path)
				So(err, ShouldBeNil)
				So(present, ShouldBeTrue)

				So(tc.apply(t, doc, dir), ShouldBeNil)
				So(SaveSpec(path, doc), ShouldBeNil)

				Convey("Then every byte the command did not mean to change is still there", func() {
					So(string(mustReadSpec(t, path)), ShouldEqual, tc.want)
				})
			})
		}
	})
}

func mustReadSpec(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test wrote this path
	So(err, ShouldBeNil)

	return data
}

var errUnexpectedCount = errors.New("a spec helper reported an unexpected count")
