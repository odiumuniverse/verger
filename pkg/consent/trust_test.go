package consent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// projectRoot returns an existing symlink-free absolute project root.
func projectRoot(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	return dir
}

// projectFiles lists every path under root, sorted.
func projectFiles(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		out = append(out, path)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	slices.Sort(out)

	return out
}

func TestTrustLifecycle(t *testing.T) {
	Convey("Given a trust store with a fixed clock", t, func() {
		store := NewTrustStore(filepath.Join(t.TempDir(), "trust.json"), fixedClock())
		project := projectRoot(t)
		hash := testHash("spec-v1")

		Convey("When a project is trusted", func() {
			err := store.Trust(project, hash)

			Convey("Then it is trusted for exactly that spec hash", func() {
				So(err, ShouldBeNil)

				trusted, trustedErr := store.Trusted(project, hash)
				So(trustedErr, ShouldBeNil)
				So(trusted, ShouldBeTrue)

				records, recordsErr := store.Records()
				So(recordsErr, ShouldBeNil)
				So(records, ShouldResemble, []TrustRecord{{Project: project, SpecHash: hash, TrustedAt: fixedAt}})
			})

			Convey("Then a changed spec hash re-asks", func() {
				trusted, trustedErr := store.Trusted(project, testHash("spec-v2"))
				So(trustedErr, ShouldBeNil)
				So(trusted, ShouldBeFalse)
			})

			Convey("When the project is untrusted", func() {
				untrustErr := store.Untrust(project)

				Convey("Then it is no longer trusted and untrust is idempotent", func() {
					So(untrustErr, ShouldBeNil)

					trusted, _ := store.Trusted(project, hash)
					So(trusted, ShouldBeFalse)

					records, _ := store.Records()
					So(records, ShouldBeEmpty)

					So(store.Untrust(project), ShouldBeNil)
				})
			})
		})

		Convey("When an unknown project is checked", func() {
			Convey("Then it is not trusted", func() {
				trusted, err := store.Trusted(project, hash)
				So(err, ShouldBeNil)
				So(trusted, ShouldBeFalse)
			})
		})
	})
}

func TestTrustProjectValidation(t *testing.T) {
	Convey("Given a trust store", t, func() {
		store := NewTrustStore(filepath.Join(t.TempDir(), "trust.json"), fixedClock())

		Convey("When the project root is relative or empty", func() {
			Convey("Then Trust, Untrust and Trusted report InvalidProjectError", func() {
				for _, project := range []string{"", "relative/repo", "./repo"} {
					err := store.Trust(project, testHash("h"))
					_, trustOK := errors.AsType[*InvalidProjectError](err)
					So(trustOK, ShouldBeTrue)

					untrustErr := store.Untrust(project)
					_, untrustOK := errors.AsType[*InvalidProjectError](untrustErr)
					So(untrustOK, ShouldBeTrue)

					_, trustedErr := store.Trusted(project, testHash("h"))
					_, trustedOK := errors.AsType[*InvalidProjectError](trustedErr)
					So(trustedOK, ShouldBeTrue)
				}
			})
		})

		Convey("When the absolute path is lexically messy", func() {
			project := projectRoot(t)
			messy := filepath.Join(project, "sub", "..")

			Convey("Then it normalizes to the same record", func() {
				So(store.Trust(messy, testHash("h")), ShouldBeNil)

				trusted, err := store.Trusted(project, testHash("h"))
				So(err, ShouldBeNil)
				So(trusted, ShouldBeTrue)
			})
		})

		Convey("When the same directory is reached through a symlink", func() {
			project := projectRoot(t)
			link := filepath.Join(t.TempDir(), "repo-link")
			So(os.Symlink(project, link), ShouldBeNil)

			Convey("Then both spellings share one trust record", func() {
				So(store.Trust(link, testHash("h")), ShouldBeNil)

				trusted, err := store.Trusted(project, testHash("h"))
				So(err, ShouldBeNil)
				So(trusted, ShouldBeTrue)

				records, _ := store.Records()
				So(records, ShouldHaveLength, 1)
				So(records[0].Project, ShouldEqual, project)
			})
		})
	})
}

