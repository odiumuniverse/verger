package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/store"
)

// entryFile is the completion marker of one cache entry.
const entryFile = "entry.json"

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithRunner replaces the host CLI runner used for git and npm.
func WithRunner(r hostcli.Runner) Option {
	return func(f *Fetcher) {
		f.runner = r
	}
}

// WithCacheDir replaces the source cache root (default: store cache/source).
func WithCacheDir(dir string) Option {
	return func(f *Fetcher) {
		f.cacheDir = dir
	}
}

// WithStore derives the cache root from the store's cache directory.
func WithStore(st *store.Store) Option {
	return func(f *Fetcher) {
		f.store = st
	}
}

// WithClock replaces the clock used for cache metadata.
func WithClock(now func() time.Time) Option {
	return func(f *Fetcher) {
		f.now = now
	}
}

// Fetcher fetches references into the source cache. It is safe for concurrent
// use: fetches of the same reference serialize per instance, and cache entries
// appear atomically.
type Fetcher struct {
	runner   hostcli.Runner
	resolve  func(name string) (hostcli.Binary, error)
	cacheDir string
	store    *store.Store
	now      func() time.Time

	locks sync.Map // cache key -> *sync.Mutex
}

// NewFetcher builds a fetcher over the store cache (or an explicit cache dir).
func NewFetcher(opts ...Option) (*Fetcher, error) {
	f := &Fetcher{now: time.Now}

	for _, opt := range opts {
		opt(f)
	}

	if f.runner == nil {
		f.runner = hostcli.ExecRunner{}
	}

	if f.resolve == nil {
		f.resolve = hostcli.NewResolver().Resolve
	}

	if f.cacheDir == "" {
		st := f.store

		if st == nil {
			resolved, err := store.Open("")
			if err != nil {
				return nil, fmt.Errorf("resolve store for source cache: %w", err)
			}

			st = resolved
		}

		f.cacheDir = filepath.Join(st.CacheDir(), "source")
	}

	return f, nil
}

// Fetched is one fetched payload.
type Fetched struct {
	Ref           Ref
	Root          string // package payload root inside the cache
	Commit        string // git commit sha (github/git)
	TreeDigest    digest.Hash
	ArchiveSHA256 digest.Hash
	Package       *manifest.Package
	Warnings      []string
	// Cleanup releases any temporary resources; safe to call once, never
	// removes the cache entry.
	Cleanup func() error
}

// entryMeta is the completion marker of one cache entry.
type entryMeta struct {
	Kind          Kind        `json:"kind"`
	Root          string      `json:"root"` // relative to the entry directory
	Commit        string      `json:"commit,omitempty"`
	TreeDigest    digest.Hash `json:"tree_digest"`
	ArchiveSHA256 digest.Hash `json:"archive_sha256,omitempty"`
	FetchedAt     time.Time   `json:"fetched_at,omitzero"`
	Warnings      []string    `json:"warnings,omitempty"`
}

// Fetch fetches ref into the cache and parses its manifest. A complete cache
// entry is reused; a failed fetch leaves no entry behind.
func (f *Fetcher) Fetch(ctx context.Context, ref Ref) (*Fetched, error) {
	if err := ctx.Err(); err != nil {
		return nil, fetchError(ref, StepContext, err)
	}

	switch ref.Kind {
	case KindLocal:
		return f.fetchLocal(ctx, ref)
	case KindGitHub, KindGit:
		return f.fetchCached(ctx, ref, f.fetchGit)
	case KindURL:
		return f.fetchCached(ctx, ref, f.fetchURL)
	case KindNPM:
		return f.fetchCached(ctx, ref, f.fetchNPM)
	case KindMCP, KindAgent:
		return nil, &NotSupportedError{Kind: ref.Kind}
	default:
		return nil, &RefError{Input: ref.Raw, Reason: fmt.Sprintf("unsupported kind %q", ref.Kind)}
	}
}

// buildFunc fills a temporary entry directory for one reference.
type buildFunc func(ctx context.Context, ref Ref, tmp string) (*entryMeta, error)

