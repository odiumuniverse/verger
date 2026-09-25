package lock_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/lock"
)

const (
	goldenPath   = "testdata/lock.golden.json"
	unknownPath  = "testdata/unknown.json"
	updateGolden = "VERGER_UPDATE_GOLDEN"
	snapshotHex  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	capsHex      = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	treeHex      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// testTime is the fixed generation time every fixture uses.
var testTime = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// fixtureLock is the canonical golden fixture: unsorted cells, a silenced cell
// with a reason, a synth-free strict native cell and a unicode package id.
func fixtureLock() *lock.Lock {
	return &lock.Lock{
		Schema: lock.Schema,
		Snapshot: lock.Snapshot{
			GeneratedAt: testTime,
			KeyID:       "0123456789ABCDEF",
			Digest:      digest.Hash(snapshotHex),
		},
		Cells: []lock.Cell{
			{Package: "vercel-labs/skills//find-skills", Host: "codex", Scope: lock.ScopeUser, Version: "1.4.2", Strategy: lock.StrategyLoose, UpdatedAt: testTime},
			{Package: "acme/рег-☃", Host: "claude", Scope: lock.ScopeUser, Version: "0.9.0", Strategy: lock.StrategySilenced, Reason: "hooks-unapproved"},
			{
				Package:  "vercel-labs/skills//find-skills",
				Host:     "claude",
				Scope:    lock.ScopeProject,
				Version:  "1.4.2",
				Strategy: lock.StrategyNative,
				CapsHash: capsHex,
				Digest:   digest.Hash(treeHex),
				Source:   "registry",
				Ref:      "v1.4.2",
			},
		},
	}
}

func mustMarshal(t *testing.T, value *lock.Lock) []byte {
	t.Helper()

	data, err := value.Marshal()
	if err != nil {
		t.Fatalf("marshal lock: %v", err)
	}

	return data
}

// goldenBytes returns the committed golden, or rewrites it when
// VERGER_UPDATE_GOLDEN=1 (the regeneration command documented in the report).
func goldenBytes(t *testing.T, got []byte) []byte {
	t.Helper()

	if os.Getenv(updateGolden) == "1" {
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil { //nolint:gosec // G306: a repo fixture, not a secret
			t.Fatalf("update golden: %v", err)
		}

		return got
	}

	golden, err := os.ReadFile(goldenPath) //nolint:gosec // G304: the test reads its own fixture
	if err != nil {
		t.Fatalf("read golden (regenerate with %s=1): %v", updateGolden, err)
	}

	return golden
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own fixture
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}

	return data
}

func TestGolden(t *testing.T) {
	Convey("Given the fixture lock", t, func() {
		got := mustMarshal(t, fixtureLock())
		golden := goldenBytes(t, got)

		Convey("When it is marshaled", func() {
			Convey("Then it matches testdata/lock.golden.json byte for byte", func() {
				So(string(got), ShouldEqual, string(golden))
				So(string(golden), ShouldNotContainSubstring, "{secret:")
				So(string(golden), ShouldNotContainSubstring, "/Users")
			})
		})

		Convey("When the golden is parsed back", func() {
			parsed, err := lock.Parse(golden)
			So(err, ShouldBeNil)

			Convey("Then fields survive and marshaling is idempotent", func() {
				So(parsed.Schema, ShouldEqual, lock.Schema)
				So(parsed.Snapshot.KeyID, ShouldEqual, "0123456789ABCDEF")
				So(parsed.Snapshot.Digest, ShouldEqual, digest.Hash(snapshotHex))
				So(parsed.Cells, ShouldHaveLength, 3)

				cell, ok := parsed.Cell("acme/рег-☃", "claude", lock.ScopeUser)
				So(ok, ShouldBeTrue)
				So(cell.Strategy, ShouldEqual, lock.StrategySilenced)
				So(cell.Reason, ShouldEqual, "hooks-unapproved")

				So(string(mustMarshal(t, parsed)), ShouldEqual, string(golden))
			})
		})
	})

	Convey("Given an empty current-schema lock", t, func() {
		got := mustMarshal(t, lock.New())

		Convey("When it is marshaled", func() {
			Convey("Then cells are an empty array and keys are alphabetical", func() {
				So(string(got), ShouldEqual, "{\n  \"cells\": [],\n  \"schema\": 1\n}\n")
			})
		})
	})
}

