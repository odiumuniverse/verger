package hostcli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostcli"
)

// runnerFunc adapts a function to the Runner interface.
type runnerFunc func(ctx context.Context, bin hostcli.Binary, args []string, stdin []byte) ([]byte, error)

// Run implements Runner.
func (f runnerFunc) Run(ctx context.Context, bin hostcli.Binary, args []string, stdin []byte) ([]byte, error) {
	return f(ctx, bin, args, stdin)
}

func TestExecRunner(t *testing.T) {
	Convey("Given a host CLI fixture", t, func() {
		bin := hostcli.Binary{
			Name:   "claude",
			Path:   fixture(t, "probe.sh"),
			Source: hostcli.SourceRecorded,
			PATH:   "/injected/bin:/usr/bin",
		}

		Convey("When it runs with arguments and stdin", func() {
			out, err := bin.RunWith(t.Context(), nil, []string{"--flag", "a b"}, []byte("hello"))

			Convey("Then the fixture sees the injected PATH, the exact argv and the stdin", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "path=/injected/bin:/usr/bin\nargc=2\narg=[--flag]\narg=[a b]\nstdin=hello")
			})
		})

		Convey("When no PATH is recorded", func() {
			t.Setenv("PATH", "/usr/bin:/bin")

			inherited := hostcli.Binary{Name: "claude", Path: fixture(t, "probe.sh")}

			out, err := inherited.RunWith(t.Context(), hostcli.ExecRunner{}, nil, nil)

			Convey("Then the process environment is inherited", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldContainSubstring, "path=/usr/bin:/bin\n")
			})
		})
	})

	Convey("Given a CLI whose child PATH is hostile text", t, func() {
		bin := hostcli.Binary{
			Name: "claude",
			Path: fixture(t, "probe.sh"),
			PATH: `/tmp/evil:$(id):"quoted":зн`,
		}

		Convey("When it runs", func() {
			out, err := bin.RunWith(t.Context(), hostcli.ExecRunner{}, nil, nil)

			Convey("Then the value is passed as an environment variable, never interpreted", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldContainSubstring, "path=/tmp/evil:$(id):\"quoted\":зн\n")
			})
		})
	})
}

func TestExecRunnerFailures(t *testing.T) {
	Convey("Given a host CLI that rejects the call", t, func() {
		bin := hostcli.Binary{Name: "claude", Path: fixture(t, "fail.sh")}

		Convey("When it runs", func() {
			out, err := bin.RunWith(t.Context(), nil, nil, nil)

			exitErr, ok := errors.AsType[*hostcli.ExitError](err)

			Convey("Then stdout, the exit code and stderr come back in an ExitError", func() {
				So(ok, ShouldBeTrue)
				So(string(out), ShouldEqual, "partial-out\n")
				So(exitErr.Name, ShouldEqual, "claude")
				So(exitErr.Code, ShouldEqual, 3)
				So(exitErr.Stderr, ShouldEqual, "boom on stderr")
				So(errors.Is(err, hostcli.ErrNotFound), ShouldBeFalse)
			})
		})
	})

	Convey("Given a resolved binary that vanished", t, func() {
		dir := t.TempDir()
		path := script(t, dir, "claude", "#!/bin/sh\necho ok\n", 0o755)

		bin := hostcli.Binary{Name: "claude", Path: path}

		So(os.Remove(path), ShouldBeNil)

		Convey("When it runs", func() {
			_, err := bin.RunWith(t.Context(), nil, nil, nil)

			Convey("Then it is ErrNotFound, never a host failure", func() {
				So(errors.Is(err, hostcli.ErrNotFound), ShouldBeTrue)
			})
		})
	})

	Convey("Given a binary path that never existed", t, func() {
		bin := hostcli.Binary{Name: "claude", Path: filepath.Join(t.TempDir(), "nope")}

		Convey("When it runs", func() {
			_, err := bin.RunWith(t.Context(), nil, nil, nil)

			Convey("Then it is ErrNotFound", func() {
				So(errors.Is(err, hostcli.ErrNotFound), ShouldBeTrue)
			})
		})
	})

	Convey("Given a canceled context", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		Convey("When RunWith runs through a nil runner", func() {
			_, err := hostcli.Binary{Name: "claude", Path: fixture(t, "probe.sh")}.RunWith(ctx, nil, nil, nil)

			Convey("Then the cancellation surfaces as context.Canceled", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, "claude")
			})
		})

		Convey("When the runner is invoked directly", func() {
			_, err := hostcli.ExecRunner{}.Run(ctx, hostcli.Binary{Name: "claude", Path: fixture(t, "probe.sh")}, nil, nil)

			Convey("Then the cancellation surfaces as context.Canceled", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})
	})

	Convey("Given a long-running CLI and a deadline", t, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()

		bin := hostcli.Binary{Name: "claude", Path: fixture(t, "sleep.sh")}

		Convey("When the deadline passes mid-run", func() {
			started := time.Now()

			_, err := bin.RunWith(ctx, nil, nil, nil)

			elapsed := time.Since(started)

			Convey("Then the deadline surfaces and the process is killed", func() {
				So(errors.Is(err, context.DeadlineExceeded), ShouldBeTrue)
				So(errors.Is(err, context.Canceled), ShouldBeFalse)
				So(elapsed, ShouldBeLessThan, 4*time.Second)
			})
		})
	})
}

