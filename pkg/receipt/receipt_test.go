package receipt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// testHash builds a deterministic digest for fixtures.
func testHash(seed string) digest.Hash {
	return digest.Bytes([]byte(seed))
}

// sampleReceipt returns a fully populated receipt with sorted artifacts.
func sampleReceipt() Receipt {
	at := time.Date(2026, 9, 25, 12, 30, 45, 123456789, time.UTC)

	return Receipt{
		Schema:      Schema,
		Package:     "owner/name",
		Host:        "claude",
		Scope:       ScopeUser,
		Strategy:    "synth",
		Version:     "1.2.3",
		CapsHash:    "caps-1",
		InstalledAt: at,
		UpdatedAt:   at.Add(time.Hour),
		Artifacts: []Artifact{
			{Kind: "command", Name: "run", Path: "/pkg/commands/run.md", Digest: testHash("command")},
			{Kind: "skill", Name: "guard", Path: "/pkg/skills/guard", Digest: testHash("skill")},
		},
		RMA: []Op{
			{Kind: OpCopyTree, Path: "/pkg/skills/guard", Digest: testHash("skill")},
			{
				Kind: OpConfigKey, Path: "/home/u/.claude/settings.json", KeyPath: "hooks",
				Digest: testHash("hooks"), Existed: true, Backup: "trash-1",
			},
		},
	}
}

// readTestFile reads a file the test itself created.
func readTestFile(t *testing.T, path string) []byte {
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

// checkGolden compares got with the golden file, or rewrites it under
// UPDATE_GOLDEN=1.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}

		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}

		return
	}

	want := readTestFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch %s:\n got: %s\nwant: %s", path, got, want)
	}
}

// caseSensitiveFS reports whether dir's filesystem distinguishes names that
// differ only by case; it probes by writing one name and stat-ing its folded
// variant, so a test can assert the matching invariant on either filesystem
// instead of skipping.
func caseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create case probe dir %s: %v", dir, err)
	}

	upper := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(upper, []byte("probe"), 0o600); err != nil {
		t.Fatalf("write case probe %s: %v", upper, err)
	}

	_, err := os.Stat(filepath.Join(dir, "caseprobe"))

	return errors.Is(err, fs.ErrNotExist)
}

func TestReceiptPutGetDelete(t *testing.T) {
	Convey("Given a receipt store", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		Convey("When a receipt is put", func() {
			want := sampleReceipt()

			err := store.Put(want)

			Convey("Then it lands per the layout with the spec'd modes", func() {
				So(err, ShouldBeNil)

				cell := filepath.Join(dir, "owner", "name", "claude-user.json")
				assertMode(t, cell, 0o600)
				assertMode(t, filepath.Join(dir, "owner", "name"), 0o700)
				assertMode(t, filepath.Join(dir, "owner"), 0o700)

				got, ok, getErr := store.Get("owner/name", "claude", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got, ShouldResemble, want)

				list, listErr := store.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldResemble, []Receipt{want})
			})

			Convey("Then an unknown cell reports missing without an error", func() {
				So(err, ShouldBeNil)

				got, ok, getErr := store.Get("owner/name", "codex", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeFalse)
				So(got, ShouldResemble, Receipt{})
			})
		})

		Convey("When a receipt is deleted twice", func() {
			So(store.Put(sampleReceipt()), ShouldBeNil)

			first := store.Delete("owner/name", "claude", ScopeUser)
			second := store.Delete("owner/name", "claude", ScopeUser)

			Convey("Then deletion is idempotent and empty dirs are cleaned up", func() {
				So(first, ShouldBeNil)
				So(second, ShouldBeNil)
				assertMissing(t, filepath.Join(dir, "owner"))

				_, ok, _ := store.Get("owner/name", "claude", ScopeUser)
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When a receipt is put twice", func() {
			first := sampleReceipt()
			second := sampleReceipt()
			second.Version = "2.0.0"
			second.UpdatedAt = second.UpdatedAt.Add(time.Hour)

			So(store.Put(first), ShouldBeNil)
			err := store.Put(second)

			Convey("Then the file is replaced atomically and idempotently", func() {
				So(err, ShouldBeNil)

				got, ok, _ := store.Get("owner/name", "claude", ScopeUser)
				So(ok, ShouldBeTrue)
				So(got.Version, ShouldEqual, "2.0.0")

				entries, readErr := os.ReadDir(filepath.Join(dir, "owner", "name"))
				So(readErr, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
			})
		})

		Convey("When a receipt with an unsafe package id is put", func() {
			r := sampleReceipt()
			r.Package = "../evil"

			err := store.Put(r)

			Convey("Then it is rejected as an invalid key and nothing is written", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*InvalidKeyError](err)
				So(ok, ShouldBeTrue)
				So(target.Field, ShouldEqual, "package")
				assertMissing(t, dir)
			})
		})
	})
}

