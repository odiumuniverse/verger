package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileKeyring is the Keyring that keeps values in a directory instead of a
// platform daemon. It exists for the machines that have no keyring to talk
// to — a CI runner, a headless Linux box, a container — where refusing to
// store a secret means the whole feature is unavailable exactly where it is
// most needed. It is the fallback, never the default: where a real keyring
// works, it wins, because a file is only as private as the account running
// verger.
//
// Values are written 0600 in a 0700 directory. That is not decoration: on a
// shared build box a world-readable secret file is a published secret, and
// the file backend is chosen precisely where such boxes are the norm.
type FileKeyring struct {
	dir string
}

// NewFileKeyring returns a file-backed keyring rooted at dir, creating it if
// needed. An empty dir is refused rather than defaulted: silently writing
// secrets somewhere the caller did not name is how a store ends up in the
// working directory of a build.
func NewFileKeyring(dir string) (*FileKeyring, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: the file keyring needs a directory", ErrKeyringUnsupported)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", dir, err)
	}

	return &FileKeyring{dir: dir}, nil
}

// Dir returns the directory the keyring keeps its values in.
func (k *FileKeyring) Dir() string { return k.dir }

// Path returns the file one account is stored in. It is exported because a
// person debugging a store needs to look at it, and a name that could climb
// out of the store is rejected rather than resolved.
func (k *FileKeyring) Path(account string) (string, error) {
	return k.resolve(account)
}

// Get implements Keyring.
func (k *FileKeyring) Get(account string) (string, bool, error) {
	path, err := k.resolve(account)
	if err != nil {
		return "", false, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is resolved inside the store's own directory
	if os.IsNotExist(err) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", account, err)
	}

	return string(data), true, nil
}

// Set implements Keyring.
func (k *FileKeyring) Set(account, value string) error {
	path, err := k.resolve(account)
	if err != nil {
		return err
	}

	if err := writeSecretFile(path, []byte(value)); err != nil {
		return err
	}

	return nil
}

// Delete implements Keyring. It reports whether anything was there, so a
// caller can tell "removed" from "there was nothing", which are different
// answers to `verger secret rm`.
func (k *FileKeyring) Delete(account string) (bool, error) {
	path, err := k.resolve(account)
	if err != nil {
		return false, err
	}

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, fmt.Errorf("remove %s: %w", account, err)
	}

	return true, nil
}

// resolve maps an account name to a path inside the store. A name with a
// separator, a parent reference or a NUL is refused: the account is a name,
// and a name that resolves to a path is a way to write outside the store.
func (k *FileKeyring) resolve(account string) (string, error) {
	if account == "" {
		return "", fmt.Errorf("%w: empty account name", ErrKeyringUnsupported)
	}

	if strings.ContainsAny(account, `/\`) || account == "." || account == ".." ||
		strings.ContainsRune(account, 0) {
		return "", fmt.Errorf("%w: account %q is not a name", ErrKeyringUnsupported, account)
	}

	return filepath.Join(k.dir, account), nil
}

// writeSecretFile writes value at path with owner-only permissions, creating
// the parent directory the same way. It refuses to widen an existing file:
// overwriting a 0600 file must not turn it into something a group can read.
func writeSecretFile(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare %s: %w", dir, err)
	}

	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: the path is resolved inside the store's own directory
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := handle.Write(value); err != nil {
		_ = handle.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := handle.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	// O_CREATE honours the umask but not a pre-existing wider mode, and a
	// secret file left group-readable after a rewrite is a leaked secret.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
