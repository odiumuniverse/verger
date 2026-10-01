package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/store"
)

// intentExtraKey names the journal Extra field that carries the crash-replay
// intent of one action.
const intentExtraKey = "intent"

// intentRecord is the journaled pre-write plan of one action: what will be
// written and how to reverse it. A restart uses it to forward-commit a
// completed install or roll back a partial one without touching the adapter
// again.
type intentRecord struct {
	Action    Action             `json:"action"`
	Artifacts []receipt.Artifact `json:"artifacts"`
	RMA       []receipt.Op       `json:"rma"`
}

// Test seams: production leaves them nil. They exist for the crash-replay
// tests, the accepted package-var exception (pkg/store/trash.go precedent).
var (
	// crashAfterInstall aborts the process between the install phase and the
	// receipt commit.
	crashAfterInstall func()
	// crashAfterReceipt aborts the process after the receipt commit and before
	// the lock commit.
	crashAfterReceipt func()
)

// Event step names.
const (
	stepPlan     = "plan"
	stepInstall  = "install"
	stepVerify   = "verify"
	stepRemove   = "remove"
	stepRollback = "rollback"
	stepReceipt  = "receipt"
	stepLock     = "lock"
)

// Removal causes applied when the plan leaves the cause empty.
const defaultCause = string(receipt.CauseUser)

// Trash causes apply records for its own operations.
const (
	causeConflict = "conflict"
	causeUpdate   = "update"
	causeRollback = "rollback"
	causeRecovery = "recovery"
)

// maxConflictAttempts bounds the trash-and-retry loop of one confirmed
// collision.
const maxConflictAttempts = 8

// cellKey identifies one (package, host, scope) cell.
type cellKey struct {
	pkg   string
	host  string
	scope string
}

// rmaRef carries the cell identity needed to execute or record RMA ops.
type rmaRef struct {
	pkg      string
	host     host.ID
	scope    string
	strategy string
	version  string
}

// rmaMode tells the RMA executor how a digest mismatch is interpreted.
type rmaMode int

const (
	// modeRemove executes the RMA of a removal or of a dropped artifact: a
	// mismatch is drift and the target is left in place (hands-off).
	modeRemove rmaMode = iota
	// modeRollback undoes a just-attempted install: a mismatch means the step
	// never ran, so there is nothing to undo.
	modeRollback
)

// rmaOutcome reports what an RMA execution did.
type rmaOutcome struct {
	notes    []string
	trashID  string
	handsOff bool
	err      error
}

// recoveredCell is the crash-replay verdict for one cell.
type recoveredCell struct {
	status  Status
	version string
	note    string
}

// runner carries one Run call.
type runner struct {
	ctx context.Context //nolint:containedctx // the runner is a short-lived per-Run value carrying the call context

	deps   Deps
	plan   Plan
	opts   Options
	now    func() time.Time
	logger embedlog.Logger

	lockPath string
	lockGen  int

	mu          sync.Mutex
	lockChanged bool
	cells       []CellResult
	breakers    map[host.ID]CircuitState
	notes       []string
	recovered   map[cellKey]recoveredCell
	confirmErr  error
}

// newRunner builds the per-run state.
func newRunner(ctx context.Context, deps Deps, plan Plan, opts Options) *runner {
	r := &runner{
		ctx:       ctx,
		deps:      deps,
		plan:      plan,
		opts:      opts,
		now:       opts.Now,
		logger:    opts.Logger,
		lockPath:  deps.LockPath,
		breakers:  map[host.ID]CircuitState{},
		recovered: map[cellKey]recoveredCell{},
	}

	if r.lockPath == "" {
		r.lockPath = deps.Home.LockPath()
	}

	r.cells = make([]CellResult, len(plan.Actions))

	return r
}

// recover reads the journal and finishes every interrupted action without
// touching the adapters again. Read failures are fatal; per-intent failures
// only add notes.
func (r *runner) recover() error {
	events, err := r.deps.Journal.Read()
	if err != nil {
		return &ReceiptError{Cause: fmt.Errorf("read journal: %w", err)}
	}

	lastCommit := r.trackGenerations(events)

	// Only intents after the last lock commit can be incomplete: a committed
	// event closes every action of its run.
	pending := pendingIntents(events, lastCommit)

	if r.opts.DryRun {
		for _, event := range pending {
			r.note("dry-run: interrupted action %s on %s would be replayed", event.Package, event.Host)
		}

		return nil
	}

	intents, undecodable := lastIntents(pending)

	for _, event := range undecodable {
		r.note("journal: intent event %d cannot be decoded; ignored", event.Seq)
	}

	for _, event := range intents {
		if err := r.recoverIntent(event); err != nil {
			r.note("recovery of %s on %s: %v", event.Package, event.Host, err)
		}
	}

	return nil
}

// trackGenerations records the highest lock generation and returns the seq of
// the last lock commit.
func (r *runner) trackGenerations(events []receipt.Event) int64 {
	lastCommit := int64(0)

	for _, event := range events {
		if event.LockGeneration > r.lockGen {
			r.lockGen = event.LockGeneration
		}

		if event.Kind == receipt.EventLock && event.Seq > lastCommit {
			lastCommit = event.Seq
		}
	}

	return lastCommit
}

// pendingIntents lists intent events after the last lock commit; a rollback
// event closes the earlier intents of its cell.
func pendingIntents(events []receipt.Event, after int64) []receipt.Event {
	pending := make([]receipt.Event, 0, len(events))

	for _, event := range events {
		if event.Seq <= after {
			continue
		}

		if event.Kind == receipt.EventRollback {
			pending = slices.DeleteFunc(pending, func(intent receipt.Event) bool {
				return intent.Package == event.Package && intent.Host == event.Host && intent.Scope == event.Scope
			})

			continue
		}

		if _, ok := event.Extra[intentExtraKey]; ok {
			pending = append(pending, event)
		}
	}

	return pending
}

// lastIntents keeps the newest intent per cell and lists undecodable events.
func lastIntents(pending []receipt.Event) ([]receipt.Event, []receipt.Event) {
	last := map[cellKey]receipt.Event{}
	order := make([]cellKey, 0)
	undecodable := make([]receipt.Event, 0)

	for _, event := range pending {
		var intent intentRecord

		if err := json.Unmarshal(event.Extra[intentExtraKey], &intent); err != nil {
			undecodable = append(undecodable, event)

			continue
		}

		key := cellKey{intent.Action.Delivery.Package.ID, string(intent.Action.Host), intentScope(intent.Action)}

		if _, seen := last[key]; !seen {
			order = append(order, key)
		}

		last[key] = event
	}

	out := make([]receipt.Event, 0, len(order))

	for _, key := range order {
		out = append(out, last[key])
	}

	return out, undecodable
}

// recoverIntent completes or rolls back one interrupted action.
func (r *runner) recoverIntent(event receipt.Event) error {
	var intent intentRecord

	if err := json.Unmarshal(event.Extra[intentExtraKey], &intent); err != nil {
		return fmt.Errorf("decode intent: %w", err)
	}

	switch intent.Action.Kind {
	case ActionInstall, ActionUpdate:
		return r.recoverInstall(event, intent)
	case ActionRemove:
		return r.recoverRemove(event, intent)
	default:
		return fmt.Errorf("unknown intent kind %q", intent.Action.Kind)
	}
}