func TestReceiptInvalidKeys(t *testing.T) {
	Convey("Given a receipt store", t, func() {
		store := NewStore(filepath.Join(t.TempDir(), "receipts"))

		type keyCase struct {
			field string
			pkg   string
			host  string
			scope string
		}

		keys := []keyCase{
			{"package", "", "claude", ScopeUser},
			{"package", "a//", "claude", ScopeUser},
			{"package", "a///b", "claude", ScopeUser},
			{"package", "/abs", "claude", ScopeUser},
			{"package", `a\b`, "claude", ScopeUser},
			{"host", "owner/name", "a/b", ScopeUser},
			{"host", "owner/name", "..", ScopeUser},
			{"host", "owner/name", "", ScopeUser},
			{"scope", "owner/name", "claude", "other"},
			{"scope", "owner/name", "claude", ".."},
			{"scope", "owner/name", "claude", ""},
		}

		unsafeButShaped := keys[1:7]
		shapeKeys := append([]keyCase{keys[0]}, keys[7:]...)

		for _, tc := range unsafeButShaped {
			Convey("When Put gets the unsafe key "+tc.pkg+"/"+tc.host+"/"+tc.scope, func() {
				r := sampleReceipt()
				r.Package, r.Host, r.Scope = tc.pkg, tc.host, tc.scope

				err := store.Put(r)

				Convey("Then it reports InvalidKeyError", func() {
					target, ok := errors.AsType[*InvalidKeyError](err)
					So(ok, ShouldBeTrue)
					So(target.Field, ShouldEqual, tc.field)
				})
			})
		}

		for _, tc := range shapeKeys {
			Convey("When Put gets the shape-invalid key "+tc.pkg+"/"+tc.host+"/"+tc.scope, func() {
				r := sampleReceipt()
				r.Package, r.Host, r.Scope = tc.pkg, tc.host, tc.scope

				err := store.Put(r)

				Convey("Then shape validation wins and reports InvalidReceiptError", func() {
					_, ok := errors.AsType[*InvalidReceiptError](err)
					So(ok, ShouldBeTrue)
				})
			})
		}

		for _, tc := range keys {
			Convey("When Get and Delete get the invalid key "+tc.pkg+"/"+tc.host+"/"+tc.scope, func() {
				_, _, getErr := store.Get(tc.pkg, tc.host, tc.scope)
				deleteErr := store.Delete(tc.pkg, tc.host, tc.scope)

				Convey("Then they report InvalidKeyError", func() {
					for _, err := range []error{getErr, deleteErr} {
						target, ok := errors.AsType[*InvalidKeyError](err)
						So(ok, ShouldBeTrue)
						So(target.Field, ShouldEqual, tc.field)
					}
				})
			})
		}
	})
}

