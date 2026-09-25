package secret

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// testService pins that the keychain service name is a parameter, not a
// constant: "beadle" also documents the vault-embedded value.
const testService = "beadle"

// runnerFunc adapts a function to Runner.
type runnerFunc func(name string, args []string, stdin []byte) ([]byte, int, error)

func (f runnerFunc) Run(name string, args []string, stdin []byte) ([]byte, int, error) {
	return f(name, args, stdin)
}

// testShellKeyring builds a shellKeyring with an injected lookPath so the
// real tool is never resolved.
func testShellKeyring(t *testing.T, tool, service string, runner Runner) *shellKeyring {
	t.Helper()

	notFound := 0

	switch tool {
	case darwinTool:
		notFound = darwinMissing
	case linuxTool:
		notFound = linuxMissing
	default:
		t.Fatalf("unknown test tool %q", tool)
	}

	return &shellKeyring{
		runner:   runner,
		tool:     tool,
		service:  service,
		notFound: notFound,
		lookPath: func(string) (string, error) { return "/usr/bin/" + tool, nil },
	}
}

// keyringCall is one scripted invocation of the keyring tool.
type keyringCall struct {
	args  []string
	stdin []byte
}

// recordingRunner records calls and replies through reply.
func recordingRunner(t *testing.T, calls *[]keyringCall, reply func(call keyringCall) ([]byte, int, error)) Runner {
	t.Helper()

	return runnerFunc(func(name string, args []string, stdin []byte) ([]byte, int, error) {
		call := keyringCall{args: args, stdin: stdin}

		*calls = append(*calls, call)

		if name != darwinTool && name != linuxTool {
			t.Errorf("unexpected tool %q", name)
		}

		return reply(call)
	})
}

func exitError() error {
	return &exec.ExitError{}
}

func TestEncodeDecodePayload(t *testing.T) {
	Convey("Given a table of payload values", t, func() {
		cases := []struct {
			name  string
			value string
		}{
			{"plain", "s3cr3t"},
			{"empty", ""},
			{"newlines", "line1\nline2\n"},
			{"unicode", "s3cr3t\nPEM line \u2603"},
			{"oversized", strings.Repeat("A", 1<<20)},
		}

		for _, tc := range cases {
			Convey("When "+tc.name+" round-trips", func() {
				encoded := encodePayload(tc.value)

				decoded, err := decodePayload(encoded)

				Convey("Then it is v1-prefixed hex and decodes back", func() {
					So(strings.HasPrefix(encoded, payloadPrefix), ShouldBeTrue)

					if tc.value != "" {
						So(encoded, ShouldNotContainSubstring, tc.value)
					}

					So(err, ShouldBeNil)
					So(decoded, ShouldEqual, tc.value)
				})
			})
		}

		Convey("When a foreign raw value is decoded", func() {
			decoded, err := decodePayload("hand-added password")

			Convey("Then it is returned as-is", func() {
				So(err, ShouldBeNil)
				So(decoded, ShouldEqual, "hand-added password")
			})
		})

		Convey("When a corrupt payload is decoded", func() {
			_, err := decodePayload(payloadPrefix + "zz")

			Convey("Then ErrCorruptPayload is reported", func() {
				So(errors.Is(err, ErrCorruptPayload), ShouldBeTrue)
			})
		})
	})
}

func TestKeyringPlatformSwitch(t *testing.T) {
	Convey("Given a table of GOOS values", t, func() {
		cases := []struct {
			goos     string
			tool     string
			notFound int
			ok       bool
		}{
			{"darwin", darwinTool, darwinMissing, true},
			{"linux", linuxTool, linuxMissing, true},
			{"windows", "", 0, false},
			{"freebsd", "", 0, false},
			{"", "", 0, false},
		}

		for _, tc := range cases {
			Convey("When the platform is "+tc.goos, func() {
				tool, notFound, ok := keyringPlatform(tc.goos)

				Convey("Then the mapping matches the port", func() {
					So(ok, ShouldEqual, tc.ok)
					So(tool, ShouldEqual, tc.tool)
					So(notFound, ShouldEqual, tc.notFound)
				})
			})
		}
	})
}

func TestKeyringSupportedPlatforms(t *testing.T) {
	Convey("Given the host platform", t, func() {
		Convey("When a shell keyring is constructed", func() {
			keyring, err := NewShellKeyring(ExecRunner{}, "")

			Convey("Then it is supported on darwin and linux only", func() {
				switch runtime.GOOS {
				case "darwin", "linux":
					So(err, ShouldBeNil)
					So(keyring, ShouldNotBeNil)

					shell, ok := keyring.(*shellKeyring)
					So(ok, ShouldBeTrue)
					So(shell.service, ShouldEqual, DefaultService)
				default:
					So(errors.Is(err, ErrKeyringUnsupported), ShouldBeTrue)
				}
			})
		})
	})
}

