package verger

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

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
	// Filter narrows the detected adapters when Hosts is empty — the
	// `--hosts` / `--except` of the CLI, as data, exactly as on an install.
	// A host this build cannot deliver to is the same typed refusal an
	// install gives, so the CLI maps both to one exit code.
	Filter HostFilter
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

// removeAdapters resolves the hosts a removal targets.
//
// Two rules pull in opposite directions and both are right. A removal must keep
// the hosts the machine does not currently detect, because "I just uninstalled
// the agent, and the file it left is still in my spec" is the case that most
// needs a removal. And it must honour the filter, because a filter that only
// ever reaches the detected set is a flag that prints a narrowed plan and then
// does something else.
//
// So a caller-supplied list is narrowed in place, and only an absent list falls
// back to the detected set, where an unreachable `--hosts` id is the same typed
// refusal an install gives.
func (c *Client) removeAdapters(opts RemoveOptions) ([]host.Host, error) {
	if len(opts.Hosts) == 0 {
		return c.Targets(opts.Filter)
	}

	return c.selectAdapters(opts.Hosts, opts.Filter, func(host.Host) bool { return true })
}

// excludedNote explains why one receipt keeps its files, and it has to tell the
// truth about which of the two reasons applied.
//
// "adapter X is not available" is the right sentence for a host this build
// cannot drive, and a lie for a host the user excluded on purpose: the same
// note then tells them their working installation is unavailable, which is the
// one thing they do not need to hear after asking for exactly this. The flag
// is named, so the sentence says how to change it back.
func excludedNote(pkg, hostID string, filter HostFilter, registered []host.Host) string {
	only, err := hostSet(filter.Only, "--hosts")
	if err != nil {
		only = nil
	}

	except, err := hostSet(filter.Except, "--except")
	if err != nil {
		except = nil
	}

	excluded := len(only) > 0 && !only[hostID]

	if except[hostID] {
		excluded = true
	}

	// A host outside the caller's own list was never a candidate, whatever the
	// flags say: that is the unavailable case, not the filtered one.
	if !excluded {
		return fmt.Sprintf("%s: adapter %s is not available; left installed", pkg, hostID)
	}

	if !AdapterIDs(registered)[hostID] {
		return fmt.Sprintf("%s: adapter %s is not available; left installed", pkg, hostID)
	}

	flag := "--hosts"
	if except[hostID] {
		flag = "--except"
	}

	return fmt.Sprintf("%s: %s was excluded by %s; left installed", pkg, hostID, flag)
}

