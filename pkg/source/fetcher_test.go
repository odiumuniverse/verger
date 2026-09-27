package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/store"
)

// recordRunner records every call and delegates to run.
type recordRunner struct {
	mu    sync.Mutex
	calls []hostcli.Call
	run   func(ctx context.Context, call hostcli.Call) ([]byte, error)
}

// Run implements hostcli.Runner.
func (r *recordRunner) Run(ctx context.Context, bin hostcli.Binary, args []string, stdin []byte) ([]byte, error) {
	call := hostcli.Call{Binary: bin.Name, Args: slices.Clone(args), Stdin: bytes.Clone(stdin)}

	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()

	if r.run == nil {
		return nil, &hostcli.ExitError{Name: bin.Name, Code: 127, Stderr: "no scripted response"}
	}

	return r.run(ctx, call)
}

// Calls returns a copy of every recorded call.
func (r *recordRunner) Calls() []hostcli.Call {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.calls)
}

// writeAt writes a file under root, creating parents.
func writeAt(t *testing.T, root, rel, body string) {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newTestFetcher builds a fetcher with a temp source cache and a benign binary
// resolver, so no real tool is needed.
func newTestFetcher(t *testing.T, opts ...Option) (*Fetcher, string) {
	t.Helper()

	cache := filepath.Join(t.TempDir(), "cache", "source")

	f, err := NewFetcher(append([]Option{WithCacheDir(cache)}, opts...)...)
	if err != nil {
		t.Fatalf("new fetcher: %v", err)
	}

	f.resolve = func(name string) (hostcli.Binary, error) {
		return hostcli.Binary{Name: name, Path: name}, nil
	}

	return f, cache
}

// newRealFetcher builds a fetcher without the resolver seam (real git/npm).
func newRealFetcher(t *testing.T, opts ...Option) (*Fetcher, string) {
	t.Helper()

	cache := filepath.Join(t.TempDir(), "cache", "source")

	f, err := NewFetcher(append([]Option{WithCacheDir(cache)}, opts...)...)
	if err != nil {
		t.Fatalf("new fetcher: %v", err)
	}

	return f, cache
}

// assertToolMissing fetches ref with an unresolvable tool and asserts the
// typed error.
func assertToolMissing(t *testing.T, ref Ref, tool string) {
	t.Helper()

	f, _ := newTestFetcher(t)
	f.resolve = func(name string) (hostcli.Binary, error) {
		return hostcli.Binary{}, fmt.Errorf("%s: %w", name, hostcli.ErrNotFound)
	}

	_, err := f.Fetch(t.Context(), ref)

	target, ok := errors.AsType[*ToolMissingError](err)
	if !ok {
		t.Fatalf("want ToolMissingError for %s, got %v", tool, err)
	}

	if target.Tool != tool {
		t.Fatalf("tool = %q, want %q", target.Tool, tool)
	}
}

// cacheEntries lists completed cache entry directories.
func cacheEntries(t *testing.T, cache string) []string {
	t.Helper()

	entries, err := os.ReadDir(cache)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		t.Fatalf("read cache %s: %v", cache, err)
	}

	var out []string

	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), "tmp-") {
			out = append(out, entry.Name())
		}
	}

	return out
}

func TestNewFetcher(t *testing.T) {
	Convey("Given a store-backed fetcher", t, func() {
		st, err := store.Open(filepath.Join(t.TempDir(), "store"))
		So(err, ShouldBeNil)

		Convey("When the fetcher is built with the store", func() {
			f, err := NewFetcher(WithStore(st))

			Convey("Then the cache is the store's source cache", func() {
				So(err, ShouldBeNil)
				So(f.cacheDir, ShouldEqual, filepath.Join(st.CacheDir(), "source"))
			})
		})

		Convey("When both the store and an explicit cache dir are given", func() {
			explicit := filepath.Join(t.TempDir(), "explicit")
			f, err := NewFetcher(WithStore(st), WithCacheDir(explicit))

			Convey("Then the explicit dir wins", func() {
				So(err, ShouldBeNil)
				So(f.cacheDir, ShouldEqual, explicit)
			})
		})

		Convey("When no cache option is given", func() {
			f, err := NewFetcher()

			Convey("Then the default store cache is used without creating it", func() {
				So(err, ShouldBeNil)
				So(filepath.IsAbs(f.cacheDir), ShouldBeTrue)
				So(strings.HasSuffix(f.cacheDir, filepath.Join("cache", "source")), ShouldBeTrue)
			})
		})
	})
}