// recoverInstall finalises one interrupted install/update: a receipt with the
// same version and strategy means the action committed; matching artifacts mean
// the install happened and the receipt is written forward; anything else rolls
// back through the intent RMA.
func (r *runner) recoverInstall(event receipt.Event, intent intentRecord) error {
	action := intent.Action
	pkg := action.Delivery.Package
	scope := intentScope(action)

	rec, ok, err := r.deps.Receipts.Get(pkg.ID, string(action.Host), scope)
	if err != nil {
		return &ReceiptError{Package: pkg.ID, Host: string(action.Host), Cause: err}
	}

	if ok && rec.Version == pkg.Version && rec.Strategy == string(action.Delivery.Strategy) {
		r.upsertLock(rec)

		r.markRecovered(cellKey{pkg.ID, string(action.Host), scope}, StatusCurrent, pkg.Version,
			"recovered: the interrupted action had already committed")
		r.note("recovered: the interrupted %s of %s %s on %s had already committed", action.Kind, pkg.ID, pkg.Version, action.Host)

		return nil
	}

	verified, verifiable := r.verifyArtifacts(intent)

	if verifiable && verified {
		notes := r.dropOld(action, host.Result{RMA: intent.RMA})

		rma, stale := r.commitRMA(intent.RMA, action.Previous, pkg.ID, string(action.Host))

		installed := event.At
		if action.Previous != nil && !action.Previous.InstalledAt.IsZero() {
			installed = action.Previous.InstalledAt
		}

		record := receipt.Receipt{
			Schema: receipt.Schema, Package: pkg.ID, Host: string(action.Host), Scope: scope,
			Strategy: string(action.Delivery.Strategy), Version: pkg.Version,
			InstalledAt: installed, UpdatedAt: r.now(),
			Artifacts: intent.Artifacts, RMA: rma,
		}

		if err := r.deps.Receipts.Put(record); err != nil {
			return &ReceiptError{Package: pkg.ID, Host: string(action.Host), Cause: err}
		}

		for _, note := range r.purgeStale(stale) {
			r.note("%s", note)
		}

		r.upsertLock(record)
		r.markRecovered(cellKey{pkg.ID, string(action.Host), scope}, StatusCurrent, pkg.Version,
			"recovered: the interrupted install was committed from the journal")
		r.note("recovered: committed the interrupted install of %s %s on %s", pkg.ID, pkg.Version, action.Host)

		for _, note := range notes {
			r.note("%s", note)
		}

		return nil
	}

	return r.rollbackInstall(intent, action, scope)
}

// rollbackInstall undoes one interrupted install with the intent RMA, whose
// replaced ops lost their backup ids in the crash, and reports what actually
// happened: a partial rollback must not claim recovery.
func (r *runner) rollbackInstall(intent intentRecord, action Action, scope string) error {
	pkg := action.Delivery.Package

	// The intent RMA is the dry plan: replaced ops lost their backup ids in
	// the crash. Resolve them from the trash exactly like the forward-commit
	// and in-process failure paths, so the user's original bytes come back.
	ops := r.resolveBackups(intent.RMA, action.Previous, pkg.ID, string(action.Host))

	outcome := r.executeRMA(action.Host, rmaRef{
		pkg: pkg.ID, host: action.Host, scope: scope,
		strategy: string(action.Delivery.Strategy), version: pkg.Version,
	}, ops, causeRecovery, modeRollback)
	if outcome.err != nil {
		return fmt.Errorf("roll back interrupted install: %w", outcome.err)
	}

	for _, note := range outcome.notes {
		r.note("%s", note)
	}

	if outcome.handsOff {
		r.note("recovery incomplete: the interrupted install of %s %s on %s could not be fully rolled back; some artifacts were left in place",
			pkg.ID, pkg.Version, action.Host)
	} else {
		r.note("recovered: rolled back the interrupted install of %s %s on %s", pkg.ID, pkg.Version, action.Host)
	}

	return nil
}

// recoverRemove rolls one interrupted removal forward: the RMA is idempotent,
// so completing it is always safe.
func (r *runner) recoverRemove(event receipt.Event, intent intentRecord) error {
	action := intent.Action
	if action.Previous == nil {
		return errors.New("remove intent without a previous receipt")
	}

	pkg := action.Previous.Package
	scope := action.Previous.Scope

	if committed, err := r.removeCommitted(event, pkg, string(action.Host), scope); err != nil {
		return err
	} else if committed {
		r.deleteLock(pkg, string(action.Host), scope)
		r.markRecovered(cellKey{pkg, string(action.Host), scope}, StatusCurrent, "",
			"recovered: the interrupted removal had already committed")

		return nil
	}

	outcome := r.executeRMA(action.Host, rmaRef{
		pkg: pkg, host: action.Host, scope: scope,
		strategy: action.Previous.Strategy, version: action.Previous.Version,
	}, action.Previous.RMA, causeForRemove(action), modeRemove)
	if outcome.err != nil {
		return fmt.Errorf("finish interrupted removal: %w", outcome.err)
	}

	if err := r.finishRemove(action, outcome); err != nil {
		return err
	}

	r.markRecovered(cellKey{pkg, string(action.Host), scope}, StatusCurrent, "",
		"recovered: finished the interrupted removal")

	return nil
}

// removeCommitted reports whether the removal already wrote its tombstone.
func (r *runner) removeCommitted(event receipt.Event, pkg, hostID, scope string) (bool, error) {
	tombstones, err := r.deps.Tombstones.Load()
	if err != nil {
		return false, &ReceiptError{Package: pkg, Host: hostID, Cause: err}
	}

	for _, tombstone := range tombstones {
		if tombstone.Package == pkg && tombstone.Host == hostID && tombstone.Scope == scope &&
			!tombstone.RemovedAt.Before(event.At) {
			return true, nil
		}
	}

	return false, nil
}

// verifyArtifacts reports whether every verifiable artifact of the intent
// matches the disk; verifiable is false when nothing can be checked.
func (r *runner) verifyArtifacts(intent intentRecord) (bool, bool) {
	verifiable := false

	// A document the receipt claims record by record is checked record by
	// record: comparing the whole document against one digest would call a
	// user's own added record a drift on verger's records and freeze the cell
	// (DRIFT-2).
	perRecord := adapterCheckedDocuments(intent.RMA)

	for _, artifact := range intent.Artifacts {
		ok, checkable := verifyArtifact(intent, artifact)

		// A document claimed record by record is verified by those records; the
		// whole-document comparison below is exactly the coarse check that would
		// call a user's own added record a drift (DRIFT-2).
		if checkable && perRecord[artifact.Path] {
			continue
		}

		if !checkable {
			continue
		}

		verifiable = true

		if !ok {
			return false, true
		}
	}

	return true, verifiable
}

// verifyArtifact checks one journaled artifact against the disk; checkable is
// false for artifacts only the host itself can confirm.
func verifyArtifact(intent intentRecord, artifact receipt.Artifact) (bool, bool) {
	if artifact.Path == "" || !filepath.IsAbs(artifact.Path) ||
		artifact.Kind == kindMCP || artifact.Kind == kindPatchDocument {
		return true, false
	}

	if op, found := configOpFor(intent.RMA, artifact.Path); found {
		data, err := os.ReadFile(artifact.Path) //nolint:gosec // G304: the path comes from the journaled intent
		if err != nil {
			return false, true
		}

		current, exists, err := configValueDigest(data, op.KeyPath)

		return err == nil && exists && current == artifact.Digest, true
	}

	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return false, true
	}

	if info.IsDir() {
		sum, err := digest.Tree(artifact.Path)

		return err == nil && sum == artifact.Digest, true
	}

	sum, err := digest.File(artifact.Path)

	return err == nil && sum == artifact.Digest, true
}

// kindMCP names host-install MCP artifacts, whose presence only the host
// oracle can prove. It is checked here and nowhere else, so no test claims it.
const kindMCP = "mcp"

// kindPatchDocument names a host config document whose records the adapter owns
// by id (the DSH home patch layer). The bytes on disk are not verger's alone — a
// user may add their own records — so only the adapter can tell whether
// verger's own records still hold the values it wrote; a byte digest of the
// whole file would report a foreign edit as drift on the package's records.
// pkg/apply skips it for that reason, and the drift walk skips the document's
// file op (adapterCheckedDocuments); both are pinned by tests.
const kindPatchDocument = "patch-document"

// configOpFor finds the config-key op of one artifact path.
func configOpFor(ops []receipt.Op, path string) (receipt.Op, bool) {
	for _, op := range ops {
		if op.Kind == receipt.OpConfigKey && op.Path == path {
			return op, true
		}
	}

	return receipt.Op{}, false
}

// markRecovered records the crash-replay verdict of one cell.
func (r *runner) markRecovered(key cellKey, status Status, version, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.recovered[key] = recoveredCell{status: status, version: version, note: note}
}

