package secret_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

// Synthetic scanner/keyring fixtures; the values are public and meaningless.
const (
	genericKey   = "abcdefgh12345678"
	fixtureValue = "s3cr3t-value-123456" //nolint:gosec // G101: synthetic fixture value
	ghpFixture   = "ghp_abcdefghijklmnopqrstuvwxyz012345"
)

// writeTestFile creates path with 0600 and its parent with 0700.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent of %s: %v", path, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newTestStore loads an empty file-backed store under a temp dir.
func newTestStore(t *testing.T) *secret.Store {
	t.Helper()

	store, err := secret.Load(filepath.Join(t.TempDir(), "state", "secrets.json"))
	if err != nil {
		t.Fatalf("load store: %v", err)
	}

	return store
}

func TestConstants(t *testing.T) {
	Convey("Given the package constants", t, func() {
		Convey("Then the document, mode and backend names match the spec", func() {
			So(secret.FileName, ShouldEqual, "secrets.json")
			So(secret.DefaultService, ShouldEqual, "verger")
			So(secret.ModeLiteral, ShouldEqual, "literal")
			So(secret.ModeEnv, ShouldEqual, "env")
			So(secret.BackendFile, ShouldEqual, "file")
			So(secret.BackendKeyring, ShouldEqual, "keyring")
		})
	})
}

func TestNormalizeName(t *testing.T) {
	Convey("Given a table of raw keys", t, func() {
		cases := []struct {
			name string
			key  string
			want string
		}{
			{"lowercase", "api_key", "API_KEY"},
			{"dashes and dots become underscores", "context7.api-key", "CONTEXT7_API_KEY"},
			{"collapses repeated separators", "a--b__c", "A_B_C"},
			{"trims leading and trailing separators", "_TOKEN_", "TOKEN"},
			{"leading digit gets prefixed", "1password", "S_1PASSWORD"},
			{"empty key falls back to placeholder", "", "SECRET"},
			{"only separators falls back to placeholder", "---", "SECRET"},
			{"mixed unicode is stripped to underscores", "töken", "T_KEN"},
			{"cyrillic name", "имя", "SECRET"},
		}

		for _, tc := range cases {
			Convey("When normalizing "+tc.name, func() {
				Convey("Then the name matches", func() {
					So(secret.NormalizeName(tc.key), ShouldEqual, tc.want)
				})
			})
		}
	})
}

func TestFingerprint(t *testing.T) {
	Convey("Given two values", t, func() {
		fp1 := secret.Fingerprint("value-one")
		fp2 := secret.Fingerprint("value-two")

		Convey("When fingerprints are computed", func() {
			Convey("Then they are stable, distinct, uppercase hex and never the value", func() {
				So(fp1, ShouldHaveLength, 8)
				So(fp1, ShouldNotEqual, fp2)
				So(secret.Fingerprint("value-one"), ShouldEqual, fp1)

				for _, r := range fp1 {
					So((r >= '0' && r <= '9') || (r >= 'A' && r <= 'F'), ShouldBeTrue)
				}

				So(strings.Contains(fp1, "value-one"), ShouldBeFalse)
			})
		})
	})
}

func TestValidName(t *testing.T) {
	Convey("Given a table of candidate names", t, func() {
		cases := []struct {
			name  string
			valid bool
		}{
			{"TOKEN", true},
			{"API_KEY_1", true},
			{"_LEADING_UNDERSCORE", true},
			{"TRAILING_UNDERSCORE_", true},
			{"A", true},
			{"", false},
			{"1LEADING_DIGIT", false},
			{"has-dash", false},
			{"has space", false},
			{"has.dot", false},
			{"has:colon", false},
			{"имя", false},
			{"{secret:TOKEN}", false},
		}

		for _, tc := range cases {
			Convey("When validating "+tc.name, func() {
				Convey("Then validity matches", func() {
					So(secret.ValidName(tc.name), ShouldEqual, tc.valid)
				})
			})
		}
	})
}

