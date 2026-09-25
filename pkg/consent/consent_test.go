package consent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// fixedAt is the deterministic clock instant used across the consent tests.
var fixedAt = time.Date(2026, 9, 25, 15, 4, 5, 0, time.UTC)

// fixedClock pins a store to fixedAt.
func fixedClock() Option {
	return WithClock(func() time.Time { return fixedAt })
}

// testHash builds a deterministic digest for fixtures.
func testHash(seed string) digest.Hash {
	return digest.Bytes([]byte(seed))
}

// testFile reads a file the test itself created.
func testFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads a path it created
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}

// assertMode fails when path does not carry exactly want.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}

// assertMissing fails when path exists.
func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

func TestHookHash(t *testing.T) {
	base := []manifest.Hook{
		{Event: "PreToolUse", Matcher: "Bash", Command: "echo hi", Timeout: 5, Origin: manifest.FormatClaude},
		{Event: "Stop", Matcher: "", Command: "notify", Timeout: 0, Origin: manifest.FormatClaude},
	}

	Convey("Given a hook set", t, func() {
		Convey("When the same hooks are ordered differently", func() {
			reordered := []manifest.Hook{base[1], base[0]}

			Convey("Then the hash is order-independent", func() {
				So(HookHash("acme/foo", "1.2.3", reordered), ShouldEqual, HookHash("acme/foo", "1.2.3", base))
			})
		})

		Convey("When the hooks are empty", func() {
			Convey("Then nil and empty hash to the stable empty-list golden", func() {
				empty := HookHash("empty/pkg", "0.0.0", nil)
				So(string(empty), ShouldEqual, "053fe34f4e2eaf171111007b56684a754818224b3d63bb373de2ffba4d8f4f43")
				So(HookHash("empty/pkg", "0.0.0", []manifest.Hook{}), ShouldEqual, empty)
			})
		})

		Convey("When every canonical field changes", func() {
			want := HookHash("acme/foo", "1.2.3", base)

			mutate := func(fn func([]manifest.Hook)) digest.Hash {
				hooks := append([]manifest.Hook(nil), base...)
				fn(hooks)

				return HookHash("acme/foo", "1.2.3", hooks)
			}

			Convey("Then the hash changes and the golden matches", func() {
				So(string(want), ShouldEqual, "ef6b1f99f55d4b0147879acff78ec15e234319d73bf686f1eb2d7363e236a50b")

				So(mutate(func(h []manifest.Hook) { h[0].Event = "PostToolUse" }), ShouldNotEqual, want)
				So(mutate(func(h []manifest.Hook) { h[0].Matcher = "Read" }), ShouldNotEqual, want)
				So(mutate(func(h []manifest.Hook) { h[0].Command = "echo bye" }), ShouldNotEqual, want)
				So(mutate(func(h []manifest.Hook) { h[0].Timeout = 6 }), ShouldNotEqual, want)
				So(HookHash("acme/bar", "1.2.3", base), ShouldNotEqual, want)
				So(HookHash("acme/foo", "1.2.4", base), ShouldNotEqual, want)
				So(HookHash("acme/foo", "", base), ShouldNotEqual, want)
			})
		})

		Convey("When only the origin dialect differs", func() {
			other := append([]manifest.Hook(nil), base...)
			other[0].Origin = manifest.FormatCodex
			other[1].Origin = manifest.FormatGemini

			Convey("Then the content hash is unchanged", func() {
				So(HookHash("acme/foo", "1.2.3", other), ShouldEqual, HookHash("acme/foo", "1.2.3", base))
			})
		})
	})
}

