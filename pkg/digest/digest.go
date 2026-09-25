// Package digest provides deterministic SHA-256 digests for bytes, files and
// trees.
//
// Hashes are lowercase hex, 64 characters. File content is streamed, never
// wholly buffered. Tree digests are pure functions of the included regular
// files and their slash-separated relative paths, so they are stable across
// filesystems, processes and walk orders.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrInvalidHash reports a value that is not a lowercase hex SHA-256 digest.
// The sentence-case message follows decision T0.2 Q4.
//
//nolint:staticcheck // ST1005: the sentence-case message is mandated by decision T0.2 Q4.
var ErrInvalidHash = errors.New("invalid hash.")

// Hash is a lowercase hex SHA-256 digest (64 chars). The zero Hash is invalid.
type Hash string

// String returns the hex encoding of the digest.
func (h Hash) String() string {
	return string(h)
}

// Valid reports whether the digest is a well-formed lowercase hex SHA-256.
func (h Hash) Valid() bool {
	return validHash(string(h))
}

// MarshalText implements encoding.TextMarshaler. The zero and malformed
// digests are rejected with ErrInvalidHash (decision T0.2 Q3).
func (h Hash) MarshalText() ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidHash, string(h))
	}

	return []byte(h), nil
}

// UnmarshalText parses a hex digest; see Parse.
func (h *Hash) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}

	*h = parsed

	return nil
}

// Bytes returns the digest of data.
func Bytes(data []byte) Hash {
	sum := sha256.Sum256(data)

	return Hash(hex.EncodeToString(sum[:]))
}

// Reader returns the digest of everything r yields; IO failures are reported
// as *UnreadableError.
func Reader(r io.Reader) (Hash, error) {
	hash := sha256.New()

	if _, err := io.Copy(hash, r); err != nil {
		return "", &UnreadableError{Cause: err}
	}

	return Hash(hex.EncodeToString(hash.Sum(nil))), nil
}

// File returns the content digest of the file at path. A symlink resolving to
// a regular file is accepted; directories, special files, missing and
// unreadable paths are reported as *UnreadableError.
func File(path string) (Hash, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", &UnreadableError{Path: path, Cause: err}
	}

	if !info.Mode().IsRegular() {
		return "", &UnreadableError{Path: path, Cause: errors.New("not a regular file")}
	}

	f, err := os.Open(path) //nolint:gosec // G304: the caller chooses what to digest
	if err != nil {
		return "", &UnreadableError{Path: path, Cause: err}
	}

	defer func() { _ = f.Close() }()

	sum, err := Reader(f)
	if err != nil {
		return "", &UnreadableError{Path: path, Cause: err}
	}

	return sum, nil
}

// Parse validates a lowercase hex SHA-256 digest; anything else — wrong
// length, uppercase, non-hex, empty — is ErrInvalidHash wrapped with the
// offending value.
func Parse(text string) (Hash, error) {
	if !validHash(text) {
		return "", fmt.Errorf("%w: %q", ErrInvalidHash, text)
	}

	return Hash(text), nil
}

func validHash(text string) bool {
	if len(text) != sha256.Size*2 {
		return false
	}

	for index := range len(text) {
		if !isLowerHex(text[index]) {
			return false
		}
	}

	return true
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// UnreadableError reports a path or stream that could not be digested.
type UnreadableError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *UnreadableError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("unreadable: %v", e.Cause)
	}

	return fmt.Sprintf("unreadable %s: %v", e.Path, e.Cause)
}

// Unwrap reports the underlying cause, so errors.Is works with fs.ErrNotExist
// and friends.
func (e *UnreadableError) Unwrap() error {
	return e.Cause
}
