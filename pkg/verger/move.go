package verger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// movedFiles are the state files a home move carries, in the order they are
// merged. `verger.toml` and `verger.lock` merge by key; the state files are
// taken whole, because a journal or a tombstone log has no per-entry identity
// the merge could use.
var movedFiles = []string{
	"verger.toml",
	"verger.lock",
	filepath.Join("state", "journal.jsonl"),
	filepath.Join("state", "tombstones.json"),
	filepath.Join("state", "consent.json"),
	filepath.Join("state", "trust.json"),
	// The secrets file is the last thing a merge touches: MigrateSecrets has
	// already copied its values into the target, so moving it completes the
	// hand-over without a half-written window.
	filepath.Join("state", "secrets.json"),
}

// moveHomes implements Absorb (merge) and Eject (refuse a non-empty target).
// Both take the source home's state into the target: merge=false is the
// stricter direction, where a target that already holds state is refused rather
// than merged, because ejecting into a home another tool owns would silently
// drop one of the two.
func moveHomes(_ context.Context, from, to *Client, merge bool) (*AbsorbReport, error) {
	if from == nil || to == nil {
		return nil, errors.New("absorb: both homes are required")
	}

	src, dst := from.Home().Root(), to.Home().Root()

	if src == dst {
		return nil, fmt.Errorf("absorb: source and target home are the same directory (%s)", src)
	}

	if err := refuseNonEmptyTarget(dst, merge); err != nil {
		return nil, err
	}

	report := &AbsorbReport{HomeFrom: src, HomeTo: dst, Merge: merge}

	if err := from.Ensure(Paths{Scope: User}); err != nil {
		return nil, err
	}

	// With nothing of the target's own to preserve there is nothing to merge:
	// rename the whole home, so state this merge does not know about — the lock
	// file, a live lease, unknown minor fields — travels with it (DESIGN §3.3).
	// This runs before the target is laid out, because a rename cannot replace
	// an existing directory.
	if merge {
		renamed, err := renameHome(from, to, src, dst, report)
		if err != nil {
			return nil, err
		}

		if renamed {
			return report, nil
		}
	}

	// A merge is the case where the secrets do not travel by themselves: the
	// target already has a store, so the values are carried name by name
	// (DESIGN §3.3 read → write → delete).
	if err := MigrateSecrets(from, to); err != nil {
		return nil, err
	}

	if err := to.Ensure(Paths{Scope: User}); err != nil {
		return nil, err
	}

	if err := moveStateFiles(src, dst, merge, report); err != nil {
		return nil, err
	}

	receiptsMoved, receiptsSkipped, err := moveReceipts(src, dst, merge)
	if err != nil {
		return nil, err
	}

	report.Moved = append(report.Moved, receiptsMoved...)
	report.Skipped = append(report.Skipped, receiptsSkipped...)
	slices.Sort(report.Moved)
	slices.Sort(report.Skipped)

	return report, nil
}

// moveStateFiles carries every state file a home move owns, recording which
// moved and which the target already had.
func moveStateFiles(src, dst string, merge bool, report *AbsorbReport) error {
	for _, name := range movedFiles {
		moved, err := moveStateFile(filepath.Join(src, name), filepath.Join(dst, name), name, merge)
		if err != nil {
			return err
		}

		if moved {
			report.Moved = append(report.Moved, name)

			continue
		}

		report.Skipped = append(report.Skipped, name)
	}

	return nil
}

// renameHome tries the whole-home rename that keeps unknown state with the move:
// the secrets travel by value rather than by file, because the target store was
// loaded when its client opened and would not see a file that appears later.
func renameHome(from, to *Client, src, dst string, report *AbsorbReport) (bool, error) {
	copied, err := copySecretValues(from, to)
	if err != nil {
		return false, err
	}

	// A target that already exists is the merge case, not a failure: the caller
	// falls through to the per-file merge.
	if err := renameWholeHome(src, dst); err != nil {
		return false, nil //nolint:nilerr // "cannot rename" means "merge instead"
	}

	report.Renamed = true

	if copied {
		if err := to.Secrets().Save(); err != nil {
			return false, fmt.Errorf("write the moved secrets: %w", err)
		}
	}

	return true, nil
}

