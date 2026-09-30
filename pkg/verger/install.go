package verger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/store"
)

// PlannedPackage is one fetched package of a plan.
type PlannedPackage struct {
	Ref     source.Ref   `json:"ref"`
	Package host.Package `json:"package"`
	// Target is the one host this package is delivered to. Empty means every
	// adapter in the plan; an adopt sets it, because an adopted package is
	// already installed on the host it was adopted from and must not be
	// delivered there a second time.
	Target host.ID `json:"target,omitempty"`
	allow  bool    // hooks consent, resolved before Apply
	// Restored says this package had no receipt on this machine: what the
	// executor writes came from the lock, so it is a restore. The plan knows
	// this; the executor only carries it.
	Restored bool
	// Skip is why this package was planned but not delivered, in the words a
	// user would use to describe it. It is empty for a package that goes
	// through, which is why it is a reason rather than a bool: a plan that
	// skips something has to say which thing it skipped and why, and a bare
	// "skipped" leaves the reader to guess between the reasons.
	Skip string
}

// targets returns the adapters one planned package is delivered to.
func (p PlannedPackage) targets(adapters []host.Host) []host.Host {
	if p.Target == "" {
		return adapters
	}

	for _, adapter := range adapters {
		if adapter.ID() == p.Target {
			return []host.Host{adapter}
		}
	}

	return nil
}

// PlanOptions describe what an install should fetch and deliver.
type PlanOptions struct {
	// Paths is the scope to plan for.
	Paths Paths
	// Refs are the refs as the caller wrote them; an agent ref (`host:ref`)
	// becomes an adopt instead of a fetch.
	Refs []string
	// Hosts is an explicit adapter list; an empty list means the detected hosts
	// Filter selects.
	Hosts []host.Host
	// Filter narrows the detected adapters when Hosts is empty — the `--hosts` /
	// `--except` of the CLI, as data.
	Filter HostFilter
	// Pin overrides the fetched version.
	Pin string
	// Scope is the trusted project root for a project-scope delivery.
	Scope string
}

// Plan is one executable plan, as data: the scope it runs in, the adapters it
// targets, the cells a caller renders before anything is written, and the
// actions the executor will run (DESIGN §9.1). A caller renders Plan.Cells,
// asks its own Confirmer, and hands the plan back to Apply.
type Plan struct {
	Paths    Paths            `json:"-"`
	Cells    []Cell           `json:"cells"`
	Notes    []string         `json:"notes,omitempty"`
	Packages []PlannedPackage `json:"-"`
	Adopts   []AdoptPlan      `json:"adopts,omitempty"`
	Adapters []host.Host      `json:"-"`
	// doc is the spec this plan was built against. [propagate] is a property
	// of the document, not of a package row, so the install path can only
	// honour it while the document is in hand; a front end assembling a Plan
	// by hand gets the default "all" and nothing is silently narrowed.
	doc *spec.Spec
	Dry bool `json:"dry_run,omitempty"`
	// Actions are the executor's operations. The operation that owns the plan
	// (install, remove, sync) builds them; only Apply runs them.
	Actions []apply.Action `json:"-"`
}

// AdoptPlan is one agent-installed package adopted into the spec and delivered
// to the other detected hosts (D4).
type AdoptPlan struct {
	Ref         source.Ref `json:"ref"`
	ID          string     `json:"id"`
	AdoptedFrom string     `json:"adopted_from"`
	Notes       []string   `json:"notes,omitempty"`
}

