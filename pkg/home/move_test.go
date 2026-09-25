package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// buildHomeFixture writes a small home tree with nested files, a symlink and
// distinct modes.
func buildHomeFixture(t *testing.T) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "home")
	mkFile(t, filepath.Join(root, "verger.toml"), "schema = 1\n", 0o600)
	mkFile(t, filepath.Join(root, "state", "journal.jsonl"), "{}\n", 0o600)
	mkFile(t, filepath.Join(root, "nested", "dir", "file.txt"), "content", 0o640)

	if err := os.Chmod(filepath.Join(root, "nested", "dir"), 0o750); err != nil { //nolint:gosec // G302: the fixture deliberately uses a group-readable dir mode
		t.Fatalf("chmod nested dir: %v", err)
	}

	if err := os.Symlink("verger.toml", filepath.Join(root, "spec-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	return root
}

func TestMoveHomeSameFS(t *testing.T) {
	Convey("Given a home tree on one filesystem", t, func() {
		from := buildHomeFixture(t)
		to := filepath.Join(t.TempDir(), "moved")

		before, err := digest.Tree(from)
		So(err, ShouldBeNil)

		Convey("When the home is moved", func() {
			moveErr := MoveHome(context.Background(), from, to)

			Convey("Then the tree, modes and symlinks survive and no source is left", func() {
				So(moveErr, ShouldBeNil)
				assertMissing(t, from)

				after, digestErr := digest.Tree(to)
				So(digestErr, ShouldBeNil)
				So(after, ShouldEqual, before)

				assertMode(t, filepath.Join(to, "verger.toml"), 0o600)
				assertMode(t, filepath.Join(to, "nested", "dir"), 0o750)
				assertMode(t, filepath.Join(to, "nested", "dir", "file.txt"), 0o640)

				target, linkErr := os.Readlink(filepath.Join(to, "spec-link"))
				So(linkErr, ShouldBeNil)
				So(target, ShouldEqual, "verger.toml")
			})
		})

		Convey("When the home is moved twice", func() {
			So(MoveHome(context.Background(), from, to), ShouldBeNil)
			secondErr := MoveHome(context.Background(), from, to)

			Convey("Then the second move reports a missing source", func() {
				So(secondErr, ShouldBeError)

				_, ok := errors.AsType[*SourceMissingError](secondErr)
				So(ok, ShouldBeTrue)

				after, digestErr := digest.Tree(to)
				So(digestErr, ShouldBeNil)
				So(after, ShouldEqual, before)
			})
		})
	})
}

func TestMoveHomePreconditions(t *testing.T) {
	Convey("Given a home tree", t, func() {
		from := buildHomeFixture(t)

		Convey("When the destination exists", func() {
			to := filepath.Join(t.TempDir(), "occupied")
			mkFile(t, filepath.Join(to, "keep.txt"), "occupied", 0o600)

			err := MoveHome(context.Background(), from, to)

			Convey("Then it reports DestinationExistsError and touches nothing", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "exists")

				target, ok := errors.AsType[*DestinationExistsError](err)
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, to)

				So(readTestFile(t, filepath.Join(to, "keep.txt")), ShouldEqual, "occupied")

				_, statErr := os.Stat(from)
				So(statErr, ShouldBeNil)
			})
		})

		Convey("When the source is missing", func() {
			err := MoveHome(context.Background(), filepath.Join(t.TempDir(), "gone"), filepath.Join(t.TempDir(), "dst"))

			Convey("Then it reports SourceMissingError", func() {
				So(err, ShouldBeError)
				So(err.Error(), ShouldContainSubstring, "missing")

				_, ok := errors.AsType[*SourceMissingError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When source and destination are the same", func() {
			err := MoveHome(context.Background(), from, from)

			Convey("Then it reports InvalidMoveError", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
				So(target.Reason, ShouldNotBeEmpty)
			})
		})

		Convey("When the destination parent is missing", func() {
			to := filepath.Join(t.TempDir(), "missing", "dst")

			err := MoveHome(context.Background(), from, to)

			Convey("Then it reports InvalidMoveError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the source is a file", func() {
			file := filepath.Join(t.TempDir(), "not-a-home")
			mkFile(t, file, "x", 0o600)

			err := MoveHome(context.Background(), file, filepath.Join(t.TempDir(), "dst"))

			Convey("Then it reports InvalidMoveError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When an argument is empty", func() {
			err := MoveHome(context.Background(), "", filepath.Join(t.TempDir(), "dst"))

			Convey("Then it reports InvalidMoveError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestMoveHomeDelegates(t *testing.T) {
	Convey("Given a spy on the move seam", t, func() {
		restore := move
		defer func() { move = restore }()

		type call struct{ from, to string }

		var (
			calls   []call
			stubErr error
		)

		move = func(ctx context.Context, from, to string) error {
			calls = append(calls, call{from: from, to: to})

			if stubErr != nil {
				return stubErr
			}

			return restore(ctx, from, to)
		}

		from := buildHomeFixture(t)
		to := filepath.Join(t.TempDir(), "moved")

		Convey("When MoveHome runs", func() {
			err := MoveHome(context.Background(), from, to)

			Convey("Then the resolved paths are delegated to fsutil.MoveTree", func() {
				So(err, ShouldBeNil)
				So(calls, ShouldHaveLength, 1)
				So(calls[0].from, ShouldEqual, from)
				So(calls[0].to, ShouldEqual, to)
			})
		})

		Convey("When the seam fails", func() {
			stubErr = errors.New("seam exploded")
			from = buildHomeFixture(t)

			err := MoveHome(context.Background(), from, to)

			Convey("Then the error is propagated unchanged", func() {
				So(errors.Is(err, stubErr), ShouldBeTrue)

				_, statErr := os.Stat(from)
				So(statErr, ShouldBeNil)
			})
		})
	})
}

func TestMoveHomeCanceledContext(t *testing.T) {
	Convey("Given a home tree", t, func() {
		from := buildHomeFixture(t)
		to := filepath.Join(t.TempDir(), "moved")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		Convey("When the context is canceled", func() {
			err := MoveHome(ctx, from, to)

			Convey("Then nothing moves and context.Canceled is returned", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)

				_, statErr := os.Stat(from)
				So(statErr, ShouldBeNil)
				assertMissing(t, to)
			})
		})
	})
}

func TestMoveHomePartialFailure(t *testing.T) {
	Convey("Given an unwritable destination parent", t, func() {
		from := buildHomeFixture(t)

		parent := filepath.Join(t.TempDir(), "locked-parent")
		So(os.MkdirAll(parent, 0o700), ShouldBeNil)
		So(os.Chmod(parent, 0o500), ShouldBeNil) //nolint:gosec // G302: the test locks the parent to force a rename failure

		defer func() {
			_ = os.Chmod(parent, 0o700) //nolint:gosec // G302: restoring the test dir so TempDir cleanup works
		}()

		to := filepath.Join(parent, "moved")

		Convey("When the move fails", func() {
			err := MoveHome(context.Background(), from, to)

			Convey("Then the source is intact and no destination claims success", func() {
				So(err, ShouldBeError)
				assertMissing(t, to)

				_, statErr := os.Stat(filepath.Join(from, "verger.toml"))
				So(statErr, ShouldBeNil)
			})
		})
	})
}

func TestMoveHomeUnicode(t *testing.T) {
	Convey("Given a home with unicode and spaced names", t, func() {
		root := filepath.Join(t.TempDir(), "дом с пробелом")
		mkFile(t, filepath.Join(root, "файл.txt"), "юникод", 0o600)

		before, err := digest.Tree(root)
		So(err, ShouldBeNil)

		to := filepath.Join(t.TempDir(), "новый дом")

		Convey("When it is moved", func() {
			moveErr := MoveHome(context.Background(), root, to)

			Convey("Then the bytes survive", func() {
				So(moveErr, ShouldBeNil)

				after, digestErr := digest.Tree(to)
				So(digestErr, ShouldBeNil)
				So(after, ShouldEqual, before)
			})
		})
	})
}

func TestMoveHomeTildeWithoutHome(t *testing.T) {
	Convey("Given an unset process HOME", t, func() {
		t.Setenv("HOME", "")

		Convey("When a tilde source cannot expand", func() {
			err := MoveHome(context.Background(), "~/verger", filepath.Join(t.TempDir(), "dst"))

			Convey("Then it reports InvalidMoveError with a message", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, "invalid home move")
			})
		})
	})
}

func TestMoveHomeStoreUntouched(t *testing.T) {
	Convey("Given a decoy store next to the home", t, func() {
		storeDir := filepath.Join(t.TempDir(), "store")
		mkFile(t, filepath.Join(storeDir, "trash", "bucket", "payload"), "store", 0o600)

		from := buildHomeFixture(t)
		to := filepath.Join(t.TempDir(), "moved")

		Convey("When the home moves", func() {
			err := MoveHome(context.Background(), from, to)

			Convey("Then the store bytes are untouched", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, filepath.Join(storeDir, "trash", "bucket", "payload")), ShouldEqual, "store")
			})
		})
	})
}

func TestMoveHomeBadArgumentKeepsCorrectEndpoints(t *testing.T) {
	Convey("Given a home tree", t, func() {
		from := buildHomeFixture(t)
		to := filepath.Join(t.TempDir(), "moved")

		Convey("When the destination argument is unusable", func() {
			err := MoveHome(context.Background(), from, "")

			Convey("Then InvalidMoveError names from as from and the bad value as to", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
				So(target.From, ShouldEqual, from)
				So(target.To, ShouldEqual, "")
				So(target.Reason, ShouldContainSubstring, "destination")
			})
		})

		Convey("When the source argument is unusable", func() {
			err := MoveHome(context.Background(), "   ", to)

			Convey("Then InvalidMoveError names the bad value as from and to as to", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*InvalidMoveError](err)
				So(ok, ShouldBeTrue)
				So(target.From, ShouldEqual, "   ")
				So(target.To, ShouldEqual, to)
				So(target.Reason, ShouldContainSubstring, "source")
			})
		})
	})
}
