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
// with the same URL is kept as-is.
func AddSpecSource(doc *spec.Spec, ref source.Ref) bool {
	raw := ref.Raw
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