func TestConsentLifecycle(t *testing.T) {
	Convey("Given a consent store with a fixed clock", t, func() {
		store := NewStore(filepath.Join(t.TempDir(), "state", "consent.json"), fixedClock())
		hash := testHash("hooks-v1")

		Convey("When hooks are approved", func() {
			err := store.ApproveHooks("acme/foo", "1.0.0", hash)

			Convey("Then the record is approved with the stamped time", func() {
				So(err, ShouldBeNil)
				So(store.HooksApproved("acme/foo", "1.0.0", hash), ShouldBeTrue)

				rec, ok := store.Hooks("acme/foo")
				So(ok, ShouldBeTrue)
				So(rec, ShouldResemble, HookRecord{Package: "acme/foo", Version: "1.0.0", Hash: hash, ApprovedAt: fixedAt})
			})

			Convey("Then a version bump is not approved and is pending", func() {
				So(store.HooksApproved("acme/foo", "1.1.0", hash), ShouldBeFalse)
				So(store.Pending("acme/foo", "1.1.0", hash), ShouldBeTrue)
			})

			Convey("Then a changed hash is not approved and is pending", func() {
				changed := testHash("hooks-v2")
				So(store.HooksApproved("acme/foo", "1.0.0", changed), ShouldBeFalse)
				So(store.Pending("acme/foo", "1.0.0", changed), ShouldBeTrue)
			})

			Convey("When the hooks are revoked", func() {
				revokeErr := store.RevokeHooks("acme/foo")

				Convey("Then nothing is approved or pending, and revoke is idempotent", func() {
					So(revokeErr, ShouldBeNil)

					_, ok := store.Hooks("acme/foo")
					So(ok, ShouldBeFalse)
					So(store.HooksApproved("acme/foo", "1.0.0", hash), ShouldBeFalse)
					So(store.Pending("acme/foo", "1.0.0", hash), ShouldBeFalse)
					So(store.RevokeHooks("acme/foo"), ShouldBeNil)
				})

				Convey("Then an explicit re-grant approves again", func() {
					So(store.ApproveHooks("acme/foo", "1.0.0", hash), ShouldBeNil)
					So(store.HooksApproved("acme/foo", "1.0.0", hash), ShouldBeTrue)
				})
			})
		})

		Convey("When a package was never approved", func() {
			Convey("Then it is neither approved nor pending", func() {
				So(store.HooksApproved("acme/none", "1.0.0", hash), ShouldBeFalse)
				So(store.Pending("acme/none", "1.0.0", hash), ShouldBeFalse)
			})
		})
	})
}

func TestConsentBackgroundHashChange(t *testing.T) {
	Convey("Given an approved package and a changed hash", t, func() {
		store := NewStore(filepath.Join(t.TempDir(), "consent.json"), fixedClock())
		oldHash := testHash("hooks-v1")
		newHash := testHash("hooks-v2")

		So(store.ApproveHooks("acme/foo", "1.0.0", oldHash), ShouldBeNil)

		Convey("When the background path checks the new hash", func() {
			approved := store.HooksApproved("acme/foo", "1.0.0", newHash)
			pending := store.Pending("acme/foo", "1.0.0", newHash)

			Convey("Then it delivers without hooks and leaves the record untouched", func() {
				So(approved, ShouldBeFalse)
				So(pending, ShouldBeTrue)

				rec, ok := store.Hooks("acme/foo")
				So(ok, ShouldBeTrue)
				So(rec.Hash, ShouldEqual, oldHash)
			})

			Convey("Then only an explicit approve moves it to the new hash", func() {
				So(store.ApproveHooks("acme/foo", "1.0.0", newHash), ShouldBeNil)
				So(store.HooksApproved("acme/foo", "1.0.0", newHash), ShouldBeTrue)
				So(store.Pending("acme/foo", "1.0.0", newHash), ShouldBeFalse)
			})
		})
	})
}

func TestConsentAdoptNeverApproves(t *testing.T) {
	Convey("Given an adopt delivery that must not install hooks (D4)", t, func() {
		path := filepath.Join(t.TempDir(), "state", "consent.json")
		store := NewStore(path, fixedClock())
		hash := testHash("h")

		Convey("When the adopt path runs without any consent call", func() {
			_, ok := store.Hooks("acme/adopted")

			Convey("Then no consent record or file exists", func() {
				So(ok, ShouldBeFalse)
				So(store.HooksApproved("acme/adopted", "1.0.0", hash), ShouldBeFalse)
				So(store.Pending("acme/adopted", "1.0.0", hash), ShouldBeFalse)
				assertMissing(t, path)
			})

			Convey("Then only an explicit approve creates a record", func() {
				So(store.ApproveHooks("acme/adopted", "1.0.0", hash), ShouldBeNil)
				So(store.HooksApproved("acme/adopted", "1.0.0", hash), ShouldBeTrue)
			})
		})
	})
}