// execute runs the plan in two phases: the shared writes first, one host at a
// time, then every remaining cell with one goroutine per host bounded by
// Parallel and actions sequential in plan order inside a host.
//
// The first phase is what makes a shared root safe. agy, codex, dsh and omp
// resolve skills under one directory, so one package names one physical file
// once per host. Left to the parallel phase, every one of those hosts decides
// independently that the file must be written: they look it up before any of
// them has written it, they all find nothing, and they all add a write for one
// file. Whichever finishes last leaves the tree, and a host that trashes what
// another host is mid-way through writing takes the file away from the run that
// was about to succeed. That is how an install reported every host as current
// with nothing on disk (NIGHT-pR-24).
//
// Running the writer's cell alone, before the others exist, makes it
// impossible for two hosts to execute over one path.
func (r *runner) execute() {
	writers, perHost := splitSharedWrites(groupByHost(r.plan.Actions), r.plan.Shared)

	for _, group := range writers {
		r.runHost(group)
	}

	limit := r.opts.Parallel
	if limit <= 0 {
		limit = defaultParallel
	}

	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup

	for _, group := range perHost {
		wg.Go(func() {
			sem <- struct{}{}

			defer func() { <-sem }()

			r.runHost(group)
		})
	}

	wg.Wait()
}

// splitSharedWrites partitions the host groups into the ones that write a shared
// target and the ones that do not. A host's whole group moves to the first
// phase, because a host that writes one shared file writes its own files in the
// same delivery and splitting a delivery across phases would be a second thing
// to keep in order.
func splitSharedWrites(groups []hostGroup, shared []SharedTarget) (writers, perHost []hostGroup) {
	writing := map[host.ID]bool{}

	for _, target := range shared {
		writing[target.Writer] = true
	}

	if len(writing) == 0 {
		return nil, groups
	}

	for _, group := range groups {
		if writing[group.host] {
			writers = append(writers, group)

			continue
		}

		perHost = append(perHost, group)
	}

	return writers, perHost
}

// hostGroup is the ordered action indexes of one host.
type hostGroup struct {
	host  host.ID
	index []int
}

// groupByHost buckets action indexes by host, preserving plan order.
func groupByHost(actions []Action) []hostGroup {
	groups := []hostGroup{}

	index := map[host.ID]int{}

	for i, action := range actions {
		at, ok := index[action.Host]

		if !ok {
			index[action.Host] = len(groups)
			groups = append(groups, hostGroup{host: action.Host, index: []int{i}})

			continue
		}

		groups[at].index = append(groups[at].index, i)
	}

	return groups
}

// runHost executes every action of one host sequentially.
func (r *runner) runHost(group hostGroup) {
	for _, idx := range group.index {
		action := r.plan.Actions[idx]

		if err := r.ctx.Err(); err != nil {
			r.setCell(idx, failedCell(action, "context canceled: "+err.Error()))

			continue
		}

		if breaker := r.breaker(group.host); breaker.Tripped {
			cell := failedCell(action, "circuit breaker: host "+string(group.host)+" failed earlier in this run; not retried")
			cell.Status = breaker.Status
			r.setCell(idx, cell)

			continue
		}

		r.emit(action, stepPlan, fmt.Sprintf("%s %s", action.Kind, actionPackage(action)))

		cell := r.runAction(action)
		r.setCell(idx, cell)

		switch cell.Status {
		case StatusSkew, StatusFailed:
			r.trip(group.host, cell.Status, strings.Join(cell.Notes, "; "))
		default:
			// Other statuses (hands-off, needs-auth, foreign) are per-action
			// verdicts, not host failures: the breaker stays closed.
		}
	}
}

// runAction dispatches one action unless crash replay already settled its cell.
func (r *runner) runAction(action Action) CellResult {
	cell := CellResult{
		Package:  actionPackage(action),
		Host:     action.Host,
		Scope:    intentScope(action),
		Kind:     action.Kind,
		Strategy: actionStrategy(action),
		Version:  actionVersion(action),
		Restored: action.Restored,
	}
	key := cellKey{cell.Package, string(cell.Host), cell.Scope}

	if recovered, ok := r.recoveredCell(key); ok {
		if action.Kind == ActionRemove || recovered.version == "" || recovered.version == cell.Version {
			cell.Status = recovered.status
			cell.Notes = []string{recovered.note}

			return cell
		}
	}

	if action.Kind == ActionRemove {
		return r.runRemove(action, cell)
	}

	return r.runInstall(action, cell)
}

// runInstall executes the two phases of one install or update.
func (r *runner) runInstall(action Action, cell CellResult) CellResult {
	pkg := action.Delivery.Package

	// A re-install without the previous receipt still replaces what verger
	// already owns: load it, so drift, drop-old and the RMA see the cell.
	if action.Previous == nil {
		stored, ok, err := r.deps.Receipts.Get(pkg.ID, string(action.Host), cell.Scope)
		if err != nil {
			return failedCell(action, "receipt: "+err.Error())
		}

		if ok {
			action.Previous = &stored
		}
	}

	// Drift detection: never replace something that changed behind the receipt.
	// backedUp is this cell's own backup path: one forced run can touch
	// several cells, and a single shared path would leave every cell but the
	// last pointing at a copy that is not theirs.
	var backedUp string

	if action.Previous != nil {
		if drift := r.driftNotes(*action.Previous, &backedUp); len(drift) > 0 {
			cell.Status = StatusHandsOff
			cell.Notes = slices.Clone(drift)
			cell.Notes = append(cell.Notes, "nothing was written")

			return cell
		}
	}

	cell.Backup = backedUp

	planned, err := r.deliver(action, true)
	if err != nil {
		return r.planFailure(cell, action, err)
	}

	if r.opts.DryRun {
		cell.Status = StatusCurrent
		cell.Notes = slices.Clone(planned.Notes)
		cell.Notes = append(cell.Notes, "dry-run")

		return cell
	}

	if err := r.journalIntent(action, planned); err != nil {
		return failedCell(action, "journal intent: "+err.Error())
	}

	started := r.now()
	r.emit(action, stepInstall, pkg.ID+" "+pkg.Version)

	result, err := r.deliver(action, false)
	if err != nil {
		return r.installFailed(action, cell, planned, result, err)
	}

	// The delivery returned without an error, which is not the same as the file
	// being there: a Result is the adapter's own account of itself, and a cell
	// that reports "current" for an artifact the disk does not hold is the
	// silent success — all hosts current, nothing written, exit 0. So the claim
	// is checked against the disk before the receipt commits to it.
	artifacts, ops := committed(planned, result)

	if missing := undeliveredArtifacts(artifacts, ops); len(missing) > 0 {
		return r.installFailed(action, cell, planned, result, &ArtifactsMissingError{
			Host:  action.Host,
			Paths: missing,
		})
	}

	r.emit(action, stepVerify, pkg.ID+" "+pkg.Version+" verified")

	if crash := crashAfterInstall; crash != nil {
		crash()
	}

	dropNotes := r.dropOld(action, result)

	return r.commitInstall(action, cell, started, planned, result, dropNotes)
}

// ArtifactsMissingError reports a delivery that claimed files the disk does not
// hold. It is its own type because "the run said it wrote this and did not" is a
// different answer from any step failure: the caller has to treat the cell as
// failed, not as delivered with a note.
type ArtifactsMissingError struct {
	Host  host.ID
	Paths []string
}

// Error implements error.
func (e *ArtifactsMissingError) Error() string {
	return fmt.Sprintf("%s: delivered but not on disk: %s", e.Host, strings.Join(e.Paths, ", "))
}

// committed picks the artifacts and ops a receipt will record, which is what a
// cell claims: the delivery's own result, or the plan it fell back on.
func committed(planned, result host.Result) ([]receipt.Artifact, []receipt.Op) {
	artifacts := result.Artifacts
	if len(artifacts) == 0 {
		artifacts = planned.Artifacts
	}

	ops := result.RMA
	if len(ops) == 0 {
		ops = planned.RMA
	}

	return artifacts, ops
}

// undeliveredArtifacts names the artifacts a cell claims that the disk does not
// hold with the recorded bytes.
//
// A receipt records what the run PLANNED to write, so "the plan produced this
// artifact" and "this file exists" are different claims, and only the second is
// what the user gets. Checking it here closes the class: a report saying
// `current` for a file that is not there is a failed cell from now on.
//
// Verification is the one crash replay uses, so an interrupted run and a
// finished one agree on what a written artifact looks like. An artifact only the
// host itself can confirm — an installed MCP server, a document the adapter owns
// record by record — is not checkable here and is never counted as missing.
func undeliveredArtifacts(artifacts []receipt.Artifact, ops []receipt.Op) []string {
	claim := intentRecord{Artifacts: artifacts, RMA: ops}

	var missing []string

	for _, artifact := range artifacts {
		ok, checkable := verifyArtifact(claim, artifact)
		if checkable && !ok {
			missing = append(missing, artifact.Path)
		}
	}

	slices.Sort(missing)

	return missing
}

