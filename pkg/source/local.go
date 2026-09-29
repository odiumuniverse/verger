package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// FetchLocalOffers resolves a local directory into the packages it offers.
//
// A directory is one of two things and the directory itself says which: if
// it is a package, that is what it offers. If it is not, it is a catalog —
// the shape a git source has when it points at a marketplace — and every
// immediate subdirectory that is a package is one of its entries.
//
// The distinction matters because a spec asks for one id and a local source
// is far more often a directory of packages than a single tree. Reading
// the catalog directory as one package produced a package whose name was the
// directory's, delivered nothing, and said nothing.
func (f *Fetcher) FetchLocalOffers(ctx context.Context, ref Ref) ([]*Fetched, error) {
	if ref.Kind != KindLocal {
		return nil, &RefError{Input: ref.Raw, Reason: "not a local ref"}
	}

	if isPackageDir(ref.Path) {
		one, err := f.fetchLocal(ctx, ref)
		if err != nil {
			return nil, err
		}

		return []*Fetched{one}, nil
	}

	entries, err := os.ReadDir(ref.Path)
	if err != nil {
		return nil, fetchError(ref, StepLocal, err)
	}

	var out []*Fetched

	for _, entry := range entries {
		// Only the top level is a catalog; descending would make every file a
		// package the first time a directory happened to look like one.
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		dir := filepath.Join(ref.Path, entry.Name())
		if !isPackageDir(dir) {
			// A subdirectory that is not a package is not an error: a
			// catalog may hold documentation next to its plugins.
			continue
		}

		sub, fetchErr := f.fetchLocal(ctx, Ref{
			Kind: KindLocal,
			Raw:  ref.Raw + "/" + entry.Name(),
			ID:   entry.Name(),
			Path: dir,
		})
		if fetchErr != nil {
			continue
		}

		out = append(out, sub)
	}

	return out, nil
}

// isPackageDir reports whether a directory carries a manifest of its own.
//
// It is asked directly rather than inferred from a fetch, because a fetch
// never fails on a manifest-less directory: it hands back a placeholder
// package carrying the parse error as a warning, which is the right answer
// for "fetch this and tell me what is wrong with it" and the wrong one for
// "what does this directory hold".
func isPackageDir(dir string) bool {
	_, err := manifest.ParseAny(dir)

	return err == nil
}

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