func TestTrustSaveLoadRoundTrip(t *testing.T) {
	Convey("Given a trust store with two projects", t, func() {
		dir := filepath.Join(t.TempDir(), "state")
		path := filepath.Join(dir, "trust.json")
		store := NewTrustStore(path, fixedClock())

		rootA := projectRoot(t)
		rootB := projectRoot(t)
		hashA := testHash("a")
		hashB := testHash("b")

		So(store.Trust(rootA, hashA), ShouldBeNil)
		So(store.Trust(rootB, hashB), ShouldBeNil)

		Convey("When saved", func() {
			So(store.Save(), ShouldBeNil)

			Convey("Then the file is 0600 and Records sorts by project", func() {
				assertMode(t, path, 0o600)
				assertMode(t, dir, 0o700)

				records, err := store.Records()
				So(err, ShouldBeNil)
				So(records, ShouldHaveLength, 2)
				So(records[0].Project < records[1].Project, ShouldBeTrue)

				var doc map[string]json.RawMessage
				So(json.Unmarshal(testFile(t, path), &doc), ShouldBeNil)
				So(string(doc["schema"]), ShouldEqualJSON, "1")
				So(string(doc["projects"]), ShouldContainSubstring, "spec_hash")
			})

			Convey("When a fresh store loads it", func() {
				restarted := NewTrustStore(path, fixedClock())
				So(restarted.Load(), ShouldBeNil)

				Convey("Then both records survive byte-identically on re-save", func() {
					first := testFile(t, path)

					So(restarted.Save(), ShouldBeNil)
					So(testFile(t, path), ShouldResemble, first)

					trusted, err := restarted.Trusted(rootA, hashA)
					So(err, ShouldBeNil)
					So(trusted, ShouldBeTrue)
				})
			})
		})
	})

	Convey("Given a trust file with unknown fields", t, func() {
		path := filepath.Join(t.TempDir(), "trust.json")
		root := projectRoot(t)
		raw := `{"schema":1,"future_top":true,"projects":{"` + root + `":{"project":"` + root +
			`","spec_hash":"` + string(testHash("a")) + `","trusted_at":"2026-09-25T15:04:05Z","future_rec":{"b":2}}}}`
		So(os.WriteFile(path, []byte(raw), 0o600), ShouldBeNil)

		Convey("When loaded and re-saved", func() {
			store := NewTrustStore(path, fixedClock())
			So(store.Load(), ShouldBeNil)
			So(store.Save(), ShouldBeNil)

			Convey("Then unknown fields survive", func() {
				var doc map[string]json.RawMessage
				So(json.Unmarshal(testFile(t, path), &doc), ShouldBeNil)
				So(string(doc["future_top"]), ShouldEqualJSON, "true")

				var projects map[string]map[string]json.RawMessage
				So(json.Unmarshal(doc["projects"], &projects), ShouldBeNil)
				So(string(projects[root]["future_rec"]), ShouldEqualJSON, `{"b":2}`)
			})
		})
	})
}

