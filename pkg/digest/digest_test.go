package digest_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
)

const (
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	abcSHA256   = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
)

func TestBytes(t *testing.T) {
	Convey("Given inputs with known SHA-256 sums", t, func() {
		Convey("When digests are computed", func() {
			Convey("Then empty, text and 1 KiB inputs match", func() {
				So(digest.Bytes(nil), ShouldEqual, digest.Hash(emptySHA256))
				So(digest.Bytes([]byte{}), ShouldEqual, digest.Hash(emptySHA256))
				So(digest.Bytes([]byte("abc")), ShouldEqual, digest.Hash(abcSHA256))
				So(digest.Bytes([]byte("abc")).String(), ShouldHaveLength, 64)

				data := make([]byte, 1024)
				_, randErr := rand.Read(data)
				So(randErr, ShouldBeNil)

				want := sha256.Sum256(data)
				So(digest.Bytes(data), ShouldEqual, digest.Hash(hex.EncodeToString(want[:])))
				So(digest.Bytes([]byte("abc")), ShouldNotEqual, digest.Bytes([]byte("abd")))
			})
		})
	})
}

func TestHash(t *testing.T) {
	Convey("Given a computed digest", t, func() {
		sum := digest.Bytes([]byte("abc"))

		Convey("When it is validated, parsed and marshaled", func() {
			parsed, err := digest.Parse(string(sum))

			Convey("Then it round-trips and is valid", func() {
				So(err, ShouldBeNil)
				So(parsed, ShouldEqual, sum)
				So(parsed.Valid(), ShouldBeTrue)
				So(digest.Hash("").Valid(), ShouldBeFalse)
				So(string(sum), ShouldHaveLength, 64)

				text, marshalErr := sum.MarshalText()
				So(marshalErr, ShouldBeNil)
				So(string(text), ShouldEqual, string(sum))

				var back digest.Hash
				So(back.UnmarshalText(text), ShouldBeNil)
				So(back, ShouldEqual, sum)
			})
		})

		Convey("When malformed digests are parsed", func() {
			Convey("Then they are rejected with the offending value and the spec message", func() {
				for _, bad := range []string{
					"",
					"abc",
					strings.Repeat("a", 63),
					strings.Repeat("a", 65),
					strings.ToUpper(abcSHA256),
					strings.Repeat("z", 64),
					"sha256:" + abcSHA256,
				} {
					_, err := digest.Parse(bad)
					So(errors.Is(err, digest.ErrInvalidHash), ShouldBeTrue)
					So(err.Error(), ShouldContainSubstring, digest.ErrInvalidHash.Error())
				}

				_, err := digest.Parse(strings.ToUpper(abcSHA256))
				So(err.Error(), ShouldContainSubstring, strings.ToUpper(abcSHA256))

				var target digest.Hash
				So(target.UnmarshalText([]byte("nope")), ShouldBeError)
			})
		})

		Convey("When MarshalText sees a zero or malformed digest", func() {
			_, zeroErr := digest.Hash("").MarshalText()
			_, badErr := digest.Hash(strings.ToUpper(abcSHA256)).MarshalText()

			Convey("Then both fail with ErrInvalidHash (decision T0.2 Q3)", func() {
				So(errors.Is(zeroErr, digest.ErrInvalidHash), ShouldBeTrue)
				So(errors.Is(badErr, digest.ErrInvalidHash), ShouldBeTrue)
			})
		})
	})
}

func TestErrInvalidHashMessage(t *testing.T) {
	Convey("Given the ErrInvalidHash sentinel", t, func() {
		Convey("When its message is read", func() {
			Convey("Then it is the decided sentence-case form", func() {
				So(digest.ErrInvalidHash.Error(), ShouldEqual, "invalid hash.")
			})
		})
	})
}