// Plan resolves the refs of one install: it fetches every fetchable ref, picks
// the strategy ladder per host, and adopts the agent refs. It writes nothing.
func (c *Client) Plan(ctx context.Context, opts PlanOptions) (*Plan, error) {
	// A lock or spec written by a newer verger is reported before anything
	// else, so an install can never plan against a home it cannot read.
	if err := c.checkSchemaVersions(opts.Paths); err != nil {
		return nil, err
	}

	adapters, err := c.planAdapters(opts)
	if err != nil {
		return nil, err
	}

	if len(adapters) == 0 {
		return nil, &HostUnavailableError{}
	}

	agentRefs, fetchRefs, err := splitInstallRefs(opts.Refs)
	if err != nil {
		return nil, err
	}

	if err := c.RequireTrust(opts.Paths); err != nil {
		return nil, err
	}

	doc, _, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Paths: opts.Paths, Adapters: adapters, doc: doc}

	if len(agentRefs) > 0 {
		adopts, adoptErr := c.planAdopts(ctx, agentRefs, plan)
		if adoptErr != nil {
			return nil, adoptErr
		}

		plan.Adopts = adopts

		return plan, nil
	}

	if err := refuseUnsupportedRefs(fetchRefs); err != nil {
		return nil, err
	}

	packages, err := c.fetchPackages(ctx, fetchRefs, opts, adapters)
	if err != nil {
		return nil, err
	}

	packages, err = c.dropDisabled(opts.Paths, packages)
	if err != nil {
		return nil, err
	}

	plan.Packages = packages
	plan.Cells = plannedCells(packages, adapters, opts.Paths.Scope)

	if err := requireVersions(plan.Packages); err != nil {
		return nil, err
	}

	return plan, nil
}

// splitInstallRefs parses the refs an install was given and separates the two
// kinds. An agent ref adopts something a host already has and a package ref
// fetches one; the two answer to different machinery, and an install that
// mixed them would have to guess which a given ref wanted. A typo is a usage
// error, because the user can fix it by typing again.
func splitInstallRefs(raw []string) (agent, fetch []source.Ref, err error) {
	refs, err := source.ParseAll(raw)
	if err != nil {
		return nil, nil, &UsageError{Cause: err}
	}

	agent, fetch = splitRefs(refs)
	if len(agent) > 0 && len(fetch) > 0 {
		return nil, nil, &UsageError{Cause: errors.New("install cannot mix agent refs (host:ref) with package refs")}
	}

	return agent, fetch, nil
}

// dropDisabled removes the packages the spec has turned off. The sync path
// has always done this; install did not, so `verger install` wrote a package
// the user had explicitly disabled — and said so in the spec, one line
// above. A disabled entry is a decision, not a hint, and a command that
// plans around it is a second way to undo it silently.
func (c *Client) dropDisabled(paths Paths, packages []PlannedPackage) ([]PlannedPackage, error) {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return nil, err
	}

	disabled := map[string]bool{}

	for _, entry := range doc.Packages {
		if entry.Disabled {
			disabled[entry.ID] = true
		}
	}

	if len(disabled) == 0 {
		return packages, nil
	}

	kept := make([]PlannedPackage, 0, len(packages))

	for _, item := range packages {
		if disabled[item.Package.ID] {
			continue
		}

		kept = append(kept, item)
	}

	return kept, nil
}

// requireVersions refuses a plan whose package has no version. The executor
// checks the same thing, but checking it there means the user has already
// read a table of hosts that are about to be written before the run fails on
// a fact the plan could have known: a manifest that carries no version is a
// bad package, and a bad package is not something to discover mid-apply.
func requireVersions(packages []PlannedPackage) error {
	for _, item := range packages {
		if item.Package.Version == "" {
			return &UsageError{Cause: fmt.Errorf("package %s: the manifest declares no version",
				item.Package.ID)}
		}
	}

	return nil
}

// plannedCells renders the display plan of one install without writing.
func plannedCells(packages []PlannedPackage, adapters []host.Host, scope Scope) []Cell {
	var planned []Cell

	for _, item := range packages {
		for _, adapter := range adapters {
			strategy, note := PickStrategy(item.Package, adapter.ID(), item.Ref.Kind)

			cell := Cell{
				Package: item.Package.ID, Host: string(adapter.ID()), Scope: string(scope),
				Status: StatusPlanned, Version: item.Package.Version, Strategy: string(strategy), Kind: string(apply.ActionInstall),
			}

			if note != "" {
				cell.Notes = append(cell.Notes, note)
			}

			planned = append(planned, cell)
		}
	}

	return planned
}

