package source

import (
	"context"
	"errors"
	"os"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// fetchLocal uses a local directory in place: no copy, no cache entry.
func (f *Fetcher) fetchLocal(ctx context.Context, ref Ref) (*Fetched, error) {
	if err := ctx.Err(); err != nil {
		return nil, fetchError(ref, StepContext, err)
	}

	if ref.Path == "" {
		return nil, &RefError{Input: ref.Raw, Reason: "local refs need a path"}
	}

	info, err := os.Stat(ref.Path)
	if err != nil {
		return nil, fetchError(ref, StepLocal, err)
	}

	if !info.IsDir() {
		return nil, fetchError(ref, StepLocal, errors.New("local path is not a directory"))
	}

	sum, err := digest.TreeWithSkip(ref.Path, skipGitDir)
	if err != nil {
		return nil, fetchError(ref, StepDigest, err)
	}

	pkg, warnings := parseManifest(ref.Path, ref)

	return &Fetched{
		Ref:        ref,
		Root:       ref.Path,
		TreeDigest: sum,
		Package:    pkg,
		Warnings:   warnings,
		Cleanup:    func() error { return nil },
	}, nil
}
