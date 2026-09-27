package source

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// fetchURL downloads an archive, verifies the pin before extraction and
// unpacks it into the cache entry.
func (f *Fetcher) fetchURL(ctx context.Context, ref Ref, tmp string) (*entryMeta, error) {
	if ref.URL == "" {
		return nil, &RefError{Input: ref.Raw, Reason: "archive refs need a url"}
	}

	if ref.SHA256 == "" {
		return nil, &RefError{Input: ref.Raw, Reason: "archive refs need a #sha256= pin"}
	}

	archivePath := filepath.Join(tmp, "archive")

	sum, err := f.download(ctx, ref, archivePath)
	if err != nil {
		return nil, err
	}

	if sum != ref.SHA256 {
		return nil, &PinMismatchError{Want: ref.SHA256, Got: sum}
	}

	payload := filepath.Join(tmp, "payload")
	if err := fsutil.EnsureDir(payload, 0o700); err != nil {
		return nil, fetchError(ref, StepCache, err)
	}

	warnings, err := extractArchive(ctx, archivePath, payload)
	if err != nil {
		return nil, fetchError(ref, StepExtract, err)
	}

	root, rootRel := stripSingleRoot(payload, "payload")

	tree, err := digest.TreeWithSkip(root, skipGitDir)
	if err != nil {
		return nil, fetchError(ref, StepDigest, err)
	}

	return &entryMeta{
		Kind: KindURL, Root: rootRel, TreeDigest: tree,
		ArchiveSHA256: sum, Warnings: warnings,
	}, nil
}

// download fetches the archive bytes into dest and returns their sha256.
func (f *Fetcher) download(ctx context.Context, ref Ref, dest string) (digest.Hash, error) {
	if strings.HasPrefix(ref.URL, "file://") {
		parsed, err := url.Parse(ref.URL)
		if err != nil {
			return "", fetchError(ref, StepDownload, err)
		}

		file, err := os.Open(parsed.Path)
		if err != nil {
			return "", fetchError(ref, StepDownload, err)
		}

		defer func() { _ = file.Close() }()

		return copyHashed(file, dest)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return "", fetchError(ref, StepDownload, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fetchError(ref, StepDownload, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fetchError(ref, StepDownload, fmt.Errorf("http status %s", resp.Status))
	}

	return copyHashed(resp.Body, dest)
}

// copyHashed streams src into dest while hashing.
func copyHashed(src io.Reader, dest string) (digest.Hash, error) {
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: dest is inside the fetch temp directory
	if err != nil {
		return "", err
	}

	hash := sha256.New()

	_, copyErr := io.Copy(io.MultiWriter(file, hash), src)

	closeErr := file.Close()

	if copyErr != nil {
		return "", copyErr
	}

	if closeErr != nil {
		return "", closeErr
	}

	return digest.Hash(hex.EncodeToString(hash.Sum(nil))), nil
}

// extractArchive detects the archive format and unpacks it into dest. Archive
// entries escaping dest fail the fetch; link entries are skipped with a
// warning (they could point outside the payload).
func extractArchive(ctx context.Context, archivePath, dest string) ([]string, error) {
	head := make([]byte, 4)

	file, err := os.Open(archivePath) //nolint:gosec // G304: archivePath is inside the fetch temp directory
	if err != nil {
		return nil, err
	}

	n, readErr := io.ReadFull(file, head)

	_ = file.Close()

	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}

	switch {
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return extractTarGz(ctx, archivePath, dest)
	case n >= 4 && bytes.HasPrefix(head[:n], []byte("PK\x03\x04")),
		n >= 4 && bytes.HasPrefix(head[:n], []byte("PK\x05\x06")):
		return extractZip(ctx, archivePath, dest)
	default:
		return nil, errors.New("unsupported archive format")
	}
}

// extractTarGz unpacks a gzipped tarball.
func extractTarGz(ctx context.Context, archivePath, dest string) ([]string, error) {
	file, err := os.Open(archivePath) //nolint:gosec // G304: archivePath is inside the fetch temp directory
	if err != nil {
		return nil, err
	}

	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}

	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	var warnings []string

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		warning, err := extractTarEntry(dest, tr, header)
		if err != nil {
			return nil, err
		}

		if warning != "" {
			warnings = append(warnings, warning)
		}
	}

	return warnings, nil
}

// extractTarEntry unpacks one tar entry; link and special entries are skipped
// with a warning because they could point outside the payload.
func extractTarEntry(dest string, tr *tar.Reader, header *tar.Header) (string, error) {
	target, err := safeTarget(dest, header.Name)
	if err != nil {
		return "", err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return "", fsutil.EnsureDir(target, 0o700)
	case tar.TypeReg:
		return "", writeEntry(dest, target, tr)
	case tar.TypeSymlink, tar.TypeLink:
		return fmt.Sprintf("skipped archive link %q", header.Name), nil
	default:
		return fmt.Sprintf("skipped archive entry %q", header.Name), nil
	}
}

// extractZip unpacks a zip archive.
func extractZip(ctx context.Context, archivePath, dest string) ([]string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}

	defer func() { _ = reader.Close() }()

	var warnings []string

	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		target, err := safeTarget(dest, entry.Name)
		if err != nil {
			return nil, err
		}

		switch {
		case entry.FileInfo().IsDir():
			if err := fsutil.EnsureDir(target, 0o700); err != nil {
				return nil, err
			}
		case entry.Mode()&os.ModeSymlink != 0:
			warnings = append(warnings, fmt.Sprintf("skipped archive link %q", entry.Name))
		default:
			source, err := entry.Open()
			if err != nil {
				return nil, err
			}

			writeErr := writeEntry(dest, target, source)

			_ = source.Close()

			if writeErr != nil {
				return nil, writeErr
			}
		}
	}

	return warnings, nil
}

// safeTarget maps an archive entry name to a path inside dest, refusing
// absolute names and any `..` traversal.
func safeTarget(dest, name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))

	switch {
	case clean == ".":
		return "", fmt.Errorf("archive entry %q has an empty name", name)
	case path.IsAbs(clean):
		return "", fmt.Errorf("archive entry %q is absolute", name)
	}

	for segment := range strings.SplitSeq(clean, "/") {
		if segment == ".." {
			return "", fmt.Errorf("archive entry %q escapes the destination", name)
		}
	}

	target := filepath.Join(dest, filepath.FromSlash(clean))
	if !within(dest, target) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}

	return target, nil
}

// writeEntry writes one regular archive entry under dest.
func writeEntry(dest, target string, source io.Reader) error {
	if !within(dest, target) {
		return errors.New("archive entry escapes the destination")
	}

	if err := fsutil.EnsureDir(filepath.Dir(target), 0o700); err != nil {
		return err
	}

	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: target is contained in dest
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(file, source)

	closeErr := file.Close()

	if copyErr != nil {
		return copyErr
	}

	return closeErr
}

// within reports whether target stays inside dest.
func within(dest, target string) bool {
	rel, err := filepath.Rel(dest, target)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// stripSingleRoot descends into a lone top-level directory (GitHub tarballs,
// npm `package/`), returning the payload root and its entry-relative path.
func stripSingleRoot(payload, relBase string) (string, string) {
	entries, err := os.ReadDir(payload)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return payload, relBase
	}

	return filepath.Join(payload, entries[0].Name()), filepath.Join(relBase, entries[0].Name())
}
