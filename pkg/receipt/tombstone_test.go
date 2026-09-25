package receipt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// sampleTombstone builds one removal record.
func sampleTombstone(pkg, host, scope string, at time.Time) Tombstone {
	return Tombstone{
		Schema:        Schema,
		Package:       pkg,
		Host:          host,
		Scope:         scope,
		RemovedAt:     at,
		Cause:         CauseUser,
		TrashID:       "20260925T123045.123456789Z",
		ReceiptDigest: testHash("receipt"),
	}
}

func TestTombstoneLoadMissing(t *testing.T) {
	Convey("Given a tombstone store without a file", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		Convey("When it is loaded", func() {
			records, err := store.Load()

			Convey("Then it is empty and nothing was created", func() {
				So(err, ShouldBeNil)
				So(records, ShouldBeEmpty)
				assertMissing(t, path)
			})
		})

		Convey("When prune runs on an empty store", func() {
			count, err := store.Prune(time.Now())

			Convey("Then nothing is removed", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)
				assertMissing(t, path)
			})
		})
	})
}

func TestTombstoneEmptyFile(t *testing.T) {
	Convey("Given a zero-byte tombstone file", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		So(os.MkdirAll(filepath.Dir(path), 0o700), ShouldBeNil)
		So(os.WriteFile(path, nil, 0o600), ShouldBeNil)

		Convey("When it is loaded and a record is added", func() {
			records, loadErr := store.Load()
			addErr := store.Add(sampleTombstone("a/one", "claude", ScopeUser, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)))

			Convey("Then it behaves like a missing file", func() {
				So(loadErr, ShouldBeNil)
				So(records, ShouldBeEmpty)
				So(addErr, ShouldBeNil)

				records, loadErr = store.Load()
				So(loadErr, ShouldBeNil)
				So(records, ShouldHaveLength, 1)
			})
		})
	})
}

func TestTombstoneAddUpsertSortDelete(t *testing.T) {
	Convey("Given a tombstone store", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

		b := sampleTombstone("b/two", "claude", ScopeUser, base)
		a := sampleTombstone("a/one", "codex", ScopeProject, base)

		Convey("When records are added out of order", func() {
			So(store.Add(b), ShouldBeNil)
			So(store.Add(a), ShouldBeNil)

			Convey("Then they load sorted and the file carries the spec'd shape", func() {
				records, err := store.Load()
				So(err, ShouldBeNil)
				So(records, ShouldResemble, []Tombstone{a, b})

				assertMode(t, path, 0o600)
				assertMode(t, filepath.Dir(path), 0o700)

				var doc map[string]json.RawMessage
				So(json.Unmarshal(readTestFile(t, path), &doc), ShouldBeNil)
				So(string(doc["schema"]), ShouldEqualJSON, "1")

				var list []Tombstone
				So(json.Unmarshal(doc["tombstones"], &list), ShouldBeNil)
				So(list, ShouldResemble, []Tombstone{a, b})
			})
		})

		Convey("When the same key is added twice", func() {
			first := sampleTombstone("a/one", "codex", ScopeUser, base)
			second := sampleTombstone("a/one", "codex", ScopeUser, base.Add(time.Hour))
			second.Cause = CauseCapability
			second.TrashID = "trash-2"

			So(store.Add(first), ShouldBeNil)
			So(store.Add(second), ShouldBeNil)

			Convey("Then the record is replaced, not duplicated", func() {
				records, err := store.Load()
				So(err, ShouldBeNil)
				So(records, ShouldResemble, []Tombstone{second})
			})
		})

		Convey("When records are deleted", func() {
			So(store.Add(a), ShouldBeNil)
			So(store.Add(b), ShouldBeNil)

			removed, err := store.Delete("a/one", "codex", ScopeProject)
			missing, missingErr := store.Delete("nope/x", "claude", ScopeUser)

			Convey("Then Delete reports whether a record was removed", func() {
				So(err, ShouldBeNil)
				So(removed, ShouldBeTrue)
				So(missingErr, ShouldBeNil)
				So(missing, ShouldBeFalse)

				records, loadErr := store.Load()
				So(loadErr, ShouldBeNil)
				So(records, ShouldResemble, []Tombstone{b})
			})
		})
	})
}