// PlanRemove resolves the receipts one package id owns and the actions that
// would reverse them. A host whose adapter this build does not have is left
// installed, with the reason in the notes, never silently dropped.
func (c *Client) PlanRemove(ctx context.Context, id string, opts RemoveOptions) (*RemovalPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	receipts := receipt.NewStore(opts.Paths.ReceiptsDir)

	list, err := receipts.List()
	if err != nil {
		return nil, err
	}

	adapters, err := c.removeAdapters(opts)
	if err != nil {
		return nil, err
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
			plan.Notes = append(plan.Notes, excludedNote(record.Package, record.Host, opts.Filter, c.Hosts()))

			continue
		}

		// A path four hosts share is one file, and the plan may delete it only
		// when the LAST of them lets go. Removing one host while three still
		// reference the file would delete it out from under them, so those ops
		// are pruned and the plan says which host keeps the file alive.
		//
		// The receipt still goes. A host that held nothing but the shared file
		// has, after pruning, nothing left to delete — and skipping its action
		// would leave its receipt behind forever, so the reference count would
		// never reach zero and no later removal could ever take the file.
		held := len(record.Artifacts) > 0

		plan.Notes = append(plan.Notes,
			pruneSharedArtifacts(&record, pathsSharedWithRemainingHosts(list, id, adapterByID))...)

		if !held {
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

	// The shared files this removal empties go with it, decided here rather
	// than left to whichever receipt happened to carry the writing host's op.
	releaseSharedFiles(plan, sharedFilesReleased(list, id, adapterByID))

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

	specDir := filepath.Dir(paths.SpecPath)

	for _, item := range packages {
		AddSpecPackage(doc, spec.Package{ID: item.Package.ID, Version: item.Package.Version})
		AddSpecSourceAt(doc, item.Ref, specDir)
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

// pathsSharedWithRemainingHosts is the set of artifact paths this removal may
// NOT delete, because a receipt for a host that is staying still references
// them.
//
// A path several hosts share is one physical file with one owner of record, and
// the reference count is simply "how many receipts still name it". Deleting on
// any count above one would pull the file out from under the hosts that are
// staying, which is the one failure a refcount exists to prevent.
func pathsSharedWithRemainingHosts(
	list []receipt.Receipt, id string, adapterByID map[host.ID]host.Host,
) map[string]string {
	removing := map[string]bool{}

	for _, record := range list {
		if !matchesID(record.Package, id) {
			continue
		}

		if _, ok := adapterByID[host.ID(record.Host)]; ok {
			removing[record.Host] = true
		}
	}

	shared := map[string]string{}

	for _, record := range list {
		if !matchesID(record.Package, id) || removing[record.Host] {
			continue
		}

		for _, artifact := range record.Artifacts {
			shared[artifact.Path] = record.Host
		}
	}

	return shared
}

// pruneSharedArtifacts drops the artifacts and RMA ops of one receipt that a
// remaining host still needs, and reports each one in the plan's own words: a
// silent skip reads as a removal that forgot something.
func pruneSharedArtifacts(record *receipt.Receipt, shared map[string]string) []string {
	if len(shared) == 0 {
		return nil
	}

	keepArtifacts := make([]receipt.Artifact, 0, len(record.Artifacts))
	droppedOps := map[string]bool{}
	notes := make([]string, 0, len(record.Artifacts))

	for _, artifact := range record.Artifacts {
		if keeper, sharedPath := shared[artifact.Path]; sharedPath {
			droppedOps[artifact.Path] = true
			notes = append(notes, fmt.Sprintf(
				"%s: %s is also %s's; kept on disk until the last host releases it",
				record.Package, artifact.Path, keeper))

			continue
		}

		keepArtifacts = append(keepArtifacts, artifact)
	}

	if len(keepArtifacts) == len(record.Artifacts) {
		return nil
	}

	record.Artifacts = keepArtifacts
	record.RMA = slices.DeleteFunc(record.RMA, func(op receipt.Op) bool {
		return droppedOps[op.Path]
	})

	return notes
}

// sharedFilesReleased are the shared paths whose LAST reference this removal
// gives up: a host being removed names them, no host staying names them, and
// no receipt being removed still carries the op that would delete them.
//
// That last clause is what keeps this to shared files. A host's own path is
// deleted by the op in its own receipt, and inventing a second one for it would
// delete a file the writer's receipt deliberately left in place. A shared file
// has the opposite problem: the host that wrote it kept the only op, that op was
// pruned while a reader still referenced the path, and once the writer is gone
// nothing anywhere carries it — so the file survives every later removal with
// no way left to remove it. Deciding here, from the live receipts, is what makes
// the rule independent of the order hosts were removed in.
func sharedFilesReleased(list []receipt.Receipt, id string, adapterByID map[host.ID]host.Host) map[string]receipt.Artifact {
	keepers := pathsSharedWithRemainingHosts(list, id, adapterByID)

	owned := map[string]bool{}
	released := map[string]receipt.Artifact{}

	for _, record := range list {
		if !matchesID(record.Package, id) {
			continue
		}

		if _, ok := adapterByID[host.ID(record.Host)]; !ok {
			continue
		}

		for _, op := range record.RMA {
			owned[op.Path] = true
		}

		for _, artifact := range record.Artifacts {
			if _, kept := keepers[artifact.Path]; kept {
				continue
			}

			released[artifact.Path] = artifact
		}
	}

	for path := range released {
		if owned[path] {
			delete(released, path)
		}
	}

	return released
}

// releaseSharedFiles attaches the deletion of each released shared file to the
// LAST action of the removal, so every per-host removal has already run by the
// time the one file they share goes.
//
// The op is built from the artifact rather than copied out of a receipt: by the
// time the count reaches zero, the writing host's receipt may be long gone.
func releaseSharedFiles(plan *RemovalPlan, released map[string]receipt.Artifact) {
	if len(released) == 0 || len(plan.Actions) == 0 {
		return
	}

	paths := make([]string, 0, len(released))
	for path := range released {
		paths = append(paths, path)
	}

	slices.Sort(paths)

	action := &plan.Actions[len(plan.Actions)-1]
	if action.Previous == nil {
		return
	}

	for _, path := range paths {
		artifact := released[path]

		// The writer recorded Existed=false: verger created the file, so a
		// removal takes it away instead of handing back a backup of nothing.
		action.Previous.RMA = append(action.Previous.RMA, receipt.Op{
			Kind: receipt.OpCopyTree, Path: path, Digest: artifact.Digest, Mode: 0o700,
		})
	}
}