// journalIntent appends the pre-write intent of one install or update.
func (r *runner) journalIntent(action Action, planned host.Result) error {
	pkg := action.Delivery.Package

	raw, err := json.Marshal(intentRecord{Action: action, Artifacts: planned.Artifacts, RMA: planned.RMA})
	if err != nil {
		return fmt.Errorf("encode intent: %w", err)
	}

	return r.deps.Journal.Append(receipt.Event{
		Kind: eventKindFor(action.Kind), Package: pkg.ID, Host: string(action.Host),
		Scope: intentScope(action), Version: pkg.Version,
		Extra: map[string]json.RawMessage{intentExtraKey: raw},
	})
}

// installFailed rolls one failed install attempt back and reports the cell.
func (r *runner) installFailed(action Action, cell CellResult, planned, result host.Result, failure error) CellResult {
	pkg := action.Delivery.Package

	ops := result.RMA
	if len(ops) == 0 {
		ops = planned.RMA
	}

	ops = r.resolveBackups(ops, action.Previous, pkg.ID, string(action.Host))

	outcome := r.executeRMA(action.Host, rmaRef{
		pkg: pkg.ID, host: action.Host, scope: cell.Scope,
		strategy: string(action.Delivery.Strategy), version: pkg.Version,
	}, ops, causeRollback, modeRollback)

	notes := outcome.notes
	if outcome.err != nil {
		notes = append(notes, "rollback: "+outcome.err.Error())
	}

	// Close the intent: a failed action is not an interrupted one (N-2).
	if err := r.deps.Journal.Append(receipt.Event{
		Kind: receipt.EventRollback, Package: pkg.ID, Host: string(action.Host),
		Scope: intentScope(action), Version: pkg.Version, Cause: causeRollback,
	}); err != nil {
		notes = append(notes, "journal: rollback event: "+err.Error())
	}

	r.emit(action, stepRollback, fmt.Sprintf("rolled back %s: %v", pkg.ID, failure))

	return r.planFailure(withNotes(cell, notes...), action, failure)
}

// commitInstall writes the receipt and the lock cell of one successful install.
func (r *runner) commitInstall(action Action, cell CellResult, started time.Time, planned, result host.Result, dropNotes []string) CellResult {
	pkg := action.Delivery.Package

	artifacts := result.Artifacts
	if len(artifacts) == 0 {
		artifacts = planned.Artifacts
	}

	rma := result.RMA
	if len(rma) == 0 {
		rma = planned.RMA
	}

	rma, stale := r.commitRMA(rma, action.Previous, pkg.ID, string(action.Host))

	installed := started
	if action.Previous != nil && !action.Previous.InstalledAt.IsZero() {
		installed = action.Previous.InstalledAt
	}

	record := receipt.Receipt{
		Schema: receipt.Schema, Package: pkg.ID, Host: string(action.Host), Scope: cell.Scope,
		Strategy: string(action.Delivery.Strategy), Version: pkg.Version,
		InstalledAt: installed, UpdatedAt: r.now(),
		Artifacts: artifacts, RMA: rma,
	}

	if err := r.deps.Receipts.Put(record); err != nil {
		return withNotes(failedCell(action, "receipt: "+err.Error()), dropNotes...)
	}

	r.emit(action, stepReceipt, pkg.ID+" "+pkg.Version)

	if crash := crashAfterReceipt; crash != nil {
		crash()
	}

	r.upsertLock(record)

	// A delivery that recorded nothing at all wrote nothing. Saying "current"
	// here is how `verger install` came to report two delivered hosts over an
	// empty home: the receipt was written, it was simply empty, and nothing
	// checked.
	//
	// Empty artifacts alone are not the test. A synth delivery registers a
	// marketplace — a claim with no file behind it — and a host that
	// registers rather than writes is still delivered. So the question is
	// whether the receipt claims anything: no artifacts AND no operations
	// means nothing was claimed, and "delivered" would be a lie.
	if len(record.Artifacts) == 0 && len(record.RMA) == 0 {
		cell.Status = StatusSkipped
		cell.Notes = []string{"nothing to write: the package produced no files for " + string(action.Host)}

		return cell
	}

	cell.Status = StatusCurrent
	cell.Notes = slices.Clone(result.Notes)
	cell.Notes = append(cell.Notes, dropNotes...)
	cell.Notes = append(cell.Notes, r.purgeStale(stale)...)

	return cell
}

// runRemove executes one removal: the intent first, the RMA next, the
// tombstone + receipt deletion last. A dry run stops before any write.
func (r *runner) runRemove(action Action, cell CellResult) CellResult {
	prev := action.Previous
	if prev == nil {
		cell.Status = StatusMissing
		cell.Notes = []string{"no previous receipt"}

		return cell
	}

	if r.opts.DryRun {
		cell.Status = StatusCurrent
		cell.Notes = []string{"dry-run"}

		return cell
	}

	raw, err := json.Marshal(intentRecord{Action: action, Artifacts: prev.Artifacts, RMA: prev.RMA})
	if err != nil {
		return failedCell(action, "encode intent: "+err.Error())
	}

	if err := r.deps.Journal.Append(receipt.Event{
		Kind: receipt.EventRemove, Package: prev.Package, Host: string(action.Host),
		Scope: prev.Scope, Version: prev.Version, Cause: causeForRemove(action), Initiator: action.Initiator,
		Extra: map[string]json.RawMessage{intentExtraKey: raw},
	}); err != nil {
		return failedCell(action, "journal intent: "+err.Error())
	}

	r.emit(action, stepRemove, prev.Package)

	outcome := r.executeRMA(action.Host, rmaRef{
		pkg: prev.Package, host: action.Host, scope: prev.Scope,
		strategy: prev.Strategy, version: prev.Version,
	}, prev.RMA, causeForRemove(action), modeRemove)

	cell.Notes = append(cell.Notes, outcome.notes...)

	if outcome.err != nil {
		cell.Status = StatusFailed
		cell.Notes = append(cell.Notes, "remove: "+outcome.err.Error())

		return cell
	}

	if outcome.handsOff {
		cell.Status = StatusHandsOff

		return cell
	}

	if err := r.finishRemove(action, outcome); err != nil {
		cell.Status = StatusFailed
		cell.Notes = append(cell.Notes, err.Error())

		return cell
	}

	cell.Status = StatusCurrent

	return cell
}

// finishRemove writes the tombstone, deletes the receipt and drops the lock
// cell of one completed removal.
func (r *runner) finishRemove(action Action, outcome rmaOutcome) error {
	prev := action.Previous
	cause := causeForRemove(action)

	hostID := action.Initiator
	if hostID == "" {
		hostID = string(action.Host)
	}

	tombstone := receipt.Tombstone{
		Schema: receipt.Schema, Package: prev.Package, Host: hostID, Scope: prev.Scope,
		RemovedAt: r.now(), Cause: receipt.Cause(cause), TrashID: outcome.trashID,
	}

	if err := r.deps.Tombstones.Add(tombstone); err != nil {
		return &ReceiptError{Package: prev.Package, Host: string(action.Host), Cause: err}
	}

	if err := r.deps.Receipts.Delete(prev.Package, string(action.Host), prev.Scope); err != nil {
		return &ReceiptError{Package: prev.Package, Host: string(action.Host), Cause: err}
	}

	r.deleteLock(prev.Package, string(action.Host), prev.Scope)

	r.emit(action, stepReceipt, "removed "+prev.Package)

	return nil
}

// driftNotes compares the previous receipt against the disk and reports every
// mismatch; an empty result means the cell is safe to touch.
func (r *runner) driftNotes(prev receipt.Receipt, backedUp *string) []string {
	adapterChecked := adapterCheckedDocuments(prev.RMA)

	var notes []string

	for _, op := range prev.RMA {
		if note, blocked := r.driftNote(op, adapterChecked, backedUp); blocked {
			notes = append(notes, note)
		}
	}

	return notes
}

