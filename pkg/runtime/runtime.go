// Package runtime installs and verifies the host runtime: one ESM bundle that
// carries a package's Claude-dialect hooks into OpenCode v1/v2, Kilo and Pi.
//
// The bundle is embedded in the binary (bundle/index.js, built by
// `make runtime` from runtime/src) and extracted into
// <store>/runtime/<host>/<version>/ so the shims the host adapters place point
// at an absolute, versioned path. The shared event table (table/events.json)
// is embedded too: the dialect mapping has one definition for Go and for the
// bundle, and a test pins the two copies together.
package runtime

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Bundle files, embedded at build time.
//
//go:embed bundle/index.js table/events.json
var files embed.FS

// BundleVersion is the version of the embedded runtime bundle. It is part of
// the store path (<store>/runtime/<host>/<version>) and of every shim, so a
// rebuilt runtime lands in a new directory and the shims are re-pointed
// atomically (DESIGN §4.6: an upgrade is a new directory plus an atomic shim
// rewrite).
const BundleVersion = "0.1.0"

// Paths inside one runtime directory.
const (
	bundleFile   = "index.js"
	tableFile    = "events.json"
	packagesDir  = "packages"
	heartbeatDoc = "heartbeat.json"
)

// ErrNotInstalled reports a runtime directory without the bundle.
var ErrNotInstalled = errors.New("runtime is not installed")

// Bundle returns the embedded ESM bundle.
func Bundle() ([]byte, error) {
	return files.ReadFile(filepath.Join("bundle", bundleFile))
}

// TableBytes returns the embedded event table document.
func TableBytes() ([]byte, error) {
	return files.ReadFile(filepath.Join("table", tableFile))
}

// Install extracts the embedded bundle and table into
// <store>/runtime/<host>/<version>/ and returns that directory. The write is
// atomic (a temporary sibling directory, then a rename) and idempotent: an
// existing directory whose bundle digest matches the embedded one is kept as
// is, so a re-delivery never rewrites a runtime the running host has loaded.
func Install(ctx context.Context, st *store.Store, host, version string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if version == "" {
		version = BundleVersion
	}

	dir, err := st.RuntimePath(host, version)
	if err != nil {
		return "", err
	}

	bundle, err := Bundle()
	if err != nil {
		return "", err
	}

	if current, ok := installedDigest(dir); ok && current == digest.Bytes(bundle) {
		return dir, nil
	}

	table, err := TableBytes()
	if err != nil {
		return "", err
	}

	if err := stageRuntime(dir, bundle, table); err != nil {
		return "", err
	}

	return dir, nil
}

// stageRuntime writes one runtime directory atomically: a temporary sibling
// directory, then a rename over the target. A directory that is already there
// is replaced as a whole, because the host may hold the old one open and the
// shims are re-pointed only after this returns.
func stageRuntime(dir string, bundle, table []byte) error {
	parent := filepath.Dir(dir)
	if err := fsutil.EnsureDir(parent, 0o700); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(parent, ".verger-runtime-")
	if err != nil {
		return fmt.Errorf("create runtime staging dir: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	if err := os.WriteFile(filepath.Join(tmp, bundleFile), bundle, 0o600); err != nil {
		return fmt.Errorf("write runtime bundle: %w", err)
	}

	if err := os.WriteFile(filepath.Join(tmp, tableFile), table, 0o600); err != nil {
		return fmt.Errorf("write runtime table: %w", err)
	}

	if err := fsutil.EnsureDir(filepath.Join(tmp, packagesDir), 0o700); err != nil {
		return err
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("replace runtime %s: %w", dir, err)
	}

	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("install runtime %s: %w", dir, err)
	}

	return nil
}

// Installed reports whether a runtime directory carries the bundle.
func Installed(st *store.Store, host, version string) bool {
	dir, err := st.RuntimePath(host, version)
	if err != nil {
		return false
	}

	_, ok := installedDigest(dir)

	return ok
}

// BundlePath returns the absolute path of the bundle inside one runtime
// directory.
func BundlePath(dir string) string {
	return filepath.Join(dir, bundleFile)
}

// installedDigest reads the digest recorded for one runtime directory; ok is
// false when the bundle is absent or unreadable.
func installedDigest(dir string) (digest.Hash, bool) {
	data, err := os.ReadFile(filepath.Join(dir, bundleFile)) //nolint:gosec // G304: the path is below the store root
	if err != nil {
		return "", false
	}

	return digest.Bytes(data), true
}

// KnownHosts lists the hosts the embedded table carries a dialect for. The
// host ids are the ones pkg/host uses; the runtime keeps its own copy so the
// package can be used without the adapters (and the adapters without a cycle).
func KnownHosts() []string {
	return []string{"opencode", "kilo", "pi"}
}

// Dialect returns the runtime dialect of one host id: opencode speaks v2 by
// default (the adapter passes the version it probed), kilo the v1 dialect, pi
// its own.
func Dialect(host string) (string, bool) {
	switch host {
	case "opencode":
		return "v2", true
	case "kilo":
		return "v1", true
	case "pi":
		return "pi", true
	default:
		return "", false
	}
}

// cleanPath refuses a path that is not absolute: a shim carries absolute paths
// the host resolves from its own working directory.
func cleanPath(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("runtime: %q is not an absolute path", value)
	}

	return filepath.Clean(value), nil
}

// sortedUnique returns the sorted distinct values.
func sortedUnique(values []string) []string {
	out := slices.Clone(values)

	slices.Sort(out)
	out = slices.Compact(out)

	return out
}

// readOptional reads a file, returning nil for a missing one.
func readOptional(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is below the store root or a host config dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return data, nil
}