// planAdapters returns the adapters one plan targets: the caller's list, else
// every detected host.
func (c *Client) planAdapters(opts PlanOptions) ([]host.Host, error) {
	if len(opts.Hosts) > 0 {
		return opts.Hosts, nil
	}

	return c.Targets(opts.Filter)
}

// refuseUnsupportedRefs reports a ref kind the build cannot resolve yet, naming
// the task that will.
func refuseUnsupportedRefs(refs []source.Ref) error {
	for _, ref := range refs {
		if ref.Kind == source.KindMCP {
			return &NotAvailableError{
				Feature: "mcp: refs",
				Hint:    "mcp: refs resolve through the official MCP Registry in the source resolver (T3.1)",
			}
		}
	}

	return nil
}

// specSourceFetcher resolves one ref through the sources the spec declares.
// It returns nil, nil when no declared source offers the id, which is the
// caller's cue to refuse the ref rather than look for it elsewhere.
func specSourceFetcher(ctx context.Context, st *store.Store, sources []spec.Source, ref source.Ref) (*source.Fetched, []SourceOffer, error) {
	fetcher, err := source.NewFetcher(source.WithStore(st))
	if err != nil {
		return nil, nil, err
	}

	// What each source actually offered is kept, not discarded: a ref no source
	// carries is otherwise a bare "not found", and the user has no way to tell a
	// typo from a source simply pointed at the wrong place.
	offered := make([]SourceOffer, 0, len(sources))

	for _, src := range sources {
		offers, offerErr := offerFromSource(ctx, fetcher, src, ref.ID)
		if offerErr != nil {
			return nil, nil, offerErr
		}

		offer := SourceOffer{Name: src.Name, URL: src.URL}

		for _, got := range offers {
			if got.Package.ID == ref.ID {
				return got, nil, nil
			}

			// A source that offered something else is not the package the
			// user named, and its cache entry must not outlive the probe.
			offer.IDs = append(offer.IDs, got.Package.ID)
			_ = got.Cleanup()
		}

		offered = append(offered, offer)
	}

	return nil, offered, nil
}

// offerFromSource asks one declared source what it offers. A source whose
// location cannot be read offers nothing rather than failing the install: a
// broken entry in the spec should not read as "this package is missing" when
// a later source may well have it.
func offerFromSource(ctx context.Context, fetcher *source.Fetcher, src spec.Source, id string) ([]*source.Fetched, error) {
	if src.Name == "" || src.URL == "" {
		return nil, nil
	}

	srcRef, err := source.ParseWithBase(src.URL, "")
	if err != nil {
		return nil, nil //nolint:nilerr // an unreadable source offers nothing
	}

	offers, err := fetcher.FetchLocalOffers(ctx, srcRef)
	if err != nil {
		return nil, nil //nolint:nilerr // see above
	}

	return offers, nil
}

// UndeclaredSourceError reports a ref that no declared source offers. The
// install refuses it instead of falling back to a default registry: the spec
// said where its packages come from, and going somewhere else — silently, and
// over the network — is how a name the user controls turns into a name
// somebody else publishes.
type UndeclaredSourceError struct {
	Ref     string
	Sources []string
	// Offered is what each declared source was asked for and what it had. A
	// bare list of source names tells the user nothing they can act on: "not
	// offered by a, b" says the same whether the sources are empty, hold
	// different packages, or were never readable at all - and those are three
	// different mistakes with three different fixes.
	Offered []SourceOffer
}

// SourceOffer is what one declared source was asked for and what it carried.
type SourceOffer struct {
	Name string
	URL  string
	// IDs are the packages the source actually offered. Empty alongside a URL
	// means the location could not be read, which reads differently from a URL
	// that was read and held nothing.
	IDs []string
}

