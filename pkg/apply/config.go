package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// configValueDigest returns the canonical JSON digest of the value at a dotted
// key path of a JSONC or TOML document; exists is false when the key is absent.
// JSON is a JSONC subset, so the JSONC parse is tried first.
func configValueDigest(data []byte, keyPath string) (digest.Hash, bool, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "", false, nil
	}

	if standard, err := standardizeJSONC(data); err == nil {
		var doc any

		if err := json.Unmarshal(standard, &doc); err == nil {
			return digestKeyPath(doc, keyPath)
		}
	}

	var doc map[string]any

	if err := toml.Unmarshal(data, &doc); err == nil {
		return digestKeyPath(doc, keyPath)
	}

	return "", false, errors.New("the config document is neither JSONC nor TOML")
}

// standardizeJSONC parses and standardizes a JSONC document.
func standardizeJSONC(data []byte) ([]byte, error) {
	root, err := hujson.Parse(data)
	if err != nil {
		return nil, err
	}

	return hujson.Standardize(root.Pack())
}

// digestKeyPath resolves a dotted key path and hashes the value canonically.
func digestKeyPath(doc any, keyPath string) (digest.Hash, bool, error) {
	value, ok := lookupKeyPath(doc, keyPath)
	if !ok {
		return "", false, nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return "", false, fmt.Errorf("canonical value: %w", err)
	}

	return digest.Bytes(data), true, nil
}

// lookupKeyPath walks a dotted key path through decoded maps and arrays.
// Array indices are written as [N] appended to the segment (e.g. "hooks.beforeShellExecution[0]").
func lookupKeyPath(doc any, keyPath string) (any, bool) {
	current := doc

	for segment := range strings.SplitSeq(keyPath, ".") {
		bracket := strings.Index(segment, "[")
		if bracket == -1 {
			var ok bool

			if current, ok = mapEntry(current, segment); !ok {
				return nil, false
			}

			continue
		}

		// A segment with a suffix is a key that holds an array, so the key
		// comes first and the indices are walked from there.
		if segment[:bracket] != "" {
			var ok bool

			if current, ok = mapEntry(current, segment[:bracket]); !ok {
				return nil, false
			}
		}

		var walked bool

		if current, walked = walkArrayIndices(current, segment[bracket:]); !walked {
			return nil, false
		}
	}

	return current, true
}

// mapEntry reads one key of a JSON object. A document that is not an object
// where a key was asked for is a path that does not exist, not an error.
func mapEntry(current any, key string) (any, bool) {
	object, ok := current.(map[string]any)
	if !ok {
		return nil, false
	}

	value, ok := object[key]

	return value, ok
}

// walkArrayIndices follows the [N][M] suffixes of one segment, returning the
// last element and whether every index resolved.
func walkArrayIndices(current any, rest string) (any, bool) {
	for len(rest) > 0 && rest[0] == '[' {
		end := strings.Index(rest, "]")
		if end == -1 {
			return nil, false
		}

		idxStr := rest[1:end]
		rest = rest[end+1:]

		arr, ok := current.([]any)
		if !ok {
			return nil, false
		}

		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(arr) {
			return nil, false
		}

		current = arr[idx]
	}

	return current, true
}

// configEdit applies one key-path edit to a JSONC or TOML document.
func configEdit(data []byte, keyPath string, value any, remove bool, owned digest.Hash) ([]byte, error) {
	edit := render.Edit{Path: keyPath, Value: value, Delete: remove}

	ownedDigests := render.Owned{}
	if owned.Valid() {
		ownedDigests[keyPath] = owned
	}

	if isJSONC(data) {
		out, _, err := render.EditJSONC(data, []render.Edit{edit}, ownedDigests)

		return out, err
	}

	out, _, err := render.EditTOML(data, []render.Edit{edit}, ownedDigests)

	return out, err
}

