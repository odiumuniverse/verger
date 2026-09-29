package source

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/store"
)

// hexHash is a valid lowercase sha256 fixture.
func hexHash(seed byte) digest.Hash {
	buf := make([]byte, 32)
	for i := range buf {
		buf[i] = seed
	}

	return digest.Bytes(buf)
}

func TestParseGrammarIDs(t *testing.T) {
	positive := []struct {
		name  string
		input string
		check func(Ref)
	}{
		{
			name:  "short name",
			input: "caveman",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindGitHub)
				So(r.ID, ShouldEqual, "caveman")
				So(r.Repo, ShouldEqual, "caveman")
				So(r.Owner, ShouldBeEmpty)
			},
		},
		{
			name:  "registry id",
			input: "JuliusBrussee/caveman",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindGitHub)
				So(r.ID, ShouldEqual, "JuliusBrussee/caveman")
				So(r.Owner, ShouldEqual, "JuliusBrussee")
				So(r.Repo, ShouldEqual, "caveman")
				So(r.URL, ShouldEqual, "https://github.com/JuliusBrussee/caveman.git")
			},
		},
		{
			name:  "registry id with subpath",
			input: "owner/repo//skills/foo",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindGitHub)
				So(r.ID, ShouldEqual, "owner/repo//skills/foo")
				So(r.Subpath, ShouldEqual, "skills/foo")
			},
		},
		{
			name:  "github with ref",
			input: "github:owner/repo@v1.2.3",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindGitHub)
				So(r.Rev, ShouldEqual, "v1.2.3")
				So(r.URL, ShouldEqual, "https://github.com/owner/repo.git")
			},
		},
		{
			name:  "github with subpath and ref",
			input: "github:owner/repo//skills/foo@main",
			check: func(r Ref) {
				So(r.Subpath, ShouldEqual, "skills/foo")
				So(r.Rev, ShouldEqual, "main")
			},
		},
		{
			name:  "github strips trailing .git",
			input: "github:owner/repo.git",
			check: func(r Ref) {
				So(r.Repo, ShouldEqual, "repo")
				So(r.ID, ShouldEqual, "owner/repo")
				So(r.URL, ShouldEqual, "https://github.com/owner/repo.git")
			},
		},
	}

	Convey("Given the §2.1 id grammar", t, func() {
		for _, tc := range positive {
			Convey("When parsing "+tc.name+" ("+tc.input+")", func() {
				ref, err := Parse(tc.input)

				Convey("Then it parses into the canonical form", func() {
					So(err, ShouldBeNil)
					So(ref.Raw, ShouldEqual, tc.input)
					tc.check(ref)
				})
			})
		}
	})
}

func TestParseGrammarSources(t *testing.T) {
	positive := sourceGrammarCases()

	Convey("Given the §2.1 source grammar", t, func() {
		for _, tc := range positive {
			Convey("When parsing "+tc.name+" ("+tc.input+")", func() {
				ref, err := Parse(tc.input)

				Convey("Then it parses into the canonical form", func() {
					So(err, ShouldBeNil)
					So(ref.Raw, ShouldEqual, tc.input)
					tc.check(ref)
				})
			})
		}
	})
}

func TestParseGrammarNegative(t *testing.T) {
	negative := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"owner only", "owner/"},
		{"three segments", "a/b/c"},
		{"github without repo", "github:"},
		{"github single segment", "github:owner"},
		{"github empty subpath", "owner/repo//"},
		{"subpath traversal", "owner/repo//../x"},
		{"subpath inner traversal", "owner/repo//a/../b"},
		{"archive without pin", "https://x/pkg.tgz"},
		{"archive short pin", "https://x/pkg.tgz#sha256=deadbeef"},
		{"archive uppercase pin", "https://x/pkg.tgz#sha256=" + strings.ToUpper(string(hexHash(1)))},
		{"archive non-hex pin", "https://x/pkg.tgz#sha256=" + strings.Repeat("z", 64)},
		{"git with unknown scheme", "git+ssh://example.com/x.git"},
		{"npm empty", "npm:"},
		{"npm empty version", "npm:@s/p@"},
		{"npm empty name", "npm:@1.0.0"},
		{"mcp empty", "mcp:"},
		{"local traversal", "./local/../escape"},
		{"unknown scheme", "ftp://example.com/x"},
		{"unknown host ref", "foo:bar"},
		{"nul byte", "owner/repo\x00evil"},
		{"huge input", strings.Repeat("a", 5000)},
	}

	Convey("Given the §2.1 grammar", t, func() {
		for _, tc := range negative {
			Convey("When parsing the invalid "+tc.name+" ("+tc.input+")", func() {
				_, err := Parse(tc.input)

				Convey("Then it reports RefError with a reason", func() {
					target, ok := errors.AsType[*RefError](err)
					So(ok, ShouldBeTrue)
					So(target.Input, ShouldEqual, tc.input)
					So(target.Reason, ShouldNotBeEmpty)
				})
			})
		}
	})
}