// adapterCheckedDocuments returns the document paths whose records a receipt
// owns one by one (kindPatchDocument). Their bytes are not verger's alone — a
// user may add their own records to the same file — so a byte digest would call
// a foreign edit drift on the package's own records. The adapter that owns the
// records checks them itself, per record, and reports hands-off on the one that
// moved.
func adapterCheckedDocuments(ops []receipt.Op) map[string]bool {
	documents := map[string]bool{}

	for _, op := range ops {
		if op.Kind == receipt.OpRecord && op.Note != "" {
			documents[op.Note] = true
		}
	}

	return documents
}

// driftNote checks one receipt operation against the disk; blocked is true when
// the artifact must not be touched.
func (r *runner) driftNote(op receipt.Op, adapterChecked map[string]bool, backedUp *string) (string, bool) {
	// DRIFT-2: a document the adapter owns record by record is not compared
	// as a whole, at any granularity. A coarse byte digest over such a
	// document would call every edit in it drift — including the user's own
	// additions, which belong to nobody verger owns — and freeze records
	// that never moved. The adapter checks its own records and reports the
	// one that did.
	if adapterChecked[op.Path] && op.Kind != receipt.OpRecord {
		return "", false
	}

	switch op.Kind {
	case receipt.OpHostInstall, receipt.OpRecord:
		return "", false
	case receipt.OpConfigKey:
		return r.driftConfigNote(op)
	default:
		return r.driftEntryNote(op, backedUp)
	}
}

// driftConfigNote checks one config key against its receipt digest.
func (r *runner) driftConfigNote(op receipt.Op) (string, bool) {
	data, err := os.ReadFile(op.Path) //nolint:gosec // G304: the path comes from a receipt
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}

	if err != nil {
		return fmt.Sprintf("%s#%s: cannot read: %v", op.Path, op.KeyPath, err), true
	}

	current, exists, err := configValueDigest(data, op.KeyPath)
	if err != nil {
		return fmt.Sprintf("%s#%s: cannot parse: %v", op.Path, op.KeyPath, err), true
	}

	if exists && current != op.Digest {
		return fmt.Sprintf("%s#%s: hands-off (the value changed since the receipt)", op.Path, op.KeyPath), true
	}

	return "", false
}

// driftEntryNote checks one file, tree or link against its receipt digest.
func (r *runner) driftEntryNote(op receipt.Op, backedUp *string) (string, bool) {
	info, err := os.Lstat(op.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}

	if err != nil {
		return fmt.Sprintf("%s: cannot stat: %v", op.Path, err), true
	}

	current, err := currentEntryDigest(op.Kind, op.Path, info)
	if err != nil {
		return op.Path + ": cannot digest: " + err.Error(), true
	}

	if current != op.Digest {
		// A forced run keeps the user's copy before it writes ours. The
		// overwrite is the point of Force; losing the edit is not.
		if r.opts.Force && r.opts.BackupsRoot != "" {
			saved, err := r.backupUserEdit(op)
			if err != nil {
				// A forced run that cannot keep the user's copy must not
				// overwrite anyway, and it must not hide why: a silent
				// fall-through to hands-off reads as "you forgot
				// --force", which sends the reader to the wrong flag.
				return op.Path + ": hands-off (forced, but the backup failed: " + err.Error() + ")", true
			}

			*backedUp = saved

			return "", false
		}

		return op.Path + ": hands-off (the artifact changed since the receipt)", true
	}

	return "", false
}

// backupUserEdit copies one artifact into the run's backup root and returns
// where it went. The layout is <root>/<UTC stamp>/<path>, so one run's copies
// never collide and the stamp orders them the way they happened. The
// artifact keeps its name and its bytes; only its place changes.
//
// The kind decides the copy, because a delivered skill is a directory: reading
// it as a file failed on every tree-shaped package, and the caller turned that
// failure back into a hands-off note that still said "rerun with --force" —
// the one answer that sends a reader to a flag they already used.
func (r *runner) backupUserEdit(op receipt.Op) (string, error) {
	stamp := r.now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(r.opts.BackupsRoot, stamp, op.Path)

	if err := fsutil.EnsureDir(filepath.Dir(dest), 0o700); err != nil {
		return "", fmt.Errorf("backup %s: %w", op.Path, err)
	}

	if op.Kind == receipt.OpCopyTree {
		if err := fsutil.CopyTree(r.ctx, op.Path, dest); err != nil {
			return "", fmt.Errorf("backup %s: %w", op.Path, err)
		}

		return dest, nil
	}

	// A symlink is backed up as the link it is, not as whatever it points at:
	// restoring the target's bytes would restore a different artifact.
	if op.Kind == receipt.OpSymlink {
		target, err := os.Readlink(op.Path)
		if err != nil {
			return "", fmt.Errorf("backup %s: %w", op.Path, err)
		}

		if err := os.Symlink(target, dest); err != nil {
			return "", fmt.Errorf("backup %s: %w", op.Path, err)
		}

		return dest, nil
	}

	data, err := os.ReadFile(op.Path) //nolint:gosec // G304: the path comes from a receipt
	if err != nil {
		return "", fmt.Errorf("backup %s: %w", op.Path, err)
	}

	if err := fsutil.WriteFileAtomic(dest, data, 0o600); err != nil {
		return "", fmt.Errorf("backup %s: %w", op.Path, err)
	}

	return dest, nil
}

// dropOld executes the previous receipt's RMA for artifacts the new delivery
// no longer carries.
func (r *runner) dropOld(action Action, result host.Result) []string {
	if action.Previous == nil {
		return nil
	}

	identities := map[string]bool{}

	for _, op := range result.RMA {
		identities[opIdentity(op)] = true
	}

	var dropped []receipt.Op

	for _, op := range action.Previous.RMA {
		if identities[opIdentity(op)] {
			continue
		}

		dropped = append(dropped, op)
	}

	if len(dropped) == 0 {
		return nil
	}

	prev := *action.Previous

	outcome := r.executeRMA(action.Host, rmaRef{
		pkg: prev.Package, host: action.Host, scope: prev.Scope,
		strategy: prev.Strategy, version: prev.Version,
	}, dropped, causeUpdate, modeRemove)
	if outcome.err != nil {
		return append(outcome.notes, "drop previous: "+outcome.err.Error())
	}

	return outcome.notes
}

// opIdentity compares one RMA operation across receipts. File-like ops (file,
// tree, symlink, hardlink) are keyed by path alone: a target that switches kind
// between versions is still the same target (N-3).
func opIdentity(op receipt.Op) string {
	switch op.Kind {
	case receipt.OpConfigKey:
		return "config:" + op.Path + "#" + op.KeyPath
	case receipt.OpHostInstall:
		return "host:" + strings.Join(op.Command, "\x00")
	default:
		return "entry:" + op.Path
	}
}

// executeRMA runs one reverse manifest in reverse order. Missing targets are
// tolerated, a digest mismatch leaves the target in place (hands-off in removal
// mode), host-install ops delegate to the adapter.
func (r *runner) executeRMA(hostID host.ID, ref rmaRef, ops []receipt.Op, cause string, mode rmaMode) rmaOutcome {
	out := rmaOutcome{}

	// A rollback undoes as much as it can: one failing op (a host resource
	// whose install never landed) must not leave every other artifact behind.
	var rollbackErrs []error

	for _, op := range slices.Backward(ops) {
		if err := r.ctx.Err(); err != nil {
			out.err = errors.Join(append(rollbackErrs, err)...)

			return out
		}

		note, trashID, handsOff, err := r.execRMAOp(hostID, ref, op, cause, mode)
		if err != nil && mode == modeRollback {
			rollbackErrs = append(rollbackErrs, err)

			continue
		}

		if err != nil {
			out.err = err

			return out
		}

		if note != "" {
			out.notes = append(out.notes, note)
		}

		if trashID != "" {
			out.trashID = trashID
		}

		out.handsOff = out.handsOff || handsOff
	}

	out.err = errors.Join(rollbackErrs...)

	return out
}

