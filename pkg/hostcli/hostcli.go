// Package hostcli resolves and runs the command-line tools of agent hosts
// (claude, codex, gemini, ...) from any process, including one a service
// manager starts without the user's shell PATH. An attended run records where
// it found each CLI; a later unattended run falls back to that record after
// checking it still holds the same executable. Every Ф1 package talks to hosts
// through Runner, so tests inject ScriptRunner instead of a real binary.
package hostcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// ErrNotFound reports that no usable binary of a host CLI is reachable: it is
// neither on the process PATH nor at a valid recorded location, or it vanished
// before it ran. It means "cannot verify", never a failure of the host.
var ErrNotFound = errors.New("host CLI not found")

// ExitError reports a host CLI that ran and exited non-zero: the host itself
// rejected the call. Stderr carries the child's stderr verbatim, padding
// included.
type ExitError struct {
	Name   string
	Code   int
	Stderr string
	Err    error
}

// Error implements error.
func (e *ExitError) Error() string {
	return fmt.Sprintf("%s: %v: %s", e.Name, e.Err, e.Stderr)
}

// Unwrap returns the underlying process error.
func (e *ExitError) Unwrap() error {
	return e.Err
}

// RecordsParseError reports a malformed host CLI records file.
type RecordsParseError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *RecordsParseError) Error() string {
	return fmt.Sprintf("parse host CLI records %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *RecordsParseError) Unwrap() error {
	return e.Cause
}

// Source tells how a binary was resolved.
type Source string

// Resolution sources of a Binary.
const (
	// SourceLookPath is a binary found on the process PATH.
	SourceLookPath Source = "lookpath"
	// SourceRecorded is a binary found at a location an attended run recorded.
	SourceRecorded Source = "recorded"
)

// Record is a location an attended run resolved: the binary's absolute path
// (as found on PATH, not the symlink target, which native installers re-point
// on upgrade) and the PATH of that run, which the binary's own children (node,
// git) need when a service runs it.
type Record struct {
	Path string    `json:"path"`
	PATH string    `json:"path_env"`
	At   time.Time `json:"at"`
}

// Records maps a CLI name to its recorded location.
type Records map[string]Record

// LoadRecords reads the records file; a missing file is no records. A file
// that is not a JSON object — including a JSON null, which decodes to a nil
// map — is a *RecordsParseError.
func LoadRecords(path string) (Records, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the records file path is resolved by the caller's home
	if errors.Is(err, fs.ErrNotExist) {
		return Records{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read host CLI records %s: %w", path, err)
	}

	records := Records{}

	if err := json.Unmarshal(data, &records); err != nil {
		return nil, &RecordsParseError{Path: path, Cause: err}
	}

	if records == nil {
		return nil, &RecordsParseError{Path: path, Cause: errors.New("the records document is not a JSON object")}
	}

	return records, nil
}

// Save writes the records file through fsutil.WriteFileAtomic with 0600 and
// creates the parent directory with 0700. A nil Records is saved as an empty
// object, so a nil save round-trips through LoadRecords as empty.
func (r Records) Save(path string) error {
	if r == nil {
		r = Records{}
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode host CLI records: %w", err)
	}

	dir := filepath.Dir(path)
	if err := fsutil.EnsureDir(dir, 0o700); err != nil {
		return fmt.Errorf("write host CLI records %s: %w", path, err)
	}

	if err := fsutil.WriteFileAtomicCAS(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write host CLI records %s: %w", path, err)
	}

	return nil
}

// Binary is one resolved host CLI.
type Binary struct {
	Name   string
	Path   string
	Source Source
	// PATH is the search path the binary runs with; empty keeps the process
	// environment.
	PATH string
}

// Resolver finds host CLIs: the process PATH first, then the recorded
// locations, each of which must still pass Check.
type Resolver struct {
	lookPath func(string) (string, error)
	records  Records
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithLookPath replaces the PATH lookup (exec.LookPath by default).
func WithLookPath(lookPath func(string) (string, error)) Option {
	return func(r *Resolver) { r.lookPath = lookPath }
}

// WithRecords sets the recorded locations to fall back to.
func WithRecords(records Records) Option {
	return func(r *Resolver) { r.records = records }
}

// NewResolver builds a resolver.
func NewResolver(opts ...Option) *Resolver {
	r := &Resolver{lookPath: exec.LookPath, records: Records{}}

	for _, opt := range opts {
		opt(r)
	}

	if r.lookPath == nil {
		r.lookPath = exec.LookPath
	}

	return r
}

// Resolve returns the binary of a host CLI, or an error wrapping ErrNotFound
// that says why neither the PATH nor the record reaches it.
func (r *Resolver) Resolve(name string) (Binary, error) {
	path, lookErr := r.lookPath(name)
	if lookErr == nil {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}

		return Binary{Name: name, Path: path, Source: SourceLookPath}, nil
	}

	record, ok := r.records[name]
	if !ok {
		return Binary{}, fmt.Errorf("%s: %w on PATH, and no run recorded it", name, ErrNotFound)
	}

	if err := Check(name, record); err != nil {
		return Binary{}, fmt.Errorf("%s: %w on PATH, and the recorded %s is unusable: %w", name, ErrNotFound, record.Path, err)
	}

	return Binary{Name: name, Path: record.Path, Source: SourceRecorded, PATH: record.PATH}, nil
}

// Check verifies that a recorded location still holds the named executable:
// an absolute clean path whose base name is the CLI's, a regular executable
// file (symlinks followed) that other users cannot write, and, for an env
// shebang script, an interpreter reachable on the recorded PATH. The check is
// point-in-time: the record was written by an attended run the user trusted,
// and a later rename or looser directory permissions are not re-validated.
func Check(name string, record Record) error {
	path := record.Path

	switch {
	case !filepath.IsAbs(path) || filepath.Clean(path) != path:
		return fmt.Errorf("%q is not an absolute clean path", path)
	case filepath.Base(path) != name:
		return fmt.Errorf("%q is not a %s binary", path, name)
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	switch mode := info.Mode(); {
	case !mode.IsRegular():
		return fmt.Errorf("%q is not a regular file", path)
	case mode.Perm()&0o111 == 0:
		return fmt.Errorf("%q is not executable", path)
	case mode.Perm()&0o002 != 0:
		return fmt.Errorf("%q is writable by other users", path)
	}

	interpreter, err := envInterpreter(path)
	if err != nil || interpreter == "" {
		return err
	}

	if _, found := lookPathIn(interpreter, record.PATH); !found {
		return fmt.Errorf("%q runs %s, which the recorded PATH does not reach", path, interpreter)
	}

	return nil
}

// envInterpreter returns the program an `#!/usr/bin/env X` script runs, or ""
// for a binary or a script with an absolute interpreter.
func envInterpreter(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the recorded CLI path passed Check above
	if err != nil {
		return "", err
	}

	defer func() { _ = file.Close() }()

	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && line == "" {
		return "", nil //nolint:nilerr // an empty file has no shebang to check
	}

	rest, ok := strings.CutPrefix(line, "#!")
	if !ok {
		return "", nil
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 || filepath.Base(fields[0]) != "env" {
		return "", nil
	}

	for _, field := range fields[1:] {
		if strings.HasPrefix(field, "-") || strings.Contains(field, "=") {
			continue
		}

		return field, nil
	}

	return "", nil
}

// lookPathIn finds an executable on an explicit PATH value; relative and empty
// entries are skipped, exactly as a shell would not trust them for a service.
func lookPathIn(name, path string) (string, bool) {
	for dir := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}

		candidate := filepath.Join(dir, name)

		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}

	return "", false
}
