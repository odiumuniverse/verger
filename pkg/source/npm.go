package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// fetchNPM packs an npm package without running lifecycle scripts and extracts
// the resulting tarball.
func (f *Fetcher) fetchNPM(ctx context.Context, ref Ref, tmp string) (*entryMeta, error) {
	if ref.NPM == "" {
		return nil, &RefError{Input: ref.Raw, Reason: "npm refs need a spec"}
	}

	npm, err := f.tool("npm")
	if err != nil {
		return nil, err
	}

	packDir := filepath.Join(tmp, "pack")
	if err := fsutil.EnsureDir(packDir, 0o700); err != nil {
		return nil, fetchError(ref, StepCache, err)
	}

	out, err := npm.RunWith(ctx, f.runner,
		[]string{"pack", ref.NPM, "--ignore-scripts", "--pack-destination", packDir}, nil)
	if err != nil {
		return nil, f.toolError(ref, StepNPMPack, err)
	}

	name := lastLine(string(out))
	if name == "" {
		return nil, fetchError(ref, StepNPMPack, errors.New("npm pack printed no tarball name"))
	}

	tarball := filepath.Join(packDir, filepath.Base(name))
	if _, err := os.Stat(tarball); err != nil {
		return nil, fetchError(ref, StepNPMPack, err)
	}

	payload := filepath.Join(tmp, "payload")
	if err := fsutil.EnsureDir(payload, 0o700); err != nil {
		return nil, fetchError(ref, StepCache, err)
	}

	warnings, err := extractArchive(ctx, tarball, payload)
	if err != nil {
		return nil, fetchError(ref, StepExtract, err)
	}

	root, rootRel := stripSingleRoot(payload, "payload")

	tree, err := digest.TreeWithSkip(root, skipGitDir)
	if err != nil {
		return nil, fetchError(ref, StepDigest, err)
	}

	return &entryMeta{Kind: KindNPM, Root: rootRel, TreeDigest: tree, Warnings: warnings}, nil
}

// lastLine returns the last non-empty line of npm output.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")

	for _, line := range slices.Backward(lines) {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}

	return ""
}