func TestCacheKeyDistinguishesPins(t *testing.T) {
	Convey("Given refs that differ only in a pin", t, func() {
		base := Ref{Kind: KindGit, URL: "https://example.com/acme/x.git", Rev: "v1", Subpath: "skills/a", ID: "acme/x"}

		Convey("When their cache keys are computed", func() {
			key := cacheKey(base)

			Convey("Then rev and subpath are part of the identity", func() {
				otherRev := base
				otherRev.Rev = "v2"

				otherSub := base
				otherSub.Subpath = "skills/b"

				So(cacheKey(otherRev), ShouldNotEqual, key)
				So(cacheKey(otherSub), ShouldNotEqual, key)
				So(cacheKey(base), ShouldEqual, key)
			})
		})

		Convey("When the same source is fetched at two revs", func() {
			runner := scriptedGitRunner(t, map[string]string{"plugin.json": `{"name":"x"}`})
			f, cache := newTestFetcher(t, WithRunner(runner))

			refA := Ref{Kind: KindGit, URL: "https://example.com/acme/x.git", Rev: "v1", ID: "acme/x"}
			refB := refA
			refB.Rev = "v2"

			_, firstErr := f.Fetch(t.Context(), refA)
			_, secondErr := f.Fetch(t.Context(), refB)

			Convey("Then two cache entries and two clones exist", func() {
				So(firstErr, ShouldBeNil)
				So(secondErr, ShouldBeNil)
				So(cacheEntries(t, cache), ShouldHaveLength, 2)

				clones := 0

				for _, call := range runner.Calls() {
					if call.Args[0] == "clone" {
						clones++
					}
				}

				So(clones, ShouldEqual, 2)
			})
		})
	})
}

