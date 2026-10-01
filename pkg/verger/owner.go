package verger

import (
	"maps"
	"sync"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// deliveredPath is one path this run wrote: the package that wrote it and what
// it wrote.
//
// The package id has to be remembered with the digest. Asking the receipt store
// for it mid-run answers nothing — the receipt is written when the run ends — and
// an owner with an empty id fails the very check that asked, which is the
// collision all over again with a stranger who is this package.
type deliveredPath struct {
	pkg string
	sum digest.Hash
}

// Ownership resolves path ownership from a receipt store, and remembers what
// THIS run has already written.
//
// The second half is correctness, not an optimisation. Four adapters resolve
// skills under one shared root — agy, codex, dsh and omp all read ~/.agents/ —
// so a single package names the same physical file four times. Receipts are
// committed when the run ends, so a receipt-backed Owner cannot see what an
// earlier host in the SAME run just wrote: the next host finds the file present,
// finds no owner on disk, and reports a stranger's file.
//
//	/home/u/.agents/skills/caveman already exists and is not owned by verger
//
// The install then delivers nothing and the user is told a stranger owns their
// skill. A shared path is one physical artifact, so the run must know about it
// before the receipts do.
//
// The pointer is the point: every adapter a client builds is handed this same
// Ownership, so the four hosts share one view of the run. A value would give
// each host its own empty map and change nothing.
type Ownership struct {
	receipts *receipt.Store

	mu        sync.Mutex
	delivered map[string]deliveredPath
	// shared is the PLAN's decision, seeded before any host runs: this path is
	// one physical file several hosts resolve to, and exactly this host writes
	// it. The executor runs that host's cell alone, before any other cell
	// exists, so two hosts can never execute over one path.
	//
	// It is one installed value rather than a map each planner writes into,
	// because the outcome must not depend on which goroutine reached the path
	// first — that ordering is what produced an install that reported every
	// host current with nothing on disk.
	shared map[string]host.ID
}

// NewOwnership returns the receipt-backed ownership source of one receipts
// directory. A second front end that drives pkg/apply itself injects the same
// source Install and Remove use.
func NewOwnership(receiptsDir string) *Ownership {
	return &Ownership{
		receipts:  receipt.NewStore(receiptsDir),
		delivered: map[string]deliveredPath{},
	}
}

// Owner implements host.PathOwner. The run's own writes count: a path this run
// wrote belongs to the package that wrote it, whoever did the writing.
func (o *Ownership) Owner(path string) (string, bool) {
	o.mu.Lock()
	written, ok := o.delivered[path]
	o.mu.Unlock()

	if ok {
		return written.pkg, true
	}

	record, _, ok := o.artifact(path)

	return record.Package, ok
}

// DeliveredThisRun reports what this run already wrote at path, if anything.
func (o *Ownership) DeliveredThisRun(path string) (pkg string, sum digest.Hash, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	written, ok := o.delivered[path]

	return written.pkg, written.sum, ok
}

// RecordDelivered notes that this run wrote path with the given digest on behalf
// of pkg.
func (o *Ownership) RecordDelivered(path, pkg string, sum digest.Hash) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.delivered == nil {
		o.delivered = map[string]deliveredPath{}
	}

	o.delivered[path] = deliveredPath{pkg: pkg, sum: sum}
}

// SetSharedTargets installs the plan's writer decision for every shared path in
// one step, so a fresh plan never inherits yesterday's answer.
//
// It is installed rather than added to, and deliberately NOT cleared by
// BeginRun: the decision is made while the plan is built and consumed while the
// run executes, and Apply calls BeginRun after planning. Wiping it there left
// every host convinced it owned the shared file — or none of them.
func (o *Ownership) SetSharedTargets(writers map[string]host.ID) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if len(writers) == 0 {
		o.shared = nil

		return
	}

	o.shared = maps.Clone(writers)
}

// SharedWriter reports the host the plan chose to write path, and whether the
// plan made a choice at all. A path with no choice is the host's own to write,
// as before.
func (o *Ownership) SharedWriter(path string) (hostID host.ID, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	writer, ok := o.shared[path]

	return writer, ok
}

// BeginRun clears the run-scoped layer. Ownership outlives a single run when a
// caller holds it across many — that is exactly beadle's watch loop — so a stale
// entry must not let tomorrow's run claim today's write.
func (o *Ownership) BeginRun() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.delivered = map[string]deliveredPath{}
}

// ArtifactDigest implements host.ArtifactDigests: an unchanged MCP server is
// re-delivered without a host call (NF-2).
func (o *Ownership) ArtifactDigest(path string) (digest.Hash, bool) {
	if _, sum, ok := o.DeliveredThisRun(path); ok {
		return sum, true
	}

	_, artifact, ok := o.artifact(path)

	return artifact.Digest, ok
}

// artifact finds the receipt and artifact recorded for one path.
func (o *Ownership) artifact(path string) (receipt.Receipt, receipt.Artifact, bool) {
	list, err := o.receipts.List()
	if err != nil {
		return receipt.Receipt{}, receipt.Artifact{}, false
	}

	for _, record := range list {
		for _, artifact := range record.Artifacts {
			if artifact.Path == path {
				return record, artifact, true
			}
		}
	}

	return receipt.Receipt{}, receipt.Artifact{}, false
}
