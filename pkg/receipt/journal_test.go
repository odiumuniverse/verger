package receipt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// fixedJournalTime is the deterministic clock used by journal tests.
var fixedJournalTime = time.Date(2026, 9, 25, 12, 30, 45, 123456789, time.UTC)

// newTestJournal opens a journal with a deterministic clock.
func newTestJournal(t *testing.T) *Journal {
	t.Helper()

	j := OpenJournal(filepath.Join(t.TempDir(), "state", "journal.jsonl"))
	j.now = func() time.Time { return fixedJournalTime }

	return j
}

func TestJournalAppendRead(t *testing.T) {
	Convey("Given an empty journal", t, func() {
		j := newTestJournal(t)

		Convey("When two events are appended", func() {
			err := j.Append(
				Event{Kind: EventInstall, Package: "owner/name", Host: "claude", Scope: ScopeUser, Version: "1.0.0"},
				Event{Kind: EventLock, LockGeneration: 1, LockDigest: testHash("lock")},
			)

			Convey("Then sequence and time are assigned and the file is well-formed", func() {
				So(err, ShouldBeNil)
				assertMode(t, filepath.Dir(j.path), 0o700)
				assertMode(t, j.path, 0o600)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 2)
				So(events[0].Seq, ShouldEqual, 1)
				So(events[0].At, ShouldEqual, fixedJournalTime)
				So(events[0].Kind, ShouldEqual, EventInstall)
				So(events[0].Package, ShouldEqual, "owner/name")
				So(events[1].Seq, ShouldEqual, 2)
				So(events[1].LockGeneration, ShouldEqual, 1)
				So(events[1].LockDigest, ShouldEqual, testHash("lock"))

				last, lastErr := j.LastSeq()
				So(lastErr, ShouldBeNil)
				So(last, ShouldEqual, 2)
			})
		})

		Convey("When events carry explicit Extra fields", func() {
			err := j.Append(Event{Kind: EventPin, Extra: map[string]json.RawMessage{"note": json.RawMessage(`"why"`)}})

			Convey("Then the extra map round-trips", func() {
				So(err, ShouldBeNil)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 1)
				So(string(events[0].Extra["note"]), ShouldEqualJSON, `"why"`)
			})
		})

		Convey("When Append is called with no events", func() {
			err := j.Append()

			Convey("Then it is a no-op and creates nothing", func() {
				So(err, ShouldBeNil)
				assertMissing(t, j.path)
			})
		})
	})
}

func TestJournalEmptyFile(t *testing.T) {
	Convey("Given a zero-byte journal file", t, func() {
		j := newTestJournal(t)
		So(os.MkdirAll(filepath.Dir(j.path), 0o700), ShouldBeNil)
		So(os.WriteFile(j.path, nil, 0o600), ShouldBeNil)

		Convey("When it is read and appended to", func() {
			events, err := j.Read()
			last, lastErr := j.LastSeq()
			appendErr := j.Append(Event{Kind: EventInstall, Package: "a/one"})

			Convey("Then it behaves like an empty journal", func() {
				So(err, ShouldBeNil)
				So(events, ShouldBeEmpty)
				So(lastErr, ShouldBeNil)
				So(last, ShouldEqual, 0)
				So(appendErr, ShouldBeNil)

				events, err = j.Read()
				So(err, ShouldBeNil)
				So(events, ShouldHaveLength, 1)
				So(events[0].Seq, ShouldEqual, 1)
			})
		})
	})
}

func TestJournalMissingFile(t *testing.T) {
	Convey("Given a journal path that does not exist", t, func() {
		j := OpenJournal(filepath.Join(t.TempDir(), "state", "journal.jsonl"))

		Convey("When it is read", func() {
			events, err := j.Read()
			last, lastErr := j.LastSeq()

			Convey("Then the journal is empty and LastSeq is zero", func() {
				So(err, ShouldBeNil)
				So(events, ShouldBeEmpty)
				So(lastErr, ShouldBeNil)
				So(last, ShouldEqual, 0)
			})
		})
	})
}