func TestExecRunnerPayload(t *testing.T) {
	Convey("Given arguments that would be dangerous through a shell", t, func() {
		dir := t.TempDir()
		pwned := filepath.Join(dir, "pwned")
		path := script(t, dir, "claude", "#!/bin/sh\nfor arg in \"$@\"; do printf 'arg=[%s]\\n' \"$arg\"; done\n", 0o755)

		args := []string{
			"$(touch " + pwned + ")",
			"a b",
			`"quoted"`,
			"'; rm -rf /",
			"back\\slash",
			"line1\nline2",
			"--flag=$(id)",
		}

		bin := hostcli.Binary{Name: "claude", Path: path}

		Convey("When it runs", func() {
			out, err := bin.RunWith(t.Context(), nil, args, nil)

			Convey("Then every argument arrives literally and nothing is executed", func() {
				So(err, ShouldBeNil)

				var want strings.Builder
				for _, arg := range args {
					want.WriteString("arg=[" + arg + "]\n")
				}

				So(string(out), ShouldEqual, want.String())

				_, statErr := os.Stat(pwned)
				So(errors.Is(statErr, os.ErrNotExist), ShouldBeTrue)
			})
		})
	})

	Convey("Given a CLI producing unicode and non-UTF-8 bytes", t, func() {
		bin := hostcli.Binary{Name: "claude", Path: fixture(t, "unicode.sh")}

		Convey("When it runs", func() {
			out, err := bin.RunWith(t.Context(), nil, nil, nil)

			Convey("Then the bytes are returned unmodified", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "снег ☃ tail\noctal \xff byte\n")
				So(out[strings.IndexByte(string(out), ' ')+1], ShouldEqual, 0xe2)
			})
		})
	})

	Convey("Given a CLI passing a huge payload through", t, func() {
		bin := hostcli.Binary{Name: "claude", Path: fixture(t, "passthrough.sh")}

		huge := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB

		Convey("When it runs", func() {
			out, err := bin.RunWith(t.Context(), nil, nil, huge)

			Convey("Then no output is trimmed or buffered away", func() {
				So(err, ShouldBeNil)
				So(len(out), ShouldEqual, len(huge))
				So(bytes.Equal(out, huge), ShouldBeTrue)
			})
		})
	})

	Convey("Given a version fixture", t, func() {
		bin := hostcli.Binary{Name: "claude", Path: fixture(t, "version.sh")}

		Convey("When the version is asked", func() {
			version, err := bin.Version(t.Context())

			Convey("Then the first line comes back", func() {
				So(err, ShouldBeNil)
				So(version, ShouldEqual, "2.1.281 (Claude Code)")
			})
		})

		Convey("When the version call fails", func() {
			failing := hostcli.Binary{Name: "claude", Path: fixture(t, "fail.sh")}

			_, err := failing.Version(t.Context())

			_, ok := errors.AsType[*hostcli.ExitError](err)

			Convey("Then the failure propagates", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestRunWith(t *testing.T) {
	Convey("Given an explicit fake runner", t, func() {
		bin := hostcli.Binary{Name: "claude", Path: "/resolved/claude", Source: hostcli.SourceRecorded, PATH: "/bin"}

		var (
			gotBin   hostcli.Binary
			gotArgs  []string
			gotStdin []byte
		)

		runner := runnerFunc(func(_ context.Context, b hostcli.Binary, args []string, stdin []byte) ([]byte, error) {
			gotBin, gotArgs, gotStdin = b, args, stdin

			return []byte("fake-out"), nil
		})

		Convey("When the binary runs through it", func() {
			out, err := bin.RunWith(t.Context(), runner, []string{"a", "b"}, []byte("in"))

			Convey("Then the runner receives the binary, argv and stdin unchanged", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "fake-out")
				So(gotBin, ShouldResemble, bin)
				So(gotArgs, ShouldResemble, []string{"a", "b"})
				So(gotStdin, ShouldResemble, []byte("in"))
			})
		})

		Convey("When the runner fails", func() {
			boom := errors.New("runner boom")

			failing := runnerFunc(func(context.Context, hostcli.Binary, []string, []byte) ([]byte, error) {
				return []byte("partial"), boom
			})

			out, err := bin.RunWith(t.Context(), failing, nil, nil)

			Convey("Then stdout and the error pass through unchanged", func() {
				So(string(out), ShouldEqual, "partial")
				So(errors.Is(err, boom), ShouldBeTrue)
			})
		})

		Convey("When the context is canceled", func() {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			called := false

			fake := runnerFunc(func(context.Context, hostcli.Binary, []string, []byte) ([]byte, error) {
				called = true

				return nil, nil
			})

			_, err := bin.RunWith(ctx, fake, nil, nil)

			Convey("Then the cancellation surfaces before the runner runs", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(called, ShouldBeFalse)
			})
		})
	})
}

func TestScriptRunner(t *testing.T) {
	bin := hostcli.Binary{Name: "claude", Path: "/resolved/claude"}

	Convey("Given a scripted runner", t, func() {
		boom := errors.New("scripted boom")

		runner := hostcli.NewScriptRunner(map[string]hostcli.Response{
			"claude plugin list": {Stdout: []byte("list-ok")},
			"claude fail":        {Stdout: []byte("partial"), Stderr: "denied", Code: 2},
			"claude broken":      {Err: boom},
		})

		Convey("When a keyed call is scripted", func() {
			out, err := runner.Run(t.Context(), bin, []string{"plugin", "list"}, nil)

			Convey("Then the response comes back and the call is recorded", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "list-ok")
				So(runner.Calls(), ShouldResemble, []hostcli.Call{
					{Binary: "claude", Args: []string{"plugin", "list"}},
				})
			})
		})

		Convey("When the argv does not match the key", func() {
			_, err := runner.Run(t.Context(), bin, []string{"plugin", "list", "--json"}, nil)

			exitErr, ok := errors.AsType[*hostcli.ExitError](err)

			Convey("Then the unmocked call fails loudly with code 127", func() {
				So(ok, ShouldBeTrue)
				So(exitErr.Code, ShouldEqual, 127)
				So(exitErr.Stderr, ShouldEqual, "no scripted response")
				So(errors.Is(err, hostcli.ErrNotFound), ShouldBeFalse)
			})
		})

		Convey("When the call carries stdin", func() {
			_, err := runner.Run(t.Context(), bin, []string{"plugin", "list"}, []byte("payload"))

			Convey("Then the stdin bytes are recorded", func() {
				So(err, ShouldBeNil)
				So(runner.Calls()[0].Stdin, ShouldResemble, []byte("payload"))
			})
		})

		Convey("When the response has a non-zero exit code", func() {
			out, err := runner.Run(t.Context(), bin, []string{"fail"}, nil)

			exitErr, ok := errors.AsType[*hostcli.ExitError](err)

			Convey("Then stdout and the exit details come back", func() {
				So(ok, ShouldBeTrue)
				So(string(out), ShouldEqual, "partial")
				So(exitErr.Code, ShouldEqual, 2)
				So(exitErr.Stderr, ShouldEqual, "denied")
				So(exitErr.Name, ShouldEqual, "claude")
			})
		})

		Convey("When the response carries an explicit error", func() {
			_, err := runner.Run(t.Context(), bin, []string{"broken"}, nil)

			Convey("Then that error is returned", func() {
				So(errors.Is(err, boom), ShouldBeTrue)
			})
		})

		Convey("When Set adds a response later", func() {
			runner.Set("claude later", hostcli.Response{Stdout: []byte("late")})

			out, err := runner.Run(t.Context(), bin, []string{"later"}, nil)

			Convey("Then the new key is scripted", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, "late")
			})
		})

		Convey("When the context is canceled", func() {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := runner.Run(ctx, bin, []string{"plugin", "list"}, nil)

			Convey("Then the cancellation surfaces", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})

		Convey("When the returned calls are mutated", func() {
			_, err := runner.Run(t.Context(), bin, []string{"plugin", "list"}, []byte("payload"))
			So(err, ShouldBeNil)

			calls := runner.Calls()
			calls[0].Args[0] = "mutated"
			calls[0].Stdin[0] = 'X'

			Convey("Then the recorded calls are unaffected", func() {
				So(runner.Calls()[0].Args[0], ShouldEqual, "plugin")
				So(runner.Calls()[0].Stdin, ShouldResemble, []byte("payload"))
			})
		})
	})
}