func TestTombstonePruneBoundary(t *testing.T) {
	Convey("Given tombstones around a cutoff", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		cutoff := base.Add(-30 * 24 * time.Hour)

		old := sampleTombstone("old/one", "claude", ScopeUser, cutoff.Add(-time.Nanosecond))
		boundary := sampleTombstone("boundary/one", "claude", ScopeUser, cutoff)
		fresh := sampleTombstone("fresh/one", "claude", ScopeUser, base)

		for _, record := range []Tombstone{old, boundary, fresh} {
			So(store.Add(record), ShouldBeNil)
		}

		Convey("When Prune removes records strictly before the cutoff", func() {
			count, err := store.Prune(cutoff)

			Convey("Then only strictly older records go", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1)

				records, loadErr := store.Load()
				So(loadErr, ShouldBeNil)
				So(records, ShouldResemble, []Tombstone{boundary, fresh})
			})
		})

		Convey("When Prune cuts one second later", func() {
			count, err := store.Prune(cutoff.Add(time.Second))

			Convey("Then the boundary record goes too", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 2)

				records, loadErr := store.Load()
				So(loadErr, ShouldBeNil)
				So(records, ShouldResemble, []Tombstone{fresh})
			})
		})
	})
}

func TestTombstoneValidation(t *testing.T) {
	Convey("Given tombstone validation", t, func() {
		store := NewTombstoneStore(filepath.Join(t.TempDir(), "state", "tombstones.json"))
		base := sampleTombstone("a/one", "claude", ScopeUser, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))

		cases := []struct {
			name   string
			mutate func(t *Tombstone)
		}{
			{"empty package", func(t *Tombstone) { t.Package = "" }},
			{"empty host", func(t *Tombstone) { t.Host = "" }},
			{"empty scope", func(t *Tombstone) { t.Scope = "" }},
			{"unknown scope", func(t *Tombstone) { t.Scope = "global" }},
			{"unknown cause", func(t *Tombstone) { t.Cause = "mystery" }},
			{"zero removed_at", func(t *Tombstone) { t.RemovedAt = time.Time{} }},
		}

		for _, tc := range cases {
			Convey("When "+tc.name, func() {
				record := base
				tc.mutate(&record)

				err := store.Add(record)

				Convey("Then it is rejected and nothing is written", func() {
					So(err, ShouldBeError)

					target, ok := errors.AsType[*InvalidTombstoneError](err)
					So(ok, ShouldBeTrue)
					So(target.Cause, ShouldNotBeNil)

					records, loadErr := store.Load()
					So(loadErr, ShouldBeNil)
					So(records, ShouldBeEmpty)
				})
			})
		}

		Convey("When a record carries unsafe key components", func() {
			unsafe := base
			unsafe.Package = "../evil"

			err := store.Add(unsafe)

			Convey("Then it is rejected as an invalid key", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*InvalidKeyError](err)
				So(ok, ShouldBeTrue)
				So(target.Field, ShouldEqual, "package")
				assertMissing(t, store.path)
			})
		})

		Convey("When a valid record carries schema zero", func() {
			record := base
			record.Schema = 0

			err := store.Add(record)

			Convey("Then Add forces the current schema", func() {
				So(err, ShouldBeNil)

				records, _ := store.Load()
				So(records, ShouldHaveLength, 1)
				So(records[0].Schema, ShouldEqual, Schema)
			})
		})
	})
}

