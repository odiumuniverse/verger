package verger

import (
	"cmp"
	"context"
	"slices"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// ImportOptions is one `verger import` request.
type ImportOptions struct {
	// Paths is the scope whose spec would be written.
	Paths Paths
	// Hosts narrows the question to the selected hosts. An empty filter is
	// every host this machine resolves, the same default `status` and
	// `search` use.
	Hosts HostFilter
}

// ImportCandidate is one natively installed package `import` would record, and
// where it saw it.
//
// The host is not decoration: it becomes the spec's `adopted_from`, which is the
// only thing that later tells a reader this line was written from what a host
// already had rather than from something verger fetched.
type ImportCandidate struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Host    string `json:"host"`
	// Disabled records what the host reported, so a package the user turned
	// off stays turned off instead of coming back on the next sync.
	Disabled bool `json:"disabled,omitempty"`
	// Notes carry what the user has to know about this entry before writing
	// it: a host that lists no version, a package two hosts had.
	Notes []string `json:"notes,omitempty"`
}

// ImportSkipped is one installed entry `import` will not record, and why. It
// is in the plan rather than in a log for the reason `search` keeps its skipped
// sources on screen: a package missing from a list the user did not know was
// short is the failure this type exists to prevent.
type ImportSkipped struct {
	Host   string `json:"host,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason"`
}

// ImportPlan is what `verger import` would write.
//
// It is a plan and not an action for the same reason `install` has one: the
// user sees the whole set before anything is written, and approves once.
type ImportPlan struct {
	Paths      Paths             `json:"-"`
	Candidates []ImportCandidate `json:"candidates"`
	Skipped    []ImportSkipped   `json:"skipped"`
	// AlreadyInSpec counts the installed entries the spec already declares.
	// They are counted rather than listed because they are not skipped and not
	// proposed: they are simply done, and a second run has nothing to say.
	AlreadyInSpec int `json:"already_in_spec"`
}

// Changed reports whether recording this plan would write anything at all.
func (p ImportPlan) Changed() bool {
	return len(p.Candidates) > 0
}

// ImportPlan collects what the detected hosts installed natively — the same
// list `adopt` reads, for every host at once and with no ref from the user.
//
// It writes nothing. Every host that could not answer is named in Skipped
// rather than failing the run, because a machine where one agent's CLI is
// broken still has five others full of packages the user wants written down,
// and an import that refused to run would be useless exactly when it is needed.
func (c *Client) ImportPlan(ctx context.Context, opts ImportOptions) (ImportPlan, error) {
	if err := ctx.Err(); err != nil {
		return ImportPlan{}, err
	}

	// A spec or lock written by a newer build is reported before anything is
	// read, for the same reason `status` does it: planning a write over a home
	// this build cannot read is the silent failure the check exists to stop.
	if err := c.checkSchemaVersions(opts.Paths); err != nil {
		return ImportPlan{}, err
	}

	doc, declared, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return ImportPlan{}, err
	}

	targets, err := c.Targets(opts.Hosts)
	if err != nil {
		return ImportPlan{}, err
	}

	declaredIDs := map[string]bool{}

	if declared {
		for _, pkg := range doc.Packages {
			declaredIDs[pkg.ID] = true
		}
	}

	plan := ImportPlan{
		Paths:      opts.Paths,
		Candidates: []ImportCandidate{},
		Skipped:    []ImportSkipped{},
	}

	// One row per id across every host: the same package on two hosts is one
	// package, and a spec carrying it twice is a document no command can read.
	byID := map[string]*ImportCandidate{}

	for _, adapter := range targets {
		c.collectHostInstalls(ctx, &plan, byID, adapter, declaredIDs)
	}

	for _, candidate := range byID {
		plan.Candidates = append(plan.Candidates, *candidate)
	}

	slices.SortFunc(plan.Candidates, func(left, right ImportCandidate) int {
		return cmp.Compare(left.ID, right.ID)
	})

	return plan, nil
}

// collectHostInstalls folds one host's listing into the plan. Every failure is
// recorded and the walk continues: the hosts are independent, and one broken
// CLI must not cost the user the packages the others have.
func (c *Client) collectHostInstalls(
	ctx context.Context,
	plan *ImportPlan,
	byID map[string]*ImportCandidate,
	adapter host.Host,
	declaredIDs map[string]bool,
) {
	name := string(adapter.ID())

	listed, err := adapter.Oracle().List(ctx)
	if err != nil {
		plan.Skipped = append(plan.Skipped, ImportSkipped{Host: name, Reason: err.Error()})

		return
	}

	for _, entry := range listed {
		// A host that lists an entry with no name has told us nothing a spec
		// line could carry, and a line with an empty id is a spec no command
		// can act on.
		if entry.Name == "" {
			plan.Skipped = append(plan.Skipped, ImportSkipped{
				Host:   name,
				Reason: "the host lists an entry with no name",
			})

			continue
		}

		if declaredIDs[entry.Name] {
			plan.AlreadyInSpec++

			continue
		}

		if existing, seen := byID[entry.Name]; seen {
			existing.Notes = append(existing.Notes, "also installed on "+name)

			continue
		}

		candidate := ImportCandidate{
			ID:       entry.Name,
			Version:  entry.Version,
			Host:     name,
			Disabled: !entry.Enabled,
		}

		// adopt records a version-less package and annotates it; import does
		// the same rather than dropping what the user already has.
		if entry.Version == "" {
			candidate.Notes = append(candidate.Notes,
				name+" lists no version; recorded, not re-deliverable until it does")
		}

		byID[entry.Name] = &candidate
	}
}

// RecordImports writes the plan's entries into the scope's spec and reports how
// many lines were added.
func (c *Client) RecordImports(plan ImportPlan) (int, error) {
	return RecordImportsAt(plan.Paths.SpecPath, plan)
}

// RecordImportsAt writes the plan's entries into the spec at path and reports
// how many lines were added.
//
// It goes through AddSpecPackage, the same path install and adopt write
// through, so the file comes out the same way whichever verb touched it last —
// and the bytes the user wrote stay where they were (SPEC-PRESERVE).
//
// The path is a parameter rather than read off the plan because `import
// --dry-run` has to apply this exact step to a throwaway copy: a preview
// produced by anything but the real writer is a second guess, and a second
// guess about the user's own file is how a dry run lies.
//
// A plan that would add nothing does not save: the second run of `import` has
// to leave the file exactly as it found it, not rewrite it identically.
func RecordImportsAt(path string, plan ImportPlan) (int, error) {
	if len(plan.Candidates) == 0 {
		return 0, nil
	}

	doc, _, err := LoadSpec(path)
	if err != nil {
		return 0, err
	}

	added := 0

	for _, candidate := range plan.Candidates {
		entry := spec.Package{
			ID:          candidate.ID,
			Version:     candidate.Version,
			Disabled:    candidate.Disabled,
			AdoptedFrom: candidate.Host,
		}

		if AddSpecPackage(doc, entry) {
			added++
		}
	}

	if added == 0 {
		return 0, nil
	}

	if err := SaveSpec(path, doc); err != nil {
		return 0, err
	}

	return added, nil
}