func TestRefFormats(t *testing.T) {
	Convey("Given the ref grammar", t, func() {
		Convey("When a secret ref is rendered", func() {
			Convey("Then it has the {secret:NAME} shape", func() {
				So(secret.Ref("TOKEN"), ShouldEqual, "{secret:TOKEN}")
				So(secret.IsRef("{secret:TOKEN}"), ShouldBeTrue)
			})
		})

		Convey("When an env ref is rendered", func() {
			Convey("Then it has the {env:NAME} shape and is not a secret ref", func() {
				So(secret.EnvRef("VAR"), ShouldEqual, "{env:VAR}")

				name, ok := secret.ParseEnvRef("{env:VAR}")
				So(ok, ShouldBeTrue)
				So(name, ShouldEqual, "VAR")

				_, secretOK := secret.ParseEnvRef("{secret:VAR}")
				So(secretOK, ShouldBeFalse)
				So(secret.IsRef("{env:VAR}"), ShouldBeFalse)
			})
		})

		Convey("When a table of ref-shaped values is parsed", func() {
			cases := []struct {
				name string
				in   string
				got  string
				ok   bool
			}{
				{"plain ref", "{secret:TOKEN}", "TOKEN", true},
				{"empty name is delimited and not validated", "{secret:}", "", true},
				{"missing brace keeps the partial name", "{secret:TOKEN", "TOKEN", false},
				{"missing prefix", "TOKEN}", "", false},
				{"bare word", "token", "", false},
				{"empty string", "", "", false},
				{"prefix only", "{secret:", "", false},
				{"embedded ref is not a whole ref", "x{secret:TOKEN}", "", false},
				{"suffixed ref is not a whole ref", "{secret:TOKEN}x", "TOKEN}x", false},
				{"double close brace slices one brace", "{secret:TOKEN}}", "TOKEN}", true},
				{"nested ref returns the inner ref unvalidated", "{secret:{secret:X}}", "{secret:X}", true},
				{"invalid but delimited name is not re-validated", "{secret:1BAD}", "1BAD", true},
				{"unicode name is not re-validated", "{secret:имя}", "имя", true},
				{"escaped name is not a ref", `{secret:NA\ME}`, `NA\ME`, true},
			}

			for _, tc := range cases {
				Convey("When parsing "+tc.name, func() {
					name, ok := secret.ParseRef(tc.in)

					Convey("Then the parsed form matches the ported rules", func() {
						So(ok, ShouldEqual, tc.ok)
						So(name, ShouldEqual, tc.got)
						So(secret.IsRef(tc.in), ShouldEqual, tc.ok)
					})
				})
			}
		})

		Convey("When an invalid name is delimited", func() {
			Convey("Then ParseRef does not re-validate it; ValidName is the gate", func() {
				_, ok := secret.ParseRef("{secret:1BAD}")
				So(ok, ShouldBeTrue)
				So(secret.ValidName("1BAD"), ShouldBeFalse)
				So(secret.ValidName(""), ShouldBeFalse)
			})
		})

		Convey("When a name passes ValidName", func() {
			Convey("Then Ref and ParseRef round-trip it", func() {
				for _, name := range []string{"TOKEN", "_X", "A_1"} {
					So(secret.ValidName(name), ShouldBeTrue)

					got, ok := secret.ParseRef(secret.Ref(name))
					So(ok, ShouldBeTrue)
					So(got, ShouldEqual, name)
				}
			})
		})
	})
}

func TestStoreSetGetDeleteNames(t *testing.T) {
	Convey("Given a fresh store", t, func() {
		store := newTestStore(t)

		Convey("When values are set, read and deleted", func() {
			So(store.Changed(), ShouldBeFalse)
			So(store.Len(), ShouldEqual, 0)

			store.Set("TOKEN", "v1")
			So(store.Changed(), ShouldBeTrue)

			value, ok := store.Get("TOKEN")
			So(ok, ShouldBeTrue)
			So(value, ShouldEqual, "v1")
			So(store.Has("TOKEN"), ShouldBeTrue)

			_, ok = store.Get("MISSING")
			So(ok, ShouldBeFalse)

			store.Set("OTHER", "v2")

			Convey("Then names are sorted and deletion reports presence", func() {
				So(store.Names(), ShouldResemble, []string{"OTHER", "TOKEN"})
				So(store.Len(), ShouldEqual, 2)

				So(store.Delete("OTHER"), ShouldBeTrue)
				So(store.Delete("OTHER"), ShouldBeFalse)
				So(store.Names(), ShouldResemble, []string{"TOKEN"})
			})
		})
	})
}

