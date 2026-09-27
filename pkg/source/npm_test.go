package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostcli"
)

func TestFetchNPM(t *testing.T) {
	tarball := buildTarGz(t, []tarEntry{
		{name: "package/", dir: true},
		{name: "package/.claude-plugin/plugin.json", body: `{"name":"npmpkg"}`},
		{name: "package/skills/a/SKILL.md", body: "# a\n"},
	})

	Convey("Given an npm ref and a scripted npm pack", t, func() {
		runner := &recordRunner{}
		f, cache := newTestFetcher(t, WithRunner(runner))

		runner.run = func(_ context.Context, call hostcli.Call) ([]byte, error) {
			if call.Binary != "npm" {
				return nil, &hostcli.ExitError{Name: call.Binary, Code: 127, Stderr: "unexpected binary"}
			}

			dest := call.Args[len(call.Args)-1]
			if err := os.WriteFile(filepath.Join(dest, "npmpkg-1.0.0.tgz"), tarball, 0o600); err != nil {
				return nil, err
			}

			return []byte("npmpkg-1.0.0.tgz\n"), nil
		}

		ref, err := Parse("npm:@scope/npmpkg@1.0.0")
		So(err, ShouldBeNil)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), ref)

			Convey("Then npm pack ran with --ignore-scripts and the tarball is extracted", func() {
				So(err, ShouldBeNil)

				calls := runner.Calls()
				So(calls, ShouldHaveLength, 1)
				So(calls[0].Binary, ShouldEqual, "npm")
				So(calls[0].Args[:3], ShouldResemble, []string{"pack", "@scope/npmpkg@1.0.0", "--ignore-scripts"})
				So(calls[0].Args[3], ShouldEqual, "--pack-destination")
				So(filepath.IsAbs(calls[0].Args[4]), ShouldBeTrue)

				So(filepath.Base(got.Root), ShouldEqual, "package")

				_, statErr := os.Stat(filepath.Join(got.Root, ".claude-plugin", "plugin.json"))
				So(statErr, ShouldBeNil)
				So(got.Package, ShouldNotBeNil)
				So(string(got.Package.Format), ShouldEqual, "claude")
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})
		})
	})

	Convey("Given npm exits non-zero", t, func() {
		runner := &recordRunner{run: func(_ context.Context, call hostcli.Call) ([]byte, error) {
			return nil, &hostcli.ExitError{Name: call.Binary, Code: 1, Stderr: "npm ERR! 404 Not Found"}
		}}
		f, cache := newTestFetcher(t, WithRunner(runner))

		Convey("When the pack fails", func() {
			_, err := f.Fetch(t.Context(), Ref{Kind: KindNPM, NPM: "foo@1.0.0"})

			Convey("Then it reports FetchError at the pack step and leaves no entry", func() {
				target, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
				So(target.Step, ShouldEqual, StepNPMPack)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})
}

func TestFetchNPMMissingTool(t *testing.T) {
	Convey("Given npm is not resolvable", t, func() {
		Convey("When an npm ref is fetched", func() {
			Convey("Then it reports ToolMissingError for npm", func() {
				assertToolMissing(t, Ref{Kind: KindNPM, NPM: "foo@1.0.0"}, "npm")
			})
		})
	})
}
