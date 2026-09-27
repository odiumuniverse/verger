package id

import (
	"errors"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestValidatePackage(t *testing.T) {
	Convey("Given canonical package ids", t, func() {
		for _, value := range []string{
			"a",
			"acme/kit",
			"JuliusBrussee/caveman",
			"owner/name_2.0",
			"UPPER/Case",
			"a+b/c@d",
			".hidden/name",
			"mcp:io.github.github/github-mcp-server",
			"claude:caveman@caveman",
			"a//b",
			"vercel-labs/skills//find-skills",
			"owner/repo//skills/foo/bar",
		} {
			Convey("Given the id "+value, func() {
				Convey("When it is validated", func() {
					Convey("Then it is accepted", func() {
						So(ValidatePackage(value), ShouldBeNil)
					})
				})
			})
		}
	})

	Convey("Given malformed package ids", t, func() {
		for _, value := range []string{
			"",
			".",
			"..",
			"/absolute",
			"trailing/",
			"./local/path",
			"a/./b",
			"a/../b",
			"a b",
			"a\tb",
			`a\b`,
			"a\x00b",
			"a*b",
			"a//",
			"//x",
			"a///b",
			"a//b//c",
			"a//b/",
			"a//./b",
			"a//../b",
		} {
			Convey("Given the id "+strings.ReplaceAll(value, "\x00", "<NUL>"), func() {
				Convey("When it is validated", func() {
					err := ValidatePackage(value)

					Convey("Then it is rejected with InvalidIDError", func() {
						target, ok := errors.AsType[*InvalidIDError](err)
						So(ok, ShouldBeTrue)
						So(target.Value, ShouldEqual, value)
						So(target.Reason, ShouldNotBeEmpty)
					})
				})
			})
		}
	})
}

func TestValidateElement(t *testing.T) {
	Convey("Given safe path elements", t, func() {
		for _, value := range []string{
			"claude",
			"codex",
			"1.2.3",
			"v0.1.0-beta+meta",
			"20260925T123045.123456789Z-2",
		} {
			Convey("Given the element "+value, func() {
				Convey("When it is validated", func() {
					Convey("Then it is accepted", func() {
						So(ValidateElement(value), ShouldBeTrue)
					})
				})
			})
		}
	})

	Convey("Given unsafe path elements", t, func() {
		for _, value := range []string{
			"",
			".",
			"..",
			"a/b",
			"a b",
			"a:b",
			"a@b",
			`a\b`,
			"a\x00b",
			"/a",
			"a$",
		} {
			Convey("Given the element "+strings.ReplaceAll(value, "\x00", "<NUL>"), func() {
				Convey("When it is validated", func() {
					Convey("Then it is rejected", func() {
						So(ValidateElement(value), ShouldBeFalse)
					})
				})
			})
		}
	})
}
