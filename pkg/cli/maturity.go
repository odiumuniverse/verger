package cli

import (
	"github.com/odiumuniverse/verger/pkg/host"
)

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
// the run that proves it. It lives in pkg/cli and not in pkg/host because the
// host interface is a delivery contract while a level is a statement about the
// evidence behind one adapter — and because §10.3 wants the level visible in
// the surfaces that report on hosts (status today, TUI with Ф2).
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

// hostMaturity is the level of one host id as it appears in a receipt or a
// lock cell. An id verger knows nothing about is `experimental`, not an error:
// the id is the caller's, the level is the safest true statement about it.
func hostMaturity(id string) string {
	if level, ok := hostLevels[host.ID(id)]; ok {
		return level
	}

	return levelExperimental
}