func TestMarshalStability(t *testing.T) {
	Convey("Given two locks with cells inserted in opposite orders", t, func() {
		first := fixtureLock()

		second := fixtureLock()
		second.Cells[0], second.Cells[2] = second.Cells[2], second.Cells[0]

		Convey("When both are marshaled", func() {
			Convey("Then the bytes are identical and repeated marshaling is stable", func() {
				firstBytes := mustMarshal(t, first)
				So(string(mustMarshal(t, second)), ShouldEqual, string(firstBytes))

				for range 5 {
					So(string(mustMarshal(t, first)), ShouldEqual, string(firstBytes))
				}
			})
		})

		Convey("When the same lock is marshaled twice", func() {
			Convey("Then the digest is stable and the receiver is not reordered", func() {
				So(first.Digest(), ShouldEqual, first.Digest())
				So(first.Digest(), ShouldEqual, digest.Bytes(mustMarshal(t, first)))
				So(first.Cells[0].Package, ShouldEqual, "vercel-labs/skills//find-skills")
			})
		})
	})
}

func TestNoHTMLEscaping(t *testing.T) {
	Convey("Given a lock with <, > and & in known fields", t, func() {
		value := fixtureLock()
		value.Cells[0].Package = "acme/<pkg>&co"
		value.Cells[0].Ref = "github:acme/co?a=1&b=2"
		value.Cells[1].Reason = "hooks <disabled> & deferred"

		data := mustMarshal(t, value)

		Convey("When it is marshaled", func() {
			Convey("Then the characters stay literal per rule 8", func() {
				So(string(data), ShouldContainSubstring, `"package": "acme/<pkg>&co"`)
				So(string(data), ShouldContainSubstring, "github:acme/co?a=1&b=2")
				So(string(data), ShouldContainSubstring, "hooks <disabled> & deferred")
				So(string(data), ShouldNotContainSubstring, `\u003c`)
				So(string(data), ShouldNotContainSubstring, `\u003e`)
				So(string(data), ShouldNotContainSubstring, `\u0026`)
			})
		})

		Convey("When the bytes are parsed back", func() {
			back, err := lock.Parse(data)
			So(err, ShouldBeNil)

			Convey("Then the literal values survive", func() {
				So(back.Cells[0].Package, ShouldEqual, "acme/<pkg>&co")
				So(back.Cells[0].Ref, ShouldEqual, "github:acme/co?a=1&b=2")
				So(back.Cells[1].Reason, ShouldEqual, "hooks <disabled> & deferred")
			})
		})
	})
}

func TestUnknownRoundTrip(t *testing.T) {
	Convey("Given a lock carrying unknown fields at both levels", t, func() {
		parsed, err := lock.Parse(readFixture(t, unknownPath))
		So(err, ShouldBeNil)

		encoded := mustMarshal(t, parsed)

		Convey("When it is marshaled", func() {
			Convey("Then unknown lock and cell keys survive", func() {
				So(string(encoded), ShouldContainSubstring, "future_lock")
				So(string(encoded), ShouldContainSubstring, "future_cell")
				So(string(encoded), ShouldContainSubstring, "future_cell_obj")
				So(string(encoded), ShouldContainSubstring, "keep-me")
			})
		})

		Convey("When it goes through Save and ParseFile", func() {
			path := filepath.Join(t.TempDir(), "verger.lock")
			So(parsed.Save(path), ShouldBeNil)

			reloaded, err := lock.ParseFile(path)
			So(err, ShouldBeNil)

			Convey("Then the unknown fields live through the disk round-trip", func() {
				So(string(mustMarshal(t, reloaded)), ShouldEqual, string(encoded))
			})
		})
	})
}

func TestSchemaGuard(t *testing.T) {
	Convey("Given a lock written by a newer verger", t, func() {
		_, err := lock.Parse([]byte(`{"schema":2,"cells":[]}`))

		detail, ok := errors.AsType[*lock.SchemaNewerError](err)

		Convey("When it is parsed", func() {
			Convey("Then SchemaNewerError carries the versions", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Found, ShouldEqual, 2)
					So(detail.Supported, ShouldEqual, lock.Schema)
				}
			})
		})
	})

	Convey("Given locks without a usable schema", t, func() {
		for _, raw := range []string{`{}`, `{"schema":0,"cells":[]}`, `{"schema":-1,"cells":[]}`} {
			Convey("When "+raw+" is parsed", func() {
				_, err := lock.Parse([]byte(raw))

				detail, ok := errors.AsType[*lock.SchemaInvalidError](err)

				Convey("Then SchemaInvalidError is reported", func() {
					So(ok, ShouldBeTrue)

					if ok {
						So(detail.Found, ShouldBeLessThanOrEqualTo, 0)
					}
				})
			})
		}
	})

	Convey("Given a duplicate schema key", t, func() {
		_, err := lock.Parse([]byte(`{"schema":1,"schema":2,"cells":[]}`))

		detail, ok := errors.AsType[*lock.SchemaNewerError](err)

		Convey("When it is parsed", func() {
			Convey("Then the documented last-wins policy applies", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Found, ShouldEqual, 2)
				}
			})
		})
	})

	Convey("Given a newer lock on disk", t, func() {
		path := filepath.Join(t.TempDir(), "verger.lock")
		So(os.WriteFile(path, []byte(`{"schema":2,"cells":[]}`), 0o600), ShouldBeNil)

		_, err := lock.ParseFile(path)

		detail, ok := errors.AsType[*lock.SchemaNewerError](err)

		Convey("When it is loaded", func() {
			Convey("Then the path is filled in", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Path, ShouldEqual, path)
				}
			})
		})
	})
}

