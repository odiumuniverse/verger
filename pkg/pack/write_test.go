package pack

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/store"
)

// openStore returns a store rooted in a temp dir.
func openStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return st
}

// synthTarget resolves the synth dir of the golden input.
func synthTarget(t *testing.T, st *store.Store) string {
	t.Helper()

	target, err := st.SynthPath(goldenID, goldenVersion)
	if err != nil {
		t.Fatalf("synth path: %v", err)
	}

	return target
}

// stagingLeftovers lists tmp/old siblings of target's parent tree.
func stagingLeftovers(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := entry.Name()
		if strings.Contains(name, ".tmp-") || strings.Contains(name, ".old-") {
			out = append(out, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return out
}

// assertModes checks dirs and regular files below root.
func assertModes(t *testing.T, root string) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}

		want := fs.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}

		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

func TestWriteLayoutModesIdempotent(t *testing.T) {
	Convey("Given a temp store and the golden package", t, func() {
		st := openStore(t)
		in := fixtureInput(t)

		res, err := Write(t.Context(), st, in)

		Convey("When it is written", func() {
			Convey("Then it lands in the synth store dir with the artifact bytes", func() {
				So(err, ShouldBeNil)

				target := synthTarget(t, st)
				So(res.Dir, ShouldEqual, target)

				for rel, data := range res.Artifact.Files {
					got, readErr := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel))) //nolint:gosec // G304: the test reads a path it just wrote
					So(readErr, ShouldBeNil)
					So(got, ShouldResemble, data)
				}
			})

			Convey("Then modes are 0700/0600 and no staging survives", func() {
				target := synthTarget(t, st)
				assertModes(t, target)
				So(stagingLeftovers(t, filepath.Dir(target)), ShouldBeEmpty)
			})

			Convey("Then a second write is an idempotent no-op", func() {
				target := synthTarget(t, st)
				probe := filepath.Join(target, "plugin.json")

				before, statErr := os.Stat(probe)
				So(statErr, ShouldBeNil)

				second, secondErr := Write(t.Context(), st, in)
				So(secondErr, ShouldBeNil)
				So(second.Dir, ShouldEqual, target)

				after, statErr := os.Stat(probe)
				So(statErr, ShouldBeNil)
				So(after.ModTime(), ShouldEqual, before.ModTime())
				So(stagingLeftovers(t, filepath.Dir(target)), ShouldBeEmpty)
			})
		})
	})
}

func TestWriteUpdateReplaces(t *testing.T) {
	Convey("Given a written synth dir", t, func() {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "skills", "solo", "SKILL.md"), "---\nname: solo\n---\n\nOne.\n")

		st := openStore(t)

		in := Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Root:       root,
			Components: []manifest.Component{treeComponent(t, root, manifest.KindSkill, "solo", "skills/solo")},
		}

		first, err := Write(t.Context(), st, in)
		So(err, ShouldBeNil)

		Convey("When the target gains a foreign file", func() {
			foreign := filepath.Join(first.Dir, "zz-foreign.txt")
			writeFile(t, foreign, "stray\n")

			third, thirdErr := Write(t.Context(), st, in)

			Convey("Then the target is replaced and the foreign file is gone", func() {
				So(thirdErr, ShouldBeNil)
				So(third.Dir, ShouldEqual, first.Dir)

				_, statErr := os.Lstat(foreign)
				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)
				So(stagingLeftovers(t, filepath.Dir(third.Dir)), ShouldBeEmpty)
			})
		})

		Convey("When the payload changes and the package is written again", func() {
			writeFile(t, filepath.Join(root, "skills", "solo", "SKILL.md"), "---\nname: solo\n---\n\nTwo.\n")

			second, secondErr := Write(t.Context(), st, in)

			Convey("Then the target is replaced and no staging or backup survives", func() {
				So(secondErr, ShouldBeNil)
				So(second.Dir, ShouldEqual, first.Dir)

				got, readErr := os.ReadFile(filepath.Join(second.Dir, "skills", "solo", "SKILL.md")) //nolint:gosec // G304: the test reads a path it just wrote
				So(readErr, ShouldBeNil)
				So(string(got), ShouldContainSubstring, "Two.")
				So(stagingLeftovers(t, filepath.Dir(second.Dir)), ShouldBeEmpty)
			})
		})
	})
}