func TestUnreadableErrorMessage(t *testing.T) {
	Convey("Given manually built UnreadableError values", t, func() {
		Convey("When their messages are rendered", func() {
			Convey("Then a nil cause renders readable in both branches", func() {
				So((&digest.UnreadableError{}).Error(), ShouldEqual, "unreadable: <nil>")
				So((&digest.UnreadableError{Path: "/tmp/x"}).Error(), ShouldEqual, "unreadable /tmp/x: <nil>")
			})

			Convey("Then a real cause renders as itself", func() {
				So((&digest.UnreadableError{Path: "p", Cause: errors.New("boom")}).Error(),
					ShouldEqual, "unreadable p: boom")
			})
		})
	})
}

func TestReader(t *testing.T) {
	Convey("Given readable and failing streams", t, func() {
		Convey("When the digest is computed", func() {
			empty, err := digest.Reader(strings.NewReader(""))
			So(err, ShouldBeNil)
			So(empty, ShouldEqual, digest.Hash(emptySHA256))

			text, err := digest.Reader(strings.NewReader("abc"))
			So(err, ShouldBeNil)
			So(text, ShouldEqual, digest.Bytes([]byte("abc")))

			sentinel := errors.New("stream failed")

			_, err = digest.Reader(iotest.ErrReader(sentinel))

			detail, typed := errors.AsType[*digest.UnreadableError](err)

			Convey("Then content digests match and stream errors are typed", func() {
				So(errors.Is(err, sentinel), ShouldBeTrue)
				So(typed, ShouldBeTrue)
				So(detail.Path, ShouldBeEmpty)
			})
		})
	})
}

func TestFile(t *testing.T) {
	Convey("Given an empty file and a 10 MiB file", t, func() {
		dir := t.TempDir()

		empty := filepath.Join(dir, "empty.bin")
		So(os.WriteFile(empty, nil, 0o600), ShouldBeNil)

		large := make([]byte, 10<<20)
		for i := range large {
			large[i] = byte(i*31 + 7)
		}

		big := filepath.Join(dir, "large.bin")
		So(os.WriteFile(big, large, 0o600), ShouldBeNil)

		Convey("When their digests are computed", func() {
			Convey("Then empty is the known vector and large matches Bytes", func() {
				emptySum, err := digest.File(empty)
				So(err, ShouldBeNil)
				So(emptySum, ShouldEqual, digest.Hash(emptySHA256))

				bigSum, err := digest.File(big)
				So(err, ShouldBeNil)
				So(bigSum, ShouldEqual, digest.Bytes(large))
			})
		})
	})

	Convey("Given a symlink to a file", t, func() {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.txt")
		So(os.WriteFile(target, []byte("abc"), 0o600), ShouldBeNil)

		link := filepath.Join(dir, "link.txt")
		So(os.Symlink(target, link), ShouldBeNil)

		Convey("When the link is digested", func() {
			Convey("Then the resolved file content is hashed", func() {
				sum, err := digest.File(link)
				So(err, ShouldBeNil)
				So(sum, ShouldEqual, digest.Hash(abcSHA256))
			})
		})
	})

	Convey("Given missing and directory paths", t, func() {
		dir := t.TempDir()

		Convey("When they are digested", func() {
			_, err := digest.File(filepath.Join(dir, "missing.bin"))
			So(errors.Is(err, fs.ErrNotExist), ShouldBeTrue)

			_, err = digest.File(dir)

			Convey("Then missing wraps fs.ErrNotExist and directories are typed errors", func() {
				detail, typed := errors.AsType[*digest.UnreadableError](err)
				So(typed, ShouldBeTrue)
				So(detail.Path, ShouldEqual, dir)
			})
		})
	})
}

func TestFileUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode 000")
	}

	Convey("Given a chmod 000 file", t, func() {
		unreadable := filepath.Join(t.TempDir(), "locked.bin")
		So(os.WriteFile(unreadable, []byte("x"), 0o600), ShouldBeNil)
		So(os.Chmod(unreadable, 0o000), ShouldBeNil)

		_, err := digest.File(unreadable)

		Convey("When it is digested", func() {
			Convey("Then it is a typed UnreadableError", func() {
				detail, typed := errors.AsType[*digest.UnreadableError](err)
				So(typed, ShouldBeTrue)
				So(detail.Path, ShouldEqual, unreadable)
			})
		})
	})
}
