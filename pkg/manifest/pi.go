package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// piManifestFile is pi's own package manifest: an npm `package.json` carrying a
// `pi` key (pi 0.74.2, docs/packages.md, live-probed in W1-B).
const piManifestFile = "package.json"

// PiPackageError reports a payload that is a pi package — a `package.json` with a
// `pi` key — which is not one of the formats verger parses.
//
// The refusal is explicit because the alternative is worse: a payload with no
// recognised manifest parses into a versionless package that fails much later,
// inside the executor, with a message about a missing version (W3-E2E10 F4).
type PiPackageError struct {
	Path   string
	Reason string
}

// Error implements error.
func (e *PiPackageError) Error() string {
	return e.Path + ": " + e.Reason
}

// piReason names what a caller can do with a pi package today.
const piReason = "this is a pi package (package.json with a \"pi\" key); verger does not parse that " +
	"format yet, so it cannot install it — install it with `pi install <path>` or republish it as an " +
	"agent-plugins package"

// piPackageMeta is the part of a pi package.json the reader needs.
type piPackageMeta struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	Keywords []string        `json:"keywords"`
	Pi       json.RawMessage `json:"pi"`
}

// IsPiPackage reports whether root is a pi package: an npm package.json that
// declares the `pi` key or the `pi-package` keyword. It is the check the fetcher's
// best-effort parse would otherwise swallow, so a host plan can refuse the
// payload by name instead of failing in the executor.
func IsPiPackage(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, piManifestFile)) //nolint:gosec // G304: the path is the payload root
	if err != nil {
		return false
	}

	var meta piPackageMeta

	if json.Unmarshal(data, &meta) != nil {
		return false
	}

	if len(meta.Pi) > 0 {
		return true
	}

	return slices.Contains(meta.Keywords, "pi-package")
}

// PiPackageRefusal returns the typed refusal for a pi package payload, or nil
// when the payload is something verger does parse.
func PiPackageRefusal(root string) error {
	if !IsPiPackage(root) {
		return nil
	}

	return &PiPackageError{Path: root, Reason: piReason}
}

// IsPiPackageError reports whether err is the pi-package refusal.
func IsPiPackageError(err error) bool {
	var typed *PiPackageError

	return errors.As(err, &typed)
}