func TestWriteCancelLeavesNothing(t *testing.T) {
	Convey("Given a canceled context", t, func() {
		st := openStore(t)
		in := fixtureInput(t)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		Convey("When the package is written", func() {
			_, err := Write(ctx, st, in)

			Convey("Then the write is refused before staging", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)

				target := synthTarget(t, st)
				_, statErr := os.Stat(target)
				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)
				So(stagingLeftovers(t, st.Root()), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a written package and a canceled update", t, func() {
		st := openStore(t)
		in := fixtureInput(t)

		_, err := Write(t.Context(), st, in)
		So(err, ShouldBeNil)

		target := synthTarget(t, st)
		probe := filepath.Join(target, "plugin.json")

		before, readErr := os.ReadFile(probe) //nolint:gosec // G304: the test reads its own synth path
		So(readErr, ShouldBeNil)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		Convey("When the changed package is written", func() {
			in.Description = "Changed."

			_, writeErr := Write(ctx, st, in)

			Convey("Then the previous target is untouched", func() {
				So(errors.Is(writeErr, context.Canceled), ShouldBeTrue)

				after, readErr := os.ReadFile(probe) //nolint:gosec // G304: the test reads its own synth path
				So(readErr, ShouldBeNil)
				So(after, ShouldResemble, before)
				So(stagingLeftovers(t, filepath.Dir(target)), ShouldBeEmpty)
			})
		})
	})
}

func TestWriteStagingFailureKeepsTarget(t *testing.T) {
	Convey("Given a written package and an unwritable synth parent", t, func() {
		if os.Geteuid() == 0 {
			t.Skip("running as root: read-only dirs are not enforced")
		}

		st := openStore(t)
		in := fixtureInput(t)

		_, err := Write(t.Context(), st, in)
		So(err, ShouldBeNil)

		target := synthTarget(t, st)
		parent := filepath.Dir(target)

		before, readErr := os.ReadFile(filepath.Join(target, "plugin.json")) //nolint:gosec // G304: the test reads its own synth path
		So(readErr, ShouldBeNil)

		if chmodErr := os.Chmod(parent, 0o500); chmodErr != nil { //nolint:gosec // G302: the test makes the parent read-only on purpose
			t.Fatalf("chmod parent: %v", chmodErr)
		}

		defer func() { _ = os.Chmod(parent, 0o700) }() //nolint:gosec // G302: restoring the temp dir permissions

		Convey("When a changed package is written", func() {
			in.Description = "Changed."

			_, writeErr := Write(t.Context(), st, in)

			Convey("Then a *PackWriteError reports the failure and the target is intact", func() {
				typed, ok := errors.AsType[*PackWriteError](writeErr)
				So(ok, ShouldBeTrue)
				So(typed.Dir, ShouldEqual, target)

				after, readErr := os.ReadFile(filepath.Join(target, "plugin.json")) //nolint:gosec // G304: the test reads its own synth path
				So(readErr, ShouldBeNil)
				So(after, ShouldResemble, before)
			})
		})
	})
}

func TestWriteRejectsHostileInput(t *testing.T) {
	Convey("Given a package id escaping the store", t, func() {
		st := openStore(t)
		in := fixtureInput(t)
		in.ID = "../escape"

		Convey("When it is written", func() {
			_, err := Write(t.Context(), st, in)

			Convey("Then the store validation error passes through and nothing is created", func() {
				_, ok := errors.AsType[*store.InvalidIDError](err)
				So(ok, ShouldBeTrue)

				entries, readErr := os.ReadDir(st.Root())
				So(readErr, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a version escaping the store", t, func() {
		st := openStore(t)
		in := fixtureInput(t)
		in.Version = "../v"

		Convey("When it is written", func() {
			_, err := Write(t.Context(), st, in)

			Convey("Then it is refused by the store", func() {
				_, ok := errors.AsType[*store.InvalidIDError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a nil store", t, func() {
		Convey("When the package is written", func() {
			_, err := Write(t.Context(), nil, fixtureInput(t))

			Convey("Then a *PackWriteError is returned", func() {
				_, ok := errors.AsType[*PackWriteError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a component escaping the payload root", t, func() {
		st := openStore(t)
		in := fixtureInput(t)
		in.Components = []manifest.Component{
			{Kind: manifest.KindAgent, Name: "evil", Path: "../evil.md"},
		}

		Convey("When it is written", func() {
			_, err := Write(t.Context(), st, in)

			Convey("Then the *RenderError passes through and the store stays empty", func() {
				_ = renderError(t, err)

				entries, readErr := os.ReadDir(st.Root())
				So(readErr, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})
}

func TestWriteSymlinksAsIs(t *testing.T) {
	Convey("Given a skill tree with a symlink to an outside file", t, func() {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.txt")
		writeFile(t, outside, "outside-secret")

		writeFile(t, filepath.Join(root, "skills", "linked", "SKILL.md"), "---\nname: linked\n---\n\nBody.\n")

		if err := os.Symlink(outside, filepath.Join(root, "skills", "linked", "escape.txt")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		st := openStore(t)
		in := Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Root:       root,
			Components: []manifest.Component{treeComponent(t, root, manifest.KindSkill, "linked", "skills/linked")},
		}

		res, err := Write(t.Context(), st, in)

		Convey("When it is written", func() {
			Convey("Then the link is recreated, never followed", func() {
				So(err, ShouldBeNil)

				link := filepath.Join(res.Dir, "skills", "linked", "escape.txt")

				info, statErr := os.Lstat(link)
				So(statErr, ShouldBeNil)
				So(info.Mode()&fs.ModeSymlink != 0, ShouldBeTrue)

				target, readErr := os.Readlink(link)
				So(readErr, ShouldBeNil)
				So(target, ShouldEqual, outside)

				content, readErr := os.ReadFile(link) //nolint:gosec // G304: the test follows the link it just created
				So(readErr, ShouldBeNil)
				So(string(content), ShouldEqual, "outside-secret")
			})

			Convey("Then the outside file is untouched", func() {
				content, readErr := os.ReadFile(outside) //nolint:gosec // G304: the test reads its own temp file
				So(readErr, ShouldBeNil)
				So(string(content), ShouldEqual, "outside-secret")
			})
		})
	})
}

func TestStageArtifactRefusesEscapes(t *testing.T) {
	Convey("Given a hostile artifact path", t, func() {
		staging := t.TempDir()
		art := Artifact{
			Files:    map[string][]byte{"../escape.txt": []byte("x")},
			symlinks: map[string]string{},
		}

		Convey("When it is staged", func() {
			err := stageArtifact(t.Context(), staging, art)

			Convey("Then staging refuses and nothing lands outside", func() {
				So(err, ShouldBeError)

				_, statErr := os.Stat(filepath.Join(filepath.Dir(staging), "escape.txt"))
				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)
			})
		})
	})

	Convey("Given a hostile symlink path", t, func() {
		staging := t.TempDir()
		art := Artifact{
			Files:    map[string][]byte{},
			symlinks: map[string]string{"../escape": "target"},
		}

		Convey("When it is staged", func() {
			err := stageArtifact(t.Context(), staging, art)

			Convey("Then staging refuses as well", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestWriteTouchesOnlySynth(t *testing.T) {
	Convey("Given an initialized store", t, func() {
		st := openStore(t)

		if err := st.Ensure(); err != nil {
			t.Fatalf("store ensure: %v", err)
		}

		before := snapshotTree(t, st.Root())

		Convey("When a package is written", func() {
			_, err := Write(t.Context(), st, fixtureInput(t))

			Convey("Then only the synth subtree changed", func() {
				So(err, ShouldBeNil)

				after := snapshotTree(t, st.Root())

				for path, state := range after {
					if strings.HasPrefix(path, "synth") {
						continue
					}

					So(state, ShouldEqual, before[path])
				}
			})

			Convey("Then the written tree is intact after a moment", func() {
				So(err, ShouldBeNil)
				time.Sleep(10 * time.Millisecond)

				target := synthTarget(t, st)
				data, readErr := os.ReadFile(filepath.Join(target, "plugin.json")) //nolint:gosec // G304: the test reads its own synth path
				So(readErr, ShouldBeNil)
				So(manifestName(t, data), ShouldEqual, goldenName)
			})
		})
	})
}