func TestJournalTornTrailingLine(t *testing.T) {
	Convey("Given a journal with a torn trailing line", t, func() {
		j := newTestJournal(t)
		So(j.Append(Event{Kind: EventInstall, Package: "a/one"}), ShouldBeNil)
		So(j.Append(Event{Kind: EventRemove, Package: "a/one"}), ShouldBeNil)

		file, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o600)
		So(err, ShouldBeNil)

		_, writeErr := file.WriteString(`{"seq":3,"at":"2026-09-25T12:30:45Z","kind":"torn-garbage`)
		So(writeErr, ShouldBeNil)
		So(file.Close(), ShouldBeNil)

		Convey("When the journal is read", func() {
			events, readErr := j.Read()
			last, lastSeqErr := j.LastSeq()

			Convey("Then the torn line is ignored", func() {
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 2)
				So(lastSeqErr, ShouldBeNil)
				So(last, ShouldEqual, 2)
			})
		})

		Convey("When append repairs the torn line and continues", func() {
			err := j.Append(Event{Kind: EventUpdate, Package: "a/one"})

			Convey("Then the garbage is dropped and the sequence continues", func() {
				So(err, ShouldBeNil)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 3)
				So(events[2].Seq, ShouldEqual, 3)
				So(events[2].Kind, ShouldEqual, EventUpdate)

				raw := readTestFile(t, j.path)
				So(strings.Contains(string(raw), "torn-garbage"), ShouldBeFalse)
				So(strings.HasSuffix(string(raw), "\n"), ShouldBeTrue)
			})
		})
	})
}

func TestJournalCorruptLine(t *testing.T) {
	Convey("Given a journal with a corrupt complete line", t, func() {
		j := newTestJournal(t)

		content := `{"seq":1,"at":"2026-09-25T12:30:45Z","kind":"install"}` + "\n" +
			"{broken}" + "\n" +
			`{"seq":3,"at":"2026-09-25T12:30:45Z","kind":"remove"}` + "\n"

		So(os.MkdirAll(filepath.Dir(j.path), 0o700), ShouldBeNil)
		So(os.WriteFile(j.path, []byte(content), 0o600), ShouldBeNil)

		Convey("When the journal is read", func() {
			_, readErr := j.Read()
			_, lastErr := j.LastSeq()

			Convey("Then the 1-based line number is reported", func() {
				target, ok := errors.AsType[*CorruptJournalError](readErr)
				So(ok, ShouldBeTrue)
				So(target.Line, ShouldEqual, 2)

				_, lastOK := errors.AsType[*CorruptJournalError](lastErr)
				So(lastOK, ShouldBeTrue)
			})
		})

		Convey("When a blank line sits in the middle", func() {
			blank := `{"seq":1,"at":"2026-09-25T12:30:45Z","kind":"install"}` + "\n\n" +
				`{"seq":2,"at":"2026-09-25T12:30:45Z","kind":"remove"}` + "\n"
			So(os.WriteFile(j.path, []byte(blank), 0o600), ShouldBeNil)

			_, readErr := j.Read()

			Convey("Then it reports a corrupt line too", func() {
				target, ok := errors.AsType[*CorruptJournalError](readErr)
				So(ok, ShouldBeTrue)
				So(target.Line, ShouldEqual, 2)
			})
		})

		Convey("When a complete line carries an unknown kind", func() {
			unknown := `{"seq":1,"at":"2026-09-25T12:30:45Z","kind":"dance"}` + "\n"
			So(os.WriteFile(j.path, []byte(unknown), 0o600), ShouldBeNil)

			_, readErr := j.Read()

			Convey("Then validation fails as corrupt", func() {
				_, ok := errors.AsType[*CorruptJournalError](readErr)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestJournalValidation(t *testing.T) {
	Convey("Given journal event validation", t, func() {
		j := newTestJournal(t)

		cases := []struct {
			name  string
			event Event
		}{
			{"unknown kind", Event{Kind: "dance"}},
			{"unknown scope", Event{Kind: EventInstall, Scope: "global"}},
			{"negative seq", Event{Kind: EventInstall, Seq: -1}},
			{"negative lock generation", Event{Kind: EventLock, LockGeneration: -2}},
			{"invalid lock digest", Event{Kind: EventLock, LockDigest: "nope"}},
		}

		for _, tc := range cases {
			Convey("When appending a "+tc.name, func() {
				err := j.Append(tc.event)

				Convey("Then it reports InvalidEventError and writes nothing", func() {
					So(err, ShouldBeError)

					_, ok := errors.AsType[*InvalidEventError](err)
					So(ok, ShouldBeTrue)

					events, readErr := j.Read()
					So(readErr, ShouldBeNil)
					So(events, ShouldBeEmpty)
				})
			})
		}

		Convey("When one batch mixes a valid and an invalid event", func() {
			err := j.Append(
				Event{Kind: EventInstall, Package: "a/one"},
				Event{Kind: "dance"},
			)

			Convey("Then nothing is written and the sequence is untouched", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidEventError](err)
				So(ok, ShouldBeTrue)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldBeEmpty)
			})
		})
	})
}

func TestJournalExplicitSeq(t *testing.T) {
	Convey("Given a journal", t, func() {
		j := newTestJournal(t)

		Convey("When an explicit sequence number is appended", func() {
			err := j.Append(Event{Kind: EventInstall, Seq: 5, Package: "a/one"})

			Convey("Then it is kept and the next assignment continues after it", func() {
				So(err, ShouldBeNil)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events[0].Seq, ShouldEqual, 5)

				So(j.Append(Event{Kind: EventRemove, Package: "a/one"}), ShouldBeNil)

				events, readErr = j.Read()
				So(readErr, ShouldBeNil)
				So(events[1].Seq, ShouldEqual, 6)
			})
		})

		Convey("When an explicit sequence number would go backwards", func() {
			So(j.Append(Event{Kind: EventInstall, Seq: 5}), ShouldBeNil)

			err := j.Append(Event{Kind: EventRemove, Seq: 5})

			Convey("Then it is rejected as InvalidEventError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidEventError](err)
				So(ok, ShouldBeTrue)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 1)
			})
		})
	})
}

func TestJournalNormalizesTimeToUTC(t *testing.T) {
	Convey("Given a journal", t, func() {
		j := newTestJournal(t)

		zone := time.FixedZone("UTC+3", 3*60*60)
		at := time.Date(2026, 9, 25, 15, 30, 0, 0, zone)

		Convey("When an event carries a local time", func() {
			err := j.Append(Event{Kind: EventInstall, At: at})

			Convey("Then it is stored in UTC", func() {
				So(err, ShouldBeNil)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events[0].At.UTC(), ShouldEqual, at.UTC())
				So(events[0].At.Location(), ShouldEqual, time.UTC)
			})
		})
	})
}