func TestScriptRunnerUnscripted(t *testing.T) {
	bin := hostcli.Binary{Name: "claude", Path: "/resolved/claude"}

	Convey("Given a scripted runner with no script", t, func() {
		runner := hostcli.NewScriptRunner(nil)

		Convey("When any call runs", func() {
			_, err := runner.Run(t.Context(), bin, nil, nil)

			exitErr, ok := errors.AsType[*hostcli.ExitError](err)

			Convey("Then it fails with the unmocked-call error", func() {
				So(ok, ShouldBeTrue)
				So(exitErr.Code, ShouldEqual, 127)
			})
		})
	})

	Convey("Given a scripted runner used concurrently", t, func() {
		const (
			workers = 16
			perCall = 5
		)

		runner := hostcli.NewScriptRunner(map[string]hostcli.Response{
			"claude plugin list": {Stdout: []byte("ok")},
		})

		Convey("When many goroutines call it", func() {
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
			)

			for range workers {
				wg.Go(func() {
					for range perCall {
						out, err := runner.Run(t.Context(), bin, []string{"plugin", "list"}, []byte("in"))

						mu.Lock()

						if err != nil {
							errs = append(errs, err)
						} else if string(out) != "ok" {
							errs = append(errs, errors.New("unexpected stdout: "+string(out)))
						}

						mu.Unlock()
					}
				})
			}

			wg.Wait()

			Convey("Then every call is scripted and recorded under the race detector", func() {
				So(errs, ShouldBeEmpty)
				So(runner.Calls(), ShouldHaveLength, workers*perCall)
			})
		})
	})
}
