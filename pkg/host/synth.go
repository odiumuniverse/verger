package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/pack"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Owner marketplace layout (decision F3): <store>/synth/<owner> is one
// marketplace, its document lists every package of the owner as
// ./<name dir>/<version>.
const (
	ownerMarketplaceDoc  = ".claude-plugin/marketplace.json"
	ownerMarketplaceLock = ".marketplace.lock"
	collisionSuffix      = "-verger"
	lockRetryDelay       = 10 * time.Millisecond
)

// synthLayout is one synth package placed in its owner marketplace.
type synthLayout struct {
	identity pack.Identity // plugin name and default marketplace name
	root     string        // <store>/synth/<owner>: the marketplace root
	dir      string        // the package's name dir below root
	version  string        // the version dir below dir
}

// newSynthLayout checks that Package.SynthDir is <root>/<name>/<version> for
// the package id (store.SynthElements) and derives the plugin identity.
func newSynthLayout(host ID, pkg Package) (synthLayout, error) {
	if pkg.SynthDir == "" {
		return synthLayout{}, &NotSupportedError{Host: host, Operation: "synth install without a synth dir"}
	}

	identity, err := pack.IdentityOf(pkg.ID)
	if err != nil {
		return synthLayout{}, &NotSupportedError{Host: host, Operation: "synth install without an owner/name identity", Cause: err}
	}

	owner, name, err := store.SynthElements(pkg.ID)
	if err != nil {
		return synthLayout{}, &NotSupportedError{Host: host, Operation: "synth install without an owner/name identity", Cause: err}
	}

	dir, err := filepath.Abs(pkg.SynthDir)
	if err != nil {
		return synthLayout{}, &DeliveryError{Host: string(host), Package: pkg.ID, Step: stepPlan, Cause: err}
	}

	version := filepath.Base(dir)
	nameDir := filepath.Dir(dir)
	root := filepath.Dir(nameDir)

	if filepath.Base(nameDir) != name || filepath.Base(root) != owner {
		return synthLayout{}, &DeliveryError{
			Host: string(host), Package: pkg.ID, Step: stepPlan,
			Cause: fmt.Errorf("synth dir %s is not <store>/synth/%s/%s/<version>", dir, owner, name),
		}
	}

	return synthLayout{identity: identity, root: root, dir: name, version: version}, nil
}

// source is the package's entry source in the owner marketplace.
func (l synthLayout) source() string {
	return "./" + l.dir + "/" + l.version
}

// docPath is the owner marketplace document.
func (l synthLayout) docPath() string {
	return filepath.Join(l.root, filepath.FromSlash(ownerMarketplaceDoc))
}

// readDoc reads the owner marketplace document; ok is false when there is
// none yet. The document is verger's alone, so an unreadable one is reported
// rather than guessed at.
func (l synthLayout) readDoc() (string, []pack.MarketplaceEntry, bool, error) {
	data, err := os.ReadFile(l.docPath()) //nolint:gosec // G304: the owner marketplace below the store synth root
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, false, nil
	}

	if err != nil {
		return "", nil, false, err
	}

	name, entries, err := pack.ParseMarketplace(data)
	if err != nil {
		return "", nil, false, err
	}

	return name, entries, true, nil
}

// checkClaim refuses a plugin name another package of the owner already
// lists: the identity projection is not injective, and the owner
// marketplace never lets a second package overwrite the first.
func (l synthLayout) checkClaim(entries []pack.MarketplaceEntry) error {
	for _, entry := range entries {
		if entry.Name != l.identity.Name {
			continue
		}

		dir, _, _ := strings.Cut(strings.TrimPrefix(entry.Source, "./"), "/")
		if dir != l.dir && isDir(filepath.Join(l.root, dir)) {
			return fmt.Errorf("plugin %q of marketplace %s is already listed for %s", entry.Name, l.root, entry.Source)
		}
	}

	return nil
}

// writeDoc upserts the package into the owner marketplace document under an
// exclusive lock (hosts deliver in parallel) and sets the marketplace name:
// the document is a pure function of the owner's packages still present in
// the store (pack.RenderMarketplace), so an entry whose dir is gone is
// dropped. changed reports a write.
func (l synthLayout) writeDoc(ctx context.Context, name string) (bool, error) {
	if err := fsutil.EnsureDir(filepath.Dir(l.docPath()), 0o700); err != nil {
		return false, err
	}

	fileLock := flock.New(filepath.Join(l.root, ownerMarketplaceLock))

	if _, err := fileLock.TryLockContext(ctx, lockRetryDelay); err != nil {
		return false, fmt.Errorf("lock %s: %w", fileLock.Path(), err)
	}

	defer func() { _ = fileLock.Unlock() }()

	_, current, _, err := l.readDoc()
	if err != nil {
		return false, err
	}

	if err := l.checkClaim(current); err != nil {
		return false, err
	}

	entries := []pack.MarketplaceEntry{{Name: l.identity.Name, Source: l.source()}}

	for _, entry := range current {
		if entry.Name != l.identity.Name && isDir(filepath.Join(l.root, filepath.FromSlash(entry.Source))) {
			entries = append(entries, entry)
		}
	}

	data, err := pack.RenderMarketplace(name, entries)
	if err != nil {
		return false, err
	}

	previous, err := os.ReadFile(l.docPath()) //nolint:gosec // G304: the owner marketplace below the store synth root
	if err == nil && bytes.Equal(previous, data) {
		return false, nil
	}

	if err := fsutil.WriteFileAtomic(l.docPath(), data, 0o600); err != nil {
		return false, err
	}

	return true, nil
}

// registeredMarketplace is one marketplace a host reports.
type registeredMarketplace struct {
	Name string
	Path string
}

// chooseMarketplace picks the marketplace name of a synth delivery from what
// the host already has registered (decision F3): the preferred name when it
// is free or already points at the owner root, else `<owner>-verger`. ok is
// false when every candidate is a foreign marketplace.
func (l synthLayout) chooseMarketplace(preferred string, registered []registeredMarketplace) (string, bool, bool) {
	candidates := slices.Compact([]string{preferred, l.identity.Owner, l.identity.Owner + collisionSuffix})

	for _, name := range candidates {
		index := slices.IndexFunc(registered, func(m registeredMarketplace) bool { return m.Name == name })
		if index < 0 {
			return name, false, true
		}

		if samePath(registered[index].Path, l.root) {
			return name, true, true
		}
	}

	return "", false, false
}

// samePath compares two paths after resolving symlinks (/tmp vs /private/tmp).
func samePath(a, b string) bool {
	return resolvedPath(a) == resolvedPath(b)
}

// resolvedPath is the cleaned absolute path with the symlinks of its longest
// existing ancestor resolved, so a path that does not exist yet compares
// equal to its resolved parent (/var vs /private/var).
func resolvedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}

	for current, rest := abs, ""; ; current, rest = filepath.Dir(current), filepath.Join(filepath.Base(current), rest) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest)
		}

		if filepath.Dir(current) == current {
			return abs
		}
	}
}
