package verger

import (
	"context"
	"fmt"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// RemoveOptions carry what a removal needs beyond its key.
type RemoveOptions struct {
	// Paths is the scope whose receipts are scanned.
	Paths Paths
	// Hosts narrows the adapters; an empty list means every registered adapter.
	Hosts []host.Host
	// Cause is recorded on the receipt so a later restore knows why.
	Cause receipt.Cause
	// Confirm answers the executor's questions; nil declines them.
	Confirm Confirmer
	// DryRun plans and reports without writing.
	DryRun bool
}

// RemovalPlan is a Plan that removes instead of installs. The id is named
// because a removal is keyed by package, not by a fetch, and it is what the spec
// entry it drops is matched on.
type RemovalPlan struct {
	Plan

	// PackageID is the id the removal matched.
	PackageID string `json:"package"`
}

// PlanRemove resolves the receipts one package id owns and the actions that
// would reverse them. A host whose adapter this build does not have is left
// installed, with the reason in the notes, never silently dropped.
func (c *Client) PlanRemove(_ context.Context, id string, opts RemoveOptions) (*RemovalPlan, error) {
	receipts := receipt.NewStore(opts.Paths.ReceiptsDir)

	list, err := receipts.List()
	if err != nil {
		return nil, err
	}

	adapters := opts.Hosts
	if len(adapters) == 0 {
		adapters = c.Hosts()
	}

	adapterByID := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		adapterByID[adapter.ID()] = adapter
	}

	//nolint:modernize // a composite literal cannot spell an embedded field's promoted names
	plan := &RemovalPlan{Plan: Plan{Paths: opts.Paths, Adapters: adapters}, PackageID: id}

	for i := range list {
		record := list[i]

		if !matchesID(record.Package, id) {
			continue
		}

		hostID := host.ID(record.Host)

		if _, ok := adapterByID[hostID]; !ok {
			plan.Notes = append(plan.Notes,
				fmt.Sprintf("%s: adapter %s is not available; left installed", record.Package, record.Host))

			continue
		}

		plan.Actions = append(plan.Actions, apply.Action{
			Kind: apply.ActionRemove, Host: hostID, Previous: &record, Cause: string(opts.Cause),
		})
		plan.Cells = append(plan.Cells, Cell{
			Package: record.Package, Host: record.Host, Scope: record.Scope,
			Status: StatusPlanned, Version: record.Version, Strategy: record.Strategy, Kind: string(apply.ActionRemove),
		})
	}

	return plan, nil
}

// Remove executes a removal plan: it runs the host-install inverses through the
// executor and drops the spec entry, so a later restore finds the receipts the
// trash buckets belong to.
func (c *Client) Remove(ctx context.Context, plan *RemovalPlan, opts ApplyOptions) (*apply.Report, error) {
	if err := c.RequireTrust(plan.Paths); err != nil {
		return nil, err
	}

	if len(plan.Actions) == 0 {
		return &apply.Report{}, nil
	}

	if !opts.DryRun {
		if err := c.Ensure(plan.Paths); err != nil {
			return nil, err
		}

		if err := c.removeSpecRecord(plan.Paths, plan.PackageID); err != nil {
			return nil, err
		}
	}

	return c.Apply(ctx, &plan.Plan, opts)
}

// RestoredEntry is the stable JSON shape of one restored trash entry.
type RestoredEntry struct {
	Package  string `json:"package"`
	Host     string `json:"host"`
	Original string `json:"original"`
	TrashID  string `json:"trash_id"`
}

// RestoreResult is what a restore restored.
type RestoreResult struct {
	Restored []RestoredEntry `json:"restored"`
}

// Restore reverses a removal: it puts back every trash entry the package's
// removal replaced. It is a trash restore rather than an executor action
// because the removed artifacts live in the trash buckets their receipts
// named, and restoring them must not re-run a host installer.
func (c *Client) Restore(ctx context.Context, id string, opts RemoveOptions) (*RestoreResult, error) {
	entries, err := c.Store().Trash().List()
	if err != nil {
		return nil, err
	}

	result := &RestoreResult{}

	for _, entry := range entries {
		if !matchesID(entry.Package, id) {
			continue
		}

		if !opts.DryRun {
			if _, err := c.Store().Trash().Restore(ctx, entry.ID); err != nil {
				return nil, err
			}
		}

		result.Restored = append(result.Restored, RestoredEntry{
			Package: entry.Package, Host: entry.Host, Original: entry.Original, TrashID: entry.ID,
		})
	}

	return result, nil
}

// removeSpecRecord deletes every package entry with the id and saves the spec.
func (c *Client) removeSpecRecord(paths Paths, id string) error {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	if RemoveSpecPackage(doc, id) == 0 {
		return nil
	}

	return SaveSpec(paths.SpecPath, doc)
}

// recordInstall writes the fetched sources and packages into the spec (D3).
func (c *Client) recordInstall(paths Paths, packages []PlannedPackage) error {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	for _, item := range packages {
		AddSpecPackage(doc, item.Package.ID, item.Package.Version)
		AddSpecSource(doc, item.Ref)
	}

	return SaveSpec(paths.SpecPath, doc)
}

// SpecHooksMode maps the library hooks mode onto the spec's own vocabulary.
func SpecHooksMode(mode HooksMode) spec.HooksMode {
	switch mode {
	case HooksSkip:
		return spec.HooksNo
	case HooksYes:
		return spec.HooksYes
	default:
		return spec.HooksAsk
	}
}

// LibraryHooksMode maps a spec hooks mode onto the library's.
func LibraryHooksMode(mode spec.HooksMode) HooksMode {
	switch mode {
	case spec.HooksNo:
		return HooksSkip
	case spec.HooksYes:
		return HooksYes
	default:
		return HooksAsk
	}
}

// HooksModeFromSpec resolves the effective hooks mode for one scope: an explicit
// mode wins, else the spec default, else HooksAsk (D5: on by default).
func HooksModeFromSpec(paths Paths, mode HooksMode) (HooksMode, error) {
	if mode != HooksAsk {
		return mode, nil
	}

	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return HooksAsk, err
	}

	return LibraryHooksMode(doc.Defaults.Hooks), nil
}
