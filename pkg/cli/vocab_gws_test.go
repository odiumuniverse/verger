package cli

import (
	"bytes"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestHumanStateUsesTheUserVocabulary pins the mapping from every internal
// status to the five words a user can act on (W7-UX-SPEC §1). An unmapped
// status would leak a word like "hands-off" or "foreign" straight into the
// human table, which is exactly what the spec bans.
func TestHumanStateUsesTheUserVocabulary(t *testing.T) {
	Convey("Given every status the tool can record", t, func() {
		cases := []struct {
			status string
			word   string
		}{
			{"current", wordDelivered},
			{"missing", wordSkipped},
			{"skew", wordSkipped},
			{"planned", wordSkipped},
			{"removed", wordRemoved},
			{"foreign", wordNotOurs},
			{"needs-auth", wordBlocked},
			{"failed", wordFailed},
			{"skipped", wordSkipped},
		}

		for _, tc := range cases {
			Convey("When the internal status is "+tc.status, func() {
				Convey("Then it reads as "+tc.word, func() {
					word, _ := cellState(tc.status, nil)
					So(word, ShouldEqual, tc.word)
				})
			})
		}
	})
}

// TestHumanStateNeverLeaksInternalWords is the ban from W7-UX-SPEC §1.2:
// rung, cell, ladder, synth, tombstone, hands-off, canon, pivot, kind,
// surface, vault, foreign and drift must not reach a human.
func TestHumanStateNeverLeaksInternalWords(t *testing.T) {
	Convey("Given a status from any layer", t, func() {
		statuses := []string{
			"current", "missing", "skew", "planned", "removed", "skipped",
			"foreign", "needs-auth", "needs-runtime", "failed", "hands-off",
		}

		banned := []string{
			"rung", "cell", "ladder", "synth", "tombstone", "hands-off",
			"canon", "pivot", "kind", "surface", "vault", "foreign", "drift",
		}

		for _, status := range statuses {
			Convey("When the status is "+status, func() {
				word, detail := cellState(status, nil)
				rendered := humanState(word, detail).word + " " + humanState(word, detail).reason

				for _, bannedWord := range banned {
					Convey("Then the word "+bannedWord+" is absent", func() {
						So(strings.Contains(rendered, bannedWord), ShouldBeFalse)
					})
				}
			})
		}
	})
}

// TestUnknownStatusIsReportedNotHidden pins the default: a status this build
// does not recognise is reported as failed, never silently rendered as fine.
func TestUnknownStatusIsReportedNotHidden(t *testing.T) {
	Convey("Given a status from a newer build", t, func() {
		word, detail := cellState("some-future-state", nil)
		st := humanState(word, detail)

		Convey("When it is rendered", func() {
			Convey("Then it is surfaced rather than assumed healthy", func() {
				So(st.word, ShouldEqual, wordFailed)
				So(st.reason, ShouldContainSubstring, "some-future-state")
			})
		})
	})
}

// TestStrategyPhraseIsProse pins W7-UX-SPEC §1.2: strategy words are removed
// from the human table, replaced by the consequence the user can act on.
func TestStrategyPhraseIsProse(t *testing.T) {
	Convey("Given each delivery strategy", t, func() {
		Convey("When it is phrased for a human", func() {
			Convey("Then the strategy word itself is gone", func() {
				So(strategyPhrase("native"), ShouldEqual, "installed by the host itself")
				So(strategyPhrase("synth"), ShouldEqual, "written as files")
				So(strategyPhrase("loose"), ShouldEqual, "written as files")
				So(strategyPhrase("silenced"), ShouldContainSubstring, "no way to take it")
				So(strategyPhrase(""), ShouldBeEmpty)
			})
		})
	})
}

// TestFootnotesExplainEveryNonDeliveredRow pins the footnote format: a
// skipped, blocked, your-edit, not-ours or failed row carries a marker, and
// the reason is spelled out once at the bottom.
func TestFootnotesExplainEveryNonDeliveredRow(t *testing.T) {
	Convey("Given a run with one delivered and three unfulfilled cells", t, func() {
		notes := newFootnotes()

		Convey("When the rows are marked", func() {
			delivered := notes.mark(wordDelivered, "current")
			skipped := notes.mark(wordSkipped, "missing")
			yours := notes.mark(wordYourEdit, "hands-off")
			theirs := notes.mark(wordNotOurs, "foreign")

			Convey("Then delivered needs no footnote", func() {
				So(delivered, ShouldBeEmpty)
			})

			Convey("Then every other row carries one", func() {
				So(skipped, ShouldNotBeEmpty)
				So(yours, ShouldNotBeEmpty)
				So(theirs, ShouldNotBeEmpty)
			})

			Convey("Then each distinct reason is numbered once", func() {
				So(skipped, ShouldEqual, " [1]")
				So(yours, ShouldEqual, " [2]")
				So(theirs, ShouldEqual, " [3]")

				var buf bytes.Buffer

				notes.write(&buf)
				So(buf.String(), ShouldContainSubstring, "[1] not delivered on this machine")
				So(buf.String(), ShouldContainSubstring, "[3] this file was not written by verger")
			})
		})
	})
}

// TestFootnotesDeduplicateReasons keeps the block short when many cells share
// one cause — the common case, not the exception.
func TestFootnotesDeduplicateReasons(t *testing.T) {
	Convey("Given ten cells missing for the same reason", t, func() {
		notes := newFootnotes()

		for range 10 {
			So(notes.mark(wordSkipped, "missing"), ShouldEqual, " [1]")
		}

		var buf bytes.Buffer

		notes.write(&buf)

		Convey("When the block is printed", func() {
			Convey("Then the reason appears once", func() {
				So(strings.Count(buf.String(), "not delivered on this machine"), ShouldEqual, 1)
			})
		})
	})
}