func TestExecRunner(t *testing.T) {
	Convey("Given a fixture script that prints to stdout and stderr", t, func() {
		dir := t.TempDir()
		script := filepath.Join(dir, "fixture.sh")

		body := "#!/bin/sh\nprintf 'out\\n'\nprintf 'boom\\n' >&2\nexit 3\n"

		if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // G306: an executable fixture needs the exec bit
			t.Fatalf("write fixture: %v", err)
		}

		Convey("When it exits non-zero", func() {
			stdout, code, err := ExecRunner{}.Run(script, nil, nil)

			Convey("Then stdout, the exit code and a wrapping error are returned", func() {
				So(string(stdout), ShouldEqual, "out\n")
				So(code, ShouldEqual, 3)
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "boom")
			})
		})
	})

	Convey("Given a missing binary", t, func() {
		Convey("When it is run", func() {
			_, code, err := ExecRunner{}.Run(filepath.Join(t.TempDir(), "nope"), nil, nil)

			Convey("Then a negative code and an error are returned", func() {
				So(code, ShouldEqual, -1)
				So(err, ShouldBeError)
			})
		})
	})
}

func TestKeyringGetFoundDarwin(t *testing.T) {
	Convey("Given a macOS keyring returning an encoded payload", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return []byte(encodePayload("s3cr3t") + "\n"), 0, nil
		}))

		Convey("When the value is fetched", func() {
			value, found, err := keyring.Get("ALPHA")

			Convey("Then it decodes and uses find-generic-password with the configured service", func() {
				So(err, ShouldBeNil)
				So(found, ShouldBeTrue)
				So(value, ShouldEqual, "s3cr3t")
				So(calls[0].args, ShouldResemble, []string{"find-generic-password", "-s", testService, "-a", "ALPHA", "-w"})
				So(calls[0].stdin, ShouldBeNil)
			})
		})
	})
}

func TestKeyringGetFoundLinux(t *testing.T) {
	Convey("Given a linux keyring returning a payload with a newline", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, linuxTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return []byte(encodePayload("pem\nline") + "\n"), 0, nil
		}))

		Convey("When the value is fetched", func() {
			value, found, err := keyring.Get("ALPHA")

			Convey("Then only the tool's trailing newline is trimmed", func() {
				So(err, ShouldBeNil)
				So(found, ShouldBeTrue)
				So(value, ShouldEqual, "pem\nline")
				So(calls[0].args, ShouldResemble, []string{"lookup", "service", testService, "account", "ALPHA"})
			})
		})
	})
}

func TestKeyringGetNotFound(t *testing.T) {
	Convey("Given a keyring that reports a missing item", t, func() {
		for tool, code := range map[string]int{darwinTool: darwinMissing, linuxTool: linuxMissing} {
			var calls []keyringCall

			keyring := testShellKeyring(t, tool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
				return nil, code, exitError()
			}))

			Convey("When the value is fetched from "+tool, func() {
				value, found, err := keyring.Get("ALPHA")

				Convey("Then it reports absence without error", func() {
					So(err, ShouldBeNil)
					So(found, ShouldBeFalse)
					So(value, ShouldBeEmpty)
				})
			})
		}
	})
}

func TestKeyringGetFailure(t *testing.T) {
	Convey("Given a keyring that fails with an unrelated error", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return nil, 36, errors.New("security: SecKeychainSearchCopyNext: The specified item could not be found")
		}))

		Convey("When the value is fetched", func() {
			_, _, err := keyring.Get("ALPHA")

			Convey("Then the error is reported", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "keyring get ALPHA")
			})
		})
	})
}

func TestKeyringSetUsesHexTransport(t *testing.T) {
	Convey("Given a macOS keyring", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return nil, 0, nil
		}))

		value := "line1\nline2 $ecret"

		Convey("When a value is stored", func() {
			So(keyring.Set("ALPHA", value), ShouldBeNil)
			So(calls, ShouldHaveLength, 1)

			payload := calls[0].args[len(calls[0].args)-1]

			decoded, err := hex.DecodeString(payload)

			Convey("Then it is hex-encoded and never reaches argv in plaintext", func() {
				So(calls[0].args, ShouldResemble, []string{"add-generic-password", "-U", "-s", testService, "-a", "ALPHA", "-X", payload})
				So(calls[0].stdin, ShouldBeNil)

				So(err, ShouldBeNil)
				So(string(decoded), ShouldEqual, encodePayload(value))
				So(string(decoded), ShouldEqual, payloadPrefix+hex.EncodeToString([]byte(value)))

				for _, arg := range calls[0].args {
					So(arg, ShouldNotContainSubstring, value)
					So(arg, ShouldNotContainSubstring, "line1")
				}
			})
		})
	})
}