// refuseNonEmptyTarget reports why an eject into a home that already holds state
// is refused; a merge (absorb) is allowed to find state, because that is what it
// merges.
func refuseNonEmptyTarget(dst string, merge bool) error {
	if merge {
		return nil
	}

	existing, err := existingState(dst)
	if err != nil {
		return err
	}

	if len(existing) > 0 {
		return fmt.Errorf("eject: %s already holds state (%s); refusing to merge", dst, existing[0])
	}

	return nil
}

// renameWholeHome moves a home wholesale when the target does not exist yet.
// It refuses a target that already holds state, which is the merge case.
func renameWholeHome(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return errors.New("target already exists")
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	if err := os.Rename(src, dst); err != nil {
		// Another filesystem: copy, sync, remove (DESIGN §3.3).
		return fmt.Errorf("rename %s -> %s: %w", src, dst, err)
	}

	return nil
}

// existingState lists the state files already present in one home.
func existingState(root string) ([]string, error) {
	var found []string

	for _, name := range movedFiles {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			found = append(found, name)
		}
	}

	return found, nil
}

// moveStateFile carries one state file from the source home to the target. A
// spec or lock merges by key; anything else moves whole when the target has
// none and is reported as skipped when it already has one.
func moveStateFile(src, dst, name string, merge bool) (bool, error) {
	data, err := os.ReadFile(src) //nolint:gosec // G304: the path is derived from a resolved home root
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}

	switch name {
	case "verger.toml":
		return mergeSpec(src, dst, data, merge)
	case "verger.lock":
		return mergeLock(src, dst, data, merge)
	default:
		return moveWholeFile(src, dst, data)
	}
}

// moveWholeFile writes data to dst when the target has no file there, and
// reports the move as skipped when it does.
func moveWholeFile(src, dst string, data []byte) (bool, error) {
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	}

	if err := fsutil.EnsureDir(filepath.Dir(dst), 0o700); err != nil {
		return false, err
	}

	if err := fsutil.WriteFileAtomic(dst, data, 0o600); err != nil {
		return false, err
	}

	if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}

	return true, nil
}

// mergeSpec merges the source spec into the target's, package by package
// (DESIGN §3.3): a package the target does not have is taken whole, a package
// both have keeps the target's version.
func mergeSpec(src, dst string, data []byte, merge bool) (bool, error) {
	target, hadTarget, err := loadSpecFile(dst)
	if err != nil {
		return false, err
	}

	source, _, err := parseSpecFile(src, data)
	if err != nil {
		return false, err
	}

	if hadTarget && !merge {
		return false, nil
	}

	changed := false

	for _, pkg := range source.Packages {
		if hasSpecPackage(target, pkg.ID) {
			continue
		}

		target.Packages = append(target.Packages, pkg)
		changed = true
	}

	for _, src := range source.Sources {
		if hasSpecSource(target, src.Name) {
			continue
		}

		target.Sources = append(target.Sources, src)
		changed = true
	}

	if !changed {
		return false, nil
	}

	return true, saveSpecFile(dst, target)
}

// mergeLock merges the source lock into the target's, cell by cell (DESIGN
// §3.3): a cell the target does not have is taken, a cell both have keeps the
// newer version.
func mergeLock(src, dst string, data []byte, merge bool) (bool, error) {
	target, hadTarget, err := loadLockFile(dst)
	if err != nil {
		return false, err
	}

	source, _, err := parseLockFile(src, data)
	if err != nil {
		return false, err
	}

	if hadTarget && !merge {
		return false, nil
	}

	changed := false

	for _, cell := range source.Cells {
		existing, found := target.Cell(cell.Package, cell.Host, cell.Scope)
		if found && !cell.UpdatedAt.After(existing.UpdatedAt) {
			continue
		}

		if err := target.Upsert(cell); err != nil {
			return false, err
		}

		changed = true
	}

	if !changed {
		return false, nil
	}

	return true, saveLockFile(dst, target)
}

