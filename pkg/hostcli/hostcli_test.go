package hostcli_test

import (
	"encoding/json"
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

// script writes an executable script and returns its path.
func script(t *testing.T, dir, name, body string, perm os.FileMode) string {
	t.Helper()

	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}

	return path
}

// fixture returns the absolute path of a testdata script.
func fixture(t *testing.T, name string) string {
	t.Helper()

	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("resolve fixture %s: %v", name, err)
	}

	return abs
}

// missingOnPATH is a lookPath that never finds anything.
func missingOnPATH(name string) (string, error) {
	return "", errors.New(name + ": not found in PATH")
}

func TestResolve(t *testing.T) {
	Convey("Given a CLI on the process PATH", t, func() {
		resolver := hostcli.NewResolver(hostcli.WithLookPath(func(name string) (string, error) {
			return "/usr/bin/" + name, nil
		}))

		Convey("When it resolves", func() {
			bin, err := resolver.Resolve("claude")

			Convey("Then the PATH hit wins and keeps the process environment", func() {
				So(err, ShouldBeNil)
				So(bin, ShouldResemble, hostcli.Binary{Name: "claude", Path: "/usr/bin/claude", Source: hostcli.SourceLookPath})
			})
		})
	})

	Convey("Given a lookPath that returns a relative path", t, func() {
		resolver := hostcli.NewResolver(hostcli.WithLookPath(func(name string) (string, error) {
			return filepath.Join("bin", name), nil
		}))

		Convey("When it resolves", func() {
			bin, err := resolver.Resolve("claude")

			Convey("Then the result is absolute", func() {
				So(err, ShouldBeNil)
				So(filepath.IsAbs(bin.Path), ShouldBeTrue)
				So(strings.HasSuffix(bin.Path, filepath.Join("bin", "claude")), ShouldBeTrue)
			})
		})
	})

	Convey("Given a CLI missing from the PATH but recorded by an attended run", t, func() {
		dir := t.TempDir()
		path := script(t, dir, "claude", "#!/bin/sh\necho ok\n", 0o755)

		resolver := hostcli.NewResolver(
			hostcli.WithLookPath(missingOnPATH),
			hostcli.WithRecords(hostcli.Records{"claude": {Path: path, PATH: "/opt/tools/bin:/usr/bin"}}),
		)

		Convey("When it resolves", func() {
			bin, err := resolver.Resolve("claude")

			Convey("Then the record answers and carries its PATH", func() {
				So(err, ShouldBeNil)
				So(bin, ShouldResemble, hostcli.Binary{
					Name:   "claude",
					Path:   path,
					Source: hostcli.SourceRecorded,
					PATH:   "/opt/tools/bin:/usr/bin",
				})
			})
		})
	})

	Convey("Given a CLI both on the PATH and recorded", t, func() {
		dir := t.TempDir()
		path := script(t, dir, "claude", "#!/bin/sh\n", 0o755)

		resolver := hostcli.NewResolver(
			hostcli.WithLookPath(func(name string) (string, error) { return "/usr/local/bin/" + name, nil }),
			hostcli.WithRecords(hostcli.Records{"claude": {Path: path, PATH: "/recorded"}}),
		)

		Convey("When it resolves", func() {
			bin, err := resolver.Resolve("claude")

			Convey("Then the live PATH hit wins over the record", func() {
				So(err, ShouldBeNil)
				So(bin.Source, ShouldEqual, hostcli.SourceLookPath)
				So(bin.Path, ShouldEqual, "/usr/local/bin/claude")
				So(bin.PATH, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a CLI missing from the PATH and never recorded", t, func() {
		resolver := hostcli.NewResolver(hostcli.WithLookPath(missingOnPATH))

		Convey("When it resolves", func() {
			_, err := resolver.Resolve("claude")

			Convey("Then ErrNotFound names the PATH miss and the missing record", func() {
				So(errors.Is(err, hostcli.ErrNotFound), ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, "PATH")
				So(err.Error(), ShouldContainSubstring, "claude")
			})
		})
	})
}

func TestResolveRecordedRejected(t *testing.T) {
	Convey("Given a recorded location that no longer passes Check", t, func() {
		dir := t.TempDir()
		gone := filepath.Join(dir, "gone", "claude")

		resolver := hostcli.NewResolver(
			hostcli.WithLookPath(missingOnPATH),
			hostcli.WithRecords(hostcli.Records{"claude": {Path: gone}}),
		)

		Convey("When it resolves", func() {
			_, err := resolver.Resolve("claude")

			Convey("Then ErrNotFound names the unusable record", func() {
				So(errors.Is(err, hostcli.ErrNotFound), ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, gone)
			})
		})
	})
}

func TestResolveConcurrent(t *testing.T) {
	Convey("Given a resolver used concurrently", t, func() {
		dir := t.TempDir()
		path := script(t, dir, "claude", "#!/bin/sh\n", 0o755)

		resolver := hostcli.NewResolver(
			hostcli.WithLookPath(missingOnPATH),
			hostcli.WithRecords(hostcli.Records{"claude": {Path: path}}),
		)

		Convey("When many goroutines resolve", func() {
			const n = 32

			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				bins []hostcli.Binary
				errs []error
			)

			for range n {
				wg.Go(func() {
					bin, err := resolver.Resolve("claude")

					mu.Lock()
					defer mu.Unlock()

					if err != nil {
						errs = append(errs, err)

						return
					}

					bins = append(bins, bin)
				})
			}

			wg.Wait()

			Convey("Then every goroutine sees the same record", func() {
				So(errs, ShouldBeEmpty)
				So(bins, ShouldHaveLength, n)

				for _, bin := range bins {
					So(bin, ShouldResemble, hostcli.Binary{Name: "claude", Path: path, Source: hostcli.SourceRecorded})
				}
			})
		})
	})
}

func TestCheck(t *testing.T) {
	Convey("Given recorded locations that no longer hold the CLI", t, func() {
		dir := t.TempDir()

		notExecutable := script(t, t.TempDir(), "claude", "#!/bin/sh\n", 0o644)
		otherWritable := script(t, t.TempDir(), "claude", "#!/bin/sh\n", 0o757)

		directory := filepath.Join(t.TempDir(), "claude")
		if err := os.Mkdir(directory, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		tests := []struct {
			name   string
			record hostcli.Record
		}{
			{"relative path", hostcli.Record{Path: filepath.Join("bin", "claude")}},
			{"unclean path", hostcli.Record{Path: t.TempDir() + "/x/../claude"}},
			{"wrong base name", hostcli.Record{Path: script(t, t.TempDir(), "gemini", "#!/bin/sh\n", 0o755)}},
			{"directory", hostcli.Record{Path: directory}},
			{"not executable", hostcli.Record{Path: notExecutable}},
			{"writable by other users", hostcli.Record{Path: otherWritable}},
			{"gone", hostcli.Record{Path: filepath.Join(dir, "missing", "claude")}},
		}

		for _, tt := range tests {
			Convey("When the record is a "+tt.name, func() {
				err := hostcli.Check("claude", tt.record)

				_, resolveErr := hostcli.NewResolver(
					hostcli.WithLookPath(missingOnPATH),
					hostcli.WithRecords(hostcli.Records{"claude": tt.record}),
				).Resolve("claude")

				Convey("Then Check rejects it and Resolve reports not found", func() {
					So(err, ShouldBeError)
					So(errors.Is(resolveErr, hostcli.ErrNotFound), ShouldBeTrue)
				})
			})
		}
	})

	Convey("Given recorded locations that still hold the CLI", t, func() {
		plain := script(t, t.TempDir(), "claude", "#!/bin/sh\n", 0o755)
		empty := script(t, t.TempDir(), "claude", "", 0o755)
		binary := script(t, t.TempDir(), "claude", "\x7fELF not a script\n", 0o755)
		shScript := script(t, t.TempDir(), "claude", "#!/bin/sh\necho hi\n", 0o755)
		envScript := script(t, t.TempDir(), "claude", "#!/usr/bin/env fakenode\necho hi\n", 0o755)
		flagEnvScript := script(t, t.TempDir(), "claude", "#!/usr/bin/env -S fakenode --flag\necho hi\n", 0o755)
		absInterpreter := script(t, t.TempDir(), "claude", "#!/opt/interp fakenode\necho hi\n", 0o755)

		Convey("When no interpreter is involved", func() {
			Convey("Then every location is accepted", func() {
				So(hostcli.Check("claude", hostcli.Record{Path: plain}), ShouldBeNil)
				So(hostcli.Check("claude", hostcli.Record{Path: empty}), ShouldBeNil)
				So(hostcli.Check("claude", hostcli.Record{Path: binary}), ShouldBeNil)
				So(hostcli.Check("claude", hostcli.Record{Path: shScript}), ShouldBeNil)
				So(hostcli.Check("claude", hostcli.Record{Path: absInterpreter}), ShouldBeNil)
			})
		})

		Convey("When the env shebang interpreter is on the recorded PATH", func() {
			bin := t.TempDir()
			script(t, bin, "fakenode", "#!/bin/sh\n", 0o755)

			Convey("Then the record is usable", func() {
				So(hostcli.Check("claude", hostcli.Record{Path: envScript, PATH: "/nowhere:" + bin}), ShouldBeNil)
				So(hostcli.Check("claude", hostcli.Record{Path: flagEnvScript, PATH: bin}), ShouldBeNil)
			})
		})

		Convey("When the env shebang interpreter is missing", func() {
			Convey("Then the record is rejected", func() {
				So(hostcli.Check("claude", hostcli.Record{Path: envScript, PATH: "/nowhere"}), ShouldBeError)
				So(hostcli.Check("claude", hostcli.Record{Path: flagEnvScript, PATH: ""}), ShouldBeError)
			})
		})

		Convey("When the recorded PATH has only relative entries", func() {
			bin := t.TempDir()
			script(t, bin, "fakenode", "#!/bin/sh\n", 0o755)

			cwd, err := os.Getwd()
			So(err, ShouldBeNil)

			rel, err := filepath.Rel(cwd, bin)
			So(err, ShouldBeNil)
			So(filepath.IsAbs(rel), ShouldBeFalse)

			Convey("Then the interpreter is not reached", func() {
				So(hostcli.Check("claude", hostcli.Record{Path: envScript, PATH: rel}), ShouldBeError)
			})
		})

		Convey("When the path is a symlink to an executable", func() {
			target := script(t, t.TempDir(), "real", "#!/bin/sh\n", 0o755)
			link := filepath.Join(t.TempDir(), "claude")

			if err := os.Symlink(target, link); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			Convey("Then the symlink is followed and accepted", func() {
				So(hostcli.Check("claude", hostcli.Record{Path: link}), ShouldBeNil)
			})
		})
	})
}

func TestRecords(t *testing.T) {
	Convey("Given a records file path", t, func() {
		path := filepath.Join(t.TempDir(), "state", "hostcli.json")

		Convey("When nothing was recorded yet", func() {
			records, err := hostcli.LoadRecords(path)

			Convey("Then it loads empty", func() {
				So(err, ShouldBeNil)
				So(records, ShouldBeEmpty)
			})
		})

		Convey("When records are saved and loaded", func() {
			at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
			saved := hostcli.Records{"claude": {Path: "/Users/u/.local/bin/claude", PATH: "/Users/u/.local/bin:/usr/bin", At: at}}

			So(saved.Save(path), ShouldBeNil)

			loaded, err := hostcli.LoadRecords(path)
			So(err, ShouldBeNil)

			info, statErr := os.Stat(path)
			So(statErr, ShouldBeNil)

			dirInfo, dirErr := os.Stat(filepath.Dir(path))
			So(dirErr, ShouldBeNil)

			entries, readErr := os.ReadDir(filepath.Dir(path))
			So(readErr, ShouldBeNil)

			Convey("Then they round-trip, stay private and leave no temp file", func() {
				So(loaded, ShouldResemble, saved)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o600))
				So(dirInfo.Mode().Perm(), ShouldEqual, os.FileMode(0o700))
				So(entries, ShouldHaveLength, 1)
			})
		})

		Convey("When records are saved twice", func() {
			at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

			first := hostcli.Records{"claude": {Path: "/bin/claude", At: at}}
			So(first.Save(path), ShouldBeNil)

			second := hostcli.Records{"gemini": {Path: "/bin/gemini", At: at}}
			So(second.Save(path), ShouldBeNil)

			loaded, err := hostcli.LoadRecords(path)

			Convey("Then the last write wins completely", func() {
				So(err, ShouldBeNil)
				So(loaded, ShouldResemble, second)
			})
		})

		Convey("When the JSON shape is inspected", func() {
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

			So(hostcli.Records{"claude": {Path: "/bin/claude", PATH: "/usr/bin", At: at}}.Save(path), ShouldBeNil)

			raw, readErr := os.ReadFile(path) //nolint:gosec // G304: test reads its own temp file
			So(readErr, ShouldBeNil)

			var decoded map[string]struct {
				Path string    `json:"path"`
				PATH string    `json:"path_env"`
				At   time.Time `json:"at"`
			}

			unmarshalErr := json.Unmarshal(raw, &decoded)

			Convey("Then the documented keys and a trailing newline are used", func() {
				So(unmarshalErr, ShouldBeNil)
				So(decoded["claude"].Path, ShouldEqual, "/bin/claude")
				So(decoded["claude"].PATH, ShouldEqual, "/usr/bin")
				So(decoded["claude"].At.Equal(at), ShouldBeTrue)
				So(strings.HasSuffix(string(raw), "\n"), ShouldBeTrue)
			})
		})
	})
}

func TestRecordsEdges(t *testing.T) {
	Convey("Given a records file path", t, func() {
		path := filepath.Join(t.TempDir(), "state", "hostcli.json")

		Convey("When records hold unicode and JSON-significant bytes", func() {
			saved := hostcli.Records{
				"клод": {Path: "/Users/u/бинарники/claude", PATH: `/bin:$(id):"quoted":зн`},
			}

			So(saved.Save(path), ShouldBeNil)

			loaded, err := hostcli.LoadRecords(path)

			Convey("Then they round-trip byte-exactly", func() {
				So(err, ShouldBeNil)
				So(loaded, ShouldResemble, saved)
			})
		})

		Convey("When empty records are saved", func() {
			So(hostcli.Records{}.Save(path), ShouldBeNil)

			raw, readErr := os.ReadFile(path) //nolint:gosec // G304: test reads its own temp file
			So(readErr, ShouldBeNil)

			loaded, err := hostcli.LoadRecords(path)

			Convey("Then an empty object round-trips", func() {
				So(strings.TrimSpace(string(raw)), ShouldEqual, "{}")
				So(err, ShouldBeNil)
				So(loaded, ShouldBeEmpty)
			})
		})

		Convey("When the file is corrupt", func() {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, err := hostcli.LoadRecords(path)

			target, ok := errors.AsType[*hostcli.RecordsParseError](err)

			Convey("Then a RecordsParseError names the path and cause", func() {
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, path)
				So(target.Cause, ShouldNotBeNil)
			})
		})

		Convey("When the file is JSON but not an object", func() {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, err := hostcli.LoadRecords(path)

			_, ok := errors.AsType[*hostcli.RecordsParseError](err)

			Convey("Then it is a parse error", func() {
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the file is empty", func() {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, err := hostcli.LoadRecords(path)

			_, ok := errors.AsType[*hostcli.RecordsParseError](err)

			Convey("Then it is a parse error, never silently empty", func() {
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the file is a directory", func() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			_, err := hostcli.LoadRecords(path)

			_, parseOK := errors.AsType[*hostcli.RecordsParseError](err)

			Convey("Then the read failure is reported, not a parse error", func() {
				So(err, ShouldBeError)
				So(parseOK, ShouldBeFalse)
			})
		})

		Convey("When the parent path is a regular file", func() {
			blocker := filepath.Join(t.TempDir(), "blocker")
			script(t, filepath.Dir(blocker), filepath.Base(blocker), "not a directory\n", 0o644)

			err := hostcli.Records{"claude": {Path: "/bin/claude"}}.Save(filepath.Join(blocker, "state", "hostcli.json"))

			Convey("Then the write failure names the path", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, blocker)
			})
		})
	})
}

func TestExitError(t *testing.T) {
	Convey("Given an ExitError", t, func() {
		cause := errors.New("exit status 3")
		exitErr := &hostcli.ExitError{Name: "claude", Code: 3, Stderr: "boom", Err: cause}

		Convey("When it renders and unwraps", func() {
			Convey("Then it names the CLI and carries the cause", func() {
				So(exitErr.Error(), ShouldContainSubstring, "claude")
				So(exitErr.Error(), ShouldContainSubstring, "boom")
				So(exitErr.Code, ShouldEqual, 3)
				So(errors.Is(exitErr, cause), ShouldBeTrue)
			})
		})
	})
}