func TestTrustCorruptAndSchema(t *testing.T) {
	Convey("Given a trust store path", t, func() {
		path := filepath.Join(t.TempDir(), "trust.json")

		Convey("When the file is missing", func() {
			store := NewTrustStore(path, fixedClock())

			Convey("Then Load reports an empty store", func() {
				So(store.Load(), ShouldBeNil)

				records, err := store.Records()
				So(err, ShouldBeNil)
				So(records, ShouldBeEmpty)
			})
		})

		Convey("When the JSON is broken", func() {
			So(os.WriteFile(path, []byte("{not json"), 0o600), ShouldBeNil)
			err := NewTrustStore(path, fixedClock()).Load()

			Convey("Then it reports ConsentParseError and the bytes stay intact", func() {
				_, ok := errors.AsType[*ConsentParseError](err)
				So(ok, ShouldBeTrue)
				So(testFile(t, path), ShouldResemble, []byte("{not json"))
			})
		})

		Convey("When the schema is newer", func() {
			So(os.WriteFile(path, []byte(`{"schema":2,"projects":{}}`), 0o600), ShouldBeNil)
			err := NewTrustStore(path, fixedClock()).Load()

			Convey("Then it reports SchemaNewerError", func() {
				target, ok := errors.AsType[*SchemaNewerError](err)
				So(ok, ShouldBeTrue)
				So(target.Found, ShouldEqual, 2)
				So(target.Supported, ShouldEqual, Schema)
			})
		})

		Convey("When the schema is zero", func() {
			So(os.WriteFile(path, []byte(`{"projects":{}}`), 0o600), ShouldBeNil)
			err := NewTrustStore(path, fixedClock()).Load()

			Convey("Then it reports SchemaInvalidError", func() {
				_, ok := errors.AsType[*SchemaInvalidError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When a record key does not match its record", func() {
			raw := `{"schema":1,"projects":{"/a/b":{"project":"/a/c","spec_hash":"` + string(testHash("a")) + `"}}}`
			So(os.WriteFile(path, []byte(raw), 0o600), ShouldBeNil)
			err := NewTrustStore(path, fixedClock()).Load()

			Convey("Then it reports ConsentParseError", func() {
				_, ok := errors.AsType[*ConsentParseError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestSpecHash(t *testing.T) {
	Convey("Given a project spec", t, func() {
		base := []byte("schema = 1\n\n[[package]]\nid = \"acme/foo\"\nversion = \"1.0.0\"\n")

		parsed, err := spec.Parse(base)
		So(err, ShouldBeNil)

		Convey("When hashed", func() {
			hash := SpecHash(parsed)

			Convey("Then it equals the canonical spec digest", func() {
				So(hash, ShouldEqual, parsed.Digest())
			})

			Convey("Then comment-only edits keep the hash (D11 canonical)", func() {
				commented, parseErr := spec.Parse([]byte("# a comment\nschema = 1\n\n[[package]]\nid = \"acme/foo\"\nversion = \"1.0.0\"\n"))
				So(parseErr, ShouldBeNil)
				So(SpecHash(commented), ShouldEqual, hash)
			})

			Convey("Then a semantic edit changes the hash", func() {
				edited, parseErr := spec.Parse([]byte("schema = 1\n\n[[package]]\nid = \"acme/foo\"\nversion = \"2.0.0\"\n"))
				So(parseErr, ShouldBeNil)
				So(SpecHash(edited), ShouldNotEqual, hash)
			})

			Convey("Then a nil spec hashes to the zero value", func() {
				So(SpecHash(nil), ShouldEqual, digest.Hash(""))
			})
		})
	})
}

func TestTrustSemanticReAsk(t *testing.T) {
	Convey("Given a trusted project", t, func() {
		store := NewTrustStore(filepath.Join(t.TempDir(), "trust.json"), fixedClock())
		project := projectRoot(t)

		original, err := spec.Parse([]byte("schema = 1\n\n[[package]]\nid = \"acme/foo\"\nversion = \"1.0.0\"\n"))
		So(err, ShouldBeNil)
		So(store.Trust(project, SpecHash(original)), ShouldBeNil)

		Convey("When only comments change", func() {
			commented, parseErr := spec.Parse([]byte("# note\nschema = 1\n\n[[package]]\nid = \"acme/foo\"\nversion = \"1.0.0\"\n"))
			So(parseErr, ShouldBeNil)

			trusted, trustedErr := store.Trusted(project, SpecHash(commented))
			So(trustedErr, ShouldBeNil)

			Convey("Then the project stays trusted", func() {
				So(trusted, ShouldBeTrue)
			})
		})

		Convey("When the spec changes semantically", func() {
			edited, parseErr := spec.Parse([]byte("schema = 1\n\n[[package]]\nid = \"acme/foo\"\nversion = \"2.0.0\"\n"))
			So(parseErr, ShouldBeNil)

			trusted, trustedErr := store.Trusted(project, SpecHash(edited))
			So(trustedErr, ShouldBeNil)

			Convey("Then the project is untrusted and re-asked", func() {
				So(trusted, ShouldBeFalse)
			})
		})
	})
}

func TestTrustNeverWritesIntoProject(t *testing.T) {
	Convey("Given a project directory with a committed spec", t, func() {
		project := projectRoot(t)
		committed := filepath.Join(project, "verger.toml")
		So(os.WriteFile(committed, []byte("schema = 1\n"), 0o600), ShouldBeNil)

		before := projectFiles(t, project)

		Convey("When the project is trusted through the home-state store", func() {
			store := NewTrustStore(filepath.Join(t.TempDir(), "home", "state", "trust.json"), fixedClock())
			So(store.Trust(project, testHash("h")), ShouldBeNil)
			So(store.Save(), ShouldBeNil)

			Convey("Then nothing is written into the project tree", func() {
				So(projectFiles(t, project), ShouldResemble, before)
				So(testFile(t, committed), ShouldResemble, []byte("schema = 1\n"))

				// The generated non-committable project files
				// (settings.local.json, .git/info/exclude) are T1.12's write
				// policy; consent itself only records in the home state.
				assertMissing(t, filepath.Join(project, ".claude", "settings.local.json"))
				assertMissing(t, filepath.Join(project, ".git", "info", "exclude"))
			})
		})
	})
}

func TestTrustUnicodePaths(t *testing.T) {
	Convey("Given a project path with unicode", t, func() {
		path := filepath.Join(t.TempDir(), "trust.json")
		store := NewTrustStore(path, fixedClock())
		project := filepath.Join(projectRoot(t), "репо-☃")
		So(os.MkdirAll(project, 0o700), ShouldBeNil)

		hash := testHash("юникод")
		So(store.Trust(project, hash), ShouldBeNil)
		So(store.Save(), ShouldBeNil)

		Convey("When reloaded", func() {
			restarted := NewTrustStore(path, fixedClock())
			So(restarted.Load(), ShouldBeNil)

			Convey("Then the unicode project round-trips", func() {
				trusted, err := restarted.Trusted(project, hash)
				So(err, ShouldBeNil)
				So(trusted, ShouldBeTrue)
			})
		})
	})
}