// fetchCached runs one build through the per-key lock and the cache.
func (f *Fetcher) fetchCached(ctx context.Context, ref Ref, build buildFunc) (*Fetched, error) {
	key := string(cacheKey(ref))
	entry := filepath.Join(f.cacheDir, key)

	unlock := f.lockKey(key)
	defer unlock()

	if got, ok, err := f.loadEntry(ref, entry); err != nil {
		return nil, err
	} else if ok {
		return got, nil
	}

	if err := fsutil.EnsureDir(f.cacheDir, 0o700); err != nil {
		return nil, fetchError(ref, StepCache, err)
	}

	tmp, err := os.MkdirTemp(f.cacheDir, "tmp-")
	if err != nil {
		return nil, fetchError(ref, StepCache, err)
	}

	cleanup := func() error {
		if err := os.RemoveAll(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		return nil
	}

	meta, err := build(ctx, ref, tmp)
	if err != nil {
		_ = cleanup()

		return nil, err
	}

	meta.FetchedAt = f.now().UTC()

	if err := writeEntryMeta(tmp, meta); err != nil {
		_ = cleanup()

		return nil, fetchError(ref, StepCache, err)
	}

	if err := os.Rename(tmp, entry); err != nil {
		_ = cleanup()

		// Another process may have completed the same entry; adopt it.
		if got, ok, loadErr := f.loadEntry(ref, entry); loadErr == nil && ok {
			return got, nil
		}

		return nil, fetchError(ref, StepCache, err)
	}

	return f.finishFetched(ref, filepath.Join(entry, meta.Root), meta, cleanup)
}

// loadEntry reads a complete cache entry; an unreadable or incomplete entry is
// removed and reported as a miss so the fetch self-heals.
func (f *Fetcher) loadEntry(ref Ref, entry string) (*Fetched, bool, error) {
	data, err := os.ReadFile(filepath.Join(entry, entryFile)) //nolint:gosec // G304: the entry path is derived from the ref key
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fetchError(ref, StepCache, err)
	}

	meta := entryMeta{}
	if err := json.Unmarshal(data, &meta); err != nil {
		_ = os.RemoveAll(entry)

		return nil, false, nil //nolint:nilerr // a corrupt entry is dropped and refetched
	}

	root := filepath.Join(entry, meta.Root)

	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		_ = os.RemoveAll(entry)

		return nil, false, nil //nolint:nilerr // an incomplete entry is dropped and refetched
	}

	got, err := f.finishFetched(ref, root, &meta, func() error { return nil })
	if err != nil {
		return nil, false, err
	}

	return got, true, nil
}

// finishFetched parses the manifest and assembles the result value.
func (f *Fetcher) finishFetched(ref Ref, root string, meta *entryMeta, cleanup func() error) (*Fetched, error) {
	pkg, warnings := parseManifest(root, ref)

	all := append([]string(nil), meta.Warnings...)
	all = append(all, warnings...)

	return &Fetched{
		Ref:           ref,
		Root:          root,
		Commit:        meta.Commit,
		TreeDigest:    meta.TreeDigest,
		ArchiveSHA256: meta.ArchiveSHA256,
		Package:       pkg,
		Warnings:      all,
		Cleanup:       cleanup,
	}, nil
}

// parseManifest parses the payload best-effort: a payload without a manifest is
// valid for skill/loose sources and yields a package with warnings.
func parseManifest(root string, ref Ref) (*manifest.Package, []string) {
	pkg, err := manifest.ParseAny(root)
	if err == nil {
		return pkg, nil
	}

	warning := err.Error()

	return &manifest.Package{
		ID:       ref.ID,
		Root:     root,
		Warnings: []string{warning},
	}, []string{warning}
}

// writeEntryMeta writes the completion marker inside tmp.
func writeEntryMeta(tmp string, meta *entryMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode source entry: %w", err)
	}

	if err := fsutil.WriteFileAtomic(filepath.Join(tmp, entryFile), append(data, '\n'), 0o600); err != nil {
		return err
	}

	return nil
}

