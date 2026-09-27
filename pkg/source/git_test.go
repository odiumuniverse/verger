package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/hostcli"
)

// scriptedCommit is the canned rev-parse answer of the scripted git runner.
const scriptedCommit = "0123456789abcdef0123456789abcdef01234567"

// scriptedGitRunner materializes tree on clone and answers checkout/rev-parse.
func scriptedGitRunner(t *testing.T, tree map[string]string) *recordRunner {
	t.Helper()

	return &recordRunner{run: func(_ context.Context, call hostcli.Call) ([]byte, error) {
		switch {
		case len(call.Args) >= 3 && call.Args[0] == "clone":
			for rel, body := range tree {
				path := filepath.Join(call.Args[2], filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					return nil, err
				}

				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					return nil, err
				}
			}

			return nil, nil
		case len(call.Args) >= 3 && call.Args[0] == "-C" && call.Args[2] == "checkout":
			return nil, nil
		case len(call.Args) >= 3 && call.Args[0] == "-C" && call.Args[2] == "rev-parse":
			return []byte(scriptedCommit + "\n"), nil
		default:
			return nil, &hostcli.ExitError{Name: call.Binary, Code: 127, Stderr: "unscripted"}
		}
	}}
}

func TestFetchGitScripted(t *testing.T) {
	tree := map[string]string{
		".claude-plugin/plugin.json": `{"name":"gitpkg"}`,
		".git/HEAD":                  "ref: refs/heads/main\n",
		"skills/a/SKILL.md":          "# a\n",
	}

	Convey("Given a scripted git remote", t, func() {
		runner := scriptedGitRunner(t, tree)
		f, cache := newTestFetcher(t, WithRunner(runner))
		ref := Ref{Kind: KindGit, Raw: "git+https://example.com/acme/x.git", URL: "https://example.com/acme/x.git", Rev: "v1", ID: "acme/x"}

		Convey("When it is fetched", func() {
			got, err := f.Fetch(t.Context(), ref)

			Convey("Then the commit is pinned and .git removed", func() {
				So(err, ShouldBeNil)
				So(got.Commit, ShouldEqual, scriptedCommit)
				So(got.Package, ShouldNotBeNil)
				So(string(got.Package.Format), ShouldEqual, "claude")

				_, statErr := os.Lstat(filepath.Join(got.Root, ".git"))
				So(errors.Is(statErr, os.ErrNotExist), ShouldBeTrue)

				want, derr := digest.TreeWithSkip(got.Root, skipGitDir)
				So(derr, ShouldBeNil)
				So(got.TreeDigest, ShouldEqual, want)

				calls := runner.Calls()
				So(calls, ShouldHaveLength, 3)
				So(calls[0].Binary, ShouldEqual, "git")
				So(calls[0].Args[0], ShouldEqual, "clone")
				So(calls[0].Args[1], ShouldEqual, "https://example.com/acme/x.git")
				So(calls[1].Args[2], ShouldEqual, "checkout")
				So(calls[1].Args[3], ShouldEqual, "v1")
				So(calls[2].Args[2], ShouldEqual, "rev-parse")
				So(calls[2].Args[3], ShouldEqual, "HEAD")
			})

			Convey("Then a second fetch reuses the cache without git", func() {
				firstCalls := len(runner.Calls())

				again, err := f.Fetch(t.Context(), ref)

				So(err, ShouldBeNil)
				So(again.Root, ShouldEqual, got.Root)
				So(again.Commit, ShouldEqual, scriptedCommit)
				So(len(runner.Calls()), ShouldEqual, firstCalls)
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})
		})

		Convey("When no ref is given", func() {
			ref.Rev = ""

			_, err := f.Fetch(t.Context(), ref)

			Convey("Then the default branch is used without checkout", func() {
				So(err, ShouldBeNil)

				calls := runner.Calls()
				So(calls, ShouldHaveLength, 2)
				So(calls[0].Args[0], ShouldEqual, "clone")
				So(calls[1].Args[2], ShouldEqual, "rev-parse")
			})
		})

		Convey("When a subpath is given", func() {
			ref.Subpath = "skills/a"

			got, err := f.Fetch(t.Context(), ref)

			Convey("Then the payload root is the subpath", func() {
				So(err, ShouldBeNil)
				So(filepath.Base(got.Root), ShouldEqual, "a")

				want, derr := digest.TreeWithSkip(got.Root, skipGitDir)
				So(derr, ShouldBeNil)
				So(got.TreeDigest, ShouldEqual, want)
			})
		})
	})
}