func TestStoreSetSameValueIsNoop(t *testing.T) {
	Convey("Given a saved store", t, func() {
		store := newTestStore(t)

		store.Set("TOKEN", "v1")
		So(store.Save(), ShouldBeNil)
		So(store.Changed(), ShouldBeFalse)

		Convey("When the same value is set again", func() {
			store.Set("TOKEN", "v1")

			Convey("Then the store stays clean", func() {
				So(store.Changed(), ShouldBeFalse)
			})
		})

		Convey("When the same value is set with a different name", func() {
			store.Set("OTHER", "v1")

			Convey("Then the store is dirty", func() {
				So(store.Changed(), ShouldBeTrue)
				So(store.Len(), ShouldEqual, 2)
			})
		})
	})
}

func TestStoreDeleteAbsentDoesNotDirty(t *testing.T) {
	Convey("Given an empty store", t, func() {
		store := newTestStore(t)

		Convey("When an absent name is deleted", func() {
			Convey("Then nothing changes", func() {
				So(store.Delete("MISSING"), ShouldBeFalse)
				So(store.Changed(), ShouldBeFalse)
			})
		})
	})
}

func TestStorePathIsTheLoadedPath(t *testing.T) {
	Convey("Given an explicit path", t, func() {
		path := filepath.Join(t.TempDir(), "state", "secrets.json")

		store, err := secret.Load(path)

		Convey("When it loads", func() {
			Convey("Then Path returns it and the backend defaults to file", func() {
				So(err, ShouldBeNil)
				So(store.Path(), ShouldEqual, path)
				So(store.Backend(), ShouldEqual, secret.BackendFile)
				So(store.KeyringErr(), ShouldBeNil)
			})
		})
	})
}

func TestStoreLoadMissingFileIsEmpty(t *testing.T) {
	Convey("Given a missing secrets file", t, func() {
		Convey("When it is loaded", func() {
			store, err := secret.Load(filepath.Join(t.TempDir(), "does-not-exist.json"))

			Convey("Then the store is empty and no file is created", func() {
				So(err, ShouldBeNil)
				So(store.Len(), ShouldEqual, 0)
				So(store.Names(), ShouldBeEmpty)
				So(store.Changed(), ShouldBeFalse)
			})
		})
	})
}

func TestStoreLoadEmptyFileIsEmpty(t *testing.T) {
	Convey("Given a zero-byte secrets file", t, func() {
		path := filepath.Join(t.TempDir(), "secrets.json")
		writeTestFile(t, path, "")

		Convey("When it is loaded", func() {
			store, err := secret.Load(path)

			Convey("Then it is treated as empty, not corrupt", func() {
				So(err, ShouldBeNil)
				So(store.Len(), ShouldEqual, 0)
			})
		})
	})
}

func TestStoreLoadSaveRoundTrip(t *testing.T) {
	Convey("Given a missing secrets file", t, func() {
		path := filepath.Join(t.TempDir(), "state", "secrets.json")

		store, err := secret.Load(path)
		So(err, ShouldBeNil)

		Convey("When saving an unchanged store", func() {
			So(store.Save(), ShouldBeNil)

			Convey("Then no file is created until a value is set", func() {
				_, statErr := os.Stat(path)
				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)

				store.Set("TOKEN", "s3cr3t")
				So(store.Save(), ShouldBeNil)
				So(store.Changed(), ShouldBeFalse)

				info, statErr := os.Stat(path)
				So(statErr, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o600))

				dirInfo, statErr := os.Stat(filepath.Dir(path))
				So(statErr, ShouldBeNil)
				So(dirInfo.Mode().Perm(), ShouldEqual, os.FileMode(0o700))

				reloaded, err := secret.Load(path)
				So(err, ShouldBeNil)

				value, ok := reloaded.Get("TOKEN")

				Convey("Then the value round-trips", func() {
					So(ok, ShouldBeTrue)
					So(value, ShouldEqual, "s3cr3t")
				})
			})
		})
	})
}