// execRMAOp executes one reverse operation.
func (r *runner) execRMAOp(hostID host.ID, ref rmaRef, op receipt.Op, cause string, mode rmaMode) (string, string, bool, error) {
	switch op.Kind {
	case receipt.OpHostInstall:
		return "", "", false, r.hostUninstall(hostID, ref, op)
	case receipt.OpConfigKey:
		note, handsOff, err := r.undoConfigKey(ref, op, mode)

		return note, "", handsOff, err
	case receipt.OpWriteFile, receipt.OpCopyTree, receipt.OpSymlink, receipt.OpHardlink:
		return r.undoEntry(ref, op, cause, mode)
	case receipt.OpRecord:
		// A record op claims a value inside a document the package's own
		// file op reverses; there is nothing to undo on its own.
		return "", "", false, nil
	default:
		return "", "", false, fmt.Errorf("unknown rma op %q", op.Kind)
	}
}

// hostUninstall delegates one host-install op to the adapter.
func (r *runner) hostUninstall(hostID host.ID, ref rmaRef, op receipt.Op) error {
	adapter, ok := r.deps.Hosts[hostID]
	if !ok {
		return fmt.Errorf("host %s is not registered", hostID)
	}

	if _, err := adapter.Uninstall(r.ctx, "", receipt.Receipt{
		Schema: receipt.Schema, Package: ref.pkg, Host: string(hostID), Scope: ref.scope,
		Strategy: ref.strategy, Version: ref.version, RMA: []receipt.Op{op},
	}); err != nil {
		return fmt.Errorf("host uninstall %s: %w", hostID, err)
	}

	return nil
}

// undoEntry reverses one file, tree or link operation.
func (r *runner) undoEntry(ref rmaRef, op receipt.Op, cause string, mode rmaMode) (string, string, bool, error) {
	info, err := os.Lstat(op.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return r.undoMissing(op)
	}

	if err != nil {
		return "", "", false, fmt.Errorf("stat %s: %w", op.Path, err)
	}

	current, err := currentEntryDigest(op.Kind, op.Path, info)
	if err != nil {
		return "", "", false, err
	}

	if current != op.Digest {
		return r.undoMismatch(op, mode)
	}

	if !op.Existed {
		trashID, err := r.trash(ref, op.Path, cause, current)
		if err != nil {
			return "", "", false, err
		}

		// A delivery that created a directory and filled it — a rendered
		// plugin, a copied skill tree — must not leave the empty shell
		// behind in the host's own config root. Only a directory this
		// delivery emptied is removed, and never one that still holds
		// anything.
		r.pruneEmptiedParent(op.Path)

		return "", trashID, false, nil
	}

	if op.Backup == "" {
		return op.Path + ": hands-off (no backup recorded); left in place", "", true, nil
	}

	return r.undoRestore(ref, op, cause, current)
}

// pruneEmptiedParent removes the directory of a reversed file when the
// reversal left it empty. A directory that still holds anything, and a
// directory verger did not create, are both left alone: the emptiness is the
// whole test.
func (r *runner) pruneEmptiedParent(path string) {
	dir := filepath.Dir(path)

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}

	_ = os.Remove(dir)
}

// undoMissing handles a target that is already gone: an idempotent no-op, or a
// backup restore when the target replaced something.
func (r *runner) undoMissing(op receipt.Op) (string, string, bool, error) {
	if !op.Existed || op.Backup == "" {
		return "", "", false, nil
	}

	if gone, err := r.backupGone(op); gone || err != nil {
		return backupGoneNote(op), "", false, err
	}

	if _, err := r.deps.Store.Trash().Restore(r.ctx, op.Backup); err != nil {
		return "", "", false, fmt.Errorf("restore %s: %w", op.Path, err)
	}

	return fmt.Sprintf("restored %s from the trash", op.Path), "", false, nil
}

// backupGone reports whether the bucket holding an op's pre-install bytes no
// longer exists (reaped by a trash GC) — the restore can never succeed (N-1).
func (r *runner) backupGone(op receipt.Op) (bool, error) {
	_, err := r.deps.Store.Trash().Get(op.Backup)
	if _, missing := errors.AsType[*store.NotFoundError](err); missing {
		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("backup %s of %s: %w", op.Backup, op.Path, err)
	}

	return false, nil
}

// backupGoneNote explains a restore that cannot happen.
func backupGoneNote(op receipt.Op) string {
	return op.Path + ": the pre-install backup " + op.Backup + " is gone from the trash; left in place"
}

// undoMismatch interprets a digest mismatch for one file operation.
func (r *runner) undoMismatch(op receipt.Op, mode rmaMode) (string, string, bool, error) {
	if mode == modeRollback {
		return "", "", false, nil // the step never ran
	}

	return op.Path + ": hands-off (the artifact changed since the receipt); left in place", "", true, nil
}

// undoRestore replaces verger's artifact with the trashed previous bytes.
func (r *runner) undoRestore(ref rmaRef, op receipt.Op, cause string, current digest.Hash) (string, string, bool, error) {
	// Never trash verger's artifact when the bytes it replaced are gone: the
	// path would end up empty and the removal could never converge (N-1).
	if gone, err := r.backupGone(op); gone || err != nil {
		return backupGoneNote(op), "", false, err
	}

	trashID, err := r.trash(ref, op.Path, cause, current)
	if err != nil {
		return "", "", false, err
	}

	if _, err := r.deps.Store.Trash().Restore(r.ctx, op.Backup); err != nil {
		return "", "", false, fmt.Errorf("restore %s: %w", op.Path, err)
	}

	return fmt.Sprintf("restored %s from the trash", op.Path), trashID, false, nil
}

// trash moves one artifact into the store trash with its cell tags.
func (r *runner) trash(ref rmaRef, path, cause string, sum digest.Hash) (string, error) {
	entry, err := r.deps.Store.Trash().Put(r.ctx, path, store.PutOptions{
		Package: ref.pkg, Host: string(ref.host), Cause: cause, Digest: sum,
	})
	if err != nil {
		return "", fmt.Errorf("trash %s: %w", path, err)
	}

	return entry.ID, nil
}

// currentEntryDigest hashes one on-disk entry with the op's semantics.
func currentEntryDigest(kind receipt.OpKind, path string, info fs.FileInfo) (digest.Hash, error) {
	switch {
	case kind == receipt.OpSymlink && info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("read link %s: %w", path, err)
		}

		return digest.Bytes([]byte(target)), nil
	case info.IsDir():
		sum, err := digest.Tree(path)
		if err != nil {
			return "", fmt.Errorf("digest tree %s: %w", path, err)
		}

		return sum, nil
	default:
		sum, err := digest.File(path)
		if err != nil {
			return "", fmt.Errorf("digest %s: %w", path, err)
		}

		return sum, nil
	}
}

// resolveBackups fills the Backup of replaced ops that lost their bucket id in
// a crash: the newest trash entry of the same path and cell is the backup the
// interrupted run created. A bucket the adapter staged under its own source
// path is matched by the previous receipt's digest for the op path.
func (r *runner) resolveBackups(ops []receipt.Op, prev *receipt.Receipt, pkg, hostID string) []receipt.Op {
	entries, err := r.deps.Store.Trash().List()
	if err != nil {
		return ops
	}

	out := slices.Clone(ops)

	for i, op := range out {
		if !op.Existed || op.Backup != "" || op.Kind == receipt.OpHostInstall ||
			op.Kind == receipt.OpConfigKey || op.Kind == receipt.OpRecord {
			continue
		}

		out[i].Backup = backupByPath(entries, op.Path, pkg, hostID)

		if out[i].Backup == "" {
			out[i].Backup = r.backupByDigest(entries, op, prev, pkg, hostID)
		}
	}

	return out
}

// commitRMA prepares the reverse manifest a receipt records: lost backup ids
// are resolved, then every op the previous receipt already owned keeps the
// pre-install state that receipt recorded. It returns the RMA and the stale
// intermediate buckets to purge once the receipt is written.
func (r *runner) commitRMA(ops []receipt.Op, prev *receipt.Receipt, pkg, hostID string) ([]receipt.Op, []string) {
	return inheritPreInstall(r.resolveBackups(ops, prev, pkg, hostID), prev)
}