func TestTombstoneCorruptAndSchema(t *testing.T) {
	Convey("Given a tombstone store", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		writeDoc := func(content string) {
			So(os.MkdirAll(filepath.Dir(path), 0o700), ShouldBeNil)
			So(os.WriteFile(path, []byte(content), 0o600), ShouldBeNil)
		}

		Convey("When the document is broken JSON", func() {
			writeDoc("{broken")

			_, loadErr := store.Load()
			addErr := store.Add(sampleTombstone("a/one", "claude", ScopeUser, time.Now()))

			Convey("Then Load and Add report CorruptTombstonesError", func() {
				_, loadOK := errors.AsType[*CorruptTombstonesError](loadErr)
				So(loadOK, ShouldBeTrue)

				_, addOK := errors.AsType[*CorruptTombstonesError](addErr)
				So(addOK, ShouldBeTrue)
			})
		})

		Convey("When the document schema is newer", func() {
			writeDoc(`{"schema":2,"tombstones":[]}`)

			_, loadErr := store.Load()

			Convey("Then it reports SchemaNewerError", func() {
				target, ok := errors.AsType[*SchemaNewerError](loadErr)
				So(ok, ShouldBeTrue)
				So(target.Found, ShouldEqual, 2)
				So(target.Supported, ShouldEqual, Schema)
			})
		})

		Convey("When the document schema is zero", func() {
			writeDoc(`{"schema":0,"tombstones":[]}`)

			_, loadErr := store.Load()

			Convey("Then it reports CorruptTombstonesError", func() {
				_, ok := errors.AsType[*CorruptTombstonesError](loadErr)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When a record is invalid on disk", func() {
			writeDoc(`{"schema":1,"tombstones":[{"schema":1,"package":"a/one","host":"claude","scope":"user","removed_at":"0001-01-01T00:00:00Z","cause":"user"}]}`)

			_, loadErr := store.Load()

			Convey("Then it reports CorruptTombstonesError", func() {
				_, ok := errors.AsType[*CorruptTombstonesError](loadErr)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestTombstoneExtrasRoundTrip(t *testing.T) {
	Convey("Given a tombstone file with future fields", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		doc := `{"schema":1,"future_doc":{"x":1},"tombstones":[` +
			`{"schema":1,"package":"a/one","host":"claude","scope":"user",` +
			`"removed_at":"2026-09-25T12:00:00Z","cause":"user","future_record":"keep"}]}`

		So(os.MkdirAll(filepath.Dir(path), 0o700), ShouldBeNil)
		So(os.WriteFile(path, []byte(doc), 0o600), ShouldBeNil)

		Convey("When another record is added", func() {
			So(store.Add(sampleTombstone("b/two", "codex", ScopeUser, time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC))), ShouldBeNil)

			Convey("Then document and record extras survive", func() {
				var parsed map[string]json.RawMessage
				So(json.Unmarshal(readTestFile(t, path), &parsed), ShouldBeNil)
				So(string(parsed["future_doc"]), ShouldEqualJSON, `{"x":1}`)

				var records []map[string]json.RawMessage
				So(json.Unmarshal(parsed["tombstones"], &records), ShouldBeNil)
				So(records, ShouldHaveLength, 2)
				So(string(records[0]["future_record"]), ShouldEqualJSON, `"keep"`)
			})
		})
	})
}

func TestTombstoneGolden(t *testing.T) {
	Convey("Given a fixed tombstone sequence", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		store := NewTombstoneStore(path)

		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		So(store.Add(sampleTombstone("owner/name", "claude", ScopeUser, base)), ShouldBeNil)
		So(store.Add(sampleTombstone("owner/name", "codex", ScopeProject, base.Add(time.Minute))), ShouldBeNil)

		Convey("When the file is compared to the golden fixture", func() {
			checkGolden(t, filepath.Join("testdata", "tombstones.golden.json"), readTestFile(t, path))
		})
	})
}

func TestTombstoneRestartIdempotent(t *testing.T) {
	Convey("Given a tombstone store on disk", t, func() {
		path := filepath.Join(t.TempDir(), "state", "tombstones.json")
		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

		So(NewTombstoneStore(path).Add(sampleTombstone("a/one", "claude", ScopeUser, base)), ShouldBeNil)
		first := readTestFile(t, path)

		Convey("When a fresh instance adds the same record again", func() {
			So(NewTombstoneStore(path).Add(sampleTombstone("a/one", "claude", ScopeUser, base)), ShouldBeNil)

			Convey("Then the file bytes are unchanged", func() {
				So(readTestFile(t, path), ShouldResemble, first)
			})
		})
	})
}