func TestParseMalformed(t *testing.T) {
	Convey("Given bytes that are not a lock document", t, func() {
		raws := []string{"", "{", "[]", `"schema": 1`, `{"schema":1,"cells":[]} tail`, "\ufeff{\"schema\": 1}"}

		for _, raw := range raws {
			Convey("When "+strings.ReplaceAll(raw, "\ufeff", "BOM")+" is parsed", func() {
				_, err := lock.Parse([]byte(raw))

				_, ok := errors.AsType[*lock.InvalidLockError](err)

				Convey("Then InvalidLockError wraps the cause", func() {
					So(ok, ShouldBeTrue)
				})
			})
		}
	})

	Convey("Given duplicate cells", t, func() {
		raw := `{"schema":1,"cells":[` +
			`{"package":"acme/x","host":"claude","scope":"user","version":"1","strategy":"native"},` +
			`{"package":"acme/x","host":"claude","scope":"user","version":"2","strategy":"loose"}]}`

		_, err := lock.Parse([]byte(raw))

		detail, ok := errors.AsType[*lock.InvalidLockError](err)

		Convey("When it is parsed", func() {
			Convey("Then the duplicate is rejected with both coordinates named", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Cause.Error(), ShouldContainSubstring, "duplicate cell acme/x claude/user")
				}
			})
		})
	})

	Convey("Given a lock with an invalid cell", t, func() {
		raw := `{"schema":1,"cells":[{"package":"acme/x","host":"claude","scope":"user","version":"","strategy":"native"}]}`

		_, err := lock.Parse([]byte(raw))

		detail, ok := errors.AsType[*lock.InvalidCellError](err)

		Convey("When it is parsed", func() {
			Convey("Then InvalidCellError names the cell", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Package, ShouldEqual, "acme/x")
					So(detail.Host, ShouldEqual, "claude")
					So(detail.Scope, ShouldEqual, "user")
				}
			})
		})
	})
}

func TestParseFileMissing(t *testing.T) {
	Convey("Given a missing lock file", t, func() {
		path := filepath.Join(t.TempDir(), "verger.lock")

		_, err := lock.ParseFile(path)

		Convey("When it is loaded", func() {
			Convey("Then fs.ErrNotExist is wrapped, not InvalidLockError", func() {
				So(errors.Is(err, fs.ErrNotExist), ShouldBeTrue)

				_, invalid := errors.AsType[*lock.InvalidLockError](err)
				So(invalid, ShouldBeFalse)
			})
		})
	})
}

func TestSave(t *testing.T) {
	Convey("Given a valid fixture lock", t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "verger.lock")

		So(fixtureLock().Save(path), ShouldBeNil)

		info, statErr := os.Stat(path)
		So(statErr, ShouldBeNil)

		got, readErr := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file
		So(readErr, ShouldBeNil)

		entries, readDirErr := os.ReadDir(dir)

		Convey("When it is saved", func() {
			Convey("Then bytes are canonical, the mode is 0600 and no temp remains", func() {
				So(string(got), ShouldEqual, string(mustMarshal(t, fixtureLock())))
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o600))
				So(readDirErr, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Name(), ShouldEqual, "verger.lock")
			})
		})

		Convey("When an existing file has a custom mode", func() {
			So(os.Chmod(path, 0o640), ShouldBeNil) //nolint:gosec // G302: the test checks mode preservation
			So(lock.New().Save(path), ShouldBeNil)

			info, err := os.Stat(path)

			Convey("Then the existing mode is preserved", func() {
				So(err, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o640))
			})
		})
	})

	Convey("Given a missing parent directory", t, func() {
		path := filepath.Join(t.TempDir(), "missing", "verger.lock")

		err := lock.New().Save(path)

		Convey("When it is saved", func() {
			Convey("Then it fails without creating anything", func() {
				So(err, ShouldBeError)
				So(errors.Is(err, fs.ErrNotExist), ShouldBeTrue)
			})
		})
	})
}