func TestJournalConcurrentAppends(t *testing.T) {
	Convey("Given a journal", t, func() {
		j := newTestJournal(t)

		const n = 16

		Convey("When many goroutines append concurrently", func() {
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
			)

			for i := range n {
				wg.Go(func() {
					err := j.Append(Event{Kind: EventInstall, Package: fmt.Sprintf("pkg/%d", i)})

					mu.Lock()
					defer mu.Unlock()

					if err != nil {
						errs = append(errs, err)
					}
				})
			}

			wg.Wait()

			Convey("Then every event gets a unique sequence and all lines parse", func() {
				So(errs, ShouldBeEmpty)

				events, err := j.Read()
				So(err, ShouldBeNil)
				So(events, ShouldHaveLength, n)

				seen := map[int64]bool{}
				for _, e := range events {
					So(seen[e.Seq], ShouldBeFalse)

					seen[e.Seq] = true
				}

				for seq := int64(1); seq <= n; seq++ {
					So(seen[seq], ShouldBeTrue)
				}

				raw := readTestFile(t, j.path)
				lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
				So(lines, ShouldHaveLength, n)

				for _, line := range lines {
					var e Event
					So(json.Unmarshal([]byte(line), &e), ShouldBeNil)
				}
			})
		})
	})
}

func TestJournalUnicodeAndLargeEvents(t *testing.T) {
	Convey("Given a journal", t, func() {
		j := newTestJournal(t)

		long := strings.Repeat("x", 64*1024)

		Convey("When unicode and large payloads are appended", func() {
			err := j.Append(Event{
				Kind:    EventInstall,
				Package: "пакет/с-пробелом",
				Host:    "claude",
				Extra:   map[string]json.RawMessage{"command": json.RawMessage(`"` + long + `"`)},
			})

			Convey("Then they round-trip", func() {
				So(err, ShouldBeNil)

				events, readErr := j.Read()
				So(readErr, ShouldBeNil)
				So(events, ShouldHaveLength, 1)
				So(events[0].Package, ShouldEqual, "пакет/с-пробелом")
				So(len(events[0].Extra["command"]), ShouldBeGreaterThan, 64*1024)
			})
		})
	})
}

func TestJournalGolden(t *testing.T) {
	Convey("Given a fixed journal sequence", t, func() {
		j := newTestJournal(t)

		err := j.Append(
			Event{Kind: EventInstall, Package: "owner/name", Host: "claude", Scope: ScopeUser, Version: "1.0.0"},
			Event{Kind: EventLock, LockGeneration: 1, LockDigest: testHash("lock")},
			Event{Kind: EventRemove, Package: "owner/name", Host: "codex", Cause: string(CauseUser), Initiator: "codex"},
		)
		So(err, ShouldBeNil)

		Convey("When the journal file is compared to the golden fixture", func() {
			raw := readTestFile(t, j.path)
			checkGolden(t, filepath.Join("testdata", "journal.golden.jsonl"), raw)
		})
	})
}