// inheritPreInstall keeps the pre-install state of every target the previous
// receipt owned: a re-delivery or update trashes verger's own previous
// artifact, which is not what a removal must restore (NF-1). The buckets
// holding verger's own previous artifacts are returned as stale.
func inheritPreInstall(ops []receipt.Op, prev *receipt.Receipt) ([]receipt.Op, []string) {
	if prev == nil {
		return ops, nil
	}

	before := make(map[string]receipt.Op, len(prev.RMA))
	for _, op := range prev.RMA {
		before[opIdentity(op)] = op
	}

	out := slices.Clone(ops)

	var stale []string

	for i, op := range out {
		old, ok := before[opIdentity(op)]
		if !ok {
			continue
		}

		if op.Backup != "" && op.Backup != old.Backup {
			stale = append(stale, op.Backup)
		}

		out[i].Existed = old.Existed
		out[i].Backup = old.Backup
	}

	return out, stale
}

// purgeStale removes the trash buckets of verger's own previous artifacts once
// a receipt no longer references them; a failed purge only leaves a note.
func (r *runner) purgeStale(ids []string) []string {
	var notes []string

	for _, id := range ids {
		if err := r.deps.Store.Trash().Remove(id); err != nil {
			notes = append(notes, "trash: stale backup "+id+" kept: "+err.Error())
		}
	}

	return notes
}

// backupByPath returns the newest bucket of one path and cell.
func backupByPath(entries []store.Entry, path, pkg, hostID string) string {
	for _, entry := range slices.Backward(entries) {
		if entry.Original == path && entry.Package == pkg && entry.Host == hostID {
			return entry.ID
		}
	}

	return ""
}

// backupByDigest returns the newest bucket of one cell whose stored payload
// matches the previous receipt's digest for the op path. The digest check
// keeps the fallback content-addressed: a bucket staged under a different
// source path is the recorded previous artifact and nothing else.
func (r *runner) backupByDigest(entries []store.Entry, op receipt.Op, prev *receipt.Receipt, pkg, hostID string) string {
	want, ok := previousDigest(prev, op.Path)
	if !ok {
		return ""
	}

	for _, entry := range slices.Backward(entries) {
		if entry.Package != pkg || entry.Host != hostID {
			continue
		}

		if r.payloadDigest(entry) == want {
			return entry.ID
		}
	}

	return ""
}

// previousDigest returns the digest a previous receipt recorded for one path.
func previousDigest(prev *receipt.Receipt, path string) (digest.Hash, bool) {
	if prev == nil {
		return "", false
	}

	for _, op := range slices.Backward(prev.RMA) {
		if op.Path == path && op.Kind != receipt.OpHostInstall && op.Kind != receipt.OpConfigKey && op.Digest.Valid() {
			return op.Digest, true
		}
	}

	return "", false
}

// payloadDigest hashes the stored payload of one trash bucket; an unreadable
// bucket reports no digest.
func (r *runner) payloadDigest(entry store.Entry) digest.Hash {
	payload := filepath.Join(r.deps.Store.TrashDir(), entry.ID, entry.Stored)

	if entry.Kind == "dir" {
		sum, err := digest.Tree(payload)
		if err != nil {
			return ""
		}

		return sum
	}

	sum, err := digest.File(payload)
	if err != nil {
		return ""
	}

	return sum
}

// deliver calls the adapter, resolving confirmed collisions by moving the
// foreign path to the trash and retrying.
func (r *runner) deliver(action Action, dry bool) (host.Result, error) {
	adapter, ok := r.deps.Hosts[action.Host]
	if !ok {
		return host.Result{}, fmt.Errorf("host %s is not registered", action.Host)
	}

	delivery := action.Delivery
	delivery.DryRun = dry

	for attempt := 0; attempt <= maxConflictAttempts; attempt++ {
		result, err := adapter.Deliver(r.ctx, "", delivery)
		if err == nil {
			return result, nil
		}

		collision, isCollision := errors.AsType[*host.CollisionError](err)
		if !isCollision {
			return result, err
		}

		confirmed, confirmErr := r.confirm(Question{
			Kind: "conflict", Package: action.Delivery.Package.ID, Host: action.Host,
			Message: collision.Error(),
		})
		if confirmErr != nil {
			return result, confirmErr
		}

		if !confirmed {
			return result, &conflictRefusedError{cause: err}
		}

		if r.opts.DryRun {
			r.note("dry-run: %s would be moved to the trash to resolve a conflict", collision.Path)

			return result, &conflictDryRunError{path: collision.Path}
		}

		if attempt == maxConflictAttempts {
			return result, err
		}

		if _, err := r.deps.Store.Trash().Put(r.ctx, collision.Path, store.PutOptions{
			Package: action.Delivery.Package.ID, Host: string(action.Host), Cause: causeConflict,
		}); err != nil {
			return result, fmt.Errorf("resolve conflict %s: %w", collision.Path, err)
		}
	}

	return host.Result{}, errors.New("conflict resolution did not converge")
}

// confirm asks the confirmer; a missing confirmer records the run-level
// ErrConfirmationRequired.
func (r *runner) confirm(q Question) (bool, error) {
	if r.opts.Confirm == nil {
		err := fmt.Errorf("%w: %s", ErrConfirmationRequired, q.Message)

		r.mu.Lock()
		if r.confirmErr == nil {
			r.confirmErr = fmt.Errorf("%w on %s", ErrConfirmationRequired, q.Package+"@"+string(q.Host))
		}
		r.mu.Unlock()

		return false, err
	}

	return r.opts.Confirm.Confirm(r.ctx, q)
}

// conflictRefusedError marks a confirmed conflict the user declined.
type conflictRefusedError struct {
	cause error
}

// Error implements error.
func (e *conflictRefusedError) Error() string {
	return "conflict not confirmed; nothing was written: " + e.cause.Error()
}

// Unwrap returns the underlying collision.
func (e *conflictRefusedError) Unwrap() error { return e.cause }

// conflictDryRunError marks a conflict a dry run would resolve.
type conflictDryRunError struct {
	path string
}

// Error implements error.
func (e *conflictDryRunError) Error() string {
	return "dry-run: " + e.path + " would be moved to the trash"
}

// planFailure maps one planning error of an action to a cell result.
func (r *runner) planFailure(cell CellResult, action Action, err error) CellResult {
	if _, ok := errors.AsType[*conflictDryRunError](err); ok {
		cell.Status = StatusCurrent
		cell.Notes = append(cell.Notes, err.Error())

		return cell
	}

	if handsOff, ok := errors.AsType[*render.HandsOffError](err); ok {
		_, confirmErr := r.confirm(Question{
			Kind: "conflict", Package: action.Delivery.Package.ID, Host: action.Host,
			Message: handsOff.Error(),
		})
		if confirmErr != nil {
			return failedCell(action, confirmErr.Error())
		}

		cell.Status = StatusHandsOff
		cell.Notes = append(cell.Notes, err.Error(), "nothing was written")

		return cell
	}

	return r.classify(cell, err)
}

// classify maps an adapter error to the cell status.
func (r *runner) classify(cell CellResult, err error) CellResult {
	cell.Notes = append(cell.Notes, err.Error())

	switch {
	case errors.Is(err, ErrConfirmationRequired):
		cell.Status = StatusFailed
	case isErr[*conflictRefusedError](err):
		cell.Status = StatusForeign
	case isErr[*host.MissingSecretsError](err):
		cell.Status = StatusNeedsAuth
	case isErr[*host.PolicyError](err), isErr[*host.UnsupportedStrategyError](err), isErr[*host.NotSupportedError](err):
		cell.Status = StatusFailed
	case isErr[*host.DeliveryError](err) && deliveryStep(err) == "verify":
		cell.Status = StatusSkew
	default:
		cell.Status = StatusFailed
	}

	return cell
}

// isErr reports whether err matches a typed error.
func isErr[T error](err error) bool {
	_, ok := errors.AsType[T](err)

	return ok
}

// deliveryStep extracts the failing step of a *host.DeliveryError.
func deliveryStep(err error) string {
	target, ok := errors.AsType[*host.DeliveryError](err)
	if !ok {
		return ""
	}

	return target.Step
}

// failedCell builds a failed cell with one note.
func failedCell(action Action, note string) CellResult {
	return CellResult{
		Package: actionPackage(action), Host: action.Host, Scope: intentScope(action),
		Kind: action.Kind, Strategy: actionStrategy(action), Version: actionVersion(action),
		Status: StatusFailed, Notes: []string{note},
	}
}

