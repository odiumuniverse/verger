package verger

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/source"
)

// SearchOptions is one `verger search` request.
type SearchOptions struct {
	Paths Paths
	Query string
	// Hosts selects which hosts' installs count as installed. Empty means the
	// resolved targets, the same default `status` uses.
	Hosts []string
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

	InSpec     bool   `json:"in_spec"`
	InLock     bool   `json:"in_lock"`
	Installed  bool   `json:"installed"`
	OfferedBy  string `json:"offered_by,omitempty"`
	CachedOnly bool   `json:"cached_only,omitempty"`
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

	res := SearchResult{Query: query, Matches: []SearchMatch{}}

	// A spec or lock written by a newer build is reported before anything is
	// read, for the same reason `status` does it: reporting "no matches" over
	// a home this build cannot read is the silent failure the check exists for.
	if err := c.checkSchemaVersions(opts.Paths); err != nil {
		return SearchResult{}, err
	}

	found := map[string]*SearchMatch{}

	touch := func(id string) *SearchMatch {
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

	// The spec: what the user asked to have, and the source list the offers
	// come from. One read, both uses.
	doc, declared, err := LoadSpec(opts.Paths.SpecPath)
	if err != nil {
		return SearchResult{}, err
	}
	if declared {
		for _, p := range doc.Packages {
			if m := touch(p.ID); m != nil {
				m.InSpec = true
			}
		}
	}

	// The lock: what was actually resolved and pinned.
	if lk, err := LoadLock(opts.Paths.LockPath); err != nil {
		return SearchResult{}, err
	} else if lk != nil {
		for _, cell := range lk.Cells {
			if m := touch(cell.Package); m != nil {
				m.InLock = true
			}
		}
	}

	// The receipts: what was delivered, to whom.
	if opts.Paths.ReceiptsDir != "" {
		rs, err := receipt.NewStore(opts.Paths.ReceiptsDir).List()
		if err != nil {
			return SearchResult{}, err
		}
		for _, r := range rs {
			if m := touch(r.Package); m != nil {
				m.Installed = true
				if m.Source == "" {
					m.Source = r.Host
				}
			}
		}
	}

	// The declared sources. A local one is read; every other kind is not, and
	// says so.
	if declared {
		fetcher, err := source.NewFetcher(source.WithStore(c.Store()))
		if err != nil {
			return SearchResult{}, err
		}
		for _, s := range doc.Sources {
			if s.Name == "" || s.URL == "" {
				continue
			}
			// The spec's own directory is the base, the same base the plan
			// resolves a declared source against: a relative `local:` entry
			// means the same thing in search as it does in install.
			ref, err := source.ParseWithBase(s.URL, filepath.Dir(opts.Paths.SpecPath))
			if err != nil {
				res.Skipped = append(res.Skipped, SearchSkipped{Source: s.Name, Reason: err.Error()})
				continue
			}
			// The one hard line. `FetchLocalOffers` refuses anything that is
			// not a local ref, and search does not go around it to fetch a
			// remote: a package missing from this list would otherwise read
			// as "no such package anywhere" when the truth is "we did not
			// ask anyone who would know".
			offers, err := fetcher.FetchLocalOffers(ctx, ref)
			if err != nil {
				res.Skipped = append(res.Skipped, SearchSkipped{Source: s.Name, Reason: err.Error()})
				continue
			}
			for _, offer := range offers {
				if offer == nil || offer.Package == nil {
					continue
				}
				// A local payload can carry a name and no id; it is still a
				// package the user could ask for by name, so it is indexed
				// by whichever of the two it has.
				id := offer.Package.ID
				if id == "" {
					id = offer.Package.Name
				}
				if m := touch(id); m != nil {
					m.OfferedBy = s.Name
					if m.Name == "" {
						m.Name = offer.Package.Name
					}
					if m.Description == "" {
						m.Description = offer.Package.Description
					}
				}
			}
		}
	}

	// The filter, applied last so every source contributed to the same map.
	needle := strings.ToLower(query)
	for _, m := range found {
		if !matches(needle, m) {
			continue
		}
		res.Matches = append(res.Matches, *m)
	}
	sort.Slice(res.Matches, func(i, j int) bool {
		return res.Matches[i].ID < res.Matches[j].ID
	})

	return res, nil
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
