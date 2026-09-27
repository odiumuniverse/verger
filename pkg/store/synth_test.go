package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	pkgid "github.com/odiumuniverse/verger/pkg/id"
)

func TestSynthPathLayout(t *testing.T) {
	Convey("Given an open store", t, func() {
		st := newStore(t, time.Now)

		Convey("When a slashed package id resolves", func() {
			path, err := st.SynthPath("JuliusBrussee/caveman", "1.2.3")

			Convey("Then the synth dir nests the owner, the name and the version", func() {
				So(err, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(st.Root(), "synth", "JuliusBrussee", "caveman", "1.2.3"))
			})
		})

		Convey("When a canonical subpath id resolves", func() {
			path, err := st.SynthPath("vercel-labs/skills//find-skills", "1.2.3")

			Convey("Then the rest of the id is one escaped name element below the owner", func() {
				So(err, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(st.Root(), "synth", "vercel-labs", "skills+2F+2Ffind-skills", "1.2.3"))
			})
		})

		Convey("When ids differing only by the subpath separator resolve", func() {
			subpath, subErr := st.SynthPath("vercel-labs/skills//find-skills", "1.2.3")
			nested, nestedErr := st.SynthPath("vercel-labs/skills/find-skills", "1.2.3")

			Convey("Then they never share a synth dir", func() {
				So(subErr, ShouldBeNil)
				So(nestedErr, ShouldBeNil)
				So(subpath, ShouldNotEqual, nested)
			})
		})

		Convey("When a single-segment id resolves", func() {
			_, err := st.SynthPath("caveman", "1.2.3")

			Convey("Then it is refused: a synth dir needs an owner", func() {
				_, ok := errors.AsType[*InvalidIDError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When EnsureSynthPath runs", func() {
			path, err := st.EnsureSynthPath("JuliusBrussee/caveman", "1.2.3")

			Convey("Then the whole chain exists with mode 0700", func() {
				So(err, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(st.Root(), "synth", "JuliusBrussee", "caveman", "1.2.3"))

				for current := path; current != st.Root(); current = filepath.Dir(current) {
					assertMode(t, current, 0o700)
				}

				_, secondErr := st.EnsureSynthPath("JuliusBrussee/caveman", "1.2.3")
				So(secondErr, ShouldBeNil)
			})
		})

		Convey("When Ensure runs", func() {
			err := st.Ensure()

			Convey("Then the synth root exists", func() {
				So(err, ShouldBeNil)
				assertMode(t, filepath.Join(st.Root(), "synth"), 0o700)
			})
		})
	})
}

// unescapeSynthElement reverses the `+HH` escaping of one synth element.
func unescapeSynthElement(t *testing.T, element string) string {
	t.Helper()

	var out strings.Builder

	for i := 0; i < len(element); i++ {
		if element[i] != '+' {
			out.WriteByte(element[i])

			continue
		}

		if i+2 >= len(element) {
			t.Fatalf("truncated escape in %q", element)
		}

		value, err := strconv.ParseUint(element[i+1:i+3], 16, 8)
		if err != nil {
			t.Fatalf("bad escape in %q: %v", element, err)
		}

		out.WriteByte(byte(value))

		i += 2
	}

	return out.String()
}

// TestSynthElements pins the synth dir identity: the owner is the first id
// segment, the name is the rest of the id, both escaped reversibly so the
// mapping is injective and a plain owner/name id stays readable.
func TestSynthElements(t *testing.T) {
	ids := []string{
		"JuliusBrussee/caveman",
		"acme/foo.nvim",
		"acme/my_plugin",
		"vercel-labs/skills//find-skills",
		"vercel-labs/skills/find-skills",
		"gitlab.com/group/repo",
		"acme/a+b",
		"acme/a+2Bb",
		"npm:@scope/pkg",
		"acme/repo//plugins/sub@1",
	}

	Convey("Given package ids of every shape", t, func() {
		Convey("When each is split into synth elements", func() {
			seen := map[string]string{}

			for _, id := range ids {
				owner, name, err := SynthElements(id)
				So(err, ShouldBeNil)

				Convey("Then "+id+" round-trips and yields safe path elements", func() {
					So(unescapeSynthElement(t, owner)+"/"+unescapeSynthElement(t, name), ShouldEqual, id)
					So(pkgid.ValidateElement(owner), ShouldBeTrue)
					So(pkgid.ValidateElement(name), ShouldBeTrue)
				})

				key := owner + "/" + name
				So(seen[key], ShouldBeEmpty)

				seen[key] = id
			}
		})

		Convey("When a plain owner/name id is split", func() {
			owner, name, err := SynthElements("JuliusBrussee/caveman")

			Convey("Then both elements are the id segments verbatim", func() {
				So(err, ShouldBeNil)
				So(owner, ShouldEqual, "JuliusBrussee")
				So(name, ShouldEqual, "caveman")
			})
		})

		Convey("When the id has no owner or is invalid", func() {
			for _, id := range []string{"caveman", "", "../x", "a//"} {
				_, _, err := SynthElements(id)

				Convey("Then "+id+" is refused", func() {
					_, ok := errors.AsType[*InvalidIDError](err)
					So(ok, ShouldBeTrue)
				})
			}
		})
	})
}

func TestSynthPathValidation(t *testing.T) {
	Convey("Given an open store", t, func() {
		st := newStore(t, time.Now)

		Convey("When path elements are hostile", func() {
			for _, pkg := range []string{"", "../x", "a//", "a///b", "/abs", "a/../b", "a\\b", "a\x00b"} {
				Convey("Then package id "+pkg+" is refused", func() {
					_, err := st.SynthPath(pkg, "1.0.0")
					_, ok := errors.AsType[*InvalidIDError](err)

					So(ok, ShouldBeTrue)
				})
			}

			for _, version := range []string{"", "..", "a/b", ".", "v 1"} {
				Convey("Then version "+version+" is refused", func() {
					_, err := st.SynthPath("JuliusBrussee/caveman", version)
					_, ok := errors.AsType[*InvalidIDError](err)

					So(ok, ShouldBeTrue)
				})
			}
		})

		Convey("When a refused path is ensured", func() {
			_, err := st.EnsureSynthPath("../escape", "1.0.0")

			Convey("Then nothing is created", func() {
				So(err, ShouldNotBeNil)

				_, statErr := os.Stat(st.Root())
				So(os.IsNotExist(statErr), ShouldBeTrue)
			})
		})
	})
}