// Error implements error.
func (e *UndeclaredSourceError) Error() string {
	if len(e.Sources) == 0 {
		return "no source declares " + e.Ref
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s is not offered by any source the spec declares:", e.Ref)

	for _, offer := range e.Offered {
		switch {
		case len(offer.IDs) > 0:
			fmt.Fprintf(&b, "\n  %s (%s) offers: %s", offer.Name, offer.URL, strings.Join(offer.IDs, ", "))
		case offer.URL == "":
			fmt.Fprintf(&b, "\n  %s offers: nothing (the spec gives it no url)", offer.Name)
		default:
			fmt.Fprintf(&b, "\n  %s (%s) offers: nothing readable", offer.Name, offer.URL)
		}
	}

	return b.String()
}

// fetchPackages fetches every ref into one host package.
func (c *Client) fetchPackages(ctx context.Context, refs []source.Ref, opts PlanOptions, adapters []host.Host) ([]PlannedPackage, error) {
	fetcher, err := source.NewFetcher(source.WithStore(c.Store()))
	if err != nil {
		return nil, err
	}

	sources := declaredSources(opts.Paths)

	// The channel must be on the ref before resolve fetches it. This is the one
	// point every install, update and plan goes through, so wiring it here
	// covers all three paths rather than three times.
	sp, _, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return nil, err
	}

	applyChannels(sp, refs)

	var packages []PlannedPackage

	for _, ref := range refs {
		fetched, err := c.resolve(ctx, fetcher, sources, ref)
		if err != nil {
			return nil, err
		}

		defer func() { _ = fetched.Cleanup() }()

		version := firstNonEmpty(opts.Pin, fetched.Package.Version)
		fetched.Package.Version = version

		pkg, err := c.buildHostPackage(fetched, adapters[0].ID(), version, string(opts.Paths.Scope), opts.Paths.Project)
		if err != nil {
			return nil, err
		}

		packages = append(packages, PlannedPackage{Ref: ref, Package: pkg})
	}

	return packages, nil
}

// declaredSources reads the sources a spec declares. A spec that cannot be
// read declares nothing, which leaves ref resolution exactly as it was.
func declaredSources(paths Paths) []spec.Source {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return nil
	}

	return doc.Sources
}

// resolve fetches one ref, going through the spec's declared sources first.
//
// A bare `owner/name` is the one ref shape that has to guess: the grammar
// reads it as GitHub, and that guess sent an install to github.com for a
// package the spec had already said lived somewhere else. When the spec
// declares sources, the guess is replaced by them, and a ref none of them
// offers is refused rather than fetched from the default registry — a name
// the user controls must not turn into somebody else's package.
func (c *Client) resolve(ctx context.Context, fetcher *source.Fetcher, sources []spec.Source, ref source.Ref) (*source.Fetched, error) {
	if len(sources) == 0 || !bareID(ref) {
		return fetchResolved(ctx, fetcher, ref)
	}

	got, offered, err := specSourceFetcher(ctx, c.Store(), sources, ref)
	if err != nil {
		return nil, err
	}

	if got == nil {
		names := make([]string, 0, len(sources))

		for _, src := range sources {
			names = append(names, src.Name)
		}

		return nil, &UndeclaredSourceError{Ref: ref.ID, Sources: names, Offered: offered}
	}

	return got, nil
}

