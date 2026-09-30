package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// A channel is resolved against a remote, so these tests build the remote: a
// real bare git repository with real tags, and an httptest server serving one
// registry document. Nothing here touches the network - a channel resolved
// against the real registry or github would be a test that passes whenever
// somebody else's dist-tags happen to have the name it wants.

// gitRemote builds a bare repository whose history carries the given tags and
// branches, and returns the path a clone would be made from.
func gitRemote(t *testing.T, tags, branches []string) string {
	t.Helper()

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	remote := filepath.Join(dir, "remote.git")

	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	git := func(args ...string) {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...) //nolint:gosec // G204: test fixture with fixed args
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=verger", "GIT_AUTHOR_EMAIL=verger@example.invalid",
			"GIT_COMMITTER_NAME=verger", "GIT_COMMITTER_EMAIL=verger@example.invalid")

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := exec.CommandContext(t.Context(), "git", "init", "-q", "-b", "main", work).Run(); err != nil { //nolint:gosec // G204: test fixture with fixed args
		t.Skipf("git init unavailable: %v", err)
	}

	if err := os.WriteFile(filepath.Join(work, "SKILL.md"), []byte("# one\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	git("add", ".")
	git("commit", "-q", "-m", "first")

	for _, tag := range tags {
		git("tag", tag)
	}

	for _, branch := range branches {
		git("branch", branch)
	}

	if err := exec.CommandContext(t.Context(), "git", "clone", "-q", "--bare", work, remote).Run(); err != nil { //nolint:gosec // G204: test fixture with fixed args
		t.Fatalf("clone --bare: %v", err)
	}

	return remote
}

func TestResolveChannelGitTags(t *testing.T) {
	Convey("Given a bare repo with release and prerelease tags", t, func() {
		remote := gitRemote(t,
			[]string{"v1.0.0", "v1.1.0", "v1.2.0-beta.1", "v1.2.0-beta.2", "v2.0.0-rc.1", "not-a-version"},
			[]string{"next"},
		)

		f, err := NewFetcher(WithCacheDir(t.TempDir()))
		So(err, ShouldBeNil)

		ref := Ref{Kind: KindGit, Raw: remote, ID: "o/r", URL: remote}

		Convey("When the stable channel is resolved", func() {
			got, err := f.ResolveChannel(context.Background(), ref, "stable")

			Convey("Then it is the highest release, not the highest tag", func() {
				// v2.0.0-rc.1 sorts above v1.1.0 as a string and above v1.2.0 by
				// its numbers, and is still older than v1.1.0 by precedence.
				// Taking the largest number without asking about the prerelease
				// is the bug this test exists for.
				So(err, ShouldBeNil)
				So(got.Rev, ShouldEqual, "v1.1.0")
				So(got.Version, ShouldEqual, "1.1.0")
				So(got.Via, ShouldEqual, "tag")
			})
		})

		Convey("When a prerelease channel is resolved", func() {
			got, err := f.ResolveChannel(context.Background(), ref, "beta")

			Convey("Then it is the highest tag carrying that prerelease", func() {
				So(err, ShouldBeNil)
				So(got.Rev, ShouldEqual, "v1.2.0-beta.2")
				So(got.Version, ShouldEqual, "1.2.0-beta.2")
			})
		})

		Convey("When a prerelease word that is not beta is resolved", func() {
			got, err := f.ResolveChannel(context.Background(), ref, "rc")

			Convey("Then it finds that prerelease, not beta's", func() {
				So(err, ShouldBeNil)
				So(got.Rev, ShouldEqual, "v2.0.0-rc.1")
			})
		})

		Convey("When a branch name is resolved", func() {
			got, err := f.ResolveChannel(context.Background(), ref, "next")

			Convey("Then it is the branch tip, not a tag", func() {
				So(err, ShouldBeNil)
				So(got.Via, ShouldEqual, "branch")
				So(got.Rev, ShouldEqual, "next")
				So(got.Version, ShouldNotBeEmpty)
			})
		})

		Convey("When the channel names nothing that exists", func() {
			_, err := f.ResolveChannel(context.Background(), ref, "nightly")

			Convey("Then it is refused and says what does exist", func() {
				// A silent fallback to the newest of something would be a
				// different product, chosen by the code instead of by the user.
				var chanErr *ChannelError

				So(err, ShouldHaveSameTypeAs, chanErr)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "available:")
				So(err.Error(), ShouldContainSubstring, "stable")
			})
		})
	})
}

func TestResolveChannelRejectsKindsWithoutOne(t *testing.T) {
	Convey("Given a local ref", t, func() {
		f, err := NewFetcher(WithCacheDir(t.TempDir()))
		So(err, ShouldBeNil)

		Convey("When a channel is asked of it", func() {
			_, err := f.ResolveChannel(context.Background(),
				Ref{Kind: KindLocal, Raw: "./here", Path: t.TempDir()}, "stable")

			Convey("Then it says the channel does not apply here", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, NotApplicable)
			})
		})
	})
}

