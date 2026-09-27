package apply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// lookupKeyPath walks a dotted key path through decoded maps.
func lookupKeyPath(doc any, keyPath string) (any, bool) {
	current := doc

	for segment := range strings.SplitSeq(keyPath, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}

		current, ok = object[segment]
		if !ok {
			return nil, false
		}
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

	if op.Existed {
		if op.Backup == "" {
			return fmt.Sprintf("%s#%s: hands-off (no backup recorded); left in place", op.Path, op.KeyPath), true, nil
		}

		return r.restoreConfigKey(op, data, current)
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
