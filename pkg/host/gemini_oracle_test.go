package host_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
)

func TestGeminiOracleList(t *testing.T) {
	Convey("Given a scripted gemini extensions list", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		cases := []struct {
			name   string
			output string
			want   int
		}{
			{"flat array", `[{"name":"a","version":"1"},{"name":"b"}]`, 2},
			{"wrapped object", `{"extensions":[{"name":"a","version":"1.0"}]}`, 1},
			{"deep nesting", `{"data":{"installed":{"items":[{"name":"a","path":"/p"}]}}}`, 1},
			{"noise fields", `[{"foo":1,"name":"a","extra":{"x":true}},{"nope":2}]`, 1},
			{"empty array", `[]`, 0},
			{"empty object", `{}`, 0},
		}

		for _, item := range cases {
			Convey("When the oracle parses "+item.name, func() {
				h, runner := newGemini(t, home, map[string]hostcli.Response{
					"gemini extensions list --output-format json": response(item.output),
				})

				listed, err := h.Oracle().List(t.Context())

				Convey("Then the entries are extracted tolerantly from the exact argv", func() {
					So(err, ShouldBeNil)
					So(listed, ShouldHaveLength, item.want)
					So(callKeys(runner), ShouldResemble, []string{"gemini extensions list --output-format json"})
				})
			})
		}

		Convey("When the output is garbage", func() {
			h, _ := newGemini(t, home, map[string]hostcli.Response{
				"gemini extensions list --output-format json": response("not json at all"),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then it is an *OracleError carrying host gemini, never a silent empty success", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Host, ShouldEqual, "gemini")
				So(typed.Output, ShouldContainSubstring, "not json")
			})
		})

		Convey("When the output is a non-container scalar", func() {
			h, _ := newGemini(t, home, map[string]hostcli.Response{
				"gemini extensions list --output-format json": response(`"ok"`),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then it is an *OracleError", func() {
				_, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the CLI exits non-zero", func() {
			h, _ := newGemini(t, home, map[string]hostcli.Response{
				"gemini extensions list --output-format json": {Code: 3, Stderr: "boom"},
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then the *ExitError passes through unchanged", func() {
				typed, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(typed.Code, ShouldEqual, 3)
			})
		})

		Convey("When the context is canceled", func() {
			h, _ := newGemini(t, home, nil)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := h.Oracle().List(ctx)

			Convey("Then the context error surfaces", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})
	})
}

func TestGeminiOracleValidate(t *testing.T) {
	Convey("Given a scripted gemini extensions validate", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		Convey("When validation succeeds with output", func() {
			h, runner := newGemini(t, home, map[string]hostcli.Response{
				"gemini extensions validate /tmp/ext": response("warning one\nwarning two\n\n"),
			})

			warnings, err := h.Oracle().Validate(t.Context(), "/tmp/ext")

			Convey("Then every output line is a warning", func() {
				So(err, ShouldBeNil)
				So(warnings, ShouldResemble, []string{"warning one", "warning two"})
				So(callKeys(runner), ShouldResemble, []string{"gemini extensions validate /tmp/ext"})
			})
		})

		Convey("When validation fails", func() {
			h, _ := newGemini(t, home, map[string]hostcli.Response{
				"gemini extensions validate /tmp/ext": {Code: 1, Stderr: "extension.json invalid"},
			})

			_, err := h.Oracle().Validate(t.Context(), "/tmp/ext")

			Convey("Then it is an *OracleError carrying the output", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Host, ShouldEqual, "gemini")
				So(typed.Output, ShouldContainSubstring, "extension.json invalid")
			})
		})

		Convey("When the CLI has no validate subcommand", func() {
			h, _ := newGemini(t, home, map[string]hostcli.Response{
				"gemini extensions validate " + geminiFixtureRoot: {Code: 2, Stderr: "error: unrecognized subcommand 'validate'"},
			})

			warnings, err := h.Oracle().Validate(t.Context(), geminiFixtureRoot)

			Convey("Then the manifest fallback validates the directory", func() {
				So(err, ShouldBeNil)
				So(warnings, ShouldBeNil)
			})
		})

		Convey("When the gemini binary is missing", func() {
			t.Setenv("PATH", t.TempDir())

			h, _ := newGemini(t, home, nil)

			Convey("When the directory carries no manifest", func() {
				_, err := h.Oracle().Validate(t.Context(), filepath.Join(t.TempDir(), "missing"))

				Convey("Then the fallback reports an *OracleError", func() {
					_, ok := errors.AsType[*host.OracleError](err)
					So(ok, ShouldBeTrue)
				})
			})

			Convey("When the directory carries the fixture manifest", func() {
				warnings, err := h.Oracle().Validate(t.Context(), geminiFixtureRoot)

				Convey("Then the fallback parses it and returns warnings", func() {
					So(err, ShouldBeNil)
					So(warnings, ShouldBeNil)
				})
			})
		})
	})
}