func TestKeyringSetUsesStdinOnLinux(t *testing.T) {
	Convey("Given a linux keyring", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, linuxTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return nil, 0, nil
		}))

		value := "line1\nline2 $ecret"

		Convey("When a value is stored", func() {
			So(keyring.Set("ALPHA", value), ShouldBeNil)
			So(calls, ShouldHaveLength, 1)

			Convey("Then the payload goes over stdin and never in plaintext", func() {
				So(calls[0].args, ShouldResemble, []string{"store", "--label=" + testService + ": ALPHA", "service", testService, "account", "ALPHA"})
				So(string(calls[0].stdin), ShouldEqual, encodePayload(value))
				So(string(calls[0].stdin), ShouldContainSubstring, payloadPrefix)
				So(string(calls[0].stdin), ShouldNotContainSubstring, value)
			})
		})
	})
}

func TestKeyringLinuxRoundTripKeepsTrailingNewline(t *testing.T) {
	Convey("Given a linux keyring that stores stdin verbatim", t, func() {
		value := "line1\nline2\n"

		var stored []byte

		keyring := testShellKeyring(t, linuxTool, testService, runnerFunc(func(_ string, args []string, stdin []byte) ([]byte, int, error) {
			if args[0] == "store" {
				stored = append([]byte(nil), stdin...)

				return nil, 0, nil
			}

			return append(append([]byte(nil), stored...), '\n'), 0, nil
		}))

		Convey("When a value ending with a newline round-trips", func() {
			So(keyring.Set("ALPHA", value), ShouldBeNil)
			So(string(stored), ShouldEqual, encodePayload(value))

			got, found, err := keyring.Get("ALPHA")

			Convey("Then the trailing newline survives", func() {
				So(err, ShouldBeNil)
				So(found, ShouldBeTrue)
				So(got, ShouldEqual, value)
			})
		})
	})
}

func TestKeyringSetFailure(t *testing.T) {
	Convey("Given a keyring whose write is denied", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return nil, 1, errors.New("security: write denied")
		}))

		Convey("When a value is stored", func() {
			err := keyring.Set("ALPHA", "s3cr3t")

			Convey("Then the error is reported without the value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "keyring set ALPHA")
				So(err.Error(), ShouldNotContainSubstring, "s3cr3t")
			})
		})
	})
}

func TestKeyringDeleteRemovesEntry(t *testing.T) {
	Convey("Given a keyring where delete succeeds and the read-back misses", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(call keyringCall) ([]byte, int, error) {
			if call.args[0] == "delete-generic-password" {
				return nil, 0, nil
			}

			return nil, darwinMissing, exitError()
		}))

		Convey("When the entry is deleted", func() {
			removed, err := keyring.Delete("ALPHA")

			Convey("Then delete is followed by a read-back", func() {
				So(err, ShouldBeNil)
				So(removed, ShouldBeTrue)
				So(calls, ShouldHaveLength, 2)
				So(calls[0].args, ShouldResemble, []string{"delete-generic-password", "-s", testService, "-a", "ALPHA"})
				So(calls[1].args[0], ShouldEqual, "find-generic-password")
			})
		})
	})
}

func TestKeyringDeleteNotFound(t *testing.T) {
	Convey("Given a keyring where the entry is already missing", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return nil, darwinMissing, exitError()
		}))

		Convey("When the entry is deleted", func() {
			removed, err := keyring.Delete("ALPHA")

			Convey("Then absence is reported without error", func() {
				So(err, ShouldBeNil)
				So(removed, ShouldBeFalse)
			})
		})
	})
}

func TestKeyringDeleteReadBackProof(t *testing.T) {
	Convey("Given a keyring whose entry survives delete", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(call keyringCall) ([]byte, int, error) {
			if call.args[0] == "delete-generic-password" {
				return nil, 0, nil
			}

			return []byte("still here\n"), 0, nil
		}))

		Convey("When the entry is deleted", func() {
			removed, err := keyring.Delete("ALPHA")

			Convey("Then the read-back reports it still present", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "still present")
				So(removed, ShouldBeFalse)
			})
		})
	})
}

func TestKeyringGetReturnsForeignRawValue(t *testing.T) {
	Convey("Given a keyring item added outside verger", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return []byte("hand-added password\n"), 0, nil
		}))

		Convey("When it is fetched", func() {
			value, found, err := keyring.Get("ALPHA")

			Convey("Then the raw value is returned as-is", func() {
				So(err, ShouldBeNil)
				So(found, ShouldBeTrue)
				So(value, ShouldEqual, "hand-added password")
			})
		})
	})
}