// moveReceipts carries the receipt tree cell by cell: a cell the target does
// not have is taken whole, a cell both have keeps the target's.
func moveReceipts(src, dst string, merge bool) (moved, skipped []string, err error) {
	source := receipt.NewStore(filepath.Join(src, "state", "receipts"))
	target := receipt.NewStore(filepath.Join(dst, "state", "receipts"))

	list, listErr := source.List()
	if listErr != nil {
		return nil, nil, fmt.Errorf("read source receipts: %w", listErr)
	}

	for _, record := range list {
		existing, found, getErr := target.Get(record.Package, record.Host, record.Scope)
		if getErr != nil {
			return nil, nil, getErr
		}

		relative := filepath.Join(record.Package, fmt.Sprintf("%s-%s.json", record.Host, record.Scope))
		sourcePath := filepath.Join(src, "state", "receipts", relative)

		if found && !record.InstalledAt.After(existing.InstalledAt) {
			skipped = append(skipped, relative)

			continue
		}

		if !merge {
			skipped = append(skipped, relative)

			continue
		}

		putErr := target.Put(record)
		if putErr != nil {
			return nil, nil, putErr
		}

		moved = append(moved, relative)

		if removeErr := os.Remove(sourcePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, nil, removeErr
		}
	}

	return moved, skipped, nil
}

// MigrateSecrets carries the secret values of one home into another, the way
// DESIGN §3.3 describes a keychain hand-over: read → write → delete, once per
// name, and idempotent — a name the target already holds with the same value is
// left alone, so a repeated absorb neither duplicates nor drops anything. The
// keychain service name travels with the store, because each client opened its
// own (beadle's service when it embeds verger, "verger" when it does not).
func MigrateSecrets(from, to *Client) error {
	copied, err := copySecretValues(from, to)
	if err != nil {
		return err
	}

	if copied {
		if err := to.Secrets().Save(); err != nil {
			return fmt.Errorf("write the target secrets: %w", err)
		}
	}

	src := from.Secrets()
	if src == nil {
		return nil
	}

	for _, name := range src.Names() {
		src.Delete(name)
	}

	if src.Changed() {
		if err := src.Save(); err != nil {
			return fmt.Errorf("clear the source secrets: %w", err)
		}
	}

	return nil
}

// copySecretValues copies every source value into the target store in memory
// and reports whether anything changed. It saves nothing and creates nothing:
// the caller decides when the target directory exists, which matters because a
// whole-home rename must still find an absent target.
func copySecretValues(from, to *Client) (bool, error) {
	src, dst := from.Secrets(), to.Secrets()

	if src == nil || dst == nil || src.Path() == dst.Path() {
		return false, nil
	}

	changed := false

	for _, name := range src.Names() {
		value, ok := src.Get(name)
		if !ok {
			continue
		}

		if current, has := dst.Get(name); has && current == value {
			continue
		}

		dst.Set(name, value)

		changed = true
	}

	return changed, nil
}

// loadSpecFile reads one spec document; a missing file is an empty spec.
func loadSpecFile(path string) (*spec.Spec, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from a resolved home root
	if errors.Is(err, os.ErrNotExist) {
		return spec.New(), false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	doc, _, err := parseSpecFile(path, data)

	return doc, true, err
}

// parseSpecFile parses one spec document's bytes.
func parseSpecFile(path string, data []byte) (*spec.Spec, bool, error) {
	doc, err := spec.Parse(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse spec %s: %w", path, err)
	}

	return doc, true, nil
}

// saveSpecFile writes one spec document.
func saveSpecFile(path string, doc *spec.Spec) error {
	return doc.Save(path)
}

// loadLockFile reads one lock document; a missing file is an empty lock.
func loadLockFile(path string) (*lock.Lock, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from a resolved home root
	if errors.Is(err, os.ErrNotExist) {
		return lock.New(), false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	doc, _, err := parseLockFile(path, data)

	return doc, true, err
}

// parseLockFile parses one lock document's bytes.
func parseLockFile(path string, data []byte) (*lock.Lock, bool, error) {
	doc, err := lock.Parse(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse lock %s: %w", path, err)
	}

	return doc, true, nil
}

// saveLockFile writes one lock document.
func saveLockFile(path string, doc *lock.Lock) error {
	return doc.Save(path)
}

// hasSpecPackage reports whether the spec already declares one package id.
func hasSpecPackage(doc *spec.Spec, id string) bool {
	for _, pkg := range doc.Packages {
		if pkg.ID == id {
			return true
		}
	}

	return false
}

// hasSpecSource reports whether the spec already declares one source name.
func hasSpecSource(doc *spec.Spec, name string) bool {
	for _, src := range doc.Sources {
		if src.Name == name {
			return true
		}
	}

	return false
}
