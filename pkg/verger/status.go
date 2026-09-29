package verger

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/source"
)

// Cell statuses shared by the status matrix and an execution report.
const (
	// StatusCurrent means the receipt and the file agree.
	StatusCurrent = "current"
	// StatusMissing means a lock cell has no receipt.
	StatusMissing = "missing"
	// StatusSkew means the lock and the receipt disagree on the version.
	StatusSkew = "skew"
	// StatusPlanned is a cell of a plan that has not run.
	StatusPlanned = "planned"
	// StatusHandsOff means the cell was refused rather than written.
	StatusHandsOff = "hands-off"
	// StatusForeign means the cell exists but no receipt proves verger wrote it.
	StatusForeign = "foreign"
	// StatusFailed means the cell's execution failed.
	StatusFailed = "failed"
	// StatusRemoved means the cell was removed by this run.
	StatusRemoved = "removed"
)

// Cell is the stable JSON shape of one applied cell — the data `status --json`
// prints and a second front end renders in its own table.
type Cell struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Level    string   `json:"level,omitempty"` // host maturity (DESIGN §10.3): experimental|beta|stable
	Kind     string   `json:"kind,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// StatusDocument is the package × host matrix of one scope.
type StatusDocument struct {
	Home  string `json:"home,omitempty"`
	Cells []Cell `json:"cells"`
}

// StatusOptions narrow the matrix.
type StatusOptions struct {
	// Paths is the scope whose receipts and lock the matrix merges.
	Paths Paths
	// OutdatedOnly keeps only the skewed and missing cells.
	OutdatedOnly bool
}

// Status merges receipts and lock cells into the stable matrix. It is the same
// data `status --json` prints, with no rendering.
func (c *Client) Status(ctx context.Context, opts StatusOptions) (StatusDocument, error) {
	if err := ctx.Err(); err != nil {
		return StatusDocument{}, err
	}

	// A lock or spec written by a newer verger is reported before the matrix
	// is built: reporting "nothing changed" over a home this build cannot
	// read is the silent failure this gate exists to stop.
	if err := c.checkSchemaVersions(opts.Paths); err != nil {
		return StatusDocument{}, err
	}

	if opts.Paths.ReceiptsDir == "" {
		// A zero Paths has no receipts dir to read; answering with an empty
		// matrix would look like "nothing installed" instead of "no scope".
		return StatusDocument{}, &UsageError{Cause: errors.New("status needs a resolved scope: call Client.Paths first")}
	}
	// A spec whose sources cannot be read is not a spec that declares
	// nothing. `sync` already refuses on one; `status` answering "no
	// cells" over it is the read-only command confirming, with a green
	// answer, the very thing the user came to find out about.
	if err := checkSpecSources(opts.Paths); err != nil {
		return StatusDocument{}, err
	}

	receipts := receipt.NewStore(opts.Paths.ReceiptsDir)

	list, err := receipts.List()
	if err != nil {
		return StatusDocument{}, err
	}

	lockDoc, err := LoadLock(opts.Paths.LockPath)
	if err != nil {
		return StatusDocument{}, err
	}

	doc := StatusDocument{Home: opts.Paths.Root, Cells: []Cell{}}

	doc.Cells = append(doc.Cells, c.receiptCells(ctx, list, lockDoc)...)

	doc.Cells = append(doc.Cells, lockOnlyCells(lockDoc, list)...)

	slices.SortFunc(doc.Cells, func(left, right Cell) int {
		return cmp.Or(
			cmp.Compare(left.Package, right.Package),
			cmp.Compare(left.Host, right.Host),
			cmp.Compare(left.Scope, right.Scope),
		)
	})

	if opts.OutdatedOnly {
		doc.Cells = slices.DeleteFunc(doc.Cells, func(cell Cell) bool {
			return cell.Status != StatusSkew && cell.Status != StatusMissing
		})

		if doc.Cells == nil {
			doc.Cells = []Cell{}
		}
	}

	return doc, nil
}

// receiptCells turns the receipt store into matrix cells, one per record.
func (c *Client) receiptCells(ctx context.Context, list []Receipt, lockDoc *lock.Lock) []Cell {
	cells := make([]Cell, 0, len(list))

	for _, record := range list {
		cell := Cell{
			Package: record.Package, Host: record.Host, Scope: record.Scope,
			Status: StatusCurrent, Version: record.Version, Strategy: record.Strategy,
			Level: HostMaturity(record.Host),
		}

		if lockCell, ok := lockDoc.Cell(record.Package, record.Host, record.Scope); ok && lockCell.Version != record.Version {
			cell.Status = StatusSkew
			cell.Notes = []string{"lock has " + lockCell.Version}
		}

		// Drift is checked first on purpose: a file that is present but has
		// moved is the user's own edit and must stay hands-off, which says
		// more than "missing" and must not be overwritten.
		markDrift(ctx, &cell, c.mcpProbe(record.Host), record)

		// A receipt survives a clone; the files it names do not. Without
		// this the cell reads "current" about a package this machine never
		// received, and `verger sync` has nothing to do about it.
		if cell.Status == StatusCurrent && !ReceiptFilesPresent(record) {
			cell.Status = StatusMissing
			cell.Notes = append(cell.Notes, "receipt is here but the delivered files are not")
		}

		cells = append(cells, cell)
	}

	return cells
}

// lockOnlyCells reports the cells a lock names that no receipt backs. They
// are what makes a status honest about a home that was cloned: the lock came
// across, the packages did not.
func lockOnlyCells(lockDoc *lock.Lock, list []Receipt) []Cell {
	var cells []Cell

	for _, lockCell := range lockDoc.Cells {
		if _, ok := findReceipt(list, lockCell.Package, lockCell.Host, lockCell.Scope); ok {
			continue
		}

		cells = append(cells, Cell{
			Package: lockCell.Package, Host: lockCell.Host, Scope: lockCell.Scope,
			Status: StatusMissing, Version: lockCell.Version, Strategy: string(lockCell.Strategy),
			Level: HostMaturity(lockCell.Host),
			Notes: []string{"lock cell without a receipt"},
		})
	}

	return cells
}

// checkSpecSources resolves every local source a spec declares, and reports
// the first one it cannot. It is deliberately source-level rather than
// package-level: the question status is answering is "can this spec be read
// at all", and a source that does not parse is a fact no amount of reading
// receipts will change.
//
// A spec that is absent is not a failure. An empty home is the ordinary
// first-run state, and answering "no cells" about it is the truth.
func checkSpecSources(paths Paths) error {
	if paths.SpecPath == "" {
		return nil
	}

	doc, ok, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	if !ok {
		return nil
	}

	dir := filepath.Dir(paths.SpecPath)

	for _, src := range doc.Sources {
		if !isLocalSourceURL(src.URL) {
			continue
		}

		if _, err := source.ParseSource(src.URL, dir); err != nil {
			return &UsageError{Cause: fmt.Errorf("source %s: %w", src.Name, err)}
		}
	}

	return nil
}

// markDrift marks one cell hands-off when a delivered value moved (DRIFT-1):
// the check is per config key, so a key the user added to the same document
// changes nothing here. A CLI-managed MCP server is compared against the
// host's own reported value through probe.
func markDrift(ctx context.Context, cell *Cell, probe apply.DriftProbe, record receipt.Receipt) {
	drift, err := apply.ReceiptDrift(ctx, record, probe)
	if err != nil || !drift.Drifted() {
		return
	}

	cell.Status = StatusHandsOff
	cell.Notes = append(cell.Notes, drift.Note())
}

// mcpProbe builds the drift probe of one host, bound to the adapters this client
// holds. A host whose oracle cannot probe CLI-managed MCP servers answers
// Unknown, which the drift walk skips.
func (c *Client) mcpProbe(id string) apply.DriftProbe {
	adapters := c.Hosts()
	byID := make(map[host.ID]host.Host, len(adapters))

	for _, h := range adapters {
		byID[h.ID()] = h
	}

	return mcpDriftProbe{hosts: byID, id: host.ID(id)}
}

// mcpDriftProbe adapts the host adapters to apply.DriftProbe.
type mcpDriftProbe struct {
	hosts map[host.ID]host.Host
	id    host.ID
}

// MCPDigest implements apply.DriftProbe.
func (p mcpDriftProbe) MCPDigest(ctx context.Context, name string) (apply.MCPDrift, error) {
	h, ok := p.hosts[p.id]
	if !ok {
		return apply.MCPDrift{Unknown: true}, nil
	}

	prober, ok := h.Oracle().(host.MCPProber)
	if !ok {
		return apply.MCPDrift{Unknown: true}, nil
	}

	server, err := prober.MCPGet(ctx, name)
	if err != nil {
		if errors.Is(err, host.ErrServerNotFound) {
			return apply.MCPDrift{Gone: true}, nil
		}

		return apply.MCPDrift{}, err
	}

	data, err := json.Marshal(server)
	if err != nil {
		return apply.MCPDrift{}, err
	}

	return apply.MCPDrift{Sum: digest.Bytes(data)}, nil
}

// findReceipt locates one receipt by key.
func findReceipt(list []receipt.Receipt, pkg, hostID, scope string) (receipt.Receipt, bool) {
	for _, record := range list {
		if record.Package == pkg && record.Host == hostID && record.Scope == scope {
			return record, true
		}
	}

	return receipt.Receipt{}, false
}

// WhyDocument is the stable JSON document of one cell's explanation.
type WhyDocument struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Reasons  []string `json:"reasons"`
	Blockers []string `json:"blockers"`
}

// Why explains one package on one host: what the receipt and the lock say, what
// the spec says, and what the consent gate is waiting for.
func (c *Client) Why(ctx context.Context, paths Paths, pkgID, hostID string) (WhyDocument, error) {
	if err := ctx.Err(); err != nil {
		return WhyDocument{}, err
	}

	doc := WhyDocument{
		Package: pkgID, Host: hostID, Scope: string(paths.Scope),
		Reasons: []string{}, Blockers: []string{},
	}

	receipts := receipt.NewStore(paths.ReceiptsDir)

	list, err := receipts.List()
	if err != nil {
		return WhyDocument{}, err
	}

	record, hasReceipt := findReceipt(list, pkgID, hostID, string(paths.Scope))

	lockDoc, err := LoadLock(paths.LockPath)
	if err != nil {
		return WhyDocument{}, err
	}

	lockCell, hasLock := lockDoc.Cell(pkgID, hostID, string(paths.Scope))

	fillWhyCell(&doc, record, hasReceipt, lockCell, hasLock)

	doc.Reasons = append(doc.Reasons, whySpecReasons(paths, pkgID)...)

	if err := c.appendHooksReason(pkgID, &doc); err != nil {
		return WhyDocument{}, err
	}

	slices.Sort(doc.Reasons)

	return doc, nil
}

// fillWhyCell fills the receipt/lock state of one explanation.
func fillWhyCell(doc *WhyDocument, record receipt.Receipt, hasReceipt bool, lockCell lock.Cell, hasLock bool) {
	switch {
	case hasReceipt:
		doc.Status = StatusCurrent
		doc.Version = record.Version
		doc.Strategy = record.Strategy
		doc.Reasons = append(doc.Reasons, fmt.Sprintf("strategy %s from the %s receipt", record.Strategy, record.Scope))

		if record.Version != "" {
			doc.Reasons = append(doc.Reasons, "version "+record.Version)
		}

		if hasLock && lockCell.Version != record.Version {
			doc.Status = StatusSkew
			doc.Blockers = append(doc.Blockers, "the lock cell wants "+lockCell.Version)
		}
	case hasLock:
		doc.Status = StatusMissing
		doc.Version = lockCell.Version
		doc.Strategy = string(lockCell.Strategy)
		doc.Blockers = append(doc.Blockers, "the lock has a cell but no receipt: run `verger install` or `verger remove`")
	default:
		doc.Status = StatusMissing
		doc.Blockers = append(doc.Blockers, "no receipt and no lock cell for this package/host")
	}
}

// whySpecReasons reports the spec entry state of one package.
func whySpecReasons(paths Paths, id string) []string {
	doc, ok, err := LoadSpec(paths.SpecPath)
	if err != nil || !ok {
		return nil
	}

	var reasons []string

	for _, pkg := range doc.Packages {
		if pkg.ID != id && !matchesID(pkg.ID, id) {
			continue
		}

		reasons = append(reasons, "spec "+pkg.ID+" "+pkg.Version)

		if pkg.Version == "" {
			reasons = append(reasons, "the spec pins no version")
		}
	}

	return reasons
}

// appendHooksReason reports whether the hooks consent gate still waits for this
// package's current content hash.
func (c *Client) appendHooksReason(id string, doc *WhyDocument) error {
	store, err := c.ConsentStore()
	if err != nil {
		return err
	}

	if record, ok := store.Hooks(id); ok {
		doc.Reasons = append(doc.Reasons, "hooks approved at "+record.Version)

		return nil
	}

	doc.Reasons = append(doc.Reasons, "hooks: no approval recorded")

	return nil
}

// Host maturity levels (DESIGN §10.3): a host is `experimental` until a green
// e2e run promotes it to `beta`, and a full cycle on a live machine — install,
// oracle, remove, adopt — to `stable`. A host verger cannot deliver to at all
// never rises above `experimental`, whatever else is true of it.
const (
	levelExperimental = "experimental"
	levelBeta         = "beta"
	levelStable       = "stable"
)

// hostLevels is the evidence table: what each adapter has actually proven, and
// the run that proves it. It moved here with the rest of the facade because a
// level is part of what `status` reports, not a CLI rendering detail.
//
// An id missing from this map is `experimental`: landing an adapter and nothing
// else cannot promote it, only a green run can. Adding a row here is therefore
// a claim that the named run happened, and the comment must name it.
var hostLevels = map[host.ID]string{
	// Full cycle green on a live machine: `docs/TASKS.md` T1.13 records
	// "live claude local+remote+adopt+negative green" for claude 2.1.283, and
	// the `omp` row records "e2e green locally on omp 18.4.1".
	host.Claude: levelStable,
	host.Omp:    levelStable,

	// Both e2e scenarios green on codex-cli 0.157.1
	// (docs/reviews/T1.7-T1.8-grammar-fix-claude.e2e.log:26,49). The adopt leg
	// enabled since that run has not run on a live machine, so not stable yet.
	host.Codex: levelBeta,

	// gemini is deliberately absent: its only recorded run failed on the
	// host-owned integrity store (the same log:75), and that policy is now
	// settled (docs/e2e.md, "Host-owned files"). A green run promotes it.
}

// HostMaturity is the level of one host id as it appears in a receipt or a
// lock cell. An id verger knows nothing about is `experimental`, not an error:
// the id is the caller's, the level is the safest true statement about it.
func HostMaturity(id string) string {
	if level, ok := hostLevels[host.ID(id)]; ok {
		return level
	}

	return levelExperimental
}
