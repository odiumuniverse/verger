package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
)

// codexList is the real `codex plugin list --json` answer (codex-cli 0.157.1
// shape, T1.7-G) for the installed plugin selectors.
func codexList(ids ...string) string {
	entries := make([]string, 0, len(ids))

	for _, id := range ids {
		plugin, marketplace, _ := strings.Cut(id, "@")
		entries = append(entries, `{"pluginId":"`+id+`","name":"`+plugin+`","marketplaceName":"`+marketplace+
			`","version":"1.2.3","installed":true,"enabled":true}`)
	}

	return `{"installed":[` + strings.Join(entries, ",") + `],"available":[]}`
}

// TestCodexOracleListRealShape pins the codex-cli 0.157.1 list
// (testdata/codex/plugin-list-0.157.1.json, captured live; the `available`
// entry is from `--available`): only installed plugins count, keyed by
// pluginId/name/marketplaceName.
func TestCodexOracleListRealShape(t *testing.T) {
	Convey("Given the real codex plugin list output", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		output, err := os.ReadFile(filepath.Join("testdata", "codex", "plugin-list-0.157.1.json"))
		So(err, ShouldBeNil)

		h, _ := newCodex(t, home, map[string]hostcli.Response{"codex plugin list --json": {Stdout: output}})

		Convey("When the oracle lists it", func() {
			listed, listErr := h.Oracle().List(t.Context())

			Convey("Then the installed plugin comes back and the available one is not installed", func() {
				// The list carries no install path; `source.path` is the
				// marketplace copy, which the parser never takes for one.
				So(listErr, ShouldBeNil)
				So(listed, ShouldResemble, []host.Installed{{
					Name: "foo", Marketplace: "acme", Version: "1.1.0", Enabled: true,
				}})
			})
		})
	})
}

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
			{"the real empty list", `{"installed":[],"available":[]}`, 0},
			{"a bare plugin id", `{"installed":[{"pluginId":"a@m","installed":true}]}`, 1},
			{"legacy flat array", `[{"name":"a","version":"1"},{"name":"b"}]`, 2},
			{"noise fields", `[{"foo":1,"name":"a","extra":{"x":true}},{"nope":2}]`, 1},
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

// TestCodexOracleValidate pins that codex-cli 0.157.1 has no plugin validate
// subcommand: validation is not supported and no call is made.
func TestCodexOracleValidate(t *testing.T) {
	Convey("Given the Codex oracle", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		h, runner := newCodex(t, home, nil)

		Convey("When a synth dir is validated", func() {
			_, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then it reports ErrNotSupported without a call", func() {
				So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})
}
