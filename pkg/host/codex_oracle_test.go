package host_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
)

func TestCodexOracleList(t *testing.T) {
	Convey("Given a scripted codex plugin list", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		cases := []struct {
			name   string
			output string
			want   int
		}{
			{"flat array", `[{"name":"a","version":"1"},{"name":"b"}]`, 2},
			{"wrapped object", `{"plugins":[{"name":"a","marketplace":"m"}]}`, 1},
			{"deep nesting", `{"data":{"installed":{"items":[{"name":"a","path":"/p"}]}}}`, 1},
			{"noise fields", `[{"foo":1,"name":"a","extra":{"x":true}},{"nope":2}]`, 1},
			{"empty array", `[]`, 0},
			{"empty object", `{}`, 0},
		}

		for _, item := range cases {
			Convey("When the oracle parses "+item.name, func() {
				h, _ := newCodex(t, home, map[string]hostcli.Response{
					"codex plugin list --json": response(item.output),
				})

				listed, err := h.Oracle().List(t.Context())

				Convey("Then the entries are extracted tolerantly", func() {
					So(err, ShouldBeNil)
					So(listed, ShouldHaveLength, item.want)
				})
			})
		}

		Convey("When the output is garbage", func() {
			h, _ := newCodex(t, home, map[string]hostcli.Response{
				"codex plugin list --json": response("not json at all"),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then it is an *OracleError carrying host codex, never a silent empty success", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Host, ShouldEqual, "codex")
				So(typed.Output, ShouldContainSubstring, "not json")
			})
		})

		Convey("When the output is a non-container scalar", func() {
			h, _ := newCodex(t, home, map[string]hostcli.Response{
				"codex plugin list --json": response(`"ok"`),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then it is an *OracleError", func() {
				_, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the CLI exits non-zero", func() {
			h, _ := newCodex(t, home, map[string]hostcli.Response{
				"codex plugin list --json": {Code: 3, Stderr: "boom"},
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then the *ExitError passes through unchanged", func() {
				typed, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(typed.Code, ShouldEqual, 3)
			})
		})

		Convey("When the context is canceled", func() {
			h, _ := newCodex(t, home, nil)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := h.Oracle().List(ctx)

			Convey("Then the context error surfaces", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})
	})
}

func TestCodexOracleValidate(t *testing.T) {
	Convey("Given a scripted codex plugin validate", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		Convey("When validation succeeds with output", func() {
			h, runner := newCodex(t, home, map[string]hostcli.Response{
				"codex plugin validate /tmp/synth": response("warning one\nwarning two\n\n"),
			})

			warnings, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then every output line is a warning", func() {
				So(err, ShouldBeNil)
				So(warnings, ShouldResemble, []string{"warning one", "warning two"})
				So(callKeys(runner), ShouldResemble, []string{"codex plugin validate /tmp/synth"})
			})
		})

		Convey("When validation fails", func() {
			h, _ := newCodex(t, home, map[string]hostcli.Response{
				"codex plugin validate /tmp/synth": {Code: 1, Stderr: "plugin.json invalid"},
			})

			_, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then it is an *OracleError carrying the output", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Host, ShouldEqual, "codex")
				So(typed.Output, ShouldContainSubstring, "plugin.json invalid")
			})
		})

		Convey("When the CLI has no validate subcommand", func() {
			h, _ := newCodex(t, home, map[string]hostcli.Response{
				"codex plugin validate /tmp/synth": {Code: 2, Stderr: "error: unrecognized subcommand 'validate'"},
			})

			_, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then it reports ErrNotSupported", func() {
				So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
			})
		})

		Convey("When the codex binary is missing", func() {
			t.Setenv("PATH", t.TempDir())

			h, _ := newCodex(t, home, nil)

			_, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then it reports ErrNotSupported", func() {
				So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
			})
		})
	})
}