func TestKeyringGetRejectsCorruptPayload(t *testing.T) {
	Convey("Given a keyring holding a corrupt payload", t, func() {
		var calls []keyringCall

		keyring := testShellKeyring(t, darwinTool, testService, recordingRunner(t, &calls, func(keyringCall) ([]byte, int, error) {
			return []byte(payloadPrefix + "zz\n"), 0, nil
		}))

		Convey("When it is fetched", func() {
			_, _, err := keyring.Get("ALPHA")

			Convey("Then the corrupt payload is rejected", func() {
				So(errors.Is(err, ErrCorruptPayload), ShouldBeTrue)
			})
		})
	})
}

func TestKeyringLookPathIsLazy(t *testing.T) {
	Convey("Given a keyring whose tool is missing", t, func() {
		called := 0

		keyring := &shellKeyring{
			runner: runnerFunc(func(string, []string, []byte) ([]byte, int, error) {
				called++

				return nil, 0, nil
			}),
			tool:     darwinTool,
			service:  testService,
			notFound: darwinMissing,
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		}

		Convey("When get, set and delete are called", func() {
			_, _, err := keyring.Get("ALPHA")
			So(errors.Is(err, ErrKeyringUnavailable), ShouldBeTrue)
			So(err.Error(), ShouldContainSubstring, darwinTool)

			So(errors.Is(keyring.Set("ALPHA", "s3cr3t"), ErrKeyringUnavailable), ShouldBeTrue)

			_, err = keyring.Delete("ALPHA")

			Convey("Then the tool is never executed", func() {
				So(errors.Is(err, ErrKeyringUnavailable), ShouldBeTrue)
				So(called, ShouldEqual, 0)
			})
		})
	})
}

func TestKeyringConcurrentUse(t *testing.T) {
	Convey("Given a keyring with a thread-safe runner", t, func() {
		const n = 16

		var (
			mu     sync.Mutex
			stored = map[string]string{}
		)

		keyring := testShellKeyring(t, linuxTool, testService, runnerFunc(func(_ string, args []string, stdin []byte) ([]byte, int, error) {
			mu.Lock()
			defer mu.Unlock()

			switch args[0] {
			case "store":
				stored[args[len(args)-1]] = string(stdin)

				return nil, 0, nil
			case "lookup":
				payload, ok := stored[args[len(args)-1]]
				if !ok {
					return nil, linuxMissing, exitError()
				}

				return []byte(payload), 0, nil
			default:
				delete(stored, args[len(args)-1])

				return nil, 0, nil
			}
		}))

		Convey("When many goroutines set and get distinct accounts", func() {
			var (
				wg   sync.WaitGroup
				errs []error
			)

			for i := range n {
				wg.Go(func() {
					account := fmt.Sprintf("ACCOUNT_%d", i)

					if err := keyring.Set(account, encodePayload(account)); err != nil {
						mu.Lock()
						defer mu.Unlock()

						errs = append(errs, err)

						return
					}

					value, found, err := keyring.Get(account)
					if err != nil {
						mu.Lock()
						defer mu.Unlock()

						errs = append(errs, err)

						return
					}

					if !found || value != encodePayload(account) {
						mu.Lock()
						defer mu.Unlock()

						errs = append(errs, fmt.Errorf("account %s: found=%v value=%q", account, found, value))
					}
				})
			}

			wg.Wait()

			Convey("Then every round-trip succeeds under the race detector", func() {
				So(errs, ShouldBeEmpty)
			})
		})
	})
}

// ---- keyring-backed Store ---------------------------------------------------

type fakeKeyring struct {
	values     map[string]string
	gets       int
	sets       int
	deletes    int
	failGet    error
	failSet    error
	failDelete error
}

func newFakeKeyring(values map[string]string) *fakeKeyring {
	if values == nil {
		values = map[string]string{}
	}

	return &fakeKeyring{values: values}
}

func (f *fakeKeyring) Get(account string) (string, bool, error) {
	f.gets++

	if f.failGet != nil {
		return "", false, f.failGet
	}

	value, ok := f.values[account]

	return value, ok, nil
}

func (f *fakeKeyring) Set(account, value string) error {
	f.sets++

	if f.failSet != nil {
		return f.failSet
	}

	f.values[account] = value

	return nil
}

func (f *fakeKeyring) Delete(account string) (bool, error) {
	f.deletes++

	if f.failDelete != nil {
		return false, f.failDelete
	}

	if _, ok := f.values[account]; !ok {
		return false, nil
	}

	delete(f.values, account)

	return true, nil
}

// recordingKeyring records the accounts of every keyring operation.
type recordingKeyring struct {
	gets    []string
	sets    []string
	deletes []string
}