// withNotes appends notes to a cell.
func withNotes(cell CellResult, notes ...string) CellResult {
	cell.Notes = append(cell.Notes, notes...)

	return cell
}

// actionPackage returns the package id an action concerns.
func actionPackage(action Action) string {
	if action.Kind == ActionRemove && action.Previous != nil {
		return action.Previous.Package
	}

	return action.Delivery.Package.ID
}

// actionVersion returns the version an action records.
func actionVersion(action Action) string {
	if action.Kind == ActionRemove && action.Previous != nil {
		return action.Previous.Version
	}

	return action.Delivery.Package.Version
}

// actionStrategy returns the strategy an action concerns.
func actionStrategy(action Action) lock.Strategy {
	if action.Kind == ActionRemove && action.Previous != nil {
		return lock.Strategy(action.Previous.Strategy)
	}

	return action.Delivery.Strategy
}

// intentScope resolves the receipt scope of one action: the delivery or
// previous receipt scope, defaulting to user.
func intentScope(action Action) string {
	if action.Previous != nil && action.Previous.Scope != "" {
		return action.Previous.Scope
	}

	if action.Delivery.Package.Scope != "" {
		return action.Delivery.Package.Scope
	}

	return receipt.ScopeUser
}

// causeForRemove returns the registered removal cause, defaulting to user.
func causeForRemove(action Action) string {
	if action.Cause == "" {
		return defaultCause
	}

	return action.Cause
}

// eventKindFor maps an action kind to its journal event kind.
func eventKindFor(kind Kind) receipt.EventKind {
	switch kind {
	case ActionUpdate:
		return receipt.EventUpdate
	default:
		return receipt.EventInstall
	}
}

// upsertLock stores the lock cell of a committed receipt.
func (r *runner) upsertLock(record receipt.Receipt) {
	if record.Version == "" || record.Strategy == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	err := r.deps.Lock.Upsert(lock.Cell{
		Package: record.Package, Host: record.Host, Scope: record.Scope,
		Version: record.Version, Strategy: lock.Strategy(record.Strategy), UpdatedAt: r.now(),
	})
	if err != nil {
		r.notes = append(r.notes, "lock cell "+record.Package+"/"+record.Host+": "+err.Error())

		return
	}

	r.lockChanged = true
}

// deleteLock drops the lock cell of one removal.
func (r *runner) deleteLock(pkg, hostID, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.deps.Lock.Delete(pkg, hostID, scope) > 0 {
		r.lockChanged = true
	}
}

// commit saves the lock once at the end and appends its generation event.
func (r *runner) commit() error {
	if r.opts.DryRun {
		return nil
	}

	r.mu.Lock()
	changed := r.lockChanged
	r.mu.Unlock()

	if !changed {
		return nil
	}

	if err := r.deps.Lock.Save(r.lockPath); err != nil {
		return &LockError{Path: r.lockPath, Cause: err}
	}

	generation := r.lockGen + 1

	r.emit(Action{}, stepLock, fmt.Sprintf("lock generation %d", generation))

	if err := r.deps.Journal.Append(receipt.Event{
		Kind: receipt.EventLock, LockGeneration: generation, LockDigest: r.deps.Lock.Digest(),
	}); err != nil {
		return &ReceiptError{Cause: fmt.Errorf("journal lock generation: %w", err)}
	}

	r.lockGen = generation

	return nil
}

// breaker returns the circuit state of one host.
func (r *runner) breaker(hostID host.ID) CircuitState {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.breakers[hostID]
}

// trip opens the circuit of one host for the rest of the run.
func (r *runner) trip(hostID host.ID, status Status, cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.breakers[hostID] = CircuitState{Host: hostID, Tripped: true, Status: status, Cause: cause}
}

// note appends one run-level note.
func (r *runner) note(format string, args ...any) {
	message := fmt.Sprintf(format, args...)

	r.mu.Lock()
	r.notes = append(r.notes, message)
	r.mu.Unlock()

	r.logf("%s", message)
}

// logf logs when the caller supplied a logger.
func (r *runner) logf(format string, args ...any) {
	if r.logger.Log() == nil {
		return
	}

	r.logger.Printf(format, args...)
}

// setCell stores the result of one action.
func (r *runner) setCell(idx int, cell CellResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The backup path is per cell, not per run: one forced run can touch
	// several cells and each has to name its own copy. The run's directory
	// is shared, the path inside it is not.
	r.cells[idx] = cell
}

// recoveredCell returns the crash-replay verdict of one cell.
func (r *runner) recoveredCell(key cellKey) (recoveredCell, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cell, ok := r.recovered[key]

	return cell, ok
}

// report assembles the run report in plan order.
func (r *runner) report() Report {
	r.mu.Lock()
	defer r.mu.Unlock()

	breakers := make([]CircuitState, 0, len(r.breakers))

	for _, breaker := range r.breakers {
		if breaker.Tripped {
			breakers = append(breakers, breaker)
		}
	}

	slices.SortFunc(breakers, func(a, b CircuitState) int {
		return strings.Compare(string(a.Host), string(b.Host))
	})

	cells := make([]CellResult, 0, len(r.cells))

	for _, cell := range r.cells {
		if cell.Kind != "" {
			cells = append(cells, cell)
		}
	}

	return Report{Cells: cells, Notes: slices.Clone(r.notes), Breakers: breakers}
}

// runErr is the run-level error, if any: cancellation wins over confirmation.
func (r *runner) runErr() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.confirmErr
}

// emit sends one progress event; a nil channel drops it and cancellation stops
// the send.
func (r *runner) emit(action Action, step, message string) {
	if r.opts.Events == nil {
		return
	}

	event := Event{
		At: r.now(), Package: actionPackage(action), Host: action.Host,
		Step: step, Message: message,
	}

	select {
	case r.opts.Events <- event:
	case <-r.ctx.Done():
	}
}

// validateDeps rejects a run with missing dependencies.
func validateDeps(deps Deps) error {
	var missing string

	switch {
	case deps.Home == nil:
		missing = "home"
	case deps.Store == nil:
		missing = "store"
	case deps.Receipts == nil:
		missing = "receipts"
	case deps.Journal == nil:
		missing = "journal"
	case deps.Tombstones == nil:
		missing = "tombstones"
	case deps.Hosts == nil:
		missing = "hosts"
	case deps.Owned == nil:
		missing = "owned"
	case deps.Lock == nil:
		missing = "lock"
	default:
		return nil
	}

	return &ConfigError{Cause: errors.New("missing dependency: " + missing)}
}

// validatePlan rejects actions that cannot be executed.
func validatePlan(deps Deps, plan Plan) error {
	for i, action := range plan.Actions {
		if _, ok := deps.Hosts[action.Host]; !ok {
			return &ConfigError{Cause: fmt.Errorf("action %d: host %q is not registered", i, action.Host)}
		}

		switch action.Kind {
		case ActionInstall, ActionUpdate:
			if action.Delivery.Package.ID == "" {
				return &ConfigError{Cause: fmt.Errorf("action %d: package id is required", i)}
			}

			if action.Delivery.Package.Version == "" {
				return &ConfigError{Cause: fmt.Errorf("action %d: package version is required", i)}
			}

			if !deliverableStrategy(action.Delivery.Strategy) {
				return &ConfigError{Cause: fmt.Errorf("action %d: strategy %q is not deliverable", i, action.Delivery.Strategy)}
			}

			if action.Kind == ActionUpdate && action.Previous == nil {
				return &ConfigError{Cause: fmt.Errorf("action %d: update needs a previous receipt", i)}
			}
		case ActionRemove:
			if action.Previous == nil {
				return &ConfigError{Cause: fmt.Errorf("action %d: remove needs a previous receipt", i)}
			}
		default:
			return &ConfigError{Cause: fmt.Errorf("action %d: unknown kind %q", i, action.Kind)}
		}
	}

	return nil
}

// deliverableStrategy reports whether an adapter can deliver the strategy.
func deliverableStrategy(strategy lock.Strategy) bool {
	switch strategy {
	case lock.StrategyNative, lock.StrategySynth, lock.StrategyLoose:
		return true
	default:
		return false
	}
}
