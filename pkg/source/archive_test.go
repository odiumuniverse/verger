package source

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// tarEntry is one fixture entry of buildTarGz.
type tarEntry struct {
	name string
	body string
	link string // symlink target when sym is true
	dir  bool
	sym  bool
}

// buildTarGz renders entries as a gzipped tarball.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o600, Typeflag: tar.TypeReg}

		switch {
		case entry.dir:
			header.Typeflag = tar.TypeDir
			header.Mode = 0o700
		case entry.sym:
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.link
			header.Mode = 0o777
		}

		if !entry.dir {
			header.Size = int64(len(entry.body))
		}

		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("tar header %s: %v", entry.name, err)
		}

		if !entry.dir {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatalf("tar body %s: %v", entry.name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return buf.Bytes()
}

// buildZip renders entries as a zip archive.
func buildZip(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for _, entry := range entries {
		name := entry.name

		if entry.dir && name[len(name)-1] != '/' {
			name += "/"
		}

		writer, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip entry %s: %v", name, err)
		}

		if !entry.dir {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatalf("zip body %s: %v", name, err)
			}
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	return buf.Bytes()
}

// serveBytes starts an httptest server answering path with data.
func serveBytes(t *testing.T, path string, data []byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(data)
	}))

	t.Cleanup(srv.Close)

	return srv
}

// archiveRef builds a URL ref pinned to data.
func archiveRef(url string, data []byte) Ref {
	return Ref{Kind: KindURL, Raw: url, URL: url, SHA256: digest.Bytes(data)}
}

