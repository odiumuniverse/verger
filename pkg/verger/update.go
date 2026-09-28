package verger

import (
	"context"
	"errors"
	"fmt"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// UpdateOptions describe one re-apply run. Update is the reconcile a caller
// already knows will change something: the same plan as Sync, with the same
// refusals, exposed under the name DESIGN §9.1 gives it.
type UpdateOptions = SyncOptions

// Update re-applies the packages the spec declares: it is Sync, so a front end
// that wants "make it match" and one that wants "bring it up to date" cannot
// drift apart (DESIGN §9.3).
func (c *Client) Update(ctx context.Context, opts UpdateOptions) (*SyncPlan, *apply.Report, error) {
	return c.Sync(ctx, opts)
}

// Outdated returns the cells that no longer match their package: a skewed
// version, or a lock cell with no receipt (DESIGN §9.3 — beadle's
// `status --check`). It writes nothing.
func (c *Client) Outdated(ctx context.Context, opts StatusOptions) (StatusDocument, error) {
	opts.OutdatedOnly = true

	return c.Status(ctx, opts)
}

// PinOptions describe one pin change.
type PinOptions struct {
	// Paths is the scope whose spec carries the pin.
	Paths Paths
	// ID is the package to pin; a bare short name matches any owner.
	ID string
	// Version is the version to pin to; empty drops the pin.
	Version string
	// DryRun reports the change without writing it.
	DryRun bool
}

// PinResult is what one pin change did, as data.
type PinResult struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Pinned  bool   `json:"pinned"`
	Changed bool   `json:"changed"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// Pin stores or clears one package's version pin in the spec. An empty version
// unpins, and an id no entry carries is refused rather than silently created: a
// pin for a package verger does not know is a caller mistake.
func (c *Client) Pin(ctx context.Context, opts PinOptions) (PinResult, error) {
	if err := c.RequireTrust(opts.Paths); err != nil {
		return PinResult{}, err
	}

	doc, _, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return PinResult{}, err
	}

	result := PinResult{ID: opts.ID, Version: opts.Version, Pinned: opts.Version != "", DryRun: opts.DryRun}

	if _, err := SetSpecPin(doc, opts.ID, opts.Version); err != nil {
		return PinResult{}, err
	}

	if opts.DryRun {
		return result, nil
	}

	if err := c.Ensure(opts.Paths); err != nil {
		return PinResult{}, err
	}

	if err := SaveSpec(opts.Paths.SpecPath, doc); err != nil {
		return PinResult{}, err
	}

	result.Changed = true

	return result, nil
}

// Unpin drops one package's version pin.
func (c *Client) Unpin(ctx context.Context, opts PinOptions) (PinResult, error) {
	opts.Version = ""

	return c.Pin(ctx, opts)
}

// SetDisabled turns one package's delivery on or off in the spec (U2: every
// behaviour on by default and switchable off). A disabled package stays
// declared — a sync keeps it desired and leaves the machine alone.
func (c *Client) SetDisabled(ctx context.Context, paths Paths, id string, disabled, dryRun bool) error {
	if err := c.RequireTrust(paths); err != nil {
		return err
	}

	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	changed := false

	for i := range doc.Packages {
		if !matchesID(doc.Packages[i].ID, id) {
			continue
		}

		if doc.Packages[i].Disabled == disabled {
			continue
		}

		doc.Packages[i].Disabled = disabled

		changed = true
	}

	if !changed {
		return nil
	}

	if dryRun {
		return nil
	}

	if err := c.Ensure(paths); err != nil {
		return err
	}

	return SaveSpec(paths.SpecPath, doc)
}

// Disabled reports whether the spec declares one package disabled.
func (c *Client) Disabled(paths Paths, id string) (bool, bool) {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return false, false
	}

	for _, entry := range doc.Packages {
		if matchesID(entry.ID, id) {
			return entry.Disabled, true
		}
	}

	return false, false
}

// PinVocabulary is the status word a pin carries, so a second front end can
// render the same four answers `beadle plugins pins` does (GAP-17): ok, missing,
// no-effect and unknown-package are named here rather than re-invented.
type PinVocabulary string

// The four pin answers.
const (
	PinOK       PinVocabulary = "ok"
	PinMissing  PinVocabulary = "missing"         // the package is not installed
	PinNoEffect PinVocabulary = "no-effect"       // installed, but the pin changes nothing
	PinUnknown  PinVocabulary = "unknown-package" // no such package in the spec
)

// PinState is the state of one package's pin, as data.
type PinState struct {
	ID         string        `json:"id"`
	Version    string        `json:"version,omitempty"`
	Vocabulary PinVocabulary `json:"vocabulary"`
	Locked     bool          `json:"locked,omitempty"` // the receipt's version differs
}

// PinStates answers the pin question for every package in one scope: what the
// spec pins, what is installed, and the vocabulary a UI renders.
func (c *Client) PinStates(ctx context.Context, paths Paths) ([]PinState, error) {
	doc, ok, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return nil, err
	}

	if !ok {
		return []PinState{}, nil
	}

	receipts := receipt.NewStore(paths.ReceiptsDir)

	list, err := receipts.List()
	if err != nil {
		return nil, err
	}

	out := make([]PinState, 0, len(doc.Packages))

	for _, entry := range doc.Packages {
		state := PinState{ID: entry.ID, Version: entry.Version}

		record, found := findReceipt(list, entry.ID, "", string(paths.Scope))
		if !found {
			for _, candidate := range list {
				if matchesID(candidate.Package, entry.ID) && candidate.Scope == string(paths.Scope) {
					record, found = candidate, true

					break
				}
			}
		}

		switch {
		case !found:
			state.Vocabulary = PinMissing
		case entry.Version == "" || record.Version == entry.Version:
			state.Vocabulary = PinOK
		default:
			state.Vocabulary = PinNoEffect
			state.Locked = true
		}

		out = append(out, state)
	}

	return out, nil
}

// PinStateFor answers the pin question for one package id, in the same four
// words as PinStates: an id the spec does not declare is `unknown-package`
// rather than a silent absence (GAP-17).
func (c *Client) PinStateFor(ctx context.Context, paths Paths, id string) (PinState, error) {
	states, err := c.PinStates(ctx, paths)
	if err != nil {
		return PinState{}, err
	}

	for _, state := range states {
		if matchesID(state.ID, id) {
			return state, nil
		}
	}

	return PinState{ID: id, Vocabulary: PinUnknown}, nil
}

// PackageNotInSpec reports a spec operation on a package the spec does not
// declare. It is a caller mistake, not a state the tool can resolve.
type PackageNotInSpecError struct {
	ID string
}

// Error implements error.
func (e *PackageNotInSpecError) Error() string {
	return fmt.Sprintf("package %q is not in the spec", e.ID)
}

// ErrNoSpec reports a scope with no spec document at all.
var ErrNoSpec = errors.New("no spec")

// SpecPackage reports whether the spec declares one package, and how.
func SpecPackage(doc *spec.Spec, id string) (spec.Package, bool) {
	for _, entry := range doc.Packages {
		if matchesID(entry.ID, id) {
			return entry, true
		}
	}

	return spec.Package{}, false
}