func TestReceiptSubpathPackage(t *testing.T) {
	Convey("Given a receipt for a canonical subpath id", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		r := sampleReceipt()
		r.Package = "vercel-labs/skills//find-skills"

		Convey("When it is put, got and listed", func() {
			So(store.Put(r), ShouldBeNil)

			got, ok, err := store.Get(r.Package, r.Host, r.Scope)
			So(err, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(got.Package, ShouldEqual, r.Package)

			list, listErr := store.List()
			So(listErr, ShouldBeNil)
			So(list, ShouldHaveLength, 1)
			So(list[0].Package, ShouldEqual, r.Package)

			_, statErr := os.Stat(filepath.Join(dir, "vercel-labs", "skills", "find-skills", "claude-user.json"))
			So(statErr, ShouldBeNil)
		})

		Convey("When an id that maps to the same cell is put", func() {
			So(store.Put(r), ShouldBeNil)

			other := sampleReceipt()
			other.Package = "vercel-labs/skills/find-skills"

			err := store.Put(other)

			Convey("Then the write is refused as a key collision", func() {
				_, ok := errors.AsType[*KeyCollisionError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestReceiptValidation(t *testing.T) {
	Convey("Given receipt validation rules", t, func() {
		base := sampleReceipt()

		cases := []struct {
			name   string
			mutate func(r *Receipt)
		}{
			{"empty package", func(r *Receipt) { r.Package = "" }},
			{"empty host", func(r *Receipt) { r.Host = "" }},
			{"empty scope", func(r *Receipt) { r.Scope = "" }},
			{"unknown scope", func(r *Receipt) { r.Scope = "global" }},
			{"empty strategy", func(r *Receipt) { r.Strategy = "" }},
			{"duplicate artifact paths", func(r *Receipt) { r.Artifacts[1].Path = r.Artifacts[0].Path }},
			{"artifact empty kind", func(r *Receipt) { r.Artifacts[0].Kind = "" }},
			{"artifact empty name", func(r *Receipt) { r.Artifacts[0].Name = "" }},
			{"artifact empty path", func(r *Receipt) { r.Artifacts[0].Path = "" }},
			{"artifact invalid digest", func(r *Receipt) { r.Artifacts[0].Digest = "nope" }},
			{"op unknown kind", func(r *Receipt) { r.RMA[0].Kind = "explode" }},
			{"op relative path", func(r *Receipt) { r.RMA[0].Path = "relative/path" }},
			{"config-key missing key path", func(r *Receipt) { r.RMA[1].KeyPath = "" }},
			{"config-key empty path", func(r *Receipt) { r.RMA[1].Path = "" }},
			{"host-install empty command", func(r *Receipt) { r.RMA[0] = Op{Kind: OpHostInstall} }},
			{"op invalid digest", func(r *Receipt) { r.RMA[0].Digest = "zz" }},
		}

		for _, tc := range cases {
			Convey("When "+tc.name, func() {
				r := base
				r.Artifacts = append([]Artifact(nil), base.Artifacts...)
				r.RMA = append([]Op(nil), base.RMA...)
				tc.mutate(&r)

				validateErr := r.Validate()

				Convey("Then Validate and Put reject it with InvalidReceiptError", func() {
					So(validateErr, ShouldBeError)

					_, ok := errors.AsType[*InvalidReceiptError](validateErr)
					So(ok, ShouldBeTrue)

					store := NewStore(filepath.Join(t.TempDir(), "receipts"))

					putErr := store.Put(r)
					_, putOK := errors.AsType[*InvalidReceiptError](putErr)
					So(putOK, ShouldBeTrue)

					list, listErr := store.List()
					So(listErr, ShouldBeNil)
					So(list, ShouldBeEmpty)
				})
			})
		}
	})
}

func TestReceiptArtifactsSorted(t *testing.T) {
	Convey("Given a receipt with unsorted artifacts", t, func() {
		r := sampleReceipt()
		r.Artifacts = []Artifact{
			{Kind: "skill", Name: "z", Path: "/z", Digest: testHash("z")},
			{Kind: "command", Name: "b", Path: "/b", Digest: testHash("b")},
			{Kind: "command", Name: "a", Path: "/a", Digest: testHash("a")},
		}

		store := NewStore(filepath.Join(t.TempDir(), "receipts"))

		Convey("When it is put", func() {
			err := store.Put(r)

			Convey("Then artifacts are stored sorted by kind, name and path", func() {
				So(err, ShouldBeNil)

				got, ok, _ := store.Get(r.Package, r.Host, r.Scope)
				So(ok, ShouldBeTrue)
				So(got.Artifacts, ShouldResemble, []Artifact{
					{Kind: "command", Name: "a", Path: "/a", Digest: testHash("a")},
					{Kind: "command", Name: "b", Path: "/b", Digest: testHash("b")},
					{Kind: "skill", Name: "z", Path: "/z", Digest: testHash("z")},
				})
			})
		})
	})
}

func TestReceiptListSorting(t *testing.T) {
	Convey("Given several receipts and junk files", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		mk := func(pkg, host, scope string) Receipt {
			r := sampleReceipt()
			r.Package, r.Host, r.Scope = pkg, host, scope

			So(store.Put(r), ShouldBeNil)

			return r
		}

		bHost := mk("b/two", "claude", ScopeUser)
		aUser := mk("a/one", "codex", ScopeUser)
		aProject := mk("a/one", "codex", ScopeProject)
		aSamePkg := mk("a/one", "claude", ScopeUser)

		scopeDir := filepath.Join(dir, "a", "one")
		So(os.WriteFile(filepath.Join(scopeDir, ".claude-user.json.tmp-abc"), []byte("tmp"), 0o600), ShouldBeNil)
		So(os.WriteFile(filepath.Join(scopeDir, "notes.txt"), []byte("x"), 0o600), ShouldBeNil)
		So(os.Mkdir(filepath.Join(scopeDir, "subdir"), 0o700), ShouldBeNil)

		Convey("When the store is listed", func() {
			list, err := store.List()

			Convey("Then entries sort by package, host and scope with junk ignored", func() {
				So(err, ShouldBeNil)
				So(list, ShouldResemble, []Receipt{aSamePkg, aProject, aUser, bHost})
			})
		})
	})
}

func TestReceiptCorruptAndSchema(t *testing.T) {
	Convey("Given a receipt store", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		writeCell := func(host, content string) string {
			cell := filepath.Join(dir, "owner", "name", host+"-"+ScopeUser+".json")
			So(os.MkdirAll(filepath.Dir(cell), 0o700), ShouldBeNil)
			So(os.WriteFile(cell, []byte(content), 0o600), ShouldBeNil)

			return cell
		}

		Convey("When the cell JSON is broken", func() {
			writeCell("claude", "{not json")

			_, _, getErr := store.Get("owner/name", "claude", ScopeUser)
			_, listErr := store.List()

			Convey("Then Get and List report CorruptReceiptError", func() {
				_, getOK := errors.AsType[*CorruptReceiptError](getErr)
				So(getOK, ShouldBeTrue)

				_, listOK := errors.AsType[*CorruptReceiptError](listErr)
				So(listOK, ShouldBeTrue)
			})
		})

		Convey("When the cell schema is newer", func() {
			r := sampleReceipt()
			r.Schema = 2

			data, err := json.Marshal(r)
			So(err, ShouldBeNil)
			writeCell("claude", string(data))

			_, _, getErr := store.Get("owner/name", "claude", ScopeUser)
			_, listErr := store.List()

			Convey("Then Get and List report SchemaNewerError", func() {
				target, ok := errors.AsType[*SchemaNewerError](getErr)
				So(ok, ShouldBeTrue)
				So(target.Found, ShouldEqual, 2)
				So(target.Supported, ShouldEqual, Schema)

				_, listOK := errors.AsType[*SchemaNewerError](listErr)
				So(listOK, ShouldBeTrue)
			})
		})

		Convey("When the cell schema is zero", func() {
			r := sampleReceipt()
			r.Schema = 0

			data, err := json.Marshal(r)
			So(err, ShouldBeNil)
			writeCell("claude", string(data))

			_, _, getErr := store.Get("owner/name", "claude", ScopeUser)

			Convey("Then Get reports CorruptReceiptError", func() {
				_, ok := errors.AsType[*CorruptReceiptError](getErr)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the cell content does not match its key", func() {
			r := sampleReceipt()
			r.Host = "codex"

			data, err := json.Marshal(r)
			So(err, ShouldBeNil)
			writeCell("claude", string(data))

			_, _, getErr := store.Get("owner/name", "claude", ScopeUser)
			_, listErr := store.List()

			Convey("Then both report CorruptReceiptError", func() {
				_, getOK := errors.AsType[*CorruptReceiptError](getErr)
				So(getOK, ShouldBeTrue)

				_, listOK := errors.AsType[*CorruptReceiptError](listErr)
				So(listOK, ShouldBeTrue)
			})
		})

		Convey("When the cell digest is malformed", func() {
			writeCell("claude", `{"schema":1,"package":"owner/name","host":"claude","scope":"user","strategy":"loose",`+
				`"artifacts":[{"kind":"skill","name":"a","path":"/a","digest":"not-hex"}]}`)

			_, _, getErr := store.Get("owner/name", "claude", ScopeUser)

			Convey("Then Get reports CorruptReceiptError", func() {
				_, ok := errors.AsType[*CorruptReceiptError](getErr)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestReceiptExtrasRoundTrip(t *testing.T) {
	Convey("Given a receipt file with future fields", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		cell := filepath.Join(dir, "owner", "name", "claude-user.json")
		So(os.MkdirAll(filepath.Dir(cell), 0o700), ShouldBeNil)

		future := `{"schema":1,"package":"owner/name","host":"claude","scope":"user","strategy":"loose",` +
			`"future_field":{"a":1},"installed_at":"2026-09-25T12:30:45Z"}`
		So(os.WriteFile(cell, []byte(future), 0o600), ShouldBeNil)

		Convey("When the receipt is read and rewritten", func() {
			got, ok, getErr := store.Get("owner/name", "claude", ScopeUser)
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeTrue)

			got.Version = "2.0.0"
			putErr := store.Put(got)

			Convey("Then the unknown field survives the rewrite", func() {
				So(putErr, ShouldBeNil)

				raw := readTestFile(t, cell)

				var doc map[string]json.RawMessage
				So(json.Unmarshal(raw, &doc), ShouldBeNil)
				So(string(doc["future_field"]), ShouldEqualJSON, `{"a":1}`)
				So(string(doc["version"]), ShouldEqualJSON, `"2.0.0"`)
			})
		})
	})
}

func TestReceiptOpKindsRoundTrip(t *testing.T) {
	Convey("Given every RMA op kind", t, func() {
		store := NewStore(filepath.Join(t.TempDir(), "receipts"))

		r := sampleReceipt()
		r.RMA = []Op{
			{Kind: OpWriteFile, Path: "/a/file", Digest: testHash("f"), Mode: 0o600, Existed: true, Backup: "trash-f"},
			{Kind: OpCopyTree, Path: "/a/tree", Digest: testHash("t")},
			{Kind: OpSymlink, Path: "/a/link", Digest: testHash("l")},
			{Kind: OpHardlink, Path: "/a/hard", Digest: testHash("h")},
			{Kind: OpConfigKey, Path: "/a/config.json", KeyPath: "mcpServers.x", Digest: testHash("c")},
			{Kind: OpHostInstall, Command: []string{"claude", "plugin", "uninstall", "x@y"}},
		}

		Convey("When the receipt round-trips", func() {
			So(store.Put(r), ShouldBeNil)

			got, ok, err := store.Get(r.Package, r.Host, r.Scope)

			Convey("Then every op survives byte-for-byte", func() {
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got, ShouldResemble, r)
			})
		})
	})
}

func TestReceiptOmittedZeroFields(t *testing.T) {
	Convey("Given a minimal receipt", t, func() {
		store := NewStore(filepath.Join(t.TempDir(), "receipts"))

		r := Receipt{Package: "p/x", Host: "claude", Scope: ScopeUser, Strategy: "loose"}
		So(store.Put(r), ShouldBeNil)

		Convey("When the file is inspected", func() {
			raw := readTestFile(t, filepath.Join(store.dir, "p", "x", "claude-user.json"))

			var doc map[string]json.RawMessage
			So(json.Unmarshal(raw, &doc), ShouldBeNil)

			Convey("Then zero times and empty lists are omitted", func() {
				// `installed_at` and `updated_at` are deliberately absent from
				// this list: Put stamps both on every write, so a written
				// receipt never has them zero and the update cooldown has
				// something to measure from. The `omitzero` contract is still
				// covered by the keys that remain.
				for _, key := range []string{"artifacts", "rma", "version", "caps_hash", "runtime_version"} {
					_, present := doc[key]
					So(present, ShouldBeFalse)
				}

				Convey("And the two stamps are present, because Put writes them", func() {
					for _, key := range []string{"installed_at", "updated_at"} {
						_, present := doc[key]
						So(present, ShouldBeTrue)
					}
				})

				So(string(doc["schema"]), ShouldEqualJSON, "1")
			})
		})
	})
}

func TestReceiptGolden(t *testing.T) {
	Convey("Given a fixed receipt", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		r := sampleReceipt()
		So(store.Put(r), ShouldBeNil)

		Convey("When it is written", func() {
			raw := readTestFile(t, filepath.Join(dir, "owner", "name", "claude-user.json"))

			Convey("Then the file matches the golden fixture", func() {
				checkGolden(t, filepath.Join("testdata", "receipt.golden.json"), raw)
			})
		})
	})
}

func TestReceiptRestartIdempotent(t *testing.T) {
	Convey("Given a receipt store on disk", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		So(NewStore(dir).Put(sampleReceipt()), ShouldBeNil)

		first := readTestFile(t, filepath.Join(dir, "owner", "name", "claude-user.json"))

		Convey("When a fresh store instance re-reads and re-puts it", func() {
			restarted := NewStore(dir)
			got, ok, err := restarted.Get("owner/name", "claude", ScopeUser)
			So(err, ShouldBeNil)
			So(ok, ShouldBeTrue)

			So(restarted.Put(got), ShouldBeNil)

			Convey("Then the bytes are unchanged", func() {
				second := readTestFile(t, filepath.Join(dir, "owner", "name", "claude-user.json"))
				So(second, ShouldResemble, first)
			})
		})
	})
}

func TestReceiptPackageIDsAreInjective(t *testing.T) {
	Convey("Given package ids that differ only in separator vs literal underscores", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		slashed := sampleReceipt()
		slashed.Package = "acme/foo"

		literal := sampleReceipt()
		literal.Package = "acme__foo"

		slashedPath := filepath.Join(dir, "acme", "foo", "claude-user.json")
		literalPath := filepath.Join(dir, "acme__foo", "claude-user.json")

		Convey("When both receipts are put", func() {
			So(store.Put(slashed), ShouldBeNil)
			So(store.Put(literal), ShouldBeNil)

			Convey("Then they land in distinct nested paths with distinct bytes", func() {
				assertMode(t, slashedPath, 0o600)
				assertMode(t, literalPath, 0o600)

				So(readTestFile(t, slashedPath), ShouldNotResemble, readTestFile(t, literalPath))
			})

			Convey("Then each is individually retrievable without cross-talk", func() {
				got, ok, err := store.Get("acme/foo", "claude", ScopeUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Package, ShouldEqual, "acme/foo")

				got, ok, err = store.Get("acme__foo", "claude", ScopeUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Package, ShouldEqual, "acme__foo")
			})

			Convey("Then List returns both entries", func() {
				list, err := store.List()
				So(err, ShouldBeNil)
				So(list, ShouldHaveLength, 2)

				packages := []string{list[0].Package, list[1].Package}
				So(packages, ShouldResemble, []string{"acme/foo", "acme__foo"})
			})

			Convey("Then deleting one never touches the other", func() {
				So(store.Delete("acme/foo", "claude", ScopeUser), ShouldBeNil)

				_, ok, err := store.Get("acme/foo", "claude", ScopeUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)

				got, ok, err := store.Get("acme__foo", "claude", ScopeUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Package, ShouldEqual, "acme__foo")

				assertMissing(t, filepath.Join(dir, "acme"))
			})
		})

		Convey("When a receipt uses a deeply nested package id", func() {
			deep := sampleReceipt()
			deep.Package = "a/b/c"
			deep.Scope = ScopeProject

			So(store.Put(deep), ShouldBeNil)

			Convey("Then the layout nests every segment and varies the leaf with scope", func() {
				cell := filepath.Join(dir, "a", "b", "c", "claude-project.json")
				assertMode(t, cell, 0o600)
				assertMode(t, filepath.Join(dir, "a", "b", "c"), 0o700)

				got, ok, err := store.Get("a/b/c", "claude", ScopeProject)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Package, ShouldEqual, "a/b/c")
			})
		})

		Convey("When a registry-style single-segment id is used", func() {
			single := sampleReceipt()
			single.Package = "mcp:io.github.github/github-mcp-server"

			So(store.Put(single), ShouldBeNil)

			Convey("Then it nests by its single segment and stays retrievable", func() {
				cell := filepath.Join(dir, "mcp:io.github.github", "github-mcp-server", "claude-user.json")
				assertMode(t, cell, 0o600)

				got, ok, err := store.Get(single.Package, "claude", ScopeUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Package, ShouldEqual, single.Package)
			})
		})
	})
}

func TestReceiptPutDoesNotReorderCallerArtifacts(t *testing.T) {
	Convey("Given a receipt whose artifacts are not sorted", t, func() {
		store := NewStore(filepath.Join(t.TempDir(), "receipts"))

		r := sampleReceipt()
		r.Artifacts = []Artifact{
			{Kind: "skill", Name: "z", Path: "/z", Digest: testHash("z")},
			{Kind: "command", Name: "b", Path: "/b", Digest: testHash("b")},
			{Kind: "command", Name: "a", Path: "/a", Digest: testHash("a")},
		}
		callerOrder := append([]Artifact(nil), r.Artifacts...)

		Convey("When the receipt is put", func() {
			err := store.Put(r)

			Convey("Then the caller's slice keeps its original order", func() {
				So(err, ShouldBeNil)
				So(r.Artifacts, ShouldResemble, callerOrder)

				got, ok, _ := store.Get(r.Package, r.Host, r.Scope)
				So(ok, ShouldBeTrue)
				So(got.Artifacts[0].Name, ShouldEqual, "a")
			})
		})
	})
}

func TestReceiptCaseVariantPackageCollision(t *testing.T) {
	Convey("Given a store on a filesystem of probed case sensitivity", t, func() {
		root := t.TempDir()
		dir := filepath.Join(root, "receipts")
		store := NewStore(dir)
		caseSensitive := caseSensitiveFS(t, filepath.Join(root, "case-probe"))
		t.Logf("filesystem case-sensitive=%v", caseSensitive)

		first := sampleReceipt()
		first.Package = "acme/foo"
		first.Version = "1.0.0"

		second := sampleReceipt()
		second.Package = "ACME/foo"
		second.Version = "2.0.0"

		So(store.Put(first), ShouldBeNil)

		cell := filepath.Join(dir, "acme", "foo", "claude-user.json")
		stored := readTestFile(t, cell)

		Convey("When a case-variant package id is put", func() {
			err := store.Put(second)

			Convey("Then the probed filesystem property decides the invariant", func() {
				if caseSensitive {
					So(err, ShouldBeNil)

					got, ok, getErr := store.Get("acme/foo", "claude", ScopeUser)
					So(getErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(got.Version, ShouldEqual, "1.0.0")

					got, ok, getErr = store.Get("ACME/foo", "claude", ScopeUser)
					So(getErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(got.Version, ShouldEqual, "2.0.0")

					So(readTestFile(t, cell), ShouldResemble, stored)

					list, listErr := store.List()
					So(listErr, ShouldBeNil)
					So(list, ShouldHaveLength, 2)

					return
				}

				collision, collisionOK := errors.AsType[*KeyCollisionError](err)
				So(collisionOK, ShouldBeTrue)
				So(collision.Package, ShouldEqual, "ACME/foo")
				So(collision.Host, ShouldEqual, "claude")
				So(collision.Scope, ShouldEqual, ScopeUser)
				So(collision.Existing.Package, ShouldEqual, "acme/foo")
				So(collision.Existing.Host, ShouldEqual, "claude")
				So(collision.Existing.Scope, ShouldEqual, ScopeUser)

				got, ok, getErr := store.Get("acme/foo", "claude", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Version, ShouldEqual, "1.0.0")
				So(readTestFile(t, cell), ShouldResemble, stored)

				list, listErr := store.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)
				So(list[0].Package, ShouldEqual, "acme/foo")
			})

			Convey("Then an exact-key re-Put still overwrites idempotently", func() {
				updated := first
				updated.Version = "1.1.0"

				putErr := store.Put(updated)

				So(putErr, ShouldBeNil)

				got, ok, getErr := store.Get("acme/foo", "claude", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Version, ShouldEqual, "1.1.0")
			})
		})
	})
}

func TestReceiptCaseVariantPackageDirectoryCollision(t *testing.T) {
	Convey("Given a store on a filesystem of probed case sensitivity", t, func() {
		root := t.TempDir()
		dir := filepath.Join(root, "receipts")
		store := NewStore(dir)
		caseSensitive := caseSensitiveFS(t, filepath.Join(root, "case-probe"))
		t.Logf("filesystem case-sensitive=%v", caseSensitive)

		first := sampleReceipt()
		first.Package = "acme/foo"
		first.Host = "claude"
		first.Version = "1.0.0"

		second := sampleReceipt()
		second.Package = "ACME/foo"
		second.Host = "codex"
		second.Version = "2.0.0"

		So(store.Put(first), ShouldBeNil)

		firstCell := filepath.Join(dir, "acme", "foo", "claude-user.json")
		stored := readTestFile(t, firstCell)

		Convey("When a case-variant package with a different leaf is put", func() {
			err := store.Put(second)

			Convey("Then the probed filesystem property decides the invariant", func() {
				if caseSensitive {
					So(err, ShouldBeNil)

					got, ok, getErr := store.Get("acme/foo", "claude", ScopeUser)
					So(getErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(got.Version, ShouldEqual, "1.0.0")

					got, ok, getErr = store.Get("ACME/foo", "codex", ScopeUser)
					So(getErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(got.Version, ShouldEqual, "2.0.0")

					So(readTestFile(t, firstCell), ShouldResemble, stored)

					list, listErr := store.List()
					So(listErr, ShouldBeNil)
					So(list, ShouldHaveLength, 2)

					return
				}

				collision, collisionOK := errors.AsType[*KeyCollisionError](err)
				So(collisionOK, ShouldBeTrue)
				So(collision.Package, ShouldEqual, "ACME/foo")
				So(collision.Host, ShouldEqual, "codex")
				So(collision.Scope, ShouldEqual, ScopeUser)
				So(collision.Existing.Package, ShouldEqual, "acme/foo")
				So(collision.Existing.Host, ShouldEqual, "claude")
				So(collision.Existing.Scope, ShouldEqual, ScopeUser)

				// The refused write may not leave a differently-keyed leaf
				// behind: List must stay intact instead of going corrupt.
				list, listErr := store.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)
				So(list[0].Package, ShouldEqual, "acme/foo")

				got, ok, getErr := store.Get("acme/foo", "claude", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Version, ShouldEqual, "1.0.0")
				So(readTestFile(t, firstCell), ShouldResemble, stored)

				assertMissing(t, filepath.Join(dir, "ACME", "foo", "codex-user.json"))
			})

			Convey("Then an exact-key re-Put still overwrites idempotently", func() {
				updated := first
				updated.Version = "1.1.0"

				putErr := store.Put(updated)

				So(putErr, ShouldBeNil)

				got, ok, getErr := store.Get("acme/foo", "claude", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Version, ShouldEqual, "1.1.0")
			})
		})
	})
}

func TestReceiptCaseVariantHostCollision(t *testing.T) {
	Convey("Given a store on a filesystem of probed case sensitivity", t, func() {
		root := t.TempDir()
		dir := filepath.Join(root, "receipts")
		store := NewStore(dir)
		caseSensitive := caseSensitiveFS(t, filepath.Join(root, "case-probe"))

		first := sampleReceipt()
		first.Version = "1.0.0"

		second := sampleReceipt()
		second.Host = "Claude"
		second.Version = "2.0.0"

		So(store.Put(first), ShouldBeNil)

		cell := filepath.Join(dir, "owner", "name", "claude-user.json")
		stored := readTestFile(t, cell)

		Convey("When a case-variant host is put", func() {
			err := store.Put(second)

			Convey("Then the probed filesystem property decides the invariant", func() {
				if caseSensitive {
					So(err, ShouldBeNil)

					got, ok, getErr := store.Get("owner/name", "claude", ScopeUser)
					So(getErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(got.Version, ShouldEqual, "1.0.0")

					got, ok, getErr = store.Get("owner/name", "Claude", ScopeUser)
					So(getErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(got.Version, ShouldEqual, "2.0.0")

					So(readTestFile(t, cell), ShouldResemble, stored)

					list, listErr := store.List()
					So(listErr, ShouldBeNil)
					So(list, ShouldHaveLength, 2)

					return
				}

				collision, collisionOK := errors.AsType[*KeyCollisionError](err)
				So(collisionOK, ShouldBeTrue)
				So(collision.Host, ShouldEqual, "Claude")
				So(collision.Existing.Host, ShouldEqual, "claude")

				got, ok, getErr := store.Get("owner/name", "claude", ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(got.Version, ShouldEqual, "1.0.0")
				So(readTestFile(t, cell), ShouldResemble, stored)

				list, listErr := store.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)
			})
		})

		Convey("When the scope is a case variant", func() {
			upper := sampleReceipt()
			upper.Scope = "USER"

			putErr := store.Put(upper)
			deleteErr := store.Delete("owner/name", "claude", "USER")

			Convey("Then it is an invalid key, never a collision", func() {
				_, putOK := errors.AsType[*InvalidReceiptError](putErr)
				So(putOK, ShouldBeTrue)

				_, deleteOK := errors.AsType[*InvalidKeyError](deleteErr)
				So(deleteOK, ShouldBeTrue)
			})
		})
	})
}

func TestReceiptPutRefusesUndecodableCell(t *testing.T) {
	Convey("Given a store whose target cell already holds an unreadable receipt", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)
		cell := filepath.Join(dir, "owner", "name", "claude-user.json")
		So(os.MkdirAll(filepath.Dir(cell), 0o700), ShouldBeNil)

		Convey("When the cell holds broken JSON", func() {
			So(os.WriteFile(cell, []byte("{not json"), 0o600), ShouldBeNil)

			err := store.Put(sampleReceipt())

			Convey("Then Put refuses and keeps the stored bytes", func() {
				_, ok := errors.AsType[*CorruptReceiptError](err)
				So(ok, ShouldBeTrue)

				So(readTestFile(t, cell), ShouldResemble, []byte("{not json"))
			})
		})

		Convey("When the cell holds a newer schema", func() {
			r := sampleReceipt()
			r.Schema = Schema + 1

			data, marshalErr := json.Marshal(r)
			So(marshalErr, ShouldBeNil)
			So(os.WriteFile(cell, data, 0o600), ShouldBeNil)

			err := store.Put(sampleReceipt())

			Convey("Then Put refuses with SchemaNewerError", func() {
				_, ok := errors.AsType[*SchemaNewerError](err)
				So(ok, ShouldBeTrue)

				So(readTestFile(t, cell), ShouldResemble, data)
			})
		})
	})
}

func TestReceiptPutRefusesUndecodableSibling(t *testing.T) {
	Convey("Given a store whose sibling cell is undecodable", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)
		sibling := filepath.Join(dir, "owner", "name", "codex-user.json")
		So(os.MkdirAll(filepath.Dir(sibling), 0o700), ShouldBeNil)
		So(os.WriteFile(sibling, []byte("{not json"), 0o600), ShouldBeNil)

		Convey("When another leaf of the same package is put", func() {
			err := store.Put(sampleReceipt())

			Convey("Then the sibling blocks the write and stays untouched", func() {
				_, ok := errors.AsType[*CorruptReceiptError](err)
				So(ok, ShouldBeTrue)

				So(readTestFile(t, sibling), ShouldResemble, []byte("{not json"))
				assertMissing(t, filepath.Join(dir, "owner", "name", "claude-user.json"))
			})
		})
	})
}

func TestReceiptExtrasCaseInsensitiveMasking(t *testing.T) {
	Convey("Given a stored receipt using a differently-cased known key", t, func() {
		dir := filepath.Join(t.TempDir(), "receipts")
		store := NewStore(dir)

		cell := filepath.Join(dir, "owner", "name", "claude-user.json")
		So(os.MkdirAll(filepath.Dir(cell), 0o700), ShouldBeNil)

		stored := `{"Schema":1,"Package":"owner/name","Host":"claude","Scope":"user","Strategy":"loose"}`
		So(os.WriteFile(cell, []byte(stored), 0o600), ShouldBeNil)

		Convey("When the receipt is read and rewritten", func() {
			got, ok, getErr := store.Get("owner/name", "claude", ScopeUser)
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(got.Package, ShouldEqual, "owner/name")

			putErr := store.Put(got)

			Convey("Then the known key is not duplicated case-insensitively", func() {
				So(putErr, ShouldBeNil)

				var doc map[string]json.RawMessage
				So(json.Unmarshal(readTestFile(t, cell), &doc), ShouldBeNil)

				seen := 0

				for key := range doc {
					if strings.EqualFold(key, "package") {
						seen++
					}
				}

				So(seen, ShouldEqual, 1)
			})
		})
	})
}
