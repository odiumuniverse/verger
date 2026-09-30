package verger

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// SearchOptions is one `verger search` request.
type SearchOptions struct {
	Paths Paths
	Query string
	// Hosts selects which hosts' installs count as installed. Empty means the
	// resolved targets, the same default `status` uses, and the filter is
	// resolved through Targets rather than applied to the receipts afterwards:
	// asking about a host this machine does not have has to be the same
	// refusal every other verb gives, not a quietly empty answer.
	Hosts HostFilter
}

// SearchMatch is one package the query found, and where it was found. The
// `In…` flags are the whole point: "do I already have this" is four different
// questions — is it in the spec, pinned in the lock, delivered, or merely
// offered — and collapsing them into a yes/no is how a user ends up installing
// something they had.
type SearchMatch struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"`

	InSpec    bool   `json:"in_spec"`
	InLock    bool   `json:"in_lock"`
	Installed bool   `json:"installed"`
	OfferedBy string `json:"offered_by,omitempty"`
}

// SearchSkipped is one declared source that search could not read without
// reaching the network, and why. It is in the result rather than in a log
// because the user is looking at a list: a package missing from a list they
// did not know was incomplete is the failure this type exists to prevent.
type SearchSkipped struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// SearchResult is one `verger search` answer.
type SearchResult struct {
	Query   string          `json:"query"`
	Matches []SearchMatch   `json:"matches"`
	Skipped []SearchSkipped `json:"skipped,omitempty"`
}

// Search filters the data verger already has on this machine: the spec, the
// lock, the receipts, and the offers of declared *local* sources. It never
// reaches the network — a remote source contributes only what the cache
// already holds, and is reported in Skipped so the answer is not silently
// partial.
//
// A match is a case-insensitive substring over the id, the name and the
// description. A user who remembers half a name must still find the package;
// exact matching would answer "no such package" to a question that had one.
//
// The answer comes in three steps, each a function of its own: collect every
// candidate this machine knows about, keep the ones the query names, then put
// them in a stable order. They fail differently, and a reader asking "why did
// it miss my package" needs to know which one to read.
func (c *Client) Search(ctx context.Context, opts SearchOptions) (SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return SearchResult{}, err
	}

	query := strings.TrimSpace(opts.Query)
	if query == "" {
		// An empty query is not "everything": a user who typed it has not
		// asked a question, and answering with the whole inventory teaches
		// them that the flag is a dump.
		return SearchResult{}, &UsageError{Cause: errors.New("search needs a query: verger search <text>")}
	}

	// A spec or lock written by a newer build is reported before anything is
	// read, for the same reason `status` does it: reporting "no matches" over
	// a home this build cannot read is the silent failure the check exists for.
	if err := c.checkSchemaVersions(opts.Paths); err != nil {
		return SearchResult{}, err
	}

	// Step one: every package verger can account for on this machine, whether
	// or not the query names it.
	found, skipped, err := c.collectCandidates(ctx, opts)
	if err != nil {
		return SearchResult{}, err
	}

	// Step two: the ones the query names.
	res := SearchResult{
		Query:   query,
		Matches: matchingCandidates(found, query),
		Skipped: skipped,
	}

	// Step three: a stable order, so two runs over one home print the same
	// list in the same order and a diff of two runs means something.
	rankMatches(res.Matches)

	return res, nil
}

// collectCandidates gathers every package this machine knows about into one
// row per package id, and records the declared sources it could not read.
//
// One map, not one list per source: a package the spec declares and a receipt
// has delivered comes back as one row carrying both facts, rather than as two
// rows the user has to reconcile into the answer they were looking for.
func (c *Client) collectCandidates(
	ctx context.Context,
	opts SearchOptions,
) (map[string]*SearchMatch, []SearchSkipped, error) {
	// The spec: what the user asked to have, and the source list the offers
	// come from. One read, both uses.
	doc, declared, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return nil, nil, err
	}

	selected, err := c.selectedHosts(opts.Hosts)
	if err != nil {
		return nil, nil, err
	}

	found := map[string]*SearchMatch{}
	skipped := []SearchSkipped{}

	indexSpec(found, doc, declared)

	if err := indexLock(found, opts.Paths.LockPath); err != nil {
		return nil, nil, err
	}

	if err := indexReceipts(found, opts.Paths.ReceiptsDir, selected); err != nil {
		return nil, nil, err
	}

	if !declared {
		return found, skipped, nil
	}

	if err := c.indexOffers(ctx, &skipped, found, doc, opts.Paths.SpecPath); err != nil {
		return nil, nil, err
	}

	return found, skipped, nil
}

// matchingCandidates keeps the rows the query names. It runs after the
// collection, never during it: a package the spec does not declare but a local
// source offers must still be findable by name, and a filter applied per source
// would have dropped it before the offer was read.
func matchingCandidates(found map[string]*SearchMatch, query string) []SearchMatch {
	needle := strings.ToLower(query)
	rows := []SearchMatch{}

	for _, m := range found {
		if !matches(needle, m) {
			continue
		}

		rows = append(rows, *m)
	}

	return rows
}

// rankMatches puts the rows in their stable order. It sorts in place because
// the caller owns the slice and there is nothing else referring to it.
func rankMatches(rows []SearchMatch) {
	slices.SortFunc(rows, func(left, right SearchMatch) int {
		return strings.Compare(left.ID, right.ID)
	})
}