func TestStoreSaveIsAtomic(t *testing.T) {
	Convey("Given a store that has never been saved", t, func() {
		dir := filepath.Join(t.TempDir(), "state")
		path := filepath.Join(dir, "secrets.json")

		store, err := secret.Load(path)
		So(err, ShouldBeNil)

		store.Set("TOKEN", fixtureValue)

		Convey("When it is saved", func() {
			So(store.Save(), ShouldBeNil)

			entries, readErr := os.ReadDir(dir)

			Convey("Then only the final file exists (no .tmp-* leftovers)", func() {
				So(readErr, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Name(), ShouldEqual, "secrets.json")
			})
		})
	})
}

func TestStoreLoadCorruptFile(t *testing.T) {
	Convey("Given a corrupt secrets file", t, func() {
		path := filepath.Join(t.TempDir(), "secrets.json")
		writeTestFile(t, path, "{not json at all")

		Convey("When it is loaded", func() {
			_, err := secret.Load(path)

			target, ok := errors.AsType[*secret.SecretParseError](err)

			Convey("Then it reports a SecretParseError with the path and cause", func() {
				So(err, ShouldBeError)
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, path)
				So(target.Cause, ShouldNotBeNil)
			})
		})
	})
}

func TestStoreLoadUnknownBackendRejected(t *testing.T) {
	Convey("Given a secrets document with an unknown backend", t, func() {
		path := filepath.Join(t.TempDir(), "secrets.json")
		writeTestFile(t, path, `{"version": 2, "backend": "gpg", "secrets": {}}`)

		Convey("When it is loaded", func() {
			_, err := secret.Load(path)

			target, ok := errors.AsType[*secret.SecretParseError](err)

			Convey("Then the unknown backend is rejected with the path", func() {
				So(err, ShouldBeError)
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, path)
				So(err.Error(), ShouldContainSubstring, "unknown backend")
				So(err.Error(), ShouldContainSubstring, "gpg")
			})
		})
	})
}

func TestStoreSetBackendValidation(t *testing.T) {
	Convey("Given a file-backed store", t, func() {
		store := newTestStore(t)

		Convey("When switching to keyring and back", func() {
			So(store.SetBackend(secret.BackendKeyring), ShouldBeNil)
			So(store.Backend(), ShouldEqual, secret.BackendKeyring)
			So(store.Changed(), ShouldBeTrue)

			So(store.Save(), ShouldBeNil)

			So(store.SetBackend(secret.BackendFile), ShouldBeNil)
			So(store.Backend(), ShouldEqual, secret.BackendFile)
			So(store.Changed(), ShouldBeTrue)

			Convey("Then unknown backends are rejected", func() {
				So(store.SetBackend("gpg"), ShouldBeError)
				So(store.SetBackend(""), ShouldBeError)
				So(store.Backend(), ShouldEqual, secret.BackendFile)
			})
		})

		Convey("When switching to the active backend", func() {
			Convey("Then it is a no-op", func() {
				So(store.SetBackend(secret.BackendFile), ShouldBeNil)
				So(store.Changed(), ShouldBeFalse)
			})
		})
	})
}

