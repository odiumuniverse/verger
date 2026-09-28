package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	pkgid "github.com/odiumuniverse/verger/pkg/id"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// ManifestSchema is the schema of the per-package runtime manifest.
const ManifestSchema = 1

// Hook is one hook as the runtime reads it: the canonical fields the bundle
// hashes for consent, in the lowercase JSON the bundle expects.
type Hook struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// Script is one hook script the runtime verifies before running any hook.
type Script struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is the per-package runtime manifest a shim points at: the hooks the
// runtime runs, and the scripts whose content the consent covers.
type Manifest struct {
	Schema  int      `json:"schema"`
	Package string   `json:"package"`
	Version string   `json:"version"`
	Hooks   []Hook   `json:"hooks"`
	Scripts []Script `json:"scripts"`
}

// NewManifest converts the canonical hooks of one package into a runtime
// manifest. scripts are the hook script files the caller resolved; their
// digests are recorded here and re-checked by the runtime on every load.
func NewManifest(pkg, version string, hooks []manifest.Hook, scripts map[string]digest.Hash) Manifest {
	out := Manifest{Schema: ManifestSchema, Package: pkg, Version: version}

	for _, hook := range hooks {
		out.Hooks = append(out.Hooks, Hook{
			Event: hook.Event, Matcher: hook.Matcher, Command: hook.Command, Timeout: hook.Timeout,
		})
	}

	for _, path := range sortedKeys(scripts) {
		out.Scripts = append(out.Scripts, Script{Path: path, SHA256: scripts[path].String()})
	}

	return out
}

// ManifestPath returns the manifest file of one package inside a runtime
// directory: one file per package, named so two package ids never collide.
func ManifestPath(dir, pkg string) string {
	return filepath.Join(dir, packagesDir, escapePackage(pkg)+".json")
}

// WriteManifest writes the manifest atomically into the runtime directory and
// returns its path and the sha256 of the bytes written. The digest is what the
// shim pins next to the consent hash: it authenticates the whole record,
// including the script digests, so editing the file can no longer re-point a
// script the user approved.
func WriteManifest(dir string, m Manifest) (string, digest.Hash, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode runtime manifest: %w", err)
	}

	path := ManifestPath(dir, m.Package)

	if err := fsutil.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}

	body := append(slices.Clone(data), '\n')

	if err := fsutil.WriteFileAtomic(path, body, 0o600); err != nil {
		return "", "", fmt.Errorf("write runtime manifest %s: %w", path, err)
	}

	return path, digest.Bytes(body), nil
}

// ReadManifest reads one runtime manifest back.
func ReadManifest(path string) (Manifest, error) {
	data, err := readOptional(path)
	if err != nil {
		return Manifest{}, err
	}

	if len(data) == 0 {
		return Manifest{}, fmt.Errorf("runtime manifest %s is missing", path)
	}

	var m Manifest

	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse runtime manifest %s: %w", path, err)
	}

	return m, nil
}

// RemoveManifest deletes the manifest of one package; a missing file is not an
// error.
func RemoveManifest(dir, pkg string) error {
	err := os.Remove(ManifestPath(dir, pkg))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// escapePackage renders a package id as one path element.
func escapePackage(pkg string) string {
	escaped := make([]rune, 0, len(pkg))

	for _, r := range pkg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			escaped = append(escaped, r)
		default:
			escaped = append(escaped, '_')
		}
	}

	return string(escaped)
}

// ValidatePackageID refuses a package id the store cannot carry.
func ValidatePackageID(pkg string) error {
	if err := pkgid.ValidatePackage(pkg); err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	return nil
}

// sortedKeys returns the sorted keys of one map.
func sortedKeys[V any](values map[string]V) []string {
	out := make([]string, 0, len(values))

	for key := range values {
		out = append(out, key)
	}

	slices.Sort(out)

	return out
}