// isJSONC reports whether the document parses as JSONC (or is empty).
func isJSONC(data []byte) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}

	_, err := standardizeJSONC(data)

	return err == nil
}

// undoConfigKey reverses one config-key operation: a digest match unsets the
// key or restores the trashed previous value; a mismatch leaves the document
// alone.
func (r *runner) undoConfigKey(ref rmaRef, op receipt.Op, mode rmaMode) (string, bool, error) {
	data, err := readConfig(op.Path)
	if err != nil {
		return "", false, err
	}

	current, exists, err := configValueDigest(data, op.KeyPath)
	if err != nil {
		return "", false, fmt.Errorf("digest %s#%s: %w", op.Path, op.KeyPath, err)
	}

	if !exists {
		return r.restoreConfigKey(op, data, "")
	}

	if current != op.Digest {
		return r.configMismatch(op, mode)
	}

	if op.Existed && op.Backup != "" {
		return r.restoreConfigKey(op, data, current)
	}

	// A key recorded as pre-existing with no backup is a key the package found
	// already holding its own value: nothing was replaced, so there is no
	// trashed value to put back. The claim that makes removing it safe is the
	// VALUE — the key on disk still hashes to what the receipt recorded, so it is
	// still the key verger owns, and a removal that leaves it behind has wired a
	// deleted package into a host for good.
	//
	// A rollback does not get that claim. The failing delivery never replaced
	// this key, so unsetting it would destroy a value the user had before the
	// run started.
	if op.Existed && mode == modeRollback {
		return fmt.Sprintf("%s#%s: hands-off (no backup recorded); left in place", op.Path, op.KeyPath), true, nil
	}

	out, err := configEdit(data, op.KeyPath, nil, true, current)
	if err != nil {
		return "", false, fmt.Errorf("unset %s#%s: %w", op.Path, op.KeyPath, err)
	}

	if err := r.writeConfig(op.Path, out); err != nil {
		return "", false, err
	}

	return "", false, nil
}

// readConfig reads one config document; a missing file is an empty document.
func readConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path comes from a receipt
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return data, nil
}

// configMismatch interprets a config digest mismatch for one mode.
func (r *runner) configMismatch(op receipt.Op, mode rmaMode) (string, bool, error) {
	if mode == modeRollback {
		return "", false, nil // the step never ran
	}

	return fmt.Sprintf("%s#%s: hands-off (the value changed since the receipt); left in place", op.Path, op.KeyPath), true, nil
}

// restoreConfigKey writes the trashed previous value of one config key back; a
// key that was absent before is left unset.
func (r *runner) restoreConfigKey(op receipt.Op, data []byte, owned digest.Hash) (string, bool, error) {
	if !op.Existed || op.Backup == "" {
		return "", false, nil // idempotent: the key is already gone
	}

	value, err := r.backupValue(op.Backup)
	if err != nil {
		return "", true, err
	}

	out, err := configEdit(data, op.KeyPath, value, false, owned)
	if err != nil {
		return "", true, fmt.Errorf("restore %s#%s: %w", op.Path, op.KeyPath, err)
	}

	if err := r.writeConfig(op.Path, out); err != nil {
		return "", true, err
	}

	r.consumeBackup(op.Backup)

	return fmt.Sprintf("restored %s#%s from the trash", op.Path, op.KeyPath), false, nil
}

// backupValue decodes the value stored in one trash bucket.
func (r *runner) backupValue(id string) (any, error) {
	entry, err := r.deps.Store.Trash().Get(id)
	if err != nil {
		return nil, fmt.Errorf("backup %s: %w", id, err)
	}

	data, err := os.ReadFile(filepath.Join(r.deps.Store.TrashDir(), id, entry.Stored)) //nolint:gosec // G304: the bucket id comes from a receipt
	if err != nil {
		return nil, fmt.Errorf("read backup %s: %w", id, err)
	}

	var value any

	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode backup %s: %w", id, err)
	}

	return value, nil
}

