package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// fetchGit clones a git source, pins its commit and removes .git.
func (f *Fetcher) fetchGit(ctx context.Context, ref Ref, tmp string) (*entryMeta, error) {
	if ref.URL == "" {
		return nil, &RefError{Input: ref.Raw, Reason: "git refs need a url"}
	}

	if ref.ID == "" {
		return nil, &RefError{Input: ref.Raw, Reason: "git refs need a derivable owner/repo id"}
	}

	git, err := f.tool("git")
	if err != nil {
		return nil, err
	}

	cloneDir := filepath.Join(tmp, "payload")

	if err := ctx.Err(); err != nil {
		return nil, fetchError(ref, StepContext, err)
	}

	if _, err := git.RunWith(ctx, f.runner, []string{"clone", ref.URL, cloneDir}, nil); err != nil {
		return nil, f.toolError(ref, StepClone, err)
	}

	if ref.Rev != "" {
		if _, err := git.RunWith(ctx, f.runner, []string{"-C", cloneDir, "checkout", ref.Rev}, nil); err != nil {
			return nil, f.toolError(ref, StepCheckout, err)
		}
	}

	out, err := git.RunWith(ctx, f.runner, []string{"-C", cloneDir, "rev-parse", "HEAD"}, nil)
	if err != nil {
		return nil, f.toolError(ref, StepRevParse, err)
	}

	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return nil, fetchError(ref, StepRevParse, errors.New("empty commit"))
	}

	if err := os.RemoveAll(filepath.Join(cloneDir, ".git")); err != nil {
		return nil, fetchError(ref, StepCleanup, err)
	}

	root, rootRel := payloadRoot(ref, cloneDir)

	if _, err := os.Stat(root); err != nil {
		return nil, fetchError(ref, StepSubpath, err)
	}

	sum, err := digest.TreeWithSkip(root, skipGitDir)
	if err != nil {
		return nil, fetchError(ref, StepDigest, err)
	}

	return &entryMeta{Kind: ref.Kind, Root: rootRel, Commit: commit, TreeDigest: sum}, nil
}

// payloadRoot applies the ref subpath to a payload directory.
func payloadRoot(ref Ref, dir string) (string, string) {
	if ref.Subpath == "" {
		return dir, "payload"
	}

	return filepath.Join(dir, filepath.FromSlash(ref.Subpath)), filepath.Join("payload", filepath.FromSlash(ref.Subpath))
}