// selectedHosts resolves the host filter into the set of hosts whose receipts
// count as installed. An empty filter is every host this machine resolves,
// which is the default `status` has always meant, so the common case cannot
// filter by accident.
func (c *Client) selectedHosts(filter HostFilter) (map[string]bool, error) {
	targets, err := c.Targets(filter)
	if err != nil {
		return nil, err
	}

	selected := make(map[string]bool, len(targets))
	for _, adapter := range targets {
		selected[string(adapter.ID())] = true
	}

	return selected, nil
}

// touchMatch returns the row of one package id, creating it on first sight.
// An empty id gets no row: a package with no id is nothing the user can ask
// for or install, and printing it would answer a question about nothing.
func touchMatch(found map[string]*SearchMatch, id string) *SearchMatch {
	if id == "" {
		return nil
	}

	m, ok := found[id]
	if !ok {
		m = &SearchMatch{ID: id}
		found[id] = m
	}

	return m
}

// indexSpec marks every package the spec declares.
func indexSpec(found map[string]*SearchMatch, doc *spec.Spec, declared bool) {
	if !declared {
		return
	}

	for _, p := range doc.Packages {
		if m := touchMatch(found, p.ID); m != nil {
			m.InSpec = true
		}
	}
}

// indexLock marks every package the lock resolved and pinned.
func indexLock(found map[string]*SearchMatch, lockPath string) error {
	lk, err := LoadLock(lockPath)
	if err != nil {
		return err
	}

	if lk == nil {
		return nil
	}

	for _, cell := range lk.Cells {
		if m := touchMatch(found, cell.Package); m != nil {
			m.InLock = true
		}
	}

	return nil
}

// indexReceipts marks every package a receipt says is delivered, to one of the
// selected hosts.
func indexReceipts(found map[string]*SearchMatch, receiptsDir string, selected map[string]bool) error {
	if receiptsDir == "" {
		return nil
	}

	list, err := receipt.NewStore(receiptsDir).List()
	if err != nil {
		return err
	}

	for _, r := range list {
		if !selected[r.Host] {
			continue
		}

		if m := touchMatch(found, r.Package); m != nil {
			m.Installed = true

			if m.Source == "" {
				m.Source = r.Host
			}
		}
	}

	return nil
}

// indexOffers reads the offers of the declared sources. Only a local one is
// read; every other kind is appended to skipped instead, so the list is never
// silently short of what the user declared.
func (c *Client) indexOffers(
	ctx context.Context,
	skipped *[]SearchSkipped,
	found map[string]*SearchMatch,
	doc *spec.Spec,
	specPath string,
) error {
	fetcher, err := source.NewFetcher(source.WithStore(c.Store()))
	if err != nil {
		return err
	}

	for _, s := range doc.Sources {
		if s.Name == "" || s.URL == "" {
			continue
		}

		c.indexSourceOffers(ctx, skipped, found, fetcher, s, specPath)
	}

	return nil
}

// indexSourceOffers indexes one declared source, or records why it could not
// be read.
func (c *Client) indexSourceOffers(
	ctx context.Context,
	skipped *[]SearchSkipped,
	found map[string]*SearchMatch,
	fetcher *source.Fetcher,
	declared spec.Source,
	specPath string,
) {
	// The spec's own directory is the base, the same base the plan resolves a
	// declared source against: a relative `local:` entry means the same thing
	// in search as it does in install.
	ref, err := source.ParseWithBase(declared.URL, filepath.Dir(specPath))
	if err != nil {
		*skipped = append(*skipped, SearchSkipped{Source: declared.Name, Reason: err.Error()})

		return
	}

	// The one hard line. `FetchLocalOffers` refuses anything that is not a
	// local ref, and search does not go around it to fetch a remote: a package
	// missing from this list would otherwise read as "no such package
	// anywhere" when the truth is "we did not ask anyone who would know".
	offers, err := fetcher.FetchLocalOffers(ctx, ref)
	if err != nil {
		*skipped = append(*skipped, SearchSkipped{Source: declared.Name, Reason: err.Error()})

		return
	}

	for _, offer := range offers {
		if offer == nil || offer.Package == nil {
			continue
		}

		indexOffer(found, offer.Package, declared.Name)
	}
}

// indexOffer records one offered package under the identifier it has.
func indexOffer(found map[string]*SearchMatch, pkg *manifest.Package, offeredBy string) {
	// A local payload can carry a name and no id; it is still a package the
	// user could ask for by name, so it is indexed by whichever of the two it
	// has.
	id := pkg.ID
	if id == "" {
		id = pkg.Name
	}

	m := touchMatch(found, id)
	if m == nil {
		return
	}

	m.OfferedBy = offeredBy

	if m.Name == "" {
		m.Name = pkg.Name
	}

	if m.Description == "" {
		m.Description = pkg.Description
	}
}

// matches is the case-insensitive substring test over the three fields a user
// can remember something by. The id is the one that is always present, so a
// package with no name is still findable.
func matches(needle string, m *SearchMatch) bool {
	for _, field := range []string{m.ID, m.Name, m.Description} {
		if field != "" && strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}

	return false
}