// cacheKey hashes the canonical identity of a ref (subpath and rev included,
// raw spelling excluded).
func cacheKey(ref Ref) digest.Hash {
	wire := struct {
		Kind     Kind        `json:"kind"`
		ID       string      `json:"id,omitempty"`
		Owner    string      `json:"owner,omitempty"`
		Repo     string      `json:"repo,omitempty"`
		Subpath  string      `json:"subpath,omitempty"`
		Rev      string      `json:"rev,omitempty"`
		URL      string      `json:"url,omitempty"`
		SHA256   digest.Hash `json:"sha256,omitempty"`
		NPM      string      `json:"npm,omitempty"`
		Path     string      `json:"path,omitempty"`
		Agent    string      `json:"agent,omitempty"`
		AgentRef string      `json:"agent_ref,omitempty"`
		MCP      string      `json:"mcp,omitempty"`
	}{
		Kind: ref.Kind, ID: ref.ID, Owner: ref.Owner, Repo: ref.Repo, Subpath: ref.Subpath,
		Rev: ref.Rev, URL: ref.URL, SHA256: ref.SHA256, NPM: ref.NPM, Path: ref.Path,
		Agent: ref.Agent, AgentRef: ref.AgentRef, MCP: ref.MCP,
	}

	data, err := json.Marshal(wire)
	if err != nil {
		return digest.Bytes([]byte(ref.String()))
	}

	return digest.Bytes(data)
}

// lockKey serializes fetches of one key inside this instance.
func (f *Fetcher) lockKey(key string) func() {
	value, _ := f.locks.LoadOrStore(key, &sync.Mutex{})

	mu, ok := value.(*sync.Mutex)
	if !ok {
		return func() {}
	}

	mu.Lock()

	return mu.Unlock
}

// tool resolves git/npm, reporting a typed error when it is missing.
func (f *Fetcher) tool(name string) (hostcli.Binary, error) {
	bin, err := f.resolve(name)
	if err != nil {
		return hostcli.Binary{}, &ToolMissingError{Tool: name, Cause: err}
	}

	return bin, nil
}

// toolError classifies a runner failure: authentication problems get their own
// step so callers can map them to a needs-auth status.
func (f *Fetcher) toolError(ref Ref, step Step, err error) error {
	if isAuthError(err) {
		return fetchError(ref, StepAuth, err)
	}

	return fetchError(ref, step, err)
}

// isAuthError reports whether a runner failure looks like an authentication
// rejection.
func isAuthError(err error) bool {
	text := strings.ToLower(err.Error())

	for _, marker := range []string{
		"authentication failed",
		"could not read username",
		"could not read password",
		"invalid username or password",
		"permission denied (publickey)",
		"terminal prompts disabled",
		"http 401",
		"http 403",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}

	return false
}

// fetchError wraps one fetch step with the (sanitized) ref.
func fetchError(ref Ref, step Step, cause error) error {
	return &FetchError{Kind: ref.Kind, Ref: ref.String(), Step: step, Cause: cause}
}

// PinMismatchError reports an archive whose bytes do not match the pin.
type PinMismatchError struct {
	Want digest.Hash
	Got  digest.Hash
}

// Error implements error.
func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("archive sha256 mismatch: want %s, got %s", e.Want, e.Got)
}

// Step names one phase of a fetch. FetchError.Step is one of the exported
// StepXxx constants; the set is closed.
type Step string

// Fetch steps reported through FetchError.
const (
	StepContext  Step = "context"
	StepCache    Step = "cache"
	StepLocal    Step = "local"
	StepDownload Step = "download"
	StepExtract  Step = "extract"
	StepClone    Step = "clone"
	StepCheckout Step = "checkout"
	StepRevParse Step = "rev-parse"
	StepSubpath  Step = "subpath"
	StepCleanup  Step = "cleanup"
	StepNPMPack  Step = "npm-pack"
	StepDigest   Step = "digest"
	StepAuth     Step = "auth"
)

// FetchError reports a failed clone, download or extraction step. Its message
// is sanitized; Unwrap keeps the original cause.
type FetchError struct {
	Kind  Kind
	Ref   string
	Step  Step
	Cause error
}

// Error implements error.
func (e *FetchError) Error() string {
	return fmt.Sprintf("fetch %s %s: %s: %s", e.Kind, e.Ref, e.Step, sanitizeText(e.Cause.Error()))
}

// Unwrap returns the underlying cause.
func (e *FetchError) Unwrap() error {
	return e.Cause
}

// ToolMissingError reports an unresolvable git or npm binary.
type ToolMissingError struct {
	Tool  string
	Cause error
}

// Error implements error.
func (e *ToolMissingError) Error() string {
	return fmt.Sprintf("%s is not available: %v", e.Tool, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *ToolMissingError) Unwrap() error {
	return e.Cause
}

// NotSupportedError reports a kind that Ф1 parses but cannot fetch yet.
type NotSupportedError struct {
	Kind Kind
}

// Error implements error.
func (e *NotSupportedError) Error() string {
	return fmt.Sprintf("%s references are not supported yet", e.Kind)
}