// consumeBackup drops a consumed config backup bucket on a best-effort basis.
func (r *runner) consumeBackup(id string) {
	if err := r.deps.Store.Trash().Remove(id); err != nil {
		r.logf("apply: remove consumed backup %s: %v", id, err)
	}
}

// writeConfig writes one config document atomically, preserving its mode.
func (r *runner) writeConfig(path string, data []byte) error {
	perm := fs.FileMode(0o600)

	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}

	if err := fsutil.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	if err := fsutil.WriteFileAtomic(path, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// DriftReport names what changed in one receipt's documents since it was
// written. It is per config key, never per document: a user who added their own
// key or record to a document verger also writes changes nothing here, and only
// the key that actually moved is reported (DRIFT-1, DRIFT-2).
type DriftReport struct {
	Package string
	Host    string
	Scope   string
	// Keys are the config keys whose value moved, sorted.
	Keys []string
	// Paths are the documents that were read, sorted.
	Paths []string
}

// Drifted reports whether anything moved.
func (d DriftReport) Drifted() bool { return len(d.Keys) > 0 }

// Note renders one report for a status row.
func (d DriftReport) Note() string {
	if !d.Drifted() {
		return ""
	}

	return strings.Join(d.Keys, ", ") + " changed outside verger"
}

// MCPDrift is the answer of one CLI-managed MCP server probe.
type MCPDrift struct {
	Sum     digest.Hash
	Gone    bool // the host no longer has the server
	Unknown bool // the host could not be asked
}

// DriftProbe answers the current value of one artifact the host manages
// itself, so a receipt digest can be compared against what the host holds. A
// host whose MCP surface is a document needs no probe: the config-key op is
// the claim.
type DriftProbe interface {
	MCPDigest(ctx context.Context, name string) (MCPDrift, error)
}

// ReceiptDrift compares one receipt against the documents it wrote, key by key,
// and against the host's own value of every CLI-managed MCP server it claims.
// A missing key is not drift (the artifact is gone, which the status matrix
// reports as a missing cell), and a key whose value still matches is not drift,
// whatever else the document now carries. probe may be nil: a receipt with no
// CLI-managed MCP server then reports no MCP drift.
func ReceiptDrift(ctx context.Context, record receipt.Receipt, probe DriftProbe) (DriftReport, error) {
	report := DriftReport{Package: record.Package, Host: record.Host, Scope: record.Scope}

	keys := map[string]bool{}
	paths := map[string]bool{}

	if err := claimDrift(record, receipt.OpConfigKey, configKeyClaim, keys, paths); err != nil {
		return DriftReport{}, err
	}

	// Per-record drift (DRIFT-2): a record op claims one value inside a
	// document this package owns (e.g. one hook record in hooks.json). The
	// note is a dotted key path with optional array indices; the digest is
	// compared against the current value at that path.
	if err := claimDrift(record, receipt.OpRecord, recordClaim, keys, paths); err != nil {
		return DriftReport{}, err
	}

	// Whole-file drift: a delivered file whose content no longer matches the
	// digest the receipt recorded is the user's own edit. This is the one
	// check that must never be skipped — it is what stops a redelivery from
	// overwriting work the user did, so a plain write-file artifact is
	// compared exactly like a config key. A file that is not there at all is
	// not drift: it is reported as missing, which reinstalls rather than
	// argues with anyone.
	fileKeys, err := fileDriftKeys(record)
	if err != nil {
		return DriftReport{}, err
	}

	for key := range fileKeys {
		keys[key] = true
	}
	// A CLI-managed MCP server has no config-key op: the host holds the value,
	// so the receipt digest is compared against what the host reports now.
	mcpKeys, err := mcpDriftKeys(ctx, record, probe)
	if err != nil {
		return DriftReport{}, err
	}

	for key := range mcpKeys {
		keys[key] = true
	}

	report.Keys = slices.Sorted(maps.Keys(keys))
	report.Paths = slices.Sorted(maps.Keys(paths))

	return report, nil
}

// claimDrift walks the ops of one kind that claim a value inside a document
// this package owns, and records the ones whose value moved. The two kinds
// differ only in where the value's path is written — a config key's KeyPath
// and a record's Note — and sharing the walk is what keeps them from
// drifting into two different definitions of "the same document".
func claimDrift(
	record receipt.Receipt, kind receipt.OpKind, claim func(receipt.Op) string, keys, paths map[string]bool,
) error {
	for _, op := range record.RMA {
		if op.Kind != kind {
			continue
		}

		keyPath := claim(op)
		if op.Path == "" || keyPath == "" {
			continue
		}

		data, err := os.ReadFile(op.Path) //nolint:gosec // G304: the path comes from a receipt
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("read %s: %w", op.Path, err)
		}

		current, exists, err := configValueDigest(data, keyPath)
		if err != nil {
			return fmt.Errorf("parse %s: %w", op.Path, err)
		}

		paths[op.Path] = true

		if exists && current != op.Digest {
			keys[op.Path+"#"+keyPath] = true
		}
	}

	return nil
}