// bareID reports whether ref is an unqualified `owner/name`, the shape that
// parses as GitHub but names nothing about where it lives. An explicit
// `github:` prefix, a URL, a path and an npm specifier all say where they
// come from, so they keep their own kind.
func bareID(ref source.Ref) bool {
	if ref.Kind != source.KindGitHub || ref.ID == "" || ref.Raw != ref.ID {
		return false
	}

	// `owner/name` and nothing else. A colon means the ref names a scheme of
	// its own — `github:owner/name`, and the canonical `local:name` id verger
	// gives a locally installed package, which is a fact about the package
	// rather than a place to fetch it from. Both are self-describing and both
	// must reach their own kind.
	if strings.Contains(ref.Raw, ":") {
		return false
	}

	owner, name, ok := strings.Cut(ref.Raw, "/")

	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

// Apply executes one plan (DESIGN §9.1). It is the single entry point a second
// front end codes against: the trust gate, the executor and the progress events
// all live here, so `Install`, `Remove` and `Adopt` differ only in what they put
// in the plan and what they record around it.
func (c *Client) Apply(ctx context.Context, plan *Plan, opts ApplyOptions) (*apply.Report, error) {
	if err := c.RequireTrust(plan.Paths); err != nil {
		return nil, err
	}

	adapters := plan.Adapters
	if len(adapters) == 0 {
		adapters = c.Hosts()
	}

	deps, err := c.applyDeps(adapters, plan.Paths)
	if err != nil {
		return nil, err
	}

	// Progress events are bridged here rather than handed to pkg/apply as is:
	// apply speaks its own vocabulary (a step name), the facade speaks the one
	// a second front end renders from (GAP-4).
	var (
		source = make(chan apply.Event, eventBuffer)
		stop   func()
	)

	if opts.Events != nil {
		stop = relayEvents(ctx, source, opts.Events)
	}

	execOpts := c.execOptions(opts)
	execOpts.Events = source

	report, err := apply.Run(ctx, deps, apply.Plan{Actions: plan.Actions}, execOpts)

	if stop != nil {
		stop()
	}

	if err != nil {
		return nil, err
	}

	report.Notes = append(report.Notes, plan.Notes...)

	return &report, nil
}

// eventBuffer is how many progress events a slow consumer may fall behind
// before the executor blocks on it.
const eventBuffer = 64

// relayEvents forwards executor events onto a caller's channel as facade
// events, and returns the function that closes the relay. The executor never
// closes its channel (DESIGN: "never closed by apply"), so the relay owns the
// close and stops exactly once the run is over.
func relayEvents(ctx context.Context, source chan apply.Event, sink chan<- Event) func() {
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-source:
				if !open {
					return
				}

				select {
				case sink <- Event{
					Kind:    EventStep(event.Step),
					Package: event.Package,
					Host:    string(event.Host),
					Message: event.Message,
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return func() {
		close(source)

		<-done
	}
}

// EventStep maps one executor step name onto the facade's event vocabulary.
func EventStep(step string) EventKind {
	switch step {
	case "plan":
		return EventPlanned
	case "install", "remove", "verify", "receipt":
		return EventApplied
	case "rollback":
		return EventSkipped
	case "lock":
		// The lock write belongs to the run, not to a cell.
		return EventNote
	default:
		return EventNote
	}
}

// Install runs a whole install: it resolves the hooks consent, builds the
// actions, applies them and records the spec (D3). The caller renders
// Plan.Cells before calling it, so the plan is on screen before anything is
// written; the execution itself is Client.Apply, the one entry point.
func (c *Client) Install(ctx context.Context, plan *Plan, opts ApplyOptions) (*apply.Report, error) {
	if err := c.RequireTrust(plan.Paths); err != nil {
		return nil, err
	}

	if len(plan.Adopts) > 0 {
		return c.installAdopted(ctx, plan, opts)
	}

	if !opts.DryRun && len(plan.Packages) > 0 {
		if err := c.Ensure(plan.Paths); err != nil {
			return nil, err
		}
	}

	if err := c.buildInstallActions(ctx, plan, opts); err != nil {
		return nil, err
	}

	report, err := c.Apply(ctx, plan, opts)
	if err != nil {
		return nil, err
	}

	if !opts.DryRun && len(plan.Packages) > 0 {
		if err := c.recordInstall(plan.Paths, plan.Packages); err != nil {
			return nil, err
		}
	}

	// A cell the executor refused is not a quiet success. This is the same
	// verdict Sync returns, for the same reason: the run printed "nothing
	// was written" and a caller that reads only the exit code would call
	// that a completed install. The report comes back alongside the error,
	// because the error names the situation and the report names the cell.
	if refused := refusedCells(report); len(refused) > 0 {
		return report, &HandsOffError{Cells: refused}
	}

	return report, nil
}

// installAdopted records the adopted spec and then delivers the plan the adopt
// built for the other hosts.
func (c *Client) installAdopted(ctx context.Context, plan *Plan, opts ApplyOptions) (*apply.Report, error) {
	applyOpts := opts
	applyOpts.Hooks = HooksSkip // an adopted delivery never brings hooks (D4)

	if err := c.CommitAdopt(plan.Paths, plan.Adopts, opts.DryRun); err != nil {
		return nil, err
	}

	if len(plan.Packages) == 0 {
		// An adoption is a delivery, even when there is no other host to
		// deliver to: the package is now the spec's, recorded with
		// `adopted_from`. Reporting it as a silent spec edit returned a
		// report with no cells, and `adopt --json` printed an empty matrix
		// for a run that had taken a package over.
		report := apply.Report{Notes: plan.Notes, Cells: adoptedCells(plan.Adopts)}

		return &report, nil
	}

	if err := c.buildInstallActions(ctx, plan, applyOpts); err != nil {
		return nil, err
	}

	return c.Apply(ctx, plan, applyOpts)
}

// adoptedCells renders the adoptions of a plan as report cells, so a run that
// only adopted still says what it took over.
func adoptedCells(adopts []AdoptPlan) []apply.CellResult {
	if len(adopts) == 0 {
		return nil
	}

	cells := make([]apply.CellResult, 0, len(adopts))

	for _, adopt := range adopts {
		cells = append(cells, apply.CellResult{
			Package: adopt.ID,
			Host:    host.ID(adopt.AdoptedFrom),
			Scope:   string(User),
			Status:  apply.StatusCurrent,
			Kind:    apply.ActionAdopt,
			Notes:   adopt.Notes,
		})
	}

	return cells
}

// buildInstallActions resolves the hooks consent and builds one install action
// per planned cell. It fills plan.Actions; only Apply runs them.
func (c *Client) buildInstallActions(ctx context.Context, plan *Plan, opts ApplyOptions) error {
	adapters := plan.Adapters

	// Removals a reconcile already planned stay in the plan; this only adds the
	// installs, so a sync does not lose the removals it found.
	actions := slices.Clone(plan.Actions)

	for i := range plan.Packages {
		item := &plan.Packages[i]

		allow, err := c.HooksDecision(item.Package, opts.Hooks, opts.Confirm, opts.DryRun)
		if err != nil {
			return err
		}

		item.allow = allow

		allowed, origin := propagateTargets(plan, item, adapters)

		for _, adapter := range allowed {
			if !opts.Switches.HooksOn(adapter.ID()) {
				allow = false
			}

			strategy, _ := PickStrategy(item.Package, adapter.ID(), item.Ref.Kind)

			pkg, _, err := c.prepareDelivery(ctx, item.Package, strategy)
			if err != nil {
				return err
			}

			actions = append(actions, apply.Action{
				Kind: apply.ActionInstall, Host: adapter.ID(), Restored: item.Restored,
				Delivery: host.Delivery{
					Package: pkg, Strategy: strategy, AllowHooks: allow, DryRun: opts.DryRun,
					Project: opts.Scope,
				},
			})
		}

		for _, skipped := range skippedHosts(allowed, origin, adapters) {
			plan.Notes = append(plan.Notes, item.Package.ID+": "+string(skipped)+
				" is not written here: [propagate] install = origin keeps this package at the origin host")
		}
	}

	plan.Actions = apply.DedupeActions(actions)

	return nil
}

// propagateTargets narrows one planned package's hosts by the effective
// [propagate] policy. "origin" keeps the package where the event happened and
// writes nothing elsewhere; "ask" writes nothing unattended, because the
// question belongs to a person and a plan is not a person. Without the spec
// document the default is "all" and nothing is narrowed.
func propagateTargets(plan *Plan, item *PlannedPackage, adapters []host.Host) (allowed []host.Host, origin host.ID) {
	candidates := item.targets(adapters)
	if len(candidates) == 0 || plan.doc == nil {
		return candidates, ""
	}

	var entry *spec.Package

	for i := range plan.doc.Packages {
		if plan.doc.Packages[i].ID == item.Package.ID {
			entry = &plan.doc.Packages[i]

			break
		}
	}

	for i, adapter := range candidates {
		switch plan.doc.EffectiveMode(spec.EventInstall, "", string(adapter.ID()), entry) {
		case spec.ModeOrigin:
			// The first detected adapter is the origin: the order the plan
			// itself fixed, not the host's own idea of one.
			if i == 0 {
				origin = adapter.ID()

				return candidates[:1], origin
			}
		case spec.ModeAsk:
			return nil, ""
		case spec.ModeAll:
			// Every adapter takes it; nothing to narrow.
		}
	}

	return candidates, origin
}

// skippedHosts names the adapters a policy left out, so the plan says which
// hosts were considered and declined rather than looking as if they were
// never detected.
func skippedHosts(allowed []host.Host, origin host.ID, adapters []host.Host) []host.ID {
	kept := make(map[host.ID]bool, len(allowed))

	for _, adapter := range allowed {
		kept[adapter.ID()] = true
	}

	var skipped []host.ID

	for _, adapter := range adapters {
		if !kept[adapter.ID()] && adapter.ID() != origin {
			skipped = append(skipped, adapter.ID())
		}
	}

	return skipped
}

// planAdopts resolves the agent refs of one install: the source host's own
// oracle names the package, the spec records it, and every other detected host
// gets a delivery without hooks (D4).
func (c *Client) planAdopts(ctx context.Context, refs []source.Ref, plan *Plan) ([]AdoptPlan, error) {
	adapterByID := map[host.ID]host.Host{}

	for _, adapter := range plan.Adapters {
		adapterByID[adapter.ID()] = adapter
	}

	var adopts []AdoptPlan

	for _, ref := range refs {
		from, ok := adapterByID[host.ID(ref.Agent)]
		if !ok {
			return nil, &UsageError{Cause: fmt.Errorf("adopt: host %q is not available", ref.Agent)}
		}

		listed, err := from.Oracle().List(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s oracle: %w", ref.Agent, err)
		}

		entry, found := findInstalled(listed, ref.AgentRef)
		if !found {
			return nil, fmt.Errorf("%s: %s is not listed by the oracle", ref.Agent, ref.AgentRef)
		}

		id := firstNonEmpty(entry.Name, ref.AgentRef)
		notes := []string{fmt.Sprintf("adopted %s from %s (%s)", id, ref.Agent, entry.Version)}
		// A host whose listing carries no version (pi's resolved paths, dsh's
		// missing verb) cannot be re-delivered: the executor requires a version.
		// The adoption is still recorded, the delivery is skipped (W3-E2E10 F5).
		if entry.Version == "" {
			notes = append(notes, ref.Agent+" lists no version; recorded, not re-delivered")
			adopts = append(adopts, AdoptPlan{Ref: ref, ID: id, AdoptedFrom: ref.Agent, Notes: notes})

			continue
		}

		for _, adapter := range plan.Adapters {
			if adapter.ID() == from.ID() {
				continue
			}

			pkg := host.Package{
				ID: id, Version: entry.Version, Root: entry.Path,
				Marketplace: entry.Marketplace, Scope: string(plan.Paths.Scope), ProjectRoot: plan.Paths.Project,
			}

			strategy, note := PickStrategy(pkg, adapter.ID(), source.KindLocal)
			if note != "" {
				notes = append(notes, note)
			}

			delivered, deliveryNotes, err := c.prepareDelivery(ctx, pkg, strategy)
			if err != nil {
				return nil, err
			}

			notes = append(notes, deliveryNotes...)

			plan.Packages = append(plan.Packages, PlannedPackage{Ref: ref, Package: delivered, Target: adapter.ID()})
		}

		adopts = append(adopts, AdoptPlan{Ref: ref, ID: id, AdoptedFrom: ref.Agent, Notes: notes})
	}

	return adopts, nil
}

// Adopt takes packages the hosts already have (D4): it resolves each agent ref
// through the source host's own oracle, records where it came from and delivers
// it to the other detected hosts without hooks. It is the named entry point the
// farm migration drives (T4.4); `Plan` + `Install` are its two halves and remain
// available for a front end that wants to render the plan itself.
func (c *Client) Adopt(ctx context.Context, opts PlanOptions, applyOpts ApplyOptions) (*Plan, *apply.Report, error) {
	if len(opts.Refs) == 0 {
		return nil, nil, &UsageError{Cause: errors.New("adopt needs at least one host:ref")}
	}

	plan, err := c.Plan(ctx, opts)
	if err != nil {
		return nil, nil, err
	}

	if len(plan.Adopts) == 0 {
		return plan, &apply.Report{}, nil
	}

	report, err := c.Install(ctx, plan, applyOpts)
	if err != nil {
		return plan, nil, err
	}

	return plan, report, nil
}

// CommitAdopt records the adopted packages in the spec; a dry run keeps the
// plan in memory only.
func (c *Client) CommitAdopt(paths Paths, adopts []AdoptPlan, dryRun bool) error {
	if dryRun || len(adopts) == 0 {
		return nil
	}

	if err := c.Ensure(paths); err != nil {
		return err
	}

	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	for _, adopt := range adopts {
		if !hasSpecPackageID(doc, adopt.ID) {
			doc.Packages = append(doc.Packages, spec.Package{ID: adopt.ID, AdoptedFrom: adopt.AdoptedFrom})
		}
	}

	return SaveSpec(paths.SpecPath, doc)
}

// splitRefs separates agent refs (host:ref) from fetchable refs.
func splitRefs(refs []source.Ref) ([]source.Ref, []source.Ref) {
	var agentRefs, fetchRefs []source.Ref

	for _, ref := range refs {
		if ref.Agent != "" {
			agentRefs = append(agentRefs, ref)

			continue
		}

		fetchRefs = append(fetchRefs, ref)
	}

	return agentRefs, fetchRefs
}

// findInstalled locates one entry of a host's own listing by selector.
// The selector may carry a marketplace suffix (name@marketplace); the oracle
// splits it, so the comparison must too.
func findInstalled(listed []host.Installed, selector string) (host.Installed, bool) {
	name, marketplace, _ := strings.Cut(selector, "@")
	if name == "" {
		name = selector
	}

	for _, entry := range listed {
		if entry.Name != name {
			continue
		}

		if marketplace != "" && entry.Marketplace != "" && entry.Marketplace != marketplace {
			continue
		}

		return entry, true
	}

	return host.Installed{}, false
}

// hasSpecPackageID reports whether the spec already carries the id.
func hasSpecPackageID(doc *spec.Spec, id string) bool {
	for _, pkg := range doc.Packages {
		if pkg.ID == id {
			return true
		}
	}

	return false
}

// NotAvailableError reports a capability the build does not offer yet, with
// the hint a caller should print. It moved to the facade so a second front end
// maps it onto its own UI instead of matching on text.
type NotAvailableError struct {
	Feature string
	Hint    string
}

// Error implements error.
func (e *NotAvailableError) Error() string {
	message := e.Feature + " is not available yet"

	if e.Hint != "" {
		message += ": " + e.Hint
	}

	return message
}
