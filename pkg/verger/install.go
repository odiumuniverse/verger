package verger

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
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
	Dry      bool             `json:"dry_run,omitempty"`
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
	refs, err := source.ParseAll(opts.Refs)
	if err != nil {
		return nil, &UsageError{Cause: err}
	}

	agentRefs, fetchRefs := splitRefs(refs)
	if len(agentRefs) > 0 && len(fetchRefs) > 0 {
		return nil, &UsageError{Cause: errors.New("install cannot mix agent refs (host:ref) with package refs")}
	}

	if err := c.RequireTrust(opts.Paths); err != nil {
		return nil, err
	}

	adapters, err := c.planAdapters(opts)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Paths: opts.Paths, Adapters: adapters}

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

	if len(adapters) == 0 {
		return nil, errors.New("no detected hosts; pass Hosts to target one explicitly")
	}

	packages, err := c.fetchPackages(ctx, fetchRefs, opts, adapters)
	if err != nil {
		return nil, err
	}

	plan.Packages = packages
	plan.Cells = plannedCells(packages, adapters, opts.Paths.Scope)

	return plan, nil
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

// fetchPackages fetches every ref into one host package.
func (c *Client) fetchPackages(ctx context.Context, refs []source.Ref, opts PlanOptions, adapters []host.Host) ([]PlannedPackage, error) {
	fetcher, err := source.NewFetcher(source.WithStore(c.Store()))
	if err != nil {
		return nil, err
	}

	var packages []PlannedPackage

	for _, ref := range refs {
		fetched, err := fetcher.Fetch(ctx, ref)
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
		report := apply.Report{Notes: plan.Notes}
		for _, adopt := range plan.Adopts {
			report.Notes = append(report.Notes, adopt.Notes...)
		}

		return &report, nil
	}

	if err := c.buildInstallActions(ctx, plan, applyOpts); err != nil {
		return nil, err
	}

	return c.Apply(ctx, plan, applyOpts)
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

		for _, adapter := range item.targets(adapters) {
			if !opts.Switches.HooksOn(adapter.ID()) {
				allow = false
			}

			strategy, _ := PickStrategy(item.Package, adapter.ID(), item.Ref.Kind)

			pkg, _, err := c.prepareDelivery(ctx, item.Package, strategy)
			if err != nil {
				return err
			}

			actions = append(actions, apply.Action{
				Kind: apply.ActionInstall, Host: adapter.ID(),
				Delivery: host.Delivery{
					Package: pkg, Strategy: strategy, AllowHooks: allow, DryRun: opts.DryRun,
				},
			})
		}
	}

	plan.Actions = actions

	return nil
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

// findInstalled locates one entry of a host's own listing by name.
func findInstalled(listed []host.Installed, name string) (host.Installed, bool) {
	for _, entry := range listed {
		if entry.Name == name {
			return entry, true
		}
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
