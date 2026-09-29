package cli

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestCellStateMapsEveryInternalCode is the table that pins the one place
// the two vocabularies meet. It is deliberately explicit about the two
// mappings that are easy to get wrong and expensive when they are:
//
//   - `skew` is `skipped`, not `your edit`. The file is ours, at an old
//     version; nobody edited anything and there is no decision to make. The
//     remedy is a re-run, not "keep your version or overwrite it".
//   - `removed` is its own word, not `delivered`. The package was delivered
//     and is now gone; reporting it as delivered claims the opposite of what
//     happened.
//
// Every internal code the tool can record has a row, so a new one cannot be
// added without deciding what it means to a user.
func TestCellStateMapsEveryInternalCode(t *testing.T) {
	cases := []struct {
		name   string
		status string
		notes  []string
		word   string
		detail string
	}{
		{
			name: "current is delivered", status: "current",
			word: wordDelivered, detail: "current",
		},
		{
			name: "missing is skipped, not blocked", status: "missing",
			word: wordSkipped, detail: "missing",
		},
		{
			name: "skew is skipped, never your edit", status: "skew",
			word: wordSkipped, detail: "skew",
		},
		{
			name: "planned is skipped", status: "planned",
			word: wordSkipped, detail: "planned",
		},
		{
			name: "removed keeps its own word", status: "removed",
			word: wordRemoved, detail: "removed",
		},
		{
			name: "a delivery that wrote nothing is skipped, not failed", status: "skipped",
			notes: []string{"nothing to write: the package produced no files for claude"},
			word:  wordSkipped, detail: "skipped",
		},
		{
			name: "foreign is not ours", status: "foreign",
			word: wordNotOurs, detail: "foreign",
		},
		{
			name: "needs-auth is blocked", status: "needs-auth",
			word: wordBlocked, detail: "needs-auth",
		},
		{
			name: "needs-runtime is blocked", status: "needs-runtime",
			word: wordBlocked, detail: "needs-runtime",
		},
		{
			name: "failed stays failed", status: "failed",
			word: wordFailed, detail: "failed",
		},
		{
			name: "hands-off with a content drift is your edit", status: "hands-off",
			notes: []string{"content drift detected on disk"},
			word:  wordYourEdit, detail: "hands-off",
		},
		{
			name: "hands-off with no drift is blocked", status: "hands-off",
			notes: []string{"host refused the write"},
			word:  wordBlocked, detail: "hands-off",
		},
		{
			name: "hands-off with no note is blocked, not guessed", status: "hands-off",
			word: wordBlocked, detail: "hands-off",
		},
		{
			name: "an unknown code is surfaced, not assumed healthy", status: "from-the-future",
			word: wordFailed, detail: "from-the-future",
		},
	}

	for _, tc := range cases {
		Convey("Given the internal status "+tc.name, t, func() {
			word, detail := cellState(tc.status, tc.notes)

			Convey("Then it maps to the documented word and keeps its detail", func() {
				So(word, ShouldEqual, tc.word)
				So(detail, ShouldEqual, tc.detail)
			})
		})
	}
}

// TestCellStateNeverLosesTheDetail pins the reason the mapping is not
// one-to-one: two internal codes share a word, so `detail` is the only way a
// consumer can tell them apart.
func TestCellStateNeverLosesTheDetail(t *testing.T) {
	Convey("Given two codes that both read as skipped", t, func() {
		Convey("When they are mapped", func() {
			missingWord, missingDetail := cellState("missing", nil)
			skewWord, skewDetail := cellState("skew", nil)

			Convey("Then the words agree and the details do not", func() {
				So(missingWord, ShouldEqual, skewWord)
				So(missingDetail, ShouldNotEqual, skewDetail)
			})
		})
	})
}

// TestEveryInternalCodeHasARow is the guard against a new status slipping in
// untranslated: the list below is the complete set the tool can record.
func TestEveryInternalCodeHasARow(t *testing.T) {
	codes := []string{
		"current", "missing", "skew", "planned", "removed", "skipped",
		"foreign", "needs-auth", "needs-runtime", "failed", "hands-off",
	}

	words := []string{
		wordDelivered, wordSkipped, wordBlocked, wordRemoved,
		wordYourEdit, wordNotOurs, wordFailed,
	}

	Convey("Given every status the tool can record", t, func() {
		for _, code := range codes {
			Convey("When the status "+code+" is mapped", func() {
				word, detail := cellState(code, []string{"content drift detected"})

				Convey("Then it lands on a known word and keeps its code", func() {
					So(word, ShouldBeIn, words)
					So(detail, ShouldNotBeEmpty)
				})
			})
		}
	})
}

// TestSkewIsNeverYourEdit states the correction in one place, so a later
// reader does not "simplify" it back: version skew means the file is ours at
// an older version, not that a person changed it.
func TestSkewIsNeverYourEdit(t *testing.T) {
	Convey("Given a skewed cell", t, func() {
		word, _ := cellState("skew", []string{"content drift detected on disk"})

		Convey("When it is rendered for a user", func() {
			Convey("Then it is skipped even when the note mentions drift", func() {
				So(word, ShouldEqual, wordSkipped)
				So(word, ShouldNotEqual, wordYourEdit)
			})
		})
	})
}