func (r *recordingKeyring) Get(account string) (string, bool, error) {
	r.gets = append(r.gets, account)

	return "", false, nil
}

func (r *recordingKeyring) Set(account, value string) error {
	r.sets = append(r.sets, account)

	return nil
}

func (r *recordingKeyring) Delete(account string) (bool, error) {
	r.deletes = append(r.deletes, account)

	return false, nil
}

// keyringIndex writes a version-2 name index with 0600 under path.
func keyringIndex(t *testing.T, path string, names ...string) {
	t.Helper()

	secrets := map[string]string{}

	for _, name := range names {
		secrets[name] = ""
	}

	encoded, err := json.Marshal(map[string]any{"version": 2, "backend": "keyring", "secrets": secrets})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}

	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// keyringStore returns a temp index path and a store backed by fake.
func keyringStore(t *testing.T, fake Keyring, names ...string) (string, *Store) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state", "secrets.json")
	keyringIndex(t, path, names...)

	store, err := Load(path, WithKeyring(fake), WithService(testService))
	if err != nil {
		t.Fatalf("load store: %v", err)
	}

	return path, store
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: tests read their own temp files
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

func TestStoreServiceNameSwitch(t *testing.T) {
	Convey("Given a store with an explicit service", t, func() {
		store, err := Load(filepath.Join(t.TempDir(), "secrets.json"), WithService("beadle"))
		So(err, ShouldBeNil)

		Convey("When the shell keyring is materialized", func() {
			keyring, keyringErr := store.shellKeyring()

			shell, ok := keyring.(*shellKeyring)

			Convey("Then the service name is carried into it", func() {
				So(keyringErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(shell.service, ShouldEqual, "beadle")
				So(shell.tool, ShouldEqual, platformTool())
			})
		})
	})

	Convey("Given a store with the default service", t, func() {
		store, err := Load(filepath.Join(t.TempDir(), "secrets.json"))
		So(err, ShouldBeNil)

		Convey("When the shell keyring is materialized", func() {
			keyring, keyringErr := store.shellKeyring()

			shell, ok := keyring.(*shellKeyring)

			Convey("Then it is the standalone verger service", func() {
				So(keyringErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(shell.service, ShouldEqual, DefaultService)
			})
		})
	})
}

// platformTool returns the tool of the current GOOS, "" when unsupported.
func platformTool() string {
	tool, _, ok := keyringPlatform(runtime.GOOS)
	if !ok {
		return ""
	}

	return tool
}

func TestKeyringStorePrefetch(t *testing.T) {
	Convey("Given a keyring index with one prefetched secret", t, func() {
		fake := newFakeKeyring(map[string]string{"ALPHA": "s3cr3t"})
		_, store := keyringStore(t, fake, "ALPHA")

		Convey("When the store is used", func() {
			value, ok := store.Get("ALPHA")

			Convey("Then the keyring backend is active and the value is available", func() {
				So(store.Backend(), ShouldEqual, BackendKeyring)
				So(fake.gets, ShouldEqual, 1)
				So(store.KeyringErr(), ShouldBeNil)
				So(store.Probe(), ShouldBeNil)

				So(ok, ShouldBeTrue)
				So(value, ShouldEqual, "s3cr3t")
			})
		})
	})
}

func TestKeyringStorePrefetchFailure(t *testing.T) {
	Convey("Given a keyring that fails to preload", t, func() {
		fake := newFakeKeyring(nil)
		fake.failGet = errors.New("keyring is locked")

		_, store := keyringStore(t, fake, "ALPHA")

		Convey("When the store is used", func() {
			value, ok := store.Get("ALPHA")

			Convey("Then the failure surfaces and the store stays empty", func() {
				So(errors.Is(store.KeyringErr(), fake.failGet), ShouldBeTrue)
				So(errors.Is(store.Probe(), fake.failGet), ShouldBeTrue)

				So(ok, ShouldBeFalse)
				So(value, ShouldBeEmpty)
				So(store.Names(), ShouldBeEmpty)
			})
		})
	})
}

func TestKeyringStoreV1BackCompat(t *testing.T) {
	Convey("Given a v1 secrets document", t, func() {
		path := filepath.Join(t.TempDir(), "state", "secrets.json")

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(path, []byte(`{"version": 1, "secrets": {"ALPHA": "s3cr3t"}}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		fake := newFakeKeyring(nil)

		store, err := Load(path, WithKeyring(fake))
		So(err, ShouldBeNil)

		Convey("When a value is added and saved", func() {
			store.Set("BETA", "fresh")
			So(store.Save(), ShouldBeNil)

			raw := readFile(t, path)

			Convey("Then the file backend stays active and keeps writing v1", func() {
				So(store.Backend(), ShouldEqual, BackendFile)
				So(fake.gets, ShouldEqual, 0)
				So(store.Probe(), ShouldBeNil)

				So(raw, ShouldContainSubstring, `"version": 1`)
				So(raw, ShouldContainSubstring, "s3cr3t")
				So(raw, ShouldNotContainSubstring, `"backend"`)
			})
		})
	})
}

func TestKeyringStoreUnknownBackend(t *testing.T) {
	Convey("Given a secrets document with an unknown backend", t, func() {
		path := filepath.Join(t.TempDir(), "secrets.json")
		keyringIndex(t, path, "ALPHA")

		raw := readFile(t, path)
		raw = strings.Replace(raw, `"keyring"`, `"gpg"`, 1)

		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		Convey("When it is loaded", func() {
			_, err := Load(path)

			target, ok := errors.AsType[*SecretParseError](err)

			Convey("Then it is rejected with a SecretParseError", func() {
				So(err, ShouldBeError)
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, path)
				So(err.Error(), ShouldContainSubstring, "unknown backend")
			})
		})
	})
}

func TestKeyringStoreBuffersUntilSave(t *testing.T) {
	Convey("Given a keyring-backed store", t, func() {
		fake := newFakeKeyring(map[string]string{"ALPHA": "s3cr3t"})
		path, store := keyringStore(t, fake, "ALPHA")

		store.Set("BETA", "fresh")

		Convey("When the store is saved", func() {
			So(fake.sets, ShouldEqual, 0)
			So(store.Changed(), ShouldBeTrue)

			So(store.Save(), ShouldBeNil)

			raw := readFile(t, path)
			So(raw, ShouldNotContainSubstring, "fresh")
			So(raw, ShouldNotContainSubstring, "s3cr3t")

			reloaded, err := Load(path, WithKeyring(fake), WithService(testService))
			So(err, ShouldBeNil)

			value, ok := reloaded.Get("BETA")

			Convey("Then values reach the keyring, the index holds names only and reload sees the value", func() {
				So(fake.sets, ShouldEqual, 1)
				So(fake.values["BETA"], ShouldEqual, "fresh")
				So(raw, ShouldContainSubstring, `"backend": "keyring"`)
				So(raw, ShouldContainSubstring, `"ALPHA": ""`)
				So(raw, ShouldContainSubstring, `"BETA": ""`)
				So(store.Changed(), ShouldBeFalse)

				So(ok, ShouldBeTrue)
				So(value, ShouldEqual, "fresh")
			})
		})
	})
}

func TestKeyringStoreDeleteOnSave(t *testing.T) {
	Convey("Given a keyring-backed store with two secrets", t, func() {
		fake := newFakeKeyring(map[string]string{"ALPHA": "s3cr3t", "BETA": "other"})
		path, store := keyringStore(t, fake, "ALPHA", "BETA")

		So(store.Delete("ALPHA"), ShouldBeTrue)

		Convey("When the store is saved", func() {
			So(fake.deletes, ShouldEqual, 0)

			So(store.Save(), ShouldBeNil)

			_, ok := fake.values["ALPHA"]

			raw := readFile(t, path)

			Convey("Then the keyring entry is removed and the index drops the name", func() {
				So(fake.deletes, ShouldEqual, 1)
				So(ok, ShouldBeFalse)
				So(raw, ShouldNotContainSubstring, `"ALPHA"`)
				So(raw, ShouldContainSubstring, `"BETA"`)
			})
		})
	})
}

func TestKeyringStoreDeleteFailureKeepsIndex(t *testing.T) {
	Convey("Given a keyring whose delete fails", t, func() {
		fake := newFakeKeyring(map[string]string{"ALPHA": "s3cr3t"})
		path, store := keyringStore(t, fake, "ALPHA")

		So(store.Delete("ALPHA"), ShouldBeTrue)

		fake.failDelete = errors.New("delete denied")

		before := readFile(t, path)

		Convey("When save fails and then the keyring heals", func() {
			err := store.Save()
			So(err, ShouldBeError)
			So(err.Error(), ShouldContainSubstring, "remove secrets from the keyring")
			So(readFile(t, path), ShouldEqual, before)
			So(err.Error(), ShouldNotContainSubstring, "s3cr3t")

			fake.failDelete = nil

			Convey("Then retrying is idempotent and drops the name", func() {
				So(store.Save(), ShouldBeNil)
				So(readFile(t, path), ShouldNotContainSubstring, `"ALPHA"`)
			})
		})
	})
}

func TestKeyringStoreSaveFailureKeepsIndex(t *testing.T) {
	Convey("Given a keyring whose write fails", t, func() {
		fake := newFakeKeyring(map[string]string{"ALPHA": "s3cr3t"})
		path, store := keyringStore(t, fake, "ALPHA")

		store.Set("BETA", "fresh")

		fake.failSet = errors.New("write denied")

		before := readFile(t, path)

		Convey("When save fails and then the keyring heals", func() {
			err := store.Save()
			So(err, ShouldBeError)
			So(err.Error(), ShouldContainSubstring, "save secrets to the keyring")
			So(readFile(t, path), ShouldEqual, before)
			So(store.Changed(), ShouldBeTrue)
			So(err.Error(), ShouldNotContainSubstring, "fresh")

			fake.failSet = nil

			Convey("Then retrying stores the value", func() {
				So(store.Save(), ShouldBeNil)
				So(fake.values["BETA"], ShouldEqual, "fresh")
			})
		})
	})
}

func TestKeyringStoreSaveRefusedAfterPrefetchFailure(t *testing.T) {
	Convey("Given a store whose prefetch failed", t, func() {
		fake := newFakeKeyring(nil)
		fake.failGet = errors.New("keyring is locked")

		path, store := keyringStore(t, fake, "ALPHA", "BETA")

		before := readFile(t, path)

		store.Set("GAMMA", "fresh")

		Convey("When it is saved", func() {
			err := store.Save()

			Convey("Then the save is refused and the index is never truncated", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "save secrets to the keyring")
				So(readFile(t, path), ShouldEqual, before)
				So(readFile(t, path), ShouldContainSubstring, `"ALPHA"`)
			})
		})
	})
}

func TestKeyringStoreMigratesBothWays(t *testing.T) {
	Convey("Given a v1 file-backed secrets document", t, func() {
		path := filepath.Join(t.TempDir(), "state", "secrets.json")

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(path, []byte(`{"version": 1, "secrets": {"ALPHA": "s3cr3t"}}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		fake := newFakeKeyring(nil)

		store, err := Load(path, WithKeyring(fake), WithService(testService))
		So(err, ShouldBeNil)

		Convey("When it migrates to keyring and back to file", func() {
			So(store.SetBackend(BackendKeyring), ShouldBeNil)
			So(store.Probe(), ShouldBeNil)
			So(store.Save(), ShouldBeNil)

			So(fake.values["ALPHA"], ShouldEqual, "s3cr3t")

			raw := readFile(t, path)
			So(raw, ShouldContainSubstring, `"backend": "keyring"`)
			So(raw, ShouldNotContainSubstring, "s3cr3t")

			So(store.SetBackend(BackendFile), ShouldBeNil)
			So(store.Save(), ShouldBeNil)

			raw = readFile(t, path)

			Convey("Then values move back and unknown backends are rejected", func() {
				So(raw, ShouldContainSubstring, "s3cr3t")
				So(raw, ShouldNotContainSubstring, `"backend"`)

				So(store.SetBackend(BackendFile), ShouldBeNil)

				err := store.SetBackend("gpg")
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "unknown secrets backend")
			})
		})
	})
}

func TestKeyringStoreNamesStayStable(t *testing.T) {
	Convey("Given a keyring-backed store with a value", t, func() {
		fake := newFakeKeyring(map[string]string{"TOKEN": "v1"})
		_, store := keyringStore(t, fake, "TOKEN")

		name := store.nameFor("token", "v1")
		So(name, ShouldEqual, "TOKEN")

		store.Set(name, "v1")
		So(store.Save(), ShouldBeNil)

		Convey("When names are queried again", func() {
			second := store.nameFor("token", "v2")

			Convey("Then the first name is stable and the collision is fingerprinted", func() {
				So(store.Names(), ShouldResemble, []string{"TOKEN"})
				So(store.nameFor("token", "v1"), ShouldEqual, "TOKEN")
				So(second, ShouldEqual, "TOKEN_"+Fingerprint("v2"))
			})
		})
	})
}

func TestKeyringStoreExtractionRoundTrip(t *testing.T) {
	Convey("Given an empty keyring-backed store", t, func() {
		fake := newFakeKeyring(nil)
		path, store := keyringStore(t, fake)

		source := []byte("token=s3cr3tvalue123\n")

		extracted, names, changed, err := ExtractText(source, store)
		So(err, ShouldBeNil)

		Convey("When the extraction is saved and resolved back", func() {
			So(changed, ShouldBeTrue)
			So(names, ShouldHaveLength, 1)
			So(string(extracted), ShouldContainSubstring, Ref(names[0]))
			So(fake.sets, ShouldEqual, 0)

			So(store.Save(), ShouldBeNil)
			So(fake.sets, ShouldEqual, 1)
			So(fake.values[names[0]], ShouldEqual, "s3cr3tvalue123")

			reloaded, err := Load(path, WithKeyring(fake), WithService(testService))
			So(err, ShouldBeNil)

			resolved, missing := ResolveText(extracted, reloaded)

			Convey("Then the source round-trips", func() {
				So(missing, ShouldBeEmpty)
				So(string(resolved), ShouldEqual, string(source))
			})
		})
	})
}

func TestKeyringProbe(t *testing.T) {
	Convey("Given a keyring-backed store", t, func() {
		rec := &recordingKeyring{}
		store, err := Load(filepath.Join(t.TempDir(), "secrets.json"), WithKeyring(rec), WithService(testService))
		So(err, ShouldBeNil)
		So(store.SetBackend(BackendKeyring), ShouldBeNil)

		Convey("When the keychain is probed", func() {
			probeErr := store.Probe()

			Convey("Then one get and one delete run against the probe account, never a real name", func() {
				So(probeErr, ShouldBeNil)
				So(rec.gets, ShouldResemble, []string{testService + "-doctor-probe"})
				So(rec.deletes, ShouldResemble, []string{testService + "-doctor-probe"})
				So(rec.sets, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a file-backed store", t, func() {
		rec := &recordingKeyring{}
		store, err := Load(filepath.Join(t.TempDir(), "secrets.json"), WithKeyring(rec), WithService(testService))
		So(err, ShouldBeNil)

		Convey("When the store is probed", func() {
			Convey("Then the keychain is not touched", func() {
				So(store.Probe(), ShouldBeNil)
				So(rec.gets, ShouldBeEmpty)
				So(rec.deletes, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a keyring whose probe fails", t, func() {
		fake := newFakeKeyring(nil)
		fake.failGet = errors.New("keyring is locked")

		store, err := Load(filepath.Join(t.TempDir(), "secrets.json"), WithKeyring(fake))
		So(err, ShouldBeNil)
		So(store.SetBackend(BackendKeyring), ShouldBeNil)

		Convey("When the store is probed", func() {
			probeErr := store.Probe()

			Convey("Then the failure is surfaced and wrapped", func() {
				So(errors.Is(probeErr, fake.failGet), ShouldBeTrue)
				So(probeErr.Error(), ShouldContainSubstring, "keyring probe")
			})
		})
	})

	Convey("Given a keyring tool that is not installed", t, func() {
		keyring := &shellKeyring{
			runner:   runnerFunc(func(string, []string, []byte) ([]byte, int, error) { return nil, 0, nil }),
			tool:     platformTool(),
			service:  testService,
			notFound: darwinMissing,
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		}

		store, err := Load(filepath.Join(t.TempDir(), "secrets.json"))
		So(err, ShouldBeNil)

		store.keyring = keyring
		store.backend = BackendKeyring

		Convey("When the store is probed", func() {
			probeErr := store.Probe()

			Convey("Then ErrKeyringUnavailable is wrapped", func() {
				So(errors.Is(probeErr, ErrKeyringUnavailable), ShouldBeTrue)
			})
		})
	})
}

func TestKeyringNoSecretValueLeaks(t *testing.T) {
	Convey("Given a table of failing keyring calls carrying a secret", t, func() {
		value := "s3cr3t-value-123456"

		Convey("When a get fails", func() {
			keyring := testShellKeyring(t, darwinTool, testService, runnerFunc(func(string, []string, []byte) ([]byte, int, error) {
				return nil, 1, errors.New("security: read denied")
			}))

			_, _, err := keyring.Get("ALPHA")

			Convey("Then the error carries no value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldNotContainSubstring, value)
			})
		})

		Convey("When a set fails", func() {
			keyring := testShellKeyring(t, darwinTool, testService, runnerFunc(func(string, []string, []byte) ([]byte, int, error) {
				return nil, 1, errors.New("security: write denied")
			}))

			err := keyring.Set("ALPHA", value)

			Convey("Then the error carries no value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldNotContainSubstring, value)
			})
		})

		Convey("When a delete fails", func() {
			keyring := testShellKeyring(t, darwinTool, testService, runnerFunc(func(string, []string, []byte) ([]byte, int, error) {
				return nil, 1, errors.New("security: delete denied")
			}))

			_, err := keyring.Delete("ALPHA")

			Convey("Then the error carries no value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldNotContainSubstring, value)
			})
		})

		Convey("When a keyring-backed save fails", func() {
			fake := newFakeKeyring(nil)
			_, store := keyringStore(t, fake)

			store.Set("ALPHA", value)

			fake.failSet = errors.New("write denied")

			err := store.Save()

			Convey("Then the error carries no value", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldNotContainSubstring, value)
			})
		})
	})
}