func TestParseAll(t *testing.T) {
	Convey("Given a list of refs", t, func() {
		Convey("When every ref is valid", func() {
			refs, err := ParseAll([]string{"owner/one", "github:owner/two@v1", "mcp:x/y"})

			Convey("Then order is preserved", func() {
				So(err, ShouldBeNil)
				So(refs, ShouldHaveLength, 3)
				So(refs[0].ID, ShouldEqual, "owner/one")
				So(refs[1].Rev, ShouldEqual, "v1")
				So(refs[2].Kind, ShouldEqual, KindMCP)
			})
		})

		Convey("When the second ref is bad", func() {
			refs, err := ParseAll([]string{"owner/one", "owner/", "owner/three"})

			Convey("Then it errors on the first bad ref with no partial result", func() {
				_, ok := errors.AsType[*RefError](err)
				So(ok, ShouldBeTrue)
				So(refs, ShouldBeNil)
			})
		})
	})
}

func TestRefStringRedactsCredentials(t *testing.T) {
	Convey("Given a git ref carrying userinfo", t, func() {
		ref, err := Parse("git+https://user:secret@example.com/acme/x.git#main")
		So(err, ShouldBeNil)

		Convey("When it is rendered", func() {
			rendered := ref.String()

			Convey("Then the password never appears", func() {
				So(rendered, ShouldNotContainSubstring, "secret")
				So(rendered, ShouldNotContainSubstring, "user:")
				So(rendered, ShouldContainSubstring, "example.com")
			})
		})
	})

	Convey("Given a git ref whose url carries a signed query", t, func() {
		ref, err := Parse("git+https://user:secret@example.com/acme/x.git?X-Amz-Signature=SIGVALUE&auth=AUTHVALUE#main")
		So(err, ShouldBeNil)

		Convey("When it is rendered", func() {
			rendered := ref.String()

			Convey("Then the whole query is gone and the rev survives", func() {
				So(rendered, ShouldNotContainSubstring, "secret")
				So(rendered, ShouldNotContainSubstring, "user:")
				So(rendered, ShouldNotContainSubstring, "SIGVALUE")
				So(rendered, ShouldNotContainSubstring, "AUTHVALUE")
				So(rendered, ShouldContainSubstring, "example.com/acme/x.git")
				So(rendered, ShouldContainSubstring, "#main")
			})
		})
	})
}

func TestRefErrorRedactsCredentials(t *testing.T) {
	Convey("Given refs whose raw input carries credentials", t, func() {
		Convey("When an unpinned credential archive url is rejected", func() {
			input := "https://user:secret@example.com/pkg.tar.gz?X-Amz-Signature=SIGVALUE&token=TOKENVALUE" //nolint:gosec // G101: credential-shaped fixture for the redaction test

			_, err := Parse(input)

			Convey("Then the error keeps the raw input but renders no secret", func() {
				target, ok := errors.AsType[*RefError](err)
				So(ok, ShouldBeTrue)
				So(target.Input, ShouldEqual, input)

				rendered := err.Error()
				So(rendered, ShouldNotContainSubstring, "secret")
				So(rendered, ShouldNotContainSubstring, "user:")
				So(rendered, ShouldNotContainSubstring, "SIGVALUE")
				So(rendered, ShouldNotContainSubstring, "TOKENVALUE")
				So(rendered, ShouldContainSubstring, "example.com")
			})
		})

		Convey("When a git url with a foreign scheme is rejected", func() {
			_, err := Parse("git+ssh://user:secret@example.com/acme/x.git")

			Convey("Then the error renders no userinfo", func() {
				_, ok := errors.AsType[*RefError](err)
				So(ok, ShouldBeTrue)

				rendered := err.Error()
				So(rendered, ShouldNotContainSubstring, "secret")
				So(rendered, ShouldNotContainSubstring, "user:")
				So(rendered, ShouldContainSubstring, "example.com")
			})
		})
	})
}