func TestResolveChannelNPMDistTag(t *testing.T) {
	Convey("Given a registry that publishes two dist-tags", t, func() {
		var asked string

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = r.URL.Path

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"dist-tags":{"latest":"2.1.0","beta":"3.0.0-beta.4"}}`))
		}))

		defer server.Close()

		f, err := NewFetcher(WithCacheDir(t.TempDir()), WithNPMRegistry(server.URL))
		So(err, ShouldBeNil)

		ref := Ref{Kind: KindNPM, Raw: "@acme/thing", NPM: "@acme/thing"}

		Convey("When no channel is written", func() {
			got, err := f.ResolveChannel(context.Background(), ref, "")

			Convey("Then it means latest, which is what npm means by it", func() {
				So(err, ShouldBeNil)
				So(got.Rev, ShouldEqual, "@acme/thing@2.1.0")
				So(got.Channel, ShouldEqual, "latest")
				So(got.Via, ShouldEqual, "dist-tag")
			})

			Convey("And the registry was asked about that package", func() {
				So(asked, ShouldEqual, "/@acme/thing")
			})
		})

		Convey("When a published dist-tag is asked for", func() {
			got, err := f.ResolveChannel(context.Background(), ref, "beta")

			Convey("Then it resolves to that version", func() {
				So(err, ShouldBeNil)
				So(got.Version, ShouldEqual, "3.0.0-beta.4")
			})
		})

		Convey("When a dist-tag the registry does not publish is asked for", func() {
			_, err := f.ResolveChannel(context.Background(), ref, "canary")

			Convey("Then it is refused, listing the tags that exist", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "canary")
				So(err.Error(), ShouldContainSubstring, "latest")
				So(err.Error(), ShouldContainSubstring, "beta")
			})
		})
	})
}

func TestCompareSemverPutsPrereleaseBelowItsRelease(t *testing.T) {
	Convey("Given a release and its own prerelease", t, func() {
		_, release, ok := parseSemver("v1.2.0")
		So(ok, ShouldBeTrue)

		_, pre, ok := parseSemver("v1.2.0-rc.1")
		So(ok, ShouldBeTrue)

		Convey("Then the release is the newer of the two", func() {
			So(compareSemver(release, pre), ShouldBeGreaterThan, 0)
			So(compareSemver(pre, release), ShouldBeLessThan, 0)
		})

		Convey("And a higher number wins among peers", func() {
			_, older, ok := parseSemver("v1.1.9")
			So(ok, ShouldBeTrue)
			So(compareSemver(release, older), ShouldBeGreaterThan, 0)
		})

		Convey("And something that is not semver is not a candidate at all", func() {
			_, _, ok := parseSemver("nightly")
			So(ok, ShouldBeFalse)

			_, _, ok = parseSemver("v1.2")
			So(ok, ShouldBeFalse)
		})
	})
}

// Given a ref that carries a channel
// When it is fetched
// Then the checkout is the revision the channel names, not the remote's default.
func TestFetchResolvesTheChannelBeforeFetching(t *testing.T) {
	Convey("Given a bare repo with two release tags", t, func() {
		remote := gitRemote(t, []string{"v1.0.0", "v1.1.0"}, []string{"next"})

		f, err := NewFetcher(WithCacheDir(t.TempDir()))
		So(err, ShouldBeNil)

		Convey("When a ref carrying channel=stable is fetched", func() {
			ref := Ref{Kind: KindGit, Raw: remote, ID: "o/r", URL: remote, Channel: "stable"}

			fetched, err := f.Fetch(context.Background(), ref)
			So(err, ShouldBeNil)

			defer func() { _ = fetched.Cleanup() }()

			Convey("Then it checked out the highest release", func() {
				So(fetched.Ref.Rev, ShouldEqual, "v1.1.0")
			})

			Convey("Then the channel is spent and no longer describes the ref", func() {
				// Keeping it would make the cache key depend on a name that no
				// longer says anything about what was fetched.
				So(fetched.Ref.Channel, ShouldEqual, "")
			})
		})

		Convey("When a ref carrying a channel names one the repo does not have", func() {
			ref := Ref{Kind: KindGit, Raw: remote, ID: "o/r", URL: remote, Channel: "nightly"}

			_, err := f.Fetch(context.Background(), ref)

			Convey("Then the fetch is refused and the offer is listed", func() {
				So(err, ShouldNotBeNil)

				var chanErr *ChannelError
				So(errors.As(err, &chanErr), ShouldBeTrue)
				So(chanErr.Channel, ShouldEqual, "nightly")
				So(chanErr.Available, ShouldContain, "stable")
				So(chanErr.Available, ShouldContain, "next")
			})
		})

		Convey("When a local ref carries a channel", func() {
			ref := Ref{Kind: KindLocal, Raw: "local:./nowhere", Path: "/nowhere", Channel: "stable"}

			_, err := f.Fetch(context.Background(), ref)

			Convey("Then it is refused as inapplicable, not silently ignored", func() {
				// Silently dropping it would install a local directory while the
				// spec said "stable" - the file the user asked for is not the
				// thing they would get.
				So(err, ShouldNotBeNil)

				var chanErr *ChannelError
				So(errors.As(err, &chanErr), ShouldBeTrue)
				So(chanErr.Reason, ShouldEqual, NotApplicable)
			})
		})
	})
}