func TestFetchArchive(t *testing.T) {
	claudeFixture := []tarEntry{
		{name: "pkg-1.0/", dir: true},
		{name: "pkg-1.0/.claude-plugin/plugin.json", body: `{"name":"archived"}`},
		{name: "pkg-1.0/skills/a/SKILL.md", body: "# a\n"},
	}

	Convey("Given an httptest tar.gz archive", t, func() {
		data := buildTarGz(t, claudeFixture)
		srv := serveBytes(t, "/pkg.tar.gz", data)
		f, cache := newTestFetcher(t)

		Convey("When it is fetched with the correct pin", func() {
			got, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/pkg.tar.gz", data))

			Convey("Then it is verified, extracted, stripped and parsed", func() {
				So(err, ShouldBeNil)
				So(got.ArchiveSHA256, ShouldEqual, digest.Bytes(data))
				So(got.Package, ShouldNotBeNil)
				So(string(got.Package.Format), ShouldEqual, "claude")
				So(got.TreeDigest, ShouldNotBeEmpty)

				want, derr := digest.TreeWithSkip(got.Root, skipGitDir)
				So(derr, ShouldBeNil)
				So(got.TreeDigest, ShouldEqual, want)

				_, statErr := os.Stat(filepath.Join(got.Root, ".claude-plugin", "plugin.json"))
				So(statErr, ShouldBeNil)
				So(filepath.Base(got.Root), ShouldEqual, "pkg-1.0")
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})

			Convey("Then Cleanup is idempotent and keeps the cache entry", func() {
				So(got.Cleanup, ShouldNotBeNil)
				So(got.Cleanup(), ShouldBeNil)
				So(got.Cleanup(), ShouldBeNil)
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})
		})

		Convey("When the pin is wrong", func() {
			ref := archiveRef(srv.URL+"/pkg.tar.gz", data)
			ref.SHA256 = hexHash(9)

			_, err := f.Fetch(t.Context(), ref)

			Convey("Then it reports PinMismatchError before extraction and leaves no entry", func() {
				target, ok := errors.AsType[*PinMismatchError](err)
				So(ok, ShouldBeTrue)
				So(target.Want, ShouldEqual, hexHash(9))
				So(target.Got, ShouldEqual, digest.Bytes(data))
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})

		Convey("When the pin is wrong on a corrupt archive", func() {
			// The pin must be checked before extraction: a mutant that
			// extracts first would surface the gzip error instead.
			corrupt := []byte("not an archive")
			corruptSrv := serveBytes(t, "/corrupt.tar.gz", corrupt)
			ref := archiveRef(corruptSrv.URL+"/corrupt.tar.gz", corrupt)
			ref.SHA256 = hexHash(9)

			_, err := f.Fetch(t.Context(), ref)

			Convey("Then the pin mismatch wins over the extraction error", func() {
				_, ok := errors.AsType[*PinMismatchError](err)
				So(ok, ShouldBeTrue)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})

		Convey("When the pin is missing on a hand-built ref", func() {
			ref := archiveRef(srv.URL+"/pkg.tar.gz", data)
			ref.SHA256 = ""

			_, err := f.Fetch(t.Context(), ref)

			Convey("Then it reports RefError and leaves no entry", func() {
				_, ok := errors.AsType[*RefError](err)
				So(ok, ShouldBeTrue)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a zip archive", t, func() {
		data := buildZip(t, []tarEntry{
			{name: "pkg/", dir: true},
			{name: "pkg/plugin.json", body: `{"name":"zipped"}`},
		})
		srv := serveBytes(t, "/pkg.zip", data)
		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/pkg.zip", data))

			Convey("Then zip is extracted and the single root is stripped", func() {
				So(err, ShouldBeNil)
				So(filepath.Base(got.Root), ShouldEqual, "pkg")

				_, statErr := os.Stat(filepath.Join(got.Root, "plugin.json"))
				So(statErr, ShouldBeNil)
			})
		})
	})

	Convey("Given a corrupt gzip body", t, func() {
		data := []byte("this is not a gzip stream")
		srv := serveBytes(t, "/bad.tar.gz", data)
		f, cache := newTestFetcher(t)

		Convey("When it is fetched with a matching pin", func() {
			_, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/bad.tar.gz", data))

			Convey("Then extraction fails loudly and leaves no entry", func() {
				_, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})
}

func TestFetchArchiveHostile(t *testing.T) {
	Convey("Given an archive entry escaping the destination", t, func() {
		data := buildTarGz(t, []tarEntry{{name: "../evil.txt", body: "evil"}})
		srv := serveBytes(t, "/evil.tar.gz", data)
		f, cache := newTestFetcher(t)
		outside := filepath.Join(filepath.Dir(cache), "evil.txt")

		Convey("When it is fetched", func() {
			_, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/evil.tar.gz", data))

			Convey("Then traversal is refused and nothing escapes", func() {
				_, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)

				_, statErr := os.Lstat(outside)
				So(errors.Is(statErr, os.ErrNotExist), ShouldBeTrue)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})

	Convey("Given an archive entry with an absolute path", t, func() {
		data := buildTarGz(t, []tarEntry{{name: "/abs/evil.txt", body: "evil"}})
		srv := serveBytes(t, "/abs.tar.gz", data)
		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			_, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/abs.tar.gz", data))

			Convey("Then it is refused", func() {
				_, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given an archive with a symlink entry pointing outside", t, func() {
		data := buildTarGz(t, []tarEntry{
			{name: "pkg/", dir: true},
			{name: "pkg/plugin.json", body: `{"name":"sym"}`},
			{name: "pkg/escape", sym: true, link: "../../etc/passwd"},
		})
		srv := serveBytes(t, "/sym.tar.gz", data)
		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/sym.tar.gz", data))

			Convey("Then the symlink is skipped with a warning", func() {
				So(err, ShouldBeNil)

				_, statErr := os.Lstat(filepath.Join(got.Root, "escape"))
				So(errors.Is(statErr, os.ErrNotExist), ShouldBeTrue)
				So(got.Warnings, ShouldNotBeEmpty)
			})
		})
	})
}

func TestFetchArchiveTransports(t *testing.T) {
	Convey("Given a file:// archive", t, func() {
		data := buildTarGz(t, []tarEntry{{name: "pkg/plugin.json", body: `{"name":"file"}`}})
		path := filepath.Join(t.TempDir(), "pkg.tar.gz")
		So(os.WriteFile(path, data, 0o600), ShouldBeNil)

		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), archiveRef("file://"+path, data))

			Convey("Then the local archive is read and extracted", func() {
				So(err, ShouldBeNil)

				_, statErr := os.Stat(filepath.Join(got.Root, "plugin.json"))
				So(statErr, ShouldBeNil)
			})
		})
	})

	Convey("Given a server answering 404", t, func() {
		f, cache := newTestFetcher(t)

		Convey("When the archive is fetched", func() {
			_, err := f.Fetch(t.Context(), archiveRef("http://127.0.0.1:1/missing.tar.gz", []byte("x")))

			Convey("Then it reports a FetchError and leaves no entry", func() {
				_, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a slow server", t, func() {
		started := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		}))

		t.Cleanup(srv.Close)

		f, cache := newTestFetcher(t)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		Convey("When the context is canceled mid-download", func() {
			go func() {
				<-started
				cancel()
			}()

			_, err := f.Fetch(ctx, archiveRef(srv.URL+"/slow.tar.gz", []byte("x")))

			Convey("Then cancellation surfaces through the error chain", func() {
				So(err, ShouldBeError)
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})
}

func TestFetchArchiveCacheAndConcurrency(t *testing.T) {
	Convey("Given one archive ref fetched twice", t, func() {
		data := buildTarGz(t, []tarEntry{{name: "pkg/plugin.json", body: `{"name":"cache"}`}})
		srv := serveBytes(t, "/pkg.tar.gz", data)
		f, cache := newTestFetcher(t)
		ref := archiveRef(srv.URL+"/pkg.tar.gz", data)

		Convey("When it is fetched twice", func() {
			first, err := f.Fetch(t.Context(), ref)
			So(err, ShouldBeNil)

			second, err := f.Fetch(t.Context(), ref)

			Convey("Then the cache entry is reused", func() {
				So(err, ShouldBeNil)
				So(second.Root, ShouldEqual, first.Root)
				So(second.TreeDigest, ShouldEqual, first.TreeDigest)
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})
		})

		Convey("When two goroutines fetch it concurrently", func() {
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				roots []string
				errs  []error
			)

			for range 2 {
				wg.Go(func() {
					got, err := f.Fetch(t.Context(), ref)

					mu.Lock()
					defer mu.Unlock()

					if err != nil {
						errs = append(errs, err)

						return
					}

					roots = append(roots, got.Root)
				})
			}

			wg.Wait()

			Convey("Then exactly one entry exists and both agree", func() {
				So(errs, ShouldBeEmpty)
				So(roots, ShouldHaveLength, 2)
				So(roots[0], ShouldEqual, roots[1])
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})
		})
	})
}

func TestFetchArchiveUnicodeAndMultiRoot(t *testing.T) {
	Convey("Given an archive with unicode paths", t, func() {
		data := buildTarGz(t, []tarEntry{
			{name: "pkg/", dir: true},
			{name: "pkg/файл с пробелом.txt", body: "snow ☃"},
		})
		srv := serveBytes(t, "/uni.tar.gz", data)
		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/uni.tar.gz", data))

			Convey("Then unicode entries survive", func() {
				So(err, ShouldBeNil)

				body, readErr := os.ReadFile(filepath.Join(got.Root, "файл с пробелом.txt")) //nolint:gosec // G304: test fixture
				So(readErr, ShouldBeNil)
				So(string(body), ShouldEqual, "snow ☃")
			})
		})
	})

	Convey("Given an archive with two top-level entries", t, func() {
		data := buildTarGz(t, []tarEntry{
			{name: "one/a.txt", body: "a"},
			{name: "two/b.txt", body: "b"},
		})
		srv := serveBytes(t, "/multi.tar.gz", data)
		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), archiveRef(srv.URL+"/multi.tar.gz", data))

			Convey("Then no single root is stripped", func() {
				So(err, ShouldBeNil)

				_, statErr := os.Stat(filepath.Join(got.Root, "one", "a.txt"))
				So(statErr, ShouldBeNil)
			})
		})
	})
}