func TestFetchLocal(t *testing.T) {
	Convey("Given a local package directory with a manifest", t, func() {
		dir := t.TempDir()
		writeAt(t, dir, ".claude-plugin/plugin.json", `{"name":"local-pkg"}`)
		writeAt(t, dir, "skills/a/SKILL.md", "# a\n")
		writeAt(t, dir, ".git/HEAD", "ref: refs/heads/main\n")

		f, cache := newTestFetcher(t)
		ref := Ref{Kind: KindLocal, Raw: "./local", ID: "acme/local", Path: dir}

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), ref)

			Convey("Then it is used in place, digested and parsed", func() {
				So(err, ShouldBeNil)
				So(got.Root, ShouldEqual, dir)
				So(got.Ref.ID, ShouldEqual, "acme/local")
				So(got.Commit, ShouldBeEmpty)
				So(got.ArchiveSHA256, ShouldBeEmpty)
				So(got.Package, ShouldNotBeNil)
				So(got.Package.Format, ShouldEqual, manifest.FormatClaude)

				want, derr := digest.TreeWithSkip(dir, skipGitDir)
				So(derr, ShouldBeNil)
				So(got.TreeDigest, ShouldEqual, want)

				So(cacheEntries(t, cache), ShouldBeEmpty)
			})

			Convey("Then Cleanup is a safe no-op", func() {
				So(got.Cleanup, ShouldNotBeNil)
				So(got.Cleanup(), ShouldBeNil)
				So(got.Cleanup(), ShouldBeNil)

				_, statErr := os.Stat(filepath.Join(dir, "skills", "a", "SKILL.md"))
				So(statErr, ShouldBeNil)
			})
		})

		Convey("When the directory is missing", func() {
			ref.Path = filepath.Join(dir, "missing")

			_, err := f.Fetch(t.Context(), ref)

			Convey("Then it reports a FetchError", func() {
				_, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the directory contains a symlink", func() {
			link := filepath.Join(dir, "link-to-skills")
			So(os.Symlink(filepath.Join(dir, "skills"), link), ShouldBeNil)

			got, err := f.Fetch(t.Context(), ref)

			Convey("Then the digest skips it and the link stays in place", func() {
				So(err, ShouldBeNil)

				want, derr := digest.TreeWithSkip(dir, skipGitDir)
				So(derr, ShouldBeNil)
				So(got.TreeDigest, ShouldEqual, want)

				_, statErr := os.Lstat(link)
				So(statErr, ShouldBeNil)
			})
		})

		Convey("When the path is a file", func() {
			file := filepath.Join(t.TempDir(), "file.txt")
			So(os.WriteFile(file, []byte("x"), 0o600), ShouldBeNil)
			ref.Path = file

			_, err := f.Fetch(t.Context(), ref)

			Convey("Then it reports a FetchError", func() {
				_, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestFetchManifestHandoff(t *testing.T) {
	Convey("Given a payload without any manifest", t, func() {
		dir := t.TempDir()
		writeAt(t, dir, "skills/a/SKILL.md", "# a\n")

		f, _ := newTestFetcher(t)

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), Ref{Kind: KindLocal, ID: "acme/loose", Path: dir})

			Convey("Then it is valid with warnings and the ref id", func() {
				So(err, ShouldBeNil)
				So(got.Package, ShouldNotBeNil)
				So(got.Package.ID, ShouldEqual, "acme/loose")
				So(got.Package.Root, ShouldEqual, dir)
				So(got.Warnings, ShouldNotBeEmpty)
			})
		})
	})
}

func TestFetchNotSupported(t *testing.T) {
	Convey("Given mcp and agent refs", t, func() {
		f, _ := newTestFetcher(t)

		refs := []Ref{
			{Kind: KindMCP, MCP: "io.github.x/y"},
			{Kind: KindAgent, Agent: "claude", AgentRef: "caveman@caveman"},
		}

		for _, ref := range refs {
			Convey("When "+string(ref.Kind)+" is fetched", func() {
				_, err := f.Fetch(t.Context(), ref)

				Convey("Then it reports NotSupportedError", func() {
					target, ok := errors.AsType[*NotSupportedError](err)
					So(ok, ShouldBeTrue)
					So(target.Kind, ShouldEqual, ref.Kind)
				})
			})
		}
	})
}

func TestFetchErrorRedactsCredentials(t *testing.T) {
	Convey("Given a clone failure that quotes a credential url and a signed query", t, func() {
		runner := &recordRunner{run: func(_ context.Context, call hostcli.Call) ([]byte, error) {
			return nil, &hostcli.ExitError{
				Name:   call.Binary,
				Code:   128,
				Stderr: "fatal: unable to access https://user:secret@example.com/acme/x.git?X-Amz-Signature=SIGVALUE",
			}
		}}
		f, _ := newTestFetcher(t, WithRunner(runner))

		ref, err := Parse("git+https://user:secret@example.com/acme/x.git")
		So(err, ShouldBeNil)

		Convey("When the fetch fails", func() {
			_, fetchErr := f.Fetch(t.Context(), ref)

			Convey("Then the rendered error carries neither userinfo nor the signature", func() {
				_, ok := errors.AsType[*FetchError](fetchErr)
				So(ok, ShouldBeTrue)

				rendered := fetchErr.Error()
				So(rendered, ShouldNotContainSubstring, "secret")
				So(rendered, ShouldNotContainSubstring, "user:")
				So(rendered, ShouldNotContainSubstring, "SIGVALUE")
				So(rendered, ShouldContainSubstring, "example.com")
			})
		})
	})
}

func TestFetchCanceledContext(t *testing.T) {
	Convey("Given a canceled context", t, func() {
		dir := t.TempDir()
		writeAt(t, dir, "README.md", "x")

		f, _ := newTestFetcher(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		Convey("When a local fetch starts", func() {
			_, err := f.Fetch(ctx, Ref{Kind: KindLocal, Path: dir})

			Convey("Then cancellation surfaces through the error chain", func() {
				So(err, ShouldBeError)
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})
	})
}