func TestConcurrentMarshal(t *testing.T) {
	Convey("Given one lock marshaled from several goroutines", t, func() {
		value := fixtureLock()
		want := mustMarshal(t, value)

		Convey("When eight goroutines marshal it", func() {
			results := make([][]byte, 8)

			var group sync.WaitGroup

			for index := range results {
				group.Go(func() {
					data, err := value.Marshal()
					if err != nil {
						t.Errorf("marshal: %v", err)

						return
					}

					results[index] = data
				})
			}

			group.Wait()

			Convey("Then every result matches and cells were not reordered in place", func() {
				for _, got := range results {
					So(string(got), ShouldEqual, string(want))
				}

				So(value.Cells[0].Package, ShouldEqual, "vercel-labs/skills//find-skills")
			})
		})
	})
}

func TestBoundary(t *testing.T) {
	Convey("Given a lock with unicode values and long refs", t, func() {
		value := lock.New()
		value.Snapshot = lock.Snapshot{Digest: digest.Hash(snapshotHex)}

		longRef := strings.Repeat("r", 4096)
		So(value.Upsert(lock.Cell{
			Package:  "acme/рег-☃",
			Host:     "claude",
			Scope:    lock.ScopeUser,
			Version:  "0.9.0-β.1",
			Strategy: lock.StrategyNative,
			Ref:      longRef,
		}), ShouldBeNil)

		Convey("When it is marshaled and parsed back", func() {
			data := mustMarshal(t, value)

			back, err := lock.Parse(data)
			So(err, ShouldBeNil)

			cell, ok := back.Cell("acme/рег-☃", "claude", lock.ScopeUser)

			Convey("Then unicode and long values round-trip", func() {
				So(ok, ShouldBeTrue)
				So(cell.Version, ShouldEqual, "0.9.0-β.1")
				So(cell.Ref, ShouldEqual, longRef)
			})
		})
	})

	Convey("Given a snapshot with only a digest", t, func() {
		value := &lock.Lock{Schema: lock.Schema, Snapshot: lock.Snapshot{Digest: digest.Hash(snapshotHex)}, Cells: []lock.Cell{}}

		Convey("When it is marshaled", func() {
			data := string(mustMarshal(t, value))

			Convey("Then generated_at and key_id are omitted", func() {
				So(data, ShouldContainSubstring, `"digest"`)
				So(data, ShouldNotContainSubstring, "generated_at")
				So(data, ShouldNotContainSubstring, "key_id")
			})
		})
	})

	Convey("Given a lock with a zero snapshot and zero UpdatedAt", t, func() {
		value := lock.New()
		So(value.Upsert(lock.Cell{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyNative}), ShouldBeNil)

		Convey("When it is marshaled", func() {
			data := string(mustMarshal(t, value))

			Convey("Then snapshot and updated_at are omitted", func() {
				So(data, ShouldNotContainSubstring, "snapshot")
				So(data, ShouldNotContainSubstring, "updated_at")
			})
		})
	})
}

func TestSaveRefusesNewerBuiltLock(t *testing.T) {
	Convey("Given a hand-built lock carrying a newer schema", t, func() {
		path := filepath.Join(t.TempDir(), "verger.lock")

		doc := lock.New()
		doc.Schema = lock.Schema + 1

		Convey("When Marshal and Save are called", func() {
			_, marshalErr := doc.Marshal()
			saveErr := doc.Save(path)

			marshalDetail, marshalOK := errors.AsType[*lock.SchemaNewerError](marshalErr)
			saveDetail, saveOK := errors.AsType[*lock.SchemaNewerError](saveErr)

			_, statErr := os.Stat(path)

			Convey("Then both refuse with *SchemaNewerError and write nothing", func() {
				So(marshalOK, ShouldBeTrue)
				So(saveOK, ShouldBeTrue)

				if marshalOK {
					So(marshalDetail.Found, ShouldEqual, lock.Schema+1)
				}

				if saveOK {
					So(saveDetail.Found, ShouldEqual, lock.Schema+1)
				}

				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)
			})
		})
	})
}

func TestSaveReportsStatErrors(t *testing.T) {
	Convey("Given a self-referential symlink at the lock path", t, func() {
		path := filepath.Join(t.TempDir(), "verger.lock")

		So(os.Symlink(path, path), ShouldBeNil)

		Convey("When Save is called", func() {
			err := lock.New().Save(path)

			Convey("Then the stat error surfaces and the link is untouched", func() {
				So(err, ShouldBeError)
				So(errors.Is(err, fs.ErrNotExist), ShouldBeFalse)

				info, statErr := os.Lstat(path)
				So(statErr, ShouldBeNil)
				So(info.Mode()&fs.ModeSymlink, ShouldNotEqual, fs.FileMode(0))
			})
		})
	})
}

func TestInvalidLockErrorMessage(t *testing.T) {
	Convey("Given an InvalidLockError without a cause", t, func() {
		Convey("When its message is rendered", func() {
			Convey("Then the nil cause renders as <nil>, not a malformed verb", func() {
				So((&lock.InvalidLockError{}).Error(), ShouldEqual, "invalid lock: <nil>")
			})
		})
	})
}
