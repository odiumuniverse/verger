package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The user-facing vocabulary (W7-UX-SPEC §1). A user acts on five words and
// nothing else:
//
//	delivered  we wrote it and the disk matches
//	skipped    we did not write it, and the reason is on the same line
//	blocked    a rule refused it
//	your edit  it is there, it is not what we wrote, and we left it alone
//	not ours   it is there and we never wrote it
//	failed     we tried and it broke — deliberately kept separate from
//	           skipped, so a bug never hides behind a normal outcome
//
// The internal status strings stay exactly as they are: they are what
// `--json` emits, and changing them is a breaking change for every consumer
// (W7-UX-SPEC §2.2 leaves that decision to the orchestrator). This file is
// the only place the two vocabularies meet, and it meets them in one
// direction: internal in, human out.

// The five words, plus three that are outcomes of a run rather than states
// of a cell and so must not be merged into a state: `failed` (never
// "skipped"), `removed` (it was delivered and is now gone, so "delivered"
// would claim the opposite), and `restored` (it came from the lock rather
// than from this machine, so "delivered" would claim work that never ran
// here).
const (
	wordDelivered = "delivered"
	wordSkipped   = "skipped"
	wordBlocked   = "blocked"
	wordRemoved   = "removed"
	wordRestored  = "restored"
	wordYourEdit  = "your edit"
	wordNotOurs   = "not ours"
	wordFailed    = "failed"
)

// state is one status rendered for a human: the word that goes in the STATUS
// column, and the reason that goes in the footnote block.
type state struct {
	word   string
	reason string
}

// cellState maps one internal status to the user-facing word and the exact
// detail that produced it. It is the only place the two vocabularies meet.
//
// The mapping is deliberately not one-to-one. `missing` (declared, never
// delivered) and `skew` (delivered at the wrong version) are both
// `skipped` to a user — nothing is blocked and nothing was edited — and
// `detail` is what lets a consumer tell them apart without parsing prose.
// `skew` is emphatically *not* a user edit: the file is ours, at an old
// version, and the fix is a re-run, not a decision about someone's edit.
//
// `hands-off` is the one genuinely ambiguous internal code: it covers both
// "the content on disk moved under us, so we refused" and "a rule stopped
// us". Those are different user actions, so the note decides. An unknown
// status is reported as failed rather than guessed at — a status this build
// does not know is exactly the case where assuming health would hide a
// problem.
func cellState(status string, notes []string) (word, detail string) {
	switch status {
	case "current":
		return wordDelivered, "current"
	case "missing":
		return wordSkipped, "missing"
	case "skew":
		return wordSkipped, "skew"
	case "planned":
		return wordSkipped, "planned"
	case "removed":
		return wordRemoved, "removed"
	case "skipped":
		// The delivery ran and wrote nothing on purpose: the package had
		// nothing for this host, or all of it was already there. The reason
		// travels in the cell's note, and the footnote block prints the same
		// sentence every other skipped row gets.
		return wordSkipped, "skipped"
	case "foreign":
		return wordNotOurs, "foreign"
	case "needs-auth", "needs-runtime":
		return wordBlocked, status
	case "failed":
		return wordFailed, "failed"
	case "hands-off":
		if hasDriftNote(notes) {
			return wordYourEdit, "hands-off"
		}

		return wordBlocked, "hands-off"
	default:
		return wordFailed, status
	}
}

// driftMarkers are the note fragments that mean "the content on disk is not
// what we wrote". They are matched loosely on purpose: the note is prose
// written for a human, and the question here is only which of two very
// different user actions the cell implies.
var driftMarkers = []string{"drift", "differs", "modified", "changed", "not the one we wrote"}

// hasDriftNote reports whether a cell's notes say the file drifted.
func hasDriftNote(notes []string) bool {
	for _, note := range notes {
		lower := strings.ToLower(note)

		for _, marker := range driftMarkers {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}

	return false
}

// humanState renders one already-mapped cell for the human table: the word,
// plus the reason a footnote can carry. The detail travels with the word
// because it is the only way an unrecognised internal code can reach a
// human — a build that does not know a status must say which one it saw,
// not print a generic "something broke" and swallow the evidence.
func humanState(word, detail string) state {
	switch word {
	case wordDelivered:
		return state{word: wordDelivered, reason: "written and verified on this host"}
	case wordSkipped:
		return state{word: wordSkipped, reason: "not delivered on this machine; run `verger sync`"}
	case wordYourEdit:
		return state{word: wordYourEdit, reason: "the file is not the one we wrote; we left your copy alone"}
	case wordNotOurs:
		return state{word: wordNotOurs, reason: "this file was not written by verger, so it was left alone"}
	case wordBlocked:
		return state{word: wordBlocked, reason: "a rule refused this; the note above says which"}
	case wordRemoved:
		return state{word: wordRemoved, reason: "removed by this run; the files are in the trash"}
	case wordRestored:
		return state{word: wordRestored, reason: "restored from the lock; no work ran on this machine"}
	case wordFailed:
		if detail != "" && detail != "failed" {
			return state{
				word:   wordFailed,
				reason: "we do not recognise the recorded state " + detail + "; see the note above",
			}
		}

		return state{word: wordFailed, reason: "we tried to write this and it broke; see the note above"}
	default:
		return state{word: wordFailed, reason: "we tried to write this and it broke; see the note above"}
	}
}

// strategyPhrase turns a delivery strategy into a sentence a user can read.
//
// W7-UX-SPEC §1.2 removes strategy words from the human table entirely: a
// user does not choose "synth" and cannot act on it. What they can act on is
// the consequence — who actually put the file there. The words themselves
// stay in `--json`.
func strategyPhrase(strategy string) string {
	switch strategy {
	case "native":
		return "installed by the host itself"
	case "synth", "loose":
		return "written as files"
	case "silenced":
		return "this host has no way to take it, so nothing was written"
	case "":
		return ""
	default:
		return "written as files"
	}
}

// footnotes collects the distinct reasons of a run and prints them numbered,
// each with the command that resolves it last. This is the format
// W7-UX-SPEC §1.3 asks for: the table stays scannable, and the reason is one
// glance away.
type footnotes struct {
	order  []string
	reason map[string]string
	marks  map[string]int
}

// newFootnotes returns an empty collector.
func newFootnotes() *footnotes {
	return &footnotes{reason: map[string]string{}, marks: map[string]int{}}
}

// mark records that a status occurred and returns its "[n]" marker, or an
// empty string when the word needs no explanation (delivered needs none).
func (f *footnotes) mark(word, detail string) string {
	st := humanState(word, detail)
	if st.reason == "" || st.word == wordDelivered {
		return ""
	}

	if _, seen := f.reason[st.reason]; !seen {
		f.reason[st.reason] = st.reason
		f.order = append(f.order, st.reason)
	}

	f.marks[st.reason] = len(f.order)

	return " [" + strconv.Itoa(f.marks[st.reason]) + "]"
}

// write prints the numbered reason block, if any reason was collected.
func (f *footnotes) write(out io.Writer) {
	for i, reason := range f.order {
		_, _ = fmt.Fprintf(out, "  [%d] %s\n", i+1, reason)
	}
}