func TestFetchGitFailures(t *testing.T) {
	Convey("Given a failing git", t, func() {
		runner := &recordRunner{run: func(_ context.Context, call hostcli.Call) ([]byte, error) {
			return nil, &hostcli.ExitError{Name: call.Binary, Code: 128, Stderr: "fatal: repository not found"}
		}}
		f, cache := newTestFetcher(t, WithRunner(runner))

		Convey("When the clone fails", func() {
			_, err := f.Fetch(t.Context(), Ref{Kind: KindGit, URL: "https://example.com/acme/x.git", ID: "acme/x"})

			Convey("Then it reports FetchError at the clone step and leaves no entry", func() {
				target, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
				So(target.Step, ShouldEqual, StepClone)
				So(cacheEntries(t, cache), ShouldBeEmpty)
			})
		})
	})

	Convey("Given an authentication failure carrying credentials", t, func() {
		runner := &recordRunner{run: func(_ context.Context, call hostcli.Call) ([]byte, error) {
			return nil, &hostcli.ExitError{
				Name:   call.Binary,
				Code:   128,
				Stderr: "fatal: Authentication failed for 'https://user:secret@example.com/acme/x.git/'",
			}
		}}
		f, _ := newTestFetcher(t, WithRunner(runner))

		ref := Ref{ //nolint:gosec // G101: credential-shaped fixture for the redaction test
			Kind: KindGit,
			Raw:  "git+https://user:secret@example.com/acme/x.git",
			URL:  "https://user:secret@example.com/acme/x.git",
			ID:   "acme/x",
		}

		Convey("When the clone is rejected", func() {
			_, err := f.Fetch(t.Context(), ref)

			Convey("Then the error is an auth FetchError with no secret", func() {
				target, ok := errors.AsType[*FetchError](err)
				So(ok, ShouldBeTrue)
				So(target.Step, ShouldEqual, StepAuth)

				rendered := err.Error()
				So(rendered, ShouldNotContainSubstring, "secret")
				So(rendered, ShouldNotContainSubstring, "user:")
			})
		})
	})
}

func TestFetchGitMissingTool(t *testing.T) {
	Convey("Given git is not resolvable", t, func() {
		Convey("When a git ref is fetched", func() {
			Convey("Then it reports ToolMissingError for git", func() {
				assertToolMissing(t, Ref{Kind: KindGit, URL: "https://example.com/acme/x.git", ID: "acme/x"}, "git")
			})
		})
	})
}

// runGit runs one git command in dir.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture with fixed args
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

func TestFetchGitRealRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("git: skipped in -short mode")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	Convey("Given a real local git repository", t, func() {
		repo := t.TempDir()
		runGit(t, repo, "init", "-q", "-b", "main")
		writeAt(t, repo, ".claude-plugin/plugin.json", `{"name":"real"}`)
		writeAt(t, repo, "skills/a/SKILL.md", "# a\n")
		runGit(t, repo, "add", ".")
		runGit(t, repo, "-c", "user.name=verger", "-c", "user.email=verger@example.com", "commit", "-q", "-m", "init")
		runGit(t, repo, "tag", "v1")
		head := runGit(t, repo, "rev-parse", "HEAD")

		f, cache := newRealFetcher(t)
		ref := Ref{Kind: KindGit, Raw: repo, URL: repo, Rev: "v1", ID: "acme/real"}

		Convey("When it is fetched at the tag", func() {
			got, err := f.Fetch(t.Context(), ref)

			Convey("Then the commit is resolved and .git is gone", func() {
				So(err, ShouldBeNil)
				So(got.Commit, ShouldEqual, head)
				So(got.Package, ShouldNotBeNil)

				_, statErr := os.Lstat(filepath.Join(got.Root, ".git"))
				So(errors.Is(statErr, os.ErrNotExist), ShouldBeTrue)

				_, skillErr := os.Stat(filepath.Join(got.Root, "skills", "a", "SKILL.md"))
				So(skillErr, ShouldBeNil)
				So(cacheEntries(t, cache), ShouldHaveLength, 1)
			})

			Convey("Then a second fetch is an idempotent cache hit", func() {
				again, err := f.Fetch(t.Context(), ref)

				So(err, ShouldBeNil)
				So(again.Root, ShouldEqual, got.Root)
				So(again.Commit, ShouldEqual, head)
			})
		})

		Convey("When a commit sha is checked out", func() {
			ref.Rev = head

			got, err := f.Fetch(t.Context(), ref)

			Convey("Then the same commit is pinned", func() {
				So(err, ShouldBeNil)
				So(got.Commit, ShouldEqual, head)
			})
		})

		Convey("When a subpath is selected", func() {
			ref.Subpath = "skills/a"

			got, err := f.Fetch(t.Context(), ref)

			Convey("Then the payload root is the subpath", func() {
				So(err, ShouldBeNil)
				So(filepath.Base(got.Root), ShouldEqual, "a")
				So(got.TreeDigest, ShouldNotBeEmpty)
			})
		})
	})
}