func TestParseGitRequiresDerivableID(t *testing.T) {
	Convey("Given a git url without an owner/repo path", t, func() {
		_, err := Parse("git+https://example.com/x.git")

		Convey("Then it is a typed RefError, not an empty id", func() {
			target, ok := errors.AsType[*RefError](err)
			So(ok, ShouldBeTrue)
			So(target.Reason, ShouldNotBeEmpty)
		})
	})
}

func TestRefSubpathIDAcceptedDownstream(t *testing.T) {
	Convey("Given the DESIGN §3.4 subpath id form", t, func() {
		ref, err := Parse("vercel-labs/skills//find-skills")
		So(err, ShouldBeNil)
		So(ref.ID, ShouldEqual, "vercel-labs/skills//find-skills")

		Convey("When the store and the spec validator see it", func() {
			st, openErr := store.Open(filepath.Join(t.TempDir(), "store"))
			So(openErr, ShouldBeNil)

			Convey("Then both accept the canonical id", func() {
				_, pathErr := st.PackageDataPath(ref.ID, "claude")
				So(pathErr, ShouldBeNil)
				So(spec.ValidateID(ref.ID), ShouldBeNil)
			})
		})
	})
}

// sourceGrammarCases are the §2.1 SOURCE forms that must parse — the remote
// and package forms, kept out of TestParseGrammarSources so the table does not
// push that function past the length the linter allows. The cases are the same
// ones; only their home changed.
func sourceGrammarCases() []struct {
	name  string
	input string
	check func(Ref)
} {
	return []struct {
		name  string
		input string
		check func(Ref)
	}{
		{
			name:  "git url with ref",
			input: "git+https://example.com/acme/x.git#main",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindGit)
				So(r.URL, ShouldEqual, "https://example.com/acme/x.git")
				So(r.Rev, ShouldEqual, "main")
			},
		},
		{
			name:  "git url without ref",
			input: "git+https://example.com/acme/x.git",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindGit)
				So(r.Rev, ShouldBeEmpty)
			},
		},
		{
			name:  "archive with pin",
			input: "https://example.com/pkg.tar.gz#sha256=" + string(hexHash(7)),
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindURL)
				So(r.URL, ShouldEqual, "https://example.com/pkg.tar.gz")
				So(r.SHA256, ShouldEqual, hexHash(7))
			},
		},
		{
			name:  "file archive with pin",
			input: "file:///tmp/pkg.tar.gz#sha256=" + string(hexHash(8)),
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindURL)
				So(strings.HasPrefix(r.URL, "file://"), ShouldBeTrue)
				So(r.SHA256, ShouldEqual, hexHash(8))
			},
		},
		{
			name:  "scoped npm",
			input: "npm:@scope/pkg@1.2.3",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindNPM)
				So(r.NPM, ShouldEqual, "@scope/pkg@1.2.3")
			},
		},
		{
			name:  "plain npm",
			input: "npm:foo@1.0.0",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindNPM)
				So(r.NPM, ShouldEqual, "foo@1.0.0")
			},
		},
		{
			name:  "mcp registry id",
			input: "mcp:io.github.github/github-mcp-server",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindMCP)
				So(r.MCP, ShouldEqual, "io.github.github/github-mcp-server")
			},
		},
		{
			name:  "local path",
			input: "/tmp",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindLocal)
				So(filepath.IsAbs(r.Path), ShouldBeTrue)
			},
		},
		{
			name:  "local scheme",
			input: "local:/tmp",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindLocal)
				So(r.Path, ShouldEqual, "/tmp")
				So(filepath.Base(r.Path), ShouldEqual, "tmp")
			},
		},
		{
			name:  "absolute path",
			input: "/tmp",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindLocal)
				So(r.Path, ShouldEqual, "/tmp")
			},
		},
		{
			name:  "file scheme",
			input: "file:/tmp",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindLocal)
				So(r.Path, ShouldEqual, "/tmp")
			},
		},
		{
			name:  "agent claude",
			input: "claude:caveman@caveman",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindAgent)
				So(r.Agent, ShouldEqual, "claude")
				So(r.AgentRef, ShouldEqual, "caveman@caveman")
			},
		},
		{
			name:  "agent gemini",
			input: "gemini:my-extension",
			check: func(r Ref) {
				So(r.Kind, ShouldEqual, KindAgent)
				So(r.Agent, ShouldEqual, "gemini")
				So(r.AgentRef, ShouldEqual, "my-extension")
			},
		},
	}
}