// configKeyClaim is where a config-key op writes the path of the value it
// claims.
func configKeyClaim(op receipt.Op) string { return op.KeyPath }

// recordClaim is where a record op writes the same thing, as a note.
func recordClaim(op receipt.Op) string { return op.Note }

// mcpDriftKeys compares every CLI-managed MCP server a receipt claims against
// the host's own reported value; a host that cannot be asked contributes none.
func mcpDriftKeys(ctx context.Context, record receipt.Receipt, probe DriftProbe) (map[string]bool, error) {
	if probe == nil {
		return nil, nil
	}

	keys := map[string]bool{}

	for _, artifact := range record.Artifacts {
		if artifact.Kind != "mcp" || artifact.Path == "" {
			continue
		}

		prefix := record.Host + "://mcp/"
		if !strings.HasPrefix(artifact.Path, prefix) {
			continue
		}

		result, err := probe.MCPDigest(ctx, strings.TrimPrefix(artifact.Path, prefix))
		if err != nil {
			return nil, err
		}

		if result.Unknown {
			continue
		}

		if result.Gone || result.Sum != artifact.Digest {
			keys[artifact.Path] = true
		}
	}

	return keys, nil
}

// fileDriftKeys compares every whole file a receipt wrote against the content
// on disk. Only write-file and copy-tree ops carry such a claim: a symlink or
// a hardlink op says nothing about the bytes at the far end, and a config-key
// or record op is compared inside its document by the walks above.
//
// A file that is absent contributes nothing. "Missing" is a different
// verdict with a different remedy — reinstall — and reporting it as drift
// would turn a deleted file into a conflict the user has to settle by hand.
func fileDriftKeys(record receipt.Receipt) (map[string]bool, error) {
	drifted := map[string]bool{}

	for _, artifact := range record.Artifacts {
		if artifact.Path == "" || artifact.Digest == "" {
			continue
		}

		if !claimsWholeFile(record, artifact.Path) {
			continue
		}

		data, err := os.ReadFile(artifact.Path) //nolint:gosec // G304: the path comes from a receipt
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", artifact.Path, err)
		}

		if digest.Bytes(data) != artifact.Digest {
			drifted[artifact.Path] = true
		}
	}

	return drifted, nil
}

// claimsWholeFile reports whether a receipt op asserts the bytes at this
// path, as opposed to claiming a value inside a document or a link.
func claimsWholeFile(record receipt.Receipt, path string) bool {
	for _, op := range record.RMA {
		if op.Path != path {
			continue
		}

		switch op.Kind {
		case receipt.OpWriteFile, receipt.OpCopyTree:
			return true
		case receipt.OpConfigKey, receipt.OpRecord, receipt.OpSymlink, receipt.OpHardlink, receipt.OpHostInstall:
			return false
		}
	}

	return false
}