func TestConsentModesAndNonTTY(t *testing.T) {
	Convey("Given the CLI-facing consent modes", t, func() {
		Convey("Then they mirror spec.HooksMode values", func() {
			So(ModeAsk, ShouldEqual, string(spec.HooksAsk))
			So(ModeYes, ShouldEqual, string(spec.HooksYes))
			So(ModeNo, ShouldEqual, string(spec.HooksNo))
		})

		Convey("When -y accepts the defaults (yes)", func() {
			store := NewStore(filepath.Join(t.TempDir(), "consent.json"), fixedClock())
			hash := testHash("h")

			// The CLI maps ModeYes to an explicit approve of the current hash.
			So(store.ApproveHooks("acme/foo", "1.0.0", hash), ShouldBeNil)

			Convey("Then hooks are approved for exactly this hash", func() {
				So(store.HooksApproved("acme/foo", "1.0.0", hash), ShouldBeTrue)
			})
		})

		Convey("When non-TTY runs without -y (ask, no answer)", func() {
			store := NewStore(filepath.Join(t.TempDir(), "consent.json"), fixedClock())

			Convey("Then nothing is auto-approved and the question stays open", func() {
				So(store.HooksApproved("acme/foo", "1.0.0", testHash("h")), ShouldBeFalse)

				_, ok := store.Hooks("acme/foo")
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When the spec says no", func() {
			store := NewStore(filepath.Join(t.TempDir(), "consent.json"), fixedClock())

			Convey("Then no consent call is made and the hash stays unapproved", func() {
				So(store.HooksApproved("acme/foo", "1.0.0", testHash("h")), ShouldBeFalse)
				So(store.Pending("acme/foo", "1.0.0", testHash("h")), ShouldBeFalse)
			})
		})
	})
}

func TestConsentSaveLoadRoundTrip(t *testing.T) {
	Convey("Given a consent store with two approvals", t, func() {
		dir := filepath.Join(t.TempDir(), "state")
		path := filepath.Join(dir, "consent.json")
		store := NewStore(path, fixedClock())

		hashA := testHash("a")
		hashB := testHash("b")

		So(store.ApproveHooks("acme/foo", "1.0.0", hashA), ShouldBeNil)
		So(store.ApproveHooks("acme/bar", "2.0.0", hashB), ShouldBeNil)

		Convey("When the store is saved", func() {
			So(store.Save(), ShouldBeNil)

			Convey("Then the file is 0600 under a 0700 state dir", func() {
				assertMode(t, path, 0o600)
				assertMode(t, dir, 0o700)

				var doc map[string]json.RawMessage
				So(json.Unmarshal(testFile(t, path), &doc), ShouldBeNil)
				So(string(doc["schema"]), ShouldEqualJSON, "1")
				So(string(doc["hooks"]), ShouldContainSubstring, `"acme/foo"`)
			})

			Convey("When a fresh store loads it", func() {
				restarted := NewStore(path, fixedClock())
				loadErr := restarted.Load()

				Convey("Then every record survives", func() {
					So(loadErr, ShouldBeNil)

					recA, okA := restarted.Hooks("acme/foo")
					So(okA, ShouldBeTrue)
					So(recA, ShouldResemble, HookRecord{Package: "acme/foo", Version: "1.0.0", Hash: hashA, ApprovedAt: fixedAt})

					recB, okB := restarted.Hooks("acme/bar")
					So(okB, ShouldBeTrue)
					So(recB.Version, ShouldEqual, "2.0.0")
					So(restarted.HooksApproved("acme/bar", "2.0.0", hashB), ShouldBeTrue)
				})

				Convey("Then a re-save is byte-identical", func() {
					first := testFile(t, path)

					So(restarted.Save(), ShouldBeNil)
					So(testFile(t, path), ShouldResemble, first)
				})
			})
		})
	})

	Convey("Given a consent file with unknown fields", t, func() {
		path := filepath.Join(t.TempDir(), "consent.json")
		raw := `{"schema":1,"future_top":{"a":1},"hooks":{"acme/foo":{"package":"acme/foo","version":"1.0.0",` +
			`"hash":"` + string(testHash("a")) + `","future_rec":42,"approved_at":"2026-09-25T15:04:05Z"}}}`
		So(os.WriteFile(path, []byte(raw), 0o600), ShouldBeNil)

		Convey("When it is loaded and re-saved", func() {
			store := NewStore(path, fixedClock())
			So(store.Load(), ShouldBeNil)
			So(store.Save(), ShouldBeNil)

			Convey("Then unknown top-level and record fields survive", func() {
				var doc map[string]json.RawMessage
				So(json.Unmarshal(testFile(t, path), &doc), ShouldBeNil)
				So(string(doc["future_top"]), ShouldEqualJSON, `{"a":1}`)

				var hooks map[string]map[string]json.RawMessage
				So(json.Unmarshal(doc["hooks"], &hooks), ShouldBeNil)
				So(string(hooks["acme/foo"]["future_rec"]), ShouldEqualJSON, "42")
			})
		})
	})
}

func TestConsentCorruptAndSchema(t *testing.T) {
	Convey("Given a consent store path", t, func() {
		path := filepath.Join(t.TempDir(), "consent.json")

		Convey("When the file is missing", func() {
			store := NewStore(path, fixedClock())

			Convey("Then Load reports an empty store", func() {
				So(store.Load(), ShouldBeNil)

				_, ok := store.Hooks("acme/foo")
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When the JSON is broken", func() {
			So(os.WriteFile(path, []byte("{not json"), 0o600), ShouldBeNil)
			err := NewStore(path, fixedClock()).Load()

			Convey("Then it reports ConsentParseError and the bytes stay intact", func() {
				_, ok := errors.AsType[*ConsentParseError](err)
				So(ok, ShouldBeTrue)
				So(testFile(t, path), ShouldResemble, []byte("{not json"))
			})
		})

		Convey("When Save runs over a corrupt file", func() {
			So(os.WriteFile(path, []byte("{not json"), 0o600), ShouldBeNil)
			err := NewStore(path, fixedClock()).Save()

			Convey("Then it refuses and keeps the bytes", func() {
				_, ok := errors.AsType[*ConsentParseError](err)
				So(ok, ShouldBeTrue)
				So(testFile(t, path), ShouldResemble, []byte("{not json"))
			})
		})

		Convey("When the schema is newer", func() {
			So(os.WriteFile(path, []byte(`{"schema":2,"hooks":{}}`), 0o600), ShouldBeNil)
			err := NewStore(path, fixedClock()).Load()

			Convey("Then it reports SchemaNewerError", func() {
				target, ok := errors.AsType[*SchemaNewerError](err)
				So(ok, ShouldBeTrue)
				So(target.Found, ShouldEqual, 2)
				So(target.Supported, ShouldEqual, Schema)
			})
		})

		Convey("When the schema is zero", func() {
			So(os.WriteFile(path, []byte(`{"hooks":{}}`), 0o600), ShouldBeNil)
			err := NewStore(path, fixedClock()).Load()

			Convey("Then it reports SchemaInvalidError", func() {
				_, ok := errors.AsType[*SchemaInvalidError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When a record key does not match its record", func() {
			raw := `{"schema":1,"hooks":{"acme/foo":{"package":"acme/bar","version":"1.0.0","hash":"` +
				string(testHash("a")) + `"}}}`
			So(os.WriteFile(path, []byte(raw), 0o600), ShouldBeNil)
			err := NewStore(path, fixedClock()).Load()

			Convey("Then it reports ConsentParseError", func() {
				_, ok := errors.AsType[*ConsentParseError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestConsentConcurrency(t *testing.T) {
	Convey("Given one store instance guarded by a mutex (the home flock stand-in)", t, func() {
		path := filepath.Join(t.TempDir(), "consent.json")
		store := NewStore(path, fixedClock())

		const writers = 8

		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)

		Convey("When writers approve and save concurrently", func() {
			errs := make(chan error, writers)

			for i := range writers {
				wg.Go(func() {
					pkg := fmt.Sprintf("acme/p%d", i)

					mu.Lock()
					defer mu.Unlock()

					if err := store.ApproveHooks(pkg, "1.0.0", testHash(pkg)); err != nil {
						errs <- err

						return
					}

					errs <- store.Save()
				})
			}

			wg.Wait()
			close(errs)

			Convey("Then every approval survives a reload", func() {
				for err := range errs {
					So(err, ShouldBeNil)
				}

				restarted := NewStore(path, fixedClock())
				So(restarted.Load(), ShouldBeNil)

				for i := range writers {
					pkg := fmt.Sprintf("acme/p%d", i)
					So(restarted.HooksApproved(pkg, "1.0.0", testHash(pkg)), ShouldBeTrue)
				}
			})
		})

		Convey("When two stores write sequentially with reloads", func() {
			first := NewStore(path, fixedClock())
			So(first.ApproveHooks("acme/one", "1.0.0", testHash("one")), ShouldBeNil)
			So(first.Save(), ShouldBeNil)

			second := NewStore(path, fixedClock())
			So(second.Load(), ShouldBeNil)
			So(second.ApproveHooks("acme/two", "1.0.0", testHash("two")), ShouldBeNil)
			So(second.Save(), ShouldBeNil)

			Convey("Then the reloaded union holds both records", func() {
				final := NewStore(path, fixedClock())
				So(final.Load(), ShouldBeNil)
				So(final.HooksApproved("acme/one", "1.0.0", testHash("one")), ShouldBeTrue)
				So(final.HooksApproved("acme/two", "1.0.0", testHash("two")), ShouldBeTrue)
			})
		})
	})
}

func TestConsentUnicodePackages(t *testing.T) {
	Convey("Given a package id with unicode", t, func() {
		path := filepath.Join(t.TempDir(), "consent.json")
		store := NewStore(path, fixedClock())
		pkg := "acme/рег-☃"
		hash := testHash("юникод")

		So(store.ApproveHooks(pkg, "1.0.0", hash), ShouldBeNil)
		So(store.Save(), ShouldBeNil)

		Convey("When reloaded", func() {
			restarted := NewStore(path, fixedClock())
			So(restarted.Load(), ShouldBeNil)

			Convey("Then the unicode key round-trips", func() {
				rec, ok := restarted.Hooks(pkg)
				So(ok, ShouldBeTrue)
				So(rec.Package, ShouldEqual, pkg)
				So(restarted.HooksApproved(pkg, "1.0.0", hash), ShouldBeTrue)
			})
		})
	})
}