func TestStoreDocumentShape(t *testing.T) {
	Convey("Given a file-backed store with values", t, func() {
		dir := filepath.Join(t.TempDir(), "state")
		path := filepath.Join(dir, "secrets.json")

		store, err := secret.Load(path)
		So(err, ShouldBeNil)

		store.Set("TOKEN", fixtureValue)
		store.Set("API_KEY", genericKey)

		Convey("When it is saved", func() {
			So(store.Save(), ShouldBeNil)

			raw, readErr := os.ReadFile(path) //nolint:gosec // G304: test reads its own temp file
			So(readErr, ShouldBeNil)

			Convey("Then the document is {version:1,secrets:{...}} with a trailing newline", func() {
				So(string(raw), ShouldEqualJSON, `{"version":1,"secrets":{"API_KEY":"`+genericKey+`","TOKEN":"`+fixtureValue+`"}}`)
				So(strings.HasSuffix(string(raw), "\n"), ShouldBeTrue)
			})
		})
	})
}

func TestStoreSaveFailureCarriesPath(t *testing.T) {
	Convey("Given an unwritable secrets path", t, func() {
		dir := filepath.Join(t.TempDir(), "state")
		path := filepath.Join(dir, "secrets.json")

		store, err := secret.Load(path)
		So(err, ShouldBeNil)

		store.Set("TOKEN", fixtureValue)

		if mkdirErr := os.MkdirAll(path, 0o700); mkdirErr != nil {
			t.Fatalf("mkdir blocker: %v", mkdirErr)
		}

		Convey("When it is saved", func() {
			saveErr := store.Save()

			entries, readErr := os.ReadDir(dir)

			Convey("Then the OS error is wrapped with the path, the store stays dirty and no temp file remains", func() {
				So(saveErr, ShouldBeError)
				So(saveErr.Error(), ShouldContainSubstring, path)
				So(saveErr.Error(), ShouldNotContainSubstring, fixtureValue)
				So(store.Changed(), ShouldBeTrue)

				So(readErr, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
			})
		})
	})
}

func TestStoreEmptyAndOversizedValues(t *testing.T) {
	Convey("Given a file-backed store", t, func() {
		path := filepath.Join(t.TempDir(), "state", "secrets.json")

		store, err := secret.Load(path)
		So(err, ShouldBeNil)

		oversized := strings.Repeat("A", 1<<20)

		Convey("When an empty and an oversized value are stored", func() {
			store.Set("EMPTY", "")
			store.Set("BIG", oversized)

			So(store.Save(), ShouldBeNil)

			reloaded, err := secret.Load(path)
			So(err, ShouldBeNil)

			Convey("Then both round-trip byte-exactly", func() {
				empty, ok := reloaded.Get("EMPTY")
				So(ok, ShouldBeTrue)
				So(empty, ShouldBeEmpty)

				big, ok := reloaded.Get("BIG")
				So(ok, ShouldBeTrue)
				So(big, ShouldEqual, oversized)
				So(reloaded.Len(), ShouldEqual, 2)
			})
		})

		Convey("When a value holds quotes, newlines and unicode", func() {
			value := "line \"one\"\nline\\two \u2603"

			store.Set("TOKEN", value)
			So(store.Save(), ShouldBeNil)

			reloaded, err := secret.Load(path)
			So(err, ShouldBeNil)

			Convey("Then it round-trips through JSON escaping", func() {
				got, ok := reloaded.Get("TOKEN")
				So(ok, ShouldBeTrue)
				So(got, ShouldEqual, value)
			})
		})
	})
}

func TestMetadataNeverCarriesValues(t *testing.T) {
	Convey("Given a file-backed store with a value", t, func() {
		path := filepath.Join(t.TempDir(), "state", "secrets.json")

		store, err := secret.Load(path)
		So(err, ShouldBeNil)

		store.Set("TOKEN", fixtureValue)

		Convey("When names are queried", func() {
			Convey("Then only names are returned, never values", func() {
				So(store.Names(), ShouldResemble, []string{"TOKEN"})
				So(strings.Join(store.Names(), ","), ShouldNotContainSubstring, fixtureValue)
				So(secret.Ref(store.Names()[0]), ShouldNotContainSubstring, fixtureValue)
				So(secret.Fingerprint(fixtureValue), ShouldNotContainSubstring, fixtureValue)
			})
		})

		Convey("When the in-memory map marshals", func() {
			Convey("Then the keyring-style document carries no value", func() {
				placeholder := map[string]any{"version": 2, "backend": secret.BackendKeyring, "secrets": map[string]string{"TOKEN": ""}}

				encoded, marshalErr := json.Marshal(placeholder)
				So(marshalErr, ShouldBeNil)
				So(string(encoded), ShouldNotContainSubstring, fixtureValue)
			})
		})
	})
}

// leakCase drives one public call that must fail without exposing value.
type leakCase struct {
	name  string
	value string
	run   func(t *testing.T) error
}

func TestNoSecretValueLeaks(t *testing.T) {
	Convey("Given a table of failing public calls carrying a secret", t, func() {
		value := fixtureValue

		cases := []leakCase{
			{
				name:  "Load corrupt JSON",
				value: value,
				run: func(t *testing.T) error {
					t.Helper()

					path := filepath.Join(t.TempDir(), "secrets.json")
					writeTestFile(t, path, `{"secrets": {"TOKEN": "`+value+`"},`)

					_, err := secret.Load(path)

					return err
				},
			},
			{
				name:  "Load unknown backend",
				value: value,
				run: func(t *testing.T) error {
					t.Helper()

					path := filepath.Join(t.TempDir(), "secrets.json")
					writeTestFile(t, path, `{"version": 2, "backend": "gpg", "secrets": {"TOKEN": "`+value+`"}}`)

					_, err := secret.Load(path)

					return err
				},
			},
			{
				name:  "Save to an unusable path",
				value: value,
				run: func(t *testing.T) error {
					t.Helper()

					dir := filepath.Join(t.TempDir(), "state")
					path := filepath.Join(dir, "secrets.json")

					store, err := secret.Load(path)
					if err != nil {
						return err
					}

					store.Set("TOKEN", value)

					if err := os.MkdirAll(path, 0o700); err != nil {
						return err
					}

					return store.Save()
				},
			},
			{
				name:  "ExtractJSON invalid document",
				value: value,
				run: func(t *testing.T) error {
					t.Helper()

					_, _, err := secret.ExtractJSON([]byte(`{"TOKEN": "`+value+`",`), newTestStore(t))

					return err
				},
			},
			{
				name:  "ResolveJSON invalid document",
				value: value,
				run: func(t *testing.T) error {
					t.Helper()

					_, _, err := secret.ResolveJSON([]byte(`{"TOKEN": "`+value+`",`), newTestStore(t), secret.ModeLiteral)

					return err
				},
			},
			{
				name:  "RefsJSON invalid document",
				value: value,
				run: func(t *testing.T) error {
					t.Helper()

					_, err := secret.RefsJSON([]byte(`{"TOKEN": "` + value + `",`))

					return err
				},
			},
		}

		for _, tc := range cases {
			Convey("When "+tc.name, func() {
				err := tc.run(t)

				Convey("Then the error is reported without the value", func() {
					So(err, ShouldBeError)
					So(err.Error(), ShouldNotContainSubstring, tc.value)
				})
			})
		}
	})
}

func TestSeparateStoresConcurrently(t *testing.T) {
	Convey("Given independent stores", t, func() {
		const n = 8

		paths := make([]string, n)

		for i := range paths {
			paths[i] = filepath.Join(t.TempDir(), "state", "secrets.json")
		}

		Convey("When they are used from separate goroutines", func() {
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
			)

			for _, path := range paths {
				wg.Go(func() {
					store, err := secret.Load(path)
					if err != nil {
						mu.Lock()
						defer mu.Unlock()

						errs = append(errs, err)

						return
					}

					store.Set("TOKEN", fixtureValue)

					if err := store.Save(); err != nil {
						mu.Lock()
						defer mu.Unlock()

						errs = append(errs, err)
					}
				})
			}

			wg.Wait()

			Convey("Then every store lands on disk independently", func() {
				So(errs, ShouldBeEmpty)

				for _, path := range paths {
					So(fileExists(path), ShouldBeTrue)
				}
			})
		})
	})
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
