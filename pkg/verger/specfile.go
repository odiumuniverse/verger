package verger

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// LoadSpec reads one spec document; a missing file is an empty spec and
// present=false.
func LoadSpec(path string) (*spec.Spec, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the spec file
	if errors.Is(err, fs.ErrNotExist) {
		return spec.New(), false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read spec %s: %w", path, err)
	}

	parsed, err := spec.Parse(data)
	if err != nil {
		return nil, false, err
	}

	return parsed, true, nil
}

// SaveSpec writes one spec document, creating the parent directory.
func SaveSpec(path string, doc *spec.Spec) error {
	if err := fsutil.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return doc.Save(path)
}

// AddSpecPackage inserts a package entry when absent and reports whether the
// spec changed.
func AddSpecPackage(doc *spec.Spec, id, version string) bool {
	for _, pkg := range doc.Packages {
		if pkg.ID == id {
			return false
		}
	}

	doc.Packages = append(doc.Packages, spec.Package{ID: id, Version: version})

	return true
}

// RemoveSpecPackage drops every entry with the id and reports the count.
func RemoveSpecPackage(doc *spec.Spec, id string) int {
	kept := make([]spec.Package, 0, len(doc.Packages))
	removed := 0

	for _, pkg := range doc.Packages {
		if matchesID(pkg.ID, id) {
			removed++

			continue
		}

		kept = append(kept, pkg)
	}

	doc.Packages = kept

	return removed
}

// SetSpecPin stores or clears the version pin of one package.
func SetSpecPin(doc *spec.Spec, id, version string) (bool, error) {
	for i := range doc.Packages {
		if doc.Packages[i].ID == id {
			doc.Packages[i].Version = version

			return true, nil
		}
	}

	return false, &UsageError{Cause: fmt.Errorf("package %q is not in the spec", id)}
}

// SourceName derives a stable source name from one ref.
func SourceName(ref source.Ref) string {
	switch {
	case ref.Repo != "":
		return ref.Repo
	case ref.Path != "":
		return filepath.Base(ref.Path)
	default:
		return filepath.Base(ref.Raw)
	}
}

// AddSpecSource records one fetched ref as a spec source; an existing source
// with the same URL is kept as-is. It records the ref exactly as the caller
// wrote it — use AddSpecSourceAt when the spec's own directory is known, so a
// local path can be stored in a form that survives a clone.
func AddSpecSource(doc *spec.Spec, ref source.Ref) bool {
	return addSpecSource(doc, ref, "")
}

// AddSpecSourceAt records one fetched ref, rewriting a local path that lies
// under specDir into a path relative to it.
//
// This is the difference between a vault that works on the machine that
// created it and one that works everywhere. An absolute path is only correct
// on the machine that wrote it: clone the repo to a laptop and the source
// points at a directory that does not exist. source.Parse already resolves a
// relative local ref against the spec's own directory, so storing "./x" is
// enough — and `..` is never needed, because a path that is not under
// specDir keeps its absolute spelling and is reported instead.
func AddSpecSourceAt(doc *spec.Spec, ref source.Ref, specDir string) bool {
	return addSpecSource(doc, ref, specDir)
}

// addSpecSource is the shared body; an empty specDir disables the rewrite.
func addSpecSource(doc *spec.Spec, ref source.Ref, specDir string) bool {
	raw, _ := portableSourceURL(ref, specDir)

	// A ref that names no place of its own — a bare `owner/name`, which the
	// grammar reads as GitHub and which the spec's own sources may well have
	// answered instead — has no address to write down. Recording its id as a
	// url produced a source pointing at itself (`url = 'acme/caveman'`): a
	// second false address for a package that already had a real one, and one
	// more on every install.
	if !refNamesAPlace(ref) {
		return false
	}

	name := SourceName(ref)

	for _, src := range doc.Sources {
		if src.URL == raw {
			return false
		}

		if src.Name == name {
			suffix := digest.Bytes([]byte(raw)).String()
			name = name + "-" + suffix[len(suffix)-6:]
		}
	}

	doc.Sources = append(doc.Sources, spec.Source{Name: name, URL: raw})

	return true
}

// refNamesAPlace reports whether a ref carries a location that can be written
// into a spec. A local path, a git or url specifier and an npm name all do; a
// bare `owner/name` does not — it is an id, and an id is not an address.
func refNamesAPlace(ref source.Ref) bool {
	return ref.Kind != source.KindGitHub || ref.Raw != ref.ID
}

// NonPortableSources returns the spec's local sources that will not resolve
// on another machine — an absolute path, or one outside the spec's own
// directory.
//
// It is derived rather than stored: a flag written into the document would
// need a schema field and a migration to mean something the paths already
// mean on their own. `verger doctor` reports what this returns, so a vault
// that would break on clone is visible before anyone clones it.
func NonPortableSources(doc *spec.Spec, specDir string) []string {
	if doc == nil || specDir == "" {
		return nil
	}

	var out []string

	for _, src := range doc.Sources {
		if !isLocalSourceURL(src.URL) {
			continue
		}

		// A relative source is resolved against the spec's own directory,
		// which is exactly what source.Parse does with it. Comparing the
		// stored spelling directly would call "./x" non-portable.
		abs := src.URL
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(specDir, abs)
		}

		rel, err := filepath.Rel(specDir, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out = append(out, src.URL)
		}
	}

	return out
}

// portableSourceURL returns the URL to store for one ref and whether it will
// still resolve on another machine.
func portableSourceURL(ref source.Ref, specDir string) (url string, portable bool) {
	if ref.Kind != source.KindLocal || ref.Path == "" || specDir == "" {
		return ref.Raw, true
	}

	rel, err := filepath.Rel(specDir, ref.Path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Outside the spec directory: a relative spelling would need "..",
		// which a local ref is not allowed to contain, and an absolute one
		// only works here.
		return ref.Path, false
	}

	return "./" + filepath.ToSlash(rel), true
}

// RemoveSpecSource drops every source with the name.
func RemoveSpecSource(doc *spec.Spec, name string) int {
	kept := make([]spec.Source, 0, len(doc.Sources))
	removed := 0

	for _, src := range doc.Sources {
		if src.Name == name {
			removed++

			continue
		}

		kept = append(kept, src)
	}

	doc.Sources = kept

	return removed
}

// ReceiptFilesPresent reports whether every artifact one receipt claims is
// still on disk.
//
// A receipt is machine-local evidence, not a portable claim: when a vault is
// cloned, the receipts either stay behind or arrive describing files this
// machine never had. Neither a receipt nor a lock cell says anything about
// what is on this disk, so both status and sync have to look.
func ReceiptFilesPresent(record receipt.Receipt) bool {
	for _, artifact := range record.Artifacts {
		if _, err := os.Stat(artifact.Path); err != nil {
			return false
		}
	}

	return true
}

// matchesID reports whether a stored package id answers a caller query: the
// exact id, a bare short name matching any owner, or the short name of a
// canonical `local:<name>` id.
func matchesID(stored, query string) bool {
	if stored == query {
		return true
	}

	if strings.Contains(query, "/") {
		return false
	}

	_, short := SplitID(stored)
	if short == query {
		return true
	}

	return strings.TrimPrefix(short, "local:") == query
}
